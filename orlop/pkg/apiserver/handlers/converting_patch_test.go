package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type publicPatchStore struct {
	storage.ResourceStore
	updates      int
	beforeUpdate func()
}

func (s *publicPatchStore) Update(ctx context.Context, obj client.Object) error {
	s.updates++
	if s.beforeUpdate != nil {
		s.beforeUpdate()
	}
	return s.ResourceStore.Update(ctx, obj)
}

func publicPatchFixture(t *testing.T) (*ConvertingResourceHandler, *publicPatchStore, *object) {
	t.Helper()
	h, memory := setupStatusStrippingTest(t)
	h.converter = conversion.NewConverter(h.publicScheme, h.privateScheme, "private.test/")
	existing := &object{
		TypeMeta: metav1.TypeMeta{APIVersion: objectGVK.GroupVersion().String(), Kind: objectGVK.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name: "patch", Namespace: "default", UID: "original-uid", Generation: 4,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)),
			Labels:            map[string]string{"keep": "original", "private.test/owner": "controller"},
			Annotations:       map[string]string{"private.test/secret": "retained"},
			Finalizers:        []string{"private.test/finalizer"},
		},
		Spec: objectSpec{Field: "original", Value: 9007199254740993},
		Status: objectStatus{
			PlacementResult: &statusPlacementResult{ManagementClusterName: "controller"},
			Conditions:      []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Controller", LastTransitionTime: metav1.NewTime(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))}},
		},
	}
	if err := memory.Create(t.Context(), existing); err != nil {
		t.Fatal(err)
	}
	store := &publicPatchStore{ResourceStore: memory}
	h.store = store
	return h, store, existing
}

func publicPatchRequest(t *testing.T, h *ConvertingResourceHandler, ctx context.Context, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Patch("/namespaces/{namespace}/objects/{name}", h.Patch)
	req := httptest.NewRequestWithContext(ctx, http.MethodPatch, "/namespaces/default/objects/patch", strings.NewReader(body))
	req.Header.Set(constants.HeaderContentType, contentType)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestConvertingJSONPatchSuccess(t *testing.T) {
	h, store, before := publicPatchFixture(t)
	patch := `[{"op":"test","path":"/metadata/uid","value":"original-uid"},{"op":"test","path":"/metadata/resourceVersion","value":"` + before.ResourceVersion + `"},{"op":"replace","path":"/spec/field","value":"updated"},{"op":"add","path":"/metadata/labels/new.example~1key","value":"label"}]`
	rr := publicPatchRequest(t, h, t.Context(), "application/json-patch+json; charset=utf-8", patch)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rr.Code, rr.Body)
	}
	got, err := store.Get(t.Context(), "default", "patch")
	if err != nil {
		t.Fatal(err)
	}
	stored := got.(*object)
	if store.updates != 1 || stored.Spec.Field != "updated" || stored.Spec.Value != before.Spec.Value || stored.Generation != before.Generation+1 || stored.ResourceVersion == before.ResourceVersion {
		t.Fatalf("unexpected persisted result: %#v", stored)
	}
	if stored.Labels["new.example/key"] != "label" || stored.Labels["private.test/owner"] != "controller" || !apiequality.Semantic.DeepEqual(stored.Status, before.Status) || !reflect.DeepEqual(stored.Finalizers, before.Finalizers) || !reflect.DeepEqual(stored.Annotations, before.Annotations) || stored.UID != before.UID {
		t.Fatalf("lost protected fields: %#v", stored)
	}
	var response object
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Spec != stored.Spec || response.ResourceVersion != stored.ResourceVersion || response.Labels["private.test/owner"] != "" || len(response.Finalizers) != 0 {
		t.Fatalf("invalid public response: %#v", response)
	}
}

func TestConvertingJSONPatchSpecificFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, marker string
		code               int
	}{
		{"uid", `[{"op":"test","path":"/metadata/uid","value":"wrong-secret-uid"},{"op":"replace","path":"/spec/field","value":"must-not-write"}]`, "JSON patch test failed", http.StatusConflict},
		{"rv", `[{"op":"test","path":"/metadata/resourceVersion","value":"0"},{"op":"replace","path":"/spec/field","value":"must-not-write"}]`, "JSON patch test failed", http.StatusConflict},
		{"atomic", `[{"op":"replace","path":"/spec/field","value":"must-not-write"},{"op":"test","path":"/metadata/uid","value":"wrong-secret-uid"}]`, "JSON patch test failed", http.StatusConflict},
		{"object not array", `{}`, "invalid JSON patch", http.StatusBadRequest},
		{"null", `null`, "invalid JSON patch", http.StatusBadRequest},
		{"malformed", `[`, "invalid JSON patch", http.StatusBadRequest},
		{"bad operation", `[{"op":"unknown","path":"/spec/field"}]`, "invalid JSON patch", http.StatusBadRequest},
		{"missing path", `[{"op":"replace","path":"/spec/missing","value":"secret-value"}]`, "JSON patch operation failed", http.StatusBadRequest},
		{"non object result", `[{"op":"replace","path":"","value":null}]`, "patched resource must be an object", http.StatusBadRequest},
		{"negative array index", `[{"op":"remove","path":"/status/conditions/-1"}]`, "JSON patch operation failed", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, before := publicPatchFixture(t)
			var logs bytes.Buffer
			h.logger = logr.FromSlogHandler(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, tc.body)
			if rr.Code != tc.code || !strings.Contains(rr.Body.String(), tc.marker) {
				t.Fatalf("PATCH: %d %s", rr.Code, rr.Body)
			}
			if strings.Contains(rr.Body.String(), "secret") || strings.Contains(rr.Body.String(), "original-uid") {
				t.Fatal("patch failure leaked values")
			}
			if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "original-uid") || strings.Contains(logs.String(), "must-not-write") {
				t.Fatal("patch failure logged request values")
			}
			after, err := store.Get(t.Context(), "default", "patch")
			if err != nil {
				t.Fatal(err)
			}
			if store.updates != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("failed patch mutated store")
			}
		})
	}
}

func TestConvertingJSONPatchAuthorizationPrecedesTests(t *testing.T) {
	h, store, _ := publicPatchFixture(t)
	calls := 0
	ctx := WithObjectAuthorizer(t.Context(), func(context.Context, client.Object) (bool, error) { calls++; return false, nil })
	rr := publicPatchRequest(t, h, ctx, constants.ContentTypeJSONPatch, `[{"op":"test","path":"/metadata/uid","value":"wrong"}]`)
	if rr.Code != http.StatusForbidden || calls != 1 || store.updates != 0 {
		t.Fatalf("authorization: calls=%d updates=%d response=%d %s", calls, store.updates, rr.Code, rr.Body)
	}
}

func TestConvertingJSONPatchResultAuthorization(t *testing.T) {
	h, store, before := publicPatchFixture(t)
	calls := 0
	ctx := WithObjectAuthorizer(t.Context(), func(_ context.Context, obj client.Object) (bool, error) {
		calls++
		return obj.(*object).Spec.Field == "original", nil
	})
	rr := publicPatchRequest(t, h, ctx, constants.ContentTypeJSONPatch, `[{"op":"replace","path":"/spec/field","value":"unauthorized"}]`)
	after, err := store.Get(t.Context(), "default", "patch")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusForbidden || calls != 2 || store.updates != 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("result authorization: %d %s", rr.Code, rr.Body)
	}
}

func TestConvertingJSONPatchPreservesStatus(t *testing.T) {
	for _, patch := range []string{
		`[{"op":"replace","path":"/status/placementResult/managementClusterName","value":"forged"}]`,
		`[{"op":"remove","path":"/status"}]`,
		`[{"op":"copy","from":"/spec","path":"/status"}]`,
		`[{"op":"move","from":"/status/placementResult/managementClusterName","path":"/spec/field"}]`,
		`[{"op":"add","path":"/Status","value":{"placementResult":{"managementClusterName":"forged"}}},{"op":"add","path":"/STATUS","value":{}}]`,
		`[{"op":"copy","from":"","path":"/snapshot"},{"op":"replace","path":"/snapshot/status","value":{"placementResult":{"managementClusterName":"forged"}}},{"op":"move","from":"/snapshot","path":""}]`,
	} {
		t.Run(patch, func(t *testing.T) {
			h, store, before := publicPatchFixture(t)
			rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, patch)
			if rr.Code != http.StatusOK {
				t.Fatalf("PATCH: %d %s", rr.Code, rr.Body)
			}
			after, err := store.Get(t.Context(), "default", "patch")
			if err != nil {
				t.Fatal(err)
			}
			if !apiequality.Semantic.DeepEqual(before.Status, after.(*object).Status) {
				t.Fatal("controller status changed")
			}
		})
	}
}

