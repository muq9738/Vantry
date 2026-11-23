package core

import (
	"github.com/muq9738/Vantry/pkg/api"
)

// The unified inputs projection replaced the two integration branches' own
// queue and steering tables. Older clients still subscribe to message.updated /
// message.steering and read Snapshot.Messages / Snapshot.Steering, so those are
// derived from the same input rows: one durable authority, two projections. A
// derived projection never schedules or resends anything.

const (
	eventMessageUpdated  = "message.updated"
	eventSteeringUpdated = "message.steering"
)

type messageEventData struct {
	Message api.QueuedMessage `json:"message"`
}

type steeringEventData struct {
	Steering api.SteeringMessage `json:"steering"`
}

// legacyMessageStatus maps an input status onto the queue vocabulary older
// clients used. A queued input is still pending to them.
func legacyMessageStatus(status string) string {
	switch status {
	case inputQueued:
		return "pending"
	default:
		return status
	}
}

// toQueuedMessage is the read-only legacy view of a queue input.
func (r inputRecord) toQueuedMessage() api.QueuedMessage {
	return api.QueuedMessage{
		ID:          r.ID,
		ThreadID:    r.ThreadID,
		Message:     r.Message,
		Attachments: r.Attachments,
		Status:      legacyMessageStatus(r.Status),
		RunID:       r.RunID,
		CreatedAt:   r.CreatedAt,
	}
}

// toSteeringMessage is the read-only legacy view of a steer input. Delivered
// means Pi acknowledged the RPC, never that the model consumed it; unknown means
// the outcome could not be confirmed and the message is never resent.
func (r inputRecord) toSteeringMessage() api.SteeringMessage {
	status := api.SteeringPending
	switch r.Status {
	case inputDelivered:
		status = api.SteeringAccepted
	case inputFailed:
		status = api.SteeringRejected
	case inputUnknown:
		status = api.SteeringUnknown
	case inputPending:
		status = api.SteeringPending
	default:
		// A cancelled steer was withdrawn before it was handed off.
		status = api.SteeringRejected
	}
	return api.SteeringMessage{
		ID:          r.ID,
		CommandID:   r.CommandID,
		ThreadID:    r.ThreadID,
		RunID:       r.RunID,
		AttemptID:   r.AttemptID,
		Message:     r.Message,
		Attachments: r.Attachments,
		Status:      status,
		Disposition: r.Disposition,
		Error:       r.Error,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// checkAttachments validates Core-owned attachment identities before they are
// recorded, so an unavailable attachment is refused instead of failing later
// inside a provider.
func (c *Core) checkAttachments(items []api.Attachment) error {
	if len(items) == 0 {
		return nil
	}
	if len(items) > maxAttachments {
		return errorf(api.InvalidCommand, "最多发送 %d 个附件", maxAttachments)
	}
	if c.validateAttachments == nil {
		return errorf(api.InvalidCommand, "当前 Core 不支持附件")
	}
	return c.validateAttachments(items)
}

const maxAttachments = 8

// unconfirmedControlReason explains a steer whose delivery Pi never confirmed.
const unconfirmedControlReason = "Pi did not confirm delivery; the message will not be resent"
