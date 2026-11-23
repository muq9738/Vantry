package pi

import "errors"

var errOrphanPending = errors.New("provider worker exited; waiting for its execution to release the workspace")

type piProcess struct {
	AttemptID    string `json:"attemptId"`
	PID          int    `json:"pid"`
	ProcessStart string `json:"processStart,omitempty"`
	Groups       []int  `json:"groups,omitempty"`
}
