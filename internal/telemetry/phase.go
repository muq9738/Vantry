package telemetry

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/provider"
)

// RunPhase enumerates the canonical desktop execution states for a Vantry run.
type RunPhase string

const (
	PhaseStarting        RunPhase = "starting"
	PhaseThinking        RunPhase = "thinking"
	PhaseStreaming       RunPhase = "streaming"
	PhaseExecutingTool   RunPhase = "executing_tool"
	PhaseWaitingApproval RunPhase = "waiting_approval"
	PhaseCompacting      RunPhase = "compacting"
	PhaseRetrying        RunPhase = "retrying"
	PhaseWaitingChildren RunPhase = "waiting_children"
	PhaseCompleted       RunPhase = "completed"
	PhaseFailed          RunPhase = "failed"
	PhaseCancelled       RunPhase = "cancelled"
)

// Terminal reports whether p is a final settled run state.
func (p RunPhase) Terminal() bool {
	return p == PhaseCompleted || p == PhaseFailed || p == PhaseCancelled
}

// Active reports whether p represents ongoing or parked non-terminal work.
func (p RunPhase) Active() bool {
	return !p.Terminal()
}

// ActiveToolInfo describes an in-flight tool execution on the call stack.
type ActiveToolInfo struct {
	CallID string `json:"callId,omitempty"`
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
}

