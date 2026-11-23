package desktop

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/muq9738/Vantry/pkg/api"
)

// A turn reads as the workbench's timeline does: the user's message, then the reply's
// text and the work the agent did in the order they happened, a live line
// while it works, and once done its time, actions and the files it changed.

type turnPart struct {
	text  string
	tools []api.ToolCall
}

// splitTurn interleaves a reply's text with its tool calls by the length the
// reply had when each call started. Calls without an offset follow the text.
func splitTurn(output string, tools []api.ToolCall) []turnPart {
	if len(tools) == 0 {
		if output == "" {
			return nil
		}
		return []turnPart{{text: output}}
	}
	placed := make([]api.ToolCall, len(tools))
	copy(placed, tools)
	offset := func(t api.ToolCall) int {
		if t.OutputOffset < 0 || t.OutputOffset > len(output) {
			return len(output)
		}
		at := t.OutputOffset
		for at > 0 && at < len(output) && !utf8.RuneStart(output[at]) {
			at--
		}
		return at
	}
	sort.SliceStable(placed, func(i, j int) bool { return offset(placed[i]) < offset(placed[j]) })
	var parts []turnPart
	at := 0
	for i := 0; i < len(placed); {
		next := offset(placed[i])
		if text := output[at:next]; strings.TrimSpace(text) != "" {
			parts = append(parts, turnPart{text: text})
		}
		at = next
		j := i
		for j < len(placed) && offset(placed[j]) == next {
			j++
		}
		parts = append(parts, turnPart{tools: placed[i:j]})
		i = j
	}
	if text := output[at:]; strings.TrimSpace(text) != "" {
		parts = append(parts, turnPart{text: text})
	}
	return parts
}

func (w *workbench) conversationRow(c *ui.Context, p palette, entry conversationEntry, narrow bool) {
	input := entry.run.Input
	items := entry.run.Attachments
	if entry.pending != nil {
		input, items = entry.pending.send.Message, entry.pending.send.Attachments
	}
	turn := ui.Column(c).FillWidth().MaxWidth(readingWidth).AlignSelf(ui.Center).Gap(14).Selectable().SelectionColor(accentSelection(p.dark))
	turn.CopyData(copyConversationSelection)
	turn.Children(func() {
		if sender := entry.run.SenderThreadID; sender != "" && entry.pending == nil {
			w.senderBadge(c, p, sender)
		}
		w.userMessage(c, p, input, items)
		ui.Column(c).FillWidth().Gap(8).Children(func() {
			if entry.pending != nil {
				w.pendingStatusView(c, p, entry.pending)
				return
			}
			w.assistantTurn(c, p, entry.run)
		})
	})
	w.replyMenu(c, turn)
}

// senderBadge marks a message another Pi session sent into this thread:
// ⇄ From thread: <title>, which opens that thread.
func (w *workbench) senderBadge(c *ui.Context, p palette, sender string) {
	title := w.titleFor(sender)
	label := w.tr("来自会话：", "From thread: ") + title
	ui.Row(c).FillWidth().Children(func() {
		ui.Spacer(c)
		badge := ui.Button(c.Key("sender:"+sender), "").Label(label).Tooltip(w.tr("打开发送消息的会话", "Open the thread that sent this")).
			Height(22).Padding(0, 9).Radius(11).Shrink(1).MinWidth(0).Border(1, p.accent.Alpha(0.35)).Background(p.accent.Alpha(0.1)).TextColor(p.accentInk)
		if badge.Hovered() {
			badge.Background(p.accent.Alpha(0.18))
		}
		badge.Children(func() {
			ui.Row(c).Gap(5).AlignItems(ui.Center).MinWidth(0).Children(func() {
				ui.Icon(c, swapIcon).FontSize(11)
				ui.Text(c, label).FontSize(11).SingleLine().Ellipsis("…").MaxWidth(420)
			})
		})
		if badge.Clicked() {
			w.openThreadLink(sender)
		}
	})
}

// openThreadLink selects a thread named by a vantry-thread:// link or badge.
func (w *workbench) openThreadLink(id string) bool {
	id = strings.TrimSpace(strings.TrimSuffix(id, "/"))
	if id == "" || !w.hasSession(id) {
		w.setAction("找不到链接指向的会话，它可能已被删除。")
		return false
	}
	w.selectThread(id)
	w.widgets.composerFocus.Focus()
	return true
}

