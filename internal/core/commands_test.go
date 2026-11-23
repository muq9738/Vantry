package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// TestDuplicateCommandDoesNotRunProviderTwice proves that an identical retry is
// answered from the persisted receipt: one run, one provider invocation.
func TestDuplicateCommandDoesNotRunProviderTwice(t *testing.T) {
	p := emitProvider("a")
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	cmd := sendCommand("send-1", threadID, "hello")

	first, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if !first.Accepted || first.RunID == "" || first.Sequence == 0 {
		t.Fatalf("unexpected receipt: %+v", first)
	}
	waitRunTerminal(t, c, first.RunID)

	second, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatalf("duplicate dispatch: %v", err)
	}
	if second != first {
		t.Fatalf("duplicate receipt mismatch: first %+v second %+v", first, second)
	}
	if got := p.callCount(); got != 1 {
		t.Fatalf("provider called %d times, want 1", got)
	}
	snap := snapshotOf(t, c)
	if len(snap.Runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(snap.Runs))
	}
	if len(snap.Threads) != 1 {
		t.Fatalf("got %d threads, want 1", len(snap.Threads))
	}

	// The same holds for thread.create and run.stop.
	again, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "create-1", Kind: api.CreateThread, Title: "thread create-1",
	})
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if again.ThreadID != threadID {
		t.Fatalf("duplicate create returned thread %q, want %q", again.ThreadID, threadID)
	}
	if got := countRows(t, c, "threads"); got != 1 {
		t.Fatalf("got %d threads after duplicate create, want 1", got)
	}

	stopCmd := api.Command{
		Version: api.ProtocolVersion, CommandID: "stop-1", Kind: api.StopRun,
		ThreadID: threadID, RunID: first.RunID,
	}
	stop1, err := c.Dispatch(context.Background(), stopCmd)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	stop2, err := c.Dispatch(context.Background(), stopCmd)
	if err != nil {
		t.Fatalf("duplicate stop: %v", err)
	}
	if stop1 != stop2 {
		t.Fatalf("duplicate stop mismatch: %+v vs %+v", stop1, stop2)
	}
}

// TestCommandIDConflict proves that reusing a commandId with different content
// is a conflict rather than a silent replay.
func TestCommandIDConflict(t *testing.T) {
	p := emitProvider("a")
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")

	first, err := c.Dispatch(context.Background(), sendCommand("shared", threadID, "one"))
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	waitRunStatus(t, c, first.RunID, api.RunCompleted)

	_, err = c.Dispatch(context.Background(), sendCommand("shared", threadID, "two"))
	requireCode(t, err, api.Conflict)

	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "shared", Kind: api.CreateThread, Title: "other",
	})
	requireCode(t, err, api.Conflict)

	// A protocol version mismatch and an unknown kind are rejected before any
	// state is touched.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion + 1, CommandID: "v2", Kind: api.CreateThread,
	})
	requireCode(t, err, api.VersionMismatch)

	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "unknown", Kind: "thread.unknown", ThreadID: threadID,
	})
	requireCode(t, err, api.InvalidCommand)

	if got := countRows(t, c, "threads"); got != 1 {
		t.Fatalf("got %d threads, want 1", got)
	}
	if got := p.callCount(); got != 1 {
		t.Fatalf("provider called %d times, want 1", got)
	}
}

