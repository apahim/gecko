package versionresolution

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/util/catalog/catalogtest"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCachedResolutionRevocationRecoversWithoutSpecChange(t *testing.T) {
	c := &privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "hyperfleet", UID: "cluster-uid", Generation: 1}, Spec: privatev1.ClusterSpec{Release: privatev1.ClusterReleaseSpec{Version: "5.0.0", ChannelGroup: "stable"}}}
	v := catalogVersion("5.0.0", "image", "stable")
	w := catalogtest.Ready(t, v)
	ch := &privatev1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "stable"}, Spec: privatev1.ChannelSpec{FleetMinorVersion: "5.0", InstallDefaultVersion: "5.0.0", MinimumSupportedVersion: "5.0"}}
	store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(c, v).WithObjects(c, v, w, ch).Build()
	r := NewReconciler(newTestLogger(t), store).WithAPIReader(store)
	_, err := r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), c))
	require.True(t, meta.IsStatusConditionTrue(c.Status.Conditions, "VersionResolved"))
	before := c.Spec
	generation := c.Generation
	w.Spec.Verified = new(false)
	require.NoError(t, store.Update(t.Context(), w))
	result, err := r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.LessOrEqual(t, result.RequeueAfter, 30*time.Second)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), c))
	require.False(t, meta.IsStatusConditionTrue(c.Status.Conditions, "VersionResolved"))
	require.NotNil(t, c.Status.VersionResolution)
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	_, err = r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), c))
	require.True(t, meta.IsStatusConditionTrue(c.Status.Conditions, "VersionResolved"))
	require.Equal(t, before, c.Spec)
	require.Equal(t, generation, c.Generation)
}
