package nodepoolvrresolution

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/util/catalog/catalogtest"
	"github.com/openshift-online/gecko/controllers/util/setup"

	"k8s.io/apimachinery/pkg/api/meta"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCachedResolutionRevocationRecoversWithoutSpecChange(t *testing.T) {
	p := makeNP("cluster-1", "np-1", "5.0.0")
	c := makeCluster("cluster-1")
	c.Namespace = p.Namespace
	v := catalogVersion("5.0.0", "image", "candidate")
	w := catalogtest.Ready(t, v)
	store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(p, v).WithObjects(p, c, v, w).Build()
	r := NewReconciler(newTestLogger(t), store).WithAPIReader(store)
	_, err := r.Reconcile(t.Context(), npReq(p.Namespace, p.Name))
	require.NoError(t, err)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(p), p))
	require.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, "NodePoolVersionResolved"))
	before := p.Spec
	generation := p.Generation
	w.Spec.Verified = new(false)
	require.NoError(t, store.Update(t.Context(), w))
	result, err := r.Reconcile(t.Context(), npReq(p.Namespace, p.Name))
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.LessOrEqual(t, result.RequeueAfter, 30*time.Second)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(p), p))
	require.False(t, meta.IsStatusConditionTrue(p.Status.Conditions, "NodePoolVersionResolved"))
	require.NotNil(t, p.Status.VersionResolution)
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	_, err = r.Reconcile(t.Context(), npReq(p.Namespace, p.Name))
	require.NoError(t, err)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(p), p))
	require.True(t, meta.IsStatusConditionTrue(p.Status.Conditions, "NodePoolVersionResolved"))
	require.Equal(t, before, p.Spec)
	require.Equal(t, generation, p.Generation)
}
