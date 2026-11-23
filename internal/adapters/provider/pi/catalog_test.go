package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogUsesRPCAndReturnsOnlyModelMetadata(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	directory := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", directory)
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), []byte(`{"defaultProvider":"extension-only","defaultModel":"custom-model"}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "pi")
	script := `#!` + python + `
import json,sys
assert "--no-extensions" in sys.argv and "--no-mcp" in sys.argv and "--no-tools" in sys.argv
model={"provider":"example","id":"family/model","name":"Model","reasoning":True,"input":["text","image"],"contextWindow":128000,"maxTokens":16000,"headers":{"Authorization":"private-fixture-value"}}
for line in sys.stdin:
 command=json.loads(line)
 kind=command["type"]
 data={"get_available_models":{"models":[model]},"get_state":{"model":model},"get_available_thinking_levels":{"levels":["off","high","max"]},"set_model":model}[kind]
 print(json.dumps({"type":"notification"}),flush=True)
 print(json.dumps({"id":command["id"],"type":"response","success":True,"data":data}),flush=True)
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := Catalog(context.Background(), path, "", "example/family/model")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Default != "example/family/model" || len(catalog.ThinkingLevels) != 3 {
		t.Fatalf("catalog: %+v", catalog)
	}
	// Only the real configuration gap is reported; a loadable catalog never
	// carries a standing hint about the resources the query skipped.
	if catalog.ConfiguredDefault != "extension-only/custom-model" || len(catalog.Warnings) != 1 {
		t.Fatalf("configured model gap was not reported: %+v", catalog)
	}
	if !strings.Contains(catalog.Warnings[0], "默认模型") {
		t.Fatalf("warning must name the unavailable default: %v", catalog.Warnings)
	}
	raw, _ := json.Marshal(catalog)
	if strings.Contains(string(raw), "private-fixture-value") || strings.Contains(string(raw), "headers") {
		t.Fatal("registry credentials escaped metadata projection")
	}
}

func TestCatalogReportsNoWarningsWhenModelsLoadNormally(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	directory := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", directory)
	// The configured default is part of the loaded catalog, which is the normal
	// Pi operation the model picker must render without any notice.
	settings := `{"defaultProvider":"example","defaultModel":"family/model"}`
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "pi")
	script := `#!` + python + `
import json,sys
model={"provider":"example","id":"family/model","name":"Model"}
for line in sys.stdin:
 command=json.loads(line)
 kind=command["type"]
 data={"get_available_models":{"models":[model]},"get_state":{"model":model},"get_available_thinking_levels":{"levels":["off","high"]},"set_model":model}[kind]
 print(json.dumps({"id":command["id"],"type":"response","success":True,"data":data}),flush=True)
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := Catalog(context.Background(), path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Default != "example/family/model" || catalog.ConfiguredDefault != "example/family/model" {
		t.Fatalf("catalog: %+v", catalog)
	}
	if len(catalog.Warnings) != 0 {
		t.Fatalf("normal Pi operation must not add model picker hints: %v", catalog.Warnings)
	}
}

func TestInstalledPiCatalogOptIn(t *testing.T) {
	if os.Getenv("VANTRY_CATALOG_TEST") != "1" {
		t.Skip("set VANTRY_CATALOG_TEST=1 to query installed Pi without prompting a model")
	}
	catalog, err := Catalog(context.Background(), "pi", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) == 0 {
		t.Fatal("installed Pi returned no available models")
	}
	t.Logf("Pi models=%d default=%s configured=%s thinking=%v warnings=%v", len(catalog.Models), catalog.Default, catalog.ConfiguredDefault, catalog.ThinkingLevels, catalog.Warnings)
}
