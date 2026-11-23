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

// TestRestartPreservesHistoryAndInterruptsActiveRun proves the recovery barrier:
// a process that vanished mid-run leaves the run active on disk, the next Open
// marks it interrupted without replaying the user message, and the committed
// history plus command receipts survive.
func TestRestartPreservesHistoryAndInterruptsActiveRun(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	p := &funcProvider{runFn: func(_ context.Context, _ provider.Request, emit func(string) error) error {
		calls.Add(1)
		if err := emit("partial"); err != nil {
			return err
		}
		<-release // deliberately ignores cancellation, like a wedged child process
		return nil
	}}

	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	first.shutdownGrace = 100 * time.Millisecond
	threadID := mustCreateThread(t, first, "create-1")
	sendCmd := sendCommand("send-1", threadID, "hello")
	sent, err := first.Dispatch(context.Background(), sendCmd)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	waitRunStatus(t, first, sent.RunID, api.RunRunning)
	waitFor(t, "partial output to be committed", func() bool {
		run, ok := findRun(snapshotOf(t, first), sent.RunID)
		return ok && run.Output == "partial"
	})
	eventsBefore := len(loadAllEvents(t, first))
	if eventsBefore == 0 {
		t.Fatal("expected committed events before the crash")
	}

	crashForTest(first)

	second := openCoreAt(t, path, p)
	defer func() {
		close(release)
		if err := second.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	run := waitRunStatus(t, second, sent.RunID, api.RunInterrupted)
	if run.Output != "partial" {
		t.Fatalf("output after restart = %q, want the committed prefix", run.Output)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider was invoked %d times across the restart, want 1 (no replay)", got)
	}

	snap := snapshotOf(t, second)
	if len(snap.Threads) != 1 || snap.Threads[0].ID != threadID {
		t.Fatalf("thread history lost across restart: %+v", snap.Threads)
	}
	if len(snap.Runs) != 1 {
		t.Fatalf("got %d runs after restart, want 1", len(snap.Runs))
	}

	// The durable log survives and now ends with the interruption event.
	events := loadAllEvents(t, second)
	if len(events) <= eventsBefore {
		t.Fatalf("event log shrank across restart: %d -> %d", eventsBefore, len(events))
	}
	if events[len(events)-1].Type != eventRunInterrupted {
		t.Fatalf("last event after restart = %q, want %q", events[len(events)-1].Type, eventRunInterrupted)
	}
	for i, ev := range events {
		if ev.Sequence != int64(i+1) {
			t.Fatalf("event %d has sequence %d, want %d", i, ev.Sequence, i+1)
		}
	}

	// Receipts are still authoritative: the identical retry is answered from disk
	// and does not start a second run.
	replayed, err := second.Dispatch(context.Background(), sendCmd)
	if err != nil {
		t.Fatalf("duplicate send after restart: %v", err)
	}
	if replayed != sent {
		t.Fatalf("receipt changed across restart: %+v vs %+v", sent, replayed)
	}
	if got := countRows(t, second, "runs"); got != 1 {
		t.Fatalf("got %d runs after the duplicate retry, want 1", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider called %d times after the duplicate retry, want 1", got)
	}

	// A fresh command on the recovered thread is accepted: the interruption
	// released the per-thread slot.
	next, err := second.Dispatch(context.Background(), sendCommand("send-2", threadID, "again"))
	if err != nil {
		t.Fatalf("send after recovery: %v", err)
	}
	if next.RunID == sent.RunID {
		t.Fatal("recovery reused the interrupted run id")
	}
}

// TestRecoveryAbandonsUnsettledEffects proves that queued (never claimed) work
// from a previous process is abandoned rather than executed.
func TestRecoveryAbandonsUnsettledEffects(t *testing.T) {
	p := emitProvider("x")
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")

	// Stop the worker first: the seed must not be claimed before the simulated
	// crash, otherwise this would test the happy path instead of recovery.
	first.workCancel()
	first.workWG.Wait()

	// Insert a queued run + pending effect directly, exactly what a crash between
	// the command commit and the worker claim leaves behind.
	if err := first.write(context.Background(), func(tx *txn) error {
		now := nowNanos()
		if _, err := tx.exec(`INSERT INTO runs
			(id, thread_id, attempt_id, active_attempt_id, status, input, output, error, created_at, updated_at)
			VALUES ('run-queued', ?, 'att-queued', 'att-queued', 'queued', 'unsent', '', '', ?, ?)`,
			threadID, now, now); err != nil {
			return err
		}
		if _, err := tx.exec(`INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at)
			VALUES ('att-queued', 'run-queued', 1, 'queued', ?, ?)`, now, now); err != nil {
			return err
		}
		_, err := tx.exec(`INSERT INTO effect_outbox
			(id, kind, thread_id, run_id, attempt_id, status, attempt_count, claim_token, lease_until, last_error, created_at, updated_at)
			VALUES ('eff-queued', 'provider.run', ?, 'run-queued', 'att-queued', 'pending', 0, '', 0, '', ?, ?)`,
			threadID, now, now)
		return err
	}); err != nil {
		t.Fatalf("seed queued work: %v", err)
	}

	crashForTest(first)

	second := openCoreAt(t, path, p)
	defer second.Close()

	run := waitRunStatus(t, second, "run-queued", api.RunInterrupted)
	if run.Output != "" {
		t.Fatalf("abandoned run gained output %q", run.Output)
	}
	if got := p.callCount(); got != 0 {
		t.Fatalf("provider was invoked %d times for abandoned work, want 0", got)
	}
	var status string
	if err := second.store.db.QueryRow(`SELECT status FROM effect_outbox WHERE id = 'eff-queued'`).Scan(&status); err != nil {
		t.Fatalf("read abandoned effect: %v", err)
	}
	if status != outboxAbandoned {
		t.Fatalf("effect status = %q, want %q", status, outboxAbandoned)
	}
}
