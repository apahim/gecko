package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"sigs.k8s.io/yaml"
)

func apiWIFTemplate(t *testing.T) *privatev1.WIFConfigTemplate {
	t.Helper()
	data, err := os.ReadFile("../../../../helm/charts/platform-api-server/files/wifconfigtemplate-project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w := &privatev1.WIFConfigTemplate{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "WIFConfigTemplate"}, ObjectMeta: metav1.ObjectMeta{Name: "5.0.0"}}
	if err := yaml.UnmarshalStrict(data, &w.Spec); err != nil {
		t.Fatal(err)
	}
	return w
}

func apiWIFRequest(t *testing.T, router http.Handler, method, path, contentType string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func apiWIFReadCluster(t *testing.T, store *memory.MemoryStore) *privatev1.Cluster {
	t.Helper()
	obj, err := store.Get(t.Context(), "customer", "test-cluster")
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*privatev1.Cluster)
}

func apiWIFAssertUnchanged(t *testing.T, before, after *privatev1.Cluster) {
	t.Helper()
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("rejected patch changed persisted state")
	}
}

func apiWIFAssertNoOp(t *testing.T, before, after *privatev1.Cluster) {
	t.Helper()
	beforeSpec, err := json.Marshal(before.Spec)
	if err != nil {
		t.Fatal(err)
	}
	afterSpec, err := json.Marshal(after.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeSpec, afterSpec) || after.Generation != before.Generation || after.ResourceVersion == before.ResourceVersion {
		t.Fatal("identical-history JSONPatch changed spec/generation or lacked persisted readback")
	}
	beforeStatus, err := json.Marshal(before.Status)
	if err != nil {
		t.Fatal(err)
	}
	afterStatus, err := json.Marshal(after.Status)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeStatus, afterStatus) {
		t.Fatal("public JSONPatch changed private controller status")
	}
}

func TestWIFTemplatePrivateMutationAPI(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("WIFConfigTemplate")
	store := memory.NewMemoryStore("wifconfigtemplates", scheme, gvk)
	h := handlers.NewResourceHandler(store, maintenanceClusterSchemaProcessor(t, privatev1.WIFConfigTemplateSchemaYAML), gvk, "wifconfigtemplates", scheme, logr.Discard())
	router := chi.NewRouter()
	base := "/apis/gcp.managed.openshift.io/v1/wifconfigtemplates"
	router.Post(base, h.Create)
	router.Get(base+"/{name}", h.Get)
	router.Put(base+"/{name}", h.Update)
	router.Patch(base+"/{name}", h.Patch)
	router.Delete(base+"/{name}", h.Delete)
	w := apiWIFTemplate(t)
	res := apiWIFRequest(t, router, http.MethodPost, base, "application/json", w)
	if res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	for _, verified := range []bool{false, true, false} {
		res = apiWIFRequest(t, router, http.MethodPatch, base+"/5.0.0", "application/merge-patch+json", map[string]any{"spec": map[string]any{"verified": verified}})
		if res.Code != http.StatusOK {
			t.Fatalf("verification patch: %d %s", res.Code, res.Body.String())
		}
	}
	obj, err := store.Get(t.Context(), "", "5.0.0")
	if err != nil {
		t.Fatal(err)
	}
	stored := obj.(*privatev1.WIFConfigTemplate)
	// A complete GitOps no-op PUT is allowed with the current resource version.
	res = apiWIFRequest(t, router, http.MethodPut, base+"/5.0.0", "application/json", stored)
	if res.Code != http.StatusOK {
		t.Fatalf("no-op PUT: %d %s", res.Code, res.Body.String())
	}
	for _, test := range []struct {
		name, method, content string
		body                  any
	}{
		{"edit merge", http.MethodPatch, "application/merge-patch+json", map[string]any{"spec": map[string]any{"schemaVersion": 2}}},
		{"edit JSON patch", http.MethodPatch, "application/json-patch+json", []map[string]any{{"op": "remove", "path": "/spec/iam/roles/0/permissions/0"}}},
		{"remove parent", http.MethodPatch, "application/merge-patch+json", map[string]any{"spec": nil}},
		{"delete", http.MethodDelete, "application/json", map[string]any{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := apiWIFRequest(t, router, test.method, base+"/5.0.0", test.content, test.body)
			if res.Code < 400 || res.Code >= 500 {
				t.Fatalf("mutation accepted or internal failure: %d %s", res.Code, res.Body.String())
			}
		})
	}
}

