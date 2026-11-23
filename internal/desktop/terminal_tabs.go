package desktop

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// sshKeyIcon marks a prompt that waits for the user.
var sshKeyIcon = lucide("keyboard")

// terminalHostsTab is the terminal's host list: the SSH machines it can
// open a terminal on, with adding, checking and editing them.
const terminalHostsTab = "\x00hosts"

// terminalTabState is the terminal dock's own tab strip: the remote hosts,
// this computer's shell for the thread and one tab per SSH session, as an
// IDE shows its terminals.
type terminalTabState struct {
	// active is "" for the local terminal of the thread, terminalHostsTab
	// for the host list, otherwise the target of the SSH session.
	active string
	// copied marks the session whose command was just copied, for the
	// status bar's confirmation.
	copied   string
	copiedAt time.Time
}

// projectTerminal is this computer's shell for the thread being read.
func (w *workbench) projectTerminal() *terminal.Terminal {
	return w.features.terminals[w.selected]
}

// terminalDock is the terminal dock: its tabs, the status bar of the active
// session, and the terminal itself.
func (w *workbench) terminalDock(c *ui.Context, p palette) {
	active := w.termTab.active
	if active != "" && active != terminalHostsTab && w.ssh.sessions[active] == nil {
		active, w.termTab.active = "", ""
	}
	w.terminalTabStrip(c, p)
	switch {
	case active == terminalHostsTab:
		w.sshHub(c, p)
	case active != "":
		w.sshTerminalPane(c, p, w.ssh.sessions[active])
	case w.projectTerminal() == nil:
		w.terminalEmptyState(c, p)
	default:
		w.projectTerminalPane(c, p)
	}
}

// sshSessions lists the SSH sessions in the order they were opened.
func (w *workbench) sshSessions() []*sshSession {
	var out []*sshSession
	for _, target := range w.ssh.order {
		if session := w.ssh.sessions[target]; session != nil {
			out = append(out, session)
		}
	}
	return out
}

// terminalTabStrip is the row of terminals: the project shell, one tab per
// machine with its status dot, and the way to add another.
func (w *workbench) terminalTabStrip(c *ui.Context, p palette) {
	ui.Row(c.Key("terminal-tabs")).Height(30).Gap(4).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Scroll(c.Key("terminal-tab-scroll")).Row().Grow(1).MinWidth(0).Gap(4).AlignItems(ui.Center).Children(func() {
			w.terminalHostsTabButton(c, p)
			if w.projectTerminal() != nil {
				w.terminalTab(c, p, "", w.tr("本机", "Local"), true, p.success)
			}
			for _, session := range w.sshSessions() {
				session.refresh()
				phase, _, _ := session.view()
				w.terminalTab(c, p, session.target, session.tabLabel(), true, sshDotColor(p, phase))
			}
		})
		add := iconButton(c, p, "terminal-add", w.tr("打开终端", "Open a terminal"), plusIcon).Size(26, 26).Padding(6)
		add.PopoverMenuButton(func(m *ui.Menu) {
			local := w.tr("本机终端", "This computer")
			if machine := w.terminalRemoteMachine(); machine != "" {
				local = machine
			}
			if m.Item(local).Disabled(w.projectTerminal() != nil).Chosen() {
				w.openTerminal()
			}
			if m.Item(w.tr("远程主机…", "Remote hosts…")).Chosen() {
				w.termTab.active = terminalHostsTab
				w.loadSSHHosts(false)
			}
			hosts := w.sshRelayHosts()
			if len(hosts) > 0 {
				m.Separator()
				for _, host := range hosts {
					host := host
					item := m.Item(w.tr("连接 ", "Connect ") + host)
					if w.ssh.sessions[host] != nil {
						item.Disabled(true)
					}
					if item.Chosen() {
						w.openSSHTerminal(host)
					}
				}
			}
		})
	})
}

