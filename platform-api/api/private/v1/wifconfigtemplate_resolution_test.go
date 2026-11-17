package v1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTemplateNumericFloorAndPrereleaseInheritance(t *testing.T) {
	templates := []WIFConfigTemplate{{ObjectMeta: metav1.ObjectMeta{Name: "5.0.6"}}, {ObjectMeta: metav1.ObjectMeta{Name: "5.0.0"}}, {ObjectMeta: metav1.ObjectMeta{Name: "5.0.999999999999999999999999"}}}
	for _, tc := range []struct{ release, want string }{{"5.0.0", "5.0.0"}, {"5.0.2", "5.0.0"}, {"5.0.6", "5.0.6"}, {"5.0.10", "5.0.6"}, {"5.1.0", ""}, {"6.0.0", ""}, {"5.0.0-0.nightly-2026-11-17", "5.0.0"}, {"5.0.6-rc.1", "5.0.6"}} {
		t.Run(tc.release, func(t *testing.T) {
			got, err := SelectWIFConfigTemplate(tc.release, templates)
			if err != nil {
				t.Fatal(err)
			}
			name := ""
			if got != nil {
				name = got.Name
			}
			if name != tc.want {
				t.Fatalf("got %q, want %q", name, tc.want)
			}
		})
	}
	if _, err := SelectWIFConfigTemplate("v5.0.0", templates); err == nil {
		t.Fatal("malformed release accepted")
	}
	templates[0].Name = "05.0.6"
	if _, err := SelectWIFConfigTemplate("5.0.6", templates); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}

func TestPinnedVerificationAndExactCoverage(t *testing.T) {
	w := wifTestTemplate(t)
	ref := wifTestRef(t, w)
	if reason, _ := CheckWIFConfigReference(&ref, w); reason != WIFTemplateUnverified {
		t.Fatal(reason)
	}
	w.Spec.Verified = new(true)
	if reason, _ := CheckWIFConfigReference(&ref, w); reason != "" {
		t.Fatal(reason)
	}
	cluster := wifTestCluster(ref)
	if !HasWIFConfigCoverage(cluster, &ref) {
		t.Fatal("exact application rejected")
	}
	other := ref
	other.Name = "5.0.1"
	if HasWIFConfigCoverage(cluster, &other) {
		t.Fatal("newer name inferred coverage")
	}
	other = ref
	other.Digest = "sha256:" + strings.Repeat("0", 64)
	if HasWIFConfigCoverage(cluster, &other) {
		t.Fatal("wrong content inferred coverage")
	}
	w.UID = "replaced"
	if reason, _ := CheckWIFConfigReference(&ref, w); reason != WIFTemplateIntegrityMismatch {
		t.Fatal(reason)
	}
}

func TestCurrentVersionAvailability(t *testing.T) {
	w := wifTestTemplate(t)
	ref := wifTestRef(t, w)
	v := &Version{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Spec: VersionSpec{ReleaseImage: "image", ChannelGroups: []string{"stable"}, WIFConfigTemplateRef: &ref}}
	for _, status := range []metav1.ConditionStatus{metav1.ConditionFalse, metav1.ConditionUnknown, metav1.ConditionTrue} {
		v.Status.Conditions = []metav1.Condition{{Type: VersionAvailable, Status: status, ObservedGeneration: 1}}
		if IsVersionAvailable(v) {
			t.Fatal("stale condition accepted")
		}
		v.Status.Conditions[0].ObservedGeneration = 2
		if IsVersionAvailable(v) != (status == metav1.ConditionTrue) {
			t.Fatal("wrong availability")
		}
	}
}
