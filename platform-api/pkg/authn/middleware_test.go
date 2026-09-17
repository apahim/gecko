package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
)

func TestMiddlewareRequiresVerifiedEmail(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		status int
	}{
		{name: "missing header", status: http.StatusUnauthorized},
		{name: "missing email", claims: map[string]any{"email_verified": true}, status: http.StatusUnauthorized},
		{name: "empty email", claims: map[string]any{"email": "", "email_verified": true}, status: http.StatusUnauthorized},
		{name: "missing verified claim", claims: map[string]any{"email": "alice@example.com"}, status: http.StatusUnauthorized},
		{name: "unverified", claims: map[string]any{"email": "alice@example.com", "email_verified": false}, status: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/apis/gcp.managed.openshift.io/v1/namespaces/project-a/clusters", nil)
			if tt.claims != nil {
				payload, err := json.Marshal(tt.claims)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set(constants.HeaderEndpointAPIUserInfo, base64.RawURLEncoding.EncodeToString(payload))
			}
			response := httptest.NewRecorder()
			Middleware(Config{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, req)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d", response.Code, tt.status)
			}
		})
	}
}

func TestMiddlewareNormalizesEmailAndSetsContext(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"email":          "Alice@EXAMPLE.COM",
		"email_verified": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/apis/gcp.managed.openshift.io/v1/namespaces/project-a/clusters", nil)
	req.Header.Set(constants.HeaderEndpointAPIUserInfo, base64.RawURLEncoding.EncodeToString(payload))
	response := httptest.NewRecorder()
	Middleware(Config{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, ok := UserFromContext(r.Context())
		if !ok || email != "Alice@example.com" {
			t.Fatalf("context email = %q, ok = %v", email, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestMiddlewareDevHeader(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/apis/gcp.managed.openshift.io/v1/namespaces/project-a/clusters", nil)
	req.Header.Set("X-Dev-User", "Alice@EXAMPLE.COM")
	response := httptest.NewRecorder()
	Middleware(Config{AllowDevHeader: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, ok := UserFromContext(r.Context())
		if !ok || email != "Alice@example.com" {
			t.Fatalf("context email = %q, ok = %v", email, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestMiddlewareAllowsMetadataWithoutIdentity(t *testing.T) {
	for _, url := range []string{"/healthz", "/apis", "/apis/gcp.managed.openshift.io/v1", "/openapi/v3"} {
		t.Run(url, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			response := httptest.NewRecorder()
			Middleware(Config{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, req)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
			}
		})
	}
}
