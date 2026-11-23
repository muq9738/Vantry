package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// Remote desktop is a native plugin page using LAN by default or a relay
// explicitly configured by the user. The desktop draws and forwards input;
// the launcher links the RemoteDesktopDriver through Options.

// RemoteDesktopStats is the live state of a connection.
type RemoteDesktopStats struct {
	Connected           bool
	Mode                string // "direct-webrtc" or "relay-webrtc"; Transport describes the selected route.
	Transport           string
	Codec               string
	Hardware            bool
	FirstFrameMs        float64
	FrameAgeMs          float64
	FrameAgeP50Ms       float64
	FrameAgeP95Ms       float64
	ClockSynchronized   bool
	LocalCandidateType  string
	RemoteCandidateType string
	CandidateProtocol   string
	RelayHost           string
	Width, Height       int
	FPS                 int
	RTTMs               int
	Kbps                int
	Error               string
}

// RemoteDesktopConnect is what one connection asks the engine for.
type RemoteDesktopConnect struct {
	Target       string // opaque LAN or self-hosted relay connection link
	DirectAddrs  []string
	RelaySSHHost string // optional, supplied by the user
	RelayURL     string
	Token        string
	Quality      string // "ultra" | "balanced" | "low"
	DirectOnly   bool
	RelayOnly    bool
}

// RemoteDesktopHostStatus describes this machine while it is controlled.
type RemoteDesktopHostStatus struct {
	Hosting        bool      `json:"hosting"`
	StartRequestID string    `json:"startRequestId,omitempty"`
	ListenAddr     string    `json:"listenAddr"`
	DirectAddrs    []string  `json:"directAddrs"`
	RelaySSHHost   string    `json:"relaySshHost"`
	RelayPort      int       `json:"relayPort"`
	MobileWebURL   string    `json:"mobileWebUrl"`
	Token          string    `json:"token"`
	ConnectURL     string    `json:"connectUrl,omitempty"`
	Clients        int       `json:"clients"`
	ShareCode      string    `json:"shareCode,omitempty"`
	ShareExpiresAt time.Time `json:"shareExpiresAt,omitempty"`
	PendingViewer  string    `json:"pendingViewer,omitempty"`
}

// RemoteDesktopDriver is the streaming engine. Connect returns once the
// session is up (or failed); onFrame may be called from any goroutine when
// a new frame is ready, and LatestFrame returns it with a sequence that
// advances per frame. The frame must stay unchanged until the next one.
type RemoteDesktopDriver interface {
	Connect(ctx context.Context, cfg RemoteDesktopConnect, onFrame func()) error
	Disconnect() error
	LatestFrame() (*image.RGBA, uint64, RemoteDesktopStats)
	SendMouse(kind string, normX, normY float64, button int)
	SendWheel(dx, dy float64)
	SendKey(kind string, keyCode int, text string, modifiers uint32)
	SetQuality(quality string)
}

// RemoteDesktopHoster is optionally implemented by a driver that also hosts
// this machine; without it the view uses Core's rd.* workbench actions.
type RemoteDesktopHoster interface {
	HostStart(ctx context.Context, relaySSHHost string) (RemoteDesktopHostStatus, error)
	HostStop(ctx context.Context) (RemoteDesktopHostStatus, error)
	HostStatus(ctx context.Context) (RemoteDesktopHostStatus, error)
}

const (
	actionRDStatus    = "rd.status"
	actionRDHostStart = "rd.host_start"
	actionRDHostStop  = "rd.host_stop"
)

var rdQualities = []string{"ultra", "balanced", "low"}

type remoteDesktopState struct {
	pluginID string
	target   string
	token    string
	// legacyRelay is a relay saved in the plugin by earlier versions; the
	// shared remote environment relay takes precedence over it.
	legacyRelay string
	quality     int
	connecting  bool
	connected   bool
	err         string
	generation  int
	cancel      context.CancelFunc

	bitmap         *ui.Bitmap
	seq            uint64
	stats          RemoteDesktopStats
	frameW, frameH int
	viewW, viewH   float32
	focus          ui.Handle
	maximized      bool
	heldKeys       map[int]bool
	heldButtons    map[int]bool
	lastX, lastY   float64
	wake           atomic.Bool

	host          *RemoteDesktopHostStatus
	hostErr       string
	hostLoaded    bool
	nextHostPoll  time.Time
	qr            pairingQRState
	showGuestCode bool
	hostPending   bool
	permission    remotePermissionState
}

// openRemoteDesktop resolves a page contributed by an installed plugin.
func (w *workbench) openRemoteDesktop(target string) {
	for _, plugin := range w.features.plugins {
		if plugin.Error != "" {
			continue
		}
		for _, page := range plugin.Navigation {
			if page.NativePanel == "remote-desktop" {
				w.openPluginPage(plugin, page)
				if target = strings.TrimSpace(target); target != "" && !w.rd.connected {
					w.rd.target = target
				}
				return
			}
		}
	}
	w.openDock("plugins")
}

