package local

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

type orchestrationMock struct {
	*fakeService
	snapshotCalls atomic.Int32
	backendCalls  atomic.Int32

	capabilitiesFunc  func(context.Context, api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error)
	delegateTaskFunc  func(context.Context, api.DelegateTaskRequest) (api.TaskDetail, error)
	taskStatusFunc    func(context.Context, api.TaskStatusQuery) (api.TaskStatusResult, error)
	cancelTaskFunc    func(context.Context, api.CancelTaskRequest) (api.CancelTaskResult, error)
	listThreadsFunc   func(context.Context, api.ThreadListQuery) (api.ThreadListResult, error)
	readThreadFunc    func(context.Context, api.ThreadReadQuery) (api.ThreadReadResult, error)
	launchThreadFunc  func(context.Context, api.ThreadLaunchRequest) (api.LaunchedThread, error)
	createThreadsFunc func(context.Context, api.CreateThreadsRequest) (api.CreateThreadsResult, error)
	sendToThreadFunc  func(context.Context, api.InterThreadSendRequest) (api.InterThreadSendResult, error)
	waitThreadFunc    func(context.Context, api.ThreadWaitQuery) (api.ThreadWaitResult, error)
	searchFunc        func(context.Context, api.SearchQuery) ([]api.SearchHit, error)
}

func (m *orchestrationMock) Snapshot(ctx context.Context) (api.Snapshot, error) {
	m.snapshotCalls.Add(1)
	return m.fakeService.Snapshot(ctx)
}

func (m *orchestrationMock) OrchestratorCapabilities(ctx context.Context, q api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
	m.backendCalls.Add(1)
	if m.capabilitiesFunc != nil {
		return m.capabilitiesFunc(ctx, q)
	}
	return api.OrchestratorCapabilities{}, nil
}

func (m *orchestrationMock) DelegateTask(ctx context.Context, req api.DelegateTaskRequest) (api.TaskDetail, error) {
	m.backendCalls.Add(1)
	if m.delegateTaskFunc != nil {
		return m.delegateTaskFunc(ctx, req)
	}
	return api.TaskDetail{}, nil
}

func (m *orchestrationMock) TaskStatus(ctx context.Context, q api.TaskStatusQuery) (api.TaskStatusResult, error) {
	m.backendCalls.Add(1)
	if m.taskStatusFunc != nil {
		return m.taskStatusFunc(ctx, q)
	}
	return api.TaskStatusResult{}, nil
}

func (m *orchestrationMock) CancelTask(ctx context.Context, req api.CancelTaskRequest) (api.CancelTaskResult, error) {
	m.backendCalls.Add(1)
	if m.cancelTaskFunc != nil {
		return m.cancelTaskFunc(ctx, req)
	}
	return api.CancelTaskResult{}, nil
}

func (m *orchestrationMock) ListThreads(ctx context.Context, q api.ThreadListQuery) (api.ThreadListResult, error) {
	m.backendCalls.Add(1)
	if m.listThreadsFunc != nil {
		return m.listThreadsFunc(ctx, q)
	}
	return api.ThreadListResult{}, nil
}

func (m *orchestrationMock) ReadThread(ctx context.Context, q api.ThreadReadQuery) (api.ThreadReadResult, error) {
	m.backendCalls.Add(1)
	if m.readThreadFunc != nil {
		return m.readThreadFunc(ctx, q)
	}
	return api.ThreadReadResult{}, nil
}

func (m *orchestrationMock) LaunchThread(ctx context.Context, req api.ThreadLaunchRequest) (api.LaunchedThread, error) {
	m.backendCalls.Add(1)
	if m.launchThreadFunc != nil {
		return m.launchThreadFunc(ctx, req)
	}
	return api.LaunchedThread{}, nil
}

func (m *orchestrationMock) CreateThreads(ctx context.Context, req api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
	m.backendCalls.Add(1)
	if m.createThreadsFunc != nil {
		return m.createThreadsFunc(ctx, req)
	}
	return api.CreateThreadsResult{}, nil
}

func (m *orchestrationMock) SendToThread(ctx context.Context, req api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
	m.backendCalls.Add(1)
	if m.sendToThreadFunc != nil {
		return m.sendToThreadFunc(ctx, req)
	}
	return api.InterThreadSendResult{}, nil
}

func (m *orchestrationMock) WaitThread(ctx context.Context, q api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
	m.backendCalls.Add(1)
	if m.waitThreadFunc != nil {
		return m.waitThreadFunc(ctx, q)
	}
	return api.ThreadWaitResult{}, nil
}

func (m *orchestrationMock) Search(ctx context.Context, q api.SearchQuery) ([]api.SearchHit, error) {
	m.backendCalls.Add(1)
	if m.searchFunc != nil {
		return m.searchFunc(ctx, q)
	}
	return nil, nil
}

// brokerOnlyMock implements api.Service and api.OrchestrationBroker, without Search.
type brokerOnlyMock struct {
	*fakeService
	listResult api.ThreadListResult
}

