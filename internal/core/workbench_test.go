package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// TestQueueMessageStartsImmediatelyWhenIdle proves the idle path: the message
// becomes the run in the same transaction and the input records that identity.
func TestQueueMessageStartsImmediatelyWhenIdle(t *testing.T) {
	c := openTestCore(t, emitProvider("hello"))
	threadID := mustCreateThread(t, c, "create-1")

	receipt := mustQueue(t, c, "queue-1", threadID, "first message")
	if !receipt.Accepted || receipt.InputID == "" || receipt.RunID == "" || receipt.Sequence == 0 {
		t.Fatalf("unexpected queue receipt: %+v", receipt)
	}
	run := waitRunStatus(t, c, receipt.RunID, api.RunCompleted)
	input := waitInputStatus(t, c, receipt.InputID, inputStarted)
	if input.RunID != run.ID || input.AttemptID != run.AttemptID || input.ThreadID != threadID {
		t.Fatalf("queued input did not record its run: %+v (run %+v)", input, run)
	}
	if input.Kind != inputKindQueue || input.Message != "first message" {
		t.Fatalf("unexpected input projection: %+v", input)
	}
	if payload := firstEventData(t, c, eventInputUpdated); len(payload["input"]) == 0 {
		t.Fatalf("input.updated payload is not {input: ...}: %s", payload)
	}

	// An identical retry is answered from the receipt and starts nothing new.
	again := mustQueue(t, c, "queue-1", threadID, "first message")
	if again != receipt {
		t.Fatalf("duplicate queue receipt mismatch: %+v vs %+v", again, receipt)
	}
	if got := countRows(t, c, "runs"); got != 1 {
		t.Fatalf("got %d runs after a duplicate queue, want 1", got)
	}
}

// TestQueueMessageRunsInFIFOOrder proves only one item runs at a time and that
// the queued messages start in submission order once the slot is released.
func TestQueueMessageRunsInFIFOOrder(t *testing.T) {
	gate := make(chan struct{})
	var (
		mu    sync.Mutex
		order []string
	)
	p := &routeProvider{fn: func(ctx context.Context, request provider.Request, emit func(string) error) error {
		if request.Message == "first" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-gate:
			}
		}
		mu.Lock()
		order = append(order, request.Message)
		mu.Unlock()
		return emit("out: " + request.Message)
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")

	first := mustQueue(t, c, "queue-first", threadID, "first")
	waitRunStatus(t, c, first.RunID, api.RunRunning)
	second := mustQueue(t, c, "queue-second", threadID, "second")
	third := mustQueue(t, c, "queue-third", threadID, "third")

	for _, receipt := range []api.Receipt{second, third} {
		if receipt.RunID != "" {
			t.Fatalf("queued message %q started while the slot was busy: %+v", receipt.InputID, receipt)
		}
		input, ok := findInput(snapshotOf(t, c), receipt.InputID)
		if !ok || input.Status != inputQueued {
			t.Fatalf("queued input %q is not queued: %+v", receipt.InputID, input)
		}
	}
	if got := countRows(t, c, "runs"); got != 1 {
		t.Fatalf("got %d runs while one run is active, want 1", got)
	}

	close(gate)
	waitRunStatus(t, c, first.RunID, api.RunCompleted)
	waitFor(t, "all queued messages to run", func() bool {
		snap := snapshotOf(t, c)
		if len(snap.Runs) != 3 {
			return false
		}
		for _, run := range snap.Runs {
			if run.Status != api.RunCompleted {
				return false
			}
		}
		return true
	})

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "first,second,third" {
		t.Fatalf("provider ran %v, want FIFO order", order)
	}
	for _, receipt := range []api.Receipt{second, third} {
		input := waitInputStatus(t, c, receipt.InputID, inputStarted)
		if input.RunID == "" {
			t.Fatalf("queued input %q never recorded its run", receipt.InputID)
		}
	}
}

// TestCancelInputOnlyCancelsQueuedMessage proves input.cancel cannot withdraw a
// message that already became a run.
func TestCancelInputOnlyCancelsQueuedMessage(t *testing.T) {
	gate := make(chan struct{})
	p := &routeProvider{fn: func(ctx context.Context, _ provider.Request, emit func(string) error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
		return emit("done")
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")

	first := mustQueue(t, c, "queue-first", threadID, "first")
	waitRunStatus(t, c, first.RunID, api.RunRunning)
	second := mustQueue(t, c, "queue-second", threadID, "second")

	cancelled, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "cancel-second", Kind: api.CancelInput, InputID: second.InputID,
	})
	if err != nil {
		t.Fatalf("cancel queued input: %v", err)
	}
	if cancelled.InputID != second.InputID || !cancelled.Accepted {
		t.Fatalf("unexpected cancel receipt: %+v", cancelled)
	}
	if input := waitInputStatus(t, c, second.InputID, inputCancelled); input.RunID != "" {
		t.Fatalf("cancelled input still points at a run: %+v", input)
	}

	// Repeating the withdrawal with a new command id is still accepted.
	again, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "cancel-second-again", Kind: api.CancelInput, InputID: second.InputID,
	})
	if err != nil || !again.Accepted {
		t.Fatalf("second cancel of the same input: %+v %v", again, err)
	}

	// A message that already started cannot be withdrawn.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "cancel-first", Kind: api.CancelInput, InputID: first.InputID,
	})
	requireCode(t, err, api.Conflict)

	// An unknown input is not found.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "cancel-unknown", Kind: api.CancelInput, InputID: "inp_missing",
	})
	requireCode(t, err, api.NotFound)

	close(gate)
	waitRunStatus(t, c, first.RunID, api.RunCompleted)
	time.Sleep(50 * time.Millisecond)
	if got := countRows(t, c, "runs"); got != 1 {
		t.Fatalf("a cancelled queued message started anyway (%d runs)", got)
	}
}

