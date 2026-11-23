package desktop

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/skip2/go-qrcode"
)

type pairingQRState struct {
	link   string
	bitmap *ui.Bitmap
}

func pairingQR(c *ui.Context, state *pairingQRState, link string) {
	if link == "" {
		return
	}
	if state.link != link {
		code, err := qrcode.New(link, qrcode.Medium)
		if err != nil {
			return
		}
		frame := code.Image(200)
		if state.bitmap == nil {
			state.bitmap = ui.NewBitmap(frame)
		} else {
			state.bitmap.Update(frame)
		}
		state.link = link
	}
	if state.bitmap != nil {
		ui.Image(c, state.bitmap).Size(168, 168)
	}
}
func (w *workbench) sessionPairing(c *ui.Context, p palette) {
	status := w.access.status
	if !status.Enabled || status.PairingCode == "" {
		return
	}
	remaining := time.Until(status.PairingExpiresAt)
	link, code, address := status.PairingLink, status.PairingCode, nonEmptyString(status.Relay, w.tr("局域网", "LAN"))
	mobile := status.T3PairingLink != ""
	if mobile {
		link = status.T3PairingLink
		var valid bool
		code, address, valid = mobilePairingPresentation(link)
		if !valid {
			ui.Text(c, w.tr("手机连接地址不可用。请在设备设置填写手机可达的 HTTPS 地址，再生成配对码。", "Set a reachable HTTPS mobile address in device settings, then generate a new pairing code.")).FontSize(12).TextColor(p.warning).Wrap()
			return
		}
	}
	ui.Column(c.Key("session-pairing")).Gap(10).Padding(16).Radius(10).Background(p.soft).Children(func() {
		ui.Text(c, w.tr("电脑配对码", "Computer pairing code")).FontSize(13).Bold().TextColor(p.ink)
		if status.PairingState == "paired" {
			ui.Text(c, w.tr("已完成配对，此码已兑换。", "Paired. This code has been redeemed.")+status.PairedWith).FontSize(12).TextColor(p.success)
			return
		}
		if remaining <= 0 {
			ui.Text(c, w.tr("配对码已过期，请重新生成。", "This pairing code expired. Generate another code.")).FontSize(12).TextColor(p.warning)
			return
		}
		c.After(time.Second)
		ui.Text(c, code).Font("Menlo").FontSize(20).TextColor(p.ink).Selectable()
		ui.Text(c, fmt.Sprintf(w.tr("%s · %d 分 %02d 秒后过期 · 仅可兑换一次", "%s · expires in %d:%02d · single use"), address, int(remaining.Minutes()), int(remaining.Seconds())%60)).FontSize(11).TextColor(p.muted).Wrap()
		ui.Row(c).Gap(8).Children(func() {
			if outlineButton(c, p, "copy-session-code", w.tr("复制配对码", "Copy code")).Clicked() {
				c.WriteClipboard(code)
			}
			if outlineButton(c, p, "copy-session-link", w.tr("复制配对链接", "Copy link")).Clicked() {
				c.WriteClipboard(link)
			}
			if quietButton(c, p, "revoke-session-code", w.tr("撤销此码", "Revoke code")).Clicked() {
				w.accessCall(api.WorkbenchRequest{Action: "access.code.revoke"})
			}
		})
		if mobile {
			ui.Text(c, w.tr("用 Vantry 手机端扫描下方二维码，一次配对即可访问会话与远程桌面。", "Scan with Vantry on your phone. Pair once for conversations and remote desktop.")).FontSize(12).TextColor(p.muted).Wrap()
			if quietButton(c, p, "copy-native-session-link", w.tr("复制电脑端连接链接", "Copy desktop client link")).Clicked() {
				c.WriteClipboard(status.PairingLink)
			}
		}
		pairingQR(c, &w.access.qr, link)
		ui.Text(c, w.tr("已配对手机可访问会话，并在本机开启桌面共享后查看画面与控制键鼠。可随时撤销设备授权。", "Paired phones can use conversations and, while desktop sharing is on, view and control this computer. Revoke a device at any time.")).FontSize(11).TextColor(p.muted).Wrap()
	})
}

