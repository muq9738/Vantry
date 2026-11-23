package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func snapshotOneRun() api.Snapshot {
	return api.Snapshot{
		Sequence: 3,
		Threads:  []api.Thread{thread("t1", "第一条线程", 0)},
		Runs: []api.Run{
			run("r1", "t1", api.RunRunning, "把标题改成蓝色", "处", 0),
		},
	}
}

func snapshotCompletedRun() api.Snapshot {
	snap := snapshotOneRun()
	snap.Runs[0].Status = api.RunCompleted
	return snap
}

func TestSnapshotThenEventsBuildTheView(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	if w.selected != "t1" {
		t.Errorf("selection after snapshot = %q, want t1", w.selected)
	}
	runs := w.runsFor("t1")
	if len(runs) != 1 || runs[0].Output != "处" {
		t.Fatalf("runs after snapshot: %+v", runs)
	}

	if !w.applyEvent(deltaEvent(4, "t1", "r1", "理中")) {
		t.Fatal("delta event should fold in")
	}
	committed := run("r1", "t1", api.RunCompleted, "把标题改成蓝色", "处", time.Minute)
	if !w.applyEvent(statusEvent(5, committed)) {
		t.Fatal("run entity should fold in")
	}

	runs = w.runsFor("t1")
	if got := runs[0].Output; got != "处理中" {
		t.Errorf("output = %q, want 处理中: a lagging entity must not cut streamed text", got)
	}
	if runs[0].Status != api.RunCompleted {
		t.Errorf("status = %q, want completed", runs[0].Status)
	}
	if w.sequence != 5 || w.appliedEvents != 2 || w.unknownEvents != 0 {
		t.Errorf("cursor %d, applied %d, unknown %d", w.sequence, w.appliedEvents, w.unknownEvents)
	}
}

func TestStaleEventIsCountedAndNotFolded(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	if !w.applyEvent(deltaEvent(2, "t1", "r1", "重复")) {
		t.Error("a stale event should still be reported as handled")
	}
	if w.runsFor("t1")[0].Output != "处" {
		t.Errorf("stale event changed output: %+v", w.runsFor("t1")[0])
	}
	if w.staleEvents != 1 || w.sequence != 3 {
		t.Errorf("stale %d, cursor %d", w.staleEvents, w.sequence)
	}
}

func TestEventsThatCannotBeFoldedAskForResync(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	unknown := api.Event{
		Sequence: 4, ID: "ev-4", Type: "workspace.updated", ThreadID: "t1",
		Data: mustJSON(map[string]any{"note": "这个版本界面还不认识"}),
	}
	if w.applyEvent(unknown) {
		t.Error("an unrecognised event must not be swallowed")
	}

	// An output delta for a run Core never sent is the same problem.
	orphan := deltaEvent(5, "t1", "missing", "字")
	if w.applyEvent(orphan) {
		t.Error("a delta for an unknown run must ask for a snapshot")
	}
	// A run in a thread that is not in the view either.
	stray := statusEvent(6, run("r9", "t-absent", api.RunRunning, "x", "", 0))
	if w.applyEvent(stray) {
		t.Error("an event for an unknown thread must ask for a snapshot")
	}
	if w.unknownEvents != 3 {
		t.Errorf("unknown %d, want 3", w.unknownEvents)
	}
	if w.sequence != 3 {
		t.Errorf("cursor moved to %d although nothing was folded in", w.sequence)
	}
}

func TestThreadEntityAppearsAndKeepsSelection(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	w.mergeThread(thread("t2", "第二条线程", time.Hour))
	if w.selected != "t1" {
		t.Errorf("selection moved to %q, want t1", w.selected)
	}
	if _, ok := w.threadByID("t2"); !ok {
		t.Error("t2 missing after its entity arrived")
	}
}

