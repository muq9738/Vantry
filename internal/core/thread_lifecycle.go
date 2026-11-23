package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func (c *Core) dispatchDeleteThread(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", cmd.Kind)
	}
	var receipt api.Receipt
	var stopped []cancelledRun
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			receipt = prior
			return nil
		}
		thread, err := loadThread(t, cmd.ThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "thread %q not found", cmd.ThreadID)
		}
		if err != nil {
			return wrapInternal("read deleted thread", err)
		}
		if !cmd.Force {
			if active, err := activeRunID(t, thread.ID); err != nil {
				return err
			} else if active != "" {
				return errorf(api.Conflict, "thread %q has active run %q", thread.ID, active)
			}
			var unsettled int
			if err := t.queryRow(`SELECT COUNT(1) FROM tasks task
			WHERE (task.parent_thread_id = ? OR task.child_thread_id = ?) AND (
				task.status NOT IN (?, ?, ?, ?) OR task.delivery_status NOT IN (?, ?)
				OR EXISTS (SELECT 1 FROM runs child WHERE child.id = task.child_run_id AND child.status IN (?, ?, ?))
				OR (task.delivery_status = ? AND EXISTS (SELECT 1 FROM cohorts cohort
					WHERE cohort.parent_run_id = task.parent_run_id AND cohort.generation = task.generation
					AND cohort.status = ? AND cohort.continuation_attempt_id = ''))
			)`, thread.ID, thread.ID,
				statusCompleted, statusCancelled, statusInterrupted, statusFailed,
				deliveryAcknowledged, deliveryDisposed, statusQueued, statusRunning, statusWaitingChildren,
				deliveryAcknowledged, deliveryAcknowledged).Scan(&unsettled); err != nil {
				return wrapInternal("read unsettled thread tasks", err)
			}
			if unsettled != 0 {
				return errorf(api.Conflict, "thread %q has %d unsettled task(s)", thread.ID, unsettled)
			}
		} else if err := c.prepareForceDelete(t, thread.ID, &stopped); err != nil {
			return err
		}
		// Old branch databases have real foreign keys on the legacy tables. All
		// dependants must disappear before attempts, runs and finally the thread.
		// Receipts and events remain so retries and subscription replay keep their
		// committed business identities even after the projection is deleted.
		for _, statement := range []string{
			`DELETE FROM inputs WHERE thread_id = ?`,
			`DELETE FROM queued_messages WHERE thread_id = ?`,
			`DELETE FROM steering_messages WHERE thread_id = ?`,
			`DELETE FROM tool_calls WHERE thread_id = ?`,
			`DELETE FROM provider_requests WHERE thread_id = ?`,
			`DELETE FROM effect_outbox WHERE thread_id = ? AND (kind <> 'provider.stop' OR status <> 'pending')`,
			`DELETE FROM cohorts WHERE parent_run_id IN (SELECT id FROM runs WHERE thread_id = ?)`,
			`DELETE FROM tasks WHERE parent_thread_id = ? OR child_thread_id = ?`,
			`DELETE FROM thread_watches WHERE watcher_thread_id = ? OR target_thread_id = ?`,
			`DELETE FROM attempts WHERE run_id IN (SELECT id FROM runs WHERE thread_id = ?)`,
			`DELETE FROM runs WHERE thread_id = ?`,
			`DELETE FROM threads WHERE id = ?`,
		} {
			args := []any{thread.ID}
			if strings.HasPrefix(statement, "DELETE FROM tasks ") || strings.HasPrefix(statement, "DELETE FROM thread_watches ") {
				args = append(args, thread.ID)
			}
			if _, err := t.exec(statement, args...); err != nil {
				return wrapInternal("delete thread projections", err)
			}
		}
		ev, err := t.event(eventThreadDeleted, thread.ID, "", "", threadEventData{Thread: thread.toAPI()})
		if err != nil {
			return err
		}
		receipt = api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: thread.ID, Sequence: ev.Sequence}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	c.deliverStops(stopped)
	c.notifyWorker()
	return receipt, nil
}

