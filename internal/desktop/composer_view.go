package desktop

import (
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// composerBox is the workbench's ChatComposer: a single rounded card holding only the
// text and its footer row (attach + Plan on the left; the Pi model, the
// thinking effort and the primary action on the right), with the
// ContextStrip (host, project, branch and permissions) beneath it.
// Cards above it surface only while something needs attention.
func (w *workbench) composerBox(c *ui.Context, p palette, home bool) {
	target := w.selected
	comp := w.composerFor(target)
	draft := comp.draft
	run, active := w.activeRun(target)
	_, height := c.Size()
	w.ensureModelDetails()
	w.subagentComposerBar(c, p, target)
	w.childApprovalsView(c, p, target)
	w.inputsView(c, p)
	w.queuedView(c, p)
	w.retryRows(c, p, target)
	w.filePickerView(c, p, target, comp.draft)
	w.skillPickerView(c, p, target, comp.draft)
	draft = comp.draft // a picked candidate is the value the input takes now
	if !home && height >= 560 {
		w.taskOrchestrationCard(c, p, target)
	}
	// The composer is the one raised surface on the canvas: a lifted radius-16
	// card whose border warms while it holds focus, and turns accent while
	// files are dragged over it.
	box := ui.Column(c.Key("composer")).FillWidth().Padding(4).Gap(2).Radius(16).Background(p.raised).Border(1, p.line)
	if p.dark {
		box.Shadow(0, 8, 28, 0, ui.Hex("#00000059"))
	} else {
		box.Shadow(0, 4, 20, 0, ui.Hex("#1D1D1F12"))
	}
	if box.FocusWithin() {
		box.BorderColor(p.muted.Alpha(0.36))
	}
	if box.FileDragOver() {
		box.BorderColor(p.accent)
	}
	if files := box.DroppedFiles(); len(files) > 0 && target != "" {
		w.importAttachments(target, files)
	}
	box.ObserveInput(func(ev ui.InputEvent) { w.observeComposerInput(target, ev) })
	box.Children(func() {
		w.attachmentView(c, p, comp)
		draft = comp.draft // removing an attachment may strip its inline chip from comp.draft
		placeholder := w.tr3("问点什么… @ 文件、$ 技能、/ 命令", "Ask anything… @ files, $ skills, / commands", "何でも聞いてください… @ ファイル、$ スキル、/ コマンド")
		if home {
			placeholder = w.tr3("问点什么… @ 文件、$ 技能、/ 命令", "Ask anything… @ files, $ skills, / commands", "何でも聞いてください… @ ファイル、$ スキル、/ コマンド")
		}
		// Short windows give the conversation the room: one resting line.
		minLines, maxLines := 2, 8
		if height < 700 {
			minLines, maxLines = 1, 4
		}
		if height < 560 {
			maxLines = 2
		}
		ui.Box(c.Key("draft-shell")).FillWidth().Children(func() {
			ui.Box(c.Key("draft-chip-bg")).Absolute().Fill().PassThrough().Draw(func(painter *ui.Painter, r ui.Rect) {
				paintComposerChips(painter, r, comp, p)
			})
			field := ui.TextAreaBase(c.Key("draft:"+target), &draft).Lines(minLines, maxLines).FillWidth().Padding(10, 12, 6).FontSize(14).LineHeight(1.5)
			field.Placeholder(placeholder).Label(w.tr("消息输入框", "Message")).Bind(&w.widgets.composerFocus)
			field.TrackLines(&comp.chipLines).TrackScroll(&comp.chipScroll)
			newlineKey := platformPrimaryNewlineKey(runtime.GOOS)
			description := w.tr("输入消息；Enter 换行，Ctrl + Enter 发送", "Type a message; Enter for a new line, Ctrl + Enter to send")
			if w.widgets.sendWithEnter {
				description = fmt.Sprintf(w.tr("输入消息；Enter 发送，%s / Shift + Enter 换行", "Type a message; Enter to send, %s or Shift + Enter for a new line"), newlineKey)
			}
			description += w.tr("；/ 调用技能，@ 引用文件", "; / runs a skill, @ mentions a file")
			if active {
				if w.canSteer(run, active) && w.widgets.steerMode {
					description += w.tr("；运行中默认调整当前任务", "; while running it steers the current turn")
				} else {
					description += w.tr("；运行中默认加入队列", "; while running it joins the queue")
				}
			}
			field.Description(description)
			if !w.widgets.composerMounted {
				field.AutoFocus()
				w.widgets.composerMounted = true
			}
			if target == "" {
				field.Disabled(true).Description(w.tr("正在连接本地服务", "Connecting to the local service"))
			}
			if comp.feedback.Level == noticeError {
				field.Error(comp.feedback.Text)
			}
			if field.FocusLeft() && (comp.composing || comp.compositionEnding) {
				comp.composing, comp.compositionEnding = false, true
				comp.lastCompositionSet = time.Now()
			}
			// Save the bound value before a send callback can clear the composer.
			// Writing the stale local value afterwards would resurrect a sent draft.
			if draft != comp.draft {
				// A pasted thread link collapses into its "@title" mention, as
				// T3 Code's composer chips; the caret keeps its place.
				_, caret := field.TextAnchorCaret()
				if collapsed, at, changed := comp.collapseThreadLinks(draft, caret); changed {
					draft = collapsed
					comp.caretAfter, comp.caretRestore = at, true
					c.After(time.Millisecond)
				}
				w.setDraft(target, draft)
			}
			if len(comp.mentions) > 0 || strings.Contains(draft, "/skill:") {
				field.TextRanges(comp.mentionRanges(p)...)
			}
			field.OnPaste(func(pasted string) bool { return w.handleComposerPaste(target, pasted) })
			field.OnTextKey(func(k ui.TextKey) bool { return w.composerKey(target, k) })
			// Cmd/Ctrl+Enter queued a line break between frames, where the
			// field's selection is unreachable: insert it here and restore caret.
			w.applyPendingComposerNewline(c, field, comp, target, &draft)
		})
		footer := ui.Row(c.Key("composer-footer")).FillWidth().MinWidth(0).Padding(2, 6, 4, 6).Gap(6).AlignItems(ui.Center)
		w.widgets.composerFooterW = footer.Bounds().W
		footer.Children(func() {
			w.addMenu(c, p, comp, target, draft)
			w.composerContextRing(c, p, target)
			if target != "" {
				w.planToggle(c, p, comp)
			}
			ui.Spacer(c)
			w.permissionChip(c, p)
			w.modelEffortPill(c, p)
			w.primaryActions(c, p, comp, target, draft, run, active)
		})
	})
	w.contextStrip(c, p)
	if !home && height >= 560 && w.features.panel == "" {
		w.composerStatusFooter(c, p, target, run, active)
	}
	if line := w.composerLine(comp, active, run); line.valid() {
		ui.Column(c).Padding(4, 4, 0).Children(func() { w.noticeLine(c, p, line) })
	}
}

// addMenu is the footer's + : add files or images, or hand the draft to a
// subtask of the latest turn.
func (w *workbench) addMenu(c *ui.Context, p palette, comp *composer, target, draft string) {
	attach := iconButton(c, p, "add-attachment", w.tr("添加附件", "Add attachment"), lucide("plus")).Size(28, 28).Padding(6).Radius(14)
	attach.Description(w.tr("选择或拖入图片与文件，每个最多 8 MiB；也可派出子任务", "Choose or drop images and files, up to 8 MiB each; or delegate a subtask"))
	if target == "" {
		attach.Disabled(true)
		return
	}
	parent, hasParent := w.latestRun(target)
	canDelegate := hasParent && parent.Status != api.RunCancelled && parent.Status != api.RunFailed && parent.Status != api.RunInterrupted
	attach.PopoverMenuButton(func(m *ui.Menu) {
		if m.Item(w.tr("添加文件、图片或 PDF…", "Add files, images, or PDFs…")).Disabled(comp.uploading).Chosen() {
			w.pickAttachments()
		}
		if m.Item(w.tr("从剪贴板粘贴图片 / PDF", "Paste image or PDF from clipboard")).Disabled(comp.uploading).Chosen() {
			w.pasteFromClipboardMenu(target)
		}
		if canDelegate {
			blocked := comp.pending != nil || comp.uploading || len(comp.attachments) > 0 || strings.TrimSpace(draft) == ""
			if m.Item(w.tr("派出子任务", "Delegate as subtask")).Disabled(blocked).Chosen() {
				w.setDraft(target, draft)
				w.delegateDraft(target)
				w.widgets.composerFocus.Focus()
			}
		}
	}, ui.PopoverMenuButtonOptions{At: ui.AnchorTopStart, Self: ui.AnchorBottomStart})
}

// progressRing draws a crisp circular progress arc natively with ui.Painter.
func progressRing(c *ui.Context, size, fraction float32, track, tone ui.Color) ui.Element {
	frac := max(float32(0), min(float32(1), fraction))
	return ui.Box(c).Size(size, size).Shrink(0).Draw(func(pt *ui.Painter, r ui.Rect) {
		strokeW := float32(2.0)
		inset := strokeW / 2
		ringRect := ui.Rect{X: r.X + inset, Y: r.Y + inset, W: r.W - strokeW, H: r.H - strokeW}
		pt.Stroke(ringRect, track, ringRect.W/2, strokeW)
		if frac <= 0 {
			return
		}
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		rad := (min(r.W, r.H) - strokeW) / 2
		steps := max(6, int(math.Ceil(float64(frac)*24)))
		pts := make([][2]float32, steps+1)
		for i := 0; i <= steps; i++ {
			angle := -math.Pi/2 + (float64(frac)*2*math.Pi*float64(i))/float64(steps)
			pts[i] = [2]float32{
				cx + rad*float32(math.Cos(angle)),
				cy + rad*float32(math.Sin(angle)),
			}
		}
		pt.StrokePolyline(pts, strokeW, tone)
	})
}

// composerContextRing shows the thread's real context window fill as a compact
// ring pill inside the composer footer left. Hidden whenever real telemetry or
// context window size is unavailable.
func (w *workbench) composerContextRing(c *ui.Context, p palette, threadID string) {
	_, _, tel, ctxWindow := w.threadTelemetry(threadID)
	if tel == nil || tel.LastContextTokens <= 0 || ctxWindow <= 0 {
		return
	}
	pct := int(min(int64(999), (tel.LastContextTokens*100)/ctxWindow))
	frac := float32(tel.LastContextTokens) / float32(ctxWindow)
	tone := p.accent
	if pct >= 85 {
		tone = p.danger
	} else if pct >= 70 {
		tone = p.warning
	}
	pillBox := ui.Row(c.Key("composer-ctx-ring")).Height(24).Padding(0, 8, 0, 6).Gap(5).Radius(12).
		Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Shrink(0).
		Tooltip(fmt.Sprintf("%s / %s tokens", compactCount64(tel.LastContextTokens), compactCount64(ctxWindow)))
	pillBox.Children(func() {
		progressRing(c, 12, frac, p.line, tone)
		ui.Text(c, fmt.Sprintf("%d%%", pct)).FontSize(11).FontWeight(500).FontFeatures("tnum").TextColor(p.muted).NoWrap()
	})
}

// composerFooterWidth is the composer footer's last laid-out width; before
// the first layout it assumes a roomy composer.
func (w *workbench) composerFooterWidth() float32 {
	if w.widgets.composerFooterW <= 0 {
		return 760
	}
	return w.widgets.composerFooterW
}

// modelEffortPill unifies the model selector and effort indicator into one
// cohesive Stardust pill while keeping both controls independently accessible.
func (w *workbench) modelEffortPill(c *ui.Context, p palette) {
	base, _ := w.modelSelection()
	showEffort := w.modelSwitchable(w.selected) && base != ""
	// The pill gives way before the composer's send button does: the model
	// name ellipsizes and, in a narrow column, the effort keeps only its dot.
	ui.Row(c.Key("model-effort-pill")).Height(26).Padding(0, 2).Gap(0).Radius(13).
		Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Shrink(1).MinWidth(0).Children(func() {
		w.modelPicker(c, p)
		if showEffort {
			ui.Box(c).Size(1, 12).Background(p.line).Shrink(0)
			w.thinkingPicker(c, p)
		}
	})
}

// planToggle is the minimal Plan capsule: off it is a quiet outline, on it
// takes the accent. It changes only how the next send is framed.
func (w *workbench) planToggle(c *ui.Context, p palette, comp *composer) {
	label := w.tr("规划", "Plan")
	toggle := ui.Button(c.Key("plan-toggle"), "").Label(w.tr("规划模式", "Plan mode")).Role(ui.RoleSwitch).Checked(comp.plan).
		Height(26).Padding(0, 9).Radius(13).Border(1, p.line).Background(ui.Transparent).TextColor(p.muted).Shrink(0)
	toggle.Tooltip(w.tr("规划模式：先给出计划，不修改文件或执行命令", "Plan mode: propose a plan first, without editing files or running commands"))
	switch {
	case comp.plan:
		toggle.Background(p.accent.Alpha(0.14)).BorderColor(p.accent.Alpha(0.55)).TextColor(p.accentInk)
	case toggle.Hovered():
		toggle.TextColor(p.ink)
	}
	compact := w.composerFooterWidth() < 440
	if compact {
		toggle.Padding(0, 7).Tooltip(label + " · " + w.tr("先给出计划，不修改文件或执行命令", "propose a plan first, without editing files or running commands"))
	}
	toggle.Children(func() {
		ui.Icon(c, planIcon).FontSize(12)
		if !compact {
			ui.Text(c, label).FontSize(11).NoWrap()
		}
	})
	if toggle.Clicked() {
		comp.plan = !comp.plan
		w.widgets.composerFocus.Focus()
	}
}

// permissionChip shows and switches the tool permission new threads start
// with: auto, auto edits, or ask. It sits as a subtle tinted pill inside the
// composer footer.
func (w *workbench) permissionChip(c *ui.Context, p palette) {
	tier := approvalTierFor(w.defaults.ApprovalPolicy)
	tone := ui.Hex("#2563EB")
	if p.dark {
		tone = ui.Hex("#60A5FA")
	}
	switch tier.policy {
	case "auto-accept-edits":
		tone = p.accentInk
	case "ask":
		tone = p.warning
	}
	chip := ui.Button(c.Key("permission-chip"), "").Label(w.tr("工具权限", "Permissions")).
		Height(26).Padding(0, 9).Radius(13).Shrink(0).
		Background(tone.Alpha(0.11)).Border(1, tone.Alpha(0.22)).TextColor(tone)
	open := ui.Local(chip, "permission-open", func() bool { return false })
	if *open {
		chip.Expanded(true).Background(tone.Alpha(0.18))
	} else if chip.Hovered() {
		chip.Background(tone.Alpha(0.17))
	}
	chip.Tooltip(w.tr("新会话工具权限：", "New-thread permissions: ") + w.approvalLabel(tier))
	chip.Description(w.tr("新会话工具权限：", "New-thread permissions: ") + w.approvalLabel(tier))
	chatW := w.chatColumnWidth(c)
	chip.Children(func() {
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			icon := shieldIcon
			if tier.policy == "auto" {
				icon = shieldOffIcon
			}
			ui.Icon(c, icon).FontSize(11.5).TextColor(tone)
			if chatW >= 460 {
				ui.Text(c, w.approvalShort(tier.policy)).FontSize(11.5).FontWeight(500).TextColor(tone).NoWrap()
			}
		})
	})
	if chip.Clicked() {
		*open = !*open
	}
	options := make([]kitOption, len(approvalTiers))
	for i, option := range approvalTiers {
		options[i] = kitOption{ID: option.policy, Label: w.approvalLabel(option), Detail: w.approvalDetail(option)}
	}
	kitMenu(c, p, chip, open, w.tr("工具权限", "Permissions"), options, tier.policy, false, func(id string) {
		if id != w.defaults.ApprovalPolicy {
			w.defaults.ApprovalPolicy = id
			w.saveState()
		}
	})
}

