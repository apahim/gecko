package controlplaneupgrade

import (
	"testing"
	"time"

	"github.com/openshift-online/gecko/controllers/util/catalog/catalogtest"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBridgeCandidateSelection(t *testing.T) {
	got, err := selectYStreamCandidate("4.22.21", []string{"5.0.0", "5.1.0"}, "5.0")
	if err != nil || got != "5.0.0" {
		t.Fatalf("next-minor bridge: %q %v", got, err)
	}
	got, err = selectYStreamCandidate("4.21.9", []string{"5.0.0"}, "5.0")
	if err != nil || got != "" {
		t.Fatalf("skipped minor accepted: %q %v", got, err)
	}
	got, err = selectZStreamCandidate("4.23.1", []string{"5.0.0", "5.0.2", "6.0.0"})
	if err != nil || got != "5.0.2" {
		t.Fatalf("same logical stream: %q %v", got, err)
	}
	if isValidCriticalPin("5.0.1", "4.23.0") {
		t.Fatal("bridge must not expand raw critical-pin bypass authority")
	}
	for _, advertised := range [][]string{{"4.23.2", "5.0.2"}, {"5.0.2", "4.23.2"}} {
		got, err = selectZStreamCandidate("5.0.1", advertised)
		if err != nil || got != "5.0.2" {
			t.Fatalf("alias tie not deterministic: %q %v", got, err)
		}
	}
	got, err = selectZStreamCandidate("4.23.7", []string{"5.0.1"})
	if err != nil || got != "" {
		t.Fatalf("normalized downgrade selected: %q %v", got, err)
	}
}

func TestBridgeApprovalNeedsRawTargetAssertionAndAutomaticMaintenance(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "customer"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			cluster := readyCluster("4.22.21", []string{"5.0.0"})
			cluster.Status.HostedClusterResult.ObservedConditions = append(cluster.Status.HostedClusterResult.ObservedConditions, metav1.Condition{Type: "ClusterVersionUpgradeable", Status: metav1.ConditionTrue})
			if !automatic {
				cluster.Spec.Release.Version = "5.0.0"
			}
			r, store := buildReconciler(t, cluster)
			store.persistUpdates = true
			store.channel.Spec.FleetMinorVersion = "5.0"
			source := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.22.21"}, Spec: privatev1.VersionSpec{ReleaseImage: "source-payload", ChannelGroups: []string{"stable"}}}
			target := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "5.0.0"}, Spec: privatev1.VersionSpec{ReleaseImage: "target-payload", ChannelGroups: []string{"stable"}}}
			catalogtest.Ready(t, source)
			catalogtest.Ready(t, target)
			if source.Spec.WIFConfigTemplateRef.Digest != target.Spec.WIFConfigTemplateRef.Digest {
				t.Fatal("fixture permissions differ")
			}
			store.releases = []privatev1.Version{*source, *target}
			store.cluster.Spec.Platform.GCP.IAM.TemplateApplications = []privatev1.WIFConfigTemplateReference{*source.Spec.WIFConfigTemplateRef}
			if _, err := r.Reconcile(t.Context(), clusterReq(cluster.Name)); err != nil {
				t.Fatal(err)
			}
			if privatev1.ClusterInstallationVersion(store.cluster) != "4.22.21" || meta.FindStatusCondition(store.cluster.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted).Reason != privatev1.WIFConfigUpdateRequired {
				t.Fatal("equal digest bypassed distinct target assertion")
			}
			catalogtest.Apply(store.cluster, target.Spec.WIFConfigTemplateRef)
			if automatic {
				exclusions := []privatev1.ControlPlaneMaintenanceExclusion{{Name: "hold", StartTime: metav1.NewTime(time.Now().Add(-time.Hour)), EndTime: metav1.NewTime(time.Now().Add(time.Hour))}}
				store.cluster.Spec.Maintenance = &privatev1.ClusterMaintenanceSpec{Exclusions: &exclusions}
				if _, err := r.Reconcile(t.Context(), clusterReq(cluster.Name)); err != nil {
					t.Fatal(err)
				}
				if privatev1.ClusterReleaseApproved(store.cluster) && store.cluster.Spec.Release.Version != "4.22.21" {
					t.Fatal("automatic bridge bypassed maintenance")
				}
				if meta.FindStatusCondition(store.cluster.Status.Conditions, conditionProgressing).Reason != "MaintenanceExclusionActive" {
					t.Fatal("automatic maintenance hold not observed")
				}
				store.cluster.Spec.Maintenance = nil
			}
			for i := 0; i < 2; i++ {
				if _, err := r.Reconcile(t.Context(), clusterReq(cluster.Name)); err != nil {
					t.Fatal(err)
				}
			}
			if !privatev1.ClusterReleaseApproved(store.cluster) || store.cluster.Spec.Release.Version != "5.0.0" || privatev1.ClusterInstallationVersion(store.cluster) != "5.0.0" {
				t.Fatal("target not approved with exact raw identity")
			}
			if store.cluster.Status.ControlPlaneUpgrade.TargetVersion != "5.0.0" || target.Spec.ReleaseImage != "target-payload" || target.Spec.WIFConfigTemplateRef.Name != "5.0.0" {
				t.Fatal("comparison normalization leaked into dispatch identity")
			}
		})
	}
}
