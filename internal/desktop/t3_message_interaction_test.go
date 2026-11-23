package desktop

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func clickComposerAction(t *testing.T, tt *ui.Tester, name string, mods ui.Modifiers) {
	t.Helper()
	button, ok := tt.AccessByRole(ui.RoleButton, name)
	if !ok {
		t.Fatalf("action %q missing", name)
	}
	b := button.Bounds
	tt.ClickAtWith(mods, b.X+b.W/2, b.Y+b.H/2)
}

func TestT3RunningComposerSteersByDefaultAndModifierQueues(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunRunning, "执行任务", "处理中", 0))
	service.receipt = steeringReceipt
	tt := ui.NewTester(w.view, 1080, 760)
	mustClick(t, tt, "消息输入框")
	tt.Type("先检查取消边界")
	tt.Key(0, ui.KeyEnter)
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.SteerRun || commands[0].Message != "先检查取消边界" {
		t.Fatalf("default action did not steer: %+v", commands)
	}
	w.setDraft("t1", "这条等当前任务结束后执行")
	tt.Frame()
	clickComposerAction(t, tt, "追加信息", ui.Cmd)
	commands = service.dispatched()
	if len(commands) != 2 || commands[1].Kind != api.QueueMessage || commands[1].RunID != "" {
		t.Fatalf("modifier did not queue independently: %+v", commands)
	}
}

func TestT3RunningInputPreferenceSurvivesRestart(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.defaults.StatePath = filepath.Join(t.TempDir(), "desktop-state.json")
	w.widgets.steerMode = false
	w.saveState()
	restored := newWorkbench(&fakeService{})
	restored.defaults.StatePath = w.defaults.StatePath
	if err := restored.restoreState(); err != nil {
		t.Fatal(err)
	}
	if restored.widgets.steerMode {
		t.Fatal("explicit queue default was lost on restart")
	}
}

func TestT3QueueIsInlineAndCancelEditingRestoresDraft(t *testing.T) {
	w, service, _ := queueFixture()
	w.setDraft("t1", "尚未发送的草稿")
	tt := ui.NewTester(w.view, 1080, 760)
	if !tt.HasText("第一条排队") || !tt.HasText("第二条排队") {
		t.Fatal("queue must be visible without opening a popover")
	}
	clickComposerAction(t, tt, "编辑排队消息", 0)
	if w.composerFor("t1").draft != "第一条排队" {
		t.Fatal("queue edit did not enter the main composer")
	}
	w.setDraft("t1", "临时编辑")
	tt.Frame()
	mustClick(t, tt, "取消编辑")
	if w.composerFor("t1").draft != "尚未发送的草稿" || len(service.dispatched()) != 0 {
		t.Fatal("cancel lost the draft or dispatched work")
	}
	clickComposerAction(t, tt, "立即发送", 0)
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.PromoteInputToSteer || commands[0].InputID != "in-1" {
		t.Fatalf("send-now was not atomic: %+v", commands)
	}
}

func TestQueuedPlanEditHidesInstructionsAndRestoresOriginalMode(t *testing.T) {
	w, service, inputs := queueFixture()
	input := inputs[0]
	input.Message = applyInteractionMode(true, "排队的计划")
	w.inputs[0] = input
	w.setDraft("t1", "未发送的普通草稿")
	w.beginQueueComposerEdit(input)
	comp := w.composerFor("t1")
	if comp.draft != "排队的计划" || !comp.plan {
		t.Fatal("queue edit exposed implementation instructions or lost plan mode")
	}
	w.setDraft("t1", "更新后的计划")
	if !w.saveQueueComposerEdit("t1") {
		t.Fatal("save failed")
	}
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Message != applyInteractionMode(true, "更新后的计划") {
		t.Fatalf("plan edit changed mode: %+v", commands)
	}
	if comp.draft != "未发送的普通草稿" || comp.plan {
		t.Fatal("original draft mode was not restored")
	}
}