func TestClusterIAMPublicPatchAPI(t *testing.T) {
	scheme, publicScheme := runtime.NewScheme(), runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := publicv1.AddToScheme(publicScheme); err != nil {
		t.Fatal(err)
	}
	converter := conversion.NewConverter(publicScheme, scheme, "")
	w := apiWIFTemplate(t)
	w.UID = "template-uid"
	w.Spec.Verified = new(true)
	digest, err := privatev1.WIFConfigDigest(w.Spec)
	if err != nil {
		t.Fatal(err)
	}
	privatev1.SetValidatorDeps(privatev1.ValidatorDeps{AllowIAMApplicationWrites: true, WIFConfigTemplate: func(context.Context, string) (*privatev1.WIFConfigTemplate, error) { return w, nil }})
	t.Cleanup(func() { privatev1.SetValidatorDeps(privatev1.ValidatorDeps{}) })
	ref := privatev1.WIFConfigTemplateReference{Name: w.Name, UID: string(w.UID), Digest: digest}
	publicCluster := clusterForMaintenanceAPITest()
	publicCluster.Spec.Maintenance = nil
	publicCluster.Spec.InfraID = "iam-new-1"
	publicCluster.Spec.IssuerURL = "https://issuer.example/iam-new-1"
	gcp := publicCluster.Spec.Platform.GCP
	gcp.WorkloadIdentity.PoolID = "iam-new-1-wi-pool"
	gcp.WorkloadIdentity.ProviderID = "iam-new-1-k8s-provider"
	email := func(component string) string {
		return "iam-new-1-" + component + "@customer-project.iam.gserviceaccount.com"
	}
	gcp.WorkloadIdentity.ServiceAccountsRef = &publicv1.ServiceAccountsRef{NodePoolEmail: email("nodepool-mgmt"), ControlPlaneEmail: email("ctrlplane-op"), CloudControllerEmail: email("cloud-controller"), StorageEmail: email("gcp-pd-csi"), ImageRegistryEmail: email("image-registry"), NetworkEmail: email("cloud-network")}
	gcp.IAM = &publicv1.ClusterIAMSpec{WorkerServiceAccount: "123456789012-compute@developer.gserviceaccount.com", TemplateApplications: []publicv1.WIFConfigTemplateReference{{Name: ref.Name, UID: ref.UID, Digest: ref.Digest}}}
	privateObj, err := converter.PublicToPrivate(&publicCluster, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := privateObj.(*privatev1.Cluster)
	c.UID = "cluster-uid"
	c.Spec.SafeName = privatev1.DefaultSafeName(c.Name, c.UID)
	if err := c.ValidateCreate(t.Context()); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("Cluster")
	store := memory.NewMemoryStore("clusters", scheme, gvk)
	if err := store.Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewConvertingResourceHandler(store, maintenanceClusterSchemaProcessor(t, publicv1.ClusterSchemaYAML), converter, gvk, "clusters", publicScheme, scheme, nil, logr.Discard())
	router := chi.NewRouter()
	base := "/apis/gcp.managed.openshift.io/v1/namespaces/{namespace}/clusters/{name}"
	router.Patch(base, h.Patch)
	router.Put(base, h.Update)
	path := "/apis/gcp.managed.openshift.io/v1/namespaces/customer/clusters/test-cluster"
	// Establish that this stored fixture supports valid HTTP updates, so the
	// negative cases cannot pass merely because of unrelated schema errors.
	valid := apiWIFRequest(t, router, http.MethodPatch, path, "application/merge-patch+json", map[string]any{"metadata": map[string]any{"labels": map[string]string{"test": "retained-history"}}})
	if valid.Code != http.StatusOK {
		t.Fatalf("valid history-preserving PATCH: %d %s", valid.Code, valid.Body.String())
	}
	// Customer spec writes must never manufacture a Complete controller-owned
	// requirements view. This field is an observation, not an assertion.
	forged := apiWIFRequest(t, router, http.MethodPatch, path, "application/merge-patch+json", map[string]any{"status": map[string]any{"iamRequirements": map[string]any{"state": "Complete", "observedGeneration": 1, "objectUID": "cluster-uid", "clusterUID": "cluster-uid"}}})
	if forged.Code < 200 || forged.Code >= 300 && forged.Code < 400 || forged.Code >= 500 {
		t.Fatalf("status isolation patch failed unexpectedly: %d %s", forged.Code, forged.Body.String())
	}
	// A full public PUT is also unable to turn an assertion into controller
	// status; the resource may reject the request or preserve stored status.
	currentForPUT, err := store.Get(t.Context(), "customer", "test-cluster")
	if err != nil {
		t.Fatal(err)
	}
	publicObject, err := converter.PrivateToPublic(currentForPUT)
	if err != nil {
		t.Fatal(err)
	}
	put := publicObject.(*publicv1.Cluster)
	put.Status.IAMRequirements = &publicv1.IAMRequirementsStatus{State: "Complete", ObjectUID: "cluster-uid", ClusterUID: "cluster-uid", ObservedGeneration: 1}
	forged = apiWIFRequest(t, router, http.MethodPut, path, "application/json", put)
	if forged.Code < 200 || forged.Code >= 300 && forged.Code < 400 || forged.Code >= 500 {
		t.Fatalf("status isolation PUT failed unexpectedly: %d %s", forged.Code, forged.Body.String())
	}
	afterForgery := apiWIFReadCluster(t, store)
	if afterForgery.Status.IAMRequirements != nil {
		t.Fatal("customer PATCH forged a controller-owned IAM requirements status")
	}
	// Seed private controller bookkeeping that the public schema cannot expose.
	// A successful public patch must retain it, not just the public status view.
	afterForgery.Status.IAMDispatch = &privatev1.IAMDispatchState{Dispatched: &privatev1.IAMDispatchReceipt{
		ObjectUID: string(afterForgery.UID), ClusterUID: string(afterForgery.UID),
		ReleaseVersion: afterForgery.Spec.Release.Version, ChannelGroup: afterForgery.Spec.Release.ChannelGroup,
		ReleaseImage: "private-payload", TemplateRef: ref,
	}}
	if err := store.Update(t.Context(), afterForgery); err != nil {
		t.Fatal(err)
	}
	// Exercise the exact executor protocol through the real converting handler,
	// not a fake HTTP server. Revoked verification and a closed assertion-write
	// gate must still permit retaining the identical application history.
	w.Spec.Verified = new(false)
	privatev1.SetValidatorDeps(privatev1.ValidatorDeps{AllowIAMApplicationWrites: false, WIFConfigTemplate: func(context.Context, string) (*privatev1.WIFConfigTemplate, error) { return w, nil }})
	noOp := apiWIFRequest(t, router, http.MethodPatch, path, "application/json-patch+json", []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": string(afterForgery.GetUID())},
		{"op": "test", "path": "/metadata/resourceVersion", "value": afterForgery.GetResourceVersion()},
		{"op": "replace", "path": "/spec/platform/gcp/iam/templateApplications", "value": gcp.IAM.TemplateApplications},
	})
	if noOp.Code != http.StatusOK {
		t.Fatalf("valid JSONPatch retaining history with both gates closed: %d %s", noOp.Code, noOp.Body.String())
	}
	afterNoOp := apiWIFReadCluster(t, store)
	apiWIFAssertNoOp(t, afterForgery, afterNoOp)
	for _, test := range []struct {
		name, content string
		body          any
		code          int
		marker        string
	}{
		{"clear applications", "application/merge-patch+json", map[string]any{"spec": map[string]any{"platform": map[string]any{"gcp": map[string]any{"iam": map[string]any{"templateApplications": []any{}}}}}}, http.StatusBadRequest, "validation failed"},
		{"null iam", "application/merge-patch+json", map[string]any{"spec": map[string]any{"platform": map[string]any{"gcp": map[string]any{"iam": nil}}}}, http.StatusBadRequest, "validation failed"},
		{"null gcp", "application/merge-patch+json", map[string]any{"spec": map[string]any{"platform": map[string]any{"gcp": nil}}}, http.StatusBadRequest, "validation failed"},
		{"edit digest", "application/json-patch+json", []map[string]any{{"op": "replace", "path": "/spec/platform/gcp/iam/templateApplications/0/digest", "value": "sha256:" + strings.Repeat("0", 64)}}, http.StatusBadRequest, "validation failed"},
		{"clear applications JSON", "application/json-patch+json", []map[string]any{{"op": "replace", "path": "/spec/platform/gcp/iam/templateApplications", "value": []any{}}}, http.StatusBadRequest, "validation failed"},
		{"closed write gate JSON", "application/json-patch+json", []map[string]any{{"op": "add", "path": "/spec/platform/gcp/iam/templateApplications/-", "value": publicv1.WIFConfigTemplateReference{Name: "5.0.1", UID: "new-template", Digest: digest}}}, http.StatusBadRequest, "new IAM application writes are disabled"},
		{"stale version", "application/merge-patch+json", map[string]any{"metadata": map[string]any{"resourceVersion": "999999"}}, http.StatusConflict, "resource version mismatch"},
		{"wrong UID JSON test", "application/json-patch+json", []map[string]any{{"op": "test", "path": "/metadata/uid", "value": "replacement-cluster"}, {"op": "add", "path": "/metadata/labels", "value": map[string]string{"cas": "must-not-write"}}}, http.StatusConflict, "JSON patch test failed"},
		{"stale revision JSON test", "application/json-patch+json", []map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(afterNoOp.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": "999999"}, {"op": "add", "path": "/metadata/labels", "value": map[string]string{"cas": "must-not-write"}}}, http.StatusConflict, "JSON patch test failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := apiWIFReadCluster(t, store)
			res := apiWIFRequest(t, router, http.MethodPatch, path, test.content, test.body)
			if res.Code != test.code || !strings.Contains(res.Body.String(), test.marker) {
				t.Fatalf("invalid mutation: %d %s", res.Code, res.Body.String())
			}
			after := apiWIFReadCluster(t, store)
			apiWIFAssertUnchanged(t, before, after)
		})
	}
	// Clearing all public IAM fields in a PUT cannot erase retained history.
	obj, err := store.Get(t.Context(), "customer", "test-cluster")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := converter.PrivateToPublic(obj)
	if err != nil {
		t.Fatal(err)
	}
	pub.(*publicv1.Cluster).Spec.Platform.GCP.IAM = nil
	res := apiWIFRequest(t, router, http.MethodPut, path, "application/json", pub)
	if res.Code < 400 || res.Code >= 500 {
		t.Fatalf("PUT removal: %d %s", res.Code, res.Body.String())
	}
}

