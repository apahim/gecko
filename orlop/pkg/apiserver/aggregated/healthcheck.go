package aggregated

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"

	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/server/healthz"
)

var _ healthz.HealthChecker = (*StorageHealthChecker)(nil)

// StorageHealthChecker verifies storage backend connectivity by performing
// a lightweight List operation against the store.
type StorageHealthChecker struct {
	storageFactory func(resourceType string, scheme *runtime.Scheme, gvk runtimeschema.GroupVersionKind) (storage.ResourceStore, error)
	scheme         *runtime.Scheme
	probeGVK       runtimeschema.GroupVersionKind
	probeResource  string
	timeout        time.Duration
}

// NewStorageHealthChecker creates a health checker that probes storage connectivity.
// probeResource/probeGVK identify a resource type to list as a connectivity test.
func NewStorageHealthChecker(
	storageFactory func(string, *runtime.Scheme, runtimeschema.GroupVersionKind) (storage.ResourceStore, error),
	scheme *runtime.Scheme,
	probeResource string,
	probeGVK runtimeschema.GroupVersionKind,
) *StorageHealthChecker {
	return &StorageHealthChecker{
		storageFactory: storageFactory,
		scheme:         scheme,
		probeGVK:       probeGVK,
		probeResource:  probeResource,
		timeout:        5 * time.Second,
	}
}

func (c *StorageHealthChecker) Name() string {
	return "storage"
}

func (c *StorageHealthChecker) Check(_ *http.Request) error {
	store, err := c.storageFactory(c.probeResource, c.scheme, c.probeGVK)
	if err != nil {
		return fmt.Errorf("storage factory failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	opts := storage.ListOptions{}
	opts.Limit = 1
	_, err = store.List(ctx, opts)
	if err != nil {
		return fmt.Errorf("storage probe failed: %w", err)
	}
	return nil
}
