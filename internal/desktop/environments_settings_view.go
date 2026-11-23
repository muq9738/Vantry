package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

type environmentSettingsState struct {
	loaded            bool
	editingPeerID     string
	editName          string
	editRelay         string
	editRelays        string
	editDirect        string
	rePairPeerID      string
	rePairLink        string
	rePairRelay       string
	addExpanded       bool
	addLink           string
	localSettingsOpen bool
	confirmRemoveID   string
	checkingPeerID    string
	feedback          string
	feedbackIsErr     bool
}

func (s *environmentSettingsState) beginEditPeer(peer remoteaccess.PeerInfo) {
	s.editingPeerID = peer.ID
	s.editName = peer.Name
	s.editRelay = peer.Relay
	var extras []string
	for _, r := range peer.Relays {
		trimmed := strings.TrimSpace(r)
		if trimmed != "" && trimmed != peer.Relay {
			extras = append(extras, trimmed)
		}
	}
	s.editRelays = strings.Join(extras, ", ")
	s.editDirect = strings.Join(peer.Direct, ", ")
	s.rePairPeerID = ""
	s.rePairLink = ""
	s.confirmRemoveID = ""
	s.feedback = ""
	s.feedbackIsErr = false
}

func (w *workbench) localWorkbench() *workbench {
	if w.environments != nil && w.environments.root != nil {
		return w.environments.root
	}
	return w
}

func (w *workbench) openEnvironmentSettings(peerID string) {
	root := w.localWorkbench()
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "environments"
	es := &root.widgets.envSettings
	es.feedback = ""
	es.feedbackIsErr = false
	es.confirmRemoveID = ""
	if peerID != "" {
		es.editingPeerID = peerID
		for _, peer := range root.effectiveAccessStatus().Peers {
			if peer.ID == peerID {
				es.beginEditPeer(peer)
				break
			}
		}
	}
	root.loadEnvironmentSettings(false)
}

func (w *workbench) effectiveAccessStatus() remoteaccess.Status {
	root := w.localWorkbench()
	if len(root.access.status.Peers) > 0 || root.access.status.Name != "" || root.access.status.Enabled || root.access.status.Relay != "" {
		return root.access.status
	}
	if root.environments != nil {
		return root.environments.status
	}
	return root.access.status
}

func (w *workbench) loadEnvironmentSettings(explicit bool) {
	root := w.localWorkbench()
	es := &root.widgets.envSettings
	if _, ok := root.service.(api.WorkbenchService); !ok {
		es.loaded = true
		return
	}
	action := "access.status"
	if explicit {
		action = "access.discover"
		es.feedback = ""
		es.feedbackIsErr = false
	}
	root.workbenchCall("env-settings", api.WorkbenchRequest{Action: action}, func(response api.WorkbenchResponse, err error) {
		es.loaded = true
		if err != nil {
			if explicit {
				es.feedback = explainAccessErr(err)
				es.feedbackIsErr = true
			}
			return
		}
		if strings.TrimSpace(response.Output) == "" {
			return
		}
		var status remoteaccess.Status
		if json.Unmarshal([]byte(response.Output), &status) != nil {
			if explicit {
				es.feedback = root.tr("未能读取环境状态，请刷新重试。", "Failed to read environment status.")
				es.feedbackIsErr = true
			}
			return
		}
		root.applyEnvironmentStatus(status)
		if es.editingPeerID != "" && es.editName == "" {
			for _, peer := range status.Peers {
				if peer.ID == es.editingPeerID {
					es.beginEditPeer(peer)
					break
				}
			}
		}
	})
}

func (w *workbench) applyEnvironmentStatus(status remoteaccess.Status) {
	root := w.localWorkbench()
	root.access.status = status
	if root.access.name == "" {
		root.access.name = nonEmptyString(status.Name, machineName())
	}
	if root.access.relay == "" {
		root.access.relay = status.Relay
	}
	if root.access.publicURL == "" {
		root.access.publicURL = status.PublicURL
	}
	if root.environments != nil {
		root.environments.syncStatus(status)
	}
}

