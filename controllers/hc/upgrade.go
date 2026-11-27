package hc

import (
	"context"
	"fmt"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/util/catalog"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *Reconciler) preparationFailure(ctx context.Context, c *privatev1.Cluster, cause error) error {
	if privatev1.ClusterReleaseApproved(c) && meta.IsStatusConditionTrue(c.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted) && !privatev1.ClusterUpgradeSubmissionStarted(c) {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type: "ResourcesApplied", Status: metav1.ConditionFalse, Reason: "ManifestBuildFailed",
			Message: "The desired resources could not be prepared before dispatch", ObservedGeneration: c.Generation,
		})
		if err := r.client.Status().Update(ctx, c); err != nil {
			return fmt.Errorf("record preparation failure: %w", err)
		}
	}
	return fmt.Errorf("build manifests: %w", cause)
}

// applyResources records the dispatch boundary before any external write. An
// uncertain dispatch keeps the approval lock across errors and restarts.
//
// The IAM dispatch protocol layers onto the same boundary: the durable
// receipt for the exact target is persisted in the same status write, and new
// work additionally requires the shared dispatch predicate. A committed
// receipt authorizes only its own exact target, so a restart or a revocation
// after commitment may redeliver the same target but never dispatch a
// different one. Failed boundary persistence means zero Apply.
func (r *Reconciler) applyResources(ctx context.Context, c *privatev1.Cluster, managementCluster, groupKey string, manifests [][]byte) (*transport.Status, error) {
	upgrading := privatev1.ClusterReleaseApproved(c) && meta.IsStatusConditionTrue(c.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted) &&
		c.Status.HostedClusterResult != nil && c.Spec.Release.Version != c.Status.HostedClusterResult.Version

	vr := c.Status.VersionResolution
	evaluation, err := catalog.EvaluateDispatch(ctx, r.dispatchReader(), c, vr.ReleaseVersion, catalog.ReleaseGroup(c))
	if err != nil {
		// Zero external Apply when the dispatch predicate cannot be evaluated.
		return nil, fmt.Errorf("evaluate IAM dispatch: %w", err)
	}
	if evaluation.Intent != nil {
		// The receipt binds the payload actually dispatched, not a fresh
		// catalog read that may have advanced past the resolved image.
		evaluation.Intent.ReleaseImage = vr.ReleaseImage
	}

	// New work is an initial installation or an upgrade. Continued
	// reconciliation of the observed release is not gated, so a verification
	// revocation never recalls a committed rollout. An already committed
	// exact target may be redelivered after a restart, a revocation or
	// upstream aging; it never authorizes a different target.
	newWork := upgrading || c.Status.HostedClusterResult == nil || c.Status.HostedClusterResult.Version == ""
	blocked, iamConditionChanged := r.iamGate(c, evaluation, newWork)
	if blocked {
		if iamConditionChanged {
			if err := r.client.Status().Update(ctx, c); err != nil && !apierrors.IsConflict(err) {
				return nil, fmt.Errorf("record IAM dispatch gate: %w", err)
			}
		}
		// Fail-closed and level-triggered: the next reconcile retries.
		return &transport.Status{Stale: true}, nil
	}

	submitted := meta.FindStatusCondition(c.Status.Conditions, privatev1.ControlPlaneUpgradeSubmitted)
	accepted := meta.FindStatusCondition(c.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted)
	needSubmission := upgrading && (submitted == nil || submitted.Status == metav1.ConditionFalse || submitted.ObservedGeneration < accepted.ObservedGeneration)
	needReceipt := evaluation.Intent != nil && !evaluation.Intent.Committed(c.Status.IAMDispatch, string(c.UID), string(c.UID))

	if needSubmission || needReceipt || iamConditionChanged {
		if needSubmission {
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: privatev1.ControlPlaneUpgradeSubmitted, Status: metav1.ConditionUnknown,
				Reason: "Dispatching", Message: "The approved release is being dispatched", ObservedGeneration: c.Generation,
			})
		}
		if needReceipt {
			commitPendingIAMDispatch(c, evaluation.Intent.Receipt(string(c.UID), string(c.UID)))
		}
		privatev1.SyncClusterIAMRequirements(c)
		if err := r.client.Status().Update(ctx, c); err != nil && !apierrors.IsConflict(err) {
			return nil, fmt.Errorf("record dispatch boundary: %w", err)
		}
		return &transport.Status{Stale: true}, nil
	}

	status, err := r.transport.Apply(ctx, managementCluster, groupKey, manifests)
	if err != nil {
		return nil, err
	}
	if err := r.acknowledgeDispatch(ctx, c, upgrading, submitted, evaluation); err != nil {
		if apierrors.IsConflict(err) {
			return &transport.Status{Stale: true}, nil
		}
		return nil, fmt.Errorf("record dispatch acknowledgement: %w", err)
	}
	return status, nil
}

