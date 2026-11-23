package desktop

import (
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

const reviewReply = "下面用示例数据展示图表和处理路径。\n\n" +
	"```chart\n{\"type\":\"bar\",\"title\":\"每周完成任务\",\"subtitle\":\"界面验收用示例数据\",\"unit\":\"项\",\"labels\":[\"第一周\",\"第二周\",\"第三周\"],\"series\":[{\"name\":\"计划\",\"data\":[12,16,18]},{\"name\":\"完成\",\"data\":[10,15,18]}]}\n```\n\n" +
	"```chart\n{\"type\":\"stats\",\"items\":[{\"label\":\"完成任务\",\"value\":\"43\",\"note\":\"三周合计\"},{\"label\":\"待完成\",\"value\":\"3\"},{\"label\":\"完成率\",\"value\":\"93%\"}]}\n```\n\n" +
	"请求的处理路径：\n\n```mermaid\nflowchart LR\n  UI[桌面] -->|message.send| Core\n  Core --> W{Pi worker}\n  W -->|工具| WS[(工作区)]\n  W -.->|delta| Core\n  Core -->|run.output| UI\n```\n\n" +
	"```mermaid\nsequenceDiagram\n  autonumber\n  participant D as 桌面\n  participant C as Core\n  D->>C: message.send\n  C-->>D: receipt\n  loop 流式\n    C-->>D: run.output\n  end\n  Note over D,C: 只发 delta\n```\n\n" +
	"| 周次 | 计划 | 完成 |\n| --- | ---: | ---: |\n| 第一周 | 12 | 10 |\n| 第二周 | 16 | 15 |\n"

func reviewTimelineWorkbench() *workbench {
	r := run("r1", "t1", api.RunCompleted, "帮我整理这三周的任务完成情况，并画图。", reviewReply, 0)
	r.Reasoning = "先读取基准结果。\n再按场景整理成图表。"
	r.Model = "magpie/deepseek-v4.1-flash"
	r.UpdatedAt = r.CreatedAt.Add(42 * time.Second)
	w, _ := newViewWorkbench(r)
	th := w.threads[0]
	th.Title = "任务完成情况与图表"
	th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Users/me/Develop/Vantry"}
	w.mergeThread(th)
	at := len("下面用示例数据展示图表和处理路径。\n\n")
	w.tools = []api.ToolCall{
		{ID: "a", ThreadID: "t1", RunID: "r1", Name: "read", Arguments: json.RawMessage(`{"path":"/Users/me/Develop/Vantry/.agent/perf/after.txt"}`), Status: "completed"},
		{ID: "b", ThreadID: "t1", RunID: "r1", Name: "bash", Arguments: json.RawMessage(`{"command":"go test ./internal/desktop -bench Perf"}`), Output: "PASS", Status: "completed"},
		{ID: "c", ThreadID: "t1", RunID: "r1", Name: "edit", Arguments: json.RawMessage(`{"path":"/Users/me/Develop/Vantry/internal/desktop/stream_buffer.go","oldText":"out += delta","newText":"b.WriteString(delta)\nreturn b.String()"}`), Status: "completed", OutputOffset: at},
	}
	for _, extra := range []struct {
		id, title string
		status    api.RunStatus
		pinned    bool
		archived  bool
	}{{"t2", "整理侧栏分区", api.RunRunning, false, false}, {"thread-c", "发布说明", api.RunCompleted, true, false}, {"t4", "旧的实验", api.RunCompleted, false, true}} {
		t := thread(extra.id, extra.title, time.Hour)
		t.Pinned, t.Archived = extra.pinned, extra.archived
		t.Workspace = th.Workspace
		w.mergeThread(t)
		w.mergeRun(run("r"+extra.id, extra.id, extra.status, extra.title, "好的", time.Hour))
	}
	return w
}

// TestTimelineRenders exports the workbench timeline and sidebar for review.
func TestTimelineRenders(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR to export native review images")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name          string
		width, height int
		dark          bool
		scroll        float32
	}{{"timeline-1440-light", 1440, 1700, false, 0}, {"timeline-1440-dark", 1440, 1700, true, 0}, {"timeline-1080-dark-end", 1080, 900, true, -1}, {"timeline-640-light", 640, 600, false, -1}} {
		t.Run(spec.name, func(t *testing.T) {
			w := reviewTimelineWorkbench()
			tt := ui.NewTester(w.view, spec.width, spec.height)
			tt.SetDark(spec.dark)
			tt.SetLocale("zh-Hans")
			tt.Advance(2 * time.Second)
			if spec.scroll < 0 {
				w.logState("t1").ScrollToEnd()
				tt.Advance(time.Second)
			}
			file, err := os.Create(filepath.Join(output, spec.name+".png"))
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
