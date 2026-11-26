package v1

// IAMDispatchState is private dispatcher bookkeeping. A pending target never
// erases authority for the last completed target; neither slot is cloud attestation.
type IAMDispatchState struct {
	// pending is a durably committed, possibly partially delivered intent.
	// +optional
	Pending *IAMDispatchReceipt `json:"pending,omitempty"`
	// dispatched is the last target observed after a non-stale delivery.
	// +optional
	Dispatched *IAMDispatchReceipt `json:"dispatched,omitempty"`
}

// IAMDispatchReceipt binds one immutable dispatch identity to object and parent.
// It deliberately excludes derived Cincinnati channel and whole spec generation.
type IAMDispatchReceipt struct {
	// objectUID identifies the Cluster or NodePool owning this receipt.
	// +required
	// +kubebuilder:validation:MaxLength=128
	ObjectUID string `json:"objectUID"`
	// clusterUID binds a NodePool receipt to the exact parent installation.
	// +required
	// +kubebuilder:validation:MaxLength=128
	ClusterUID string `json:"clusterUID"`
	// releaseVersion identifies the complete release, including prerelease suffix.
	// +required
	// +kubebuilder:validation:MaxLength=128
	ReleaseVersion string `json:"releaseVersion"`
	// releaseImage is the exact payload passed into manifest construction.
	// +required
	// +kubebuilder:validation:MaxLength=3072
	ReleaseImage string `json:"releaseImage"`
	// channelGroup identifies release membership, not the advancing CVO channel.
	// +required
	// +kubebuilder:validation:MaxLength=128
	ChannelGroup string `json:"channelGroup"`
	// templateReference binds the exact retained IAM contract authorizing this intent.
	// +required
	TemplateReference WIFConfigTemplateReference `json:"templateReference"` //nolint:kubeapilinter // Full Reference follows Gecko API naming conventions rather than upstream Ref abbreviations.
}
