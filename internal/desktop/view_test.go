package desktop

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/muq9738/Vantry/pkg/api"
)

func newViewWorkbench(runs ...api.Run) (*workbench, *fakeService) {
	f := &fakeService{health: api.Health{Version: api.ProtocolVersion, State: "ready"}}
	w := newWorkbench(f)
	w.applySnapshot(api.Snapshot{
		Sequence: 3,
		Threads:  []api.Thread{thread("t1", "第一条线程", 0)},
		Runs:     runs,
	})
	return w, f
}

func mustClick(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	if err := tt.Click(name); err != nil {
		t.Fatalf("click %q: %v", name, err)
	}
}

func committedPendingRun(p *submission, status api.RunStatus) api.Run {
	r := run(p.runID, p.threadID, status, p.send.Message, "已提交的回复", 0)
	r.ID = p.runID
	r.CreatedAt = p.startedAt
	r.UpdatedAt = p.startedAt
	return r
}

func TestViewShowsContinuousConversationWithoutRunChrome(t *testing.T) {
	w, _ := newViewWorkbench(
		run("r1", "t1", api.RunRunning, "把标题改成蓝色", "正在处理", 0),
	)
	tt := ui.NewTester(w.view, 1080, 700)
	tt.Advance(time.Second) // streamed text reveals a word at a time

	for _, want := range []string{
		"Vantry", "把标题改成蓝色", "正在处理", "正在回复",
	} {
		if !tt.HasText(want) {
			t.Errorf("view is missing %q; texts %q", want, tt.Texts())
		}
	}
	for _, unwanted := range []string{"任务 r1", "r1-attempt", "已连接 Core", "协议 v1"} {
		if tt.HasText(unwanted) {
			t.Errorf("persistent conversation exposes diagnostic chrome %q: %q", unwanted, tt.Texts())
		}
	}
	for _, text := range tt.Texts() {
		if strings.Contains(text, "已接入真实模型") || strings.Contains(text, "模型已就绪") {
			t.Errorf("view claims a real model is connected: %q", text)
		}
	}
}

func TestViewShowsFailedRunAndItsError(t *testing.T) {
	failed := run("r9", "t1", api.RunFailed, "跑一下测试", "", time.Minute)
	failed.Error = "provider 未配置"
	w, _ := newViewWorkbench(failed)

	tt := ui.NewTester(w.view, 1080, 700)
	if !tt.HasText("回复失败") || !tt.HasText("错误：provider 未配置") {
		t.Errorf("failed run not shown: %q", tt.Texts())
	}
	if tt.HasText("正在准备回复…") {
		t.Error("a terminal run must not look like it is still streaming")
	}
}

func TestViewStreamsNewOutputIntoItsRun(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "讲个故事", "从前", 0))
	tt := ui.NewTester(w.view, 1080, 700)

	w.post(func() {
		if !w.applyEvent(deltaEvent(4, "t1", "r1", "有一个人")) {
			t.Error("delta should fold in")
		}
	})
	tt.Advance(time.Second)
	if !tt.HasText("从前有一个人") {
		t.Errorf("streamed output not shown: %q", tt.Texts())
	}
}

func TestComposerSendsTypedMessage(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "上一句", "上一句的回复", 0))
	tt := ui.NewTester(w.view, 1080, 700)

	mustClick(t, tt, "消息输入框")
	if !tt.Focused("消息输入框") {
		t.Error("the composer should take the keyboard focus on click")
	}
	tt.Type("把标题改成绿色")
	mustClick(t, tt, "发送")

	commands := f.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.SendMessage || commands[0].Message != "把标题改成绿色" {
		t.Fatalf("commands %+v", commands)
	}
	tt.Frame()
	if got := w.composerFor("t1").draft; got != "" {
		t.Errorf("submitted input should leave the composer; new draft = %q", got)
	}
}

