package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// Input kinds and statuses mirror pkg/api. A queue input is the durable FIFO of
// a thread; a steer input addresses exactly one active attempt.
const (
	inputKindQueue = "queue"
	inputKindSteer = "steer"

	inputQueued    = "queued"
	inputPending   = "pending"
	inputStarted   = "started"
	inputDelivered = "delivered"
	inputCancelled = "cancelled"
	inputFailed    = "failed"
	// inputUnknown is a steer whose delivery could not be confirmed. It is
	// terminal for scheduling: Core never resends it on its own.
	inputUnknown = "unknown"

	toolRunning     = "running"
	toolCompleted   = "completed"
	toolFailed      = "failed"
	toolInterrupted = "interrupted"

	requestPending    = "pending"
	requestResponding = "responding"
	requestResponded  = "responded"
	requestExpired    = "expired"
	requestFailed     = "failed"
)

const (
	// maxToolOutputBytes bounds the display text kept for one tool call.
	maxToolOutputBytes = 32 * 1024
	// controlRetryBase/Max bound the retry backoff of a control handoff that the
	// provider refused or could not persist yet.
	controlRetryBase = 200 * time.Millisecond
	controlRetryMax  = 5 * time.Second
)

// Event payloads. They carry the committed entity like the other projections.
type inputEventData struct {
	Input api.Input `json:"input"`
}

type toolEventData struct {
	Tool api.ToolCall `json:"tool"`
}

type requestEventData struct {
	Request api.ProviderRequest `json:"request"`
}

// ---- input projection ----

