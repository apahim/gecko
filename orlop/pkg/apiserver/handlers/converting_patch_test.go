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

func TestConvertingJSONPatchNoOpGeneration(t *testing.T) {
	h, store, before := publicPatchFixture(t)
	rr := publicPatchRequest(t, h, t.Context(), constants.ContentTypeJSONPatch, `[{"op":"test","path":"/metadata/uid","value":"original-uid"},{"op":"replace","path":"/spec/value","value":9007199254740993}]`)
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
