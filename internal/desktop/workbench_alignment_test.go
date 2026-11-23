package desktop

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// projectWorkbench is two projects' threads, each with a branch, so their
// sidebar rows are the workbench's three-line cards.
func projectWorkbench() (*workbench, *fakeService) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry", Branch: "main"}
	second := thread("t2", "第二条线程", time.Minute)
	second.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/stock-risk-agent", Branch: "dev"}
	third := thread("thread-c", "第三条线程", 2*time.Minute)
	third.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry", Branch: "feature"}
	w.mergeThread(second)
	w.mergeThread(third)
	w.mergeRun(run("r2", "t2", api.RunCompleted, "第二个问题", "第二个回答", time.Minute))
	w.mergeRun(run("r3", "thread-c", api.RunCompleted, "第三个问题", "第三个回答", 2*time.Minute))
	return w, service
}

func hoverRow(t *testing.T, tt *ui.Tester, name string) ui.Rect {
	t.Helper()
	row, ok := tt.AccessByRole(ui.RoleTreeItem, name)
	if !ok {
		t.Fatalf("row %q missing: %q", name, tt.Texts())
	}
	tt.Move(row.Bounds.X+row.Bounds.W/2, row.Bounds.Y+row.Bounds.H/2)
	tt.Frame()
	return row.Bounds
}

func TestProjectInitialsAndBadgeColorsAreStable(t *testing.T) {
	for name, want := range map[string]string{"Vantry": "VY", "stock-risk-agent": "SR", "FrameTrail": "FL", "notes_app": "NA", "x": "X", "": "?"} {
		if got := projectInitials(name); got != want {
			t.Errorf("projectInitials(%q) = %q, want %q", name, got, want)
		}
	}
	if badgeColor("Vantry") != badgeColor("Vantry") {
		t.Fatal("badge color must be deterministic")
	}
}

func TestSidebarRowsAreThreeLineBlocks(t *testing.T) {
	w, _ := projectWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	row, ok := tt.AccessByRole(ui.RoleTreeItem, "第二条线程")
	if !ok || row.Bounds.H < rowHeight-2 || row.Bounds.H > rowHeight+2 {
		t.Fatalf("thread row must be the list's %dpx block: %+v %v", rowHeight, row, ok)
	}
	title, ok := tt.Find("第二条线程")
	if !ok || title.Y < row.Bounds.Y || title.Y > row.Bounds.Y+row.Bounds.H-title.H {
		t.Fatalf("title must sit inside its row: title %+v row %+v", title, row.Bounds)
	}
	if !strings.Contains(row.Description, "dev") {
		t.Fatalf("row lacks its branch: %q", row.Description)
	}
	for _, text := range []string{"SR", "stock-risk-agent", "VY", "Vantry"} {
		if !tt.HasText(text) {
			t.Errorf("row meta missing %q: %q", text, tt.Texts())
		}
	}
	// The default dark theme is neutral Graphite with its blue accent.
	_, p := workbenchTheme(true, true, true)
	if p.preset != "graphite" || p.accent != ui.Hex("#346BF1") {
		t.Fatalf("dark palette = preset %q selected %v accent %v", p.preset, p.selected, p.accent)
	}
}

func TestSidebarHoverSettleIsOneClick(t *testing.T) {
	w, service := projectWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	if _, ok := tt.Find("归档"); ok {
		t.Fatal("hover actions must stay hidden until the row is hovered")
	}
	hoverRow(t, tt, "第二条线程")
	if w.widgets.hoverThread != scopedSessionID("local", "t2") {
		t.Fatalf("hover card target = %q", w.widgets.hoverThread)
	}
	settle, ok := tt.Find("归档")
	row, _ := tt.AccessByRole(ui.RoleTreeItem, "第二条线程")
	if !ok || settle.Y > row.Bounds.Y+24 || settle.X < row.Bounds.X+row.Bounds.W/2 {
		t.Fatalf("Settle must sit at the row's top right: settle=%+v row=%+v", settle, row.Bounds)
	}
	mustClick(t, tt, "归档")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.UpdateThread || commands[0].ThreadID != "t2" || commands[0].Archived == nil || !*commands[0].Archived {
		t.Fatalf("settle = %+v", commands)
	}
}

