package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

var testSchemaYAML = "type: object\nproperties:\n  spec:\n    type: object"

func makeDiscoveryHandler(verbs []string) *DiscoveryHandler {
	advertiseStatus := false
	return NewDiscoveryHandler(&mockResourceProvider{
		resources: []types.ResourceInfo{
			{
				GVK: runtimeschema.GroupVersionKind{
					Group:   "test.group",
					Version: "v1",
					Kind:    "Widget",
				},
				Plural:     "widgets",
				Singular:   "widget",
				Namespaced: true,
				SchemaYAML: testSchemaYAML,
				Verbs:      verbs,
			},
		},
	}, &DiscoveryOptions{AdvertiseStatus: &advertiseStatus})
}

// --- APIResourceList verb filtering ---

func TestAPIResourceList_NoVerbAnnotation_AllVerbsAdvertised(t *testing.T) {
	h := makeDiscoveryHandler(nil) // nil → all verbs
	req := httptest.NewRequest(http.MethodGet, "/apis/test.group/v1", nil)
	w := httptest.NewRecorder()
	h.APIResourceList(w, req, "test.group", "v1")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var list metav1.APIResourceList
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.APIResources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(list.APIResources))
	}
	verbs := list.APIResources[0].Verbs
	// Default set must include all standard verbs
	expected := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
	for _, v := range expected {
		found := false
		for _, av := range verbs {
			if av == v {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected verb %q in advertised verbs %v", v, verbs)
		}
	}
}

func TestAPIResourceList_RestrictedVerbs_OnlyDeclaredAdvertised(t *testing.T) {
	h := makeDiscoveryHandler([]string{"list", "get"})
	req := httptest.NewRequest(http.MethodGet, "/apis/test.group/v1", nil)
	w := httptest.NewRecorder()
	h.APIResourceList(w, req, "test.group", "v1")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var list metav1.APIResourceList
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.APIResources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(list.APIResources))
	}
	verbs := list.APIResources[0].Verbs
	if len(verbs) != 2 {
		t.Errorf("expected 2 advertised verbs, got %d: %v", len(verbs), verbs)
	}
	for _, v := range verbs {
		if v != "list" && v != "get" {
			t.Errorf("unexpected verb %q in advertised verbs", v)
		}
	}
}

// --- filterPathEntry ---

func TestFilterPathEntry_NilVerbs_AllKept(t *testing.T) {
	res := types.ResourceInfo{} // Verbs nil → all allowed
	entry := map[string]interface{}{
		"parameters": []interface{}{},
		"get":        "get-op",
		"post":       "post-op",
		"put":        "put-op",
		"delete":     "delete-op",
	}
	result := filterPathEntry(entry, res, "list")
	for _, k := range []string{"parameters", "get", "post", "put", "delete"} {
		if _, ok := result[k]; !ok {
			t.Errorf("key %q missing from result", k)
		}
	}
}

func TestFilterPathEntry_RestrictedVerbs_DisallowedRemoved(t *testing.T) {
	res := types.ResourceInfo{Verbs: []string{"list", "get"}}
	// Simulate a collection path entry
	entry := map[string]interface{}{
		"parameters": []interface{}{},
		"get":        "list-op", // "get" on collection = "list"
		"post":       "create-op",
	}
	result := filterPathEntry(entry, res, "list")
	if _, ok := result["parameters"]; !ok {
		t.Error("parameters should always be kept")
	}
	if _, ok := result["get"]; !ok {
		t.Error("get (list) should be kept")
	}
	if _, ok := result["post"]; ok {
		t.Error("post (create) should be removed")
	}
}

func TestFilterPathEntry_ItemPath_GetKept_PutRemoved(t *testing.T) {
	res := types.ResourceInfo{Verbs: []string{"get", "list"}}
	entry := map[string]interface{}{
		"parameters": []interface{}{},
		"get":        "get-op",
		"put":        "update-op",
		"delete":     "delete-op",
	}
	result := filterPathEntry(entry, res, "get")
	if _, ok := result["get"]; !ok {
		t.Error("get should be kept")
	}
	if _, ok := result["put"]; ok {
		t.Error("put (update) should be removed")
	}
	if _, ok := result["delete"]; ok {
		t.Error("delete should be removed")
	}
}
