package desktop

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// v3Service records workbench requests and answers them through answer;
// unknown actions get an empty response.
type v3Service struct {
	mu       sync.Mutex
	requests []api.WorkbenchRequest
	answer   func(api.WorkbenchRequest) (api.WorkbenchResponse, error)
}

func (s *v3Service) handle(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	s.mu.Lock()
	if r.Action != "models" {
		s.requests = append(s.requests, r)
	}
	answer := s.answer
	s.mu.Unlock()
	if answer != nil {
		return answer(r)
	}
	return api.WorkbenchResponse{}, nil
}

func (s *v3Service) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.requests {
		out = append(out, r.Action)
	}
	return out
}

func (s *v3Service) find(action string) (api.WorkbenchRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.Action == action {
			return r, true
		}
	}
	return api.WorkbenchRequest{}, false
}

func (s *v3Service) reset() {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
}

func v3Workbench(t *testing.T, answer func(api.WorkbenchRequest) (api.WorkbenchResponse, error)) (*workbench, *v3Service) {
	t.Helper()
	recorder := &v3Service{answer: answer}
	service := &fakeWorkbenchService{fakeService: &fakeService{health: api.Health{Version: api.ProtocolVersion, State: "ready"}}, workbench: recorder.handle}
	w := newWorkbench(service)
	th := thread("t1", "原生工作台 · IDEA Git 与远程桌面", 0)
	th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry"}
	w.applySnapshot(api.Snapshot{Sequence: 3, Threads: []api.Thread{th}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "整理工作台", "已整理工具坞、Git 与远程桌面。", 0)}})
	w.defaults.Workspace = cloneWorkspace(th.Workspace)
	w.newTerminal = pipeTerminals(t, nil)
	return w, recorder
}

// pipeTerminals makes terminals on in-memory connections, recording the
// options each was asked for; no shell or ssh process starts.
func pipeTerminals(t *testing.T, opened *[]terminal.Options) func(terminal.Options) (*terminal.Terminal, error) {
	return func(o terminal.Options) (*terminal.Terminal, error) {
		if opened != nil {
			*opened = append(*opened, o)
		}
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
		go func() {
			buf := make([]byte, 256)
			for {
				if _, err := server.Read(buf); err != nil {
					return
				}
			}
		}()
		return terminal.New(terminal.Options{Conn: client, NoBlink: true})
	}
}

