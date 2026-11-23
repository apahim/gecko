package nodepool

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/client/transport/mock"
	"github.com/openshift-online/gecko/controllers/util/constants"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Unused methods on the nil embedded client fail on unexpected transport calls.
type garAccessTestTransport struct {
	transport.Client
	inventory      *transport.DeleteStatus
	inventoryErr   error
	inventoryCalls int
	applyCalls     int
	applyStatus    *transport.Status
}

func (tr *garAccessTestTransport) GetDeleteStatus(context.Context, string, string) (*transport.DeleteStatus, error) {
	tr.inventoryCalls++
	return tr.inventory, tr.inventoryErr
}

func (tr *garAccessTestTransport) Apply(context.Context, string, string, [][]byte) (*transport.Status, error) {
	tr.applyCalls++
	return tr.applyStatus, nil
}

func newGARAccessTestReconciler(t *testing.T, np *privatev1.NodePool, cluster *privatev1.Cluster) (*Reconciler, *mockStoreClient, *garAccessTestTransport) {
	t.Helper()
	tr := &garAccessTestTransport{
		inventory: &transport.DeleteStatus{},
		applyStatus: &transport.Status{
			Conditions: []metav1.Condition{{Type: "Applied", Status: metav1.ConditionTrue, Reason: "Applied"}},
			ResourceStatuses: map[string]map[string]string{
				nodePoolResourceKey(cluster, np): {
					"readyCondition": "True", "allNodesHealthyCondition": "True", "allMachinesReadyCondition": "True",
				},
			},
		},
	}
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	return r, store, tr
}

func TestReconcile_GARAccessAllowsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		enabled, installed, dispatched bool
		ready                          bool
		wantInventory, wantGate        int
	}{
		{name: "disabled by default"},
		{name: "installed bypasses unavailable GAR", enabled: true, installed: true},
		{name: "persisted dispatch before status acknowledgement", enabled: true, dispatched: true, wantInventory: 1},
		{name: "new pool with ready grant", enabled: true, ready: true, wantInventory: 1, wantGate: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			np := testNodePool("4.16.0")
			if tc.installed {
				np.Status.Version = "4.16.0"
			}
			r, store, tr := newGARAccessTestReconciler(t, np, testCluster(true, true))
			if tc.dispatched {
				tr.inventory.ApplyDesiresCount = 1
				require.Empty(t, np.Status.Version)
				require.Empty(t, np.Status.Conditions)
			}
			gateCalls := 0
			if tc.enabled {
				r.WithGARAccessGate(func(context.Context, *privatev1.Cluster) (bool, error) {
					gateCalls++
					if !tc.ready {
						return false, fmt.Errorf("KCC unavailable")
					}
					return true, nil
				})
			}
			result, err := r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
			require.NoError(t, err)
			require.Equal(t, 5*time.Minute, result.RequeueAfter)
			require.Equal(t, 1, tr.applyCalls)
			require.Equal(t, tc.wantInventory, tr.inventoryCalls)
			require.Equal(t, tc.wantGate, gateCalls)
			captured := store.statusWriter.captured.(*privatev1.NodePool)
			require.True(t, meta.IsStatusConditionTrue(captured.Status.Conditions, "NodePoolAvailable"))
			require.True(t, meta.IsStatusConditionTrue(captured.Status.Conditions, "NodePoolResourcesApplied"))
		})
	}
}

func TestReconcile_GARAccessPendingIsIdempotentAndRecovers(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Generation = 7
	// A waiting condition does not prove that transport has dispatched anything.
	np.Status.Conditions = []metav1.Condition{{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionUnknown, Reason: "VersionResolutionNotReady",
	}}
	r, store, tr := newGARAccessTestReconciler(t, np, testCluster(true, true))
	ready := false
	r.WithGARAccessGate(func(context.Context, *privatev1.Cluster) (bool, error) { return ready, nil })

	result, err := r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)
	require.Zero(t, tr.applyCalls)
	captured := store.statusWriter.captured.(*privatev1.NodePool)
	for _, conditionType := range []string{"NodePoolResourcesApplied", "NodePoolAvailable", "NodePoolProgressing"} {
		condition := meta.FindStatusCondition(captured.Status.Conditions, conditionType)
		require.NotNil(t, condition)
		require.Equal(t, metav1.ConditionUnknown, condition.Status)
		require.Equal(t, "GARAccessPending", condition.Reason)
		require.Equal(t, np.Generation, condition.ObservedGeneration)
	}
	// Persist the first result; the existing store mock otherwise only captures it.
	store.nodepool = captured.DeepCopy()
	before := store.nodepool.DeepCopy()
	store.statusWriter = &mockStatusWriter{}
	result, err = r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)
	require.Zero(t, tr.applyCalls)
	require.False(t, store.statusWriter.called, "unchanged pending status should not be rewritten")
	require.Equal(t, before.Status, store.nodepool.Status)

	ready = true
	result, err = r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, result.RequeueAfter)
	require.Equal(t, 1, tr.applyCalls)
	captured = store.statusWriter.captured.(*privatev1.NodePool)
	require.True(t, meta.IsStatusConditionTrue(captured.Status.Conditions, "NodePoolAvailable"))
	require.True(t, meta.IsStatusConditionTrue(captured.Status.Conditions, "NodePoolResourcesApplied"))
}

