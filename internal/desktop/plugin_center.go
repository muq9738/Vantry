package desktop

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// The plugin center follows mygo-music: plugins install from a GitHub
// repository, update in place, declare their settings for the host to draw
// natively, ask before starting processes, and contribute pages that open on
// the main stage like any first-class workspace.
const (
	actionPluginsInstall = "plugins.install"
	actionPluginsUpdate  = "plugins.update"
	actionPluginsRemove  = "plugins.remove"
	actionPluginsGrant   = "plugins.grant"
	actionPluginsSetting = "plugins.setting"
	actionPluginsPage    = "plugins.page"
	stagePluginPrefix    = "plugin:"
)

type pluginCenterState struct {
	repo     string
	loaded   bool
	drafts   map[string]string
	open     map[string]bool
	pages    map[string]string
	pageErr  map[string]string
	installs int
}

var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)

// normalizeGitHubRepo accepts owner/repo or a GitHub URL and returns
// owner/repo, or "" when it names no repository.
func normalizeGitHubRepo(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"https://", "http://", "git@"} {
		text = strings.TrimPrefix(text, prefix)
	}
	text = strings.TrimPrefix(text, "www.")
	if rest, ok := strings.CutPrefix(text, "github.com/"); ok {
		text = rest
	} else if rest, ok := strings.CutPrefix(text, "github.com:"); ok {
		text = rest
	}
	text = strings.TrimSuffix(strings.TrimSuffix(text, "/"), ".git")
	parts := strings.Split(text, "/")
	if len(parts) > 2 {
		text = parts[0] + "/" + parts[1]
	}
	if !githubRepoPattern.MatchString(text) {
		return ""
	}
	return text
}

func pluginKey(id, name string) string { return id + "\x00" + name }

func pluginStage(id, page string) string { return stagePluginPrefix + pluginKey(id, page) }

func (w *workbench) acceptPlugins(response api.WorkbenchResponse) {
	if response.Plugins != nil {
		w.features.plugins = response.Plugins
		w.plugs.loaded = true
	}
}

func (w *workbench) installPlugin() bool {
	repo := normalizeGitHubRepo(w.plugs.repo)
	if repo == "" {
		w.setToolFeedback("plugins:"+w.selected, notice{Text: w.tr("请输入 owner/repo 或 GitHub 仓库地址。", "Enter owner/repo or a GitHub repository URL."), Level: noticeWarn})
		return false
	}
	target := w.selected
	w.workbenchCall("plugins-install", api.WorkbenchRequest{Action: actionPluginsInstall, Name: repo}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("plugins:"+target, notice{Text: describe(w.tr("安装 ", "Install ")+repo, err), Level: noticeError})
			return
		}
		w.acceptPlugins(response)
		w.plugs.repo = ""
		w.plugs.installs++
		w.setToolFeedback("plugins:"+target, notice{Text: w.tr("已安装 ", "Installed ") + repo, Level: noticeInfo})
	})
	return true
}

func (w *workbench) checkPluginUpdates() {
	target := w.selected
	w.workbenchCall("plugins", api.WorkbenchRequest{Action: "plugins.list", Name: "check-updates", Message: "check-updates"}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("plugins:"+target, notice{Text: describe(w.tr("检查插件更新失败", "Checking plugin updates failed"), err), Level: noticeError})
			return
		}
		w.acceptPlugins(response)
		updates := 0
		for _, plugin := range w.features.plugins {
			if plugin.HasUpdate {
				updates++
			}
		}
		text := w.tr("插件均为最新版本。", "All plugins are up to date.")
		if updates > 0 {
			text = fmt.Sprintf(w.tr("%d 个插件可热更新。", "%d plugins can update."), updates)
		}
		w.setToolFeedback("plugins:"+target, notice{Text: text, Level: noticeInfo})
	})
}

func (w *workbench) pluginCall(key, label string, request api.WorkbenchRequest) {
	target := w.selected
	w.workbenchCall(key, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("plugins:"+target, notice{Text: describe(label, err), Level: noticeError})
			w.loadPlugins()
			return
		}
		w.acceptPlugins(response)
		w.setToolFeedback("plugins:"+target, notice{Text: label + w.tr(" 已完成", " done"), Level: noticeInfo})
	})
}

