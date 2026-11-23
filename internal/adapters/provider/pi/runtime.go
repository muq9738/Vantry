package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

// ConfiguredModel reads only model-selection fields from Pi's user settings.
// Credentials and project trust settings remain owned by Pi itself.
func piAgentDirectory() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// ConfiguredModelKeys reads only provider/model IDs explicitly declared in the
// user's ~/.pi/agent/models.json (never credentials). When the user configured
// an explicit provider catalog there (e.g. Magpie only), Vantry aligns with
// that exact list instead of surfacing unrelated built-in auth leftovers.
func ConfiguredModelKeys() ([]string, bool) {
	dir := piAgentDirectory()
	if dir == "" {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		return nil, false
	}
	var doc struct {
		Providers map[string]struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Providers) == 0 {
		return nil, false
	}
	// Preserve deterministic provider order while keeping each provider's model order.
	providerNames := make([]string, 0, len(doc.Providers))
	for name := range doc.Providers {
		providerNames = append(providerNames, name)
	}
	for i := 0; i < len(providerNames); i++ {
		for j := i + 1; j < len(providerNames); j++ {
			if providerNames[j] < providerNames[i] {
				providerNames[i], providerNames[j] = providerNames[j], providerNames[i]
			}
		}
	}
	var keys []string
	seen := map[string]bool{}
	for _, prov := range providerNames {
		for _, m := range doc.Providers[prov].Models {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			key := id
			if !strings.HasPrefix(id, prov+"/") {
				key = prov + "/" + id
			}
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	if len(keys) == 0 {
		return nil, false
	}
	return keys, true
}

func ConfiguredModel() string {
	agentDirectory := piAgentDirectory()
	if agentDirectory == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(agentDirectory, "settings.json"))
	if err != nil {
		return ""
	}
	var settings struct {
		Provider string `json:"defaultProvider"`
		Model    string `json:"defaultModel"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return ""
	}
	if settings.Provider == "" || settings.Model == "" {
		return ""
	}
	if strings.HasPrefix(settings.Model, settings.Provider+"/") {
		return settings.Model
	}
	return settings.Provider + "/" + settings.Model
}

// Finder-launched apps often have a minimal PATH. Resolve an existing
// external Pi installation without altering user configuration or creating
// symlinks.
func resolvePiExecutable(requested string) string {
	res := ResolvePiRuntime(requested, "")
	if res.ActiveSource == RuntimeSourceMissing {
		if strings.TrimSpace(requested) != "" {
			return requested
		}
		return "pi"
	}
	return res.Path
}

func piEnvironment(executable string) []string {
	// A launcher that already pointed Git at another checkout must not leak that
	// routing into Pi's own Git work. User credentials, hooks, and configuration
	// stay inherited; only the routing variables are dropped.
	environment := workspace.Environment()
	if nodeExe := findSiblingNode(executable); nodeExe != "" {
		return append(environment, "PATH="+filepath.Dir(nodeExe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return environment
}

func workerPiEnvironment(job jobRequest, jobDir string) []string {
	base := piEnvironment(job.PiExecutable)
	reserved := map[string]bool{
		EnvExecutable:          true,
		EnvDataDir:             true,
		EnvSocketPath:          true,
		EnvJobDir:              true,
		EnvRunID:               true,
		EnvAttemptID:           true,
		EnvThreadID:            true,
		EnvParentTaskID:        true,
		EnvDirectOrchestration: true,
		EnvApprovalPolicy:      true,
		EnvInteractionMode:     true,
	}
	filtered := make([]string, 0, len(base)+10)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if reserved[key] {
			continue
		}
		filtered = append(filtered, entry)
	}
	policy, err := normalizeApprovalPolicyValue(job.Request.ApprovalPolicy)
	if err != nil {
		policy = ApprovalPolicyAsk
	}
	filtered = append(
		filtered,
		EnvExecutable+"="+job.Executable,
		EnvDataDir+"="+job.DataDir,
		EnvSocketPath+"="+job.SocketPath,
		EnvJobDir+"="+jobDir,
		EnvRunID+"="+job.Request.RunID,
		EnvAttemptID+"="+job.Request.AttemptID,
		EnvThreadID+"="+job.Request.ThreadID,
		EnvApprovalPolicy+"="+policy,
	)
	if parentTaskID := strings.TrimSpace(job.Request.ParentTaskID); parentTaskID != "" {
		filtered = append(filtered, EnvParentTaskID+"="+parentTaskID)
	}
	if mode := effectiveInteractionMode(job); mode != "" {
		filtered = append(filtered, EnvInteractionMode+"="+mode)
	}
	return filtered
}
