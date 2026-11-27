// Package nodepool implements the nodepool controller reconciler.
package nodepool

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/nodepool/manifest"
	"github.com/openshift-online/gecko/controllers/util/catalog"
	"github.com/openshift-online/gecko/controllers/util/constants"
	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	adapterName    = "nodepool-controller"
	requeuePending = 15 * time.Second
	requeueStable  = 5 * time.Minute
)

// Reconciler implements the nodepool controller reconciliation loop.
type Reconciler struct {
	transport transport.Client
	log       logger.Logger
	client    client.Client
	// iamEnforcement gates new dispatch on the shared IAM dispatch
	// predicate. Receipts are maintained even while enforcement is off, so
	// the staged rollout can validate the views before activation.
	iamEnforcement bool
	// iamCatalogReader is the uncached reader used to evaluate the shared IAM
	// dispatch predicate. A Version availability change must never be
	// evaluated from a stale informer cache at a dispatch boundary; it falls
	// back to the cached client when not configured.
	iamCatalogReader client.Reader
}

// New creates a new nodepool Reconciler.
func New(transport transport.Client, log logger.Logger, c client.Client) *Reconciler {
	return &Reconciler{
		transport: transport,
		log:       log,
		client:    c,
	}
}

// WithIAMDispatchEnforcement activates the IAM dispatch gate for new work.
func (r *Reconciler) WithIAMDispatchEnforcement(enabled bool) *Reconciler {
	r.iamEnforcement = enabled
	return r
}

// WithCatalogReader sets the reader used to evaluate the IAM dispatch
// predicate. Pass the manager's uncached API reader so a Version availability
// change is always evaluated fresh at the dispatch boundary.
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

// Reconcile runs the nodepool controller loop for one nodepool event.
// req.Namespace = project namespace, req.Name = nodepoolID.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	nodepoolID := req.Name
	log := r.log.With("nodepoolID", nodepoolID)

	// Read nodepool from cache.
	var np privatev1.NodePool
	if err := r.client.Get(ctx, req.NamespacedName, &np); err != nil {
		if apierrors.IsNotFound(err) {
			log.Infof(ctx, "nodepool %s not found, skipping", nodepoolID)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("nodepool reconciler: get nodepool: %w", err)
	}

	// Read parent cluster from cache using the cluster ID from spec.
	clusterID := np.Spec.ClusterID
	log = log.With("clusterID", clusterID)
	var cluster privatev1.Cluster
	clusterFound := true
	clusterKey := types.NamespacedName{Namespace: req.Namespace, Name: clusterID}
	if err := r.client.Get(ctx, clusterKey, &cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("nodepool reconciler: get cluster: %w", err)
		}
		clusterFound = false
	}

	// Handle deletion.
	if !np.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &np, &cluster, clusterFound, log)
	}

	// If cluster not found, nothing to reconcile.
	if !clusterFound {
		log.Infof(ctx, "cluster %s not found for nodepool %s, skipping", clusterID, nodepoolID)
		return reconcile.Result{}, nil
	}

	// Ensure finalizer is present.
	if !controllerutil.ContainsFinalizer(&np, constants.FinalizerNodePool) {
		controllerutil.AddFinalizer(&np, constants.FinalizerNodePool)
		if err := r.client.Update(ctx, &np); err != nil {
			return reconcile.Result{}, fmt.Errorf("nodepool reconciler: add finalizer: %w", err)
		}
		return reconcile.Result{}, nil // re-reconcile with finalizer in place
	}

	// Hold the nodepool back until placement and version resolution permit
	// installing the release currently approved for it.
	if result, waiting, err := r.waitForInstallation(ctx, &np, &cluster, log); waiting || err != nil {
		return result, err
	}

	// Extract nodepool GCP platform fields.
	var machineType, zone string
	var diskSizeGB int64
	var diskType string
	if gcp := np.Spec.Platform.GCP; gcp != nil {
		machineType = gcp.MachineType
		diskSizeGB = gcp.DiskSizeGB
		diskType = gcp.DiskType
		zone = gcp.Zone
	}
	if machineType == "" {
		machineType = manifest.DefaultMachineType
	}
	if diskSizeGB == 0 {
		diskSizeGB = manifest.DefaultDiskSizeGB
	}
	if diskType == "" {
		diskType = manifest.DefaultDiskType
	}

	// Extract cluster GCP platform fields.
	var gcpRegion, gcpSubnet string
	if gcp := cluster.Spec.Platform.GCP; gcp != nil {
		gcpRegion = gcp.Region
		gcpSubnet = gcp.Subnet
	}
	if zone == "" && gcpRegion != "" {
		zone = gcpRegion + "-a"
	}

	replicas := defaultReplicas
	if np.Spec.NodeCount != nil {
		replicas = *np.Spec.NodeCount
	}
	safeName := cluster.Spec.SafeName
	if safeName == "" {
		safeName = privatev1.DefaultSafeName(cluster.Name, cluster.UID)
	}

	manifests, err := manifest.Build(manifest.Input{
		NodePoolID:         nodepoolID,
		NodePoolName:       np.Name,
		NodePoolGeneration: np.Generation,
		ClusterID:          string(cluster.UID),
		ClusterName:        safeName,
		Replicas:           replicas,
		MachineType:        machineType,
		GCPRegion:          gcpRegion,
		Zone:               zone,
		GCPSubnet:          gcpSubnet,
		DiskSizeGB:         diskSizeGB,
		DiskType:           diskType,
		ReleaseImage:       np.Status.VersionResolution.ReleaseImage,
	})
	if err != nil {
		return reconcile.Result{}, r.preparationFailure(ctx, &np, err)
	}

	groupKey, err := transport.NodePoolGroupKey(req.Namespace, np.Spec.ClusterID, req.Name)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("build group key: %w", err)
	}

	managementCluster := cluster.Status.PlacementResult.ManagementClusterName

	mwStatus, err := r.applyResources(ctx, &np, &cluster, managementCluster, groupKey, manifests)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("nodepool reconciler: apply resources: %w", err)
	}

	// If status is stale, skip condition updates and requeue quickly.
	if mwStatus != nil && mwStatus.Stale {
		log.Infof(ctx, "nodepool reconciler: nodepool %s status is stale, requeueing after %s", nodepoolID, requeuePending)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	return r.reportStatus(ctx, &np, &cluster, mwStatus, log)
}