// terminalTab is one tab: a status dot, its name, and closing it.
func (w *workbench) terminalTab(c *ui.Context, p palette, target, name string, live bool, dot ui.Color) {
	selected := w.termTab.active == target
	key := "terminal-tab:" + target
	if target == "" {
		key = "terminal-tab:project"
	}
	tab := ui.Button(c.Key(key), "").Label(w.tr("终端 ", "Terminal ")+name).Tooltip(name).Height(26).Padding(0, 4, 0, 6).Radius(7).
		Border(1, func() ui.Color {
			if selected {
				return p.line
			}
			return ui.Transparent
		}()).Background(func() ui.Color {
		if selected {
			return p.raised
		}
		return p.soft
	}()).TextColor(p.ink).Shrink(0).Role(ui.RoleTab).AccessSelected(selected)
	if !selected && tab.Hovered() {
		tab.Background(p.lineSoft)
	}
	tab.Children(func() {
		ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
			color := dot
			if !live {
				color = p.faint
			}
			ui.Box(c).Size(6, 6).Radius(3).Background(color).Shrink(0)
			ui.Text(c, name).FontSize(11).NoWrap().MaxWidth(120).SingleLine().Ellipsis("…")
			close := iconButton(c, p, "close:"+target, w.tr("关闭终端 ", "Close terminal ")+name, closeIcon).Size(16, 16).Padding(3)
			if close.Clicked() {
				if target == "" {
					if term := w.projectTerminal(); term != nil {
						_ = term.Close()
						delete(w.features.terminals, w.selected)
					}
				} else {
					w.closeSSHTerminal(target)
				}
				return
			}
		})
	})
	if tab.Clicked() {
		w.termTab.active = target
	}
}

// terminalHostsTabButton is the leading tab: the remote hosts a terminal
// can open on.
func (w *workbench) terminalHostsTabButton(c *ui.Context, p palette) {
	selected := w.termTab.active == terminalHostsTab
	name := w.tr("远程主机", "Hosts")
	tab := ui.Button(c.Key("terminal-tab:hosts"), "").Label(w.tr("远程主机列表", "Remote hosts")).Tooltip(w.tr("SSH 远程主机：连接、添加与检测", "SSH hosts: connect, add and check")).
		Height(26).Padding(0, 8).Radius(7).Border(1, ui.Transparent).Background(p.soft).TextColor(p.muted).Shrink(0).Role(ui.RoleTab).AccessSelected(selected)
	if selected {
		tab.Background(p.raised).Border(1, p.line).TextColor(p.ink)
	} else if tab.Hovered() {
		tab.Background(p.lineSoft).TextColor(p.ink)
	}
	tab.Children(func() {
		ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, serverIcon).FontSize(12)
			ui.Text(c, name).FontSize(11).NoWrap()
		})
	})
	if tab.Clicked() {
		w.termTab.active = terminalHostsTab
		w.loadSSHHosts(false)
	}
}

// projectTerminalPane is this computer's shell for the thread.
func (w *workbench) projectTerminalPane(c *ui.Context, p palette) {
	term := w.projectTerminal()
	if term == nil {
		w.terminalEmptyState(c, p)
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Box(c).Size(7, 7).Radius(4).Background(p.success).Shrink(0)
		where := w.tr("本机终端", "This computer")
		if dir := w.selectedWorkspacePath(); dir != "" {
			where += " · " + filepath.Base(dir)
		}
		ui.Text(c, where).FontSize(11).TextColor(p.muted).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		if quietButton(c, p, "close-terminal", w.tr("关闭此终端", "Close terminal")).Height(24).Clicked() {
			_ = term.Close()
			delete(w.features.terminals, w.selected)
			w.widgets.composerFocus.Focus()
		}
	})
	ui.Column(c).Grow(1).MinHeight(0).Radius(8).Clip().Border(1, p.lineSoft).Children(func() {
		terminal.View(c, term).Grow(1).FillWidth().Label(w.tr("本机终端输入", "Local terminal input"))
	})
}

