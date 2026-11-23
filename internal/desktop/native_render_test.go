package desktop

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// TestNativeInteractionRenders uses MyGo's real offscreen renderer with local
// fixtures. It neither opens an OS window nor calls a paid model. Set the
// output directory explicitly to export PNGs for visual review.
func TestNativeInteractionRenders(t *testing.T) {
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
	}{{"1440-light", 1440, 900, false}, {"1080-dark", 1080, 700, true}, {"640-narrow", 640, 600, false}, {"640-short", 640, 420, false}} {
		for _, panel := range []string{"conversation", "models", "session", "git", "plugins", "remote", "terminal"} {
			t.Run(spec.name+"-"+panel, func(t *testing.T) {
				w := nativeReviewWorkbench()
				if spec.height == 420 {
					w.setDraft("t1", strings.Repeat("长草稿仍可继续编辑。\n", 8))
					for i := 0; i < 7; i++ {
						w.composerFor("t1").attachments = append(w.composerFor("t1").attachments, api.Attachment{ID: fmt.Sprint(i), Name: fmt.Sprintf("file-%d.txt", i)})
					}
					for i := 0; i < 6; i++ {
						w.messages = append(w.messages, api.QueuedMessage{ID: fmt.Sprintf("queued-%d", i), ThreadID: "t1", Status: "pending", Message: "下一条排队消息"})
					}
				}
				if panel == "terminal" {
					// Render the real libghostty-vt view, with an in-memory connection.
					// No login shell, user's startup files or remote host is involved.
					client, server := net.Pipe()
					t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
					term, err := terminal.New(terminal.Options{Conn: client, NoBlink: true})
					if err != nil {
						t.Fatal(err)
					}
					w.features.terminals = map[string]*terminal.Terminal{"t1": term}
					t.Cleanup(w.closeTerminals)
				}
				if panel == "models" {
					w.widgets.modelPickerOpen = true
				} else if panel != "conversation" {
					w.features.panel = panel
				}
				tt := ui.NewTesterWith(w.view, spec.width, spec.height, ui.TesterOptions{Dark: spec.dark})
				if term := w.features.terminals["t1"]; term != nil {
					term.Feed([]byte("\x1b[35mVantry native terminal\x1b[0m\r\n/Projects/Vantry\r\n$ local layout fixture (no shell started)\r\n"))
					tt.Frame()
				}
				tt.Advance(time.Second)
				file, err := os.Create(filepath.Join(output, spec.name+"-"+panel+".png"))
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
			})
		}
	}
}

func TestFirstRunModelPickerRenders(t *testing.T) {
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
	}{{"1440-light", 1440, 900, false}, {"1080-dark", 1080, 700, true}, {"640-narrow", 640, 600, false}, {"640-short", 640, 420, false}} {
		t.Run(spec.name+"-welcome-models", func(t *testing.T) {
			service := newFakeQueryService()
			service.models = []api.ModelInfo{
				{ID: "example-provider/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Provider: "example-provider"},
				{ID: "kimi-for-coding", Name: "Kimi For Coding", Provider: "kimi"},
				{ID: "long-model-name-for-layout", Name: "Layout fixture · Reasoning", Provider: "fixture"},
			}
			w := newWorkbench(service)
			w.applySnapshot(api.Snapshot{})
			tt := ui.NewTesterWith(w.view, spec.width, spec.height, ui.TesterOptions{Dark: spec.dark})
			tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
			tt.Advance(time.Second)
			for _, name := range []string{"消息输入框", "选择模型", "重新载入"} {
				assertControlInWindow(t, tt, name, spec.width, spec.height)
			}
			file, err := os.Create(filepath.Join(output, spec.name+"-welcome-models.png"))
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
		})
	}
}

