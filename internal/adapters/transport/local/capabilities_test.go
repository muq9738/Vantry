package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// statusFake adds the optional capability endpoints to the base fake service.
type statusFake struct {
	*fakeService
	status     api.ServerStatus
	statusErr  error
	drainCalls atomic.Int32
	drainErr   error
}

func (s *statusFake) ServerStatus(context.Context) (api.ServerStatus, error) {
	return s.status, s.statusErr
}

func (s *statusFake) RequestDrainShutdown(context.Context) error {
	s.drainCalls.Add(1)
	return s.drainErr
}

// readerFake serves the light projections itself and counts snapshot reads, which
// proves the transport does not fall back to a full snapshot.
type readerFake struct {
	*fakeService
	snapshotCalls atomic.Int32
	summaries     api.ThreadSummaries
}

func (s *readerFake) Snapshot(ctx context.Context) (api.Snapshot, error) {
	s.snapshotCalls.Add(1)
	return s.fakeService.Snapshot(ctx)
}

func (s *readerFake) ThreadSummaries(context.Context) (api.ThreadSummaries, error) {
	return s.summaries, nil
}

func (s *readerFake) ThreadDetail(_ context.Context, query api.ThreadDetailQuery) (api.ThreadDetail, error) {
	return api.ThreadDetail{Sequence: s.summaries.Sequence, RunCount: query.LimitRuns}, nil
}

func largeThreadSnapshot() api.Snapshot {
	longReply := strings.Repeat("这是一段很长的历史回复正文。", 2000)
	longReasoning := strings.Repeat("很长很长的思考过程。", 1000)
	return api.Snapshot{
		Sequence: 42,
		Threads: []api.Thread{
			{ID: "thread-a", Title: "并发改造", Model: "pi/sonnet", ProviderID: "pi"},
			{ID: "thread-b", Title: "归档会话"},
		},
		Runs: []api.Run{
			{ID: "run-1", ThreadID: "thread-a", Status: api.RunCompleted, Output: longReply, Reasoning: longReasoning},
			{ID: "run-2", ThreadID: "thread-a", Status: api.RunRunning, Output: longReply, Reasoning: longReasoning},
			{ID: "run-3", ThreadID: "thread-b", Status: api.RunFailed, Output: "短回复", Error: "失败"},
		},
		Inputs: []api.Input{
			{ID: "input-1", ThreadID: "thread-a", Status: "queued", Message: "排队中"},
			{ID: "input-2", ThreadID: "thread-a", Status: "delivered", Message: "已送达"},
			{ID: "input-3", ThreadID: "thread-b", Status: "pending", Message: "等待中"},
		},
		Tools: []api.ToolCall{
			{ID: "tool-1", ThreadID: "thread-a", RunID: "run-1", Name: "read", Output: longReply},
		},
		Requests: []api.ProviderRequest{
			{ID: "request-1", ThreadID: "thread-a", Status: "pending", Title: "确认"},
			{ID: "request-2", ThreadID: "thread-a", Status: "responded", Title: "已确认"},
		},
		Tasks: []api.Task{
			{ID: "task-1", ParentThreadID: "thread-a", ChildThreadID: "thread-b", Title: "子任务"},
		},
	}
}

