package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

type fakeRPCConfig struct{ Mode, Commands, Arguments, Calls, EOF string }

// A separate controllable RPC process. It refuses CLI prompts, records every
// input, and only exits cleanly after the client closes stdin.
func fakeRPCMain() {
	var config fakeRPCConfig
	data, err := os.ReadFile(os.Args[2])
	if err != nil || json.Unmarshal(data, &config) != nil {
		os.Exit(71)
	}
	_ = os.WriteFile(config.Arguments, []byte(strings.Join(os.Args[3:], "\n")), 0o600)
	calls, _ := os.OpenFile(config.Calls, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = calls.WriteString("x\n")
	_ = calls.Close()
	log, err := os.OpenFile(config.Commands, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(72)
	}
	defer log.Close()
	emit := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	response := func(id, command string, success bool, data any) {
		emit(map[string]any{"type": "response", "id": id, "command": command, "success": success, "data": data, "error": "deliberate RPC refusal"})
	}
	settle := func() { emit(map[string]any{"type": "agent_settled", "aborted": false}) }
	if strings.Contains(strings.Join(os.Args[3:], " "), "approval.ts") {
		emit(map[string]any{"type": "extension_ui_request", "id": "ready", "method": "notify", "message": "vantry_approval_ready"})
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = os.WriteFile(config.EOF, []byte("EOF"), 0o600)
				return
			}
			os.Exit(73)
		}
		_, _ = log.Write(line)
		_ = log.Sync()
		var command struct {
			Type, ID, Message string
			Confirmed         bool
		}
		if json.Unmarshal(line, &command) != nil {
			os.Exit(74)
		}
		switch command.Type {
		case "get_state":
			if config.Mode == "slow-start" {
				// Extension startup can legitimately exceed the command timeout.
				time.Sleep(rpcTimeout + time.Second)
			}
			response(command.ID, command.Type, config.Mode != "reject-state", map[string]any{"isStreaming": false})
			if config.Mode == "backpressure" {
				emit(map[string]any{"type": "notification", "text": strings.Repeat("startup", 100000)})
			}
		case "prompt":
			if config.Mode == "reject-prompt" {
				response(command.ID, command.Type, false, nil)
				continue
			}
			if config.Mode == "settled-before-accept" {
				settle()
			}
			if config.Mode == "settled-reject" {
				settle()
				response(command.ID, command.Type, false, nil)
				continue
			}
			response(command.ID, command.Type, true, map[string]string{"disposition": "started"})
			if config.Mode == "silent" {
				continue
			}
			emit(map[string]any{"type": "agent_start"})
			if config.Mode == "chat" {
				directory, _ := os.Getwd()
				emit(map[string]any{"type": "fixture_cwd", "path": directory})
			}
			emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "text_delta", "delta": "ready\u2028line"}})
			switch config.Mode {
			case "backpressure":
				controls := filepath.Join(filepath.Dir(config.Commands), "jobs", "attempt-1", "controls")
				limit := time.Now().Add(10 * time.Second)
				for time.Now().Before(limit) {
					entries, _ := os.ReadDir(controls)
					if len(entries) > 0 {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				emit(map[string]any{"type": "notification", "text": strings.Repeat("streaming", 100000)})
			case "approval", "expired", "stale-dialog", "stall-dialog-paused":
				emit(map[string]any{"type": "tool_execution_start", "toolCallId": "tool-1", "toolName": "bash", "args": map[string]string{"command": "true"}})
				timeout := 0
				if config.Mode == "expired" {
					timeout = 30
				}
				emit(map[string]any{"type": "extension_ui_request", "id": "pi:dialog/1", "method": "confirm", "title": "Allow bash?", "timeout": timeout})
			case "stall-tool":
				emit(map[string]any{"type": "tool_execution_start", "toolCallId": "tool-1", "toolName": "bash", "args": map[string]string{"command": "sleep 99"}})
			case "stall-stream":
				// Stay silent with no active tool so stream-idle watchdog fires.
			case "normal", "slow-start", "settled-before-accept", "chat":
				settle()
			case "settle-control":
				// Give Core time to persist a control but never accept it.
				time.Sleep(100 * time.Millisecond)
				settle()
			}
		case "steer":
			if config.Mode == "sent-crash" {
				time.Sleep(30 * time.Second)
				continue
			}
			response(command.ID, command.Type, config.Mode != "reject-steer", map[string]string{"disposition": "queued"})
			settle()
		case "extension_ui_response":
			if command.ID != "pi:dialog/1" {
				os.Exit(75)
			}
			emit(map[string]any{"type": "tool_execution_end", "toolCallId": "tool-1", "toolName": "bash", "isError": !command.Confirmed, "result": map[string]any{"content": []any{}}})
			settle()
		case "get_available_models":
			if config.Mode == "catalog-hang" {
				time.Sleep(30 * time.Second)
				continue
			}
			success := config.Mode != "catalog-reject"
			emit(map[string]any{"type": "response", "id": command.ID, "command": command.Type, "success": success, "error": "secret-token https://private.endpoint", "data": map[string]any{"models": []any{
				map[string]any{"id": "model/variant", "provider": "test", "name": "Test model", "apiKey": "secret-token", "baseUrl": "https://private.endpoint"},
				map[string]any{"id": "model/variant", "provider": "test", "name": "duplicate"},
			}}})
		default:
			os.Exit(76)
		}
	}
}

