package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// The host page of the dock. It shows the machines of the SSH configuration
// and the ones added here as one list, each with where it leads, where it
// came from and how it last answered; a machine opened shows the two things
// one does with it (a terminal, a remote workbench) and keeps the rest behind
// its menu. The remote desktop of a plugin and the connections that need a
// raw address live further down, out of the way of that list.

// zapIcon marks a machine's latency.
var zapIcon = lucide("zap")

// sshHub is the host page.
func (w *workbench) sshHub(c *ui.Context, p palette) {
	w.loadSSHHosts(false)
	if w.ssh.add.open {
		w.sshHostFormView(c, p)
		return
	}
	entries := w.sshEntries()
	w.sshToolbar(c, p)
	// A dock given the whole stage keeps the page readable rather than
	// stretching one row across the window.
	width, _ := c.Size()
	ui.Scroll(c.Key("ssh-hosts")).Grow(1).MinHeight(0).Children(func() {
		ui.Column(c.Key("ssh-page")).FillWidth().AlignItems(ui.Center).Children(func() {
			page := ui.Column(c.Key("ssh-page-body")).FillWidth().Gap(4).Children(func() {
				w.sshHostSection(c, p, entries)
				w.sshRemoteDesktopSection(c, p)
				w.sshAdvanced(c, p)
			})
			if width > 820 {
				page.MaxWidth(760)
			}
		})
	})
}

// sshToolbar is the search and the way to add a machine, as wide as the
// page it sits above.
func (w *workbench) sshToolbar(c *ui.Context, p palette) {
	width, _ := c.Size()
	ui.Row(c.Key("ssh-toolbar-wrap")).FillWidth().Justify(ui.Center).Shrink(0).Children(func() {
		bar := ui.Row(c.Key("ssh-toolbar")).Gap(6).AlignItems(ui.Center).FillWidth()
		if width > 820 {
			bar.MaxWidth(760)
		}
		bar.Children(func() {
			ui.TextInput(c.Key("ssh-search"), &w.ssh.search).Label(w.tr("搜索主机", "Search hosts")).
				Placeholder(w.tr("搜索主机、别名或地址", "Search hosts, aliases or addresses")).Grow(1).MinWidth(0).Height(30).Padding(0, 10)
			add := quietButton(c, p, "ssh-add", "").Label(w.tr("添加主机", "Add host")).Height(30).Padding(0, 9)
			add.Border(1, p.line).Background(p.raised).TextColor(p.ink)
			add.Children(func() {
				ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, plusIcon).FontSize(12).TextColor(p.muted)
					ui.Text(c, w.tr("添加", "Add")).FontSize(11.5)
				})
			})
			if add.Clicked() {
				w.openSSHHostForm("")
			}
		})
	})
}

// sshHostSection is the list of machines with its states.
func (w *workbench) sshHostSection(c *ui.Context, p palette, entries []sshEntry) {
	ui.Row(c.Key("ssh-host-title")).Height(26).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.Text(c, w.tr("主机", "Hosts")).FontSize(11).Bold().TextColor(p.muted)
		ui.Text(c, fmt.Sprint(len(entries))).FontSize(10).TextColor(p.faint)
		ui.Spacer(c)
		menu := iconButton(c, p, "host-list-menu", w.tr("主机列表菜单", "Host list menu"), moreIcon).Size(24, 24).Padding(5)
		menu.PopoverMenuButton(func(m *ui.Menu) {
			if m.Item(w.tr("重新读取 SSH 配置", "Read the SSH configuration again")).Disabled(w.features.busy["ssh-hosts"]).Chosen() {
				w.loadSSHHosts(true)
			}
			if m.Item(w.tr("恢复已移除的主机", "Restore removed hosts")).Disabled(len(w.remote.removed) == 0).Chosen() {
				w.restoreSSHEntries()
			}
		})
	})
	switch {
	case len(entries) == 0 && (w.ssh.loading || !w.ssh.loaded && w.features.busy["ssh-hosts"]):
		w.sshHostsLoading(c, p)
	case len(entries) == 0 && w.ssh.err != "":
		w.sshHostsError(c, p)
	case len(entries) == 0 && strings.TrimSpace(w.ssh.search) != "":
		w.sshHostsNoMatch(c, p)
	case len(entries) == 0:
		w.sshHostsEmpty(c, p)
	}
	for _, e := range entries {
		w.sshHostRow(c, p, e)
	}
}

