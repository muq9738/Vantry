package desktop

import (
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// subagentPanelMemory is the open subagent panel for one root session.
// Drafts stay on that child's composer, so two children never share a box.
type subagentPanelMemory struct {
	open         bool
	threadID     string
	focusPending bool
	draftFocus   ui.Handle
}

func (w *workbench) subagentPanelState(key string) *subagentPanelMemory {
	if w.subagentPanels == nil {
		w.subagentPanels = map[string]*subagentPanelMemory{}
	}
	if w.subagentPanels[key] == nil {
		w.subagentPanels[key] = &subagentPanelMemory{}
	}
	return w.subagentPanels[key]
}

// subagentPanelKey is the root session a child belongs to, or threadID when
// it has no parent on this machine.
func (w *workbench) subagentPanelKey(threadID string) string {
	if threadID == "" {
		threadID = w.selected
	}
	if threadID == "" {
		return ""
	}
	if root := w.sidebarAncestor(threadID); root != "" {
		return root
	}
	return threadID
}

func (w *workbench) currentSubagentPanel() *subagentPanelMemory {
	key := w.subagentPanelKey(w.selected)
	if key == "" || w.subagentPanels == nil {
		return nil
	}
	return w.subagentPanels[key]
}

func (w *workbench) subagentPanelOpen() bool {
	st := w.currentSubagentPanel()
	return st != nil && st.open && st.threadID != ""
}

func (w *workbench) subagentPanelShows(id string) bool {
	st := w.currentSubagentPanel()
	return st != nil && st.open && id != "" && st.threadID == id
}

// openSubagentPanel shows threadID beside the current root session without
// changing w.selected. The choice is kept per root session.
func (w *workbench) openSubagentPanel(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}
	key := w.subagentPanelKey(threadID)
	if key == "" {
		return
	}
	st := w.subagentPanelState(key)
	st.open = true
	if st.threadID != threadID {
		st.threadID = threadID
	}
	st.focusPending = true
	w.saveState()
}

func (w *workbench) closeSubagentPanel() {
	st := w.currentSubagentPanel()
	if st == nil || !st.open {
		return
	}
	st.open = false
	w.widgets.composerFocus.Focus()
	w.saveState()
}

