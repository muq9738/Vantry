package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

const testTimeout = 10 * time.Second

// funcProvider is a fully controlled provider: tests decide what it emits and
// how long it takes, and it never talks to a real model.
type funcProvider struct {
	runFn func(ctx context.Context, req provider.Request, emit func(string) error) error

	mu    sync.Mutex
	calls []provider.Request
}

func (p *funcProvider) Run(ctx context.Context, req provider.Request, emit func(string) error) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	if p.runFn != nil {
		return p.runFn(ctx, req, emit)
	}
	return emit("echo: " + req.Message)
}

func (p *funcProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *funcProvider) requestAt(i int) provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[i]
}

// emitProvider returns a provider that emits a fixed list of deltas.
func emitProvider(deltas ...string) *funcProvider {
	return &funcProvider{runFn: func(_ context.Context, _ provider.Request, emit func(string) error) error {
		for _, delta := range deltas {
			if err := emit(delta); err != nil {
				return err
			}
		}
		return nil
	}}
}

func openCoreAt(t *testing.T, path string, p provider.Provider) *Core {
	t.Helper()
	c, err := Open(context.Background(), Options{DBPath: path, Provider: p})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

// openTestCore opens a core on a temporary database and closes it when the test
// ends. Both the directory and the database are private to the test.
func openTestCore(t *testing.T, p provider.Provider) *Core {
	t.Helper()
	c := openCoreAt(t, filepath.Join(t.TempDir(), "core.db"), p)
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

// setTestHookPreCommit installs the rollback failpoint. It takes the write
// coordinator so the hook cannot race with the worker's own writes.
func (c *Core) setTestHookPreCommit(fn func(*sql.Tx) error) {
	c.wmu.Lock()
	c.testHookPreCommit = fn
	c.wmu.Unlock()
}

// crashForTest simulates a process that disappeared: the database handle is
// closed while a run is still active and its effect is still claimed. Neither
// the run nor the outbox row is resolved, exactly like kill -9.
func crashForTest(c *Core) {
	// A crash stops committing immediately: mark the store as closing before
	// cancelling observers, otherwise a provider that returns on cancellation
	// could still settle its run and strand a continuation that a real process
	// death would never have created.
	c.wmu.Lock()
	c.closing = true
	c.wmu.Unlock()
	c.state.Store(stateDraining)
	c.workCancel()
	// Let in-flight writes leave the reserved connection before it is closed, so
	// the simulated crash cannot leave a half-open transaction behind.
	settled := make(chan struct{})
	go func() {
		c.workWG.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(2 * time.Second):
	}
	c.lifeMu.Lock()
	_ = c.store.Close()
	c.state.Store(stateStopped)
	c.lifeMu.Unlock()
}

func mustCreateThread(t *testing.T, c *Core, commandID string) string {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), api.Command{
		Version:   api.ProtocolVersion,
		CommandID: commandID,
		Kind:      api.CreateThread,
		Title:     "thread " + commandID,
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return receipt.ThreadID
}

func mustSend(t *testing.T, c *Core, commandID, threadID, message string) api.Receipt {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), api.Command{
		Version:   api.ProtocolVersion,
		CommandID: commandID,
		Kind:      api.SendMessage,
		ThreadID:  threadID,
		Message:   message,
	})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	return receipt
}

func sendCommand(commandID, threadID, message string) api.Command {
	return api.Command{
		Version:   api.ProtocolVersion,
		CommandID: commandID,
		Kind:      api.SendMessage,
		ThreadID:  threadID,
		Message:   message,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func snapshotOf(t *testing.T, c *Core) api.Snapshot {
	t.Helper()
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

func findRun(snap api.Snapshot, runID string) (api.Run, bool) {
	for _, run := range snap.Runs {
		if run.ID == runID {
			return run, true
		}
	}
	return api.Run{}, false
}

func waitRunStatus(t *testing.T, c *Core, runID string, status api.RunStatus) api.Run {
	t.Helper()
	var run api.Run
	waitFor(t, "run "+runID+" to become "+string(status), func() bool {
		found, ok := findRun(snapshotOf(t, c), runID)
		if !ok {
			return false
		}
		run = found
		return found.Status == status
	})
	return run
}

func waitRunTerminal(t *testing.T, c *Core, runID string) api.Run {
	t.Helper()
	var run api.Run
	waitFor(t, "run "+runID+" to finish", func() bool {
		found, ok := findRun(snapshotOf(t, c), runID)
		if !ok {
			return false
		}
		run = found
		return found.Status.Terminal()
	})
	return run
}

func countRows(t *testing.T, c *Core, table string) int {
	t.Helper()
	var n int
	if err := c.store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func attemptStatus(t *testing.T, c *Core, runID string) string {
	t.Helper()
	var status string
	err := c.store.db.QueryRow(`SELECT status FROM attempts WHERE run_id = ? ORDER BY ordinal DESC LIMIT 1`, runID).Scan(&status)
	if err != nil {
		t.Fatalf("read attempt status: %v", err)
	}
	return status
}

func effectStatus(t *testing.T, c *Core, runID string) string {
	t.Helper()
	var status string
	if err := c.store.db.QueryRow(`SELECT status FROM effect_outbox WHERE run_id = ?`, runID).Scan(&status); err != nil {
		t.Fatalf("read effect status: %v", err)
	}
	return status
}

func readEvents(t *testing.T, sub *api.Subscription, n int) []api.Event {
	t.Helper()
	out := make([]api.Event, 0, n)
	deadline := time.After(testTimeout)
	for len(out) < n {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription closed after %d of %d events", len(out), n)
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(out), n)
		}
	}
	return out
}

func readSubscriptionError(t *testing.T, sub *api.Subscription) (api.ErrorCode, bool) {
	t.Helper()
	select {
	case err, ok := <-sub.Errors:
		if !ok {
			return "", false
		}
		return api.CodeOf(err), true
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a subscription error")
		return "", false
	}
}

func requireCode(t *testing.T, err error, want api.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %q, got nil", want)
	}
	if got := api.CodeOf(err); got != want {
		t.Fatalf("expected error code %q, got %q (%v)", want, got, err)
	}
}
