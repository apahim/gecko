package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var validatingObjectGVK = schema.GroupVersionKind{
	Group:   "test.orlop.gcp.managed.openshift.io",
	Version: "v1",
	Kind:    "ValidatingObject",
}

// validatingObject mirrors the real private API types: its ValidateUpdate
// type-asserts the old object to its own concrete type before using it.
type validatingObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              mockSpec `json:"spec"`
}

func (o *validatingObject) DeepCopyObject() runtime.Object {
	if o == nil {
		return nil
	}
	out := &validatingObject{}
	*out = *o
	out.TypeMeta = o.TypeMeta
	o.DeepCopyInto(&out.ObjectMeta)
	out.Spec = o.Spec
	return out
}

func (o *validatingObject) ValidateCreate(_ context.Context) error { return nil }

func (o *validatingObject) ValidateDelete(_ context.Context) error { return nil }

func (o *validatingObject) ValidateUpdate(_ context.Context, oldObj runtime.Object) error {
	if _, ok := oldObj.(*validatingObject); !ok {
		return fmt.Errorf("expected old object to be *validatingObject, got %T", oldObj)
	}
	return nil
}

func newValidatingObjectHandler(t *testing.T) (*ResourceHandler, *memory.MemoryStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(validatingObjectGVK, &validatingObject{})
	metav1.AddToGroupVersion(scheme, validatingObjectGVK.GroupVersion())

	store := memory.NewMemoryStore("validatingobjects", scheme, validatingObjectGVK)
	handler := NewResourceHandler(
		store,
		newPermissiveProcessor(t),
		validatingObjectGVK,
		"validatingobjects",
		scheme,
		logr.Discard(),
	)
	return handler, store
}

// seedStoredUnstructured writes the object in the form the postgres and spanner
// stores hand back: they decode every persisted row into
// *unstructured.Unstructured, never into the typed object.
func seedStoredUnstructured(t *testing.T, store *memory.MemoryStore) *unstructured.Unstructured {
	t.Helper()
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": validatingObjectGVK.GroupVersion().String(),
		"kind":       validatingObjectGVK.Kind,
		"metadata": map[string]interface{}{
			"name":      "test-obj",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"field": "old-value",
		},
	}}
	if err := store.Create(context.Background(), existing); err != nil {
		t.Fatalf("failed to seed existing object: %v", err)
	}
	return existing
}

func newRouteRequest(t *testing.T, method, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/test", bytes.NewReader([]byte(body)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(constants.URLParamNamespace, "default")
	rctx.URLParams.Add(constants.URLParamName, "test-obj")
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// A CustomValidator must be handed the typed old object on the Update path.
// The store returns unstructured, so without conversion every validator that
// asserts its concrete type rejects every update.
func TestResourceHandlerUpdate_ValidatorReceivesTypedOldObject(t *testing.T) {
	handler, store := newValidatingObjectHandler(t)
	existing := seedStoredUnstructured(t, store)

	body, err := json.Marshal(map[string]interface{}{
		"apiVersion": validatingObjectGVK.GroupVersion().String(),
		"kind":       validatingObjectGVK.Kind,
		"metadata": map[string]interface{}{
			"name":            "test-obj",
			"namespace":       "default",
			"resourceVersion": existing.GetResourceVersion(),
		},
		"spec": map[string]interface{}{"field": "new-value"},
	})
	if err != nil {
		t.Fatalf("failed to marshal body: %v", err)
	}

	rr := httptest.NewRecorder()
	handler.Update(rr, newRouteRequest(t, http.MethodPut, string(body)))

	if rr.Code != http.StatusOK {
		t.Fatalf("Update status = %d, want %d, body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

// Same contract on the Patch path, which reads the old object from the same store.
func TestResourceHandlerPatch_ValidatorReceivesTypedOldObject(t *testing.T) {
	handler, store := newValidatingObjectHandler(t)
	seedStoredUnstructured(t, store)

	req := newRouteRequest(t, http.MethodPatch, `{"spec":{"field":"patched"}}`)
	req.Header.Set("Content-Type", "application/merge-patch+json")

	rr := httptest.NewRecorder()
	handler.Patch(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Patch status = %d, want %d, body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

var validatingObjectStorageGVK = schema.GroupVersionKind{
	Group:   "test.orlop.gcp.managed.openshift.io",
	Version: "v2",
	Kind:    "ValidatingObject",
}

// validatingObjectStorage is the stored version of validatingObject, used to
// exercise the handler's serving-vs-storage version split.
type validatingObjectStorage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              mockSpec `json:"spec"`
}

func (o *validatingObjectStorage) DeepCopyObject() runtime.Object {
	if o == nil {
		return nil
	}
	out := &validatingObjectStorage{}
	*out = *o
	out.TypeMeta = o.TypeMeta
	o.DeepCopyInto(&out.ObjectMeta)
	out.Spec = o.Spec
	return out
}

func newVersionedValidatingObjectHandler(t *testing.T) (*ResourceHandler, *memory.MemoryStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(validatingObjectGVK, &validatingObject{})
	scheme.AddKnownTypeWithName(validatingObjectStorageGVK, &validatingObjectStorage{})
	metav1.AddToGroupVersion(scheme, validatingObjectGVK.GroupVersion())
	metav1.AddToGroupVersion(scheme, validatingObjectStorageGVK.GroupVersion())

	store := memory.NewMemoryStore("validatingobjects", scheme, validatingObjectStorageGVK)
	handler := NewResourceHandler(
		store,
		newPermissiveProcessor(t),
		validatingObjectGVK,
		"validatingobjects",
		scheme,
		logr.Discard(),
	)
	handler.SetStorageGVK(validatingObjectStorageGVK)
	return handler, store
}

// When the handler serves a different version than it stores, the old object
// handed to the validator must be the serving version. Patch already converts
// before validating; Update must do the same, otherwise a serving-version
// validator is handed the storage-version object.
func TestResourceHandlerUpdate_ValidatorReceivesServingVersionOldObject(t *testing.T) {
	handler, store := newVersionedValidatingObjectHandler(t)

	existing := &validatingObjectStorage{
		TypeMeta: metav1.TypeMeta{
			APIVersion: validatingObjectStorageGVK.GroupVersion().String(),
			Kind:       validatingObjectStorageGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: "test-obj", Namespace: "default"},
		Spec:       mockSpec{Field: "old-value"},
	}
	if err := store.Create(context.Background(), existing); err != nil {
		t.Fatalf("failed to seed existing object: %v", err)
	}

	body, err := json.Marshal(map[string]interface{}{
		"apiVersion": validatingObjectGVK.GroupVersion().String(),
		"kind":       validatingObjectGVK.Kind,
		"metadata": map[string]interface{}{
			"name":            "test-obj",
			"namespace":       "default",
			"resourceVersion": existing.GetResourceVersion(),
		},
		"spec": map[string]interface{}{"field": "new-value"},
	})
	if err != nil {
		t.Fatalf("failed to marshal body: %v", err)
	}

	rr := httptest.NewRecorder()
	handler.Update(rr, newRouteRequest(t, http.MethodPut, string(body)))

	if rr.Code != http.StatusOK {
		t.Fatalf("Update status = %d, want %d, body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}
