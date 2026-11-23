package desktop

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type mcpPanelState struct {
	catalog     *api.MCPCatalog
	scope       string
	expanded    map[string]bool
	addOpen     bool
	addTab      int // 0: quick add (URL / stdio), 1: paste JSON
	transport   int // 0: HTTP URL, 1: stdio command
	scopeTarget string
	name        string
	endpoint    string
	description string
	exposure    string
	config      string
}

func (w *workbench) mcpScope() string {
	return w.environmentKey + "\x00" + w.selected + "\x00" + w.selectedWorkspacePath()
}

func (w *workbench) ensureMCPDefaults() {
	if w.mcp.exposure == "" {
		w.mcp.exposure = "codemode"
	}
	if w.mcp.expanded == nil {
		w.mcp.expanded = map[string]bool{}
	}
	hasWorkspace := w.selectedWorkspacePath() != "" || w.hasThread(w.selected)
	if w.mcp.scopeTarget == "" {
		// Align with `pi mcp add`: default to user-level unless user switches to project.
		w.mcp.scopeTarget = "user"
	}
	if !hasWorkspace && w.mcp.scopeTarget == "project" {
		w.mcp.scopeTarget = "user"
	}
}

func (w *workbench) loadMCP() { w.mcpCall("mcp.list", "", nil, false) }

func (w *workbench) mcpCall(action, name string, input json.RawMessage, closeAddOnSuccess bool) {
	scope, target := w.mcpScope(), w.selected
	if w.mcp.scope != scope {
		w.mcp = mcpPanelState{scope: scope}
	}
	w.ensureMCPDefaults()
	request := api.WorkbenchRequest{Action: action, Workspace: w.selectedWorkspacePath(), Name: name, Input: input}
	if w.hasThread(target) {
		request.ThreadID = target
	}
	w.workbenchCall("mcp:"+scope, request, func(r api.WorkbenchResponse, err error) {
		if w.mcpScope() != scope {
			return
		}
		if err != nil {
			w.setToolFeedback("mcp:"+target, notice{Text: describe(w.tr("MCP 操作失败", "MCP operation failed"), err), Level: noticeError})
			return
		}
		w.mcp.catalog = r.MCP
		if w.mcp.catalog != nil && w.mcp.catalog.ProbeError != "" {
			w.setToolFeedback("mcp:"+target, notice{Text: w.mcp.catalog.ProbeError, Level: noticeWarn})
		} else {
			w.setToolFeedback("mcp:"+target, notice{})
		}
		if closeAddOnSuccess && action == "mcp.upsert" {
			w.mcp.addOpen = false
			w.mcp.name = ""
			w.mcp.endpoint = ""
			w.mcp.description = ""
			w.mcp.exposure = "codemode"
			w.mcp.config = ""
		}
	})
}

func mcpDisplayExposure(value string) string {
	switch value {
	case "", "codemode", "codemode-deferred":
		return "codemode"
	default:
		return value
	}
}

func (w *workbench) mcpScopeBadge(scope string) string {
	switch scope {
	case "project-override":
		return w.tr("项目覆盖", "Override")
	case "project":
		return w.tr("项目", "Project")
	default:
		return w.tr("全局", "Global")
	}
}

func (w *workbench) mcpLiveStatusBadge(server api.MCPServerInfo) (string, ui.Color, bool) {
	if server.LiveState == "" {
		return "", ui.Color{}, false
	}
	switch server.LiveState {
	case "connected":
		return fmt.Sprintf(w.tr("已连接 · %d 工具", "Connected · %d tools"), len(server.Tools)), ui.Hex("#16A34A"), true
	case "needs_auth":
		return w.tr("需登录", "Sign in"), ui.Hex("#D97706"), true
	case "disabled":
		return w.tr("已停用", "Disabled"), ui.Hex("#6B7280"), true
	default:
		return server.LiveState, ui.Hex("#DC2626"), true
	}
}

func formatProbeToolsPreview(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	const limit = 6
	if len(tools) <= limit {
		return strings.Join(tools, ", ")
	}
	return fmt.Sprintf("%s … +%d", strings.Join(tools[:limit], ", "), len(tools)-limit)
}

