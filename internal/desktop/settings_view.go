package desktop

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/pkg/api"
)

// dialogFrame is the shared chrome of the Usage and smaller dialogs: a
// raised card with a titled header row and a scrolling body.
func (w *workbench) dialogFrame(c *ui.Context, p palette, open *bool, key, title, closeLabel string, icon *ui.SVG, preferred float32, body func(width float32)) {
	width, height := c.Size()
	panelWidth := min(preferred, width-32)
	ui.DialogBase(c, open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#0000008C"))
		panel.Width(panelWidth).MaxHeight(height-48).Radius(16).Background(p.raised).Border(1, p.line).Label(title).
			Shadow(0, 24, 64, 0, ui.Hex("#00000059"))
		ui.Column(c).FillWidth().Children(func() {
			ui.Row(c).Height(52).Padding(0, 12, 0, 20).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icon).FontSize(16).TextColor(p.muted)
				ui.Text(c, title).FontSize(15).FontWeight(600).Grow(1)
				if iconButton(c, p, "close-"+key, closeLabel, closeIcon).Size(28, 28).Padding(6).Clicked() {
					*open = false
				}
			})
			ui.Scroll(c.Key(key + "-body")).MaxHeight(height - 110).Children(func() {
				ui.Column(c).Padding(4, 20, 20).Gap(18).Children(func() { body(panelWidth - 40) })
			})
		})
	})
}

// settingsPages define the settings window's
// order: everyday preferences first, connection and version last.
type settingsPage struct {
	id     string
	zh, en string
	icon   *ui.SVG
}

var settingsPages = []settingsPage{
	{"general", "通用", "General", gearIcon},
	{"appearance", "外观", "Appearance", lucide("palette")},
	{"agent", "Agent 与权限", "Agents & permissions", agentsIcon},
	{"providers", "提供方", "Providers", agentsIcon},
	{"environments", "环境", "Environments", serverIcon},
	{"about", "关于与更新", "About & updates", lucide("info")},
}

// settingsDialog uses a navigation column of
// sections beside one scrolling page of grouped rows.
func (w *workbench) settingsDialog(c *ui.Context, p palette) {
	if !w.widgets.settingsOpen {
		return
	}
	before := struct {
		enter, manual      bool
		approval, provider string
		themeSet, dark     bool
		preset             string
	}{w.widgets.sendWithEnter, w.widgets.manualDelivery, w.defaults.ApprovalPolicy, w.defaults.ProviderID, w.widgets.themeSet, w.widgets.dark, w.widgets.themePreset}
	page := w.widgets.settingsPage
	if page == "" {
		page = "general"
	}
	if page == "connections" {
		page = "environments"
	}
	width, height := c.Size()
	panelWidth, panelHeight := min(940, width-32), min(700, height-40)
	compact := panelWidth < 680
	title := w.tr("设置", "Settings")
	ui.DialogBase(c, &w.widgets.settingsOpen, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#0000008C"))
		panel.Width(panelWidth).Height(panelHeight).Radius(16).Background(pageFill(p)).Border(1, p.line).Label(title).
			Shadow(0, 24, 64, 0, ui.Hex("#00000066"))
		var layout ui.Element
		if compact {
			layout = ui.Column(c).FillWidth().Height(panelHeight - 2).AlignItems(ui.Stretch)
		} else {
			layout = ui.Row(c).FillWidth().Height(panelHeight - 2).AlignItems(ui.Stretch)
		}
		layout.Children(func() {
			w.settingsNav(c, p, page, compact)
			ui.Column(c).Grow(1).MinWidth(0).MinHeight(0).Children(func() {
				current := settingsPages[0]
				for _, candidate := range settingsPages {
					if candidate.id == page {
						current = candidate
					}
				}
				ui.Row(c).Height(56).Padding(0, 14, 0, 28).Gap(10).AlignItems(ui.Center).Shrink(0).Children(func() {
					ui.Text(c, w.tr(current.zh, current.en)).FontSize(17).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine()
					ui.Text(c, "Esc").FontSize(11).TextColor(p.faint)
					if iconButton(c, p, "close-settings", w.tr("关闭设置", "Close settings"), closeIcon).Size(28, 28).Padding(6).Clicked() {
						w.widgets.settingsOpen = false
					}
				})
				ui.Scroll(c.Key("settings-body:" + page)).Grow(1).Children(func() {
					ui.Column(c).Padding(4, 28, 32).Gap(28).MaxWidth(720).Children(func() {
						switch page {
						case "appearance":
							w.settingsAppearance(c, p)
						case "agent":
							w.settingsAgent(c, p)
						case "providers":
							w.settingsProviders(c, p)
						case "environments":
							w.settingsEnvironments(c, p)
						case "about":
							w.settingsAbout(c, p)
						default:
							w.settingsGeneral(c, p)
						}
					})
				})
			})
		})
	})
	after := struct {
		enter, manual      bool
		approval, provider string
		themeSet, dark     bool
		preset             string
	}{w.widgets.sendWithEnter, w.widgets.manualDelivery, w.defaults.ApprovalPolicy, w.defaults.ProviderID, w.widgets.themeSet, w.widgets.dark, w.widgets.themePreset}
	if before != after {
		w.saveState()
	}
}

func (w *workbench) settingsNav(c *ui.Context, p palette, page string, compact bool) {
	pick := func(entry settingsPage) {
		if navItem(c, p, "settings-nav:"+entry.id, w.tr(entry.zh, entry.en), entry.icon, entry.id == page).Clicked() {
			w.widgets.settingsPage = entry.id
		}
	}
	if compact {
		ui.ScrollHorizontal(c.Key("settings-nav")).FillWidth().Shrink(0).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
			ui.Row(c).Padding(10, 12).Gap(4).Children(func() {
				for _, entry := range settingsPages {
					ui.Box(c.Key("settings-nav-cell:" + entry.id)).Width(150).Children(func() { pick(entry) })
				}
			})
		})
		return
	}
	ui.Column(c.Key("settings-nav")).Width(216).Shrink(0).Padding(16, 10).Gap(2).Background(navFill(p)).
		BorderWidth(0, 1, 0, 0).BorderColor(p.lineSoft).Role(ui.RoleNavigation).Label(w.tr("设置分区", "Settings sections")).Children(func() {
		ui.Row(c).Height(36).Padding(0, 10).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, w.tr("设置", "Settings")).FontSize(13).FontWeight(600).TextColor(p.ink)
		})
		for _, entry := range settingsPages {
			pick(entry)
		}
		ui.Spacer(c)
		if w.defaults.Version != "" {
			ui.Text(c, "Vantry "+versionLabel(w.defaults.Version)).FontSize(11).TextColor(p.faint).Padding(0, 10)
		}
	})
}

