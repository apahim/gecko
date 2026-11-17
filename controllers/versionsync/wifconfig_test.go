package versionsync

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/util/catalog/catalogtest"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func availabilityStore(t *testing.T, name string, withTemplate bool) (client.Client, *privatev1.WIFConfigTemplate) {
	t.Helper()
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: privatev1.VersionSpec{ReleaseImage: "image", ChannelGroups: []string{"stable"}}}
	w := catalogtest.Ready(t, v)
	w.Spec.Verified = new(false)
	v.Spec.WIFConfigTemplateRef = nil
	v.Status.Conditions = nil
	objects := []client.Object{v, ciChannel("stable", name, "5.0")}
	if withTemplate {
		objects = append(objects, w)
	}
	return fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(&privatev1.Version{}, &privatev1.Channel{}).WithObjects(objects...).Build(), w
}

func readAvailability(t *testing.T, c client.Reader, name string) *privatev1.Version {
	t.Helper()
	v := &privatev1.Version{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: name}, v))
	return v
}

func TestUnverifiedPinRecoveryAndNoop(t *testing.T) {
	store, w := availabilityStore(t, "5.0.10", true)
	c := NewController(nil, newTestLogger(t), store)
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	v := readAvailability(t, store, "5.0.10")
	ref := *v.Spec.WIFConfigTemplateRef
	require.Equal(t, "5.0.0", ref.Name)
	require.Equal(t, privatev1.WIFTemplateUnverified, meta.FindStatusCondition(v.Status.Conditions, privatev1.VersionAvailable).Reason)
	require.False(t, privatev1.IsVersionAvailable(v))
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(w), w))
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	v = readAvailability(t, store, v.Name)
	require.True(t, privatev1.IsVersionAvailable(v))
	require.Equal(t, ref, *v.Spec.WIFConfigTemplateRef)
	before := v.DeepCopy()
	channel := readChannel(t, store, "stable")
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	require.True(t, reflect.DeepEqual(before, readAvailability(t, store, v.Name)), "no-op changed status, transition time or resource version")
	require.Equal(t, channel, readChannel(t, store, "stable"))
	w.Spec.Verified = new(false)
	require.NoError(t, store.Update(t.Context(), w))
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	require.False(t, privatev1.IsVersionAvailable(readAvailability(t, store, v.Name)))
	require.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(readChannel(t, store, "stable").Status.Conditions, privatev1.ChannelDefaultVersionAvailable).Status)
}

func TestHighestUnverifiedBoundaryDoesNotFallBack(t *testing.T) {
	store, w := availabilityStore(t, "5.0.0", true)
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	higher := w.DeepCopy()
	higher.Name = "5.0.6"
	higher.UID = "higher-unverified"
	higher.ResourceVersion = ""
	higher.Spec.Verified = new(false)
	require.NoError(t, store.Create(t.Context(), higher))
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "5.0.10"}, Spec: privatev1.VersionSpec{ReleaseImage: "image", ChannelGroups: []string{"stable"}}}
	require.NoError(t, store.Create(t.Context(), v))
	c := NewController(nil, newTestLogger(t), store)
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	got := readAvailability(t, store, v.Name)
	require.Equal(t, "5.0.6", got.Spec.WIFConfigTemplateRef.Name)
	require.False(t, privatev1.IsVersionAvailable(got))
	require.True(t, privatev1.IsVersionAvailable(readAvailability(t, store, "5.0.0")))
}

func TestLateTemplateAndReplacedPin(t *testing.T) {
	store, w := availabilityStore(t, "5.0.10", false)
	c := NewController(nil, newTestLogger(t), store)
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	v := readAvailability(t, store, "5.0.10")
	require.Nil(t, v.Spec.WIFConfigTemplateRef)
	require.False(t, privatev1.IsVersionAvailable(v))
	w.Spec.Verified = new(true)
	require.NoError(t, store.Create(t.Context(), w))
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	v = readAvailability(t, store, v.Name)
	require.True(t, privatev1.IsVersionAvailable(v))
	ref := *v.Spec.WIFConfigTemplateRef
	// Simulate externally corrupted/replaced storage, not an allowed API delete.
	require.NoError(t, store.Delete(t.Context(), w))
	w.ResourceVersion = ""
	w.UID = "replacement"
	require.NoError(t, store.Create(t.Context(), w))
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	v = readAvailability(t, store, v.Name)
	require.Equal(t, ref, *v.Spec.WIFConfigTemplateRef)
	require.Equal(t, privatev1.WIFTemplateIntegrityMismatch, meta.FindStatusCondition(v.Status.Conditions, privatev1.VersionAvailable).Reason)
}