func TestPrivateVersionPayloadEditAdvancesCatalogGeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("Version")
	store := memory.NewMemoryStore("versions", scheme, gvk)
	v := &privatev1.Version{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "Version"}, ObjectMeta: metav1.ObjectMeta{Name: "5.0.0", UID: "version-uid", Generation: 7}, Spec: privatev1.VersionSpec{ChannelGroups: []string{"candidate"}, ReleaseImage: "quay.io/example/release@sha256:old"}}
	if err := store.Create(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	h := handlers.NewResourceHandler(store, maintenanceClusterSchemaProcessor(t, privatev1.VersionSchemaYAML), gvk, "versions", scheme, logr.Discard())
	router := chi.NewRouter()
	router.Put("/versions/{name}", h.Update)
	stored, err := store.Get(t.Context(), "", "5.0.0")
	if err != nil {
		t.Fatal(err)
	}
	updated := stored.(*privatev1.Version).DeepCopy()
	updated.Spec.ReleaseImage = "quay.io/example/release@sha256:new"
	res := apiWIFRequest(t, router, http.MethodPut, "/versions/5.0.0", "application/json", updated)
	if res.Code != http.StatusOK {
		t.Fatalf("private payload update failed: %d %s", res.Code, res.Body.String())
	}
	stored, err = store.Get(t.Context(), "", "5.0.0")
	if err != nil {
		t.Fatal(err)
	}
	got := stored.(*privatev1.Version)
	if got.Generation != 8 || got.UID != "version-uid" || got.Spec.ReleaseImage != updated.Spec.ReleaseImage {
		t.Fatalf("payload edit did not change pinned catalog identity: %+v", got)
	}
}