func (w *workbench) settingsGeneral(c *ui.Context, p palette) {
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr("语言与输入", "Language & input"), "", nil)
		settingsGroup(c, p, "settings-group-input", func() {
			settingsRow(c, p, "settings-row-locale", true, w.tr3("界面语言", "Interface language", "表示言語"), w.tr3("切换后立即生效，包括菜单、提示与自动提交摘要语言。", "Applies immediately, including menus, hints, and auto commit summaries.", "メニュー・ヒント・自動コミット要約の言語にすぐ反映されます。"), func() {
				labels := []string{"简体中文", "English", "日本語"}
				chosen := localeDisplayLabel(w.widgets.locale)
				ui.Select(c.Key("settings-locale"), &chosen, labels).Label(w.tr3("界面语言", "Interface language", "表示言語")).MinWidth(160)
				if next := localeCodeFromLabel(chosen); next != normalizeLocale(w.widgets.locale) {
					w.setLocale(next)
				}
			})
			settingsRow(c, p, "settings-row-send", false, w.tr("发送快捷键", "Send shortcut"), w.tr("运行中按住 ⌘ / Ctrl 可在排队与调整本轮之间临时切换。", "While running, hold ⌘ / Ctrl to switch between queue and steer once."), func() {
				mode := 0
				if w.widgets.sendWithEnter {
					mode = 1
				}
				ui.Segmented(c.Key("settings-send"), &mode, w.tr("⌘ Enter 发送", "⌘ Enter sends"), w.tr("Enter 发送", "Enter sends")).Label(w.tr("发送快捷键", "Send shortcut"))
				w.widgets.sendWithEnter = mode == 1
			})
		})
	})
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr("子任务", "Subtasks"), "", nil)
		settingsGroup(c, p, "settings-group-subtasks", func() {
			settingsRow(c, p, "settings-row-continue", true, w.tr("子任务自动续行", "Auto-continue subtasks"), w.tr("子任务全部完成后，父任务自动带着结果继续。", "When subtasks finish, the parent continues with their results."), func() {
				auto := !w.widgets.manualDelivery
				ui.Switch(c.Key("settings-auto-continue"), &auto).Label(w.tr("子任务自动续行", "Auto-continue subtasks"))
				w.widgets.manualDelivery = !auto
			})
		})
	})
}

func (w *workbench) settingsAppearance(c *ui.Context, p palette) {
	currentPreset := normalizeThemePreset(w.widgets.themePreset)

	// Multi-theme swatch grid + System/Dark/Light segmented mode (Graphite default)
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr3("主题风格", "Theme & appearance", "テーマと外観"),
			w.tr3("默认采用石墨配色，支持浅色与深色模式，切换后立即生效。", "Graphite is the default palette; all palettes support light and dark mode and apply immediately.", "標準はグラファイト配色で、ライト・ダーク両対応です。"), nil)
		ui.Row(c.Key("settings-theme-swatches")).FillWidth().Gap(10).Wrap().Children(func() {
			for _, spec := range themePresets {
				spec := spec
				swatchPal := paletteFor(spec.id, p.dark)
				chosen := currentPreset == spec.id
				name := w.tr3(spec.zh, spec.en, spec.ja)
				card := ui.Button(c.Key("theme-swatch:"+spec.id), "").Label(name).Role(ui.RoleRadio).Checked(chosen).
					Width(120).Padding(8).Gap(7).Radius(12).Background(p.raised).Border(1, p.lineSoft)
				if chosen {
					card.Border(1.5, p.accent).Background(p.accent.Alpha(0.08))
				} else if card.Hovered() {
					card.BorderColor(p.line).Background(p.rowHover)
				}
				card.Children(func() {
					ui.Column(c).FillWidth().Gap(6).Children(func() {
						// Mini preview window painted with swatch palette
						ui.Box(c.Key("theme-swatch-preview:"+spec.id)).FillWidth().Height(54).Radius(7).Draw(func(pt *ui.Painter, r ui.Rect) {
							pt.Clip(r, 7, func() {
								pt.Fill(r, swatchPal.canvas, 0)
								sbW := r.W * 0.30
								pt.Fill(ui.Rect{X: r.X, Y: r.Y, W: sbW, H: r.H}, swatchPal.sidebar, 0)
								pt.Line(r.X+sbW, r.Y, r.X+sbW, r.Y+r.H, 1, swatchPal.lineSoft)
								// Sidebar active row & dot
								pt.Fill(ui.Rect{X: r.X + 4, Y: r.Y + 8, W: sbW - 8, H: 7}, swatchPal.selected, 3)
								pt.Fill(ui.Rect{X: r.X + 4, Y: r.Y + 19, W: sbW - 12, H: 4}, swatchPal.faint.Alpha(0.4), 2)
								pt.Fill(ui.Rect{X: r.X + 4, Y: r.Y + 27, W: sbW - 10, H: 4}, swatchPal.faint.Alpha(0.3), 2)
								// Canvas mini messages & composer card with accent CTA
								cx := r.X + sbW + 7
								cw := r.W - sbW - 14
								pt.Fill(ui.Rect{X: cx, Y: r.Y + 8, W: cw * 0.62, H: 5}, swatchPal.ink.Alpha(0.72), 2.5)
								pt.Fill(ui.Rect{X: cx, Y: r.Y + 17, W: cw * 0.82, H: 4}, swatchPal.muted.Alpha(0.42), 2)
								// Mini composer card + accent button
								pt.Fill(ui.Rect{X: cx, Y: r.Y + 29, W: cw, H: 18}, swatchPal.raised, 5)
								pt.Stroke(ui.Rect{X: cx, Y: r.Y + 29, W: cw, H: 18}, swatchPal.line, 5, 1)
								pt.Fill(ui.Rect{X: cx + cw - 13, Y: r.Y + 33, W: 9, H: 9}, swatchPal.accent, 4.5)
								pt.Fill(ui.Rect{X: cx + 5, Y: r.Y + 36, W: cw * 0.45, H: 3.5}, swatchPal.faint.Alpha(0.45), 1.5)
							})
							pt.Stroke(ui.Rect{X: r.X + 0.5, Y: r.Y + 0.5, W: r.W - 1, H: r.H - 1}, swatchPal.line, 7, 1)
						})
						ui.Row(c).FillWidth().Gap(5).AlignItems(ui.Center).Children(func() {
							ui.Box(c).Size(8, 8).Radius(4).Background(swatchPal.accent).Shrink(0)
							weight := 500
							if chosen {
								weight = 600
							}
							ui.Text(c, name).FontSize(12).FontWeight(weight).TextColor(p.ink).Grow(1).SingleLine()
							if chosen {
								ui.Icon(c, checkIcon).FontSize(12).TextColor(p.accentInk).Shrink(0)
							}
						})
					})
				})
				if card.Clicked() {
					w.widgets.themePreset = spec.id
				}
			}
		})
		settingsGroup(c, p, "settings-group-theme", func() {
			settingsRow(c, p, "settings-row-theme", true, w.tr("外观", "Appearance"), w.tr("跟随系统时随 macOS 的浅色与深色自动切换。", "System follows macOS light and dark mode."), func() {
				theme := 0
				if w.widgets.themeSet {
					theme = 2
					if w.widgets.dark {
						theme = 1
					}
				}
				ui.Segmented(c.Key("settings-theme"), &theme, w.tr("跟随系统", "System"), w.tr("深色", "Dark"), w.tr("浅色", "Light")).Label(w.tr("外观主题", "Theme"))
				switch theme {
				case 0:
					w.widgets.themeSet = false
				case 1:
					w.widgets.themeSet, w.widgets.dark = true, true
				case 2:
					w.widgets.themeSet, w.widgets.dark = true, false
				}
			})
		})
	})
}

