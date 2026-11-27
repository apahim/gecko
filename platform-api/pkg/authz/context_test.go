package authz

import (
	"testing"

	"github.com/cedar-policy/cedar-go"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTrustedLocationContext(t *testing.T) {
	for _, tt := range []struct{ plural, field, location, region, resourceType string }{
		{"clusters", "region", "us-central1", "us-central1", "cluster"},
		{"clusters", "region", "US-central1", "US-central1", "cluster"},
		{"clusters", "region", "us-central1-a", "us-central1-a", "cluster"},
		{"nodepools", "zone", "us-west4-ai2b", "", "nodepool"},
		{"nodepools", "zone", "us-central1-a", "us-central1", "nodepool"},
		{"nodepools", "zone", "us-west4-ai2b-extra", "", "nodepool"},
		{"nodepools", "zone", "US-west4-a", "US-west4", "nodepool"},
		{"nodepools", "zone", "us-west4-aa", "", "nodepool"},
		{"roles", "region", "us-central1", "", "role"},
		{"rolebindings", "region", "us-central1", "", "rolebinding"},
		{"clusters", "region", "", "", "cluster"},
		{"unknowns", "region", "us-central1", "", ""},
	} {
		t.Run(tt.plural+"/"+tt.location, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"metadata": map[string]interface{}{"name": "demo", "generateName": "ignored-"},
				"spec":     map[string]interface{}{"resourceRegion": "spoofed", "resourceType": "spoofed", "platform": map[string]interface{}{"gcp": map[string]interface{}{tt.field: tt.location}}},
			}}
			record, err := CedarContextForObject(obj, tt.plural, "GET")
			if err != nil {
				t.Fatal(err)
			}
			values := record.Map()
			for key, want := range map[cedar.String]string{"resourceRegion": tt.region, "resourceType": tt.resourceType} {
				got, present := values[key]
				if want == "" {
					if present {
						t.Fatalf("unexpected %s: %v", key, got)
					}
				} else if got != cedar.String(want) {
					t.Fatalf("%s got %v, want %q", key, got, want)
				}
			}
			if values["resourcePlural"] != cedar.String(tt.plural) || values["resourceName"] != cedar.String("demo") {
				t.Fatal("legacy context changed")
			}
		})
	}
}

// Admission owns region validation; context construction must not introduce a
// second, potentially divergent region contract.
func TestRegionForStandardZone(t *testing.T) {
	for _, tt := range []struct{ zone, region string }{
		{"us-central1-a", "us-central1"}, {"europe-west2-b", "europe-west2"},
		{"already-validated-prefix-a", "already-validated-prefix"},
		{"us-west4-ai2b", ""}, {"us-west4-aa", ""}, {"", ""}, {"a", ""}, {"-a", ""}, {"us-west4-", ""},
	} {
		t.Run(tt.zone, func(t *testing.T) {
			if got := regionForStandardZone(tt.zone); got != tt.region {
				t.Fatalf("got %q, want %q", got, tt.region)
			}
		})
	}
}
