package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

func (c *Core) dispatchCancelTask(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.TaskID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "taskId is required for task.cancel")
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
		task, err := loadTask(t, cmd.TaskID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "task %q not found", cmd.TaskID)
		}
		if err != nil {
			return wrapInternal("read cancelled task", err)
		}
		if cmd.ThreadID != "" && cmd.ThreadID != task.ParentThreadID {
			return errorf(api.Conflict, "task does not belong to caller thread")
		}
		if err := c.cancelTaskTree(t, task, &stopped, map[string]bool{}, firstNonEmpty(cmd.Reason, "task cancelled")); err != nil {
			return err
		}
		receipt = api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: task.ParentThreadID,
			RunID: task.ParentRunID, TaskID: task.ID, Generation: task.Generation, DeliveryMessageID: task.DeliveryMessageID}
		receipt.Sequence, _ = lastEventSequence(t)
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	c.deliverStops(stopped)
	c.notifyWorker()
	return receipt, nil
}

func (c *Core) prepareForceDelete(t *txn, threadID string, stopped *[]cancelledRun) error {
	visited := map[string]bool{}
	if err := c.cancelThreadWork(t, threadID, stopped, visited, "thread deleted"); err != nil {
		return err
	}
	rows, err := t.query(`SELECT `+taskColumns+` FROM tasks WHERE child_thread_id = ?`, threadID)
	if err != nil {
		return err
	}
	var incoming []taskRecord
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return err
		}
		incoming = append(incoming, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, task := range incoming {
		if err := c.cancelTaskTree(t, task, stopped, visited, "thread deleted"); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) deliverStops(stopped []cancelledRun) {
	for _, run := range stopped {
		c.cancelRun(run.request.RunID)
		c.stopProvider(run.request)
	}
}

// cancelTaskTree disposes the assignment before visiting descendants, so no
// descendant completion can deliver results into a parent being cancelled.
func (c *Core) cancelTaskTree(t *txn, task taskRecord, stopped *[]cancelledRun, visited map[string]bool, reason string) error {
	changed := !task.terminal() || task.DeliveryStatus != deliveryDisposed
	if !task.terminal() {
		task.Status = statusCancelled
	}
	if changed {
		task.DeliveryStatus = deliveryDisposed
		if err := c.updateTask(t, task); err != nil {
			return err
		}
	}
	if err := c.cancelThreadWork(t, task.ChildThreadID, stopped, visited, reason); err != nil {
		return err
	}
	return c.syncCohort(t, task.ParentRunID, task.Generation)
}

// All runs and assignments of the child session are in scope, including later
// follow-ups and queued conversation inputs created after the original run.
func (c *Core) cancelThreadWork(t *txn, threadID string, stopped *[]cancelledRun, visited map[string]bool, reason string) error {
	if visited[threadID] {
		return nil
	}
	visited[threadID] = true
	if err := c.cancelQueuedInputs(t, threadID, reason); err != nil {
		return err
	}
	active, err := activeRunID(t, threadID)
	if err != nil {
		return err
	}
	if active != "" {
		run, err := loadRun(t, active)
		if err != nil {
			return wrapInternal("read cancelled session run", err)
		}
		_, cancelled, err := c.cancelRunLocked(t, run)
		if err != nil {
			return err
		}
		*stopped = append(*stopped, cancelled...)
	}
	rows, err := t.query(`SELECT `+taskColumns+` FROM tasks WHERE parent_thread_id = ? ORDER BY created_at, id`, threadID)
	if err != nil {
		return wrapInternal("read descendant tasks", err)
	}
	var tasks []taskRecord
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if err := c.cancelTaskTree(t, task, stopped, visited, reason); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) dispatchUpgradeTaskWake(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ParentRunID) == "" && strings.TrimSpace(cmd.TaskID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "parentRunId or taskId is required for task.wake_upgrade")
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
		parentID, generation := cmd.ParentRunID, cmd.Generation
		if cmd.TaskID != "" {
			task, err := loadTask(t, cmd.TaskID)
			if errors.Is(err, sql.ErrNoRows) {
				return errorf(api.NotFound, "task %q not found", cmd.TaskID)
			}
			if err != nil {
				return err
			}
			if parentID != "" && parentID != task.ParentRunID {
				return errorf(api.Conflict, "task belongs to a different parent run")
			}
			parentID, generation = task.ParentRunID, task.Generation
		}
		parent, err := loadRun(t, parentID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "parent run %q not found", parentID)
		}
		if err != nil {
			return err
		}
		if cmd.ThreadID != "" && cmd.ThreadID != parent.ThreadID {
			return errorf(api.Conflict, "parent run does not belong to caller thread")
		}
		var cohort cohortRecord
		if generation > 0 {
			cohort, _, err = loadCohort(t, parentID, generation)
		} else {
			cohort, err = scanCohort(t.queryRow(`SELECT `+cohortColumns+` FROM cohorts WHERE parent_run_id = ?
				ORDER BY generation DESC LIMIT 1`, parentID))
		}
		if errors.Is(err, sql.ErrNoRows) || err == nil && cohort.ParentRunID == "" {
			return errorf(api.NotFound, "cohort for run %q not found", parentID)
		}
		if err != nil {
			return err
		}
		if cohort.ResumePolicy == resumeManual && (cohort.Status == deliveryPending || cohort.Status == deliveryDelivered) {
			cohort.ResumePolicy = resumeAuto
			if err := c.updateCohort(t, cohort); err != nil {
				return err
			}
		}
		if err := c.syncCohort(t, parentID, cohort.Generation); err != nil {
			return err
		}
		receipt = api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: parent.ThreadID,
			RunID: parentID, TaskID: cmd.TaskID, Generation: cohort.Generation, DeliveryMessageID: cohort.DeliveryMessageID}
		receipt.Sequence, _ = lastEventSequence(t)
		return insertReceipt(t, cmd, receipt)
	})
	if err == nil {
		c.notifyWorker()
	}
	return receipt, err
}
