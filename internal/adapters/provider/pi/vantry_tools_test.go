package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func findPiExtensionLoader(piExecutable string) (string, bool) {
	candidates := []string{
		filepath.Join(filepath.Dir(piExecutable), "core", "extensions", "loader.js"),
		filepath.Join(filepath.Dir(piExecutable), "..", "core", "extensions", "loader.js"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

func TestInstalledPiLoadsVantryToolsExtensionInRPC(t *testing.T) {
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		t.Skip("installed Pi not available")
	}
	root := t.TempDir()
	toolsExt := filepath.Join(root, "vantry_tools.ts")
	if err := os.WriteFile(toolsExt, vantryToolsExtension, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		piExecutable,
		"--mode", "rpc",
		"--no-session",
		"--no-extensions",
		"--no-mcp",
		"--extension", toolsExt,
	)
	command.Dir = root
	command.Env = piEnvironment(piExecutable)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = command.Wait() }()

	if err := writeRPC(stdin, map[string]string{"type": "get_state", "id": "vantry-tools-check"}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("Pi startup with vantry_tools.ts failed: %v stderr=%s", err, stderr.String())
		}
		var record rpcRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == "response" && record.ID == "vantry-tools-check" {
			if !record.Success {
				t.Fatalf("get_state failed with vantry_tools.ts: %s stderr=%s", line, stderr.String())
			}
			break
		}
	}
	_ = stdin.Close()
	_, _ = io.Copy(io.Discard, reader)
	if err := command.Wait(); err != nil {
		t.Fatalf("Pi EOF shutdown failed: %v stderr=%s", err, stderr.String())
	}
}

