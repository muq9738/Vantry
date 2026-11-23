package desktop

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// The benchmarks pin the cost of the screens that grow with use: a long
// sidebar, a long conversation, a streaming reply and folding output events.
// Compare runs with: go test ./internal/desktop -run '^$' -bench Perf -count 6

const perfReply = `## 结论

当前实现分三层：**Core**、**provider** 与 **desktop**。下面是关键路径：

- 订阅从游标重放，再进入实时事件；
- 工具调用按 attempt 与调用 ID 投影；
- 审批请求在同一个事务里推进游标。

| 模块 | 文件 | 行数 |
| --- | --- | ---: |
| Core | internal/core/core.go | 420 |
| Worker | internal/core/worker.go | 512 |
| Desktop | internal/desktop/view.go | 700 |

` + "```go\nfunc (c *Core) Dispatch(ctx context.Context, cmd api.Command) (api.Receipt, error) {\n\treturn c.dispatch(ctx, cmd)\n}\n```\n" + `
最后，渲染层只重建变化的块，其余块复用上一次的布局。`

func perfWorkbench(threads, runsPerThread int) *workbench {
	f := &fakeService{health: api.Health{Version: api.ProtocolVersion, State: "ready"}}
	w := newWorkbench(f)
	snap := api.Snapshot{Sequence: 1}
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < threads; i++ {
		th := api.Thread{ID: fmt.Sprintf("t%03d", i), Title: fmt.Sprintf("会话 %d：整理工作区与模型目录", i), CreatedAt: base.Add(time.Duration(i) * time.Minute), ProviderID: "pi",
			Workspace: &api.WorkspaceSpec{Kind: "directory", Path: fmt.Sprintf("/Users/me/Develop/project-%d", i%6)}}
		th.Pinned = i%37 == 0
		snap.Threads = append(snap.Threads, th)
	}
	for r := 0; r < runsPerThread; r++ {
		id := fmt.Sprintf("r%03d", r)
		snap.Runs = append(snap.Runs, api.Run{ID: id, ThreadID: "t000", AttemptID: "a" + id, Status: api.RunCompleted, ProviderID: "pi",
			Input: fmt.Sprintf("第 %d 个问题：解释一下这段实现，并给出表格。", r), Output: perfReply,
			CreatedAt: base.Add(time.Duration(r) * time.Second), UpdatedAt: base.Add(time.Duration(r)*time.Second + 3*time.Second)})
		snap.Tools = append(snap.Tools, api.ToolCall{ID: "tool" + id, ThreadID: "t000", RunID: id, AttemptID: "a" + id, Name: "bash",
			Arguments: mustJSON(map[string]string{"command": "rg -n Dispatch internal/core"}), Output: strings.Repeat("internal/core/core.go:42: func Dispatch\n", 8), Status: "completed"})
	}
	w.applySnapshot(snap)
	w.selectThread("t000")
	return w
}

func BenchmarkPerfSidebarFrame(b *testing.B) {
	w := perfWorkbench(300, 0)
	w.selectThread("t001")
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Frame()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tt.Frame()
	}
}

func BenchmarkPerfConversationFrame(b *testing.B) {
	w := perfWorkbench(20, 40)
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Frame()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tt.Frame()
	}
}

// Each operation delivers 48 bytes of real Markdown and completes the UI
// update. Report total ns/op, including any extra layout or animation frames.
// Keep the exact same workload on the baseline and candidate.
func BenchmarkPerfStreamingFrame(b *testing.B) {
	w := perfWorkbench(20, 12)
	run := w.runs[len(w.runs)-1]
	run.Status, run.Output = api.RunRunning, ""
	w.mergeRun(run)
	frames := 0
	tt := ui.NewTester(func(c *ui.Context) { frames++; w.view(c) }, 1440, 900)
	tt.Frame()
	text := strings.Repeat(perfReply+"\n\n", 3)
	at := 0
	seq := int64(10)
	frames = 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if at == len(text) {
			w.runs[len(w.runs)-1].Output = ""
			at = 0
		}
		end := min(at+48, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end++
		}
		seq++
		w.applyEvent(deltaEvent(seq, "t000", run.ID, text[at:end]))
		at = end
		tt.Frame()
	}
	b.ReportMetric(float64(frames)/float64(b.N), "frames/op")
}

// Folding deltas into a long reply must not copy the whole reply each time.
func BenchmarkPerfFoldDeltas(b *testing.B) {
	w := perfWorkbench(1, 1)
	run := w.runs[0]
	run.Status, run.Output = api.RunRunning, strings.Repeat("前文内容。", 40000)
	w.mergeRun(run)
	seq := int64(10)
	initial := run.Output
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%1024 == 0 {
			w.runs[0].Output = initial
		}
		seq++
		w.applyEvent(deltaEvent(seq, "t000", run.ID, "追加一个词 "))
	}
}

// BenchmarkPerfVisualFrame is a steady frame of a conversation whose replies
// hold a chart and a flowchart: parsed and laid out once, painted each frame.
func BenchmarkPerfVisualFrame(b *testing.B) {
	w := perfWorkbench(20, 40)
	visual := perfReply + "\n```chart\n{\"type\":\"line\",\"title\":\"延迟\",\"unit\":\"ms\",\"labels\":[\"1\",\"2\",\"3\",\"4\",\"5\",\"6\"],\"series\":[{\"name\":\"p50\",\"data\":[12,14,11,15,13,12]},{\"name\":\"p99\",\"data\":[40,52,38,60,45,41]}]}\n```\n\n```mermaid\nflowchart LR\n  A[桌面] --> B[Core]\n  B --> C{Pi}\n  C -->|工具| D[工作区]\n  C -->|回复| A\n```\n"
	for i := range w.runs {
		w.runs[i].Output = visual
	}
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tt.Frame()
	}
}
