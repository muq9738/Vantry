package desktop

import (
	"runtime"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

var (
	helpIcon     = lucide("circle-help")
	forkIcon     = lucide("git-fork")
	trashIcon    = lucide("trash-2")
	upIcon       = lucide("chevron-up")
	downIcon     = lucide("chevron-down")
	planIcon     = lucide("list-todo")
	chatIcon     = lucide("message-circle")
	starIcon     = lucide("star")
	refreshIcon  = lucide("refresh-cw")
	pullIcon     = lucide("arrow-down-to-line")
	pushIcon     = lucide("arrow-up-from-line")
	stashIcon    = lucide("package")
	prIcon       = lucide("git-pull-request")
	folderUpIcon = lucide("folder-up")
	homeIcon     = lucide("house")
	collapseIcon = lucide("minimize-2")
	commitIcon   = lucide("git-commit-horizontal")
	logIcon      = lucide("history")
	rollbackIcon = lucide("undo-2")
	searchSmall  = lucide("search")
	boltIcon     = lucide("zap")
	plugIcon     = lucide("plug")
	githubIcon   = lucide("github")
	downloadIcon = lucide("download")
	linkIcon     = lucide("link")
	outlineIcon  = lucide("list-tree")
	quoteIcon    = lucide("quote")
)

// pill is a compact rounded label: status, branch or count. Tone colors the
// text and a translucent wash of the same hue, never a saturated block.
func pill(c *ui.Context, p palette, text string, tone ui.Color) ui.Element {
	row := ui.Row(c).Height(20).Padding(0, 8).Gap(5).AlignItems(ui.Center).Radius(10).Background(tone.Alpha(0.12)).Shrink(0)
	row.Children(func() {
		ui.Text(c, text).FontSize(11).TextColor(tone).SingleLine().NoWrap()
	})
	return row
}

// microTag is a small monospaced tag: where a host comes from, a version.
func microTag(c *ui.Context, p palette, text string, tone ui.Color) {
	ui.Row(c).Height(16).Padding(0, 5).Radius(4).AlignItems(ui.Center).Background(tone.Alpha(0.1)).Shrink(0).Children(func() {
		ui.Text(c, text).Font("Menlo").FontSize(9).TextColor(tone).NoWrap()
	})
}

// kbd renders a key cap for the shortcut sheet.
func kbd(c *ui.Context, p palette, key string) {
	ui.Row(c).Height(20).Padding(0, 6).AlignItems(ui.Center).Radius(5).Background(p.code).Border(1, p.line).Shrink(0).Children(func() {
		ui.Text(c, key).Font("Menlo").FontSize(11).TextColor(p.ink).NoWrap()
	})
}

// selectedBranch is the authoritative branch already in memory: a live Git
// read, the workspace inspector, then the thread's committed workspace spec,
// or another directory thread sharing the same local checkout on a new draft.
func (w *workbench) selectedBranch() string {
	if git := w.features.git[w.selected]; git != nil && git.Branch != "" {
		return git.Branch
	}
	if w.workspaceChanges != nil && w.workspaceThread == w.selected && w.workspaceChanges.Branch != "" {
		return w.workspaceChanges.Branch
	}
	if thread, ok := w.selectedThread(); ok && thread.Workspace != nil {
		return thread.Workspace.Branch
	}
	if spec := w.selectedWorkspace(); spec != nil {
		if spec.Branch != "" {
			return spec.Branch
		}
		if spec.Kind != "worktree" && spec.Path != "" {
			for _, th := range w.threads {
				if th.Workspace == nil || th.Workspace.Kind == "worktree" || th.Workspace.Path != spec.Path {
					continue
				}
				if git := w.features.git[th.ID]; git != nil && git.Branch != "" {
					return git.Branch
				}
				if th.Workspace.Branch != "" {
					return th.Workspace.Branch
				}
			}
		}
	}
	return ""
}

// sessionState is the one-word status of a session for the title bar pill.
// live marks the states drawn with a pulse.
func (w *workbench) sessionState(id string) (string, bool, func(palette) ui.Color) {
	if comp := w.composers[id]; comp != nil && comp.pending != nil && !comp.pending.accepted {
		if comp.pending.inFlight {
			return w.tr("发送中", "Sending"), true, func(p palette) ui.Color { return p.running }
		}
		return w.tr("待确认", "Unconfirmed"), false, func(p palette) ui.Color { return p.warning }
	}
	if w.needsYou(id) {
		return w.tr("需要你处理", "Needs you"), false, func(p palette) ui.Color { return p.warning }
	}
	if run, ok := w.activeRun(id); ok {
		if run.Status == api.RunWaiting {
			return w.tr("等待子任务", "Waiting on subtasks"), false, func(p palette) ui.Color { return p.warning }
		}
		return w.tr("运行中", "Running"), true, func(p palette) ui.Color { return p.running }
	}
	if run, ok := w.latestRun(id); ok {
		switch run.Status {
		case api.RunFailed, api.RunInterrupted:
			return w.tr("需检查", "Check"), false, func(p palette) ui.Color { return p.danger }
		case api.RunCompleted:
			return w.tr("已完成", "Done"), false, func(p palette) ui.Color { return p.success }
		case api.RunCancelled:
			return w.tr("已停止", "Stopped"), false, func(p palette) ui.Color { return p.muted }
		}
	}
	return "", false, nil
}

// breadcrumb is the title bar's reading line, as the workbench's: the project badge
// and name, a slash, the title, then a quiet status pill. Branch and
// worktree live in the ContextStrip.
func (w *workbench) breadcrumb(c *ui.Context, p palette, title string, narrow bool) {
	ui.Row(c).Grow(1).MinWidth(0).Gap(7).AlignItems(ui.Center).Children(func() {
		if spec := w.selectedWorkspace(); spec != nil && !narrow {
			name := w.workspaceLabel()
			projectBadge(c, name, 16)
			ui.Text(c, name).FontSize(13).TextColor(p.muted).MaxWidth(160).SingleLine().Ellipsis("…").Shrink(0)
			w.headerBranchPill(c, p, narrow)
			ui.Text(c, "/").FontSize(13).TextColor(p.faint)
		} else if !narrow && w.hasConversation(w.selected) {
			ui.Text(c, w.tr3("无项目", "No project", "プロジェクトなし")).FontSize(13).TextColor(p.faint).Shrink(0)
			ui.Text(c, "/").FontSize(13).TextColor(p.faint)
		}
		ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0).Role(ui.RoleHeading).Level(2)
		if text, live, tone := w.sessionState(w.selected); text != "" && w.hasConversation(w.selected) {
			color := tone(p)
			ui.Row(c).Height(20).Padding(0, 8).Gap(5).AlignItems(ui.Center).Radius(10).Background(color.Alpha(0.12)).Shrink(0).Label(w.tr("会话状态 ", "Thread status ") + text).Children(func() {
				if live {
					activityIndicator(c, color).Size(8, 8)
				} else {
					ui.Box(c).Size(6, 6).Radius(3).Background(color).Shrink(0)
				}
				ui.Text(c, text).FontSize(11).TextColor(color).NoWrap()
			})
		}
		ui.Spacer(c)
	})
}

