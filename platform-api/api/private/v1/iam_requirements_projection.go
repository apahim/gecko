package v1

import (
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const requirementsRefreshInterval = 5 * time.Minute

func projectionChanged(current, next *IAMRequirementsStatus) bool {
	if current != nil {
		next.LastRefreshTime = current.LastRefreshTime
	}
	now := time.Now().UTC()
	if reflect.DeepEqual(current, next) && current.LastRefreshTime != nil &&
		!current.LastRefreshTime.After(now.Add(time.Minute)) && now.Sub(current.LastRefreshTime.Time) < requirementsRefreshInterval {
		return false
	}
	refreshed := metav1.NewTime(now)
	next.LastRefreshTime = &refreshed
	return true
}

func progressSettled(conditions []metav1.Condition, observed string, kind string) bool {
	if observed == "" {
		return true // Pending-only initial work has no observed release yet.
	}
	progress := meta.FindStatusCondition(conditions, kind)
	return progress != nil && progress.Status == metav1.ConditionFalse
}

func releaseRequirement(r *IAMDispatchReceipt, objectUID, clusterUID string) (*IAMReleaseRequirement, bool) {
	if r == nil {
		return nil, true
	}
	if r.ObjectUID != objectUID || r.ClusterUID != clusterUID || r.ReleaseVersion == "" || r.ReleaseImage == "" || r.ChannelGroup == "" || validateWIFReferenceShape(r.TemplateRef) != nil {
		return nil, false
	}
	return &IAMReleaseRequirement{ReleaseVersion: r.ReleaseVersion, ReleaseImage: r.ReleaseImage, ChannelGroup: r.ChannelGroup, TemplateRef: r.TemplateRef}, true
}

// SyncClusterIAMRequirements updates only the public observation. It does not
// change private receipt authority, specs, or release eligibility. Call it in
// the same status write as receipt creation/promotion and on steady reconciles.
func SyncClusterIAMRequirements(c *Cluster) bool {
	if c == nil || c.UID == "" {
		return false
	}
	uid := string(c.UID)
	next := &IAMRequirementsStatus{State: "Incomplete", ObjectUID: uid, ClusterUID: uid, ObservedGeneration: c.Generation}
	valid := true
	if c.Status.IAMDispatch != nil {
		next.Dispatched, valid = releaseRequirement(c.Status.IAMDispatch.Dispatched, uid, uid)
		var pendingOK bool
		next.Pending, pendingOK = releaseRequirement(c.Status.IAMDispatch.Pending, uid, uid)
		valid = valid && pendingOK
	}
	observed := ""
	var conditions []metav1.Condition
	if c.Status.HostedClusterResult != nil {
		observed = c.Status.HostedClusterResult.Version
		conditions = c.Status.HostedClusterResult.ObservedConditions
	}
	if valid && (next.Dispatched != nil || next.Pending != nil) &&
		(observed == "" || next.Dispatched != nil && next.Dispatched.ReleaseVersion == observed || next.Pending != nil && next.Pending.ReleaseVersion == observed) &&
		progressSettled(conditions, observed, "ClusterVersionProgressing") &&
		(c.Spec.Release.Version == observed || next.Pending != nil && next.Pending.ReleaseVersion == c.Spec.Release.Version) &&
		(c.Status.HostedClusterResult == nil || c.Status.HostedClusterResult.DesiredVersion == "" || c.Status.HostedClusterResult.DesiredVersion == observed || next.Pending != nil && next.Pending.ReleaseVersion == c.Status.HostedClusterResult.DesiredVersion) &&
		c.DeletionTimestamp.IsZero() {
		next.State = "Complete"
	}
	if !projectionChanged(c.Status.IAMRequirements, next) {
		return false
	}
	c.Status.IAMRequirements = next
	return true
}

// SyncNodePoolIAMRequirements applies the same fail-closed projection to one
// NodePool, bound to its parent Cluster's immutable UID.
func SyncNodePoolIAMRequirements(p *NodePool, parent *Cluster) bool {
	if p == nil || p.UID == "" || parent == nil || parent.UID == "" {
		return false
	}
	uid, parentUID := string(p.UID), string(parent.UID)
	next := &IAMRequirementsStatus{State: "Incomplete", ObjectUID: uid, ClusterUID: parentUID, ObservedGeneration: p.Generation}
	valid := true
	if p.Status.IAMDispatch != nil {
		next.Dispatched, valid = releaseRequirement(p.Status.IAMDispatch.Dispatched, uid, parentUID)
		var pendingOK bool
		next.Pending, pendingOK = releaseRequirement(p.Status.IAMDispatch.Pending, uid, parentUID)
		valid = valid && pendingOK
	}
	observed := p.Status.Version
	if valid && (next.Dispatched != nil || next.Pending != nil) &&
		(observed == "" || next.Dispatched != nil && next.Dispatched.ReleaseVersion == observed || next.Pending != nil && next.Pending.ReleaseVersion == observed) &&
		progressSettled(p.Status.Conditions, observed, "NodePoolProgressing") &&
		(p.Spec.Release.Version == observed || next.Pending != nil && next.Pending.ReleaseVersion == p.Spec.Release.Version) && p.DeletionTimestamp.IsZero() {
		next.State = "Complete"
	}
	if !projectionChanged(p.Status.IAMRequirements, next) {
		return false
	}
	p.Status.IAMRequirements = next
	return true
}