func TestSendDispatchesMessageAndClearsDraft(t *testing.T) {
	f := &fakeService{}
	w := newWorkbench(f)
	w.applySnapshot(snapshotCompletedRun())
	w.setDraft("t1", "  改成虚线  ")

	if !w.send("t1") {
		t.Fatal("send should be accepted")
	}
	got := f.dispatched()
	if len(got) != 1 {
		t.Fatalf("commands %d, want 1", len(got))
	}
	if got[0].Kind != api.SendMessage || got[0].Message != "改成虚线" || got[0].Version != api.ProtocolVersion {
		t.Errorf("command = %+v", got[0])
	}
	if strings.TrimSpace(w.composerFor("t1").draft) != "" {
		t.Errorf("draft kept after an accepted send: %q", w.composerFor("t1").draft)
	}
	if pending := w.composerFor("t1").pending; pending == nil || pending.inFlight || !pending.accepted {
		t.Errorf("accepted submission should be awaiting its Run, not in flight: %+v", pending)
	}
}

func TestUnconfirmedCommandIsSeparateFromNewDraftAndRetry(t *testing.T) {
	f := &fakeService{receiptErr: &api.Error{Code: api.NotReady, Message: "Core 还在启动"}}
	w := newWorkbench(f)
	w.applySnapshot(snapshotCompletedRun())
	w.setDraft("t1", "原始指令")

	if !w.send("t1") {
		t.Fatal("first send should be dispatched")
	}
	first := f.dispatched()[0]
	c := w.composerFor("t1")
	if c.draft != "" || c.pending == nil || c.pending.send.Message != "原始指令" {
		t.Fatalf("submitted payload was not frozen separately: %+v", c)
	}
	w.setDraft("t1", "等待期间写下的新草稿")
	if fb := c.feedback; fb.Level != noticeWarn || !strings.Contains(fb.Text, "尚未就绪") {
		t.Errorf("feedback = %+v, want an actionable retry warning", fb)
	}

	f.mu.Lock()
	f.receiptErr = nil
	f.mu.Unlock()
	if !w.retryPending("t1") {
		t.Fatal("original request should be retryable")
	}
	second := f.dispatched()[1]
	if !reflect.DeepEqual(second, first) {
		t.Errorf("retry changed the original command:\nfirst  %+v\nsecond %+v", first, second)
	}
	if c.draft != "等待期间写下的新草稿" {
		t.Errorf("receipt or retry changed the new draft: %q", c.draft)
	}
	if w.send("t1") {
		t.Error("the edited draft must not replace or resend the pending command")
	}

	// Once Core's entity for the original request is visible, a deliberate
	// send of the new draft is a new command.
	committed := run("run-from-cmd", "t1", api.RunCompleted, "原始指令", "回复", 0)
	committed.CreatedAt = c.pending.startedAt
	committed.UpdatedAt = committed.CreatedAt
	w.mergeRun(committed)
	if c.pending != nil {
		t.Fatal("the original request should merge into its committed Run")
	}
	if !w.send("t1") {
		t.Fatal("the new draft should send after the original is reconciled")
	}
	third := f.dispatched()[2]
	if third.CommandID == first.CommandID || third.Message != "等待期间写下的新草稿" {
		t.Errorf("new send must use a new ID and the new payload: %+v", third)
	}
}

func TestOtherClientMatchingTextCannotConfirmPendingCommand(t *testing.T) {
	f := &fakeService{receiptErr: &api.Error{Code: api.NotReady, Message: "receipt lost"}}
	w := newWorkbench(f)
	w.applySnapshot(snapshotCompletedRun())
	w.setDraft("t1", "相同的指令")
	w.send("t1")
	pending := w.composerFor("t1").pending
	w.setDraft("t1", "另一条新草稿")

	other := run("another-client-run", "t1", api.RunCompleted, "相同的指令", "其他客户端的回复", 0)
	other.CreatedAt = pending.startedAt.Add(time.Second)
	other.UpdatedAt = other.CreatedAt
	w.mergeRun(other)

	if got := w.composerFor("t1").pending; got != pending {
		t.Fatal("matching text and timestamps acknowledged a different client's Run")
	}
	if w.send("t1") || len(f.dispatched()) != 1 {
		t.Fatal("an unconfirmed command must keep its identity until the original receipt is recovered")
	}
	if got := w.composerFor("t1").draft; got != "另一条新草稿" {
		t.Fatalf("unrelated Run changed the new draft: %q", got)
	}
}