// TestRunStopCancelsQueuedInputs proves a stopped thread cannot continue on its
// own with messages that were still waiting.
func TestRunStopCancelsQueuedInputs(t *testing.T) {
	p := &routeProvider{fn: func(ctx context.Context, _ provider.Request, emit func(string) error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(testTimeout):
			return emit("late")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")

	first := mustQueue(t, c, "queue-first", threadID, "first")
	waitRunStatus(t, c, first.RunID, api.RunRunning)
	second := mustQueue(t, c, "queue-second", threadID, "second")

	if _, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "stop-1", Kind: api.StopRun,
		ThreadID: threadID, RunID: first.RunID,
	}); err != nil {
		t.Fatalf("stop run: %v", err)
	}
	waitRunStatus(t, c, first.RunID, api.RunCancelled)
	if input := waitInputStatus(t, c, second.InputID, inputCancelled); input.RunID != "" {
		t.Fatalf("cancelled queue input points at a run: %+v", input)
	}
	time.Sleep(50 * time.Millisecond)
	if got := countRows(t, c, "runs"); got != 1 {
		t.Fatalf("a stopped thread started another run (%d runs)", got)
	}
}

// TestQueuedMessagesSurviveRestartInOrder proves the FIFO is durable: the
// recovery barrier resumes the same order instead of dropping the backlog.
func TestQueuedMessagesSurviveRestartInOrder(t *testing.T) {
	gate := make(chan struct{})
	first := &routeProvider{fn: func(ctx context.Context, _ provider.Request, emit func(string) error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
		return emit("first")
	}}
	path := filepath.Join(t.TempDir(), "core.db")
	crashed := openCoreAt(t, path, first)
	threadID := mustCreateThread(t, crashed, "create-1")
	active := mustQueue(t, crashed, "queue-first", threadID, "first")
	waitRunStatus(t, crashed, active.RunID, api.RunRunning)
	second := mustQueue(t, crashed, "queue-second", threadID, "second")
	third := mustQueue(t, crashed, "queue-third", threadID, "third")
	crashForTest(crashed)

	// Re-open the same database with a provider that completes immediately.
	again := openCoreAt(t, path, emitProvider("ok"))
	t.Cleanup(func() {
		if err := again.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	waitFor(t, "queued backlog to run after restart", func() bool {
		completed := 0
		for _, candidate := range snapshotOf(t, again).Runs {
			if candidate.Status == api.RunCompleted {
				completed++
			}
		}
		return completed == 2 && countRows(t, again, "runs") == 3
	})
	snap := snapshotOf(t, again)
	var order []string
	for _, run := range snap.Runs {
		order = append(order, run.Input)
	}
	if strings.Join(order, ",") != "first,second,third" {
		t.Fatalf("recovered run order = %v, want first,second,third", order)
	}
	waitInputStatus(t, again, second.InputID, inputStarted)
	waitInputStatus(t, again, third.InputID, inputStarted)
}

// TestWaitingParentContinuationOvertakesQueuedMessage proves a parked parent's
// continuation keeps the thread slot: a queued message never preempts it.
func TestWaitingParentContinuationOvertakesQueuedMessage(t *testing.T) {
	parentGate := make(chan struct{})
	childGate := make(chan struct{})
	var (
		mu    sync.Mutex
		calls []string
	)
	p := &routeProvider{fn: func(ctx context.Context, request provider.Request, emit func(string) error) error {
		mu.Lock()
		calls = append(calls, request.Message)
		mu.Unlock()
		switch request.Message {
		case "parent first":
			<-parentGate
			return emit("parent output")
		case "child work":
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-childGate:
			}
			return emit("child output")
		default:
			return emit("handled")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent first")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentAttempt := latestAttempt(t, c, parent.RunID).AttemptID
	_ = dispatchOrFail(t, c, delegateCommand("delegate-1", parent.RunID, parentAttempt, "child work", ""))

	close(parentGate)
	waitRunStatus(t, c, parent.RunID, api.RunWaiting)

	queued := mustQueue(t, c, "queue-1", threadID, "queued after parent")
	input, ok := findInput(snapshotOf(t, c), queued.InputID)
	if !ok || input.Status != inputQueued {
		t.Fatalf("the waiting parent did not keep the slot: %+v", input)
	}

	close(childGate)
	// The parent continuation runs first; the queued message follows.
	started := waitInputStatus(t, c, queued.InputID, inputStarted)
	if started.RunID == "" {
		t.Fatalf("started queued input has no run: %+v", started)
	}
	waitRunStatus(t, c, started.RunID, api.RunCompleted)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("provider calls = %v, want parent, child, continuation and queued message", calls)
	}
	if calls[0] != "parent first" || calls[1] != "child work" {
		t.Fatalf("unexpected first calls: %v", calls)
	}
	if !strings.Contains(calls[2], "父任务") {
		t.Fatalf("queued message overtook the parent continuation: %q", calls[2])
	}
}

// TestSteerRequiresPreciseActiveAttempt proves steering is refused when the
// provider cannot control Pi or when the addressed attempt is not current.
func TestSteerRequiresPreciseActiveAttempt(t *testing.T) {
	inner := newWorkbenchProvider(t)
	c := openTestCore(t, &nonControllerProvider{inner: inner})
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "steer-unsupported", Kind: api.SteerRun,
		ThreadID: threadID, RunID: sent.RunID, ParentAttemptID: attemptID, Message: "go left",
	})
	requireCode(t, err, api.InvalidCommand)

	inner.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)

	// A settled run has no active attempt to steer.
	controller := newWorkbenchProvider(t)
	controller.recover = true
	second := openTestCore(t, controller)
	threadID = mustCreateThread(t, second, "create-1")
	sent = mustSend(t, second, "send-1", threadID, "hello")
	waitRunStatus(t, second, sent.RunID, api.RunRunning)
	attemptID = latestAttempt(t, second, sent.RunID).AttemptID

	_, err = second.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "steer-stale", Kind: api.SteerRun,
		ThreadID: threadID, RunID: sent.RunID, ParentAttemptID: "att_stale", Message: "go left",
	})
	requireCode(t, err, api.Conflict)

	_, err = second.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "steer-wrong-run", Kind: api.SteerRun,
		ThreadID: threadID, RunID: "run_missing", ParentAttemptID: attemptID, Message: "go left",
	})
	requireCode(t, err, api.NotFound)
	controller.finish()
}

