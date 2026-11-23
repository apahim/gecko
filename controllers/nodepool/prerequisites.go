package nodepool

import (
	"context"
	"fmt"

	"github.com/openshift-online/gecko/controllers/util/constants"
	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// prepareNodePool persists the finalizer and checks provisioning prerequisites.
// Deletion is handled by the caller before any of these gates.
func (r *Reconciler) prepareNodePool(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster, log logger.Logger) (bool, reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(np, constants.FinalizerNodePool) {
		controllerutil.AddFinalizer(np, constants.FinalizerNodePool)
		if err := r.client.Update(ctx, np); err != nil {
			return false, reconcile.Result{}, fmt.Errorf("nodepool reconciler: add finalizer: %w", err)
		}
		return false, reconcile.Result{}, nil // re-reconcile with finalizer in place
	}

	if cluster.Status.PlacementResult == nil || cluster.Status.PlacementResult.ManagementClusterName == "" {
		if setWaitingNPConditions(np, "PlacementNotReady", "Waiting for cluster placement to select a management cluster") {
			if err := r.client.Status().Update(ctx, np); err != nil && !apierrors.IsConflict(err) {
				return false, reconcile.Result{}, fmt.Errorf("nodepool reconciler: update nodepool status: %w", err)
			}
		}
		log.Infof(ctx, "placement not ready for nodepool %s, requeueing after %s", np.Name, requeuePending)
		return false, reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	// Installed or already-dispatched pools must not depend on KCC availability.
	if ready, err := r.garAccessReady(ctx, np, cluster); err != nil {
		return false, reconcile.Result{}, err
	} else if !ready {
		return false, reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	if np.Status.VersionResolution == nil || np.Status.VersionResolution.ReleaseVersion == "" {
		if setWaitingNPConditions(np, "VersionResolutionNotReady", "Waiting for nodepool version resolution") {
			if err := r.client.Status().Update(ctx, np); err != nil && !apierrors.IsConflict(err) {
				return false, reconcile.Result{}, fmt.Errorf("nodepool reconciler: update nodepool status: %w", err)
			}
		}
		log.Infof(ctx, "nodepool VR not ready for nodepool %s, requeueing after %s", np.Name, requeuePending)
		return false, reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	// Pending upgrade approval keeps the observed release eligible for scaling.
	version := installationVersion(np)
	if np.Status.VersionResolution.ReleaseVersion != version {
		msg := fmt.Sprintf("VR version %q does not match spec version %q",
			np.Status.VersionResolution.ReleaseVersion, version)
		if setWaitingNPConditions(np, "VersionMismatch", msg) {
			if err := r.client.Status().Update(ctx, np); err != nil && !apierrors.IsConflict(err) {
				return false, reconcile.Result{}, fmt.Errorf("nodepool reconciler: update nodepool status: %w", err)
			}
		}
		log.Infof(ctx, "nodepool VR version %q does not match spec version %q, requeueing after %s",
			np.Status.VersionResolution.ReleaseVersion, version, requeuePending)
		return false, reconcile.Result{RequeueAfter: requeuePending}, nil
	}

	return true, reconcile.Result{}, nil
}