func TestLostSendReceiptReconcilesAfterRetryReturnsRunIdentity(t *testing.T) {
	f := &fakeService{receiptErr: &api.Error{Code: api.NotReady, Message: "receipt lost"}}
	w := newWorkbench(f)
	w.applySnapshot(snapshotCompletedRun())
	w.setDraft("t1", "回执丢失的原指令")
	w.send("t1")
	pending := w.composerFor("t1").pending
	original := f.dispatched()[0]
	w.setDraft("t1", "重连期间的新草稿")

	committed := run("r-lost-receipt", "t1", api.RunCompleted, "回执丢失的原指令", "已提交输出", 0)
	committed.CreatedAt = pending.startedAt.Add(time.Second)
	committed.UpdatedAt = committed.CreatedAt
	w.applySnapshot(api.Snapshot{
		Sequence: 4,
		Threads:  []api.Thread{thread("t1", "第一条线程", 0)},
		Runs:     []api.Run{snapshotCompletedRun().Runs[0], committed},
	})
	comp := w.composerFor("t1")
	if comp.pending != pending || comp.draft != "重连期间的新草稿" || len(f.dispatched()) != 1 {
		t.Fatal("snapshot inferred command identity or replayed an unconfirmed command")
	}

	f.mu.Lock()
	f.receiptErr = nil
	f.receipt = func(cmd api.Command) (api.Receipt, error) {
		return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: "t1", RunID: committed.ID}, nil
	}
	f.mu.Unlock()
	if !w.retryPending("t1") {
		t.Fatal("original command should remain manually retryable")
	}
	commands := f.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[1], original) {
		t.Fatalf("receipt recovery must reuse the complete original command: %+v", commands)
	}
	if comp.pending != nil || comp.draft != "重连期间的新草稿" {
		t.Fatalf("authoritative receipt did not reconcile without replacing the new draft: %+v", comp)
	}
	if entries := w.conversationFor("t1"); len(entries) != 2 || entries[1].pending != nil || entries[1].run.ID != committed.ID {
		t.Fatalf("confirmed instruction should appear once: %+v", entries)
	}
}

func TestHeldSendIsNotDispatchedTwice(t *testing.T) {
	f := &fakeService{}
	w := newWorkbench(f)
	w.applySnapshot(snapshotCompletedRun())
	held := holdWork(w)
	w.setDraft("t1", "只发一次")

	if !w.send("t1") {
		t.Fatal("first send should be accepted")
	}
	if w.send("t1") {
		t.Error("a second send while the first is in flight must be ignored")
	}
	if ids := commandIDs(w); len(ids) != 1 {
		t.Fatalf("in-flight commands %v, want one", ids)
	}
	held.release()
	if got := f.dispatched(); len(got) != 1 {
		t.Fatalf("Core saw %d commands, want 1", len(got))
	}
	if ids := commandIDs(w); len(ids) != 0 {
		t.Fatalf("in-flight commands after the receipt %v, want none", ids)
	}
	// After the original Run is visible, deliberately sending the same text as
	// a new instruction uses a new ID and is still deduplicated in flight.
	p := w.composerFor("t1").pending
	committed := run("run-from-cmd", "t1", api.RunCompleted, "只发一次", "完成", 0)
	committed.CreatedAt = p.startedAt
	committed.UpdatedAt = committed.CreatedAt
	w.mergeRun(committed)
	w.setDraft("t1", "只发一次")
	held = holdWork(w)
	if !w.send("t1") {
		t.Fatal("send after the receipt should be accepted")
	}
	if w.send("t1") {
		t.Error("repeated send while in flight must be ignored")
	}
	held.release()
	if got := f.dispatched(); len(got) != 2 || got[0].CommandID == got[1].CommandID {
		t.Errorf("commands %+v", got)
	}
}

func TestSnapshotKeepsNewDraftAndInFlightCommand(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotCompletedRun())
	held := holdWork(w)
	w.setDraft("t1", "还在发送的原文")
	w.send("t1")
	w.setDraft("t1", "回执到达前的新草稿")

	w.applySnapshot(api.Snapshot{
		Sequence: 9,
		Threads:  []api.Thread{thread("t1", "第一条线程", 0)},
		Runs:     []api.Run{run("r1", "t1", api.RunRunning, "把标题改成蓝色", "处理", 0)},
	})

	c := w.composerFor("t1")
	if c.draft != "回执到达前的新草稿" {
		t.Errorf("draft after resync = %q, want the new text kept", c.draft)
	}
	if c.pending == nil || !c.pending.inFlight || c.pending.send.Message != "还在发送的原文" {
		t.Errorf("in-flight command lost on resync: %+v", c.pending)
	}
	held.release()
	if c.draft != "回执到达前的新草稿" {
		t.Errorf("receipt cleared the newer draft: %q", c.draft)
	}
}

