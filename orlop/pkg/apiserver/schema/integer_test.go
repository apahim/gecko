package schema_test

import (
	"reflect"
	"testing"

	kjson "k8s.io/apimachinery/pkg/util/json"
)

const integerTransitionSchema = `
type: object
properties:
  spec:
    type: object
    properties:
      clusterNetwork:
        type: array
        items:
          type: object
          properties:
            hostPrefix:
              type: integer
              format: int32
              minimum: 0
              maximum: 32
              default: 23
        x-kubernetes-validations:
          - rule: "self == oldSelf"
            message: "network must remain immutable"
      wide:
        type: integer
        format: int64
        x-kubernetes-validations:
          - rule: "self >= 0"
            message: "wide must be nonnegative"
          - rule: "self == oldSelf"
            message: "wide must remain immutable"
      ratio:
        type: number
        x-kubernetes-validations:
          - rule: "self > 0.0"
            message: "ratio must be positive"
`

func TestProcessIntegerTransitionJSON(t *testing.T) {
	processor := newProcessor(t, integerTransitionSchema)
	decode := func(data string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := kjson.Unmarshal([]byte(data), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	original := `{"spec":{"clusterNetwork":[{"hostPrefix":23}],"wide":9223372036854775807,"ratio":1.25}}`
	old := decode(original)
	snapshot := decode(original)
	current := decode(original)
	if errs := processor.Process(t.Context(), current, nil); len(errs) > 0 {
		t.Fatal(errs)
	}
	if errs := processor.Process(t.Context(), current, old); len(errs) > 0 {
		t.Fatalf("unchanged integer array rejected: %v", errs)
	}
	if !reflect.DeepEqual(old, snapshot) {
		t.Fatal("validation mutated old input")
	}
	current["spec"].(map[string]any)["clusterNetwork"].([]any)[0].(map[string]any)["hostPrefix"] = int64(24)
	if errs := processor.Process(t.Context(), current, old); len(errs) == 0 {
		t.Fatal("immutable integer change accepted")
	}
	if old["spec"].(map[string]any)["wide"] != int64(9223372036854775807) {
		t.Fatal("large exact integer was rounded")
	}
	adjacentOld := decode(`{"spec":{"wide":9007199254740992}}`)
	adjacentNew := decode(`{"spec":{"wide":9007199254740993}}`)
	if errs := processor.Process(t.Context(), adjacentNew, adjacentOld); len(errs) == 0 {
		t.Fatal("adjacent large integers were treated as equal")
	}
	// Defaults must use the same canonical numeric representation as requests.
	defaulted := decode(`{"spec":{"clusterNetwork":[{}],"wide":9007199254740993,"ratio":2}}`)
	explicitOld := decode(`{"spec":{"clusterNetwork":[{"hostPrefix":23}],"wide":9007199254740993,"ratio":2}}`)
	if errs := processor.Process(t.Context(), defaulted, nil); len(errs) > 0 {
		t.Fatal(errs)
	}
	if errs := processor.Process(t.Context(), defaulted, explicitOld); len(errs) > 0 {
		t.Fatalf("defaulted integer transition rejected: %v", errs)
	}
}

func TestProcessRejectsInvalidIntegerJSON(t *testing.T) {
	processor := newProcessor(t, integerTransitionSchema)
	for _, prefix := range []string{`23.5`, `"23"`, `2147483648`, `-1`, `33`, `9223372036854775808`, `-9223372036854775809`} {
		t.Run(prefix, func(t *testing.T) {
			var current map[string]any
			if err := kjson.Unmarshal([]byte(`{"spec":{"clusterNetwork":[{"hostPrefix":`+prefix+`}]}}`), &current); err != nil {
				t.Fatal(err)
			}
			var old map[string]any
			if err := kjson.Unmarshal([]byte(`{"spec":{"clusterNetwork":[{"hostPrefix":23}]}}`), &old); err != nil {
				t.Fatal(err)
			}
			if errs := processor.Process(t.Context(), current, old); len(errs) == 0 {
				t.Fatal("invalid integer accepted")
			}
		})
	}
}
