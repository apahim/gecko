package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// Channel provides clients with the default version for cluster installation
// and the minor version approved for automatic fleet upgrades. Channel resources
// are managed by the platform and are read-only to end users.
type Channel struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ChannelSpec `json:"spec"`

	// +optional
	Status ChannelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ChannelList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Channel `json:"items"`
}

// ChannelSpec contains platform-managed controls for a channel group.
type ChannelSpec struct {
	// InstallDefaultVersion is the exact release selected when a client does
	// not provide a version during cluster creation.

	// +required
	// +kubebuilder:validation:MinLength=1
	InstallDefaultVersion string `json:"installDefaultVersion"`

	// FleetMinorVersion is the major.minor release line approved for automatic
	// fleet upgrades.

	// +required
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`
	FleetMinorVersion string `json:"fleetMinorVersion"`
}

// ChannelStatus contains observations made by the version-sync controller.
type ChannelStatus struct {
	// Conditions include DefaultVersionAvailable: True when the pinned default
	// is present in this Channel's catalog, False when a successful sync finds
	// it absent, and Unknown when availability cannot be determined. A missing
	// condition means availability has not yet been evaluated. Consumers must
	// check observedGeneration before interpreting a condition after spec changes.
	// An unavailable default does not prevent synchronization of other releases.

	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

func init() { register(&Channel{}, &ChannelList{}) }