func TestNewSessionDefersCreateUntilFirstSend(t *testing.T) {
	f := &fakeService{}
	f.receipt = func(cmd api.Command) (api.Receipt, error) {
		switch cmd.Kind {
		case api.CreateThread:
			return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: "t9", Sequence: 4}, nil
		default:
			return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: cmd.ThreadID, RunID: "r9", Sequence: 5}, nil
		}
	}
	w := newWorkbench(f)
	w.applySnapshot(snapshotCompletedRun())

	local := w.newSession()
	if !isLocalSession(local) || w.selected != local || len(f.dispatched()) != 0 {
		t.Fatalf("new session should be local only: selected %q, commands %+v", w.selected, f.dispatched())
	}
	if again := w.newSession(); again != local {
		t.Error("repeated new-session clicks must not pile up empty drafts")
	}
	w.setDraft(local, "  第一条指令，用来派生标题  ")
	if !w.send(local) {
		t.Fatal("first send should create then send")
	}
	commands := f.dispatched()
	if len(commands) != 2 || commands[0].Kind != api.CreateThread || commands[1].Kind != api.SendMessage {
		t.Fatalf("commands = %+v, want create then send", commands)
	}
	if commands[0].Title != "第一条指令，用来派生标题" || commands[1].ThreadID != "t9" {
		t.Errorf("first payload = %+v", commands)
	}
	if w.selected != "t9" || w.composerFor("t9").draft != "" {
		t.Errorf("partial session was not migrated with its outbox: selected %q", w.selected)
	}
	w.applyEvent(threadEvent(6, thread("t9", "服务端保存的新标题", time.Hour)))
	if got := w.titleFor("t9"); got != "服务端保存的新标题" {
		t.Errorf("persisted title = %q", got)
	}
}

func TestFirstSendRetryAfterCreateSuccessUsesOnlyOriginalSend(t *testing.T) {
	failSend := true
	f := &fakeService{}
	f.receipt = func(cmd api.Command) (api.Receipt, error) {
		switch cmd.Kind {
		case api.CreateThread:
			return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: "t-partial", Sequence: 1}, nil
		case api.SendMessage:
			if failSend {
				return api.Receipt{}, &api.Error{Code: api.NotReady, Message: "receipt lost"}
			}
			return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: cmd.ThreadID, RunID: "r-partial", Sequence: 2}, nil
		}
		return api.Receipt{}, nil
	}
	w := newWorkbench(f)
	local := w.newSession()
	w.setDraft(local, "局部成功后的原始指令")
	if !w.send(local) {
		t.Fatal("first send should start")
	}
	commands := f.dispatched()
	if len(commands) != 2 || w.selected != "t-partial" {
		t.Fatalf("partial create state: selected %q, commands %+v", w.selected, commands)
	}
	original := commands[1]
	w.setDraft("t-partial", "不能混入原请求的新草稿")
	failSend = false
	if !w.retryPending("t-partial") {
		t.Fatal("original message.send should be retryable")
	}
	commands = f.dispatched()
	if len(commands) != 3 || !reflect.DeepEqual(commands[2], original) {
		t.Fatalf("retry must be only the identical message.send: %+v", commands)
	}
	if got := w.composerFor("t-partial").draft; got != "不能混入原请求的新草稿" {
		t.Errorf("new draft changed during retry: %q", got)
	}
}

