package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

// This test needs no model, credentials, or prompt. All configuration and
// extensions are temporary; the installed Pi and global agent config are read-only.
func TestInstalledPiLoadsOnlyExplicitApprovalExtension(t *testing.T) {
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		t.Skip("installed Pi not available")
	}
	root := t.TempDir()
	extension := filepath.Join(root, "approval.ts")
	if err := os.WriteFile(extension, approvalExtension, 0o600); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(filepath.Join(agentDir, "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "automatic-extension-ran")
	forbidden := "import fs from 'node:fs'; export default function () { fs.writeFileSync(" + fmtJSString(sentinel) + ", 'unexpected'); }"
	if err := os.WriteFile(filepath.Join(agentDir, "extensions", "forbidden.ts"), []byte(forbidden), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, piExecutable, "--mode", "rpc", "--no-session", "--no-extensions", "--no-mcp", "--extension", extension)
	command.Dir = root
	command.Env = append(piEnvironment(piExecutable), "PI_CODING_AGENT_DIR="+agentDir)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdin, _ := command.StdinPipe()
	stdout, _ := command.StdoutPipe()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = command.Wait() }()
	if err := writeRPC(stdin, map[string]string{"type": "get_state", "id": "check"}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	ready := false
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("Pi startup: %v, stderr=%s", err, stderr.String())
		}
		var record rpcRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == "extension_ui_request" && strings.Contains(string(record.Message), "vantry_approval_ready") {
			ready = true
		}
		if record.Type == "response" && record.ID == "check" {
			if !record.Success || !ready {
				t.Fatalf("explicit extension failed to initialize: %s stderr=%s", line, stderr.String())
			}
			break
		}
	}
	_ = stdin.Close()
	_, _ = io.Copy(io.Discard, reader)
	if err := command.Wait(); err != nil {
		t.Fatalf("Pi EOF shutdown: %v stderr=%s", err, stderr.String())
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("--no-extensions loaded an automatic extension: %v", err)
	}

	// Load the same embedded TS through Pi's real extension loader and exercise
	// the registered gate without executing tools or submitting model prompts.
	loader := filepath.Join(filepath.Dir(piExecutable), "core", "extensions", "loader.js")
	if _, err := os.Stat(loader); err != nil {
		loader = filepath.Join(filepath.Dir(piExecutable), "..", "core", "extensions", "loader.js")
		if _, err := os.Stat(loader); err != nil {
			t.Log("explicit loading verified; installed Pi does not expose its JS extension loader for gate checks")
			return
		}
	}
	script := `
import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';
const {loadExtensions}=await import(pathToFileURL(process.argv[1]).href);
const loaded=await loadExtensions([process.argv[2]],process.cwd());
assert.deepEqual(loaded.errors,[]);
// Pi 1.5 passes handlers through the runtime; older loaders keep the map.
const handler=loaded.runtime?.handlers?.get('tool_call')?.[0]??loaded.extensions[0].handlers?.get('tool_call')?.[0];
// The host session may export an auto policy; this fixture must default to ask.
delete process.env.VANTRY_APPROVAL_POLICY;
delete process.env.VANTRY_INTERACTION_MODE;
for (const toolName of ['bash','write','edit']) {
  let count=0;
  const event={toolName,input:{command:'true',path:'example'}};
  const ctx={hasUI:true,ui:{confirm:async()=>{count++;return true;}}};
  assert.equal(await handler(event,ctx),undefined);
  assert.equal(count,1);
  ctx.ui.confirm=async()=>false;
  assert.equal((await handler(event,ctx)).block,true);
  ctx.ui.confirm=async()=>{throw new Error('UI unavailable')};
  assert.equal((await handler(event,ctx)).block,true);
  assert.equal((await handler(event,{hasUI:false})).block,true);
}
assert.equal(await handler({toolName:'read',input:{}},{hasUI:false}),undefined);
console.log('approval-gate-ok');
`
	node := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, loader, extension)
	node.Dir = root
	node.Env = command.Env
	output, err := node.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "approval-gate-ok") {
		t.Fatalf("real approval gate: %v %s", err, output)
	}
}