func (w *workbench) updatePlugin(plugin api.PluginInfo) {
	w.pluginCall("plugin-update:"+plugin.ID, w.tr("热更新 ", "Update ")+plugin.Name, api.WorkbenchRequest{Action: actionPluginsUpdate, PluginID: plugin.ID, Name: plugin.Repo})
}

func (w *workbench) removePlugin(plugin api.PluginInfo) {
	if strings.HasPrefix(w.stage, stagePluginPrefix+plugin.ID+"\x00") {
		w.stage = ""
	}
	w.pluginCall("plugin-remove:"+plugin.ID, w.tr("移除 ", "Remove ")+plugin.Name, api.WorkbenchRequest{Action: actionPluginsRemove, PluginID: plugin.ID})
}

func (w *workbench) grantPlugin(id string, granted bool) {
	for i := range w.features.plugins {
		if w.features.plugins[i].ID == id {
			w.features.plugins[i].Granted = granted
		}
	}
	verb := "revoke"
	if granted {
		verb = "grant"
	}
	input, _ := json.Marshal(map[string]bool{"granted": granted})
	w.pluginCall("plugin-grant:"+id, w.tr("进程权限", "Process permission"), api.WorkbenchRequest{Action: actionPluginsGrant, PluginID: id, Message: verb, Input: input})
}

func (w *workbench) setPluginSetting(id, key, value string) {
	for i := range w.features.plugins {
		if w.features.plugins[i].ID != id {
			continue
		}
		for j := range w.features.plugins[i].Settings {
			if w.features.plugins[i].Settings[j].Key == key {
				w.features.plugins[i].Settings[j].Value = value
			}
		}
	}
	if w.rd.pluginID == id {
		w.rd.pluginID = "" // Apply saved plugin settings when its page is reopened.
	}
	input, _ := json.Marshal(map[string]string{"key": key, "value": value})
	w.pluginCall("plugin-setting:"+pluginKey(id, key), w.tr("保存设置", "Save setting"), api.WorkbenchRequest{Action: actionPluginsSetting, PluginID: id, Name: key, Message: value, Input: input})
}

// openPluginPage shows a plugin's page on the main stage.
func (w *workbench) openPluginPage(plugin api.PluginInfo, page api.PluginNavItem) {
	key := pluginKey(plugin.ID, page.Page)
	if plugin.Error != "" {
		w.setToolFeedback("plugins:"+w.selected, notice{Text: plugin.Error, Level: noticeError})
		return
	}
	w.stage = pluginStage(plugin.ID, page.Page)
	if page.NativePanel == "remote-access" {
		w.loadRemoteAccess()
		return
	}
	if page.NativePanel == "remote-desktop" {
		w.prepareRemoteDesktop(plugin)
		return
	}
	if w.plugs.pages == nil {
		w.plugs.pages, w.plugs.pageErr = map[string]string{}, map[string]string{}
	}
	delete(w.plugs.pageErr, key)
	input, _ := json.Marshal(map[string]any{"page": page.Page, "threadId": w.selected, "message": w.composerFor(w.selected).draft})
	w.workbenchCall("plugin-page:"+key, api.WorkbenchRequest{Action: actionPluginsPage, PluginID: plugin.ID, Name: page.Page, ThreadID: w.selected, Workspace: w.selectedWorkspacePath(), Input: input}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.plugs.pageErr[key] = describe(w.tr("打开页面失败", "Opening the page failed"), err)
			return
		}
		w.plugs.pages[key] = response.Output
	})
}