func TestSidebarHoverSnoozeAndWake(t *testing.T) {
	w, service := projectWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	hoverRow(t, tt, "第二条线程")
	before := time.Now()
	mustClick(t, tt, "稍后提醒")
	mustClick(t, tt, "1 小时后")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].ThreadID != "t2" || commands[0].SnoozedUntil == nil {
		t.Fatalf("snooze = %+v", commands)
	}
	if until := *commands[0].SnoozedUntil; until.Before(before.Add(59*time.Minute)) || until.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("snooze until %v is not an hour away", until)
	}
	later := time.Now().Add(time.Hour)
	w.threads[slices.IndexFunc(w.threads, func(th api.Thread) bool { return th.ID == "t2" })].SnoozedUntil = &later
	tt.Frame()
	hoverRow(t, tt, "第二条线程")
	mustClick(t, tt, "稍后提醒")
	mustClick(t, tt, "取消提醒")
	commands = service.dispatched()
	if len(commands) != 2 || commands[1].SnoozedUntil == nil || !commands[1].SnoozedUntil.IsZero() {
		t.Fatalf("wake = %+v", commands)
	}
}

func TestSettledRowHoverUnsettles(t *testing.T) {
	w, service := projectWorkbench()
	w.threads[slices.IndexFunc(w.threads, func(th api.Thread) bool { return th.ID == "t2" })].Archived = true
	*w.projectExpanded(sectionSettled) = true
	tt := ui.NewTester(w.view, 1440, 900)
	hoverRow(t, tt, "第二条线程")
	mustClick(t, tt, "移回")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Archived == nil || *commands[0].Archived {
		t.Fatalf("unsettle = %+v", commands)
	}
}

func TestDraftRowHoverDiscards(t *testing.T) {
	w, _ := projectWorkbench()
	draft := w.newSession()
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1440, 900)
	hoverRow(t, tt, "新会话")
	mustClick(t, tt, "丢弃草稿")
	if slices.Contains(w.localOrder, draft) || w.composers[draft] != nil {
		t.Fatalf("draft not discarded: order=%v", w.localOrder)
	}
}

func TestSidebarBackForwardFollowVisitedThreads(t *testing.T) {
	w, _ := projectWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	if info, _ := tt.Access("后退"); !info.Disabled {
		t.Fatal("back must start disabled")
	}
	mustClick(t, tt, "第二条线程")
	mustClick(t, tt, "第三条线程")
	if w.selected != "thread-c" {
		t.Fatalf("selected %q", w.selected)
	}
	mustClick(t, tt, "后退")
	if w.selected != "t2" {
		t.Fatalf("back went to %q", w.selected)
	}
	mustClick(t, tt, "后退")
	if w.selected != "t1" {
		t.Fatalf("second back went to %q", w.selected)
	}
	mustClick(t, tt, "前进")
	if w.selected != "t2" {
		t.Fatalf("forward went to %q", w.selected)
	}
	// A new visit drops the forward history, as a browser's does.
	mustClick(t, tt, "第一条线程")
	if w.canGoForward() {
		t.Fatal("forward history survived a new visit")
	}
}

func TestSidebarProjectFilter(t *testing.T) {
	w, _ := projectWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "按项目筛选")
	item, ok := tt.AccessByRole(ui.RoleMenuItemRadio, "stock-risk-agent")
	if !ok {
		t.Fatalf("filter menu lacks the project: %q", tt.Texts())
	}
	tt.ClickAt(item.Bounds.X+item.Bounds.W/2, item.Bounds.Y+item.Bounds.H/2)
	tt.Frame()
	if w.widgets.projectFilter != "/Projects/stock-risk-agent" {
		t.Fatalf("filter = %q", w.widgets.projectFilter)
	}
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "第三条线程"); ok {
		t.Fatal("another project's thread is still listed")
	}
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "第二条线程"); !ok {
		t.Fatal("the filtered project's thread is missing")
	}
	mustClick(t, tt, "按项目筛选")
	mustClick(t, tt, "全部项目")
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "第三条线程"); !ok || w.widgets.projectFilter != "" {
		t.Fatal("All projects did not clear the filter")
	}
}

