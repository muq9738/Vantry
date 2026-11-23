package desktop

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// sidebarRow is one thread row: a two-line block, as T3 Stardust's list. The
// first line is the row's state glyph, its title and its time (or, under the
// pointer, its quick actions); the second line states what the thread is
// doing in the colour of that state - running, needs approval, new result,
// failed - followed by its progress and where it lives. Inside a project
// group the project is the header, so the second line names the branch;
// in the Running, Needs-you and Pinned sections it names the project.
//
// The row stays a native tree item: its name, selection, description,
// keyboard navigation, drag and context menu are the Sidebar's. entry names
// the owning machine and thread ID; every read and action goes to entry's
// own workbench so equal IDs on two machines stay two distinct sessions.
func (w *workbench) sidebarRow(c *ui.Context, p palette, entry sidebarEntry, now time.Time, showProject bool) {
	owner, id := entry.owner, entry.id
	if owner == nil {
		owner = w
	}
	thread, committed := owner.threadByID(id)
	title := owner.displayTitleFor(id)
	if !committed && title == newSessionTitle {
		title = w.tr(newSessionTitle, "New thread")
	}
	renaming := committed && owner.sessions.renamingID == id
	composite := scopedSessionID(entry.scope, id)
	representingActive := w.currentScopedID() == composite
	if !representingActive && w.currentScopedID() != "" {
		if scope, openID, ok := parseScopedSessionID(w.currentScopedID()); ok && scope == entry.scope && openID != "" {
			for _, desc := range owner.descendantThreadIDs(id) {
				if desc == openID {
					representingActive = true
					break
				}
			}
		}
	}
	// Keep the real label for native typeahead; custom children paint it once.
	item := ui.SidebarItem(c, composite, nil, title).Label(title).Selected(representingActive).Radius(10).TextColor(ui.Transparent)
	if renaming {
		item.Role(ui.RoleGroup) // Expose the nested native rename field.
	}
	if committed && !renaming {
		item.Drag(threadDrag{owner: owner, id: id})
		if item.Dragging() {
			w.widgets.draggingThread = true
			// MyGo paints the same element as the moving drag preview and
			// resets its opacity there. Hide its original card so a small
			// pointer movement cannot superimpose two copies of every line.
			item.Opacity(0)
		}
	}
	if item.Clicked() {
		w.widgets.navActivate = true
		if w.currentScopedID() != composite {
			w.openScopedSession(composite)
		}
	}
	bounds := item.Bounds()
	item.ObserveInput(func(ev ui.InputEvent) {
		// A double press on the title area edits it in place, once, on the
		// machine that owns the thread. Ignore double clicks on the top-right
		// hover actions so snooze/settle buttons never start a rename.
		inActions := bounds.W > 0 && ev.Y < bounds.Y+26 && ev.X > bounds.X+bounds.W-68
		if ev.Kind == ui.InputPointerDown && ev.Button == 0 && ev.Mods == 0 && ev.Clicks >= 2 &&
			!inActions && committed && owner.sessions.renamingID == "" {
			owner.beginRename(id)
		}
	})
	comp := owner.composers[id]
	liveAgents, totalAgents := owner.threadSubagentCounts(id)
	needsYou, working := owner.familyNeedsYou(id), owner.threadWorking(id)
	hasDraft := comp != nil && (strings.TrimSpace(comp.draft) != "" || len(comp.attachments) > 0)
	unconfirmed := comp != nil && comp.pending != nil && !comp.pending.accepted
	unread := owner.isUnread(id) && !(w.environments != nil && w.environments.current == owner && owner.selected == id)
	live := rowLiveState(w, p, owner, id, thread, now, needsYou, working, liveAgents, totalAgents, unconfirmed, unread, hasDraft)
	project := workspaceName(thread.Workspace)
	host := w.sidebarMachineLabel(owner)
	// A thread without a project is named by the machine that runs it, so
	// every row still says where it lives.
	identity := nonEmptyString(project, host)
	if identity == "" {
		identity = w.tr("会话", "Thread")
	}
	// Another machine's name is worth the line only while the list mixes
	// machines: this window's own threads are the default reading.
	machine := ""
	if w.widgets.sidebarMultiMachine && !owner.isLocalMachine() && project != "" {
		machine = host
	}
	branch := owner.threadBranch(id, thread)
	detail, detailIcon := branch, branchIcon
	if thread.Workspace != nil && (thread.Workspace.Kind == "worktree" || thread.Workspace.Repository != "") {
		detailIcon = worktreeIcon
	}
	searchNote := owner.subagentSearchNote(id, w.sidebarSearch())
	if snippet := owner.searchSnippet(id, w.sidebarSearch()); snippet != "" {
		detail, detailIcon = firstLine(snippet), searchIcon
	}
	if searchNote != "" && detailIcon != searchIcon {
		detail, detailIcon = searchNote, agentsIcon
	}
	if detail == "" {
		detail, detailIcon = owner.threadModelLabel(thread), sparkIcon
	}
	progress := owner.rowProgress(id, working)
	// The accessible description states the same facts the two lines draw,
	// in reading order, so a screen reader hears one complete sentence.
	facts := make([]string, 0, 6)
	addFact := func(part string) {
		if strings.TrimSpace(part) != "" {
			facts = append(facts, part)
		}
	}
	addFact(project)
	addFact(machine)
	addFact(live.text)
	if live.text == "" {
		if at := owner.sessionActivity(id); !at.IsZero() {
			addFact(w.formatAge(now, at))
		}
	}
	if progress.label != "" {
		addFact(progress.label)
	}
	if unread && live.text != w.tr("有新结果", "New result") {
		addFact(w.tr("有新结果", "new result"))
	}
	if snoozed(thread, now) {
		addFact(w.tr("已延后提醒", "snoozed"))
	}
	if thread.ParentThreadID != "" {
		addFact(w.tr("子任务，父会话：", "subtask of ") + owner.titleFor(thread.ParentThreadID))
	}
	if searchNote != "" {
		addFact(searchNote)
	}
	if branch != "" && detail != branch {
		addFact(branch)
	}
	addFact(detail)
	if len(facts) > 0 {
		item.Description(strings.Join(facts, w.tr("；", "; ")))
	}
	hovered := item.Hovered() || w.snoozeMenuOpen(composite)
	if hovered && committed && !renaming {
		w.widgets.hoverThread, w.widgets.hoverRect = composite, item.Bounds()
	}
	// The selected thread lifts off the paper sidebar as a raised card.
	if representingActive && !item.Dragging() {
		item.Background(p.card)
		if p.dark {
			item.Border(1, p.line).Shadow(0, 2, 8, 0, ui.Hex("#00000073"))
		} else {
			item.Border(1, p.lineSoft).Shadow(0, 2, 8, 0, ui.Hex("#1D1D1F18"))
		}
	}
	// A read, inactive row recedes (regular weight, most of the ink); the
	// active, unread or attention-needing ones stay bright.
	titleColor, titleWeight := p.ink.Mix(p.sidebar, 0.2), 400
	if representingActive || unread || needsYou || working || liveAgents > 0 {
		titleColor, titleWeight = p.ink, 500
	}
	item.Height(rowHeight).PaddingStart(10).PaddingEnd(8)
	item.Children(func() {
		// Line one: the state glyph, the title, and the time or actions.
		ui.Row(c.Key("row-title")).Absolute().Top(8).Left(10).Right(8).Height(18).Gap(7).AlignItems(ui.Center).Children(func() {
			rowGlyph(c, p, live)
			if !renaming {
				ui.Text(c, title).FontSize(13).FontWeight(titleWeight).TextColor(titleColor).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			}
			ui.Spacer(c)
			if renaming {
				return
			}
			if hovered && !item.Dragging() {
				w.rowActions(c, p, entry, thread, committed, working, now)
			} else if live.elapsed != "" {
				ui.Text(c, live.elapsed).FontSize(11).FontFeatures("tnum").TextColor(p.faint).Shrink(0).NoWrap()
			} else if at := owner.familyActivity(id); !at.IsZero() {
				ui.Text(c, w.formatAge(now, at)).FontSize(11).TextColor(p.faint).Shrink(0).NoWrap()
			}
		})
		if renaming {
			w.renameField(c, p, owner, composite, 0, 12)
			return
		}
		// Line two: the coloured state, the run's progress, then where the
		// thread lives and its quiet marks.
		ui.Row(c.Key("row-facts")).Absolute().Bottom(8).Left(31).Right(8).Height(16).Gap(5).AlignItems(ui.Center).Children(func() {
			if live.text != "" {
				ui.Text(c, live.text).FontSize(11).FontWeight(500).TextColor(live.color).NoWrap().Shrink(0)
			}
			if progress.total > 0 {
				miniProgress(c, p, progress.done, progress.total, live.color)
				ui.Text(c, fmt.Sprintf("%d/%d", progress.done, progress.total)).FontSize(10.5).FontFeatures("tnum").TextColor(p.faint).NoWrap().Shrink(0)
			} else if progress.label != "" {
				ui.Text(c, progress.label).FontSize(10.5).TextColor(p.faint).NoWrap().Shrink(0)
			}
			if live.text != "" || progress.label != "" {
				ui.Text(c, "·").FontSize(11).TextColor(p.faint).Shrink(0)
			}
			if showProject && project != "" && detailIcon != searchIcon {
				projectBadge(c, project, 12)
				ui.Text(c, project).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			} else {
				ui.Icon(c, detailIcon).FontSize(10.5).TextColor(p.faint).Shrink(0)
				ui.Text(c, detail).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			}
			ui.Spacer(c)
			if machine != "" {
				ui.Row(c.Key("row-machine")).Gap(3).AlignItems(ui.Center).Shrink(0).Children(func() {
					ui.Icon(c, serverIcon).FontSize(10).TextColor(p.faint).Shrink(0)
					ui.Text(c, shortMachine(machine)).FontSize(10.5).TextColor(p.faint).SingleLine().NoWrap()
				})
			}
			if thread.Pinned {
				ui.Icon(c, pinIcon).FontSize(10.5).TextColor(p.faint).Shrink(0)
			}
			if totalAgents > 0 {
				agentColor := p.faint
				if liveAgents > 0 {
					agentColor = p.running
				}
				ui.Row(c).Gap(2.5).AlignItems(ui.Center).Shrink(0).Label(fmt.Sprintf(w.tr("%d 个子代理", "%d agents"), totalAgents)).Children(func() {
					ui.Icon(c, agentsIcon).FontSize(10.5).TextColor(agentColor)
					ui.Text(c, fmt.Sprint(totalAgents)).FontSize(10.5).FontWeight(500).TextColor(agentColor).NoWrap()
				})
			}
			if committed && owner.piThread(id) {
				piMark(c, p.faint, 9.5)
			}
		})
	})
	if !committed {
		return
	}
	item.ContextMenu(func(m *ui.Menu) { w.threadMenu(c, m, owner, thread, title, working) })
}

