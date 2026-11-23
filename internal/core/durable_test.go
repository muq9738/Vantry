package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"

	_ "modernc.org/sqlite"
)

// durableProvider is a controllable provider.Durable implementation. Its
// RunEvents hook decides what the independent "worker" emits, and every request
// is recorded so tests can assert the persisted cursor given to a re-attachment.
type durableProvider struct {
	recoverable bool
	runEvents   func(ctx context.Context, req provider.Request, emit func(provider.Event) error) error

	mu    sync.Mutex
	calls []provider.Request
	stops []provider.Request
}

func (p *durableProvider) Run(context.Context, provider.Request, func(string) error) error {
	return errors.New("durable provider must be driven through RunEvents")
}

func (p *durableProvider) RunEvents(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	if p.runEvents != nil {
		return p.runEvents(ctx, req, emit)
	}
	return emit(provider.Event{Sequence: req.AfterSequence + 1, Delta: "ok"})
}

func (p *durableProvider) Recoverable(provider.Request) bool { return p.recoverable }

func (p *durableProvider) Stop(_ context.Context, req provider.Request) error {
	p.mu.Lock()
	p.stops = append(p.stops, req)
	p.mu.Unlock()
	return nil
}

func (p *durableProvider) requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.calls...)
}

func (p *durableProvider) stopCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stops)
}

var _ provider.Durable = (*durableProvider)(nil)