// pluginCenter groups installed plugins, discovery and installation:
// then recommended repositories, then a GitHub install field, each as a
// titled group of hairline-divided rows.
func (w *workbench) pluginCenter(c *ui.Context, p palette) {
	ui.Scroll(c.Key("plugin-cards")).Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).Gap(22).Padding(4, 2, 16).Children(func() {
			ui.Column(c).Gap(10).Children(func() {
				settingsHeading(c, p, fmt.Sprintf(w.tr("已安装插件 · %d", "Installed · %d"), len(w.features.plugins)), "", func() {
					check := quietButton(c, p, "plugins-check", w.tr("检查更新", "Check updates")).Label(w.tr("检查插件更新", "Check Updates")).Height(26).FontSize(12)
					if w.features.busy["plugins"] {
						check.Disabled(true)
					}
					if check.Clicked() {
						w.checkPluginUpdates()
					}
					if iconButton(c, p, "plugins-refresh", w.tr("刷新插件", "Refresh plugins"), refreshIcon).Size(26, 26).Padding(6).Disabled(w.features.busy["plugins"]).Clicked() {
						w.loadPlugins()
					}
				})
				if len(w.features.plugins) == 0 {
					settingsGroup(c, p, "plugin-empty", func() {
						ui.Column(c).Gap(8).Padding(24, 20).AlignItems(ui.Center).Children(func() {
							ui.Icon(c, gridIcon).FontSize(22).TextColor(p.faint)
							ui.Text(c, w.tr("还没有插件。从下方推荐或 GitHub 仓库安装，也可把命令插件放进 Core 数据目录的 plugins/<插件名>/。", "No plugins yet. Install a recommended one or any GitHub repository below, or put command plugins under plugins/<name>/ in the Core data directory.")).
								FontSize(12).TextColor(p.muted).TextAlign(ui.Center).Wrap()
						})
					})
				} else {
					settingsGroup(c, p, "plugin-installed", func() {
						for i, plugin := range w.features.plugins {
							w.pluginCard(c, p, i, plugin)
						}
					})
				}
			})
			if output := w.features.pluginOutputs[w.selected]; output != "" {
				w.pluginOutputView(c, p, output)
			}
			w.pluginRepositoryCatalog(c, p)
			ui.Column(c.Key("plugin-install")).Gap(10).Children(func() {
				settingsHeading(c, p, w.tr("从 GitHub 安装", "Install from GitHub"), w.tr("输入 owner/repo 或仓库地址；安装前不会下载或执行任何内容。", "Enter owner/repo or a repository URL; nothing downloads or runs until you install."), nil)
				settingsGroup(c, p, "plugin-install-group", func() {
					ui.Row(c).Padding(12, 12, 12, 16).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, githubIcon).FontSize(15).TextColor(p.muted).Shrink(0)
						input := kitField(ui.TextInput(c.Key("plugin-repo"), &w.plugs.repo).Label(w.tr("插件仓库", "Plugin repository")).Placeholder("owner/repo").Grow(1).MinWidth(0), p, false)
						install := primaryAction(c, p, "plugin-install-button", w.tr("安装", "Install")).Label(w.tr("安装插件", "Install plugin"))
						if normalizeGitHubRepo(w.plugs.repo) == "" || w.features.busy["plugins-install"] {
							install.Disabled(true)
						}
						if install.Clicked() || input.Submitted() && normalizeGitHubRepo(w.plugs.repo) != "" {
							w.installPlugin()
						}
					})
				})
			})
		})
	})
}

// Official plugins have independent repositories and release cycles. Listing
// them here never downloads or executes anything until Install is chosen.
// Already-installed plugins are not repeated beneath Installed.
func (w *workbench) pluginRepositoryCatalog(c *ui.Context, p palette) {
	type catalogEntry struct {
		id, repo, zh, en, dzh, den string
		icon                       *ui.SVG
	}
	var available []catalogEntry
	for _, entry := range []catalogEntry{
		{"vantry-remote-desktop", "muq9738/vantry-plugin-remote-desktop", "远程桌面", "Remote Desktop", "可选安装；通过独立共享码连接，经所选中继传送画面与键鼠。", "Optional plugin. Share using a separate desktop code through your selected relay.", remoteIcon},
	} {
		installed := false
		for _, plugin := range w.features.plugins {
			if plugin.ID == entry.id {
				installed = true
				break
			}
		}
		if !installed {
			available = append(available, entry)
		}
	}
	if len(available) == 0 {
		return
	}
	ui.Column(c.Key("plugin-repositories")).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr("推荐插件", "Recommended"), "", nil)
		settingsGroup(c, p, "plugin-repositories-group", func() {
			for i, entry := range available {
				ui.Row(c.Key(entry.id)).FillWidth().Padding(12, 16).Gap(12).AlignItems(ui.Center).Children(func() {
					pluginTile(c, p, entry.icon, "")
					ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
						ui.Text(c, w.tr(entry.zh, entry.en)).FontSize(13).FontWeight(500).TextColor(p.ink).SingleLine()
						ui.Text(c, w.tr(entry.dzh, entry.den)).FontSize(12).TextColor(p.muted).Wrap()
						ui.Text(c, entry.repo).Font("Menlo").FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…").MinWidth(0)
					})
					button := outlineButton(c, p, "install", w.tr("安装", "Install")).Label(w.tr("安装插件：", "Install plugin: ") + w.tr(entry.zh, entry.en)).Shrink(0)
					button.Disabled(w.features.busy["plugins-install"])
					if button.Clicked() {
						w.plugs.repo = entry.repo
						w.installPlugin()
					}
				}).BorderWidth(func() float32 {
					if i == 0 {
						return 0
					}
					return 1
				}(), 0, 0, 0).BorderColor(p.lineSoft)
			}
		})
	})
}

