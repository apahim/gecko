package v1_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/aggregated"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
)

func TestVersionSpecGenerationInvalidatesAvailabilityThroughStorage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("Version")
	store := memory.NewMemoryStore("versions", scheme, gvk)
	ref := privatev1.WIFConfigTemplateReference{Name: "5.0.0", UID: "template-uid", Digest: "sha256:" + strings.Repeat("0", 64)}
	v := &privatev1.Version{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "Version"}, ObjectMeta: metav1.ObjectMeta{Name: "5.0.0", Generation: 1}, Spec: privatev1.VersionSpec{ReleaseImage: "image", ChannelGroups: []string{"stable"}, WIFConfigTemplateRef: &ref}, Status: privatev1.VersionStatus{Conditions: []metav1.Condition{{Type: privatev1.VersionAvailable, Status: metav1.ConditionTrue, Reason: "Available", ObservedGeneration: 1, LastTransitionTime: metav1.Now()}}}}
	if err := store.Create(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	strategy := aggregated.NewResourceStrategy(scheme, maintenanceClusterSchemaProcessor(t, privatev1.VersionSchemaYAML), false, gvk, logr.Discard())
	storage := aggregated.NewResourceStorage(store, strategy, gvk, "versions", "version", scheme, logr.Discard(), nil)
	update := rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _ runtime.Object, old runtime.Object) (runtime.Object, error) {
		next := old.DeepCopyObject().(*privatev1.Version)
		next.Spec.ReleaseImage = "new-image"
		return next, nil
	})
	obj, _, err := storage.Update(t.Context(), v.Name, update, nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*privatev1.Version)
	if got.Generation != 2 || got.Status.Conditions[0].ObservedGeneration != 1 || privatev1.IsVersionAvailable(got) {
		t.Fatal("spec update did not invalidate stale availability")
	}
}

func TestPrivateIAMDispatchReceiptHiddenAndPreserved(t *testing.T) {
	converter := newClusterConverter(t)
	c := &privatev1.Cluster{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "Cluster"}, Status: privatev1.ClusterStatus{IAMDispatch: &privatev1.IAMDispatchState{Pending: &privatev1.IAMDispatchReceipt{ObjectUID: "cluster-uid", ClusterUID: "cluster-uid", ReleaseVersion: "5.0.0", ReleaseImage: "private-payload", ChannelGroup: "stable", TemplateRef: privatev1.WIFConfigTemplateReference{Name: "5.0.0", UID: "template-uid", Digest: "sha256:" + strings.Repeat("0", 64)}}}}}
	pub, err := converter.PrivateToPublic(c)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(pub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "iamDispatch") || strings.Contains(string(data), "private-payload") {
		t.Fatal("private dispatch authority exposed publicly")
	}
	updated, err := converter.PublicToPrivate(pub, c)
	if err != nil {
		t.Fatal(err)
	}
	if updated.(*privatev1.Cluster).Status.IAMDispatch == nil || updated.(*privatev1.Cluster).Status.IAMDispatch.Pending.ReleaseImage != "private-payload" {
		t.Fatal("ordinary public update erased private dispatch authority")
	}
}