// sshHostsLoading is the list before the configuration has been read.
func (w *workbench) sshHostsLoading(c *ui.Context, p palette) {
	ui.Column(c.Key("ssh-loading")).Gap(8).Padding(12).Children(func() {
		for i := 0; i < 3; i++ {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(7, 7).Radius(4).Background(p.lineSoft)
				ui.Column(c).Gap(5).Grow(1).Children(func() {
					ui.Box(c).Height(9).Width(96).Radius(4).Background(p.lineSoft)
					ui.Box(c).Height(8).Width(148).Radius(4).Background(p.lineSoft.Alpha(0.7))
				})
			})
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			activityIndicator(c, p.accent)
			ui.Text(c, w.tr("正在读取 ~/.ssh/config…", "Reading ~/.ssh/config…")).FontSize(11).TextColor(p.muted)
		})
	})
}

// sshHostsError says the configuration could not be read, and offers to try.
func (w *workbench) sshHostsError(c *ui.Context, p palette) {
	ui.Column(c.Key("ssh-error")).Gap(8).Padding(14).Radius(10).Background(p.danger.Alpha(0.08)).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, alertIcon).FontSize(14).TextColor(p.danger).Shrink(0)
			ui.Text(c, w.tr("读不到 SSH 配置", "The SSH configuration could not be read")).FontSize(12).FontWeight(600).TextColor(p.ink).Grow(1)
		})
		ui.Text(c, w.ssh.err).FontSize(11).TextColor(p.muted).Wrap()
		ui.Row(c).Gap(6).Children(func() {
			if quietButton(c, p, "ssh-retry", w.tr("重试", "Try again")).Height(26).Padding(0, 10).Clicked() {
				w.loadSSHHosts(true)
			}
			if quietButton(c, p, "ssh-error-add", w.tr("手动添加", "Add a host")).Height(26).Padding(0, 10).Clicked() {
				w.openSSHHostForm("")
			}
		})
	})
}

// sshHostsNoMatch is the search with nothing to show for it.
func (w *workbench) sshHostsNoMatch(c *ui.Context, p palette) {
	ui.Column(c.Key("ssh-no-match")).Gap(6).Padding(18, 14).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, searchIcon).FontSize(18).TextColor(p.faint)
		ui.Text(c, w.tr("没有匹配「", "Nothing matches \"")+w.ssh.search+"」").FontSize(12).TextColor(p.muted).Wrap().TextAlign(ui.Center)
		if quietButton(c, p, "ssh-clear-search", w.tr("清除搜索", "Clear the search")).Height(26).Padding(0, 10).Clicked() {
			w.ssh.search = ""
		}
	})
}

// sshHostsEmpty is the list of a user who has no machines yet.
func (w *workbench) sshHostsEmpty(c *ui.Context, p palette) {
	ui.Column(c.Key("ssh-empty")).Gap(9).Padding(22, 14).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(40, 40).Radius(11).Background(p.soft).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
			ui.Icon(c, serverIcon).FontSize(18).TextColor(p.muted)
		})
		ui.Text(c, w.tr("还没有主机", "No hosts yet")).FontSize(12.5).FontWeight(600).TextColor(p.ink)
		ui.Text(c, w.tr("Vantry 读取 ~/.ssh/config 里的主机；也可以手动添加一台，填地址、用户与密钥。", "Vantry reads the hosts of ~/.ssh/config; you can also add one by hand with its address, user and key.")).
			FontSize(11).TextColor(p.muted).TextAlign(ui.Center).Wrap()
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			if ui.PrimaryButton(c.Key("ssh-empty-add"), w.tr("添加主机", "Add a host")).Height(27).Padding(0, 12).Clicked() {
				w.openSSHHostForm("")
			}
			if quietButton(c, p, "ssh-empty-reload", w.tr("重新读取", "Read again")).Height(27).Padding(0, 10).Clicked() {
				w.loadSSHHosts(true)
			}
		})
	})
}

