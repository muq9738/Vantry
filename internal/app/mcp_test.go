package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestWorkbenchMCPCodemodeTrustProbeAndRemove(t *testing.T) {
	home := t.TempDir()
	agentDir := filepath.Join(home, ".pi", "agent")
	workspaceDir := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	fakePi := filepath.Join(binDir, "pi")
	probeJSON := `{"servers":[{"name":"gemini-ego","scope":"project","enabled":true,"exposure":"codemode","transport":"stdio","state":"connected","tools":["gemini_start","gemini_status"]}],"errors":[]}`
	script := "#!/bin/sh\nif [ \"$1\" = \"mcp\" ] && [ \"$2\" = \"list\" ]; then\n  printf '%s\\n' '" + probeJSON + "'\n  exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(fakePi, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	svc := &workbenchService{
		config: Config{
			DataDir:      t.TempDir(),
			PiExecutable: fakePi,
		},
		root: context.Background(),
	}

	// 1. User-scope upsert works even before a workspace is selected.
	resp, err := svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action: "mcp.upsert",
		Name:   "magpie",
		Input:  json.RawMessage(`{"scope":"user","config":{"url":"https://example.com/mcp"}}`),
	})
	if err != nil || resp.MCP == nil || len(resp.MCP.Servers) != 1 {
		t.Fatalf("user upsert = %+v, err=%v", resp.MCP, err)
	}
	if resp.MCP.Servers[0].Scope != "user" || resp.MCP.Servers[0].Exposure != "codemode" {
		t.Fatalf("unexpected user server: %+v", resp.MCP.Servers[0])
	}

	// 2. Project-scope upsert defaults to exposure=codemode.
	resp, err = svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action:    "mcp.upsert",
		Workspace: workspaceDir,
		Name:      "gemini-ego",
		Input:     json.RawMessage(`{"command":"node","args":["server.mjs"],"description":"Gemini via Ego"}`),
	})
	if err != nil || resp.MCP == nil || resp.MCP.ProjectTrusted {
		t.Fatalf("project upsert = %+v, err=%v", resp.MCP, err)
	}

	// 3. Trust project via mcp.trust.
	resp, err = svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action:    "mcp.trust",
		Workspace: workspaceDir,
		Input:     json.RawMessage(`{"trusted":true}`),
	})
	if err != nil || resp.MCP == nil || !resp.MCP.ProjectTrusted {
		t.Fatalf("mcp.trust = %+v, err=%v", resp.MCP, err)
	}

	// 4. Probe live status and tools via mcp.probe.
	resp, err = svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action:    "mcp.probe",
		Workspace: workspaceDir,
	})
	if err != nil || resp.MCP == nil || !resp.MCP.Probed {
		t.Fatalf("mcp.probe = %+v, err=%v", resp.MCP, err)
	}
	var probedTools int
	for _, srv := range resp.MCP.Servers {
		if srv.Name == "gemini-ego" && srv.LiveState == "connected" {
			probedTools = len(srv.Tools)
		}
	}
	if probedTools != 2 {
		t.Fatalf("expected 2 probed tools for gemini-ego, got %+v", resp.MCP.Servers)
	}

	// 5. Remove project server.
	resp, err = svc.Workbench(context.Background(), api.WorkbenchRequest{
		Action:    "mcp.remove",
		Workspace: workspaceDir,
		Name:      "gemini-ego",
	})
	if err != nil || resp.MCP == nil || len(resp.MCP.Servers) != 1 || resp.MCP.Servers[0].Name != "magpie" {
		t.Fatalf("mcp.remove = %+v, err=%v", resp.MCP, err)
	}
}