func TestThreadSummariesTrimStoredReplyText(t *testing.T) {
	snapshot := largeThreadSnapshot()
	service := &fakeService{snapshotFunc: func(context.Context) (api.Snapshot, error) {
		return snapshot, nil
	}}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	summaries, err := client.ThreadSummaries(context.Background())
	if err != nil {
		t.Fatalf("ThreadSummaries() error = %v", err)
	}
	if summaries.Sequence != snapshot.Sequence {
		t.Fatalf("sequence = %d, want %d", summaries.Sequence, snapshot.Sequence)
	}
	if len(summaries.Threads) != 2 {
		t.Fatalf("threads = %+v", summaries.Threads)
	}
	first := summaries.Threads[0]
	if first.ID != "thread-a" || first.Title != "并发改造" || first.Model != "pi/sonnet" {
		t.Fatalf("summary metadata = %+v", first)
	}
	if first.RunCount != 2 || first.ActiveRuns != 1 {
		t.Fatalf("run counts = %+v", first)
	}
	if first.PendingInputs != 1 || first.PendingRequests != 1 {
		t.Fatalf("pending counts = %+v", first)
	}
	if first.LatestRunID != "run-2" || first.LatestStatus != api.RunRunning {
		t.Fatalf("latest run = %+v", first)
	}

	encoded, err := json.Marshal(summaries)
	if err != nil {
		t.Fatal(err)
	}
	full, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)*10 > len(full) {
		t.Fatalf("summary payload %d bytes is not materially smaller than snapshot %d bytes", len(encoded), len(full))
	}
	for _, forbidden := range []string{"output", "reasoning", strings.Repeat("这是一段很长的历史回复正文。", 2)} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("summary payload leaks %q", forbidden)
		}
	}
}

func TestThreadDetailScopesToOneThreadAndLimitsRuns(t *testing.T) {
	snapshot := largeThreadSnapshot()
	service := &fakeService{snapshotFunc: func(context.Context) (api.Snapshot, error) {
		return snapshot, nil
	}}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	detail, err := client.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: "thread-a", LimitRuns: 2})
	if err != nil {
		t.Fatalf("ThreadDetail() error = %v", err)
	}
	if detail.Thread.ID != "thread-a" {
		t.Fatalf("thread = %+v", detail.Thread)
	}
	if detail.RunCount != 2 || len(detail.Runs) != 2 || detail.TruncatedRuns {
		t.Fatalf("runs = %+v runCount=%d truncated=%t", detail.Runs, detail.RunCount, detail.TruncatedRuns)
	}
	for _, run := range detail.Runs {
		if run.ThreadID != "thread-a" {
			t.Fatalf("detail leaked run %+v", run)
		}
	}
	if len(detail.Inputs) != 2 || len(detail.Tools) != 1 || len(detail.Requests) != 2 {
		t.Fatalf("detail = %+v", detail)
	}
	if len(detail.Tasks) != 1 {
		t.Fatalf("tasks = %+v", detail.Tasks)
	}

	limited, err := client.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: "thread-a", LimitRuns: 1})
	if err != nil {
		t.Fatalf("ThreadDetail(limit=1) error = %v", err)
	}
	if limited.RunCount != 2 || len(limited.Runs) != 1 || !limited.TruncatedRuns {
		t.Fatalf("limited detail = %+v", limited)
	}
	if limited.Runs[0].ID != "run-2" {
		t.Fatalf("limit kept the wrong run: %+v", limited.Runs)
	}

	all, err := client.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: "thread-b"})
	if err != nil {
		t.Fatalf("ThreadDetail(thread-b) error = %v", err)
	}
	if len(all.Runs) != 1 || all.Runs[0].ID != "run-3" {
		t.Fatalf("other thread runs = %+v", all.Runs)
	}
}

func TestThreadDetailRejectsInvalidQueries(t *testing.T) {
	service := &fakeService{snapshotFunc: func(context.Context) (api.Snapshot, error) {
		return largeThreadSnapshot(), nil
	}}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	if _, err := client.ThreadDetail(context.Background(), api.ThreadDetailQuery{}); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("empty thread error = %v", err)
	}
	if _, err := client.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: "thread-a", LimitRuns: -1}); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("negative limit error = %v", err)
	}
	if _, err := client.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: "missing"}); api.CodeOf(err) != api.NotFound {
		t.Fatalf("unknown thread error = %v", err)
	}
}