func TestGranularApprovalModesAndPlanProtection(t *testing.T) {
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		t.Skip("installed Pi not available")
	}
	loader, ok := findPiExtensionLoader(piExecutable)
	if !ok {
		t.Skip("installed Pi does not expose core/extensions/loader.js")
	}

	root := t.TempDir()
	extension := filepath.Join(root, "approval.ts")
	if err := os.WriteFile(extension, approvalExtension, 0o600); err != nil {
		t.Fatal(err)
	}

	script := `
import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";

const { loadExtensions } = await import(pathToFileURL(process.argv[1]).href);
const loaded = await loadExtensions([process.argv[2]], process.cwd());
assert.deepEqual(loaded.errors, []);
const handler = loaded.runtime?.handlers?.get("tool_call")?.[0] ?? loaded.extensions[0].handlers?.get("tool_call")?.[0];

// 1. auto / full-access: allow bash, write, edit without prompting
for (const policy of ["auto", "full-access"]) {
  process.env.VANTRY_APPROVAL_POLICY = policy;
  delete process.env.VANTRY_INTERACTION_MODE;
  for (const toolName of ["bash", "write", "edit", "read"]) {
    let prompts = 0;
    const res = await handler(
      { toolName, input: { command: "rm -rf build", path: "a.go" } },
      { hasUI: true, ui: { confirm: async () => { prompts++; return true; } } },
    );
    assert.equal(res, undefined, policy + " should allow " + toolName);
    assert.equal(prompts, 0);
  }
}

// 2. auto-accept-edits: allow write/edit automatically, prompt only for bash
process.env.VANTRY_APPROVAL_POLICY = "auto-accept-edits";
delete process.env.VANTRY_INTERACTION_MODE;
for (const toolName of ["write", "edit", "read"]) {
  let prompts = 0;
  const res = await handler(
    { toolName, input: { path: "src/main.go" } },
    { hasUI: true, ui: { confirm: async () => { prompts++; return false; } } },
  );
  assert.equal(res, undefined, "auto-accept-edits must allow " + toolName);
  assert.equal(prompts, 0);
}
{
  let prompts = 0;
  const okRes = await handler(
    { toolName: "bash", input: { command: "go test ./..." } },
    { hasUI: true, ui: { confirm: async () => { prompts++; return true; } } },
  );
  assert.equal(okRes, undefined);
  assert.equal(prompts, 1);

  const blockedRes = await handler(
    { toolName: "bash", input: { command: "go test ./..." } },
    { hasUI: true, ui: { confirm: async () => false } },
  );
  assert.equal(blockedRes?.block, true);
}

// 3. ask / approval-required: prompt for bash, write, and edit
for (const policy of ["ask", "approval-required"]) {
  process.env.VANTRY_APPROVAL_POLICY = policy;
  delete process.env.VANTRY_INTERACTION_MODE;
  for (const toolName of ["bash", "write", "edit"]) {
    let prompts = 0;
    const res = await handler(
      { toolName, input: { command: "ls", path: "a.go" } },
      { hasUI: true, ui: { confirm: async () => { prompts++; return true; } } },
    );
    assert.equal(res, undefined);
    assert.equal(prompts, 1, policy + " must prompt for " + toolName);
  }
}

// 4. Plan mode: block write/edit immediately; prompt on destructive bash even under auto; allow read-only bash & vantry_* tools
process.env.VANTRY_APPROVAL_POLICY = "auto";
process.env.VANTRY_INTERACTION_MODE = "plan";

for (const toolName of ["write", "edit"]) {
  let prompts = 0;
  const blocked = await handler(
    { toolName, input: { path: "plan.md" } },
    { hasUI: true, ui: { confirm: async () => { prompts++; return true; } } },
  );
  assert.equal(blocked?.block, true, "plan mode must block " + toolName);
  assert.ok(blocked?.reason.includes("Plan"), "reason must mention Plan mode: " + blocked?.reason);
  assert.equal(prompts, 0, "plan mode blocks write/edit directly without confirm dialog");
}

for (const safeCmd of ["ls -la", "git status", "git diff HEAD", "rg ApprovalPolicy internal/ 2>/dev/null", "printf 'a -> b\n'"]) {
  let prompts = 0;
  const res = await handler(
    { toolName: "bash", input: { command: safeCmd } },
    { hasUI: true, ui: { confirm: async () => { prompts++; return false; } } },
  );
  assert.equal(res, undefined, "plan+auto should allow read-only command: " + safeCmd);
  assert.equal(prompts, 0);
}

for (const dangerousCmd of ["rm -rf dist", "git commit -m test", "echo x > out.txt", "npm install left-pad", "sed -i 's/a/b/' foo.txt"]) {
  let prompts = 0;
  const denied = await handler(
    { toolName: "bash", input: { command: dangerousCmd } },
    { hasUI: true, ui: { confirm: async () => { prompts++; return false; } } },
  );
  assert.equal(denied?.block, true, "plan mode must guard destructive command: " + dangerousCmd);
  assert.equal(prompts, 1);

  const allowed = await handler(
    { toolName: "bash", input: { command: dangerousCmd } },
    { hasUI: true, ui: { confirm: async () => true } },
  );
  assert.equal(allowed, undefined);
}

for (const vantryTool of ["vantry_delegate_task", "vantry_task_status", "vantry_task_followup", "vantry_task_ack", "vantry_request_secret"]) {
  process.env.VANTRY_APPROVAL_POLICY = "approval-required";
  process.env.VANTRY_INTERACTION_MODE = "plan";
  const res = await handler({ toolName: vantryTool, input: {} }, { hasUI: false });
  assert.equal(res, undefined, vantryTool + " must never be blocked by approval.ts");
}

console.log("granular-approval-ok");
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	node := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, loader, extension)
	node.Dir = root
	node.Env = piEnvironment(piExecutable)
	output, err := node.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "granular-approval-ok") {
		t.Fatalf("granular approval check failed: %v\n%s", err, output)
	}
}

func TestProviderGranularApprovalHandshakeAndValidation(t *testing.T) {
	p, request, _ := newRPCProvider(t, "normal")
	request.ApprovalPolicy = "invalid-mode"
	if err := p.RunEvents(context.Background(), request, func(provider.Event) error { return nil }); err == nil || !strings.Contains(err.Error(), "unsupported approval policy") {
		t.Fatalf("expected unsupported approval policy error, got %v", err)
	}

	for _, policy := range []string{
		ApprovalPolicyAuto,
		ApprovalPolicyFullAccess,
		ApprovalPolicyAutoAcceptEdits,
		ApprovalPolicyAsk,
		ApprovalPolicyApprovalRequired,
	} {
		t.Run(policy, func(t *testing.T) {
			instance, req, cfg := newRPCProvider(t, "normal")
			req.ApprovalPolicy = policy
			if err := instance.RunEvents(context.Background(), req, func(provider.Event) error { return nil }); err != nil {
				t.Fatalf("policy %s failed: %v", policy, err)
			}
			args := readFile(t, cfg.Arguments)
			wantGate := policy == ApprovalPolicyAutoAcceptEdits || policy == ApprovalPolicyAsk || policy == ApprovalPolicyApprovalRequired
			if strings.Contains(args, "approval.ts") != wantGate {
				t.Fatalf("policy %s approval.ts presence = %v, want %v (args=%s)", policy, !wantGate, wantGate, args)
			}
		})
	}

	t.Run("plan-mode-mounts-approval-even-with-auto", func(t *testing.T) {
		instance, req, cfg := newRPCProvider(t, "normal")
		instance.options.InteractionMode = InteractionModePlan
		req.ApprovalPolicy = ApprovalPolicyAuto
		if err := instance.RunEvents(context.Background(), req, func(provider.Event) error { return nil }); err != nil {
			t.Fatalf("plan mode run failed: %v", err)
		}
		args := readFile(t, cfg.Arguments)
		if !strings.Contains(args, "approval.ts") {
			t.Fatalf("plan mode must mount approval.ts even under auto policy:\n%s", args)
		}
		instructions := readFile(t, filepath.Join(instance.options.DataDir, "jobs", req.AttemptID, "instructions.md"))
		for _, want := range []string{"Plan Mode (Read-Only Deep Investigation)", "configuration & data sources"} {
			if !strings.Contains(instructions, want) {
				t.Fatalf("plan mode instructions.md missing %q:\n%s", want, instructions)
			}
		}
	})
}

func fmtJSString(value string) string { data, _ := json.Marshal(value); return string(data) }

func TestApprovalReadOnlyAllowlistAndSessionGrants(t *testing.T) {
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		t.Skip("installed Pi not available")
	}
	loader, ok := findPiExtensionLoader(piExecutable)
	if !ok {
		t.Skip("installed Pi does not expose core/extensions/loader.js")
	}

	root := t.TempDir()
	extension := filepath.Join(root, "approval.ts")
	if err := os.WriteFile(extension, approvalExtension, 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	script := `
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const { loadExtensions } = await import(pathToFileURL(process.argv[1]).href);
const extPath = process.argv[2];
const dataDir = process.argv[3];