// primaryActions is the footer's right end: an emerald send while idle; while
// running, a clear stop, or queue/steer with a small stop beside it.
func (w *workbench) primaryActions(c *ui.Context, p palette, comp *composer, target, draft string, run api.Run, active bool) {
	if w.editingQueue(target) {
		w.queueComposerActions(c, p, comp, target)
		return
	}
	hasContent := strings.TrimSpace(draft) != "" || len(comp.attachments) > 0
	canSteer := w.canSteer(run, active)
	if active && !canSteer {
		w.widgets.steerMode = false
	}
	if active && hasContent {
		label, icon := w.tr("排队", "Queue"), queueIcon
		if w.widgets.steerMode && canSteer {
			label, icon = w.tr("追加信息", "Steer"), steerIcon
		}
		mode := kitSelectTrigger(c, p, "run-input-mode", w.tr("运行中发送方式", "While running"), false).Height(26).Padding(0, 7).Radius(13)
		modeOpen := ui.Local(mode, "run-mode-open", func() bool { return false })
		if *modeOpen {
			mode.Expanded(true)
		}
		mode.Description(w.tr("选择草稿的默认发送方式；按住 ⌘ / Ctrl 点击主按钮可使用另一种方式", "Default for drafts while running; ⌘ / Ctrl-click the main button for the other"))
		mode.Children(func() {
			ui.Icon(c, icon).FontSize(13)
			ui.Text(c, label).FontSize(11).NoWrap()
			ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint)
		})
		if mode.Clicked() {
			*modeOpen = !*modeOpen
		}
		current := "queue"
		if w.widgets.steerMode {
			current = "steer"
		}
		kitMenu(c, p, mode, modeOpen, w.tr("运行中发送方式", "While running"), []kitOption{
			{ID: "queue", Label: w.tr("加入队列", "Queue"), Detail: w.tr("当前回复结束后再发送", "Send after this reply finishes")},
			{ID: "steer", Label: w.tr("追加信息", "Steer message"), Detail: w.tr("插进正在进行的回复", "Add to the reply in progress"), Disabled: !canSteer},
		}, current, false, func(id string) {
			w.widgets.steerMode = id == "steer"
			w.saveState()
		})
	}
	if active {
		stopLabel := w.tr("停止", "Stop")
		if w.stoppingRuns[run.ID] {
			stopLabel = w.tr("正在停止", "Stopping")
		}
		var stop ui.Element
		if hasContent {
			stop = iconButton(c, p, "stop", stopLabel, composerStopIcon).Size(26, 26).Padding(5).Radius(13)
		} else {
			stop = ui.Button(c.Key("stop"), "").Label(stopLabel).Tooltip(stopLabel).Size(30, 30).Padding(0).Radius(15).Shrink(0).
				Border(0, p.line).Background(p.accent).TextColor(ui.Hex("#FFFFFF"))
			if stop.Hovered() {
				stop.Background(p.danger).TextColor(ui.Hex("#FFFFFF"))
			}
			stop.Children(func() {
				ui.Box(c).Size(10, 10).Radius(2.5).Background(ui.Hex("#FFFFFF"))
			})
		}
		stop.Description(w.tr("停止当前任务，并取消尚未开始的排队消息", "Stop the current turn and cancel queued messages that have not started"))
		if w.stoppingRuns[run.ID] {
			stop.Disabled(true)
		}
		if stop.Clicked() {
			w.stopActiveRun()
		}
	}
	if active && !hasContent {
		return
	}
	label, key, icon := w.tr("发送", "Send"), "", composerSendIcon
	steer := false
	if active {
		steer = followUpSteer(w.widgets.steerMode, canSteer, c.Modifiers()&ui.Cmd != 0)
		label, key, icon = w.tr("加入队列", "Queue"), "queue:"+target, queueIcon
		if steer {
			label, key, icon = w.tr("追加信息", "Steer message"), "steer:"+target, steerIcon
		}
	}
	send := ui.PrimaryButton(c.Key("primary-action"), "").Label(label).Tooltip(label).Size(30, 30).Padding(7).Radius(15).Shrink(0)
	if !hasContent {
		// Nothing to send yet: a neutral well, not a dimmed accent.
		send.Background(p.selected).TextColor(p.faint)
	}
	send.Description(label)
	if active {
		send.Description(label + w.tr("；按住 ⌘ / Ctrl 点击可切换排队与调整", "; ⌘ / Ctrl-click to switch between queue and steer"))
	}
	send.Children(func() { ui.Icon(c, icon).FontSize(16) })
	if comp.pending != nil || comp.uploading || !hasContent || key != "" && w.controlBusy(key) || steer && comp.steering != nil {
		send.Disabled(true)
	}
	if send.Clicked() {
		w.setDraft(target, draft)
		if active {
			w.queueDraft(target, followUpSteer(w.widgets.steerMode, canSteer, send.ClickModifiers()&ui.Cmd != 0))
		} else {
			w.send(target)
		}
		w.widgets.composerFocus.Focus()
	}
}

