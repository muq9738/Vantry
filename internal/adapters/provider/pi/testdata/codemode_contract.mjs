// Exercise Pi's real QuickJS sandbox, registered Vantry tools and permission
// pipeline with a seeded assistant tool-call fixture. No LLM/API request is made.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { pathToFileURL } from "node:url";

const [sdkEntry, toolsPath, approvalPath, root] = process.argv.slice(2);
const sdk = await import(pathToFileURL(sdkEntry).href);
const requests = [], events = [];
let session, server, allow = false, confirms = 0, subagentRan = false;
let pendingList, pendingRead, waitStarted, waitClosed;
const started = new Promise(resolve => { waitStarted = resolve; });
const closed = new Promise(resolve => { waitClosed = resolve; });
const list = { threads: [{ threadId: "same-thread", title: "Only the useful title", extra: "x".repeat(20000) }], total: 1 };
const read = { thread: { threadId: "same-thread", environmentId: "studio" }, items: [{ text: "REMOTE-READ-OK" }] };
function flushParallelReads() {
  if (pendingList && pendingRead) {
    pendingList.end(JSON.stringify(list));
    pendingRead.end(JSON.stringify(read));
  }
}
function resultText(result) { return result.content.filter(c => c.type === "text").map(c => c.text).join("\n"); }
try {
  await fs.writeFile(path.join(root, "note.txt"), "NATIVE-CODEMODE-READ-OK");
  Object.assign(process.env, {
    VANTRY_THREAD_ID: "parent-thread", VANTRY_RUN_ID: "parent-run", VANTRY_ATTEMPT_ID: "parent-attempt",
    VANTRY_APPROVAL_POLICY: "ask", VANTRY_INTERACTION_MODE: "default", VANTRY_SOCKET_PATH: path.join(root, "core.sock"),
  });
  server = http.createServer(async (req, res) => {
    let raw = "";
    for await (const chunk of req) raw += chunk;
    const body = JSON.parse(raw);
    requests.push({ path: req.url, body });
    res.setHeader("content-type", "application/json");
    if (req.url === "/v1/orchestration/threads/list") {
      pendingList = res; flushParallelReads();
    } else if (req.url === "/v1/orchestration/threads/read") {
      pendingRead = res; flushParallelReads();
    } else if (req.url === "/v1/orchestration/threads/wait") {
      res.on("close", waitClosed); waitStarted();
    } else {
      res.statusCode = 404; res.end(JSON.stringify({ code: "not_found", message: "Unexpected fixture endpoint" }));
    }
  });
  await new Promise(resolve => server.listen(process.env.VANTRY_SOCKET_PATH, resolve));
  const loader = new sdk.DefaultResourceLoader({
    cwd: root, agentDir: path.join(root, "agent"), noExtensions: true, noSkills: true,
    noPromptTemplates: true, noThemes: true, noContextFiles: true,
    additionalExtensionPaths: [toolsPath, approvalPath],
    extensionFactories: [
      sdk.createCodemodeExtension({ mode: "on" }), sdk.createToolSearchExtension(),
      pi => pi.registerTool({
        name: "subagent", label: "Native child fixture", description: "Native provider delegation remains available",
        parameters: { type: "object", properties: {} },
        async execute() { subagentRan = true; return { content: [{ type: "text", text: "native child completed" }], details: {} }; },
      }),
    ],
  });
  await loader.reload();
  assert.deepEqual(loader.getExtensions().errors, []);
  const manager = sdk.SessionManager.inMemory(root);
  // Pi requires a real owning assistant message for all nested tool calls.
  manager.appendMessage({
    role: "assistant", api: "openai-completions", provider: "fixture", model: "fixture",
    content: [{ type: "toolCall", id: "native-codemode-owner", name: "codemode", arguments: { code: "fixture" } }],
    stopReason: "toolUse", timestamp: Date.now(),
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
  });
  ({ session } = await sdk.createAgentSession({
    cwd: root, agentDir: path.join(root, "agent"), resourceLoader: loader, sessionManager: manager,
    model: { provider: "fixture", id: "fixture", name: "Fixture", api: "openai-completions", baseUrl: "http://127.0.0.1:9",
      reasoning: false, input: ["text"], contextWindow: 32768, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
    settingsManager: sdk.SettingsManager.inMemory({ defaultTools: ["+codemode", "+tool_search"], compaction: { enabled: false }, retry: { enabled: false } }),
  }));
  await session.bindExtensions({ uiContext: { notify() {}, async confirm() { confirms++; return allow; } } });
  session.subscribe(event => { if (event.type.startsWith("tool_execution")) events.push(event); });
  const initial = session.getActiveToolNames();
  const vantry = session.getAllTools().filter(t => t.name.startsWith("vantry_"));
  assert.equal(vantry.length, 17);
  assert.deepEqual(initial.filter(n => n.startsWith("vantry_")), ["vantry_request_secret"]);
  assert.ok(!initial.includes("vantry_delegate_task"));
  assert.ok(!initial.includes("vantry_thread_read"));
  assert.ok(session.getCallableToolNames().includes("vantry_delegate_task"));
  assert.ok(session.getCallableToolNames().includes("vantry_thread_read"));
  assert.ok(!session.getCallableToolNames().includes("vantry_request_secret"));
  for (const info of vantry) assert.ok(session.getToolDefinition(info.name).outputSchema);
  const readDeclared = session.agent.state.tools.find(t => t.name === "read");
  assert.ok(readDeclared && !readDeclared.description.includes("Codemode:"), "read description must not advertise Codemode suffix");
  const startTurn1 = await session._prepareRun({ messageType: "user", prompt: "inspect codebase" });
  assert.ok(startTurn1[0]?.role === "system", "Turn 1 must emit initial system prompt sections");
  assert.ok(!session.systemPrompt.includes("instead of many separate calls"), "Pi blanket codemode rule must be overridden");
  assert.ok(session.systemPrompt.includes("Call native read, grep, find, and ls tools directly in parallel"));
  session.agent.state.messages.push(...startTurn1);
  const startTurn2 = await session._prepareRun({ messageType: "user", prompt: "follow up" });
  assert.notEqual(startTurn2[0]?.role, "system", "Turn 2 with unchanged prompt must emit no system patch");
  const declarationBytes = () => Buffer.byteLength(JSON.stringify(session.agent.state.tools.map(t => ({ name: t.name, description: t.description, parameters: t.parameters }))));
  const compactBytes = declarationBytes();
  session.setActiveToolsByName([...initial.filter(n => n !== "codemode" && n !== "tool_search"), ...vantry.map(t => t.name)]);
  const baselineBytes = declarationBytes();
  session.setActiveToolsByName(initial);
  assert.ok(compactBytes < baselineBytes, `Discovery must save declarations: ${compactBytes} >= ${baselineBytes}`);
  const codeTool = session.agent.state.tools.find(t => t.name === "codemode");
  const execute = (id, code, signal) => codeTool.execute(id, { code }, signal, undefined);
  const batch = await execute("batch", `
    const result = await Promise.allSettled([
      tools.vantry_thread_list({ limit: 3 }),
      tools.vantry_thread_read({ threadId: "same-thread", environmentId: "studio" })
    ]);
    if (result.some(r => r.status !== "fulfilled")) throw new Error("parallel query failed");
    text({title: result[0].value.threads[0].title, environment: result[1].value.thread.environmentId, result: result[1].value.items[0].text});
  `);
  assert.ok(!batch.isError, resultText(batch));
  assert.match(resultText(batch), /Only the useful title/);
  assert.match(resultText(batch), /REMOTE-READ-OK/);
  assert.ok(resultText(batch).length < 500);
  assert.ok(!resultText(batch).includes(list.threads[0].extra));
  const readRequest = requests.find(r => r.path.endsWith("/read"));
  assert.equal(readRequest.body.environmentId, "studio");
  assert.equal(readRequest.body.callerThreadId, "parent-thread");
  const starts = events.filter(e => e.type === "tool_execution_start" && e.parentToolCallId === "batch");
  assert.deepEqual(starts.map(e => e.toolName).sort(), ["vantry_thread_list", "vantry_thread_read"]);
  assert.ok(starts.every(e => e.toolCallId.startsWith("batch/")));
  const searchTool = session.agent.state.tools.find(t => t.name === "tool_search");
  const discovered = await searchTool.execute("discover", { query: "vantry_thread_read", limit: 1 }, undefined, undefined);
  assert.ok(!discovered.isError);
  assert.ok(session.getActiveToolNames().includes("vantry_thread_read"));
  const denied = await execute("denied", 'await tools.bash({command:"touch denied.txt"})');
  assert.ok(denied.isError);
  assert.equal(confirms, 1);
  await assert.rejects(fs.access(path.join(root, "denied.txt")));
  allow = true;
  const approved = await execute("approved", 'const r=await tools.bash({command:"printf APPROVED-CODEMODE"});text(r.output)');
  assert.ok(!approved.isError, resultText(approved));
  assert.match(resultText(approved), /APPROVED-CODEMODE/);
  assert.equal(confirms, 2);
  process.env.VANTRY_INTERACTION_MODE = "plan";
  const plan = await execute("plan", 'await tools.write({path:"blocked-by-plan.txt",content:"no"})');
  assert.ok(plan.isError);
  assert.equal(confirms, 2, "Plan must block writes before asking for approval");
  await assert.rejects(fs.access(path.join(root, "blocked-by-plan.txt")));
  process.env.VANTRY_INTERACTION_MODE = "default";
  const nativeChild = await execute("native-child", 'await tools.subagent({})');
  assert.ok(!nativeChild.isError, resultText(nativeChild));
  assert.equal(subagentRan, true);
  assert.equal(confirms, 2, "The bridge must not add a separate approval rule for native child tools");
  const secret = await execute("secret", 'await tools.vantry_request_secret({prompt:"not allowed from scripts"})');
  assert.ok(secret.isError);
  assert.equal(confirms, 2);
  const beforeInvalid = requests.length;
  const invalid = await execute("invalid", 'await tools.vantry_thread_read({environmentId:"studio"})');
  assert.ok(invalid.isError);
  assert.equal(requests.length, beforeInvalid, "Invalid arguments reached Core");
  const controller = new AbortController();
  const waiting = execute("cancel", 'await tools.vantry_thread_wait({threadId:"slow",timeoutMs:30000})', controller.signal);
  await Promise.race([started, new Promise((_, reject) => setTimeout(() => reject(new Error("Wait tool never started")), 3000))]);
  controller.abort();
  await waiting.catch(error => assert.match(String(error), /abort|cancel/i));
  await Promise.race([closed, new Promise((_, reject) => setTimeout(() => reject(new Error("Cancelled Core request remained open")), 3000))]);
  const directReadInsideCodemode = await execute("read-ok", 'const c = await tools.read({ path: "note.txt" }); text(c);');
  assert.ok(!directReadInsideCodemode.isError, resultText(directReadInsideCodemode));
  assert.match(resultText(directReadInsideCodemode), /NATIVE-CODEMODE-READ-OK/);

  await fs.writeFile(path.join(root, "big1.txt"), "A".repeat(20000));
  await fs.writeFile(path.join(root, "mid.txt"), "SMALL_MIDDLE_FILE_MARKER_OK");
  await fs.writeFile(path.join(root, "big2.txt"), "C".repeat(20000));
  const multiCode = `// @options: {"max_output_tokens": 500}\nconst [a, b, c] = await Promise.all([tools.read({ path: "big1.txt" }), tools.read({ path: "mid.txt" }), tools.read({ path: "big2.txt" })]);\ntext(a); text(b); text(c);`;
  const rawMulti = await execute("trunc-multi", multiCode);
  assert.ok(!rawMulti.isError, resultText(rawMulti));
  assert.ok(rawMulti.details?.fullOutputPath, "expected spilled fullOutputPath");
  assert.ok(!resultText(rawMulti).includes("SMALL_MIDDLE_FILE_MARKER_OK"), "raw Pi merged truncation drops middle text section");
  const afterMulti = await session._afterToolCall({
    toolCall: { id: "trunc-multi", name: "codemode" },
    args: { code: multiCode },
    result: rawMulti,
    isError: Boolean(rawMulti.isError),
  });
  const rebalancedMultiText = resultText(afterMulti);
  assert.ok(rebalancedMultiText.includes("SMALL_MIDDLE_FILE_MARKER_OK"), "rebalanced output must preserve small middle section intact");
  assert.match(rebalancedMultiText, /\[Full section: .* lines \d+-\d+\]/);
  assert.match(rebalancedMultiText, /call those native tools directly in parallel/);
  assert.equal(afterMulti.details?.rebalanced?.mode, "multi-text");
  assert.equal(afterMulti.details?.rebalanced?.onlyRoutineRetrieval, true);

  const jsonCode = `// @options: {"max_output_tokens": 500}\nreturn ["${"X".repeat(20000)}", "MIDDLE_JSON_ELEMENT_OK", "${"Z".repeat(20000)}"];`;
  const rawJson = await execute("trunc-json", jsonCode);
  assert.ok(!rawJson.isError, resultText(rawJson));
  assert.ok(rawJson.details?.fullOutputPath);
  assert.ok(!resultText(rawJson).includes("MIDDLE_JSON_ELEMENT_OK"));
  const afterJson = await session._afterToolCall({
    toolCall: { id: "trunc-json", name: "codemode" },
    args: { code: jsonCode },
    result: rawJson,
    isError: Boolean(rawJson.isError),
  });
  assert.ok(resultText(afterJson).includes("MIDDLE_JSON_ELEMENT_OK"), "rebalanced JSON array output must preserve small middle element");
  assert.equal(afterJson.details?.rebalanced?.mode, "json-array");

  const errCode = `// @options: {"max_output_tokens": 400}\ntext("${"Y".repeat(25000)}");\nthrow new Error("FATAL_SCRIPT_ERR_PRESERVED");`;
  const rawErr = await execute("trunc-err", errCode);
  assert.ok(rawErr.isError);
  assert.ok(rawErr.details?.fullOutputPath);
  const afterErr = await session._afterToolCall({
    toolCall: { id: "trunc-err", name: "codemode" },
    args: { code: errCode },
    result: rawErr,
    isError: true,
  });
  assert.ok(afterErr.isError);
  assert.ok(resultText(afterErr).includes("FATAL_SCRIPT_ERR_PRESERVED"), "rebalanced error output must keep Script error intact");

  // Non-codemode general tool_result guard: oversized HTML+GBK bash output spills to jobDir and adds head+tail + hints.
  const prevJobDir = process.env.VANTRY_JOB_DIR;
  const bashJobDir = path.join(root, "bash-guard-job");
  process.env.VANTRY_JOB_DIR = bashJobDir;
  try {
    const hugeHtml =
      "<!DOCTYPE html><html><head><meta charset='gbk'></head><body><div><span><a><b><u><i>" +
      "HEAD_PAGE_MARKER\n" +
      "\uFFFD".repeat(30) +
      "X".repeat(20_000) +
      "\nTAIL_SCRAPE_RESULT_OK</body></html>";
    const afterBash = await session._afterToolCall({
      toolCall: { id: "bash-big-html", name: "bash" },
      args: { command: "curl -s https://example.invalid" },
      result: { content: [{ type: "text", text: hugeHtml }], details: {} },
      isError: false,
    });
    const prunedText = resultText(afterBash);
    assert.ok(prunedText.includes("HEAD_PAGE_MARKER"));
    assert.ok(prunedText.includes("TAIL_SCRAPE_RESULT_OK"));
    assert.ok(prunedText.includes("[Encoding hint:"));
    assert.ok(prunedText.includes("[HTML hint:"));
    assert.ok(afterBash.details?.vantryPruned?.spillPath);
    const spilled = await fs.readFile(afterBash.details.vantryPruned.spillPath, "utf8");
    assert.equal(spilled, hugeHtml);

    // Consecutive errors trigger the loop guard circuit breaker on the 3rd failure.
    for (let i = 1; i <= 3; i++) {
      const failRes = await session._afterToolCall({
        toolCall: { id: `bash-fail-${i}`, name: "bash" },
        args: { command: `curl -f https://bad.invalid/${i}` },
        result: { content: [{ type: "text", text: "curl: (22) HTTP 503" }], details: {} },
        isError: true,
      });
      if (i < 3) {
        assert.equal(failRes?.details?.vantryLoopGuard, undefined);
      } else {
        assert.equal(failRes.details?.vantryLoopGuard?.kind, "consecutive_failures");
        assert.match(resultText(failRes), /Loop guard:/);
      }
    }
  } finally {
    if (prevJobDir === undefined) delete process.env.VANTRY_JOB_DIR;
    else process.env.VANTRY_JOB_DIR = prevJobDir;
  }

  let coordSession;
  process.env.VANTRY_DIRECT_ORCHESTRATION = "1";
  try {
    const coordLoader = new sdk.DefaultResourceLoader({
      cwd: root, agentDir: path.join(root, "agent-coord"), noExtensions: true, noSkills: true,
      noPromptTemplates: true, noThemes: true, noContextFiles: true,
      additionalExtensionPaths: [toolsPath, approvalPath],
      extensionFactories: [sdk.createCodemodeExtension({ mode: "on" }), sdk.createToolSearchExtension()],
    });
    await coordLoader.reload();
    ({ session: coordSession } = await sdk.createAgentSession({
      cwd: root, agentDir: path.join(root, "agent-coord"), resourceLoader: coordLoader,
      sessionManager: sdk.SessionManager.inMemory(root),
      model: { provider: "fixture", id: "fixture", name: "Fixture", api: "openai-completions", baseUrl: "http://127.0.0.1:9",
        reasoning: false, input: ["text"], contextWindow: 32768, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
      settingsManager: sdk.SettingsManager.inMemory({ defaultTools: ["+codemode", "+tool_search"], compaction: { enabled: false }, retry: { enabled: false } }),
    }));
    await coordSession.bindExtensions({ uiContext: { notify() {}, async confirm() { return false; } } });
    const coordActive = coordSession.getActiveToolNames();
    for (const directName of ["vantry_delegate_task", "vantry_orchestrator_capabilities", "vantry_task_status", "vantry_request_secret"]) {
      assert.ok(coordActive.includes(directName), `${directName} must be active in coordinator sessions`);
    }
  } finally {
    delete process.env.VANTRY_DIRECT_ORCHESTRATION;
    coordSession?.dispose();
  }

  let childSession;
  process.env.VANTRY_PARENT_TASK_ID = "task_child_1";
  process.env.VANTRY_DIRECT_ORCHESTRATION = "1";
  try {
    const childLoader = new sdk.DefaultResourceLoader({
      cwd: root, agentDir: path.join(root, "agent-child"), noExtensions: true, noSkills: true,
      noPromptTemplates: true, noThemes: true, noContextFiles: true,
      additionalExtensionPaths: [toolsPath, approvalPath],
      extensionFactories: [sdk.createCodemodeExtension({ mode: "on" }), sdk.createToolSearchExtension()],
    });
    await childLoader.reload();
    ({ session: childSession } = await sdk.createAgentSession({
      cwd: root, agentDir: path.join(root, "agent-child"), resourceLoader: childLoader,
      sessionManager: sdk.SessionManager.inMemory(root),
      model: { provider: "fixture", id: "fixture", name: "Fixture", api: "openai-completions", baseUrl: "http://127.0.0.1:9",
        reasoning: false, input: ["text"], contextWindow: 32768, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } },
      settingsManager: sdk.SettingsManager.inMemory({ defaultTools: ["+codemode", "+tool_search"], compaction: { enabled: false }, retry: { enabled: false } }),
    }));
    await childSession.bindExtensions({ uiContext: { notify() {}, async confirm() { return false; } } });
    const childActive = childSession.getActiveToolNames();
    assert.ok(childActive.includes("vantry_request_secret"));
    for (const parentOnly of ["vantry_delegate_task", "vantry_orchestrator_capabilities", "vantry_task_status"]) {
      assert.ok(!childActive.includes(parentOnly), `${parentOnly} must be deferred in child sessions`);
      assert.ok(childSession.getCallableToolNames().includes(parentOnly), `${parentOnly} remains callable if needed`);
    }
  } finally {
    delete process.env.VANTRY_PARENT_TASK_ID;
    delete process.env.VANTRY_DIRECT_ORCHESTRATION;
    childSession?.dispose();
  }
  await fs.writeFile(path.join(root, "native-events.jsonl"), events.map(e => JSON.stringify(e)).join("\n") + "\n");
  process.stdout.write(JSON.stringify({
    status: "passed", registeredVantryTools: vantry.length, declaredVantryTools: 1,
    toolDeclarationBytes: { baseline: baselineBytes, compact: compactBytes },
    unfilteredFixtureBytes: Buffer.byteLength(JSON.stringify(list)) + Buffer.byteLength(JSON.stringify(read)),
    filteredResultBytes: Buffer.byteLength(resultText(batch)), approvals: confirms,
    nestedEventCount: events.length, cancellationReachedCore: true,
  }) + "\n");
} finally {
  session?.dispose();
  server?.closeAllConnections();
  await new Promise(resolve => server ? server.close(resolve) : resolve());
}
