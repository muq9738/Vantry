package desktop

import (
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestShortcutsCheatsheetOpensWithCmdSlashAndClosesWithEscape(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	tt := ui.NewTester(w.view, 1080, 760)
	tt.Key(ui.Cmd, ui.KeySlash)
	if !w.widgets.shortcutsOpen || !tt.HasText("快捷键速查") || !tt.HasText("全局导航") || !tt.HasText("F6") {
		t.Fatalf("cheat sheet not shown: open=%v texts=%q", w.widgets.shortcutsOpen, tt.Texts())
	}
	tt.Key(0, ui.KeyEscape)
	if w.widgets.shortcutsOpen {
		t.Fatal("Escape did not close the cheat sheet")
	}
	mustClick(t, tt, "快捷键")
	if !w.widgets.shortcutsOpen {
		t.Fatal("title bar ? button did not open the cheat sheet")
	}
}

func TestSidebarForkSelectsNewThreadFromReceipt(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	service.receipt = func(cmd api.Command) (api.Receipt, error) {
		return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: "t-fork"}, nil
	}
	tt := ui.NewTester(w.view, 1280, 820)
	if err := tt.RightClick("第一条线程"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("从此处分叉会话"); err != nil {
		t.Fatalf("fork item: %v menu=%q", err, tt.Menu())
	}
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != commandForkThread || commands[0].ThreadID != "t1" || commands[0].RunID != "" {
		t.Fatalf("fork command=%+v", commands)
	}
	if w.selected != "t-fork" || w.awaitingThread != "t-fork" {
		t.Fatalf("fork not selected: selected=%q awaiting=%q", w.selected, w.awaitingThread)
	}
}

func TestTurnForkCarriesRunID(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	service.receipt = func(cmd api.Command) (api.Receipt, error) {
		return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: "t-fork"}, nil
	}
	tt := ui.NewTester(w.view, 1280, 820)
	tt.Advance(time.Second)
	mustClick(t, tt, "从本轮分叉")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != commandForkThread || commands[0].RunID != "r1" {
		t.Fatalf("turn fork=%+v", commands)
	}
}

