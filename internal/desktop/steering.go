package desktop

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func (w *workbench) foldSteeringEvent(event api.Event) bool {
	var payload struct {
		Steering *api.SteeringMessage `json:"steering"`
	}
	if json.Unmarshal(event.Data, &payload) != nil || payload.Steering == nil {
		return false
	}
	message := *payload.Steering
	if message.ID == "" || message.CommandID == "" || message.ThreadID == "" || message.RunID == "" {
		return false
	}
	w.mergeSteering(message)
	return true
}

// steerActiveRun freezes an adjustment for the currently running attempt. It
// neither creates a run nor consumes the independent next-message queue.
func (w *workbench) steerActiveRun() bool {
	target := w.selected
	run, active := w.activeRun(target)
	comp := w.composerFor(target)
	if !active || run.Status != api.RunRunning || comp.steering != nil || comp.uploading {
		return false
	}
	text := strings.TrimSpace(comp.outgoing())
	if text == "" && len(comp.attachments) > 0 {
		text = "请查看附件。"
	}
	if text == "" {
		return false
	}
	pending := &steeringSubmission{
		command: api.Command{
			Version: api.ProtocolVersion, CommandID: newCommandID("steer"), Kind: api.SteerMessage,
			ThreadID: target, RunID: run.ID, Message: text,
			Attachments: append([]api.Attachment(nil), comp.attachments...),
		},
		startedAt: time.Now(),
	}
	comp.steering = pending
	comp.draft, comp.attachments, comp.mentions = "", nil, nil
	w.dispatchSteering(target, pending)
	return true
}

func (w *workbench) dispatchSteering(threadID string, pending *steeringSubmission) {
	if pending == nil || pending.inFlight || pending.accepted {
		return
	}
	command := pending.command
	if w.inFlightCommands[command.CommandID] {
		return
	}
	pending.inFlight = true
	pending.feedback = notice{Text: "正在提交调整…", Level: noticeInfo}
	w.inFlightCommands[command.CommandID] = true
	w.dispatch(command, func(receipt api.Receipt, err error) {
		w.post(func() { w.afterSteer(threadID, pending, command, receipt, err) })
	})
}

func (w *workbench) afterSteer(threadID string, pending *steeringSubmission, command api.Command, receipt api.Receipt, err error) {
	delete(w.inFlightCommands, command.CommandID)
	pending.inFlight = false
	comp := w.composerFor(threadID)
	if comp.steering != pending {
		return
	}
	if err != nil {
		pending.feedback = notice{Text: describe("调整结果待确认", err), Level: levelFor(err)}
		w.connNotice = notice{Text: describe("本地服务连接", err), Level: levelFor(err)}
		return
	}
	if !receipt.Accepted {
		pending.feedback = notice{Text: "本地服务未接受调整，可重试原请求；后续队列不受影响。", Level: noticeError}
		return
	}
	pending.accepted = true
	pending.messageID = receipt.MessageID
	pending.feedback = notice{Text: "本地服务已接纳调整，等待实际交付状态。", Level: noticeInfo}
	w.connNotice = w.connectedNotice()
	w.reconcileSteering()
}

// retrySteering is available only while the receipt itself is unconfirmed.
// Once Core has accepted, unknown delivery is read-only and never resent.
func (w *workbench) retrySteering(threadID string) bool {
	pending := w.composerFor(threadID).steering
	if pending == nil || pending.inFlight || pending.accepted {
		return false
	}
	w.dispatchSteering(threadID, pending)
	return true
}

func steeringStatusText(message api.SteeringMessage) string {
	switch message.Status {
	case api.SteeringPending:
		return "本地服务已接纳 · 等待发送"
	case api.SteeringSending:
		return "正在向 Pi 交付 · 不会自动重发"
	case api.SteeringAccepted:
		text := "Pi 已接纳"
		if message.Disposition != "" {
			text += "（" + message.Disposition + "）"
		}
		return text + " · 不代表模型已消费"
	case api.SteeringRejected:
		return "调整未送达"
	case api.SteeringUnknown:
		return "交付状态未知 · 不会自动重试或新建指令"
	default:
		return "交付状态 " + fmt.Sprint(message.Status)
	}
}

func steeringStatusLevel(message api.SteeringMessage) noticeLevel {
	switch message.Status {
	case api.SteeringRejected, api.SteeringUnknown:
		return noticeError
	default:
		return noticeInfo
	}
}

func (w *workbench) steeringView(c *ui.Context, p palette, run api.Run) {
	for _, message := range w.steering {
		if message.ThreadID != run.ThreadID || message.RunID != run.ID {
			continue
		}
		projected := false
		for _, input := range w.inputs {
			if input.ID == message.ID && input.Kind == "steer" {
				projected = true
				break
			}
		}
		if projected {
			continue
		}
		message := message
		ui.Column(c.Key("steering:"+message.ID)).Gap(3).Padding(7, 9).Background(p.soft).Radius(7).Children(func() {
			ui.Text(c, "调整当前任务 · "+message.Message).FontSize(11).TextColor(p.ink).Wrap().Selectable()
			for _, attachment := range message.Attachments {
				ui.Text(c, "附件 · "+attachment.Name).FontSize(11).TextColor(p.muted).Wrap()
			}
			w.noticeLine(c, p, notice{Text: steeringStatusText(message), Level: steeringStatusLevel(message)})
			if message.Error != "" {
				ui.Text(c, message.Error).FontSize(11).TextColor(w.noticeColor(p, steeringStatusLevel(message))).Wrap().Selectable()
			}
		})
	}
	pending := w.composerFor(run.ThreadID).steering
	if pending == nil || pending.command.RunID != run.ID {
		return
	}
	ui.Column(c.Key("steering-outbox:"+pending.command.CommandID)).Gap(4).Padding(7, 9).Background(p.soft).Radius(7).Children(func() {
		ui.Text(c, "调整当前任务 · "+pending.command.Message).FontSize(11).TextColor(p.ink).Wrap().Selectable()
		for _, attachment := range pending.command.Attachments {
			ui.Text(c, "附件 · "+attachment.Name).FontSize(11).TextColor(p.muted).Wrap()
		}
		w.noticeLine(c, p, pending.feedback)
		if !pending.accepted && !pending.inFlight {
			retry := ui.Button(c, "重试原调整")
			retry.Description("沿用原 commandId、原始文本和附件；不使用之后编辑的草稿")
			if retry.Clicked() {
				w.retrySteering(run.ThreadID)
			}
		}
	})
}
