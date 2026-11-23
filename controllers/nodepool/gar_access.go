package nodepool

import (
	"context"
	"fmt"

	"github.com/openshift-online/gecko/controllers/client/transport"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// GARAccessGate reports whether the parent Cluster's GAR Reader grant is ready.
type GARAccessGate func(context.Context, *privatev1.Cluster) (bool, error)

// WithGARAccessGate enables a prerequisite for initial downstream dispatch only.
func (r *Reconciler) WithGARAccessGate(gate GARAccessGate) *Reconciler {
	r.garAccessGate = gate
	return r
}

func (r *Reconciler) garAccessReady(ctx context.Context, np *privatev1.NodePool, cluster *privatev1.Cluster) (bool, error) {
	if r.garAccessGate == nil || np.Status.Version != "" {
		return true, nil
	}
	groupKey, err := transport.NodePoolGroupKey(np.Namespace, np.Spec.ClusterID, np.Name)
	if err != nil {
		return false, fmt.Errorf("nodepool reconciler: build GAR gate group key: %w", err)
	}
	// This inventory includes persisted ApplyDesires even before kube-applier
	// reports status or Gecko acknowledges a dispatch. Do not gate their recovery.
	dispatch, err := r.transport.GetDeleteStatus(ctx, cluster.Status.PlacementResult.ManagementClusterName, groupKey)
	if err != nil {
		return false, fmt.Errorf("nodepool reconciler: get dispatch inventory: %w", err)
	}
	if dispatch == nil {
		return false, fmt.Errorf("nodepool reconciler: dispatch inventory is unavailable")
	}
	if dispatch.ApplyDesiresCount > 0 {
		return true, nil
	}
	ready, err := r.garAccessGate(ctx, cluster)
	if err != nil {
		return false, fmt.Errorf("nodepool reconciler: check GAR access: %w", err)
	}
	if ready {
		return true, nil
	}
	if setWaitingNPConditions(np, "GARAccessPending", "Waiting for Config Connector to confirm the customer GAR Reader grant") {
		if err := r.client.Status().Update(ctx, np); err != nil && !apierrors.IsConflict(err) {
			return false, fmt.Errorf("nodepool reconciler: update GAR waiting status: %w", err)
		}
	}
	return false, nil
}
