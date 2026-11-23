package garaccess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/util/logger"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func controllerConfig() Config {
	return Config{
		Repository:   "projects/platform-project/locations/us-central1/repositories/gcp-hcp-quay",
		IAMNamespace: "gecko-system",
	}
}

func controllerCluster(project, name, number string) *privatev1.Cluster {
	return &privatev1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: project, Name: name, UID: types.UID(name + "-uid"),
			Finalizers: []string{"test.gecko/worker-teardown"},
		},
		Spec: privatev1.ClusterSpec{
			Platform: privatev1.ClusterPlatformSpec{
				Type: "GCP",
				GCP: &privatev1.GCPClusterPlatform{
					ProjectID: project,
					WorkloadIdentity: privatev1.WorkloadIdentitySpec{
						ProjectNumber: number,
					},
				},
			},
		},
	}
}

func controllerReader(objects ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(setup.NewScheme()).
		WithStatusSubresource(&privatev1.Cluster{}).WithObjects(objects...).Build()
}

func controllerKCC(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	gvk := NewPolicyMember().GroupVersionKind()
	scheme.AddKnownTypeWithName(gvk, NewPolicyMember())
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("IAMPolicyMemberList"), &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(NewPolicyMember()).WithObjects(objects...).Build()
}

func controllerGrant(config Config, cluster *privatev1.Cluster) *unstructured.Unstructured {
	member := "principalSet://cloudresourcemanager.googleapis.com/projects/" +
		cluster.Spec.Platform.GCP.WorkloadIdentity.ProjectNumber + "/type/ServiceAccount"
	policy := desiredGrant(config, cluster.Namespace, member)
	policy.SetUID(types.UID(policy.GetName() + "-uid"))
	policy.SetGeneration(1)
	return policy
}

func controllerReconciler(t *testing.T, reader client.Reader, kcc client.Client, config Config) *Reconciler {
	t.Helper()
	log, err := logger.NewLogger(logger.Config{Level: "info", Format: logger.FormatText, Writer: io.Discard})
	require.NoError(t, err)
	rec, err := New(log, reader, kcc, config)
	require.NoError(t, err)
	return rec
}

func controllerPolicies(t *testing.T, kcc client.Client) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(NewPolicyMember().GroupVersionKind().GroupVersion().WithKind("IAMPolicyMemberList"))
	require.NoError(t, kcc.List(context.Background(), list))
	return list.Items
}

func TestReconcilerRejectsMissingConfigurationAndProject(t *testing.T) {
	rec, err := New(nil, nil, nil, Config{})
	require.ErrorContains(t, err, "gar-repository")
	require.Nil(t, rec)
	rec, err = New(nil, nil, nil, controllerConfig())
	require.NoError(t, err)
	_, err = rec.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "cluster-without-project"},
	})
	require.ErrorContains(t, err, "customer project namespace is empty")
}

func TestReconcileCreatesSharedGrantForExistingClusters(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	first := controllerCluster("customer-project", "cluster-z", "123456789")
	second := controllerCluster("customer-project", "cluster-a", "123456789")
	reader, kcc := controllerReader(first, second), controllerKCC()
	rec := controllerReconciler(t, reader, kcc, config)
	request := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: first.Namespace}}

	result, err := rec.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, retryPending, result.RequeueAfter)
	policies := controllerPolicies(t, kcc)
	require.Len(t, policies, 1, "existing Clusters without NodePools must share one grant")
	policy := &policies[0]
	require.Regexp(t, `^gar-quay-reader-[a-f0-9]{24}$`, policy.GetName())
	require.Equal(t, config.IAMNamespace, policy.GetNamespace())
	require.Empty(t, policy.GetOwnerReferences())
	require.Equal(t, managedBy, policy.GetLabels()[managedLabel])
	require.Equal(t, first.Namespace, policy.GetLabels()[projectLabel])
	require.Equal(t, "cluster-a\ncluster-z", policy.GetAnnotations()[consumersKey])
	require.Equal(t, "active", policy.GetAnnotations()[stateAnnotation])
	require.Equal(t, map[string]interface{}{
		"member": "principalSet://cloudresourcemanager.googleapis.com/projects/123456789/type/ServiceAccount",
		"role":   "roles/artifactregistry.reader",
		"resourceRef": map[string]interface{}{
			"apiVersion": "artifactregistry.cnrm.cloud.google.com/v1beta1",
			"kind":       "ArtifactRegistryRepository",
			"external":   config.Repository,
		},
	}, policy.Object["spec"])

	// A fresh reconciler and pending KCC status must not duplicate or rewrite it.
	before := policy.DeepCopy()
	result, err = controllerReconciler(t, reader, kcc, config).Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, reconcile.Result{}, result)
	policies = controllerPolicies(t, kcc)
	require.Len(t, policies, 1)
	require.Equal(t, before.Object, policies[0].Object)
}

