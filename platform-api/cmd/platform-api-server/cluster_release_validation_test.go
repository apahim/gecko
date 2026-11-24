package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
)

func newReleaseValidationVersion(name string, groups ...string) *privatev1.Version {
	version := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: privatev1.VersionSpec{ReleaseImage: "image", ChannelGroups: groups}}
	version.SetGroupVersionKind(privatev1.GroupVersion.WithKind("Version"))
	return version
}

func TestClusterReleaseValidatorCatalog(t *testing.T) {
	scheme := getPrivateScheme()
	versionGVK := privatev1.GroupVersion.WithKind("Version")
	versions := memory.NewMemoryStore(apiserver.GroupKindResourceType(versionGVK.GroupKind()), scheme, versionGVK)
	for _, version := range []*privatev1.Version{
		newReleaseValidationVersion("4.22.14", "stable"),
		newReleaseValidationVersion("4.23.9", "candidate"),
	} {
		if err := versions.Create(t.Context(), version); err != nil {
			t.Fatal(err)
		}
	}
	validator, err := newClusterReleaseValidator(func(resourceType string, s *runtime.Scheme, gvk runtimeschema.GroupVersionKind) (storage.ResourceStore, error) {
		return versions, nil
	}, scheme)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, version, group, want string
	}{
		{name: "published concrete version", version: "4.22.14", group: "stable"},
		{name: "missing concrete version", version: "4.99.999", group: "stable", want: "version 4.99.999 is not present in the Version catalog"},
		{name: "wrong channel group", version: "4.23.9", group: "stable", want: "version 4.23.9 is not published in channel group stable"},
		{name: "published stream", version: "4.23", group: "candidate"},
		{name: "stream without catalog versions", version: "4.99", group: "stable", want: "no version in stream 4.99 is published in channel group stable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validator.Validate(t.Context(), tc.version, tc.group)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate(%s, %s) = %v, want nil", tc.version, tc.group, err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Validate(%s, %s) = %v, want %q", tc.version, tc.group, err, tc.want)
			}
		})
	}
}