func (w *workbench) prepareRemoteDesktop(plugin api.PluginInfo) {
	if w.rd.pluginID != plugin.ID {
		w.rd.pluginID = plugin.ID
		w.rd.quality = 1
		w.rd.legacyRelay = ""
		for _, setting := range plugin.Settings {
			switch setting.Key {
			case "relayURL", "defaultRelayHost":
				if strings.Contains(setting.Value, "://") {
					w.rd.legacyRelay = setting.Value
				}
			case "quality", "defaultQuality":
				for i, quality := range rdQualities {
					if setting.Value == quality || setting.Value == "high" && quality == "ultra" {
						w.rd.quality = i
					}
				}
			}
		}
	}
	if w.dock.maximized {
		w.dock.maximized = false
	}
	w.loadSSHHosts(false)
	w.loadRemoteHostStatus()
}

func (w *workbench) stageView(c *ui.Context, p palette, _ bool) {
	if strings.HasPrefix(w.stage, stagePluginPrefix) {
		w.pluginPageStage(c, p)
	} else {
		w.stage = ""
	}
}

// stageHeader is the strip above a main-stage workspace: back to the
// conversation, its title, and its own actions.
func (w *workbench) stageHeader(c *ui.Context, p palette, icon *ui.SVG, title string, trailing func()) {
	ui.Row(c.Key("stage-header")).Height(44).Padding(0, 12, 0, 16).Gap(8).AlignItems(ui.Center).Shrink(0).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
		back := quietButton(c, p, "stage-back", "").Label(w.tr("返回对话", "Back to conversation")).Height(28).Padding(0, 8)
		back.Children(func() {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, backIcon).FontSize(12)
				ui.Text(c, w.tr("对话", "Chat")).FontSize(11)
			})
		})
		if back.Clicked() {
			w.stage = ""
			w.widgets.composerFocus.Focus()
		}
		ui.Box(c).Size(1, 16).Background(p.line).Shrink(0)
		ui.Icon(c, icon).FontSize(14).TextColor(p.accentInk)
		ui.Text(c, title).FontSize(13).Bold().TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0).Role(ui.RoleHeading).Level(2)
		if trailing != nil {
			trailing()
		}
	})
}

// ------------------------------------------------------------ connection

// sharedRelay is the relay of this computer's remote environments
// (Settings › Environments): remote desktop crosses networks through the
// same relay instead of keeping an address of its own.
func (w *workbench) sharedRelay() string {
	root := w
	if h := w.environments; h != nil && h.root != nil {
		root = h.root
	}
	relay := nonEmptyString(strings.TrimSpace(root.access.status.Relay), strings.TrimSpace(root.access.relay))
	if relay == "" {
		relay = strings.TrimSpace(w.rd.legacyRelay)
	}
	return strings.TrimRight(relay, "/")
}

// remoteDesktopLANTarget reports a LAN connection link, which never uses a
// relay; codes and desktop links go through the shared relay.
func remoteDesktopLANTarget(target string) bool {
	return strings.HasPrefix(strings.TrimSpace(target), "vantry-lan:")
}

// remoteDesktopNeedsRelay reports a target only a relay can reach: a short
// desktop code or a relay ticket. Desktop links carry their relay.
func remoteDesktopNeedsRelay(target string) bool {
	target = strings.TrimSpace(target)
	return strings.HasPrefix(strings.ToUpper(target), "DESK-") || strings.HasPrefix(target, "vantry-rtc:")
}

// remoteDesktopConfig connects LAN links directly and everything else
// through the shared environment relay, when one is configured.
func (w *workbench) remoteDesktopConfig() RemoteDesktopConnect {
	cfg := RemoteDesktopConnect{Target: strings.TrimSpace(w.rd.target), Quality: rdQualities[min(max(w.rd.quality, 0), 2)], DirectOnly: true}
	if cfg.Target == "" || remoteDesktopLANTarget(cfg.Target) {
		return cfg
	}
	if relay := w.sharedRelay(); relay != "" {
		cfg.DirectOnly, cfg.RelayOnly = false, true
		cfg.RelaySSHHost, cfg.RelayURL = relay, relay
	}
	return cfg
}

