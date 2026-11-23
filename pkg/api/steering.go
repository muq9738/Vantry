package api

import "time"

// Steering is bound to the attempt current when message.steer is accepted.
// Receipt.Accepted means Core persisted the command, not that Pi consumed it.
type SteeringStatus string

const (
	SteeringPending  SteeringStatus = "pending"
	SteeringSending  SteeringStatus = "sending"
	SteeringAccepted SteeringStatus = "accepted"
	SteeringRejected SteeringStatus = "rejected"
	SteeringUnknown  SteeringStatus = "unknown"
)

// Accepted confirms only Pi's RPC acknowledgement (queued or handled). It does
// not confirm a model consumed the message, including when a stop wins later.
// Unknown means delivery cannot be confirmed; Core never automatically resends.
// An identical commandId retry always returns its original receipt. Subscribe
// to message.steering or read Snapshot.Steering for subsequent delivery state.
type SteeringMessage struct {
	ID          string         `json:"id"`
	CommandID   string         `json:"commandId"`
	ThreadID    string         `json:"threadId"`
	RunID       string         `json:"runId"`
	AttemptID   string         `json:"attemptId"`
	Message     string         `json:"message"`
	Attachments []Attachment   `json:"attachments,omitempty"`
	Status      SteeringStatus `json:"status"`
	Disposition string         `json:"disposition,omitempty"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}
