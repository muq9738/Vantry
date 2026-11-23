package desktop

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func assertControlInWindow(t *testing.T, tt *ui.Tester, name string, width, height int) {
	t.Helper()
	control, ok := tt.Access(name)
	r := control.Bounds
	if !ok || r.W < 12 || r.H < 12 || r.X < 0 || r.Y < 0 || r.X+r.W > float32(width)+0.5 || r.Y+r.H > float32(height)+0.5 {
		t.Errorf("%q outside %dx%d or unreachable: found=%v bounds=%+v", name, width, height, ok, r)
	}
}

func TestWorkbenchPanelsKeepComposerAndPrimaryActionVisible(t *testing.T) {
	for _, size := range [][2]int{{1440, 900}, {1080, 700}, {640, 600}, {640, 420}} {
		for _, panel := range []string{"models", "session", "git", "plugins", "remote", "terminal"} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], panel), func(t *testing.T) {
				w := nativeReviewWorkbench()
				for i := 0; i < 6; i++ {
					w.messages = append(w.messages, api.QueuedMessage{ID: fmt.Sprintf("queued-%d", i), ThreadID: "t1", Status: "pending", Message: "queued message"})
				}
				for i := 0; i < 7; i++ {
					w.composerFor("t1").attachments = append(w.composerFor("t1").attachments, api.Attachment{ID: fmt.Sprint(i), Name: fmt.Sprintf("file-%d.txt", i)})
				}
				w.setDraft("t1", strings.Repeat("多行草稿\n", 8))
				if panel == "models" {
					w.widgets.modelPickerOpen = true
				} else {
					w.features.panel = panel
				}
				tt := ui.NewTester(w.view, size[0], size[1])
				for _, name := range []string{"消息输入框", "添加附件", "工具权限", "选择模型", "运行中发送方式", "追加信息", "停止"} {
					assertControlInWindow(t, tt, name, size[0], size[1])
				}
				if panel != "models" {
					assertControlInWindow(t, tt, "关闭工具面板", size[0], size[1])
				}
			})
		}
	}
}

func TestSidebarSearchAndKeyboardKeepFocus(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "第一条线程", "回答", 0))
	w.mergeThread(thread("t2", "第二条线程", time.Minute))
	w.mergeRun(run("r2", "t2", api.RunCompleted, "第二条线程", "回答", time.Minute))
	w.mergeThread(thread("thread-c", "第三条线程", time.Hour))
	w.mergeRun(run("r3", "thread-c", api.RunCompleted, "第三条线程", "回答", time.Hour))
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "第一条线程")
	tt.Key(0, ui.KeyHome)
	if w.selected != "thread-c" {
		t.Fatalf("Home should choose most recent thread, got %s", w.selected)
	}
	tt.Key(0, ui.KeyDown)
	if w.selected != "t2" {
		t.Fatalf("second arrow lost sidebar focus: %s", w.selected)
	}
	mustClick(t, tt, "搜索会话")
	tt.Type("第三条线程")
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "第三条线程"); !ok {
		t.Fatalf("search hid the matching row: %q", tt.Texts())
	}
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "第二条线程"); ok {
		t.Fatal("search kept a row it does not match")
	}
	tt.Key(0, ui.KeyEscape)
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "第二条线程"); !ok {
		t.Fatal("clearing the search did not restore the list")
	}
}

func TestModelPickerSelectionAndEscapeReturnFocus(t *testing.T) {
	w := nativeReviewWorkbench()
	w.runs[0].Status = api.RunCompleted
	w.features.catalogPath = w.modelWorkspace()
	w.acceptThinking(w.defaults.Model, []string{"low", "high"})
	tt := ui.NewTester(w.view, 640, 600)
	mustClick(t, tt, "选择模型")
	if !tt.Focused("搜索模型") {
		t.Fatal("model search did not receive focus")
	}
	selected, ok := tt.AccessByRole(ui.RoleOption, "fixture/long-model-name-for-layout")
	if !ok || !selected.Selected {
		t.Fatalf("the selected model is not exposed to accessibility: found=%v %+v", ok, selected)
	}
	tt.Key(0, ui.KeyEscape)
	if w.widgets.modelPickerOpen || !tt.Focused("选择模型") {
		t.Fatal("Escape should close the picker and restore its trigger focus")
	}
}

