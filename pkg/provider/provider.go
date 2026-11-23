// Package provider defines the narrow execution port used by the first Core.
// Persistent run state belongs to Core; providers emit only uncommitted deltas.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

type Request struct {
	ThreadID        string
	SenderThreadID  string
	SenderTitle     string
	ParentTaskID    string
	RunID           string
	AttemptID       string
	Message         string
	Attachments     []api.Attachment
	ProviderID      string
	Model           string
	Workspace       *api.WorkspaceSpec
	ApprovalPolicy  string
	InteractionMode string
	// AfterSequence is the last provider event committed for this attempt.
	AfterSequence int64
}

type Provider interface {
	Run(ctx context.Context, request Request, emit func(delta string) error) error
}

// Event sequences belong to a single attempt and are stable across attachments.
// Core commits the cursor and the corresponding output in the same transaction.
type Event struct {
	OccurredAt time.Time       `json:"occurredAt,omitempty"`
	Sequence   int64           `json:"sequence"`
	Delta      string          `json:"delta,omitempty"`
	Type       string          `json:"type,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// ErrDetached means the observer stopped while durable execution remains owned
// by its independent worker. It must not terminalize a still-recoverable run.
var ErrDetached = errors.New("provider observer detached")

// ErrExecutionCancelled reports an explicit cancellation by the execution owner.
// Detaching an observer remains a separate operation.
var ErrExecutionCancelled = errors.New("provider execution cancelled")

// Durable providers can attach to an already submitted attempt without sending
// its prompt again. Stop is an explicit persistent cancellation request; merely
// cancelling the observing context must not kill the independent execution.
type Durable interface {
	Provider
	RunEvents(context.Context, Request, func(Event) error) error
	Recoverable(Request) bool
	Stop(context.Context, Request) error
}