async function freshHandler() {
  const loaded = await loadExtensions([extPath], process.cwd());
  assert.deepEqual(loaded.errors, []);
  return loaded.runtime?.handlers?.get("tool_call")?.[0] ?? loaded.extensions[0].handlers?.get("tool_call")?.[0];
}

// 1. Read-only allowlist under auto-accept-edits
delete process.env.VANTRY_DATA_DIR;
delete process.env.VANTRY_THREAD_ID;
delete process.env.VANTRY_INTERACTION_MODE;
process.env.VANTRY_APPROVAL_POLICY = "auto-accept-edits";

const h1 = await freshHandler();
for (const safeCmd of [
  "ls -la",
  "git status",
  "git diff HEAD",
  "rg foo internal/ | head -n 5",
  "find . -name '*.go'",
]) {
  let prompts = 0;
  const res = await h1(
    { toolName: "bash", input: { command: safeCmd } },
    {
      hasUI: true,
      ui: {
        select: async () => { prompts++; return "允许本次"; },
        confirm: async () => { prompts++; return true; },
      },
    },
  );
  assert.equal(res, undefined, "read-only command should auto-allow under auto-accept-edits: " + safeCmd);
  assert.equal(prompts, 0, "unexpected prompt for read-only command: " + safeCmd);
}

