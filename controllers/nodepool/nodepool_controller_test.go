package nodepool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/client/transport/mock"
	"github.com/openshift-online/gecko/controllers/util/constants"
	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestLogger(t *testing.T) logger.Logger {
	t.Helper()
	log, err := logger.NewLogger(logger.Config{
		Level:     "debug",
		Format:    logger.FormatText,
		Output:    "stdout",
		Component: "test",
		Version:   "test",
	})
	require.NoError(t, err)
	return log
}

// mockStatusWriter captures Status().Update and Status().Patch calls and can
// return a configured error.
type mockStatusWriter struct {
	called    bool
	updates   int
	updateErr error
	captured  client.Object
	patchData []byte
	patchErr  error
}

func (m *mockStatusWriter) Update(_ context.Context, obj client.Object, _ ...client.SubResourceUpdateOption) error {
	m.called = true
	m.updates++
	m.captured = obj
	return m.updateErr
}
func (m *mockStatusWriter) Create(_ context.Context, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
	return nil
}
func (m *mockStatusWriter) Patch(_ context.Context, obj client.Object, patch client.Patch, _ ...client.SubResourcePatchOption) error {
	m.called = true
	m.captured = obj
	// Capture the serialized patch payload: a MergeFrom snapshot taken after
	// the in-place mutation produces an empty diff that persists nothing
	// against a real API server even though the passed object is mutated.
	data, err := patch.Data(obj)
	m.patchData, m.patchErr = data, err
	return m.updateErr
}
func (m *mockStatusWriter) Apply(_ context.Context, _ runtime.ApplyConfiguration, _ ...client.SubResourceApplyOption) error {
	return nil
}

// mockStoreClient is a minimal client.Client backed by a fixed NodePool and Cluster.
type mockStoreClient struct {
	nodepool     *privatev1.NodePool
	cluster      *privatev1.Cluster
	version      *privatev1.Version
	npGetErr     error
	clsGetErr    error
	statusWriter *mockStatusWriter
	updateCalled bool
	updateErr    error
	updated      client.Object
}

func (m *mockStoreClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch o := obj.(type) {
	case *privatev1.NodePool:
		if m.npGetErr != nil {
			return m.npGetErr
		}
		if m.nodepool == nil {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "nodepool"}, "")
		}
		*o = *m.nodepool
		return nil
	case *privatev1.Cluster:
		if m.clsGetErr != nil {
			return m.clsGetErr
		}
		if m.cluster == nil {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "cluster"}, "")
		}
		*o = *m.cluster
		return nil
	case *privatev1.Version:
		if m.version == nil {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "versions"}, "")
		}
		*o = *m.version
		return nil
	default:
		return fmt.Errorf("unexpected type %T", obj)
	}
}

func (m *mockStoreClient) Status() client.SubResourceWriter { return m.statusWriter }

func (m *mockStoreClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return nil
}
func (m *mockStoreClient) Create(_ context.Context, _ client.Object, _ ...client.CreateOption) error {
	return nil
}
func (m *mockStoreClient) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	return nil
}
func (m *mockStoreClient) Update(_ context.Context, obj client.Object, _ ...client.UpdateOption) error {
	m.updateCalled = true
	m.updated = obj
	return m.updateErr
}
func (m *mockStoreClient) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
	return nil
}
func (m *mockStoreClient) DeleteAllOf(_ context.Context, _ client.Object, _ ...client.DeleteAllOfOption) error {
	return nil
}
func (m *mockStoreClient) Apply(_ context.Context, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
	return nil
}
func (m *mockStoreClient) SubResource(_ string) client.SubResourceClient { return nil }
func (m *mockStoreClient) Scheme() *runtime.Scheme                       { return nil }
func (m *mockStoreClient) RESTMapper() meta.RESTMapper                   { return nil }
func (m *mockStoreClient) GroupVersionKindFor(_ runtime.Object) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, nil
}
func (m *mockStoreClient) IsObjectNamespaced(_ runtime.Object) (bool, error) { return false, nil }

// errTransport is a transport.Client that returns configurable errors.
type errTransport struct {
	applyErr         error
	applyResult      *transport.Status
	deleteErr        error
	deleteStatus     *transport.DeleteStatus
	deleteStatusErr  error
	cleanupDeleteErr error
}

func (e *errTransport) Apply(_ context.Context, _, _ string, _ [][]byte) (*transport.Status, error) {
	return e.applyResult, e.applyErr
}
func (e *errTransport) GetStatus(_ context.Context, _, _ string) (*transport.Status, error) {
	return nil, nil
}
func (e *errTransport) Delete(_ context.Context, _, _ string) error { return e.deleteErr }
func (e *errTransport) GetDeleteStatus(_ context.Context, _, _ string) (*transport.DeleteStatus, error) {
	if e.deleteStatusErr != nil {
		return nil, e.deleteStatusErr
	}
	if e.deleteStatus != nil {
		return e.deleteStatus, nil
	}
	// Return ApplyDesiresCount=1 to trigger Delete() call.
	return &transport.DeleteStatus{AllSuccessful: false, TotalCount: 0, PendingCount: 0, ApplyDesiresCount: 1}, nil
}
func (e *errTransport) CleanupDeleteDesires(_ context.Context, _, _ string) error {
	return e.cleanupDeleteErr
}

// npReq returns a reconcile.Request for the given clusterID/nodepoolID pair.
func npReq(clusterID, nodepoolID string) reconcile.Request {
	return reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: clusterID, Name: nodepoolID},
	}
}

// mustNodePoolGroupKey returns the group key for a nodepool, panicking on error.
func mustNodePoolGroupKey(namespace, clusterName, nodePoolName string) string {
	key, err := transport.NodePoolGroupKey(namespace, clusterName, nodePoolName)
	if err != nil {
		panic(err)
	}
	return key
}

// conflictErr returns a Kubernetes conflict error.
func conflictErr() error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "nodepools"}, "test", fmt.Errorf("conflict"))
}

// testNodePool creates a NodePool. If vrVersion is non-empty, the spec and VR status are set.
func testNodePool(vrVersion string) *privatev1.NodePool {
	np := &privatev1.NodePool{}
	np.SetName("np-test")
	np.SetNamespace("cluster-test")
	np.SetFinalizers([]string{constants.FinalizerNodePool})
	np.Spec = privatev1.NodePoolSpec{
		ClusterID: "cluster-test",
		Platform: privatev1.NodePoolPlatformSpec{
			Type: "GCP",
			GCP: &privatev1.GCPNodePoolPlatform{
				MachineType: "n2-standard-4",
				DiskSizeGB:  100,
				DiskType:    "pd-ssd",
				Zone:        "us-central1-b",
			},
		},
	}
	if vrVersion != "" {
		np.Spec.Release = privatev1.NodePoolReleaseSpec{Version: vrVersion}
		np.Status.VersionResolution = &privatev1.VersionResolutionResult{
			ReleaseImage:   "quay.io/openshift-release-dev/ocp-release:4.16.0-x86_64",
			ReleaseVersion: vrVersion,
		}
	}
	return np
}

// testCluster creates a Cluster with placement and optionally the HC Available condition.
func testCluster(placementReady, hcAvailable bool) *privatev1.Cluster {
	c := &privatev1.Cluster{}
	c.SetName("cluster-test")
	c.SetNamespace("hyperfleet")
	c.SetUID(types.UID("550e8400-e29b-41d4-a716-446655440000"))
	c.Spec = privatev1.ClusterSpec{
		SafeName: privatev1.DefaultSafeName("cluster-test", c.UID),
		Platform: privatev1.ClusterPlatformSpec{
			Type: "GCP",
			GCP: &privatev1.GCPClusterPlatform{
				Subnet: "my-subnet",
				Region: "us-central1",
			},
		},
	}
	if placementReady {
		c.Status.PlacementResult = &privatev1.PlacementResult{
			ManagementClusterName: "mc-us-c1",
			BaseDomain:            "hc.example.com",
		}
	}
	if hcAvailable {
		c.Status.Conditions = []metav1.Condition{
			{Type: "HostedClusterAvailable", Status: metav1.ConditionTrue, Reason: "Available"},
		}
	}
	return c
}

