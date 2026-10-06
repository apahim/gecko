package versionsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type ciTag struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	PullSpec string `json:"pullSpec"`
}

func accepted(version string) ciTag {
	return ciTag{Name: version, Phase: "Accepted", PullSpec: "registry.ci.openshift.org/ocp/release:" + version}
}
func ciChannel(name, defaultVersion string, streams ...string) *privatev1.Channel {
	return &privatev1.Channel{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 3}, Spec: privatev1.ChannelSpec{InstallDefaultVersion: defaultVersion, ReleaseStreams: streams}}
}
func ciTestController(t *testing.T, store client.Client, responses map[string][]ciTag) *Controller {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for stream, tags := range responses {
			if r.URL.Path == "/api/v1/releasestream/"+stream+"/tags" {
				_ = json.NewEncoder(w).Encode(struct {
					Name string  `json:"name"`
					Tags []ciTag `json:"tags"`
				}{stream, tags})
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	source, err := NewReleaseControllerClient(server.URL)
	require.NoError(t, err)
	return NewCIController(source, newTestLogger(t), store)
}
func ciStore(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, privatev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&privatev1.Channel{}).WithObjects(objects...).Build()
}
func readChannel(t *testing.T, store client.Client, name string) *privatev1.Channel {
	t.Helper()
	channel := &privatev1.Channel{}
	require.NoError(t, store.Get(context.Background(), client.ObjectKey{Name: name}, channel))
	return channel
}
func TestCISyncCatalogAndDefaults(t *testing.T) {
	ctx := context.Background()
	nightly := "4.22.0-0.nightly-2026-10-04-051631"
	store := ciStore(t,
		ciChannel("custom-stable", "4.22.1", "4-stable", "extra"),
		ciChannel("custom-nightly", "4.22.1", "nightlies"),
		ciChannel("shared", nightly, "nightlies"),
		&privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.22.9"}, Spec: privatev1.VersionSpec{ReleaseImage: "old", ChannelGroups: []string{"removed"}}},
	)
	responses := map[string][]ciTag{
		"4-stable":  {accepted("4.22.1"), accepted("4.22.2"), accepted("4.21.9")},
		"extra":     {accepted("4.22.2"), accepted("4.23.0-rc.1")},
		"nightlies": {accepted(nightly), {Name: "4.22.3", Phase: "Rejected", PullSpec: "rejected"}},
	}
	controller := ciTestController(t, store, responses)
	controller.sync(ctx, newTestLogger(t))
	var versions privatev1.VersionList
	require.NoError(t, store.List(ctx, &versions))
	require.Len(t, versions.Items, 4)
	for _, version := range versions.Items {
		if version.Name == nightly {
			assert.Equal(t, []string{"custom-nightly", "shared"}, version.Spec.ChannelGroups)
		} else {
			assert.Equal(t, []string{"custom-stable"}, version.Spec.ChannelGroups)
		}
	}
	stable := readChannel(t, store, "custom-stable")
	condition := meta.FindStatusCondition(stable.Status.Conditions, privatev1.ChannelDefaultVersionAvailable)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.EqualValues(t, 3, condition.ObservedGeneration)
	missing := readChannel(t, store, "custom-nightly")
	condition = meta.FindStatusCondition(missing.Status.Conditions, privatev1.ChannelDefaultVersionAvailable)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "4.22.1", missing.Spec.InstallDefaultVersion)
	// Reconciliation is idempotent for catalog contents and status writes.
	controller.sync(ctx, newTestLogger(t))
	assert.Equal(t, stable, readChannel(t, store, "custom-stable"))
	assert.Equal(t, missing, readChannel(t, store, "custom-nightly"))
	var repeated privatev1.VersionList
	require.NoError(t, store.List(ctx, &repeated))
	assert.Equal(t, versions.Items, repeated.Items)
	// A previously missing pin becomes available without changing the pin.
	responses["nightlies"] = append(responses["nightlies"], accepted("4.22.1"))
	controller.sync(ctx, newTestLogger(t))
	assert.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(readChannel(t, store, "custom-nightly").Status.Conditions, privatev1.ChannelDefaultVersionAvailable).Status)
}

