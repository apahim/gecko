package aggregated

import (
	"net/http"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// typedWatch projects persisted unstructured data into the registered API type.
// Removed fields must not leak through watch when GET/LIST no longer serve them.
func (s *ResourceStorage) typedWatch(source watch.Interface) watch.Interface {
	return watch.Filter(source, func(event watch.Event) (watch.Event, bool) {
		if event.Type == watch.Error {
			return event, true
		}
		if event.Object == nil {
			// A storage bookmark without object metadata carries no usable RV.
			if event.Type == watch.Bookmark {
				return event, false
			}
			return invalidServingWatchEvent(), true
		}
		typed, err := conversion.TypedOldObject(s.scheme, s.gvk, event.Object.DeepCopyObject())
		if err != nil {
			return invalidServingWatchEvent(), true
		}
		typed.GetObjectKind().SetGroupVersionKind(s.gvk)
		event.Object = typed
		return event, true
	})
}

func invalidServingWatchEvent() watch.Event {
	return watch.Event{Type: watch.Error, Object: &metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonInternalError, Code: http.StatusInternalServerError, Message: "unable to represent stored resource"}}
}
