package desktop

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

// controlAction freezes a control command until its receipt arrives. Accepted
// request/cancel actions stay locked until their committed projection changes;
// queue and steer can be deliberately issued again after the receipt.
type controlAction struct {
	command  api.Command
	inFlight bool
	accepted bool
	retain   bool
}

func (w *workbench) controlBusy(key string) bool {
	a := w.controls[key]
	return a != nil && (a.inFlight || a.accepted)
}

func (w *workbench) controlRetry(key string) bool {
	a := w.controls[key]
	return a != nil && !a.inFlight && !a.accepted
}

// dispatchControl never substitutes the current draft or form value on retry.
// A receipt acknowledges only Core's command, not Pi delivery or request
// completion; those states arrive through input.updated/request.updated.
func (w *workbench) dispatchControl(key, actionText, successText string, command api.Command, retain bool) bool {
	if w.controlBusy(key) {
		return false
	}
	a := w.controls[key]
	if a == nil {
		command.Version = api.ProtocolVersion
		command.CommandID = newCommandID("control")
		a = &controlAction{command: command, retain: retain}
		w.controls[key] = a
	}
	command = a.command
	if w.inFlightCommands[command.CommandID] {
		return false
	}
	a.inFlight = true
	w.inFlightCommands[command.CommandID] = true
	w.actionNotice = notice{}
	w.dispatch(command, func(receipt api.Receipt, err error) {
		w.post(func() {
			defer delete(w.inFlightCommands, command.CommandID)
			if w.controls[key] != a {
				return
			}
			a.inFlight = false
			if err != nil {
				w.actionNotice = notice{Text: describe(actionText+"结果待确认，可重试原操作", err), Level: levelFor(err)}
				return
			}
			if !receipt.Accepted {
				w.actionNotice = notice{Text: actionText + "未被本地服务接受，可重试原操作。", Level: noticeError}
				return
			}
			a.accepted = true
			if successText != "" {
				w.actionNotice = notice{Text: successText, Level: noticeInfo}
			}
			if !a.retain {
				delete(w.controls, key)
			}
		})
	})
	return true
}

func (w *workbench) queueDraft(threadID string, steer bool) bool {
	run, active := w.activeRun(threadID)
	if !active || isLocalSession(threadID) {
		return false
	}
	kind, key, label := api.QueueMessage, "queue:"+threadID, "排队"
	success := "已提交排队，等待状态更新。"
	if steer {
		if !w.controlCapable(threadID) || run.Status != api.RunRunning || run.AttemptID == "" {
			return false
		}
		kind, key, label = api.SteerRun, "steer:"+threadID, "调整"
		success = "调整命令已提交，等待 Pi 接收状态。"
	}
	// Sending the current draft is distinct from retrying an older frozen
	// command. Keep that command addressable only by its explicit retry UI.
	comp := w.composerFor(threadID)
	if w.controlRetry(key) && (strings.TrimSpace(comp.draft) != "" || len(comp.attachments) > 0) {
		prior := w.controls[key]
		w.controls["retry:"+prior.command.CommandID] = prior
		delete(w.controls, key)
	}
	if !w.controlRetry(key) {
		if !w.modelReadyToSend(threadID) {
			return false
		}
		comp := w.composerFor(threadID)
		if comp.pending != nil || comp.uploading || steer && comp.steering != nil {
			return false
		}
		message := strings.TrimSpace(comp.outgoing())
		if message == "" && len(comp.attachments) > 0 {
			message = "请查看附件。"
		}
		if message == "" {
			return false
		}
		if !steer {
			message = applyInteractionMode(comp.plan, message)
		}
		command := api.Command{Kind: kind, ThreadID: threadID, Message: message, Attachments: append([]api.Attachment(nil), comp.attachments...)}
		if steer {
			command.RunID = run.ID
			command.ParentAttemptID = run.AttemptID
		}
		if !w.dispatchControl(key, label, success, command, false) {
			return false
		}
		comp.draft = ""
		comp.attachments = nil
		comp.mentions = nil
		comp.feedback = notice{}
		w.saveState()
		return true
	}
	return w.dispatchControl(key, label, success, api.Command{}, false)
}