func nodePoolResourceKey(cluster *privatev1.Cluster, np *privatev1.NodePool) string {
	return fmt.Sprintf("hypershift.openshift.io/v1beta1/nodepools/clusters-%s/%s", cluster.UID, np.Name)
}

// buildReconciler wires up a nodepool Reconciler with injectable errors.
func buildReconciler(
	t *testing.T,
	np *privatev1.NodePool,
	cluster *privatev1.Cluster,
	tr transport.Client,
	npGetErr, clsGetErr, statusErr error,
) (*Reconciler, *mockStoreClient) {
	t.Helper()
	storeClient := &mockStoreClient{
		nodepool:     np,
		cluster:      cluster,
		npGetErr:     npGetErr,
		clsGetErr:    clsGetErr,
		statusWriter: &mockStatusWriter{updateErr: statusErr},
	}
	return New(tr, newTestLogger(t), storeClient), storeClient
}

// ---------------------------------------------------------------------------
// Test cases – early exits: NodePool get
// ---------------------------------------------------------------------------

func TestReconcile_NodePoolNotFound(t *testing.T) {
	tr := mock.New()
	r, _ := buildReconciler(t, nil, nil, tr, nil, nil, nil) // nil nodepool → NotFound

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-missing"))
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), result.RequeueAfter)
	require.Empty(t, tr.ApplyCalls)
}

func TestReconcile_NodePoolGetError(t *testing.T) {
	tr := mock.New()
	r, _ := buildReconciler(t, nil, nil, tr, fmt.Errorf("etcd timeout"), nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "get nodepool")
}

// ---------------------------------------------------------------------------
// Test cases – early exits: Cluster get
// ---------------------------------------------------------------------------

func TestReconcile_ClusterNotFound(t *testing.T) {
	np := testNodePool("4.16.0")

	tr := mock.New()
	r, _ := buildReconciler(t, np, nil, tr, nil, nil, nil) // nil cluster → NotFound

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Empty(t, tr.ApplyCalls)
}

func TestReconcile_ClusterGetError(t *testing.T) {
	np := testNodePool("4.16.0")

	tr := mock.New()
	r, _ := buildReconciler(t, np, nil, tr, nil, fmt.Errorf("etcd timeout"), nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "get cluster")
}

// ---------------------------------------------------------------------------
// Test cases – early exits: placement gate
// ---------------------------------------------------------------------------

func TestReconcile_NoPlacement(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(false, true) // placement not ready

	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter, "should requeue while placement is not ready")
	require.Empty(t, tr.ApplyCalls)
}

func TestReconcile_NoPlacement_StatusUpdateConflict(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(false, true)

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, conflictErr())

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter)
	require.True(t, storeClient.statusWriter.called)
}

func TestReconcile_NoPlacement_StatusUpdateError(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(false, true)

	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, fmt.Errorf("server error"))

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "update nodepool status")
}

// ---------------------------------------------------------------------------
// Test cases – early exits: VR gates
// ---------------------------------------------------------------------------

func TestReconcile_NodePoolVRNotReady(t *testing.T) {
	np := testNodePool("") // no VR
	cluster := testCluster(true, true)

	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter, "should requeue while VR is not ready")
	require.Empty(t, tr.ApplyCalls)
}

func TestReconcile_VRNotReady_StatusUpdateConflict(t *testing.T) {
	np := testNodePool("")
	cluster := testCluster(true, true)

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, conflictErr())

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter)
	require.True(t, storeClient.statusWriter.called)
}

func TestReconcile_VRNotReady_StatusUpdateError(t *testing.T) {
	np := testNodePool("")
	cluster := testCluster(true, true)

	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, fmt.Errorf("server error"))

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "update nodepool status")
}

func TestReconcile_VRVersionMismatch(t *testing.T) {
	np := testNodePool("4.15.0")       // VR resolved to 4.15.0
	np.Spec.Release.Version = "4.16.0" // but spec wants 4.16.0
	cluster := testCluster(true, true)

	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter, "should requeue while VR version mismatches")
	require.Empty(t, tr.ApplyCalls)
}

func TestReconcile_VRVersionMismatch_StatusUpdateConflict(t *testing.T) {
	np := testNodePool("4.15.0")
	np.Spec.Release.Version = "4.16.0"
	cluster := testCluster(true, true)

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, conflictErr())

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter)
	require.True(t, storeClient.statusWriter.called)
}

func TestReconcile_VRVersionMismatch_StatusUpdateError(t *testing.T) {
	np := testNodePool("4.15.0")
	np.Spec.Release.Version = "4.16.0"
	cluster := testCluster(true, true)

	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, fmt.Errorf("server error"))

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "update nodepool status")
}

// ---------------------------------------------------------------------------
// Test cases – platform field defaults
// ---------------------------------------------------------------------------

// replicasFromManifests parses the replica count from the raw JSON in the first manifest.
func replicasFromManifests(t *testing.T, manifests [][]byte) int32 {
	t.Helper()
	require.NotEmpty(t, manifests)
	var obj struct {
		Spec struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(manifests[0], &obj))
	return obj.Spec.Replicas
}

// TestReconcile_NodeCountHonored verifies that when Spec.NodeCount is set, it is used
// as the replica count instead of the default.
func TestReconcile_NodeCountHonored(t *testing.T) {
	count := int32(3)
	np := testNodePool("4.16.0")
	np.Spec.NodeCount = &count
	cluster := testCluster(true, true)

	npKey := nodePoolResourceKey(cluster, np)
	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {"readyCondition": "True", "allNodesHealthyCondition": "True"},
		},
	}

	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1)
	require.Equal(t, int32(3), replicasFromManifests(t, tr.ApplyCalls[0].Manifests))
}

// TestReconcile_UsesClusterUIDForNodePoolIdentity verifies that the NodePool is
// applied into the UID-backed HostedCluster namespace while spec.clusterName
// references the parent Cluster object's private safeName.
func TestReconcile_UsesClusterUIDForNodePoolIdentity(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	clusterUID := "550e8400-e29b-41d4-a716-446655440000"
	cluster.SetUID(types.UID(clusterUID))
	cluster.Spec.SafeName = privatev1.DefaultSafeName(cluster.Name, cluster.UID)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			nodePoolResourceKey(cluster, np): {"readyCondition": "True", "allNodesHealthyCondition": "True", "allMachinesReadyCondition": "True"},
		},
	}

	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1)

	var obj map[string]any
	require.NoError(t, json.Unmarshal(tr.ApplyCalls[0].Manifests[0], &obj))
	metadata := obj["metadata"].(map[string]any)
	require.Equal(t, np.Name, metadata["name"])
	require.Equal(t, "clusters-"+clusterUID, metadata["namespace"])

	labels := metadata["labels"].(map[string]any)
	require.Equal(t, clusterUID, labels["gcp.managed.openshift.io/cluster-id"])

	spec := obj["spec"].(map[string]any)
	require.Equal(t, cluster.Spec.SafeName, spec["clusterName"])
}

// TestReconcile_DefaultNodeCount verifies that when Spec.NodeCount is nil, defaultReplicas is used.
func TestReconcile_DefaultNodeCount(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Spec.NodeCount = nil
	cluster := testCluster(true, true)

	npKey := nodePoolResourceKey(cluster, np)
	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {"readyCondition": "True", "allNodesHealthyCondition": "True"},
		},
	}

	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1)
	require.Equal(t, int32(1), replicasFromManifests(t, tr.ApplyCalls[0].Manifests))
}

