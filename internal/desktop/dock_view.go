package desktop

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// The workbench dock holds the project tools (files, Git, SSH, the
// terminal and plugins) beside the conversation, at the full height of the
// main stage, as an IDE's tool window: never a strip squeezed between the
// timeline and the composer. Wide stages split; ⤢ gives the tools the whole
// stage; narrow stages show the dock as a full-height sheet in place of the
// timeline, with the composer kept below it.
const (
	dockSideBreakpoint = 900
	dockMinWidth       = 340
	chatMinWidth       = 380
)

type dockMode int

const (
	dockNone dockMode = iota
	dockSide
	dockSheet
	dockMaximized
)

type dockState struct {
	// widths are the user's chosen widths by tool; zero is the default.
	widths       map[string]float32
	maximized    bool
	tabFocus     ui.Handle
	focusPending bool
}

// dockTabs are the tools of the dock's tab strip, in IDE order. The
// terminal holds both this computer's shell and SSH hosts; the older
// "remote" (SSH) panel opens as the terminal's host list.
var dockTabs = []headerTool{
	{"files", "文件", "Files", fileIcon},
	{"git", "Git", "Git", branchIcon},
	{"terminal", "终端", "Terminal", terminalIcon},
	{"plugins", "插件", "Plugins", toolIcon},
	{"mcp", "MCP", "MCP", serverIcon},
}

func defaultDockWidth(panel string) float32 {
	switch panel {
	case "git", "terminal":
		return 520
	}
	return 440
}

// mainStageWidth is the room right of the sidebar.
func (w *workbench) mainStageWidth(c *ui.Context, narrow bool) float32 {
	width, _ := c.Size()
	if narrow || w.widgets.sidebarClosed {
		return width
	}
	if width < 1180 {
		return width - sidebarNarrow
	}
	return width - sidebarWide
}

func (w *workbench) dockMode(c *ui.Context, narrow bool) dockMode {
	if w.features.panel == "" {
		return dockNone
	}
	if w.mainStageWidth(c, narrow) < dockSideBreakpoint {
		return dockSheet
	}
	if w.dock.maximized {
		return dockMaximized
	}
	return dockSide
}

// dockWidth is the side dock's width for the current tool: the user's or the
// default, kept between its minimum and what leaves the chat readable.
func (w *workbench) dockWidth(main float32) float32 {
	want := w.dock.widths[w.features.panel]
	if want == 0 {
		want = defaultDockWidth(w.features.panel)
	}
	return max(float32(dockMinWidth), min(want, main-chatMinWidth))
}

// openDock shows a tool in the dock, loading what it reads; a tab click
// never closes the dock, unlike a second click of its title-bar button.
func (w *workbench) openDock(id string) {
	if id == "remote" {
		// SSH hosts live in the terminal: show its host list.
		w.termTab.active = terminalHostsTab
		w.loadEnvironments()
		w.loadSSHHosts(false)
		if w.features.panel == "terminal" {
			return
		}
		w.features.panel = "terminal"
		w.stage = ""
		return
	}
	if w.features.panel == id {
		return
	}
	wasClosed := w.features.panel == ""
	w.features.panel = id
	w.stage = ""
	if wasClosed && id != "terminal" && id != "session" {
		w.dock.focusPending = true
	}
	switch id {
	case "terminal":
		if w.termTab.active != terminalHostsTab && len(w.sshSessions()) == 0 && len(w.terminalTaskSessions()) == 0 {
			w.openTerminal()
		}
	case "git":
		w.loadGit()
	case "plugins":
		w.loadPlugins()
	case "mcp":
		w.loadMCP()
	case "files":
		w.ensureFileListing()
	}
}

func (w *workbench) closeDock() {
	w.features.panel = ""
	w.dock.maximized = false
	w.widgets.composerFocus.Focus()
}

func (w *workbench) dockMotion(mode dockMode) ui.ElementTransition {
	// Only the entrance slides; the width follows a drag at once.
	transition := ui.ElementTransition{Duration: 160 * time.Millisecond, Colors: true, Enter: &ui.Motion{X: 18}, Exit: &ui.Motion{}}
	if mode == dockSheet {
		transition.Enter = &ui.Motion{Y: 10}
	}
	return transition
}

