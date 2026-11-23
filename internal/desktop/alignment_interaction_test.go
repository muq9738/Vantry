package desktop

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestFindQuoteAndOutlineKeepDraftAndNeverDispatch(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "第一问", "第一段回答", 0), run("r2", "t1", api.RunCompleted, "第二问", "特殊结果", time.Minute))
	w.setDraft("t1", "已有草稿")
	tt := ui.NewTester(w.view, 1080, 900)
	tt.Key(ui.Cmd, ui.KeyF)
	if !tt.Focused("会话内查找") {
		t.Fatal("find shortcut did not focus search")
	}
	tt.Type("特殊")
	if len(w.timelineNav.matches) != 1 || w.timelineNav.matches[0] != 1 {
		t.Fatalf("matches=%v", w.timelineNav.matches)
	}
	tt.Key(0, ui.KeyEscape)
	if w.timelineNav.open || !tt.Focused("消息输入框") {
		t.Fatalf("Escape: open=%v composer=%v search=%v texts=%v", w.timelineNav.open, tt.Focused("消息输入框"), tt.Focused("会话内查找"), tt.Texts())
	}
	w.quoteReply("一行\n二行")
	if got := w.composerFor("t1").draft; got != "已有草稿\n\n> 一行\n> 二行\n\n" {
		t.Fatalf("quote=%q", got)
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("navigation or quoting dispatched a command")
	}
	mustClick(t, tt, "跳转到对话轮次")
	if !tt.HasText("1. 第一问") || !tt.HasText("2. 第二问") {
		t.Fatalf("outline missing: %v", tt.Texts())
	}
	mustClick(t, tt, "1. 第一问")
	if w.timelineNav.outline {
		t.Fatal("outline stayed open")
	}
}

func TestThreadDropsUseCommittedCommandsAndRefuseArchiveOfActiveRun(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunRunning, "工作", "", 0))
	if w.dropThread(threadDrag{id: "t1"}, sectionSettled) {
		t.Fatal("archived active thread")
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("invalid drop dispatched")
	}
	if !w.dropThread(threadDrag{id: "t1"}, sectionPinned) {
		t.Fatal("pin drop rejected")
	}
	cmds := service.dispatched()
	if len(cmds) != 1 || cmds[0].Kind != api.UpdateThread || cmds[0].ThreadID != "t1" || cmds[0].Pinned == nil || !*cmds[0].Pinned {
		t.Fatalf("commands=%+v", cmds)
	}
	th, _ := w.threadByID("t1")
	if th.Pinned {
		t.Fatal("receipt invented pinned projection")
	}
}

func TestSidebarCollapseSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w, _ := newViewWorkbench()
	w.defaults.StatePath = path
	*w.projectExpanded(sectionActive) = false
	*w.projectExpanded(sectionSettled) = true
	w.saveState()
	next, _ := newViewWorkbench()
	next.defaults.StatePath = path
	if err := next.restoreState(); err != nil {
		t.Fatal(err)
	}
	if *next.projectExpanded(sectionActive) || !*next.projectExpanded(sectionSettled) {
		t.Fatal("collapse preferences lost")
	}
}

func TestToolCacheRefreshesSameLengthArgumentsAndSnapshot(t *testing.T) {
	w, _ := newViewWorkbench()
	tool := api.ToolCall{ID: "one", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)}
	if w.toolView(tool).path != "a.go" {
		t.Fatal("first view")
	}
	tool.Arguments = json.RawMessage(`{"path":"b.go"}`)
	if w.toolView(tool).path != "b.go" {
		t.Fatal("same-length replacement kept stale path")
	}
	w.applySnapshot(api.Snapshot{})
	if len(w.toolViews) != 0 || len(w.outputs) != 0 {
		t.Fatal("snapshot retained obsolete caches")
	}
}

func TestUnsupportedMermaidPreservesSource(t *testing.T) {
	for _, source := range []string{
		"gantt\ntitle 项目排期\nsection 阶段\n任务 :a1, 2026-01-01, 3d",
		"journey\ntitle 用户路径\nsection 登录\n打开首页: 5: 用户",
		"stateDiagram-v2\nnote right of Idle : 暂不支持状态注释块",
		"pie\n\"valid\":1\ninvalid value",
	} {
		if diagram := parseMermaid(source, measureFixed); diagram.err == nil {
			t.Errorf("silently discarded syntax: %s", source)
		}
	}
	for _, supported := range []string{
		"flowchart LR\nsubgraph Boundary\nA-->B\nend",
		"sequenceDiagram\nactor A\nA->>+B: hello\nB-->>-A: ok",
		"stateDiagram-v2\n[*] --> Ready\nReady --> [*]",
	} {
		if diagram := parseMermaid(supported, measureFixed); diagram.err != nil {
			t.Errorf("expected native mermaid support for %q, got error: %v", supported, diagram.err)
		}
	}
}

func TestVisualZoomAndNarrowChartControls(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "绘图", "```chart\n{\"type\":\"stats\",\"items\":[{\"label\":\"a\",\"value\":\"1\"},{\"label\":\"b\",\"value\":\"2\"}]}\n```", 0))
	tt := ui.NewTester(w.view, 640, 600)
	mustClick(t, tt, "放大")
	if !w.visuals.expanded {
		t.Fatal("chart expand failed")
	}
	mustClick(t, tt, "关闭")
	w.expandVisual("mermaid", "flowchart LR\nA[开始]-->B[完成]")
	tt.Frame()
	mustClick(t, tt, "放大图形")
	if w.visuals.zoom != 1.25 {
		t.Fatal("zoom did not advance")
	}
	mustClick(t, tt, "重置缩放")
	if w.visuals.zoom != 1 {
		t.Fatal("zoom did not reset")
	}
	tt.Key(0, ui.KeyEscape)
	if w.visuals.expanded {
		t.Fatal("Escape did not close expanded diagram")
	}
}

func TestReplyImagePathsAreDecodedWithoutExternalFetch(t *testing.T) {
	for _, bad := range []string{"https://example.com/x.png", "file://server/x.png", "data:image/png;base64,a"} {
		if _, err := replyImagePath(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if got, err := replyImagePath("plots/a%20b.png"); err != nil || got != "plots/a b.png" {
		t.Fatalf("path=%q %v", got, err)
	}
}

// Pinning and menu actions are exercised through MyGo's actual menu dispatch.
func TestSidebarNativeContextMenuPin(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "测试", "完成", 0))
	tt := ui.NewTester(w.view, 1080, 900)
	if err := tt.RightClick("第一条线程"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("置顶会话"); err != nil {
		t.Fatal(err)
	}
	cmds := service.dispatched()
	if len(cmds) != 1 || cmds[0].Pinned == nil || !*cmds[0].Pinned {
		t.Fatalf("commands=%+v", cmds)
	}
}
