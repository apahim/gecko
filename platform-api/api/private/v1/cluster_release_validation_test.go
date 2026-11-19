package v1

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func releaseValidationCluster() *Cluster {
	cluster := &Cluster{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "customer", UID: "test-uid"}}
	cluster.Spec.SafeName = DefaultSafeName(cluster.Name, cluster.UID)
	cluster.Spec.Release = ClusterReleaseSpec{Version: "4.22.14", ChannelGroup: "stable"}
	cluster.Status.HostedClusterResult = &HostedClusterResult{
		Version: "4.22.14", DesiredVersion: "4.22.14", AvailableUpdates: []string{"4.22.15"},
	}
	return cluster
}

func TestClusterCreateReleaseCatalogValidation(t *testing.T) {
	cluster := releaseValidationCluster()
	SetValidatorDeps(ValidatorDeps{ValidateClusterRelease: func(_ context.Context, version, channelGroup string) error {
		if version != "4.22.14" || channelGroup != "stable" {
			t.Fatalf("unexpected validation input: %s %s", version, channelGroup)
		}
		return errors.New("version 4.22.14 is not present in the Version catalog")
	}})
	t.Cleanup(func() { SetValidatorDeps(ValidatorDeps{}) })

	if err := cluster.ValidateCreate(t.Context()); err == nil ||
		err.Error() != "version 4.22.14 is not present in the Version catalog" {
		t.Fatalf("create error = %v, want catalog rejection", err)
	}

	SetValidatorDeps(ValidatorDeps{})
	if err := cluster.ValidateCreate(t.Context()); err != nil {
		t.Fatalf("create without storage-backed checker: %v", err)
	}
}

func TestClusterUpdateReleaseCatalogValidation(t *testing.T) {
	validations := 0
	SetValidatorDeps(ValidatorDeps{ValidateClusterRelease: func(_ context.Context, version, channelGroup string) error {
		validations++
		if version != "4.22.15" || channelGroup != "stable" {
			t.Fatalf("unexpected validation input: %s %s", version, channelGroup)
		}
		return nil
	}})
	t.Cleanup(func() { SetValidatorDeps(ValidatorDeps{}) })

	old := releaseValidationCluster()

	// Unrelated spec changes and rollbacks must not consult the catalog.
	rolled := old.DeepCopy()
	rolled.Spec.Release.Version = old.Status.HostedClusterResult.Version
	if err := rolled.ValidateUpdate(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	canceled := old.DeepCopy()
	if err := canceled.ValidateUpdate(t.Context(), old); err != nil {
		t.Fatal(err)
	}

	// A new upgrade request must be checked against the catalog.
	upgraded := old.DeepCopy()
	upgraded.Spec.Release.Version = "4.22.15"
	if err := upgraded.ValidateUpdate(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if validations != 1 {
		t.Fatalf("storage-backed checks = %d, want 1", validations)
	}
}

func TestClusterUpdateReleaseCatalogRejection(t *testing.T) {
	SetValidatorDeps(ValidatorDeps{ValidateClusterRelease: func(_ context.Context, _, _ string) error {
		return errors.New("version 4.22.15 is not published in channel group stable")
	}})
	t.Cleanup(func() { SetValidatorDeps(ValidatorDeps{}) })

	old := releaseValidationCluster()
	upgraded := old.DeepCopy()
	upgraded.Spec.Release.Version = "4.22.15"
	if err := upgraded.ValidateUpdate(t.Context(), old); err == nil ||
		err.Error() != "version 4.22.15 is not published in channel group stable" {
		t.Fatalf("update error = %v, want catalog rejection", err)
	}
}

func TestClusterUnobservedReleaseChangeUsesCatalog(t *testing.T) {
	validations := 0
	SetValidatorDeps(ValidatorDeps{ValidateClusterRelease: func(_ context.Context, version, channelGroup string) error {
		validations++
		if version != "4.22.15" || channelGroup != "stable" {
			t.Fatalf("unexpected validation input: %s %s", version, channelGroup)
		}
		return nil
	}})
	t.Cleanup(func() { SetValidatorDeps(ValidatorDeps{}) })

	old := releaseValidationCluster()
	old.Status.HostedClusterResult = nil
	reconfigured := old.DeepCopy()
	reconfigured.Spec.Release.Version = "4.22.15"
	if err := reconfigured.ValidateUpdate(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if validations != 1 {
		t.Fatalf("storage-backed checks = %d, want 1", validations)
	}
}
