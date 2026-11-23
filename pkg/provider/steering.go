package provider

import (
	"context"

	"github.com/muq9738/Vantry/pkg/api"
)

// Steering is an optional control port. It must never launch an attempt or send
// an initial prompt. Core invokes Steer at most once per persisted command;
// implementations must also deduplicate by the supplied business identities.
// SteeringStatus is read-only, including after an observer or Core restart.
type Steering interface {
	CanSteer(Request) bool
	Steer(context.Context, api.SteeringMessage) (SteeringResult, error)
	SteeringStatus(context.Context, api.SteeringMessage) (SteeringResult, error)
}

type SteeringResult struct {
	Status      api.SteeringStatus `json:"status"`
	Disposition string             `json:"disposition,omitempty"`
	Error       string             `json:"error,omitempty"`
}
