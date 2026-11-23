//go:build unix

package pi

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

type piCommand struct{ *exec.Cmd }

func newPiCommand(ctx context.Context, executable string, args []string, jobDir, attemptID string, workspaceLock *os.File) (*piCommand, error) {
	command := exec.CommandContext(ctx, executable, args...)
	// Keep the same flock open in Pi. SIGKILL of its observer/worker cannot
	// release the workspace while that execution still holds the descriptor.
	command.ExtraFiles = []*os.File{workspaceLock}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		_, err := terminateProcessTree(jobDir, attemptID, command.Process.Pid)
		return err
	}
	return &piCommand{command}, nil
}
func newPiQueryCommand(ctx context.Context, executable string, args ...string) (*piCommand, error) {
	return &piCommand{exec.CommandContext(ctx, executable, args...)}, nil
}

// Unix retains the inherited workspace descriptor until Pi exits.
func (command *piCommand) Close() error { return nil }
