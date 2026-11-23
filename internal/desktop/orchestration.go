package desktop

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func cloneWorkspace(spec *api.WorkspaceSpec) *api.WorkspaceSpec {
	if spec == nil {
		return nil
	}
	copy := *spec
	return &copy
}

func (w *workbench) foldOrchestration(event api.Event) bool {
	var payload struct {
		Task   *api.Task   `json:"task"`
		Cohort *api.Cohort `json:"cohort"`
	}
	if json.Unmarshal(event.Data, &payload) != nil {
		return false
	}
	if payload.Task != nil && payload.Task.ID != "" {
		for i, task := range w.tasks {
			if task.ID == payload.Task.ID {
				if payload.Task.Generation >= task.Generation {
					w.tasks[i] = *payload.Task
				}
				return true
			}
		}
		w.tasks = append(w.tasks, *payload.Task)
		return true
	}
	if payload.Cohort != nil && payload.Cohort.ParentRunID != "" {
		for i, cohort := range w.cohorts {
			if cohort.ParentRunID == payload.Cohort.ParentRunID && cohort.Generation == payload.Cohort.Generation {
				w.cohorts[i] = *payload.Cohort
				return true
			}
		}
		w.cohorts = append(w.cohorts, *payload.Cohort)
		return true
	}
	// Activity events advance the subscription cursor even when they have no
	// assistant text. Their output, if any, arrives through run.output.
	return event.Type == "run.provider" || event.Type == "provider.event" || event.Type == "run.provider_event"
}

func (w *workbench) latestRun(threadID string) (api.Run, bool) {
	var latest api.Run
	found := false
	for _, run := range w.runs {
		if run.ThreadID == threadID && (!found || !run.CreatedAt.Before(latest.CreatedAt)) {
			latest, found = run, true
		}
	}
	return latest, found
}

func (w *workbench) delegateDraft(threadID string) bool {
	parent, ok := w.latestRun(threadID)
	if !ok || parent.Status == api.RunCancelled || parent.Status == api.RunFailed || parent.Status == api.RunInterrupted {
		return false
	}
	policy := "auto"
	if w.widgets.manualDelivery {
		policy = "manual"
	}
	return w.submitTaskDraft(threadID, api.Command{
		Version: api.ProtocolVersion, CommandID: newCommandID("delegate"), Kind: api.DelegateTask,
		ThreadID: threadID, ParentRunID: parent.ID, ParentAttemptID: parent.AttemptID,
		ResumePolicy: policy,
	})
}

func (w *workbench) followUpDraft(task api.Task) bool {
	return w.submitTaskDraft(w.selected, api.Command{
		Version: api.ProtocolVersion, CommandID: newCommandID("followup"), Kind: api.FollowUpTask,
		ThreadID: task.ParentThreadID, ParentRunID: task.ParentRunID,
		TaskID: task.ID, Generation: task.Generation,
	})
}

func (w *workbench) submitTaskDraft(threadID string, command api.Command) bool {
	composer := w.composerFor(threadID)
	message := strings.TrimSpace(composer.outgoing())
	if composer.pending != nil || message == "" {
		return false
	}
	command.Message, command.Title = message, titleFromInstruction(message)
	pending := &submission{send: command, threadID: threadID, startedAt: time.Now()}
	composer.pending, composer.draft = pending, ""
	composer.feedback = notice{Text: "正在派出子任务…", Level: noticeInfo}
	w.saveState()
	w.dispatchSend(threadID, pending)
	return true
}

func (w *workbench) taskAction(key string, command api.Command) {
	if prior, ok := w.taskActions[key]; ok {
		command = prior
	} else {
		command.Version, command.CommandID = api.ProtocolVersion, newCommandID("task-action")
		w.taskActions[key] = command
	}
	if w.inFlightCommands[command.CommandID] {
		return
	}
	w.inFlightCommands[command.CommandID] = true
	w.dispatch(command, func(receipt api.Receipt, err error) {
		w.post(func() {
			delete(w.inFlightCommands, command.CommandID)
			if err != nil {
				w.actionNotice = notice{Text: describe("任务操作未确认，可重试原操作", err), Level: levelFor(err)}
				return
			}
			if !receipt.Accepted {
				return
			}
			delete(w.taskActions, key)
			w.actionNotice = notice{}
		})
	})
}

func deliveryLabel(status api.DeliveryStatus) string {
	switch status {
	case api.DeliveryPending:
		return "等待结果"
	case api.DeliveryDelivered:
		return "结果已交付"
	case api.DeliveryAcknowledged:
		return "结果已确认"
	case api.DeliveryDisposed:
		return "结果已忽略"
	default:
		return string(status)
	}
}

