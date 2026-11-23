package desktop

import (
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func navigationIDs(groups []navigationGroup) map[string][]string {
	out := map[string][]string{}
	for _, group := range groups {
		out[group.key] = append([]string(nil), group.ids...)
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestSubagentSidebarHidesChildrenOfExistingParents(t *testing.T) {
	w := newWorkbench(&fakeService{})
	parent := thread("parent", "父会话", 0)
	parent.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/proj/a"}
	child := thread("child", "子代理密钥旋转", time.Minute)
	child.ParentThreadID = "parent"
	child.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/proj/a"}
	pinned := thread("pinned-child", "置顶子代理", 2*time.Minute)
	pinned.ParentThreadID = "parent"
	pinned.Pinned = true
	orphan := thread("orphan", "孤儿子代理", 3*time.Minute)
	orphan.ParentThreadID = "missing-parent"
	pinned.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/proj/a"}
	taskChild := thread("task-child", "仅任务关联", 4*time.Minute)
	taskChild.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/proj/b"}
	w.applySnapshot(api.Snapshot{
		Sequence: 1,
		Threads:  []api.Thread{parent, child, pinned, orphan, taskChild},
		Runs: []api.Run{
			run("rp", "parent", api.RunCompleted, "父指令", "父回复", 0),
			run("rc", "child", api.RunRunning, "旋转密钥", "进行中", time.Minute),
			run("rpin", "pinned-child", api.RunCompleted, "置顶的子任务", "完成", 2*time.Minute),
			run("ro", "orphan", api.RunCompleted, "没有父线程", "仍应单独列出", 3*time.Minute),
		},
	})
	w.tasks = []api.Task{{
		ID: "task-only", ParentThreadID: "parent", ChildThreadID: "task-child", Title: "仅任务关联", Status: api.RunQueued,
	}}
	w.selectThread("parent")

	groups := navigationIDs(w.navigationGroupsFor("", ""))
	active := append(append([]string{}, groups[sectionActive]...), groups[sectionPinned]...)
	if containsID(active, "child") || containsID(active, "pinned-child") || containsID(groups[sectionActive], "task-child") || containsID(groups[sectionPinned], "pinned-child") {
		t.Fatalf("subagents listed beside a live parent: %#v", groups)
	}
	if !containsID(groups[sectionActive], "parent") || !containsID(groups[sectionActive], "orphan") {
		t.Fatalf("parent or true orphan missing: %#v", groups)
	}

	for i := range w.threads {
		if w.threads[i].ID == "parent" {
			w.threads[i].Archived = true
		}
	}
	groups = navigationIDs(w.navigationGroupsFor("", ""))
	if containsID(groups[sectionActive], "child") || containsID(groups[sectionPinned], "pinned-child") {
		t.Fatalf("archived parent's children became rows: %#v", groups)
	}
	if !containsID(groups[sectionSettled], "parent") {
		t.Fatalf("archived parent missing from settled shelf: %#v", groups)
	}

	w.threads[0].Archived = false
	filtered := navigationIDs(w.navigationGroupsFor("", "/proj/b"))
	flat := append(append([]string{}, filtered[sectionActive]...), filtered[sectionPinned]...)
	if containsID(flat, "task-child") || containsID(flat, "child") || containsID(flat, "parent") {
		t.Fatalf("project filter listed a subagent or the other project: %#v", filtered)
	}

	searched := navigationIDs(w.navigationGroupsFor("密钥", ""))
	found := append(append([]string{}, searched[sectionActive]...), searched[sectionPinned]...)
	if containsID(found, "child") || !containsID(searched[sectionActive], "parent") {
		t.Fatalf("search hit on a subagent = %#v", searched)
	}
	note := w.subagentSearchNote("parent", "密钥")
	if !strings.Contains(note, "子代理密钥旋转") {
		t.Fatalf("parent annotation = %q", note)
	}

	w.requests = []api.ProviderRequest{{ID: "req", ThreadID: "child", Status: "pending"}}
	if !w.familyNeedsYou("parent") {
		t.Fatal("parent row does not surface a child approval")
	}
	live, total := w.threadSubagentCounts("parent")
	if live == 0 || total < 2 {
		t.Fatalf("parent subagent counts = %d/%d", live, total)
	}

	w.frame.valid = true
	cached := navigationIDs(w.cachedNavigationGroups())
	cachedFlat := append(append([]string{}, cached[sectionActive]...), cached[sectionPinned]...)
	if containsID(cachedFlat, "child") || containsID(cachedFlat, "task-child") || !containsID(cached[sectionActive], "orphan") {
		t.Fatalf("cached sidebar listed subagents: %#v", cached)
	}
}

func TestSubagentPanelOpensWithoutSwitchingSelection(t *testing.T) {
	w, _ := projectWorkbench()
	child := thread("child", "子任务：核对数据", 4*time.Minute)
	child.ParentThreadID = "t1"
	child.Model = "fixture/model"
	w.mergeThread(child)
	w.mergeRun(run("rc", "child", api.RunRunning, "核对数据", "仍在读取边界。", 4*time.Minute))
	now := time.Now()
	w.tasks = []api.Task{{
		ID: "task-1", ParentThreadID: "t1", ParentRunID: "r1", ChildThreadID: "child",
		Title: "核对数据", Status: api.RunRunning, Generation: 1, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now,
	}}
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	if !tt.HasText("1 个子代理 · 0 完成") {
		t.Fatalf("subagent group header missing: %v", tt.Texts())
	}
	mustClick(t, tt, "打开子任务")
	if w.selected != "t1" {
		t.Fatalf("selected = %q", w.selected)
	}
	if !w.subagentPanelShows("child") || !tt.HasText("在主视图打开") {
		t.Fatalf("panel open=%v texts=%v", w.subagentPanelShows("child"), tt.Texts())
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if w.subagentPanelOpen() {
		t.Fatal("escape left the subagent panel open")
	}
}

func TestSubagentPanelSendTargetsChildThread(t *testing.T) {
	w, service := projectWorkbench()
	child := thread("child", "子任务：核对数据", 4*time.Minute)
	child.ParentThreadID = "t1"
	child.ProviderID = "pi"
	w.mergeThread(child)
	w.mergeRun(run("rc", "child", api.RunCompleted, "核对数据", "已核对。", 4*time.Minute))
	w.selectThread("t1")
	w.setDraft("t1", "父草稿留着")
	w.setDraft("child", "继续检查边界")
	w.openSubagentPanel("child")
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	mustClick(t, tt, "发送给子代理")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.SendMessage || commands[0].ThreadID != "child" || !strings.Contains(commands[0].Message, "继续检查边界") {
		t.Fatalf("send = %+v", commands)
	}
	if w.selected != "t1" || w.composerFor("t1").draft != "父草稿留着" {
		t.Fatalf("selected=%q parent draft=%q", w.selected, w.composerFor("t1").draft)
	}
	if w.composerFor("child").draft != "" {
		t.Fatalf("child draft was not consumed: %q", w.composerFor("child").draft)
	}
	w.composerFor("child").pending = nil

	running := run("rc2", "child", api.RunRunning, "下一轮", "", 5*time.Minute)
	running.AttemptID = "attempt-1"
	w.mergeRun(running)
	w.setDraft("child", "补充一个边界")
	tt.Frame()
	mustClick(t, tt, "追加信息给子代理")
	commands = service.dispatched()
	if len(commands) != 2 || commands[1].Kind != api.SteerRun || commands[1].ThreadID != "child" || commands[1].RunID != "rc2" {
		t.Fatalf("steer = %+v", commands)
	}
}

func TestSubagentPanelStateIsPerSession(t *testing.T) {
	dir := t.TempDir()
	w, _ := projectWorkbench()
	w.defaults.StatePath = filepath.Join(dir, "state.json")
	child := thread("child", "子任务：核对数据", 4*time.Minute)
	child.ParentThreadID = "t1"
	w.mergeThread(child)
	w.selectThread("t1")
	w.openSubagentPanel("child")
	if !w.subagentPanelShows("child") {
		t.Fatal("panel did not open")
	}
	w.selectThread("t2")
	if w.subagentPanelOpen() {
		t.Fatal("panel leaked into another session")
	}
	w.selectThread("t1")
	if !w.subagentPanelShows("child") {
		t.Fatal("panel state was not kept for the parent session")
	}

	next := newWorkbench(&fakeService{})
	next.defaults.StatePath = w.defaults.StatePath
	if err := next.restoreState(); err != nil {
		t.Fatal(err)
	}
	restored := next.subagentPanels["t1"]
	if restored == nil || !restored.open || restored.threadID != "child" {
		t.Fatalf("restored panel = %#v", restored)
	}
}

func TestSubagentPanelRenders(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR to export native review images")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name  string
		dark  bool
		width int
	}{{"subagent-light", false, 1440}, {"subagent-dark", true, 1440}, {"subagent-float", false, 700}} {
		w, _ := projectWorkbench()
		w.threads[0].Title = "设计评审"
		now := time.Now()
		running := thread("child-run", "梳理离屏渲染", time.Minute)
		running.ParentThreadID = "t1"
		running.Model = "gpt-5.4"
		done := thread("child-done", "补齐侧边栏规则", 2*time.Minute)
		done.ParentThreadID = "t1"
		done.Model = "claude-sonnet"
		w.mergeThread(running)
		w.mergeThread(done)
		w.mergeRun(run("rr", "child-run", api.RunRunning, "把运行中的子代理放进右侧面板，而不是左侧会话列表。", "面板跟在父会话旁边。\n\n- 标题、模型和耗时在顶部\n- 过程沿用对话渲染\n- 底部可以直接追加消息", time.Minute))
		w.mergeRun(run("rd", "child-done", api.RunCompleted, "隐藏有父线程的子代理。", "只有父线程不在本机时才单独成行。", 2*time.Minute))
		for i := range w.runs {
			switch w.runs[i].ID {
			case "r1":
				w.runs[i].Input = "并行看一下侧边栏和子代理面板。"
				w.runs[i].Output = "两个子代理已经开始：一个在整理渲染，一个在补侧边栏规则。"
			case "rr":
				w.runs[i].CreatedAt, w.runs[i].UpdatedAt = now.Add(-4*time.Minute), now
			case "rd":
				w.runs[i].CreatedAt, w.runs[i].UpdatedAt = now.Add(-6*time.Minute), now.Add(-time.Minute)
			}
		}
		w.setDraft("child-run", "补一句边界情况。")
		w.tasks = []api.Task{
			{ID: "k1", ParentThreadID: "t1", ParentRunID: "r1", ChildThreadID: "child-run", Title: "梳理离屏渲染", Status: api.RunRunning, Generation: 1, CreatedAt: now.Add(-4 * time.Minute), UpdatedAt: now},
			{ID: "k2", ParentThreadID: "t1", ParentRunID: "r1", ChildThreadID: "child-done", Title: "补齐侧边栏规则", Status: api.RunCompleted, Generation: 1, CreatedAt: now.Add(-6 * time.Minute), UpdatedAt: now.Add(-time.Minute)},
		}
		w.selectThread("t1")
		w.openSubagentPanel("child-run")
		tt := ui.NewTesterWith(w.view, spec.width, 900, ui.TesterOptions{Dark: spec.dark})
		tt.Advance(time.Second)
		if !w.subagentPanelShows("child-run") {
			t.Fatalf("%s: panel closed", spec.name)
		}
		file, err := os.Create(filepath.Join(output, spec.name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(file, tt.Image())
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("render %s: %v, %v", spec.name, err, closeErr)
		}
	}
}

func TestSubagentSlashCommandAndAutoOpenTabs(t *testing.T) {
	w, service := projectWorkbench()
	w.selectThread("t1")

	// Bare /subagent with no children opens the right-side creator.
	w.setDraft("t1", "/subagent")
	if !w.send("t1") {
		t.Fatal("bare /subagent did not execute")
	}
	if !w.subagentPanelOpen() {
		t.Fatal("bare /subagent did not open right-side panel")
	}
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Frame()
	if !tt.HasText("派出新子代理") {
		t.Fatalf("subagent creator missing: %v", tt.Texts())
	}

	// /subagent <task> dispatches DelegateTask directly.
	w.closeSubagentPanel()
	w.setDraft("t1", "/subagent 梳理权限边界")
	if !w.send("t1") {
		t.Fatal("/subagent <task> did not dispatch")
	}
	cmds := service.dispatched()
	if len(cmds) != 1 || cmds[0].Kind != api.DelegateTask || cmds[0].Message != "梳理权限边界" {
		t.Fatalf("dispatched = %+v", cmds)
	}

	// A new running task event auto-opens the right-side panel silently.
	c1 := thread("child-1", "梳理权限边界", time.Minute)
	c1.ParentThreadID = "t1"
	c2 := thread("child-2", "核对渲染回归", 2*time.Minute)
	c2.ParentThreadID = "t1"
	w.mergeThread(c1)
	w.mergeThread(c2)
	w.mergeRun(run("rc1", "child-1", api.RunRunning, "梳理权限边界", "检查中", time.Minute))
	w.mergeRun(run("rc2", "child-2", api.RunCompleted, "核对渲染回归", "已完成", 2*time.Minute))
	taskBytes, _ := json.Marshal(map[string]any{
		"task": api.Task{
			ID: "task-auto-1", ParentThreadID: "t1", ParentRunID: "r1",
			ChildThreadID: "child-1", Title: "梳理权限边界", Status: api.RunRunning, Generation: 1,
		},
	})
	w.foldOrchestration(api.Event{Type: "task.created", Data: taskBytes})
	if !w.subagentPanelShows("child-1") {
		t.Fatalf("expected new live task to auto-open subagent panel for child-1")
	}

	// Horizontal tab strip lets user switch directly to child-2.
	w.tasks = append(w.tasks, api.Task{
		ID: "task-auto-2", ParentThreadID: "t1", ParentRunID: "r1",
		ChildThreadID: "child-2", Title: "核对渲染回归", Status: api.RunCompleted, Generation: 1,
	})
	tt.Frame()
	mustClick(t, tt, "核对渲染回归")
	if !w.subagentPanelShows("child-2") {
		t.Fatalf("clicking subagent tab did not switch to child-2")
	}

	// Manual dismissal prevents unsolicited auto-reopen for another task.
	w.closeSubagentPanel()
	task3Bytes, _ := json.Marshal(map[string]any{
		"task": api.Task{
			ID: "task-auto-3", ParentThreadID: "t1", ParentRunID: "r1",
			ChildThreadID: "child-1", Title: "再次派发", Status: api.RunRunning, Generation: 2,
		},
	})
	w.foldOrchestration(api.Event{Type: "task.created", Data: task3Bytes})
	if w.subagentPanelOpen() {
		t.Fatal("dismissed subagent panel should stay closed until user opens or dispatches again")
	}
}