// TestReconcile_DefaultPlatformValues verifies that when the NodePool has no GCP platform
// spec set, the reconciler applies default machine type, disk size, disk type, and derives
// the zone from the cluster's GCP region.
func TestReconcile_DefaultPlatformValues(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Spec.Platform.GCP = nil // no GCP spec → all defaults apply

	cluster := testCluster(true, true) // cluster has GCP.Region = "us-central1"

	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {"readyCondition": "True", "allNodesHealthyCondition": "True"},
		},
	}

	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeueStable, result.RequeueAfter)
	require.Len(t, tr.ApplyCalls, 1)
	// Zone should be derived from region: "us-central1" → "us-central1-a"
	// MachineType/DiskSizeGB/DiskType should be defaults (validated inside manifest.Build).
}

// TestReconcile_ZoneDerivedFromRegion verifies that when the NodePool GCP spec exists
// but zone is empty, the zone is derived from the cluster's region.
func TestReconcile_ZoneDerivedFromRegion(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Spec.Platform.GCP.Zone = "" // explicit empty zone → derived from cluster region

	cluster := testCluster(true, true) // cluster region = "us-central1"

	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {"readyCondition": "True", "allNodesHealthyCondition": "True"},
		},
	}

	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeueStable, result.RequeueAfter)
	require.Len(t, tr.ApplyCalls, 1)
}

// ---------------------------------------------------------------------------
// Test cases – transport errors
// ---------------------------------------------------------------------------

func TestReconcile_TransportApplyError(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)

	tr := &errTransport{applyErr: fmt.Errorf("transport unavailable")}
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "apply resources")
}

// TestReconcile_MWStatusNil_RequeuesPending verifies that when Apply returns nil status
// (resources not yet processed), both conditions are set to False and the reconciler
// requeues with the pending interval.
func TestReconcile_MWStatusNil_RequeuesPending(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)

	// Apply returns nil status to simulate the resources not yet having a status.
	tr := &errTransport{}
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter)
	require.True(t, storeClient.statusWriter.called)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	applied := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolResourcesApplied")
	require.NotNil(t, applied)
	require.Equal(t, metav1.ConditionFalse, applied.Status)
	require.Equal(t, "ResourcesNotFound", applied.Reason)
	require.Equal(t, "Resources have not been applied yet", applied.Message)
	available := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolAvailable")
	require.NotNil(t, available)
	require.Equal(t, metav1.ConditionFalse, available.Status)
	require.Equal(t, "ResourcesNotFound", available.Reason)
	require.Equal(t, "Resources have not been applied yet", available.Message)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionFalse, progressing.Status)
	require.Equal(t, "AsExpected", progressing.Reason)
	require.Equal(t, "Resources have not been applied yet", progressing.Message)
}

func TestApplyStatusConditions_ObservedVersionChangeIsPersisted(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)
	status := &transport.Status{
		Conditions: []metav1.Condition{{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"}},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
				"updatingConfigCondition":   "False",
				"updatingVersionCondition":  "False",
				"version":                   "4.16.0",
			},
		},
	}
	r := &Reconciler{}
	require.True(t, r.applyStatusConditions(np, string(cluster.UID), status))
	require.False(t, r.applyStatusConditions(np, string(cluster.UID), status))
	status.ResourceStatuses[npKey]["version"] = "4.16.1"
	require.True(t, r.applyStatusConditions(np, string(cluster.UID), status))
	require.Equal(t, "4.16.1", np.Status.Version)
}

func TestSetWaitingNPConditionsPreservesUpgradeStatus(t *testing.T) {
	np := testNodePool("4.16.1")
	np.Status.Version = "4.16.0"
	upgrade := metav1.Condition{Type: "NodePoolUpgradeProgressing", Status: metav1.ConditionTrue, Reason: "UpgradeAccepted", LastTransitionTime: metav1.Now()}
	np.Status.Conditions = append(np.Status.Conditions, upgrade)
	require.True(t, setWaitingNPConditions(np, "VersionResolutionNotReady", "Waiting for nodepool version resolution"))
	require.Equal(t, &upgrade, meta.FindStatusCondition(np.Status.Conditions, "NodePoolUpgradeProgressing"))
	require.True(t, meta.IsStatusConditionTrue(np.Status.Conditions, privatev1.NodePoolUpgradeProgressing))
}

// ---------------------------------------------------------------------------
// Test cases – happy path and condition-driven requeue
// ---------------------------------------------------------------------------

func TestReconcile_HappyPath(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)

	tr := mock.New()
	npGroupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	npKey := nodePoolResourceKey(cluster, np)
	tr.StatusOverrides["mc-us-c1/"+npGroupKey] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
				"replicas":                  "2",
				"version":                   "4.16.0",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeueStable, result.RequeueAfter)

	require.Len(t, tr.ApplyCalls, 1)
	require.Equal(t, "mc-us-c1", tr.ApplyCalls[0].TargetCluster)
	require.Equal(t, npGroupKey, tr.ApplyCalls[0].GroupKey)
	require.True(t, storeClient.statusWriter.called, "expected Status().Update to be called")

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	available := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolAvailable")
	require.NotNil(t, available)
	require.Equal(t, metav1.ConditionTrue, available.Status)
	require.Equal(t, "NodePoolAvailable", available.Reason)
	healthy := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolHealthy")
	require.NotNil(t, healthy)
	require.Equal(t, metav1.ConditionTrue, healthy.Status)
	require.Equal(t, "NodePoolHealthy", healthy.Reason)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionFalse, progressing.Status)
	require.Equal(t, "AsExpected", progressing.Reason)
	require.Equal(t, "4.16.0", captured.Status.Version)
}

// TestReconcile_MWNotApplied_RequeuesPending verifies that when Applied=False the
// reconciler requeues with the pending interval.
func TestReconcile_MWNotApplied_RequeuesPending(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)

	npKey := nodePoolResourceKey(cluster, np)
	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionFalse, Reason: "ApplyFailed"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "False",
				"allMachinesReadyCondition": "False",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	available := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolAvailable")
	require.NotNil(t, available)
	require.Equal(t, metav1.ConditionFalse, available.Status)
	require.Equal(t, "NodePoolNotAvailable", available.Reason)
	healthy := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolHealthy")
	require.NotNil(t, healthy)
	require.Equal(t, metav1.ConditionFalse, healthy.Status)
	require.Equal(t, "NodePoolNotHealthy", healthy.Reason)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "MachinesNotReady", progressing.Reason)
}

// TestReconcile_ResourcesApplied_ClusterNotAvailable_RequeuesPending verifies that when
// nodepool resources are applied but the parent cluster's HostedClusterAvailable is False,
// the reconciler keeps polling at the pending interval.
func TestReconcile_ResourcesApplied_ClusterNotAvailable_RequeuesPending(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, false) // placement ready, HC NOT available

	npKey := nodePoolResourceKey(cluster, np)
	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {"readyCondition": "True", "allNodesHealthyCondition": "True"},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter, "should requeue at pending interval while cluster is not available")
	require.True(t, storeClient.statusWriter.called)
}

// TestReconcile_ResourcesApplied_NodePoolNotAvailable_RequeuesPending verifies that when
// nodepool resources are applied and the cluster is available, but NodePoolAvailable is False,
// the reconciler keeps polling at the pending interval.
func TestReconcile_ResourcesApplied_NodePoolNotAvailable_RequeuesPending(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true) // placement ready, HC available

	npKey := nodePoolResourceKey(cluster, np)
	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":           "False", // NodePool not available
				"allNodesHealthyCondition": "False",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter, "should requeue at pending interval while nodepool is not available")
	require.True(t, storeClient.statusWriter.called)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	applied := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolResourcesApplied")
	require.NotNil(t, applied)
	require.Equal(t, metav1.ConditionTrue, applied.Status)
	available := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolAvailable")
	require.NotNil(t, available)
	require.Equal(t, metav1.ConditionFalse, available.Status)
}

// TestReconcile_StatusUpdateConflict_ReturnsNoError verifies that a conflict error on
// Status.Update after applyStatusConditions is silently swallowed.
func TestReconcile_StatusUpdateConflict_ReturnsNoError(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)

	npKey := nodePoolResourceKey(cluster, np)
	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {"readyCondition": "True", "allNodesHealthyCondition": "True"},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, conflictErr())

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter) // conflict → immediate return
	require.True(t, storeClient.statusWriter.called)
}

