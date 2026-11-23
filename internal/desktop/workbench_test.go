package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type fakeQueryService struct {
	*fakeService
	mu          sync.Mutex
	models      []api.ModelInfo
	modelErr    error
	modelCalls  int
	changes     map[string]api.WorkspaceChanges
	inspectErr  error
	terminalErr error
	inspected   []string
	opened      []string
}

func newFakeQueryService() *fakeQueryService {
	return &fakeQueryService{fakeService: &fakeService{}, changes: map[string]api.WorkspaceChanges{}}
}

func (f *fakeQueryService) ListModels(context.Context) ([]api.ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelCalls++
	return append([]api.ModelInfo(nil), f.models...), f.modelErr
}

func (f *fakeQueryService) InspectWorkspace(_ context.Context, threadID string) (api.WorkspaceChanges, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected = append(f.inspected, threadID)
	return f.changes[threadID], f.inspectErr
}

func (f *fakeQueryService) OpenTerminal(_ context.Context, threadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, threadID)
	return f.terminalErr
}

func (f *fakeQueryService) workCalls() (inspect, open []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inspected...), append([]string(nil), f.opened...)
}

func (f *fakeQueryService) modelQueryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelCalls
}

func workbenchEvent(sequence int64, kind string, value any) api.Event {
	payload := map[string]any{}
	switch v := value.(type) {
	case api.Input:
		payload["input"] = v
		return api.Event{Sequence: sequence, Type: kind, ThreadID: v.ThreadID, RunID: v.RunID, AttemptID: v.AttemptID, Data: mustJSON(payload)}
	case api.ToolCall:
		payload["tool"] = v
		return api.Event{Sequence: sequence, Type: kind, ThreadID: v.ThreadID, RunID: v.RunID, AttemptID: v.AttemptID, Data: mustJSON(payload)}
	case api.ProviderRequest:
		payload["request"] = v
		return api.Event{Sequence: sequence, Type: kind, ThreadID: v.ThreadID, RunID: v.RunID, AttemptID: v.AttemptID, Data: mustJSON(payload)}
	default:
		panic("unsupported workbench event")
	}
}

func TestWorkbenchSnapshotAndIncrementalEventsRestoreControls(t *testing.T) {
	input := api.Input{ID: "in-1", ThreadID: "t1", Kind: "queue", Message: "下一条", Status: "queued", CreatedAt: baseTime}
	tool := api.ToolCall{ID: "tool-1", ThreadID: "t1", RunID: "r1", AttemptID: "r1-attempt", Name: "bash", Arguments: json.RawMessage(`{"command":"go test"}`), Status: "running"}
	request := api.ProviderRequest{ID: "request-1", ProviderRequestID: "pi-1", ThreadID: "t1", RunID: "r1", AttemptID: "r1-attempt", Method: "confirm", Title: "允许执行？", Status: "pending"}
	w := newWorkbench(&fakeService{})
	snap := snapshotOneRun()
	snap.Inputs = []api.Input{input}
	snap.Tools = []api.ToolCall{tool}
	snap.Requests = []api.ProviderRequest{request}
	w.applySnapshot(snap)
	if len(w.inputs) != 1 || len(w.tools) != 1 || len(w.requests) != 1 {
		t.Fatalf("snapshot projections = %d/%d/%d", len(w.inputs), len(w.tools), len(w.requests))
	}

	input.Status = "started"
	tool.Status, tool.Output = "completed", "ok"
	request.Status = "responding"
	for i, event := range []api.Event{
		workbenchEvent(4, eventInputUpdated, input),
		workbenchEvent(5, eventToolUpdated, tool),
		workbenchEvent(6, eventRequestUpdated, request),
	} {
		if !w.applyEvent(event) {
			t.Fatalf("event %d did not fold", i)
		}
	}
	if w.inputs[0].Status != "started" || w.tools[0].Output != "ok" || w.requests[0].Status != "responding" {
		t.Fatalf("incremental projections = %+v %+v %+v", w.inputs[0], w.tools[0], w.requests[0])
	}
	if w.sequence != 6 || w.unknownEvents != 0 {
		t.Fatalf("sequence=%d unknown=%d", w.sequence, w.unknownEvents)
	}
}

