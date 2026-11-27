package manifest

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//go:embed assets/docker-credential-gcp
var credentialHelper []byte

// credentialHelperConfig delivers the executable without configuring any caller to use it.
func credentialHelperConfig(namespace, nodePoolName string) (*corev1.ConfigMap, error) {
	machineConfig := map[string]any{
		"apiVersion": "machineconfiguration.openshift.io/v1",
		"kind":       "MachineConfig",
		"metadata": map[string]any{
			"name":   "gcp-worker-credential-helper",
			"labels": map[string]string{"machineconfiguration.openshift.io/role": "worker"},
		},
		"spec": map[string]any{"config": map[string]any{
			"ignition": map[string]string{"version": "3.2.0"},
			"storage": map[string]any{"files": []map[string]any{{
				"path":      "/usr/local/bin/docker-credential-gcp",
				"mode":      0o755,
				"overwrite": true,
				"user":      map[string]string{"name": "root"},
				"group":     map[string]string{"name": "root"},
				"contents":  map[string]string{"source": "data:;base64," + base64.StdEncoding.EncodeToString(credentialHelper)},
			}}},
		}},
	}
	config, err := json.Marshal(machineConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal credential helper MachineConfig: %w", err)
	}

	// Give each pool its own stable ConfigMap so its transport group can delete it independently.
	hash := sha256.Sum256([]byte(nodePoolName))
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("gcp-worker-credential-helper-%x", hash[:12]),
			Namespace: namespace,
		},
		Data: map[string]string{"config": string(config)},
	}, nil
}
