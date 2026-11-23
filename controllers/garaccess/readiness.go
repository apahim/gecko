package garaccess

import (
	"context"
	"fmt"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GrantReady reads the exact Cluster grant from the regional Kubernetes API.
// KCC readiness is a provisioning checkpoint, not a live Google IAM check.
func GrantReady(ctx context.Context, reader client.Reader, config Config, cluster *privatev1.Cluster) (bool, error) {
	if err := config.Validate(); err != nil {
		return false, err
	}
	member, err := customerPrincipalSet(cluster)
	if err != nil {
		return false, err
	}
	desired := desiredGrant(config, cluster.Namespace, member)
	existing := NewPolicyMember()
	if err := reader.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get GAR Reader grant readiness: %w", err)
	}
	if !matchesGrant(existing, desired) {
		return false, fmt.Errorf("IAMPolicyMember does not match the expected GAR Reader grant")
	}
	if !existing.GetDeletionTimestamp().IsZero() || existing.GetAnnotations()[stateAnnotation] != "active" {
		return false, nil
	}
	observed, found, err := unstructured.NestedInt64(existing.Object, "status", "observedGeneration")
	if err != nil {
		return false, fmt.Errorf("read GAR grant observed generation: %w", err)
	}
	if !found || existing.GetGeneration() == 0 || observed != existing.GetGeneration() {
		return false, nil
	}
	conditions, _, err := unstructured.NestedSlice(existing.Object, "status", "conditions")
	if err != nil {
		return false, fmt.Errorf("read GAR grant conditions: %w", err)
	}
	for _, value := range conditions {
		condition, ok := value.(map[string]interface{})
		if ok && condition["type"] == "Ready" && condition["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}
