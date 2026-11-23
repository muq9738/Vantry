package desktop

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Run's arguments are checked before anything touches the application loop, so
// these stay unit tests: no window is opened here.
func TestRunRejectsIncompleteArguments(t *testing.T) {
	if err := Run(context.Background(), nil); err == nil {
		t.Error("Run without an api.Service should say so")
	}
	//lint:ignore SA1012 the point is that a missing context is refused
	if err := Run(nil, &fakeService{}); err == nil {
		t.Error("Run without a context should say so")
	}
}

// Closing the client releases only this client's work: its context is
// cancelled and its window is asked to close. Core is never called with a
// stop; api.Service has no such method and desktop does not add one.
func TestClientExitReleasesOnlyClientWork(t *testing.T) {
	f := &fakeService{}
	w := newWorkbench(f)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := newHost(ctx, w)

	quitCalled := false
	h.quit = func() { quitCalled = true }

	// The window never opened, so shutdown has to stop the loop instead.
	h.shutdown()

	if !quitCalled {
		t.Error("shutdown should stop the loop when no window exists")
	}
	select {
	case <-h.finished:
	default:
		t.Error("the client should report itself finished")
	}
	if w.ctx.Err() == nil {
		t.Error("the client context should be cancelled so the worker stops")
	}
	if _, _, seen := f.counts(); len(seen) != 0 {
		t.Errorf("Subscribe calls %v, want none: no window ever opened", seen)
	}
}

func TestWatchContextStopsWithTheLoop(t *testing.T) {
	w := newWorkbench(&fakeService{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHost(ctx, w)
	h.quit = func() {}

	stop := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		h.watchContext(stop)
		close(returned)
	}()
	close(stop)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("watchContext should stop when the loop ends")
	}
	if h.closing {
		t.Error("a loop that ended on its own must not look like a cancelled client")
	}
}

func TestClientContextIsScopedToTheWindow(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	w := newWorkbench(&fakeService{})
	h := newHost(parent, w)
	h.quit = func() {}
	if w.ctx == parent {
		t.Error("the workbench should hold a context this client can cancel alone")
	}
	cancelParent()
	if !errors.Is(w.ctx.Err(), context.Canceled) {
		t.Errorf("workbench context error = %v, want it to follow the caller's", w.ctx.Err())
	}
	if h.ctx.Err() == nil {
		t.Error("the host should see the caller's cancellation too")
	}
}
