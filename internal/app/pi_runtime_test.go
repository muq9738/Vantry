package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestWorkbenchPiRuntimeStatusSettingsAndSetup(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}

	home := t.TempDir()
	agentDir := filepath.Join(home, ".pi", "agent")
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	fakePi := filepath.Join(t.TempDir(), "pi")
	script := `#!` + python + `
import json, os, sys
if "--version" in sys.argv:
    print("1.6.0")
    sys.exit(0)
agent_dir = os.environ["PI_CODING_AGENT_DIR"]
models = []
if os.path.exists(os.path.join(agent_dir, "models.json")):
    with open(os.path.join(agent_dir, "models.json")) as f:
        doc = json.load(f)
    for pid, prov in doc.get("providers", {}).items():
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

	svc := &workbenchService{
		config: Config{
			DataDir:      dataDir,
			PiExecutable: fakePi,
		},
		Service: &statusService{baseService: &baseService{health: readyHealth()}, status: compatibleStatus()},
		root:    context.Background(),
	}

	// CapabilityPiRuntime must be advertised in ServerStatus.
	st, err := svc.ServerStatus(context.Background())
	if err != nil || !st.HasCapability(api.CapabilityPiRuntime) {
		t.Fatalf("ServerStatus missing CapabilityPiRuntime: %+v err=%v", st, err)
	}

	// pi.status before setup reports external custom path and version 1.6.0.
	resp, err := svc.Workbench(context.Background(), api.WorkbenchRequest{Action: "pi.status"})
	if err != nil || resp.PiRuntime == nil {
		t.Fatalf("pi.status = %+v err=%v", resp, err)
	}
	if resp.PiRuntime.ActiveSource != piprovider.RuntimeSourceCustom || resp.PiRuntime.ActiveVersion != "1.6.0" || resp.PiRuntime.Configured || resp.PiRuntime.ModelCount != 0 {
		t.Fatalf("unexpected initial PiRuntime: %+v", resp.PiRuntime)
	}

	// pi.settings.set updates customPath, mirrorMode, and npmRegistry cleanly, and allows clearing.
	resp, err = svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action: "pi.settings.set",
		Input:  json.RawMessage(`{"customPath":` + mustJSONString(t, fakePi) + `,"mirrorMode":"cn","npmRegistry":"https://registry.npmmirror.com/"}`),
	})
	if err != nil || resp.PiRuntime == nil {
		t.Fatalf("pi.settings.set = %+v err=%v", resp, err)
	}
	wantPath, _ := filepath.EvalSymlinks(fakePi)
	if resp.PiRuntime.CustomPath != wantPath || resp.PiRuntime.MirrorMode != piprovider.MirrorModeCN || resp.PiRuntime.NPMRegistry != "https://registry.npmmirror.com" {
		t.Fatalf("settings not applied: %+v", resp.PiRuntime)
	}

	// Clearing customPath and npmRegistry resets them cleanly.
	resp, err = svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action: "pi.settings.set",
		Input:  json.RawMessage(`{"customPath":"","npmRegistry":""}`),
	})
	if err != nil || resp.PiRuntime == nil {
		t.Fatalf("pi.settings.set clear = %+v err=%v", resp, err)
	}
	if resp.PiRuntime.CustomPath != "" || resp.PiRuntime.NPMRegistry != "" {
		t.Fatalf("settings not cleared: %+v", resp.PiRuntime)
	}

	// Active running task refuses pi.upgrade.
	busySvc := &workbenchService{
		config: Config{DataDir: dataDir, PiExecutable: fakePi},
		Service: &statusService{
			baseService: &baseService{
				health:   readyHealth(),
				snapshot: api.Snapshot{Runs: []api.Run{{ID: "run-1", ThreadID: "t1", Status: api.RunRunning}}},
			},
			status: compatibleStatus(),
		},
		root: context.Background(),
	}
	if _, err := busySvc.Workbench(context.Background(), api.WorkbenchRequest{Action: "pi.upgrade"}); err == nil || !strings.Contains(err.Error(), "正在运行") {
		t.Fatalf("expected active run to block pi.upgrade, got err=%v", err)
	}

	// pi.setup.save writes ~/.pi/agent and verifies models via RPC.
	resp, err = svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action: "pi.setup.save",
		Input: json.RawMessage(`{
			"preset":"custom",
			"providerId":"acme",
			"apiKey":"sk-acme-123",
			"baseUrl":"https://api.acme.example/v1",
			"api":"openai-completions",
			"models":["acme-pro"],
			"setDefault":true
		}`),
	})
	if err != nil || resp.Catalog == nil || resp.PiRuntime == nil {
		t.Fatalf("pi.setup.save = %+v err=%v", resp, err)
	}
	if len(resp.Catalog.Models) != 1 || resp.Catalog.Models[0].Key() != "acme/acme-pro" || !resp.PiRuntime.Configured || resp.PiRuntime.DefaultModel != "acme/acme-pro" {
		t.Fatalf("unexpected post-setup response: catalog=%+v runtime=%+v", resp.Catalog, resp.PiRuntime)
	}
}

func mustJSONString(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