func TestSearchBarActions(t *testing.T) {
	w, _ := projectWorkbench()
	picked := 0
	w.pickDirectory = func() { picked++ }
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "添加项目")
	if picked != 1 {
		t.Fatal("Add project did not open the folder picker")
	}
	w.pickDirectory = nil
	mustClick(t, tt, "添加项目")
	if !w.widgets.projectPickerOpen && !w.remoteDirectories.open {
		t.Fatal("Add project without a native picker must open the project chooser")
	}
	tt.Frame()
	tt.Key(0, ui.KeyEscape)
	w.remoteDirectories.open, w.widgets.projectPickerOpen = false, false
	tt.Frame()
	search, _ := tt.Access("搜索会话")
	add, _ := tt.Access("添加项目")
	compose, _ := tt.Access("新会话")
	if !(search.Bounds.X < add.Bounds.X && add.Bounds.X < compose.Bounds.X) || overlaps(search.Bounds, add.Bounds) {
		t.Fatalf("search bar order: search=%+v add=%+v new=%+v", search.Bounds, add.Bounds, compose.Bounds)
	}
}

func TestThreadContextMenuActions(t *testing.T) {
	w, _ := projectWorkbench()
	w.sessions.unread = nil
	tt := ui.NewTester(w.view, 1440, 900)
	if err := tt.RightClick("第二条线程"); err != nil {
		t.Fatal(err)
	}
	menu := strings.Join(tt.Menu(), "|")
	order := []string{"在会话内查找", "在此项目新建会话", "置顶会话", "归档会话", "稍后提醒", "重命名会话", "重新生成标题", "标为未读", "从此处分叉会话", "按此项目筛选", "复制", "在终端中打开", "删除"}
	at := -1
	for _, item := range order {
		i := strings.Index(menu, item)
		if i <= at {
			t.Fatalf("menu item %q missing or out of order in %q", item, menu)
		}
		at = i
	}
	if strings.Contains(menu, "打开项目聊天") {
		t.Fatalf("menu unexpectedly retained project chat item: %q", menu)
	}
	if strings.Contains(menu, "删除会话…") || strings.Contains(menu, "…") && strings.Contains(menu, "删除…") {
		t.Fatalf("delete still asks for more: %q", menu)
	}
	if err := tt.ChooseMenuItem("复制", "会话链接"); err != nil {
		t.Fatalf("copy link: %v %q", err, tt.Menu())
	}
	if got := tt.Clipboard(); got != api.FormatThreadLink("t2", "第二条线程") {
		t.Fatalf("thread link = %q", got)
	}
}