func (w *workbench) selectedWorkspace() *api.WorkspaceSpec {
	spec := w.defaults.Workspace
	if thread, ok := w.selectedThread(); ok {
		spec = thread.Workspace
	}
	if spec == nil || spec.Path == "" && spec.Repository == "" {
		return nil
	}
	if isScratchWorkspacePath(api.WorkspaceProjectPath(spec)) {
		return nil
	}
	return spec
}

type shortcutEntry struct{ keys, label string }

// shortcutGroups is the cheat sheet's content, grouped by intent.
func (w *workbench) shortcutGroups() []struct {
	title   string
	entries []shortcutEntry
} {
	newlinePrimary := platformPrimaryNewlineKey(runtime.GOOS)
	send, newline, alternate := "Ctrl Enter", "Enter", "Ctrl ⇧ Enter"
	if w.widgets.sendWithEnter {
		send = "Enter"
		newline = newlinePrimary + " / ⇧ Enter"
		if runtime.GOOS == "darwin" {
			alternate = "Ctrl ⇧ Enter / ⌥ Enter"
		} else {
			alternate = "Alt Enter"
		}
	}
	t := w.tr
	return []struct {
		title   string
		entries []shortcutEntry
	}{
		{t("全局导航", "Global"), []shortcutEntry{
			{"⌘ N", t("新会话", "New thread")},
			{"⌘ K", t("搜索会话", "Search threads")},
			{"⌘ ,", t("打开设置", "Open settings")},
			{"F6", t("在会话导航与输入框之间切换焦点", "Move focus between the sidebar and the composer")},
			{"⌘ F", t("会话内搜索", "Find in thread")},
			{"⌘ /", t("打开或关闭快捷键速查", "Toggle this sheet")},
			{"Ctrl O", t("展开或收起当前会话工具明细", "Toggle tool details in thread")},
			{"Ctrl T", t("展开或收起当前会话思考过程", "Toggle reasoning blocks in thread")},
			{"Esc", t("关闭弹层、工具面板或搜索", "Close popovers, tool drawers or search")},
			{"Esc ×2", t("运行中停止当前任务；空闲时回退与检查点", "Stop active turn while running; open rewind when idle")},
		}},
		{t("会话导航", "Sidebar"), []shortcutEntry{
			{"↑ ↓", t("在会话之间移动", "Move between threads")},
			{"← →", t("折叠或展开当前项目", "Fold or unfold the project")},
			{"Enter", t("进入会话并聚焦输入框", "Open the thread and focus the composer")},
			{t("右键", "Right-click"), t("置顶、归档、分叉或删除会话", "Pin, settle, fork or delete")},
		}},
		{t("输入与发送", "Composer"), []shortcutEntry{
			{send, t("发送消息", "Send")},
			{newline, t("换行", "New line")},
			{alternate, t("运行中：在排队与调整本轮之间临时切换", "While running: switch queue / steer once")},
			{"@", t("引用工作区文件；↑ ↓ 选择，Enter / Tab 插入", "Mention a file; ↑ ↓ to pick, Enter / Tab to insert")},
			{"/", t("调用技能；↑ ↓ 选择，Enter / Tab 插入", "Run a skill; ↑ ↓ to pick, Enter / Tab to insert")},
		}},
		{t("顶栏工具", "Title bar tools"), []shortcutEntry{
			{t("文件 · 终端", "Files · Terminal"), t("浏览并引用项目文件；项目终端", "Browse and mention files; project shell")},
			{t("Git · 插件", "Git · Plugins"), t("Fetch / Pull / Push / 提交；命令插件", "Fetch / Pull / Push / commit; command plugins")},
			{t("远程 · 会话", "Remote · Thread"), t("远程工作台；重命名与会话选项", "Remote workbench; rename and thread options")},
		}},
	}
}

