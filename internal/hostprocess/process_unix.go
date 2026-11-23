//go:build unix

package hostprocess

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type processTree struct{}

func (cmd *Command) configure()   { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func (cmd *Command) start() error { return cmd.Cmd.Start() }

func (cmd *Command) KillTree() error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func (cmd *Command) EnsureGone() {
	_ = cmd.KillTree()
	if cmd.Process == nil {
		return
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-cmd.Process.Pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// Unix commands have no additional handle to release. Descendants are killed
// explicitly at the original operation's cancellation/cleanup sites.
func (cmd *Command) Close() error { return nil }

func ConfigureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
