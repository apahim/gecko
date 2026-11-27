// Shared dispatch predicate for WIF-backed installation and upgrade work.
// One predicate is evaluated at every dispatch boundary: automatic, customer
// and critical-override approval, persisted preparation, and the final HC and
// NodePool Apply boundaries. A boundary that cannot evaluate the predicate
// fail-closes with zero external Apply; callers never fall back to a
// different target.
package catalog

import (
	"context"
	"fmt"
	"slices"

	channelutil "github.com/openshift-online/gecko/controllers/util/channel"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DispatchIntent is the exact immutable target of one dispatch: full release,
// payload, channel group and the pinned IAM contract. Receipts derived from it
// deliberately exclude the whole-object generation and derived Cincinnati
// channel, so unrelated status changes never invalidate a committed target.
type DispatchIntent struct {
	ReleaseVersion    string
	ReleaseImage      string
	ChannelGroup      string
	TemplateReference privatev1.WIFConfigTemplateReference
}

// Receipt materializes the intent as a durable dispatch receipt bound to the
// dispatched object and its parent installation.
func (i *DispatchIntent) Receipt(objectUID, clusterUID string) *privatev1.IAMDispatchReceipt {
	return &privatev1.IAMDispatchReceipt{
		ObjectUID:         objectUID,
		ClusterUID:        clusterUID,
		ReleaseVersion:    i.ReleaseVersion,
		ReleaseImage:      i.ReleaseImage,
		ChannelGroup:      i.ChannelGroup,
		TemplateReference: i.TemplateReference,
	}
}

// MatchesReceipt reports whether a receipt carries exactly this intent for the
// dispatched object and its parent installation. Receipts bound to different
// object or parent identities never match — mirroring the public requirements
// projection — so a recreated object or parent can never inherit a prior
// commitment.
func (i *DispatchIntent) MatchesReceipt(r *privatev1.IAMDispatchReceipt, objectUID, clusterUID string) bool {
	return r != nil &&
		r.ObjectUID == objectUID &&
		r.ClusterUID == clusterUID &&
		r.ReleaseVersion == i.ReleaseVersion &&
		r.ReleaseImage == i.ReleaseImage &&
		r.ChannelGroup == i.ChannelGroup &&
		r.TemplateReference == i.TemplateReference
}

// Committed reports whether the dispatch state already carries this exact
// intent as pending or dispatched for the given object and parent identities.
// A committed target may be redelivered after a restart, a revocation or
// upstream aging; it never authorizes a different target, and status-only
// generation changes never invalidate it.
func (i *DispatchIntent) Committed(state *privatev1.IAMDispatchState, objectUID, clusterUID string) bool {
	if state == nil {
		return false
	}
	return i.MatchesReceipt(state.Pending, objectUID, clusterUID) || i.MatchesReceipt(state.Dispatched, objectUID, clusterUID)
}

// DispatchEvaluation is the shared dispatch predicate evaluated against one
// release. Availability is the Version's own condition at its current
// generation; coverage is the Cluster's exact append-only application
// assertion of the pinned contract.
type DispatchEvaluation struct {
	// Intent is nil when the release is not in the catalog, is not in the
	// channel group, or carries no pinned IAM contract.
	Intent *DispatchIntent
	// Available reports the Version Available condition True at the current
	// generation, which implies a complete release and an intact verified pin.
	Available bool
	// AvailabilityReason and AvailabilityMessage describe why the release is
	// not currently available for new use (the Version's own condition reason
	// when one exists).
	AvailabilityReason  string
	AvailabilityMessage string
	// Covered reports whether the Cluster asserts the exact pinned contract.
	Covered bool
}

// ReleaseGroup returns the Cluster's effective channel group.
func ReleaseGroup(cluster *privatev1.Cluster) string {
	if cluster.Spec.Release.ChannelGroup != "" {
		return cluster.Spec.Release.ChannelGroup
	}
	return channelutil.DefaultChannelGroup
}

// EvaluateDispatch reads the Version fresh — cached resolution success cannot
// bypass this check — and evaluates availability and Cluster coverage for one
// release. An error is a retryable infrastructure failure and must mean zero
// external Apply; a missing Version or channel is a fail-closed evaluation,
// not an error.
func EvaluateDispatch(ctx context.Context, reader client.Reader, cluster *privatev1.Cluster, version, channelGroup string) (*DispatchEvaluation, error) {
	evaluation := &DispatchEvaluation{}
	var release privatev1.Version
	if err := reader.Get(ctx, client.ObjectKey{Name: version}, &release); err != nil {
		if apierrors.IsNotFound(err) {
			evaluation.AvailabilityReason, evaluation.AvailabilityMessage =
				reasonVersionNotFound, fmt.Sprintf("Version %s is not present in the Version catalog", version)
			return evaluation, nil
		}
		return nil, fmt.Errorf("get Version from catalog: %w", err)
	}
	if !slices.Contains(release.Spec.ChannelGroups, channelGroup) {
		evaluation.AvailabilityReason, evaluation.AvailabilityMessage =
			reasonChannelMismatch, fmt.Sprintf("Version %s is not published in channel group %s", version, channelGroup)
		return evaluation, nil
	}
	available := meta.FindStatusCondition(release.Status.Conditions, privatev1.VersionAvailable)
	if available == nil || available.Status != metav1.ConditionTrue || available.ObservedGeneration != release.Generation {
		evaluation.AvailabilityReason, evaluation.AvailabilityMessage =
			"VersionIAMUnavailable", fmt.Sprintf("Version %s availability is missing, false or stale", version)
		if available != nil {
			evaluation.AvailabilityReason, evaluation.AvailabilityMessage = available.Reason, available.Message
		}
	} else {
		evaluation.Available = true
	}
	pin := release.Spec.WIFConfigTemplateReference
	if pin == nil {
		// Available=True implies a pin; keep the guard fail-closed anyway.
		evaluation.Available = false
		if evaluation.AvailabilityReason == "" {
			evaluation.AvailabilityReason, evaluation.AvailabilityMessage =
				"TemplateReferenceMissing", fmt.Sprintf("Version %s has no pinned IAM template reference", version)
		}
		return evaluation, nil
	}
	evaluation.Intent = &DispatchIntent{
		ReleaseVersion:    version,
		ReleaseImage:      release.Spec.ReleaseImage,
		ChannelGroup:      channelGroup,
		TemplateReference: *pin,
	}
	evaluation.Covered = clusterAssertsContract(cluster, *pin)
	return evaluation, nil
}

// NewWorkPermitted reports whether new work may be dispatched against this
// evaluation, with the fail-closed reason and message when it may not.
// Missing cluster coverage requires a customer IAM update; an unavailable
// release contract requires platform action. Both are distinct from
// NoUpgradeAvailable: the target exists and is advertised.
func (e *DispatchEvaluation) NewWorkPermitted() (bool, string, string) {
	if e.Intent == nil || !e.Available {
		return false, privatev1.VersionIAMUnavailable, fmt.Sprintf(
			"The IAM contract of release %s is not currently available for new use: %s: %s",
			e.releaseVersion(), e.AvailabilityReason, e.AvailabilityMessage)
	}
	if !e.Covered {
		return false, privatev1.WIFConfigUpdateRequired, fmt.Sprintf(
			"The Cluster has not asserted the IAM contract %q pinned to release %s; the customer IAM update must be applied first",
			e.Intent.TemplateReference.Name, e.Intent.ReleaseVersion)
	}
	return true, "", ""
}

func (e *DispatchEvaluation) releaseVersion() string {
	if e.Intent != nil {
		return e.Intent.ReleaseVersion
	}
	return ""
}

// clusterAssertsContract reports whether the Cluster carries the exact
// append-only application assertion matching the pinned contract. A later
// numeric boundary or an equal executable digest alone does not cover a
// different name/UID reference, and revocation does not invalidate a
// retained assertion.
func clusterAssertsContract(cluster *privatev1.Cluster, pin privatev1.WIFConfigTemplateReference) bool {
	if cluster == nil || cluster.Spec.Platform.GCP == nil || cluster.Spec.Platform.GCP.IAM == nil {
		return false
	}
	return slices.ContainsFunc(cluster.Spec.Platform.GCP.IAM.TemplateApplications, func(app privatev1.WIFConfigTemplateReference) bool {
		return app == pin
	})
}