func TestFileReaderUsesDockWidthMarkdownPreviewAndWrappedSource(t *testing.T) {
	body := "# 阅读文档\n\n" + strings.Repeat("这里的长段落必须完整显示，不应向右被截断。", 35) + "\n\n完整结尾标记\n"
	for _, width := range []int{1080, 1920} {
		w := newWorkbench(&fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
			if req.Action == actionFilesRead {
				return api.WorkbenchResponse{Output: body}, nil
			}
			return api.WorkbenchResponse{}, nil
		}})
		th := thread("t1", "Thread", 0)
		th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
		w.applySnapshot(api.Snapshot{Threads: []api.Thread{th}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "请阅读说明", "查看 [阅读文档](/p/docs/guide.md)。", 0)}})
		w.replyLink("/p/docs/guide.md")
		tt := ui.NewTester(w.view, width, 900)
		tt.Advance(time.Second)
		if !tt.HasText("完整结尾标记") {
			t.Fatal("Markdown preview lost file contents")
		}
		preview, ok := tt.Access("文件预览 docs/guide.md")
		if !ok || preview.Bounds.W < 390 || preview.Bounds.X+preview.Bounds.W > float32(width) {
			t.Fatalf("preview width is wrong at %d: %+v", width, preview.Bounds)
		}
		writeInteractionScreenshot(t, tt, "file-preview-"+strconv.Itoa(width))
		b := preview.Bounds
		tt.Scroll(b.X+b.W/2, b.Y+b.H/2, 0, 5000)
		tt.Advance(time.Second)
		ending, found := tt.AccessByRole(ui.RoleText, "完整结尾标记")
		if !found || ending.Bounds.Y < b.Y || ending.Bounds.Y+ending.Bounds.H > b.Y+b.H+1 {
			t.Fatalf("file ending cannot be scrolled into view: %+v in %+v", ending.Bounds, b)
		}
		if _, visible := tt.Access("目录列表"); visible {
			t.Fatal("file tree must not consume the reading width by default")
		}
		mustClick(t, tt, "查看源码")
		key := listingKey("thread:t1", "docs/guide.md")
		state := w.files.views[key]
		if state == nil || !state.SoftWrap || state.Buffer.String() != body {
			t.Fatal("source view is not complete and wrapped")
		}
		if state.LineTop(3)-state.LineTop(2) < 40 {
			t.Fatal("long source line did not actually wrap at dock width")
		}
		writeInteractionScreenshot(t, tt, "file-source-"+strconv.Itoa(width))
		mustClick(t, tt, "阅读预览")
		mustClick(t, tt, "显示目录")
		if _, visible := tt.Access("目录列表"); !visible {
			t.Fatal("file tree toggle did not restore navigation")
		}
	}
}

func TestT3MessageInteractionNativeReview(t *testing.T) {
	for _, width := range []int{1080, 1920} {
		for _, dark := range []bool{false, true} {
			w, _, _ := queueFixture()
			w.widgets.themeSet, w.widgets.dark = true, dark
			w.runs[0].CreatedAt, w.runs[0].UpdatedAt = time.Now().Add(-30*time.Second), time.Now()
			w.threads[0].CreatedAt = time.Now().Add(-2 * time.Minute)
			w.threads[0].Model = "example-provider/gpt-6-sol:high"
			w.models = []api.ModelInfo{{Provider: "example-provider", ID: "gpt-6-sol", Name: "GPT-6-Sol", Reasoning: true}}
			w.modelsLoaded = true
			w.features.thinkingModel = "example-provider/gpt-6-sol"
			w.features.thinkingLevels = []string{"low", "high", "max"}
			w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
			item := api.Attachment{ID: "review-image", Name: "screenshot.png", MIME: "image/png"}
			img := image.NewRGBA(image.Rect(0, 0, 120, 80))
			for y := 0; y < 80; y++ {
				for x := 0; x < 120; x++ {
					img.Set(x, y, color.RGBA{uint8(40 + x), uint8(80 + y), 180, 255})
				}
			}
			var encoded bytes.Buffer
			_ = png.Encode(&encoded, img)
			item.Size = int64(encoded.Len())
			rememberAttachmentPreview(item, encoded.Bytes())
			w.setDraft("t1", "请查看这张图片 ")
			w.insertPastedAttachmentsAt("t1", []api.Attachment{item}, len([]rune(w.composerFor("t1").draft)), len([]rune(w.composerFor("t1").draft)))
			w.setDraft("t1", w.composerFor("t1").draft+" 中的按钮，修改后继续当前任务。")
			tt := ui.NewTester(w.view, width, 850)
			tt.Advance(time.Second)
			if !tt.HasText("第一条排队") || !tt.HasText("第二条排队") {
				t.Fatal("inline queue missing")
			}
			if _, ok := tt.Access("追加信息"); !ok {
				t.Fatal("running action missing")
			}
			name := "light"
			if dark {
				name = "dark"
			}
			writeInteractionScreenshot(t, tt, "t3-message-"+name+"-"+strconv.Itoa(width))
		}
	}
}

func writeInteractionScreenshot(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		return
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(output, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, tt.Image()); err != nil {
		t.Fatal(err)
	}
}