func TestCIFailedSnapshotPreservesVersions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streams   []string
		responses map[string][]ciTag
	}{
		{"no streams", nil, nil},
		{"unknown stream", []string{"missing"}, nil},
		{"partial failure", []string{"good", "missing"}, map[string][]ciTag{"good": {accepted("4.22.2")}}},
		{"empty catalog", []string{"empty"}, map[string][]ciTag{"empty": {}}},
		{"unsupported only", []string{"old"}, map[string][]ciTag{"old": {accepted("4.21.1")}}},
		{"missing payload", []string{"bad"}, map[string][]ciTag{"bad": {{Name: "4.22.2", Phase: "Accepted"}}}},
		{"conflicting payloads", []string{"one", "two"}, map[string][]ciTag{"one": {accepted("4.22.1")}, "two": {{Name: "4.22.1", Phase: "Accepted", PullSpec: "different"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.22.1"}, Spec: privatev1.VersionSpec{ReleaseImage: "old", ChannelGroups: []string{"test"}}}
			store := ciStore(t, old, ciChannel("test", "4.22.1", tc.streams...))
			controller := ciTestController(t, store, tc.responses)
			controller.sync(context.Background(), newTestLogger(t))
			var versions privatev1.VersionList
			require.NoError(t, store.List(context.Background(), &versions))
			require.Len(t, versions.Items, 1)
			assert.Equal(t, old.Spec, versions.Items[0].Spec)
			condition := meta.FindStatusCondition(readChannel(t, store, "test").Status.Conditions, privatev1.ChannelDefaultVersionAvailable)
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionUnknown, condition.Status)
			assert.Equal(t, "FetchFailed", condition.Reason)
		})
	}
}

func TestCIEmptyStreamDoesNotBlockOtherChannels(t *testing.T) {
	store := ciStore(t, ciChannel("empty", "4.22.1", "empty"), ciChannel("good", "4.22.2", "good"))
	controller := ciTestController(t, store, map[string][]ciTag{"empty": {}, "good": {accepted("4.22.2")}})
	controller.sync(context.Background(), newTestLogger(t))
	assert.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(readChannel(t, store, "empty").Status.Conditions, privatev1.ChannelDefaultVersionAvailable).Status)
	assert.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(readChannel(t, store, "good").Status.Conditions, privatev1.ChannelDefaultVersionAvailable).Status)
}

type failingVersionCreate struct{ client.Client }

func (c failingVersionCreate) Create(context.Context, client.Object, ...client.CreateOption) error {
	return fmt.Errorf("write unavailable")
}
func TestCIApplyFailureReportsUnknown(t *testing.T) {
	store := ciStore(t, ciChannel("test", "4.22.2", "stream"), &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.22.1"}})
	controller := ciTestController(t, failingVersionCreate{store}, map[string][]ciTag{"stream": {accepted("4.22.2")}})
	controller.sync(context.Background(), newTestLogger(t))
	condition := meta.FindStatusCondition(readChannel(t, store, "test").Status.Conditions, privatev1.ChannelDefaultVersionAvailable)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionUnknown, condition.Status)
	assert.Equal(t, "ApplyFailed", condition.Reason)
	var old privatev1.Version
	require.NoError(t, store.Get(context.Background(), client.ObjectKey{Name: "4.22.1"}, &old))
}

// A status outage must not block catalog updates, and the next sync must retry it.
type failingStatusClient struct{ client.Client }
type failingStatusWriter struct{ client.SubResourceWriter }

func (c failingStatusClient) Status() client.SubResourceWriter {
	return failingStatusWriter{c.Client.Status()}
}
func (w failingStatusWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return fmt.Errorf("status unavailable")
}
func TestCIStatusFailureRetriesWithoutBlockingCatalog(t *testing.T) {
	store := ciStore(t, ciChannel("test", "4.22.1", "stream"))
	controller := ciTestController(t, failingStatusClient{store}, map[string][]ciTag{"stream": {accepted("4.22.1")}})
	controller.sync(context.Background(), newTestLogger(t))
	var version privatev1.Version
	require.NoError(t, store.Get(context.Background(), client.ObjectKey{Name: "4.22.1"}, &version))
	assert.Empty(t, readChannel(t, store, "test").Status.Conditions)
	controller.apiClient = store
	controller.sync(context.Background(), newTestLogger(t))
	assert.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(readChannel(t, store, "test").Status.Conditions, privatev1.ChannelDefaultVersionAvailable).Status)
}

func TestCICatalogChangesRemoveStaleMemberships(t *testing.T) {
	store := ciStore(t, ciChannel("one", "4.22.1", "one"), ciChannel("two", "4.22.1", "two"))
	responses := map[string][]ciTag{"one": {accepted("4.22.1"), accepted("4.22.2")}, "two": {accepted("4.22.1")}}
	controller := ciTestController(t, store, responses)
	controller.sync(context.Background(), newTestLogger(t))
	responses["one"] = []ciTag{}
	controller.sync(context.Background(), newTestLogger(t))
	var versions privatev1.VersionList
	require.NoError(t, store.List(context.Background(), &versions))
	require.Len(t, versions.Items, 1)
	assert.Equal(t, "4.22.1", versions.Items[0].Name)
	assert.Equal(t, []string{"two"}, versions.Items[0].Spec.ChannelGroups)
	assert.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(readChannel(t, store, "one").Status.Conditions, privatev1.ChannelDefaultVersionAvailable).Status)
}