func explainAccessErr(err error) string {
	var typed *api.Error
	if errors.As(err, &typed) && strings.TrimSpace(typed.Message) != "" {
		return typed.Message
	}
	return explain(err)
}

func (w *workbench) envAccessCall(request api.WorkbenchRequest, successFeedback string, onDone func(remoteaccess.Status)) {
	root := w.localWorkbench()
	es := &root.widgets.envSettings
	es.feedback = ""
	es.feedbackIsErr = false
	root.workbenchCall("env-settings", request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			es.feedback = explainAccessErr(err)
			es.feedbackIsErr = true
			return
		}
		var status remoteaccess.Status
		if strings.TrimSpace(response.Output) != "" {
			if json.Unmarshal([]byte(response.Output), &status) != nil {
				es.feedback = root.tr("环境操作未返回有效状态。", "Invalid environment status response.")
				es.feedbackIsErr = true
				return
			}
			root.applyEnvironmentStatus(status)
		}
		if successFeedback != "" {
			es.feedback = successFeedback
			es.feedbackIsErr = false
		}
		if onDone != nil {
			onDone(status)
		}
	})
}

func parseEndpointList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		trimmed := strings.TrimSpace(f)
		if trimmed != "" && !seen[trimmed] {
			seen[trimmed] = true
			out = append(out, trimmed)
		}
	}
	return out
}

func (w *workbench) settingsEnvironments(c *ui.Context, p palette) {
	root := w.localWorkbench()
	es := &root.widgets.envSettings
	if !es.loaded {
		es.loaded = true
		if len(root.effectiveAccessStatus().Peers) == 0 {
			root.loadEnvironmentSettings(false)
		}
	}
	status := root.effectiveAccessStatus()
	hub := root.environments
	busy := root.features.busy["env-settings"]

	ui.Column(c).Gap(20).Children(func() {
		// Section 1: Paired remote environments (connected OR disconnected)
		ui.Column(c).Gap(10).Children(func() {
			settingsHeading(
				c, p,
				w.tr3("远程环境", "Remote environments", "リモート環境"),
				w.tr3(
					"管理所有已配对机器（含当前已断开或离线机器），支持修改名称、中继与直连地址、重新连接或移除。",
					"Manage every paired machine—even when disconnected or offline. Configure name, relay URLs, direct endpoints, reconnect, or remove.",
					"接続中・切断中を問わずペア済み環境を管理し、リレーや直結アドレスを設定できます。",
				),
				func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						if outlineButton(c, p, "env-refresh", w.tr("刷新", "Refresh")).Disabled(busy).Clicked() {
							root.loadEnvironmentSettings(true)
						}
						addLabel := w.tr("添加环境", "Add environment")
						if es.addExpanded {
							addLabel = w.tr("收起添加", "Close")
						}
						if outlineButton(c, p, "env-add-toggle", addLabel).Clicked() {
							es.addExpanded = !es.addExpanded
						}
					})
				},
			)

			if es.addExpanded {
				w.envAddPeerCard(c, p, root, es, status, busy)
			}

			if len(status.Peers) == 0 {
				settingsGroup(c, p, "settings-group-env-empty", func() {
					settingsRow(
						c, p, "settings-row-env-empty", true,
						w.tr("暂无已配对的远程环境", "No paired remote environments"),
						w.tr("点击右上角「添加环境」粘贴配对链接或输入 SESSION 配对码，或从下方局域网设备直接配对。", "Click Add environment above to paste a vantry:// pairing link or session code, or pair with a nearby LAN machine below."),
						func() {},
					)
				})
			} else {
				for idx, peer := range status.Peers {
					peer := peer
					w.envPeerCard(c, p, root, hub, es, peer, idx, busy)
				}
			}

			if es.feedback != "" {
				color := p.success
				if es.feedbackIsErr {
					color = p.danger
				}
				ui.Text(c, es.feedback).FontSize(11.5).TextColor(color).Wrap()
			}
		})

		// Section 2: Unpaired nearby LAN machines
		w.envNearbySection(c, p, root, es, status, busy)

		// Section 3: Local host sharing & paired clients
		w.envLocalHostSection(c, p, root, es, status, busy)

		// Section 4: SSH workbench launcher
		ui.Column(c).Gap(10).Children(func() {
			settingsHeading(c, p, w.tr("SSH 远程工作台", "SSH remote workbench"), "", nil)
			settingsGroup(c, p, "settings-group-remote", func() {
				settingsRow(
					c, p, "settings-row-remote", true,
					w.tr("通过 SSH 连接其他机器", "Connect over SSH"),
					w.tr("打开 SSH 远程面板，通过 SSH 隧道接入远端 Vantry 工作台。", "Open the SSH remote dock to tunnel into another machine's Vantry workbench."),
					func() {
						if outlineButton(c, p, "settings-remote", w.tr("管理 SSH 远程环境", "Manage SSH environments")).Clicked() {
							w.widgets.settingsOpen = false
							w.features.panel = ""
							w.togglePanel("remote")
						}
					},
				)
			})
		})
	})
}

