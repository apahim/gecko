package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSpannerMigrationImagePinKeepsJobUnchanged(t *testing.T) {
	helm := os.Getenv("HELM_BIN")
	if helm == "" {
		t.Skip("set HELM_BIN for local Helm packaging test")
	}
	common := []string{"template", "platform-api-server", "../../../helm/charts/platform-api-server",
		"--show-only", "templates/spanner-migrate-job.yaml", "--set", "gateway.enabled=false",
		"--set", "publicApi.enabled=false", "--set", "esp.enabled=false",
		"--set", "spanner.enabled=true", "--set", "spanner.projectId=test-project",
		"--set", "spanner.instanceId=test-spanner", "--set", "image.registry=quay.io",
		"--set", "image.repository=apahim/gecko"}
	render := func(extra ...string) ([]byte, error) {
		args := append(append([]string(nil), common...), extra...)
		return exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
	}
	baseline, err := render("--set", "image.tag=wif-p2-known-api")
	if err != nil {
		t.Fatalf("baseline migration render: %v: %s", err, baseline)
	}
	pinned, err := render("--set", "image.tag=wif-p4-new-api", "--set", "spanner.migration.image.registry=quay.io",
		"--set", "spanner.migration.image.repository=apahim/gecko", "--set", "spanner.migration.image.tag=wif-p2-known-api")
	if err != nil || !bytes.Equal(baseline, pinned) {
		t.Fatalf("API image bump changed the completed migration Job: %v\nold: %s\nnew: %s", err, baseline, pinned)
	}
	partial, err := render("--set", "spanner.migration.image.tag=wif-p2-known-api")
	if err == nil || !strings.Contains(string(partial), "requires registry, repository, and tag together") {
		t.Fatalf("partial migration image override was accepted: %v: %s", err, partial)
	}
	bad, err := render("--set", "spanner.migration.image.registry= ", "--set", "spanner.migration.image.repository=apahim/gecko", "--set", "spanner.migration.image.tag=wif-p2-known-api")
	if err == nil || !strings.Contains(string(bad), "non-empty concrete registry") {
		t.Fatalf("blank migration image registry was accepted: %v: %s", err, bad)
	}
}
