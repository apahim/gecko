package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"sigs.k8s.io/yaml"
)

const wifChartPath = "../../../helm/charts/platform-api-server"

// helmTemplateOutput renders one template of a chart with base settings that
// satisfy chart validation, returning Helm's exact bytes.
func helmTemplateOutput(t *testing.T, chart string, sets []string, showOnly string) string {
	t.Helper()
	helm := os.Getenv("HELM_BIN")
	if helm == "" {
		t.Skip("set HELM_BIN to run the local Helm packaging integration check")
	}
	args := []string{"template", "wif-phase1", chart,
		"--set", "gateway.enabled=false",
		"--set", "esp.enabled=false",
		"--set", "publicApi.bindAddress=0.0.0.0",
	}
	for _, set := range sets {
		args = append(args, "--set", set)
	}
	args = append(args, "--show-only", showOnly)
	cmd := exec.CommandContext(t.Context(), helm, args...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Helm render: %v: %s", err, data)
	}
	return string(data)
}

// Explicit local packaging integration check; no network or cluster credentials.
func TestWIFConfigTemplateHelmRendering(t *testing.T) {
	helm := os.Getenv("HELM_BIN")
	if helm == "" {
		t.Skip("set HELM_BIN to run the local Helm packaging integration check")
	}
	for _, verified := range []string{"false", "true"} {
		cmd := exec.CommandContext(t.Context(), helm, "template", "wif-phase1", wifChartPath, "--set", "gateway.enabled=false", "--set", "esp.enabled=false", "--set", "publicApi.bindAddress=0.0.0.0", "--set", "wifConfigTemplates.enabled=true", "--set", "wifConfigTemplates.entries[0].name=5.0.0", "--set", "wifConfigTemplates.entries[0].verified="+verified, "--show-only", "templates/wifconfigtemplates.yaml")
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Helm render: %v: %s", err, data)
		}
		var template privatev1.WIFConfigTemplate
		if err := yaml.UnmarshalStrict(data, &template); err != nil {
			t.Fatal(err)
		}
		if err := template.ValidateCreate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if template.Name != "5.0.0" || privatev1.IsWIFConfigVerified(template.Spec) != (verified == "true") {
			t.Fatal("rendered name/verification does not match explicit GitOps intent")
		}
		digest, err := privatev1.WIFConfigDigest(template.Spec)
		if err != nil {
			t.Fatal(err)
		}
		if digest != "sha256:63975f7b264368634bc10a4bff7474142492e833257578a42c912d57336f2971" {
			t.Fatal("rendered IAM permissions/trust changed")
		}
		for _, guard := range []string{"Prune=false", "Delete=false", "SkipDryRunOnMissingResource=true"} {
			if !strings.Contains(template.Annotations["argocd.argoproj.io/sync-options"], guard) {
				t.Fatalf("missing catalog retention/startup guard: %s", guard)
			}
		}
	}
}

// The per-boundary packaging must be render-neutral for the existing single
// baseline entry: the default contract file selection reproduces the reviewed
// pre-per-boundary output byte for byte.
func TestWIFConfigTemplateHelmRenderingBaselineGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/wifconfigtemplate-baseline-golden.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defaultEntry := helmTemplateOutput(t, wifChartPath, []string{
		"wifConfigTemplates.enabled=true",
		"wifConfigTemplates.entries[0].name=5.0.0",
		"wifConfigTemplates.entries[0].verified=false",
	}, "templates/wifconfigtemplates.yaml")
	if defaultEntry != string(want) {
		t.Fatal("default single-baseline rendering drifted from the reviewed golden output")
	}
	explicitEntry := helmTemplateOutput(t, wifChartPath, []string{
		"wifConfigTemplates.enabled=true",
		"wifConfigTemplates.entries[0].name=5.0.0",
		"wifConfigTemplates.entries[0].verified=false",
		"wifConfigTemplates.entries[0].contractFile=files/wifconfigtemplate-project.yaml",
	}, "templates/wifconfigtemplates.yaml")
	if explicitEntry != string(want) {
		t.Fatal("explicit contract file rendering differs from the default baseline rendering")
	}
}

