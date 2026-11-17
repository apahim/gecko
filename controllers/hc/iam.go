package hc

import (
	"context"
	"errors"
	"fmt"

	"github.com/openshift-online/gecko/controllers/util/catalog"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func clusterWorkKnown(c *privatev1.Cluster) bool {
	return (c.Status.IAMDispatch != nil && c.Status.IAMDispatch.Dispatched != nil) || (c.Status.HostedClusterResult != nil && c.Status.HostedClusterResult.Version != "")
}

func (r *Reconciler) iamReady(ctx context.Context, c *privatev1.Cluster) (bool, error) {
	vr := c.Status.VersionResolution
	committed := vr != nil && vr.ReleaseVersion == privatev1.ClusterInstallationVersion(c) && vr.ChannelGroup == catalog.ClusterInstallationGroup(c) && privatev1.CommittedIAMDispatch(c.Status.IAMDispatch, string(c.UID), string(c.UID), vr) && privatev1.HasWIFConfigCoverage(c, vr.WIFConfigTemplateRef)
	var err error
	if !committed {
		_, err = catalog.Prerequisites(ctx, r.reader(), c, privatev1.ClusterInstallationVersion(c), catalog.ClusterInstallationGroup(c))
	}
	return r.iamCondition(ctx, c, err)
}

func (r *Reconciler) iamCondition(ctx context.Context, c *privatev1.Cluster, checkErr error) (bool, error) {
	condition := metav1.Condition{Type: privatev1.IAMPermissionsReady, Status: metav1.ConditionTrue, Reason: "IAMContractCovered", Message: "The installation target has exact applied IAM contract coverage", ObservedGeneration: c.Generation}
	if checkErr != nil {
		var blocked *catalog.UnavailableError
		if !errors.As(checkErr, &blocked) {
			return false, checkErr
		}
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, blocked.Reason, blocked.Message
	}
	if meta.SetStatusCondition(&c.Status.Conditions, condition) {
		if err := r.client.Status().Update(ctx, c); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, fmt.Errorf("update cluster status failed")
		}
	}
	return checkErr == nil, nil
}

// Observation is not authorization to create/update a Desire. Only previously
// committed or observed work may use this path; an empty initial group must not
// fabricate successful installation feedback.
func (r *Reconciler) observeResources(ctx context.Context, c *privatev1.Cluster, groupKey string) (reconcile.Result, error) {
	if !clusterWorkKnown(c) || c.Status.PlacementResult == nil || c.Status.PlacementResult.ManagementClusterName == "" {
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}
	status, err := r.transport.GetStatus(ctx, c.Status.PlacementResult.ManagementClusterName, groupKey)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("observe committed resources: %w", err)
	}
	if status == nil || status.Stale {
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}
	changed, err := r.applyStatusConditions(c, status)
	if err != nil {
		return reconcile.Result{}, err
	}
	if c.Status.HostedClusterResult != nil {
		changed = privatev1.PromoteIAMDispatch(c.Status.IAMDispatch, c.Status.HostedClusterResult.Version) || changed
	}
	if changed {
		if err := r.client.Status().Update(ctx, c); err != nil && !apierrors.IsConflict(err) {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: requeuePending}, nil
}

func clusterSubmissionCondition(c *privatev1.Cluster, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: privatev1.ControlPlaneUpgradeSubmitted, Status: status, Reason: reason, Message: message, ObservedGeneration: c.Generation})
}

func (r *Reconciler) freshDispatch(ctx context.Context, c *privatev1.Cluster) (*privatev1.IAMDispatchReceipt, bool, error) {
	var fresh privatev1.Cluster
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(c), &fresh); err != nil {
		return nil, false, fmt.Errorf("read current dispatch installation failed")
	}
	if fresh.UID != c.UID || !fresh.DeletionTimestamp.IsZero() || !sameClusterDispatchInput(c, &fresh) {
		return nil, false, &catalog.UnavailableError{Reason: "DispatchStateChanged", Message: "Installation state changed; dispatch will be retried"}
	}
	vr := c.Status.VersionResolution
	if vr == nil || vr.ReleaseVersion != privatev1.ClusterInstallationVersion(&fresh) || vr.ChannelGroup != catalog.ClusterInstallationGroup(&fresh) {
		return nil, false, &catalog.UnavailableError{Reason: "VersionResolutionNotReady", Message: "Waiting for resolution of the exact installation target"}
	}
	return catalog.Dispatch(ctx, r.reader(), &fresh, string(c.UID), vr, fresh.Status.IAMDispatch)
}
