package api

// PiRuntimeStatus describes Vantry's resolved external Pi executable,
// local Node/npm toolchain, network mirror routing, 4-gate compatibility check,
// and ~/.pi/agent configuration summary.
type PiRuntimeStatus struct {
	ActiveSource    string `json:"activeSource"` // system | custom | missing
	ActivePath      string `json:"activePath,omitempty"`
	ActiveVersion   string `json:"activeVersion,omitempty"`
	CustomPath      string `json:"customPath,omitempty"`
	SystemPath      string `json:"systemPath,omitempty"`
	SystemVersion   string `json:"systemVersion,omitempty"`
	SystemIsNPM     bool   `json:"systemIsNpm,omitempty"`
	NodeVersion     string `json:"nodeVersion,omitempty"`
	RequiredNode    string `json:"requiredNode,omitempty"`
	NPMAvailable    bool   `json:"npmAvailable,omitempty"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable,omitempty"`
	CompatVerified  bool   `json:"compatVerified,omitempty"`
	CompatBlocked   bool   `json:"compatBlocked,omitempty"`
	CompatReason    string `json:"compatReason,omitempty"`
	CanUpgrade      bool   `json:"canUpgrade,omitempty"`
	CanInstall      bool   `json:"canInstall,omitempty"`
	ActiveRuns      int    `json:"activeRuns,omitempty"`
	MirrorMode      string `json:"mirrorMode"` // auto | cn | direct
	ResolvedRoute   string `json:"resolvedRoute,omitempty"`
	NPMRegistry     string `json:"npmRegistry,omitempty"`
	AgentDir        string `json:"agentDir,omitempty"`
	Configured      bool   `json:"configured,omitempty"`
	ModelCount      int    `json:"modelCount,omitempty"`
	DefaultModel    string `json:"defaultModel,omitempty"`
	Error           string `json:"error,omitempty"`
}

// PiRuntimeSettingsInput updates external Pi path and npm mirror preferences.
type PiRuntimeSettingsInput struct {
	CustomPath  string `json:"customPath,omitempty"`
	MirrorMode  string `json:"mirrorMode,omitempty"`
	NPMRegistry string `json:"npmRegistry,omitempty"`
}

// PiUpgradeInput requests upgrading (or installing when missing) the external
// npm-managed Pi runtime.
type PiUpgradeInput struct {
	Version string `json:"version,omitempty"`
	Install bool   `json:"install,omitempty"`
}

// PiSetupPreset describes a quick-start provider preset for first-run setup.
type PiSetupPreset struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ProviderID      string `json:"providerId"`
	API             string `json:"api"`
	DefaultBaseURL  string `json:"defaultBaseUrl,omitempty"`
	DefaultModel    string `json:"defaultModel,omitempty"`
	RequiresBaseURL bool   `json:"requiresBaseUrl,omitempty"`
	Description     string `json:"description,omitempty"`
}

// PiSetupRequest configures a provider key and optional custom endpoint in
// the standard Pi agent directory (~/.pi/agent) without creating symlinks.
type PiSetupRequest struct {
	Preset     string   `json:"preset,omitempty"`
	ProviderID string   `json:"providerId"`
	APIKey     string   `json:"apiKey"`
	BaseURL    string   `json:"baseUrl,omitempty"`
	API        string   `json:"api,omitempty"`
	Models     []string `json:"models,omitempty"`
	SetDefault bool     `json:"setDefault"`
}