func fixtureGitState() *api.GitState {
	now := time.Now()
	return &api.GitState{
		Root: "/Projects/Vantry", Branch: "main", RemoteBranch: "origin/main", Ahead: 2, Behind: 1,
		Branches: []string{"main", "feature/dock"}, RemoteBranches: []string{"origin/main", "origin/feature/dock"},
		Stashes: []api.GitStash{{Ref: "stash@{0}", Branch: "main", Message: "WIP dock spacing"}},
		Files: []api.GitFile{
			{Path: "internal/desktop/dock_view.go", Index: "A", Worktree: " "},
			{Path: "internal/desktop/view.go", Index: " ", Worktree: "M"},
			{Path: "internal/desktop/panels.go", Index: " ", Worktree: "D"},
			{Path: "docs/notes.md", Index: "?", Worktree: "?"},
		},
		Diff: "[已暂存 / staged]\ndiff --git a/internal/desktop/dock_view.go b/internal/desktop/dock_view.go\nnew file mode 100644\n--- /dev/null\n+++ b/internal/desktop/dock_view.go\n@@ -0,0 +1,3 @@\n+package desktop\n+\n+// dockView is the right tool dock.\n" +
			"\n[未暂存 / unstaged]\ndiff --git a/internal/desktop/view.go b/internal/desktop/view.go\n--- a/internal/desktop/view.go\n+++ b/internal/desktop/view.go\n@@ -1,3 +1,3 @@\n package desktop\n-// panelView squeezed tools under the chat.\n+// dockView puts tools beside the chat.\n func x() {}\n",
		Commits: []api.GitCommit{
			{ID: "75928e6aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ShortID: "75928e6", Subject: "feat(api): IDEA Git, SSH config discovery", Author: "Dev", AuthorTime: now.Add(-2 * time.Hour), Refs: []string{"HEAD -> main", "origin/main"}, Parents: []string{"b2d46c0bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
			{ID: "b2d46c0bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ShortID: "b2d46c0", Subject: "Merge branch 'feature/search'", Author: "Dev", AuthorTime: now.Add(-26 * time.Hour), Parents: []string{"d58c63eccccccccccccccccccccccccccccccccc", "a1b2c3dddddddddddddddddddddddddddddddddd"}},
			{ID: "a1b2c3dddddddddddddddddddddddddddddddddd", ShortID: "a1b2c3d", Subject: "feat(search): rotate capabilities", Author: "Lane 2", AuthorTime: now.Add(-50 * time.Hour), Refs: []string{"feature/search"}, Parents: []string{"d58c63eccccccccccccccccccccccccccccccccc"}},
			{ID: "d58c63eccccccccccccccccccccccccccccccccc", ShortID: "d58c63e", Subject: "chore: merge orchestration lanes", Author: "Dev", AuthorTime: now.Add(-72 * time.Hour), Refs: []string{"tag: v0.1.3"}},
		},
	}
}

func TestWorkbenchDockSplitsBesideChatAndMaximizes(t *testing.T) {
	w := nativeReviewWorkbench()
	w.runs[0].Status = api.RunCompleted
	tt := ui.NewTester(w.view, 1440, 900)
	before, ok := tt.Access("消息输入框")
	if !ok {
		t.Fatal("composer missing")
	}
	mustClick(t, tt, "Git")
	tt.Advance(time.Second)
	if w.features.panel != "git" {
		t.Fatalf("panel=%q", w.features.panel)
	}
	dock, ok := tt.Access("工具坞")
	if !ok {
		t.Fatal("dock missing")
	}
	if dock.Bounds.H < 800 || dock.Bounds.W < 500 || dock.Bounds.X+dock.Bounds.W < 1439 {
		t.Fatalf("dock is not a full-height right split: %+v", dock.Bounds)
	}
	after, _ := tt.Access("消息输入框")
	if math.Abs(float64(after.Bounds.H-before.Bounds.H)) > 0.5 || after.Bounds.X+after.Bounds.W > dock.Bounds.X {
		t.Fatalf("composer squished or under the dock: before=%+v after=%+v dock=%+v", before.Bounds, after.Bounds, dock.Bounds)
	}
	if _, ok := tt.Access("会话内容"); !ok {
		t.Fatal("the conversation must stay visible beside the dock")
	}
	mustClick(t, tt, "最大化工具坞")
	tt.Advance(time.Second)
	full, _ := tt.Access("工具坞")
	if !w.dock.maximized || full.Bounds.W < 1100 {
		t.Fatalf("maximize did not fill the stage: %+v", full.Bounds)
	}
	if _, ok := tt.Access("会话内容"); ok {
		t.Fatal("maximized dock must take the whole stage")
	}
	mustClick(t, tt, "还原工具坞")
	mustClick(t, tt, "工具页 终端")
	mustClick(t, tt, "远程主机列表")
	if w.features.panel != "terminal" || w.termTab.active != terminalHostsTab {
		t.Fatalf("tab switch panel=%q active=%q", w.features.panel, w.termTab.active)
	}
	mustClick(t, tt, "工具页 插件")
	if w.features.panel != "plugins" {
		t.Fatalf("tab switch panel=%q", w.features.panel)
	}
	mustClick(t, tt, "工具页 插件")
	if w.features.panel != "plugins" {
		t.Fatal("a tab click must never close the dock")
	}
	mustClick(t, tt, "关闭工具面板")
	if w.features.panel != "" || !tt.Focused("消息输入框") {
		t.Fatal("close did not return to the composer")
	}

	// Narrow stages: a full-height sheet in place of the timeline.
	tt.SetSize(640, 600)
	w.openDock("git")
	tt.Frame()
	tt.Advance(time.Second)
	sheet, ok := tt.Access("工具坞")
	composer, _ := tt.Access("消息输入框")
	if !ok || sheet.Bounds.W < 600 || sheet.Bounds.Y > 60 || sheet.Bounds.Y+sheet.Bounds.H > composer.Bounds.Y+1 {
		t.Fatalf("narrow sheet: %+v composer %+v", sheet.Bounds, composer.Bounds)
	}
	if _, ok := tt.Access("最大化工具坞"); ok {
		t.Fatal("the sheet already fills the stage; no maximize")
	}
}

func TestDockResizeHandleChangesWidth(t *testing.T) {
	w := nativeReviewWorkbench()
	w.runs[0].Status = api.RunCompleted
	w.features.panel = "plugins"
	tt := ui.NewTester(w.view, 1440, 900)
	dock, _ := tt.Access("工具坞")
	handle, ok := tt.Access("调整工具坞宽度")
	if !ok || math.Abs(float64(dock.Bounds.W-440)) > 1 {
		t.Fatalf("default plugins dock: %+v handle=%v", dock.Bounds, ok)
	}
	x, y := handle.Bounds.X+handle.Bounds.W/2, handle.Bounds.Y+200
	tt.Press(x, y)
	for step := 1; step <= 6; step++ {
		tt.Move(x-float32(step*20), y)
		tt.Frame()
	}
	tt.Release(x-120, y)
	tt.Advance(time.Second)
	wider, _ := tt.Access("工具坞")
	if wider.Bounds.W < 520 || w.dock.widths["plugins"] < 520 {
		t.Fatalf("drag did not widen the dock: %+v widths=%v", wider.Bounds, w.dock.widths)
	}
}

func TestGitOpensForChosenProjectBeforeFirstMessage(t *testing.T) {
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action == "git.status" {
			return api.WorkbenchResponse{Git: fixtureGitState()}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	local := w.newSession()
	if w.selected != local || w.hasThread(local) {
		t.Fatal("fixture should select a draft")
	}
	if ok, why := w.toolAvailable("git"); !ok {
		t.Fatalf("git locked before the first message: %s", why)
	}
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "Git")
	request, ok := service.find("git.status")
	if !ok || request.ThreadID != "" || request.Workspace != "/Projects/Vantry" {
		t.Fatalf("thread-less status must address the project folder: %+v", request)
	}
	tt.Frame()
	if !tt.HasText("dock_view.go") {
		t.Fatalf("changes not listed: %q", tt.Texts())
	}
}

func TestSSHHubListsConfigHostsSearchAndActions(t *testing.T) {
	reachable := true
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case actionSSHHosts:
			return api.WorkbenchResponse{SSHHosts: []api.SSHHostInfo{
				{Alias: "devbox", Aliases: []string{"devbox-win", "shanghai"}, User: "demo", HasIdentityFile: true, SourceFile: "~/.ssh/config"},
				{Alias: "tokyo-a", User: "demo", Port: 2222, ProxyJump: "devbox"},
				{Alias: "mini", RemoteSocket: "/Users/demo/.config/Vantry/core.sock", HasHerdr: true},
			}}, nil
		case actionSSHProbe:
			return api.WorkbenchResponse{SSHHosts: []api.SSHHostInfo{{Alias: r.Name, Reachable: &reachable, LatencyMs: 24}}}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	var opened []terminal.Options
	w.newTerminal = pipeTerminals(t, &opened)
	tt := ui.NewTester(w.view, 1440, 900)
	w.openDock("remote") // the SSH hosts, inside the terminal
	tt.Frame()
	for _, text := range []string{"devbox", "tokyo-a", "mini", "~/.ssh/config", "demo@devbox:22", "demo@tokyo-a:2222"} {
		if !tt.HasText(text) {
			t.Fatalf("SSH hub missing %q: %q", text, tt.Texts())
		}
	}
	if _, ok := service.find(actionSSHHosts); !ok {
		t.Fatal("opening the hub must read ~/.ssh/config through ssh.hosts")
	}
	mustClick(t, tt, "主机 devbox")
	tt.Frame()
	if !tt.HasText("devbox-win") || !tt.HasText("shanghai") {
		t.Fatalf("the aliases of an opened host are missing: %q", tt.Texts())
	}
	// 检测连接 lives behind the host's menu, which is a native one: test
	// what it runs.
	w.probeSSHEntry(w.sshEntry("devbox"))
	tt.Frame()
	if probe, ok := service.find(actionSSHProbe); !ok || probe.Name != "devbox" || !tt.HasText("24 ms") {
		t.Fatalf("probe=%+v texts=%q", probe, tt.Texts())
	}
	mustClick(t, tt, "搜索主机")
	tt.Type("tok")
	tt.Frame()
	if _, ok := tt.Access("SSH 终端 devbox"); ok || !tt.HasText("tokyo-a") {
		t.Fatal("search did not filter the hosts")
	}
	w.ssh.search = ""
	tt.Frame()
	mustClick(t, tt, "打开终端 devbox")
	if len(opened) != 1 || !reflect.DeepEqual(opened[0].Command, sshEntry{name: "devbox", target: "devbox", key: "devbox"}.sshArgv()) || w.termTab.active != "devbox" {
		t.Fatalf("ssh terminal: %+v active=%q", opened, w.termTab.active)
	}
	// A terminal opens in the dock's terminal tab, where each session has a
	// tab of its own.
	tt.Frame()
	if w.features.panel != "terminal" {
		t.Fatalf("opening a host terminal must show the terminal tab, panel=%q", w.features.panel)
	}
	if _, ok := tt.Access("SSH devbox"); !ok {
		t.Fatal("embedded ssh terminal not shown")
	}
	mustClick(t, tt, "远程主机列表")
	tt.Frame()
	service.reset()
	mustClick(t, tt, "主机 tokyo-a")
	tt.Frame()
	mustClick(t, tt, "连接远端工作台 tokyo-a")
	if probe, ok := service.find(actionSSHProbe); !ok || probe.Name != "tokyo-a" {
		t.Fatalf("remote Vantry must probe for the Core socket first: %+v", service.requests)
	}
	tt.Frame()
	if !w.ssh.advancedOpen || w.features.remoteHost != "tokyo-a" {
		t.Fatal("a host whose probe finds no socket must open the advanced form")
	}
	if _, ok := tt.Access("远端 Vantry 地址"); !ok {
		tt.Frame()
		if _, ok := tt.Access("远端 Vantry 地址"); !ok {
			t.Fatal("advanced form keeps the address field")
		}
	}
	if _, ok := tt.Access("远程桌面 mini"); ok {
		t.Fatal("SSH hub must leave remote desktop to the plugin")
	}
	w.closeSSHTerminals()
}

func TestIdeaGitCommitPageGroupsRollbackAndCommitPush(t *testing.T) {
	state := fixtureGitState()
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{Git: state}, nil
	})
	w.features.panel = "git"
	w.features.git = map[string]*api.GitState{"t1": state}
	tt := ui.NewTester(w.view, 1440, 900)
	for _, group := range []string{"已暂存 (1)", "更改 (2)", "未版本控制 (1)"} {
		if _, ok := tt.Access(group); !ok {
			t.Fatalf("group %q missing", group)
		}
	}
	for _, text := range []string{"dock_view.go", "internal/desktop", "notes.md", "↑2", "↓1", "Pop 1"} {
		if !tt.HasText(text) {
			t.Fatalf("missing %q: %q", text, tt.Texts())
		}
	}
	// The first change previews at once (the diff header names it).
	if !tt.HasText("+3") || !tt.HasText("@@ -0,0 +1,3 @@") {
		t.Fatalf("diff preview missing: %q", tt.Texts())
	}
	mustClick(t, tt, "internal/desktop/view.go")
	tt.Frame()
	if !tt.HasText("@@ -1,3 +1,3 @@") || w.gitView.selected["t1"] != "false:internal/desktop/view.go" {
		t.Fatal("selected file diff missing")
	}
	mustClick(t, tt, "回滚 internal/desktop/view.go")
	tt.Frame()
	service.reset()
	mustClick(t, tt, "确认回滚 internal/desktop/view.go")
	if r, ok := service.find(actionGitDiscard); !ok || !reflect.DeepEqual(r.Paths, []string{"internal/desktop/view.go"}) || r.ThreadID != "t1" {
		t.Fatalf("rollback=%+v", service.requests)
	}
	tt.Frame()
	mustClick(t, tt, "选择提交 docs/notes.md")
	if !w.gitView.checked["t1"]["docs/notes.md"] {
		t.Fatal("checkbox did not include the file")
	}
	mustClick(t, tt, "提交说明")
	tt.Type("feat(desktop): right workbench dock")
	mustClick(t, tt, "修正上次提交")
	if !w.gitView.amend {
		t.Fatal("amend not set")
	}
	service.reset()
	mustClick(t, tt, "提交并推送")
	actions := service.actions()
	if len(actions) < 2 || actions[0] != "git.commit" || actions[1] != actionGitPush {
		t.Fatalf("commit & push sequence=%v", actions)
	}
	commit, _ := service.find("git.commit")
	if !commit.Amend || commit.Message != "feat(desktop): right workbench dock" || !reflect.DeepEqual(commit.Paths, []string{"docs/notes.md", "internal/desktop/dock_view.go"}) {
		t.Fatalf("commit=%+v", commit)
	}
	if w.gitView.amend || w.gitView.messages["t1"] != "" {
		t.Fatal("a confirmed commit must clear the box")
	}
}

