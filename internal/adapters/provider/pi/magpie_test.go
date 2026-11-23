package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestMagpieCatalogUsesPickedPiModelsAndExcludesOtherProviders(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", root)
	os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"defaultProvider":"magpie","defaultModel":"one"}`), 0600)
	selection := filepath.Join(root, "magpie.json")
	os.WriteFile(selection, []byte(`{"pickedModels":{"pi":["shared","one","one","missing"],"other-client":["unselected"]}}`), 0600)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	executable := filepath.Join(root, "pi")
	source := `#!` + python + `
import json,sys
models=[{"provider":"magpie","id":"one","name":"One"},{"provider":"magpie","id":"shared","name":"Selected"},{"provider":"magpie","id":"unselected","name":"Not selected"},{"provider":"xai","id":"shared","name":"Other"},{"provider":"other","id":"one","name":"Other"}]
for line in sys.stdin:
 r=json.loads(line)
 data={"get_available_models":{"models":models},"get_state":{"model":models[0]},"get_available_thinking_levels":{"levels":["off","high"]},"set_model":models[1]}[r["type"]]
 print(json.dumps({"id":r["id"],"type":"response","success":True,"data":data}),flush=True)
`
	if err := os.WriteFile(executable, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := MagpieCatalog(context.Background(), executable, "", "magpie/shared", selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 || catalog.Models[0].Key() != "magpie/shared" || catalog.Models[1].Key() != "magpie/one" {
		t.Fatalf("catalog ignored Magpie selection or its order: %+v", catalog.Models)
	}
	for _, model := range catalog.Models {
		if model.Provider != "magpie" {
			t.Fatalf("unselected provider leaked: %+v", model)
		}
	}
	if catalog.Default != "magpie/one" || len(catalog.Warnings) != 1 {
		t.Fatalf("default or missing-export notice: %+v", catalog)
	}
	if _, err := MagpieCatalog(context.Background(), "must-not-start", "", "xai/shared", selection); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("other provider was accepted: %v", err)
	}
	if _, err := MagpieCatalog(context.Background(), "must-not-start", "", "magpie/unselected", selection); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("unpicked Magpie model accepted: %v", err)
	}
}

func TestEmptyMagpieSelectionNeverFallsBackToPiRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magpie.json")
	os.WriteFile(path, []byte(`{"pickedModels":{"pi":[],"other-client":["other"]}}`), 0600)
	catalog, err := MagpieCatalog(context.Background(), "must-not-start", "", "", path)
	if err != nil || len(catalog.Models) != 0 || catalog.Default != "" || len(catalog.Warnings) == 0 {
		t.Fatalf("empty source fell back: %+v %v", catalog, err)
	}
	if _, err := MagpieCatalog(context.Background(), "must-not-start", "", "", path+"-missing"); api.CodeOf(err) != api.NotReady {
		t.Fatalf("missing source did not produce actionable error: %v", err)
	}
}

func TestMagpieExecutionScopeUsesModelIdentityAndThinkingSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magpie.json")
	data, _ := json.Marshal(map[string]any{"pickedModels": map[string][]string{"pi": {"family/model", "family/model:latest"}}})
	os.WriteFile(path, data, 0600)
	for _, model := range []string{"magpie/family/model", "magpie/family/model:high", "magpie/family/model:latest", "magpie/family/model:latest:max"} {
		if err := ValidateMagpieModel(path, model); err != nil {
			t.Fatalf("configured identity rejected %q: %v", model, err)
		}
	}
	for _, model := range []string{"xai/family/model", "other/family/model", "magpie/family/model-other", ""} {
		if err := ValidateMagpieModel(path, model); api.CodeOf(err) != api.InvalidCommand {
			t.Fatalf("out-of-source identity accepted %q: %v", model, err)
		}
	}
}