func (w *workbench) userMessage(c *ui.Context, p palette, input string, items []api.Attachment) {
	ui.Row(c).FillWidth().Gap(5).AlignItems(ui.End).Children(func() {
		ui.Spacer(c)
		bubble := ui.Column(c).MaxWidth(min(float32(580), readingWidth*0.8)).Padding(10, 15).Gap(6).Radius(18).Background(p.user).Border(1, p.lineSoft)
		bubble.Children(func() { w.userMessageBody(c, p, input, items) })
		copy := iconButton(c, p, "copy-user-message", w.tr("复制消息", "Copy message"), copyIcon).Size(24, 24).Padding(5).Radius(6).Unselectable()
		copy.Description(w.tr("复制整条用户消息", "Copy the entire user message"))
		if !bubble.Hovered() && !copy.Hovered() {
			copy.Opacity(0.55)
		}
		if copy.Clicked() {
			c.WriteClipboard(visibleUserMessage(input, items))
		}
	})
}

func (w *workbench) assistantTurn(c *ui.Context, p palette, run api.Run) {
	tools := w.runTools(run.ID)
	parts := splitTurn(run.Output, tools)
	streaming := !run.Status.Terminal()
	lastText := -1
	for i, part := range parts {
		if part.tools == nil {
			lastText = i
		}
	}
	if run.Reasoning != "" {
		w.reasoningRow(c, p, run, streaming && len(parts) == 0)
	}
	var reply ui.Element
	options := w.replyMarkdown(run.ThreadID)
	options.ShareSelection = true
	prevMessage := w.visuals.renderingMessage
	w.visuals.renderingMessage = run.ID
	defer func() {
		options.ShareSelection = false
		w.visuals.renderingMessage = prevMessage
	}()
	for i, part := range parts {
		key := c.Key("part:" + strconv.Itoa(i))
		if part.tools != nil {
			w.workGroup(key, p, run, i, part.tools, streaming && i == len(parts)-1)
			continue
		}
		ui.Column(key).FillWidth().Children(func() {
			if streaming && i == lastText {
				reply = markdown.Stream(c, part.text, options)
			} else {
				reply = markdown.View(c, part.text, options)
			}
		})
	}
	if streaming {
		w.liveActivity(c, p, run, tools)
	}
	w.steerHistoryView(c, p, run.ID)
	w.steeringView(c, p, run)
	w.requestListView(c, p, run.ID)
	w.taskListView(c, p, run.ID)
	if run.Error != "" {
		errText := run.Error
		if errText == "pi workspace is required" {
			errText = w.tr("当时未配置工作目录，未能启动任务。现在可以直接重新发送，或先选择项目。", "No workspace was set, so the task could not start. Send again, or choose a project first.")
		}
		ui.Row(c).Gap(8).Padding(10, 12).Radius(8).Background(p.danger.Alpha(0.08)).Children(func() {
			ui.Icon(c, alertIcon).FontSize(14).TextColor(p.danger)
			ui.Text(c, w.tr("错误：", "Error: ")+errText).FontSize(12).TextColor(p.danger).Wrap().Grow(1).Selectable()
		})
	}
	if !streaming && (!reply.Valid() || markdown.Revealed(reply)) {
		w.turnMeta(c, p, run, tools)
	}
}

// disclosureRow is the timeline's compact fold line, modeled on T3 Code's
// WorkLogRow: tight 26px vertical cadence, inline label + metadata, and a
// quiet chevron revealed on hover or while expanded.
func disclosureRow(c *ui.Context, p palette, key, label string, open bool, children func(hovered bool)) ui.Element {
	row := ui.Button(c.Key(key), "").Label(label).FillWidth().Height(24).Padding(0, 6).Gap(6).Radius(6).
		Border(0, p.line).Background(ui.Transparent).TextColor(p.muted).Justify(ui.Start).Expanded(open)
	hovered := row.Hovered() || row.FocusVisible()
	if hovered {
		row.Background(p.soft)
	}
	row.Children(func() { children(hovered) })
	return row
}

// disclosureChevron ends a disclosure row; it keeps its room while hidden
// so the line does not shift under the pointer.
func disclosureChevron(c *ui.Context, p palette, open, hovered bool) {
	icon := rightIcon
	if open {
		icon = chevronIcon
	}
	chevron := ui.Icon(c, icon).FontSize(12).TextColor(p.faint).Shrink(0)
	if !open && !hovered {
		chevron.Opacity(0)
	}
}