func (w *workbench) persistedSubagentPanels() map[string]persistedSubagentPanel {
	if len(w.subagentPanels) == 0 {
		return nil
	}
	out := make(map[string]persistedSubagentPanel, len(w.subagentPanels))
	for id, panel := range w.subagentPanels {
		if panel == nil || id == "" || (panel.threadID == "" && !panel.open) {
			continue
		}
		out[id] = persistedSubagentPanel{Open: panel.open, ThreadID: panel.threadID}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (w *workbench) subagentPanelFloats(c *ui.Context, narrow bool) bool {
	if narrow {
		return true
	}
	return w.mainStageWidth(c, narrow) < dockSideBreakpoint
}

func (w *workbench) subagentPanelWidth(main float32) float32 {
	want := w.subagentWidth
	if want == 0 {
		want = 440
	}
	return max(float32(360), min(want, main-chatMinWidth))
}

func (w *workbench) subagentResizeHandle(c *ui.Context, p palette, main float32) {
	handle := ui.Box(c.Key("subagent-resize")).Width(5).Shrink(0).Cursor(ui.CursorResizeEW).Role(ui.RoleSplitter).
		Label(w.tr("调整子代理面板宽度", "Resize subagent panel"))
	if dx, _, ok := handle.Dragged(); ok && dx != 0 {
		w.subagentWidth = max(float32(360), min(w.subagentPanelWidth(main)-dx, main-chatMinWidth))
	}
	if handle.DoubleClicked() {
		w.subagentWidth = 0
	}
	color := p.lineSoft
	if handle.Hovered() || handle.Pressed() {
		color = p.accent.Alpha(0.55)
	}
	handle.Draw(func(painter *ui.Painter, r ui.Rect) {
		painter.Fill(ui.Rect{X: r.X + r.W/2 - 0.5, Y: r.Y, W: 1, H: r.H}, color, 0)
	})
}

func (w *workbench) subagentSideView(c *ui.Context, p palette, width float32) {
	panel := ui.Column(c.Key("subagent-panel")).Width(width).Shrink(0).MinHeight(0).Clip().
		Background(p.surface).BorderWidth(0, 0, 0, 1).BorderColor(p.lineSoft)
	panel.Role(ui.RoleRegion).Label(w.tr("子代理面板", "Subagent panel"))
	panel.Children(func() { w.subagentPanelBody(c, p, false) })
}

func (w *workbench) subagentFloatingView(c *ui.Context, p palette) {
	ui.Overlay(c, func() {
		scrim := ui.Box(c.Key("subagent-scrim")).Absolute().Left(0).Top(0).Right(0).Bottom(0).Background(ui.Hex("#00000066"))
		if scrim.Clicked() {
			w.closeSubagentPanel()
		}
		width, _ := c.Size()
		panelW := min(float32(440), max(float32(300), width-20))
		panel := ui.Column(c.Key("subagent-panel")).Absolute().Top(10).Bottom(10).Right(10).Width(panelW).
			Radius(16).Clip().Background(p.surface).Border(1, p.line).Role(ui.RoleRegion).
			Label(w.tr("子代理面板", "Subagent panel"))
		if p.dark {
			panel.Shadow(0, 16, 40, 0, ui.Hex("#00000080"))
		} else {
			panel.Shadow(0, 12, 32, 0, ui.Hex("#0F172A24"))
		}
		if panel.Clicked() {
			// Keep presses on the sheet from dismissing it via the scrim.
		}
		if panel.OverlayShortcut(0, ui.KeyEscape) {
			w.closeSubagentPanel()
			return
		}
		panel.Children(func() { w.subagentPanelBody(c, p, true) })
	})
}

func (w *workbench) subagentPanelBody(c *ui.Context, p palette, floating bool) {
	if !floating && c.Shortcut(0, ui.KeyEscape) {
		w.closeSubagentPanel()
		return
	}
	st := w.currentSubagentPanel()
	if st == nil || st.threadID == "" {
		return
	}
	threadID := st.threadID
	ui.Column(c).Grow(1).MinHeight(0).Children(func() {
		w.subagentPanelHeader(c, p, st, threadID)
		w.subagentTranscript(c, p, threadID)
		w.subagentComposer(c, p, st, threadID)
	})
}

func (w *workbench) subagentPanelHeader(c *ui.Context, p palette, st *subagentPanelMemory, threadID string) {
	status := w.subagentStatus(threadID)
	tone := taskStatusTone(p, status)
	title := stripInlineMarkdownTitle(nonEmptyString(w.displayTitleFor(threadID), w.tr("子代理", "Subagent")))
	model := ""
	if child, ok := w.threadByID(threadID); ok {
		model = w.threadModelLabel(child)
	}
	elapsed, live := w.subagentElapsed(threadID, status)
	parentTitle := ""
	if parent, ok := w.existingParentID(threadID); ok {
		parentTitle = w.displayTitleFor(parent)
	}
	items := w.subagentNavItems(w.subagentPanelKey(threadID))
	if live {
		c.After(time.Second)
	}
	ui.Column(c.Key("subagent-header")).Shrink(0).Padding(12, 12, 10).Gap(8).
		BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(28, 28).Shrink(0).Children(func() {
				agentAvatar(c, p, title, 28)
				ui.Box(c).Absolute().Right(-1).Bottom(-1).Size(9, 9).Radius(5).Background(tone).Border(1.5, p.surface)
			})
			ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
				ui.Text(c, w.tr("子代理", "Subagent")).FontSize(10.5).FontWeight(600).TextColor(p.faint).NoWrap()
				ui.Text(c, title).FontSize(14).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…")
			})
			if iconButton(c, p, "subagent-close", w.tr("关闭子代理面板", "Close subagent panel"), closeIcon).Size(26, 26).Padding(5).Clicked() {
				w.closeSubagentPanel()
			}
		})
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			w.subagentStatusChip(c, p, status)
			if model != "" {
				ui.Text(c, model).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			}
			if elapsed > 0 {
				ui.Text(c, spanDuration(elapsed)).FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
			}
		})
		if parentTitle != "" {
			ui.Text(c, w.tr("父会话", "Parent")+" · "+parentTitle).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…")
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			if len(items) > 1 {
				current := title
				switcher := quietButton(c, p, "subagent-switch", "").Label(w.tr("切换子代理", "Switch subagent")).Height(26).Padding(0, 8).Radius(8).Shrink(1).MinWidth(0)
				switcher.Children(func() {
					ui.Row(c).Gap(4).AlignItems(ui.Center).MinWidth(0).Children(func() {
						ui.Text(c, current).FontSize(11.5).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
						ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint).Shrink(0)
					})
				})
				switcher.PopoverMenuButton(func(m *ui.Menu) {
					for _, item := range items {
						item := item
						label := stripInlineMarkdownTitle(item.title)
						if m.Item(label).Radio(item.threadID == threadID).Chosen() {
							w.openSubagentPanel(item.threadID)
						}
					}
				}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomStart, Self: ui.AnchorTopStart})
			}
			ui.Spacer(c)
			run, active := w.activeRun(threadID)
			if active {
				stopLabel := w.tr("停止子代理", "Stop subagent")
				if w.stoppingRuns[run.ID] {
					stopLabel = w.tr("正在停止", "Stopping")
				}
				stop := quietButton(c, p, "subagent-stop", stopLabel).Height(26).Radius(8)
				if w.stoppingRuns[run.ID] {
					stop.Disabled(true)
				}
				if stop.Clicked() {
					w.stopThread(threadID)
				}
			}
			if quietButton(c, p, "subagent-main", w.tr("在主视图打开", "Open in main view")).Height(26).Radius(8).Clicked() {
				w.closeSubagentPanel()
				w.selectThread(threadID)
				w.widgets.composerFocus.Focus()
			}
		})
	})
	_ = st
}