func newRPCProvider(t *testing.T, mode string) (*Provider, provider.Request, fakeRPCConfig) {
	t.Helper()
	root := t.TempDir()
	config := fakeRPCConfig{Mode: mode, Commands: filepath.Join(root, "commands"), Arguments: filepath.Join(root, "arguments"), Calls: filepath.Join(root, "calls"), EOF: filepath.Join(root, "eof")}
	configPath := filepath.Join(root, "fake-config.json")
	if err := writeJSONFile(configPath, config); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	fake := filepath.Join(root, "fake-rpc")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec "+shellQuote(executable)+" pi-rpc-helper "+shellQuote(configPath)+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, request := newTestProvider(t, root, fake)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.Stop(ctx, request); err != nil {
			t.Errorf("stop RPC fixture: %v", err)
			return
		}
		jobDir := filepath.Join(root, "jobs", request.AttemptID)
		started, err := fileExists(filepath.Join(jobDir, "started.json"))
		if err != nil || !started {
			if err != nil {
				t.Errorf("read RPC fixture launch: %v", err)
			}
			return
		}
		// Stop only persists an intent. Keep TempDir alive until the worker
		// consumes it, publishes its result and releases its existing lock.
		// Removing the directory first can erase stop.json before its next tick.
		for {
			finished, err := fileExists(filepath.Join(jobDir, "result.json"))
			if err != nil {
				t.Errorf("read RPC fixture result: %v", err)
				return
			}
			if finished {
				lock, acquired, err := tryLock(filepath.Join(jobDir, "worker.lock"))
				if err != nil {
					t.Errorf("read RPC fixture exit: %v", err)
					return
				}
				if acquired {
					unlock(lock)
					return
				}
			}
			select {
			case <-ctx.Done():
				t.Errorf("RPC fixture did not finish before temporary directory cleanup: %v", ctx.Err())
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	return p, request, config
}

func controlResults(t *testing.T, events []provider.Event) map[string]provider.ControlResult {
	t.Helper()
	results := map[string]provider.ControlResult{}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("sequence[%d]=%d", i, event.Sequence)
		}
		if event.Type == "control_result" {
			var result provider.ControlResult
			if err := json.Unmarshal(event.Data, &result); err != nil {
				t.Fatal(err)
			}
			if _, exists := results[result.ID]; exists {
				t.Fatalf("duplicate result: %+v", result)
			}
			results[result.ID] = result
		}
	}
	return results
}

