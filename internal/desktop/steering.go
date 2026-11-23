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
		return "正在追加…"
	case api.SteeringSending:
		return "正在追加…"
	case api.SteeringAccepted:
		return "已追加到本轮"
	case api.SteeringRejected:
		return "追加未送达"
	case api.SteeringUnknown:
		return "交付状态未知 · 不会自动重发"
	default:
		return fmt.Sprint(message.Status)
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
		isErr := steeringStatusLevel(message) == noticeError
		ui.Column(c.Key("steering:"+message.ID)).FillWidth().Gap(3).Padding(4, 0, 2).AlignItems(ui.End).Children(func() {
			bubble := ui.Column(c).MaxWidth(min(float32(540), readingWidth*0.78)).Padding(8, 13).Gap(4).Radius(16).Background(p.user).Border(1, p.lineSoft)
			if isErr {
				bubble.BorderColor(p.danger.Alpha(0.4))
			}
			bubble.Children(func() {
				ui.Text(c, strings.TrimSpace(message.Message)).FontSize(13.5).LineHeight(1.45).TextColor(p.ink).Wrap().Selectable()
				for _, attachment := range message.Attachments {
					ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, fileIcon).FontSize(11).TextColor(p.faint)
						ui.Text(c, attachment.Name).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
					})
				}
			})
			metaColor := p.faint
			if isErr {
				metaColor = p.danger
			}
			ui.Row(c).Gap(5).Padding(0, 4).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, steerIcon).FontSize(10.5).TextColor(metaColor)
				ui.Text(c, steeringStatusText(message)).FontSize(11).TextColor(metaColor).NoWrap()
				if message.Error != "" {
					ui.Text(c, "· "+message.Error).FontSize(11).TextColor(w.noticeColor(p, steeringStatusLevel(message))).SingleLine().Ellipsis("…").MaxWidth(360)
				}
			})
		})
	}
	pending := w.composerFor(run.ThreadID).steering
	if pending == nil || pending.command.RunID != run.ID {
		return
	}
	ui.Column(c.Key("steering-outbox:"+pending.command.CommandID)).FillWidth().Gap(3).Padding(4, 0, 2).AlignItems(ui.End).Children(func() {
		bubble := ui.Column(c).MaxWidth(min(float32(540), readingWidth*0.78)).Padding(8, 13).Gap(4).Radius(16).Background(p.user).Border(1, p.lineSoft)
		bubble.Children(func() {
			ui.Text(c, strings.TrimSpace(pending.command.Message)).FontSize(13.5).LineHeight(1.45).TextColor(p.ink).Wrap().Selectable()
			for _, attachment := range pending.command.Attachments {
				ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, fileIcon).FontSize(11).TextColor(p.faint)
					ui.Text(c, attachment.Name).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
				})
			}
		})
		ui.Row(c).Gap(6).Padding(0, 4).AlignItems(ui.Center).Children(func() {
			w.noticeLine(c, p, pending.feedback)
			if !pending.accepted && !pending.inFlight {
				retry := quietButton(c, p, "retry-steer", "重试").Height(22).Padding(0, 8).FontSize(11)
				retry.Description("沿用原 commandId、原始文本和附件；不使用之后编辑的草稿")
				if retry.Clicked() {
					w.retrySteering(run.ThreadID)
				}
			}
		})
	})
}