// TestSteerStaysPendingUntilControlResult proves a nil Control return is only a
// durable handoff: the steer becomes delivered only after control_result.
func TestSteerStaysPendingUntilControlResult(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	receipt := mustSteer(t, c, "steer-1", threadID, sent.RunID, attemptID, "focus on tests")
	if receipt.InputID == "" || receipt.Sequence == 0 {
		t.Fatalf("unexpected steer receipt: %+v", receipt)
	}
	waitFor(t, "control handoff", func() bool { return len(p.controlCalls()) == 1 })
	control := p.controlCalls()[0]
	if control.ID != receipt.InputID || control.Kind != "steer" || control.Message != "focus on tests" {
		t.Fatalf("unexpected control intent: %+v", control)
	}
	input := waitInputStatus(t, c, receipt.InputID, inputPending)
	if input.AttemptID != attemptID || input.RunID != sent.RunID {
		t.Fatalf("steer input lost its target attempt: %+v", input)
	}

	// Pi reports acceptance: only now is the steer delivered.
	p.add("control_result", provider.ControlResult{ID: receipt.InputID, Status: "delivered"})
	waitInputStatus(t, c, receipt.InputID, inputDelivered)
	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
}

// TestSteerFailsWhenAttemptEnds proves an undelivered steer cannot survive the
// attempt that was supposed to receive it.
func TestSteerFailsWhenAttemptEnds(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	p.onControl = func(provider.Control) *provider.ControlResult { return nil }
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	accepted := mustSteer(t, c, "steer-1", threadID, sent.RunID, attemptID, "too late")
	failed := mustSteer(t, c, "steer-2", threadID, sent.RunID, attemptID, "also too late")
	waitFor(t, "both controls to be handed over", func() bool { return len(p.controlCalls()) == 2 })
	p.add("control_result", provider.ControlResult{
		ID: accepted.InputID, Status: "failed", Error: "worker refused the instruction",
	})
	waitInputStatus(t, c, accepted.InputID, inputFailed)

	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
	if input := waitInputStatus(t, c, failed.InputID, inputFailed); input.Error == "" {
		t.Fatalf("a steer left behind by the attempt has no reason: %+v", input)
	}
}

// TestToolEventsProjectBoundedOutput proves real Pi tool events become one
// bounded ToolCall projection keyed by attempt and provider call id.
func TestToolEventsProjectBoundedOutput(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	p.add("tool_execution_start", map[string]any{
		"toolCallId": "call-1", "toolName": "bash", "args": map[string]any{"command": "echo hi"},
	})
	p.add("tool_execution_update", map[string]any{
		"toolCallId": "call-1", "toolName": "bash", "partialResult": "partial output",
	})
	huge := "HEAD_START_MARKER\n" + strings.Repeat("x", 40*1024) + "\nTAIL_ERROR_MARKER"
	p.add("tool_execution_end", map[string]any{
		"toolCallId": "call-1", "toolName": "bash", "result": huge,
	})

	toolID := attemptID + ":call-1"
	var tool api.ToolCall
	waitFor(t, "tool projection", func() bool {
		found, ok := findTool(snapshotOf(t, c), toolID)
		if !ok {
			return false
		}
		tool = found
		return found.Status == toolCompleted
	})
	if tool.Name != "bash" || tool.RunID != sent.RunID || tool.ThreadID != threadID {
		t.Fatalf("unexpected tool projection: %+v", tool)
	}
	if len(tool.Output) > maxToolOutputBytes {
		t.Fatalf("tool output is %d bytes, want at most %d", len(tool.Output), maxToolOutputBytes)
	}
	if !strings.Contains(tool.Output, "HEAD_START_MARKER") {
		t.Fatalf("truncated tool output lost head marker")
	}
	if !strings.Contains(tool.Output, "TAIL_ERROR_MARKER") {
		t.Fatalf("truncated tool output lost tail error marker")
	}
	if !strings.Contains(tool.Output, "bytes truncated") {
		t.Fatalf("truncated tool output lost its marker: %q", tool.Output)
	}
	if !utf8.ValidString(tool.Output) {
		t.Fatalf("truncated tool output produced invalid UTF-8")
	}
	var args map[string]string
	if err := json.Unmarshal(tool.Arguments, &args); err != nil || args["command"] != "echo hi" {
		t.Fatalf("raw tool arguments were not preserved: %s (%v)", tool.Arguments, err)
	}

	payload := firstEventData(t, c, eventToolUpdated)
	if _, ok := payload["tool"]; !ok {
		t.Fatalf("tool.updated payload is not {tool: ...}: %s", payload)
	}

	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
}

