package desktop

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// Thread governance commands. They are api constants now; the local names stay
// for the desktop's callers and tests.
const (
	commandForkThread   = api.ForkThread
	commandDeleteThread = api.DeleteThread
	eventThreadDeleted  = "thread.deleted"
)

type sessionGovernance struct {
	unread        map[string]bool
	deleteConfirm string // retired two-step delete; always empty now
	forking       map[string]bool
	deleting      map[string]bool

	// renamingID is the sidebar row being renamed inline, renameText its value.
	renamingID string
	renameText string
	// renameSelect asks the inline field to select its whole title on its
	// first frame, so the start of a long title is in view and typing
	// replaces it, as T3 Code's rename input does.
	renameSelect bool

	// history is the visited threads, for the sidebar's ← and → buttons.
	history    []string
	historyAt  int
	navigating bool
}

// markUnread notes a background session whose run just settled. The current
// session never collects a mark: its result is already on screen.
func (w *workbench) markUnread(threadID string) {
	if threadID == "" || threadID == w.selected {
		return
	}
	if w.sessions.unread == nil {
		w.sessions.unread = map[string]bool{}
	}
	w.sessions.unread[threadID] = true
}

func (w *workbench) isUnread(id string) bool { return w.sessions.unread[id] }

func (w *workbench) clearUnread(id string) { delete(w.sessions.unread, id) }

type forkOptions struct {
	ThreadID    string
	RunID       string
	Model       string
	ProviderID  string
	Title       string
	Branch      string
	UseWorktree bool
}

// forkThread asks Core for a new thread that starts from id's history, up to
// runID when given. The fork is selected once the receipt names it.
func (w *workbench) forkThread(id, runID string) bool {
	return w.forkThreadWithOptions(forkOptions{ThreadID: id, RunID: runID})
}

func (w *workbench) forkThreadWithOptions(opts forkOptions) bool {
	id, runID := opts.ThreadID, opts.RunID
	if id == "" || !w.hasThread(id) || isLocalSession(id) {
		return false
	}
	if w.sessions.forking == nil {
		w.sessions.forking = map[string]bool{}
	}
	key := id + ":" + runID
	if w.sessions.forking[key] {
		return false
	}
	w.sessions.forking[key] = true
	cmd := api.Command{
		Version:    api.ProtocolVersion,
		CommandID:  newCommandID("fork"),
		Kind:       commandForkThread,
		ThreadID:   id,
		RunID:      runID,
		Model:      strings.TrimSpace(opts.Model),
		ProviderID: strings.TrimSpace(opts.ProviderID),
		Title:      strings.TrimSpace(opts.Title),
	}
	if opts.UseWorktree {
		if th, ok := w.threadByID(id); ok && th.Workspace != nil {
			repo := strings.TrimSpace(th.Workspace.Repository)
			if repo == "" {
				repo = strings.TrimSpace(th.Workspace.Path)
			}
			if repo != "" {
				cmd.Workspace = &api.WorkspaceSpec{
					Kind:       "worktree",
					Repository: repo,
					Branch:     strings.TrimSpace(opts.Branch),
					BaseRef:    strings.TrimSpace(th.Workspace.BaseRef),
				}
			}
		}
	}
	w.setAction("正在分叉会话…")
	w.dispatch(cmd, func(receipt api.Receipt, err error) {
		w.post(func() {
			delete(w.sessions.forking, key)
			if err != nil {
				w.setAction(describe("分叉会话失败", err))
				return
			}
			if !receipt.Accepted || receipt.ThreadID == "" || receipt.ThreadID == id {
				w.setAction("本地服务未接受分叉请求，原会话不受影响。")
				return
			}
			w.setAction("")
			if !w.hasThread(receipt.ThreadID) {
				w.awaitingThread = receipt.ThreadID
				w.composerFor(receipt.ThreadID)
			}
			w.selectThread(receipt.ThreadID)
			w.widgets.composerFocus.Focus()
		})
	})
	return true
}