// rowGlyph is the 14px state mark at the start of a row: a pulsing ember dot
// while it runs, a solid dot in the state's colour when it needs you, has a
// new result or failed, and an empty slot otherwise so titles stay aligned.
func rowGlyph(c *ui.Context, p palette, live rowLive) {
	ui.Row(c.Key("row-glyph")).Size(14, 14).Justify(ui.Center).AlignItems(ui.Center).Shrink(0).Children(func() {
		switch {
		case live.pulse:
			activityIndicator(c, live.color).Size(12, 12)
			c.After(time.Second) // the elapsed time keeps counting
		case live.dot:
			ui.Box(c).Size(7, 7).Radius(4).Background(live.color).Shrink(0)
		case live.icon != nil:
			ui.Icon(c, live.icon).FontSize(11).TextColor(live.color)
		}
	})
}

// miniProgress is the row's 26px run bar: done of total subtasks.
func miniProgress(c *ui.Context, p palette, done, total int, tone ui.Color) {
	frac := float32(0)
	if total > 0 {
		frac = min(1, float32(done)/float32(total))
	}
	track := p.line.Mix(p.ink, 0.12)
	ui.Box(c.Key("row-progress")).Size(26, 3.5).Radius(2).Shrink(0).Background(track).Draw(func(pt *ui.Painter, r ui.Rect) {
		if frac <= 0 {
			pt.Fill(ui.Rect{X: r.X, Y: r.Y, W: 5, H: r.H}, tone, 1.75)
			return
		}
		pt.Fill(ui.Rect{X: r.X, Y: r.Y, W: max(5, r.W*frac), H: r.H}, tone, 1.75)
	})
}

