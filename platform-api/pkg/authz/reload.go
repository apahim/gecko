package authz

import (
	"context"
	"time"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"

	"k8s.io/apimachinery/pkg/api/meta"
)

const policyReloadTimeout = 30 * time.Second

// StartWatching starts one ResourceStore watcher per authorization resource.
// Every event triggers a full policy rebuild and entity-cache invalidation;
// this intentionally favors a simple, consistent snapshot over selective
// invalidation while the authorization dataset is small.
func (a *Authorizer) StartWatching(stopCh <-chan struct{}) {
	// Reconcile once immediately before the asynchronous watchers subscribe.
	// This closes the startup race where a resource changes between the initial
	// load and watcher creation.
	startupCtx, startupCancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stopCh:
			startupCancel()
		case <-startupCtx.Done():
		}
	}()
	if err := a.reloadWithTimeout(startupCtx); err != nil {
		a.logger.Error(err, "authorization policy reload failed at watcher startup")
	}
	startupCancel()
	for name, store := range map[string]storage.ResourceStore{
		"PlatformRole": a.stores.PlatformRoles,
		"Role":         a.stores.Roles,
		"RoleBinding":  a.stores.RoleBindings,
	} {
		go a.watchStore(stopCh, name, store)
	}
}

func (a *Authorizer) watchStore(stopCh <-chan struct{}, resourceName string, store storage.ResourceStore) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	backoff := 100 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}

		resourceVersion, err := currentResourceVersion(ctx, store)
		if err != nil {
			a.logger.Error(err, "authorization resource snapshot failed", "resource", resourceName)
			if !waitForRetry(ctx, backoff) {
				return
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}

		events, stopWatch, err := store.Watch(ctx, storage.ListOptions{}, resourceVersion)
		if err != nil {
			a.logger.Error(err, "authorization watch failed", "resource", resourceName)
			if !waitForRetry(ctx, backoff) {
				return
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 100 * time.Millisecond
		if err := a.reloadWithTimeout(ctx); err != nil {
			a.logger.Error(err, "authorization policy reload failed after watcher connection", "resource", resourceName)
		}

		watchClosed := false
		for !watchClosed {
			select {
			case <-ctx.Done():
				stopWatch()
				return
			case _, ok := <-events:
				if !ok {
					watchClosed = true
					break
				}
				if err := a.reloadWithTimeout(ctx); err != nil {
					// Keep the last-known-good policy set. The next event or
					// reconnect will retry the rebuild.
					a.logger.Error(err, "authorization policy reload failed", "resource", resourceName)
				}
			}
		}
		stopWatch()
		if !waitForRetry(ctx, backoff) {
			return
		}
	}
}

func (a *Authorizer) reloadWithTimeout(ctx context.Context) error {
	reloadCtx, cancel := context.WithTimeout(ctx, policyReloadTimeout)
	defer cancel()
	return a.Reload(reloadCtx)
}

func currentResourceVersion(ctx context.Context, store storage.ResourceStore) (string, error) {
	list, err := store.List(ctx, storage.ListOptions{})
	if err != nil {
		return "", err
	}
	accessor, err := meta.ListAccessor(list)
	if err != nil {
		return "", err
	}
	return accessor.GetResourceVersion(), nil
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
