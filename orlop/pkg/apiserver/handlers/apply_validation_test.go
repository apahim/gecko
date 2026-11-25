package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-logr/logr"

	privatev1 "github.com/openshift-online/gecko/orlop/apis/private/test/v1"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/apply"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

// newApplyValidationHandler builds a ResourceHandler wired with a memory store
// and an apply manager so ApplyPatch can be exercised end to end.
func newApplyValidationHandler(t *testing.T) (*ResourceHandler, *memory.MemoryStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := privatev1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	props := &apiext.JSONSchemaProps{Type: "object", Properties: map[string]apiext.JSONSchemaProps{
		"apiVersion": {Type: "string"},
		"kind":       {Type: "string"},
		"metadata":   {Type: "object", XPreserveUnknownFields: ptr.To(true)},
		"spec":       {Type: "object", XPreserveUnknownFields: ptr.To(true)},
		"status":     {Type: "object", XPreserveUnknownFields: ptr.To(true)},
	}}
	structural, err := structuralschema.NewStructural(props)
	if err != nil {
		t.Fatal(err)
	}
	applyMgr, err := apply.NewManager(scheme, structural, gvkV1)
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewMemoryStore("objects", scheme, gvkV1)
	h := &ResourceHandler{
		gvk:          gvkV1,
		scheme:       scheme,
		store:        store,
		applyManager: applyMgr,
		logger:       logr.Discard(),
	}
	return h, store
}

func applyValidationRequest(h *ResourceHandler, name, body string, force bool) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Patch("/namespaces/{namespace}/objects/{name}", h.ApplyPatch)
	url := "/namespaces/default/objects/" + name + "?fieldManager=test-controller"
	if force {
		url += "&force=true"
	}
	req := httptest.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/apply-patch+yaml")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func applyConfigBody(name, publicField string) string {
	return `apiVersion: test.orlop.gcp.managed.openshift.io/v1
kind: Object
metadata:
  name: ` + name + `
spec:
  publicField: ` + publicField + `
`
}

// TestApplyPatchRunsCreateValidation verifies that a server-side apply which
// creates an object runs the type's ValidateCreate hook and rejects the write
// before it reaches storage.
func TestApplyPatchRunsCreateValidation(t *testing.T) {
	h, store := newApplyValidationHandler(t)

	rr := applyValidationRequest(h, "apply-rejected-create", applyConfigBody("apply-rejected-create", "invalid-create-value"), false)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for apply-create rejected by ValidateCreate, got %d: %s", rr.Code, rr.Body)
	}
	if obj, err := store.Get(t.Context(), "default", "apply-rejected-create"); err == nil {
		t.Fatalf("expected no object stored after rejected apply-create, got %#v", obj)
	}
}

// TestApplyPatchRunsUpdateValidation verifies that a server-side apply which
// updates an object runs the type's ValidateUpdate hook with the stored object
// and rejects the write before it reaches storage.
func TestApplyPatchRunsUpdateValidation(t *testing.T) {
	h, store := newApplyValidationHandler(t)

	seed := &privatev1.Object{
		TypeMeta:   metav1.TypeMeta{APIVersion: gvkV1.GroupVersion().String(), Kind: gvkV1.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "apply-rejected-update", Namespace: "default"},
		Spec:       privatev1.ObjectSpec{PublicField: "original"},
	}
	if err := store.Create(t.Context(), seed); err != nil {
		t.Fatal(err)
	}

	rr := applyValidationRequest(h, "apply-rejected-update", applyConfigBody("apply-rejected-update", "invalid-update-value"), true)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for apply-update rejected by ValidateUpdate, got %d: %s", rr.Code, rr.Body)
	}
	got, err := store.Get(t.Context(), "default", "apply-rejected-update")
	if err != nil {
		t.Fatal(err)
	}
	if stored := got.(*privatev1.Object); stored.Spec.PublicField != "original" {
		t.Fatalf("expected stored object unchanged after rejected apply-update, got spec.publicField=%q", stored.Spec.PublicField)
	}
}

// TestApplyPatchAcceptsValidObjects is the positive control: applies that pass
// validation must still create and update objects.
func TestApplyPatchAcceptsValidObjects(t *testing.T) {
	h, store := newApplyValidationHandler(t)

	rr := applyValidationRequest(h, "apply-valid", applyConfigBody("apply-valid", "created-via-apply"), false)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 for valid apply-create, got %d: %s", rr.Code, rr.Body)
	}

	rr = applyValidationRequest(h, "apply-valid", applyConfigBody("apply-valid", "updated-via-apply"), true)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid apply-update, got %d: %s", rr.Code, rr.Body)
	}
	got, err := store.Get(t.Context(), "default", "apply-valid")
	if err != nil {
		t.Fatal(err)
	}
	if stored := got.(*privatev1.Object); stored.Spec.PublicField != "updated-via-apply" {
		t.Fatalf("expected stored spec.publicField=updated-via-apply, got %q", stored.Spec.PublicField)
	}
}
