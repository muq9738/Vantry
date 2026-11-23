package desktop

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

type remoteAccessState struct {
	loaded                     bool
	name, relay, key, link     string
	publicURL                  string
	qr                         pairingQRState
	adding, settings, advanced bool
	pairName                   string
	status                     remoteaccess.Status
	feedback                   string
	nextPoll                   time.Time
	probed                     map[string]bool
}

func (w *workbench) loadRemoteAccess() {
	if w.access.loaded {
		return
	}
	w.access.loaded = true
	w.accessCall(api.WorkbenchRequest{Action: "access.discover"})
}
func (w *workbench) accessCall(request api.WorkbenchRequest) { w.accessRequest(request, nil) }
func (w *workbench) accessRequest(request api.WorkbenchRequest, done func(remoteaccess.Status)) {
	if request.Action != "access.status" && request.Action != "access.check" {
		w.access.feedback = ""
	}
	w.workbenchCall("remote-access", request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			var serviceError *api.Error
			if errors.As(err, &serviceError) {
				w.access.feedback = serviceError.Message
			} else {
				w.access.feedback = explain(err)
			}
			return
		}
		var status remoteaccess.Status
		if json.Unmarshal([]byte(response.Output), &status) != nil {
			w.access.feedback = "未能读取设备状态，请刷新重试。"
			return
		}
		w.access.status = status
		if w.access.publicURL == "" {
			w.access.publicURL = status.PublicURL
		}
		if w.access.name == "" {
			w.access.name = nonEmptyString(status.Name, machineName())
		}
		if w.access.relay == "" {
			w.access.relay = status.Relay
		}
		if request.Action == "access.pair" {
			w.access.link = ""
			w.access.adding = false
			w.access.probed = nil
		}
		if done != nil {
			done(status)
		} else {
			w.probeNextDevice()
		}
	})
}
func (w *workbench) probeNextDevice() {
	if w.access.probed == nil {
		w.access.probed = map[string]bool{}
	}
	for _, peer := range w.access.status.Peers {
		if w.access.probed[peer.ID] {
			continue
		}
		w.access.probed[peer.ID] = true
		w.accessRequest(api.WorkbenchRequest{Action: "access.check", Name: peer.ID, Message: "auto"}, nil)
		return
	}
}
func (w *workbench) saveRemoteAccess(action string) {
	input, _ := json.Marshal(map[string]string{"name": w.access.name, "relay": w.access.relay, "key": w.access.key, "publicUrl": w.access.publicURL})
	w.accessCall(api.WorkbenchRequest{Action: action, Input: input})
}
func (w *workbench) connectPairedHost(peer remoteaccess.PeerInfo) { w.connectDevice(peer, "auto") }
func (w *workbench) connectDevice(peer remoteaccess.PeerInfo, mode string) {
	w.access.feedback = "正在连接 " + peer.Name + "…"
	w.accessRequest(api.WorkbenchRequest{Action: "access.check", Name: peer.ID, Message: mode}, func(status remoteaccess.Status) {
		for _, device := range status.Peers {
			if device.ID == peer.ID && device.Reachable != nil && *device.Reachable {
				w.openDeviceWindow(peer, mode)
				return
			}
		}
		w.access.feedback = "暂时无法连接 " + peer.Name + "，请确认设备在线，或重新配对。"
	})
}
func (w *workbench) openDeviceWindow(peer remoteaccess.PeerInfo, mode string) {
	if w.defaults.StatePath == "" {
		w.access.feedback = "请从本机窗口连接设备。"
		return
	}
	executable, err := os.Executable()
	if err != nil {
		w.access.feedback = "无法定位 Vantry 应用"
		return
	}
	dataDir := filepath.Dir(w.defaults.StatePath)
	w.start(func() {
		command := exec.Command(executable, "desktop", "--data-dir", dataDir, "--peer", peer.ID, "--peer-mode", mode)
		stderr := &remoteTail{}
		command.Stderr = stderr
		if err := command.Start(); err != nil {
			w.post(func() { w.access.feedback = "无法打开远程工作台。" })
			return
		}
		w.post(func() { w.access.feedback = "" })
		err := command.Wait()
		w.post(func() {
			if err != nil {
				w.access.feedback = strings.TrimSpace(strings.TrimPrefix(string(stderr.data), "vantry:"))
				if w.access.feedback == "" {
					w.access.feedback = "远程设备暂时无法连接。"
				}
			}
		})
	})
}