// retryRows surface unconfirmed queue/steer requests above the composer.
func (w *workbench) retryRows(c *ui.Context, p palette, target string) {
	for _, key := range w.retryKeys(target) {
		action := w.controls[key]
		label := w.tr("重试排队", "Retry queue")
		if action.command.Kind == api.SteerRun {
			label = w.tr("重试调整", "Retry steer")
		}
		ui.Row(c).Padding(0, 6, 6).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, w.tr("待确认：", "Unconfirmed: ")+firstLine(action.command.Message)).FontSize(11).TextColor(p.warning).SingleLine().Ellipsis("…").Grow(1)
			if quietButton(c, p, "retry:"+key, label).Tooltip(w.tr("核对原请求的结果；不会发送当前草稿", "Check the original request; the current draft is not sent")).Clicked() {
				w.dispatchControl(key, label, w.tr("原请求结果已确认。", "Original request confirmed."), action.command, false)
			}
		})
	}
}

// followUpSteer resolves the default running-turn action and its one-shot
// alternate. Unsupported steering always falls back to the independent queue.
func followUpSteer(defaultSteer, canSteer, alternate bool) bool {
	if !canSteer {
		return false
	}
	if alternate {
		return !defaultSteer
	}
	return defaultSteer
}

func (w *workbench) canSteer(run api.Run, active bool) bool {
	return active && run.Status == api.RunRunning && run.AttemptID != "" && w.piThread(w.selected)
}

func composerPopoverWidth(windowWidth, preferred float32) float32 {
	return max(float32(1), min(preferred, windowWidth-24))
}

