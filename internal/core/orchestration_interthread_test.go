package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type interRun struct {
	events chan provider.Event
	done   chan struct{}
	once   sync.Once
}

type interProvider struct {
	*durableProvider
	runs     map[string]*interRun
	controls []controlIntent
}

func newInterProvider() *interProvider {
	p := &interProvider{durableProvider: &durableProvider{recoverable: true}, runs: map[string]*interRun{}}
	p.runEvents = func(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
		p.mu.Lock()
		run := p.runs[req.RunID]
		if run == nil {
			run = &interRun{events: make(chan provider.Event, 32), done: make(chan struct{})}
			p.runs[req.RunID] = run
		}
		p.mu.Unlock()
		sequence := req.AfterSequence
		for {
			select {
			case <-ctx.Done():
				return provider.ErrDetached
			case <-run.done:
				return nil
			case event := <-run.events:
				sequence++
				event.Sequence = sequence
				if err := emit(event); err != nil {
					return err
				}
			}
		}
	}
	return p
}

func (p *interProvider) Control(_ context.Context, request provider.Request, control provider.Control) error {
	p.mu.Lock()
	p.controls = append(p.controls, controlIntent{Request: request, Control: control})
	p.mu.Unlock()
	return nil
}

func (p *interProvider) run(t *testing.T, id string) *interRun {
	t.Helper()
	var run *interRun
	waitFor(t, "provider to enter run", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		run = p.runs[id]
		return run != nil
	})
	return run
}

func (p *interProvider) finish(t *testing.T, id string) {
	t.Helper()
	run := p.run(t, id)
	run.once.Do(func() { close(run.done) })
}

func (p *interProvider) event(t *testing.T, id, kind string, payload any) {
	t.Helper()
	event := provider.Event{Type: kind}
	if kind == "text_delta" {
		event.Delta = payload.(string)
	} else {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		event.Data = raw
	}
	p.run(t, id).events <- event
}

func interThread(t *testing.T, c *Core, id, policy, mode string, workspace *api.WorkspaceSpec) string {
	t.Helper()
	return dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.CreateThread, CommandID: "create:" + id,
		Title: id, ProviderID: "pi", Model: "pi-model:high", ApprovalPolicy: policy, InteractionMode: mode, Workspace: workspace}).ThreadID
}

func TestInterSchemaNineUpgradesAndReopensPartialColumns(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			for _, ddl := range []string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5, migrationV6, migrationV7, migrationV8} {
				mustExec(t, db, ddl)
			}
			if partial {
				mustExec(t, db, `ALTER TABLE threads ADD COLUMN created_by_thread_id TEXT NOT NULL DEFAULT ''`)
			}
			mustExec(t, db, `INSERT INTO threads (id, title, created_at) VALUES ('old', 'legacy', 1)`)
			mustExec(t, db, `INSERT INTO runs (id, thread_id, attempt_id, active_attempt_id, status, input, output, created_at, updated_at)
				VALUES ('old-run', 'old', 'old-attempt', '', 'completed', 'question', 'answer', 2, 3)`)
			mustExec(t, db, `INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at) VALUES ('old-attempt', 'old-run', 1, 'completed', 2, 3)`)
			mustExec(t, db, `INSERT INTO inputs (id, thread_id, kind, message, status, created_at, updated_at) VALUES ('old-input', 'old', 'queue', 'history', 'cancelled', 2, 3)`)
			mustExec(t, db, `PRAGMA user_version = 8`)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for open := 0; open < 2; open++ {
				c := openCoreAt(t, path, emitProvider())
				pauseTestWorker(c)
				detail, err := c.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: "old"})
				if err != nil || detail.Thread.SnoozedUntil != nil || detail.Thread.CreatedByRunID != "" || detail.Runs[0].SenderThreadID != "" || detail.Inputs[0].SenderThreadID != "" {
					t.Fatalf("legacy projection: %+v, %v", detail, err)
				}
				var version int
				if err := c.store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
					t.Fatalf("version: %d, %v", version, err)
				}
				for _, column := range [][2]string{{"threads", "snoozed_until"}, {"threads", "created_by_thread_id"}, {"threads", "created_by_run_id"}, {"runs", "sender_thread_id"}, {"inputs", "sender_thread_id"}} {
					if !databaseColumn(t, path, column[0], column[1]) {
						t.Fatalf("missing %v", column)
					}
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestInterSnoozeProvenanceAndRunWake(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	created := dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.CreateThread, CommandID: "create", CreatedByThreadID: "creator", CreatedByRunID: "creator-run"})
	until := time.Now().In(time.FixedZone("test", 8*3600)).Add(time.Hour)
	set := api.Command{Version: 1, Kind: api.UpdateThread, CommandID: "snooze", ThreadID: created.ThreadID, SnoozedUntil: &until}
	dispatchOrFail(t, c, set)
	detail, err := c.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: created.ThreadID})
	if err != nil || detail.Thread.CreatedByThreadID != "creator" || detail.Thread.CreatedByRunID != "creator-run" || detail.Thread.SnoozedUntil == nil || !detail.Thread.SnoozedUntil.Equal(until) || detail.Thread.SnoozedUntil.Location() != time.UTC {
		t.Fatalf("thread metadata: %+v, %v", detail.Thread, err)
	}
	summaries, err := c.ThreadSummaries(context.Background())
	if err != nil || summaries.Threads[0].SnoozedUntil == nil || summaries.Threads[0].CreatedByThreadID != "creator" {
		t.Fatalf("summary: %+v, %v", summaries, err)
	}
	zero := time.Time{}
	set.CommandID, set.SnoozedUntil = "clear", &zero
	dispatchOrFail(t, c, set)
	if lifecycleThread(t, c, created.ThreadID).SnoozedUntil != nil {
		t.Fatal("explicit unsnooze failed")
	}
	past := time.Unix(-2, 0)
	set.CommandID, set.SnoozedUntil = "past", &past
	dispatchOrFail(t, c, set)
	var stored int64
	if err := c.store.db.QueryRow(`SELECT snoozed_until FROM threads WHERE id = ?`, created.ThreadID).Scan(&stored); err != nil || stored != past.UnixNano() {
		t.Fatalf("past snooze: %d, %v", stored, err)
	}
	if lifecycleThread(t, c, created.ThreadID).SnoozedUntil != nil {
		t.Fatal("non-positive snooze should project as nil")
	}
	set.CommandID, set.SnoozedUntil = "resnooze", &until
	dispatchOrFail(t, c, set)
	cmd := sendCommand("send", created.ThreadID, "wake")
	cmd.SenderThreadID = "creator"
	sent := dispatchOrFail(t, c, cmd)
	if lifecycleThread(t, c, created.ThreadID).SnoozedUntil != nil {
		t.Fatal("new run did not wake thread")
	}
	if latestAttempt(t, c, sent.RunID).SenderThreadID != "creator" {
		t.Fatal("regular send lost sender")
	}
}