// sshHostRow is one machine: its status, its name, where it leads, where it
// comes from, and when it was last used.
func (w *workbench) sshHostRow(c *ui.Context, p palette, e sshEntry) {
	selected := w.ssh.selected == e.key
	probe, busy := w.probeFor(e), w.probing(e)
	card := ui.Column(c.Key("ssh-host:" + e.key)).Gap(0).Radius(10).Shrink(0)
	switch {
	case selected:
		card.Background(p.soft).Border(1, p.line)
	case card.Hovered():
		card.Background(p.soft)
	}
	card.Children(func() {
		ui.Row(c.Key("ssh-host-row:"+e.key)).Padding(9, 10).Gap(8).AlignItems(ui.Center).Children(func() {
			pick := ui.Button(c.Key("pick"), "").Label(w.tr("主机 ", "Host ")+e.name).Expanded(selected).
				Grow(1).MinWidth(0).Padding(0).Border(0, p.line).Background(ui.Transparent).TextColor(p.ink)
			pick.Role(ui.RoleButton)
			pick.Children(func() {
				ui.Column(c).Gap(3).FillWidth().Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Text(c, e.name).FontSize(12.5).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
						microTag(c, p, w.sourceLabel(e), p.faint)
					})
					ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
						ui.Text(c, e.destination()).Font("Menlo").FontSize(10).TextColor(p.muted).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
						if last := w.ssh.recent[e.key]; !last.IsZero() {
							ui.Text(c, "· "+w.formatAge(c.Now(), last)).FontSize(10).TextColor(p.faint).NoWrap().Shrink(0)
						}
					})
				})
			})
			if pick.Clicked() {
				if selected {
					w.ssh.selected = ""
				} else {
					w.ssh.selected = e.key
					w.features.remoteHost, w.features.remoteSocket = e.target, e.socket
				}
			}
			w.sshHostStatus(c, p, e, probe, busy)
		})
		if selected {
			w.sshHostActions(c, p, e, probe, busy)
		}
	})
}

// sshHostStatus is the state of a machine: not tested, testing, its latency
// when it answers, or why it does not.
func (w *workbench) sshHostStatus(c *ui.Context, p palette, e sshEntry, probe hostProbe, busy bool) {
	if busy {
		ui.Row(c).Key("ssh-status-" + e.key).Gap(5).AlignItems(ui.Center).Shrink(0).Role(ui.RoleStatus).
			Label(w.tr("正在检测 ", "Testing ") + e.name).Children(func() {
			activityIndicator(c, p.running)
			ui.Text(c, w.tr("检测中", "Testing")).FontSize(10).TextColor(p.muted).NoWrap()
		})
		return
	}
	text, color := w.hostStatusText(p, e, probe)
	ui.Row(c).Key("ssh-status-" + e.key).Gap(5).AlignItems(ui.Center).Shrink(0).Role(ui.RoleStatus).Label(text).Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(color).Shrink(0)
		ui.Text(c, text).FontSize(10).TextColor(color).NoWrap()
	})
}

// hostStatusText is the words and the colour of a machine's state.
func (w *workbench) hostStatusText(p palette, e sshEntry, probe hostProbe) (string, ui.Color) {
	switch {
	case !probe.tested:
		return w.tr("未检测", "Not tested"), p.faint
	case probe.reachable:
		if probe.latency > 0 {
			return w.tr("在线", "Online") + " " + latencyText(probe.latency), p.success
		}
		return w.tr("在线", "Online"), p.success
	case probe.kind == "auth":
		return w.tr("认证失败", "Sign-in failed"), p.warning
	case probe.kind == "hostkey":
		return w.tr("身份异常", "Host key differs"), p.warning
	}
	return w.tr("离线", "Offline"), p.danger
}

// latencyText is how long a machine took to answer, short.
func latencyText(ms int) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}
	return fmt.Sprintf("%.1f s", float64(ms)/1000)
}