func (c *Core) dispatchForkThread(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", cmd.Kind)
	}
	var worktreeSpec *api.WorkspaceSpec
	if cmd.Workspace != nil && cmd.Workspace.Kind == "worktree" {
		repoHint := firstNonEmpty(cmd.Workspace.Repository, cmd.Workspace.Path)
		var alreadyRecorded bool
		if err := c.orchestrationRead(ctx, func(t *txn) error {
			if _, found, err := lookupReceipt(t, cmd); err != nil {
				return err
			} else if found {
				alreadyRecorded = true
				return nil
			}
			src, err := loadThread(t, cmd.ThreadID)
			if errors.Is(err, sql.ErrNoRows) {
				return errorf(api.NotFound, "thread %q not found", cmd.ThreadID)
			}
			if err != nil {
				return wrapInternal("read fork source thread", err)
			}
			if repoHint == "" && src.Workspace != nil {
				repoHint = firstNonEmpty(src.Workspace.Repository, src.Workspace.Path)
			}
			return nil
		}); err != nil {
			return api.Receipt{}, err
		}
		if !alreadyRecorded {
			if repoHint == "" {
				return api.Receipt{}, errorf(api.InvalidCommand, "worktree fork requires a git workspace")
			}
			root, err := exec.CommandContext(ctx, "git", "-C", repoHint, "rev-parse", "--show-toplevel").Output()
			if err != nil {
				return api.Receipt{}, errorf(api.InvalidCommand, "worktree fork requires a valid git repository")
			}
			repository := strings.TrimSpace(string(root))
			if err := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "--verify", "HEAD").Run(); err != nil {
				return api.Receipt{}, errorf(api.InvalidCommand, "worktree fork requires a git repository with a commit")
			}
			worktreeSpec = &api.WorkspaceSpec{
				Kind:       "worktree",
				Repository: repository,
				Branch:     cmd.Workspace.Branch,
				BaseRef:    firstNonEmpty(cmd.Workspace.BaseRef, "HEAD"),
			}
		}
	}
	var (
		receipt      api.Receipt
		forkSourceID string
		forkTargetID string
		copiedRuns   int
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			receipt = prior
			return nil
		}
		source, err := loadThread(t, cmd.ThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "thread %q not found", cmd.ThreadID)
		}
		if err != nil {
			return wrapInternal("read fork source thread", err)
		}
		cutoff, found, err := forkSourceRun(t, source.ID, firstNonEmpty(cmd.SourceRunID, cmd.RunID))
		if err != nil {
			return err
		}
		model := source.Model
		providerID := source.ProviderID
		if strings.TrimSpace(cmd.Model) != "" || strings.TrimSpace(cmd.ProviderID) != "" {
			model = firstNonEmpty(strings.TrimSpace(cmd.Model), source.Model)
			resolvedProvider, provErr := c.switchedProvider(source, model, cmd.ProviderID)
			if provErr != nil {
				return provErr
			}
			providerID = resolvedProvider
		}
		workspace := firstWorkspace(worktreeSpec, cmd.Workspace, source.Workspace)
		thread := api.Thread{
			ID: newID("th"), Title: firstNonEmpty(cmd.Title, source.Title), CreatedAt: time.Now().UTC(),
			ProviderID: providerID, Model: model, Workspace: workspace,
			ApprovalPolicy: source.ApprovalPolicy, InteractionMode: firstNonEmpty(source.InteractionMode, interactionDefault),
			ForkedFromThreadID: source.ID,
		}
		if found {
			thread.ForkedFromRunID = cutoff.ID
		}
		if err := insertThread(t, thread); err != nil {
			return err
		}
		if _, err := t.event(eventThreadCreated, thread.ID, "", "", threadEventData{Thread: thread}); err != nil {
			return err
		}
		if found {
			n, err := copyForkHistory(t, source.ID, cutoff, thread.ID)
			if err != nil {
				return err
			}
			forkSourceID, forkTargetID, copiedRuns = source.ID, thread.ID, n
		}
		receipt = api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: thread.ID}
		receipt.Sequence, _ = lastEventSequence(t)
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if c.forkSession != nil && forkTargetID != "" && copiedRuns > 0 {
		_ = c.forkSession(forkSourceID, forkTargetID, copiedRuns)
	}
	return receipt, nil
}

