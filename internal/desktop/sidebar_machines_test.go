package desktop

import (
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// twoMachineHub wires a local workbench with one already connected remote
// machine. The remote thread reuses the local thread ID on purpose: every
// interaction must resolve (machine, thread), never the bare ID.
func twoMachineHub() (*workbench, *workbench, *environmentHub, *fakeService, *fakeService) {
	local := &fakeService{}
	remote := &fakeService{}
	root := newWorkbench(local)
	root.applySnapshot(api.Snapshot{Threads: []api.Thread{
		{ID: "same-id", Title: "本地会话", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/local/app", Branch: "main"}},
		{ID: "local-only", Title: "只有本机的会话"},
	}})
	hub := newEnvironmentHub(root, context.Background())
	target := newWorkbench(remote)
	target.environments, target.environmentKey = hub, "env-mini"
	target.defaults.RemoteName = "Mac mini"
	target.applySnapshot(api.Snapshot{Threads: []api.Thread{
		{ID: "same-id", Title: "远程会话", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/remote/app", Branch: "dev"}},
		{ID: "remote-only", Title: "远程独有会话", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/local/app"}},
	}})
	hub.records = append(hub.records, environmentRecord{key: "env-mini", connectionID: "peer-mini", label: "Mac mini", view: target})
	return root, target, hub, local, remote
}

func sidebarTester(hub *environmentHub) *ui.Tester {
	return ui.NewTester(func(c *ui.Context) { hub.current.view(c) }, 1280, 820)
}

// The default sidebar lists the sessions of every connected machine, with
// each machine's groups separate; an explicit machine filter narrows the
// list without moving any thread.
func TestSidebarAggregatesMachinesByDefault(t *testing.T) {
	root, remote, hub, _, _ := twoMachineHub()
	tt := sidebarTester(hub)
	for _, title := range []string{"本地会话", "远程会话", "远程独有会话"} {
		if _, ok := tt.AccessByRole(ui.RoleTreeItem, title); !ok {
			t.Fatalf("aggregated sidebar missing %q: %q", title, tt.Texts())
		}
	}
	groups := root.sidebarGroups()
	// Same-named projects on two machines stay two rows in one flat list.
	appScopes := map[string]bool{}
	for _, g := range groups {
		for _, entry := range g.entries {
			thread, _ := entry.owner.threadByID(entry.id)
			if workspaceKey(thread.Workspace) == "/local/app" {
				appScopes[entry.scope] = true
			}
		}
	}
	if len(appScopes) != 2 {
		t.Fatalf("rows from both machines' /local/app project missing: %v in %+v", appScopes, groups)
	}
	// Rows keep their own Core's ID space: both "same-id" rows exist.
	seen := map[string]bool{}
	for _, g := range groups {
		for _, entry := range g.entries {
			seen[scopedSessionID(entry.scope, entry.id)] = true
		}
	}
	if !seen["local"+scopeSep+"same-id"] || !seen["peer-mini"+scopeSep+"same-id"] {
		t.Fatalf("equal thread IDs must list once per machine: %v", seen)
	}

	hub.setFilter("peer-mini")
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "本地会话"); ok {
		t.Fatal("machine filter still showed local threads")
	}
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "远程会话"); !ok {
		t.Fatal("machine filter hid the filtered machine's threads")
	}
	// The filter changed what is listed, never what owns or selects threads.
	if hub.current != root || remote.selected != "same-id" {
		t.Fatal("machine filter moved selection or the current environment")
	}
	hub.setFilter("")
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "本地会话"); !ok {
		t.Fatal("clearing the filter did not restore the aggregated list")
	}
}

// Choosing a row brings its machine forward and selects inside that Core.
func TestSidebarRowSelectionOpensOwningMachine(t *testing.T) {
	root, remote, hub, _, _ := twoMachineHub()
	tt := sidebarTester(hub)
	mustClick(t, tt, "远程会话")
	if hub.current != remote || remote.selected != "same-id" {
		t.Fatalf("remote row did not switch to its machine: current=%v selected=%q", hub.current == remote, remote.selected)
	}
	if root.selected != "same-id" {
		t.Fatalf("local selection moved: %q", root.selected)
	}
}

// A real double click on the row title opens the inline rename on the owning
// machine; Enter saves to that Core, Escape cancels, leaving commits.
func TestSidebarDoubleClickRenamesOwningCore(t *testing.T) {
	root, remote, hub, local, remoteSvc := twoMachineHub()
	tt := sidebarTester(hub)
	row, ok := tt.AccessByRole(ui.RoleTreeItem, "远程会话")
	if !ok {
		t.Fatal("remote row missing")
	}
	x, y := row.Bounds.X+row.Bounds.W/2, row.Bounds.Y+row.Bounds.H/2
	tt.ClickAt(x, y)
	if remote.sessions.renamingID != "" {
		t.Fatal("a single click opened a rename")
	}
	tt.ClickAt(x, y)
	tt.Frame()
	if remote.sessions.renamingID != "same-id" || root.sessions.renamingID != "" {
		t.Fatalf("double-click renamed on the wrong machine: remote=%q local=%q", remote.sessions.renamingID, root.sessions.renamingID)
	}
	if !tt.Focused("重命名会话") {
		t.Fatal("rename field did not take the focus")
	}
	if w := remote.sessions.renameText; w != "远程会话" {
		t.Fatalf("rename seed = %q", w)
	}
	for range []rune("远程会话") {
		tt.Key(0, ui.KeyBackspace)
	}
	tt.Type("远端新标题")
	tt.Key(0, ui.KeyEnter)
	commands := remoteSvc.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.UpdateThread || commands[0].ThreadID != "same-id" || commands[0].Title != "远端新标题" {
		t.Fatalf("remote rename commands = %+v", commands)
	}
	if len(local.dispatched()) != 0 {
		t.Fatal("a remote rename reached the local Core")
	}
	if remote.sessions.renamingID != "" {
		t.Fatal("rename stayed open after Enter")
	}
}

func TestSidebarRenameEscapeAndBlur(t *testing.T) {
	// Escape cancels: no command, the field is gone.
	root, remote, hub, _, remoteSvc := twoMachineHub()
	tt := sidebarTester(hub)
	row, _ := tt.AccessByRole(ui.RoleTreeItem, "远程会话")
	x, y := row.Bounds.X+row.Bounds.W/2, row.Bounds.Y+row.Bounds.H/2
	tt.ClickAt(x, y)
	tt.ClickAt(x, y)
	tt.Frame()
	tt.Type("不保存的标题")
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if remote.sessions.renamingID != "" || len(remoteSvc.dispatched()) != 0 {
		t.Fatal("Escape did not cancel the rename")
	}

	// Losing the field's focus commits the edited title.
	tt.ClickAt(x, y)
	tt.ClickAt(x, y)
	tt.Frame()
	for range []rune("远程会话") {
		tt.Key(0, ui.KeyBackspace)
	}
	tt.Type("失焦提交")
	if remote.sessions.renameText != "失焦提交" {
		t.Fatalf("rename field did not receive text: %q", remote.sessions.renameText)
	}
	mustClick(t, tt, "搜索会话") // another field takes the focus
	tt.Frame()
	if remote.sessions.renamingID != "" {
		t.Fatalf("rename still open: focused(search)=%v texts=%q", tt.Focused("搜索会话"), tt.Texts())
	}
	commands := remoteSvc.dispatched()
	if len(commands) != 1 || commands[0].Title != "失焦提交" {
		t.Fatalf("blur commit commands = %+v", commands)
	}
	if remote.sessions.renamingID != "" || root.sessions.renamingID != "" {
		t.Fatal("rename stayed open after losing focus")
	}
}

// The context menu rename of a remote row also edits in place, and local
// drafts survive machine switching and filtering.
func TestSidebarDraftsAndFilterSurviveMachineSwitch(t *testing.T) {
	root, remote, hub, _, _ := twoMachineHub()
	localDraft := root.newSession()
	root.setDraft(localDraft, "本机草稿内容")
	remoteDraft := remote.newSession()
	remote.setDraft(remoteDraft, "远端草稿内容")
	tt := sidebarTester(hub)
	mustClick(t, tt, "远程会话")
	if hub.current != remote {
		t.Fatal("selection did not switch machines")
	}
	hub.setFilter("local")
	hub.setFilter("peer-mini")
	hub.setFilter("")
	tt.Frame()
	if got := root.composerFor(localDraft).draft; got != "本机草稿内容" {
		t.Fatalf("local draft lost: %q", got)
	}
	if got := remote.composerFor(remoteDraft).draft; got != "远端草稿内容" {
		t.Fatalf("remote draft lost: %q", got)
	}
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "新会话"); !ok {
		t.Fatal("draft rows vanished after filtering")
	}
}

