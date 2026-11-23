package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/muq9738/Vantry/internal/hostprocess"
)

const EnvSandboxMode = "VANTRY_SANDBOX_MODE"

type sandboxConfigFile struct {
	Mode hostprocess.SandboxMode `json:"mode"`
}

func sandboxConfigPath(dataDir string) string {
	trimmed := strings.TrimSpace(dataDir)
	if trimmed == "" {
		return ""
	}
	return filepath.Join(trimmed, "sandbox.json")
}

// ReadSandboxMode returns the active Seatbelt sandbox mode and any explicit env
// override name/value (from VANTRY_SANDBOX_MODE) taking precedence over disk.
func ReadSandboxMode(dataDir string) (hostprocess.SandboxMode, string) {
	if rawEnv := strings.TrimSpace(os.Getenv(EnvSandboxMode)); rawEnv != "" {
		if mode, err := hostprocess.NormalizeSandboxMode(rawEnv); err == nil {
			return mode, rawEnv
		}
		return hostprocess.SandboxOff, rawEnv
	}
	cfgPath := sandboxConfigPath(dataDir)
	if cfgPath == "" {
		return hostprocess.SandboxOff, ""
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return hostprocess.SandboxOff, ""
	}
	var doc sandboxConfigFile
	if json.Unmarshal(raw, &doc) != nil {
		return hostprocess.SandboxOff, ""
	}
	if mode, err := hostprocess.NormalizeSandboxMode(string(doc.Mode)); err == nil {
		return mode, ""
	}
	return hostprocess.SandboxOff, ""
}

// WriteSandboxMode persists mode atomically to <dataDir>/sandbox.json (0600).
func WriteSandboxMode(dataDir string, mode hostprocess.SandboxMode) error {
	normalized, err := hostprocess.NormalizeSandboxMode(string(mode))
	if err != nil {
		return err
	}
	cfgPath := sandboxConfigPath(dataDir)
	if cfgPath == "" {
		return os.ErrInvalid
	}
	data, err := json.MarshalIndent(sandboxConfigFile{Mode: normalized}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateFileAtomic(cfgPath, data)
}

func resolveEffectiveSandboxMode(dataDir, defaultMode string) hostprocess.SandboxMode {
	if rawEnv := strings.TrimSpace(os.Getenv(EnvSandboxMode)); rawEnv != "" {
		if mode, err := hostprocess.NormalizeSandboxMode(rawEnv); err == nil {
			return mode
		}
		return hostprocess.SandboxOff
	}
	if cfgPath := sandboxConfigPath(dataDir); cfgPath != "" {
		if raw, err := os.ReadFile(cfgPath); err == nil {
			var doc sandboxConfigFile
			if json.Unmarshal(raw, &doc) == nil {
				if mode, err := hostprocess.NormalizeSandboxMode(string(doc.Mode)); err == nil {
					return mode
				}
			}
		}
	}
	if mode, err := hostprocess.NormalizeSandboxMode(defaultMode); err == nil {
		return mode
	}
	return hostprocess.SandboxOff
}