// rowProgressInfo is what a live row can say about its run from data
// already in memory: subtasks done of total, or the steps taken so far.
type rowProgressInfo struct {
	done, total int
	label       string
}

func (w *workbench) rowProgress(id string, working bool) rowProgressInfo {
	if !working {
		return rowProgressInfo{}
	}
	run, ok := w.activeRun(id)
	if !ok {
		return rowProgressInfo{}
	}
	var info rowProgressInfo
	for _, task := range w.tasks {
		if task.ParentRunID != run.ID {
			continue
		}
		info.total++
		if task.Status.Terminal() {
			info.done++
		}
	}
	if info.total > 0 {
		info.label = fmt.Sprintf(w.tr("子任务 %d/%d", "subtasks %d/%d"), info.done, info.total)
		return info
	}
	if steps := len(w.runTools(run.ID)); steps > 0 {
		info.label = fmt.Sprintf(w.tr("%d 步", "%d steps"), steps)
	}
	return info
}

// renameField is the inline rename inside a row: Enter saves, Escape cancels,
// and leaving the field commits. It reads as a real field over the row (a
// darker well with the focus ring), opens with the whole title selected so
// its start stays in view, and keeps zero vertical padding so MyGo centers
// the 13px line in the 26px box.
func (w *workbench) renameField(c *ui.Context, p palette, owner *workbench, composite string, indent, top float32) {
	var field ui.Element
	ui.Row(c.Key("rename-well")).Absolute().Top(top).Left(5+indent).Right(5).Height(28).Padding(0, 8).Radius(7).
		Background(p.canvas).Border(1.5, p.accent).Shadow(0, 0, 0, 3, p.accent.Alpha(0.18)).AlignItems(ui.Center).Children(func() {
		field = ui.TextInputBase(c.Key("rename:"+composite), &owner.sessions.renameText).Label(w.tr("重命名会话", "Rename thread")).
			Grow(1).MinWidth(0).Height(24).FontSize(13).TextColor(p.ink).SelectionColor(p.accent.Alpha(0.35)).AutoFocus()
	})
	if owner.sessions.renameSelect && field.Focused() {
		// Anchor at the end, caret at the start: everything is selected and
		// the reveal scrolls the title's beginning into view.
		owner.sessions.renameSelect = false
		field.SetTextSelection(len([]rune(owner.sessions.renameText)), 0)
	}
	if field.Submitted() {
		owner.commitRename()
		return
	}
	if field.Shortcut(0, ui.KeyEscape) {
		owner.cancelRename()
		return
	}
	if field.FocusLeft() {
		owner.commitRename()
	}
}