// reasoningRow folds the model's reasoning, as the workbench's "Thinking"/"Thought".
func (w *workbench) reasoningRow(c *ui.Context, p palette, run api.Run, live bool) {
	if w.widgets.reasoningOpen == nil {
		w.widgets.reasoningOpen = map[string]bool{}
	}
	open, chosen := w.widgets.reasoningOpen[run.ID]
	if !chosen {
		open = w.widgets.defaultReasoningOpen
	}
	ui.Column(c.Key("reasoning")).FillWidth().Gap(4).Children(func() {
		label := w.tr("思考过程", "Reasoning")
		if live {
			label = w.tr("正在思考", "Thinking")
		}
		action := w.tr("展开思考过程", "Expand reasoning")
		if open {
			action = w.tr("收起思考过程", "Collapse reasoning")
		}
		toggle := disclosureRow(c, p, "toggle-reasoning", action, open, func(hovered bool) {
			if live {
				activityIndicator(c, p.accent).Size(11, 11).Shrink(0)
			} else {
				ui.Icon(c, brainIcon).FontSize(13).TextColor(p.faint).Shrink(0)
			}
			ui.Text(c, label).FontSize(12.5).FontWeight(500).TextColor(p.muted).NoWrap().Shrink(0)
			if !open {
				ui.Text(c, reasoningPreview(run.Reasoning)).FontSize(12.5).TextColor(p.faint).SingleLine().Ellipsis("…").Grow(1).Shrink(1).MinWidth(0)
			} else {
				ui.Spacer(c)
			}
			disclosureChevron(c, p, open, hovered)
		})
		if toggle.Clicked() {
			w.widgets.reasoningOpen[run.ID] = !open
		}
		if open {
			// Padding, not margin, indents the quote: a filled width plus a
			// margin would push the text past the reading column.
			ui.Column(c).FillWidth().Padding(0, 0, 4, 14).Children(func() {
				ui.Column(c).FillWidth().Padding(2, 0, 2, 14).BorderWidth(0, 0, 0, 2).BorderColor(p.lineSoft).Children(func() {
					ui.Text(c, run.Reasoning).FontSize(13).LineHeight(1.65).TextColor(p.muted).Wrap().Selectable()
				})
			})
		}
	})
}

// reasoningPreview is the last line of the reasoning, read from its end so a
// long block costs the same as a short one.
func reasoningPreview(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 400 {
		cut := len(text) - 400
		for cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut++
		}
		text = text[cut:]
	}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return strings.TrimSpace(text)
}

// workGroup shows consecutive tool calls in Codex / Antigravity's quiet
// WorkLog style: collapsed by default into a single summary line (with an
// inline live preview while running), expanding into an indented vertical
// timeline rail only when the user opens it (or via Ctrl+O).
func (w *workbench) workGroup(c *ui.Context, p palette, run api.Run, index int, tools []api.ToolCall, live bool) {
	if w.widgets.workOpen == nil {
		w.widgets.workOpen = map[string]bool{}
	}
	key := run.ID + ":" + strconv.Itoa(index)
	running, failed := 0, 0
	var activeTool *api.ToolCall
	for i := range tools {
		switch tools[i].Status {
		case "running":
			running++
			activeTool = &tools[i]
		case "failed", "interrupted":
			failed++
		}
	}
	open, chosen := w.widgets.workOpen[key]
	if !chosen {
		open = len(tools) == 1 || w.widgets.defaultToolsExpanded
	}
	if len(tools) == 1 {
		ui.Column(c).FillWidth().Children(func() {
			w.toolEntry(c.Key("tool:"+tools[0].ID), p, run, tools[0])
		})
		return
	}
	box := ui.Column(c).FillWidth().Gap(2)
	if open {
		box.Padding(4, 6, 6).Radius(11).Background(p.soft.Alpha(0.55)).Border(1, p.lineSoft)
	}
	box.Children(func() {
		label := w.tr("收起步骤", "Collapse steps")
		if !open {
			label = w.tr("展开步骤", "Expand steps")
		}
		toggle := disclosureRow(c, p, "toggle-work", label, open, func(hovered bool) {
			if running > 0 {
				activityIndicator(c, p.accent).Size(11, 11).Shrink(0)
			} else {
				tone := p.faint
				if failed > 0 {
					tone = p.danger.Mix(p.faint, 0.35)
				}
				ui.Icon(c, toolIcon).FontSize(12.5).TextColor(tone).Shrink(0)
			}
			summaryColor := p.muted
			if hovered || open {
				summaryColor = p.ink
			}
			ui.Text(c, w.workSummary(tools)).FontSize(12.5).FontWeight(500).TextColor(summaryColor).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			if !open && (live || running > 0) {
				previewTool := activeTool
				if previewTool == nil && len(tools) > 0 {
					previewTool = &tools[len(tools)-1]
				}
				if previewTool != nil {
					if hint := w.compactToolHint(run.ThreadID, *previewTool); hint != "" {
						ui.Text(c, "· "+hint).FontSize(11.5).TextColor(p.faint).SingleLine().Ellipsis("…").Shrink(2).MinWidth(0)
					}
				}
			}
			if failed > 0 {
				ui.Row(c).Height(17).Padding(0, 6).Radius(9).Background(p.danger.Alpha(0.12)).AlignItems(ui.Center).Shrink(0).Children(func() {
					ui.Text(c, fmt.Sprintf(w.tr3("%d 失败", "%d failed", "%d 失敗"), failed)).FontSize(10.5).FontWeight(500).TextColor(p.danger).NoWrap()
				})
			}
			disclosureChevron(c, p, open, hovered)
			ui.Spacer(c)
		})
		if toggle.Clicked() {
			w.widgets.workOpen[key] = !open
		}
		if !open {
			return
		}
		renderTools := func() {
			ui.Column(c).FillWidth().Padding(2, 0, 2, 10).Children(func() {
				ui.Column(c).FillWidth().Padding(0, 0, 0, 10).Gap(1).BorderWidth(0, 0, 0, 1.5).BorderColor(p.line).Children(func() {
					for _, tool := range tools {
						w.toolEntry(c.Key("tool:"+tool.ID), p, run, tool)
					}
				})
			})
		}
		if len(tools) > 10 {
			ui.Scroll(c.Key("work-scroll:" + key)).FillWidth().MaxHeight(280).Children(renderTools)
		} else {
			renderTools()
		}
	})
}