// pluginTile is the 32-point square that fronts a plugin row: its Lucide
// glyph, or its initial when it brings none.
func pluginTile(c *ui.Context, p palette, icon *ui.SVG, initial string) {
	ui.Row(c).Size(34, 34).Radius(9).Justify(ui.Center).AlignItems(ui.Center).Background(p.soft).Border(1, p.lineSoft).Shrink(0).Children(func() {
		if icon != nil {
			ui.Icon(c, icon).FontSize(16).TextColor(p.ink)
			return
		}
		ui.Text(c, initial).FontSize(14).FontWeight(600).TextColor(p.ink)
	})
}

func (w *workbench) pluginCard(c *ui.Context, p palette, index int, plugin api.PluginInfo) {
	card := ui.Column(c.Key("plugin:"+plugin.ID)).FillWidth().Padding(14, 16).Gap(12)
	if index > 0 {
		card.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
	}
	card.Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Start).Children(func() {
			initial := "P"
			if r, _ := utf8.DecodeRuneInString(strings.TrimSpace(plugin.Name)); r != utf8.RuneError {
				initial = strings.ToUpper(string(r))
			}
			var icon *ui.SVG
			for _, page := range plugin.Navigation {
				if page.NativePanel == "remote-desktop" || page.NativePanel == "remote-access" {
					icon = remoteIcon
				}
			}
			pluginTile(c, p, icon, initial)
			ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Text(c, plugin.Name).FontSize(13).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
					if plugin.Version != "" {
						microTag(c, p, "v"+strings.TrimPrefix(plugin.Version, "v"), p.muted)
					}
					if plugin.Builtin {
						microTag(c, p, w.tr("内置", "Builtin"), p.accentInk)
					}
				})
				if plugin.Description != "" {
					ui.Text(c, plugin.Description).FontSize(12).TextColor(p.muted).Wrap()
				}
				if plugin.Repo != "" {
					ui.Text(c, plugin.Repo).Font("Menlo").FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…")
				}
			})
			if plugin.HasUpdate {
				label := w.tr("热更新", "Update")
				if plugin.LatestVersion != "" {
					label += " → v" + strings.TrimPrefix(plugin.LatestVersion, "v")
				}
				update := primaryAction(c, p, "update", label).Label(w.tr("热更新 ", "Update ") + plugin.Name).Shrink(0)
				if w.features.busy["plugin-update:"+plugin.ID] {
					update.Disabled(true)
				}
				if update.Clicked() {
					w.updatePlugin(plugin)
				}
			}
		})
		if plugin.Error != "" {
			ui.Row(c).Gap(6).AlignItems(ui.Start).Padding(8, 10).Radius(8).Background(p.danger.Alpha(0.08)).Children(func() {
				ui.Icon(c, alertIcon).FontSize(13).TextColor(p.danger).Shrink(0)
				ui.Text(c, plugin.Error).FontSize(12).TextColor(p.danger).Wrap().Grow(1).MinWidth(0)
			})
		}
		ui.Row(c).Gap(8).Wrap().AlignItems(ui.Center).Padding(0, 0, 0, 46).Children(func() {
			if plugin.Error == "" && (len(plugin.Navigation) == 0 || plugin.Navigation[0].NativePanel == "") {
				run := outlineButton(c, p, "run", w.tr("运行", "Run")).Label(w.tr("运行插件 ", "Run plugin ") + plugin.Name)
				run.Disabled(w.features.busy["plugin-run:"+w.selected] || plugin.NeedsProcessGrant && !plugin.Granted)
				if run.Clicked() {
					w.runPlugin(plugin.ID)
				}
			}
			for _, page := range plugin.Navigation {
				if plugin.Error != "" {
					continue
				}
				page := page
				open := ui.Button(c.Key("page:"+page.Page), "").Label(w.tr("打开页面 ", "Open page ")+page.Title).Height(28).Padding(0, 12).Radius(8).
					Border(1, p.line).Background(p.raised).TextColor(p.ink)
				if open.Hovered() {
					open.Background(p.soft)
				}
				open.Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Text(c, page.Title).FontSize(12).NoWrap()
						ui.Icon(c, lucide("arrow-up-right")).FontSize(12).TextColor(p.muted)
					})
				})
				if open.Clicked() {
					w.openPluginPage(plugin, page)
				}
			}
			if !plugin.Builtin {
				remove := quietButton(c, p, "remove", w.tr("移除", "Remove")).Label(w.tr("移除插件 ", "Remove plugin ")+plugin.Name).Height(28).Padding(0, 10)
				remove.Disabled(w.features.busy["plugin-remove:"+plugin.ID])
				if remove.Hovered() {
					remove.TextColor(p.danger).Background(p.danger.Alpha(0.1))
				}
				if remove.Clicked() {
					w.removePlugin(plugin)
				}
			}
		})
		if plugin.NeedsProcessGrant {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(10, 12).Margin(0, 0, 0, 46).Radius(10).Border(1, p.lineSoft).Background(p.warning.Alpha(0.06)).Children(func() {
				ui.Icon(c, lucide("triangle-alert")).FontSize(14).TextColor(p.warning).Shrink(0)
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
					ui.Text(c, w.tr("允许启动外部程序", "Allow process execution")).FontSize(12).FontWeight(500).TextColor(p.ink)
					ui.Text(c, w.tr("插件可在本机运行命令；仅授权你信任的插件。", "The plugin may run commands on this machine; grant only plugins you trust.")).FontSize(11.5).TextColor(p.muted).Wrap()
				})
				granted := plugin.Granted
				ui.Switch(c.Key("grant"), &granted).Label(w.tr("允许启动外部程序 ", "Allow process execution ") + plugin.Name)
				if granted != plugin.Granted {
					w.grantPlugin(plugin.ID, granted)
				}
			})
		}
		if len(plugin.Settings) > 0 {
			ui.Column(c).Padding(0, 0, 0, 46).AlignItems(ui.Start).Children(func() { w.pluginSettings(c, p, plugin) })
		}
	})
}