func TestQueueSteerAndCancelUseExactCommittedIdentity(t *testing.T) {
	service := &fakeService{}
	w := newWorkbench(service)
	snap := snapshotOneRun()
	snap.Threads[0].ProviderID = "pi"
	w.applySnapshot(snap)

	w.setDraft("t1", "  持久排队下一条  ")
	if !w.queueDraft("t1", false) {
		t.Fatal("queue was not dispatched")
	}
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Kind != api.QueueMessage || commands[0].ThreadID != "t1" || commands[0].Message != "持久排队下一条" || commands[0].RunID != "" {
		t.Fatalf("queue command = %+v", commands)
	}
	if strings.Contains(w.actionNotice.Text, "已接收") || strings.Contains(w.actionNotice.Text, "delivered") {
		t.Fatalf("receipt claimed delivery: %q", w.actionNotice.Text)
	}

	w.setDraft("t1", "纠正当前方向")
	if !w.queueDraft("t1", true) {
		t.Fatal("steer was not dispatched")
	}
	commands = service.dispatched()
	steer := commands[1]
	if steer.Kind != api.SteerRun || steer.ThreadID != "t1" || steer.RunID != "r1" || steer.ParentAttemptID != "r1-attempt" || steer.Message != "纠正当前方向" {
		t.Fatalf("steer lost exact identity: %+v", steer)
	}
	if strings.Contains(w.actionNotice.Text, "Pi 已接收") {
		t.Fatal("accepted steer receipt must not claim Pi delivery")
	}

	queued := api.Input{ID: "input-q", ThreadID: "t1", Kind: "queue", Message: "持久排队下一条", Status: "queued", CreatedAt: baseTime}
	if !w.applyEvent(workbenchEvent(4, eventInputUpdated, queued)) {
		t.Fatal("queue projection missing")
	}
	if !w.cancelInput(queued) {
		t.Fatal("cancel was not dispatched")
	}
	commands = service.dispatched()
	cancel := commands[2]
	if cancel.Kind != api.CancelInput || cancel.InputID != "input-q" || cancel.ThreadID != "t1" {
		t.Fatalf("cancel command = %+v", cancel)
	}
	if w.cancelInput(queued) {
		t.Fatal("accepted cancellation repeated before input.updated")
	}
	queued.Status = "cancelled"
	w.applyEvent(workbenchEvent(5, eventInputUpdated, queued))

	pending := api.Input{ID: "input-s", ThreadID: "t1", RunID: "r1", AttemptID: "r1-attempt", Kind: "steer", Message: "纠正当前方向", Status: "pending"}
	w.applyEvent(workbenchEvent(6, eventInputUpdated, pending))
	tt := ui.NewTester(w.view, 1080, 900)
	mustClick(t, tt, "查看队列")
	if !tt.HasText("等待 Pi 接收") || tt.HasText("Pi 已接收") {
		t.Fatalf("pending steer delivery state is wrong: %q", tt.Texts())
	}
	pending.Status = "delivered"
	w.applyEvent(workbenchEvent(7, eventInputUpdated, pending))
	tt.Frame()
	if !tt.HasText("Pi 已接收") || tt.HasText("等待 Pi 接收") || tt.HasText("查看队列") {
		t.Fatalf("delivered steer did not replace its pending state: %q", tt.Texts())
	}
}

func TestControlRetryRetainsOriginalPayload(t *testing.T) {
	service := &fakeService{receiptErr: errors.New("receipt lost")}
	w := newWorkbench(service)
	w.applySnapshot(snapshotOneRun())
	w.setDraft("t1", "原始 Steer")
	w.queueDraft("t1", true)
	first := service.dispatched()[0]
	w.setDraft("t1", "不能替换原命令的新草稿")
	service.receiptErr = nil
	if !w.dispatchControl("steer:t1", "重试调整", "", api.Command{}, false) {
		t.Fatal("explicit retry should retain the original steer")
	}
	commands := service.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[1], first) || w.composerFor("t1").draft != "不能替换原命令的新草稿" {
		t.Fatalf("steer retry changed identity or draft: %+v", commands)
	}
}

