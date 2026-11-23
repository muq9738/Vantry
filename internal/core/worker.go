package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// effect is one claimed outbox row: the durable right to execute provider work
// for exactly one run attempt.
type effect struct {
	id              string
	kind            string
	threadID        string
	senderThreadID  string
	senderTitle     string
	parentTaskID    string
	runID           string
	attemptID       string
	input           string
	token           string
	providerID      string
	model           string
	workspace       *api.WorkspaceSpec
	approvalPolicy  string
	interactionMode string
	attachments     []api.Attachment
	afterSequence   int64
}

// workerLoop claims durable effects and executes them outside any transaction.
// Each claim runs in its own goroutine, so providers for different threads run
// in parallel while SQLite still has exactly one writer.
func (c *Core) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(workerIdleInterval)
	defer ticker.Stop()
	var nextPrune time.Time
	if c.eventRetention >= 0 && c.eventPruneInterval > 0 {
		nextPrune = time.Now().Add(c.eventPruneInterval)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-ticker.C:
		}
		c.drainStopEffects(ctx)
		c.drainEffects(ctx)
		// Control handoffs and dialog deadlines are cheap to probe and expensive
		// to poll, so one read decides whether either sweep is needed.
		if c.hasWorkbenchWork(ctx, nowNanos()) {
			c.drainControlEffects(ctx)
			c.expireRequests(ctx)
		}
		c.reconcileSweep(ctx)
		if !nextPrune.IsZero() && !time.Now().Before(nextPrune) {
			nextPrune = time.Now().Add(c.eventPruneInterval)
			if c.hasPrunableEvents(ctx, c.eventRetention) {
				_, _, _ = c.PruneEvents(ctx, c.eventRetention)
			}
		}
	}
}

func (c *Core) drainEffects(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		eff, ok, err := c.claimNext(ctx)
		if err != nil || !ok {
			return
		}
		c.workWG.Add(1)
		go func(e effect) {
			defer c.workWG.Done()
			c.executeEffect(ctx, e)
		}(eff)
	}
}

// reconcileSweep repairs projection windows a crash can leave behind: a child
// run that is terminal but whose result was never delivered, or an accepted
// generation whose continuation was never created.
func (c *Core) reconcileSweep(ctx context.Context) {
	if !c.hasReconcileWork(ctx) {
		return
	}
	var started bool
	if err := c.write(ctx, func(t *txn) error {
		var err error
		started, err = c.reconcileAll(t)
		return err
	}); err != nil {
		return
	}
	if started {
		c.notifyWorker()
	}
}