// pluginSettings draws the plugin's declared settings natively: text
// fields, switches and choices, as rows of a nested group.
func (w *workbench) pluginSettings(c *ui.Context, p palette, plugin api.PluginInfo) {
	if w.plugs.open == nil {
		w.plugs.open = map[string]bool{}
	}
	open := w.plugs.open[plugin.ID]
	head := ui.Button(c.Key("settings-toggle"), "").Label(fmt.Sprintf(w.tr("插件设置 %s（%d）", "%s settings (%d)"), plugin.Name, len(plugin.Settings))).Expanded(open).
		Height(26).Padding(0, 8).Radius(7).Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
	if head.Hovered() {
		head.Background(p.soft).TextColor(p.ink)
	}
	head.Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, lucide("sliders-horizontal")).FontSize(12)
			ui.Text(c, fmt.Sprintf(w.tr("设置 · %d", "Settings · %d"), len(plugin.Settings))).FontSize(12)
			icon := rightIcon
			if open {
				icon = chevronIcon
			}
			ui.Icon(c, icon).FontSize(12).TextColor(p.faint)
		})
	})
	if head.Clicked() {
		w.plugs.open[plugin.ID] = !open
	}
	if !open {
		return
	}
	if w.plugs.drafts == nil {
		w.plugs.drafts = map[string]string{}
	}
	ui.Column(c.Key("settings")).Margin(8, 0, 0).Radius(10).Border(1, p.lineSoft).Background(p.canvas.Alpha(0.4)).Children(func() {
		for i, spec := range plugin.Settings {
			spec := spec
			value := nonEmptyString(spec.Value, spec.Default)
			label := nonEmptyString(spec.Label, spec.Key)
			item := ui.Column(c.Key("setting:"+spec.Key)).Gap(6).Padding(10, 12)
			if i > 0 {
				item.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
			}
			item.Children(func() {
				switch spec.Type {
				case "boolean", "bool":
					on := value == "true"
					ui.Checkbox(c.Key("value"), &on, label).Description(spec.Help)
					if on != (value == "true") {
						w.setPluginSetting(plugin.ID, spec.Key, fmt.Sprint(on))
					}
					return
				case "select":
					ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(p.ink)
					options, labels := []string{}, []string{}
					current := 0
					for i, option := range spec.Options {
						options = append(options, option.Value)
						labels = append(labels, nonEmptyString(option.Label, option.Value))
						if option.Value == value {
							current = i
						}
					}
					if len(options) > 0 && len(options) <= 4 {
						chosen := current
						ui.Segmented(c.Key("value"), &chosen, labels...).Label(label)
						if chosen != current {
							w.setPluginSetting(plugin.ID, spec.Key, options[chosen])
						}
					} else if len(options) > 0 {
						chosen := labels[current]
						ui.Select(c.Key("value"), &chosen, labels).Label(label).FillWidth()
						for i, l := range labels {
							if l == chosen && i != current {
								w.setPluginSetting(plugin.ID, spec.Key, options[i])
							}
						}
					}
				default:
					key := pluginKey(plugin.ID, spec.Key)
					draft, editing := w.plugs.drafts[key]
					if !editing {
						draft = value
					}
					ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(p.ink)
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						field := kitField(ui.TextInput(c.Key("value"), &draft).Label(label).Placeholder(spec.Default).Grow(1).MinWidth(0), p, false)
						save := outlineButton(c, p, "save", w.tr("保存", "Save")).Label(w.tr("保存设置 ", "Save setting ") + label)
						if draft == value {
							save.Disabled(true)
						}
						if save.Clicked() || field.Submitted() && draft != value {
							delete(w.plugs.drafts, key)
							w.setPluginSetting(plugin.ID, spec.Key, draft)
							return
						}
						if draft != value {
							w.plugs.drafts[key] = draft
						} else {
							delete(w.plugs.drafts, key)
						}
					})
				}
				if spec.Help != "" {
					ui.Text(c, spec.Help).FontSize(11).TextColor(p.muted).Wrap()
				}
			})
		}
	})
}

