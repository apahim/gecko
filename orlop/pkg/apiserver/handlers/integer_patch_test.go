package handlers

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMergePatchPreservesIntegerPrecision(t *testing.T) {
	for _, number := range []string{"23", "9007199254740993", "9223372036854775807", "-9223372036854775808"} {
		t.Run(number, func(t *testing.T) {
			original := []byte(`{"spec":{"nested":[{"integer":` + number + `,"fraction":1.25}]}}`)
			for _, patch := range [][]byte{[]byte(`{"metadata":{"labels":{"test":"unchanged"}}}`), []byte(`{"spec":{"nested":[{"integer":` + number + `,"fraction":1.25}]}}`)} {
				merged, err := jsonMergePatch(original, patch)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				decoder := json.NewDecoder(bytes.NewReader(merged))
				decoder.UseNumber()
				if err := decoder.Decode(&got); err != nil {
					t.Fatal(err)
				}
				entry := got["spec"].(map[string]any)["nested"].([]any)[0].(map[string]any)
				if entry["integer"].(json.Number).String() != number || entry["fraction"].(json.Number).String() != "1.25" {
					t.Fatalf("patch rounded or truncated numbers: %s", merged)
				}
			}
		})
	}
}

func TestDecodeObjectMapNumbers(t *testing.T) {
	var got map[string]any
	if err := decodeObjectMap(strings.NewReader(`{"nested":[{"small":23,"wide":9007199254740993,"max":9223372036854775807,"min":-9223372036854775808,"fraction":1.25,"text":"23","null":null}]}`), &got); err != nil {
		t.Fatal(err)
	}
	entry := got["nested"].([]any)[0].(map[string]any)
	for key, want := range map[string]any{"small": int64(23), "wide": int64(9007199254740993), "max": int64(9223372036854775807), "min": int64(-9223372036854775808), "fraction": float64(1.25), "text": "23", "null": nil} {
		if entry[key] != want {
			t.Fatalf("%s: got %T %v, want %T %v", key, entry[key], entry[key], want, want)
		}
	}
	for _, input := range []string{`{"value":1e400}`, `{"value":`, ``} {
		if err := decodeObjectMap(strings.NewReader(input), &got); err == nil {
			t.Fatalf("invalid JSON accepted: %s", input)
		}
	}
	if err := decodeObjectMap(strings.NewReader(`null`), &got); err != nil || got != nil {
		t.Fatalf("null semantics changed: %v %#v", err, got)
	}
	if err := decodeObjectMap(strings.NewReader(`{"value":23} {"ignored":true}`), &got); err != nil || got["value"] != int64(23) {
		t.Fatalf("single-value streaming semantics changed: %v %#v", err, got)
	}
}