func (w *workbench) connectRemoteDesktop() bool {
	driver := w.defaults.RemoteDesktop
	if strings.TrimSpace(w.rd.target) == "" {
		w.rd.err = w.tr("请选择或输入要连接的机器。", "Choose or enter the machine to connect to.")
		return false
	}
	if driver == nil {
		w.rd.err = w.tr("当前构建未链接远程桌面引擎。", "No remote desktop engine is linked into this build.")
		return false
	}
	if w.rd.connecting || w.rd.connected {
		return false
	}
	cfg := w.remoteDesktopConfig()
	if cfg.RelaySSHHost == "" && remoteDesktopNeedsRelay(cfg.Target) {
		w.rd.err = w.tr("跨网络连接需要中继：请在 设置 › 远程环境 配置中继，或粘贴包含中继地址的桌面共享链接。", "Connecting across networks needs a relay: set one in Settings › Environments, or paste a sharing link that includes it.")
		return false
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.rd.generation++
	generation := w.rd.generation
	w.rd.cancel, w.rd.connecting, w.rd.err = cancel, true, ""
	w.rd.seq, w.rd.stats = 0, RemoteDesktopStats{}
	onFrame := func() {
		// One queued wake-up at a time: the view pulls the latest frame.
		if w.rd.wake.CompareAndSwap(false, true) {
			w.post(func() { w.rd.wake.Store(false) })
		}
	}
	w.start(func() {
		err := driver.Connect(ctx, cfg, onFrame)
		w.post(func() {
			if generation != w.rd.generation {
				return
			}
			w.rd.connecting = false
			if err != nil {
				cancel()
				w.rd.err = remoteDesktopFailure(w.tr("远程桌面连接失败", "Remote desktop connection failed"), err)
				return
			}
			w.rd.connected = true
			w.rd.focus.Focus()
		})
	})
	return true
}

func (w *workbench) disconnectRemoteDesktop() {
	driver := w.defaults.RemoteDesktop
	if driver == nil || !w.rd.connected && !w.rd.connecting {
		return
	}
	w.releaseRemoteKeys()
	w.rd.generation++
	w.rd.connected, w.rd.connecting = false, false
	if w.rd.cancel != nil {
		w.rd.cancel()
		w.rd.cancel = nil
	}
	w.rd.stats, w.rd.seq = RemoteDesktopStats{}, 0
	w.rd.maximized = false
	w.start(func() { _ = driver.Disconnect() })
}

// pullRemoteFrame shows the engine's latest frame through one reusable
// bitmap, uploading only when the sequence advanced.
func (w *workbench) pullRemoteFrame() {
	driver := w.defaults.RemoteDesktop
	if driver == nil || !w.rd.connected {
		return
	}
	frame, seq, stats := driver.LatestFrame()
	w.rd.stats = stats
	if !stats.Connected && stats.Error != "" {
		w.disconnectRemoteDesktop()
		w.rd.err = remoteDesktopFailure(w.tr("远程桌面已断开", "Remote desktop disconnected"), fmt.Errorf("%s", stats.Error))
		return
	}
	if frame == nil || seq == w.rd.seq {
		return
	}
	if w.rd.bitmap == nil {
		w.rd.bitmap = ui.NewBitmap(frame)
	} else {
		w.rd.bitmap.Update(frame)
	}
	w.rd.seq = seq
	w.rd.frameW, w.rd.frameH = frame.Bounds().Dx(), frame.Bounds().Dy()
}

// letterbox fits a frame of fw×fh pixels inside r, keeping its aspect.
func letterbox(fw, fh int, r ui.Rect) ui.Rect {
	if fw <= 0 || fh <= 0 || r.W <= 0 || r.H <= 0 {
		return r
	}
	scale := min(r.W/float32(fw), r.H/float32(fh))
	w, h := float32(fw)*scale, float32(fh)*scale
	return ui.Rect{X: r.X + (r.W-w)/2, Y: r.Y + (r.H-h)/2, W: w, H: h}
}

// normalizeRemotePoint maps a viewport point to [0, 1] across the frame.
func (w *workbench) normalizeRemotePoint(x, y float32) (float64, float64, bool) {
	r := letterbox(w.rd.frameW, w.rd.frameH, ui.Rect{W: w.rd.viewW, H: w.rd.viewH})
	if r.W <= 0 || r.H <= 0 {
		return 0, 0, false
	}
	nx, ny := (x-r.X)/r.W, (y-r.Y)/r.H
	inside := nx >= 0 && nx <= 1 && ny >= 0 && ny <= 1
	return float64(min(max(nx, 0), 1)), float64(min(max(ny, 0), 1)), inside
}

func (w *workbench) remoteInput(ev ui.InputEvent) bool {
	driver := w.defaults.RemoteDesktop
	if driver == nil || !w.rd.connected {
		return false
	}
	switch ev.Kind {
	case ui.InputPointerDown, ui.InputPointerUp, ui.InputPointerMove:
		nx, ny, inside := w.normalizeRemotePoint(ev.X, ev.Y)
		kind := "move"
		switch ev.Kind {
		case ui.InputPointerDown:
			if !inside {
				return false
			}
			kind = "down"
			w.rd.focus.Focus()
		case ui.InputPointerUp:
			kind = "up"
		default:
			if !inside {
				return false
			}
		}
		button := ev.Button
		if kind == "move" {
			for held := 0; held < 3; held++ {
				if w.rd.heldButtons[held] {
					kind, button = "drag", held
					break
				}
			}
		}
		w.rd.lastX, w.rd.lastY = nx, ny
		if kind == "down" {
			if w.rd.heldButtons == nil {
				w.rd.heldButtons = map[int]bool{}
			}
			w.rd.heldButtons[ev.Button] = true
		} else if kind == "up" {
			delete(w.rd.heldButtons, ev.Button)
		}
		driver.SendMouse(kind, nx, ny, button)
		return true
	case ui.InputScroll:
		driver.SendWheel(float64(ev.DX), float64(ev.DY))
		return true
	case ui.InputKeyDown:
		// Printable input is committed by the local input method. Sending its
		// physical key too would type it twice or bypass IME composition.
		if remoteTextKey(ev) && ev.Mods&(ui.Ctrl|ui.Super) == 0 {
			return false
		}
		code, ok := remoteMacKey(ev)
		if !ok {
			return false
		}
		if w.rd.heldKeys == nil {
			w.rd.heldKeys = map[int]bool{}
		}
		w.rd.heldKeys[code] = true
		driver.SendKey("down", code, "", remoteCGFlags(ev.Mods))
		return true
	case ui.InputKeyUp:
		code, ok := remoteMacKey(ev)
		if !ok || !w.rd.heldKeys[code] {
			return false
		}
		delete(w.rd.heldKeys, code)
		driver.SendKey("up", code, "", remoteCGFlags(ev.Mods))
		return true
	case ui.InputText:
		runes := []rune(ev.Text)
		for len(runes) > 0 {
			n := min(512, len(runes))
			driver.SendKey("down", 0, string(runes[:n]), 0)
			driver.SendKey("up", 0, "", 0)
			runes = runes[n:]
		}
		return true
	case ui.InputCompose:
		return true // Only committed Unicode is sent to the remote input sink.
	}
	return false
}

// ------------------------------------------------------------ hosting

func decodeHostStatus(response api.WorkbenchResponse) *RemoteDesktopHostStatus {
	for _, raw := range [][]byte{[]byte(strings.TrimSpace(response.Output)), response.Data} {
		if len(raw) == 0 {
			continue
		}
		var status RemoteDesktopHostStatus
		if json.Unmarshal(raw, &status) == nil {
			return &status
		}
	}
	return nil
}

// hostCall always addresses the Core that owns this view. A local decoder's
// optional host methods must never turn a remote view into local capture.
func (w *workbench) hostCall(action string, done func(*RemoteDesktopHostStatus, error)) {
	// No relay is named: the owning Core uses its shared environment relay.
	w.workbenchCall("rd-host", api.WorkbenchRequest{Action: action}, func(response api.WorkbenchResponse, err error) {
		done(decodeHostStatus(response), err)
	})
}

func (w *workbench) loadRemoteHostStatus() {
	if w.rd.hostLoaded {
		return
	}
	w.rd.hostLoaded = true
	epoch := w.rd.permission.hostEpoch
	w.hostCall(actionRDStatus, func(status *RemoteDesktopHostStatus, err error) {
		if epoch != w.rd.permission.hostEpoch {
			return
		}
		if err == nil && status != nil {
			w.rd.host = status
		}
	})
}

func (w *workbench) setHosting(on bool) {
	if on {
		w.beginRemoteHostingIntent()
		return
	}
	w.cancelRemoteHostingIntent()
	w.rd.permission.hostEpoch++
	epoch := w.rd.permission.hostEpoch
	action, label := actionRDHostStop, w.tr("停止被控", "Stop hosting")
	w.rd.hostPending, w.rd.hostErr = true, ""
	w.hostCall(action, func(status *RemoteDesktopHostStatus, err error) {
		if epoch != w.rd.permission.hostEpoch {
			return
		}
		w.rd.hostPending = false
		if err != nil {
			w.rd.hostErr = remoteDesktopFailure(label, err)
			return
		}
		if status != nil {
			w.rd.host = status
		} else {
			w.rd.host = &RemoteDesktopHostStatus{Hosting: on}
		}
	})
}

func remoteDesktopFailure(action string, err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "屏幕录制"):
		return action + "：请在系统设置 → 隐私与安全性 → 屏幕录制中允许 Vantry，然后重新开启被控。"
	case strings.Contains(message, "辅助功能"):
		return action + "：请在系统设置 → 隐私与安全性 → 辅助功能中允许 Vantry。"
	case strings.Contains(message, "unauthorized"):
		return action + "：连接口令无效或已过期，请从对方机器重新复制。"
	case strings.Contains(message, "no active displays"):
		return action + "：目标机器没有可用显示器。"
	case strings.Contains(message, "LAN host is unreachable"):
		return action + "：确认两台电脑在同一局域网，并在系统设置允许 Vantry 的本地网络和传入连接；跨网络需配置自己的中继。"
	case strings.Contains(message, "LAN connection link") || strings.Contains(message, "LAN link certificate"):
		return action + "：请从目标电脑重新复制本次共享的局域网连接地址。"
	case strings.Contains(message, "your relay is unavailable"):
		return action + "：请检查你配置的 SSH 主机和中继服务。"
	default:
		return describe(action, err)
	}
}

