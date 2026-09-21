package types

import (
	"testing"
)

func TestVerbAllowed_NilVerbs(t *testing.T) {
	ri := ResourceInfo{} // Verbs is nil
	for _, verb := range []string{"create", "get", "list", "update", "patch", "delete", "watch"} {
		if !ri.VerbAllowed(verb) {
			t.Errorf("VerbAllowed(%q) = false; want true when Verbs is nil", verb)
		}
	}
}

func TestVerbAllowed_EmptyVerbs(t *testing.T) {
	ri := ResourceInfo{Verbs: []string{}}
	for _, verb := range []string{"create", "get", "list", "update", "patch", "delete", "watch"} {
		if !ri.VerbAllowed(verb) {
			t.Errorf("VerbAllowed(%q) = false; want true when Verbs is empty", verb)
		}
	}
}

func TestVerbAllowed_RestrictedSet(t *testing.T) {
	ri := ResourceInfo{Verbs: []string{"list", "get"}}

	allowed := []string{"list", "get"}
	for _, verb := range allowed {
		if !ri.VerbAllowed(verb) {
			t.Errorf("VerbAllowed(%q) = false; want true", verb)
		}
	}

	disallowed := []string{"create", "update", "patch", "delete", "watch"}
	for _, verb := range disallowed {
		if ri.VerbAllowed(verb) {
			t.Errorf("VerbAllowed(%q) = true; want false", verb)
		}
	}
}

func TestVerbAllowed_SingleVerb(t *testing.T) {
	ri := ResourceInfo{Verbs: []string{"create"}}
	if !ri.VerbAllowed("create") {
		t.Error("VerbAllowed(\"create\") = false; want true")
	}
	if ri.VerbAllowed("get") {
		t.Error("VerbAllowed(\"get\") = true; want false")
	}
}
