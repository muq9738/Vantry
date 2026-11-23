package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// routeProvider records every request and lets a test decide what each request
// emits. It is the non-durable path used by the existing contract.
type routeProvider struct {
	fn func(ctx context.Context, req provider.Request, emit func(string) error) error

	mu    sync.Mutex
	calls []provider.Request
}

func (p *routeProvider) Run(ctx context.Context, req provider.Request, emit func(string) error) error {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	if p.fn == nil {
		return emit("ok")
	}
	return p.fn(ctx, req, emit)
}

func (p *routeProvider) requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.calls...)
}

func (p *routeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *routeProvider) requestsForRun(runID string) []provider.Request {
	var out []provider.Request
	for _, req := range p.requests() {
		if req.RunID == runID {
			out = append(out, req)
		}
	}
	return out
}

func dispatchOrFail(t *testing.T, c *Core, cmd api.Command) api.Receipt {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatalf("dispatch %s: %v", cmd.Kind, err)
	}
	return receipt
}

func delegateCommand(commandID, parentRunID, parentAttemptID, message, policy string) api.Command {
	return api.Command{
		Version:         api.ProtocolVersion,
		CommandID:       commandID,
		Kind:            api.DelegateTask,
		ParentRunID:     parentRunID,
		ParentAttemptID: parentAttemptID,
		Message:         message,
		ResumePolicy:    policy,
	}
}

func findTask(t *testing.T, c *Core, taskID string) api.Task {
	t.Helper()
	var task api.Task
	waitFor(t, "task "+taskID, func() bool {
		for _, candidate := range snapshotOf(t, c).Tasks {
			if candidate.ID == taskID {
				task = candidate
				return true
			}
		}
		return false
	})
	return task
}

func waitTaskDelivery(t *testing.T, c *Core, taskID string, status api.DeliveryStatus) api.Task {
	t.Helper()
	var task api.Task
	waitFor(t, "task "+taskID+" delivery "+string(status), func() bool {
		for _, candidate := range snapshotOf(t, c).Tasks {
			if candidate.ID != taskID {
				continue
			}
			task = candidate
			return candidate.DeliveryStatus == status
		}
		return false
	})
	return task
}

func cohortOf(t *testing.T, c *Core, parentRunID string, generation int64) api.Cohort {
	t.Helper()
	var cohort api.Cohort
	waitFor(t, "cohort", func() bool {
		for _, candidate := range snapshotOf(t, c).Cohorts {
			if candidate.ParentRunID == parentRunID && candidate.Generation == generation {
				cohort = candidate
				return true
			}
		}
		return false
	})
	return cohort
}