// RetryInfo carries automatic retry progress and backoff details.
type RetryInfo struct {
	Attempt     int     `json:"attempt"`
	MaxAttempts int     `json:"maxAttempts,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	WaitSeconds float64 `json:"waitSeconds"`
	DelayMs     int64   `json:"delayMs,omitempty"`
}

// ApprovalInfo describes a blocking interactive dialog / tool approval request.
type ApprovalInfo struct {
	RequestID       string `json:"requestId"`
	Method          string `json:"method"`
	Title           string `json:"title,omitempty"`
	Message         string `json:"message,omitempty"`
	boundToolCallID string
}

// CompactionInfo describes an active context window compaction pass.
type CompactionInfo struct {
	Reason               string `json:"reason,omitempty"`
	TokensBefore         int64  `json:"tokensBefore,omitempty"`
	EstimatedTokensAfter int64  `json:"estimatedTokensAfter,omitempty"`
}

// PhaseSnapshot is the immutable projection emitted by PhaseTracker after each
// state transition, ready for desktop status bars and run cards.
type PhaseSnapshot struct {
	Phase     RunPhase  `json:"phase"`
	Sequence  int64     `json:"sequence"`
	UpdatedAt time.Time `json:"updatedAt"`

	// Tool execution context (populated during PhaseExecutingTool and preserved
	// in ActiveTools while nested under PhaseWaitingApproval).
	ToolName    string           `json:"toolName,omitempty"`
	ToolTarget  string           `json:"toolTarget,omitempty"`
	ActiveTools []ActiveToolInfo `json:"activeTools,omitempty"`

	// Approval context (populated during PhaseWaitingApproval).
	Approval         *ApprovalInfo `json:"approval,omitempty"`
	PendingApprovals int           `json:"pendingApprovals,omitempty"`

	// Compaction context (populated during PhaseCompacting).
	Compaction *CompactionInfo `json:"compaction,omitempty"`

	// Retry context (populated during PhaseRetrying).
	RetryAttempt     int        `json:"retryAttempt,omitempty"`
	RetryMaxAttempts int        `json:"retryMaxAttempts,omitempty"`
	RetryReason      string     `json:"retryReason,omitempty"`
	RetryWaitSeconds int        `json:"retryWaitSeconds,omitempty"`
	Retry            *RetryInfo `json:"retry,omitempty"`

	// Terminal or transient failure detail.
	Error string `json:"error,omitempty"`
}

// PhaseTracker reduces a stream of provider.Event records into accurate,
// nesting-aware PhaseSnapshot projections.
type PhaseTracker struct {
	mu               sync.Mutex
	lastSequence     int64
	updatedAt        time.Time
	basePhase        RunPhase
	preCompactPhase  RunPhase
	activeTools      []ActiveToolInfo
	pendingApprovals []ApprovalInfo
	compacting       *CompactionInfo
	retrying         *RetryInfo
	transientFailed  bool
	lastError        string
	terminalPhase    RunPhase
}

// NewPhaseTracker initializes a tracker in PhaseStarting.
func NewPhaseTracker() *PhaseTracker {
	return &PhaseTracker{
		basePhase: PhaseStarting,
		updatedAt: time.Now().UTC(),
	}
}

// Snapshot returns the current PhaseSnapshot safely across goroutines.
func (t *PhaseTracker) Snapshot() PhaseSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

// ApplyJSON parses a raw JSONL provider event line and applies it to the tracker.
func (t *PhaseTracker) ApplyJSON(line []byte) (PhaseSnapshot, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return t.Snapshot(), nil
	}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return t.Snapshot(), err
	}
	return t.Apply(provider.Event{
		Type: env.Type,
		Data: append(json.RawMessage(nil), trimmed...),
	}), nil
}

// Apply reduces one provider.Event into the next PhaseSnapshot.
// Duplicate positive sequences are ignored idempotently.
func (t *PhaseTracker) Apply(event provider.Event) PhaseSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	if event.Sequence > 0 {
		if event.Sequence <= t.lastSequence {
			return t.snapshotLocked()
		}
		t.lastSequence = event.Sequence
	}
	if !event.OccurredAt.IsZero() {
		t.updatedAt = event.OccurredAt.UTC()
	} else {
		t.updatedAt = time.Now().UTC()
	}

	if t.terminalPhase != "" {
		return t.snapshotLocked()
	}

	rawType := rawEventType(event)
	kind := strings.TrimSpace(event.Type)
	if kind == "" {
		kind = rawType
	}

	// Pi's durable worker rewrites `event.Type` to `"error"` when message_end,
	// turn_end, auto_retry_end, or compaction_end carries a failure, while
	// keeping the original record type inside `event.Data`. Inspect both so
	// nested states (compacting/retrying) unwind cleanly before surfacing failure.
	if kind == "error" && rawType != "" && rawType != "error" {
		t.applyErrorWrappedRecordLocked(rawType, event.Data)
		return t.snapshotLocked()
	}

	switch kind {
	case "session", "agent_start", "turn_start":
		t.transientFailed = false
		if t.basePhase == PhaseStarting {
			t.basePhase = PhaseThinking
		}

	case "message_start":
		if isAssistantMessageStart(event.Data) {
			t.transientFailed = false
			t.lastError = ""
			t.basePhase = PhaseThinking
		}

	case "thinking_start", "thinking_delta":
		t.transientFailed = false
		t.lastError = ""
		t.retrying = nil
		t.basePhase = PhaseThinking

	case "text_start", "text_delta":
		t.transientFailed = false
		t.lastError = ""
		t.retrying = nil
		t.basePhase = PhaseStreaming

	case "message_update":
		t.applyMessageUpdateLocked(event)

	case "tool_execution_start":
		t.transientFailed = false
		t.retrying = nil
		t.pushToolLocked(event.Data)
		// Once a tool runs, the assistant text block for this turn has finished;
		// after all tools complete, the model resumes in PhaseThinking.
		t.basePhase = PhaseThinking

	case "tool_execution_update":
		t.updateToolLocked(event.Data)

	case "tool_execution_end":
		t.popToolLocked(event.Data)
		if len(t.activeTools) == 0 {
			t.basePhase = PhaseThinking
		}

	case "extension_ui_request":
		t.pushApprovalLocked(event.Data)

	case "extension_ui_response":
		t.popApprovalByResponseLocked(event.Data)

	case "control_result":
		t.applyControlResultLocked(event.Data)

	case "compaction_start", "auto_compaction_start":
		t.transientFailed = false
		if t.compacting == nil {
			t.preCompactPhase = t.basePhase
			if t.preCompactPhase == PhaseStarting || t.preCompactPhase == PhaseStreaming {
				t.preCompactPhase = PhaseThinking
			}
		}
		t.compacting = parseCompactionStart(event.Data)

	case "compaction_end", "auto_compaction_end":
		endInfo, aborted, willRetry, errMsg := parseCompactionEnd(event.Data)
		t.compacting = nil
		if t.preCompactPhase != "" {
			t.basePhase = t.preCompactPhase
			t.preCompactPhase = ""
		} else if t.basePhase == PhaseStarting {
			t.basePhase = PhaseThinking
		}
		if errMsg != "" {
			t.lastError = errMsg
			t.transientFailed = true
		} else if aborted {
			t.terminalPhase = PhaseCancelled
			t.clearTransientStacksLocked()
		} else if willRetry || endInfo != nil {
			t.transientFailed = false
			t.lastError = ""
			t.basePhase = PhaseThinking
		}

	case "auto_retry_start", "summarization_retry_scheduled":
		t.transientFailed = false
		t.retrying = parseRetryStart(event.Data)

	case "summarization_retry_attempt_start", "summarization_retry_finished":
		t.retrying = nil

	case "auto_retry_end":
		success, finalErr, attempt := parseRetryEnd(event.Data)
		t.retrying = nil
		if success {
			t.transientFailed = false
			t.lastError = ""
			if t.basePhase == PhaseStarting {
				t.basePhase = PhaseThinking
			}
		} else {
			if finalErr == "" && attempt > 0 {
				finalErr = "automatic retry exhausted"
			}
			if finalErr != "" {
				t.lastError = finalErr
			}
			t.transientFailed = true
		}

	case "message_end", "turn_end":
		stopReason, errMsg, isAssistant := parseTurnEndStatus(event.Data)
		if isAssistant {
			switch stopReason {
			case "error":
				t.lastError = firstString(errMsg, t.lastError, "assistant turn failed")
				t.transientFailed = true
			case "aborted":
				t.lastError = ""
				t.terminalPhase = PhaseCancelled
				t.clearTransientStacksLocked()
			default:
				t.transientFailed = false
				t.basePhase = PhaseThinking
			}
		}

	case "agent_end":
		if parseWillRetry(event.Data) {
			// A failed turn with willRetry=true is about to enter auto_retry or
			// overflow compaction recovery rather than terminating as failed.
			t.transientFailed = false
			if t.retrying == nil && t.compacting == nil {
				t.retrying = &RetryInfo{
					Attempt: 1,
					Reason:  t.lastError,
				}
			}
		}

	case "waiting_children", "run.waiting_children":
		t.clearTransientStacksLocked()
		t.transientFailed = false
		t.terminalPhase = ""
		t.basePhase = PhaseWaitingChildren

	case "agent_settled":
		aborted := parseAborted(event.Data)
		t.clearTransientStacksLocked()
		if aborted {
			t.lastError = ""
			t.terminalPhase = PhaseCancelled
		} else if t.transientFailed || t.lastError != "" {
			if t.lastError == "" {
				t.lastError = "provider run failed"
			}
			t.terminalPhase = PhaseFailed
		} else {
			t.terminalPhase = PhaseCompleted
		}

	case "error":
		msg := extractErrorMessage(event.Data)
		if msg == "" {
			msg = strings.TrimSpace(event.Delta)
		}
		t.lastError = firstString(msg, t.lastError, "provider stream error")
		t.transientFailed = true

	default:
		if event.Delta != "" {
			t.transientFailed = false
			t.lastError = ""
			t.retrying = nil
			t.basePhase = PhaseStreaming
		}
	}

	return t.snapshotLocked()
}

// ResolveApproval explicitly clears a pending approval dialog by request ID and
// restores the underlying phase (such as PhaseExecutingTool or PhaseThinking).
func (t *PhaseTracker) ResolveApproval(requestID string) PhaseSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updatedAt = time.Now().UTC()
	t.removeApprovalByIDLocked(requestID)
	return t.snapshotLocked()
}

// MarkWaitingChildren transitions the run into PhaseWaitingChildren when the
// parent provider turn settled with unconsumed child task deliveries.
func (t *PhaseTracker) MarkWaitingChildren() PhaseSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updatedAt = time.Now().UTC()
	t.clearTransientStacksLocked()
	t.transientFailed = false
	t.lastError = ""
	t.terminalPhase = ""
	t.basePhase = PhaseWaitingChildren
	return t.snapshotLocked()
}

// MarkCompleted seals the tracker in PhaseCompleted.
func (t *PhaseTracker) MarkCompleted() PhaseSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updatedAt = time.Now().UTC()
	t.clearTransientStacksLocked()
	t.transientFailed = false
	t.lastError = ""
	t.terminalPhase = PhaseCompleted
	return t.snapshotLocked()
}

// MarkFailed seals the tracker in PhaseFailed with the given error reason.
func (t *PhaseTracker) MarkFailed(reason string) PhaseSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updatedAt = time.Now().UTC()
	t.clearTransientStacksLocked()
	t.lastError = firstString(reason, t.lastError, "run failed")
	t.terminalPhase = PhaseFailed
	return t.snapshotLocked()
}

// MarkCancelled seals the tracker in PhaseCancelled.
func (t *PhaseTracker) MarkCancelled() PhaseSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updatedAt = time.Now().UTC()
	t.clearTransientStacksLocked()
	t.transientFailed = false
	t.lastError = ""
	t.terminalPhase = PhaseCancelled
	return t.snapshotLocked()
}

func (t *PhaseTracker) applyErrorWrappedRecordLocked(rawType string, data json.RawMessage) {
	switch rawType {
	case "compaction_end", "auto_compaction_end":
		_, aborted, _, errMsg := parseCompactionEnd(data)
		t.compacting = nil
		if t.preCompactPhase != "" {
			t.basePhase = t.preCompactPhase
			t.preCompactPhase = ""
		}
		if aborted {
			t.terminalPhase = PhaseCancelled
			t.clearTransientStacksLocked()
			return
		}
		t.lastError = firstString(errMsg, extractErrorMessage(data), "compaction failed")
		t.transientFailed = true

	case "auto_retry_end":
		_, finalErr, _ := parseRetryEnd(data)
		t.retrying = nil
		t.lastError = firstString(finalErr, extractErrorMessage(data), "automatic retry failed")
		t.transientFailed = true

	case "message_end", "turn_end":
		stopReason, errMsg, _ := parseTurnEndStatus(data)
		if stopReason == "aborted" {
			t.lastError = ""
			t.terminalPhase = PhaseCancelled
			t.clearTransientStacksLocked()
			return
		}
		t.lastError = firstString(errMsg, extractErrorMessage(data), "assistant turn failed")
		t.transientFailed = true

	default:
		t.lastError = firstString(extractErrorMessage(data), t.lastError, "provider error")
		t.transientFailed = true
	}
}

func (t *PhaseTracker) applyMessageUpdateLocked(event provider.Event) {
	if len(event.Data) == 0 {
		if event.Delta != "" {
			t.transientFailed = false
			t.retrying = nil
			t.basePhase = PhaseStreaming
		}
		return
	}
	var payload struct {
		AssistantMessageEvent struct {
			Type   string          `json:"type"`
			Delta  string          `json:"delta"`
			Reason string          `json:"reason"`
			Error  json.RawMessage `json:"error"`
		} `json:"assistantMessageEvent"`
	}
	if json.Unmarshal(event.Data, &payload) != nil {
		return
	}
	switch payload.AssistantMessageEvent.Type {
	case "thinking_start", "thinking_delta":
		t.transientFailed = false
		t.lastError = ""
		t.retrying = nil
		t.basePhase = PhaseThinking
	case "text_start", "text_delta":
		t.transientFailed = false
		t.lastError = ""
		t.retrying = nil
		t.basePhase = PhaseStreaming
	case "toolcall_start", "toolcall_delta":
		t.transientFailed = false
		t.retrying = nil
		if t.basePhase == PhaseStarting {
			t.basePhase = PhaseThinking
		}
	case "error":
		t.lastError = firstString(decodeRawErrorText(payload.AssistantMessageEvent.Error), payload.AssistantMessageEvent.Reason, "assistant stream error")
		t.transientFailed = true
	}
}

func (t *PhaseTracker) pushToolLocked(data json.RawMessage) {
	callID, name, args := parseToolPayload(data)
	if name == "" {
		name = "tool"
	}
	target := SummarizeToolTarget(name, args)
	info := ActiveToolInfo{
		CallID: callID,
		Name:   name,
		Target: target,
	}
	if callID != "" {
		for i := range t.activeTools {
			if t.activeTools[i].CallID == callID {
				t.activeTools[i] = info
				return
			}
		}
	}
	t.activeTools = append(t.activeTools, info)
}

func (t *PhaseTracker) updateToolLocked(data json.RawMessage) {
	callID, name, args := parseToolPayload(data)
	if callID == "" {
		return
	}
	for i := range t.activeTools {
		if t.activeTools[i].CallID == callID {
			if name != "" {
				t.activeTools[i].Name = name
			}
			if len(args) > 0 {
				if target := SummarizeToolTarget(t.activeTools[i].Name, args); target != "" {
					t.activeTools[i].Target = target
				}
			}
			return
		}
	}
}

func (t *PhaseTracker) popToolLocked(data json.RawMessage) {
	callID, name, _ := parseToolPayload(data)
	if callID != "" {
		// Any dialog bound to this specific tool execution must already have
		// been answered or dismissed in Pi before tool_execution_end fires.
		filteredApprovals := t.pendingApprovals[:0]
		for _, ap := range t.pendingApprovals {
			if ap.boundToolCallID != callID {
				filteredApprovals = append(filteredApprovals, ap)
			}
		}
		t.pendingApprovals = filteredApprovals

		for i := len(t.activeTools) - 1; i >= 0; i-- {
			if t.activeTools[i].CallID == callID {
				t.activeTools = append(t.activeTools[:i], t.activeTools[i+1:]...)
				return
			}
		}
	}
	if name != "" {
		for i := len(t.activeTools) - 1; i >= 0; i-- {
			if t.activeTools[i].Name == name {
				t.activeTools = append(t.activeTools[:i], t.activeTools[i+1:]...)
				return
			}
		}
	}
	if len(t.activeTools) > 0 {
		t.activeTools = t.activeTools[:len(t.activeTools)-1]
	}
}

func (t *PhaseTracker) pushApprovalLocked(data json.RawMessage) {
	ap, ok := parseApprovalRequest(data)
	if !ok {
		return
	}
	if len(t.activeTools) > 0 {
		ap.boundToolCallID = t.activeTools[len(t.activeTools)-1].CallID
	}
	for i := range t.pendingApprovals {
		if t.pendingApprovals[i].RequestID == ap.RequestID {
			t.pendingApprovals[i] = ap
			return
		}
	}
	t.pendingApprovals = append(t.pendingApprovals, ap)
}

func (t *PhaseTracker) popApprovalByResponseLocked(data json.RawMessage) {
	var resp struct {
		ID        string `json:"id"`
		RequestID string `json:"requestId"`
	}
	if len(data) > 0 && json.Unmarshal(data, &resp) == nil {
		id := firstString(resp.RequestID, resp.ID)
		if id != "" {
			t.removeApprovalByIDLocked(id)
			return
		}
	}
	if len(t.pendingApprovals) > 0 {
		t.pendingApprovals = t.pendingApprovals[:len(t.pendingApprovals)-1]
	}
}

func (t *PhaseTracker) applyControlResultLocked(data json.RawMessage) {
	if len(data) == 0 {
		return
	}
	var result provider.ControlResult
	if json.Unmarshal(data, &result) != nil || result.ID == "" {
		return
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	if status == "delivered" || status == "failed" || status == "unknown" {
		t.removeApprovalByIDLocked(result.ID)
	}
}

func (t *PhaseTracker) removeApprovalByIDLocked(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		if len(t.pendingApprovals) > 0 {
			t.pendingApprovals = t.pendingApprovals[:len(t.pendingApprovals)-1]
		}
		return
	}
	for i := len(t.pendingApprovals) - 1; i >= 0; i-- {
		reqID := t.pendingApprovals[i].RequestID
		if reqID == id || strings.HasSuffix(id, ":"+reqID) || strings.HasSuffix(reqID, ":"+id) {
			t.pendingApprovals = append(t.pendingApprovals[:i], t.pendingApprovals[i+1:]...)
			return
		}
	}
}

func (t *PhaseTracker) clearTransientStacksLocked() {
	t.activeTools = nil
	t.pendingApprovals = nil
	t.compacting = nil
	t.retrying = nil
	t.preCompactPhase = ""
}

func (t *PhaseTracker) snapshotLocked() PhaseSnapshot {
	snap := PhaseSnapshot{
		Sequence:  t.lastSequence,
		UpdatedAt: t.updatedAt,
	}

	if len(t.activeTools) > 0 {
		snap.ActiveTools = append([]ActiveToolInfo(nil), t.activeTools...)
	}

	if t.terminalPhase != "" {
		snap.Phase = t.terminalPhase
		snap.ActiveTools = nil
		if t.terminalPhase == PhaseFailed {
			snap.Error = t.lastError
		}
		return snap
	}

	// Priority order for nested active states:
	// 1. Retrying (backoff countdown overrides outer compaction or turn state)
	// 2. WaitingApproval (human action required before a blocked tool can proceed)
	// 3. Compacting (context window summarization in progress)
	// 4. ExecutingTool (one or more tool calls running)
	// 5. Transient failure awaiting settlement or base streaming/thinking/starting state
	switch {
	case t.retrying != nil:
		retryCopy := *t.retrying
		snap.Phase = PhaseRetrying
		snap.Retry = &retryCopy
		snap.RetryAttempt = retryCopy.Attempt
		snap.RetryMaxAttempts = retryCopy.MaxAttempts
		snap.RetryReason = retryCopy.Reason
		if retryCopy.DelayMs > 0 {
			snap.RetryWaitSeconds = int(math.Ceil(float64(retryCopy.DelayMs) / 1000.0))
		} else if retryCopy.WaitSeconds > 0 {
			snap.RetryWaitSeconds = int(math.Ceil(retryCopy.WaitSeconds))
		}
		if t.compacting != nil {
			compCopy := *t.compacting
			snap.Compaction = &compCopy
		}

	case len(t.pendingApprovals) > 0:
		apCopy := t.pendingApprovals[len(t.pendingApprovals)-1]
		snap.Phase = PhaseWaitingApproval
		snap.Approval = &apCopy
		snap.PendingApprovals = len(t.pendingApprovals)
		if len(t.activeTools) > 0 {
			top := t.activeTools[len(t.activeTools)-1]
			snap.ToolName = top.Name
			snap.ToolTarget = top.Target
		}

	case t.compacting != nil:
		compCopy := *t.compacting
		snap.Phase = PhaseCompacting
		snap.Compaction = &compCopy

	case len(t.activeTools) > 0:
		top := t.activeTools[len(t.activeTools)-1]
		snap.Phase = PhaseExecutingTool
		snap.ToolName = top.Name
		snap.ToolTarget = top.Target

	case t.transientFailed:
		snap.Phase = PhaseFailed
		snap.Error = t.lastError

	default:
		snap.Phase = t.basePhase
		if snap.Phase == "" {
			snap.Phase = PhaseStarting
		}
	}

	return snap
}

var secretTokenPattern = regexp.MustCompile(`(?i)(bearer\s+[a-z0-9._\-]+|sk-[a-z0-9_\-]{12,}|api[_-]?key[=:\s]+[^\s"']+)`)

// SummarizeToolTarget extracts a short, single-line, credential-redacted target
// summary (such as a file path, shell command, or search query) from raw tool JSON args.
func SummarizeToolTarget(toolName string, rawArgs json.RawMessage) string {
	trimmed := bytes.TrimSpace(rawArgs)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var strArg string
	if json.Unmarshal(trimmed, &strArg) == nil {
		return cleanTargetText(strArg, 72)
	}
	var m map[string]any
	if json.Unmarshal(trimmed, &m) != nil || len(m) == 0 {
		return ""
	}

	lowerTool := strings.ToLower(strings.TrimSpace(toolName))
	switch lowerTool {
	case "bash", "shell", "exec", "terminal":
		if cmd := lookupString(m, "command", "cmd", "script"); cmd != "" {
			return cleanTargetText(cmd, 72)
		}
	case "read", "write", "edit", "multi_edit", "view":
		if p := lookupString(m, "path", "file_path", "filePath", "file", "filename"); p != "" {
			return cleanTargetText(p, 72)
		}
	case "ffgrep", "grep", "rg", "search":
		pat := lookupString(m, "pattern", "query", "q")
		scope := lookupString(m, "path", "dir", "glob")
		if pat != "" && scope != "" {
			return cleanTargetText(pat+" in "+scope, 72)
		}
		if pat != "" {
			return cleanTargetText(pat, 72)
		}
	case "fffind", "find", "glob", "ls":
		pat := lookupString(m, "pattern", "glob", "path", "dir")
		if pat != "" {
			return cleanTargetText(pat, 72)
		}
	}

	priorityKeys := []string{
		"path", "file_path", "filePath", "command", "cmd",
		"pattern", "query", "url", "uri", "target",
		"title", "task", "name", "action", "id",
	}
	for _, key := range priorityKeys {
		if val := lookupString(m, key); val != "" {
			return cleanTargetText(val, 72)
		}
	}
	return ""
}

func lookupString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || raw == nil {
			continue
		}
		if s, ok := raw.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func cleanTargetText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	s = strings.Join(strings.Fields(s), " ")
	s = secretTokenPattern.ReplaceAllString(s, "[REDACTED]")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:maxRunes-1])) + "…"
}

