package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRoleBindingConditionsHTTP(t *testing.T) {
	server := newPublicAuthorizationTestServer(t)
	for _, surface := range []struct {
		name    string
		request publicAuthorizationRequester
		private bool
	}{
		{"public", server.request, false}, {"private", server.privateRequest, true},
	} {
		t.Run(surface.name, func(t *testing.T) {
			name := "allowed-conditions-" + surface.name
			collection := server.apiPrefix + "/namespaces/project-a/rolebindings"
			path := collection + "/" + name
			conditions := []privatev1.RoleBindingRequirement{{Key: "region", Operator: "In", Values: []string{"TYPO", "europe-*", " literal value "}}, {Key: "name", Operator: "NotIn", Values: []string{"never-match"}}}
			body := func(revision string, requirements interface{}, include bool) []byte {
				t.Helper()
				spec := map[string]interface{}{"subject": "bob@example.com", "roleRef": map[string]interface{}{"kind": "PlatformRole", "name": "cluster-viewer", "apiGroup": privatev1.GroupVersion.Group}}
				if include {
					spec["conditions"] = requirements
				}
				metadata := map[string]interface{}{"name": name, "namespace": "project-a"}
				if revision != "" {
					metadata["resourceVersion"] = revision
				}
				data, err := json.Marshal(map[string]interface{}{"apiVersion": privatev1.GroupVersion.String(), "kind": "RoleBinding", "metadata": metadata, "spec": spec})
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			send := func(method, url string, data []byte, want int) string {
				t.Helper()
				contentType := "application/json"
				if method == http.MethodPatch {
					contentType = "application/merge-patch+json"
				}
				status, response := surface.request(method, url, data, contentType)
				if status != want {
					t.Fatalf("%s %s: status %d want %d: %s", method, url, status, want, response)
				}
				return response
			}
			stored := func() *privatev1.RoleBinding {
				t.Helper()
				object, err := server.bindingStore.Get(t.Context(), "project-a", name)
				if err != nil {
					t.Fatal(err)
				}
				binding := object.(*privatev1.RoleBinding)
				return binding
			}
			assertRequirements := func(want []privatev1.RoleBindingRequirement) {
				t.Helper()
				binding := stored()
				if len(want) == 0 {
					if len(binding.Spec.Conditions) != 0 {
						t.Fatal("clearing did not persist empty requirements")
					}
					return
				}
				if !reflect.DeepEqual(binding.Spec.Conditions, want) {
					t.Fatalf("got %#v want %#v", binding.Spec.Conditions, want)
				}
			}
			response := send(http.MethodPost, collection, body("", conditions, true), http.StatusCreated)
			assertRequirements(conditions)
			var object map[string]interface{}
			if err := json.Unmarshal([]byte(response), &object); err != nil {
				t.Fatal(err)
			}
			_, rawVisible := object["spec"].(map[string]interface{})["condition"]
			if rawVisible {
				t.Fatalf("raw field visibility %v", rawVisible)
			}
			send(http.MethodGet, path, nil, http.StatusOK)
			list := send(http.MethodGet, collection, nil, http.StatusOK)
			if !strings.Contains(list, name) {
				t.Fatal("created binding absent from list")
			}
			// Omission preserves a complete merged candidate; arrays are replacements.
			send(http.MethodPatch, path, []byte(`{"spec":{"subject":"bob@example.com"}}`), http.StatusOK)
			assertRequirements(conditions)
			replacement := []privatev1.RoleBindingRequirement{{Key: "name", Operator: "In", Values: []string{"demo-*", "demo-*", " literal value "}}}
			patch, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"conditions": replacement}})
			send(http.MethodPatch, path, patch, http.StatusOK)
			assertRequirements(replacement)
			for _, clear := range []string{`[]`, `null`} {
				send(http.MethodPut, path, body(stored().ResourceVersion, conditions, true), http.StatusOK)
				send(http.MethodPatch, path, []byte(`{"spec":{"conditions":`+clear+`}}`), http.StatusOK)
				assertRequirements(nil)
			}
			send(http.MethodPut, path, body(stored().ResourceVersion, conditions, true), http.StatusOK)
			send(http.MethodPut, path, body(stored().ResourceVersion, nil, false), http.StatusOK)
			assertRequirements(nil)
			send(http.MethodPut, path, body(stored().ResourceVersion, conditions, true), http.StatusOK)
			testRoleBindingRawInput(t, surface.name, path, collection, conditions, replacement, body, stored, send, assertRequirements)
			testRoleBindingSchemaInput(t, surface.private, path, collection, body, stored, send)
			// Complete-object validation must reject required-field removal atomically.
			before := stored().DeepCopy()
			invalidCode := http.StatusBadRequest
			if surface.private {
				invalidCode = http.StatusUnprocessableEntity
			}
			send(http.MethodPatch, path, []byte(`{"spec":{"subject":null}}`), invalidCode)
			oversized, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"subject": "changed@example.com", "conditions": []privatev1.RoleBindingRequirement{{Key: "name", Operator: "In", Values: []string{strings.Repeat("x", 4096)}}}}})
			send(http.MethodPatch, path, oversized, invalidCode)
			oversizedRequirements := []privatev1.RoleBindingRequirement{{Key: "name", Operator: "In", Values: []string{strings.Repeat("x", 4096)}}}
			send(http.MethodPut, path, body(stored().ResourceVersion, oversizedRequirements, true), invalidCode)
			var oversizedCreate map[string]interface{}
			if err := json.Unmarshal(body("", oversizedRequirements, true), &oversizedCreate); err != nil {
				t.Fatal(err)
			}
			oversizedCreate["metadata"].(map[string]interface{})["name"] = "allowed-oversized-" + surface.name
			oversizedBody, err := json.Marshal(oversizedCreate)
			if err != nil {
				t.Fatal(err)
			}
			send(http.MethodPost, collection, oversizedBody, invalidCode)
			send(http.MethodGet, collection+"/allowed-oversized-"+surface.name, nil, http.StatusNotFound)
			if !reflect.DeepEqual(before, stored()) {
				t.Fatal("failed validation partially changed binding")
			}
			if surface.private {
				// Structured-only values can round-trip on private GET/PUT.
				current := send(http.MethodGet, path, nil, http.StatusOK)
				send(http.MethodPut, path, []byte(current), http.StatusOK)
			}
			send(http.MethodDelete, path, nil, http.StatusOK)
			send(http.MethodGet, path, nil, http.StatusNotFound)
		})
	}
}

