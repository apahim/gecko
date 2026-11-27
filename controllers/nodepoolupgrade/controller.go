// Package nodepoolupgrade coordinates customer-requested NodePool upgrades.
package nodepoolupgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openshift-online/gecko/controllers/util/catalog"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilversion "k8s.io/apimachinery/pkg/util/version"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	waitInterval         = 30 * time.Second
	retryInterval        = time.Second
	maxScheduleWait      = time.Hour
	conditionScheduled   = "NodePoolUpgradeScheduled"
	conditionProgressing = "NodePoolUpgradeProgressing"
	conditionDegraded    = "NodePoolUpgradeDegraded"
)

// Reconciler owns upgrade scheduling and status. Installation controllers retain
// ownership of release resolution, resource application, and observed pool health.
type Reconciler struct {
	client client.Client
	now    func() time.Time
	// iamEnforcement gates scheduling on the shared IAM dispatch predicate.
	// The nodepool installer's Apply boundary re-checks it independently.
	iamEnforcement bool
	// iamCatalogReader is the uncached reader used to evaluate the shared IAM
	// dispatch predicate. A Version availability change must never be
	// evaluated from a stale informer cache at a scheduling boundary; it
	// falls back to the cached client when not configured.
	iamCatalogReader client.Reader
}

func NewReconciler(c client.Client) *Reconciler { return &Reconciler{client: c, now: time.Now} }

// WithIAMDispatchEnforcement gates scheduling on the IAM dispatch predicate.
func (r *Reconciler) WithIAMDispatchEnforcement(enabled bool) *Reconciler {
	r.iamEnforcement = enabled
	return r
}

// WithCatalogReader sets the reader used to evaluate the IAM dispatch
// predicate. Pass the manager's uncached API reader so a Version availability
// change is always evaluated fresh at the scheduling boundary.
func (r *Reconciler) WithCatalogReader(reader client.Reader) *Reconciler {
	r.iamCatalogReader = reader
	return r
}

// dispatchReader returns the reader for the shared IAM dispatch predicate,
// preferring the uncached reader over the cached client.
func (r *Reconciler) dispatchReader() client.Reader {
	if r.iamCatalogReader != nil {
		return r.iamCatalogReader
	}
	return r.client
}

func (r *Reconciler) Reconcile(ctx context.Context, key reconcile.Request) (reconcile.Result, error) {
	var pool privatev1.NodePool
	if err := r.client.Get(ctx, key.NamespacedName, &pool); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	before := pool.DeepCopy()
	if pool.Status.Version == "" {
		return reconcile.Result{}, nil // Initial installation is unchanged.
	}
	if privatev1.NodePoolUpgradeActive(&pool) {
		return r.observe(ctx, before, &pool)
	}
	if pool.Spec.Release.Version == pool.Status.Version {
		progressing := meta.FindStatusCondition(pool.Status.Conditions, conditionProgressing)
		if progressing != nil && progressing.Reason == "UpgradeCompleted" {
			return reconcile.Result{}, nil
		}
		if progressing != nil || meta.FindStatusCondition(pool.Status.Conditions, conditionScheduled) != nil {
			conditions(&pool, metav1.ConditionFalse, metav1.ConditionFalse, "UpgradeCanceled", "No version upgrade is requested")
			setCondition(&pool, privatev1.NodePoolUpgradeSubmitted, metav1.ConditionFalse, "NotSubmitted", "No upgrade is pending submission")
			return r.persist(ctx, before, &pool, 0)
		}
		return reconcile.Result{}, nil
	}
	// A rejected configuration remains visible until the desired configuration
	// changes. Conditions describe the current spec, not a history of attempts.
	if degraded := meta.FindStatusCondition(pool.Status.Conditions, conditionDegraded); degraded != nil && degraded.Status == metav1.ConditionTrue && degraded.ObservedGeneration == pool.Generation {
		return reconcile.Result{}, nil
	}
	setCondition(&pool, privatev1.NodePoolUpgradeSubmitted, metav1.ConditionFalse, "NotSubmitted", "The desired upgrade has not been dispatched")
	if start := pool.Spec.Release.UpgradeStartTime; start != nil && r.now().Before(start.Time) {
		remaining := start.Sub(r.now())
		if remaining > maxScheduleWait {
			remaining = maxScheduleWait
		}
		conditions(&pool, metav1.ConditionTrue, metav1.ConditionFalse, "WaitingForStartTime", "Waiting for spec.release.upgradeStartTime")
		return r.persist(ctx, before, &pool, remaining)
	}
	if !rolloutComplete(&pool) {
		conditions(&pool, metav1.ConditionFalse, metav1.ConditionFalse, "WaitingForNodePool", "Waiting for fresh, healthy NodePool feedback")
		return r.persist(ctx, before, &pool, waitInterval)
	}
	if err := r.validate(ctx, &pool, pool.Spec.Release.Version); err != nil {
		return r.validationResult(ctx, before, &pool, err)
	}
	// Optimistic status persistence orders approval against concurrent spec
	// updates. Once accepted, admission locks the release settings until a
	// confirmed pre-submission failure or healthy completion.
	conditions(&pool, metav1.ConditionFalse, metav1.ConditionTrue, "UpgradeAccepted", "The desired release is approved for rollout")
	return r.persist(ctx, before, &pool, retryInterval)
}