// ------------------------------------------------------------ view

func (w *workbench) rdModeBadge(stats RemoteDesktopStats) (string, bool) {
	relay := nonEmptyString(stats.RelayHost, w.sharedRelay())
	var text string
	direct := stats.Mode == "direct" || stats.Mode == "direct-webrtc" || stats.Mode == "" && remoteDesktopLANTarget(w.rd.target)
	if stats.Mode == "relay-webrtc" || stats.Mode == "direct-webrtc" {
		direct = stats.Mode == "direct-webrtc"
		text = w.tr("中继", "Relay")
		if direct {
			text = w.tr("局域网", "LAN")
		}
		text += " · WebRTC / H.264"
		if stats.Transport == "ssh-tcp" {
			text += " · SSH/TCP"
		} else if stats.Transport == "wss-tcp" {
			text += " · WSS/TCP"
		}
		if stats.Hardware {
			text += w.tr(" · 硬解", " · hardware decode")
		}
		if stats.FirstFrameMs > 0 {
			text += fmt.Sprintf(w.tr(" · 首帧 %.0fms", " · first frame %.0fms"), stats.FirstFrameMs)
		}
		if stats.ClockSynchronized && stats.FrameAgeP95Ms > 0 {
			text += fmt.Sprintf(w.tr(" · 估计帧龄 P95 %.1fms", " · estimated frame age P95 %.1fms"), stats.FrameAgeP95Ms)
		}
	} else if direct {
		text = w.tr("局域网直连", "LAN direct")
	} else if stats.Mode == "relay-http" {
		text = w.tr("中继 Relay (HTTP)", "Relay (HTTP)")
	} else {
		text = fmt.Sprintf(w.tr("中继 Relay (%s)", "Relay (%s)"), relay)
	}
	if stats.RTTMs > 0 {
		text += fmt.Sprintf(" · %dms", stats.RTTMs)
	}
	if stats.FPS > 0 {
		text += fmt.Sprintf(" · %d FPS", stats.FPS)
	}
	return text, direct
}

