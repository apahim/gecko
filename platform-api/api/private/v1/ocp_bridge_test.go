package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodePool422To500UpgradeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, current, target, controlPlane, group string
		allowed                                    bool
	}{
		{"next minor bridge", "4.22.21", "5.0.0", "5.0.0", "candidate", true},
		{"workers ahead of control plane", "4.22.21", "5.0.0", "4.22.21", "candidate", false},
		{"normalized downgrade", "4.23.1", "5.0.0", "5.0.2", "candidate", false},
		{"same release alias", "4.23.0", "5.0.0", "5.0.0", "candidate", false},
		{"nonsequential minor", "4.21.1", "5.0.0", "5.0.0", "candidate", false},
		{"unknown major", "5.0.0", "6.0.0", "6.0.0", "candidate", false},
		{"existing patch", "5.0.0", "5.0.2", "5.0.2", "candidate", true},
		{"prerelease to final", "4.22.21", "5.0.0-rc.8", "5.0.0", "candidate", true},
		{"alias patch forward", "5.0.0", "4.23.1", "5.0.2", "candidate", true},
		{"EUS bridge", "4.22.21", "5.1.0", "5.1.0", "eus", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := upgradeValidationPool()
			pool.Status.Version = tc.current
			pool.Spec.Release.ChannelGroup = tc.group
			cluster := &Cluster{Status: ClusterStatus{HostedClusterResult: &HostedClusterResult{Version: tc.controlPlane}}}
			release := &Version{ObjectMeta: metav1.ObjectMeta{Name: tc.target}, Spec: VersionSpec{ChannelGroups: []string{tc.group}}}
			err := ValidateNodePoolUpgradeTarget(pool, cluster, release, tc.target)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%t, got %v", tc.allowed, err)
			}
		})
	}
}

func TestClusterBridgeStillRequiresExactAdvertisedTarget(t *testing.T) {
	cluster := &Cluster{Status: ClusterStatus{HostedClusterResult: &HostedClusterResult{Version: "4.22.21", AvailableUpdates: []string{"5.0.0"}}}}
	if err := ValidateClusterUpgradeTarget(cluster, "5.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"5.0.1", "4.23.0"} {
		if err := ValidateClusterUpgradeTarget(cluster, target); err == nil {
			t.Fatalf("unadvertised raw identity %q accepted", target)
		}
	}
	cluster.Status.HostedClusterResult.Version = "4.23.1"
	if err := ValidateClusterUpgradeTarget(cluster, "5.0.0"); err == nil {
		t.Fatal("normalized downgrade accepted")
	}
	cluster.Status.HostedClusterResult.Version = "5.0.0"
	cluster.Status.HostedClusterResult.AvailableUpdates = []string{"6.0.0"}
	if err := ValidateClusterUpgradeTarget(cluster, "6.0.0"); err == nil {
		t.Fatal("unsupported major accepted")
	}
}

func TestBridgeDoesNotNormalizeWIFBoundaryIdentity(t *testing.T) {
	templates := []WIFConfigTemplate{
		{ObjectMeta: metav1.ObjectMeta{Name: "4.22.0", UID: "source"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "4.23.0", UID: "alias"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "5.0.0", UID: "target"}},
	}
	for release, want := range map[string]string{"4.22.21": "4.22.0", "4.23.0": "4.23.0", "5.0.0": "5.0.0", "5.0.0-rc.8": "5.0.0"} {
		got, err := SelectWIFConfigTemplate(release, templates)
		if err != nil || got == nil || got.Name != want {
			t.Fatalf("raw WIF boundary %s: %+v %v", release, got, err)
		}
	}
}
