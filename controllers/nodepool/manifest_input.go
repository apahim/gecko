package nodepool

import (
	"github.com/openshift-online/gecko/controllers/nodepool/manifest"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
)

func manifestInput(np *privatev1.NodePool, cluster *privatev1.Cluster) manifest.Input {
	input := manifest.Input{NodePoolID: np.Name, NodePoolName: np.Name, NodePoolGeneration: np.Generation, ClusterID: string(cluster.UID), ClusterName: cluster.Spec.SafeName, Replicas: defaultReplicas, MachineType: manifest.DefaultMachineType, DiskSizeGB: manifest.DefaultDiskSizeGB, DiskType: manifest.DefaultDiskType, ReleaseImage: np.Status.VersionResolution.ReleaseImage}
	if gcp := np.Spec.Platform.GCP; gcp != nil {
		if gcp.MachineType != "" {
			input.MachineType = gcp.MachineType
		}
		if gcp.DiskSizeGB != 0 {
			input.DiskSizeGB = gcp.DiskSizeGB
		}
		if gcp.DiskType != "" {
			input.DiskType = gcp.DiskType
		}
		input.Zone = gcp.Zone
	}
	if gcp := cluster.Spec.Platform.GCP; gcp != nil {
		input.GCPRegion, input.GCPSubnet = gcp.Region, gcp.Subnet
		if gcp.IAM != nil {
			input.WorkerServiceAccount = gcp.IAM.WorkerServiceAccount
		}
	}
	if input.Zone == "" && input.GCPRegion != "" {
		input.Zone = input.GCPRegion + "-a"
	}
	if np.Spec.NodeCount != nil {
		input.Replicas = *np.Spec.NodeCount
	}
	if input.ClusterName == "" {
		input.ClusterName = privatev1.DefaultSafeName(cluster.Name, cluster.UID)
	}
	return input
}