func TestReconcile_GARAccessFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inventory    *transport.DeleteStatus
		inventoryErr error
		gateErr      error
		wantError    string
	}{
		{name: "gate error even when ready", inventory: &transport.DeleteStatus{}, gateErr: fmt.Errorf("KCC unavailable"), wantError: "KCC unavailable"},
		{name: "dispatch inventory error", inventoryErr: fmt.Errorf("inventory unavailable"), wantError: "inventory unavailable"},
		{name: "nil dispatch inventory", wantError: "dispatch inventory is unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			np := testNodePool("4.16.0")
			r, store, tr := newGARAccessTestReconciler(t, np, testCluster(true, true))
			tr.inventory, tr.inventoryErr = tc.inventory, tc.inventoryErr
			r.WithGARAccessGate(func(context.Context, *privatev1.Cluster) (bool, error) { return true, tc.gateErr })
			_, err := r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
			require.ErrorContains(t, err, tc.wantError)
			require.Zero(t, tr.applyCalls)
			require.False(t, store.statusWriter.called, "dependency errors must not report resources applied")
		})
	}
}

func TestReconcile_GARAccessRetainsPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*privatev1.NodePool, *privatev1.Cluster)
		reason string
	}{
		{name: "finalizer", change: func(np *privatev1.NodePool, _ *privatev1.Cluster) { np.Finalizers = nil }},
		{name: "placement", change: func(_ *privatev1.NodePool, c *privatev1.Cluster) { c.Status.PlacementResult = nil }, reason: "PlacementNotReady"},
		{name: "empty management cluster", change: func(_ *privatev1.NodePool, c *privatev1.Cluster) { c.Status.PlacementResult.ManagementClusterName = "" }, reason: "PlacementNotReady"},
		{name: "version resolution", change: func(np *privatev1.NodePool, _ *privatev1.Cluster) { np.Status.VersionResolution = nil }, reason: "VersionResolutionNotReady"},
		{name: "empty resolved version", change: func(np *privatev1.NodePool, _ *privatev1.Cluster) { np.Status.VersionResolution.ReleaseVersion = "" }, reason: "VersionResolutionNotReady"},
		{name: "version mismatch", change: func(np *privatev1.NodePool, _ *privatev1.Cluster) {
			np.Status.VersionResolution.ReleaseVersion = "4.15.0"
		}, reason: "VersionMismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			np, cluster := testNodePool("4.16.0"), testCluster(true, true)
			tc.change(np, cluster)
			r, store, tr := newGARAccessTestReconciler(t, np, cluster)
			r.WithGARAccessGate(func(context.Context, *privatev1.Cluster) (bool, error) { return true, nil })
			result, err := r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
			require.NoError(t, err)
			require.Zero(t, tr.applyCalls)
			if tc.reason == "" {
				require.Zero(t, result.RequeueAfter)
				require.True(t, store.updateCalled)
				require.Contains(t, store.updated.GetFinalizers(), constants.FinalizerNodePool)
				return
			}
			require.Equal(t, 15*time.Second, result.RequeueAfter)
			captured := store.statusWriter.captured.(*privatev1.NodePool)
			condition := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolResourcesApplied")
			require.NotNil(t, condition)
			require.Equal(t, metav1.ConditionUnknown, condition.Status)
			require.Equal(t, tc.reason, condition.Reason)
		})
	}
}

func TestReconcile_DeletionBypassesGARAccess(t *testing.T) {
	for _, gateErr := range []error{nil, fmt.Errorf("KCC unavailable")} {
		t.Run(fmt.Sprintf("gate error %v", gateErr), func(t *testing.T) {
			np := testNodePool("") // Deletion also bypasses version resolution.
			now := metav1.Now()
			np.DeletionTimestamp = &now
			np.Status.Conditions = []metav1.Condition{{
				Type: "NodePoolResourcesApplied", Status: metav1.ConditionUnknown, Reason: "GARAccessPending",
			}}
			tr := mock.New()
			key := "mc-us-c1/" + mustNodePoolGroupKey(np.Namespace, np.Spec.ClusterID, np.Name)
			tr.DeleteStatusOverrides[key] = &transport.DeleteStatus{ApplyDesiresCount: 1}
			r, store := buildReconciler(t, np, testCluster(true, true), tr, nil, nil, nil)
			gateCalls := 0
			r.WithGARAccessGate(func(context.Context, *privatev1.Cluster) (bool, error) {
				gateCalls++
				return false, gateErr
			})
			result, err := r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
			require.NoError(t, err)
			require.Equal(t, 15*time.Second, result.RequeueAfter)
			require.Len(t, tr.DeleteCalls, 1)
			require.False(t, store.updateCalled, "retain finalizer until deletion completes")

			tr.DeleteStatusOverrides[key] = &transport.DeleteStatus{AllSuccessful: true, TotalCount: 1}
			result, err = r.Reconcile(context.Background(), npReq(np.Namespace, np.Name))
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)
			require.Len(t, tr.CleanupDeleteDesiresCalls, 1)
			require.NotContains(t, store.updated.GetFinalizers(), constants.FinalizerNodePool)
			require.Empty(t, tr.ApplyCalls)
			require.Zero(t, gateCalls)
		})
	}
}