// splitCommandArgs splits a stdio command line into executable and args while
// respecting single/double quotes and simple backslash escapes.
func splitCommandArgs(raw string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inToken := false
	escaped := false
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			inToken = true
		case r == '\\' && quote != '\'':
			escaped = true
			inToken = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inToken = true
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case unicode.IsSpace(r):
			if inToken {
				out = append(out, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if inToken {
		out = append(out, cur.String())
	}
	return out
}

func buildQuickServerUpsertInput(scopeTarget string, transport int, endpoint, description string) (json.RawMessage, bool) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, false
	}
	payload := map[string]any{
		"exposure": "codemode",
	}
	if desc := strings.TrimSpace(description); desc != "" {
		payload["description"] = desc
	}
	if transport == 0 {
		payload["url"] = endpoint
	} else {
		parts := splitCommandArgs(endpoint)
		if len(parts) == 0 {
			return nil, false
		}
		payload["command"] = parts[0]
		if len(parts) > 1 {
			payload["args"] = parts[1:]
		}
	}
	return wrapScopeEnvelope(scopeTarget, payload), true
}

// buildServerUpsertInput parses a single server object or {"mcpServers": {...}}
// snippet, injects exposure=codemode when omitted, and optionally infers the
// server name when the caller pasted a single-entry mcpServers snippet.
func buildServerUpsertInput(scopeTarget, name, description, exposure, rawJSON string) (string, json.RawMessage, bool) {
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawJSON), &rawMap); err != nil || rawMap == nil {
		return "", nil, false
	}
	resolvedName := strings.TrimSpace(name)
	if wrapperRaw, hasWrapper := rawMap["mcpServers"]; hasWrapper {
		var servers map[string]map[string]json.RawMessage
		if err := json.Unmarshal(wrapperRaw, &servers); err != nil || len(servers) == 0 {
			return "", nil, false
		}
		if resolvedName == "" {
			if len(servers) != 1 {
				return "", nil, false
			}
			for k := range servers {
				resolvedName = strings.TrimSpace(k)
			}
		}
		entry, ok := servers[resolvedName]
		if !ok || entry == nil {
			return "", nil, false
		}
		if desc := strings.TrimSpace(description); desc != "" {
			if _, exists := entry["description"]; !exists {
				entry["description"], _ = json.Marshal(desc)
			}
		}
		if exp := strings.TrimSpace(exposure); exp != "" {
			if _, exists := entry["exposure"]; !exists {
				entry["exposure"], _ = json.Marshal(exp)
			}
		}
		servers[resolvedName] = entry
		rawMap["mcpServers"], _ = json.Marshal(servers)
	} else {
		if resolvedName == "" {
			return "", nil, false
		}
		if desc := strings.TrimSpace(description); desc != "" {
			if _, exists := rawMap["description"]; !exists {
				rawMap["description"], _ = json.Marshal(desc)
			}
		}
		if exp := strings.TrimSpace(exposure); exp != "" {
			if _, exists := rawMap["exposure"]; !exists {
				rawMap["exposure"], _ = json.Marshal(exp)
			}
		}
	}
	mergedConfig, err := json.Marshal(rawMap)
	if err != nil {
		return "", nil, false
	}
	if scopeTarget == "user" {
		wrapped, _ := json.Marshal(map[string]any{
			"scope":  "user",
			"config": json.RawMessage(mergedConfig),
		})
		return resolvedName, wrapped, true
	}
	return resolvedName, json.RawMessage(mergedConfig), true
}

func wrapScopeEnvelope(scopeTarget string, payload any) json.RawMessage {
	if scopeTarget == "user" {
		b, _ := json.Marshal(map[string]any{"scope": "user", "config": payload})
		return b
	}
	b, _ := json.Marshal(payload)
	return b
}

