package environments

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/core"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type remoteDialogFixture struct {
	mu        sync.Mutex
	responses map[string]chan provider.Control
	calls     atomic.Int32
	stops     atomic.Int32
}

func (p *remoteDialogFixture) Run(context.Context, provider.Request, func(string) error) error {
	return errors.New("expected durable port")
}
func (p *remoteDialogFixture) Recoverable(provider.Request) bool { return true }
func (p *remoteDialogFixture) Stop(context.Context, provider.Request) error {
	p.stops.Add(1)
	return nil
}
func (p *remoteDialogFixture) RunEvents(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
	p.calls.Add(1)
	p.mu.Lock()
	if p.responses == nil {
		p.responses = map[string]chan provider.Control{}
	}
	response := make(chan provider.Control, 2)
	p.responses[req.AttemptID] = response
	p.mu.Unlock()
	send := func(seq int64, kind, delta string, data any) error {
		raw, _ := json.Marshal(data)
		return emit(provider.Event{Sequence: seq, Type: kind, Delta: delta, Data: raw})
	}
	if err := send(1, "text_delta", "before|", nil); err != nil {
		return err
	}
	if err := send(2, "thinking_delta", "", map[string]any{"assistantMessageEvent": map[string]string{"delta": "remote reasoning"}}); err != nil {
		return err
	}
	if err := send(3, "tool_execution_start", "", map[string]any{"toolCallId": "bash-1", "toolName": "bash", "args": map[string]string{"command": "fixture only"}}); err != nil {
		return err
	}
	if err := send(4, "extension_ui_request", "", map[string]any{"id": "dialog-1", "method": "confirm", "title": "Allow remote fixture?"}); err != nil {
		return err
	}
	seq := int64(5)
	for {
		select {
		case <-ctx.Done():
			return provider.ErrDetached
		case control := <-response:
			if err := send(seq, "control_result", "", provider.ControlResult{ID: control.ID, Status: "delivered"}); err != nil {
				return err
			}
			seq++
			if control.Kind == "steer" {
				continue
			}
			if err := send(seq, "text_delta", "|after", nil); err != nil {
				return err
			}
			seq++
			return send(seq, "tool_execution_end", "", map[string]any{"toolCallId": "bash-1", "toolName": "bash", "result": map[string]string{"output": "fixture completed"}})
		}
	}
}
func (p *remoteDialogFixture) Control(ctx context.Context, req provider.Request, control provider.Control) error {
	p.mu.Lock()
	ch := p.responses[req.AttemptID]
	p.mu.Unlock()
	if ch == nil {
		return errors.New("fixture not ready")
	}
	select {
	case ch <- control:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type localFixture struct{}

func (localFixture) Run(ctx context.Context, req provider.Request, emit func(string) error) error {
	return emit("local")
}
func (localFixture) RunEvents(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
	return emit(provider.Event{Sequence: 1, Delta: "local"})
}
func (localFixture) Recoverable(provider.Request) bool            { return true }
func (localFixture) Stop(context.Context, provider.Request) error { return nil }

func TestRemoteProxyRecoversEventsControlsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fixture := &remoteDialogFixture{}
	peer := startPeerNode(t, fixture)
	dir := t.TempDir()
	var fed *Federator
	var localCore *core.Core
	var routed *Provider
	open := func() {
		var err error
		fed, err = NewFederator(FederatorOptions{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		routed = &Provider{Local: localFixture{}, Federator: fed, ResolveTarget: func(context.Context, string) (PeerTarget, error) {
			return PeerTarget{EnvironmentID: "target", SocketPath: peer.socketPath}, nil
		}}
		localCore, err = core.Open(ctx, core.Options{DBPath: filepath.Join(dir, "local.db"), Provider: routed})
		if err != nil {
			t.Fatal(err)
		}
	}
	open()
	defer func() { localCore.Close(); fed.Close() }()
	dispatch := func(cmd api.Command) api.Receipt {
		t.Helper()
		cmd.Version = api.ProtocolVersion
		receipt, err := localCore.Dispatch(ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	dispatch(api.Command{CommandID: "create", Kind: api.CreateThread, ThreadID: "proxy", ProviderID: "pi", Model: "remote-model", Workspace: &api.WorkspaceSpec{EnvironmentID: "target", Kind: "directory", Path: "/remote-only"}, ApprovalPolicy: "ask"})
	sent := dispatch(api.Command{CommandID: "send", Kind: api.SendMessage, ThreadID: "proxy", Message: "remote task"})
	wait := func(description string, check func(api.Snapshot) bool) api.Snapshot {
		t.Helper()
		for i := 0; i < 500; i++ {
			snap, err := localCore.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if check(snap) {
				return snap
			}
			select {
			case <-ctx.Done():
				t.Fatal(description, ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
		}
		t.Fatal("timeout: " + description)
		return api.Snapshot{}
	}
	snap := wait("remote approval", func(s api.Snapshot) bool { return len(s.Requests) == 1 && len(s.Tools) == 1 })
	requestID := snap.Requests[0].ID
	if snap.Runs[0].Output != "before|" || snap.Runs[0].Reasoning != "remote reasoning" {
		t.Fatalf("remote output projection: %+v", snap.Runs)
	}
	if err := localCore.Close(); err != nil {
		t.Fatal(err)
	}
	fed.Close()
	open()
	snap = wait("same request after reattach", func(s api.Snapshot) bool { return len(s.Requests) == 1 && s.Requests[0].ID == requestID })
	if fixture.calls.Load() != 1 {
		t.Fatalf("restart repeated remote prompt: %d", fixture.calls.Load())
	}
	steer := dispatch(api.Command{CommandID: "steer", Kind: api.SteerRun, ThreadID: "proxy", RunID: sent.RunID, Message: "adjust while awaiting approval"})
	wait("true remote steer acknowledgement", func(s api.Snapshot) bool {
		for _, in := range s.Inputs {
			if in.ID == steer.InputID {
				return in.Status == "delivered"
			}
		}
		return false
	})
	dispatch(api.Command{CommandID: "approve", Kind: api.RespondRequest, RequestID: requestID, Confirmed: true})
	final := wait("remote completion", func(s api.Snapshot) bool { return s.Runs[0].Status.Terminal() })
	if final.Runs[0].Status != api.RunCompleted || final.Runs[0].Output != "before||after" || len(final.Tools) != 1 || final.Tools[0].Status != "completed" || final.Requests[0].Status != "responded" {
		t.Fatalf("remote replay duplicated or lost data: %+v", final)
	}
	if fixture.calls.Load() != 1 {
		t.Fatalf("recovery resent prompt: %d", fixture.calls.Load())
	}

	// The peer may settle before its terminal event reaches the local proxy. A
	// steer bound to the old attempt must fail, not enqueue another peer run.
	late := routed.Control(ctx, provider.Request{ThreadID: "proxy", RunID: sent.RunID, AttemptID: final.Runs[0].AttemptID, Workspace: &api.WorkspaceSpec{EnvironmentID: "target"}}, provider.Control{ID: "late-steer", Kind: "steer", Message: "must not become a new prompt"})
	if api.CodeOf(late) != api.Conflict {
		t.Fatalf("late remote steer queued unexpected work: %v", late)
	}
	afterLate, err := peer.core.Snapshot(ctx)
	if err != nil || len(afterLate.Runs) != 1 {
		t.Fatalf("late steer created another remote run: %+v %v", afterLate.Runs, err)
	}

	// Cancel a later remote turn through the normal local run.stop outbox. The
	// remote Core owns and acknowledges the stop; observer detachment did not.
	dispatch(api.Command{CommandID: "next-model", Kind: api.SetThreadModel, ThreadID: "proxy", Model: "remote-next-model"})
	second := dispatch(api.Command{CommandID: "send-again", Kind: api.SendMessage, ThreadID: "proxy", Message: "cancel me"})
	wait("second remote request", func(s api.Snapshot) bool {
		for _, r := range s.Requests {
			if r.RunID == second.RunID && r.Status == "pending" {
				return true
			}
		}
		return false
	})
	dispatch(api.Command{CommandID: "stop", Kind: api.StopRun, ThreadID: "proxy", RunID: second.RunID})
	wait("cancelled local proxy", func(s api.Snapshot) bool {
		for _, r := range s.Runs {
			if r.ID == second.RunID {
				return r.Status == api.RunCancelled
			}
		}
		return false
	})
	remoteSnap, err := peer.core.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := false
	for _, r := range remoteSnap.Runs {
		if r.Status == api.RunCancelled {
			cancelled = true
		}
	}
	for _, run := range remoteSnap.Runs {
		if run.Status == api.RunCancelled && run.Model != "remote-next-model" {
			t.Fatalf("later remote model ignored: %+v", run)
		}
	}
	if !cancelled || fixture.stops.Load() != 1 {
		t.Fatalf("stop failed to reach peer: %+v stops=%d", remoteSnap.Runs, fixture.stops.Load())
	}
	if len(remoteSnap.Threads) != 1 || len(remoteSnap.Runs) != 2 {
		t.Fatalf("remote session was recreated: %+v", remoteSnap)
	}
}

func TestRemoteProviderRejectsLocalAttachmentHandles(t *testing.T) {
	p := &Provider{}
	err := p.RunEvents(context.Background(), provider.Request{Workspace: &api.WorkspaceSpec{EnvironmentID: "target"}, Attachments: []api.Attachment{{ID: "local-image"}}}, func(provider.Event) error { return nil })
	if api.CodeOf(err) != api.InvalidCommand || !strings.Contains(err.Error(), "附件") {
		t.Fatalf("local attachment crossed environments: %v", err)
	}
}