func forkSourceRun(t *txn, threadID, runID string) (runRecord, bool, error) {
	if runID != "" {
		run, err := loadRun(t, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return runRecord{}, false, errorf(api.NotFound, "source run %q not found", runID)
		}
		if err != nil {
			return runRecord{}, false, wrapInternal("read fork source run", err)
		}
		if run.ThreadID != threadID {
			return runRecord{}, false, errorf(api.NotFound, "source run %q does not belong to thread %q", runID, threadID)
		}
		if !api.RunStatus(run.Status).Terminal() {
			return runRecord{}, false, errorf(api.Conflict, "source run %q has not settled", runID)
		}
		return run, true, nil
	}
	run, err := scanRun(t.queryRow(`SELECT `+runColumns+` FROM runs
		WHERE thread_id = ? AND status IN (?, ?, ?, ?) ORDER BY created_at DESC, id DESC LIMIT 1`,
		threadID, statusCompleted, statusCancelled, statusInterrupted, statusFailed))
	if errors.Is(err, sql.ErrNoRows) {
		return runRecord{}, false, nil
	}
	if err != nil {
		return runRecord{}, false, wrapInternal("read latest settled source run", err)
	}
	return run, true, nil
}

func copyForkHistory(t *txn, sourceID string, cutoff runRecord, targetID string) (int, error) {
	rows, err := t.query(`SELECT `+runColumns+` FROM runs WHERE thread_id = ? AND status = ?
		AND (created_at < ? OR (created_at = ? AND id <= ?)) ORDER BY created_at, id`,
		sourceID, statusCompleted, cutoff.CreatedAt.UnixNano(), cutoff.CreatedAt.UnixNano(), cutoff.ID)
	if err != nil {
		return 0, wrapInternal("read fork history", err)
	}
	var history []runRecord
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return 0, wrapInternal("scan fork history", err)
		}
		history = append(history, run)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, wrapInternal("read fork history", err)
	}
	rows.Close()
	for _, source := range history {
		// The new thread namespaces the source run's business identity. Keeping
		// the source ID as the suffix also preserves timeline order when source
		// runs have identical creation times.
		runID, attemptID := "run_"+targetID+":"+source.ID, "att_"+targetID+":"+source.ID
		// Copy only settled history, with identities specific to this fork and
		// no execution outbox. It can be displayed immediately but never replayed.
		if _, err := t.exec(`INSERT INTO runs
			(id, thread_id, attempt_id, active_attempt_id, status, input, output, reasoning, error,
			 created_at, updated_at, provider_id, model, workspace, parent_task_id, approval_policy, attachments, interaction_mode, sender_thread_id)
			SELECT ?, ?, ?, '', status, input, output, reasoning, error,
				created_at, updated_at, provider_id, model, workspace, '', approval_policy, attachments, interaction_mode, sender_thread_id
			FROM runs WHERE id = ?`, runID, targetID, attemptID, source.ID); err != nil {
			return 0, wrapInternal("copy fork run", err)
		}
		if _, err := t.exec(`INSERT INTO attempts (id, run_id, ordinal, status, input, created_at, updated_at)
			VALUES (?, ?, 1, ?, ?, ?, ?)`, attemptID, runID, statusCompleted, source.Input,
			source.CreatedAt.UnixNano(), source.UpdatedAt.UnixNano()); err != nil {
			return 0, wrapInternal("copy fork attempt", err)
		}
		source.ID, source.ThreadID, source.AttemptID, source.ActiveAttemptID, source.ParentTaskID = runID, targetID, attemptID, "", ""
		if _, err := t.event(eventRunCompleted, targetID, runID, attemptID, runEventData{Run: source.toAPI()}); err != nil {
			return 0, err
		}
	}
	return len(history), nil
}

func (c *Core) dispatchMergeBackThread(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", cmd.Kind)
	}
	var receipt api.Receipt
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			receipt = prior
			return nil
		}
		source, err := loadThread(t, cmd.ThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "thread %q not found", cmd.ThreadID)
		}
		if err != nil {
			return wrapInternal("read merge-back source", err)
		}
		targetID := firstNonEmpty(cmd.TargetThreadID, source.ForkedFromThreadID, source.ParentThreadID)
		if strings.TrimSpace(targetID) == "" || targetID == source.ID {
			return errorf(api.InvalidCommand, "merge-back requires a different target thread")
		}
		target, err := loadThread(t, targetID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "target thread %q not found", targetID)
		}
		if err != nil {
			return wrapInternal("read merge-back target", err)
		}
		run, err := scanRun(t.queryRow(`SELECT `+runColumns+` FROM runs WHERE thread_id = ? AND status = ?
			ORDER BY created_at DESC, id DESC LIMIT 1`, source.ID, statusCompleted))
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.Conflict, "thread %q has no completed run to merge back", source.ID)
		}
		if err != nil {
			return wrapInternal("read merge-back result", err)
		}
		delivery := cmd
		delivery.Message = fmt.Sprintf("Result from thread %s (%s), run %s:\n\n%s",
			source.Title, source.ID, run.ID, firstNonEmpty(run.Output, "(no output)"))
		receipt, err = c.acceptQueuedMessage(t, target, delivery)
		if err != nil {
			return err
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	c.notifyWorker()
	return receipt, nil
}