func TestIdeaGitBranchPopoverChecksOutAndCreates(t *testing.T) {
	state := fixtureGitState()
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{Git: state}, nil
	})
	w.features.panel = "git"
	w.features.git = map[string]*api.GitState{"t1": state}
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "分支：main")
	tt.Frame()
	for _, text := range []string{"本地分支", "远程分支", "origin/feature/dock"} {
		if !tt.HasText(text) {
			t.Fatalf("branch popover missing %q", text)
		}
	}
	mustClick(t, tt, "签出 feature/dock")
	if r, ok := service.find(actionGitCheckout); !ok || r.Branch != "feature/dock" {
		t.Fatalf("checkout=%+v", service.requests)
	}
	mustClick(t, tt, "分支：main")
	tt.Frame()
	mustClick(t, tt, "新分支名")
	tt.Type("feature/ssh-hub")
	mustClick(t, tt, "新建分支")
	if r, ok := service.find(actionGitBranchCreate); !ok || r.Name != "feature/ssh-hub" {
		t.Fatalf("create=%+v", service.requests)
	}
	if w.gitCreateBranch("bad name") {
		t.Fatal("invalid branch names must not reach Core")
	}
}

func TestCommitGraphLanes(t *testing.T) {
	rows := commitGraph(fixtureGitState().Commits)
	if len(rows) != 4 {
		t.Fatal(len(rows))
	}
	merge := rows[1]
	if merge.col != 0 || len(merge.out) != 2 || merge.out[0] != 0 || merge.out[1] != 1 {
		t.Fatalf("merge row=%+v", merge)
	}
	side := rows[2]
	if side.col != 1 || !reflect.DeepEqual(side.through, []int{0}) || !reflect.DeepEqual(side.out, []int{0}) {
		t.Fatalf("side branch must join lane 0: %+v", side)
	}
	root := rows[3]
	if root.col != 0 || !reflect.DeepEqual(root.in, []int{0}) || len(root.out) != 0 || root.lanes != 1 {
		t.Fatalf("root row: %+v", root)
	}
}

func TestIdeaGitLogSearchAndCommitDetail(t *testing.T) {
	state := fixtureGitState()
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case actionGitLog:
			commits := state.Commits
			if r.Message != "" {
				commits = commits[2:3]
			}
			return api.WorkbenchResponse{Git: &api.GitState{Commits: commits}}, nil
		case actionGitCommitDetail:
			detail, _ := json.Marshal(map[string]any{
				"commit": state.Commits[2],
				"files":  []api.GitFile{{Path: "internal/app/search.go", Index: "M", Worktree: " "}},
				"diff":   "diff --git a/internal/app/search.go b/internal/app/search.go\n--- a/internal/app/search.go\n+++ b/internal/app/search.go\n@@ -1 +1 @@\n-old capability order\n+rotated capability order\n",
			})
			return api.WorkbenchResponse{Output: string(detail)}, nil
		}
		return api.WorkbenchResponse{Git: state}, nil
	})
	w.features.panel = "git"
	w.features.git = map[string]*api.GitState{"t1": state}
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "Git 日志页")
	tt.Frame()
	if _, ok := service.find(actionGitLog); !ok {
		t.Fatal("log page must read git.log")
	}
	for _, text := range []string{"HEAD → main", "origin/main", "v0.1.3", "75928e6", "Merge branch 'feature/search'"} {
		if !tt.HasText(text) {
			t.Fatalf("log missing %q: %q", text, tt.Texts())
		}
	}
	mustClick(t, tt, "feat(search): rotate capabilities · a1b2c3d")
	tt.Frame()
	if r, ok := service.find(actionGitCommitDetail); !ok || r.Name != state.Commits[2].ID {
		t.Fatalf("detail request=%+v", service.requests)
	}
	if !tt.HasText("search.go") || !tt.HasText("@@ -1,1 +1,1 @@") || !tt.HasText("Lane 2") {
		t.Fatalf("detail not shown: %q", tt.Texts())
	}
	mustClick(t, tt, "筛选提交")
	tt.Type("rotate")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if r := service.requests[len(service.requests)-1]; r.Action != actionGitLog || r.Message != "rotate" {
		t.Fatalf("search request=%+v", r)
	}
	if tt.HasText("Merge branch 'feature/search'") {
		t.Fatal("search did not narrow the log")
	}
}