func (w *workbench) retryKeys(threadID string) []string {
	var keys []string
	for key, action := range w.controls {
		if action.command.ThreadID == threadID && (action.command.Kind == api.QueueMessage || action.command.Kind == api.SteerRun) && w.controlRetry(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func (w *workbench) threadProvider(threadID string) string {
	if comp := w.composers[threadID]; comp != nil && comp.provider != nil && *comp.provider != "" {
		return *comp.provider
	}
	if thread, ok := w.threadByID(threadID); ok && thread.ProviderID != "" {
		return thread.ProviderID
	}
	if w.defaults.ProviderID != "" {
		return w.defaults.ProviderID
	}
	return "pi"
}

func (w *workbench) piThread(threadID string) bool {
	id := w.threadProvider(threadID)
	return id == "" || id == "pi"
}

func (w *workbench) controlCapable(threadID string) bool {
	return w.piThread(threadID)
}

// modelSwitchable reports whether the composer may choose a model for the
// thread. Legacy demo or retired non-Pi threads join Pi when the user picks a
// Pi model, so the picker opens whenever new threads use Pi.
func (w *workbench) modelSwitchable(threadID string) bool {
	if w.piThread(threadID) {
		return true
	}
	thread, ok := w.threadByID(threadID)
	return ok && thread.ProviderID != "pi" && (w.defaults.ProviderID == "" || w.defaults.ProviderID == "pi")
}

func (w *workbench) cancelInput(input api.Input) bool {
	if input.Kind != "queue" || input.Status != "queued" {
		return false
	}
	return w.dispatchControl("cancel:"+input.ID, "取消排队", "", api.Command{
		Kind: api.CancelInput, ThreadID: input.ThreadID, InputID: input.ID,
	}, true)
}

func (w *workbench) respondRequest(request api.ProviderRequest, value string, confirmed, cancelled bool) bool {
	if request.Status != "pending" {
		return false
	}
	if request.Method == "select" && !cancelled && !containsString(request.Options, value) {
		w.actionNotice = notice{Text: "请选择请求提供的选项。", Level: noticeWarn}
		return false
	}
	return w.dispatchControl("request:"+request.ID, "请求响应", "响应命令已提交，等待请求状态更新。", api.Command{
		Kind: api.RespondRequest, ThreadID: request.ThreadID, RunID: request.RunID,
		RequestID: request.ID, Value: value, Confirmed: confirmed, Cancelled: cancelled,
	}, true)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (w *workbench) foldInput(event api.Event) bool {
	var payload struct {
		Input *api.Input `json:"input"`
	}
	if json.Unmarshal(event.Data, &payload) != nil || payload.Input == nil {
		return false
	}
	input := *payload.Input
	if input.ThreadID == "" {
		input.ThreadID = event.ThreadID
	}
	if input.ID == "" || input.ThreadID == "" || !w.hasThread(input.ThreadID) {
		return false
	}
	for i := range w.inputs {
		if w.inputs[i].ID == input.ID {
			w.inputs[i] = input
			w.afterInput(input)
			return true
		}
	}
	w.inputs = append(w.inputs, input)
	sort.SliceStable(w.inputs, func(i, j int) bool { return w.inputs[i].CreatedAt.Before(w.inputs[j].CreatedAt) })
	w.afterInput(input)
	return true
}

func (w *workbench) afterInput(input api.Input) {
	if input.Kind != "queue" || input.Status != "queued" {
		delete(w.controls, "cancel:"+input.ID)
	}
}

func (w *workbench) foldTool(event api.Event) bool {
	var payload struct {
		Tool *api.ToolCall `json:"tool"`
	}
	if json.Unmarshal(event.Data, &payload) != nil || payload.Tool == nil {
		return false
	}
	tool := *payload.Tool
	if tool.ThreadID == "" {
		tool.ThreadID = event.ThreadID
	}
	if tool.RunID == "" {
		tool.RunID = event.RunID
	}
	if tool.ID == "" || tool.ThreadID == "" || tool.RunID == "" || !w.hasRun(tool.RunID) {
		return false
	}
	for i := range w.tools {
		if w.tools[i].ID == tool.ID {
			w.tools[i] = tool
			return true
		}
	}
	w.tools = append(w.tools, tool)
	sort.SliceStable(w.tools, func(i, j int) bool { return w.tools[i].UpdatedAt.Before(w.tools[j].UpdatedAt) })
	return true
}

func (w *workbench) foldRequest(event api.Event) bool {
	var payload struct {
		Request *api.ProviderRequest `json:"request"`
	}
	if json.Unmarshal(event.Data, &payload) != nil || payload.Request == nil {
		return false
	}
	request := *payload.Request
	if request.ThreadID == "" {
		request.ThreadID = event.ThreadID
	}
	if request.RunID == "" {
		request.RunID = event.RunID
	}
	if request.ID == "" || request.ThreadID == "" || request.RunID == "" || !w.hasRun(request.RunID) {
		return false
	}
	for i := range w.requests {
		if w.requests[i].ID == request.ID {
			w.requests[i] = request
			w.afterRequest(request)
			return true
		}
	}
	w.requests = append(w.requests, request)
	sort.SliceStable(w.requests, func(i, j int) bool { return w.requests[i].CreatedAt.Before(w.requests[j].CreatedAt) })
	w.afterRequest(request)
	return true
}

func (w *workbench) afterRequest(request api.ProviderRequest) {
	if request.Status != "pending" {
		delete(w.controls, "request:"+request.ID)
	}
}

func inputStatusLabel(input api.Input) string {
	switch input.Status {
	case "queued":
		return "排队中"
	case "pending":
		return "正在追加…"
	case "started":
		return "已开始"
	case "delivered":
		return "已追加到本轮"
	case "unknown":
		return "交付状态未知 · 不会自动重发"
	case "cancelled":
		return "已取消"
	case "failed":
		return "发送失败"
	default:
		return input.Status
	}
}

func toolStatusLabel(status string) string {
	switch status {
	case "running":
		return "运行中"
	case "completed":
		return "已完成"
	case "failed":
		return "失败"
	case "interrupted":
		return "已中断"
	default:
		return status
	}
}

func requestStatusLabel(status string) string {
	switch status {
	case "pending":
		return "等待响应"
	case "responding":
		return "正在提交响应"
	case "responded":
		return "已响应"
	case "expired":
		return "已过期"
	case "failed":
		return "响应失败"
	default:
		return status
	}
}
