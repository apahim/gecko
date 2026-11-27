package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

//nolint:kubeapilinter // Test-only storage decorator, not a serialized API type.
type failingBindingStore struct {
	storage.ResourceStore
	fail atomic.Bool
}

func (s *failingBindingStore) Create(ctx context.Context, obj client.Object) error {
	if s.fail.Load() {
		return errors.New("injected storage failure")
	}
	return s.ResourceStore.Create(ctx, obj)
}
func (s *failingBindingStore) Update(ctx context.Context, obj client.Object) error {
	if s.fail.Load() {
		return errors.New("injected storage failure")
	}
	return s.ResourceStore.Update(ctx, obj)
}

func TestRoleBindingStorageFailureLeavesPairUnchanged(t *testing.T) {
	var store *failingBindingStore
	server := newPublicAuthorizationTestServerWithDecorator(t, func(gvk schema.GroupVersionKind, underlying storage.ResourceStore) storage.ResourceStore {
		if gvk.Kind != "RoleBinding" {
			return underlying
		}
		store = &failingBindingStore{ResourceStore: underlying}
		return store
	})
	binding := &privatev1.RoleBinding{}
	binding.Name = "allowed-store-failure"
	binding.UID = "storage-failure-binding"
	binding.Namespace = "project-a"
	binding.Spec = privatev1.RoleBindingSpec{Subject: "bob@example.com", RoleRef: privatev1.RoleRef{Kind: "PlatformRole", Name: "cluster-viewer", APIGroup: privatev1.GroupVersion.Group}, Conditions: []privatev1.RoleBindingRequirement{{Key: "region", Operator: "In", Values: []string{"us-central1"}}}}
	binding.SetGroupVersionKind(privatev1.GroupVersion.WithKind("RoleBinding"))
	if err := store.Create(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	before := binding.DeepCopy()
	store.fail.Store(true)
	for _, surface := range []struct {
		name    string
		request publicAuthorizationRequester
	}{{"public", server.request}, {"private", server.privateRequest}} {
		t.Run(surface.name, func(t *testing.T) {
			collection := server.apiPrefix + "/namespaces/project-a/rolebindings"
			path := collection + "/" + binding.Name
			code, response := surface.request(http.MethodPatch, path, []byte(`{"spec":{"subject":"changed@example.com","conditions":[]}}`), "application/merge-patch+json")
			if code != http.StatusInternalServerError {
				t.Fatalf("failed update: %d %s", code, response)
			}
			stored, err := store.Get(t.Context(), "project-a", binding.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, stored) {
				t.Fatal("storage failure partially changed structured requirements")
			}
			candidate := binding.DeepCopy()
			candidate.Name = "allowed-failed-create"
			candidate.ResourceVersion = ""
			candidate.UID = ""
			body, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			code, response = surface.request(http.MethodPost, collection, body, "application/json")
			if code != http.StatusInternalServerError {
				t.Fatalf("failed create: %d %s", code, response)
			}
			if _, err := store.Get(t.Context(), "project-a", candidate.Name); !apierrors.IsNotFound(err) {
				t.Fatalf("failed create persisted: %v", err)
			}
		})
	}
}
