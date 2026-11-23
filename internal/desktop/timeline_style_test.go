package desktop

import (
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func timelineStyleWorkbench() *workbench {
	first := "我会先检查工作区和目标分支，保留当前改动，再执行合并与相关验证。\n\n"
	second := "合并已完成，没有遗留冲突。现在核对真实行为，而不是只看构建是否成功。\n\n"
	final := "## 已完成\n\n- **分支合并**：保留现有工作，未推送远端。\n- **本地验证**：测试通过，结果可复核。\n\n### 实现依据\n\n使用 `git status --short --branch` 核对工作区。工具明细按需展开，失败步骤保持可见。\n\n```go\nfunc ready() bool {\n    return true\n}\n```\n\n> 当前画面仅使用本地渲染 fixture，没有执行 Git 写操作或调用模型。\n"
	r := run("styled", "t1", api.RunCompleted, "把目标分支合并进来，保留本地改动并验证。", first+second+final, 0)
	r.Reasoning = strings.Repeat("正在核对工作目录、当前分支和变更范围。", 10) + "\n" + strings.Repeat("Reviewing the existing implementation and the actual verification results. ", 12)
	r.UpdatedAt = r.CreatedAt.Add(42 * time.Second)
	r.Model = "fixture/review"
	w, _ := newViewWorkbench(r)
	w.threads[0].Title = "分支合并与行为验收"
	w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry", Branch: "feature/native-interactions"}
	w.tools = []api.ToolCall{
		{ID: "inspect", ThreadID: "t1", RunID: r.ID, Name: "bash", Arguments: json.RawMessage(`{"command":"git status --short --branch"}`), Status: "completed", OutputOffset: len(first), Output: "## feature/native-interactions"},
		{ID: "read", ThreadID: "t1", RunID: r.ID, Name: "read", Arguments: json.RawMessage(`{"path":"internal/desktop/timeline.go"}`), Status: "completed", OutputOffset: len(first), Output: "package desktop"},
		{ID: "test", ThreadID: "t1", RunID: r.ID, Name: "bash", Arguments: json.RawMessage(`{"command":"go test ./internal/desktop"}`), Status: "completed", OutputOffset: len(first) + len(second), Output: "ok desktop"},
	}
	return w
}

func TestTimelineReasoningLabelStaysHorizontal(t *testing.T) {
	for _, width := range []int{1440, 1080, 860, 640} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			w := timelineStyleWorkbench()
			tt := ui.NewTester(w.view, width, 900)
			label, ok := tt.Find("思考过程")
			if !ok {
				t.Fatalf("missing reasoning label: %v", tt.Texts())
			}
			if label.H <= 0 || label.H > 24 || label.W <= label.H*2.5 {
				t.Fatalf("reasoning title was squeezed into vertical text: %+v", label)
			}
			if err := tt.Click("思考过程"); err != nil {
				t.Fatal(err)
			}
			if !w.widgets.reasoningOpen["styled"] {
				t.Fatal("reasoning no longer expands")
			}
		})
	}
}

func TestTimelineStyleHasReadableMarkdownAndStableStepLabels(t *testing.T) {
	w := timelineStyleWorkbench()
	tt := ui.NewTester(w.view, 1399, 1000)
	if !tt.HasText("已完成") || !tt.HasText("实现依据") {
		t.Fatalf("rich reply content missing: %v", tt.Texts())
	}
	theme := w.replyMarkdown("t1").Theme
	if theme.FontSize < 15 || theme.StrongWeight > 650 || theme.BlockSpacing < 0.9 {
		t.Fatalf("reply typography lacks the intended reading hierarchy: %+v", theme)
	}
	for _, name := range []string{"运行 1 条命令，读取 1 个文件"} {
		label, ok := tt.Find(name)
		if !ok || label.H <= 0 || label.H > 24 || label.W <= label.H*2.5 {
			t.Fatalf("step label squeezed: %+v %v", label, ok)
		}
	}
}

func TestTimelineStyleRenders(t *testing.T) {
	output := os.Getenv("VANTRY_TIMELINE_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_TIMELINE_RENDER_DIR to export native timeline frames")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		width, height int
		dark          bool
	}{{1399, 900, true}, {1399, 900, false}, {1080, 760, true}, {640, 700, true}} {
		w := timelineStyleWorkbench()
		w.widgets.themeSet, w.widgets.dark = true, spec.dark
		tt := ui.NewTesterWith(w.view, spec.width, spec.height, ui.TesterOptions{Dark: spec.dark})
		tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
		w.logState("t1").ScrollTo(0, ui.Start)
		tt.Frame()
		name := fmt.Sprintf("timeline-%d-dark-%t.png", spec.width, spec.dark)
		file, err := os.Create(filepath.Join(output, name))
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
	}
}

