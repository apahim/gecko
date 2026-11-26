package v1

import (
	"context"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func testRoleBinding(requirements []RoleBindingRequirement) *RoleBinding {
	return &RoleBinding{Spec: RoleBindingSpec{Subject: "alice@example.com", RoleRef: RoleRef{Kind: "PlatformRole", Name: "viewer", APIGroup: GroupVersion.Group}, Conditions: requirements}}
}

func TestRoleBindingConditionValidation(t *testing.T) {
	for _, tt := range []struct {
		name         string
		requirements []RoleBindingRequirement
		valid        bool
	}{
		{"omitted", nil, true}, {"mixed", []RoleBindingRequirement{{"region", "In", []string{"TYPO", "europe-*", " literal value "}}, {"name", "NotIn", []string{"*-private"}}}, true},
		{"empty alternatives", []RoleBindingRequirement{{"name", "NotIn", []string{}}}, false},
		{"duplicates", []RoleBindingRequirement{{"name", "In", []string{"x", "x"}}, {"name", "In", []string{"x"}}}, true},
		{"bad key", []RoleBindingRequirement{{"version", "In", []string{"x"}}}, false},
		{"bad operator", []RoleBindingRequirement{{"name", "Like", []string{"x"}}}, false},
		{"missing values", []RoleBindingRequirement{{"name", "In", nil}}, false},
		{"oversize", []RoleBindingRequirement{{"name", "In", []string{strings.Repeat("x", 4096)}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			binding := testRoleBinding(tt.requirements)
			validationErr := binding.ValidateCreate(context.Background())
			if tt.valid && validationErr != nil {
				t.Fatalf("valid candidate: %v", validationErr)
			}
			if !tt.valid && validationErr == nil {
				t.Fatal("validator accepted invalid candidate")
			}
			if tt.valid {
				if err := binding.ValidateUpdate(context.Background(), testRoleBinding(nil)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRoleBindingUpdatePreservesIdentity(t *testing.T) {
	old := testRoleBinding(nil)
	old.Name = "target"
	old.Namespace = "project-a"
	for _, field := range []string{"name", "namespace"} {
		t.Run(field, func(t *testing.T) {
			candidate := old.DeepCopy()
			if field == "name" {
				candidate.Name = "other"
			} else {
				candidate.Namespace = "project-b"
			}
			if err := candidate.ValidateUpdate(t.Context(), old); err == nil {
				t.Fatal("update changed stored identity")
			}
			if old.Name != "target" || old.Namespace != "project-a" {
				t.Fatal("validation mutated old object")
			}
		})
	}
}

func TestRequirementValuesSchemaMinimum(t *testing.T) {
	for _, path := range []string{".schemas/rolebinding_schema.yaml", "../../public/v1/.schemas/rolebinding_schema.yaml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]interface{}
		if err := yaml.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		conditions := schema["properties"].(map[string]interface{})["spec"].(map[string]interface{})["properties"].(map[string]interface{})["conditions"].(map[string]interface{})
		if minimum, ok := conditions["minItems"]; ok && minimum != float64(0) {
			t.Fatalf("top-level conditions must allow empty: %v", minimum)
		}
		node := schema
		for _, key := range []string{"properties", "spec", "properties", "conditions", "items", "properties", "values"} {
			node = node[key].(map[string]interface{})
		}
		if node["minItems"] != float64(1) {
			t.Fatalf("%s: minItems=%v", path, node["minItems"])
		}
	}
}

func TestCompileRoleBindingConditionsRejectsImpossibleRequirementCount(t *testing.T) {
	if expression, err := CompileRoleBindingConditions(make([]RoleBindingRequirement, 4097)); err == nil || expression != nil {
		t.Fatal("impossible requirement count accepted")
	}
}