func (w *workbench) mcpPanel(c *ui.Context, p palette) {
	w.ensureMCPDefaults()
	busy := w.features.busy["mcp:"+w.mcpScope()]
	cat := w.mcp.catalog
	if w.mcp.scope != w.mcpScope() {
		cat = nil
	}

	enabledCount := 0
	if cat != nil {
		for _, srv := range cat.Servers {
			if srv.Enabled {
				enabledCount++
			}
		}
	}

	ui.Scroll(c.Key("mcp-panel")).Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).Gap(10).Children(func() {
			// Single-row Codex / T3 Code style header: title + count pill + Probe / + Add / Config menu.
			ui.Row(c.Key("mcp-header")).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "MCP").FontSize(14).FontWeight(600).TextColor(p.ink).NoWrap()
				if cat != nil {
					summary := fmt.Sprintf(w.tr("%d 已启用 · Codemode", "%d enabled · Codemode"), enabledCount)
					ui.Text(c, summary).FontSize(11).TextColor(p.faint).Padding(2, 7).Radius(9).Background(p.soft).NoWrap().
						Tooltip(w.tr("由外置 Pi 原生 builtin:mcp + codemode 沙盒驱动，工具 Schema 不占初始上下文。", "Powered by external Pi builtin:mcp + codemode sandbox so tool schemas stay out of initial context."))
				}
				ui.Spacer(c)
				probeBtn := kitButton(c, p, "mcp-probe", w.tr("检测连接", "Probe"), controlOutline, controlSM)
				probeBtn.Disabled(busy || cat == nil)
				if probeBtn.Clicked() {
					w.mcpCall("mcp.probe", "", nil, false)
				}
				addBtn := kitButton(c, p, "mcp-open-add", w.tr("添加", "Add"), controlPrimary, controlSM)
				addBtn.Disabled(busy || cat == nil)
				if addBtn.Clicked() {
					w.mcp.addOpen = true
				}
				more := iconButton(c, p, "mcp-header-more", w.tr("MCP 配置选项", "MCP config options"), moreIcon).Size(26, 26).Padding(5)
				more.PopoverMenuButton(func(m *ui.Menu) {
					if m.Item(w.tr("刷新配置", "Refresh")).Disabled(busy).Chosen() {
						w.loadMCP()
					}
					if cat != nil && !w.defaults.RemotePeer && w.proxyEnvironmentID() == "" {
						if cat.UserFile != "" {
							m.Separator()
							if m.Item(w.tr("打开全局 mcp.json", "Open global mcp.json")).Chosen() {
								w.openInEditor(cat.UserFile)
							}
						}
						if cat.ProjectFile != "" {
							if m.Item(w.tr("打开项目 .pi/mcp.json", "Open project .pi/mcp.json")).Chosen() {
								w.openInEditor(cat.ProjectFile)
							}
						}
					}
					if cat != nil && cat.ProjectFile != "" && cat.ProjectTrusted {
						m.Separator()
						if m.Item(w.tr("取消项目 MCP 信任", "Untrust project MCP")).Disabled(busy).Chosen() {
							b, _ := json.Marshal(map[string]bool{"trusted": false})
							w.mcpCall("mcp.trust", "", b, false)
						}
					}
				}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomEnd, Self: ui.AnchorTopEnd})
			})

			if cat == nil {
				ui.Text(c, w.tr("正在读取 Pi MCP 配置…", "Loading Pi MCP configuration…")).FontSize(12).TextColor(p.muted).Padding(8, 2)
				return
			}

			// Show project trust banner ONLY when untrusted (Pi RPC silently skips .pi/mcp.json).
			if cat.ProjectFile != "" && !cat.ProjectTrusted {
				ui.Row(c.Key("mcp-trust-banner")).FillWidth().Padding(8, 10).Gap(8).Radius(10).AlignItems(ui.Center).
					Background(p.warning.Alpha(0.10)).Border(1, p.warning.Alpha(0.32)).Children(func() {
					ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
						ui.Text(c, w.tr("项目未信任 · Pi 暂不加载项目 MCP", "Project untrusted · Pi skips project MCP")).FontSize(12).FontWeight(600).TextColor(p.warning).Wrap()
						ui.Text(c, cat.ProjectFile).Font("Menlo").FontSize(10.5).TextColor(p.muted).SingleLine().Ellipsis("…").Tooltip(cat.ProjectFile)
					})
					trustBtn := kitButton(c, p, "mcp-trust-toggle", w.tr("信任此项目", "Trust project"), controlOutline, controlSM)
					trustBtn.Disabled(busy)
					if trustBtn.Clicked() {
						b, _ := json.Marshal(map[string]bool{"trusted": true})
						w.mcpCall("mcp.trust", "", b, false)
					}
				})
			}

			if len(cat.Servers) == 0 {
				settingsGroup(c, p, "mcp-empty", func() {
					ui.Column(c).FillWidth().Padding(20, 16).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, serverIcon).FontSize(20).TextColor(p.faint)
						ui.Text(c, w.tr("尚未配置 MCP 服务", "No MCP servers configured")).FontSize(13).FontWeight(500).TextColor(p.ink)
						ui.Text(c, w.tr("直接读写 Pi 的 mcp.json，默认以 codemode 沙盒按需加载，不浪费上下文 Token。", "Reads and writes Pi's mcp.json directly with codemode sandbox enabled by default.")).FontSize(11.5).TextColor(p.muted).TextAlign(ui.Center).Wrap()
						if kitButton(c, p, "mcp-empty-add", w.tr("+ 添加第一个服务", "+ Add server"), controlOutline, controlSM).Clicked() {
							w.mcp.addOpen = true
						}
					})
				})
				return
			}

			// Compact Codex-style server list: one clean row per server + Switch + "..." menu.
			settingsGroup(c, p, "mcp-servers", func() {
				for idx, server := range cat.Servers {
					w.mcpServerRow(c, p, cat, server, idx == 0, busy)
				}
			})
		})
	})
}