// An unreachable saved machine shows an offline status with a retry, while
// local sessions keep listing; a machine that drops mid-session keeps its
// already loaded threads marked offline.
func TestSidebarOfflineMachinesDoNotBlockLocal(t *testing.T) {
	root, remote, hub, _, _ := twoMachineHub()
	calls := 0
	root.defaults.ConnectEnvironment = func(context.Context, string) (*EnvironmentSession, error) {
		calls++
		return nil, errors.New("dial refused")
	}
	hub.records = append(hub.records, environmentRecord{key: "dead", connectionID: "peer-dead", label: "关机的机器"})
	hub.ensureViews()
	if calls != 1 || hub.records[2].view != nil || hub.records[2].failed == "" {
		t.Fatalf("background connect misbehaved: calls=%d record=%+v", calls, hub.records[2])
	}
	if hub.current != root {
		t.Fatal("a background connect stole the current session")
	}
	tt := sidebarTester(hub)
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "本地会话"); !ok {
		t.Fatal("an offline machine blocked the local list")
	}
	// Under "All machines", unconnected saved machines stay out of the main
	// sidebar (aligned with T3 Code) and remain accessible in the machine menu.
	if tt.HasText("关机的机器") {
		t.Fatalf("unconnected machine should not clutter the unfiltered sidebar: %q", tt.Texts())
	}
	// Narrowing the filter to that machine shows its offline status and retry.
	hub.setFilter("peer-dead")
	tt.Frame()
	if !tt.HasText("关机的机器") {
		t.Fatalf("filtered offline machine status missing: %q", tt.Texts())
	}
	mustClick(t, tt, "连接机器 关机的机器")
	if calls != 2 {
		t.Fatalf("retry did not dial again: %d", calls)
	}
	hub.setFilter("")
	tt.Frame()

	// A connected machine that loses Core keeps its loaded threads listed.
	remote.connNotice = notice{Text: "Core 同步：本地服务暂时不可用", Level: noticeError}
	tt.Frame()
	if !tt.HasText("已离线") {
		t.Fatal("offline connected machine is not marked")
	}
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "远程会话"); !ok {
		t.Fatal("offline machine dropped its already loaded sessions")
	}
}

