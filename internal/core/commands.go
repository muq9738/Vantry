package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// Committed event payloads. They carry the committed entity state (or the
// delta plus the accumulated output for output events) so a subscriber can
// apply them to its own projection without reading the database.
type threadEventData struct {
	Thread api.Thread `json:"thread"`
}

type runEventData struct {
	Run api.Run `json:"run"`
}

// Output events carry only the appended text. The run row and snapshots own
// the whole reply; repeating it in every event made the event log and each
// subscriber's decoding grow with the square of a reply's length.
type outputEventData struct {
	Delta  string `json:"delta"`
	Output string `json:"output,omitempty"`
}

type taskEventData struct {
	Task api.Task `json:"task"`
}

type cohortEventData struct {
	Cohort api.Cohort `json:"cohort"`
}

// Dispatch validates one command, then commits its events, projection changes,
// receipt and outbox effect in a single transaction. A repeated commandId with
// the same content returns the original receipt; the same id with different
// content is a conflict. Provider work is never performed here.
func (c *Core) Dispatch(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if cmd.Version != api.ProtocolVersion {
		return api.Receipt{}, errorf(api.VersionMismatch,
			"protocol version %d is not supported (want %d)", cmd.Version, api.ProtocolVersion)
	}
	if strings.TrimSpace(cmd.CommandID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "commandId is required")
	}
	if state := c.state.Load(); state >= stateDraining {
		return api.Receipt{}, errorf(api.NotReady, "core is %s", stateName(state))
	}

	if _, err := normalizeInteractionMode(cmd.InteractionMode); err != nil {
		return api.Receipt{}, err
	}

	switch cmd.Kind {
	case api.DeleteThread:
		return c.dispatchDeleteThread(ctx, cmd)
	case api.ForkThread:
		return c.dispatchForkThread(ctx, cmd)
	case api.RewindThread:
		return c.dispatchRewindThread(ctx, cmd)
	case api.MergeBackThread:
		return c.dispatchMergeBackThread(ctx, cmd)
	case api.EditInput, api.ReorderInput, api.PromoteInputToSteer, "input.update", "input.move":
		return c.dispatchQueueOperation(ctx, cmd)
	case api.CreateThread:
		return c.dispatchCreateThread(ctx, cmd)
	case api.SetThreadModel:
		return c.dispatchSetThreadModel(ctx, cmd)
	case api.SendMessage:
		return c.dispatchSendMessage(ctx, cmd)
	case api.StopRun:
		return c.dispatchStopRun(ctx, cmd)
	case api.DelegateTask:
		return c.dispatchDelegate(ctx, cmd)
	case api.FollowUpTask:
		return c.dispatchFollowup(ctx, cmd)
	case api.AcknowledgeTask:
		return c.dispatchAcknowledge(ctx, cmd)
	case api.DisposeTask:
		return c.dispatchDispose(ctx, cmd)
	case api.CancelTask:
		return c.dispatchCancelTask(ctx, cmd)
	case api.UpgradeTaskWake:
		return c.dispatchUpgradeTaskWake(ctx, cmd)
	case api.InterThreadSend:
		return c.dispatchInterThreadSend(ctx, cmd)
	case api.QueueMessage:
		return c.dispatchQueueMessage(ctx, cmd)
	case api.CancelInput:
		return c.dispatchCancelInput(ctx, cmd)
	case api.UpdateThread:
		return c.dispatchUpdateThread(ctx, cmd)
	case api.CancelMessage:
		// Legacy alias of input.cancel: the queue item ID is the message ID.
		return c.dispatchCancelInput(ctx, cmd)
	case api.SteerRun:
		return c.dispatchSteer(ctx, cmd)
	case api.SteerMessage:
		// Legacy alias of run.steer. The attempt is resolved from the run, and
		// the same input identity is persisted either way.
		return c.dispatchSteer(ctx, cmd)
	case api.RespondRequest:
		return c.dispatchRespond(ctx, cmd)
	default:
		return api.Receipt{}, errorf(api.InvalidCommand, "unsupported command kind %q", cmd.Kind)
	}
}

// commandPayload is the canonical form compared for a repeated commandId. It is
// the actual command JSON, not a digest: a stored receipt is only replayed when
// the content matches field by field.
func commandPayload(cmd api.Command) string {
	raw, err := json.Marshal(cmd)
	if err != nil {
		// api.Command is a plain struct of strings and an int; marshalling cannot
		// fail. Return a value no stored payload can equal.
		return "\x00unencodable"
	}
	return string(raw)
}

// dispatchWrite rechecks the lifecycle while holding the writer, since Close
// can begin after Dispatch's initial check and before this transaction starts.
func (c *Core) dispatchWrite(ctx context.Context, fn func(*txn) error) error {
	return c.write(ctx, func(t *txn) error {
		if state := c.state.Load(); state != stateReady {
			return errorf(api.NotReady, "core is %s", stateName(state))
		}
		return fn(t)
	})
}

// runSeed describes a run created by a command. The input is stored on the run,
// the attempt and the outbox row so a continuation can add a new attempt without
// ever overwriting the original user input.
type runSeed struct {
	ThreadID        string
	SenderThreadID  string
	Input           string
	Attachments     []api.Attachment
	ParentTaskID    string
	ProviderID      string
	Model           string
	Workspace       *api.WorkspaceSpec
	ApprovalPolicy  string
	InteractionMode string
}

