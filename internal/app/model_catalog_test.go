package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestModelCatalogDefaultsToPiWithoutMagpieConfiguration(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PI_CODING_AGENT_DIR", root)
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"defaultProvider":"example","defaultModel":"family/model"}`), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "pi")
	script := `#!` + python + `
import json,sys
models=[{"provider":"example","id":"family/model","name":"Pi Default"},{"provider":"magpie","id":"selected","name":"Magpie"}]
for line in sys.stdin:
 r=json.loads(line)
 data={"get_available_models":{"models":models},"get_state":{"model":models[0]},"get_available_thinking_levels":{"levels":["off","high"]},"set_model":models[0]}[r["type"]]
 print(json.dumps({"id":r["id"],"type":"response","command":r["type"],"success":True,"data":data}),flush=True)
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	service := &workbenchService{config: Config{PiExecutable: executable}}
	response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "models", Model: "example/family/model"})
	if err != nil {
		t.Fatalf("Pi model lookup required an unrelated Magpie configuration: %v", err)
	}
	catalog := response.Catalog
	if catalog == nil || len(catalog.Models) != 2 || catalog.Default != "example/family/model" || catalog.ConfiguredDefault != catalog.Default {
		t.Fatalf("Pi registry/default was replaced or filtered: %+v", catalog)
	}
	models, err := service.ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0].Key() != "example/family/model" {
		t.Fatalf("native picker lost the Pi provider: %+v %v", models, err)
	}

	selection := filepath.Join(root, "magpie.json")
	if err := os.WriteFile(selection, []byte(`{"pickedModels":{"pi":["selected"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	service.config.MagpieSettingsPath = selection
	response, err = service.Workbench(context.Background(), api.WorkbenchRequest{Action: "models"})
	if err != nil || response.Catalog == nil || len(response.Catalog.Models) != 1 || response.Catalog.Models[0].Key() != "magpie/selected" {
		t.Fatalf("explicit Magpie selection no longer applies: %+v %v", response.Catalog, err)
	}
	if _, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "models", Model: "example/family/model"}); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("explicit Magpie restriction was ignored: %v", err)
	}
}