// sshTerminalPane is one machine's terminal, under the bar that says where
// the connection stands.
func (w *workbench) sshTerminalPane(c *ui.Context, p palette, session *sshSession) {
	session.refresh()
	phase, failure, _ := session.view()
	w.sshTerminalStatus(c, p, session, phase, failure)
	ui.Column(c.Key("ssh-terminal:"+session.target)).Grow(1).MinHeight(0).Radius(8).Clip().Border(1, p.lineSoft).Children(func() {
		terminal.View(c, session.term).Grow(1).FillWidth().Label("SSH " + session.target)
	})
	// A session still being made shows its state without waiting for
	// output, which a slow network may take seconds to bring.
	if phase == sshConnecting {
		c.After(500 * time.Millisecond)
	}
}

// sshTerminalStatus is the bar above a machine's terminal: the state of the
// connection, and what to do about it.
func (w *workbench) sshTerminalStatus(c *ui.Context, p palette, session *sshSession, phase sshPhase, failure *sshFailure) {
	if phase == sshReady || phase == sshConnecting {
		ui.Row(c.Key("ssh-status")).Gap(8).AlignItems(ui.Center).Shrink(0).Padding(0, 2, 4).Children(func() {
			if phase == sshConnecting {
				activityIndicator(c, p.accent)
				ui.Text(c, w.tr("正在连接 ", "Connecting to ")+session.name+"…").FontSize(11).TextColor(p.muted).Grow(1).SingleLine().Ellipsis("…")
			} else {
				ui.Box(c).Size(7, 7).Radius(4).Background(p.success).Shrink(0)
				ui.Text(c, session.name+w.tr(" · 已连接", " · connected")).FontSize(11).TextColor(p.muted).Grow(1).SingleLine().Ellipsis("…")
			}
			w.sshTerminalActions(c, p, session, false)
		})
		return
	}
	if phase == sshPrompting {
		ui.Row(c.Key("ssh-status")).Gap(8).AlignItems(ui.Center).Shrink(0).Padding(8, 10, 6).Radius(8).Background(p.warning.Alpha(0.1)).Children(func() {
			ui.Icon(c, sshKeyIcon).FontSize(13).TextColor(p.warning).Shrink(0)
			ui.Text(c, w.tr("等待你在终端里输入", "Waiting for your input in the terminal")).FontSize(11.5).TextColor(p.warning).Grow(1).SingleLine().Ellipsis("…")
			if ui.Button(c, w.tr("转到这里", "Focus")).Height(24).Padding(0, 10).Border(1, p.warning.Alpha(0.5)).Background(ui.Transparent).TextColor(p.warning).Clicked() {
				w.widgets.composerFocus.Focus()
			}
		})
		return
	}
	panel := ui.Column(c.Key("ssh-status")).Gap(6).Padding(10, 12).Radius(8).Shrink(0).Background(func() ui.Color {
		if phase == sshFailed {
			return p.danger.Alpha(0.1)
		}
		return p.soft
	}())
	panel.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			color := p.faint
			icon := serverIcon
			if phase == sshFailed {
				color, icon = p.danger, alertIcon
			}
			ui.Icon(c, icon).FontSize(14).TextColor(color).Shrink(0)
			ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
				title := w.tr("连接已断开", "Connection closed")
				if phase == sshFailed && failure != nil {
					title = failure.title
				}
				ui.Text(c, session.name+" · "+title).FontSize(12).FontWeight(600).TextColor(func() ui.Color {
					if phase == sshFailed {
						return p.danger
					}
					return p.ink
				}()).SingleLine().Ellipsis("…")
				if phase == sshFailed && failure != nil {
					ui.Text(c, failure.detail).FontSize(11).TextColor(p.muted).Wrap()
					if failure.raw != "" {
						ui.Text(c, failure.raw).Font("Menlo").FontSize(10).TextColor(p.faint).SingleLine().Ellipsis("…")
					}
				}
			})
		})
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			if ui.PrimaryButton(c.Key("ssh-reconnect"), w.tr("重新连接", "Reconnect")).Height(26).Padding(0, 12).Clicked() {
				w.reconnectSSHTerminal(session.target)
			}
			w.sshTerminalActions(c, p, session, true)
		})
	})
}