func TestReconcileSeparatesCustomerProjectsAndRepositories(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	first := controllerCluster("customer-one", "cluster", "123")
	second := controllerCluster("customer-two", "cluster", "456")
	reader, kcc := controllerReader(first, second), controllerKCC()
	rec := controllerReconciler(t, reader, kcc, config)
	for _, cluster := range []*privatev1.Cluster{first, second} {
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}})
		require.NoError(t, err)
	}
	otherRepository := config
	otherRepository.Repository = "projects/other-platform/locations/us-west1/repositories/gcp-hcp-quay"
	_, err := controllerReconciler(t, reader, kcc, otherRepository).Reconcile(ctx,
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: first.Namespace}})
	require.NoError(t, err)
	policies := controllerPolicies(t, kcc)
	require.Len(t, policies, 3)
	names := map[string]bool{}
	for i := range policies {
		require.False(t, names[policies[i].GetName()], "each grant tuple needs a distinct name")
		names[policies[i].GetName()] = true
	}
}

func TestReconcileRetainsGrantUntilLastClusterIsGone(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	first := controllerCluster("customer-project", "cluster-a", "123")
	second := controllerCluster("customer-project", "cluster-b", "123")
	policy := controllerGrant(config, first)
	policy.SetFinalizers([]string{"cnrm.cloud.google.com/finalizer"})
	policy.SetAnnotations(map[string]string{stateAnnotation: "active", consumersKey: "cluster-a\ncluster-b"})
	reader, kcc := controllerReader(first, second), controllerKCC(policy)
	rec := controllerReconciler(t, reader, kcc, config)
	request := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: first.Namespace}}
	for _, cluster := range []*privatev1.Cluster{first, second} {
		require.NoError(t, reader.Delete(ctx, cluster))
		_, err := rec.Reconcile(ctx, request)
		require.NoError(t, err)
		stored := NewPolicyMember()
		require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
		require.True(t, stored.GetDeletionTimestamp().IsZero(), "deleting Clusters still consume the grant")

		require.NoError(t, reader.Get(ctx, client.ObjectKeyFromObject(cluster), cluster))
		cluster.Finalizers = nil
		require.NoError(t, reader.Update(ctx, cluster))
		_, err = rec.Reconcile(ctx, request)
		require.NoError(t, err)
		require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
		if cluster == first {
			require.True(t, stored.GetDeletionTimestamp().IsZero())
			require.Equal(t, second.Name, stored.GetAnnotations()[consumersKey])
			continue
		}
		require.False(t, stored.GetDeletionTimestamp().IsZero(), "last teardown requests KCC cleanup")
		require.Equal(t, "draining", stored.GetAnnotations()[stateAnnotation])
		require.Empty(t, stored.GetAnnotations()[consumersKey])
		result, err := rec.Reconcile(ctx, request)
		require.NoError(t, err)
		require.Equal(t, retryPending, result.RequeueAfter, "wait for KCC finalization")
		stored.SetFinalizers(nil) // Simulate KCC completing its own finalization.
		require.NoError(t, kcc.Update(ctx, stored))
	}
	result, err := rec.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, reconcile.Result{}, result)
	require.Empty(t, controllerPolicies(t, kcc))
}

