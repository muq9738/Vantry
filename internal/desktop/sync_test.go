package desktop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func subscriptionFor(events chan api.Event, errs chan error) (*api.Subscription, *bool) {
	closed := false
	return &api.Subscription{
		Events: events,
		Errors: errs,
		Close:  func() { closed = true },
	}, &closed
}

func TestSyncOnceSubscribesAfterTheSnapshotCursor(t *testing.T) {
	f := &fakeService{snapshots: []api.Snapshot{snapshotOneRun()}}
	events := make(chan api.Event, 1)
	events <- deltaEvent(4, "t1", "r1", "理中")
	close(events)
	sub, closed := subscriptionFor(events, make(chan error))
	f.subscriptions = []*api.Subscription{sub}

	w := newWorkbench(f)
	reason, err := w.syncOnce(context.Background(), newResync())
	if err != nil {
		t.Fatalf("syncOnce error %v", err)
	}
	if reason != "事件流已关闭" {
		t.Errorf("reason = %q, want the closed stream", reason)
	}
	if _, _, seen := f.counts(); len(seen) != 1 || seen[0] != 3 {
		t.Errorf("Subscribe cursors %v, want the snapshot's 3", seen)
	}
	if got := w.runsFor("t1")[0].Output; got != "处理中" {
		t.Errorf("output = %q, want the streamed delta folded in", got)
	}
	if !*closed {
		t.Error("the subscription must be closed when the stream ends")
	}
}

func TestSyncOnceAsksForANewSnapshotAfterAStreamFailure(t *testing.T) {
	f := &fakeService{snapshots: []api.Snapshot{snapshotOneRun()}}
	errs := make(chan error, 1)
	errs <- &api.Error{Code: api.ResyncRequired, Message: "slow consumer"}
	sub, _ := subscriptionFor(make(chan api.Event), errs)
	f.subscriptions = []*api.Subscription{sub}

	w := newWorkbench(f)
	reason, err := w.syncOnce(context.Background(), newResync())
	if err != nil {
		t.Fatalf("syncOnce error %v", err)
	}
	if reason != "事件流已断开" {
		t.Errorf("reason = %q", reason)
	}
	if !strings.Contains(w.streamNotice.Text, "同步落后") {
		t.Errorf("stream notice = %+v, want actionable recovery feedback", w.streamNotice)
	}
	if w.streamNotice.Level != noticeWarn {
		t.Errorf("stream notice level = %v, want a warning", w.streamNotice.Level)
	}
}

func TestSyncOnceFailsBeforeSubscribingWhenSnapshotFails(t *testing.T) {
	f := &fakeService{snapshotErr: &api.Error{Code: api.NotReady, Message: "core still opening"}}
	w := newWorkbench(f)

	if _, err := w.syncOnce(context.Background(), newResync()); err == nil {
		t.Fatal("syncOnce should report the snapshot failure")
	}
	if _, _, seen := f.counts(); len(seen) != 0 {
		t.Errorf("Subscribe calls %v, want none before a snapshot succeeds", seen)
	}
}

func TestSyncWorkerRefetchesSnapshotAfterStreamEnds(t *testing.T) {
	first := make(chan api.Event)
	close(first)
	subFirst, closedFirst := subscriptionFor(first, nil)

	secondEvents := make(chan api.Event)
	subSecond, closedSecond := subscriptionFor(secondEvents, make(chan error))

	f := &fakeService{
		snapshots: []api.Snapshot{
			snapshotOneRun(),
			{
				Sequence: 8,
				Threads:  []api.Thread{thread("t1", "第一条线程", 0)},
				Runs:     []api.Run{run("r1", "t1", api.RunCompleted, "把标题改成蓝色", "处理完成", 0)},
			},
		},
		subscriptions: []*api.Subscription{subFirst, subSecond},
	}
	w := newWorkbench(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.syncWorker(ctx, newResync())
		close(done)
	}()

	waitFor(t, 5*time.Second, func() bool {
		snapshots, _, _ := f.counts()
		return snapshots >= 2
	}, "a second snapshot after the first stream ended")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the event worker did not return after its context ended")
	}

	_, _, seen := f.counts()
	if len(seen) < 2 || seen[0] != 3 || seen[1] != 8 {
		t.Errorf("Subscribe cursors %v, want 3 then the fresh snapshot's 8", seen)
	}
	if w.sequence != 8 {
		t.Errorf("cursor = %d, want 8", w.sequence)
	}
	if got := w.runsFor("t1")[0]; got.Output != "处理完成" || got.Status != api.RunCompleted {
		t.Errorf("run after recovery = %+v", got)
	}
	if !*closedFirst || !*closedSecond {
		t.Errorf("subscriptions left open: %v %v", *closedFirst, *closedSecond)
	}
	// A healthy reconnection clears the transient recovery banner.
	if w.streamNotice.valid() {
		t.Errorf("stream notice = %+v, want routine recovery feedback cleared", w.streamNotice)
	}
	if w.connNotice.Level != noticeInfo {
		t.Errorf("connection notice = %+v, want recovery after a good snapshot", w.connNotice)
	}
}