func (w *workbench) envAddPeerCard(c *ui.Context, p palette, root *workbench, es *environmentSettingsState, status remoteaccess.Status, busy bool) {
	settingsGroup(c, p, "settings-group-env-add", func() {
		ui.Column(c).FillWidth().Padding(14, 16).Gap(10).Children(func() {
			ui.Text(c, w.tr("添加或配对远程环境", "Add or pair remote environment")).FontSize(12.5).FontWeight(600).TextColor(p.ink)
			ui.Text(c, w.tr("粘贴目标电脑生成的 vantry://connect… 链接、SESSION-… 配对码或 T3 地址。短码使用本机配置的中继兑换。", "Paste a vantry://connect… link, SESSION-… code, or T3 URL from the target machine.")).FontSize(11.5).TextColor(p.muted).Wrap()
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				input := ui.TextInput(c.Key("env-add-link"), &es.addLink).Label(w.tr("配对链接或会话配对码", "Pairing link or session code")).Placeholder("vantry://connect… / SESSION-… / https://…").Password().Grow(1).MinWidth(0)
				btn := primaryAction(c, p, "env-add-submit", w.tr("确认配对", "Pair")).Disabled(busy || strings.TrimSpace(es.addLink) == "")
				if btn.Clicked() || (input.Submitted() && strings.TrimSpace(es.addLink) != "") {
					link := strings.TrimSpace(es.addLink)
					w.envAccessCall(
						api.WorkbenchRequest{Action: "access.pair", Name: nonEmptyString(root.access.relay, status.Relay), Message: link},
						w.tr("已保存远程环境配对。", "Remote environment paired."),
						func(remoteaccess.Status) {
							es.addLink = ""
							es.addExpanded = false
							if root.environments != nil {
								root.environments.ensureViews()
							}
						},
					)
				}
			})
		})
	})
}

