package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/provider/controlled"
	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/core"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type peerNode struct {
	dir        string
	socketPath string
	core       *core.Core
	server     *local.Server
	serveDone  chan error
	cancel     context.CancelFunc
	closeOnce  sync.Once
}

func startPeerNode(t *testing.T, prov provider.Provider) *peerNode {
	t.Helper()

	// Keep Unix domain socket paths well below Darwin's 104-byte sun_path limit
	// and inside a mode-0700 directory owned by the current user.
	dir, err := os.MkdirTemp("/tmp", "vantry-fed-")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}

	ctx := context.Background()
	c, err := core.Open(ctx, core.Options{
		DBPath:            filepath.Join(dir, "core.db"),
		Provider:          prov,
		DefaultProviderID: "controlled",
		DefaultModel:      "controlled-v1",
		DefaultWorkspace: &api.WorkspaceSpec{
			Kind: "local",
			Path: dir,
		},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("core.Open() error = %v", err)
	}

	socketPath := filepath.Join(dir, "core.sock")
	srv, err := local.Listen(socketPath, c)
	if err != nil {
		_ = c.Close()
		_ = os.RemoveAll(dir)
		t.Fatalf("local.Listen() error = %v", err)
	}

	serveCtx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(serveCtx)
	}()

	node := &peerNode{
		dir:        dir,
		socketPath: socketPath,
		core:       c,
		server:     srv,
		serveDone:  serveDone,
		cancel:     cancel,
	}

	t.Cleanup(func() {
		node.close(t)
		_ = os.RemoveAll(dir)
	})
	return node
}