// Same-ID threads on two machines stay two distinct sessions for every
// sidebar action, including the context menu's rename and settle.
func TestSidebarActionsNeverCrossCores(t *testing.T) {
	root, remote, hub, local, remoteSvc := twoMachineHub()
	tt := sidebarTester(hub)
	if err := tt.RightClick("远程会话"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("重命名会话"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if remote.sessions.renamingID != "same-id" || root.sessions.renamingID != "" {
		t.Fatalf("menu rename hit the wrong Core: remote=%q local=%q", remote.sessions.renamingID, root.sessions.renamingID)
	}
	for range []rune("远程会话") {
		tt.Key(0, ui.KeyBackspace)
	}
	tt.Type("仅远端")
	tt.Key(0, ui.KeyEnter)
	commands := remoteSvc.dispatched()
	if len(commands) != 1 || commands[0].Title != "仅远端" || len(local.dispatched()) != 0 {
		t.Fatalf("rename crossed machines: remote=%+v local=%+v", commands, local.dispatched())
	}
}

// Real MyGo rendering of the aggregated sidebar: default list, machine
// filter and the inline rename of a remote row. Local fixtures only.
func TestSidebarMachinesRender(t *testing.T) {
	output := os.Getenv("VANTRY_SIDEBAR_MACHINES_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_SIDEBAR_MACHINES_RENDER_DIR to export frames")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	_, remote, hub, _, _ := twoMachineHub()
	hub.records = append(hub.records, environmentRecord{key: "dead", connectionID: "peer-dead", label: "关机的机器", failed: "连接失败"})
	tt := sidebarTester(hub)
	capture := func(name string) {
		t.Helper()
		file, err := os.Create(filepath.Join(output, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, tt.Image()); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	capture("aggregated")
	hub.setFilter("peer-mini")
	tt.Frame()
	capture("filtered-remote")
	hub.setFilter("")
	tt.Frame()
	row, _ := tt.AccessByRole(ui.RoleTreeItem, "远程会话")
	x, y := row.Bounds.X+row.Bounds.W/2, row.Bounds.Y+row.Bounds.H/2
	tt.ClickAt(x, y)
	tt.ClickAt(x, y)
	tt.Frame()
	if remote.sessions.renamingID != "same-id" {
		t.Fatal("render setup: rename did not open")
	}
	capture("rename-remote")
}
