package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestSidebarRemoteDragNeverUpdatesSameIDLocalThread(t *testing.T) {
	_, _, hub, local, remote := twoMachineHub()
	tt := sidebarTester(hub)
	row, ok := tt.AccessByRole(ui.RoleTreeItem, "远程会话")
	if !ok {
		t.Fatal("remote thread is missing from the aggregated sidebar")
	}
	x, y := row.Bounds.X+45, row.Bounds.Y+row.Bounds.H/2
	tt.Press(x, y)
	tt.Move(x+70, y+60)
	target, ok := tt.Find("拖放至归档")
	if !ok {
		t.Fatal("dragging the remote thread did not reveal drop targets")
	}
	x, y = target.X+target.W/2, target.Y+target.H/2
	tt.Move(x, y)
	if len(local.dispatched()) != 0 || len(remote.dispatched()) != 0 {
		t.Fatal("dragging changed a thread before release")
	}
	tt.Release(x, y)
	commands := remote.dispatched()
	if len(local.dispatched()) != 0 || len(commands) != 1 || commands[0].ThreadID != "same-id" || commands[0].Archived == nil || !*commands[0].Archived {
		t.Fatalf("remote drop crossed its Core boundary: local=%+v remote=%+v", local.dispatched(), commands)
	}
}
