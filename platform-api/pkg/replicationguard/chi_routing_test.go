package replicationguard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestChiRoutesNonCanonicalPathsToWriteHandlers pins the chi routing behavior the
// follower write guard is built on. The guard does not canonicalize request paths
// precisely because chi does not either; it matches raw path segments. This test
// is the source of truth for that assumption: should a future chi upgrade change
// how it dispatches "." / ".." or empty segments, parseRoute would quietly fall
// out of step with the router, and this test fails loudly rather than letting a
// write slip past the guard.
func TestChiRoutesNonCanonicalPathsToWriteHandlers(t *testing.T) {
	writeHandler := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	router := chi.NewRouter()
	router.Route("/apis/g/v1", func(r chi.Router) {
		r.Route("/namespaces/{namespace}", func(r chi.Router) {
			r.Post("/roles", writeHandler)
			r.Put("/roles/{name}", writeHandler)
			// Nested parent/child route, mirroring how orlop registers a nested
			// resource (registerVerbsForNestedResource mounts the child subrouter
			// and serves the collection on "/").
			r.Route("/clusters/{parentID}/widgets", func(r chi.Router) {
				r.Post("/", writeHandler)
				r.Put("/{name}", writeHandler)
			})
		})
	})

	for _, test := range []struct {
		name        string
		method      string
		path        string
		wantsRouted bool
	}{
		{"canonical collection", http.MethodPost, "/apis/g/v1/namespaces/customer/roles", true},
		{"dot-dot namespace", http.MethodPost, "/apis/g/v1/namespaces/../roles", true},
		{"dot namespace", http.MethodPost, "/apis/g/v1/namespaces/./roles", true},
		{"empty namespace", http.MethodPost, "/apis/g/v1/namespaces//roles", true},
		{"dot name", http.MethodPut, "/apis/g/v1/namespaces/customer/roles/.", true},
		{"dot-dot name", http.MethodPut, "/apis/g/v1/namespaces/customer/roles/..", true},
		{"extra dot-dot segment", http.MethodPost, "/apis/g/v1/namespaces/customer/../roles", false},
		{"collection trailing slash", http.MethodPost, "/apis/g/v1/namespaces/customer/roles/", false},
		{"empty name", http.MethodPut, "/apis/g/v1/namespaces/customer/roles/", false},
		// Nested parent/child routes. The guard must fence the nested collection
		// both with and without a trailing slash (chi's Mount serves both), so
		// pin that chi actually dispatches both here.
		{"nested collection", http.MethodPost, "/apis/g/v1/namespaces/customer/clusters/c1/widgets", true},
		{"nested collection trailing slash", http.MethodPost, "/apis/g/v1/namespaces/customer/clusters/c1/widgets/", true},
		{"nested item", http.MethodPut, "/apis/g/v1/namespaces/customer/clusters/c1/widgets/w1", true},
		{"nested item trailing slash", http.MethodPut, "/apis/g/v1/namespaces/customer/clusters/c1/widgets/w1/", false},
		{"nested dot-dot parent id", http.MethodPost, "/apis/g/v1/namespaces/customer/clusters/../widgets", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), test.method, test.path, nil))
			routed := response.Code == http.StatusNoContent
			if routed != test.wantsRouted {
				t.Fatalf("%s %s: routed = %v (status %d), want routed = %v", test.method, test.path, routed, response.Code, test.wantsRouted)
			}
		})
	}
}
