package garaccess

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Gate checks the live regional grant before NodePool resources are dispatched.
type Gate struct {
	reader   client.Reader
	identity *identityLookup
	config   Config
}

// NewGate requires an uncached regional API reader, not the Gecko cache.
func NewGate(ctx context.Context, reader client.Reader, config Config) (*Gate, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	identity, err := newIdentityLookup(ctx)
	if err != nil {
		return nil, err
	}
	return &Gate{reader: reader, identity: identity, config: config}, nil
}

// Check succeeds only for the matching active, current-generation Ready grant.
func (g *Gate) Check(ctx context.Context, project string) error {
	member, err := g.identity.customerMember(ctx, project)
	if err != nil {
		return err
	}
	desired := desiredGrant(g.config, project, member)
	policy := NewPolicyMember()
	if err := g.reader.Get(ctx, client.ObjectKeyFromObject(desired), policy); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("waiting for IAMPolicyMember %s/%s", desired.GetNamespace(), desired.GetName())
		}
		return fmt.Errorf("read GAR Reader grant: %w", err)
	}
	if !matchesGrant(policy, desired) {
		return fmt.Errorf("IAMPolicyMember %s does not match the expected customer GAR grant", policy.GetName())
	}
	if !policy.GetDeletionTimestamp().IsZero() || policy.GetAnnotations()[stateAnnotation] != "active" {
		return fmt.Errorf("IAMPolicyMember %s is draining or deleting", policy.GetName())
	}
	observed, found, err := unstructured.NestedInt64(policy.Object, "status", "observedGeneration")
	if err != nil {
		return fmt.Errorf("read KCC observedGeneration: %w", err)
	}
	if !found || observed != policy.GetGeneration() {
		return fmt.Errorf("waiting for Config Connector to observe generation %d", policy.GetGeneration())
	}
	conditions, _, err := unstructured.NestedSlice(policy.Object, "status", "conditions")
	if err != nil {
		return fmt.Errorf("read KCC conditions: %w", err)
	}
	for _, entry := range conditions {
		condition, ok := entry.(map[string]interface{})
		if !ok || condition["type"] != "Ready" {
			continue
		}
		if condition["status"] == "True" {
			return nil
		}
		return fmt.Errorf("Config Connector Ready=%v: %v (%v)", condition["status"], condition["message"], condition["reason"])
	}
	return fmt.Errorf("waiting for Config Connector Ready condition")
}
