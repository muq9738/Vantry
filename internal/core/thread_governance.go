package core

import (
	"context"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func enforceCeiling(caller threadRecord, policy, mode string) error {
	callerPolicy, err := normalizeApprovalPolicy(caller.ApprovalPolicy)
	if err != nil {
		return err
	}
	policy, err = normalizeApprovalPolicy(policy)
	if err != nil {
		return err
	}
	callerMode, err := normalizeInteractionMode(caller.InteractionMode)
	if err != nil {
		return err
	}
	mode, err = normalizeInteractionMode(mode)
	if err != nil {
		return err
	}
	ranks := map[string]int{approvalAsk: 0, approvalAcceptEdits: 1, approvalAuto: 2}
	if ranks[policy] > ranks[callerPolicy] || callerMode == "plan" && mode == interactionDefault {
		return errorf(api.Conflict, "requested thread permissions exceed caller thread %q", caller.ID)
	}
	return nil
}

func wakeThread(t *txn, threadID string) error {
	thread, err := loadThread(t, threadID)
	if err != nil {
		return wrapInternal("read waking thread", err)
	}
	if thread.SnoozedUntil == nil {
		return nil
	}
	if _, err := t.exec(`UPDATE threads SET snoozed_until = 0 WHERE id = ?`, threadID); err != nil {
		return wrapInternal("wake thread", err)
	}
	thread.SnoozedUntil = nil
	_, err = t.event(eventThreadUpdated, threadID, "", "", threadEventData{Thread: thread.toAPI()})
	return err
}

func (c *Core) dispatchInterThreadSend(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	targetID := firstNonEmpty(cmd.TargetThreadID, cmd.ThreadID)
	if strings.TrimSpace(targetID) == "" || strings.TrimSpace(cmd.Message) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "target thread and message are required")
	}
	mode := firstNonEmpty(cmd.DeliveryMode, "auto")
	if mode != "auto" && mode != "queue" && mode != "steer" {
		return api.Receipt{}, errorf(api.InvalidCommand, "unsupported delivery mode %q", mode)
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
		target, err := requiredThread(t, targetID)
		if err != nil {
			return err
		}
		var sender threadRecord
		if cmd.SenderThreadID != "" {
			sender, err = requiredThread(t, cmd.SenderThreadID)
			if err != nil {
				return err
			}
			if err := enforceCeiling(sender, target.ApprovalPolicy, target.InteractionMode); err != nil {
				return err
			}
		}
		if target.Archived {
			return errorf(api.Conflict, "thread %q is archived", target.ID)
		}
		if err := wakeThread(t, target.ID); err != nil {
			return err
		}
		active, err := activeRunID(t, target.ID)
		if err != nil {
			return err
		}
		if active == "" {
			if mode == "steer" && !cmd.FallbackToQueue {
				return errorf(api.Conflict, "thread %q has no running attempt to steer", target.ID)
			}
			// Use the same FIFO as normal messages, including any older backlog.
			receipt, err = c.acceptQueuedMessage(t, target, cmd)
			if err != nil {
				return err
			}
			receipt.Delivery = "queued"
			if receipt.RunID != "" {
				receipt.Delivery = "started"
			}
		} else {
			run, err := loadRun(t, active)
			if err != nil {
				return wrapInternal("read conversation run", err)
			}
			steered := false
			if mode != "queue" && run.Status == statusRunning && run.ActiveAttemptID != "" {
				request, steerErr := c.steerRequest(t, run, run.ActiveAttemptID, cmd.Attachments)
				if steerErr == nil {
					request.SenderThreadID, request.SenderTitle = cmd.SenderThreadID, sender.Title
					input := inputRecord{ID: newID("inp"), ThreadID: target.ID, SenderThreadID: cmd.SenderThreadID,
						RunID: run.ID, AttemptID: run.ActiveAttemptID, Kind: inputKindSteer,
						Message: cmd.Message, Attachments: cmd.Attachments, CommandID: cmd.CommandID, Status: inputPending}
					if err := c.insertInput(t, input); err != nil {
						return err
					}
					if err := c.queueControl(t, request, provider.Control{ID: input.ID, Kind: "steer", Message: input.Message, Attachments: input.Attachments}); err != nil {
						return err
					}
					receipt = api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: target.ID,
						RunID: run.ID, InputID: input.ID, MessageID: input.ID, Delivery: "steered"}
					steered = true
				} else if mode == "steer" && !cmd.FallbackToQueue {
					return steerErr
				}
			}
			if !steered {
				if mode == "steer" && !cmd.FallbackToQueue {
					return errorf(api.Conflict, "run %q has no running attempt to steer", run.ID)
				}
				receipt, err = c.acceptQueuedMessage(t, target, cmd)
				if err != nil {
					return err
				}
				receipt.Delivery = "queued"
			}
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