// sshHostActions is what an opened machine offers: a terminal, a remote
// workbench, and behind its menu the rest.
func (w *workbench) sshHostActions(c *ui.Context, p palette, e sshEntry, probe hostProbe, busy bool) {
	ui.Column(c.Key("ssh-host-detail:"+e.key)).Gap(8).Padding(0, 10, 10).Children(func() {
		if probe.tested && !probe.reachable && probe.message != "" {
			ui.Row(c).Gap(6).AlignItems(ui.Start).Children(func() {
				ui.Icon(c, alertIcon).FontSize(12).TextColor(p.warning).Shrink(0)
				ui.Text(c, probe.message).FontSize(10.5).TextColor(p.warning).Wrap().Grow(1).MinWidth(0)
			})
		}
		if probe.tested && probe.reachable {
			w.sshProbeFacts(c, p, probe)
		}
		if others := e.otherAliases(); len(others) > 0 {
			ui.Text(c, w.tr("也叫 ", "Also known as ")+strings.Join(others, "、")).FontSize(10).TextColor(p.faint).SingleLine().Ellipsis("…")
		}
		if e.identity != "" {
			ui.Text(c, w.tr("密钥 ", "Key ")+e.identity).Font("Menlo").FontSize(10).TextColor(p.faint).SingleLine().Ellipsis("…")
		}
		if jump := e.jumpNote(); jump != "" {
			ui.Text(c, w.tr("经由 ", "Through ")+jump).Font("Menlo").FontSize(10).TextColor(p.faint).SingleLine().Ellipsis("…")
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			terminal := ui.PrimaryButton(c.Key("terminal"), w.tr("打开终端", "Open terminal")).
				Label(w.tr("打开终端 ", "Open terminal ")+e.name).Height(28).Grow(1).Padding(0, 10)
			terminal.Tooltip(w.tr("在这台机器上开一个终端", "Open a terminal on this machine"))
			if terminal.Clicked() {
				w.openSSHEntry(e)
			}
			workbench := quietButton(c, p, "workbench", w.tr("连接远端工作台", "Remote workbench")).
				Label(w.tr("连接远端工作台 ", "Remote workbench ")+e.name).Height(28).Grow(1).Padding(0, 6)
			workbench.Border(1, p.line).Background(p.raised).TextColor(p.ink)
			if workbench.Clicked() {
				w.connectRemoteVantry(e)
			}
			w.sshHostMenu(c, p, e, busy)
		})
	})
}

// sshHostMenu keeps the rest of a machine's actions together.
func (w *workbench) sshHostMenu(c *ui.Context, p palette, e sshEntry, busy bool) {
	more := iconButton(c, p, "host-more", w.tr("更多操作 ", "More actions ")+e.name, moreIcon).Size(28, 28).Padding(6)
	if busy {
		more.Disabled(true)
	}
	more.Border(1, p.line).Background(p.raised).TextColor(p.ink)
	more.PopoverMenuButton(func(m *ui.Menu) {
		if m.Item(w.tr("检测连接", "Test the connection")).Chosen() {
			w.probeSSHEntry(e)
		}
		if m.Item(w.tr("复制 ssh 命令", "Copy the ssh command")).Chosen() {
			c.WriteClipboard(sshCommandLine(e.sshArgv()))
			w.setToolFeedback("remote", notice{Text: w.tr("已复制：", "Copied: ") + sshCommandLine(e.sshArgv()), Level: noticeInfo})
		}
		edit := m.Item(w.tr("编辑", "Edit"))
		if e.source != "manual" {
			edit.Disabled(true)
		}
		if edit.Chosen() {
			w.openSSHHostForm(e.key)
		}
		m.Separator()
		if m.Item(w.tr("移除", "Remove")).Chosen() {
			w.removeHost(e)
		}
	})
}

// sshProbeFacts is what testing a machine found: how fast it answered, what
// it runs, and whether Vantry is installed there.
func (w *workbench) sshProbeFacts(c *ui.Context, p palette, probe hostProbe) {
	ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
		if probe.latency > 0 {
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, zapIcon).FontSize(11).TextColor(p.success)
				ui.Text(c, latencyText(probe.latency)).Font("Menlo").FontSize(10).TextColor(p.success)
			})
		}
		if probe.remoteOS != "" {
			ui.Text(c, "·").FontSize(10).TextColor(p.faint)
			ui.Text(c, osLabel(probe.remoteOS)).FontSize(10).TextColor(p.muted).SingleLine().Ellipsis("…").MaxWidth(200)
		}
		ui.Text(c, "·").FontSize(10).TextColor(p.faint)
		if probe.socket != "" {
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, checkIcon).FontSize(11).TextColor(p.success)
				ui.Text(c, w.tr("已安装 Vantry", "Vantry installed")).FontSize(10).TextColor(p.success)
			})
		} else {
			ui.Text(c, w.tr("未安装 Vantry", "No Vantry yet")).FontSize(10).TextColor(p.faint)
		}
	})
}

// osLabel shortens uname's answer to the system and its release.
func osLabel(uname string) string {
	fields := strings.Fields(uname)
	if len(fields) < 2 {
		return uname
	}
	system := fields[0]
	if system == "Darwin" {
		system = "macOS"
	}
	return system + " " + fields[1]
}

// otherAliases are the other names of a machine in the SSH configuration.
func (e sshEntry) otherAliases() []string {
	var others []string
	for _, alias := range e.host.Aliases {
		if alias != e.host.Alias && alias != e.target && alias != "" {
			others = append(others, alias)
		}
	}
	return others
}