func TestLiveActivityShowsToolTargetSilenceAndStop(t *testing.T) {
	now := time.Now().UTC()
	r := run("live-1", "t1", api.RunRunning, "check slow curl", "", 0)
	r.CreatedAt = now.Add(-45 * time.Second)
	w, _ := newViewWorkbench(r)
	w.tools = []api.ToolCall{
		{
			ID:        "tool-1",
			ThreadID:  "t1",
			RunID:     "live-1",
			Name:      "bash",
			Arguments: json.RawMessage(`{"command":"curl -s https://example.com"}`),
			Status:    "running",
			UpdatedAt: now.Add(-35 * time.Second),
		},
	}

	tt := ui.NewTester(w.view, 1280, 800)
	tt.SetLocale("zh-Hans")
	tt.Frame()

	texts := strings.Join(tt.Texts(), "\n")
	if !strings.Contains(texts, "正在运行") {
		t.Fatalf("missing active verb in liveActivity: %s", texts)
	}
	if !strings.Contains(texts, "curl -s https://example.com") {
		t.Fatalf("missing active tool target in liveActivity: %s", texts)
	}
	if !strings.Contains(texts, "静默") {
		t.Fatalf("missing silence indicator in liveActivity: %s", texts)
	}
	if _, ok := tt.Find("停止"); !ok {
		t.Fatalf("missing quick Stop button after 30s silence: %v", tt.Texts())
	}
}

func TestLiveActivityAndComposerTelemetryStrip(t *testing.T) {
	t.Run("compacting-live-activity", func(t *testing.T) {
		r := run("live-compact", "t1", api.RunRunning, "big file", "", 0)
		r.Telemetry = &api.RunTelemetry{Compacting: true, CompactionReason: "threshold"}
		w, _ := newViewWorkbench(r)
		tt := ui.NewTester(w.view, 1280, 800)
		tt.SetLocale("zh-Hans")
		tt.Frame()
		joined := strings.Join(tt.Texts(), "\n")
		if !strings.Contains(joined, "正在压缩上下文…") {
			t.Fatalf("missing compacting indicator: %s", joined)
		}
	})

	t.Run("retrying-live-activity", func(t *testing.T) {
		r := run("live-retry", "t1", api.RunRunning, "rate limit", "", 0)
		r.Telemetry = &api.RunTelemetry{Retrying: true, RetryAttempt: 2, RetryWaitSeconds: 5}
		w, _ := newViewWorkbench(r)
		tt := ui.NewTester(w.view, 1280, 800)
		tt.SetLocale("zh-Hans")
		tt.Frame()
		joined := strings.Join(tt.Texts(), "\n")
		if !strings.Contains(joined, "自动重试 #2 (5s)") {
			t.Fatalf("missing retry indicator: %s", joined)
		}
	})

	t.Run("composer-telemetry-strip-metered-vs-idle", func(t *testing.T) {
		r := run("metered", "t1", api.RunCompleted, "hi", "done", 0)
		r.Model = "magpie/deepseek-v4"
		r.Telemetry = &api.RunTelemetry{
			InputTokens:       6000,
			OutputTokens:      2500,
			CacheReadTokens:   4000,
			TotalTokens:       12500,
			LastContextTokens: 75000,
			CostUSD:           0.025,
			DurationMs:        2500,
			TTFTMs:            400,
		}
		w, _ := newViewWorkbench(r)
		w.models = []api.ModelInfo{{ID: "deepseek-v4", Provider: "magpie", Name: "DeepSeek V4", ContextWindow: 100000}}
		tt := ui.NewTester(w.view, 1280, 800)
		tt.SetLocale("zh-Hans")
		tt.Frame()

		joined := strings.Join(tt.Texts(), "\n")
		for _, want := range []string{"75%", "新增 8.5k", "缓存 4.0k (40%)", "$0.025"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("composer status footer missing %q in:\n%s", want, joined)
			}
		}

		// Fold run.telemetry live event updates the run's Telemetry.
		if !w.foldEvent(api.Event{
			Sequence: 99,
			ID:       "ev-tel",
			Type:     eventRunTelemetry,
			ThreadID: "t1",
			RunID:    "metered",
			Data:     json.RawMessage(`{"telemetry":{"totalTokens":15000,"lastContextTokens":88000,"inputTokens":7000,"outputTokens":3000,"cacheReadTokens":5000,"costUsd":0.031,"durationMs":2500}}`),
		}) {
			t.Fatal("foldEvent(run.telemetry) returned false")
		}
		tt.Frame()
		if !strings.Contains(strings.Join(tt.Texts(), "\n"), "88%") {
			t.Fatalf("expected updated context 88%% after run.telemetry fold: %v", tt.Texts())
		}

		// Idle thread with no telemetry renders no strip tokens.
		rPlain := run("plain", "t1", api.RunCompleted, "hi", "done", 0)
		wPlain, _ := newViewWorkbench(rPlain)
		ttPlain := ui.NewTester(wPlain.view, 1280, 800)
		ttPlain.SetLocale("zh-Hans")
		ttPlain.Frame()
		plainJoined := strings.Join(ttPlain.Texts(), "\n")
		if strings.Contains(plainJoined, "tok") || strings.Contains(plainJoined, "ctx ") {
			t.Fatalf("unexpected telemetry strip on idle untelemetered thread: %s", plainJoined)
		}
	})
}
