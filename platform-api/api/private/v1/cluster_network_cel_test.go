package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/aggregated"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/conversion"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/handlers"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
	publicv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

// TestClusterNetworkHostPrefixSurvivesUpdates reproduces the CEL type error
// "invalid data, expected int, got float64" on the clusterNetwork immutability
// rule (self == oldSelf). Numbers decoded from JSON into map[string]interface{}
// arrive as float64, but the structural schema declares hostPrefix as integer;
// the CEL validator refuses to convert float64 to a CEL int when evaluating the
// transition rule, so every update of a cluster created with hostPrefix set was
// rejected. The cluster then could never receive status updates.
func TestClusterNetworkHostPrefixSurvivesUpdates(t *testing.T) {
	privateScheme, publicScheme := runtime.NewScheme(), runtime.NewScheme()
	if err := privatev1.AddToScheme(privateScheme); err != nil {
		t.Fatal(err)
	}
	if err := publicv1.AddToScheme(publicScheme); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("Cluster")
	store := memory.NewMemoryStore("clusters", privateScheme, gvk)
	converter := conversion.NewConverter(publicScheme, privateScheme, "")
	handler := handlers.NewConvertingResourceHandler(store, maintenanceClusterSchemaProcessor(t, publicv1.ClusterSchemaYAML),
		converter, gvk, "clusters", publicScheme, privateScheme, nil, logr.Discard())
	base := "/apis/gcp.managed.openshift.io/v1/namespaces/{namespace}/clusters"
	router := chi.NewRouter()
	router.Post(base, handler.Create)
	router.Put(base+"/{name}", handler.Update)
	path := "/apis/gcp.managed.openshift.io/v1/namespaces/customer/clusters"
	request := func(method, url string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), method, url, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}

	cluster := clusterForMaintenanceAPITest()
	cluster.Spec.Networking = publicv1.NetworkingSpec{
		ClusterNetwork: []publicv1.ClusterNetworkEntry{{CIDR: "10.132.0.0/14", HostPrefix: 23}},
	}

	// Create via the public API with hostPrefix set.
	response := request(http.MethodPost, path, &cluster)
	if response.Code != http.StatusCreated {
		t.Fatalf("create with hostPrefix returned %d: %s", response.Code, response.Body.String())
	}
	created := publicv1.Cluster{}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Spec.Networking.ClusterNetwork) != 1 || created.Spec.Networking.ClusterNetwork[0].HostPrefix != 23 {
		t.Fatalf("created cluster lost hostPrefix: %+v", created.Spec.Networking.ClusterNetwork)
	}

	// An unchanged PUT must not be rejected by the immutability rule.
	response = request(http.MethodPut, path+"/test-cluster", &created)
	if response.Code != http.StatusOK {
		t.Fatalf("unchanged PUT with hostPrefix returned %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "expected int, got float64") {
		t.Fatalf("unchanged PUT hit the float64 CEL type error: %s", response.Body.String())
	}
	updated := publicv1.Cluster{}
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}

	// Mutating clusterNetwork must still be rejected: the immutability rule
	// stays in force after the type fix.
	mutated := updated.DeepCopy()
	mutated.Spec.Networking.ClusterNetwork[0].HostPrefix = 24
	response = request(http.MethodPut, path+"/test-cluster", mutated)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "clusterNetwork is immutable") {
		t.Fatalf("mutating clusterNetwork returned %d: %s", response.Code, response.Body.String())
	}
}

// TestClusterNetworkHostPrefixAggregatedStatusUpdate covers the private API
// path used by controllers: a status update through the aggregated API storage
// must not fail CEL validation of the immutable clusterNetwork when hostPrefix
// is set.
func TestClusterNetworkHostPrefixAggregatedStatusUpdate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	publicCluster := clusterForMaintenanceAPITest()
	publicCluster.Spec.Networking = publicv1.NetworkingSpec{
		ClusterNetwork: []publicv1.ClusterNetworkEntry{{CIDR: "10.132.0.0/14", HostPrefix: 23}},
	}
	converted, err := newClusterConverter(t).PublicToPrivate(&publicCluster, nil)
	if err != nil {
		t.Fatal(err)
	}
	cluster := converted.(*privatev1.Cluster)
	ctx := genericapirequest.WithNamespace(t.Context(), cluster.Namespace)
	if err := cluster.Default(ctx); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("Cluster")
	store := memory.NewMemoryStore("clusters", scheme, gvk)
	if err := store.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	strategy := aggregated.NewResourceStrategy(scheme, maintenanceClusterSchemaProcessor(t, privatev1.ClusterSchemaYAML), true, gvk, logr.Discard())
	storage := aggregated.NewResourceStorage(store, strategy, gvk, "clusters", "cluster", scheme, logr.Discard(), nil)
	statusStorage := aggregated.NewStatusStorage(storage)
	updater := rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _, old runtime.Object) (runtime.Object, error) {
		updated := old.(*privatev1.Cluster).DeepCopy()
		updated.Status.Conditions = []metav1.Condition{{
			Type: "HostedClusterAvailable", Status: metav1.ConditionTrue, Reason: "AsExpected", LastTransitionTime: metav1.Now(),
		}}
		return updated, nil
	})
	if _, _, err := statusStorage.Update(context.WithoutCancel(ctx), cluster.Name, updater, nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("status update with hostPrefix failed: %v", err)
	}
	persisted, err := store.Get(ctx, cluster.Namespace, cluster.Name)
	if err != nil {
		t.Fatal(err)
	}
	updated := persisted.(*privatev1.Cluster)
	if len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatalf("status conditions were not persisted: %v", updated.Status.Conditions)
	}
	if len(updated.Spec.Networking.ClusterNetwork) != 1 || updated.Spec.Networking.ClusterNetwork[0].HostPrefix != 23 {
		t.Fatalf("status update changed clusterNetwork: %+v", updated.Spec.Networking.ClusterNetwork)
	}
}