func (w *workbench) envPeerCard(c *ui.Context, p palette, root *workbench, hub *environmentHub, es *environmentSettingsState, peer remoteaccess.PeerInfo, _ int, busy bool) {
	var record *environmentRecord
	if hub != nil {
		record = hub.recordByConnection(peer.ID)
	}

	isConnected := record != nil && record.view != nil
	isConnecting := record != nil && record.connecting
	connNoticeOffline := isConnected && record.view.connNotice.valid() && record.view.connNotice.Level != noticeInfo

	badgeText := w.tr("已断开", "Disconnected")
	badgeTone := p.warning
	switch {
	case isConnecting:
		badgeText, badgeTone = w.tr("连接中…", "Connecting…"), p.running
	case isConnected && !connNoticeOffline:
		modeLabel := ""
		if record.view.defaults.ConnectionMode != nil {
			switch record.view.defaults.ConnectionMode() {
			case "direct":
				modeLabel = w.tr(" · 直连", " · Direct")
			case "relay":
				modeLabel = w.tr(" · 中继", " · Relay")
			}
		}
		badgeText, badgeTone = w.tr("已连接", "Connected")+modeLabel, p.success
	case connNoticeOffline:
		badgeText, badgeTone = w.tr("连接已中断", "Offline"), p.warning
	case peer.Reachable != nil && *peer.Reachable:
		if peer.Mode == "direct" {
			badgeText = w.tr("直连可达", "Direct ready")
		} else {
			badgeText = w.tr("中继可达", "Relay ready")
		}
		if peer.LatencyMs > 0 {
			badgeText += fmt.Sprintf(" (%dms)", peer.LatencyMs)
		}
		badgeTone = p.success
	case peer.Reachable != nil && !*peer.Reachable:
		badgeText, badgeTone = w.tr("已断开 · 暂不可达", "Disconnected · Unreachable"), p.warning
	case peer.Nearby:
		badgeText, badgeTone = w.tr("已断开 · 局域网在线", "Disconnected · On LAN"), p.muted
	}

	var metaParts []string
	switch {
	case peer.Relay != "":
		metaParts = append(metaParts, w.tr("中继：", "Relay: ")+peer.Relay)
	case len(peer.Relays) > 0:
		metaParts = append(metaParts, w.tr("中继：", "Relay: ")+peer.Relays[0])
	default:
		metaParts = append(metaParts, w.tr("仅局域网直连", "LAN direct only"))
	}
	if len(peer.Relays) > 1 {
		metaParts = append(metaParts, fmt.Sprintf(w.tr("+%d 备用中继", "+%d fallback relay"), len(peer.Relays)-1))
	}
	if len(peer.Direct) > 0 {
		directText := w.tr("直连：", "Direct: ") + peer.Direct[0]
		if len(peer.Direct) > 1 {
			directText += fmt.Sprintf(w.tr(" 等 %d 个", " +%d more"), len(peer.Direct)-1)
		}
		metaParts = append(metaParts, directText)
	}
	metaLine := strings.Join(metaParts, " · ")

	errorDetail := strings.TrimSpace(peer.LastError)
	if errorDetail == "" && record != nil {
		errorDetail = strings.TrimSpace(record.failed)
	}

	editing := es.editingPeerID == peer.ID

	settingsGroup(c, p, "settings-env-peer:"+peer.ID, func() {
		ui.Column(c).FillWidth().Children(func() {
			ui.Row(c).FillWidth().MinHeight(64).Padding(12, 14).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Row(c).Size(32, 32).Radius(8).Justify(ui.Center).AlignItems(ui.Center).Background(p.soft).Border(1, p.lineSoft).Shrink(0).Children(func() {
					ui.Icon(c, serverIcon).FontSize(15).TextColor(p.muted)
				})
				ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
						ui.Text(c, peer.Name).FontSize(13).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…")
						kitBadge(c, badgeText, badgeTone)
					})
					ui.Text(c, metaLine).FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…").Tooltip(metaLine)
					if errorDetail != "" && !isConnected {
						ui.Text(c, errorDetail).FontSize(11).TextColor(p.warning).Wrap()
					}
				})
				ui.Row(c).Gap(6).AlignItems(ui.Center).Shrink(0).Wrap().Children(func() {
					if isConnected && record != nil && record.view != nil {
						if outlineButton(c, p, "env-open:"+peer.ID, w.tr("打开", "Open")).Clicked() {
							for i := range hub.records {
								if hub.records[i].connectionID == peer.ID {
									hub.choose(w, i, false)
									break
								}
							}
							w.widgets.settingsOpen = false
						}
						if outlineButton(c, p, "env-disconnect:"+peer.ID, w.tr("断开", "Disconnect")).Clicked() {
							hub.disconnectRecord(peer.ID)
						}
					} else {
						connBtn := primaryAction(c, p, "env-connect:"+peer.ID, w.tr("连接", "Connect"))
						connBtn.Label(w.tr("连接环境 ", "Connect environment ") + peer.Name)
						connBtn.Disabled(busy || isConnecting)
						if connBtn.Clicked() {
							if hub != nil && hub.root.defaults.ConnectEnvironment != nil {
								if record == nil {
									hub.syncStatus(root.effectiveAccessStatus())
									record = hub.recordByConnection(peer.ID)
								}
								if record != nil {
									hub.connectRecord(record, nil, false)
								}
							} else {
								root.connectDevice(peer, "auto")
							}
						}
					}

					checkBtn := outlineButton(c, p, "env-check:"+peer.ID, w.tr("检测", "Check"))
					checkBtn.Disabled(busy || es.checkingPeerID == peer.ID)
					if checkBtn.Clicked() {
						es.checkingPeerID = peer.ID
						w.envAccessCall(api.WorkbenchRequest{Action: "access.check", Name: peer.ID, Message: "auto"}, "", func(remoteaccess.Status) {
							es.checkingPeerID = ""
						})
					}

					editLabel := w.tr("设置", "Settings")
					if editing {
						editLabel = w.tr("收起", "Hide")
					}
					editBtn := outlineButton(c, p, "env-edit:"+peer.ID, editLabel)
					editBtn.Label(editLabel + " " + peer.Name)
					if editBtn.Clicked() {
						if editing {
							es.editingPeerID = ""
						} else {
							es.beginEditPeer(peer)
						}
					}

					if es.confirmRemoveID == peer.ID {
						confirmBtn := outlineButton(c, p, "env-remove-confirm:"+peer.ID, w.tr("确认移除", "Confirm remove")).TextColor(p.danger)
						if confirmBtn.Clicked() {
							es.confirmRemoveID = ""
							if es.editingPeerID == peer.ID {
								es.editingPeerID = ""
							}
							w.envAccessCall(api.WorkbenchRequest{Action: "access.remove", Name: peer.ID}, w.tr("已移除环境 ", "Removed environment ")+peer.Name, nil)
						}
					} else {
						rmBtn := quietButton(c, p, "env-remove:"+peer.ID, w.tr("移除", "Remove")).Height(28).TextColor(p.danger)
						if rmBtn.Clicked() {
							es.confirmRemoveID = peer.ID
						}
					}
				})
			})

			if editing {
				w.envPeerEditor(c, p, root, hub, es, peer, busy)
			}
		})
	})
}

