package core

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// TestCloseIsBoundedWithUnresponsiveProvider proves that Close does not hang on
// a provider that ignores cancellation, that the still-active run is recorded as
// interrupted, and that a late result callback cannot touch the closed database.
func TestCloseIsBoundedWithUnresponsiveProvider(t *testing.T) {
	release := make(chan struct{})
	var finished atomic.Bool
	var lateErr atomic.Value

	p := &funcProvider{runFn: func(_ context.Context, _ provider.Request, emit func(string) error) error {
		defer finished.Store(true)
		if err := emit("partial"); err != nil {
			return err
		}
		<-release // ignore the context on purpose
		lateErr.Store(emit("late"))
		return nil
	}}

	path := filepath.Join(t.TempDir(), "core.db")
	c := openCoreAt(t, path, p)
	c.shutdownGrace = 150 * time.Millisecond

	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	// Wait until the provider is parked, so the committed prefix cannot lose the
	// race against shutdown.
	waitFor(t, "partial output to be committed", func() bool {
		run, ok := findRun(snapshotOf(t, c), sent.RunID)
		return ok && run.Output == "partial"
	})

	started := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 3*time.Second {
		t.Fatalf("Close took %v, expected to be bounded by the shutdown grace", elapsed)
	}
	if state := c.state.Load(); state != stateStopped {
		t.Fatalf("state after Close = %q", stateName(state))
	}

	// Writes are refused before they reach the closed handle.
	writeErr := c.write(context.Background(), func(*txn) error { return nil })
	if writeErr == nil {
		t.Fatal("write after Close succeeded, want a refusal")
	}
	if _, err := c.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot after Close succeeded")
	}
	if _, err := c.Subscribe(context.Background(), 0); err == nil {
		t.Fatal("Subscribe after Close succeeded")
	}
	if _, err := c.Dispatch(context.Background(), sendCommand("send-2", threadID, "again")); err == nil {
		t.Fatal("Dispatch after Close succeeded")
	}
	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health after Close: %v", err)
	}
	if health.State != "stopped" {
		t.Fatalf("health state after Close = %q", health.State)
	}

	// Releasing the wedged provider must not panic or write to the closed DB.
	close(release)
	waitFor(t, "the wedged provider to return", func() bool { return finished.Load() })
	if v := lateErr.Load(); v != nil && v.(error) == nil {
		t.Fatal("late emit after Close was accepted")
	}

	// The drain recorded the run before the database closed.
	reopened := openCoreAt(t, path, &funcProvider{})
	defer reopened.Close()
	run := waitRunStatus(t, reopened, sent.RunID, api.RunInterrupted)
	if run.Output != "partial" {
		t.Fatalf("output after reopen = %q, want the committed prefix", run.Output)
	}
}

// TestOpenValidatesOptions covers the cheap contract checks on Open.
func TestOpenValidatesOptions(t *testing.T) {
	if _, err := Open(context.Background(), Options{DBPath: filepath.Join(t.TempDir(), "core.db")}); err == nil {
		t.Fatal("Open without a provider succeeded")
	}
	if _, err := Open(context.Background(), Options{Provider: &funcProvider{}}); err == nil {
		t.Fatal("Open without a database path succeeded")
	}
}

// TestCommandsAreRejectedWhileDraining proves that a draining core refuses new
// writes instead of racing the shutdown.
func TestCommandsAreRejectedWhileDraining(t *testing.T) {
	c := openCoreAt(t, filepath.Join(t.TempDir(), "core.db"), emitProvider("a"))
	c.state.Store(stateDraining)
	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "create-1", Kind: api.CreateThread, Title: "t",
	})
	requireCode(t, err, api.NotReady)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

var _ provider.Provider = (*funcProvider)(nil)
