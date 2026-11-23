package hostplatform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CommandSpec is one native command invocation. Arguments are passed verbatim
// as argv and are never joined into a shell command line, so a payload cannot
// become a command, an option, or a script statement.
type CommandSpec struct {
	Name  string
	Args  []string
	Stdin string
}

// stopGrace is how long a sleep inhibitor may take to exit before it is killed.
const stopGrace = 2 * time.Second

var errEmptyCommand = errors.New("hostplatform: 命令为空")

// commandRunner is one captured command run; it is a named type so the
// clipboard platform flows can be tested without the native tools.
type commandRunner func(context.Context, CommandSpec) (commandOutput, error)

// commandOutput is captured standard output plus whether the capture limit was
// reached, so a caller can reject oversized clipboard data without ever
// holding more than the limit in memory.
type commandOutput struct {
	stdout []byte
	excess bool
}

func commandFor(ctx context.Context, spec CommandSpec) *exec.Cmd {
	command := exec.CommandContext(ctx, spec.Name, spec.Args...)
	if spec.Stdin != "" {
		command.Stdin = strings.NewReader(spec.Stdin)
	}
	return command
}

// runCommand runs a native command and reports only whether it succeeded; the
// desktop tools here print diagnostics that the workbench does not show.
func runCommand(ctx context.Context, spec CommandSpec) error {
	if spec.Name == "" {
		return errEmptyCommand
	}
	command := commandFor(ctx, spec)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return fmt.Errorf("执行 %s：%w", spec.Name, err)
	}
	return nil
}

// runCommandOutput captures standard output up to limit bytes. Reading past the
// limit keeps the child from blocking on a full pipe while discarding the
// excess, and reports it so the caller can raise its own size error.
func runCommandOutput(ctx context.Context, spec CommandSpec, limit int) (commandOutput, error) {
	if spec.Name == "" {
		return commandOutput{}, errEmptyCommand
	}
	capture := &cappedBuffer{limit: limit}
	command := commandFor(ctx, spec)
	command.Stdout = capture
	command.Stderr = io.Discard
	err := command.Run()
	output := capture.output()
	if err != nil {
		return output, fmt.Errorf("执行 %s：%w", spec.Name, err)
	}
	return output, nil
}

type cappedBuffer struct {
	buffer bytes.Buffer
	limit  int
	excess bool
}

func (c *cappedBuffer) Write(chunk []byte) (int, error) {
	remaining := c.limit - c.buffer.Len()
	if remaining < len(chunk) {
		c.excess = true
		if remaining > 0 {
			_, _ = c.buffer.Write(chunk[:remaining])
		}
		return len(chunk), nil
	}
	return c.buffer.Write(chunk)
}

func (c *cappedBuffer) output() commandOutput {
	return commandOutput{stdout: c.buffer.Bytes(), excess: c.excess}
}

// Process is a started sleep inhibitor. Stop terminates the child and reaps it,
// and is safe to call more than once.
type Process interface {
	Stop()
}

// LaunchFunc starts an inhibitor command. Nil means "run it as a subprocess".
type LaunchFunc func(CommandSpec) (Process, error)

// launchProcess starts spec in the background and returns a handle whose Stop
// terminates and reaps it.
func launchProcess(spec CommandSpec) (Process, error) {
	if spec.Name == "" {
		return nil, errEmptyCommand
	}
	command := exec.Command(spec.Name, spec.Args...)
	if spec.Stdin != "" {
		command.Stdin = strings.NewReader(spec.Stdin)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("启动 %s：%w", spec.Name, err)
	}
	process := &childProcess{command: command, exited: make(chan struct{})}
	go process.reap()
	return process, nil
}

type childProcess struct {
	command *exec.Cmd
	exited  chan struct{}
	once    sync.Once
}

func (p *childProcess) reap() {
	_ = p.command.Wait()
	close(p.exited)
}

func (p *childProcess) Stop() {
	p.once.Do(p.stop)
}

// stop asks the child to exit, then kills it, and always waits for Wait so no
// zombie is left behind. Every supported inhibitor is also bound to our own
// process (caffeinate watches our pid), so even SIGKILL on this process ends
// the child instead of orphaning it.
func (p *childProcess) stop() {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = terminateProcess(p.command.Process)
	select {
	case <-p.exited:
		return
	case <-time.After(stopGrace):
	}
	_ = p.command.Process.Kill()
	<-p.exited
}
