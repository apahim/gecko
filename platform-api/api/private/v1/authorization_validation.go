package v1

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"

	orloptypes "github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	"k8s.io/apimachinery/pkg/runtime"
)

const authorizationAPIGroup = "gcp.managed.openshift.io"

var validAuthorizationPermissions = map[string]struct{}{
	"cluster.create":     {},
	"cluster.list":       {},
	"cluster.get":        {},
	"cluster.update":     {},
	"cluster.delete":     {},
	"nodepool.create":    {},
	"nodepool.list":      {},
	"nodepool.get":       {},
	"nodepool.update":    {},
	"nodepool.delete":    {},
	"rolebinding.create": {},
	"rolebinding.list":   {},
	"rolebinding.get":    {},
	"rolebinding.update": {},
	"rolebinding.delete": {},
	"role.create":        {},
	"role.list":          {},
	"role.get":           {},
	"role.update":        {},
	"role.delete":        {},
}

// ValidAuthorizationPermissions returns the complete set of permissions
// accepted by Role and PlatformRole resources.
func ValidAuthorizationPermissions() []string {
	permissions := make([]string, 0, len(validAuthorizationPermissions))
	for permission := range validAuthorizationPermissions {
		permissions = append(permissions, permission)
	}
	sort.Strings(permissions)
	return permissions
}

// IsValidAuthorizationPermission reports whether permission is supported by
// the public API authorization model.
func IsValidAuthorizationPermission(permission string) bool {
	_, ok := validAuthorizationPermissions[permission]
	return ok
}

// NormalizeEmail applies the canonical principal representation shared by
// RoleBinding admission and ESPv2 identity extraction: Unicode NFC, preserved
// local-part case, and a lower-case domain.
func NormalizeEmail(email string) (string, error) {
	email = norm.NFC.String(strings.TrimSpace(email))
	if email == "" {
		return "", fmt.Errorf("email must not be empty")
	}
	if strings.Count(email, "@") != 1 {
		return "", fmt.Errorf("email must contain exactly one @")
	}
	parts := strings.SplitN(email, "@", 2)
	if parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("email must contain a local part and domain")
	}
	return parts[0] + "@" + strings.ToLower(parts[1]), nil
}

// ValidatorDeps supplies storage-backed checks that cannot live in the API
// types package without creating an import cycle with the authorization
// implementation.
//
//nolint:kubeapilinter // runtime validation callbacks are not serialized API fields.
type ValidatorDeps struct {
	RoleExists         func(context.Context, string, string) (bool, error)
	PlatformRoleExists func(context.Context, string) (bool, error)
}

var validatorDeps struct {
	sync.RWMutex
	deps ValidatorDeps
}

// SetValidatorDeps installs the runtime checks used by RoleBinding
// validation. It is safe to call during server startup before requests are
// served.
func SetValidatorDeps(deps ValidatorDeps) {
	validatorDeps.Lock()
	validatorDeps.deps = deps
	validatorDeps.Unlock()
}

func getValidatorDeps() ValidatorDeps {
	validatorDeps.RLock()
	defer validatorDeps.RUnlock()
	return validatorDeps.deps
}

func validatePermissions(permissions []string) error {
	if len(permissions) == 0 {
		return fmt.Errorf("spec.permissions must contain at least one permission")
	}

	seen := make(map[string]struct{}, len(permissions))
	for _, permission := range permissions {
		if !IsValidAuthorizationPermission(permission) {
			return fmt.Errorf("unknown permission %q (valid permissions: %s)", permission, strings.Join(ValidAuthorizationPermissions(), ", "))
		}
		if _, duplicate := seen[permission]; duplicate {
			return fmt.Errorf("spec.permissions contains duplicate permission %q", permission)
		}
		seen[permission] = struct{}{}
	}
	return nil
}

func (r *Role) ValidateCreate(_ context.Context) error {
	return validatePermissions(r.Spec.Permissions)
}

func (r *Role) ValidateUpdate(_ context.Context, oldObj runtime.Object) error {
	if _, ok := oldObj.(*Role); !ok {
		return fmt.Errorf("expected old object to be *Role, got %T", oldObj)
	}
	return validatePermissions(r.Spec.Permissions)
}

func (r *Role) ValidateDelete(_ context.Context) error { return nil }

func (r *PlatformRole) ValidateCreate(_ context.Context) error {
	return validatePermissions(r.Spec.Permissions)
}

func (r *PlatformRole) ValidateUpdate(_ context.Context, oldObj runtime.Object) error {
	if _, ok := oldObj.(*PlatformRole); !ok {
		return fmt.Errorf("expected old object to be *PlatformRole, got %T", oldObj)
	}
	return validatePermissions(r.Spec.Permissions)
}

func (r *PlatformRole) ValidateDelete(_ context.Context) error { return nil }

func (r *RoleBinding) ValidateCreate(ctx context.Context) error {
	if err := r.validateSubject(); err != nil {
		return err
	}
	return validateRoleReference(ctx, r.Namespace, r.Spec.RoleRef)
}

func (r *RoleBinding) ValidateUpdate(ctx context.Context, oldObj runtime.Object) error {
	if _, ok := oldObj.(*RoleBinding); !ok {
		return fmt.Errorf("expected old object to be *RoleBinding, got %T", oldObj)
	}
	if err := r.validateSubject(); err != nil {
		return err
	}
	return validateRoleReference(ctx, r.Namespace, r.Spec.RoleRef)
}

func (r *RoleBinding) ValidateDelete(_ context.Context) error { return nil }

func (r *RoleBinding) validateSubject() error {
	canonical, err := NormalizeEmail(r.Spec.Subject)
	if err != nil {
		return fmt.Errorf("spec.subject: %w", err)
	}
	// Canonicalization is deliberately performed during validation so both
	// public and private API writes store the same principal key.
	r.Spec.Subject = canonical
	return nil
}

func validateRoleReference(ctx context.Context, namespace string, ref RoleRef) error {
	if ref.Name == "" {
		return fmt.Errorf("spec.roleRef.name must not be empty")
	}
	if ref.APIGroup != authorizationAPIGroup {
		return fmt.Errorf("spec.roleRef.apiGroup must be %q", authorizationAPIGroup)
	}

	deps := getValidatorDeps()
	switch ref.Kind {
	case "PlatformRole":
		if deps.PlatformRoleExists == nil {
			return nil
		}
		exists, err := deps.PlatformRoleExists(ctx, ref.Name)
		if err != nil {
			return fmt.Errorf("checking PlatformRole %q: %w", ref.Name, err)
		}
		if !exists {
			return fmt.Errorf("PlatformRole %q does not exist", ref.Name)
		}
	case "Role":
		if deps.RoleExists == nil {
			return nil
		}
		exists, err := deps.RoleExists(ctx, namespace, ref.Name)
		if err != nil {
			return fmt.Errorf("checking Role %q in namespace %q: %w", ref.Name, namespace, err)
		}
		if !exists {
			return fmt.Errorf("Role %q does not exist in namespace %q", ref.Name, namespace)
		}
	default:
		return fmt.Errorf("spec.roleRef.kind must be PlatformRole or Role")
	}
	return nil
}

var _ orloptypes.CustomValidator = (*Role)(nil)
var _ orloptypes.CustomValidator = (*PlatformRole)(nil)
var _ orloptypes.CustomValidator = (*RoleBinding)(nil)