// threadBranch is the branch already in memory: a live Git read, then the
// committed workspace spec.
func (w *workbench) threadBranch(id string, thread api.Thread) string {
	if git := w.features.git[id]; git != nil && git.Branch != "" {
		return git.Branch
	}
	if thread.Workspace != nil {
		return thread.Workspace.Branch
	}
	return ""
}

// threadSubagentCounts returns the active and total subagents/peer children of id.
func (w *workbench) threadSubagentCounts(id string) (live, total int) {
	if id == "" {
		return 0, 0
	}
	seenChild := map[string]bool{}
	for _, task := range w.tasks {
		if task.ParentThreadID != id {
			continue
		}
		if task.ChildThreadID != "" {
			seenChild[task.ChildThreadID] = true
		}
		total++
		if !task.Status.Terminal() {
			live++
		}
	}
	for _, th := range w.threads {
		if th.ID == id || seenChild[th.ID] {
			continue
		}
		if th.ParentThreadID == id || th.CreatedByThreadID == id {
			seenChild[th.ID] = true
			total++
			if w.threadWorking(th.ID) {
				live++
			}
		}
	}
	return live, total
}

// threadAgentCount is the delegated subtasks and peer threads a thread made.
func (w *workbench) threadAgentCount(id string) int {
	_, total := w.threadSubagentCounts(id)
	return total
}

func (w *workbench) snoozeMenuOpen(id string) bool {
	open := w.widgets.snoozeOpen[id]
	return open != nil && *open
}

func (w *workbench) snoozeOpenFor(id string) *bool {
	if w.widgets.snoozeOpen == nil {
		w.widgets.snoozeOpen = map[string]*bool{}
	}
	if w.widgets.snoozeOpen[id] == nil {
		w.widgets.snoozeOpen[id] = new(bool)
	}
	return w.widgets.snoozeOpen[id]
}