func TestSyncWorkerReportsAnUnreachableCore(t *testing.T) {
	f := &fakeService{
		snapshotErr: errors.New("dial unix /tmp/nope.sock: connect: no such file or directory"),
		healthErr:   errors.New("dial unix /tmp/nope.sock: connect: no such file or directory"),
	}
	w := newWorkbench(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.syncWorker(ctx, newResync())
		close(done)
	}()

	waitFor(t, 5*time.Second, func() bool {
		snapshots, _, _ := f.counts()
		return snapshots >= 1
	}, "the first snapshot attempt")
	cancel()
	<-done

	if w.connNotice.Level != noticeError {
		t.Errorf("connection notice = %+v", w.connNotice)
	}
	if !strings.Contains(w.connNotice.Text, "暂时不可用") || strings.Contains(w.connNotice.Text, "/tmp/") {
		t.Errorf("connection notice = %q, want actionable text without socket details", w.connNotice.Text)
	}
	if !strings.Contains(w.streamNotice.Text, "正在重新取快照") {
		t.Errorf("stream notice = %q, want the retry stated", w.streamNotice.Text)
	}
	if _, _, seen := f.counts(); len(seen) != 0 {
		t.Errorf("Subscribe cursors %v, want none while Core is unreachable", seen)
	}
}

func TestUnknownEventMakesTheWorkerResync(t *testing.T) {
	f := &fakeService{snapshots: []api.Snapshot{snapshotOneRun()}}
	events := make(chan api.Event, 1)
	events <- api.Event{
		Sequence: 4, ID: "ev-4", Type: "future.thing", ThreadID: "t1",
		Data: mustJSON(map[string]any{"anything": true}),
	}
	sub, _ := subscriptionFor(events, make(chan error))
	f.subscriptions = []*api.Subscription{sub}

	w := newWorkbench(f)
	res := newResync()
	reason, err := w.syncOnce(context.Background(), res)
	if err != nil {
		t.Fatalf("syncOnce error %v", err)
	}
	if !strings.Contains(reason, "无法折叠") {
		t.Errorf("reason = %q, want the unknown event to ask for a resync", reason)
	}
	if w.unknownEvents != 1 {
		t.Errorf("unknown events = %d, want 1 counted", w.unknownEvents)
	}
}

func TestHealthMismatchIsShown(t *testing.T) {
	f := &fakeService{health: api.Health{Version: 2, State: "ready"}}
	w := newWorkbench(f)
	w.checkHealth(context.Background())
	if w.healthNotice.Level != noticeError || !strings.Contains(w.healthNotice.Text, "版本") {
		t.Errorf("health notice = %+v", w.healthNotice)
	}

	f.health = api.Health{Version: api.ProtocolVersion, State: "ready"}
	w.checkHealth(context.Background())
	if w.healthNotice.valid() || w.connNotice.valid() {
		t.Errorf("healthy state should not create persistent success feedback: health=%+v conn=%+v", w.healthNotice, w.connNotice)
	}
}

