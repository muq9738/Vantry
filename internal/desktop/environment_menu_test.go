package desktop

import (
	"context"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestMachineFilterConnectsWithoutSwitchingConversation(t *testing.T) {
	root := newWorkbench(&fakeService{})
	root.defaults.ProviderID = "controlled"
	root.newSession()
	h := newEnvironmentHub(root, context.Background())
	t.Cleanup(h.close)
	h.records = append(h.records, environmentRecord{key: "remote", connectionID: "remote", label: "Mac mini"})
	root.defaults.ConnectEnvironment = func(context.Context, string) (*EnvironmentSession, error) {
		return &EnvironmentSession{Service: &fakeService{}, Info: api.EnvironmentInfo{ID: "remote", ProviderID: "controlled"}}, nil
	}
	tt := ui.NewTester(func(c *ui.Context) { h.current.view(c) }, 1280, 820)
	mustClick(t, tt, "全部机器")
	tt.Frame()
	if err := tt.ChooseMenuItem("Mac mini · 未连接"); err != nil {
		t.Fatalf("machine menu item: %v; menu=%q", err, tt.Menu())
	}
	tt.Frame()
	if h.current != root || h.filter != "remote" || h.records[1].view == nil || h.records[1].view.environmentKey != "remote" {
		t.Fatal("machine filter must connect for listing while retaining the current conversation")
	}
}