func TestClusterReleaseCatalogAdmission(t *testing.T) {
	privatePort, publicPort := freePublicAPITestPort(t), freePublicAPITestPort(t)
	privateScheme := getPrivateScheme()
	clusterGVK := privatev1.GroupVersion.WithKind("Cluster")
	clusterStore := memory.NewMemoryStore(apiserver.GroupKindResourceType(clusterGVK.GroupKind()), privateScheme, clusterGVK)
	versionGVK := privatev1.GroupVersion.WithKind("Version")
	versionStore := memory.NewMemoryStore(apiserver.GroupKindResourceType(versionGVK.GroupKind()), privateScheme, versionGVK)

	for _, version := range []*privatev1.Version{
		newReleaseValidationVersion("4.22.14", "stable"),
		newReleaseValidationVersion("4.22.15", "stable"),
	} {
		if err := versionStore.Create(t.Context(), version); err != nil {
			t.Fatal(err)
		}
	}

	cluster := testCluster("customer-project", "example-cluster", "us-central1")
	cluster.Spec.Platform.GCP = &privatev1.GCPClusterPlatform{
		ProjectID: "customer-project", Region: "us-central1", Network: "network", Subnet: "subnet",
		WorkloadIdentity: privatev1.WorkloadIdentitySpec{PoolID: "pool", ProjectNumber: "123456789", ProviderID: "provider",
			ServiceAccountsRef: &privatev1.ServiceAccountsRef{
				NodePoolEmail: "nodepool@example.iam.gserviceaccount.com", ControlPlaneEmail: "control@example.iam.gserviceaccount.com",
				CloudControllerEmail: "cloud@example.iam.gserviceaccount.com", StorageEmail: "storage@example.iam.gserviceaccount.com",
				ImageRegistryEmail: "registry@example.iam.gserviceaccount.com", NetworkEmail: "network@example.iam.gserviceaccount.com",
			}},
	}
	cluster.Spec.Release = privatev1.ClusterReleaseSpec{Version: "4.22.14", ChannelGroup: "stable"}
	cluster.Status.HostedClusterResult = &privatev1.HostedClusterResult{
		Version: "4.22.14", DesiredVersion: "4.22.14", AvailableUpdates: []string{"4.22.15", "4.22.16"},
	}
	if err := clusterStore.Create(t.Context(), cluster); err != nil {
		t.Fatal(err)
	}

	stores := map[string]storage.ResourceStore{
		"Cluster": clusterStore,
		"Version": versionStore,
	}
	factory := func(resourceType string, scheme *runtime.Scheme, gvk runtimeschema.GroupVersionKind) (storage.ResourceStore, error) {
		if store := stores[gvk.Kind]; store != nil {
			return store, nil
		}
		return memory.NewMemoryStore(resourceType, scheme, gvk), nil
	}
	releaseValidator, err := newClusterReleaseValidator(factory, privateScheme)
	if err != nil {
		t.Fatal(err)
	}
	privatev1.SetValidatorDeps(privatev1.ValidatorDeps{ValidateClusterRelease: releaseValidator.Validate})
	t.Cleanup(func() { privatev1.SetValidatorDeps(privatev1.ValidatorDeps{}) })

	server, err := apiserver.New(apiserver.Options{
		Address: "127.0.0.1",
		Private: apiserver.PrivateAPIOptions{
			Port: privatePort, Scheme: privateScheme, Resources: getPrivateResources(), DisableAuth: true,
		},
		Public: apiserver.PublicAPIOptions{
			Enable: true, Address: "127.0.0.1", Port: publicPort, Scheme: getPublicScheme(), Resources: getPublicResources(),
		},
		StorageFactory: factory,
		Logger:         logr.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = server.Run()
		close(runDone)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown API server: %v", err)
		}
		select {
		case <-runDone:
			if runErr != nil {
				t.Errorf("API server stopped with error: %v", runErr)
			}
		case <-ctx.Done():
			t.Errorf("API server did not stop: %v", ctx.Err())
		}
	})
	privateBaseURL := fmt.Sprintf("https://127.0.0.1:%d", privatePort)
	publicBaseURL := fmt.Sprintf("http://127.0.0.1:%d", publicPort)
	client := &http.Client{Timeout: 3 * time.Second}
	waitForPublicAPITestServer(t, client, publicBaseURL+"/apis", runDone, &runErr)
	privateClient := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}

	createBody := func(version string) []byte {
		body, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]string{"name": "new-cluster"},
			"spec": map[string]interface{}{
				"release": map[string]string{"version": version, "channelGroup": "stable"},
				"platform": map[string]interface{}{
					"type": "GCP",
					"gcp": map[string]interface{}{
						"projectID": "customer-project", "region": "us-central1", "network": "network", "subnet": "subnet",
						"workloadIdentity": map[string]interface{}{
							"poolID": "pool", "projectNumber": "123456789", "providerID": "provider",
							"serviceAccountsRef": map[string]string{
								"nodePoolEmail": "nodepool@example.iam.gserviceaccount.com", "controlPlaneEmail": "control@example.iam.gserviceaccount.com",
								"cloudControllerEmail": "cloud@example.iam.gserviceaccount.com", "storageEmail": "storage@example.iam.gserviceaccount.com",
								"imageRegistryEmail": "registry@example.iam.gserviceaccount.com", "networkEmail": "network@example.iam.gserviceaccount.com",
							},
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, tc := range []struct {
		name, version string
		status        int
		wantBody      string
	}{
		{name: "create rejects missing version", version: "4.99.999", status: http.StatusUnprocessableEntity, wantBody: "version 4.99.999 is not present in the Version catalog"},
		{name: "create accepts published version", version: "4.22.14", status: http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				privateBaseURL+"/apis/gcp.managed.openshift.io/v1/namespaces/customer-project/clusters", bytes.NewReader(createBody(tc.version)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := privateClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status {
				t.Fatalf("HTTP status=%d, want %d: %s", response.StatusCode, tc.status, data)
			}
			if tc.wantBody != "" && !strings.Contains(string(data), tc.wantBody) {
				t.Fatalf("expected catalog rejection %q: %s", tc.wantBody, data)
			}
		})
	}

	patchBody, err := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"release": map[string]string{"version": "4.22.16"}}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPatch,
		publicBaseURL+"/apis/gcp.managed.openshift.io/v1/namespaces/customer-project/clusters/example-cluster", bytes.NewReader(patchBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "version 4.22.16 is not present in the Version catalog") {
		t.Fatalf("upgrade status=%d, want catalog rejection: %s", response.StatusCode, data)
	}

	patchBody, err = json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"release": map[string]string{"version": "4.22.15"}}})
	if err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPatch,
		publicBaseURL+"/apis/gcp.managed.openshift.io/v1/namespaces/customer-project/clusters/example-cluster", bytes.NewReader(patchBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("published upgrade status=%d, want %d: %s", response.StatusCode, http.StatusOK, data)
	}
}
