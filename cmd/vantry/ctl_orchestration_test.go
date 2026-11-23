package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// brokerRecorder is an api.Service that also exposes Core's session broker, so
// the CLI's preferred path can be asserted instead of only its fallback.
type brokerRecorder struct {
	commandRecorder
	threadQueries []api.ThreadListQuery
	readQueries   []api.ThreadReadQuery
	sends         []api.InterThreadSendRequest
	waits         []api.ThreadWaitQuery
}

func (b *brokerRecorder) OrchestratorCapabilities(context.Context, api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
	return api.OrchestratorCapabilities{}, nil
}

func (b *brokerRecorder) DelegateTask(context.Context, api.DelegateTaskRequest) (api.TaskDetail, error) {
	return api.TaskDetail{}, nil
}

func (b *brokerRecorder) TaskStatus(context.Context, api.TaskStatusQuery) (api.TaskStatusResult, error) {
	return api.TaskStatusResult{}, nil
}

func (b *brokerRecorder) CancelTask(context.Context, api.CancelTaskRequest) (api.CancelTaskResult, error) {
	return api.CancelTaskResult{}, nil
}

func (b *brokerRecorder) ListThreads(_ context.Context, query api.ThreadListQuery) (api.ThreadListResult, error) {
	b.threadQueries = append(b.threadQueries, query)
	return api.ThreadListResult{Threads: []api.ThreadListItem{{ThreadID: "t1", Status: "running"}}, Total: 1}, nil
}

func (b *brokerRecorder) ReadThread(_ context.Context, query api.ThreadReadQuery) (api.ThreadReadResult, error) {
	b.readQueries = append(b.readQueries, query)
	return api.ThreadReadResult{
		Thread: api.ThreadListItem{ThreadID: query.ThreadID},
		Items:  []api.ThreadTimelineItem{{Position: 1, Type: "user_message", Text: "hi"}},
	}, nil
}

func (b *brokerRecorder) LaunchThread(context.Context, api.ThreadLaunchRequest) (api.LaunchedThread, error) {
	return api.LaunchedThread{}, nil
}

func (b *brokerRecorder) CreateThreads(context.Context, api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
	return api.CreateThreadsResult{}, nil
}

func (b *brokerRecorder) SendToThread(_ context.Context, request api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
	b.sends = append(b.sends, request)
	return api.InterThreadSendResult{ThreadID: request.TargetThreadID, Link: "link", Delivery: "queued"}, nil
}

func (b *brokerRecorder) WaitThread(_ context.Context, query api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
	b.waits = append(b.waits, query)
	return api.ThreadWaitResult{ThreadID: query.ThreadID, Status: "completed"}, nil
}

// snapshotService exposes only the base contract and a controllable
// subscription, which forces the CLI onto its snapshot and event fallbacks.
type snapshotService struct {
	commandRecorder
	snapshot   api.Snapshot
	events     chan api.Event
	failures   chan error
	after      int64
	subscribed chan struct{}
}

func (s *snapshotService) Snapshot(context.Context) (api.Snapshot, error) {
	return s.snapshot, nil
}

func (s *snapshotService) Subscribe(_ context.Context, after int64) (*api.Subscription, error) {
	s.after = after
	if s.subscribed != nil {
		close(s.subscribed)
	}
	return &api.Subscription{Events: s.events, Errors: s.failures, Close: func() {}}, nil
}

