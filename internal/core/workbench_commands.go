package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// dispatchQueueMessage appends one message to the thread's durable FIFO. The
// thread has at most one active run (waiting_children included); when that slot
// is free the message starts immediately, otherwise it waits in order. Starting
// the next item and updating the queue state happen in the same transaction.
func (c *Core) dispatchQueueMessage(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", api.QueueMessage)
	}
	if strings.TrimSpace(cmd.Message) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "message is required for %s", api.QueueMessage)
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
		receipt, err = c.acceptQueuedMessage(t, thread, cmd)
		if err != nil {
			return err
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	if existing.CommandID != "" {
		return existing, nil
	}
	c.notifyWorker()
	return receipt, nil
}

// dispatchCancelInput cancels one message that has not started yet. A message
// that already became a run cannot be withdrawn here; run.stop is the only way to
// cancel an execution.
func (c *Core) dispatchCancelInput(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	// message.cancel is the legacy alias and addresses the same row by messageId.
	targetID := strings.TrimSpace(cmd.InputID)
	if targetID == "" {
		targetID = strings.TrimSpace(cmd.MessageID)
	}
	if targetID == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "inputId is required for %s", api.CancelInput)
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

		input, err := loadInput(t, targetID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "input %q not found", targetID)
		}
		if err != nil {
			return wrapInternal("read input", err)
		}
		if cmd.ThreadID != "" && cmd.ThreadID != input.ThreadID {
			return errorf(api.NotFound, "input %q does not belong to thread %q", input.ID, cmd.ThreadID)
		}
		if input.Kind != inputKindQueue {
			return errorf(api.Conflict, "input %q is a steer and cannot be cancelled", input.ID)
		}

		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  input.ThreadID,
			RunID:     input.RunID,
			InputID:   input.ID,
			MessageID: input.ID,
		}
		switch input.Status {
		case inputCancelled:
			// Already withdrawn: no new state and no new event.
		case inputQueued:
			input.Status, input.Error = inputCancelled, "queue cancelled by input.cancel"
			if err := c.updateInput(t, input); err != nil {
				return err
			}
			if err := c.reindexQueuedInputs(t, input.ThreadID); err != nil {
				return err
			}
			if seq, ok := lastEventSequence(t); ok {
				receipt.Sequence = seq
			}
		default:
			return errorf(api.Conflict,
				"input %q already started as run %q and cannot be cancelled",
				input.ID, input.RunID)
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

// dispatchSteer binds an instruction to the current running attempt. A
// thread-level steer (no explicit attempt) also accepts a completed/idle target
// by queueing the instruction atomically with the same command receipt.
func (c *Core) dispatchSteer(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", api.SteerRun)
	}
	if strings.TrimSpace(cmd.RunID) == "" && strings.TrimSpace(cmd.ParentAttemptID) != "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "runId is required when an attempt is specified")
	}
	if strings.TrimSpace(cmd.Message) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "message is required for %s", cmd.Kind)
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
			return wrapInternal("read steer thread", err)
		}
		if thread.Archived {
			return errorf(api.Conflict, "thread %q is archived", thread.ID)
		}
		runID := strings.TrimSpace(cmd.RunID)
		if runID == "" {
			runID, err = activeRunID(t, thread.ID)
			if err != nil {
				return err
			}
			if runID == "" {
				receipt, err = c.acceptQueuedMessage(t, thread, cmd)
				if err != nil {
					return err
				}
				return insertReceipt(t, cmd, receipt)
			}
		}
		run, err := loadRun(t, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "run %q not found", runID)
		}
		if err != nil {
			return wrapInternal("read run", err)
		}
		if run.ThreadID != cmd.ThreadID {
			return errorf(api.NotFound, "run %q does not belong to thread %q", run.ID, cmd.ThreadID)
		}
		if run.Status == statusCompleted && (cmd.ParentAttemptID == "" || cmd.FallbackToQueue) {
			receipt, err = c.acceptQueuedMessage(t, thread, cmd)
			if err != nil {
				return err
			}
			return insertReceipt(t, cmd, receipt)
		}
		if run.Status != statusRunning || run.ActiveAttemptID == "" {
			return errorf(api.Conflict, "run %q has no running attempt to steer", run.ID)
		}
		// The legacy message.steer alias names no attempt; it is resolved once,
		// here, and the resolved attempt is what the input is bound to.
		attemptID := strings.TrimSpace(cmd.ParentAttemptID)
		if attemptID == "" {
			attemptID = run.ActiveAttemptID
		}
		if attemptID != run.ActiveAttemptID {
			return errorf(api.Conflict,
				"attempt %q is no longer current for run %q (current %q)",
				attemptID, run.ID, run.ActiveAttemptID)
		}
		request, err := c.steerRequest(t, run, attemptID, cmd.Attachments)
		if err != nil {
			return err
		}
		input := inputRecord{
			ID:             newID("inp"),
			ThreadID:       run.ThreadID,
			SenderThreadID: cmd.SenderThreadID,
			RunID:          run.ID,
			AttemptID:      attemptID,
			Kind:           inputKindSteer,
			Message:        cmd.Message,
			Attachments:    cmd.Attachments,
			CommandID:      cmd.CommandID,
			Status:         inputPending,
		}
		if err := c.insertInput(t, input); err != nil {
			return err
		}
		if err := c.queueControl(t, request, provider.Control{
			ID:          input.ID,
			Kind:        "steer",
			Message:     input.Message,
			Attachments: input.Attachments,
		}); err != nil {
			return err
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  run.ThreadID,
			RunID:     run.ID,
			InputID:   input.ID,
			MessageID: input.ID,
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
	c.notifyWorker()
	return receipt, nil
}