func TestNameConditionGenerateName(t *testing.T) {
	server := newPublicAuthorizationTestServer(t)
	collection := server.apiPrefix + "/namespaces/project-a/rolebindings"
	spec := `"spec":{"subject":"bob@example.com","roleRef":{"kind":"PlatformRole","name":"cluster-viewer","apiGroup":"gcp.managed.openshift.io"}}`
	for _, tt := range []struct {
		metadata string
		status   int
	}{
		{`{"generateName":"allowed-"}`, http.StatusForbidden},
		{`{"name":"allowed-explicit","generateName":"ignored-"}`, http.StatusCreated},
	} {
		code, response := server.request(http.MethodPost, collection, []byte(`{"metadata":`+tt.metadata+`,`+spec+`}`), "application/json")
		if code != tt.status {
			t.Fatalf("metadata %s: %d %s", tt.metadata, code, response)
		}
	}
}

// testRoleBindingRawInput verifies that obsolete raw Cedar input is ignored on
// either API; only structured requirements are admitted and returned.
func testRoleBindingRawInput(t *testing.T, surfaceName, path, collection string,
	conditions, replacement []privatev1.RoleBindingRequirement,
	body func(string, interface{}, bool) []byte, stored func() *privatev1.RoleBinding,
	send func(string, string, []byte, int) string, assertRequirements func([]privatev1.RoleBindingRequirement)) {
	t.Helper()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		for i, raw := range []string{`null`, `""`, `"false"`, `"invalid Cedar"`, `{"anything":true}`} {
			target := path
			candidate := map[string]interface{}{}
			if err := json.Unmarshal(body(stored().ResourceVersion, conditions, true), &candidate); err != nil {
				t.Fatal(err)
			}
			code := http.StatusOK
			if method == http.MethodPost {
				target = collection
				metadata := candidate["metadata"].(map[string]interface{})
				metadata["name"] = fmt.Sprintf("allowed-raw-%s-%d", surfaceName, i)
				delete(metadata, "resourceVersion")
				code = http.StatusCreated
			}
			candidate["spec"].(map[string]interface{})["condition"] = json.RawMessage(raw)
			payload, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			response := send(method, target, payload, code)
			if strings.Contains(response, `"condition":`) {
				t.Fatal("obsolete raw field exposed")
			}
			if method == http.MethodPost {
				send(http.MethodDelete, collection+"/"+candidate["metadata"].(map[string]interface{})["name"].(string), nil, http.StatusOK)
			} else {
				assertRequirements(conditions)
			}
		}
	}
	// Structured updates do not depend on any stored/generated Cedar cache.
	patch, err := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"conditions": replacement, "condition": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	send(http.MethodPatch, path, patch, http.StatusOK)
	assertRequirements(replacement)
	send(http.MethodPut, path, body(stored().ResourceVersion, conditions, true), http.StatusOK)
}

