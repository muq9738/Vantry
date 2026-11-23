package pi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestSaveProviderSetupNonDestructiveMergeAndValidation(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}

	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	// Pre-populate unrelated provider credentials, models, and settings.
	if err := os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(`{"existing":{"type":"api_key","key":"keep-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(`{"providers":{"existing":{"baseUrl":"https://keep.example/v1","api":"openai-completions","models":[{"id":"keep-model"}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"theme":"dark","defaultProvider":"existing","defaultModel":"keep-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fakePi := filepath.Join(t.TempDir(), "pi")
	script := `#!` + python + `
import json, os, sys
agent_dir = os.environ["PI_CODING_AGENT_DIR"]
with open(os.path.join(agent_dir, "auth.json")) as f:
    auth = json.load(f)
models = []
if os.path.exists(os.path.join(agent_dir, "models.json")):
    with open(os.path.join(agent_dir, "models.json")) as f:
        doc = json.load(f)
    for pid, prov in doc.get("providers", {}).items():
        if pid in auth:
            for m in prov.get("models", []):
                models.append({"provider": pid, "id": m["id"], "name": m.get("name", m["id"])})
for line in sys.stdin:
    cmd = json.loads(line)
    kind = cmd["type"]
    data = {
        "get_available_models": {"models": models},
        "get_state": {"model": models[0] if models else {"provider": "", "id": ""}},
        "get_available_thinking_levels": {"levels": ["off", "high"]},
        "set_model": models[0] if models else {},
    }[kind]
    print(json.dumps({"id": cmd["id"], "type": "response", "success": True, "data": data}), flush=True)
`
	if err := os.WriteFile(fakePi, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	// Reject bad inputs.
	if _, err := SaveProviderSetup(context.Background(), fakePi, agentDir, api.PiSetupRequest{
		Preset: "custom", ProviderID: "my-gw", APIKey: "", BaseURL: "https://gw.example/v1", Models: []string{"m1"},
	}); err == nil {
		t.Fatal("expected empty apiKey error")
	}
	if _, err := SaveProviderSetup(context.Background(), fakePi, agentDir, api.PiSetupRequest{
		Preset: "custom", ProviderID: "BAD ID", APIKey: "sk-1", BaseURL: "https://gw.example/v1", Models: []string{"m1"},
	}); err == nil {
		t.Fatal("expected invalid providerId error")
	}
	if _, err := SaveProviderSetup(context.Background(), fakePi, agentDir, api.PiSetupRequest{
		Preset: "custom", ProviderID: "my-gw", APIKey: "sk-1", BaseURL: "ftp://bad", Models: []string{"m1"},
	}); err == nil {
		t.Fatal("expected invalid baseUrl error")
	}

	catalog, err := SaveProviderSetup(context.Background(), fakePi, agentDir, api.PiSetupRequest{
		Preset:     "custom",
		ProviderID: "my-gw",
		APIKey:     "sk-gw-secret",
		BaseURL:    "https://gw.example/v1/",
		API:        "openai-completions",
		Models:     []string{"gpt-4o, deepseek-chat"},
		SetDefault: true,
	})
	if err != nil {
		t.Fatalf("SaveProviderSetup failed: %v", err)
	}
	if len(catalog.Models) != 3 || catalog.ConfiguredDefault != "my-gw/gpt-4o" {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}

	// Existing files merged cleanly without deleting unrelated entries and kept 0600 permissions.
	for _, name := range []string{"auth.json", "models.json", "settings.json"} {
		info, err := os.Stat(filepath.Join(agentDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o, want 0600", name, info.Mode().Perm())
		}
	}

	authRaw, _ := os.ReadFile(filepath.Join(agentDir, "auth.json"))
	var authMap map[string]map[string]string
	if err := json.Unmarshal(authRaw, &authMap); err != nil {
		t.Fatal(err)
	}
	if authMap["existing"]["key"] != "keep-secret" || authMap["my-gw"]["key"] != "sk-gw-secret" {
		t.Fatalf("auth.json lost or miswrote credentials: %+v", authMap)
	}

	settingsRaw, _ := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	var settingsMap map[string]any
	if err := json.Unmarshal(settingsRaw, &settingsMap); err != nil {
		t.Fatal(err)
	}
	if settingsMap["theme"] != "dark" || settingsMap["defaultProvider"] != "my-gw" || settingsMap["defaultModel"] != "gpt-4o" {
		t.Fatalf("settings.json unexpected: %+v", settingsMap)
	}
	if !HasPiConfiguration(agentDir) {
		t.Fatal("HasPiConfiguration should report true")
	}

	// Ensure no secrets appear in catalog JSON.
	rawCatalog, _ := json.Marshal(catalog)
	if strings.Contains(string(rawCatalog), "sk-gw-secret") || strings.Contains(string(rawCatalog), "keep-secret") {
		t.Fatal("catalog leaked API key")
	}
}
