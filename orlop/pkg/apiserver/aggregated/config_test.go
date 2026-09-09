package aggregated

import (
	"strings"
	"testing"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

func validConfig() *Config {
	scheme := runtime.NewScheme()
	return &Config{
		Scheme: scheme,
		Resources: []types.ResourceInfo{
			{
				GVK:    runtimeschema.GroupVersionKind{Group: "test", Version: "v1", Kind: "Obj"},
				Plural: "objs",
			},
		},
		StorageFactory: func(string, *runtime.Scheme, runtimeschema.GroupVersionKind) (storage.ResourceStore, error) {
			return nil, nil
		},
	}
}

func TestCompleteDisableAuthForcesLocalhost(t *testing.T) {
	cfg := validConfig()
	cfg.DisableAuth = true

	completed, err := cfg.Complete()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed.BindAddress != "127.0.0.1" {
		t.Errorf("expected BindAddress 127.0.0.1, got %q", completed.BindAddress)
	}
}

func TestCompleteDisableAuthAllowsExplicitLocalhost(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "localhost"} {
		cfg := validConfig()
		cfg.DisableAuth = true
		cfg.BindAddress = addr

		completed, err := cfg.Complete()
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", addr, err)
		}
		if completed.BindAddress != "127.0.0.1" {
			t.Errorf("expected BindAddress 127.0.0.1 for input %q, got %q", addr, completed.BindAddress)
		}
	}
}

func TestCompleteDisableAuthRejectsNonLocalhost(t *testing.T) {
	for _, addr := range []string{"0.0.0.0", "10.0.0.1", "192.168.1.1"} {
		cfg := validConfig()
		cfg.DisableAuth = true
		cfg.BindAddress = addr

		_, err := cfg.Complete()
		if err == nil {
			t.Fatalf("expected error for DisableAuth with BindAddress %q", addr)
		}
		if !strings.Contains(err.Error(), "disable-auth requires binding to localhost") {
			t.Errorf("unexpected error message for %q: %v", addr, err)
		}
	}
}

func TestCompleteWithoutDisableAuthAllowsAnyAddress(t *testing.T) {
	cfg := validConfig()
	cfg.BindAddress = "0.0.0.0"

	completed, err := cfg.Complete()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed.BindAddress != "0.0.0.0" {
		t.Errorf("expected BindAddress 0.0.0.0, got %q", completed.BindAddress)
	}
}

func TestCompleteDefaultBindAddress(t *testing.T) {
	cfg := validConfig()

	completed, err := cfg.Complete()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed.BindAddress != "0.0.0.0" {
		t.Errorf("expected default BindAddress 0.0.0.0, got %q", completed.BindAddress)
	}
}
