package desktop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestMCPPanelCodexStyleTrustProbeToggleAndAddModal(t *testing.T) {
	sharedScope := "user"
	sharedExposure := "codemode"
	sharedEnabled := true
	sharedRemoved := false
	hasGemini := false
	trusted := false

	buildCatalog := func(probed bool) *api.MCPCatalog {
		servers := []api.MCPServerInfo{}
		if !sharedRemoved {
			srv := api.MCPServerInfo{
				Name:      "shared",
				Scope:     sharedScope,
				Transport: "http",
				Endpoint:  "http://127.0.0.1:3425/mcp",
				Enabled:   sharedEnabled,
				Exposure:  sharedExposure,
			}
			if probed {
				srv.LiveState = "connected"
				srv.Tools = []string{"magpie_search", "rill_info"}
			}
			servers = append(servers, srv)
		}
		if hasGemini {
			servers = append(servers, api.MCPServerInfo{
				Name:        "gemini-ego",
				Scope:       "project",
				Transport:   "stdio",
				Enabled:     true,
				Exposure:    "codemode",
				Description: "Gemini browser MCP",
			})
		}
		return &api.MCPCatalog{
			Workspace:       "/repo",
			UserFile:        "/home/.pi/agent/mcp.json",
			ProjectFile:     "/repo/.pi/mcp.json",
			ProjectTrusted:  trusted,
			CodemodeDefault: true,
			Probed:          probed,
			Servers:         servers,
		}
	}

	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case "mcp.probe":
			return api.WorkbenchResponse{MCP: buildCatalog(true)}, nil
		case "mcp.trust":
			trusted = true
			return api.WorkbenchResponse{MCP: buildCatalog(false)}, nil
		case "mcp.upsert":
			if r.Name == "shared" {
				var raw map[string]any
				_ = json.Unmarshal(r.Input, &raw)
				if exp, ok := raw["exposure"].(string); ok {
					sharedScope = "project-override"
					sharedExposure = exp
				}
				if en, ok := raw["enabled"].(bool); ok {
					sharedEnabled = en
				}
			} else if r.Name == "gemini-ego" {
				hasGemini = true
			}
			return api.WorkbenchResponse{MCP: buildCatalog(false)}, nil
		case "mcp.remove":
			if r.Name == "gemini-ego" {
				hasGemini = false
			} else if r.Name == "shared" {
				sharedRemoved = true
			}
			return api.WorkbenchResponse{MCP: buildCatalog(false)}, nil
		default:
			return api.WorkbenchResponse{MCP: buildCatalog(false)}, nil
		}
	})

	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "MCP")
	tt.Frame()
	if _, ok := service.find("mcp.list"); !ok {
		t.Fatal("opening MCP must request configuration")
	}
	if _, ok := service.find("mcp.probe"); ok {
		t.Fatal("opening MCP must not probe servers eagerly")
	}
	// Compact panel: no permanent inline JSON form or 4 exposure buttons cluttering rows.
	if tt.HasText("服务 JSON 配置") || tt.HasText("deferred · shared") {
		t.Fatalf("compact MCP view should hide form and exposure buttons by default: %v", tt.Texts())
	}
	if !tt.HasText("shared") || !tt.HasText("信任此项目") {
		t.Fatalf("missing server or untrusted banner: %v", tt.Texts())
	}

	// Trust project collapses the trust banner once trusted.
	mustClick(t, tt, "信任此项目")
	tt.Frame()
	trustReq, ok := service.find("mcp.trust")
	if !ok || !strings.Contains(string(trustReq.Input), `"trusted":true`) {
		t.Fatalf("expected mcp.trust true, got %+v", service.requests)
	}
	if tt.HasText("信任此项目") {
		t.Fatalf("trusted project should hide trust banner: %v", tt.Texts())
	}

	// Probe triggers mcp.probe and renders live tool discovery.
	mustClick(t, tt, "检测连接")
	tt.Frame()
	if _, ok := service.find("mcp.probe"); !ok {
		t.Fatal("clicking Probe must dispatch mcp.probe")
	}
	if !tt.HasText("已连接 · 2 工具") || !tt.HasText("magpie_search") {
		t.Fatalf("missing probed live tools output: %v", tt.Texts())
	}

	// Native toggle disables the server.
	mustClick(t, tt, "切换 shared")
	tt.Frame()
	if sharedEnabled {
		t.Fatal("switch did not disable shared server")
	}

	// Open "+ 添加" modal and add a stdio server via Quick Add (defaults to codemode).
	mustClick(t, tt, "添加")
	tt.Frame()
	if !w.mcp.addOpen {
		t.Fatal("clicking Add did not open modal")
	}
	w.mcp.scopeTarget = "project"
	w.mcp.transport = 1 // stdio
	w.mcp.name = "gemini-ego"
	w.mcp.endpoint = `node "/tmp/gemini/server.mjs" --flag`
	w.mcp.description = "Gemini browser MCP"
	tt.Frame()
	mustClick(t, tt, "保存")
	tt.Frame()

	var upsertReq *api.WorkbenchRequest
	for i := range service.requests {
		r := service.requests[i]
		if r.Action == "mcp.upsert" && r.Name == "gemini-ego" {
			upsertReq = &r
		}
	}
	if upsertReq == nil {
		t.Fatalf("did not submit new server config: %+v", service.requests)
	}
	var parsed map[string]any
	if err := json.Unmarshal(upsertReq.Input, &parsed); err != nil {
		t.Fatalf("invalid submitted json: %v", err)
	}
	if parsed["exposure"] != "codemode" || parsed["command"] != "node" || parsed["description"] != "Gemini browser MCP" {
		t.Fatalf("expected codemode + split stdio command, got %+v", parsed)
	}
	if w.mcp.addOpen {
		t.Fatal("add modal did not close after successful save")
	}
}

func TestBuildServerUpsertInputExtractsNameFromMCPServersSnippet(t *testing.T) {
	name, raw, ok := buildServerUpsertInput("user", "", "", "codemode", `{"mcpServers":{"docs":{"url":"https://example.com/mcp"}}}`)
	if !ok || name != "docs" {
		t.Fatalf("got ok=%v name=%q", ok, name)
	}
	if !strings.Contains(string(raw), `"exposure":"codemode"`) || !strings.Contains(string(raw), `"scope":"user"`) {
		t.Fatalf("unexpected payload: %s", raw)
	}
}
