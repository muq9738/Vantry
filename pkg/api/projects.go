package api

import (
	"path/filepath"
	"strings"
	"time"
)

// WorkspaceProjectPath groups a checkout and its linked worktrees by the
// repository they belong to. Chat-only threads have no project.
func WorkspaceProjectPath(spec *WorkspaceSpec) string {
	if spec == nil {
		return ""
	}
	path := strings.TrimSpace(spec.Repository)
	if path == "" {
		path = strings.TrimSpace(spec.Path)
	}
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

// ProjectUpdate is a durable milestone delivered to a project's coordinator
// chat. Sequence and ID identify its first source event. Run, attempt, status
// and request identities prevent repeated recovery milestones. Status is a run status or needs_input for a pending provider dialog.
type ProjectUpdate struct {
	ID             string    `json:"id"`
	Sequence       int64     `json:"sequence"`
	ThreadID       string    `json:"threadId"`
	SourceThreadID string    `json:"sourceThreadId"`
	RunID          string    `json:"runId"`
	AttemptID      string    `json:"attemptId,omitempty"`
	RequestID      string    `json:"requestId,omitempty"`
	Title          string    `json:"title"`
	Status         string    `json:"status"`
	Summary        string    `json:"summary,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}
