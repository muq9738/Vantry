package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// Provider keeps Core's normal run/task/outbox lifecycle while executing a
// linked workspace on its owning Core. Local executions retain their original
// provider, including the independent Pi worker's recovery and control port.
type Provider struct {
	Local         provider.Durable
	Federator     *Federator
	ResolveTarget func(context.Context, string) (PeerTarget, error)
	mu            sync.Mutex
	attempts      map[string]*sync.Mutex
}

func (p *Provider) remote(req provider.Request) bool {
	return req.Workspace != nil && req.Workspace.EnvironmentID != ""
}

func (p *Provider) attemptLock(id string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempts == nil {
		p.attempts = map[string]*sync.Mutex{}
	}
	if p.attempts[id] == nil {
		p.attempts[id] = &sync.Mutex{}
	}
	return p.attempts[id]
}

func (p *Provider) Run(ctx context.Context, req provider.Request, emit func(string) error) error {
	return p.RunEvents(ctx, req, func(event provider.Event) error {
		if event.Delta != "" {
			return emit(event.Delta)
		}
		return nil
	})
}

func (p *Provider) Recoverable(req provider.Request) bool {
	return p.remote(req) || p.Local.Recoverable(req)
}

func remoteThreadID(req provider.Request) string { return "remote_" + req.ThreadID }

// ensureRemote is serialized with Stop: cancelling before a dispatch must not
// leave a remote prompt running after the local stop outbox is acknowledged.
func (p *Provider) ensureRemote(ctx context.Context, req provider.Request) (*RemoteTaskHandle, error) {
	if handle, ok := p.Federator.GetTask(req.AttemptID); ok {
		return handle, nil
	}
	target, err := p.ResolveTarget(ctx, req.Workspace.EnvironmentID)
	if err != nil {
		return nil, err
	}
	workspace := *req.Workspace
	workspace.EnvironmentID = ""
	firstAttempt := true
	for _, handle := range p.Federator.ListTasks() {
		if handle.RemoteThreadID == remoteThreadID(req) && handle.Target.EnvironmentID == target.EnvironmentID {
			firstAttempt = false
			break
		}
	}
	return p.Federator.DelegateRemote(ctx, target, RemoteTaskSpec{
		LocalTaskID: req.AttemptID, RemoteTaskID: req.AttemptID,
		CommandID: "remote:" + req.AttemptID, RemoteThreadID: remoteThreadID(req),
		Title: "Remote task", Prompt: req.Message, Workspace: &workspace,
		ProviderID: req.ProviderID, Model: req.Model,
		ApprovalPolicy: req.ApprovalPolicy, InteractionMode: req.InteractionMode,
		ExistingThread: !firstAttempt,
	})
}