func createRun(t *txn, seed runSeed) (api.Run, string, api.Event, error) {
	mode, err := normalizeInteractionMode(seed.InteractionMode)
	if err != nil {
		return api.Run{}, "", api.Event{}, err
	}
	if err := wakeThread(t, seed.ThreadID); err != nil {
		return api.Run{}, "", api.Event{}, err
	}
	now := time.Now().UTC()
	workspace, err := encodeWorkspace(seed.Workspace)
	if err != nil {
		return api.Run{}, "", api.Event{}, err
	}
	runID := newID("run")
	attemptID := newID("att")
	run := api.Run{
		ID:              runID,
		ThreadID:        seed.ThreadID,
		SenderThreadID:  seed.SenderThreadID,
		AttemptID:       attemptID,
		Status:          api.RunQueued,
		Input:           seed.Input,
		Attachments:     seed.Attachments,
		CreatedAt:       now,
		UpdatedAt:       now,
		ProviderID:      seed.ProviderID,
		Model:           seed.Model,
		Workspace:       seed.Workspace,
		ApprovalPolicy:  firstNonEmpty(seed.ApprovalPolicy, approvalAuto),
		InteractionMode: mode,
	}
	attachments := "[]"
	if len(seed.Attachments) > 0 {
		raw, err := json.Marshal(seed.Attachments)
		if err != nil {
			return api.Run{}, "", api.Event{}, wrapInternal("encode run attachments", err)
		}
		attachments = string(raw)
	}
	if _, err := t.exec(`INSERT INTO runs
		(id, thread_id, attempt_id, active_attempt_id, status, input, output, error, created_at, updated_at,
		 provider_id, model, workspace, parent_task_id, approval_policy, attachments, interaction_mode, sender_thread_id)
		VALUES (?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.ThreadID, attemptID, attemptID, statusQueued, run.Input,
		now.UnixNano(), now.UnixNano(), seed.ProviderID, seed.Model, workspace, seed.ParentTaskID,
		run.ApprovalPolicy, attachments, run.InteractionMode, run.SenderThreadID); err != nil {
		return api.Run{}, "", api.Event{}, wrapInternal("insert run", err)
	}
	attemptInput := seed.Input
	if _, err := t.exec(`INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at, input, provider_cursor)
		VALUES (?, ?, 1, ?, ?, ?, ?, 0)`,
		attemptID, runID, statusQueued, now.UnixNano(), now.UnixNano(), attemptInput); err != nil {
		return api.Run{}, "", api.Event{}, wrapInternal("insert attempt", err)
	}
	if _, err := t.exec(`INSERT INTO effect_outbox
		(id, kind, thread_id, run_id, attempt_id, status, attempt_count, claim_token, lease_until,
		 last_error, created_at, updated_at, input)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', 0, '', ?, ?, ?)`,
		newID("eff"), effectKindProviderRun, run.ThreadID, run.ID, attemptID, outboxPending,
		now.UnixNano(), now.UnixNano(), attemptInput); err != nil {
		return api.Run{}, "", api.Event{}, wrapInternal("insert effect", err)
	}
	ev, err := t.event(eventRunQueued, run.ThreadID, run.ID, attemptID, runEventData{Run: run})
	if err != nil {
		return api.Run{}, "", api.Event{}, err
	}
	return run, attemptID, ev, nil
}