func pluginFixture() []api.PluginInfo {
	return []api.PluginInfo{
		{ID: "music", Name: "MyGo Music", Version: "1.2.0", Description: "在 Vantry 里听歌、管理歌单", Repo: "egoist/mygo-music", HasUpdate: true, LatestVersion: "1.3.0",
			NeedsProcessGrant: true, Settings: []api.PluginSettingSpec{
				{Key: "source", Label: "音源", Type: "select", Value: "netease", Options: []api.PluginSettingOption{{Value: "netease", Label: "网易云"}, {Value: "qq", Label: "QQ 音乐"}}},
				{Key: "lyrics", Label: "显示歌词", Type: "boolean", Value: "false"},
				{Key: "cookie", Label: "账号备注", Type: "string", Default: "默认"},
			}, Navigation: []api.PluginNavItem{{Title: "音乐库", Page: "library"}}},
		{ID: "notes", Name: "项目笔记", Description: "读取当前草稿并整理为本地笔记", Builtin: true},
	}
}

func TestPluginCenterInstallUpdateSettingsGrantAndPage(t *testing.T) {
	plugins := pluginFixture()
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case "plugins.list", actionPluginsInstall:
			return api.WorkbenchResponse{Plugins: plugins}, nil
		case actionPluginsPage:
			return api.WorkbenchResponse{Output: `{"vantryPanel":{"title":"音乐库","nodes":[{"type":"list","items":["春日影","迷星叫"]}]}}`}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "插件")
	tt.Frame()
	for _, text := range []string{"从 GitHub 安装", "MyGo Music", "v1.2.0", "egoist/mygo-music", "内置", "热更新 → v1.3.0", "允许启动外部程序", "音乐库"} {
		if !tt.HasText(text) {
			t.Fatalf("plugin center missing %q: %q", text, tt.Texts())
		}
	}
	if run, _ := tt.Access("运行插件 MyGo Music"); !run.Disabled {
		t.Fatal("a plugin needing process permission must not run before the grant")
	}
	mustClick(t, tt, "插件仓库")
	tt.Type("https://github.com/egoist/mygo-music.git")
	mustClick(t, tt, "安装插件")
	if r, ok := service.find(actionPluginsInstall); !ok || r.Name != "egoist/mygo-music" {
		t.Fatalf("install=%+v", service.requests)
	}
	mustClick(t, tt, "热更新 MyGo Music")
	if r, ok := service.find(actionPluginsUpdate); !ok || r.PluginID != "music" {
		t.Fatalf("update=%+v", service.requests)
	}
	mustClick(t, tt, "允许启动外部程序 MyGo Music")
	if r, ok := service.find(actionPluginsGrant); !ok || r.PluginID != "music" || r.Message != "grant" {
		t.Fatalf("grant=%+v", service.requests)
	}
	mustClick(t, tt, "插件设置 MyGo Music（3）")
	tt.Frame()
	mustClick(t, tt, "显示歌词")
	if r, ok := service.find(actionPluginsSetting); !ok || r.Name != "lyrics" || r.Message != "true" {
		t.Fatalf("boolean setting=%+v", service.requests)
	}
	service.reset()
	mustClick(t, tt, "QQ 音乐")
	if r, ok := service.find(actionPluginsSetting); !ok || r.Name != "source" || r.Message != "qq" {
		t.Fatalf("select setting=%+v", service.requests)
	}
	service.reset()
	field, ok := tt.AccessByRole(ui.RoleTextField, "账号备注")
	if !ok {
		t.Fatal("string setting field missing")
	}
	tt.ClickAt(field.Bounds.X+field.Bounds.W/2, field.Bounds.Y+field.Bounds.H/2)
	tt.Type("demo")
	mustClick(t, tt, "保存设置 账号备注")
	if r, ok := service.find(actionPluginsSetting); !ok || r.Name != "cookie" || r.Message != "默认demo" && r.Message != "demo" {
		t.Fatalf("string setting=%+v", service.requests)
	}
	mustClick(t, tt, "打开页面 音乐库")
	tt.Frame()
	if w.stage != pluginStage("music", "library") || !tt.HasText("春日影") {
		t.Fatalf("plugin page stage=%q texts=%q", w.stage, tt.Texts())
	}
	if _, ok := tt.Access("音乐库"); !ok {
		t.Fatal("plugin page should appear in the sidebar app switcher")
	}
	mustClick(t, tt, "返回对话")
	if w.stage != "" {
		t.Fatal("back did not return to the conversation")
	}
	for in, want := range map[string]string{
		"egoist/mygo-music":                      "egoist/mygo-music",
		"https://github.com/egoist/mygo-music":   "egoist/mygo-music",
		"github.com/egoist/mygo-music/tree/main": "egoist/mygo-music",
		"git@github.com:egoist/mygo-music.git":   "egoist/mygo-music",
		"not a repo":                             "",
		"../etc/passwd":                          "",
	} {
		if got := normalizeGitHubRepo(in); got != want {
			t.Errorf("normalizeGitHubRepo(%q)=%q want %q", in, got, want)
		}
	}
}

// fakeRemoteDesktop streams frames the test pushes and records input.
type fakeRemoteDesktop struct {
	mu           sync.Mutex
	cfg          RemoteDesktopConnect
	onFrame      func()
	frame        *image.RGBA
	seq          uint64
	stats        RemoteDesktopStats
	mouse        []string
	wheel        [][2]float64
	keys         []string
	keyEvents    []remoteKeyRecord
	quality      string
	disconnected bool
}

func (f *fakeRemoteDesktop) Connect(_ context.Context, cfg RemoteDesktopConnect, onFrame func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg, f.onFrame = cfg, onFrame
	f.stats = RemoteDesktopStats{Connected: true, Mode: "relay-webrtc", Transport: "ssh-tcp", Codec: "h264", Hardware: true, RelayHost: "qmt", Width: 160, Height: 90, FPS: 30, RTTMs: 4}
	return nil
}
func (f *fakeRemoteDesktop) Disconnect() error {
	f.mu.Lock()
	f.disconnected = true
	f.mu.Unlock()
	return nil
}
func (f *fakeRemoteDesktop) LatestFrame() (*image.RGBA, uint64, RemoteDesktopStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frame, f.seq, f.stats
}
func (f *fakeRemoteDesktop) SendMouse(kind string, x, y float64, button int) {
	f.mu.Lock()
	f.mouse = append(f.mouse, kind+":"+formatNorm(x)+","+formatNorm(y))
	f.mu.Unlock()
}
func (f *fakeRemoteDesktop) SendWheel(dx, dy float64) {
	f.mu.Lock()
	f.wheel = append(f.wheel, [2]float64{dx, dy})
	f.mu.Unlock()
}
func (f *fakeRemoteDesktop) SendKey(kind string, code int, text string, mods uint32) {
	f.mu.Lock()
	f.keys = append(f.keys, kind+":"+text)
	f.keyEvents = append(f.keyEvents, remoteKeyRecord{kind: kind, code: code, text: text, modifiers: mods})
	f.mu.Unlock()
}
func (f *fakeRemoteDesktop) SetQuality(q string) {
	f.mu.Lock()
	f.quality = q
	f.mu.Unlock()
}

func (f *fakeRemoteDesktop) push(tint uint8) {
	frame := image.NewRGBA(image.Rect(0, 0, 160, 90))
	for y := 0; y < 90; y++ {
		for x := 0; x < 160; x++ {
			frame.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y * 2), tint, 255})
		}
	}
	f.mu.Lock()
	f.frame = frame
	f.seq++
	onFrame := f.onFrame
	f.mu.Unlock()
	onFrame()
}

