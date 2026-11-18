package schema_test

import (
	"strings"
	"testing"
)

// TestProcess_OldSelf_IntegerFloat64Normalization covers the case where obj
// and oldObj come from encoding/json round-trips, so every number is a
// float64. Integer-typed fields must be normalized to int64 before CEL
// evaluation; otherwise transition rules like `self == oldSelf` fail with
// "invalid data, expected int, got float64" even when the value is unchanged.
func TestProcess_OldSelf_IntegerFloat64Normalization(t *testing.T) {
	const schemaYAML = `
type: object
properties:
  apiVersion:
    type: string
  kind:
    type: string
  metadata:
    type: object
  spec:
    type: object
    properties:
      entries:
        type: array
        x-kubernetes-list-type: atomic
        items:
          type: object
          properties:
            cidr:
              type: string
            hostPrefix:
              type: integer
              format: int32
          required:
            - cidr
        x-kubernetes-validations:
          - rule: "self == oldSelf"
            message: "entries are immutable and cannot be modified once set."
`

	processor := newProcessor(t, schemaYAML)

	// Simulate the JSON round-trip: encoding/json decodes 23 as float64.
	obj := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"spec": map[string]any{
			"entries": []any{
				map[string]any{"cidr": "10.132.0.0/14", "hostPrefix": float64(23)},
			},
		},
	}
	oldObj := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"spec": map[string]any{
			"entries": []any{
				map[string]any{"cidr": "10.132.0.0/14", "hostPrefix": float64(23)},
			},
		},
	}

	if errs := processor.Process(t.Context(), obj, oldObj); len(errs) > 0 {
		t.Fatalf("unchanged update with float64 integers rejected: %v", errs.ToAggregate())
	}

	// The immutability rule still fires on a real change.
	mutated := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"spec": map[string]any{
			"entries": []any{
				map[string]any{"cidr": "10.132.0.0/14", "hostPrefix": float64(24)},
			},
		},
	}
	errs := processor.Process(t.Context(), mutated, oldObj)
	if len(errs) == 0 {
		t.Fatal("mutating an immutable list with float64 integers was accepted")
	}
	if !strings.Contains(errs.ToAggregate().Error(), "entries are immutable") {
		t.Fatalf("expected immutability error, got: %v", errs.ToAggregate())
	}
}

// TestProcess_OldSelf_IntegerFloat64MismatchedTypes verifies normalization
// when one side already carries int64 (e.g. from a typed object or
// defaulting) and the other float64 (from a decoded request body).
func TestProcess_OldSelf_IntegerFloat64MismatchedTypes(t *testing.T) {
	const schemaYAML = `
type: object
properties:
  spec:
    type: object
    properties:
      hostPrefix:
        type: integer
        format: int32
        x-kubernetes-validations:
          - rule: "self == oldSelf"
            message: "hostPrefix is immutable"
`

	processor := newProcessor(t, schemaYAML)

	obj := map[string]any{
		"spec": map[string]any{
			"hostPrefix": float64(23),
		},
	}
	oldObj := map[string]any{
		"spec": map[string]any{
			"hostPrefix": int64(23),
		},
	}

	if errs := processor.Process(t.Context(), obj, oldObj); len(errs) > 0 {
		t.Fatalf("mixed int64/float64 representation rejected: %v", errs.ToAggregate())
	}
}

// TestNormalizeIntegers_Int64Boundaries pins the conversion boundaries.
// float64(math.MaxInt64) rounds up to 2^63, so 2^63 itself is outside int64
// range and must be left as float64 (an int64 conversion would be
// implementation-dependent). The preceding representable float64 and -2^63
// are in range and must convert.
func TestNormalizeIntegers_Int64Boundaries(t *testing.T) {
	const schemaYAML = `
type: object
properties:
  spec:
    type: object
    properties:
      hostPrefix:
        type: integer
        format: int32
`

	processor := newProcessor(t, schemaYAML)

	for _, tc := range []struct {
		name    string
		value   float64
		wantInt bool
	}{
		{"two_pow_63_out_of_range", 9223372036854775808.0, false},      // 2^63
		{"preceding_representable_float", 9223372036854774784.0, true}, // 2^63 - 1024
		{"negative_two_pow_63_in_range", -9223372036854775808.0, true}, // -2^63
		{"zero", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := map[string]any{"spec": map[string]any{"hostPrefix": tc.value}}
			processor.Process(t.Context(), obj, nil)
			got := obj["spec"].(map[string]any)["hostPrefix"]
			if _, ok := got.(int64); ok != tc.wantInt {
				t.Fatalf("value %v: got %T, want int64=%v", tc.value, got, tc.wantInt)
			}
		})
	}
}
