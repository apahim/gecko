package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +orlop:public-verbs: get,list
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

	// +orlop:public
	// +required
	Spec VersionSpec `json:"spec"`

	// status reports current release availability.
	// +orlop:public
	// +optional
	Status VersionStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
type VersionList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// +orlop:public
	Items []Version `json:"items"`
}

// VersionSpec contains release information synchronized from Cincinnati.
type VersionSpec struct {
	// wifConfigTemplateReference pins the release's installation IAM contract once resolved.
	// +orlop:public
	// +optional
	WIFConfigTemplateReference *WIFConfigTemplateReference `json:"wifConfigTemplateReference,omitempty"` //nolint:kubeapilinter // Full Reference follows Gecko API naming conventions rather than upstream Ref abbreviations.
	// ChannelGroups contains the Cincinnati channel groups containing this release.
	// +orlop:public
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:UniqueItems=true
	// +listType=set
	ChannelGroups []string `json:"channelGroups"`

	// ReleaseImage is the Cincinnati release payload used internally by Gecko.
	// It is intentionally not exposed through the public API.
	// +required
	// +kubebuilder:validation:MinLength=1
	ReleaseImage string `json:"releaseImage"`
}

// VersionStatus reports catalog availability observations.
type VersionStatus struct {
	// conditions reports availability; consumers require current Available=True.
	// +orlop:public
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VersionAvailable is the availability condition consumers must observe as
// True with a current observedGeneration before dispatching against a release.
const VersionAvailable = "Available"

func init() { register(&Version{}, &VersionList{}) }
