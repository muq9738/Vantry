package desktop

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestParseDispatchSpecPipeAndJSON(t *testing.T) {
	pipe := `多路并行重构计划
* 调研接口与依赖 | role=research | model=astra | isolation=shared | 梳理现有模块边界
* 批量生成清洗脚本 | role=implementation | model=deepseek | count=5 | isolation=worktree | 分 5 路并行
- 补齐边界单测 | test | model=swe-2 | x2 | 覆盖失败场景`
	spec := parseDispatchSpec(pipe)
	if spec.Title != "多路并行重构计划" {
		t.Fatalf("unexpected title: %q", spec.Title)
	}
	if len(spec.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(spec.Items))
	}
	if !spec.Items[0].Selected || spec.Items[0].Role != "research" || spec.Items[0].Model != "astra" || spec.Items[0].Isolation != "shared" {
		t.Fatalf("unexpected item[0]: %+v", spec.Items[0])
	}
	if !spec.Items[1].Selected || spec.Items[1].Count != 5 || spec.Items[1].Isolation != "worktree" {
		t.Fatalf("unexpected item[1]: %+v", spec.Items[1])
	}
	if spec.Items[2].Selected || spec.Items[2].Role != "test" || spec.Items[2].Count != 2 {
		t.Fatalf("unexpected item[2]: %+v", spec.Items[2])
	}

	rawJSON := `{"title":"JSON 计划","tasks":[{"title":"UI 交互打磨","role":"design","model":"opus-5.5","isolation":"worktree","count":2,"selected":true}]}`
	jspec := parseDispatchSpec(rawJSON)
	if jspec.Title != "JSON 计划" || len(jspec.Items) != 1 || jspec.Items[0].Role != "design" || jspec.Items[0].Count != 2 {
		t.Fatalf("unexpected JSON spec: %+v", jspec)
	}
}

func TestDispatchCardConfirmAdjustModelAndCount(t *testing.T) {
	reply := "```dispatch\n脚本与调研并行编排\n" +
		"* 调研接口结构 | role=research | model=astra | isolation=shared | 梳理调用链\n" +
		"* 批量生成脚本 | role=implementation | model=deepseek | count=3 | isolation=worktree | 分模块并行\n" +
		"```"
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "安排一下", reply, 0))
	w.models = []api.ModelInfo{
		{Provider: "example-a", ID: "group/gtp-6-astra", Name: "gtp-6-astra"},
		{Provider: "example-b", ID: "group/deepseek-v4.1-flash", Name: "deepseek-v4.1-flash"},
		{Provider: "example-a", ID: "group/claude-opus-5.5", Name: "claude-opus-5.5"},
	}

	tt := ui.NewTester(w.view, 1280, 1200)
	tt.Advance(time.Second)

	// Verify rendered header, resolved model short labels, and parallel summary (1 + 3 = 4 workers).
	for _, want := range []string{"脚本与调研并行编排", "2 项 · 4 路", "gtp-6-astra", "deepseek-v4.1-flash"} {
		if !tt.HasText(want) {
			t.Fatalf("missing expected dispatch card text %q in %v", want, tt.Texts())
		}
	}

	// Open model picker for the first row (gtp-6-astra) and switch it to claude-opus-5.5.
	mustClick(t, tt, "选择模型: gtp-6-astra")
	tt.Frame()
	mustClick(t, tt, "claude-opus-5.5")
	tt.Frame()

	// Confirm dispatch.
	mustClick(t, tt, "开始编排 · 4 路")
	tt.Frame()

	commands := f.dispatched()
	if len(commands) != 1 {
		t.Fatalf("expected 1 dispatched confirmation command, got %+v", commands)
	}
	msg := commands[0].Message
	for _, want := range []string{
		"Promise.allSettled",
		"example-a/group/claude-opus-5.5",
		"example-b/group/deepseek-v4.1-flash",
		"并行 3 路",
		"isolation=worktree",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("confirmation message missing %q:\n%s", want, msg)
		}
	}
	if !tt.HasText("已派发 4 路：") {
		t.Fatalf("dispatch card did not collapse after confirmation: %v", tt.Texts())
	}
}

func TestDispatchCardDirectFallback(t *testing.T) {
	reply := "```dispatch\n简单排查\n* 查日志 | role=research | model=deepseek | 看报错\n```"
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "查一下", reply, 0))
	tt := ui.NewTester(w.view, 1200, 900)
	tt.Advance(time.Second)

	mustClick(t, tt, "直接做")
	tt.Frame()

	commands := f.dispatched()
	if len(commands) != 1 || !strings.Contains(commands[0].Message, "不用拆分派发子代理") {
		t.Fatalf("expected direct-execution reply, got %+v", commands)
	}
	if !tt.HasText("不拆分，主会话直接执行") {
		t.Fatalf("dispatch card did not collapse on direct fallback: %v", tt.Texts())
	}
}

func TestDispatchCardRender(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR")
	}
	reply := "```dispatch\nVantry 编排试跑（只读巡检）\n" +
		"* 核对 Core 到 Pi Worker 透传链路 | role=research | model=astra | 检查 ParentTaskID 与请求流向\n" +
		"* 复核 dispatch_card.go 交互与边界状态 | role=design | model=opus\n" +
		"* 扫描 desktop 包内未使用的辅助函数 | role=implementation | model=deepseek | count=2\n" +
		"- 检查近期改动的边界单测覆盖 | role=test | model=sol | isolation=worktree | 默认未勾选\n" +
		"```"
	for _, dark := range []bool{false, true} {
		w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "安排一下", reply, 0))
		w.models = []api.ModelInfo{
			{Provider: "example-a", ID: "group/gtp-6-astra", Name: "gtp-6-astra"},
			{Provider: "example-b", ID: "group/deepseek-v4.1-flash", Name: "deepseek-v4.1-flash"},
			{Provider: "example-a", ID: "group/claude-opus-5.5", Name: "claude-opus-5.5"},
			{Provider: "example-a", ID: "group/gpt-6-sol", Name: "gpt-6-sol"},
		}
		tt := ui.NewTesterWith(w.view, 1180, 820, ui.TesterOptions{Dark: dark})
		tt.Advance(time.Second)
		if dark {
			mustClick(t, tt, "选择模型: deepseek-v4.1-flash")
			tt.Advance(time.Second)
		}
		name := map[bool]string{false: "dispatch-light.png", true: "dispatch-dark-picker.png"}[dark]
		f, err := os.Create(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
}