// firstEventData returns the first committed payload of one event type.
func firstEventData(t *testing.T, c *Core, eventType string) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := c.store.db.QueryRow(`SELECT data FROM events WHERE type = ? ORDER BY sequence LIMIT 1`,
		eventType).Scan(&raw); err != nil {
		t.Fatalf("read %s event: %v", eventType, err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode %s payload: %v", eventType, err)
	}
	return payload
}

// TestRequestRespondProjectsDialog proves a real Pi dialog is projected, that
// select answers must come from the offered options, and that only control_result
// completes the response.
func TestRequestRespondProjectsDialog(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	p.add("extension_ui_request", map[string]any{
		"id": "ui-1", "method": "select", "title": "Which suite?",
		"message": "Pick one", "options": []string{"unit", "integration"},
	})
	p.add("extension_ui_request", map[string]any{
		"id": "ui-ignored", "method": "notify", "title": "not an approval",
	})

	requestID := attemptID + ":ui-1"
	var request api.ProviderRequest
	waitFor(t, "pending dialog", func() bool {
		found, ok := findRequest(snapshotOf(t, c), requestID)
		if !ok {
			return false
		}
		request = found
		return found.Status == requestPending
	})
	if request.Method != "select" || request.ProviderRequestID != "ui-1" ||
		len(request.Options) != 2 || request.Title != "Which suite?" {
		t.Fatalf("unexpected dialog projection: %+v", request)
	}
	if _, ok := findRequest(snapshotOf(t, c), attemptID+":ui-ignored"); ok {
		t.Fatal("a non-blocking notification was recorded as an approval request")
	}
	payload := firstEventData(t, c, eventRequestUpdated)
	if _, ok := payload["request"]; !ok {
		t.Fatalf("request.updated payload is not {request: ...}: %s", payload)
	}

	// A value outside the offered options is refused.
	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "respond-invalid", Kind: api.RespondRequest,
		RequestID: requestID, Value: "e2e",
	})
	requireCode(t, err, api.InvalidCommand)

	accepted := mustRespond(t, c, "respond-1", requestID, "integration", false, false)
	if accepted.RequestID != requestID || accepted.RunID != sent.RunID {
		t.Fatalf("unexpected respond receipt: %+v", accepted)
	}
	if request := waitRequestStatus(t, c, requestID, requestResponding); request.Status != requestResponding {
		t.Fatalf("accepted response did not move the request to responding: %+v", request)
	}

	// Another command can never overwrite the accepted response.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "respond-2", Kind: api.RespondRequest,
		RequestID: requestID, Value: "unit",
	})
	requireCode(t, err, api.Conflict)

	waitFor(t, "dialog handoff", func() bool { return len(p.controlCalls()) == 1 })
	control := p.controlCalls()[0]
	if control.ID != requestID || control.RequestID != "ui-1" || control.Value != "integration" || control.Kind != "response" {
		t.Fatalf("unexpected response control: %+v", control)
	}

	// Still responding until Pi acknowledges.
	if request, _ := findRequest(snapshotOf(t, c), requestID); request.Status != requestResponding {
		t.Fatalf("request completed before control_result: %+v", request)
	}
	p.add("control_result", provider.ControlResult{ID: requestID, Status: "delivered"})
	waitRequestStatus(t, c, requestID, requestResponded)

	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
}

func waitRequestStatus(t *testing.T, c *Core, requestID, status string) api.ProviderRequest {
	t.Helper()
	var request api.ProviderRequest
	waitFor(t, "request "+requestID+" to become "+status, func() bool {
		found, ok := findRequest(snapshotOf(t, c), requestID)
		if !ok {
			return false
		}
		request = found
		return found.Status == status
	})
	return request
}

