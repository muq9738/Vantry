package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestInterThreadOrchestrationAndConversationE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	dataDir, err := os.MkdirTemp("/tmp", "vt-inter-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })

	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "README.md"), []byte("# Vantry E2E\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	coreCtx, stopCore := context.WithCancel(ctx)
	coreDone := make(chan error, 1)
	cfg := Config{
		DataDir:    dataDir,
		ProviderID: "controlled",
		Delay:      5 * time.Millisecond,
		Workspace:  &api.WorkspaceSpec{Kind: "directory", Path: wsDir},
	}
	go func() { coreDone <- RunCore(coreCtx, cfg) }()
	defer func() {
		stopCore()
		<-coreDone
	}()

	var client *local.Client
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := EnsureCore(ctx, cfg, true)
		if err == nil {
			client = c
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if client == nil {
		t.Fatal("EnsureCore attach timed out")
	}
	defer client.Close()

	// 1. Verify ServerStatus advertises CapabilityInterThreadOrchestration and DrainShutdown.
	status, err := client.ServerStatus(ctx)
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	for _, capName := range []string{
		api.CapabilityStatus,
		api.CapabilityThreadSummaries,
		api.CapabilityThreadDetail,
		api.CapabilityInterThreadOrchestration,
		api.CapabilityDrainShutdown,
	} {
		if !status.HasCapability(capName) {
			t.Fatalf("missing capability %q in %+v", capName, status)
		}
	}

	// 2. Launch coordinator session A and batch-create peer sessions B & C.
	coord, err := client.LaunchThread(ctx, api.ThreadLaunchRequest{
		ClientRequestID: "launch-coord",
		ThreadLaunchSpec: api.ThreadLaunchSpec{
			Title:   "主控会话 A",
			Message: "初始化主控上下文",
		},
	})
	if err != nil {
		t.Fatalf("LaunchThread: %v", err)
	}
	waitedA, err := client.WaitThread(ctx, api.ThreadWaitQuery{
		ThreadID:  coord.ThreadID,
		RunID:     coord.RunID,
		TimeoutMs: 5000,
	})
	if err != nil || waitedA.TimedOut || waitedA.Status != string(api.RunCompleted) {
		t.Fatalf("WaitThread A: res=%+v err=%v", waitedA, err)
	}

	peers, err := client.CreateThreads(ctx, api.CreateThreadsRequest{
		ClientRequestID: "batch-peers",
		CallerThreadID:  coord.ThreadID,
		CallerRunID:     coord.RunID,
		Threads: []api.ThreadLaunchSpec{
			{Title: "同伴会话 B"},
			{Title: "同伴会话 C", Message: "准备并行子模块"},
		},
	})
	if err != nil || len(peers.Threads) != 2 {
		t.Fatalf("CreateThreads: res=%+v err=%v", peers, err)
	}
	peerB := peers.Threads[0].ThreadID
	peerC := peers.Threads[1].ThreadID

	// 3. Session A converses directly with Session B via SendToThread (inter-session conversation).
	sent, err := client.SendToThread(ctx, api.InterThreadSendRequest{
		ClientRequestID: "a-to-b-1",
		CallerThreadID:  coord.ThreadID,
		TargetThreadID:  peerB,
		Message:         "你好同伴 B，请核对接口契约并回复结论",
		Mode:            "auto",
	})
	if err != nil {
		t.Fatalf("SendToThread A->B: %v", err)
	}
	if sent.Delivery != "started" || sent.RunID == "" {
		t.Fatalf("unexpected SendToThread delivery: %+v", sent)
	}

	waitedB, err := client.WaitThread(ctx, api.ThreadWaitQuery{
		CallerThreadID: coord.ThreadID,
		ThreadID:       peerB,
		RunID:          sent.RunID,
		TimeoutMs:      5000,
	})
	if err != nil || waitedB.TimedOut || waitedB.Status != string(api.RunCompleted) {
		t.Fatalf("WaitThread B: res=%+v err=%v", waitedB, err)
	}

	// 4. Read Session B's timeline from Session A and verify SenderThreadID provenance & Link.
	readB, err := client.ReadThread(ctx, api.ThreadReadQuery{
		CallerThreadID: coord.ThreadID,
		ThreadID:       peerB,
		View:           "messages",
		Limit:          10,
	})
	if err != nil {
		t.Fatalf("ReadThread B: %v", err)
	}
	if !strings.Contains(readB.Thread.Link, "vantry-thread://"+peerB) {
		t.Fatalf("unexpected thread link: %q", readB.Thread.Link)
	}
	foundSender := false
	for _, item := range readB.Items {
		if item.Type == "user_message" && item.SenderThreadID == coord.ThreadID {
			foundSender = true
		}
	}
	if !foundSender {
		t.Fatalf("expected SenderThreadID=%q in ReadThread items: %+v", coord.ThreadID, readB.Items)
	}

	// 5. Search across threads via /v1/orchestration/threads/search.
	hits, err := client.Search(ctx, api.SearchQuery{Query: "核对接口契约", Limit: 10})
	if err != nil || len(hits) == 0 {
		t.Fatalf("Search failed: hits=%+v err=%v", hits, err)
	}

	// 6. DelegateTask with mode="wait" from Session A's active run.
	// Start an active run on A with slightly longer controlled delay or use B's settled state:
	// First verify Snooze / Unsnooze and single-step Force Delete on Peer C.
	_, _ = client.WaitThread(ctx, api.ThreadWaitQuery{ThreadID: peerC, TimeoutMs: 5000})
	snoozeAt := time.Now().UTC().Add(2 * time.Hour)
	if _, err := client.Dispatch(ctx, api.Command{
		Version:      api.ProtocolVersion,
		CommandID:    "snooze-c",
		Kind:         api.UpdateThread,
		ThreadID:     peerC,
		SnoozedUntil: &snoozeAt,
	}); err != nil {
		t.Fatalf("Snooze C: %v", err)
	}
	snoozedTrue := true
	listedSnoozed, err := client.ListThreads(ctx, api.ThreadListQuery{
		AllProjects: true,
		Snoozed:     &snoozedTrue,
	})
	if err != nil || listedSnoozed.Total != 1 || listedSnoozed.Threads[0].ThreadID != peerC {
		t.Fatalf("ListThreads snoozed: %+v err=%v", listedSnoozed, err)
	}

	// Wake C with zero time, then single-step Force delete C.
	wakeTime := time.Time{}
	if _, err := client.Dispatch(ctx, api.Command{
		Version:      api.ProtocolVersion,
		CommandID:    "wake-c",
		Kind:         api.UpdateThread,
		ThreadID:     peerC,
		SnoozedUntil: &wakeTime,
	}); err != nil {
		t.Fatalf("Wake C: %v", err)
	}
	if _, err := client.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "force-del-c",
		Kind:      api.DeleteThread,
		ThreadID:  peerC,
		Force:     true,
	}); err != nil {
		t.Fatalf("Force delete C: %v", err)
	}
	allList, err := client.ListThreads(ctx, api.ThreadListQuery{AllProjects: true})
	if err != nil {
		t.Fatalf("ListThreads all: %v", err)
	}
	for _, th := range allList.Threads {
		if th.ThreadID == peerC {
			t.Fatalf("deleted thread %q still returned in ListThreads", peerC)
		}
	}
}
