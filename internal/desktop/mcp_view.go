package desktop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type mcpPanelState struct {
	catalog                             *api.MCPCatalog
	scope, scopeTarget                  string
	name, description, exposure, config string
}

func (w *workbench) mcpScope() string {
	return w.environmentKey + "\x00" + w.selected + "\x00" + w.selectedWorkspacePath()
}

func (w *workbench) ensureMCPDefaults() {
	if w.mcp.exposure == "" {
		w.mcp.exposure = "codemode"
	}
	hasWorkspace := w.selectedWorkspacePath() != "" || w.hasThread(w.selected)
	if w.mcp.scopeTarget == "" {
		if hasWorkspace {
			w.mcp.scopeTarget = "project"
		} else {
			w.mcp.scopeTarget = "user"
		}
	}
	if !hasWorkspace && w.mcp.scopeTarget == "project" {
		w.mcp.scopeTarget = "user"
	}
}

func (w *workbench) loadMCP() { w.mcpCall("mcp.list", "", nil, false) }

func (w *workbench) mcpCall(action, name string, input json.RawMessage, clearForm bool) {
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
		if clearForm && action == "mcp.upsert" && w.mcp.name == name {
			w.mcp.name = ""
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
		return w.tr("项目覆盖", "Project override")
	case "project":
		return w.tr("项目", "Project")
	default:
		return w.tr("用户", "User")
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
		return w.tr("需 OAuth 登录", "Needs sign-in"), ui.Hex("#D97706"), true
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

func buildServerUpsertInput(scopeTarget, description, exposure, rawJSON string) (json.RawMessage, bool) {
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawJSON), &rawMap); err != nil || rawMap == nil {
		return nil, false
	}
	if _, hasWrapper := rawMap["mcpServers"]; !hasWrapper {
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
		return nil, false
	}
	if scopeTarget == "user" {
		wrapped, _ := json.Marshal(map[string]any{
			"scope":  "user",
			"config": json.RawMessage(mergedConfig),
		})
		return wrapped, true
	}
	return json.RawMessage(mergedConfig), true
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
	ui.Scroll(c.Key("mcp-panel")).Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).Gap(16).Children(func() {
			settingsHeading(
				c, p,
				"MCP · Codemode",
				w.tr(
					"由 Pi 原生执行（stdio / HTTP + QuickJS codemode 沙盒），默认不注入全量工具 schema。",
					"Managed natively by Pi (stdio / HTTP + QuickJS codemode sandbox) so tool schemas stay out of prompt context.",
				),
				func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						probeBtn := kitButton(c, p, "mcp-probe", w.tr("检测连接", "Probe"), controlOutline, controlSM)
						probeBtn.Disabled(busy)
						if probeBtn.Clicked() {
							w.mcpCall("mcp.probe", "", nil, false)
						}
						if iconButton(c, p, "mcp-refresh", w.tr("刷新 MCP 配置", "Refresh MCP configuration"), refreshIcon).Size(26, 26).Disabled(busy).Clicked() {
							w.loadMCP()
						}
					})
				},
			)

			if w.mcp.catalog != nil && w.mcp.scope == w.mcpScope() {
				cat := w.mcp.catalog
				if !cat.CodemodeDefault {
					ui.Text(c, w.tr("当前配置含 autoEnableCodemode: false，Vantry 运行时仍会自动注入 builtin:codemode。", "Config sets autoEnableCodemode: false; Vantry workers still enable builtin:codemode.")).FontSize(11).TextColor(p.warning).Wrap()
				}
				if cat.ProjectFile != "" {
					settingsGroup(c, p, "mcp-trust-card", func() {
						ui.Row(c).FillWidth().Padding(10, 12).Gap(10).AlignItems(ui.Center).Children(func() {
							ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
								trustLabel := w.tr("项目未信任 · Pi RPC 会跳过 .pi/mcp.json", "Project untrusted · Pi RPC skips .pi/mcp.json")
								trustColor := p.warning
								if cat.ProjectTrusted {
									trustLabel = w.tr("项目已信任 · Pi 将加载项目 MCP", "Project trusted · Pi loads project MCP")
									trustColor = p.success
								}
								ui.Text(c, trustLabel).FontSize(12).FontWeight(600).TextColor(trustColor).Wrap()
								ui.Text(c, cat.ProjectFile).FontSize(10.5).TextColor(p.faint).Wrap()
							})
							btnLabel := w.tr("信任此项目", "Trust project")
							if cat.ProjectTrusted {
								btnLabel = w.tr("取消信任", "Untrust")
							}
							trustBtn := kitButton(c, p, "mcp-trust-toggle", btnLabel, controlOutline, controlSM)
							trustBtn.Disabled(busy)
							if trustBtn.Clicked() {
								b, _ := json.Marshal(map[string]bool{"trusted": !cat.ProjectTrusted})
								w.mcpCall("mcp.trust", "", b, false)
							}
						})
					})
				} else if cat.UserFile != "" {
					ui.Text(c, w.tr("当前未选择项目目录；仅管理全局用户配置：", "No project selected; managing global user config: ")+cat.UserFile).FontSize(11).TextColor(p.muted).Wrap()
				}

				if cat.Probed {
					ui.Text(c, w.tr("以下包含本次 pi mcp list 实测连接状态；需要 OAuth 的远程服务请在终端运行 pi mcp login <服务名>。", "Live status from pi mcp list is shown below. For OAuth servers, run pi mcp login <server> in terminal.")).FontSize(11).TextColor(p.muted).Wrap()
				} else {
					ui.Text(c, w.tr("这是配置视图；点击右上角「检测连接」才会真实拉起服务并发现工具。", "Configuration view only. Click Probe to test connections and list discovered tools.")).FontSize(11).TextColor(p.muted).Wrap()
				}

				settingsGroup(c, p, "mcp-servers", func() {
					for _, server := range cat.Servers {
						server := server
						writeScope := "project"
						if server.Scope == "user" && cat.ProjectFile == "" {
							writeScope = "user"
						}
						removeScope := "project"
						if server.Scope == "user" {
							removeScope = "user"
						}

						ui.Column(c.Key("mcp-server:"+server.Name)).Padding(10, 12).Gap(8).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
							ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
								ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
									ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
										ui.Text(c, server.Name).FontSize(13).Bold().TextColor(p.ink)
										kitBadge(c, w.mcpScopeBadge(server.Scope), p.muted)
										if badgeText, badgeTone, ok := w.mcpLiveStatusBadge(server); ok {
											settingsStatus(c, nil, badgeText, badgeTone)
										}
									})
									expLabel := mcpDisplayExposure(server.Exposure)
									if len(server.ToolExposure) > 0 {
										expLabel = fmt.Sprintf("%s (+%d toolExposure)", expLabel, len(server.ToolExposure))
									}
									ui.Text(c, server.Transport+" · "+expLabel).FontSize(11).TextColor(p.muted)
									if server.Endpoint != "" {
										ui.Text(c, server.Endpoint).FontSize(11).TextColor(p.faint).Wrap()
									}
									if server.Description != "" {
										ui.Text(c, server.Description).FontSize(11).TextColor(p.muted).Wrap()
									}
									if len(server.Tools) > 0 {
										ui.Text(c, "Tools: "+formatProbeToolsPreview(server.Tools)).FontSize(10.5).TextColor(p.faint).Wrap()
									}
									if server.LiveError != "" {
										ui.Text(c, server.LiveError).FontSize(11).TextColor(p.danger).Wrap()
									} else if server.Error != "" {
										ui.Text(c, server.Error).FontSize(11).TextColor(p.danger).Wrap()
									}
								})
							})

							ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
								for _, exp := range []string{"codemode", "deferred", "direct", "hidden"} {
									exp := exp
									chosen := mcpDisplayExposure(server.Exposure) == exp
									seg := ui.Button(c.Key("mcp-exp:"+server.Name+":"+exp), "").Label(exp+" · "+server.Name).
										Role(ui.RoleRadio).AccessSelected(chosen).Height(22).Padding(0, 7).Radius(6).Border(1, p.lineSoft)
									styleSegment(seg, p, chosen)
									seg.Children(func() { ui.Text(c, exp).FontSize(10.5).FontWeight(500).NoWrap() })
									seg.Disabled(busy)
									if seg.Clicked() && !chosen {
										w.mcpCall("mcp.upsert", server.Name, wrapScopeEnvelope(writeScope, map[string]string{"exposure": exp}), false)
									}
								}
								ui.Spacer(c)
								toggleLabel := w.tr("停用", "Disable")
								if cat.ProjectFile != "" && server.Scope != "user" {
									toggleLabel = w.tr("在项目停用", "Disable here")
								}
								if !server.Enabled {
									toggleLabel = w.tr("启用", "Enable")
									if cat.ProjectFile != "" && server.Scope != "user" {
										toggleLabel = w.tr("在项目启用", "Enable here")
									}
								}
								toggleBtn := ui.Button(c.Key("mcp-toggle:"+server.Name), toggleLabel).Label(toggleLabel+" · "+server.Name).
									Height(24).Padding(0, 8).Radius(6).Border(1, p.lineSoft).Background(p.soft).TextColor(p.ink)
								toggleBtn.Disabled(busy)
								if toggleBtn.Clicked() {
									w.mcpCall("mcp.upsert", server.Name, wrapScopeEnvelope(writeScope, map[string]bool{"enabled": !server.Enabled}), false)
								}
								if server.Scope != "user" || cat.ProjectFile == "" {
									rmLabel := w.tr("移除", "Remove")
									if server.Scope == "project-override" {
										rmLabel = w.tr("恢复继承", "Reset override")
									}
									rmBtn := ui.Button(c.Key("mcp-remove:"+server.Name), rmLabel).Label(rmLabel+" · "+server.Name).
										Height(24).Padding(0, 8).Radius(6).Border(1, p.lineSoft).Background(ui.Transparent).TextColor(p.muted)
									rmBtn.Disabled(busy)
									if rmBtn.Clicked() {
										rmInput, _ := json.Marshal(map[string]string{"scope": removeScope})
										w.mcpCall("mcp.remove", server.Name, rmInput, false)
									}
								}
							})
						})
					}
					if len(cat.Servers) == 0 {
						ui.Text(c, w.tr("当前尚无 MCP 服务。", "No MCP servers configured.")).Padding(12).TextColor(p.muted)
					}
				})
			}

			settingsHeading(
				c, p,
				w.tr("添加或更新服务", "Add or update a server"),
				w.tr("默认写入 exposure: codemode，支持单服务对象或 mcpServers 片段，已有服务按字段合并。", "Defaults to exposure: codemode. Accepts one server JSON or an mcpServers wrapper; updates merge fields."),
				nil,
			)

			hasProject := w.mcp.catalog != nil && w.mcp.catalog.ProjectFile != "" || w.selectedWorkspacePath() != "" || w.hasThread(w.selected)
			kitSegments(c, p, "mcp-form-scope", w.tr("保存位置", "Target scope"), []kitOption{
				{ID: "project", Label: w.tr("当前项目", "This project"), Disabled: !hasProject},
				{ID: "user", Label: w.tr("全局用户", "Global user")},
			}, w.mcp.scopeTarget, func(id string) {
				if id == "project" && !hasProject {
					return
				}
				w.mcp.scopeTarget = id
			})

			kitField(ui.TextInput(c.Key("mcp-name"), &w.mcp.name).Label(w.tr("MCP 服务名", "MCP server name")).Placeholder("gemini-ego").FillWidth(), p, false)
			kitField(ui.TextInput(c.Key("mcp-description"), &w.mcp.description).Label(w.tr("用途说明（Codemode 搜索提示）", "Description (for codemode discovery)")).Placeholder(w.tr("例如：搜索文档、分析网页视频、访问仓库图谱", "e.g. Search docs, inspect pages, query code graph")).FillWidth(), p, false)

			kitSegments(c, p, "mcp-form-exposure", w.tr("工具暴露模式", "Tool exposure"), []kitOption{
				{ID: "codemode", Label: "codemode"},
				{ID: "deferred", Label: "deferred"},
				{ID: "direct", Label: "direct"},
				{ID: "hidden", Label: "hidden"},
			}, w.mcp.exposure, func(id string) {
				w.mcp.exposure = id
			})

			ui.TextArea(c.Key("mcp-config"), &w.mcp.config).Label(w.tr("服务 JSON 配置", "Server JSON configuration")).Lines(4, 10).FillWidth().FontSize(12)
			saveLabel := w.tr("保存到当前项目", "Save to this project")
			if w.mcp.scopeTarget == "user" {
				saveLabel = w.tr("保存到全局用户", "Save to global user")
			}
			button := primaryAction(c, p, "mcp-save", saveLabel)
			button.Disabled(busy || strings.TrimSpace(w.mcp.name) == "" || strings.TrimSpace(w.mcp.config) == "" || w.mcp.catalog == nil || w.mcp.scope != w.mcpScope())
			if button.Clicked() {
				mergedInput, ok := buildServerUpsertInput(w.mcp.scopeTarget, w.mcp.description, w.mcp.exposure, w.mcp.config)
				if !ok {
					w.setToolFeedback("mcp:"+w.selected, notice{Text: w.tr("请输入有效的 JSON 对象", "Enter a valid JSON object"), Level: noticeError})
				} else {
					w.mcpCall("mcp.upsert", strings.TrimSpace(w.mcp.name), mergedInput, true)
				}
			}
			ui.Text(c, w.tr("Gemini、Ego、Rill 等能力通过外部 MCP 接入，由 Pi 在沙盒中按需调用 tools.mcp__*；凭据留在宿主机器，不会明文回显。", "External capabilities connect as MCP servers invoked via tools.mcp__* inside Pi's codemode sandbox; secrets stay on the host.")).FontSize(11).TextColor(p.faint).Wrap()
		})
	})
}
