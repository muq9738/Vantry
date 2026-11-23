package pi

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// testOwnerEnv names the test binary that spawned a detached fixture. Workers
// are detached on purpose (Setsid) so they outlive Core; in tests that means an
// interrupted, timed-out or killed run would leave them blocked forever.
const testOwnerEnv = "VANTRY_PI_TEST_OWNER_PID"

// exitWithTestOwner ends this fixture process once the test binary that
// started it is gone, however that run ended.
func exitWithTestOwner() {
	owner, err := strconv.Atoi(os.Getenv(testOwnerEnv))
	if err != nil || owner <= 0 {
		return
	}
	go func() {
		for range time.Tick(200 * time.Millisecond) {
			if !processAlive(owner) {
				os.Exit(0)
			}
		}
	}()
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // FindProcess already failed for an exited process.
	}
	err = process.Signal(syscall.Signal(0))
	return !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "pi-rpc-helper" || os.Args[1] == "provider-worker") {
		exitWithTestOwner()
	}
	if len(os.Args) > 1 && os.Args[1] == "pi-rpc-helper" {
		fakeRPCMain()
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "provider-worker" {
		flags := flag.NewFlagSet("provider-worker", flag.ExitOnError)
		jobDir := flags.String("job-dir", "", "")
		_ = flags.Parse(os.Args[2:])
		err := RunWorker(context.Background(), *jobDir)
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	_ = os.Setenv(testOwnerEnv, strconv.Itoa(os.Getpid()))
	os.Exit(m.Run())
}

func TestDurableReplayDetachToolsAndSessionReuse(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "pi-calls")
	arguments := filepath.Join(root, "pi-args")
	fake := writeFakePi(t, root, `
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hel"}}'
sleep 0.5
printf '%s\n' '{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"true"}}'
printf '%s\n' '{"type":"tool_execution_end","toolCallId":"call-1","toolName":"bash","isError":true,"result":{"content":[]}}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"lo"}}'
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`, calls, arguments)
	providerInstance, request := newTestProvider(t, root, fake)
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var firstEvents []provider.Event
	var cursor int64
	err := providerInstance.RunEvents(ctx, request, func(event provider.Event) error {
		mu.Lock()
		firstEvents = append(firstEvents, event)
		mu.Unlock()
		if event.Delta == "Hel" {
			cursor = event.Sequence
			cancel()
		}
		return nil
	})
	if !errors.Is(err, provider.ErrDetached) {
		t.Fatalf("first observer error = %v, want ErrDetached", err)
	}
	if cursor == 0 {
		t.Fatal("observer did not receive the first assistant delta")
	}
	var replayed []provider.Event
	request.AfterSequence = cursor
	if err := providerInstance.RunEvents(context.Background(), request, func(event provider.Event) error {
		replayed = append(replayed, event)
		return nil
	}); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if len(replayed) == 0 || replayed[0].Sequence != cursor+1 {
		t.Fatalf("replay starts at %+v, want sequence %d", replayed, cursor+1)
	}
	var output strings.Builder
	for _, event := range append(append([]provider.Event(nil), firstEvents...), replayed...) {
		output.WriteString(event.Delta)
	}
	if output.String() != "Hello" {
		t.Fatalf("assistant output = %q", output.String())
	}
	if !hasEvent(replayed, "tool_execution_start") || !hasEvent(replayed, "tool_execution_end") {
		t.Fatalf("tool events were not journaled: %+v", replayed)
	}
	assertFileText(t, calls, "x\n")
	args := readFile(t, arguments)
	for _, expected := range []string{"--mode\nrpc", "--session", "--model\nexample-provider/deepseek-v4.1-flash", "--provider\nexample-provider", "--no-extensions", "--no-mcp", "--append-system-prompt", filepath.Join(root, "jobs", request.AttemptID, "instructions.md")} {
		if !strings.Contains(args, expected) {
			t.Errorf("Pi arguments do not contain %q:\n%s", expected, args)
		}
	}
	if strings.Contains(args, "--print") || strings.Contains(args, request.Message) {
		t.Fatalf("prompt was passed as a CLI argument: %s", args)
	}
	jobDir := filepath.Join(root, "jobs", request.AttemptID)
	wantOrchPath := filepath.Join(root, instructionsDirName, request.ThreadID, orchestrationFileName)
	wantVisualPath := filepath.Join(root, instructionsDirName, visualBlocksFileName)
	wantCLIPath := filepath.Join(root, instructionsDirName, request.ThreadID, cliFallbackFileName)
	instructions := readFile(t, filepath.Join(jobDir, "instructions.md"))
	for _, want := range []string{
		"vantry_delegate_task",
		wantOrchPath,
		wantVisualPath,
		request.ThreadID,
	} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions.md missing %q:\n%s", want, instructions)
		}
	}
	for _, forbidden := range []string{"run-1", "attempt-1", "delegate --parent-run 'run-1' --parent-attempt 'attempt-1'", wantCLIPath} {
		if strings.Contains(instructions, forbidden) {
			t.Fatalf("instructions.md should keep volatile/heavy content %q out of permanent prompt:\n%s", forbidden, instructions)
		}
	}
	orchestration := readFile(t, wantOrchPath)
	if !strings.Contains(orchestration, wantCLIPath) {
		t.Fatalf("orchestration.md missing cli-fallback path %q:\n%s", wantCLIPath, orchestration)
	}
	cliFallback := readFile(t, wantCLIPath)
	if !strings.Contains(cliFallback, "delegate --parent-run 'run-1' --parent-attempt 'attempt-1'") {
		t.Fatalf("cli-fallback.md missing delegate fallback command:\n%s", cliFallback)
	}
	visualBlocks := readFile(t, wantVisualPath)
	if !strings.Contains(visualBlocks, "Native Visuals & Structured Replies") {
		t.Fatalf("visual-blocks.md missing full visuals reference:\n%s", visualBlocks)
	}
	var result jobResult
	found, err := readJSONFile(filepath.Join(root, "jobs", request.AttemptID, "result.json"), &result)
	if err != nil || !found || result.Status != "completed" {
		t.Fatalf("durable result = %+v, found=%v, err=%v", result, found, err)
	}

	// Existing jobs persisted an empty policy before Core exposed auto/ask.
	persisted, _, err := readJobRequest(jobDir)
	if err != nil {
		t.Fatal(err)
	}
	persisted.Request.ApprovalPolicy = ""
	if err := writeJSONFile(filepath.Join(jobDir, "request.json"), persisted); err != nil {
		t.Fatal(err)
	}
	request.AfterSequence = result.LastSequence
	if err := providerInstance.RunEvents(context.Background(), request, func(provider.Event) error { t.Fatal("completed cursor replayed an event"); return nil }); err != nil {
		t.Fatalf("reattach old default policy: %v", err)
	}
	assertFileText(t, calls, "x\n")

	// A new attempt on the same thread submits a new prompt but resumes the
	// same Pi session file; replaying attempt one did not invoke Pi again.
	second := request
	second.RunID = "run-2"
	second.AttemptID = "attempt-2"
	second.Message = "continue the parent"
	second.AfterSequence = 0
	if err := providerInstance.RunEvents(context.Background(), second, func(provider.Event) error { return nil }); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	assertFileText(t, calls, "x\nx\n")
	secondArgs := readFile(t, arguments)
	if !strings.Contains(secondArgs, filepath.Join(root, "sessions", request.ThreadID+".jsonl")) {
		t.Fatalf("second attempt did not reuse thread session:\n%s", secondArgs)
	}
}

