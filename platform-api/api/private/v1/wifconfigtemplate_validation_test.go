package v1

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/yaml"
)

func wifTestTemplate(t *testing.T) *WIFConfigTemplate {
	t.Helper()
	data, err := os.ReadFile("../../../../helm/charts/platform-api-server/files/wifconfigtemplate-project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w := &WIFConfigTemplate{ObjectMeta: metav1.ObjectMeta{Name: "5.0.0", UID: types.UID("template-uid")}}
	if err := yaml.UnmarshalStrict(data, &w.Spec); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWIFConfigBaseline(t *testing.T) {
	w := wifTestTemplate(t)
	if err := w.ValidateCreate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if IsWIFConfigVerified(w.Spec) {
		t.Fatal("baseline must not implicitly be verified")
	}
	if len(w.Spec.IAM.Roles) != 6 || len(w.Spec.IAM.RoleBindings) != 6 {
		t.Fatal("baseline must contain six project custom roles and grants")
	}
	counts := map[string]int{"hcpCloudControllerProject": 51, "hcpCloudNetworkProject": 5, "hcpControlPlaneProject": 36, "hcpGCPPDCSIProject": 30, "hcpImageRegistryProject": 9, "hcpNodePoolProject": 11}
	total := 0
	for _, role := range w.Spec.IAM.Roles {
		if len(role.Permissions) != counts[role.Name] {
			t.Errorf("%s permissions = %d, want %d", role.Name, len(role.Permissions), counts[role.Name])
		}
		total += len(role.Permissions)
	}
	if total != 142 {
		t.Fatalf("permission entries = %d, want 142", total)
	}
	digest, err := WIFConfigDigest(w.Spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("baseline executable digest: %s", digest)
	if digest != "sha256:c0b4c3dbdd48d1925bf7c172ac0385639d00092f083b5cac11865e83c3acea71" {
		t.Fatal("baseline executable contract changed; review permissions/trust before accepting a new golden digest")
	}
}

func TestWIFConfigDigestAndImmutability(t *testing.T) {
	w := wifTestTemplate(t)
	before := w.DeepCopy()
	digest, err := WIFConfigDigest(w.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, verified := range []*bool{new(false), new(true), nil} {
		w.Spec.Verified = verified
		if err := w.ValidateUpdate(t.Context(), before); err != nil {
			t.Fatal(err)
		}
		got, err := WIFConfigDigest(w.Spec)
		if err != nil || got != digest {
			t.Fatalf("verification changed digest: %q %v", got, err)
		}
	}
	slices.Reverse(w.Spec.IAM.ServiceAccounts)
	slices.Reverse(w.Spec.IAM.RequiredComponents)
	slices.Reverse(w.Spec.IAM.Roles)
	slices.Reverse(w.Spec.IAM.RoleBindings)
	slices.Reverse(w.Spec.IAM.WorkloadIdentityBindings.ServiceAccountRefs)
	for i := range w.Spec.IAM.Roles {
		slices.Reverse(w.Spec.IAM.Roles[i].Permissions)
	}
	for i := range w.Spec.IAM.ServiceAccounts {
		slices.Reverse(w.Spec.IAM.ServiceAccounts[i].WorkloadIdentitySubjects)
	}
	slices.Reverse(w.Spec.IAM.ServiceAccountRoleBindings[0].MemberServiceAccountRefs)
	snapshot := w.DeepCopy()
	if got, err := WIFConfigDigest(w.Spec); err != nil || got != digest {
		t.Fatalf("list ordering changed digest: %q %v", got, err)
	}
	if !reflect.DeepEqual(snapshot, w) {
		t.Fatal("digest mutated input")
	}
	if err := w.ValidateUpdate(t.Context(), before); err != nil {
		t.Fatal(err)
	}
	w.Spec.IAM.Roles[0].Permissions = append(w.Spec.IAM.Roles[0].Permissions, "compute.instances.update")
	if err := w.ValidateUpdate(t.Context(), before); err == nil {
		t.Fatal("contract edit accepted")
	}
	if err := w.ValidateDelete(t.Context()); err == nil {
		t.Fatal("catalog deletion accepted")
	}
}

func TestWIFConfigValidation(t *testing.T) {
	for _, name := range []string{"v5.0.0", "05.0.0", "5.0", "5.0.0-rc.1", "5.0.0+build", "5.0.-1", strings.Repeat("1", 33) + ".0.0"} {
		t.Run(name, func(t *testing.T) {
			w := wifTestTemplate(t)
			w.Name = name
			if w.ValidateCreate(t.Context()) == nil {
				t.Fatal("invalid name accepted")
			}
		})
	}
	cases := map[string]func(*WIFConfigTemplate){
		"schema":              func(w *WIFConfigTemplate) { w.Spec.SchemaVersion = 2 },
		"missing component":   func(w *WIFConfigTemplate) { w.Spec.IAM.ServiceAccounts = w.Spec.IAM.ServiceAccounts[:5] },
		"duplicate component": func(w *WIFConfigTemplate) { w.Spec.IAM.ServiceAccounts[1] = w.Spec.IAM.ServiceAccounts[0] },
		"wildcard subject": func(w *WIFConfigTemplate) {
			w.Spec.IAM.ServiceAccounts[0].WorkloadIdentitySubjects = []string{"system:serviceaccount:*:*"}
		},
		"duplicate subject": func(w *WIFConfigTemplate) {
			s := &w.Spec.IAM.ServiceAccounts[0]
			s.WorkloadIdentitySubjects = append(s.WorkloadIdentitySubjects, s.WorkloadIdentitySubjects[0])
		},
		"duplicate permission": func(w *WIFConfigTemplate) {
			r := &w.Spec.IAM.Roles[0]
			r.Permissions = append(r.Permissions, r.Permissions[0])
		},
		"duplicate role": func(w *WIFConfigTemplate) { w.Spec.IAM.Roles[1] = w.Spec.IAM.Roles[0] },
		"unknown grant":  func(w *WIFConfigTemplate) { w.Spec.IAM.RoleBindings[0].RoleRef = "missing" },
		"scope":          func(w *WIFConfigTemplate) { w.Spec.IAM.RoleBindings[0].Scope.Kind = "ResourceName" },
		"duplicate grant": func(w *WIFConfigTemplate) {
			w.Spec.IAM.RoleBindings = append(w.Spec.IAM.RoleBindings, w.Spec.IAM.RoleBindings[0])
		},
		"unbound role": func(w *WIFConfigTemplate) {
			w.Spec.IAM.Roles = append(w.Spec.IAM.Roles, WIFConfigRole{Name: "unused", Permissions: []string{"compute.instances.get"}})
		},
		"federation role": func(w *WIFConfigTemplate) { w.Spec.IAM.WorkloadIdentityBindings.Role = "roles/owner" },
		"actAs scope":     func(w *WIFConfigTemplate) { w.Spec.IAM.ServiceAccountRoleBindings[0].Scope.Kind = "Project" },
		"actAs members": func(w *WIFConfigTemplate) {
			w.Spec.IAM.ServiceAccountRoleBindings[0].MemberServiceAccountRefs = append(w.Spec.IAM.ServiceAccountRoleBindings[0].MemberServiceAccountRefs, "cloud-controller")
		},
		"naming":     func(w *WIFConfigTemplate) { w.Spec.IAM.Naming.CustomRoleIDTemplate = "hcp_{role}" },
		"truncation": func(w *WIFConfigTemplate) { w.Spec.IAM.Naming.MaxCustomRoleIDLength = 20 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w := wifTestTemplate(t)
			mutate(w)
			if w.ValidateCreate(t.Context()) == nil {
				t.Fatal("invalid contract accepted")
			}
		})
	}
}

func wifTestRef(t *testing.T, w *WIFConfigTemplate) WIFConfigTemplateReference {
	t.Helper()
	digest, err := WIFConfigDigest(w.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return WIFConfigTemplateReference{Name: w.Name, UID: string(w.UID), Digest: digest}
}

func wifTestCluster(ref WIFConfigTemplateReference) *Cluster {
	infra, project := "iam-new-1", "test-project"
	email := func(component string) string {
		return infra + "-" + component + "@" + project + ".iam.gserviceaccount.com"
	}
	c := &Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: project, UID: types.UID("cluster-uid")}}
	c.Spec.SafeName = DefaultSafeName(c.Name, c.UID)
	c.Spec.InfraID = infra
	c.Spec.IssuerURL = "https://issuer.example/" + infra
	c.Spec.Platform = ClusterPlatformSpec{Type: "GCP", GCP: &GCPClusterPlatform{ProjectID: project, WorkloadIdentity: WorkloadIdentitySpec{ProjectNumber: "123456789", PoolID: infra + "-wi-pool", ProviderID: infra + "-k8s-provider", ServiceAccountsRef: &ServiceAccountsRef{NodePoolEmail: email("nodepool-mgmt"), ControlPlaneEmail: email("ctrlplane-op"), CloudControllerEmail: email("cloud-controller"), StorageEmail: email("gcp-pd-csi"), ImageRegistryEmail: email("image-registry"), NetworkEmail: email("cloud-network")}}, IAM: &ClusterIAMSpec{WorkerServiceAccount: "123456789-compute@developer.gserviceaccount.com", TemplateApplications: []WIFConfigTemplateReference{ref}}}}
	return c
}

func TestClusterIAMApplications(t *testing.T) {
	w := wifTestTemplate(t)
	w.Spec.Verified = new(true)
	SetValidatorDeps(ValidatorDeps{AllowIAMApplicationWrites: true, WIFConfigTemplate: func(_ context.Context, name string) (*WIFConfigTemplate, error) {
		if name == w.Name {
			return w, nil
		}
		return nil, fmt.Errorf("not found")
	}})
	t.Cleanup(func() { SetValidatorDeps(ValidatorDeps{}) })
	ref := wifTestRef(t, w)
	old := wifTestCluster(ref)
	if err := old.ValidateCreate(t.Context()); err != nil {
		t.Fatal(err)
	}
	deps := getValidatorDeps()
	deps.AllowIAMApplicationWrites = false
	SetValidatorDeps(deps)
	if old.ValidateCreate(t.Context()) == nil {
		t.Fatal("rollout gate accepted a new application")
	}
	if err := old.DeepCopy().ValidateUpdate(t.Context(), old); err != nil {
		t.Fatalf("rollout gate blocked retained history: %v", err)
	}
	deps.AllowIAMApplicationWrites = true
	SetValidatorDeps(deps)
	// A new exact verified contract can be appended without replacing history.
	w.Name = "5.0.1"
	w.UID = "second-template-uid"
	next := old.DeepCopy()
	next.Spec.Platform.GCP.IAM.TemplateApplications = append(next.Spec.Platform.GCP.IAM.TemplateApplications, wifTestRef(t, w))
	if err := next.ValidateUpdate(t.Context(), old); err != nil {
		t.Fatalf("valid append rejected: %v", err)
	}
	reordered := next.DeepCopy()
	apps := reordered.Spec.Platform.GCP.IAM.TemplateApplications
	apps[0], apps[1] = apps[1], apps[0]
	if err := reordered.ValidateUpdate(t.Context(), next); err == nil {
		t.Fatal("reordered application history accepted as an append")
	}
	w.Name, w.UID = "5.0.0", "template-uid"
	cases := map[string]func(*Cluster){
		"parent removal": func(c *Cluster) { c.Spec.Platform.GCP.IAM = nil },
		"gcp removal":    func(c *Cluster) { c.Spec.Platform.GCP = nil },
		"entry removal":  func(c *Cluster) { c.Spec.Platform.GCP.IAM.TemplateApplications = nil },
		"entry edit": func(c *Cluster) {
			c.Spec.Platform.GCP.IAM.TemplateApplications[0].Digest = "sha256:" + strings.Repeat("0", 64)
		},
		"duplicate": func(c *Cluster) {
			c.Spec.Platform.GCP.IAM.TemplateApplications = append(c.Spec.Platform.GCP.IAM.TemplateApplications, ref)
		},
		"worker change": func(c *Cluster) {
			c.Spec.Platform.GCP.IAM.WorkerServiceAccount = "987-compute@developer.gserviceaccount.com"
		},
		"unknown": func(c *Cluster) {
			r := ref
			r.Name = "5.0.1"
			c.Spec.Platform.GCP.IAM.TemplateApplications = append(c.Spec.Platform.GCP.IAM.TemplateApplications, r)
		},
		"infra change": func(c *Cluster) { c.Spec.InfraID = "other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := old.DeepCopy()
			mutate(c)
			if err := c.ValidateUpdate(t.Context(), old); err == nil {
				t.Fatal("invalid update accepted")
			}
		})
	}
	w.Spec.Verified = new(false)
	if err := old.DeepCopy().ValidateUpdate(t.Context(), old); err != nil {
		t.Fatalf("revocation invalidated history: %v", err)
	}
	if err := old.ValidateCreate(t.Context()); err == nil {
		t.Fatal("new unverified application accepted")
	}
	w.Spec.Verified = new(true)
	for _, mutate := range []func(*Cluster){
		func(c *Cluster) { c.Spec.Platform.GCP.IAM.TemplateApplications[0].UID = "replaced" },
		func(c *Cluster) {
			c.Spec.Platform.GCP.IAM.TemplateApplications[0].Digest = "sha256:" + strings.Repeat("0", 64)
		},
		func(c *Cluster) {
			c.Spec.Platform.GCP.IAM.WorkerServiceAccount = "987-compute@developer.gserviceaccount.com"
		},
		func(c *Cluster) {
			c.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountsRef.NodePoolEmail = "foreign@example.com"
		},
	} {
		c := old.DeepCopy()
		mutate(c)
		if c.ValidateCreate(t.Context()) == nil {
			t.Fatal("invalid initial application accepted")
		}
	}
	SetValidatorDeps(ValidatorDeps{})
	if err := old.ValidateCreate(t.Context()); err == nil {
		t.Fatal("missing lookup must fail closed")
	}
	if err := old.DeepCopy().ValidateUpdate(t.Context(), old); err != nil {
		t.Fatalf("unavailable lookup invalidated retained history: %v", err)
	}
}

func TestVersionWIFReferencePin(t *testing.T) {
	w := wifTestTemplate(t)
	ref := wifTestRef(t, w)
	SetValidatorDeps(ValidatorDeps{WIFConfigTemplate: func(context.Context, string) (*WIFConfigTemplate, error) { return w, nil }})
	t.Cleanup(func() { SetValidatorDeps(ValidatorDeps{}) })
	old := &Version{}
	next := old.DeepCopy()
	next.Spec.WIFConfigTemplateRef = &ref
	if err := next.ValidateUpdate(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	old = next.DeepCopy()
	next.Spec.WIFConfigTemplateRef = nil
	if next.ValidateUpdate(t.Context(), old) == nil {
		t.Fatal("cleared pinned reference accepted")
	}
	next = old.DeepCopy()
	next.Spec.ChannelGroups = []string{"stable"}
	if err := next.ValidateUpdate(t.Context(), old); err != nil {
		t.Fatal(err)
	}
}

func TestWIFSpecFieldsRequireProtocolDecision(t *testing.T) {
	typeOfSpec := reflect.TypeFor[WIFConfigTemplateSpec]()
	fields := []string{}
	for i := 0; i < typeOfSpec.NumField(); i++ {
		fields = append(fields, typeOfSpec.Field(i).Name)
	}
	if !slices.Equal(fields, []string{"Verified", "SchemaVersion", "IAM"}) {
		t.Fatal("new spec field requires an explicit digest/immutability compatibility decision")
	}
}

func TestWIFLookupFailureCategories(t *testing.T) {
	w := wifTestTemplate(t)
	ref := wifTestRef(t, w)
	for _, test := range []struct {
		err     error
		message string
	}{
		{apierrors.NewNotFound(schema.GroupResource{Group: GroupVersion.Group, Resource: "wifconfigtemplates"}, ref.Name), "not present"},
		{fmt.Errorf("private-storage-details"), "temporarily unavailable"},
	} {
		SetValidatorDeps(ValidatorDeps{WIFConfigTemplate: func(context.Context, string) (*WIFConfigTemplate, error) { return nil, test.err }})
		err := validateWIFReference(t.Context(), ref, false, "")
		if err == nil || !strings.Contains(err.Error(), test.message) || strings.Contains(err.Error(), "private-storage-details") {
			t.Fatalf("unsafe/incorrect lookup failure category: %v", err)
		}
	}
	SetValidatorDeps(ValidatorDeps{})
}

func TestWIFApplicationHonorsContractNamingLimit(t *testing.T) {
	w := wifTestTemplate(t)
	w.Spec.Verified = new(true)
	w.Spec.IAM.Naming.MaxInfraIDLength = 1
	if err := w.ValidateCreate(t.Context()); err != nil {
		t.Fatal(err)
	}
	SetValidatorDeps(ValidatorDeps{AllowIAMApplicationWrites: true, WIFConfigTemplate: func(context.Context, string) (*WIFConfigTemplate, error) { return w, nil }})
	t.Cleanup(func() { SetValidatorDeps(ValidatorDeps{}) })
	c := wifTestCluster(wifTestRef(t, w))
	if err := c.ValidateCreate(t.Context()); err == nil || !strings.Contains(err.Error(), "naming limit") {
		t.Fatalf("contract naming bound not enforced: %v", err)
	}
}
