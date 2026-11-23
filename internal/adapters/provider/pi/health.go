package pi

import (
	"context"
	"strings"
)

// Version queries the existing Pi installation without starting a session.
func Version(ctx context.Context, executable string) (string, error) {
	executable = resolvePiExecutable(executable)
	command, err := newPiQueryCommand(ctx, executable, "--version")
	if err != nil {
		return "", err
	}
	defer command.Close()
	command.Env = piEnvironment(executable)
	out, err := command.Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if i := strings.IndexByte(version, '\n'); i >= 0 {
		version = version[:i]
	}
	if len(version) > 120 {
		version = version[:120]
	}
	return version, nil
}