func (m *brokerOnlyMock) OrchestratorCapabilities(context.Context, api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
	return api.OrchestratorCapabilities{}, nil
}
func (m *brokerOnlyMock) DelegateTask(context.Context, api.DelegateTaskRequest) (api.TaskDetail, error) {
	return api.TaskDetail{}, nil
}
func (m *brokerOnlyMock) TaskStatus(context.Context, api.TaskStatusQuery) (api.TaskStatusResult, error) {
	return api.TaskStatusResult{}, nil
}
func (m *brokerOnlyMock) CancelTask(context.Context, api.CancelTaskRequest) (api.CancelTaskResult, error) {
	return api.CancelTaskResult{}, nil
}
func (m *brokerOnlyMock) ListThreads(context.Context, api.ThreadListQuery) (api.ThreadListResult, error) {
	return m.listResult, nil
}
func (m *brokerOnlyMock) ReadThread(context.Context, api.ThreadReadQuery) (api.ThreadReadResult, error) {
	return api.ThreadReadResult{}, nil
}
func (m *brokerOnlyMock) LaunchThread(context.Context, api.ThreadLaunchRequest) (api.LaunchedThread, error) {
	return api.LaunchedThread{}, nil
}
func (m *brokerOnlyMock) CreateThreads(context.Context, api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
	return api.CreateThreadsResult{}, nil
}
func (m *brokerOnlyMock) SendToThread(context.Context, api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
	return api.InterThreadSendResult{}, nil
}
func (m *brokerOnlyMock) WaitThread(context.Context, api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
	return api.ThreadWaitResult{}, nil
}

// searchThreadsOnlyMock simulates *core.Core exposing SearchThreads rather than Search.
type searchThreadsOnlyMock struct {
	*fakeService
	searchThreadsFunc func(context.Context, api.SearchQuery) ([]api.SearchHit, error)
}

func (m *searchThreadsOnlyMock) SearchThreads(ctx context.Context, q api.SearchQuery) ([]api.SearchHit, error) {
	return m.searchThreadsFunc(ctx, q)
}

var allOrchestrationPaths = []string{
	pathOrchestrationCapabilities,
	pathOrchestrationDelegate,
	pathOrchestrationTaskStatus,
	pathOrchestrationTaskCancel,
	pathOrchestrationThreadsList,
	pathOrchestrationThreadsRead,
	pathOrchestrationThreadsLaunch,
	pathOrchestrationThreadsCreateBatch,
	pathOrchestrationThreadsSend,
	pathOrchestrationThreadsWait,
	pathOrchestrationThreadsSearch,
}

