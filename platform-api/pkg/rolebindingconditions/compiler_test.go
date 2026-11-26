package rolebindingconditions

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cedar-policy/cedar-go"
)

func evaluate(t *testing.T, requirements []Requirement, attributes map[string]string) bool {
	t.Helper()
	expression, err := Compile(requirements)
	if err != nil {
		t.Fatal(err)
	}
	predicate := "true"
	if expression != nil {
		predicate = *expression
	}
	var parsed cedar.PolicyList
	if err := parsed.UnmarshalCedar([]byte(`permit(principal, action, resource) when {` + predicate + `};`)); err != nil {
		t.Fatalf("generated predicate did not parse: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatal("compiler injected a policy")
	}
	policies := cedar.NewPolicySet()
	policies.Add("test", parsed[0])
	context := cedar.RecordMap{}
	for key, value := range attributes {
		context[cedar.String(key)] = cedar.String(value)
	}
	decision, diagnostic := cedar.Authorize(policies, nil, cedar.Request{
		Principal: cedar.NewEntityUID("User", "test"), Action: cedar.NewEntityUID("Action", "get"), Resource: cedar.NewEntityUID("Resource", "target"), Context: cedar.NewRecord(context),
	})
	if len(diagnostic.Errors) != 0 {
		t.Fatalf("evaluation error: %v", diagnostic.Errors)
	}
	return decision == cedar.Allow
}

func TestCompileMatching(t *testing.T) {
	for _, key := range []string{"name", "region"} {
		attribute := "resourceName"
		if key == "region" {
			attribute = "resourceRegion"
		}
		for _, operator := range []string{"In", "NotIn"} {
			for _, tt := range []struct {
				name           string
				values         []string
				actual         string
				present, match bool
			}{
				{"exact", []string{"us-central1"}, "us-central1", true, true},
				{"case sensitive", []string{"us-central1"}, "US-central1", true, false},
				{"mixed exact", []string{"us-central1", "europe-*"}, "us-central1", true, true},
				{"mixed wildcard", []string{"us-central1", "europe-*"}, "europe-west2", true, true},
				{"mixed miss", []string{"us-central1", "europe-*"}, "us-west4", true, false},
				{"zero length wildcard", []string{"demo-*"}, "demo-", true, true},
				{"whole string", []string{"demo-*"}, "other-demo-a", true, false},
				{"multiple wildcard", []string{"d**m*"}, "demo", true, true},
				{"bare wildcard", []string{"*"}, "anything", true, true},
				{"missing", []string{"*"}, "", false, false},
				{"surrounding whitespace preserved", []string{" x "}, " x ", true, true},
				{"surrounding whitespace not trimmed", []string{" x "}, "x", true, false},
				{"duplicates", []string{"x", "x"}, "x", true, true},
				{"opaque spelling", []string{"TYPO ! unicode:日本"}, "TYPO ! unicode:日本", true, true},
				{"literal regex", []string{"[a-z]?"}, "[a-z]?", true, true},
				{"injection exact", []string{"x\" }; forbid(principal,action,resource) when {true"}, "x\" }; forbid(principal,action,resource) when {true", true, true},
				{"injection wildcard", []string{"x*\" }; forbid(principal,action,resource) when {true"}, "xyz\" }; forbid(principal,action,resource) when {true", true, true},
				{"backslash literal before star", []string{`a\*`}, `a\suffix`, true, true},
				{"backslash does not escape star", []string{`a\*`}, `a*suffix`, true, false},
			} {
				t.Run(key+"/"+operator+"/"+tt.name, func(t *testing.T) {
					attributes := map[string]string{}
					if tt.present {
						attributes[attribute] = tt.actual
					}
					want := tt.present && tt.match
					if operator == "NotIn" {
						want = tt.present && !tt.match
					}
					got := evaluate(t, []Requirement{{Key: key, Operator: operator, Values: tt.values}}, attributes)
					if got != want {
						t.Fatalf("got %v, want %v", got, want)
					}
				})
			}
		}
	}
}

func TestCompileRepeatedRequirements(t *testing.T) {
	requirements := []Requirement{{"name", "In", []string{"demo-*"}}, {"name", "NotIn", []string{"*-internal"}}, {"region", "In", []string{"us-central1", "europe-*"}}}
	if !evaluate(t, requirements, map[string]string{"resourceName": "demo-public", "resourceRegion": "europe-west2"}) {
		t.Fatal("valid conjunction denied")
	}
	if evaluate(t, requirements, map[string]string{"resourceName": "demo-internal", "resourceRegion": "europe-west2"}) {
		t.Fatal("exclusion ignored")
	}
	requirements = append(requirements, Requirement{"name", "NotIn", []string{"demo-*"}})
	if evaluate(t, requirements, map[string]string{"resourceName": "demo-public", "resourceRegion": "europe-west2"}) {
		t.Fatal("contradictory requirements allowed")
	}
}

func TestCompileValidationAndLength(t *testing.T) {
	for _, requirements := range [][]Requirement{{{"bad", "In", []string{"x"}}}, {{"name", "Like", []string{"x"}}}, {{"name", "In", nil}}} {
		if _, err := Compile(requirements); err == nil {
			t.Fatal("invalid structure accepted")
		}
	}
	if result, err := Compile(nil); err != nil || result != nil {
		t.Fatal("empty conditions not unconditioned")
	}
	base, err := Compile([]Requirement{{"name", "In", []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	overhead := utf8.RuneCountInString(*base) - 1
	for _, size := range []int{4095, 4096, 4097} {
		expression, err := Compile([]Requirement{{"name", "In", []string{strings.Repeat("x", size-overhead)}}})
		if size <= 4096 {
			if err != nil || utf8.RuneCountInString(*expression) != size {
				t.Fatalf("size %d: %v", size, err)
			}
		} else if err == nil {
			t.Fatal("oversize predicate accepted")
		}
	}
}

func FuzzCompile(f *testing.F) {
	for _, seed := range []string{"demo-*", `a\*`, "日本", `\" }; forbid(principal,action,resource) when {true`, ""} {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, value, actual string) {
		requirements := []Requirement{{"name", "In", []string{value}}}
		first, err := Compile(requirements)
		second, err2 := Compile(requirements)
		if (err == nil) != (err2 == nil) {
			t.Fatal("nondeterministic error")
		}
		if err != nil {
			return
		}
		if *first != *second {
			t.Fatal("nondeterministic output")
		}
		if !utf8.ValidString(actual) {
			return
		}
		oracle, err := regexp.Compile("(?s)^" + strings.ReplaceAll(regexp.QuoteMeta(value), `\*`, ".*") + "$")
		if err != nil {
			t.Fatal(err)
		}
		want := oracle.MatchString(actual)
		attributes := map[string]string{"resourceName": actual}
		if got := evaluate(t, requirements, attributes); got != want {
			t.Fatalf("compiled wildcard result %v, want %v", got, want)
		}
		requirements[0].Operator = "NotIn"
		if got := evaluate(t, requirements, attributes); got == want {
			t.Fatal("NotIn is not complement for present value")
		}
		if evaluate(t, requirements, nil) {
			t.Fatal("missing attribute granted access")
		}
	})
}

func TestCompileRejectsBlankValues(t *testing.T) {
	for _, key := range []string{"name", "region"} {
		for _, operator := range []string{"In", "NotIn"} {
			for _, value := range []string{"", " ", "\t\r\n", "\u00a0\u2003", "\u3000"} {
				for _, values := range [][]string{{value}, {"valid", value}} {
					if _, err := Compile([]Requirement{{key, operator, values}}); err == nil {
						t.Fatalf("accepted blank value %q for %s/%s", value, key, operator)
					}
				}
			}
		}
	}
}

func TestCompileRejectsEmptyValues(t *testing.T) {
	for _, key := range []string{"name", "region"} {
		for _, op := range []string{"In", "NotIn"} {
			for _, values := range [][]string{nil, {}} {
				if expression, err := Compile([]Requirement{{key, op, values}}); err == nil || expression != nil {
					t.Fatalf("accepted %s/%s values=%#v", key, op, values)
				}
			}
		}
	}
	if expression, err := Compile([]Requirement{}); err != nil || expression != nil {
		t.Fatal("top-level empty conditions rejected")
	}
}