func (w *workbench) compactToolHint(threadID string, tool api.ToolCall) string {
	view := w.toolView(tool)
	verb := strings.TrimSpace(w.toolVerb(view))
	target := strings.TrimSpace(view.target)
	if view.path != "" && (view.kind == toolRead || view.kind == toolEdit || view.kind == toolWrite || view.kind == toolList) {
		target = w.relativePath(threadID, view.path)
	}
	switch {
	case verb != "" && target != "":
		return verb + " " + target
	case target != "":
		return target
	default:
		return verb
	}
}

func (w *workbench) toolEntry(c *ui.Context, p palette, run api.Run, tool api.ToolCall) {
	if w.widgets.toolExpanded == nil {
		w.widgets.toolExpanded = map[string]bool{}
	}
	view := w.toolView(tool)
	expanded, chosen := w.widgets.toolExpanded[tool.ID]
	if !chosen {
		expanded = w.widgets.defaultToolsExpanded
	}
	awaiting := w.awaitingApproval(run.ID)
	ui.Column(c).FillWidth().Gap(2).Children(func() {
		label := w.tr("展开工具", "Expand tool")
		if expanded {
			label = w.tr("收起工具", "Collapse tool")
		}
		row := disclosureRow(c, p, "toggle", label, expanded, func(hovered bool) {
			// One 12px line per step, as T3 Code's work entries: a faint kind
			// icon, the verb, and the target in small monospace. A failed step
			// keeps the same line with a muted red icon instead of a label.
			color, verbColor := p.faint, p.muted
			switch {
			case tool.Status == "running" && !awaiting:
				activityIndicator(c, p.accent).Size(10, 10).Shrink(0)
			case tool.Status == "running" && awaiting:
				color = p.warning
				ui.Icon(c, shieldIcon).FontSize(12).TextColor(p.warning).Shrink(0)
			case tool.Status == "failed":
				ui.Icon(c, lucide("circle-x")).FontSize(12).TextColor(p.danger.Mix(p.faint, 0.35)).Shrink(0)
			default:
				ui.Icon(c, toolKindIcon(view.kind)).FontSize(12).TextColor(p.faint).Shrink(0)
			}
			ui.Text(c, w.toolVerb(view)).FontSize(12).TextColor(verbColor).NoWrap().Shrink(0)
			target := view.target
			isPath := view.path != "" && (view.kind == toolRead || view.kind == toolEdit || view.kind == toolWrite || view.kind == toolList)
			if isPath {
				target = w.relativePath(run.ThreadID, view.path)
			}
			targetColor := p.faint
			if hovered || expanded {
				targetColor = p.muted
			}
			if target != "" {
				txt := ui.Text(c, target).FontSize(12).TextColor(targetColor).Shrink(1).MinWidth(0).SingleLine().Ellipsis("…")
				if isPath || view.kind == toolCommand {
					txt.Font("Menlo").FontSize(11)
				}
			}
			if view.added > 0 || view.removed > 0 {
				diffCounts(c, p, view.added, view.removed)
			}
			if status := toolStatusText(tool, awaiting); status != "" && tool.Status != "failed" {
				ui.Text(c, w.tr(status, toolStatusEnglish(status))).FontSize(11).TextColor(color).NoWrap().Shrink(0)
			}
			disclosureChevron(c, p, expanded, hovered)
			ui.Spacer(c)
		})
		row.Description(view.verb + " " + view.target)
		if row.Clicked() {
			w.widgets.toolExpanded[tool.ID] = !expanded
		}
		if expanded {
			w.toolDetails(c, p, tool, view)
		}
	})
}

func toolStatusText(tool api.ToolCall, awaiting bool) string {
	switch tool.Status {
	case "running":
		if awaiting {
			return "等待确认"
		}
		return "运行中"
	case "failed":
		return "失败"
	case "interrupted":
		return "已中断"
	}
	return ""
}