func (w *workbench) mcpServerRow(c *ui.Context, p palette, cat *api.MCPCatalog, server api.MCPServerInfo, first, busy bool) {
	writeScope := "project"
	if server.Scope == "user" && cat.ProjectFile == "" {
		writeScope = "user"
	}
	removeScope := "project"
	if server.Scope == "user" {
		removeScope = "user"
	}

	expMode := mcpDisplayExposure(server.Exposure)
	expanded := w.mcp.expanded[server.Name]
	hasErr := server.LiveError != "" || server.Error != ""

	dotTone := p.muted
	switch {
	case !server.Enabled:
		dotTone = p.faint.Alpha(0.45)
	case server.LiveState == "connected":
		dotTone = p.success
	case server.LiveState == "needs_auth":
		dotTone = p.warning
	case hasErr || (server.LiveState != "" && server.LiveState != "disabled"):
		dotTone = p.danger
	}

	col := ui.Column(c.Key("mcp-server:"+server.Name)).FillWidth().Padding(10, 12).Gap(6)
	if !first {
		col.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
	}
	col.Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			// Clickable main title area toggles details drawer.
			mainBtn := ui.Button(c.Key("mcp-expand:"+server.Name), "").Label(server.Name).
				Grow(1).MinWidth(0).Padding(2, 0).Border(0, ui.Transparent).Background(ui.Transparent).Justify(ui.Start)
			mainBtn.Children(func() {
				ui.Column(c).FillWidth().Gap(2).Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).MinWidth(0).Children(func() {
						ui.Box(c).Size(7, 7).Radius(4).Background(dotTone).Shrink(0)
						nameColor := p.ink
						if !server.Enabled {
							nameColor = p.muted
						}
						ui.Text(c, server.Name).FontSize(13).FontWeight(600).TextColor(nameColor).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
						kitBadge(c, w.mcpScopeBadge(server.Scope), p.faint)
						if expMode == "direct" {
							kitBadge(c, "direct", p.warning)
						} else if expMode != "codemode" {
							kitBadge(c, expMode, p.faint)
						}
					})
					sub := server.Transport
					if server.Endpoint != "" {
						sub += " · " + server.Endpoint
					} else if server.Description != "" {
						sub += " · " + server.Description
					}
					if badgeText, badgeTone, ok := w.mcpLiveStatusBadge(server); ok {
						ui.Row(c).Gap(6).AlignItems(ui.Center).MinWidth(0).Children(func() {
							ui.Text(c, badgeText).FontSize(11).FontWeight(500).TextColor(badgeTone).NoWrap().Shrink(0)
							if server.Endpoint != "" {
								ui.Text(c, server.Endpoint).Font("Menlo").FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
							}
						})
					} else {
						ui.Text(c, sub).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…")
					}
				})
			})
			if mainBtn.Clicked() {
				w.mcp.expanded[server.Name] = !expanded
			}

			// Native Switch for instant Enable / Disable (Codex / T3 Code style).
			on := server.Enabled
			sw := ui.Switch(c.Key("mcp-toggle:"+server.Name), &on).Label(w.tr("切换 ", "Toggle ") + server.Name).Disabled(busy)
			if on != server.Enabled {
				w.mcpCall("mcp.upsert", server.Name, wrapScopeEnvelope(writeScope, map[string]bool{"enabled": on}), false)
			}
			_ = sw

			// Overflow menu: exposure mode, OAuth helper, remove / reset override.
			more := iconButton(c, p, "mcp-more:"+server.Name, w.tr("操作 · ", "Actions · ")+server.Name, moreIcon).Size(24, 24).Padding(4)
			more.Disabled(busy)
			more.PopoverMenuButton(func(m *ui.Menu) {
				for _, opt := range []struct {
					id, label string
				}{
					{"codemode", w.tr("暴露模式：codemode（省 Token · 默认）", "Exposure: codemode (default)")},
					{"deferred", w.tr("暴露模式：deferred（按需检索）", "Exposure: deferred")},
					{"direct", w.tr("暴露模式：direct（直连 · 占上下文）", "Exposure: direct (high context)")},
					{"hidden", w.tr("暴露模式：hidden（隐藏）", "Exposure: hidden")},
				} {
					opt := opt
					if m.Item(opt.label).Radio(expMode == opt.id).Chosen() && expMode != opt.id {
						w.mcpCall("mcp.upsert", server.Name, wrapScopeEnvelope(writeScope, map[string]string{"exposure": opt.id}), false)
					}
				}
				if server.LiveState == "needs_auth" {
					m.Separator()
					if m.Item(w.tr("复制登录命令 (pi mcp login)", "Copy login command (pi mcp login)")).Chosen() {
						c.WriteClipboard("pi mcp login " + server.Name)
						w.setToolFeedback("mcp:"+w.selected, notice{Text: w.tr("已复制：pi mcp login ", "Copied: pi mcp login ") + server.Name, Level: noticeInfo})
					}
				}
				if server.Scope != "user" || cat.ProjectFile == "" {
					m.Separator()
					rmLabel := w.tr("移除服务", "Remove server")
					if server.Scope == "project-override" {
						rmLabel = w.tr("恢复全局继承", "Reset project override")
					}
					if m.Item(rmLabel).Chosen() {
						rmInput, _ := json.Marshal(map[string]string{"scope": removeScope})
						w.mcpCall("mcp.remove", server.Name, rmInput, false)
					}
				}
			}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomEnd, Self: ui.AnchorTopEnd})
		})

		// Always surface live tools preview right after Probe, or errors immediately.
		if len(server.Tools) > 0 && (expanded || cat.Probed) {
			ui.Text(c, w.tr("工具：", "Tools: ")+formatProbeToolsPreview(server.Tools)).Font("Menlo").FontSize(10.5).TextColor(p.muted).Wrap().Padding(0, 0, 0, 13)
		}
		if server.LiveError != "" {
			ui.Text(c, server.LiveError).FontSize(11).TextColor(p.danger).Wrap().Padding(0, 0, 0, 13)
		} else if server.Error != "" {
			ui.Text(c, server.Error).FontSize(11).TextColor(p.danger).Wrap().Padding(0, 0, 0, 13)
		}
		if expanded {
			ui.Column(c).Padding(4, 0, 2, 13).Gap(3).Children(func() {
				if server.Description != "" {
					ui.Text(c, server.Description).FontSize(11.5).TextColor(p.muted).Wrap()
				}
				meta := fmt.Sprintf("%s · exposure: %s", server.Transport, expMode)
				if len(server.ToolExposure) > 0 {
					meta += fmt.Sprintf(" (+%d toolExposure)", len(server.ToolExposure))
				}
				ui.Text(c, meta).Font("Menlo").FontSize(10.5).TextColor(p.faint)
			})
		}
	})
}