// taskStatusTone is the dot color of a subtask or peer thread's status.
func taskStatusTone(p palette, status api.RunStatus) ui.Color {
	switch status {
	case api.RunRunning, api.RunQueued:
		return p.running
	case api.RunWaiting:
		return p.warning
	case api.RunCompleted:
		return p.success
	case api.RunFailed, api.RunInterrupted:
		return p.danger
	}
	return p.faint
}

// cancelTask stops a delegated subtask that is still running.
func (w *workbench) cancelTask(task api.Task) bool {
	if task.Status.Terminal() {
		return false
	}
	w.taskAction("cancel:"+task.ID, api.Command{Kind: api.CancelTask, ThreadID: task.ParentThreadID, TaskID: task.ID, ParentRunID: task.ParentRunID})
	return true
}

// agentCardHeader is the first line of a workbench subtask or peer card: the
// status dot, the title, the child's Pi model and its status.
func (w *workbench) agentCardHeader(c *ui.Context, p palette, title string, status api.RunStatus, childID string) {
	tone := taskStatusTone(p, status)
	if status == api.RunRunning {
		activityIndicator(c, tone).Size(9, 9).Shrink(0)
	} else {
		ui.Box(c).Size(7, 7).Radius(4).Background(tone).Shrink(0)
	}
	ui.Text(c, stripInlineMarkdownTitle(title)).FontSize(12.5).FontWeight(500).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…").MinWidth(0)
	if child, ok := w.threadByID(childID); ok && w.piThread(childID) {
		ui.Row(c).Gap(3).AlignItems(ui.Center).Shrink(1).MinWidth(0).MaxWidth(170).Children(func() {
			piMark(c, p.faint, 9.5)
			ui.Text(c, w.threadModelLabel(child)).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").MinWidth(0)
		})
	}
	ui.Text(c, w.tr(statusLabel(status), englishStatus(status))).FontSize(11).FontWeight(500).TextColor(tone).NoWrap().Shrink(0)
}

type subagentNavItem struct {
	threadID string
	title    string
	status   api.RunStatus
	task     *api.Task
}

func (w *workbench) subagentNavItems(rootID string) []subagentNavItem {
	if rootID == "" {
		return nil
	}
	var items []subagentNavItem
	seen := map[string]bool{}
	for i := range w.tasks {
		task := &w.tasks[i]
		if task.ParentThreadID != rootID || task.ChildThreadID == "" || seen[task.ChildThreadID] {
			continue
		}
		seen[task.ChildThreadID] = true
		items = append(items, subagentNavItem{
			threadID: task.ChildThreadID,
			title:    nonEmptyString(task.Title, w.displayTitleFor(task.ChildThreadID)),
			status:   task.Status,
			task:     task,
		})
	}
	for _, th := range w.threads {
		if th.ID == rootID || seen[th.ID] {
			continue
		}
		if th.ParentThreadID == rootID || th.CreatedByThreadID == rootID {
			seen[th.ID] = true
			st := api.RunStatus("")
			if latest, ok := w.latestRun(th.ID); ok {
				st = latest.Status
			}
			items = append(items, subagentNavItem{
				threadID: th.ID,
				title:    w.displayTitleFor(th.ID),
				status:   st,
			})
		}
	}
	return items
}

// subagentRelationshipsControl is T3 Code's ThreadRelationshipsControl in the
// chat header: a compact pill listing the thread's subagents (or sibling
// subagents when viewing a child) so switching requires one click without
// cluttering the left sidebar.
func (w *workbench) subagentRelationshipsControl(c *ui.Context, p palette) {
	if w.selected == "" {
		return
	}
	rootID := w.selected
	if th, ok := w.threadByID(w.selected); ok && th.ParentThreadID != "" {
		rootID = th.ParentThreadID
	}
	items := w.subagentNavItems(rootID)
	if len(items) == 0 {
		return
	}
	live := 0
	for _, item := range items {
		if item.status == api.RunRunning || item.status == api.RunQueued || item.status == api.RunWaiting {
			live++
		}
	}
	badgeText := fmt.Sprintf(w.tr("%d 个子代理", "%d agents"), len(items))
	tone := p.muted
	if live > 0 {
		badgeText = fmt.Sprintf(w.tr("%d/%d 运行中", "%d/%d working"), live, len(items))
		tone = p.running
	}
	btn := ui.Button(c.Key("header-subagents"), "").Label(w.tr("子代理列表", "Subagents")).Height(24).Padding(0, 8).Radius(12).Shrink(0).
		Border(1, p.lineSoft).Background(p.soft).TextColor(tone)
	if btn.Hovered() {
		btn.Background(p.selected).BorderColor(p.line).TextColor(p.ink)
	}
	btn.Children(func() {
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			if live > 0 {
				activityIndicator(c, p.running).Size(8, 8)
			} else {
				ui.Icon(c, agentsIcon).FontSize(12).TextColor(tone)
			}
			ui.Text(c, badgeText).FontSize(11).FontWeight(500).NoWrap()
			ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint)
		})
	})
	btn.PopoverMenuButton(func(m *ui.Menu) {
		if rootID != w.selected {
			if m.Item(w.tr("父会话：", "Parent: ") + w.displayTitleFor(rootID)).Chosen() {
				w.selectThread(rootID)
			}
			m.Separator()
		}
		for _, item := range items {
			item := item
			st := w.tr(statusLabel(item.status), englishStatus(item.status))
			label := stripInlineMarkdownTitle(item.title)
			if st != "" && item.status != "" {
				label += " · " + st
			}
			if m.Item(label).Radio(w.subagentPanelShows(item.threadID)).Chosen() {
				w.openSubagentPanel(item.threadID)
			}
		}
	}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomEnd, Self: ui.AnchorTopEnd})
}