func (w *workbench) settingsAgent(c *ui.Context, p palette) {
	w.ensureSandboxLoaded()
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr("新会话默认工具权限", "Default tool permissions"), w.tr("应用于之后新建的会话；已有会话保持创建时的权限。", "Applies to threads created from now on; existing threads keep theirs."), nil)
		settingsGroup(c, p, "settings-group-approval", func() {
			ui.Column(c).FillWidth().Role(ui.RoleRadioGroup).Label(w.tr("工具审批", "Tool approval")).Children(func() {
				for i, tier := range approvalTiers {
					tier := tier
					chosen := tier.policy == w.defaults.ApprovalPolicy
					row := ui.Button(c.Key("tier:"+tier.policy), "").Label(w.approvalLabel(tier)).Role(ui.RoleRadio).Checked(chosen).FillWidth().
						MinHeight(56).Padding(12, 16).Gap(14).Radius(0).Border(0, p.line).Background(ui.Transparent).TextColor(p.ink)
					if i > 0 {
						row.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
					}
					if row.Hovered() && !chosen {
						row.Background(p.rowHover)
					}
					row.Children(func() {
						ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
							ui.Text(c, w.approvalLabel(tier)).FontSize(13).FontWeight(500).SingleLine()
							ui.Text(c, w.approvalDetail(tier)).FontSize(12).TextColor(p.muted).Wrap()
						})
						ring := p.line
						if chosen {
							ring = p.accent
						}
						ui.Row(c).Size(16, 16).Radius(8).Border(1.5, ring).Shrink(0).Justify(ui.Center).AlignItems(ui.Center).Children(func() {
							if chosen {
								ui.Box(c).Size(8, 8).Radius(4).Background(p.accent)
							}
						})
					})
					if row.Clicked() {
						w.defaults.ApprovalPolicy = tier.policy
					}
				}
			})
		})
	})
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr("工作区沙箱隔离 (macOS Seatbelt)", "Workspace sandbox (macOS Seatbelt)"),
			w.tr("将命令写入权限限制在当前工作区与会话缓存目录内，锁定 .git/hooks 与 .git/config。", "Confines writes to the active workspace and session directories while protecting .git/hooks and .git/config."), nil)
		settingsGroup(c, p, "settings-group-sandbox", func() {
			detail := w.tr("开启后通过内核 sandbox-exec 隔离写入越界，无需 Docker 容器开销；将在下次新建会话轮次生效。", "Uses macOS Seatbelt to block out-of-workspace writes without container overhead; applies to new turns.")
			if w.widgets.sandbox.envOverride != "" {
				detail = fmt.Sprintf(w.tr("当前由环境变量 VANTRY_SANDBOX_MODE=%s 接管，界面开关已锁定。", "Overridden by VANTRY_SANDBOX_MODE=%s."), w.widgets.sandbox.envOverride)
			} else if !w.widgets.sandbox.supported {
				detail = w.tr("当前平台未检测到 /usr/bin/sandbox-exec，工作区沙箱不可用。", "Seatbelt (/usr/bin/sandbox-exec) is unavailable on this platform.")
			}
			settingsRow(c, p, "settings-row-sandbox", false, w.tr("启用工作区写隔离", "Isolate workspace writes"), detail, func() {
				enabled := w.widgets.sandbox.mode == string(hostprocess.SandboxWorkspaceWrite)
				before := enabled
				disabled := !w.widgets.sandbox.supported || w.widgets.sandbox.envOverride != "" || w.widgets.sandbox.saving
				sw := ui.Switch(c.Key("settings-sandbox"), &enabled).Label(w.tr("启用工作区写隔离", "Isolate workspace writes"))
				if disabled {
					sw.Disabled(true)
				}
				if enabled != before && !disabled {
					next := string(hostprocess.SandboxOff)
					if enabled {
						next = string(hostprocess.SandboxWorkspaceWrite)
					}
					w.setSandboxMode(next)
				}
			})
		})
		if w.widgets.sandbox.err != "" {
			ui.Text(c, w.widgets.sandbox.err).FontSize(11).TextColor(p.danger).Wrap()
		}
	})
}

type sandboxState struct {
	mode        string
	supported   bool
	envOverride string
	loaded      bool
	loading     bool
	saving      bool
	err         string
}

func (w *workbench) ensureSandboxLoaded() {
	if w.widgets.sandbox.loaded || w.widgets.sandbox.loading {
		return
	}
	if _, ok := w.service.(api.WorkbenchService); !ok {
		w.widgets.sandbox.loaded = true
		if w.widgets.sandbox.mode == "" {
			w.widgets.sandbox.mode = string(hostprocess.SandboxOff)
		}
		if !w.widgets.sandbox.supported {
			w.widgets.sandbox.supported = hostprocess.SandboxSupported()
		}
		return
	}
	w.widgets.sandbox.loading = true
	w.workbenchCall("sandbox:get", api.WorkbenchRequest{Action: "sandbox.get"}, func(response api.WorkbenchResponse, err error) {
		w.widgets.sandbox.loading = false
		w.widgets.sandbox.loaded = true
		if err != nil {
			w.widgets.sandbox.err = describe(w.tr("读取沙箱配置失败", "Could not load sandbox status"), err)
			return
		}
		var payload struct {
			Mode        string `json:"mode"`
			Supported   bool   `json:"supported"`
			EnvOverride string `json:"envOverride"`
		}
		if json.Unmarshal([]byte(response.Output), &payload) == nil {
			w.widgets.sandbox.mode = payload.Mode
			w.widgets.sandbox.supported = payload.Supported
			w.widgets.sandbox.envOverride = payload.EnvOverride
		}
	})
}

func (w *workbench) setSandboxMode(mode string) {
	if w.widgets.sandbox.saving {
		return
	}
	prev := w.widgets.sandbox.mode
	w.widgets.sandbox.mode = mode
	w.widgets.sandbox.err = ""
	if _, ok := w.service.(api.WorkbenchService); !ok {
		return
	}
	w.widgets.sandbox.saving = true
	w.workbenchCall("sandbox:set", api.WorkbenchRequest{Action: "sandbox.set", Message: mode}, func(response api.WorkbenchResponse, err error) {
		w.widgets.sandbox.saving = false
		if err != nil {
			w.widgets.sandbox.mode = prev
			w.widgets.sandbox.err = describe(w.tr("保存沙箱设置失败", "Could not update sandbox mode"), err)
			return
		}
		var payload struct {
			Mode        string `json:"mode"`
			Supported   bool   `json:"supported"`
			EnvOverride string `json:"envOverride"`
		}
		if json.Unmarshal([]byte(response.Output), &payload) == nil {
			w.widgets.sandbox.mode = payload.Mode
			w.widgets.sandbox.supported = payload.Supported
			w.widgets.sandbox.envOverride = payload.EnvOverride
		}
	})
}

func (w *workbench) settingsAbout(c *ui.Context, p palette) {
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr("应用更新", "Updates"), w.tr("从 GitHub Releases 检查新版本，优先下载增量包。", "Check GitHub Releases for new versions; delta packages download first."), nil)
		w.updateSection(c, p)
	})
}

