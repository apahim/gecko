package versionsync

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func readVersion(t *testing.T, store client.Client, name string) *privatev1.Version {
	t.Helper()
	version := &privatev1.Version{}
	require.NoError(t, store.Get(context.Background(), client.ObjectKey{Name: name}, version))
	return version
}

func availabilityVersion(name string, generation int64, pin *privatev1.WIFConfigTemplateReference) *privatev1.Version {
	return &privatev1.Version{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: generation},
		Spec: privatev1.VersionSpec{
			ReleaseImage:               "quay.io/release:" + name,
			ChannelGroups:              []string{"stable"},
			WIFConfigTemplateReference: pin,
		},
	}
}

func pinnedReference(t *testing.T, template privatev1.WIFConfigTemplate) *privatev1.WIFConfigTemplateReference {
	t.Helper()
	digest, err := privatev1.WIFConfigDigest(template.Spec)
	require.NoError(t, err)
	return &privatev1.WIFConfigTemplateReference{Name: template.Name, UID: string(template.UID), Digest: digest}
}

func availabilityCondition(t *testing.T, store client.Client, name string) *metav1.Condition {
	t.Helper()
	condition := readVersionCondition(t, store, name)
	require.NotNil(t, condition)
	return condition
}

func readVersionCondition(t *testing.T, store client.Client, name string) *metav1.Condition {
	t.Helper()
	version := readVersion(t, store, name)
	for i := range version.Status.Conditions {
		if version.Status.Conditions[i].Type == privatev1.VersionAvailable {
			return &version.Status.Conditions[i]
		}
	}
	return nil
}

func refreshTestController(t *testing.T, store client.Client) *Controller {
	t.Helper()
	return &Controller{apiClient: store, eligibilityInterval: defaultEligibilityInterval}
}

// The fast loop pins an unpinned Version whose boundary has been published
// and reports it available in the same pass, with a current observed
// generation.
func TestRefreshEligibilityPinsAndReportsAvailable(t *testing.T) {
	template := validTemplate(t, "5.0.0", "template-uid", true)
	store := ciStore(t, availabilityVersion("5.0.6", 3, nil), &template)

	refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))

	version := readVersion(t, store, "5.0.6")
	assert.Equal(t, pinnedReference(t, template), version.Spec.WIFConfigTemplateReference)
	condition := availabilityCondition(t, store, "5.0.6")
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, availabilityReasonAvailable, condition.Reason)
	assert.EqualValues(t, version.Generation, condition.ObservedGeneration)
}

// The unverified highest match blocks availability; selection never falls
// back to a lower verified boundary.
func TestRefreshEligibilityUnverifiedHighestMatchBlocks(t *testing.T) {
	verified := validTemplate(t, "5.0.0", "uid-low", true)
	unverified := validTemplate(t, "5.0.3", "uid-high", false)
	store := ciStore(t, availabilityVersion("5.0.6", 1, nil), &verified, &unverified)

	refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))

	version := readVersion(t, store, "5.0.6")
	require.NotNil(t, version.Spec.WIFConfigTemplateReference)
	assert.Equal(t, "5.0.3", version.Spec.WIFConfigTemplateReference.Name, "the unverified highest match is pinned, not the lower verified boundary")
	condition := availabilityCondition(t, store, "5.0.6")
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, availabilityReasonTemplateUnverified, condition.Reason)
}

// Pins are immutable: a later higher applicable boundary produces a
// review-required unavailable condition, never a repointed reference.
func TestRefreshEligibilityHigherBoundaryRequiresReview(t *testing.T) {
	pinned := validTemplate(t, "5.0.0", "uid-low", true)
	higher := validTemplate(t, "5.0.3", "uid-high", true)
	store := ciStore(t, availabilityVersion("5.0.6", 2, pinnedReference(t, pinned)), &pinned, &higher)
	before := readVersion(t, store, "5.0.6").DeepCopy()

	refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))

	after := readVersion(t, store, "5.0.6")
	assert.Equal(t, before.Spec, after.Spec, "the pinned reference must never be repointed")
	condition := availabilityCondition(t, store, "5.0.6")
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, availabilityReasonReviewRequired, condition.Reason)
}

func TestRefreshEligibilityStaleReference(t *testing.T) {
	template := validTemplate(t, "5.0.0", "real-uid", true)
	for _, tc := range []struct {
		name   string
		mutate func(ref *privatev1.WIFConfigTemplateReference)
	}{
		{"uid mismatch", func(ref *privatev1.WIFConfigTemplateReference) { ref.UID = "wrong-uid" }},
		{"digest mismatch", func(ref *privatev1.WIFConfigTemplateReference) {
			ref.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pin := pinnedReference(t, template)
			tc.mutate(pin)
			store := ciStore(t, availabilityVersion("5.0.6", 1, pin), &template)

			refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))

			condition := availabilityCondition(t, store, "5.0.6")
			assert.Equal(t, metav1.ConditionFalse, condition.Status)
			assert.Equal(t, availabilityReasonTemplateStale, condition.Reason)
		})
	}
}