// TestRecoverableProviderKeepsAttemptAndReattaches proves the recovery contract:
// a crash keeps the run and its attempt, the claim is reset, and the provider is
// re-attached to the same attempt with the committed cursor instead of being
// sent the prompt again.
func TestRecoverableProviderKeepsAttemptAndReattaches(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var attaches atomic.Int32
	p := &durableProvider{recoverable: true}
	p.runEvents = func(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
		attaches.Add(1)
		for seq := req.AfterSequence + 1; seq <= 3; seq++ {
			if err := emit(provider.Event{Sequence: seq, Delta: fmt.Sprintf("%d", seq)}); err != nil {
				return err
			}
			if seq == 2 {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return nil
	}

	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitRunStatus(t, first, sent.RunID, api.RunRunning)
	waitFor(t, "two committed provider events", func() bool {
		run, ok := findRun(snapshotOf(t, first), sent.RunID)
		return ok && run.Output == "12"
	})
	attemptBefore := latestAttempt(t, first, sent.RunID).AttemptID

	crashForTest(first)

	second := openCoreAt(t, path, p)
	defer second.Close()

	run := waitRunStatus(t, second, sent.RunID, api.RunCompleted)
	if run.Output != "123" {
		t.Fatalf("output after re-attachment = %q, want 123", run.Output)
	}
	if run.AttemptID != attemptBefore {
		t.Fatalf("attempt changed across re-attachment: %q -> %q", attemptBefore, run.AttemptID)
	}
	requests := p.requests()
	if len(requests) != 2 {
		t.Fatalf("provider attached %d times, want 2", len(requests))
	}
	if requests[1].AttemptID != attemptBefore {
		t.Fatalf("re-attachment used attempt %q, want %q", requests[1].AttemptID, attemptBefore)
	}
	if requests[1].AfterSequence != 2 {
		t.Fatalf("re-attachment cursor = %d, want 2", requests[1].AfterSequence)
	}
	if got := attemptStatus(t, second, sent.RunID); got != statusCompleted {
		t.Fatalf("attempt status = %q, want completed", got)
	}
	for _, ev := range loadAllEvents(t, second) {
		if ev.Type == eventRunInterrupted {
			t.Fatalf("recoverable run was recorded as interrupted at %d", ev.Sequence)
		}
	}
	if p.stopCount() != 0 {
		t.Fatalf("recovery called Stop %d times", p.stopCount())
	}
}

// TestNonRecoverableDurableProviderIsInterrupted proves that a durable provider
// without recovery support is terminalized exactly like the controlled provider.
func TestNonRecoverableDurableProviderIsInterrupted(t *testing.T) {
	p := &durableProvider{recoverable: false, runEvents: func(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
		if err := emit(provider.Event{Sequence: 1, Delta: "partial"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitFor(t, "partial output", func() bool {
		run, ok := findRun(snapshotOf(t, first), sent.RunID)
		return ok && run.Output == "partial"
	})
	crashForTest(first)

	second := openCoreAt(t, path, p)
	defer second.Close()
	run := waitRunStatus(t, second, sent.RunID, api.RunInterrupted)
	if run.Output != "partial" {
		t.Fatalf("interrupted output = %q, want the committed prefix", run.Output)
	}
	if got := len(p.requests()); got != 1 {
		t.Fatalf("provider was invoked %d times, want 1", got)
	}
}

// TestDetachedObserverLeavesRunActive proves ErrDetached is not a failure: the
// run stays active and a later Core re-attaches to the same attempt.
func TestDetachedObserverLeavesRunActive(t *testing.T) {
	p := &durableProvider{recoverable: true}
	p.runEvents = func(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
		if req.AfterSequence == 0 {
			if err := emit(provider.Event{Sequence: 1, Delta: "a"}); err != nil {
				return err
			}
			return provider.ErrDetached
		}
		return emit(provider.Event{Sequence: req.AfterSequence + 1, Delta: "b"})
	}
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitFor(t, "detached attempt output", func() bool {
		run, ok := findRun(snapshotOf(t, first), sent.RunID)
		return ok && run.Output == "a"
	})
	time.Sleep(50 * time.Millisecond)
	if run := latestAttempt(t, first, sent.RunID); run.Status != api.RunRunning {
		t.Fatalf("detached run status = %q, want running", run.Status)
	}
	if got := attemptStatus(t, first, sent.RunID); got != statusRunning {
		t.Fatalf("detached attempt status = %q, want running", got)
	}
	if p.stopCount() != 0 {
		t.Fatalf("ErrDetached was reported as a stop intent")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openCoreAt(t, path, p)
	defer second.Close()
	run := waitRunStatus(t, second, sent.RunID, api.RunCompleted)
	if run.Output != "ab" {
		t.Fatalf("output after re-attachment = %q, want ab", run.Output)
	}
	if got := len(p.requests()); got != 2 {
		t.Fatalf("provider attached %d times, want 2", got)
	}
}

// TestProviderEventCursorRejectsGapAndIgnoresDuplicates proves the durable event
// transaction: a repeated sequence is not appended twice and a gap fails the
// attempt with an explicit error.
func TestProviderEventCursorRejectsGapAndIgnoresDuplicates(t *testing.T) {
	p := &durableProvider{runEvents: func(_ context.Context, req provider.Request, emit func(provider.Event) error) error {
		if err := emit(provider.Event{Sequence: 1, Delta: "a"}); err != nil {
			return err
		}
		if err := emit(provider.Event{Sequence: 1, Delta: "duplicate"}); err != nil {
			return err
		}
		return emit(provider.Event{Sequence: 3, Delta: "gap"})
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	run := waitRunStatus(t, c, sent.RunID, api.RunFailed)
	if run.Output != "a" {
		t.Fatalf("output = %q, want the single committed event", run.Output)
	}
	if !strings.Contains(run.Error, "gap") {
		t.Fatalf("failure does not explain the gap: %q", run.Error)
	}
	var cursor int64
	if err := c.store.db.QueryRow(`SELECT provider_cursor FROM attempts WHERE run_id = ?`, sent.RunID).
		Scan(&cursor); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != 1 {
		t.Fatalf("provider cursor = %d, want 1", cursor)
	}
	for _, ev := range loadAllEvents(t, c) {
		if ev.Type != eventRunOutput {
			continue
		}
		var data outputEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode output event: %v", err)
		}
		if strings.Contains(data.Delta, "duplicate") || strings.Contains(data.Delta, "gap") {
			t.Fatalf("refused event was committed: %+v", data)
		}
	}
}

// TestDuplicateEventsAfterReattach proves a re-attached provider that replays an
// already committed sequence appends nothing, so output is never duplicated.
func TestDuplicateEventsAfterReattach(t *testing.T) {
	p := &durableProvider{recoverable: true}
	p.runEvents = func(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
		switch req.AfterSequence {
		case 0:
			if err := emit(provider.Event{Sequence: 1, Delta: "a"}); err != nil {
				return err
			}
			if err := emit(provider.Event{Sequence: 2, Delta: "b"}); err != nil {
				return err
			}
			return provider.ErrDetached
		default:
			// The worker replays the last event before continuing.
			if err := emit(provider.Event{Sequence: req.AfterSequence, Delta: "b"}); err != nil {
				return err
			}
			return emit(provider.Event{Sequence: req.AfterSequence + 1, Delta: "c"})
		}
	}
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitFor(t, "detached output", func() bool {
		run, ok := findRun(snapshotOf(t, first), sent.RunID)
		return ok && run.Output == "ab"
	})
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second := openCoreAt(t, path, p)
	defer second.Close()
	run := waitRunStatus(t, second, sent.RunID, api.RunCompleted)
	if run.Output != "abc" {
		t.Fatalf("output after replay = %q, want abc", run.Output)
	}
}

// TestStartupCompensationDeliversUndeliveredResult proves the recovery barrier
// repairs the "terminal child run, result not delivered" window and that running
// it twice (two opens) does not duplicate the delivery or the continuation.
func TestStartupCompensationDeliversUndeliveredResult(t *testing.T) {
	p := &routeProvider{fn: func(_ context.Context, _ provider.Request, emit func(string) error) error {
		return emit("continued")
	}}
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")

	// Seed exactly the crash window: the child run is terminal and its result is
	// stored, but the task was never delivered and no continuation exists.
	if err := first.write(context.Background(), func(tx *txn) error {
		now := nowNanos()
		statements := []struct {
			query string
			args  []any
		}{
			{`INSERT INTO runs (id, thread_id, attempt_id, active_attempt_id, status, input, output, error,
				created_at, updated_at, provider_id, model, workspace, parent_task_id)
				VALUES ('run-parent', ?, 'att-parent', '', 'completed', 'parent input', 'parent-first', '', ?, ?,
				'controlled', '', '', '')`, []any{threadID, now, now}},
			{`INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at, input, provider_cursor)
				VALUES ('att-parent', 'run-parent', 1, 'completed', ?, ?, 'parent input', 0)`, []any{now, now}},
			{`INSERT INTO effect_outbox (id, kind, thread_id, run_id, attempt_id, status, attempt_count,
				claim_token, lease_until, last_error, created_at, updated_at, input)
				VALUES ('eff-parent', 'provider.run', ?, 'run-parent', 'att-parent', 'settled', 1, '', 0, '', ?, ?, 'parent input')`,
				[]any{threadID, now, now}},
			{`INSERT INTO threads (id, title, created_at, provider_id, model, workspace, parent_thread_id, parent_task_id)
				VALUES ('th-child', 'child', ?, 'controlled', '', '', ?, 'task-1')`, []any{now, threadID}},
			{`INSERT INTO runs (id, thread_id, attempt_id, active_attempt_id, status, input, output, error,
				created_at, updated_at, provider_id, model, workspace, parent_task_id)
				VALUES ('run-child', 'th-child', 'att-child', '', 'completed', 'child work', 'child-result', '', ?, ?,
				'controlled', '', '', 'task-1')`, []any{now, now}},
			{`INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at, input, provider_cursor)
				VALUES ('att-child', 'run-child', 1, 'completed', ?, ?, 'child work', 1)`, []any{now, now}},
			{`INSERT INTO effect_outbox (id, kind, thread_id, run_id, attempt_id, status, attempt_count,
				claim_token, lease_until, last_error, created_at, updated_at, input)
				VALUES ('eff-child', 'provider.run', 'th-child', 'run-child', 'att-child', 'settled', 1, '', 0, '', ?, ?, 'child work')`,
				[]any{now, now}},
			{`INSERT INTO cohorts (parent_run_id, generation, resume_policy, delivery_message_id, status,
				continuation_attempt_id, created_at, updated_at)
				VALUES ('run-parent', 1, 'auto', 'msg-1', 'pending', '', ?, ?)`, []any{now, now}},
			{`INSERT INTO tasks (id, parent_thread_id, parent_run_id, parent_attempt_id, child_thread_id,
				child_run_id, title, generation, status, delivery_status, delivery_message_id, result, error,
				created_at, updated_at)
				VALUES ('task-1', ?, 'run-parent', 'att-parent', 'th-child', 'run-child', 'child task', 1,
				'queued', 'pending', 'msg-1', '', '', ?, ?)`, []any{threadID, now, now}},
		}
		for _, statement := range statements {
			if _, err := tx.exec(statement.query, statement.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed crash window: %v", err)
	}
	crashForTest(first)

	second := openCoreAt(t, path, p)
	task := waitTaskDelivery(t, second, "task-1", api.DeliveryAcknowledged)
	if task.Result != "child-result" {
		t.Fatalf("recovered task result = %q", task.Result)
	}
	waitFor(t, "recovered continuation", func() bool {
		return attemptCount(t, second, "run-parent") == 2
	})
	run := waitRunStatus(t, second, "run-parent", api.RunCompleted)
	if run.Output != "parent-first\n\ncontinued" {
		t.Fatalf("recovered continuation lost its paragraph boundary: %q", run.Output)
	}
	var boundary outputEventData
	if err := json.Unmarshal(firstEventData(t, second, eventRunOutput)["delta"], &boundary.Delta); err != nil {
		t.Fatalf("decode continuation boundary: %v", err)
	}
	if boundary.Delta != "\n\n" {
		t.Fatalf("continuation boundary event delta = %q", boundary.Delta)
	}
	requests := p.requests()
	if len(requests) != 1 || !strings.Contains(requests[0].Message, "child-result") {
		t.Fatalf("continuation message does not carry the recovered result: %+v", requests)
	}
	cohort := cohortOf(t, second, "run-parent", 1)
	if cohort.Status != api.DeliveryAcknowledged || cohort.ContinuationAttemptID == "" {
		t.Fatalf("recovered cohort = %+v", cohort)
	}

	eventsBefore := countRows(t, second, "events")
	attemptsBefore := countRows(t, second, "attempts")
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	third := openCoreAt(t, path, p)
	defer third.Close()
	time.Sleep(100 * time.Millisecond)
	if got := countRows(t, third, "events"); got != eventsBefore {
		t.Fatalf("second recovery wrote %d extra events", got-eventsBefore)
	}
	if got := countRows(t, third, "attempts"); got != attemptsBefore {
		t.Fatalf("second recovery created %d extra attempts", got-attemptsBefore)
	}
	if got := countRows(t, third, "cohorts"); got != 1 {
		t.Fatalf("second recovery created a duplicate cohort (%d)", got)
	}
	if run := latestAttempt(t, third, "run-parent"); run.Output != "parent-first\n\ncontinued" {
		t.Fatalf("second recovery changed continuation output: %q", run.Output)
	}
}

// TestSchemaOneUpgradeKeepsHistoryAndRefusesUnverifiableCommandId proves the
// schema 1 → 2 migration: history and legacy configuration survive, while a
// command receipt that cannot be verified against the actual command is refused.
func TestSchemaOneUpgradeKeepsHistoryAndRefusesUnverifiableCommandId(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(), migrationV1); err != nil {
		raw.Close()
		t.Fatalf("create schema 1: %v", err)
	}
	statements := []string{
		`INSERT INTO threads (id, title, created_at) VALUES ('th-legacy', 'legacy', 100)`,
		`INSERT INTO runs (id, thread_id, attempt_id, active_attempt_id, status, input, output, error, created_at, updated_at)
			VALUES ('run-legacy', 'th-legacy', 'att-legacy', '', 'completed', 'old input', 'old output', '', 100, 100)`,
		`INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at)
			VALUES ('att-legacy', 'run-legacy', 1, 'completed', 100, 100)`,
		`INSERT INTO command_receipts (command_id, kind, fingerprint, thread_id, run_id, sequence, accepted, created_at)
			VALUES ('create-legacy', 'thread.create', 'deadbeef', 'th-legacy', '', 1, 1, 100)`,
		`PRAGMA user_version = 1`,
	}
	for _, statement := range statements {
		if _, err := raw.ExecContext(context.Background(), statement); err != nil {
			raw.Close()
			t.Fatalf("seed schema 1 (%s): %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	c := openCoreAt(t, path, emitProvider("ok"))
	defer c.Close()

	// The legacy receipt has no payload, so the same id must not be re-executed.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "create-legacy", Kind: api.CreateThread, Title: "legacy",
	})
	requireCode(t, err, api.Conflict)

	snap := snapshotOf(t, c)
	if len(snap.Threads) != 1 || snap.Threads[0].ID != "th-legacy" {
		t.Fatalf("legacy thread history lost: %+v", snap.Threads)
	}
	if snap.Threads[0].ProviderID != "controlled" {
		t.Fatalf("legacy thread provider = %q, want controlled", snap.Threads[0].ProviderID)
	}
	if len(snap.Runs) != 1 || snap.Runs[0].Output != "old output" {
		t.Fatalf("legacy run history lost: %+v", snap.Runs)
	}
	if snap.Runs[0].ProviderID != "controlled" {
		t.Fatalf("legacy run provider = %q, want controlled", snap.Runs[0].ProviderID)
	}

	// New commands are accepted and verified by payload after the upgrade.
	threadID := mustCreateThread(t, c, "create-new")
	sent := mustSend(t, c, "send-new", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
	if replay, err := c.Dispatch(context.Background(), sendCommand("send-new", threadID, "hello")); err != nil || replay != sent {
		t.Fatalf("payload-verified replay failed: %+v %v", replay, err)
	}
}

// TestDelegateFromCompletedRunRequiresLatestRun proves the UI path: delegating
// from the most recent completed Run parks it again, while an obsolete Run of a
// thread that already has a newer Run is refused.
func TestDelegateFromCompletedRunRequiresLatestRun(t *testing.T) {
	p := emitProvider("done")
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	first := mustSend(t, c, "send-1", threadID, "first")
	waitRunStatus(t, c, first.RunID, api.RunCompleted)
	firstAttempt := latestAttempt(t, c, first.RunID).AttemptID
	second := mustSend(t, c, "send-2", threadID, "second")
	waitRunStatus(t, c, second.RunID, api.RunCompleted)

	_, err := c.Dispatch(context.Background(), delegateCommand("delegate-stale", first.RunID, firstAttempt, "child", "manual"))
	requireCode(t, err, api.Conflict)

	latestAttemptID := latestAttempt(t, c, second.RunID).AttemptID
	delegated := dispatchOrFail(t, c, delegateCommand("delegate-latest", second.RunID, latestAttemptID, "child", "manual"))
	if delegated.TaskID == "" {
		t.Fatalf("delegate from the latest completed run was not accepted: %+v", delegated)
	}
	waitRunStatus(t, c, second.RunID, api.RunWaiting)
	if run := latestAttempt(t, c, second.RunID); run.Status != api.RunWaiting {
		t.Fatalf("parked parent status = %q, want waiting_children", run.Status)
	}
}

// TestThreadDefaultsResolveIntoRuns proves empty thread fields are resolved from
// core.Options while the command payload stays untouched, so a retry after a
// configuration change replays the original receipt.
func TestThreadDefaultsResolveIntoRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	workspace := &api.WorkspaceSpec{Kind: "directory", Path: "/tmp/vantry-default"}
	first, err := Open(context.Background(), Options{
		DBPath:            path,
		Provider:          emitProvider("ok"),
		DefaultProviderID: "pi",
		DefaultModel:      "default-model",
		DefaultWorkspace:  workspace,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	create := api.Command{
		Version: api.ProtocolVersion, CommandID: "create-1", Kind: api.CreateThread, Title: "thread",
	}
	created, err := first.Dispatch(context.Background(), create)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	thread := snapshotOf(t, first).Threads[0]
	if thread.ProviderID != "pi" || thread.Model != "default-model" ||
		thread.Workspace == nil || thread.Workspace.Path != workspace.Path {
		t.Fatalf("thread did not resolve the defaults: %+v", thread)
	}
	sent := mustSend(t, first, "send-1", created.ThreadID, "hello")
	run := waitRunStatus(t, first, sent.RunID, api.RunCompleted)
	if run.ProviderID != "pi" || run.Model != "default-model" ||
		run.Workspace == nil || run.Workspace.Path != workspace.Path {
		t.Fatalf("run did not inherit the resolved configuration: %+v", run)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The same command after a configuration change must replay its receipt and
	// must not rewrite the thread.
	second, err := Open(context.Background(), Options{
		DBPath:            path,
		Provider:          emitProvider("ok"),
		DefaultProviderID: "controlled",
		DefaultModel:      "other-model",
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	replayed, err := second.Dispatch(context.Background(), create)
	if err != nil {
		t.Fatalf("replay create: %v", err)
	}
	if replayed != created {
		t.Fatalf("replayed receipt = %+v, want %+v", replayed, created)
	}
	if thread := snapshotOf(t, second).Threads[0]; thread.ProviderID != "pi" || thread.Model != "default-model" {
		t.Fatalf("thread configuration drifted after replay: %+v", thread)
	}
}

// TestStopForwardsDurableIntent proves run.stop is an explicit persistent stop
// intent for the attempt: the durable provider is told to stop the same attempt
// while Core stops observing it.
func TestStopForwardsDurableIntent(t *testing.T) {
	started := make(chan struct{}, 1)
	p := &durableProvider{recoverable: true, runEvents: func(ctx context.Context, _ provider.Request, emit func(provider.Event) error) error {
		if err := emit(provider.Event{Sequence: 1, Delta: "a"}); err != nil {
			return err
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("provider was never entered")
	}
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID
	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "stop-1", Kind: api.StopRun,
		ThreadID: threadID, RunID: sent.RunID,
	})
	run := waitRunStatus(t, c, sent.RunID, api.RunCancelled)
	if run.Status != api.RunCancelled || run.Output != "a" {
		t.Fatalf("unexpected cancelled run: %+v", run)
	}
	waitFor(t, "durable stop intent", func() bool { return p.stopCount() == 1 })
	if got := p.stops[0].AttemptID; got != attemptID {
		t.Fatalf("stop intent targeted attempt %q, want %q", got, attemptID)
	}
	if got := effectStatus(t, c, sent.RunID); got != outboxCancelled {
		t.Fatalf("outbox status = %q, want cancelled", got)
	}
}

// TestWaitingParentRecoversAfterChildInterruption proves the restart path of a
// parked parent: the non-durable child is interrupted by the barrier, its result
// is delivered, and the parent run continues on the same run id.
func TestWaitingParentRecoversAfterChildInterruption(t *testing.T) {
	parentGate := make(chan struct{})
	childStarted := make(chan struct{}, 1)
	p := &routeProvider{fn: func(ctx context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "child"):
			select {
			case childStarted <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		default:
			return emit("continued")
		}
	}}
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	parent := mustSend(t, first, "send-1", threadID, "parent message")
	waitRunStatus(t, first, parent.RunID, api.RunRunning)
	parentAttempt := latestAttempt(t, first, parent.RunID).AttemptID
	delegated := dispatchOrFail(t, first, delegateCommand("delegate-1", parent.RunID, parentAttempt, "child work", ""))
	select {
	case <-childStarted:
	case <-time.After(testTimeout):
		t.Fatal("child provider was never entered")
	}
	close(parentGate)
	waitRunStatus(t, first, parent.RunID, api.RunWaiting)
	crashForTest(first)

	second := openCoreAt(t, path, p)
	defer second.Close()

	task := waitTaskDelivery(t, second, delegated.TaskID, api.DeliveryAcknowledged)
	if task.Status != api.RunInterrupted {
		t.Fatalf("recovered task status = %q, want interrupted", task.Status)
	}
	run := waitRunStatus(t, second, parent.RunID, api.RunCompleted)
	if !strings.Contains(run.Output, "parent-first") || !strings.Contains(run.Output, "continued") {
		t.Fatalf("parent did not continue after recovery: %q", run.Output)
	}
	if got := attemptCount(t, second, parent.RunID); got != 2 {
		t.Fatalf("parent got %d attempts, want a single continuation", got)
	}
	requests := p.requests()
	if len(requests) == 0 {
		t.Fatal("no provider requests were recorded")
	}
	if !strings.Contains(requests[len(requests)-1].Message, "interrupted") {
		t.Fatalf("continuation does not report the interrupted child: %q", requests[len(requests)-1].Message)
	}
}
