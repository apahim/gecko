package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/aggregated"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/handlers"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
	publicv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

func integerNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func integerCluster(t *testing.T) *privatev1.Cluster {
	t.Helper()
	pub := clusterForMaintenanceAPITest()
	pub.Spec.Maintenance = nil
	pub.Spec.Networking.ClusterNetwork = []publicv1.ClusterNetworkEntry{{CIDR: "10.132.0.0/14", HostPrefix: 23}}
	obj, err := newClusterConverter(t).PublicToPrivate(&pub, nil)
	integerNoError(t, err)
	c := obj.(*privatev1.Cluster)
	c.UID = "integer-cluster-uid"
	integerNoError(t, c.Default(t.Context()))
	return c
}

func TestClusterIntegerAggregatedUpdates(t *testing.T) {
	for _, statusOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("status=%t", statusOnly), func(t *testing.T) {
			scheme := runtime.NewScheme()
			integerNoError(t, privatev1.AddToScheme(scheme))
			gvk := privatev1.GroupVersion.WithKind("Cluster")
			c := integerCluster(t)
			ctx := genericapirequest.WithNamespace(t.Context(), c.Namespace)
			store := memory.NewMemoryStore("clusters", scheme, gvk)
			integerNoError(t, store.Create(ctx, c))
			strategy := aggregated.NewResourceStrategy(scheme, maintenanceClusterSchemaProcessor(t, privatev1.ClusterSchemaYAML), true, gvk, logr.Discard())
			storage := aggregated.NewResourceStorage(store, strategy, gvk, "clusters", "cluster", scheme, logr.Discard(), nil)
			update := rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, old runtime.Object) (runtime.Object, error) {
				next := old.(*privatev1.Cluster).DeepCopy()
				if statusOnly {
					next.Status.Conditions = []metav1.Condition{{Type: "IAMPermissionsReady", Status: metav1.ConditionFalse, Reason: "WIFConfigUpdateRequired", LastTransitionTime: metav1.Now()}}
				} else {
					next.Finalizers = []string{"example.com/test"}
				}
				return next, nil
			})
			if statusOnly {
				_, _, err := aggregated.NewStatusStorage(storage).Update(ctx, c.Name, update, nil, nil, false, &metav1.UpdateOptions{})
				integerNoError(t, err)
			} else {
				_, _, err := storage.Update(ctx, c.Name, update, nil, nil, false, &metav1.UpdateOptions{})
				integerNoError(t, err)
			}
			stored, err := store.Get(ctx, c.Namespace, c.Name)
			integerNoError(t, err)
			got := stored.(*privatev1.Cluster)
			if got.Spec.Networking.ClusterNetwork[0].HostPrefix != 23 || (statusOnly && len(got.Status.Conditions) != 1) || (!statusOnly && len(got.Finalizers) != 1) {
				t.Fatal("unchanged networking or requested update was not persisted")
			}
			changed := got.DeepCopy()
			changed.Spec.Networking.ClusterNetwork[0].HostPrefix = 24
			errs := strategy.ValidateUpdate(ctx, changed, got)
			if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "clusterNetwork is immutable") || strings.Contains(errs.ToAggregate().Error(), "expected int") {
				t.Fatalf("wrong immutable-change result: %v", errs)
			}
		})
	}
}

func integerRequest(t *testing.T, router http.Handler, method, path string, body any, want int, contentTypes ...string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	integerNoError(t, err)
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/merge-patch+json")
	}
	if len(contentTypes) > 0 && contentTypes[0] != "" {
		req.Header.Set("Content-Type", contentTypes[0])
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != want {
		t.Fatalf("%s: want HTTP %d, got %d: %s", method, want, rr.Code, rr.Body.String())
	}
	return rr
}