// waitForInstallation holds the nodepool back until cluster placement and
// version resolution permit installing the release currently approved for it.
// While a desired upgrade waits for approval, the observed release keeps
// reconciling so scaling and fresh health feedback are not blocked. It
// returns the waiting result when the request must requeue.
func (r *Reconciler) waitForInstallation(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster, log logger.Logger) (reconcile.Result, bool, error) {
	wait := func(reason, message string) (reconcile.Result, error) {
		if setWaitingNPConditions(np, reason, message) {
			if err := r.client.Status().Update(ctx, np); err != nil && !apierrors.IsConflict(err) {
				return reconcile.Result{}, fmt.Errorf("nodepool reconciler: update nodepool status: %w", err)
			}
		}
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	// Gate: cluster placement must be ready.
	if cluster.Status.PlacementResult == nil || cluster.Status.PlacementResult.ManagementClusterName == "" {
		log.Infof(ctx, "placement not ready for nodepool %s, requeueing after %s", np.Name, requeuePending)
		result, err := wait("PlacementNotReady", "Waiting for cluster placement to select a management cluster")
		return result, true, err
	}

	// Gate: nodepool VR must be ready.
	if np.Status.VersionResolution == nil || np.Status.VersionResolution.ReleaseVersion == "" {
		log.Infof(ctx, "nodepool VR not ready for nodepool %s, requeueing after %s", np.Name, requeuePending)
		result, err := wait("VersionResolutionNotReady", "Waiting for nodepool version resolution")
		return result, true, err
	}

	// Gate: VR must match the release currently permitted for installation.
	if version := installationVersion(np); np.Status.VersionResolution.ReleaseVersion != version {
		log.Infof(ctx, "nodepool VR version %q does not match spec version %q, requeueing after %s",
			np.Status.VersionResolution.ReleaseVersion, version, requeuePending)
		msg := fmt.Sprintf("VR version %q does not match spec version %q",
			np.Status.VersionResolution.ReleaseVersion, version)
		result, err := wait("VersionMismatch", msg)
		return result, true, err
	}
	return reconcile.Result{}, false, nil
}

// reportStatus writes the observed conditions with the public IAM
// requirements projection and returns the requeue for the nodepool's current
// health. Objects without receipts keep no view at all, which consumers must
// read as Incomplete.
func (r *Reconciler) reportStatus(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster, mwStatus *transport.Status, log logger.Logger) (reconcile.Result, error) {
	conditionsChanged := r.applyStatusConditions(np, string(cluster.UID), mwStatus)
	requirementsSynced := false
	if np.Status.IAMDispatch != nil || np.Status.IAMRequirements != nil {
		requirementsSynced = privatev1.SyncNodePoolIAMRequirements(np, cluster)
	}
	if conditionsChanged || requirementsSynced {
		if err := r.client.Status().Update(ctx, np); err != nil {
			if apierrors.IsConflict(err) {
				return reconcile.Result{}, nil
			}
			return reconcile.Result{}, fmt.Errorf("nodepool reconciler: update nodepool status: %w", err)
		}
	}

	if !meta.IsStatusConditionTrue(np.Status.Conditions, "NodePoolResourcesApplied") {
		log.Infof(ctx, "nodepool reconciler: nodepool %s resources not yet applied, requeueing after %s", np.Name, requeuePending)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	if !meta.IsStatusConditionTrue(cluster.Status.Conditions, "HostedClusterAvailable") {
		log.Infof(ctx, "nodepool reconciler: cluster %s not yet available, requeueing nodepool %s after %s", np.Spec.ClusterID, np.Name, requeuePending)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	if !meta.IsStatusConditionTrue(np.Status.Conditions, "NodePoolAvailable") {
		log.Infof(ctx, "nodepool reconciler: nodepool %s not yet available, requeueing after %s", np.Name, requeuePending)
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	log.Infof(ctx, "nodepool reconciler: nodepool %s reconciled, requeueing after %s", np.Name, requeueStable)
	return reconcile.Result{RequeueAfter: requeueStable}, nil
}

// handleDeletion cleans up management-cluster resources and removes the finalizer.
// Deletion flow:
// 1. Call transport.Delete to enqueue DeleteDesires (async)
// 2. Requeue, wait for kube-applier-gcp to process (poll GetDeleteStatus)
// 3. Once all DeleteDesires report Successful=True, cleanup DeleteDesires
// 4. Remove finalizer
func (r *Reconciler) handleDeletion(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster, clusterFound bool, log logger.Logger) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(np, constants.FinalizerNodePool) {
		return reconcile.Result{}, nil
	}

	// Only call transport.Delete if resources were applied to an MC.
	if clusterFound &&
		meta.FindStatusCondition(np.Status.Conditions, "NodePoolResourcesApplied") != nil &&
		cluster.Status.PlacementResult != nil && cluster.Status.PlacementResult.ManagementClusterName != "" {
		mcName := cluster.Status.PlacementResult.ManagementClusterName

		groupKey, err := transport.NodePoolGroupKey(np.Namespace, np.Spec.ClusterID, np.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("nodepool reconciler: build group key: %w", err)
		}

		// Check if deletion already in progress by querying delete status first.
		deleteStatus, err := r.transport.GetDeleteStatus(ctx, mcName, groupKey)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("nodepool reconciler: get delete status: %w", err)
		}

		if deleteStatus.TotalCount == 0 {
			// No DeleteDesires exist.
			if deleteStatus.ApplyDesiresCount > 0 {
				// ApplyDesires still present → deletion never started, call Delete().
				log.Infof(ctx, "nodepool reconciler: deleting resources for nodepool %s from %s", np.Name, mcName)
				if err := r.transport.Delete(ctx, mcName, groupKey); err != nil {
					return reconcile.Result{}, fmt.Errorf("nodepool reconciler: delete resources: %w", err)
				}
				if err := r.setDeletionProgressing(ctx, np, "DeletingResources",
					transport.DeleteInitiatedMessage(deleteStatus.ApplyDesiresCount)); err != nil {
					return reconcile.Result{}, err
				}
				log.Infof(ctx, "nodepool reconciler: delete initiated for nodepool %s, requeueing to poll status", np.Name)
				return reconcile.Result{RequeueAfter: requeuePending}, nil
			}
			// TotalCount=0 and ApplyDesiresCount=0 → deletion already complete (no-op), proceed to finalizer.
		}

		if !deleteStatus.AllSuccessful {
			// Deletion in progress — wait for completion.
			reason := "DeletionInProgress"
			if deleteStatus.FailedCount > 0 {
				reason = "DeletionBlocked"
			}
			if err := r.setDeletionProgressing(ctx, np, reason, transport.DeletePendingMessage(deleteStatus)); err != nil {
				return reconcile.Result{}, err
			}
			log.Infof(ctx, "nodepool reconciler: deletion in progress for nodepool %s (%d/%d pending), requeueing",
				np.Name, deleteStatus.PendingCount, deleteStatus.TotalCount)
			return reconcile.Result{RequeueAfter: requeuePending}, nil
		}

		// All DeleteDesires successful — cleanup before removing finalizer.
		log.Infof(ctx, "nodepool reconciler: deletion complete for nodepool %s, cleaning up %d DeleteDesires",
			np.Name, deleteStatus.TotalCount)
		if err := r.transport.CleanupDeleteDesires(ctx, mcName, groupKey); err != nil {
			return reconcile.Result{}, fmt.Errorf("nodepool reconciler: cleanup delete desires: %w", err)
		}
	} else {
		log.Infof(ctx, "nodepool reconciler: parent cluster not available for nodepool %s, skipping transport cleanup", np.Name)
	}

	controllerutil.RemoveFinalizer(np, constants.FinalizerNodePool)
	if err := r.client.Update(ctx, np); err != nil {
		return reconcile.Result{}, fmt.Errorf("nodepool reconciler: remove finalizer: %w", err)
	}

	log.Infof(ctx, "nodepool reconciler: finalizer removed for nodepool %s", np.Name)
	return reconcile.Result{}, nil
}

// setDeletionProgressing records the DeletionProgressing condition on a
// nodepool being deleted. The condition status is always True while deletion
// is underway; the reason encodes the phase (DeletingResources,
// DeletionInProgress, DeletionBlocked) and the message summarizes what is
// holding the deletion up. Writes are change-gated and conflicts are
// tolerated, mirroring the status-update pattern in Reconcile.
func (r *Reconciler) setDeletionProgressing(ctx context.Context, np *privatev1.NodePool, reason, message string) error {
	if !meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "DeletionProgressing",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: np.Generation,
	}) {
		return nil
	}
	if err := r.client.Status().Update(ctx, np); err != nil && !apierrors.IsConflict(err) {
		return fmt.Errorf("nodepool reconciler: record deletion progress: %w", err)
	}
	return nil
}

