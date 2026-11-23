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

func TestDesignedSidebarPreservesNativeTypeSelection(t *testing.T) {
	w, _ := projectWorkbench()
	for id, title := range map[string]string{"t1": "Alpha session", "t2": "Bravo session", "thread-c": "Charlie session"} {
		thread, _ := w.threadByID(id)
		thread.Title = title
		w.mergeThread(thread)
	}
	w.selectThread("t1")
	tt := ui.NewTester(w.view, 1399, 820)
	mustClick(t, tt, "Alpha session")
	w.widgets.sidebarFocus.Focus()
	tt.Frame()
	tt.Key(0, ui.KeyB)
	if w.selected != "t2" {
		t.Fatalf("typing a title prefix no longer selects the matching native sidebar item: %q", w.selected)
	}
}

// designReviewWorkbench is a dense but realistic day: several projects in the
// sidebar (one running, one waiting on approval, a subtask), a finished turn
// with reasoning, steps, a long Markdown reply and changed files, then a live
// turn that asks for approval and has delegated a subtask.
func designReviewWorkbench() *workbench {
	first := "我先确认当前分支、工作区改动和 `release-main` 的状态，再执行合并、解决冲突并做相关验证；不会推送远程。\n\n"
	second := "当前分支是 `release-demo-merge`，工作区干净。本地 `release-main` 与远端一致；本次待合入的是图片预览拖动功能及对应回归文档。\n\n"
	final := "已将 `release-main` 合并到当前分支 `release-demo-merge`。\n\n" +
		"## 结果\n\n" +
		"- **合并提交**：`410351c3c`\n" +
		"- **冲突情况**：自动合并成功，无遗留冲突，工作区干净。\n" +
		"- **验证通过**：相关 ESLint、`canvas-drawing` 371 项测试，以及图片拖动、缩放、切图、标注和 Worker 预览的本地浏览器回归。\n\n" +
		"### 需要你留意\n\n" +
		"1. Worker 大图预览在低内存设备上会退回主线程渲染，这是有意保留的行为。\n" +
		"2. 回归文档新增了两条手工检查项，建议合并前过一遍。\n\n" +
		"```ts\nexport function shouldUseWorker(size: number): boolean {\n  return size > 4 * 1024 * 1024 && 'OffscreenCanvas' in globalThis\n}\n```\n\n" +
		"| 检查项 | 结果 |\n| --- | --- |\n| ESLint | 通过 |\n| canvas-drawing | 371 / 371 |\n\n" +
		"> 未推送远端；如需推送请在 Git 面板确认。\n"
	done := run("merge", "t1", api.RunCompleted, "把代码从 release-main 合并进来并解决冲突", first+second+final, 0)
	done.Reasoning = "正在核对工作目录、当前分支和变更范围。\nI'm trying to determine if it's necessary to do a full build, given that the TypeScript and Vue changes were minimal and the lint already covers them."
	done.UpdatedAt = done.CreatedAt.Add(3*time.Minute + 12*time.Second)
	done.Model = "fixture/gpt-6.1-sol"
	done.Telemetry = &api.RunTelemetry{
		InputTokens:       5200,
		OutputTokens:      1800,
		CacheReadTokens:   35800,
		TotalTokens:       42800,
		LastContextTokens: 100000,
		DurationMs:        19200,
	}
	live := run("live", "t1", api.RunRunning, "再跑一遍完整构建，确认没有类型错误，然后整理变更说明。", "好的，我先运行完整构建，再根据结果整理说明。\n\n", time.Hour)
	live.Model = "fixture/gpt-6.1-sol"
	live.Telemetry = &api.RunTelemetry{
		InputTokens:       6000,
		OutputTokens:      2100,
		CacheReadTokens:   34700,
		TotalTokens:       42800,
		LastContextTokens: 100000,
		DurationMs:        22400,
	}
	w, _ := newViewWorkbench(done, live)
	w.defaults.Version = "0.1.9"
	w.threads[0].Title = "梳理下完成度"
	w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/demo-chart-app", Branch: "release-demo-merge"}
	w.threads[0].Model = "fixture/gpt-6.1-sol"
	child := thread("child", "构建与类型检查", time.Hour+time.Minute)
	child.ParentThreadID, child.Workspace = "t1", cloneWorkspace(w.threads[0].Workspace)
	runningBg := thread("bg-live", "优化星舰推理滑块手感", time.Hour-3*time.Minute)
	runningBg.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/vantry-desktop", Branch: "feat/stardust-ui"}
	unreadBg := thread("bg-unread", "上游更新评估与前后端缺陷排查", -18*time.Minute)
	unreadBg.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/vantry-desktop", Branch: "main"}
	other := thread("other", "看下进展", -27*time.Minute)
	other.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/demo-chart", Branch: "feature/share-readonlypage"}
	notes := thread("notes", "整理周报要点", -3*time.Hour)
	w.threads = append(w.threads, child, runningBg, unreadBg, other, notes)
	bgRun := run("bg-live-run", "bg-live", api.RunRunning, "优化动画性能", "正在分析渲染帧率…", time.Hour-3*time.Minute)
	w.runs = append(w.runs,
		bgRun,
		run("bg-unread-run", "bg-unread", api.RunCompleted, "排查前后端缺陷", "已完成排查并输出修复清单。", -18*time.Minute),
		run("other-run", "other", api.RunCompleted, "看下进展", "进展良好。", -27*time.Minute),
	)
	w.markUnread("bg-unread")
	w.defaults.Workspace = cloneWorkspace(w.threads[0].Workspace)
	w.defaults.Model = "fixture/gpt-6.1-sol"
	w.features.catalog = &api.ModelCatalog{Default: w.defaults.Model, Models: []api.ModelInfo{
		{Provider: "fixture", ID: "gpt-6.1-sol", Name: "GPT-6.1-Sol · routing group", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 400000},
		{Provider: "fixture", ID: "claude-opus", Name: "Claude Opus 5.5", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 1000000},
		{Provider: "fixture", ID: "fast", Name: "Fast draft", Input: []string{"text"}, ContextWindow: 64000},
	}, ThinkingLevels: []string{"off", "low", "medium", "high", "max"}}
	w.features.catalogPath = w.modelWorkspace()
	w.models = append([]api.ModelInfo(nil), w.features.catalog.Models...)
	w.modelsLoaded = true
	w.acceptThinking(w.defaults.Model+":max", w.features.catalog.ThinkingLevels)
	w.threads[0].Model = "fixture/gpt-6.1-sol:max"
	at := func(n int) int { return n }
	w.tools = []api.ToolCall{
		{ID: "status", ThreadID: "t1", RunID: "merge", Name: "bash", Arguments: json.RawMessage(`{"command":"pwd; command -v rtk || true; git status --short --branch; git branch --show-current; git branch -a --list 'release-*'"}`), Status: "completed", OutputOffset: at(len(first)), Output: "## release-demo-merge"},
		{ID: "skill", ThreadID: "t1", RunID: "merge", Name: "read", Arguments: json.RawMessage(`{"path":"/Users/me/.agents/skills/karpathy-guidelines/SKILL.md"}`), Status: "completed", OutputOffset: len(first), Output: "# guidelines"},
		{ID: "fetch", ThreadID: "t1", RunID: "merge", Name: "bash", Arguments: json.RawMessage(`{"command":"git fetch origin release-main && git log --oneline -3 origin/release-main"}`), Status: "failed", OutputOffset: len(first), Output: "fatal: could not read Username"},
		{ID: "merge", ThreadID: "t1", RunID: "merge", Name: "bash", Arguments: json.RawMessage(`{"command":"git merge --no-ff release-main"}`), Status: "completed", OutputOffset: len(first) + len(second), Output: "Merge made by the 'ort' strategy."},
		{ID: "edit", ThreadID: "t1", RunID: "merge", Name: "edit", Arguments: json.RawMessage(`{"path":"/Projects/demo-chart-app/src/preview/worker.ts","edits":[{"oldText":"return size > 8 * 1024 * 1024","newText":"return size > 4 * 1024 * 1024 && 'OffscreenCanvas' in globalThis"}]}`), Status: "completed", OutputOffset: len(first) + len(second)},
		{ID: "build", ThreadID: "t1", RunID: "live", Name: "bash", Arguments: json.RawMessage(`{"command":"pnpm -w build && pnpm -w typecheck"}`), Status: "running", OutputOffset: -1},
	}
	w.requests = []api.ProviderRequest{{ID: "approve", ThreadID: "t1", RunID: "live", Method: "confirm", Title: "允许执行 bash？", Message: `{"command":"pnpm -w build && pnpm -w typecheck"}`, Status: "pending"}}
	w.tasks = []api.Task{
		{ID: "task", ParentThreadID: "t1", ParentRunID: "live", ChildThreadID: "child", Title: "构建与类型检查", Status: api.RunRunning, Generation: 1},
		{ID: "bg-t1", ParentThreadID: "bg-live", ParentRunID: "bg-live-run", Title: "离线贴图预烘焙", Status: api.RunCompleted, Generation: 1},
		{ID: "bg-t2", ParentThreadID: "bg-live", ParentRunID: "bg-live-run", Title: "2 秒静止休眠验证", Status: api.RunRunning, Generation: 1},
		{ID: "bg-t3", ParentThreadID: "bg-live", ParentRunID: "bg-live-run", Title: "导出明暗对比截屏", Status: api.RunQueued, Generation: 1},
	}
	w.setDraft("t1", "")
	return w
}