// sshRemoteDesktopSection is the plugin's remote desktop, kept out of the
// machine list: it is another device, reached another way.
func (w *workbench) sshRemoteDesktopSection(c *ui.Context, p palette) {
	plugin, page, ok := w.remoteDesktopEntry()
	if !ok {
		return
	}
	ui.Column(c.Key("ssh-rd-section")).Gap(2).Shrink(0).Children(func() {
		ui.Row(c).Height(24).AlignItems(ui.Center).Children(func() {
			ui.Text(c, w.tr("其他设备", "Other devices")).FontSize(11).Bold().TextColor(p.muted)
		})
		row := ui.Row(c.Key("ssh-rd-row")).Padding(8, 10).Gap(9).Radius(10).AlignItems(ui.Center)
		if row.Hovered() {
			row.Background(p.soft)
		}
		row.Children(func() {
			ui.Box(c).Size(26, 26).Radius(7).Background(p.lineSoft).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				ui.Icon(c, remoteIcon).FontSize(13).TextColor(p.muted)
			})
			ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
				title := nonEmptyString(page.Title, w.tr("远程桌面", "Remote desktop"))
				if w.rd.connected {
					title += w.tr(" · 已连接", " · connected")
				}
				ui.Text(c, title).FontSize(12).TextColor(p.ink).SingleLine().Ellipsis("…")
				ui.Text(c, w.tr("观看并操作另一台电脑的屏幕", "Watch and control another computer's screen")).FontSize(10).TextColor(p.faint).SingleLine().Ellipsis("…")
			})
			open := quietButton(c, p, "ssh-open-rd", w.tr("打开", "Open")).Label(w.tr("打开远程桌面", "Open remote desktop")).Height(26).Padding(0, 10)
			open.Border(1, p.line).Background(p.raised).TextColor(p.ink)
			if open.Clicked() {
				w.features.panel = ""
				w.openPluginPage(plugin, page)
			}
		})
	})
}

// sshAdvanced is where a machine that needs an address rather than a name is
// reached: a Vantry already running there, and the machines saved for it.
func (w *workbench) sshAdvanced(c *ui.Context, p palette) {
	ui.Column(c.Key("ssh-advanced")).Gap(8).Padding(9, 10).Radius(10).Border(1, p.lineSoft).Shrink(0).Children(func() {
		head := ui.Button(c.Key("ssh-advanced-toggle"), "").Label(w.tr("其他连接方式", "Other ways to connect")).Expanded(w.ssh.advancedOpen).
			FillWidth().Height(26).Padding(0, 1).Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
		head.Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).FillWidth().Children(func() {
				icon := rightIcon
				if w.ssh.advancedOpen {
					icon = chevronIcon
				}
				ui.Icon(c, icon).FontSize(10).TextColor(p.faint).Shrink(0)
				ui.Text(c, w.tr("其他连接方式", "Other ways to connect")).FontSize(11).Bold().TextColor(p.muted).Shrink(0)
				ui.Text(c, w.tr("无需 SSH 配置，直接连接已运行的 Vantry", "Reach a running Vantry without SSH configuration")).FontSize(10).TextColor(p.faint).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			})
		})
		if head.Clicked() {
			w.ssh.advancedOpen = !w.ssh.advancedOpen
		}
		if !w.ssh.advancedOpen {
			return
		}
		ui.Field(c, w.tr("机器", "Machine"), func() {
			ui.TextInput(c.Key("advanced-host"), &w.features.remoteHost).Placeholder(w.tr("SSH 配置里的名字，或 user@host", "An SSH name, or user@host")).FillWidth()
		}).Description(w.tr("这台机器上已经开着的 Vantry。", "The Vantry running on that machine."))
		ui.Field(c, w.tr("远端 Vantry 地址", "Remote Vantry address"), func() {
			ui.TextInput(c.Key("advanced-socket"), &w.features.remoteSocket).Placeholder("/Users/me/.config/Vantry/core.sock").FillWidth()
		}).Description(w.tr("连接时通常能自动找到；找不到时在这里填写。", "Usually found when connecting; enter it when it is not."))
		ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
			if ui.PrimaryButton(c.Key("advanced-connect"), w.tr("在新窗口连接", "Connect in a new window")).Height(27).Padding(0, 12).
				Disabled(w.features.busy["remote"]).Clicked() {
				w.connectRemote()
			}
			ui.TextInput(c.Key("advanced-alias"), &w.remote.alias).Label(w.tr("收藏名", "Name")).Placeholder(w.tr("可选", "Optional")).Width(110).Height(27)
			save := quietButton(c, p, "advanced-save", w.tr("收藏", "Save")).Label(w.tr("收藏这台机器", "Save this machine")).Height(27).Padding(0, 10)
			save.Border(1, p.line).Background(p.raised).TextColor(p.ink)
			if save.Clicked() {
				w.saveRemoteFavorite()
			}
		})
		ui.Text(c, w.tr("远端需要已经启动 Vantry；窗口断开后任务仍在远端继续。收藏只保存名字、机器与地址，不保存任何凭据。", "Vantry must be running there; tasks keep going if the window closes. Saved machines keep a name, the machine and its address, never credentials.")).
			FontSize(10).TextColor(p.faint).Wrap()
		w.savedMachines(c, p)
	})
}

