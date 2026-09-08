package transport

import (
	"testing"
)

func TestGroupKey_ClusterGroupKey(t *testing.T) {
	t.Run("valid inputs", func(t *testing.T) {
		key, err := ClusterGroupKey("my-namespace", "my-cluster")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "projects/my-namespace/clusters/my-cluster"
		if key != want {
			t.Errorf("got %q, want %q", key, want)
		}
	})

	t.Run("empty namespace", func(t *testing.T) {
		_, err := ClusterGroupKey("", "my-cluster")
		if err == nil {
			t.Fatal("expected error for empty namespace, got nil")
		}
	})

	t.Run("empty clusterName", func(t *testing.T) {
		_, err := ClusterGroupKey("my-namespace", "")
		if err == nil {
			t.Fatal("expected error for empty clusterName, got nil")
		}
	})
}

func TestGroupKey_NodePoolGroupKey(t *testing.T) {
	t.Run("valid inputs", func(t *testing.T) {
		key, err := NodePoolGroupKey("my-namespace", "my-cluster", "my-nodepool")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "projects/my-namespace/clusters/my-cluster/nodepools/my-nodepool"
		if key != want {
			t.Errorf("got %q, want %q", key, want)
		}
	})

	t.Run("empty namespace", func(t *testing.T) {
		_, err := NodePoolGroupKey("", "my-cluster", "my-nodepool")
		if err == nil {
			t.Fatal("expected error for empty namespace, got nil")
		}
	})

	t.Run("empty clusterName", func(t *testing.T) {
		_, err := NodePoolGroupKey("my-namespace", "", "my-nodepool")
		if err == nil {
			t.Fatal("expected error for empty clusterName, got nil")
		}
	})

	t.Run("empty nodePoolName", func(t *testing.T) {
		_, err := NodePoolGroupKey("my-namespace", "my-cluster", "")
		if err == nil {
			t.Fatal("expected error for empty nodePoolName, got nil")
		}
	})
}
