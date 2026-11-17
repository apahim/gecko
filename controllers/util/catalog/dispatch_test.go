package catalog

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/util/catalog/catalogtest"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDispatchIdentityAndCommittedRecovery(t *testing.T) {
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "5.0.10"}, Spec: privatev1.VersionSpec{ReleaseImage: "target-image", ChannelGroups: []string{"stable"}}}
	w := catalogtest.Ready(t, v)
	c := &privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "parent-uid"}}
	catalogtest.Apply(c, v.Spec.WIFConfigTemplateRef)
	vr := &privatev1.VersionResolutionResult{ReleaseVersion: v.Name, ReleaseImage: v.Spec.ReleaseImage, ChannelGroup: "stable", CincinnatiChannel: "stable-5.0", WIFConfigTemplateRef: v.Spec.WIFConfigTemplateRef.DeepCopy()}
	store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithObjects(v, w).Build()
	receipt, persist, err := Dispatch(t.Context(), store, c, "pool-uid", vr, nil)
	require.NoError(t, err)
	require.True(t, persist)
	state := &privatev1.IAMDispatchState{Pending: receipt}
	w.Spec.Verified = new(false)
	require.NoError(t, store.Update(t.Context(), w))
	require.NoError(t, store.Delete(t.Context(), v))
	// Exact committed redelivery survives both catalog aging and revocation.
	_, persist, err = Dispatch(t.Context(), store, c, "pool-uid", vr, state)
	require.NoError(t, err)
	require.False(t, persist)
	vr.CincinnatiChannel = "stable-5.1"
	_, persist, err = Dispatch(t.Context(), store, c, "pool-uid", vr, state)
	require.NoError(t, err)
	require.False(t, persist)
	_, _, err = Dispatch(t.Context(), store, c, "different-pool-uid", vr, state)
	require.Error(t, err)
	otherParent := c.DeepCopy()
	otherParent.UID = "recreated-parent"
	_, _, err = Dispatch(t.Context(), store, otherParent, "pool-uid", vr, state)
	require.Error(t, err)
	changed := vr.DeepCopy()
	changed.ReleaseImage = "different-payload"
	_, _, err = Dispatch(t.Context(), store, c, "pool-uid", changed, state)
	require.Error(t, err)
	// Promotion retains committed identity and does not infer a numeric superset.
	require.False(t, privatev1.PromoteIAMDispatch(state, "5.0.9"))
	require.True(t, privatev1.PromoteIAMDispatch(state, v.Name))
	require.Nil(t, state.Pending)
	require.Equal(t, receipt, state.Dispatched)
	_, persist, err = Dispatch(t.Context(), store, c, "pool-uid", vr, state)
	require.NoError(t, err)
	require.False(t, persist)
	missing := c.DeepCopy()
	missing.Spec.Platform.GCP.IAM.TemplateApplications = nil
	_, _, err = Dispatch(t.Context(), store, missing, "pool-uid", vr, state)
	require.Error(t, err)
}

func TestNewDispatchClosesRevocationAndPayloadRaces(t *testing.T) {
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "5.0.0"}, Spec: privatev1.VersionSpec{ReleaseImage: "image", ChannelGroups: []string{"stable"}}}
	w := catalogtest.Ready(t, v)
	c := &privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "cluster-uid"}}
	catalogtest.Apply(c, v.Spec.WIFConfigTemplateRef)
	vr := &privatev1.VersionResolutionResult{ReleaseVersion: v.Name, ReleaseImage: "old-image", ChannelGroup: "stable", WIFConfigTemplateRef: v.Spec.WIFConfigTemplateRef.DeepCopy()}
	store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithObjects(v, w).Build()
	_, _, err := Dispatch(t.Context(), store, c, string(c.UID), vr, nil)
	var blocked *UnavailableError
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, "VersionResolutionOutdated", blocked.Reason)
	vr.ReleaseImage = "image"
	w.Spec.Verified = new(false)
	require.NoError(t, store.Update(t.Context(), w))
	_, _, err = Dispatch(t.Context(), store, c, string(c.UID), vr, nil)
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, privatev1.WIFTemplateUnverified, blocked.Reason)
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	_, persist, err := Dispatch(t.Context(), store, c, string(c.UID), vr, nil)
	require.NoError(t, err)
	require.True(t, persist)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(w), w))
}
