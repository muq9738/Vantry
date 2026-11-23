package desktop

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestTerminalRunRequestParsingAndLifecycle(t *testing.T) {
	prefill, err := json.Marshal(terminalRunPayload{
		Kind:    vantryTerminalRequestKind,
		Version: 1,
		Title:   "Steam Guard 设置",
		Command: "steamguard setup",
		Cwd:     "/tmp/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := api.ProviderRequest{
		ID:       "att-1:ui-term-1",
		ThreadID: "th-1",
		RunID:    "run-1",
		Method:   "editor",
		Title:    "交互终端 · Steam Guard 设置",
		Prefill:  string(prefill),
		Status:   "pending",
	}

	parsed, ok := parseTerminalRunRequest(req)
	if !ok || parsed.Command != "steamguard setup" || parsed.Cwd != "/tmp/project" || parsed.Title != "Steam Guard 设置" {
		t.Fatalf("parseTerminalRunRequest = %+v ok=%v", parsed, ok)
	}
	if _, ok := parseTerminalRunRequest(api.ProviderRequest{Method: "editor", Prefill: "plain text"}); ok {
		t.Fatal("plain editor request must not match vantry_terminal_run")
	}

	service := &fakeService{}
	w := newWorkbench(service)
	w.threads = []api.Thread{{ID: "th-1", Title: "main"}, {ID: "th-bg", Title: "background"}}
	w.runs = []api.Run{{ID: "run-1", ThreadID: "th-1", Status: api.RunRunning}}
	w.selected = "th-1"

	var launched []terminal.Options
	w.newTerminal = pipeTerminals(t, &launched)

	// Background thread request must NOT hijack the active dock.
	bgReq := req
	bgReq.ID = "att-bg:ui-term-bg"
	bgReq.ThreadID = "th-bg"
	w.requests = append(w.requests, bgReq)
	w.afterRequest(bgReq)
	if len(launched) != 0 || w.features.panel == "terminal" {
		t.Fatalf("background thread hijacked terminal dock: launched=%d panel=%q", len(launched), w.features.panel)
	}

	// Active local thread request auto-opens Terminal dock and launches once.
	w.requests = append(w.requests, req)
	w.afterRequest(req)
	w.afterRequest(req) // duplicate snapshot/event must not launch twice
	if len(launched) != 1 {
		t.Fatalf("expected exactly 1 terminal launch, got %d", len(launched))
	}
	if launched[0].Dir != "/tmp/project" || len(launched[0].Command) == 0 {
		t.Fatalf("unexpected terminal options: %+v", launched[0])
	}
	if w.features.panel != "terminal" || w.termTab.active != terminalTaskTabPrefix+req.ID {
		t.Fatalf("expected terminal dock focused on task tab, got panel=%q active=%q", w.features.panel, w.termTab.active)
	}

	// Process exit automatically sends structured exitCode back to Core without terminal screen output.
	w.finishTerminalTaskOnExit(req.ID, 0, "/tmp/project")
	w.finishTerminalTaskOnExit(req.ID, 0, "/tmp/project") // idempotent
	cmds := service.dispatched()
	if len(cmds) != 1 {
		t.Fatalf("expected 1 respond command on exit, got %d (%+v)", len(cmds), cmds)
	}
	cmd := cmds[0]
	if cmd.Kind != api.RespondRequest || cmd.RequestID != req.ID || cmd.Cancelled {
		t.Fatalf("unexpected respond command: %+v", cmd)
	}
	var result terminalRunResult
	if err := json.Unmarshal([]byte(cmd.Value), &result); err != nil {
		t.Fatalf("decode terminal result %q: %v", cmd.Value, err)
	}
	if result.Status != "exited" || result.ExitCode == nil || *result.ExitCode != 0 || result.Cwd != "/tmp/project" {
		t.Fatalf("unexpected terminal result: %+v", result)
	}

	// Cancellation closes the session and responds with Cancelled=true.
	req2 := req
	req2.ID = "att-1:ui-term-2"
	w.requests = append(w.requests, req2)
	w.afterRequest(req2)
	if !w.cancelTerminalTask(req2) {
		t.Fatal("cancelTerminalTask returned false")
	}
	cmds = service.dispatched()
	if len(cmds) != 2 || !cmds[1].Cancelled || cmds[1].RequestID != req2.ID {
		t.Fatalf("expected cancelled response for req2, got %+v", cmds)
	}
}

func TestTerminalRunRealPTYExitResumesAutomaticallyWithoutLeakingOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix PTY test")
	}
	dir := t.TempDir()
	prefill, err := json.Marshal(terminalRunPayload{
		Kind:    vantryTerminalRequestKind,
		Version: 1,
		Title:   "Real PTY Smoke",
		Command: "printf 'TOP_SECRET_OTP_998877\\n'; exit 0",
		Cwd:     dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := api.ProviderRequest{
		ID:       "att-pty:ui-term-1",
		ThreadID: "th-pty",
		RunID:    "run-pty",
		Method:   "editor",
		Title:    "交互终端 · Real PTY Smoke",
		Prefill:  string(prefill),
		Status:   "pending",
	}
	service := &fakeService{}
	w := newWorkbench(service)
	w.threads = []api.Thread{{ID: "th-pty", Title: "pty"}}
	w.runs = []api.Run{{ID: "run-pty", ThreadID: "th-pty", Status: api.RunRunning}}
	w.selected = "th-pty"
	w.requests = []api.ProviderRequest{req}
	defer w.closeTerminalTasks()

	w.afterRequest(req)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cmds := service.dispatched(); len(cmds) > 0 {
			cmd := cmds[0]
			if cmd.Kind != api.RespondRequest || cmd.RequestID != req.ID || cmd.Cancelled {
				t.Fatalf("unexpected respond command: %+v", cmd)
			}
			if strings.Contains(cmd.Value, "TOP_SECRET_OTP_998877") {
				t.Fatalf("terminal screen output leaked into LLM response: %s", cmd.Value)
			}
			var res terminalRunResult
			if err := json.Unmarshal([]byte(cmd.Value), &res); err != nil {
				t.Fatalf("decode response %q: %v", cmd.Value, err)
			}
			if res.Status != "exited" || res.ExitCode == nil || *res.ExitCode != 0 || res.Cwd != dir {
				t.Fatalf("unexpected terminal result: %+v", res)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for real PTY process exit to auto-respond to Core")
}