// preparationFailure reports a confirmed failure before dispatch. The upgrade
// coordinator can then release its lock without implying a HyperShift rollback.
func (r *Reconciler) preparationFailure(ctx context.Context, np *privatev1.NodePool, cause error) error {
	if privatev1.NodePoolUpgradeActive(np) && !privatev1.NodePoolUpgradeSubmissionStarted(np) {
		before := np.DeepCopy()
		meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
			Type: "NodePoolResourcesApplied", Status: metav1.ConditionFalse, Reason: "ManifestBuildFailed",
			Message: "The desired NodePool resources could not be prepared before dispatch", ObservedGeneration: np.Generation,
		})
		if err := r.client.Status().Patch(ctx, np, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("record NodePool preparation failure: %w", err)
		}
	}
	return fmt.Errorf("nodepool reconciler: build manifests: %w", cause)
}

// applyResources persists the dispatch boundary before transport can write the
// target. An unknown result remains locked across errors or restarts; recovery
// after dispatch is intentionally outside the pre-submission cancellation flow.
//
// The IAM dispatch protocol layers onto the same boundary: the durable receipt
// for the exact target is persisted in the same status write, and new work
// additionally requires the shared dispatch predicate evaluated against the
// parent Cluster's application assertions. A committed receipt authorizes
// only its own exact target; failed boundary persistence means zero Apply.
func (r *Reconciler) applyResources(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster, managementCluster, groupKey string, manifests [][]byte) (*transport.Status, error) {
	upgrading := privatev1.NodePoolUpgradeActive(np) && np.Spec.Release.Version != np.Status.Version

	vr := np.Status.VersionResolution
	evaluation, err := catalog.EvaluateDispatch(ctx, r.dispatchReader(), cluster, vr.ReleaseVersion, catalog.ReleaseGroup(cluster))
	if err != nil {
		// Zero external Apply when the dispatch predicate cannot be evaluated.
		return nil, fmt.Errorf("nodepool reconciler: evaluate IAM dispatch: %w", err)
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
	newWork := upgrading || np.Status.Version == ""
	// Snapshot before the gate: iamGate mutates np.Status.Conditions in place,
	// so a MergeFrom patch must diff against the pre-gate object or the gate's
	// own condition change produces an empty patch and is never persisted.
	before := np.DeepCopy()
	blocked, iamConditionChanged := r.iamGate(np, cluster, evaluation, newWork)
	if blocked {
		if iamConditionChanged {
			if err := r.client.Status().Patch(ctx, np, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsConflict(err) {
				return nil, fmt.Errorf("nodepool reconciler: record IAM dispatch gate: %w", err)
			}
		}
		// Fail-closed and level-triggered: the next reconcile retries.
		return &transport.Status{Stale: true}, nil
	}

	submitted := meta.FindStatusCondition(np.Status.Conditions, privatev1.NodePoolUpgradeSubmitted)
	needSubmission := upgrading && (submitted == nil || submitted.Status == metav1.ConditionFalse)
	needReceipt := evaluation.Intent != nil && !evaluation.Intent.Committed(np.Status.IAMDispatch, string(np.UID), string(cluster.UID))

	if needSubmission || needReceipt || iamConditionChanged {
		if needSubmission {
			meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
				Type: privatev1.NodePoolUpgradeSubmitted, Status: metav1.ConditionUnknown,
				Reason: "Dispatching", Message: "The desired release is being dispatched to the management cluster",
				ObservedGeneration: np.Generation,
			})
		}
		if needReceipt {
			commitPendingIAMDispatch(np, evaluation.Intent.Receipt(string(np.UID), string(cluster.UID)))
		}
		privatev1.SyncNodePoolIAMRequirements(np, cluster)
		if err := r.client.Status().Patch(ctx, np, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsConflict(err) {
			return nil, fmt.Errorf("nodepool reconciler: persist dispatch boundary: %w", err)
		}
		return &transport.Status{Stale: true}, nil
	}

	status, err := r.transport.Apply(ctx, managementCluster, groupKey, manifests)
	if err != nil {
		return nil, err
	}
	if err := r.acknowledgeDispatch(ctx, np, cluster, upgrading, submitted, evaluation); err != nil {
		if apierrors.IsConflict(err) {
			return &transport.Status{Stale: true}, nil
		}
		return nil, fmt.Errorf("nodepool reconciler: persist dispatch acknowledgement: %w", err)
	}
	return status, nil
}

// iamGate applies the shared IAM dispatch predicate to new work and records
// the dispatcher-owned gate condition. Maintenance of the observed release
// and an already committed exact target are not re-gated. It returns whether
// dispatch is blocked and whether the condition changed.
// iamGate applies the shared IAM dispatch predicate to new work and records
// the dispatcher-owned gate condition. Maintenance of the observed release
// and an already committed exact target are not re-gated. It returns whether
// dispatch is blocked and whether the condition changed.
func (r *Reconciler) iamGate(np *privatev1.NodePool, cluster *privatev1.Cluster, evaluation *catalog.DispatchEvaluation, newWork bool) (blocked, conditionChanged bool) {
	if !r.iamEnforcement {
		return false, false
	}
	status, reason, message := metav1.ConditionTrue, "IAMPermissionsReady",
		"The target release IAM contract is asserted for the parent Cluster"
	if newWork && (evaluation.Intent == nil || !evaluation.Intent.Committed(np.Status.IAMDispatch, string(np.UID), string(cluster.UID))) {
		if permitted, blockReason, blockMessage := evaluation.NewWorkPermitted(); !permitted {
			status, reason, message = metav1.ConditionFalse, blockReason, blockMessage
		}
	}
	return status == metav1.ConditionFalse, setIAMPermissionsCondition(np, status, reason, message)
}

// acknowledgeDispatch promotes the submission marker and the dispatch receipt
// once the transport has accepted the dispatch write. Dispatched records that
// the write was accepted, not that the transport has observed the application:
// read-back staleness delays only the observed conditions, never the receipt
// promotion. The committed pending intent is preserved: it remains the
// authority for same-target redelivery.
func (r *Reconciler) acknowledgeDispatch(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster, upgrading bool, submitted *metav1.Condition, evaluation *catalog.DispatchEvaluation) error {
	needAck := (upgrading && submitted.Status != metav1.ConditionTrue) ||
		(evaluation.Intent != nil && !evaluation.Intent.MatchesReceipt(dispatchedReceipt(np.Status.IAMDispatch), string(np.UID), string(cluster.UID)))
	if !needAck {
		return nil
	}
	before := np.DeepCopy()
	if upgrading && submitted.Status != metav1.ConditionTrue {
		meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
			Type: privatev1.NodePoolUpgradeSubmitted, Status: metav1.ConditionTrue,
			Reason: "Dispatched", Message: "The desired release was dispatched to the management cluster",
			ObservedGeneration: np.Generation,
		})
	}
	if evaluation.Intent != nil && !evaluation.Intent.MatchesReceipt(dispatchedReceipt(np.Status.IAMDispatch), string(np.UID), string(cluster.UID)) {
		commitDispatchedIAMDispatch(np, evaluation.Intent.Receipt(string(np.UID), string(cluster.UID)))
	}
	privatev1.SyncNodePoolIAMRequirements(np, cluster)
	return r.client.Status().Patch(ctx, np, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// setIAMPermissionsCondition records the dispatcher-owned installation-target
// IAM gate. It returns whether the condition changed.
func setIAMPermissionsCondition(np *privatev1.NodePool, status metav1.ConditionStatus, reason, message string) bool {
	return meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type: privatev1.NodePoolIAMPermissionsReady, Status: status, Reason: reason, Message: message, ObservedGeneration: np.Generation,
	})
}

