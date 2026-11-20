package ocpversion

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
)

func TestNormalizeComparisonCopy(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"4.22.21", "4.22.21"}, {"4.23.0", "4.23.0"}, {"5.0.0", "4.23.0"},
		{"5.1.7", "4.24.7"}, {"5.0.0-rc.8+build.1", "4.23.0-rc.8+build.1"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			v := version.MustParseSemantic(tc.raw)
			got, err := Normalize(v)
			if err != nil || got.String() != tc.want || v.String() != tc.raw || v == got {
				t.Fatalf("normalization: %v %v", got, err)
			}
		})
	}
	for _, raw := range []string{"6.0.0", "3.9.0", "secret-invalid"} {
		if _, err := Parse(raw); err == nil || strings.Contains(err.Error(), raw) {
			t.Fatalf("expected sanitized unsupported-version error for %s", raw)
		}
	}
	if _, err := Normalize(nil); err == nil {
		t.Fatal("nil version accepted")
	}
	if _, err := Normalize(version.MustParseSemantic("5.0.0").WithMinor(^uint(0))); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestNormalizedOrderingPreservesPrerelease(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		less bool
	}{
		{"4.22.21", "5.0.0", true}, {"4.23.1", "5.0.0", false},
		{"5.0.0-rc.8", "5.0.0", true}, {"4.23.0", "5.0.0", false},
	} {
		a, err := Parse(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Parse(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if a.LessThan(b) != tc.less {
			t.Fatalf("incorrect order: %s < %s", tc.a, tc.b)
		}
	}
}

func TestObservedAliasesRetainLocksWithoutAuthorizingUpgrade(t *testing.T) {
	for _, tc := range []struct {
		a, b   string
		active bool
	}{
		{"4.23.0", "5.0.0", true}, {"5.0.0", "4.23.0", true},
		{"4.22.21", "5.0.0", true}, {"5.0.1", "4.23.0", false},
		{"5.0.0", "5.0.0", false},
	} {
		got, err := ObservedTransition(tc.a, tc.b)
		if err != nil || got != tc.active {
			t.Fatalf("observed transition %s -> %s: %t %v", tc.a, tc.b, got, err)
		}
	}
	if _, err := ObservedTransition("5.0.0", "6.0.0"); err == nil {
		t.Fatal("unknown observation must remain uncertain")
	}
}