// requestDelete deletes a thread in one click, as the workbench does: no second
// confirmation. Force lets Core remove a thread whose turn or subtasks are
// still running. A delete already in flight for the thread is not repeated.
func (w *workbench) requestDelete(id string) bool {
	w.sessions.deleteConfirm = ""
	if id == "" || isLocalSession(id) || !w.hasThread(id) {
		return false
	}
	if w.sessions.deleting == nil {
		w.sessions.deleting = map[string]bool{}
	}
	if w.sessions.deleting[id] {
		return false
	}
	w.sessions.deleting[id] = true
	cmd := api.Command{Version: api.ProtocolVersion, CommandID: newCommandID("delete"), Kind: api.DeleteThread, ThreadID: id, Force: true}
	w.dispatch(cmd, func(receipt api.Receipt, err error) {
		w.post(func() {
			delete(w.sessions.deleting, id)
			if err != nil {
				w.setAction(describe("删除会话失败", err))
				return
			}
			if !receipt.Accepted {
				w.setAction("本地服务未接受删除请求，会话已保留。")
				return
			}
			w.forgetThread(id)
		})
	})
	return true
}

// deleteThread is requestDelete under the workbench's name.
func (w *workbench) deleteThread(id string) bool { return w.requestDelete(id) }

func (w *workbench) cancelDelete() { w.sessions.deleteConfirm = "" }

// confirmDelete is kept for older callers; deleting needs no confirmation.
func (w *workbench) confirmDelete() bool {
	id := w.sessions.deleteConfirm
	w.sessions.deleteConfirm = ""
	return w.requestDelete(id)
}

// discardDraft drops an unsent local draft session from the sidebar.
func (w *workbench) discardDraft(id string) bool {
	if !isLocalSession(id) {
		return false
	}
	if c := w.composers[id]; c != nil && (c.pending != nil || c.uploading) {
		return false
	}
	keep := w.localOrder[:0]
	for _, local := range w.localOrder {
		if local != id {
			keep = append(keep, local)
		}
	}
	w.localOrder = keep
	delete(w.composers, id)
	w.forgetHistory(id)
	w.navigation.valid = false
	if w.selected == id {
		w.selected = ""
		w.selectThread("")
		if w.selected == "" {
			w.newSession()
		}
	}
	w.saveState()
	return true
}

// forgetThread removes a deleted thread and everything projected under it.
func (w *workbench) forgetThread(id string) {
	keepThreads := w.threads[:0]
	for _, t := range w.threads {
		if t.ID != id {
			keepThreads = append(keepThreads, t)
		}
	}
	w.threads = keepThreads
	keepRuns := w.runs[:0]
	for _, r := range w.runs {
		if r.ThreadID != id {
			keepRuns = append(keepRuns, r)
		} else {
			w.releaseOutput(r.ID)
		}
	}
	w.runs = keepRuns
	if c := w.composers[id]; c != nil && c.pending == nil {
		delete(w.composers, id)
	}
	delete(w.sessions.unread, id)
	w.forgetHistory(id)
	if w.sessions.renamingID == id {
		w.sessions.renamingID = ""
	}
	w.frame = frameIndex{}
	w.navigation.valid = false
	if w.selected == id {
		w.selected = ""
		w.selectThread("")
		if w.selected == "" {
			w.newSession()
		}
	}
	w.saveState()
}

func (w *workbench) foldThreadDeleted(ev api.Event) bool {
	id := ev.ThreadID
	if id == "" {
		var payload struct {
			ThreadID string      `json:"threadId"`
			Thread   *api.Thread `json:"thread"`
		}
		if json.Unmarshal(ev.Data, &payload) == nil {
			id = payload.ThreadID
			if id == "" && payload.Thread != nil {
				id = payload.Thread.ID
			}
		}
	}
	if id == "" {
		return false
	}
	if w.hasThread(id) {
		w.forgetThread(id)
	}
	return true
}