// Non-readonly or unsafe commands still prompt under auto-accept-edits
for (const unsafeCmd of [
  "go test ./...",
  "find . -delete",
  "echo hi > a.txt",
  "ls $(whoami)",
  "ls; rm a",
  "python3 -c 'print(1)'",
]) {
  let prompts = 0;
  const res = await h1(
    { toolName: "bash", input: { command: unsafeCmd } },
    {
      hasUI: true,
      ui: {
        select: async (_title, _opts) => { prompts++; return "拒绝"; },
        confirm: async () => { prompts++; return false; },
      },
    },
  );
  assert.equal(prompts, 1, "expected prompt under auto-accept-edits for: " + unsafeCmd);
  assert.equal(res?.block, true, "expected deny to block: " + unsafeCmd);
}

// ask policy still prompts even for ls
process.env.VANTRY_APPROVAL_POLICY = "ask";
{
  let prompts = 0;
  const res = await h1(
    { toolName: "bash", input: { command: "ls -la" } },
    {
      hasUI: true,
      ui: {
        select: async () => { prompts++; return "允许本次"; },
        confirm: async () => true,
      },
    },
  );
  assert.equal(res, undefined);
  assert.equal(prompts, 1, "ask must still prompt for ls -la");
}

// 2. Session grants via select + destructive exclusion
process.env.VANTRY_APPROVAL_POLICY = "auto-accept-edits";
{
  let capturedOptions = [];
  let prompts = 0;
  const resGo1 = await h1(
    { toolName: "bash", input: { command: "go test ./..." } },
    {
      hasUI: true,
      ui: {
        select: async (_title, opts) => {
          prompts++;
          capturedOptions = opts;
          return "本会话允许 bash:go";
        },
        confirm: async () => true,
      },
    },
  );
  assert.equal(resGo1, undefined);
  assert.equal(prompts, 1);
  assert.deepEqual(capturedOptions, ["允许本次", "本会话允许 bash:go", "拒绝"]);

  // Second invocation of bash:go reuses session grant without prompting
  const resGo2 = await h1(
    { toolName: "bash", input: { command: "go test ./pkg/..." } },
    {
      hasUI: true,
      ui: {
        select: async () => { prompts++; return "拒绝"; },
        confirm: async () => false,
      },
    },
  );
  assert.equal(resGo2, undefined);
  assert.equal(prompts, 1, "session grant bash:go must skip repeat prompts");

  // Unrelated executable still prompts
  const resNpm = await h1(
    { toolName: "bash", input: { command: "npm test" } },
    {
      hasUI: true,
      ui: {
        select: async () => { prompts++; return "拒绝"; },
        confirm: async () => false,
      },
    },
  );
  assert.equal(resNpm?.block, true);
  assert.equal(prompts, 2);

  // Destructive commands never offer session grant
  let rmOpts = [];
  const resRm = await h1(
    { toolName: "bash", input: { command: "rm -rf build" } },
    {
      hasUI: true,
      ui: {
        select: async (_title, opts) => { rmOpts = opts; return "拒绝"; },
        confirm: async () => false,
      },
    },
  );
  assert.equal(resRm?.block, true);
  assert.deepEqual(rmOpts, ["允许本次", "拒绝"]);
}