// dockResizeHandle is the divider between the chat and the dock: drag to
// resize, double-click to restore the tool's default width.
func (w *workbench) dockResizeHandle(c *ui.Context, p palette, main float32) {
	handle := ui.Box(c.Key("dock-resize")).Width(5).Shrink(0).Cursor(ui.CursorResizeEW).Role(ui.RoleSplitter).
		Label(w.tr("调整工具坞宽度", "Resize tool dock"))
	if w.dock.widths == nil {
		w.dock.widths = map[string]float32{}
	}
	if dx, _, ok := handle.Dragged(); ok && dx != 0 {
		w.dock.widths[w.features.panel] = max(float32(dockMinWidth), min(w.dockWidth(main)-dx, main-chatMinWidth))
	}
	if handle.DoubleClicked() {
		delete(w.dock.widths, w.features.panel)
	}
	color := p.lineSoft
	if handle.Hovered() || handle.Pressed() {
		color = p.accent.Alpha(0.55)
	}
	handle.Draw(func(painter *ui.Painter, r ui.Rect) {
		painter.Fill(ui.Rect{X: r.X + r.W/2 - 0.5, Y: r.Y, W: 1, H: r.H}, color, 0)
	})
}

func (w *workbench) dockView(c *ui.Context, p palette, mode dockMode, width float32) {
	if w.features.panel == "remote" {
		// The SSH hub is the terminal's host list.
		w.features.panel, w.termTab.active = "terminal", terminalHostsTab
	}
	id := w.features.panel
	dock := ui.Column(c.Key("workbench-dock:" + w.selected)).MinHeight(0).Clip().Background(p.surface).Transition(w.dockMotion(mode))
	dock.Role(ui.RoleRegion).Label(w.tr("工具坞", "Tool dock"))
	switch mode {
	case dockSide:
		dock.Width(width).Shrink(0).BorderWidth(0, 0, 0, 1).BorderColor(p.lineSoft)
	case dockSheet:
		dock.Grow(1).MinWidth(0).Background(p.surface).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft)
	default:
		dock.Grow(1).MinWidth(0)
	}
	if id != "terminal" && dock.Shortcut(0, ui.KeyEscape) {
		w.closeDock()
		return
	}
	dock.Children(func() {
		w.dockHeader(c, p, mode, width)
		var body ui.Element
		_, winH := c.Size()
		padV, gapV := float32(12), float32(10)
		if winH < 680 {
			padV, gapV = 6, 6
		}
		if id == "session" {
			body = ui.Scroll(c.Key("dock-body:"+id)).Grow(1).MinHeight(0).Gap(gapV).Padding(padV, 14, max(6, padV-2))
		} else {
			body = ui.Column(c.Key("dock-body:"+id)).Grow(1).MinHeight(0).Gap(gapV).Padding(padV, 14, max(6, padV-2))
		}
		body.Children(func() {
			switch id {
			case "session":
				w.sessionPanel(c, p)
			case "terminal":
				w.terminalDock(c, p)
			case "git":
				if !w.gitView.expanded {
					w.ideaGitPanel(c, p, mode)
				}
			case "plugins":
				w.pluginCenter(c, p)
			case "mcp":
				w.mcpPanel(c, p)
			case "files":
				w.filesPanel(c, p)
			}
		})
		if n := w.panelFeedback(); n.valid() {
			ui.Row(c.Key("dock-feedback")).Padding(6, 12).Shrink(0).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
				w.noticeLine(c, p, n)
			})
		}
	})
}

