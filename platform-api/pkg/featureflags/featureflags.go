// Package featureflags provides Gecko's provider-neutral feature flag access.
package featureflags

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	flagd "github.com/open-feature/go-sdk-contrib/providers/flagd/pkg"
	"github.com/open-feature/go-sdk/openfeature"
)

const defaultOfflinePollMs = 5000

// Evaluator evaluates feature flags through OpenFeature.
//
// The provider is configured from the environment so deployment configuration
// remains independent of the consuming feature. With no flagd file path set,
// evaluations return the caller's supplied defaults.
type Evaluator struct {
	client      *openfeature.Client `json:"-"`
	initialized bool                `json:"-"`
}

// NewFromEnvironment initializes the flagd file provider when
// FLAGD_OFFLINE_FLAG_SOURCE_PATH is configured.
func NewFromEnvironment() (*Evaluator, error) {
	path := strings.TrimSpace(os.Getenv("FLAGD_OFFLINE_FLAG_SOURCE_PATH"))
	if path == "" {
		return &Evaluator{}, nil
	}

	pollMs := defaultOfflinePollMs
	if raw := strings.TrimSpace(os.Getenv("FLAGD_OFFLINE_POLL_MS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("FLAGD_OFFLINE_POLL_MS must be a positive integer: %q", raw)
		}
		pollMs = parsed
	}

	provider, err := flagd.NewProvider(
		flagd.WithFileResolver(),
		flagd.WithOfflineFilePath(path),
		flagd.WithOfflinePollMs(pollMs),
	)
	if err != nil {
		return nil, fmt.Errorf("create flagd provider: %w", err)
	}
	if err := openfeature.SetProviderAndWait(provider); err != nil {
		return nil, fmt.Errorf("initialize flagd provider: %w", err)
	}

	return &Evaluator{
		client:      openfeature.NewDefaultClient(),
		initialized: true,
	}, nil
}

// Boolean evaluates a Boolean flag. When no provider is configured, the
// supplied default is returned without error.
func (e *Evaluator) Boolean(ctx context.Context, key string, defaultValue bool) (bool, error) {
	if e == nil || e.client == nil {
		return defaultValue, nil
	}
	return e.client.BooleanValue(ctx, key, defaultValue, openfeature.EvaluationContext{})
}

// Shutdown releases the provider's file watcher and OpenFeature resources.
func (e *Evaluator) Shutdown() {
	if e != nil && e.initialized {
		openfeature.Shutdown()
	}
}
