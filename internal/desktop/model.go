// Package desktop is Vantry's native workbench: a MyGo window that talks to
// an independent Core through api.Service only.
//
// The reading flow is built from committed data: one Snapshot, then the events
// that follow its cursor. A small, explicitly separate outbox holds commands
// whose receipts have not arrived; it is UI-process state, not a claim of
// cross-process persistence. Commands run off the UI thread and their results
// return through the window's update mechanism.
package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/muq9738/Vantry/internal/desktop/webcanvas"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	newSessionTitle    = "新会话"
	localSessionPrefix = "local."
)

type noticeLevel int

const (
	noticeInfo noticeLevel = iota
	noticeWarn
	noticeError
)

// notice is contextual feedback. Successful routine synchronization produces
// no notice; only something the user needs to act on occupies the layout.
type notice struct {
	Text  string
	Level noticeLevel
}

func (n notice) valid() bool { return n.Text != "" }

// submission freezes the complete commands for one user instruction. draft is
// deliberately elsewhere: editing after send can never mutate these payloads.
// A first send owns both commands. If create succeeded but send did not, the
// received threadID and the original send command remain available for retry.
type submission struct {
	create         api.Command
	send           api.Command
	threadID       string
	runID          string
	startedAt      time.Time
	createAccepted bool
	sendStarted    bool
	accepted       bool
	inFlight       bool
}

func (s *submission) key() string {
	if s == nil {
		return ""
	}
	return "pending:" + s.send.CommandID
}

// steeringSubmission is the legacy SteerMessage outbox. It remains readable
// for older Core projections, but the integrated composer uses SteerRun and
// Snapshot.Inputs so one click never dispatches both contracts.
type steeringSubmission struct {
	command   api.Command
	messageID string
	startedAt time.Time
	accepted  bool
	inFlight  bool
	feedback  notice
}

// composer is per-session input state. Drafts can optionally be persisted by
// StatePath; focus/editing state and the unconfirmed outbox remain process-only.
// IME flags are maintained by the native input observer so a
// candidate-confirming Enter cannot become a send.
type composer struct {
	draft       string
	model       *string // explicit choice for this local draft; nil follows Pi settings
	provider    *string // execution provider for this draft; nil follows the thread or Pi
	attachments []api.Attachment
	uploading   bool
	pending     *submission
	steering    *steeringSubmission
	feedback    notice
	plan        bool // 规划模式: ask for a plan before any change

	composing          bool
	compositionEnding  bool
	lastCompositionSet time.Time

	// newlinePending defers a Cmd+Enter line break to the next frame, where
	// the live selection is known: the editor never takes the chord itself.
	// caretAfter restores the caret once the new text is in the buffer.
	newlinePending bool
	liveAnchor     int
	liveCaret      int
	caretAfter     int
	caretRestore   bool
	// skillIndex and skillDismissed are the / skill picker's selection and
	// the Esc-latched token, as filePicker does for @ mentions.
	skillIndex     int
	skillDismissed string
	// mentions are the thread links, skills, and inline pasted attachments the
	// draft shows as colored chips; send expands them back (composer_mentions.go).
	mentions []composerMention
	pasteSeq int
	// lastPaste is the most recent attachment paste, so ⌘Z can drop it when
	// nothing else has edited the draft since.
	lastPaste *composerPasteUndo
	// chipLines and chipScroll track the field so a rounded chip wash can sit
	// behind the text while it has not scrolled.
	chipLines  ui.TextLines
	chipScroll ui.ScrollState
	// previewAttachment is the thumbnail opened large, or empty.
	previewAttachment string
}

// workbench holds view state. Every field below post is touched only on the
// view thread: background work posts a closure, never mutates it directly.
type workbench struct {
	service api.Service

	post  func(fn func())
	start func(fn func())
	quit  func()
	ctx   context.Context

	// Testable host hooks for clipboard paste and editor detection.
	clipboardProbe     func(pasted string) (clipboardPastePayload, bool)
	hostClipboardMedia func(ctx context.Context) ([]byte, string, string, bool, error)
	detectedEditors    []editorOption

	sequence     int64
	threads      []api.Thread
	runs         []api.Run
	outputs      map[string]*strings.Builder // amortized appends for streaming replies
	tasks        []api.Task
	cohorts      []api.Cohort
	inputs       []api.Input
	tools        []api.ToolCall
	requests     []api.ProviderRequest
	messages     []api.QueuedMessage
	steering     []api.SteeringMessage
	defaults     Options
	defaultModel string // resolved from Pi settings/launch options, never from saved UI selection
	taskActions  map[string]api.Command
	controls     map[string]*controlAction

	models        []api.ModelInfo
	modelsLoading bool
	modelsLoaded  bool
	modelsErr     string
	modelSaving   bool
	modelErr      string
	modelCommand  *api.Command
	pickDirectory func()
	// nativeDirectoryPicker is the system folder dialog, offered from the
	// folder palette on this computer only.
	nativeDirectoryPicker func()

	workspaceChanges *api.WorkspaceChanges
	workspaceThread  string
	workspaceLoading bool
	workspaceErr     string
	workspaceNotice  notice
	workspaceVersion int
	terminalCalls    map[string]bool

	appliedEvents int64
	staleEvents   int64
	unknownEvents int64

	selected             string
	awaitingThread       string
	localSerial          int
	localOrder           []string
	startupModelPrompted bool

	inFlightCommands map[string]bool
	stoppingRuns     map[string]bool
	composers        map[string]*composer
	// skillCatalogs caches each target scope's skills.list answer and
	// skillErrs its last failure, so a list never leaks across targets.
	skillCatalogs map[string][]skillEntry
	skillErrs     map[string]string

	healthNotice notice
	connNotice   notice
	streamNotice notice
	actionNotice notice
	stateNotice  notice
	resyncReason string

	widgets        viewState
	visuals        visualState
	window         *mygo.Window
	webSnapshotter *webcanvas.Snapshotter
	frame          frameIndex
	navigation     navigationCache
	timelineNav    timelineNavigation
	projectChoice  projectChoice
	gitView        gitViewState

	toolViews  map[string]toolViewEntry
	diffStates map[string]*editor.DiffState
	patchCache struct {
		text  string
		files []patchFile
	}
	features  workbenchTools
	providers providerSettings
	sessions  sessionGovernance
	files     fileBrowser
	picker    filePicker
	queue     queueEditing
	remote    remoteFavorites

	// stage is what the main stage shows instead of the conversation: ""
	// for the conversation or a plugin-contributed page.
	stage string
	dock  dockState
	// subagentPanels is per root session: whether the side panel is open and
	// which child it shows. subagentWidth is the side-by-side width; zero is
	// the default.
	subagentPanels    map[string]*subagentPanelMemory
	subagentWidth     float32
	ssh               sshHubState
	termTab           terminalTabState
	plugs             pluginCenterState
	mcp               mcpPanelState
	rd                remoteDesktopState
	access            remoteAccessState
	environments      *environmentHub
	environmentKey    string
	environmentInfo   api.EnvironmentInfo
	remoteDirectories remoteDirectoryState
	updates           updateState
	// newTerminal makes the embedded terminals; tests replace it so no
	// shell or ssh process starts.
	newTerminal func(terminal.Options) (*terminal.Terminal, error)
}