func TestCopiedThreadLinkNamesOnlyTheSession(t *testing.T) {
	w, _ := projectWorkbench()
	chat := thread("thread-chat", "项目协调员 · Vantry", 3*time.Minute)
	chat.ProjectPath = "/Projects/Vantry"
	chat.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry"}
	w.mergeThread(chat)
	tt := ui.NewTester(w.view, 1440, 900)
	if err := tt.RightClick("项目协调员 · Vantry"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("复制", "会话链接"); err != nil {
		t.Fatalf("copy link: %v %q", err, tt.Menu())
	}
	if got, want := tt.Clipboard(), "[项目协调员](vantry-thread://thread-chat)"; got != want {
		t.Fatalf("thread link = %q, want %q", got, want)
	}
}

func TestInlineRenameFromContextMenu(t *testing.T) {
	w, service := projectWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	if err := tt.RightClick("第二条线程"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("重命名会话"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if w.sessions.renamingID != "t2" || !tt.Focused("重命名会话") {
		t.Fatalf("inline rename not started: id=%q", w.sessions.renamingID)
	}
	for range []rune("第二条线程") {
		tt.Key(0, ui.KeyBackspace)
	}
	tt.Type("改名后的会话")
	tt.Key(0, ui.KeyEnter)
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.UpdateThread || commands[0].ThreadID != "t2" || commands[0].Title != "改名后的会话" {
		t.Fatalf("rename = %+v", commands)
	}
	if w.sessions.renamingID != "" {
		t.Fatal("rename stayed open after Enter")
	}
}

func TestContextMenuMarksUnreadAndRegeneratesTitle(t *testing.T) {
	w, service := projectWorkbench()
	w.runs[slices.IndexFunc(w.runs, func(r api.Run) bool { return r.ID == "r2" })].Input = "整理   第二个项目的风险模型\n并输出结论"
	w.sessions.unread = nil
	tt := ui.NewTester(w.view, 1440, 900)
	if err := tt.RightClick("第二条线程"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("标为未读"); err != nil {
		t.Fatal(err)
	}
	if !w.isUnread("t2") {
		t.Fatal("mark unread did nothing")
	}
	if err := tt.RightClick("第二条线程"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("重新生成标题"); err != nil {
		t.Fatal(err)
	}
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Title != "整理 第二个项目的风险模型 并输出结论" {
		t.Fatalf("regenerated title = %+v", commands)
	}
}

func TestFooterIsIconOnlyWithoutLocaleCapsule(t *testing.T) {
	w, _ := projectWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	for _, name := range []string{"设置", "Git 面板", "用量", "切换深色"} {
		info, ok := tt.AccessByRole(ui.RoleButton, name)
		if !ok || info.Bounds.W > 30 {
			t.Errorf("footer %q must be a compact icon button: %+v %v", name, info, ok)
		}
	}
	if tt.HasText("中") || tt.HasText("EN") {
		t.Fatalf("language capsule left in the footer: %q", tt.Texts())
	}
	mustClick(t, tt, "Git 面板")
	if w.features.panel != "git" {
		t.Fatalf("footer Git opened %q", w.features.panel)
	}
}

func TestSenderBadgeOpensSenderThread(t *testing.T) {
	w, _ := projectWorkbench()
	incoming := run("r-in", "t1", api.RunCompleted, "请帮我核对风险报告", "已核对", 3*time.Minute)
	incoming.SenderThreadID = "t2"
	w.mergeRun(incoming)
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	mustClick(t, tt, "来自会话：第二条线程")
	if w.selected != "t2" {
		t.Fatalf("sender badge opened %q", w.selected)
	}
}

func TestThreadLinksOpenInPlace(t *testing.T) {
	w, _ := projectWorkbench()
	w.runs[0].Output = "结果见 [第三条线程](vantry-thread://thread-c) 和 [文档](https://example.invalid/doc)。"
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	if _, ok := tt.Find("第三条线程"); !ok {
		t.Fatalf("link text missing: %q", tt.Texts())
	}
	w.replyLink("vantry-thread://thread-c")
	if w.selected != "thread-c" {
		t.Fatalf("thread link opened %q", w.selected)
	}
	if len(tt.OpenedURLs()) != 0 {
		t.Fatalf("a thread link left the app: %q", tt.OpenedURLs())
	}
	w.replyLink("vantry-thread://missing")
	if w.selected != "thread-c" || !strings.Contains(w.actionNotice.Text, "找不到") {
		t.Fatalf("missing thread link: selected=%q notice=%q", w.selected, w.actionNotice.Text)
	}
	w.replyLink("https://example.invalid/doc")
	tt.Frame()
	if urls := tt.OpenedURLs(); len(urls) != 1 || urls[0] != "https://example.invalid/doc" {
		t.Fatalf("web link = %q", urls)
	}
}

func TestRunningSubtaskCardCancelsInOneClick(t *testing.T) {
	w, service := projectWorkbench()
	child := thread("child", "子任务：核对数据", 4*time.Minute)
	child.ParentThreadID, child.ParentTaskID = "t1", "task-1"
	w.mergeThread(child)
	w.tasks = []api.Task{{ID: "task-1", ParentThreadID: "t1", ParentRunID: "r1", ChildThreadID: "child", Title: "核对数据", Status: api.RunRunning, Generation: 1}}
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	mustClick(t, tt, "取消子任务")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.CancelTask || commands[0].TaskID != "task-1" || commands[0].ThreadID != "t1" {
		t.Fatalf("cancel = %+v", commands)
	}
	w.tasks[0].Status = api.RunCompleted
	tt.Frame()
	if _, ok := tt.Access("取消子任务"); ok {
		t.Fatal("a finished subtask still offers Cancel")
	}
	mustClick(t, tt, "打开子任务")
	if w.selected != "t1" {
		t.Fatalf("opening a subagent switched the main thread to %q", w.selected)
	}
	if !w.subagentPanelShows("child") {
		t.Fatal("opening a subagent did not show it in the side panel")
	}
}

func TestPeerThreadCardOpens(t *testing.T) {
	w, _ := projectWorkbench()
	peer := thread("peer", "并行调研", 5*time.Minute)
	peer.CreatedByThreadID, peer.CreatedByRunID = "t1", "r1"
	w.mergeThread(peer)
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	mustClick(t, tt, "打开会话 并行调研")
	if w.selected != "peer" {
		t.Fatalf("peer open = %q", w.selected)
	}
}

func TestChildPendingApprovalSurfacesInParent(t *testing.T) {
	w, service := projectWorkbench()
	child := thread("child", "执行迁移", 4*time.Minute)
	child.ParentThreadID = "t1"
	w.mergeThread(child)
	w.mergeRun(run("rc", "child", api.RunRunning, "迁移", "", 4*time.Minute))
	w.requests = []api.ProviderRequest{{ID: "req-1", ThreadID: "child", RunID: "rc", Method: "confirm", Title: "运行 rm -rf build/", Status: "pending"}}
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1440, 900)
	if !tt.HasText("子任务「执行迁移」正在等待权限确认") || !tt.HasText("运行 rm -rf build/") {
		t.Fatalf("child approval not surfaced: %q", tt.Texts())
	}
	mustClick(t, tt, "允许执行")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.RespondRequest || commands[0].ThreadID != "child" || commands[0].RequestID != "req-1" || !commands[0].Confirmed {
		t.Fatalf("allow = %+v", commands)
	}
	mustClick(t, tt, "查看子会话")
	if w.selected != "t1" || !w.subagentPanelShows("child") {
		t.Fatalf("open child selected=%q panel=%v", w.selected, w.subagentPanelShows("child"))
	}
}

func TestModelPickerHasPiMarkAndQuietReload(t *testing.T) {
	w := nativeReviewWorkbench()
	w.runs[0].Status = api.RunCompleted
	w.widgets.modelPickerOpen = true
	tt := ui.NewTester(w.view, 1440, 900)
	for _, text := range []string{"当前目录包含 Pi 内置模型和已配置的可用模型；由扩展注册的模型暂不可用。", "应用于后续消息", "已配置的默认模型为"} {
		if tt.HasText(text) {
			t.Errorf("picker hint %q is back", text)
		}
	}
	header, ok := tt.Access("重新载入")
	count, _ := tt.Find("Pi 模型列表 · 2 个")
	if !ok || header.Bounds.W > 24 || header.Bounds.Y > count.Y+count.H || header.Bounds.Y+header.Bounds.H < count.Y {
		t.Fatalf("reload must be a quiet icon beside the count: reload=%+v count=%+v", header.Bounds, count)
	}
}

func TestThinkingLevelsPreloadForStandaloneButton(t *testing.T) {
	requests := 0
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action != "models" {
			return api.WorkbenchResponse{}, fmt.Errorf("unexpected %s", r.Action)
		}
		requests++
		return api.WorkbenchResponse{Catalog: &api.ModelCatalog{Default: "pi/model", Models: []api.ModelInfo{{Provider: "pi", ID: "model", Name: "Pi Model"}}, ThinkingLevels: []string{"low", "medium", "high"}}}, nil
	}}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)}})
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1080, 760)
	tt.Frame()
	if requests != 1 {
		t.Fatalf("model details preload requests = %d", requests)
	}
	if _, ok := tt.Access("思考强度"); !ok {
		t.Fatalf("thinking button missing after preload: %q", tt.Texts())
	}
	tt.Frame()
	if requests != 1 {
		t.Fatalf("preload repeated: %d", requests)
	}
	mustClick(t, tt, "思考强度")
	if !tt.HasText("中等") || !tt.HasText("默认") {
		t.Fatalf("level menu = %q", tt.Texts())
	}
}

