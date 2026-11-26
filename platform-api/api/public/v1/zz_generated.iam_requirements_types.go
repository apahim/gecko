package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// IAMRequirementsStatus is a read-only projection of known release contracts,
// not a dispatch receipt, cloud attestation, or authorization for an IAM update.
// A client must re-read all related resources, require Complete at the same
// object generation, and reject missing, stale, or future lastRefreshTime
// values before using this view to prepare a permission retention plan.
type IAMRequirementsStatus struct {
	// state is Incomplete when known work cannot be fully identified. Missing
	// status is also incomplete; it never means that no permissions are needed.

	// +required
	// +kubebuilder:validation:Enum=Complete;Incomplete
	// +kubebuilder:validation:MaxLength=10
	State string `json:"state"`
	// observedGeneration is the object generation used to build this view.
	// Clients must compare it with metadata.generation.

	// +required
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration"`
	// lastRefreshTime is when the controller last reconciled this view. It is
	// not cloud verification; clients must reject stale or future timestamps.

	// +optional
	LastRefreshTime *metav1.Time `json:"lastRefreshTime,omitempty"`
	// objectUID binds these requirements to this exact resource instance.

	// +required
	// +kubebuilder:validation:MaxLength=128
	ObjectUID string `json:"objectUID"`
	// clusterUID identifies the exact parent Cluster installation. For a
	// Cluster, it equals objectUID.

	// +required
	// +kubebuilder:validation:MaxLength=128
	ClusterUID string `json:"clusterUID"`
	// dispatched is the last release confirmed by non-stale observation.
	// An unavailable or unverified template may still be needed by running work.

	// +optional
	Dispatched *IAMReleaseRequirement `json:"dispatched,omitempty"`
	// pending is a release committed for delivery that may already be partly
	// running. Never infer it is absent because observed feedback has not arrived.

	// +optional
	Pending *IAMReleaseRequirement `json:"pending,omitempty"`
}

// IAMReleaseRequirement identifies the release and immutable IAM contract of
// previously committed work. This does not assert that cloud IAM was applied.
type IAMReleaseRequirement struct {
	// releaseVersion is the full release name, including any prerelease suffix.

	// +required
	// +kubebuilder:validation:MaxLength=128
	ReleaseVersion string `json:"releaseVersion"`
	// releaseImage is the exact payload associated with this requirement.

	// +required
	// +kubebuilder:validation:MaxLength=3072
	ReleaseImage string `json:"releaseImage"`
	// channelGroup is the release's channel group.

	// +required
	// +kubebuilder:validation:MaxLength=128
	ChannelGroup string `json:"channelGroup"`
	// templateReference pins the exact immutable IAM contract.

	// +required
	TemplateReference WIFConfigTemplateReference `json:"templateReference"`
} //nolint:kubeapilinter // Full Reference follows Gecko API naming conventions rather than upstream Ref abbreviations.