// TestRequestRespondForConfirmAndEditor proves both a denial and a text answer
// carry a value the provider can act on, and that an unfinished dialog expires
// with its attempt instead of looking approved.
func TestRequestRespondForConfirmAndEditor(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	p.add("extension_ui_request", map[string]any{
		"id": "ui-confirm", "method": "confirm", "title": "Run bash?",
	})
	p.add("extension_ui_request", map[string]any{
		"id": "ui-editor", "method": "editor", "title": "Edit instructions",
	})
	confirmID := attemptID + ":ui-confirm"
	editorID := attemptID + ":ui-editor"
	waitRequestStatus(t, c, editorID, requestPending)

	// Denying a confirmation is a valid answer without a value.
	mustRespond(t, c, "confirm-1", confirmID, "", false, false)
	waitFor(t, "confirm handoff", func() bool { return len(p.controlCalls()) == 1 })
	if control := p.controlCalls()[0]; control.Confirmed || control.ID != confirmID {
		t.Fatalf("unexpected confirm control: %+v", control)
	}
	p.add("control_result", provider.ControlResult{ID: confirmID, Status: "delivered"})
	waitRequestStatus(t, c, confirmID, requestResponded)

	// An editor may deliberately clear the content. An empty answer is not a cancellation.
	mustRespond(t, c, "editor-empty", editorID, "", false, false)
	waitFor(t, "empty editor handoff", func() bool { return len(p.controlCalls()) == 2 })
	if response := p.controlCalls()[1]; response.Value != "" || response.Cancelled {
		t.Fatalf("empty editor answer changed: %+v", response)
	}

	// The attempt ends with the answer still unacknowledged: it expires and can
	// no longer be answered.
	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
	if request := waitRequestStatus(t, c, editorID, requestExpired); request.Error == "" {
		t.Fatalf("expired dialog has no reason: %+v", request)
	}
	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "editor-late", Kind: api.RespondRequest,
		RequestID: editorID, Value: "too late",
	})
	requireCode(t, err, api.Conflict)
}