func (w *workbench) envPeerEditor(c *ui.Context, p palette, root *workbench, hub *environmentHub, es *environmentSettingsState, peer remoteaccess.PeerInfo, busy bool) {
	ui.Column(c.Key("env-editor:"+peer.ID)).FillWidth().Padding(14, 16).Gap(10).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.soft).Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, w.tr("环境连接设置", "Environment settings")+" · "+peer.Name).FontSize(12.5).FontWeight(600).TextColor(p.ink).Grow(1)
			if peer.DeviceID != "" {
				ui.Text(c, "ID: "+peer.DeviceID).Font("Menlo").FontSize(10).TextColor(p.faint).Selectable()
			}
		})

		ui.Field(c, w.tr("环境名称", "Environment name"), func() {
			ui.TextInput(c.Key("env-edit-name:"+peer.ID), &es.editName).Label(w.tr("环境名称", "Environment name")).Placeholder(w.tr("例如 Mac mini / Dev Laptop", "e.g. Mac mini")).FillWidth()
		})
		ui.Field(c, w.tr("主中继地址", "Primary relay URL"), func() {
			ui.TextInput(c.Key("env-edit-relay:"+peer.ID), &es.editRelay).Label(w.tr("主中继地址", "Primary relay URL")).Placeholder("https://relay.example.com/vantry-relay").FillWidth()
		}).Description(w.tr("跨网络时优先尝试该中继地址；留空表示仅使用局域网直连或备用中继。", "Used across networks; leave blank for direct/fallback only."))

		ui.Field(c, w.tr("备用中继地址（逗号分隔，可选）", "Fallback relay URLs (comma-separated)"), func() {
			ui.TextInput(c.Key("env-edit-relays:"+peer.ID), &es.editRelays).Label(w.tr("备用中继地址", "Fallback relay URLs")).Placeholder("https://backup-relay.example.com:9443").FillWidth()
		})

		ui.Field(c, w.tr("直连地址 host:port（逗号分隔，可选）", "Direct LAN addresses host:port (comma-separated)"), func() {
			ui.TextInput(c.Key("env-edit-direct:"+peer.ID), &es.editDirect).Label(w.tr("直连地址", "Direct addresses")).Placeholder("192.168.1.20:52041").FillWidth()
		}).Description(w.tr("同一局域网下优先直连；主机 IP 变动后可在此直接更新，无需重新配对。", "Direct LAN addresses tried before relay; update here if host IP changed without re-pairing."))

		submitUpdate := func(reconnect bool) {
			update := remoteaccess.PeerUpdate{
				ID:     peer.ID,
				Name:   strings.TrimSpace(es.editName),
				Relay:  strings.TrimSpace(es.editRelay),
				Relays: parseEndpointList(es.editRelays),
				Direct: parseEndpointList(es.editDirect),
			}
			raw, _ := json.Marshal(update)
			msg := w.tr("已保存环境 ", "Saved settings for ") + update.Name
			w.envAccessCall(api.WorkbenchRequest{Action: "access.peer.update", Name: peer.ID, Input: raw}, msg, func(status remoteaccess.Status) {
				for _, updatedPeer := range status.Peers {
					if updatedPeer.ID == peer.ID {
						es.beginEditPeer(updatedPeer)
						break
					}
				}
				es.feedback = msg
				if reconnect && hub != nil && hub.root.defaults.ConnectEnvironment != nil {
					hub.reconnectRecord(peer.ID)
				}
			})
		}

		ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
			if primaryAction(c, p, "env-save:"+peer.ID, w.tr("保存设置", "Save settings")).Disabled(busy).Clicked() {
				submitUpdate(false)
			}
			if outlineButton(c, p, "env-save-reconnect:"+peer.ID, w.tr("保存并重连", "Save & reconnect")).Disabled(busy).Clicked() {
				submitUpdate(true)
			}
			if outlineButton(c, p, "env-test-direct:"+peer.ID, w.tr("直连检测", "Test direct")).Disabled(busy).Clicked() {
				w.envAccessCall(api.WorkbenchRequest{Action: "access.check", Name: peer.ID, Message: "direct"}, "", nil)
			}
			if outlineButton(c, p, "env-test-relay:"+peer.ID, w.tr("中继检测", "Test relay")).Disabled(busy || strings.TrimSpace(es.editRelay) == "").Clicked() {
				w.envAccessCall(api.WorkbenchRequest{Action: "access.check", Name: peer.ID, Message: "relay"}, "", nil)
			}
			if outlineButton(c, p, "env-open-win:"+peer.ID, w.tr("新窗口打开", "Open window")).Disabled(busy).Clicked() {
				root.openDeviceWindow(peer, "auto")
			}
			rePairOpen := es.rePairPeerID == peer.ID
			label := w.tr("更新配对凭证", "Update pairing")
			if rePairOpen {
				label = w.tr("取消更新配对", "Cancel re-pair")
			}
			if quietButton(c, p, "env-repair-toggle:"+peer.ID, label).Height(28).Clicked() {
				if rePairOpen {
					es.rePairPeerID = ""
				} else {
					es.rePairPeerID = peer.ID
					es.rePairLink = ""
					es.rePairRelay = peer.Relay
				}
			}
		})

		if es.rePairPeerID == peer.ID {
			ui.Column(c).FillWidth().Gap(8).Padding(10, 12).Radius(8).Background(p.raised).Border(1, p.lineSoft).Children(func() {
				ui.Text(c, w.tr("如果目标主机重置了证书或更换了秘钥，粘贴新的 vantry://connect… 链接或 SESSION-… 配对码即可原位更新，不丢失列表条目。", "If the target machine rotated its certificate or secret, paste its new vantry://connect… link or SESSION-… code to update credentials in place.")).FontSize(11).TextColor(p.muted).Wrap()
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.TextInput(c.Key("env-repair-link:"+peer.ID), &es.rePairLink).Placeholder("vantry://connect… / SESSION-…").Password().Grow(1).MinWidth(0)
					if primaryAction(c, p, "env-repair-submit:"+peer.ID, w.tr("确认更新凭证", "Apply pairing")).Disabled(busy || strings.TrimSpace(es.rePairLink) == "").Clicked() {
						relayHint := nonEmptyString(strings.TrimSpace(es.rePairRelay), strings.TrimSpace(es.editRelay))
						w.envAccessCall(
							api.WorkbenchRequest{Action: "access.pair", Name: relayHint, Message: strings.TrimSpace(es.rePairLink)},
							w.tr("已更新配对凭证。", "Updated pairing credentials."),
							func(status remoteaccess.Status) {
								es.rePairPeerID = ""
								es.rePairLink = ""
								for _, updatedPeer := range status.Peers {
									if updatedPeer.ID == peer.ID || updatedPeer.DeviceID == peer.DeviceID {
										es.beginEditPeer(updatedPeer)
										break
									}
								}
							},
						)
					}
				})
			})
		}
	})
}

