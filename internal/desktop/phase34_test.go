package desktop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestDoubleEscapeStopsActiveRunAndOpensRewindWhenIdle(t *testing.T) {
	// Active run: 1st Esc arms without stopping; 2nd Esc within window stops.
	w, f := newViewWorkbench(run("r1", "t1", api.RunRunning, "继续运行", "输出", 0))
	tt := ui.NewTester(w.view, 1080, 700)
	tt.SetLocale("zh-Hans")
	mustClick(t, tt, "消息输入框")

	tt.Key(0, ui.KeyEscape)
	if len(f.dispatched()) != 0 {
		t.Fatal("first Escape must not stop active run")
	}
	tt.Advance(200 * time.Millisecond)
	tt.Key(0, ui.KeyEscape)
	dispatched := f.dispatched()
	if len(dispatched) != 1 || dispatched[0].Kind != api.StopRun || dispatched[0].RunID != "r1" {
		t.Fatalf("second Escape while running = %+v, want single StopRun(r1)", dispatched)
	}

	// Idle thread with settled turn: double Esc opens rewind dialog.
	wIdle, _ := newViewWorkbench(run("r2", "t1", api.RunCompleted, "fix bug", "fixed", 0))
	ttIdle := ui.NewTester(wIdle.view, 1080, 700)
	ttIdle.SetLocale("zh-Hans")
	mustClick(t, ttIdle, "消息输入框")
	ttIdle.Key(0, ui.KeyEscape)
	if wIdle.widgets.rewind.open {
		t.Fatal("single Escape while idle must not open rewind dialog")
	}
	ttIdle.Advance(150 * time.Millisecond)
	ttIdle.Key(0, ui.KeyEscape)
	if !wIdle.widgets.rewind.open || !ttIdle.HasText("回退与检查点") {
		t.Fatalf("double Escape while idle did not open rewind dialog: open=%v texts=%v", wIdle.widgets.rewind.open, ttIdle.Texts())
	}
}

func TestCtrlOAndCtrlTToggleToolsAndReasoning(t *testing.T) {
	first := "第一步分析。\n\n"
	r := run("turn1", "t1", api.RunCompleted, "检查代码", first+"结果完成。", 0)
	r.Reasoning = "第一行背景分析。\n这是详细的内部推理过程，包含步骤分析。"
	w, _ := newViewWorkbench(r)
	w.tools = []api.ToolCall{
		{ID: "tool-1", ThreadID: "t1", RunID: "turn1", Name: "bash", Arguments: json.RawMessage(`{"command":"git status"}`), Status: "completed", OutputOffset: len(first), Output: "clean tree"},
		{ID: "tool-2", ThreadID: "t1", RunID: "turn1", Name: "read", Arguments: json.RawMessage(`{"path":"go.mod"}`), Status: "completed", OutputOffset: len(first), Output: "module github.com/muq9738/Vantry"},
	}

	tt := ui.NewTester(w.view, 1280, 800)
	tt.SetLocale("zh-Hans")
	tt.Advance(time.Second)

	// Default collapsed: full multi-line reasoning body & expanded tool output are hidden.
	if tt.HasText("第一行背景分析。") {
		t.Fatal("reasoning should be collapsed by default")
	}

	// Ctrl+T expands reasoning.
	tt.Key(ui.Ctrl, ui.KeyT)
	if !tt.HasText("第一行背景分析。") {
		t.Fatalf("Ctrl+T did not expand reasoning: %v", tt.Texts())
	}
	tt.Key(ui.Ctrl, ui.KeyT)
	if tt.HasText("第一行背景分析。") {
		t.Fatal("second Ctrl+T did not collapse reasoning")
	}

	// Ctrl+O expands tool groups & tool details.
	tt.Key(ui.Ctrl, ui.KeyO)
	if !w.widgets.defaultToolsExpanded || !tt.HasText("clean tree") {
		t.Fatalf("Ctrl+O did not expand tool details: default=%v texts=%v", w.widgets.defaultToolsExpanded, tt.Texts())
	}
	tt.Key(ui.Ctrl, ui.KeyO)
	if w.widgets.defaultToolsExpanded || tt.HasText("clean tree") {
		t.Fatalf("second Ctrl+O did not collapse tool details: default=%v", w.widgets.defaultToolsExpanded)
	}
}