func (c *Core) dispatchCreateThread(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	var (
		receipt  api.Receipt
		existing api.Receipt
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			existing = prior
			return nil
		}

		threadID := cmd.ThreadID
		if threadID == "" {
			threadID = newID("th")
		}
		var taken int
		if err := t.queryRow(`SELECT COUNT(1) FROM threads WHERE id = ?`, threadID).Scan(&taken); err != nil {
			return wrapInternal("read thread", err)
		}
		if taken > 0 {
			return errorf(api.Conflict, "thread %q already exists", threadID)
		}

		policy, err := normalizeApprovalPolicy(cmd.ApprovalPolicy)
		if err != nil {
			return err
		}
		thread := api.Thread{
			ID:                threadID,
			Title:             cmd.Title,
			CreatedByThreadID: cmd.CreatedByThreadID,
			CreatedByRunID:    cmd.CreatedByRunID,
			SnoozedUntil:      optionalTime(timeNanos(cmd.SnoozedUntil)),
			CreatedAt:         time.Now().UTC(),
			ProviderID:        firstNonEmpty(cmd.ProviderID, c.defaultProviderID),
			Model:             firstNonEmpty(cmd.Model, c.defaultModel),
			Workspace:         firstWorkspace(cmd.Workspace, c.defaultWorkspace),
			ApprovalPolicy:    policy,
			InteractionMode:   firstNonEmpty(strings.TrimSpace(cmd.InteractionMode), interactionDefault),
		}
		if err := insertThread(t, thread); err != nil {
			return err
		}
		ev, err := t.event(eventThreadCreated, thread.ID, "", "", threadEventData{Thread: thread})
		if err != nil {
			return err
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  thread.ID,
			Sequence:  ev.Sequence,
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	return receipt, nil
}

func insertThread(t *txn, thread api.Thread) error {
	mode, err := normalizeInteractionMode(thread.InteractionMode)
	if err != nil {
		return err
	}
	workspace, err := encodeWorkspace(thread.Workspace)
	if err != nil {
		return err
	}
	if _, err := t.exec(`INSERT INTO threads
		(id, title, created_at, provider_id, model, workspace, parent_thread_id, parent_task_id,
		 approval_policy, interaction_mode, forked_from_thread_id, forked_from_run_id,
		 snoozed_until, created_by_thread_id, created_by_run_id, project_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		thread.ID, thread.Title, thread.CreatedAt.UnixNano(), thread.ProviderID, thread.Model,
		workspace, thread.ParentThreadID, thread.ParentTaskID,
		firstNonEmpty(thread.ApprovalPolicy, approvalAuto), mode, thread.ForkedFromThreadID, thread.ForkedFromRunID,
		timeNanos(thread.SnoozedUntil), thread.CreatedByThreadID, thread.CreatedByRunID, thread.ProjectPath); err != nil {
		return wrapInternal("insert thread", err)
	}
	return nil
}

// activeRunID returns the run that currently owns a thread's single active slot,
// including a run parked in waiting_children.
func activeRunID(t *txn, threadID string) (string, error) {
	var runID string
	err := t.queryRow(`SELECT id FROM runs WHERE thread_id = ? AND status IN (?, ?, ?) LIMIT 1`,
		threadID, statusQueued, statusRunning, statusWaitingChildren).Scan(&runID)
	switch {
	case err == nil:
		return runID, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	default:
		return "", wrapInternal("read active run", err)
	}
}

func (c *Core) dispatchSendMessage(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", api.SendMessage)
	}
	if strings.TrimSpace(cmd.Message) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "message is required for %s", api.SendMessage)
	}

	var (
		receipt  api.Receipt
		existing api.Receipt
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			existing = prior
			return nil
		}

		thread, err := loadThread(t, cmd.ThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "thread %q not found", cmd.ThreadID)
		}
		if err != nil {
			return wrapInternal("read thread", err)
		}
		if thread.Archived {
			return errorf(api.Conflict, "thread %q is archived; restore it before sending", thread.ID)
		}
		if err := c.checkAttachments(cmd.Attachments); err != nil {
			return err
		}

		activeID, err := activeRunID(t, cmd.ThreadID)
		if err != nil {
			return err
		}
		if activeID != "" {
			return errorf(api.Conflict, "thread %q already has active run %q", cmd.ThreadID, activeID)
		}

		run, _, ev, err := createRun(t, runSeed{
			ThreadID:        thread.ID,
			SenderThreadID:  cmd.SenderThreadID,
			Input:           cmd.Message,
			Attachments:     cmd.Attachments,
			ProviderID:      thread.ProviderID,
			Model:           thread.Model,
			Workspace:       thread.Workspace,
			ApprovalPolicy:  thread.ApprovalPolicy,
			InteractionMode: firstNonEmpty(cmd.InteractionMode, thread.InteractionMode),
		})
		if err != nil {
			return err
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  run.ThreadID,
			RunID:     run.ID,
			Sequence:  ev.Sequence,
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	// The accepted run is now durable; the worker (not the command path) drives
	// the provider.
	c.notifyWorker()
	return receipt, nil
}

// cancelledRun is one run terminalized by run.stop, kept so the committed
// cancellation can be forwarded to a durable provider after the transaction.
type cancelledRun struct {
	request provider.Request
}

func (c *Core) dispatchStopRun(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.RunID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "runId is required for %s", api.StopRun)
	}

	var (
		receipt  api.Receipt
		existing api.Receipt
		stopped  []cancelledRun
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			existing = prior
			return nil
		}

		run, err := loadRun(t, cmd.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "run %q not found", cmd.RunID)
		}
		if err != nil {
			return wrapInternal("read run", err)
		}
		if cmd.ThreadID != "" && cmd.ThreadID != run.ThreadID {
			return errorf(api.NotFound, "run %q does not belong to thread %q", run.ID, cmd.ThreadID)
		}

		if !run.occupyingSlot() {
			// Stopping a terminal run is idempotent: no new event, no new state.
			receipt = api.Receipt{
				CommandID: cmd.CommandID,
				Accepted:  true,
				ThreadID:  run.ThreadID,
				RunID:     run.ID,
			}
			return insertReceipt(t, cmd, receipt)
		}

		lastSeq, cancelled, err := c.cancelRunLocked(t, run)
		if err != nil {
			return err
		}
		stopped = cancelled
		// A stopped thread must not continue on its own: the messages that have
		// not started yet are cancelled with the same durable decision.
		if err := c.cancelQueuedInputs(t, run.ThreadID, "queue cancelled by run.stop"); err != nil {
			return err
		}
		if seq, ok := lastEventSequence(t); ok && seq > lastSeq {
			lastSeq = seq
		}

		// A cancelled parent must never be revived: unconsumed deliveries of its
		// children are disposed in the same transaction.
		if activeTasks, err := loadTasksByParentRun(t, run.ID); err != nil {
			return err
		} else if len(activeTasks) > 0 {
			if err := c.cascadeStopChildren(t, run, activeTasks, &stopped, &lastSeq); err != nil {
				return err
			}
		}

		// Stopping a single child still delivers its terminal state to the parent.
		if err := c.reconcileTaskForRun(t, run.ID); err != nil {
			return err
		}

		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  run.ThreadID,
			RunID:     run.ID,
			Sequence:  lastSeq,
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	for _, run := range stopped {
		// Cancellation after commit: the attempt is already invalidated durably, so
		// even a provider that keeps emitting cannot commit anything.
		c.cancelRun(run.request.RunID)
		c.stopProvider(run.request)
	}
	// A stop that terminalized a child may have queued the parent's continuation.
	c.notifyWorker()
	return receipt, nil
}

// cancelRunLocked commits the cancellation of one run and its attempt/effect.
func (c *Core) cancelRunLocked(t *txn, run runRecord) (int64, []cancelledRun, error) {
	now := time.Now().UTC()
	cancelled := run
	cancelled.Status = statusCancelled
	cancelled.ActiveAttemptID = ""
	cancelled.UpdatedAt = now

	if _, err := t.exec(`UPDATE runs SET status = ?, active_attempt_id = '', updated_at = ?
		WHERE id = ? AND status IN (?, ?, ?)`,
		statusCancelled, now.UnixNano(), run.ID, statusQueued, statusRunning, statusWaitingChildren); err != nil {
		return 0, nil, wrapInternal("cancel run", err)
	}
	if _, err := t.exec(`UPDATE attempts SET status = ?, updated_at = ?
		WHERE run_id = ? AND status IN (?, ?, ?)`,
		statusCancelled, now.UnixNano(), run.ID, statusQueued, statusRunning, statusWaitingChildren); err != nil {
		return 0, nil, wrapInternal("cancel attempt", err)
	}
	// The pending effect is settled in the same transaction as the terminal
	// run state, so the outbox never disagrees with the run.
	if _, err := t.exec(`UPDATE effect_outbox SET status = ?, updated_at = ?
		WHERE run_id = ? AND status IN (?, ?)`,
		outboxCancelled, now.UnixNano(), run.ID, outboxPending, outboxClaimed); err != nil {
		return 0, nil, wrapInternal("cancel effect", err)
	}
	ev, err := t.event(eventRunCancelled, run.ThreadID, run.ID, run.AttemptID,
		runEventData{Run: cancelled.toAPI()})
	if err != nil {
		return 0, nil, err
	}
	request, err := providerRequestForAttempt(t, run, run.AttemptID)
	if err != nil {
		return 0, nil, err
	}
	// A cancelled attempt leaves no running tool, open dialog or pending steer.
	if err := c.cleanupAttemptWork(t, run.AttemptID, "attempt was cancelled"); err != nil {
		return 0, nil, err
	}
	if _, ok := c.provider.(provider.Durable); ok {
		payload, err := json.Marshal(request)
		if err != nil {
			return 0, nil, err
		}
		if _, err := t.exec(`INSERT INTO effect_outbox
			(id, kind, thread_id, run_id, attempt_id, status, created_at, updated_at, input)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, newID("eff"), effectKindProviderStop,
			run.ThreadID, run.ID, run.AttemptID, outboxPending, now.UnixNano(), now.UnixNano(), string(payload)); err != nil {
			return 0, nil, wrapInternal("record provider stop intent", err)
		}
	}
	return ev.Sequence, []cancelledRun{{request: request}}, nil
}

