package main

import (
	"os"
	"strings"
	"testing"
)

// The Spanner DDL migration Job is Force=true,Replace=true, so any rendered
// change deletes and recreates it. Its image is pinned independently through
// migrationImage: nominally catalog/API-only changes (API image bumps, WIF
// packaging) must not alter the rendered Job, while a deliberate
// migrationImage bump must.
func TestSpannerMigrateJobHelmRendering(t *testing.T) {
	want, err := os.ReadFile("testdata/spanner-migrate-job-golden.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spannerSets := []string{
		"spanner.enabled=true",
		"spanner.projectId=test-project",
		"spanner.instanceId=test-instance",
	}
	golden := helmTemplateOutput(t, wifChartPath, spannerSets, "templates/spanner-migrate-job.yaml")
	if golden != string(want) {
		t.Fatal("default migration Job rendering drifted from the reviewed golden output")
	}
	apiOnlyChange := helmTemplateOutput(t, wifChartPath, append(spannerSets,
		"image.tag=deadbee",
		"wifConfigTemplates.enabled=true",
	), "templates/spanner-migrate-job.yaml")
	if apiOnlyChange != string(want) {
		t.Fatal("catalog/API-only changes altered the rendered migration Job")
	}
	migrationBump := helmTemplateOutput(t, wifChartPath, append(spannerSets,
		"migrationImage.tag=migratedd",
	), "templates/spanner-migrate-job.yaml")
	if migrationBump == string(want) {
		t.Fatal("migrationImage bump did not change the rendered migration Job")
	}
	if !strings.Contains(migrationBump, "gecko-platform-api-server:migratedd") {
		t.Fatal("migration Job does not use the pinned migrationImage")
	}
	if strings.Contains(migrationBump, "gecko-platform-api-server:deadbee") {
		t.Fatal("migration Job unexpectedly follows the API image")
	}
}