func newWorkbench(service api.Service) *workbench {
	inline := func(fn func()) { fn() }
	return &workbench{
		service:          service,
		defaults:         Options{ProviderID: "pi", ApprovalPolicy: "auto"},
		widgets:          viewState{sendWithEnter: true, steerMode: true},
		taskActions:      map[string]api.Command{},
		controls:         map[string]*controlAction{},
		terminalCalls:    map[string]bool{},
		post:             inline,
		start:            inline,
		ctx:              context.Background(),
		inFlightCommands: map[string]bool{},
		stoppingRuns:     map[string]bool{},
		composers:        map[string]*composer{},
		skillCatalogs:    map[string][]skillEntry{},
		skillErrs:        map[string]string{},
		connNotice:       notice{Text: "正在连接本地服务…", Level: noticeInfo},
	}
}

// ---------------------------------------------------------------- data state

// applySnapshot replaces committed data only. It never clears drafts or an
// unconfirmed command. Stable Run IDs let the native list retain its reading
// anchor, and reconciliation removes a provisional turn only when Core's
// entity for that same instruction is visible.
func (w *workbench) applySnapshot(snap api.Snapshot) {
	clear(w.outputs)
	clear(w.toolViews)
	clear(w.diffStates)
	w.frame = frameIndex{}
	w.navigation.valid = false
	w.sequence = snap.Sequence
	w.threads = append([]api.Thread(nil), snap.Threads...)
	w.runs = append([]api.Run(nil), snap.Runs...)
	w.tasks = append([]api.Task(nil), snap.Tasks...)
	w.cohorts = append([]api.Cohort(nil), snap.Cohorts...)
	w.inputs = append([]api.Input(nil), snap.Inputs...)
	w.tools = append([]api.ToolCall(nil), snap.Tools...)
	w.requests = append([]api.ProviderRequest(nil), snap.Requests...)
	w.messages = append([]api.QueuedMessage(nil), snap.Messages...)
	w.steering = append([]api.SteeringMessage(nil), snap.Steering...)
	w.sortSteering()
	w.reconcileSteering()
	sort.SliceStable(w.inputs, func(i, j int) bool { return w.inputs[i].CreatedAt.Before(w.inputs[j].CreatedAt) })
	sort.SliceStable(w.tools, func(i, j int) bool { return w.tools[i].UpdatedAt.Before(w.tools[j].UpdatedAt) })
	sort.SliceStable(w.requests, func(i, j int) bool { return w.requests[i].CreatedAt.Before(w.requests[j].CreatedAt) })
	for _, input := range w.inputs {
		w.afterInput(input)
	}
	for _, request := range w.requests {
		w.afterRequest(request)
	}
	w.sortRuns()
	w.sortThreads()
	w.retireReceipts()
	w.selectThread("")
	if w.selected == "" {
		w.newSession()
	}
	// The first genuinely empty workspace offers a real, prompt-free catalog.
	// This local draft is only a composer identity: no Core thread is created
	// until the user sends, and catalog lookup never inspects its workspace.
	if len(snap.Threads) == 0 && w.defaults.Workspace == nil && w.defaults.ProviderID == "pi" && !w.startupModelPrompted {
		w.startupModelPrompted = true
		w.widgets.modelPickerOpen = true
		w.ensureModels(false)
	}
	w.reconcileRuns()
	w.streamNotice = notice{}
	w.resyncReason = ""
	w.connNotice = w.connectedNotice()
}

func (w *workbench) applyEvent(ev api.Event) bool {
	handled := w.foldEvent(ev)
	if !handled {
		w.unknownEvents++
	}
	return handled
}

func (w *workbench) foldEvent(ev api.Event) bool {
	if ev.Sequence <= w.sequence {
		w.staleEvents++
		return true
	}
	handled := false
	switch ev.Type {
	case "project.updated":
		handled = true
	case eventRunOutput:
		handled = w.foldOutput(ev)
	case eventRunReasoning:
		handled = w.foldReasoning(ev)
	case eventRunTelemetry:
		handled = w.foldTelemetry(ev)
	case "message.updated":
		handled = w.foldMessage(ev)
	case "message.steering":
		handled = w.foldSteeringEvent(ev)
	case eventThreadCreated, "thread.updated":
		handled = w.foldThread(ev)
	case eventRunQueued, eventRunStarted, eventRunCompleted, eventRunCancelled, eventRunInterrupted, eventRunFailed:
		handled = w.foldRun(ev)
	case eventInputUpdated:
		handled = w.foldInput(ev)
	case eventToolUpdated:
		handled = w.foldTool(ev)
	case eventRequestUpdated:
		handled = w.foldRequest(ev)
	case eventThreadDeleted:
		handled = w.foldThreadDeleted(ev)
	case eventThreadRewound:
		handled = w.foldThreadRewound(ev)
	default:
		handled = w.foldOrchestration(ev) || w.foldRun(ev) || w.foldThread(ev) || w.foldOutput(ev)
	}
	if handled {
		w.commit(ev)
	}
	return handled
}