func (w *workbench) mcpAddDialog(c *ui.Context, p palette) {
	if !w.mcp.addOpen {
		return
	}
	w.ensureMCPDefaults()
	busy := w.features.busy["mcp:"+w.mcpScope()]
	hasProject := (w.mcp.catalog != nil && w.mcp.catalog.ProjectFile != "") || w.selectedWorkspacePath() != "" || w.hasThread(w.selected)
	width, _ := c.Size()

	ui.DialogBase(c, &w.mcp.addOpen, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000066"))
		panel.Width(min(float32(480), max(float32(320), width-32))).Padding(18).Radius(14).Background(p.raised).Border(1, p.line)
		ui.Column(c).FillWidth().Gap(12).Children(func() {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, serverIcon).FontSize(15).TextColor(p.accentInk)
				ui.Text(c, w.tr("添加 MCP 服务", "Add MCP server")).FontSize(14).FontWeight(600).TextColor(p.ink).Grow(1)
				if iconButton(c, p, "mcp-add-close", w.tr("关闭", "Close"), closeIcon).Size(26, 26).Padding(5).Clicked() {
					w.mcp.addOpen = false
				}
			})

			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Segmented(c.Key("mcp-add-tab"), &w.mcp.addTab, w.tr("快速添加", "Quick add"), w.tr("粘贴 JSON", "Paste JSON")).Label(w.tr("添加方式", "Add mode"))
				ui.Spacer(c)
				kitSegments(c, p, "mcp-form-scope", w.tr("保存位置", "Target scope"), []kitOption{
					{ID: "user", Label: w.tr("全局", "Global")},
					{ID: "project", Label: w.tr("当前项目", "Project"), Disabled: !hasProject},
				}, w.mcp.scopeTarget, func(id string) {
					if id == "project" && !hasProject {
						return
					}
					w.mcp.scopeTarget = id
				})
			})

			if w.mcp.addTab == 0 {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, w.tr("类型", "Type")).FontSize(12).TextColor(p.muted).NoWrap()
					ui.Segmented(c.Key("mcp-add-transport"), &w.mcp.transport, "HTTP URL", w.tr("本地命令", "Stdio")).Label(w.tr("传输类型", "Transport"))
				})
				kitField(ui.TextInput(c.Key("mcp-name"), &w.mcp.name).Label(w.tr("MCP 服务名", "MCP server name")).Placeholder("docs / filesystem").FillWidth(), p, false)
				if w.mcp.transport == 0 {
					kitField(ui.TextInput(c.Key("mcp-endpoint"), &w.mcp.endpoint).Label(w.tr("服务 URL", "Server URL")).Placeholder("https://example.com/mcp").FillWidth(), p, false)
				} else {
					kitField(ui.TextInput(c.Key("mcp-endpoint"), &w.mcp.endpoint).Label(w.tr("启动命令", "Command")).Placeholder("npx -y @modelcontextprotocol/server-filesystem .").FillWidth(), p, false)
				}
				kitField(ui.TextInput(c.Key("mcp-description"), &w.mcp.description).Label(w.tr("一句话用途（可选）", "Description (optional)")).Placeholder(w.tr("例如：搜索项目文档或调用代码图谱", "e.g. Search docs or query code graph")).FillWidth(), p, false)
			} else {
				kitField(ui.TextInput(c.Key("mcp-name"), &w.mcp.name).Label(w.tr("MCP 服务名（粘贴 mcpServers 可留空）", "Server name (optional for mcpServers snippet)")).Placeholder("gemini-ego").FillWidth(), p, false)
				ui.TextArea(c.Key("mcp-config"), &w.mcp.config).Label(w.tr("服务 JSON 配置", "Server JSON configuration")).
					Placeholder(`{"mcpServers": {"docs": {"url": "https://example.com/mcp"}}}`).Lines(5, 10).FillWidth().Font("Menlo").FontSize(12)
			}

			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, w.tr("默认 codemode 沙盒（0 初始 schema Token）", "Defaults to codemode (0 initial schema tokens)")).FontSize(11).TextColor(p.faint).Grow(1).MinWidth(0).Wrap()
				if outlineButton(c, p, "mcp-add-cancel", w.tr("取消", "Cancel")).Clicked() {
					w.mcp.addOpen = false
				}
				saveLabel := w.tr("保存", "Save")
				canSave := !busy && w.mcp.catalog != nil && w.mcp.scope == w.mcpScope()
				if w.mcp.addTab == 0 {
					canSave = canSave && strings.TrimSpace(w.mcp.name) != "" && strings.TrimSpace(w.mcp.endpoint) != ""
				} else {
					canSave = canSave && strings.TrimSpace(w.mcp.config) != ""
				}
				saveBtn := primaryAction(c, p, "mcp-save", saveLabel)
				saveBtn.Disabled(!canSave)
				if saveBtn.Clicked() {
					if w.mcp.addTab == 0 {
						payload, ok := buildQuickServerUpsertInput(w.mcp.scopeTarget, w.mcp.transport, w.mcp.endpoint, w.mcp.description)
						if !ok {
							w.setToolFeedback("mcp:"+w.selected, notice{Text: w.tr("请填写有效的 URL 或启动命令", "Enter a valid URL or command"), Level: noticeError})
						} else {
							w.mcpCall("mcp.upsert", strings.TrimSpace(w.mcp.name), payload, true)
						}
					} else {
						resolvedName, mergedInput, ok := buildServerUpsertInput(w.mcp.scopeTarget, w.mcp.name, w.mcp.description, w.mcp.exposure, w.mcp.config)
						if !ok {
							w.setToolFeedback("mcp:"+w.selected, notice{Text: w.tr("请输入有效的 JSON 对象（单服务需填服务名）", "Enter a valid JSON object"), Level: noticeError})
						} else {
							w.mcpCall("mcp.upsert", resolvedName, mergedInput, true)
						}
					}
				}
			})
		})
	})
}
