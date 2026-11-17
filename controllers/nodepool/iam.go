package nodepool

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/util/catalog"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func poolWorkKnown(p *privatev1.NodePool) bool {
	return p.Status.Version != "" || (p.Status.IAMDispatch != nil && p.Status.IAMDispatch.Dispatched != nil)
}

func (r *Reconciler) iamReady(ctx context.Context, p *privatev1.NodePool, c *privatev1.Cluster) (bool, error) {
	vr := p.Status.VersionResolution
	committed := vr != nil && vr.ReleaseVersion == installationVersion(p) && vr.ChannelGroup == catalog.NodePoolInstallationGroup(p) && privatev1.CommittedIAMDispatch(p.Status.IAMDispatch, string(p.UID), string(c.UID), vr) && privatev1.HasWIFConfigCoverage(c, vr.WIFConfigTemplateRef)
	var err error
	if !committed {
		_, err = catalog.Prerequisites(ctx, r.reader(), c, installationVersion(p), catalog.NodePoolInstallationGroup(p))
	}
	return r.iamCondition(ctx, p, err)
}

func (r *Reconciler) iamCondition(ctx context.Context, p *privatev1.NodePool, checkErr error) (bool, error) {
	condition := metav1.Condition{Type: privatev1.IAMPermissionsReady, Status: metav1.ConditionTrue, Reason: "IAMContractCovered", Message: "The installation target has exact applied IAM contract coverage", ObservedGeneration: p.Generation}
	if checkErr != nil {
		var blocked *catalog.UnavailableError
		if !errors.As(checkErr, &blocked) {
			return false, checkErr
		}
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, blocked.Reason, blocked.Message
	}
	if meta.SetStatusCondition(&p.Status.Conditions, condition) {
		if err := r.client.Status().Update(ctx, p); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, fmt.Errorf("update nodepool status failed")
		}
	}
	return checkErr == nil, nil
}

func (r *Reconciler) observeResources(ctx context.Context, p *privatev1.NodePool, c *privatev1.Cluster) (reconcile.Result, error) {
	if !poolWorkKnown(p) || c.Status.PlacementResult == nil || c.Status.PlacementResult.ManagementClusterName == "" {
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}
	key, err := transport.NodePoolGroupKey(p.Namespace, p.Spec.ClusterID, p.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	status, err := r.transport.GetStatus(ctx, c.Status.PlacementResult.ManagementClusterName, key)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("observe committed resources: %w", err)
	}
	if status == nil || status.Stale {
		return reconcile.Result{RequeueAfter: requeuePending}, nil
	}
	changed := r.applyStatusConditions(p, string(c.UID), status)
	changed = privatev1.PromoteIAMDispatch(p.Status.IAMDispatch, p.Status.Version) || changed
	if changed {
		if err := r.client.Status().Update(ctx, p); err != nil && !apierrors.IsConflict(err) {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: requeuePending}, nil
}

func (r *Reconciler) freshDispatch(ctx context.Context, p *privatev1.NodePool, c *privatev1.Cluster) (*privatev1.IAMDispatchReceipt, bool, error) {
	var current privatev1.NodePool
	var parent privatev1.Cluster
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(p), &current); err != nil {
		return nil, false, fmt.Errorf("read current NodePool dispatch state failed")
	}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(c), &parent); err != nil {
		return nil, false, fmt.Errorf("read current parent dispatch state failed")
	}
	if current.UID != p.UID || parent.UID != c.UID || !current.DeletionTimestamp.IsZero() || !parent.DeletionTimestamp.IsZero() || !reflect.DeepEqual(current.Spec, p.Spec) || !reflect.DeepEqual(parent.Spec, c.Spec) || !reflect.DeepEqual(current.Status.VersionResolution, p.Status.VersionResolution) || !reflect.DeepEqual(parent.Status.PlacementResult, c.Status.PlacementResult) {
		return nil, false, &catalog.UnavailableError{Reason: "DispatchStateChanged", Message: "Installation state changed; dispatch will be retried"}
	}
	vr := p.Status.VersionResolution
	if vr == nil || vr.ReleaseVersion != installationVersion(&current) || vr.ChannelGroup != catalog.NodePoolInstallationGroup(&current) {
		return nil, false, &catalog.UnavailableError{Reason: "VersionResolutionNotReady", Message: "Waiting for resolution of the exact NodePool installation target"}
	}
	return catalog.Dispatch(ctx, r.reader(), &parent, string(p.UID), vr, current.Status.IAMDispatch)
}
