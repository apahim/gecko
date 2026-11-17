package upgradepolicy

import (
	"time"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilversion "k8s.io/apimachinery/pkg/util/version"
)

// HealthOK reports whether a new upgrade target may be requested. Both z-stream and
// y-stream upgrades require HostedCluster availability and positive confirmation of
// no degradation. Y-stream upgrades additionally require ClusterVersionUpgradeable=True.
// An absent or Unknown HostedClusterDegraded reading blocks selection just like a
// confirmed True does — deny by default when there isn't enough evidence to confirm
// the cluster is healthy.
func HealthOK(clusterConditions, observed []metav1.Condition, isYStream bool) (ok bool, reason, message string) {
	if !meta.IsStatusConditionTrue(clusterConditions, "HostedClusterAvailable") {
		return false, "HostedClusterUnavailable", "HostedCluster must be available before an upgrade can start"
	}
	degraded := meta.FindStatusCondition(observed, "HostedClusterDegraded")
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		message := "HostedCluster has not reported HostedClusterDegraded=False"
		if degraded != nil && degraded.Message != "" {
			message = degraded.Message
		}
		return false, "HostedClusterDegraded", message
	}

	// Y-stream upgrades require ClusterVersionUpgradeable=True
	if isYStream {
		upgradeable := meta.FindStatusCondition(observed, "ClusterVersionUpgradeable")
		if upgradeable == nil || upgradeable.Status != metav1.ConditionTrue {
			message := "Y-stream upgrade requires ClusterVersionUpgradeable=True"
			if upgradeable != nil && upgradeable.Message != "" {
				message = upgradeable.Message
			}
			return false, "ClusterVersionNotUpgradeable", message
		}
	}

	return true, "", ""
}

// AutomaticAuthorizationHolds evaluates the selected automatic target immediately
// before dispatch, or while the upgrade controller is still preparing it.
func AutomaticAuthorizationHolds(cluster *privatev1.Cluster, now time.Time) (bool, string, string) {
	result, hc := cluster.Status.ControlPlaneUpgrade, cluster.Status.HostedClusterResult
	if result == nil || hc == nil {
		return false, "WaitingForHostedClusterFeedback", "Waiting for the selected target and observed control-plane version"
	}
	current, err := utilversion.ParseSemantic(hc.Version)
	if err != nil {
		return false, "InvalidObservedVersion", "The observed control-plane version is invalid"
	}
	selected, err := utilversion.ParseSemantic(result.TargetVersion)
	if err != nil {
		return false, "InvalidTargetVersion", "The selected control-plane version is invalid"
	}
	isYStream := current.Major() != selected.Major() || current.Minor() != selected.Minor()
	if ok, reason, message := HealthOK(cluster.Status.Conditions, hc.ObservedConditions, isYStream); !ok {
		return false, reason, message
	}
	if override, _, _ := OverrideCondition(cluster, result.TargetVersion, hc.Version); !override {
		if permitted, reason, message, _ := MaintenancePermitted(cluster, now); !permitted {
			return false, reason, message
		}
	}
	return true, "", ""
}

// IsValidCriticalPin restricts maintenance overrides to strictly newer patches
// in the observed release's major.minor stream.
func IsValidCriticalPin(pin, completed string) bool {
	current, err := utilversion.ParseSemantic(completed)
	if err != nil {
		return false
	}
	target, err := utilversion.ParseSemantic(pin)
	return err == nil && current.Major() == target.Major() &&
		current.Minor() == target.Minor() && current.LessThan(target)
}