func TestSyncWorkerRateBoundsRapidStreamEOFAndFastPathResync(t *testing.T) {
	origInit, origMax, origStable := syncStreamInitialBackoff, syncStreamMaxBackoff, syncStreamStableWindow
	syncStreamInitialBackoff = 45 * time.Millisecond
	syncStreamMaxBackoff = 120 * time.Millisecond
	syncStreamStableWindow = 10 * time.Second
	t.Cleanup(func() {
		syncStreamInitialBackoff = origInit
		syncStreamMaxBackoff = origMax
		syncStreamStableWindow = origStable
	})

	var subs []*api.Subscription
	for range 20 {
		ch := make(chan api.Event)
		close(ch)
		sub, _ := subscriptionFor(ch, nil)
		subs = append(subs, sub)
	}
	snaps := make([]api.Snapshot, 20)
	for i := range snaps {
		snaps[i] = snapshotOneRun()
	}
	f := &fakeService{snapshots: snaps, subscriptions: subs}
	w := newWorkbench(f)
	ctx, cancel := context.WithTimeout(context.Background(), 220*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.syncWorker(ctx, newResync())
		close(done)
	}()
	<-done
	count, _, _ := f.counts()
	if count >= 6 {
		t.Fatalf("rapid EOF loop ran %d snapshots in 220ms; expected rate-bounded backoff", count)
	}
}

func TestDrainStreamPreservesBufferedErrorWhenEventsClose(t *testing.T) {
	f := &fakeService{snapshots: []api.Snapshot{snapshotOneRun()}}
	events := make(chan api.Event)
	close(events)
	errs := make(chan error, 1)
	errs <- &api.Error{Code: api.ResyncRequired, Message: "cursor gap"}
	sub, _ := subscriptionFor(events, errs)
	f.subscriptions = []*api.Subscription{sub}

	w := newWorkbench(f)
	for range 10 {
		reason, err := w.syncOnce(context.Background(), newResync())
		if err != nil {
			t.Fatal(err)
		}
		if reason != "事件流已断开" {
			t.Fatalf("buffered error lost when events closed: reason=%q", reason)
		}
		if !strings.Contains(w.streamNotice.Text, "同步落后") {
			t.Fatalf("streamNotice=%+v, want error notice", w.streamNotice)
		}
		// Reset subscription for repeated checks to avoid select bias flakiness.
		events2 := make(chan api.Event)
		close(events2)
		errs2 := make(chan error, 1)
		errs2 <- &api.Error{Code: api.ResyncRequired, Message: "cursor gap"}
		sub2, _ := subscriptionFor(events2, errs2)
		f.mu.Lock()
		f.snapshots = append(f.snapshots, snapshotOneRun())
		f.subscriptions = append(f.subscriptions, sub2)
		f.mu.Unlock()
	}
}

func TestDrainStreamCoalescesStreamingDeltasIntoFrameBatches(t *testing.T) {
	orig := syncFrameCoalesceWindow
	syncFrameCoalesceWindow = 25 * time.Millisecond
	t.Cleanup(func() { syncFrameCoalesceWindow = orig })

	f := &fakeService{snapshots: []api.Snapshot{snapshotOneRun()}}
	events := make(chan api.Event, 32)
	sub, _ := subscriptionFor(events, make(chan error))
	f.subscriptions = []*api.Subscription{sub}

	w := newWorkbench(f)
	var posts atomic.Int32
	basePost := w.post
	w.post = func(fn func()) {
		posts.Add(1)
		basePost(fn)
	}

	done := make(chan string, 1)
	go func() {
		reason, _ := w.syncOnce(context.Background(), newResync())
		done <- reason
	}()

	var expected strings.Builder
	expected.WriteString("处理中")
	for i := 0; i < 20; i++ {
		chunk := fmt.Sprintf("[%d]", i)
		expected.WriteString(chunk)
		events <- deltaEvent(int64(4+i), "t1", "r1", chunk)
	}
	// Terminal non-delta event flushes the buffered deltas immediately in exact order.
	events <- api.Event{
		Sequence: 24,
		ID:       "ev-done",
		Type:     eventRunCompleted,
		ThreadID: "t1",
		RunID:    "r1",
		Data:     mustJSON(map[string]any{"run": run("r1", "t1", api.RunCompleted, "把标题改成蓝色", expected.String(), 0)}),
	}
	close(events)

	<-done

	// 1 snapshot + 1 noteSubscribed + coalesced batches (far fewer than 21).
	if got := posts.Load(); got > 5 {
		t.Fatalf("expected coalesced post count <= 5, got %d", got)
	}
	runs := w.runsFor("t1")
	if len(runs) != 1 || runs[0].Status != api.RunCompleted || runs[0].Output != expected.String() {
		t.Fatalf("coalescing dropped or reordered run output: %+v", runs)
	}
}