func (w *workbench) foldRun(ev api.Event) bool {
	run, ok := decodeRun(ev.Data)
	if !ok {
		return false
	}
	if run.ID == "" {
		run.ID = ev.RunID
	}
	if run.ThreadID == "" {
		run.ThreadID = ev.ThreadID
	}
	if run.ID == "" || run.Status == "" || run.ThreadID == "" || !w.hasThread(run.ThreadID) {
		return false
	}
	w.mergeRun(run)
	return true
}

func (w *workbench) foldThread(ev api.Event) bool {
	thread, ok := decodeThread(ev.Data)
	if !ok || thread.ID == "" {
		return false
	}
	w.mergeThread(thread)
	return true
}

// foldOutput appends delta exactly once. A full output without a delta is
// reconciled as a snapshot of the Run rather than appended a second time.
func (w *workbench) foldOutput(ev api.Event) bool {
	runID := ev.RunID
	delta, full, found := decodeOutput(ev.Data)
	if !found {
		return false
	}
	if runID == "" {
		if run, ok := decodeRun(ev.Data); ok {
			runID = run.ID
		}
	}
	if runID == "" || !w.hasRun(runID) {
		return false
	}
	if delta != "" {
		w.appendDelta(runID, ev.ThreadID, delta)
		return true
	}
	if full != "" {
		w.setRunOutput(runID, full)
		return true
	}
	return true
}

func (w *workbench) commit(ev api.Event) {
	if ev.Sequence > w.sequence {
		w.sequence = ev.Sequence
	}
	w.appliedEvents++
}

func (w *workbench) hasRun(id string) bool {
	for i := range w.runs {
		if w.runs[i].ID == id {
			return true
		}
	}
	return false
}

func (w *workbench) appendDelta(runID, threadID, delta string) bool {
	for i := range w.runs {
		if w.runs[i].ID != runID {
			continue
		}
		w.runs[i].Output = w.appendOutput(runID, w.runs[i].Output, delta)
		if w.runs[i].ThreadID == "" && threadID != "" {
			w.runs[i].ThreadID = threadID
		}
		return true
	}
	return false
}

func (w *workbench) setRunOutput(runID, output string) bool {
	for i := range w.runs {
		if w.runs[i].ID != runID {
			continue
		}
		if !strings.HasPrefix(w.runs[i].Output, output) {
			w.runs[i].Output = output
		}
		return true
	}
	return false
}

func (w *workbench) mergeRun(run api.Run) {
	for i := range w.runs {
		if w.runs[i].ID != run.ID {
			continue
		}
		if !w.runs[i].Status.Terminal() && run.Status.Terminal() {
			w.markUnread(nonEmptyString(run.ThreadID, w.runs[i].ThreadID))
		}
		w.runs[i] = joinRun(w.runs[i], run)
		w.afterRunMerged(run)
		return
	}
	if run.Status.Terminal() {
		w.markUnread(run.ThreadID)
	}
	w.runs = append(w.runs, run)
	w.sortRuns()
	w.afterRunMerged(run)
}

func (w *workbench) afterRunMerged(run api.Run) {
	if run.Status.Terminal() {
		w.releaseOutput(run.ID)
		delete(w.stoppingRuns, run.ID)
		if c := w.composers[run.ThreadID]; c != nil && c.pending == nil && c.feedback.Level == noticeInfo {
			c.feedback = notice{}
		}
	}
	w.reconcileRuns()
}

// joinRun keeps streamed text when an entity frame lags output deltas.
func joinRun(old, next api.Run) api.Run {
	merged := next
	if merged.ThreadID == "" {
		merged.ThreadID = old.ThreadID
	}
	if merged.Input == "" {
		merged.Input = old.Input
	}
	if merged.AttemptID == "" {
		merged.AttemptID = old.AttemptID
	}
	if merged.CreatedAt.IsZero() {
		merged.CreatedAt = old.CreatedAt
	}
	if merged.UpdatedAt.IsZero() {
		merged.UpdatedAt = old.UpdatedAt
	}
	if strings.HasPrefix(old.Reasoning, merged.Reasoning) {
		merged.Reasoning = old.Reasoning // a run event may predate deltas already folded
	}
	switch {
	case strings.HasPrefix(old.Output, merged.Output):
		merged.Output = old.Output
	case strings.HasPrefix(merged.Output, old.Output):
	default:
		if next.UpdatedAt.Before(old.UpdatedAt) {
			merged.Output = old.Output
		}
	}
	if merged.Telemetry == nil && old.Telemetry != nil {
		copyTel := *old.Telemetry
		merged.Telemetry = &copyTel
	}
	if merged.Telemetry != nil && merged.Status.Terminal() {
		copyTel := *merged.Telemetry
		copyTel.Compacting = false
		copyTel.Retrying = false
		copyTel.Phase = string(merged.Status)
		merged.Telemetry = &copyTel
	}
	return merged
}