// 3. Per-thread grants persistence across fresh extension reloads
process.env.VANTRY_DATA_DIR = dataDir;
process.env.VANTRY_THREAD_ID = "th-grant-persist";
{
  const hPersist1 = await freshHandler();
  let prompts1 = 0;
  const r1 = await hPersist1(
    { toolName: "bash", input: { command: "go test ./internal/core/..." } },
    {
      hasUI: true,
      ui: {
        select: async () => { prompts1++; return "本会话允许 bash:go"; },
        confirm: async () => true,
      },
    },
  );
  assert.equal(r1, undefined);
  assert.equal(prompts1, 1);

  const grantsFile = path.join(dataDir, "sessions", "th-grant-persist.grants.json");
  assert.equal(fs.existsSync(grantsFile), true, "expected persisted grants file");

  const hPersist2 = await freshHandler();
  let prompts2 = 0;
  const r2 = await hPersist2(
    { toolName: "bash", input: { command: "go vet ./..." } },
    {
      hasUI: true,
      ui: {
        select: async () => { prompts2++; return "拒绝"; },
        confirm: async () => false,
      },
    },
  );
  assert.equal(r2, undefined);
  assert.equal(prompts2, 0, "persisted grant should survive fresh extension reload");
}

console.log("readonly-and-session-grants-ok");
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, loader, extension, dataDir)
	node.Dir = root
	node.Env = piEnvironment(piExecutable)
	output, err := node.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "readonly-and-session-grants-ok") {
		t.Fatalf("read-only allowlist & session grants test failed: %v\n%s", err, output)
	}
}