func (w *workbench) remoteAccessStage(c *ui.Context, p palette) {
	now := c.Now()
	if w.access.nextPoll.IsZero() {
		w.access.nextPoll = now.Add(3 * time.Second)
	}
	if !now.Before(w.access.nextPoll) {
		w.access.nextPoll = now.Add(3 * time.Second)
		w.accessCall(api.WorkbenchRequest{Action: "access.status"})
	}
	c.After(w.access.nextPoll.Sub(now))
	ui.Column(c.Key("remote-access-stage")).Grow(1).MinHeight(0).Children(func() {
		w.stageHeader(c, p, linkIcon, w.tr("远程访问", "Remote access"), nil)
		ui.Scroll(c.Key("device-scroll")).Grow(1).MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().AlignItems(ui.Center).Padding(30, 28).Children(func() {
				ui.Column(c).FillWidth().MaxWidth(820).Gap(20).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Column(c).Gap(6).Grow(1).MinWidth(0).Children(func() {
							ui.Text(c, w.tr("设备", "Devices")).FontSize(22).Bold().TextColor(p.ink)
							ui.Text(c, w.tr("优先局域网直连，跨网自动使用中继。", "Direct on your network; relay when away.")).FontSize(12).TextColor(p.muted).Wrap()
						})
						if iconButton(c, p, "access-refresh", w.tr("刷新设备", "Refresh devices"), refreshIcon).Size(30, 30).Padding(7).Disabled(w.features.busy["remote-access"]).Clicked() {
							w.access.probed = nil
							w.accessCall(api.WorkbenchRequest{Action: "access.discover"})
						}
						if quietButton(c, p, "add-device", w.tr("添加设备", "Add device")).Label(w.tr("添加配对设备", "Add paired device")).Height(30).Padding(0, 12).Clicked() {
							w.access.adding = !w.access.adding
							w.access.pairName = ""
						}
						if iconButton(c, p, "device-settings", w.tr("远程访问设置", "Remote access settings"), gearIcon).Size(30, 30).Padding(7).Clicked() {
							w.access.settings = !w.access.settings
						}
					})
					w.deviceList(c, p)
					if w.access.adding {
						w.devicePairForm(c, p)
					}
					w.localDeviceRow(c, p)
					w.sessionPairing(c, p)
					w.sessionClients(c, p)
					if w.access.settings {
						w.remoteAccessSettings(c, p)
					}
					if w.access.feedback != "" {
						ui.Text(c, w.access.feedback).FontSize(11).TextColor(p.warning).Wrap()
					}
					if w.access.status.DiscoveryError != "" {
						ui.Text(c, w.access.status.DiscoveryError).FontSize(11).TextColor(p.muted).Wrap()
					}
				})
			})
		})
	})
}