func TestConfiguredMultilineComposerGrowsAndUsesCtrlEnter(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "上一句", "回复", 0))
	w.widgets.sendWithEnter = false // Preserve the explicitly selected multiline mode.
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	before, _ := tt.Access("消息输入框")

	tt.Type("第一行")
	tt.Key(0, ui.KeyEnter)
	tt.Type("第二行")
	tt.Key(0, ui.KeyEnter)
	tt.Type("第三行")
	if got := w.composerFor("t1").draft; got != "第一行\n第二行\n第三行" {
		t.Fatalf("multiline draft = %q", got)
	}
	if len(f.dispatched()) != 0 {
		t.Fatal("plain Enter must not send in the configured multiline mode")
	}
	after, _ := tt.Access("消息输入框")
	if after.Bounds.H <= before.Bounds.H {
		t.Errorf("TextArea did not auto-grow: before %.1f, after %.1f", before.Bounds.H, after.Bounds.H)
	}

	// ⌘+Enter is a line break, never a send: the draft gains a newline.
	tt.Key(ui.Super, ui.KeyEnter)
	if got := w.composerFor("t1").draft; got != "第一行\n第二行\n第三行\n" {
		t.Fatalf("⌘+Enter must insert a newline, draft = %q", got)
	}
	if len(f.dispatched()) != 0 {
		t.Fatal("⌘+Enter sent a message")
	}

	tt.Key(ui.Ctrl, ui.KeyEnter)
	commands := f.dispatched()
	if len(commands) != 1 || commands[0].Message != "第一行\n第二行\n第三行" {
		t.Fatalf("Ctrl+Enter should send the complete multiline payload: %+v", commands)
	}
}

func TestEnterModeSettingsUsesShiftEnterForNewline(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "上一句", "回复", 0))
	w.widgets.sendWithEnter = false
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "设置")
	mustClick(t, tt, "Enter 发送")
	if !w.widgets.sendWithEnter {
		t.Fatal("settings did not switch the send shortcut")
	}
	tt.Key(0, ui.KeyEscape)
	mustClick(t, tt, "消息输入框")
	tt.Type("第一行")
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Type("第二行")
	if len(f.dispatched()) != 0 || w.composerFor("t1").draft != "第一行\n第二行" {
		t.Fatalf("Shift+Enter must insert a newline: draft %q, commands %+v", w.composerFor("t1").draft, f.dispatched())
	}
	tt.Key(0, ui.KeyEnter)
	if commands := f.dispatched(); len(commands) != 1 || commands[0].Message != "第一行\n第二行" {
		t.Fatalf("Enter mode sent %+v", commands)
	}
}

func TestCompositionAndImmediateCommitKeyDoNotSend(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "上一句", "回复", 0))
	w.widgets.sendWithEnter = false
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")

	tt.Compose("ni", 2)
	tt.Key(ui.Ctrl, ui.KeyEnter)
	if len(f.dispatched()) != 0 {
		t.Fatal("Ctrl+Enter stole an active IME composition")
	}
	tt.Type("你")
	tt.Key(ui.Ctrl, ui.KeyEnter) // immediately after the composition commit
	if len(f.dispatched()) != 0 {
		t.Fatal("the key immediately following an IME commit must not send")
	}
	comp := w.composerFor("t1")
	comp.lastCompositionSet = time.Now().Add(-time.Second)
	tt.Key(0, ui.KeyEnter)
	if got := comp.draft; got != "你\n" {
		t.Errorf("ordinary Enter after the IME boundary should be a newline, got %q", got)
	}
	tt.Key(ui.Ctrl, ui.KeyEnter)
	if commands := f.dispatched(); len(commands) != 1 || commands[0].Message != "你" {
		t.Fatalf("an intentional later Ctrl+Enter should send: %+v", commands)
	}
}

