package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// acceptQueuedMessage is shared by queue, merge-back and the steer completion
// fallback. The caller records the original command receipt in this same
// transaction; a fallback never dispatches a second command or changes its ID.
func (c *Core) acceptQueuedMessage(t *txn, thread threadRecord, cmd api.Command) (api.Receipt, error) {
	if thread.Archived {
		return api.Receipt{}, errorf(api.Conflict, "thread %q is archived; restore it before queueing a message", thread.ID)
	}
	if err := c.checkAttachments(cmd.Attachments); err != nil {
		return api.Receipt{}, err
	}
	mode, err := normalizeInteractionMode(firstNonEmpty(cmd.InteractionMode, thread.InteractionMode))
	if err != nil {
		return api.Receipt{}, err
	}
	input := inputRecord{
		ID: newID("inp"), ThreadID: thread.ID, SenderThreadID: cmd.SenderThreadID, Kind: inputKindQueue,
		Message: cmd.Message, Attachments: cmd.Attachments, CommandID: cmd.CommandID, Status: inputQueued,
		Configuration: &api.InputConfiguration{
			ProviderID: thread.ProviderID, Model: thread.Model,
			ApprovalPolicy: thread.ApprovalPolicy, InteractionMode: mode,
		},
	}
	if err := c.insertInput(t, input); err != nil {
		return api.Receipt{}, err
	}
	// Also respects an older backlog left at the completion/recovery boundary.
	if _, err := c.startQueuedInput(t, thread.ID); err != nil {
		return api.Receipt{}, err
	}
	input, err = loadInput(t, input.ID)
	if err != nil {
		return api.Receipt{}, wrapInternal("read accepted queue input", err)
	}
	receipt := api.Receipt{
		CommandID: cmd.CommandID, Accepted: true, ThreadID: thread.ID,
		RunID: input.RunID, InputID: input.ID, MessageID: input.ID,
	}
	receipt.Sequence, _ = lastEventSequence(t)
	return receipt, nil
}

// startInputRun creates execution without rewriting the queue's accepted
// configuration. A nil configuration retains historical start-time defaults;
// an older non-nil configuration with no mode means default, just like the API.
func startInputRun(t *txn, thread threadRecord, input inputRecord) (inputRecord, error) {
	if config := input.Configuration; config != nil {
		thread.ProviderID, thread.Model, thread.ApprovalPolicy = config.ProviderID, config.Model, config.ApprovalPolicy
		thread.InteractionMode = config.InteractionMode
	}
	run, attemptID, _, err := createRun(t, runSeed{
		ThreadID: thread.ID, SenderThreadID: input.SenderThreadID, Input: input.Message, Attachments: input.Attachments,
		ProviderID: thread.ProviderID, Model: thread.Model, Workspace: thread.Workspace,
		ApprovalPolicy: thread.ApprovalPolicy, InteractionMode: thread.InteractionMode,
	})
	if err != nil {
		return inputRecord{}, err
	}
	input.RunID, input.AttemptID, input.Status = run.ID, attemptID, inputStarted
	return input, nil
}

func queuedInputs(t *txn, threadID string) ([]inputRecord, error) {
	rows, err := t.query(`SELECT `+inputColumns+` FROM inputs
		WHERE thread_id = ? AND kind = ? AND status = ? ORDER BY queue_order, created_at, id`,
		threadID, inputKindQueue, inputQueued)
	if err != nil {
		return nil, wrapInternal("read queue order", err)
	}
	defer rows.Close()
	var inputs []inputRecord
	for rows.Next() {
		input, err := scanInput(rows)
		if err != nil {
			return nil, wrapInternal("scan queue order", err)
		}
		inputs = append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapInternal("read queue order", err)
	}
	return inputs, nil
}

