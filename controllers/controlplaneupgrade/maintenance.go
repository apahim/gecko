package controlplaneupgrade

import (
	"time"

	"github.com/openshift-online/gecko/controllers/util/upgradepolicy"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
)

func maintenancePermitted(cluster *privatev1.Cluster, now time.Time) (bool, string, string, *time.Time) {
	return upgradepolicy.MaintenancePermitted(cluster, now)
}

func overrideCondition(cluster *privatev1.Cluster, candidate, completed string) (bool, string, string) {
	return upgradepolicy.OverrideCondition(cluster, candidate, completed)
}
