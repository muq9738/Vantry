package core

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// workbenchProvider is a durable provider whose journal is driven by the test.
// It never talks to a model; it exists to prove how Core projects real Pi tool,
// dialog and control events.
type workbenchProvider struct {
	mu         sync.Mutex
	script     []provider.Event
	done       chan struct{}
	entered    chan struct{}
	calls      []provider.Request
	controls   []provider.Control
	controlErr error
	onControl  func(provider.Control) *provider.ControlResult
	recover    bool
}

func newWorkbenchProvider(t *testing.T) *workbenchProvider {
	t.Helper()
	return &workbenchProvider{done: make(chan struct{}), entered: make(chan struct{}, 8)}
}

// add appends one journaled provider event and returns it with its sequence.
func (p *workbenchProvider) add(eventType string, data any) provider.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev := provider.Event{Type: eventType}
	if eventType == "text_delta" {
		ev.Delta, _ = data.(string)
	} else if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		ev.Data = raw
	}
	ev.Sequence = int64(len(p.script)) + 1
	p.script = append(p.script, ev)
	return ev
}

func (p *workbenchProvider) finish() { close(p.done) }

func (p *workbenchProvider) Run(ctx context.Context, request provider.Request, emit func(string) error) error {
	return p.RunEvents(ctx, request, func(event provider.Event) error {
		if event.Delta == "" {
			return nil
		}
		return emit(event.Delta)
	})
}

func (p *workbenchProvider) RunEvents(ctx context.Context, request provider.Request, emit func(provider.Event) error) error {
	p.mu.Lock()
	p.calls = append(p.calls, request)
	p.mu.Unlock()
	select {
	case p.entered <- struct{}{}:
	default:
	}

	next := request.AfterSequence + 1
	for {
		p.mu.Lock()
		var pending []provider.Event
		for _, ev := range p.script {
			if ev.Sequence >= next {
				pending = append(pending, ev)
			}
		}
		p.mu.Unlock()
		for _, ev := range pending {
			if ev.Sequence != next {
				return nil // a gap is a test bug, not a Core behaviour
			}
			if err := emit(ev); err != nil {
				return err
			}
			next++
		}
		select {
		case <-ctx.Done():
			return provider.ErrDetached
		case <-p.done:
			// Deliver whatever the test appended last before finishing.
			p.mu.Lock()
			remaining := append([]provider.Event(nil), p.script...)
			p.mu.Unlock()
			for _, ev := range remaining {
				if ev.Sequence < next {
					continue
				}
				if err := emit(ev); err != nil {
					return err
				}
				next = ev.Sequence + 1
			}
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (p *workbenchProvider) Recoverable(provider.Request) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recover
}

func (p *workbenchProvider) Stop(context.Context, provider.Request) error { return nil }

func (p *workbenchProvider) Control(_ context.Context, _ provider.Request, control provider.Control) error {
	p.mu.Lock()
	p.controls = append(p.controls, control)
	err := p.controlErr
	hook := p.onControl
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if hook != nil {
		if result := hook(control); result != nil {
			p.add("control_result", result)
		}
	}
	return nil
}

func (p *workbenchProvider) controlCalls() []provider.Control {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Control(nil), p.controls...)
}

// controlCallsReset forgets the recorded handoffs; the caller holds the lock.
func (p *workbenchProvider) controlCallsReset() { p.controls = nil }

func (p *workbenchProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// nonControllerProvider is durable but cannot accept steer or dialog responses.
// It deliberately does not embed the controller so Control is not promoted.
type nonControllerProvider struct{ inner *workbenchProvider }

func (p *nonControllerProvider) Run(ctx context.Context, request provider.Request, emit func(string) error) error {
	return p.inner.Run(ctx, request, emit)
}

func (p *nonControllerProvider) RunEvents(ctx context.Context, request provider.Request, emit func(provider.Event) error) error {
	return p.inner.RunEvents(ctx, request, emit)
}

func (p *nonControllerProvider) Recoverable(request provider.Request) bool {
	return p.inner.Recoverable(request)
}

func (p *nonControllerProvider) Stop(ctx context.Context, request provider.Request) error {
	return p.inner.Stop(ctx, request)
}

func mustQueue(t *testing.T, c *Core, commandID, threadID, message string) api.Receipt {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), api.Command{
		Version:   api.ProtocolVersion,
		CommandID: commandID,
		Kind:      api.QueueMessage,
		ThreadID:  threadID,
		Message:   message,
	})
	if err != nil {
		t.Fatalf("queue message: %v", err)
	}
	return receipt
}

func mustSteer(t *testing.T, c *Core, commandID, threadID, runID, attemptID, message string) api.Receipt {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), api.Command{
		Version:         api.ProtocolVersion,
		CommandID:       commandID,
		Kind:            api.SteerRun,
		ThreadID:        threadID,
		RunID:           runID,
		ParentAttemptID: attemptID,
		Message:         message,
	})
	if err != nil {
		t.Fatalf("steer: %v", err)
	}
	return receipt
}

func mustRespond(t *testing.T, c *Core, commandID, requestID, value string, confirmed, cancelled bool) api.Receipt {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), api.Command{
		Version:   api.ProtocolVersion,
		CommandID: commandID,
		Kind:      api.RespondRequest,
		RequestID: requestID,
		Value:     value,
		Confirmed: confirmed,
		Cancelled: cancelled,
	})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	return receipt
}

func findInput(snap api.Snapshot, inputID string) (api.Input, bool) {
	for _, input := range snap.Inputs {
		if input.ID == inputID {
			return input, true
		}
	}
	return api.Input{}, false
}

func waitInputStatus(t *testing.T, c *Core, inputID, status string) api.Input {
	t.Helper()
	var input api.Input
	waitFor(t, "input "+inputID+" to become "+status, func() bool {
		found, ok := findInput(snapshotOf(t, c), inputID)
		if !ok {
			return false
		}
		input = found
		return found.Status == status
	})
	return input
}

func findRequest(snap api.Snapshot, requestID string) (api.ProviderRequest, bool) {
	for _, request := range snap.Requests {
		if request.ID == requestID {
			return request, true
		}
	}
	return api.ProviderRequest{}, false
}

func findTool(snap api.Snapshot, toolID string) (api.ToolCall, bool) {
	for _, tool := range snap.Tools {
		if tool.ID == toolID {
			return tool, true
		}
	}
	return api.ToolCall{}, false
}
