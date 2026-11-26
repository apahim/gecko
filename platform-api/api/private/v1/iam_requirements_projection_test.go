package v1_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
	publicv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func requirementReceipt(object, parent, version string) *privatev1.IAMDispatchReceipt {
	return &privatev1.IAMDispatchReceipt{ObjectUID: object, ClusterUID: parent, ReleaseVersion: version,
		ReleaseImage: "quay.io/example/release@sha256:" + strings.Repeat("a", 64), ChannelGroup: "candidate",
		TemplateReference: privatev1.WIFConfigTemplateReference{Name: "5.0.0", UID: "template-uid", Digest: "sha256:" + strings.Repeat("b", 64)}}
}

func TestIAMRequirementsClusterProjectionAndPublicIsolation(t *testing.T) {
	c := &privatev1.Cluster{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "Cluster"}, ObjectMeta: metav1.ObjectMeta{UID: "cluster-uid", Generation: 3}}
	if !privatev1.SyncClusterIAMRequirements(c) || c.Status.IAMRequirements.State != "Incomplete" {
		t.Fatal("missing receipts must not be interpreted as no requirements")
	}
	c.Status.IAMDispatch = &privatev1.IAMDispatchState{Dispatched: requirementReceipt("cluster-uid", "cluster-uid", "5.0.2")}
	c.Spec.Release.Version = "5.0.2"
	c.Status.HostedClusterResult = &privatev1.HostedClusterResult{Version: "5.0.2", ObservedConditions: []metav1.Condition{{Type: "ClusterVersionProgressing", Status: metav1.ConditionFalse}}}
	if !privatev1.SyncClusterIAMRequirements(c) || c.Status.IAMRequirements.State != "Complete" {
		t.Fatal("complete observed control plane is not available for readback")
	}
	if privatev1.SyncClusterIAMRequirements(c) {
		t.Fatal("unchanged projection caused status churn")
	}
	if c.Status.IAMRequirements.LastRefreshTime == nil {
		t.Fatal("missing bounded freshness signal")
	}
	old := metav1.NewTime(time.Now().Add(-6 * time.Minute))
	c.Status.IAMRequirements.LastRefreshTime = &old
	if !privatev1.SyncClusterIAMRequirements(c) || !c.Status.IAMRequirements.LastRefreshTime.After(old.Time) {
		t.Fatal("controller failed to refresh an aged requirements view")
	}
	future := metav1.NewTime(time.Now().Add(2 * time.Minute))
	c.Status.IAMRequirements.LastRefreshTime = &future
	if !privatev1.SyncClusterIAMRequirements(c) || !c.Status.IAMRequirements.LastRefreshTime.Time.Before(future.Time) {
		t.Fatal("future-dated projection timestamp was not corrected")
	}
	c.Spec.Release.Version = "5.0.3"
	c.Generation++
	privatev1.SyncClusterIAMRequirements(c)
	if c.Status.IAMRequirements.State != "Incomplete" || c.Status.IAMRequirements.Dispatched == nil {
		t.Fatal("unreceipted spec target was reported complete")
	}
	c.Spec.Release.Version = "5.0.2"
	c.Generation--
	privatev1.SyncClusterIAMRequirements(c)
	assertClusterPublicRequirements(t, c)
	c.Status.IAMDispatch.Pending = requirementReceipt("cluster-uid", "cluster-uid", "5.0.3")
	c.Spec.Release.Version = "5.0.3"
	if !privatev1.SyncClusterIAMRequirements(c) || c.Status.IAMRequirements.Pending.ReleaseVersion != "5.0.3" || c.Status.IAMRequirements.Dispatched.ReleaseVersion != "5.0.2" {
		t.Fatal("committed pending work discarded an observed running requirement")
	}
	c.Spec.Release.Version = "5.0.4"
	privatev1.SyncClusterIAMRequirements(c)
	if c.Status.IAMRequirements.State != "Incomplete" {
		t.Fatal("unreceipted target during pending delivery was declared complete")
	}
	c.Spec.Release.Version = "5.0.3"
	c.Status.HostedClusterResult.ObservedConditions = []metav1.Condition{{Type: "ClusterVersionProgressing", Status: metav1.ConditionTrue}}
	privatev1.SyncClusterIAMRequirements(c)
	if c.Status.IAMRequirements.State != "Incomplete" || c.Status.IAMRequirements.Pending == nil {
		t.Fatal("in-progress release must retain pending details but fail closed")
	}
	for _, conditions := range [][]metav1.Condition{nil, {{Type: "ClusterVersionProgressing", Status: metav1.ConditionUnknown}}} {
		c.Status.HostedClusterResult.ObservedConditions = conditions
		privatev1.SyncClusterIAMRequirements(c)
		if c.Status.IAMRequirements.State != "Incomplete" {
			t.Fatal("missing/unknown control-plane progress feedback was declared complete")
		}
	}
	// A replaced private receipt must never be projected as an authorized
	// requirement of this installation.
	c.Status.IAMDispatch.Pending.ClusterUID = "replacement"
	privatev1.SyncClusterIAMRequirements(c)
	if c.Status.IAMRequirements.State != "Incomplete" || c.Status.IAMRequirements.Pending != nil {
		t.Fatal("wrong parent UID was exposed or declared complete")
	}
}