func (w *workbench) mergeThread(thread api.Thread) {
	for i := range w.threads {
		if w.threads[i].ID != thread.ID {
			continue
		}
		if thread.Title != "" {
			w.threads[i].Title = thread.Title
		}
		if !thread.CreatedAt.IsZero() {
			w.threads[i].CreatedAt = thread.CreatedAt
		}
		w.threads[i].ProviderID = thread.ProviderID
		w.threads[i].Model = thread.Model
		w.threads[i].Workspace = thread.Workspace
		w.threads[i].ProjectPath = thread.ProjectPath
		w.threads[i].ParentThreadID = thread.ParentThreadID
		w.threads[i].ParentTaskID = thread.ParentTaskID
		w.threads[i].ApprovalPolicy = thread.ApprovalPolicy
		w.threads[i].Pinned, w.threads[i].Archived = thread.Pinned, thread.Archived
		w.retireReceipts()
		return
	}
	w.threads = append(w.threads, thread)
	w.sortThreads()
	w.selectThread("")
	w.retireReceipts()
}

func (w *workbench) sortThreads() {
	sort.SliceStable(w.threads, func(i, j int) bool {
		return w.threads[i].CreatedAt.Before(w.threads[j].CreatedAt)
	})
}

func (w *workbench) sortRuns() {
	sort.SliceStable(w.runs, func(i, j int) bool {
		return w.runs[i].CreatedAt.Before(w.runs[j].CreatedAt)
	})
}

func (w *workbench) sortSteering() {
	sort.SliceStable(w.steering, func(i, j int) bool {
		if w.steering[i].CreatedAt.Equal(w.steering[j].CreatedAt) {
			return w.steering[i].ID < w.steering[j].ID
		}
		return w.steering[i].CreatedAt.Before(w.steering[j].CreatedAt)
	})
}

func (w *workbench) mergeSteering(message api.SteeringMessage) {
	for i, current := range w.steering {
		if current.ID == message.ID && message.ID != "" || current.CommandID == message.CommandID && message.CommandID != "" {
			w.steering[i] = message
			w.sortSteering()
			w.reconcileSteering()
			return
		}
	}
	w.steering = append(w.steering, message)
	w.sortSteering()
	w.reconcileSteering()
}

func (w *workbench) reconcileSteering() {
	for _, comp := range w.composers {
		pending := comp.steering
		if pending == nil || !pending.accepted {
			continue
		}
		for _, message := range w.steering {
			if message.CommandID == pending.command.CommandID || pending.messageID != "" && message.ID == pending.messageID {
				comp.steering = nil
				break
			}
		}
	}
}

func (w *workbench) retireReceipts() {
	if w.awaitingThread != "" && w.hasThread(w.awaitingThread) {
		w.awaitingThread = ""
	}
}

func (w *workbench) hasThread(id string) bool {
	if w.frame.valid {
		if _, ok := w.frame.threads[id]; ok {
			return true
		}
	}
	for _, t := range w.threads {
		if t.ID == id {
			return true
		}
	}
	return false
}

func (w *workbench) threadByID(id string) (api.Thread, bool) {
	if w.frame.valid {
		if i, ok := w.frame.threads[id]; ok && i < len(w.threads) && w.threads[i].ID == id {
			return w.threads[i], true
		} // otherwise a thread merged during the frame: scan
	}
	for _, t := range w.threads {
		if t.ID == id {
			return t, true
		}
	}
	return api.Thread{}, false
}

func (w *workbench) runsFor(threadID string) []api.Run {
	out := make([]api.Run, 0, len(w.runs))
	for _, r := range w.runs {
		if r.ThreadID == threadID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (w *workbench) activeRun(threadID string) (api.Run, bool) {
	var latest api.Run
	found := false
	for _, run := range w.runs {
		if run.ThreadID == threadID && !run.Status.Terminal() && (!found || !run.CreatedAt.Before(latest.CreatedAt)) {
			latest, found = run, true
		}
	}
	return latest, found
}

func (w *workbench) selectedThread() (api.Thread, bool) {
	return w.threadByID(w.selected)
}

func isLocalSession(id string) bool { return strings.HasPrefix(id, localSessionPrefix) }

func (w *workbench) hasSession(id string) bool {
	if id == "" {
		return false
	}
	c := w.composers[id]
	return w.hasThread(id) || w.awaitingThread == id || c != nil && (isLocalSession(id) || c.pending != nil)
}

// selectThread keeps a local draft or a partially created session selected
// across snapshots. An empty selection falls back to the first committed
// thread; it never creates a Core thread by itself.
func (w *workbench) selectThread(id string) {
	previous := w.selected
	if id == "" {
		if w.hasSession(w.selected) {
			return
		}
		w.selected = ""
		if len(w.threads) > 0 {
			w.selected = w.threads[0].ID
		}
		return
	}
	if previous != id {
		w.gitView.commitOpen, w.gitView.expanded = false, false
		if comp := w.composers[previous]; comp != nil {
			comp.composing, comp.compositionEnding = false, false
			comp.lastCompositionSet = time.Time{}
		}
		w.features.showRename = false
		if w.sessions.deleteConfirm != id {
			w.sessions.deleteConfirm = ""
		}
		w.picker.reset()
		w.noteVisit(previous, id)
	}
	w.selected = id
	w.clearUnread(id)
	w.retireReceipts()
	if previous != w.selected {
		w.saveState()
		if w.features.panel == "git" && w.hasThread(id) && w.features.git[id] == nil {
			w.loadGit()
		}
		if w.features.panel == "files" {
			w.ensureFileListing()
		}
		if w.widgets.workspaceOpen {
			w.workspaceChanges = nil
			w.workspaceErr = ""
			w.refreshWorkspace()
		}
	}
}

// --------------------------------------------------------------- projection

// titleFor treats Core's persisted title as authoritative, including a user
// rename back to "新会话". Only uncommitted/untitled sessions derive a title.
func (w *workbench) titleFor(threadID string) string {
	if thread, ok := w.threadByID(threadID); ok && strings.TrimSpace(thread.Title) != "" {
		return thread.Title
	}
	if runs := w.runsFor(threadID); len(runs) > 0 && strings.TrimSpace(runs[0].Input) != "" {
		return titleFromInstruction(runs[0].Input)
	}
	if c := w.composers[threadID]; c != nil && c.pending != nil {
		return titleFromInstruction(c.pending.send.Message)
	}
	return newSessionTitle
}

// displayTitleFor cleans raw Markdown punctuation (such as "## " or "**")
// from prompt-derived titles while preserving plain titles and custom names.
func (w *workbench) displayTitleFor(threadID string) string {
	return stripInlineMarkdownTitle(w.titleFor(threadID))
}

func titleFromInstruction(text string) string {
	text = strings.TrimPrefix(text, planModePrefix)
	text = stripInlineMarkdownTitle(text)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return newSessionTitle
	}
	const limit = 36
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}

func stripInlineMarkdownTitle(text string) string {
	if !strings.ContainsAny(text, "#*`") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for strings.HasPrefix(trimmed, "#") {
			trimmed = strings.TrimPrefix(trimmed, "#")
		}
		lines[i] = strings.TrimSpace(trimmed)
	}
	cleaned := strings.Join(lines, " ")
	cleaned = strings.ReplaceAll(cleaned, "**", "")
	cleaned = strings.ReplaceAll(cleaned, "__", "")
	cleaned = strings.ReplaceAll(cleaned, "`", "")
	for _, marker := range []string{" ### ", " ## ", " # "} {
		cleaned = strings.ReplaceAll(cleaned, marker, " ")
	}
	cleaned = strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
	if cleaned == "" {
		return text
	}
	return cleaned
}