func (w *workbench) remoteDesktopPeerLabel() string {
	if ticket, err := url.Parse(strings.TrimSpace(w.rd.target)); err == nil && (ticket.Scheme == "vantry-rtc" || ticket.Scheme == "vantry-lan") {
		return ticket.Hostname()
	}
	return w.tr("远端会话", "Remote session")
}

func (w *workbench) remoteDesktopStage(c *ui.Context, p palette) {
	w.pullRemoteFrame()
	if w.rd.host != nil && w.rd.host.Hosting {
		now := time.Now()
		if w.rd.nextHostPoll.IsZero() {
			w.rd.nextHostPoll = now.Add(time.Second)
		}
		if !now.Before(w.rd.nextHostPoll) {
			w.rd.nextHostPoll = now.Add(time.Second)
			w.rd.hostLoaded = false
			w.loadRemoteHostStatus()
		}
		c.After(time.Second)
	}
	ui.Column(c.Key("remote-desktop-stage")).Grow(1).MinHeight(0).Children(func() {
		if !w.rd.maximized {
			title := w.tr("远程桌面", "Remote Desktop")
			if w.rd.connected {
				title += " · " + w.remoteDesktopPeerLabel()
			}
			w.stageHeader(c, p, remoteIcon, title, func() {
				hosting := w.rd.host != nil && w.rd.host.Hosting
				badge := w.tr("就绪 · 端到端独立授权", "Ready · explicit host approval")
				tone := p.faint
				if hosting {
					badge = w.tr("正在共享本机桌面", "Sharing this desktop")
					tone = p.success
				}
				ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
					if hosting {
						ui.Box(c).Size(6, 6).Radius(3).Background(p.success)
					}
					ui.Text(c, badge).FontSize(11).TextColor(tone).NoWrap()
				})
			})
		}
		if w.rd.connected {
			w.remoteViewport(c, p)
			return
		}
		ui.Scroll(c.Key("rd-setup")).Grow(1).MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().Padding(22, 28, 36).AlignItems(ui.Center).Children(func() {
				ui.Column(c).FillWidth().MaxWidth(980).Gap(18).Children(func() {
					ui.Row(c).FillWidth().Padding(12, 16).Gap(12).Radius(12).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Children(func() {
						ui.Row(c).Size(34, 34).Radius(9).Background(p.accent.Alpha(0.12)).Justify(ui.Center).AlignItems(ui.Center).Shrink(0).Children(func() {
							ui.Icon(c, remoteIcon).FontSize(17).TextColor(p.accentInk)
						})
						ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
							ui.Text(c, w.tr("点对点远程桌面", "Peer-to-peer Remote Desktop")).FontSize(13.5).FontWeight(600).TextColor(p.ink).SingleLine()
							ui.Text(c, w.tr("局域网内零配置直连；跨网络自动使用远程环境的中继。每次观看与键鼠控制均需被控端单独授权。", "Direct on your network; across networks it uses your remote environments' relay. Every session requires explicit host approval.")).
								FontSize(12).TextColor(p.muted).Wrap()
						})
					})
					ui.Row(c).FillWidth().Gap(18).Wrap().AlignItems(ui.Start).Children(func() {
						w.rdConnectCard(c, p)
						w.rdHostCard(c, p)
					})
				})
			})
		})
	})
}