func attemptCount(t *testing.T, c *Core, runID string) int {
	t.Helper()
	var n int
	if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM attempts WHERE run_id = ?`, runID).Scan(&n); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	return n
}

func latestAttempt(t *testing.T, c *Core, runID string) api.Run {
	t.Helper()
	run, ok := findRun(snapshotOf(t, c), runID)
	if !ok {
		t.Fatalf("run %q is missing from the snapshot", runID)
	}
	return run
}

// TestAutoDelegateWaitsForParentThenContinues proves the core closed loop: a
// child result is accepted while the parent provider is still running, the
// parent run parks in waiting_children, and the continuation attempt starts only
// after the parent provider has settled.
func TestAutoDelegateWaitsForParentThenContinues(t *testing.T) {
	parentGate := make(chan struct{})
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "child"):
			return emit("child-output")
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)

	delegated := dispatchOrFail(t, c, delegateCommand("delegate-1", parent.RunID, parentRun.AttemptID, "child work", ""))
	if delegated.TaskID == "" || delegated.Generation != 1 || delegated.DeliveryMessageID == "" {
		t.Fatalf("delegate receipt is incomplete: %+v", delegated)
	}
	if delegated.RunID == parent.RunID || delegated.ThreadID == threadID {
		t.Fatalf("delegate did not create an independent child: %+v", delegated)
	}

	// The child finishes and its result is accepted by Core while the parent
	// provider is still running; no continuation may exist yet.
	task := waitTaskDelivery(t, c, delegated.TaskID, api.DeliveryAcknowledged)
	if task.Result != "child-output" || task.Status != api.RunCompleted {
		t.Fatalf("unexpected task result: %+v", task)
	}
	running := latestAttempt(t, c, parent.RunID)
	if running.Status != api.RunRunning {
		t.Fatalf("parent status while its provider runs = %q, want running", running.Status)
	}
	if got := attemptCount(t, c, parent.RunID); got != 1 {
		t.Fatalf("parent got %d attempts before its provider settled, want 1", got)
	}

	close(parentGate)

	waitFor(t, "parent continuation attempt", func() bool {
		return attemptCount(t, c, parent.RunID) == 2
	})
	final := waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	if final.Input != "parent message" {
		t.Fatalf("original run input was overwritten: %q", final.Input)
	}
	if final.Output != "parent-first\n\ncontinued" {
		t.Fatalf("run output lost its continuation paragraph boundary: %q", final.Output)
	}

	parentRequests := p.requestsForRun(parent.RunID)
	if len(parentRequests) != 2 {
		t.Fatalf("parent provider called %d times, want 2", len(parentRequests))
	}
	continuation := parentRequests[1]
	if !strings.Contains(continuation.Message, "child-output") ||
		!strings.Contains(continuation.Message, delegated.TaskID) ||
		!strings.Contains(continuation.Message, "generation：1") {
		t.Fatalf("continuation message is missing the delivered result: %q", continuation.Message)
	}
	if continuation.AttemptID == parentRun.AttemptID {
		t.Fatal("continuation reused the original attempt id")
	}

	cohort := cohortOf(t, c, parent.RunID, 1)
	if cohort.Status != api.DeliveryAcknowledged || cohort.DeliveryMessageID != delegated.DeliveryMessageID {
		t.Fatalf("unexpected cohort projection: %+v", cohort)
	}
	if cohort.ContinuationAttemptID != continuation.AttemptID {
		t.Fatalf("cohort continuation attempt = %q, want %q", cohort.ContinuationAttemptID, continuation.AttemptID)
	}
	var attemptInput string
	if err := c.store.db.QueryRow(`SELECT input FROM attempts WHERE id = ?`, continuation.AttemptID).
		Scan(&attemptInput); err != nil {
		t.Fatalf("read continuation attempt: %v", err)
	}
	if attemptInput != continuation.Message {
		t.Fatalf("continuation attempt stored %q, want the delivered message", attemptInput)
	}
}

// TestManualAcknowledgeControlsContinuation proves that resumePolicy=manual
// leaves the delivery to the user, that a stale generation or delivery identity
// is refused, and that a repeated acknowledgement never creates a second
// continuation.
func TestManualAcknowledgeControlsContinuation(t *testing.T) {
	parentGate := make(chan struct{})
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "child"):
			return emit("child-output")
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)

	delegated := dispatchOrFail(t, c, delegateCommand("delegate-1", parent.RunID, parentRun.AttemptID, "child work", "manual"))
	task := waitTaskDelivery(t, c, delegated.TaskID, api.DeliveryDelivered)
	if task.Result != "child-output" {
		t.Fatalf("delivered task has no result: %+v", task)
	}
	close(parentGate)

	waiting := waitRunStatus(t, c, parent.RunID, api.RunWaiting)
	if waiting.Status != api.RunWaiting {
		t.Fatalf("parent status = %q, want waiting_children", waiting.Status)
	}
	// A run parked in waiting_children still owns the thread's single slot.
	_, err := c.Dispatch(context.Background(), sendCommand("send-while-waiting", threadID, "next"))
	requireCode(t, err, api.Conflict)
	if got := attemptCount(t, c, parent.RunID); got != 1 {
		t.Fatalf("manual delivery created %d attempts before confirmation", got)
	}

	// Wrong generation, wrong delivery identity and an unknown generation are
	// all refused.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-stale-gen", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 2, DeliveryMessageID: delegated.DeliveryMessageID,
	})
	requireCode(t, err, api.NotFound)
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-stale-msg", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 1, DeliveryMessageID: delegated.DeliveryMessageID + "-stale",
	})
	requireCode(t, err, api.Conflict)

	ack := api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-1", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 1, DeliveryMessageID: delegated.DeliveryMessageID,
	}
	acked := dispatchOrFail(t, c, ack)
	if acked.Generation != 1 || acked.DeliveryMessageID != delegated.DeliveryMessageID {
		t.Fatalf("acknowledge receipt is incomplete: %+v", acked)
	}
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	waitFor(t, "manual continuation", func() bool { return attemptCount(t, c, parent.RunID) == 2 })

	// The identical retry replays its receipt, and a new command id for the same
	// delivery is accepted without a second continuation.
	if replay := dispatchOrFail(t, c, ack); replay != acked {
		t.Fatalf("acknowledge replay = %+v, want %+v", replay, acked)
	}
	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-1-again", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 1, DeliveryMessageID: delegated.DeliveryMessageID,
	})
	time.Sleep(50 * time.Millisecond)
	if got := attemptCount(t, c, parent.RunID); got != 2 {
		t.Fatalf("repeated acknowledgement produced %d attempts, want 2", got)
	}
	cohort := cohortOf(t, c, parent.RunID, 1)
	if cohort.Status != api.DeliveryAcknowledged {
		t.Fatalf("cohort status = %q, want acknowledged", cohort.Status)
	}
}

// TestParallelDelegatesShareOneGeneration proves that parallel children of the
// same parent attempt form one cohort with one delivery message and one
// continuation carrying both results.
func TestParallelDelegatesShareOneGeneration(t *testing.T) {
	parentGate := make(chan struct{})
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "child-a"):
			return emit("result-a")
		case strings.HasPrefix(req.Message, "child-b"):
			return emit("result-b")
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)

	first := dispatchOrFail(t, c, delegateCommand("delegate-a", parent.RunID, parentRun.AttemptID, "child-a", ""))
	second := dispatchOrFail(t, c, delegateCommand("delegate-b", parent.RunID, parentRun.AttemptID, "child-b", ""))
	if first.Generation != second.Generation || first.DeliveryMessageID != second.DeliveryMessageID {
		t.Fatalf("parallel delegates landed in different generations: %+v vs %+v", first, second)
	}
	// A repeated delegate command is replayed, not duplicated.
	if replay := dispatchOrFail(t, c, delegateCommand("delegate-a", parent.RunID, parentRun.AttemptID, "child-a", "")); replay != first {
		t.Fatalf("delegate replay = %+v, want %+v", replay, first)
	}

	waitTaskDelivery(t, c, first.TaskID, api.DeliveryAcknowledged)
	waitTaskDelivery(t, c, second.TaskID, api.DeliveryAcknowledged)
	if got := countRows(t, c, "tasks"); got != 2 {
		t.Fatalf("got %d tasks, want 2", got)
	}
	close(parentGate)
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	waitFor(t, "single continuation attempt", func() bool { return attemptCount(t, c, parent.RunID) == 2 })

	parentRequests := p.requestsForRun(parent.RunID)
	if len(parentRequests) != 2 {
		t.Fatalf("parent provider called %d times, want 2", len(parentRequests))
	}
	message := parentRequests[1].Message
	for _, want := range []string{"result-a", "result-b", first.TaskID, second.TaskID} {
		if !strings.Contains(message, want) {
			t.Fatalf("continuation message is missing %q: %s", want, message)
		}
	}
	if got := countRows(t, c, "attempts"); got != 4 { // parent 2 + two children
		t.Fatalf("got %d attempts, want 4", got)
	}
}

// TestFollowupStartsNextGeneration keeps old generations queryable and refuses
// to overwrite an unconsumed delivery.
func TestFollowupStartsNextGeneration(t *testing.T) {
	parentGate := make(chan struct{})
	release := make(chan struct{}, 4)
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "child"):
			return emit("child-output")
		case strings.HasPrefix(req.Message, "follow"):
			select {
			case <-release:
			case <-time.After(testTimeout):
			}
			return emit("follow-output")
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)

	delegated := dispatchOrFail(t, c, delegateCommand("delegate-1", parent.RunID, parentRun.AttemptID, "child work", "manual"))
	waitTaskDelivery(t, c, delegated.TaskID, api.DeliveryDelivered)
	close(parentGate)
	waitRunStatus(t, c, parent.RunID, api.RunWaiting)

	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-1", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 1, DeliveryMessageID: delegated.DeliveryMessageID,
	})
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	waitFor(t, "first continuation", func() bool { return attemptCount(t, c, parent.RunID) == 2 })

	// The parent already completed its first continuation; a follow-up reopens a
	// new generation instead of losing the task.
	release <- struct{}{}
	follow := dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "follow-1", Kind: api.FollowUpTask,
		TaskID: delegated.TaskID, Message: "follow work",
	})
	if follow.Generation != 2 || follow.RunID == delegated.RunID || follow.ThreadID != delegated.ThreadID {
		t.Fatalf("unexpected follow-up receipt: %+v", follow)
	}
	waitTaskDelivery(t, c, delegated.TaskID, api.DeliveryDelivered)

	// The open generation must not be overwritten by another follow-up.
	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "follow-2", Kind: api.FollowUpTask,
		TaskID: delegated.TaskID, Message: "second follow work",
	})
	requireCode(t, err, api.Conflict)

	// Acknowledging generation 2 with generation 1's delivery identity conflicts.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-gen2-wrong-msg", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 2, DeliveryMessageID: delegated.DeliveryMessageID,
	})
	requireCode(t, err, api.Conflict)

	// Re-acknowledging the already accepted generation 1 stays a no-op.
	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-1-again", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 1, DeliveryMessageID: delegated.DeliveryMessageID,
	})
	time.Sleep(50 * time.Millisecond)
	if got := attemptCount(t, c, parent.RunID); got != 2 {
		t.Fatalf("stale acknowledgement produced %d attempts, want 2", got)
	}

	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-2", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 2, DeliveryMessageID: follow.DeliveryMessageID,
	})
	waitFor(t, "second continuation", func() bool { return attemptCount(t, c, parent.RunID) == 3 })
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)

	snap := snapshotOf(t, c)
	if len(snap.Tasks) != 1 {
		t.Fatalf("got %d task rows, want 1 (one task across generations)", len(snap.Tasks))
	}
	if snap.Tasks[0].Generation != 2 || snap.Tasks[0].ChildRunID != follow.RunID {
		t.Fatalf("task projection = %+v, want generation 2 on the new run", snap.Tasks[0])
	}
	var childRuns int
	for _, run := range snap.Runs {
		if run.ThreadID == delegated.ThreadID {
			childRuns++
		}
	}
	if childRuns != 2 {
		t.Fatalf("child thread has %d runs, want both generations preserved", childRuns)
	}
	if len(snap.Cohorts) != 2 {
		t.Fatalf("got %d cohorts, want 2 generations", len(snap.Cohorts))
	}
}

// TestDisposeExcludesResult proves a disposed delivery is never added to the
// parent input and can never be acknowledged afterwards.
func TestDisposeExcludesResult(t *testing.T) {
	parentGate := make(chan struct{})
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "keep"):
			return emit("kept-result")
		case strings.HasPrefix(req.Message, "drop"):
			return emit("disposed-result")
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)

	keep := dispatchOrFail(t, c, delegateCommand("delegate-keep", parent.RunID, parentRun.AttemptID, "keep work", "manual"))
	drop := dispatchOrFail(t, c, delegateCommand("delegate-drop", parent.RunID, parentRun.AttemptID, "drop work", "manual"))
	waitTaskDelivery(t, c, keep.TaskID, api.DeliveryDelivered)
	waitTaskDelivery(t, c, drop.TaskID, api.DeliveryDelivered)

	// A disposal with the wrong generation or delivery identity is refused.
	_, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "dispose-wrong-gen", Kind: api.DisposeTask,
		TaskID: drop.TaskID, Generation: 2, DeliveryMessageID: drop.DeliveryMessageID,
	})
	requireCode(t, err, api.Conflict)
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "dispose-wrong-msg", Kind: api.DisposeTask,
		TaskID: drop.TaskID, Generation: 1, DeliveryMessageID: keep.DeliveryMessageID + "-stale",
	})
	requireCode(t, err, api.Conflict)

	disposed := dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "dispose-1", Kind: api.DisposeTask,
		TaskID: drop.TaskID, Generation: 1, DeliveryMessageID: drop.DeliveryMessageID,
	})
	if disposed.Accepted != true {
		t.Fatalf("dispose was not accepted: %+v", disposed)
	}
	task := findTask(t, c, drop.TaskID)
	if task.DeliveryStatus != api.DeliveryDisposed {
		t.Fatalf("task delivery = %q, want disposed", task.DeliveryStatus)
	}

	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-1", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 1, DeliveryMessageID: keep.DeliveryMessageID,
	})
	close(parentGate)
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	waitFor(t, "continuation", func() bool { return attemptCount(t, c, parent.RunID) == 2 })

	parentRequests := p.requestsForRun(parent.RunID)
	message := parentRequests[len(parentRequests)-1].Message
	if !strings.Contains(message, "kept-result") {
		t.Fatalf("continuation lost the accepted result: %q", message)
	}
	if strings.Contains(message, "disposed-result") {
		t.Fatalf("continue message contains a disposed result: %q", message)
	}

	// A late acknowledgement for the disposed generation is refused and the run
	// is not extended again.
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-after-dispose", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: 1, DeliveryMessageID: keep.DeliveryMessageID,
	})
	if err != nil {
		t.Fatalf("acknowledging an already accepted generation: %v", err)
	}
	if got := attemptCount(t, c, parent.RunID); got != 2 {
		t.Fatalf("late acknowledgement produced %d attempts, want 2", got)
	}

	// Disposing the only result of a fresh parent leaves a disposed cohort that
	// cannot be acknowledged at all.
	fresh := mustCreateThread(t, c, "create-2")
	parent2 := mustSend(t, c, "send-2", fresh, "parent message")
	waitRunStatus(t, c, parent2.RunID, api.RunCompleted)
	// Delegating from a recently completed run parks it instead of losing the task.
	solo := dispatchOrFail(t, c, delegateCommand("delegate-solo", parent2.RunID, "", "drop work", "manual"))
	waitTaskDelivery(t, c, solo.TaskID, api.DeliveryDelivered)
	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "dispose-solo", Kind: api.DisposeTask,
		TaskID: solo.TaskID, Generation: 1, DeliveryMessageID: solo.DeliveryMessageID,
	})
	_, err = c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "ack-solo", Kind: api.AcknowledgeTask,
		ParentRunID: parent2.RunID, Generation: 1, DeliveryMessageID: solo.DeliveryMessageID,
	})
	requireCode(t, err, api.Conflict)
}

// TestParentStopCascadesChildren proves that cancelling a parent cancels active
// descendants, disposes unconsumed deliveries and never starts a continuation.
func TestParentStopCascadesChildren(t *testing.T) {
	parentGate := make(chan struct{})
	childGate := make(chan struct{})
	var childStarted sync.WaitGroup
	childStarted.Add(1)
	var once sync.Once
	p := &routeProvider{fn: func(ctx context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "quick"):
			return emit("quick-result")
		case strings.HasPrefix(req.Message, "slow"):
			once.Do(childStarted.Done)
			select {
			case <-childGate:
				return emit("slow-result")
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)

	quick := dispatchOrFail(t, c, delegateCommand("delegate-quick", parent.RunID, parentRun.AttemptID, "quick work", "manual"))
	slow := dispatchOrFail(t, c, delegateCommand("delegate-slow", parent.RunID, parentRun.AttemptID, "slow work", "manual"))
	waitFor(t, "quick child run to finish", func() bool {
		return findTask(t, c, quick.TaskID).Status == api.RunCompleted
	})
	childStarted.Wait()
	waitRunStatus(t, c, slow.RunID, api.RunRunning)

	stopped := dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "stop-parent", Kind: api.StopRun,
		ThreadID: threadID, RunID: parent.RunID,
	})
	if !stopped.Accepted {
		t.Fatalf("stop was not accepted: %+v", stopped)
	}
	if run := waitRunStatus(t, c, parent.RunID, api.RunCancelled); run.Status != api.RunCancelled {
		t.Fatalf("parent status = %q, want cancelled", run.Status)
	}
	waitRunStatus(t, c, slow.RunID, api.RunCancelled)
	for _, taskID := range []string{quick.TaskID, slow.TaskID} {
		task := findTask(t, c, taskID)
		if task.DeliveryStatus != api.DeliveryDisposed {
			t.Fatalf("task %s delivery = %q, want disposed", taskID, task.DeliveryStatus)
		}
	}
	if cohort := cohortOf(t, c, parent.RunID, 1); cohort.Status != api.DeliveryDisposed {
		t.Fatalf("cohort status = %q, want disposed", cohort.Status)
	}

	// Releasing the wedged child must not resurrect the cancelled parent.
	close(childGate)
	close(parentGate)
	time.Sleep(150 * time.Millisecond)
	if got := attemptCount(t, c, parent.RunID); got != 1 {
		t.Fatalf("cancelled parent got %d attempts, want 1", got)
	}
	if run := latestAttempt(t, c, parent.RunID); run.Status != api.RunCancelled {
		t.Fatalf("parent was resurrected to %q", run.Status)
	}
}

// TestChildStopStillDeliversItsTerminalState proves that cancelling one child
// delivers its cancelled terminal state to a waiting parent.
func TestChildStopStillDeliversItsTerminalState(t *testing.T) {
	parentGate := make(chan struct{})
	childGate := make(chan struct{})
	var childStarted sync.WaitGroup
	childStarted.Add(1)
	var once sync.Once
	p := &routeProvider{fn: func(ctx context.Context, req provider.Request, emit func(string) error) error {
		switch {
		case strings.HasPrefix(req.Message, "parent"):
			<-parentGate
			return emit("parent-first")
		case strings.HasPrefix(req.Message, "child"):
			if req.Message == "child work" {
				once.Do(childStarted.Done)
				select {
				case <-childGate:
					return emit("unreachable")
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return emit("child-next")
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)
	delegated := dispatchOrFail(t, c, delegateCommand("delegate-1", parent.RunID, parentRun.AttemptID, "child work", ""))
	childStarted.Wait()
	waitRunStatus(t, c, delegated.RunID, api.RunRunning)

	close(parentGate)
	waitRunStatus(t, c, parent.RunID, api.RunWaiting)

	dispatchOrFail(t, c, api.Command{
		Version: api.ProtocolVersion, CommandID: "stop-child", Kind: api.StopRun,
		ThreadID: delegated.ThreadID, RunID: delegated.RunID,
	})
	waitRunStatus(t, c, delegated.RunID, api.RunCancelled)
	task := waitTaskDelivery(t, c, delegated.TaskID, api.DeliveryAcknowledged)
	if task.Status != api.RunCancelled {
		t.Fatalf("delivered task status = %q, want cancelled", task.Status)
	}

	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	waitFor(t, "continuation after child stop", func() bool { return attemptCount(t, c, parent.RunID) == 2 })
	parentRequests := p.requestsForRun(parent.RunID)
	message := parentRequests[len(parentRequests)-1].Message
	if !strings.Contains(message, "cancelled") {
		t.Fatalf("continuation does not report the cancelled child: %q", message)
	}
}

// TestParentStopRacesChildCompletion exercises the race window between a child
// terminal commit and a parent stop: the parent must end cancelled and no
// continuation may run afterwards.
func TestParentStopRacesChildCompletion(t *testing.T) {
	for iteration := 0; iteration < 6; iteration++ {
		parentGate := make(chan struct{})
		p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
			switch {
			case strings.HasPrefix(req.Message, "parent"):
				<-parentGate
				return emit("parent-first")
			case strings.HasPrefix(req.Message, "child"):
				return emit("child-output")
			default:
				return emit("continued")
			}
		}}
		c := openCoreAt(t, t.TempDir()+"/core.db", p)
		threadID := mustCreateThread(t, c, "create-1")
		parent := mustSend(t, c, "send-1", threadID, "parent message")
		waitRunStatus(t, c, parent.RunID, api.RunRunning)
		parentRun := latestAttempt(t, c, parent.RunID)
		dispatchOrFail(t, c, delegateCommand("delegate-1", parent.RunID, parentRun.AttemptID, "child work", "manual"))
		close(parentGate)
		waitRunStatus(t, c, parent.RunID, api.RunWaiting)

		stopDone := make(chan error, 1)
		go func() {
			_, err := c.Dispatch(context.Background(), api.Command{
				Version: api.ProtocolVersion, CommandID: "stop-parent", Kind: api.StopRun,
				ThreadID: threadID, RunID: parent.RunID,
			})
			stopDone <- err
		}()
		if err := <-stopDone; err != nil {
			t.Fatalf("stop parent: %v", err)
		}
		waitRunStatus(t, c, parent.RunID, api.RunCancelled)
		time.Sleep(50 * time.Millisecond)
		run := latestAttempt(t, c, parent.RunID)
		if run.Status != api.RunCancelled {
			t.Fatalf("iteration %d: parent ended %q, want cancelled", iteration, run.Status)
		}
		var active int
		if err := c.store.db.QueryRow(`SELECT COUNT(1) FROM attempts
			WHERE run_id = ? AND status IN ('queued', 'running')`, parent.RunID).Scan(&active); err != nil {
			t.Fatalf("count active attempts: %v", err)
		}
		if active != 0 {
			t.Fatalf("iteration %d: cancelled parent has %d active attempts", iteration, active)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
}

// TestTaskAndCohortEventPayloads proves the committed event contract: task and
// cohort events carry {task: ...} / {cohort: ...} and a parked run emits
// run.waiting_children with {run: ...}.
func TestTaskAndCohortEventPayloads(t *testing.T) {
	parentGate := make(chan struct{})
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		if strings.HasPrefix(req.Message, "parent") {
			<-parentGate
			return emit("parent-first")
		}
		return emit("child-output")
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "parent message")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentRun := latestAttempt(t, c, parent.RunID)
	delegated := dispatchOrFail(t, c, delegateCommand("delegate-1", parent.RunID, parentRun.AttemptID, "child work", "manual"))
	waitTaskDelivery(t, c, delegated.TaskID, api.DeliveryDelivered)
	close(parentGate)
	waitRunStatus(t, c, parent.RunID, api.RunWaiting)

	var (
		sawTask   bool
		sawCohort bool
		sawWait   bool
	)
	for _, ev := range loadAllEvents(t, c) {
		switch ev.Type {
		case eventTaskCreated, eventTaskUpdated:
			var payload struct {
				Task *api.Task `json:"task"`
			}
			if err := json.Unmarshal(ev.Data, &payload); err != nil || payload.Task == nil {
				t.Fatalf("task event %d has no {task} payload: %s", ev.Sequence, ev.Data)
			}
			if payload.Task.ID == "" || payload.Task.ParentRunID != parent.RunID {
				t.Fatalf("task payload is incomplete: %+v", payload.Task)
			}
			sawTask = true
		case eventCohortCreated, eventCohortUpdated:
			var payload struct {
				Cohort *api.Cohort `json:"cohort"`
			}
			if err := json.Unmarshal(ev.Data, &payload); err != nil || payload.Cohort == nil {
				t.Fatalf("cohort event %d has no {cohort} payload: %s", ev.Sequence, ev.Data)
			}
			if payload.Cohort.DeliveryMessageID != delegated.DeliveryMessageID {
				t.Fatalf("cohort payload lost the delivery identity: %+v", payload.Cohort)
			}
			sawCohort = true
		case eventRunWaiting:
			var payload struct {
				Run *api.Run `json:"run"`
			}
			if err := json.Unmarshal(ev.Data, &payload); err != nil || payload.Run == nil {
				t.Fatalf("waiting event %d has no {run} payload: %s", ev.Sequence, ev.Data)
			}
			if payload.Run.Status != api.RunWaiting || payload.Run.ID != parent.RunID {
				t.Fatalf("waiting payload = %+v", payload.Run)
			}
			sawWait = true
		}
	}
	if !sawTask || !sawCohort || !sawWait {
		t.Fatalf("missing events: task=%v cohort=%v waiting=%v", sawTask, sawCohort, sawWait)
	}
}

// TestNestedDelegationAwaitsItsOwnDescendants proves a delegated child that
// delegates further is not delivered to its parent until its own descendants are
// consumed and its own continuation has finished.
func TestNestedDelegationDeliversAfterItsOwnDescendants(t *testing.T) {
	parentGate := make(chan struct{})
	childGate := make(chan struct{})
	grandGate := make(chan struct{})
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		switch req.Message {
		case "level-1":
			<-parentGate
			return emit("parent-done")
		case "level-2":
			<-childGate
			return emit("child-done")
		case "level-3":
			<-grandGate
			return emit("grand-done")
		default:
			return emit("continued")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	parent := mustSend(t, c, "send-1", threadID, "level-1")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	parentAttempt := latestAttempt(t, c, parent.RunID).AttemptID

	child := dispatchOrFail(t, c, delegateCommand("delegate-child", parent.RunID, parentAttempt, "level-2", ""))
	waitRunStatus(t, c, child.RunID, api.RunRunning)
	childAttempt := latestAttempt(t, c, child.RunID).AttemptID
	grand := dispatchOrFail(t, c, delegateCommand("delegate-grand", child.RunID, childAttempt, "level-3", ""))
	waitRunStatus(t, c, grand.RunID, api.RunRunning)

	close(childGate)
	waitRunStatus(t, c, child.RunID, api.RunWaiting)
	// The child has not finished its own work, so its result must not be
	// delivered to the parent yet.
	if task := findTask(t, c, child.TaskID); task.DeliveryStatus != api.DeliveryPending || task.Status != api.RunWaiting {
		t.Fatalf("child task delivered too early: %+v", task)
	}
	if cohort := cohortOf(t, c, parent.RunID, 1); cohort.Status != api.DeliveryPending {
		t.Fatalf("parent cohort status = %q, want pending", cohort.Status)
	}

	close(grandGate)
	waitRunStatus(t, c, grand.RunID, api.RunCompleted)
	// The grandchild result continues the child, whose own continuation now runs.
	waitRunStatus(t, c, child.RunID, api.RunCompleted)
	waitFor(t, "child continuation", func() bool { return attemptCount(t, c, child.RunID) == 2 })
	waitTaskDelivery(t, c, child.TaskID, api.DeliveryAcknowledged)

	// The parent provider is still running, so its continuation waits for it.
	if got := attemptCount(t, c, parent.RunID); got != 1 {
		t.Fatalf("parent continued before its provider settled (%d attempts)", got)
	}
	close(parentGate)
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	waitFor(t, "parent continuation", func() bool { return attemptCount(t, c, parent.RunID) == 2 })
	if run := latestAttempt(t, c, parent.RunID); !strings.Contains(run.Output, "continued") {
		t.Fatalf("parent continuation did not run: %+v", run)
	}
}
