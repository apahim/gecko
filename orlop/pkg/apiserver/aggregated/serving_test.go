package aggregated

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	testv1 "github.com/openshift-online/gecko/orlop/apis/private/test/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
)

func TestTypedWatchProjection(t *testing.T) {
	env := setupStorageTest(t)
	for _, eventType := range []watch.EventType{watch.Added, watch.Modified, watch.Deleted, watch.Bookmark} {
		t.Run(string(eventType), func(t *testing.T) {
			source := watch.NewRaceFreeFake()
			filtered := env.storage.typedWatch(source)
			defer filtered.Stop()
			document := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": testGVK.GroupVersion().String(), "kind": testGVK.Kind,
				"metadata": map[string]interface{}{"name": "test", "resourceVersion": "12", "annotations": map[string]interface{}{metav1.InitialEventsAnnotationKey: "true"}},
				"spec":     map[string]interface{}{"publicField": "kept", "obsolete": "hidden"},
			}}
			source.Action(eventType, document)
			select {
			case event := <-filtered.ResultChan():
				typed, ok := event.Object.(*testv1.Object)
				if !ok || event.Type != eventType {
					t.Fatalf("unexpected projected event: %#v", event)
				}
				if typed.Spec.PublicField != "kept" || typed.ResourceVersion != "12" || typed.Annotations[metav1.InitialEventsAnnotationKey] != "true" {
					t.Fatal("projection lost current fields/metadata")
				}
				data, err := json.Marshal(event.Object)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "obsolete") {
					t.Fatal("obsolete field exposed")
				}
				_, found, err := unstructured.NestedFieldNoCopy(document.Object, "spec", "obsolete")
				if err != nil {
					t.Fatalf("lookup obsolete source field: %v", err)
				}
				if !found {
					t.Fatal("projection mutated stored event")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for event")
			}
		})
	}
}

func TestTypedWatchErrorsAndStop(t *testing.T) {
	env := setupStorageTest(t)
	source := watch.NewRaceFreeFake()
	filtered := env.storage.typedWatch(source)
	status := &metav1.Status{Status: metav1.StatusFailure, Message: "original"}
	source.Error(status)
	if event := <-filtered.ResultChan(); event.Type != watch.Error || event.Object != status {
		t.Fatal("error event not preserved")
	}
	// Empty storage bookmarks are skipped; malformed resource events become
	// sanitized errors rather than leaking the original unstructured document.
	source.Action(watch.Bookmark, nil)
	source.Action(watch.Added, &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"publicField": []interface{}{"sensitive"}}}})
	event := <-filtered.ResultChan()
	result, ok := event.Object.(*metav1.Status)
	if !ok || event.Type != watch.Error || result.Code != 500 || strings.Contains(result.Message, "sensitive") {
		t.Fatalf("bad conversion failure: %#v", event)
	}
	source.Action(watch.Added, nil)
	if event := <-filtered.ResultChan(); event.Type != watch.Error {
		t.Fatal("nil event did not fail safely")
	}
	filtered.Stop()
	if !source.IsStopped() {
		t.Fatal("Stop did not propagate")
	}
	select {
	case _, ok := <-filtered.ResultChan():
		if ok {
			t.Fatal("watch channel still open")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
}

func TestProjectedWatchStopWithUnreadEvent(t *testing.T) {
	source := watch.NewRaceFreeFake()
	projected := make(chan struct{})
	w := newProjectedWatch(source, func(e watch.Event) (watch.Event, bool) { close(projected); return e, true })
	defer w.Stop()
	source.Action(watch.Added, &metav1.Status{})
	select {
	case <-projected:
	case <-time.After(5 * time.Second):
		t.Fatal("projection did not run")
	}
	// Do not receive the pending event. Stop must cancel its outgoing send.
	w.Stop()
	w.Stop()
	select {
	case <-w.(*projectedWatch).finished:
		if !source.IsStopped() {
			t.Fatal("upstream not stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch blocked on unread event")
	}
}