func (w *workbench) subagentStatusChip(c *ui.Context, p palette, status api.RunStatus) {
	tone := taskStatusTone(p, status)
	label := w.tr(statusLabel(status), englishStatus(status))
	if label == "" {
		label = w.tr("空闲", "Idle")
		tone = p.faint
	}
	ui.Row(c.Key("subagent-status")).Height(18).Padding(0, 6).Gap(4).Radius(9).Shrink(0).
		Background(tone.Alpha(0.14)).AlignItems(ui.Center).Children(func() {
		if status == api.RunRunning || status == api.RunQueued {
			activityIndicator(c, tone).Size(8, 8)
		} else {
			ui.Box(c).Size(6, 6).Radius(3).Background(tone)
		}
		ui.Text(c, label).FontSize(11).FontWeight(500).TextColor(tone).NoWrap()
	})
}

func (w *workbench) subagentStatus(threadID string) api.RunStatus {
	if run, ok := w.latestRun(threadID); ok && run.Status != "" {
		return run.Status
	}
	for i := len(w.tasks) - 1; i >= 0; i-- {
		task := w.tasks[i]
		if task.ChildThreadID == threadID && task.Status != "" {
			return task.Status
		}
	}
	return ""
}

func (w *workbench) subagentElapsed(threadID string, status api.RunStatus) (time.Duration, bool) {
	start, end := time.Time{}, time.Time{}
	if run, ok := w.latestRun(threadID); ok {
		start, end = run.CreatedAt, run.UpdatedAt
	}
	if start.IsZero() {
		for _, task := range w.tasks {
			if task.ChildThreadID != threadID {
				continue
			}
			start, end = task.CreatedAt, task.UpdatedAt
			break
		}
	}
	if start.IsZero() {
		return 0, false
	}
	if !status.Terminal() || end.IsZero() || end.Before(start) {
		end = time.Now()
	}
	if !end.After(start) {
		return 0, false
	}
	return end.Sub(start), true
}

// subagentEntries is the child's conversation. It does not reuse the frame
// cache, which holds only the main view's thread.
func (w *workbench) subagentEntries(threadID string) []conversationEntry {
	var entries []conversationEntry
	for _, r := range w.runs {
		if r.ThreadID == threadID {
			entries = append(entries, conversationEntry{id: r.ID, run: r})
		}
	}
	if len(entries) > 1 {
		entries = sortedConversation(entries)
	}
	if c := w.composers[threadID]; c != nil && c.pending != nil {
		entries = append(entries, conversationEntry{id: c.pending.key(), pending: c.pending})
	}
	return entries
}

func sortedConversation(entries []conversationEntry) []conversationEntry {
	out := append([]conversationEntry(nil), entries...)
	for i := 1; i < len(out); i++ {
		item := out[i]
		j := i
		for j > 0 && out[j-1].createdAt().After(item.createdAt()) {
			out[j] = out[j-1]
			j--
		}
		out[j] = item
	}
	return out
}

