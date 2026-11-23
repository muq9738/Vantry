// Package hostprocess starts commands whose descendants belong to the same
// cancellable operation. Detached Core/worker processes use ConfigureDetached.
package hostprocess

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

type Command struct {
	*exec.Cmd
	tree processTree
}

func New(name string, args ...string) *Command {
	cmd := &Command{Cmd: exec.Command(name, args...)}
	cmd.configure()
	return cmd
}

func NewContext(ctx context.Context, name string, args ...string) *Command {
	cmd := &Command{Cmd: exec.CommandContext(ctx, name, args...)}
	cmd.configure()
	cmd.Cancel = cmd.KillTree
	return cmd
}

func (cmd *Command) Start() error { return cmd.start() }

func (cmd *Command) Run() error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

func (cmd *Command) Output() ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	err := cmd.Run()
	return output.Bytes(), err
}

func (cmd *Command) CombinedOutput() ([]byte, error) {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, errors.New("exec: Stdout or Stderr already set")
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.Bytes(), err
}