func (w *workbench) envNearbySection(c *ui.Context, p palette, root *workbench, es *environmentSettingsState, status remoteaccess.Status, _ bool) {
	paired := map[string]bool{}
	for _, peer := range status.Peers {
		if peer.DeviceID != "" {
			paired[peer.DeviceID] = true
		}
	}
	var nearby []remoteaccess.NearbyDevice
	for _, dev := range status.Nearby {
		if !paired[dev.ID] {
			nearby = append(nearby, dev)
		}
	}
	if len(nearby) == 0 && status.DiscoveryError == "" {
		return
	}
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(c, p, w.tr("局域网发现", "Nearby on LAN"), "", nil)
		settingsGroup(c, p, "settings-group-env-nearby", func() {
			ui.Column(c).FillWidth().Children(func() {
				for i, dev := range nearby {
					dev := dev
					desc := w.tr("局域网内正在运行 Vantry，尚未配对", "Running Vantry on LAN · not yet paired")
					if len(dev.Addresses) > 0 {
						desc = strings.Join(dev.Addresses, ", ")
					}
					settingsRow(c, p, "env-nearby:"+dev.ID, i == 0, dev.Name, desc, func() {
						if outlineButton(c, p, "env-nearby-pair:"+dev.ID, w.tr("输入配对码", "Enter code")).Clicked() {
							es.addExpanded = true
							es.addLink = ""
							root.access.pairName = dev.Name
						}
					})
				}
				if status.DiscoveryError != "" {
					ui.Row(c).FillWidth().Padding(10, 14).Children(func() {
						ui.Text(c, status.DiscoveryError).FontSize(11).TextColor(p.muted).Wrap()
					})
				}
			})
		})
	})
}

