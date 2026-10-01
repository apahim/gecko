package featureflags

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEvaluatorWithoutProviderReturnsDefault(t *testing.T) {
	t.Setenv("FLAGD_OFFLINE_FLAG_SOURCE_PATH", "")

	evaluator, err := NewFromEnvironment()
	if err != nil {
		t.Fatalf("NewFromEnvironment() error = %v", err)
	}

	got, err := evaluator.Boolean(context.Background(), "missing", true)
	if err != nil {
		t.Fatalf("Boolean() error = %v", err)
	}
	if !got {
		t.Fatalf("Boolean() = %v, want true", got)
	}
}

func TestEvaluatorReadsFlagdFile(t *testing.T) {
	flags := []byte(`{
  "$schema": "https://flagd.dev/schema/v0/flags.json",
  "flags": {
    "example.enabled": {
      "state": "ENABLED",
      "variants": {
        "enabled": true,
        "disabled": false
      },
      "defaultVariant": "disabled"
    }
  }
}`)
	path := filepath.Join(t.TempDir(), "flags.json")
	if err := os.WriteFile(path, flags, 0o600); err != nil {
		t.Fatalf("write flag file: %v", err)
	}

	t.Setenv("FLAGD_OFFLINE_FLAG_SOURCE_PATH", path)
	t.Setenv("FLAGD_OFFLINE_POLL_MS", "100")
	evaluator, err := NewFromEnvironment()
	if err != nil {
		t.Fatalf("NewFromEnvironment() error = %v", err)
	}
	defer evaluator.Shutdown()

	got, err := evaluator.Boolean(context.Background(), "example.enabled", true)
	if err != nil {
		t.Fatalf("Boolean() error = %v", err)
	}
	if got {
		t.Fatalf("Boolean() = %v, want false", got)
	}
}
