package api

// MCPServerInfo describes configuration and optional on-demand live probe state,
// never claiming live connection unless Probed is true. Credentials, command
// arguments and headers remain on the owning Core host.
type MCPServerInfo struct {
	Name         string            `json:"name"`
	Scope        string            `json:"scope"`
	Transport    string            `json:"transport"`
	Endpoint     string            `json:"endpoint,omitempty"`
	Enabled      bool              `json:"enabled"`
	Exposure     string            `json:"exposure"`
	ToolExposure map[string]string `json:"toolExposure,omitempty"`
	Description  string            `json:"description,omitempty"`
	Error        string            `json:"error,omitempty"`
	LiveState    string            `json:"liveState,omitempty"`
	Tools        []string          `json:"tools,omitempty"`
	LiveError    string            `json:"liveError,omitempty"`
}

type MCPCatalog struct {
	Workspace       string          `json:"workspace,omitempty"`
	UserFile        string          `json:"userFile,omitempty"`
	ProjectFile     string          `json:"projectFile,omitempty"`
	ProjectTrusted  bool            `json:"projectTrusted"`
	CodemodeDefault bool            `json:"codemodeDefault"`
	Probed          bool            `json:"probed,omitempty"`
	ProbeError      string          `json:"probeError,omitempty"`
	Servers         []MCPServerInfo `json:"servers"`
	Notice          string          `json:"notice"`
}