func TestRunningAllowsNextDraftButOnlyStopIsPrimary(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunRunning, "正在执行", "已有输出", 0))
	w.widgets.sendWithEnter = false
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("下一条草稿")
	tt.Key(0, ui.KeyEnter)
	if len(f.dispatched()) != 0 || w.composerFor("t1").draft != "下一条草稿\n" {
		t.Fatalf("running input was sent or lost: %+v", f.dispatched())
	}
	if _, ok := tt.AccessByRole(ui.RoleButton, "发送"); ok {
		t.Error("send must not compete with stop as the primary action while running")
	}
	mustClick(t, tt, "停止")
	if !tt.HasText("正在停止") || !w.stoppingRuns["r1"] {
		t.Errorf("stop request should show pending feedback until Core terminates: %q", tt.Texts())
	}
	if button, ok := tt.AccessByRole(ui.RoleButton, "正在停止"); !ok || !button.Disabled {
		t.Errorf("stopping primary action = %+v, ok=%v", button, ok)
	}
	w.applyEvent(statusEvent(4, run("r1", "t1", api.RunCancelled, "正在执行", "已有输出", time.Minute)))
	tt.Advance(time.Second) // a finished reply's meta follows its last words
	if !tt.HasText("已停止 · 已保留收到的回复") || w.composerFor("t1").draft != "下一条草稿\n" {
		t.Errorf("terminal stop did not preserve content and draft: %q", tt.Texts())
	}
}

func TestEditingWhileSendIsInFlightSurvivesReceiptAndRetryBoundary(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "上一句", "回复", 0))
	held := holdWork(w)
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("第一条消息")
	mustClick(t, tt, "发送")
	tt.Frame()
	mustClick(t, tt, "消息输入框")
	tt.Type("回执后的新草稿")
	held.release()
	tt.Frame()

	commands := f.dispatched()
	if len(commands) != 1 || commands[0].Message != "第一条消息" {
		t.Fatalf("commands %+v", commands)
	}
	comp := w.composerFor("t1")
	if comp.draft != "回执后的新草稿" {
		t.Errorf("receipt replaced the new draft: %q", comp.draft)
	}
	if send, ok := tt.AccessByRole(ui.RoleButton, "发送"); !ok || !send.Disabled {
		t.Fatal("a new draft cannot overtake the unmerged original command")
	}
	w.mergeRun(committedPendingRun(comp.pending, api.RunCompleted))
	tt.Frame()
	mustClick(t, tt, "发送")
	held.release()
	commands = f.dispatched()
	if len(commands) != 2 || commands[1].Message != "回执后的新草稿" || commands[1].CommandID == commands[0].CommandID {
		t.Fatalf("the edited draft should be a distinct new command: %+v", commands)
	}
}

func TestUnconfirmedRowRetriesOriginalWithoutUsingDraft(t *testing.T) {
	f := &fakeService{receiptErr: &api.Error{Code: api.NotReady}}
	w := newWorkbench(f)
	w.applySnapshot(snapshotCompletedRun())
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("原始请求")
	mustClick(t, tt, "发送")
	mustClick(t, tt, "消息输入框")
	tt.Type("独立新草稿")
	tt.Frame()
	if !tt.HasText("发送结果待确认") {
		t.Fatalf("pending state not rendered next to the original instruction: %q", tt.Texts())
	}
	first := f.dispatched()[0]
	f.mu.Lock()
	f.receiptErr = nil
	f.mu.Unlock()
	mustClick(t, tt, "重试原请求")
	commands := f.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[1], first) || w.composerFor("t1").draft != "独立新草稿" {
		t.Fatalf("retry used edited content or duplicated the command: %+v", commands)
	}
}