func (n *peerNode) close(t *testing.T) {
	t.Helper()
	n.closeOnce.Do(func() {
		n.cancel()
		_ = n.server.Close(context.Background())
		select {
		case err := <-n.serveDone:
			if err != nil {
				t.Errorf("server.Serve() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("timed out waiting for peer server to stop")
		}
		if err := n.core.Close(); err != nil {
			t.Errorf("core.Close() error = %v", err)
		}
	})
}

// trackingClient wraps a PeerClient to observe lifecycle and stream cursors in tests.
type trackingClient struct {
	PeerClient
	closed        atomic.Bool
	onDispatch    func(context.Context, api.Command) (api.Receipt, error)
	onSubscribe   func(context.Context, int64) (*api.Subscription, error)
	closeCallback func()
}

func (c *trackingClient) Dispatch(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if c.onDispatch != nil {
		return c.onDispatch(ctx, cmd)
	}
	return c.PeerClient.Dispatch(ctx, cmd)
}

func (c *trackingClient) Subscribe(ctx context.Context, after int64) (*api.Subscription, error) {
	if c.onSubscribe != nil {
		return c.onSubscribe(ctx, after)
	}
	return c.PeerClient.Subscribe(ctx, after)
}

func (c *trackingClient) Close() error {
	c.closed.Store(true)
	if c.closeCallback != nil {
		c.closeCallback()
	}
	return c.PeerClient.Close()
}

func TestFederationDelegateAndStreamWaitEndToEnd(t *testing.T) {
	// Spin up two real local Core + UDS instances: a local host Core and a
	// simulated remote GPU builder Core.
	localHostNode := startPeerNode(t, controlled.Provider{Delay: 4 * time.Millisecond})
	remoteGPUNode := startPeerNode(t, controlled.Provider{Delay: 6 * time.Millisecond})

	var dialCount atomic.Int32
	var createdClients []*trackingClient
	var clientsMu sync.Mutex

	fed, err := NewFederator(FederatorOptions{
		DataDir:           t.TempDir(),
		ReconnectBackoff:  10 * time.Millisecond,
		ReconcileInterval: 100 * time.Millisecond,
		Dial: func(ctx context.Context, target PeerTarget) (PeerClient, error) {
			dialCount.Add(1)
			// Simulate SSH-forwarded remote target by dialing its UDS endpoint directly.
			raw, err := DefaultDial(ctx, PeerTarget{
				EnvironmentID: target.EnvironmentID,
				Name:          target.Name,
				SocketPath:    target.SocketPath,
			})
			if err != nil {
				return nil, err
			}
			tc := &trackingClient{PeerClient: raw}
			clientsMu.Lock()
			createdClients = append(createdClients, tc)
			clientsMu.Unlock()
			return tc, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFederator() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create a parent thread on the local Core to mirror a real local session
	// delegating sub-work to a remote peer Core.
	localClient := local.Dial(localHostNode.socketPath)
	t.Cleanup(func() { _ = localClient.Close() })
	if _, err := localClient.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "local-parent-create",
		Kind:      api.CreateThread,
		ThreadID:  "local-parent-thread",
		Title:     "Local Orchestrator Session",
	}); err != nil {
		t.Fatalf("local CreateThread error = %v", err)
	}

	remoteTarget := PeerTarget{
		EnvironmentID: "env-linux-gpu",
		Name:          "Linux GPU Builder",
		Host:          "builder@gpu-box.internal",
		SocketPath:    remoteGPUNode.socketPath,
	}

	prompt := "请在远端 GPU 机器执行 CUDA 算子构建与回归测试，并汇总日志结果。"
	wantWorkspace := &api.WorkspaceSpec{
		Kind: "local",
		Path: remoteGPUNode.dir,
	}
	handle, err := fed.DelegateRemote(ctx, remoteTarget, RemoteTaskSpec{
		LocalTaskID:    "local-task-1",
		RemoteTaskID:   "remote-task-1",
		CommandID:      "cmd-delegate-1",
		Title:          "CUDA Build Subtask",
		Prompt:         prompt,
		Workspace:      wantWorkspace,
		ProviderID:     "controlled",
		Model:          "controlled-gpu",
		ApprovalPolicy: "auto",
	})
	if err != nil {
		t.Fatalf("DelegateRemote() error = %v", err)
	}
	if handle.RemoteThreadID == "" || handle.RemoteRunID == "" {
		t.Fatalf("DelegateRemote() returned incomplete handle: %+v", handle)
	}

	var streamedChunks []string
	result, err := fed.WatchRemote(ctx, remoteTarget, handle, func(delta RemoteStreamDelta) {
		if delta.OutputDelta != "" {
			streamedChunks = append(streamedChunks, delta.OutputDelta)
		}
	})
	if err != nil {
		t.Fatalf("WatchRemote() error = %v", err)
	}

	wantOutput := fmt.Sprintf("这是 Vantry 的本机受控 Provider。\n\n已收到：%s\n\n任务已完成。", prompt)
	if result.Status != api.RunCompleted {
		t.Fatalf("result.Status = %q, want %q", result.Status, api.RunCompleted)
	}
	if result.Output != wantOutput {
		t.Fatalf("result.Output = %q, want %q", result.Output, wantOutput)
	}
	if result.RemoteThreadID != handle.RemoteThreadID || result.RemoteRunID != handle.RemoteRunID {
		t.Fatalf("result IDs = (%q, %q), want (%q, %q)",
			result.RemoteThreadID, result.RemoteRunID, handle.RemoteThreadID, handle.RemoteRunID)
	}
	if len(streamedChunks) < 2 {
		t.Fatalf("expected multi-chunk streaming deltas, got %d chunk(s): %v", len(streamedChunks), streamedChunks)
	}
	if strings.Join(streamedChunks, "") != wantOutput {
		t.Fatalf("concatenated chunks = %q, want %q", strings.Join(streamedChunks, ""), wantOutput)
	}

	// Verify the remote Core recorded the requested thread settings.
	snap, err := remoteGPUNode.core.Snapshot(ctx)
	if err != nil {
		t.Fatalf("remote Snapshot() error = %v", err)
	}
	if len(snap.Threads) != 1 {
		t.Fatalf("remote thread count = %d, want 1", len(snap.Threads))
	}
	gotThread := snap.Threads[0]
	if gotThread.ProviderID != "controlled" || gotThread.Model != "controlled-gpu" || gotThread.ApprovalPolicy != "auto" {
		t.Fatalf("remote thread = %+v", gotThread)
	}
	if gotThread.Workspace == nil || gotThread.Workspace.Path != wantWorkspace.Path {
		t.Fatalf("remote thread workspace = %+v, want %+v", gotThread.Workspace, wantWorkspace)
	}

	// Dispatch a second task to the same peer target and verify connection pooling reuses the live client.
	handle2, err := fed.DelegateRemote(ctx, remoteTarget, RemoteTaskSpec{
		LocalTaskID: "local-task-2",
		CommandID:   "cmd-delegate-2",
		Prompt:      "二次构建校验",
	})
	if err != nil {
		t.Fatalf("second DelegateRemote() error = %v", err)
	}
	res2, err := fed.WaitRemote(ctx, remoteTarget, handle2)
	if err != nil {
		t.Fatalf("second WaitRemote() error = %v", err)
	}
	if res2.Status != api.RunCompleted {
		t.Fatalf("second result.Status = %q, want %q", res2.Status, api.RunCompleted)
	}
	if gotDials := dialCount.Load(); gotDials != 1 {
		t.Fatalf("dialCount = %d, want 1 (healthy pooled connection should be reused)", gotDials)
	}

	// Closing the Federator must cleanly release all pooled clients.
	if err := fed.Close(); err != nil {
		t.Fatalf("fed.Close() error = %v", err)
	}
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for i, c := range createdClients {
		if !c.closed.Load() {
			t.Fatalf("pooled client %d was not closed on Federator.Close()", i)
		}
	}
}

func TestFederationIdempotentRetryDoesNotDuplicateRemoteRun(t *testing.T) {
	remoteNode := startPeerNode(t, controlled.Provider{Delay: 5 * time.Millisecond})
	target := PeerTarget{
		EnvironmentID: "env-mac-mini",
		Name:          "Mac Mini Peer",
		SocketPath:    remoteNode.socketPath,
	}

	// Simulate a transient network drop right AFTER the remote Core has already
	// committed message.send (the worst-case at-least-once failure window).
	var dropAfterFirstSend atomic.Bool
	dropAfterFirstSend.Store(true)

	fed, err := NewFederator(FederatorOptions{
		DataDir:          t.TempDir(),
		ReconnectBackoff: 5 * time.Millisecond,
		Dial: func(ctx context.Context, target PeerTarget) (PeerClient, error) {
			raw, err := DefaultDial(ctx, target)
			if err != nil {
				return nil, err
			}
			var tc *trackingClient
			tc = &trackingClient{
				PeerClient: raw,
				onDispatch: func(dCtx context.Context, cmd api.Command) (api.Receipt, error) {
					receipt, err := raw.Dispatch(dCtx, cmd)
					if err == nil && cmd.Kind == api.SendMessage && dropAfterFirstSend.CompareAndSwap(true, false) {
						_ = raw.Close()
						return api.Receipt{}, errors.New("simulated network reset after remote commit")
					}
					return receipt, err
				},
			}
			return tc, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFederator() error = %v", err)
	}
	defer fed.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	spec := RemoteTaskSpec{
		LocalTaskID:    "task-idempotent-1",
		RemoteTaskID:   "rtask-idempotent-1",
		CommandID:      "cmd-idempotent-base-1",
		Title:          "Idempotent Task",
		Prompt:         "执行一次且仅一次的任务",
		ProviderID:     "controlled",
		Model:          "controlled-v1",
		ApprovalPolicy: "auto",
	}

	// First call automatically recovers from the dropped send response via its
	// deterministic CommandIDs.
	handle1, err := fed.DelegateRemote(ctx, target, spec)
	if err != nil {
		t.Fatalf("first DelegateRemote() error = %v", err)
	}

	// Explicit caller retry with the same spec on the same Federator.
	handle2, err := fed.DelegateRemote(ctx, target, spec)
	if err != nil {
		t.Fatalf("second DelegateRemote() error = %v", err)
	}

	// Caller retry from a brand-new stateless Federator (simulating caller retry
	// without local memory).
	fedFresh, err := NewFederator(FederatorOptions{})
	if err != nil {
		t.Fatalf("NewFederator(fresh) error = %v", err)
	}
	defer fedFresh.Close()

	handle3, err := fedFresh.DelegateRemote(ctx, target, spec)
	if err != nil {
		t.Fatalf("fresh DelegateRemote() error = %v", err)
	}

	if handle1.RemoteThreadID != handle2.RemoteThreadID || handle1.RemoteThreadID != handle3.RemoteThreadID {
		t.Fatalf("thread IDs diverged across retries: %q vs %q vs %q",
			handle1.RemoteThreadID, handle2.RemoteThreadID, handle3.RemoteThreadID)
	}
	if handle1.RemoteRunID != handle2.RemoteRunID || handle1.RemoteRunID != handle3.RemoteRunID {
		t.Fatalf("run IDs diverged across retries: %q vs %q vs %q",
			handle1.RemoteRunID, handle2.RemoteRunID, handle3.RemoteRunID)
	}

	res, err := fed.WaitRemote(ctx, target, handle1)
	if err != nil {
		t.Fatalf("WaitRemote() error = %v", err)
	}
	if res.Status != api.RunCompleted {
		t.Fatalf("res.Status = %q, want %q", res.Status, api.RunCompleted)
	}

	snap, err := remoteNode.core.Snapshot(ctx)
	if err != nil {
		t.Fatalf("remote Snapshot() error = %v", err)
	}
	if len(snap.Threads) != 1 {
		t.Fatalf("remote Threads = %d, want exactly 1", len(snap.Threads))
	}
	if len(snap.Runs) != 1 {
		t.Fatalf("remote Runs = %d, want exactly 1", len(snap.Runs))
	}

	// Reusing the same LocalTaskID with a different CommandID must be rejected as Conflict.
	_, conflictErr := fed.DelegateRemote(ctx, target, RemoteTaskSpec{
		LocalTaskID: spec.LocalTaskID,
		CommandID:   "different-command-base",
		Prompt:      spec.Prompt,
	})
	if api.CodeOf(conflictErr) != api.Conflict {
		t.Fatalf("conflicting DelegateRemote() error = %v, want api.Conflict", conflictErr)
	}
}

// phasedProvider emits deterministic chunks and synchronizes with a mid-stream
// disconnect so tests can verify cursor-based replay deterministically.
type phasedProvider struct {
	firstChunkEmitted chan struct{}
	allowRemaining    chan struct{}
	once              sync.Once
}

func (p *phasedProvider) Run(ctx context.Context, req provider.Request, emit func(string) error) error {
	chunks := []string{
		"chunk-1[before-drop]|",
		"chunk-2[before-drop]|",
		"chunk-3[while-disconnected]|",
		"chunk-4[after-reconnect]|",
		"done:" + req.Message,
	}
	for i, ch := range chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(ch); err != nil {
			return err
		}
		if i == 1 {
			p.once.Do(func() { close(p.firstChunkEmitted) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.allowRemaining:
			}
		}
	}
	return nil
}

func TestFederationReconnectsSubscriptionFromLatestSequenceCursor(t *testing.T) {
	prov := &phasedProvider{
		firstChunkEmitted: make(chan struct{}),
		allowRemaining:    make(chan struct{}),
	}
	remoteNode := startPeerNode(t, prov)
	target := PeerTarget{
		EnvironmentID: "env-reconnect",
		Name:          "Reconnect Peer",
		SocketPath:    remoteNode.socketPath,
	}

	var (
		mu               sync.Mutex
		subscribeCursors []int64
		activeClients    []*trackingClient
	)

	fed, err := NewFederator(FederatorOptions{
		DataDir:           t.TempDir(),
		ReconnectBackoff:  15 * time.Millisecond,
		ReconcileInterval: 200 * time.Millisecond,
		Dial: func(ctx context.Context, target PeerTarget) (PeerClient, error) {
			raw, err := DefaultDial(ctx, target)
			if err != nil {
				return nil, err
			}
			tc := &trackingClient{
				PeerClient: raw,
				onSubscribe: func(sCtx context.Context, after int64) (*api.Subscription, error) {
					mu.Lock()
					subscribeCursors = append(subscribeCursors, after)
					mu.Unlock()
					return raw.Subscribe(sCtx, after)
				},
			}
			mu.Lock()
			activeClients = append(activeClients, tc)
			mu.Unlock()
			return tc, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFederator() error = %v", err)
	}
	defer fed.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle, err := fed.DelegateRemote(ctx, target, RemoteTaskSpec{
		LocalTaskID: "task-reconnect-1",
		CommandID:   "cmd-reconnect-1",
		Prompt:      "resumable-stream",
	})
	if err != nil {
		t.Fatalf("DelegateRemote() error = %v", err)
	}

	seenSecondChunk := make(chan struct{})
	var chunkCount atomic.Int32

	type waitOutcome struct {
		res RemoteTaskResult
		err error
	}
	doneCh := make(chan waitOutcome, 1)
	go func() {
		res, wErr := fed.WatchRemote(ctx, target, handle, func(d RemoteStreamDelta) {
			if d.OutputDelta != "" {
				if chunkCount.Add(1) == 2 {
					close(seenSecondChunk)
				}
			}
		})
		doneCh <- waitOutcome{res: res, err: wErr}
	}()

	// Wait until the remote provider has emitted the first two chunks AND the
	// subscriber has received them so its Sequence cursor has advanced.
	select {
	case <-prov.firstChunkEmitted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for first chunks from provider")
	}
	select {
	case <-seenSecondChunk:
	case <-ctx.Done():
		t.Fatal("timed out waiting for WatchRemote to receive second chunk")
	}

	// Forcefully sever the active client transport mid-subscription, then let the
	// remote provider emit the remaining chunks while the client is reconnecting.
	mu.Lock()
	if len(activeClients) == 0 {
		mu.Unlock()
		t.Fatal("expected an active client")
	}
	firstClient := activeClients[0]
	mu.Unlock()

	_ = firstClient.Close()
	close(prov.allowRemaining)

	var outcome waitOutcome
	select {
	case outcome = <-doneCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for WatchRemote to complete after reconnect")
	}
	if outcome.err != nil {
		t.Fatalf("WatchRemote() error = %v", outcome.err)
	}

	wantOutput := "chunk-1[before-drop]|chunk-2[before-drop]|chunk-3[while-disconnected]|chunk-4[after-reconnect]|done:resumable-stream"
	if outcome.res.Status != api.RunCompleted {
		t.Fatalf("result.Status = %q, want %q", outcome.res.Status, api.RunCompleted)
	}
	if outcome.res.Output != wantOutput {
		t.Fatalf("result.Output = %q, want %q", outcome.res.Output, wantOutput)
	}

	mu.Lock()
	cursors := append([]int64(nil), subscribeCursors...)
	totalClients := len(activeClients)
	mu.Unlock()

	if totalClients < 2 {
		t.Fatalf("expected redial after disconnect, got %d client(s)", totalClients)
	}
	if len(cursors) < 2 {
		t.Fatalf("expected at least 2 Subscribe calls, got %v", cursors)
	}
	if cursors[1] <= cursors[0] {
		t.Fatalf("expected reconnect Subscribe cursor (%d) > initial cursor (%d); all cursors=%v",
			cursors[1], cursors[0], cursors)
	}
}

func TestFederationCancelRemoteStopsRunningTask(t *testing.T) {
	// Use controlled.Provider with a slow per-chunk delay and a long prompt so
	// the remote run stays active until CancelRemote terminates it.
	remoteNode := startPeerNode(t, controlled.Provider{Delay: 120 * time.Millisecond})
	target := PeerTarget{
		EnvironmentID: "env-cancel",
		Name:          "Cancelable Peer",
		SocketPath:    remoteNode.socketPath,
	}

	fed, err := NewFederator(FederatorOptions{
		DataDir:          t.TempDir(),
		ReconnectBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewFederator() error = %v", err)
	}
	defer fed.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	handle, err := fed.DelegateRemote(ctx, target, RemoteTaskSpec{
		LocalTaskID: "task-cancel-1",
		CommandID:   "cmd-cancel-1",
		Prompt:      strings.Repeat("长时间运行的远端编译任务内容，用于测试远程取消能力。", 20),
	})
	if err != nil {
		t.Fatalf("DelegateRemote() error = %v", err)
	}

	started := make(chan struct{})
	var startOnce sync.Once
	waitDone := make(chan struct {
		res RemoteTaskResult
		err error
	}, 1)

	go func() {
		res, wErr := fed.WatchRemote(ctx, target, handle, func(d RemoteStreamDelta) {
			if d.Status == api.RunRunning || d.OutputDelta != "" {
				startOnce.Do(func() { close(started) })
			}
		})
		waitDone <- struct {
			res RemoteTaskResult
			err error
		}{res: res, err: wErr}
	}()

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("timed out waiting for remote run to start")
	}

	// CancelRemote must stop the running remote run, and a repeated CancelRemote
	// call must succeed idempotently.
	if err := fed.CancelRemote(ctx, target, handle); err != nil {
		t.Fatalf("first CancelRemote() error = %v", err)
	}
	if err := fed.CancelRemote(ctx, target, handle); err != nil {
		t.Fatalf("idempotent second CancelRemote() error = %v", err)
	}

	select {
	case out := <-waitDone:
		if out.err != nil {
			t.Fatalf("WatchRemote() error = %v", out.err)
		}
		if out.res.Status != api.RunCancelled {
			t.Fatalf("result.Status = %q, want %q", out.res.Status, api.RunCancelled)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for cancelled run to finish WatchRemote")
	}

	snap, err := remoteNode.core.Snapshot(ctx)
	if err != nil {
		t.Fatalf("remote Snapshot() error = %v", err)
	}
	run, ok := findRun(snap, handle.RemoteRunID)
	if !ok {
		t.Fatalf("remote run %q not found in snapshot", handle.RemoteRunID)
	}
	if run.Status != api.RunCancelled {
		t.Fatalf("remote run status = %q, want %q", run.Status, api.RunCancelled)
	}
}

func TestFederationRestoresTasksFromPersistentLedgerAfterRestart(t *testing.T) {
	// Two remote nodes running controlled.Provider at a steady pace so tasks are
	// in-flight while the local Federator restarts.
	remoteNodeA := startPeerNode(t, controlled.Provider{Delay: 25 * time.Millisecond})
	remoteNodeB := startPeerNode(t, controlled.Provider{Delay: 120 * time.Millisecond})

	targetA := PeerTarget{
		EnvironmentID: "env-peer-a",
		Name:          "Peer A",
		SocketPath:    remoteNodeA.socketPath,
	}
	targetB := PeerTarget{
		EnvironmentID: "env-peer-b",
		Name:          "Peer B",
		SocketPath:    remoteNodeB.socketPath,
	}

	dataDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	// Phase 1: Delegate two tasks from the initial Federator instance, then shut
	// it down to simulate a local process restart.
	fed1, err := NewFederator(FederatorOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewFederator(1) error = %v", err)
	}

	promptA := "重启后需要继续等待拿回完整输出的远端任务"
	origA, err := fed1.DelegateRemote(ctx, targetA, RemoteTaskSpec{
		LocalTaskID: "persisted-wait-task",
		CommandID:   "cmd-persisted-a",
		Prompt:      promptA,
	})
	if err != nil {
		t.Fatalf("DelegateRemote(A) error = %v", err)
	}

	origB, err := fed1.DelegateRemote(ctx, targetB, RemoteTaskSpec{
		LocalTaskID: "persisted-cancel-task",
		CommandID:   "cmd-persisted-b",
		Prompt:      strings.Repeat("重启后需要执行远程取消的慢速任务。", 20),
	})
	if err != nil {
		t.Fatalf("DelegateRemote(B) error = %v", err)
	}

	ledgerPath := filepath.Join(dataDir, "remote-tasks.json")
	info, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", ledgerPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("remote-tasks.json permissions = %04o, want 0600", perm)
	}

	if err := fed1.Close(); err != nil {
		t.Fatalf("fed1.Close() error = %v", err)
	}

	// Phase 2: Open a brand-new Federator pointing at the same DataDir and recover
	// both tasks solely from remote-tasks.json.
	fed2, err := NewFederator(FederatorOptions{
		DataDir:          dataDir,
		ReconnectBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewFederator(2) error = %v", err)
	}
	defer fed2.Close()

	active := fed2.ActiveTasks()
	if len(active) != 2 {
		t.Fatalf("ActiveTasks() after restart = %d, want 2", len(active))
	}

	restoredA, ok := fed2.GetTask("persisted-wait-task")
	if !ok {
		t.Fatal("persisted-wait-task missing after restart")
	}
	if restoredA.RemoteThreadID != origA.RemoteThreadID || restoredA.RemoteRunID != origA.RemoteRunID {
		t.Fatalf("restoredA IDs = (%q, %q), want (%q, %q)",
			restoredA.RemoteThreadID, restoredA.RemoteRunID, origA.RemoteThreadID, origA.RemoteRunID)
	}
	if restoredA.Target.EnvironmentID != targetA.EnvironmentID || restoredA.Target.SocketPath != targetA.SocketPath {
		t.Fatalf("restoredA.Target = %+v, want %+v", restoredA.Target, targetA)
	}

	restoredB, ok := fed2.GetTask("persisted-cancel-task")
	if !ok {
		t.Fatal("persisted-cancel-task missing after restart")
	}
	if restoredB.RemoteThreadID != origB.RemoteThreadID || restoredB.RemoteRunID != origB.RemoteRunID {
		t.Fatalf("restoredB IDs = (%q, %q), want (%q, %q)",
			restoredB.RemoteThreadID, restoredB.RemoteRunID, origB.RemoteThreadID, origB.RemoteRunID)
	}

	// Resume waiting on Task A using the restored handle (even passing empty PeerTarget
	// verifies target resolution from the persisted ledger).
	resA, err := fed2.WaitRemote(ctx, PeerTarget{}, restoredA)
	if err != nil {
		t.Fatalf("WaitRemote(restoredA) error = %v", err)
	}
	wantOutA := fmt.Sprintf("这是 Vantry 的本机受控 Provider。\n\n已收到：%s\n\n任务已完成。", promptA)
	if resA.Status != api.RunCompleted || resA.Output != wantOutA {
		t.Fatalf("WaitRemote(restoredA) = %+v, want completed output %q", resA, wantOutA)
	}

	// Cancel Task B using the restored handle, then wait for its terminal state.
	if err := fed2.CancelRemote(ctx, PeerTarget{}, restoredB); err != nil {
		t.Fatalf("CancelRemote(restoredB) error = %v", err)
	}
	resB, err := fed2.WaitRemote(ctx, PeerTarget{}, restoredB)
	if err != nil {
		t.Fatalf("WaitRemote(restoredB) error = %v", err)
	}
	if resB.Status != api.RunCancelled {
		t.Fatalf("WaitRemote(restoredB) status = %q, want %q", resB.Status, api.RunCancelled)
	}

	// Phase 3: Re-read disk ledger to confirm terminal statuses were persisted
	// and file permissions remain 0600.
	infoAfter, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatalf("Stat(%s) after update error = %v", ledgerPath, err)
	}
	if perm := infoAfter.Mode().Perm(); perm != 0o600 {
		t.Fatalf("remote-tasks.json permissions after update = %04o, want 0600", perm)
	}

	fed3, err := NewFederator(FederatorOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewFederator(3) error = %v", err)
	}
	defer fed3.Close()

	if len(fed3.ActiveTasks()) != 0 {
		t.Fatalf("ActiveTasks() in fed3 = %d, want 0", len(fed3.ActiveTasks()))
	}
	finalA, ok := fed3.GetTask("persisted-wait-task")
	if !ok || finalA.Status != api.RunCompleted || finalA.Output != wantOutA {
		t.Fatalf("finalA in fed3 = %+v, ok=%v", finalA, ok)
	}
	finalB, ok := fed3.GetTask("persisted-cancel-task")
	if !ok || finalB.Status != api.RunCancelled {
		t.Fatalf("finalB in fed3 = %+v, ok=%v", finalB, ok)
	}
}

// reasoningDurableProvider exercises reasoning delta streaming alongside output deltas.
type reasoningDurableProvider struct{}

func (reasoningDurableProvider) Run(context.Context, provider.Request, func(string) error) error {
	return errors.New("unexpected non-durable Run call")
}

func (reasoningDurableProvider) Recoverable(provider.Request) bool { return false }

func (reasoningDurableProvider) Stop(context.Context, provider.Request) error { return nil }

func (reasoningDurableProvider) RunEvents(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
	thinking, err := json.Marshal(map[string]any{
		"assistantMessageEvent": map[string]any{"delta": "step-1: analyze remote workspace; step-2: run checks"},
	})
	if err != nil {
		return err
	}
	events := []provider.Event{
		{Sequence: 1, Type: "thinking_delta", Data: thinking},
		{Sequence: 2, Delta: "result-for:" + req.Message},
	}
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(ev); err != nil {
			return err
		}
	}
	return nil
}

func TestFederationStreamsReasoningAndIdleRedial(t *testing.T) {
	remoteNode := startPeerNode(t, reasoningDurableProvider{})
	target := PeerTarget{
		EnvironmentID: "env-reasoning",
		SocketPath:    remoteNode.socketPath,
	}

	var dials atomic.Int32
	fed, err := NewFederator(FederatorOptions{
		IdleTimeout:      25 * time.Millisecond,
		ReconnectBackoff: 10 * time.Millisecond,
		Dial: func(ctx context.Context, target PeerTarget) (PeerClient, error) {
			dials.Add(1)
			return DefaultDial(ctx, target)
		},
	})
	if err != nil {
		t.Fatalf("NewFederator() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	h1, err := fed.DelegateRemote(ctx, target, RemoteTaskSpec{
		LocalTaskID: "reasoning-1",
		Prompt:      "check-gpu",
	})
	if err != nil {
		t.Fatalf("DelegateRemote(1) error = %v", err)
	}
	res1, err := fed.WaitRemote(ctx, target, h1)
	if err != nil {
		t.Fatalf("WaitRemote(1) error = %v", err)
	}
	if res1.Reasoning != "step-1: analyze remote workspace; step-2: run checks" {
		t.Fatalf("res1.Reasoning = %q", res1.Reasoning)
	}
	if res1.Output != "result-for:check-gpu" {
		t.Fatalf("res1.Output = %q", res1.Output)
	}

	// Let the pooled connection exceed IdleTimeout and verify automatic redial.
	time.Sleep(40 * time.Millisecond)

	h2, err := fed.DelegateRemote(ctx, target, RemoteTaskSpec{
		LocalTaskID: "reasoning-2",
		Prompt:      "check-mem",
	})
	if err != nil {
		t.Fatalf("DelegateRemote(2) error = %v", err)
	}
	res2, err := fed.WaitRemote(ctx, target, h2)
	if err != nil {
		t.Fatalf("WaitRemote(2) error = %v", err)
	}
	if res2.Status != api.RunCompleted || res2.Output != "result-for:check-mem" {
		t.Fatalf("res2 = %+v", res2)
	}
	if got := dials.Load(); got < 2 {
		t.Fatalf("expected idle connection to redial, got %d dial(s)", got)
	}

	// Validate SSH target safety checks before closing.
	if err := ValidateTarget(PeerTarget{Host: "-oProxyCommand=evil", SocketPath: "/tmp/vantry.sock"}); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("ValidateTarget(host option injection) = %v, want invalid_command", err)
	}
	if err := ValidateTarget(PeerTarget{Host: "user@box", SocketPath: "relative/core.sock"}); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("ValidateTarget(relative remote socket) = %v, want invalid_command", err)
	}

	if err := fed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := fed.DelegateRemote(ctx, target, RemoteTaskSpec{LocalTaskID: "after-close", Prompt: "x"}); !errors.Is(err, ErrFederatorClosed) {
		t.Fatalf("DelegateRemote() after Close() = %v, want ErrFederatorClosed", err)
	}
}

func TestFederationConcurrentDelegationAndResyncRecovery(t *testing.T) {
	remoteNode := startPeerNode(t, controlled.Provider{Delay: 3 * time.Millisecond})
	target := PeerTarget{
		EnvironmentID: "env-concurrent",
		Name:          "Concurrent Peer",
		SocketPath:    remoteNode.socketPath,
	}

	var (
		dials        atomic.Int32
		injectResync atomic.Bool
	)
	injectResync.Store(true)

	dataDir := t.TempDir()
	fed, err := NewFederator(FederatorOptions{
		DataDir:           dataDir,
		ReconnectBackoff:  5 * time.Millisecond,
		ReconcileInterval: 100 * time.Millisecond,
		Dial: func(ctx context.Context, target PeerTarget) (PeerClient, error) {
			dials.Add(1)
			raw, err := DefaultDial(ctx, target)
			if err != nil {
				return nil, err
			}
			return &trackingClient{
				PeerClient: raw,
				onSubscribe: func(sCtx context.Context, after int64) (*api.Subscription, error) {
					if injectResync.CompareAndSwap(true, false) {
						return nil, &api.Error{
							Code:    api.ResyncRequired,
							Message: "simulated slow consumer cursor eviction",
						}
					}
					return raw.Subscribe(sCtx, after)
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewFederator() error = %v", err)
	}
	defer fed.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("concurrent-task-%d", idx)
			prompt := fmt.Sprintf("并发联邦子任务 #%d", idx)
			handle, err := fed.DelegateRemote(ctx, target, RemoteTaskSpec{
				LocalTaskID: taskID,
				CommandID:   fmt.Sprintf("cmd-concurrent-%d", idx),
				Prompt:      prompt,
			})
			if err != nil {
				errs <- fmt.Errorf("worker %d DelegateRemote: %w", idx, err)
				return
			}
			res, err := fed.WaitRemote(ctx, target, handle)
			if err != nil {
				errs <- fmt.Errorf("worker %d WaitRemote: %w", idx, err)
				return
			}
			want := fmt.Sprintf("这是 Vantry 的本机受控 Provider。\n\n已收到：%s\n\n任务已完成。", prompt)
			if res.Status != api.RunCompleted || res.Output != want {
				errs <- fmt.Errorf("worker %d unexpected result: status=%q output=%q", idx, res.Status, res.Output)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	if gotDials := dials.Load(); gotDials != 1 {
		t.Fatalf("concurrent workers dialed %d times, want 1 shared pooled connection", gotDials)
	}
	if gotTasks := len(fed.ListTasks()); gotTasks != workers {
		t.Fatalf("ListTasks() = %d, want %d", gotTasks, workers)
	}
}
