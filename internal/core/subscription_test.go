package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// TestSubscriptionReplayThenLive proves the subscription handshake: replay from
// the cursor covers the durable log without a gap, and the same subscription
// then continues with live events. A second subscription created at a snapshot
// cursor sees only newer events.
func TestSubscriptionReplayThenLive(t *testing.T) {
	p := emitProvider("a", "b", "c")
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	first := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, first.RunID, api.RunCompleted)

	snap := snapshotOf(t, c)
	if snap.Sequence == 0 {
		t.Fatal("snapshot reported no event sequence")
	}

	all, err := c.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatalf("subscribe from zero: %v", err)
	}
	defer all.Close()

	replayed := readEvents(t, all, int(snap.Sequence))
	for i, ev := range replayed {
		if ev.Sequence != int64(i+1) {
			t.Fatalf("event %d has sequence %d, want %d", i, ev.Sequence, i+1)
		}
	}
	wantTypes := []string{
		eventThreadCreated, eventRunQueued, eventRunStarted,
		eventRunOutput, eventRunOutput, eventRunOutput, eventRunCompleted,
	}
	if len(replayed) != len(wantTypes) {
		t.Fatalf("replayed %d events, want %d", len(replayed), len(wantTypes))
	}
	for i, want := range wantTypes {
		if replayed[i].Type != want {
			t.Fatalf("event %d type = %q, want %q", i, replayed[i].Type, want)
		}
	}
	// Run events carry the run and its attempt identity separately.
	run, ok := findRun(snap, first.RunID)
	if !ok || run.AttemptID == "" {
		t.Fatalf("run projection is missing its attempt identity: %+v", run)
	}
	for _, ev := range replayed {
		if ev.Type == eventThreadCreated {
			continue
		}
		if ev.RunID != first.RunID {
			t.Fatalf("event %q belongs to run %q, want %q", ev.Type, ev.RunID, first.RunID)
		}
		if ev.AttemptID != run.AttemptID {
			t.Fatalf("event %q carries attempt %q, want %q", ev.Type, ev.AttemptID, run.AttemptID)
		}
	}

	// A subscription created at the snapshot cursor must not replay history.
	live, err := c.Subscribe(context.Background(), snap.Sequence)
	if err != nil {
		t.Fatalf("subscribe from snapshot cursor: %v", err)
	}
	defer live.Close()

	second := mustSend(t, c, "send-2", threadID, "again")
	waitRunStatus(t, c, second.RunID, api.RunCompleted)

	const secondRunEvents = 6 // queued, started, 3 deltas, completed
	liveEvents := readEvents(t, live, secondRunEvents)
	replayedLive := readEvents(t, all, secondRunEvents)
	for i := range liveEvents {
		if liveEvents[i].Sequence <= snap.Sequence {
			t.Fatalf("live subscription received replayed event %d", liveEvents[i].Sequence)
		}
		if liveEvents[i].Sequence != replayedLive[i].Sequence || liveEvents[i].Type != replayedLive[i].Type {
			t.Fatalf("event %d differs between subscriptions: %+v vs %+v", i, liveEvents[i], replayedLive[i])
		}
		if liveEvents[i].RunID != second.RunID {
			t.Fatalf("live event belongs to run %q, want %q", liveEvents[i].RunID, second.RunID)
		}
	}
	if got := liveEvents[len(liveEvents)-1].Type; got != eventRunCompleted {
		t.Fatalf("last live event = %q, want %q", got, eventRunCompleted)
	}
}

// TestSubscriptionCloseReleases proves Close is idempotent, releases the
// delivery goroutine and unregisters the subscriber.
func TestSubscriptionCloseReleases(t *testing.T) {
	c := openTestCore(t, emitProvider("a"))
	if got := c.hub.count(); got != 0 {
		t.Fatalf("hub has %d subscribers before subscribing", got)
	}
	sub, err := c.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if got := c.hub.count(); got != 1 {
		t.Fatalf("hub has %d subscribers, want 1", got)
	}
	sub.Close()
	sub.Close() // must be safe to call repeatedly
	if got := c.hub.count(); got != 0 {
		t.Fatalf("hub still has %d subscribers after Close", got)
	}
	if _, ok := <-sub.Events; ok {
		t.Fatal("event channel is still open after Close")
	}
	if _, ok := <-sub.Errors; ok {
		t.Fatal("error channel is still open after Close")
	}
}