func TestPersistentStopCancelsWorker(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "pi-calls")
	arguments := filepath.Join(root, "pi-args")
	fake := writeFakePi(t, root, `
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"working"}}'
sleep 30
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`, calls, arguments)
	providerInstance, request := newTestProvider(t, root, fake)
	started := time.Now()
	err := providerInstance.RunEvents(context.Background(), request, func(event provider.Event) error {
		if event.Delta == "working" {
			return providerInstance.Stop(context.Background(), request)
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunEvents error = %v, want cancellation", err)
	}
	if time.Since(started) > 8*time.Second {
		t.Fatal("persistent stop did not interrupt the Pi process group")
	}
	assertFileText(t, calls, "x\n")
	var result jobResult
	_, readErr := readJSONFile(filepath.Join(root, "jobs", request.AttemptID, "result.json"), &result)
	if readErr != nil || result.Status != "cancelled" {
		t.Fatalf("result = %+v, err=%v", result, readErr)
	}
}

func TestStopBeforeFirstAttachPreventsSubmission(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	fake := writeFakePi(t, root, `
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`, calls, filepath.Join(root, "args"))
	providerInstance, request := newTestProvider(t, root, fake)
	if err := providerInstance.Stop(context.Background(), request); err != nil {
		t.Fatalf("persist early stop: %v", err)
	}
	if err := providerInstance.RunEvents(context.Background(), request, func(provider.Event) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunEvents after early stop = %v", err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi was submitted despite early stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", request.AttemptID, "started.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker launched despite early stop: %v", err)
	}
}