// rowHeight is the pitch of every list row: two lines in one block, so the
// column keeps a steady rhythm the eye can scan.
const rowHeight = 52

// rowLive is a row's live state: the glyph at the start of its first line
// and the coloured word that opens its second line.
type rowLive struct {
	text    string
	color   ui.Color
	icon    *ui.SVG
	pulse   bool   // a live run: the glyph breathes
	dot     bool   // a state that wants attention: a solid dot
	elapsed string // a live run's elapsed time, in place of its age
}

// rowLiveState reads a thread's state once, so the row and the hover card
// never disagree: unconfirmed sends and approvals first, then a live run
// (or live subagents), then the quiet outcomes.
func rowLiveState(w *workbench, p palette, owner *workbench, id string, thread api.Thread, now time.Time, needsYou, working bool, liveAgents, totalAgents int, unconfirmed, unread, hasDraft bool) rowLive {
	switch {
	case unconfirmed:
		return rowLive{text: w.tr("待确认", "Unconfirmed"), color: p.warning, dot: true}
	case needsYou:
		return rowLive{text: w.tr("待授权", "Needs approval"), color: p.warning, dot: true}
	case working:
		run, _ := owner.activeRun(id)
		live := rowLive{text: w.tr("运行中", "Running"), color: p.running, pulse: true}
		switch {
		case run.Status == api.RunWaiting && liveAgents > 0:
			live.text = fmt.Sprintf(w.tr("子代理 %d/%d", "Agents %d/%d"), liveAgents, totalAgents)
		case run.Status == api.RunWaiting:
			live = rowLive{text: w.tr("等待子任务", "Waiting"), color: p.warning, icon: clockIcon}
		}
		if !run.CreatedAt.IsZero() {
			if elapsed := now.Sub(run.CreatedAt); elapsed >= time.Second && elapsed < 48*time.Hour {
				live.elapsed = compactDuration(elapsed)
			}
		}
		return live
	case liveAgents > 0:
		return rowLive{text: fmt.Sprintf(w.tr("子代理 %d/%d", "Agents %d/%d"), liveAgents, totalAgents), color: p.running, pulse: true}
	}
	if latest, ok := owner.latestRun(id); ok && (latest.Status == api.RunFailed || latest.Status == api.RunInterrupted) {
		return rowLive{text: w.tr("失败", "Failed"), color: p.danger, dot: true}
	}
	if unread {
		return rowLive{text: w.tr("有新结果", "New result"), color: p.accent, dot: true}
	}
	if snoozed(thread, now) {
		return rowLive{text: w.tr("已延后", "Snoozed"), color: p.faint, icon: clockIcon}
	}
	if hasDraft {
		return rowLive{text: w.tr("草稿", "Draft"), color: p.warning, icon: editIcon}
	}
	return rowLive{}
}

// compactDuration is a short elapsed time: 12s, 3m, 2h, 4d.
func compactDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// rowActions are the hover controls at the top right of a row, as the workbench's:
// snooze and Settle for an active thread, Unsettle on the shelf, and discard
// for an unsent draft. Each takes one click and reaches only the row's own
// machine.
func (w *workbench) rowActions(c *ui.Context, p palette, entry sidebarEntry, thread api.Thread, committed, working bool, now time.Time) {
	owner, id, composite := entry.owner, entry.id, scopedSessionID(entry.scope, entry.id)
	if owner == nil {
		owner = w
	}
	ui.Row(c.Key("row-actions")).Gap(3).AlignItems(ui.Center).Shrink(0).Children(func() {
		if !committed {
			if !isLocalSession(id) {
				return
			}
			discard := rowIconButton(c, p, "discard:"+composite, w.tr("丢弃草稿", "Discard draft"), closeIcon)
			if discard.Clicked() {
				owner.discardDraft(id)
			}
			return
		}
		if thread.Archived {
			back := rowPill(c, p, "unsettle:"+composite, w.tr("移回", "Unsettle"), unsettleIcon)
			back.Description(w.tr("移回活跃会话", "Move back to active threads"))
			if back.Clicked() {
				owner.settleThread(id, false)
			}
			return
		}
		snooze := rowIconButton(c, p, "snooze:"+composite, w.tr("稍后提醒", "Snooze thread"), clockIcon)
		if snoozed(thread, now) {
			snooze.TextColor(p.accentInk)
		}
		snooze.PopoverMenuButton(func(m *ui.Menu) { owner.snoozeMenu(m, thread, now) },
			ui.PopoverMenuButtonOptions{At: ui.AnchorBottomEnd, Self: ui.AnchorTopEnd, Open: w.snoozeOpenFor(composite)})
		settle := rowPill(c, p, "settle:"+composite, w.tr("归档", "Settle"), checkIcon)
		settle.Description(w.tr("归档会话", "Settle thread"))
		if working {
			settle.Disabled(true).Tooltip(w.tr("会话仍在运行", "The thread is still running"))
		}
		if settle.Clicked() {
			owner.settleThread(id, true)
		}
	})
}