// Multiple boundaries can be published from the shared immutable baseline.
func TestWIFConfigTemplateHelmRenderingMultiBoundary(t *testing.T) {
	output := helmTemplateOutput(t, wifChartPath, []string{
		"wifConfigTemplates.enabled=true",
		"wifConfigTemplates.entries[0].name=5.0.0",
		"wifConfigTemplates.entries[0].verified=true",
		"wifConfigTemplates.entries[1].name=5.0.3",
		"wifConfigTemplates.entries[1].verified=false",
	}, "templates/wifconfigtemplates.yaml")
	docs := strings.Split(strings.TrimPrefix(output, "---"), "---")
	if len(docs) != 2 {
		t.Fatalf("rendered %d documents, want 2", len(docs))
	}
	wantDigest := "sha256:63975f7b264368634bc10a4bff7474142492e833257578a42c912d57336f2971"
	for i, doc := range docs {
		var template privatev1.WIFConfigTemplate
		if err := yaml.UnmarshalStrict([]byte(doc), &template); err != nil {
			t.Fatalf("doc %d: %v", i, err)
		}
		if err := template.ValidateCreate(t.Context()); err != nil {
			t.Fatalf("doc %d: %v", i, err)
		}
		digest, err := privatev1.WIFConfigDigest(template.Spec)
		if err != nil {
			t.Fatal(err)
		}
		if digest != wantDigest {
			t.Fatalf("doc %d digest %s changed", i, digest)
		}
	}
	first, second := &privatev1.WIFConfigTemplate{}, &privatev1.WIFConfigTemplate{}
	if err := yaml.UnmarshalStrict([]byte(docs[0]), first); err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict([]byte(docs[1]), second); err != nil {
		t.Fatal(err)
	}
	if first.Name != "5.0.0" || !privatev1.IsWIFConfigVerified(first.Spec) {
		t.Fatalf("first entry rendered as %q verified=%t", first.Name, privatev1.IsWIFConfigVerified(first.Spec))
	}
	if second.Name != "5.0.3" || privatev1.IsWIFConfigVerified(second.Spec) {
		t.Fatalf("second entry rendered as %q verified=%t", second.Name, privatev1.IsWIFConfigVerified(second.Spec))
	}
}

// A genuinely different future contract needs its own retained file; entries
// select their contract file independently.
func TestWIFConfigTemplateHelmRenderingPerBoundaryFile(t *testing.T) {
	if os.Getenv("HELM_BIN") == "" {
		t.Skip("set HELM_BIN to run the local Helm packaging integration check")
	}
	baseline, err := os.ReadFile("../../../helm/charts/platform-api-server/files/wifconfigtemplate-project.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Same contract topology with one extra non-impersonation permission: a
	// distinct, still-valid schemaVersion 1 contract with a different digest.
	varied := strings.Replace(string(baseline), "        - compute.instances.setTags\n", "        - compute.instances.setTags\n        - compute.instances.stop\n", 1)
	if varied == string(baseline) {
		t.Fatal("failed to vary the second contract file")
	}
	chart := t.TempDir() + "/platform-api-server"
	if out, err := exec.CommandContext(t.Context(), "cp", "-R", wifChartPath, chart).CombinedOutput(); err != nil {
		t.Fatalf("copy chart: %v: %s", err, out)
	}
	if err := os.WriteFile(chart+"/files/wifconfigtemplate-project-503.yaml", []byte(varied), 0o644); err != nil {
		t.Fatal(err)
	}
	output := helmTemplateOutput(t, chart, []string{
		"wifConfigTemplates.enabled=true",
		"wifConfigTemplates.entries[0].name=5.0.0",
		"wifConfigTemplates.entries[0].verified=true",
		"wifConfigTemplates.entries[1].name=5.0.3",
		"wifConfigTemplates.entries[1].verified=true",
		"wifConfigTemplates.entries[1].contractFile=files/wifconfigtemplate-project-503.yaml",
	}, "templates/wifconfigtemplates.yaml")
	docs := strings.Split(strings.TrimPrefix(output, "---"), "---")
	if len(docs) != 2 {
		t.Fatalf("rendered %d documents, want 2", len(docs))
	}
	digests := make([]string, 2)
	for i, doc := range docs {
		var template privatev1.WIFConfigTemplate
		if err := yaml.UnmarshalStrict([]byte(doc), &template); err != nil {
			t.Fatalf("doc %d: %v", i, err)
		}
		if err := template.ValidateCreate(t.Context()); err != nil {
			t.Fatalf("doc %d: %v", i, err)
		}
		digest, err := privatev1.WIFConfigDigest(template.Spec)
		if err != nil {
			t.Fatal(err)
		}
		digests[i] = digest
	}
	if digests[0] != "sha256:63975f7b264368634bc10a4bff7474142492e833257578a42c912d57336f2971" {
		t.Fatalf("baseline entry digest changed: %s", digests[0])
	}
	if digests[0] == digests[1] {
		t.Fatal("per-boundary contract files did not produce distinct contracts")
	}
}
