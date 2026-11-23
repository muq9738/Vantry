//go:build windows

package pi

import (
	"context"
	"os"

	"github.com/muq9738/Vantry/internal/hostprocess"
)

type piCommand struct{ *hostprocess.Command }

func newPiCommand(ctx context.Context, executable string, args []string, jobDir, attemptID string, workspaceLock *os.File) (*piCommand, error) {
	executable, args, err := piLaunchArguments(executable, args)
	if err != nil {
		return nil, err
	}
	command := hostprocess.NewContext(ctx, executable, args...)
	// A crashed worker closes the noninherited job handle and kills all tools.
	// Lock acquisition also checks this job until kernel termination completes.
	command.SetJobIdentity(workspaceLock.Name())
	command.SetExecutionIdentity(piExecutionIdentity(jobDir, attemptID))
	return &piCommand{command}, nil
}
func newPiQueryCommand(ctx context.Context, executable string, args ...string) (*piCommand, error) {
	executable, args, err := piLaunchArguments(executable, args)
	if err != nil {
		return nil, err
	}
	return &piCommand{hostprocess.NewContext(ctx, executable, args...)}, nil
}
func piExecutionIdentity(jobDir, attemptID string) string { return jobDir + "\n" + attemptID }