// rowIconButton is a row's single-icon hover action, sized to the first line.
func rowIconButton(c *ui.Context, p palette, key, label string, icon *ui.SVG) ui.Element {
	button := ui.Button(c.Key(key), "").Label(label).Tooltip(label).Size(20, 20).Padding(3).Shrink(0).Radius(5).
		Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
	if button.Hovered() {
		button.Background(p.soft).TextColor(p.ink)
	}
	button.Children(func() { ui.Icon(c, icon).FontSize(12) })
	return button
}

// rowPill is a compact icon-and-word button for a row's hover actions.
func rowPill(c *ui.Context, p palette, key, label string, icon *ui.SVG) ui.Element {
	pill := ui.Button(c.Key(key), "").Label(label).Tooltip(label).Height(20).Padding(0, 6).Radius(5).Shrink(0).
		Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
	if pill.Hovered() {
		pill.Background(p.soft).TextColor(p.ink)
	}
	pill.Children(func() {
		ui.Row(c).Gap(3).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon).FontSize(11.5)
			ui.Text(c, label).FontSize(11).NoWrap()
		})
	})
	return pill
}

// threadModelLabel is the display name of a thread's model.
func (w *workbench) threadModelLabel(thread api.Thread) string {
	model := thread.Model
	if model == "" {
		model = w.configuredDefaultModel()
	}
	if model == "" {
		return w.tr("默认模型", "Default model")
	}
	for _, item := range w.models {
		if item.Name == "" {
			continue
		}
		if item.Key() == model || strings.HasPrefix(model, item.Key()+":") {
			return item.Name
		}
	}
	return shortModel(model)
}

// threadHoverCard is the workbench's ThreadHoverCard: a quiet summary of the hovered
// row beside the sidebar. It takes no input and is hidden from assistive
// technology, which reads the same facts from the row's description.
func (w *workbench) threadHoverCard(c *ui.Context, p palette, sidebarWidth float32) {
	composite := w.widgets.hoverThread
	if composite == "" || w.widgets.draggingThread || w.snoozeMenuOpen(composite) {
		return
	}
	scope, id, ok := parseScopedSessionID(composite)
	if !ok {
		return
	}
	owner := w
	if h := w.environments; h != nil {
		record := h.recordByScope(scope)
		if record == nil || record.view == nil {
			return
		}
		owner = record.view
	}
	const delay = 450 * time.Millisecond
	if elapsed := c.Now().Sub(w.widgets.hoverSince); elapsed < delay {
		c.After(delay - elapsed)
		return
	}
	thread, ok := owner.threadByID(id)
	rect := w.widgets.hoverRect
	if !ok || rect.H <= 0 {
		return
	}
	_, height := c.Size()
	y := min(max(rect.Y, 8), max(float32(8), height-170))
	project := workspaceName(thread.Workspace)
	branch := owner.threadBranch(id, thread)
	ui.Overlay(c, func() {
		card := ui.Column(c.Key("thread-hover-card")).Label(w.tr("会话预览", "Thread preview")).Absolute().Left(sidebarWidth+6).Top(y).Width(280).Padding(10, 12).Gap(6).
			Radius(10).Background(p.raised).Border(1, p.line).Shadow(0, 6, 18, 0, ui.Hex("#00000040")).
			PassThrough().AccessHidden(true)
		card.Children(func() {
			ui.Text(c, owner.titleFor(id)).FontSize(12).Bold().TextColor(p.ink).Wrap().MaxLines(2)
			line := func(icon func(), text string) {
				ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
					ui.Row(c).Size(14, 14).Justify(ui.Center).AlignItems(ui.Center).Shrink(0).Children(icon)
					ui.Text(c, text).FontSize(11).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(0)
				})
			}
			if project != "" {
				line(func() { projectBadge(c, project, 13) }, project)
			}
			line(func() { ui.Icon(c, serverIcon).FontSize(12).TextColor(p.faint) }, owner.hostLabel())
			if branch != "" {
				line(func() { ui.Icon(c, branchIcon).FontSize(12).TextColor(p.faint) }, branch)
			}
			if owner.piThread(id) {
				line(func() { piMark(c, p.muted, 11) }, owner.threadModelLabel(thread))
			}
		})
	})
}

