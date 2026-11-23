package api

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Attachment is a Core-owned copy. IDs, never client paths, identify uploaded files.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
}

type QueuedMessage struct {
	ID          string       `json:"id"`
	ThreadID    string       `json:"threadId"`
	Message     string       `json:"message"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Status      string       `json:"status"`
	RunID       string       `json:"runId,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
}

type ModelInfo struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Name          string   `json:"name"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	ContextWindow int64    `json:"contextWindow"`
	MaxTokens     int64    `json:"maxTokens"`
}

func (m ModelInfo) Key() string {
	if m.Provider == "" || strings.HasPrefix(m.ID, m.Provider+"/") {
		return m.ID
	}
	return m.Provider + "/" + m.ID
}

type ModelCatalog struct {
	Models         []ModelInfo `json:"models"`
	Default        string      `json:"default"`
	ThinkingLevels []string    `json:"thinkingLevels,omitempty"`
	// ConfiguredDefault is the user's settings selection; Default is the model
	// Pi actually selected with Vantry's resource policy. They may differ when
	// the configured model requires an extension unavailable to the worker.
	ConfiguredDefault string   `json:"configuredDefault,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

// GitFile paths are checkout-root-relative, decoded from porcelain -z.
type GitFile struct {
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath,omitempty"`
	Index        string `json:"index"`
	Worktree     string `json:"worktree"`
}

type GitCommit struct {
	ID         string    `json:"id"`
	ShortID    string    `json:"shortId"`
	Subject    string    `json:"subject"`
	Author     string    `json:"author"`
	AuthorTime time.Time `json:"authorTime"`
	Refs       []string  `json:"refs,omitempty"`
	Parents    []string  `json:"parents,omitempty"`
}

type GitStash struct {
	Ref     string `json:"ref"`
	Branch  string `json:"branch,omitempty"`
	Message string `json:"message"`
}

type GitState struct {
	Files          []GitFile   `json:"files"`
	Root           string      `json:"root,omitempty"`
	Truncated      bool        `json:"truncated,omitempty"`
	Branch         string      `json:"branch"`
	RemoteBranch   string      `json:"remoteBranch,omitempty"`
	Ahead          int         `json:"ahead,omitempty"`
	Behind         int         `json:"behind,omitempty"`
	Status         string      `json:"status"`
	Diff           string      `json:"diff"`
	Branches       []string    `json:"branches"`
	RemoteBranches []string    `json:"remoteBranches,omitempty"`
	Stashes        []GitStash  `json:"stashes,omitempty"`
	Commits        []GitCommit `json:"commits,omitempty"`
	Conflicts      bool        `json:"conflicts"`
}

type SSHHostInfo struct {
	Alias   string   `json:"alias"`
	Aliases []string `json:"aliases,omitempty"`
	// User and HostName are the resolved destination of an alias: what ssh
	// connects to, as ~/.ssh/config names it.
	User            string `json:"user,omitempty"`
	HostName        string `json:"hostName,omitempty"`
	Port            int    `json:"port,omitempty"`
	ProxyJump       string `json:"proxyJump,omitempty"`
	ForwardAgent    bool   `json:"forwardAgent,omitempty"`
	HasIdentityFile bool   `json:"hasIdentityFile,omitempty"`
	SourceFile      string `json:"sourceFile,omitempty"`
	Reachable       *bool  `json:"reachable,omitempty"`
	ProbeError      string `json:"probeError,omitempty"`
	// ProbeFailure names the kind of failure, for the status of a host:
	// "auth", "hostkey", "dns", "refused", "timeout", "route",
	// "handshake", "closed", or "other".
	ProbeFailure string `json:"probeFailure,omitempty"`
	LatencyMs    int    `json:"latencyMs,omitempty"`
	RemoteSocket string `json:"remoteSocket,omitempty"`
	RemoteOS     string `json:"remoteOS,omitempty"`
	HasHerdr     bool   `json:"hasHerdr,omitempty"`
}

type PluginSettingOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type PluginSettingSpec struct {
	Key     string                `json:"key"`
	Label   string                `json:"label"`
	Help    string                `json:"help,omitempty"`
	Type    string                `json:"type,omitempty"` // string | select | boolean
	Default string                `json:"default,omitempty"`
	Value   string                `json:"value,omitempty"`
	Options []PluginSettingOption `json:"options,omitempty"`
}

type PluginNavItem struct {
	Title string `json:"title"`
	Page  string `json:"page"`
	Icon  string `json:"icon,omitempty"`
	// NativePanel selects a host-rendered plugin page. It never starts
	// capture or input access until the user acts inside that page.
	NativePanel string `json:"nativePanel,omitempty"`
}

type PluginInfo struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Version           string              `json:"version,omitempty"`
	Description       string              `json:"description,omitempty"`
	Repo              string              `json:"repo,omitempty"`
	Builtin           bool                `json:"builtin,omitempty"`
	Disabled          bool                `json:"disabled,omitempty"`
	HasUpdate         bool                `json:"hasUpdate,omitempty"`
	LatestVersion     string              `json:"latestVersion,omitempty"`
	NeedsProcessGrant bool                `json:"needsProcessGrant,omitempty"`
	Granted           bool                `json:"granted,omitempty"`
	Settings          []PluginSettingSpec `json:"settings,omitempty"`
	Navigation        []PluginNavItem     `json:"navigation,omitempty"`
	Error             string              `json:"error,omitempty"`
}

type AppUpdateInfo struct {
	CurrentVersion string    `json:"currentVersion"`
	LatestVersion  string    `json:"latestVersion,omitempty"`
	Available      bool      `json:"available"`
	DeltaAvailable bool      `json:"deltaAvailable,omitempty"`
	DeltaSize      int64     `json:"deltaSize,omitempty"`
	AssetName      string    `json:"assetName,omitempty"`
	AssetSize      int64     `json:"assetSize,omitempty"`
	Notes          string    `json:"notes,omitempty"`
	PublishedAt    time.Time `json:"publishedAt,omitempty"`
	ReleaseURL     string    `json:"releaseUrl,omitempty"`
	Staged         bool      `json:"staged,omitempty"`
	StagedVersion  string    `json:"stagedVersion,omitempty"`
	StagedPath     string    `json:"stagedPath,omitempty"`
	TargetPath     string    `json:"targetPath,omitempty"`
	InstallerPath  string    `json:"installerPath,omitempty"`
	UsedDelta      bool      `json:"usedDelta,omitempty"`
	CanAutoRestart bool      `json:"canAutoRestart,omitempty"`
}

// WorkbenchService is optional, allowing older clients to keep the core protocol.
// All filesystem operations happen on the machine that owns Core.
type WorkbenchService interface {
	Workbench(context.Context, WorkbenchRequest) (WorkbenchResponse, error)
}

type WorkbenchRequest struct {
	Paths     []string        `json:"paths,omitempty"`
	Message   string          `json:"message,omitempty"`
	Action    string          `json:"action"`
	ThreadID  string          `json:"threadId,omitempty"`
	Workspace string          `json:"workspace,omitempty"`
	Model     string          `json:"model,omitempty"`
	Name      string          `json:"name,omitempty"`
	Data      []byte          `json:"data,omitempty"`
	Branch    string          `json:"branch,omitempty"`
	PluginID  string          `json:"pluginId,omitempty"`
	Amend     bool            `json:"amend,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type WorkbenchResponse struct {
	MCP        *MCPCatalog    `json:"mcp,omitempty"`
	Data       []byte         `json:"data,omitempty"` // bounded image content from the Core host
	Workspace  string         `json:"workspace,omitempty"`
	Catalog    *ModelCatalog  `json:"catalog,omitempty"`
	Attachment *Attachment    `json:"attachment,omitempty"`
	Git        *GitState      `json:"git,omitempty"`
	SSHHosts   []SSHHostInfo  `json:"sshHosts,omitempty"`
	Plugins    []PluginInfo     `json:"plugins,omitempty"`
	Update     *AppUpdateInfo   `json:"update,omitempty"`
	PiRuntime  *PiRuntimeStatus `json:"piRuntime,omitempty"`
	Output     string           `json:"output,omitempty"`
}