// modelPicker is the composer's [Pi] model button and its searchable list.
// The list carries no catalog notes: only a failure to load or to switch
// shows, and Reload sits quietly in the list's header.
func (w *workbench) modelPicker(c *ui.Context, p palette) {
	width, height := c.Size()
	modelWidth, modelTextWidth := float32(240), float32(170)
	switch footer := w.composerFooterWidth(); {
	case width < 760 || footer < 440:
		modelWidth, modelTextWidth = 150, 92
	case footer < 560:
		modelWidth, modelTextWidth = 190, 124
	}
	label := w.modelLabel()
	button := quietButton(c, p, "model-picker", "").Label(w.tr("选择模型", "Choose model")).MaxWidth(modelWidth).Height(22).Padding(0, 7).Radius(11).
		Shrink(1).MinWidth(44).Expanded(w.widgets.modelPickerOpen).Bind(&w.widgets.modelFocus)
	button.Description(w.tr("当前模型：", "Current model: ") + label + w.tr("。打开 Pi 模型列表，搜索并选择后续消息使用的模型", ". Open the Pi model list to choose the model for later messages"))
	button.Tooltip(w.tr("打开模型列表", "Open model list"))
	button.Children(func() {
		ui.Row(c).Gap(5).AlignItems(ui.Center).MinWidth(0).Children(func() {
			w.providerMark(c, p.accent, 12.5, w.threadProvider(w.selected))
			ui.Text(c, label).FontSize(11.5).FontWeight(500).TextColor(p.ink).MaxWidth(modelTextWidth).Shrink(1).MinWidth(0).SingleLine().Ellipsis("…")
		})
	})
	if w.proxyEnvironmentID() != "" {
		button.Disabled(true).Tooltip(w.tr("远端模型目录请在目标机器中查看", "Open the target environment to view its model catalog"))
		return
	}
	if !w.modelSwitchable(w.selected) {
		button.Disabled(true).Tooltip(w.tr("离线演示不调用 AI 模型", "The offline demo does not call an AI model"))
		return
	}
	if button.Clicked() {
		w.widgets.modelPickerOpen = !w.widgets.modelPickerOpen
		if w.widgets.modelPickerOpen {
			w.widgets.projectPickerOpen = false
			w.ensureModels(false)
			if _, ok := w.service.(api.WorkbenchService); ok {
				if w.features.catalog == nil || w.features.catalogPath != w.modelWorkspace() {
					w.loadModels()
				} else if base, _ := w.modelSelection(); base != "" && base != w.features.thinkingModel && !w.features.thinkingLoading {
					w.loadThinking()
				}
			}
		}
	}
	ui.PopoverBase(c, button, &w.widgets.modelPickerOpen, func(panel ui.Element) {
		shortWindow := height < 520
		panel.AttachTo(button, ui.AnchorTopEnd, ui.AnchorBottomEnd).Margin(0, 0, 8).
			Width(composerPopoverWidth(width, 412)).MaxHeight(max(float32(220), height-64)).Padding(10).Radius(16).
			Background(p.raised).Border(1, p.line).Shadow(0, 16, 44, 0, ui.Hex("#00000059"))
		if panel.Shortcut(0, ui.KeyEscape) {
			w.widgets.modelPickerOpen = false
			w.widgets.modelFocus.Focus()
			return
		}
		ui.Column(c).Gap(7).Children(func() {
			// Top Spotlight search row
			searchShell := ui.Row(c.Key("model-search-shell")).FillWidth().Height(32).Padding(0, 8, 0, 2).Gap(4).
				Radius(10).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center)
			searchShell.Children(func() {
				search := ui.SearchField(c, &w.widgets.modelSearch).Label(w.tr("搜索模型", "Search models")).
					Placeholder(w.tr3("搜索模型…", "Search models…", "モデルを検索…")).
					Grow(1).MinWidth(0).Height(28).Padding(0, 4).Radius(8).Border(0, ui.Transparent).Background(ui.Transparent).AutoFocus()
				if search.Shortcut(0, ui.KeyDown) {
					w.widgets.modelOptionsFocus.Focus(c)
				}
				if strings.TrimSpace(w.widgets.modelSearch) == "" {
					ui.Text(c, "⌘K").Font("Menlo").FontSize(10).TextColor(p.faint).Shrink(0).NoWrap().AccessHidden(true)
				}
			})

			// Horizontal provider filter pills derived from real catalog
			providerTabs := w.modelProviderTabs()
			activeFilter := w.widgets.modelProviderFilter
			filterValid := activeFilter == "" || activeFilter == "__featured__"
			if !filterValid {
				for _, tab := range providerTabs {
					if tab.id == activeFilter {
						filterValid = true
						break
					}
				}
				if !filterValid {
					activeFilter = ""
					w.widgets.modelProviderFilter = ""
				}
			}
			if !shortWindow && len(providerTabs) > 1 {
				ui.ScrollHorizontal(c.Key("model-provider-tabs")).FillWidth().Children(func() {
					ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
						for _, tab := range providerTabs {
							tab := tab
							selected := activeFilter == tab.id
							pillBtn := ui.Button(c.Key("model-prov:"+tab.id), "").Label(tab.label).
								Height(24).Padding(0, 10).Radius(12).Border(1, ui.Transparent).Background(ui.Transparent).TextColor(p.muted).Shrink(0)
							if selected {
								pillBtn.Background(p.ink).TextColor(p.canvas)
							} else if pillBtn.Hovered() {
								pillBtn.Background(p.soft).TextColor(p.ink)
							}
							pillBtn.Children(func() {
								weight := 500
								if selected {
									weight = 600
								}
								ui.Text(c, tab.label).FontSize(11.5).FontWeight(weight).NoWrap()
							})
							if pillBtn.Clicked() {
								w.widgets.modelProviderFilter = tab.id
							}
						}
					})
				})
			}

			// Quiet catalog count header with inline reload icon
			ui.Row(c.Key("model-list-header")).Height(22).Gap(6).AlignItems(ui.Center).Children(func() {
				switch {
				case w.modelsLoading:
					ui.Text(c, w.tr("正在载入模型…", "Loading models…")).FontSize(11).TextColor(p.muted)
				case len(w.models) > 0:
					label := fmt.Sprintf(w.tr("Pi 模型列表 · %d 个", "Pi models · %d"), w.piModelCount())
					ui.Text(c, label).FontSize(11).TextColor(p.muted)
				}
				ui.Spacer(c)
				reload := iconButton(c, p, "refresh-models", w.tr("重新载入", "Reload"), refreshIcon).Size(22, 22).Padding(4).Radius(6)
				reload.Description(w.tr("重新载入 Pi 模型列表", "Reload the Pi model list"))
				if w.modelsLoading {
					reload.Disabled(true)
				}
				if reload.Clicked() {
					w.ensureModels(true)
					if _, ok := w.service.(api.WorkbenchService); ok {
						w.loadModels()
					}
				}
			})

			if w.modelsErr != "" {
				ui.Text(c, w.modelsErr).FontSize(11).TextColor(p.danger).Wrap().Invalid(true)
			}
			if w.modelErr != "" {
				ui.Text(c, w.modelErr).FontSize(11).TextColor(p.danger).Wrap()
			}

			query := strings.ToLower(strings.TrimSpace(w.widgets.modelSearch))
			filteredModels := w.filteredCatalogModels(query, activeFilter)
			count := len(filteredModels)
			showDefaultRow := query == "" && activeFilter == ""
			rowCount := count
			if showDefaultRow {
				rowCount++
			}
			maxListH := min(float32(196), max(float32(96), height*0.25))
			if shortWindow {
				maxListH = max(float32(84), height*0.36)
			}
			listHeight := min(maxListH, max(float32(52), float32(rowCount*44+6)))
			firstOption := true
			baseModel, _ := w.modelSelection()
			ui.Scroll(c.Key("model-list")).Height(listHeight).Children(func() {
				ui.Column(c).Gap(2).Role(ui.RoleListBox).Label(w.tr("模型列表", "Models")).FocusGroup(ui.Vertical).Children(func() {
					if showDefaultRow {
						chosen := w.currentModel() == ""
						choice := quietButton(c, p, "default-model", "").Label(w.tr("跟随默认模型", "Follow default model")).Role(ui.RoleOption).AccessSelected(chosen).FillWidth().Disabled(w.modelSaving)
						choice.Children(func() {
							ui.Column(c).Gap(2).Grow(1).Children(func() {
								ui.Text(c, w.tr("使用 Pi 默认模型", "Use the Pi default model")).FontSize(12)
								if model := w.configuredDefaultModel(); model != "" {
									ui.Text(c, w.modelDisplayName(model)).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
								}
							})
						})
						if chosen {
							choice.Background(p.selected)
						}
						if firstOption {
							choice.Bind(&w.widgets.modelOptionsFocus)
							firstOption = false
						}
						if choice.Clicked() {
							w.chooseProviderModel("pi", "")
						}
					}
					for _, model := range filteredModels {
						model := model
						providerID, raw := splitExecutionModel(model)
						name := model.Name
						if name == "" {
							name = model.Key()
						}
						chosen := w.threadProvider(w.selected) == providerID && (model.Key() == baseModel || raw == baseModel)
						choice := ui.Button(c.Key("model:"+model.Key()), "").Label(model.Key()).Role(ui.RoleOption).AccessSelected(chosen).FillWidth().Padding(6, 9).Gap(9).Border(0, p.line).Background(ui.Transparent).Radius(9)
						if firstOption {
							choice.Bind(&w.widgets.modelOptionsFocus)
							firstOption = false
						}
						switch {
						case chosen:
							choice.Background(p.selected)
						case choice.Hovered():
							choice.Background(p.soft)
						}
						if w.modelSaving {
							choice.Disabled(true)
						}
						choice.Children(func() {
							markTone := p.faint
							if chosen {
								markTone = p.accent
							}
							w.providerMark(c, markTone, 12.5, providerID)
							ui.Column(c).Gap(1.5).Grow(1).MinWidth(0).Children(func() {
								ui.Text(c, name).FontSize(12.5).FontWeight(500).TextColor(p.ink).SingleLine().Ellipsis("…")
								ui.Text(c, model.Key()).FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…")
							})
							if providerID == "pi" && model.Key() == w.configuredDefaultModel() {
								ui.Text(c, w.tr("默认", "Default")).FontSize(10.5).TextColor(p.faint).Shrink(0)
							}
							if chosen {
								ui.Icon(c, checkIcon).FontSize(13.5).TextColor(p.accentInk).Shrink(0)
							}
						})
						if choice.Clicked() {
							w.chooseProviderModel(providerID, raw)
						}
					}
					if count == 0 && !w.modelsLoading {
						ui.Text(c, w.tr("没有匹配的模型", "No matching models")).FontSize(11).TextColor(p.muted).Wrap().Padding(6, 8)
						if len(w.models) == 0 && !w.defaults.RemotePeer {
							if primaryAction(c, p, "empty-model-setup", w.tr3("配置模型", "Configure models", "モデルを設定")).Clicked() {
								w.openPiSetup()
							}
						}
					}
				})
			})

			// Interactive Stardust Workshop 5-Stage Visual Reasoning Effort Slider
			if !shortWindow && baseModel != "" {
				w.effortSliderSection(c, p, "model-popover-effort", nil)
			}

			// Bottom 3-segment Permission Switcher (matching Stardust unified popover)
			if !shortWindow {
				w.popoverPermissionBar(c, p)
			}

			if w.features.modelFeedback.Level == noticeError {
				w.noticeLine(c, p, w.features.modelFeedback)
			}
		})
	})
}