// sshTerminalActions are the secondary actions of a session's bar.
func (w *workbench) sshTerminalActions(c *ui.Context, p palette, session *sshSession, compact bool) {
	copyLabel := w.tr("复制命令", "Copy command")
	if w.termTab.copied == session.target && time.Since(w.termTab.copiedAt) < 3*time.Second {
		copyLabel = w.tr("已复制", "Copied")
	}
	if quietButton(c, p, "ssh-copy", copyLabel).Label(w.tr("复制 ssh 命令 ", "Copy ssh command ") + session.target).Height(24).FontSize(11).Clicked() {
		c.WriteClipboard(session.command)
		w.termTab.copied, w.termTab.copiedAt = session.target, time.Now()
	}
	if compact {
		if quietButton(c, p, "ssh-close", w.tr("关闭终端", "Close terminal")).Height(24).FontSize(11).Clicked() {
			w.closeSSHTerminal(session.target)
		}
		return
	}
	more := iconButton(c, p, "ssh-more", w.tr("会话操作 ", "Session actions ")+session.target, moreIcon).Size(24, 24).Padding(5)
	more.PopoverMenuButton(func(m *ui.Menu) {
		if m.Item(w.tr("复制 ssh 命令", "Copy ssh command")).Chosen() {
			c.WriteClipboard(session.command)
			w.termTab.copied, w.termTab.copiedAt = session.target, time.Now()
		}
		if m.Item(w.tr("显示主机", "Show host")).Chosen() {
			w.ssh.selected = session.target
			w.termTab.active = terminalHostsTab
		}
		m.Separator()
		if m.Item(w.tr("关闭终端", "Close terminal")).Chosen() {
			w.closeSSHTerminal(session.target)
		}
	})
}

// terminalEmptyState is the terminal before anything runs there: open this
// computer's shell, or one on a remote host.
func (w *workbench) terminalEmptyState(c *ui.Context, p palette) {
	machine := w.terminalRemoteMachine()
	ui.Column(c.Key("terminal-empty")).Grow(1).MinHeight(0).Justify(ui.Center).AlignItems(ui.Center).Gap(10).Padding(24).Children(func() {
		ui.Box(c).Size(44, 44).Radius(12).Background(p.soft).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
			ui.Icon(c, terminalIcon).FontSize(20).TextColor(p.muted)
		})
		ui.Text(c, w.tr("这里放终端", "Terminals live here")).FontSize(13).FontWeight(600).TextColor(p.ink)
		body := w.tr("在本机项目目录打开 shell，或通过 SSH 连接远程主机。每个终端是一个标签，断开时会说明原因并可直接重连。", "Open a shell on this computer in the project folder, or connect to a remote host over SSH. Each terminal is a tab; a dropped one says why and offers to reconnect.")
		if machine != "" {
			body = fmt.Sprintf(w.tr("这个会话运行在「%s」上，通过它的 SSH 主机打开终端。", "This thread runs on %s; open a terminal through its SSH host."), machine)
		}
		ui.Text(c, body).FontSize(11).TextColor(p.muted).TextAlign(ui.Center).MaxWidth(320).Wrap()
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if machine == "" {
				if ui.PrimaryButton(c.Key("start-terminal"), w.tr("打开本机终端", "Open local terminal")).Height(28).Padding(0, 12).Disabled(w.features.busy["terminal:"+w.selected]).Clicked() {
					w.openTerminal()
				}
			}
			if quietButton(c, p, "open-hosts", w.tr("连接远程主机", "Connect a remote host")).Height(28).Padding(0, 10).Clicked() {
				w.termTab.active = terminalHostsTab
				w.loadSSHHosts(false)
			}
		})
	})
}

// sshTabLabel is the name under a terminal tab: the alias, shortened, and the
// port of a manual entry that is not 22.
func (s *sshSession) tabLabel() string {
	if s == nil {
		return ""
	}
	name := strings.TrimSpace(s.name)
	if name == "" {
		name = s.target
	}
	if len(name) > 22 {
		name = name[:22] + "…"
	}
	return name
}