func (w *workbench) shortcutsDialog(c *ui.Context, p palette) {
	if !w.widgets.shortcutsOpen {
		return
	}
	width, height := c.Size()
	ui.DialogBase(c, &w.widgets.shortcutsOpen, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000055"))
		panel.Width(min(float32(620), width-32)).MaxHeight(height-48).Radius(14).Background(p.raised).Border(1, p.line).Label(w.tr("快捷键速查", "Keyboard shortcuts"))
		ui.Column(c).FillWidth().Children(func() {
			ui.Row(c).Height(48).Padding(0, 12, 0, 18).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
				ui.Icon(c, helpIcon).FontSize(15).TextColor(p.muted)
				ui.Text(c, w.tr("快捷键速查", "Keyboard shortcuts")).FontSize(14).Bold().Grow(1)
				ui.Text(c, w.tr("Esc 关闭", "Esc to close")).FontSize(11).TextColor(p.faint)
				if iconButton(c, p, "close-shortcuts", w.tr("关闭快捷键速查", "Close shortcuts"), closeIcon).Clicked() {
					w.widgets.shortcutsOpen = false
				}
			})
			columns := 2
			if width < 640 {
				columns = 1
			}
			groups := w.shortcutGroups()
			ui.Scroll(c.Key("shortcut-groups")).MaxHeight(height - 110).Children(func() {
				ui.Row(c).Padding(14, 18, 18).Gap(22).AlignItems(ui.Start).Wrap().Children(func() {
					for _, group := range groups {
						col := ui.Column(c.Key("shortcut:" + group.title)).Gap(8)
						if columns == 2 {
							col.Width(min(float32(270), (width-120)/2))
						} else {
							col.FillWidth()
						}
						col.Children(func() {
							ui.Text(c, group.title).FontSize(11).Bold().TextColor(p.muted)
							for _, entry := range group.entries {
								ui.Row(c).Gap(10).AlignItems(ui.Center).MinHeight(24).Children(func() {
									ui.Text(c, entry.label).FontSize(12).TextColor(p.ink).Grow(1).Wrap()
									kbd(c, p, entry.keys)
								})
							}
						})
					}
				})
			})
		})
	})
}