// TestSingleActiveRunPerThread proves the per-thread limit: while a run is
// active a second message for the same thread is rejected, and after the run
// finishes a new message is accepted again.
func TestSingleActiveRunPerThread(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	p := &funcProvider{runFn: func(ctx context.Context, _ provider.Request, _ func(string) error) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")

	first := mustSend(t, c, "send-1", threadID, "one")
	waitRunStatus(t, c, first.RunID, api.RunRunning)
	select {
	case <-entered:
	case <-time.After(testTimeout):
		t.Fatal("provider was never entered")
	}

	_, err := c.Dispatch(context.Background(), sendCommand("send-2", threadID, "two"))
	requireCode(t, err, api.Conflict)
	if got := p.callCount(); got != 1 {
		t.Fatalf("provider called %d times while the thread was busy, want 1", got)
	}
	if got := countRows(t, c, "runs"); got != 1 {
		t.Fatalf("got %d runs, want 1", got)
	}

	close(gate)
	waitRunStatus(t, c, first.RunID, api.RunCompleted)

	next := mustSend(t, c, "send-3", threadID, "three")
	waitRunStatus(t, c, next.RunID, api.RunCompleted)
	if got := countRows(t, c, "runs"); got != 2 {
		t.Fatalf("got %d runs after the first finished, want 2", got)
	}
}

// TestDifferentThreadsRunInParallel proves that provider work is not serialised
// behind the single SQLite writer: two threads must have two providers in
// flight at the same time.
func TestDifferentThreadsRunInParallel(t *testing.T) {
	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	both := make(chan struct{})
	var once sync.Once

	p := &funcProvider{runFn: func(ctx context.Context, _ provider.Request, _ func(string) error) error {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		concurrent := inFlight >= 2
		mu.Unlock()
		if concurrent {
			once.Do(func() { close(both) })
		}
		select {
		case <-both:
		case <-time.After(testTimeout):
		case <-ctx.Done():
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}}

	c := openTestCore(t, p)
	threadA := mustCreateThread(t, c, "create-a")
	threadB := mustCreateThread(t, c, "create-b")

	runA := mustSend(t, c, "send-a", threadA, "a")
	runB := mustSend(t, c, "send-b", threadB, "b")

	waitRunStatus(t, c, runA.RunID, api.RunCompleted)
	waitRunStatus(t, c, runB.RunID, api.RunCompleted)

	mu.Lock()
	got := peak
	mu.Unlock()
	if got < 2 {
		t.Fatalf("peak concurrent provider calls = %d, want >= 2", got)
	}
}

// TestCancelRejectsLateOutput proves that a stop committed while the provider is
// still running invalidates the attempt: late deltas are refused, the stored
// output keeps only what was committed before the stop, and the attempt is
// terminal.
func TestCancelRejectsLateOutput(t *testing.T) {
	late := make(chan error, 1)

	p := &funcProvider{runFn: func(ctx context.Context, _ provider.Request, emit func(string) error) error {
		if err := emit("committed"); err != nil {
			return err
		}
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // widen the window on purpose
		late <- emit("late")
		return nil
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	waitFor(t, "first delta to be committed", func() bool {
		run, ok := findRun(snapshotOf(t, c), sent.RunID)
		return ok && run.Output == "committed"
	})

	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "stop-1", Kind: api.StopRun,
		ThreadID: threadID, RunID: sent.RunID,
	})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}

	select {
	case err := <-late:
		if err == nil {
			t.Fatal("late delta after cancellation was accepted, want an error")
		}
	case <-time.After(testTimeout):
		t.Fatal("provider did not observe cancellation")
	}

	run := waitRunTerminal(t, c, sent.RunID)
	if run.Status != api.RunCancelled {
		t.Fatalf("run status = %q, want cancelled", run.Status)
	}
	if run.Output != "committed" {
		t.Fatalf("run output = %q, want only the committed delta", run.Output)
	}
	if got := attemptStatus(t, c, sent.RunID); got != statusCancelled {
		t.Fatalf("attempt status = %q, want cancelled", got)
	}
	// The terminal run state and the outbox settlement are committed together.
	if got := effectStatus(t, c, sent.RunID); got != outboxCancelled {
		t.Fatalf("outbox status = %q, want %q after cancellation", got, outboxCancelled)
	}
	for _, ev := range loadAllEvents(t, c) {
		if ev.Type != eventRunOutput {
			continue
		}
		var data outputEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode output event: %v", err)
		}
		if data.Delta == "late" {
			t.Fatalf("late delta was committed as event %d", ev.Sequence)
		}
	}

	// The conditional commit itself rejects the stale attempt even with a live
	// context: the outbox claim and the active attempt are both required.
	runID := sent.RunID
	var (
		effID string
		token string
		attID string
	)
	if err := c.store.db.QueryRow(`SELECT id, claim_token, attempt_id FROM effect_outbox WHERE run_id = ?`, runID).
		Scan(&effID, &token, &attID); err != nil {
		t.Fatalf("read claimed effect: %v", err)
	}
	stale := effect{id: effID, runID: runID, attemptID: attID, token: token}
	if err := c.commitDelta(context.Background(), stale, "ghost"); !errors.Is(err, errStaleAttempt) {
		t.Fatalf("stale delta commit returned %v, want errStaleAttempt", err)
	}
	if after := waitRunTerminal(t, c, runID); after.Output != "committed" {
		t.Fatalf("stale delta changed output to %q", after.Output)
	}
}

// TestRollbackLeavesNoPartialState proves that event, projection, receipt and
// outbox commit atomically: an injected failure before commit leaves nothing
// behind, and the same command can be retried successfully afterwards.
func TestRollbackLeavesNoPartialState(t *testing.T) {
	p := emitProvider("ok")
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")

	eventsBefore := countRows(t, c, "events")
	c.setTestHookPreCommit(func(*sql.Tx) error { return errors.New("injected commit failure") })
	cmd := sendCommand("send-1", threadID, "hello")
	_, err := c.Dispatch(context.Background(), cmd)
	if err == nil {
		t.Fatal("dispatch succeeded despite the injected failure")
	}
	c.setTestHookPreCommit(nil)

	if got := countRows(t, c, "runs"); got != 0 {
		t.Fatalf("runs after rollback = %d, want 0", got)
	}
	if got := countRows(t, c, "attempts"); got != 0 {
		t.Fatalf("attempts after rollback = %d, want 0", got)
	}
	if got := countRows(t, c, "effect_outbox"); got != 0 {
		t.Fatalf("outbox after rollback = %d, want 0", got)
	}
	if got := countRows(t, c, "events"); got != eventsBefore {
		t.Fatalf("events after rollback = %d, want %d", got, eventsBefore)
	}
	var receipts int
	if err := c.store.db.QueryRow(`SELECT COUNT(*) FROM command_receipts WHERE command_id = ?`, cmd.CommandID).Scan(&receipts); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("receipt for a rolled back command was persisted")
	}

	// A retry with the same id must now succeed: the id was never claimed.
	receipt, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	waitRunTerminal(t, c, receipt.RunID)
	if got := p.callCount(); got != 1 {
		t.Fatalf("provider called %d times, want 1", got)
	}
	if got := countRows(t, c, "runs"); got != 1 {
		t.Fatalf("runs after retry = %d, want 1", got)
	}
}

func loadAllEvents(t *testing.T, c *Core) []api.Event {
	t.Helper()
	events, err := c.loadEventsAfter(context.Background(), 0, 100000)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	return events
}