// TestReconcile_StatusUpdateError_ReturnsError verifies that a non-conflict error on
// Status.Update after applyStatusConditions is propagated.
func TestReconcile_StatusUpdateError_ReturnsError(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, fmt.Errorf("server error"))

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "update nodepool status")
	require.True(t, storeClient.statusWriter.called)
}

// TestReconcile_StaleStatus_RequeuesPending verifies that when the transport
// reports stale status, the reconciler requeues with the pending interval even
// though resources are applied and conditions are True.
func TestReconcile_StaleStatus_RequeuesPending(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)

	npKey := nodePoolResourceKey(cluster, np)
	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":           "True",
				"allNodesHealthyCondition": "True",
			},
		},
		Stale: true,
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, requeuePending, result.RequeueAfter)

	// Verify that stale status was not written to the nodepool conditions.
	require.Nil(t, storeClient.statusWriter.captured, "status should not be updated when status is stale")
}

// ---------------------------------------------------------------------------
// Test cases – finalizer management
// ---------------------------------------------------------------------------

// TestReconcile_AddsFinalizer verifies that a nodepool without the finalizer gets it added
// and the reconciler returns immediately to re-reconcile.
func TestReconcile_AddsFinalizer(t *testing.T) {
	np := testNodePool("4.16.0")
	np.SetFinalizers(nil) // no finalizer yet
	cluster := testCluster(true, true)

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.True(t, storeClient.updateCalled, "expected Update to be called to add finalizer")
	require.Empty(t, tr.ApplyCalls, "should not Apply before finalizer is persisted")

	updated := storeClient.updated.(*privatev1.NodePool)
	require.Contains(t, updated.Finalizers, constants.FinalizerNodePool)
}

// TestReconcile_AddFinalizer_UpdateError verifies that an Update error when adding the
// finalizer is propagated.
func TestReconcile_AddFinalizer_UpdateError(t *testing.T) {
	np := testNodePool("4.16.0")
	np.SetFinalizers(nil)
	cluster := testCluster(true, true)

	tr := mock.New()
	storeClient := &mockStoreClient{
		nodepool:     np,
		cluster:      cluster,
		statusWriter: &mockStatusWriter{},
		updateErr:    fmt.Errorf("etcd write error"),
	}
	r := New(tr, newTestLogger(t), storeClient)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "add finalizer")
}

// ---------------------------------------------------------------------------
// Test cases – deletion
// ---------------------------------------------------------------------------

// TestReconcile_Deletion_HappyPath verifies the async deletion flow:
// 1. First reconcile: calls transport.Delete, requeues
// 2. Second reconcile: polls GetDeleteStatus (AllSuccessful=true), cleans up, removes finalizer
func TestReconcile_Deletion_HappyPath(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	// Simulate ApplyDesires exist (deletion not started).
	npGroupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	key := "mc-us-c1/" + npGroupKey
	tr.DeleteStatusOverrides[key] = &transport.DeleteStatus{
		AllSuccessful:     false,
		TotalCount:        0,
		PendingCount:      0,
		ApplyDesiresCount: 2,
	}

	// First reconcile: initiate deletion.
	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter, "should requeue after initiating delete")
	require.Len(t, tr.DeleteCalls, 1)
	require.Equal(t, "mc-us-c1", tr.DeleteCalls[0].TargetCluster)
	require.Equal(t, npGroupKey, tr.DeleteCalls[0].GroupKey)
	require.False(t, storeClient.updateCalled, "finalizer not removed yet")

	// Simulate kube-applier-gcp completing deletion.
	tr.DeleteStatusOverrides[key] = &transport.DeleteStatus{AllSuccessful: true, TotalCount: 2, PendingCount: 0, ApplyDesiresCount: 0}

	// Second reconcile: check status, cleanup, remove finalizer.
	storeClient.updateCalled = false
	result, err = r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Len(t, tr.CleanupDeleteDesiresCalls, 1, "should cleanup DeleteDesires")
	require.Equal(t, "mc-us-c1", tr.CleanupDeleteDesiresCalls[0].TargetCluster)
	require.Equal(t, npGroupKey, tr.CleanupDeleteDesiresCalls[0].GroupKey)

	require.True(t, storeClient.updateCalled, "expected Update to remove finalizer")
	updated := storeClient.updated.(*privatev1.NodePool)
	require.NotContains(t, updated.Finalizers, constants.FinalizerNodePool)
}

func TestReconcile_Deletion_Pending(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	groupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	tr.DeleteStatusOverrides["mc-us-c1/"+groupKey] = &transport.DeleteStatus{
		AllSuccessful: false,
		TotalCount:    1,
		PendingCount:  1,
	}
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)
	require.Empty(t, tr.DeleteCalls)
	require.Empty(t, tr.CleanupDeleteDesiresCalls)
	require.False(t, storeClient.updateCalled)
}

// TestReconcile_Deletion_RecordsProgressOnDeleteInitiation verifies that the
// reconcile which initiates transport deletion records a DeletionProgressing
// condition with the DeletingResources reason.
func TestReconcile_Deletion_RecordsProgressOnDeleteInitiation(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	groupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	tr.DeleteStatusOverrides["mc-us-c1/"+groupKey] = &transport.DeleteStatus{
		AllSuccessful:     false,
		TotalCount:        0,
		PendingCount:      0,
		ApplyDesiresCount: 7,
	}
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)
	require.Len(t, tr.DeleteCalls, 1)

	require.True(t, storeClient.statusWriter.called, "deletion progress condition should be recorded")
	cond := meta.FindStatusCondition(
		storeClient.statusWriter.captured.(*privatev1.NodePool).Status.Conditions, "DeletionProgressing")
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "DeletingResources", cond.Reason)
	require.Equal(t, "Deletion of 7 resources initiated", cond.Message)
}

// TestReconcile_Deletion_RecordsPendingProgress verifies that the deletion
// wait records pending resource references in the condition message.
func TestReconcile_Deletion_RecordsPendingProgress(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	groupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	tr.DeleteStatusOverrides["mc-us-c1/"+groupKey] = &transport.DeleteStatus{
		AllSuccessful: false,
		TotalCount:    3,
		PendingCount:  2,
		PendingResources: []string{
			"hypershift.openshift.io/v1beta1/hostedclusters/clusters-abc/hc-1",
			"hypershift.openshift.io/v1beta1/hostedclusters/clusters-abc/hc-2",
		},
	}
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)

	cond := meta.FindStatusCondition(
		storeClient.statusWriter.captured.(*privatev1.NodePool).Status.Conditions, "DeletionProgressing")
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "DeletionInProgress", cond.Reason)
	require.Equal(t, "2 of 3 resources still being deleted: "+
		"hypershift.openshift.io/v1beta1/hostedclusters/clusters-abc/hc-1, "+
		"hypershift.openshift.io/v1beta1/hostedclusters/clusters-abc/hc-2", cond.Message)
}

// TestReconcile_Deletion_RecordsBlockedProgress verifies that explicit
// kube-applier-gcp failures switch the condition to DeletionBlocked and carry
// the failure details.
func TestReconcile_Deletion_RecordsBlockedProgress(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	groupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	tr.DeleteStatusOverrides["mc-us-c1/"+groupKey] = &transport.DeleteStatus{
		AllSuccessful:    false,
		TotalCount:       2,
		PendingCount:     1,
		PendingResources: []string{"g/v1/rs/ns/stuck"},
		FailedCount:      1,
		Failures:         []transport.DeleteFailure{{Resource: "g/v1/rs/ns/stuck", Reason: "KubeAPIError", Message: "boom"}},
	}
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)

	cond := meta.FindStatusCondition(
		storeClient.statusWriter.captured.(*privatev1.NodePool).Status.Conditions, "DeletionProgressing")
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "DeletionBlocked", cond.Reason)
	require.Equal(t, "1 of 2 resources still being deleted: g/v1/rs/ns/stuck; "+
		"deletion failed for 1 resources: g/v1/rs/ns/stuck (KubeAPIError: boom)", cond.Message)
}

