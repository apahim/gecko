package nodepool

import (
	"context"
	"fmt"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
)

// Keep the historical annotation key for existing NodePool cleanup targets.
const workerCleanupTargetAnnotation = "gcp.managed.openshift.io/gar-worker-target"

func (r *Reconciler) recordWorkerCleanupTarget(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster) (bool, error) {
	target := cluster.Status.PlacementResult.ManagementClusterName
	if recorded := np.GetAnnotations()[workerCleanupTargetAnnotation]; recorded != "" && recorded != target {
		return false, fmt.Errorf("worker cleanup target %s differs from current placement %s", recorded, target)
	}
	if np.GetAnnotations()[workerCleanupTargetAnnotation] != target {
		annotations := np.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		// Persist the cleanup target before transport can create worker resources.
		annotations[workerCleanupTargetAnnotation] = target
		np.SetAnnotations(annotations)
		if err := r.client.Update(ctx, np); err != nil {
			return false, fmt.Errorf("record NodePool worker cleanup target: %w", err)
		}
		return false, nil
	}
	return true, nil
}

func (r *Reconciler) workerCleanupTarget(np *privatev1.NodePool, cluster *privatev1.Cluster, clusterFound bool) (string, error) {
	target := np.GetAnnotations()[workerCleanupTargetAnnotation]
	if target != "" {
		return target, nil
	}
	// Pre-existing pools may have dispatched without a successful status write.
	if !clusterFound || cluster.Status.PlacementResult == nil || cluster.Status.PlacementResult.ManagementClusterName == "" {
		return "", fmt.Errorf("cannot verify worker cleanup for NodePool %s: no recorded management cluster or current placement", np.Name)
	}
	return cluster.Status.PlacementResult.ManagementClusterName, nil
}
