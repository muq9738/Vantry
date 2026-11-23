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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func lifecycleCommand(id string, kind api.CommandKind, threadID string) api.Command {
	return api.Command{Version: api.ProtocolVersion, CommandID: id, Kind: kind, ThreadID: threadID}
}

// Pausing before seeding isolates transaction/migration tests from scheduling.
// Execution behaviour is separately exercised with the live fake providers.
func pauseTestWorker(c *Core) {
	c.workCancel()
	c.workWG.Wait()
}

func seedLifecycleRun(t *testing.T, c *Core, threadID string, status api.RunStatus, input, output, reasoning string, at time.Time) api.Run {
	t.Helper()
	var result api.Run
	err := c.write(context.Background(), func(tx *txn) error {
		thread, err := loadThread(tx, threadID)
		if err != nil {
			return err
		}
		run, attemptID, _, err := createRun(tx, runSeed{
			ThreadID: threadID, Input: input, ProviderID: thread.ProviderID, Model: thread.Model,
			Workspace: thread.Workspace, ApprovalPolicy: thread.ApprovalPolicy, InteractionMode: thread.InteractionMode,
		})
		if err != nil {
			return err
		}
		active := attemptID
		if status.Terminal() || status == api.RunWaiting {
			active = ""
		}
		if _, err := tx.exec(`UPDATE runs SET status = ?, active_attempt_id = ?, output = ?, reasoning = ?, created_at = ?, updated_at = ? WHERE id = ?`,
			string(status), active, output, reasoning, at.UnixNano(), at.UnixNano(), run.ID); err != nil {
			return err
		}
		if _, err := tx.exec(`UPDATE attempts SET status = ? WHERE id = ?`, string(status), attemptID); err != nil {
			return err
		}
		if _, err := tx.exec(`UPDATE effect_outbox SET status = ? WHERE run_id = ?`, outboxSettled, run.ID); err != nil {
			return err
		}
		run.Status, run.Output, run.Reasoning, run.CreatedAt, run.UpdatedAt = status, output, reasoning, at, at
		result = run
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func lifecycleThread(t *testing.T, c *Core, id string) api.Thread {
	t.Helper()
	for _, thread := range snapshotOf(t, c).Threads {
		if thread.ID == id {
			return thread
		}
	}
	t.Fatalf("thread %q missing", id)
	return api.Thread{}
}

func lifecycleRuns(t *testing.T, c *Core, threadID string) []api.Run {
	t.Helper()
	var result []api.Run
	for _, run := range snapshotOf(t, c).Runs {
		if run.ThreadID == threadID {
			result = append(result, run)
		}
	}
	return result
}

func assertCommandReplay(t *testing.T, c *Core, cmd api.Command, want api.Receipt) {
	t.Helper()
	before := snapshotOf(t, c)
	got := dispatchOrFail(t, c, cmd)
	if got != want {
		t.Fatalf("replay = %+v, want %+v", got, want)
	}
	if after := snapshotOf(t, c); !reflect.DeepEqual(before, after) {
		t.Fatal("replay changed the committed snapshot")
	}
}

func TestDeleteThreadRemovesDependantsAndRetainsReceiptsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	c := openCoreAt(t, path, emitProvider())
	pauseTestWorker(c)
	defer c.Close()
	target := mustCreateThread(t, c, "target")
	parent := mustCreateThread(t, c, "parent")
	child := mustCreateThread(t, c, "child")
	at := time.Now().Add(-time.Hour)
	run := seedLifecycleRun(t, c, target, api.RunCompleted, "input", "output", "reasoning", at)
	parentRun := seedLifecycleRun(t, c, parent, api.RunCompleted, "parent", "parent result", "", at)
	childRun := seedLifecycleRun(t, c, child, api.RunCompleted, "child", "child result", "", at)
	err := c.write(context.Background(), func(tx *txn) error {
		if err := c.insertInput(tx, inputRecord{ID: "input", ThreadID: target, RunID: run.ID, AttemptID: run.AttemptID,
			Kind: inputKindQueue, Message: "cancelled", Status: inputCancelled}); err != nil {
			return err
		}
		if err := c.saveTool(tx, toolRecord{ID: "tool", ThreadID: target, RunID: run.ID, AttemptID: run.AttemptID, Status: toolCompleted, Arguments: json.RawMessage(`{}`)}, false); err != nil {
			return err
		}
		if err := c.insertRequest(tx, requestRecord{ID: "request", ProviderRequestID: "dialog", ThreadID: target,
			RunID: run.ID, AttemptID: run.AttemptID, Status: requestResponded, Method: "confirm"}); err != nil {
			return err
		}
		if _, err := tx.exec(`INSERT INTO queued_messages (id, thread_id, message, status, created_at) VALUES ('legacy-queue', ?, 'old', 'cancelled', 1)`, target); err != nil {
			return err
		}
		if _, err := tx.exec(`INSERT INTO steering_messages (id, command_id, thread_id, run_id, attempt_id, message, status, created_at, updated_at)
			VALUES ('legacy-steer', 'legacy-command', ?, ?, ?, 'old', 'accepted', 1, 1)`, target, run.ID, run.AttemptID); err != nil {
			return err
		}
		for i, pair := range [][4]string{{parent, parentRun.ID, target, run.ID}, {target, run.ID, child, childRun.ID}} {
			if err := c.insertTask(tx, taskRecord{ID: fmt.Sprintf("task-%d", i), ParentThreadID: pair[0], ParentRunID: pair[1],
				ChildThreadID: pair[2], ChildRunID: pair[3], Generation: 1, Status: statusCompleted, DeliveryStatus: deliveryDisposed}); err != nil {
				return err
			}
			if err := c.insertCohort(tx, cohortRecord{ParentRunID: pair[1], Generation: 1, ResumePolicy: resumeManual,
				DeliveryMessageID: fmt.Sprintf("delivery-%d", i), Status: deliveryDisposed}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := lifecycleCommand("delete", api.DeleteThread, target)
	before := snapshotOf(t, c).Sequence
	receipt := dispatchOrFail(t, c, cmd)
	if !receipt.Accepted || receipt.ThreadID != target || receipt.Sequence != before+1 {
		t.Fatalf("delete receipt: %+v", receipt)
	}
	for _, table := range []string{"threads", "runs", "attempts", "inputs", "queued_messages", "steering_messages", "tool_calls", "provider_requests", "effect_outbox"} {
		key := "thread_id"
		value := target
		if table == "threads" {
			key = "id"
		}
		if table == "attempts" {
			key, value = "run_id", run.ID
		}
		var n int
		if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM `+table+` WHERE `+key+` = ?`, value).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s retained %d rows for deleted thread: %v", table, n, err)
		}
	}
	if got := countRows(t, c, "tasks"); got != 0 {
		t.Fatalf("retained %d incoming/outgoing tasks", got)
	}
	if got := countRows(t, c, "cohorts"); got != 1 {
		t.Fatalf("cohorts = %d, want the surviving parent's cohort", got)
	}
	if countRows(t, c, "threads") != 2 || countRows(t, c, "runs") != 2 || countRows(t, c, "command_receipts") != 4 {
		t.Fatal("deletion changed unrelated threads or removed command receipts")
	}
	var event api.Event
	var raw []byte
	if err := c.store.db.QueryRow(`SELECT type, thread_id, data FROM events WHERE sequence = ?`, receipt.Sequence).Scan(&event.Type, &event.ThreadID, &raw); err != nil {
		t.Fatal(err)
	}
	var data threadEventData
	if err := json.Unmarshal(raw, &data); err != nil || event.Type != eventThreadDeleted || data.Thread.ID != target {
		t.Fatalf("deleted event: %+v %s %v", event, raw, err)
	}
	assertCommandReplay(t, c, cmd, receipt)
	changed := cmd
	changed.Title = "different payload"
	_, err = c.Dispatch(context.Background(), changed)
	requireCode(t, err, api.Conflict)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openCoreAt(t, path, emitProvider())
	defer reopened.Close()
	assertCommandReplay(t, reopened, cmd, receipt)
	if countRows(t, reopened, "inputs") != 0 {
		t.Fatal("legacy projection recreated deleted inputs on reopen")
	}
}

func TestDeleteThreadRejectsActiveRuns(t *testing.T) {
	for _, status := range []api.RunStatus{api.RunQueued, api.RunRunning, api.RunWaiting} {
		t.Run(string(status), func(t *testing.T) {
			c := openTestCore(t, emitProvider())
			pauseTestWorker(c)
			threadID := mustCreateThread(t, c, "create")
			seedLifecycleRun(t, c, threadID, status, "input", "", "", time.Now())
			before := snapshotOf(t, c)
			_, err := c.Dispatch(context.Background(), lifecycleCommand("delete", api.DeleteThread, threadID))
			requireCode(t, err, api.Conflict)
			if !reflect.DeepEqual(before, snapshotOf(t, c)) || countRows(t, c, "command_receipts") != 1 {
				t.Fatal("rejected deletion mutated state")
			}
		})
	}
}

func TestDeleteThreadRejectsUnsettledTasksOnEitherSide(t *testing.T) {
	for _, side := range []string{"parent", "child"} {
		for _, state := range []struct{ status, delivery, cohort, continuation string }{
			{statusRunning, deliveryPending, deliveryPending, ""},
			{statusCompleted, deliveryPending, deliveryPending, ""},
			{statusFailed, deliveryDelivered, deliveryDelivered, ""},
			{statusCompleted, deliveryAcknowledged, deliveryAcknowledged, ""},
		} {
			t.Run(side+"/"+state.status+"/"+state.delivery, func(t *testing.T) {
				c := openTestCore(t, emitProvider())
				pauseTestWorker(c)
				parent, child := mustCreateThread(t, c, "parent"), mustCreateThread(t, c, "child")
				pr := seedLifecycleRun(t, c, parent, api.RunCompleted, "p", "p", "", time.Now())
				cr := seedLifecycleRun(t, c, child, api.RunCompleted, "c", "c", "", time.Now())
				err := c.write(context.Background(), func(tx *txn) error {
					if err := c.insertCohort(tx, cohortRecord{ParentRunID: pr.ID, Generation: 1, ResumePolicy: resumeManual,
						DeliveryMessageID: "delivery", Status: state.cohort, ContinuationAttemptID: state.continuation}); err != nil {
						return err
					}
					return c.insertTask(tx, taskRecord{ID: "task", ParentThreadID: parent, ParentRunID: pr.ID,
						ChildThreadID: child, ChildRunID: cr.ID, Generation: 1, Status: state.status, DeliveryStatus: state.delivery})
				})
				if err != nil {
					t.Fatal(err)
				}
				target := parent
				if side == "child" {
					target = child
				}
				before := snapshotOf(t, c)
				_, err = c.Dispatch(context.Background(), lifecycleCommand("delete", api.DeleteThread, target))
				requireCode(t, err, api.Conflict)
				if !reflect.DeepEqual(before, snapshotOf(t, c)) {
					t.Fatal("unsettled task was changed by rejected deletion")
				}
			})
		}
	}
}

func TestForkThreadCopiesCompletedContextThroughCutoffWithoutExecution(t *testing.T) {
	p := emitProvider()
	c := openTestCore(t, p)
	pauseTestWorker(c)
	workspace := &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()}
	source := dispatchOrFail(t, c, api.Command{Version: api.ProtocolVersion, CommandID: "source", Kind: api.CreateThread,
		Title: "source title", ProviderID: "pi", Model: "model", Workspace: workspace, ApprovalPolicy: "ask", InteractionMode: "plan"})
	at := time.Now().Add(-time.Hour)
	first := seedLifecycleRun(t, c, source.ThreadID, api.RunCompleted, "first input", "first output", "first reasoning", at)
	failed := seedLifecycleRun(t, c, source.ThreadID, api.RunFailed, "failed input", "partial", "", at.Add(time.Second))
	last := seedLifecycleRun(t, c, source.ThreadID, api.RunCompleted, "last input", "last output", "last reasoning", at.Add(2*time.Second))
	active := seedLifecycleRun(t, c, source.ThreadID, api.RunRunning, "active", "partial", "", at.Add(3*time.Second))
	for _, tc := range []struct {
		name, cutoff, title string
		workspace           *api.WorkspaceSpec
		want                []api.Run
	}{
		{"latest", "", "", nil, []api.Run{first, last}},
		{"explicit", first.ID, "fork title", &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()}, []api.Run{first}},
		{"failed-cutoff", failed.ID, "", nil, []api.Run{first}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := lifecycleCommand("fork-"+tc.name, api.ForkThread, source.ThreadID)
			cmd.SourceRunID, cmd.Title, cmd.Workspace = tc.cutoff, tc.title, tc.workspace
			receipt := dispatchOrFail(t, c, cmd)
			thread := lifecycleThread(t, c, receipt.ThreadID)
			cutoff := firstNonEmpty(tc.cutoff, last.ID)
			if thread.ForkedFromThreadID != source.ThreadID || thread.ForkedFromRunID != cutoff || thread.ParentThreadID != "" || thread.Archived {
				t.Fatalf("fork lineage: %+v", thread)
			}
			if thread.Title != firstNonEmpty(tc.title, "source title") || thread.ProviderID != "pi" || thread.Model != "model" || thread.ApprovalPolicy != "ask" || thread.InteractionMode != "plan" ||
				!reflect.DeepEqual(thread.Workspace, firstWorkspace(tc.workspace, workspace)) {
				t.Fatalf("fork configuration: %+v", thread)
			}
			runs := lifecycleRuns(t, c, thread.ID)
			if len(runs) != len(tc.want) {
				t.Fatalf("fork has %d runs, want %d", len(runs), len(tc.want))
			}
			for i, run := range runs {
				original := tc.want[i]
				if run.ID == original.ID || run.AttemptID == original.AttemptID || run.Status != api.RunCompleted ||
					run.Input != original.Input || run.Output != original.Output || run.Reasoning != original.Reasoning || run.InteractionMode != "plan" {
					t.Fatalf("copied context: %+v, original %+v", run, original)
				}
				if attemptStatus(t, c, run.ID) != statusCompleted {
					t.Fatal("history attempt is executable")
				}
			}
			var effects int
			if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM effect_outbox WHERE thread_id = ?`, thread.ID).Scan(&effects); err != nil || effects != 0 {
				t.Fatalf("fork history scheduled execution: %d %v", effects, err)
			}
			var completedEvents int
			if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM events WHERE thread_id = ? AND type = ?`, thread.ID, eventRunCompleted).Scan(&completedEvents); err != nil || completedEvents != len(runs) {
				t.Fatalf("history not emitted to timeline: %d %v", completedEvents, err)
			}
			assertCommandReplay(t, c, cmd, receipt)
			cmd.SourceRunID = active.ID
			_, err := c.Dispatch(context.Background(), cmd)
			requireCode(t, err, api.Conflict)
		})
	}
	if len(lifecycleRuns(t, c, source.ThreadID)) != 4 || p.callCount() != 0 {
		t.Fatal("fork changed or executed source history")
	}
}

func TestForkThreadPreservesHistoryOrderWhenTimestampsTie(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	source := mustCreateThread(t, c, "source")
	at := time.Now().Add(-time.Hour)
	for _, input := range []string{"first", "second", "third"} {
		seedLifecycleRun(t, c, source, api.RunCompleted, input, "result: "+input, "reasoning: "+input, at)
	}
	history := lifecycleRuns(t, c, source)
	fork := dispatchOrFail(t, c, lifecycleCommand("fork", api.ForkThread, source))
	copies := lifecycleRuns(t, c, fork.ThreadID)
	if len(copies) != len(history) {
		t.Fatalf("copied %d runs, want %d", len(copies), len(history))
	}
	for i, run := range copies {
		if run.Input != history[i].Input || run.Output != history[i].Output || run.Reasoning != history[i].Reasoning || !run.CreatedAt.Equal(history[i].CreatedAt) {
			t.Fatalf("history order changed at %d: %+v vs %+v", i, run, history[i])
		}
	}
}

func TestForkThreadEmptyAndInvalidSources(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	source, other := mustCreateThread(t, c, "empty"), mustCreateThread(t, c, "other")
	for _, withActive := range []bool{false, true} {
		if withActive {
			seedLifecycleRun(t, c, source, api.RunRunning, "active", "", "", time.Now())
		}
		cmd := lifecycleCommand(fmt.Sprintf("fork-empty-%t", withActive), api.ForkThread, source)
		receipt := dispatchOrFail(t, c, cmd)
		thread := lifecycleThread(t, c, receipt.ThreadID)
		if thread.ForkedFromThreadID != source || thread.ForkedFromRunID != "" || len(lifecycleRuns(t, c, thread.ID)) != 0 {
			t.Fatalf("empty fork: %+v", thread)
		}
	}
	otherRun := seedLifecycleRun(t, c, other, api.RunCompleted, "other", "other", "", time.Now())
	for _, tc := range []struct {
		thread, run string
		code        api.ErrorCode
	}{
		{"", "", api.InvalidCommand}, {"missing", "", api.NotFound},
		{source, "missing", api.NotFound}, {source, otherRun.ID, api.NotFound},
	} {
		cmd := lifecycleCommand("bad-fork", api.ForkThread, tc.thread)
		cmd.SourceRunID = tc.run
		_, err := c.Dispatch(context.Background(), cmd)
		requireCode(t, err, tc.code)
	}
	cmd := lifecycleCommand("active-fork", api.ForkThread, source)
	cmd.SourceRunID = lifecycleRuns(t, c, source)[0].ID
	_, err := c.Dispatch(context.Background(), cmd)
	requireCode(t, err, api.Conflict)
}

func TestMergeBackUsesLatestCompletedResultAndTargetQueue(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	parent := mustCreateThread(t, c, "parent")
	at := time.Now().Add(-time.Hour)
	seedLifecycleRun(t, c, parent, api.RunCompleted, "original", "original result", "", at)
	fork := dispatchOrFail(t, c, lifecycleCommand("fork", api.ForkThread, parent))
	latest := seedLifecycleRun(t, c, fork.ThreadID, api.RunCompleted, "new work", "new result", "details", at.Add(time.Second))
	seedLifecycleRun(t, c, fork.ThreadID, api.RunFailed, "failed later", "partial", "", at.Add(2*time.Second))
	seedLifecycleRun(t, c, parent, api.RunRunning, "busy", "", "", at.Add(3*time.Second))
	cmd := lifecycleCommand("merge", api.MergeBackThread, fork.ThreadID)
	receipt := dispatchOrFail(t, c, cmd)
	input := waitInputStatus(t, c, receipt.InputID, inputQueued)
	if receipt.ThreadID != parent || receipt.RunID != "" || !strings.Contains(input.Message, latest.ID) || !strings.Contains(input.Message, "new result") || strings.Contains(input.Message, "partial") {
		t.Fatalf("merge delivery: %+v %+v", receipt, input)
	}
	assertCommandReplay(t, c, cmd, receipt)
	cmd.TargetThreadID = fork.ThreadID
	_, err := c.Dispatch(context.Background(), cmd)
	requireCode(t, err, api.Conflict)

	idle := mustCreateThread(t, c, "idle")
	cmd = lifecycleCommand("merge-idle", api.MergeBackThread, fork.ThreadID)
	cmd.TargetThreadID = idle
	receipt = dispatchOrFail(t, c, cmd)
	if receipt.RunID == "" || receipt.ThreadID != idle {
		t.Fatalf("idle merge did not start execution: %+v", receipt)
	}
	run, ok := findRun(snapshotOf(t, c), receipt.RunID)
	if !ok || run.Status != api.RunQueued || !strings.Contains(run.Input, "new result") {
		t.Fatalf("merge execution: %+v", run)
	}
	assertCommandReplay(t, c, cmd, receipt)
}

func TestMergeBackValidatesLineageAndArchivedTarget(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	parent, child, empty := mustCreateThread(t, c, "parent"), mustCreateThread(t, c, "child"), mustCreateThread(t, c, "empty")
	seedLifecycleRun(t, c, child, api.RunCompleted, "work", "result", "", time.Now().Add(-time.Hour))
	if err := c.write(context.Background(), func(tx *txn) error {
		_, err := tx.exec(`UPDATE threads SET parent_thread_id = ? WHERE id = ?`, parent, child)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cmd := lifecycleCommand("merge-child", api.MergeBackThread, child)
	receipt := dispatchOrFail(t, c, cmd)
	if receipt.ThreadID != parent || receipt.RunID == "" {
		t.Fatalf("child lineage fallback: %+v", receipt)
	}
	archived := mustCreateThread(t, c, "archived")
	flag := true
	archive := lifecycleCommand("archive", api.UpdateThread, archived)
	archive.Archived = &flag
	dispatchOrFail(t, c, archive)
	for _, tc := range []struct {
		source, target string
		code           api.ErrorCode
	}{
		{"", parent, api.InvalidCommand}, {"missing", parent, api.NotFound},
		{child, "missing", api.NotFound}, {child, child, api.InvalidCommand},
		{empty, "", api.InvalidCommand}, {empty, parent, api.Conflict}, {child, archived, api.Conflict},
	} {
		cmd := lifecycleCommand("bad-merge", api.MergeBackThread, tc.source)
		cmd.TargetThreadID = tc.target
		before := snapshotOf(t, c)
		_, err := c.Dispatch(context.Background(), cmd)
		requireCode(t, err, tc.code)
		if !reflect.DeepEqual(before, snapshotOf(t, c)) {
			t.Fatal("rejected merge mutated state")
		}
	}
}

func TestThreadLifecycleCommandsRollBackAllProjectionsAndReceipt(t *testing.T) {
	for _, kind := range []api.CommandKind{api.DeleteThread, api.ForkThread, api.MergeBackThread} {
		t.Run(string(kind), func(t *testing.T) {
			c := openTestCore(t, emitProvider())
			pauseTestWorker(c)
			source, target := mustCreateThread(t, c, "source"), mustCreateThread(t, c, "target")
			seedLifecycleRun(t, c, source, api.RunCompleted, "input", "result", "reasoning", time.Now().Add(-time.Hour))
			cmd := lifecycleCommand("operation", kind, source)
			if kind == api.MergeBackThread {
				cmd.TargetThreadID = target
			}
			before, effects, receipts, attempts := snapshotOf(t, c), countRows(t, c, "effect_outbox"), countRows(t, c, "command_receipts"), countRows(t, c, "attempts")
			c.setTestHookPreCommit(func(*sql.Tx) error { return errors.New("rollback lifecycle command") })
			_, err := c.Dispatch(context.Background(), cmd)
			c.setTestHookPreCommit(nil)
			if err == nil {
				t.Fatal("injected failure succeeded")
			}
			if !reflect.DeepEqual(before, snapshotOf(t, c)) || effects != countRows(t, c, "effect_outbox") || receipts != countRows(t, c, "command_receipts") || attempts != countRows(t, c, "attempts") {
				t.Fatal("failed transaction leaked projection, effect, event or receipt")
			}
			receipt := dispatchOrFail(t, c, cmd)
			assertCommandReplay(t, c, cmd, receipt)
		})
	}
}

func TestInteractionModeCapturedByThreadsRunsQueuesAndDelegates(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	create := lifecycleCommand("create-plan", api.CreateThread, "")
	create.InteractionMode, create.ProviderID, create.Model = "plan", "pi", "model"
	threadID := dispatchOrFail(t, c, create).ThreadID
	first := mustSend(t, c, "first-plan", threadID, "plan this")
	firstRun, _ := findRun(snapshotOf(t, c), first.RunID)
	if lifecycleThread(t, c, threadID).InteractionMode != "plan" || firstRun.InteractionMode != "plan" {
		t.Fatalf("mode not inherited: %+v", firstRun)
	}
	queueCmd := lifecycleCommand("queue-default", api.QueueMessage, threadID)
	queueCmd.Message, queueCmd.InteractionMode = "execute next", "default"
	queued := dispatchOrFail(t, c, queueCmd)
	input := waitInputStatus(t, c, queued.InputID, inputQueued)
	if input.Configuration == nil || input.Configuration.InteractionMode != "default" {
		t.Fatalf("queue did not capture explicit default: %+v", input)
	}
	delegate := delegateCommand("delegate-plan", first.RunID, firstRun.AttemptID, "child plan", resumeManual)
	child := dispatchOrFail(t, c, delegate)
	childRun, _ := findRun(snapshotOf(t, c), child.RunID)
	if lifecycleThread(t, c, child.ThreadID).InteractionMode != "plan" || childRun.InteractionMode != "plan" {
		t.Fatalf("child did not inherit plan mode: %+v", childRun)
	}
	delegate.CommandID, delegate.InteractionMode = "delegate-default", "default"
	otherChild := dispatchOrFail(t, c, delegate)
	if lifecycleThread(t, c, otherChild.ThreadID).InteractionMode != "default" {
		t.Fatal("explicit child mode was ignored")
	}
	update := lifecycleCommand("busy-mode", api.UpdateThread, threadID)
	update.InteractionMode = "default"
	_, err := c.Dispatch(context.Background(), update)
	requireCode(t, err, api.Conflict)
	for _, kind := range []api.CommandKind{api.CreateThread, api.SendMessage, api.QueueMessage, api.UpdateThread} {
		cmd := lifecycleCommand("invalid-mode", kind, threadID)
		cmd.Message, cmd.InteractionMode = "text", "unsupported"
		_, err := c.Dispatch(context.Background(), cmd)
		requireCode(t, err, api.InvalidCommand)
	}
}

func TestInteractionModeUpdateAndPerRunOverridePersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	c := openCoreAt(t, path, emitProvider("done"))
	threadID := mustCreateThread(t, c, "create")
	if lifecycleThread(t, c, threadID).InteractionMode != "default" {
		t.Fatal("empty mode did not resolve to default")
	}
	update := lifecycleCommand("mode-plan", api.UpdateThread, threadID)
	update.InteractionMode = "plan"
	receipt := dispatchOrFail(t, c, update)
	assertCommandReplay(t, c, update, receipt)
	send := sendCommand("send-default", threadID, "do this")
	send.InteractionMode = "default"
	run := waitRunStatus(t, c, dispatchOrFail(t, c, send).RunID, api.RunCompleted)
	if run.InteractionMode != "default" || lifecycleThread(t, c, threadID).InteractionMode != "plan" {
		t.Fatalf("per-run override changed thread defaults: %+v", run)
	}
	queued := mustQueue(t, c, "queue-plan", threadID, "plan this")
	if run := waitRunStatus(t, c, queued.RunID, api.RunCompleted); run.InteractionMode != "plan" {
		t.Fatalf("queue mode not copied to run: %+v", run)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openCoreAt(t, path, emitProvider())
	defer c.Close()
	if lifecycleThread(t, c, threadID).InteractionMode != "plan" || len(lifecycleRuns(t, c, threadID)) != 2 {
		t.Fatal("mode/history lost on reopen")
	}
	assertCommandReplay(t, c, update, receipt)
}

func TestForkThreadModelOverrideAndWorktreeIsolation(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Vantry Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}

	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	source := dispatchOrFail(t, c, api.Command{
		Version:    api.ProtocolVersion,
		CommandID:  "src",
		Kind:       api.CreateThread,
		Title:      "main line",
		ProviderID: "pi",
		Model:      "openai/gpt-5",
		Workspace:  &api.WorkspaceSpec{Kind: "directory", Path: repo},
	})
	seedLifecycleRun(t, c, source.ThreadID, api.RunCompleted, "q1", "a1", "", time.Now())

	forkCmd := api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "fork-wt-model",
		Kind:      api.ForkThread,
		ThreadID:  source.ThreadID,
		Title:     "alt branch",
		Model:     "anthropic/claude-opus-4",
		Workspace: &api.WorkspaceSpec{Kind: "worktree"},
	}
	receipt := dispatchOrFail(t, c, forkCmd)
	forked := lifecycleThread(t, c, receipt.ThreadID)
	if forked.Model != "anthropic/claude-opus-4" || forked.ProviderID != "pi" {
		t.Fatalf("forked model/provider = %q / %q", forked.Model, forked.ProviderID)
	}
	if forked.Workspace == nil || forked.Workspace.Kind != "worktree" || forked.Workspace.Repository == "" {
		t.Fatalf("forked worktree workspace = %+v", forked.Workspace)
	}
	assertCommandReplay(t, c, forkCmd, receipt)
}
