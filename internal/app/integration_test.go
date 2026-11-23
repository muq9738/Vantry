package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/pkg/api"
)

// A real subprocess lets the test kill Core between commits without a graceful
// Close. It also exercises the OS lock and stale socket left by that process.
func TestCoreProcess(t *testing.T) {
	directory := os.Getenv("VANTRY_TEST_CORE_DIR")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := RunCore(ctx, Config{DataDir: directory, Delay: 12 * time.Millisecond, ProviderID: "controlled"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

type coreProcess struct {
	command *exec.Cmd
	log     *os.File
	stopped bool
}

func startTestCore(t *testing.T, directory string) *coreProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.CreateTemp(directory, "core-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCoreProcess$")
	command.Env = append(os.Environ(), "VANTRY_TEST_CORE_DIR="+directory)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	process := &coreProcess{command: command, log: logFile}
	t.Cleanup(func() {
		if !process.stopped {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = logFile.Close()
		if t.Failed() {
			content, _ := os.ReadFile(logFile.Name())
			t.Logf("Core subprocess log:\n%s", content)
		}
	})
	client := local.Dial(filepath.Join(directory, "core.sock"))
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		if err := checkReady(ctx, client); err == nil {
			return process
		}
		select {
		case <-ctx.Done():
			t.Fatal("Core subprocess did not become ready")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (p *coreProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = p.command.Wait()
	p.stopped = true
}

func awaitSnapshot(t *testing.T, service api.Service, predicate func(api.Snapshot) bool) api.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		snapshot, err := service.Snapshot(ctx)
		if err == nil && predicate(snapshot) {
			return snapshot
		}
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot condition not reached; last snapshot=%+v error=%v", snapshot, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func dispatchTest(t *testing.T, service api.Service, command api.Command) api.Receipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	receipt, err := service.Dispatch(ctx, command)
	if err != nil {
		t.Fatalf("dispatch %+v: %v", command, err)
	}
	return receipt
}

func TestIndependentCoreLifecycle(t *testing.T) {
	// Darwin's Unix socket path limit is shorter than many testing.TempDir paths.
	directory, err := os.MkdirTemp("/tmp", "vantry-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	process := startTestCore(t, directory)
	client := local.Dial(filepath.Join(directory, "core.sock"))
	t.Cleanup(func() { _ = client.Close() })
	create := api.Command{Version: 1, CommandID: "create-once", Kind: api.CreateThread, ThreadID: "thread-a", Title: "恢复测试"}
	created := dispatchTest(t, client, create)
	if retry := dispatchTest(t, client, create); retry != created {
		t.Fatalf("create receipt changed on retry: %+v != %+v", retry, created)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	subscription, err := client.Subscribe(ctx, created.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	send := api.Command{Version: 1, CommandID: "send-once", Kind: api.SendMessage, ThreadID: "thread-a", Message: strings.Repeat("流式恢复测试", 100)}
	accepted := dispatchTest(t, client, send)
	if retry := dispatchTest(t, client, send); retry != accepted {
		t.Fatalf("send receipt changed on retry: %+v != %+v", retry, accepted)
	}
	select {
	case event := <-subscription.Events:
		if event.Sequence <= created.Sequence || event.RunID != accepted.RunID {
			t.Fatalf("unexpected replay event: %+v", event)
		}
	case err := <-subscription.Errors:
		t.Fatalf("subscription failed: %v", err)
	case <-ctx.Done():
		t.Fatal("no event received after command")
	}
	snapshot := awaitSnapshot(t, client, func(s api.Snapshot) bool {
		return len(s.Runs) == 1 && s.Runs[0].Status == api.RunRunning && len(s.Runs[0].Output) > 0
	})
	initialOutput := len(snapshot.Runs[0].Output)
	// Losing all clients must leave the provider's invocation alive.
	subscription.Close()
	_ = client.Close()
	client = local.Dial(filepath.Join(directory, "core.sock"))
	awaitSnapshot(t, client, func(s api.Snapshot) bool {
		return len(s.Runs) == 1 && len(s.Runs[0].Output) > initialOutput
	})

	// A second process must fail before opening the already-owned database.
	executable, _ := os.Executable()
	duplicate := exec.Command(executable, "-test.run=^TestCoreProcess$")
	duplicate.Env = append(os.Environ(), "VANTRY_TEST_CORE_DIR="+directory)
	if result, err := duplicate.CombinedOutput(); err == nil || !strings.Contains(string(result), ErrAlreadyRunning.Error()) {
		t.Fatalf("duplicate Core did not respect instance lock: error=%v output=%s", err, result)
	}

	stop := api.Command{Version: 1, CommandID: "stop-once", Kind: api.StopRun, ThreadID: "thread-a", RunID: accepted.RunID}
	dispatchTest(t, client, stop)
	awaitSnapshot(t, client, func(s api.Snapshot) bool {
		return len(s.Runs) == 1 && s.Runs[0].Status == api.RunCancelled
	})

	second := api.Command{Version: 1, CommandID: "survive-crash", Kind: api.SendMessage, ThreadID: "thread-a", Message: strings.Repeat("崩溃后不重复发送", 100)}
	secondReceipt := dispatchTest(t, client, second)
	beforeCrash := awaitSnapshot(t, client, func(s api.Snapshot) bool {
		for _, run := range s.Runs {
			if run.ID == secondReceipt.RunID {
				return run.Status == api.RunRunning && run.Output != ""
			}
		}
		return false
	})
	_ = client.Close()
	process.kill(t)
	process = startTestCore(t, directory)
	client = local.Dial(filepath.Join(directory, "core.sock"))
	recovered := awaitSnapshot(t, client, func(s api.Snapshot) bool {
		for _, run := range s.Runs {
			if run.ID == secondReceipt.RunID {
				return run.Status == api.RunInterrupted && run.Output != ""
			}
		}
		return false
	})
	if len(recovered.Threads) != 1 || len(recovered.Runs) != 2 || recovered.Sequence <= beforeCrash.Sequence {
		t.Fatalf("recovery did not retain committed history: %+v", recovered)
	}
	if retry := dispatchTest(t, client, second); retry != secondReceipt {
		t.Fatalf("persisted receipt changed after crash: %+v != %+v", retry, secondReceipt)
	}
	changed := second
	changed.Message = "different payload"
	if _, err := client.Dispatch(ctx, changed); api.CodeOf(err) != api.Conflict {
		t.Fatalf("changed payload should conflict, got %v", err)
	}
	// The old cursor must replay committed history, including recovery events.
	replay, err := client.Subscribe(ctx, beforeCrash.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	select {
	case event := <-replay.Events:
		if event.Sequence <= beforeCrash.Sequence {
			t.Fatalf("replayed event predates cursor: %+v", event)
		}
	case err := <-replay.Errors:
		t.Fatalf("replay error: %v", err)
	case <-ctx.Done():
		t.Fatal("recovery event was not replayed")
	}
	// Terminate cooperatively as well, proving the listener and process drain.
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := process.command.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("graceful Core shutdown: %v", err)
	}
	process.stopped = true
}