type templateReadFailure struct{ client.Client }

func (c templateReadFailure) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	if _, ok := obj.(*privatev1.WIFConfigTemplateList); ok {
		return errors.New("untrusted-storage-details")
	}
	return c.Client.List(ctx, obj, opts...)
}

func TestTemplateReadFailurePreservesPinsAndCatalog(t *testing.T) {
	store, w := availabilityStore(t, "5.0.10", true)
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	c := NewController(nil, newTestLogger(t), store)
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	before := readAvailability(t, store, "5.0.10")
	c.apiClient = templateReadFailure{store}
	require.Error(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	got := readAvailability(t, store, before.Name)
	require.Equal(t, before.Spec, got.Spec)
	require.Equal(t, metav1.ConditionUnknown, meta.FindStatusCondition(got.Status.Conditions, privatev1.VersionAvailable).Status)
	require.Error(t, c.apply(t.Context(), newTestLogger(t), map[string]privatev1.VersionSpec{}))
	require.Equal(t, before.Spec, readAvailability(t, store, before.Name).Spec)
}

func TestCatalogRefreshPreservesPinnedReference(t *testing.T) {
	store, w := availabilityStore(t, "5.0.10", true)
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	c := NewController(nil, newTestLogger(t), store)
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	before := readAvailability(t, store, "5.0.10")
	desired := privatev1.VersionSpec{ReleaseImage: "updated-image", ChannelGroups: []string{"fast", "stable"}}
	require.NoError(t, c.apply(t.Context(), newTestLogger(t), map[string]privatev1.VersionSpec{before.Name: desired}))
	got := readAvailability(t, store, before.Name)
	require.Equal(t, before.Spec.WIFConfigTemplateRef, got.Spec.WIFConfigTemplateRef)
	require.Equal(t, "updated-image", got.Spec.ReleaseImage)
	newBoundary := w.DeepCopy()
	newBoundary.Name = "5.0.6"
	newBoundary.UID = "new-boundary"
	newBoundary.ResourceVersion = ""
	require.NoError(t, store.Create(t.Context(), newBoundary))
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	require.Equal(t, before.Spec.WIFConfigTemplateRef, readAvailability(t, store, before.Name).Spec.WIFConfigTemplateRef)
	got = readAvailability(t, store, before.Name)
	require.False(t, privatev1.IsVersionAvailable(got))
	require.Equal(t, "WIFConfigBoundaryChanged", meta.FindStatusCondition(got.Status.Conditions, privatev1.VersionAvailable).Reason)
}

type poisonedVersionStatus struct{ client.Client }
type poisonedWriter struct{ client.SubResourceWriter }

func (c poisonedVersionStatus) Status() client.SubResourceWriter {
	return poisonedWriter{c.Client.Status()}
}
func (w poisonedWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if v, ok := obj.(*privatev1.Version); ok && v.Name == "5.0.1" {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "versions/status"}, v.Name, errors.New("untrusted-server-details"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestOnePoisonedVersionDoesNotStarveLaterAvailability(t *testing.T) {
	store, w := availabilityStore(t, "5.0.10", true)
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	bad := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "5.0.1"}, Spec: privatev1.VersionSpec{ReleaseImage: "image", ChannelGroups: []string{"stable"}}}
	require.NoError(t, store.Create(t.Context(), bad))
	c := NewController(nil, newTestLogger(t), poisonedVersionStatus{store})
	require.Error(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	require.True(t, privatev1.IsVersionAvailable(readAvailability(t, store, "5.0.10")))
	require.False(t, privatev1.IsVersionAvailable(readAvailability(t, store, "5.0.1")))
}

func TestEligibilityRecoveryDoesNotEraseUpstreamFailureDiagnostic(t *testing.T) {
	store, w := availabilityStore(t, "5.0.10", true)
	c := NewController(nil, newTestLogger(t), store)
	c.reportSourceDefaults(t.Context(), newTestLogger(t), nil, "FetchFailed")
	w.Spec.Verified = new(true)
	require.NoError(t, store.Update(t.Context(), w))
	require.NoError(t, c.refreshAvailability(t.Context(), newTestLogger(t)))
	require.True(t, privatev1.IsVersionAvailable(readAvailability(t, store, "5.0.10")))
	require.Equal(t, "FetchFailed", meta.FindStatusCondition(readChannel(t, store, "stable").Status.Conditions, privatev1.ChannelDefaultVersionAvailable).Reason)
	require.LessOrEqual(t, availabilityInterval, 30*time.Second)
}