func assertClusterPublicRequirements(t *testing.T, c *privatev1.Cluster) {
	t.Helper()
	converter := newClusterConverter(t)
	converted, err := converter.PrivateToPublic(c)
	if err != nil {
		t.Fatal(err)
	}
	pub := converted.(*publicv1.Cluster)
	if pub.Status.IAMRequirements == nil || pub.Status.IAMRequirements.Dispatched == nil || pub.Status.IAMRequirements.Dispatched.TemplateReference.UID != "template-uid" || pub.Status.IAMRequirements.ObservedGeneration != 3 {
		t.Fatalf("public contract projection missing: %+v", pub.Status.IAMRequirements)
	}
	encoded, err := json.Marshal(pub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "iamDispatch") || strings.Contains(string(encoded), "versionResolution") {
		t.Fatalf("private dispatch authority leaked to public API: %s", encoded)
	}
}

func TestIAMRequirementsNodePoolProjectionAndUncertainFeedback(t *testing.T) {
	parent := &privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "cluster-uid"}}
	p := &privatev1.NodePool{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "NodePool"}, ObjectMeta: metav1.ObjectMeta{UID: "pool-uid", Generation: 2}}
	p.Status.IAMDispatch = &privatev1.IAMDispatchState{Pending: requirementReceipt("pool-uid", "cluster-uid", "5.0.2")}
	if !privatev1.SyncNodePoolIAMRequirements(p, parent) || p.Status.IAMRequirements.Pending == nil || p.Status.IAMRequirements.Dispatched != nil {
		t.Fatal("pending NodePool requirement was omitted")
	}
	p.Status.Version = "5.0.2"
	p.Spec.Release.Version = "5.0.2"
	p.Status.Conditions = []metav1.Condition{{Type: "NodePoolProgressing", Status: metav1.ConditionFalse}}
	p.Status.IAMDispatch.Dispatched = p.Status.IAMDispatch.Pending
	p.Status.IAMDispatch.Pending = nil
	privatev1.SyncNodePoolIAMRequirements(p, parent)
	if p.Status.IAMRequirements.State != "Complete" || p.Status.IAMRequirements.Dispatched.ReleaseVersion != p.Status.Version {
		t.Fatal("observed worker requirement was not projected")
	}
	p.Spec.Release.Version = "5.0.3"
	p.Generation++
	privatev1.SyncNodePoolIAMRequirements(p, parent)
	if p.Status.IAMRequirements.State != "Incomplete" {
		t.Fatal("unreceipted worker target was reported complete")
	}
	p.Spec.Release.Version = "5.0.2"
	p.Generation--
	privatev1.SyncNodePoolIAMRequirements(p, parent)
	pub, err := newClusterConverter(t).PrivateToPublic(p)
	if err != nil {
		t.Fatal(err)
	}
	publicPool := pub.(*publicv1.NodePool)
	if publicPool.Status.IAMRequirements.ClusterUID != "cluster-uid" || publicPool.Status.IAMRequirements.ObjectUID != "pool-uid" {
		t.Fatal("public NodePool lost immutable parent identity")
	}
	p.Status.Version = "5.0.3"
	privatev1.SyncNodePoolIAMRequirements(p, parent)
	if p.Status.IAMRequirements.State != "Incomplete" {
		t.Fatal("unaccounted observed version was declared complete")
	}
	p.Status.Version = "5.0.2"
	p.Status.Conditions = []metav1.Condition{{Type: "NodePoolProgressing", Status: metav1.ConditionTrue}}
	privatev1.SyncNodePoolIAMRequirements(p, parent)
	if p.Status.IAMRequirements.State != "Incomplete" {
		t.Fatal("active worker rollout was declared complete")
	}
	for _, conditions := range [][]metav1.Condition{nil, {{Type: "NodePoolProgressing", Status: metav1.ConditionUnknown}}} {
		p.Status.Conditions = conditions
		privatev1.SyncNodePoolIAMRequirements(p, parent)
		if p.Status.IAMRequirements.State != "Incomplete" {
			t.Fatal("missing/unknown worker progress feedback was declared complete")
		}
	}
	parent.UID = "replacement"
	privatev1.SyncNodePoolIAMRequirements(p, parent)
	if p.Status.IAMRequirements.State != "Incomplete" || p.Status.IAMRequirements.Dispatched != nil {
		t.Fatal("foreign parent receipt was treated as this NodePool's requirement")
	}
}
