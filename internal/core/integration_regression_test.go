package core

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

// A publisher may retain a subscriber pointer just before cancellation removes
// it from the hub. Reporting overflow through that pointer must stay safe after
// the delivery goroutine has already closed its public channels.
func TestSubscriberFailureAfterDeliveryExit(t *testing.T) {
	c := openTestCore(t, emitProvider("ok"))
	sub, err := c.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	c.hub.mu.Lock()
	var retained *subscriber
	for s := range c.hub.subs {
		retained = s
	}
	c.hub.mu.Unlock()
	if retained == nil {
		t.Fatal("subscriber was not registered")
	}
	sub.Close()
	retained.fail(errorf(api.ResyncRequired, "overflow concurrent with cancellation"))
}

// This calls the command transaction after its public entrance check, as when a
// command waits for the writer while Close changes the lifecycle to draining.
func TestCommandTransactionRechecksReadiness(t *testing.T) {
	c := openTestCore(t, emitProvider("ok"))
	command := api.Command{Version: 1, CommandID: "queued-at-shutdown", Kind: api.CreateThread, Title: "late"}
	c.state.Store(stateDraining)
	_, err := c.dispatchCreateThread(context.Background(), command)
	requireCode(t, err, api.NotReady)
	if snapshot := snapshotOf(t, c); len(snapshot.Threads) != 0 || snapshot.Sequence != 0 {
		t.Fatalf("command committed after draining: %+v", snapshot)
	}
}

// SQLite's foreign_keys and busy_timeout are per connection. Holding one pooled
// connection forces a second physical handle, whose settings must also hold.
func TestEverySQLiteConnectionEnforcesConstraints(t *testing.T) {
	c := openTestCore(t, emitProvider("ok"))
	ctx := context.Background()
	first, err := c.store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := c.store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.ExecContext(ctx, `INSERT INTO attempts
		(id, run_id, ordinal, status, created_at, updated_at)
		VALUES ('orphan', 'missing-run', 1, 'queued', 0, 0)`); err == nil {
		t.Error("a new SQLite connection accepted an attempt without its Run")
	}
	var busyTimeout int
	if err := second.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Errorf("new connection busy_timeout = %d, want 5000", busyTimeout)
	}
}

func TestSettlementFailureRetriesWithoutRepeatingProvider(t *testing.T) {
	p := emitProvider("committed output")
	c := openTestCore(t, p)
	var injected atomic.Bool
	c.setTestHookPreCommit(func(tx *sql.Tx) error {
		var completed int
		if err := tx.QueryRow("SELECT COUNT(1) FROM events WHERE type = ?", eventRunCompleted).Scan(&completed); err != nil {
			return err
		}
		if completed > 0 && injected.CompareAndSwap(false, true) {
			return errors.New("one transient settlement failure")
		}
		return nil
	})
	threadID := mustCreateThread(t, c, "create-settle-retry")
	receipt := mustSend(t, c, "send-settle-retry", threadID, "hello")
	run := waitRunStatus(t, c, receipt.RunID, api.RunCompleted)
	if !injected.Load() || run.Output != "committed output" || p.callCount() != 1 {
		t.Fatalf("settlement retry changed execution: injected=%v run=%+v calls=%d", injected.Load(), run, p.callCount())
	}
	// The recovered terminal state also releases the thread for its next Run.
	next := mustSend(t, c, "send-after-retry", threadID, "next")
	waitRunStatus(t, c, next.RunID, api.RunCompleted)
}