func parseToolPayload(data json.RawMessage) (callID, name string, args json.RawMessage) {
	if len(data) == 0 {
		return "", "", nil
	}
	var p struct {
		ToolCallID      string          `json:"toolCallId"`
		ToolCallIDSnake string          `json:"tool_call_id"`
		ID              string          `json:"id"`
		ToolName        string          `json:"toolName"`
		ToolNameSnake   string          `json:"tool_name"`
		Name            string          `json:"name"`
		Args            json.RawMessage `json:"args"`
		Arguments       json.RawMessage `json:"arguments"`
		Input           json.RawMessage `json:"input"`
	}
	if json.Unmarshal(data, &p) != nil {
		return "", "", nil
	}
	callID = firstString(p.ToolCallID, p.ToolCallIDSnake, p.ID)
	name = firstString(p.ToolName, p.ToolNameSnake, p.Name)
	switch {
	case len(p.Args) > 0 && string(p.Args) != "null":
		args = p.Args
	case len(p.Arguments) > 0 && string(p.Arguments) != "null":
		args = p.Arguments
	case len(p.Input) > 0 && string(p.Input) != "null":
		args = p.Input
	}
	return callID, name, args
}

func parseApprovalRequest(data json.RawMessage) (ApprovalInfo, bool) {
	if len(data) == 0 {
		return ApprovalInfo{RequestID: "approval", Method: "confirm"}, true
	}
	type uiWire struct {
		ID      string  `json:"id"`
		Method  string  `json:"method"`
		Title   string  `json:"title"`
		Message string  `json:"message"`
		Params  *uiWire `json:"params"`
	}
	var w uiWire
	if json.Unmarshal(data, &w) != nil {
		return ApprovalInfo{}, false
	}
	if w.Params != nil {
		if w.Title == "" {
			w.Title = w.Params.Title
		}
		if w.Message == "" {
			w.Message = w.Params.Message
		}
		if w.Method == "" {
			w.Method = w.Params.Method
		}
	}
	method := strings.ToLower(strings.TrimSpace(w.Method))
	switch method {
	case "confirm", "select", "input", "editor":
	default:
		// Non-blocking extension UI notifications (notify, setStatus, etc.) do
		// not block execution on human approval.
		return ApprovalInfo{}, false
	}
	id := strings.TrimSpace(w.ID)
	if id == "" {
		return ApprovalInfo{}, false
	}
	title := strings.TrimSpace(w.Title)
	if title == "" {
		title = method
	}
	return ApprovalInfo{
		RequestID: id,
		Method:    method,
		Title:     title,
		Message:   strings.TrimSpace(w.Message),
	}, true
}

