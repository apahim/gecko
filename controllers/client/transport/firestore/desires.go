package firestore

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/kube-applier-gcp/pkg/api/kubeapplier"
	"github.com/openshift-online/kube-applier-gcp/pkg/desireid"
)

// kindToResource maps well-known Kind strings to their plural Kubernetes resource names.
var kindToResource = map[string]string{
	"Namespace":      "namespaces",
	"ExternalSecret": "externalsecrets",
	"Certificate":    "certificates",
	"HostedCluster":  "hostedclusters",
	"NodePool":       "nodepools",
	"Job":            "jobs",
	"ConfigMap":      "configmaps",
	"Secret":         "secrets",
}

type parsedManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// parseManifest extracts a ResourceReference from a raw manifest JSON byte slice.
// Derives group/version by splitting apiVersion on "/" (core group has no "/").
// Maps Kind to plural resource name using kindToResource; falls back to
// lowercasing the Kind and appending "s" for unknown kinds.
// unknownKind is true when the fallback was used — callers should log a warning.
func parseManifest(raw []byte) (ref kubeapplier.ResourceReference, unknownKind bool, err error) {
	var pm parsedManifest
	if err := json.Unmarshal(raw, &pm); err != nil {
		return kubeapplier.ResourceReference{}, false, fmt.Errorf("parse manifest: unmarshal: %w", err)
	}
	if pm.APIVersion == "" {
		return kubeapplier.ResourceReference{}, false, fmt.Errorf("parse manifest: missing apiVersion")
	}
	if pm.Kind == "" {
		return kubeapplier.ResourceReference{}, false, fmt.Errorf("parse manifest: missing kind")
	}
	if pm.Metadata.Name == "" {
		return kubeapplier.ResourceReference{}, false, fmt.Errorf("parse manifest: missing metadata.name")
	}

	var group, version string
	if idx := strings.Index(pm.APIVersion, "/"); idx >= 0 {
		group = pm.APIVersion[:idx]
		version = pm.APIVersion[idx+1:]
	} else {
		// Core group (e.g. "v1")
		group = ""
		version = pm.APIVersion
	}

	resource, ok := kindToResource[pm.Kind]
	if !ok {
		resource = strings.ToLower(pm.Kind) + "s"
		unknownKind = true
	}

	return kubeapplier.ResourceReference{
		Group:     group,
		Version:   version,
		Resource:  resource,
		Namespace: pm.Metadata.Namespace,
		Name:      pm.Metadata.Name,
	}, unknownKind, nil
}

// resourceKey formats a ResourceReference into the canonical identity string.
func resourceKey(ref kubeapplier.ResourceReference) string {
	return transport.ResourceKey(ref.Group, ref.Version, ref.Resource, ref.Namespace, ref.Name)
}

// buildApplyDesireDoc returns the Firestore document ID and the document data
// map for an ApplyDesire. The KubeContent is stored as "spec_kubeContent"
// at the document root (a map[string]any from the raw JSON), matching the
// kube-applier-gcp rawext_codec convention. The "spec" field has KubeContent=nil
// because the Firestore ApplyDesireSpec struct tags it as firestore:"-".
func buildApplyDesireDoc(groupKey, mcName string, ref kubeapplier.ResourceReference, raw []byte) (docID string, data map[string]any, err error) {
	docID = desireid.NewDocumentID(groupKey, ref.Group, ref.Version, ref.Resource, ref.Namespace, ref.Name)

	var kubeContentMap map[string]any
	if err := json.Unmarshal(raw, &kubeContentMap); err != nil {
		return "", nil, fmt.Errorf("buildApplyDesireDoc: unmarshal kubeContent: %w", err)
	}

	data = map[string]any{
		"spec": kubeapplier.ApplyDesireSpec{
			ManagementCluster: mcName,
			GroupKey:          groupKey,
			TargetItem:        ref,
			KubeContent:       nil, // stored separately as spec_kubeContent
		},
		"status":           kubeapplier.ApplyDesireStatus{},
		"spec_kubeContent": kubeContentMap,
	}
	return docID, data, nil
}

// buildReadDesireDoc returns the Firestore document ID and data map for a ReadDesire.
// ReadDesire has no KubeContent in spec; it only has it in status (written by kube-applier-gcp).
func buildReadDesireDoc(groupKey, mcName string, ref kubeapplier.ResourceReference) (docID string, data map[string]any) {
	docID = desireid.NewDocumentID(groupKey, ref.Group, ref.Version, ref.Resource, ref.Namespace, ref.Name)
	data = map[string]any{
		"spec": kubeapplier.ReadDesireSpec{
			ManagementCluster: mcName,
			GroupKey:          groupKey,
			TargetItem:        ref,
		},
		"status": kubeapplier.ReadDesireStatus{},
	}
	return docID, data
}

// buildDeleteDesireDoc returns the Firestore document ID and data map for a DeleteDesire.
func buildDeleteDesireDoc(groupKey, mcName string, ref kubeapplier.ResourceReference) (docID string, data map[string]any) {
	docID = desireid.NewDocumentID(groupKey, ref.Group, ref.Version, ref.Resource, ref.Namespace, ref.Name)
	data = map[string]any{
		"spec": kubeapplier.DeleteDesireSpec{
			ManagementCluster: mcName,
			GroupKey:          groupKey,
			TargetItem:        ref,
		},
		"status": kubeapplier.DeleteDesireStatus{},
	}
	return docID, data
}
