package desktop

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func steeringReceipt(command api.Command) (api.Receipt, error) {
	receipt := api.Receipt{CommandID: command.CommandID, Accepted: true, ThreadID: command.ThreadID, RunID: command.RunID, Sequence: 4}
	switch command.Kind {
	case api.SteerMessage:
		receipt.MessageID = "steering-message-1"
	case api.QueueMessage:
		receipt.MessageID = "queued-message-1"
	}
	return receipt, nil
}

func TestSteerEntryFreezesCommandAndQueueRemainsIndependent(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunRunning, "initial", "", 0))
	f.receipt = steeringReceipt
	w.setDraft("t1", "先检查取消边界")
	w.composerFor("t1").attachments = []api.Attachment{{ID: "a1", Name: "context.txt"}}
	w.widgets.steerMode = true
	tt := ui.NewTester(w.view, 1080, 700)
	action, ok := tt.AccessByRole(ui.RoleButton, "追加信息")
	if !ok {
		t.Fatal("steer action missing")
	}
	tt.ClickAt(action.Bounds.X+action.Bounds.W/2, action.Bounds.Y+action.Bounds.H/2)
	commands := f.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.SteerRun || commands[0].ThreadID != "t1" || commands[0].RunID != "r1" || commands[0].ParentAttemptID != "r1-attempt" || commands[0].Message != "先检查取消边界" || commands[0].Attachments[0].ID != "a1" {
		t.Fatalf("unexpected steer command: %+v", commands)
	}
	if w.composerFor("t1").steering != nil || !tt.HasText("等待 Pi 接收状态") {
		t.Fatalf("integrated steer used the legacy outbox or claimed delivery: %q", tt.Texts())
	}

	w.setDraft("t1", "当前完成后处理下一条")
	tt.Frame()
	mustClick(t, tt, "运行中发送方式")
	mustClick(t, tt, "加入队列")
	mustClick(t, tt, "加入队列")
	commands = f.dispatched()
	if len(commands) != 2 || commands[1].Kind != api.QueueMessage || commands[1].RunID != "" || commands[1].Message != "当前完成后处理下一条" {
		t.Fatalf("queue was conflated with steer: %+v", commands)
	}

	input := api.Input{ID: "steer-input-1", ThreadID: "t1", RunID: "r1", AttemptID: "r1-attempt", Kind: "steer", Message: "先检查取消边界", Status: "delivered", CreatedAt: baseTime, UpdatedAt: baseTime}
	if !w.applyEvent(workbenchEvent(4, eventInputUpdated, input)) {
		t.Fatal("input.updated event was not handled")
	}
	tt.Frame()
	if !tt.HasText("已追加到本轮") {
		t.Fatalf("delivery projection missing: %q", tt.Texts())
	}
	if len(f.dispatched()) != 2 {
		t.Fatal("delivery event caused an automatic resend or dual-contract dispatch")
	}
}

func TestSteerRetryKeepsOriginalIdentityPayloadAndLaterDraft(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunRunning, "initial", "", 0))
	f.receiptErr = errors.New("receipt lost")
	w.setDraft("t1", "original adjustment")
	w.composerFor("t1").attachments = []api.Attachment{{ID: "original-file"}}
	if !w.steerActiveRun() {
		t.Fatal("steer was not submitted")
	}
	w.setDraft("t1", "later independent draft")
	w.composerFor("t1").attachments = []api.Attachment{{ID: "later-file"}}
	f.receiptErr = nil
	f.receipt = steeringReceipt
	if !w.retrySteering("t1") {
		t.Fatal("unconfirmed steering could not be retried")
	}
	commands := f.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[0], commands[1]) {
		t.Fatalf("retry changed the frozen command: %+v", commands)
	}
	comp := w.composerFor("t1")
	if comp.draft != "later independent draft" || len(comp.attachments) != 1 || comp.attachments[0].ID != "later-file" {
		t.Fatalf("retry consumed the later draft: %+v", comp)
	}
	if w.retrySteering("t1") {
		t.Fatal("an accepted command was sent again")
	}
}

func TestSteeringSnapshotAndUnknownNeverAutomaticallyRetry(t *testing.T) {
	for _, status := range []api.SteeringStatus{api.SteeringPending, api.SteeringSending, api.SteeringAccepted, api.SteeringRejected, api.SteeringUnknown} {
		t.Run(string(status), func(t *testing.T) {
			w, f := newViewWorkbench(run("r1", "t1", api.RunRunning, "initial", "", 0))
			message := api.SteeringMessage{
				ID: "sm-" + string(status), CommandID: "client-original", ThreadID: "t1", RunID: "r1", AttemptID: "a1",
				Message: "do not clone me", Status: status, Disposition: "handled", Error: "delivery detail",
				CreatedAt: baseTime, UpdatedAt: baseTime.Add(time.Second),
			}
			w.applySnapshot(api.Snapshot{Sequence: 4, Threads: []api.Thread{thread("t1", "Thread", 0)}, Runs: w.runs, Steering: []api.SteeringMessage{message}})
			tt := ui.NewTester(w.view, 1080, 700)
			if !tt.HasText(steeringStatusText(message)) || !tt.HasText("delivery detail") {
				t.Fatalf("snapshot status not rendered: %q", tt.Texts())
			}
			if w.retrySteering("t1") || len(f.dispatched()) != 0 {
				t.Fatal("snapshot created or retried a command")
			}
			if status == api.SteeringUnknown {
				if _, ok := tt.Access("重试"); ok || !strings.Contains(steeringStatusText(message), "不会自动重发") {
					t.Fatal("unknown state offered an unsafe retry")
				}
			}
			message.Status = api.SteeringAccepted
			if !w.applyEvent(api.Event{Sequence: 5, ID: "update", Type: "message.steering", ThreadID: "t1", RunID: "r1", Data: mustJSON(map[string]any{"steering": message})}) {
				t.Fatal("steering update was rejected")
			}
			if len(w.steering) != 1 || w.steering[0].Status != api.SteeringAccepted || len(f.dispatched()) != 0 {
				t.Fatalf("event duplicated or resent steering: %+v", w.steering)
			}
		})
	}
}

func TestSteeringContributesToRecentNavigation(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(api.Snapshot{
		Threads:  []api.Thread{thread("old", "Old", 0), thread("new", "New", time.Hour)},
		Steering: []api.SteeringMessage{{ID: "sm", CommandID: "steer", ThreadID: "old", RunID: "r", Status: api.SteeringSending, CreatedAt: baseTime.Add(2 * time.Hour), UpdatedAt: baseTime.Add(3 * time.Hour)}},
	})
	if ids := w.navigationSessions(); len(ids) != 2 || ids[0] != "old" {
		t.Fatalf("steering activity did not reorder sessions: %v", ids)
	}
}

func TestSteerRequiresRunningRun(t *testing.T) {
	for _, status := range []api.RunStatus{api.RunQueued, api.RunWaiting, api.RunCompleted, api.RunFailed} {
		w, f := newViewWorkbench(run("r1", "t1", status, "initial", "", 0))
		w.setDraft("t1", "not a new run")
		if w.steerActiveRun() || len(f.dispatched()) != 0 || w.composerFor("t1").draft != "not a new run" {
			t.Fatalf("%s accepted a steer", status)
		}
	}
}
