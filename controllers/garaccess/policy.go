package garaccess

import (
	"crypto/sha256"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	readerRole      = "roles/artifactregistry.reader"
	managedLabel    = "app.kubernetes.io/managed-by"
	managedBy       = "gecko-gar-access"
	projectLabel    = "gcp.managed.openshift.io/gar-customer-project"
	stateAnnotation = "gcp.managed.openshift.io/gar-access-state"
	consumersKey    = "gcp.managed.openshift.io/gar-consumers"
)

// NewPolicyMember supplies the GVK for the regional Kubernetes client/cache.
func NewPolicyMember() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "iam.cnrm.cloud.google.com", Version: "v1beta1", Kind: "IAMPolicyMember",
	})
	return obj
}

func desiredGrant(config Config, project, member string) *unstructured.Unstructured {
	obj := NewPolicyMember()
	hash := sha256.Sum256([]byte(config.Repository + "\n" + member + "\n" + readerRole))
	obj.SetName(fmt.Sprintf("gar-quay-reader-%x", hash[:12]))
	obj.SetNamespace(config.IAMNamespace)
	obj.SetLabels(map[string]string{managedLabel: managedBy, projectLabel: project})
	obj.SetAnnotations(map[string]string{
		"cnrm.cloud.google.com/state-into-spec": "absent",
		stateAnnotation:                         "active",
	})
	// No owner references: this grant belongs to all consumers in the project.
	obj.Object["spec"] = map[string]interface{}{
		"member": member,
		"role":   readerRole,
		"resourceRef": map[string]interface{}{
			"apiVersion": "artifactregistry.cnrm.cloud.google.com/v1beta1",
			"kind":       "ArtifactRegistryRepository",
			"external":   config.Repository,
		},
	}
	return obj
}

func matchesGrant(existing, desired *unstructured.Unstructured) bool {
	if existing.GetName() != desired.GetName() || existing.GetNamespace() != desired.GetNamespace() ||
		existing.GetLabels()[managedLabel] != managedBy ||
		existing.GetLabels()[projectLabel] != desired.GetLabels()[projectLabel] ||
		len(existing.GetOwnerReferences()) != 0 {
		return false
	}
	// Compare owned fields, allowing benign KCC defaulting of other spec fields.
	for _, path := range [][]string{
		{"spec", "member"}, {"spec", "role"},
		{"spec", "resourceRef", "apiVersion"}, {"spec", "resourceRef", "kind"},
		{"spec", "resourceRef", "external"},
	} {
		actual, found, err := unstructured.NestedString(existing.Object, path...)
		if err != nil || !found {
			return false
		}
		expected, _, _ := unstructured.NestedString(desired.Object, path...)
		if actual != expected {
			return false
		}
	}
	// A memberFrom or named resourceRef would introduce another identity/target.
	for _, path := range [][]string{{"spec", "memberFrom"}, {"spec", "resourceRef", "name"}, {"spec", "resourceRef", "namespace"}} {
		value, found, err := unstructured.NestedFieldNoCopy(existing.Object, path...)
		if err != nil || (found && value != nil && value != "") {
			return false
		}
	}
	return true
}
