package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-logr/logr"

	privatev1 "github.com/openshift-online/gecko/orlop/apis/private/test/v1"
	privatev2 "github.com/openshift-online/gecko/orlop/apis/private/test/v2"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/apply"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
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

func TestApplyPatchValidatesServingVersionOfStoredObject(t *testing.T) {
	for _, test := range []struct {
		name, oldValue, newValue string
		wantStatus               int
	}{
		{name: "valid update", oldValue: "original", newValue: "updated", wantStatus: http.StatusOK},
		{name: "invalid new value", oldValue: "original", newValue: "invalid-update-new", wantStatus: http.StatusBadRequest},
		{name: "invalid old value", oldValue: "invalid-update-old", newValue: "updated", wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _ := newApplyValidationHandler(t)
			if err := privatev2.AddToScheme(h.scheme); err != nil {
				t.Fatal(err)
			}
			// managedfields uses the scheme to convert the live storage object
			// while merging; the handler must also convert it for validation.
			if err := h.scheme.AddConversionFunc((*privatev2.Object)(nil), (*privatev1.Object)(nil), func(a, b interface{}, _ conversion.Scope) error {
				old, next := a.(*privatev2.Object), b.(*privatev1.Object)
				next.ObjectMeta = *old.ObjectMeta.DeepCopy()
				next.Spec.PublicField = old.Spec.PublicField
				next.SetGroupVersionKind(gvkV1)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			h.SetStorageGVK(gvkV2)
			store := memory.NewMemoryStore("objects", h.scheme, gvkV2)
			h.store = store
			seed := &privatev2.Object{
				TypeMeta:   metav1.TypeMeta{APIVersion: gvkV2.GroupVersion().String(), Kind: gvkV2.Kind},
				ObjectMeta: metav1.ObjectMeta{Name: "cross-version", Namespace: "default"},
				Spec:       privatev2.ObjectSpec{PublicField: test.oldValue},
			}
			if err := store.Create(t.Context(), seed); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(t.Context(), "default", seed.Name)
			if err != nil {
				t.Fatal(err)
			}
			rr := applyValidationRequest(h, seed.Name, applyConfigBody(seed.Name, test.newValue), true)
			if rr.Code != test.wantStatus {
				t.Fatalf("apply update = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body)
			}
			after, err := store.Get(t.Context(), "default", seed.Name)
			if err != nil {
				t.Fatal(err)
			}
			stored := after.(*privatev2.Object)
			if test.wantStatus == http.StatusOK {
				if stored.Spec.PublicField != test.newValue {
					t.Fatalf("stored value = %q, want %q", stored.Spec.PublicField, test.newValue)
				}
				var served privatev1.Object
				if err := json.Unmarshal(rr.Body.Bytes(), &served); err != nil {
					t.Fatal(err)
				}
				if served.APIVersion != gvkV1.GroupVersion().String() || served.Spec.PublicField != test.newValue {
					t.Fatalf("unexpected serving-version response: %+v", served)
				}
			} else {
				if !strings.Contains(rr.Body.String(), "spec.publicField is rejected by update validation") {
					t.Fatalf("update was not rejected by the serving-version field validator: %s", rr.Body)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected apply update changed persisted state")
				}
			}
		})
	}
}