// iamGate applies the shared IAM dispatch predicate to new work and records
// the dispatcher-owned gate condition. Maintenance of the observed release
// and an already committed exact target are not re-gated. It returns whether
// dispatch is blocked and whether the condition changed.
func (r *Reconciler) iamGate(c *privatev1.Cluster, evaluation *catalog.DispatchEvaluation, newWork bool) (blocked, conditionChanged bool) {
	if !r.iamEnforcement {
		return false, false
	}
	status, reason, message := metav1.ConditionTrue, "IAMPermissionsReady",
		"The target release IAM contract is asserted for this Cluster"
	if newWork && (evaluation.Intent == nil || !evaluation.Intent.Committed(c.Status.IAMDispatch, string(c.UID), string(c.UID))) {
		if permitted, blockReason, blockMessage := evaluation.NewWorkPermitted(); !permitted {
			status, reason, message = metav1.ConditionFalse, blockReason, blockMessage
		}
	}
	return status == metav1.ConditionFalse, setIAMPermissionsCondition(c, status, reason, message)
}

// acknowledgeDispatch promotes the submission marker and the dispatch receipt
// once the transport has accepted the dispatch write. Dispatched records that
// the write was accepted, not that the transport has observed the application:
// read-back staleness delays only the observed conditions, never the receipt
// promotion. The committed pending intent is preserved: it remains the
// authority for same-target redelivery.
func (r *Reconciler) acknowledgeDispatch(ctx context.Context, c *privatev1.Cluster, upgrading bool, submitted *metav1.Condition, evaluation *catalog.DispatchEvaluation) error {
	needAck := (upgrading && submitted.Status != metav1.ConditionTrue) ||
		(evaluation.Intent != nil && !evaluation.Intent.MatchesReceipt(dispatchedReceipt(c.Status.IAMDispatch), string(c.UID), string(c.UID)))
	if !needAck {
		return nil
	}
	if upgrading && submitted.Status != metav1.ConditionTrue {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type: privatev1.ControlPlaneUpgradeSubmitted, Status: metav1.ConditionTrue,
			Reason: "Dispatched", Message: "The approved release was dispatched", ObservedGeneration: c.Generation,
		})
	}
	if evaluation.Intent != nil && !evaluation.Intent.MatchesReceipt(dispatchedReceipt(c.Status.IAMDispatch), string(c.UID), string(c.UID)) {
		commitDispatchedIAMDispatch(c, evaluation.Intent.Receipt(string(c.UID), string(c.UID)))
	}
	privatev1.SyncClusterIAMRequirements(c)
	return r.client.Status().Update(ctx, c)
}

// setIAMPermissionsCondition records the dispatcher-owned installation-target
// IAM gate. It returns whether the condition changed.
func setIAMPermissionsCondition(c *privatev1.Cluster, status metav1.ConditionStatus, reason, message string) bool {
	return meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type: privatev1.IAMPermissionsReady, Status: status, Reason: reason, Message: message, ObservedGeneration: c.Generation,
	})
}

// commitPendingIAMDispatch durably records the exact dispatch intent before
// any external write. A different unconfirmed pending target is replaced; the
// last dispatched target keeps its own separate authority.
func commitPendingIAMDispatch(c *privatev1.Cluster, receipt *privatev1.IAMDispatchReceipt) {
	if c.Status.IAMDispatch == nil {
		c.Status.IAMDispatch = &privatev1.IAMDispatchState{}
	}
	c.Status.IAMDispatch.Pending = receipt
}

// commitDispatchedIAMDispatch promotes the exact intent after a non-stale
// delivery. The committed pending intent is preserved: it remains the
// authority for same-target redelivery across restarts and revocations.
func commitDispatchedIAMDispatch(c *privatev1.Cluster, receipt *privatev1.IAMDispatchReceipt) {
	if c.Status.IAMDispatch == nil {
		c.Status.IAMDispatch = &privatev1.IAMDispatchState{Pending: receipt, Dispatched: receipt}
		return
	}
	if c.Status.IAMDispatch.Pending == nil {
		c.Status.IAMDispatch.Pending = receipt
	}
	c.Status.IAMDispatch.Dispatched = receipt
}

func dispatchedReceipt(state *privatev1.IAMDispatchState) *privatev1.IAMDispatchReceipt {
	if state == nil {
		return nil
	}
	return state.Dispatched
}