// threadMenu is the thread's actions, aligned with the workbench's
// buildThreadActionMenuItems, shared by its sidebar row. owner is the machine
// that runs the thread; every command reaches that Core only.
func (w *workbench) threadMenu(c *ui.Context, m *ui.Menu, owner *workbench, thread api.Thread, title string, working bool) {
	id := thread.ID
	now := time.Now()
	branch := owner.threadBranch(id, thread)
	if m.Item(w.tr("在会话内查找", "Find in thread")).Chosen() {
		w.showThread(owner, id)
		owner.timelineNav.open = true
		owner.timelineNav.focus.Focus()
	}
	if thread.Workspace != nil {
		if m.Item(w.tr("在此项目新建会话", "New thread in project")).Chosen() {
			w.showThread(owner, "")
			owner.newSessionIn(thread.Workspace)
		}
	}
	pin := w.tr("置顶会话", "Pin thread")
	if thread.Pinned {
		pin = w.tr("取消置顶", "Unpin thread")
	}
	if m.Item(pin).Chosen() {
		value := !thread.Pinned
		owner.updateThread(api.Command{ThreadID: id, Pinned: &value})
	}
	settle := w.tr("归档会话", "Settle thread")
	if thread.Archived {
		settle = w.tr("移回活跃", "Unsettle")
	}
	if m.Item(settle).Disabled(working && !thread.Archived).Chosen() {
		owner.settleThread(id, !thread.Archived)
	}
	m.Submenu(w.tr("稍后提醒", "Snooze"), func(m *ui.Menu) { owner.snoozeMenu(m, thread, now) })
	m.Separator()
	if m.Item(w.tr("重命名会话", "Rename")).Chosen() {
		owner.beginRename(id)
	}
	if _, ok := owner.latestRun(id); ok && m.Item(w.tr("重新生成标题", "Regenerate title")).Chosen() {
		owner.regenerateTitle(id)
	}
	if owner.isUnread(id) {
		if m.Item(w.tr("标为已读", "Mark read")).Chosen() {
			owner.setUnread(id, false)
		}
	} else if m.Item(w.tr("标为未读", "Mark unread")).Chosen() {
		owner.setUnread(id, true)
	}
	if m.Item(w.tr("从此处分叉会话", "Fork thread")).Disabled(owner.sessions.forking[id+":"]).Chosen() {
		owner.forkThread(id, "")
	}
	if thread.ParentThreadID != "" && m.Item(w.tr("打开父会话", "Open parent thread")).Chosen() {
		w.showThread(owner, thread.ParentThreadID)
	}
	if key := workspaceKey(thread.Workspace); key != "" {
		label := w.tr("按此项目筛选", "Filter by project")
		if w.sidebarProjectFilter() == key {
			label = w.tr("取消项目筛选", "Clear project filter")
		}
		if m.Item(label).Chosen() {
			if w.sidebarProjectFilter() == key {
				w.setProjectFilter("")
			} else {
				w.setProjectFilter(key)
			}
		}
	}
	m.Separator()
	m.Submenu(w.tr("复制", "Copy"), func(m *ui.Menu) {
		if path := workspaceKey(thread.Workspace); path != "" && m.Item(w.tr("路径", "Path")).Chosen() {
			c.WriteClipboard(path)
		}
		if branch != "" && m.Item(w.tr("分支名", "Branch")).Chosen() {
			c.WriteClipboard(branch)
		}
		if m.Item(w.tr("会话链接", "Thread link")).Chosen() {
			c.WriteClipboard(api.ThreadReference(thread))
		}
		if m.Item(w.tr("会话标题", "Thread title")).Chosen() {
			c.WriteClipboard(title)
		}
		if m.Item(w.tr("会话 ID", "Thread ID")).Chosen() {
			c.WriteClipboard(id)
		}
	})
	if thread.Workspace != nil && m.Item(w.tr("在终端中打开", "Open in Terminal")).Disabled(owner.terminalCalls[id]).Chosen() {
		w.showThread(owner, id)
		owner.openSystemTerminal()
	}
	m.Separator()
	if m.Item(w.tr("删除", "Delete")).Disabled(owner.sessions.deleting[id]).Chosen() {
		owner.requestDelete(id)
	}
}

