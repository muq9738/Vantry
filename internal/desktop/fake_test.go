package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// fakeService stands in for Core: it records what the workbench asks for and
// answers from data the test supplies.
type fakeService struct {
	mu sync.Mutex

	health      api.Health
	healthErr   error
	snapshots   []api.Snapshot
	snapshotErr error

	subscriptions []*api.Subscription
	subscribeErr  error

	created       int
	receipt       func(api.Command) (api.Receipt, error)
	receiptErr    error
	commandCalls  []api.Command
	snapshotCalls int
	subscribeSeen []int64
}

func (f *fakeService) Health(context.Context) (api.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health, f.healthErr
}

func (f *fakeService) Snapshot(context.Context) (api.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempt := f.snapshotCalls
	f.snapshotCalls++
	if f.snapshotErr != nil {
		return api.Snapshot{}, f.snapshotErr
	}
	if attempt >= len(f.snapshots) {
		if len(f.snapshots) == 0 {
			return api.Snapshot{}, nil
		}
		return f.snapshots[len(f.snapshots)-1], nil
	}
	return f.snapshots[attempt], nil
}

func (f *fakeService) Subscribe(_ context.Context, after int64) (*api.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	f.subscribeSeen = append(f.subscribeSeen, after)
	index := len(f.subscribeSeen) - 1
	if index >= len(f.subscriptions) {
		if len(f.subscriptions) == 0 {
			return nil, &api.Error{Code: api.InternalError, Message: "测试未准备订阅"}
		}
		index = len(f.subscriptions) - 1
	}
	return f.subscriptions[index], nil
}

func (f *fakeService) Dispatch(_ context.Context, cmd api.Command) (api.Receipt, error) {
	f.mu.Lock()
	f.commandCalls = append(f.commandCalls, cmd)
	receipt := f.receipt
	err := f.receiptErr
	f.mu.Unlock()
	if err != nil {
		return api.Receipt{}, err
	}
	if receipt != nil {
		return receipt(cmd)
	}
	// The defaults answer each command the way the contract says Core would, so
	// a test only spells out what it is checking.
	switch cmd.Kind {
	case api.CreateThread:
		f.created++
		return api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  fmt.Sprintf("t-new-%d", f.created),
			Sequence:  1,
		}, nil
	case api.SendMessage:
		return api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  cmd.ThreadID,
			RunID:     "run-from-cmd",
			Sequence:  1,
		}, nil
	default:
		return api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  cmd.ThreadID,
			RunID:     cmd.RunID,
			Sequence:  1,
		}, nil
	}
}

func (f *fakeService) counts() (snapshots int, commands int, subscribes []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls, len(f.commandCalls), append([]int64(nil), f.subscribeSeen...)
}

func (f *fakeService) dispatched() []api.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Command(nil), f.commandCalls...)
}

// heldWork keeps RPCs queued so a command can stay in flight while the next
// click is checked.
type heldWork struct{ calls []func() }

func (h *heldWork) release() {
	for len(h.calls) > 0 {
		fn := h.calls[0]
		h.calls = h.calls[1:]
		fn()
	}
}

func holdWork(w *workbench) *heldWork {
	h := &heldWork{}
	w.start = func(fn func()) { h.calls = append(h.calls, fn) }
	return h
}

var baseTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func thread(id, title string, offset time.Duration) api.Thread {
	return api.Thread{ID: id, Title: title, CreatedAt: baseTime.Add(offset)}
}

func run(id, threadID string, status api.RunStatus, input, output string, offset time.Duration) api.Run {
	at := baseTime.Add(offset)
	return api.Run{
		ID:        id,
		ThreadID:  threadID,
		AttemptID: id + "-attempt",
		Status:    status,
		Input:     input,
		Output:    output,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

// statusEventName maps a status to the event Core commits for it.
func statusEventName(status api.RunStatus) string {
	switch status {
	case api.RunQueued:
		return eventRunQueued
	case api.RunRunning:
		return eventRunStarted
	case api.RunCompleted:
		return eventRunCompleted
	case api.RunCancelled:
		return eventRunCancelled
	case api.RunInterrupted:
		return eventRunInterrupted
	case api.RunFailed:
		return eventRunFailed
	}
	return "run.status"
}

// statusEvent writes Core's committed entity envelope: {"run": api.Run}.
func statusEvent(sequence int64, r api.Run) api.Event {
	return api.Event{
		Sequence:  sequence,
		ID:        fmt.Sprintf("ev-%d", sequence),
		Type:      statusEventName(r.Status),
		ThreadID:  r.ThreadID,
		RunID:     r.ID,
		Timestamp: r.UpdatedAt,
		Data:      mustJSON(map[string]any{"run": r}),
	}
}

func threadEvent(sequence int64, t api.Thread) api.Event {
	return api.Event{
		Sequence:  sequence,
		ID:        fmt.Sprintf("ev-%d", sequence),
		Type:      eventThreadCreated,
		ThreadID:  t.ID,
		Timestamp: t.CreatedAt,
		Data:      mustJSON(map[string]any{"thread": t}),
	}
}

// outputEvent writes Core's run.output payload: {delta, output}.
func outputEvent(sequence int64, threadID, runID, delta, full string) api.Event {
	return api.Event{
		Sequence:  sequence,
		ID:        fmt.Sprintf("ev-%d", sequence),
		Type:      eventRunOutput,
		ThreadID:  threadID,
		RunID:     runID,
		Timestamp: baseTime,
		Data:      mustJSON(map[string]string{"delta": delta, "output": full}),
	}
}

func deltaEvent(sequence int64, threadID, runID, delta string) api.Event {
	return outputEvent(sequence, threadID, runID, delta, "")
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
