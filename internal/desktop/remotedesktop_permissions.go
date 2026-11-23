package desktop

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	actionRDPermissions        = "rd.permissions"
	actionRDPermissionRequest  = "rd.permission.request"
	actionRDPermissionSettings = "rd.permission.settings"
)

type RemotePermissionAccess struct {
	State           string `json:"state"`
	CanRequest      bool   `json:"canRequest"`
	CanOpenSettings bool   `json:"canOpenSettings"`
	Message         string `json:"message"`
}
type RemotePermissions struct {
	Platform          string                 `json:"platform"`
	ScreenCapture     RemotePermissionAccess `json:"screenCapture"`
	InputControl      RemotePermissionAccess `json:"inputControl"`
	CanHost           bool                   `json:"canHost"`
	UnavailableReason string                 `json:"unavailableReason"`
}
type remotePermissionState struct {
	status                        *RemotePermissions
	intent, shown, busy, starting bool
	generation                    uint64
	hostEpoch                     uint64
	requestID                     string
	cancel                        context.CancelFunc
	err                           string
	frameNow, nextProbe           time.Time
	polls                         int
}

func permissionReady(access RemotePermissionAccess) bool {
	return access.State == "granted" || access.State == "not-required"
}
func (s *RemotePermissions) ready() bool {
	return s != nil && s.CanHost && permissionReady(s.ScreenCapture) && permissionReady(s.InputControl)
}
func (w *workbench) permissionPageActive() bool {
	if w.rd.pluginID == "" || !strings.HasPrefix(w.stage, stagePluginPrefix+w.rd.pluginID+"\x00") {
		return false
	}
	return w.environments == nil || w.environments.current == w
}
func (w *workbench) beginRemoteHostingIntent() {
	if !w.permissionPageActive() {
		return
	}
	p := &w.rd.permission
	if p.intent || p.starting {
		return
	}
	p.generation++
	p.requestID = newCommandID("rd-enable")
	p.intent, p.shown = true, true
	p.err = ""
	p.polls = 0
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.busy = false
	w.probeRemotePermissions()
}
func (w *workbench) cancelRemoteHostingIntent() {
	p := &w.rd.permission
	if !p.intent && !p.busy && !p.starting {
		return
	}
	p.generation++
	p.intent, p.busy, p.starting = false, false, false
	p.polls = 0
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	w.rd.hostPending = false
}
func (w *workbench) probeRemotePermissions() { w.permissionCall(actionRDPermissions, "") }
func (w *workbench) requestRemotePermission(kind string) {
	if !w.rd.permission.intent || !w.permissionPageActive() {
		return
	}
	w.permissionCall(actionRDPermissionRequest, kind)
}
func (w *workbench) openRemotePermissionSettings(kind string) {
	if !w.rd.permission.intent || !w.permissionPageActive() {
		return
	}
	w.permissionCall(actionRDPermissionSettings, kind)
}
func (w *workbench) permissionCall(action, kind string) {
	p := &w.rd.permission
	if !w.permissionPageActive() || p.busy || p.starting {
		return
	}
	if action != actionRDPermissions && !p.intent {
		return
	}
	service, ok := w.service.(api.WorkbenchService)
	if !ok {
		p.err = w.tr("当前机器需要更新权限接口。", "Update the selected machine to use permission controls.")
		return
	}
	generation := p.generation
	timeout := 35 * time.Second
	if action == actionRDPermissionRequest {
		timeout = 75 * time.Second
	}
	ctx, cancel := context.WithTimeout(w.ctx, timeout)
	p.cancel = cancel
	p.busy = true
	p.err = ""
	w.start(func() {
		response, err := service.Workbench(ctx, api.WorkbenchRequest{Action: action, Name: kind})
		cancel()
		w.post(func() {
			if generation != p.generation || !w.permissionPageActive() {
				return
			}
			p.busy = false
			p.cancel = nil
			if err != nil {
				p.err = remoteDesktopFailure(w.tr("权限检查未完成", "Permission check did not finish"), err)
				return
			}
			var status RemotePermissions
			if json.Unmarshal([]byte(response.Output), &status) != nil || status.Platform == "" {
				p.err = w.tr("目标机器返回的权限状态无效，请更新目标应用。", "The target returned invalid permission information; update it.")
				return
			}
			p.status = &status
			if action != actionRDPermissions && p.intent {
				p.polls = 15
				p.nextProbe = p.frameNow.Add(2 * time.Second)
			}
			if p.intent && status.ready() {
				w.startPermissionAuthorizedHost()
			}
		})
	})
}
func (w *workbench) startPermissionAuthorizedHost() {
	p := &w.rd.permission
	if !p.intent || p.starting || !p.status.ready() || !w.permissionPageActive() {
		return
	}
	service, ok := w.service.(api.WorkbenchService)
	if !ok {
		return
	}
	generation := p.generation
	requestID := p.requestID
	p.hostEpoch++
	ctx, cancel := context.WithTimeout(w.ctx, 35*time.Second)
	p.cancel = cancel
	p.starting = true
	w.rd.hostPending = true
	// An empty relay asks the owning Core for its shared remote environment
	// relay: LAN first, that relay only as a lazy backup.
	w.start(func() {
		response, err := service.Workbench(ctx, api.WorkbenchRequest{Action: actionRDHostStart, Message: requestID})
		cancel()
		w.post(func() {
			if generation != p.generation || !p.intent || !w.permissionPageActive() {
				// A cancelled request can race an accepted Core response. Stop only on
				// the original operation on its Core; a later enable can adopt the host.
				w.start(func() {
					cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
					defer done()
					_, _ = service.Workbench(cleanup, api.WorkbenchRequest{Action: actionRDHostStop, Message: requestID})
				})
				return
			}
			p.starting = false
			p.cancel = nil
			w.rd.hostPending = false
			p.intent = false
			p.polls = 0
			if err != nil {
				p.err = remoteDesktopFailure(w.tr("开启被控失败", "Could not enable hosting"), err)
				return
			}
			status := decodeHostStatus(response)
			if status == nil || !status.Hosting {
				p.err = w.tr("目标机器未确认共享已开启。", "The target has not confirmed that sharing is enabled.")
				return
			}
			w.rd.host = status
			w.rd.hostErr = ""
			p.err = ""
		})
	})
}
func (w *workbench) remotePermissionFocusReturned() {
	if w.permissionPageActive() && w.rd.permission.intent && !w.rd.permission.busy && !w.rd.permission.starting {
		w.probeRemotePermissions()
	}
}
func (w *workbench) pollRemotePermissions(c *ui.Context) {
	p := &w.rd.permission
	p.frameNow = c.Now()
	if !p.intent || p.polls <= 0 || !w.permissionPageActive() || p.busy || p.starting {
		return
	}
	if p.nextProbe.After(c.Now()) {
		c.After(p.nextProbe.Sub(c.Now()))
		return
	}
	p.polls--
	p.nextProbe = c.Now().Add(2 * time.Second)
	w.probeRemotePermissions()
	if p.polls > 0 {
		c.After(2 * time.Second)
	}
}
func permissionStateText(w *workbench, state string) string {
	switch state {
	case "granted":
		return w.tr("已允许", "Allowed")
	case "not-required":
		return w.tr("无需授权", "Not required")
	case "denied":
		return w.tr("需要允许", "Permission needed")
	case "unavailable":
		return w.tr("组件不可用", "Component unavailable")
	default:
		return w.tr("待检查", "Not checked")
	}
}

