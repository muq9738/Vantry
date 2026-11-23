package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestFirstFrameCannotOverrideTheIncomingConversation(t *testing.T) {
	w := newWorkbench(&fakeService{})
	tt := ui.NewTester(w.view, 1280, 820)
	if w.selected != "" || len(w.localOrder) != 0 {
		t.Fatalf("drawing a loading frame created a local session: %q %v", w.selected, w.localOrder)
	}
	if input, ok := tt.Access("消息输入框"); !ok || !input.Disabled {
		t.Fatal("input enabled before its session is known")
	}
	w.applySnapshot(snapshotOneRun())
	tt.Frame()
	if w.selected != "t1" || !tt.HasText("把标题改成蓝色") {
		t.Fatalf("loaded conversation displaced: %q %v", w.selected, tt.Texts())
	}
	if len(w.localOrder) != 0 {
		t.Fatal("rendering the loaded conversation created an extra draft")
	}
}

func TestEmptyInitialSnapshotOpensOneLocalDraft(t *testing.T) {
	service := &fakeService{}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{})
	selected := w.selected
	if !isLocalSession(selected) {
		t.Fatal("empty workspace has no composer session")
	}
	tt := ui.NewTester(w.view, 1280, 820)
	for i := 0; i < 4; i++ {
		tt.Frame()
		w.applySnapshot(api.Snapshot{})
	}
	if w.selected != selected || len(w.localOrder) != 1 || len(service.dispatched()) != 0 {
		t.Fatal("empty frames created extra conversations")
	}
}

func TestChoosingTheCurrentDirectoryDoesNotSwitchConversation(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "question", "answer", 0))
	w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/tmp/project"}
	w.setDraft("t1", "keep typing here")
	w.configureWorkspace("/tmp/project/../project")
	if w.selected != "t1" || len(w.localOrder) != 0 || w.composerFor("t1").draft != "keep typing here" {
		t.Fatal("same project opened an unrelated draft")
	}
}

func TestFirstEmptySnapshotQueriesModelsWithoutCreatingOrInspectingAThread(t *testing.T) {
	service := newFakeQueryService()
	service.models = []api.ModelInfo{
		{ID: "example-provider/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Provider: "example-provider"},
		{ID: "kimi-for-coding", Name: "Kimi For Coding", Provider: "kimi"},
	}
	w := newWorkbench(service)
	w.defaults.Model = "explicit/existing"
	w.applySnapshot(api.Snapshot{})
	selected := w.selected
	if !isLocalSession(selected) || !w.widgets.modelPickerOpen {
		t.Fatalf("first-run selection = %q, picker=%v", selected, w.widgets.modelPickerOpen)
	}
	if service.modelQueryCount() != 1 || len(service.dispatched()) != 0 {
		t.Fatalf("startup queries=%d commands=%+v", service.modelQueryCount(), service.dispatched())
	}
	inspect, open := service.workCalls()
	if len(inspect) != 0 || len(open) != 0 {
		t.Fatalf("startup touched a draft workspace: inspect=%v terminal=%v", inspect, open)
	}
	if w.defaults.Model != "explicit/existing" {
		t.Fatal("catalog silently replaced the explicit selection")
	}
	tt := ui.NewTester(w.view, 1440, 900)
	if !tt.HasText("今天想构建什么？") || !tt.HasText("DeepSeek V4.1 Flash") || !tt.HasText("kimi/kimi-for-coding") {
		t.Fatalf("real catalog is not visible in the welcome picker: %q", tt.Texts())
	}
	mustClick(t, tt, "Kimi For Coding")
	if w.defaults.Model != "kimi/kimi-for-coding" || w.selected != selected || w.widgets.modelPickerOpen {
		t.Fatalf("model choice = %q session=%q open=%v", w.defaults.Model, w.selected, w.widgets.modelPickerOpen)
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("choosing a first-run model created a Core thread")
	}
	inspect, open = service.workCalls()
	if len(inspect) != 0 || len(open) != 0 {
		t.Fatal("choosing a model inspected or opened a local-draft workspace")
	}
}

func TestFirstRunModelCatalogExplainsOldServiceAndCanReload(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.applySnapshot(api.Snapshot{})
	tt := ui.NewTester(w.view, 640, 600)
	if !tt.HasText("本地服务版本与界面不兼容：缺少模型目录查询。") || tt.HasText("会话不存在") {
		t.Fatalf("old-service feedback is misleading: %q", tt.Texts())
	}
	mustClick(t, tt, "重新载入")
	if !w.widgets.modelPickerOpen || len(w.localOrder) != 1 {
		t.Fatal("reload discarded the welcome picker or created another draft")
	}
}

func TestFirstRunModelQueryErrorKeepsExplicitChoiceAndReload(t *testing.T) {
	service := newFakeQueryService()
	service.modelErr = &api.Error{Code: api.VersionMismatch, Message: "old core"}
	w := newWorkbench(service)
	w.defaults.Model = "chosen/model"
	w.applySnapshot(api.Snapshot{})
	tt := ui.NewTester(w.view, 1080, 700)
	if !tt.HasText("模型目录载入失败：本地服务版本与界面不兼容") || tt.HasText("找不到对应会话") {
		t.Fatalf("query error was misreported: %q", tt.Texts())
	}
	service.modelErr = nil
	service.models = []api.ModelInfo{{ID: "actual", Name: "Actual model", Provider: "test"}}
	mustClick(t, tt, "重新载入")
	if service.modelQueryCount() != 2 || w.defaults.Model != "chosen/model" || !tt.HasText("Actual model") {
		t.Fatalf("reload queries=%d model=%q texts=%q", service.modelQueryCount(), w.defaults.Model, tt.Texts())
	}
}

func TestModelReceiptDoesNotCloseAnotherConversationsPicker(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "question", "answer", 0))
	w.applyEvent(threadEvent(4, thread("t2", "other", 0)))
	held := holdWork(w)
	w.chooseModel("provider/new-model")
	w.selectThread("t2")
	w.widgets.modelPickerOpen = true
	held.release()
	if w.selected != "t2" || !w.widgets.modelPickerOpen {
		t.Fatal("late model receipt changed the current conversation's interaction")
	}
}
