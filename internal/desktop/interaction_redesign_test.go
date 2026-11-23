package desktop

import (
	"errors"
	"reflect"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestHomeKeepsWelcomeAndComposerTogetherWithoutEmptyHistory(t *testing.T) {
	w, _ := newViewWorkbench()
	w.threads = append(w.threads, thread("unused", "线程 7", 0))
	tt := ui.NewTester(w.view, 1280, 820)
	heading, ok := tt.Access("今天想构建什么？")
	if !ok {
		t.Fatal("home heading missing")
	}
	field, ok := tt.Access("消息输入框")
	if !ok {
		t.Fatal("home composer missing")
	}
	gap := field.Bounds.Y - (heading.Bounds.Y + heading.Bounds.H)
	if gap < 20 || gap > 100 {
		t.Fatalf("welcome and input separated: gap=%v", gap)
	}
	if field.Bounds.W < 500 || field.Bounds.W > 760 {
		t.Fatalf("home composer width=%v", field.Bounds.W)
	}
	if tt.HasText("线程 7") || tt.HasText("本机工作台") || tt.HasText("草稿按会话保留") {
		t.Fatalf("empty screen contains prototype chrome: %v", tt.Texts())
	}
}

func TestModelPickerUpdatesCurrentThreadOnlyAfterCommittedEvent(t *testing.T) {
	service := newFakeQueryService()
	service.models = []api.ModelInfo{{ID: "test/new", Name: "New model", Provider: "test"}}
	w := newWorkbench(service)
	snapshot := snapshotOneRun()
	snapshot.Threads[0].Model = "test/old"
	snapshot.Runs[0].Model = "test/old"
	w.applySnapshot(snapshot)
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "选择模型")
	mustClick(t, tt, "New model")
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.SetThreadModel || commands[0].ThreadID != "t1" || commands[0].Model != "test/new" {
		t.Fatalf("model choice command=%+v", commands)
	}
	if w.currentModel() != "test/old" {
		t.Fatal("receipt invented a committed model projection")
	}
	changed := snapshot.Threads[0]
	changed.Model = "test/new"
	event := threadEvent(4, changed)
	event.Type = "thread.updated"
	if !w.applyEvent(event) {
		t.Fatal("thread.updated not folded")
	}
	tt.Frame()
	if w.currentModel() != "test/new" || w.runs[0].Model != "test/old" {
		t.Fatalf("selection rewrote active execution: thread=%q run=%q", w.currentModel(), w.runs[0].Model)
	}
}

func TestUnknownControlCanBeRetriedFromUIAfterRunSettles(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunRunning, "current", "output", 0))
	service.receiptErr = errors.New("receipt lost")
	w.setDraft("t1", "original queued instruction")
	w.queueDraft("t1", false)
	original := service.dispatched()[0]
	w.setDraft("t1", "new independent draft")
	w.applyEvent(statusEvent(4, run("r1", "t1", api.RunCompleted, "current", "output", 0)))
	service.receiptErr = nil
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "重试排队")
	commands := service.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[1], original) || w.composerFor("t1").draft != "new independent draft" {
		t.Fatalf("retry lost its immutable intent or new draft: %+v", commands)
	}
}

func TestProjectSelectionPreservesUnsentTextAndWaitsToCreate(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "old", "result", 0))
	w.setDraft("t1", "my next request")
	w.configureWorkspace("/tmp/another-project")
	if !isLocalSession(w.selected) || w.composerFor(w.selected).draft != "my next request" || w.defaults.Workspace.Path != "/tmp/another-project" {
		t.Fatal("new project lost its draft or directory")
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("selecting a project prematurely created a task")
	}
	if w.composerFor("t1").draft != "my next request" {
		t.Fatal("previous session draft was removed")
	}
}

func TestApprovalDoesNotPretendTheToolIsExecuting(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "run tests", "", 0))
	w.tools = []api.ToolCall{{ID: "tool", ThreadID: "t1", RunID: "r1", Name: "bash", Status: "running"}}
	w.requests = []api.ProviderRequest{{ID: "request", ThreadID: "t1", RunID: "r1", Method: "confirm", Title: "允许执行 bash？", Status: "pending"}}
	tt := ui.NewTester(w.view, 1080, 700)
	if !tt.HasText("需要你的确认") || !tt.HasText("等待确认") || tt.HasText("正在回复") {
		t.Fatalf("approval state contradicts execution state: %v", tt.Texts())
	}
}

func TestUnsentDemoDraftDoesNotAdvertiseARealModel(t *testing.T) {
	w, _ := newViewWorkbench()
	w.defaults.ProviderID = "controlled"
	w.defaults.Model = "provider/model"
	w.newSession()
	tt := ui.NewTester(w.view, 1080, 700)
	if !tt.HasText("本地演示") || tt.HasText("provider/model") {
		t.Fatalf("demo misrepresented model use: %v", tt.Texts())
	}
	if button, ok := tt.Access("选择模型"); !ok || !button.Disabled {
		t.Fatal("demo offered unsupported model switching")
	}
}