// macOS privacy pane icons match the system permission being requested.
// Third-party source attributions are retained in THIRD_PARTY_NOTICES.md.
var (
	macScreenRecordingIcon = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><defs><linearGradient id="g" x2="0" y2="1"><stop stop-color="#ff6972"/><stop offset="1" stop-color="#ff2938"/></linearGradient></defs><rect x="0.5" y="0.5" width="31" height="31" rx="7" fill="url(#g)" stroke="#ffffff40"/><circle cx="16" cy="16" r="10" fill="none" stroke="#fff" stroke-width="2"/><circle cx="16" cy="16" r="4.5" fill="#fff"/></svg>`))
	macAccessibilityIcon   = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><defs><linearGradient id="g" x2="0" y2="1"><stop stop-color="#48b6ff"/><stop offset="1" stop-color="#0085ff"/></linearGradient></defs><rect x="0.5" y="0.5" width="31" height="31" rx="7" fill="url(#g)" stroke="#ffffff40"/><circle cx="16" cy="16" r="10" fill="none" stroke="#fff" stroke-width="1.75"/><circle cx="16" cy="10" r="1.6" fill="#fff"/><path d="m10 13 6 1 6-1M16 14v4m0 0-2.5 6m2.5-6 2.5 6" fill="none" stroke="#fff" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>`))
)

