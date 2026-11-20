package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func render42221Example(t *testing.T, args ...string) map[string]map[string]interface{} {
	t.Helper()
	helm := os.Getenv("HELM_BIN")
	if helm == "" {
		t.Skip("set HELM_BIN for local inactive-example packaging tests")
	}
	data, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("local render: %v: %s", err, data)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	objects := map[string]map[string]interface{}{}
	for {
		var obj map[string]interface{}
		if err := decoder.Decode(&obj); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(obj) == 0 {
			continue
		}
		meta := obj["metadata"].(map[string]interface{})
		objects[obj["kind"].(string)+"/"+meta["name"].(string)] = obj
	}
	return objects
}

func Test42221OptInTemplateExamplesKeepGatesAndJob(t *testing.T) {
	args := []string{"template", "platform-api-server", "../../../helm/charts/platform-api-server",
		"--set", "gateway.enabled=false", "--set", "publicApi.enabled=false", "--set", "esp.enabled=false",
		"--set", "spanner.enabled=true", "--set", "spanner.projectId=test-project", "--set", "spanner.instanceId=test-spanner",
		"--set", "image.registry=quay.io", "--set", "image.repository=apahim/gecko", "--set", "image.tag=unchanged-api",
		"--set", "spanner.migration.image.registry=quay.io", "--set", "spanner.migration.image.repository=apahim/gecko", "--set", "spanner.migration.image.tag=retained-migration"}
	before := render42221Example(t, args...)
	after := render42221Example(t, append(args, "-f", "../../../helm/examples/wif-42221/platform-api-values.yaml")...)
	for name, obj := range before {
		if !reflect.DeepEqual(obj, after[name]) {
			t.Fatalf("overlay changed existing object %s", name)
		}
	}
	if len(after) != len(before)+2 {
		t.Fatal("example must add only the two unverified templates")
	}
	for _, name := range []string{"4.22.0", "5.0.0"} {
		key := "WIFConfigTemplate/" + name
		if before[key] != nil {
			t.Fatal("shared chart defaults enabled a template")
		}
		data, _ := json.Marshal(after[key])
		var w privatev1.WIFConfigTemplate
		if err := json.Unmarshal(data, &w); err != nil {
			t.Fatal(err)
		}
		if err := w.ValidateCreate(t.Context()); err != nil {
			t.Fatal(err)
		}
		digest, err := privatev1.WIFConfigDigest(w.Spec)
		if err != nil || digest != "sha256:c0b4c3dbdd48d1925bf7c172ac0385639d00092f083b5cac11865e83c3acea71" || privatev1.IsWIFConfigVerified(w.Spec) {
			t.Fatal("template changed permissions or enabled verification")
		}
	}
	dep := after["Deployment/platform-api-server"]["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	container := dep["containers"].([]interface{})[0].(map[string]interface{})
	found := false
	for _, arg := range container["args"].([]interface{}) {
		if arg == "--enable-iam-application-writes=false" {
			found = true
		}
	}
	if !found {
		t.Fatal("assertion gate not explicitly closed")
	}
}

func Test42221CandidateFloorExamplePreservesOtherChannels(t *testing.T) {
	base := filepath.Join(t.TempDir(), "existing-values.yaml")
	data := []byte("channels:\n")
	for _, name := range []string{"candidate", "fast", "stable", "eus"} {
		data = append(data, []byte("  "+name+":\n    minimumSupportedVersion: '5.0'\n    installDefaultVersion: '5.0.0'\n    fleetMinorVersion: '5.0'\n")...)
	}
	if err := os.WriteFile(base, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"template", "gecko-version-sync", "../../../helm/charts/gecko-version-sync", "--show-only", "templates/channels.yaml", "--set", "image.tag=retained-version-sync", "-f", base}
	before := render42221Example(t, args...)
	after := render42221Example(t, append(args, "-f", "../../../helm/examples/wif-42221/version-sync-values.yaml")...)
	if len(before) != 4 || len(after) != 4 {
		t.Fatal("channel membership changed")
	}
	for name, obj := range before {
		if name != "Channel/candidate" {
			if !reflect.DeepEqual(obj, after[name]) {
				t.Fatalf("changed unrelated channel %s", name)
			}
			continue
		}
		spec := after[name]["spec"].(map[string]interface{})
		if spec["minimumSupportedVersion"] != "4.22" || spec["fleetMinorVersion"] != "5.0" || spec["installDefaultVersion"] != "5.0.0" {
			t.Fatal("candidate overlay altered defaults/authorization")
		}
	}
}
