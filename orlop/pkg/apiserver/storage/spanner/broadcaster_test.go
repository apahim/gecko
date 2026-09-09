package spanner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var broadcasterCounterID = "test_broadcaster"

func testSchemeAndGVK() (*runtime.Scheme, schema.GroupVersionKind) {
	scheme := runtime.NewScheme()
	gv := schema.GroupVersion{Group: "test.example.com", Version: "v1"}
	scheme.AddKnownTypeWithName(gv.WithKind("TestObject"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gv.WithKind("TestObjectList"), &unstructured.UnstructuredList{})
	gvk := schema.GroupVersionKind{Group: "test.example.com", Version: "v1", Kind: "TestObject"}
	return scheme, gvk
}

func setupBroadcasterWithStore(t *testing.T) (*spannerBroadcaster, *SpannerStore) {
	t.Helper()

	scheme, gvk := testSchemeAndGVK()

	rt := gvkString(gvk)

	changeStreamName := "cs_" + resourcesTable

	broadcaster, err := newSpannerBroadcaster(context.Background(), spannerBroadcasterConfig{
		Client:                sharedClient,
		ResourceType:          rt,
		TableName:             resourcesTable,
		ChangeStreamName:      changeStreamName,
		Scheme:                scheme,
		GVK:                   gvk,
		HeartbeatMilliseconds: 1000, // 1s heartbeat for faster emulator tests
	})
	if err != nil {
		t.Fatalf("newSpannerBroadcaster() failed: %v", err)
	}
	t.Cleanup(func() { broadcaster.Close() })

	store := &SpannerStore{
		client:        sharedClient,
		resourceType:  rt,
		scheme:        scheme,
		gvk:           gvk,
		broadcaster:   broadcaster,
		tableName:     resourcesTable,
		countersTable: countersTable,
		counterID:     broadcasterCounterID,
	}

	return broadcaster, store
}

func uniqueNS(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, testCounterSeq.Add(1))
}

func drainUntil(t *testing.T, ch <-chan storage.ResourceEvent, ns, name string, timeout time.Duration) storage.ResourceEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			if event.Object.GetNamespace() == ns && event.Object.GetName() == name {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event (ns=%s, name=%s)", ns, name)
		}
	}
}

// eventTimeout is the maximum time to wait for a single change-stream event.
// Generous to accommodate emulator latency.
const eventTimeout = 30 * time.Second

func TestBroadcaster_SubscribeReceivesLiveEvents(t *testing.T) {
	_, store := setupBroadcasterWithStore(t)
	ns := uniqueNS("bc-live")

	eventCh, stop, err := store.broadcaster.Subscribe("")
	if err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}
	defer stop()

	obj := newTestObject(withName("obj"), withNamespace(ns))
	if err := store.Create(context.Background(), obj); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	event := drainUntil(t, eventCh, ns, "obj", eventTimeout)
	if event.Type != storage.EventAdded {
		t.Errorf("expected event type %s, got %s", storage.EventAdded, event.Type)
	}
}

func TestBroadcaster_SubscribeReplaysHistoricalEvents(t *testing.T) {
	_, store := setupBroadcasterWithStore(t)
	ns := uniqueNS("bc-hist")

	obj := newTestObject(withName("obj"), withNamespace(ns))
	if err := store.Create(context.Background(), obj); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	eventCh, stop, err := store.broadcaster.Subscribe("0")
	if err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}
	defer stop()

	drainUntil(t, eventCh, ns, "obj", eventTimeout)
}

func TestBroadcaster_SubscribeSinceResourceVersion(t *testing.T) {
	_, store := setupBroadcasterWithStore(t)
	ns := uniqueNS("bc-since")

	obj1 := newTestObject(withName("before"), withNamespace(ns))
	if err := store.Create(context.Background(), obj1); err != nil {
		t.Fatalf("Create() obj1 failed: %v", err)
	}
	sinceRV := obj1.GetResourceVersion()

	obj2 := newTestObject(withName("after"), withNamespace(ns))
	if err := store.Create(context.Background(), obj2); err != nil {
		t.Fatalf("Create() obj2 failed: %v", err)
	}

	eventCh, stop, err := store.broadcaster.Subscribe(sinceRV)
	if err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}
	defer stop()

	deadline := time.After(eventTimeout)
	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			if event.Object.GetNamespace() != ns {
				continue
			}
			if event.Object.GetName() == "before" {
				t.Fatal("received event for 'before' which should have been filtered by sinceResourceVersion")
			}
			if event.Object.GetName() == "after" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for 'after' event")
		}
	}
}

