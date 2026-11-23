package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func (c *Core) subscribeCurrent(ctx context.Context) (*api.Subscription, error) {
	var sequence int64
	if err := c.orchestrationRead(ctx, func(t *txn) error {
		var err error
		sequence, err = projectionSequence(t)
		return err
	}); err != nil {
		return nil, err
	}
	return c.Subscribe(ctx, sequence)
}

// Register before reading the condition: both a completion already committed
// and one racing the initial read must wake the caller without a polling loop.
func (c *Core) waitOrchestration(ctx context.Context, timeoutMs int, check func(context.Context) (bool, error)) (bool, error) {
	if timeoutMs < 0 {
		return false, errorf(api.InvalidCommand, "timeoutMs must not be negative")
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(boundedLimit(timeoutMs, 120_000, 600_000))*time.Millisecond)
	defer cancel()
	finish := func() (bool, error) {
		// A deadline can interrupt the very first SQL read. Return a populated
		// final state even in that window, using one bounded read at the deadline.
		finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer finalCancel()
		done, err := check(finalCtx)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err != nil {
			return false, err
		}
		return !done, nil
	}
	sub, err := c.subscribeCurrent(waitCtx)
	if err != nil {
		if waitCtx.Err() != nil {
			return finish()
		}
		return false, err
	}
	defer func() {
		if sub != nil {
			sub.Close()
		}
	}()
	for {
		if done, err := check(waitCtx); err != nil {
			if waitCtx.Err() == nil {
				return false, err
			}
		} else if done {
			return false, nil
		}
		if waitCtx.Err() != nil {
			return finish()
		}
		select {
		case <-waitCtx.Done():
			return finish()
		case _, ok := <-sub.Events:
			if !ok {
				if waitCtx.Err() != nil {
					return finish()
				}
				err := <-sub.Errors
				if api.CodeOf(err) == api.ResyncRequired {
					sub.Close()
					sub, err = c.subscribeCurrent(waitCtx)
					if err == nil {
						continue
					}
				}
				if err == nil {
					err = errorf(api.NotReady, "core subscription closed")
				}
				return false, err
			}
		}
	}
}

func (c *Core) awaitDelegatedTask(ctx context.Context, taskID string, timeoutMs int) (api.TaskDetail, error) {
	var detail api.TaskDetail
	timedOut, err := c.waitOrchestration(ctx, timeoutMs, func(waitCtx context.Context) (bool, error) {
		latest, err := c.taskDetail(waitCtx, taskID)
		if err != nil {
			return false, err
		}
		detail = latest
		return detail.Status.Terminal() || detail.HasPendingQuestion || detail.PendingApproval != nil, nil
	})
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer finalCancel()
	if detail.TaskID == "" {
		if latest, readErr := c.taskDetail(finalCtx, taskID); readErr == nil {
			detail = latest
		}
	}
	if !detail.Status.Terminal() {
		// The wait's caller may already have cancelled. The accepted assignment
		// still needs its durable wake policy upgraded in that case.
		upgradeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		_, upgradeErr := c.Dispatch(upgradeCtx, api.Command{Version: api.ProtocolVersion,
			Kind: api.UpgradeTaskWake, CommandID: newID("wake"), TaskID: taskID})
		cancel()
		if upgradeErr != nil {
			return detail, upgradeErr
		}
	} else if observeErr := c.observeTaskResults(finalCtx, []api.TaskDetail{detail}); observeErr != nil {
		return detail, observeErr
	} else if latest, readErr := c.taskDetail(finalCtx, taskID); readErr == nil {
		detail = latest
	}
	detail.WaitTimedOut = timedOut
	return detail, err
}