func TestRewindDialogListsTurnsAndDispatchesRewind(t *testing.T) {
	f := &fakeService{}
	f.receipt = func(cmd api.Command) (api.Receipt, error) {
		return api.Receipt{CommandID: cmd.CommandID, Accepted: true, ThreadID: cmd.ThreadID, RunID: cmd.RunID, Sequence: 10}, nil
	}
	var actions []string
	wb := &fakeWorkbenchService{
		fakeService: f,
		workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
			actions = append(actions, req.Action)
			switch req.Action {
			case "git.checkpoints.list":
				return api.WorkbenchResponse{Output: `{"checkpoints":[{"id":"run-r2","ref":"refs/vantry/checkpoints/run-r2","label":"Before turn: second step"}]}`}, nil
			case "git.checkpoints.diff":
				return api.WorkbenchResponse{Output: "--- a/main.go\n+++ b/main.go\n-old\n+new\n"}, nil
			case "git.checkpoints.restore":
				return api.WorkbenchResponse{Output: `{"backup":{"id":"pre-restore-1"}}`}, nil
			default:
				return api.WorkbenchResponse{}, nil
			}
		},
	}

	w := newWorkbench(wb)
	r1 := run("r1", "t1", api.RunCompleted, "first step prompt", "first ans", 0)
	r2 := run("r2", "t1", api.RunCompleted, "second step prompt", "second ans", time.Minute)
	w.applySnapshot(api.Snapshot{
		Sequence: 3,
		Threads:  []api.Thread{thread("t1", "测试回退", 0)},
		Runs:     []api.Run{r1, r2},
	})

	tt := ui.NewTester(w.view, 1280, 800)
	tt.SetLocale("zh-Hans")
	tt.Advance(time.Second)

	mustClick(t, tt, "从本轮回退")
	if !w.widgets.rewind.open || !tt.HasText("含文件快照") || !tt.HasText("second step prompt") {
		t.Fatalf("rewind dialog missing turn/snapshot: open=%v texts=%v", w.widgets.rewind.open, tt.Texts())
	}
	mustClick(t, tt, "#2 second step prompt")

	mustClick(t, tt, "查看文件差异")
	if !tt.HasText("+++ b/main.go") {
		t.Fatalf("checkpoint diff preview missing: %v", tt.Texts())
	}

	mustClick(t, tt, "回退对话与文件")
	dispatched := f.dispatched()
	if len(dispatched) != 1 || dispatched[0].Kind != api.RewindThread || dispatched[0].RunID != "r2" {
		t.Fatalf("expected RewindThread(r2), got %+v (actions=%v)", dispatched, actions)
	}
	if gotDraft := w.composerFor("t1").draft; gotDraft != "second step prompt" {
		t.Fatalf("expected rewound prompt restored to draft, got %q", gotDraft)
	}
}

func TestCrossThreadInboxBadgeZeroClutterAndPopoverJump(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "hi", "hello", 0))
	t2 := thread("t2", "后台待审批会话", time.Minute)
	w.threads = append(w.threads, t2)
	w.runs = append(w.runs, run("r2", "t2", api.RunRunning, "deploy", "", time.Minute))

	tt := ui.NewTester(w.view, 1280, 800)
	tt.SetLocale("zh-Hans")
	tt.Frame()

	// Zero items -> zero clutter (no inbox badge).
	if _, ok := tt.Access("待处理收件箱"); ok {
		t.Fatal("inbox badge must be hidden when zero attention/unread items exist")
	}

	// Add background pending approval on t2 + unread result on t3.
	t3 := thread("t3", "后台已完成会话", 2*time.Minute)
	w.threads = append(w.threads, t3)
	w.runs = append(w.runs, run("r3", "t3", api.RunCompleted, "summarize", "全部整理完成", 2*time.Minute))
	w.markUnread("t3")
	w.requests = []api.ProviderRequest{{
		ID: "req-bg", ThreadID: "t2", RunID: "r2", Method: "confirm", Title: "允许执行 make build？", Status: "pending",
	}}
	tt.Frame()

	if _, ok := tt.Access("待处理收件箱"); !ok {
		t.Fatalf("expected inbox badge when background approval & unread exist: %v", tt.Texts())
	}
	// Badge must never duplicate "需要你的确认".
	if strings.Contains(strings.Join(tt.Texts(), "\n"), "需要你的确认") {
		t.Fatalf("closed inbox badge or inactive screen unexpectedly rendered '需要你的确认': %v", tt.Texts())
	}

	mustClick(t, tt, "待处理收件箱")
	if !w.widgets.inboxOpen || !tt.HasText("后台待审批会话") || !tt.HasText("后台已完成会话") {
		t.Fatalf("inbox popover missing expected threads: open=%v texts=%v", w.widgets.inboxOpen, tt.Texts())
	}

	mustClick(t, tt, "后台已完成会话")
	if w.selected != "t3" || w.isUnread("t3") || w.widgets.inboxOpen {
		t.Fatalf("clicking inbox row failed jump/clear: selected=%q unread=%v open=%v", w.selected, w.isUnread("t3"), w.widgets.inboxOpen)
	}
}

