package hostprocess

import (
	"fmt"
	"strings"
)

type SandboxMode string

const (
	SandboxOff            SandboxMode = "off"
	SandboxWorkspaceWrite SandboxMode = "workspace-write"
)

type SandboxConfig struct {
	Mode              SandboxMode
	WorkspaceDir      string
	ExtraWritableDirs []string
	ProtectGitMeta    bool
	AllowTempDirs     bool
	AllowNetwork      bool
}

func NormalizeSandboxMode(raw string) (SandboxMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off", "disabled", "none", "false":
		return SandboxOff, nil
	case "workspace-write", "workspace", "seatbelt", "on", "true":
		return SandboxWorkspaceWrite, nil
	default:
		return "", fmt.Errorf("unsupported sandbox mode %q", raw)
	}
}