func TestProviderRequestFormsSubmitAndLock(t *testing.T) {
	for _, method := range []string{"confirm", "select", "input", "editor"} {
		t.Run(method, func(t *testing.T) {
			w, service := newViewWorkbench(run("r1", "t1", api.RunRunning, "执行任务", "处理中", 0))
			request := api.ProviderRequest{
				ID: "req-" + method, ProviderRequestID: "pi-" + method, ThreadID: "t1", RunID: "r1", AttemptID: "r1-attempt",
				Method: method, Title: "需要用户响应", Options: []string{"选项 A", "选项 B"}, Prefill: "预填", Status: "pending",
			}
			w.requests = []api.ProviderRequest{request}
			tt := ui.NewTester(w.view, 1080, 900)
			wantValue := ""
			switch method {
			case "confirm":
				mustClick(t, tt, "允许执行")
			case "select":
				wantValue = "选项 A"
				mustClick(t, tt, "提交响应")
			case "input":
				mustClick(t, tt, "审批输入")
				tt.Type("-补充")
				wantValue = "预填-补充"
				mustClick(t, tt, "提交响应")
			case "editor":
				mustClick(t, tt, "审批内容")
				tt.Type("-补充")
				wantValue = "预填-补充"
				mustClick(t, tt, "提交响应")
			}
			commands := service.dispatched()
			if len(commands) != 1 {
				t.Fatalf("request commands = %+v, notice=%+v values=%v", commands, w.actionNotice, w.widgets.requestValues)
			}
			command := commands[0]
			if command.Kind != api.RespondRequest || command.RequestID != request.ID || command.Value != wantValue || command.Confirmed != (method == "confirm") || command.Cancelled {
				t.Fatalf("request response = %+v, want value %q", command, wantValue)
			}
			if w.respondRequest(request, wantValue, method == "confirm", false) || len(service.dispatched()) != 1 {
				t.Fatal("accepted response repeated while projection was pending")
			}
			request.Status = "responding"
			w.applyEvent(workbenchEvent(4, eventRequestUpdated, request))
			tt.Frame()
			if w.respondRequest(request, wantValue, method == "confirm", false) || len(service.dispatched()) != 1 {
				t.Fatal("responding request accepted another response")
			}
		})
	}
}

func TestToolDetailsExpandFromCommittedProjection(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "执行任务", "处理中", 0))
	w.tools = []api.ToolCall{{ID: "tool", ThreadID: "t1", RunID: "r1", Name: "bash", Arguments: json.RawMessage(`{"command":"go test ./..."}`), Output: "PASS", Status: "completed"}}
	tt := ui.NewTester(w.view, 1080, 900)
	if tt.HasText("参数") || tt.HasText("PASS") {
		t.Fatal("full parameters and output should start collapsed")
	}
	mustClick(t, tt, "展开工具")
	for _, want := range []string{"go test ./...", "PASS", "已完成", "命令", "输出"} {
		if !tt.HasText(want) {
			t.Errorf("expanded tool missing %q: %q", want, tt.Texts())
		}
	}
}