// commitPendingIAMDispatch durably records the exact dispatch intent before
// any external write. A different unconfirmed pending target is replaced; the
// last dispatched target keeps its own separate authority.
func commitPendingIAMDispatch(np *privatev1.NodePool, receipt *privatev1.IAMDispatchReceipt) {
	if np.Status.IAMDispatch == nil {
		np.Status.IAMDispatch = &privatev1.IAMDispatchState{}
	}
	np.Status.IAMDispatch.Pending = receipt
}

// commitDispatchedIAMDispatch promotes the exact intent after a non-stale
// delivery. The committed pending intent is preserved: it remains the
// authority for same-target redelivery across restarts and revocations.
func commitDispatchedIAMDispatch(np *privatev1.NodePool, receipt *privatev1.IAMDispatchReceipt) {
	if np.Status.IAMDispatch == nil {
		np.Status.IAMDispatch = &privatev1.IAMDispatchState{Pending: receipt, Dispatched: receipt}
		return
	}
	if np.Status.IAMDispatch.Pending == nil {
		np.Status.IAMDispatch.Pending = receipt
	}
	np.Status.IAMDispatch.Dispatched = receipt
}

func dispatchedReceipt(state *privatev1.IAMDispatchState) *privatev1.IAMDispatchReceipt {
	if state == nil {
		return nil
	}
	return state.Dispatched
}