// subagentComposerBar mirrors T3 Code's ProviderSubagentBar above the composer
// when viewing a delegated subagent session.
func (w *workbench) subagentComposerBar(c *ui.Context, p palette, threadID string) {
	thread, ok := w.threadByID(threadID)
	if !ok || thread.ParentThreadID == "" || thread.ParentThreadID == threadID {
		return
	}
	parentTitle := w.displayTitleFor(thread.ParentThreadID)
	status := api.RunStatus("")
	if latest, hasRun := w.latestRun(threadID); hasRun {
		status = latest.Status
	}
	tone := taskStatusTone(p, status)
	ui.Row(c.Key("subagent-bar:"+threadID)).FillWidth().Margin(0, 0, 8).Height(34).Padding(0, 8, 0, 12).Gap(8).Radius(11).
		Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Role(ui.RoleGroup).
		Label(w.tr("子代理会话", "Subagent session")).Children(func() {
		if status == api.RunRunning {
			activityIndicator(c, tone).Size(9, 9).Shrink(0)
		} else {
			ui.Icon(c, agentsIcon).FontSize(13).TextColor(tone).Shrink(0)
		}
		ui.Text(c, w.tr("子代理 · 独立执行", "Subagent · runs independently")).FontSize(12).FontWeight(500).TextColor(p.ink).NoWrap().Shrink(0)
		ui.Text(c, w.tr("父会话：", "Parent: ")+parentTitle).FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
		back := quietButton(c, p, "subagent-open-parent", "").Label(w.tr("返回父会话", "Open parent")).Height(24).Padding(0, 8).Radius(7).Shrink(0)
		back.Children(func() {
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, backIcon).FontSize(11)
				ui.Text(c, w.tr("返回父会话", "Open parent")).FontSize(11.5).NoWrap()
			})
		})
		if back.Clicked() {
			w.selectThread(thread.ParentThreadID)
		}
	})
}

func englishStatus(status api.RunStatus) string {
	switch status {
	case api.RunQueued:
		return "Queued"
	case api.RunRunning:
		return "Running"
	case api.RunWaiting:
		return "Waiting"
	case api.RunCompleted:
		return "Done"
	case api.RunCancelled:
		return "Stopped"
	case api.RunInterrupted:
		return "Interrupted"
	case api.RunFailed:
		return "Failed"
	}
	return string(status)
}

// taskListView shows the work a turn handed off in a cohesive hairline card:
// delegated subtasks (with Open and, while they run, Cancel) and peer threads.
func (w *workbench) taskListView(c *ui.Context, p palette, parentRunID string) {
	if w.widgets.taskExpanded == nil {
		w.widgets.taskExpanded = map[string]bool{}
	}
	var turnTasks []api.Task
	children := map[string]bool{}
	for _, task := range w.tasks {
		if task.ParentRunID != parentRunID {
			continue
		}
		turnTasks = append(turnTasks, task)
		children[task.ChildThreadID] = true
	}
	if len(turnTasks) > 0 {
		w.subagentGroup(c, p, parentRunID, turnTasks)
	}
	for _, peer := range w.threads {
		if peer.CreatedByRunID != parentRunID || children[peer.ID] || peer.ID == w.selected {
			continue
		}
		peer := peer
		status := api.RunStatus("")
		if latest, ok := w.latestRun(peer.ID); ok {
			status = latest.Status
		}
		ui.Row(c.Key("peer:"+peer.ID)).Gap(6).Padding(8, 10).Radius(10).Background(p.soft).Border(1, p.line).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, swapIcon).FontSize(13).TextColor(p.muted)
			w.agentCardHeader(c, p, w.titleFor(peer.ID), status, peer.ID)
			if quietButton(c, p, "open-peer", w.tr("打开", "Open")).Label(w.tr("打开会话 ", "Open thread ") + w.titleFor(peer.ID)).Clicked() {
				w.selectThread(peer.ID)
			}
		})
	}
	for _, cohort := range w.cohorts {
		if cohort.ParentRunID != parentRunID || cohort.Status != api.DeliveryDelivered {
			continue
		}
		cohort := cohort
		ui.Row(c.Key(fmt.Sprintf("cohort:%s:%d", parentRunID, cohort.Generation))).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "子任务结果已交付，确认后继续父任务。").FontSize(12).Wrap().Grow(1)
			if ui.Button(c, "确认结果并继续").Clicked() {
				w.taskAction(fmt.Sprintf("ack:%s:%d", parentRunID, cohort.Generation), api.Command{
					Kind: api.AcknowledgeTask, ParentRunID: parentRunID,
					Generation: cohort.Generation, DeliveryMessageID: cohort.DeliveryMessageID,
				})
			}
		})
	}
}