func (w *workbench) sessionMatches(threadID, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	if strings.Contains(strings.ToLower(w.titleFor(threadID)), query) {
		return true
	}
	for _, r := range w.runsFor(threadID) {
		if strings.Contains(strings.ToLower(r.Input), query) || strings.Contains(strings.ToLower(r.Output), query) {
			return true
		}
	}
	if c := w.composers[threadID]; c != nil {
		return strings.Contains(strings.ToLower(c.draft), query)
	}
	return false
}

// --------------------------------------------------------------- outbox

func (w *workbench) composerFor(threadID string) *composer {
	if threadID == "" {
		return &composer{}
	}
	c := w.composers[threadID]
	if c == nil {
		c = &composer{}
		w.composers[threadID] = c
	}
	return c
}

// setDraft changes only the new draft. In particular it never invalidates or
// rewrites the command in pending, whose ID belongs to its frozen payload.
func (w *workbench) setDraft(threadID, text string) {
	if threadID == "" {
		return
	}
	c := w.composerFor(threadID)
	if c.draft == text {
		return
	}
	if c.lastPaste != nil && text != c.lastPaste.after {
		c.lastPaste = nil
	}
	c.draft = text
	c.pruneMentions()
	if c.pending == nil {
		c.feedback = notice{}
	}
	w.saveState()
}

// newSession selects a local, empty draft. No thread.create is dispatched
// until the user actually sends an instruction.
func (w *workbench) newSession() string {
	if isLocalSession(w.selected) {
		if c := w.composers[w.selected]; c != nil && c.pending == nil && !c.uploading && len(c.attachments) == 0 && strings.TrimSpace(c.draft) == "" {
			w.widgets.composerFocus.Focus()
			return w.selected
		}
	}
	if w.defaultModel != "" {
		w.defaults.Model = w.defaultModel
	}
	w.localSerial++
	id := fmt.Sprintf("%s%d", localSessionPrefix, w.localSerial)
	w.composerFor(id)
	w.localOrder = append(w.localOrder, id)
	w.selectThread(id)
	w.logState(id).ScrollToEnd()
	w.widgets.composerFocus.Focus()
	return id
}

// send freezes a new command (and, for a local draft, a create command), then
// clears only the input value that was just submitted. Later edits are a new
// draft. A receipt never clears that later draft.
func (w *workbench) send(threadID string) bool {
	if threadID == "" {
		w.setAction("请先新建或选择一个会话。")
		return false
	}
	c := w.composerFor(threadID)
	text := strings.TrimSpace(c.outgoing())
	if c.uploading {
		return false
	}
	if text == "" && len(c.attachments) > 0 {
		text = "请查看附件。"
	}
	if text == "" {
		c.feedback = notice{Text: "输入为空，未发送。", Level: noticeWarn}
		return false
	}
	if c.pending != nil {
		return false
	}
	if w.tryBuiltinSlashCommand(threadID, c, text) {
		return true
	}
	if !w.modelReadyToSend(threadID) {
		return false
	}
	if _, active := w.activeRun(threadID); active {
		c.feedback = notice{Text: "当前回复结束后即可发送；草稿已保留。", Level: noticeInfo}
		return false
	}
	text = applyInteractionMode(c.plan, text)

	p := &submission{
		send: api.Command{
			Version:     api.ProtocolVersion,
			Kind:        api.SendMessage,
			ThreadID:    threadID,
			Message:     text,
			Attachments: append([]api.Attachment(nil), c.attachments...),
			CommandID:   newCommandID("send"),
		},
		threadID:  threadID,
		startedAt: time.Now(),
	}
	c.pending = p
	c.draft = ""
	c.attachments = nil
	c.mentions = nil
	c.feedback = notice{Text: "正在发送…", Level: noticeInfo}
	w.saveState()
	w.setAction("")
	w.logState(threadID).ScrollToEnd()

	if isLocalSession(threadID) {
		p.create = api.Command{
			Version:        api.ProtocolVersion,
			Kind:           api.CreateThread,
			Title:          titleFromInstruction(text),
			CommandID:      newCommandID("thread"),
			ProviderID:     w.threadProvider(threadID),
			Model:          w.draftModel(threadID),
			Workspace:      cloneWorkspace(w.defaults.Workspace),
			ApprovalPolicy: w.defaults.ApprovalPolicy,
		}
		p.threadID = ""
		p.send.ThreadID = ""
		w.dispatchCreate(threadID, p)
	} else {
		w.dispatchSend(threadID, p)
	}
	w.widgets.composerFocus.Focus()
	return true
}