func TestDeleteCLIForcesByDefaultAndHonorsOptOut(t *testing.T) {
	recorder := &commandRecorder{}
	if err := control(context.Background(), recorder, []string{"delete", "--thread", "thread-1"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := control(context.Background(), recorder, []string{"delete", "--thread", "thread-1", "--force=false"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(recorder.commands) != 2 {
		t.Fatalf("delete dispatched %d commands", len(recorder.commands))
	}
	forced, guarded := recorder.commands[0], recorder.commands[1]
	if forced.Kind != api.DeleteThread || forced.ThreadID != "thread-1" || !forced.Force {
		t.Fatalf("delete must force by default: %+v", forced)
	}
	if guarded.Force {
		t.Fatalf("--force=false must keep Core's settlement guard: %+v", guarded)
	}
	missing := &commandRecorder{}
	if err := control(context.Background(), missing, []string{"delete"}, io.Discard, io.Discard); err == nil || len(missing.commands) != 0 {
		t.Fatal("delete without --thread reached Core")
	}
}

func TestCancelTaskCLIForwardsTaskThreadAndReason(t *testing.T) {
	recorder := &commandRecorder{}
	args := []string{"cancel-task", "--command-id", "cancel-once", "--task", "task-1", "--thread", "parent", "--reason", "陈旧派发"}
	for i := 0; i < 2; i++ {
		if err := control(context.Background(), recorder, args, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorder.commands) != 2 || !reflect.DeepEqual(recorder.commands[0], recorder.commands[1]) {
		t.Fatalf("cancel-task retry changed payload: %+v", recorder.commands)
	}
	command := recorder.commands[0]
	if command.Kind != api.CancelTask || command.TaskID != "task-1" || command.ThreadID != "parent" || command.Reason != "陈旧派发" {
		t.Fatalf("cancel-task lost its identity: %+v", command)
	}
	missing := &commandRecorder{}
	if err := control(context.Background(), missing, []string{"cancel-task", "--thread", "parent"}, io.Discard, io.Discard); err == nil || len(missing.commands) != 0 {
		t.Fatal("cancel-task without --task reached Core")
	}
}

func TestSnoozeCLIWritesDeadlineAndUnsnoozeClearsIt(t *testing.T) {
	recorder := &commandRecorder{}
	before := time.Now().UTC()
	if err := control(context.Background(), recorder, []string{"snooze", "--thread", "thread-1", "--duration", "2h"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := control(context.Background(), recorder, []string{"snooze", "--thread", "thread-1", "--until", "2026-03-04T05:06:07Z"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := control(context.Background(), recorder, []string{"unsnooze", "--thread", "thread-1"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(recorder.commands) != 3 {
		t.Fatalf("snooze commands: %+v", recorder.commands)
	}
	relative, absolute, cleared := recorder.commands[0], recorder.commands[1], recorder.commands[2]
	for _, command := range []api.Command{relative, absolute, cleared} {
		if command.Kind != api.UpdateThread || command.ThreadID != "thread-1" || command.SnoozedUntil == nil {
			t.Fatalf("snooze must update the thread deadline: %+v", command)
		}
	}
	if deadline := relative.SnoozedUntil.UTC(); deadline.Before(before.Add(119*time.Minute)) || deadline.After(time.Now().Add(121*time.Minute)) {
		t.Fatalf("--duration deadline drifted: %s", deadline)
	}
	if want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC); !absolute.SnoozedUntil.Equal(want) {
		t.Fatalf("--until deadline %s, want %s", absolute.SnoozedUntil, want)
	}
	if !cleared.SnoozedUntil.IsZero() {
		t.Fatalf("unsnooze must clear the deadline: %s", cleared.SnoozedUntil)
	}
	for _, args := range [][]string{
		{"snooze", "--thread", "thread-1"},
		{"snooze", "--thread", "thread-1", "--duration", "1h", "--until", "2026-03-04T05:06:07Z"},
		{"snooze", "--thread", "thread-1", "--until", "tomorrow"},
		{"snooze", "--duration", "1h"},
		{"unsnooze"},
	} {
		rejected := &commandRecorder{}
		if err := control(context.Background(), rejected, args, io.Discard, io.Discard); err == nil || len(rejected.commands) != 0 {
			t.Fatalf("snooze accepted %v", args)
		}
	}
}

func TestForkAndMergeBackCLIKeepThreadAndRunIdentity(t *testing.T) {
	recorder := &commandRecorder{}
	if err := control(context.Background(), recorder, []string{"fork", "--thread", "source", "--run", "run-2", "--title", "分支副本"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := control(context.Background(), recorder, []string{"merge-back", "--thread", "source", "--target", "target", "--run", "run-2"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(recorder.commands) != 2 {
		t.Fatalf("fork/merge-back commands: %+v", recorder.commands)
	}
	fork, merge := recorder.commands[0], recorder.commands[1]
	if fork.Kind != api.ForkThread || fork.ThreadID != "source" || fork.SourceRunID != "run-2" || fork.Title != "分支副本" {
		t.Fatalf("fork lost its source: %+v", fork)
	}
	if merge.Kind != api.MergeBackThread || merge.ThreadID != "source" || merge.TargetThreadID != "target" || merge.SourceRunID != "run-2" {
		t.Fatalf("merge-back lost either side: %+v", merge)
	}
	for _, args := range [][]string{
		{"fork"},
		{"merge-back", "--thread", "source"},
		{"merge-back", "--target", "target"},
	} {
		rejected := &commandRecorder{}
		if err := control(context.Background(), rejected, args, io.Discard, io.Discard); err == nil || len(rejected.commands) != 0 {
			t.Fatalf("accepted incomplete thread command %v", args)
		}
	}
}

func TestThreadSendCLIUsesBrokerThenDurableCommand(t *testing.T) {
	broker := &brokerRecorder{}
	if err := control(context.Background(), broker, []string{"thread-send", "--command-id", "send-once",
		"--thread", "target", "--from", "caller", "--message", "请继续检查", "--delivery-mode", "steer"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(broker.sends) != 1 || len(broker.commands) != 0 {
		t.Fatalf("broker send path: sends=%+v commands=%+v", broker.sends, broker.commands)
	}
	request := broker.sends[0]
	if request.TargetThreadID != "target" || request.CallerThreadID != "caller" || request.Message != "请继续检查" ||
		request.Mode != "steer" || request.ClientRequestID != "send-once" {
		t.Fatalf("broker request lost fields: %+v", request)
	}

	fallback := &commandRecorder{}
	if err := control(context.Background(), fallback, []string{"thread-send", "--thread", "target", "--message", "请继续检查"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(fallback.commands) != 1 {
		t.Fatalf("fallback send was not dispatched: %+v", fallback.commands)
	}
	command := fallback.commands[0]
	if command.Kind != api.InterThreadSend || command.ThreadID != "target" || command.TargetThreadID != "target" ||
		command.Message != "请继续检查" || command.SenderThreadID != "" || command.DeliveryMode != "" {
		t.Fatalf("fallback command lost fields: %+v", command)
	}
	for _, args := range [][]string{
		{"thread-send", "--thread", "target"},
		{"thread-send", "--message", "no target"},
		{"thread-send", "--thread", "target", "--message", "text", "--delivery-mode", "later"},
	} {
		rejected := &commandRecorder{}
		if err := control(context.Background(), rejected, args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted invalid thread-send %v", args)
		}
	}
}

func TestThreadReadCLIUsesBrokerThenSnapshot(t *testing.T) {
	broker := &brokerRecorder{}
	var output bytes.Buffer
	if err := control(context.Background(), broker, []string{"thread-read", "--thread", "t1",
		"--view", "activity", "--after-pos", "7", "--limit", "3"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(broker.readQueries) != 1 {
		t.Fatalf("broker read was not called: %+v", broker.readQueries)
	}
	query := broker.readQueries[0]
	if query.ThreadID != "t1" || query.View != "activity" || query.Limit != 3 || query.AfterPosition == nil || *query.AfterPosition != 7 {
		t.Fatalf("read query lost fields: %+v", query)
	}
	if !strings.Contains(output.String(), "user_message") {
		t.Fatalf("broker timeline was not printed: %s", output.String())
	}

	now := time.Now().UTC()
	service := &snapshotService{snapshot: api.Snapshot{
		Threads: []api.Thread{{ID: "t1", Title: "历史会话", CreatedAt: now}},
		Runs: []api.Run{
			{ID: "r1", ThreadID: "t1", Status: api.RunCompleted, CreatedAt: now, UpdatedAt: now.Add(time.Minute)},
			{ID: "r2", ThreadID: "t1", Status: api.RunRunning, Model: "pi/model", CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(3 * time.Minute)},
		},
	}}
	output.Reset()
	if err := control(context.Background(), service, []string{"thread-read", "--thread", "t1"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result api.ThreadReadResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode read result: %v (%s)", err, output.String())
	}
	if result.Thread.ThreadID != "t1" || result.Thread.Status != "running" {
		t.Fatalf("snapshot thread summary: %+v", result.Thread)
	}
	if len(result.RecentRuns) != 2 || result.RecentRuns[0].RunID != "r2" || result.RecentRuns[1].RunID != "r1" {
		t.Fatalf("snapshot runs must be newest first: %+v", result.RecentRuns)
	}
	missing := &snapshotService{snapshot: api.Snapshot{}}
	if err := control(context.Background(), missing, []string{"thread-read", "--thread", "absent"}, io.Discard, io.Discard); err == nil {
		t.Fatal("read of an unknown thread must fail")
	}
}

func TestThreadsCLIUsesBrokerThenSnapshot(t *testing.T) {
	broker := &brokerRecorder{}
	for _, test := range []struct {
		args []string
		want *bool
	}{
		{args: []string{"threads", "--limit", "2", "--settled"}, want: boolPointer(true)},
		{args: []string{"threads", "--settled=false"}, want: boolPointer(false)},
		{args: []string{"threads"}, want: nil},
	} {
		if err := control(context.Background(), broker, test.args, io.Discard, io.Discard); err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
	}
	if len(broker.threadQueries) != 3 {
		t.Fatalf("broker list calls: %+v", broker.threadQueries)
	}
	if query := broker.threadQueries[0]; query.Limit != 2 || query.Settled == nil || !*query.Settled {
		t.Fatalf("--limit/--settled not forwarded: %+v", query)
	}
	if query := broker.threadQueries[1]; query.Settled == nil || *query.Settled {
		t.Fatalf("--settled=false not forwarded: %+v", query)
	}
	if query := broker.threadQueries[2]; query.Settled != nil || query.Limit != 0 {
		t.Fatalf("absent filters must stay unset: %+v", query)
	}

	now := time.Now().UTC()
	service := &snapshotService{snapshot: api.Snapshot{
		Threads: []api.Thread{
			{ID: "live", Title: "进行中", CreatedAt: now},
			{ID: "old", Title: "已归拢", CreatedAt: now.Add(-time.Hour), Archived: true},
		},
		Runs: []api.Run{{ID: "r1", ThreadID: "live", Status: api.RunRunning, CreatedAt: now, UpdatedAt: now}},
	}}
	var output bytes.Buffer
	if err := control(context.Background(), service, []string{"threads"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var listed api.ThreadListResult
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatalf("decode thread list: %v (%s)", err, output.String())
	}
	if listed.Total != 2 || len(listed.Threads) != 2 {
		t.Fatalf("snapshot listing: %+v", listed)
	}
	for _, item := range listed.Threads {
		if item.Link == "" || item.ThreadID == "live" && item.Settled {
			t.Fatalf("snapshot item projected badly: %+v", item)
		}
	}
	output.Reset()
	if err := control(context.Background(), service, []string{"threads", "--settled"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Threads) != 1 || listed.Threads[0].ThreadID != "old" {
		t.Fatalf("--settled must keep only settled threads: %+v", listed.Threads)
	}
	output.Reset()
	if err := control(context.Background(), service, []string{"threads", "--limit", "1"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Threads) != 1 || listed.Total != 2 {
		t.Fatalf("--limit must bound the page and keep the total: %+v", listed)
	}
}

func TestThreadWaitCLIUsesBrokerThenSubscription(t *testing.T) {
	broker := &brokerRecorder{}
	if err := control(context.Background(), broker, []string{"thread-wait", "--thread", "t1", "--run", "r1", "--timeout", "90s"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(broker.waits) != 1 {
		t.Fatalf("broker wait was not called: %+v", broker.waits)
	}
	if query := broker.waits[0]; query.ThreadID != "t1" || query.RunID != "r1" || query.TimeoutMs != 90000 {
		t.Fatalf("wait query lost fields: %+v", query)
	}

	now := time.Now().UTC()
	service := &snapshotService{
		snapshot: api.Snapshot{
			Sequence: 10,
			Threads:  []api.Thread{{ID: "t1", Title: "运行中会话", CreatedAt: now}},
			Runs:     []api.Run{{ID: "r1", ThreadID: "t1", Status: api.RunRunning, CreatedAt: now, UpdatedAt: now}},
		},
		events:     make(chan api.Event, 2),
		failures:   make(chan error),
		subscribed: make(chan struct{}),
	}
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- control(context.Background(), service, []string{"thread-wait", "--thread", "t1", "--timeout", "5s"}, &output, io.Discard)
	}()
	select {
	case <-service.subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("thread-wait never subscribed to Core events")
	}
	if service.after != 10 {
		t.Fatalf("wait must resume after the snapshot cursor, got %d", service.after)
	}
	service.events <- api.Event{Sequence: 11, Type: "run.completed", ThreadID: "t1", RunID: "r1",
		Data: json.RawMessage(`{"run":{"id":"r1","status":"completed","output":"已完成"}}`)}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("thread-wait did not return after the run settled")
	}
	var result api.ThreadWaitResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode wait result: %v (%s)", err, output.String())
	}
	if result.Status != "completed" || result.TimedOut || result.Summary != "已完成" || result.Link == "" {
		t.Fatalf("wait result: %+v", result)
	}

	settled := &snapshotService{snapshot: api.Snapshot{
		Threads: []api.Thread{{ID: "t2", Title: "已结束", CreatedAt: now}},
		Runs:    []api.Run{{ID: "r9", ThreadID: "t2", Status: api.RunFailed, Error: "中断", CreatedAt: now, UpdatedAt: now}},
	}}
	output.Reset()
	if err := control(context.Background(), settled, []string{"thread-wait", "--thread", "t2"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.RunID != "r9" || result.Error != "中断" || result.TimedOut {
		t.Fatalf("settled thread must report its last run: %+v", result)
	}
}

func TestCreateCLIAcceptsEveryApprovalPolicy(t *testing.T) {
	for _, policy := range []string{"auto", "auto-accept-edits", "ask"} {
		recorder := &commandRecorder{}
		if err := control(context.Background(), recorder, []string{"create", "--title", "线程", "--approval", policy}, io.Discard, io.Discard); err != nil {
			t.Fatalf("approval %q rejected: %v", policy, err)
		}
		if len(recorder.commands) != 1 || recorder.commands[0].ApprovalPolicy != policy {
			t.Fatalf("approval %q lost: %+v", policy, recorder.commands)
		}
	}
	recorder := &commandRecorder{}
	if err := control(context.Background(), recorder, []string{"create", "--approval", "full-access"}, io.Discard, io.Discard); err == nil || len(recorder.commands) != 0 {
		t.Fatal("unknown approval policy reached Core")
	}
}

func boolPointer(value bool) *bool { return &value }
