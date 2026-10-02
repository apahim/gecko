package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PlatformRole is a cluster-scoped, system-managed role. PlatformRoles are
// seeded by Helm and are intentionally not projected to the public API.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type PlatformRole struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec PlatformRoleSpec `json:"spec"`
}

// PlatformRoleList is a list of PlatformRole resources.
//
// +kubebuilder:object:root=true
type PlatformRoleList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// +optional
	Items []PlatformRole `json:"items"`
}

// PlatformRoleSpec describes the permissions granted by a PlatformRole.
type PlatformRoleSpec struct {
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	// +required
	Permissions []string `json:"permissions"`
}

func init() {
	register(&PlatformRole{}, &PlatformRoleList{})
}
