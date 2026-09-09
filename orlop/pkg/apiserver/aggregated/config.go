package aggregated

import (
	"fmt"

	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/server/healthz"
)

// Config holds configuration for an aggregated API server.
type Config struct {
	// Scheme is the runtime scheme containing all API types.
	Scheme *runtime.Scheme

	// Resources lists the API resources to serve.
	Resources []types.ResourceInfo

	// StorageFactory creates a ResourceStore for a given resource type.
	StorageFactory func(resourceType string, scheme *runtime.Scheme, gvk runtimeschema.GroupVersionKind) (storage.ResourceStore, error)

	// BindAddress is the address to listen on for HTTPS traffic.
	// When DisableAuth is true, this is forced to "127.0.0.1".
	BindAddress string

	// BindPort is the port to listen on for HTTPS traffic.
	BindPort int

	// CertFile is the path to a PEM-encoded TLS certificate.
	// If empty, a self-signed certificate is generated.
	CertFile string

	// KeyFile is the path to a PEM-encoded TLS private key.
	// If empty, a self-signed key is generated.
	KeyFile string

	// AuthenticationKubeconfig is the path to a kubeconfig for delegated
	// authentication via TokenReview. If empty, in-cluster config is used.
	AuthenticationKubeconfig string

	// AuthorizationKubeconfig is the path to a kubeconfig for delegated
	// authorization via SubjectAccessReview. If empty, in-cluster config is used.
	AuthorizationKubeconfig string

	// DisableAuth disables delegated authentication and authorization.
	// When true, all requests are treated as authenticated (anonymous allowed).
	// Intended for testing and standalone development mode.
	DisableAuth bool

	// HealthCheckers are additional health checks registered with /healthz, /livez, /readyz.
	HealthCheckers []healthz.HealthChecker

	// Logger is the logger for server operations.
	Logger logr.Logger
}

// completedConfig is the private validated config that cannot be constructed outside this package.
type completedConfig struct {
	*Config
}

// CompletedConfig wraps a validated Config.
// It can only be created by calling Config.Complete().
type CompletedConfig struct {
	*completedConfig
}

// Complete validates the config and returns a CompletedConfig.
func (c *Config) Complete() (CompletedConfig, error) {
	if c.Scheme == nil {
		return CompletedConfig{}, fmt.Errorf("scheme is required")
	}
	if len(c.Resources) == 0 {
		return CompletedConfig{}, fmt.Errorf("at least one resource is required")
	}
	if c.StorageFactory == nil {
		return CompletedConfig{}, fmt.Errorf("storage factory is required")
	}
	if c.DisableAuth {
		if c.BindAddress != "" && c.BindAddress != "127.0.0.1" && c.BindAddress != "localhost" {
			return CompletedConfig{}, fmt.Errorf("--disable-auth requires binding to localhost, refusing to bind to %q", c.BindAddress)
		}
		c.BindAddress = "127.0.0.1"
	}
	if c.BindAddress == "" {
		c.BindAddress = "0.0.0.0"
	}
	if c.BindPort == 0 {
		c.BindPort = 6443
	}
	if c.Logger.GetSink() == nil {
		c.Logger = logr.Discard()
	}
	return CompletedConfig{&completedConfig{Config: c}}, nil
}