// dockHeader is the tool tab strip, then maximize and close.
func (w *workbench) dockHeader(c *ui.Context, p palette, mode dockMode, width float32) {
	window, _ := c.Size()
	if mode != dockSide {
		width = window
	}
	compact := width < 480
	ui.Row(c.Key("dock-header")).Height(42).Padding(0, 10, 0, 14).Gap(8).AlignItems(ui.Center).Shrink(0).
		BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
		if w.features.panel == "session" {
			ui.Icon(c, moreIcon).FontSize(13).TextColor(p.accentInk)
			ui.Text(c, w.tr("会话", "Thread")).FontSize(12).Bold().TextColor(p.ink).Shrink(0).NoWrap().Role(ui.RoleHeading).Level(3)
			ui.Text(c, w.titleFor(w.selected)).FontSize(11).TextColor(p.faint).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		} else {
			w.dockTabStrip(c, p, compact)
			ui.Spacer(c)
		}
		if mode != dockSheet {
			label, icon := w.tr("最大化工具坞", "Maximize tool dock"), expandIcon
			if mode == dockMaximized {
				label, icon = w.tr("还原工具坞", "Restore tool dock"), collapseIcon
			}
			toggle := iconButton(c, p, "dock-maximize", label, icon).Size(26, 26).Padding(5)
			if mode == dockMaximized {
				toggle.Background(p.selected).TextColor(p.accentInk)
			}
			if toggle.Clicked() {
				w.dock.maximized = !w.dock.maximized
			}
		}
		if iconButton(c, p, "dock-close", w.tr("关闭工具面板", "Close tool drawer"), closeIcon).Size(26, 26).Padding(5).Clicked() {
			w.closeDock()
		}
	})
}

func (w *workbench) dockTabStrip(c *ui.Context, p palette, compact bool) {
	ui.Row(c.Key("dock-tabs")).Height(30).Padding(2).Gap(2).Radius(8).Background(p.soft).AlignItems(ui.Center).Shrink(1).MinWidth(0).
		Role(ui.RoleTabList).Label(w.tr("工具页", "Tool tabs")).Children(func() {
		for _, tool := range dockTabs {
			name := w.tr(tool.zh, tool.en)
			selected := w.features.panel == tool.id
			tab := ui.Button(c.Key("dock-tab:"+tool.id), "").Label(w.tr("工具页 ", "Tool tab ")+name).Height(26).Radius(6).
				Border(0, p.line).Background(ui.Transparent).TextColor(p.muted).Role(ui.RoleTab).Selected(selected).Shrink(0)
			if compact {
				tab.Width(30).Padding(0)
				tab.Tooltip(name)
			} else {
				tab.Padding(0, 10)
			}
			if ok, why := w.toolAvailable(tool.id); !ok {
				tab.Disabled(true).Description(why)
			}
			if selected {
				tab.Bind(&w.dock.tabFocus)
				if w.dock.focusPending {
					w.dock.focusPending = false
					w.dock.tabFocus.Focus()
				}
				tab.Background(p.raised).TextColor(p.ink)
				if !p.dark {
					tab.Border(1, p.lineSoft)
				}
			} else if tab.Hovered() {
				tab.Background(p.lineSoft).TextColor(p.ink)
			}
			tab.Children(func() {
				ui.Row(c).Gap(5).AlignItems(ui.Center).Justify(ui.Center).FillWidth().Children(func() {
					color := p.muted
					if selected {
						color = p.accentInk
					}
					ui.Icon(c, tool.icon).FontSize(12.5).TextColor(color).Shrink(0)
					if !compact {
						ui.Text(c, name).FontSize(11.5).FontWeight(500).NoWrap()
					}
				})
			})
			if tab.Clicked() {
				w.openDock(tool.id)
			}
		}
	})
}

// makeTerminal shows a program in an embedded terminal. Vantry runs it in a
// pty of its own rather than letting the terminal library start it (see
// ptyProcess), and says in words why it ended.
func (w *workbench) makeTerminal(options terminal.Options) (*terminal.Terminal, error) {
	term, _, err := w.launchTerminal(options, options.Command, func(code int) string {
		if code == 0 {
			return w.tr("进程已退出", "Process exited")
		}
		return fmt.Sprintf(w.tr("进程已退出（代码 %d）", "Process exited with code %d"), code)
	})
	return term, err
}
