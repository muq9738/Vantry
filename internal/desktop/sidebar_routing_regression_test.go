package desktop

import (
	"context"
	"errors"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestMachineFilterMenuDoesNotSwitchConversation(t *testing.T) {
	root, remote, hub, _, _ := twoMachineHub()
	tt := sidebarTester(hub)
	mustClick(t, tt, "机器筛选：全部机器")
	if err := tt.ChooseMenuItem("Mac mini"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if hub.filter != "peer-mini" || hub.current != root || root.selected != "same-id" || remote.selected != "same-id" {
		t.Fatalf("filtering also navigated the conversation: filter=%q local=%v", hub.filter, hub.current == root)
	}
}

func TestFilteredOfflineRetryDialsItsOwnMachine(t *testing.T) {
	root, _, hub, _, _ := twoMachineHub()
	hub.records = append(hub.records, environmentRecord{key: "offline", connectionID: "peer-offline", label: "关机的机器"})
	var calls []string
	root.defaults.ConnectEnvironment = func(_ context.Context, id string) (*EnvironmentSession, error) {
		calls = append(calls, id)
		return nil, errors.New("machine offline")
	}
	hub.setFilter("peer-offline")
	tt := sidebarTester(hub)
	mustClick(t, tt, "连接机器 关机的机器")
	if len(calls) != 1 || calls[0] != "peer-offline" || hub.current != root {
		t.Fatalf("filtered retry used an index in the unfiltered machine list: calls=%v local=%v", calls, hub.current == root)
	}
}

func TestFilteredUnavailableMachineCannotCreateLocalWork(t *testing.T) {
	root, _, hub, local, remote := twoMachineHub()
	hub.records = append(hub.records, environmentRecord{key: "offline", connectionID: "peer-offline", label: "关机的机器"})
	hub.setFilter("peer-offline")
	tt := sidebarTester(hub)
	for _, label := range []string{"新会话", "添加项目"} {
		control, ok := tt.AccessByRole(ui.RoleButton, label)
		if !ok || !control.Disabled {
			t.Fatalf("unavailable target left %q enabled: %+v", label, control)
		}
	}
	before := len(root.localOrder)
	tt.Key(ui.Cmd, ui.KeyN)
	if len(root.localOrder) != before || hub.current != root || len(local.dispatched()) != 0 || len(remote.dispatched()) != 0 {
		t.Fatal("a shortcut under an unavailable machine filter created local work")
	}
}

func TestNewThreadShortcutUsesFilteredMachine(t *testing.T) {
	root, remote, hub, _, _ := twoMachineHub()
	hub.setFilter("peer-mini")
	tt := sidebarTester(hub)
	before := len(root.localOrder)
	tt.Key(ui.Cmd, ui.KeyN)
	if hub.current != remote || !isLocalSession(remote.selected) || len(root.localOrder) != before {
		t.Fatalf("new-thread shortcut ignored its explicit machine filter: remote=%v selected=%q", hub.current == remote, remote.selected)
	}
}
