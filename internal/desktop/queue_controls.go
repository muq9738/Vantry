package desktop

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// Queue editing commands travel as plain kinds until pkg/api names them. An
// older Core answers with a rejection, which the control outbox reports and
// keeps retryable; the queued input itself is never touched locally.
const (
	commandUpdateInput api.CommandKind = "input.update"
	commandMoveInput   api.CommandKind = "input.move"
)

type queueEditing struct {
	editing  map[string]string // input ID -> text being edited
	composer *queueComposerEdit
}

func (q *queueEditing) begin(input api.Input) {
	if q.editing == nil {
		q.editing = map[string]string{}
	}
	q.editing[input.ID] = input.Message
}

func (q *queueEditing) end(id string) { delete(q.editing, id) }

func editableInput(input api.Input) bool { return input.Kind == "queue" && input.Status == "queued" }

func (w *workbench) saveInputEdit(input api.Input) bool {
	text, ok := w.queue.editing[input.ID]
	text = strings.TrimSpace(text)
	if !ok || !editableInput(input) {
		return false
	}
	if text == "" {
		w.actionNotice = notice{Text: "排队消息不能为空；如需移除请使用取消。", Level: noticeWarn}
		return false
	}
	if text == strings.TrimSpace(input.Message) {
		w.queue.end(input.ID)
		return false
	}
	delete(w.controls, "edit:"+input.ID) // a new text is a new command, never a retry of the old one
	if !w.dispatchControl("edit:"+input.ID, "修改排队消息", "排队消息已更新，等待状态同步。", api.Command{
		Kind: commandUpdateInput, ThreadID: input.ThreadID, InputID: input.ID, Message: text,
	}, false) {
		return false
	}
	w.queue.end(input.ID)
	return true
}

func (w *workbench) moveInput(input api.Input, direction string) bool {
	if !editableInput(input) || direction != "up" && direction != "down" {
		return false
	}
	key := "move:" + input.ID + ":" + direction
	delete(w.controls, key)
	return w.dispatchControl(key, "调整排队顺序", "", api.Command{
		Kind: commandMoveInput, ThreadID: input.ThreadID, InputID: input.ID, Value: direction,
	}, false)
}

// promoteInput uses Core's atomic promotion. The queue entry and the steer
// share one input identity; a race with completion cannot lose the message.
func (w *workbench) promoteInput(input api.Input) bool {
	if !editableInput(input) {
		return false
	}
	key := "promote:" + input.ID
	if w.controlBusy(key) {
		return false
	}
	return w.dispatchControl(key, "立即发送", "已立即发送，等待状态同步。", api.Command{
		Kind: api.PromoteInputToSteer, ThreadID: input.ThreadID, InputID: input.ID,
	}, false)
}

// queueCard is an inline pending message above the composer, with T3's
// send-now, edit-in-composer and remove actions. Reordering stays in its menu.
func (w *workbench) queueCard(c *ui.Context, p palette, input api.Input, index, count int) {
	_, editing := w.queue.editing[input.ID]
	ui.Column(c.Key("input:"+input.ID)).FillWidth().MaxWidth(580).Gap(5).Padding(8, 10).Radius(12).Background(p.raised).Border(1, p.lineSoft).Children(func() {
		preview := strings.TrimPrefix(input.Message, planModePrefix)
		ui.Text(c, preview).FontSize(12).TextColor(p.ink).Wrap().MaxHeight(40).Tooltip(preview)
		ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
			status := w.tr("等待下一轮", "Queued for the next turn")
			if input.Kind != "queue" {
				status = inputStatusLabel(input)
			}
			if editing {
				status = w.tr("编辑排队消息", "Editing in the composer")
			}
			ui.Text(c, status).FontSize(11).TextColor(p.faint).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
			if editing || !editableInput(input) {
				return
			}
			run, active := w.activeRun(input.ThreadID)
			sendNow := quietButton(c, p, "promote:"+input.ID, w.tr("立即发送", "Send now")).Height(22).FontSize(11).Padding(0, 6)
			sendNow.Description(w.tr("将这条排队消息追加到当前任务", "Send this queued message into the current turn"))
			sendNow.Disabled(!w.canSteer(run, active) || w.controlBusy("promote:"+input.ID))
			if sendNow.Clicked() {
				w.promoteInput(input)
			}
			edit := iconButton(c, p, "edit:"+input.ID, w.tr("编辑排队消息", "Edit queued message"), editIcon).Size(22, 22).Padding(4)
			if edit.Clicked() {
				w.beginQueueComposerEdit(input)
			}
			remove := iconButton(c, p, "cancel:"+input.ID, w.tr("移除排队消息", "Remove queued message"), closeIcon).Size(22, 22).Padding(4)
			remove.Disabled(w.controlBusy("cancel:" + input.ID))
			if remove.Clicked() {
				w.cancelInput(input)
			}
			more := iconButton(c, p, "queue-more:"+input.ID, w.tr("排队消息操作", "Queued message actions"), moreIcon).Size(22, 22).Padding(4)
			more.PopoverMenuButton(func(m *ui.Menu) {
				if m.Item(w.tr("上移", "Move up")).Disabled(index == 0).Chosen() {
					w.moveInput(input, "up")
				}
				if m.Item(w.tr("下移", "Move down")).Disabled(index == count-1).Chosen() {
					w.moveInput(input, "down")
				}
			}, ui.PopoverMenuButtonOptions{At: ui.AnchorTopEnd, Self: ui.AnchorBottomEnd})
			if w.controlRetry("edit:"+input.ID) && quietButton(c, p, "retry-edit:"+input.ID, w.tr("重试更新", "Retry update")).Height(22).Clicked() {
				w.dispatchControl("edit:"+input.ID, "更新排队消息", "排队消息已更新。", api.Command{}, false)
			}
		})
	})
}
