package desktop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestEnvironmentSelectionRoutesSessionToItsOwnCore(t *testing.T) {
	local := &fakeService{}
	remote := &fakeService{}
	root := newWorkbench(local)
	root.defaults = Options{ProviderID: "controlled", Model: "local-model", StatePath: filepath.Join(t.TempDir(), "desktop-state.json")}
	root.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "same-thread", Title: "Local history", ProviderID: "controlled"}}})
	root.newSession()
	sourceDraft := root.selected
	root.setDraft(sourceDraft, "run on the remote host")
	root.defaults.ConnectEnvironment = func(context.Context, string) (*EnvironmentSession, error) {
		return &EnvironmentSession{Service: remote, Info: api.EnvironmentInfo{ID: "env-mini", Name: "Mac mini", ProviderID: "controlled", DefaultModel: "remote-model", Home: "/remote/home", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/remote/project"}}}, nil
	}
	hub := newEnvironmentHub(root, context.Background())
	hub.records = append(hub.records, environmentRecord{key: "mini", connectionID: "peer-mini", label: "Mac mini"})
	hub.bind = func(target *workbench) {
		target.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "same-thread", Title: "Remote history", ProviderID: "controlled"}}})
	}
	hub.choose(root, 1, true)
	target := hub.current
	if target == root || target.environmentKey != "env-mini" {
		t.Fatal("machine choice did not bind a separate environment")
	}
	if target.threads[0].Title != "Remote history" || root.threads[0].Title != "Local history" {
		t.Fatal("equal thread IDs mixed environment histories")
	}
	if target.defaults.Model != "remote-model" || target.defaults.Workspace.Path != "/remote/project" {
		t.Fatal("local model or path leaked into remote session")
	}
	if target.composerFor(target.selected).draft != "run on the remote host" || root.composerFor(sourceDraft).draft != "" {
		t.Fatal("draft did not move with the chosen execution environment")
	}
	if !target.send(target.selected) {
		t.Fatal("remote draft was not sent")
	}
	commands := remote.dispatched()
	if len(local.dispatched()) != 0 || len(commands) != 2 || commands[0].Kind != api.CreateThread || commands[1].Kind != api.SendMessage {
		t.Fatal("remote create/send reached the wrong Core")
	}
	if commands[0].Model != "remote-model" || commands[0].Workspace.Path != "/remote/project" {
		t.Fatal("remote execution context was not frozen into create")
	}
	hub.choose(target, 0, false)
	if hub.current != root || root.threads[0].Title != "Local history" {
		t.Fatal("switching back lost local state")
	}
}

func TestRemoteThreadLinkKeepsEnvironmentScopeWithEqualThreadIDs(t *testing.T) {
	for _, environmentID := range []string{"env-studio", "peer-studio"} {
		t.Run(environmentID, func(t *testing.T) {
			root := newWorkbench(&fakeService{})
			root.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "same-thread", Title: "Local"}, {ID: "local-current", Title: "Current"}}})
			root.selectThread("local-current")
			hub := newEnvironmentHub(root, context.Background())
			remote := newWorkbench(&fakeService{})
			remote.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "same-thread", Title: "Remote"}}})
			remote.environments, remote.environmentKey = hub, "env-studio"
			hub.records = append(hub.records, environmentRecord{key: "env-studio", connectionID: "peer-studio", view: remote})
			root.replyLink("vantry-thread://same-thread?environmentId=" + environmentID)
			if hub.current != remote || remote.selected != "same-thread" || root.selected != "local-current" {
				t.Fatal("remote link navigated to a local thread with the same ID")
			}
		})
	}
}

func TestRemoteProxyShowsExecutionEnvironmentAndBlocksLocalWorkspaceTools(t *testing.T) {
	root := newWorkbench(&fakeService{})
	root.defaults.ProviderID = "pi"
	root.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "proxy", Title: "Remote task", ProviderID: "pi", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/remote/project", EnvironmentID: "peer-studio"}}}})
	root.selectThread("proxy")
	hub := newEnvironmentHub(root, context.Background())
	hub.records = append(hub.records, environmentRecord{key: "env-studio", connectionID: "peer-studio", label: "家里的 Studio"})
	tt := ui.NewTester(root.view, 1399, 820)
	if _, ok := tt.Find("远端执行环境 家里的 Studio"); !ok || !tt.HasText("远端工作区") {
		t.Fatal("proxy UI mislabeled remote execution as local")
	}
	for _, tool := range []string{"files", "git"} {
		if available, _ := root.toolAvailable(tool); available {
			t.Fatalf("remote proxy offered local %s operations", tool)
		}
	}
	// The terminal stays available, but reaches the task's machine over SSH:
	// it never opens a local shell for a remote task.
	root.openDock("terminal")
	if root.features.terminals["proxy"] != nil || root.termTab.active != terminalHostsTab {
		t.Fatalf("remote proxy opened a local shell: active=%q", root.termTab.active)
	}
	for _, label := range []string{"项目与工作区", "选择模型"} {
		control, ok := tt.Access(label)
		if !ok || !control.Disabled {
			t.Fatalf("proxy control %q can select local configuration", label)
		}
	}
}

