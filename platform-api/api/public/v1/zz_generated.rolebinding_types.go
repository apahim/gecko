package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// RoleBinding binds an email principal to a PlatformRole or a namespace Role.
// PlatformRole is cluster-scoped, but the binding itself remains scoped to the
// namespace in which access is granted.
//
// Conditions and object-state filtering are intentionally left for GCP-1031;
// accepting them here before the authorization engine evaluates them would be
// unsafe.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
type RoleBinding struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec RoleBindingSpec `json:"spec"`
}

// RoleBindingList is a list of RoleBinding resources.
//
// +kubebuilder:object:root=true
type RoleBindingList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// +optional
	Items []RoleBinding `json:"items"`
}

// RoleBindingSpec identifies the principal and role granted in a namespace.
type RoleBindingSpec struct {

	// +kubebuilder:validation:MinLength=1
	// +required
	Subject string `json:"subject"`

	// +required
	RoleRef RoleRef `json:"roleRef"`
}

// RoleRef identifies either a cluster-scoped PlatformRole or a namespaced
// Role. APIGroup is explicit so references cannot silently cross API groups.
type RoleRef struct {

	// +required
	Kind string `json:"kind"`

	// +required
	Name string `json:"name"`

	// +required
	APIGroup string `json:"apiGroup"`
}

func init() {
	register(&RoleBinding{}, &RoleBindingList{})
}