type modelProviderTab struct {
	id, label string
}

func modelProviderFamily(model api.ModelInfo) (string, string) {
	raw := strings.ToLower(strings.TrimSpace(model.Provider))
	if raw == "" {
		if idx := strings.IndexByte(model.Key(), '/'); idx > 0 {
			raw = strings.ToLower(strings.TrimSpace(model.Key()[:idx]))
		}
	}
	switch {
	case strings.Contains(raw, "anthropic") || strings.Contains(raw, "claude"):
		return "claude", "Claude"
	case strings.Contains(raw, "openai") || strings.Contains(raw, "codex"):
		return "codex", "Codex"
	case strings.Contains(raw, "google") || strings.Contains(raw, "gemini"):
		return "gemini", "Gemini"
	case strings.Contains(raw, "deepseek"):
		return "deepseek", "DeepSeek"
	case strings.Contains(raw, "xai") || strings.Contains(raw, "grok"):
		return "grok", "Grok"
	case strings.Contains(raw, "kimi") || strings.Contains(raw, "moonshot"):
		return "kimi", "Kimi"
	case strings.Contains(raw, "minimax"):
		return "minimax", "MiniMax"
	case raw == "":
		return "pi", "Pi"
	default:
		return raw, strings.ToUpper(raw[:1]) + raw[1:]
	}
}

func (w *workbench) modelProviderTabs() []modelProviderTab {
	tabs := []modelProviderTab{
		{id: "", label: w.tr3("全部", "All", "すべて")},
	}
	hasFeatured := false
	for _, m := range w.models {
		if m.Reasoning || m.Key() == w.configuredDefaultModel() {
			hasFeatured = true
			break
		}
	}
	if hasFeatured && len(w.models) > 1 {
		tabs = append(tabs, modelProviderTab{id: "__featured__", label: w.tr3("常用", "Featured", "よく使う")})
	}
	seen := map[string]bool{}
	for _, m := range w.models {
		id, label := modelProviderFamily(m)
		if !seen[id] {
			seen[id] = true
			tabs = append(tabs, modelProviderTab{id: id, label: label})
		}
	}
	return tabs
}

func (w *workbench) filteredCatalogModels(query, providerFilter string) []api.ModelInfo {
	var out []api.ModelInfo
	defKey := w.configuredDefaultModel()
	for _, model := range w.models {
		if query != "" && !strings.Contains(strings.ToLower(model.Key()+" "+model.Name+" "+model.Provider), query) {
			continue
		}
		if providerFilter == "__featured__" {
			if !model.Reasoning && model.Key() != defKey {
				continue
			}
		} else if providerFilter != "" {
			famID, _ := modelProviderFamily(model)
			if famID != providerFilter {
				continue
			}
		}
		out = append(out, model)
	}
	return out
}

// popoverPermissionBar renders the 3-segment permission switcher at the bottom
// of the unified model popover (完全访问 / 自动 / 需审批).
func (w *workbench) popoverPermissionBar(c *ui.Context, p palette) {
	current := normalizeApprovalPolicy(w.defaults.ApprovalPolicy)
	segments := []struct {
		policy     string
		zh, en, ja string
	}{
		{"auto", "完全访问", "Full", "完全許可"},
		{"auto-accept-edits", "自动", "Auto", "自動"},
		{"ask", "需审批", "Ask", "承認制"},
	}
	ui.Row(c.Key("popover-perm-bar")).FillWidth().Padding(4, 2, 0).Gap(8).AlignItems(ui.Center).
		BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
		ui.Text(c, w.tr3("访问权限", "Permissions", "実行権限")).FontSize(11).TextColor(p.muted).SingleLine().Shrink(0)
		ui.Spacer(c)
		ui.Row(c.Key("popover-perm-seg")).Height(26).Padding(2).Gap(2).Radius(9).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Shrink(0).Children(func() {
			for _, seg := range segments {
				seg := seg
				active := current == seg.policy
				btn := ui.Button(c.Key("popover-perm:"+seg.policy), "").Label(w.tr3(seg.zh, seg.en, seg.ja)).
					Height(22).Padding(0, 8).Radius(7).Border(0, ui.Transparent).Background(ui.Transparent).TextColor(p.muted).Shrink(0)
				if active {
					btn.Background(p.card).Border(1, p.line).TextColor(p.ink)
				} else if btn.Hovered() {
					btn.Background(p.rowHover).TextColor(p.ink)
				}
				btn.Children(func() {
					weight := 400
					if active {
						weight = 600
					}
					ui.Text(c, w.tr3(seg.zh, seg.en, seg.ja)).FontSize(11).FontWeight(weight).NoWrap()
				})
				if btn.Clicked() && seg.policy != w.defaults.ApprovalPolicy {
					w.defaults.ApprovalPolicy = seg.policy
					w.saveState()
				}
			}
		})
	})
}

