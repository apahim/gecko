package versionsync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReleaseControllerAcceptedTags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/prefix/api/v1/releasestream/4.22.0-0.nightly/tags", r.URL.Path)
		assert.Equal(t, "Accepted", r.URL.Query().Get("phase"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		fmt.Fprint(w, `{"name":"4.22.0-0.nightly","tags":[
   {"name":"4.22.0-0.nightly-2026-10-04-051631","phase":"Accepted","pullSpec":"registry.ci.openshift.org/ocp/release:4.22.0-0.nightly-2026-10-04-051631"},
   {"name":"4.22.0-0.nightly-2026-10-01-110840","phase":"Accepted","pullSpec":"registry.ci.openshift.org/ocp/release:4.22.0-0.nightly-2026-10-01-110840"},
   {"name":"4.22.0-0.nightly-rejected","phase":"Rejected"},
   {"name":"4.22.0-0.nightly-ready","phase":"Ready"},
   {"name":"4.22.0-0.nightly-pending","phase":"Pending"},
   {"name":"4.22.0-0.nightly-failed","phase":"Failed"}]}`)
	}))
	defer server.Close()
	source, err := NewReleaseControllerClient(server.URL + "/prefix/")
	require.NoError(t, err)
	releases, err := source.ListReleases(context.Background(), "4.22.0-0.nightly")
	require.NoError(t, err)
	require.Len(t, releases, 2)
	assert.Equal(t, "4.22.0-0.nightly-2026-10-04-051631", releases[0].Version)
	assert.Equal(t, "registry.ci.openshift.org/ocp/release:4.22.0-0.nightly-2026-10-01-110840", releases[1].Payload)
}

func TestReleaseControllerErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"unknown stream", "", 404},
		{"outage", "", 503},
		{"invalid JSON", `{`, 200},
		{"missing tags", `{"name":"stream"}`, 200},
		{"invalid tags", `{"name":"stream","tags":{}}`, 200},
		{"graph response", `{"nodes":[]}`, 200},
		{"wrong stream", `{"name":"other","tags":[]}`, 200},
		{"missing payload", `{"name":"stream","tags":[{"name":"4.22.1","phase":"Accepted"}]}`, 200},
		{"missing name", `{"name":"stream","tags":[{"pullSpec":"image","phase":"Accepted"}]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			source, err := NewReleaseControllerClient(server.URL)
			require.NoError(t, err)
			_, err = source.ListReleases(context.Background(), "stream")
			require.Error(t, err)
		})
	}
}

func TestReleaseControllerConfigurationAndCancellation(t *testing.T) {
	for _, endpoint := range []string{"", "localhost", "ftp://host", "https://host?query=x", "https://host/#fragment"} {
		_, err := NewReleaseControllerClient(endpoint)
		require.Error(t, err)
	}
	source, err := NewReleaseControllerClient("https://example.invalid")
	require.NoError(t, err)
	for _, stream := range []string{"", "..", "foo/bar", "foo?bar"} {
		_, err := source.ListReleases(context.Background(), stream)
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = source.ListReleases(ctx, "stream")
	require.ErrorIs(t, err, context.Canceled)
}