func TestInterSendModesAndSenderHandoff(t *testing.T) {
	p := newInterProvider()
	c := openTestCore(t, p)
	sender := interThread(t, c, "sender", "auto", "default", nil)
	target := interThread(t, c, "target", "auto", "default", nil)
	until := time.Now().Add(time.Hour)
	dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.UpdateThread, CommandID: "snooze", ThreadID: target, SnoozedUntil: &until})
	req := api.InterThreadSendRequest{ClientRequestID: "first", CallerThreadID: sender, TargetThreadID: target, Message: "first"}
	first, err := c.SendToThread(context.Background(), req)
	if err != nil || first.Delivery != "started" || first.RunID == "" {
		t.Fatalf("idle auto: %+v, %v", first, err)
	}
	p.run(t, first.RunID)
	if lifecycleThread(t, c, target).SnoozedUntil != nil {
		t.Fatal("conversation did not wake target")
	}
	calls := p.requests()
	if calls[0].SenderThreadID != sender || calls[0].SenderTitle != "sender" {
		t.Fatalf("provider sender: %+v", calls[0])
	}
	replayed, err := c.SendToThread(context.Background(), req)
	if err != nil || replayed.RunID != first.RunID || replayed.Delivery != first.Delivery || replayed.InputID != first.InputID {
		t.Fatalf("receipt replay: %+v, %v", replayed, err)
	}
	for _, mode := range []string{"auto", "steer"} {
		steered, err := c.SendToThread(context.Background(), api.InterThreadSendRequest{CallerThreadID: sender, TargetThreadID: target, Message: mode, Mode: mode})
		if err != nil || steered.Delivery != "steered" || steered.RunID != first.RunID {
			t.Fatalf("running %s: %+v, %v", mode, steered, err)
		}
		input := waitInputStatus(t, c, steered.InputID, inputPending)
		if input.SenderThreadID != sender {
			t.Fatal("steer input lost sender")
		}
	}
	waitFor(t, "steer handoffs", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return len(p.controls) == 2 })
	p.mu.Lock()
	controlRequest := p.controls[0].Request
	p.mu.Unlock()
	if controlRequest.SenderThreadID != sender {
		t.Fatal("steer control lost sender")
	}
	queued, err := c.SendToThread(context.Background(), api.InterThreadSendRequest{CallerThreadID: sender, TargetThreadID: target, Message: "queued", Mode: "queue"})
	if err != nil || queued.Delivery != "queued" || queued.RunID != "" {
		t.Fatalf("queue: %+v, %v", queued, err)
	}
	regular := dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.QueueMessage, CommandID: "regular-queue", ThreadID: target, SenderThreadID: sender, Message: "regular queued"})
	p.finish(t, first.RunID)
	promoted := waitInputStatus(t, c, queued.InputID, inputStarted)
	p.run(t, promoted.RunID)
	if latestAttempt(t, c, promoted.RunID).SenderThreadID != sender {
		t.Fatal("queue promotion lost sender")
	}
	for _, call := range p.requests() {
		if call.RunID == promoted.RunID && (call.SenderThreadID != sender || call.SenderTitle != "sender") {
			t.Fatalf("promoted provider request: %+v", call)
		}
	}
	p.finish(t, promoted.RunID)
	next := waitInputStatus(t, c, regular.InputID, inputStarted)
	p.run(t, next.RunID)
	if next.SenderThreadID != sender || latestAttempt(t, c, next.RunID).SenderThreadID != sender {
		t.Fatal("regular queue lost sender")
	}
}

func TestInterSendQueuedWaitingStrictAndFallback(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	thread := mustCreateThread(t, c, "thread")
	strict := api.Command{Version: 1, Kind: api.InterThreadSend, CommandID: "strict", ThreadID: thread, Message: "strict", DeliveryMode: "steer"}
	_, err := c.Dispatch(context.Background(), strict)
	requireCode(t, err, api.Conflict)
	strict.CommandID, strict.FallbackToQueue = "fallback", true
	first := dispatchOrFail(t, c, strict)
	if first.Delivery != "started" {
		t.Fatalf("fallback: %+v", first)
	}
	queued := dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.InterThreadSend, CommandID: "while-queued", ThreadID: thread, Message: "queued"})
	if queued.Delivery != "queued" {
		t.Fatalf("queued target: %+v", queued)
	}
	if err := c.write(context.Background(), func(tx *txn) error {
		_, err := tx.exec(`UPDATE runs SET status = 'waiting_children', active_attempt_id = '' WHERE id = ?`, first.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waiting := dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.InterThreadSend, CommandID: "while-waiting", TargetThreadID: thread, Message: "waiting"})
	if waiting.Delivery != "queued" {
		t.Fatalf("waiting target: %+v", waiting)
	}
	bad := strict
	bad.CommandID, bad.DeliveryMode = "bad-mode", "unsupported"
	_, err = c.Dispatch(context.Background(), bad)
	requireCode(t, err, api.InvalidCommand)
}

func TestInterPermissionCeilings(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	policies := []string{"ask", "auto-accept-edits", "auto"}
	for senderRank, senderPolicy := range policies {
		for targetRank, targetPolicy := range policies {
			for _, mode := range []string{"plan", "default"} {
				name := fmt.Sprintf("%d-%d-%s", senderRank, targetRank, mode)
				sender := interThread(t, c, "sender-"+name, senderPolicy, "plan", nil)
				target := interThread(t, c, "target-"+name, targetPolicy, mode, nil)
				_, err := c.SendToThread(context.Background(), api.InterThreadSendRequest{CallerThreadID: sender, TargetThreadID: target, Message: "hello"})
				if targetRank > senderRank || mode == "default" {
					requireCode(t, err, api.Conflict)
				} else if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	caller := interThread(t, c, "caller", "ask", "plan", nil)
	_, err := c.LaunchThread(context.Background(), api.ThreadLaunchRequest{CallerThreadID: caller, ThreadLaunchSpec: api.ThreadLaunchSpec{ApprovalPolicy: "auto"}})
	requireCode(t, err, api.Conflict)
	parent := mustSend(t, c, "parent", caller, "parent")
	_, err = c.DelegateTask(context.Background(), api.DelegateTaskRequest{CallerThreadID: caller, ParentRunID: parent.RunID, Task: "delegate", InteractionMode: "default"})
	requireCode(t, err, api.Conflict)
	bound := delegateCommand("bound", parent.RunID, "", "delegate", resumeAuto)
	bound.ThreadID, bound.ApprovalPolicy = caller, "auto"
	_, err = c.Dispatch(context.Background(), bound)
	requireCode(t, err, api.Conflict)
}

func TestInterCancelTaskCascadesAndReleasesDisposedCohort(t *testing.T) {
	p := newInterProvider()
	c := openTestCore(t, p)
	parentThread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", parentThread, "parent")
	p.run(t, parent.RunID)
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	p.run(t, child.RunID)
	grandchild := dispatchOrFail(t, c, delegateCommand("grandchild", child.RunID, "", "grandchild", resumeManual))
	p.run(t, grandchild.RunID)
	queued := dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.QueueMessage, CommandID: "grandchild-queue", ThreadID: grandchild.ThreadID, Message: "must not start"})
	p.finish(t, child.RunID)
	waitRunStatus(t, c, child.RunID, api.RunWaiting)
	p.finish(t, parent.RunID)
	waitRunStatus(t, c, parent.RunID, api.RunWaiting)
	wrong := api.Command{Version: 1, Kind: api.CancelTask, CommandID: "wrong-parent", ThreadID: grandchild.ThreadID, TaskID: child.TaskID}
	_, err := c.Dispatch(context.Background(), wrong)
	requireCode(t, err, api.Conflict)
	cancel := api.Command{Version: 1, Kind: api.CancelTask, CommandID: "cancel", ThreadID: parentThread, TaskID: child.TaskID}
	receipt := dispatchOrFail(t, c, cancel)
	assertCommandReplay(t, c, cancel, receipt)
	for _, taskID := range []string{child.TaskID, grandchild.TaskID} {
		task := findTask(t, c, taskID)
		if task.Status != api.RunCancelled || task.DeliveryStatus != api.DeliveryDisposed {
			t.Fatalf("cancelled task: %+v", task)
		}
	}
	waitInputStatus(t, c, queued.InputID, inputCancelled)
	waitRunStatus(t, c, grandchild.RunID, api.RunCancelled)
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	if cohort := cohortOf(t, c, parent.RunID, child.Generation); cohort.Status != api.DeliveryDisposed {
		t.Fatalf("cohort: %+v", cohort)
	}
	waitFor(t, "both stop intents", func() bool { return p.stopCount() == 2 })
	if attemptCount(t, c, parent.RunID) != 1 {
		t.Fatal("all-disposed cohort invoked a continuation")
	}
}

func TestInterCancelTaskCoversLaterChildSessionRuns(t *testing.T) {
	p := newInterProvider()
	c := openTestCore(t, p)
	parentThread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", parentThread, "parent")
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	p.finish(t, child.RunID)
	waitTaskDelivery(t, c, child.TaskID, api.DeliveryDelivered)
	later := mustSend(t, c, "later", child.ThreadID, "later conversation")
	p.run(t, later.RunID)
	nested := dispatchOrFail(t, c, delegateCommand("nested", later.RunID, "", "nested", resumeManual))
	p.run(t, nested.RunID)
	_, err := c.CancelTask(context.Background(), api.CancelTaskRequest{TaskID: child.TaskID, CallerThreadID: parentThread})
	if err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, c, later.RunID, api.RunCancelled)
	waitRunStatus(t, c, nested.RunID, api.RunCancelled)
	if got := findTask(t, c, child.TaskID); got.Status != api.RunCompleted || got.DeliveryStatus != api.DeliveryDisposed {
		t.Fatalf("historical terminal task: %+v", got)
	}
}