// TestReconcile_Deletion_ProgressIsIdempotent verifies that a reconcile which
// observes unchanged deletion progress does not write the status again.
func TestReconcile_Deletion_ProgressIsIdempotent(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	groupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	tr.DeleteStatusOverrides["mc-us-c1/"+groupKey] = &transport.DeleteStatus{
		AllSuccessful:    false,
		TotalCount:       3,
		PendingCount:     2,
		PendingResources: []string{"g/v1/rs/ns/a", "g/v1/rs/ns/b"},
	}
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, 1, storeClient.statusWriter.updates)

	// Simulate the status write having been persisted.
	np.Status.Conditions = storeClient.statusWriter.captured.(*privatev1.NodePool).Status.Conditions
	storeClient.statusWriter.updates = 0

	_, err = r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, storeClient.statusWriter.updates, "unchanged deletion progress must not rewrite the status")
}

func TestReconcile_Deletion_GetStatusError(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)

	r, storeClient := buildReconciler(t, np, testCluster(true, true), &errTransport{
		deleteStatusErr: fmt.Errorf("firestore unavailable"),
	}, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "get delete status")
	require.False(t, storeClient.updateCalled)
}

func TestReconcile_Deletion_CleanupError(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)

	r, storeClient := buildReconciler(t, np, testCluster(true, true), &errTransport{
		deleteStatus:     &transport.DeleteStatus{AllSuccessful: true, TotalCount: 1},
		cleanupDeleteErr: fmt.Errorf("firestore unavailable"),
	}, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "cleanup delete desires")
	require.False(t, storeClient.updateCalled)
}

// TestReconcile_Deletion_ClusterNotFound verifies that when the parent cluster is gone,
// transport.Delete is NOT called but the finalizer is still removed.
func TestReconcile_Deletion_ClusterNotFound(t *testing.T) {
	np := testNodePool("4.16.0")
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, nil, tr, nil, nil, nil) // nil cluster → NotFound

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)

	require.Empty(t, tr.DeleteCalls, "should not call Delete when cluster not found")
	require.True(t, storeClient.updateCalled, "expected Update to remove finalizer")
	require.False(t, storeClient.statusWriter.called, "no DeletionProgressing condition when the cluster is gone")
	updated := storeClient.updated.(*privatev1.NodePool)
	require.NotContains(t, updated.Finalizers, constants.FinalizerNodePool)
}

// TestReconcile_Deletion_NoPlacement verifies that when the parent cluster has no placement,
// transport.Delete is NOT called but the finalizer is still removed.
func TestReconcile_Deletion_NoPlacement(t *testing.T) {
	np := testNodePool("4.16.0")
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(false, false) // no placement

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)

	require.Empty(t, tr.DeleteCalls, "should not call Delete without placement")
	require.True(t, storeClient.updateCalled, "expected Update to remove finalizer")
	updated := storeClient.updated.(*privatev1.NodePool)
	require.NotContains(t, updated.Finalizers, constants.FinalizerNodePool)
}

// TestReconcile_Deletion_TransportError verifies that a transport.Delete error is
// propagated and the finalizer is NOT removed (controller will retry).
func TestReconcile_Deletion_TransportError(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := &errTransport{deleteErr: fmt.Errorf("firestore unavailable")}
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "delete resources")
	require.False(t, storeClient.updateCalled, "finalizer should not be removed on error")
}

// TestReconcile_Deletion_NoFinalizer verifies that when the nodepool has a DeletionTimestamp
// but no finalizer, the reconciler returns immediately (nothing to do).
func TestReconcile_Deletion_NoFinalizer(t *testing.T) {
	np := testNodePool("4.16.0")
	np.SetFinalizers(nil)
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)

	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Empty(t, tr.DeleteCalls)
	require.False(t, storeClient.updateCalled)
}

// TestReconcile_Deletion_RemoveFinalizerError verifies that an error removing the
// finalizer after successful deletion/cleanup is propagated.
func TestReconcile_Deletion_RemoveFinalizerError(t *testing.T) {
	np := testNodePool("4.16.0")
	np.Status.Conditions = append(np.Status.Conditions, metav1.Condition{
		Type: "NodePoolResourcesApplied", Status: metav1.ConditionTrue, Reason: "Applied",
	})
	now := metav1.Now()
	np.SetDeletionTimestamp(&now)
	cluster := testCluster(true, true)

	tr := mock.New()
	storeClient := &mockStoreClient{
		nodepool:     np,
		cluster:      cluster,
		statusWriter: &mockStatusWriter{},
		updateErr:    fmt.Errorf("etcd write error"),
	}
	r := New(tr, newTestLogger(t), storeClient)

	// Simulate ApplyDesires exist (deletion not started).
	npGroupKey := mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")
	key := "mc-us-c1/" + npGroupKey
	tr.DeleteStatusOverrides[key] = &transport.DeleteStatus{
		AllSuccessful:     false,
		TotalCount:        0,
		PendingCount:      0,
		ApplyDesiresCount: 2,
	}

	// First reconcile: initiate deletion, requeues.
	result, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)
	require.Len(t, tr.DeleteCalls, 1)

	// Simulate completion.
	tr.DeleteStatusOverrides[key] = &transport.DeleteStatus{AllSuccessful: true, TotalCount: 2, PendingCount: 0, ApplyDesiresCount: 0}

	// Second reconcile: cleanup succeeds, but finalizer removal fails.
	_, err = r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "remove finalizer")
	require.Len(t, tr.CleanupDeleteDesiresCalls, 1, "cleanup should have been called before finalizer removal failed")
	require.True(t, storeClient.updateCalled)
}

// ---------------------------------------------------------------------------
// Test cases – NodePoolProgressing condition
// ---------------------------------------------------------------------------

func TestReconcile_Progressing_AllMachinesReadyFalse(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "False",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "MachinesNotReady", progressing.Reason)
}

func TestReconcile_Progressing_AllMachinesReadyAbsent(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":           "True",
				"allNodesHealthyCondition": "True",
				// allMachinesReadyCondition intentionally absent
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "MachinesNotReady", progressing.Reason)
}

func TestReconcile_Progressing_UpdatingConfig(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
				"updatingConfigCondition":   "True",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "UpdatingConfig", progressing.Reason)
}

func TestReconcile_Progressing_Priority_UpdatingConfigWins(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
				"updatingConfigCondition":   "True",
				"updatingVersionCondition":  "True",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "UpdatingConfig", progressing.Reason)
}

func TestReconcile_Progressing_UpdatingVersion(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
				"updatingVersionCondition":  "True",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "UpdatingVersion", progressing.Reason)
}

func TestReconcile_Progressing_AsExpected(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
				"updatingConfigCondition":   "False",
				"updatingVersionCondition":  "False",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionFalse, progressing.Status)
	require.Equal(t, "AsExpected", progressing.Reason)
}

func TestReconcile_Progressing_Priority_MachinesNotReadyWins(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "False",
				"updatingConfigCondition":   "True",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "MachinesNotReady", progressing.Reason)
}

// ---------------------------------------------------------------------------
// Test cases – HyperShift condition message propagation (GCP-1516 issues 4 & 5)
// ---------------------------------------------------------------------------

// TestReconcile_ConditionMessages_UnhealthyReady verifies the HyperShift Ready
// condition message is surfaced verbatim on NodePoolAvailable when not available.
func TestReconcile_ConditionMessages_UnhealthyReady(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "False",
				"readyConditionMessage":     "GCP actuator is failing: permission denied on service account",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	available := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolAvailable")
	require.NotNil(t, available)
	require.Equal(t, metav1.ConditionFalse, available.Status)
	require.Equal(t, "NodePoolNotAvailable", available.Reason)
	require.Equal(t, "GCP actuator is failing: permission denied on service account", available.Message)
}