func TestDeleteIsOneClickWithoutConfirmation(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	second := thread("t2", "第二条线程", time.Minute)
	w.threads = append(w.threads, second)
	w.runs = append(w.runs, run("r2", "t2", api.RunRunning, "在跑", "", time.Minute))
	tt := ui.NewTester(w.view, 1280, 820)

	if err := tt.RightClick("第一条线程"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("删除"); err != nil {
		t.Fatalf("delete item: %v menu=%q", err, tt.Menu())
	}
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.DeleteThread || commands[0].ThreadID != "t1" || !commands[0].Force {
		t.Fatalf("one click must delete at once: %+v", commands)
	}
	tt.Frame()
	if tt.HasText("确认删除") || w.sessions.deleteConfirm != "" {
		t.Fatal("a second confirmation is still shown")
	}
	if w.hasThread("t1") || w.selected == "t1" {
		t.Fatal("accepted delete left the thread projected or selected")
	}
	// A running thread deletes in one click too, forced.
	if !w.deleteThread("t2") {
		t.Fatal("running thread refused")
	}
	commands = service.dispatched()
	if len(commands) != 2 || commands[1].ThreadID != "t2" || !commands[1].Force {
		t.Fatalf("forced delete = %+v", commands)
	}
}

func TestDeleteInFlightIsNotRepeated(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	held := holdWork(w)
	if !w.requestDelete("t1") || w.requestDelete("t1") {
		t.Fatal("a second click while the first delete is in flight must be ignored")
	}
	held.release()
	if got := service.dispatched(); len(got) != 1 {
		t.Fatalf("duplicate delete commands: %+v", got)
	}
}

func TestThreadDeletedEventRemovesThread(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	if !w.applyEvent(api.Event{Sequence: 10, Type: eventThreadDeleted, ThreadID: "t1", Data: mustJSON(map[string]string{"threadId": "t1"})}) {
		t.Fatal("thread.deleted not handled")
	}
	if w.hasThread("t1") || len(w.runsFor("t1")) != 0 {
		t.Fatal("deleted thread still projected")
	}
}

func TestBackgroundCompletionMarksUnreadUntilOpened(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	w.threads = append(w.threads, thread("t2", "后台会话", time.Minute))
	background := run("r2", "t2", api.RunRunning, "后台任务", "", time.Minute)
	w.runs = append(w.runs, background)
	w.selectThread("t1")
	done := background
	done.Status = api.RunCompleted
	if !w.applyEvent(statusEvent(20, done)) {
		t.Fatal("completion not folded")
	}
	if !w.isUnread("t2") || w.isUnread("t1") {
		t.Fatalf("unread=%v", w.sessions.unread)
	}
	tt := ui.NewTester(w.view, 1280, 820)
	if info, ok := tt.Access("后台会话"); !ok || !strings.Contains(info.Description, "有新结果") {
		t.Fatalf("unread row not described: %+v %v", info, ok)
	}
	w.selectThread("t2")
	if w.isUnread("t2") {
		t.Fatal("opening the session did not clear its mark")
	}
	// The selected session's own completion never marks it.
	w.runs = append(w.runs, run("r3", "t2", api.RunRunning, "再来", "", 2*time.Minute))
	finished := run("r3", "t2", api.RunCompleted, "再来", "好", 2*time.Minute)
	w.applyEvent(statusEvent(21, finished))
	if w.isUnread("t2") {
		t.Fatal("current session collected an unread mark")
	}
}

func TestSidebarSearchShowsMatchedConversationSnippet(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "如何配置 Magpie 代理", "在 mcporter.json 中加入 exa 搜索入口即可。", 0))
	if got := w.searchSnippet("t1", "mcporter"); !strings.HasPrefix(got, "匹配对话：") || !strings.Contains(got, "mcporter") {
		t.Fatalf("snippet=%q", got)
	}
	if got := w.searchSnippet("t1", "第一条"); got != "" {
		t.Fatalf("title match should not add a snippet: %q", got)
	}
	w.widgets.search = "mcporter"
	tt := ui.NewTester(w.view, 1280, 820)
	if info, ok := tt.Access("第一条线程"); !ok || !strings.Contains(info.Description, "匹配对话") {
		t.Fatalf("search row lacks snippet: %+v", info)
	}
}

func filesService(t *testing.T, requests *[]api.WorkbenchRequest) *fakeWorkbenchService {
	t.Helper()
	return &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		*requests = append(*requests, r)
		switch r.Action {
		case actionFilesSearch:
			return api.WorkbenchResponse{Output: `{"files":["internal/core/store.go","internal/core/workbench.go"]}`}, nil
		case actionFilesList:
			if len(r.Paths) == 1 && r.Paths[0] == "internal" {
				return api.WorkbenchResponse{Output: `[{"name":"core","path":"internal/core","type":"dir"},{"name":"main.go","path":"internal/main.go"}]`}, nil
			}
			return api.WorkbenchResponse{Output: "README.md\ninternal/\n"}, nil
		case actionFilesRead:
			return api.WorkbenchResponse{Output: "package main\n\nfunc main() {}\n"}, nil
		}
		return api.WorkbenchResponse{}, errors.New("unsupported")
	}}
}

