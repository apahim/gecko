package versionsync

import (
	"context"
	"fmt"

	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Availability condition reasons owned by the version-sync controller.
const (
	availabilityReasonAvailable          = "Available"
	availabilityReasonMetadataIncomplete = "ReleaseMetadataIncomplete"
	availabilityReasonTemplateMissing    = "TemplateReferenceMissing"
	availabilityReasonTemplateUnverified = "TemplateUnverified"
	availabilityReasonTemplateStale      = "TemplateReferenceStale"
	availabilityReasonTemplateInvalid    = "TemplateReferenceInvalid"
	availabilityReasonReviewRequired     = "TemplateReferenceReviewRequired"
)

// versionAvailability computes the Available condition for one Version. True
// requires complete release metadata plus a pinned reference that exactly
// matches the highest applicable boundary's identity and executable digest
// and is verified. Boundaries retained in the catalog but failing current
// contract validation are unpinnable and block fail-closed with a distinct
// reason. A missing, False, Unknown or stale condition never
// authorizes dispatch; consumers must also require a current
// observedGeneration, which the caller carries.
func versionAvailability(version *privatev1.Version, templates []privatev1.WIFConfigTemplate) metav1.Condition {
	condition := metav1.Condition{
		Type:               privatev1.VersionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: version.Generation,
		Reason:             availabilityReasonMetadataIncomplete,
		Message:            "Release metadata is incomplete",
	}
	if version.Spec.ReleaseImage == "" || len(version.Spec.ChannelGroups) == 0 {
		return condition
	}
	pin := version.Spec.WIFConfigTemplateReference
	floor := selectTemplateBoundary(version.Name, templates)
	if floor == nil {
		if pin == nil {
			condition.Reason = availabilityReasonTemplateMissing
			condition.Message = "No applicable IAM template boundary is pinned for this release"
			return condition
		}
		// The pin is immutable once set, so a boundary disappearing from the
		// catalog surfaces as a review-required unavailable state.
		condition.Reason = availabilityReasonReviewRequired
		condition.Message = fmt.Sprintf("The IAM catalog no longer contains an applicable boundary for pinned template %q", pin.Name)
		return condition
	}
	// A retained boundary failing current contract validation can never be
	// pinned or republished: block fail-closed instead of falling back to a
	// lower boundary or attempting a write admission will reject.
	if boundaryErr := pinnableBoundary(floor); boundaryErr != nil {
		condition.Reason = availabilityReasonTemplateInvalid
		condition.Message = fmt.Sprintf("The highest applicable IAM template boundary %q fails current contract validation and cannot be pinned: %v", floor.Name, boundaryErr)
		return condition
	}
	if pin == nil {
		condition.Reason = availabilityReasonTemplateMissing
		condition.Message = "No applicable IAM template boundary is pinned for this release"
		return condition
	}
	// The pin is immutable once set, so a later higher applicable boundary
	// cannot repoint it: it surfaces as a review-required unavailable state.
	if floor.Name != pin.Name {
		condition.Reason = availabilityReasonReviewRequired
		condition.Message = fmt.Sprintf("Pinned template boundary %q is below the highest applicable boundary %q; the reference is immutable and requires review", pin.Name, floor.Name)
		return condition
	}
	if string(floor.UID) != pin.UID {
		condition.Reason = availabilityReasonTemplateStale
		condition.Message = fmt.Sprintf("Pinned template %q no longer identifies the retained catalog resource", pin.Name)
		return condition
	}
	digest, err := privatev1.WIFConfigDigest(floor.Spec)
	if err != nil || digest != pin.Digest {
		condition.Reason = availabilityReasonTemplateStale
		condition.Message = fmt.Sprintf("Pinned template %q no longer matches the catalog contract digest", pin.Name)
		return condition
	}
	if !privatev1.IsWIFConfigVerified(floor.Spec) {
		condition.Reason = availabilityReasonTemplateUnverified
		condition.Message = fmt.Sprintf("Template %q is not verified for new use", pin.Name)
		return condition
	}
	condition.Status = metav1.ConditionTrue
	condition.Reason = availabilityReasonAvailable
	condition.Message = fmt.Sprintf("Release is available with verified template %q", pin.Name)
	return condition
}

// refreshEligibility re-observes template verification and boundary
// publication faster than the discovery sync: it pins unpinned Versions whose
// boundary has been published and rewrites availability conditions. A failed
// template read skips the whole pass, preserving pins and previous conditions
// rather than flapping availability; the level-triggered pass retries.
func (c *Controller) refreshEligibility(ctx context.Context, log logger.Logger) {
	templates, err := c.templates(ctx)
	if err != nil {
		log.Errorf(ctx, "list WIFConfigTemplates failed, preserving version availability snapshot: %v", err)
		return
	}
	var versions privatev1.VersionList
	if err := c.apiClient.List(ctx, &versions); err != nil {
		log.Errorf(ctx, "list versions for eligibility refresh failed: %v", err)
		return
	}
	for i := range versions.Items {
		version := &versions.Items[i]
		if !version.DeletionTimestamp.IsZero() {
			continue
		}
		if version.Spec.WIFConfigTemplateReference == nil {
			pin, err := resolvePin(version.Name, templates)
			if err != nil {
				log.Errorf(ctx, "resolve IAM template reference for Version %q failed: %v", version.Name, err)
			} else if pin != nil {
				version.Spec.WIFConfigTemplateReference = pin
				if err := c.apiClient.Update(ctx, version); err != nil {
					log.Errorf(ctx, "pin Version %q to IAM template %q failed: %v", version.Name, pin.Name, err)
					continue // Availability must not report a stale generation; retry next pass.
				}
				log.Infof(ctx, "pinned Version %s to IAM template %s", version.Name, pin.Name)
			}
		}
		before := version.DeepCopy()
		meta.SetStatusCondition(&version.Status.Conditions, versionAvailability(version, templates))
		if equality.Semantic.DeepEqual(before.Status, version.Status) {
			continue
		}
		if err := c.apiClient.Status().Update(ctx, version); err != nil {
			log.Errorf(ctx, "update Version %q availability failed: %v", version.Name, err)
		}
	}
}