func TestThreadReaderBackendSkipsSnapshotProjection(t *testing.T) {
	service := &readerFake{
		fakeService: &fakeService{},
		summaries: api.ThreadSummaries{
			Sequence: 7,
			Threads:  []api.ThreadSummary{{ID: "thread-a", Title: "直接投影"}},
		},
	}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	summaries, err := client.ThreadSummaries(context.Background())
	if err != nil {
		t.Fatalf("ThreadSummaries() error = %v", err)
	}
	if summaries.Sequence != 7 || summaries.Threads[0].Title != "直接投影" {
		t.Fatalf("summaries = %+v", summaries)
	}
	detail, err := client.ThreadDetail(context.Background(), api.ThreadDetailQuery{ThreadID: "thread-a", LimitRuns: 3})
	if err != nil {
		t.Fatalf("ThreadDetail() error = %v", err)
	}
	if detail.RunCount != 3 {
		t.Fatalf("detail = %+v", detail)
	}
	if calls := service.snapshotCalls.Load(); calls != 0 {
		t.Fatalf("transport fell back to %d snapshot reads", calls)
	}
}

func TestServerStatusRoundTrip(t *testing.T) {
	service := &statusFake{
		fakeService: &fakeService{},
		status: api.ServerStatus{
			ProtocolVersion: api.ProtocolVersion,
			State:           "ready",
			SchemaVersion:   api.StatusSchemaVersion,
			PID:             99,
			ActiveRuns:      2,
			Capabilities:    []string{api.CapabilityStatus, api.CapabilityDrainShutdown},
		},
	}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	status, err := client.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus() error = %v", err)
	}
	if status.PID != 99 || status.ActiveRuns != 2 || status.SchemaVersion != api.StatusSchemaVersion {
		t.Fatalf("status = %+v", status)
	}
	if !status.HasCapability(api.CapabilityDrainShutdown) {
		t.Fatalf("status = %+v", status)
	}
}

func TestLegacyCoreWithoutStatusIsReportedPrecisely(t *testing.T) {
	// A service that predates the capability contract has no status endpoint.
	service := &fakeService{}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	_, err := client.ServerStatus(context.Background())
	if !api.IsMissingCapabilityReport(err) {
		t.Fatalf("error = %v, want a typed NotFound", err)
	}
	if !strings.Contains(err.Error(), "旧版 Core") {
		t.Fatalf("error is not explanatory: %v", err)
	}
	if err := client.RequestDrainShutdown(context.Background()); !api.IsMissingCapabilityReport(err) {
		t.Fatalf("drain error = %v, want a typed NotFound", err)
	}
	if _, err := client.ThreadSummaries(context.Background()); err != nil {
		// The transport still serves the trimmed projection from a snapshot; only
		// capability reporting is missing from this Core.
		t.Fatalf("summaries error = %v", err)
	}
}

func TestShutdownIfIdleRefusesBusyCore(t *testing.T) {
	service := &statusFake{
		fakeService: &fakeService{},
		drainErr:    &api.Error{Code: api.Conflict, Message: "core 仍有 1 个活动任务"},
	}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	if err := client.RequestDrainShutdown(context.Background()); api.CodeOf(err) != api.Conflict {
		t.Fatalf("error = %v, want api.Conflict", err)
	}
	if got := service.drainCalls.Load(); got != 1 {
		t.Fatalf("drain requests = %d, want 1", got)
	}
	// A refused shutdown must leave the Core answering.
	if err := client.RequestDrainShutdown(context.Background()); api.CodeOf(err) != api.Conflict {
		t.Fatalf("second error = %v, want api.Conflict", err)
	}
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Core stopped despite refusal: %v", err)
	}
}

func TestShutdownIfIdleRetiresTheListener(t *testing.T) {
	service := &statusFake{fakeService: &fakeService{}}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)

	if err := client.RequestDrainShutdown(context.Background()); err != nil {
		t.Fatalf("RequestDrainShutdown() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Lstat(ts.socket); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket was not released after the drain acceptance")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := client.Health(context.Background()); err == nil {
		t.Fatal("retired Core still answers health")
	}
}