func (w *workbench) deviceList(c *ui.Context, p palette) {
	paired := map[string]bool{}
	ui.Column(c.Key("device-list")).Gap(0).Children(func() {
		for _, peer := range w.access.status.Peers {
			peer := peer
			paired[peer.DeviceID] = true
			label := w.tr("已配对", "Paired")
			color := p.muted
			if peer.Nearby {
				label = w.tr("已在局域网发现", "On your network")
			}
			if peer.Reachable != nil {
				if !*peer.Reachable {
					label = w.tr("暂时无法连接", "Unavailable")
				} else if peer.Mode == "direct" {
					label = w.tr("直连可用", "Direct available")
					color = p.success
				} else {
					label = w.tr("通过中继连接", "Via relay")
					color = p.success
				}
			}
			w.deviceRow(c, p, peer.ID, peer.Name, label, color, func() {
				if ui.PrimaryButton(c.Key("connect"), w.tr("连接", "Connect")).Label(w.tr("连接设备 ", "Connect device ")+peer.Name).Height(30).Padding(0, 16).Disabled(w.features.busy["remote-access"]).Clicked() {
					w.connectDevice(peer, "auto")
				}
				menu := iconButton(c, p, "device-menu", w.tr("设备操作 ", "Device actions ")+peer.Name, moreIcon).Size(28, 28).Padding(6)
				menu.PopoverMenuButton(func(m *ui.Menu) {
					if m.Item(w.tr("仅使用直连", "Connect directly")).Chosen() {
						w.connectDevice(peer, "direct")
					}
					if m.Item(w.tr("通过中继连接", "Connect through relay")).Disabled(peer.Relay == "").Chosen() {
						w.connectDevice(peer, "relay")
					}
					m.Separator()
					if m.Item(w.tr("移除设备", "Remove device")).Chosen() {
						w.accessCall(api.WorkbenchRequest{Action: "access.remove", Name: peer.ID})
					}
				})
			})
		}
		for _, device := range w.access.status.Nearby {
			device := device
			if paired[device.ID] {
				continue
			}
			w.deviceRow(c, p, device.ID, device.Name, w.tr("附近的设备 · 需要配对", "Nearby · Pair to connect"), p.muted, func() {
				if quietButton(c, p, "pair", w.tr("配对", "Pair")).Label(w.tr("配对设备 ", "Pair device ")+device.Name).Height(30).Padding(0, 16).Clicked() {
					w.access.adding = true
					w.access.pairName = device.Name
				}
			})
		}
		if len(w.access.status.Peers) == 0 && len(w.access.status.Nearby) == 0 {
			ui.Column(c.Key("no-devices")).Padding(30, 20).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, remoteIcon).FontSize(28).TextColor(p.faint)
				ui.Text(c, w.tr("正在查找附近的设备", "Looking for nearby devices")).FontSize(13).TextColor(p.muted)
				ui.Text(c, w.tr("在另一台电脑上打开 Vantry，并允许访问本机。", "Open Vantry on the other computer and allow access.")).FontSize(11).TextColor(p.faint).Wrap()
			})
		}
	})
}
func (w *workbench) deviceRow(c *ui.Context, p palette, id, name, label string, color ui.Color, actions func()) {
	row := ui.Row(c.Key("device:"+id)).Padding(18, 12).Gap(14).AlignItems(ui.Center).Radius(10).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft)
	if row.Hovered() {
		row.Background(p.soft)
	}
	row.Children(func() {
		ui.Row(c).Size(40, 40).Radius(10).Background(p.lineSoft).AlignItems(ui.Center).Justify(ui.Center).Children(func() { ui.Icon(c, remoteIcon).FontSize(20).TextColor(p.muted) })
		ui.Column(c).Grow(1).MinWidth(0).Gap(6).Children(func() {
			ui.Text(c, name).FontSize(14).Bold().TextColor(p.ink).SingleLine().Ellipsis("…")
			ui.Text(c, label).FontSize(11).TextColor(color)
		})
		actions()
	})
}
func (w *workbench) devicePairForm(c *ui.Context, p palette) {
	ui.Column(c.Key("pair-device")).Gap(10).Padding(16).Radius(10).Background(p.soft).Border(1, p.lineSoft).Children(func() {
		title := w.tr("添加设备", "Add device")
		if w.access.pairName != "" {
			title = w.tr("与 ", "Pair with ") + w.access.pairName + w.tr(" 配对", "")
		}
		ui.Text(c, title).FontSize(12).Bold().TextColor(p.ink)
		ui.Text(c, w.tr("输入另一台设备的会话配对码，或粘贴配对链接。短码在所选中继兑换。", "Enter the other device’s session code or paste its pairing link. Short codes use the selected relay.")).FontSize(11).TextColor(p.muted).Wrap()
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			input := ui.TextInput(c.Key("pair-link"), &w.access.link).Label(w.tr("会话配对码或链接", "Session code or link")).Placeholder("SESSION-… / vantry://connect…").Password().Grow(1).MinWidth(0).Height(32).Padding(0, 10)
			add := ui.PrimaryButton(c.Key("pair-submit"), w.tr("配对", "Pair")).Label(w.tr("确认配对", "Confirm pairing")).Height(32).Padding(0, 14).Disabled(strings.TrimSpace(w.access.link) == "" || w.features.busy["remote-access"])
			if add.Clicked() || input.Submitted() && w.access.link != "" {
				w.accessCall(api.WorkbenchRequest{Action: "access.pair", Name: w.access.relay, Message: w.access.link})
			}
		})
	})
}
func (w *workbench) localDeviceRow(c *ui.Context, p palette) {
	status := w.access.status
	ui.Row(c.Key("this-device")).Padding(18, 4).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, homeIcon).FontSize(18).TextColor(p.faint)
		ui.Column(c).Grow(1).MinWidth(0).Gap(5).Children(func() {
			ui.Text(c, w.tr("这台电脑", "This computer")+" · "+nonEmptyString(status.Name, machineName())).FontSize(12).TextColor(p.muted).SingleLine().Ellipsis("…")
			label := w.tr("未允许访问", "Access is off")
			if status.Enabled {
				label = w.tr("允许局域网直连", "Direct access enabled")
				if status.RelayConnected {
					label += w.tr(" · 中继在线", " · Relay online")
				}
			}
			ui.Text(c, label).FontSize(10).TextColor(p.faint)
		})
		if status.Enabled {
			if quietButton(c, p, "copy-pair", w.tr("生成电脑配对码", "Generate computer code")).Height(28).FontSize(11).Disabled(w.features.busy["remote-access"]).Clicked() {
				w.accessCall(api.WorkbenchRequest{Action: "access.code"})
			}
		}
		on := status.Enabled
		ui.Switch(c.Key("allow-access"), &on).Label(w.tr("允许访问本机", "Allow access to this computer")).Disabled(w.features.busy["remote-access"])
		if on != status.Enabled {
			if on {
				w.saveRemoteAccess("access.start")
			} else {
				w.accessCall(api.WorkbenchRequest{Action: "access.stop"})
			}
		}
	})
}
func (w *workbench) remoteAccessSettings(c *ui.Context, p palette) {
	ui.Column(c.Key("access-settings")).Gap(12).Padding(16).Radius(10).Background(p.soft).Border(1, p.lineSoft).Children(func() {
		ui.Text(c, w.tr("设备设置", "Device settings")).FontSize(12).Bold().TextColor(p.ink)
		ui.TextInput(c.Key("device-name"), &w.access.name).Label(w.tr("设备名称", "Device name")).Placeholder(w.tr("设备名称", "Device name")).FillWidth()
		ui.TextInput(c.Key("relay-address"), &w.access.relay).Label(w.tr("跨网中继地址", "Relay URL")).Placeholder(w.tr("中继地址（可选，局域网无需配置）", "Relay URL (optional on your network)")).FillWidth()
		ui.Text(c, w.tr("设备优先直连；跨网络时，通过这里配置的 Vantry 原生中继连接。", "Devices connect directly when available and use the native Vantry relay across networks.")).FontSize(11).TextColor(p.faint).Wrap()
		ui.TextInput(c.Key("t3-public-url"), &w.access.publicURL).Label(w.tr("手机连接地址", "Mobile connection address")).Placeholder("https://…").FillWidth()
		ui.Text(c, w.tr("填写手机可访问的 HTTPS 根地址。二维码与手动连接均使用此地址。", "Use a phone-reachable HTTPS root address. Both the QR code and manual connection use this address.")).FontSize(11).TextColor(p.faint).Wrap()
		advanced := quietButton(c, p, "relay-token", w.tr("访问令牌", "Access token")).Label(w.tr("中继访问令牌设置", "Relay token settings")).Height(24).Justify(ui.Start).Padding(0).FontSize(11)
		if advanced.Clicked() {
			w.access.advanced = !w.access.advanced
		}
		if w.access.advanced {
			ui.Text(c, w.tr("本机中继路由：", "Device relay route: ")+w.access.status.RouteID).Font("Menlo").FontSize(10).TextColor(p.faint).Selectable().Wrap()
			ui.TextInput(c.Key("relay-key"), &w.access.key).Label(w.tr("中继访问令牌", "Relay access token")).Placeholder(w.tr("填写可替换已保存的令牌", "Enter a new token to replace the saved one")).Password().FillWidth()
		}
		ui.Row(c).Gap(8).Children(func() {
			if ui.PrimaryButton(c.Key("save-settings"), w.tr("保存设置", "Save settings")).Height(30).Padding(0, 14).Disabled(w.features.busy["remote-access"]).Clicked() {
				w.saveRemoteAccess("access.configure")
			}
			ui.Spacer(c)
			if w.access.status.Enabled {
				if quietButton(c, p, "reset-pair", w.tr("重置配对", "Reset pairing")).Height(26).FontSize(11).TextColor(p.muted).Clicked() {
					w.accessCall(api.WorkbenchRequest{Action: "access.rotate"})
				}
			}
		})
		if w.access.status.Error != "" {
			ui.Text(c, w.access.status.Error).FontSize(11).TextColor(p.warning).Wrap()
		}
	})
}
