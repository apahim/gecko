package v1

import (
	"context"
	"strings"
	"testing"
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
		{"empty alternatives", []RoleBindingRequirement{{"name", "NotIn", []string{}}}, true},
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