// thinkingLevelLabel is a thinking level as people say it.
func (w *workbench) thinkingLevelLabel(level string) string {
	switch strings.ToLower(level) {
	case "":
		return w.tr("默认强度", "Default effort")
	case "off":
		return w.tr("关闭", "Off")
	case "minimal":
		return w.tr("极简", "Minimal")
	case "low":
		return w.tr("低", "Low")
	case "medium":
		return w.tr("中等", "Medium")
	case "high":
		return w.tr("高", "High")
	case "xhigh":
		return w.tr("超高", "XHigh")
	case "max":
		return w.tr("最高", "Max")
	}
	if level == "" {
		return level
	}
	return strings.ToUpper(level[:1]) + level[1:]
}

// effortDotColor maps a thinking level to its Stardust indicator hue.
func effortDotColor(p palette, level string) ui.Color {
	switch strings.ToLower(level) {
	case "max", "xhigh":
		return p.accent
	case "high":
		return ui.Hex("#0EA5C6")
	case "medium":
		return p.success
	case "low", "minimal":
		return p.warning
	case "off":
		return p.faint
	default:
		return p.muted
	}
}

// thinkingPicker is the inline effort segment inside the unified model pill:
// colored effort dot + level label + chevron.
func (w *workbench) thinkingPicker(c *ui.Context, p palette) {
	if !w.modelSwitchable(w.selected) {
		return
	}
	base, _ := w.modelSelection()
	level := w.displayedEffortLevel()
	known := base != "" && base == w.features.thinkingModel && len(w.features.thinkingLevels) > 0
	if base == "" {
		return
	}
	// A narrow composer keeps only the colored dot; the level stays in the
	// accessible name and the tooltip.
	compact := w.composerFooterWidth() < 520
	button := kitSelectTrigger(c, p, "thinking-picker", w.tr("思考强度", "Thinking effort"), w.widgets.thinkingOpen).Height(22).Padding(0, 7).Radius(11).Shrink(0)
	button.Description(w.tr("当前思考强度：", "Current thinking effort: ") + w.thinkingLevelLabel(level))
	button.Tooltip(w.tr("思考强度：", "Thinking effort: ") + w.thinkingLevelLabel(level))
	button.Children(func() {
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			ui.Box(c.Key("thinking-dot")).Size(6, 6).Radius(3).Background(effortDotColor(p, level)).Shrink(0)
			if !compact {
				ui.Text(c, w.thinkingLevelLabel(level)).FontSize(11.5).FontWeight(500).TextColor(p.muted).NoWrap()
			}
			ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint).Shrink(0)
		})
	})
	if w.features.thinkingLoading {
		button.Disabled(true)
	}
	if button.Clicked() {
		w.widgets.thinkingOpen = !w.widgets.thinkingOpen
	}
	width, _ := c.Size()
	ui.PopoverBase(c, button, &w.widgets.thinkingOpen, func(panel ui.Element) {
		panel.AttachTo(button, ui.AnchorTopEnd, ui.AnchorBottomEnd).Margin(0, 0, 8).
			Width(composerPopoverWidth(width, 360)).Padding(8).Radius(15).Background(p.raised).Border(1, p.line).Shadow(0, 14, 38, 0, ui.Hex("#00000052"))
		if panel.Shortcut(0, ui.KeyEscape) {
			w.widgets.thinkingOpen = false
			return
		}
		w.effortSliderSection(c, p, "thinking-popover-effort", nil)
	})
	if !known && w.widgets.thinkingOpen && !w.features.thinkingLoading {
		w.loadThinking()
	}
}

// ensureModelDetails loads the model catalog once per workspace in the
// background, so the thinking levels are ready before the picker opens.
func (w *workbench) ensureModelDetails() {
	if _, ok := w.service.(api.WorkbenchService); !ok || w.selected == "" || !w.modelSwitchable(w.selected) {
		return
	}
	path := w.modelWorkspace()
	if w.features.catalogLoading || w.features.thinkingLoading || w.features.catalogAuto == path+"\x00" {
		return
	}
	if w.features.catalog != nil && w.features.catalogPath == path {
		return
	}
	w.features.catalogAuto = path + "\x00"
	w.loadModels()
}

func (w *workbench) chooseModel(model string) {
	w.commitModel(model, true)
}

func (w *workbench) commitModel(model string, closePicker bool) {
	if w.selected == "" {
		w.newSession()
	}
	if w.modelSaving {
		return
	}
	if !w.modelInCatalog(model) {
		w.modelErr = modelOutsideCatalog
		w.widgets.modelPickerOpen = true
		return
	}
	if comp := w.composerFor(w.selected); comp.feedback.Text == modelOutsideCatalog {
		comp.feedback = notice{}
	}
	if isLocalSession(w.selected) || !w.hasThread(w.selected) {
		w.defaults.Model = model
		w.composerFor(w.selected).model = &model
		if closePicker {
			w.widgets.modelPickerOpen = false
			w.widgets.composerFocus.Focus()
		}
		w.modelErr = ""
		w.saveState()
		if closePicker {
			if _, ok := w.service.(api.WorkbenchService); ok && model != "" {
				w.loadThinking()
			}
		}
		return
	}
	threadID := w.selected
	command := api.Command{Version: api.ProtocolVersion, CommandID: newCommandID("model"), Kind: api.SetThreadModel, ThreadID: threadID, Model: model}
	if prior := w.modelCommand; prior != nil && prior.ThreadID == threadID && prior.Model == model {
		command = *prior
	}
	w.modelCommand = &command
	w.modelSaving, w.modelErr = true, ""
	w.dispatch(command, func(receipt api.Receipt, err error) {
		w.post(func() {
			w.modelSaving = false
			if err != nil {
				if w.selected == threadID {
					w.modelErr = describe("模型切换未确认，可以重试", err)
				}
				return
			}
			if !receipt.Accepted {
				if w.selected == threadID {
					w.modelErr = "本地服务未接受模型切换，请重试。"
				}
				return
			}
			w.modelCommand = nil
			w.defaults.Model = model
			if closePicker && w.selected == threadID {
				w.widgets.modelPickerOpen = false
				w.widgets.composerFocus.Focus()
			}
			w.saveState()
			if closePicker {
				if base, _ := w.modelSelection(); base != "" && base != w.features.thinkingModel && !w.features.thinkingLoading {
					if _, ok := w.service.(api.WorkbenchService); ok {
						w.loadThinking()
					}
				}
			}
			w.flushEffortPick()
		})
	})
}

func (w *workbench) configureWorkspace(path string) {
	path = strings.TrimSpace(path)
	current := w.defaults.Workspace
	if thread, ok := w.selectedThread(); ok {
		current = thread.Workspace
	}
	if current == nil && path == "" || current != nil && current.Path != "" && filepath.Clean(current.Path) == filepath.Clean(path) {
		return
	}
	if !isLocalSession(w.selected) {
		comp := w.composerFor(w.selected)
		draft := comp.draft
		attachments := append([]api.Attachment(nil), comp.attachments...)
		mentions := append([]composerMention(nil), comp.mentions...)
		w.newSession()
		nextComp := w.composerFor(w.selected)
		nextComp.attachments = attachments
		nextComp.mentions = mentions
		w.setDraft(w.selected, draft)
	}
	if path == "" {
		w.defaults.Workspace = nil
	} else {
		w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: path}
	}
	w.invalidateModels()
	w.saveState()
}