func TestLostCreateReceiptRetriesOriginalCreateBeforeSend(t *testing.T) {
	createCalls := 0
	f := &fakeService{}
	f.receipt = func(cmd api.Command) (api.Receipt, error) {
		if cmd.Kind == api.CreateThread {
			createCalls++
			if createCalls == 1 {
				return api.Receipt{}, &api.Error{Code: api.NotReady, Message: "create receipt lost"}
			}
			return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: "t-created", Sequence: 1}, nil
		}
		return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: cmd.ThreadID, RunID: "r-created", Sequence: 2}, nil
	}
	w := newWorkbench(f)
	local := w.newSession()
	w.setDraft(local, "创建回执可能丢失")
	w.send(local)
	first := f.dispatched()[0]
	if w.selected != local || len(f.dispatched()) != 1 {
		t.Fatalf("an unconfirmed create must not skip to send: selected %q, commands %+v", w.selected, f.dispatched())
	}
	if !w.retryPending(local) {
		t.Fatal("original create should be retryable")
	}
	commands := f.dispatched()
	if len(commands) != 3 || !reflect.DeepEqual(commands[0], first) || !reflect.DeepEqual(commands[1], first) || commands[2].Kind != api.SendMessage {
		t.Fatalf("recovery should replay create once, then send: %+v", commands)
	}
	if createCalls != 2 {
		t.Errorf("create calls = %d, want exactly the original attempt and retry", createCalls)
	}
}

func TestMultiplePartialSessionsRemainVisibleAcrossSnapshots(t *testing.T) {
	created := 0
	f := &fakeService{}
	f.receipt = func(cmd api.Command) (api.Receipt, error) {
		if cmd.Kind == api.CreateThread {
			created++
			return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: fmt.Sprintf("t-partial-%d", created)}, nil
		}
		return api.Receipt{}, &api.Error{Code: api.NotReady, Message: "send receipt lost"}
	}
	w := newWorkbench(f)
	first := w.newSession()
	w.setDraft(first, "第一个局部成功会话")
	w.send(first)
	second := w.newSession()
	w.setDraft(second, "第二个局部成功会话")
	w.send(second)
	w.applySnapshot(api.Snapshot{Sequence: 10})

	for _, id := range []string{"t-partial-1", "t-partial-2"} {
		if !w.hasSession(id) {
			t.Errorf("partial session %q disappeared after snapshot", id)
		}
		found := false
		for _, nav := range w.navigationSessions() {
			found = found || nav == id
		}
		if !found {
			t.Errorf("partial session %q missing from navigation: %v", id, w.navigationSessions())
		}
	}
}

func TestStopActiveRun(t *testing.T) {
	f := &fakeService{}
	w := newWorkbench(f)
	w.applySnapshot(snapshotOneRun())
	held := holdWork(w)

	if !w.stopActiveRun() {
		t.Fatal("stop should be dispatched for a running run")
	}
	if !w.stoppingRuns["r1"] {
		t.Error("stop should count as in flight until its receipt")
	}
	if w.stopActiveRun() {
		t.Error("a second stop while the first is in flight must be ignored")
	}
	held.release()
	commands := f.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.StopRun || commands[0].RunID != "r1" {
		t.Fatalf("commands %+v, want one run.stop for r1", commands)
	}
	if !w.stoppingRuns["r1"] {
		t.Error("an accepted stop request is not a terminal result; keep the stopping state")
	}

	// Once Core commits the terminal Run there is nothing left to stop.
	w.applyEvent(statusEvent(4, run("r1", "t1", api.RunCancelled, "把标题改成蓝色", "处理中", time.Minute)))
	if _, active := w.activeRun("t1"); active {
		t.Error("a cancelled run is not active")
	}
	if w.stopActiveRun() {
		t.Error("stop with no active run must not dispatch")
	}
	if got := len(f.dispatched()); got != 1 {
		t.Fatalf("commands %d, want 1", got)
	}
}

func TestStopAndNaturalCompletionRaceUsesCoreTerminalState(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())
	held := holdWork(w)
	if !w.stopActiveRun() {
		t.Fatal("stop should start")
	}
	completed := run("r1", "t1", api.RunCompleted, "把标题改成蓝色", "自然完成", time.Minute)
	w.applyEvent(statusEvent(4, completed))
	held.release()
	if w.stoppingRuns["r1"] {
		t.Error("a late stop receipt must not resurrect the stopping state")
	}
	if got := w.runsFor("t1")[0].Status; got != api.RunCompleted {
		t.Errorf("status = %q, want Core's natural completion", got)
	}
}

func TestSnapshotTerminalStateClearsStoppingFeedback(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())
	w.stopActiveRun()
	snap := snapshotOneRun()
	snap.Runs[0].Status = api.RunCompleted
	w.applySnapshot(snap)
	if w.stoppingRuns["r1"] {
		t.Error("a terminal snapshot must clear the provisional stopping state")
	}
}

