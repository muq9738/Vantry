//go:build windows

package hostprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func helperArgs(role, directory string) []string {
	return []string{"-test.run=^TestWindowsProcessHelper$", "--", role, directory}
}

// The worker deliberately owns its only job handle. Killing it must also kill
// the leader and grandchild, including a child created with plain os/exec.
func TestWindowsProcessHelper(t *testing.T) {
	args := os.Args
	if len(args) < 4 || args[len(args)-3] != "--" {
		return
	}
	role, directory := args[len(args)-2], args[len(args)-1]
	switch role {
	case "worker":
		child := New(os.Args[0], helperArgs("leader", directory)...)
		child.SetJobIdentity(directory)
		if err := child.Start(); err != nil {
			panic(err)
		}
		defer child.Close()
	case "leader":
		child := exec.Command(os.Args[0], helperArgs("grandchild", directory)...)
		if err := child.Start(); err != nil {
			panic(err)
		}
	case "grandchild", "unrelated":
	default:
		panic("unknown helper role")
	}
	if err := os.WriteFile(filepath.Join(directory, role+".pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		panic(err)
	}
	for {
		time.Sleep(time.Second)
	}
}

func waitHelper(t *testing.T, directory, role string) windows.Handle {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(directory, role+".pid"))
		pid, parseErr := strconv.Atoi(string(data))
		if err == nil && parseErr == nil {
			h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { windows.CloseHandle(h) })
			return h
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not start", role)
	return 0
}

func TestWorkerDeathKillsOwnedDescendantsAndPreservesUnrelatedProcess(t *testing.T) {
	directory := t.TempDir()
	unrelated := exec.Command(os.Args[0], helperArgs("unrelated", directory)...)
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	unrelatedHandle := waitHelper(t, directory, "unrelated")
	worker := exec.Command(os.Args[0], helperArgs("worker", directory)...)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Process.Kill(); _ = worker.Wait() }()
	waitHelper(t, directory, "worker")
	leader := waitHelper(t, directory, "leader")
	grandchild := waitHelper(t, directory, "grandchild")
	if active, err := JobActive(directory); err != nil || !active {
		t.Fatalf("execution job is missing: active=%v err=%v", active, err)
	}
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = worker.Wait()
	for _, process := range []windows.Handle{leader, grandchild} {
		state, err := windows.WaitForSingleObject(process, 10000)
		if err != nil || state != windows.WAIT_OBJECT_0 {
			t.Fatalf("owned child survived worker death: state=%d err=%v", state, err)
		}
	}
	if state, err := windows.WaitForSingleObject(unrelatedHandle, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("unrelated child was affected: state=%d err=%v", state, err)
	}
	if active, err := JobActive(directory); err != nil || active {
		t.Fatalf("terminated job is still active: active=%v err=%v", active, err)
	}
}

func TestCanceledCommandKillsItsJob(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := NewContext(ctx, os.Args[0], helperArgs("leader", directory)...)
	defer cmd.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	grandchild := waitHelper(t, directory, "grandchild")
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("canceled command reported success")
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if state, err := windows.WaitForSingleObject(grandchild, 10000); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("grandchild survived cancellation: state=%d err=%v", state, err)
	}
}

func TestAttemptJobDoesNotConfuseLaterWorkspaceWriter(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "workspace-guard")
	start := func(name string) *Command {
		directory := filepath.Join(root, name)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		command := New(os.Args[0], helperArgs("leader", directory)...)
		command.SetJobIdentity(shared)
		command.SetExecutionIdentity(directory + "\n" + name)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Close(); _ = command.Wait() })
		waitHelper(t, directory, "grandchild")
		return command
	}
	first := start("attempt-a")
	if active, err := JobActive(filepath.Join(root, "attempt-a") + "\nattempt-a"); err != nil || !active {
		t.Fatalf("missing attempt identity: %v %v", active, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	_ = start("attempt-b")
	if active, err := JobActive(shared); err != nil || !active {
		t.Fatal("new writer lost its workspace guard")
	}
	if active, err := JobActive(filepath.Join(root, "attempt-a") + "\nattempt-a"); err != nil || active {
		t.Fatalf("new writer held old attempt settlement: %v %v", active, err)
	}
}

func TestNamedJobCannotBeAdopted(t *testing.T) {
	identity := t.TempDir()
	first, err := createJob(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(first)
	second, err := createJob(identity)
	if second != 0 {
		windows.CloseHandle(second)
	}
	if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		t.Fatal(fmt.Errorf("adopted an existing job: %w", err))
	}
}