// cascadeStopChildren cancels every active descendant run and disposes the
// deliveries the cancelled parent can no longer consume.
func (c *Core) cascadeStopChildren(t *txn, parent runRecord, tasks []taskRecord, stopped *[]cancelledRun, lastSeq *int64) error {
	generations := make(map[int64]struct{})
	for _, task := range tasks {
		current := task
		if child, ok, err := c.childRun(t, current.ChildRunID); err != nil {
			return err
		} else if ok {
			if child.occupyingSlot() {
				seq, cancelled, err := c.cancelRunLocked(t, child)
				if err != nil {
					return err
				}
				*stopped = append(*stopped, cancelled...)
				if seq > *lastSeq {
					*lastSeq = seq
				}
				current.Status = statusCancelled
				current.Result = child.Output
				current.Error = child.Error
				// A cancelled descendant cannot consume its own children either.
				if descendants, err := loadTasksByParentRun(t, child.ID); err != nil {
					return err
				} else if len(descendants) > 0 {
					if err := c.cascadeStopChildren(t, child, descendants, stopped, lastSeq); err != nil {
						return err
					}
				}
			} else {
				current.Status = child.Status
				current.Result = child.Output
				current.Error = child.Error
			}
		}
		if !current.consumed() {
			current.DeliveryStatus = deliveryDisposed
		}
		if err := c.updateTask(t, current); err != nil {
			return err
		}
		generations[current.Generation] = struct{}{}
	}

	for generation := range generations {
		cohort, found, err := loadCohort(t, parent.ID, generation)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		cohortTasks, err := loadCohortTasks(t, parent.ID, generation)
		if err != nil {
			return err
		}
		status := deliveryDisposed
		for _, task := range cohortTasks {
			if task.DeliveryStatus == deliveryAcknowledged {
				status = deliveryAcknowledged
				break
			}
		}
		if cohort.Status == status && cohort.Status != deliveryPending {
			continue
		}
		// Only dispositions and acknowledgements remain; a cancelled parent never
		// gains a continuation.
		cohort.Status = status
		if err := c.updateCohort(t, cohort); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) dispatchDelegate(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ParentRunID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "parentRunId is required for %s", api.DelegateTask)
	}
	if strings.TrimSpace(cmd.Message) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "message is required for %s", api.DelegateTask)
	}
	policy := cmd.ResumePolicy
	if policy == "" {
		policy = resumeAuto
	}
	if policy != resumeAuto && policy != resumeManual {
		return api.Receipt{}, errorf(api.InvalidCommand, "resumePolicy %q is not supported", cmd.ResumePolicy)
	}

	var (
		receipt  api.Receipt
		existing api.Receipt
		queued   string
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			existing = prior
			return nil
		}

		parent, err := loadRun(t, cmd.ParentRunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "parent run %q not found", cmd.ParentRunID)
		}
		if err != nil {
			return wrapInternal("read parent run", err)
		}
		if cmd.ThreadID != "" && cmd.ThreadID != parent.ThreadID {
			return errorf(api.Conflict, "parent run does not belong to caller thread")
		}
		if !parent.occupyingSlot() {
			// The UI may delegate from a recently completed Run. Only the latest Run
			// of its thread may be parked again: an older Run cannot be revived while
			// a newer or still active Run exists.
			parked, err := c.parkParentForChildren(t, parent)
			if err != nil {
				return err
			}
			parent = parked
		}
		if cmd.ParentAttemptID != "" {
			if cmd.ParentAttemptID != currentParentAttempt(parent) {
				return errorf(api.Conflict,
					"parent attempt %q is no longer current for run %q", cmd.ParentAttemptID, parent.ID)
			}
		}

		cohort, _, err := c.openCohort(t, parent.ID, policy, cmd.ResumePolicy != "")
		if err != nil {
			return err
		}

		parentThread, err := loadThread(t, parent.ThreadID)
		if err != nil {
			return wrapInternal("read parent thread", err)
		}
		providerID := cmd.ProviderID
		if providerID == "" {
			providerID = parentThread.ProviderID
		}
		model := cmd.Model
		if model == "" && providerID == parentThread.ProviderID {
			model = parentThread.Model
		}
		workspace := cmd.Workspace
		if workspace == nil {
			workspace = childWorkspace(parentThread.Workspace)
		}
		// Approval policy is inherited from the parent thread unless the command
		// states one explicitly.
		approvalPolicy := parentThread.ApprovalPolicy
		if cmd.ApprovalPolicy != "" {
			if approvalPolicy, err = normalizeApprovalPolicy(cmd.ApprovalPolicy); err != nil {
				return err
			}
		}

		mode, err := normalizeInteractionMode(firstNonEmpty(cmd.InteractionMode, parentThread.InteractionMode))
		if err != nil {
			return err
		}
		// A thread-bound caller has its parent's ceiling. Unscoped desktop
		// commands retain their explicit operator-selected configuration.
		if cmd.ThreadID != "" {
			if err := enforceCeiling(parentThread, approvalPolicy, mode); err != nil {
				return err
			}
		}
		taskID := newID("task")
		childThreadID := newID("th")
		title := cmd.Title
		if strings.TrimSpace(title) == "" {
			title = taskTitle(cmd.Message)
		}
		childThread := api.Thread{
			ID:                childThreadID,
			Title:             title,
			CreatedAt:         time.Now().UTC(),
			ProviderID:        providerID,
			Model:             model,
			Workspace:         workspace,
			ParentThreadID:    parent.ThreadID,
			ParentTaskID:      taskID,
			CreatedByThreadID: parent.ThreadID,
			CreatedByRunID:    parent.ID,
			ApprovalPolicy:    approvalPolicy,
			InteractionMode:   mode,
		}
		if err := insertThread(t, childThread); err != nil {
			return err
		}
		if _, err := t.event(eventThreadCreated, childThread.ID, "", "", threadEventData{Thread: childThread}); err != nil {
			return err
		}

		run, _, ev, err := createRun(t, runSeed{
			ThreadID:        childThreadID,
			Input:           cmd.Message,
			ParentTaskID:    taskID,
			ProviderID:      providerID,
			Model:           model,
			Workspace:       workspace,
			ApprovalPolicy:  approvalPolicy,
			InteractionMode: mode,
		})
		if err != nil {
			return err
		}

		task := taskRecord{
			ID:                taskID,
			ParentThreadID:    parent.ThreadID,
			ParentRunID:       parent.ID,
			ParentAttemptID:   currentParentAttempt(parent),
			ChildThreadID:     childThreadID,
			ChildRunID:        run.ID,
			Title:             title,
			Generation:        cohort.Generation,
			Status:            statusQueued,
			DeliveryStatus:    deliveryPending,
			DeliveryMessageID: cohort.DeliveryMessageID,
		}
		if err := c.insertTask(t, task); err != nil {
			return err
		}

		receipt = api.Receipt{
			CommandID:         cmd.CommandID,
			Accepted:          true,
			ThreadID:          childThreadID,
			RunID:             run.ID,
			Sequence:          ev.Sequence,
			TaskID:            taskID,
			Generation:        cohort.Generation,
			DeliveryMessageID: cohort.DeliveryMessageID,
		}
		queued = run.ID
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	if queued != "" {
		c.notifyWorker()
	}
	return receipt, nil
}