func TestAtFilePickerSearchesAndInsertsWithKeyboard(t *testing.T) {
	var requests []api.WorkbenchRequest
	service := filesService(t, &requests)
	w := newWorkbench(service)
	th := thread("t1", "Thread", 0)
	th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{th}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)}})
	tt := ui.NewTester(w.view, 1080, 760)
	mustClick(t, tt, "消息输入框")
	tt.Type("请看 @core")
	tt.Frame()
	if !tt.HasText("workbench.go") || !tt.HasText("store.go") {
		t.Fatalf("candidates missing: %q", tt.Texts())
	}
	searched := false
	for _, r := range requests {
		if r.Action == actionFilesSearch && r.ThreadID == "t1" && r.Message != "" {
			searched = true
		}
	}
	if !searched {
		t.Fatalf("no files.search request: %+v", requests)
	}
	tt.Key(0, ui.KeyDown)
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if got := w.composerFor("t1").draft; got != "请看 @internal/core/workbench.go " {
		t.Fatalf("draft=%q", got)
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("picker Enter sent the message")
	}
	tt.Type("@zzz")
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if tt.HasText("引用工作区文件") || strings.Contains(strings.Join(tt.Texts(), " "), "匹配 “zzz”") {
		t.Fatal("Escape did not dismiss the picker")
	}
}

func TestFilePickerDegradesWithoutFileService(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "q", "a", 0))
	w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
	w.setDraft("t1", "看 @main")
	tt := ui.NewTester(w.view, 1080, 760)
	tt.Frame()
	if !tt.HasText("文件搜索暂不可用") {
		t.Fatalf("no graceful hint: %q", tt.Texts())
	}
	if _, _, ok := mentionQuery("mail me@host.com"); ok {
		t.Fatal("e-mail address treated as a mention")
	}
}

func TestFilesPanelBrowsesReadsAndQuotesPath(t *testing.T) {
	var requests []api.WorkbenchRequest
	service := filesService(t, &requests)
	w := newWorkbench(service)
	th := thread("t1", "Thread", 0)
	th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{th}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)}})
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "文件")
	tt.Frame()
	if w.features.panel != "files" || !tt.HasText("README.md") {
		t.Fatalf("root listing missing: panel=%q %q", w.features.panel, tt.Texts())
	}
	mustClick(t, tt, "internal")
	tt.Frame()
	if w.fileDir("t1") != "internal" || !tt.HasText("main.go") || !tt.HasText("..") {
		t.Fatalf("subdirectory not entered: dir=%q %q", w.fileDir("t1"), tt.Texts())
	}
	mustClick(t, tt, "internal/main.go")
	tt.Frame()
	if w.files.open["t1"] != "internal/main.go" {
		t.Fatal("file not opened")
	}
	read := false
	for _, r := range requests {
		if r.Action == actionFilesRead && len(r.Paths) == 1 && r.Paths[0] == "internal/main.go" {
			read = true
		}
	}
	if !read {
		t.Fatalf("no files.read: %+v", requests)
	}
	mustClick(t, tt, "引用路径到输入框")
	if got := w.composerFor("t1").draft; got != "@internal/main.go " {
		t.Fatalf("quoted draft=%q", got)
	}
	mustClick(t, tt, "显示目录")
	mustClick(t, tt, "返回上级目录 ..")
	if w.fileDir("t1") != "" {
		t.Fatalf("parent dir=%q", w.fileDir("t1"))
	}
}

func TestPlanModeFramesSendWithoutChangingTitle(t *testing.T) {
	w, service := newViewWorkbench()
	local := w.newSession()
	tt := ui.NewTester(w.view, 1080, 760)
	mustClick(t, tt, "规划模式")
	if !w.composerFor(local).plan {
		t.Fatal("plan capsule did not switch")
	}
	w.setDraft(local, "重构同步层")
	w.send(local)
	commands := service.dispatched()
	if len(commands) != 2 || commands[0].Title != "重构同步层" || !strings.HasPrefix(commands[1].Message, planModePrefix) || !strings.HasSuffix(commands[1].Message, "重构同步层") {
		t.Fatalf("plan send=%+v", commands)
	}
	if titleFromInstruction(commands[1].Message) != "重构同步层" {
		t.Fatal("plan prefix leaked into derived titles")
	}
}

