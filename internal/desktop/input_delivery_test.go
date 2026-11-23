package desktop

import (
	"testing"

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
	for _, text := range []string{"补充指令：先处理取消", "交付状态未知", "不会自动重发", "Pi ended before acknowledging this input"} {
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