// deleteConfirmView is retired: deleting takes one click.
func (w *workbench) deleteConfirmView(c *ui.Context, p palette, id string) {}

// ------------------------------------------------------------- history

// noteVisit records a thread switch for the ← / → buttons. Stepping through
// the history itself does not record.
func (w *workbench) noteVisit(previous, id string) {
	s := &w.sessions
	if s.navigating || id == "" {
		return
	}
	if len(s.history) == 0 && previous != "" && previous != id {
		s.history, s.historyAt = []string{previous}, 0
	}
	if s.historyAt < len(s.history)-1 {
		s.history = s.history[:s.historyAt+1]
	}
	if n := len(s.history); n > 0 && s.history[n-1] == id {
		s.historyAt = n - 1
		return
	}
	s.history = append(s.history, id)
	if len(s.history) > 64 {
		s.history = append([]string(nil), s.history[len(s.history)-64:]...)
	}
	s.historyAt = len(s.history) - 1
}

func (w *workbench) historyTarget(delta int) (int, bool) {
	s := &w.sessions
	for i := s.historyAt + delta; i >= 0 && i < len(s.history); i += delta {
		if id := s.history[i]; id != w.selected && w.hasSession(id) {
			return i, true
		}
	}
	return 0, false
}

func (w *workbench) canGoBack() bool    { _, ok := w.historyTarget(-1); return ok }
func (w *workbench) canGoForward() bool { _, ok := w.historyTarget(1); return ok }

// goHistory steps back (-1) or forward (+1) through visited threads.
func (w *workbench) goHistory(delta int) bool {
	at, ok := w.historyTarget(delta)
	if !ok {
		return false
	}
	w.sessions.historyAt = at
	w.sessions.navigating = true
	w.selectThread(w.sessions.history[at])
	w.sessions.navigating = false
	return true
}

// renameHistory follows a local draft to the thread Core created for it.
func (w *workbench) renameHistory(oldID, newID string) {
	for i, id := range w.sessions.history {
		if id == oldID {
			w.sessions.history[i] = newID
		}
	}
}

func (w *workbench) forgetHistory(id string) {
	s := &w.sessions
	keep := s.history[:0]
	at := s.historyAt
	for i, visited := range s.history {
		if visited == id {
			if i <= s.historyAt {
				at--
			}
			continue
		}
		keep = append(keep, visited)
	}
	s.history = keep
	s.historyAt = max(0, min(at, len(keep)-1))
}

// ------------------------------------------------------- row actions

// settleThread files a thread on the Settled shelf, or brings it back.
func (w *workbench) settleThread(id string, settled bool) bool {
	thread, ok := w.threadByID(id)
	if !ok || thread.Archived == settled {
		return false
	}
	if settled && w.threadWorking(id) {
		return false
	}
	value := settled
	w.updateThread(api.Command{ThreadID: id, Archived: &value})
	return true
}

// snoozeThread hides a thread until the given time; a zero time wakes it.
func (w *workbench) snoozeThread(id string, until time.Time) bool {
	if _, ok := w.threadByID(id); !ok {
		return false
	}
	value := until
	w.updateThread(api.Command{ThreadID: id, SnoozedUntil: &value})
	return true
}

// snoozed reports a thread snoozed past now.
func snoozed(thread api.Thread, now time.Time) bool {
	return thread.SnoozedUntil != nil && thread.SnoozedUntil.After(now)
}

// snoozePresets are the workbench's snooze choices relative to now.
func (w *workbench) snoozePresets(now time.Time) []struct {
	label string
	until time.Time
} {
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 9, 0, 0, 0, now.Location())
	return []struct {
		label string
		until time.Time
	}{
		{w.tr("1 小时后", "In 1 hour"), now.Add(time.Hour)},
		{w.tr("4 小时后", "In 4 hours"), now.Add(4 * time.Hour)},
		{w.tr("明天上午 9:00", "Tomorrow 9:00"), tomorrow},
	}
}