func (c *Core) dispatchFollowup(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.TaskID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "taskId is required for %s", api.FollowUpTask)
	}
	if strings.TrimSpace(cmd.Message) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "message is required for %s", api.FollowUpTask)
	}

	var (
		receipt  api.Receipt
		existing api.Receipt
		queued   string
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			existing = prior
			return nil
		}

		task, err := loadTask(t, cmd.TaskID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "task %q not found", cmd.TaskID)
		}
		if err != nil {
			return wrapInternal("read task", err)
		}
		if cmd.Generation != 0 && cmd.Generation != task.Generation {
			return errorf(api.Conflict, "task %q generation is %d, not %d", task.ID, task.Generation, cmd.Generation)
		}
		if cmd.ParentRunID != "" && cmd.ParentRunID != task.ParentRunID {
			return errorf(api.Conflict, "task %q does not belong to parent run %q", task.ID, cmd.ParentRunID)
		}
		parent, err := loadRun(t, task.ParentRunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "parent run %q not found", task.ParentRunID)
		}
		if err != nil {
			return wrapInternal("read parent run", err)
		}
		if !parent.occupyingSlot() {
			// The parent finished, but a follow-up re-opens the task: the run parks
			// in waiting_children again instead of losing the new generation. A
			// cancelled, interrupted or failed parent is never revived.
			parked, err := c.parkParentForChildren(t, parent)
			if err != nil {
				return err
			}
			parent = parked
		}
		// A follow-up must never overwrite a generation whose delivery the parent
		// has not consumed yet.
		if !task.consumed() {
			return errorf(api.Conflict,
				"generation %d of task %q has not been acknowledged or disposed yet",
				task.Generation, task.ID)
		}
		if active, err := activeRunID(t, task.ChildThreadID); err != nil {
			return err
		} else if active != "" {
			return errorf(api.Conflict, "child thread %q already has active run %q",
				task.ChildThreadID, active)
		}

		policy := resumeAuto
		if previous, found, err := loadCohort(t, task.ParentRunID, task.Generation); err != nil {
			return err
		} else if found {
			if previous.Status != deliveryAcknowledged && previous.Status != deliveryDisposed {
				return errorf(api.Conflict, "generation %d still has unconsumed results", task.Generation)
			}
			if previous.Status == deliveryAcknowledged && previous.ContinuationAttemptID == "" {
				return errorf(api.Conflict, "generation %d is still waiting to become parent input", task.Generation)
			}
			policy = previous.ResumePolicy
		}
		if cmd.ResumePolicy != "" {
			policy = cmd.ResumePolicy
		}
		cohort, _, err := c.openCohort(t, task.ParentRunID, policy, cmd.ResumePolicy != "")
		if err != nil {
			return err
		}
		generation := cohort.Generation

		childThread, err := loadThread(t, task.ChildThreadID)
		if err != nil {
			return wrapInternal("read child thread", err)
		}
		run, _, ev, err := createRun(t, runSeed{
			ThreadID:        childThread.ID,
			Input:           cmd.Message,
			ParentTaskID:    task.ID,
			ProviderID:      childThread.ProviderID,
			Model:           childThread.Model,
			Workspace:       childThread.Workspace,
			ApprovalPolicy:  childThread.ApprovalPolicy,
			InteractionMode: firstNonEmpty(cmd.InteractionMode, childThread.InteractionMode),
		})
		if err != nil {
			return err
		}

		task.Generation = generation
		task.ChildRunID = run.ID
		task.ParentAttemptID = currentParentAttempt(parent)
		task.Status = statusQueued
		task.DeliveryStatus = deliveryPending
		task.DeliveryMessageID = cohort.DeliveryMessageID
		task.Result = ""
		task.Error = ""
		if err := c.updateTask(t, task); err != nil {
			return err
		}

		receipt = api.Receipt{
			CommandID:         cmd.CommandID,
			Accepted:          true,
			ThreadID:          childThread.ID,
			RunID:             run.ID,
			Sequence:          ev.Sequence,
			TaskID:            task.ID,
			Generation:        generation,
			DeliveryMessageID: cohort.DeliveryMessageID,
		}
		queued = run.ID
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	if queued != "" {
		c.notifyWorker()
	}
	return receipt, nil
}