func (p *Provider) RunEvents(ctx context.Context, req provider.Request, emit func(provider.Event) error) error {
	if !p.remote(req) {
		return p.Local.RunEvents(ctx, req, emit)
	}
	if len(req.Attachments) != 0 {
		return &api.Error{Code: api.InvalidCommand, Message: "远端任务不接受本机附件句柄，请在目标环境添加附件"}
	}
	lock := p.attemptLock(req.AttemptID)
	var handle *RemoteTaskHandle
	var err error
	for {
		lock.Lock()
		handle, err = p.ensureRemote(ctx, req)
		lock.Unlock()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return provider.ErrDetached
		}
		if isDeterministicAPIError(err) {
			return err
		}
		if err := remoteRetry(ctx); err != nil {
			return err
		}
	}
	var client PeerClient

	// Replay peer events with a stable per-attempt ordinal. Core's committed
	// provider_cursor is the acknowledgement, so a crash after ledger progress
	// but before a local commit cannot drop output or replay a prompt.
	cursor, ordinal := handle.InitialSequence, int64(0)
	var terminal *api.Run
	for {
		if ctx.Err() != nil {
			return provider.ErrDetached
		}
		if client == nil {
			client, err = p.Federator.Client(ctx, handle.Target)
			if err != nil {
				if isDeterministicAPIError(err) {
					return err
				}
				if err := remoteRetry(ctx); err != nil {
					return err
				}
				continue
			}
		}
		sub, err := client.Subscribe(ctx, cursor)
		if err != nil {
			if api.CodeOf(err) == api.ResyncRequired {
				return errors.New("远端事件历史已裁剪，无法无损恢复此任务；请读取目标线程结果")
			}
			if err := remoteRetry(ctx); err != nil {
				return err
			}
			client, err = p.Federator.Client(ctx, handle.Target)
			if err != nil {
				continue
			}
			continue
		}
		finished, streamErr := func() (bool, error) {
			defer sub.Close()
			// Snapshot is a fallback, not part of the ordered event stream. A
			// slow full-state response must not block output or the terminal
			// event that releases the parent's waiting_children state.
			streamCtx, cancelStream := context.WithCancel(ctx)
			defer cancelStream()
			type snapshotResult struct {
				snapshot api.Snapshot
				err      error
			}
			snapshots := make(chan snapshotResult)
			go func(streamClient PeerClient) {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-streamCtx.Done():
						return
					case <-ticker.C:
					}
					probeCtx, cancelProbe := context.WithTimeout(streamCtx, 5*time.Second)
					snapshot, err := streamClient.Snapshot(probeCtx)
					cancelProbe()
					select {
					case snapshots <- snapshotResult{snapshot: snapshot, err: err}:
					case <-streamCtx.Done():
						return
					}
				}
			}(client)
			for {
				select {
				case <-ctx.Done():
					return false, provider.ErrDetached
				case event, ok := <-sub.Events:
					if !ok {
						return false, nil
					}
					if event.Sequence <= cursor {
						continue
					}
					cursor = event.Sequence
					if event.RunID != handle.RemoteRunID {
						continue
					}
					ordinal++
					if ordinal > req.AfterSequence {
						if err := emit(remoteProviderEvent(event, ordinal)); err != nil {
							return false, err
						}
					}
					if strings.HasPrefix(event.Type, "run.") {
						var data struct {
							Run api.Run `json:"run"`
						}
						if json.Unmarshal(event.Data, &data) == nil && data.Run.Status.Terminal() {
							copy := data.Run
							terminal = &copy
							return true, nil
						}
					}
				case err, ok := <-sub.Errors:
					if ok && err != nil && api.CodeOf(err) == api.ResyncRequired {
						return false, err
					}
					return false, nil
				case result := <-snapshots:
					// A terminal snapshot alone must not advance the local cursor:
					// the subscription still owns replay of the preceding events.
					if result.err != nil {
						return false, nil
					}
					snap := result.snapshot
					if run, ok := findRun(snap, handle.RemoteRunID); ok && run.Status.Terminal() && cursor >= snap.Sequence {
						terminal = &run
						return true, nil
					}
				}
			}
		}()
		if streamErr != nil {
			return streamErr
		}
		if finished && terminal != nil {
			p.Federator.persistTerminal(req.AttemptID, handle.Target, handle, cursor, RemoteTaskResult{
				Status: terminal.Status, Output: terminal.Output, Reasoning: terminal.Reasoning,
				Error: terminal.Error, RemoteThreadID: handle.RemoteThreadID, RemoteRunID: handle.RemoteRunID,
			})
			switch terminal.Status {
			case api.RunCompleted:
				return nil
			case api.RunCancelled:
				return provider.ErrExecutionCancelled
			case api.RunInterrupted:
				return context.Canceled
			default:
				return fmt.Errorf("远端任务 %s: %s", terminal.Status, terminal.Error)
			}
		}
		if err := remoteRetry(ctx); err != nil {
			return err
		}
		client, err = p.Federator.Client(ctx, handle.Target)
		if err != nil {
			continue
		}
	}
}

func remoteRetry(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return provider.ErrDetached
	case <-timer.C:
		return nil
	}
}