func TestVantryToolsNativeExecutionAndSecretIsolation(t *testing.T) {
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		t.Skip("installed Pi not available")
	}
	loader, ok := findPiExtensionLoader(piExecutable)
	if !ok {
		t.Skip("installed Pi does not expose core/extensions/loader.js")
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	toolsExt := filepath.Join(root, "vantry_tools.ts")
	if err := os.WriteFile(toolsExt, vantryToolsExtension, 0o600); err != nil {
		t.Fatal(err)
	}
	approvalExt := filepath.Join(root, "approval.ts")
	if err := os.WriteFile(approvalExt, approvalExtension, 0o600); err != nil {
		t.Fatal(err)
	}

	ctlLog := filepath.Join(root, "ctl-invocations.jsonl")
	shellPwned := filepath.Join(root, "shell-injection-ran")
	fakeVantry := filepath.Join(root, "fake-vantry")
	fakeVantryScript := `#!/usr/bin/env node
import fs from "node:fs";

const args = process.argv.slice(2);
fs.appendFileSync(process.env.FAKE_CTL_LOG, JSON.stringify(args) + "\n");

const action = args[3];
if (action === "delegate") {
  console.log(JSON.stringify({
    commandId: "cmd_delegate_1",
    accepted: true,
    threadId: "th_child_1",
    runId: "run_child_1",
    taskId: "task_native_1",
    generation: 1
  }));
} else if (action === "status") {
  console.log(JSON.stringify({
    sequence: 42,
    threads: [{ id: "thread-parent", title: "Parent" }, { id: "thread-other", title: "Other" }],
    runs: [{ id: "run-parent", threadId: "thread-parent", status: "running" }],
    tasks: [
      {
        id: "task_native_1",
        parentThreadId: "thread-parent",
        parentRunId: "run-parent",
        childThreadId: "th_child_1",
        childRunId: "run_child_1",
        title: "Audit API",
        generation: 1,
        status: "completed",
        deliveryStatus: "delivered",
        deliveryMessageId: "deliv_1",
        result: "A".repeat(4500),
        updatedAt: "2026-04-01T00:00:00Z"
      },
      {
        id: "task_native_2",
        parentThreadId: "thread-parent",
        parentRunId: "run-older-parent",
        childThreadId: "th_child_2",
        childRunId: "run_child_2",
        title: "Check tests",
        generation: 1,
        status: "running",
        deliveryStatus: "pending",
        updatedAt: "2026-04-01T00:01:00Z"
      },
      {
        id: "task_unrelated",
        parentThreadId: "thread-other",
        parentRunId: "run-other",
        childThreadId: "th_other",
        childRunId: "run_other_child",
        title: "Unrelated thread task",
        generation: 1,
        status: "completed",
        deliveryStatus: "acknowledged",
        result: "should not appear",
        updatedAt: "2026-04-01T00:02:00Z"
      }
    ],
    cohorts: [
      { parentRunId: "run-parent", generation: 1, resumePolicy: "manual", deliveryMessageId: "deliv_1", status: "delivered", updatedAt: "2026-04-01T00:00:00Z" },
      { parentRunId: "run-other", generation: 1, resumePolicy: "auto", status: "acknowledged", updatedAt: "2026-04-01T00:02:00Z" }
    ]
  }));
} else if (action === "followup") {
  console.log(JSON.stringify({
    commandId: "cmd_followup_1",
    accepted: true,
    taskId: args[args.indexOf("--task") + 1],
    runId: "run_child_2"
  }));
} else if (action === "ack") {
  console.log(JSON.stringify({
    commandId: "cmd_ack_1",
    accepted: true,
    generation: Number(args[args.indexOf("--generation") + 1]),
    deliveryMessageId: args[args.indexOf("--delivery") + 1]
  }));
} else {
  console.error("unexpected ctl action:", action);
  process.exit(2);
}
`
	if err := os.WriteFile(fakeVantry, []byte(fakeVantryScript), 0o700); err != nil {
		t.Fatal(err)
	}

	jobDir := filepath.Join(root, "jobs", "attempt-parent")
	if err := makePrivateDir(jobDir); err != nil {
		t.Fatal(err)
	}

	script := `
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const { loadExtensions } = await import(pathToFileURL(process.argv[1]).href);
const loaded = await loadExtensions([process.argv[2], process.argv[3]], process.cwd());
assert.deepEqual(loaded.errors, []);

const toolsExt = loaded.extensions[0];
const approvalExt = loaded.extensions[1];
const approvalHandler = approvalExt.handlers.get("tool_call")[0];

const expectedTools = [
  "vantry_delegate_task",
  "vantry_task_status",
  "vantry_task_followup",
  "vantry_task_ack",
  "vantry_request_secret",
];
for (const name of expectedTools) {
  assert.ok(toolsExt.tools.has(name), "missing tool " + name);
  // Even under strict ask + plan mode, vantry_* tools are never blocked by approval.ts!
  const gate = await approvalHandler(
    { toolName: name, input: { message: "test" } },
    { hasUI: false },
  );
  assert.equal(gate, undefined, name + " should bypass bash/file approval gate");
}

// 1. Test vantry_delegate_task with shell metacharacters (must NOT run through shell)
const trickyMessage = "audit 'single' \"double\" $(touch " + process.env.SHELL_PWNED_PATH + ") \x60false\x60\nline2";
const delegateDef = toolsExt.tools.get("vantry_delegate_task").definition;
const delegateRes = await delegateDef.execute(
  "call-1",
  { title: "Security Check", message: trickyMessage, model: "magpie/deepseek", resumePolicy: "manual" },
  undefined,
  undefined,
  { hasUI: true, cwd: process.cwd() },
);
const delegateJson = JSON.parse(delegateRes.content[0].text);
assert.equal(delegateJson.taskId, "task_native_1");
assert.equal(fs.existsSync(process.env.SHELL_PWNED_PATH), false, "shell injection must never execute");

// 2. Test vantry_task_status filtering & truncation
const statusDef = toolsExt.tools.get("vantry_task_status").definition;
const statusRes = await statusDef.execute("call-2", {}, undefined, undefined, { hasUI: true, cwd: process.cwd() });
const statusJson = JSON.parse(statusRes.content[0].text);
assert.equal(statusJson.taskCount, 2);
assert.deepEqual(statusJson.tasks.map((t) => t.id), ["task_native_1", "task_native_2"]);
assert.equal(statusJson.tasks[0].resultTruncated, true);
assert.ok(statusJson.tasks[0].result.length < 4200);
assert.equal(statusJson.cohorts.length, 1);
assert.equal(statusJson.cohorts[0].parentRunId, "run-parent");

const singleStatusRes = await statusDef.execute(
  "call-2b",
  { taskId: "task_native_2" },
  undefined,
  undefined,
  { hasUI: true, cwd: process.cwd() },
);
const singleStatusJson = JSON.parse(singleStatusRes.content[0].text);
assert.equal(singleStatusJson.taskCount, 1);
assert.equal(singleStatusJson.tasks[0].id, "task_native_2");

// 3. Test vantry_task_followup
const followupDef = toolsExt.tools.get("vantry_task_followup").definition;
const followupRes = await followupDef.execute(
  "call-3",
  { taskId: "task_native_1", message: "verify edge case" },
  undefined,
  undefined,
  { hasUI: true, cwd: process.cwd() },
);
assert.equal(JSON.parse(followupRes.content[0].text).accepted, true);

// 4. Test vantry_task_ack
const ackDef = toolsExt.tools.get("vantry_task_ack").definition;
const ackRes = await ackDef.execute(
  "call-4",
  { generation: 1, deliveryMessageId: "deliv_1" },
  undefined,
  undefined,
  { hasUI: true, cwd: process.cwd() },
);
assert.equal(JSON.parse(ackRes.content[0].text).deliveryMessageId, "deliv_1");

// 5. Test vantry_request_secret (strictly redacted return, 0700 dir, 0600 file)
const secretDef = toolsExt.tools.get("vantry_request_secret").definition;
const sensitiveSecret = ["synthetic", "input", "marker", "998877"].join("-");
let capturedPrompt = "";
let capturedPlaceholder = null;
const secretRes = await secretDef.execute(
  "call-5",
  { prompt: "Enter deployment token", envVarName: "DEPLOY_TOKEN" },
  undefined,
  undefined,
  {
    hasUI: true,
    cwd: process.cwd(),
    ui: {
      input: async (title, placeholder) => {
        capturedPrompt = title;
        capturedPlaceholder = placeholder;
        return sensitiveSecret;
      },
    },
  },
);
assert.equal(capturedPrompt, "Enter deployment token");
assert.equal(capturedPlaceholder, "");

const rawReturned = JSON.stringify(secretRes);
assert.equal(rawReturned.includes(sensitiveSecret), false, "plaintext secret leaked into tool return!");

const secretPayload = JSON.parse(secretRes.content[0].text);
assert.equal(typeof secretPayload.secretFilePath, "string");
assert.equal(typeof secretPayload.usageHint, "string");
assert.ok(secretPayload.usageHint.includes("DEPLOY_TOKEN"));
assert.equal(secretPayload.usageHint.includes(sensitiveSecret), false);

const secretsDir = path.dirname(secretPayload.secretFilePath);
assert.equal(secretsDir, path.join(process.env.VANTRY_JOB_DIR, "secrets"));
assert.equal(fs.statSync(secretsDir).mode & 0o777, 0o700);
assert.equal(fs.statSync(secretPayload.secretFilePath).mode & 0o777, 0o600);
assert.equal(fs.readFileSync(secretPayload.secretFilePath, "utf8"), sensitiveSecret);

// Cancelled input and missing UI must reject without leaking or writing empty files
const filesBefore = fs.readdirSync(secretsDir).length;
await assert.rejects(() =>
  secretDef.execute(
    "call-5b",
    { prompt: "Enter token" },
    undefined,
    undefined,
    { hasUI: true, cwd: process.cwd(), ui: { input: async () => undefined } },
  ),
);
await assert.rejects(() =>
  secretDef.execute(
    "call-5c",
    { prompt: "Enter token" },
    undefined,
    undefined,
    { hasUI: false, cwd: process.cwd() },
  ),
);
await assert.rejects(() =>
  secretDef.execute(
    "call-5d",
    { prompt: "Enter token", envVarName: "BAD;ENV" },
    undefined,
    undefined,
    { hasUI: true, cwd: process.cwd(), ui: { input: async () => sensitiveSecret } },
  ),
);
assert.equal(fs.readdirSync(secretsDir).length, filesBefore);

console.log("vantry-tools-ok");
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, loader, toolsExt, approvalExt)
	node.Dir = root
	node.Env = append(
		piEnvironment(piExecutable),
		"FAKE_CTL_LOG="+ctlLog,
		"SHELL_PWNED_PATH="+shellPwned,
		EnvSocketPath+"=", // CLI fixture must not inherit the host agent's live Core socket.
		EnvExecutable+"="+fakeVantry,
		EnvDataDir+"="+root,
		EnvJobDir+"="+jobDir,
		EnvThreadID+"=thread-parent",
		EnvRunID+"=run-parent",
		EnvAttemptID+"=attempt-parent",
		EnvApprovalPolicy+"=ask",
		EnvInteractionMode+"=plan",
	)
	output, err := node.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "vantry-tools-ok") {
		t.Fatalf("vantry_tools execution test failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "synthetic-input-marker-998877") {
		t.Fatal("sensitive secret value appeared in process output")
	}

	// Verify exact recorded CLI invocations
	lines := strings.Split(strings.TrimSpace(readFile(t, ctlLog)), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 ctl calls, got %d: %v", len(lines), lines)
	}
	var firstCall []string
	if err := json.Unmarshal([]byte(lines[0]), &firstCall); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"ctl", "--data-dir", root, "delegate", "--parent-run", "run-parent", "--parent-attempt", "attempt-parent"}
	for i, want := range wantPrefix {
		if i >= len(firstCall) || firstCall[i] != want {
			t.Fatalf("delegate argv[%d] = %q, want %q (full=%v)", i, firstCall[i], want, firstCall)
		}
	}
}

func TestWorkerUserExtensionsAndMCPCompatibilitySwitches(t *testing.T) {
	t.Run("default-isolated", func(t *testing.T) {
		root := t.TempDir()
		calls := filepath.Join(root, "calls")
		argsFile := filepath.Join(root, "args")
		envFile := filepath.Join(root, "env")
		fake := writeFakePiWithEnvCapture(t, root, calls, argsFile, envFile)

		instance, request := newTestProvider(t, root, fake)
		if err := instance.RunEvents(context.Background(), request, nilReceiver); err != nil {
			t.Fatalf("default run: %v", err)
		}
		args := readFile(t, argsFile)
		if !strings.Contains(args, "--no-extensions") || !strings.Contains(args, "--no-mcp") {
			t.Fatalf("default worker must pass --no-extensions and --no-mcp:\n%s", args)
		}
		if strings.Contains(args, "--exclude-tools") {
			t.Fatalf("without user extensions there is nothing to exclude:\n%s", args)
		}
		expectedToolsPath := filepath.Join(root, "jobs", request.AttemptID, "vantry_tools.ts")
		if !strings.Contains(args, "--extension\n"+expectedToolsPath) {
			t.Fatalf("worker did not inject vantry_tools.ts:\n%s", args)
		}
		envText := readFile(t, envFile)
		for _, expectedEnv := range []string{
			EnvDataDir + "=" + root,
			EnvSocketPath + "=" + filepath.Join(root, "core.sock"),
			EnvJobDir + "=" + filepath.Join(root, "jobs", request.AttemptID),
			EnvThreadID + "=thread-1",
			EnvRunID + "=run-1",
			EnvAttemptID + "=attempt-1",
			EnvApprovalPolicy + "=auto",
		} {
			if !strings.Contains(envText, expectedEnv) {
				t.Fatalf("worker env missing %q:\n%s", expectedEnv, envText)
			}
		}
		instructions := readFile(t, filepath.Join(root, "jobs", request.AttemptID, "instructions.md"))
		for _, expectedSnippet := range []string{
			"vantry_delegate_task",
			"vantry_request_secret",
			"orchestration.md",
			"visual-blocks.md",
			"Ordinary coding tasks need no Vantry orchestration tools.",
			"Delegation goes through Vantry only.",
			"Never fall back to native subagents, scripted workflows",
			"Run a scripted workflow only when the user asks for one.",
		} {
			if !strings.Contains(instructions, expectedSnippet) {
				t.Fatalf("instructions missing %q:\n%s", expectedSnippet, instructions)
			}
		}
		orchestration := readFile(t, filepath.Join(root, instructionsDirName, request.ThreadID, orchestrationFileName))
		for _, expectedOrchSnippet := range []string{
			"vantry_environment_list",
			"vantry_orchestrator_capabilities",
			"vantry_task_status",
			"vantry_task_cancel",
			"vantry_thread_list",
			"vantry_thread_search",
			"vantry_thread_read",
			"vantry_thread_send",
			"vantry_thread_wait",
			"vantry_thread_launch",
			"vantry_create_threads",
			"vantry_thread_transfer",
			"vantry_thread_manage",
			"vantry_task_followup",
			"vantry_task_ack",
			"vantry-thread://",
			"cli-fallback.md",
		} {
			if !strings.Contains(orchestration, expectedOrchSnippet) {
				t.Fatalf("orchestration.md missing %q:\n%s", expectedOrchSnippet, orchestration)
			}
		}
	})

	t.Run("options-enabled", func(t *testing.T) {
		root := t.TempDir()
		calls := filepath.Join(root, "calls")
		argsFile := filepath.Join(root, "args")
		envFile := filepath.Join(root, "env")
		fake := writeFakePiWithEnvCapture(t, root, calls, argsFile, envFile)

		instance, request := newTestProvider(t, root, fake)
		instance.options.EnableUserExtensions = true
		instance.options.EnableUserMCP = true
		if err := instance.RunEvents(context.Background(), request, nilReceiver); err != nil {
			t.Fatalf("options-enabled run: %v", err)
		}
		args := readFile(t, argsFile)
		if strings.Contains(args, "--no-extensions") || strings.Contains(args, "--no-mcp") {
			t.Fatalf("enabled user ecosystem still passed --no-extensions/--no-mcp:\n%s", args)
		}
		if !strings.Contains(args, "vantry_tools.ts") {
			t.Fatalf("vantry_tools.ts missing when user extensions enabled:\n%s", args)
		}
		// User extensions load, but their native child tools stay off by default.
		if !strings.Contains(args, "--exclude-tools\n"+nativeSubagentTools+"\n") {
			t.Fatalf("native subagent tools must be excluded by default:\n%s", args)
		}
	})

	t.Run("native-subagents-opt-in", func(t *testing.T) {
		t.Setenv(EnvNativeSubagents, "1")
		root := t.TempDir()
		calls := filepath.Join(root, "calls")
		argsFile := filepath.Join(root, "args")
		envFile := filepath.Join(root, "env")
		fake := writeFakePiWithEnvCapture(t, root, calls, argsFile, envFile)

		instance, request := newTestProvider(t, root, fake)
		instance.options.EnableUserExtensions = true
		if err := instance.RunEvents(context.Background(), request, nilReceiver); err != nil {
			t.Fatalf("opt-in run: %v", err)
		}
		if args := readFile(t, argsFile); strings.Contains(args, "--exclude-tools") {
			t.Fatalf("opt-in must keep native subagent tools:\n%s", args)
		}
		instructions := readFile(t, filepath.Join(root, "jobs", request.AttemptID, "instructions.md"))
		if !strings.Contains(instructions, "never start an async or background native child") {
			t.Fatalf("opt-in instructions must forbid detached native children:\n%s", instructions)
		}
	})

	t.Run("env-override-enabled", func(t *testing.T) {
		t.Setenv(EnvUserExtensions, "1")
		t.Setenv(EnvUserMCP, "1")
		root := t.TempDir()
		calls := filepath.Join(root, "calls")
		argsFile := filepath.Join(root, "args")
		envFile := filepath.Join(root, "env")
		fake := writeFakePiWithEnvCapture(t, root, calls, argsFile, envFile)

		instance, request := newTestProvider(t, root, fake)
		if err := instance.RunEvents(context.Background(), request, nilReceiver); err != nil {
			t.Fatalf("env-override run: %v", err)
		}
		args := readFile(t, argsFile)
		if strings.Contains(args, "--no-extensions") || strings.Contains(args, "--no-mcp") {
			t.Fatalf("env override failed to remove --no-extensions/--no-mcp:\n%s", args)
		}
	})

	t.Run("mcp-enabled-with-no-user-extensions", func(t *testing.T) {
		root := t.TempDir()
		calls := filepath.Join(root, "calls")
		argsFile := filepath.Join(root, "args")
		envFile := filepath.Join(root, "env")
		fake := writeFakePiWithEnvCapture(t, root, calls, argsFile, envFile)

		instance, request := newTestProvider(t, root, fake)
		instance.options.EnableUserExtensions = false
		instance.options.EnableUserMCP = true
		if err := instance.RunEvents(context.Background(), request, nilReceiver); err != nil {
			t.Fatalf("mcp-only run: %v", err)
		}
		args := readFile(t, argsFile)
		if !strings.Contains(args, "--no-extensions") {
			t.Fatalf("expected --no-extensions:\n%s", args)
		}
		if strings.Contains(args, "--no-mcp") {
			t.Fatalf("unexpected --no-mcp when EnableUserMCP=true:\n%s", args)
		}
		for _, want := range []string{
			"--extension\nbuiltin:mcp\n",
			"--extension\nbuiltin:codemode\n",
			"--extension\nbuiltin:tool-search\n",
			"--tools\n+codemode,+tool_search\n",
		} {
			if !strings.Contains(args, want) {
				t.Fatalf("missing %q in args:\n%s", want, args)
			}
		}
		instructions := readFile(t, filepath.Join(root, "jobs", request.AttemptID, "instructions.md"))
		for _, want := range []string{"describeNamespace", "searchTools", "tools.mcp__"} {
			if !strings.Contains(instructions, want) {
				t.Fatalf("MCP codemode instructions missing %q:\n%s", want, instructions)
			}
		}
	})
}

func nilReceiver(provider.Event) error { return nil }

func writeFakePiWithEnvCapture(t *testing.T, root, calls, arguments, envOut string) string {
	t.Helper()
	body := "env > " + shellQuote(envOut) + "\nprintf '%s\\n' '{\"type\":\"agent_settled\",\"aborted\":false}'\n"
	return writeFakePi(t, root, body, calls, arguments)
}

type recordedSocketCall struct {
	Method string
	Path   string
	Raw    string
}

func decodeStrictJSON(r *http.Request, dst any) (string, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return string(body), err
	}
	return string(body), nil
}

func TestVantryToolsUDSSocketSuiteAndNativeSubagents(t *testing.T) {
	t.Setenv(EnvDirectOrchestration, "")
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		t.Skip("installed Pi not available")
	}
	loader, ok := findPiExtensionLoader(piExecutable)
	if !ok {
		t.Skip("installed Pi does not expose core/extensions/loader.js")
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	toolsExt := filepath.Join(root, "vantry_tools.ts")
	if err := os.WriteFile(toolsExt, vantryToolsExtension, 0o600); err != nil {
		t.Fatal(err)
	}
	approvalExt := filepath.Join(root, "approval.ts")
	if err := os.WriteFile(approvalExt, approvalExtension, 0o600); err != nil {
		t.Fatal(err)
	}

	// Keep Unix socket path well below macOS 104-byte sun_path limit.
	sockDir, err := os.MkdirTemp("", "vt-uds-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sockDir, 0o700); err != nil {
		_ = os.RemoveAll(sockDir)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socketPath := filepath.Join(sockDir, "c.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		calls    []recordedSocketCall
		commands []api.Command
	)

	writeResp := func(w http.ResponseWriter, status int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orchestration/capabilities", func(w http.ResponseWriter, r *http.Request) {
		var query api.OrchestratorCapabilitiesQuery
		raw, err := decodeStrictJSON(r, &query)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		writeResp(w, http.StatusOK, api.OrchestratorCapabilities{
			ParentThreadID:      query.CallerThreadID,
			InheritedProviderID: "pi",
			InheritedModel:      "magpie/deepseek-v4",
			InheritedThinking:   "high",
			ApprovalPolicy:      "auto",
			InteractionMode:     "default",
			Models: []api.OrchestratorModelCapability{
				{
					ID:    "magpie/deepseek-v4",
					Label: "DeepSeek V4",
					Options: []api.OrchestratorModelOption{
						{ID: "thinking", Values: []string{"low", "medium", "high"}, Default: "high"},
					},
				},
			},
			ApprovalPolicies: []string{"auto", "auto-accept-edits", "ask"},
			InteractionModes: []string{"default", "plan"},
			MaxBatchThreads:  20,
		})
	})

	mux.HandleFunc("/v1/orchestration/delegate", func(w http.ResponseWriter, r *http.Request) {
		var req api.DelegateTaskRequest
		raw, err := decodeStrictJSON(r, &req)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		status := api.RunRunning
		workState := "working"
		summary := ""
		next := api.OrchestratorRunningTaskNext
		if req.Mode == "wait" {
			status = api.RunCompleted
			workState = "result_available"
			summary = "worktree task done: " + req.Task
			next = ""
		}
		writeResp(w, http.StatusOK, api.TaskDetail{
			TaskID:         "task_uds_1",
			ParentThreadID: req.CallerThreadID,
			ParentRunID:    req.ParentRunID,
			ChildThreadID:  "th_child_uds",
			Link:           api.FormatThreadLink("th_child_uds", req.Title),
			ChildRunID:     "run_child_uds",
			Title:          req.Title,
			Status:         status,
			WorkState:      workState,
			DeliveryStatus: api.DeliveryDelivered,
			Model:          req.Model,
			Summary:        summary,
			Next:           next,
			UpdatedAt:      time.Now().UTC(),
		})
	})

	mux.HandleFunc("/v1/orchestration/task-status", func(w http.ResponseWriter, r *http.Request) {
		var query api.TaskStatusQuery
		raw, err := decodeStrictJSON(r, &query)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		if query.TaskID == "task_missing" {
			writeResp(w, http.StatusNotFound, api.Error{Code: api.NotFound, Message: "task not found"})
			return
		}
		parentRunID := query.ParentRunID
		if query.TaskID == "task_older_parent" {
			if query.ParentRunID != "" {
				writeResp(w, http.StatusConflict, api.Error{Code: api.Conflict, Message: "task belongs to a different parent run"})
				return
			}
			parentRunID = "run-older-parent"
		}
		writeResp(w, http.StatusOK, api.TaskStatusResult{
			ThreadID: query.CallerThreadID,
			RunID:    query.ParentRunID,
			Tasks: []api.TaskDetail{
				{
					TaskID:         nonEmpty(query.TaskID, "task_uds_1"),
					ParentThreadID: query.CallerThreadID,
					ParentRunID:    parentRunID,
					ChildThreadID:  "th_child_uds",
					Link:           api.FormatThreadLink("th_child_uds", "Child Task"),
					Status:         api.RunCompleted,
					WorkState:      "result_available",
					DeliveryStatus: api.DeliveryAcknowledged,
					Summary:        "verified",
				},
			},
		})
	})

	mux.HandleFunc("/v1/orchestration/task-cancel", func(w http.ResponseWriter, r *http.Request) {
		var req api.CancelTaskRequest
		raw, err := decodeStrictJSON(r, &req)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		writeResp(w, http.StatusOK, api.CancelTaskResult{
			TaskID: req.TaskID,
			Status: "cancelled",
		})
	})

	mux.HandleFunc("/v1/orchestration/threads/list", func(w http.ResponseWriter, r *http.Request) {
		var query api.ThreadListQuery
		raw, err := decodeStrictJSON(r, &query)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		writeResp(w, http.StatusOK, api.ThreadListResult{
			CurrentThreadID: query.CallerThreadID,
			Total:           1,
			Threads: []api.ThreadListItem{
				{
					ThreadID: "th_peer_api",
					Title:    "API Peer",
					Link:     api.FormatThreadLink("th_peer_api", "API Peer"),
					Status:   "running",
					Settled:  false,
					Snoozed:  false,
				},
			},
		})
	})

	mux.HandleFunc("/v1/orchestration/threads/search", func(w http.ResponseWriter, r *http.Request) {
		var query api.SearchQuery
		raw, err := decodeStrictJSON(r, &query)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		writeResp(w, http.StatusOK, []api.SearchHit{
			{
				ThreadID: "th_peer_api",
				RunID:    "run_peer_1",
				Title:    "API Peer",
				Field:    api.SearchOutput,
				Snippet:  "matched " + query.Query,
				Score:    10,
			},
		})
	})

	mux.HandleFunc("/v1/orchestration/threads/read", func(w http.ResponseWriter, r *http.Request) {
		var query api.ThreadReadQuery
		raw, err := decodeStrictJSON(r, &query)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		afterPos := -1
		if query.AfterPosition != nil {
			afterPos = *query.AfterPosition
		}
		nextPos := afterPos + 2
		writeResp(w, http.StatusOK, api.ThreadReadResult{
			Thread: api.ThreadListItem{
				ThreadID: query.ThreadID,
				Title:    "API Peer",
				Link:     api.FormatThreadLink(query.ThreadID, "API Peer"),
				Status:   "running",
			},
			RecentRuns: []api.ThreadRunSummary{
				{RunID: "run_peer_active", Ordinal: 2, Status: api.RunRunning},
			},
			Items: []api.ThreadTimelineItem{
				{
					Position:       afterPos + 1,
					ItemID:         "item_1",
					RunID:          "run_peer_active",
					CreatedBy:      "agent",
					SenderThreadID: query.CallerThreadID,
					Type:           "assistant_message",
					Status:         "completed",
					Text:           "peer reply after " + string(rune('0'+max(0, afterPos%10))),
				},
			},
			NextPosition: &nextPos,
			HasMore:      false,
		})
	})

	mux.HandleFunc("/v1/orchestration/threads/send", func(w http.ResponseWriter, r *http.Request) {
		var req api.InterThreadSendRequest
		raw, err := decodeStrictJSON(r, &req)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		delivery := "started"
		if req.Mode == "steer" {
			delivery = "steered"
		} else if req.Mode == "queue" {
			delivery = "queued"
		}
		writeResp(w, http.StatusOK, api.InterThreadSendResult{
			ThreadID: req.TargetThreadID,
			Link:     api.FormatThreadLink(req.TargetThreadID, "API Peer"),
			RunID:    "run_peer_active",
			Status:   api.RunRunning,
			Delivery: delivery,
			Sequence: 77,
		})
	})

	mux.HandleFunc("/v1/orchestration/threads/wait", func(w http.ResponseWriter, r *http.Request) {
		var query api.ThreadWaitQuery
		raw, err := decodeStrictJSON(r, &query)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		writeResp(w, http.StatusOK, api.ThreadWaitResult{
			ThreadID: query.ThreadID,
			Link:     api.FormatThreadLink(query.ThreadID, "API Peer"),
			RunID:    nonEmpty(query.RunID, "run_peer_active"),
			Status:   "completed",
			TimedOut: false,
			Summary:  "peer settled cleanly",
		})
	})

	mux.HandleFunc("/v1/orchestration/threads/launch", func(w http.ResponseWriter, r *http.Request) {
		var req api.ThreadLaunchRequest
		raw, err := decodeStrictJSON(r, &req)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		writeResp(w, http.StatusOK, api.LaunchedThread{
			ThreadID: "th_launched_1",
			Link:     api.FormatThreadLink("th_launched_1", req.Title),
			Title:    req.Title,
			RunID:    "run_launched_1",
			Status:   api.RunQueued,
			Model:    req.Model,
		})
	})

	mux.HandleFunc("/v1/orchestration/threads/create-batch", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateThreadsRequest
		raw, err := decodeStrictJSON(r, &req)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		mu.Unlock()
		launched := make([]api.LaunchedThread, 0, len(req.Threads))
		for i, spec := range req.Threads {
			id := "th_batch_" + string(rune('1'+i))
			launched = append(launched, api.LaunchedThread{
				ThreadID: id,
				Link:     api.FormatThreadLink(id, spec.Title),
				Title:    spec.Title,
				Status:   api.RunQueued,
			})
		}
		writeResp(w, http.StatusOK, api.CreateThreadsResult{Threads: launched})
	})

	mux.HandleFunc("/v1/commands", func(w http.ResponseWriter, r *http.Request) {
		var cmd api.Command
		raw, err := decodeStrictJSON(r, &cmd)
		if err != nil {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.InvalidCommand, Message: err.Error()})
			return
		}
		if cmd.Version != api.ProtocolVersion {
			writeResp(w, http.StatusBadRequest, api.Error{Code: api.VersionMismatch, Message: "bad version"})
			return
		}
		mu.Lock()
		calls = append(calls, recordedSocketCall{Method: r.Method, Path: r.URL.Path, Raw: raw})
		commands = append(commands, cmd)
		mu.Unlock()

		threadID := cmd.ThreadID
		if cmd.Kind == api.ForkThread {
			threadID = "th_forked_child"
		}
		writeResp(w, http.StatusOK, api.Receipt{
			CommandID:         cmd.CommandID,
			Accepted:          true,
			ThreadID:          threadID,
			RunID:             cmd.RunID,
			TaskID:            cmd.TaskID,
			Generation:        cmd.Generation,
			DeliveryMessageID: cmd.DeliveryMessageID,
			RequestID:         cmd.RequestID,
			Sequence:          101,
		})
	})

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	script := `
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import path from "node:path";
import { pathToFileURL } from "node:url";

const { loadExtensions } = await import(pathToFileURL(process.argv[1]).href);
const loaded = await loadExtensions([process.argv[2], process.argv[3]], process.cwd());
assert.deepEqual(loaded.errors, []);

const toolsExt = loaded.extensions[0];
const approvalExt = loaded.extensions[1];
const toolsGuard = async (event, ctx) => {
  for (const handler of toolsExt.handlers.get("tool_call") ?? []) {
    const result = await handler(event, ctx);
    if (result?.block) return result;
  }
  return undefined;
};
const approvalGuard = approvalExt.handlers.get("tool_call")[0];

// Also verify mock pi objects without pi.on still register cleanly
const loaderDir = path.dirname(process.argv[1]);
const loaderRequire = createRequire(pathToFileURL(process.argv[1]).href);
const jitiMod = await import(pathToFileURL(path.join(loaderDir, "jiti-loader.js")).href).catch(() => null);
if (jitiMod?.createJiti) {
  const jiti = jitiMod.createJiti(pathToFileURL(process.argv[1]).href, {
    alias: {
      "@earendil-works/pi-ai": path.join(loaderDir, "pi-ai-extension-view.js"),
      typebox: loaderRequire.resolve("typebox"),
    },
  });
  const mod = await jiti.import(process.argv[2]);
  const minimalRegistered = [];
  mod.default({ registerTool: (tool) => minimalRegistered.push(tool.name) });
  assert.equal(minimalRegistered.length, 18);

  // Verify Pure Pi default vs Project Coordinator vs Child exposure gating.
  const collectExposure = (envPatch) => {
    const prev = {};
    for (const [k, v] of Object.entries(envPatch)) {
      prev[k] = process.env[k];
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
    try {
      const registered = [];
      mod.default({ registerTool: (tool) => registered.push(tool) });
      return registered;
    } finally {
      for (const [k, v] of Object.entries(prev)) {
        if (v === undefined) delete process.env[k];
        else process.env[k] = v;
      }
    }
  };
  const pureDefault = collectExposure({ VANTRY_DIRECT_ORCHESTRATION: undefined, VANTRY_PARENT_TASK_ID: undefined });
  assert.deepEqual(
    pureDefault.filter((t) => t.exposure === "direct").map((t) => t.name),
    [],
  );
  assert.equal(pureDefault.find((t) => t.name === "vantry_request_secret")?.exposure, "model-only");
  assert.equal(pureDefault.find((t) => t.name === "vantry_terminal_run")?.exposure, "model-only");
  assert.equal(pureDefault.filter((t) => t.exposure === "deferred").length, 16);

  // Unit tests for evaluateToolSearchGuard (prevents KV cache bust on irrelevant queries).
  const activeDefaults = ["read", "bash", "edit", "write", "codemode", "tool_search", "vantry_request_secret", "vantry_terminal_run"];
  const allForSearch = [
    ...pureDefault,
    { name: "bash", description: "Execute bash commands", exposure: "direct" },
    { name: "mcp__magpie__magpie-search__web_search", description: "Search the web via Exa", exposure: "codemode" },
  ];
  const blockedTerminal = mod.evaluateToolSearchGuard(
    "Vantry cmd interactive terminal PTY launch local command tty interactive user input",
    allForSearch,
    activeDefaults,
  );
  assert.equal(blockedTerminal.allow, false);
  assert.match(blockedTerminal.reason, /vantry_terminal_run/);

  const blockedGeneric = mod.evaluateToolSearchGuard("vantry local command ui", allForSearch, activeDefaults);
  assert.equal(blockedGeneric.allow, false);

  assert.equal(mod.evaluateToolSearchGuard("vantry_thread_read", allForSearch, activeDefaults).allow, true);
  assert.equal(mod.evaluateToolSearchGuard("vantry_environment_list linked target environment list", allForSearch, activeDefaults).allow, true);
  assert.equal(mod.evaluateToolSearchGuard("web_search exa", allForSearch, activeDefaults).allow, true);

  // Unit tests for evaluateContinuityGuard (catches steer-dropped execution and dangling next steps).
  const steerDropped = mod.evaluateContinuityGuard({
    assistantText: "对，就在你的 Mac 上执行，不需要 qmt。刚才我误用了 buff-steam enroll。\n\n接下来只做 Mac 本地的 Steam 会话续登，不查 BUFF、不走 qmt。",
    lastUserText: "不是本地执行吗",
    sawMidRunUserMessage: true,
    toolCallsInAttempt: 8,
    toolCallsSinceLastUser: 0,
  });
  assert.equal(steerDropped.shouldContinue, true);

  const danglingDeferred = mod.evaluateContinuityGuard({
    assistantText: "刚才那轮登录超时了，会话还没保存。\n\n后续应让程序直接用 Mac 托管的令牌完成验证码这一步。",
    lastUserText: "我真的服了",
    sawMidRunUserMessage: false,
    toolCallsInAttempt: 3,
    toolCallsSinceLastUser: 3,
  });
  assert.equal(danglingDeferred.shouldContinue, true);
  assert.equal(danglingDeferred.reason, "dangling_next_step");

  const steerCorrectionOnly = mod.evaluateContinuityGuard({
    assistantText: "是我选错入口，应该跳过 BUFF 直接续登本地会话。",
    lastUserText: "为什么要 qmt 啊",
    sawMidRunUserMessage: true,
    toolCallsInAttempt: 5,
    toolCallsSinceLastUser: 0,
  });
  assert.equal(steerCorrectionOnly.shouldContinue, true);
  assert.equal(steerCorrectionOnly.reason, "steer_dropped_execution");

  // Legitimate user handoffs, questions, and stop commands must NOT trigger continuation.
  assert.equal(
    mod.evaluateContinuityGuard({
      assistantText: "完成后告诉我「绑好了」。我会先核对文件权限，再做只读验证。",
      lastUserText: "帮我启动设置",
      sawMidRunUserMessage: false,
      toolCallsInAttempt: 4,
      toolCallsSinceLastUser: 4,
    }).shouldContinue,
    false,
  );
  assert.equal(
    mod.evaluateContinuityGuard({
      assistantText: "已停止，接下来不再执行任何修改。",
      lastUserText: "停下别做了",
      sawMidRunUserMessage: true,
      toolCallsInAttempt: 4,
      toolCallsSinceLastUser: 0,
    }).shouldContinue,
    false,
  );
  assert.equal(
    mod.evaluateContinuityGuard({
      assistantText: "接下来要做哪一步？\n\x60\x60\x60ask\n选择方案\n* 方案 A | 推荐\n- 方案 B | 备选\n\x60\x60\x60",
      lastUserText: "看下方案",
      sawMidRunUserMessage: false,
      toolCallsInAttempt: 1,
      toolCallsSinceLastUser: 1,
    }).shouldContinue,
    false,
  );

  const coordinatorTools = collectExposure({ VANTRY_DIRECT_ORCHESTRATION: "1", VANTRY_PARENT_TASK_ID: undefined });
  assert.deepEqual(
    coordinatorTools.filter((t) => t.exposure === "direct").map((t) => t.name).sort(),
    ["vantry_delegate_task", "vantry_orchestrator_capabilities", "vantry_task_status"],
  );

  const childWithCoordEnv = collectExposure({ VANTRY_DIRECT_ORCHESTRATION: "1", VANTRY_PARENT_TASK_ID: "task_1" });
  assert.deepEqual(
    childWithCoordEnv.filter((t) => t.exposure === "direct").map((t) => t.name),
    [],
  );

  // Unit tests for pruneToolResultText (spill + head+tail + GBK/HTML hints) & ToolLoopGuard.
  const fsSync = await import("node:fs");
  const underPrune = await mod.pruneToolResultText("small ok output", { toolName: "bash", jobDir: process.env.VANTRY_JOB_DIR });
  assert.equal(underPrune.changed, false);

  const oversizedText =
    "<!DOCTYPE html><html><head></head><body><div><span><a><b><u><i>" +
    "START_HEAD_MARKER\n" +
    "\uFFFD".repeat(25) +
    "B".repeat(20_000) +
    "\nEND_TAIL_MARKER</body></html>";
  const overPrune = await mod.pruneToolResultText(oversizedText, {
    toolName: "bash",
    toolCallId: "call/guard:1",
    jobDir: process.env.VANTRY_JOB_DIR,
  });
  assert.equal(overPrune.changed, true);
  assert.ok(overPrune.spillPath && overPrune.spillPath.includes("tool-outputs"));
  assert.equal(fsSync.statSync(overPrune.spillPath).mode & 0o777, 0o600);
  assert.equal(fsSync.readFileSync(overPrune.spillPath, "utf8"), oversizedText);
  assert.ok(overPrune.text.includes("START_HEAD_MARKER"));
  assert.ok(overPrune.text.includes("END_TAIL_MARKER"));
  assert.ok(overPrune.text.includes("[Encoding hint:") && overPrune.text.includes("gb18030"));
  assert.ok(overPrune.text.includes("[HTML hint:"));

  const guard = new mod.ToolLoopGuard();
  assert.equal(guard.observe({ toolName: "bash", input: { command: "c1" }, text: "err", isError: true }), undefined);
  assert.equal(guard.observe({ toolName: "bash", input: { command: "c2" }, text: "err", isError: true }), undefined);
  const tFail = guard.observe({ toolName: "bash", input: { command: "c3" }, text: "err", isError: true });
  assert.equal(tFail?.kind, "consecutive_failures");
  assert.equal(tFail?.count, 3);

  // Cross-tool failure streak triggers at 5.
  const streakGuard = new mod.ToolLoopGuard();
  for (let i = 0; i < 4; i++) {
    assert.equal(streakGuard.observe({ toolName: "tool" + i, input: { n: i }, text: "fail", isError: true }), undefined);
  }
  const tStreak = streakGuard.observe({ toolName: "tool4", input: { n: 4 }, text: "fail", isError: true });
  assert.equal(tStreak?.kind, "failure_streak");

  // Edit-test cycle must NOT false-positive as repeated_call.
  const editTestGuard = new mod.ToolLoopGuard();
  for (let i = 0; i < 4; i++) {
    assert.equal(editTestGuard.observe({ toolName: "bash", input: { command: "go test" }, text: "ok", isError: false }), undefined);
    assert.equal(editTestGuard.observe({ toolName: "edit", input: { path: "a.go", oldText: "1", newText: "2" }, text: "ok", isError: false }), undefined);
  }

  // Identical repeated non-error calls trigger on 3rd.
  const repeatGuard = new mod.ToolLoopGuard();
  assert.equal(repeatGuard.observe({ toolName: "bash", input: { command: "curl x" }, text: "blocked", isError: false }), undefined);
  assert.equal(repeatGuard.observe({ toolName: "bash", input: { command: "curl x" }, text: "blocked", isError: false }), undefined);
  const tRepeat = repeatGuard.observe({ toolName: "bash", input: { command: "curl x" }, text: "blocked", isError: false });
  assert.equal(tRepeat?.kind, "repeated_call");
}

const expectedTools = [
  "vantry_orchestrator_capabilities",
  "vantry_delegate_task",
  "vantry_task_status",
  "vantry_task_cancel",
  "vantry_task_followup",
  "vantry_task_ack",
  "vantry_thread_list",
  "vantry_thread_search",
  "vantry_thread_read",
  "vantry_thread_send",
  "vantry_thread_wait",
  "vantry_thread_launch",
  "vantry_create_threads",
  "vantry_thread_transfer",
  "vantry_thread_manage",
  "vantry_request_secret",
];
for (const name of expectedTools) {
  assert.ok(toolsExt.tools.has(name), "missing tool " + name);
  const blockedByTools = await toolsGuard({ toolName: name, input: {} }, { hasUI: false });
  assert.equal(blockedByTools, undefined);
  const blockedByApproval = await approvalGuard({ toolName: name, input: {} }, { hasUI: false });
  assert.equal(blockedByApproval, undefined, name + " must bypass approval.ts in plan+ask mode");
}

// Native provider tools and application-owned delegation are separate. The
// bridge must not ban native tools or force them into Vantry delegation.
for (const nativeName of ["subagent", "subagent_supervisor", "bg_wait"]) {
  assert.equal(await toolsGuard({ toolName: nativeName, input: {} }, { hasUI: true }), undefined);
}
const directTools = [...toolsExt.tools.entries()]
  .filter(([, tool]) => tool.definition.exposure === "direct")
  .map(([name]) => name).sort();
assert.deepEqual(directTools, []);
assert.equal(toolsExt.tools.get("vantry_request_secret")?.definition.exposure, "model-only");
assert.equal(toolsExt.tools.get("vantry_terminal_run")?.definition.exposure, "model-only");

// Verify vantry_terminal_run uses structured editor UI request and never leaks secrets.
const termDef = toolsExt.tools.get("vantry_terminal_run").definition;
let capturedTermEditor = null;
const termExited = await termDef.execute(
  "term-call-1",
  { command: "steamguard setup", title: "Steam Guard 设置", cwd: "." },
  undefined,
  undefined,
  {
    hasUI: true,
    cwd: process.cwd(),
    ui: {
      editor: async (title, prefill) => {
        capturedTermEditor = { title, prefill };
        return JSON.stringify({ status: "exited", exitCode: 0, cwd: process.cwd() });
      },
    },
  },
);
assert.ok(capturedTermEditor && capturedTermEditor.title.includes("Steam Guard 设置"));
const parsedPrefill = JSON.parse(capturedTermEditor.prefill);
assert.equal(parsedPrefill.kind, "vantry_terminal_run");
assert.equal(parsedPrefill.command, "steamguard setup");
assert.equal(parsedPrefill.cwd, process.cwd());
const parsedTermOut = JSON.parse(termExited.content[0].text);
assert.equal(parsedTermOut.status, "exited");
assert.equal(parsedTermOut.exitCode, 0);

const termCancelled = await termDef.execute(
  "term-call-2",
  { command: "steamguard setup" },
  undefined,
  undefined,
  {
    hasUI: true,
    cwd: process.cwd(),
    ui: { editor: async () => undefined },
  },
);
assert.equal(JSON.parse(termCancelled.content[0].text).status, "cancelled");

// Verify agent_before_settle continuity guard nudges at most ONCE per attempt.
const runHook = async (name, ev) => {
  let out;
  for (const h of toolsExt.handlers.get(name) ?? []) {
    const res = await h(ev, { hasUI: true });
    if (res !== undefined) out = res;
  }
  return out;
};
const prevMode = process.env.VANTRY_INTERACTION_MODE;
process.env.VANTRY_INTERACTION_MODE = "default";
try {
  await runHook("agent_start", {});
  await runHook("message_end", { message: { role: "user", content: "那你做掉啊" } });
  await runHook("tool_execution_start", { toolName: "bash", toolCallId: "b1" });
  await runHook("message_end", { message: { role: "user", content: "不是本地执行吗" } });
  const settleEvent = {
    type: "agent_before_settle",
    outcome: "completed",
    entries: [],
    continue: false,
    context: {
      canContinue: true,
      contextMessages: [
        { role: "assistant", stopReason: "stop", content: [{ type: "text", text: "接下来只做 Mac 本地的 Steam 会话续登，不走 qmt。" }] },
      ],
    },
  };
  const firstNudge = await runHook("agent_before_settle", settleEvent);
  assert.equal(firstNudge?.continue, true);
  assert.equal(firstNudge?.entries?.length, 1);
  assert.equal(firstNudge?.entries?.[0]?.customType, "vantry_continuity_guard");
  assert.equal(firstNudge?.entries?.[0]?.display, false);
  const secondNudge = await runHook("agent_before_settle", settleEvent);
  assert.equal(secondNudge, undefined, "Continuity guard must nudge at most once per attempt");
} finally {
  if (prevMode === undefined) delete process.env.VANTRY_INTERACTION_MODE;
  else process.env.VANTRY_INTERACTION_MODE = prevMode;
}

assert.equal(await toolsGuard({ toolName: "read", input: { path: "README.md" } }, { hasUI: false }), undefined);

// Verify tool_result handler preserves images, skips read/vantry_*, and keeps JSON valid on vantry_* loop hints.
const toolResultHandlers = toolsExt.handlers.get("tool_result") ?? [];
assert.ok(toolResultHandlers.length >= 1);
const runToolResult = async (event) => {
  let current = event;
  for (const h of toolResultHandlers) {
    const out = await h(current);
    if (out) {
      current = { ...current, ...out };
    }
  }
  return current;
};
const withImage = await runToolResult({
  toolName: "bash",
  toolCallId: "img-big-1",
  input: { command: "dump" },
  content: [{ type: "text", text: "IMG_HEAD\n" + "Z".repeat(18_000) + "\nIMG_TAIL" }, { type: "image", data: "abc", mimeType: "image/png" }],
  details: {},
  isError: false,
});
assert.equal(withImage.content.length, 2);
assert.equal(withImage.content[1].type, "image");
assert.ok(withImage.content[0].text.includes("IMG_HEAD") && withImage.content[0].text.includes("IMG_TAIL"));
const skippedRead = await runToolResult({
  toolName: "read",
  toolCallId: "read-big-1",
  input: { path: "big.txt" },
  content: [{ type: "text", text: "R".repeat(18_000) }],
  details: {},
  isError: false,
});
assert.equal(skippedRead.details?.vantryPruned, undefined);

for (let i = 0; i < 3; i++) {
  const vantryRepeat = await runToolResult({
    toolName: "vantry_task_status",
    toolCallId: "vt-" + i,
    input: { taskId: "same" },
    content: [{ type: "text", text: "{\"tasks\":[]}" }],
    details: {},
    isError: false,
  });
  assert.deepEqual(JSON.parse(vantryRepeat.content[0].text), { tasks: [] });
  if (i === 2) {
    assert.equal(vantryRepeat.content.length, 2);
    assert.match(vantryRepeat.content[1].text, /Loop guard:/);
  }
}

const callTool = async (name, params) => {
  const def = toolsExt.tools.get(name).definition;
  const res = await def.execute("call-" + name, params, undefined, undefined, {
    hasUI: true,
    cwd: process.cwd(),
  });
  return JSON.parse(res.content[0].text);
};

// 1. Capabilities keep the selected environment in the wire request.
const caps = await callTool("vantry_orchestrator_capabilities", { search: "deepseek", environmentId: "paired-host" });
assert.equal(caps.parentThreadId, "th_caller_main");
assert.equal(caps.maxBatchThreads, 20);

// 2. Delegate task with the 'task' field + isolation: 'worktree' + mode: 'wait'
const delWait = await callTool("vantry_delegate_task", {
  title: "Worktree Fix",
  task: "fix race in parser",
  environmentId: "paired-host",
  workspace: { kind: "directory", path: "/remote/repository" },
  role: "implementation",
  model: "magpie/deepseek-v4",
  thinking: "high",
  isolation: "worktree",
  mode: "wait",
  timeoutMs: 30000,
  approvalPolicy: "auto-accept-edits",
  interactionMode: "plan",
  clientRequestId: "req_del_wait",
});
assert.equal(delWait.taskId, "task_uds_1");
assert.equal(delWait.workState, "result_available");
assert.equal(delWait.summary, "worktree task done: fix race in parser");

// Also verify legacy 'message' field works over UDS
const delAsync = await callTool("vantry_delegate_task", {
  title: "Async Research",
  message: "inspect schema",
  isolation: "shared",
  mode: "async",
});
assert.equal(delAsync.workState, "working");
assert.ok(delAsync.next.includes("Do not poll"));

// 3. Task status & error surfacing
const taskStatus = await callTool("vantry_task_status", { taskId: "task_uds_1", acknowledge: true });
assert.equal(taskStatus.tasks[0].deliveryStatus, "acknowledged");
// A new user turn must still be able to inspect a task delegated by an older
// run in this thread; explicit task IDs must not inherit the current run filter.
const olderTaskStatus = await callTool("vantry_task_status", { taskId: "task_older_parent" });
assert.equal(olderTaskStatus.tasks[0].parentRunId, "run-older-parent");
const currentTaskStatus = await callTool("vantry_task_status", {});
assert.equal(currentTaskStatus.runId, process.env.VANTRY_RUN_ID);
await assert.rejects(
  () => callTool("vantry_task_status", { taskId: "task_missing" }),
  /Vantry Core not_found.*task not found/,
);

// 4. Task cancel, followup, ack
const cancelled = await callTool("vantry_task_cancel", {
  taskId: "task_uds_1",
  reason: "superseded by narrower approach",
  clientRequestId: "req_cancel_1",
});
assert.equal(cancelled.status, "cancelled");

const followup = await callTool("vantry_task_followup", {
  taskId: "task_uds_1",
  message: "also check edge case",
  clientRequestId: "req_followup_1",
});
assert.equal(followup.accepted, true);

const ack = await callTool("vantry_task_ack", {
  generation: 2,
  deliveryMessageId: "deliv_uds_2",
  clientRequestId: "req_ack_2",
});
assert.equal(ack.deliveryMessageId, "deliv_uds_2");

// 5. Thread list & search
const listed = await callTool("vantry_thread_list", {
  environmentId: "paired-host",
  workspace: "/repo/vantry",
  allProjects: false,
  includeSubagents: true,
  settled: false,
  snoozed: false,
  statuses: ["running", "idle"],
  titleContains: "API",
  cursor: 0,
  limit: 10,
});
assert.equal(listed.currentThreadId, "th_caller_main");
assert.equal(listed.threads[0].threadId, "th_peer_api");

const hits = await callTool("vantry_thread_search", {
  query: "orchestration",
  includeArchived: true,
  limit: 5,
});
assert.equal(hits.length, 1);
assert.equal(hits[0].threadId, "th_peer_api");

// 6. Thread read (including afterPosition: 0 and afterPosition: 5)
const readInitial = await callTool("vantry_thread_read", {
  environmentId: "paired-host",
  threadId: "th_peer_api",
  view: "messages",
  afterPosition: 0,
  limit: 20,
  runLimit: 3,
  maxCharsPerItem: 2000,
});
assert.equal(readInitial.items[0].position, 1);
assert.equal(readInitial.nextPosition, 2);

const readNext = await callTool("vantry_thread_read", {
  threadId: "th_peer_api",
  view: "activity",
  afterPosition: 5,
});
assert.equal(readNext.items[0].position, 6);

// 7. Inter-session conversation via vantry_thread_send (auto & steer) + vantry_thread_wait
const sendAuto = await callTool("vantry_thread_send", {
  threadId: "th_peer_api",
  message: "Can you share the updated endpoint schema?",
  mode: "auto",
  clientRequestId: "req_send_auto",
});
assert.equal(sendAuto.delivery, "started");

const sendSteer = await callTool("vantry_thread_send", {
  threadId: "th_peer_api",
  message: "Use POST instead of GET for thread queries.",
  mode: "steer",
  clientRequestId: "req_send_steer",
});
assert.equal(sendSteer.delivery, "steered");

const waited = await callTool("vantry_thread_wait", {
  threadId: "th_peer_api",
  runId: "run_peer_active",
  timeoutMs: 5000,
  untilDelegatedWorkSettled: true,
});
assert.equal(waited.status, "completed");
assert.equal(waited.summary, "peer settled cleanly");

// 8. Peer thread launch & batch create_threads
const launched = await callTool("vantry_thread_launch", {
  title: "Peer Verifier",
  message: "run integration checks",
  model: "magpie/deepseek-v4",
  thinking: "medium",
  workspaceKind: "worktree",
  repository: "/repo/vantry",
  branch: "feat/peer-check",
  baseRef: "main",
  clientRequestId: "req_launch_1",
});
assert.equal(launched.threadId, "th_launched_1");

const batch = await callTool("vantry_create_threads", {
  threads: [
    { title: "Reviewer A", message: "audit transport" },
    { title: "Reviewer B", message: "audit ui", interactionMode: "plan" },
  ],
  clientRequestId: "req_batch_1",
});
assert.equal(batch.threads.length, 2);

await assert.rejects(
  () =>
    callTool("vantry_create_threads", {
      threads: Array.from({ length: 21 }, (_, idx) => ({ title: "T" + idx })),
    }),
  /at most 20 threads/,
);

// 9. Thread transfer (fork & merge_back)
const forked = await callTool("vantry_thread_transfer", {
  action: "fork",
  threadId: "th_peer_api",
  sourceRunId: "run_peer_1",
  title: "API Peer Experiment",
  clientRequestId: "req_fork_1",
});
assert.equal(forked.threadId, "th_forked_child");

const merged = await callTool("vantry_thread_transfer", {
  action: "merge_back",
  threadId: "th_forked_child",
  targetThreadId: "th_caller_main",
  clientRequestId: "req_merge_1",
});
assert.equal(merged.accepted, true);

// 10. Thread manage actions
await callTool("vantry_thread_manage", { action: "rename", threadId: "th_peer_api", title: "Renamed Peer" });
await callTool("vantry_thread_manage", { action: "pin", threadId: "th_peer_api" });
await callTool("vantry_thread_manage", { action: "settle", threadId: "th_peer_api" });
await callTool("vantry_thread_manage", {
  action: "snooze",
  threadId: "th_peer_api",
  snoozedUntil: "2026-05-01T10:00:00Z",
});
await callTool("vantry_thread_manage", { action: "unsnooze", threadId: "th_peer_api" });
await callTool("vantry_thread_manage", {
  action: "set_model",
  threadId: "th_peer_api",
  model: "magpie/deepseek-v4",
});
// Interrupt without runId resolves active run via threads/read
await callTool("vantry_thread_manage", { action: "interrupt", threadId: "th_peer_api" });
await callTool("vantry_thread_manage", {
  action: "respond_request",
  threadId: "th_peer_api",
  runId: "run_peer_active",
  requestId: "pi:dialog/99",
  confirmed: true,
});

console.log("vantry-uds-suite-ok");
`

	jobDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, loader, toolsExt, approvalExt)
	node.Dir = root
	node.Env = append(
		piEnvironment(piExecutable),
		EnvJobDir+"="+jobDir,
		EnvSocketPath+"="+socketPath,
		EnvThreadID+"=th_caller_main",
		EnvRunID+"=run_caller_main",
		EnvAttemptID+"=att_caller_main",
		EnvApprovalPolicy+"=approval-required",
		EnvInteractionMode+"=plan",
	)
	output, err := node.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "vantry-uds-suite-ok") {
		t.Fatalf("UDS suite failed: %v\n%s", err, output)
	}

	mu.Lock()
	recordedCmds := append([]api.Command(nil), commands...)
	recordedCalls := append([]recordedSocketCall(nil), calls...)
	mu.Unlock()

	if len(recordedCalls) < 18 {
		t.Fatalf("expected >= 18 UDS HTTP calls, got %d", len(recordedCalls))
	}

	// Verify InterThreadSend wire payload carries callerThreadId + targetThreadId
	var sawAutoSend, sawSteerSend bool
	for _, c := range recordedCalls {
		if c.Path == "/v1/orchestration/threads/send" {
			var sendReq api.InterThreadSendRequest
			if err := json.Unmarshal([]byte(c.Raw), &sendReq); err != nil {
				t.Fatal(err)
			}
			if sendReq.CallerThreadID != "th_caller_main" || sendReq.TargetThreadID != "th_peer_api" {
				t.Fatalf("unexpected InterThreadSendRequest: %+v", sendReq)
			}
			if sendReq.Mode == "auto" {
				sawAutoSend = true
			}
			if sendReq.Mode == "steer" {
				sawSteerSend = true
			}
		}
	}
	if !sawAutoSend || !sawSteerSend {
		t.Fatalf("expected both auto and steer inter-thread sends; auto=%v steer=%v", sawAutoSend, sawSteerSend)
	}

	for _, endpoint := range []string{"/v1/orchestration/capabilities", "/v1/orchestration/delegate", "/v1/orchestration/threads/list", "/v1/orchestration/threads/read"} {
		seen := false
		for _, call := range recordedCalls {
			if call.Path != endpoint {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(call.Raw), &body); err != nil {
				t.Fatal(err)
			}
			if body["environmentId"] == "paired-host" {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("Pi tool lost target environment for %s", endpoint)
		}
	}

	// Verify ForkThread, MergeBackThread, snooze/unsnooze, StopRun, and RespondRequest commands
	kindsSeen := map[api.CommandKind]int{}
	var sawUnsnoozeZero bool
	for _, cmd := range recordedCmds {
		kindsSeen[cmd.Kind]++
		if cmd.Kind == api.UpdateThread && cmd.SnoozedUntil != nil && cmd.SnoozedUntil.IsZero() {
			sawUnsnoozeZero = true
		}
		if cmd.Kind == api.StopRun && cmd.RunID != "run_peer_active" {
			t.Fatalf("interrupt resolved wrong runId: %+v", cmd)
		}
	}
	for _, wantKind := range []api.CommandKind{
		api.FollowUpTask,
		api.AcknowledgeTask,
		api.ForkThread,
		api.MergeBackThread,
		api.UpdateThread,
		api.SetThreadModel,
		api.StopRun,
		api.RespondRequest,
	} {
		if kindsSeen[wantKind] == 0 {
			t.Fatalf("expected command kind %q in %+v", wantKind, kindsSeen)
		}
	}
	if !sawUnsnoozeZero {
		t.Fatal("unsnooze did not dispatch zero snoozedUntil timestamp")
	}
}

func TestInterSessionPromptFraming(t *testing.T) {
	t.Run("unit-framing-without-mutating-input", func(t *testing.T) {
		dataDir := t.TempDir()
		originalMessage := "Please check the database schema changes."
		job := jobRequest{
			DataDir: dataDir,
			Request: provider.Request{
				ThreadID:       "th_receiver",
				SenderThreadID: "th_sender_peer",
				SenderTitle:    "Schema Reviewer",
				RunID:          "run_1",
				AttemptID:      "att_1",
				Message:        originalMessage,
			},
		}
		cmd, err := framedPromptCommand(job)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := cmd["message"].(string)
		wantPrefix := "[Inter-session message from Vantry Pi thread \"Schema Reviewer\" (threadId: th_sender_peer) — you may reply or collaborate using vantry_thread_send / vantry_thread_read if needed]\n\n"
		if got != wantPrefix+originalMessage {
			t.Fatalf("framed prompt = %q, want %q", got, wantPrefix+originalMessage)
		}
		if job.Request.Message != originalMessage {
			t.Fatalf("job.Request.Message mutated to %q", job.Request.Message)
		}

		// Empty SenderTitle falls back to SenderThreadID
		jobNoTitle := job
		jobNoTitle.Request.SenderTitle = ""
		cmdNoTitle, err := framedPromptCommand(jobNoTitle)
		if err != nil {
			t.Fatal(err)
		}
		gotNoTitle, _ := cmdNoTitle["message"].(string)
		wantFallbackPrefix := "[Inter-session message from Vantry Pi thread \"th_sender_peer\" (threadId: th_sender_peer) — you may reply or collaborate using vantry_thread_send / vantry_thread_read if needed]\n\n"
		if gotNoTitle != wantFallbackPrefix+originalMessage {
			t.Fatalf("fallback framed prompt = %q, want %q", gotNoTitle, wantFallbackPrefix+originalMessage)
		}

		// Normal user message (empty SenderThreadID) has no provenance header
		normalJob := job
		normalJob.Request.SenderThreadID = ""
		normalCmd, err := framedPromptCommand(normalJob)
		if err != nil {
			t.Fatal(err)
		}
		if normalCmd["message"] != originalMessage {
			t.Fatalf("normal prompt = %v, want %q", normalCmd["message"], originalMessage)
		}
	})

	t.Run("rpc-run-sends-provenance-header-without-mutating-persisted-request", func(t *testing.T) {
		p, request, config := newRPCProvider(t, "normal")
		request.SenderThreadID = "th_peer_ui"
		request.SenderTitle = "Frontend Session"
		request.Message = "Can we align on the pagination cursor field?"

		if err := p.RunEvents(context.Background(), request, nilReceiver); err != nil {
			t.Fatalf("RPC inter-session run failed: %v", err)
		}

		rawCommands := readFile(t, config.Commands)
		wantHeader := `[Inter-session message from Vantry Pi thread \"Frontend Session\" (threadId: th_peer_ui) — you may reply or collaborate using vantry_thread_send / vantry_thread_read if needed]\n\nCan we align on the pagination cursor field?`
		if !strings.Contains(rawCommands, wantHeader) {
			t.Fatalf("Pi RPC stdin did not contain framed provenance header:\n%s", rawCommands)
		}

		// Persisted request.json must preserve raw un-framed Message
		persisted, found, err := readJobRequest(filepath.Join(p.options.DataDir, "jobs", request.AttemptID))
		if err != nil || !found {
			t.Fatalf("read persisted job request: found=%v err=%v", found, err)
		}
		if persisted.Request.Message != request.Message {
			t.Fatalf("persisted Request.Message = %q, want un-mutated %q", persisted.Request.Message, request.Message)
		}
	})
}