func TestReconcileRetainsUnreadyClusterConsumers(t *testing.T) {
	for _, status := range []metav1.ConditionStatus{metav1.ConditionFalse, metav1.ConditionUnknown} {
		t.Run(string(status), func(t *testing.T) {
			cluster := controllerCluster("customer-project", "cluster", "123")
			cluster.Status.Conditions = []metav1.Condition{{Type: "HostedClusterAvailable", Status: status}}
			policy := controllerGrant(controllerConfig(), cluster)
			reader, kcc := controllerReader(cluster), controllerKCC(policy)
			_, err := controllerReconciler(t, reader, kcc, controllerConfig()).Reconcile(context.Background(),
				reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}})
			require.NoError(t, err)
			stored := NewPolicyMember()
			require.NoError(t, kcc.Get(context.Background(), client.ObjectKeyFromObject(policy), stored))
			require.True(t, stored.GetDeletionTimestamp().IsZero())
			require.Equal(t, cluster.Name, stored.GetAnnotations()[consumersKey])
		})
	}
}

func TestReconcilePreservesKCCStatusAndUnownedMetadata(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	cluster := controllerCluster("customer-project", "cluster", "123")
	policy := controllerGrant(config, cluster)
	policy.SetLabels(map[string]string{managedLabel: managedBy, projectLabel: cluster.Namespace, "infra-id": "regional-one"})
	policy.SetAnnotations(map[string]string{stateAnnotation: "draining", "operator-note": "keep"})
	status := map[string]interface{}{
		"observedGeneration": int64(1),
		"conditions": []interface{}{map[string]interface{}{
			"type": "Ready", "status": "False", "reason": "PermissionDenied",
		}},
	}
	policy.Object["status"] = status
	reader, kcc := controllerReader(cluster), controllerKCC(policy)
	result, err := controllerReconciler(t, reader, kcc, config).Reconcile(ctx,
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}})
	require.NoError(t, err, "KCC failure must not turn grant declaration reconciliation into a provisioning gate")
	require.Equal(t, reconcile.Result{}, result)
	stored := NewPolicyMember()
	require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
	require.Equal(t, status, stored.Object["status"])
	require.Equal(t, "regional-one", stored.GetLabels()["infra-id"])
	require.Equal(t, "keep", stored.GetAnnotations()["operator-note"])
	require.Equal(t, "active", stored.GetAnnotations()[stateAnnotation])
	require.Equal(t, cluster.Name, stored.GetAnnotations()[consumersKey])
}

func TestReconcileRejectsInvalidInventoryWithoutChangingGrants(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*privatev1.Cluster)
	}{
		{"conflicting-project-number", func(c *privatev1.Cluster) { c.Spec.Platform.GCP.WorkloadIdentity.ProjectNumber = "456" }},
		{"invalid-project-number", func(c *privatev1.Cluster) { c.Spec.Platform.GCP.WorkloadIdentity.ProjectNumber = "0123" }},
		{"project-namespace-mismatch", func(c *privatev1.Cluster) { c.Spec.Platform.GCP.ProjectID = "other-project" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			config := controllerConfig()
			first := controllerCluster("customer-project", "cluster-a", "123")
			second := controllerCluster("customer-project", "cluster-b", "123")
			test.change(second)
			policy := controllerGrant(config, first)
			reader, kcc := controllerReader(first, second), controllerKCC(policy)
			before := controllerPolicies(t, kcc)
			_, err := controllerReconciler(t, reader, kcc, config).Reconcile(ctx,
				reconcile.Request{NamespacedName: client.ObjectKey{Namespace: first.Namespace}})
			require.Error(t, err)
			require.Equal(t, before, controllerPolicies(t, kcc))
		})
	}
}