// dispatchRespond answers one pending dialog of the current active attempt. The
// request becomes responding immediately, but only control_result makes it
// responded; a second command can never overwrite an accepted response.
func (c *Core) dispatchRespond(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.RequestID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "requestId is required for %s", api.RespondRequest)
	}
	if _, ok := c.provider.(provider.Controller); !ok {
		return api.Receipt{}, errorf(api.InvalidCommand,
			"provider %T does not support request.respond", c.provider)
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

		request, err := loadRequest(t, cmd.RequestID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "request %q not found", cmd.RequestID)
		}
		if err != nil {
			return wrapInternal("read provider request", err)
		}
		run, err := loadRun(t, request.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "run %q of request %q not found", request.RunID, request.ID)
		}
		if err != nil {
			return wrapInternal("read run", err)
		}
		if run.Status != statusRunning || run.ActiveAttemptID != request.AttemptID {
			return errorf(api.Conflict,
				"request %q belongs to an attempt that is no longer active", request.ID)
		}
		if !request.ExpiresAt.IsZero() && !request.ExpiresAt.After(time.Now()) {
			return errorf(api.Conflict, "request %q has expired", request.ID)
		}
		if request.Status != requestPending {
			return errorf(api.Conflict,
				"request %q is already %s and cannot be answered again", request.ID, request.Status)
		}
		if err := validateResponse(request, cmd); err != nil {
			return err
		}

		request.Status, request.Error = requestResponding, ""
		if err := c.updateRequest(t, request); err != nil {
			return err
		}
		providerRequest, err := providerRequestForAttempt(t, run, request.AttemptID)
		if err != nil {
			return err
		}
		if err := c.queueControl(t, providerRequest, provider.Control{
			ID:        request.ID,
			Kind:      "response",
			RequestID: request.ProviderRequestID,
			Value:     cmd.Value,
			Confirmed: cmd.Confirmed,
			Cancelled: cmd.Cancelled,
		}); err != nil {
			return err
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  request.ThreadID,
			RunID:     request.RunID,
			RequestID: request.ID,
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
	c.notifyWorker()
	return receipt, nil
}

// validateResponse rejects a response the provider could not carry out: a select
// value must be one of the offered options. Empty input/editor text is a valid
// answer (distinct from Cancelled), matching Pi's UI response protocol.
func validateResponse(request requestRecord, cmd api.Command) error {
	if cmd.Cancelled {
		return nil
	}
	switch request.Method {
	case "confirm":
		// Confirmed=false is the documented denial; both answers are valid.
		return nil
	case "select":
		for _, option := range request.Options {
			if option == cmd.Value {
				return nil
			}
		}
		return errorf(api.InvalidCommand,
			"value %q is not one of the options of request %q", cmd.Value, request.ID)
	case "input", "editor":
		return nil
	default:
		return errorf(api.Conflict, "request %q has unsupported method %q", request.ID, request.Method)
	}
}