// childApprovals are the pending requests of the threads the selected thread
// handed work to: its subtasks and the peer threads it launched.
func (w *workbench) childApprovals(parentID string) []api.ProviderRequest {
	if parentID == "" {
		return nil
	}
	children := map[string]bool{}
	for _, task := range w.tasks {
		if task.ParentThreadID == parentID && task.ChildThreadID != "" {
			children[task.ChildThreadID] = true
		}
	}
	for _, thread := range w.threads {
		if thread.ID != parentID && (thread.ParentThreadID == parentID || thread.CreatedByThreadID == parentID) {
			children[thread.ID] = true
		}
	}
	var pending []api.ProviderRequest
	for _, request := range w.requests {
		if children[request.ThreadID] && request.Status == "pending" {
			pending = append(pending, request)
		}
	}
	return pending
}

// childApprovalsView surfaces a subtask's pending approval in its parent,
// above the composer, with Allow, Deny and Open thread.
func (w *workbench) childApprovalsView(c *ui.Context, p palette, parentID string) {
	for _, request := range w.childApprovals(parentID) {
		request := request
		child := w.titleFor(request.ThreadID)
		busy := w.controlBusy("request:" + request.ID)
		ui.Column(c.Key("child-approval:"+request.ID)).FillWidth().Margin(0, 0, 8).Padding(10, 12).Gap(8).Radius(12).
			Background(p.raised).Border(1, p.warning.Alpha(0.4)).Role(ui.RoleGroup).
			Label(w.tr("子任务「"+child+"」正在等待权限确认", "Subtask \""+child+"\" needs approval")).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, shieldIcon).FontSize(15).TextColor(p.warning)
				ui.Text(c, w.tr("子任务「"+child+"」正在等待权限确认", "Subtask \""+child+"\" needs approval")).FontSize(12.5).FontWeight(600).TextColor(p.ink).Grow(1).Wrap()
			})
			headline, _ := splitApprovalTitle(request)
			if detail := strings.TrimSpace(headline); detail != "" {
				ui.Text(c, detail).FontSize(11).TextColor(p.muted).Wrap()
			}
			allowOnce, allowSession, denyOpt, isApprovalSel := parseApprovalSelect(request)
			ui.Row(c).Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
				open := quietButton(c, p, "open-child-thread", w.tr("查看子会话", "Open thread"))
				if open.Clicked() {
					w.openSubagentPanel(request.ThreadID)
				}
				ui.Spacer(c)
				denyLabel := w.tr("拒绝", "Deny")
				if isApprovalSel {
					denyLabel = denyOpt
				}
				deny := ui.Button(c.Key("deny"), denyLabel).FontSize(12).Padding(5, 12).Disabled(busy)
				if deny.Clicked() {
					if isApprovalSel {
						w.respondRequest(request, denyOpt, false, false)
					} else {
						w.respondRequest(request, "", false, request.Method != "confirm")
					}
				}
				if isApprovalSel {
					if allowSession != "" {
						sess := ui.Button(c.Key("allow-session"), allowSession).FontSize(12).Padding(5, 10).Disabled(busy)
						if sess.Clicked() {
							w.respondRequest(request, allowSession, false, false)
						}
					}
					allow := ui.PrimaryButton(c.Key("allow"), allowOnce).FontSize(12).Padding(5, 13).Disabled(busy)
					if allow.Clicked() {
						w.respondRequest(request, allowOnce, false, false)
					}
				} else if request.Method == "confirm" {
					allow := ui.PrimaryButton(c.Key("allow"), w.tr("允许执行", "Allow")).FontSize(12).Padding(5, 13).Disabled(busy)
					if allow.Clicked() {
						w.respondRequest(request, "", true, false)
					}
				}
			})
		})
	}
}

