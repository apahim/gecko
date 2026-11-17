package catalog

import (
	"context"

	channelutil "github.com/openshift-online/gecko/controllers/util/channel"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Dispatch returns either an exact committed authority (persist=false), or a
// newly verified intent that MUST be persisted optimistically before Apply.
// Revocation affects new identities, not redelivery/maintenance of committed ones.
func Dispatch(ctx context.Context, reader client.Reader, c *privatev1.Cluster, objectUID string, vr *privatev1.VersionResolutionResult, state *privatev1.IAMDispatchState) (receipt *privatev1.IAMDispatchReceipt, persist bool, err error) {
	if c == nil || c.UID == "" || objectUID == "" || vr == nil || vr.WIFConfigTemplateRef == nil {
		return nil, false, &UnavailableError{Reason: "VersionResolutionNotReady", Message: "Waiting for a complete target and installation identity"}
	}
	if state != nil && state.Pending != nil && !privatev1.MatchesIAMDispatch(state.Pending, objectUID, string(c.UID), vr) {
		return nil, false, &UnavailableError{Reason: "DispatchIntentPending", Message: "A different committed dispatch must be observed before changing target"}
	}
	if privatev1.CommittedIAMDispatch(state, objectUID, string(c.UID), vr) {
		if !privatev1.HasWIFConfigCoverage(c, vr.WIFConfigTemplateRef) {
			return nil, false, &UnavailableError{Reason: privatev1.WIFConfigUpdateRequired, Message: "The committed IAM contract is not covered by retained application history"}
		}
		if state.Pending != nil {
			return state.Pending.DeepCopy(), false, nil
		}
		return state.Dispatched.DeepCopy(), false, nil
	}
	v, err := Prerequisites(ctx, reader, c, vr.ReleaseVersion, vr.ChannelGroup)
	if err != nil {
		return nil, false, err
	}
	if v.Spec.ReleaseImage != vr.ReleaseImage || *v.Spec.WIFConfigTemplateRef != *vr.WIFConfigTemplateRef {
		return nil, false, &UnavailableError{Reason: "VersionResolutionOutdated", Message: "Waiting for current payload and pinned IAM contract resolution"}
	}
	return &privatev1.IAMDispatchReceipt{ObjectUID: objectUID, ClusterUID: string(c.UID), ReleaseVersion: vr.ReleaseVersion, ReleaseImage: vr.ReleaseImage, ChannelGroup: vr.ChannelGroup, TemplateRef: *vr.WIFConfigTemplateRef}, true, nil
}

// ClusterInstallationGroup keeps pending channel intent out of running manifests.
func ClusterInstallationGroup(c *privatev1.Cluster) string {
	if !privatev1.ClusterReleaseApproved(c) && c.Status.VersionResolution != nil && c.Status.VersionResolution.ChannelGroup != "" {
		return c.Status.VersionResolution.ChannelGroup
	}
	if c.Spec.Release.ChannelGroup != "" {
		return c.Spec.Release.ChannelGroup
	}
	return channelutil.DefaultChannelGroup
}

func NodePoolInstallationGroup(p *privatev1.NodePool) string {
	if !privatev1.NodePoolReleaseApproved(p) && p.Status.VersionResolution != nil && p.Status.VersionResolution.ChannelGroup != "" {
		return p.Status.VersionResolution.ChannelGroup
	}
	return NodePoolRequestedGroup(p)
}

func NodePoolRequestedGroup(p *privatev1.NodePool) string {
	if p.Spec.Release.ChannelGroup != "" {
		return p.Spec.Release.ChannelGroup
	}
	return "candidate"
}

func NodePoolInstallationVersion(p *privatev1.NodePool) string {
	if !privatev1.NodePoolReleaseApproved(p) {
		return p.Status.Version
	}
	return p.Spec.Release.Version
}
