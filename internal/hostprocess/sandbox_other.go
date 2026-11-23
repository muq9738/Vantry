//go:build !darwin

package hostprocess

import (
	"errors"
	"fmt"
)

func SandboxSupported() bool { return false }

func BuildSeatbeltProfile(_ SandboxConfig) (string, error) {
	return "", errors.New("workspace Seatbelt sandbox is only supported on macOS")
}

func WrapSandboxArgs(cfg SandboxConfig, executable string, args []string) (string, []string, error) {
	if cfg.Mode == "" || cfg.Mode == SandboxOff {
		return executable, args, nil
	}
	return "", nil, fmt.Errorf("sandbox mode %q is only supported on macOS", cfg.Mode)
}
