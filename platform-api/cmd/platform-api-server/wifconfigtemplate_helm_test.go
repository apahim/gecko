package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"sigs.k8s.io/yaml"
)

// Explicit local packaging integration check; no network or cluster credentials.
func TestWIFConfigTemplateHelmRendering(t *testing.T) {
	helm := os.Getenv("HELM_BIN")
	if helm == "" {
		t.Skip("set HELM_BIN to run the local Helm packaging integration check")
	}
	for _, verified := range []string{"false", "true"} {
		cmd := exec.CommandContext(t.Context(), helm, "template", "wif-phase1", "../../../helm/charts/platform-api-server", "--set", "gateway.enabled=false", "--set", "esp.enabled=false", "--set", "publicApi.bindAddress=0.0.0.0", "--set", "wifConfigTemplates.enabled=true", "--set", "wifConfigTemplates.entries[0].name=5.0.0", "--set", "wifConfigTemplates.entries[0].verified="+verified, "--show-only", "templates/wifconfigtemplates.yaml")
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
		if digest != "sha256:71d9aaf90729b0365610a16bd46d7cb4d4582e1e69b79f114f968086017d1af4" {
			t.Fatal("rendered IAM permissions/trust changed")
		}
		for _, guard := range []string{"Prune=false", "Delete=false", "SkipDryRunOnMissingResource=true"} {
			if !strings.Contains(template.Annotations["argocd.argoproj.io/sync-options"], guard) {
				t.Fatalf("missing catalog retention/startup guard: %s", guard)
			}
		}
	}
}
