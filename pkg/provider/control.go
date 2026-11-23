package provider

import (
	"context"
	"github.com/muq9738/Vantry/pkg/api"
)

// Control is an idempotent intent addressed to one active attempt. Persisting
// it is not acknowledgement from Pi: a later control_result event reports that.
type Control struct {
	ID          string           `json:"id"`
	Kind        string           `json:"kind"` // steer | response
	Message     string           `json:"message,omitempty"`
	Attachments []api.Attachment `json:"attachments,omitempty"`
	RequestID   string           `json:"requestId,omitempty"` // Pi's original dialog ID
	Value       string           `json:"value,omitempty"`
	Confirmed   bool             `json:"confirmed,omitempty"`
	Cancelled   bool             `json:"cancelled,omitempty"`
}

// Controller must not resend a control already written to Pi. Core retries
// durable handoff until accepted or the target attempt has settled.
type Controller interface {
	Control(context.Context, Request, Control) error
}

// ControlRouter reports control support when one provider routes requests to
// multiple executors. A single-executor Controller already declares support.
type ControlRouter interface {
	SupportsControl(Request) bool
}

type ControlResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // delivered | failed | unknown
	Error  string `json:"error,omitempty"`
}

type ModelCatalog interface {
	Models(context.Context) ([]api.ModelInfo, error)
}