// TestRequestExpiresOnItsDeadline proves a dialog that nobody answered is closed
// by its deadline and is never mistaken for an approved request.
func TestRequestExpiresOnItsDeadline(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	p.add("extension_ui_request", map[string]any{
		"id": "ui-1", "method": "confirm", "title": "Run bash?",
	})
	requestID := attemptID + ":ui-1"
	waitRequestStatus(t, c, requestID, requestPending)

	if _, err := c.store.db.Exec(`UPDATE provider_requests SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Minute).UnixNano(), requestID); err != nil {
		t.Fatalf("move the deadline: %v", err)
	}
	if request := waitRequestStatus(t, c, requestID, requestExpired); request.Error == "" {
		t.Fatalf("expired dialog has no reason: %+v", request)
	}
	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "respond-late", Kind: api.RespondRequest,
		RequestID: requestID, Confirmed: true,
	})
	requireCode(t, err, api.Conflict)
	p.finish()
}

// TestApprovalPolicyPersistsAndIsInherited proves the policy is durable on the
// thread, the run and the provider request, and that children inherit it.
func TestApprovalPolicyPersistsAndIsInherited(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)

	created, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "create-ask", Kind: api.CreateThread,
		Title: "ask thread", ApprovalPolicy: "ask",
	})
	if err != nil {
		t.Fatalf("create thread with ask: %v", err)
	}
	thread := findThread(t, c, created.ThreadID)
	if thread.ApprovalPolicy != approvalAsk {
		t.Fatalf("thread approval policy = %q, want ask", thread.ApprovalPolicy)
	}
	sent := mustSend(t, c, "send-1", created.ThreadID, "hello")
	run := waitRunStatus(t, c, sent.RunID, api.RunRunning)
	if run.ApprovalPolicy != approvalAsk {
		t.Fatalf("run approval policy = %q, want ask", run.ApprovalPolicy)
	}
	waitFor(t, "provider request", func() bool { return p.callCount() == 1 })
	if got := p.calls[0].ApprovalPolicy; got != approvalAsk {
		t.Fatalf("provider request approval policy = %q, want ask", got)
	}

	attemptID := latestAttempt(t, c, sent.RunID).AttemptID
	delegated := dispatchOrFail(t, c, delegateCommand("delegate-1", sent.RunID, attemptID, "child work", ""))
	child := findThread(t, c, delegated.ThreadID)
	if child.ApprovalPolicy != approvalAsk {
		t.Fatalf("child thread approval policy = %q, want inherited ask", child.ApprovalPolicy)
	}
	childRun := waitRunStatus(t, c, delegated.RunID, api.RunRunning)
	if childRun.ApprovalPolicy != approvalAsk {
		t.Fatalf("child run approval policy = %q, want inherited ask", childRun.ApprovalPolicy)
	}

	// The default stays auto and an unknown policy is refused.
	defaultThread := findThread(t, c, mustCreateThread(t, c, "create-default"))
	if defaultThread.ApprovalPolicy != approvalAuto {
		t.Fatalf("default approval policy = %q, want auto", defaultThread.ApprovalPolicy)
	}
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "create-bad", Kind: api.CreateThread,
		Title: "bad", ApprovalPolicy: "always",
	})
	requireCode(t, err, api.InvalidCommand)
	p.finish()
}

func findThread(t *testing.T, c *Core, threadID string) api.Thread {
	t.Helper()
	for _, thread := range snapshotOf(t, c).Threads {
		if thread.ID == threadID {
			return thread
		}
	}
	t.Fatalf("thread %q is missing from the snapshot", threadID)
	return api.Thread{}
}

// TestSchemaTwoUpgradeAddsWorkbenchState proves an existing schema 2 database
// keeps its history and gains the new projections with historical defaults.
func TestSchemaTwoUpgradeAddsWorkbenchState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	for _, migration := range []string{migrationV1, migrationV2} {
		if _, err := raw.ExecContext(context.Background(), migration); err != nil {
			raw.Close()
			t.Fatalf("apply legacy migration: %v", err)
		}
	}
	statements := []string{
		`INSERT INTO threads (id, title, created_at) VALUES ('th-legacy', 'legacy', 100)`,
		`INSERT INTO runs (id, thread_id, attempt_id, active_attempt_id, status, input, output, error, created_at, updated_at)
			VALUES ('run-legacy', 'th-legacy', 'att-legacy', '', 'completed', 'old input', 'old output', '', 100, 100)`,
		`INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at, input)
			VALUES ('att-legacy', 'run-legacy', 1, 'completed', 100, 100, 'old input')`,
		`PRAGMA user_version = 2`,
	}
	for _, statement := range statements {
		if _, err := raw.ExecContext(context.Background(), statement); err != nil {
			raw.Close()
			t.Fatalf("seed schema 2 (%s): %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	c := openCoreAt(t, path, emitProvider("ok"))
	defer c.Close()

	snap := snapshotOf(t, c)
	if len(snap.Threads) != 1 || snap.Threads[0].ApprovalPolicy != approvalAuto {
		t.Fatalf("legacy thread did not keep the auto default: %+v", snap.Threads)
	}
	if len(snap.Runs) != 1 || snap.Runs[0].Output != "old output" || snap.Runs[0].ApprovalPolicy != approvalAuto {
		t.Fatalf("legacy run history lost: %+v", snap.Runs)
	}
	if snap.Inputs == nil || snap.Tools == nil || snap.Requests == nil {
		t.Fatal("new workbench projections are not available after the upgrade")
	}

	// The upgraded database accepts the new durable FIFO.
	receipt := mustQueue(t, c, "queue-1", "th-legacy", "new message")
	waitRunStatus(t, c, receipt.RunID, api.RunCompleted)
	waitInputStatus(t, c, receipt.InputID, inputStarted)
}

// TestControlHandoffIsRetriedWithTheSameID proves a refused handoff is retried
// with the same stable control id and never creates a second steer input.
func TestControlHandoffIsRetriedWithTheSameID(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	p.controlErr = context.DeadlineExceeded
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	receipt := mustSteer(t, c, "steer-1", threadID, sent.RunID, attemptID, "retry me")
	waitFor(t, "first refused handoff", func() bool { return len(p.controlCalls()) >= 1 })
	for _, control := range p.controlCalls() {
		if control.ID != receipt.InputID {
			t.Fatalf("control retried with a different id: %+v", control)
		}
	}
	if got := countRows(t, c, "inputs"); got != 1 {
		t.Fatalf("got %d inputs after a refused handoff, want 1", got)
	}

	// The provider accepts the retry, then acknowledges it.
	p.mu.Lock()
	p.controlErr = nil
	p.mu.Unlock()
	waitFor(t, "control retry", func() bool { return len(p.controlCalls()) >= 2 })
	p.add("control_result", provider.ControlResult{ID: receipt.InputID, Status: "delivered"})
	waitInputStatus(t, c, receipt.InputID, inputDelivered)
	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
}

// TestSteerHandoffSurvivesRestartWithTheSameID proves a crash between accepting a
// steer and handing it to the worker redelivers the same intent instead of
// losing it or creating a second input.
func TestSteerHandoffSurvivesRestartWithTheSameID(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	p.controlErr = context.DeadlineExceeded
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitRunStatus(t, first, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, first, sent.RunID).AttemptID
	receipt := mustSteer(t, first, "steer-1", threadID, sent.RunID, attemptID, "survive the crash")
	waitFor(t, "refused handoff before the crash", func() bool { return len(p.controlCalls()) >= 1 })
	crashForTest(first)

	p.mu.Lock()
	p.controlErr = nil
	p.controlCallsReset()
	p.mu.Unlock()

	restarted := openCoreAt(t, path, p)
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	waitFor(t, "control redelivery after restart", func() bool { return len(p.controlCalls()) >= 1 })
	for _, control := range p.controlCalls() {
		if control.ID != receipt.InputID {
			t.Fatalf("restart changed the control id: %+v", control)
		}
	}
	if got := countRows(t, restarted, "inputs"); got != 1 {
		t.Fatalf("restart created %d inputs, want 1", got)
	}
	if input := waitInputStatus(t, restarted, receipt.InputID, inputPending); input.Status != inputPending {
		t.Fatalf("steer changed state without control_result: %+v", input)
	}

	p.add("control_result", provider.ControlResult{ID: receipt.InputID, Status: "delivered"})
	waitInputStatus(t, restarted, receipt.InputID, inputDelivered)
	p.finish()
	waitRunStatus(t, restarted, sent.RunID, api.RunCompleted)
}

// TestRunStopInterruptsRunningTool proves a terminal attempt leaves no tool
// marked running, so the UI can never show work that is no longer happening.
func TestRunStopInterruptsRunningTool(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	p.add("tool_execution_start", map[string]any{
		"toolCallId": "call-1", "toolName": "bash", "args": map[string]any{"command": "sleep 100"},
	})
	toolID := attemptID + ":call-1"
	waitFor(t, "running tool", func() bool {
		tool, ok := findTool(snapshotOf(t, c), toolID)
		return ok && tool.Status == toolRunning
	})

	if _, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "stop-1", Kind: api.StopRun,
		ThreadID: threadID, RunID: sent.RunID,
	}); err != nil {
		t.Fatalf("stop run: %v", err)
	}
	waitFor(t, "interrupted tool", func() bool {
		tool, ok := findTool(snapshotOf(t, c), toolID)
		return ok && tool.Status == toolInterrupted
	})
}

// TestReconnectKeepsActiveDialog proves a restart re-attaches to the same attempt
// and keeps a dialog that Pi is still waiting on, so it stays answerable.
func TestReconnectKeepsActiveDialog(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	path := filepath.Join(t.TempDir(), "core.db")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitRunStatus(t, first, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, first, sent.RunID).AttemptID
	p.add("extension_ui_request", map[string]any{
		"id": "ui-1", "method": "confirm", "title": "Run bash?",
	})
	requestID := attemptID + ":ui-1"
	waitRequestStatus(t, first, requestID, requestPending)
	crashForTest(first)

	restarted := openCoreAt(t, path, p)
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if request := waitRequestStatus(t, restarted, requestID, requestPending); request.Status != requestPending {
		t.Fatalf("a re-attached attempt lost its active dialog: %+v", request)
	}
	mustRespond(t, restarted, "respond-1", requestID, "", true, false)
	waitRequestStatus(t, restarted, requestID, requestResponding)
	waitFor(t, "control handoff after reconnect", func() bool { return len(p.controlCalls()) >= 1 })
	p.add("control_result", provider.ControlResult{ID: requestID, Status: "delivered"})
	waitRequestStatus(t, restarted, requestID, requestResponded)
	p.finish()
	waitRunStatus(t, restarted, sent.RunID, api.RunCompleted)
}

func TestBoundedToolOutputHeadTailAndUTF8(t *testing.T) {
	under, err := json.Marshal("small output")
	if err != nil {
		t.Fatal(err)
	}
	if got := boundedToolOutput(under); got != "small output" {
		t.Fatalf("under-limit output = %q, want %q", got, "small output")
	}

	asciiLines := "HEADER_LINE\n" + strings.Repeat("line body 0123456789abcdef\n", 1800) + "FATAL_TAIL_TRACEBACK\n"
	rawASCII, err := json.Marshal(asciiLines)
	if err != nil {
		t.Fatal(err)
	}
	gotASCII := boundedToolOutput(rawASCII)
	if len(gotASCII) > maxToolOutputBytes {
		t.Fatalf("ASCII output len = %d > %d", len(gotASCII), maxToolOutputBytes)
	}
	if !strings.Contains(gotASCII, "HEADER_LINE") || !strings.Contains(gotASCII, "FATAL_TAIL_TRACEBACK") {
		t.Fatalf("head+tail markers missing in bounded ASCII output")
	}
	if !strings.Contains(gotASCII, "bytes truncated") || !utf8.ValidString(gotASCII) {
		t.Fatalf("invalid marker or UTF-8 in bounded ASCII output")
	}

	// Multi-byte CJK + 4-byte emoji characters around head and tail boundaries.
	cjk := "开头中文提示\n" + strings.Repeat("繁中测试🚀", 4000) + "\n结尾错误栈🔥"
	rawCJK, err := json.Marshal(cjk)
	if err != nil {
		t.Fatal(err)
	}
	gotCJK := boundedToolOutput(rawCJK)
	if len(gotCJK) > maxToolOutputBytes {
		t.Fatalf("CJK output len = %d > %d", len(gotCJK), maxToolOutputBytes)
	}
	if !utf8.ValidString(gotCJK) {
		t.Fatalf("bounded CJK/emoji output split a UTF-8 rune")
	}
	if !strings.Contains(gotCJK, "开头中文提示") || !strings.Contains(gotCJK, "结尾错误栈🔥") {
		t.Fatalf("head+tail markers missing in bounded CJK output")
	}
}

func TestCoreTelemetryCompactionRetryUsageAndLedger(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "core.db")
	telemetryDir := filepath.Join(dir, "telemetry")
	c, err := Open(context.Background(), Options{
		DBPath:       dbPath,
		Provider:     p,
		TelemetryDir: telemetryDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := c.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}
	})

	sub, err := c.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	threadID := mustCreateThread(t, c, "tel-create")
	sent := mustSend(t, c, "tel-send", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)

	waitTelemetry := func(desc string, pred func(api.RunTelemetry) bool) {
		t.Helper()
		deadline := time.After(testTimeout)
		for {
			select {
			case ev, ok := <-sub.Events:
				if !ok {
					t.Fatalf("subscription closed while waiting for %s", desc)
				}
				if ev.Type != eventRunTelemetry || ev.RunID != sent.RunID {
					continue
				}
				var payload struct {
					Telemetry api.RunTelemetry `json:"telemetry"`
				}
				if err := json.Unmarshal(ev.Data, &payload); err == nil && pred(payload.Telemetry) {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for run.telemetry (%s)", desc)
			}
		}
	}

	p.add("auto_compaction_start", map[string]any{"type": "auto_compaction_start", "reason": "threshold", "tokensBefore": 18000})
	waitTelemetry("compacting=true", func(tel api.RunTelemetry) bool { return tel.Compacting })
	if snap := snapshotOf(t, c); snap.Runs[0].Telemetry == nil || !snap.Runs[0].Telemetry.Compacting {
		t.Fatalf("snapshot missing live compacting state: %+v", snap.Runs[0].Telemetry)
	}

	p.add("auto_compaction_end", map[string]any{"type": "auto_compaction_end", "willRetry": true})
	waitTelemetry("compacting=false", func(tel api.RunTelemetry) bool { return !tel.Compacting })

	p.add("auto_retry_start", map[string]any{"type": "auto_retry_start", "attempt": 1, "maxAttempts": 3, "delayMs": 2000, "errorMessage": "rate limit"})
	waitTelemetry("retrying=true", func(tel api.RunTelemetry) bool { return tel.Retrying && tel.RetryAttempt == 1 && tel.RetryWaitSeconds == 2 })

	p.add("auto_retry_end", map[string]any{"type": "auto_retry_end", "success": true, "attempt": 1})
	waitTelemetry("retrying=false", func(tel api.RunTelemetry) bool { return !tel.Retrying })

	p.add("text_delta", "hello")
	p.add("message_end", map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "assistant",
			"usage": map[string]any{
				"input": 1200, "output": 300, "cacheRead": 800, "cacheWrite": 100, "totalTokens": 2400,
				"cost": map[string]any{"total": 0.0123},
			},
		},
	})
	waitTelemetry("usage tokens", func(tel api.RunTelemetry) bool {
		return tel.TotalTokens == 2400 && tel.LastContextTokens == 2400 && tel.CostUSD > 0.012
	})

	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)

	ledgerPath := filepath.Join(telemetryDir, "usage-ledger.json")
	if _, err := os.Stat(ledgerPath); err != nil {
		t.Fatalf("expected persisted usage ledger at %s: %v", ledgerPath, err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	reopened, err := Open(context.Background(), Options{
		DBPath:       dbPath,
		Provider:     emitProvider("unused"),
		TelemetryDir: telemetryDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("reopened Close: %v", err)
		}
	}()
	snap := snapshotOf(t, reopened)
	if len(snap.Runs) != 1 || snap.Runs[0].Telemetry == nil {
		t.Fatalf("reopened snapshot lost hydrated telemetry: %+v", snap.Runs)
	}
	if snap.Runs[0].Telemetry.TotalTokens != 2400 || snap.Runs[0].Telemetry.LastContextTokens != 2400 {
		t.Fatalf("unexpected reopened telemetry: %+v", snap.Runs[0].Telemetry)
	}
}

func TestRewindThreadTruncatesHistoryAndSession(t *testing.T) {
	var truncatedCalls []struct {
		threadID string
		keep     int
	}
	dbPath := filepath.Join(t.TempDir(), "core.db")
	c, err := Open(context.Background(), Options{
		DBPath:   dbPath,
		Provider: emitProvider("ok"),
		RewindSession: func(threadID string, keepCompletedRuns int) error {
			truncatedCalls = append(truncatedCalls, struct {
				threadID string
				keep     int
			}{threadID, keepCompletedRuns})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	ctx := context.Background()
	threadID := mustCreateThread(t, c, "rw-create")
	sent1 := mustSend(t, c, "r1", threadID, "turn 1")
	waitRunStatus(t, c, sent1.RunID, api.RunCompleted)
	sent2 := mustSend(t, c, "r2", threadID, "turn 2")
	waitRunStatus(t, c, sent2.RunID, api.RunCompleted)
	sent3 := mustSend(t, c, "r3", threadID, "turn 3")
	waitRunStatus(t, c, sent3.RunID, api.RunCompleted)

	// Rewind from turn 2: removes turn 2 and turn 3, keeps turn 1 (1 completed turn).
	rwCmd := api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "rewind-2",
		Kind:      api.RewindThread,
		ThreadID:  threadID,
		RunID:     sent2.RunID,
	}
	rw, err := c.Dispatch(ctx, rwCmd)
	if err != nil {
		t.Fatalf("rewind 2: %v", err)
	}
	if !rw.Accepted || rw.ThreadID != threadID || rw.RunID != sent2.RunID {
		t.Fatalf("unexpected rewind receipt: %+v", rw)
	}
	// Idempotent retry returns the same receipt without re-invoking RewindSession.
	rwDup, err := c.Dispatch(ctx, rwCmd)
	if err != nil {
		t.Fatalf("idempotent rewind: %v", err)
	}
	if rwDup != rw {
		t.Fatalf("idempotent rewind mismatch: %+v != %+v", rwDup, rw)
	}
	if len(truncatedCalls) != 1 || truncatedCalls[0].threadID != threadID || truncatedCalls[0].keep != 1 {
		t.Fatalf("RewindSession calls = %+v, want [{%s 1}]", truncatedCalls, threadID)
	}
	snap := snapshotOf(t, c)
	if len(snap.Runs) != 1 || snap.Runs[0].ID != sent1.RunID {
		t.Fatalf("remaining runs = %+v, want only %s", snap.Runs, sent1.RunID)
	}

	// Rewind from turn 1 removes the last remaining run -> keepCompleted = 0.
	if _, err := c.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "rewind-1",
		Kind:      api.RewindThread,
		ThreadID:  threadID,
		RunID:     sent1.RunID,
	}); err != nil {
		t.Fatalf("rewind 1: %v", err)
	}
	if len(truncatedCalls) != 2 || truncatedCalls[1].keep != 0 {
		t.Fatalf("second RewindSession calls = %+v, want keep=0", truncatedCalls)
	}
	snap2 := snapshotOf(t, c)
	if len(snap2.Runs) != 0 {
		t.Fatalf("expected 0 runs after full rewind, got %+v", snap2.Runs)
	}
}