func parseCompactionStart(data json.RawMessage) *CompactionInfo {
	info := &CompactionInfo{}
	if len(data) == 0 {
		return info
	}
	var w struct {
		Reason       string `json:"reason"`
		TokensBefore int64  `json:"tokensBefore"`
	}
	if json.Unmarshal(data, &w) == nil {
		info.Reason = strings.TrimSpace(w.Reason)
		info.TokensBefore = w.TokensBefore
	}
	return info
}

func parseCompactionEnd(data json.RawMessage) (info *CompactionInfo, aborted, willRetry bool, errMsg string) {
	if len(data) == 0 {
		return nil, false, false, ""
	}
	var w struct {
		Reason       string `json:"reason"`
		Aborted      bool   `json:"aborted"`
		WillRetry    bool   `json:"willRetry"`
		ErrorMessage string `json:"errorMessage"`
		Result       *struct {
			TokensBefore         int64 `json:"tokensBefore"`
			EstimatedTokensAfter int64 `json:"estimatedTokensAfter"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &w) != nil {
		return nil, false, false, ""
	}
	if w.Result != nil {
		info = &CompactionInfo{
			Reason:               strings.TrimSpace(w.Reason),
			TokensBefore:         w.Result.TokensBefore,
			EstimatedTokensAfter: w.Result.EstimatedTokensAfter,
		}
	}
	return info, w.Aborted, w.WillRetry, strings.TrimSpace(w.ErrorMessage)
}

func parseRetryStart(data json.RawMessage) *RetryInfo {
	info := &RetryInfo{Attempt: 1}
	if len(data) == 0 {
		return info
	}
	var w struct {
		Attempt      int     `json:"attempt"`
		MaxAttempts  int     `json:"maxAttempts"`
		DelayMs      int64   `json:"delayMs"`
		RetryAfterMs int64   `json:"retry_after_ms"`
		WaitSeconds  float64 `json:"waitSeconds"`
		ErrorMessage string  `json:"errorMessage"`
		Reason       string  `json:"reason"`
		Error        string  `json:"error"`
	}
	if json.Unmarshal(data, &w) == nil {
		if w.Attempt > 0 {
			info.Attempt = w.Attempt
		}
		info.MaxAttempts = w.MaxAttempts
		delayMs := w.DelayMs
		if delayMs == 0 && w.RetryAfterMs > 0 {
			delayMs = w.RetryAfterMs
		}
		if delayMs > 0 {
			info.DelayMs = delayMs
			info.WaitSeconds = float64(delayMs) / 1000.0
		} else if w.WaitSeconds > 0 {
			info.WaitSeconds = w.WaitSeconds
			info.DelayMs = int64(w.WaitSeconds * 1000)
		}
		info.Reason = firstString(w.ErrorMessage, w.Reason, w.Error)
	}
	return info
}

func parseRetryEnd(data json.RawMessage) (success bool, finalError string, attempt int) {
	if len(data) == 0 {
		return true, "", 0
	}
	var w struct {
		Success      *bool  `json:"success"`
		Attempt      int    `json:"attempt"`
		FinalError   string `json:"finalError"`
		ErrorMessage string `json:"errorMessage"`
		Error        string `json:"error"`
	}
	if json.Unmarshal(data, &w) != nil {
		return false, "", 0
	}
	ok := true
	if w.Success != nil {
		ok = *w.Success
	} else if w.FinalError != "" || w.ErrorMessage != "" || w.Error != "" {
		ok = false
	}
	return ok, firstString(w.FinalError, w.ErrorMessage, w.Error), w.Attempt
}

func parseTurnEndStatus(data json.RawMessage) (stopReason, errMsg string, isAssistant bool) {
	if len(data) == 0 {
		return "", "", true
	}
	var w struct {
		Message *struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			StopReason2  string `json:"stop_reason"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &w) != nil || w.Message == nil {
		return "", "", false
	}
	role := strings.ToLower(strings.TrimSpace(w.Message.Role))
	if role != "" && role != "assistant" {
		return "", "", false
	}
	return firstString(w.Message.StopReason, w.Message.StopReason2), strings.TrimSpace(w.Message.ErrorMessage), true
}

func parseWillRetry(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}
	var w struct {
		WillRetry bool `json:"willRetry"`
	}
	return json.Unmarshal(data, &w) == nil && w.WillRetry
}

func parseAborted(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}
	var w struct {
		Aborted bool `json:"aborted"`
	}
	return json.Unmarshal(data, &w) == nil && w.Aborted
}

func extractErrorMessage(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var plain string
	if json.Unmarshal(data, &plain) == nil && strings.TrimSpace(plain) != "" {
		return strings.TrimSpace(plain)
	}
	var w struct {
		FinalError   string          `json:"finalError"`
		ErrorMessage string          `json:"errorMessage"`
		Message      json.RawMessage `json:"message"`
		Error        json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &w) == nil {
		if s := firstString(w.FinalError, w.ErrorMessage); s != "" {
			return s
		}
		if s := decodeRawErrorText(w.Error); s != "" {
			return s
		}
		var msgObj struct {
			ErrorMessage string `json:"errorMessage"`
		}
		if len(w.Message) > 0 && json.Unmarshal(w.Message, &msgObj) == nil && msgObj.ErrorMessage != "" {
			return strings.TrimSpace(msgObj.ErrorMessage)
		}
		if s := decodeRawErrorText(w.Message); s != "" {
			return s
		}
	}
	return ""
}

func decodeRawErrorText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(trimmed, &obj) == nil {
		return firstString(obj.Message, obj.Error)
	}
	return ""
}