// TestSubscriptionContextCancelReleases proves that cancelling the subscription
// context releases the same resources as an explicit Close.
func TestSubscriptionContextCancelReleases(t *testing.T) {
	c := openTestCore(t, emitProvider("a"))
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := c.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if got := c.hub.count(); got != 1 {
		t.Fatalf("hub has %d subscribers, want 1", got)
	}
	cancel()
	waitFor(t, "the subscription to be released", func() bool { return c.hub.count() == 0 })
	select {
	case _, ok := <-sub.Events:
		if ok {
			t.Fatal("received an event after cancellation")
		}
	case <-time.After(testTimeout):
		t.Fatal("event channel was not closed after cancellation")
	}
}

// TestSlowSubscriberReceivesResyncRequired proves the bounded-buffer policy: a
// consumer that never reads is disconnected with an explicit resync_required
// instead of blocking commits or silently dropping events.
func TestSlowSubscriberReceivesResyncRequired(t *testing.T) {
	deltas := make([]string, 40)
	for i := range deltas {
		deltas[i] = "d"
	}
	c := openTestCore(t, emitProvider(deltas...))
	c.subBufferSize = 4 // small on purpose so the overflow is deterministic

	threadID := mustCreateThread(t, c, "create-1")
	sub, err := c.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	// The consumer deliberately does not read while the run produces events.
	sent := mustSend(t, c, "send-1", threadID, "hello")

	code, ok := readSubscriptionError(t, sub)
	if !ok {
		t.Fatal("slow subscriber was disconnected without an error")
	}
	if code != api.ResyncRequired {
		t.Fatalf("slow subscriber error = %q, want %q", code, api.ResyncRequired)
	}

	waitRunStatus(t, c, sent.RunID, api.RunCompleted)

	// Resynchronising from a fresh snapshot cursor works: the full log is still
	// available, nothing was pruned.
	fresh, err := c.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	defer fresh.Close()
	events := readEvents(t, fresh, int(snapshotOf(t, c).Sequence))
	if len(events) < len(deltas) {
		t.Fatalf("resync replayed %d events, want at least %d", len(events), len(deltas))
	}
}

// TestSubscriptionHasNoGapUnderConcurrentWrites proves the handshake under
// pressure: the replay of an existing log runs while new events are committed,
// and the delivered sequence stays strictly contiguous with no duplicates and
// no gaps.
func TestSubscriptionHasNoGapUnderConcurrentWrites(t *testing.T) {
	// Enough events that the replay of the existing log spans several pages
	// (eventReplayPageSize) while the live run is producing more.
	const deltas = 300
	filler := make([]string, deltas)
	for i := range filler {
		filler[i] = "x"
	}
	c := openTestCore(t, emitProvider(filler...))
	threadID := mustCreateThread(t, c, "create-1")

	// Seed a non-trivial log so the replay is still running when the live events
	// start arriving.
	seeded := mustSend(t, c, "send-1", threadID, "one")
	waitRunStatus(t, c, seeded.RunID, api.RunCompleted)

	sub, err := c.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	type collected struct {
		mu     sync.Mutex
		events []api.Event
	}
	got := &collected{}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case ev, ok := <-sub.Events:
				if !ok {
					return
				}
				got.mu.Lock()
				got.events = append(got.events, ev)
				got.mu.Unlock()
			case <-time.After(testTimeout):
				return
			}
		}
	}()

	live := mustSend(t, c, "send-2", threadID, "two")
	waitRunStatus(t, c, live.RunID, api.RunCompleted)
	final := snapshotOf(t, c).Sequence

	waitFor(t, "the reader to receive the whole log", func() bool {
		got.mu.Lock()
		defer got.mu.Unlock()
		return int64(len(got.events)) >= final
	})

	sub.Close()
	<-readerDone

	got.mu.Lock()
	defer got.mu.Unlock()
	if seq := int64(len(got.events)); seq != final {
		t.Fatalf("received %d events, want exactly %d", seq, final)
	}
	for i, ev := range got.events {
		if ev.Sequence != int64(i+1) {
			t.Fatalf("event %d has sequence %d, want %d (gap or duplicate)", i, ev.Sequence, i+1)
		}
	}
}