// testRoleBindingSchemaInput verifies the ordinary schema path, without a
// RoleBinding-specific HTTP interceptor.
func testRoleBindingSchemaInput(t *testing.T, private bool, path, collection string,
	body func(string, interface{}, bool) []byte, stored func() *privatev1.RoleBinding,
	send func(string, string, []byte, int) string) {
	t.Helper()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		for _, invalid := range []string{
			`[{"key":"name","operator":"In","values":[]}]`,
			`[{"key":"name","operator":"In","values":null}]`,
			`[{"key":"name","operator":"In"}]`,
			`[{"key":"name","operator":"NotIn","values":[]}]`,
			`[{"key":"name","operator":"NotIn","values":null}]`,
			`[{"key":"name","operator":"NotIn"}]`,
			`[{"key":"region","operator":"In","values":[]}]`,
			`[{"key":"region","operator":"In","values":null}]`,
			`[{"key":"region","operator":"In"}]`,
			`[{"key":"region","operator":"NotIn","values":[]}]`,
			`[{"key":"region","operator":"NotIn","values":null}]`,
			`[{"key":"region","operator":"NotIn"}]`,
			`[{"key":"name","operator":"Like","values":["x"]}]`,
			`[{"key":"other","operator":"In","values":["x"]}]`,
			`[{"key":"name","operator":"In","values":null}]`,
			`[{"key":"name","operator":"In"}]`,
			`[{"operator":"In","values":["x"]}]`,
			`[{"key":"name","values":["x"]}]`,
			`[{"key":"name","operator":"In","values":[1]}]`,
			`[{"key":"name","operator":"In","values":[null]}]`,
			`[{"key":"name","operator":"In","values":[""]}]`,
			`[{"key":"region","operator":"NotIn","values":[" "]}]`,
			`[{"key":"name","operator":"In","values":["x","\t\r\n"]}]`,
			`[{"key":"region","operator":"NotIn","values":["\u00a0\u2003"]}]`,
			`[{"key":"name","operator":"In","values":["x",null]}]`,
			`[{"key":"name","operator":"In","values":"x"}]`,
			`[null]`, `["x"]`, `{}`,
		} {
			candidate := map[string]interface{}{}
			if err := json.Unmarshal(body(stored().ResourceVersion, nil, false), &candidate); err != nil {
				t.Fatal(err)
			}
			target := path
			if method == http.MethodPost {
				target = collection
				metadata := candidate["metadata"].(map[string]interface{})
				metadata["name"] = "allowed-invalid-schema"
				delete(metadata, "resourceVersion")
			}
			candidate["spec"].(map[string]interface{})["conditions"] = json.RawMessage(invalid)
			payload, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			code := http.StatusBadRequest
			var typed []privatev1.RoleBindingRequirement
			decodeErr := json.Unmarshal([]byte(invalid), &typed)
			// GenericAPIServer reports malformed typed POST/PUT as 400;
			// merge-patch decode and validation failures are 422.
			if private && (method == http.MethodPatch || decodeErr == nil) {
				code = http.StatusUnprocessableEntity
			}
			before := stored().DeepCopy()
			send(method, target, payload, code)
			if !reflect.DeepEqual(before, stored()) {
				t.Fatal("invalid schema input changed stored object")
			}
		}
	}
	// Unknown properties follow normal schema pruning, rather than bespoke rejection.
	send(http.MethodPatch, path, []byte(`{"spec":{"conditions":[{"key":"name","operator":"In","values":["demo-*"],"extra":true}]}}`), http.StatusOK)
	send(http.MethodPut, path, body(stored().ResourceVersion, []privatev1.RoleBindingRequirement{{Key: "name", Operator: "In", Values: []string{"demo-*"}}}, true), http.StatusOK)
}

// documentRoleBindingStore emulates database backends returning unstructured
// documents, including obsolete fields that are absent from current API types.
//
//nolint:kubeapilinter // Test-only storage adapter, not a serialized API type.
type documentRoleBindingStore struct{ storage.ResourceStore }

