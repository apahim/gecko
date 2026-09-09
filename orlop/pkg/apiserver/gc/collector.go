package gc

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Collector implements garbage collection for objects with owner references.
// It periodically scans all objects and deletes those whose owners no longer exist.
// When broadcasters implement storage.EventPruner, old events are pruned each cycle.
type Collector struct {
	stores         map[runtimeschema.GroupKind]storage.ResourceStore
	broadcasters   []storage.EventBroadcaster
	eventRetention time.Duration
	interval       time.Duration
	logger         logr.Logger
	stopCh         chan struct{}
	stopOnce       sync.Once
}

// NewCollector creates a new garbage collector.
func NewCollector(stores map[runtimeschema.GroupKind]storage.ResourceStore, interval time.Duration, logger logr.Logger) *Collector {
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}
	return &Collector{
		stores:         stores,
		eventRetention: 24 * time.Hour,
		interval:       interval,
		logger:         logger,
		stopCh:         make(chan struct{}),
	}
}

// SetBroadcasters configures the broadcasters whose old events should be
// pruned during garbage collection. Only broadcasters that implement
// storage.EventPruner will have their events pruned.
func (c *Collector) SetBroadcasters(broadcasters []storage.EventBroadcaster) {
	c.broadcasters = broadcasters
}

// SetEventRetention configures how long events are retained before pruning.
// Defaults to 24 hours.
func (c *Collector) SetEventRetention(d time.Duration) {
	c.eventRetention = d
}

// Start begins the garbage collection loop in a background goroutine.
func (c *Collector) Start(ctx context.Context) {
	c.logger.Info("Starting garbage collector", "interval", c.interval)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Run once immediately
	c.collectGarbage()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("Garbage collector stopped")
			return
		case <-c.stopCh:
			c.logger.Info("Garbage collector stopped")
			return
		case <-ticker.C:
			c.collectGarbage()
		}
	}
}

// Stop gracefully stops the garbage collector.
func (c *Collector) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// collectGarbage performs one garbage collection cycle across all stores.
func (c *Collector) collectGarbage() {
	c.logger.V(1).Info("Running garbage collection cycle")
	startTime := time.Now()

	deleted := 0
	checked := 0

	ctx := context.Background()

	for gk, store := range c.stores {
		// List all objects in this store
		list, err := store.List(ctx, storage.ListOptions{})
		if err != nil {
			c.logger.Error(err, "Failed to list objects for GC", "groupKind", gk)
			continue
		}

		items, err := meta.ExtractList(list)
		if err != nil {
			c.logger.Error(err, "Failed to extract list items", "groupKind", gk)
			continue
		}

		// Check each object for orphaned owner references
		for _, item := range items {
			checked++

			obj, ok := item.(client.Object)
			if !ok {
				continue
			}

			accessor, err := meta.Accessor(obj)
			if err != nil {
				continue
			}

			ownerRefs := accessor.GetOwnerReferences()
			if len(ownerRefs) == 0 {
				continue
			}

			// Check if any owner still exists
			shouldDelete := false
			for _, ownerRef := range ownerRefs {
				exists, err := c.ownerExists(ownerRef, accessor.GetNamespace())
				if err != nil {
					c.logger.Error(err, "Failed to check owner existence",
						"object", accessor.GetName(),
						"namespace", accessor.GetNamespace(),
						"owner", ownerRef.Name)
					continue
				}

				if !exists {
					c.logger.Info("Owner no longer exists, deleting dependent",
						"object", accessor.GetName(),
						"namespace", accessor.GetNamespace(),
						"owner", ownerRef.Name,
						"ownerKind", ownerRef.Kind)
					shouldDelete = true
					break
				}
			}

			if shouldDelete {
				// Delete the object
				if err := store.Delete(ctx, accessor.GetNamespace(), accessor.GetName()); err != nil {
					if !errors.IsNotFound(err) {
						c.logger.Error(err, "Failed to delete orphaned object",
							"object", accessor.GetName(),
							"namespace", accessor.GetNamespace())
					}
				} else {
					deleted++
					c.logger.V(1).Info("Deleted orphaned object",
						"object", accessor.GetName(),
						"namespace", accessor.GetNamespace())
				}
			}
		}
	}

	c.pruneEvents(ctx)

	duration := time.Since(startTime)
	c.logger.Info("Garbage collection cycle complete",
		"duration", duration,
		"checked", checked,
		"deleted", deleted)
}

// pruneEvents calls PruneOldEvents on each broadcaster that supports it.
func (c *Collector) pruneEvents(ctx context.Context) {
	for _, b := range c.broadcasters {
		pruner, ok := b.(storage.EventPruner)
		if !ok {
			continue
		}
		if err := pruner.PruneOldEvents(ctx, c.eventRetention); err != nil {
			c.logger.Error(err, "Failed to prune old events")
		}
	}
}

// ownerExists checks if an owner object still exists in storage.
func (c *Collector) ownerExists(ownerRef metav1.OwnerReference, namespace string) (bool, error) {
	ctx := context.Background()
	// Find the store for the owner's resource type
	// This is a simplified check - in a real implementation, we'd need to map
	// Kind to resource type more accurately
	for _, store := range c.stores {
		_, err := store.Get(ctx, namespace, ownerRef.Name)
		if err == nil {
			// Owner exists
			return true, nil
		}
		if errors.IsNotFound(err) {
			// Owner doesn't exist in this store, continue checking others
			continue
		}
		// Some other error occurred
		return false, err
	}

	// Owner not found in any store
	return false, nil
}