// Versions without an applicable same-minor boundary stay unpinned and
// explicitly unavailable, including releases from minors that never
// published a boundary.
func TestRefreshEligibilityMissingBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		version   string
		templates []privatev1.WIFConfigTemplate
	}{
		{"other major", "4.23.1", []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "template-uid", true)}},
		{"other minor", "5.1.0", []privatev1.WIFConfigTemplate{testTemplate("5.0.0", "template-uid", true)}},
		{"release below boundary", "5.0.2", []privatev1.WIFConfigTemplate{testTemplate("5.0.3", "template-uid", true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := ciStore(t, availabilityVersion(tc.version, 1, nil))
			for i := range tc.templates {
				require.NoError(t, store.Create(context.Background(), &tc.templates[i]))
			}

			refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))

			version := readVersion(t, store, tc.version)
			assert.Nil(t, version.Spec.WIFConfigTemplateReference, "no boundary may be pinned")
			condition := availabilityCondition(t, store, tc.version)
			assert.Equal(t, metav1.ConditionFalse, condition.Status)
			assert.Equal(t, availabilityReasonTemplateMissing, condition.Reason)
		})
	}
}

func TestRefreshEligibilityIncompleteMetadata(t *testing.T) {
	version := availabilityVersion("5.0.6", 1, nil)
	version.Spec.ReleaseImage = ""
	template := testTemplate("5.0.0", "template-uid", true)
	store := ciStore(t, version, &template)

	refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))

	condition := availabilityCondition(t, store, "5.0.6")
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, availabilityReasonMetadataIncomplete, condition.Reason)
}

// Re-running the pass (a producer restart or a no-op tick) issues no writes
// and keeps transition times stable.
func TestRefreshEligibilityNoOpAndRestartStability(t *testing.T) {
	template := validTemplate(t, "5.0.0", "template-uid", true)
	store := ciStore(t, availabilityVersion("5.0.6", 3, nil), &template)
	controller := refreshTestController(t, store)

	controller.refreshEligibility(context.Background(), newTestLogger(t))
	first := readVersion(t, store, "5.0.6")

	controller.refreshEligibility(context.Background(), newTestLogger(t))
	second := readVersion(t, store, "5.0.6")

	assert.Equal(t, first.ResourceVersion, second.ResourceVersion, "a no-op pass must not write")
	assert.Equal(t, first.Status, second.Status)
	condition := availabilityCondition(t, store, "5.0.6")
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.EqualValues(t, second.Generation, condition.ObservedGeneration)
}

// A failed template read skips the entire pass: no pins, no condition
// writes, no availability flapping on transient outages.
func TestRefreshEligibilitySkipsPassOnTemplateReadFailure(t *testing.T) {
	template := testTemplate("5.0.0", "template-uid", true)
	store := ciStore(t, availabilityVersion("5.0.6", 3, nil), &template)
	before := readVersion(t, store, "5.0.6").DeepCopy()

	controller := &Controller{apiClient: &templateListFailing{Client: store}, eligibilityInterval: defaultEligibilityInterval}
	controller.refreshEligibility(context.Background(), newTestLogger(t))

	after := readVersion(t, store, "5.0.6")
	assert.Equal(t, before.Spec, after.Spec)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "no write may occur when template reads fail")
	assert.Nil(t, readVersionCondition(t, store, "5.0.6"))
}

// A retained boundary failing current contract validation can never be
// pinned: the fast loop attempts no rejected write, leaves the Version
// unpinned, and reports a distinct fail-closed condition instead of falling
// back to a lower valid boundary. Retained catalogs can hold such boundaries
// permanently because the catalog is append-only and contracts immutable.
func TestRefreshEligibilityInvalidBoundaryBlocks(t *testing.T) {
	valid := validTemplate(t, "5.0.0", "uid-low", true)
	invalid := testTemplate("5.0.3", "uid-high", true)
	store := ciStore(t, availabilityVersion("5.0.6", 1, nil))
	require.NoError(t, store.Create(context.Background(), &valid))
	require.NoError(t, store.Create(context.Background(), &invalid))
	before := readVersion(t, store, "5.0.6").DeepCopy()

	refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))

	after := readVersion(t, store, "5.0.6")
	assert.Nil(t, after.Spec.WIFConfigTemplateReference, "an invalid boundary must not be pinned")
	assert.Equal(t, before.Spec, after.Spec)
	condition := availabilityCondition(t, store, "5.0.6")
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, availabilityReasonTemplateInvalid, condition.Reason)
	assert.Contains(t, condition.Message, "5.0.3")
}

// A failed pin write (for example a conflict) is retried on the next pass;
// the failed pass writes no condition against a stale generation.
func TestRefreshEligibilityRetriesFailedPinWrite(t *testing.T) {
	template := validTemplate(t, "5.0.0", "template-uid", true)
	store := ciStore(t, availabilityVersion("5.0.6", 3, nil), &template)

	failing := &updateFailing{Client: store, failures: 1}
	refreshTestController(t, failing).refreshEligibility(context.Background(), newTestLogger(t))

	version := readVersion(t, store, "5.0.6")
	assert.Nil(t, version.Spec.WIFConfigTemplateReference, "the conflicted pin must not persist")
	assert.Nil(t, readVersionCondition(t, store, "5.0.6"), "no condition may be written against a stale generation")

	refreshTestController(t, store).refreshEligibility(context.Background(), newTestLogger(t))
	version = readVersion(t, store, "5.0.6")
	assert.Equal(t, pinnedReference(t, template), version.Spec.WIFConfigTemplateReference)
	condition := availabilityCondition(t, store, "5.0.6")
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
}

type templateListFailing struct {
	client.Client
}

func (c *templateListFailing) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*privatev1.WIFConfigTemplateList); ok {
		return fmt.Errorf("template catalog unavailable")
	}
	return c.Client.List(ctx, list, opts...)
}

type updateFailing struct {
	client.Client
	failures int
}

func (c *updateFailing) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.failures > 0 {
		c.failures--
		return fmt.Errorf("update conflicted")
	}
	return c.Client.Update(ctx, obj, opts...)
}