func TestHomeHeroAndProjectCard(t *testing.T) {
	w, _ := projectWorkbench()
	w.detectedEditors = []editorOption{
		{ID: "finder", Name: "Finder", ZHName: "访达", JAName: "Finder", Icon: folderIcon, IsFileManager: true},
	}
	w.selectThread("t1")
	w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry"}
	w.newSession()
	tt := ui.NewTester(w.view, 1440, 900)
	if !tt.HasText("想在 Vantry 构建什么？") {
		t.Fatalf("hero missing: %q", tt.Texts())
	}
	for _, name := range []string{"项目卡片", "在访达中打开", "在终端中打开", "提交并推送", "变更"} {
		assertControlInWindow(t, tt, name, 1440, 900)
	}
	card, _ := tt.Access("项目卡片")
	if card.Bounds.X+card.Bounds.W < 1440-40 || card.Bounds.Y > 80 {
		t.Fatalf("project card must float at the top right: %+v", card.Bounds)
	}
	mustClick(t, tt, "或不关联项目直接开始")
	if w.defaults.Workspace != nil {
		t.Fatalf("start without a project kept %+v", w.defaults.Workspace)
	}
	tt.Frame()
	if !tt.HasText("今天想构建什么？") {
		t.Fatalf("chat-only hero missing: %q", tt.Texts())
	}
}