func formatNorm(v float64) string {
	return strings.TrimRight(strings.TrimRight(strconvFloat(v), "0"), ".")
}

func strconvFloat(v float64) string {
	data, _ := json.Marshal(math.Round(v*100) / 100)
	return string(data)
}

func TestRemoteDesktopStreamsFramesAndForwardsInput(t *testing.T) {
	host := RemoteDesktopHostStatus{Hosting: true, ListenAddr: "0.0.0.0:7788", DirectAddrs: []string{"192.168.1.20:7788", "100.64.0.7:7788"}, RelaySSHHost: "relay-host", RelayPort: 17788, MobileWebURL: "https://relay.example.com/rd/demo-mbp", Token: "VANTRY-7F3K-92QX", Clients: 1}
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case actionRDStatus:
			return api.WorkbenchResponse{Output: `{"hosting":false}`}, nil
		case actionRDPermissions:
			return api.WorkbenchResponse{Output: `{"platform":"darwin","canHost":true,"screenCapture":{"state":"granted"},"inputControl":{"state":"granted"}}`}, nil
		case actionRDHostStart:
			data, _ := json.Marshal(host)
			return api.WorkbenchResponse{Output: string(data)}, nil
		case actionSSHHosts:
			return api.WorkbenchResponse{SSHHosts: []api.SSHHostInfo{{Alias: "qmt"}, {Alias: "mini"}, {Alias: "us"}}}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	driver := &fakeRemoteDesktop{}
	w.defaults.RemoteDesktop = driver
	remoteDesktopPluginFixture(t, w)
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "远程桌面")
	tt.Frame()
	if w.stage != stageRemoteDesktop {
		t.Fatalf("sidebar entry stage=%q", w.stage)
	}
	for _, text := range []string{"连接另一台电脑", "局域网链接直接连接", "允许此电脑被控制"} {
		if !tt.HasText(text) {
			t.Fatalf("remote desktop missing %q: %q", text, tt.Texts())
		}
	}
	// Host this Mac through Core's rd.* fallback.
	mustClick(t, tt, "本机被控")
	if r, ok := service.find(actionRDHostStart); !ok || r.Name != "" {
		t.Fatalf("host start=%+v", service.requests)
	}
	tt.Frame()
	if !tt.HasText("已配对的手机可直接打开此桌面，无需再次扫码或确认。") || tt.HasText(host.Token) {
		t.Fatal("hosting UI exposed a private token or lost the copy action")
	}
	w.access.status.Relay = "own-relay"
	tt.Frame()
	mustClick(t, tt, "远程桌面目标")
	tt.Type("vantry-rtc://own-relay/test-session?token=private-test-only-token")
	mustClick(t, tt, "立即远程连接")
	if !driver.cfg.RelayOnly || len(driver.cfg.DirectAddrs) != 0 || driver.cfg.RelaySSHHost != "own-relay" || driver.cfg.Quality != "balanced" {
		t.Fatal("RTC view did not retain its relay-only configuration")
	}
	tt.Frame()
	if !w.rd.connected || !tt.HasText("等待第一帧画面…") {
		t.Fatal("connected view should wait for the first frame")
	}
	driver.push(10)
	tt.Frame()
	first := w.rd.bitmap
	if first == nil || w.rd.seq != 1 {
		t.Fatal("first frame not shown")
	}
	driver.push(200)
	tt.Frame()
	if w.rd.bitmap != first || w.rd.seq != 2 {
		t.Fatal("frames must reuse one bitmap")
	}
	if !tt.HasText("中继 · WebRTC / H.264 · SSH/TCP · 硬解 · 4ms · 30 FPS") {
		t.Fatal("RTC HUD lost codec, hardware or actual transport")
	}
	driver.mu.Lock()
	driver.stats = RemoteDesktopStats{Connected: true, Mode: "relay-webrtc", Transport: "ssh-tcp", Codec: "h264", RelayHost: "qmt", Width: 160, Height: 90, FPS: 24, RTTMs: 26}
	driver.mu.Unlock()
	driver.push(90)
	tt.Frame()
	if !tt.HasText("中继 · WebRTC / H.264 · SSH/TCP · 26ms · 24 FPS") {
		t.Fatalf("relay HUD missing: %q", tt.Texts())
	}
	view, ok := tt.Access("远程桌面画面")
	if !ok {
		t.Fatal("viewport missing")
	}
	fit := letterbox(160, 90, view.Bounds)
	px, py := fit.X+fit.W*0.25, fit.Y+fit.H*0.75
	tt.Press(px, py)
	tt.Release(px, py)
	tt.Wheel(px, py, 0, 3)
	tt.Type("x")
	driver.mu.Lock()
	mouse, wheel, keys := append([]string(nil), driver.mouse...), append([][2]float64(nil), driver.wheel...), append([]string(nil), driver.keys...)
	driver.mu.Unlock()
	down := -1
	for i, m := range mouse {
		if strings.HasPrefix(m, "down:") {
			down = i
			break
		}
	}
	if down < 0 || mouse[down] != "down:0.25,0.75" || mouse[len(mouse)-1] != "up:0.25,0.75" {
		t.Fatalf("mouse normalization=%v (fit %+v)", mouse, fit)
	}
	if len(wheel) == 0 || !strings.Contains(strings.Join(keys, " "), "down:x") {
		t.Fatalf("wheel=%v keys=%v", wheel, keys)
	}
	mustClick(t, tt, "省流")
	if driver.quality != "low" {
		t.Fatalf("quality=%q", driver.quality)
	}
	mustClick(t, tt, "断开远程桌面")
	if !driver.disconnected || w.rd.connected {
		t.Fatal("disconnect not forwarded")
	}
	if x, y, inside := (&workbench{rd: remoteDesktopState{frameW: 100, frameH: 100, viewW: 200, viewH: 100}}).normalizeRemotePoint(10, 50); inside || x != 0 || y != 0.5 {
		t.Fatalf("letterbox bars must clamp outside points: %v %v %v", x, y, inside)
	}
}

func TestRemoteDesktopWithoutEngineExplainsAndNeverFallsBack(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	remoteDesktopPluginFixture(t, w)
	w.openRemoteDesktop("us")
	if w.connectRemoteDesktop() || !strings.Contains(w.rd.err, "未链接远程桌面引擎") {
		t.Fatalf("missing engine: %q", w.rd.err)
	}
	w.rd.target = "vantry-lan://192.168.1.20:7788/test"
	w.access.status.Relay = "own-relay"
	if cfg := w.remoteDesktopConfig(); !cfg.DirectOnly || cfg.RelayOnly || cfg.RelaySSHHost != "" {
		t.Fatal("a LAN link silently selected a relay")
	}
	w.rd.target = "DESK-ABCD-EFGH"
	if cfg := w.remoteDesktopConfig(); cfg.DirectOnly || !cfg.RelayOnly || cfg.RelaySSHHost != "own-relay" {
		t.Fatal("the shared environment relay was not used")
	}
}