func TestRenamingAndPanelFeedbackDoNotFollowSessionSelection(t *testing.T) {
	w, _ := newViewWorkbench()
	w.mergeThread(thread("t2", "Second", time.Minute))
	w.mergeRun(run("r2", "t2", api.RunCompleted, "Second", "answer", time.Minute))
	w.features.panel = "session"
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "重命名")
	if !w.features.showRename {
		t.Fatal("rename did not open")
	}
	mustClick(t, tt, "Second")
	if w.features.showRename {
		t.Fatal("rename editor followed another thread")
	}
	if _, ok := tt.Access("会话标题"); ok {
		t.Fatal("stale rename input remained mounted")
	}
	mustClick(t, tt, "关闭工具面板")
	if !tt.Focused("消息输入框") {
		t.Fatal("closing panel did not restore focus")
	}
}

func TestShortModelPickerKeepsCatalogAndComposerReachable(t *testing.T) {
	w := nativeReviewWorkbench()
	w.runs[0].Status = api.RunCompleted
	tt := ui.NewTester(w.view, 640, 420)
	mustClick(t, tt, "选择模型")
	assertControlInWindow(t, tt, "重新载入", 640, 420)
	mustClick(t, tt, "fixture/fast")
	commands := f_dispatched(w)
	if len(commands) != 1 || commands[0].Kind != api.SetThreadModel || commands[0].ThreadID != "t1" || commands[0].Model != "fixture/fast" {
		t.Fatalf("model applied from short picker: %+v", commands)
	}
	assertControlInWindow(t, tt, "消息输入框", 640, 420)
}

func TestModelPickerFitsChildThreadHeader(t *testing.T) {
	w := nativeReviewWorkbench()
	w.threads[1].Title = strings.Repeat("很长的子任务名称", 10)
	w.selectThread("child")
	tt := ui.NewTester(w.view, 640, 420)
	for _, name := range []string{"会话导航", "返回父任务", "工具权限", "选择模型", "工作区"} {
		assertControlInWindow(t, tt, name, 640, 420)
	}
}

func TestRemoveAttachmentUsesStableIdentityAndKeepsDraft(t *testing.T) {
	w, _ := newViewWorkbench()
	w.setDraft("t1", "keep draft")
	comp := w.composerFor("t1")
	comp.attachments = []api.Attachment{{ID: "a", Name: "a.txt"}, {ID: "b", Name: "b.txt"}, {ID: "c", Name: "c.txt"}}
	tt := ui.NewTester(w.view, 640, 600)
	mustClick(t, tt, "移除附件 a.txt")
	mustClick(t, tt, "移除附件 b.txt")
	if len(comp.attachments) != 1 || comp.attachments[0].ID != "c" || comp.draft != "keep draft" {
		t.Fatalf("attachment removal damaged state: %+v", comp)
	}
}

func TestIMECancellationAndThreadSwitchCannotArmAnAccidentalSend(t *testing.T) {
	for _, enterMode := range []bool{false, true} {
		t.Run(fmt.Sprint(enterMode), func(t *testing.T) {
			w, f := newViewWorkbench()
			w.widgets.sendWithEnter = enterMode
			w.mergeThread(thread("t2", "Second", time.Minute))
			w.mergeRun(run("r2", "t2", api.RunCompleted, "Second", "answer", time.Minute))
			tt := ui.NewTester(w.view, 1080, 700)
			mustClick(t, tt, "消息输入框")
			tt.Type("保留的草稿")
			tt.Compose("ni", 2)
			tt.Compose("", 0) // native preedit cancelled without any committed text
			mods := ui.Ctrl
			if enterMode {
				mods = 0
			}
			tt.Key(mods, ui.KeyEnter)
			if len(f.dispatched()) != 0 {
				t.Fatal("the cancellation boundary sent a message")
			}
			comp := w.composerFor("t1")
			comp.lastCompositionSet = time.Now().Add(-time.Second)
			tt.Key(mods, ui.KeyEnter)
			if commands := f.dispatched(); len(commands) != 1 || commands[0].Message != "保留的草稿" {
				t.Fatalf("intentional send after cancellation failed: %+v", commands)
			}
			tt.Compose("zhong", 5)
			mustClick(t, tt, "Second")
			if comp.composing || comp.compositionEnding {
				t.Fatal("leaving a session retained a stale composition")
			}
			mustClick(t, tt, "消息输入框")
			tt.Type("第二份草稿")
			tt.Key(mods, ui.KeyEnter)
			if len(f.dispatched()) != 2 || f.dispatched()[1].ThreadID != "t2" {
				t.Fatal("old composition blocked another session")
			}
		})
	}
}