// subagentGroup is T3 Code's subagent block in the timeline: a header with
// the agents' stacked avatars, "N subagents" over "N done", and the span
// they took; open, one line per agent with its avatar and status dot, its
// title over the first line of its result, and when it finished. A row
// opens the agent's thread; its menu keeps cancel and delivery actions.
func (w *workbench) subagentGroup(c *ui.Context, p palette, parentRunID string, tasks []api.Task) {
	key := "subagents:" + parentRunID
	live, failed := 0, 0
	first, last := time.Time{}, time.Time{}
	for _, task := range tasks {
		if !task.Status.Terminal() {
			live++
		}
		if task.Status == api.RunFailed || task.Status == api.RunInterrupted {
			failed++
		}
		if first.IsZero() || !task.CreatedAt.IsZero() && task.CreatedAt.Before(first) {
			first = task.CreatedAt
		}
		if task.UpdatedAt.After(last) {
			last = task.UpdatedAt
		}
	}
	open, chosen := w.widgets.taskExpanded[key]
	if !chosen {
		open = true
	}
	ui.Column(c.Key("tasks-group:" + parentRunID)).FillWidth().Gap(2).Children(func() {
		header := ui.Button(c.Key("subagents-header"), "").Label(w.tr("子代理", "Subagents")).Expanded(open).FillWidth().Height(40).Padding(0, 6).Gap(10).Radius(8).
			Border(0, p.line).Background(ui.Transparent).Justify(ui.Start)
		hovered := header.Hovered() || header.FocusVisible()
		if hovered {
			header.Background(p.soft)
		}
		header.Children(func() {
			// Stacked avatars: up to three, then +N.
			ui.Row(c).Shrink(0).AlignItems(ui.Center).Children(func() {
				for i, task := range tasks {
					if i == 3 {
						ui.Text(c, fmt.Sprintf("+%d", len(tasks)-3)).FontSize(10.5).TextColor(p.faint).Margin(0, 0, 0, 4).NoWrap()
						break
					}
					avatar := agentAvatar(c, p, nonEmptyString(task.Title, task.ChildThreadID), 20)
					if i > 0 {
						avatar.Margin(0, 0, 0, -6)
					}
				}
			})
			ui.Column(c).Gap(0).Grow(1).MinWidth(0).Children(func() {
				done := len(tasks) - live
				ui.Text(c, fmt.Sprintf(w.tr("%d 个子代理 · %d 完成", "%d subagents · %d done"), len(tasks), done)).FontSize(12.5).FontWeight(500).TextColor(p.ink).SingleLine().Ellipsis("…")
				sub, tone := w.tr("全部结束", "All finished"), p.faint
				if live > 0 {
					sub, tone = fmt.Sprintf(w.tr("%d 个进行中", "%d working"), live), p.running
				}
				if failed > 0 {
					sub += fmt.Sprintf(w.tr(" · %d 个未完成", " · %d failed"), failed)
					if live == 0 {
						tone = p.danger
					}
				}
				ui.Text(c, sub).FontSize(11).TextColor(tone).SingleLine()
			})
			if !first.IsZero() && !last.IsZero() && last.After(first) {
				span := last.Sub(first)
				if live > 0 {
					span = time.Since(first)
				}
				ui.Text(c, spanDuration(span)).FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
			}
			disclosureChevron(c, p, open, hovered)
		})
		if header.Clicked() {
			w.widgets.taskExpanded[key] = !open
		}
		if !open {
			return
		}
		for _, task := range tasks {
			w.subagentRow(c, p, task)
		}
	})
}