const uiBlockFixture = "交互卡片如下：\n\n```vantry-ui\n" + `{"title":"发布体检","subtitle":"v0.1.4 · desktop","components":[
 {"type":"tabs","tabs":[
  {"label":"概览","components":[{"type":"metrics","items":[{"label":"测试","value":"414","delta":"+12"},{"label":"耗时","value":"64s","delta":"-8s"},{"label":"覆盖率","value":"81%"}]}]},
  {"label":"明细","components":[{"type":"table","columns":["模块","大小"],"rows":[["desktop","120"],["core","80"],["api","300"]]}]}
 ]},
 {"type":"steps","items":[{"label":"构建","status":"done"},{"label":"测试","status":"active","text":"go test -race"},{"label":"发布","status":"pending"}]},
 {"type":"progress","items":[{"label":"上传增量包","value":42,"max":100}]},
 {"type":"actions","items":[{"label":"复制测试命令","command":"go test ./internal/desktop/..."},{"label":"写入草稿","prefill":"请继续发布 v0.1.4"}]}
]}` + "\n```\n"

func TestVantryUIBlocksRenderInteractiveCards(t *testing.T) {
	doc, err := parseUIDoc(strings.TrimSuffix(strings.SplitN(uiBlockFixture, "```vantry-ui\n", 2)[1], "\n```\n"))
	if err != nil || doc.Title != "发布体检" || len(doc.Blocks) != 4 || doc.Blocks[2].Items[0].Progress != 0.42 {
		t.Fatalf("doc=%+v err=%v", doc, err)
	}
	if _, err := parseUIDoc(`{"nothing":true}`); err == nil {
		t.Fatal("empty doc must fail")
	}
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "做一张发布体检卡片", uiBlockFixture, 0))
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	for _, text := range []string{"发布体检", "414", "+12", "构建", "go test -race", "42%", "复制测试命令"} {
		if !tt.HasText(text) {
			t.Fatalf("ui block missing %q: %q", text, tt.Texts())
		}
	}
	if strings.Contains(strings.Join(tt.Texts(), "\n"), `"components"`) {
		t.Fatal("raw JSON shown")
	}
	mustClick(t, tt, "明细")
	tt.Frame()
	if !tt.HasText("desktop") || !tt.HasText("300") {
		t.Fatal("tab switch failed")
	}
	mustClick(t, tt, "按 大小 排序")
	tt.Frame()
	order := []string{}
	for _, text := range tt.Texts() {
		if text == "80" || text == "120" || text == "300" {
			order = append(order, text)
		}
	}
	if !reflect.DeepEqual(order, []string{"80", "120", "300"}) {
		t.Fatalf("numeric sort=%v", order)
	}
	mustClick(t, tt, "复制测试命令")
	if tt.Clipboard() != "go test ./internal/desktop/..." {
		t.Fatalf("clipboard=%q", tt.Clipboard())
	}
	mustClick(t, tt, "写入草稿")
	if !strings.Contains(w.composerFor("t1").draft, "请继续发布 v0.1.4") {
		t.Fatalf("draft=%q", w.composerFor("t1").draft)
	}
}

func TestBarChartHoverShowsCrosshairBadge(t *testing.T) {
	output := "```chart\n" + `{"type":"bar","title":"周提交","labels":["一","二","三"],"series":[{"name":"desktop","data":[3,7,5]},{"name":"core","data":[2,1,4]}]}` + "\n```\n"
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "画图", output, 0))
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	plot, ok := tt.Access("周提交")
	if !ok {
		t.Fatal("chart missing")
	}
	x, y := plot.Bounds.X+plot.Bounds.W*0.5, plot.Bounds.Y+plot.Bounds.H*0.6
	tt.Move(x, y)
	tt.Frame()
	if !tt.HasText("7") {
		t.Fatalf("hover badge missing: %q", tt.Texts())
	}
}

func TestLineChartCachesStaticLayerAcrossHover(t *testing.T) {
	output := "```chart\n" + `{"type":"area","title":"延迟","labels":["一","二","三","四"],"series":[{"name":"直连","data":[4,6,5,7]},{"name":"中继","data":[26,24,30,28]}],"unit":"ms"}` + "\n```\n"
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "画图", output, 0))
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	plot, ok := tt.Access("延迟")
	if !ok {
		t.Fatal("chart missing")
	}
	for _, f := range []float32{0.3, 0.5, 0.7} {
		tt.Move(plot.Bounds.X+plot.Bounds.W*f, plot.Bounds.Y+plot.Bounds.H*0.6)
		tt.Frame()
	}
	if !tt.HasText("三 · 直连 5ms · 中继 30ms") && !tt.HasText("二 · 直连 6ms · 中继 24ms") {
		t.Fatalf("line hover badge missing: %q", tt.Texts())
	}
	if len(tt.Image().Pix) == 0 {
		t.Fatal("no frame painted")
	}
}

func TestSettingsChecksAndInstallsGitHubUpdate(t *testing.T) {
	var quitCalls int
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case actionUpdateCheck:
			return api.WorkbenchResponse{Update: &api.AppUpdateInfo{
				CurrentVersion: "0.1.3",
				LatestVersion:  "0.1.4",
				Available:      true,
				DeltaAvailable: true,
				DeltaSize:      1 << 20,
				AssetSize:      3 << 20,
				Notes:          "- 右侧工具坞\n- IDEA Git",
			}}, nil
		case actionUpdateInstall:
			return api.WorkbenchResponse{
				Update: &api.AppUpdateInfo{
					CurrentVersion: "0.1.3",
					LatestVersion:  "0.1.4",
					Available:      true,
					DeltaAvailable: true,
					DeltaSize:      1 << 20,
					AssetSize:      3 << 20,
					Notes:          "- 右侧工具坞\n- IDEA Git",
					Staged:         true,
					StagedVersion:  "0.1.4",
					StagedPath:     "/Applications/.vantry-update-123/payload/Vantry.app",
					TargetPath:     "/Applications/Vantry.app",
					InstallerPath:  "/Applications/.vantry-update-123/安装更新.command",
					UsedDelta:      true,
					CanAutoRestart: true,
				},
				Output: "更新已下载并完成签名校验，点击「重启并更新」即可安装。",
			}, nil
		case actionUpdateApply:
			return api.WorkbenchResponse{Output: "正在退出并安装更新…"}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	w.defaults.Version = "0.1.3"
	w.quit = func() { quitCalls++ }
	tt := ui.NewTester(w.view, 1080, 760)
	mustClick(t, tt, "设置")
	mustClick(t, tt, "关于与更新")
	tt.Frame()
	if !tt.HasText("应用更新") || !tt.HasText("Vantry v0.1.3") {
		t.Fatalf("updates section missing: %q", tt.Texts())
	}
	tt.Wheel(540, 380, 0, 4000) // the section sits at the end of Settings
	tt.Advance(time.Second)
	mustClick(t, tt, "检查更新")
	tt.Frame()
	tt.Wheel(540, 380, 0, 4000)
	tt.Advance(time.Second)
	for _, text := range []string{"新版本 v0.1.4", "增量更新", "1.0 MB", "右侧工具坞", "下载增量更新"} {
		if !tt.HasText(text) {
			t.Fatalf("update info missing %q: %q", text, tt.Texts())
		}
	}
	mustClick(t, tt, "下载增量更新")
	if r, ok := service.find(actionUpdateInstall); !ok || r.Name != "0.1.4" {
		t.Fatalf("install=%+v", service.requests)
	}
	tt.Frame()
	tt.Wheel(540, 380, 0, 4000)
	tt.Advance(time.Second)
	if !tt.HasText("v0.1.4 已下载并完成校验（增量更新）") {
		t.Fatalf("staged status missing: %q", tt.Texts())
	}
	for _, txt := range tt.Texts() {
		if strings.Contains(txt, ".vantry-update-") {
			t.Fatalf("raw hidden staging path should not clutter text UI: %q", txt)
		}
	}
	mustClick(t, tt, "重启并更新")
	applyReq, ok := service.find(actionUpdateApply)
	if !ok || applyReq.Name != "0.1.4" || !strings.Contains(string(applyReq.Input), "desktopPid") {
		t.Fatalf("apply request missing desktopPid: ok=%v req=%+v", ok, applyReq)
	}
	if quitCalls != 1 {
		t.Fatalf("quitCalls = %d, want 1", quitCalls)
	}
}