// rdCard is one column of the remote desktop page: a clean titled surface
// with hairline-divided rows.
func (w *workbench) rdCard(c *ui.Context, p palette, key string, icon *ui.SVG, title, subtitle string, body func()) {
	width, _ := c.Size()
	ui.Column(c.Key(key)).Grow(1).Basis(0).MinWidth(min(420, width-56)).Gap(8).Children(func() {
		ui.Row(c).Padding(0, 2).Gap(7).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon).FontSize(14).TextColor(p.muted)
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink).Role(ui.RoleHeading).Level(3).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
		})
		settingsGroup(c, p, key+"-group", func() {
			if subtitle != "" {
				ui.Text(c, subtitle).FontSize(12).TextColor(p.muted).Wrap().Padding(12, 16, 0)
			}
			body()
		})
	})
}

func (w *workbench) rdConnectCard(c *ui.Context, p palette) {
	w.rdCard(c, p, "rd-connect", lucide("arrow-up-right"), w.tr("连接另一台电脑", "Connect to another computer"), "", func() {
		ui.Column(c).Padding(14, 16).Gap(8).Children(func() {
			ui.Text(c, w.tr("共享码或连接链接", "Sharing code or link")).FontSize(12.5).FontWeight(500).TextColor(p.ink)
			placeholder := "vantry-lan://… / DESK-… / vantry://desktop…"
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				input := ui.TextInput(c.Key("rd-target"), &w.rd.target).Label(w.tr("远程桌面目标", "Remote desktop target")).
					Placeholder(placeholder).Password().Grow(1).MinWidth(0).Height(34).Padding(0, 11)
				connect := ui.PrimaryButton(c.Key("rd-connect-button"), w.tr("连接", "Connect")).Label(w.tr("立即远程连接", "Connect Desktop")).
					Height(34).Padding(0, 16).Radius(8).Shrink(0)
				if w.rd.connecting || strings.TrimSpace(w.rd.target) == "" {
					connect.Disabled(true)
				}
				if connect.Clicked() || input.Submitted() && !w.rd.connecting && strings.TrimSpace(w.rd.target) != "" {
					w.connectRemoteDesktop()
				}
			})
			ui.Text(c, w.tr("局域网链接直接连接；共享码与跨网链接自动经远程环境的中继。", "LAN links connect directly; codes and cross-network links go through your environments' relay.")).FontSize(11.5).TextColor(p.faint).Wrap()
			if w.rd.connecting {
				ui.Row(c).Gap(8).Padding(2, 0, 0).AlignItems(ui.Center).Children(func() {
					activityIndicator(c, p.accent)
					ui.Text(c, w.tr("正在等待对方确认或建立连接…", "Connecting and awaiting host approval…")).FontSize(12).TextColor(p.muted)
				})
			}
			if w.rd.err != "" {
				ui.Row(c).Gap(6).Padding(8, 10).Radius(8).Background(p.danger.Alpha(0.08)).AlignItems(ui.Start).Children(func() {
					ui.Icon(c, alertIcon).FontSize(13).TextColor(p.danger).Shrink(0)
					ui.Text(c, w.rd.err).FontSize(12).TextColor(p.danger).Wrap().Grow(1).MinWidth(0)
				})
			}
		})
		w.rdSharedRelayRow(c, p, "rd-viewer-relay")
		settingsRow(c, p, "rd-quality-row", false, w.tr("画面偏好", "Stream quality"), w.tr("连接后也可在顶部悬浮栏实时切换。", "Can also be switched live from the stream bar."), func() {
			ui.Segmented(c.Key("rd-quality"), &w.rd.quality, w.tr("高清", "Ultra"), w.tr("均衡", "Balanced"), w.tr("省流", "Saver")).Label(w.tr("画质", "Quality"))
		})
	})
}