func TestReconcileLeavesUnrelatedDeclarationsUntouched(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	cluster := controllerCluster("customer-project", "cluster", "123")
	owned := controllerGrant(config, cluster)
	unmanaged := owned.DeepCopy()
	unmanaged.SetName("operator-managed")
	unmanaged.SetLabels(map[string]string{projectLabel: cluster.Namespace})
	otherProject := controllerGrant(config, controllerCluster("another-project", "another-cluster", "456"))
	otherNamespace := owned.DeepCopy()
	otherNamespace.SetNamespace("another-namespace")
	otherConfig := config
	otherConfig.Repository = "projects/other-platform/locations/us-west1/repositories/gcp-hcp-quay"
	otherRepository := controllerGrant(otherConfig, cluster)
	kcc := controllerKCC(owned, unmanaged, otherProject, otherNamespace, otherRepository)
	_, err := controllerReconciler(t, controllerReader(), kcc, config).Reconcile(ctx,
		reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(kcc.Get(ctx, client.ObjectKeyFromObject(owned), NewPolicyMember())))
	for _, policy := range []*unstructured.Unstructured{unmanaged, otherProject, otherNamespace, otherRepository} {
		stored := NewPolicyMember()
		require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
		require.Equal(t, policy.Object["spec"], stored.Object["spec"])
		require.True(t, stored.GetDeletionTimestamp().IsZero())
	}
}

func TestReconcileRefusesUnexpectedOwnedDeclarations(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*unstructured.Unstructured)
	}{
		{"wrong-role", func(p *unstructured.Unstructured) {
			p.Object["spec"].(map[string]interface{})["role"] = "roles/artifactregistry.writer"
		}},
		{"unexpected-name", func(p *unstructured.Unstructured) { p.SetName("unexpected-reader") }},
		{"owner-reference", func(p *unstructured.Unstructured) {
			p.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner-uid"}})
		}},
		{"condition", func(p *unstructured.Unstructured) {
			p.Object["spec"].(map[string]interface{})["condition"] = map[string]interface{}{"title": "limited", "expression": "false"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			config := controllerConfig()
			cluster := controllerCluster("customer-project", "cluster", "123")
			policy := controllerGrant(config, cluster)
			test.change(policy)
			kcc := controllerKCC(policy)
			before := controllerPolicies(t, kcc)
			_, err := controllerReconciler(t, controllerReader(), kcc, config).Reconcile(ctx,
				reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}})
			require.ErrorContains(t, err, "refuse to delete")
			require.Equal(t, before, controllerPolicies(t, kcc))
		})
	}
}

func TestReconcileRejectsMalformedMemberWithoutChangingGrant(t *testing.T) {
	for _, test := range []struct {
		name   string
		member interface{}
	}{
		{"non-string-member", int64(123)},
		{"member-object", map[string]interface{}{"unexpected": "member"}},
		{"missing-member", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			config := controllerConfig()
			cluster := controllerCluster("customer-project", "cluster", "123")
			policy := controllerGrant(config, cluster)
			spec := policy.Object["spec"].(map[string]interface{})
			original := spec["member"]
			if test.member == nil {
				delete(spec, "member")
			} else {
				spec["member"] = test.member
			}
			kcc := controllerKCC(policy)
			before := controllerPolicies(t, kcc)
			rec := controllerReconciler(t, controllerReader(), kcc, config)
			request := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}}
			_, err := rec.Reconcile(ctx, request)
			require.ErrorContains(t, err, "spec.member")
			require.ErrorContains(t, err, policy.GetName())
			require.Equal(t, before, controllerPolicies(t, kcc))

			stored := NewPolicyMember()
			require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
			stored.Object["spec"].(map[string]interface{})["member"] = original
			require.NoError(t, kcc.Update(ctx, stored))
			_, err = rec.Reconcile(ctx, request)
			require.NoError(t, err, "correcting the member must allow normal cleanup")
			require.Empty(t, controllerPolicies(t, kcc))
		})
	}
}