func mobilePairingPresentation(link string) (code, address string, valid bool) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return "", "", false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip := net.ParseIP(host); host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		return "", "", false
	}
	fragment, err := url.ParseQuery(u.Fragment)
	if err != nil || fragment.Get("token") == "" {
		return "", "", false
	}
	return fragment.Get("token"), u.Scheme + "://" + u.Host, true
}
func (w *workbench) sessionClients(c *ui.Context, p palette) {
	status := w.access.status
	for _, client := range status.Clients {
		client := client
		settingsRow(c, p, "access-client-"+client.ID, false, client.Name, client.CreatedAt.Local().Format("2006-01-02 15:04"), func() {
			if quietButton(c, p, "revoke", w.tr("撤销设备", "Revoke device")).Clicked() {
				w.accessCall(api.WorkbenchRequest{Action: "access.client.revoke", Name: client.ID})
			}
		})
	}
}
func (w *workbench) desktopPairing(c *ui.Context, p palette, host *RemoteDesktopHostStatus) {
	remaining := time.Until(host.ShareExpiresAt)
	ui.Column(c.Key("desktop-pairing")).Gap(12).Padding(14, 16).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
		ui.Text(c, w.tr("已配对的手机可直接打开此桌面，无需再次扫码或确认。", "Paired phones can open this desktop without another code or approval.")).FontSize(13).TextColor(p.ink).Wrap()
		if host.PendingViewer != "" {
			ui.Column(c).Gap(8).Padding(10, 12).Radius(10).Background(p.warning.Alpha(0.1)).Border(1, p.warning.Alpha(0.35)).Children(func() {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, shieldIcon).FontSize(14).TextColor(p.warning)
					ui.Text(c, host.PendingViewer+w.tr(" 请求观看画面并控制键鼠", " requests screen viewing & input control")).FontSize(12.5).FontWeight(600).TextColor(p.ink).Wrap().Grow(1)
				})
				ui.Row(c).Gap(8).Children(func() {
					allow := primaryAction(c, p, "rd.approve", w.tr("允许本次连接", "Allow this connection")).Height(28).Padding(0, 12)
					if allow.Clicked() {
						w.hostCall("rd.approve", func(status *RemoteDesktopHostStatus, err error) {
							if err != nil {
								w.rd.hostErr = err.Error()
							} else {
								w.rd.host = status
							}
						})
					}
					if outlineButton(c, p, "rd.reject", w.tr("拒绝", "Decline")).Height(28).Clicked() {
						w.hostCall("rd.reject", func(status *RemoteDesktopHostStatus, err error) {
							if err != nil {
								w.rd.hostErr = err.Error()
							} else {
								w.rd.host = status
							}
						})
					}
				})
			})
		}
		if quietButton(c, p, "toggle-guest-desktop-code", w.tr("临时分享给未配对电脑", "Share with an unpaired computer")).Clicked() {
			w.rd.showGuestCode = !w.rd.showGuestCode
			if w.rd.showGuestCode && host.ShareCode == "" && host.RelaySSHHost != "" {
				w.hostCall("rd.code", func(status *RemoteDesktopHostStatus, err error) {
					if err != nil {
						w.rd.hostErr = err.Error()
					} else {
						w.rd.host = status
					}
				})
			}
		}
		if !w.rd.showGuestCode {
			return
		}
		ui.Row(c).Gap(14).AlignItems(ui.Center).Wrap().Children(func() {
			ui.Column(c).Gap(6).Grow(1).MinWidth(180).Children(func() {
				ui.Text(c, w.tr("一次性桌面共享码", "Single-use sharing code")).FontSize(12).FontWeight(600).TextColor(p.muted)
				if remaining > 0 {
					c.After(time.Second)
					ui.Text(c, host.ShareCode).Font("Menlo").FontSize(20).FontWeight(600).TextColor(p.ink).Selectable()
					ui.Text(c, fmt.Sprintf(w.tr("%d 分 %02d 秒后过期 · 仅限单次兑换", "Expires in %d:%02d · single use"), int(remaining.Minutes()), int(remaining.Seconds())%60)).FontSize(11.5).TextColor(p.faint)
				} else {
					ui.Text(c, w.tr("共享码已过期；已连接的设备不受影响。", "Code expired; active connections continue.")).FontSize(12).TextColor(p.warning)
				}
				ui.Row(c).Gap(8).Padding(2, 0, 0).Wrap().Children(func() {
					if remaining > 0 {
						if outlineButton(c, p, "copy-desktop-code", w.tr("复制共享码", "Copy code")).Height(28).Clicked() {
							c.WriteClipboard(host.ShareCode)
						}
					}
					if quietButton(c, p, "new-desktop-code", w.tr("重新生成共享码", "New code")).Height(28).Clicked() {
						w.hostCall("rd.code", func(status *RemoteDesktopHostStatus, err error) {
							if err != nil {
								w.rd.hostErr = err.Error()
							} else {
								w.rd.host = status
							}
						})
					}
				})
			})
			if remaining > 0 && host.ConnectURL != "" {
				ui.Column(c).Padding(6).Radius(10).Background(ui.Hex("#FFFFFF")).Border(1, p.lineSoft).Shrink(0).Children(func() {
					pairingQR(c, &w.rd.qr, host.ConnectURL)
				})
			}
		})
	})
}
