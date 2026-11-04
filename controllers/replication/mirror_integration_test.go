package replication

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/aggregated"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/storage/memory"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestAPIMirrorAppliesToDirectPrivateAPI(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	if err := privatev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	config := aggregated.Config{
		Scheme:    scheme,
		Resources: []types.ResourceInfo{privatev1.RoleResourceInfo},
		StorageFactory: func(resourceType string, scheme *runtime.Scheme, gvk schema.GroupVersionKind) (storage.ResourceStore, error) {
			return memory.NewMemoryStore(resourceType, scheme, gvk), nil
		},
		BindAddress: "127.0.0.1",
		BindPort:    port,
		DisableAuth: true,
		Logger:      logr.Discard(),
	}
	serverConfig, err := config.Complete()
	if err != nil {
		t.Fatal(err)
	}
	server, err := aggregated.New(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.GenericAPIServer.PrepareRun().RunWithContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("private API shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("private API did not shut down")
		}
	})

	baseURL := fmt.Sprintf("https://127.0.0.1:%d", port)
	// The in-process private API uses a temporary self-signed certificate.
	clientConfig := &rest.Config{Host: baseURL, TLSClientConfig: rest.TLSClientConfig{Insecure: true}} //nolint:gosec
	httpClient, err := rest.HTTPClientFor(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := httpClient.Get(baseURL + "/readyz") //nolint:noctx
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("private API did not become ready: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	api, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, "tenant-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("direct private API unexpectedly serves core Namespaces: %v", err)
	}
	mirror, err := NewAPIMirror(api, Config{Region: "us-west1", LeaderRegion: "us-central1", Generation: 3, ResyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := mirror.ApplyIncremental(ctx, roleEvent(t, "admin", "1")); err != nil || !applied {
		t.Fatalf("Role apply through direct private API: applied=%v err=%v", applied, err)
	}
	role, err := api.Resource(supportedResources[roleResourceID]).Namespace("tenant-a").Get(ctx, "admin", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if role.GetAnnotations()[sourceVersionAnnotation] != "1" {
		t.Fatalf("Role missing source version annotation: %v", role.GetAnnotations())
	}
	snapshotRole := roleEvent(t, "viewer", "2")
	snapshotRole.SyncID = "direct-private-api"
	snapshotRole.SnapshotResourceVersion = "3"
	if _, err := mirror.StageSnapshot(ctx, snapshotRole); err != nil {
		t.Fatal(err)
	}
	inventory := Event{
		EventType: Inventory, ResourceKind: roleResourceID, OriginRegion: "us-central1", Generation: 3,
		SnapshotResourceVersion: "3", SyncID: snapshotRole.SyncID, PageCount: 1,
		Inventory: []ObjectRef{{Namespace: "tenant-a", Name: "viewer", SourceResourceVersion: "2", SourceUID: "uid-viewer"}},
		UpdatedAt: time.Now().UTC(),
	}
	result, err := mirror.StageSnapshot(ctx, inventory)
	if err != nil || !result.Completed {
		t.Fatalf("snapshot through direct private API: result=%+v err=%v", result, err)
	}
	if _, err := api.Resource(supportedResources[roleResourceID]).Namespace("tenant-a").Get(ctx, "viewer", metav1.GetOptions{}); err != nil {
		t.Fatalf("snapshot Role missing from private API: %v", err)
	}
}
