package environments

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type blockedSnapshotPeer struct {
	PeerClient
	events          chan api.Event
	snapshotStarted chan struct{}
	snapshotStopped chan struct{}
}

func (p *blockedSnapshotPeer) Health(context.Context) (api.Health, error) {
	return api.Health{Version: api.ProtocolVersion, State: "ready"}, nil
}
func (p *blockedSnapshotPeer) Close() error { return nil }
func (p *blockedSnapshotPeer) Subscribe(context.Context, int64) (*api.Subscription, error) {
	return &api.Subscription{Events: p.events, Errors: make(chan error), Close: func() {}}, nil
}
func (p *blockedSnapshotPeer) Snapshot(ctx context.Context) (api.Snapshot, error) {
	close(p.snapshotStarted)
	<-ctx.Done()
	close(p.snapshotStopped)
	return api.Snapshot{}, ctx.Err()
}

// A remote Core can finish while a large Snapshot response is still in flight.
// The status fallback must never stop the subscription from delivering output
// and the terminal event, or the local parent can wait indefinitely behind it.
func TestRemoteProviderStreamsWhileSnapshotIsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := &blockedSnapshotPeer{
		events: make(chan api.Event, 2), snapshotStarted: make(chan struct{}), snapshotStopped: make(chan struct{}),
	}
	fed, err := NewFederator(FederatorOptions{Dial: func(context.Context, PeerTarget) (PeerClient, error) { return peer, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer fed.Close()
	target := PeerTarget{EnvironmentID: "peer", SocketPath: "/unused-peer.sock"}
	fed.tasks["attempt"] = &RemoteTaskHandle{LocalTaskID: "attempt", Target: target, RemoteThreadID: "remote-thread", RemoteRunID: "remote-run"}
	p := &Provider{Federator: fed}
	done := make(chan error, 1)
	output := make(chan string, 1)
	go func() {
		done <- p.RunEvents(ctx, provider.Request{AttemptID: "attempt", Workspace: &api.WorkspaceSpec{EnvironmentID: "peer"}}, func(event provider.Event) error {
			if event.Delta != "" {
				output <- event.Delta
			}
			return nil
		})
	}()
	select {
	case <-peer.snapshotStarted:
	case err := <-done:
		t.Fatalf("provider stopped before fallback: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot fallback never started")
	}
	text, _ := json.Marshal(map[string]string{"delta": "built"})
	terminal, _ := json.Marshal(map[string]api.Run{"run": {ID: "remote-run", Status: api.RunCompleted, Output: "built"}})
	peer.events <- api.Event{Sequence: 1, RunID: "remote-run", Type: "run.output", Data: text}
	peer.events <- api.Event{Sequence: 2, RunID: "remote-run", Type: "run.completed", Data: terminal}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked snapshot stalled remote output and completion")
	}
	select {
	case got := <-output:
		if got != "built" {
			t.Fatalf("output = %q", got)
		}
	default:
		t.Fatal("terminal state overtook remote output")
	}
	select {
	case <-peer.snapshotStopped:
	case <-time.After(time.Second):
		t.Fatal("completed stream did not cancel its in-flight snapshot")
	}
}