func TestBreadcrumbShowsProjectBadgeAndOrigin(t *testing.T) {
	w, _ := projectWorkbench()
	child := thread("peer", "并行调研", 5*time.Minute)
	child.CreatedByThreadID = "t2"
	child.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry"}
	w.mergeThread(child)
	w.mergeRun(run("rp", "peer", api.RunCompleted, "调研", "完成", 5*time.Minute))
	w.selectThread("peer")
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "来源会话")
	if w.selected != "t2" {
		t.Fatalf("origin chip opened %q", w.selected)
	}
}

// TestWorkbenchAlignmentRenders exports the hover card, menus and composer for review.
func TestWorkbenchAlignmentRenders(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR to export native review images")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{1440, 900}, {1080, 760}, {640, 600}} {
		for _, dark := range []bool{true, false} {
			for _, scene := range []string{"hover", "models", "approval", "home", "rename"} {
				theme := "light"
				if dark {
					theme = "dark"
				}
				name := fmt.Sprintf("workbench-%d-%s-%s", size[0], theme, scene)
				t.Run(name, func(t *testing.T) {
					w := nativeReviewWorkbench()
					w.widgets.themeSet, w.widgets.dark = true, dark
					w.runs[0].Status = api.RunCompleted
					second := thread("risk", "梳理当前进度并拉取最新代码", 20*time.Hour)
					second.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/stock-risk-agent", Branch: "main"}
					w.mergeThread(second)
					w.mergeRun(run("r-risk", "risk", api.RunCompleted, "梳理", "完成", 20*time.Hour))
					switch scene {
					case "models":
						w.widgets.modelPickerOpen = true
					case "approval":
						w.mergeRun(run("rc", "child", api.RunRunning, "迁移", "", 4*time.Minute))
						w.requests = []api.ProviderRequest{{ID: "req-1", ThreadID: "child", RunID: "rc", Method: "confirm", Title: "运行 go test ./...", Status: "pending"}}
						w.tasks = []api.Task{{ID: "task-1", ParentThreadID: "t1", ParentRunID: "review-run", ChildThreadID: "child", Title: "检查附件与输入法边界", Status: api.RunRunning, Generation: 1}}
					case "home":
						w.newSession()
					case "rename":
						w.beginRename("risk")
					}
					tt := ui.NewTesterWith(w.view, size[0], size[1], ui.TesterOptions{Dark: dark})
					tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
					tt.Advance(time.Second)
					if scene == "hover" && size[0] >= narrowWidth {
						hoverRow(t, tt, "梳理当前进度并拉取最新代码")
						tt.Advance(time.Second)
					}
					file, err := os.Create(filepath.Join(output, name+".png"))
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					if err := png.Encode(file, tt.Image()); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
