package garaccess

import (
	"fmt"
	"regexp"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
)

var projectNumberPattern = regexp.MustCompile(`^[1-9][0-9]{0,24}$`)

// customerPrincipalSet uses accepted Cluster setup data. This flow assumes the
// WIF pool and worker resources belong to the same customer project.
func customerPrincipalSet(cluster *privatev1.Cluster) (string, error) {
	if cluster == nil || cluster.Spec.Platform.GCP == nil {
		return "", fmt.Errorf("cluster must specify its GCP project")
	}
	gcp := cluster.Spec.Platform.GCP
	if cluster.Namespace == "" || gcp.ProjectID != cluster.Namespace {
		return "", fmt.Errorf("cluster GCP project must match its customer namespace")
	}
	projectNumber := gcp.WorkloadIdentity.ProjectNumber
	if !projectNumberPattern.MatchString(projectNumber) {
		return "", fmt.Errorf("cluster project number must be a positive decimal without leading zeros")
	}
	return fmt.Sprintf("principalSet://cloudresourcemanager.googleapis.com/projects/%s/type/ServiceAccount", projectNumber), nil
}