func remoteProviderEvent(event api.Event, ordinal int64) provider.Event {
	out := provider.Event{Sequence: ordinal, OccurredAt: event.Timestamp, Type: "remote_event"}
	switch event.Type {
	case "run.output":
		var payload struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(event.Data, &payload)
		out.Type, out.Delta = "text_delta", payload.Delta
	case "run.reasoning":
		var payload struct {
			Delta string `json:"reasoningDelta"`
		}
		_ = json.Unmarshal(event.Data, &payload)
		out.Type = "thinking_delta"
		out.Data, _ = json.Marshal(map[string]any{"assistantMessageEvent": map[string]string{"delta": payload.Delta}})
	case "tool.updated", "request.updated", "input.updated":
		out.Type, out.Data = "remote."+event.Type, event.Data
	}
	return out
}

func (p *Provider) Stop(ctx context.Context, req provider.Request) error {
	if !p.remote(req) {
		return p.Local.Stop(ctx, req)
	}
	lock := p.attemptLock(req.AttemptID)
	lock.Lock()
	defer lock.Unlock()
	handle, ok := p.Federator.GetTask(req.AttemptID)
	if !ok {
		// Even a lost dispatch receipt can have created this deterministic peer
		// thread. Find and cancel that run; never submit the prompt merely to stop.
		target, err := p.ResolveTarget(ctx, req.Workspace.EnvironmentID)
		if err != nil {
			return err
		}
		client, err := p.Federator.Client(ctx, target)
		if err != nil {
			return err
		}
		snap, err := client.Snapshot(ctx)
		if err != nil {
			return err
		}
		for _, run := range snap.Runs {
			if run.ThreadID == remoteThreadID(req) && !run.Status.Terminal() {
				_, err = client.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: "remote-stop:" + req.AttemptID + ":" + run.ID,
					Kind: api.StopRun, ThreadID: run.ThreadID, RunID: run.ID})
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	return p.Federator.CancelRemote(ctx, handle.Target, handle)
}

func (p *Provider) SupportsControl(req provider.Request) bool {
	if p.remote(req) {
		return true
	}
	if router, ok := p.Local.(provider.ControlRouter); ok {
		return router.SupportsControl(req)
	}
	_, ok := p.Local.(provider.Controller)
	return ok
}

func (p *Provider) Control(ctx context.Context, req provider.Request, control provider.Control) error {
	if !p.remote(req) {
		controller, ok := p.Local.(provider.Controller)
		if !ok {
			return errors.New("local provider does not support controls")
		}
		return controller.Control(ctx, req, control)
	}
	if len(control.Attachments) != 0 {
		return errors.New("远端控制不接受本机附件句柄")
	}
	handle, ok := p.Federator.GetTask(req.AttemptID)
	if !ok {
		return errors.New("远端运行尚未连接，请稍后重试")
	}
	client, err := p.Federator.Client(ctx, handle.Target)
	if err != nil {
		return err
	}
	cmd := api.Command{Version: api.ProtocolVersion, CommandID: "remote-control:" + control.ID,
		ThreadID: handle.RemoteThreadID, RunID: handle.RemoteRunID}
	switch control.Kind {
	case "steer":
		// The local control is bound to this proxy attempt. Do not let an
		// already-settled peer run turn it into an untracked second prompt.
		snapshot, err := client.Snapshot(ctx)
		if err != nil {
			return err
		}
		run, found := findRun(snapshot, handle.RemoteRunID)
		if !found || run.Status != api.RunRunning || run.AttemptID == "" {
			return &api.Error{Code: api.Conflict, Message: "远端轮次已结束，调整未投递；请作为后续消息发送"}
		}
		cmd.Kind, cmd.Message, cmd.ParentAttemptID = api.SteerRun, control.Message, run.AttemptID
	case "response":
		cmd.Kind, cmd.RequestID, cmd.Value, cmd.Confirmed, cmd.Cancelled = api.RespondRequest, control.RequestID, control.Value, control.Confirmed, control.Cancelled
	default:
		return errors.New("unsupported remote control")
	}
	_, err = client.Dispatch(ctx, cmd)
	return err
}

var _ provider.Durable = (*Provider)(nil)
var _ provider.Controller = (*Provider)(nil)
