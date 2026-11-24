package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +orlop:authorization-exempt-verbs: get,list
// +kubebuilder:subresource:status
// Version provides clients with versions for cluster creation and upgrade
// validation, including their channel-group membership. Version resources are
// synchronized from Cincinnati by the version-sync controller and are read-only
// to end users.
type Version struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec VersionSpec `json:"spec"`

	// status reports current release availability.

	// +optional
	Status VersionStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
type VersionList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Version `json:"items"`
}

// VersionSpec contains release information synchronized from Cincinnati.
type VersionSpec struct {
	// wifConfigTemplateRef pins the release's installation IAM contract once resolved.

	// +optional
	WIFConfigTemplateRef *WIFConfigTemplateReference `json:"wifConfigTemplateRef,omitempty"`
	// ChannelGroups contains the Cincinnati channel groups containing this release.

	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:UniqueItems=true
	// +listType=set
	ChannelGroups []string `json:"channelGroups"`
}

// VersionStatus reports catalog availability observations.
type VersionStatus struct {
	// conditions reports availability; consumers require current Available=True.

	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func init() { register(&Version{}, &VersionList{}) }