func TestThinkingSelectorUsesSupportedLevelsAndKeepsModelKey(t *testing.T) {
	w := nativeReviewWorkbench()
	base := w.defaults.Model
	w.acceptThinking(base, []string{"low", "high"})
	tt := ui.NewTester(w.view, 640, 600)
	// The effort button stands beside the model: no model search to open.
	mustClick(t, tt, "思考强度")
	if w.widgets.modelPickerOpen {
		t.Fatal("thinking level must not open the model search")
	}
	mustClick(t, tt, "高")
	commands := f_dispatched(w)
	if len(commands) != 1 || commands[0].Model != base+":high" {
		t.Fatalf("thinking selection = %+v", commands)
	}
	w.threads[0].Model = base + ":high"
	tt.Frame()
	if info, _ := tt.Access("思考强度"); !strings.Contains(info.Description, "高") {
		t.Fatalf("thinking button does not show the level: %+v", info)
	}
	// The popover stays open after a pick, so the slider can animate.
	if !w.widgets.thinkingOpen {
		t.Fatal("picking an effort closed the slider before it could move")
	}
	mustClick(t, tt, "低")
	commands = f_dispatched(w)
	if len(commands) != 2 || commands[1].Model != base+":low" {
		t.Fatalf("stacked thinking suffix: %+v", commands)
	}
}

func TestNativeTerminalKeepsItsKeysAndCanCloseWithoutRemount(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	term, err := terminal.New(terminal.Options{Conn: client, NoBlink: true})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := newViewWorkbench()
	w.features.terminals = map[string]*terminal.Terminal{"t1": term}
	t.Cleanup(w.closeTerminals)
	w.features.panel = "terminal"
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "本机终端输入")
	keys := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		n, _ := server.Read(buf)
		keys <- string(buf[:n])
	}()
	tt.Key(0, ui.KeyEscape)
	received := <-keys
	if w.features.panel != "terminal" || received != "\x1b" {
		t.Fatalf("terminal Escape: panel=%q bytes=%q focused=%v", w.features.panel, received, tt.Focused("本机终端输入"))
	}
	mustClick(t, tt, "关闭此终端")
	if w.features.terminals["t1"] != nil || !tt.Focused("消息输入框") {
		t.Fatal("closed terminal remained mounted or stole focus")
	}
}

func TestReducedMotionSettlesPanelWithoutGeometryAnimation(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "question", "answer", 0))
	tt := ui.NewTesterWith(w.view, 1080, 700, ui.TesterOptions{Preferences: ui.Preferences{ReduceMotion: true}})
	mustClick(t, tt, "会话")
	immediate, _ := tt.Access("关闭工具面板")
	tt.Advance(200 * time.Millisecond)
	settled, _ := tt.Access("关闭工具面板")
	if immediate.Bounds != settled.Bounds {
		t.Fatalf("reduced-motion panel moved: %+v -> %+v", immediate.Bounds, settled.Bounds)
	}
	mustClick(t, tt, "关闭工具面板")
	if _, ok := tt.Access("关闭工具面板"); ok {
		t.Fatal("reduced-motion exit remained mounted")
	}
}