// rdSharedRelayRow shows the relay remote desktop uses across networks:
// the one configured for remote environments, edited only in Settings.
func (w *workbench) rdSharedRelayRow(c *ui.Context, p palette, key string) {
	relay := w.sharedRelay()
	value := relay
	if value == "" {
		value = w.tr("未配置 · 仅局域网", "Not set · LAN only")
	}
	settingsRow(c, p, key, false, w.tr("跨网中继", "Relay"), w.tr("与远程环境共用同一配置。", "Shared with your remote environments."), func() {
		ui.Text(c, value).Font("Menlo").FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…").MaxWidth(220)
		if quietButton(c, p, key+"-settings", w.tr("设置", "Settings")).Label(w.tr("在设置中修改远程环境中继", "Change the environment relay in Settings")).Height(26).Padding(0, 8).Clicked() {
			w.widgets.settingsOpen = true
			w.widgets.settingsPage = "environments"
		}
	})
}

func (w *workbench) rdInfoRow(c *ui.Context, p palette, key, label, value string, copyable bool) {
	settingsRow(c, p, key, false, label, "", func() {
		ui.Text(c, nonEmptyString(value, "—")).Font("Menlo").FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…").Selectable().MaxWidth(260)
		if copyable && value != "" {
			if iconButton(c, p, "copy", w.tr("复制", "Copy ")+label, copyIcon).Size(26, 26).Padding(6).Clicked() {
				c.WriteClipboard(value)
			}
		}
	})
}

func (w *workbench) rdHostCard(c *ui.Context, p palette) {
	w.rdCard(c, p, "rd-host", lucide("monitor"), w.tr("共享本机桌面 · ", "Share this computer · ")+w.hostLabel(), "", func() {
		permission := &w.rd.permission
		if permission.status == nil && !permission.busy && permission.err == "" {
			w.probeRemotePermissions()
		}
		hosting := w.rd.host != nil && w.rd.host.Hosting
		status := w.tr("关闭时不捕获屏幕，也不接受远端输入。", "No screen or input is shared while off.")
		switch {
		case !hosting && (w.rd.hostPending || permission.starting):
			status = w.tr("正在开启共享…", "Starting host…")
		case permission.intent && !hosting:
			status = w.tr("等待完成系统权限授权", "Awaiting system permission")
		case hosting:
			status = w.tr("正在共享", "Sharing active")
			if w.rd.host.Clients > 0 {
				status += fmt.Sprintf(w.tr(" · %d 台设备已连接", " · %d connected"), w.rd.host.Clients)
			}
		}
		settingsRow(c, p, "rd-hosting-row", true, w.tr("允许此电脑被控制", "Allow this computer to be controlled"), status, func() {
			if w.rd.hostPending {
				activityIndicator(c, p.accent)
			}
			on := hosting
			toggle := ui.Switch(c.Key("rd-hosting"), &on).Label(w.tr("本机被控", "Host this computer"))
			if w.rd.hostPending || w.features.busy["rd-host"] || permission.intent || permission.busy {
				toggle.Disabled(true)
			}
			if on != hosting {
				w.setHosting(on)
			}
		})
		w.remotePermissionPanel(c, p)
		if w.rd.hostErr != "" {
			ui.Row(c).Padding(8, 16, 12).Children(func() {
				ui.Row(c).FillWidth().Gap(6).Padding(8, 10).Radius(8).Background(p.danger.Alpha(0.08)).AlignItems(ui.Start).Children(func() {
					ui.Icon(c, alertIcon).FontSize(13).TextColor(p.danger).Shrink(0)
					ui.Text(c, w.rd.hostErr).FontSize(12).TextColor(p.danger).Wrap().Grow(1).MinWidth(0)
				})
			})
		}
		if !hosting {
			return
		}
		host := w.rd.host
		w.desktopPairing(c, p, host)
		if w.rd.showGuestCode {
			settingsRow(c.Key("rd-ticket"), p, "rd-ticket-row", false, w.tr("临时分享链接", "Guest sharing link"), w.tr("仅供未配对电脑申请临时访问。", "For an unpaired computer to request temporary access."), func() {
				if outlineButton(c, p, "copy-rtc-ticket", w.tr("复制链接", "Copy link")).Clicked() {
					c.WriteClipboard(host.ConnectURL)
				}
			})
		}
		w.rdInfoRow(c, p, "rd-lan-status", w.tr("局域网地址", "LAN address"), strings.Join(host.DirectAddrs, ", "), false)
		if host.RelaySSHHost != "" {
			w.rdInfoRow(c, p, "rd-relay-status", w.tr("跨网中继", "Relay"), host.RelaySSHHost, false)
		}
	})
}

