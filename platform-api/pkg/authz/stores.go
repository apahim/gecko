package authz

import (
	"context"
	"fmt"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

// Stores contains the authorization resources used by policy generation and
// entity loading. The server constructs these through its memoized factory so
// API handlers, validators, and authorization watchers share store instances.
type Stores struct {
	Scheme        *runtime.Scheme
	PlatformRoles storage.ResourceStore
	Roles         storage.ResourceStore
	RoleBindings  storage.ResourceStore
}

// NewStores creates stores for the three authorization resource kinds.
func NewStores(factory apiserver.StorageFactory, scheme *runtime.Scheme) (Stores, error) {
	if factory == nil {
		return Stores{}, fmt.Errorf("storage factory is required")
	}
	if scheme == nil {
		return Stores{}, fmt.Errorf("scheme is required")
	}

	platformRoleGVK := privatev1.GroupVersion.WithKind("PlatformRole")
	roleGVK := privatev1.GroupVersion.WithKind("Role")
	roleBindingGVK := privatev1.GroupVersion.WithKind("RoleBinding")

	platformRoles, err := newStore(factory, scheme, platformRoleGVK)
	if err != nil {
		return Stores{}, err
	}
	roles, err := newStore(factory, scheme, roleGVK)
	if err != nil {
		return Stores{}, err
	}
	roleBindings, err := newStore(factory, scheme, roleBindingGVK)
	if err != nil {
		return Stores{}, err
	}

	return Stores{
		Scheme:        scheme,
		PlatformRoles: platformRoles,
		Roles:         roles,
		RoleBindings:  roleBindings,
	}, nil
}

func newStore(factory apiserver.StorageFactory, scheme *runtime.Scheme, gvk runtimeschema.GroupVersionKind) (storage.ResourceStore, error) {
	store, err := factory(apiserver.GroupKindResourceType(gvk.GroupKind()), scheme, gvk)
	if err != nil {
		return nil, fmt.Errorf("create store for %s: %w", gvk.Kind, err)
	}
	if store == nil {
		return nil, fmt.Errorf("create store for %s returned nil", gvk.Kind)
	}
	return store, nil
}

func (s Stores) RoleExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := s.Roles.Get(ctx, namespace, name)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s Stores) PlatformRoleExists(ctx context.Context, name string) (bool, error) {
	_, err := s.PlatformRoles.Get(ctx, "", name)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