func TestReconcilePreservesGrantWhenConsumerArrivesDuringCleanup(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "consumer-arrives"
		if unavailable {
			name = "inventory-unavailable"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			config := controllerConfig()
			cluster := controllerCluster("customer-project", "late-cluster", "123")
			policy := controllerGrant(config, cluster)
			reader, kcc := controllerReader(), controllerKCC(policy)
			inventoryErr := errors.New("inventory unavailable")
			injected := false
			guarded := interceptor.NewClient(reader, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					stored := NewPolicyMember()
					require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
					if stored.GetAnnotations()[stateAnnotation] == "draining" {
						injected = true
						if unavailable {
							return inventoryErr
						}
						require.NoError(t, c.Create(ctx, cluster))
					}
					return c.List(ctx, list, opts...)
				},
			})
			request := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}}
			result, err := controllerReconciler(t, guarded, kcc, config).Reconcile(ctx, request)
			if unavailable {
				require.ErrorIs(t, err, inventoryErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, retryPending, result.RequeueAfter)
			}
			require.True(t, injected, "exercise the live check after marking the grant draining")
			stored := NewPolicyMember()
			require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
			require.True(t, stored.GetDeletionTimestamp().IsZero())

			_, err = controllerReconciler(t, reader, kcc, config).Reconcile(ctx, request)
			require.NoError(t, err)
			if unavailable {
				require.True(t, apierrors.IsNotFound(kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored)))
			} else {
				require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
				require.Equal(t, "active", stored.GetAnnotations()[stateAnnotation])
				require.Equal(t, cluster.Name, stored.GetAnnotations()[consumersKey])
			}
		})
	}
}

func TestReconcileRetriesConcurrentGrantChanges(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	cluster := controllerCluster("customer-project", "cluster", "123")
	policy := controllerGrant(config, cluster)
	kcc := controllerKCC(policy)
	guarded := interceptor.NewClient(kcc, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			options := (&client.DeleteOptions{}).ApplyOptions(opts)
			require.NotNil(t, options.Preconditions)
			require.NotNil(t, options.Preconditions.UID)
			require.NotNil(t, options.Preconditions.ResourceVersion)
			require.Equal(t, policy.GetUID(), *options.Preconditions.UID)
			current := NewPolicyMember()
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), current))
			require.Equal(t, current.GetResourceVersion(), *options.Preconditions.ResourceVersion)
			annotations := current.GetAnnotations()
			annotations["concurrent-change"] = "preserve"
			current.SetAnnotations(annotations)
			require.NoError(t, c.Update(ctx, current))
			return c.Delete(ctx, obj, opts...)
		},
	})
	request := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}}
	_, err := controllerReconciler(t, controllerReader(), guarded, config).Reconcile(ctx, request)
	require.True(t, apierrors.IsConflict(err), "concurrent changes must invalidate the guarded deletion")
	stored := NewPolicyMember()
	require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
	require.Equal(t, "preserve", stored.GetAnnotations()["concurrent-change"])
	require.True(t, stored.GetDeletionTimestamp().IsZero())
	_, err = controllerReconciler(t, controllerReader(), kcc, config).Reconcile(ctx, request)
	require.NoError(t, err)
	require.Empty(t, controllerPolicies(t, kcc))
}

