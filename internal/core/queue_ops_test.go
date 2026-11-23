package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func assertQueueIDs(t *testing.T, c *Core, threadID string, want ...string) {
	t.Helper()
	var queued []api.Input
	for _, input := range snapshotOf(t, c).Inputs {
		if input.ThreadID == threadID && input.Kind == inputKindQueue && input.Status == inputQueued {
			queued = append(queued, input)
		}
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].Position < queued[j].Position })
	if len(queued) != len(want) {
		t.Fatalf("queue has %d inputs, want %d: %+v", len(queued), len(want), queued)
	}
	for i, input := range queued {
		if input.Position != i || input.ID != want[i] {
			t.Fatalf("queue[%d] = %s at %d, want %s: %+v", i, input.ID, input.Position, want[i], queued)
		}
	}
}

func seedQueueInput(t *testing.T, c *Core, input inputRecord) {
	t.Helper()
	if err := c.write(context.Background(), func(tx *txn) error { return c.insertInput(tx, input) }); err != nil {
		t.Fatal(err)
	}
}

func TestEditAndReorderInputPreserveAttachmentsConfigurationAndExecutionOrder(t *testing.T) {
	gate := make(chan struct{})
	p := &funcProvider{runFn: func(ctx context.Context, request provider.Request, emit func(string) error) error {
		if request.Message == "first" {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return emit("done")
	}}
	c := openTestCore(t, p)
	c.validateAttachments = func([]api.Attachment) error { return nil }
	threadID := mustCreateThread(t, c, "create")
	first := mustQueue(t, c, "first", threadID, "first")
	waitRunStatus(t, c, first.RunID, api.RunRunning)
	attachment := api.Attachment{ID: "attachment", Name: "note.txt", MIME: "text/plain"}
	queueCmd := lifecycleCommand("second", api.QueueMessage, threadID)
	queueCmd.Message, queueCmd.Attachments, queueCmd.InteractionMode = "second", []api.Attachment{attachment}, "plan"
	second := dispatchOrFail(t, c, queueCmd)
	third := mustQueue(t, c, "third", threadID, "third")
	before := waitInputStatus(t, c, second.InputID, inputQueued)
	edit := lifecycleCommand("edit", api.EditInput, threadID)
	edit.InputID, edit.Message = second.InputID, "edited second"
	// Editing changes only the body, even when unrelated payload fields exist.
	edit.Attachments = []api.Attachment{{ID: "ignored-replacement"}}
	receipt := dispatchOrFail(t, c, edit)
	after := waitInputStatus(t, c, second.InputID, inputQueued)
	if after.Message != edit.Message || after.Position != before.Position || !after.CreatedAt.Equal(before.CreatedAt) ||
		!reflect.DeepEqual(before.Attachments, after.Attachments) || !reflect.DeepEqual(before.Configuration, after.Configuration) {
		t.Fatalf("edit changed fields other than the message: before %+v, after %+v", before, after)
	}
	assertCommandReplay(t, c, edit, receipt)
	edit.Message = "different"
	_, err := c.Dispatch(context.Background(), edit)
	requireCode(t, err, api.Conflict)

	reorder := lifecycleCommand("reorder", api.ReorderInput, threadID)
	reorder.InputID, reorder.Position = third.InputID, 0
	receipt = dispatchOrFail(t, c, reorder)
	assertQueueIDs(t, c, threadID, third.InputID, second.InputID)
	assertCommandReplay(t, c, reorder, receipt)
	reorder.Position = 1
	_, err = c.Dispatch(context.Background(), reorder)
	requireCode(t, err, api.Conflict)
	fourth := mustQueue(t, c, "fourth", threadID, "fourth")
	assertQueueIDs(t, c, threadID, third.InputID, second.InputID, fourth.InputID)
	close(gate)
	waitFor(t, "all edited/reordered inputs to complete", func() bool {
		runs := lifecycleRuns(t, c, threadID)
		if len(runs) != 4 {
			return false
		}
		for _, run := range runs {
			if run.Status != api.RunCompleted {
				return false
			}
		}
		return true
	})
	var messages []string
	for i := 0; i < p.callCount(); i++ {
		messages = append(messages, p.requestAt(i).Message)
	}
	if strings.Join(messages, ",") != "first,third,edited second,fourth" {
		t.Fatalf("execution order = %v", messages)
	}
	if !reflect.DeepEqual(p.requestAt(2).Attachments, []api.Attachment{attachment}) {
		t.Fatal("edit/reorder lost execution attachments")
	}
	input := waitInputStatus(t, c, second.InputID, inputStarted)
	run, _ := findRun(snapshotOf(t, c), input.RunID)
	if run.InteractionMode != "plan" {
		t.Fatalf("accepted queue mode was lost: %+v", run)
	}
	assertQueueIDs(t, c, threadID)
}

func TestQueueOperationsValidateIdentityStateAndPositionWithoutMutation(t *testing.T) {
	c := openTestCore(t, newWorkbenchProvider(t))
	pauseTestWorker(c)
	threadID := mustCreateThread(t, c, "create")
	seedLifecycleRun(t, c, threadID, api.RunRunning, "active", "", "", time.Now())
	for _, input := range []inputRecord{
		{ID: "queued", Kind: inputKindQueue, Status: inputQueued},
		{ID: "started", Kind: inputKindQueue, Status: inputStarted},
		{ID: "cancelled", Kind: inputKindQueue, Status: inputCancelled},
		{ID: "steer", Kind: inputKindSteer, Status: inputPending},
	} {
		input.ThreadID, input.Message = threadID, "body"
		seedQueueInput(t, c, input)
	}
	for _, kind := range []api.CommandKind{api.EditInput, api.ReorderInput, api.PromoteInputToSteer} {
		for _, tc := range []struct {
			id, thread string
			code       api.ErrorCode
		}{
			{"", threadID, api.InvalidCommand}, {"missing", threadID, api.NotFound},
			{"queued", "other-thread", api.NotFound}, {"started", threadID, api.Conflict},
			{"cancelled", threadID, api.Conflict}, {"steer", threadID, api.Conflict},
		} {
			t.Run(string(kind)+"/"+tc.id+"/"+tc.thread, func(t *testing.T) {
				cmd := lifecycleCommand("rejected", kind, tc.thread)
				cmd.InputID, cmd.Message = tc.id, "new body"
				before, receipts, effects := snapshotOf(t, c), countRows(t, c, "command_receipts"), countRows(t, c, "effect_outbox")
				_, err := c.Dispatch(context.Background(), cmd)
				requireCode(t, err, tc.code)
				if !reflect.DeepEqual(before, snapshotOf(t, c)) || receipts != countRows(t, c, "command_receipts") || effects != countRows(t, c, "effect_outbox") {
					t.Fatal("rejected operation changed state")
				}
			})
		}
	}
	for _, position := range []int{-1, 1, 100} {
		cmd := lifecycleCommand("bad-position", api.ReorderInput, threadID)
		cmd.InputID, cmd.Position = "queued", position
		_, err := c.Dispatch(context.Background(), cmd)
		requireCode(t, err, api.InvalidCommand)
	}
	emptyEdit := lifecycleCommand("empty-edit", api.EditInput, threadID)
	emptyEdit.InputID, emptyEdit.Message = "queued", " \n "
	_, err := c.Dispatch(context.Background(), emptyEdit)
	requireCode(t, err, api.InvalidCommand)
	// A no-op reorder still has a durable receipt and can be retried.
	noop := lifecycleCommand("noop", api.ReorderInput, threadID)
	noop.InputID = "queued"
	receipt := dispatchOrFail(t, c, noop)
	assertCommandReplay(t, c, noop, receipt)
}

func TestQueuePositionsStayContiguousAfterCancelAndReorderBothDirections(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	threadID := mustCreateThread(t, c, "thread")
	seedLifecycleRun(t, c, threadID, api.RunRunning, "active", "", "", time.Now())
	for _, id := range []string{"a", "b", "c", "d"} {
		seedQueueInput(t, c, inputRecord{ID: id, ThreadID: threadID, Kind: inputKindQueue, Status: inputQueued, Message: id})
	}
	move := lifecycleCommand("move-tail", api.ReorderInput, threadID)
	move.InputID, move.Position = "a", 3
	dispatchOrFail(t, c, move)
	assertQueueIDs(t, c, threadID, "b", "c", "d", "a")
	move.CommandID, move.InputID, move.Position = "move-middle", "a", 1
	dispatchOrFail(t, c, move)
	assertQueueIDs(t, c, threadID, "b", "a", "c", "d")
	cancel := lifecycleCommand("cancel", api.CancelInput, threadID)
	cancel.InputID = "a"
	dispatchOrFail(t, c, cancel)
	assertQueueIDs(t, c, threadID, "b", "c", "d")
	last := mustQueue(t, c, "append", threadID, "e")
	assertQueueIDs(t, c, threadID, "b", "c", "d", last.InputID)
}

func TestPromoteQueuedInputUsesSameIdentityAndDurableControl(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.onControl = func(control provider.Control) *provider.ControlResult {
		return &provider.ControlResult{ID: control.ID, Status: "delivered"}
	}
	c := openTestCore(t, p)
	c.validateAttachments = func([]api.Attachment) error { return nil }
	threadID := mustCreateThread(t, c, "thread")
	active := mustSend(t, c, "send", threadID, "active")
	run := waitRunStatus(t, c, active.RunID, api.RunRunning)
	older := mustQueue(t, c, "older", threadID, "older")
	cmd := lifecycleCommand("queue-selected", api.QueueMessage, threadID)
	attachment := api.Attachment{ID: "attachment", Name: "extra.txt", MIME: "text/plain"}
	cmd.Message, cmd.Attachments = "adjust now", []api.Attachment{attachment}
	selected := dispatchOrFail(t, c, cmd)
	cmd = lifecycleCommand("promote", api.PromoteInputToSteer, threadID)
	cmd.InputID = selected.InputID
	receipt := dispatchOrFail(t, c, cmd)
	if receipt.InputID != selected.InputID || receipt.RunID != active.RunID {
		t.Fatalf("promotion changed input identity or execution: %+v", receipt)
	}
	input := waitInputStatus(t, c, receipt.InputID, inputDelivered)
	if input.Kind != inputKindSteer || input.AttemptID != run.AttemptID || input.Message != "adjust now" || !reflect.DeepEqual(input.Attachments, []api.Attachment{attachment}) {
		t.Fatalf("promoted input: %+v", input)
	}
	waitFor(t, "one promotion control", func() bool { return len(p.controlCalls()) == 1 })
	control := p.controlCalls()[0]
	if control.ID != selected.InputID || control.Message != input.Message || !reflect.DeepEqual(control.Attachments, input.Attachments) {
		t.Fatalf("promotion control: %+v", control)
	}
	var payload string
	if err := c.store.db.QueryRow(`SELECT input FROM effect_outbox WHERE kind = ? AND run_id = ?`, effectKindProviderControl, active.RunID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var intent controlIntent
	if err := json.Unmarshal([]byte(payload), &intent); err != nil || intent.Request.AttemptID != run.AttemptID || intent.Control.ID != input.ID {
		t.Fatalf("durable promotion intent: %+v %v", intent, err)
	}
	assertQueueIDs(t, c, threadID, older.InputID)
	if countRows(t, c, "runs") != 1 || countRows(t, c, "inputs") != 2 {
		t.Fatal("promotion duplicated execution or input")
	}
	for _, legacy := range snapshotOf(t, c).Messages {
		if legacy.ID == selected.InputID {
			t.Fatal("promoted input remained in the legacy queue projection")
		}
	}
	assertCommandReplay(t, c, cmd, receipt)
	cmd.Message = "changed payload"
	_, err := c.Dispatch(context.Background(), cmd)
	requireCode(t, err, api.Conflict)
	p.finish()
	waitFor(t, "only the remaining queue input to run", func() bool {
		runs := lifecycleRuns(t, c, threadID)
		return len(runs) == 2 && runs[0].Status == api.RunCompleted && runs[1].Status == api.RunCompleted
	})
	if len(p.controlCalls()) != 1 {
		t.Fatal("promotion replay resent control")
	}
}

func TestPromoteQueuedInputAfterCompletionStartsCapturedConfiguration(t *testing.T) {
	c := openTestCore(t, emitProvider()) // Idle fallback does not require Controller.
	pauseTestWorker(c)
	threadID := mustCreateThread(t, c, "thread")
	seedLifecycleRun(t, c, threadID, api.RunCompleted, "previous", "result", "", time.Now().Add(-time.Hour))
	seedQueueInput(t, c, inputRecord{ID: "older", ThreadID: threadID, Kind: inputKindQueue, Status: inputQueued, Message: "older"})
	seedQueueInput(t, c, inputRecord{ID: "selected", ThreadID: threadID, Kind: inputKindQueue, Status: inputQueued, Message: "next",
		Configuration: &api.InputConfiguration{ProviderID: "pi", Model: "captured", ApprovalPolicy: "ask", InteractionMode: "plan"}})
	cmd := lifecycleCommand("promote", api.PromoteInputToSteer, threadID)
	cmd.InputID = "selected"
	receipt := dispatchOrFail(t, c, cmd)
	input := waitInputStatus(t, c, "selected", inputStarted)
	run, ok := findRun(snapshotOf(t, c), receipt.RunID)
	if !ok || input.RunID != run.ID || run.Input != "next" || run.Model != "captured" || run.InteractionMode != "plan" || run.ApprovalPolicy != "ask" || input.Kind != inputKindQueue {
		t.Fatalf("idle promotion: %+v %+v %+v", receipt, input, run)
	}
	assertQueueIDs(t, c, threadID, "older")
	assertCommandReplay(t, c, cmd, receipt)
	var controls int
	if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM effect_outbox WHERE kind = ?`, effectKindProviderControl).Scan(&controls); err != nil || controls != 0 {
		t.Fatalf("idle promotion scheduled a control: %d %v", controls, err)
	}
}

func TestPromotionConflictsPreserveQueuedInput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     api.RunStatus
		controller bool
		archived   bool
		code       api.ErrorCode
	}{
		{"queued-run", api.RunQueued, true, false, api.Conflict},
		{"waiting-run", api.RunWaiting, true, false, api.Conflict},
		{"unsupported", api.RunRunning, false, false, api.InvalidCommand},
		{"archived-idle", api.RunCompleted, true, true, api.Conflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p provider.Provider = emitProvider()
			if tc.controller {
				p = newWorkbenchProvider(t)
			}
			c := openTestCore(t, p)
			pauseTestWorker(c)
			threadID := mustCreateThread(t, c, "create")
			seedLifecycleRun(t, c, threadID, tc.status, "active", "", "", time.Now())
			seedQueueInput(t, c, inputRecord{ID: "selected", ThreadID: threadID, Kind: inputKindQueue, Status: inputQueued, Message: "next"})
			if tc.archived {
				if err := c.write(context.Background(), func(tx *txn) error {
					_, err := tx.exec(`UPDATE threads SET archived = 1 WHERE id = ?`, threadID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotOf(t, c)
			cmd := lifecycleCommand("promote", api.PromoteInputToSteer, threadID)
			cmd.InputID = "selected"
			_, err := c.Dispatch(context.Background(), cmd)
			requireCode(t, err, tc.code)
			if !reflect.DeepEqual(before, snapshotOf(t, c)) {
				t.Fatal("rejected promotion changed its queue input")
			}
		})
	}
}

func TestQueueOperationsRollBackInputOrderingControlAndReceipts(t *testing.T) {
	for _, kind := range []api.CommandKind{api.EditInput, api.ReorderInput, api.PromoteInputToSteer} {
		t.Run(string(kind), func(t *testing.T) {
			c := openTestCore(t, newWorkbenchProvider(t))
			pauseTestWorker(c)
			threadID := mustCreateThread(t, c, "thread")
			seedLifecycleRun(t, c, threadID, api.RunRunning, "active", "", "", time.Now())
			for _, id := range []string{"a", "b"} {
				seedQueueInput(t, c, inputRecord{ID: id, ThreadID: threadID, Kind: inputKindQueue, Status: inputQueued, Message: id})
			}
			cmd := lifecycleCommand("operation", kind, threadID)
			cmd.InputID, cmd.Message, cmd.Position = "b", "new body", 0
			before, effects, receipts := snapshotOf(t, c), countRows(t, c, "effect_outbox"), countRows(t, c, "command_receipts")
			c.setTestHookPreCommit(func(*sql.Tx) error { return errors.New("rollback queue operation") })
			_, err := c.Dispatch(context.Background(), cmd)
			c.setTestHookPreCommit(nil)
			if err == nil || !reflect.DeepEqual(before, snapshotOf(t, c)) || effects != countRows(t, c, "effect_outbox") || receipts != countRows(t, c, "command_receipts") {
				t.Fatal("queue transaction leaked changes after rollback")
			}
			receipt := dispatchOrFail(t, c, cmd)
			assertCommandReplay(t, c, cmd, receipt)
		})
	}
}

func TestConcurrentPromotionRetriesCreateOnlyOneControl(t *testing.T) {
	c := openTestCore(t, newWorkbenchProvider(t))
	pauseTestWorker(c)
	threadID := mustCreateThread(t, c, "thread")
	seedLifecycleRun(t, c, threadID, api.RunRunning, "active", "", "", time.Now())
	seedQueueInput(t, c, inputRecord{ID: "queued", ThreadID: threadID, Kind: inputKindQueue, Status: inputQueued, Message: "adjust"})
	cmd := lifecycleCommand("promote", api.PromoteInputToSteer, threadID)
	cmd.InputID = "queued"
	type result struct {
		receipt api.Receipt
		err     error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := c.Dispatch(context.Background(), cmd)
			results <- result{receipt, err}
		}()
	}
	wg.Wait()
	close(results)
	var first api.Receipt
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first.CommandID == "" {
			first = got.receipt
		} else if got.receipt != first {
			t.Fatalf("concurrent receipt mismatch: %+v %+v", first, got.receipt)
		}
	}
	var controls int
	if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM effect_outbox WHERE kind = ?`, effectKindProviderControl).Scan(&controls); err != nil || controls != 1 {
		t.Fatalf("concurrent promotion created %d controls: %v", controls, err)
	}
}

func TestSteerCompletionFallbackModesAndStrictTerminalConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    api.RunStatus
		kind      api.CommandKind
		runID     bool
		attempt   bool
		fallback  bool
		wantError bool
	}{
		{"thread-default", api.RunCompleted, api.SteerRun, true, false, false, false},
		{"thread-without-run", api.RunCompleted, api.SteerRun, false, false, false, false},
		{"legacy-alias", api.RunCompleted, api.SteerMessage, true, false, false, false},
		{"explicit-opt-in", api.RunCompleted, api.SteerRun, true, true, true, false},
		{"precise-attempt", api.RunCompleted, api.SteerRun, true, true, false, true},
		{"cancelled", api.RunCancelled, api.SteerRun, true, false, true, true},
		{"failed", api.RunFailed, api.SteerRun, true, false, true, true},
		{"interrupted", api.RunInterrupted, api.SteerRun, true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openTestCore(t, emitProvider())
			pauseTestWorker(c)
			threadID := mustCreateThread(t, c, "thread")
			previous := seedLifecycleRun(t, c, threadID, tc.status, "previous", "result", "", time.Now().Add(-time.Hour))
			cmd := lifecycleCommand("steer", tc.kind, threadID)
			cmd.Message, cmd.FallbackToQueue, cmd.InteractionMode = "adjust next", tc.fallback, "plan"
			if tc.runID {
				cmd.RunID = previous.ID
			}
			if tc.attempt {
				cmd.ParentAttemptID = previous.AttemptID
			}
			before := snapshotOf(t, c)
			receipt, err := c.Dispatch(context.Background(), cmd)
			if tc.wantError {
				requireCode(t, err, api.Conflict)
				if !reflect.DeepEqual(before, snapshotOf(t, c)) {
					t.Fatal("rejected terminal steer changed state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			input := waitInputStatus(t, c, receipt.InputID, inputStarted)
			run, _ := findRun(snapshotOf(t, c), receipt.RunID)
			if input.Kind != inputKindQueue || receipt.RunID == previous.ID || run.Input != cmd.Message || run.InteractionMode != "plan" {
				t.Fatalf("completion fallback: %+v %+v", input, run)
			}
			assertCommandReplay(t, c, cmd, receipt)
			if countRows(t, c, "command_receipts") != 2 || countRows(t, c, "runs") != 2 {
				t.Fatal("completion fallback created a separate command or duplicate run")
			}
		})
	}
}

func TestThreadSteerCanStartAnEmptyThreadAndResolveActiveAttempt(t *testing.T) {
	c := openTestCore(t, newWorkbenchProvider(t))
	pauseTestWorker(c)
	threadID := mustCreateThread(t, c, "thread")
	cmd := lifecycleCommand("steer-empty", api.SteerRun, threadID)
	cmd.Message = "first instruction"
	receipt := dispatchOrFail(t, c, cmd)
	if receipt.RunID == "" || receipt.InputID == "" {
		t.Fatalf("empty thread steer did not launch: %+v", receipt)
	}
	if err := c.write(context.Background(), func(tx *txn) error {
		_, err := tx.exec(`UPDATE runs SET status = 'running' WHERE id = ?`, receipt.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cmd.CommandID, cmd.Message = "steer-running", "adjust current"
	steered := dispatchOrFail(t, c, cmd)
	input := waitInputStatus(t, c, steered.InputID, inputPending)
	if input.RunID != receipt.RunID || input.AttemptID == "" || input.Kind != inputKindSteer {
		t.Fatalf("thread steer failed to resolve attempt: %+v", input)
	}
	assertCommandReplay(t, c, cmd, steered)
}

func TestSteerCompletedRunQueuesBehindNewerExecutionAndBacklog(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	threadID := mustCreateThread(t, c, "thread")
	completed := seedLifecycleRun(t, c, threadID, api.RunCompleted, "previous", "done", "", time.Now().Add(-time.Hour))
	seedLifecycleRun(t, c, threadID, api.RunRunning, "new active", "", "", time.Now())
	older := mustQueue(t, c, "older", threadID, "already queued")
	cmd := lifecycleCommand("late-steer", api.SteerRun, threadID)
	cmd.RunID, cmd.Message = completed.ID, "late instruction"
	receipt := dispatchOrFail(t, c, cmd)
	if receipt.RunID != "" || waitInputStatus(t, c, receipt.InputID, inputQueued).Kind != inputKindQueue {
		t.Fatalf("late steer bypassed new execution: %+v", receipt)
	}
	assertQueueIDs(t, c, threadID, older.InputID, receipt.InputID)
	assertCommandReplay(t, c, cmd, receipt)
}

func TestSteerWaitsForCompletionCommitAndAtomicallyStartsNextRun(t *testing.T) {
	providerGate := make(chan struct{})
	p := &funcProvider{runFn: func(ctx context.Context, req provider.Request, emit func(string) error) error {
		if req.Message == "first" {
			select {
			case <-providerGate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return emit("done")
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "thread")
	first := mustSend(t, c, "send", threadID, "first")
	waitRunStatus(t, c, first.RunID, api.RunRunning)
	completionEntered, releaseCommit := make(chan struct{}), make(chan struct{})
	c.setTestHookPreCommit(func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, first.RunID).Scan(&status); err != nil {
			return err
		}
		if status == statusCompleted {
			// This callback owns wmu, so it may clear itself without a second lock.
			c.testHookPreCommit = nil
			close(completionEntered)
			<-releaseCommit
		}
		return nil
	})
	close(providerGate)
	select {
	case <-completionEntered:
	case <-time.After(testTimeout):
		t.Fatal("completion never reached its commit boundary")
	}
	type result struct {
		receipt api.Receipt
		err     error
	}
	resultCh := make(chan result, 1)
	cmd := lifecycleCommand("steer-at-completion", api.SteerRun, threadID)
	cmd.RunID, cmd.Message = first.RunID, "next instruction"
	go func() {
		receipt, err := c.Dispatch(context.Background(), cmd)
		resultCh <- result{receipt, err}
	}()
	close(releaseCommit)
	var got result
	select {
	case got = <-resultCh:
	case <-time.After(testTimeout):
		t.Fatal("steer did not leave the completion boundary")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	run := waitRunStatus(t, c, got.receipt.RunID, api.RunCompleted)
	if run.Input != cmd.Message || p.callCount() != 2 {
		t.Fatalf("completion lost or duplicated the steer: %+v (%d calls)", run, p.callCount())
	}
	assertCommandReplay(t, c, cmd, got.receipt)
}

func TestSteerFallbackRollbackAndValidation(t *testing.T) {
	c := openTestCore(t, emitProvider())
	pauseTestWorker(c)
	threadID := mustCreateThread(t, c, "thread")
	previous := seedLifecycleRun(t, c, threadID, api.RunCompleted, "first", "done", "", time.Now())
	cmd := lifecycleCommand("steer", api.SteerRun, threadID)
	cmd.RunID, cmd.Message = previous.ID, "next"
	before, effects, receipts := snapshotOf(t, c), countRows(t, c, "effect_outbox"), countRows(t, c, "command_receipts")
	c.setTestHookPreCommit(func(*sql.Tx) error { return errors.New("rollback fallback") })
	_, err := c.Dispatch(context.Background(), cmd)
	c.setTestHookPreCommit(nil)
	if err == nil || !reflect.DeepEqual(before, snapshotOf(t, c)) || effects != countRows(t, c, "effect_outbox") || receipts != countRows(t, c, "command_receipts") {
		t.Fatal("failed fallback leaked a message, execution or receipt")
	}
	for i, bad := range []api.Command{
		{ThreadID: "missing", Message: "message"},
		{ThreadID: threadID, RunID: "missing", Message: "message"},
		{ThreadID: threadID, ParentAttemptID: previous.AttemptID, Message: "message"},
		{ThreadID: threadID, Message: " "},
	} {
		bad.Version, bad.CommandID, bad.Kind = api.ProtocolVersion, fmt.Sprintf("invalid-%d", i), api.SteerRun
		want := api.NotFound
		if i >= 2 {
			want = api.InvalidCommand
		}
		_, err := c.Dispatch(context.Background(), bad)
		requireCode(t, err, want)
	}
	flag := true
	archive := lifecycleCommand("archive", api.UpdateThread, threadID)
	archive.Archived = &flag
	dispatchOrFail(t, c, archive)
	_, err = c.Dispatch(context.Background(), cmd)
	requireCode(t, err, api.Conflict)
}