func (w *workbench) subagentTranscript(c *ui.Context, p palette, threadID string) {
	entries := w.subagentEntries(threadID)
	if len(entries) == 0 {
		ui.Column(c).Grow(1).MinHeight(0).Padding(24, 16).Justify(ui.Center).AlignItems(ui.Center).Gap(8).Children(func() {
			ui.Icon(c, agentsIcon).FontSize(22).TextColor(p.faint)
			ui.Text(c, w.tr("这个子代理还没有消息。", "This subagent has no messages yet.")).FontSize(12.5).TextColor(p.muted).TextAlign(ui.Center).Wrap()
		})
		return
	}
	state := w.logState("subagent:" + threadID)
	state.Key = func(i int) any { return entries[i].id }
	state.Label = func(i int) string { return entries[i].inputText() }
	state.FollowEnd = true
	ui.List(c.Key("subagent-log:"+threadID), state, len(entries), func(i int) {
		w.conversationRow(c, p, entries[i], true)
	}).Grow(1).MinHeight(0).Gap(22).Padding(16, 12, 18).Role(ui.RoleLog).Label(w.tr("子代理过程", "Subagent transcript"))
}

func (w *workbench) subagentComposer(c *ui.Context, p palette, st *subagentPanelMemory, threadID string) {
	comp := w.composerFor(threadID)
	draft := comp.draft
	run, active := w.activeRun(threadID)
	// w.selected stays on the parent, so steer checks the child itself.
	canSteer := active && run.Status == api.RunRunning && run.AttemptID != "" && w.piThread(threadID)
	steer := active && followUpSteer(w.widgets.steerMode, canSteer, false)
	ui.Column(c.Key("subagent-composer")).Shrink(0).Padding(8, 12, 12).Gap(6).
		BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.surface).Children(func() {
		box := ui.Column(c).FillWidth().Padding(2).Radius(14).Background(p.raised).Border(1, p.line)
		if box.FocusWithin() {
			box.BorderColor(p.muted.Alpha(0.42))
		}
		box.Children(func() {
			placeholder := w.tr("给这个子代理发消息…", "Message this subagent…")
			if active && steer {
				placeholder = w.tr("运行中：发送会追加到当前任务…", "Running: send steers the current turn…")
			} else if active {
				placeholder = w.tr("运行中：发送会加入队列…", "Running: send joins the queue…")
			}
			field := ui.TextAreaBase(c.Key("subagent-draft:"+threadID), &draft).Lines(1, 4).FillWidth().Padding(8, 10, 4).FontSize(13.5).LineHeight(1.45)
			field.Placeholder(placeholder).Label(w.tr("子代理消息", "Subagent message"))
			field.Bind(&st.draftFocus)
			if st.focusPending {
				st.focusPending = false
				st.draftFocus.Focus()
			}
			if draft != comp.draft {
				w.setDraft(threadID, draft)
			}
			field.OnTextKey(func(k ui.TextKey) bool { return w.composerKey(threadID, k) })
			ui.Row(c).Padding(0, 4, 4).Gap(6).AlignItems(ui.Center).Children(func() {
				hint := w.tr("结束后作为新的一轮", "Sends a new turn when idle")
				if active && steer {
					hint = w.tr("追加到正在运行的任务", "Steers the running turn")
				} else if active {
					hint = w.tr("排到当前任务之后", "Queues after the running turn")
				}
				ui.Text(c, hint).FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
				label := w.tr("发送给子代理", "Send to subagent")
				icon := composerSendIcon
				if active && steer {
					label, icon = w.tr("追加信息给子代理", "Steer the subagent"), steerIcon
				} else if active {
					label, icon = w.tr("加入队列给子代理", "Queue for the subagent"), queueIcon
				}
				hasContent := strings.TrimSpace(draft) != "" || len(comp.attachments) > 0
				send := ui.PrimaryButton(c.Key("subagent-send"), "").Label(label).Tooltip(label).Height(28).Padding(0, 10).Radius(8).Shrink(0)
				if !hasContent {
					send.Background(p.selected).TextColor(p.faint)
				}
				if comp.pending != nil || comp.uploading || !hasContent || steer && comp.steering != nil {
					send.Disabled(true)
				}
				send.Children(func() {
					ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, icon).FontSize(13)
						ui.Text(c, label).FontSize(12).FontWeight(500).NoWrap()
					})
				})
				if send.Clicked() {
					w.setDraft(threadID, draft)
					if active {
						w.queueDraft(threadID, steer)
					} else {
						w.send(threadID)
					}
				}
			})
		})
		if comp.feedback.valid() {
			w.noticeLine(c, p, comp.feedback)
		}
	})
}
