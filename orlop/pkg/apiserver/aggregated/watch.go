package aggregated

import (
	"sync"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

type watchAdapter struct {
	eventCh  <-chan storage.ResourceEvent
	stopFn   func()
	result   chan watch.Event
	done     chan struct{}
	stopOnce sync.Once
}

func newWatchAdapter(eventCh <-chan storage.ResourceEvent, stopFn func()) watch.Interface {
	w := &watchAdapter{
		eventCh: eventCh,
		stopFn:  stopFn,
		result:  make(chan watch.Event, 100),
		done:    make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *watchAdapter) run() {
	defer close(w.result)
	for event := range w.eventCh {
		var eventType watch.EventType
		switch event.Type {
		case storage.EventAdded:
			eventType = watch.Added
		case storage.EventModified:
			eventType = watch.Modified
		case storage.EventDeleted:
			eventType = watch.Deleted
		case storage.EventBookmark:
			eventType = watch.Bookmark
		default:
			continue
		}
		select {
		case w.result <- watch.Event{Type: eventType, Object: event.Object}:
		case <-w.done:
			return
		}
	}
}

func (w *watchAdapter) Stop() {
	w.stopOnce.Do(func() {
		w.stopFn()
		close(w.done)
	})
}
func (w *watchAdapter) ResultChan() <-chan watch.Event {
	return w.result
}

var _ watch.Interface = (*watchAdapter)(nil)

// initialEventsWatch wraps a storage watch and prepends initial ADDED events
// plus a BOOKMARK with the k8s.io/initial-events-end annotation.
// Required for controller-runtime's reflector streaming list protocol.
type initialEventsWatch struct {
	items    []runtime.Object
	bookmark runtime.Object
	eventCh  <-chan storage.ResourceEvent
	stopFn   func()
	result   chan watch.Event
	done     chan struct{}
	stopOnce sync.Once
}

func newInitialEventsWatch(items []runtime.Object, bookmark runtime.Object, eventCh <-chan storage.ResourceEvent, stopFn func()) watch.Interface {
	w := &initialEventsWatch{
		items:    items,
		bookmark: bookmark,
		eventCh:  eventCh,
		stopFn:   stopFn,
		result:   make(chan watch.Event, 100),
		done:     make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *initialEventsWatch) run() {
	defer close(w.result)

	for _, item := range w.items {
		select {
		case w.result <- watch.Event{Type: watch.Added, Object: item}:
		case <-w.done:
			return
		}
	}

	select {
	case w.result <- watch.Event{Type: watch.Bookmark, Object: w.bookmark}:
	case <-w.done:
		return
	}

	for event := range w.eventCh {
		var eventType watch.EventType
		switch event.Type {
		case storage.EventAdded:
			eventType = watch.Added
		case storage.EventModified:
			eventType = watch.Modified
		case storage.EventDeleted:
			eventType = watch.Deleted
		case storage.EventBookmark:
			eventType = watch.Bookmark
		default:
			continue
		}
		select {
		case w.result <- watch.Event{Type: eventType, Object: event.Object}:
		case <-w.done:
			return
		}
	}
}

func (w *initialEventsWatch) Stop() {
	w.stopOnce.Do(func() {
		w.stopFn()
		close(w.done)
	})
}

func (w *initialEventsWatch) ResultChan() <-chan watch.Event {
	return w.result
}

var _ watch.Interface = (*initialEventsWatch)(nil)