func (w *workbench) dispatchCreate(sessionID string, p *submission) {
	if p == nil || p.inFlight || p.createAccepted || p.sendStarted {
		return
	}
	cmd := p.create // copy: this exact payload is the retry unit
	if w.inFlightCommands[cmd.CommandID] {
		return
	}
	p.inFlight = true
	w.inFlightCommands[cmd.CommandID] = true
	w.dispatch(cmd, func(receipt api.Receipt, err error) {
		w.post(func() { w.afterCreate(sessionID, p, cmd, err, receipt) })
	})
}

func (w *workbench) afterCreate(sessionID string, p *submission, cmd api.Command, err error, receipt api.Receipt) {
	delete(w.inFlightCommands, cmd.CommandID)
	p.inFlight = false
	c := w.composerFor(sessionID)
	if c.pending != p {
		return
	}
	if err != nil {
		c.feedback = notice{Text: describe("创建会话未确认", err), Level: levelFor(err)}
		w.connNotice = notice{Text: describe("本地服务连接", err), Level: levelFor(err)}
		return
	}
	if !receipt.Accepted || receipt.ThreadID == "" {
		c.feedback = notice{Text: "本地服务未确认新会话，请重试原请求。", Level: noticeError}
		return
	}

	p.createAccepted = true
	p.threadID = receipt.ThreadID
	p.send.ThreadID = receipt.ThreadID
	w.migrateSession(sessionID, receipt.ThreadID)
	w.connNotice = w.connectedNotice()
	// Only message.send remains. A retry from here never creates another thread.
	w.dispatchSend(receipt.ThreadID, p)
}

func (w *workbench) dispatchSend(sessionID string, p *submission) {
	if p == nil || p.inFlight || p.accepted || !p.createAccepted && isLocalSession(sessionID) {
		return
	}
	cmd := p.send // copy: retries never consult the current draft
	if cmd.ThreadID == "" || w.inFlightCommands[cmd.CommandID] {
		return
	}
	p.inFlight = true
	p.sendStarted = true
	w.inFlightCommands[cmd.CommandID] = true
	w.dispatch(cmd, func(receipt api.Receipt, err error) {
		w.post(func() { w.afterSend(sessionID, p, cmd, err, receipt) })
	})
}

func (w *workbench) afterSend(sessionID string, p *submission, cmd api.Command, err error, receipt api.Receipt) {
	delete(w.inFlightCommands, cmd.CommandID)
	p.inFlight = false
	c := w.composerFor(sessionID)
	if c.pending != p {
		return
	}
	if err != nil {
		c.feedback = notice{Text: describe("发送结果待确认", err), Level: levelFor(err)}
		w.connNotice = notice{Text: describe("本地服务连接", err), Level: levelFor(err)}
		return
	}
	if !receipt.Accepted {
		c.feedback = notice{Text: "本地服务未接受原请求，可重试；新草稿不受影响。", Level: noticeError}
		return
	}
	p.accepted = true
	p.runID = receipt.RunID
	if receipt.ThreadID != "" {
		p.threadID = receipt.ThreadID
	}
	c.feedback = notice{}
	if cmd.Kind == api.DelegateTask || cmd.Kind == api.FollowUpTask {
		c.feedback = notice{Text: "子任务已派出，结果将交回父任务。", Level: noticeInfo}
	}
	w.connNotice = w.connectedNotice()
	w.reconcileRuns()
}

// retryPending retries the original stage and payload, never the edited draft.
// Recovery is manual: reconnecting a stream does not replay user instructions.
func (w *workbench) retryPending(threadID string) bool {
	c := w.composerFor(threadID)
	p := c.pending
	if p == nil || p.inFlight || p.accepted {
		return false
	}
	c.feedback = notice{Text: "正在重试原请求…", Level: noticeInfo}
	if p.createAccepted || p.sendStarted {
		w.dispatchSend(threadID, p)
	} else {
		w.dispatchCreate(threadID, p)
	}
	return true
}

// migrateSession preserves the independent draft and outbox when create
// succeeds. The real thread may not have an entity yet; awaitingThread keeps
// that partial success selectable and visible without inventing another create.
func (w *workbench) migrateSession(localID, threadID string) {
	if !isLocalSession(localID) || threadID == "" {
		return
	}
	c := w.composers[localID]
	delete(w.composers, localID)
	w.composers[threadID] = c
	for i, id := range w.localOrder {
		if id == localID {
			w.localOrder = append(w.localOrder[:i], w.localOrder[i+1:]...)
			break
		}
	}
	w.widgets.moveLog(localID, threadID)
	w.renameHistory(localID, threadID)
	if output, ok := w.features.pluginOutputs[localID]; ok {
		w.features.pluginOutputs[threadID] = output
		delete(w.features.pluginOutputs, localID)
	}
	if feedback, ok := w.features.feedback["plugins:"+localID]; ok {
		w.setToolFeedback("plugins:"+threadID, feedback)
		delete(w.features.feedback, "plugins:"+localID)
	}
	if w.selected == localID {
		w.selected = threadID
	}
	w.awaitingThread = threadID
	w.retireReceipts()
	w.saveState()
}

// reconcileRuns merges a provisional instruction only after its command
// receipt names the committed Run. A snapshot may include an identical message
// from another client, so text and client timestamps cannot confirm a command.
func (w *workbench) reconcileRuns() {
	for _, r := range w.runs {
		if r.Status.Terminal() {
			delete(w.stoppingRuns, r.ID)
		}
	}
	for _, c := range w.composers {
		p := c.pending
		if p == nil {
			continue
		}
		for _, r := range w.runs {
			if !submissionMatchesRun(p, r) {
				continue
			}
			c.pending = nil
			c.feedback = notice{}
			break
		}
	}
}

