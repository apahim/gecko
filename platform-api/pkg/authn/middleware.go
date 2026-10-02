package authn

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/handlers"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"
)

// Config controls public API identity extraction.
type Config struct {
	// AllowDevHeader enables X-Dev-User for explicitly configured local
	// development. It must not be enabled in production.
	AllowDevHeader bool
}

type userInfoClaims struct {
	Email         string `json:"email"`
	EmailVerified *bool  `json:"email_verified"`
}

// Middleware authenticates public API requests using the identity header
// injected by ESPv2. The header is trusted only because the Helm deployment
// keeps the application listener reachable from the ESPv2 sidecar, not from
// the external network.
func Middleware(config Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isUnauthenticatedMetadataPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			email, err := extractPrincipal(r, config)
			if err != nil {
				writeUnauthorized(w, err.Error())
				return
			}

			ctx := WithUser(r.Context(), email)
			ctx = handlers.WithAuthenticatedUser(ctx, email)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractPrincipal(r *http.Request, config Config) (string, error) {
	if config.AllowDevHeader {
		if raw, present := r.Header["X-Dev-User"]; present {
			if len(raw) == 0 || strings.TrimSpace(raw[0]) == "" {
				return "", fmt.Errorf("X-Dev-User is empty")
			}
			return privatev1.NormalizeEmail(raw[0])
		}
	}

	encoded := r.Header.Get(constants.HeaderEndpointAPIUserInfo)
	if encoded == "" {
		return "", fmt.Errorf("%s header is missing", constants.HeaderEndpointAPIUserInfo)
	}

	decoded, err := decodeBase64URL(encoded)
	if err != nil {
		return "", fmt.Errorf("invalid %s header: %w", constants.HeaderEndpointAPIUserInfo, err)
	}

	var claims userInfoClaims
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return "", fmt.Errorf("invalid user-info JSON: %w", err)
	}
	if claims.Email == "" {
		return "", fmt.Errorf("email claim is missing")
	}
	if claims.EmailVerified == nil || !*claims.EmailVerified {
		return "", fmt.Errorf("email_verified claim is missing or false")
	}

	return privatev1.NormalizeEmail(claims.Email)
}

func decodeBase64URL(encoded string) ([]byte, error) {
	decoders := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
	}
	var lastErr error
	for _, decoder := range decoders {
		decoded, err := decoder.DecodeString(encoded)
		if err == nil {
			return decoded, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Do not echo claims or header values in the error. The message contains
	// only a fixed validation reason.
	_, _ = fmt.Fprintf(w, `{"error":"unauthorized","message":%q}`, message)
}

func isUnauthenticatedMetadataPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/livez", "/apis", "/apis/":
		return true
	}
	if strings.HasPrefix(path, "/openapi/") {
		return true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// /apis/{group} and /apis/{group}/{version} are discovery endpoints.
	return len(parts) == 2 && parts[0] == "apis" || len(parts) == 3 && parts[0] == "apis"
}
