package api

import (
	"context"
	"encoding/json"
	"time"
)

// Input is either a durable FIFO message or an instruction addressed to one
// active attempt. Delivered means the provider accepted it, not model completion.
// InputConfiguration captures the settings at queue acceptance. A nil value
// on historical inputs retains their legacy start-time defaults.
type InputConfiguration struct {
	ProviderID      string `json:"providerId"`
	Model           string `json:"model"`
	ApprovalPolicy  string `json:"approvalPolicy"`
	InteractionMode string `json:"interactionMode,omitempty"`
}

type Input struct {
	CommandID      string              `json:"commandId,omitempty"`
	Configuration  *InputConfiguration `json:"configuration,omitempty"`
	ID             string              `json:"id"`
	ThreadID       string              `json:"threadId"`
	SenderThreadID string              `json:"senderThreadId,omitempty"`
	RunID          string              `json:"runId,omitempty"`
	AttemptID      string              `json:"attemptId,omitempty"`
	Kind           string              `json:"kind"` // queue | steer
	Message        string              `json:"message"`
	Attachments    []Attachment        `json:"attachments,omitempty"`
	Status         string              `json:"status"`   // queued | pending | started | delivered | cancelled | failed | unknown
	Position       int                 `json:"position"` // zero-based queue order; meaningful while queued
	Error          string              `json:"error,omitempty"`
	CreatedAt      time.Time           `json:"createdAt"`
	UpdatedAt      time.Time           `json:"updatedAt"`
}

// ToolCall is a projection of real provider tool events, keyed by attempt and
// provider tool-call identity. Output is bounded display text, never executable.
type ToolCall struct {
	ID        string          `json:"id"`
	ThreadID  string          `json:"threadId"`
	RunID     string          `json:"runId"`
	AttemptID string          `json:"attemptId"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
	Status    string          `json:"status"` // running | completed | failed | interrupted
	UpdatedAt time.Time       `json:"updatedAt"`
	// OutputOffset is the byte length of the run's output when the call
	// started, where it belongs among the reply's text; -1 when unknown.
	OutputOffset int `json:"outputOffset"`
}

// Older servers/events omitted outputOffset. Zero is a real offset (before
// any reply text), so keep unknown distinct when reading historical records.
func (t *ToolCall) UnmarshalJSON(data []byte) error {
	type wire ToolCall
	value := wire{OutputOffset: -1}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*t = ToolCall(value)
	return nil
}

// ProviderRequest records a Pi RPC dialog. ID is scoped to its attempt;
// ProviderRequestID is the original ID required by extension_ui_response.
type ProviderRequest struct {
	ID                string    `json:"id"`
	ProviderRequestID string    `json:"providerRequestId"`
	ThreadID          string    `json:"threadId"`
	RunID             string    `json:"runId"`
	AttemptID         string    `json:"attemptId"`
	Method            string    `json:"method"` // confirm | select | input | editor
	Title             string    `json:"title"`
	Message           string    `json:"message,omitempty"`
	Options           []string  `json:"options,omitempty"`
	Prefill           string    `json:"prefill,omitempty"`
	Status            string    `json:"status"` // pending | responding | responded | expired | failed
	Error             string    `json:"error,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type WorkspaceFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type WorkspaceChanges struct {
	Path      string          `json:"path"`
	Branch    string          `json:"branch,omitempty"`
	IsGit     bool            `json:"isGit"`
	Files     []WorkspaceFile `json:"files"`
	Diff      string          `json:"diff"`
	Truncated bool            `json:"truncated"`
}

// WorkbenchQueries exposes the prompt-free model catalog and workspace queries.
// Catalog lookup is independent of persisted conversations.
type WorkbenchQueries interface {
	ListModels(context.Context) ([]ModelInfo, error)
	InspectWorkspace(context.Context, string) (WorkspaceChanges, error)
	OpenTerminal(context.Context, string) error
}