func TestConvertingJSONPatchRejectsIdentityChanges(t *testing.T) {
	for _, patch := range []string{
		`[{"op":"remove","path":"/metadata/resourceVersion"}]`,
		`[{"op":"replace","path":"/metadata/resourceVersion","value":"0"}]`,
		`[{"op":"replace","path":"/metadata/uid","value":"other"}]`,
		`[{"op":"replace","path":"/metadata/name","value":"other"}]`,
		`[{"op":"replace","path":"/metadata/namespace","value":"other"}]`,
		`[{"op":"replace","path":"/metadata/generation","value":999}]`,
		`[{"op":"replace","path":"/metadata/creationTimestamp","value":"2026-01-01T00:00:00Z"}]`,
		`[{"op":"add","path":"/metadata/deletionTimestamp","value":"2026-01-01T00:00:00Z"}]`,
		`[{"op":"replace","path":"/kind","value":"Other"}]`,
		`[{"op":"copy","from":"/metadata/uid","path":"/metadata/name"}]`,
		`[{"op":"move","from":"/metadata/uid","path":"/metadata/labels/copied"}]`,
		`[{"op":"replace","path":"/metadata","value":{"name":"patch","namespace":"default"}}]`,
		`[{"op":"add","path":"/metadata/finalizers","value":["forged"]}]`,
		`[{"op":"add","path":"/metadata/ownerReferences","value":[{"uid":"forged"}]}]`,
	} {
		t.Run(patch, func(t *testing.T) {
			h, store, before := publicPatchFixture(t)
			rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, patch)
			after, err := store.Get(t.Context(), "default", "patch")
			if err != nil {
				t.Fatal(err)
			}
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "server-managed") || store.updates != 0 || !reflect.DeepEqual(before, after) {
				t.Fatalf("identity PATCH: %d %s", rr.Code, rr.Body)
			}
		})
	}
}

func TestConvertingJSONPatchStorageRace(t *testing.T) {
	h, store, before := publicPatchFixture(t)
	store.beforeUpdate = func() {
		competing := before.DeepCopyObject().(*object)
		competing.Spec.Field = "concurrent"
		if err := store.ResourceStore.Update(t.Context(), competing); err != nil {
			t.Fatal(err)
		}
	}
	rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, `[{"op":"test","path":"/metadata/resourceVersion","value":"`+before.ResourceVersion+`"},{"op":"replace","path":"/spec/field","value":"overwrite"}]`)
	after, err := store.Get(t.Context(), "default", "patch")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusConflict || store.updates != 1 || after.(*object).Spec.Field != "concurrent" {
		t.Fatalf("race: %d %s", rr.Code, rr.Body)
	}
}