func TestSidebarSelectionDraftsAndSearch(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "第一条线程里的问题", "回答", 0))
	w.applyEvent(threadEvent(4, thread("t2", "第二条线程", time.Hour)))
	w.applyEvent(statusEvent(5, run("r2", "t2", api.RunCompleted, "第二条线程里的问题", "另一段回答", 2*time.Hour)))
	tt := ui.NewTester(w.view, 1080, 700)

	mustClick(t, tt, "消息输入框")
	tt.Type("第一条线程的草稿")
	mustClick(t, tt, "第二条线程")
	tt.Frame()
	mustClick(t, tt, "消息输入框")
	tt.Type("第二条线程的草稿")
	mustClick(t, tt, "第一条线程")
	tt.Frame()
	if got := w.composerFor("t1").draft; got != "第一条线程的草稿" {
		t.Errorf("t1 draft = %q", got)
	}
	if got := w.composerFor("t2").draft; got != "第二条线程的草稿" {
		t.Errorf("t2 draft = %q", got)
	}
	if len(f_dispatched(w)) != 0 {
		t.Error("switching sessions must not send anything")
	}

	mustClick(t, tt, "搜索会话")
	tt.Type("绝不存在的会话内容")
	if !tt.HasText("没有匹配的会话") {
		t.Errorf("local search did not filter navigation: %q", tt.Texts())
	}
	tt.Key(0, ui.KeyEscape)
	if tt.HasText("没有匹配的会话") {
		t.Error("Escape should clear the native search field")
	}
}

func TestNewSessionCreatesNothingUntilFirstSend(t *testing.T) {
	w, f := newViewWorkbench()
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "新会话")
	if len(f.dispatched()) != 0 || !isLocalSession(w.selected) {
		t.Fatalf("new session should be a local draft: selected %q, commands %+v", w.selected, f.dispatched())
	}
	mustClick(t, tt, "消息输入框")
	tt.Type("第一条真实指令")
	mustClick(t, tt, "发送")
	commands := f.dispatched()
	if len(commands) != 2 || commands[0].Kind != api.CreateThread || commands[1].Kind != api.SendMessage {
		t.Fatalf("first send commands = %+v", commands)
	}
	if !tt.HasText("第一条真实指令") {
		t.Errorf("derived session title/instruction missing: %q", tt.Texts())
	}
}

func TestNarrowLayoutKeepsComposerReachable(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "把标题改成蓝色", "已处理", 0))
	tt := ui.NewTester(w.view, 640, 420)
	for _, name := range []string{"会话导航", "消息输入框", "发送"} {
		access, ok := tt.Access(name)
		if !ok {
			t.Fatalf("%q missing from narrow layout: %q", name, tt.Texts())
		}
		if access.Bounds.X < 0 || access.Bounds.Y < 0 || access.Bounds.X+access.Bounds.W > 640 || access.Bounds.Y+access.Bounds.H > 420 {
			t.Errorf("%q is outside the 640x420 window: %+v", name, access.Bounds)
		}
	}
	mustClick(t, tt, "消息输入框")
	tt.Type("窄窗口里也能发送")
	mustClick(t, tt, "发送")
	if got := f.dispatched(); len(got) != 1 || got[0].Message != "窄窗口里也能发送" {
		t.Errorf("commands %+v", got)
	}
	mustClick(t, tt, "会话导航")
	mustClick(t, tt, "新会话")
	if !isLocalSession(w.selected) {
		t.Error("new-session entry should remain usable with the navigation open in a narrow window")
	}
}

func TestRecentNavigationOrderStaysStableDuringPartialCreates(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{
		thread("t-old", "较早会话", 0),
		thread("t-new", "较新会话", time.Minute),
	}})
	first := w.newSession()
	w.setDraft(first, "第一份本地草稿")
	second := w.newSession()
	w.setDraft(second, "第二份本地草稿")
	base := time.Unix(100, 0)
	w.composerFor("pending-older").pending = &submission{startedAt: base}
	w.composerFor("pending-newer").pending = &submission{startedAt: base.Add(time.Second)}
	want := strings.Join([]string{second, first, "pending-newer", "pending-older", "t-new", "t-old"}, ",")
	for i := 0; i < 10; i++ {
		if got := strings.Join(w.navigationSessions(), ","); got != want {
			t.Fatalf("recent navigation moved between frames: got %q want %q", got, want)
		}
	}
}