// usageSummary mirrors Core's usage.summary payload.
type usageSummary struct {
	Threads          int          `json:"threads"`
	Runs             int          `json:"runs"`
	ActiveRuns       int          `json:"activeRuns"`
	CompletedRuns    int          `json:"completedRuns"`
	FailedRuns       int          `json:"failedRuns"`
	Tasks            int          `json:"tasks"`
	InputTokens      int64        `json:"inputTokens,omitempty"`
	OutputTokens     int64        `json:"outputTokens,omitempty"`
	CacheReadTokens  int64        `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64        `json:"cacheWriteTokens,omitempty"`
	TotalTokens      int64        `json:"totalTokens,omitempty"`
	CacheHitRate     float64      `json:"cacheHitRate,omitempty"`
	TotalCostUSD     float64      `json:"totalCostUsd,omitempty"`
	AvgTTFTMs        int64        `json:"avgTtftMs,omitempty"`
	Models           []usageModel `json:"models"`
}

type usageModel struct {
	Model           string  `json:"model"`
	Runs            int     `json:"runs"`
	Completed       int     `json:"completed"`
	Failed          int     `json:"failed"`
	InputChars      int     `json:"inputChars"`
	OutputChars     int     `json:"outputChars"`
	InputTokens     int64   `json:"inputTokens,omitempty"`
	OutputTokens    int64   `json:"outputTokens,omitempty"`
	CacheReadTokens int64   `json:"cacheReadTokens,omitempty"`
	TotalTokens     int64   `json:"totalTokens,omitempty"`
	CacheHitRate    float64 `json:"cacheHitRate,omitempty"`
	CostUSD         float64 `json:"costUsd,omitempty"`
	AvgTTFTMs       int64   `json:"avgTtftMs,omitempty"`
}

type usageState struct {
	summary *usageSummary // from Core; nil until it answers
	loading bool
	err     string
}

// openUsage shows the dialog at once with this window's projection and asks
// Core for its complete summary off the UI thread.
func (w *workbench) openUsage() {
	w.widgets.usageOpen = true
	w.widgets.usage.err = ""
	if _, ok := w.service.(api.WorkbenchService); !ok {
		return
	}
	w.widgets.usage.loading = true
	w.workbenchCall("usage", api.WorkbenchRequest{Action: "usage.summary"}, func(response api.WorkbenchResponse, err error) {
		w.widgets.usage.loading = false
		if err != nil {
			w.widgets.usage.err = describe(w.tr("用量统计读取失败，显示当前窗口数据", "Usage summary unavailable; showing this window's data"), err)
			return
		}
		var summary usageSummary
		if json.Unmarshal([]byte(response.Output), &summary) != nil {
			w.widgets.usage.err = w.tr("用量统计格式无法识别，显示当前窗口数据", "Unrecognized usage summary; showing this window's data")
			return
		}
		w.widgets.usage.summary = &summary
	})
}

// localUsage computes the same summary from the runs and threads in memory.
func (w *workbench) localUsage() usageSummary {
	summary := usageSummary{Threads: len(w.threads), Runs: len(w.runs), Tasks: len(w.tasks)}
	byModel := map[string]*usageModel{}
	var ttftSum, ttftCount int64
	for _, r := range w.runs {
		model := r.Model
		if model == "" {
			if thread, ok := w.threadByID(r.ThreadID); ok {
				model = thread.Model
			}
		}
		if model == "" {
			model = w.configuredDefaultModel()
		}
		if model == "" {
			model = "pi/default"
		}
		stat := byModel[model]
		if stat == nil {
			stat = &usageModel{Model: model}
			byModel[model] = stat
		}
		stat.Runs++
		stat.InputChars += len(r.Input)
		stat.OutputChars += len(r.Output)
		if r.Telemetry != nil {
			summary.InputTokens += r.Telemetry.InputTokens
			summary.OutputTokens += r.Telemetry.OutputTokens
			summary.CacheReadTokens += r.Telemetry.CacheReadTokens
			summary.CacheWriteTokens += r.Telemetry.CacheWriteTokens
			summary.TotalTokens += r.Telemetry.TotalTokens
			summary.TotalCostUSD += r.Telemetry.CostUSD
			stat.InputTokens += r.Telemetry.InputTokens
			stat.OutputTokens += r.Telemetry.OutputTokens
			stat.CacheReadTokens += r.Telemetry.CacheReadTokens
			stat.TotalTokens += r.Telemetry.TotalTokens
			stat.CostUSD += r.Telemetry.CostUSD
			if r.Telemetry.TTFTMs > 0 {
				ttftSum += r.Telemetry.TTFTMs
				ttftCount++
			}
		}
		switch {
		case !r.Status.Terminal():
			summary.ActiveRuns++
		case r.Status == api.RunCompleted:
			summary.CompletedRuns++
			stat.Completed++
		case r.Status == api.RunFailed:
			summary.FailedRuns++
			stat.Failed++
		}
	}
	if promptTotal := summary.InputTokens + summary.CacheReadTokens + summary.CacheWriteTokens; promptTotal > 0 && summary.CacheReadTokens > 0 {
		summary.CacheHitRate = min(1.0, float64(summary.CacheReadTokens)/float64(promptTotal))
	}
	if ttftCount > 0 {
		summary.AvgTTFTMs = ttftSum / ttftCount
	}
	for _, stat := range byModel {
		summary.Models = append(summary.Models, *stat)
	}
	return summary
}

func (w *workbench) currentUsage() (usageSummary, bool) {
	if s := w.widgets.usage.summary; s != nil {
		summary := *s
		summary.Models = append([]usageModel(nil), s.Models...)
		return summary, true
	}
	return w.localUsage(), false
}

func compactCount(n int) string {
	return compactCount64(int64(n))
}

func compactCount64(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

func formatCostUSD(cost float64) string {
	switch {
	case cost >= 1.0:
		return fmt.Sprintf("$%.2f", cost)
	case cost >= 0.01:
		return fmt.Sprintf("$%.3f", cost)
	default:
		return fmt.Sprintf("$%.4f", cost)
	}
}

func normalizeUsageTab(tab string) string {
	switch tab {
	case "cost", "tokens", "models":
		return tab
	default:
		return "overview"
	}
}

type usageDailyBucket struct {
	label  string
	runs   int
	tokens int64
	cost   float64
}

// buildUsage7DayBuckets groups runs from the last 7 local calendar days.
func buildUsage7DayBuckets(runs []api.Run, now time.Time) []usageDailyBucket {
	if now.IsZero() {
		now = time.Now()
	}
	today := time.Date(now.Local().Year(), now.Local().Month(), now.Local().Day(), 0, 0, 0, 0, time.Local)
	buckets := make([]usageDailyBucket, 7)
	for i := 0; i < 7; i++ {
		day := today.AddDate(0, 0, i-6)
		buckets[i].label = day.Format("01/02")
	}
	for _, r := range runs {
		if r.CreatedAt.IsZero() {
			continue
		}
		rt := r.CreatedAt.Local()
		rDay := time.Date(rt.Year(), rt.Month(), rt.Day(), 0, 0, 0, 0, time.Local)
		diff := int(rDay.Sub(today).Hours() / 24)
		idx := 6 + diff
		if idx < 0 || idx >= 7 {
			continue
		}
		buckets[idx].runs++
		if r.Telemetry != nil {
			buckets[idx].tokens += r.Telemetry.TotalTokens
			buckets[idx].cost += r.Telemetry.CostUSD
		}
	}
	return buckets
}

// paintUsageDonutRing draws a crisp circular ring with centered percentage natively via Painter.
func paintUsageDonutRing(c *ui.Context, p palette, key string, fraction float64, tone ui.Color, title, subtitle string) {
	frac := max(0.0, min(1.0, fraction))
	pct := int(math.Round(frac * 100))
	ui.Column(c.Key(key)).Grow(1).MinWidth(116).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.Box(c.Key(key+"-ring")).Size(88, 88).Shrink(0).Draw(func(pt *ui.Painter, r ui.Rect) {
			strokeW := float32(9.5)
			inset := strokeW / 2
			ringRect := ui.Rect{X: r.X + inset, Y: r.Y + inset, W: r.W - strokeW, H: r.H - strokeW}
			trackCol := p.lineSoft
			if !p.dark {
				trackCol = p.line.Alpha(0.7)
			}
			pt.Stroke(ringRect, trackCol, ringRect.W/2, strokeW)
			if frac > 0.004 {
				cx, cy := r.X+r.W/2, r.Y+r.H/2
				rad := (min(r.W, r.H) - strokeW) / 2
				steps := max(12, int(math.Ceil(frac*48)))
				pts := make([][2]float32, steps+1)
				for i := 0; i <= steps; i++ {
					ang := -math.Pi/2 + (frac*2*math.Pi*float64(i))/float64(steps)
					pts[i] = [2]float32{
						cx + rad*float32(math.Cos(ang)),
						cy + rad*float32(math.Sin(ang)),
					}
				}
				pt.StrokePolyline(pts, strokeW, tone)
			}
		}).Children(func() {
			ui.Column(c).Fill().Justify(ui.Center).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf("%d%%", pct)).FontSize(18).Bold().FontFeatures("tnum").TextColor(p.ink).NoWrap()
			})
		})
		ui.Column(c).Gap(2).AlignItems(ui.Center).Children(func() {
			ui.Text(c, title).FontSize(12).FontWeight(600).TextColor(p.ink).SingleLine()
			if subtitle != "" {
				ui.Text(c, subtitle).FontSize(10.5).FontFeatures("tnum").TextColor(p.faint).SingleLine()
			}
		})
	})
}

// paintUsage7DayChart draws a native 7-day rounded column chart via ui.Painter.
func paintUsage7DayChart(c *ui.Context, p palette, key string, buckets []usageDailyBucket, mode string) {
	vals := make([]float64, len(buckets))
	maxVal := 0.0
	for i, b := range buckets {
		switch mode {
		case "cost":
			vals[i] = b.cost
		case "tokens":
			vals[i] = float64(b.tokens)
		default:
			if b.tokens > 0 {
				vals[i] = float64(b.tokens)
			} else {
				vals[i] = float64(b.runs)
			}
		}
		if vals[i] > maxVal {
			maxVal = vals[i]
		}
	}
	ui.Column(c.Key(key)).FillWidth().Gap(4).Children(func() {
		ui.Box(c.Key(key+"-canvas")).FillWidth().Height(74).Draw(func(pt *ui.Painter, r ui.Rect) {
			// Subtle horizontal reference gridlines
			pt.Line(r.X, r.Y+r.H-1, r.X+r.W, r.Y+r.H-1, 1, p.lineSoft)
			pt.Line(r.X, r.Y+r.H*0.5, r.X+r.W, r.Y+r.H*0.5, 0.8, p.lineSoft.Alpha(0.6))
			n := len(buckets)
			if n == 0 {
				return
			}
			slotW := r.W / float32(n)
			barW := min(float32(28), max(float32(10), slotW*0.52))
			chartH := r.H - 6
			for i, v := range vals {
				bx := r.X + float32(i)*slotW + (slotW-barW)/2
				if maxVal <= 0 || v <= 0 {
					pt.Fill(ui.Rect{X: bx, Y: r.Y + r.H - 4, W: barW, H: 3}, p.lineSoft, 1.5)
					continue
				}
				bh := max(float32(6), float32(v/maxVal)*chartH)
				by := r.Y + r.H - 1 - bh
				colTop := p.accent
				colBot := p.accent.Alpha(0.45)
				if i < n-1 && v < maxVal*0.95 {
					colTop = p.accent.Alpha(0.72)
					colBot = p.accent.Alpha(0.28)
				}
				pt.FillGradient(ui.Rect{X: bx, Y: by, W: barW, H: bh}, ui.LinearGradient{
					Angle: 180,
					Stops: []ui.GradientStop{
						{Color: colTop, At: 0},
						{Color: colBot, At: 1},
					},
				}, 4)
			}
		})
		ui.Row(c).FillWidth().Children(func() {
			for i, b := range buckets {
				ui.Row(c.Key(fmt.Sprintf("%s-lbl-%d", key, i))).Grow(1).Justify(ui.Center).Children(func() {
					ui.Text(c, b.label).Font("Menlo").FontSize(9.5).TextColor(p.faint).NoWrap()
				})
			}
		})
	})
}

func (w *workbench) usageDialog(c *ui.Context, p palette) {
	if !w.widgets.usageOpen {
		return
	}
	w.dialogFrame(c, p, &w.widgets.usageOpen, "usage", w.tr("用量", "Usage"), w.tr("关闭用量", "Close usage"), chartIcon, 820, func(inner float32) {
		summary, fromCore := w.currentUsage()
		activeTab := normalizeUsageTab(w.widgets.usageTab)
		models := append([]usageModel(nil), summary.Models...)
		sort.SliceStable(models, func(i, j int) bool {
			if models[i].Runs != models[j].Runs {
				return models[i].Runs > models[j].Runs
			}
			return models[i].Model < models[j].Model
		})

		// Top Stardust Header Strip: real summary headline + pill tab bar
		headline := w.tr3("用量洞察与趋势", "Usage insights & trends", "使用量インサイトと推移")
		switch {
		case summary.TotalCostUSD >= 0.0001 && summary.CacheHitRate > 0:
			headline = fmt.Sprintf(w.tr3("累计估算 %s · 缓存命中 %.0f%%", "Estimated %s · Cache hit %.0f%%", "推定 %s・キャッシュ命中 %.0f%%"),
				formatCostUSD(summary.TotalCostUSD), summary.CacheHitRate*100)
		case summary.TotalTokens > 0 && summary.CacheHitRate > 0:
			headline = fmt.Sprintf(w.tr3("总令牌 %s · 缓存命中 %.0f%%", "%s tokens · Cache hit %.0f%%", "総トークン %s・キャッシュ命中 %.0f%%"),
				compactCount64(summary.TotalTokens), summary.CacheHitRate*100)
		case summary.Runs > 0:
			headline = fmt.Sprintf(w.tr3("共 %d 次运行 · %d 个会话", "%d turns across %d threads", "%d ターン・%d スレッド"),
				summary.Runs, summary.Threads)
		}
		ui.Row(c.Key("usage-top-bar")).FillWidth().Gap(12).AlignItems(ui.Center).Wrap().Children(func() {
			ui.Column(c).Gap(2).Grow(1).MinWidth(220).Children(func() {
				ui.Text(c, headline).FontSize(17).Bold().FontFeatures("tnum").TextColor(p.ink).SingleLine().Ellipsis("…")
				source := w.tr("数据来源：当前窗口", "Source: this window")
				if fromCore {
					source = w.tr("数据来源：本地服务", "Source: local service")
				}
				if w.widgets.usage.loading {
					source += " · " + w.tr("正在刷新…", "Refreshing…")
				}
				ui.Text(c, source).FontSize(10.5).TextColor(p.faint).SingleLine()
			})

			// Stardust pill tabs: 概览 / 费用 / 令牌 / 模型
			tabs := []struct {
				id         string
				zh, en, ja string
			}{
				{"overview", "概览", "Overview", "概要"},
				{"cost", "费用", "Cost", "コスト"},
				{"tokens", "令牌", "Tokens", "トークン"},
				{"models", "模型", "Models", "モデル"},
			}
			ui.Row(c.Key("usage-tabs")).Height(32).Padding(3).Gap(2).Radius(10).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Shrink(0).Children(func() {
				for _, tab := range tabs {
					tab := tab
					sel := activeTab == tab.id
					lbl := w.tr3(tab.zh, tab.en, tab.ja)
					btn := ui.Button(c.Key("usage-tab:"+tab.id), "").Label(lbl).
						Height(26).Padding(0, 11).Radius(7.5).Border(0, ui.Transparent).Background(ui.Transparent).TextColor(p.muted).Shrink(0)
					if sel {
						btn.Background(p.card).Border(1, p.lineSoft).TextColor(p.ink)
					} else if btn.Hovered() {
						btn.Background(p.rowHover).TextColor(p.ink)
					}
					btn.Children(func() {
						wt := 500
						if sel {
							wt = 600
						}
						ui.Text(c, lbl).FontSize(12).FontWeight(wt).NoWrap()
					})
					if btn.Clicked() {
						w.widgets.usageTab = tab.id
					}
				}
			})
			if !w.widgets.usage.loading {
				if _, ok := w.service.(api.WorkbenchService); ok {
					if quietButton(c, p, "usage-refresh", w.tr("刷新", "Refresh")).Height(28).Clicked() {
						w.openUsage()
					}
				}
			}
		})
		if w.widgets.usage.err != "" {
			ui.Text(c, w.widgets.usage.err).FontSize(11).TextColor(p.warning).Wrap()
		}

		// Two-column Stardust Hero Cards: Left = 7-Day Trend & Spend, Right = Native Painter Donut Rings
		buckets := buildUsage7DayBuckets(w.runs, c.Now())
		twoCol := inner >= 580
		chartMode := "auto"
		if activeTab == "cost" {
			chartMode = "cost"
		} else if activeTab == "tokens" {
			chartMode = "tokens"
		}
		cardBorder := p.lineSoft
		if !p.dark {
			cardBorder = p.line
		}
		renderHeroCards := func() {
			// Left Card: 7-day cost/tokens KPIs + native Painter 7-day chart
			leftCard := ui.Column(c.Key("usage-card-7d")).Padding(16, 18).Gap(12).Radius(16).
				Background(p.card).Border(1, cardBorder)
			if twoCol {
				leftCard.Grow(56).MinWidth(0)
			} else {
				leftCard.FillWidth()
			}
			if !p.dark {
				leftCard.Shadow(0, 2, 12, 0, ui.Hex("#1D1D1F0C"))
			}
			leftCard.Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, w.tr3("7 日活跃与费用趋势", "7-Day Activity & Spend", "7日間の推移とコスト")).
						FontSize(13).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine()
					if summary.AvgTTFTMs > 0 {
						pill(c, p, fmt.Sprintf(w.tr3("首字 %.1fs", "TTFT %.1fs", "TTFT %.1fs"), float64(summary.AvgTTFTMs)/1000.0), p.accentInk)
					}
				})
				ui.Row(c).FillWidth().Gap(16).Children(func() {
					kpi := func(key, label, value string) {
						ui.Column(c.Key(key)).Gap(2).Shrink(0).Children(func() {
							ui.Text(c, label).FontSize(10.5).TextColor(p.faint).SingleLine()
							ui.Text(c, value).FontSize(22).Bold().FontFeatures("tnum").TextColor(p.ink).SingleLine()
						})
					}
					costStr := "$0.00"
					if summary.TotalCostUSD >= 0.0001 {
						costStr = formatCostUSD(summary.TotalCostUSD)
					}
					tokStr := "0"
					if summary.TotalTokens > 0 {
						tokStr = compactCount64(summary.TotalTokens)
					}
					kpi("kpi-cost", w.tr3("估算费用", "Est. Cost", "推定コスト"), costStr)
					kpi("kpi-tok", w.tr3("总令牌", "Total Tokens", "総トークン"), tokStr)
					kpi("kpi-cache", w.tr3("缓存命中", "Cache Hit", "キャッシュ率"), fmt.Sprintf("%.0f%%", summary.CacheHitRate*100))
				})
				if summary.TotalTokens > 0 {
					ui.Text(c, fmt.Sprintf("Tokens %s (in %s · out %s · cache %s)",
						compactCount64(summary.TotalTokens),
						compactCount64(summary.InputTokens),
						compactCount64(summary.OutputTokens),
						compactCount64(summary.CacheReadTokens),
					)).Font("Menlo").FontSize(10.5).FontFeatures("tnum").TextColor(p.muted).SingleLine().Ellipsis("…")
				}
				paintUsage7DayChart(c, p, "usage-7d-bars", buckets, chartMode)
			})

			// Right Card: Painter circular donut rings (strictly real metrics)
			rightCard := ui.Column(c.Key("usage-card-rings")).Padding(16, 18).Gap(12).Radius(16).
				Background(p.card).Border(1, cardBorder)
			if twoCol {
				rightCard.Grow(44).MinWidth(0)
			} else {
				rightCard.FillWidth()
			}
			if !p.dark {
				rightCard.Shadow(0, 2, 12, 0, ui.Hex("#1D1D1F0C"))
			}
			rightCard.Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, w.tr3("效率与结构圆环", "Efficiency & Breakdown", "効率と内訳リング")).
						FontSize(13).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine()
					ui.Text(c, w.tr3("实时统计", "Live", "リアルタイム")).FontSize(10.5).TextColor(p.faint)
				})
				ui.Spacer(c)
				ui.Row(c).FillWidth().Gap(8).Justify(ui.Center).AlignItems(ui.Center).Children(func() {
					cacheSub := w.tr3("暂无缓存读取", "No cache hits", "キャッシュなし")
					if summary.CacheReadTokens > 0 {
						cacheSub = w.tr3("缓存 ", "Cached ", "キャッシュ ") + compactCount64(summary.CacheReadTokens)
					}
					paintUsageDonutRing(c, p, "usage-ring-cache", summary.CacheHitRate, p.accent,
						w.tr3("缓存命中率", "Cache Hit", "キャッシュ率"), cacheSub)

					if activeTab == "tokens" && summary.TotalTokens > 0 {
						outFrac := float64(summary.OutputTokens) / float64(summary.TotalTokens)
						paintUsageDonutRing(c, p, "usage-ring-out", outFrac, ui.Hex("#38BDF8"),
							w.tr3("输出占比", "Output Share", "出力比率"),
							w.tr3("输出 ", "Output ", "出力 ")+compactCount64(summary.OutputTokens))
					} else {
						compFrac := 0.0
						compSub := w.tr3("暂无运行", "No runs", "実行なし")
						if summary.Runs > 0 {
							compFrac = float64(summary.CompletedRuns) / float64(summary.Runs)
							compSub = fmt.Sprintf("%d / %d %s", summary.CompletedRuns, summary.Runs, w.tr3("轮", "turns", "ターン"))
						}
						paintUsageDonutRing(c, p, "usage-ring-done", compFrac, p.success,
							w.tr3("完成率", "Completion", "完了率"), compSub)
					}
				})
				ui.Spacer(c)
			})
		}
		if twoCol {
			ui.Row(c.Key("usage-hero-row")).FillWidth().Gap(12).AlignItems(ui.Stretch).Children(renderHeroCards)
		} else {
			ui.Column(c.Key("usage-hero-row")).FillWidth().Gap(12).Children(renderHeroCards)
		}

		// Tab-specific panels
		switch activeTab {
		case "cost":
			w.usageCostTab(c, p, summary, models)
		case "tokens":
			w.usageTokensTab(c, p, summary)
		case "models":
			w.usageModelsTab(c, p, summary, models)
		default:
			// Overview tab (preserves all visual_test.go assertions and full charts)
			cards := []struct {
				label string
				value int
				tone  ui.Color
			}{
				{w.tr("总会话", "Threads"), summary.Threads, p.ink},
				{w.tr("总运行轮次", "Turns"), summary.Runs, p.ink},
				{w.tr("已完成", "Completed"), summary.CompletedRuns, p.success},
				{w.tr("进行中", "Running"), summary.ActiveRuns, p.running},
			}
			columns := 4
			if inner < 440 {
				columns = 2
			}
			cardWidth := (inner-float32(columns-1)*8)/float32(columns) - 2
			ui.Row(c.Key("usage-cards")).Gap(8).Wrap().Children(func() {
				for _, card := range cards {
					ui.Column(c.Key("usage-card:"+card.label)).Width(cardWidth).Padding(12, 14).Gap(4).Radius(12).Background(p.soft).Border(1, p.lineSoft).
						Label(fmt.Sprintf("%s %d", card.label, card.value)).Children(func() {
						ui.Text(c, fmt.Sprint(card.value)).FontSize(22).Bold().FontFeatures("tnum").TextColor(card.tone)
						ui.Text(c, card.label).FontSize(11).TextColor(p.muted).SingleLine()
					})
				}
			})

			if h24Spec := buildUsage24hChart(w, w.runs, summary.TotalTokens > 0); h24Spec != nil {
				ui.Column(c.Key("usage-chart-24h")).FillWidth().Children(func() {
					renderChart(c, p, h24Spec, nil)
				})
			}

			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, w.tr("按 Pi 模型", "By Pi model")).FontSize(12.5).Bold().Grow(1)
			})
			if len(models) == 0 {
				ui.Text(c, w.tr("还没有运行记录。发送第一条消息后，这里会按模型统计。", "No turns yet. After the first message, usage appears here by model.")).FontSize(11).TextColor(p.muted).Wrap()
			} else if modelSpec := buildUsageModelBreakdown(w, summary, models, w.runs); modelSpec != nil {
				ui.Column(c.Key("usage-chart-models")).FillWidth().Children(func() {
					renderChart(c, p, modelSpec, nil)
				})
			}

			if heatSpec := buildUsageHeatmap(w, w.runs); heatSpec != nil {
				ui.Column(c.Key("usage-chart-heatmap")).FillWidth().Children(func() {
					renderChart(c, p, heatSpec, nil)
				})
			}
			if donutSpec := buildUsageCacheDonut(w, summary); donutSpec != nil {
				ui.Column(c.Key("usage-chart-cache")).FillWidth().Children(func() {
					renderChart(c, p, donutSpec, nil)
				})
			}
		}
	})
}

func (w *workbench) usageCostTab(c *ui.Context, p palette, summary usageSummary, models []usageModel) {
	ui.Column(c.Key("usage-cost-breakdown")).FillWidth().Padding(14, 16).Gap(10).Radius(14).Background(p.soft).Border(1, p.lineSoft).Children(func() {
		ui.Text(c, w.tr3("按模型费用分布", "Spend by model", "モデル別コスト内訳")).FontSize(13).FontWeight(600).TextColor(p.ink)
		if len(models) == 0 {
			ui.Text(c, w.tr3("暂无费用记录。", "No cost records yet.", "コスト記録はまだありません。")).FontSize(11.5).TextColor(p.muted)
			return
		}
		maxCost := 0.0
		for _, m := range models {
			if m.CostUSD > maxCost {
				maxCost = m.CostUSD
			}
		}
		for _, m := range models {
			m := m
			frac := float32(0)
			if maxCost > 0 {
				frac = float32(m.CostUSD / maxCost)
			}
			ui.Column(c.Key("usage-cost-row:"+m.Model)).FillWidth().Gap(4).Children(func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, shortModel(m.Model)).FontSize(12).FontWeight(500).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
					ui.Text(c, fmt.Sprintf("%d %s", m.Runs, w.tr3("轮", "turns", "ターン"))).FontSize(11).FontFeatures("tnum").TextColor(p.faint).Shrink(0)
					costText := "—"
					if m.CostUSD >= 0.0001 {
						costText = formatCostUSD(m.CostUSD)
					}
					ui.Text(c, costText).Font("Menlo").FontSize(12).FontWeight(600).TextColor(p.ink).Shrink(0)
				})
				ui.Box(c).FillWidth().Height(6).Radius(3).Draw(func(pt *ui.Painter, r ui.Rect) {
					pt.Fill(r, p.lineSoft, 3)
					if frac > 0 {
						pt.Fill(ui.Rect{X: r.X, Y: r.Y, W: max(float32(6), r.W*frac), H: r.H}, p.accent, 3)
					}
				})
			})
		}
	})
}

func (w *workbench) usageTokensTab(c *ui.Context, p palette, summary usageSummary) {
	ui.Row(c.Key("usage-tokens-grid")).FillWidth().Gap(8).Wrap().Children(func() {
		tile := func(key, label, value string, tone ui.Color) {
			ui.Column(c.Key(key)).Grow(1).MinWidth(140).Padding(12, 14).Gap(3).Radius(12).Background(p.soft).Border(1, p.lineSoft).Children(func() {
				ui.Text(c, label).FontSize(10.5).TextColor(p.faint).SingleLine()
				ui.Text(c, value).FontSize(18).Bold().FontFeatures("tnum").TextColor(tone).SingleLine()
			})
		}
		tile("tok-in", w.tr3("输入令牌", "Input tokens", "入力トークン"), compactCount64(summary.InputTokens), ui.Hex("#38BDF8"))
		tile("tok-out", w.tr3("输出令牌", "Output tokens", "出力トークン"), compactCount64(summary.OutputTokens), p.accent)
		tile("tok-cr", w.tr3("缓存读取", "Cache read", "キャッシュ読込"), compactCount64(summary.CacheReadTokens), p.success)
		tile("tok-cw", w.tr3("缓存写入", "Cache write", "キャッシュ書込"), compactCount64(summary.CacheWriteTokens), p.warning)
	})
	if h24Spec := buildUsage24hChart(w, w.runs, true); h24Spec != nil {
		ui.Column(c.Key("usage-tokens-24h")).FillWidth().Children(func() {
			renderChart(c, p, h24Spec, nil)
		})
	}
	if donutSpec := buildUsageCacheDonut(w, summary); donutSpec != nil {
		ui.Column(c.Key("usage-tokens-donut")).FillWidth().Children(func() {
			renderChart(c, p, donutSpec, nil)
		})
	}
}

func (w *workbench) usageModelsTab(c *ui.Context, p palette, summary usageSummary, models []usageModel) {
	if len(models) == 0 {
		ui.Text(c, w.tr("还没有运行记录。发送第一条消息后，这里会按模型统计。", "No turns yet. After the first message, usage appears here by model.")).FontSize(11).TextColor(p.muted).Wrap()
		return
	}
	ui.Column(c.Key("usage-models-list")).FillWidth().Gap(8).Children(func() {
		for _, m := range models {
			m := m
			ui.Row(c.Key("usage-model-card:"+m.Model)).FillWidth().Padding(12, 14).Gap(12).Radius(12).
				Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Children(func() {
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
					ui.Text(c, shortModel(m.Model)).FontSize(13).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…")
					ui.Text(c, m.Model).Font("Menlo").FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…")
				})
				ui.Row(c).Gap(10).AlignItems(ui.Center).Shrink(0).Children(func() {
					pill(c, p, fmt.Sprintf("%d %s", m.Runs, w.tr3("轮", "turns", "ターン")), p.muted)
					if m.TotalTokens > 0 {
						ui.Text(c, compactCount64(m.TotalTokens)+" tok").Font("Menlo").FontSize(11).TextColor(p.ink).NoWrap()
					}
					if m.CostUSD >= 0.0001 {
						ui.Text(c, formatCostUSD(m.CostUSD)).Font("Menlo").FontSize(11.5).FontWeight(600).TextColor(p.accentInk).NoWrap()
					}
				})
			})
		}
	})
	if modelSpec := buildUsageModelBreakdown(w, summary, models, w.runs); modelSpec != nil {
		ui.Column(c.Key("usage-models-chart")).FillWidth().Children(func() {
			renderChart(c, p, modelSpec, nil)
		})
	}
}

func usageHourLabels() []string {
	labels := make([]string, 24)
	for h := 0; h < 24; h++ {
		labels[h] = fmt.Sprintf("%02d", h)
	}
	return labels
}

func buildUsage24hChart(w *workbench, runs []api.Run, preferTokens bool) *chartSpec {
	useTokens := false
	if preferTokens {
		for _, r := range runs {
			if !r.CreatedAt.IsZero() && r.Telemetry != nil && r.Telemetry.TotalTokens > 0 {
				useTokens = true
				break
			}
		}
	}
	buckets := make(chartValues, 24)
	anyPositive := false
	for _, r := range runs {
		if r.CreatedAt.IsZero() {
			continue
		}
		hr := r.CreatedAt.Local().Hour()
		if hr < 0 || hr >= 24 {
			continue
		}
		val := 1.0
		if useTokens {
			if r.Telemetry == nil || r.Telemetry.TotalTokens <= 0 {
				continue
			}
			val = float64(r.Telemetry.TotalTokens)
		}
		buckets[hr] += val
		anyPositive = true
	}
	if !anyPositive {
		return nil
	}
	unit := w.tr("轮", " turns")
	seriesLabel := w.tr("运行轮次", "Turns")
	if useTokens {
		unit = " tok"
		seriesLabel = "Tokens"
	}
	return &chartSpec{
		Type:   "bar",
		Title:  w.tr("24 小时活跃分布", "24h Activity"),
		Labels: usageHourLabels(),
		Unit:   unit,
		Height: 190,
		Series: []chartSeries{{Name: seriesLabel, Data: buckets}},
	}
}

func buildUsageModelBreakdown(w *workbench, summary usageSummary, models []usageModel, runs []api.Run) *chartSpec {
	if len(models) == 0 {
		return nil
	}
	limit := min(8, len(models))
	models = models[:limit]

	useTokens := false
	if summary.TotalTokens > 0 {
		for _, m := range models {
			if m.TotalTokens > 0 {
				useTokens = true
				break
			}
		}
	}

	// Precompute time bounds for 8-bin chronological sparklines per model.
	const sparkBins = 8
	var datedRuns []api.Run
	for _, r := range runs {
		if !r.CreatedAt.IsZero() {
			datedRuns = append(datedRuns, r)
		}
	}
	sort.SliceStable(datedRuns, func(i, j int) bool {
		return datedRuns[i].CreatedAt.Before(datedRuns[j].CreatedAt)
	})
	var spanSecs float64
	if len(datedRuns) >= 2 {
		spanSecs = datedRuns[len(datedRuns)-1].CreatedAt.Sub(datedRuns[0].CreatedAt).Seconds()
	}

	series := make([]chartSeries, 0, len(models))
	labels := make([]string, 0, len(models))
	anyPositive := false
	for _, m := range models {
		metric := float64(m.Runs)
		if useTokens {
			metric = float64(m.TotalTokens)
		}
		if metric > 0 {
			anyPositive = true
		}
		sub := w.tr(fmt.Sprintf("%d 轮", m.Runs), w.turns(m.Runs))
		if m.TotalTokens > 0 {
			sub += " · " + compactCount64(m.TotalTokens) + " tok"
		} else if m.OutputChars > 0 {
			sub += " · " + w.tr(compactCount(m.OutputChars)+" 字符输出", compactCount(m.OutputChars)+" chars out")
		}
		var badge string
		if m.CostUSD >= 0.0001 {
			badge = formatCostUSD(m.CostUSD)
		}
		var spark chartValues
		if len(datedRuns) >= 2 {
			bins := make(chartValues, sparkBins)
			modelHits := 0
			for idx, r := range datedRuns {
				if r.Model != m.Model {
					continue
				}
				bin := idx * sparkBins / len(datedRuns)
				if spanSecs > 0 {
					bin = int(r.CreatedAt.Sub(datedRuns[0].CreatedAt).Seconds() / spanSecs * float64(sparkBins-1))
				}
				if bin < 0 {
					bin = 0
				} else if bin >= sparkBins {
					bin = sparkBins - 1
				}
				inc := 1.0
				if useTokens && r.Telemetry != nil && r.Telemetry.TotalTokens > 0 {
					inc = float64(r.Telemetry.TotalTokens)
				}
				bins[bin] += inc
				modelHits++
			}
			if modelHits > 0 {
				spark = bins
			}
		}
		name := w.modelDisplayName(m.Model)
		labels = append(labels, name)
		series = append(series, chartSeries{
			Name:        name,
			AccessLabel: m.Model,
			Subtitle:    sub,
			Badge:       badge,
			Data:        chartValues{metric},
			Sparkline:   spark,
		})
	}
	if !anyPositive {
		return nil
	}
	unit := w.tr("轮", " turns")
	if useTokens {
		unit = " tok"
	}
	return &chartSpec{
		Type:   "breakdown",
		Labels: labels,
		Unit:   unit,
		Height: 190,
		Series: series,
	}
}

func buildUsageHeatmap(w *workbench, runs []api.Run) *chartSpec {
	if len(runs) < 3 {
		return nil
	}
	days := []string{
		w.tr("周一", "Mon"), w.tr("周二", "Tue"), w.tr("周三", "Wed"),
		w.tr("周四", "Thu"), w.tr("周五", "Fri"), w.tr("周六", "Sat"), w.tr("周日", "Sun"),
	}
	grid := make([]chartValues, 7)
	for d := range grid {
		grid[d] = make(chartValues, 24)
	}
	anyPositive := false
	for _, r := range runs {
		if r.CreatedAt.IsZero() {
			continue
		}
		local := r.CreatedAt.Local()
		row := (int(local.Weekday()) + 6) % 7 // Mon=0 .. Sun=6
		col := local.Hour()
		if col >= 0 && col < 24 {
			grid[row][col]++
			anyPositive = true
		}
	}
	if !anyPositive {
		return nil
	}
	series := make([]chartSeries, 7)
	for d := range days {
		series[d] = chartSeries{Name: days[d], Data: grid[d]}
	}
	return &chartSpec{
		Type:   "heatmap",
		Style:  "dots",
		Title:  w.tr("7×24 活跃节奏", "7×24 Activity Rhythm"),
		Labels: usageHourLabels(),
		Unit:   w.tr("轮", " runs"),
		Height: 190,
		Series: series,
	}
}

func buildUsageCacheDonut(w *workbench, summary usageSummary) *chartSpec {
	if summary.CacheReadTokens <= 0 || summary.TotalTokens <= 0 {
		return nil
	}
	nonCacheIn := summary.InputTokens - summary.CacheReadTokens
	if nonCacheIn < 0 {
		nonCacheIn = 0
	}
	outTok := summary.OutputTokens
	if outTok < 0 {
		outTok = 0
	}
	if summary.CacheReadTokens+nonCacheIn+outTok <= 0 {
		return nil
	}
	return &chartSpec{
		Type:        "donut",
		Title:       w.tr("Token 结构与缓存效率", "Token Mix & Cache Efficiency"),
		CenterLabel: fmt.Sprintf("%.0f%%", summary.CacheHitRate*100),
		Labels:      []string{w.tr("缓存命中", "Cache Read"), w.tr("常规输入", "Fresh Input"), w.tr("模型输出", "Output")},
		Unit:        " tok",
		Height:      190,
		Series: []chartSeries{{
			Name: "Tokens",
			Data: chartValues{float64(summary.CacheReadTokens), float64(nonCacheIn), float64(outTok)},
		}},
	}
}