// TestWorkbenchV3Renders always lays the new surfaces out offscreen; set
// VANTRY_DESKTOP_RENDER_DIR to export PNGs for review.
func TestWorkbenchV3Renders(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output != "" {
		if err := os.MkdirAll(output, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sizes := []struct {
		name          string
		width, height int
		dark          bool
	}{{"1440-dark", 1440, 900, true}, {"1280-light", 1280, 820, false}, {"760-narrow", 760, 640, false}}
	scenarios := []struct {
		name  string
		setup func(t *testing.T, w *workbench)
		want  string
	}{
		{"git-commit", func(t *testing.T, w *workbench) {
			w.features.panel = "git"
			w.features.git = map[string]*api.GitState{"t1": fixtureGitState()}
			w.gitView.checked = map[string]map[string]bool{"t1": {"docs/notes.md": true}}
			w.gitView.messages = map[string]string{"t1": "feat(desktop): right workbench dock"}
		}, "已暂存"},
		{"git-log", func(t *testing.T, w *workbench) {
			w.features.panel = "git"
			state := fixtureGitState()
			w.features.git = map[string]*api.GitState{"t1": state}
			w.gitView.tab = 1
			w.gitView.logLoaded = map[string]string{"t1": ""}
			w.gitView.logs = map[string][]api.GitCommit{"t1": state.Commits}
			w.gitView.commit = map[string]string{"t1": state.Commits[2].ID}
			w.gitView.detailFile = map[string]string{}
			w.gitView.details = map[string]*gitCommitDetail{"t1:" + state.Commits[2].ID: decodeCommitDetail(api.WorkbenchResponse{Git: &api.GitState{
				Commits: state.Commits[2:3], Files: []api.GitFile{{Path: "internal/app/search.go", Index: "M"}},
				Diff: "diff --git a/internal/app/search.go b/internal/app/search.go\n--- a/internal/app/search.go\n+++ b/internal/app/search.go\n@@ -1,2 +1,2 @@\n func rotate() {\n-\told()\n+\tnext()\n",
			}}, state.Commits[2].ID)}
		}, "HEAD → main"},
		{"git-maximized", func(t *testing.T, w *workbench) {
			w.features.panel = "git"
			w.dock.maximized = true
			w.features.git = map[string]*api.GitState{"t1": fixtureGitState()}
		}, "更改"},
		{"ssh-hub", func(t *testing.T, w *workbench) {
			w.features.panel = "remote"
			yes, no := true, false
			w.ssh.loaded = true
			w.ssh.hosts = []api.SSHHostInfo{
				{Alias: "devbox", Aliases: []string{"devbox-win", "shanghai"}, User: "demo", HostName: "devbox.local", Reachable: &yes, LatencyMs: 24, RemoteOS: "Darwin 25.1.0 arm64", RemoteSocket: "/Users/demo/.config/Vantry/core.sock"},
				{Alias: "tokyo-a", Port: 2222, ProxyJump: "devbox", Reachable: &no, ProbeFailure: "auth", ProbeError: "SSH 认证失败，请检查该主机的密钥或 SSH Agent。"},
				{Alias: "us-west", Port: 22, User: "root", HostName: "198.51.100.10"},
				{Alias: "mini", HasHerdr: true, RemoteSocket: "/Users/demo/.config/Vantry/core.sock", Reachable: &yes, LatencyMs: 3},
				{Alias: "orb-ubuntu", Aliases: []string{"orb"}, ProbeFailure: "timeout", ProbeError: "SSH 连接超时，请检查目标机器、网络及 Vantry 本地网络权限。"},
			}
			no2 := false
			w.ssh.hosts[4].Reachable = &no2
			w.remote.saved = []remoteFavorite{{Name: "开发机", Host: "devbox", Socket: "/run/vantry/core.sock"}}
			w.ssh.selected = "devbox"
			w.ssh.recent = map[string]time.Time{"devbox": time.Now().Add(-8 * time.Minute), "tokyo-a": time.Now().Add(-3 * time.Hour)}
		}, "devbox-win"},
		{"ssh-hub-maximized", func(t *testing.T, w *workbench) {
			w.features.panel = "remote"
			w.dock.maximized = true
			yes, no := true, false
			w.ssh.loaded = true
			w.ssh.hosts = []api.SSHHostInfo{
				{Alias: "devbox", User: "demo", HostName: "devbox.local", Reachable: &yes, LatencyMs: 24, RemoteOS: "Darwin 25.1.0 arm64"},
				{Alias: "tokyo-a", Port: 2222, User: "root", HostName: "198.51.100.10", Reachable: &no, ProbeFailure: "auth", ProbeError: "SSH 认证失败，请检查该主机的密钥或 SSH Agent。"},
				{Alias: "mini", Reachable: &yes, LatencyMs: 3},
			}
			w.ssh.manual = []manualSSHHost{{Name: "构建机", Host: "10.0.0.12", User: "root", Port: 2222, Identity: "~/.ssh/id_ed25519"}}
			w.ssh.probes = map[string]hostProbe{"root@10.0.0.12:2222": {tested: true, reachable: true, latency: 42, remoteOS: "Linux 6.8.0-45-generic aarch64"}}
			w.ssh.selected = "root@10.0.0.12:2222"
			w.ssh.recent = map[string]time.Time{"devbox": time.Now().Add(-8 * time.Minute)}
		}, "构建机"},
		{"ssh-hub-add", func(t *testing.T, w *workbench) {
			w.features.panel = "remote"
			w.ssh.loaded = true
			w.ssh.hosts = []api.SSHHostInfo{{Alias: "devbox"}, {Alias: "mini"}}
			w.openSSHHostForm("")
			w.ssh.add.name, w.ssh.add.host, w.ssh.add.user, w.ssh.add.key = "构建机", "10.0.0.12", "root", "~/.ssh/id_ed25519"
			w.ssh.add.tested = true
			w.ssh.add.result = hostProbe{tested: true, reachable: true, latency: 42, remoteOS: "Linux 6.8.0-45-generic aarch64"}
		}, "密钥文件"},
		{"ssh-hub-add-failed", func(t *testing.T, w *workbench) {
			w.features.panel = "remote"
			w.ssh.loaded = true
			w.openSSHHostForm("")
			w.ssh.add.host, w.ssh.add.user = "10.0.0.99", "root"
			w.ssh.add.errors.host = "只能包含字母、数字和 . _ : -"
			w.ssh.add.tested = true
			w.ssh.add.result = hostProbe{tested: true, kind: "refused", message: "目标机器拒绝 SSH 连接，请检查远程登录是否开启。"}
		}, "连不上"},
		{"ssh-hub-loading", func(t *testing.T, w *workbench) {
			w.features.panel = "remote"
			w.ssh.loading = true
			if w.features.busy == nil {
				w.features.busy = map[string]bool{}
			}
			w.features.busy["ssh-hosts"] = true
		}, "正在读取 ~/.ssh/config…"},
		{"ssh-hub-empty", func(t *testing.T, w *workbench) {
			w.features.panel = "remote"
			w.ssh.loaded = true
			w.ssh.err = ""
		}, "还没有主机"},
		{"ssh-hub-error", func(t *testing.T, w *workbench) {
			w.features.panel = "remote"
			w.ssh.loaded = true
			w.ssh.err = "读取 ~/.ssh/config 失败：permission denied"
		}, "读不到 SSH 配置"},
		{"terminal-ssh-ready", func(t *testing.T, w *workbench) {
			terminalSessionFixture(t, w, "devbox", sshReady, nil)
		}, "已连接"},
		{"terminal-ssh-failed", func(t *testing.T, w *workbench) {
			terminalSessionFixture(t, w, "tokyo-a", sshFailed, &sshFailure{
				title:  "认证失败",
				detail: "服务器拒绝了你的密钥或密码。检查该主机的密钥、ssh-agent，或改用密码登录。",
				raw:    "root@198.51.100.10: Permission denied (publickey,password).",
			})
		}, "重新连接"},
		{"terminal-empty", func(t *testing.T, w *workbench) {
			w.features.panel = "terminal"
		}, "这里放终端"},
		{"plugins", func(t *testing.T, w *workbench) {
			w.features.panel = "plugins"
			w.features.plugins = pluginFixture()
			w.plugs.open = map[string]bool{"music": true}
		}, "MyGo Music"},
		{"remote-desktop-idle", func(t *testing.T, w *workbench) {
			remoteDesktopPluginFixture(t, w)
			w.ssh.loaded = true
			w.ssh.hosts = []api.SSHHostInfo{{Alias: "devbox"}, {Alias: "mini"}, {Alias: "us"}, {Alias: "tokyo-a"}}
			w.openRemoteDesktop("mini")
			w.rd.hostLoaded = true
			w.rd.host = &RemoteDesktopHostStatus{Hosting: true, DirectAddrs: []string{"192.168.1.20:7788", "100.64.0.7:7788"}, RelaySSHHost: "devbox", RelayPort: 17788, MobileWebURL: "https://relay.example.com/rd/demo-mbp", Token: "VANTRY-7F3K-92QX", Clients: 1}
		}, "立即远程连接"},
		{"remote-desktop-permissions", func(t *testing.T, w *workbench) {
			remoteDesktopPluginFixture(t, w)
			w.ssh.loaded = true
			w.openRemoteDesktop("mini")
			w.rd.hostLoaded = true
			w.rd.host = &RemoteDesktopHostStatus{RelaySSHHost: "devbox"}
			w.rd.permission.intent, w.rd.permission.shown = true, true
			w.rd.permission.status = &RemotePermissions{Platform: "darwin",
				ScreenCapture: RemotePermissionAccess{State: "granted", CanRequest: true, CanOpenSettings: true},
				InputControl:  RemotePermissionAccess{State: "denied", CanRequest: true, CanOpenSettings: true, Message: "允许 Vantry 使用辅助功能，以接收键盘和鼠标操作。"}}
		}, "辅助功能"},
		{"remote-desktop-stream", func(t *testing.T, w *workbench) {
			remoteDesktopPluginFixture(t, w)
			driver := &fakeRemoteDesktop{}
			w.defaults.RemoteDesktop = driver
			w.openRemoteDesktop("mini")
			w.rd.hostLoaded = true
			w.connectRemoteDesktop()
			driver.mu.Lock()
			driver.stats.Mode, driver.stats.RelayHost, driver.stats.RTTMs, driver.stats.FPS = "relay-ssh", "qmt", 26, 24
			driver.mu.Unlock()
			driver.push(160)
		}, "中继 Relay (qmt) · 26ms · 24 FPS"},
		{"ui-card", func(t *testing.T, w *workbench) {
			w.runs[0].Output = uiBlockFixture
			clear(w.outputs)
		}, "发布体检"},
		{"settings-update", func(t *testing.T, w *workbench) {
			w.widgets.settingsOpen, w.widgets.settingsPage = true, "about"
			w.defaults.Version = "0.1.3"
			w.updates.info = &api.AppUpdateInfo{CurrentVersion: "0.1.3", LatestVersion: "0.1.4", Available: true, DeltaAvailable: true, AssetSize: 3 << 20, Notes: "- 右侧工具坞与最大化\n- JetBrains 风格 Git"}
		}, "下载更新"},
	}
	for _, size := range sizes {
		for _, scenario := range scenarios {
			t.Run(size.name+"-"+scenario.name, func(t *testing.T) {
				w, _ := v3Workbench(t, nil)
				w.widgets.themeSet, w.widgets.dark = true, size.dark
				scenario.setup(t, w)
				tt := ui.NewTesterWith(w.view, size.width, size.height, ui.TesterOptions{Dark: size.dark})
				tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
				tt.Advance(time.Second)
				tt.Frame()
				if !tt.HasText(scenario.want) {
					t.Fatalf("%q not rendered: %q", scenario.want, tt.Texts())
				}
				if output == "" {
					return
				}
				file, err := os.Create(filepath.Join(output, "v3-"+size.name+"-"+scenario.name+".png"))
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
			})
		}
	}
}

// terminalSessionFixture shows the dock's terminal tab with one machine and
// its state, on a screen that looks like the session it reports.
func terminalSessionFixture(t *testing.T, w *workbench, name string, phase sshPhase, failure *sshFailure) {
	t.Helper()
	w.ssh.loaded = true
	entry := sshEntry{name: name, target: name, key: name, host: api.SSHHostInfo{Alias: name}}
	w.ssh.hosts = []api.SSHHostInfo{entry.host}
	w.newTerminal = nil // a real terminal, so the screen draws a pty of its own
	session := &sshSession{target: name, name: name, argv: entry.sshArgv(), started: time.Now(), phase: phase}
	session.command = sshCommandLine(session.argv)
	session.settled = phase == sshFailed
	session.failure = failure
	term, run, err := w.launchTerminal(terminal.Options{NoBlink: true},
		[]string{"/bin/sh", "-c", `printf 'Welcome to Ubuntu 24.04.1 LTS (GNU/Linux 6.8.0-45-generic aarch64)\n\nLast login: Mon Nov 17 09:12:44 2025 from 10.0.0.2\nroot@tokyo-a:~# uptime\n 09:41:02 up 42 days,  3:14,  1 user,  load average: 0.08, 0.12, 0.09\nroot@tokyo-a:~# '`},
		func(code int) string { return "" })
	if err != nil {
		t.Fatalf("terminal: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })
	session.term, session.run = term, run
	w.ssh.sessions = map[string]*sshSession{name: session}
	w.ssh.order = []string{name}
	w.termTab.active = name
	w.features.panel = "terminal"
}