func TestUnconnectedEnvironmentLinkNeverFallsBackToLocal(t *testing.T) {
	root := newWorkbench(&fakeService{})
	root.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "same-thread", Title: "Local"}, {ID: "current", Title: "Current"}}})
	root.selectThread("current")
	hub := newEnvironmentHub(root, context.Background())
	hub.records = append(hub.records, environmentRecord{key: "known-but-disconnected", connectionID: "disconnected"})
	for _, environmentID := range []string{"studio-ssh", "known-but-disconnected", "disconnected"} {
		root.replyLink("vantry-thread://same-thread?environmentId=" + environmentID)
		if hub.current != root || root.selected != "current" {
			t.Fatalf("unconnected environment %q fell back to local navigation", environmentID)
		}
	}
	root.replyLink("vantry-thread://same-thread")
	if root.selected != "same-thread" {
		t.Fatal("ordinary local thread links stopped working")
	}
}

func TestEnvironmentCannotMoveCommittedThreadOrLocalAttachments(t *testing.T) {
	root := newWorkbench(&fakeService{})
	root.defaults.ProviderID = "controlled"
	root.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "committed", ProviderID: "controlled"}}})
	hub := newEnvironmentHub(root, context.Background())
	hub.records = append(hub.records, environmentRecord{key: "other", connectionID: "other", label: "Other"})
	called := false
	root.defaults.ConnectEnvironment = func(context.Context, string) (*EnvironmentSession, error) { called = true; return nil, nil }
	hub.choose(root, 1, true)
	if called {
		t.Fatal("committed thread was moved to another environment")
	}
	root.newSession()
	root.composerFor(root.selected).attachments = []api.Attachment{{ID: "local-only"}}
	hub.choose(root, 1, true)
	if called {
		t.Fatal("local attachment handle was carried to another Core")
	}
}

func TestRemoteFolderPickerUsesTargetService(t *testing.T) {
	var request api.WorkbenchRequest
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		request = r
		data, _ := json.Marshal(api.EnvironmentDirectories{Path: "/remote/home", Parent: "/remote", Entries: []api.EnvironmentDirectory{{Name: "project", Path: "/remote/home/project"}}})
		return api.WorkbenchResponse{Output: string(data)}, nil
	}}
	w := newWorkbench(service)
	w.defaults.RemotePeer = true
	w.environmentInfo.Home = "/remote/home"
	localPickerCalled := false
	w.pickDirectory = func() { localPickerCalled = true }
	w.openRemoteDirectory("")
	if localPickerCalled || request.Action != "environment.directories" || request.Workspace != "/remote/home" || len(w.remoteDirectories.listing.Entries) != 1 {
		t.Fatal("remote directory selection used local state")
	}
}

func TestFolderPaletteFiltersEntersAndAddsLikeT3(t *testing.T) {
	listings := map[string]api.EnvironmentDirectories{
		"/home/me":         {Path: "/home/me", Parent: "/home", Entries: []api.EnvironmentDirectory{{Name: "Develop", Path: "/home/me/Develop"}, {Name: "Documents", Path: "/home/me/Documents"}}},
		"/home/me/Develop": {Path: "/home/me/Develop", Parent: "/home/me", Entries: []api.EnvironmentDirectory{{Name: "DemoApp", Path: "/home/me/Develop/DemoApp"}, {Name: "Vantry", Path: "/home/me/Develop/Vantry"}}},
	}
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		data, _ := json.Marshal(listings[r.Workspace])
		return api.WorkbenchResponse{Output: string(data)}, nil
	}}
	w := newWorkbench(service)
	w.defaults.RemotePeer = true
	w.environmentInfo.Home = "/home/me"
	w.newSession()
	tt := ui.NewTester(w.view, 1200, 800)
	w.openRemoteDirectory("")
	tt.Advance(time.Second)
	if w.remoteDirectories.input != "~/" || !tt.HasText("Develop") || !tt.HasText("Documents") {
		t.Fatalf("palette did not list home: %q %q", w.remoteDirectories.input, tt.Texts())
	}
	tt.Type("Dev")
	tt.Frame()
	if tt.HasText("Documents") {
		t.Fatal("filter kept a non-matching folder")
	}
	tt.Key(0, ui.KeyEnter)
	tt.Advance(time.Second)
	if w.remoteDirectories.input != "~/Develop/" || !tt.HasText("Vantry") {
		t.Fatalf("Enter did not open the folder: %q", w.remoteDirectories.input)
	}
	tt.Key(ui.Super, ui.KeyEnter)
	tt.Frame()
	if w.remoteDirectories.open || w.defaults.Workspace == nil || w.defaults.Workspace.Path != "/home/me/Develop" {
		t.Fatalf("⌘ Enter did not use the folder: %+v", w.defaults.Workspace)
	}
}
