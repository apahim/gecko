package main

import (
	"testing"
)

func TestGetPublicResourcesCatalogVerbs(t *testing.T) {
	wantAuthorizationExempt := map[string]bool{
		"Channel":           true,
		"PlatformRole":      true,
		"Version":           true,
		"WIFConfigTemplate": false,
	}
	seen := make(map[string]bool)
	for _, resource := range getPublicResources() {
		if exempt, found := wantAuthorizationExempt[resource.GVK.Kind]; found {
			seen[resource.GVK.Kind] = true
			if resource.AuthorizationExemptVerbAllowed("get") != exempt || resource.AuthorizationExemptVerbAllowed("list") != exempt {
				t.Errorf("%s authorization exemption for get/list = %t/%t, want %t", resource.GVK.Kind, resource.AuthorizationExemptVerbAllowed("get"), resource.AuthorizationExemptVerbAllowed("list"), exempt)
			}
			if resource.Namespaced {
				t.Errorf("%s is namespaced, want cluster-scoped", resource.GVK.Kind)
			}
			if !resource.VerbAllowed("get") || !resource.VerbAllowed("list") {
				t.Errorf("%s must allow get and list verbs", resource.GVK.Kind)
			}
			for _, verb := range []string{"create", "update", "patch", "delete", "watch"} {
				if resource.VerbAllowed(verb) {
					t.Errorf("%s unexpectedly allows %s", resource.GVK.Kind, verb)
				}
			}
			continue
		}
		if len(resource.AuthorizationExemptVerbs) != 0 {
			t.Errorf("%s unexpectedly allows authorization-exempt verbs: %v", resource.GVK.Kind, resource.AuthorizationExemptVerbs)
		}
	}
	for kind := range wantAuthorizationExempt {
		if !seen[kind] {
			t.Errorf("public resource %s not found", kind)
		}
	}
}