func TestRPCHandshakeRefusalAndSettlement(t *testing.T) {
	for _, mode := range []string{"normal", "slow-start", "reject-state", "reject-prompt", "settled-before-accept", "settled-reject"} {
		t.Run(mode, func(t *testing.T) {
			p, request, config := newRPCProvider(t, mode)
			var events []provider.Event
			err := p.RunEvents(context.Background(), request, func(event provider.Event) error { events = append(events, event); return nil })
			if strings.Contains(mode, "reject") {
				if err == nil || !strings.Contains(err.Error(), "refus") {
					t.Fatalf("refusal lost: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertFileText(t, config.EOF, "EOF")
			}
			commands := readFile(t, config.Commands)
			if strings.Count(commands, `"type":"prompt"`) > 1 {
				t.Fatal("prompt resent")
			}
			if mode == "reject-state" && strings.Contains(commands, `"type":"prompt"`) {
				t.Fatal("prompt sent after failed handshake")
			}
			controlResults(t, events)
		})
	}
}

func TestRPCSteerDeliveryRefusalAndIdempotentHandoff(t *testing.T) {
	for _, mode := range []string{"control", "reject-steer", "settle-control"} {
		t.Run(mode, func(t *testing.T) {
			p, request, config := newRPCProvider(t, mode)
			control := provider.Control{ID: "input:one", Kind: "steer", Message: "steer once"}
			var events []provider.Event
			err := p.RunEvents(context.Background(), request, func(event provider.Event) error {
				events = append(events, event)
				if event.Type == "text_delta" {
					for i := 0; i < 3; i++ {
						if err := p.Control(context.Background(), request, control); err != nil {
							return err
						}
					}
					changed := control
					changed.Message = "overwrite"
					if err := p.Control(context.Background(), request, changed); err == nil {
						return errors.New("conflicting immutable control accepted")
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			results := controlResults(t, events)
			want := "delivered"
			switch mode {
			case "reject-steer":
				want = "failed"
			case "settle-control":
				// Pi settled without acknowledging: delivery is unconfirmed.
				want = "unknown"
			}
			if results[control.ID].Status != want {
				t.Fatalf("results=%+v", results)
			}
			if mode == "reject-steer" && !strings.Contains(results[control.ID].Error, "refusal") {
				t.Fatal("RPC refusal swallowed")
			}
			if n := strings.Count(readFile(t, config.Commands), `"type":"steer"`); n != 1 {
				t.Fatalf("steer count=%d", n)
			}
			if err := p.Control(context.Background(), request, control); err != nil {
				t.Fatalf("retry after settlement: %v", err)
			}
			late := control
			late.ID = "late:control"
			if err := p.Control(context.Background(), request, late); err == nil {
				t.Fatal("new control accepted after settlement")
			}
			stale := request
			stale.AttemptID = "old-attempt"
			if err := p.Control(context.Background(), stale, control); err == nil {
				t.Fatal("stale attempt accepted")
			}
		})
	}
}

func TestRPCDialogResponseExpiryAndOriginalID(t *testing.T) {
	for _, mode := range []string{"approval", "expired", "stale-dialog"} {
		t.Run(mode, func(t *testing.T) {
			p, request, config := newRPCProvider(t, mode)
			request.ApprovalPolicy = "ask"
			control := provider.Control{ID: request.AttemptID + ":pi:dialog/1", Kind: "response", RequestID: "pi:dialog/1", Confirmed: true}
			var events []provider.Event
			err := p.RunEvents(context.Background(), request, func(event provider.Event) error {
				events = append(events, event)
				if event.Type == "extension_ui_request" && strings.Contains(string(event.Data), `"method":"confirm"`) {
					if mode == "expired" {
						time.Sleep(100 * time.Millisecond)
					}
					if mode == "stale-dialog" {
						control.RequestID = "old-dialog"
					}
					if err := p.Control(context.Background(), request, control); err != nil {
						return err
					}
					return p.Control(context.Background(), request, control)
				}
				if event.Type == "control_result" && mode != "approval" {
					return p.Control(context.Background(), request, provider.Control{ID: "end:steer", Kind: "steer", Message: "finish"})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			results := controlResults(t, events)
			want := "delivered"
			count := 1
			if mode != "approval" {
				want = "failed"
				count = 0
			}
			if results[control.ID].Status != want {
				t.Fatalf("dialog result=%+v", results)
			}
			commands := readFile(t, config.Commands)
			if n := strings.Count(commands, `"type":"extension_ui_response"`); n != count {
				t.Fatalf("response count=%d, commands=%s", n, commands)
			}
			if count > 0 && !strings.Contains(commands, `"id":"pi:dialog/1"`) {
				t.Fatal("response did not use original Pi ID")
			}
			if !hasEvent(events, "tool_execution_start") || (mode == "approval" && !hasEvent(events, "tool_execution_end")) {
				t.Fatal("real tool lifecycle was not preserved")
			}
			args := readFile(t, config.Arguments)
			if !strings.Contains(args, "--no-extensions") || !strings.Contains(args, "--extension") {
				t.Fatalf("approval args: %s", args)
			}
			assertFileText(t, filepath.Join(p.options.DataDir, "jobs", request.AttemptID, "approval.ts"), string(approvalExtension))
		})
	}
}

func TestRPCSentControlCrashNeverResendsAndRecoverySequences(t *testing.T) {
	p, request, config := newRPCProvider(t, "sent-crash")
	control := provider.Control{ID: "send:crash", Kind: "steer", Message: "only once"}
	ctx, cancel := context.WithCancel(context.Background())
	err := p.RunEvents(ctx, request, func(event provider.Event) error {
		if event.Type == "text_delta" {
			if err := p.Control(context.Background(), request, control); err != nil {
				return err
			}
			cancel()
		}
		return nil
	})
	if !errors.Is(err, provider.ErrDetached) {
		t.Fatalf("detach: %v", err)
	}
	waitFor(t, func() bool {
		data, _ := os.ReadFile(config.Commands)
		return strings.Contains(string(data), `"type":"steer"`)
	})
	jobDir := filepath.Join(p.options.DataDir, "jobs", request.AttemptID)
	var marker startedMarker
	if _, err := readJSONFile(filepath.Join(jobDir, "started.json"), &marker); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(marker.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := p.Control(context.Background(), request, control); err != nil {
		t.Fatal(err)
	}
	var events []provider.Event
	err = p.RunEvents(context.Background(), request, func(event provider.Event) error { events = append(events, event); return nil })
	if err == nil || !strings.Contains(err.Error(), "will not be resent") {
		t.Fatalf("recovery: %v", err)
	}
	if result := controlResults(t, events)[control.ID]; result.Status != "unknown" || result.Error == "" {
		t.Fatalf("unacknowledged control=%+v", result)
	}
	if n := strings.Count(readFile(t, config.Commands), `"type":"steer"`); n != 1 {
		t.Fatalf("steer sent %d times", n)
	}
	assertFileText(t, config.Calls, "x\n")
}

func TestRPCWorkerClaimPreventsResendBeforeFirstEvent(t *testing.T) {
	p, request, config := newRPCProvider(t, "silent")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err := p.RunEvents(ctx, request, func(provider.Event) error { return nil })
	cancel()
	if !errors.Is(err, provider.ErrDetached) {
		t.Fatalf("detach: %v", err)
	}
	waitFor(t, func() bool {
		data, _ := os.ReadFile(config.Commands)
		return strings.Contains(string(data), `"type":"prompt"`)
	})
	jobDir := filepath.Join(p.options.DataDir, "jobs", request.AttemptID)
	var marker startedMarker
	if _, err := readJSONFile(filepath.Join(jobDir, "started.json"), &marker); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(marker.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		lock, acquired, _ := tryLock(filepath.Join(jobDir, "worker.lock"))
		if acquired {
			unlock(lock)
		}
		return acquired
	})
	if err := RunWorker(context.Background(), jobDir); err == nil || !strings.Contains(err.Error(), "will not be resent") {
		t.Fatalf("duplicate worker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobDir, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate worker terminalized an orphaned execution: %v", err)
	}
	err = p.RunEvents(context.Background(), request, func(provider.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "will not be resent") {
		t.Fatalf("orphan recovery: %v", err)
	}
	assertFileText(t, config.Calls, "x\n")
}

func TestRPCStallWatchdogToolStreamAndDialogPause(t *testing.T) {
	t.Run("parse-watchdog-timeout", func(t *testing.T) {
		t.Setenv(EnvToolIdleTimeout, "")
		if got := parseWatchdogTimeout(EnvToolIdleTimeout, defaultToolIdleTimeout); got != 0 {
			t.Fatalf("default tool idle timeout = %v, want 0 (disabled by default)", got)
		}
		t.Setenv(EnvToolIdleTimeout, "150ms")
		if got := parseWatchdogTimeout(EnvToolIdleTimeout, defaultToolIdleTimeout); got != 150*time.Millisecond {
			t.Fatalf("duration parse = %v, want 150ms", got)
		}
		t.Setenv(EnvToolIdleTimeout, "45")
		if got := parseWatchdogTimeout(EnvToolIdleTimeout, defaultToolIdleTimeout); got != 45*time.Second {
			t.Fatalf("integer seconds parse = %v, want 45s", got)
		}
		for _, off := range []string{"0", "off", "false", "disabled", "none"} {
			t.Setenv(EnvToolIdleTimeout, off)
			if got := parseWatchdogTimeout(EnvToolIdleTimeout, defaultToolIdleTimeout); got != 0 {
				t.Fatalf("disable %q = %v, want 0", off, got)
			}
		}
	})

	t.Run("stall-tool", func(t *testing.T) {
		t.Setenv(EnvToolIdleTimeout, "150ms")
		t.Setenv(EnvStreamIdleTimeout, "150ms")
		p, request, _ := newRPCProvider(t, "stall-tool")
		var events []provider.Event
		err := p.RunEvents(context.Background(), request, func(event provider.Event) error {
			events = append(events, event)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "stall watchdog") || !strings.Contains(err.Error(), `"bash"`) {
			t.Fatalf("expected tool stall watchdog error mentioning bash, got: %v", err)
		}
		if !hasEvent(events, "text_delta") || !hasEvent(events, "tool_execution_start") {
			t.Fatalf("partial output/tool events lost before stall abort: %+v", events)
		}
	})

	t.Run("stall-stream", func(t *testing.T) {
		t.Setenv(EnvToolIdleTimeout, "150ms")
		t.Setenv(EnvStreamIdleTimeout, "150ms")
		p, request, _ := newRPCProvider(t, "stall-stream")
		var events []provider.Event
		err := p.RunEvents(context.Background(), request, func(event provider.Event) error {
			events = append(events, event)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "stall watchdog") || !strings.Contains(err.Error(), "model stream") {
			t.Fatalf("expected model stream stall watchdog error, got: %v", err)
		}
		if !hasEvent(events, "text_delta") {
			t.Fatalf("partial output lost before stream stall abort: %+v", events)
		}
	})

	t.Run("dialog-pauses-watchdog", func(t *testing.T) {
		t.Setenv(EnvToolIdleTimeout, "150ms")
		t.Setenv(EnvStreamIdleTimeout, "150ms")
		p, request, _ := newRPCProvider(t, "stall-dialog-paused")
		dialogSeen := make(chan struct{}, 1)
		done := make(chan error, 1)
		var events []provider.Event
		go func() {
			done <- p.RunEvents(context.Background(), request, func(event provider.Event) error {
				events = append(events, event)
				if event.Type == "extension_ui_request" {
					select {
					case dialogSeen <- struct{}{}:
					default:
					}
				}
				return nil
			})
		}()
		select {
		case <-dialogSeen:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for extension_ui_request")
		}
		// Wait longer than toolIdleTimeout while the approval dialog is open.
		select {
		case err := <-done:
			t.Fatalf("watchdog aborted while approval dialog was open: %v", err)
		case <-time.After(260 * time.Millisecond):
		}
		if err := p.Control(context.Background(), request, provider.Control{
			ID: "dlg:pause:1", Kind: "response", RequestID: "pi:dialog/1", Confirmed: true,
		}); err != nil {
			t.Fatalf("respond to dialog: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RunEvents after dialog response: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for completion after dialog response")
		}
		if !hasEvent(events, "tool_execution_end") || !hasEvent(events, "agent_settled") {
			t.Fatalf("missing completion events after dialog response: %+v", events)
		}
	})
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for fake RPC")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
