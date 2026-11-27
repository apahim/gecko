// Package manifest provides the manifest builder for the nodepool controller.
package manifest

import (
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	// DefaultDiskSizeGB is the default disk size in GB for GCP node pool boot disks.
	DefaultDiskSizeGB = int64(100)
	// DefaultDiskType is the default disk type for GCP node pool boot disks.
	DefaultDiskType = "pd-ssd"
	// DefaultMachineType is the default GCP machine type for node pool instances.
	DefaultMachineType = "n2-standard-4"

	defaultReplicas = int32(1)
)

// Input holds all parameters for building the NodePool manifests.
type Input struct {
	NodePoolID         string
	NodePoolName       string
	NodePoolGeneration int64
	ClusterID          string
	ClusterName        string
	Replicas           int32
	MachineType        string // e.g. "n2-standard-4"
	GCPRegion          string
	Zone               string // optional; falls back to GCPRegion+"-a"
	GCPSubnet          string
	DiskSizeGB         int64  // default: 100
	DiskType           string // default: "pd-ssd"
	ReleaseImage       string
}

// Build constructs raw JSON for the NodePool and its worker configuration.
func Build(input Input) ([][]byte, error) {
	// Apply defaults
	zone := input.Zone
	if zone == "" {
		zone = input.GCPRegion + "-a"
	}

	diskSizeGB := input.DiskSizeGB
	if diskSizeGB == 0 {
		diskSizeGB = DefaultDiskSizeGB
	}

	diskType := input.DiskType
	if diskType == "" {
		diskType = DefaultDiskType
	}

	machineType := input.MachineType
	if machineType == "" {
		machineType = DefaultMachineType
	}

	replicas := input.Replicas
	if replicas == 0 {
		replicas = defaultReplicas
	}

	genStr := strconv.FormatInt(input.NodePoolGeneration, 10)
	namespace := fmt.Sprintf("clusters-%s", input.ClusterID)
	helperConfig, err := credentialHelperConfig(namespace, input.NodePoolName)
	if err != nil {
		return nil, fmt.Errorf("nodepool manifest: %w", err)
	}

	// Build the NodePool manifest as map[string]any
	nodePoolManifest := map[string]any{
		"apiVersion": "hypershift.openshift.io/v1beta1",
		"kind":       "NodePool",
		"metadata": map[string]any{
			"name":      input.NodePoolName,
			"namespace": namespace,
			"labels": map[string]any{
				"gcp.managed.openshift.io/cluster-id":  input.ClusterID,
				"gcp.managed.openshift.io/nodepool-id": input.NodePoolID,
				"gcp.managed.openshift.io/managed-by":  "nodepool-controller",
			},
			"annotations": map[string]any{
				"gcp.managed.openshift.io/generation": genStr,
			},
		},
		"spec": map[string]any{
			"clusterName": input.ClusterName,
			"replicas":    replicas,
			"config":      []map[string]string{{"name": helperConfig.Name}},
			"release": map[string]any{
				"image": input.ReleaseImage,
			},
			"arch": "amd64",
			"management": map[string]any{
				"autoRepair":  true,
				"upgradeType": "Replace",
				"replace": map[string]any{
					"strategy": "RollingUpdate",
					"rollingUpdate": map[string]any{
						"maxSurge":       1,
						"maxUnavailable": 0,
					},
				},
			},
			"platform": map[string]any{
				"type": "GCP",
				"gcp": map[string]any{
					"machineType": machineType,
					"zone":        zone,
					"subnet":      input.GCPSubnet,
					"bootDisk": map[string]any{
						"diskSizeGB": diskSizeGB,
						"diskType":   diskType,
					},
				},
			},
		},
	}

	rawBytes, err := json.Marshal(nodePoolManifest)
	if err != nil {
		return nil, fmt.Errorf("nodepool manifest: marshal NodePool: %w", err)
	}

	helperConfigBytes, err := json.Marshal(helperConfig)
	if err != nil {
		return nil, fmt.Errorf("nodepool manifest: marshal credential helper ConfigMap: %w", err)
	}
	return [][]byte{rawBytes, helperConfigBytes}, nil
}
