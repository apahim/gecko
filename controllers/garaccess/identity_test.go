package garaccess

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCustomerPrincipalSet(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		project   string
		number    string
		wantErr   string
	}{
		{name: "canonical project principal", namespace: "customer-project", project: "customer-project", number: "123456789012"},
		{name: "smallest project number", namespace: "customer-project", project: "customer-project", number: "1"},
		{name: "25 digit project number", namespace: "customer-project", project: "customer-project", number: strings.Repeat("9", 25)},
		{name: "namespace mismatch", namespace: "other-project", project: "customer-project", number: "123", wantErr: "must match"},
		{name: "missing namespace", project: "customer-project", number: "123", wantErr: "must match"},
		{name: "missing project", namespace: "customer-project", number: "123", wantErr: "must match"},
		{name: "both project and namespace missing", number: "123", wantErr: "must match"},
		{name: "missing project number", namespace: "customer-project", project: "customer-project", wantErr: "positive decimal"},
		{name: "zero project number", namespace: "customer-project", project: "customer-project", number: "0", wantErr: "positive decimal"},
		{name: "leading zeros", namespace: "customer-project", project: "customer-project", number: "0123", wantErr: "positive decimal"},
		{name: "negative number", namespace: "customer-project", project: "customer-project", number: "-123", wantErr: "positive decimal"},
		{name: "explicit plus sign", namespace: "customer-project", project: "customer-project", number: "+123", wantErr: "positive decimal"},
		{name: "fractional number", namespace: "customer-project", project: "customer-project", number: "1.5", wantErr: "positive decimal"},
		{name: "nondecimal number", namespace: "customer-project", project: "customer-project", number: "0xff", wantErr: "positive decimal"},
		{name: "whitespace", namespace: "customer-project", project: "customer-project", number: "123 ", wantErr: "positive decimal"},
		{name: "26 digit project number", namespace: "customer-project", project: "customer-project", number: strings.Repeat("9", 26), wantErr: "positive decimal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := identityCluster(tt.project, tt.number)
			cluster.Namespace = tt.namespace
			got, err := customerPrincipalSet(cluster)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "principalSet://cloudresourcemanager.googleapis.com/projects/"+tt.number+"/type/ServiceAccount", got)
		})
	}
}

func TestCustomerPrincipalSetMissingGCP(t *testing.T) {
	tests := []struct {
		name    string
		cluster *privatev1.Cluster
	}{
		{name: "nil Cluster"},
		{name: "nil GCP", cluster: &privatev1.Cluster{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := customerPrincipalSet(tt.cluster)
			require.ErrorContains(t, err, "must specify its GCP project")
			require.Empty(t, got)
		})
	}
}

func identityCluster(project, number string) *privatev1.Cluster {
	return &privatev1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: project},
		Spec: privatev1.ClusterSpec{Platform: privatev1.ClusterPlatformSpec{
			Type: "GCP",
			GCP: &privatev1.GCPClusterPlatform{
				ProjectID:        project,
				WorkloadIdentity: privatev1.WorkloadIdentitySpec{ProjectNumber: number},
			},
		}},
	}
}