func TestEmptyInputIsNotSent(t *testing.T) {
	f := &fakeService{}
	w := newWorkbench(f)
	w.applySnapshot(snapshotOneRun())
	w.setDraft("t1", "   ")

	if w.send("t1") {
		t.Error("blank text must not be sent")
	}
	if len(f.dispatched()) != 0 {
		t.Errorf("commands %+v", f.dispatched())
	}
	if fb := w.composerFor("t1").feedback; fb.Level != noticeWarn {
		t.Errorf("feedback = %+v, want a warning", fb)
	}
}

func TestUnreachableCoreDoesNotClearState(t *testing.T) {
	w := newWorkbench(&fakeService{receiptErr: errors.New("connection refused")})
	w.applySnapshot(snapshotCompletedRun())
	w.setDraft("t1", "原始指令")
	w.send("t1")
	w.setDraft("t1", "断线期间的新草稿")

	c := w.composerFor("t1")
	if c.draft != "断线期间的新草稿" || c.pending == nil || c.pending.send.Message != "原始指令" {
		t.Errorf("state was not kept separately: %+v", c)
	}
	if fb := c.feedback; fb.Level != noticeError || !strings.Contains(fb.Text, "暂时不可用") {
		t.Errorf("feedback = %+v", fb)
	}
	if w.connNotice.Level != noticeError {
		t.Errorf("connection notice = %+v", w.connNotice)
	}
	if len(w.runs) != 1 {
		t.Errorf("runs = %d, want the snapshot data kept", len(w.runs))
	}
}

func TestDeltaShapesCoreMayCommit(t *testing.T) {
	for _, payload := range []string{
		`"一段字符串"`,
		`{"delta":"d"}`,
		`{"text":"d"}`,
		`{"output":"d"}`,
		`{"chunk":"d"}`,
		`{"delta":"d","output":"处理中d"}`,
	} {
		w := newWorkbench(&fakeService{})
		w.applySnapshot(snapshotOneRun())
		ev := api.Event{
			Sequence: 4, ID: "ev", Type: eventRunOutput, ThreadID: "t1", RunID: "r1",
			Data: json.RawMessage(payload),
		}
		if !w.applyEvent(ev) {
			t.Errorf("payload %s should fold in", payload)
		}
	}
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())
	odd := api.Event{
		Sequence: 4, ID: "ev", Type: eventRunOutput, ThreadID: "t1", RunID: "r1",
		Data: mustJSON(map[string]any{"part": map[string]string{"text": "d"}}),
	}
	if w.applyEvent(odd) {
		t.Error("a delta in a shape nobody knows must ask for a snapshot")
	}
}

func TestUnknownRunStatusStaysVisible(t *testing.T) {
	if got := statusLabel(api.RunStatus("thinking")); got != "状态 thinking" {
		t.Errorf("statusLabel = %q", got)
	}
	if statusLabel("") != "状态未知" {
		t.Errorf("empty status label = %q", statusLabel(""))
	}
}

func TestCommandIDsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newCommandID("send")
		if seen[id] {
			t.Fatalf("duplicate commandId %q", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "send.") {
			t.Fatalf("commandId %q should name its command", id)
		}
	}
}

func TestDescribeStaysActionableWithoutTransportDetails(t *testing.T) {
	text := describe("发送失败", &api.Error{Code: api.Conflict, Message: "same id, other content"})
	for _, want := range []string{"发送失败", "现有状态冲突"} {
		if !strings.Contains(text, want) {
			t.Errorf("describe = %q, want it to mention %q", text, want)
		}
	}
	plain := describe("同步失败", errors.New("dial unix /private/core.sock"))
	if strings.Contains(plain, "socket") || strings.Contains(plain, "/private") {
		t.Errorf("persistent feedback leaked transport details: %q", plain)
	}
}

func commandIDs(w *workbench) []string {
	var ids []string
	for id := range w.inFlightCommands {
		ids = append(ids, id)
	}
	return ids
}

// keep the imports honest when a test above stops using one of them
var _ = context.Background