func submissionMatchesRun(p *submission, r api.Run) bool {
	return p != nil && p.accepted && p.runID != "" &&
		p.runID == r.ID && p.threadID == r.ThreadID
}

// ------------------------------------------------------------------ stop

func (w *workbench) stopActiveRun() bool { return w.stopThread(w.selected) }

func (w *workbench) stopThread(threadID string) bool {
	run, ok := w.activeRun(threadID)
	if !ok {
		if threadID == w.selected {
			w.setAction("当前会话没有正在运行的任务。")
		} else {
			w.setAction("这个子代理没有正在运行的任务。")
		}
		return false
	}
	if w.stoppingRuns[run.ID] {
		return false
	}
	cmd := api.Command{
		Version:   api.ProtocolVersion,
		Kind:      api.StopRun,
		ThreadID:  run.ThreadID,
		RunID:     run.ID,
		CommandID: newCommandID("stop"),
	}
	w.stoppingRuns[run.ID] = true
	w.setAction("")
	w.inFlightCommands[cmd.CommandID] = true
	w.dispatch(cmd, func(receipt api.Receipt, err error) {
		w.post(func() { w.afterStop(cmd, err, receipt) })
	})
	return true
}

func (w *workbench) afterStop(cmd api.Command, err error, receipt api.Receipt) {
	delete(w.inFlightCommands, cmd.CommandID)
	if err != nil {
		delete(w.stoppingRuns, cmd.RunID)
		w.composerFor(cmd.ThreadID).feedback = notice{Text: describe("停止请求失败", err), Level: noticeError}
		w.setAction(describe("停止请求失败", err))
		w.connNotice = notice{Text: describe("本地服务连接", err), Level: levelFor(err)}
		return
	}
	if !receipt.Accepted {
		delete(w.stoppingRuns, cmd.RunID)
		w.composerFor(cmd.ThreadID).feedback = notice{Text: "本地服务未接受停止请求，请稍后重试。", Level: noticeError}
		w.setAction("本地服务未接受停止请求，请稍后重试。")
		return
	}
	// Keep the stopping indicator until Core commits a terminal Run. The
	// receipt confirms only the request; natural completion may win the race.
	w.connNotice = w.connectedNotice()
	for _, r := range w.runs {
		if r.ID == cmd.RunID && r.Status.Terminal() {
			delete(w.stoppingRuns, cmd.RunID)
		}
	}
}

func (w *workbench) setAction(text string) {
	switch {
	case text == "":
		w.actionNotice = notice{}
	case strings.Contains(text, "失败"), strings.Contains(text, "未接受"), strings.Contains(text, "没有正在运行"):
		w.actionNotice = notice{Text: text, Level: noticeError}
	default:
		w.actionNotice = notice{Text: text, Level: noticeInfo}
	}
}

func (w *workbench) connectedNotice() notice { return notice{} }

func (w *workbench) dispatch(cmd api.Command, done func(api.Receipt, error)) {
	service, ctx := w.service, w.ctx
	w.start(func() {
		receipt, err := service.Dispatch(ctx, cmd)
		done(receipt, err)
	})
}

// ------------------------------------------------------------------- payload

const (
	eventThreadCreated  = "thread.created"
	eventThreadRewound  = "thread.rewound"
	eventRunQueued      = "run.queued"
	eventRunStarted     = "run.started"
	eventRunOutput      = "run.output"
	eventRunReasoning   = "run.reasoning"
	eventRunTelemetry   = "run.telemetry"
	eventRunCompleted   = "run.completed"
	eventRunCancelled   = "run.cancelled"
	eventRunInterrupted = "run.interrupted"
	eventRunFailed      = "run.failed"
	eventInputUpdated   = "input.updated"
	eventToolUpdated    = "tool.updated"
	eventRequestUpdated = "request.updated"
)

