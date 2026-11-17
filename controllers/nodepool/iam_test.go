package nodepool

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/client/transport/mock"
	"github.com/openshift-online/gecko/controllers/util/catalog/catalogtest"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type observationCounter struct {
	*mock.Client
	observations int
}

func (tr *observationCounter) GetStatus(ctx context.Context, target, key string) (*transport.Status, error) {
	tr.observations++
	return tr.Client.GetStatus(ctx, target, key)
}

func TestNodePoolExactIAMAndInitialDispatchBoundary(t *testing.T) {
	p := testNodePool("5.0.0")
	p.Status.VersionResolution.ChannelGroup = "candidate"
	c := testCluster(true, true)
	c.Namespace = p.Namespace
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Release.Version}, Spec: privatev1.VersionSpec{ReleaseImage: p.Status.VersionResolution.ReleaseImage, ChannelGroups: []string{"candidate"}}}
	w := catalogtest.Ready(t, v)
	p.Status.VersionResolution.WIFConfigTemplateRef = v.Spec.WIFConfigTemplateRef.DeepCopy()
	store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(&privatev1.NodePool{}, &privatev1.Cluster{}, &privatev1.Version{}).WithObjects(p, c, v, w).Build()
	tr := &observationCounter{Client: mock.New()}
	r := New(tr, newTestLogger(t), store).WithAPIReader(store)
	_, err := r.Reconcile(t.Context(), npReq(p.Namespace, p.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls)
	require.Zero(t, tr.observations)
	current := &privatev1.NodePool{}
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(p), current))
	require.Equal(t, privatev1.WIFConfigUpdateRequired, meta.FindStatusCondition(current.Status.Conditions, privatev1.IAMPermissionsReady).Reason)
	require.Nil(t, current.Status.IAMDispatch)
	// Test-process-only assertion, not a live record or cloud-verification shortcut.
	catalogtest.Apply(c, v.Spec.WIFConfigTemplateRef)
	require.NoError(t, store.Update(t.Context(), c))
	_, err = r.Reconcile(t.Context(), npReq(p.Namespace, p.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(p), current))
	require.NotNil(t, current.Status.IAMDispatch.Pending)
	require.Equal(t, string(c.UID), current.Status.IAMDispatch.Pending.ClusterUID)
	_, err = r.Reconcile(t.Context(), npReq(p.Namespace, p.Name))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1)
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(tr.ApplyCalls[0].Manifests[0], &manifest))
	gcp := manifest["spec"].(map[string]any)["platform"].(map[string]any)["gcp"].(map[string]any)
	require.Equal(t, map[string]any{"email": c.Spec.Platform.GCP.IAM.WorkerServiceAccount}, gcp["serviceAccount"])
}

func TestPendingIntentAloneIsNotObservedWork(t *testing.T) {
	p := testNodePool("5.0.0")
	p.Status.IAMDispatch = &privatev1.IAMDispatchState{Pending: &privatev1.IAMDispatchReceipt{ReleaseVersion: "5.0.0"}}
	require.False(t, poolWorkKnown(p))
	c := testCluster(true, true)
	tr := &observationCounter{Client: mock.New()}
	store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).Build()
	r := New(tr, newTestLogger(t), store).WithAPIReader(store)
	_, err := r.observeResources(t.Context(), p, c)
	require.NoError(t, err)
	require.Zero(t, tr.observations)
	p.Status.Version = "5.0.0"
	require.True(t, poolWorkKnown(p))
}

type countStatusUpdates struct {
	client.Client
	writes *int
}
type countedStatusWriter struct {
	client.SubResourceWriter
	writes *int
}

func (c countStatusUpdates) Status() client.SubResourceWriter {
	return countedStatusWriter{c.Client.Status(), c.writes}
}
func (w countedStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	*w.writes++
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestObservePromotesOnlyNonStaleFeedbackInOneWrite(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			p := testNodePool("5.0.0")
			c := testCluster(true, true)
			p.Status.Version = "4.99.0"
			catalogtest.CommittedPool(p, c)
			p.Status.IAMDispatch.Pending = p.Status.IAMDispatch.Dispatched.DeepCopy()
			p.Status.IAMDispatch.Dispatched.ReleaseVersion = "4.99.0"
			pending := p.Status.IAMDispatch.Pending.DeepCopy()
			store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(p).WithObjects(p).Build()
			tr := &observationCounter{Client: mock.New()}
			group, err := transport.NodePoolGroupKey(p.Namespace, p.Spec.ClusterID, p.Name)
			require.NoError(t, err)
			key := transport.ResourceKey("hypershift.openshift.io", "v1beta1", "nodepools", "clusters-"+string(c.UID), p.Name)
			tr.StatusOverrides[c.Status.PlacementResult.ManagementClusterName+"/"+group] = &transport.Status{Stale: stale, ResourceStatuses: map[string]map[string]string{key: {"version": "5.0.0"}}}
			writes := 0
			r := New(tr, newTestLogger(t), countStatusUpdates{store, &writes}).WithAPIReader(store)
			_, err = r.observeResources(t.Context(), p, c)
			require.NoError(t, err)
			require.Equal(t, 1, tr.observations)
			current := &privatev1.NodePool{}
			require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(p), current))
			if stale {
				require.Equal(t, pending, current.Status.IAMDispatch.Pending)
				require.Zero(t, writes)
			} else {
				require.Nil(t, current.Status.IAMDispatch.Pending)
				require.Equal(t, pending, current.Status.IAMDispatch.Dispatched)
				require.Equal(t, 1, writes)
				require.Equal(t, "5.0.0", current.Status.Version)
			}
		})
	}
}
