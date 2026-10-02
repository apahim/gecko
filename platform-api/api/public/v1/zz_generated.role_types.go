package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Role is a namespace-scoped, user-managed role. It is projected to the
// public API because service-admins manage user-defined roles there.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
type Role struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec RoleSpec `json:"spec"`
}

// RoleList is a list of Role resources.
//
// +kubebuilder:object:root=true
type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// +optional
	Items []Role `json:"items"`
}

// RoleSpec describes the permissions granted by a Role.
type RoleSpec struct {

	// +kubebuilder:validation:MinItems=1
	// +listType=set
	// +required
	Permissions []string `json:"permissions"`
}

func init() {
	register(&Role{}, &RoleList{})
}