// claimNext moves one pending effect to claimed and its run from queued to
// running in one transaction, recording the run.started event. A run that was
// already running is re-attached (recovery) instead of being started twice.
func (c *Core) claimNext(ctx context.Context) (effect, bool, error) {
	var (
		claimed effect
		ok      bool
	)
	err := c.write(ctx, func(t *txn) error {
		row := t.queryRow(`SELECT o.id, o.kind, o.thread_id, o.run_id, o.attempt_id, o.input,
				r.provider_id, r.model, r.workspace, r.approval_policy, r.interaction_mode, a.provider_cursor
			FROM effect_outbox o
			JOIN runs r ON r.id = o.run_id
			JOIN attempts a ON a.id = o.attempt_id
			WHERE o.status = ? AND o.kind != ? AND r.status IN (?, ?) AND r.active_attempt_id = o.attempt_id
			ORDER BY o.created_at, o.id
			LIMIT 1`, outboxPending, effectKindProviderControl, statusQueued, statusRunning)
		candidate := effect{}
		var workspace string
		if err := row.Scan(&candidate.id, &candidate.kind, &candidate.threadID,
			&candidate.runID, &candidate.attemptID, &candidate.input, &candidate.providerID,
			&candidate.model, &workspace, &candidate.approvalPolicy, &candidate.interactionMode, &candidate.afterSequence); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return wrapInternal("read pending effect", err)
		}
		candidate.workspace = decodeWorkspace(workspace)

		now := time.Now().UTC()
		candidate.token = newID("clm")
		res, err := t.exec(`UPDATE effect_outbox
			SET status = ?, claim_token = ?, attempt_count = attempt_count + 1, lease_until = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			outboxClaimed, candidate.token, now.Add(10*time.Minute).UnixNano(), now.UnixNano(),
			candidate.id, outboxPending)
		if err != nil {
			return wrapInternal("claim effect", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return wrapInternal("claim effect", err)
			}
			return errStaleAttempt
		}

		run, err := loadRun(t, candidate.runID)
		if err != nil {
			return wrapInternal("read run", err)
		}
		if !run.active() || run.ActiveAttemptID != candidate.attemptID {
			return errStaleAttempt
		}
		candidate.senderThreadID = run.SenderThreadID
		candidate.parentTaskID = run.ParentTaskID
		if run.SenderThreadID != "" {
			err := t.queryRow(`SELECT title FROM threads WHERE id = ?`, run.SenderThreadID).Scan(&candidate.senderTitle)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return wrapInternal("read run sender", err)
			}
		}
		candidate.attachments = run.Attachments
		started := run
		started.Status = statusRunning
		started.UpdatedAt = now

		if _, err := t.exec(`UPDATE runs SET status = ?, updated_at = ? WHERE id = ? AND active_attempt_id = ?`,
			statusRunning, now.UnixNano(), run.ID, candidate.attemptID); err != nil {
			return wrapInternal("start run", err)
		}
		if _, err := t.exec(`UPDATE attempts SET status = ?, updated_at = ? WHERE id = ?`,
			statusRunning, now.UnixNano(), candidate.attemptID); err != nil {
			return wrapInternal("start attempt", err)
		}
		if _, err := t.event(eventRunStarted, run.ThreadID, run.ID, candidate.attemptID,
			runEventData{Run: started.toAPI()}); err != nil {
			return err
		}
		claimed = candidate
		ok = true
		return nil
	})
	if errors.Is(err, errStaleAttempt) {
		return effect{}, false, nil
	}
	return claimed, ok, err
}

// executeEffect calls the provider outside any database transaction. The
// provider receives a context that Close and run.stop can cancel; emitting a
// delta is a conditional commit that fails once the attempt is no longer
// current.
func (c *Core) executeEffect(parent context.Context, eff effect) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if !c.watchRun(eff.runID, cancel) {
		// The core started draining between claim and execution. The run is left
		// active so the drain pass records it as interrupted.
		return
	}
	defer c.unwatchRun(eff.runID)

	// Re-read the durable state after registering cancellation: a run.stop that
	// committed between the claim and the registration is already durable, so the
	// provider must not be started at all.
	if !c.runIsActive(ctx, eff.runID, eff.attemptID) {
		return
	}

	request := provider.Request{
		ThreadID:        eff.threadID,
		SenderThreadID:  eff.senderThreadID,
		SenderTitle:     eff.senderTitle,
		ParentTaskID:    eff.parentTaskID,
		RunID:           eff.runID,
		AttemptID:       eff.attemptID,
		Message:         eff.input,
		Attachments:     eff.attachments,
		ProviderID:      eff.providerID,
		Model:           eff.model,
		Workspace:       eff.workspace,
		ApprovalPolicy:  eff.approvalPolicy,
		InteractionMode: eff.interactionMode,
		AfterSequence:   eff.afterSequence,
	}

	c.startRunTelemetry(eff)

	if durable, ok := c.provider.(provider.Durable); ok {
		recoverable := durable.Recoverable(request)
		err := durable.RunEvents(ctx, request, func(event provider.Event) error {
			return c.ingestProviderEvent(ctx, eff, event)
		})
		switch {
		case errors.Is(err, provider.ErrDetached):
			// The observer is gone; the independent worker still owns execution.
			// The run stays active so a later Core can attach again.
			return
		case ctx.Err() != nil && recoverable:
			// Core stopped observing (drain or restart). A recoverable provider
			// keeps its run and attempt; recovery resets the claim and re-attaches
			// to the same attempt instead of sending the prompt again.
			return
		}
		c.finalizeRunTelemetry(eff, err)
		c.settleWithRetry(parent, eff, err)
		c.clearLiveRunTelemetry(eff.runID, eff.attemptID)
		c.notifyWorker()
		return
	}

	emit := func(delta string) error {
		if delta == "" {
			return nil
		}
		return c.commitDelta(ctx, eff, delta)
	}
	providerErr := c.provider.Run(ctx, request, emit)
	c.finalizeRunTelemetry(eff, providerErr)
	c.settleWithRetry(parent, eff, providerErr)
	c.clearLiveRunTelemetry(eff.runID, eff.attemptID)
	c.notifyWorker()
}

// Retry only the terminal transaction while this Core owns the execution. The
// provider has already returned and must never be invoked again for this retry.
func (c *Core) settleWithRetry(ctx context.Context, eff effect, providerErr error) {
	delay := 20 * time.Millisecond
	for {
		if err := c.settleEffect(eff, providerErr); err == nil {
			return
		}
		if c.state.Load() >= stateDraining || ctx.Err() != nil {
			return // the shutdown/recovery pass resolves the durable claim
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, time.Second)
	}
}

// runIsActive re-reads the committed run state. It is the read half of the
// claim/cancel race window: cancellation is registered first, then the durable
// state is checked.
func (c *Core) runIsActive(ctx context.Context, runID, attemptID string) bool {
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return false
	}
	var (
		status string
		active string
	)
	err := c.store.db.QueryRowContext(ctx,
		`SELECT status, active_attempt_id FROM runs WHERE id = ?`, runID).Scan(&status, &active)
	if err != nil {
		return false
	}
	return status == statusRunning && active == attemptID
}

// commitDelta is the writeIfRunCurrent conditional commit for a non-durable
// provider: it only appends an output event when the run is still running, the
// attempt is still the active one and this worker still holds the outbox claim.
func (c *Core) commitDelta(ctx context.Context, eff effect, delta string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.write(ctx, func(t *txn) error {
		current, err := c.currentClaim(t, eff)
		if err != nil {
			return err
		}
		if !current {
			return errStaleAttempt
		}
		return appendRunOutput(t, eff, delta)
	})
}

// A transient SQLite failure does not mean the external execution failed.
// Retry ingestion of this same durable event without invoking the provider or
// advancing its cursor; cancellation and corrupt event sequences remain final.
func (c *Core) ingestProviderEvent(ctx context.Context, eff effect, event provider.Event) error {
	delay := 20 * time.Millisecond
	for {
		err := c.commitProviderEvent(ctx, eff, event)
		if err == nil || errors.Is(err, errStaleAttempt) || errors.Is(err, errProviderEventGap) {
			return err
		}
		if ctx.Err() != nil || c.state.Load() >= stateDraining {
			return provider.ErrDetached
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return provider.ErrDetached
		case <-timer.C:
		}
		delay = min(delay*2, time.Second)
	}
}

// commitProviderEvent is the durable-provider equivalent of commitDelta. The
// run/attempt/claim check, the provider cursor and the appended output commit in
// one transaction: a repeated sequence is never appended twice and a gap is
// refused instead of silently splicing the output.
func (c *Core) commitProviderEvent(ctx context.Context, eff effect, event provider.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.write(ctx, func(t *txn) error {
		current, err := c.currentClaim(t, eff)
		if err != nil {
			return err
		}
		if !current {
			return errStaleAttempt
		}
		cursor, err := attemptCursor(t, eff.attemptID)
		if err != nil {
			return err
		}
		if event.Sequence <= cursor {
			return nil // already committed: duplicate delivery appends nothing
		}
		if event.Sequence != cursor+1 {
			return fmt.Errorf("%w for attempt %s: got sequence %d after %d",
				errProviderEventGap, eff.attemptID, event.Sequence, cursor)
		}
		now := time.Now().UTC()
		if _, err := t.exec(`UPDATE attempts SET provider_cursor = ?, updated_at = ? WHERE id = ?`,
			event.Sequence, now.UnixNano(), eff.attemptID); err != nil {
			return wrapInternal("advance provider cursor", err)
		}
		if err := c.applyProviderProjection(t, eff, event); err != nil {
			return err
		}
		return c.observeProviderTelemetry(t, eff, event)
	})
}

func (c *Core) applyProviderProjection(t *txn, eff effect, event provider.Event) error {
	switch event.Type {
	case "remote.tool.updated", "remote.request.updated", "remote.input.updated":
		return c.applyRemoteEvent(t, eff, event)
	case "tool_execution_start", "tool_execution_update", "tool_execution_end",
		"extension_ui_request", "control_result":
		run, err := loadRun(t, eff.runID)
		if err != nil {
			return wrapInternal("read run", err)
		}
		switch event.Type {
		case "extension_ui_request":
			return c.applyUIRequest(t, run, eff.attemptID, event)
		case "control_result":
			return c.applyControlResult(t, run, eff.attemptID, event)
		}
		return c.applyToolEvent(t, run, eff.attemptID, event)
	}
	if event.Type == "thinking_delta" {
		return appendRunReasoning(t, eff, event)
	}
	if event.Delta == "" {
		return nil
	}
	return appendRunOutput(t, eff, event.Delta)
}

func attemptCursor(t *txn, attemptID string) (int64, error) {
	var cursor int64
	if err := t.queryRow(`SELECT provider_cursor FROM attempts WHERE id = ?`, attemptID).Scan(&cursor); err != nil {
		return 0, wrapInternal("read provider cursor", err)
	}
	return cursor, nil
}

// currentClaim re-checks outbox ownership and run currency inside a
// transaction. It is the guard for every provider-driven write.
func (c *Core) currentClaim(t *txn, eff effect) (bool, error) {
	var (
		status string
		token  string
	)
	err := t.queryRow(`SELECT status, claim_token FROM effect_outbox WHERE id = ?`, eff.id).Scan(&status, &token)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrapInternal("read effect", err)
	}
	if status != outboxClaimed || token != eff.token {
		return false, nil
	}
	run, err := loadRun(t, eff.runID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrapInternal("read run", err)
	}
	if run.Status != statusRunning || run.ActiveAttemptID != eff.attemptID {
		return false, nil
	}
	return true, nil
}

// settleEffect commits the terminal run state and the outbox settlement in one
// transaction. A parent whose children are still unconsumed is parked in
// waiting_children instead of completing; an accepted generation is continued
// with a new attempt once the previous attempt has settled. A terminal run is
// never resurrected.
func (c *Core) settleEffect(eff effect, providerErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()

	// Failure preserves the run and claim together for a terminal-write retry.
	// A crash before that retry is still resolved by the next recovery barrier.
	return c.write(ctx, func(t *txn) error {
		// During drain the recovery barrier owns terminalization. Settling here
		// would also start the thread's next queued message or a parent
		// continuation in a Core that is already stopping; recovery would then
		// mistake that never-submitted work for interrupted work.
		if c.state.Load() >= stateDraining {
			return nil
		}
		var (
			status string
			token  string
		)
		err := t.queryRow(`SELECT status, claim_token FROM effect_outbox WHERE id = ?`, eff.id).Scan(&status, &token)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return wrapInternal("read effect", err)
		}
		if status != outboxClaimed || token != eff.token {
			return nil
		}

		run, err := loadRun(t, eff.runID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return wrapInternal("read run", err)
		}
		now := time.Now().UTC()
		if !run.active() || run.ActiveAttemptID != eff.attemptID {
			if _, err := t.exec(`UPDATE effect_outbox SET status = ?, last_error = ?, updated_at = ?
				WHERE id = ? AND claim_token = ?`,
				outboxSettled, "run was already settled", now.UnixNano(), eff.id, eff.token); err != nil {
				return wrapInternal("settle stale effect", err)
			}
			return c.reconcileTaskForRun(t, run.ID)
		}

		settled := run
		settled.ActiveAttemptID = ""
		settled.UpdatedAt = now
		eventType := eventRunCompleted
		attemptStatus := statusCompleted
		switch {
		case providerErr == nil:
			settled.Status = statusCompleted
			open, err := c.hasOpenDeliveries(t, run.ID)
			if err != nil {
				return err
			}
			if open {
				// The provider returned but its children are still unconsumed: the
				// run parks in waiting_children and keeps the thread's single slot.
				settled.Status = statusWaitingChildren
				eventType = eventRunWaiting
			}
		case errors.Is(providerErr, provider.ErrExecutionCancelled):
			settled.Status, eventType, attemptStatus = statusCancelled, eventRunCancelled, statusCancelled
		case errors.Is(providerErr, context.Canceled), errors.Is(providerErr, context.DeadlineExceeded):
			// The attempt context was cancelled (drain or shutdown), not a provider
			// fault: record it as interrupted, without an error message.
			settled.Status = statusInterrupted
			eventType = eventRunInterrupted
			attemptStatus = statusInterrupted
		default:
			settled.Status = statusFailed
			settled.Error = providerErr.Error()
			eventType = eventRunFailed
			attemptStatus = statusFailed
		}

		res, err := t.exec(`UPDATE runs SET status = ?, active_attempt_id = '', error = ?, updated_at = ?
			WHERE id = ? AND active_attempt_id = ?`,
			settled.Status, settled.Error, now.UnixNano(), run.ID, eff.attemptID)
		if err != nil {
			return wrapInternal("settle run", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return wrapInternal("settle run", err)
		} else if n != 1 {
			// The attempt moved on between the read and the write: commit nothing.
			return errStaleAttempt
		}
		if _, err := t.exec(`UPDATE attempts SET status = ?, updated_at = ? WHERE id = ?`,
			attemptStatus, now.UnixNano(), eff.attemptID); err != nil {
			return wrapInternal("settle attempt", err)
		}
		if _, err := t.exec(`UPDATE effect_outbox SET status = ?, updated_at = ?, last_error = ?
			WHERE id = ? AND claim_token = ?`,
			outboxSettled, now.UnixNano(), settled.Error, eff.id, eff.token); err != nil {
			return wrapInternal("settle effect", err)
		}

		// Everything this attempt left behind is resolved with the terminal state:
		// running tools are interrupted, open dialogs expire, pending steers fail.
		if err := c.cleanupAttemptWork(t, eff.attemptID, "attempt ended before the work completed"); err != nil {
			return err
		}

		if settled.Status == statusWaitingChildren || settled.Status == statusCompleted {
			// A parent that already accepted results continues right away; this is
			// the only place a continuation attempt is created, so the parent
			// provider is always settled first.
			started, err := c.startContinuation(t, run.ID)
			if err != nil {
				return err
			}
			if started {
				return c.reconcileTaskForRun(t, run.ID)
			}
		}
		settledAPI := settled.toAPI()
		settledAPI.Telemetry = c.runTelemetrySnapshot(run.ID)
		if _, err := t.event(eventType, run.ThreadID, run.ID, eff.attemptID,
			runEventData{Run: settledAPI}); err != nil {
			return err
		}
		if err := c.reconcileTaskForRun(t, run.ID); err != nil {
			return err
		}
		if api.RunStatus(settled.Status).Terminal() {
			if _, err := c.reconcileThreadWatches(t, settled); err != nil {
				return err
			}
		}
		// The thread slot has settled: only now may the next queued message run.
		// A parent continuation created above already owns the slot, so a queued
		// message never overtakes it.
		_, err = c.startQueuedInput(t, run.ThreadID)
		return err
	})
}

// appendRunOutput appends inside SQLite instead of reading and rewriting the
// whole reply in Go for each delta. The run's thread is the only other field
// the event needs.
func appendRunOutput(t *txn, eff effect, delta string) error {
	var threadID string
	if err := t.queryRow(`SELECT thread_id FROM runs WHERE id = ?`, eff.runID).Scan(&threadID); err != nil {
		return wrapInternal("read run", err)
	}
	if _, err := t.exec(`UPDATE runs SET output = output || ?, updated_at = ? WHERE id = ?`,
		delta, time.Now().UTC().UnixNano(), eff.runID); err != nil {
		return wrapInternal("append output", err)
	}
	_, err := t.event(eventRunOutput, threadID, eff.runID, eff.attemptID, outputEventData{Delta: delta})
	return err
}

// appendRunReasoning keeps the reasoning a model streams beside its reply,
// never inside it, so a client can fold it as a step of the turn.
func appendRunReasoning(t *txn, eff effect, event provider.Event) error {
	var record struct {
		AssistantMessageEvent struct {
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
	}
	if json.Unmarshal(event.Data, &record) != nil || record.AssistantMessageEvent.Delta == "" {
		return nil
	}
	delta := record.AssistantMessageEvent.Delta
	var threadID string
	if err := t.queryRow(`SELECT thread_id FROM runs WHERE id = ?`, eff.runID).Scan(&threadID); err != nil {
		return wrapInternal("read run", err)
	}
	if _, err := t.exec(`UPDATE runs SET reasoning = reasoning || ?, updated_at = ? WHERE id = ?`,
		delta, time.Now().UTC().UnixNano(), eff.runID); err != nil {
		return wrapInternal("append reasoning", err)
	}
	_, err := t.event(eventRunReasoning, threadID, eff.runID, eff.attemptID, struct {
		ReasoningDelta string `json:"reasoningDelta"`
	}{delta})
	return err
}