// remotePermissionPanel is T3 Code's PermissionChecklist inside the host
// group: one row per privacy pane, each either "已允许" or an Allow button
// with an Open Settings fallback.
func (w *workbench) remotePermissionPanel(c *ui.Context, palette palette) {
	p := &w.rd.permission
	w.pollRemotePermissions(c)
	if !p.shown {
		return
	}
	ui.Column(c.Key("rd-permission-panel")).FillWidth().BorderWidth(1, 0, 0, 0).BorderColor(palette.lineSoft).Children(func() {
		ui.Row(c).Padding(12, 16, 6).Gap(8).AlignItems(ui.Center).Children(func() {
			title := w.tr("开启前检查共享能力", "Check sharing support before enabling")
			if p.status != nil && p.status.Platform == "darwin" {
				title = w.tr("系统权限检查", "System permissions")
			}
			if w.defaults.RemotePeer || w.defaults.RemoteHost != "" {
				title = w.tr("请在 ", "Complete on ") + w.hostLabel() + w.tr(" 完成授权", " ")
			}
			ui.Text(c, title).FontSize(12).FontWeight(600).TextColor(palette.muted).Grow(1).Wrap()
			if p.intent {
				if quietButton(c, palette, "rd-permission-cancel", w.tr("取消开启", "Cancel")).Height(24).Clicked() {
					w.cancelRemoteHostingIntent()
				}
			}
			recheck := quietButton(c, palette, "rd-permission-recheck", w.tr("重新检查", "Check again")).Height(24)
			if recheck.Disabled(p.busy || p.starting).Clicked() {
				w.probeRemotePermissions()
			}
		})
		ui.Column(c).Padding(2, 16, 10).Gap(6).Children(func() {
			if p.status != nil {
				if p.status.UnavailableReason != "" {
					ui.Row(c).Gap(6).Padding(8, 10).Radius(8).Background(palette.warning.Alpha(0.08)).AlignItems(ui.Start).Children(func() {
						ui.Icon(c, lucide("triangle-alert")).FontSize(13).TextColor(palette.warning).Shrink(0)
						ui.Text(c, p.status.UnavailableReason).FontSize(12).TextColor(palette.warning).Wrap().Grow(1).MinWidth(0)
					})
				}
				screenLabel, inputLabel := w.tr("屏幕共享", "Screen sharing"), w.tr("键鼠操作", "Keyboard and mouse")
				if p.status.Platform == "darwin" {
					screenLabel, inputLabel = w.tr("屏幕录制", "Screen Recording"), w.tr("辅助功能", "Accessibility")
				}
				ui.Column(c).Radius(9).Border(1, palette.lineSoft).Background(palette.canvas.Alpha(0.35)).Children(func() {
					w.remotePermissionRow(c, palette, 0, "screen-capture", screenLabel, p.status.ScreenCapture)
					w.remotePermissionRow(c, palette, 1, "input-control", inputLabel, p.status.InputControl)
				})
			} else if p.busy {
				ui.Row(c).Gap(8).Padding(8, 4).AlignItems(ui.Center).Children(func() {
					activityIndicator(c, palette.accent)
					ui.Text(c, w.tr("正在检查这台机器…", "Checking this machine…")).FontSize(12).TextColor(palette.muted)
				})
			}
		})
		ui.Row(c).Padding(0, 16, 12).Gap(8).AlignItems(ui.Center).Children(func() {
			text := w.tr("授权后自动开启共享。", "Sharing starts once permission is granted.")
			if p.status != nil && p.status.Platform == "darwin" {
				text = w.tr("两项均允许后自动开启共享；在系统设置开启开关后返回即可。", "Sharing starts automatically once both are allowed in System Settings.")
			}
			if p.status != nil && p.status.UnavailableReason != "" {
				text = w.tr("组件就绪后才能开启共享。", "Sharing requires the native component to be available.")
			}
			if p.starting {
				text = w.tr("权限齐备，正在开启…", "Permissions ready, enabling…")
			}
			ui.Text(c, text).FontSize(11.5).TextColor(palette.faint).Grow(1).Wrap()
		})
		if p.err != "" {
			ui.Text(c, p.err).FontSize(12).TextColor(palette.danger).Wrap().Padding(0, 16, 12)
		}
	})
}