func TestRepliesOfDifferentLengthsShareReadingColumn(t *testing.T) {
	shortText := "简短的第一条回复。"
	longText := strings.Repeat("第二条回复包含更长的说明，用于检查不同内容长度时的阅读对齐。", 4)
	w, _ := newViewWorkbench(
		run("r-short", "t1", api.RunCompleted, "第一条问题", shortText, 0),
		run("r-long", "t1", api.RunCompleted, "第二条问题", longText, time.Minute),
	)
	tt := ui.NewTester(w.view, 1440, 900)
	short, shortOK := tt.AccessByRole(ui.RoleText, shortText)
	long, longOK := tt.AccessByRole(ui.RoleText, longText)
	if !shortOK || !longOK {
		t.Fatalf("both replies must be visible: short=%v long=%v", shortOK, longOK)
	}
	if delta := short.Bounds.X - long.Bounds.X; delta < -0.5 || delta > 0.5 {
		t.Fatalf("reply width moved the reading column: short=%+v long=%+v", short.Bounds, long.Bounds)
	}
}

func TestStandardDesktopLayouts(t *testing.T) {
	for _, size := range []struct{ width, height int }{{1440, 900}, {1080, 700}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "桌面布局", "回复", 0))
			tt := ui.NewTester(w.view, size.width, size.height)
			field, ok := tt.Access("消息输入框")
			if !ok || field.Bounds.W < 300 || field.Bounds.H < 40 {
				t.Errorf("composer bounds at %dx%d = %+v", size.width, size.height, field.Bounds)
			}
			if _, ok := tt.AccessByRole(ui.RoleLog, "会话内容"); !ok {
				t.Error("conversation is missing")
			}
			if !tt.HasText("新会话") || !tt.HasText("搜索会话") {
				t.Errorf("desktop navigation missing at %dx%d: %q", size.width, size.height, tt.Texts())
			}
		})
	}
}

func TestViewShowsContextualResyncFeedback(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "在跑", "", 0))
	w.post(func() { w.noteResync("事件流已关闭") })
	tt := ui.NewTester(w.view, 1080, 700)
	if !tt.HasText("连接已中断，正在恢复；草稿和已收到的回复会保留。") {
		t.Errorf("resync feedback missing: %q", tt.Texts())
	}
	if tt.HasText("游标") || tt.HasText("socket") {
		t.Errorf("diagnostics leaked into the persistent banner: %q", tt.Texts())
	}
}

func TestViewIsReadableByAssistiveTechnology(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "把标题改成蓝色", "处理中", 0))
	tt := ui.NewTester(w.view, 1080, 700)
	log, ok := tt.AccessByRole(ui.RoleLog, "会话内容")
	if !ok {
		t.Fatalf("conversation is not a named log: %q", tt.Texts())
	}
	if log.Bounds.W < 100 || log.Bounds.H < 100 {
		t.Errorf("log bounds %+v", log.Bounds)
	}
	row, ok := tt.AccessByRole(ui.RoleListItem, "把标题改成蓝色")
	if !ok || row.Bounds.H <= 0 {
		t.Errorf("instruction row = %+v, ok=%v", row, ok)
	}
	if _, ok := tt.AccessByRole(ui.RoleHeading, "第一条线程"); !ok {
		t.Error("persisted session title should be a heading")
	}
	field, _ := tt.Access("消息输入框")
	if field.Description == "" || field.Placeholder == "" {
		t.Errorf("composer = %+v, want a placeholder and description", field)
	}
	for _, name := range []string{"停止", "新会话", "设置", "用量", "切换深色"} {
		if control, ok := tt.AccessByRole(ui.RoleButton, name); !ok || control.Name == "" {
			t.Errorf("button %q lacks an accessible name: %+v", name, control)
		}
	}
}

