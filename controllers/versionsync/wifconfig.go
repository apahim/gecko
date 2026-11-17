package versionsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const availabilityInterval = 30 * time.Second

// WithAPIReader avoids informer lag when evaluating mutable eligibility.
func (c *Controller) WithAPIReader(reader client.Reader) *Controller {
	c.catalogReader = reader
	return c
}

func (c *Controller) reader() client.Reader {
	if c.catalogReader != nil {
		return c.catalogReader
	}
	return c.apiClient
}

func (c *Controller) availabilityLoop(ctx context.Context, log logger.Logger) {
	ticker := time.NewTicker(availabilityInterval)
	defer ticker.Stop()
	for {
		if err := c.refreshAvailability(ctx, log); err != nil && ctx.Err() == nil {
			log.With("category", availabilityErrorCategory(err)).Error(ctx, "IAM catalog availability refresh failed; retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Availability refresh is independent of slow or failing upstream discovery.
func (c *Controller) refreshAvailability(ctx context.Context, log logger.Logger) error {
	return c.refresh(ctx, log, true)
}

func (c *Controller) refresh(ctx context.Context, log logger.Logger, reportChannels bool) error {
	c.availabilityMu.Lock()
	defer c.availabilityMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var versions privatev1.VersionList
	if err := c.reader().List(ctx, &versions); err != nil {
		return fmt.Errorf("list Versions for availability: %w", err)
	}
	var templates privatev1.WIFConfigTemplateList
	listErr := c.reader().List(ctx, &templates)
	if listErr == nil {
		if _, err := privatev1.SelectWIFConfigTemplate("0.0.0", templates.Items); err != nil {
			listErr = fmt.Errorf("IAM catalog boundary metadata is invalid")
		}
	}
	var failures []error
	for i := range versions.Items {
		v := &versions.Items[i]
		if err := c.reconcileAvailability(ctx, log, v, templates.Items, listErr); err != nil {
			failures = append(failures, err)
		}
	}
	if listErr != nil {
		failures = append(failures, fmt.Errorf("read IAM template snapshot"))
	}
	if reportChannels && c.upstreamFailure == "" {
		channels, err := c.channels(ctx)
		if err != nil {
			failures = append(failures, err)
		} else {
			desired := make(map[string]privatev1.VersionSpec, len(versions.Items))
			for _, v := range versions.Items {
				desired[v.Name] = v.Spec
			}
			c.reportDefaults(ctx, log, channels, desired, "")
		}
	}
	return errors.Join(failures...)
}

// Source failures remain owned by upstream synchronization. A template refresh
// can recover Version eligibility independently without erasing its diagnostic.
func (c *Controller) reportSourceDefaults(ctx context.Context, log logger.Logger, desired map[string]privatev1.VersionSpec, failure string) {
	c.availabilityMu.Lock()
	defer c.availabilityMu.Unlock()
	c.upstreamFailure = failure
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	channels, err := c.channels(ctx)
	if err != nil {
		log.Error(ctx, "Channel default refresh failed")
		return
	}
	c.reportDefaults(ctx, log, channels, desired, failure)
}

func (c *Controller) reconcileAvailability(ctx context.Context, log logger.Logger, v *privatev1.Version, templates []privatev1.WIFConfigTemplate, snapshotErr error) error {
	condition, err := c.availabilityDecision(ctx, v, templates, snapshotErr)
	if err != nil {
		return err
	}
	condition.ObservedGeneration = v.Generation
	before := v.DeepCopy()
	meta.SetStatusCondition(&v.Status.Conditions, condition)
	if equality.Semantic.DeepEqual(before.Status, v.Status) {
		return nil
	}
	if err := c.apiClient.Status().Update(ctx, v); err != nil {
		return fmt.Errorf("persist Version availability: %w", err)
	}
	log.With("reason", condition.Reason).With("status", condition.Status).Info(ctx, "Version availability transitioned")
	return nil
}

func (c *Controller) availabilityDecision(ctx context.Context, v *privatev1.Version, templates []privatev1.WIFConfigTemplate, snapshotErr error) (metav1.Condition, error) {
	condition := metav1.Condition{Type: privatev1.VersionAvailable, Status: metav1.ConditionFalse, ObservedGeneration: v.Generation, Reason: privatev1.WIFTemplateNotFound, Message: "Supply a matching same-major/minor IAM template boundary"}
	if snapshotErr != nil {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionUnknown, privatev1.WIFTemplateReadFailed, "IAM catalog cannot be read; availability will be retried"
		return condition, nil //nolint:nilerr // Dependency failure is persisted as Unknown, not discarded.
	}
	if v.Spec.ReleaseImage == "" || len(v.Spec.ChannelGroups) == 0 {
		condition.Reason, condition.Message = "ReleaseMetadataInvalid", "Release image and channel metadata are required"
		return condition, nil
	}
	selected, err := privatev1.SelectWIFConfigTemplate(v.Name, templates)
	if err != nil {
		condition.Reason, condition.Message = "ReleaseMetadataInvalid", "Release version metadata is invalid"
		return condition, nil //nolint:nilerr // Invalid metadata is a persisted negative condition.
	}
	if v.Spec.WIFConfigTemplateRef == nil && selected != nil {
		ref, err := privatev1.WIFConfigReference(selected)
		if err != nil {
			condition.Reason, condition.Message = privatev1.WIFTemplateIntegrityMismatch, "Selected IAM contract is invalid"
			return condition, nil //nolint:nilerr // Invalid contract is a persisted negative condition.
		}
		v.Spec.WIFConfigTemplateRef = ref
		if err := c.apiClient.Update(ctx, v); err != nil {
			return condition, fmt.Errorf("persist IAM association: %w", err)
		}
	}
	ref := v.Spec.WIFConfigTemplateRef
	if ref == nil {
		return condition, nil
	}
	if selected != nil && selected.Name != ref.Name {
		condition.Reason, condition.Message = "WIFConfigBoundaryChanged", "A newer effective IAM boundary requires platform review; the existing pin was not changed"
		return condition, nil
	}
	var pinned *privatev1.WIFConfigTemplate
	for i := range templates {
		if templates[i].Name == ref.Name {
			pinned = &templates[i]
			break
		}
	}
	reason, message := privatev1.CheckWIFConfigReference(ref, pinned)
	if reason == "" {
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "Available", "Release metadata and pinned IAM contract are available"
	} else {
		condition.Reason, condition.Message = reason, message
	}
	return condition, nil
}

func availabilityErrorCategory(err error) string {
	switch {
	case apierrors.IsConflict(err):
		return "Conflict"
	case apierrors.IsForbidden(err):
		return "Forbidden"
	case apierrors.IsUnauthorized(err):
		return "Unauthorized"
	case apierrors.IsTimeout(err):
		return "Timeout"
	default:
		return "ReadOrWriteFailed"
	}
}