func TestApprovalTiersRoundTrip(t *testing.T) {
	for _, policy := range []string{"auto", "auto-accept-edits", "ask"} {
		if normalizeApprovalPolicy(policy) != policy || approvalPolicyForLabel(approvalTierFor(policy).label) != policy {
			t.Fatalf("tier %q did not round trip", policy)
		}
	}
	if normalizeApprovalPolicy("bogus") != "auto" {
		t.Fatal("unknown policy not normalized")
	}
}

func queueFixture() (*workbench, *fakeService, []api.Input) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunRunning, "长任务", "进行中", 0))
	inputs := []api.Input{
		{ID: "in-1", ThreadID: "t1", Kind: "queue", Status: "queued", Message: "第一条排队", CreatedAt: baseTime.Add(time.Second)},
		{ID: "in-2", ThreadID: "t1", Kind: "queue", Status: "queued", Message: "第二条排队", CreatedAt: baseTime.Add(2 * time.Second)},
	}
	w.inputs = append(w.inputs, inputs...)
	return w, service, inputs
}

func TestQueueCardEditMoveAndPromote(t *testing.T) {
	w, service, inputs := queueFixture()
	w.setDraft("t1", "尚未发送的草稿")
	w.beginQueueComposerEdit(inputs[0])
	tt := ui.NewTester(w.view, 1080, 760)
	if _, ok := tt.Access("保存排队消息"); !ok {
		t.Fatal("queue edit did not use the main composer")
	}
	w.setDraft("t1", "改过的第一条")
	tt.Frame()
	mustClick(t, tt, "保存排队消息")
	if got := w.composerFor("t1").draft; got != "尚未发送的草稿" {
		t.Fatalf("original draft lost: %q", got)
	}
	if !w.moveInput(inputs[1], "up") {
		t.Fatal("move not dispatched")
	}
	if !w.promoteInput(inputs[1]) {
		t.Fatal("promote not dispatched")
	}
	commands := service.dispatched()
	if len(commands) != 3 {
		t.Fatalf("commands=%+v", commands)
	}
	if c := commands[0]; c.Kind != api.EditInput || c.InputID != "in-1" || c.Message != "改过的第一条" {
		t.Fatalf("edit=%+v", c)
	}
	if c := commands[1]; c.Kind != commandMoveInput || c.InputID != "in-2" || c.Value != "up" {
		t.Fatalf("move=%+v", c)
	}
	if c := commands[2]; c.Kind != api.PromoteInputToSteer || c.InputID != "in-2" {
		t.Fatalf("promotion must be one atomic command: %+v", c)
	}
	if _, editing := w.queue.editing["in-1"]; editing || w.queue.composer != nil {
		t.Fatal("editor stayed open after save")
	}
}

func TestRejectedAtomicPromotionDoesNotSendSeparateSteer(t *testing.T) {
	w, service, inputs := queueFixture()
	service.receipt = func(cmd api.Command) (api.Receipt, error) {
		return api.Receipt{CommandID: cmd.CommandID, Accepted: false}, nil
	}
	w.promoteInput(inputs[0])
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.PromoteInputToSteer {
		t.Fatalf("promotion sent separate commands: %+v", commands)
	}
}