type designSpec struct {
	width, height int
	dark          bool
	scale         float32
}

var designSpecs = []designSpec{
	{1440, 900, true, 1}, {1440, 900, false, 1}, {1399, 820, true, 2}, {1080, 760, true, 1}, {1080, 760, false, 1}, {640, 760, true, 1}, {1280, 600, true, 1},
}

func renderDesignFrame(t *testing.T, dir, name string, spec designSpec, setup func(*workbench, *ui.Tester)) {
	t.Helper()
	w := designReviewWorkbench()
	w.widgets.themeSet, w.widgets.dark = true, spec.dark
	tt := ui.NewTesterWith(w.view, spec.width, spec.height, ui.TesterOptions{Dark: spec.dark})
	tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
	if spec.scale > 1 {
		tt.SetScale(spec.scale)
	}
	tt.Advance(time.Second)
	if setup != nil {
		setup(w, tt)
	}
	tt.Advance(600 * time.Millisecond)
	tt.Frame()
	file, err := os.Create(filepath.Join(dir, fmt.Sprintf("%s-%dx%d-%s.png", name, spec.width, spec.height, map[bool]string{true: "dark", false: "light"}[spec.dark])))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

// TestDesignReviewRenders exports the design acceptance frames:
// VANTRY_DESIGN_RENDER_DIR=/abs/dir go test ./internal/desktop -run TestDesignReviewRenders
func TestDesignReviewRenders(t *testing.T) {
	dir := os.Getenv("VANTRY_DESIGN_RENDER_DIR")
	if dir == "" {
		t.Skip("set VANTRY_DESIGN_RENDER_DIR to export design review frames")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	top := func(w *workbench, tt *ui.Tester) {
		w.logState("t1").ScrollTo(0, ui.Start)
		tt.Frame()
	}
	for _, spec := range designSpecs {
		renderDesignFrame(t, dir, "timeline-top", spec, top)
		renderDesignFrame(t, dir, "timeline-live", spec, nil)
		if spec.width == 1440 {
			renderDesignFrame(t, dir, "stardust-workbench", spec, func(w *workbench, tt *ui.Tester) {
				// Enrich showcase fixture (test-only) with artifacts, expanded task
				// orchestration checklist, and the right Artifacts inspector open.
				now := tt.Now()
				w.runs[0].UpdatedAt = now.Add(-12 * time.Minute)
				w.runs[1].UpdatedAt = now.Add(-2 * time.Minute)
				w.runs[0].Output += "\n\n```html title=\"release-checklist.html\"\n<div class=\"card\"><h3>Stardust v0.1.9 Release</h3><p>All checks passed.</p></div>\n```\n\n```svg title=\"status-ring.svg\"\n<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 24 24\" fill=\"none\" stroke=\"currentColor\" stroke-width=\"2\"><circle cx=\"12\" cy=\"12\" r=\"9\"/><path d=\"m9 12 2 2 4-4\"/></svg>\n```\n"
				w.runs[1].Output += "\n```mermaid title=\"architecture-flow.mmd\"\nflowchart LR\n  A[会话] --> B[产物]\n```\n"
				w.tasks = append(w.tasks,
					api.Task{ID: "t-done", ParentThreadID: "t1", ParentRunID: "live", Title: "核对分支差异与合并状态", Status: api.RunCompleted, Generation: 1},
					api.Task{ID: "t-wait1", ParentThreadID: "t1", ParentRunID: "live", Title: "生成发布说明与回归清单", Status: api.RunQueued, Generation: 1},
					api.Task{ID: "t-wait2", ParentThreadID: "t1", ParentRunID: "live", Title: "导出明暗主题视觉验收截图", Status: api.RunQueued, Generation: 1},
				)
				w.widgets.orchestrationExpanded = map[string]bool{"t1": true}
				w.widgets.workspaceOpen = true
				w.widgets.rightPanelTab = "artifacts"
				tt.Frame()
			})
			renderDesignFrame(t, dir, "home", spec, func(w *workbench, tt *ui.Tester) {
				w.newSession()
			})
		}
		if spec.width >= 1080 && spec.dark {
			renderDesignFrame(t, dir, "model-popover", spec, func(w *workbench, tt *ui.Tester) {
				w.widgets.modelPickerOpen = true
			})
			renderDesignFrame(t, dir, "steps-open", spec, func(w *workbench, tt *ui.Tester) {
				w.widgets.reasoningOpen = map[string]bool{"merge": true}
				w.widgets.workOpen = map[string]bool{"merge:1": true}
				w.widgets.toolExpanded = map[string]bool{"fetch": true}
				top(w, tt)
			})
			if spec.width != 1440 {
				renderDesignFrame(t, dir, "home", spec, func(w *workbench, tt *ui.Tester) {
					w.newSession()
				})
			}
			renderDesignFrame(t, dir, "hover", spec, func(w *workbench, tt *ui.Tester) {
				top(w, tt)
				if meta, ok := tt.Find("已完成 · 3 分 12 秒"); ok {
					tt.Move(meta.X+meta.W+40, meta.Y+meta.H/2)
					tt.Advance(100 * time.Millisecond)
				}
				if row, ok := tt.Find("看下进展"); ok {
					tt.Move(row.X+20, row.Y+4)
					tt.Advance(700 * time.Millisecond)
				}
			})
		}
	}
}

// TestDesignAcceptanceAcrossWindows checks the layout promises of
// docs/desktop-design-next.md in every review window: the composer and the
// approval actions stay reachable, the reading column keeps its measure,
// fold labels never wrap into vertical text, the approval state is stated
// once (by its card), and the title bar carries no counter sentence.
func TestDesignAcceptanceAcrossWindows(t *testing.T) {
	for _, spec := range designSpecs {
		t.Run(fmt.Sprintf("%dx%d-dark-%t", spec.width, spec.height, spec.dark), func(t *testing.T) {
			w := designReviewWorkbench()
			w.widgets.themeSet, w.widgets.dark = true, spec.dark
			tt := ui.NewTesterWith(w.view, spec.width, spec.height, ui.TesterOptions{Dark: spec.dark})
			tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
			tt.Advance(time.Second)
			assertControlInWindow(t, tt, "消息输入框", spec.width, spec.height)
			assertControlInWindow(t, tt, "允许执行", spec.width, spec.height)
			assertControlInWindow(t, tt, "拒绝", spec.width, spec.height)
			if card, ok := tt.Find("允许执行 bash？"); !ok || card.W > readingWidth {
				t.Fatalf("approval outside the reading column: %+v %v", card, ok)
			}
			count := 0
			for _, text := range tt.Texts() {
				if text == "需要你的确认" {
					count++
				}
				if strings.HasSuffix(text, "轮对话") {
					t.Fatalf("title bar shows a turn counter sentence: %q", text)
				}
			}
			if count != 1 {
				t.Fatalf("approval state stated %d times, want once by its card", count)
			}
			if _, ok := tt.Access("跳转到对话轮次"); !ok {
				t.Fatal("turn outline lost its accessible trigger")
			}
			w.logState("t1").ScrollTo(0, ui.Start)
			tt.Frame()
			for _, label := range []string{"思考过程", "运行 2 条命令，读取 1 个文件"} {
				r, ok := tt.Find(label)
				if !ok || r.H > 24 || r.W <= r.H*2.5 {
					t.Fatalf("%q squeezed or missing: %+v %v", label, r, ok)
				}
			}
			// The reading column is centred in the stage at its measure.
			reply, ok := tt.Find("结果")
			if !ok {
				t.Fatal("reply heading missing")
			}
			side := float32(0)
			switch {
			case spec.width >= 1180:
				side = sidebarWide
			case spec.width >= narrowWidth:
				side = sidebarNarrow
			}
			stage := float32(spec.width) - side - 56
			if stage > readingWidth {
				want := side + 28 + (stage-readingWidth)/2
				if reply.X < want-4 || reply.X > want+4 {
					t.Fatalf("reading column not centred at its measure: heading x=%v want≈%v", reply.X, want)
				}
			}
		})
	}
}

// TestDesignOpenReasoningStaysInColumn guards the open reasoning quote:
// its text must wrap inside the reading column, not run past it.
func TestDesignOpenReasoningStaysInColumn(t *testing.T) {
	for _, width := range []int{1440, 1080, 640} {
		w := designReviewWorkbench()
		w.widgets.reasoningOpen = map[string]bool{"merge": true}
		tt := ui.NewTester(w.view, width, 900)
		w.logState("t1").ScrollTo(0, ui.Start)
		tt.Frame()
		body, ok := tt.Find(w.runs[0].Reasoning)
		toggle, okToggle := tt.AccessByRole(ui.RoleButton, "收起思考过程")
		if !ok || !okToggle {
			t.Fatalf("%d: open reasoning missing: %v %v", width, ok, okToggle)
		}
		// The fold row spans the reading column; its quote must end inside.
		right := toggle.Bounds.X + toggle.Bounds.W
		if body.X+body.W > right+1 {
			t.Fatalf("%d: reasoning runs past the column: body %+v column right %v", width, body, right)
		}
	}
}

// TestDesignQuietHistoryAndFeedback checks the interaction details: older
// turns keep their actions reachable, the latest turn's copy confirms in
// place, and a project's own rows do not repeat its name.
func TestDesignQuietHistoryAndFeedback(t *testing.T) {
	w := designReviewWorkbench()
	w.runs[1].Status = api.RunCompleted
	w.runs[1].UpdatedAt = w.runs[1].CreatedAt.Add(12 * time.Second)
	w.requests = nil
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	if _, ok := tt.AccessByRole(ui.RoleButton, "复制回复"); !ok {
		t.Fatal("copy reply action unreachable")
	}
	mustClick(t, tt, "复制回复")
	if _, ok := tt.Access("已复制"); !ok {
		t.Fatal("copy did not confirm in place")
	}
	tt.Advance(2 * time.Second)
	if _, ok := tt.Access("已复制"); ok {
		t.Fatal("copy confirmation did not expire")
	}
	row, ok := tt.AccessByRole(ui.RoleTreeItem, "梳理下完成度")
	if !ok || row.Bounds.H < rowHeight-2 || row.Bounds.H > rowHeight+4 {
		t.Fatalf("thread row is not the list's two-line block: %+v %v", row, ok)
	}
	// Every row carries its project and branch in its accessible description.
	if !strings.Contains(row.Description, "demo-chart-app") || !strings.Contains(row.Description, "release-demo-merge") {
		t.Fatalf("row does not name its project and branch: %+v", row)
	}
}