// subagentRow is one Stardust subagent card: avatar with status dot, title,
// colored status pill, model/snippet subtitle, elapsed time and chevron.
func (w *workbench) subagentRow(c *ui.Context, p palette, task api.Task) {
	title := stripInlineMarkdownTitle(nonEmptyString(task.Title, w.displayTitleFor(task.ChildThreadID)))
	tone := taskStatusTone(p, task.Status)
	openHere := task.ChildThreadID != "" && w.subagentPanelShows(task.ChildThreadID)
	card := ui.Column(c.Key(fmt.Sprintf("task:%s:%d", task.ID, task.Generation))).FillWidth().Padding(6, 8).Gap(4).Radius(11).
		Background(p.raised).Border(1, p.lineSoft)
	hovered := card.Hovered()
	switch {
	case openHere:
		card.Background(p.accent.Alpha(0.08)).BorderColor(p.accent.Alpha(0.4))
	case hovered:
		card.BorderColor(p.line)
	}
	card.Children(func() {
		ui.Row(c).FillWidth().MinHeight(34).Gap(4).AlignItems(ui.Center).Children(func() {
			row := ui.Button(c.Key("open"), "").Label(w.tr("打开子任务", "Open subtask")).Description(title).Selected(openHere).
				Grow(1).MinWidth(0).MinHeight(34).Padding(2, 4).Gap(9).Radius(8).Border(0, p.line).Background(ui.Transparent).Justify(ui.Start)
			model, elapsed := "", ""
			if child, ok := w.threadByID(task.ChildThreadID); ok {
				model = w.threadModelLabel(child)
			}
			if !task.CreatedAt.IsZero() {
				end := task.UpdatedAt
				if !task.Status.Terminal() || end.IsZero() || end.Before(task.CreatedAt) {
					end = time.Now()
				}
				if end.After(task.CreatedAt) {
					elapsed = spanDuration(end.Sub(task.CreatedAt))
				}
			}
			detail := model
			if hint := strings.TrimSpace(task.Result); hint != "" {
				row.Tooltip(firstLine(hint))
				if detail == "" {
					detail = firstLine(hint)
				}
			}
			if detail == "" {
				detail = w.tr("子代理", "Subagent")
			}
			row.Children(func() {
				ui.Box(c).Size(24, 24).Shrink(0).Children(func() {
					agentAvatar(c, p, nonEmptyString(task.Title, task.ChildThreadID), 24)
					ui.Box(c).Absolute().Right(-2).Bottom(-2).Size(9, 9).Radius(5).Background(tone).Border(1.5, p.raised)
				})
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).MinWidth(0).Children(func() {
						ui.Text(c, title).FontSize(12.5).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
						state := w.tr(statusLabel(task.Status), englishStatus(task.Status))
						ui.Row(c).Height(18).Padding(0, 6).Gap(4).Radius(9).Background(tone.Alpha(0.12)).AlignItems(ui.Center).Shrink(0).Children(func() {
							if task.Status == api.RunRunning || task.Status == api.RunQueued {
								activityIndicator(c, tone).Size(8, 8).Shrink(0)
							} else {
								ui.Box(c).Size(5, 5).Radius(3).Background(tone).Shrink(0)
							}
							ui.Text(c, state).FontSize(10.5).FontWeight(500).TextColor(tone).NoWrap()
						})
					})
					ui.Text(c, detail).FontSize(11.5).TextColor(p.faint).SingleLine().Ellipsis("…")
				})
				if elapsed != "" {
					ui.Text(c, elapsed).FontSize(11).FontFeatures("tnum").TextColor(p.faint).NoWrap().Shrink(0)
				} else if task.Status == api.RunCompleted && !task.UpdatedAt.IsZero() {
					ui.Text(c, task.UpdatedAt.Local().Format("15:04")).FontSize(11).FontFeatures("tnum").TextColor(p.faint).NoWrap().Shrink(0)
				}
			})
			if row.Clicked() {
				w.openSubagentPanel(task.ChildThreadID)
			}
			if !task.Status.Terminal() {
				cancel := iconButton(c, p, "cancel-task", w.tr("取消子任务", "Cancel subtask"), lucide("square")).Size(24, 24).Padding(6)
				cancel.Tooltip(w.tr("停止这个仍在运行的子代理", "Stop this running subagent"))
				if cancel.Hovered() {
					cancel.TextColor(p.danger).Background(p.danger.Alpha(0.1))
				} else if !hovered {
					cancel.Opacity(0.45)
				}
				if cancel.Clicked() {
					w.cancelTask(task)
				}
			}
			ui.Icon(c, rightIcon).FontSize(11).TextColor(p.faint).Shrink(0)
			row.ContextMenu(func(m *ui.Menu) {
				if m.Item(w.tr("打开子代理面板", "Open subagent panel")).Chosen() {
					w.openSubagentPanel(task.ChildThreadID)
				}
				if m.Item(w.tr("在主视图打开", "Open in main view")).Chosen() {
					w.closeSubagentPanel()
					w.selectThread(task.ChildThreadID)
				}
				if !task.Status.Terminal() && m.Item(w.tr("取消子代理", "Cancel subagent")).Chosen() {
					w.cancelTask(task)
				}
				if task.DeliveryStatus == api.DeliveryDelivered && m.Item(w.tr("忽略此结果", "Dispose result")).Chosen() {
					w.taskAction(fmt.Sprintf("dispose:%s:%d", task.ID, task.Generation), api.Command{
						Kind: api.DisposeTask, TaskID: task.ID, ParentRunID: task.ParentRunID,
						Generation: task.Generation, DeliveryMessageID: task.DeliveryMessageID,
					})
				}
				if task.DeliveryStatus == api.DeliveryAcknowledged || task.DeliveryStatus == api.DeliveryDisposed {
					draftEmpty := strings.TrimSpace(w.composerFor(w.selected).draft) == ""
					if m.Item(w.tr("用草稿追问子代理", "Follow up with draft")).Disabled(draftEmpty).Chosen() {
						w.followUpDraft(task)
					}
				}
				if m.Item(w.tr("复制结果", "Copy result")).Disabled(strings.TrimSpace(task.Result) == "").Chosen() {
					c.WriteClipboard(task.Result)
				}
			})
		})
	})
}