func (r *Reconciler) observe(ctx context.Context, before, pool *privatev1.NodePool) (reconcile.Result, error) {
	if !privatev1.NodePoolUpgradeSubmissionStarted(pool) {
		if c := meta.FindStatusCondition(pool.Status.Conditions, "NodePoolVersionResolved"); c != nil && c.ObservedGeneration == pool.Generation && c.Status == metav1.ConditionFalse {
			return r.fail(ctx, before, pool, "VersionResolutionFailed", "The desired release could not be resolved before submission")
		}
		if c := meta.FindStatusCondition(pool.Status.Conditions, "NodePoolResourcesApplied"); c != nil && c.ObservedGeneration == pool.Generation && c.Status == metav1.ConditionFalse && c.Reason == "ManifestBuildFailed" {
			return r.fail(ctx, before, pool, "PreparationFailed", "The desired release could not be prepared for submission")
		}
	}
	if pool.Status.Version == pool.Spec.Release.Version && rolloutComplete(pool) {
		conditions(pool, metav1.ConditionFalse, metav1.ConditionFalse, "UpgradeCompleted", "The NodePool reached the desired version and is healthy")
		return r.persist(ctx, before, pool, 0)
	}
	if current, err := utilversion.ParseSemantic(pool.Status.Version); err == nil {
		if target, err := utilversion.ParseSemantic(pool.Spec.Release.Version); err == nil && target.LessThan(current) {
			if !privatev1.NodePoolUpgradeSubmissionStarted(pool) {
				return r.fail(ctx, before, pool, "TargetSuperseded", "The observed NodePool version is newer than the desired version")
			}
		}
	}
	conditions(pool, metav1.ConditionFalse, metav1.ConditionTrue, "WaitingForNodePool", "Waiting for the desired version and a healthy rollout")
	return r.persist(ctx, before, pool, waitInterval)
}

func rolloutComplete(pool *privatev1.NodePool) bool {
	for _, expected := range []struct {
		name   string
		status metav1.ConditionStatus
	}{
		{"NodePoolAvailable", metav1.ConditionTrue}, {"NodePoolHealthy", metav1.ConditionTrue}, {"NodePoolProgressing", metav1.ConditionFalse},
	} {
		c := meta.FindStatusCondition(pool.Status.Conditions, expected.name)
		if c == nil || c.Status != expected.status || c.ObservedGeneration != pool.Generation {
			return false
		}
	}
	return true
}

type validationError struct{ message string }

func (e *validationError) Error() string { return e.message }

// iamBlockError reports the shared IAM dispatch predicate blocking an
// otherwise valid target. Unlike a validation error it is not degraded: the
// target is correct and waiting on an IAM prerequisite.
type iamBlockError struct {
	reason  string
	message string
}

func (e *iamBlockError) Error() string { return e.message }

func (r *Reconciler) validate(ctx context.Context, pool *privatev1.NodePool, target string) error {
	var cluster privatev1.Cluster
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Spec.ClusterID}, &cluster); err != nil {
		return fmt.Errorf("read Cluster for upgrade: %w", err)
	}
	var release privatev1.Version
	if err := r.client.Get(ctx, types.NamespacedName{Name: target}, &release); err != nil {
		return fmt.Errorf("read Version for upgrade: %w", err)
	}
	if err := privatev1.ValidateNodePoolUpgradeTarget(pool, &cluster, &release, target); err != nil {
		return &validationError{err.Error()}
	}
	// One shared dispatch predicate at the scheduling boundary; the nodepool
	// installer's Apply boundary re-checks it independently.
	if r.iamEnforcement {
		evaluation, err := catalog.EvaluateDispatch(ctx, r.dispatchReader(), &cluster, target, catalog.ReleaseGroup(&cluster))
		if err != nil {
			return err
		}
		if permitted, reason, message := evaluation.NewWorkPermitted(); !permitted {
			return &iamBlockError{reason: reason, message: message}
		}
	}
	return nil
}

func (r *Reconciler) validationResult(ctx context.Context, before, pool *privatev1.NodePool, err error) (reconcile.Result, error) {
	var invalid *validationError
	if errors.As(err, &invalid) {
		return r.fail(ctx, before, pool, "TargetRejected", invalid.message)
	}
	var blocked *iamBlockError
	if errors.As(err, &blocked) {
		// The target stays selected and waiting: an IAM prerequisite, not a
		// rejected configuration, so it is not degraded.
		conditions(pool, metav1.ConditionFalse, metav1.ConditionFalse, blocked.reason, blocked.message)
		setCondition(pool, privatev1.NodePoolUpgradeSubmitted, metav1.ConditionFalse, "NotSubmitted", "The desired upgrade has not been dispatched")
		return r.persist(ctx, before, pool, waitInterval)
	}
	return reconcile.Result{}, err
}

func (r *Reconciler) fail(ctx context.Context, before, pool *privatev1.NodePool, reason, message string) (reconcile.Result, error) {
	conditions(pool, metav1.ConditionFalse, metav1.ConditionFalse, reason, message)
	setCondition(pool, conditionDegraded, metav1.ConditionTrue, reason, message)
	return r.persist(ctx, before, pool, 0)
}

func conditions(pool *privatev1.NodePool, scheduled, progressing metav1.ConditionStatus, reason, message string) {
	setCondition(pool, conditionScheduled, scheduled, reason, message)
	setCondition(pool, conditionProgressing, progressing, reason, message)
	setCondition(pool, conditionDegraded, metav1.ConditionFalse, "AsExpected", "")
}

func setCondition(pool *privatev1.NodePool, typ string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: message, ObservedGeneration: pool.Generation,
	})
}

func (r *Reconciler) persist(ctx context.Context, before, pool *privatev1.NodePool, after time.Duration) (reconcile.Result, error) {
	if !apiequality.Semantic.DeepEqual(before.Status, pool.Status) {
		if err := r.client.Status().Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return reconcile.Result{RequeueAfter: retryInterval}, nil
			}
			return reconcile.Result{}, fmt.Errorf("patch NodePool upgrade status: %w", err)
		}
	}
	return reconcile.Result{RequeueAfter: after}, nil
}