func TestStructuredPluginPanelRendersNatively(t *testing.T) {
	output := `{"vantryPanel":{"title":"构建报告","subtitle":"main · 3 分钟前","nodes":[
		{"type":"badge","text":"通过","tone":"success"},{"type":"badge","text":"2 警告","tone":"warning"},
		{"type":"kv","items":[{"key":"耗时","value":"42s"},{"key":"覆盖率","value":81.5}]},
		{"type":"list","title":"变更","items":["desktop: 新面板","core: 文件读取"]},
		{"type":"markdown","markdown":"**下一步**：合并到 main"},
		{"type":"code","language":"bash","code":"go test ./..."},
		{"type":"card","title":"子项","tone":"info","children":[{"type":"text","text":"嵌套内容"}]}]}}`
	panel, ok := decodeVantryPanel(output)
	if !ok || panel.Title != "构建报告" || len(panel.Nodes) != 7 || panel.Nodes[2].Pairs[1][1] != "81.5" {
		t.Fatalf("decode=%+v ok=%v", panel, ok)
	}
	if _, ok := decodeVantryPanel(`{"other":1}`); ok {
		t.Fatal("non-panel JSON decoded as panel")
	}
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "q", "a", 0))
	w.features.panel = "plugins"
	w.features.pluginOutputs = map[string]string{"t1": output}
	tt := ui.NewTester(w.view, 1440, 900)
	for _, text := range []string{"构建报告", "通过", "耗时", "42s", "desktop: 新面板", "go test ./...", "嵌套内容"} {
		if !tt.HasText(text) {
			t.Fatalf("missing %q in %q", text, tt.Texts())
		}
	}
	if strings.Contains(strings.Join(tt.Texts(), "\n"), `"vantryPanel"`) {
		t.Fatal("raw JSON shown instead of the panel")
	}
	mustClick(t, tt, "将结果加入草稿")
	if draft := w.composerFor("t1").draft; !strings.Contains(draft, "耗时：42s") || strings.Contains(draft, "{") {
		t.Fatalf("draft=%q", draft)
	}
}

func TestRemoteFavoritesPersistWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w, _ := newViewWorkbench()
	w.defaults.StatePath = path
	w.features.remoteHost, w.features.remoteSocket = "devbox", "/run/vantry/core.sock"
	w.remote.alias = "开发机"
	if !w.saveRemoteFavorite() {
		t.Fatal("favorite not saved")
	}
	next, _ := newViewWorkbench()
	next.defaults.StatePath = path
	if err := next.restoreState(); err != nil {
		t.Fatal(err)
	}
	if len(next.remote.saved) != 1 || next.remote.saved[0] != (remoteFavorite{Name: "开发机", Host: "devbox", Socket: "/run/vantry/core.sock"}) {
		t.Fatalf("restored=%+v", next.remote.saved)
	}
	next.features.panel = "remote"
	tt := ui.NewTester(next.view, 1080, 760)
	mustClick(t, tt, "主机 开发机")
	if next.features.remoteHost != "devbox" || next.features.remoteSocket != "/run/vantry/core.sock" {
		t.Fatal("favorite did not fill the form")
	}
	next.removeHost(next.sshEntry("devbox"))
	if len(next.remote.saved) != 0 {
		t.Fatal("favorite not removed")
	}
	if envs := decodeEnvironments(api.WorkbenchResponse{Output: `{"environments":[{"name":"gpu","host":"gpu-1","socket":"/s"}]}`}); len(envs) != 1 || envs[0].Host != "gpu-1" {
		t.Fatalf("environments=%+v", envs)
	}
}

func TestStartupModelIsNotReplacedByPersistedChoice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w, _ := newViewWorkbench()
	w.defaults.StatePath = path
	w.defaults.Model = "manual/last-choice"
	w.saveState()
	next := newWorkbench(&fakeService{})
	next.defaults = Options{StatePath: path, ProviderID: "pi", Model: "startup/explicit"}
	if err := next.restoreState(); err != nil {
		t.Fatal(err)
	}
	if next.defaults.Model != "startup/explicit" {
		t.Fatalf("startup model replaced: %q", next.defaults.Model)
	}
}