func TestModelsLoadOffViewAndNativeSelectionReachesCreate(t *testing.T) {
	service := newFakeQueryService()
	service.models = []api.ModelInfo{{ID: "openai/model-a", Name: "Model A", Provider: "openai"}}
	w := newWorkbench(service)
	w.applySnapshot(snapshotCompletedRun())
	w.newSession()
	held := holdWork(w)
	tt := ui.NewTester(w.view, 1080, 900)
	mustClick(t, tt, "选择模型")
	if len(held.calls) != 1 || !w.modelsLoading {
		t.Fatalf("catalog did not start asynchronously: calls=%d loading=%v", len(held.calls), w.modelsLoading)
	}
	held.release()
	tt.Frame()
	if !tt.HasText("Model A") {
		t.Fatalf("catalog choices missing: %q", tt.Texts())
	}
	mustClick(t, tt, "Model A")
	if w.defaults.Model != "openai/model-a" {
		t.Fatalf("native model selection = %q", w.defaults.Model)
	}
	mustClick(t, tt, "设置")
	mustClick(t, tt, "Agent 与权限")
	mustClick(t, tt, "每次询问")
	tt.Key(0, ui.KeyEscape)
	if w.defaults.ApprovalPolicy != "ask" || w.widgets.settingsOpen {
		t.Fatalf("approval selection = %q", w.defaults.ApprovalPolicy)
	}
	if got := w.modelOptions("does-not-match"); len(got) != 2 || got[0] != "" || got[1] != "openai/model-a" {
		t.Fatalf("filter must retain default/current selection: %v", got)
	}

	local := w.newSession()
	w.setDraft(local, "检查模型配置")
	w.send(local)
	held.release()
	commands := service.dispatched()
	if len(commands) != 2 || commands[0].Model != "openai/model-a" || commands[0].ApprovalPolicy != "ask" {
		t.Fatalf("create did not freeze model/approval: %+v", commands)
	}
}

func TestWorkspaceCallbacksAreAsyncAndIsolatedBySelection(t *testing.T) {
	service := newFakeQueryService()
	service.changes["t1"] = api.WorkspaceChanges{Path: "/work/one", Branch: "one", IsGit: true, Files: []api.WorkspaceFile{{Path: "one.go", Status: "M"}}, Diff: "diff-one"}
	service.changes["t2"] = api.WorkspaceChanges{Path: "/work/two", Branch: "two", IsGit: true, Files: []api.WorkspaceFile{{Path: "two.go", Status: "A"}}, Diff: "diff-two", Truncated: true}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "一", 0), thread("t2", "二", time.Minute)}})
	held := holdWork(w)
	w.widgets.workspaceOpen = true
	if !w.refreshWorkspace() || len(held.calls) != 1 {
		t.Fatal("workspace inspection was not scheduled")
	}
	if inspect, _ := service.workCalls(); len(inspect) != 0 {
		t.Fatal("inspection ran on the view call stack")
	}
	w.selectThread("t2")
	if len(held.calls) != 2 {
		t.Fatalf("selection did not schedule its own inspection: %d", len(held.calls))
	}
	held.release()
	inspect, _ := service.workCalls()
	if strings.Join(inspect, ",") != "t1,t2" || w.workspaceChanges == nil || w.workspaceChanges.Path != "/work/two" {
		t.Fatalf("stale callback won selection race: calls=%v changes=%+v", inspect, w.workspaceChanges)
	}
	tt := ui.NewTester(w.view, 1080, 900)
	for _, want := range []string{"/work/two", "two", "two.go", "查看全部变更", "内容过长"} {
		if !tt.HasText(want) {
			t.Errorf("workspace panel missing %q: %q", want, tt.Texts())
		}
	}
	if tt.HasText("diff-one") || tt.HasText("diff-two") {
		t.Fatal("raw diff should not displace the conversation")
	}
	mustClick(t, tt, "查看全部变更")
	if !tt.HasText("diff-two") || tt.HasText("diff-one") {
		t.Fatal("diff dialog did not use the selected workspace")
	}
	mustClick(t, tt, "关闭变更")

	mustClick(t, tt, "打开终端")
	if _, open := service.workCalls(); len(open) != 0 {
		t.Fatal("terminal call blocked the view")
	}
	held.release()
	_, open := service.workCalls()
	if len(open) != 1 || open[0] != "t2" {
		t.Fatalf("terminal calls = %v", open)
	}
}

