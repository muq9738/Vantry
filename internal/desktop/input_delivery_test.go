package desktop

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestUnifiedUnknownSteeringRemainsVisibleWithoutRetry(t *testing.T) {
	current := run("r1", "t1", api.RunCompleted, "initial", "reply", 0)
	w, service := newViewWorkbench(current)
	w.applySnapshot(api.Snapshot{Sequence: 4, Threads: []api.Thread{thread("t1", "会话", 0)}, Runs: []api.Run{current},
		Inputs:   []api.Input{{ID: "input-1", ThreadID: "t1", RunID: "r1", Kind: "steer", Message: "先处理取消", Status: "unknown", Error: "Pi ended before acknowledging this input"}},
		Steering: []api.SteeringMessage{{ID: "input-1", CommandID: "command-1", ThreadID: "t1", RunID: "r1", Message: "先处理取消", Status: api.SteeringUnknown}},
	})
	tester := ui.NewTester(w.view, 1080, 700)
	for _, text := range []string{"先处理取消", "交付状态未知", "不会自动重发", "Pi ended before acknowledging this input"} {
		if !tester.HasText(text) {
			t.Fatalf("unknown delivery disappeared from the unified history: missing %q", text)
		}
	}
	if tester.HasText("重试原调整") {
		t.Fatal("accepted input with unknown delivery offered a new submission")
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("rendering unknown delivery resubmitted the input")
	}
}

func TestSteerInputNeverLeaksIntoLegacyQueuedView(t *testing.T) {
	current := run("r1", "t1", api.RunRunning, "initial", "", 0)
	w, _ := newViewWorkbench(current)
	w.applySnapshot(api.Snapshot{
		Sequence: 5,
		Threads:  []api.Thread{thread("t1", "会话", 0)},
		Runs:     []api.Run{current},
		Inputs: []api.Input{{
			ID: "inp-steer-1", ThreadID: "t1", RunID: "r1", Kind: "steer", Message: "怎么说？？？", Status: "delivered",
		}},
		Messages: []api.QueuedMessage{{
			ID: "inp-steer-1", ThreadID: "t1", Status: "pending", Message: "怎么说？？？",
		}},
	})
	tt := ui.NewTester(w.view, 1080, 700)
	if !tt.HasText("怎么说？？？") || !tt.HasText("已追加到本轮") {
		t.Fatalf("steer bubble missing clean delivered label: %v", tt.Texts())
	}
	if tt.HasText("不代表模型已消费") {
		t.Fatalf("steer bubble still shows defensive jargon: %v", tt.Texts())
	}
	if tt.HasText("撤回") || tt.HasText("待发送") {
		t.Fatalf("steer input leaked into legacy queuedView: %v", tt.Texts())
	}
}

func TestLiveToolGroupCollapsedByDefaultAndWorkspaceLockShown(t *testing.T) {
	th1 := thread("t1", "当前会话", 0)
	th1.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/tmp/shared-repo"}
	th2 := thread("t2", "后台编译会话", -time.Minute)
	th2.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/tmp/shared-repo"}
	r1 := run("r1", "t1", api.RunRunning, "检查项目", "", 0)
	r2 := run("r2", "t2", api.RunRunning, "跑构建", "正在编译", -time.Minute)
	w, _ := newViewWorkbench()
	w.applySnapshot(api.Snapshot{
		Sequence: 6,
		Threads:  []api.Thread{th1, th2},
		Runs:     []api.Run{r1, r2},
	})
	w.selected = "t1"
	tt := ui.NewTester(w.view, 1080, 700)
	if !tt.HasText("等待工作区释放") || !tt.HasText("后台编译会话") {
		t.Fatalf("workspace lock wait hint missing: %v", tt.Texts())
	}

	// Once tools arrive in a live run, multi-tool groups stay collapsed by default.
	r1.Output = "我先检查一下项目。"
	w.applySnapshot(api.Snapshot{
		Sequence: 7,
		Threads:  []api.Thread{th1},
		Runs:     []api.Run{r1},
		Tools: []api.ToolCall{
			{ID: "tc1", RunID: "r1", Name: "read", Status: "completed", Arguments: []byte(`{"path":"README.md"}`)},
			{ID: "tc2", RunID: "r1", Name: "read", Status: "completed", Arguments: []byte(`{"path":"go.mod"}`)},
			{ID: "tc3", RunID: "r1", Name: "bash", Status: "running", Arguments: []byte(`{"command":"git status"}`)},
		},
	})
	tt.Frame()
	if _, expanded := tt.AccessByRole(ui.RoleButton, "收起步骤"); expanded {
		t.Fatalf("live multi-tool group should stay collapsed by default: %v", tt.Texts())
	}
	if _, collapsed := tt.AccessByRole(ui.RoleButton, "展开步骤"); !collapsed {
		t.Fatalf("collapsed multi-tool disclosure row missing: %v", tt.Texts())
	}
}

func TestComposerEnterBehaviorWindowsAndMac(t *testing.T) {
	// Default Enter-to-send mode on macOS: Enter sends, Cmd+Enter breaks line.
	if nl, send, _ := composerEnterBehavior(true, 0, "darwin"); nl || !send {
		t.Fatalf("darwin Enter default = (%v,%v), want (false,true)", nl, send)
	}
	if nl, send, _ := composerEnterBehavior(true, ui.Super, "darwin"); !nl || send {
		t.Fatalf("darwin Cmd+Enter default = (%v,%v), want (true,false)", nl, send)
	}
	// Default Enter-to-send mode on Windows: Enter sends, Ctrl+Enter breaks line.
	if nl, send, _ := composerEnterBehavior(true, 0, "windows"); nl || !send {
		t.Fatalf("windows Enter default = (%v,%v), want (false,true)", nl, send)
	}
	if nl, send, _ := composerEnterBehavior(true, ui.Ctrl, "windows"); !nl || send {
		t.Fatalf("windows Ctrl+Enter default = (%v,%v), want (true,false)", nl, send)
	}
	// Explicit Ctrl+Enter send mode on Windows: Enter breaks line (default), Ctrl+Enter sends.
	if nl, send, _ := composerEnterBehavior(false, 0, "windows"); nl || send {
		t.Fatalf("windows multiline Enter = (%v,%v), want (false,false)", nl, send)
	}
	if nl, send, _ := composerEnterBehavior(false, ui.Ctrl, "windows"); nl || !send {
		t.Fatalf("windows multiline Ctrl+Enter = (%v,%v), want (false,true)", nl, send)
	}
}