// installationVersion keeps unapproved release intent out of applied manifests.
func installationVersion(np *privatev1.NodePool) string {
	if !privatev1.NodePoolReleaseApproved(np) {
		return np.Status.Version
	}
	return np.Spec.Release.Version
}

// setWaitingNPConditions sets NodePoolResourcesApplied and NodePoolAvailable to Unknown.
// Returns true if either condition changed.
func setWaitingNPConditions(np *privatev1.NodePool, reason, message string) bool {
	gen := np.Generation
	a := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolResourcesApplied",
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
	})
	b := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolAvailable",
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
	})
	c := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolProgressing",
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gen,
	})
	return a || b || c
}

// applyStatusConditions derives conditions from the resource status and writes them to the nodepool.
// Returns true if any condition changed.
func (r *Reconciler) applyStatusConditions(np *privatev1.NodePool, clusterID string, mwStatus *transport.Status) bool {
	gen := np.Generation

	if mwStatus == nil {
		a := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
			Type:               "NodePoolResourcesApplied",
			Status:             metav1.ConditionFalse,
			Reason:             "ResourcesNotFound",
			Message:            "Resources have not been applied yet",
			ObservedGeneration: gen,
		})
		b := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
			Type:               "NodePoolAvailable",
			Status:             metav1.ConditionFalse,
			Reason:             "ResourcesNotFound",
			Message:            "Resources have not been applied yet",
			ObservedGeneration: gen,
		})
		c := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
			Type:               "NodePoolProgressing",
			Status:             metav1.ConditionFalse,
			Reason:             "AsExpected",
			Message:            "Resources have not been applied yet",
			ObservedGeneration: gen,
		})
		return a || b || c
	}

	// Extract conditions from top-level applied conditions.
	appliedStatus := metav1.ConditionStatus("False")
	appliedReason := "Unknown"
	appliedMessage := ""
	for _, c := range mwStatus.Conditions {
		if c.Type == "Applied" {
			appliedStatus = c.Status
			appliedReason = c.Reason
			appliedMessage = c.Message
			break
		}
	}

	// Extract resource status by NodePool resource identity key.
	npKey := transport.ResourceKey(constants.HyperShiftGroup, constants.HyperShiftVersion, "nodepools",
		fmt.Sprintf("clusters-%s", clusterID), np.Name)
	// rs is nil if npKey not in map; nil-safe below.
	rs := mwStatus.ResourceStatuses[npKey]
	versionChanged := false
	if rs != nil && rs["version"] != "" && np.Status.Version != rs["version"] {
		np.Status.Version = rs["version"]
		versionChanged = true
	}
	availableStatus := "False"
	allNodesHealthy := "False"
	// Empty string for progressing conditions: != "True" captures absent fields.
	allMachinesReady := ""
	updatingConfig := ""
	updatingVersion := ""
	if rs != nil {
		if v, ok := rs["readyCondition"]; ok {
			availableStatus = v
		}
		if v, ok := rs["allNodesHealthyCondition"]; ok {
			allNodesHealthy = v
		}
		if v, ok := rs["allMachinesReadyCondition"]; ok {
			allMachinesReady = v
		}
		if v, ok := rs["updatingConfigCondition"]; ok {
			updatingConfig = v
		}
		if v, ok := rs["updatingVersionCondition"]; ok {
			updatingVersion = v
		}
	}

	// HyperShift condition messages are surfaced verbatim on the matching Gecko
	// conditions when they are not healthy. Reads are nil-safe: a nil rs or an absent
	// key (feedback extracted before messages were propagated) both yield "".
	availableMessage := rs["readyConditionMessage"]
	healthMessage := rs["allNodesHealthyConditionMessage"]
	machinesMessage := rs["allMachinesReadyConditionMessage"]
	updatingConfigMessage := rs["updatingConfigConditionMessage"]
	updatingVersionMessage := rs["updatingVersionConditionMessage"]

	healthStatus := metav1.ConditionFalse
	healthReason := "NodePoolNotHealthy"
	if allNodesHealthy == "True" {
		healthStatus = metav1.ConditionTrue
		healthReason = "NodePoolHealthy"
		healthMessage = "" // messages only on problems, mirroring the HC controller
	}

	availableReason := "NodePoolNotAvailable"
	if availableStatus == "True" {
		availableReason = "NodePoolAvailable"
		availableMessage = ""
	}

	a := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolResourcesApplied",
		Status:             appliedStatus,
		Reason:             appliedReason,
		Message:            appliedMessage,
		ObservedGeneration: gen,
	})
	b := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolAvailable",
		Status:             metav1.ConditionStatus(availableStatus),
		Reason:             availableReason,
		Message:            availableMessage,
		ObservedGeneration: gen,
	})
	c := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolHealthy",
		Status:             healthStatus,
		Reason:             healthReason,
		Message:            healthMessage,
		ObservedGeneration: gen,
	})

	progressingStatus := metav1.ConditionFalse
	progressingReason := "AsExpected"
	progressingMessage := ""
	switch {
	case allMachinesReady != "True":
		progressingStatus = metav1.ConditionTrue
		progressingReason = "MachinesNotReady"
		progressingMessage = machinesMessage
	case updatingConfig == "True":
		progressingStatus = metav1.ConditionTrue
		progressingReason = "UpdatingConfig"
		progressingMessage = updatingConfigMessage
	case updatingVersion == "True":
		progressingStatus = metav1.ConditionTrue
		progressingReason = "UpdatingVersion"
		progressingMessage = updatingVersionMessage
	}

	d := meta.SetStatusCondition(&np.Status.Conditions, metav1.Condition{
		Type:               "NodePoolProgressing",
		Status:             progressingStatus,
		Reason:             progressingReason,
		Message:            progressingMessage,
		ObservedGeneration: gen,
	})
	return a || b || c || d || versionChanged
}

// defaultReplicas is the hardcoded default for this POC.
const defaultReplicas = int32(1)