type inputRecord struct {
	Configuration  *api.InputConfiguration
	ID             string
	ThreadID       string
	SenderThreadID string
	RunID          string
	AttemptID      string
	Kind           string
	Message        string
	Attachments    []api.Attachment
	CommandID      string
	Status         string
	Position       int
	Disposition    string
	Error          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const inputColumns = `id, thread_id, run_id, attempt_id, kind, message, attachments, command_id,
	status, disposition, error, created_at, updated_at, configuration, queue_order, sender_thread_id`

func (r inputRecord) toAPI() api.Input {
	return api.Input{
		CommandID:      r.CommandID,
		Configuration:  r.Configuration,
		ID:             r.ID,
		ThreadID:       r.ThreadID,
		SenderThreadID: r.SenderThreadID,
		RunID:          r.RunID,
		AttemptID:      r.AttemptID,
		Kind:           r.Kind,
		Message:        r.Message,
		Attachments:    r.Attachments,
		Status:         r.Status,
		Position:       r.Position,
		Error:          r.Error,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
}

func scanInput(row scanner) (inputRecord, error) {
	var (
		rec              inputRecord
		attachments      string
		configuration    string
		created, updated int64
	)
	if err := row.Scan(&rec.ID, &rec.ThreadID, &rec.RunID, &rec.AttemptID, &rec.Kind,
		&rec.Message, &attachments, &rec.CommandID, &rec.Status, &rec.Disposition,
		&rec.Error, &created, &updated, &configuration, &rec.Position, &rec.SenderThreadID); err != nil {
		return inputRecord{}, err
	}
	if attachments != "" && attachments != "[]" {
		if err := json.Unmarshal([]byte(attachments), &rec.Attachments); err != nil {
			return inputRecord{}, wrapInternal("decode input attachments", err)
		}
	}
	if configuration != "" {
		if err := json.Unmarshal([]byte(configuration), &rec.Configuration); err != nil {
			return inputRecord{}, wrapInternal("decode input configuration", err)
		}
	}
	rec.CreatedAt = fromNanos(created)
	rec.UpdatedAt = fromNanos(updated)
	return rec, nil
}

func loadInput(t *txn, inputID string) (inputRecord, error) {
	return scanInput(t.queryRow(`SELECT `+inputColumns+` FROM inputs WHERE id = ?`, inputID))
}

func (c *Core) insertInput(t *txn, rec inputRecord) error {
	now := time.Now().UTC()
	rec.CreatedAt, rec.UpdatedAt = now, now
	if rec.Kind == inputKindQueue && rec.Status == inputQueued {
		if err := t.queryRow(`SELECT COALESCE(MAX(queue_order) + 1, 0) FROM inputs
			WHERE thread_id = ? AND kind = ? AND status = ?`, rec.ThreadID, inputKindQueue, inputQueued).Scan(&rec.Position); err != nil {
			return wrapInternal("read next queue position", err)
		}
	}
	attachments := "[]"
	if len(rec.Attachments) > 0 {
		raw, err := json.Marshal(rec.Attachments)
		if err != nil {
			return wrapInternal("encode input attachments", err)
		}
		attachments = string(raw)
	}
	configuration := ""
	if rec.Configuration != nil {
		raw, err := json.Marshal(rec.Configuration)
		if err != nil {
			return wrapInternal("encode input configuration", err)
		}
		configuration = string(raw)
	}
	if _, err := t.exec(`INSERT INTO inputs
		(id, thread_id, run_id, attempt_id, kind, message, attachments, command_id,
		 status, disposition, error, created_at, updated_at, configuration, queue_order, sender_thread_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.ThreadID, rec.RunID, rec.AttemptID, rec.Kind, rec.Message, attachments,
		rec.CommandID, rec.Status, rec.Disposition, rec.Error,
		now.UnixNano(), now.UnixNano(), configuration, rec.Position, rec.SenderThreadID); err != nil {
		return wrapInternal("insert input", err)
	}
	return c.emitInput(t, rec)
}

// updateInput persists editable fields and control/queue state. Configuration
// remains the accepted snapshot; an explicit validated edit can replace files.
func (c *Core) updateInput(t *txn, rec inputRecord) error {
	rec.UpdatedAt = time.Now().UTC()
	attachments := "[]"
	if len(rec.Attachments) > 0 {
		raw, err := json.Marshal(rec.Attachments)
		if err != nil {
			return wrapInternal("encode input attachments", err)
		}
		attachments = string(raw)
	}
	if _, err := t.exec(`UPDATE inputs SET run_id = ?, attempt_id = ?, kind = ?, message = ?, command_id = ?, attachments = ?,
		status = ?, queue_order = ?, disposition = ?, error = ?, updated_at = ? WHERE id = ?`,
		rec.RunID, rec.AttemptID, rec.Kind, rec.Message, rec.CommandID, attachments,
		rec.Status, rec.Position, rec.Disposition, rec.Error, rec.UpdatedAt.UnixNano(), rec.ID); err != nil {
		return wrapInternal("update input", err)
	}
	return c.emitInput(t, rec)
}

// emitInput commits the input projection and its two legacy projections. The
// input row is the only authority; the older message events are derived from it
// so an existing client keeps working without a second scheduler.
func (c *Core) emitInput(t *txn, rec inputRecord) error {
	if _, err := t.event(eventInputUpdated, rec.ThreadID, rec.RunID, rec.AttemptID,
		inputEventData{Input: rec.toAPI()}); err != nil {
		return err
	}
	switch rec.Kind {
	case inputKindQueue:
		if _, err := t.event(eventMessageUpdated, rec.ThreadID, rec.RunID, rec.AttemptID,
			messageEventData{Message: rec.toQueuedMessage()}); err != nil {
			return err
		}
	case inputKindSteer:
		if _, err := t.event(eventSteeringUpdated, rec.ThreadID, rec.RunID, rec.AttemptID,
			steeringEventData{Steering: rec.toSteeringMessage()}); err != nil {
			return err
		}
	}
	return nil
}

// nextQueuedInput reads the durable queue order. Creation time and ID break
// ties in historical databases that predate explicit positions.
func nextQueuedInput(t *txn, threadID string) (inputRecord, bool, error) {
	rec, err := scanInput(t.queryRow(`SELECT `+inputColumns+` FROM inputs
		WHERE thread_id = ? AND kind = ? AND status = ?
		ORDER BY queue_order, created_at, id LIMIT 1`, threadID, inputKindQueue, inputQueued))
	if errors.Is(err, sql.ErrNoRows) {
		return inputRecord{}, false, nil
	}
	if err != nil {
		return inputRecord{}, false, wrapInternal("read queued input", err)
	}
	return rec, true, nil
}

// startQueuedInput starts the next queued message of a thread, if (and only if)
// the thread's single run slot is free. It is called after a run has completely
// settled, so the next item can never overtake a parent continuation, which
// occupies the same slot.
func (c *Core) startQueuedInput(t *txn, threadID string) (bool, error) {
	active, err := activeRunID(t, threadID)
	if err != nil || active != "" {
		return false, err
	}
	input, found, err := nextQueuedInput(t, threadID)
	if err != nil || !found {
		return false, err
	}
	thread, err := loadThread(t, threadID)
	if err != nil {
		return false, wrapInternal("read thread for queued input", err)
	}
	if thread.Archived {
		// An archived thread never resumes work on its own. Archiving refuses a
		// non-empty queue, so this only matters for a migrated database.
		return false, nil
	}
	input, err = startInputRun(t, thread, input)
	if err != nil {
		return false, err
	}
	if err := c.updateInput(t, input); err != nil {
		return false, err
	}
	return true, c.reindexQueuedInputs(t, threadID)
}

// cancelQueuedInputs drops every message that has not started yet. A stop must
// never let the thread continue on its own afterwards.
func (c *Core) cancelQueuedInputs(t *txn, threadID, reason string) error {
	rows, err := t.query(`SELECT `+inputColumns+` FROM inputs
		WHERE thread_id = ? AND kind = ? AND status = ? ORDER BY created_at, id`,
		threadID, inputKindQueue, inputQueued)
	if err != nil {
		return wrapInternal("read queued inputs", err)
	}
	var pending []inputRecord
	for rows.Next() {
		rec, err := scanInput(rows)
		if err != nil {
			rows.Close()
			return wrapInternal("scan queued input", err)
		}
		pending = append(pending, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return wrapInternal("read queued inputs", err)
	}
	rows.Close()

	for _, rec := range pending {
		rec.Status, rec.Error = inputCancelled, reason
		if err := c.updateInput(t, rec); err != nil {
			return err
		}
	}
	return nil
}

// ---- tool projection ----

type toolRecord struct {
	ID        string
	ThreadID  string
	RunID     string
	AttemptID string
	Name      string
	Arguments json.RawMessage
	Output    string
	Status    string
	UpdatedAt time.Time
	Offset    int
}

const toolColumns = `id, thread_id, run_id, attempt_id, name, arguments, output, status, updated_at, output_offset`

func (r toolRecord) toAPI() api.ToolCall {
	return api.ToolCall{
		ID:        r.ID,
		ThreadID:  r.ThreadID,
		RunID:     r.RunID,
		AttemptID: r.AttemptID,
		Name:      r.Name,
		Arguments: r.Arguments,
		Output:    r.Output,
		Status:    r.Status,
		UpdatedAt: r.UpdatedAt,

		OutputOffset: r.Offset,
	}
}

func scanTool(row scanner) (toolRecord, error) {
	var (
		rec     toolRecord
		args    []byte
		updated int64
	)
	if err := row.Scan(&rec.ID, &rec.ThreadID, &rec.RunID, &rec.AttemptID, &rec.Name,
		&args, &rec.Output, &rec.Status, &updated, &rec.Offset); err != nil {
		return toolRecord{}, err
	}
	if len(args) > 0 {
		rec.Arguments = json.RawMessage(args)
	}
	rec.UpdatedAt = fromNanos(updated)
	return rec, nil
}

func loadTool(t *txn, toolID string) (toolRecord, error) {
	return scanTool(t.queryRow(`SELECT `+toolColumns+` FROM tool_calls WHERE id = ?`, toolID))
}

func (c *Core) saveTool(t *txn, rec toolRecord, exists bool) error {
	rec.UpdatedAt = time.Now().UTC()
	args := []byte(rec.Arguments)
	if exists {
		if _, err := t.exec(`UPDATE tool_calls SET name = ?, arguments = ?, output = ?, status = ?, updated_at = ?
			WHERE id = ?`, rec.Name, args, rec.Output, rec.Status, rec.UpdatedAt.UnixNano(), rec.ID); err != nil {
			return wrapInternal("update tool call", err)
		}
	} else if _, err := t.exec(`INSERT INTO tool_calls
		(id, thread_id, run_id, attempt_id, name, arguments, output, status, updated_at, output_offset)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.ThreadID, rec.RunID, rec.AttemptID, rec.Name, args, rec.Output,
		rec.Status, rec.UpdatedAt.UnixNano(), rec.Offset); err != nil {
		return wrapInternal("insert tool call", err)
	}
	_, err := t.event(eventToolUpdated, rec.ThreadID, rec.RunID, rec.AttemptID, toolEventData{Tool: rec.toAPI()})
	return err
}

// ---- provider request projection ----

type requestRecord struct {
	ID                string
	ProviderRequestID string
	ThreadID          string
	RunID             string
	AttemptID         string
	Method            string
	Title             string
	Message           string
	Options           []string
	Prefill           string
	Status            string
	Error             string
	ExpiresAt         time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const requestColumns = `id, provider_request_id, thread_id, run_id, attempt_id, method, title, message,
	options, prefill, status, error, expires_at, created_at, updated_at`

func (r requestRecord) toAPI() api.ProviderRequest {
	return api.ProviderRequest{
		ID:                r.ID,
		ProviderRequestID: r.ProviderRequestID,
		ThreadID:          r.ThreadID,
		RunID:             r.RunID,
		AttemptID:         r.AttemptID,
		Method:            r.Method,
		Title:             r.Title,
		Message:           r.Message,
		Options:           r.Options,
		Prefill:           r.Prefill,
		Status:            r.Status,
		Error:             r.Error,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func scanRequest(row scanner) (requestRecord, error) {
	var (
		rec              requestRecord
		options          string
		expires          int64
		created, updated int64
	)
	if err := row.Scan(&rec.ID, &rec.ProviderRequestID, &rec.ThreadID, &rec.RunID, &rec.AttemptID,
		&rec.Method, &rec.Title, &rec.Message, &options, &rec.Prefill, &rec.Status, &rec.Error,
		&expires, &created, &updated); err != nil {
		return requestRecord{}, err
	}
	if options != "" {
		_ = json.Unmarshal([]byte(options), &rec.Options)
	}
	if expires > 0 {
		rec.ExpiresAt = fromNanos(expires)
	}
	rec.CreatedAt = fromNanos(created)
	rec.UpdatedAt = fromNanos(updated)
	return rec, nil
}

func loadRequest(t *txn, requestID string) (requestRecord, error) {
	return scanRequest(t.queryRow(`SELECT `+requestColumns+` FROM provider_requests WHERE id = ?`, requestID))
}

func (c *Core) insertRequest(t *txn, rec requestRecord) error {
	now := time.Now().UTC()
	rec.CreatedAt, rec.UpdatedAt = now, now
	options, err := json.Marshal(rec.Options)
	if err != nil {
		return err
	}
	var expires int64
	if !rec.ExpiresAt.IsZero() {
		expires = rec.ExpiresAt.UnixNano()
	}
	if _, err := t.exec(`INSERT INTO provider_requests
		(id, provider_request_id, thread_id, run_id, attempt_id, method, title, message, options,
		 prefill, status, error, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.ProviderRequestID, rec.ThreadID, rec.RunID, rec.AttemptID, rec.Method,
		rec.Title, rec.Message, string(options), rec.Prefill, rec.Status, rec.Error,
		expires, now.UnixNano(), now.UnixNano()); err != nil {
		return wrapInternal("insert provider request", err)
	}
	return c.emitRequest(t, rec)
}

func (c *Core) updateRequest(t *txn, rec requestRecord) error {
	rec.UpdatedAt = time.Now().UTC()
	if _, err := t.exec(`UPDATE provider_requests SET status = ?, error = ?, updated_at = ?
		WHERE id = ?`, rec.Status, rec.Error, rec.UpdatedAt.UnixNano(), rec.ID); err != nil {
		return wrapInternal("update provider request", err)
	}
	return c.emitRequest(t, rec)
}

func (c *Core) emitRequest(t *txn, rec requestRecord) error {
	_, err := t.event(eventRequestUpdated, rec.ThreadID, rec.RunID, rec.AttemptID,
		requestEventData{Request: rec.toAPI()})
	return err
}

// ---- provider request construction ----

// providerRequestForAttempt builds the immutable handoff request for one attempt
// of a run: the attempt's own input, the run configuration and its approval
// policy, so the worker never has to re-derive any of it.
func providerRequestForAttempt(t *txn, run runRecord, attemptID string) (provider.Request, error) {
	request := provider.Request{
		ThreadID:        run.ThreadID,
		SenderThreadID:  run.SenderThreadID,
		RunID:           run.ID,
		AttemptID:       attemptID,
		Message:         run.Input,
		Attachments:     run.Attachments,
		ProviderID:      run.ProviderID,
		Model:           run.Model,
		Workspace:       run.Workspace,
		ApprovalPolicy:  run.ApprovalPolicy,
		InteractionMode: run.InteractionMode,
	}
	if run.SenderThreadID != "" {
		err := t.queryRow(`SELECT title FROM threads WHERE id = ?`, run.SenderThreadID).Scan(&request.SenderTitle)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return provider.Request{}, wrapInternal("read sender title", err)
		}
	}
	if attemptID == "" {
		return request, nil
	}
	var input string
	err := t.queryRow(`SELECT input FROM attempts WHERE id = ?`, attemptID).Scan(&input)
	switch {
	case err == nil && input != "":
		request.Message = input
	case err == nil, errors.Is(err, sql.ErrNoRows):
	default:
		return provider.Request{}, wrapInternal("read attempt input", err)
	}
	return request, nil
}

// ---- control outbox ----

// controlIntent is the persisted unit of a steer or a dialog response. Its ID
// is the Input ID (steer) or the ProviderRequest ID (response), so a Core crash
// can hand the same intent over again without the worker resending a control Pi
// already received.
type controlIntent struct {
	Request provider.Request `json:"request"`
	Control provider.Control `json:"control"`
	outbox  string           `json:"-"`
}

// queueControl records the durable control intent in the same transaction as the
// state change that produced it. It is not an acknowledgement from Pi.
func (c *Core) queueControl(t *txn, request provider.Request, control provider.Control) error {
	payload, err := json.Marshal(controlIntent{Request: request, Control: control})
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := t.exec(`INSERT INTO effect_outbox
		(id, kind, thread_id, run_id, attempt_id, status, attempt_count, claim_token, lease_until,
		 last_error, created_at, updated_at, input)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', 0, '', ?, ?, ?)`,
		newID("eff"), effectKindProviderControl, request.ThreadID, request.RunID, request.AttemptID,
		outboxPending, now.UnixNano(), now.UnixNano(), string(payload)); err != nil {
		return wrapInternal("record control intent", err)
	}
	return nil
}

// drainControlEffects hands persisted control intents to the provider. A nil
// return only means the handoff is durable; the steer or dialog changes state
// when the provider reports control_result.
func (c *Core) drainControlEffects(ctx context.Context) {
	controller, ok := c.provider.(provider.Controller)
	if !ok {
		return
	}
	now := nowNanos()
	c.lifeMu.RLock()
	if c.state.Load() == stateStopped {
		c.lifeMu.RUnlock()
		return
	}
	rows, err := c.store.db.QueryContext(ctx, `SELECT id, run_id, attempt_id, input FROM effect_outbox
		WHERE kind = ? AND status = ? AND (lease_until = 0 OR lease_until <= ?)
		ORDER BY created_at, id LIMIT 32`, effectKindProviderControl, outboxPending, now)
	if err != nil {
		c.lifeMu.RUnlock()
		return
	}
	var candidates []controlIntent
	for rows.Next() {
		var (
			id, runID, attemptID string
			payload              string
		)
		if rows.Scan(&id, &runID, &attemptID, &payload) != nil {
			break
		}
		var intent controlIntent
		if json.Unmarshal([]byte(payload), &intent) != nil {
			continue
		}
		if intent.Request.RunID == "" {
			intent.Request.RunID = runID
		}
		if intent.Request.AttemptID == "" {
			intent.Request.AttemptID = attemptID
		}
		intent.outbox = id
		candidates = append(candidates, intent)
	}
	_ = rows.Close()
	c.lifeMu.RUnlock()

	for _, intent := range candidates {
		if ctx.Err() != nil {
			return
		}
		// The target attempt must still be the active one; a control for a
		// settled attempt is resolved by the attempt cleanup instead.
		if !c.runIsActive(ctx, intent.Request.RunID, intent.Request.AttemptID) {
			c.failControlTarget(intent, "attempt is no longer active")
			continue
		}
		handoffErr := controller.Control(ctx, intent.Request, intent.Control)
		c.settleControlIntent(intent, handoffErr)
	}
}

// settleControlIntent records the handoff result of one control intent. A
// successful handoff keeps the steer/request pending until control_result.
func (c *Core) settleControlIntent(intent controlIntent, handoffErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	_ = c.write(ctx, func(t *txn) error {
		var (
			status  string
			attempt int64
		)
		err := t.queryRow(`SELECT status, attempt_count FROM effect_outbox WHERE id = ?`, intent.outbox).
			Scan(&status, &attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return wrapInternal("read control intent", err)
		}
		if status != outboxPending {
			return nil
		}
		now := time.Now().UTC()
		if handoffErr == nil {
			_, err = t.exec(`UPDATE effect_outbox SET status = ?, attempt_count = attempt_count + 1,
				last_error = '', updated_at = ? WHERE id = ? AND status = ?`,
				outboxSettled, now.UnixNano(), intent.outbox, outboxPending)
			return err
		}
		delay := controlRetryBase << min(attempt, 5)
		if delay > controlRetryMax {
			delay = controlRetryMax
		}
		_, err = t.exec(`UPDATE effect_outbox SET attempt_count = attempt_count + 1, last_error = ?,
			lease_until = ?, updated_at = ? WHERE id = ? AND status = ?`,
			handoffErr.Error(), now.Add(delay).UnixNano(), now.UnixNano(), intent.outbox, outboxPending)
		return err
	})
}

// failControlTarget resolves a control whose attempt is gone: the steer fails and
// the dialog expires. It never pretends Pi accepted anything.
func (c *Core) failControlTarget(intent controlIntent, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	_ = c.write(ctx, func(t *txn) error {
		if _, err := t.exec(`UPDATE effect_outbox SET status = ?, last_error = ?, updated_at = ?
			WHERE id = ? AND status IN (?, ?)`,
			outboxCancelled, reason, nowNanos(), intent.outbox, outboxPending, outboxClaimed); err != nil {
			return wrapInternal("cancel control intent", err)
		}
		return c.resolveControlTarget(t, intent.Control, inputFailed, requestExpired, reason)
	})
}

// resolveControlTarget marks the entity behind a control ID terminal when its
// attempt disappeared before the handoff. Steer IDs and dialog IDs share one
// namespace: an Input ID or a ProviderRequest ID. An unanswered dialog expires,
// it is never reported as failed or approved.
func (c *Core) resolveControlTarget(t *txn, control provider.Control, inputStatus, requestStatus, reason string) error {
	input, err := loadInput(t, control.ID)
	switch {
	case err == nil:
		if input.Kind != inputKindSteer || input.Status != inputPending {
			return nil
		}
		input.Status, input.Error = inputStatus, reason
		return c.updateInput(t, input)
	case !errors.Is(err, sql.ErrNoRows):
		return wrapInternal("read control input", err)
	}
	request, err := loadRequest(t, control.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return wrapInternal("read control request", err)
	}
	if request.Status != requestPending && request.Status != requestResponding {
		return nil
	}
	request.Status, request.Error = requestStatus, reason
	return c.updateRequest(t, request)
}

// ---- provider event projections ----

// toolEventPayload is the subset of Pi's tool_execution_* JSON that becomes the
// ToolCall projection. Raw arguments are preserved verbatim for display.
type toolEventPayload struct {
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Args          json.RawMessage `json:"args"`
	PartialResult json.RawMessage `json:"partialResult"`
	Result        json.RawMessage `json:"result"`
	IsError       bool            `json:"isError"`

	ToolCallIDSnake string          `json:"tool_call_id"`
	ToolNameSnake   string          `json:"tool_name"`
	ArgsSnake       json.RawMessage `json:"arguments"`
}

func (p toolEventPayload) callID() string {
	if p.ToolCallID != "" {
		return p.ToolCallID
	}
	return p.ToolCallIDSnake
}

func (p toolEventPayload) name() string {
	if p.ToolName != "" {
		return p.ToolName
	}
	return p.ToolNameSnake
}

func (p toolEventPayload) arguments() json.RawMessage {
	if len(p.Args) > 0 && string(p.Args) != "null" {
		return p.Args
	}
	if len(p.ArgsSnake) > 0 && string(p.ArgsSnake) != "null" {
		return p.ArgsSnake
	}
	return nil
}

// applyToolEvent projects one real Pi tool execution. The caller has already
// advanced the provider cursor in the same transaction, so a redelivered event
// cannot be applied twice.
func (c *Core) applyToolEvent(t *txn, run runRecord, attemptID string, event provider.Event) error {
	var payload toolEventPayload
	if len(event.Data) > 0 {
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil // an unparsable tool event is not a reason to fail the run
		}
	}
	callID := payload.callID()
	if callID == "" {
		return nil
	}
	toolID := attemptID + ":" + callID
	rec := toolRecord{
		ID:        toolID,
		ThreadID:  run.ThreadID,
		RunID:     run.ID,
		AttemptID: attemptID,
		Name:      payload.name(),
		Arguments: payload.arguments(),
		Offset:    len(run.Output),
	}
	status := toolRunning
	switch event.Type {
	case "tool_execution_start":
	case "tool_execution_update":
		rec.Output = boundedToolOutput(payload.PartialResult)
	case "tool_execution_end":
		rec.Output = boundedToolOutput(payload.Result)
		if rec.Output == "" {
			rec.Output = boundedToolOutput(payload.PartialResult)
		}
		if payload.IsError {
			status = toolFailed
		} else {
			status = toolCompleted
		}
	default:
		return nil
	}

	existing, err := loadTool(t, toolID)
	switch {
	case err == nil:
		if rec.Name != "" {
			existing.Name = rec.Name
		}
		if len(rec.Arguments) > 0 {
			existing.Arguments = rec.Arguments
		}
		if rec.Output != "" {
			existing.Output = rec.Output
		}
		existing.Status = status
		return c.saveTool(t, existing, true)
	case errors.Is(err, sql.ErrNoRows):
		rec.Status = status
		return c.saveTool(t, rec, false)
	default:
		return wrapInternal("read tool call", err)
	}
}

// applyUIRequest projects a blocking Pi dialog. Non-blocking convenience methods
// (notify, setStatus, ...) never become an approval-like request.
func (c *Core) applyUIRequest(t *txn, run runRecord, attemptID string, event provider.Event) error {
	var payload uiRequestPayload
	if len(event.Data) > 0 {
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil
		}
	}
	payload = payload.merged()
	method := strings.ToLower(strings.TrimSpace(payload.Method))
	switch method {
	case "confirm", "select", "input", "editor":
	default:
		return nil
	}
	if payload.ID == "" {
		return nil
	}
	requestID := attemptID + ":" + payload.ID
	if _, err := loadRequest(t, requestID); err == nil {
		return nil // duplicate delivery of a request already projected
	} else if !errors.Is(err, sql.ErrNoRows) {
		return wrapInternal("read provider request", err)
	}
	rec := requestRecord{
		ID:                requestID,
		ProviderRequestID: payload.ID,
		ThreadID:          run.ThreadID,
		RunID:             run.ID,
		AttemptID:         attemptID,
		Method:            method,
		Title:             payload.Title,
		Message:           payload.Message,
		Options:           payload.Options,
		Prefill:           payload.Prefill,
		Status:            requestPending,
	}
	// Pi owns the dialog's lifetime. A request without a timeout must remain
	// answerable while Pi waits; replaying its journal must not extend a deadline.
	if payload.Timeout > 0 {
		occurred := event.OccurredAt
		if occurred.IsZero() {
			occurred = time.Now().UTC()
		}
		rec.ExpiresAt = occurred.Add(time.Duration(payload.Timeout) * time.Millisecond)
	}
	if rec.Title == "" {
		rec.Title = rec.Method
	}
	return c.insertRequest(t, rec)
}

// uiRequestPayload accepts both the flat Pi shape and a params-object variant.
type uiRequestPayload struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Title   string            `json:"title"`
	Message string            `json:"message"`
	Options []string          `json:"options"`
	Prefill string            `json:"prefill"`
	Timeout int64             `json:"timeout"`
	Params  *uiRequestPayload `json:"params"`
}

// merged fills the empty fields from the nested params object when Pi wraps the
// dialog request. ID and method are always top-level.
func (p uiRequestPayload) merged() uiRequestPayload {
	if p.Params == nil {
		return p
	}
	nested := *p.Params
	if p.Title == "" {
		p.Title = nested.Title
	}
	if p.Message == "" {
		p.Message = nested.Message
	}
	if len(p.Options) == 0 {
		p.Options = nested.Options
	}
	if p.Prefill == "" {
		p.Prefill = nested.Prefill
	}
	if p.Timeout == 0 {
		p.Timeout = nested.Timeout
	}
	return p
}

// applyControlResult is the only thing that turns a handoff into an accepted
// steer or answered dialog.
func (c *Core) applyControlResult(t *txn, run runRecord, attemptID string, event provider.Event) error {
	if attemptID == "" {
		return nil
	}
	var result provider.ControlResult
	if len(event.Data) > 0 {
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return nil
		}
	}
	if result.ID == "" {
		return nil
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	reason := result.Error
	switch status {
	case "delivered", "failed", "unknown":
	default:
		status, reason = "failed", firstNonEmpty(reason, "provider reported an unknown control status")
	}
	if status == "failed" && reason == "" {
		reason = "provider rejected the control"
	}
	if status == "unknown" && reason == "" {
		reason = unconfirmedControlReason
	}

	// A result is accepted for its own run and attempt even when the attempt has
	// already settled: Pi may report the outcome of a control it did read just
	// before it stopped. The entity identity and its unresolved state are what
	// matter, and this path can never resend anything.
	input, err := loadInput(t, result.ID)
	switch {
	case err == nil:
		if input.RunID != run.ID || input.AttemptID != attemptID || input.Kind != inputKindSteer {
			return nil
		}
		if input.Status != inputPending && input.Status != inputUnknown {
			return nil
		}
		switch status {
		case "delivered":
			input.Status, input.Error = inputDelivered, ""
		case "unknown":
			input.Status, input.Error = inputUnknown, reason
		default:
			input.Status, input.Error = inputFailed, reason
		}
		return c.updateInput(t, input)
	case !errors.Is(err, sql.ErrNoRows):
		return wrapInternal("read control input", err)
	}
	request, err := loadRequest(t, result.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return wrapInternal("read control request", err)
	}
	if request.RunID != run.ID || request.AttemptID != attemptID ||
		(request.Status != requestPending && request.Status != requestResponding) {
		return nil
	}
	if status == "delivered" {
		request.Status, request.Error = requestResponded, ""
	} else {
		// A dialog answer that Pi did not confirm is never reported as answered.
		request.Status, request.Error = requestFailed, reason
	}
	return c.updateRequest(t, request)
}

// ---- terminal cleanup ----

// cleanupAttemptWork resolves everything an attempt leaves behind when it ends or
// is stopped: running tools are interrupted, unanswered dialogs expire and
// undelivered steers fail. A recoverable attempt that is only re-attached keeps
// its still-active dialogs, so this is never called for it.
func (c *Core) cleanupAttemptWork(t *txn, attemptID, reason string) error {
	if attemptID == "" {
		return nil
	}
	toolRows, err := t.query(`SELECT `+toolColumns+` FROM tool_calls
		WHERE attempt_id = ? AND status = ?`, attemptID, toolRunning)
	if err != nil {
		return wrapInternal("read running tools", err)
	}
	var tools []toolRecord
	for toolRows.Next() {
		rec, err := scanTool(toolRows)
		if err != nil {
			toolRows.Close()
			return wrapInternal("scan running tool", err)
		}
		tools = append(tools, rec)
	}
	if err := toolRows.Err(); err != nil {
		toolRows.Close()
		return wrapInternal("read running tools", err)
	}
	toolRows.Close()
	for _, rec := range tools {
		rec.Status = toolInterrupted
		if err := c.saveTool(t, rec, true); err != nil {
			return err
		}
	}

	requestRows, err := t.query(`SELECT `+requestColumns+` FROM provider_requests
		WHERE attempt_id = ? AND status IN (?, ?)`, attemptID, requestPending, requestResponding)
	if err != nil {
		return wrapInternal("read open requests", err)
	}
	var requests []requestRecord
	for requestRows.Next() {
		rec, err := scanRequest(requestRows)
		if err != nil {
			requestRows.Close()
			return wrapInternal("scan open request", err)
		}
		requests = append(requests, rec)
	}
	if err := requestRows.Err(); err != nil {
		requestRows.Close()
		return wrapInternal("read open requests", err)
	}
	requestRows.Close()
	for _, rec := range requests {
		rec.Status, rec.Error = requestExpired, reason
		if err := c.updateRequest(t, rec); err != nil {
			return err
		}
	}

	inputRows, err := t.query(`SELECT `+inputColumns+` FROM inputs
		WHERE attempt_id = ? AND kind = ? AND status = ?`, attemptID, inputKindSteer, inputPending)
	if err != nil {
		return wrapInternal("read pending steers", err)
	}
	var steers []inputRecord
	for inputRows.Next() {
		rec, err := scanInput(inputRows)
		if err != nil {
			inputRows.Close()
			return wrapInternal("scan pending steer", err)
		}
		steers = append(steers, rec)
	}
	if err := inputRows.Err(); err != nil {
		inputRows.Close()
		return wrapInternal("read pending steers", err)
	}
	inputRows.Close()
	for _, rec := range steers {
		rec.Status, rec.Error = inputFailed, reason
		if err := c.updateInput(t, rec); err != nil {
			return err
		}
	}

	if _, err := t.exec(`UPDATE effect_outbox SET status = ?, last_error = ?, updated_at = ?
		WHERE kind = ? AND attempt_id = ? AND status IN (?, ?)`,
		outboxCancelled, reason, nowNanos(), effectKindProviderControl, attemptID,
		outboxPending, outboxClaimed); err != nil {
		return wrapInternal("cancel control intents", err)
	}
	return nil
}

// expireRequests closes dialogs whose deadline passed before anyone answered
// them. An expired dialog can no longer be answered, so it is never mistaken for
// an approved one.
func (c *Core) expireRequests(ctx context.Context) {
	now := nowNanos()
	c.lifeMu.RLock()
	if c.state.Load() == stateStopped {
		c.lifeMu.RUnlock()
		return
	}
	rows, err := c.store.db.QueryContext(ctx, `SELECT id FROM provider_requests
		WHERE status = ? AND expires_at > 0 AND expires_at <= ? LIMIT 64`, requestPending, now)
	if err != nil {
		c.lifeMu.RUnlock()
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			break
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	c.lifeMu.RUnlock()
	if len(ids) == 0 {
		return
	}
	_ = c.write(ctx, func(t *txn) error {
		for _, id := range ids {
			rec, err := loadRequest(t, id)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return wrapInternal("read expiring request", err)
			}
			if rec.Status != requestPending || rec.ExpiresAt.IsZero() || rec.ExpiresAt.After(time.Now()) {
				continue
			}
			rec.Status, rec.Error = requestExpired, "request expired before it was answered"
			if err := c.updateRequest(t, rec); err != nil {
				return err
			}
		}
		return nil
	})
}

// hasWorkbenchWork is the read-only probe that keeps idle ticks cheap.
func (c *Core) hasWorkbenchWork(ctx context.Context, now int64) bool {
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return false
	}
	var n int
	err := c.store.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(1) FROM provider_requests WHERE status = ? AND expires_at > 0 AND expires_at <= ?)
		+ (SELECT COUNT(1) FROM effect_outbox WHERE kind = ? AND status = ? AND (lease_until = 0 OR lease_until <= ?))
		+ (SELECT COUNT(1) FROM inputs i WHERE i.kind = ? AND i.status = ?
			AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.thread_id = i.thread_id
				AND r.status IN (?, ?, ?)))`,
		requestPending, now, effectKindProviderControl, outboxPending, now,
		inputKindQueue, inputQueued, statusQueued, statusRunning, statusWaitingChildren).Scan(&n)
	return err == nil && n > 0
}

// boundedToolOutput keeps display text small and valid UTF-8, preserving both
// the head and tail so trailing errors and stack traces are never lost. It
// never treats tool output as executable content.
func boundedToolOutput(raw json.RawMessage) string {
	text := decodeToolOutput(raw)
	if len(text) <= maxToolOutputBytes {
		return text
	}

	const (
		maxHeadBytes = 12 * 1024
		headOnlyMark = "\n…(truncated)"
		maxMarkerLen = len("\n…(9999999999 bytes truncated)…\n")
	)

	headOnly := func(s string) string {
		h := maxToolOutputBytes - len(headOnlyMark)
		for h > 0 && !utf8.RuneStart(s[h]) {
			h--
		}
		return s[:h] + headOnlyMark
	}

	head := min(maxHeadBytes, len(text))
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	searchStart := max(0, head-256)
	if idx := strings.LastIndexByte(text[searchStart:head], '\n'); idx >= 0 && searchStart+idx > 512 {
		head = searchStart + idx
	}

	tailBudget := maxToolOutputBytes - head - maxMarkerLen
	if tailBudget <= 0 {
		return headOnly(text)
	}
	tailStart := len(text) - tailBudget
	for tailStart < len(text) && !utf8.RuneStart(text[tailStart]) {
		tailStart++
	}
	searchEnd := min(len(text), tailStart+256)
	if nl := strings.IndexByte(text[tailStart:searchEnd], '\n'); nl >= 0 && len(text)-(tailStart+nl+1) > 512 {
		tailStart += nl + 1
	}
	if tailStart <= head || tailStart >= len(text) {
		return headOnly(text)
	}

	marker := fmt.Sprintf("\n…(%d bytes truncated)…\n", tailStart-head)
	for head+len(marker)+(len(text)-tailStart) > maxToolOutputBytes {
		_, size := utf8.DecodeRuneInString(text[tailStart:])
		if size <= 0 || tailStart+size >= len(text) {
			return headOnly(text)
		}
		tailStart += size
		marker = fmt.Sprintf("\n…(%d bytes truncated)…\n", tailStart-head)
	}
	return text[:head] + marker + text[tailStart:]
}

// decodeToolOutput extracts display text from the shapes Pi uses for tool
// results: a string, a content array, or an arbitrary JSON value.
func decodeToolOutput(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var content struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &content) == nil && len(content.Content) > 0 {
		var b strings.Builder
		for _, part := range content.Content {
			if part.Text != "" {
				b.WriteString(part.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return trimmed
}