func (w *workbench) remoteViewport(c *ui.Context, p palette) {
	stats := w.rd.stats
	view := ui.Box(c.Key("rd-viewport")).Grow(1).MinHeight(0).FillWidth().Background(ui.Hex("#05060A")).Focusable().Bind(&w.rd.focus).
		Role(ui.RoleGroup).Label(w.tr("远程桌面画面", "Remote desktop screen")).Cursor(ui.CursorDefault)
	bounds := view.Bounds()
	w.rd.viewW, w.rd.viewH = bounds.W, bounds.H
	view.HandleInput(w.remoteInput).InputMethod(true).TextCaret(ui.Rect{X: float32(w.rd.lastX) * bounds.W, Y: float32(w.rd.lastY) * bounds.H, W: 1, H: 18})
	if view.FocusLeft() {
		w.releaseRemoteKeys()
	}
	bitmap, fw, fh := w.rd.bitmap, w.rd.frameW, w.rd.frameH
	view.Draw(func(painter *ui.Painter, r ui.Rect) {
		if bitmap == nil {
			return
		}
		painter.Image(bitmap, letterbox(fw, fh, r), ui.FillBox)
	})
	view.Children(func() {
		if bitmap == nil {
			ui.Column(c).Absolute().Left(0).Right(0).Top(0).Bottom(0).Justify(ui.Center).AlignItems(ui.Center).Gap(8).Children(func() {
				activityIndicator(c, p.accent)
				ui.Text(c, w.tr("等待第一帧画面…", "Waiting for the first frame…")).FontSize(12).TextColor(ui.Hex("#C9D1DD"))
			})
		}
		w.remoteHUD(c, p, stats)
	})
}

// remoteHUD floats over the stream: link mode and timing, quality, the
// full-stage toggle and disconnect.
func (w *workbench) remoteHUD(c *ui.Context, p palette, stats RemoteDesktopStats) {
	ui.Row(c.Key("rd-hud")).Absolute().Top(10).Left(0).Right(0).Justify(ui.Center).Children(func() {
		ui.Row(c).Height(36).Padding(0, 6, 0, 12).Gap(8).Radius(18).AlignItems(ui.Center).Background(ui.Hex("#11141BE6")).Border(1, ui.Hex("#FFFFFF1F")).
			Shadow(0, 6, 22, 0, ui.Hex("#00000066")).Children(func() {
			text, direct := w.rdModeBadge(stats)
			dot := ui.Hex("#22C55E")
			if !direct {
				dot = ui.Hex("#38BDF8")
			}
			ui.Box(c).Size(8, 8).Radius(4).Background(dot).Shrink(0)
			ui.Text(c, text).FontSize(11).TextColor(ui.Hex("#E7ECF3")).NoWrap().Label(w.tr("连接状态 ", "Link ") + text)
			if stats.Width > 0 {
				ui.Text(c, fmt.Sprintf("%d×%d", stats.Width, stats.Height)).Font("Menlo").FontSize(10).TextColor(ui.Hex("#8E99AB")).NoWrap()
			}
			ui.Box(c).Size(1, 16).Background(ui.Hex("#FFFFFF26"))
			quality := w.rd.quality
			ui.Segmented(c.Key("rd-hud-quality"), &quality, w.tr("高清原画", "Ultra"), w.tr("均衡流畅", "Balanced"), w.tr("省流", "Saver")).Label(w.tr("远程画质", "Stream quality"))
			if quality != w.rd.quality {
				w.rd.quality = quality
				if driver := w.defaults.RemoteDesktop; driver != nil {
					driver.SetQuality(rdQualities[quality])
				}
			}
			label, icon := w.tr("全屏画面", "Full stage"), expandIcon
			if w.rd.maximized {
				label, icon = w.tr("退出全屏画面", "Exit full stage"), collapseIcon
			}
			full := iconButton(c, p, "rd-maximize", label, icon).Size(26, 26).Padding(6).TextColor(ui.Hex("#C9D1DD"))
			if full.Clicked() {
				w.rd.maximized = !w.rd.maximized
			}
			stop := ui.Button(c.Key("rd-disconnect"), w.tr("断开", "Disconnect")).Label(w.tr("断开远程桌面", "Disconnect remote desktop")).Height(26).Padding(0, 10).Radius(13).
				Background(ui.Hex("#EF444429")).TextColor(ui.Hex("#FCA5A5")).Border(1, ui.Hex("#EF444455"))
			if stop.Clicked() {
				w.disconnectRemoteDesktop()
			}
		})
	})
	if stats.Error != "" {
		ui.Row(c.Key("rd-error")).Absolute().Bottom(14).Left(0).Right(0).Justify(ui.Center).Children(func() {
			ui.Text(c, stats.Error).FontSize(11).TextColor(ui.Hex("#FCA5A5")).Padding(6, 12).Radius(8).Background(ui.Hex("#11141BE6"))
		})
	}
}