// TestReconcile_ConditionMessages_UnhealthyNodes verifies the AllNodesHealthy
// condition message is surfaced verbatim on NodePoolHealthy when unhealthy.
func TestReconcile_ConditionMessages_UnhealthyNodes(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":                  "True",
				"allNodesHealthyCondition":        "False",
				"allNodesHealthyConditionMessage": "Node pool is reporting 1 unhealthy nodes",
				"allMachinesReadyCondition":       "True",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	healthy := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolHealthy")
	require.NotNil(t, healthy)
	require.Equal(t, metav1.ConditionFalse, healthy.Status)
	require.Equal(t, "NodePoolNotHealthy", healthy.Reason)
	require.Equal(t, "Node pool is reporting 1 unhealthy nodes", healthy.Message)
}

// TestReconcile_ConditionMessages_MachinesNotReady verifies the AllMachinesReady
// condition message is surfaced verbatim on NodePoolProgressing. This is the
// customer-project IAM failure shape from issue 5: the specific error only
// exists in the HyperShift condition message.
func TestReconcile_ConditionMessages_MachinesNotReady(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":                   "True",
				"allNodesHealthyCondition":         "True",
				"allMachinesReadyCondition":        "False",
				"allMachinesReadyConditionMessage": "Failed to create machine: googleapi: Error 403: missing iam.serviceAccount.actAs permission",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "MachinesNotReady", progressing.Reason)
	require.Equal(t, "Failed to create machine: googleapi: Error 403: missing iam.serviceAccount.actAs permission", progressing.Message)
}

// TestReconcile_ConditionMessages_UpdatingConfig verifies the UpdatingConfig
// condition message is surfaced verbatim on NodePoolProgressing.
func TestReconcile_ConditionMessages_UpdatingConfig(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":                 "True",
				"allNodesHealthyCondition":       "True",
				"allMachinesReadyCondition":      "True",
				"updatingConfigCondition":        "True",
				"updatingConfigConditionMessage": "Configuration update in progress",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "UpdatingConfig", progressing.Reason)
	require.Equal(t, "Configuration update in progress", progressing.Message)
}

// TestReconcile_ConditionMessages_Priority_MachinesNotReadyWins verifies the
// progressing message follows the same precedence as the reason: machines not
// ready wins over updating config.
func TestReconcile_ConditionMessages_Priority_MachinesNotReadyWins(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":                   "True",
				"allNodesHealthyCondition":         "True",
				"allMachinesReadyCondition":        "False",
				"allMachinesReadyConditionMessage": "machines not ready message",
				"updatingConfigCondition":          "True",
				"updatingConfigConditionMessage":   "updating config message",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	progressing := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, "MachinesNotReady", progressing.Reason)
	require.Equal(t, "machines not ready message", progressing.Message)
}

// TestReconcile_ConditionMessages_AppliedMessage verifies the ManifestWork
// Applied condition message is carried on NodePoolResourcesApplied.
func TestReconcile_ConditionMessages_AppliedMessage(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully", Message: "Resource is applied"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "True",
				"allNodesHealthyCondition":  "True",
				"allMachinesReadyCondition": "True",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	applied := meta.FindStatusCondition(captured.Status.Conditions, "NodePoolResourcesApplied")
	require.NotNil(t, applied)
	require.Equal(t, metav1.ConditionTrue, applied.Status)
	require.Equal(t, "AppliedSuccessfully", applied.Reason)
	require.Equal(t, "Resource is applied", applied.Message)
}

// TestReconcile_ConditionMessages_HealthyStaysMessageless verifies messages
// are dropped from healthy conditions, mirroring the HC controller.
func TestReconcile_ConditionMessages_HealthyStaysMessageless(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":                   "True",
				"readyConditionMessage":            "stale informational message",
				"allNodesHealthyCondition":         "True",
				"allNodesHealthyConditionMessage":  "stale informational message",
				"allMachinesReadyCondition":        "True",
				"allMachinesReadyConditionMessage": "stale informational message",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	for _, condType := range []string{"NodePoolAvailable", "NodePoolHealthy", "NodePoolProgressing"} {
		cond := meta.FindStatusCondition(captured.Status.Conditions, condType)
		require.NotNil(t, cond, condType)
		require.Empty(t, cond.Message, "%s must stay message-less when healthy", condType)
	}
}

// TestReconcile_ConditionMessages_AbsentMessageKeys verifies feedback extracted
// before message propagation (no *Message keys) leaves conditions message-less.
func TestReconcile_ConditionMessages_AbsentMessageKeys(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)

	tr := mock.New()
	tr.StatusOverrides["mc-us-c1/"+mustNodePoolGroupKey("cluster-test", "cluster-test", "np-test")] = &transport.Status{
		Conditions: []metav1.Condition{
			{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"},
		},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":            "False",
				"allNodesHealthyCondition":  "False",
				"allMachinesReadyCondition": "False",
			},
		},
	}

	r, storeClient := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(context.Background(), npReq("cluster-test", "np-test"))
	require.NoError(t, err)

	captured := storeClient.statusWriter.captured.(*privatev1.NodePool)
	for _, condType := range []string{"NodePoolResourcesApplied", "NodePoolAvailable", "NodePoolHealthy", "NodePoolProgressing"} {
		cond := meta.FindStatusCondition(captured.Status.Conditions, condType)
		require.NotNil(t, cond, condType)
		require.Empty(t, cond.Message, "%s must stay message-less without message feedback", condType)
	}
}

// TestApplyStatusConditions_MessagesIdempotent verifies re-applying identical
// feedback (messages included) does not report a change.
func TestApplyStatusConditions_MessagesIdempotent(t *testing.T) {
	np := testNodePool("4.16.0")
	cluster := testCluster(true, true)
	npKey := nodePoolResourceKey(cluster, np)
	status := &transport.Status{
		Conditions: []metav1.Condition{{Type: "Applied", Status: metav1.ConditionTrue, Reason: "AppliedSuccessfully"}},
		ResourceStatuses: map[string]map[string]string{
			npKey: {
				"readyCondition":                   "False",
				"readyConditionMessage":            "not available message",
				"allNodesHealthyCondition":         "False",
				"allNodesHealthyConditionMessage":  "unhealthy message",
				"allMachinesReadyCondition":        "False",
				"allMachinesReadyConditionMessage": "machines message",
				"version":                          "4.16.0",
			},
		},
	}
	r := &Reconciler{}
	require.True(t, r.applyStatusConditions(np, string(cluster.UID), status))
	require.False(t, r.applyStatusConditions(np, string(cluster.UID), status))

	available := meta.FindStatusCondition(np.Status.Conditions, "NodePoolAvailable")
	require.NotNil(t, available)
	require.Equal(t, "not available message", available.Message)
	healthy := meta.FindStatusCondition(np.Status.Conditions, "NodePoolHealthy")
	require.NotNil(t, healthy)
	require.Equal(t, "unhealthy message", healthy.Message)
	progressing := meta.FindStatusCondition(np.Status.Conditions, "NodePoolProgressing")
	require.NotNil(t, progressing)
	require.Equal(t, "machines message", progressing.Message)
}

func TestReconcile_PendingReleaseUsesObservedImageAndAllowsScaling(t *testing.T) {
	np := testNodePool("4.22.14")
	np.Status.Version = "4.22.14"
	np.Spec.Release.Version = "4.22.16"
	np.Spec.Release.UpgradeStartTime = &metav1.Time{Time: time.Now().Add(time.Hour)}
	replicas := int32(3)
	np.Spec.NodeCount = &replicas
	cluster := testCluster(true, true)
	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1)
	require.Equal(t, replicas, replicasFromManifests(t, tr.ApplyCalls[0].Manifests))
	var obj map[string]any
	require.NoError(t, json.Unmarshal(tr.ApplyCalls[0].Manifests[0], &obj))
	require.Equal(t, np.Status.VersionResolution.ReleaseImage, obj["spec"].(map[string]any)["release"].(map[string]any)["image"])
}