type threadRewoundEventData struct {
	ThreadID      string   `json:"threadId"`
	RunID         string   `json:"runId"`
	RemovedRunIDs []string `json:"removedRunIds"`
}

// dispatchRewindThread removes targetRun and every subsequent run of the
// thread from Core's projections, emits thread.rewound, and invokes
// RewindSession so the provider session matches the remaining completed turns.
func (c *Core) dispatchRewindThread(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	threadID := strings.TrimSpace(cmd.ThreadID)
	if threadID == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", cmd.Kind)
	}
	targetRunID := strings.TrimSpace(firstNonEmpty(cmd.RunID, cmd.SourceRunID))
	if targetRunID == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "runId is required for %s", cmd.Kind)
	}

	var (
		receipt       api.Receipt
		removedRunIDs []string
		keepCompleted int
		shouldTruncate bool
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			receipt = prior
			return nil
		}
		thread, err := loadThread(t, threadID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "thread %q not found", threadID)
		}
		if err != nil {
			return wrapInternal("read rewind thread", err)
		}
		if active, err := activeRunID(t, thread.ID); err != nil {
			return err
		} else if active != "" {
			return errorf(api.Conflict, "thread %q has active run %q", thread.ID, active)
		}

		targetRun, err := loadRun(t, targetRunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "run %q not found", targetRunID)
		}
		if err != nil {
			return wrapInternal("read rewind run", err)
		}
		if targetRun.ThreadID != thread.ID {
			return errorf(api.NotFound, "run %q does not belong to thread %q", targetRunID, thread.ID)
		}
		if !api.RunStatus(targetRun.Status).Terminal() {
			return errorf(api.Conflict, "run %q has not settled", targetRunID)
		}

		cutoff := targetRun.CreatedAt.UnixNano()
		rows, err := t.query(`SELECT id FROM runs WHERE thread_id = ?
			AND (created_at > ? OR (created_at = ? AND id >= ?))
			ORDER BY created_at, id`, thread.ID, cutoff, cutoff, targetRun.ID)
		if err != nil {
			return wrapInternal("list runs to rewind", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return wrapInternal("scan rewind run", err)
			}
			removedRunIDs = append(removedRunIDs, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return wrapInternal("read runs to rewind", err)
		}
		rows.Close()

		for _, runID := range removedRunIDs {
			for _, stmt := range []string{
				`DELETE FROM inputs WHERE run_id = ?`,
				`DELETE FROM queued_messages WHERE run_id = ?`,
				`DELETE FROM steering_messages WHERE run_id = ?`,
				`DELETE FROM tool_calls WHERE run_id = ?`,
				`DELETE FROM provider_requests WHERE run_id = ?`,
				`DELETE FROM project_updates WHERE run_id = ?`,
				`DELETE FROM effect_outbox WHERE run_id = ? AND (kind <> 'provider.stop' OR status <> 'pending')`,
				`DELETE FROM cohorts WHERE parent_run_id = ?`,
				`DELETE FROM tasks WHERE parent_run_id = ?`,
				`DELETE FROM attempts WHERE run_id = ?`,
				`DELETE FROM runs WHERE id = ?`,
			} {
				if _, err := t.exec(stmt, runID); err != nil {
					return wrapInternal("delete rewound run data", err)
				}
			}
		}

		if err := t.queryRow(`SELECT COUNT(1) FROM runs WHERE thread_id = ? AND status = ?`,
			thread.ID, statusCompleted).Scan(&keepCompleted); err != nil {
			return wrapInternal("count remaining completed runs", err)
		}

		ev, err := t.event(eventThreadRewound, thread.ID, targetRun.ID, "", threadRewoundEventData{
			ThreadID:      thread.ID,
			RunID:         targetRun.ID,
			RemovedRunIDs: append([]string(nil), removedRunIDs...),
		})
		if err != nil {
			return err
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  thread.ID,
			RunID:     targetRun.ID,
			Sequence:  ev.Sequence,
		}
		shouldTruncate = true
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if shouldTruncate {
		for _, runID := range removedRunIDs {
			c.clearLiveRunTelemetryByID(runID)
		}
		if c.rewindSession != nil {
			_ = c.rewindSession(threadID, keepCompleted)
		}
	}
	return receipt, nil
}