// Core's real event names and payloads, as delivered with the first working
// Core: {"thread": …}, {"run": …} and run.output {delta, output}.
func TestCoreEventNamesFoldWithoutAnotherSnapshot(t *testing.T) {
	f := &fakeService{}
	w := newWorkbench(f)
	w.applySnapshot(api.Snapshot{Sequence: 1, Threads: []api.Thread{thread("t1", "第一条线程", 0)}})

	if !w.applyEvent(threadEvent(2, thread("t2", "第二条线程", time.Hour))) {
		t.Fatal("thread.created should fold in")
	}
	if !w.applyEvent(statusEvent(3, run("r1", "t2", api.RunQueued, "讲一个故事", "", time.Hour))) {
		t.Fatal("run.queued should fold in")
	}
	if !w.applyEvent(statusEvent(4, run("r1", "t2", api.RunRunning, "讲一个故事", "从前", time.Hour))) {
		t.Fatal("run.started should fold in")
	}
	if !w.applyEvent(outputEvent(5, "t2", "r1", "有一个人", "从前有一个人")) {
		t.Fatal("run.output should fold in")
	}
	if got := w.runsFor("t2")[0].Output; got != "从前有一个人" {
		t.Errorf("output = %q, want the delta appended once, not doubled with output", got)
	}
	if _, _, seen := f.counts(); len(seen) != 0 {
		t.Errorf("Subscribe cursors %v, want no resync for known events", seen)
	}
	if w.unknownEvents != 0 {
		t.Errorf("unknown events = %d, want 0", w.unknownEvents)
	}

	completed := run("r1", "t2", api.RunCompleted, "讲一个故事", "从前有一个人，故事结束了。", 2*time.Hour)
	if !w.applyEvent(statusEvent(6, completed)) {
		t.Fatal("run.completed should fold in")
	}
	got := w.runsFor("t2")[0]
	if got.Status != api.RunCompleted || got.Output != "从前有一个人，故事结束了。" {
		t.Errorf("run after run.completed = %+v", got)
	}
}

func TestRunOutputWithoutDeltaAdoptsTheCommittedText(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	if !w.applyEvent(outputEvent(4, "t1", "r1", "", "完整的已提交输出")) {
		t.Fatal("a full output with no delta should fold in")
	}
	if got := w.runsFor("t1")[0].Output; got != "完整的已提交输出" {
		t.Errorf("output = %q", got)
	}
	// A committed value that only reaches part of what already streamed in is
	// the older one and must not cut the text.
	if !w.applyEvent(outputEvent(5, "t1", "r1", "", "完整")) {
		t.Fatal("a lagging full output should still be understood")
	}
	if got := w.runsFor("t1")[0].Output; got != "完整的已提交输出" {
		t.Errorf("output = %q, want the streamed text kept", got)
	}
}

func TestRunQueuedAddsARunTheSnapshotNeverShowed(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	if !w.applyEvent(statusEvent(4, run("r7", "t1", api.RunQueued, "新问题", "", 5*time.Minute))) {
		t.Fatal("run.queued for a new run should fold in")
	}
	runs := w.runsFor("t1")
	if len(runs) != 2 || runs[1].ID != "r7" {
		t.Fatalf("runs = %+v", runs)
	}
	if _, active := w.activeRun("t1"); !active {
		t.Error("a queued run is the active one")
	}
}

func TestEventWithUnknownNameButAKnownShapeStillFolds(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	renamed := statusEvent(4, run("r1", "t1", api.RunFailed, "把标题改成蓝色", "处理中", time.Minute))
	renamed.Type = "run.finished"
	if !w.applyEvent(renamed) {
		t.Fatal("a renamed event carrying {run: …} should fold in")
	}
	if got := w.runsFor("t1")[0]; got.Status != api.RunFailed {
		t.Errorf("run = %+v", got)
	}
	if w.unknownEvents != 0 {
		t.Errorf("unknown events = %d, want 0", w.unknownEvents)
	}
}

func TestEventForUnknownThreadAsksForSnapshot(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotOneRun())

	if w.applyEvent(statusEvent(4, run("r8", "t-ghost", api.RunRunning, "x", "", 0))) {
		t.Error("a run in an unknown thread must not be shown as committed")
	}
	if len(w.runs) != 1 {
		t.Errorf("runs = %d, want the unknown run left out until a snapshot", len(w.runs))
	}
}