// savedMachines lists the machines saved for a direct connection.
func (w *workbench) savedMachines(c *ui.Context, p palette) {
	if len(w.remote.saved) == 0 {
		return
	}
	ui.Column(c.Key("saved-machines")).Gap(2).Children(func() {
		ui.Text(c, w.tr("已收藏", "Saved")).FontSize(10).Bold().TextColor(p.faint)
		for i, favorite := range w.remote.saved {
			i, favorite := i, favorite
			ui.Row(c.Key("saved-machine:"+favorite.Host)).Padding(4, 6).Gap(6).Radius(7).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, starIcon).FontSize(11).TextColor(p.faint).Shrink(0)
				ui.Column(c).Gap(1).Grow(1).MinWidth(0).Children(func() {
					ui.Text(c, favorite.title()).FontSize(11).TextColor(p.ink).SingleLine().Ellipsis("…")
					ui.Text(c, favorite.Host).Font("Menlo").FontSize(9.5).TextColor(p.faint).SingleLine().Ellipsis("…")
				})
				if quietButton(c, p, "saved-connect", w.tr("连接", "Connect")).Label(w.tr("连接 ", "Connect ")+favorite.title()).Height(22).Padding(0, 8).FontSize(10.5).Clicked() {
					w.useRemoteFavorite(favorite)
					w.connectRemote()
				}
				if iconButton(c, p, "saved-remove", w.tr("移除收藏 ", "Remove saved ")+favorite.title(), trashIcon).Size(20, 20).Padding(4).Clicked() {
					w.removeRemoteFavorite(i)
				}
			})
		}
	})
}

// openSSHHostForm opens the form for a new machine, or for the one being
// edited, filled with what is known about it.
func (w *workbench) openSSHHostForm(key string) {
	form := sshHostForm{open: true, port: "22"}
	if host, ok := w.manualHost(key); ok {
		form.editing = key
		form.name, form.host, form.user, form.key = host.Name, host.Host, host.User, host.Identity
		form.port = strconv.Itoa(hostPort(host.Port))
	}
	w.ssh.add = form
	w.ssh.selected = ""
}

// manualHost finds a machine added here by its key.
func (w *workbench) manualHost(key string) (manualSSHHost, bool) {
	for _, host := range w.ssh.manual {
		if hostKey(host.target(), host.Port) == key {
			return host, true
		}
	}
	return manualSSHHost{}, false
}

