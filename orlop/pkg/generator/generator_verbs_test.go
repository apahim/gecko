package generator

import (
	"go/parser"
	"go/token"
	"testing"
)

func parseTestFile(t *testing.T, src string) (*Generator, string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	g := &Generator{
		typeVerbs: make(map[string][]string),
	}
	if err := g.scanTypeVerbs(f, "types.go"); err != nil {
		t.Fatalf("unexpected scanTypeVerbs error: %v", err)
	}
	_ = f
	return g, "types.go"
}

func TestScanTypeVerbs_Absent(t *testing.T) {
	src := `package v1
// +kubebuilder:object:root=true
type Widget struct {}
`
	g, _ := parseTestFile(t, src)
	if v := g.typeVerbs["Widget"]; v != nil {
		t.Errorf("expected nil verbs when annotation absent, got %v", v)
	}
}

func TestScanTypeVerbs_ValidSet(t *testing.T) {
	src := `package v1
// +kubebuilder:object:root=true
// +orlop:public-verbs: create,get,list,delete
type Widget struct {}
`
	g, _ := parseTestFile(t, src)
	want := []string{"create", "get", "list", "delete"}
	got := g.typeVerbs["Widget"]
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("verbs[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestScanTypeVerbs_WithWhitespace(t *testing.T) {
	src := `package v1
// +orlop:public-verbs:  list ,  get
type Widget struct {}
`
	g, _ := parseTestFile(t, src)
	got := g.typeVerbs["Widget"]
	if len(got) != 2 || got[0] != "list" || got[1] != "get" {
		t.Errorf("got %v, want [list get]", got)
	}
}

func TestScanTypeVerbs_Deduplication(t *testing.T) {
	src := `package v1
// +orlop:public-verbs: get,list,get
type Widget struct {}
`
	g, _ := parseTestFile(t, src)
	got := g.typeVerbs["Widget"]
	if len(got) != 2 {
		t.Errorf("expected 2 verbs after dedup, got %d: %v", len(got), got)
	}
}

func TestScanTypeVerbs_UnknownToken(t *testing.T) {
	src := `package v1
// +orlop:public-verbs: get,fly
type Widget struct {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	g := &Generator{typeVerbs: make(map[string][]string)}
	err = g.scanTypeVerbs(f, "types.go")
	if err == nil {
		t.Fatal("expected error for unknown verb token, got nil")
	}
}

func TestScanTypeVerbs_MultipleTypes(t *testing.T) {
	src := `package v1
// +orlop:public-verbs: list,get
type Cluster struct {}

// +orlop:public-verbs: create,get,list,delete
type NodePool struct {}

type Other struct {}
`
	g, _ := parseTestFile(t, src)
	if got := g.typeVerbs["Cluster"]; len(got) != 2 || got[0] != "list" || got[1] != "get" {
		t.Errorf("Cluster verbs: got %v, want [list get]", got)
	}
	if got := g.typeVerbs["NodePool"]; len(got) != 4 {
		t.Errorf("NodePool verbs: got %v, want 4 verbs", got)
	}
	if got := g.typeVerbs["Other"]; got != nil {
		t.Errorf("Other verbs: got %v, want nil", got)
	}
}

func TestScanTypeVerbs_ErrorOnPackageDoc(t *testing.T) {
	src := `// +orlop:public-verbs: list
package v1
type Widget struct {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	g := &Generator{typeVerbs: make(map[string][]string)}
	err = g.scanTypeVerbs(f, "types.go")
	if err == nil {
		t.Fatal("expected error when annotation is on package doc, got nil")
	}
}

func TestScanTypeVerbs_ErrorOnField(t *testing.T) {
	src := `package v1
type Widget struct {
	// +orlop:public-verbs: list
	Name string
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	g := &Generator{typeVerbs: make(map[string][]string)}
	err = g.scanTypeVerbs(f, "types.go")
	if err == nil {
		t.Fatal("expected error when annotation is on a struct field, got nil")
	}
}

func TestParseVerbList_Valid(t *testing.T) {
	verbs, err := parseVerbList("create,get,list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(verbs) != 3 {
		t.Errorf("expected 3 verbs, got %d: %v", len(verbs), verbs)
	}
}

func TestParseVerbList_Unknown(t *testing.T) {
	_, err := parseVerbList("get,teleport")
	if err == nil {
		t.Fatal("expected error for unknown verb")
	}
}