// sidebarFooter is the workbench's SidebarUtilityMenu: settings, Git and usage as
// icons on the left, the appearance switch on the right. Language lives in
// Settings.
func (w *workbench) sidebarFooter(c *ui.Context, p palette, width float32) {
	// The settled shelf above already draws the rule; the footer sits
	// flush below it so the bottom of the sidebar has one line, not two.
	ui.Row(c.Key("sidebar-footer")).Height(42).Padding(0, 8, 2).Gap(2).AlignItems(ui.Center).Shrink(0).Children(func() {
		settings := iconButton(c, p, "open-settings", w.tr("设置", "Settings"), gearIcon).Size(28, 28).Padding(6)
		settings.Tooltip(w.tr("设置 ⌘,", "Settings ⌘,"))
		if settings.Clicked() {
			w.widgets.settingsOpen = true
		}
		git := iconButton(c, p, "footer-git", w.tr("Git 面板", "Git panel"), branchIcon).Size(28, 28).Padding(6)
		if w.features.panel == "git" {
			git.TextColor(p.accentInk)
		}
		if git.Clicked() {
			if ok, _ := w.toolAvailable("git"); ok {
				w.togglePanel("git")
			} else {
				w.beginProjectChoice()
				w.widgets.projectPickerOpen = true
				w.widgets.modelPickerOpen = false
			}
		}
		if iconButton(c, p, "open-usage", w.tr("用量", "Usage"), chartIcon).Size(28, 28).Padding(6).Clicked() {
			w.openUsage()
		}
		help := iconButton(c, p, "shortcuts", w.tr("快捷键", "Shortcuts"), helpIcon).Size(28, 28).Padding(6)
		help.Tooltip(w.tr("快捷键速查 ⌘/", "Keyboard shortcuts ⌘/"))
		if w.widgets.shortcutsOpen {
			help.TextColor(p.ink)
		}
		if help.Clicked() {
			w.widgets.shortcutsOpen = !w.widgets.shortcutsOpen
		}
		ui.Spacer(c)
		label, icon := w.tr("切换深色", "Dark appearance"), moonIcon
		if p.dark {
			label, icon = w.tr("切换浅色", "Light appearance"), sunIcon
		}
		if iconButton(c, p, "appearance", label, icon).Size(28, 28).Padding(6).Clicked() {
			w.widgets.themeSet = true
			w.widgets.dark = !p.dark
			w.saveState()
		}
		// The running version is not advertised here; only a newer one shows,
		// as a small update badge leading to Settings › About.
		w.maybeAutoCheckUpdates(time.Now())
		if w.updateBadgeVisible() {
			latest := versionLabel(w.updates.info.LatestVersion)
			label := w.tr("有新版本 ", "Update available ") + latest
			badge := ui.Button(c.Key("footer-update"), "").Label(label).Tooltip(label+w.tr(" · 点击查看并更新", " · click to review and install")).
				Size(28, 28).Padding(6).Radius(8).Shrink(0).Border(0, ui.Transparent).Background(ui.Transparent).TextColor(p.accentInk)
			if badge.Hovered() {
				badge.Background(p.accent.Alpha(0.12))
			}
			badge.Children(func() {
				ui.Icon(c, updateBadgeIcon).FontSize(16)
				ui.Box(c).Absolute().Right(4).Top(4).Size(6, 6).Radius(3).Background(p.accent)
			})
			if badge.Clicked() {
				w.widgets.settingsOpen = true
				w.widgets.settingsPage = "about"
			}
		}
	})
}