func TestVersionAvailabilityPublicConversion(t *testing.T) {
	converter := newClusterConverter(t)
	v := &privatev1.Version{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "Version"}, Status: privatev1.VersionStatus{Conditions: []metav1.Condition{{Type: "Available", Status: metav1.ConditionTrue, Reason: "Available"}, {Type: "PrivateReason", Status: metav1.ConditionTrue, Reason: "Private"}}}}
	pub, err := converter.PrivateToPublic(v)
	if err != nil {
		t.Fatal(err)
	}
	conditions := pub.(*publicv1.Version).Status.Conditions
	if len(conditions) != 1 || conditions[0].Type != "Available" {
		t.Fatalf("public conditions: %+v", conditions)
	}
}

func TestWIFTemplateUnstructuredDeleteDenied(t *testing.T) {
	scheme, publicScheme := runtime.NewScheme(), runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := publicv1.AddToScheme(publicScheme); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("WIFConfigTemplate")
	processor := maintenanceClusterSchemaProcessor(t, privatev1.WIFConfigTemplateSchemaYAML)
	for _, surface := range []string{"aggregated", "private handler", "converting handler"} {
		t.Run(surface, func(t *testing.T) {
			store := memory.NewMemoryStore("wifconfigtemplates", scheme, gvk)
			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(apiWIFTemplate(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Create(t.Context(), &unstructured.Unstructured{Object: data}); err != nil {
				t.Fatal(err)
			}
			if surface == "aggregated" {
				strategy := aggregated.NewResourceStrategy(scheme, processor, false, gvk, logr.Discard())
				storage := aggregated.NewResourceStorage(store, strategy, gvk, "wifconfigtemplates", "wifconfigtemplate", scheme, logr.Discard(), nil)
				if _, supported := any(storage).(rest.CollectionDeleter); supported {
					t.Fatal("DeleteCollection must remain unsupported")
				}
				if _, _, err := storage.Delete(t.Context(), "5.0.0", nil, &metav1.DeleteOptions{}); !apierrors.IsBadRequest(err) {
					t.Fatalf("unstructured deletion was not denied: %v", err)
				}
			} else {
				router := chi.NewRouter()
				if surface == "private handler" {
					h := handlers.NewResourceHandler(store, processor, gvk, "wifconfigtemplates", scheme, logr.Discard())
					router.Delete("/wifconfigtemplates/{name}", h.Delete)
				} else {
					converter := conversion.NewConverter(publicScheme, scheme, "")
					h := handlers.NewConvertingResourceHandler(store, processor, converter, gvk, "wifconfigtemplates", publicScheme, scheme, nil, logr.Discard())
					router.Delete("/wifconfigtemplates/{name}", h.Delete)
				}
				res := apiWIFRequest(t, router, http.MethodDelete, "/wifconfigtemplates/5.0.0", "application/json", map[string]any{})
				if res.Code != http.StatusBadRequest {
					t.Fatalf("unstructured delete: %d %s", res.Code, res.Body.String())
				}
			}
			if _, err := store.Get(t.Context(), "", "5.0.0"); err != nil {
				t.Fatalf("retained template disappeared: %v", err)
			}
		})
	}
}

func TestVersionStatusSubresourcePreservesPinnedSpec(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := privatev1.GroupVersion.WithKind("Version")
	store := memory.NewMemoryStore("versions", scheme, gvk)
	v := &privatev1.Version{TypeMeta: metav1.TypeMeta{APIVersion: privatev1.GroupVersion.String(), Kind: "Version"}, ObjectMeta: metav1.ObjectMeta{Name: "5.0.0", Generation: 1}, Spec: privatev1.VersionSpec{ChannelGroups: []string{"stable"}, ReleaseImage: "quay.io/example/release@sha256:abc", WIFConfigTemplateRef: &privatev1.WIFConfigTemplateReference{Name: "5.0.0", UID: "template-uid", Digest: "sha256:" + strings.Repeat("0", 64)}}}
	if err := store.Create(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	strategy := aggregated.NewResourceStrategy(scheme, maintenanceClusterSchemaProcessor(t, privatev1.VersionSchemaYAML), false, gvk, logr.Discard())
	resource := aggregated.NewResourceStorage(store, strategy, gvk, "versions", "version", scheme, logr.Discard(), nil)
	status := aggregated.NewStatusStorage(resource)
	updater := rest.DefaultUpdatedObjectInfo(nil, func(_ context.Context, _ runtime.Object, old runtime.Object) (runtime.Object, error) {
		next := old.DeepCopyObject().(*privatev1.Version)
		next.Spec.ReleaseImage = "forged-spec"
		next.Spec.WIFConfigTemplateRef = nil
		next.Status.Conditions = []metav1.Condition{{Type: "Available", Status: metav1.ConditionTrue, Reason: "Available", Message: "test", ObservedGeneration: 1, LastTransitionTime: metav1.Now()}}
		return next, nil
	})
	if _, _, err := status.Update(t.Context(), v.Name, updater, nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	obj, err := store.Get(t.Context(), "", v.Name)
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*privatev1.Version)
	if got.Spec.ReleaseImage != v.Spec.ReleaseImage || got.Spec.WIFConfigTemplateRef == nil || len(got.Status.Conditions) != 1 {
		t.Fatal("status subresource failed spec/status isolation")
	}
}