func TestReconcileRetriesAPIFailures(t *testing.T) {
	for _, test := range []struct {
		operation string
		consumer  bool
		grant     bool
	}{
		{"cluster-list", true, true},
		{"grant-get", true, false},
		{"grant-create", true, false},
		{"grant-update", true, true},
		{"grant-list", false, true},
		{"draining-update", false, true},
		{"grant-delete", false, true},
	} {
		t.Run(test.operation, func(t *testing.T) {
			ctx := context.Background()
			config := controllerConfig()
			cluster := controllerCluster("customer-project", "cluster", "123")
			reader, kcc := controllerReader(), controllerKCC()
			if test.consumer {
				require.NoError(t, reader.Create(ctx, cluster))
			}
			policy := controllerGrant(config, cluster)
			if test.grant {
				require.NoError(t, kcc.Create(ctx, policy))
			}
			apiErr := errors.New("API temporarily unavailable")
			guardedReader := interceptor.NewClient(reader, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if test.operation == "cluster-list" {
						return apiErr
					}
					return c.List(ctx, list, opts...)
				},
			})
			guardedKCC := interceptor.NewClient(kcc, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if test.operation == "grant-get" {
						return apiErr
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if test.operation == "grant-create" {
						return apiErr
					}
					return c.Create(ctx, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if test.operation == "grant-list" {
						return apiErr
					}
					return c.List(ctx, list, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if test.operation == "grant-update" || test.operation == "draining-update" {
						return apiErr
					}
					return c.Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if test.operation == "grant-delete" {
						return apiErr
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			request := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}}
			_, err := controllerReconciler(t, guardedReader, guardedKCC, config).Reconcile(ctx, request)
			require.ErrorIs(t, err, apiErr)
			if test.grant {
				stored := NewPolicyMember()
				require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored))
				require.Equal(t, policy.Object["spec"], stored.Object["spec"])
				require.True(t, stored.GetDeletionTimestamp().IsZero())
			} else {
				require.Empty(t, controllerPolicies(t, kcc))
			}

			// Removing the injected API failure must allow ordinary convergence.
			_, err = controllerReconciler(t, reader, kcc, config).Reconcile(ctx, request)
			require.NoError(t, err)
			if test.consumer {
				require.Len(t, controllerPolicies(t, kcc), 1)
			} else {
				require.Empty(t, controllerPolicies(t, kcc))
			}
		})
	}
}

func TestPolicyMemberEventsSelectOnlyThisRepositoryAndNamespace(t *testing.T) {
	config := controllerConfig()
	cluster := controllerCluster("customer-project", "cluster", "123")
	rec := controllerReconciler(t, controllerReader(), controllerKCC(), config)
	policy := controllerGrant(config, cluster)
	require.Equal(t, []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: cluster.Namespace}}},
		rec.PolicyMemberToProject(context.Background(), policy), "existing declarations must recover the project queue after restart")
	require.Empty(t, rec.PolicyMemberToProject(context.Background(), cluster))
	for _, change := range []func(*unstructured.Unstructured){
		func(p *unstructured.Unstructured) { p.SetNamespace("another-namespace") },
		func(p *unstructured.Unstructured) { p.SetLabels(map[string]string{projectLabel: cluster.Namespace}) },
		func(p *unstructured.Unstructured) { p.SetLabels(map[string]string{managedLabel: managedBy}) },
		func(p *unstructured.Unstructured) {
			p.Object["spec"].(map[string]interface{})["resourceRef"] = map[string]interface{}{"external": "another-repository"}
		},
	} {
		other := policy.DeepCopy()
		change(other)
		require.Empty(t, rec.PolicyMemberToProject(context.Background(), other))
	}
}

func TestPolicyMemberEventsLogMalformedRepository(t *testing.T) {
	for _, test := range []struct {
		name        string
		resourceRef interface{}
	}{
		{"non-string-repository", map[string]interface{}{"external": int64(123)}},
		{"non-object-resource-ref", "not-an-object"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			log, err := logger.NewLogger(logger.Config{Level: "info", Format: logger.FormatText, Writer: &output})
			require.NoError(t, err)
			config := controllerConfig()
			rec, err := New(log, nil, nil, config)
			require.NoError(t, err)
			policy := controllerGrant(config, controllerCluster("customer-project", "cluster", "123"))
			policy.Object["spec"].(map[string]interface{})["resourceRef"] = test.resourceRef

			require.Empty(t, rec.PolicyMemberToProject(context.Background(), policy))
			require.Contains(t, output.String(), "spec.resourceRef.external")
			require.Contains(t, output.String(), policy.GetNamespace()+"/"+policy.GetName())
		})
	}
}
