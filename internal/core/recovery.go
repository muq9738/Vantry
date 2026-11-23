package core

import (
	"context"
	"database/sql"
	"errors"

	"github.com/muq9738/Vantry/pkg/provider"
)

// recover runs inside the Open barrier, before the core reports ready.
//
// A run whose provider supports durable recovery keeps its Run and Attempt: only
// its outbox claim is reset so the same attempt is re-attached (never re-sent).
// Every other active run is marked interrupted without re-invoking the provider.
// Afterwards the barrier reconciles undelivered results and pending
// continuations so a crash window cannot leave a result stranded.
func (c *Core) recover(ctx context.Context) error {
	var started bool
	err := c.write(ctx, func(t *txn) error {
		if err := c.resolveActiveWork(t, "interrupted: core restarted before the run finished"); err != nil {
			return err
		}
		var err error
		started, err = c.reconcileAll(t)
		return err
	})
	if err != nil {
		return err
	}
	if started {
		c.notifyWorker()
	}
	c.notifyWorker()
	return nil
}

// interruptActiveRuns is the graceful-close equivalent of recover. Both paths
// converge on the same durable state, so a crash and a clean stop are resolved
// the same way on the next start.
func (c *Core) interruptActiveRuns(ctx context.Context) error {
	return c.write(ctx, func(t *txn) error {
		return c.resolveActiveWork(t, "interrupted: core stopped before the run finished")
	})
}

// keptRun is one active run whose provider can recover it; its attempt stays
// active and its effect claim is released for re-attachment.
type keptRun struct {
	runID     string
	attemptID string
}

func (c *Core) resolveActiveWork(t *txn, reason string) error {
	now := nowNanos()

	rows, err := t.query(`SELECT `+runColumns+` FROM runs WHERE status IN (?, ?) ORDER BY created_at, id`,
		statusQueued, statusRunning)
	if err != nil {
		return wrapInternal("read active runs", err)
	}
	var active []runRecord
	for rows.Next() {
		rec, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return wrapInternal("scan active run", err)
		}
		active = append(active, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return wrapInternal("read active runs", err)
	}
	rows.Close()

	kept := make(map[string]keptRun, len(active))
	for _, run := range active {
		attemptInput := run.Input
		if run.ActiveAttemptID != "" {
			if err := t.queryRow(`SELECT input FROM attempts WHERE id = ?`, run.ActiveAttemptID).
				Scan(&attemptInput); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return wrapInternal("read attempt input", err)
			}
		}
		if c.recoverable(run, attemptInput) {
			// Only the claim is reset; the run and attempt stay active so the next
			// claim re-attaches to the same attempt with its committed cursor.
			kept[run.ID] = keptRun{runID: run.ID, attemptID: run.ActiveAttemptID}
			continue
		}
		interrupted := run
		interrupted.Status = statusInterrupted
		interrupted.ActiveAttemptID = ""
		interrupted.Error = reason
		interrupted.UpdatedAt = fromNanos(now)
		if _, err := t.exec(`UPDATE runs SET status = ?, active_attempt_id = '', error = ?, updated_at = ?
			WHERE id = ? AND status IN (?, ?)`,
			statusInterrupted, reason, now, run.ID, statusQueued, statusRunning); err != nil {
			return wrapInternal("interrupt run", err)
		}
		if _, err := t.exec(`UPDATE attempts SET status = ?, updated_at = ?
			WHERE run_id = ? AND status IN (?, ?)`,
			statusInterrupted, now, run.ID, statusQueued, statusRunning); err != nil {
			return wrapInternal("interrupt attempt", err)
		}
		if _, err := t.event(eventRunInterrupted, run.ThreadID, run.ID, run.AttemptID,
			runEventData{Run: interrupted.toAPI()}); err != nil {
			return err
		}
		// An interrupted attempt cannot deliver anything more: its tools, dialogs
		// and steers are resolved together with the terminal run state.
		if err := c.cleanupAttemptWork(t, run.ActiveAttemptID, "attempt was interrupted"); err != nil {
			return err
		}
	}

	return c.resolveEffects(t, kept, reason, now)
}

// resolveEffects releases claims of recoverable attempts for re-attachment and
// abandons every other unsettled effect.
func (c *Core) resolveEffects(t *txn, kept map[string]keptRun, reason string, now int64) error {
	// Control intents are not provider executions: a crash may hand the same
	// immutable control over again, so they stay pending across recovery and are
	// cancelled only when their attempt settles.
	rows, err := t.query(`SELECT id, run_id, attempt_id, status FROM effect_outbox
		WHERE status IN (?, ?) AND kind NOT IN (?, ?) ORDER BY created_at, id`,
		outboxPending, outboxClaimed, effectKindProviderStop, effectKindProviderControl)
	if err != nil {
		return wrapInternal("read unsettled effects", err)
	}
	type effectRow struct {
		id        string
		runID     string
		attemptID string
		status    string
	}
	var effects []effectRow
	for rows.Next() {
		var rec effectRow
		if err := rows.Scan(&rec.id, &rec.runID, &rec.attemptID, &rec.status); err != nil {
			rows.Close()
			return wrapInternal("scan unsettled effect", err)
		}
		effects = append(effects, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return wrapInternal("read unsettled effects", err)
	}
	rows.Close()

	for _, eff := range effects {
		if keep, ok := kept[eff.runID]; ok && keep.attemptID == eff.attemptID {
			if _, err := t.exec(`UPDATE effect_outbox SET status = ?, claim_token = '', lease_until = 0,
				last_error = '', updated_at = ? WHERE id = ?`,
				outboxPending, now, eff.id); err != nil {
				return wrapInternal("release effect claim", err)
			}
			continue
		}
		if _, err := t.exec(`UPDATE effect_outbox SET status = ?, last_error = ?, updated_at = ?
			WHERE id = ?`, outboxAbandoned, reason, now, eff.id); err != nil {
			return wrapInternal("abandon effect", err)
		}
	}
	return nil
}

// recoverable asks the provider whether it can re-attach to this run's attempt.
// Only a durable provider may answer yes; nothing external is executed here.
func (c *Core) recoverable(run runRecord, attemptInput string) bool {
	durable, ok := c.provider.(provider.Durable)
	if !ok || run.ActiveAttemptID == "" {
		return false
	}
	return durable.Recoverable(provider.Request{
		ThreadID:        run.ThreadID,
		SenderThreadID:  run.SenderThreadID,
		RunID:           run.ID,
		AttemptID:       run.ActiveAttemptID,
		Message:         attemptInput,
		Attachments:     run.Attachments,
		ProviderID:      run.ProviderID,
		Model:           run.Model,
		Workspace:       run.Workspace,
		ApprovalPolicy:  run.ApprovalPolicy,
		InteractionMode: run.InteractionMode,
	})
}
