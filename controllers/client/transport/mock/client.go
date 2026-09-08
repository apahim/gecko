package mock

import (
	"context"
	"sync"

	"github.com/openshift-online/gecko/controllers/client/transport"
)

// ApplyCall records arguments from an Apply call.
type ApplyCall struct {
	TargetCluster string
	GroupKey      string
	Manifests     [][]byte
}

// DeleteCall records arguments from a Delete call.
type DeleteCall struct {
	TargetCluster string
	GroupKey      string
}

// CleanupDeleteDesiresCall records arguments from a CleanupDeleteDesires call.
type CleanupDeleteDesiresCall struct {
	TargetCluster string
	GroupKey      string
}

// Client is an in-memory implementation of transport.Client for use in tests.
type Client struct {
	mu sync.RWMutex

	// StatusOverrides allows tests to inject a specific status for Apply/GetStatus calls.
	// Key format: "targetCluster/groupKey".
	StatusOverrides map[string]*transport.Status

	// DeleteStatusOverrides allows tests to inject a specific delete status.
	// Key format: "targetCluster/groupKey".
	DeleteStatusOverrides map[string]*transport.DeleteStatus

	// ApplyCalls records all Apply invocations for test assertions.
	ApplyCalls []ApplyCall

	// DeleteCalls records all Delete invocations for test assertions.
	DeleteCalls []DeleteCall

	// CleanupDeleteDesiresCalls records all CleanupDeleteDesires invocations.
	CleanupDeleteDesiresCalls []CleanupDeleteDesiresCall

	// DeleteErr, if non-nil, is returned by Delete.
	DeleteErr error

	// CleanupDeleteDesiresErr, if non-nil, is returned by CleanupDeleteDesires.
	CleanupDeleteDesiresErr error
}

// Ensure Client implements transport.Client.
var _ transport.Client = (*Client)(nil)

// New creates a new in-memory mock Client.
func New() *Client {
	return &Client{
		StatusOverrides:       make(map[string]*transport.Status),
		DeleteStatusOverrides: make(map[string]*transport.DeleteStatus),
	}
}

func storeKey(targetCluster, groupKey string) string {
	return targetCluster + "/" + groupKey
}

// Apply records the call and returns any configured status override.
func (c *Client) Apply(ctx context.Context, targetCluster, groupKey string, manifests [][]byte) (*transport.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ApplyCalls = append(c.ApplyCalls, ApplyCall{
		TargetCluster: targetCluster,
		GroupKey:      groupKey,
		Manifests:     manifests,
	})

	key := storeKey(targetCluster, groupKey)
	if override, ok := c.StatusOverrides[key]; ok {
		return override, nil
	}
	return &transport.Status{}, nil
}

// GetStatus returns any configured status override, or an empty status.
func (c *Client) GetStatus(ctx context.Context, targetCluster, groupKey string) (*transport.Status, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := storeKey(targetCluster, groupKey)
	if override, ok := c.StatusOverrides[key]; ok {
		return override, nil
	}
	return &transport.Status{}, nil
}

// Delete records the call. Always succeeds.
func (c *Client) Delete(ctx context.Context, targetCluster, groupKey string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.DeleteCalls = append(c.DeleteCalls, DeleteCall{
		TargetCluster: targetCluster,
		GroupKey:      groupKey,
	})
	return c.DeleteErr
}

// GetDeleteStatus returns any configured delete status override, or AllSuccessful=true by default.
func (c *Client) GetDeleteStatus(ctx context.Context, targetCluster, groupKey string) (*transport.DeleteStatus, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := storeKey(targetCluster, groupKey)
	if override, ok := c.DeleteStatusOverrides[key]; ok {
		return override, nil
	}
	// Default: deletion complete (no DeleteDesires, no ApplyDesires).
	return &transport.DeleteStatus{AllSuccessful: true, PendingCount: 0, TotalCount: 0, ApplyDesiresCount: 0}, nil
}

// CleanupDeleteDesires records the call and returns any configured error.
func (c *Client) CleanupDeleteDesires(ctx context.Context, targetCluster, groupKey string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.CleanupDeleteDesiresCalls = append(c.CleanupDeleteDesiresCalls, CleanupDeleteDesiresCall{
		TargetCluster: targetCluster,
		GroupKey:      groupKey,
	})
	return c.CleanupDeleteDesiresErr
}

// Reset clears all stored state and recorded calls. Useful between test cases.
func (c *Client) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.StatusOverrides = make(map[string]*transport.Status)
	c.DeleteStatusOverrides = make(map[string]*transport.DeleteStatus)
	c.ApplyCalls = nil
	c.DeleteCalls = nil
	c.CleanupDeleteDesiresCalls = nil
	c.DeleteErr = nil
	c.CleanupDeleteDesiresErr = nil
}
