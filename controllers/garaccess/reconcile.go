package garaccess

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *Reconciler) ensureGrant(ctx context.Context, project, member string) (reconcile.Result, error) {
	desired := desiredGrant(r.config, project, member)
	existing := NewPolicyMember()
	err := r.kcc.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := r.kcc.Create(ctx, desired); err != nil {
			return reconcile.Result{}, fmt.Errorf("create GAR Reader grant for %s: %w", project, err)
		}
		r.log.Infof(ctx, "gar-access: created %s/%s for %s", desired.GetNamespace(), desired.GetName(), project)
		return reconcile.Result{RequeueAfter: retryPending}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("get GAR Reader grant for %s: %w", project, err)
	}
	matches, err := matchesGrant(existing, desired)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !matches {
		return reconcile.Result{}, fmt.Errorf("IAMPolicyMember %s does not match the Gecko-owned grant for %s", existing.GetName(), project)
	}
	if !existing.GetDeletionTimestamp().IsZero() {
		// KCC's final deletion event queues grant recreation if consumers remain.
		return reconcile.Result{}, nil
	}
	annotations := existing.GetAnnotations()
	_, hasLegacyConsumers := annotations[consumersKey]
	if annotations[stateAnnotation] != "active" || hasLegacyConsumers {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[stateAnnotation] = "active"
		delete(annotations, consumersKey)
		existing.SetAnnotations(annotations)
		if err := r.kcc.Update(ctx, existing); err != nil {
			return reconcile.Result{}, fmt.Errorf("update GAR grant metadata for %s: %w", project, err)
		}
	}
	return reconcile.Result{}, nil
}

// Cluster finalization owns worker cleanup. GAR retains access for every extant
// Cluster, including deleting Clusters, without duplicating NodePool inventory.
func (r *Reconciler) clustersRemain(ctx context.Context, project string) (bool, error) {
	consumerCount, _, err := r.consumers(ctx, project)
	if err != nil {
		return false, err
	}
	return consumerCount != 0, nil
}

// removeGrants runs after Reconcile has observed no Cluster consumers.
func (r *Reconciler) removeGrants(ctx context.Context, project string) (reconcile.Result, error) {
	policies := &unstructured.UnstructuredList{}
	policies.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "iam.cnrm.cloud.google.com", Version: "v1beta1", Kind: "IAMPolicyMemberList",
	})
	if err := r.kcc.List(ctx, policies, client.InNamespace(r.config.IAMNamespace),
		client.MatchingLabels{managedLabel: managedBy, projectLabel: project}); err != nil {
		return reconcile.Result{}, fmt.Errorf("list GAR Reader grants for %s: %w", project, err)
	}
	for i := range policies.Items {
		policy := &policies.Items[i]
		if len(r.PolicyMemberToProject(ctx, policy)) == 0 {
			continue
		}
		member, found, err := unstructured.NestedString(policy.Object, "spec", "member")
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("read spec.member of IAMPolicyMember %s: %w", policy.GetName(), err)
		}
		if !found {
			return reconcile.Result{}, fmt.Errorf("IAMPolicyMember %s is missing spec.member", policy.GetName())
		}
		matches, err := matchesGrant(policy, desiredGrant(r.config, project, member))
		if err != nil {
			return reconcile.Result{}, err
		}
		if !matches {
			return reconcile.Result{}, fmt.Errorf("refuse to delete unexpected IAMPolicyMember %s", policy.GetName())
		}
		if !policy.GetDeletionTimestamp().IsZero() {
			// KCC updates and deletion events resume cleanup.
			return reconcile.Result{}, nil
		}
		// Mark revocation before the final live Cluster-inventory check.
		annotations := policy.GetAnnotations()
		_, hasLegacyConsumers := annotations[consumersKey]
		if annotations[stateAnnotation] != "draining" || hasLegacyConsumers {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[stateAnnotation] = "draining"
			delete(annotations, consumersKey)
			policy.SetAnnotations(annotations)
			if err := r.kcc.Update(ctx, policy); err != nil {
				return reconcile.Result{}, fmt.Errorf("mark GAR Reader grant draining for %s: %w", project, err)
			}
		}
		remaining, err := r.clustersRemain(ctx, project)
		if err != nil {
			return reconcile.Result{}, err
		}
		if remaining {
			return reconcile.Result{RequeueAfter: retryPending}, nil
		}
		uid, version := policy.GetUID(), policy.GetResourceVersion()
		if err := r.kcc.Delete(ctx, policy, client.Preconditions{UID: &uid, ResourceVersion: &version}); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("delete GAR Reader grant for %s: %w", project, err)
		}
		r.log.Infof(ctx, "gar-access: deleting %s/%s for %s", policy.GetNamespace(), policy.GetName(), project)
		return reconcile.Result{RequeueAfter: retryPending}, nil
	}
	return reconcile.Result{}, nil
}