func (w *workbench) remotePermissionRow(c *ui.Context, p palette, index int, kind, label string, access RemotePermissionAccess) {
	state := &w.rd.permission
	icon, purpose := remoteIcon, w.tr("传输本机桌面画面", "Stream this screen to the viewer")
	if kind == "input-control" {
		icon, purpose = lucide("mouse-pointer-click"), w.tr("响应远端键盘与鼠标操作", "Accept remote keyboard & mouse input")
	}
	row := ui.Row(c.Key("rd-permission:"+kind)).Gap(10).Padding(9, 12).AlignItems(ui.Center)
	if index > 0 {
		row.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
	}
	row.Children(func() {
		iconTone := p.muted
		if permissionReady(access) {
			iconTone = p.success
		}
		ui.Row(c).Size(28, 28).Radius(7).Background(p.soft).Justify(ui.Center).AlignItems(ui.Center).Shrink(0).Children(func() {
			ui.Icon(c, icon).FontSize(14).TextColor(iconTone)
		})
		ui.Column(c).Gap(1).Grow(1).MinWidth(0).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, label).FontSize(12.5).FontWeight(500).TextColor(p.ink).SingleLine()
				switch {
				case permissionReady(access):
					settingsStatus(c, lucide("circle-check"), permissionStateText(w, access.State), p.success)
				case access.State == "unavailable":
					settingsStatus(c, nil, permissionStateText(w, access.State), p.faint)
				default:
					ui.Row(c).Height(18).Padding(0, 6).Radius(5).Background(p.warning.Alpha(0.12)).AlignItems(ui.Center).Shrink(0).Children(func() {
						ui.Text(c, permissionStateText(w, access.State)).FontSize(10.5).FontWeight(500).TextColor(p.warning).NoWrap()
					})
				}
			})
			detail := purpose
			if access.State == "unavailable" && access.Message != "" {
				detail = access.Message
			}
			ui.Text(c, detail).FontSize(11.5).TextColor(p.muted).Wrap()
		})
		if !permissionReady(access) && access.CanOpenSettings {
			open := quietButton(c, p, "settings:"+kind, w.tr("打开设置", "Settings")).Label(w.tr("打开 ", "Open ")+label+w.tr(" 设置", " settings")).Height(26).Padding(0, 8)
			if open.Disabled(!state.intent || state.busy || state.starting).Clicked() {
				w.openRemotePermissionSettings(kind)
			}
		}
		if !permissionReady(access) && access.CanRequest {
			allow := primaryAction(c, p, "request:"+kind, w.tr("允许", "Allow")).Label(w.tr("允许 ", "Allow ")+label).Height(26).Padding(0, 11)
			if allow.Disabled(!state.intent || state.busy || state.starting).Clicked() {
				w.requestRemotePermission(kind)
			}
		}
	})
}