// sshHostFormView is the add-or-edit form: where the machine is, who to be
// there, which key to offer, and a test before it is saved.
func (w *workbench) sshHostFormView(c *ui.Context, p palette) {
	form := &w.ssh.add
	editing := form.editing != ""
	ui.Row(c.Key("ssh-form-head")).Gap(8).AlignItems(ui.Center).Shrink(0).Children(func() {
		back := iconButton(c, p, "ssh-form-back", w.tr("返回主机列表", "Back to hosts"), rightIcon).Size(24, 24).Padding(5)
		back.Rotate(180)
		if back.Clicked() {
			form.open = false
		}
		title := w.tr("添加主机", "Add a host")
		if editing {
			title = w.tr("编辑主机", "Edit host")
		}
		ui.Text(c, title).FontSize(12.5).Bold().TextColor(p.ink).Grow(1)
		ui.Text(c, w.tr("仅保存在这台电脑上", "Kept on this Mac only")).FontSize(10).TextColor(p.faint)
	})
	ui.Scroll(c.Key("ssh-form-scroll")).Grow(1).MinHeight(0).Gap(10).Children(func() {
		ui.Column(c.Key("ssh-form")).Gap(10).Children(func() {
			ui.Field(c, w.tr("别名", "Name"), func() {
				ui.TextInput(c.Key("ssh-form-name"), &form.name).Placeholder(w.tr("例如 构建机", "For example build-box")).FillWidth().Height(30).Padding(0, 10)
			}).Description(w.tr("列表与终端标签里显示的名字；可留空。", "Shown in the list and its terminal tab; optional."))
			ui.Field(c, w.tr("主机", "Host"), func() {
				ui.TextInput(c.Key("ssh-form-host"), &form.host).Placeholder("10.0.0.12 / build.example.com").AutoFocus().FillWidth().Height(30).Padding(0, 10)
			}).Description(w.tr("地址或域名。", "An address or a host name.")).Error(form.errors.host)
			ui.Row(c).Gap(8).Children(func() {
				ui.Field(c, w.tr("用户", "User"), func() {
					ui.TextInput(c.Key("ssh-form-user"), &form.user).Placeholder(localUser()).Grow(1).Height(30).Padding(0, 10)
				}).Error(form.errors.user).Grow(1).MinWidth(0)
				ui.Field(c, w.tr("端口", "Port"), func() {
					ui.TextInput(c.Key("ssh-form-port"), &form.port).Placeholder("22").Width(74).Height(30).Padding(0, 10)
				}).Error(form.errors.port).Shrink(0)
			})
			ui.Field(c, w.tr("密钥文件", "Key file"), func() {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.TextInput(c.Key("ssh-form-key"), &form.key).Placeholder("~/.ssh/id_ed25519").Grow(1).MinWidth(0).Height(30).Padding(0, 10)
					if runtime.GOOS == "darwin" {
						if quietButton(c, p, "ssh-form-pick", w.tr("选择…", "Choose…")).Label(w.tr("选择密钥文件", "Choose a key file")).Height(30).Padding(0, 10).Clicked() {
							w.pickSSHKey()
						}
					}
				})
			}).Description(w.tr("留空则使用 ssh-agent 或默认密钥。", "Leave it empty to use ssh-agent or your default keys.")).Error(form.errors.key)
		})
		if form.testing {
			ui.Row(c.Key("ssh-form-testing")).Gap(8).AlignItems(ui.Center).Children(func() {
				activityIndicator(c, p.accent)
				ui.Text(c, w.tr("正在连接 ", "Connecting to ")+nonEmptyString(form.host, "…")+"…").FontSize(11).TextColor(p.muted)
			})
		} else if form.tested {
			ui.Column(c.Key("ssh-form-result")).Gap(3).Padding(8, 10).Radius(8).Background(func() ui.Color {
				if form.result.reachable {
					return p.success.Alpha(0.1)
				}
				return p.danger.Alpha(0.1)
			}()).Children(func() {
				if form.result.reachable {
					ui.Text(c, w.tr("连上了", "Connected")).FontSize(11.5).FontWeight(600).TextColor(p.success)
					w.sshProbeFacts(c, p, form.result)
				} else {
					ui.Text(c, w.tr("连不上", "Could not connect")).FontSize(11.5).FontWeight(600).TextColor(p.danger)
					if form.result.message != "" {
						ui.Text(c, form.result.message).FontSize(10.5).TextColor(p.muted).Wrap()
					}
				}
			})
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
			save := ui.PrimaryButton(c.Key("ssh-form-save"), w.tr("保存", "Save"))
			if editing {
				save = ui.PrimaryButton(c.Key("ssh-form-save"), w.tr("保存修改", "Save changes"))
			}
			save.Height(28).Padding(0, 12)
			if save.Clicked() {
				w.saveSSHHostForm()
			}
			test := quietButton(c, p, "ssh-form-test", w.tr("测试连接", "Test connection")).Height(28).Padding(0, 10)
			test.Border(1, p.line).Background(p.raised).TextColor(p.ink)
			if test.Clicked() && !form.testing {
				w.testSSHHostForm()
			}
			ui.Spacer(c)
			if quietButton(c, p, "ssh-form-cancel", w.tr("取消", "Cancel")).Height(28).Padding(0, 8).Clicked() {
				form.open = false
			}
		})
	})
}

