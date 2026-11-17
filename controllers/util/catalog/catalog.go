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
// Consumers choose whether it is terminal: customer-selected releases require a
// new spec attempt, while an automatic target can wait for catalog recovery.
func ResolutionRejected(condition *metav1.Condition, generation int64) bool {
	return condition != nil && condition.Status == metav1.ConditionFalse &&
		condition.ObservedGeneration == generation &&
		(condition.Reason == reasonVersionNotFound || condition.Reason == reasonChannelMismatch)
}

// UnavailableError means the catalog does not offer the requested release.
// Other errors indicate infrastructure failures and must remain retryable.
type UnavailableError struct {
	Reason  string
	Message string
}

func (e *UnavailableError) Error() string { return e.Message }

// Resolve returns the image published for version in channelGroup. Version
// resources are cluster-scoped and named after their release version.
func Resolve(ctx context.Context, reader client.Reader, version, channelGroup string) (string, error) {
	var release privatev1.Version
	if err := reader.Get(ctx, client.ObjectKey{Name: version}, &release); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &UnavailableError{
				Reason:  reasonVersionNotFound,
				Message: fmt.Sprintf("Version %s is not present in the Version catalog", version),
			}
		}
		return "", fmt.Errorf("get Version from catalog: %w", err)
	}
	if !slices.Contains(release.Spec.ChannelGroups, channelGroup) {
		return "", &UnavailableError{
			Reason:  reasonChannelMismatch,
			Message: fmt.Sprintf("Version %s is not published in channel group %s", version, channelGroup),
		}
	}
	return release.Spec.ReleaseImage, nil
}
