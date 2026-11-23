package garaccess

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestMatchesGrantRejectsMalformedDesiredFields(t *testing.T) {
	for _, path := range [][]string{
		{"spec", "member"}, {"spec", "role"},
		{"spec", "resourceRef", "apiVersion"}, {"spec", "resourceRef", "kind"},
		{"spec", "resourceRef", "external"},
	} {
		t.Run(strings.Join(path, "."), func(t *testing.T) {
			for _, test := range []struct {
				name  string
				value interface{}
			}{
				{"non-string", int64(123)},
				{"missing", nil},
			} {
				t.Run(test.name, func(t *testing.T) {
					existing := controllerGrant(controllerConfig(), controllerCluster("customer-project", "cluster", "123"))
					desired := existing.DeepCopy()
					if test.value == nil {
						unstructured.RemoveNestedField(desired.Object, path...)
					} else {
						require.NoError(t, unstructured.SetNestedField(desired.Object, test.value, path...))
					}
					matches, err := matchesGrant(existing, desired)
					require.False(t, matches)
					require.ErrorContains(t, err, strings.Join(path, "."))
					require.ErrorContains(t, err, desired.GetName())
				})
			}
		})
	}
}

func TestMatchesGrantReportsMalformedExistingFields(t *testing.T) {
	for _, path := range [][]string{
		{"spec", "member"}, {"spec", "role"},
		{"spec", "resourceRef", "apiVersion"}, {"spec", "resourceRef", "kind"},
		{"spec", "resourceRef", "external"},
	} {
		t.Run(strings.Join(path, "."), func(t *testing.T) {
			existing := controllerGrant(controllerConfig(), controllerCluster("customer-project", "cluster", "123"))
			desired := existing.DeepCopy()
			require.NoError(t, unstructured.SetNestedField(existing.Object, int64(123), path...))

			matches, err := matchesGrant(existing, desired)
			require.False(t, matches)
			require.ErrorContains(t, err, strings.Join(path, "."))
			require.ErrorContains(t, err, existing.GetName())
		})
	}
}