func TestInterForceDeleteCancelsWorkersAndReconcilesParent(t *testing.T) {
	p := newInterProvider()
	c := openTestCore(t, p)
	parentThread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", parentThread, "parent")
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	p.run(t, child.RunID)
	grandchild := dispatchOrFail(t, c, delegateCommand("grandchild", child.RunID, "", "grandchild", resumeManual))
	p.run(t, grandchild.RunID)
	p.finish(t, parent.RunID)
	waitRunStatus(t, c, parent.RunID, api.RunWaiting)
	delete := api.Command{Version: 1, Kind: api.DeleteThread, CommandID: "strict-delete", ThreadID: child.ThreadID}
	_, err := c.Dispatch(context.Background(), delete)
	requireCode(t, err, api.Conflict)
	delete.CommandID, delete.Force = "force-delete", true
	receipt := dispatchOrFail(t, c, delete)
	assertCommandReplay(t, c, delete, receipt)
	_, err = c.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: child.ThreadID})
	requireCode(t, err, api.NotFound)
	waitRunStatus(t, c, grandchild.RunID, api.RunCancelled)
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	waitFor(t, "deleted and descendant workers stop", func() bool { return p.stopCount() == 2 })
	for _, table := range []string{"runs", "inputs", "tool_calls", "provider_requests", "queued_messages", "steering_messages", "effect_outbox"} {
		var count int
		if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM `+table+` WHERE thread_id = ?`, child.ThreadID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: %d, %v", table, count, err)
		}
	}
}

func TestInterForceDeleteRetainsUndeliveredStopAcrossRestart(t *testing.T) {
	p := &unavailableStopProvider{durableProvider: &durableProvider{recoverable: true}}
	p.runEvents = func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) error {
		<-ctx.Done()
		return provider.ErrDetached
	}
	path := filepath.Join(t.TempDir(), "core.db")
	c := openCoreAt(t, path, p)
	thread := mustCreateThread(t, c, "thread")
	run := mustSend(t, c, "send", thread, "running")
	waitRunStatus(t, c, run.RunID, api.RunRunning)
	waitFor(t, "provider entered", func() bool { return len(p.requests()) == 1 })
	dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.DeleteThread, CommandID: "delete", ThreadID: thread, Force: true})
	var pending int
	if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM effect_outbox WHERE kind = 'provider.stop' AND status = 'pending'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending stop: %d, %v", pending, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	p.available.Store(true)
	c = openCoreAt(t, path, p)
	defer c.Close()
	waitFor(t, "stop after restart", func() bool { return p.stopCount() == 1 })
	if len(p.requests()) != 1 {
		t.Fatal("deleted run was replayed")
	}
}

func TestInterWakeUpgradeDeliveredManualCohort(t *testing.T) {
	release := make(chan struct{})
	p := &routeProvider{fn: func(ctx context.Context, req provider.Request, emit func(string) error) error {
		if req.Message == "parent" {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return emit("answer")
	}}
	c := openTestCore(t, p)
	parentThread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", parentThread, "parent")
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	waitTaskDelivery(t, c, child.TaskID, api.DeliveryDelivered)
	close(release)
	waitRunStatus(t, c, parent.RunID, api.RunWaiting)
	upgrade := api.Command{Version: 1, Kind: api.UpgradeTaskWake, CommandID: "upgrade", ParentRunID: parent.RunID}
	receipt := dispatchOrFail(t, c, upgrade)
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	assertCommandReplay(t, c, upgrade, receipt)
	if got := cohortOf(t, c, parent.RunID, child.Generation); got.ResumePolicy != resumeAuto || got.Status != api.DeliveryAcknowledged {
		t.Fatalf("upgraded cohort: %+v", got)
	}
	if attemptCount(t, c, parent.RunID) != 2 {
		t.Fatal("upgrade did not create exactly one continuation")
	}
}

func interGit(t *testing.T, commit bool) string {
	t.Helper()
	root := t.TempDir()
	commands := [][]string{{"init", "-q", root}}
	if commit {
		commands = append(commands, []string{"-C", root, "-c", "user.name=Core Test", "-c", "user.email=core@example.invalid", "commit", "-q", "--allow-empty", "-m", "initial"})
	}
	for _, args := range commands {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v, %s", args, err, output)
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestInterDelegateIsolationRoleModelAndIdempotency(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	repository := interGit(t, true)
	nested := filepath.Join(repository, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	caller := interThread(t, c, "caller", "auto", "default", &api.WorkspaceSpec{Kind: "directory", Path: nested})
	parent := mustSend(t, c, "parent", caller, "parent")
	req := api.DelegateTaskRequest{ClientRequestID: "implementation", CallerThreadID: caller, ParentRunID: parent.RunID,
		Task: "implement", Role: "implementation", Model: "other", Thinking: "max", Isolation: "worktree", ApprovalPolicy: "ask", InteractionMode: "plan"}
	detail, err := c.DelegateTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	child := lifecycleThread(t, c, detail.ChildThreadID)
	if child.Workspace == nil || child.Workspace.Kind != "worktree" || child.Workspace.Repository != repository || child.Workspace.BaseRef != "HEAD" || child.Model != "other:max" || child.ApprovalPolicy != "ask" || child.InteractionMode != "plan" {
		t.Fatalf("child configuration: %+v", child)
	}
	if run := latestAttempt(t, c, detail.ChildRunID); run.Input != "[Role: implementation]\n\nimplement" {
		t.Fatalf("role input: %q", run.Input)
	}
	dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.SetThreadModel, CommandID: "new-model", ThreadID: caller, Model: "later"})
	replay, err := c.DelegateTask(context.Background(), req)
	if err != nil || replay.TaskID != detail.TaskID {
		t.Fatalf("delegate retry: %+v, %v", replay, err)
	}
	req.Task = "different"
	_, err = c.DelegateTask(context.Background(), req)
	requireCode(t, err, api.Conflict)
	shared, err := c.DelegateTask(context.Background(), api.DelegateTaskRequest{CallerThreadID: caller, ParentRunID: parent.RunID, Task: "shared", Isolation: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if spec := lifecycleThread(t, c, shared.ChildThreadID).Workspace; spec.Kind != "directory" || spec.Path != nested {
		t.Fatalf("shared workspace: %+v", spec)
	}
	for i, path := range []string{t.TempDir(), interGit(t, false)} {
		thread := interThread(t, c, fmt.Sprintf("invalid-%d", i), "auto", "default", &api.WorkspaceSpec{Kind: "directory", Path: path})
		run := mustSend(t, c, fmt.Sprintf("invalid-parent-%d", i), thread, "parent")
		_, err := c.DelegateTask(context.Background(), api.DelegateTaskRequest{CallerThreadID: thread, ParentRunID: run.RunID, Task: "work", Isolation: "worktree"})
		requireCode(t, err, api.InvalidCommand)
	}
}

func TestInterDelegateWaitCompletionTimeoutAndCancellation(t *testing.T) {
	for _, outcome := range []string{"completed", "timeout", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			p := newInterProvider()
			c := openTestCore(t, p)
			caller := mustCreateThread(t, c, "caller")
			parent := mustSend(t, c, "parent", caller, "parent")
			p.run(t, parent.RunID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 3000
			if outcome == "timeout" {
				timeout = 40
			}
			result := make(chan api.TaskDetail, 1)
			errorsOut := make(chan error, 1)
			go func() {
				detail, err := c.DelegateTask(ctx, api.DelegateTaskRequest{CallerThreadID: caller, ParentRunID: parent.RunID, Task: "child", Mode: "wait", TimeoutMs: timeout})
				result <- detail
				errorsOut <- err
			}()
			var task api.Task
			waitFor(t, "wait assignment", func() bool {
				tasks := snapshotOf(t, c).Tasks
				if len(tasks) == 1 {
					task = tasks[0]
					return true
				}
				return false
			})
			p.run(t, task.ChildRunID)
			if outcome == "completed" {
				p.event(t, task.ChildRunID, "text_delta", "child result")
				waitFor(t, "child text", func() bool { return latestAttempt(t, c, task.ChildRunID).Output == "child result" })
				p.finish(t, task.ChildRunID)
			} else if outcome == "cancelled" {
				cancel()
			}
			var detail api.TaskDetail
			select {
			case detail = <-result:
			case <-time.After(testTimeout):
				t.Fatal("wait did not return")
			}
			err := <-errorsOut
			if outcome == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if outcome == "completed" {
				if !detail.Status.Terminal() || detail.Summary != "child result" || detail.DeliveryStatus != api.DeliveryAcknowledged {
					t.Fatalf("wait result: %+v", detail)
				}
			} else {
				if detail.WaitTimedOut != (outcome == "timeout") {
					t.Fatalf("wait timeout: %+v", detail)
				}
				if cohort := cohortOf(t, c, parent.RunID, task.Generation); cohort.ResumePolicy != resumeAuto {
					t.Fatalf("abandoned wait remained manual: %+v", cohort)
				}
				p.finish(t, parent.RunID)
				waitRunStatus(t, c, parent.RunID, api.RunWaiting)
				p.finish(t, task.ChildRunID)
				waitRunStatus(t, c, parent.RunID, api.RunCompleted)
				if attemptCount(t, c, parent.RunID) != 2 {
					t.Fatal("child did not wake parent after wait ended")
				}
			}
			waitFor(t, "wait subscription cleanup", func() bool { return c.hub.count() == 0 })
		})
	}
}

func seedInterRun(t *testing.T, c *Core, thread, commandID, input, output, reasoning string) api.Receipt {
	t.Helper()
	cmd := sendCommand(commandID, thread, input)
	cmd.SenderThreadID = "source-session"
	receipt := dispatchOrFail(t, c, cmd)
	if err := c.write(context.Background(), func(tx *txn) error {
		if _, err := tx.exec(`UPDATE runs SET status = 'completed', active_attempt_id = '', output = ?, reasoning = ?, updated_at = ? WHERE id = ?`, output, reasoning, nowNanos(), receipt.RunID); err != nil {
			return err
		}
		if _, err := tx.exec(`UPDATE attempts SET status = 'completed' WHERE run_id = ?`, receipt.RunID); err != nil {
			return err
		}
		_, err := tx.exec(`UPDATE effect_outbox SET status = 'settled' WHERE run_id = ?`, receipt.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestInterReadTimelinePaginationUnicodeAndByteBudget(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	thread := mustCreateThread(t, c, "thread")
	for i := 0; i < 4; i++ {
		seedInterRun(t, c, thread, fmt.Sprint(i), fmt.Sprint("input-", i), fmt.Sprint("output-", i), "reasoning")
	}
	latest, err := c.ReadThread(context.Background(), api.ThreadReadQuery{ThreadID: thread, Limit: 3})
	if err != nil || len(latest.Items) != 3 || latest.Items[0].Position != 5 || latest.Items[2].Position != 7 || latest.EarlierPosition == nil {
		t.Fatalf("latest page: %+v, %v", latest, err)
	}
	previous, err := c.ReadThread(context.Background(), api.ThreadReadQuery{ThreadID: thread, BeforePosition: latest.EarlierPosition, Limit: 3})
	if err != nil || len(previous.Items) != 3 || previous.Items[0].Position != 2 || previous.Items[2].Position != 4 {
		t.Fatalf("previous page: %+v, %v", previous, err)
	}
	position := 1
	after, err := c.ReadThread(context.Background(), api.ThreadReadQuery{ThreadID: thread, AfterPosition: &position, Limit: 2})
	if err != nil || len(after.Items) != 2 || after.Items[0].Position != 2 || after.Items[1].SenderThreadID != "" || after.Items[0].SenderThreadID != "source-session" {
		t.Fatalf("after page: %+v, %v", after, err)
	}
	unicode := "甲🙂乙🚀丙丁"
	last := seedInterRun(t, c, thread, "unicode", "question", unicode, "reasoning")
	read, err := c.ReadThread(context.Background(), api.ThreadReadQuery{ThreadID: thread, ItemID: last.RunID + ":assistant", TextOffset: 1, MaxCharsPerItem: 3})
	if err != nil || len(read.Items) != 1 || read.Items[0].Text != "🙂乙🚀" || !read.Items[0].TextTruncated || read.Items[0].NextTextOffset == nil || *read.Items[0].NextTextOffset != 4 {
		t.Fatalf("rune slice: %+v, %v", read, err)
	}
	continued, err := c.ReadThread(context.Background(), api.ThreadReadQuery{ThreadID: thread, ItemID: read.Items[0].ItemID, TextOffset: *read.Items[0].NextTextOffset, MaxCharsPerItem: 3})
	if err != nil || continued.Items[0].Text != "丙丁" || continued.Items[0].TextTruncated {
		t.Fatalf("continued text: %+v, %v", continued, err)
	}
	seedInterRun(t, c, thread, "large", "input", strings.Repeat("🙂<&>汉", 20000), "reasoning")
	budget, err := c.ReadThread(context.Background(), api.ThreadReadQuery{ThreadID: thread, Limit: 100, MaxCharsPerItem: 30000, RunLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(budget)
	if err != nil || len(raw) > threadReadByteBudget || len(budget.Items) == 0 || !budget.Items[len(budget.Items)-1].TextTruncated {
		t.Fatalf("budget: bytes=%d, items=%d, error=%v", len(raw), len(budget.Items), err)
	}
	for _, item := range budget.Items {
		if !utf8.ValidString(item.Text) {
			t.Fatal("invalid text boundary")
		}
	}
}

func TestInterReadActivityAndObserveChildResult(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	thread := mustCreateThread(t, c, "thread")
	run := seedInterRun(t, c, thread, "parent", "input", "output", "reasoning")
	child := dispatchOrFail(t, c, delegateCommand("child", run.RunID, "", "child", resumeManual))
	if err := c.write(context.Background(), func(tx *txn) error {
		if err := c.saveTool(tx, toolRecord{ID: "tool", ThreadID: thread, RunID: run.RunID, AttemptID: "attempt", Name: "read", Arguments: json.RawMessage(`{"path":"file"}`), Output: "tool result", Status: toolCompleted}, false); err != nil {
			return err
		}
		if err := c.insertRequest(tx, requestRecord{ID: "request", ThreadID: thread, RunID: run.RunID, AttemptID: "attempt", ProviderRequestID: "confirm", Method: "confirm", Title: "approve", Message: "please confirm", Status: requestPending}); err != nil {
			return err
		}
		if _, err := tx.exec(`UPDATE runs SET error = 'visible error' WHERE id = ?`, run.RunID); err != nil {
			return err
		}
		if _, err := tx.exec(`UPDATE runs SET status = 'completed', active_attempt_id = '', output = 'child output' WHERE id = ?`, child.RunID); err != nil {
			return err
		}
		if _, err := tx.exec(`UPDATE effect_outbox SET status = 'settled' WHERE run_id = ?`, child.RunID); err != nil {
			return err
		}
		return c.reconcileTaskForRun(tx, child.RunID)
	}); err != nil {
		t.Fatal(err)
	}
	activity, err := c.ReadThread(context.Background(), api.ThreadReadQuery{ThreadID: thread, View: "activity", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range activity.Items {
		seen[item.Type] = true
	}
	for _, kind := range []string{"user_message", "assistant_message", "reasoning", "tool_call", "approval_request", "subagent", "error"} {
		if !seen[kind] {
			t.Fatalf("missing %s in %+v", kind, activity.Items)
		}
	}
	if _, err := c.ReadThread(context.Background(), api.ThreadReadQuery{CallerThreadID: thread, ThreadID: child.ThreadID}); err != nil {
		t.Fatal(err)
	}
	if task := findTask(t, c, child.TaskID); task.DeliveryStatus != api.DeliveryAcknowledged {
		t.Fatalf("observed task: %+v", task)
	}
}

func TestInterLaunchBatchScratchAndCatalog(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	workspace := &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()}
	caller := interThread(t, c, "caller", "ask", "plan", workspace)
	parent := mustSend(t, c, "parent", caller, "parent")
	req := api.ThreadLaunchRequest{CallerThreadID: caller, CallerRunID: parent.RunID, ClientRequestID: "launch",
		ThreadLaunchSpec: api.ThreadLaunchSpec{Title: "top level", Message: "hello", Thinking: "low"}}
	launched, err := c.LaunchThread(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	thread := lifecycleThread(t, c, launched.ThreadID)
	if thread.ParentThreadID != "" || thread.CreatedByThreadID != caller || thread.CreatedByRunID != parent.RunID || thread.Model != "pi-model:low" || thread.Workspace.Path != workspace.Path || thread.InteractionMode != "plan" || thread.ApprovalPolicy != "ask" {
		t.Fatalf("launched thread: %+v", thread)
	}
	if latestAttempt(t, c, launched.RunID).SenderThreadID != caller {
		t.Fatal("initial launch sender missing")
	}
	replay, err := c.LaunchThread(context.Background(), req)
	if err != nil || replay.ThreadID != launched.ThreadID || replay.RunID != launched.RunID {
		t.Fatalf("launch retry: %+v, %v", replay, err)
	}
	scratchReq := api.ThreadLaunchRequest{CallerThreadID: caller, ClientRequestID: "scratch", ThreadLaunchSpec: api.ThreadLaunchSpec{Scratch: true}}
	scratch, err := c.LaunchThread(context.Background(), scratchReq)
	if err != nil {
		t.Fatal(err)
	}
	scratchThread := lifecycleThread(t, c, scratch.ThreadID)
	if scratchThread.Workspace == nil || scratchThread.Workspace.Kind != "directory" || scratchThread.Workspace.Path == workspace.Path {
		t.Fatalf("scratch workspace: %+v", scratchThread.Workspace)
	}
	if info, err := os.Stat(scratchThread.Workspace.Path); err != nil || !info.IsDir() {
		t.Fatalf("scratch directory: %v", err)
	}
	scratchReplay, err := c.LaunchThread(context.Background(), scratchReq)
	if err != nil || scratchReplay.ThreadID != scratch.ThreadID {
		t.Fatalf("scratch replay: %+v, %v", scratchReplay, err)
	}
	batch, err := c.CreateThreads(context.Background(), api.CreateThreadsRequest{CallerThreadID: caller, ClientRequestID: "batch", Threads: []api.ThreadLaunchSpec{{Title: "one"}, {Title: "two", Message: "work"}}})
	if err != nil || len(batch.Threads) != 2 {
		t.Fatalf("batch: %+v, %v", batch, err)
	}
	_, err = c.CreateThreads(context.Background(), api.CreateThreadsRequest{Threads: make([]api.ThreadLaunchSpec, 21)})
	requireCode(t, err, api.InvalidCommand)
	fallback, err := c.OrchestratorCapabilities(context.Background(), api.OrchestratorCapabilitiesQuery{CallerThreadID: caller})
	if err != nil || fallback.InheritedModel != "pi-model" || fallback.InheritedThinking != "high" || len(fallback.Models) != 1 || len(fallback.ApprovalPolicies) != 1 || len(fallback.InteractionModes) != 1 {
		t.Fatalf("fallback catalog: %+v, %v", fallback, err)
	}
	c.SetModelCatalogProvider(func(_ context.Context, path, model string) (api.ModelCatalog, error) {
		// Real Pi catalogs reject "model:thinking" keys; only the base model is passed.
		if path != workspace.Path || model != "pi-model" {
			return api.ModelCatalog{}, errors.New("wrong catalog context")
		}
		return api.ModelCatalog{Models: []api.ModelInfo{{Provider: "pi", ID: "live", Name: "Live Model"}, {ID: "other"}}, ThinkingLevels: []string{"off", "high", "max"}}, nil
	})
	live, err := c.OrchestratorCapabilities(context.Background(), api.OrchestratorCapabilitiesQuery{CallerThreadID: caller, Search: "live"})
	if err != nil || len(live.Models) != 1 || live.Models[0].ID != "pi/live" || len(live.Models[0].Options[0].Values) != 3 {
		t.Fatalf("live catalog: %+v, %v", live, err)
	}
	status, err := c.ServerStatus(context.Background())
	if err != nil || status.SchemaVersion != schemaVersion || !status.HasCapability(api.CapabilityInterThreadOrchestration) {
		t.Fatalf("server status: %+v, %v", status, err)
	}
}

func TestInterListThreadsFiltersAndPagination(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	workspace := &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()}
	caller := interThread(t, c, "caller", "auto", "default", workspace)
	for i := 0; i < 28; i++ {
		interThread(t, c, fmt.Sprintf("conversation-%02d", i), "auto", "default", workspace)
	}
	other := interThread(t, c, "elsewhere", "auto", "default", &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()})
	parent := mustSend(t, c, "parent", caller, "parent")
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	until := time.Now().Add(time.Hour)
	dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.UpdateThread, CommandID: "snooze", ThreadID: other, SnoozedUntil: &until})
	first, err := c.ListThreads(context.Background(), api.ThreadListQuery{CallerThreadID: caller})
	if err != nil || len(first.Threads) != 25 || first.Total != 29 || first.NextCursor == nil {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	second, err := c.ListThreads(context.Background(), api.ThreadListQuery{CallerThreadID: caller, Cursor: *first.NextCursor})
	if err != nil || len(second.Threads) != 4 || second.NextCursor != nil {
		t.Fatalf("second page: %+v, %v", second, err)
	}
	all, err := c.ListThreads(context.Background(), api.ThreadListQuery{AllProjects: true, IncludeSubagents: true, Limit: 1000})
	if err != nil || all.Total != 31 || len(all.Threads) != 31 || all.Threads[0].ThreadID != other {
		t.Fatalf("all projects: %+v, %v", all, err)
	}
	flag := true
	snoozed, err := c.ListThreads(context.Background(), api.ThreadListQuery{AllProjects: true, Snoozed: &flag})
	if err != nil || len(snoozed.Threads) != 1 || snoozed.Threads[0].ThreadID != other || !snoozed.Threads[0].Snoozed {
		t.Fatalf("snoozed filter: %+v, %v", snoozed, err)
	}
	filtered, err := c.ListThreads(context.Background(), api.ThreadListQuery{AllProjects: true, TitleContains: "CONVERSATION-0", Settled: &flag})
	if err != nil || filtered.Total != 10 {
		t.Fatalf("title filter: %+v, %v", filtered, err)
	}
	if err := c.write(context.Background(), func(tx *txn) error {
		_, err := tx.exec(`UPDATE runs SET status = 'waiting_children', active_attempt_id = '' WHERE id = ?`, parent.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	live := false
	waiting, err := c.ListThreads(context.Background(), api.ThreadListQuery{CallerThreadID: caller, Settled: &live, Statuses: []string{"waiting_children"}})
	if err != nil || waiting.Total != 1 || waiting.Threads[0].ThreadID != caller || waiting.Threads[0].Settled {
		t.Fatalf("waiting filter: %+v, %v", waiting, err)
	}
	for _, item := range first.Threads {
		if item.ThreadID == child.ThreadID {
			t.Fatal("default list included subagent")
		}
	}
}

func TestInterTaskStatusTracksChildrenAndBlockingDialogs(t *testing.T) {
	p := newInterProvider()
	c := openTestCore(t, p)
	parentThread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", parentThread, "parent")
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	p.run(t, child.RunID)
	status, err := c.TaskStatus(context.Background(), api.TaskStatusQuery{TaskID: child.TaskID})
	if err != nil || len(status.Tasks) != 1 || status.Tasks[0].Status != api.RunRunning || status.Tasks[0].WorkState != "working" || status.Tasks[0].Next != api.OrchestratorRunningTaskNext {
		t.Fatalf("running task: %+v, %v", status, err)
	}
	grandchild := dispatchOrFail(t, c, delegateCommand("grandchild", child.RunID, "", "grandchild", resumeManual))
	p.run(t, grandchild.RunID)
	p.event(t, child.RunID, "extension_ui_request", map[string]any{"id": "confirm", "method": "confirm", "title": "Permission"})
	waitFor(t, "task pending approval", func() bool {
		status, err = c.TaskStatus(context.Background(), api.TaskStatusQuery{ParentRunID: parent.RunID})
		return err == nil && status.Tasks[0].PendingApproval != nil
	})
	detail := status.Tasks[0]
	if !detail.HasPendingChildRuns || detail.WorkState != "waiting_for_children" || detail.PendingApproval.Title != "Permission" || detail.Next != "" {
		t.Fatalf("blocked task: %+v", detail)
	}
	p.finish(t, child.RunID)
	waitRunStatus(t, c, child.RunID, api.RunWaiting)
	p.finish(t, grandchild.RunID)
	waitTaskDelivery(t, c, grandchild.TaskID, api.DeliveryDelivered)
	_, err = c.TaskStatus(context.Background(), api.TaskStatusQuery{CallerThreadID: child.ThreadID, TaskID: grandchild.TaskID, Acknowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDelivery(t, c, child.TaskID, api.DeliveryDelivered)
	dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.UpdateThread, CommandID: "rename-child", ThreadID: child.ThreadID, Title: "child title"})
	status, err = c.TaskStatus(context.Background(), api.TaskStatusQuery{CallerThreadID: parentThread, TaskID: child.TaskID, Acknowledge: true})
	if err != nil || status.Tasks[0].WorkState != "result_available" || status.Tasks[0].HasPendingChildRuns || status.Tasks[0].DeliveryStatus != api.DeliveryAcknowledged || status.Tasks[0].Link != api.FormatThreadLink(child.ThreadID, "child title") {
		t.Fatalf("terminal task: %+v, %v", status, err)
	}
}

func TestInterOldReadCannotAcknowledgeFollowupResult(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	thread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", thread, "parent")
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	complete := func(runID string) {
		t.Helper()
		if err := c.write(context.Background(), func(tx *txn) error {
			if _, err := tx.exec(`UPDATE runs SET status = 'completed', active_attempt_id = '', output = 'answer' WHERE id = ?`, runID); err != nil {
				return err
			}
			if _, err := tx.exec(`UPDATE effect_outbox SET status = 'settled' WHERE run_id = ?`, runID); err != nil {
				return err
			}
			return c.reconcileTaskForRun(tx, runID)
		}); err != nil {
			t.Fatal(err)
		}
	}
	complete(child.RunID)
	old, err := c.TaskStatus(context.Background(), api.TaskStatusQuery{TaskID: child.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.DisposeTask, CommandID: "dispose", TaskID: child.TaskID, Generation: child.Generation, DeliveryMessageID: child.DeliveryMessageID})
	followup := dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.FollowUpTask, CommandID: "followup", TaskID: child.TaskID, Message: "follow up", ResumePolicy: resumeManual})
	complete(followup.RunID)
	if err := c.observeTaskResults(context.Background(), old.Tasks); err != nil {
		t.Fatal(err)
	}
	if task := findTask(t, c, child.TaskID); task.DeliveryStatus != api.DeliveryDelivered {
		t.Fatalf("unseen follow-up was acknowledged: %+v", task)
	}
	_, err = c.TaskStatus(context.Background(), api.TaskStatusQuery{CallerThreadID: thread, TaskID: child.TaskID, Acknowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	if task := findTask(t, c, child.TaskID); task.DeliveryStatus != api.DeliveryAcknowledged {
		t.Fatalf("current read did not acknowledge: %+v", task)
	}
}

func TestInterWakeUpgradeAppliesToReusedTaskID(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	thread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", thread, "parent")
	child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
	first, err := c.awaitDelegatedTask(context.Background(), child.TaskID, 2)
	if err != nil || !first.WaitTimedOut {
		t.Fatalf("initial wait: %+v, %v", first, err)
	}
	dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.CancelTask, CommandID: "cancel", TaskID: child.TaskID})
	followup := dispatchOrFail(t, c, api.Command{Version: 1, Kind: api.FollowUpTask, CommandID: "followup", TaskID: child.TaskID, Message: "follow up", ResumePolicy: resumeManual})
	second, err := c.awaitDelegatedTask(context.Background(), child.TaskID, 2)
	if err != nil || !second.WaitTimedOut {
		t.Fatalf("followup wait: %+v, %v", second, err)
	}
	if cohort := cohortOf(t, c, parent.RunID, followup.Generation); cohort.ResumePolicy != resumeAuto {
		t.Fatalf("followup retained manual wake: %+v", cohort)
	}
}

func TestInterGovernanceRollbackDoesNotStopProvider(t *testing.T) {
	for _, kind := range []api.CommandKind{api.CancelTask, api.DeleteThread} {
		t.Run(string(kind), func(t *testing.T) {
			p := newInterProvider()
			c := openTestCore(t, p)
			thread := mustCreateThread(t, c, "parent")
			parent := mustSend(t, c, "parent-send", thread, "parent")
			child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
			p.run(t, child.RunID)
			before := snapshotOf(t, c)
			target := thread
			if kind == api.DeleteThread {
				target = child.ThreadID
			}
			c.setTestHookPreCommit(func(*sql.Tx) error { return errors.New("rollback") })
			_, err := c.Dispatch(context.Background(), api.Command{Version: 1, Kind: kind, CommandID: "rollback", TaskID: child.TaskID, ThreadID: target, Force: true})
			c.setTestHookPreCommit(nil)
			if err == nil {
				t.Fatal("injected failure committed")
			}
			after := snapshotOf(t, c)
			beforeJSON, _ := json.Marshal(before)
			afterJSON, _ := json.Marshal(after)
			if string(beforeJSON) != string(afterJSON) || p.stopCount() != 0 {
				t.Fatal("rollback changed state or stopped a worker")
			}
		})
	}
}

func TestInterTinyWaitReturnsPopulatedTimeout(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	thread := mustCreateThread(t, c, "thread")
	run := mustSend(t, c, "run", thread, "queued")
	out, err := c.WaitThread(context.Background(), api.ThreadWaitQuery{ThreadID: thread, TimeoutMs: 1})
	if err != nil || !out.TimedOut || out.RunID != run.RunID || out.Link == "" || out.Status != "queued" {
		t.Fatalf("tiny timeout: %+v, %v", out, err)
	}
}

func TestInterWaitDelegateReturnsEarlyOnQuestion(t *testing.T) {
	p := newInterProvider()
	c := openTestCore(t, p)
	thread := mustCreateThread(t, c, "parent")
	parent := mustSend(t, c, "parent-send", thread, "parent")
	result := make(chan api.TaskDetail, 1)
	errorsOut := make(chan error, 1)
	go func() {
		detail, err := c.DelegateTask(context.Background(), api.DelegateTaskRequest{CallerThreadID: thread, ParentRunID: parent.RunID, Task: "child", Mode: "wait", TimeoutMs: 3000})
		result <- detail
		errorsOut <- err
	}()
	var task api.Task
	waitFor(t, "child assignment", func() bool {
		tasks := snapshotOf(t, c).Tasks
		if len(tasks) != 1 {
			return false
		}
		task = tasks[0]
		return true
	})
	p.event(t, task.ChildRunID, "extension_ui_request", map[string]any{"id": "question", "method": "input", "title": "Need input"})
	select {
	case detail := <-result:
		if !detail.HasPendingQuestion || detail.WaitTimedOut || detail.Next != "" {
			t.Fatalf("question result: %+v", detail)
		}
	case <-time.After(testTimeout):
		t.Fatal("question did not end wait")
	}
	if err := <-errorsOut; err != nil {
		t.Fatal(err)
	}
}

func TestInterWaitThreadDelegatedSettlementAndPendingRequests(t *testing.T) {
	t.Run("delegated settlement", func(t *testing.T) {
		releaseParent, releaseChild := make(chan struct{}), make(chan struct{})
		p := &routeProvider{fn: func(ctx context.Context, req provider.Request, _ func(string) error) error {
			release := releaseChild
			if req.Message == "parent" {
				release = releaseParent
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
			if req.Message == "parent" {
				return errors.New("parent failure")
			}
			return nil
		}}
		c := openTestCore(t, p)
		thread := mustCreateThread(t, c, "thread")
		parent := mustSend(t, c, "parent", thread, "parent")
		child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
		close(releaseParent)
		waitRunStatus(t, c, parent.RunID, api.RunFailed)
		plain, err := c.WaitThread(context.Background(), api.ThreadWaitQuery{ThreadID: thread, RunID: parent.RunID, TimeoutMs: 1000})
		if err != nil || plain.TimedOut || plain.Status != "failed" {
			t.Fatalf("terminal wait: %+v, %v", plain, err)
		}
		result := make(chan api.ThreadWaitResult, 1)
		errorsOut := make(chan error, 1)
		go func() {
			out, err := c.WaitThread(context.Background(), api.ThreadWaitQuery{ThreadID: thread, RunID: parent.RunID, UntilDelegatedWorkSettled: true, TimeoutMs: 3000})
			result <- out
			errorsOut <- err
		}()
		waitFor(t, "settlement subscription", func() bool { return c.hub.count() == 1 })
		select {
		case out := <-result:
			t.Fatalf("returned before child settled: %+v", out)
		default:
		}
		close(releaseChild)
		waitRunTerminal(t, c, child.RunID)
		select {
		case out := <-result:
			if out.TimedOut || out.Status != "failed" {
				t.Fatalf("settled result: %+v", out)
			}
		case <-time.After(testTimeout):
			t.Fatal("settlement wait stuck")
		}
		if err := <-errorsOut; err != nil {
			t.Fatal(err)
		}
	})
	for _, method := range []string{"confirm", "input", "select", "editor"} {
		t.Run(method, func(t *testing.T) {
			p := newInterProvider()
			c := openTestCore(t, p)
			thread := mustCreateThread(t, c, "thread")
			run := mustSend(t, c, "run", thread, "work")
			result := make(chan api.ThreadWaitResult, 1)
			errorsOut := make(chan error, 1)
			go func() {
				out, err := c.WaitThread(context.Background(), api.ThreadWaitQuery{ThreadID: thread, TimeoutMs: 2000})
				result <- out
				errorsOut <- err
			}()
			waitFor(t, "request subscription", func() bool { return c.hub.count() == 1 })
			p.event(t, run.RunID, "extension_ui_request", map[string]any{"id": "dialog", "method": method, "title": "Need answer"})
			select {
			case out := <-result:
				if out.TimedOut || method == "confirm" && out.PendingApproval == nil || method != "confirm" && !out.HasPendingQuestion {
					t.Fatalf("early wake: %+v", out)
				}
			case <-time.After(testTimeout):
				t.Fatal("request did not wake wait")
			}
			if err := <-errorsOut; err != nil {
				t.Fatal(err)
			}
			waitFor(t, "request wait cleanup", func() bool { return c.hub.count() == 0 })
		})
	}
	t.Run("timeout", func(t *testing.T) {
		p := newInterProvider()
		c := openTestCore(t, p)
		thread := mustCreateThread(t, c, "thread")
		mustSend(t, c, "run", thread, "work")
		out, err := c.WaitThread(context.Background(), api.ThreadWaitQuery{ThreadID: thread, TimeoutMs: 20})
		if err != nil || !out.TimedOut || out.Next != api.OrchestratorRunningTaskNext {
			t.Fatalf("timeout: %+v, %v", out, err)
		}
	})
}

func TestInterAutoWakeOnWaitTimeoutAndNotifyOnComplete(t *testing.T) {
	requestMessage := func(p *interProvider, runID string, idx int) string {
		var out []string
		for _, r := range p.requests() {
			if r.RunID == runID {
				out = append(out, r.Message)
			}
		}
		if idx < len(out) {
			return out[idx]
		}
		return ""
	}

	t.Run("wait timeout upgrades manual delegated cohort and wakes parent", func(t *testing.T) {
		p := newInterProvider()
		c := openTestCore(t, p)
		parentThread := mustCreateThread(t, c, "parent")
		parent := mustSend(t, c, "parent-run", parentThread, "parent")
		child := dispatchOrFail(t, c, delegateCommand("child", parent.RunID, "", "child", resumeManual))
		p.run(t, parent.RunID)
		p.run(t, child.RunID)
		p.finish(t, parent.RunID)
		waitRunStatus(t, c, parent.RunID, api.RunWaiting)

		out, err := c.WaitThread(context.Background(), api.ThreadWaitQuery{
			CallerThreadID: parentThread,
			ThreadID:       child.ThreadID,
			RunID:          child.RunID,
			TimeoutMs:      20,
		})
		if err != nil || !out.TimedOut || !out.AutoWakeRegistered {
			t.Fatalf("expected timeout with AutoWakeRegistered=true, got %+v, err=%v", out, err)
		}

		p.event(t, child.RunID, "text_delta", "child-finished")
		p.finish(t, child.RunID)
		waitFor(t, "parent continuation attempt", func() bool {
			return attemptCount(t, c, parent.RunID) == 2
		})
		p.finish(t, parent.RunID)
		waitRunStatus(t, c, parent.RunID, api.RunCompleted)
		if msg := requestMessage(p, parent.RunID, 1); !strings.Contains(msg, "child-finished") {
			t.Fatalf("unexpected continuation input: %q", msg)
		}
	})

	t.Run("wait timeout registers peer thread_watch and wakes caller", func(t *testing.T) {
		p := newInterProvider()
		c := openTestCore(t, p)
		caller := mustCreateThread(t, c, "caller")
		launched, err := c.LaunchThread(context.Background(), api.ThreadLaunchRequest{
			CallerThreadID: caller,
			Title:          "peer",
			Message:        "peer work",
		})
		if err != nil {
			t.Fatal(err)
		}
		p.run(t, launched.RunID)

		out, err := c.WaitThread(context.Background(), api.ThreadWaitQuery{
			CallerThreadID: caller,
			ThreadID:       launched.ThreadID,
			RunID:          launched.RunID,
			TimeoutMs:      20,
		})
		if err != nil || !out.TimedOut || !out.AutoWakeRegistered {
			t.Fatalf("expected peer AutoWakeRegistered=true, got %+v, err=%v", out, err)
		}

		p.event(t, launched.RunID, "text_delta", "peer-done")
		p.finish(t, launched.RunID)
		waitRunStatus(t, c, launched.RunID, api.RunCompleted)
		waitFor(t, "caller auto-wake run", func() bool {
			return len(lifecycleRuns(t, c, caller)) == 1
		})
		runs := lifecycleRuns(t, c, caller)
		p.finish(t, runs[0].ID)
		waitRunStatus(t, c, runs[0].ID, api.RunCompleted)
		msg := requestMessage(p, runs[0].ID, 0)
		if !strings.Contains(msg, "peer-done") || !strings.Contains(msg, launched.ThreadID) {
			t.Fatalf("peer wake message missing summary: %q", msg)
		}
	})

	t.Run("launch with notifyOnComplete wakes caller on failure without wait", func(t *testing.T) {
		p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
			if req.Message == "fail soon" {
				return errors.New("boom")
			}
			return emit("ack")
		}}
		c := openTestCore(t, p)
		caller := mustCreateThread(t, c, "caller")
		launched, err := c.LaunchThread(context.Background(), api.ThreadLaunchRequest{
			CallerThreadID:   caller,
			Title:            "failing-peer",
			Message:          "fail soon",
			NotifyOnComplete: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		waitRunStatus(t, c, launched.RunID, api.RunFailed)

		waitFor(t, "caller auto-wake on failure", func() bool {
			return len(lifecycleRuns(t, c, caller)) == 1
		})
		runs := lifecycleRuns(t, c, caller)
		waitRunStatus(t, c, runs[0].ID, api.RunCompleted)
		reqs := p.requestsForRun(runs[0].ID)
		if len(reqs) == 0 || !strings.Contains(reqs[0].Message, "boom") || !strings.Contains(reqs[0].Message, "failed") {
			t.Fatalf("peer failure wake missing error/status: %+v", reqs)
		}
	})
}

func TestContinuationMessageTruncatesLargeOutput(t *testing.T) {
	large := strings.Repeat("A", maxContinuationResultRunes+2500)
	msg := continuationMessage([]taskRecord{{
		ID:            "task_1",
		ChildThreadID: "th_child",
		Title:         "big child",
		Generation:    1,
		Status:        statusCompleted,
		Result:        large,
	}})
	if !strings.Contains(msg, "已省略 2500 字符") || !strings.Contains(msg, "vantry-thread://th_child") {
		t.Fatalf("expected truncated continuation with thread link, got len=%d:\n%s", len(msg), msg[:200])
	}
}