func (c *Core) dispatchAcknowledge(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ParentRunID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "parentRunId is required for %s", api.AcknowledgeTask)
	}
	if cmd.Generation <= 0 {
		return api.Receipt{}, errorf(api.InvalidCommand, "generation is required for %s", api.AcknowledgeTask)
	}
	if strings.TrimSpace(cmd.DeliveryMessageID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "deliveryMessageId is required for %s", api.AcknowledgeTask)
	}

	var (
		receipt  api.Receipt
		existing api.Receipt
		queued   bool
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			existing = prior
			return nil
		}

		parent, err := loadRun(t, cmd.ParentRunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "parent run %q not found", cmd.ParentRunID)
		}
		if err != nil {
			return wrapInternal("read parent run", err)
		}
		cohort, found, err := loadCohort(t, cmd.ParentRunID, cmd.Generation)
		if err != nil {
			return err
		}
		if !found {
			return errorf(api.NotFound, "generation %d of run %q not found", cmd.Generation, cmd.ParentRunID)
		}
		if cohort.DeliveryMessageID != cmd.DeliveryMessageID {
			return errorf(api.Conflict,
				"delivery message %q does not belong to generation %d of run %q",
				cmd.DeliveryMessageID, cmd.Generation, cmd.ParentRunID)
		}
		// Reconcile first so a terminal group that a crash left undelivered can
		// still be acknowledged by its persisted identity.
		if err := c.syncCohort(t, cmd.ParentRunID, cmd.Generation); err != nil {
			return err
		}
		cohort, found, err = loadCohort(t, cmd.ParentRunID, cmd.Generation)
		if err != nil {
			return err
		}
		if !found {
			return errorf(api.NotFound, "generation %d of run %q not found", cmd.Generation, cmd.ParentRunID)
		}

		receipt = api.Receipt{
			CommandID:         cmd.CommandID,
			Accepted:          true,
			ThreadID:          parent.ThreadID,
			RunID:             parent.ID,
			Generation:        cmd.Generation,
			DeliveryMessageID: cmd.DeliveryMessageID,
		}

		switch cohort.Status {
		case deliveryAcknowledged:
			// Already accepted: a repeated or stale acknowledgement never creates a
			// second continuation.
			receipt.Sequence = 0
		case deliveryDisposed:
			return errorf(api.Conflict,
				"delivery %q was disposed and cannot be acknowledged", cmd.DeliveryMessageID)
		case deliveryPending:
			return errorf(api.Conflict,
				"generation %d of run %q still has running children", cmd.Generation, cmd.ParentRunID)
		case deliveryDelivered:
			tasks, err := loadCohortTasks(t, cmd.ParentRunID, cmd.Generation)
			if err != nil {
				return err
			}
			if err := c.acknowledgeCohort(t, cohort, tasks); err != nil {
				return err
			}
			if seq, ok := lastEventSequence(t); ok {
				receipt.Sequence = seq
			}
			queued = true
		default:
			return errorf(api.Conflict, "delivery %q is in an unknown state", cmd.DeliveryMessageID)
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	if queued {
		c.notifyWorker()
	}
	return receipt, nil
}

