package desktop

import (
	"encoding/json"
	"errors"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

func (h *environmentHub) configure(request api.WorkbenchRequest, removed string) {
	h.root.workbenchCall("environment-connections", request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			var typed *api.Error
			if errors.As(err, &typed) {
				h.failure = typed.Message
			} else {
				h.failure = explain(err)
			}
			return
		}
		var status remoteaccess.Status
		if json.Unmarshal([]byte(response.Output), &status) != nil {
			h.failure = "机器没有返回连接状态。"
			return
		}
		h.status = status
		h.failure = ""
		h.pairing = ""
		if removed != "" {
			for i, record := range h.records {
				if record.connectionID != removed {
					continue
				}
				if record.view != nil {
					record.view.saveState()
					record.view.closeTerminals()
				}
				if h.current == record.view {
					h.current = h.root
				}
				if record.cancel != nil {
					record.cancel()
				}
				if record.close != nil {
					_ = record.close()
				}
				h.records = append(h.records[:i:i], h.records[i+1:]...)
				if h.filter == removed {
					h.filter = ""
				}
				break
			}
		}
		h.load()
	})
}

func (w *workbench) environmentConnectionsDialog(c *ui.Context, p palette) {
	h := w.environments
	if h == nil || !h.manage {
		return
	}
	w.dialogFrame(c, p, &h.manage, "environment-connections", w.tr("机器连接", "Machine connections"), w.tr("关闭机器连接", "Close machine connections"), serverIcon, 620, func(float32) {
		ui.Text(c, w.tr("连接另一台电脑上的 Vantry，在同一个窗口选择机器、项目并发起 Agent 会话。", "Connect to Vantry on another computer, then choose its machine and project in this window.")).FontSize(12).TextColor(p.muted).Wrap()
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			input := ui.TextInput(c.Key("ide-pairing-link"), &h.pairing).Label(w.tr("机器配对链接", "Machine pairing link")).Placeholder("vantry://connect…").Password().Grow(1).MinWidth(0).Height(32).Padding(0, 10)
			add := ui.PrimaryButton(c.Key("connect-machine"), w.tr("添加机器", "Add machine")).Height(32).Padding(0, 12).Disabled(h.pairing == "" || h.root.features.busy["environment-connections"])
			if add.Clicked() || input.Submitted() && h.pairing != "" {
				h.configure(api.WorkbenchRequest{Action: "access.pair", Message: h.pairing}, "")
			}
		})
		ui.Column(c.Key("ide-environments")).Gap(4).Children(func() {
			for i, record := range h.records {
				i, record := i, record
				ui.Row(c.Key("ide-environment:" + record.key)).Height(42).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, serverIcon).FontSize(14).TextColor(p.muted)
					ui.Text(c, record.label).FontSize(12).TextColor(p.ink).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
					label := w.tr("连接", "Connect")
					if record.view != nil {
						label = w.tr("打开", "Open")
					}
					if quietButton(c, p, "open-environment", label).Label(w.tr("打开机器 ", "Open machine ") + record.label).Height(28).Disabled(record.connecting).Clicked() {
						h.choose(w, i, false)
						h.manage = false
					}
					if record.connectionID != "" {
						if iconButton(c, p, "remove-environment", w.tr("移除机器 ", "Remove machine ")+record.label, closeIcon).Size(26, 26).Padding(6).Disabled(record.connecting).Clicked() {
							h.configure(api.WorkbenchRequest{Action: "access.remove", Name: record.connectionID}, record.connectionID)
						}
					}
				})
			}
		})
		ui.Box(c).FillWidth().Height(1).Background(p.lineSoft)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Gap(4).Grow(1).Children(func() {
				ui.Text(c, w.tr("允许其他 Vantry 连接本机 IDE", "Allow other Vantry clients")).FontSize(12).TextColor(p.ink)
				ui.Text(c, h.root.hostLabel()).FontSize(10).TextColor(p.faint)
			})
			on := h.status.Enabled
			ui.Switch(c.Key("ide-exposure"), &on).Label(w.tr("允许机器连接", "Allow machine connections")).Disabled(h.root.features.busy["environment-connections"])
			if on != h.status.Enabled {
				if on {
					input, _ := json.Marshal(map[string]string{"name": h.root.hostLabel(), "relay": h.status.Relay})
					h.configure(api.WorkbenchRequest{Action: "access.start", Input: input}, "")
				} else {
					h.configure(api.WorkbenchRequest{Action: "access.stop"}, "")
				}
			}
		})
		if h.status.Enabled {
			if quietButton(c, p, "copy-ide-pairing", w.tr("复制本机配对链接", "Copy this machine's pairing link")).Height(28).Clicked() {
				c.WriteClipboard(h.status.PairingLink)
			}
		}
		if h.failure != "" {
			ui.Text(c, h.failure).FontSize(11).TextColor(p.warning).Wrap()
		}
	})
}