// testSSHHostForm connects to the machine of the form without saving it.
func (w *workbench) testSSHHostForm() {
	form := &w.ssh.add
	host, port, ok := w.validateHostForm()
	if !ok {
		return
	}
	form.testing, form.tested = true, false
	request := api.WorkbenchRequest{
		Action: actionSSHProbe, Name: host,
		Input: manualSSHHost{Host: host, User: strings.TrimSpace(form.user), Port: port, Identity: strings.TrimSpace(form.key)}.input(),
	}
	w.workbenchCall("ssh-form-test", request, func(response api.WorkbenchResponse, err error) {
		form.testing = false
		if err != nil {
			form.tested = true
			form.result = hostProbe{tested: true, message: describe(w.tr("测试失败", "The test failed"), err)}
			return
		}
		form.tested = true
		for _, info := range response.SSHHosts {
			form.result = probeFromHost(info)
		}
	})
}

// validateHostForm checks the form, marking the fields that need attention.
func (w *workbench) validateHostForm() (string, int, bool) {
	form := &w.ssh.add
	form.errors = sshHostFormErrors{}
	host := strings.TrimSpace(form.host)
	switch {
	case host == "":
		form.errors.host = w.tr("填写主机地址或域名。", "Enter an address or a host name.")
	case strings.Contains(host, "@"):
		form.errors.host = w.tr("这里只填主机；用户在上面的字段里。", "Host only; the user goes in its own field.")
	case !validSSHTarget(host):
		form.errors.host = w.tr("只能包含字母、数字和 . _ : -", "Letters, digits and . _ : - only.")
	}
	user := strings.TrimSpace(form.user)
	if user != "" && (strings.ContainsAny(user, " @:/\t") || strings.HasPrefix(user, "-")) {
		form.errors.user = w.tr("用户名不能含空格、@ 或 :", "No spaces, @ or : in a user name")
	}
	port := 22
	if raw := strings.TrimSpace(form.port); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			form.errors.port = w.tr("1–65535", "1–65535")
		} else {
			port = n
		}
	}
	if key := strings.TrimSpace(form.key); strings.HasPrefix(key, "-") {
		form.errors.key = w.tr("路径不能以 - 开头", "A path cannot start with -")
	}
	return host, port, form.errors.host == "" && form.errors.user == "" && form.errors.port == "" && form.errors.key == ""
}

// saveSSHHostForm adds the machine, or saves the changes to it.
func (w *workbench) saveSSHHostForm() {
	form := &w.ssh.add
	host, port, ok := w.validateHostForm()
	if !ok {
		return
	}
	entry := manualSSHHost{
		Name: strings.TrimSpace(form.name), Host: host,
		User: strings.TrimSpace(form.user), Port: port, Identity: strings.TrimSpace(form.key),
	}
	if form.editing != "" {
		for i, host := range w.ssh.manual {
			if hostKey(host.target(), host.Port) == form.editing {
				w.ssh.manual[i] = entry
			}
		}
	} else {
		for _, host := range w.ssh.manual {
			if hostKey(host.target(), host.Port) == hostKey(entry.target(), entry.Port) {
				w.setToolFeedback("remote", notice{Text: w.tr("这台主机已经在列表里了。", "This host is already in the list."), Level: noticeWarn})
				return
			}
		}
		w.ssh.manual = append(w.ssh.manual, entry)
	}
	w.saveState()
	form.open = false
	w.ssh.selected = hostKey(entry.target(), entry.Port)
	w.setToolFeedback("remote", notice{Text: w.tr("已保存 ", "Saved ") + entry.title(), Level: noticeInfo})
}

// removeHost removes a machine from this list: one added here is deleted,
// one of the SSH configuration is hidden — its configuration, which is shared
// with every other tool, is left alone.
func (w *workbench) removeHost(e sshEntry) {
	if e.manual >= 0 {
		w.ssh.manual = append(w.ssh.manual[:e.manual:e.manual], w.ssh.manual[e.manual+1:]...)
		delete(w.ssh.probes, e.key)
		if w.ssh.selected == e.key {
			w.ssh.selected = ""
		}
		w.saveState()
		w.setToolFeedback("remote", notice{Text: w.tr("已移除 ", "Removed ") + e.name, Level: noticeInfo})
		return
	}
	w.removeSSHHost(e.key)
}

// pickSSHKey chooses a key file with the system's own panel.
func (w *workbench) pickSSHKey() {
	w.start(func() {
		script := `POSIX path of (choose file with prompt "选择 SSH 私钥" default location (path to home folder))`
		out, err := exec.Command("osascript", "-e", script).Output()
		if err != nil {
			return // the panel was cancelled
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			return
		}
		w.post(func() {
			w.ssh.add.key = strings.Replace(path, w.homeDir(), "~", 1)
		})
	})
}

// homeDir is the home directory of the app's user, for short paths.
func (w *workbench) homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}
