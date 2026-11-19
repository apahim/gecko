package v1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	pkgschema "github.com/openshift-online/gecko/orlop/pkg/apiserver/schema"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/yaml"
)

// TestClusterSchemaMaintenanceWindowNull verifies that a null maintenance
// window passes schema validation. Before PruneNonNullableNullsWithoutDefaults
// was added to Processor.Process, "window":null would fail with
// "must be of type object: null" even though null is the correct way to clear
// a previously configured window in a public PUT.
func TestClusterSchemaMaintenanceWindowNull(t *testing.T) {
	processor := newClusterSchemaProcessor(t)
	cluster := validCluster("4.15.0")
	// Set window to nil — this serializes as "window":null in the maintenance object.
	cluster.Spec.Maintenance = &ClusterMaintenanceSpec{Window: nil}

	if errs := processor.Process(context.Background(), clusterAsMap(t, cluster), nil); len(errs) > 0 {
		t.Fatalf("expected 'window:null' to pass schema validation, got: %v", errs)
	}
}

func TestClusterSchemaAcceptsPrereleaseVersion(t *testing.T) {
	processor := newClusterSchemaProcessor(t)
	object := clusterAsMap(t, validCluster("5.0.0-ec.6"))

	if errs := processor.Process(context.Background(), object, nil); len(errs) > 0 {
		t.Fatalf("expected prerelease cluster request to pass schema validation, got: %v", errs)
	}
}

func TestClusterSchemaRejectsInvalidReleaseVersions(t *testing.T) {
	for _, version := range []string{"v5.0.0", "5.0.0-01", "04.23", "4.023", "4.23-rc.1", "4"} {
		t.Run(version, func(t *testing.T) {
			processor := newClusterSchemaProcessor(t)
			errs := processor.Process(context.Background(), clusterAsMap(t, validCluster(version)), nil)
			if len(errs) == 0 {
				t.Fatalf("expected version %q to fail schema validation", version)
			}
			if !strings.Contains(errs.ToAggregate().Error(), "version must be major.minor") {
				t.Fatalf("expected version validation error, got: %v", errs)
			}
		})
	}
}

func TestClusterSchemaIngressEmail(t *testing.T) {
	processor := newClusterSchemaProcessor(t)
	clusterWithEmail := func(email string) map[string]any {
		cluster := validCluster("5.0.0-ec.6")
		cluster.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountsRef.IngressEmail = &email
		return clusterAsMap(t, cluster)
	}
	// A well-formed email and an omitted field both pass.
	cluster := validCluster("5.0.0-ec.6")
	if errs := processor.Process(context.Background(), clusterAsMap(t, cluster), nil); len(errs) > 0 {
		t.Fatalf("expected omitted ingressEmail to pass schema validation, got: %v", errs)
	}
	if errs := processor.Process(context.Background(), clusterWithEmail("ingress@test-project.iam.gserviceaccount.com"), nil); len(errs) > 0 {
		t.Fatalf("expected well-formed ingressEmail to pass schema validation, got: %v", errs)
	}

	for _, email := range []string{
		// An explicitly present empty string is rejected: the field is
		// *string-optional, so unset means omitted or null, not "".
		"",
		"a@test-project.iam.gserviceaccount.com",
		"@test-project.iam.gserviceaccount.com",
		"ing ress@test-project.iam.gserviceaccount.com",
		"Ingress@test-project.iam.gserviceaccount.com",
		"x@y@test-project.iam.gserviceaccount.com",
		"ingress@test-project.iam.gserviceaccount.comm",
	} {
		t.Run("reject-"+email, func(t *testing.T) {
			errs := processor.Process(context.Background(), clusterWithEmail(email), nil)
			if len(errs) == 0 {
				t.Fatalf("expected ingressEmail %q to fail schema validation", email)
			}
		})
	}
}

// TestClusterSchemaIngressEmailImmutability verifies that the CEL rules
// reject adding, removing, and changing ingressEmail on update. The field's
// own rule covers changes; the enclosing non-optional workloadIdentity struct
// rule covers add and remove.
func TestClusterSchemaIngressEmailImmutability(t *testing.T) {
	processor := newClusterSchemaProcessor(t)
	clusterWithEmail := func(email string) map[string]any {
		cluster := validCluster("5.0.0-ec.6")
		cluster.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountsRef.IngressEmail = &email
		return clusterAsMap(t, cluster)
	}
	withoutEmail := func() map[string]any {
		cluster := validCluster("5.0.0-ec.6")
		return clusterAsMap(t, cluster)
	}
	for _, tc := range []struct {
		name    string
		old, nw map[string]any
	}{
		{name: "add", old: withoutEmail(), nw: clusterWithEmail("ingress@test-project.iam.gserviceaccount.com")},
		{name: "remove", old: clusterWithEmail("ingress@test-project.iam.gserviceaccount.com"), nw: withoutEmail()},
		{name: "change", old: clusterWithEmail("ingress@test-project.iam.gserviceaccount.com"), nw: clusterWithEmail("ingress2@test-project.iam.gserviceaccount.com")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := processor.Process(context.Background(), tc.nw, tc.old)
			if len(errs) == 0 {
				t.Fatalf("expected %s of ingressEmail to be rejected", tc.name)
			}
		})
	}
}