func decodeOutput(data json.RawMessage) (delta, full string, found bool) {
	if len(data) == 0 {
		return "", "", false
	}
	var bare string
	if err := json.Unmarshal(data, &bare); err == nil {
		return bare, "", true
	}
	var payload struct {
		Delta  *string `json:"delta"`
		Text   *string `json:"text"`
		Chunk  *string `json:"chunk"`
		Output *string `json:"output"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", "", false
	}
	for _, p := range []*string{payload.Delta, payload.Text, payload.Chunk} {
		if p != nil {
			delta = *p
			found = true
			break
		}
	}
	if payload.Output != nil {
		full = *payload.Output
		found = true
	}
	return delta, full, found
}

func decodeRun(data json.RawMessage) (api.Run, bool) {
	var run api.Run
	if err := json.Unmarshal(data, &run); err == nil && run.ID != "" && run.Status != "" {
		return run, true
	}
	var wrapped struct {
		Run *api.Run `json:"run"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Run != nil {
		return *wrapped.Run, true
	}
	return api.Run{}, false
}

func (w *workbench) foldTelemetry(ev api.Event) bool {
	if ev.RunID == "" || !w.hasRun(ev.RunID) {
		return false
	}
	var wrapped struct {
		Telemetry *api.RunTelemetry `json:"telemetry"`
	}
	var tel *api.RunTelemetry
	if json.Unmarshal(ev.Data, &wrapped) == nil && wrapped.Telemetry != nil {
		tel = wrapped.Telemetry
	} else {
		var bare api.RunTelemetry
		if json.Unmarshal(ev.Data, &bare) == nil && (bare.Phase != "" || bare.TotalTokens > 0 || bare.LastContextTokens > 0 || bare.Compacting || bare.Retrying) {
			tel = &bare
		}
	}
	if tel == nil {
		return false
	}
	for i := range w.runs {
		if w.runs[i].ID == ev.RunID {
			copyTel := *tel
			w.runs[i].Telemetry = &copyTel
			return true
		}
	}
	return false
}

func (w *workbench) modelContextWindow(modelKey string) int64 {
	if modelKey == "" {
		return 0
	}
	for _, item := range w.models {
		key := item.Key()
		if key == modelKey || strings.HasPrefix(modelKey, key+":") || item.ID == modelKey {
			if item.ContextWindow > 0 {
				return item.ContextWindow
			}
		}
	}
	return 0
}

func (w *workbench) threadTelemetry(threadID string) (run api.Run, active bool, tel *api.RunTelemetry, ctxWindow int64) {
	if threadID == "" {
		return api.Run{}, false, nil, 0
	}
	if activeRun, ok := w.activeRun(threadID); ok {
		run = activeRun
		active = true
		tel = activeRun.Telemetry
	} else {
		runs := w.runsFor(threadID)
		for i := len(runs) - 1; i >= 0; i-- {
			if runs[i].Telemetry != nil {
				run = runs[i]
				tel = runs[i].Telemetry
				break
			}
		}
	}
	model := run.Model
	if model == "" {
		if thread, ok := w.threadByID(threadID); ok {
			model = thread.Model
		}
	}
	if model == "" {
		model = w.configuredDefaultModel()
	}
	return run, active, tel, w.modelContextWindow(model)
}

func decodeThread(data json.RawMessage) (api.Thread, bool) {
	var thread api.Thread
	if err := json.Unmarshal(data, &thread); err == nil && thread.ID != "" {
		return thread, true
	}
	var wrapped struct {
		Thread *api.Thread `json:"thread"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Thread != nil {
		return *wrapped.Thread, true
	}
	return api.Thread{}, false
}

// ------------------------------------------------------------------- helpers

func newCommandID(kind string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s.time.%d", kind, time.Now().UnixNano())
	}
	return kind + "." + hex.EncodeToString(b[:])
}

// describe is user-facing feedback, not a socket/protocol diagnostic dump.
// Detailed transport errors remain available to the caller's logs, while the
// persistent UI stays focused on the recoverable action.
func describe(action string, err error) string {
	if err == nil {
		return action
	}
	return action + "：" + explain(err)
}

func explain(err error) string {
	switch api.CodeOf(err) {
	case api.InvalidCommand:
		return "当前界面无法完成此操作"
	case api.VersionMismatch:
		return "本地服务版本与界面不兼容"
	case api.NotReady:
		return "本地服务尚未就绪，请稍后重试"
	case api.NotFound:
		return "找不到对应会话或任务"
	case api.Conflict:
		return "当前会话已有任务进行，或操作与现有状态冲突"
	case api.ResyncRequired:
		return "同步落后，正在重新读取会话"
	default:
		return "本地服务暂时不可用"
	}
}

func levelFor(err error) noticeLevel {
	switch api.CodeOf(err) {
	case api.NotReady, api.ResyncRequired:
		return noticeWarn
	default:
		return noticeError
	}
}

// foldReasoning appends a reasoning delta beside the reply, never into it.
func (w *workbench) foldReasoning(ev api.Event) bool {
	var payload struct {
		Delta *string `json:"reasoningDelta"`
	}
	if json.Unmarshal(ev.Data, &payload) != nil || payload.Delta == nil {
		return false
	}
	delta := *payload.Delta
	for i := range w.runs {
		if w.runs[i].ID == ev.RunID {
			w.runs[i].Reasoning = w.appendOutput("reasoning:"+ev.RunID, w.runs[i].Reasoning, delta)
			return true
		}
	}
	return false
}

func (w *workbench) foldThreadRewound(ev api.Event) bool {
	var payload struct {
		ThreadID      string   `json:"threadId"`
		RunID         string   `json:"runId"`
		RemovedRunIDs []string `json:"removedRunIds"`
	}
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		return false
	}
	removed := make(map[string]bool, len(payload.RemovedRunIDs))
	for _, id := range payload.RemovedRunIDs {
		if id != "" {
			removed[id] = true
		}
	}
	if len(removed) == 0 && payload.RunID != "" {
		removed[payload.RunID] = true
	}
	if len(removed) == 0 {
		return false
	}
	keepRuns := w.runs[:0]
	for _, r := range w.runs {
		if removed[r.ID] {
			w.releaseOutput(r.ID)
			w.releaseOutput("reasoning:" + r.ID)
			delete(w.stoppingRuns, r.ID)
			continue
		}
		keepRuns = append(keepRuns, r)
	}
	w.runs = keepRuns

	keepTools := w.tools[:0]
	for _, tool := range w.tools {
		if removed[tool.RunID] {
			delete(w.toolViews, tool.ID)
			continue
		}
		keepTools = append(keepTools, tool)
	}
	w.tools = keepTools

	keepReqs := w.requests[:0]
	for _, req := range w.requests {
		if !removed[req.RunID] {
			keepReqs = append(keepReqs, req)
		}
	}
	w.requests = keepReqs

	keepTasks := w.tasks[:0]
	for _, task := range w.tasks {
		if !removed[task.ParentRunID] && !removed[task.ChildRunID] {
			keepTasks = append(keepTasks, task)
		}
	}
	w.tasks = keepTasks

	keepCohorts := w.cohorts[:0]
	for _, cohort := range w.cohorts {
		if !removed[cohort.ParentRunID] {
			keepCohorts = append(keepCohorts, cohort)
		}
	}
	w.cohorts = keepCohorts

	keepInputs := w.inputs[:0]
	for _, input := range w.inputs {
		if !removed[input.RunID] {
			keepInputs = append(keepInputs, input)
		}
	}
	w.inputs = keepInputs

	clear(w.diffStates)
	w.frame = frameIndex{}
	w.navigation.valid = false
	return true
}