func TestBroadcaster_MultipleSubscribers(t *testing.T) {
	_, store := setupBroadcasterWithStore(t)
	ns := uniqueNS("bc-multi")

	ch1, stop1, err := store.broadcaster.Subscribe("")
	if err != nil {
		t.Fatalf("Subscribe() 1 failed: %v", err)
	}
	defer stop1()

	ch2, stop2, err := store.broadcaster.Subscribe("")
	if err != nil {
		t.Fatalf("Subscribe() 2 failed: %v", err)
	}
	defer stop2()

	obj := newTestObject(withName("obj"), withNamespace(ns))
	if err := store.Create(context.Background(), obj); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	drainUntil(t, ch1, ns, "obj", eventTimeout)
	drainUntil(t, ch2, ns, "obj", eventTimeout)
}

func TestBroadcaster_UnsubscribeStopsDelivery(t *testing.T) {
	broadcaster, _ := setupBroadcasterWithStore(t)

	eventCh, stop, err := broadcaster.Subscribe("")
	if err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}

	stop()

	select {
	case _, ok := <-eventCh:
		if ok {
			return
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event channel not closed after unsubscribe")
	}
}

func TestBroadcaster_DeleteEvent(t *testing.T) {
	_, store := setupBroadcasterWithStore(t)
	ns := uniqueNS("bc-delete")

	obj := newTestObject(withName("to-delete"), withNamespace(ns))
	if err := store.Create(context.Background(), obj); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	eventCh, stop, err := store.broadcaster.Subscribe("")
	if err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}
	defer stop()

	// Wait for the CREATE event to arrive before deleting
	drainUntil(t, eventCh, ns, "to-delete", eventTimeout)

	if err := store.Delete(context.Background(), ns, "to-delete"); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	deadline := time.After(eventTimeout)
	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			if event.Object.GetNamespace() != ns || event.Object.GetName() != "to-delete" {
				continue
			}
			if event.Type == storage.EventDeleted {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for DELETE event")
		}
	}
}

func TestBroadcaster_UpdateEvent(t *testing.T) {
	_, store := setupBroadcasterWithStore(t)
	ns := uniqueNS("bc-update")

	obj := newTestObject(withName("to-update"), withNamespace(ns))
	if err := store.Create(context.Background(), obj); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	eventCh, stop, err := store.broadcaster.Subscribe("")
	if err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}
	defer stop()

	// Wait for the CREATE event before updating
	drainUntil(t, eventCh, ns, "to-update", eventTimeout)

	obj.Object["spec"] = map[string]any{"updated": true}
	if err := store.Update(context.Background(), obj); err != nil {
		t.Fatalf("Update() failed: %v", err)
	}

	deadline := time.After(eventTimeout)
	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			if event.Object.GetNamespace() != ns || event.Object.GetName() != "to-update" {
				continue
			}
			if event.Type != storage.EventModified {
				continue // might be the ADDED from create, keep draining
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for MODIFIED event")
		}
	}
}

func TestBroadcaster_CloseShutdown(t *testing.T) {
	broadcaster, _ := setupBroadcasterWithStore(t)

	eventCh, _, err := broadcaster.Subscribe("")
	if err != nil {
		t.Fatalf("Subscribe() failed: %v", err)
	}

	if err := broadcaster.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	select {
	case _, ok := <-eventCh:
		if ok {
			select {
			case _, ok := <-eventCh:
				if ok {
					t.Error("channel still open after Close and drain")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("channel not closed after Close")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after Close")
	}

	_, _, err = broadcaster.Subscribe("")
	if err == nil {
		t.Error("expected error subscribing to closed broadcaster")
	}
}
