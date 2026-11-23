package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

// Cancellation must leave neither a receipt nor an open SQLite transaction on
// the reserved writer. The next command must succeed on that same connection.
func TestCancelledCommandRollsBackBeforeNextWriter(t *testing.T) {
	c := openTestCore(t, emitProvider("unused"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.setTestHookPreCommit(func(*sql.Tx) error {
		cancel()
		return nil
	})
	command := api.Command{Version: 1, CommandID: "cancelled-create", Kind: api.CreateThread, Title: "cancelled"}
	if _, err := c.Dispatch(ctx, command); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled transaction: %v", err)
	}
	c.setTestHookPreCommit(nil)
	if got := len(snapshotOf(t, c).Threads); got != 0 {
		t.Fatalf("cancelled command committed %d threads", got)
	}
	if _, err := c.Dispatch(context.Background(), command); err != nil {
		t.Fatalf("next command could not reuse the writer: %v", err)
	}
}

func TestCancellationDuringBeginDoesNotPoisonWriter(t *testing.T) {
	c := openTestCore(t, emitProvider("unused"))
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { cancel(); close(done) }()
		_, err := c.Dispatch(ctx, api.Command{Version: 1, Kind: api.CreateThread, CommandID: fmt.Sprintf("cancel-%d", i)})
		<-done
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel %d: %v", i, err)
		}
		if _, err := c.Dispatch(context.Background(), api.Command{Version: 1, Kind: api.CreateThread, CommandID: fmt.Sprintf("after-%d", i)}); err != nil {
			t.Fatalf("writer remained in a transaction after cancellation %d: %v", i, err)
		}
	}
}
