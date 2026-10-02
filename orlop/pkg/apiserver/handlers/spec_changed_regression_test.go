package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-logr/logr"

	testv1 "github.com/openshift-online/gecko/orlop/apis/private/test/v1"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"
	pkgschema "github.com/openshift-online/gecko/orlop/pkg/apiserver/schema"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// unstructuredGetStore wraps a MemoryStore but returns objects from Get as
// *unstructured.Unstructured, reproducing what the postgres and spanner storage
// backends do in production (their Get decodes stored bytes into an unstructured
// map, not the typed object). MemoryStore alone returns the typed object it was
// handed, which never triggers the typed-vs-unstructured representation
// mismatch — so a churn test built on it would be vacuous.
type unstructuredGetStore struct {
	*memory.MemoryStore
}

func (s *unstructuredGetStore) Get(ctx context.Context, namespace, name string) (client.Object, error) {
	obj, err := s.MemoryStore.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// newTestObjectProcessor builds a real (non-nil) schema.Processor from
// testv1.Object's generated OpenAPI schema, mirroring how ResourceHandler is
// actually constructed in production. Existing helpers_test.go specChanged
// cases use hand-built maps with no processor involved; this exercises the
// real prune/default pipeline that PrepareForUpdate/Update actually invoke.
func newTestObjectProcessor(t *testing.T) *pkgschema.Processor {
	t.Helper()

	var propsV1 apiextv1.JSONSchemaProps
	if err := yaml.Unmarshal([]byte(testv1.ObjectSchemaYAML), &propsV1); err != nil {
		t.Fatalf("failed to unmarshal schema YAML: %v", err)
	}

	var props apiext.JSONSchemaProps
	if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&propsV1, &props, nil); err != nil {
		t.Fatalf("failed to convert schema: %v", err)
	}

	structural, err := extschema.NewStructural(&props)
	if err != nil {
		t.Fatalf("failed to create structural schema: %v", err)
	}

	processor, err := pkgschema.NewProcessor(structural, &props)
	if err != nil {
		t.Fatalf("failed to create processor: %v", err)
	}

	return processor
}

// TestSpecChanged_TypeMismatch reproduces the representation mismatch that
// the postgres and spanner storage backends hit in production: Get() returns
// an *unstructured.Unstructured (a map[string]interface{}, which
// encoding/json always marshals with alphabetically-sorted keys), while the
// incoming update is typically a typed struct (marshaled in Go
// struct-declaration order). ObjectSpec's declared order is publicField,
// internalField, nested, defaultField — not alphabetical — so a raw byte
// comparison of old vs. new would differ even for identical content.
// specChanged must compare semantically, not byte-for-byte.
func TestSpecChanged_TypeMismatch(t *testing.T) {
	oldObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": testv1.GroupVersion.String(),
			"kind":       "Object",
			"metadata": map[string]interface{}{
				"name":      "test-obj",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"publicField":   "value",
				"internalField": "internal",
				"nested": map[string]interface{}{
					"publicField":   "n-value",
					"internalField": "n-internal",
				},
				"defaultField": "default-value",
			},
		},
	}

	newObj := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-obj",
			Namespace: "default",
		},
		Spec: testv1.ObjectSpec{
			PublicField:   "value",
			InternalField: "internal",
			Nested: testv1.ObjectNested{
				PublicField:   "n-value",
				InternalField: "n-internal",
			},
			DefaultField: "default-value",
		},
	}

	if specChanged(oldObj, newObj) {
		t.Error("expected specChanged to return false for semantically identical spec content despite unstructured-vs-typed representation mismatch")
	}
}