func nativeReviewWorkbench() *workbench {
	w, _ := newViewWorkbench(run("review-run", "t1", api.RunRunning,
		"检查自然交互、模型、附件和项目导航。",
		"我会先梳理项目导航和输入区。\n\n- 草稿按会话保留，切换后继续编辑。\n- 回复期间也可以准备下一条消息。\n- 查看差异、终端和子任务时，保留当前阅读位置。", 0))
	w.threads[0].Title = "原生交互 · 模型选择与输入体验"
	w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry"}
	w.threads[0].Model = "fixture/long-model-name-for-layout"
	child := thread("child", "检查附件与输入法边界", time.Minute)
	child.ParentThreadID, child.Workspace = "t1", cloneWorkspace(w.threads[0].Workspace)
	other := thread("other", "资料整理与项目笔记", time.Hour)
	other.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Notes"}
	w.threads = append(w.threads, child, other)
	w.defaults.Workspace = cloneWorkspace(w.threads[0].Workspace)
	w.defaults.Model = "fixture/long-model-name-for-layout"
	w.features.catalog = &api.ModelCatalog{Default: w.defaults.Model, ConfiguredDefault: "extension/configured-default", Warnings: []string{"当前目录包含 Pi 内置模型和已配置的可用模型；由扩展注册的模型暂不可用。"}, Models: []api.ModelInfo{
		{Provider: "fixture", ID: "long-model-name-for-layout", Name: "Layout fixture · Reasoning", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000},
		{Provider: "fixture", ID: "fast", Name: "Layout fixture · Fast", Input: []string{"text"}, ContextWindow: 64000},
	}, ThinkingLevels: []string{"off", "low", "medium", "high"}}
	w.features.catalogPath = w.modelWorkspace()
	w.models = append([]api.ModelInfo(nil), w.features.catalog.Models...)
	w.modelsLoaded = true
	w.acceptThinking(w.defaults.Model, w.features.catalog.ThinkingLevels)
	w.setDraft("t1", "下一条草稿：检查窄窗口中的工具栏。\n模型和附件应保留清楚的反馈。 @交互参考长文件名.png")
	comp := w.composerFor("t1")
	comp.attachments = []api.Attachment{{ID: "preview-image", Name: "交互参考长文件名.png", MIME: "image/png", Size: 184320}}
	comp.addMention(composerMention{kind: mentionAttachment, display: "@交互参考长文件名.png", expand: "@交互参考长文件名.png", attachmentID: "preview-image"})
	rememberAttachmentPreview(comp.attachments[0], reviewPNG())
	w.runs = append(w.runs,
		run("review-2", "t1", api.RunCompleted, "第二轮：把 [🖼 交互参考长文件名.png] 留在气泡里", "缩略图在消息里，引用只是文件名。", 2*time.Minute),
		run("review-3", "t1", api.RunCompleted, "第三轮：看导航点是否对齐", "每个用户轮次一个点，当前轮次高亮。", 3*time.Minute),
		run("review-4", "t1", api.RunCompleted, "第四轮：悬停时预览这一轮", "第一行是用户消息，下面是回复摘要。", 4*time.Minute),
	)
	w.runs[1].Attachments = []api.Attachment{{ID: "sent-image", Name: "交互参考长文件名.png", MIME: "image/png", Size: 184320}}
	rememberAttachmentPreview(w.runs[1].Attachments[0], reviewPNG())
	w.features.git = map[string]*api.GitState{"t1": {
		Root: "/Projects/Vantry", Branch: "desktop-interactions", Branches: []string{"main", "desktop-interactions"},
		Status: " M internal/desktop/view.go", Files: []api.GitFile{{Path: "internal/desktop/view.go", Index: " ", Worktree: "M"}},
		Diff: "[未暂存 / unstaged]\ndiff --git a/internal/desktop/view.go b/internal/desktop/view.go\n--- a/internal/desktop/view.go\n+++ b/internal/desktop/view.go\n@@ -1 +1 @@\n-single crowded row\n+adaptive actions and visible focus\n",
	}}
	w.features.gitOutputs = map[string]string{"t1": "合并提交已创建；hook 留下了未暂存改动，以上状态已重新读取。"}
	w.steering = []api.SteeringMessage{{ID: "review-steering", CommandID: "review-steer-command", ThreadID: "t1", RunID: "review-run", AttemptID: "review-run-attempt", Message: "停止前再检查附件边界。", Status: api.SteeringAccepted, Disposition: "queued", CreatedAt: baseTime, UpdatedAt: baseTime}}
	w.features.plugins = []api.PluginInfo{{ID: "notes", Name: "项目笔记", Description: "读取当前草稿并整理为本地笔记"}}
	return w
}

func reviewPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 56, G: 140, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
