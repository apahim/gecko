package versionsync

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/versionresolution"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fleetChannel(name, minimum string) privatev1.Channel {
	return privatev1.Channel{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: privatev1.ChannelSpec{FleetMinorVersion: minimum, InstallDefaultVersion: minimum + ".0"}}
}

func TestDiscoveryStartsAtFleetMinorAndCrossesMajorBoundary(t *testing.T) {
	for _, tc := range []struct {
		minimum string
		want    []string
	}{
		{"4.21", []string{"4.21.1", "4.22.1", "4.23.1", "5.0.0", "5.1.0"}},
		{"4.23", []string{"4.23.1", "5.0.0", "5.1.0"}},
		{"5.0", []string{"5.0.0", "5.1.0"}},
	} {
		t.Run(tc.minimum, func(t *testing.T) {
			queries := []string{}
			server := newCincinnatiServer(t, func(channel string) ([]versionresolution.ReleaseInfo, int) {
				queries = append(queries, channel)
				versions := map[string]string{"stable-4.21": "4.21.1", "stable-4.22": "4.22.1", "stable-4.23": "4.23.1", "stable-5.0": "5.0.0", "stable-5.1": "5.1.0"}
				if version, ok := versions[channel]; ok {
					return []versionresolution.ReleaseInfo{{Version: version, Payload: "image:" + version}, {Version: "4.20.1", Payload: "old-upgrade-source"}}, http.StatusOK
				}
				return nil, http.StatusOK
			})
			defer server.Close()
			channel := fleetChannel("stable", tc.minimum)
			store := catalogStore(t, &channel)
			newController(t, server, store).sync(context.Background(), newTestLogger(t))
			require.NotEmpty(t, queries)
			assert.Equal(t, "stable-"+tc.minimum, queries[0])
			assert.Contains(t, queries, "stable-5.0")
			var versions privatev1.VersionList
			require.NoError(t, store.List(context.Background(), &versions))
			names := make([]string, 0, len(versions.Items))
			for _, version := range versions.Items {
				names = append(names, version.Name)
			}
			assert.ElementsMatch(t, tc.want, names)
		})
	}
}

func TestDiscoveryUsesEachChannelsFleetMinimum(t *testing.T) {
	queries := []string{}
	server := newCincinnatiServer(t, func(channel string) ([]versionresolution.ReleaseInfo, int) {
		queries = append(queries, channel)
		switch channel {
		case "stable-4.21", "stable-5.0", "fast-5.0":
			return []versionresolution.ReleaseInfo{{Version: "4.21.1", Payload: "old"}, {Version: "5.0.0", Payload: "new"}}, http.StatusOK
		default:
			return nil, http.StatusOK
		}
	})
	defer server.Close()
	stable, fast := fleetChannel("stable", "4.21"), fleetChannel("fast", "5.0")
	store := catalogStore(t, &stable, &fast)
	newController(t, server, store).sync(context.Background(), newTestLogger(t))
	assert.Contains(t, queries, "stable-4.21")
	assert.NotContains(t, queries, "fast-4.22")
	var versions privatev1.VersionList
	require.NoError(t, store.List(context.Background(), &versions))
	require.Len(t, versions.Items, 2)
	for _, version := range versions.Items {
		if version.Name == "4.21.1" {
			assert.Equal(t, []string{"stable"}, version.Spec.ChannelGroups)
		} else {
			assert.Equal(t, []string{"fast", "stable"}, version.Spec.ChannelGroups)
		}
	}
}

func catalogStore(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, privatev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestInvalidFleetMinimumPreservesCatalog(t *testing.T) {
	for _, minimum := range []string{"", "4", "4.22.1", "-1.0", "4.-1", "04.22", "4.022", "4.22x"} {
		t.Run(minimum, func(t *testing.T) {
			channel := fleetChannel("stable", minimum)
			old := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.22.1"}, Spec: privatev1.VersionSpec{ChannelGroups: []string{"stable"}, ReleaseImage: "old"}}
			store := catalogStore(t, &channel, old)
			server := newCincinnatiServer(t, func(string) ([]versionresolution.ReleaseInfo, int) {
				t.Error("invalid fleet minor should fail before requesting Cincinnati")
				return nil, http.StatusOK
			})
			defer server.Close()
			newController(t, server, store).sync(context.Background(), newTestLogger(t))
			var versions privatev1.VersionList
			require.NoError(t, store.List(context.Background(), &versions))
			require.Len(t, versions.Items, 1)
			assert.Equal(t, old.Spec, versions.Items[0].Spec)
		})
	}
}

func TestRaisingFleetMinimumRemovesOlderMembershipWithoutRepinning(t *testing.T) {
	ctx := context.Background()
	channel := fleetChannel("stable", "4.23")
	channel.Spec.InstallDefaultVersion = "4.23.1"
	store := catalogStore(t, &channel)
	server := newCincinnatiServer(t, func(channel string) ([]versionresolution.ReleaseInfo, int) {
		if channel == "stable-4.23" || channel == "stable-5.0" {
			return []versionresolution.ReleaseInfo{{Version: "4.23.1", Payload: "old"}, {Version: "5.0.0-rc.1", Payload: "new"}}, http.StatusOK
		}
		return nil, http.StatusOK
	})
	defer server.Close()
	controller := newController(t, server, store)
	controller.sync(ctx, newTestLogger(t))
	var updated privatev1.Channel
	require.NoError(t, store.Get(ctx, client.ObjectKey{Name: "stable"}, &updated))
	updated.Spec.FleetMinorVersion = "5.0"
	require.NoError(t, store.Update(ctx, &updated))
	controller.sync(ctx, newTestLogger(t))
	var versions privatev1.VersionList
	require.NoError(t, store.List(ctx, &versions))
	require.Len(t, versions.Items, 1)
	assert.Equal(t, "5.0.0-rc.1", versions.Items[0].Name)
	require.NoError(t, store.Get(ctx, client.ObjectKey{Name: "stable"}, &updated))
	assert.Equal(t, "4.23.1", updated.Spec.InstallDefaultVersion)
}
