package v1

import "testing"

func TestPendingInitialDispatchLocksOnlyReleaseIntent(t *testing.T) {
	w := wifTestTemplate(t)
	ref := wifTestRef(t, w)
	c := wifTestCluster(ref)
	c.Spec.Release = ClusterReleaseSpec{Version: "5.0.0", ChannelGroup: "stable"}
	c.Status.IAMDispatch = &IAMDispatchState{Pending: &IAMDispatchReceipt{ObjectUID: string(c.UID), ClusterUID: string(c.UID), ReleaseVersion: "5.0.0", ReleaseImage: "image", ChannelGroup: "stable", TemplateRef: ref}}
	changed := c.DeepCopy()
	changed.Spec.Release.Version = "5.0.1"
	if changed.ValidateUpdate(t.Context(), c) == nil {
		t.Fatal("initial committed dispatch release was changed")
	}
	changed = c.DeepCopy()
	changed.Generation++
	if err := changed.ValidateUpdate(t.Context(), c); err != nil {
		t.Fatalf("unrelated generation invalidated dispatch: %v", err)
	}
	p := &NodePool{}
	p.Spec.Release.Version = "5.0.0"
	p.Status.IAMDispatch = c.Status.IAMDispatch.DeepCopy()
	next := p.DeepCopy()
	next.Spec.Release.Version = "5.0.1"
	if next.ValidateUpdate(t.Context(), p) == nil {
		t.Fatal("initial NodePool committed dispatch release was changed")
	}
	next = p.DeepCopy()
	count := int32(2)
	next.Spec.NodeCount = &count
	if err := next.ValidateUpdate(t.Context(), p); err != nil {
		t.Fatalf("scaling invalidated receipt: %v", err)
	}
}