func toolStatusEnglish(status string) string {
	switch status {
	case "等待确认":
		return "Awaiting approval"
	case "运行中":
		return "Running"
	case "已中断":
		return "Interrupted"
	}
	return status
}

func diffCounts(c *ui.Context, p palette, added, removed int) {
	ui.Row(c).Gap(4).Shrink(0).Children(func() {
		if added > 0 {
			ui.Text(c, "+"+strconv.Itoa(added)).Font("Menlo").FontSize(11).TextColor(p.success)
		}
		if removed > 0 {
			ui.Text(c, "−"+strconv.Itoa(removed)).Font("Menlo").FontSize(11).TextColor(p.danger)
		}
	})
}

func (w *workbench) toolDetails(c *ui.Context, p palette, tool api.ToolCall, view toolView) {
	ui.Column(c).FillWidth().Gap(8).Padding(2, 8, 8, 29).Children(func() {
		switch view.kind {
		case toolEdit:
			for i, edit := range view.edits {
				w.inlineDiff(c.Key("edit:"+strconv.Itoa(i)), tool.ID+":"+strconv.Itoa(i), view.path, edit.oldText, edit.newText)
			}
			if len(view.edits) == 0 {
				outputPanel(c, p, w.tr("参数", "Arguments"), displayJSON(tool.Arguments))
			}
		case toolWrite:
			w.inlineDiff(c.Key("write"), tool.ID+":write", view.path, "", view.content)
		case toolCommand:
			outputPanel(c, p, w.tr("命令", "Command"), "$ "+view.command)
		default:
			outputPanel(c, p, w.tr("参数", "Arguments"), displayJSON(tool.Arguments))
		}
		if tool.Output != "" && (view.kind != toolEdit && view.kind != toolWrite || tool.Status == "failed") {
			outputPanel(c, p, w.tr("输出", "Output"), tool.Output)
		}
		if tool.Status == "completed" {
			ui.Text(c, w.tr("已完成", "Done")).FontSize(11).TextColor(p.faint)
		}
	})
}

// outputPanel is a recessed code well with a quiet caption above it.
func outputPanel(c *ui.Context, p palette, title, text string) {
	ui.Column(c).FillWidth().Gap(5).Children(func() {
		ui.Text(c, title).FontSize(11).FontWeight(500).TextColor(p.faint)
		lines := strings.Count(text, "\n") + 1
		height := min(float32(lines)*18+20, 260)
		ui.Scroll(c).FillWidth().Height(height).Radius(8).Background(p.code).Border(1, p.lineSoft).Children(func() {
			ui.Text(c, text).Font("Menlo").FontSize(11.5).LineHeight(1.55).TextColor(p.ink.Mix(p.code, 0.12)).Wrap().Selectable().Padding(9, 12)
		})
	})
}

// inlineDiff shows an edit as a unified diff, highlighted in the file's
// language, as high as its lines up to a bound.
func (w *workbench) inlineDiff(c *ui.Context, key, path, oldText, newText string) {
	if w.diffStates == nil {
		w.diffStates = map[string]*editor.DiffState{}
	}
	state := w.diffStates[key]
	if state == nil {
		lang := editor.LanguageFor(path)
		old, nw := editor.NewBuffer(oldText), editor.NewBuffer(newText)
		if lang != nil {
			old.SetLanguage(lang)
			nw.SetLanguage(lang)
		}
		state = &editor.DiffState{Old: old, New: nw, Layout: editor.DiffUnified, FitContent: true, Wrap: true, ContextLines: 3}
		w.diffStates[key] = state
	}
	if state.Old.String() != oldText || state.New.String() != newText {
		delete(w.diffStates, key)
		w.inlineDiff(c, key, path, oldText, newText)
		return
	}
	state.Layout = editor.DiffLayout(w.widgets.diffLayout)
	lines := lineCount(oldText) + lineCount(newText)
	ui.Scroll(c).FillWidth().MaxHeight(min(float32(lines)*19+24, 360)).Radius(8).Border(1, w.widgets.markdownPalette.line).Children(func() {
		editor.DiffView(c, state).FillWidth()
	})
}

