package hc

import (
	"context"
	"fmt"
	"reflect"

	"github.com/openshift-online/gecko/controllers/client/transport"
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
func (r *Reconciler) applyResources(ctx context.Context, c *privatev1.Cluster, managementCluster, groupKey string, manifests [][]byte) (*transport.Status, error) {
	upgrading := privatev1.ClusterReleaseApproved(c) && meta.IsStatusConditionTrue(c.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted) && c.Status.HostedClusterResult != nil && c.Spec.Release.Version != c.Status.HostedClusterResult.Version
	receipt, persist, err := r.freshDispatch(ctx, c)
	if err != nil {
		if _, writeErr := r.iamCondition(ctx, c, err); writeErr != nil {
			return nil, writeErr
		}
		return &transport.Status{Stale: true}, nil
	}
	if persist {
		if c.Status.IAMDispatch == nil {
			c.Status.IAMDispatch = &privatev1.IAMDispatchState{}
		}
		c.Status.IAMDispatch.Pending = receipt
		privatev1.SyncClusterIAMRequirements(c)
		if upgrading {
			clusterSubmissionCondition(c, metav1.ConditionUnknown, "Dispatching", "The exact release and IAM contract are committed for dispatch")
		}
		if err := r.client.Status().Update(ctx, c); err != nil && !apierrors.IsConflict(err) {
			return nil, fmt.Errorf("record IAM dispatch boundary: %w", err)
		}
		return &transport.Status{Stale: true}, nil
	}
	status, err := r.transport.Apply(ctx, managementCluster, groupKey, manifests)
	if err != nil {
		return nil, err
	}
	if upgrading && !meta.IsStatusConditionTrue(c.Status.Conditions, privatev1.ControlPlaneUpgradeSubmitted) {
		clusterSubmissionCondition(c, metav1.ConditionTrue, "Dispatched", "The committed release was delivered")
		if err := r.client.Status().Update(ctx, c); err != nil {
			if apierrors.IsConflict(err) {
				return &transport.Status{Stale: true}, nil
			}
			return nil, fmt.Errorf("record dispatch acknowledgement: %w", err)
		}
	}
	return status, nil
}

func sameClusterDispatchInput(a, b *privatev1.Cluster) bool {
	return reflect.DeepEqual(a.Spec, b.Spec) && reflect.DeepEqual(a.Annotations, b.Annotations) && reflect.DeepEqual(a.Status.VersionResolution, b.Status.VersionResolution) && reflect.DeepEqual(a.Status.PlacementResult, b.Status.PlacementResult)
}