func TestReconcile_UnapprovedTargetImageCannotBypassSchedule(t *testing.T) {
	np := testNodePool("4.22.16")
	np.Status.Version = "4.22.14"
	np.Spec.Release.UpgradeStartTime = &metav1.Time{Time: time.Now().Add(time.Hour)}
	cluster := testCluster(true, true)
	tr := mock.New()
	r, _ := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "an already resolved image must not bypass coordinator approval")
}

// ---------------------------------------------------------------------------
// IAM dispatch protocol
// ---------------------------------------------------------------------------

var npTestPin = privatev1.WIFConfigTemplateReference{Name: "4.22.0", UID: "template-uid", Digest: "sha256:" + strings.Repeat("a", 64)}

func npDispatchVersion(available bool, reason string) *privatev1.Version {
	v := &privatev1.Version{
		ObjectMeta: metav1.ObjectMeta{Name: "4.22.16", Generation: 2},
		Spec: privatev1.VersionSpec{
			ReleaseImage:               "quay.io/openshift-release-dev/ocp-release:4.22.16-x86_64",
			ChannelGroups:              []string{"stable"},
			WIFConfigTemplateReference: &npTestPin,
		},
	}
	status, message := metav1.ConditionFalse, reason
	if available {
		status, reason, message = metav1.ConditionTrue, "Available", "Available"
	}
	v.Status.Conditions = []metav1.Condition{{Type: privatev1.VersionAvailable, Status: status, Reason: reason, Message: message, ObservedGeneration: 2}}
	return v
}

func coveredTestCluster() *privatev1.Cluster {
	cluster := testCluster(true, true)
	cluster.Spec.Platform.GCP.IAM = &privatev1.ClusterIAMSpec{
		WorkerServiceAccount: "123456789012-compute@developer.gserviceaccount.com",
		TemplateApplications: []privatev1.WIFConfigTemplateReference{npTestPin},
	}
	return cluster
}

// persistCapturedNPStatus simulates a durable status write for multi-reconcile
// flows backed by the mock store.
func persistCapturedNPStatus(t *testing.T, store *mockStoreClient) {
	t.Helper()
	require.NotNil(t, store.statusWriter.captured)
	np, ok := store.statusWriter.captured.(*privatev1.NodePool)
	require.True(t, ok)
	store.nodepool.Status = np.Status
}

// Receipts are maintained even while enforcement is off: the durable receipt
// for the exact target is persisted before any external Apply, and a
// successful non-stale delivery promotes it. Zero Apply on failed persistence.
func TestReconcile_IAMDispatchReceiptPersistedBeforeApply(t *testing.T) {
	for _, statusErr := range []error{nil, fmt.Errorf("status unavailable"), conflictErr()} {
		name := "successful write"
		if statusErr != nil {
			name = statusErr.Error()
		}
		t.Run(name, func(t *testing.T) {
			np := testNodePool("4.22.16")
			np.SetUID(types.UID("np-uid-1234"))
			cluster := coveredTestCluster()
			tr := mock.New()
			r, store := buildReconciler(t, np, cluster, tr, nil, nil, statusErr)
			store.version = npDispatchVersion(true, "")

			_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
			if statusErr != nil && !apierrors.IsConflict(statusErr) {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, tr.ApplyCalls, "the IAM receipt must be durable before any external Apply")
			captured := store.statusWriter.captured.(*privatev1.NodePool)
			require.NotNil(t, captured.Status.IAMDispatch.Pending, "the pending receipt must be persisted")
			require.Nil(t, captured.Status.IAMDispatch.Dispatched)
			receipt := captured.Status.IAMDispatch.Pending
			require.Equal(t, string(np.UID), receipt.ObjectUID)
			require.Equal(t, string(cluster.UID), receipt.ClusterUID)
			require.Equal(t, "4.22.16", receipt.ReleaseVersion)
			require.Equal(t, np.Status.VersionResolution.ReleaseImage, receipt.ReleaseImage, "the receipt binds the payload actually dispatched")
			require.Equal(t, "stable", receipt.ChannelGroup)
			require.Equal(t, npTestPin, receipt.TemplateReference)
			require.NotNil(t, captured.Status.IAMRequirements, "the requirements projection refreshes in the same write")
			require.NotNil(t, captured.Status.IAMRequirements.Pending)

			if statusErr != nil {
				return
			}
			persistCapturedNPStatus(t, store)
			_, err = r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
			require.NoError(t, err)
			require.Len(t, tr.ApplyCalls, 1, "a committed receipt authorizes its own exact target")
			captured = store.statusWriter.captured.(*privatev1.NodePool)
			require.NotNil(t, captured.Status.IAMDispatch.Dispatched, "a non-stale delivery promotes the receipt")
			require.Equal(t, receipt, captured.Status.IAMDispatch.Dispatched)
			require.Equal(t, receipt, captured.Status.IAMDispatch.Pending, "the committed pending intent is preserved for redelivery")
		})
	}
}

// Enforcement blocks an uncovered installation fail-closed with the customer
// action reason, never dispatching; the gate is level-triggered and releases
// as soon as the parent Cluster asserts the exact pinned contract.
func TestReconcile_IAMEnforcementBlocksUncoveredInstall(t *testing.T) {
	np := testNodePool("4.22.16")
	np.SetUID(types.UID("np-uid-1234"))
	np.SetResourceVersion("1")
	cluster := coveredTestCluster()
	cluster.Spec.Platform.GCP.IAM.TemplateApplications = nil
	tr := mock.New()
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	store.version = npDispatchVersion(true, "")
	r.WithIAMDispatchEnforcement(true)

	result, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)
	require.Empty(t, tr.ApplyCalls, "an uncovered installation must not dispatch")
	captured := store.statusWriter.captured.(*privatev1.NodePool)
	gate := meta.FindStatusCondition(captured.Status.Conditions, privatev1.NodePoolIAMPermissionsReady)
	require.NotNil(t, gate)
	require.Equal(t, metav1.ConditionFalse, gate.Status)
	require.Equal(t, privatev1.WIFConfigUpdateRequired, gate.Reason)
	// The gate condition must travel in the patch payload itself. The
	// pre-gate snapshot is what makes the diff non-empty; asserting only on
	// the passed object would not catch a snapshot taken after the mutation.
	require.Contains(t, string(store.statusWriter.patchData), string(privatev1.NodePoolIAMPermissionsReady))

	// The customer asserts the exact pinned contract on the parent Cluster;
	// the next pass proceeds to the receipt boundary.
	store.cluster.Spec.Platform.GCP.IAM.TemplateApplications = []privatev1.WIFConfigTemplateReference{npTestPin}
	store.nodepool.Status = captured.Status
	_, err = r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "the first unblocked pass persists the receipt before Apply")
	captured = store.statusWriter.captured.(*privatev1.NodePool)
	require.NotNil(t, captured.Status.IAMDispatch.Pending)
	gate = meta.FindStatusCondition(captured.Status.Conditions, privatev1.NodePoolIAMPermissionsReady)
	require.Equal(t, metav1.ConditionTrue, gate.Status)
	require.Contains(t, string(store.statusWriter.patchData), string(privatev1.NodePoolIAMPermissionsReady),
		"the released gate condition must persist in the boundary write")
}

// An unavailable version contract blocks enforcement fail-closed with the
// platform action reason, carrying the Version's own availability reason.
func TestReconcile_IAMEnforcementBlocksUnavailableVersion(t *testing.T) {
	np := testNodePool("4.22.16")
	np.SetUID(types.UID("np-uid-1234"))
	np.SetResourceVersion("1")
	cluster := coveredTestCluster()
	tr := mock.New()
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	store.version = npDispatchVersion(false, "TemplateUnverified")
	r.WithIAMDispatchEnforcement(true)

	_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "an unverifiable contract must not dispatch")
	gate := meta.FindStatusCondition(store.statusWriter.captured.(*privatev1.NodePool).Status.Conditions, privatev1.NodePoolIAMPermissionsReady)
	require.NotNil(t, gate)
	require.Equal(t, metav1.ConditionFalse, gate.Status)
	require.Equal(t, privatev1.VersionIAMUnavailable, gate.Reason)
	require.Contains(t, gate.Message, "TemplateUnverified")
	require.Contains(t, string(store.statusWriter.patchData), string(privatev1.NodePoolIAMPermissionsReady))
}

