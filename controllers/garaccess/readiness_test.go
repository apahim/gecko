package garaccess

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGrantReady(t *testing.T) {
	tests := []struct {
		name    string
		path    []string
		value   interface{}
		mutate  func(*unstructured.Unstructured)
		want    bool
		wantErr string
	}{
		{name: "active owned unconditional grant", want: true},
		{name: "pending without status", path: []string{"status"}},
		{name: "Ready false", path: []string{"status", "conditions"}, value: readinessConditions("False")},
		{name: "Ready unknown", path: []string{"status", "conditions"}, value: readinessConditions("Unknown")},
		{name: "missing conditions", path: []string{"status", "conditions"}},
		{name: "empty conditions", path: []string{"status", "conditions"}, value: []interface{}{}},
		{name: "unrelated condition", path: []string{"status", "conditions"}, value: []interface{}{map[string]interface{}{"type": "Synced", "status": "True"}}},
		{name: "malformed conditions", path: []string{"status", "conditions"}, value: "Ready=True", wantErr: "read GAR grant conditions"},
		{name: "malformed condition entry", path: []string{"status", "conditions"}, value: []interface{}{"Ready=True"}},
		{name: "missing condition status", path: []string{"status", "conditions"}, value: []interface{}{map[string]interface{}{"type": "Ready"}}},
		{name: "boolean condition status", path: []string{"status", "conditions"}, value: readinessConditions(true)},
		{name: "stale generation", path: []string{"status", "observedGeneration"}, value: int64(6)},
		{name: "future generation", path: []string{"status", "observedGeneration"}, value: int64(8)},
		{name: "missing observed generation", path: []string{"status", "observedGeneration"}},
		{name: "malformed observed generation", path: []string{"status", "observedGeneration"}, value: "7", wantErr: "read GAR grant observed generation"},
		{name: "zero metadata generation", mutate: func(obj *unstructured.Unstructured) {
			obj.SetGeneration(0)
			obj.Object["status"].(map[string]interface{})["observedGeneration"] = int64(0)
		}},
		{name: "deleting", mutate: func(obj *unstructured.Unstructured) {
			now := metav1.Now()
			obj.SetFinalizers([]string{"cnrm.cloud.google.com/finalizer"})
			obj.SetDeletionTimestamp(&now)
		}},
		{name: "draining", path: []string{"metadata", "annotations", stateAnnotation}, value: "draining"},
		{name: "inactive", path: []string{"metadata", "annotations", stateAnnotation}, value: "inactive"},
		{name: "missing active state", mutate: func(obj *unstructured.Unstructured) { obj.SetAnnotations(nil) }},
		{name: "benign defaulted spec field", path: []string{"spec", "extraDefault"}, value: "default", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, cluster, grant := readinessFixture()
			if tt.path != nil {
				if tt.value == nil {
					unstructured.RemoveNestedField(grant.Object, tt.path...)
				} else {
					require.NoError(t, unstructured.SetNestedField(grant.Object, tt.value, tt.path...))
				}
			}
			if tt.mutate != nil {
				tt.mutate(grant)
			}
			reader := fake.NewClientBuilder().WithObjects(grant).Build()
			got, err := GrantReady(context.Background(), reader, config, cluster)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func TestGrantReadyRejectsUnexpectedGrant(t *testing.T) {
	tests := []struct {
		name   string
		path   []string
		value  interface{}
		mutate func(*unstructured.Unstructured)
	}{
		{name: "role", path: []string{"spec", "role"}, value: "roles/owner"},
		{name: "member", path: []string{"spec", "member"}, value: "serviceAccount:other@example.iam.gserviceaccount.com"},
		{name: "repository", path: []string{"spec", "resourceRef", "external"}, value: "projects/other/locations/us-central1/repositories/quay"},
		{name: "resource kind", path: []string{"spec", "resourceRef", "kind"}, value: "Project"},
		{name: "resource API version", path: []string{"spec", "resourceRef", "apiVersion"}, value: "resourcemanager.cnrm.cloud.google.com/v1beta1"},
		{name: "named repository", path: []string{"spec", "resourceRef", "name"}, value: "quay"},
		{name: "namespaced repository", path: []string{"spec", "resourceRef", "namespace"}, value: "other"},
		{name: "alternate member", path: []string{"spec", "memberFrom"}, value: map[string]interface{}{"serviceAccountRef": map[string]interface{}{"name": "other"}}},
		{name: "conditional grant", path: []string{"spec", "condition"}, value: map[string]interface{}{"title": "temporary", "expression": "true"}},
		{name: "missing role", path: []string{"spec", "role"}},
		{name: "malformed role", path: []string{"spec", "role"}, value: int64(1)},
		{name: "wrong managed label", path: []string{"metadata", "labels", managedLabel}, value: "other-controller"},
		{name: "missing managed label", path: []string{"metadata", "labels", managedLabel}},
		{name: "wrong project label", path: []string{"metadata", "labels", projectLabel}, value: "other-project"},
		{name: "missing project label", path: []string{"metadata", "labels", projectLabel}},
		{name: "owner reference", mutate: func(obj *unstructured.Unstructured) {
			obj.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "consumer", UID: "consumer-uid"}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, cluster, grant := readinessFixture()
			if tt.path != nil {
				if tt.value == nil {
					unstructured.RemoveNestedField(grant.Object, tt.path...)
				} else {
					require.NoError(t, unstructured.SetNestedField(grant.Object, tt.value, tt.path...))
				}
			}
			if tt.mutate != nil {
				tt.mutate(grant)
			}
			reader := fake.NewClientBuilder().WithObjects(grant).Build()
			got, err := GrantReady(context.Background(), reader, config, cluster)
			require.False(t, got)
			require.ErrorContains(t, err, "does not match the expected GAR Reader grant")
		})
	}
}

func TestGrantReadyReadErrors(t *testing.T) {
	config, cluster, _ := readinessFixture()
	t.Run("missing grant is not ready", func(t *testing.T) {
		got, err := GrantReady(context.Background(), fake.NewClientBuilder().Build(), config, cluster)
		require.NoError(t, err)
		require.False(t, got)
	})
	t.Run("API failure is propagated", func(t *testing.T) {
		readErr := errors.New("regional API unavailable")
		reader := &readinessErrorReader{err: readErr}
		got, err := GrantReady(context.Background(), reader, config, cluster)
		require.False(t, got)
		require.ErrorIs(t, err, readErr)
		require.ErrorContains(t, err, "get GAR Reader grant readiness")
	})
}

func TestGrantReadyRejectsInvalidInputWithoutReads(t *testing.T) {
	config, cluster, _ := readinessFixture()
	mismatched := cluster.DeepCopy()
	mismatched.Namespace = "other-project"
	tests := []struct {
		name    string
		config  Config
		cluster *privatev1.Cluster
		wantErr string
	}{
		{name: "invalid repository", config: Config{IAMNamespace: DefaultIAMNamespace}, cluster: cluster, wantErr: "gar-repository"},
		{name: "invalid namespace", config: Config{Repository: config.Repository}, cluster: cluster, wantErr: "iam-namespace"},
		{name: "nil Cluster", config: config, wantErr: "must specify its GCP project"},
		{name: "missing GCP", config: config, cluster: &privatev1.Cluster{}, wantErr: "must specify its GCP project"},
		{name: "project mismatch", config: config, cluster: mismatched, wantErr: "must match"},
		{name: "invalid project number", config: config, cluster: identityCluster("customer-project", "0"), wantErr: "positive decimal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &readinessErrorReader{err: errors.New("unexpected read")}
			got, err := GrantReady(context.Background(), reader, tt.config, tt.cluster)
			require.False(t, got)
			require.ErrorContains(t, err, tt.wantErr)
			require.Zero(t, reader.reads, "invalid input must not query the regional API")
		})
	}
}

func readinessFixture() (Config, *privatev1.Cluster, *unstructured.Unstructured) {
	config := Config{
		Repository:   "projects/registry-project/locations/us-central1/repositories/quay",
		IAMNamespace: DefaultIAMNamespace,
	}
	cluster := identityCluster("customer-project", "123456789012")
	grant := desiredGrant(config, cluster.Namespace,
		"principalSet://cloudresourcemanager.googleapis.com/projects/123456789012/type/ServiceAccount")
	grant.SetGeneration(7)
	grant.Object["status"] = map[string]interface{}{
		"observedGeneration": int64(7),
		"conditions":         readinessConditions("True"),
	}
	return config, cluster, grant
}

func readinessConditions(status interface{}) []interface{} {
	return []interface{}{map[string]interface{}{"type": "Ready", "status": status}}
}

type readinessErrorReader struct {
	client.Reader
	err   error
	reads int
}

func (r *readinessErrorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	r.reads++
	return r.err
}
