package desktop

import (
	"errors"
	"reflect"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestDelegateRetryPreservesParentAndOriginalInstruction(t *testing.T) {
	service := &fakeService{receiptErr: errors.New("receipt connection lost")}
	w := newWorkbench(service)
	w.applySnapshot(snapshotOneRun())
	w.widgets.manualDelivery = true
	w.setDraft("t1", "独立检查取消竞争")
	if !w.delegateDraft("t1") {
		t.Fatal("could not delegate during active parent")
	}
	first := service.dispatched()[0]
	if first.ParentRunID != "r1" || first.ParentAttemptID != "r1-attempt" || first.ResumePolicy != "manual" {
		t.Fatalf("lost parent ownership or delivery policy: %+v", first)
	}
	w.setDraft("t1", "下一条独立草稿")
	service.receiptErr = nil
	service.receipt = func(command api.Command) (api.Receipt, error) {
		return api.Receipt{CommandID: command.CommandID, Accepted: true, ThreadID: "child-thread", RunID: "child-run"}, nil
	}
	if !w.retryPending("t1") {
		t.Fatal("lost retry")
	}
	commands := service.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[0], commands[1]) {
		t.Fatalf("retry changed delegate command: %+v", commands)
	}
	if w.composerFor("t1").draft != "下一条独立草稿" {
		t.Fatal("receipt overwrote new draft")
	}
	w.applySnapshot(api.Snapshot{Sequence: 10,
		Threads: []api.Thread{{ID: "t1"}, {ID: "child-thread", ParentThreadID: "t1"}},
		Runs:    []api.Run{{ID: "r1", ThreadID: "t1", Status: api.RunWaiting}, {ID: "child-run", ThreadID: "child-thread", Status: api.RunRunning}},
	})
	if w.composerFor("t1").pending != nil {
		t.Fatal("committed child did not reconcile parent pending command")
	}
}

func TestTaskDeliveryEventsRemainDistinctAcrossGenerations(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(snapshotCompletedRun())
	first := api.Task{ID: "task", ParentRunID: "r1", Generation: 1, DeliveryStatus: api.DeliveryAcknowledged}
	second := first
	second.Generation, second.DeliveryStatus = 2, api.DeliveryDelivered
	for i, task := range []api.Task{first, second} {
		event := api.Event{Sequence: int64(4 + i), Type: "task.updated", Data: mustJSON(map[string]any{"task": task})}
		if !w.applyEvent(event) {
			t.Fatal("task event asked for resync")
		}
		if !w.applyEvent(event) {
			t.Fatal("replayed event was rejected")
		}
	}
	if len(w.tasks) != 1 || w.tasks[0].DeliveryStatus != api.DeliveryDelivered || w.tasks[0].Generation != 2 {
		t.Fatalf("follow-up did not replace the task's current generation: %+v", w.tasks)
	}
	cohort := api.Cohort{ParentRunID: "r1", Generation: 2, DeliveryMessageID: "delivery-2", Status: api.DeliveryDelivered}
	if !w.applyEvent(api.Event{Sequence: 6, Type: "cohort.delivered", Data: mustJSON(map[string]any{"cohort": cohort})}) {
		t.Fatal("cohort not folded")
	}
	if len(w.cohorts) != 1 || w.cohorts[0].DeliveryMessageID != "delivery-2" {
		t.Fatal("lost delivery identity")
	}
}

func TestAcknowledgementRetryRetainsOriginalDeliveryIdentity(t *testing.T) {
	service := &fakeService{receiptErr: errors.New("receipt lost")}
	w := newWorkbench(service)
	command := api.Command{Kind: api.AcknowledgeTask, ParentRunID: "parent", Generation: 3, DeliveryMessageID: "delivery-3"}
	w.taskAction("ack:parent:3", command)
	service.receiptErr = nil
	// A re-render cannot substitute another generation into an unconfirmed action.
	command.Generation, command.DeliveryMessageID = 4, "delivery-4"
	w.taskAction("ack:parent:3", command)
	commands := service.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[0], commands[1]) || commands[1].Generation != 3 {
		t.Fatalf("retry changed acknowledged delivery: %+v", commands)
	}
}

func TestNativeDelegateAndConfirmControlsUseCommittedIdentities(t *testing.T) {
	w, service := newViewWorkbench(run("parent", "t1", api.RunRunning, "review", "working", 0))
	tester := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tester, "消息输入框")
	tester.Type("检查持久恢复")
	mustClick(t, tester, "添加附件")
	mustClick(t, tester, "派出子任务")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.DelegateTask || commands[0].ParentRunID != "parent" {
		t.Fatalf("native delegate command: %+v", commands)
	}
	w.composerFor("t1").pending = nil
	w.runs[0].Status = api.RunWaiting
	w.tasks = []api.Task{{ID: "child", ParentRunID: "parent", ParentThreadID: "t1", Title: "恢复检查", Generation: 7,
		Status: api.RunCompleted, DeliveryStatus: api.DeliveryDelivered, DeliveryMessageID: "delivery-seven", Result: "恢复通过"}}
	w.cohorts = []api.Cohort{{ParentRunID: "parent", Generation: 7, Status: api.DeliveryDelivered, DeliveryMessageID: "delivery-seven"}}
	tester.Frame()
	mustClick(t, tester, "确认结果并继续")
	commands = service.dispatched()
	if len(commands) != 2 || commands[1].Kind != api.AcknowledgeTask || commands[1].Generation != 7 || commands[1].DeliveryMessageID != "delivery-seven" {
		t.Fatalf("native acknowledgement lost committed identity: %+v", commands)
	}
}

func TestNewThreadFreezesWorkspaceWhileSettingsChange(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/tmp/original"}
	held := holdWork(w)
	id := w.newSession()
	w.setDraft(id, "inspect project")
	w.send(id)
	w.defaults.Workspace.Path = "/tmp/other-project"
	if w.composerFor(id).pending.create.Workspace.Path != "/tmp/original" {
		t.Fatal("settings mutated an already submitted command")
	}
	held.release()
}
