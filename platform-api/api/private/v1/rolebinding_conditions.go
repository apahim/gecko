package v1

import (
	"github.com/openshift-online/gecko/platform-api/pkg/rolebindingconditions"
)

// CompileRoleBindingConditions validates and compiles the complete requirements.
func CompileRoleBindingConditions(requirements []RoleBindingRequirement) (*string, error) {
	input := make([]rolebindingconditions.Requirement, len(requirements))
	for i, requirement := range requirements {
		input[i] = rolebindingconditions.Requirement{Key: string(requirement.Key), Operator: string(requirement.Operator), Values: requirement.Values}
	}
	return rolebindingconditions.Compile(input)
}
