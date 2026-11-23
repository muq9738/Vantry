package api

import "time"

// SearchQuery searches literal text across thread metadata and run messages.
// An empty query returns no hits. Limit defaults to 50 when zero.
type SearchQuery struct {
	Query           string `json:"query"`
	IncludeArchived bool   `json:"includeArchived,omitempty"`
	ThreadID        string `json:"threadId,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}

type SearchField string

const (
	SearchTitle     SearchField = "title"
	SearchBranch    SearchField = "branch"
	SearchWorkspace SearchField = "workspace"
	SearchInput     SearchField = "input"
	SearchOutput    SearchField = "output"
)

// SearchHit identifies a matching field. RunID is set for a run's message or
// workspace. UpdatedAt is the thread's latest run activity (or creation time).
// Score orders titles before branches/workspaces, then message bodies.
type SearchHit struct {
	ThreadID  string      `json:"threadId"`
	RunID     string      `json:"runId,omitempty"`
	Title     string      `json:"title"`
	Field     SearchField `json:"field"`
	Snippet   string      `json:"snippet"`
	Score     int         `json:"score"`
	UpdatedAt time.Time   `json:"updatedAt"`
}