func (s *documentRoleBindingStore) Create(ctx context.Context, obj client.Object) error {
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: document}
	if err := s.ResourceStore.Create(ctx, u); err != nil {
		return err
	}
	obj.SetResourceVersion(u.GetResourceVersion())
	obj.SetName(u.GetName())
	return nil
}
func (s *documentRoleBindingStore) Update(ctx context.Context, obj client.Object) error {
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: document}
	if err := s.ResourceStore.Update(ctx, u); err != nil {
		return err
	}
	obj.SetResourceVersion(u.GetResourceVersion())
	return nil
}

func TestLegacyRawConditionIgnoredHTTP(t *testing.T) {
	server := newPublicAuthorizationTestServerWithDecorator(t, func(gvk runtimeschema.GroupVersionKind, original storage.ResourceStore) storage.ResourceStore {
		if gvk.Kind != "RoleBinding" {
			return original
		}
		scheme := runtime.NewScheme()
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("RoleBindingList"), &unstructured.UnstructuredList{})
		return &documentRoleBindingStore{ResourceStore: memory.NewMemoryStore("rolebindings", scheme, gvk)}
	})
	for _, surface := range []struct {
		name    string
		request publicAuthorizationRequester
	}{
		{"public", server.request}, {"private", server.privateRequest},
	} {
		t.Run(surface.name, func(t *testing.T) {
			name := "allowed-legacy-" + surface.name
			legacy := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": privatev1.GroupVersion.String(), "kind": "RoleBinding",
				"metadata": map[string]interface{}{"name": name, "namespace": "project-a", "uid": "legacy-" + surface.name},
				"spec":     map[string]interface{}{"subject": "bob@example.com", "roleRef": map[string]interface{}{"kind": "PlatformRole", "name": "cluster-viewer", "apiGroup": privatev1.GroupVersion.Group}, "condition": "false"},
			}}
			if err := server.bindingStore.Create(t.Context(), legacy); err != nil {
				t.Fatal(err)
			}
			collection := server.apiPrefix + "/namespaces/project-a/rolebindings"
			path := collection + "/" + name
			for _, url := range []string{path, collection} {
				code, response := surface.request(http.MethodGet, url, nil, "")
				if code != http.StatusOK || strings.Contains(response, `"condition":`) {
					t.Fatalf("legacy read: %d %s", code, response)
				}
			}
			before, err := server.bindingStore.Get(t.Context(), "project-a", name)
			if err != nil {
				t.Fatal(err)
			}
			raw, found, err := unstructured.NestedString(before.(*unstructured.Unstructured).Object, "spec", "condition")
			if err != nil || !found || raw != "false" {
				t.Fatal("read modified stored legacy field")
			}
			if surface.name == "private" {
				query := "?watch=true&resourceVersionMatch=NotOlderThan&sendInitialEvents=true&allowWatchBookmarks=true&timeoutSeconds=1"
				code, response := surface.request(http.MethodGet, collection+query, nil, "")
				if code != http.StatusOK || !strings.Contains(response, `"ADDED"`) || strings.Contains(response, `"condition":`) {
					t.Fatalf("legacy watch: %d %s", code, response)
				}
			}
			code, response := surface.request(http.MethodPatch, path, []byte(`{"spec":{"conditions":[{"key":"name","operator":"In","values":["demo-*"]}]}}`), "application/merge-patch+json")
			if code != http.StatusOK || strings.Contains(response, `"condition":`) {
				t.Fatalf("legacy update: %d %s", code, response)
			}
			stored, err := server.bindingStore.Get(t.Context(), "project-a", name)
			if err != nil {
				t.Fatal(err)
			}
			conditions, found, err := unstructured.NestedSlice(stored.(*unstructured.Unstructured).Object, "spec", "conditions")
			if err != nil || !found || len(conditions) != 1 {
				t.Fatal("structured update not persisted")
			}
			if _, found, _ := unstructured.NestedFieldNoCopy(stored.(*unstructured.Unstructured).Object, "spec", "condition"); found {
				t.Fatal("ordinary update retained obsolete field")
			}
			// Deletion responses, including repeated finalizer deletions, must hide raw.
			stored.SetFinalizers([]string{"example.com/cleanup"})
			if err := unstructured.SetNestedField(stored.(*unstructured.Unstructured).Object, "false", "spec", "condition"); err != nil {
				t.Fatal(err)
			}
			if err := server.bindingStore.Update(t.Context(), stored); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				code, response := surface.request(http.MethodDelete, path, nil, "")
				if code != http.StatusOK || strings.Contains(response, `"condition":`) {
					t.Fatalf("legacy deletion: %d %s", code, response)
				}
			}
		})
	}
}