func (c *Core) saveQueueOrder(t *txn, inputs []inputRecord) error {
	for position, input := range inputs {
		if input.Position == position {
			continue
		}
		input.Position = position
		if err := c.updateInput(t, input); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) reindexQueuedInputs(t *txn, threadID string) error {
	inputs, err := queuedInputs(t, threadID)
	if err != nil {
		return err
	}
	return c.saveQueueOrder(t, inputs)
}

// steerRequest validates the current execution route before any queue item is
// converted into a control. A rejected promotion leaves the original row intact.
func (c *Core) steerRequest(t *txn, run runRecord, attemptID string, attachments []api.Attachment) (provider.Request, error) {
	if _, ok := c.provider.(provider.Controller); !ok {
		return provider.Request{}, errorf(api.InvalidCommand, "provider %T does not support run.steer", c.provider)
	}
	if err := c.checkAttachments(attachments); err != nil {
		return provider.Request{}, err
	}
	request, err := providerRequestForAttempt(t, run, attemptID)
	if err != nil {
		return provider.Request{}, err
	}
	if router, ok := c.provider.(provider.ControlRouter); ok && !router.SupportsControl(request) {
		return provider.Request{}, errorf(api.InvalidCommand, "provider route does not support run.steer")
	}
	return request, nil
}

func (c *Core) dispatchQueueOperation(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	targetID := firstNonEmpty(strings.TrimSpace(cmd.InputID), strings.TrimSpace(cmd.MessageID))
	if targetID == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "inputId is required for %s", cmd.Kind)
	}
	if (cmd.Kind == api.EditInput || cmd.Kind == "input.update") && strings.TrimSpace(cmd.Message) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "message is required for %s", cmd.Kind)
	}
	if cmd.Kind == api.EditInput && cmd.Value == "replace-attachments" {
		if err := c.checkAttachments(cmd.Attachments); err != nil {
			return api.Receipt{}, err
		}
	}
	if cmd.Kind == api.ReorderInput && cmd.Position < 0 {
		return api.Receipt{}, errorf(api.InvalidCommand, "position must be zero or greater")
	}
	if cmd.Kind == "input.move" && cmd.Value != "up" && cmd.Value != "down" {
		return api.Receipt{}, errorf(api.InvalidCommand, "value must be up or down for %s", cmd.Kind)
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
		input, err := loadInput(t, targetID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "input %q not found", targetID)
		}
		if err != nil {
			return wrapInternal("read queue input", err)
		}
		if cmd.ThreadID != "" && input.ThreadID != cmd.ThreadID {
			return errorf(api.NotFound, "input %q does not belong to thread %q", input.ID, cmd.ThreadID)
		}
		if input.Kind != inputKindQueue || input.Status != inputQueued {
			return errorf(api.Conflict, "input %q is not queued", input.ID)
		}
		switch cmd.Kind {
		case api.EditInput, "input.update":
			input.Message = cmd.Message
			if cmd.Value == "replace-attachments" {
				input.Attachments = cmd.Attachments
			}
			if err := c.updateInput(t, input); err != nil {
				return err
			}
		case api.ReorderInput, "input.move":
			inputs, err := queuedInputs(t, input.ThreadID)
			if err != nil {
				return err
			}
			targetPos := cmd.Position
			if cmd.Kind == "input.move" {
				currentIdx := -1
				for i, queued := range inputs {
					if queued.ID == input.ID {
						currentIdx = i
						break
					}
				}
				if currentIdx < 0 {
					return errorf(api.Conflict, "input %q is not queued", input.ID)
				}
				if cmd.Value == "up" {
					targetPos = max(0, currentIdx-1)
				} else {
					targetPos = min(len(inputs)-1, currentIdx+1)
				}
			}
			if targetPos >= len(inputs) {
				return errorf(api.InvalidCommand, "position %d is outside the queue of %d inputs", targetPos, len(inputs))
			}
			var ordered []inputRecord
			for _, queued := range inputs {
				if queued.ID != input.ID {
					ordered = append(ordered, queued)
				}
			}
			ordered = append(ordered, inputRecord{})
			copy(ordered[targetPos+1:], ordered[targetPos:])
			ordered[targetPos] = input
			if err := c.saveQueueOrder(t, ordered); err != nil {
				return err
			}
		case api.PromoteInputToSteer:
			input, err = c.promoteQueuedInput(t, input, cmd)
			if err != nil {
				return err
			}
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID, Accepted: true, ThreadID: input.ThreadID,
			RunID: input.RunID, InputID: input.ID, MessageID: input.ID,
		}
		receipt.Sequence, _ = lastEventSequence(t)
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	c.notifyWorker()
	return receipt, nil
}

func (c *Core) promoteQueuedInput(t *txn, input inputRecord, cmd api.Command) (inputRecord, error) {
	thread, err := loadThread(t, input.ThreadID)
	if err != nil {
		return inputRecord{}, wrapInternal("read queue thread", err)
	}
	if thread.Archived {
		return inputRecord{}, errorf(api.Conflict, "thread %q is archived", thread.ID)
	}
	active, err := activeRunID(t, thread.ID)
	if err != nil {
		return inputRecord{}, err
	}
	if active == "" {
		// The execution ended before the promotion acquired the writer. Preserve
		// the same input identity and launch it as the explicitly selected next run.
		input, err = startInputRun(t, thread, input)
		if err != nil {
			return inputRecord{}, err
		}
	} else {
		run, err := loadRun(t, active)
		if err != nil {
			return inputRecord{}, wrapInternal("read active queue run", err)
		}
		if run.Status != statusRunning || run.ActiveAttemptID == "" {
			return inputRecord{}, errorf(api.Conflict, "run %q has no running attempt to steer", run.ID)
		}
		request, err := c.steerRequest(t, run, run.ActiveAttemptID, input.Attachments)
		if err != nil {
			return inputRecord{}, err
		}
		input.Kind, input.Status = inputKindSteer, inputPending
		input.RunID, input.AttemptID = run.ID, run.ActiveAttemptID
		input.CommandID, input.Position = cmd.CommandID, 0
		if err := c.queueControl(t, request, provider.Control{
			ID: input.ID, Kind: "steer", Message: input.Message, Attachments: input.Attachments,
		}); err != nil {
			return inputRecord{}, err
		}
	}
	if err := c.updateInput(t, input); err != nil {
		return inputRecord{}, err
	}
	if err := c.reindexQueuedInputs(t, thread.ID); err != nil {
		return inputRecord{}, err
	}
	return input, nil
}