func TestExitZeroWithFailedTurnIsFailure(t *testing.T) {
	root := t.TempDir()
	fake := writeFakePi(t, root, `
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"model turn failed"}}'
printf '%s\n' '{"type":"agent_settled","aborted":false}'
exit 0
`, filepath.Join(root, "calls"), filepath.Join(root, "args"))
	providerInstance, request := newTestProvider(t, root, fake)
	var sawErrorEvent bool
	err := providerInstance.RunEvents(context.Background(), request, func(event provider.Event) error {
		if event.Type == "error" {
			sawErrorEvent = true
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "model turn failed") {
		t.Fatalf("RunEvents error = %v", err)
	}
	if !sawErrorEvent {
		t.Fatal("failed Pi turn did not produce a durable error event")
	}
}

func TestSuccessfulRetryClearsTransientTurnFailure(t *testing.T) {
	root := t.TempDir()
	fake := writeFakePi(t, root, `
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"temporary overload"}}'
printf '%s\n' '{"type":"agent_end","willRetry":true}'
printf '%s\n' '{"type":"auto_retry_end","success":true,"attempt":2}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"recovered"}}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}'
printf '%s\n' '{"type":"agent_end","willRetry":false}'
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`, filepath.Join(root, "calls"), filepath.Join(root, "args"))
	providerInstance, request := newTestProvider(t, root, fake)
	var output strings.Builder
	if err := providerInstance.RunEvents(context.Background(), request, func(event provider.Event) error {
		output.WriteString(event.Delta)
		return nil
	}); err != nil {
		t.Fatalf("successful retry remained failed: %v", err)
	}
	if output.String() != "recovered" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestFinalRetryErrorOverridesTransientFailure(t *testing.T) {
	root := t.TempDir()
	fake := writeFakePi(t, root, `
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"temporary overload"}}'
printf '%s\n' '{"type":"agent_end","willRetry":true}'
printf '%s\n' '{"type":"auto_retry_end","success":false,"attempt":3,"finalError":"final provider failure"}'
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`, filepath.Join(root, "calls"), filepath.Join(root, "args"))
	providerInstance, request := newTestProvider(t, root, fake)
	err := providerInstance.RunEvents(context.Background(), request, func(provider.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "final provider failure") {
		t.Fatalf("final retry error = %v", err)
	}
}

func TestWorkerCrashNeverRelaunchesOrResends(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "worker-calls")
	worker := filepath.Join(root, "crashing-worker")
	script := "#!/bin/sh\nprintf 'x\\n' >> " + shellQuote(calls) + "\nkill -9 $$\n"
	if err := os.WriteFile(worker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	providerInstance, request := newTestProvider(t, root, filepath.Join(root, "unused-pi"))
	providerInstance.options.Executable = worker
	var firstErr error
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		firstErr = providerInstance.RunEvents(ctx, request, func(provider.Event) error { return nil })
		cancel()
		if firstErr != nil && !errors.Is(firstErr, provider.ErrDetached) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for crashed worker result")
		}
	}
	if firstErr == nil || !strings.Contains(firstErr.Error(), "will not be resent") {
		t.Fatalf("crash error = %v", firstErr)
	}
	secondErr := providerInstance.RunEvents(context.Background(), request, func(provider.Event) error { return nil })
	if secondErr == nil || secondErr.Error() != firstErr.Error() {
		t.Fatalf("second crash attachment error = %v, first %v", secondErr, firstErr)
	}
	assertFileText(t, calls, "x\n")
}

func TestRealPiOptIn(t *testing.T) {
	if os.Getenv("VANTRY_REAL_PI_TEST") != "1" {
		t.Skip("set VANTRY_REAL_PI_TEST=1 to use the installed Pi and its existing credentials")
	}
	root := t.TempDir()
	providerInstance, request := newTestProvider(t, root, "pi")
	request.Message = "Reply with exactly VANTRY_REAL_PI_OK. Do not use tools or delegate tasks."
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	var output strings.Builder
	err := providerInstance.RunEvents(ctx, request, func(event provider.Event) error {
		output.WriteString(event.Delta)
		return nil
	})
	cancel()
	if errors.Is(err, provider.ErrDetached) {
		_ = providerInstance.Stop(context.Background(), request)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = providerInstance.RunEvents(cleanupCtx, request, func(provider.Event) error { return nil })
	}
	if err != nil {
		t.Fatalf("installed Pi run failed: %v", err)
	}
	if !strings.Contains(output.String(), "VANTRY_REAL_PI_OK") {
		t.Fatalf("installed Pi output did not contain the requested marker: %q", output.String())
	}
}

func TestWorkerKilledAfterPiSubmissionDoesNotResend(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "pi-calls")
	arguments := filepath.Join(root, "pi-args")
	piPIDPath := filepath.Join(root, "pi-pid")
	fake := writeFakePi(t, root, `
printf '%s\n' "$$" > `+shellQuote(piPIDPath)+`
sleep 30
`, calls, arguments)
	providerInstance, request := newTestProvider(t, root, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { _ = providerInstance.Stop(context.Background(), request) })
	observed := make(chan error, 1)
	go func() {
		observed <- providerInstance.RunEvents(ctx, request, func(provider.Event) error { return nil })
	}()
	// Wait for actual prompt acceptance, not a fixed startup budget that races
	// the other packages under -race on a busy machine.
	waitFor(t, func() bool {
		data, err := os.ReadFile(piPIDPath)
		return err == nil && strings.TrimSpace(string(data)) != ""
	})
	cancel()
	if err := <-observed; !errors.Is(err, provider.ErrDetached) {
		t.Fatalf("initial observer error = %v", err)
	}
	assertFileText(t, calls, "x\n")
	piPID, err := strconv.Atoi(strings.TrimSpace(readFile(t, piPIDPath)))
	if err != nil || piPID <= 1 {
		t.Fatalf("fake Pi pid = %d, %v", piPID, err)
	}
	defer syscall.Kill(-piPID, syscall.SIGKILL)
	var started startedMarker
	found, err := readJSONFile(filepath.Join(root, "jobs", request.AttemptID, "started.json"), &started)
	if err != nil || !found || started.PID <= 1 {
		t.Fatalf("started marker = %+v, found=%v, err=%v", started, found, err)
	}
	if err := syscall.Kill(started.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	lockCtx, lockCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	unexpectedLock, lockErr := workspace.LockDirectory(lockCtx, providerInstance.options.DefaultWorkspace.Path)
	lockCancel()
	if lockErr == nil {
		workspace.Unlock(unexpectedLock)
		t.Fatal("worker SIGKILL released the live Pi workspace lock")
	}
	var crashErr error
	deadline := time.Now().Add(10 * time.Second)
	for {
		observeCtx, observeCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		crashErr = providerInstance.RunEvents(observeCtx, request, func(provider.Event) error { return nil })
		observeCancel()
		if crashErr != nil && !errors.Is(crashErr, provider.ErrDetached) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for killed worker")
		}
	}
	if crashErr == nil || !strings.Contains(crashErr.Error(), "will not be resent") {
		t.Fatalf("killed worker error = %v", crashErr)
	}
	if err := providerInstance.RunEvents(context.Background(), request, func(provider.Event) error { return nil }); err == nil {
		t.Fatal("reattachment unexpectedly restarted a completed crash result")
	}
	assertFileText(t, calls, "x\n")
	members, err := processGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.group == piPID {
			t.Fatalf("worker recovery left a live Pi/tool group: %+v", member)
		}
	}
}