func (w *workbench) envLocalHostSection(c *ui.Context, p palette, root *workbench, es *environmentSettingsState, status remoteaccess.Status, busy bool) {
	ui.Column(c).Gap(10).Children(func() {
		settingsHeading(
			c, p,
			w.tr("本机共享与受控设置", "This machine"),
			w.tr("允许 Vantry 电脑或手机访问本机。手机配对后可使用会话与已开启的桌面共享。", "Connect Vantry computers or phones. Paired phones can use conversations and enabled desktop sharing."),
			func() {
				lbl := w.tr("本机中继设置", "Host relay settings")
				if es.localSettingsOpen {
					lbl = w.tr("收起本机设置", "Hide settings")
				}
				if outlineButton(c, p, "env-local-settings-toggle", lbl).Clicked() {
					es.localSettingsOpen = !es.localSettingsOpen
				}
			},
		)
		settingsGroup(c, p, "settings-group-env-local", func() {
			ui.Column(c).FillWidth().Padding(10, 14).Gap(8).Children(func() {
				root.localDeviceRow(c, p)
				root.sessionPairing(c, p)
				if es.localSettingsOpen {
					root.remoteAccessSettings(c, p)
				}
				if len(status.Clients) > 0 {
					ui.Box(c).FillWidth().Height(1).Background(p.lineSoft)
					ui.Text(c, w.tr("已授权连接本机的设备", "Authorized client devices")).FontSize(11.5).FontWeight(600).TextColor(p.muted)
					root.sessionClients(c, p)
				}
			})
		})
		_ = busy
	})
}
