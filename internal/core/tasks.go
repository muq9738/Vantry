package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// A task is one durable parent→child assignment. It survives follow-ups: the
// row follows the latest generation, while every older generation stays
// readable through its own Run, its task.updated events and its cohort row.
type taskRecord struct {
	ID                string
	ParentThreadID    string
	ParentRunID       string
	ParentAttemptID   string
	ChildThreadID     string
	ChildRunID        string
	Title             string
	Generation        int64
	Status            string
	DeliveryStatus    string
	DeliveryMessageID string
	Result            string
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const taskColumns = `id, parent_thread_id, parent_run_id, parent_attempt_id, child_thread_id, child_run_id,
	title, generation, status, delivery_status, delivery_message_id, result, error, created_at, updated_at`

func (r taskRecord) toAPI() api.Task {
	return api.Task{
		ID:                r.ID,
		ParentThreadID:    r.ParentThreadID,
		ParentRunID:       r.ParentRunID,
		ChildThreadID:     r.ChildThreadID,
		ChildRunID:        r.ChildRunID,
		Title:             r.Title,
		Generation:        r.Generation,
		Status:            api.RunStatus(r.Status),
		DeliveryStatus:    api.DeliveryStatus(r.DeliveryStatus),
		DeliveryMessageID: r.DeliveryMessageID,
		Result:            r.Result,
		Error:             r.Error,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func (r taskRecord) terminal() bool { return api.RunStatus(r.Status).Terminal() }

func (r taskRecord) consumed() bool {
	return r.DeliveryStatus == deliveryAcknowledged || r.DeliveryStatus == deliveryDisposed
}

func scanTask(row scanner) (taskRecord, error) {
	var (
		rec              taskRecord
		created, updated int64
	)
	if err := row.Scan(&rec.ID, &rec.ParentThreadID, &rec.ParentRunID, &rec.ParentAttemptID,
		&rec.ChildThreadID, &rec.ChildRunID, &rec.Title, &rec.Generation, &rec.Status,
		&rec.DeliveryStatus, &rec.DeliveryMessageID, &rec.Result, &rec.Error, &created, &updated); err != nil {
		return taskRecord{}, err
	}
	rec.CreatedAt = fromNanos(created)
	rec.UpdatedAt = fromNanos(updated)
	return rec, nil
}

func loadTask(t *txn, taskID string) (taskRecord, error) {
	return scanTask(t.queryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, taskID))
}

func loadTaskByChildRun(t *txn, childRunID string) (taskRecord, bool, error) {
	rec, err := scanTask(t.queryRow(`SELECT `+taskColumns+` FROM tasks WHERE child_run_id = ?`, childRunID))
	if errors.Is(err, sql.ErrNoRows) {
		return taskRecord{}, false, nil
	}
	if err != nil {
		return taskRecord{}, false, wrapInternal("read task", err)
	}
	return rec, true, nil
}

func loadTasksByParentRun(t *txn, parentRunID string) ([]taskRecord, error) {
	rows, err := t.query(`SELECT `+taskColumns+` FROM tasks WHERE parent_run_id = ?
		ORDER BY generation, created_at, id`, parentRunID)
	if err != nil {
		return nil, wrapInternal("read tasks", err)
	}
	defer rows.Close()
	var out []taskRecord
	for rows.Next() {
		rec, err := scanTask(rows)
		if err != nil {
			return nil, wrapInternal("scan task", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapInternal("read tasks", err)
	}
	return out, nil
}

func loadCohortTasks(t *txn, parentRunID string, generation int64) ([]taskRecord, error) {
	rows, err := t.query(`SELECT `+taskColumns+` FROM tasks
		WHERE parent_run_id = ? AND generation = ? ORDER BY created_at, id`, parentRunID, generation)
	if err != nil {
		return nil, wrapInternal("read cohort tasks", err)
	}
	defer rows.Close()
	var out []taskRecord
	for rows.Next() {
		rec, err := scanTask(rows)
		if err != nil {
			return nil, wrapInternal("scan cohort task", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapInternal("read cohort tasks", err)
	}
	return out, nil
}

func (c *Core) insertTask(t *txn, rec taskRecord) error {
	rec.CreatedAt = time.Now().UTC()
	rec.UpdatedAt = rec.CreatedAt
	if _, err := t.exec(`INSERT INTO tasks
		(id, parent_thread_id, parent_run_id, parent_attempt_id, child_thread_id, child_run_id, title,
		 generation, status, delivery_status, delivery_message_id, result, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.ParentThreadID, rec.ParentRunID, rec.ParentAttemptID, rec.ChildThreadID, rec.ChildRunID,
		rec.Title, rec.Generation, rec.Status, rec.DeliveryStatus, rec.DeliveryMessageID, rec.Result,
		rec.Error, rec.CreatedAt.UnixNano(), rec.UpdatedAt.UnixNano()); err != nil {
		return wrapInternal("insert task", err)
	}
	return c.emitTask(t, rec, eventTaskCreated)
}

// updateTask persists the whole projection row and commits the task event. The
// caller has already merged the changed fields onto a freshly read record, so a
// concurrent write can never be overwritten with a stale snapshot.
func (c *Core) updateTask(t *txn, rec taskRecord) error {
	rec.UpdatedAt = time.Now().UTC()
	if _, err := t.exec(`UPDATE tasks SET parent_attempt_id = ?, child_run_id = ?, title = ?, generation = ?,
		status = ?, delivery_status = ?, delivery_message_id = ?, result = ?, error = ?, updated_at = ?
		WHERE id = ?`,
		rec.ParentAttemptID, rec.ChildRunID, rec.Title, rec.Generation, rec.Status, rec.DeliveryStatus,
		rec.DeliveryMessageID, rec.Result, rec.Error, rec.UpdatedAt.UnixNano(), rec.ID); err != nil {
		return wrapInternal("update task", err)
	}
	return c.emitTask(t, rec, eventTaskUpdated)
}

func (c *Core) emitTask(t *txn, rec taskRecord, eventType string) error {
	_, err := t.event(eventType, rec.ParentThreadID, rec.ParentRunID, rec.ParentAttemptID,
		taskEventData{Task: rec.toAPI()})
	return err
}

// A cohort is the delivery boundary of one parent→children generation: every
// task created in the same open generation shares one delivery message id.
type cohortRecord struct {
	ParentRunID           string
	Generation            int64
	ResumePolicy          string
	DeliveryMessageID     string
	Status                string
	ContinuationAttemptID string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

const cohortColumns = `parent_run_id, generation, resume_policy, delivery_message_id, status,
	continuation_attempt_id, created_at, updated_at`

func (r cohortRecord) toAPI() api.Cohort {
	return api.Cohort{
		ParentRunID:           r.ParentRunID,
		Generation:            r.Generation,
		ResumePolicy:          r.ResumePolicy,
		DeliveryMessageID:     r.DeliveryMessageID,
		Status:                api.DeliveryStatus(r.Status),
		ContinuationAttemptID: r.ContinuationAttemptID,
		CreatedAt:             r.CreatedAt,
		UpdatedAt:             r.UpdatedAt,
	}
}

func scanCohort(row scanner) (cohortRecord, error) {
	var (
		rec              cohortRecord
		created, updated int64
	)
	if err := row.Scan(&rec.ParentRunID, &rec.Generation, &rec.ResumePolicy, &rec.DeliveryMessageID,
		&rec.Status, &rec.ContinuationAttemptID, &created, &updated); err != nil {
		return cohortRecord{}, err
	}
	rec.CreatedAt = fromNanos(created)
	rec.UpdatedAt = fromNanos(updated)
	return rec, nil
}

func loadCohort(t *txn, parentRunID string, generation int64) (cohortRecord, bool, error) {
	rec, err := scanCohort(t.queryRow(`SELECT `+cohortColumns+` FROM cohorts
		WHERE parent_run_id = ? AND generation = ?`, parentRunID, generation))
	if errors.Is(err, sql.ErrNoRows) {
		return cohortRecord{}, false, nil
	}
	if err != nil {
		return cohortRecord{}, false, wrapInternal("read cohort", err)
	}
	return rec, true, nil
}

func (c *Core) insertCohort(t *txn, rec cohortRecord) error {
	rec.CreatedAt = time.Now().UTC()
	rec.UpdatedAt = rec.CreatedAt
	if _, err := t.exec(`INSERT INTO cohorts
		(parent_run_id, generation, resume_policy, delivery_message_id, status,
		 continuation_attempt_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ParentRunID, rec.Generation, rec.ResumePolicy, rec.DeliveryMessageID, rec.Status,
		rec.ContinuationAttemptID, rec.CreatedAt.UnixNano(), rec.UpdatedAt.UnixNano()); err != nil {
		return wrapInternal("insert cohort", err)
	}
	return c.emitCohort(t, rec, eventCohortCreated)
}

func (c *Core) updateCohort(t *txn, rec cohortRecord) error {
	rec.UpdatedAt = time.Now().UTC()
	if _, err := t.exec(`UPDATE cohorts SET resume_policy = ?, delivery_message_id = ?, status = ?,
		continuation_attempt_id = ?, updated_at = ?
		WHERE parent_run_id = ? AND generation = ?`,
		rec.ResumePolicy, rec.DeliveryMessageID, rec.Status, rec.ContinuationAttemptID,
		rec.UpdatedAt.UnixNano(), rec.ParentRunID, rec.Generation); err != nil {
		return wrapInternal("update cohort", err)
	}
	return c.emitCohort(t, rec, eventCohortUpdated)
}

func (c *Core) emitCohort(t *txn, rec cohortRecord, eventType string) error {
	run, err := loadRun(t, rec.ParentRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return wrapInternal("read parent run", err)
	}
	_, err = t.event(eventType, run.ThreadID, rec.ParentRunID, run.AttemptID,
		cohortEventData{Cohort: rec.toAPI()})
	return err
}

// openCohort returns the generation that currently collects new children for a
// parent run, creating the next generation when the previous one is consumed.
// An existing generation keeps its persisted resume policy and delivery message.
func (c *Core) openCohort(t *txn, parentRunID, policy string, policyExplicit bool) (cohortRecord, bool, error) {
	rec, err := scanCohort(t.queryRow(`SELECT `+cohortColumns+` FROM cohorts
		WHERE parent_run_id = ? AND status IN (?, ?) ORDER BY generation DESC LIMIT 1`,
		parentRunID, deliveryPending, deliveryDelivered))
	switch {
	case err == nil:
		if policyExplicit && rec.ResumePolicy != policy {
			return cohortRecord{}, false, errorf(api.Conflict,
				"generation %d of run %q is already open with resume policy %q",
				rec.Generation, parentRunID, rec.ResumePolicy)
		}
		if rec.Status == deliveryDelivered {
			// A new child joins an unconsumed generation: results are no longer
			// complete, so the generation returns to pending until all finish.
			rec.Status = deliveryPending
			if err := c.updateCohort(t, rec); err != nil {
				return cohortRecord{}, false, err
			}
		}
		return rec, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return cohortRecord{}, false, wrapInternal("read open cohort", err)
	}

	var generation int64
	if err := t.queryRow(`SELECT COALESCE(MAX(generation), 0) FROM cohorts WHERE parent_run_id = ?`,
		parentRunID).Scan(&generation); err != nil {
		return cohortRecord{}, false, wrapInternal("read cohort generation", err)
	}
	rec = cohortRecord{
		ParentRunID:       parentRunID,
		Generation:        generation + 1,
		ResumePolicy:      policy,
		DeliveryMessageID: newID("msg"),
		Status:            deliveryPending,
	}
	if err := c.insertCohort(t, rec); err != nil {
		return cohortRecord{}, false, err
	}
	return rec, true, nil
}

// syncCohort is the single durable reconcile step of one generation. It is
// idempotent: it only writes what actually changed, refreshes the task rows from
// the child runs (so a crash between a child terminal commit and its delivery
// is repaired), marks a fully terminal group as delivered, auto-acknowledges it
// when the generation was created with resumePolicy=auto, and never reopens a
// disposed delivery.
func (c *Core) syncCohort(t *txn, parentRunID string, generation int64) error {
	cohort, found, err := loadCohort(t, parentRunID, generation)
	if err != nil || !found {
		return err
	}
	tasks, err := loadCohortTasks(t, parentRunID, generation)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}

	allTerminal := true
	for i := range tasks {
		task := tasks[i]
		if task.DeliveryStatus == deliveryDisposed {
			continue
		}
		if run, ok, err := c.childRun(t, task.ChildRunID); err != nil {
			return err
		} else if ok {
			next := task
			next.Status = run.Status
			if api.RunStatus(run.Status).Terminal() {
				next.Result = run.Output
				next.Error = run.Error
			}
			if next.Status != task.Status || next.Result != task.Result || next.Error != task.Error {
				if err := c.updateTask(t, next); err != nil {
					return err
				}
				task = next
				tasks[i] = next
			}
		}
		if !task.terminal() {
			allTerminal = false
		}
	}
	if !allTerminal {
		return nil
	}
	if err := c.refreshCohortStatus(t, parentRunID, generation); err != nil {
		return err
	}
	cohort, _, err = loadCohort(t, parentRunID, generation)
	if err != nil {
		return err
	}
	if cohort.Status == deliveryDisposed {
		return c.finishEmptyWait(t, parentRunID)
	}

	for i := range tasks {
		if tasks[i].DeliveryStatus != deliveryPending {
			continue
		}
		next := tasks[i]
		next.DeliveryStatus = deliveryDelivered
		if err := c.updateTask(t, next); err != nil {
			return err
		}
		tasks[i] = next
	}

	switch cohort.Status {
	case deliveryPending:
		if cohort.ResumePolicy == resumeAuto {
			return c.acknowledgeCohort(t, cohort, tasks)
		}
		cohort.Status = deliveryDelivered
		if err := c.updateCohort(t, cohort); err != nil {
			return err
		}
	case deliveryDelivered:
		if cohort.ResumePolicy == resumeAuto {
			return c.acknowledgeCohort(t, cohort, tasks)
		}
	}
	return nil
}

// acknowledgeCohort marks the delivered results of one generation as accepted
// by Core and then, once the parent provider attempt has settled, creates the
// unique continuation attempt. Acknowledged means "durably accepted as parent
// input"; it never claims the model has read the content.
func (c *Core) acknowledgeCohort(t *txn, cohort cohortRecord, tasks []taskRecord) error {
	if cohort.Status == deliveryDisposed || cohort.Status == deliveryAcknowledged {
		return nil
	}
	for i := range tasks {
		// A disposed delivery is never revived by a later acknowledgement.
		if tasks[i].DeliveryStatus != deliveryDelivered {
			continue
		}
		next := tasks[i]
		next.DeliveryStatus = deliveryAcknowledged
		if err := c.updateTask(t, next); err != nil {
			return err
		}
		tasks[i] = next
	}
	cohort.Status = deliveryAcknowledged
	if err := c.updateCohort(t, cohort); err != nil {
		return err
	}
	_, err := c.startContinuation(t, cohort.ParentRunID)
	return err
}

// startContinuation creates the parent's next attempt once (and only once) the
// previous attempt has settled. The original Run input is never overwritten: the
// continuation message lives on the new attempt and its outbox row.
func (c *Core) startContinuation(t *txn, parentRunID string) (bool, error) {
	cohorts, err := c.acknowledgedCohorts(t, parentRunID)
	if err != nil || len(cohorts) == 0 {
		return false, err
	}
	parent, err := loadRun(t, parentRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrapInternal("read parent run", err)
	}
	// The parent provider must have settled: a continuation never runs a second
	// Pi session for the same parent thread concurrently.
	if parent.ActiveAttemptID != "" {
		return false, nil
	}
	switch parent.Status {
	case statusWaitingChildren, statusCompleted:
	default:
		return false, nil
	}

	tasks, err := c.acknowledgedTasks(t, parentRunID)
	if err != nil {
		return false, err
	}
	if len(tasks) == 0 {
		return false, nil
	}
	message := continuationMessage(tasks)

	attemptID, err := c.insertContinuationAttempt(t, parent, message)
	if err != nil {
		return false, err
	}
	// Commit the paragraph boundary with the unique continuation, so recovery
	// cannot append it again and both streaming and durable providers share it.
	separator := ""
	if parent.Output != "" && !strings.HasSuffix(parent.Output, "\n\n") {
		separator = "\n\n"
		if strings.HasSuffix(parent.Output, "\n") {
			separator = "\n"
		}
	}
	now := time.Now().UTC()
	if _, err := t.exec(`UPDATE runs SET status = ?, attempt_id = ?, active_attempt_id = ?, output = ?, updated_at = ?
		WHERE id = ? AND active_attempt_id = ''`,
		statusQueued, attemptID, attemptID, parent.Output+separator, now.UnixNano(), parent.ID); err != nil {
		return false, wrapInternal("queue continuation", err)
	}
	started := parent
	started.Status = statusQueued
	started.AttemptID = attemptID
	started.ActiveAttemptID = attemptID
	started.Output += separator
	started.UpdatedAt = now
	if separator != "" {
		if _, err := t.event(eventRunOutput, started.ThreadID, started.ID, attemptID,
			outputEventData{Delta: separator}); err != nil {
			return false, err
		}
	}
	if _, err := t.exec(`UPDATE cohorts SET continuation_attempt_id = ?, updated_at = ?
		WHERE parent_run_id = ? AND status = ? AND continuation_attempt_id = ''`,
		attemptID, now.UnixNano(), parentRunID, deliveryAcknowledged); err != nil {
		return false, wrapInternal("record continuation", err)
	}
	for _, cohort := range cohorts {
		cohort.ContinuationAttemptID = attemptID
		cohort.UpdatedAt = now
		if err := c.emitCohort(t, cohort, eventCohortUpdated); err != nil {
			return false, err
		}
	}
	if _, err := t.event(eventRunQueued, started.ThreadID, started.ID, attemptID,
		runEventData{Run: started.toAPI()}); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Core) insertContinuationAttempt(t *txn, parent runRecord, message string) (string, error) {
	if err := wakeThread(t, parent.ThreadID); err != nil {
		return "", err
	}
	var ordinal int64
	if err := t.queryRow(`SELECT COALESCE(MAX(ordinal), 0) FROM attempts WHERE run_id = ?`,
		parent.ID).Scan(&ordinal); err != nil {
		return "", wrapInternal("read attempt ordinal", err)
	}
	now := time.Now().UTC()
	attemptID := newID("att")
	if _, err := t.exec(`INSERT INTO attempts
		(id, run_id, ordinal, status, created_at, updated_at, input, provider_cursor)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		attemptID, parent.ID, ordinal+1, statusQueued, now.UnixNano(), now.UnixNano(), message); err != nil {
		return "", wrapInternal("insert continuation attempt", err)
	}
	if _, err := t.exec(`INSERT INTO effect_outbox
		(id, kind, thread_id, run_id, attempt_id, status, attempt_count, claim_token, lease_until,
		 last_error, created_at, updated_at, input)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', 0, '', ?, ?, ?)`,
		newID("eff"), effectKindProviderRun, parent.ThreadID, parent.ID, attemptID, outboxPending,
		now.UnixNano(), now.UnixNano(), message); err != nil {
		return "", wrapInternal("insert continuation effect", err)
	}
	return attemptID, nil
}

func (c *Core) acknowledgedCohorts(t *txn, parentRunID string) ([]cohortRecord, error) {
	rows, err := t.query(`SELECT `+cohortColumns+` FROM cohorts
		WHERE parent_run_id = ? AND status = ? AND continuation_attempt_id = ''
		ORDER BY generation`, parentRunID, deliveryAcknowledged)
	if err != nil {
		return nil, wrapInternal("read acknowledged cohorts", err)
	}
	defer rows.Close()
	var out []cohortRecord
	for rows.Next() {
		rec, err := scanCohort(rows)
		if err != nil {
			return nil, wrapInternal("scan acknowledged cohort", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapInternal("read acknowledged cohorts", err)
	}
	return out, nil
}

func (c *Core) acknowledgedTasks(t *txn, parentRunID string) ([]taskRecord, error) {
	rows, err := t.query(`SELECT `+taskColumns+` FROM tasks
		WHERE parent_run_id = ? AND delivery_status = ?
		AND EXISTS (SELECT 1 FROM cohorts c WHERE c.parent_run_id = tasks.parent_run_id
			AND c.generation = tasks.generation AND c.continuation_attempt_id = '')
		ORDER BY generation, created_at, id`, parentRunID, deliveryAcknowledged)
	if err != nil {
		return nil, wrapInternal("read acknowledged tasks", err)
	}
	defer rows.Close()
	var out []taskRecord
	for rows.Next() {
		rec, err := scanTask(rows)
		if err != nil {
			return nil, wrapInternal("scan acknowledged task", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapInternal("read acknowledged tasks", err)
	}
	return out, nil
}

const maxContinuationResultRunes = 4000

func truncateWakeResult(raw string, maxRunes int) string {
	if maxRunes <= 0 {
		return raw
	}
	runes := []rune(raw)
	if len(runes) <= maxRunes {
		return raw
	}
	head := maxRunes * 3 / 5
	tail := maxRunes - head
	omitted := len(runes) - head - tail
	return fmt.Sprintf("%s\n... (已省略 %d 字符；需要完整内容请调用 vantry_thread_read) ...\n%s",
		string(runes[:head]), omitted, string(runes[len(runes)-tail:]))
}

// continuationMessage carries the child identities, generations, terminal states
// and results that Core accepted for this attempt.
func continuationMessage(tasks []taskRecord) string {
	var b strings.Builder
	b.WriteString("父任务等待期间，Vantry 收到并已接受以下子任务结果：\n")
	for _, task := range tasks {
		linkLine := ""
		if task.ChildThreadID != "" {
			linkLine = fmt.Sprintf("- 会话链接：%s\n", api.FormatThreadLink(task.ChildThreadID, task.Title))
		}
		fmt.Fprintf(&b, "\n### 子任务 %s\n- 任务 ID：%s\n%s- generation：%d\n- 终态：%s\n\n结果：\n%s\n",
			task.Title, task.ID, linkLine, task.Generation, task.Status, truncateWakeResult(resultText(task), maxContinuationResultRunes))
	}
	return b.String()
}

func resultText(task taskRecord) string {
	if strings.TrimSpace(task.Result) != "" {
		return task.Result
	}
	if strings.TrimSpace(task.Error) != "" {
		return "（无输出）错误：" + task.Error
	}
	return "（无输出）"
}

// childRun loads a child run without failing the caller when it disappeared.
func (c *Core) childRun(t *txn, runID string) (runRecord, bool, error) {
	rec, err := loadRun(t, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return runRecord{}, false, nil
	}
	if err != nil {
		return runRecord{}, false, wrapInternal("read child run", err)
	}
	return rec, true, nil
}

// hasOpenDeliveries reports whether a parent run still has results that were not
// accepted or disposed. It is the durable condition for waiting_children.
func (c *Core) hasOpenDeliveries(t *txn, parentRunID string) (bool, error) {
	var open int
	if err := t.queryRow(`SELECT COUNT(1) FROM tasks WHERE parent_run_id = ?
		AND delivery_status IN (?, ?)`, parentRunID, deliveryPending, deliveryDelivered).Scan(&open); err != nil {
		return false, wrapInternal("read open deliveries", err)
	}
	return open > 0, nil
}

// reconcileTaskForRun repairs the task of a run that just reached a terminal
// state (or a waiting_children park) and reconciles its generation.
func (c *Core) reconcileTaskForRun(t *txn, childRunID string) error {
	task, found, err := loadTaskByChildRun(t, childRunID)
	if err != nil || !found {
		return err
	}
	return c.syncCohort(t, task.ParentRunID, task.Generation)
}

// reconcileAll scans every projection window that a crash can leave open:
// terminal child runs whose result was never delivered, and generations whose
// acknowledged results never produced a continuation. It is idempotent and it is
// run both at the Open barrier and from the background worker.
func (c *Core) reconcileAll(t *txn) (bool, error) {
	rows, err := t.query(`SELECT DISTINCT parent_run_id, generation FROM tasks
		WHERE delivery_status IN (?, ?) ORDER BY parent_run_id, generation`,
		deliveryPending, deliveryDelivered)
	if err != nil {
		return false, wrapInternal("read reconcile scope", err)
	}
	type key struct {
		runID      string
		generation int64
	}
	var scope []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.runID, &k.generation); err != nil {
			rows.Close()
			return false, wrapInternal("scan reconcile scope", err)
		}
		scope = append(scope, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, wrapInternal("read reconcile scope", err)
	}
	rows.Close()

	for _, k := range scope {
		if err := c.syncCohort(t, k.runID, k.generation); err != nil {
			return false, err
		}
	}

	parents, err := t.query(`SELECT r.id FROM runs r
		WHERE r.active_attempt_id = '' AND (r.status = ? OR (r.status = ?
		AND EXISTS (SELECT 1 FROM cohorts c WHERE c.parent_run_id = r.id
			AND c.status = ? AND c.continuation_attempt_id = '')))
		ORDER BY r.id`, statusWaitingChildren, statusCompleted, deliveryAcknowledged)
	if err != nil {
		return false, wrapInternal("read pending continuations", err)
	}
	var pending []string
	for parents.Next() {
		var runID string
		if err := parents.Scan(&runID); err != nil {
			parents.Close()
			return false, wrapInternal("scan pending continuation", err)
		}
		pending = append(pending, runID)
	}
	if err := parents.Err(); err != nil {
		parents.Close()
		return false, wrapInternal("read pending continuations", err)
	}
	parents.Close()

	started := false
	for _, runID := range pending {
		ok, err := c.startContinuation(t, runID)
		if err != nil {
			return started, err
		}
		started = started || ok
		if err := c.finishEmptyWait(t, runID); err != nil {
			return started, err
		}
	}
	// A crash between a terminal run and its queued successor is repaired here:
	// the next FIFO message starts only when the thread slot is really free.
	queuedThreads, err := c.threadsWithQueuedInput(t)
	if err != nil {
		return started, err
	}
	for _, threadID := range queuedThreads {
		ok, err := c.startQueuedInput(t, threadID)
		if err != nil {
			return started, err
		}
		started = started || ok
	}
	watchStarted, err := c.reconcilePendingWatches(t)
	if err != nil {
		return started, err
	}
	started = started || watchStarted
	return started, nil
}

const (
	watchPending   = "pending"
	watchDelivered = "delivered"
	watchDisposed  = "disposed"
)

type threadWatchRecord struct {
	ID              string
	WatcherThreadID string
	WatcherRunID    string
	TargetThreadID  string
	TargetRunID     string
	Status          string
}

func watchWakeMessage(target threadRecord, targetFound bool, run runRecord) string {
	title := run.ThreadID
	if targetFound && strings.TrimSpace(target.Title) != "" {
		title = target.Title
	}
	link := api.FormatThreadLink(run.ThreadID, title)
	body := strings.TrimSpace(run.Output)
	if body == "" && strings.TrimSpace(run.Error) != "" {
		body = "（无输出）错误：" + run.Error
	} else if body == "" {
		body = "（无输出）"
	} else if strings.TrimSpace(run.Error) != "" {
		body += "\n\n错误：" + run.Error
	}
	return fmt.Sprintf("Vantry 自动唤醒：关注的会话 %s 已结束当前轮次。\n- 会话 ID：%s\n- 运行 ID：%s\n- 终态：%s\n\n结果：\n%s\n",
		link, run.ThreadID, run.ID, run.Status, truncateWakeResult(body, maxContinuationResultRunes))
}

func (c *Core) reconcileThreadWatches(t *txn, run runRecord) (bool, error) {
	if !api.RunStatus(run.Status).Terminal() {
		return false, nil
	}
	rows, err := t.query(`SELECT id, watcher_thread_id, watcher_run_id, target_thread_id, target_run_id, status
		FROM thread_watches
		WHERE status = ? AND target_thread_id = ? AND (target_run_id = '' OR target_run_id = ?)
		ORDER BY created_at, id`, watchPending, run.ThreadID, run.ID)
	if err != nil {
		return false, wrapInternal("read thread watches", err)
	}
	var watches []threadWatchRecord
	for rows.Next() {
		var w threadWatchRecord
		if err := rows.Scan(&w.ID, &w.WatcherThreadID, &w.WatcherRunID, &w.TargetThreadID, &w.TargetRunID, &w.Status); err != nil {
			rows.Close()
			return false, wrapInternal("scan thread watch", err)
		}
		watches = append(watches, w)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, wrapInternal("read thread watches", err)
	}
	rows.Close()
	if len(watches) == 0 {
		return false, nil
	}
	target, err := loadThread(t, run.ThreadID)
	targetFound := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, wrapInternal("read watched thread", err)
	}
	now := time.Now().UTC().UnixNano()
	msg := watchWakeMessage(target, targetFound, run)
	started := false
	for _, w := range watches {
		watcher, err := loadThread(t, w.WatcherThreadID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && watcher.Archived) {
			if _, execErr := t.exec(`UPDATE thread_watches SET status = ?, updated_at = ? WHERE id = ?`,
				watchDisposed, now, w.ID); execErr != nil {
				return started, wrapInternal("dispose thread watch", execErr)
			}
			continue
		}
		if err != nil {
			return started, wrapInternal("read watcher thread", err)
		}
		if _, err := t.exec(`UPDATE thread_watches SET status = ?, updated_at = ? WHERE id = ?`,
			watchDelivered, now, w.ID); err != nil {
			return started, wrapInternal("deliver thread watch", err)
		}
		if err := wakeThread(t, watcher.ID); err != nil {
			return started, err
		}
		receipt, err := c.acceptQueuedMessage(t, watcher, api.Command{
			Version:        api.ProtocolVersion,
			CommandID:      "watch:" + w.ID,
			Kind:           api.InterThreadSend,
			TargetThreadID: watcher.ID,
			SenderThreadID: run.ThreadID,
			Message:        msg,
		})
		if err != nil {
			return started, err
		}
		if receipt.RunID != "" {
			started = true
		}
	}
	return started, nil
}

func (c *Core) reconcilePendingWatches(t *txn) (bool, error) {
	rows, err := t.query(`SELECT DISTINCT r.id FROM runs r
		JOIN thread_watches w ON w.target_thread_id = r.thread_id
			AND (w.target_run_id = '' OR w.target_run_id = r.id)
		WHERE w.status = ? AND r.status IN (?, ?, ?, ?)
		ORDER BY r.updated_at, r.id`,
		watchPending, statusCompleted, statusCancelled, statusInterrupted, statusFailed)
	if err != nil {
		return false, wrapInternal("read pending watched runs", err)
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return false, wrapInternal("scan pending watched run", err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, wrapInternal("read pending watched runs", err)
	}
	rows.Close()

	started := false
	for _, runID := range runIDs {
		run, err := loadRun(t, runID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return started, wrapInternal("read watched run", err)
		}
		ok, err := c.reconcileThreadWatches(t, run)
		if err != nil {
			return started, err
		}
		started = started || ok
	}
	return started, nil
}

// threadsWithQueuedInput lists threads that hold both a free slot and a waiting
// message.
func (c *Core) threadsWithQueuedInput(t *txn) ([]string, error) {
	rows, err := t.query(`SELECT DISTINCT i.thread_id FROM inputs i
		WHERE i.kind = ? AND i.status = ?
		AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.thread_id = i.thread_id
			AND r.status IN (?, ?, ?))
		ORDER BY i.thread_id`, inputKindQueue, inputQueued,
		statusQueued, statusRunning, statusWaitingChildren)
	if err != nil {
		return nil, wrapInternal("read queued threads", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			return nil, wrapInternal("scan queued thread", err)
		}
		out = append(out, threadID)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapInternal("read queued threads", err)
	}
	return out, nil
}

// finishEmptyWait releases a settled parent after its final result is disposed.
// It also repairs this state during recovery; no provider invocation is needed.
func (c *Core) finishEmptyWait(t *txn, runID string) error {
	run, err := loadRun(t, runID)
	if err != nil {
		return err
	}
	if run.Status != statusWaitingChildren || run.ActiveAttemptID != "" {
		return nil
	}
	open, err := c.hasOpenDeliveries(t, runID)
	if err != nil || open {
		return err
	}
	pending, err := c.acknowledgedCohorts(t, runID)
	if err != nil || len(pending) > 0 {
		return err
	}
	run.Status, run.UpdatedAt = statusCompleted, time.Now().UTC()
	if _, err := t.exec(`UPDATE runs SET status = ?, updated_at = ? WHERE id = ?`, statusCompleted, run.UpdatedAt.UnixNano(), runID); err != nil {
		return err
	}
	if _, err := t.event(eventRunCompleted, run.ThreadID, run.ID, run.AttemptID, runEventData{Run: run.toAPI()}); err != nil {
		return err
	}
	if err := c.reconcileTaskForRun(t, run.ID); err != nil {
		return err
	}
	if _, err := c.reconcileThreadWatches(t, run); err != nil {
		return err
	}
	_, err = c.startQueuedInput(t, run.ThreadID)
	return err
}

// hasReconcileWork is the read-only probe that keeps the idle worker from
// opening an empty transaction on every tick.
func (c *Core) hasReconcileWork(ctx context.Context) bool {
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return false
	}
	var n int
	err := c.store.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(1) FROM tasks t WHERE t.delivery_status = ?
			AND EXISTS (SELECT 1 FROM runs r WHERE r.id = t.child_run_id
				AND r.status IN (?, ?, ?, ?)))
		+ (SELECT COUNT(1) FROM cohorts c JOIN runs r ON r.id = c.parent_run_id
			WHERE c.status = ? AND c.continuation_attempt_id = ''
			AND r.active_attempt_id = '' AND r.status IN (?, ?))
		+ (SELECT COUNT(1) FROM runs r WHERE r.status = ? AND r.active_attempt_id = ''
			AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.parent_run_id = r.id AND t.delivery_status IN (?, ?))
			AND NOT EXISTS (SELECT 1 FROM cohorts c WHERE c.parent_run_id = r.id AND c.status = ? AND c.continuation_attempt_id = ''))
		+ (SELECT COUNT(1) FROM inputs i WHERE i.kind = ? AND i.status = ?
			AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.thread_id = i.thread_id
				AND r.status IN (?, ?, ?)))
		+ (SELECT COUNT(1) FROM thread_watches w WHERE w.status = ?
			AND EXISTS (SELECT 1 FROM runs r WHERE r.thread_id = w.target_thread_id
				AND (w.target_run_id = '' OR r.id = w.target_run_id)
				AND r.status IN (?, ?, ?, ?)))`,
		deliveryPending, statusCompleted, statusCancelled, statusInterrupted, statusFailed,
		deliveryAcknowledged, statusWaitingChildren, statusCompleted,
		statusWaitingChildren, deliveryPending, deliveryDelivered, deliveryAcknowledged,
		inputKindQueue, inputQueued, statusQueued, statusRunning, statusWaitingChildren,
		watchPending, statusCompleted, statusCancelled, statusInterrupted, statusFailed).Scan(&n)
	return err == nil && n > 0
}