func TestGitPushNeedsConfirmationAndPullIsFastForwardOnly(t *testing.T) {
	var requests []api.WorkbenchRequest
	state := &api.GitState{Branch: "feature", Branches: []string{"main", "feature"}, Files: []api.GitFile{}}
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		requests = append(requests, r)
		if r.Action == actionPRStatus {
			return api.WorkbenchResponse{Output: `{"number":42,"state":"open","checks":"success","url":"https://example.invalid/pr/42"}`}, nil
		}
		return api.WorkbenchResponse{Git: state}, nil
	}}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)}})
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "Git")
	tt.Frame()
	if !tt.HasText("PR #42") {
		t.Fatalf("PR badge missing: %q", tt.Texts())
	}
	requests = nil
	mustClick(t, tt, "Push")
	if len(requests) != 0 {
		t.Fatalf("push sent without confirmation: %+v", requests)
	}
	tt.Frame()
	mustClick(t, tt, "确认推送")
	if len(requests) < 1 || requests[0].Action != actionGitPush || requests[0].Branch != "feature" {
		t.Fatalf("push=%+v", requests)
	}
	requests = nil
	mustClick(t, tt, "Pull（仅快进）")
	if len(requests) < 1 || requests[0].Action != actionGitPull || requests[0].Message != "--ff-only" {
		t.Fatalf("pull=%+v", requests)
	}
	mustClick(t, tt, "PR #42 检查通过")
	if tt.Clipboard() != "https://example.invalid/pr/42" {
		t.Fatalf("clipboard=%q", tt.Clipboard())
	}
}