// pluginByStage finds the plugin and page a plugin stage names.
func (w *workbench) pluginByStage(stage string) (api.PluginInfo, api.PluginNavItem, bool) {
	id, page, ok := strings.Cut(strings.TrimPrefix(stage, stagePluginPrefix), "\x00")
	if !ok {
		return api.PluginInfo{}, api.PluginNavItem{}, false
	}
	for _, plugin := range w.features.plugins {
		if plugin.ID != id {
			continue
		}
		for _, item := range plugin.Navigation {
			if item.Page == page {
				return plugin, item, true
			}
		}
		return plugin, api.PluginNavItem{Title: page, Page: page}, true
	}
	return api.PluginInfo{ID: id, Name: id}, api.PluginNavItem{Title: page, Page: page}, true
}

// pluginPageStage shows a plugin page as a main-stage workspace.
func (w *workbench) pluginPageStage(c *ui.Context, p palette) {
	plugin, page, _ := w.pluginByStage(w.stage)
	if page.NativePanel == "remote-access" && plugin.Error == "" {
		w.remoteAccessStage(c, p)
		return
	}
	if page.NativePanel == "remote-desktop" && plugin.Error == "" {
		w.remoteDesktopStage(c, p)
		return
	}
	key := pluginKey(plugin.ID, page.Page)
	ui.Column(c.Key("plugin-stage:" + key)).Grow(1).MinHeight(0).Children(func() {
		w.stageHeader(c, p, plugIcon, plugin.Name+" · "+page.Title, func() {
			reload := iconButton(c, p, "plugin-page-reload", w.tr("重新载入页面", "Reload page"), refreshIcon).Size(28, 28).Padding(6)
			if w.features.busy["plugin-page:"+key] {
				reload.Disabled(true)
			}
			if reload.Clicked() {
				w.openPluginPage(plugin, page)
			}
		})
		ui.Scroll(c.Key("plugin-page-body")).Grow(1).MinHeight(0).Children(func() {
			ui.Column(c).Padding(20, 28).Gap(12).MaxWidth(980).FillWidth().Children(func() {
				switch {
				case w.plugs.pageErr[key] != "":
					ui.Text(c, w.plugs.pageErr[key]).FontSize(12).TextColor(p.danger).Wrap()
				case w.features.busy["plugin-page:"+key]:
					visualPending(c, p, w.tr("正在打开插件页面…", "Opening the plugin page…"))
				case w.plugs.pages[key] != "":
					w.pluginOutputView(c, p, w.plugs.pages[key])
				default:
					ui.Text(c, w.tr("页面没有内容。", "The page is empty.")).FontSize(12).TextColor(p.muted)
				}
			})
		})
	})
}
