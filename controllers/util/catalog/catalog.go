// Package catalog resolves release images from the platform Version catalog.
package catalog

import (
	"context"
	"fmt"
	"slices"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	reasonVersionNotFound = "VersionNotFoundInCatalog"
	reasonChannelMismatch = "VersionNotInChannelGroup"
)

// ResolutionRejected reports a catalog rejection for the current spec generation.
// It remains terminal until the user changes the spec or recreates the resource.
func ResolutionRejected(condition *metav1.Condition, generation int64) bool {
	return condition != nil && condition.Status == metav1.ConditionFalse &&
		condition.ObservedGeneration == generation &&
		(condition.Reason == reasonVersionNotFound || condition.Reason == reasonChannelMismatch)
}

// RecoverableReason classifies feedback for retry/unlock lifecycle only; it never
// authorizes dispatch or bypasses Available/coverage checks.
func RecoverableReason(reason string) bool {
	switch reason {
	case "VersionUnavailable", privatev1.WIFTemplateNotFound, privatev1.WIFTemplateUnverified, privatev1.WIFTemplateIntegrityMismatch, privatev1.WIFTemplateReadFailed, privatev1.WIFConfigUpdateRequired, "WIFConfigBoundaryChanged", "DispatchStateChanged", "DispatchIntentPending", "VersionResolutionOutdated", "VersionResolutionNotReady", "ReleaseVersionNotSet":
		return true
	default:
		return false
	}
}

// ResolutionMatches retains a payload only while it matches fresh catalog
// identity, unless an exact durable receipt already committed that target.
func ResolutionMatches(vr *privatev1.VersionResolutionResult, version, group string, v *privatev1.Version, committed bool) bool {
	if vr == nil || vr.ReleaseVersion != version || vr.ChannelGroup != group {
		return false
	}
	if committed {
		return true
	}
	return v != nil && vr.WIFConfigTemplateRef != nil && v.Spec.WIFConfigTemplateRef != nil && *vr.WIFConfigTemplateRef == *v.Spec.WIFConfigTemplateRef && vr.ReleaseImage == v.Spec.ReleaseImage
}

// UnavailableError means the catalog does not offer the requested release.
// Other errors indicate infrastructure failures and must remain retryable.
type UnavailableError struct {
	Reason  string
	Message string
}

// readError preserves retry/error classification without logging API/server prose.
type readError struct {
	message string
	cause   error
}

func (e *readError) Error() string { return e.message }
func (e *readError) Unwrap() error { return e.cause }

func (e *UnavailableError) Error() string { return e.Message }

// Resolve returns the image published for version in channelGroup. Version
// resources are cluster-scoped and named after their release version.
func Resolve(ctx context.Context, reader client.Reader, version, channelGroup string) (string, error) {
	release, err := Available(ctx, reader, version, channelGroup)
	if err != nil {
		return "", err
	}
	return release.Spec.ReleaseImage, nil
}

// Member discovers a candidate without authorizing its use. In particular,
// upgrade coordinators must not hide advertised-but-blocked candidates.
func Member(ctx context.Context, reader client.Reader, version, channelGroup string) (*privatev1.Version, error) {
	if version == "" {
		return nil, &UnavailableError{Reason: "ReleaseVersionNotSet", Message: "Waiting for a concrete release target"}
	}
	var release privatev1.Version
	if err := reader.Get(ctx, client.ObjectKey{Name: version}, &release); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &UnavailableError{
				Reason:  reasonVersionNotFound,
				Message: fmt.Sprintf("Version %s is not present in the Version catalog", version),
			}
		}
		return nil, &readError{message: "get Version from catalog failed", cause: err}
	}
	if !slices.Contains(release.Spec.ChannelGroups, channelGroup) {
		return nil, &UnavailableError{
			Reason:  reasonChannelMismatch,
			Message: fmt.Sprintf("Version %s is not published in channel group %s", version, channelGroup),
		}
	}
	return &release, nil
}

// Available requires current readiness AND a current intact verified pin. The
// final dispatch caller supplies the uncached API reader; this closes informer/
// status lag after verification revocation without re-running boundary selection.
func Available(ctx context.Context, reader client.Reader, version, channelGroup string) (*privatev1.Version, error) {
	v, err := Member(ctx, reader, version, channelGroup)
	if err != nil {
		return nil, err
	}
	if !privatev1.IsVersionAvailable(v) {
		return nil, &UnavailableError{Reason: "VersionUnavailable", Message: "Requested Version does not have current Available=True"}
	}
	ref := v.Spec.WIFConfigTemplateRef
	var template privatev1.WIFConfigTemplate
	if err := reader.Get(ctx, client.ObjectKey{Name: ref.Name}, &template); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &UnavailableError{Reason: privatev1.WIFTemplateNotFound, Message: "The pinned IAM template is missing"}
		}
		return nil, &readError{message: "read pinned IAM catalog contract failed", cause: err}
	}
	if reason, message := privatev1.CheckWIFConfigReference(ref, &template); reason != "" {
		return nil, &UnavailableError{Reason: reason, Message: message}
	}
	return v, nil
}

// Prerequisites authorizes only the exact required contract. Existing immutable
// application history preserves prior running contracts; the CLI must additionally
// retain their real cloud grants when preparing a new target.
func Prerequisites(ctx context.Context, reader client.Reader, cluster *privatev1.Cluster, version, channelGroup string) (*privatev1.Version, error) {
	v, err := Available(ctx, reader, version, channelGroup)
	if err != nil {
		return nil, err
	}
	if !privatev1.HasWIFConfigCoverage(cluster, v.Spec.WIFConfigTemplateRef) {
		ref := v.Spec.WIFConfigTemplateRef
		return nil, &UnavailableError{Reason: privatev1.WIFConfigUpdateRequired, Message: fmt.Sprintf("Apply and verify IAM template %s (%s) before dispatching release %s", ref.Name, ref.Digest, v.Name)}
	}
	return v, nil
}