func TestConvertingMergePatchCompatibility(t *testing.T) {
	for _, media := range []string{"", "application/json", constants.ContentTypeMergePatch, constants.ContentTypeStrategicMergePatch, constants.ContentTypeMergePatch + "; charset=utf-8", "text/plain", "application/x-www-form-urlencoded", "application/apply-patch+json", "application/json-patch+json; broken"} {
		t.Run(media, func(t *testing.T) {
			h, store, before := publicPatchFixture(t)
			rr := publicPatchRequest(t, h, t.Context(), media, `{"spec":{"field":"merged"},"status":{"placementResult":{"managementClusterName":"forged"}}}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("merge PATCH: %d %s", rr.Code, rr.Body)
			}
			after, err := store.Get(t.Context(), "default", "patch")
			if err != nil {
				t.Fatal(err)
			}
			if after.(*object).Spec.Field != "merged" || !apiequality.Semantic.DeepEqual(before.Status, after.(*object).Status) {
				t.Fatal("merge regression")
			}
		})
	}
}

// privateObject is the stored private counterpart of object: the same public
// members plus spec fields that exist only in the private schema.
type privateObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              privateSpec  `json:"spec,omitempty"`
	Status            objectStatus `json:"status,omitempty"`
}

type privateSpec struct {
	Field    string `json:"field,omitempty"`
	Value    int    `json:"value,omitempty"`
	SafeName string `json:"safeName,omitempty"`
}

func (m *privateObject) DeepCopyObject() runtime.Object {
	if m == nil {
		return nil
	}
	out := &privateObject{}
	*out = *m
	out.TypeMeta = m.TypeMeta
	m.DeepCopyInto(&out.ObjectMeta)
	out.Spec = m.Spec
	out.Status = m.Status
	return out
}

// privateSpecPatchFixture builds a converting handler over distinct public and
// private schemes so private-only spec members are actually exercised.
func privateSpecPatchFixture(t *testing.T) (*ConvertingResourceHandler, *publicPatchStore, *privateObject) {
	t.Helper()
	publicScheme := newObjectScheme()
	privateScheme := runtime.NewScheme()
	privateScheme.AddKnownTypeWithName(objectGVK, &privateObject{})
	metav1.AddToGroupVersion(privateScheme, objectGVK.GroupVersion())

	memoryStore := memory.NewMemoryStore("objects", privateScheme, objectGVK)
	existing := &privateObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: objectGVK.GroupVersion().String(), Kind: objectGVK.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "patch", Namespace: "default", UID: "original-uid", Generation: 4, Finalizers: []string{"private.test/finalizer"}},
		Spec:       privateSpec{Field: "original", Value: 7, SafeName: "controller-set"},
		Status:     objectStatus{PlacementResult: &statusPlacementResult{ManagementClusterName: "controller"}},
	}
	if err := memoryStore.Create(t.Context(), existing); err != nil {
		t.Fatal(err)
	}
	store := &publicPatchStore{ResourceStore: memoryStore}
	h := NewConvertingResourceHandler(
		store,
		newPermissiveProcessor(t),
		conversion.NewConverter(publicScheme, privateScheme, "private.test/"),
		objectGVK,
		"objects",
		publicScheme,
		privateScheme,
		nil,
		logr.FromSlogHandler(testLogHandler{t: t}),
	)
	return h, store, existing
}

func TestConvertingJSONPatchPreservesPrivateOnlySpecMembers(t *testing.T) {
	for _, patch := range []string{
		`[{"op":"replace","path":"/spec/field","value":"patched"}]`,
		`[{"op":"replace","path":"/spec","value":{"field":"patched"}}]`,
	} {
		t.Run(patch, func(t *testing.T) {
			h, store, before := privateSpecPatchFixture(t)
			rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, patch)
			if rr.Code != http.StatusOK {
				t.Fatalf("PATCH: %d %s", rr.Code, rr.Body)
			}
			after, err := store.Get(t.Context(), "default", "patch")
			if err != nil {
				t.Fatal(err)
			}
			stored := after.(*privateObject)
			if stored.Spec.SafeName != before.Spec.SafeName {
				t.Fatalf("private-only spec member changed: %q", stored.Spec.SafeName)
			}
			if stored.Spec.Field != "patched" {
				t.Fatalf("public spec member not patched: %#v", stored.Spec)
			}
		})
	}

	// Root replacement must also seed the private spec from the stored object.
	// json-patch v5 only replaces the document through `replace` at ""; `move`
	// or `copy` to "" stashes the value under a "" key that pruning removes,
	// so those destinations are silent no-ops rather than replacements.
	t.Run("root replacement", func(t *testing.T) {
		h, store, before := privateSpecPatchFixture(t)
		router := chi.NewRouter()
		router.Get("/namespaces/{namespace}/objects/{name}", h.Get)
		router.Patch("/namespaces/{namespace}/objects/{name}", h.Patch)

		getRR := httptest.NewRecorder()
		router.ServeHTTP(getRR, httptest.NewRequest(http.MethodGet, "/namespaces/default/objects/patch", nil))
		if getRR.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", getRR.Code, getRR.Body)
		}
		var current map[string]interface{}
		if err := json.Unmarshal(getRR.Body.Bytes(), &current); err != nil {
			t.Fatal(err)
		}
		spec, _ := current["spec"].(map[string]interface{})
		spec["field"] = "patched"
		replacement, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodPatch, "/namespaces/default/objects/patch", strings.NewReader(`[{"op":"replace","path":"","value":`+string(replacement)+`}]`))
		req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSONPatch)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH: %d %s", rr.Code, rr.Body)
		}
		after, err := store.Get(t.Context(), "default", "patch")
		if err != nil {
			t.Fatal(err)
		}
		stored := after.(*privateObject)
		if stored.Spec.SafeName != before.Spec.SafeName {
			t.Fatalf("private-only spec member changed: %q", stored.Spec.SafeName)
		}
		if stored.Spec.Field != "patched" {
			t.Fatalf("public spec member not patched: %#v, response: %s", stored.Spec, rr.Body.String())
		}
	})
}

func TestConvertingMutationRejectsInvalidResourceVersion(t *testing.T) {
	t.Run("merge patch", func(t *testing.T) {
		h, store, before := publicPatchFixture(t)
		rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeMergePatch, `{"metadata":{"resourceVersion":"abc"},"spec":{"field":"must-not-write"}}`)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid resourceVersion") {
			t.Fatalf("merge PATCH: %d %s", rr.Code, rr.Body)
		}
		after, err := store.Get(t.Context(), "default", "patch")
		if err != nil {
			t.Fatal(err)
		}
		if after.(*object).Spec.Field != before.Spec.Field {
			t.Fatal("rejected merge patch changed the stored object")
		}
	})

	t.Run("put", func(t *testing.T) {
		h, store, before := publicPatchFixture(t)
		router := chi.NewRouter()
		router.Put("/namespaces/{namespace}/objects/{name}", h.Update)
		body := `{"apiVersion":"test.io/v1","kind":"Object","metadata":{"name":"patch","namespace":"default","resourceVersion":"abc"},"spec":{"field":"must-not-write"}}`
		req := httptest.NewRequest(http.MethodPut, "/namespaces/default/objects/patch", strings.NewReader(body))
		req.Header.Set(constants.HeaderContentType, constants.ContentTypeJSON)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid resourceVersion") {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body)
		}
		after, err := store.Get(t.Context(), "default", "patch")
		if err != nil {
			t.Fatal(err)
		}
		if after.(*object).Spec.Field != before.Spec.Field {
			t.Fatal("rejected PUT changed the stored object")
		}
	})
}

func TestConvertingJSONPatchNoOpGeneration(t *testing.T) {
	h, store, before := publicPatchFixture(t)
	rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, `[{"op":"test","path":"/metadata/uid","value":"original-uid"},{"op":"test","path":"/spec/value","value":9007199254740993},{"op":"replace","path":"/spec/value","value":9007199254740993}]`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rr.Code, rr.Body)
	}
	after, err := store.Get(t.Context(), "default", "patch")
	if err != nil {
		t.Fatal(err)
	}
	if after.GetGeneration() != before.Generation || !bytes.Equal(mustMarshalJSON(t, before.Spec), mustMarshalJSON(t, after.(*object).Spec)) {
		t.Fatal("no-op changed spec/generation")
	}
}

func TestConvertingJSONPatchCopyAmplificationLimit(t *testing.T) {
	original := mustMarshalJSON(t, map[string]interface{}{"blob": strings.Repeat("x", 1<<20), "items": []interface{}{}})
	operation := `{"op":"copy","from":"/blob","path":"/items/-"}`
	patch := "[" + strings.TrimSuffix(strings.Repeat(operation+",", 4), ",") + "]"
	_, _, code, err := applyPublicPatch(constants.ContentTypeJSONPatch, original, []byte(patch))
	if code != http.StatusBadRequest || err == nil || err.Error() != "JSON patch operation failed" {
		t.Fatalf("copy amplification was not bounded: code=%d err=%v", code, err)
	}
}

func TestConvertingPatchRequestLimits(t *testing.T) {
	h, store, _ := publicPatchFixture(t)
	for _, tc := range []struct {
		name, contentType, body, message string
	}{
		{"JSONPatch body", constants.ContentTypeJSONPatch, strings.Repeat("x", maxPublicPatchBodyBytes+1), "patch body exceeds size limit"},
		{"merge patch body", "application/merge-patch+json", strings.Repeat("x", maxPublicPatchBodyBytes+1), "patch body exceeds size limit"},
		{"operation count", constants.ContentTypeJSONPatch, "[" + strings.TrimSuffix(strings.Repeat(`{"op":"test","path":"/metadata/uid","value":"original-uid"},`, maxPublicPatchOperations+1), ",") + "]", "too many JSON patch operations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := publicPatchRequest(t, h, t.Context(), tc.contentType, tc.body)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), tc.message) || store.updates != 0 {
				t.Fatalf("limit failed: status=%d body=%s updates=%d", rr.Code, rr.Body.String(), store.updates)
			}
		})
	}
}

func TestConvertingPatchPreservesCompletePrivateStatus(t *testing.T) {
	h, _, _ := publicPatchFixture(t)
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(objectGVK, &unstructured.Unstructured{})
	h.privateScheme = scheme
	previous := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": objectGVK.GroupVersion().String(), "kind": objectGVK.Kind,
		"status": map[string]interface{}{"items": []interface{}{map[string]interface{}{"public": "visible", "private": "retained", "integer": int64(9007199254740993)}}},
	}}
	for _, hasStatus := range []bool{false, true} {
		old := previous.DeepCopy()
		if !hasStatus {
			delete(old.Object, "status")
		}
		next := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": objectGVK.GroupVersion().String(), "kind": objectGVK.Kind,
			"spec":   map[string]interface{}{"field": "updated"},
			"status": map[string]interface{}{"items": []interface{}{map[string]interface{}{"public": "visible"}}, "forged": "must-not-survive"},
		}}
		result, err := h.preservePublicPatchStatus(next, old)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.(*unstructured.Unstructured).Object["status"], old.Object["status"]) || result.(*unstructured.Unstructured).Object["spec"].(map[string]interface{})["field"] != "updated" {
			t.Fatal("complete private status was not restored")
		}
	}
}

type publicPatchRevisionStore struct {
	storage.ResourceStore
	revision string
}

func (s publicPatchRevisionStore) Get(ctx context.Context, namespace, name string) (client.Object, error) {
	obj, err := s.ResourceStore.Get(ctx, namespace, name)
	if err == nil {
		obj.SetResourceVersion(s.revision)
	}
	return obj, err
}

func TestConvertingJSONPatchRequiresStoredRevision(t *testing.T) {
	for _, revision := range []string{"", "0"} {
		t.Run(revision, func(t *testing.T) {
			h, store, _ := publicPatchFixture(t)
			h.store = publicPatchRevisionStore{ResourceStore: store, revision: revision}
			rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, `[]`)
			if rr.Code != http.StatusInternalServerError || store.updates != 0 || !strings.Contains(rr.Body.String(), "stored resource revision unavailable") {
				t.Fatalf("invalid stored RV: %d %s", rr.Code, rr.Body)
			}
		})
	}
}

func TestConvertingJSONPatchPrivateMetadata(t *testing.T) {
	h, store, before := publicPatchFixture(t)
	patch := `[{"op":"add","path":"/metadata/labels/private.test~1owner","value":"forged"},{"op":"add","path":"/metadata/annotations","value":{"private.test/secret":"forged","private.test/new":"forged"}},{"op":"copy","from":"/metadata/uid","path":"/metadata/labels/copied"}]`
	rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, patch)
	if rr.Code != http.StatusOK {
		t.Fatalf("private metadata: %d %s", rr.Code, rr.Body)
	}
	after, err := store.Get(t.Context(), "default", "patch")
	if err != nil {
		t.Fatal(err)
	}
	if after.GetUID() != before.UID || after.GetLabels()["private.test/owner"] != "controller" || after.GetLabels()["copied"] != string(before.UID) || !reflect.DeepEqual(before.Annotations, after.GetAnnotations()) {
		t.Fatal("private metadata was overwritten or identity changed")
	}
}

func TestConvertingJSONPatchCombinedPreconditions(t *testing.T) {
	for _, wrong := range []string{"uid", "rv"} {
		t.Run(wrong, func(t *testing.T) {
			h, store, before := publicPatchFixture(t)
			uid, rv := string(before.UID), before.ResourceVersion
			if wrong == "uid" {
				uid = "wrong"
			} else {
				rv = "0"
			}
			patch := mustMarshalJSON(t, []map[string]interface{}{
				{"op": "test", "path": "/metadata/uid", "value": uid},
				{"op": "test", "path": "/metadata/resourceVersion", "value": rv},
				{"op": "replace", "path": "/spec/field", "value": "must-not-write"},
			})
			rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, string(patch))
			if rr.Code != http.StatusConflict || store.updates != 0 || !strings.Contains(rr.Body.String(), "JSON patch test failed") {
				t.Fatalf("combined preconditions: %d %s", rr.Code, rr.Body)
			}
		})
	}
}

func TestConvertingJSONPatchAuthorizerError(t *testing.T) {
	h, store, _ := publicPatchFixture(t)
	ctx := WithObjectAuthorizer(t.Context(), func(context.Context, client.Object) (bool, error) {
		return true, errors.New("secret authorizer failure")
	})
	rr := publicPatchRequest(t, h, ctx, constants.ContentTypeJSONPatch, `[{"op":"test","path":"/metadata/uid","value":"wrong"}]`)
	if rr.Code != http.StatusForbidden || store.updates != 0 || strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("authorizer error: %d %s", rr.Code, rr.Body)
	}
}

func TestConvertingJSONPatchEmptyArray(t *testing.T) {
	h, store, before := publicPatchFixture(t)
	rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, `[]`)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty patch: %d %s", rr.Code, rr.Body)
	}
	after, err := store.Get(t.Context(), "default", "patch")
	if err != nil {
		t.Fatal(err)
	}
	if after.GetGeneration() != before.Generation || after.(*object).Spec != before.Spec {
		t.Fatal("empty patch changed spec/generation")
	}
}