// threadOrchestrationTasks returns the tasks belonging to threadID's latest
// orchestrated run (or all tasks for threadID if none match the latest run).
func (w *workbench) threadOrchestrationTasks(threadID string) []api.Task {
	if threadID == "" || len(w.tasks) == 0 {
		return nil
	}
	var all []api.Task
	for _, task := range w.tasks {
		if task.ParentThreadID == threadID {
			all = append(all, task)
		}
	}
	if len(all) == 0 {
		return nil
	}
	latestRunID := all[len(all)-1].ParentRunID
	var scoped []api.Task
	for _, task := range all {
		if task.ParentRunID == latestRunID {
			scoped = append(scoped, task)
		}
	}
	return scoped
}

// taskOrchestrationCard renders the floating Stardust task-orchestration pill
// and expandable checklist card above the composer whenever the thread has
// real orchestrated tasks.
func (w *workbench) taskOrchestrationCard(c *ui.Context, p palette, threadID string) {
	tasks := w.threadOrchestrationTasks(threadID)
	if len(tasks) == 0 {
		return
	}
	if w.widgets.orchestrationExpanded == nil {
		w.widgets.orchestrationExpanded = map[string]bool{}
	}
	expanded := w.widgets.orchestrationExpanded[threadID]
	done, running, failed := 0, 0, 0
	var focusTitle string
	for _, task := range tasks {
		switch task.Status {
		case api.RunCompleted:
			done++
		case api.RunRunning, api.RunQueued, api.RunWaiting:
			running++
			if focusTitle == "" {
				focusTitle = stripInlineMarkdownTitle(nonEmptyString(task.Title, w.displayTitleFor(task.ChildThreadID)))
			}
		case api.RunFailed, api.RunInterrupted:
			failed++
		}
	}
	if focusTitle == "" {
		focusTitle = stripInlineMarkdownTitle(nonEmptyString(tasks[0].Title, w.displayTitleFor(tasks[0].ChildThreadID)))
	}
	total := len(tasks)

	if expanded {
		card := ui.Column(c.Key("orch-card:"+threadID)).FillWidth().Margin(0, 0, 8).Padding(10, 12).Gap(6).
			Radius(14).Background(p.raised).Border(1, p.line).Role(ui.RoleGroup).
			Label(w.tr3("任务统筹", "Task orchestration", "タスク編成"))
		if p.dark {
			card.Shadow(0, 10, 28, 0, ui.Hex("#00000066"))
		} else {
			card.Shadow(0, 8, 24, 0, ui.Hex("#1D1D1F14"))
		}
		card.Children(func() {
			ui.Row(c).FillWidth().Gap(7).AlignItems(ui.Center).Children(func() {
				ui.Text(c, w.tr3("任务统筹", "Task orchestration", "タスク編成")).FontSize(12.5).FontWeight(600).TextColor(p.ink).NoWrap()
				ui.Text(c, fmt.Sprintf("%d/%d", done, total)).FontSize(11.5).FontFeatures("tnum").TextColor(p.faint).NoWrap()
				badgeText, badgeTone := w.tr3("全部完成", "All done", "完了"), p.success
				if running > 0 {
					badgeText, badgeTone = fmt.Sprintf(w.tr3("%d 项进行中", "%d running", "%d 件実行中"), running), p.running
				} else if failed > 0 {
					badgeText, badgeTone = fmt.Sprintf(w.tr3("%d 项未完成", "%d failed", "%d 件失敗"), failed), p.danger
				}
				pill(c, p, badgeText, badgeTone)
				ui.Spacer(c)
				collapse := quietButton(c, p, "orch-collapse", w.tr3("收起", "Collapse", "折りたたむ")).Height(22).Padding(0, 8).FontSize(11)
				if collapse.Clicked() {
					w.widgets.orchestrationExpanded[threadID] = false
				}
			})
			for i, task := range tasks {
				task := task
				title := stripInlineMarkdownTitle(nonEmptyString(task.Title, w.displayTitleFor(task.ChildThreadID)))
				rowBtn := ui.Button(c.Key(fmt.Sprintf("orch-item:%d:%s", i, task.ID)), "").Label(title).
					FillWidth().Height(30).Padding(0, 6).Radius(8).Border(0, p.line).Background(ui.Transparent).Justify(ui.Start)
				if rowBtn.Hovered() {
					rowBtn.Background(p.soft)
				}
				rowBtn.Children(func() {
					ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						switch task.Status {
						case api.RunCompleted:
							ui.Icon(c, lucide("circle-check")).FontSize(14).TextColor(p.success).Shrink(0)
						case api.RunRunning:
							ui.Row(c).Size(14, 14).Radius(7).Border(1.5, p.running).Justify(ui.Center).AlignItems(ui.Center).Shrink(0).Children(func() {
								ui.Box(c).Size(6, 6).Radius(3).Background(p.running)
							})
						case api.RunFailed, api.RunInterrupted:
							ui.Icon(c, lucide("circle-x")).FontSize(14).TextColor(p.danger).Shrink(0)
						default:
							ui.Icon(c, lucide("circle")).FontSize(14).TextColor(p.faint).Shrink(0)
						}
						ui.Text(c, title).FontSize(12.5).FontWeight(500).TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
						tagText, tagTone := w.tr3("未开始", "Pending", "未開始"), p.muted
						switch task.Status {
						case api.RunCompleted:
							tagText, tagTone = w.tr3("已完成", "Done", "完了"), p.success
						case api.RunRunning:
							tagText, tagTone = w.tr3("进行中", "Running", "実行中"), p.running
						case api.RunWaiting:
							tagText, tagTone = w.tr3("等待中", "Waiting", "待機中"), p.warning
						case api.RunFailed, api.RunInterrupted:
							tagText, tagTone = w.tr3("失败", "Failed", "失敗"), p.danger
						}
						ui.Row(c).Height(20).Padding(0, 7).Radius(6).Background(tagTone.Alpha(0.12)).AlignItems(ui.Center).Shrink(0).Children(func() {
							ui.Text(c, tagText).FontSize(10.5).FontWeight(500).TextColor(tagTone).NoWrap()
						})
					})
				})
				if rowBtn.Clicked() && task.ChildThreadID != "" {
					w.openSubagentPanel(task.ChildThreadID)
				}
			}
		})
	}

	// Compact floating summary pill anchored directly above the composer.
	ui.Row(c.Key("orch-pill-row:"+threadID)).FillWidth().Margin(0, 0, 8).Justify(ui.Center).Children(func() {
		pillBtn := ui.Button(c.Key("orch-toggle:"+threadID), "").Label(w.tr3("展开任务统筹", "Toggle task orchestration", "タスク編成を切り替え")).
			Expanded(expanded).Height(32).MaxWidth(520).Padding(0, 12).Radius(16).
			Background(p.raised).Border(1, p.line).TextColor(p.ink)
		if p.dark {
			pillBtn.Shadow(0, 4, 16, 0, ui.Hex("#00000055"))
		} else {
			pillBtn.Shadow(0, 3, 12, 0, ui.Hex("#1D1D1F12"))
		}
		if pillBtn.Hovered() {
			pillBtn.BorderColor(p.accent.Alpha(0.45))
		}
		pillBtn.Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).MinWidth(0).Children(func() {
				switch {
				case running > 0:
					ui.Row(c).Size(14, 14).Radius(7).Border(1.5, p.running).Justify(ui.Center).AlignItems(ui.Center).Shrink(0).Children(func() {
						ui.Box(c).Size(6, 6).Radius(3).Background(p.running)
					})
				case failed > 0:
					ui.Icon(c, lucide("circle-x")).FontSize(14).TextColor(p.danger).Shrink(0)
				default:
					ui.Icon(c, lucide("circle-check")).FontSize(14).TextColor(p.success).Shrink(0)
				}
				summary := focusTitle
				if running == 0 && done == total {
					summary = fmt.Sprintf(w.tr3("%d 项任务完成", "%d tasks completed", "%d 件完了"), total)
				}
				ui.Text(c, summary).FontSize(12).FontWeight(500).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				// Segmented progress dashes (up to 6 segments)
				segs := min(total, 6)
				ui.Row(c).Gap(3).AlignItems(ui.Center).Shrink(0).Children(func() {
					for i := 0; i < segs; i++ {
						segColor := p.line
						if i < done {
							segColor = p.success
						} else if i < done+running {
							segColor = p.running
						}
						ui.Box(c).Size(10, 3).Radius(1.5).Background(segColor)
					}
				})
				ui.Text(c, fmt.Sprintf("%d/%d", done, total)).FontSize(11).FontFeatures("tnum").TextColor(p.faint).NoWrap().Shrink(0)
				chev := upIcon
				if expanded {
					chev = downIcon
				}
				ui.Icon(c, chev).FontSize(11).TextColor(p.faint).Shrink(0)
			})
		})
		if pillBtn.Clicked() {
			w.widgets.orchestrationExpanded[threadID] = !expanded
		}
	})
}

// agentAvatar is a subagent's initials on a tinted square, as T3 Code's
// agent avatars; the same title always gets the same tone.
func agentAvatar(c *ui.Context, p palette, name string, size float32) ui.Element {
	tone := badgeColor(name)
	box := ui.Row(c).Size(size, size).Radius(size*0.28).Background(tone.Mix(p.canvas, 0.72)).Border(1, p.canvas).
		Justify(ui.Center).AlignItems(ui.Center).Shrink(0).AccessHidden(true)
	box.Children(func() {
		ui.Text(c, projectInitials(name)).FontSize(max(size*0.42, 7)).Bold().TextColor(tone).NoWrap()
	})
	return box
}

// spanDuration is "12m 34s" / "1h 4m" / "8s".
func spanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