// The dispatch predicate reads the configured uncached catalog reader rather
// than the cached client: an availability change visible only through the
// uncached reader must gate new work at the Apply boundary.
func TestReconcile_IAMDispatchPredicateUsesCatalogReader(t *testing.T) {
	np := testNodePool("4.22.16")
	np.SetUID(types.UID("np-uid-1234"))
	cluster := coveredTestCluster()
	tr := mock.New()
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	// The cached client still observes the Version as available; only the
	// uncached reader sees the revocation.
	store.version = npDispatchVersion(true, "")
	catalogReader := &mockStoreClient{
		cluster:      cluster,
		statusWriter: &mockStatusWriter{},
		version:      npDispatchVersion(false, "TemplateUnverified"),
	}
	r.WithIAMDispatchEnforcement(true).WithCatalogReader(catalogReader)

	_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "a revocation visible only to the uncached reader must gate dispatch")
	gate := meta.FindStatusCondition(store.statusWriter.captured.(*privatev1.NodePool).Status.Conditions, privatev1.NodePoolIAMPermissionsReady)
	require.NotNil(t, gate)
	require.Equal(t, metav1.ConditionFalse, gate.Status)
	require.Equal(t, privatev1.VersionIAMUnavailable, gate.Reason)
}

// A persisted receipt bound to different object or parent identities never
// counts as committed: a recreated NodePool or parent Cluster cannot inherit a
// prior dispatch authorization, mirroring the public requirements projection.
func TestReconcile_IAMEnforcementForeignUIDReceiptNotCommitted(t *testing.T) {
	np := testNodePool("4.22.16")
	np.SetUID(types.UID("np-uid-1234"))
	np.SetResourceVersion("1")
	cluster := coveredTestCluster()
	cluster.Spec.Platform.GCP.IAM.TemplateApplications = nil // uncovered
	tr := mock.New()
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	store.version = npDispatchVersion(true, "")
	// The exact same intent, but bound to a different object and parent.
	np.Status.IAMDispatch = &privatev1.IAMDispatchState{
		Pending: &privatev1.IAMDispatchReceipt{
			ObjectUID:         "old-object-uid",
			ClusterUID:        "old-cluster-uid",
			ReleaseVersion:    "4.22.16",
			ReleaseImage:      "quay.io/openshift-release-dev/ocp-release:4.22.16-x86_64",
			ChannelGroup:      "stable",
			TemplateReference: npTestPin,
		},
	}
	r.WithIAMDispatchEnforcement(true)

	_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "a foreign-identity receipt must not authorize dispatch")
	gate := meta.FindStatusCondition(store.statusWriter.captured.(*privatev1.NodePool).Status.Conditions, privatev1.NodePoolIAMPermissionsReady)
	require.NotNil(t, gate)
	require.Equal(t, metav1.ConditionFalse, gate.Status)
	require.Equal(t, privatev1.WIFConfigUpdateRequired, gate.Reason)
}

// A successful Apply that reports stale read-back still promotes the receipt:
// Dispatched records that the transport accepted the dispatch write, while
// read-back staleness delays only the observed conditions.
func TestReconcile_IAMDispatchReceiptPromotedOnStaleFeedback(t *testing.T) {
	np := testNodePool("4.22.16")
	np.SetUID(types.UID("np-uid-1234"))
	cluster := coveredTestCluster()
	tr := mock.New()
	groupKey, err := transport.NodePoolGroupKey(np.Namespace, np.Spec.ClusterID, np.Name)
	require.NoError(t, err)
	tr.StatusOverrides["mc-us-c1/"+groupKey] = &transport.Status{Stale: true}
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	store.version = npDispatchVersion(true, "")

	_, err = r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "the first pass persists the receipt before Apply")
	persistCapturedNPStatus(t, store)

	result, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1, "a committed receipt authorizes its own exact target")
	require.Equal(t, 15*time.Second, result.RequeueAfter, "stale feedback requeues quickly")
	captured := store.statusWriter.captured.(*privatev1.NodePool)
	require.NotNil(t, captured.Status.IAMDispatch.Dispatched, "a stale read-back still promotes the receipt after the write was accepted")
	require.Equal(t, captured.Status.IAMDispatch.Pending, captured.Status.IAMDispatch.Dispatched, "the committed pending intent is preserved")
}

// Continued reconciliation of the observed release is not gated: a
// verification revocation never recalls a committed rollout, but the receipts
// for the running target are still maintained.
func TestReconcile_IAMEnforcementMaintenanceNotGated(t *testing.T) {
	np := testNodePool("4.22.16")
	np.SetUID(types.UID("np-uid-1234"))
	np.Status.Version = "4.22.16"
	cluster := coveredTestCluster()
	tr := mock.New()
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	store.version = npDispatchVersion(false, "TemplateUnverified")
	r.WithIAMDispatchEnforcement(true)

	_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "the first pass persists the receipt before Apply")
	captured := store.statusWriter.captured.(*privatev1.NodePool)
	gate := meta.FindStatusCondition(captured.Status.Conditions, privatev1.NodePoolIAMPermissionsReady)
	require.NotNil(t, gate, "the gate condition is reported for enforced objects")
	require.Equal(t, metav1.ConditionTrue, gate.Status, "maintenance of the observed release is not gated")
	require.NotNil(t, captured.Status.IAMDispatch.Pending)

	persistCapturedNPStatus(t, store)
	_, err = r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1, "maintenance of the observed release must continue")
	require.NotNil(t, store.statusWriter.captured.(*privatev1.NodePool).Status.IAMDispatch.Dispatched)
}

// An already committed exact target may be redelivered after a revocation;
// it never authorizes a different target.
func TestReconcile_IAMCommittedTargetRedeliveredAfterRevocation(t *testing.T) {
	np := testNodePool("4.22.16")
	np.SetUID(types.UID("np-uid-1234"))
	np.Status.Version = "4.22.14"
	meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{Type: privatev1.NodePoolUpgradeProgressing, Status: metav1.ConditionTrue, Reason: "UpgradeScheduled", ObservedGeneration: 3})
	meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{Type: privatev1.NodePoolUpgradeSubmitted, Status: metav1.ConditionUnknown, Reason: "Dispatching", ObservedGeneration: 3})
	np.Status.IAMDispatch = &privatev1.IAMDispatchState{Pending: &privatev1.IAMDispatchReceipt{
		ObjectUID: string(np.UID), ClusterUID: string(coveredTestCluster().UID), ReleaseVersion: "4.22.16",
		ReleaseImage: np.Status.VersionResolution.ReleaseImage, ChannelGroup: "stable", TemplateReference: npTestPin,
	}}
	cluster := coveredTestCluster()
	tr := mock.New()
	r, store := buildReconciler(t, np, cluster, tr, nil, nil, nil)
	store.version = npDispatchVersion(false, "TemplateUnverified")
	r.WithIAMDispatchEnforcement(true)

	_, err := r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls, "the first pass records the gate condition before Apply")
	captured := store.statusWriter.captured.(*privatev1.NodePool)
	gate := meta.FindStatusCondition(captured.Status.Conditions, privatev1.NodePoolIAMPermissionsReady)
	require.NotNil(t, gate)
	require.Equal(t, metav1.ConditionTrue, gate.Status, "a committed target must not be re-gated")

	persistCapturedNPStatus(t, store)
	_, err = r.Reconcile(t.Context(), npReq(np.Namespace, np.Name))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1, "a committed exact target is redelivered despite the revocation")
	captured = store.statusWriter.captured.(*privatev1.NodePool)
	require.NotNil(t, captured.Status.IAMDispatch.Dispatched)
}