func (c *Core) dispatchDispose(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.TaskID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "taskId is required for %s", api.DisposeTask)
	}
	if cmd.Generation <= 0 {
		return api.Receipt{}, errorf(api.InvalidCommand, "generation is required for %s", api.DisposeTask)
	}
	if strings.TrimSpace(cmd.DeliveryMessageID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "deliveryMessageId is required for %s", api.DisposeTask)
	}

	var (
		receipt  api.Receipt
		existing api.Receipt
	)
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			existing = prior
			return nil
		}

		task, err := loadTask(t, cmd.TaskID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "task %q not found", cmd.TaskID)
		}
		if err != nil {
			return wrapInternal("read task", err)
		}
		if task.Generation != cmd.Generation {
			return errorf(api.Conflict, "task %q generation is %d, not %d",
				task.ID, task.Generation, cmd.Generation)
		}
		if task.DeliveryMessageID != cmd.DeliveryMessageID {
			return errorf(api.Conflict,
				"delivery message %q does not belong to generation %d of task %q",
				cmd.DeliveryMessageID, cmd.Generation, task.ID)
		}
		cohort, found, err := loadCohort(t, task.ParentRunID, task.Generation)
		if err != nil {
			return err
		}
		if !found {
			return errorf(api.NotFound, "generation %d of run %q not found", task.Generation, task.ParentRunID)
		}
		if cohort.DeliveryMessageID != cmd.DeliveryMessageID {
			return errorf(api.Conflict,
				"delivery message %q does not belong to generation %d of run %q",
				cmd.DeliveryMessageID, task.Generation, task.ParentRunID)
		}
		if err := c.syncCohort(t, task.ParentRunID, task.Generation); err != nil {
			return err
		}
		task, err = loadTask(t, cmd.TaskID)
		if err != nil {
			return wrapInternal("read task", err)
		}
		cohort, _, err = loadCohort(t, task.ParentRunID, task.Generation)
		if err != nil {
			return err
		}

		parent, err := loadRun(t, task.ParentRunID)
		if err != nil {
			return wrapInternal("read parent run", err)
		}
		receipt = api.Receipt{
			CommandID:         cmd.CommandID,
			Accepted:          true,
			ThreadID:          parent.ThreadID,
			RunID:             parent.ID,
			TaskID:            task.ID,
			Generation:        task.Generation,
			DeliveryMessageID: cmd.DeliveryMessageID,
		}

		if task.DeliveryStatus == deliveryDisposed {
			receipt.Sequence = 0
			return insertReceipt(t, cmd, receipt)
		}
		if !task.terminal() {
			return errorf(api.Conflict, "task %q has no result yet", task.ID)
		}
		if task.DeliveryStatus == deliveryAcknowledged && cohort.ContinuationAttemptID != "" {
			return errorf(api.Conflict,
				"result of task %q was already delivered to the parent", task.ID)
		}

		task.DeliveryStatus = deliveryDisposed
		if err := c.updateTask(t, task); err != nil {
			return err
		}
		if err := c.refreshCohortStatus(t, task.ParentRunID, task.Generation); err != nil {
			return err
		}
		if err := c.finishEmptyWait(t, task.ParentRunID); err != nil {
			return err
		}
		if seq, ok := lastEventSequence(t); ok {
			receipt.Sequence = seq
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	return receipt, nil
}

// refreshCohortStatus recomputes the delivery status of one generation from its
// task rows: fully consumed becomes acknowledged (some result accepted) or
// disposed (nothing left to consume). A disposed delivery is terminal.
func (c *Core) refreshCohortStatus(t *txn, parentRunID string, generation int64) error {
	cohort, found, err := loadCohort(t, parentRunID, generation)
	if err != nil || !found {
		return err
	}
	if cohort.Status == deliveryDisposed {
		return nil
	}
	tasks, err := loadCohortTasks(t, parentRunID, generation)
	if err != nil {
		return err
	}
	acknowledged := false
	remaining := false
	for _, task := range tasks {
		switch task.DeliveryStatus {
		case deliveryAcknowledged:
			acknowledged = true
		case deliveryDisposed:
		default:
			remaining = true
		}
	}
	if remaining {
		return nil
	}
	next := deliveryDisposed
	if acknowledged {
		next = deliveryAcknowledged
	}
	if cohort.Status == next {
		return nil
	}
	cohort.Status = next
	return c.updateCohort(t, cohort)
}

// lastEventSequence returns the sequence of the last event appended by the
// current transaction, if any.
func lastEventSequence(t *txn) (int64, bool) {
	if len(t.events) == 0 {
		return 0, false
	}
	return t.events[len(t.events)-1].Sequence, true
}