// TestUpgradedPanelsRenderOffscreen renders every new surface with MyGo's
// real offscreen renderer at the three review sizes. It always runs (layout
// must not panic); set VANTRY_DESKTOP_RENDER_DIR to export PNGs.
func TestUpgradedPanelsRenderOffscreen(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output != "" {
		if err := os.MkdirAll(output, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	specs := []struct {
		name          string
		width, height int
		dark          bool
	}{{"1440-light", 1440, 900, false}, {"1080-dark", 1080, 700, true}, {"640-narrow", 640, 600, false}}
	scenarios := []struct {
		name  string
		setup func(w *workbench)
		want  string
	}{
		{"files", func(w *workbench) {
			w.features.panel = "files"
			scope, _, _ := w.fileScope("t1")
			w.files.listings = map[string][]fileEntry{listingKey(scope, "internal/desktop"): {
				{Name: "icons", Path: "internal/desktop/icons", Dir: true},
				{Name: "files.go", Path: "internal/desktop/files.go"},
				{Name: "view.go", Path: "internal/desktop/view.go"},
			}}
			w.files.dir = map[string]string{"t1": "internal/desktop"}
			w.files.open = map[string]string{"t1": "internal/desktop/view.go"}
			buffer := editor.NewBuffer("package desktop\n\nimport \"github.com/egoist/mygo/ui\"\n\n// palette defines the quiet surface scale.\ntype palette struct {\n\tcanvas, surface ui.Color\n}\n")
			buffer.SetLanguage(editor.LanguageFor("view.go"))
			w.files.views = map[string]*editor.State{listingKey(scope, "internal/desktop/view.go"): {Buffer: buffer, ReadOnly: true, NoFolding: true}}
		}, "引用到输入框"},
		{"queue-edit", func(w *workbench) {
			w.runs[0].Status = api.RunRunning
			w.inputs = append(w.inputs,
				api.Input{ID: "q1", ThreadID: "t1", Kind: "queue", Status: "queued", Message: "完成后再跑一次完整测试", CreatedAt: baseTime},
				api.Input{ID: "q2", ThreadID: "t1", Kind: "queue", Status: "queued", Message: "然后整理一份变更说明", CreatedAt: baseTime.Add(time.Second)})
			w.widgets.queueOpen = true
			w.beginQueueComposerEdit(w.inputs[len(w.inputs)-1])
		}, "编辑排队消息"},
		{"file-picker", func(w *workbench) {
			scope, _, _ := w.fileScope("t1")
			w.picker.results = map[string][]string{pickerKey(scope, "view"): {"internal/desktop/view.go", "internal/desktop/composer_view.go", "internal/desktop/controls_view.go"}}
			w.setDraft("t1", "请对照 @view")
		}, "composer_view.go"},
		{"shortcuts", func(w *workbench) { w.widgets.shortcutsOpen = true }, "快捷键速查"},
		{"plugin-panel", func(w *workbench) {
			w.features.panel = "plugins"
			w.features.pluginOutputs = map[string]string{"t1": `{"vantryPanel":{"title":"项目体检","subtitle":"Vantry · desktop-interactions","nodes":[{"type":"badge","text":"测试通过","tone":"success"},{"type":"badge","text":"1 个提醒","tone":"warning"},{"type":"kv","items":[{"key":"Go 文件","value":"64"},{"key":"测试","value":"205 通过"}]},{"type":"list","title":"建议","items":["补齐文件面板的空态文案","Push 前确认远程分支"]},{"type":"code","language":"bash","code":"go test -race ./internal/desktop/..."}]}}`}
		}, "项目体检"},
		{"remote-favorites", func(w *workbench) {
			w.features.panel = "remote"
			w.remote.saved = []remoteFavorite{{Name: "开发机", Host: "devbox", Socket: "/run/vantry/core.sock"}, {Name: "GPU", Host: "gpu-1", Socket: "/srv/core.sock"}}
			w.features.remoteHost, w.features.remoteSocket = "devbox", "/run/vantry/core.sock"
		}, "开发机"},
		{"git-sync", func(w *workbench) {
			w.features.panel = "git"
			w.gitView.pushConfirm = "t1"
			w.gitView.prLoaded = map[string]bool{"t1": true}
			w.gitView.pr = map[string]*prStatus{"t1": {Number: 128, State: "open", Checks: "pending", URL: "https://example.invalid/pr/128"}}
			w.runs[0].Status = api.RunCompleted
		}, "确认推送"},
		{"project-filter", func(w *workbench) {
			w.runs[0].Status = api.RunCompleted
			w.widgets.projectFilter = "/Projects/Notes"
			w.sessions.unread = map[string]bool{"child": true}
			w.widgets.search = ""
		}, "资料整理与项目笔记"},
	}
	for _, spec := range specs {
		for _, scenario := range scenarios {
			t.Run(spec.name+"-"+scenario.name, func(t *testing.T) {
				w := nativeReviewWorkbench()
				w.composerFor("t1").attachments = nil
				scenario.setup(w)
				if spec.width < narrowWidth && scenario.name == "project-filter" {
					w.widgets.mobileNav = true
				}
				tt := ui.NewTesterWith(w.view, spec.width, spec.height, ui.TesterOptions{Dark: spec.dark})
				tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
				tt.Advance(time.Second)
				if !tt.HasText(scenario.want) {
					t.Fatalf("%s: %q not rendered: %q", scenario.name, scenario.want, tt.Texts())
				}
				modal := scenario.name == "shortcuts"
				sidebarOnly := spec.width < narrowWidth && scenario.name == "project-filter"
				if !modal && !sidebarOnly {
					assertControlInWindow(t, tt, "消息输入框", spec.width, spec.height)
				}
				if output == "" {
					return
				}
				file, err := os.Create(filepath.Join(output, "upgrade-"+spec.name+"-"+scenario.name+".png"))
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

func TestDecodeFileEntriesAcceptsCommonShapes(t *testing.T) {
	for name, output := range map[string]string{
		"lines":   "a.go\nsub/\n",
		"strings": `["a.go","sub/"]`,
		"objects": `{"entries":[{"name":"a.go","path":"a.go"},{"name":"sub","path":"sub","isDir":true}]}`,
	} {
		entries, ok := decodeFileEntries(api.WorkbenchResponse{Output: output})
		if !ok || len(entries) != 2 {
			t.Fatalf("%s: %+v", name, entries)
		}
		sortEntries(entries)
		if !entries[0].Dir || entries[0].Path != "sub" || entries[1].Path != "a.go" {
			t.Fatalf("%s sorted=%+v", name, entries)
		}
	}
	if got := decodeFileContent(api.WorkbenchResponse{Output: `{"content":"hello"}`}); got != "hello" {
		t.Fatalf("content=%q", got)
	}
	raw, _ := json.Marshal(map[string]string{"text": "x"})
	if got := decodeFileContent(api.WorkbenchResponse{Data: raw}); got != "x" {
		t.Fatalf("data content=%q", got)
	}
}