// snoozeMenu fills a menu with the snooze presets, or the wake item.
func (w *workbench) snoozeMenu(m *ui.Menu, thread api.Thread, now time.Time) {
	if snoozed(thread, now) {
		if m.Item(w.tr("取消提醒", "Wake thread")).Chosen() {
			w.snoozeThread(thread.ID, time.Time{})
		}
		return
	}
	for _, preset := range w.snoozePresets(now) {
		if m.Item(preset.label).Chosen() {
			w.snoozeThread(thread.ID, preset.until)
		}
	}
}

// beginRename starts the inline rename of a sidebar row.
func (w *workbench) beginRename(id string) {
	if !w.hasThread(id) {
		return
	}
	w.sessions.renamingID, w.sessions.renameText = id, w.titleFor(id)
	w.sessions.renameSelect = true
}

// commitRename saves the inline rename; an empty or unchanged title is a no-op.
func (w *workbench) commitRename() bool {
	id, title := w.sessions.renamingID, strings.TrimSpace(w.sessions.renameText)
	w.sessions.renamingID, w.sessions.renameText = "", ""
	if id == "" || title == "" || !w.hasThread(id) || title == w.titleFor(id) {
		return false
	}
	w.updateThread(api.Command{ThreadID: id, Title: title})
	return true
}

func (w *workbench) cancelRename() { w.sessions.renamingID, w.sessions.renameText = "", "" }

// regenerateTitle derives a clean title from the thread's latest instruction.
func (w *workbench) regenerateTitle(id string) bool {
	run, ok := w.latestRun(id)
	if !ok || strings.TrimSpace(run.Input) == "" {
		return false
	}
	title := titleFromInstruction(run.Input)
	if title == newSessionTitle || title == w.titleFor(id) {
		return false
	}
	w.updateThread(api.Command{ThreadID: id, Title: title})
	return true
}

// setUnread marks or clears a thread's unread dot by hand.
func (w *workbench) setUnread(id string, unread bool) {
	if !unread {
		w.clearUnread(id)
		return
	}
	if w.sessions.unread == nil {
		w.sessions.unread = map[string]bool{}
	}
	w.sessions.unread[id] = true
}

// searchSnippet finds the first run text that matches query and returns a
// short excerpt around it, for the sidebar's second line during search.
func (w *workbench) searchSnippet(id, query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}
	lower := strings.ToLower(query)
	if strings.Contains(strings.ToLower(w.titleFor(id)), lower) {
		return ""
	}
	for _, r := range w.runsFor(id) {
		for _, text := range []string{r.Input, r.Output} {
			if snippet, ok := excerpt(text, query, 14); ok {
				return "匹配对话：" + snippet
			}
		}
	}
	if c := w.composers[id]; c != nil {
		if snippet, ok := excerpt(c.draft, query, 14); ok {
			return "匹配草稿：" + snippet
		}
	}
	return ""
}

// excerpt is a case-insensitive, rune-safe window of radius runes around the
// first occurrence of query in text, flattened to one line.
func excerpt(text, query string, radius int) (string, bool) {
	if text == "" || query == "" {
		return "", false
	}
	lowerText, lowerQuery := strings.ToLower(text), strings.ToLower(query)
	at := strings.Index(lowerText, lowerQuery)
	if at < 0 || len(lowerText) != len(text) {
		if at < 0 {
			return "", false
		}
		// Lowercasing changed byte lengths (rare scripts): fall back to a prefix.
		return flattenLine(text, radius*2), true
	}
	runes := []rune(text)
	start := utf8.RuneCountInString(text[:at])
	end := start + utf8.RuneCountInString(query)
	from, to := max(0, start-radius), min(len(runes), end+radius)
	out := flattenLine(string(runes[from:to]), radius*2+len(query))
	if from > 0 {
		out = "…" + out
	}
	if to < len(runes) {
		out += "…"
	}
	return out, true
}

func flattenLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}