// currentParentAttempt is the attempt a delegate callback belongs to: the active
// attempt of a running parent, or the settled latest attempt of a parked one.
func currentParentAttempt(parent runRecord) string {
	if parent.ActiveAttemptID != "" {
		return parent.ActiveAttemptID
	}
	return parent.AttemptID
}

// parkParentForChildren moves a completed parent run back to waiting_children so
// a new generation can be delegated from it. It refuses anything that is not the
// latest, settled run of its thread, so an obsolete parent can never be revived
// while a newer run owns the thread.
func (c *Core) parkParentForChildren(t *txn, parent runRecord) (runRecord, error) {
	if parent.Status != statusCompleted || parent.ActiveAttemptID != "" {
		return runRecord{}, errorf(api.Conflict, "run %q is not active", parent.ID)
	}
	var latest string
	if err := t.queryRow(`SELECT id FROM runs WHERE thread_id = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, parent.ThreadID).Scan(&latest); err != nil {
		return runRecord{}, wrapInternal("read latest run", err)
	}
	if latest != parent.ID {
		return runRecord{}, errorf(api.Conflict,
			"run %q is not the latest run of thread %q", parent.ID, parent.ThreadID)
	}
	active, err := activeRunID(t, parent.ThreadID)
	if err != nil {
		return runRecord{}, err
	}
	if active != "" {
		return runRecord{}, errorf(api.Conflict,
			"thread %q already has active run %q", parent.ThreadID, active)
	}
	parked := parent
	parked.Status = statusWaitingChildren
	parked.UpdatedAt = time.Now().UTC()
	if _, err := t.exec(`UPDATE runs SET status = ?, updated_at = ?
		WHERE id = ? AND status = ? AND active_attempt_id = ''`,
		statusWaitingChildren, parked.UpdatedAt.UnixNano(), parent.ID, statusCompleted); err != nil {
		return runRecord{}, wrapInternal("park parent run", err)
	}
	if _, err := t.event(eventRunWaiting, parent.ThreadID, parent.ID, parent.AttemptID,
		runEventData{Run: parked.toAPI()}); err != nil {
		return runRecord{}, err
	}
	return parked, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstWorkspace(values ...*api.WorkspaceSpec) *api.WorkspaceSpec {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func taskTitle(message string) string {
	const limit = 60
	trimmed := strings.TrimSpace(strings.ReplaceAll(message, "\n", " "))
	runes := []rune(trimmed)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return trimmed
}

func lookupReceipt(t *txn, cmd api.Command) (api.Receipt, bool, error) {
	var (
		payload     string
		delivery    string
		threadID    string
		runID       string
		taskID      string
		deliveryID  string
		inputID     string
		requestID   string
		messageID   string
		generation  int64
		sequence    int64
		acceptedInt int
	)
	err := t.queryRow(`SELECT payload, thread_id, run_id, task_id, generation, delivery_message_id,
			input_id, request_id, message_id, sequence, accepted, delivery
		FROM command_receipts WHERE command_id = ?`, cmd.CommandID).
		Scan(&payload, &threadID, &runID, &taskID, &generation, &deliveryID,
			&inputID, &requestID, &messageID, &sequence, &acceptedInt, &delivery)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Receipt{}, false, nil
	}
	if err != nil {
		return api.Receipt{}, false, wrapInternal("read command receipt", err)
	}
	if payload == "" {
		// A receipt written before the payload column existed cannot be verified
		// against the actual command, so re-executing it is refused outright.
		return api.Receipt{}, false, errorf(api.Conflict,
			"commandId %q was recorded by an older version and its content cannot be verified; refusing to re-execute it",
			cmd.CommandID)
	}
	if payload != commandPayload(cmd) {
		return api.Receipt{}, false, errorf(api.Conflict,
			"commandId %q was already used with different content", cmd.CommandID)
	}
	return api.Receipt{
		CommandID:         cmd.CommandID,
		Delivery:          delivery,
		Accepted:          acceptedInt != 0,
		ThreadID:          threadID,
		RunID:             runID,
		Sequence:          sequence,
		TaskID:            taskID,
		Generation:        generation,
		DeliveryMessageID: deliveryID,
		InputID:           inputID,
		RequestID:         requestID,
		MessageID:         messageID,
	}, true, nil
}

func insertReceipt(t *txn, cmd api.Command, receipt api.Receipt) error {
	accepted := 0
	if receipt.Accepted {
		accepted = 1
	}
	// The legacy fingerprint column stays in the schema but is no longer written:
	// a repeated commandId is verified against the command JSON in payload.
	if _, err := t.exec(`INSERT INTO command_receipts
		(command_id, kind, fingerprint, thread_id, run_id, sequence, accepted, created_at,
		 payload, task_id, generation, delivery_message_id, input_id, request_id, message_id)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cmd.CommandID, string(cmd.Kind), receipt.ThreadID, receipt.RunID, receipt.Sequence,
		accepted, nowNanos(), commandPayload(cmd), receipt.TaskID, receipt.Generation,
		receipt.DeliveryMessageID, receipt.InputID, receipt.RequestID, receipt.MessageID); err != nil {
		return wrapInternal("record command receipt", err)
	}
	if receipt.Delivery != "" {
		if _, err := t.exec(`UPDATE command_receipts SET delivery = ? WHERE command_id = ?`, receipt.Delivery, cmd.CommandID); err != nil {
			return wrapInternal("record command delivery", err)
		}
	}
	return nil
}