func TestApprovalSelectCardShowsSessionGrantButtons(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunRunning, "run tests", "", 0))
	w.requests = []api.ProviderRequest{{
		ID:       "req-sel",
		ThreadID: "t1",
		RunID:    "r1",
		Method:   "select",
		Title:    "允许执行 bash？\n{\"command\":\"go test ./...\"}",
		Options:  []string{"允许本次", "本会话允许 bash:go", "拒绝"},
		Status:   "pending",
	}}

	tt := ui.NewTester(w.view, 1280, 800)
	tt.SetLocale("zh-Hans")
	tt.Frame()

	for _, want := range []string{"需要你的确认", "允许执行 bash？", "go test ./...", "允许本次", "本会话允许 bash:go", "拒绝"} {
		if !tt.HasText(want) {
			t.Fatalf("approval select card missing %q in:\n%s", want, strings.Join(tt.Texts(), "\n"))
		}
	}

	mustClick(t, tt, "本会话允许 bash:go")
	dispatched := f.dispatched()
	if len(dispatched) != 1 || dispatched[0].Kind != api.RespondRequest || dispatched[0].Value != "本会话允许 bash:go" {
		t.Fatalf("expected RespondRequest with session grant value, got %+v", dispatched)
	}
}

func TestSettingsAgentShowsSeatbeltSandboxToggle(t *testing.T) {
	f := &fakeService{}
	mode := "off"
	var seenSet string
	wb := &fakeWorkbenchService{
		fakeService: f,
		workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
			switch req.Action {
			case "sandbox.get":
				return api.WorkbenchResponse{Output: `{"mode":"` + mode + `","supported":true,"envOverride":""}`}, nil
			case "sandbox.set":
				seenSet = req.Message
				mode = req.Message
				return api.WorkbenchResponse{Output: `{"mode":"` + mode + `","supported":true,"envOverride":""}`}, nil
			default:
				return api.WorkbenchResponse{}, nil
			}
		},
	}

	w := newWorkbench(wb)
	w.applySnapshot(api.Snapshot{Sequence: 1, Threads: []api.Thread{thread("t1", "测试", 0)}})
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "agent"

	tt := ui.NewTester(w.view, 1280, 800)
	tt.SetLocale("zh-Hans")
	tt.Frame()

	if !tt.HasText("工作区沙箱隔离 (macOS Seatbelt)") {
		t.Fatalf("settings agent page missing Seatbelt sandbox section: %v", tt.Texts())
	}
	sw, ok := tt.AccessByRole(ui.RoleSwitch, "启用工作区写隔离")
	if !ok {
		t.Fatalf("missing RoleSwitch for 启用工作区写隔离: %v", tt.Texts())
	}
	tt.ClickAt(sw.Bounds.X+sw.Bounds.W/2, sw.Bounds.Y+sw.Bounds.H/2)
	if seenSet != "workspace-write" || w.widgets.sandbox.mode != "workspace-write" {
		t.Fatalf("expected sandbox.set workspace-write, got seen=%q state=%q", seenSet, w.widgets.sandbox.mode)
	}
}