// composerStatusFooter renders the Stardust quiet status footer below the
// composer ("● 第 N 轮 · 进行中 · M 步 · 已运行 X 分钟 · 令牌 / 缓存"), showing
// strictly real fields present in run/telemetry state and omitting unknowns.
func (w *workbench) composerStatusFooter(c *ui.Context, p palette, threadID string, activeRun api.Run, active bool) {
	runs := w.runsFor(threadID)
	if len(runs) == 0 {
		return
	}
	latestRun := runs[len(runs)-1]
	if active {
		latestRun = activeRun
	}
	_, _, tel, ctxWindow := w.threadTelemetry(threadID)
	awaiting := active && w.awaitingApproval(activeRun.ID)

	dotColor := p.success
	stateText := w.tr3("已完成", "Done", "完了")
	live := false
	switch {
	case active && w.stoppingRuns[activeRun.ID]:
		dotColor, stateText = p.muted, w.tr3("正在停止…", "Stopping…", "停止中…")
	case awaiting:
		dotColor, stateText = p.warning, w.tr3("待授权", "Needs approval", "承認待ち")
	case active && tel != nil && tel.Compacting:
		dotColor, stateText, live = p.warning, w.tr3("压缩上下文", "Compacting", "圧縮中"), true
	case active && tel != nil && tel.Retrying:
		attempt := max(1, tel.RetryAttempt)
		dotColor, live = p.warning, true
		stateText = w.tr3(fmt.Sprintf("重试 #%d", attempt), fmt.Sprintf("Retrying #%d", attempt), fmt.Sprintf("再試行 #%d", attempt))
	case active:
		dotColor, stateText, live = p.running, w.tr3("进行中", "Running", "実行中"), true
	case latestRun.Status == api.RunFailed || latestRun.Status == api.RunInterrupted:
		dotColor, stateText = p.danger, w.tr3("失败", "Failed", "失敗")
	case latestRun.Status == api.RunCancelled:
		dotColor, stateText = p.muted, w.tr3("已停止", "Stopped", "停止済み")
	}

	totalSteps := 0
	totalRetries := 0
	for _, r := range runs {
		totalSteps += len(w.runTools(r.ID))
		if r.Telemetry != nil {
			totalRetries += r.Telemetry.RetryCount
		}
	}
	if totalRetries == 0 && tel != nil {
		totalRetries = tel.RetryCount
	}
	latestSteps := len(w.runTools(latestRun.ID))
	var dur time.Duration
	if !latestRun.CreatedAt.IsZero() {
		if active && latestRun.Status == api.RunRunning {
			dur = c.Now().Sub(latestRun.CreatedAt)
			if dur >= 0 {
				c.After(time.Second - dur%time.Second)
			}
		} else if !latestRun.UpdatedAt.IsZero() && latestRun.UpdatedAt.After(latestRun.CreatedAt) {
			dur = latestRun.UpdatedAt.Sub(latestRun.CreatedAt)
		}
	}
	var tokRate float64
	if tel != nil && tel.OutputTokens > 0 && tel.DurationMs >= 500 {
		tokRate = float64(tel.OutputTokens) * 1000.0 / float64(tel.DurationMs)
	}

	var parts []string
	if totalSteps > 0 {
		parts = append(parts, fmt.Sprintf(w.tr3("%d 轮 %d 步", "%d turns · %d steps", "%d ターン %d ステップ"), len(runs), totalSteps))
	} else {
		parts = append(parts, fmt.Sprintf(w.tr3("第 %d 轮", "Turn %d", "ターン %d"), len(runs)))
	}
	if active || latestRun.Status == api.RunFailed || latestRun.Status == api.RunInterrupted {
		parts = append(parts, stateText)
	}
	if tokRate > 0 {
		rateFmt := "%.0f tok/s"
		if tokRate < 10 {
			rateFmt = "%.1f tok/s"
		}
		parts = append(parts, fmt.Sprintf(rateFmt, tokRate))
	} else if dur >= time.Second && dur < 48*time.Hour {
		if active {
			parts = append(parts, w.tr3("已运行 ", "running ", "経過 ")+formatElapsed(dur))
		} else {
			parts = append(parts, formatElapsed(dur))
		}
	}

	if totalRetries > 0 {
		parts = append(parts, fmt.Sprintf(w.tr3("重试 %d 次", "%d retries", "再試行 %d回"), totalRetries))
	}

	tokBadge := ""
	cacheHitPct := -1
	freshCount := int64(0)
	if tel != nil && tel.TotalTokens > 0 {
		freshCount = freshTokens(tel.InputTokens, tel.OutputTokens, tel.CacheWriteTokens, tel.CacheReadTokens, tel.TotalTokens)
		if tel.CacheReadTokens > 0 {
			if denom := tel.InputTokens + tel.CacheReadTokens + tel.CacheWriteTokens; denom > 0 {
				cacheHitPct = int((tel.CacheReadTokens * 100) / denom)
			}
			if cacheHitPct >= 0 {
				tokBadge = fmt.Sprintf(w.tr3("新增 %s · 缓存 %s (%d%%)", "%s new · %s cached (%d%%)", "新規 %s・キャッシュ %s (%d%%)"),
					compactCount64(freshCount), compactCount64(tel.CacheReadTokens), cacheHitPct)
			} else {
				tokBadge = fmt.Sprintf(w.tr3("新增 %s · 缓存 %s", "%s new · %s cached", "新規 %s・キャッシュ %s"),
					compactCount64(freshCount), compactCount64(tel.CacheReadTokens))
			}
		} else {
			tokBadge = fmt.Sprintf(w.tr3("%s 令牌", "%s tokens", "%s トークン"), compactCount64(tel.TotalTokens))
		}
	}

	ui.Row(c.Key("composer-status-footer")).FillWidth().Height(26).Margin(4, 0, 0).Padding(0, 8).
		Justify(ui.Center).AlignItems(ui.Center).Children(func() {
		trigger := ui.Button(c.Key("composer-stats-trigger"), "").
			Label(w.tr3("会话概览与令牌统计", "Session overview & token stats", "セッション概要とトークン統計")).
			Tooltip(w.tr3("点击展开会话概览与令牌缓存明细", "Click to inspect session telemetry and cache breakdown", "クリックしてセッション統計とキャッシュ詳細を表示")).
			Height(24).Padding(0, 4).Gap(8).Radius(12).Border(0, ui.Transparent).Background(ui.Transparent).Shrink(1).MinWidth(0)
		if w.widgets.statsPopoverOpen || trigger.Hovered() {
			trigger.Background(p.soft.Alpha(0.75))
		}
		trigger.Children(func() {
			ui.Row(c).Height(22).Padding(0, 9).Gap(6).Radius(11).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Shrink(1).MinWidth(0).Children(func() {
				if live {
					activityIndicator(c, dotColor).Size(7, 7).Shrink(0)
				} else {
					ui.Box(c).Size(6, 6).Radius(3).Background(dotColor).Shrink(0)
				}
				ui.Text(c, strings.Join(parts, " · ")).FontSize(11).FontWeight(500).FontFeatures("tnum").TextColor(p.muted).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			})
			if tokBadge != "" {
				ui.Box(c).Size(1, 11).Background(p.line).Shrink(0)
				ui.Row(c).Height(22).Padding(0, 9).Radius(11).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Shrink(0).Children(func() {
					ui.Text(c, tokBadge).FontSize(11).FontWeight(500).FontFeatures("tnum").TextColor(p.muted).NoWrap()
				})
			}
			if tel != nil && tel.CostUSD >= 0.0001 {
				ui.Text(c, formatCostUSD(tel.CostUSD)).Font("Menlo").FontSize(10.5).TextColor(p.faint).NoWrap().Shrink(0)
			}
		})
		if trigger.Clicked() {
			w.widgets.statsPopoverOpen = !w.widgets.statsPopoverOpen
		}

		width, _ := c.Size()
		ui.PopoverBase(c, trigger, &w.widgets.statsPopoverOpen, func(panel ui.Element) {
			panel.AttachTo(trigger, ui.AnchorTop, ui.AnchorBottom).Margin(0, 0, 10).
				Width(composerPopoverWidth(width, 360)).Padding(12, 14).Radius(15).Background(p.raised).Border(1, p.line).
				Shadow(0, 16, 42, 0, ui.Hex("#00000059"))
			if panel.Shortcut(0, ui.KeyEscape) {
				w.widgets.statsPopoverOpen = false
				return
			}
			ui.Column(c).Gap(10).Children(func() {
				// Header row
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(7, 7).Radius(3.5).Background(dotColor).Shrink(0)
					ui.Text(c, w.tr3("会话概览 · 令牌与缓存", "Session Overview · Tokens & Cache", "セッション概要・トークンとキャッシュ")).
						FontSize(12.5).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine()
					pill(c, p, stateText, dotColor)
				})

				// Compact real metrics row
				ui.Row(c).FillWidth().Gap(6).Children(func() {
					statTile := func(key, label, value string, tone ui.Color) {
						ui.Column(c.Key(key)).Grow(1).MinWidth(0).Padding(8, 10).Gap(2).Radius(10).
							Background(p.soft).Border(1, p.lineSoft).Children(func() {
							ui.Text(c, label).FontSize(10).TextColor(p.faint).SingleLine()
							ui.Text(c, value).FontSize(13.5).FontWeight(600).FontFeatures("tnum").TextColor(tone).SingleLine()
						})
					}
					stepVal := fmt.Sprintf(w.tr3("%d 轮", "%d turns", "%d ターン"), len(runs))
					if totalSteps > 0 {
						stepVal = fmt.Sprintf(w.tr3("%d 轮 %d 步", "%d / %d steps", "%d 轮 %d 歩"), len(runs), totalSteps)
					} else if latestSteps > 0 {
						stepVal = fmt.Sprintf(w.tr3("%d 轮 %d 步", "%d / %d steps", "%d 轮 %d 歩"), len(runs), latestSteps)
					}
					statTile("stats-turns", w.tr3("进度", "Progress", "進捗"), stepVal, p.ink)
					if tokRate > 0 {
						statTile("stats-rate", w.tr3("生成速率", "Speed", "生成速度"), fmt.Sprintf("%.0f tok/s", tokRate), p.accentInk)
					} else if dur >= time.Second && dur < 48*time.Hour {
						statTile("stats-dur", w.tr3("最近耗时", "Elapsed", "経過時間"), formatElapsed(dur), p.ink)
					}
					if tel != nil && tel.CostUSD >= 0.0001 {
						statTile("stats-cost", w.tr3("估算费用", "Est. Cost", "推定コスト"), formatCostUSD(tel.CostUSD), p.ink)
					} else if dur >= time.Second && dur < 48*time.Hour && tokRate > 0 {
						statTile("stats-dur2", w.tr3("耗时", "Duration", "所要時間"), formatElapsed(dur), p.muted)
					}
					if totalRetries > 0 {
						statTile("stats-retries", w.tr3("自动重试", "Auto retries", "自動再試行"), fmt.Sprintf(w.tr3("%d 次", "%d", "%d回"), totalRetries), p.warning)
					}
				})

				// Context window usage bar if available
				if tel != nil && tel.LastContextTokens > 0 && ctxWindow > 0 {
					pct := int(min(int64(100), (tel.LastContextTokens*100)/ctxWindow))
					frac := float32(min(1.0, float64(tel.LastContextTokens)/float64(ctxWindow)))
					tone := p.accent
					if pct >= 85 {
						tone = p.danger
					} else if pct >= 70 {
						tone = p.warning
					}
					ui.Row(c).FillWidth().Padding(8, 10).Gap(8).Radius(10).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).Children(func() {
						progressRing(c, 18, frac, p.line, tone)
						ui.Column(c).Gap(1).Grow(1).MinWidth(0).Children(func() {
							ui.Text(c, w.tr3("上下文窗口", "Context window", "コンテキスト枠")).FontSize(10.5).TextColor(p.muted).SingleLine()
							ui.Text(c, fmt.Sprintf("%s / %s tokens", compactCount64(tel.LastContextTokens), compactCount64(ctxWindow))).
								Font("Menlo").FontSize(11).FontFeatures("tnum").TextColor(p.ink).SingleLine()
						})
						ui.Text(c, fmt.Sprintf("%d%%", pct)).FontSize(13).FontWeight(700).FontFeatures("tnum").TextColor(tone).Shrink(0)
					})
				}

				// Token & Cache breakdown with Painter stacked bar
				if tel != nil && tel.TotalTokens > 0 {
					ui.Column(c).FillWidth().Padding(9, 10).Gap(7).Radius(10).Background(p.soft).Border(1, p.lineSoft).Children(func() {
						ui.Row(c).AlignItems(ui.Center).Children(func() {
							ui.Text(c, w.tr3("令牌构成", "Token breakdown", "トークン内訳")).FontSize(11).FontWeight(600).TextColor(p.ink).Grow(1)
							if tel.CacheReadTokens > 0 && cacheHitPct >= 0 {
								ui.Text(c, fmt.Sprintf(w.tr3("新增 %s · 缓存命中 %d%%", "%s new · cache %d%%", "新規 %s・キャッシュ %d%%"), compactCount64(freshCount), cacheHitPct)).
									FontSize(10.5).FontWeight(600).FontFeatures("tnum").TextColor(p.success)
							} else if cacheHitPct >= 0 {
								ui.Text(c, fmt.Sprintf(w.tr3("缓存命中 %d%%", "Cache hit %d%%", "キャッシュ命中 %d%%"), cacheHitPct)).
									FontSize(10.5).FontWeight(600).FontFeatures("tnum").TextColor(p.success)
							}
						})
						// Stacked bar
						inTok := float64(max(int64(0), tel.InputTokens))
						outTok := float64(max(int64(0), tel.OutputTokens))
						cacheTok := float64(max(int64(0), tel.CacheReadTokens))
						sumTok := max(1.0, inTok+outTok+cacheTok)
						colIn, colOut, colCache := ui.Hex("#38BDF8"), p.accent, p.success
						ui.Box(c.Key("stats-token-bar")).FillWidth().Height(8).Radius(4).Draw(func(pt *ui.Painter, r ui.Rect) {
							pt.Clip(r, 4, func() {
								pt.Fill(r, p.lineSoft, 0)
								x := r.X
								wCache := float32(cacheTok/sumTok) * r.W
								wIn := float32(inTok/sumTok) * r.W
								wOut := r.W - wCache - wIn
								if wCache > 0 {
									pt.Fill(ui.Rect{X: x, Y: r.Y, W: wCache, H: r.H}, colCache, 0)
									x += wCache
								}
								if wIn > 0 {
									pt.Fill(ui.Rect{X: x, Y: r.Y, W: wIn, H: r.H}, colIn, 0)
									x += wIn
								}
								if wOut > 0 {
									pt.Fill(ui.Rect{X: x, Y: r.Y, W: wOut, H: r.H}, colOut, 0)
								}
							})
						})
						// Legend numbers
						ui.Row(c).FillWidth().Gap(10).Wrap().Children(func() {
							legend := func(dot ui.Color, name string, val int64) {
								ui.Row(c).Gap(4).AlignItems(ui.Center).Shrink(0).Children(func() {
									ui.Box(c).Size(6, 6).Radius(3).Background(dot).Shrink(0)
									ui.Text(c, fmt.Sprintf("%s %s", name, compactCount64(val))).FontSize(10.5).FontFeatures("tnum").TextColor(p.muted).NoWrap()
								})
							}
							legend(colCache, w.tr3("缓存读取", "Cache", "キャッシュ"), tel.CacheReadTokens)
							legend(colIn, w.tr3("输入", "In", "入力"), tel.InputTokens)
							legend(colOut, w.tr3("输出", "Out", "出力"), tel.OutputTokens)
							if tel.CacheWriteTokens > 0 {
								legend(p.warning, w.tr3("缓存写入", "Write", "書込"), tel.CacheWriteTokens)
							}
							legend(p.faint, w.tr3("总计", "Total", "合計"), tel.TotalTokens)
						})
					})
				}
			})
		})
	})
}