func (c *Core) WaitThread(ctx context.Context, query api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
	result := api.ThreadWaitResult{ThreadID: query.ThreadID}
	selectedRunID := query.RunID
	timedOut, err := c.waitOrchestration(ctx, query.TimeoutMs, func(waitCtx context.Context) (bool, error) {
		done := false
		err := c.orchestrationRead(waitCtx, func(t *txn) error {
			thread, err := requiredThread(t, query.ThreadID)
			if err != nil {
				return err
			}
			result.Link = thread.link()
			var run runRecord
			if selectedRunID == "" {
				run, err = scanRun(t.queryRow(`SELECT `+runColumns+` FROM runs WHERE thread_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, thread.ID))
			} else {
				run, err = loadRun(t, selectedRunID)
			}
			if errors.Is(err, sql.ErrNoRows) && selectedRunID == "" {
				result.Status = "idle"
				done = true
			} else if errors.Is(err, sql.ErrNoRows) {
				return errorf(api.NotFound, "run %q not found", selectedRunID)
			} else if err != nil {
				return err
			} else {
				if run.ThreadID != thread.ID {
					return errorf(api.Conflict, "run does not belong to target thread")
				}
				selectedRunID = run.ID
				result.RunID, result.Status, result.Summary, result.Error = run.ID, run.Status, run.Output, run.Error
				done = api.RunStatus(run.Status).Terminal()
			}
			result.HasPendingQuestion, result.PendingApproval, err = pendingThreadRequests(t, thread.ID)
			if err != nil {
				return err
			}
			if result.HasPendingQuestion || result.PendingApproval != nil {
				done, result.Next = true, ""
				return nil
			}
			if query.UntilDelegatedWorkSettled {
				var delegated bool
				if err := t.queryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_thread_id = ?
					AND status IN ('queued', 'running', 'waiting_children') AND delivery_status <> 'disposed')`, thread.ID).Scan(&delegated); err != nil {
					return err
				}
				done = done && !delegated
			}
			result.Next = ""
			if !done {
				result.Next = api.OrchestratorRunningTaskNext
			}
			return nil
		})
		return done, err
	})
	result.TimedOut = timedOut
	if timedOut && query.CallerThreadID != "" && query.CallerThreadID != query.ThreadID {
		registered, settleErr := c.registerWaitAutoWake(ctx, query.CallerThreadID, query.ThreadID, selectedRunID, &result)
		if settleErr == nil && registered {
			result.AutoWakeRegistered = true
		}
	}
	return result, err
}

func (c *Core) registerWaitAutoWake(ctx context.Context, watcherThreadID, targetThreadID, targetRunID string, result *api.ThreadWaitResult) (bool, error) {
	if strings.TrimSpace(watcherThreadID) == "" || strings.TrimSpace(targetThreadID) == "" || watcherThreadID == targetThreadID {
		return false, nil
	}
	wakeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()

	var (
		registered  bool
		upgradeTask string
	)
	err := c.dispatchWrite(wakeCtx, func(t *txn) error {
		watcher, err := loadThread(t, watcherThreadID)
		if err != nil || watcher.Archived {
			return nil
		}
		// If the watched target belongs to a delegated task owned by the caller,
		// upgrade a manual cohort to auto-resume instead of creating a duplicate peer watch.
		var task taskRecord
		var taskFound bool
		if targetRunID != "" {
			task, taskFound, err = loadTaskByChildRun(t, targetRunID)
		} else {
			rec, scanErr := scanTask(t.queryRow(`SELECT `+taskColumns+` FROM tasks
				WHERE parent_thread_id = ? AND child_thread_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
				watcherThreadID, targetThreadID))
			if scanErr == nil {
				task, taskFound = rec, true
			} else if !errors.Is(scanErr, sql.ErrNoRows) {
				err = wrapInternal("read delegated child watch", scanErr)
			}
		}
		if err != nil {
			return err
		}
		if taskFound && task.ParentThreadID == watcherThreadID {
			cohort, found, err := loadCohort(t, task.ParentRunID, task.Generation)
			if err != nil {
				return err
			}
			if found {
				if cohort.ResumePolicy == resumeManual && (cohort.Status == deliveryPending || cohort.Status == deliveryDelivered) {
					upgradeTask = task.ID
				}
				registered = true
				return nil
			}
		}

		// Re-check whether the target run settled right as the wait timed out.
		var run runRecord
		if targetRunID != "" {
			run, err = loadRun(t, targetRunID)
		} else {
			run, err = scanRun(t.queryRow(`SELECT `+runColumns+` FROM runs WHERE thread_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, targetThreadID))
		}
		if err == nil && run.ThreadID == targetThreadID {
			targetRunID = run.ID
			if api.RunStatus(run.Status).Terminal() {
				if result != nil {
					result.RunID, result.Status, result.Summary, result.Error = run.ID, run.Status, run.Output, run.Error
					result.TimedOut = false
					result.Next = ""
				}
				return nil
			}
		}

		var existingID string
		err = t.queryRow(`SELECT id FROM thread_watches WHERE status = ? AND watcher_thread_id = ? AND target_thread_id = ? AND target_run_id = ? LIMIT 1`,
			watchPending, watcherThreadID, targetThreadID, targetRunID).Scan(&existingID)
		if err == nil {
			registered = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return wrapInternal("read existing thread watch", err)
		}
		now := time.Now().UTC().UnixNano()
		if _, err := t.exec(`INSERT INTO thread_watches
			(id, watcher_thread_id, watcher_run_id, target_thread_id, target_run_id, status, created_at, updated_at)
			VALUES (?, ?, '', ?, ?, ?, ?, ?)`,
			newID("watch"), watcherThreadID, targetThreadID, targetRunID, watchPending, now, now); err != nil {
			return wrapInternal("insert thread watch", err)
		}
		registered = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if upgradeTask != "" {
		if _, upErr := c.Dispatch(wakeCtx, api.Command{
			Version:   api.ProtocolVersion,
			Kind:      api.UpgradeTaskWake,
			CommandID: newID("wake"),
			ThreadID:  watcherThreadID,
			TaskID:    upgradeTask,
		}); upErr != nil {
			return false, upErr
		}
	}
	return registered, nil
}
