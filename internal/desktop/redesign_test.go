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

// Explicit render fixtures make the actual native view reviewable without
// conflating representative content with a real provider execution.
func TestRedesignNativeFrames(t *testing.T) {
	directory := os.Getenv("VANTRY_RENDER_DIR")
	if directory == "" {
		t.Skip("native screenshot export is opt-in")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name          string
		width, height int
		dark          bool
		mode          string
	}{
		{"01-home-dark", 1280, 820, true, "home"},
		{"02-chat-dark", 1280, 820, true, "chat"},
		{"03-approval-dark", 1280, 820, true, "approval"},
		{"04-workspace-dark", 1280, 820, true, "workspace"},
		{"05-model-picker", 1280, 820, true, "model"},
		{"06-home-light", 1280, 820, false, "home"},
		{"07-chat-compact", 760, 540, true, "chat"},
	} {
		t.Run(spec.name, func(t *testing.T) {
			w, _ := newViewWorkbench()
			w.defaults.Model = "example-provider/deepseek-v4.1-flash"
			w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Users/demo/Develop/Vantry"}
			w.widgets.themeSet, w.widgets.dark = true, spec.dark
			w.connNotice = notice{}
			w.modelsLoaded = true
			w.models = []api.ModelInfo{
				{ID: "example-provider/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Provider: "example-provider"},
				{ID: "example-provider/gpt-6.1-sol", Name: "GPT 6.1 Sol", Provider: "example-provider"},
				{ID: "example-provider/kimi3.1", Name: "Kimi 3.1", Provider: "example-provider"},
			}
			if spec.mode == "home" || spec.mode == "model" {
				w.threads = nil
				w.selected = ""
				w.newSession()
				w.widgets.modelPickerOpen = spec.mode == "model"
			} else {
				answer := "已经把项目的入口和主要调用关系整理出来了。\n\n## 三条主链路\n\n- **原生界面**负责输入、对话阅读和操作反馈。\n- **独立 Core**保存会话、任务和执行状态。\n- **Pi worker**执行模型请求和工具，窗口关闭后仍可继续。\n\n接下来先收紧任务状态的边界，再补上最常用的交互。"
				w.runs = []api.Run{run("r1", "t1", api.RunCompleted, "帮我梳理一下这个项目的结构，以及下一步该先改哪里。", answer, 0)}
				w.runs[0].UpdatedAt = w.runs[0].CreatedAt.Add(18 * time.Second)
				w.threads[0].Model = w.defaults.Model
				w.threads[0].Workspace = cloneWorkspace(w.defaults.Workspace)
				w.threads[0].Title = "梳理项目结构与下一步"
				w.tools = []api.ToolCall{{ID: "tool1", ThreadID: "t1", RunID: "r1", Name: "read", Status: "completed", Arguments: json.RawMessage(`{"path":"README.md"}`)}}
				if spec.mode == "approval" {
					w.runs[0].Status = api.RunRunning
					w.runs[0].Output = "我会先读取项目入口，然后运行现有测试确认基线。"
					w.tools = []api.ToolCall{{ID: "tool1", ThreadID: "t1", RunID: "r1", Name: "bash", Status: "running", Arguments: json.RawMessage(`{"command":"go test ./... -timeout=60s"}`)}}
					w.requests = []api.ProviderRequest{{ID: "request1", ThreadID: "t1", RunID: "r1", Method: "confirm", Title: "允许执行 bash？", Message: `{"command":"go test ./... -timeout=60s"}`, Status: "pending"}}
				}
				if spec.mode == "workspace" {
					w.widgets.workspaceOpen = true
					w.workspaceChanges = &api.WorkspaceChanges{Path: w.defaults.Workspace.Path, Branch: "main", IsGit: true, Files: []api.WorkspaceFile{{Path: "internal/desktop/view.go", Status: " M"}, {Path: "internal/desktop/composer_view.go", Status: "??"}, {Path: "docs/desktop-ui.md", Status: " M"}}, Diff: "diff --git a/view.go b/view.go\n--- a/view.go\n+++ b/view.go\n@@ -1,2 +1,2 @@\n-old layout\n+native conversation layout"}
				}
			}
			tester := ui.NewTester(w.view, spec.width, spec.height)
			tester.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
			file, err := os.Create(filepath.Join(directory, spec.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			if err := png.Encode(file, tester.Image()); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
