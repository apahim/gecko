package schema

import (
	"math"

	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
)

// normalizeIntegers rewrites integral float64 values in integer-typed fields
// to int64, in place, following the structural schema.
//
// encoding/json decodes every JSON number into float64 when unmarshaling into
// map[string]interface{}. The CEL validator, however, refuses to convert a
// float64 to a CEL int for a schema field declared as integer ("invalid data,
// expected int, got float64"). Any CEL transition rule (e.g. an immutability
// guard `self == oldSelf`) over an object containing an integer field then
// fails even when the value is unchanged. Upstream Kubernetes never hits this
// because its unstructured values carry int64 for integers; orlop builds
// object maps with encoding/json, so the processor must normalize them.
//
// Non-integral floats are left untouched: the JSON-schema validation rejects
// them as non-integer values. Fields with x-kubernetes-int-or-string or
// x-kubernetes-preserve-unknown-fields are skipped, as are values already
// carrying a Go integer type.
func normalizeIntegers(s *structuralschema.Structural, obj interface{}) {
	if s == nil || obj == nil {
		return
	}
	if s.XIntOrString || s.XPreserveUnknownFields {
		return
	}
	switch s.Type {
	case "object":
		m, ok := obj.(map[string]interface{})
		if !ok {
			return
		}
		for name, prop := range s.Properties {
			if child, ok := m[name]; ok {
				m[name] = normalizeChild(&prop, child)
			}
		}
		if s.AdditionalProperties != nil && s.AdditionalProperties.Structural != nil {
			for k, v := range m {
				if _, known := s.Properties[k]; known {
					continue
				}
				m[k] = normalizeChild(s.AdditionalProperties.Structural, v)
			}
		}
	case "array":
		l, ok := obj.([]interface{})
		if !ok || s.Items == nil {
			return
		}
		for i, item := range l {
			l[i] = normalizeChild(s.Items, item)
		}
	}
}

// normalizeChild normalizes one child value against its schema: integer-typed
// integral float64 values become int64; container values are descended into.
func normalizeChild(s *structuralschema.Structural, v interface{}) interface{} {
	if s == nil || v == nil || s.XIntOrString || s.XPreserveUnknownFields {
		return v
	}
	if s.Type == "integer" {
		if f, ok := v.(float64); ok && isInt64(f) {
			return int64(f)
		}
		return v
	}
	normalizeIntegers(s, v)
	return v
}

func isInt64(v float64) bool {
	// The upper bound is exclusive: float64(math.MaxInt64) rounds up to 2^63,
	// so an inclusive <= would admit 2^63, whose int64 conversion is
	// implementation-dependent. -2^63 is exactly representable and in range.
	return v == math.Trunc(v) && v >= math.MinInt64 && v < 0x1p63
}