func TestEscapeDoesNotStopActiveRun(t *testing.T) {
	w, f := newViewWorkbench(run("r1", "t1", api.RunRunning, "继续运行", "输出", 0))
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Key(0, ui.KeyEscape)
	if len(f.dispatched()) != 0 {
		t.Fatal("Escape must not issue a stop command")
	}
	if _, active := w.activeRun("t1"); !active {
		t.Fatal("Escape changed Core's active Run")
	}
}

func TestStreamingListKeepsReadingAnchorAndJumpsToLatest(t *testing.T) {
	runs := make([]api.Run, 36)
	for i := range runs {
		runs[i] = run(fmt.Sprintf("r%d", i), "t1", api.RunCompleted,
			fmt.Sprintf("指令 %02d", i), strings.Repeat(fmt.Sprintf("回复 %02d。\n", i), 5), time.Duration(i)*time.Minute)
	}
	w, _ := newViewWorkbench(runs...)
	tt := ui.NewTester(w.view, 1080, 700)
	state := w.logState("t1")
	if !state.AtEnd() {
		t.Fatal("a conversation initially follows its latest message")
	}
	log, _ := tt.AccessByRole(ui.RoleLog, "会话内容")
	x, y := log.Bounds.X+100, log.Bounds.Y+100
	tt.Move(x, y)
	tt.Scroll(x, y, 0, -600)
	if state.AtEnd() || !tt.HasText("跳到最新") {
		t.Fatalf("manual upward scrolling should pause following and show jump: %q", tt.Texts())
	}
	first, _ := state.Visible()

	updated := append([]api.Run(nil), runs...)
	updated[35].Output += "新到的流式内容。"
	w.applySnapshot(api.Snapshot{Sequence: 4, Threads: []api.Thread{thread("t1", "第一条线程", 0)}, Runs: updated})
	tt.Frame()
	after, _ := state.Visible()
	if after != first || state.AtEnd() || !tt.HasText("跳到最新") {
		t.Errorf("snapshot stole the reading anchor: first %d, after %d, atEnd %v", first, after, state.AtEnd())
	}
	mustClick(t, tt, "跳到最新")
	if !state.AtEnd() || tt.HasText("跳到最新") {
		t.Error("jump should restore latest-content following")
	}
}

func TestDarkAppearanceAndExplicitToggle(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "暗色适配", "回复", 0))
	tt := ui.NewTesterWith(w.view, 1080, 700, ui.TesterOptions{Dark: true})
	if !w.widgets.dark {
		t.Fatal("system dark appearance was not adopted")
	}
	mustClick(t, tt, "切换浅色")
	if w.widgets.dark {
		t.Error("explicit light appearance was not applied")
	}
	tt.SetDark(true)
	if w.widgets.dark {
		t.Error("an explicit in-window choice should not be overwritten while the window is open")
	}
}

func TestCountersStayOutOfProductUI(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	if got := w.countersText(false); strings.Contains(got, "重复") || strings.Contains(got, "待重同步") {
		t.Errorf("counters = %q, want zero counters left out", got)
	}
	w.applyEvent(deltaEvent(2, "t1", "r1", "旧"))
	w.applyEvent(api.Event{Sequence: 9, ID: "ev-9", Type: "unknown.thing", ThreadID: "t1", Data: mustJSON(map[string]any{"x": 1})})
	if got := w.countersText(true); !strings.Contains(got, "重复 1") || !strings.Contains(got, "待重同步 1") {
		t.Errorf("narrow counters = %q", got)
	}
	tt := ui.NewTester(w.view, 1080, 700)
	if tt.HasText(w.countersText(false)) {
		t.Errorf("internal cursor counters occupy the product UI: %q", tt.Texts())
	}
}

func f_dispatched(w *workbench) []api.Command {
	f, ok := w.service.(*fakeService)
	if !ok {
		return nil
	}
	return f.dispatched()
}
