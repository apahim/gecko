package garaccess

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcileRetainsCollidingGrantUntilForeignConsumersDisappear(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	owner := controllerCluster("owner-project", "owner", "123")
	owner.Finalizers = nil
	reader, kcc := controllerReader(owner), controllerKCC()
	rec := controllerReconciler(t, reader, kcc, config)
	ownerRequest := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: owner.Namespace}}
	_, err := rec.Reconcile(ctx, ownerRequest)
	require.NoError(t, err)
	policies := controllerPolicies(t, kcc)
	require.Len(t, policies, 1)
	before := policies[0].DeepCopy()

	first := controllerCluster("foreign-project", "cluster-a", "123")
	first.Finalizers = nil
	last := controllerCluster(first.Namespace, "cluster-z", "123")
	last.Status.Conditions = []metav1.Condition{{Type: "HostedClusterAvailable", Status: metav1.ConditionFalse}}
	require.NoError(t, reader.Create(ctx, first))
	require.NoError(t, reader.Create(ctx, last))
	foreignRequest := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: first.Namespace}}
	_, err = rec.Reconcile(ctx, foreignRequest)
	require.ErrorContains(t, err, "does not match")
	require.Equal(t, []unstructured.Unstructured{*before}, controllerPolicies(t, kcc), "a collision must not adopt or rewrite the grant")
	require.NoError(t, reader.Delete(ctx, owner))

	assertRetained := func() *unstructured.Unstructured {
		t.Helper()
		result, err := rec.Reconcile(ctx, ownerRequest)
		require.NoError(t, err)
		require.Equal(t, time.Minute, result.RequeueAfter, "the original owner must schedule cleanup again")
		stored := NewPolicyMember()
		require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(before), stored))
		require.True(t, stored.GetDeletionTimestamp().IsZero())
		require.Equal(t, owner.Namespace, stored.GetLabels()[projectLabel])
		require.Equal(t, "draining", stored.GetAnnotations()[stateAnnotation])
		require.Equal(t, before.Object["spec"], stored.Object["spec"])
		return stored
	}
	held := assertRetained()
	require.Equal(t, held.Object, assertRetained().Object, "retention must not cause repeated metadata writes")
	require.NoError(t, reader.Delete(ctx, first))
	assertRetained()
	require.NoError(t, reader.Delete(ctx, last))
	assertRetained() // A deleting, unready Cluster without NodePools still retains access.
	require.NoError(t, reader.Get(ctx, client.ObjectKeyFromObject(last), last))
	last.Finalizers = nil
	require.NoError(t, reader.Update(ctx, last))

	_, err = rec.Reconcile(ctx, foreignRequest) // B's deletion event only queues B.
	require.NoError(t, err)
	require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(before), NewPolicyMember()))
	restarted := controllerReconciler(t, reader, kcc, config)
	require.Equal(t, []reconcile.Request{ownerRequest}, restarted.PolicyMemberToProject(ctx, held),
		"initial KCC events must recover owner cleanup after a restart")

	// Run A's scheduled retry after B's final consumer has disappeared.
	result, err := rec.Reconcile(ctx, ownerRequest)
	require.NoError(t, err)
	require.Equal(t, retryPending, result.RequeueAfter)
	require.Empty(t, controllerPolicies(t, kcc))
}

func TestReconcileCleanupMatchesForeignReferencesIndividually(t *testing.T) {
	for _, test := range []struct {
		name        string
		change      func(*privatev1.Cluster)
		retain      bool
		conflicting bool
	}{
		{"different-number", func(c *privatev1.Cluster) { c.Spec.Platform.GCP.WorkloadIdentity.ProjectNumber = "456" }, false, false},
		{"malformed-number", func(c *privatev1.Cluster) { c.Spec.Platform.GCP.WorkloadIdentity.ProjectNumber = "0123" }, false, false},
		{"missing-gcp", func(c *privatev1.Cluster) { c.Spec.Platform.GCP = nil }, false, false},
		{"matching-number-with-invalid-project-id", func(c *privatev1.Cluster) { c.Spec.Platform.GCP.ProjectID = "wrong-project" }, true, false},
		{"matching-number-in-conflicting-namespace", nil, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			config := controllerConfig()
			owner := controllerCluster("owner-project", "previous", "123")
			policy := controllerGrant(config, owner)
			foreign := controllerCluster("foreign-project", "cluster-a", "123")
			if test.change != nil {
				test.change(foreign)
			}
			reader := controllerReader(foreign, controllerCluster("unrelated-project", "malformed", "0"))
			if test.conflicting {
				require.NoError(t, reader.Create(ctx, controllerCluster(foreign.Namespace, "cluster-z", "456")))
			}
			kcc := controllerKCC(policy)
			result, err := controllerReconciler(t, reader, kcc, config).Reconcile(ctx,
				reconcile.Request{NamespacedName: client.ObjectKey{Namespace: owner.Namespace}})
			require.NoError(t, err)
			stored := NewPolicyMember()
			err = kcc.Get(ctx, client.ObjectKeyFromObject(policy), stored)
			if test.retain {
				require.NoError(t, err)
				require.Equal(t, time.Minute, result.RequeueAfter)
				require.True(t, stored.GetDeletionTimestamp().IsZero())
				require.Equal(t, owner.Namespace, stored.GetLabels()[projectLabel])
				require.Equal(t, policy.Object["spec"], stored.Object["spec"])
			} else {
				require.True(t, apierrors.IsNotFound(err))
				require.Equal(t, retryPending, result.RequeueAfter)
			}
		})
	}
}

func TestReconcileCleanupContinuesPastRetainedAndTerminatingGrants(t *testing.T) {
	ctx := context.Background()
	config := controllerConfig()
	owner := controllerCluster("owner-project", "previous", "123")
	held := controllerGrant(config, owner)
	terminating := controllerGrant(config, controllerCluster(owner.Namespace, "previous", "456"))
	terminating.SetFinalizers([]string{"cnrm.cloud.google.com/finalizer"})
	unused := controllerGrant(config, controllerCluster(owner.Namespace, "previous", "789"))
	kcc := controllerKCC(held, terminating, unused)
	require.NoError(t, kcc.Delete(ctx, terminating))
	order := map[string]int{held.GetName(): 0, terminating.GetName(): 1, unused.GetName(): 2}
	guarded := interceptor.NewClient(kcc, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			policies := list.(*unstructured.UnstructuredList)
			sort.Slice(policies.Items, func(i, j int) bool {
				return order[policies.Items[i].GetName()] < order[policies.Items[j].GetName()]
			})
			return nil
		},
	})
	reader := controllerReader(controllerCluster("foreign-project", "consumer", "123"))
	rec := controllerReconciler(t, reader, guarded, config)
	request := reconcile.Request{NamespacedName: client.ObjectKey{Namespace: owner.Namespace}}
	result, err := rec.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, retryPending, result.RequeueAfter, "retain the post-delete follow-up for other cleanup candidates")
	require.True(t, apierrors.IsNotFound(kcc.Get(ctx, client.ObjectKeyFromObject(unused), NewPolicyMember())))
	require.NoError(t, kcc.Get(ctx, client.ObjectKeyFromObject(held), NewPolicyMember()))
	result, err = rec.Reconcile(ctx, request)
	require.NoError(t, err)
	require.Equal(t, time.Minute, result.RequeueAfter, "a terminating candidate must not swallow the retained grant's retry")
}