func TestClusterIntegerHandlerUpdates(t *testing.T) {
	for _, public := range []bool{false, true} {
		for _, transport := range []struct{ method, contentType string }{{http.MethodPut, "application/json"}, {http.MethodPatch, "application/merge-patch+json"}, {http.MethodPatch, "application/json-patch+json"}} {
			method := transport.method
			t.Run(fmt.Sprintf("public=%t/%s/%s", public, method, transport.contentType), func(t *testing.T) {
				privScheme, pubScheme := runtime.NewScheme(), runtime.NewScheme()
				integerNoError(t, privatev1.AddToScheme(privScheme))
				integerNoError(t, publicv1.AddToScheme(pubScheme))
				gvk := privatev1.GroupVersion.WithKind("Cluster")
				store := memory.NewMemoryStore("clusters", privScheme, gvk)
				c := integerCluster(t)
				router := chi.NewRouter()
				base := "/apis/gcp.managed.openshift.io/v1/namespaces/{namespace}/clusters"
				if public {
					h := handlers.NewConvertingResourceHandler(store, maintenanceClusterSchemaProcessor(t, publicv1.ClusterSchemaYAML), newClusterConverter(t), gvk, "clusters", pubScheme, privScheme, nil, logr.Discard())
					router.Post(base, h.Create)
					router.Put(base+"/{name}", h.Update)
					router.Patch(base+"/{name}", h.Patch)
				} else {
					h := handlers.NewResourceHandler(store, maintenanceClusterSchemaProcessor(t, privatev1.ClusterSchemaYAML), gvk, "clusters", privScheme, logr.Discard())
					router.Post(base, h.Create)
					router.Put(base+"/{name}", h.Update)
					router.Patch(base+"/{name}", h.Patch)
					router.Put(base+"/{name}/status", h.UpdateStatus)
				}
				path := "/apis/gcp.managed.openshift.io/v1/namespaces/" + c.Namespace + "/clusters"
				var createBody any = c
				if public {
					var err error
					createBody, err = newClusterConverter(t).PrivateToPublic(c)
					integerNoError(t, err)
				}
				rr := integerRequest(t, router, http.MethodPost, path, createBody, http.StatusCreated)
				var current map[string]any
				integerNoError(t, json.Unmarshal(rr.Body.Bytes(), &current))
				current["metadata"].(map[string]any)["labels"] = map[string]any{"numeric-regression": "unchanged"}
				var update any = current
				if method == http.MethodPatch {
					update = map[string]any{"metadata": map[string]any{"labels": map[string]any{"numeric-regression": "unchanged"}}}
				}
				if transport.contentType == "application/json-patch+json" {
					update = []map[string]any{{"op": "add", "path": "/metadata/labels", "value": map[string]any{"numeric-regression": "unchanged"}}}
				}
				// Public JSONPatch now shares these integer/CEL regressions with
				// merge-patch and private JSONPatch rather than stopping at parsing.
				integerRequest(t, router, method, path+"/"+c.Name, update, http.StatusOK, transport.contentType)
				if !public {
					integerRequest(t, router, http.MethodPut, path+"/"+c.Name+"/status", map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "IAMPermissionsReady", "status": "False", "reason": "WIFConfigUpdateRequired", "lastTransitionTime": metav1.Now()}}}}, http.StatusOK)
				}
				stored, err := store.Get(t.Context(), c.Namespace, c.Name)
				integerNoError(t, err)
				got := stored.(*privatev1.Cluster)
				if got.Spec.Networking.ClusterNetwork[0].HostPrefix != 23 || got.Labels["numeric-regression"] != "unchanged" {
					t.Fatal("update changed integer networking or lost labels")
				}
				got.Spec.Networking.ClusterNetwork[0].HostPrefix = 24
				update = got
				if public {
					update, err = newClusterConverter(t).PrivateToPublic(got)
					integerNoError(t, err)
				}
				if method == http.MethodPatch {
					update = map[string]any{"spec": map[string]any{"networking": map[string]any{"clusterNetwork": []any{map[string]any{"cidr": "10.132.0.0/14", "hostPrefix": 24}}}}}
				}
				if transport.contentType == "application/json-patch+json" {
					update = []map[string]any{{"op": "replace", "path": "/spec/networking/clusterNetwork/0/hostPrefix", "value": 24}}
				}
				rr = integerRequest(t, router, method, path+"/"+c.Name, update, http.StatusBadRequest, transport.contentType)
				if !strings.Contains(rr.Body.String(), "clusterNetwork is immutable") || strings.Contains(rr.Body.String(), "expected int") {
					t.Fatalf("wrong immutable-change rejection: %s", rr.Body.String())
				}
				persisted, err := store.Get(t.Context(), c.Namespace, c.Name)
				integerNoError(t, err)
				if persisted.(*privatev1.Cluster).Spec.Networking.ClusterNetwork[0].HostPrefix != 23 {
					t.Fatal("failed update mutated stored networking")
				}
			})
		}
	}
}