func TestWorkspaceAndQueueKeepComposerInsideWindow(t *testing.T) {
	for _, size := range [][2]int{{1080, 700}, {640, 420}} {
		w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "正在执行", "回复", 0))
		w.widgets.workspaceOpen = true
		w.setDraft("t1", "下一条消息")
		w.workspaceChanges = &api.WorkspaceChanges{Path: "/tmp/project", IsGit: true, Diff: strings.Repeat("+changed line\n", 1000)}
		for i := 0; i < 10; i++ {
			w.inputs = append(w.inputs, api.Input{ID: fmt.Sprintf("queued-%d", i), ThreadID: "t1", Kind: "queue", Status: "queued", Message: "排队消息"})
		}
		tt := ui.NewTester(w.view, size[0], size[1])
		for _, label := range []string{"消息输入框", "停止", "追加信息", "运行中发送方式"} {
			field, ok := tt.Access(label)
			if !ok || field.Bounds.H < 10 || field.Bounds.Y < 0 || field.Bounds.Y+field.Bounds.H > float32(size[1]) || field.Bounds.X+field.Bounds.W > float32(size[0]) {
				t.Errorf("%s inaccessible at %v: %+v", label, size, field.Bounds)
			}
		}
		if !tt.HasText("文件变更") {
			t.Error("workspace panel disappeared")
		}
		if size[0] < 1000 {
			mustClick(t, tt, "收起工作区")
		}
		mustClick(t, tt, "运行中发送方式")
		mustClick(t, tt, "追加信息")
		steer, ok := tt.Access("追加信息")
		if !ok || steer.Bounds.X+steer.Bounds.W > float32(size[0]) || steer.Bounds.Y+steer.Bounds.H > float32(size[1]) {
			t.Errorf("Steer inaccessible: %+v", steer.Bounds)
		}
	}
}

func TestStateRoundTripRestoresSafeLocalStateOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "desktop-state.json")
	service := &fakeService{receiptErr: errors.New("lost receipt")}
	w := newWorkbench(service)
	w.defaults = Options{StatePath: path, ProviderID: "pi", Model: "openai/saved", ApprovalPolicy: "ask", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/saved/project"}}
	snap := api.Snapshot{Threads: []api.Thread{thread("t1", "一", 0)}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "旧任务", "完成", 0)}}
	w.applySnapshot(snap)
	w.setDraft("t1", "原会话草稿")
	w.setDraft("t1", "未确认原指令")
	w.send("t1")
	local := w.newSession()
	w.setDraft(local, "重启后保留的本地草稿")
	w.widgets.themeSet, w.widgets.dark = true, true
	w.widgets.sendWithEnter, w.widgets.manualDelivery = true, true
	w.saveState()

	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(data) {
		t.Fatalf("state file is not complete JSON: %v %q", err, data)
	}
	if strings.Contains(string(data), "未确认原指令") {
		t.Fatal("unconfirmed command was serialized as recoverable local state")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".desktop-state-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("atomic temporary files remain: %v %v", matches, err)
	}

	restoredService := &fakeService{}
	restored := newWorkbench(restoredService)
	restored.defaults = Options{StatePath: path, ProviderID: "pi", Model: "pi/current-default", ApprovalPolicy: "auto"}
	if err := restored.restoreState(); err != nil {
		t.Fatal(err)
	}
	restored.applySnapshot(snap)
	if restored.selected != local || restored.composerFor(local).draft != "重启后保留的本地草稿" {
		t.Fatalf("local selection/draft not restored: %q %+v", restored.selected, restored.composerFor(local))
	}
	if restored.composerFor("t1").draft != "" || restored.composerFor("t1").pending != nil {
		t.Fatal("unconfirmed command became a new draft or outbox item")
	}
	if !restored.widgets.dark || !restored.widgets.themeSet || !restored.widgets.sendWithEnter || !restored.widgets.manualDelivery {
		t.Fatalf("preferences not restored: dark=%v theme=%v sendWithEnter=%v manualDelivery=%v", restored.widgets.dark, restored.widgets.themeSet, restored.widgets.sendWithEnter, restored.widgets.manualDelivery)
	}
	if restored.defaults.Model != "pi/current-default" || restored.defaults.ApprovalPolicy != "ask" || restored.defaults.Workspace.Path != "/saved/project" {
		t.Fatalf("Pi startup model or saved preferences changed: %+v", restored.defaults)
	}
	if next := restored.newSession(); next == local {
		t.Fatal("restored local serial was reused")
	}
	if len(restoredService.dispatched()) != 0 {
		t.Fatal("state restoration dispatched a user command")
	}
}