// workspaceLockHolder returns another active thread currently running in the
// exact same directory workspace, which serializes writes via LockDirectory.
func (w *workbench) workspaceLockHolder(run api.Run) (string, string) {
	th, ok := w.threadByID(run.ThreadID)
	if !ok || th.Workspace == nil {
		return "", ""
	}
	if th.Workspace.Kind == "worktree" {
		return "", ""
	}
	dir := strings.TrimSpace(th.Workspace.Path)
	if dir == "" {
		return "", ""
	}
	for _, otherRun := range w.runs {
		if otherRun.ID == run.ID || otherRun.ThreadID == run.ThreadID || otherRun.Status != api.RunRunning {
			continue
		}
		otherThread, ok := w.threadByID(otherRun.ThreadID)
		if !ok || otherThread.Workspace == nil || otherThread.Workspace.Kind == "worktree" {
			continue
		}
		if strings.TrimSpace(otherThread.Workspace.Path) == dir {
			return otherThread.ID, nonEmptyString(w.displayTitleFor(otherThread.ID), otherThread.ID)
		}
	}
	return "", ""
}

// liveActivity is the line under a working turn: what it does now, the active
// tool target, silence warning when a tool stalls, and a quick stop action.
func (w *workbench) liveActivity(c *ui.Context, p palette, run api.Run, tools []api.ToolCall) {
	label := w.tr("正在回复", "Replying")
	var runningTool *api.ToolCall
	runningTarget := ""
	phaseWarn := false
	switch {
	case w.stoppingRuns[run.ID]:
		label = w.tr("正在停止…", "Stopping…")
	case w.awaitingApproval(run.ID):
		label = w.tr("需要你的确认", "Needs your confirmation")
	case run.Status == api.RunQueued:
		label = w.tr("等待开始…", "Waiting to start…")
	case run.Status == api.RunWaiting:
		label = w.tr("等待子任务结果", "Waiting on subtasks")
	case run.Telemetry != nil && run.Telemetry.Phase == "waiting_workspace":
		label = w.tr("等待工作区释放", "Waiting for workspace")
		if _, holderTitle := w.workspaceLockHolder(run); holderTitle != "" {
			runningTarget = fmt.Sprintf(w.tr("「%s」正在使用同一目录", "“%s” is using this directory"), holderTitle)
		}
		phaseWarn = true
	case run.Telemetry != nil && run.Telemetry.Compacting:
		label = w.tr("正在压缩上下文…", "Compacting context…")
		phaseWarn = true
	case run.Telemetry != nil && run.Telemetry.Retrying:
		attempt := max(1, run.Telemetry.RetryAttempt)
		label = w.tr(fmt.Sprintf("自动重试 #%d", attempt), fmt.Sprintf("Retrying #%d", attempt))
		if run.Telemetry.RetryWaitSeconds > 0 {
			label += fmt.Sprintf(" (%ds)", run.Telemetry.RetryWaitSeconds)
		}
		phaseWarn = true
	default:
		for i := len(tools) - 1; i >= 0; i-- {
			if tools[i].Status == "running" {
				tool := tools[i]
				runningTool = &tool
				view := w.toolView(tool)
				label = "正在" + view.verb
				runningTarget = view.target
				if view.path != "" && (view.kind == toolRead || view.kind == toolEdit || view.kind == toolWrite || view.kind == toolList) {
					runningTarget = w.relativePath(run.ThreadID, view.path)
				}
				break
			}
		}
		if label == w.tr("正在回复", "Replying") && run.Output == "" && run.Reasoning != "" {
			label = w.tr("正在思考", "Thinking")
		}
		if label == w.tr("正在回复", "Replying") && run.Output == "" && run.Reasoning == "" && len(tools) == 0 {
			if _, holderTitle := w.workspaceLockHolder(run); holderTitle != "" {
				label = w.tr("等待工作区释放", "Waiting for workspace")
				runningTarget = fmt.Sprintf(w.tr("「%s」正在使用同一目录", "“%s” is using this directory"), holderTitle)
				phaseWarn = true
			}
		}
	}
	if label == w.tr("需要你的确认", "Needs your confirmation") && !w.stoppingRuns[run.ID] {
		// The approval card below carries this state, with its actions.
		return
	}
	var toolSilent time.Duration
	if runningTool != nil && !runningTool.UpdatedAt.IsZero() && run.Status == api.RunRunning && !w.stoppingRuns[run.ID] {
		toolSilent = c.Now().Sub(runningTool.UpdatedAt)
	}
	silenceWarn := toolSilent >= 15*time.Second && toolSilent < 48*time.Hour
	silenceStop := toolSilent >= 30*time.Second && toolSilent < 48*time.Hour
	ui.Row(c).Gap(8).AlignItems(ui.Center).Height(26).Padding(0, 8).Role(ui.RoleStatus).Children(func() {
		dotColor := p.accent
		if silenceWarn || phaseWarn {
			dotColor = p.warning
		}
		activityIndicator(c, dotColor).Size(11, 11).Shrink(0)
		ui.Text(c, label).FontSize(12.5).FontWeight(500).TextColor(p.muted).NoWrap().Shrink(0)
		if runningTarget != "" {
			ui.Text(c, runningTarget).Font("Menlo").FontSize(11.5).TextColor(p.faint).SingleLine().Ellipsis("…").MaxWidth(320).Shrink(1).MinWidth(0)
		}
		if !run.CreatedAt.IsZero() && run.Status == api.RunRunning {
			elapsed := c.Now().Sub(run.CreatedAt)
			if elapsed >= time.Second && elapsed < 48*time.Hour { // a skewed clock says nothing useful
				ui.Text(c, formatElapsed(elapsed)).FontSize(12).TextColor(p.faint).NoWrap().Shrink(0)
			}
			if elapsed >= 0 {
				c.After(time.Second - elapsed%time.Second)
			} else {
				c.After(time.Second)
			}
		}
		if silenceWarn {
			ui.Text(c, fmt.Sprintf(w.tr("· 静默 %s", "· silent %s"), formatElapsed(toolSilent))).FontSize(11.5).TextColor(p.warning).NoWrap().Shrink(0)
			c.After(time.Second - toolSilent%time.Second)
		}
		if silenceStop {
			stop := quietButton(c, p, "live-stop:"+run.ID, w.tr("停止", "Stop")).Height(22).TextColor(p.danger)
			if stop.Clicked() {
				w.stopThread(run.ThreadID)
			}
		}
	})
}

