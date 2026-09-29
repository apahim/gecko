package apiserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"
)

func TestNotImplementedHandler_Returns501(t *testing.T) {
	for _, verb := range []string{"create", "get", "list", "update", "patch", "delete"} {
		t.Run(verb, func(t *testing.T) {
			h := notImplementedHandler(verb)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			h(w, req)
			if w.Code != http.StatusNotImplemented {
				t.Errorf("expected 501, got %d", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, verb) {
				t.Errorf("expected verb %q in response body, got: %s", verb, body)
			}
		})
	}
}

func TestRegisterVerbsForResource_VerbAllowedCheck(t *testing.T) {
	// Verify VerbAllowed logic is consistent for the routing decision.
	res := types.ResourceInfo{Verbs: []string{"list", "get"}}
	for _, tc := range []struct {
		verb    string
		allowed bool
	}{
		{"list", true},
		{"get", true},
		{"create", false},
		{"update", false},
		{"patch", false},
		{"delete", false},
		{"watch", false},
	} {
		got := res.VerbAllowed(tc.verb)
		if got != tc.allowed {
			t.Errorf("VerbAllowed(%q) = %v, want %v", tc.verb, got, tc.allowed)
		}
	}
}

func TestRegisterVerbsForResource_NoRestriction_AllVerbsAllowed(t *testing.T) {
	res := types.ResourceInfo{} // nil Verbs
	for _, verb := range []string{"create", "get", "list", "update", "patch", "delete", "watch"} {
		if !res.VerbAllowed(verb) {
			t.Errorf("VerbAllowed(%q) = false; want true when Verbs is nil", verb)
		}
	}
}