func TestDefaultProviderRouting(t *testing.T) {
	registry := New(Options{DefaultProviderID: ProviderControlled})
	request := provider.Request{Message: "controlled"}
	if registry.Recoverable(request) {
		t.Fatal("default controlled provider is recoverable")
	}
	var events []provider.Event
	if err := registry.RunEvents(context.Background(), request, func(event provider.Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("controlled RunEvents: %v", err)
	}
	if len(events) == 0 || events[0].Sequence != 1 {
		t.Fatalf("controlled events = %+v", events)
	}
	piRequest := request
	piRequest.ProviderID = ProviderPI
	if !registry.Recoverable(piRequest) {
		t.Fatal("explicit pi request did not bypass controlled default")
	}
	if err := registry.RunEvents(context.Background(), piRequest, func(provider.Event) error { return nil }); err == nil {
		t.Fatal("explicit pi request silently used controlled provider")
	}
	if !New(Options{}).Recoverable(request) {
		t.Fatal("empty provider and empty default must route to pi")
	}
}

func newTestProvider(t *testing.T, root, fakePi string) (*Provider, provider.Request) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	workspacePath := filepath.Join(root, "directory")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	instance := New(Options{
		DataDir: root, Executable: executable, PiExecutable: fakePi,
		SocketPath: filepath.Join(root, "core.sock"), DefaultModel: "example-provider/deepseek-v4.1-flash",
		DefaultWorkspace: &api.WorkspaceSpec{Kind: KindDirectoryForTest, Path: workspacePath},
	})
	return instance, provider.Request{ThreadID: "thread-1", RunID: "run-1", AttemptID: "attempt-1", Message: "do durable work"}
}

const KindDirectoryForTest = "directory"

func writeFakePi(t *testing.T, root, body, calls, arguments string) string {
	t.Helper()
	path := filepath.Join(root, "fake-pi")
	script := "#!/bin/sh\nprintf 'x\\n' >> " + shellQuote(calls) + "\nprintf '%s\\n' \"$@\" > " + shellQuote(arguments) + `
IFS= read -r state || exit 81
printf '%s\n' '{"id":"vantry:state","type":"response","command":"get_state","success":true,"data":{}}'
IFS= read -r prompt || exit 82
printf '%s\n' '{"id":"vantry:prompt","type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}'
` + body + `
# RPC processes stay alive until the worker closes stdin after settled.
while IFS= read -r unused; do :; done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasEvent(events []provider.Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertFileText(t *testing.T, path, expected string) {
	t.Helper()
	if actual := readFile(t, path); actual != expected {
		t.Fatalf("%s = %q, want %q", path, actual, expected)
	}
}