func formatElapsed(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%d 秒", max(seconds, 1))
	}
	if seconds < 3600 {
		return fmt.Sprintf("%d 分 %d 秒", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", seconds/3600, seconds%3600/60)
}

// turnMeta follows a finished reply: its outcome, time and model, copy, and
// the files the turn changed.
func (w *workbench) turnMeta(c *ui.Context, p palette, run api.Run, tools []api.ToolCall) {
	files := w.changedFiles(tools)
	if len(files) > 0 {
		w.changedFilesCard(c, p, run, files)
	}
	row := ui.Row(c).Gap(4).AlignItems(ui.Center).Height(26).Padding(0, 0, 0, 2)
	hovered := row.Hovered()
	latest := false
	if last, ok := w.latestRun(run.ThreadID); ok && last.ID == run.ID {
		latest = true
	}
	row.Children(func() {
		w.runStatusView(c, p, run)
		if run.Model != "" {
			ui.Text(c, "·").FontSize(11.5).TextColor(p.faint)
			ui.Text(c, w.modelName(run.Model)).FontSize(11.5).TextColor(p.faint).SingleLine().Ellipsis("…").MaxWidth(220).Shrink(1).MinWidth(0)
		}
		ui.Box(c).Width(6).Shrink(0)
		// Icon actions, as the workbench's: always there for the latest turn, revealed
		// by hover or keyboard focus on older ones so history stays calm.
		actions := ui.Row(c).Gap(1).AlignItems(ui.Center)
		if !latest && !hovered && !actions.FocusWithin() {
			actions.Opacity(0)
		}
		actions.Children(func() {
			small := func(key, label string, icon *ui.SVG) ui.Element {
				b := iconButton(c, p, key, label, icon).Size(26, 26).Padding(6).Radius(7)
				if !b.Hovered() {
					b.TextColor(p.faint)
				}
				return b
			}
			// Copy confirms in place: the icon turns into a check for a moment.
			copied := w.widgets.copiedRun == run.ID && c.Now().Sub(w.widgets.copiedAt) < 1500*time.Millisecond
			copyGlyph, copyLabel := copyIcon, w.tr("复制回复", "Copy reply")
			if copied {
				copyGlyph = checkIcon
				c.After(1500*time.Millisecond - c.Now().Sub(w.widgets.copiedAt))
			}
			if run.Output != "" {
				copyButton := small("copy-reply", copyLabel, copyGlyph)
				if copied {
					copyButton.TextColor(p.success).Label(w.tr("已复制", "Copied")).Tooltip(w.tr("已复制", "Copied"))
				}
				if copyButton.Clicked() {
					c.WriteClipboard(stripCopyObjects(run.Output))
					w.widgets.copiedRun, w.widgets.copiedAt = run.ID, c.Now()
				}
			}
			if run.Output != "" && small("quote-reply", w.tr("引用回复", "Quote reply"), quoteIcon).Clicked() {
				w.quoteReply(run.Output)
			}
			if w.hasThread(run.ThreadID) {
				fork := small("fork-turn", w.tr("从本轮分叉", "Fork from this turn"), forkIcon).Tooltip(w.tr("从本轮分叉为新会话", "Fork a new thread from this turn"))
				fork.Disabled(w.sessions.forking[run.ThreadID+":"+run.ID])
				if fork.Clicked() {
					w.forkThread(run.ThreadID, run.ID)
				}
				if run.Status.Terminal() && run.ThreadID == w.selected {
					_, hasActive := w.activeRun(run.ThreadID)
					rewind := small("rewind-turn", w.tr("从本轮回退", "Rewind from this turn"), rollbackIcon).Tooltip(w.tr("从本轮回退对话或工作区文件", "Rewind conversation or files from this turn"))
					rewind.Disabled(hasActive)
					if rewind.Clicked() {
						w.openRewindDialog(run.ID)
					}
				}
			}
		})
	})
}

func (w *workbench) modelName(model string) string {
	for _, item := range w.models {
		if (item.Key() == model || strings.HasPrefix(model, item.Key()+":")) && item.Name != "" {
			return item.Name
		}
	}
	return shortModel(model)
}

func (w *workbench) changedFilesCard(c *ui.Context, p palette, run api.Run, files []changedFile) {
	added, removed := 0, 0
	for _, f := range files {
		added += f.added
		removed += f.removed
	}
	ui.Column(c.Key("changed-files")).FillWidth().Radius(11).Border(1, p.lineSoft).Background(p.raised).Children(func() {
		ui.Row(c).Height(36).Padding(0, 8, 0, 12).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
			ui.Icon(c, editIcon).FontSize(13).TextColor(p.muted)
			ui.Text(c, fmt.Sprintf(w.tr("已修改 %d 个文件", "%d files changed"), len(files))).FontSize(12.5).FontWeight(600).TextColor(p.ink).Grow(1)
			diffCounts(c, p, added, removed)
			if quietButton(c, p, "review-changes", w.tr("查看变更", "Review changes")).Height(24).Clicked() {
				w.widgets.turnDiff = run.ID
			}
		})
		ui.Column(c).Padding(4, 4).Children(func() {
			for i, f := range files {
				if i == 8 {
					ui.Text(c, fmt.Sprintf(w.tr("还有 %d 个文件", "%d more files"), len(files)-8)).FontSize(11).TextColor(p.muted).Padding(4, 8)
					break
				}
				entry := quietButton(c, p, "file:"+f.path, "").Label(w.tr("查看 ", "View ")+f.path).FillWidth().Padding(3, 8).Height(26)
				entry.Children(func() {
					ui.Icon(c, fileIcon).FontSize(12)
					ui.Text(c, w.relativePath(run.ThreadID, f.path)).Font("Menlo").FontSize(11).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
					diffCounts(c, p, f.added, f.removed)
				})
				if entry.Clicked() {
					w.widgets.turnDiff = run.ID
				}
			}
		})
	})
}

