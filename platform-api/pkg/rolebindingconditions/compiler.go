// Package rolebindingconditions compiles structured RoleBinding requirements to
// Cedar predicates. Values are opaque client strings, not resource identifiers.
package rolebindingconditions

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cedar-policy/cedar-go/types"
)

// MaxExpressionCharacters is the storage bound on the complete generated predicate.
const MaxExpressionCharacters = 4096

// Requirement is a compiler input, not a separately served API resource.
// Requirements are ANDed, including requirements with repeated keys.
//
//nolint:kubeapilinter,geckoconventions // Internal compiler DTO, not a served API type; only the complete emitted expression is size-bounded.
type Requirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

// Compile validates structural requirements and emits a deterministic predicate.
// Empty input is unconditioned and returns nil. Values are not checked for
// resource-name/region validity or uniqueness. Empty/whitespace-only values
// are rejected; nonblank values are preserved exactly. A nil or empty
// Values slice is rejected; every requirement needs at least one value.
// '*' is the only wildcard. All other characters are literal, including '\'.
func Compile(requirements []Requirement) (*string, error) {
	if len(requirements) == 0 {
		return nil, nil
	}
	// Every requirement/value and every normalized pattern rune consumes at least one
	// emitted character. Reject impossible inputs before allocating output slices
	// or splitting patterns. These are derived bounds, not new API limits.
	if err := checkInputBudget(requirements); err != nil {
		return nil, err
	}
	expressions := make([]string, 0, len(requirements))
	for i, requirement := range requirements {
		expression, err := compileRequirement(requirement)
		if err != nil {
			return nil, fmt.Errorf("conditions[%d]: %w", i, err)
		}
		expressions = append(expressions, expression)
	}
	expression := strings.Join(expressions, " && ")
	length := utf8.RuneCountInString(expression)
	if length > MaxExpressionCharacters {
		return nil, fmt.Errorf("generated condition has %d characters; limit is %d", length, MaxExpressionCharacters)
	}
	return &expression, nil
}

func compileRequirement(requirement Requirement) (string, error) {
	var attribute string
	switch requirement.Key {
	case "name":
		attribute = "resourceName"
	case "region":
		attribute = "resourceRegion"
	default:
		return "", fmt.Errorf("key must be name or region")
	}
	if requirement.Operator != "In" && requirement.Operator != "NotIn" {
		return "", fmt.Errorf("operator must be In or NotIn")
	}
	if len(requirement.Values) == 0 {
		return "", fmt.Errorf("values must contain at least one string")
	}
	matches := make([]string, 0, len(requirement.Values))
	for _, value := range requirement.Values {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("values must not contain empty or whitespace-only strings")
		}
		if !utf8.ValidString(value) {
			return "", fmt.Errorf("values must contain valid UTF-8 strings")
		}
		if !strings.Contains(value, "*") {
			matches = append(matches, "context."+attribute+" == "+string(types.String(value).MarshalCedar()))
			continue
		}
		parts := strings.Split(collapseStars(value), "*")
		components := make([]any, 0, 2*len(parts)-1)
		for i, literal := range parts {
			if i != 0 {
				components = append(components, types.Wildcard{})
			}
			components = append(components, literal)
		}
		pattern := types.NewPattern(components...)
		matches = append(matches, "context."+attribute+" like "+string(pattern.MarshalCedar()))
	}
	disjunction := "false"
	if len(matches) != 0 {
		disjunction = strings.Join(matches, " || ")
	}
	if requirement.Operator == "NotIn" {
		disjunction = "!(" + disjunction + ")"
	} else {
		disjunction = "(" + disjunction + ")"
	}
	return "(context has " + attribute + " && " + disjunction + ")", nil
}

// checkInputBudget bounds compiler allocations even for untrusted stored input.
// It stops scanning once the lower bound exceeds the final expression limit.
func checkInputBudget(requirements []Requirement) error {
	budget := MaxExpressionCharacters
	consume := func(n int) bool {
		if n > budget {
			return false
		}
		budget -= n
		return true
	}
	exceeded := func() error {
		return fmt.Errorf("generated condition exceeds %d characters (input lower bound)", MaxExpressionCharacters)
	}
	if !consume(len(requirements)) {
		return exceeded()
	}
	for _, requirement := range requirements {
		if !consume(len(requirement.Values)) {
			return exceeded()
		}
		for _, value := range requirement.Values {
			previousStar := false
			for _, r := range value {
				if r == '*' && previousStar {
					continue
				}
				previousStar = r == '*'
				if !consume(1) {
					return exceeded()
				}
			}
		}
	}
	return nil
}

// collapseStars mirrors Cedar's pattern normalization without allocating one
// split/component entry per redundant star. The preflight bounds the resulting
// text, not raw wildcard-run length.
func collapseStars(value string) string {
	var result strings.Builder
	previousStar := false
	for _, r := range value {
		if r == '*' && previousStar {
			continue
		}
		previousStar = r == '*'
		result.WriteRune(r)
	}
	return result.String()
}
