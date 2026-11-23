package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func threadModelCommand(id, threadID, model string) api.Command {
	return api.Command{Version: api.ProtocolVersion, CommandID: id, Kind: api.SetThreadModel, ThreadID: threadID, Model: model}
}

func setThreadModelForTest(t *testing.T, c *Core, cmd api.Command) api.Receipt {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatalf("set thread model: %v", err)
	}
	return receipt
}

// Capture whole rows so switching models cannot silently rewrite an attempt or
// outbox input, claim, status or timestamp while its provider remains active.
func threadModelExecutionRows(t *testing.T, c *Core, runID string) []string {
	t.Helper()
	var result []string
	for _, table := range []string{"runs", "attempts", "effect_outbox"} {
		key := "run_id"
		if table == "runs" {
			key = "id"
		}
		rows, err := c.store.db.Query("SELECT * FROM "+table+" WHERE "+key+" = ? ORDER BY id", runID)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			raw, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result = append(result, table+":"+string(raw))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestThreadModelPreservesActiveAndQueuedExecutionAndUpdatesNewInputs(t *testing.T) {
	gate := make(chan struct{})
	fakeProvider := &funcProvider{runFn: func(ctx context.Context, req provider.Request, emit func(string) error) error {
		if req.Message == "active" {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return emit("done")
	}}
	c := openTestCore(t, fakeProvider)
	created := dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "create", Kind: api.CreateThread,
		Title: "keep title", ProviderID: "pi", Model: "old-model", ApprovalPolicy: "ask",
		Workspace: &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()},
	})
	threadBefore := snapshotOf(t, c).Threads[0]
	active := mustSend(t, c, "active", created.ThreadID, "active")
	waitRunStatus(t, c, active.RunID, api.RunRunning)
	waitFor(t, "active provider call", func() bool { return fakeProvider.callCount() == 1 })
	queued := dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "queue", Kind: api.QueueMessage,
		ThreadID: created.ThreadID, Message: "pending queue",
	})
	if queued.RunID != "" {
		t.Fatal("queued message already became a run")
	}
	input, found := findInput(snapshotOf(t, c), queued.InputID)
	if !found || input.Configuration == nil || input.Configuration.Model != "old-model" || input.Configuration.ProviderID != "pi" || input.Configuration.ApprovalPolicy != "ask" {
		t.Fatalf("queued settings were not captured: %+v", input)
	}
	before := threadModelExecutionRows(t, c, active.RunID)
	receipt := setThreadModelForTest(t, c, threadModelCommand("model", created.ThreadID, "new-model"))
	if after := threadModelExecutionRows(t, c, active.RunID); !reflect.DeepEqual(after, before) {
		t.Fatalf("model command changed existing execution rows:\nbefore %v\nafter %v", before, after)
	}
	wantThread := threadBefore
	wantThread.Model = "new-model"
	if got := snapshotOf(t, c).Threads[0]; !reflect.DeepEqual(got, wantThread) {
		t.Fatalf("thread = %+v, want %+v", got, wantThread)
	}
	var eventType string
	var data []byte
	if err := c.store.db.QueryRow(`SELECT type, data FROM events WHERE sequence = ?`, receipt.Sequence).Scan(&eventType, &data); err != nil {
		t.Fatal(err)
	}
	var payload threadEventData
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if eventType != "thread.updated" || !reflect.DeepEqual(payload.Thread, wantThread) || !receipt.Accepted || receipt.RunID != "" {
		t.Fatalf("unexpected event/receipt: %s %s %+v", eventType, data, receipt)
	}
	close(gate)
	waitRunTerminal(t, c, active.RunID)
	waitFor(t, "pending queue to finish", func() bool {
		for _, run := range snapshotOf(t, c).Runs {
			if run.Input == "pending queue" {
				return run.Model == "old-model" && run.Status == api.RunCompleted
			}
		}
		return false
	})
	next := mustSend(t, c, "next", created.ThreadID, "next send")
	if run := waitRunTerminal(t, c, next.RunID); run.Model != "new-model" {
		t.Fatalf("next run model = %q", run.Model)
	}
	if fakeProvider.callCount() != 3 {
		t.Fatalf("provider calls = %d, want 3", fakeProvider.callCount())
	}
	for i, want := range []string{"old-model", "old-model", "new-model"} {
		if req := fakeProvider.requestAt(i); req.Model != want {
			t.Fatalf("provider request %d model = %q, want %q", i, req.Model, want)
		}
	}
}

func TestThreadModelNewDelegateInheritsCurrentThread(t *testing.T) {
	fakeProvider := &funcProvider{runFn: func(ctx context.Context, req provider.Request, emit func(string) error) error {
		if req.Message == "parent" {
			<-ctx.Done()
			return ctx.Err()
		}
		return emit("child result")
	}}
	c := openTestCore(t, fakeProvider)
	created := dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "create", Kind: api.CreateThread, ProviderID: "pi", Model: "old-model",
	})
	parent := mustSend(t, c, "parent", created.ThreadID, "parent")
	active := waitRunStatus(t, c, parent.RunID, api.RunRunning)
	setThreadModelForTest(t, c, threadModelCommand("model", created.ThreadID, "new-model"))
	child := dispatchOrFail(t, c, delegateCommand("delegate", parent.RunID, active.AttemptID, "child", "manual"))
	if run := waitRunTerminal(t, c, child.RunID); run.Model != "new-model" {
		t.Fatalf("child run model = %q", run.Model)
	}
	snap := snapshotOf(t, c)
	for _, thread := range snap.Threads {
		if thread.ID == child.ThreadID && (thread.Model != "new-model" || thread.ParentThreadID != created.ThreadID) {
			t.Fatalf("child thread = %+v", thread)
		}
	}
	if run, _ := findRun(snap, parent.RunID); run.Model != "old-model" || run.AttemptID != active.AttemptID {
		t.Fatalf("parent changed: %+v", run)
	}
	for i := 0; i < fakeProvider.callCount(); i++ {
		if req := fakeProvider.requestAt(i); req.RunID == child.RunID && req.Model != "new-model" {
			t.Fatalf("child provider model = %q", req.Model)
		}
	}
}

func TestThreadModelDefaultsRestartAndIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	open := func(defaultModel string) *Core {
		t.Helper()
		c, err := Open(context.Background(), Options{DBPath: path, Provider: &funcProvider{}, DefaultProviderID: "pi", DefaultModel: defaultModel})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	c := open("default-a")
	threadID := mustCreateThread(t, c, "create")
	setThreadModelForTest(t, c, threadModelCommand("explicit", threadID, "explicit-model"))
	cmd := threadModelCommand("default", threadID, "")
	receipt := setThreadModelForTest(t, c, cmd)
	before := snapshotOf(t, c)
	if before.Threads[0].Model != "default-a" {
		t.Fatalf("empty model resolved to %q", before.Threads[0].Model)
	}
	if replay := setThreadModelForTest(t, c, cmd); replay != receipt {
		t.Fatalf("receipt replay = %+v, want %+v", replay, receipt)
	}
	changed := cmd
	changed.Model = "different"
	_, err := c.dispatchSetThreadModel(context.Background(), changed)
	requireCode(t, err, api.Conflict)
	changed = cmd
	changed.Title = "different ignored field"
	_, err = c.dispatchSetThreadModel(context.Background(), changed)
	requireCode(t, err, api.Conflict)
	if after := snapshotOf(t, c); !reflect.DeepEqual(after, before) {
		t.Fatal("retry or conflict changed the projection or event sequence")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := open("default-b")
	if got := snapshotOf(t, restarted).Threads[0]; !reflect.DeepEqual(got, before.Threads[0]) {
		t.Fatalf("restart changed thread: %+v", got)
	}
	if replay := setThreadModelForTest(t, restarted, cmd); replay != receipt {
		t.Fatalf("restart receipt replay = %+v, want %+v", replay, receipt)
	}
	run := mustSend(t, restarted, "send", threadID, "after restart")
	if got := waitRunTerminal(t, restarted, run.RunID); got.Model != "default-a" {
		t.Fatalf("restarted run model = %q", got.Model)
	}
	setThreadModelForTest(t, restarted, threadModelCommand("new-default", threadID, ""))
	if got := snapshotOf(t, restarted).Threads[0].Model; got != "default-b" {
		t.Fatalf("new default model = %q", got)
	}
}

func TestThreadModelRejectsMissingUnknownAndControlledThreads(t *testing.T) {
	c := openTestCore(t, &funcProvider{})
	for _, threadID := range []string{"", " \t"} {
		_, err := c.dispatchSetThreadModel(context.Background(), threadModelCommand("missing", threadID, "model"))
		requireCode(t, err, api.InvalidCommand)
	}
	_, err := c.dispatchSetThreadModel(context.Background(), threadModelCommand("unknown", "unknown", "model"))
	requireCode(t, err, api.NotFound)
	controlled := dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "controlled", Kind: api.CreateThread, ProviderID: "controlled",
	})
	before := snapshotOf(t, c)
	_, err = c.dispatchSetThreadModel(context.Background(), threadModelCommand("switch", controlled.ThreadID, "real-model"))
	requireCode(t, err, api.InvalidCommand)
	if after := snapshotOf(t, c); !reflect.DeepEqual(after, before) {
		t.Fatal("rejected model switch changed projection or events")
	}
	if countRows(t, c, "command_receipts") != 1 || countRows(t, c, "effect_outbox") != 0 {
		t.Fatal("rejected commands wrote a receipt or effect")
	}
	setThreadModelForTest(t, c, threadModelCommand("noop", controlled.ThreadID, ""))
	if got := snapshotOf(t, c).Threads[0].Model; got != "" {
		t.Fatalf("controlled no-op changed model to %q", got)
	}
}

func TestThreadModelTransactionRollback(t *testing.T) {
	c := openTestCore(t, &funcProvider{})
	threadID := mustCreateThread(t, c, "create")
	before := snapshotOf(t, c)
	injected := errors.New("reject commit")
	c.setTestHookPreCommit(func(*sql.Tx) error { return injected })
	cmd := threadModelCommand("model", threadID, "new-model")
	_, err := c.dispatchSetThreadModel(context.Background(), cmd)
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v, want injected failure", err)
	}
	c.setTestHookPreCommit(nil)
	if after := snapshotOf(t, c); !reflect.DeepEqual(after, before) || countRows(t, c, "command_receipts") != 1 {
		t.Fatal("failed transaction left a projection, event or receipt")
	}
	setThreadModelForTest(t, c, cmd)
}
