//go:build unix

package hostplatform

import (
	"errors"
	"syscall"
	"testing"
)

// TestKeepAwakeStopsRealInhibitorProcess proves the production launcher both
// starts a real child and reaps it on Close: after Close the pid must be gone
// (ESRCH), not a zombie or an orphan.
func TestKeepAwakeStopsRealInhibitorProcess(t *testing.T) {
	pids := make(chan int, 1)
	guard := NewKeepAwake(KeepAwakeOptions{
		Command: func() (CommandSpec, bool) {
			return CommandSpec{Name: "/bin/sh", Args: []string{"-c", "exec sleep 30"}}, true
		},
		Launch: func(spec CommandSpec) (Process, error) {
			process, err := launchProcess(spec)
			if err != nil {
				return nil, err
			}
			pids <- process.(*childProcess).command.Process.Pid
			return process, nil
		},
	})
	if err := guard.Acquire("run-1"); err != nil {
		t.Fatal(err)
	}
	pid := <-pids
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("inhibitor %d is not running: %v", pid, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("inhibitor %d still exists after Close: %v", pid, err)
	}
}
