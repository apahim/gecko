package nodepool

import (
	"context"
	"fmt"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const garWorkerTarget = "gcp.managed.openshift.io/gar-worker-target"

// GARAccessGate is required before any NodePool resources can be dispatched.
type GARAccessGate interface {
	Check(context.Context, string) error
}

func (r *Reconciler) garAccessReady(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster) (bool, error) {
	if r.GARAccess == nil {
		return false, fmt.Errorf("GAR access gate is not configured")
	}
	var checkErr error
	if cluster.Spec.Platform.GCP == nil || cluster.Spec.Platform.GCP.ProjectID != np.Namespace {
		checkErr = fmt.Errorf("cluster must target customer project %s", np.Namespace)
	} else {
		checkErr = r.GARAccess.Check(ctx, np.Namespace)
	}
	target := cluster.Status.PlacementResult.ManagementClusterName
	if recorded := np.GetAnnotations()[garWorkerTarget]; recorded != "" && recorded != target {
		checkErr = fmt.Errorf("worker cleanup target %s differs from current placement %s", recorded, target)
	}
	condition := metav1.Condition{
		Type: "NodePoolGARAccessReady", Status: metav1.ConditionTrue,
		Reason: "GARAccessReady", Message: "Config Connector reconciled the customer GAR Reader grant",
		ObservedGeneration: np.Generation,
	}
	changed := false
	if checkErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "GARAccessNotReady"
		condition.Message = checkErr.Error()
		changed = setWaitingNPConditions(np, condition.Reason, condition.Message)
		r.log.Warnf(ctx, "nodepool %s: waiting for GAR access: %v", np.Name, checkErr)
	}
	if meta.SetStatusCondition(&np.Status.Conditions, condition) {
		changed = true
	}
	if changed {
		if err := r.client.Status().Update(ctx, np); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, fmt.Errorf("update NodePool GAR access status: %w", err)
		}
	}
	if checkErr != nil {
		return false, nil
	}
	return r.recordGARWorkerTarget(ctx, np, cluster)
}

func (r *Reconciler) recordGARWorkerTarget(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster) (bool, error) {
	target := cluster.Status.PlacementResult.ManagementClusterName
	if recorded := np.GetAnnotations()[garWorkerTarget]; recorded != "" && recorded != target {
		return false, fmt.Errorf("worker cleanup target %s differs from current placement %s", recorded, target)
	}
	if np.GetAnnotations()[garWorkerTarget] != target {
		annotations := np.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		// Persist the cleanup target before transport can create worker resources.
		annotations[garWorkerTarget] = target
		np.SetAnnotations(annotations)
		if err := r.client.Update(ctx, np); err != nil {
			return false, fmt.Errorf("record NodePool worker cleanup target: %w", err)
		}
		return false, nil
	}
	return true, nil
}

func (r *Reconciler) workerCleanupTarget(np *privatev1.NodePool, cluster *privatev1.Cluster, clusterFound bool) (string, error) {
	target := np.GetAnnotations()[garWorkerTarget]
	if target != "" {
		return target, nil
	}
	// Pre-existing pools may have dispatched without a successful status write.
	if !clusterFound || cluster.Status.PlacementResult == nil || cluster.Status.PlacementResult.ManagementClusterName == "" {
		return "", fmt.Errorf("cannot verify worker cleanup for NodePool %s: no recorded management cluster or current placement", np.Name)
	}
	return cluster.Status.PlacementResult.ManagementClusterName, nil
}