func TestClusterSchemaChannelGroup(t *testing.T) {
	processor := newClusterSchemaProcessor(t)
	for _, group := range []string{"stable", "fast", "candidate", "eus", "nightly", "dev-preview", "a", strings.Repeat("a", 63)} {
		t.Run("accept-"+group, func(t *testing.T) {
			cluster := validCluster("5.0.0-ec.6")
			cluster.Spec.Release.ChannelGroup = group
			if errs := processor.Process(context.Background(), clusterAsMap(t, cluster), nil); len(errs) > 0 {
				t.Fatalf("expected channelGroup %q to pass schema validation, got: %v", group, errs)
			}
		})
	}

	for _, group := range []string{"", "Nightly", "-nightly", "nightly-", "nightly_x", "nightly.4", strings.Repeat("a", 64)} {
		t.Run("reject-"+group, func(t *testing.T) {
			cluster := validCluster("5.0.0-ec.6")
			cluster.Spec.Release.ChannelGroup = group
			errs := processor.Process(context.Background(), clusterAsMap(t, cluster), nil)
			if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "channelGroup") {
				t.Fatalf("expected channelGroup %q to fail schema validation, got: %v", group, errs)
			}
		})
	}
}

func validCluster(version string) Cluster {
	return Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "test-namespace",
		},
		Spec: ClusterSpec{
			InfraID: "test-cluster",
			Platform: ClusterPlatformSpec{
				Type: "GCP",
				GCP: &GCPClusterPlatform{
					ProjectID: "test-project",
					Region:    "us-central1",
					Network:   "test-network",
					Subnet:    "test-subnet",
					WorkloadIdentity: WorkloadIdentitySpec{
						PoolID:        "test-pool",
						ProjectNumber: "123456789",
						ProviderID:    "test-provider",
						ServiceAccountsRef: &ServiceAccountsRef{
							NodePoolEmail:        "nodepool@test-project.iam.gserviceaccount.com",
							ControlPlaneEmail:    "control-plane@test-project.iam.gserviceaccount.com",
							CloudControllerEmail: "cloud-controller@test-project.iam.gserviceaccount.com",
							StorageEmail:         "storage@test-project.iam.gserviceaccount.com",
							ImageRegistryEmail:   "image-registry@test-project.iam.gserviceaccount.com",
							NetworkEmail:         "network@test-project.iam.gserviceaccount.com",
						},
					},
				},
			},
			Release: ClusterReleaseSpec{
				Version:      version,
				ChannelGroup: "candidate",
			},
			Networking: NetworkingSpec{},
		},
	}
}

func newClusterSchemaProcessor(t *testing.T) *pkgschema.Processor {
	return newSchemaProcessor(t, ClusterSchemaYAML)
}

func newSchemaProcessor(t *testing.T, schemaYAML string) *pkgschema.Processor {
	t.Helper()

	var propsV1 apiextv1.JSONSchemaProps
	if err := yaml.Unmarshal([]byte(schemaYAML), &propsV1); err != nil {
		t.Fatalf("unmarshal generated schema: %v", err)
	}

	var props apiext.JSONSchemaProps
	if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&propsV1, &props, nil); err != nil {
		t.Fatalf("convert generated schema: %v", err)
	}

	structural, err := structuralschema.NewStructural(&props)
	if err != nil {
		t.Fatalf("create structural schema: %v", err)
	}

	processor, err := pkgschema.NewProcessor(structural, &props)
	if err != nil {
		t.Fatalf("create schema processor: %v", err)
	}

	return processor
}

func clusterAsMap(t *testing.T, cluster Cluster) map[string]any {
	return objectAsMap(t, cluster)
}

func objectAsMap(t *testing.T, input any) map[string]any {
	t.Helper()

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}

	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("unmarshal object JSON: %v", err)
	}

	return object
}

func TestClusterSchemaAcceptsMinorRelease(t *testing.T) {
	processor := newClusterSchemaProcessor(t)
	if errs := processor.Process(t.Context(), clusterAsMap(t, validCluster("4.23")), nil); len(errs) > 0 {
		t.Fatalf("minor release failed schema validation: %v", errs)
	}
}