func TestOrchestrationAllRoutesRoundTrip(t *testing.T) {
	fixedTime := time.Date(2026, 4, 20, 12, 30, 0, 0, time.UTC)
	settledFalse := false
	snoozedTrue := true
	nextCursor := 25
	afterPos := 10
	beforePos := 50
	nextPos := 20
	earlierPos := 5
	nextTextOff := 4096
	snoozeAt := fixedTime.Add(2 * time.Hour)

	wantCapQuery := api.OrchestratorCapabilitiesQuery{
		CallerThreadID: "thread-parent",
		Search:         "sonnet",
	}
	wantCaps := api.OrchestratorCapabilities{
		ParentThreadID:      "thread-parent",
		InheritedProviderID: "pi",
		InheritedModel:      "pi/claude-sonnet-4",
		InheritedThinking:   "high",
		ApprovalPolicy:      "auto-accept-edits",
		InteractionMode:     "default",
		Models: []api.OrchestratorModelCapability{
			{
				ID:    "pi/claude-sonnet-4",
				Label: "Claude Sonnet 4",
				Options: []api.OrchestratorModelOption{
					{ID: "thinking", Values: []string{"low", "medium", "high"}, Default: "high"},
				},
			},
		},
		ApprovalPolicies: []string{"approval-required", "auto-accept-edits", "full-access"},
		InteractionModes: []string{"default", "plan"},
		MaxBatchThreads:  8,
	}

	wantDelegateReq := api.DelegateTaskRequest{
		ClientRequestID: "req-delegate-1",
		CallerThreadID:  "thread-parent",
		ParentRunID:     "run-parent-1",
		ParentAttemptID: "attempt-1",
		Title:           "Transport implementation",
		Task:            "Implement orchestration UDS server and client",
		Role:            "implementation",
		Model:           "pi/claude-sonnet-4",
		Thinking:        "high",
		Isolation:       "worktree",
		Mode:            "async",
		TimeoutMs:       30000,
		ApprovalPolicy:  "auto-accept-edits",
		InteractionMode: "default",
		ResumePolicy:    "all_completed",
	}
	wantTaskDetail := api.TaskDetail{
		TaskID:              "task-1",
		ParentThreadID:      "thread-parent",
		ParentRunID:         "run-parent-1",
		ChildThreadID:       "thread-child-1",
		Link:                api.FormatThreadLink("thread-child-1", "Transport implementation"),
		ChildRunID:          "run-child-1",
		Title:               "Transport implementation",
		Status:              api.RunRunning,
		WorkState:           "working",
		DeliveryStatus:      api.DeliveryPending,
		HasPendingChildRuns: false,
		HasPendingQuestion:  false,
		PendingApproval: &api.PendingApprovalSummary{
			RequestID: "approval-1",
			Method:    "bash",
			Title:     "Run go test",
			RunID:     "run-child-1",
		},
		Model:     "pi/claude-sonnet-4",
		Next:      api.OrchestratorRunningTaskNext,
		UpdatedAt: fixedTime,
	}

	wantStatusQuery := api.TaskStatusQuery{
		CallerThreadID: "thread-parent",
		ParentRunID:    "run-parent-1",
		TaskID:         "task-1",
		Acknowledge:    true,
	}
	wantStatusResult := api.TaskStatusResult{
		ThreadID: "thread-parent",
		RunID:    "run-parent-1",
		Tasks:    []api.TaskDetail{wantTaskDetail},
		Cohorts: []api.Cohort{
			{
				ParentRunID:  "run-parent-1",
				Generation:   1,
				ResumePolicy: "all_completed",
				Status:       api.DeliveryAcknowledged,
				CreatedAt:    fixedTime,
				UpdatedAt:    fixedTime,
			},
		},
	}

	wantCancelReq := api.CancelTaskRequest{
		ClientRequestID: "req-cancel-1",
		CallerThreadID:  "thread-parent",
		TaskID:          "task-1",
		Reason:          "superseded",
	}
	wantCancelResult := api.CancelTaskResult{
		TaskID: "task-1",
		Status: string(api.RunCancelled),
	}

	wantListQuery := api.ThreadListQuery{
		CallerThreadID:   "thread-parent",
		Workspace:        "/tmp/vantry",
		AllProjects:      true,
		IncludeSubagents: true,
		Settled:          &settledFalse,
		Snoozed:          &snoozedTrue,
		Statuses:         []string{"running", "queued"},
		TitleContains:    "Transport",
		Cursor:           5,
		Limit:            20,
	}
	wantListItem := api.ThreadListItem{
		ThreadID:          "thread-child-1",
		Title:             "Transport implementation",
		Link:              api.FormatThreadLink("thread-child-1", "Transport implementation"),
		Status:            "running",
		Model:             "pi/claude-sonnet-4",
		ApprovalPolicy:    "auto-accept-edits",
		InteractionMode:   "default",
		WorkspacePath:     "/tmp/vantry",
		Branch:            "lane-2",
		IsWorktree:        true,
		ParentThreadID:    "thread-parent",
		CreatedByThreadID: "thread-parent",
		Pinned:            true,
		Settled:           false,
		Snoozed:           true,
		SnoozedUntil:      &snoozeAt,
		RunCount:          2,
		PendingInputs:     1,
		PendingRequests:   1,
		UpdatedAt:         fixedTime,
	}
	wantListResult := api.ThreadListResult{
		CurrentThreadID: "thread-parent",
		Threads:         []api.ThreadListItem{wantListItem},
		NextCursor:      &nextCursor,
		Total:           45,
	}

	wantReadQuery := api.ThreadReadQuery{
		CallerThreadID:  "thread-parent",
		ThreadID:        "thread-child-1",
		View:            "activity",
		AfterPosition:   &afterPos,
		BeforePosition:  &beforePos,
		Limit:           15,
		RunLimit:        5,
		MaxCharsPerItem: 2048,
		ItemID:          "item-12",
		TextOffset:      1024,
	}
	wantReadResult := api.ThreadReadResult{
		Thread: wantListItem,
		RecentRuns: []api.ThreadRunSummary{
			{
				RunID:          "run-child-1",
				Ordinal:        1,
				Status:         api.RunRunning,
				Model:          "pi/claude-sonnet-4",
				SenderThreadID: "thread-parent",
				CreatedAt:      fixedTime,
				UpdatedAt:      fixedTime,
			},
		},
		Items: []api.ThreadTimelineItem{
			{
				Position:       12,
				ItemID:         "item-12",
				RunID:          "run-child-1",
				CreatedBy:      "agent",
				SenderThreadID: "thread-parent",
				Type:           "assistant_message",
				Status:         "streaming",
				Title:          "Progress",
				Text:           "Wiring orchestration routes...",
				TextTruncated:  true,
				NextTextOffset: &nextTextOff,
				UpdatedAt:      fixedTime,
			},
		},
		NextPosition:    &nextPos,
		EarlierPosition: &earlierPos,
		HasMore:         true,
	}

	wantLaunchReq := api.ThreadLaunchRequest{
		ClientRequestID: "req-launch-1",
		CallerThreadID:  "thread-parent",
		CallerRunID:     "run-parent-1",
		ThreadLaunchSpec: api.ThreadLaunchSpec{
			Title:           "Peer review thread",
			Message:         "Review transport changes",
			Model:           "pi/claude-sonnet-4",
			Thinking:        "medium",
			ApprovalPolicy:  "approval-required",
			InteractionMode: "plan",
			Workspace: &api.WorkspaceSpec{
				Kind:       "worktree",
				Repository: "/tmp/vantry",
				Branch:     "review-lane-2",
				BaseRef:    "origin/main",
			},
			Scratch: false,
		},
	}
	wantLaunched := api.LaunchedThread{
		ThreadID: "thread-launched-1",
		Link:     api.FormatThreadLink("thread-launched-1", "Peer review thread"),
		Title:    "Peer review thread",
		RunID:    "run-launched-1",
		Status:   api.RunQueued,
		Model:    "pi/claude-sonnet-4",
	}

	wantBatchReq := api.CreateThreadsRequest{
		ClientRequestID: "req-batch-1",
		CallerThreadID:  "thread-parent",
		CallerRunID:     "run-parent-1",
		Threads: []api.ThreadLaunchSpec{
			{Title: "Worker A", Message: "Check server", Scratch: true},
			{Title: "Worker B", Message: "Check client", Model: "pi/claude-sonnet-4"},
		},
	}
	wantBatchResult := api.CreateThreadsResult{
		Threads: []api.LaunchedThread{
			{ThreadID: "thread-a", Link: api.FormatThreadLink("thread-a", "Worker A"), Title: "Worker A", RunID: "run-a", Status: api.RunQueued},
			{ThreadID: "thread-b", Link: api.FormatThreadLink("thread-b", "Worker B"), Title: "Worker B", RunID: "run-b", Status: api.RunRunning, Model: "pi/claude-sonnet-4"},
		},
	}

	wantSendReq := api.InterThreadSendRequest{
		ClientRequestID: "req-send-1",
		CallerThreadID:  "thread-parent",
		TargetThreadID:  "thread-launched-1",
		Message:         "Please also check cancellation behavior",
		Mode:            "steer",
	}
	wantSendResult := api.InterThreadSendResult{
		ThreadID: "thread-launched-1",
		Link:     api.FormatThreadLink("thread-launched-1", "Peer review thread"),
		RunID:    "run-launched-1",
		InputID:  "input-9",
		Status:   api.RunRunning,
		Delivery: "steered",
		Sequence: 77,
	}

	wantWaitQuery := api.ThreadWaitQuery{
		CallerThreadID:            "thread-parent",
		ThreadID:                  "thread-launched-1",
		RunID:                     "run-launched-1",
		TimeoutMs:                 15000,
		UntilDelegatedWorkSettled: true,
	}
	wantWaitResult := api.ThreadWaitResult{
		ThreadID:           "thread-launched-1",
		Link:               api.FormatThreadLink("thread-launched-1", "Peer review thread"),
		RunID:              "run-launched-1",
		Status:             string(api.RunCompleted),
		TimedOut:           false,
		Summary:            "All transport checks passed.",
		HasPendingQuestion: false,
	}

	wantSearchQuery := api.SearchQuery{
		Query:           "orchestration",
		IncludeArchived: true,
		ThreadID:        "thread-launched-1",
		Limit:           25,
	}
	wantSearchHits := []api.SearchHit{
		{
			ThreadID:  "thread-launched-1",
			RunID:     "run-launched-1",
			Title:     "Peer review thread",
			Field:     api.SearchOutput,
			Snippet:   "…orchestration routes verified…",
			Score:     100,
			UpdatedAt: fixedTime,
		},
	}

	service := &orchestrationMock{
		fakeService: &fakeService{},
		capabilitiesFunc: func(_ context.Context, got api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
			if !reflect.DeepEqual(got, wantCapQuery) {
				t.Errorf("OrchestratorCapabilities query = %#v, want %#v", got, wantCapQuery)
			}
			return wantCaps, nil
		},
		delegateTaskFunc: func(_ context.Context, got api.DelegateTaskRequest) (api.TaskDetail, error) {
			if !reflect.DeepEqual(got, wantDelegateReq) {
				t.Errorf("DelegateTask req = %#v, want %#v", got, wantDelegateReq)
			}
			return wantTaskDetail, nil
		},
		taskStatusFunc: func(_ context.Context, got api.TaskStatusQuery) (api.TaskStatusResult, error) {
			if !reflect.DeepEqual(got, wantStatusQuery) {
				t.Errorf("TaskStatus query = %#v, want %#v", got, wantStatusQuery)
			}
			return wantStatusResult, nil
		},
		cancelTaskFunc: func(_ context.Context, got api.CancelTaskRequest) (api.CancelTaskResult, error) {
			if !reflect.DeepEqual(got, wantCancelReq) {
				t.Errorf("CancelTask req = %#v, want %#v", got, wantCancelReq)
			}
			return wantCancelResult, nil
		},
		listThreadsFunc: func(_ context.Context, got api.ThreadListQuery) (api.ThreadListResult, error) {
			if !reflect.DeepEqual(got, wantListQuery) {
				t.Errorf("ListThreads query = %#v, want %#v", got, wantListQuery)
			}
			return wantListResult, nil
		},
		readThreadFunc: func(_ context.Context, got api.ThreadReadQuery) (api.ThreadReadResult, error) {
			if !reflect.DeepEqual(got, wantReadQuery) {
				t.Errorf("ReadThread query = %#v, want %#v", got, wantReadQuery)
			}
			return wantReadResult, nil
		},
		launchThreadFunc: func(_ context.Context, got api.ThreadLaunchRequest) (api.LaunchedThread, error) {
			if !reflect.DeepEqual(got, wantLaunchReq) {
				t.Errorf("LaunchThread req = %#v, want %#v", got, wantLaunchReq)
			}
			return wantLaunched, nil
		},
		createThreadsFunc: func(_ context.Context, got api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
			if !reflect.DeepEqual(got, wantBatchReq) {
				t.Errorf("CreateThreads req = %#v, want %#v", got, wantBatchReq)
			}
			return wantBatchResult, nil
		},
		sendToThreadFunc: func(_ context.Context, got api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
			if !reflect.DeepEqual(got, wantSendReq) {
				t.Errorf("SendToThread req = %#v, want %#v", got, wantSendReq)
			}
			return wantSendResult, nil
		},
		waitThreadFunc: func(_ context.Context, got api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
			if !reflect.DeepEqual(got, wantWaitQuery) {
				t.Errorf("WaitThread query = %#v, want %#v", got, wantWaitQuery)
			}
			return wantWaitResult, nil
		},
		searchFunc: func(_ context.Context, got api.SearchQuery) ([]api.SearchHit, error) {
			if got.Query == "empty" {
				return nil, nil
			}
			if !reflect.DeepEqual(got, wantSearchQuery) {
				t.Errorf("Search query = %#v, want %#v", got, wantSearchQuery)
			}
			return wantSearchHits, nil
		},
	}

	ts := startTestServer(t, service)
	client := startTestClient(t, ts)
	ctx := context.Background()

	gotCaps, err := client.OrchestratorCapabilities(ctx, wantCapQuery)
	if err != nil || !reflect.DeepEqual(gotCaps, wantCaps) {
		t.Fatalf("OrchestratorCapabilities() = %#v, err = %v", gotCaps, err)
	}

	gotTask, err := client.DelegateTask(ctx, wantDelegateReq)
	if err != nil || !reflect.DeepEqual(gotTask, wantTaskDetail) {
		t.Fatalf("DelegateTask() = %#v, err = %v", gotTask, err)
	}

	gotStatus, err := client.TaskStatus(ctx, wantStatusQuery)
	if err != nil || !reflect.DeepEqual(gotStatus, wantStatusResult) {
		t.Fatalf("TaskStatus() = %#v, err = %v", gotStatus, err)
	}

	gotCancel, err := client.CancelTask(ctx, wantCancelReq)
	if err != nil || !reflect.DeepEqual(gotCancel, wantCancelResult) {
		t.Fatalf("CancelTask() = %#v, err = %v", gotCancel, err)
	}

	gotList, err := client.ListThreads(ctx, wantListQuery)
	if err != nil || !reflect.DeepEqual(gotList, wantListResult) {
		t.Fatalf("ListThreads() = %#v, err = %v", gotList, err)
	}

	gotRead, err := client.ReadThread(ctx, wantReadQuery)
	if err != nil || !reflect.DeepEqual(gotRead, wantReadResult) {
		t.Fatalf("ReadThread() = %#v, err = %v", gotRead, err)
	}

	gotLaunched, err := client.LaunchThread(ctx, wantLaunchReq)
	if err != nil || !reflect.DeepEqual(gotLaunched, wantLaunched) {
		t.Fatalf("LaunchThread() = %#v, err = %v", gotLaunched, err)
	}

	gotBatch, err := client.CreateThreads(ctx, wantBatchReq)
	if err != nil || !reflect.DeepEqual(gotBatch, wantBatchResult) {
		t.Fatalf("CreateThreads() = %#v, err = %v", gotBatch, err)
	}

	gotSend, err := client.SendToThread(ctx, wantSendReq)
	if err != nil || !reflect.DeepEqual(gotSend, wantSendResult) {
		t.Fatalf("SendToThread() = %#v, err = %v", gotSend, err)
	}

	gotWait, err := client.WaitThread(ctx, wantWaitQuery)
	if err != nil || !reflect.DeepEqual(gotWait, wantWaitResult) {
		t.Fatalf("WaitThread() = %#v, err = %v", gotWait, err)
	}

	gotHits, err := client.Search(ctx, wantSearchQuery)
	if err != nil || !reflect.DeepEqual(gotHits, wantSearchHits) {
		t.Fatalf("Search() = %#v, err = %v", gotHits, err)
	}

	// Verify raw wire shape {"hits": [...]} and non-null empty array {"hits": []}.
	resp, rawBody := rawHTTPRequest(t, ts.socket, http.MethodPost, pathOrchestrationThreadsSearch, `{"query":"empty"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw search status = %d, body = %s", resp.StatusCode, rawBody)
	}
	var rawObj map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &rawObj); err != nil {
		t.Fatalf("decode raw search envelope: %v", err)
	}
	if len(rawObj) != 1 || string(rawObj["hits"]) != "[]" {
		t.Fatalf("raw search envelope = %s, want {\"hits\":[]}", rawBody)
	}

	emptyHits, err := client.Search(ctx, api.SearchQuery{Query: "empty"})
	if err != nil || emptyHits == nil || len(emptyHits) != 0 {
		t.Fatalf("Search(empty) = %#v, err = %v; want non-nil empty slice", emptyHits, err)
	}

	if calls := service.snapshotCalls.Load(); calls != 0 {
		t.Fatalf("orchestration endpoints triggered %d Snapshot calls; want 0", calls)
	}
}

func TestOrchestrationSearchThreadsFallback(t *testing.T) {
	wantHit := api.SearchHit{
		ThreadID: "thread-core",
		Title:    "Core SearchThreads",
		Field:    api.SearchTitle,
		Snippet:  "Core SearchThreads",
		Score:    300,
	}
	service := &searchThreadsOnlyMock{
		fakeService: &fakeService{},
		searchThreadsFunc: func(_ context.Context, q api.SearchQuery) ([]api.SearchHit, error) {
			if q.Query != "Core" || q.Limit != 5 {
				t.Errorf("SearchThreads query = %#v", q)
			}
			return []api.SearchHit{wantHit}, nil
		},
	}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	hits, err := client.SearchThreads(context.Background(), api.SearchQuery{Query: "Core", Limit: 5})
	if err != nil {
		t.Fatalf("SearchThreads() error = %v", err)
	}
	if len(hits) != 1 || hits[0].ThreadID != wantHit.ThreadID {
		t.Fatalf("SearchThreads() = %#v, want [%#v]", hits, wantHit)
	}
}

func TestOrchestrationTypedErrorsAcrossAllRoutes(t *testing.T) {
	codes := []api.ErrorCode{
		api.NotFound,
		api.Conflict,
		api.InvalidCommand,
		api.NotReady,
	}

	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			wantErr := &api.Error{Code: code, Message: "expected " + string(code)}
			service := &orchestrationMock{
				fakeService: &fakeService{},
				capabilitiesFunc: func(context.Context, api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
					return api.OrchestratorCapabilities{}, wantErr
				},
				delegateTaskFunc: func(context.Context, api.DelegateTaskRequest) (api.TaskDetail, error) {
					return api.TaskDetail{}, wantErr
				},
				taskStatusFunc: func(context.Context, api.TaskStatusQuery) (api.TaskStatusResult, error) {
					return api.TaskStatusResult{}, wantErr
				},
				cancelTaskFunc: func(context.Context, api.CancelTaskRequest) (api.CancelTaskResult, error) {
					return api.CancelTaskResult{}, wantErr
				},
				listThreadsFunc: func(context.Context, api.ThreadListQuery) (api.ThreadListResult, error) {
					return api.ThreadListResult{}, wantErr
				},
				readThreadFunc: func(context.Context, api.ThreadReadQuery) (api.ThreadReadResult, error) {
					return api.ThreadReadResult{}, wantErr
				},
				launchThreadFunc: func(context.Context, api.ThreadLaunchRequest) (api.LaunchedThread, error) {
					return api.LaunchedThread{}, wantErr
				},
				createThreadsFunc: func(context.Context, api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
					return api.CreateThreadsResult{}, wantErr
				},
				sendToThreadFunc: func(context.Context, api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
					return api.InterThreadSendResult{}, wantErr
				},
				waitThreadFunc: func(context.Context, api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
					return api.ThreadWaitResult{}, wantErr
				},
				searchFunc: func(context.Context, api.SearchQuery) ([]api.SearchHit, error) {
					return nil, wantErr
				},
			}
			ts := startTestServer(t, service)
			client := startTestClient(t, ts)
			ctx := context.Background()

			calls := []struct {
				name string
				run  func() error
			}{
				{"capabilities", func() error {
					_, err := client.OrchestratorCapabilities(ctx, api.OrchestratorCapabilitiesQuery{})
					return err
				}},
				{"delegate", func() error { _, err := client.DelegateTask(ctx, api.DelegateTaskRequest{}); return err }},
				{"task-status", func() error { _, err := client.TaskStatus(ctx, api.TaskStatusQuery{}); return err }},
				{"task-cancel", func() error { _, err := client.CancelTask(ctx, api.CancelTaskRequest{}); return err }},
				{"threads/list", func() error { _, err := client.ListThreads(ctx, api.ThreadListQuery{}); return err }},
				{"threads/read", func() error { _, err := client.ReadThread(ctx, api.ThreadReadQuery{}); return err }},
				{"threads/launch", func() error { _, err := client.LaunchThread(ctx, api.ThreadLaunchRequest{}); return err }},
				{"threads/create-batch", func() error { _, err := client.CreateThreads(ctx, api.CreateThreadsRequest{}); return err }},
				{"threads/send", func() error { _, err := client.SendToThread(ctx, api.InterThreadSendRequest{}); return err }},
				{"threads/wait", func() error { _, err := client.WaitThread(ctx, api.ThreadWaitQuery{}); return err }},
				{"threads/search", func() error { _, err := client.Search(ctx, api.SearchQuery{}); return err }},
			}

			for _, call := range calls {
				err := call.run()
				var apiErr *api.Error
				if !errors.As(err, &apiErr) {
					t.Fatalf("%s error = %T %v, want *api.Error", call.name, err, err)
				}
				if apiErr.Code != code || apiErr.Message != wantErr.Message {
					t.Fatalf("%s error = %#v, want %#v", call.name, apiErr, wantErr)
				}
			}
		})
	}
}

func TestOrchestrationRejectsMalformedAndUnknownFieldRequests(t *testing.T) {
	service := &orchestrationMock{fakeService: &fakeService{}}
	ts := startTestServer(t, service)

	oversizedBody := `{"unexpectedLarge":"` + strings.Repeat("a", maxCommandBytes) + `"}`

	for _, path := range allOrchestrationPaths {
		t.Run(path, func(t *testing.T) {
			cases := []struct {
				name       string
				method     string
				target     string
				body       string
				wantStatus int
			}{
				{
					name:       "wrong method GET",
					method:     http.MethodGet,
					target:     path,
					wantStatus: http.StatusMethodNotAllowed,
				},
				{
					name:       "query parameter rejected",
					method:     http.MethodPost,
					target:     path + "?extra=1",
					body:       `{}`,
					wantStatus: http.StatusBadRequest,
				},
				{
					name:       "unknown field rejected",
					method:     http.MethodPost,
					target:     path,
					body:       `{"unknownField":true}`,
					wantStatus: http.StatusBadRequest,
				},
				{
					name:       "null body rejected",
					method:     http.MethodPost,
					target:     path,
					body:       `null`,
					wantStatus: http.StatusBadRequest,
				},
				{
					name:       "multiple JSON objects rejected",
					method:     http.MethodPost,
					target:     path,
					body:       `{} {}`,
					wantStatus: http.StatusBadRequest,
				},
				{
					name:       "malformed JSON rejected",
					method:     http.MethodPost,
					target:     path,
					body:       `{"broken":`,
					wantStatus: http.StatusBadRequest,
				},
				{
					name:       "oversized body rejected",
					method:     http.MethodPost,
					target:     path,
					body:       oversizedBody,
					wantStatus: http.StatusBadRequest,
				},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					resp, body := rawHTTPRequest(t, ts.socket, tc.method, tc.target, tc.body)
					defer resp.Body.Close()
					if resp.StatusCode != tc.wantStatus {
						t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, tc.wantStatus, body)
					}
					if tc.wantStatus == http.StatusMethodNotAllowed && resp.Header.Get("Allow") != http.MethodPost {
						t.Fatalf("Allow header = %q, want %q", resp.Header.Get("Allow"), http.MethodPost)
					}
					var apiErr api.Error
					if err := json.Unmarshal(body, &apiErr); err != nil {
						t.Fatalf("decode error body %q: %v", body, err)
					}
					if apiErr.Code != api.InvalidCommand || apiErr.Message == "" {
						t.Fatalf("error = %#v, want non-empty invalid_command", apiErr)
					}
				})
			}
		})
	}

	if got := service.backendCalls.Load(); got != 0 {
		t.Fatalf("malformed requests reached backend %d times; want 0", got)
	}

	// Client-side size check also rejects oversized payloads before hitting the socket.
	client := startTestClient(t, ts)
	_, err := client.DelegateTask(context.Background(), api.DelegateTaskRequest{
		ParentRunID: "run-1",
		Task:        strings.Repeat("x", maxCommandBytes),
	})
	if api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("client oversized DelegateTask error = %v, want invalid_command", err)
	}
	if got := service.backendCalls.Load(); got != 0 {
		t.Fatalf("oversized client request reached backend %d times; want 0", got)
	}
}

func TestOrchestrationLegacyServiceReturnsNotFound(t *testing.T) {
	ts := startTestServer(t, &fakeService{})
	client := startTestClient(t, ts)
	ctx := context.Background()

	calls := []struct {
		name string
		run  func() error
	}{
		{"capabilities", func() error {
			_, err := client.OrchestratorCapabilities(ctx, api.OrchestratorCapabilitiesQuery{})
			return err
		}},
		{"delegate", func() error { _, err := client.DelegateTask(ctx, api.DelegateTaskRequest{}); return err }},
		{"task-status", func() error { _, err := client.TaskStatus(ctx, api.TaskStatusQuery{}); return err }},
		{"task-cancel", func() error { _, err := client.CancelTask(ctx, api.CancelTaskRequest{}); return err }},
		{"threads/list", func() error { _, err := client.ListThreads(ctx, api.ThreadListQuery{}); return err }},
		{"threads/read", func() error { _, err := client.ReadThread(ctx, api.ThreadReadQuery{}); return err }},
		{"threads/launch", func() error { _, err := client.LaunchThread(ctx, api.ThreadLaunchRequest{}); return err }},
		{"threads/create-batch", func() error { _, err := client.CreateThreads(ctx, api.CreateThreadsRequest{}); return err }},
		{"threads/send", func() error { _, err := client.SendToThread(ctx, api.InterThreadSendRequest{}); return err }},
		{"threads/wait", func() error { _, err := client.WaitThread(ctx, api.ThreadWaitQuery{}); return err }},
		{"threads/search", func() error { _, err := client.Search(ctx, api.SearchQuery{Query: "needle"}); return err }},
	}

	for _, call := range calls {
		err := call.run()
		if api.CodeOf(err) != api.NotFound {
			t.Fatalf("legacy %s error = %v (code=%s), want not_found", call.name, err, api.CodeOf(err))
		}
	}

	// Service implementing OrchestrationBroker without Search still serves broker
	// endpoints and returns NotFound only on /v1/orchestration/threads/search.
	brokerOnly := &brokerOnlyMock{
		fakeService: &fakeService{},
		listResult:  api.ThreadListResult{Total: 3, Threads: []api.ThreadListItem{}},
	}
	tsBroker := startTestServer(t, brokerOnly)
	clientBroker := startTestClient(t, tsBroker)

	listed, err := clientBroker.ListThreads(ctx, api.ThreadListQuery{})
	if err != nil || listed.Total != 3 {
		t.Fatalf("brokerOnly ListThreads() = %#v, err = %v", listed, err)
	}
	if _, err := clientBroker.Search(ctx, api.SearchQuery{Query: "needle"}); api.CodeOf(err) != api.NotFound {
		t.Fatalf("brokerOnly Search() error = %v, want not_found", err)
	}
}

func TestOrchestrationBlockingCallsRespectContextAndLongWaits(t *testing.T) {
	t.Run("WaitThread cancellation propagates across socket", func(t *testing.T) {
		entered := make(chan struct{})
		finished := make(chan struct{})
		service := &orchestrationMock{
			fakeService: &fakeService{},
			waitThreadFunc: func(ctx context.Context, q api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
				if q.ThreadID != "thread-wait" {
					t.Errorf("WaitThread threadId = %q", q.ThreadID)
				}
				close(entered)
				<-ctx.Done()
				close(finished)
				return api.ThreadWaitResult{}, ctx.Err()
			},
		}
		ts := startTestServer(t, service)
		client := startTestClient(t, ts)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errCh := make(chan error, 1)
		go func() {
			_, err := client.WaitThread(ctx, api.ThreadWaitQuery{ThreadID: "thread-wait", TimeoutMs: 600000})
			errCh <- err
		}()

		waitClosed(t, entered, "WaitThread entry on server")
		cancel()

		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("WaitThread cancellation error = %v, want context.Canceled", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("client WaitThread did not unblock on cancel")
		}
		waitClosed(t, finished, "server WaitThread context cancellation")
	})

	t.Run("DelegateTask mode=wait deadline propagates across socket", func(t *testing.T) {
		entered := make(chan struct{})
		finished := make(chan struct{})
		service := &orchestrationMock{
			fakeService: &fakeService{},
			delegateTaskFunc: func(ctx context.Context, req api.DelegateTaskRequest) (api.TaskDetail, error) {
				if req.Mode != "wait" {
					t.Errorf("DelegateTask mode = %q, want wait", req.Mode)
				}
				close(entered)
				<-ctx.Done()
				close(finished)
				return api.TaskDetail{}, ctx.Err()
			},
		}
		ts := startTestServer(t, service)
		client := startTestClient(t, ts)

		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		_, err := client.DelegateTask(ctx, api.DelegateTaskRequest{
			ParentRunID: "run-1",
			Task:        "Long running subagent",
			Mode:        "wait",
			TimeoutMs:   600000,
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("DelegateTask deadline error = %v, want context.DeadlineExceeded", err)
		}
		waitClosed(t, entered, "DelegateTask entry on server")
		waitClosed(t, finished, "server DelegateTask context cancellation")
	})

	t.Run("Long wait completes cleanly while concurrent calls share client", func(t *testing.T) {
		releaseWait := make(chan struct{})
		releaseDelegate := make(chan struct{})
		waitEntered := make(chan struct{})
		delegateEntered := make(chan struct{})

		service := &orchestrationMock{
			fakeService: &fakeService{},
			waitThreadFunc: func(ctx context.Context, q api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
				close(waitEntered)
				select {
				case <-releaseWait:
					return api.ThreadWaitResult{
						ThreadID: q.ThreadID,
						Link:     api.FormatThreadLink(q.ThreadID, "Settled"),
						Status:   string(api.RunCompleted),
						Summary:  "Done",
					}, nil
				case <-ctx.Done():
					return api.ThreadWaitResult{}, ctx.Err()
				}
			},
			delegateTaskFunc: func(ctx context.Context, req api.DelegateTaskRequest) (api.TaskDetail, error) {
				close(delegateEntered)
				select {
				case <-releaseDelegate:
					return api.TaskDetail{
						TaskID:         "task-waited",
						ParentThreadID: "thread-parent",
						ParentRunID:    req.ParentRunID,
						ChildThreadID:  "thread-child",
						Link:           api.FormatThreadLink("thread-child", "Child"),
						Title:          "Child",
						Status:         api.RunCompleted,
						WorkState:      "result_available",
						DeliveryStatus: api.DeliveryDelivered,
						Summary:        "Child finished",
					}, nil
				case <-ctx.Done():
					return api.TaskDetail{}, ctx.Err()
				}
			},
			listThreadsFunc: func(context.Context, api.ThreadListQuery) (api.ThreadListResult, error) {
				return api.ThreadListResult{Total: 2, Threads: []api.ThreadListItem{}}, nil
			},
		}
		ts := startTestServer(t, service)
		client := startTestClient(t, ts)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		var waitRes api.ThreadWaitResult
		var waitErr error
		var delRes api.TaskDetail
		var delErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			waitRes, waitErr = client.WaitThread(ctx, api.ThreadWaitQuery{ThreadID: "thread-1"})
		}()
		go func() {
			defer wg.Done()
			delRes, delErr = client.DelegateTask(ctx, api.DelegateTaskRequest{
				ParentRunID: "run-parent",
				Task:        "Wait for child",
				Mode:        "wait",
			})
		}()

		waitClosed(t, waitEntered, "WaitThread entered")
		waitClosed(t, delegateEntered, "DelegateTask entered")

		// Concurrent quick calls over the same *Client must not block behind long-waits.
		listed, err := client.ListThreads(ctx, api.ThreadListQuery{})
		if err != nil || listed.Total != 2 {
			t.Fatalf("concurrent ListThreads() = %#v, err = %v", listed, err)
		}

		close(releaseWait)
		close(releaseDelegate)
		wg.Wait()

		if waitErr != nil || waitRes.Status != string(api.RunCompleted) || waitRes.Summary != "Done" {
			t.Fatalf("WaitThread() = %#v, err = %v", waitRes, waitErr)
		}
		if delErr != nil || delRes.TaskID != "task-waited" || delRes.Summary != "Child finished" {
			t.Fatalf("DelegateTask() = %#v, err = %v", delRes, delErr)
		}
	})
}
