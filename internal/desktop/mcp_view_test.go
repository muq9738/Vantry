package desktop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestMCPPanelConfiguresCodemodeTrustProbeAndRemove(t *testing.T) {
	sharedScope := "user"
	sharedExposure := "codemode"
	sharedRemoved := false
	hasGemini := false

	buildCatalog := func(probed bool) *api.MCPCatalog {
		servers := []api.MCPServerInfo{}
		if !sharedRemoved {
			srv := api.MCPServerInfo{
				Name:      "shared",
				Scope:     sharedScope,
				Transport: "http",
				Enabled:   true,
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
			ProjectTrusted:  probed,
			CodemodeDefault: true,
			Probed:          probed,
			Servers:         servers,
		}
	}

	trusted := false
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case "mcp.probe":
			cat := buildCatalog(true)
			cat.ProjectTrusted = trusted
			return api.WorkbenchResponse{MCP: cat}, nil
		case "mcp.trust":
			trusted = true
			cat := buildCatalog(false)
			cat.ProjectTrusted = trusted
			return api.WorkbenchResponse{MCP: cat}, nil
		case "mcp.upsert":
			if r.Name == "shared" {
				sharedScope = "project-override"
				sharedExposure = "deferred"
			} else if r.Name == "gemini-ego" {
				hasGemini = true
			}
			cat := buildCatalog(false)
			cat.ProjectTrusted = trusted
			return api.WorkbenchResponse{MCP: cat}, nil
		case "mcp.remove":
			if r.Name == "gemini-ego" {
				hasGemini = false
			} else if r.Name == "shared" {
				sharedRemoved = true
			}
			cat := buildCatalog(false)
			cat.ProjectTrusted = trusted
			return api.WorkbenchResponse{MCP: cat}, nil
		default:
			cat := buildCatalog(false)
			cat.ProjectTrusted = trusted
			return api.WorkbenchResponse{MCP: cat}, nil
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
	if !tt.HasText("shared") || !tt.HasText("/repo/.pi/mcp.json") {
		t.Fatalf("missing configuration view: %q", tt.Texts())
	}

	// Trust project button triggers mcp.trust {"trusted":true}
	mustClick(t, tt, "信任此项目")
	tt.Frame()
	trustReq, ok := service.find("mcp.trust")
	if !ok || !strings.Contains(string(trustReq.Input), `"trusted":true`) {
		t.Fatalf("expected mcp.trust true, got %+v", service.requests)
	}

	// Probe triggers mcp.probe and renders live tool discovery
	mustClick(t, tt, "检测连接")
	tt.Frame()
	if _, ok := service.find("mcp.probe"); !ok {
		t.Fatal("clicking Probe must dispatch mcp.probe")
	}
	if !tt.HasText("已连接 · 2 工具") || !tt.HasText("magpie_search") {
		t.Fatalf("missing probed live tools output: %q", tt.Texts())
	}

	// Inline exposure switch sends mcp.upsert with {"exposure":"deferred"}
	mustClick(t, tt, "deferred · shared")
	tt.Frame()
	if expReq, ok := service.find("mcp.upsert"); !ok || expReq.Name != "shared" || !strings.Contains(string(expReq.Input), `"exposure":"deferred"`) {
		t.Fatalf("inline exposure switch = %+v", service.requests)
	}

	// Adding a new server injects default exposure=codemode + description
	mustClick(t, tt, "MCP 服务名")
	tt.Type("gemini-ego")
	mustClick(t, tt, "用途说明（Codemode 搜索提示）")
	tt.Type("Gemini browser MCP")
	mustClick(t, tt, "服务 JSON 配置")
	tt.Type(`{"command":"node","args":["/tmp/gemini/server.mjs"]}`)
	mustClick(t, tt, "保存到当前项目")
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
	if parsed["exposure"] != "codemode" || parsed["description"] != "Gemini browser MCP" {
		t.Fatalf("expected codemode default and description, got %+v", parsed)
	}

	// Inline remove on project server triggers mcp.remove
	mustClick(t, tt, "移除 · gemini-ego")
	tt.Frame()
	if rmReq, ok := service.find("mcp.remove"); !ok || rmReq.Name != "gemini-ego" {
		t.Fatalf("expected mcp.remove for gemini-ego: %+v", service.requests)
	}
}