// TestResourceHandlerUpdate_SymmetricProcessing_DefaultingDoesNotCauseChurn
// drives ResourceHandler.Update end-to-end with a real schema processor.
// existing and the incoming request body both omit defaultField, relying on
// the schema's default. Update's processor.Process call only ever processes
// the incoming request map; if existing isn't processed the same way before
// specChanged compares them, the newly-defaulted incoming object looks
// permanently different from existing, bumping generation on every reapply
// even though the caller-supplied content never changed.
func TestResourceHandlerUpdate_SymmetricProcessing_DefaultingDoesNotCauseChurn(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := testv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add test scheme: %v", err)
	}

	gvk := testv1.GroupVersion.WithKind("Object")
	store := memory.NewMemoryStore("objects", scheme, gvk)
	processor := newTestObjectProcessor(t)
	handler := NewResourceHandler(store, processor, gvk, "objects", scheme, logr.Discard())

	ctx := context.Background()
	existing := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-obj",
			Namespace:  "default",
			Generation: 7,
		},
		Spec: testv1.ObjectSpec{
			PublicField:   "value",
			InternalField: "internal",
			Nested: testv1.ObjectNested{
				PublicField:   "n-value",
				InternalField: "n-internal",
			},
			// DefaultField intentionally omitted, as if this object was
			// stored before the schema's default was applied.
		},
	}
	if err := store.Create(ctx, existing); err != nil {
		t.Fatalf("failed to seed existing object: %v", err)
	}

	// Reapply identical content, also omitting defaultField.
	body := map[string]interface{}{
		"apiVersion": testv1.GroupVersion.String(),
		"kind":       "Object",
		"metadata": map[string]interface{}{
			"name":            "test-obj",
			"namespace":       "default",
			"resourceVersion": existing.ResourceVersion,
		},
		"spec": map[string]interface{}{
			"publicField":   "value",
			"internalField": "internal",
			"nested": map[string]interface{}{
				"publicField":   "n-value",
				"internalField": "n-internal",
			},
		},
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/apis/test.orlop.gcp.managed.openshift.io/v1/namespaces/default/objects/test-obj", bytes.NewReader(bodyJSON))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(constants.URLParamNamespace, "default")
	rctx.URLParams.Add(constants.URLParamName, "test-obj")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	handler.Update(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Update status = %d, want %d, body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp testv1.Object
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Spec.DefaultField != "default-value" {
		t.Fatalf("expected DefaultField to be defaulted to %q, got %q", "default-value", resp.Spec.DefaultField)
	}
	if resp.Generation != 7 {
		t.Errorf("expected Generation to remain 7 (defaulting applied symmetrically to existing and incoming), got %d", resp.Generation)
	}
}

// TestConvertingHandlerUpdate_TypeMismatch_NoGenerationChurn drives
// ConvertingResourceHandler.Update end-to-end with a store whose Get returns
// *unstructured.Unstructured (as postgres/spanner do). The stored spec has
// multiple fields in non-alphabetical declaration order, so the unstructured
// old (alpha-sorted map keys) and the typed new marshal to different bytes even
// though their content is identical. Pre-fix, converting.go's specChanged
// byte-compared the two and reported a change on every reapply, bumping
// generation — the ArgoCD-OutOfSync symptom on the public/converting path.
// The shared semantic specChanged must keep generation stable.
func TestConvertingHandlerUpdate_TypeMismatch_NoGenerationChurn(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := testv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add test scheme: %v", err)
	}

	gvk := testv1.GroupVersion.WithKind("Object")
	store := &unstructuredGetStore{MemoryStore: memory.NewMemoryStore("objects", scheme, gvk)}
	processor := newTestObjectProcessor(t)
	converter := conversion.NewConverter(scheme, scheme, "")
	handler := NewConvertingResourceHandler(store, processor, converter, gvk, "objects", scheme, scheme, nil, logr.Discard())

	ctx := context.Background()
	existing := &testv1.Object{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-obj",
			Namespace:  "default",
			Generation: 5,
		},
		Spec: testv1.ObjectSpec{
			PublicField:   "value",
			InternalField: "internal",
			Nested: testv1.ObjectNested{
				PublicField:   "n-value",
				InternalField: "n-internal",
			},
			DefaultField: "default-value",
		},
	}
	if err := store.Create(ctx, existing); err != nil {
		t.Fatalf("failed to seed existing object: %v", err)
	}

	// Fetch the stored resourceVersion (MemoryStore.Update requires a match).
	stored, err := store.MemoryStore.Get(ctx, "default", "test-obj")
	if err != nil {
		t.Fatalf("failed to read back seeded object: %v", err)
	}

	// Reapply identical spec content through the public/converting path. Include
	// defaultField so schema defaulting does not itself introduce a difference —
	// this isolates the typed-vs-unstructured representation fix.
	body := map[string]interface{}{
		"apiVersion": testv1.GroupVersion.String(),
		"kind":       "Object",
		"metadata": map[string]interface{}{
			"name":            "test-obj",
			"namespace":       "default",
			"resourceVersion": stored.GetResourceVersion(),
		},
		"spec": map[string]interface{}{
			"publicField":   "value",
			"internalField": "internal",
			"nested": map[string]interface{}{
				"publicField":   "n-value",
				"internalField": "n-internal",
			},
			"defaultField": "default-value",
		},
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/apis/test.orlop.gcp.managed.openshift.io/v1/namespaces/default/objects/test-obj", bytes.NewReader(bodyJSON))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(constants.URLParamNamespace, "default")
	rctx.URLParams.Add(constants.URLParamName, "test-obj")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()

	handler.Update(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Update status = %d, want %d, body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp testv1.Object
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Generation != 5 {
		t.Errorf("expected Generation to remain 5 (identical spec, only representation differs), got %d", resp.Generation)
	}
}
