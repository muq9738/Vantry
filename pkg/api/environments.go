package api

// EnvironmentInfo is reported by the Core that owns the projects and agents.
// Its ID is an instance identity; versions are descriptive only.
type EnvironmentInfo struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	OS           string         `json:"os"`
	Arch         string         `json:"arch"`
	Home         string         `json:"home"`
	ProviderID   string         `json:"providerId"`
	DefaultModel string         `json:"defaultModel"`
	Workspace    *WorkspaceSpec `json:"workspace,omitempty"`
}

type EnvironmentDirectory struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type EnvironmentDirectories struct {
	Path    string                 `json:"path"`
	Parent  string                 `json:"parent"`
	Entries []EnvironmentDirectory `json:"entries"`
}
