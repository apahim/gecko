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
	// templateRef binds the exact retained IAM contract authorizing this intent.
	// +required
	TemplateRef WIFConfigTemplateReference `json:"templateRef"`
}

// MatchesIAMDispatch compares target identity, never generation or timestamps.
func MatchesIAMDispatch(r *IAMDispatchReceipt, objectUID, clusterUID string, vr *VersionResolutionResult) bool {
	return r != nil && vr != nil && vr.WIFConfigTemplateRef != nil && objectUID != "" && clusterUID != "" &&
		r.ObjectUID == objectUID && r.ClusterUID == clusterUID && r.ReleaseVersion == vr.ReleaseVersion &&
		r.ReleaseImage == vr.ReleaseImage && r.ChannelGroup == vr.ChannelGroup && r.TemplateRef == *vr.WIFConfigTemplateRef
}

// CommittedIAMDispatch identifies an exact recoverable intent, not a new target.
func CommittedIAMDispatch(s *IAMDispatchState, objectUID, clusterUID string, vr *VersionResolutionResult) bool {
	return s != nil && (MatchesIAMDispatch(s.Pending, objectUID, clusterUID, vr) || MatchesIAMDispatch(s.Dispatched, objectUID, clusterUID, vr))
}

// PromoteIAMDispatch runs only after non-stale transport feedback. Pending stays
// intact through uncertain outcomes and can authorize exact-identity redelivery.
func PromoteIAMDispatch(s *IAMDispatchState, observed string) bool {
	if s == nil || s.Pending == nil || observed == "" || s.Pending.ReleaseVersion != observed {
		return false
	}
	s.Dispatched = s.Pending
	s.Pending = nil
	return true
}