// turnDiffDialog reviews every edit of one turn, file by file.
func (w *workbench) turnDiffDialog(c *ui.Context, p palette) {
	if w.widgets.turnDiff == "" {
		return
	}
	open := true
	runID := w.widgets.turnDiff
	var run api.Run
	for _, r := range w.runs {
		if r.ID == runID {
			run = r
		}
	}
	files := w.changedFiles(w.runTools(runID))
	width, height := c.Size()
	ui.DialogBase(c, &open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000066"))
		panel.Width(min(1040, width-36)).Height(min(760, height-48)).Radius(14).Background(p.surface).Border(1, p.line)
		ui.Column(c).Fill().Children(func() {
			ui.Row(c).Height(48).Padding(0, 12, 0, 16).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
				ui.Text(c, fmt.Sprintf(w.tr("本轮变更 · %d 个文件", "Turn changes · %d files"), len(files))).FontSize(14).Bold().Grow(1)
				ui.Segmented(c, &w.widgets.diffLayout, w.tr("并排", "Split"), w.tr("统一", "Unified")).Label(w.tr("差异布局", "Diff layout"))
				if iconButton(c, p, "close-turn-diff", w.tr("关闭变更", "Close changes"), closeIcon).Clicked() {
					open = false
				}
			})
			ui.Scroll(c).Grow(1).Children(func() {
				ui.Column(c).Padding(14, 16).Gap(16).Children(func() {
					for _, f := range files {
						ui.Column(c.Key("file:" + f.path)).Gap(6).Children(func() {
							ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
								ui.Text(c, w.relativePath(run.ThreadID, f.path)).Font("Menlo").FontSize(12).Bold().Grow(1).Selectable()
								diffCounts(c, p, f.added, f.removed)
							})
							for _, tool := range f.tools {
								view := w.toolView(tool)
								if view.kind == toolWrite {
									w.inlineDiff(c.Key("w:"+tool.ID), tool.ID+":write", view.path, "", view.content)
								}
								for i, edit := range view.edits {
									w.inlineDiff(c.Key("e:"+tool.ID+strconv.Itoa(i)), tool.ID+":"+strconv.Itoa(i), view.path, edit.oldText, edit.newText)
								}
							}
						})
					}
				})
			})
		})
	})
	if !open {
		w.widgets.turnDiff = ""
	}
}
