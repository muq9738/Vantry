package core

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func pruningBoundary(t *testing.T, c *Core) int64 {
	t.Helper()
	var through int64
	if err := c.store.db.QueryRow(`SELECT int_value FROM core_metadata WHERE key = 'pruned_through_sequence'`).Scan(&through); err != nil {
		t.Fatal(err)
	}
	return through
}

func TestPruneEventsPreservesProjectionsAndSubscriptionCursors(t *testing.T) {
	ctx := context.Background()
	c := openTestCore(t, emitProvider("你", "好", "世界", "!"))
	threadID := mustCreateThread(t, c, "create")
	sent := mustSend(t, c, "send", threadID, "searchable input")
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
	before := snapshotOf(t, c)
	count, through, err := c.PruneEvents(ctx, 3)
	if err != nil || count != before.Sequence-3 || through != before.Sequence-3 {
		t.Fatalf("prune = %d, %d, %v; high-water %d", count, through, err, before.Sequence)
	}
	if got := countRows(t, c, "events"); got != 3 {
		t.Fatalf("retained %d events, want 3", got)
	}
	if after := snapshotOf(t, c); !reflect.DeepEqual(before, after) {
		t.Fatalf("pruning changed snapshot: before %+v, after %+v", before, after)
	}
	hits, err := c.SearchThreads(ctx, api.SearchQuery{Query: "searchable"})
	if err != nil || len(hits) != 1 || hits[0].RunID != sent.RunID {
		t.Fatalf("pruning lost searchable projection: %+v %v", hits, err)
	}
	stale, err := c.Subscribe(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if code, ok := readSubscriptionError(t, stale); !ok || code != api.ResyncRequired {
		t.Fatalf("stale cursor error = %q, %v", code, ok)
	}
	if _, open := <-stale.Events; open {
		t.Fatal("stale subscriber received events before resync")
	}
	for _, cursor := range []int64{0, through, through + 1} {
		sub, err := c.Subscribe(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		first := max(cursor+1, through+1)
		events := readEvents(t, sub, int(before.Sequence-first+1))
		for i, event := range events {
			if event.Sequence != first+int64(i) {
				t.Fatalf("cursor %d replayed sequence %d, want %d", cursor, event.Sequence, first+int64(i))
			}
		}
		sub.Close()
	}
	live, err := c.Subscribe(ctx, before.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	mustCreateThread(t, c, "live-after-pruning")
	if event := readEvents(t, live, 1)[0]; event.Sequence != before.Sequence+1 {
		t.Fatalf("fresh snapshot cursor missed live event: %+v", event)
	}
	count, throughAgain, err := c.PruneEvents(ctx, 1000)
	if err != nil || count != 0 || throughAgain != through {
		t.Fatalf("increased retention moved boundary: %d %d %v", count, throughAgain, err)
	}
}

func TestPruneEventsEmptyLogKeepsHighWaterAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "core.db")
	c := openCoreAt(t, path, emitProvider())
	defer c.Close()
	for _, id := range []string{"one", "two", "three"} {
		mustCreateThread(t, c, id)
	}
	before := snapshotOf(t, c)
	count, through, err := c.PruneEvents(ctx, 0)
	if err != nil || count != before.Sequence || through != before.Sequence || countRows(t, c, "events") != 0 {
		t.Fatalf("prune entire log = %d %d %v", count, through, err)
	}
	if after := snapshotOf(t, c); !reflect.DeepEqual(before, after) {
		t.Fatalf("empty log reset snapshot high-water: %+v", after)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openCoreAt(t, path, emitProvider())
	defer reopened.Close()
	if after := snapshotOf(t, reopened); !reflect.DeepEqual(before, after) || pruningBoundary(t, reopened) != through {
		t.Fatalf("restart lost retention metadata or projection: %+v", after)
	}
	if count, again, err := reopened.PruneEvents(ctx, 0); err != nil || count != 0 || again != through {
		t.Fatalf("repeated prune = %d %d %v", count, again, err)
	}
	sub, err := reopened.Subscribe(ctx, through)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	mustCreateThread(t, reopened, "after-restart")
	if event := readEvents(t, sub, 1)[0]; event.Sequence != through+1 {
		t.Fatalf("sequence reused after pruning/restart: %+v", event)
	}
}

func TestPruneEventsRollbackValidationAndLifecycle(t *testing.T) {
	ctx := context.Background()
	c := openTestCore(t, emitProvider())
	mustCreateThread(t, c, "one")
	mustCreateThread(t, c, "two")
	before := snapshotOf(t, c)
	injected := errors.New("pruning rollback")
	c.setTestHookPreCommit(func(*sql.Tx) error { return injected })
	if count, through, err := c.PruneEvents(ctx, 0); !errors.Is(err, injected) || count != 0 || through != 0 {
		t.Fatalf("failed transaction reported committed pruning: %d %d %v", count, through, err)
	}
	c.setTestHookPreCommit(nil)
	if after := snapshotOf(t, c); !reflect.DeepEqual(before, after) || pruningBoundary(t, c) != 0 || countRows(t, c, "events") != int(before.Sequence) {
		t.Fatalf("rollback changed events or metadata: %+v", after)
	}
	if _, _, err := c.PruneEvents(ctx, -1); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("negative retention accepted: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.PruneEvents(cancelled, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prune = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PruneEvents(ctx, 0); !errors.Is(err, errStoreClosing) && api.CodeOf(err) != api.NotReady {
		t.Fatalf("pruning after close = %v", err)
	}
}

func TestSubscriptionDetectsPruningBetweenReplayPages(t *testing.T) {
	ctx := context.Background()
	c := openTestCore(t, emitProvider())
	threadID := mustCreateThread(t, c, "seed")
	if err := c.write(ctx, func(tx *txn) error {
		for i := 0; i < eventReplayPageSize*3; i++ {
			if _, err := tx.event(eventThreadCreated, threadID, "", "", map[string]int{"index": i}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := c.Subscribe(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	readEvents(t, sub, 1) // pins the first loaded page; its next emit now blocks
	if _, through, err := c.PruneEvents(ctx, 1); err != nil || through <= eventReplayPageSize+1 {
		t.Fatalf("did not prune past the next page: %d %v", through, err)
	}
	readEvents(t, sub, eventReplayPageSize-1)
	if code, ok := readSubscriptionError(t, sub); !ok || code != api.ResyncRequired {
		t.Fatalf("mid-replay pruning = %q, %v", code, ok)
	}
	select {
	case _, open := <-sub.Events:
		if open {
			t.Fatal("replay silently skipped pruned history")
		}
	case <-time.After(testTimeout):
		t.Fatal("pruned subscriber did not disconnect")
	}
}

func TestCoreWorkerAutomaticallyPrunesEvents(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Options{
		DBPath:             filepath.Join(t.TempDir(), "core.db"),
		Provider:           emitProvider("ok"),
		EventRetention:     4,
		EventPruneInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	threadID := mustCreateThread(t, c, "auto-prune-th")
	if err := c.write(ctx, func(tx *txn) error {
		for i := 0; i < 12; i++ {
			if _, err := tx.event(eventThreadCreated, threadID, "", "", map[string]int{"index": i}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "automatic background event pruning", func() bool {
		return pruningBoundary(t, c) > 0 && countRows(t, c, "events") <= 4
	})
}
