package v1

import (
	"fmt"

	"github.com/openshift-online/gecko/platform-api/pkg/rolebindingconditions"
)

// CompileRoleBindingConditions validates and compiles the complete requirements.
func CompileRoleBindingConditions(requirements []RoleBindingRequirement) (*string, error) {
	// Each requirement contributes emitted text; avoid copying an impossible input.
	if len(requirements) > rolebindingconditions.MaxExpressionCharacters {
		return nil, fmt.Errorf("generated condition exceeds %d characters", rolebindingconditions.MaxExpressionCharacters)
	}
	input := make([]rolebindingconditions.Requirement, len(requirements))
	for i, requirement := range requirements {
		input[i] = rolebindingconditions.Requirement{Key: string(requirement.Key), Operator: string(requirement.Operator), Values: requirement.Values}
	}
	return rolebindingconditions.Compile(input)
}
