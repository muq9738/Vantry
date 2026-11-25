import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { StringEnum } from "@earendil-works/pi-ai";
import { execFile as execFileCallback } from "node:child_process";
import * as fsSync from "node:fs";
import * as fs from "node:fs/promises";
import * as http from "node:http";
import * as path from "node:path";
import { type Static, Type } from "typebox";

const PROTOCOL_VERSION = 1;

const ENV_EXECUTABLE = "VANTRY_EXECUTABLE";
const ENV_DATA_DIR = "VANTRY_DATA_DIR";
const ENV_SOCKET_PATH = "VANTRY_SOCKET_PATH";
const ENV_JOB_DIR = "VANTRY_JOB_DIR";
const ENV_RUN_ID = "VANTRY_RUN_ID";
const ENV_ATTEMPT_ID = "VANTRY_ATTEMPT_ID";
const ENV_THREAD_ID = "VANTRY_THREAD_ID";
const ENV_PARENT_TASK_ID = "VANTRY_PARENT_TASK_ID";
const ENV_DIRECT_ORCHESTRATION = "VANTRY_DIRECT_ORCHESTRATION";
const ENV_INTERACTION_MODE = "VANTRY_INTERACTION_MODE";
const VANTRY_TERMINAL_REQUEST_KIND = "vantry_terminal_run";
const MODEL_ONLY_UI_TOOLS = new Set(["vantry_request_secret", "vantry_terminal_run"]);

const DIRECT_RETRIEVAL_TOOLS = new Set(["read", "grep", "find", "ls"]);
const DEFAULT_CODEMODE_MAX_OUTPUT_TOKENS = 10_000;
const CODEMODE_CHARS_PER_TOKEN = 4;
const MAX_CODEMODE_REBALANCE_FILE_BYTES = 16 * 1024 * 1024;
const MAX_CODEMODE_JSON_PARSE_CHARS = 4 * 1024 * 1024;

const TOOL_OUTPUT_GUARD_MAX_CHARS = 16_000;
const TOOL_OUTPUT_GUARD_HEAD_CHARS = 6_000;
const TOOL_OUTPUT_GUARD_TAIL_CHARS = 8_000;
const TOOL_OUTPUT_SKIP_PRUNE = new Set(["codemode", "read", "edit", "write", "tool_search"]);
const LOOP_GUARD_FAILURE_THRESHOLD = 3;
const LOOP_GUARD_REPEAT_THRESHOLD = 3;
const LOOP_GUARD_STREAK_THRESHOLD = 5;
const LOOP_GUARD_HISTORY_SIZE = 16;
const MUTATING_TOOLS = new Set(["edit", "write"]);

const VANTRY_CODEMODE_SNIPPET =
  "Run sandboxed JavaScript to filter large payloads, call codemode MCP/Vantry tools, or batch orchestration (not routine read/grep)";
const VANTRY_CODEMODE_GUIDELINES = [
  "Call native read, grep, find, and ls tools directly in parallel for routine code/file inspection; do NOT wrap ordinary reads or greps in codemode.",
  "Use codemode only to filter/reshape large JSON or API/MCP payloads before calling text(), batch multi-subtask orchestration (Promise.allSettled), or invoke codemode-exposed MCP/Vantry tools.",
];

const MAX_RESULT_PREVIEW_CHARS = 4_000;
const MAX_BATCH_THREADS = 20;
const ENV_VAR_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;
const SAFE_ID_PATTERN = /^[A-Za-z0-9._-]+$/;

const DELEGATE_ROLES = ["implementation", "research", "review", "design", "test", "general"] as const;
const ISOLATION_MODES = ["shared", "worktree"] as const;
const DELEGATE_MODES = ["async", "wait"] as const;
const RESUME_POLICIES = ["auto", "manual"] as const;
const APPROVAL_POLICIES = ["auto", "full-access", "auto-accept-edits", "ask", "approval-required"] as const;
const INTERACTION_MODES = ["default", "plan"] as const;
const THREAD_READ_VIEWS = ["messages", "activity"] as const;
const THREAD_SEND_MODES = ["auto", "queue", "steer"] as const;
const WORKSPACE_KINDS = ["directory", "worktree"] as const;
const THREAD_TRANSFER_ACTIONS = ["fork", "merge_back"] as const;
const THREAD_MANAGE_ACTIONS = [
  "rename",
  "pin",
  "unpin",
  "settle",
  "unsettle",
  "snooze",
  "unsnooze",
  "set_model",
  "interrupt",
  "respond_request",
] as const;

let secretSequence = 0;
let commandSequence = 0;
let toolSpillSequence = 0;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function readEnv(name: string): string | undefined {
  const value = process.env[name]?.trim();
  return value && value.length > 0 ? value : undefined;
}

function isTruthyEnv(name: string): boolean {
  const value = readEnv(name)?.toLowerCase();
  return value === "1" || value === "true" || value === "yes" || value === "on";
}

function requireEnv(name: string): string {
  const value = readEnv(name);
  if (!value) {
    throw new Error(`Missing required Vantry environment variable ${name}`);
  }
  return value;
}

function allocateCommandId(clientRequestId?: string): string {
  const explicit = clientRequestId?.trim();
  if (explicit) {
    return explicit;
  }
  commandSequence += 1;
  return `cmd_${process.pid}_${Date.now()}_${commandSequence}`;
}

function resolveSocketPath(): string | undefined {
  const socketPath = readEnv(ENV_SOCKET_PATH);
  if (!socketPath) {
    return undefined;
  }
  try {
    const stat = fsSync.statSync(socketPath);
    if (stat.isSocket()) {
      return socketPath;
    }
  } catch {
    // Socket path is absent or not listening; fall back to CLI when supported.
  }
  return undefined;
}

function formatCoreApiError(endpointPath: string, statusCode: number, code: string, message: string): string {
  let hint = "";
  if (code === "not_found") {
    hint = " (verify threadId/taskId/runId with vantry_thread_list or vantry_task_status)";
  } else if (code === "conflict") {
    hint = " (inspect current session/task state before retrying)";
  } else if (code === "not_ready") {
    hint = " (Vantry Core is starting or draining)";
  }
  return `Vantry Core ${code} [${statusCode} ${endpointPath}]: ${message}${hint}`;
}

export function callCoreSocket<T = Record<string, unknown>>(
  method: "GET" | "POST",
  endpointPath: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  if (signal?.aborted) {
    return Promise.reject(new Error(`Vantry Core request to ${endpointPath} was aborted`));
  }
  const socketPath = resolveSocketPath();
  if (!socketPath) {
    const configured = readEnv(ENV_SOCKET_PATH);
    return Promise.reject(
      new Error(
        configured
          ? `Vantry Core socket not found at ${configured}; ensure Vantry Core is running`
          : `Missing ${ENV_SOCKET_PATH}; Vantry Core socket is required for ${endpointPath}`,
      ),
    );
  }

  const payload = body !== undefined ? Buffer.from(JSON.stringify(body), "utf8") : undefined;
  const headers: Record<string, string> = {
    Accept: "application/json",
  };
  if (payload !== undefined) {
    headers["Content-Type"] = "application/json";
    headers["Content-Length"] = String(payload.byteLength);
  }

  return new Promise<T>((resolve, reject) => {
    let settled = false;
    let onAbort: (() => void) | undefined;

    const cleanup = () => {
      if (signal && onAbort) {
        signal.removeEventListener("abort", onAbort);
      }
    };

    const finishResolve = (value: T) => {
      if (settled) return;
      settled = true;
      cleanup();
      resolve(value);
    };

    const finishReject = (err: Error) => {
      if (settled) return;
      settled = true;
      cleanup();
      reject(err);
    };

    const req = http.request(
      {
        socketPath,
        path: endpointPath,
        method,
        headers,
      },
      (res) => {
        const chunks: Buffer[] = [];
        res.on("data", (chunk: Buffer | string) => {
          chunks.push(typeof chunk === "string" ? Buffer.from(chunk, "utf8") : chunk);
        });
        res.on("error", (err) => {
          finishReject(new Error(`Vantry Core socket response failed (${endpointPath}): ${err.message}`));
        });
        res.on("end", () => {
          const statusCode = res.statusCode ?? 0;
          const rawText = Buffer.concat(chunks).toString("utf8").trim();
          let parsed: unknown = undefined;
          let parsedOk = false;
          if (rawText.length > 0) {
            try {
              parsed = JSON.parse(rawText) as unknown;
              parsedOk = true;
            } catch {
              parsedOk = false;
            }
          }

          if (statusCode < 200 || statusCode >= 300) {
            if (parsedOk && isRecord(parsed) && typeof parsed.code === "string" && typeof parsed.message === "string") {
              finishReject(new Error(formatCoreApiError(endpointPath, statusCode, parsed.code, parsed.message)));
              return;
            }
            const fallbackDetail = rawText || res.statusMessage || "request failed";
            finishReject(new Error(`Vantry Core HTTP ${statusCode} (${endpointPath}): ${fallbackDetail}`));
            return;
          }

          if (rawText.length === 0) {
            finishResolve({} as T);
            return;
          }
          if (!parsedOk) {
            finishReject(new Error(`Invalid JSON response from Vantry Core (${endpointPath})`));
            return;
          }
          finishResolve(parsed as T);
        });
      },
    );

    req.on("error", (err) => {
      finishReject(new Error(`Vantry Core socket request failed (${endpointPath}): ${err.message}`));
    });

    if (signal) {
      onAbort = () => {
        const abortErr = new Error(`Vantry Core request to ${endpointPath} was aborted`);
        req.destroy(abortErr);
        finishReject(abortErr);
      };
      signal.addEventListener("abort", onAbort, { once: true });
    }

    if (payload !== undefined) {
      req.write(payload);
    }
    req.end();
  });
}

function dispatchCoreCommand(
  commandFields: Record<string, unknown>,
  clientRequestId?: string,
  signal?: AbortSignal,
): Promise<Record<string, unknown>> {
  return callCoreSocket<Record<string, unknown>>(
    "POST",
    "/v1/commands",
    {
      version: PROTOCOL_VERSION,
      commandId: allocateCommandId(clientRequestId),
      ...commandFields,
    },
    signal,
  );
}

function resolveJobDir(): string {
  const explicit = readEnv(ENV_JOB_DIR);
  if (explicit) {
    return path.resolve(explicit);
  }
  const dataDir = readEnv(ENV_DATA_DIR);
  const attemptId = readEnv(ENV_ATTEMPT_ID);
  if (dataDir && attemptId) {
    if (!SAFE_ID_PATTERN.test(attemptId) || attemptId === "." || attemptId === "..") {
      throw new Error("Invalid VANTRY_ATTEMPT_ID for job directory resolution");
    }
    return path.join(path.resolve(dataDir), "jobs", attemptId);
  }
  throw new Error(`Missing ${ENV_JOB_DIR} (or ${ENV_DATA_DIR} + ${ENV_ATTEMPT_ID}) for secret storage`);
}

function execVantryCtl(ctlArgs: string[], signal?: AbortSignal): Promise<Record<string, unknown>> {
  const executable = requireEnv(ENV_EXECUTABLE);
  const dataDir = requireEnv(ENV_DATA_DIR);
  const fullArgs = ["ctl", "--data-dir", dataDir, ...ctlArgs];

  return new Promise((resolve, reject) => {
    execFileCallback(
      executable,
      fullArgs,
      {
        encoding: "utf8",
        maxBuffer: 16 * 1024 * 1024,
        windowsHide: true,
        ...(signal ? { signal } : {}),
      },
      (error, stdout, stderr) => {
        if (error) {
          const detail = (stderr || stdout || error.message || "unknown ctl error").trim();
          reject(new Error(`vantry ctl ${ctlArgs[0] ?? ""} failed: ${detail}`));
          return;
        }
        const trimmed = stdout.trim();
        if (trimmed.length === 0) {
          resolve({});
          return;
        }
        try {
          const parsed = JSON.parse(trimmed) as unknown;
          if (isRecord(parsed)) {
            resolve(parsed);
            return;
          }
          resolve({ result: parsed });
        } catch {
          resolve({ output: trimmed });
        }
      },
    );
  });
}

function toolJsonResult(payload: unknown) {
  return {
    content: [{ type: "text" as const, text: JSON.stringify(payload) }],
    structuredContent: payload,
    details: payload,
  };
}

function validateEnumValue<T extends readonly string[]>(
  fieldName: string,
  raw: string | undefined,
  allowed: T,
): T[number] | undefined {
  const trimmed = raw?.trim();
  if (!trimmed) {
    return undefined;
  }
  const normalized = trimmed.toLowerCase();
  if (!(allowed as readonly string[]).includes(normalized)) {
    throw new Error(`Unsupported ${fieldName}: ${JSON.stringify(raw)} (expected ${allowed.join(" | ")})`);
  }
  return normalized as T[number];
}

function validatePositiveInt(fieldName: string, value: number | undefined): number | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`${fieldName} must be a positive integer`);
  }
  return value;
}

function validateNonNegativeInt(fieldName: string, value: number | undefined): number | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (!Number.isInteger(value) || value < 0) {
    throw new Error(`${fieldName} must be a non-negative integer`);
  }
  return value;
}

function buildWorkspaceSpec(
  params: {
    workspaceKind?: string;
    workspacePath?: string;
    repository?: string;
    branch?: string;
    baseRef?: string;
  },
  baseCwd?: string,
): Record<string, string> | undefined {
  const rawKind = params.workspaceKind?.trim();
  const rawPath = params.workspacePath?.trim();
  const rawRepository = params.repository?.trim();
  const branch = params.branch?.trim();
  const baseRef = params.baseRef?.trim();

  if (!rawKind && !rawPath && !rawRepository && !branch && !baseRef) {
    return undefined;
  }

  const kind =
    validateEnumValue("workspaceKind", rawKind, WORKSPACE_KINDS) ?? (rawRepository ? "worktree" : "directory");
  const resolveDir = (target: string) => (baseCwd ? path.resolve(baseCwd, target) : path.resolve(target));

  const spec: Record<string, string> = { kind };
  if (rawPath) {
    spec.path = resolveDir(rawPath);
  }
  if (rawRepository) {
    spec.repository = resolveDir(rawRepository);
  }
  if (branch) {
    spec.branch = branch;
  }
  if (baseRef) {
    spec.baseRef = baseRef;
  }
  if (kind === "directory" && !spec.path) {
    throw new Error("directory workspace requires workspacePath");
  }
  return spec;
}

function truncateText(value: unknown, limit: number): { text?: string; truncated?: boolean } {
  if (typeof value !== "string" || value.length === 0) {
    return {};
  }
  if (value.length <= limit) {
    return { text: value };
  }
  return {
    text: value.slice(0, limit) + "...[truncated]",
    truncated: true,
  };
}

function shellSingleQuote(value: string): string {
  return "'" + value.replaceAll("'", "'\\''") + "'";
}

async function writeOneTimeSecretFile(jobDir: string, secretValue: string): Promise<string> {
  const secretsDir = path.join(jobDir, "secrets");
  await fs.mkdir(secretsDir, { recursive: true, mode: 0o700 });
  await fs.chmod(secretsDir, 0o700);

  for (let attempt = 0; attempt < 16; attempt++) {
    secretSequence += 1;
    const fileName = `secret-${process.pid}-${Date.now()}-${secretSequence}.secret`;
    const secretFilePath = path.join(secretsDir, fileName);
    try {
      const handle = await fs.open(secretFilePath, "wx", 0o600);
      try {
        await handle.chmod(0o600);
        await handle.writeFile(secretValue, "utf8");
      } finally {
        await handle.close();
      }
      return secretFilePath;
    } catch (error) {
      const code = (error as NodeJS.ErrnoException | undefined)?.code;
      if (code === "EEXIST") {
        continue;
      }
      throw new Error("Failed to create one-time secret file");
    }
  }
  throw new Error("Failed to allocate unique secret file path");
}

const targetEnvironmentProperty = Type.Optional(Type.String({ description: "Linked target environment ID from vantry_environment_list; omit for the local Core" }));
const remoteWorkspaceSchema = Type.Object({
  kind: StringEnum(WORKSPACE_KINDS),
  path: Type.Optional(Type.String({ description: "Absolute path on the TARGET machine" })),
  repository: Type.Optional(Type.String({ description: "Git repository on the TARGET machine" })),
  branch: Type.Optional(Type.String()),
  baseRef: Type.Optional(Type.String()),
});

const orchestratorCapabilitiesSchema = Type.Object({
  environmentId: targetEnvironmentProperty,
  search: Type.Optional(Type.String({ description: "Optional filter keyword for available Pi models" })),
});
type OrchestratorCapabilitiesInput = Static<typeof orchestratorCapabilitiesSchema>;

const delegateTaskSchema = Type.Object({
  environmentId: targetEnvironmentProperty,
  workspace: Type.Optional(remoteWorkspaceSchema),
  task: Type.Optional(
    Type.String({
      description: "Self-contained instructions for the delegated child Pi session (preferred field; alias: message)",
    }),
  ),
  message: Type.Optional(
    Type.String({
      description: "Clear, self-contained instructions for the delegated child task (legacy alias for task)",
    }),
  ),
  title: Type.Optional(Type.String({ description: "Optional short title for the child task" })),
  role: Type.Optional(
    StringEnum(DELEGATE_ROLES, {
      description: "Optional role hint for the delegated child agent",
    }),
  ),
  provider: Type.Optional(Type.String({ description: "Execution provider (pi; omitted inherits the parent)" })),
  model: Type.Optional(Type.String({ description: "Optional child model in Pi provider/model format" })),
  thinking: Type.Optional(Type.String({ description: "Optional thinking/effort override for the child session" })),
  isolation: Type.Optional(
    StringEnum(ISOLATION_MODES, {
      description: "'shared' (default: parent checkout) or 'worktree' (isolated git worktree checkout)",
    }),
  ),
  mode: Type.Optional(
    StringEnum(DELEGATE_MODES, {
      description: "'async' (default: non-blocking, Core wakes parent on settle) or 'wait' (block for result)",
    }),
  ),
  timeoutMs: Type.Optional(
    Type.Integer({ minimum: 1, description: "Optional wait timeout in milliseconds (mode: 'wait' only)" }),
  ),
  approvalPolicy: Type.Optional(
    StringEnum(APPROVAL_POLICIES, {
      description: "Optional approval policy override for the child session",
    }),
  ),
  interactionMode: Type.Optional(
    StringEnum(INTERACTION_MODES, {
      description: "Optional interaction mode ('default' or 'plan')",
    }),
  ),
  resumePolicy: Type.Optional(
    StringEnum(RESUME_POLICIES, {
      description: "Parent continuation policy when the child completes (default: auto)",
    }),
  ),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key for safe retries" })),
});
type DelegateTaskInput = Static<typeof delegateTaskSchema>;

const taskStatusSchema = Type.Object({
  taskId: Type.Optional(Type.String({ description: "Optional task ID to inspect a single task" })),
  acknowledge: Type.Optional(
    Type.Boolean({ description: "When true, mark delivered manual results acknowledged on read" }),
  ),
});
type TaskStatusInput = Static<typeof taskStatusSchema>;

const taskCancelSchema = Type.Object({
  taskId: Type.String({ description: "Delegated Vantry task ID to cancel" }),
  reason: Type.Optional(Type.String({ description: "Optional cancellation reason" })),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type TaskCancelInput = Static<typeof taskCancelSchema>;

const taskFollowupSchema = Type.Object({
  taskId: Type.String({ description: "Existing Vantry delegated task ID" }),
  message: Type.String({ description: "Follow-up instructions to send to the child task" }),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type TaskFollowupInput = Static<typeof taskFollowupSchema>;

const taskAckSchema = Type.Object({
  generation: Type.Integer({ minimum: 1, description: "Cohort delivery generation number" }),
  deliveryMessageId: Type.String({ description: "Delivery message ID to acknowledge" }),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type TaskAckInput = Static<typeof taskAckSchema>;

const threadListSchema = Type.Object({
  environmentId: targetEnvironmentProperty,
  workspace: Type.Optional(Type.String({ description: "Optional workspace path filter" })),
  allProjects: Type.Optional(Type.Boolean({ description: "Include threads across all workspaces/projects" })),
  includeSubagents: Type.Optional(Type.Boolean({ description: "Include delegated child subagent threads" })),
  settled: Type.Optional(Type.Boolean({ description: "Filter by settled (archived) shelf state" })),
  snoozed: Type.Optional(Type.Boolean({ description: "Filter by snoozed state" })),
  statuses: Type.Optional(
    Type.Array(Type.String(), {
      description: "Filter by thread statuses (idle, queued, running, waiting_children, completed, failed, cancelled, interrupted)",
    }),
  ),
  titleContains: Type.Optional(Type.String({ description: "Case-insensitive substring match on thread title" })),
  cursor: Type.Optional(Type.Integer({ minimum: 0, description: "Pagination offset cursor" })),
  limit: Type.Optional(Type.Integer({ minimum: 1, description: "Max threads to return" })),
});
type ThreadListInput = Static<typeof threadListSchema>;

const threadSearchSchema = Type.Object({
  query: Type.String({ description: "Literal search query across thread titles, branches, workspaces, and messages" }),
  includeArchived: Type.Optional(Type.Boolean({ description: "Include settled/archived threads in search results" })),
  limit: Type.Optional(Type.Integer({ minimum: 1, description: "Maximum search hits to return" })),
});
type ThreadSearchInput = Static<typeof threadSearchSchema>;

const threadReadSchema = Type.Object({
  environmentId: targetEnvironmentProperty,
  threadId: Type.String({ description: "Target Vantry thread ID to read" }),
  view: Type.Optional(
    StringEnum(THREAD_READ_VIEWS, {
      description: "'messages' (user & assistant messages) or 'activity' (full timeline including tools/approvals)",
    }),
  ),
  afterPosition: Type.Optional(
    Type.Integer({ minimum: 0, description: "Return timeline items strictly after this position cursor" }),
  ),
  beforePosition: Type.Optional(
    Type.Integer({ minimum: 0, description: "Return timeline items strictly before this position cursor" }),
  ),
  limit: Type.Optional(Type.Integer({ minimum: 1, description: "Maximum timeline items to return" })),
  runLimit: Type.Optional(Type.Integer({ minimum: 1, description: "Maximum recent run summaries to include" })),
  maxCharsPerItem: Type.Optional(
    Type.Integer({ minimum: 1, description: "Per-item character limit before truncation" }),
  ),
  itemId: Type.Optional(Type.String({ description: "Read a specific timeline item by itemId" })),
  textOffset: Type.Optional(
    Type.Integer({ minimum: 0, description: "Character offset when paging a truncated item" }),
  ),
});
type ThreadReadInput = Static<typeof threadReadSchema>;

const threadSendSchema = Type.Object({
  environmentId: targetEnvironmentProperty,
  threadId: Type.String({ description: "Target Vantry Pi session threadId to send the message to" }),
  message: Type.String({ description: "Message, question, or instructions to send to the peer Pi session" }),
  mode: Type.Optional(
    StringEnum(THREAD_SEND_MODES, {
      description: "'auto' (start if idle, queue if running), 'queue' (append to queue), or 'steer' (steer active run)",
    }),
  ),
  notifyOnComplete: Type.Optional(
    Type.Boolean({
      description: "Automatically wake this caller session with a bounded summary when the target run settles",
    }),
  ),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type ThreadSendInput = Static<typeof threadSendSchema>;

const threadWaitSchema = Type.Object({
  environmentId: targetEnvironmentProperty,
  threadId: Type.String({ description: "Target Vantry thread ID to await" }),
  runId: Type.Optional(Type.String({ description: "Optional specific runId on the target thread to await" })),
  timeoutMs: Type.Optional(Type.Integer({ minimum: 1, description: "Maximum wait duration in milliseconds" })),
  untilDelegatedWorkSettled: Type.Optional(
    Type.Boolean({ description: "Keep waiting until the target thread's delegated child tasks also settle" }),
  ),
});
type ThreadWaitInput = Static<typeof threadWaitSchema>;

const threadLaunchSpecProperties = {
  environmentId: targetEnvironmentProperty,
  title: Type.Optional(Type.String({ description: "Optional title for the new Pi session" })),
  message: Type.Optional(Type.String({ description: "Optional initial prompt to start running immediately" })),
  model: Type.Optional(Type.String({ description: "Optional model in provider/model format" })),
  thinking: Type.Optional(Type.String({ description: "Optional thinking/effort override" })),
  approvalPolicy: Type.Optional(
    StringEnum(APPROVAL_POLICIES, { description: "Optional approval policy for the new session" }),
  ),
  interactionMode: Type.Optional(
    StringEnum(INTERACTION_MODES, { description: "Optional interaction mode ('default' or 'plan')" }),
  ),
  workspaceKind: Type.Optional(
    StringEnum(WORKSPACE_KINDS, { description: "Optional workspace kind ('directory' or 'worktree')" }),
  ),
  workspacePath: Type.Optional(Type.String({ description: "Optional workspace directory path" })),
  repository: Type.Optional(Type.String({ description: "Git repository path when workspaceKind is 'worktree'" })),
  branch: Type.Optional(Type.String({ description: "Optional git branch for worktree checkout" })),
  baseRef: Type.Optional(Type.String({ description: "Optional git base ref for worktree checkout" })),
  scratch: Type.Optional(Type.Boolean({ description: "Use an isolated scratch chat directory without a repo" })),
};

const threadLaunchSchema = Type.Object({
  ...threadLaunchSpecProperties,
  notifyOnComplete: Type.Optional(
    Type.Boolean({
      description: "Automatically wake this caller session with a bounded summary when the launched run settles",
    }),
  ),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type ThreadLaunchInput = Static<typeof threadLaunchSchema>;

const createThreadsItemSchema = Type.Object(threadLaunchSpecProperties);
type CreateThreadsItemInput = Static<typeof createThreadsItemSchema>;

const createThreadsSchema = Type.Object({
  threads: Type.Array(createThreadsItemSchema, {
    minItems: 1,
    maxItems: MAX_BATCH_THREADS,
    description: "Batch of up to 20 peer Pi threads to launch in parallel",
  }),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type CreateThreadsInput = Static<typeof createThreadsSchema>;

const threadTransferSchema = Type.Object({
  action: StringEnum(THREAD_TRANSFER_ACTIONS, {
    description: "'fork' (branch a new thread from settled history) or 'merge_back' (deliver latest completed result back to target thread)",
  }),
  threadId: Type.Optional(
    Type.String({ description: "Source thread ID (defaults to current thread when omitted)" }),
  ),
  targetThreadId: Type.Optional(
    Type.String({ description: "Destination thread ID for merge_back (defaults to fork/parent origin)" }),
  ),
  sourceRunId: Type.Optional(Type.String({ description: "Optional settled run ID cutoff when action is 'fork'" })),
  title: Type.Optional(Type.String({ description: "Optional title for the forked thread" })),
  model: Type.Optional(Type.String({ description: "Optional provider/model override when action is 'fork'" })),
  workspaceKind: Type.Optional(
    StringEnum(WORKSPACE_KINDS, { description: "Optional workspace kind ('directory' or 'worktree') when action is 'fork'" }),
  ),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type ThreadTransferInput = Static<typeof threadTransferSchema>;

const threadManageSchema = Type.Object({
  action: StringEnum(THREAD_MANAGE_ACTIONS, {
    description: "Management action to apply to a Vantry session",
  }),
  threadId: Type.Optional(Type.String({ description: "Target thread ID (defaults to current thread where applicable)" })),
  title: Type.Optional(Type.String({ description: "New title when action is 'rename'" })),
  model: Type.Optional(Type.String({ description: "Model ID in provider/model format when action is 'set_model'" })),
  snoozedUntil: Type.Optional(
    Type.String({ description: "RFC3339 timestamp until which the thread is snoozed (when action is 'snooze')" }),
  ),
  runId: Type.Optional(Type.String({ description: "Target run ID for 'interrupt' or 'respond_request'" })),
  requestId: Type.Optional(Type.String({ description: "Pending provider dialog request ID for 'respond_request'" })),
  value: Type.Optional(Type.String({ description: "Text/select value for 'respond_request'" })),
  confirmed: Type.Optional(Type.Boolean({ description: "Boolean confirmation for 'respond_request'" })),
  cancelled: Type.Optional(Type.Boolean({ description: "Cancel dialog for 'respond_request'" })),
  clientRequestId: Type.Optional(Type.String({ description: "Optional idempotency key" })),
});
type ThreadManageInput = Static<typeof threadManageSchema>;

const requestSecretSchema = Type.Object({
  prompt: Type.String({
    description: "Prompt shown in Vantry's native dialog explaining what secret is requested",
  }),
  envVarName: Type.Optional(
    Type.String({
      description: "Optional environment variable name hint (e.g. OPENAI_API_KEY)",
    }),
  ),
});
type RequestSecretInput = Static<typeof requestSecretSchema>;

const terminalRunSchema = Type.Object({
  command: Type.String({
    description:
      "Shell command to run interactively inside Vantry's built-in Terminal (real PTY in the workspace directory). Use for login, 2FA/OTP, SSH prompts, sudo, or interactive TTY setup wizards.",
  }),
  title: Type.Optional(
    Type.String({
      description: "Short human-readable label for the interactive terminal tab (e.g. 'Steam Guard 设置')",
    }),
  ),
  cwd: Type.Optional(
    Type.String({
      description: "Optional working directory (relative to workspace or absolute; defaults to workspace root)",
    }),
  ),
});
type TerminalRunInput = Static<typeof terminalRunSchema>;

function serializeThreadLaunchSpec(spec: CreateThreadsItemInput, baseCwd?: string): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (spec.environmentId?.trim()) out.environmentId = spec.environmentId.trim();
  const title = spec.title?.trim();
  if (title) {
    out.title = title;
  }
  if (spec.message !== undefined && spec.message.trim().length > 0) {
    out.message = spec.message;
  }
  const model = spec.model?.trim();
  if (model) {
    out.model = model;
  }
  const thinking = spec.thinking?.trim();
  if (thinking) {
    out.thinking = thinking;
  }
  const approvalPolicy = validateEnumValue("approvalPolicy", spec.approvalPolicy, APPROVAL_POLICIES);
  if (approvalPolicy) {
    out.approvalPolicy = approvalPolicy;
  }
  const interactionMode = validateEnumValue("interactionMode", spec.interactionMode, INTERACTION_MODES);
  if (interactionMode) {
    out.interactionMode = interactionMode;
  }
  if (spec.environmentId?.trim() && [spec.workspacePath, spec.repository].some(value => value && !path.isAbsolute(value))) {
    throw new Error("Remote workspaces require absolute paths on the target machine");
  }
  const workspace = buildWorkspaceSpec(spec, spec.environmentId?.trim() ? undefined : baseCwd);
  if (workspace) {
    out.workspace = workspace;
  }
  if (spec.scratch === true) {
    out.scratch = true;
  }
  return out;
}

function isHighSurrogate(code: number): boolean {
  return code >= 0xd800 && code <= 0xdbff;
}

function isLowSurrogate(code: number): boolean {
  return code >= 0xdc00 && code <= 0xdfff;
}

function safeSliceHead(text: string, maxChars: number): string {
  if (maxChars <= 0) return "";
  if (text.length <= maxChars) return text;
  let end = maxChars;
  if (end > 0 && isHighSurrogate(text.charCodeAt(end - 1)) && isLowSurrogate(text.charCodeAt(end))) {
    end -= 1;
  }
  return text.slice(0, end);
}

function safeSliceTail(text: string, maxChars: number): string {
  if (maxChars <= 0) return "";
  if (text.length <= maxChars) return text;
  let start = text.length - maxChars;
  if (start > 0 && start < text.length && isLowSurrogate(text.charCodeAt(start)) && isHighSurrogate(text.charCodeAt(start - 1))) {
    start += 1;
  }
  return text.slice(start);
}

function parseCodemodeMaxOutputTokens(code: unknown): number {
  if (typeof code !== "string" || code.length === 0) {
    return DEFAULT_CODEMODE_MAX_OUTPUT_TOKENS;
  }
  const firstLine = code.split(/\r?\n/, 1)[0] ?? "";
  const match = /^\s*\/\/\s*@options:\s*(\{.*\})\s*$/.exec(firstLine);
  if (!match) {
    return DEFAULT_CODEMODE_MAX_OUTPUT_TOKENS;
  }
  try {
    const parsed = JSON.parse(match[1]) as unknown;
    if (isRecord(parsed) && typeof parsed.max_output_tokens === "number") {
      const n = parsed.max_output_tokens;
      if (Number.isInteger(n) && n > 0) {
        return n;
      }
    }
  } catch {
    // Ignore malformed @options line and fall back to Pi's default budget.
  }
  return DEFAULT_CODEMODE_MAX_OUTPUT_TOKENS;
}

/**
 * Water-filling fair budget allocator: sections smaller than the current fair
 * share keep 100% of their content so small/middle outputs are never wiped out
 * by oversized siblings.
 */
export function allocateSectionBudgets(lengths: readonly number[], totalBudget: number): number[] {
  const budgets = new Array<number>(lengths.length).fill(0);
  let budgetLeft = Math.max(0, Math.floor(totalBudget));
  const remaining = new Set<number>();
  for (let i = 0; i < lengths.length; i++) {
    if (lengths[i] > 0) remaining.add(i);
  }

  while (remaining.size > 0 && budgetLeft > 0) {
    const share = Math.floor(budgetLeft / remaining.size);
    if (share === 0) {
      for (const idx of remaining) {
        if (budgetLeft <= 0) break;
        budgets[idx] = 1;
        budgetLeft -= 1;
      }
      break;
    }
    let absorbed = false;
    for (const idx of [...remaining]) {
      if (lengths[idx] <= share) {
        budgets[idx] = lengths[idx];
        budgetLeft -= lengths[idx];
        remaining.delete(idx);
        absorbed = true;
      }
    }
    if (!absorbed) {
      const base = Math.floor(budgetLeft / remaining.size);
      let rem = budgetLeft - base * remaining.size;
      for (const idx of remaining) {
        budgets[idx] = base + (rem > 0 ? 1 : 0);
        if (rem > 0) rem -= 1;
      }
      break;
    }
  }

  return budgets;
}

interface CodemodeOutputSection {
  label: string;
  body: string;
  startLine?: number;
  endLine?: number;
  isConsole?: boolean;
}

export interface RebalanceCodemodeResult {
  text: string;
  stats: {
    mode: string;
    totalSections: number;
    intactSections: number;
    trimmedSections: number;
    onlyRoutineRetrieval: boolean;
  };
}

export function rebalanceCodemodeFullOutput(
  rawFullOutput: string,
  opts: {
    fullOutputPath: string;
    maxOutputTokens: number;
    isError: boolean;
    calls: ReadonlyArray<{ name?: string; status?: string }>;
  },
): RebalanceCodemodeResult | undefined {
  const lines = rawFullOutput.split("\n");
  let endIdx = lines.length;

  let scriptErrorSection: { text: string; startLine: number; endLine: number } | undefined;
  if (opts.isError) {
    for (let i = endIdx - 1; i >= 0; i--) {
      if (lines[i] === "Script error:") {
        scriptErrorSection = {
          text: lines.slice(i, endIdx).join("\n"),
          startLine: i + 1,
          endLine: endIdx,
        };
        endIdx = i;
        break;
      }
    }
  }

  let consoleSection: CodemodeOutputSection | undefined;
  if (endIdx > 0 && lines[endIdx - 1] === "</console_output>") {
    for (let j = endIdx - 2; j >= 0; j--) {
      if (lines[j] === "<console_output>") {
        consoleSection = {
          label: "<console_output>",
          body: lines.slice(j + 1, endIdx - 1).join("\n"),
          startLine: j + 1,
          endLine: endIdx,
          isConsole: true,
        };
        endIdx = j;
        break;
      }
    }
  }

  const contentSections: CodemodeOutputSection[] = [];
  let mode = "single";

  const firstMarker = endIdx > 0 ? /^==> text 1\/(\d+) <==$/.exec(lines[0]) : null;
  if (firstMarker) {
    const expectedTotal = Number(firstMarker[1]);
    if (Number.isInteger(expectedTotal) && expectedTotal >= 2) {
      const markerPositions: number[] = [0];
      let nextExpected = 2;
      for (let i = 1; i < endIdx && nextExpected <= expectedTotal; i++) {
        if (lines[i] === `==> text ${nextExpected}/${expectedTotal} <==`) {
          markerPositions.push(i);
          nextExpected += 1;
        }
      }
      if (markerPositions.length === expectedTotal) {
        mode = "multi-text";
        for (let idx = 0; idx < markerPositions.length; idx++) {
          const markerIdx = markerPositions[idx];
          const bodyStart = markerIdx + 1;
          const bodyEnd = idx + 1 < markerPositions.length ? markerPositions[idx + 1] : endIdx;
          contentSections.push({
            label: lines[markerIdx],
            body: lines.slice(bodyStart, bodyEnd).join("\n"),
            startLine: bodyStart + 1,
            endLine: Math.max(bodyStart + 1, bodyEnd),
          });
        }
      }
    }
  }

  if (contentSections.length === 0) {
    const bodyText = lines.slice(0, endIdx).join("\n");
    const trimmedBody = bodyText.trim();
    let parsedJsonSections = false;
    if ((trimmedBody.startsWith("[") || trimmedBody.startsWith("{")) && bodyText.length <= MAX_CODEMODE_JSON_PARSE_CHARS) {
      try {
        const parsed = JSON.parse(trimmedBody) as unknown;
        if (Array.isArray(parsed) && parsed.length >= 2) {
          parsedJsonSections = true;
          mode = "json-array";
          if (parsed.length <= 40) {
            for (let i = 0; i < parsed.length; i++) {
              contentSections.push({
                label: `[${i}]`,
                body: JSON.stringify(parsed[i]) ?? String(parsed[i]),
              });
            }
          } else {
            for (let i = 0; i < 20; i++) {
              contentSections.push({
                label: `[${i}]`,
                body: JSON.stringify(parsed[i]) ?? String(parsed[i]),
              });
            }
            contentSections.push({
              label: "[...]",
              body: `omitted ${parsed.length - 30} middle items [20..${parsed.length - 11}] — inspect ${opts.fullOutputPath}`,
            });
            for (let i = parsed.length - 10; i < parsed.length; i++) {
              contentSections.push({
                label: `[${i}]`,
                body: JSON.stringify(parsed[i]) ?? String(parsed[i]),
              });
            }
          }
        } else if (isRecord(parsed)) {
          const keys = Object.keys(parsed);
          if (keys.length >= 2) {
            parsedJsonSections = true;
            mode = "json-object";
            if (keys.length <= 40) {
              for (const key of keys) {
                contentSections.push({
                  label: `${key}:`,
                  body: JSON.stringify(parsed[key]) ?? String(parsed[key]),
                });
              }
            } else {
              for (const key of keys.slice(0, 25)) {
                contentSections.push({
                  label: `${key}:`,
                  body: JSON.stringify(parsed[key]) ?? String(parsed[key]),
                });
              }
              const omittedKeys = keys.slice(25, keys.length - 5);
              contentSections.push({
                label: "[...]",
                body: `omitted ${omittedKeys.length} middle keys (${omittedKeys.slice(0, 8).join(", ")}${omittedKeys.length > 8 ? ", ..." : ""}) — inspect ${opts.fullOutputPath}`,
              });
              for (const key of keys.slice(keys.length - 5)) {
                contentSections.push({
                  label: `${key}:`,
                  body: JSON.stringify(parsed[key]) ?? String(parsed[key]),
                });
              }
            }
          }
        }
      } catch {
        // Not valid JSON; treat as plain body below.
      }
    }
    if (!parsedJsonSections && bodyText.length > 0) {
      contentSections.push({
        label: "",
        body: bodyText,
        startLine: 1,
        endLine: Math.max(1, endIdx),
      });
    }
  }

  if (consoleSection) {
    contentSections.push(consoleSection);
    if (mode === "single") {
      mode = "text-with-console";
    }
  }

  const onlyRoutineRetrieval =
    opts.calls.length > 0 && opts.calls.every((c) => typeof c.name === "string" && DIRECT_RETRIEVAL_TOOLS.has(c.name));

  if (!onlyRoutineRetrieval && contentSections.length < 2 && !scriptErrorSection) {
    return undefined;
  }

  const totalBudgetChars = Math.min(
    Math.max(Math.floor(opts.maxOutputTokens * CODEMODE_CHARS_PER_TOKEN), 2_000),
    200_000,
  );

  let renderedScriptError = "";
  if (scriptErrorSection) {
    const errBudget = Math.max(400, Math.floor(totalBudgetChars * 0.25));
    if (scriptErrorSection.text.length <= errBudget) {
      renderedScriptError = scriptErrorSection.text;
    } else {
      const marker = `\n…error truncated [Full error: ${opts.fullOutputPath} lines ${scriptErrorSection.startLine}-${scriptErrorSection.endLine}]…\n`;
      const avail = Math.max(80, errBudget - marker.length);
      const headLen = Math.ceil(avail * 0.7);
      const tailLen = avail - headLen;
      renderedScriptError =
        safeSliceHead(scriptErrorSection.text, headLen) + marker + safeSliceTail(scriptErrorSection.text, tailLen);
    }
  }

  const footer = `\n\n[Full output: ${opts.fullOutputPath} (read with offset/limit)]`;
  const routineHint = onlyRoutineRetrieval
    ? "\nNote: this codemode call only wrapped routine read/grep/find/ls calls; call those native tools directly in parallel next time."
    : "";
  const fixedMetaOverhead =
    220 +
    routineHint.length +
    footer.length +
    (renderedScriptError ? renderedScriptError.length + 2 : 0) +
    contentSections.reduce((sum, s) => sum + (s.label ? s.label.length + 1 : 0) + 2, 0);

  const contentBudget = Math.max(
    Math.max(300, Math.floor(totalBudgetChars * 0.35)),
    totalBudgetChars - fixedMetaOverhead,
  );
  const budgets = allocateSectionBudgets(
    contentSections.map((s) => s.body.length),
    contentBudget,
  );

  let intactSections = 0;
  let trimmedSections = 0;
  const renderedSections: string[] = [];

  for (let i = 0; i < contentSections.length; i++) {
    const section = contentSections[i];
    const budget = budgets[i];
    let formattedBody: string;
    if (section.body.length <= budget) {
      intactSections += 1;
      formattedBody = section.body;
    } else {
      trimmedSections += 1;
      const lineHint =
        section.startLine !== undefined && section.endLine !== undefined
          ? ` [Full section: ${opts.fullOutputPath} lines ${section.startLine}-${section.endLine}]`
          : "";
      const fullMarker = `\n…${section.body.length} chars omitted${lineHint}…\n`;
      const useCompactMarker = budget < fullMarker.length + 32;
      const sampleMarker = useCompactMarker ? `\n…${section.body.length} omitted…\n` : fullMarker;
      const avail = Math.max(24, budget - sampleMarker.length);
      const headChars = Math.ceil(avail * 0.65);
      const tailChars = avail - headChars;
      const head = safeSliceHead(section.body, headChars);
      const tail = safeSliceTail(section.body, tailChars);
      const omittedChars = Math.max(0, section.body.length - head.length - tail.length);
      const actualMarker = useCompactMarker
        ? `\n…${omittedChars} omitted…\n`
        : `\n…${omittedChars} chars omitted${lineHint}…\n`;
      formattedBody = head + actualMarker + tail;
    }

    if (section.isConsole) {
      renderedSections.push(`<console_output>\n${formattedBody}\n</console_output>`);
    } else if (section.label.startsWith("==> text ")) {
      renderedSections.push(`${section.label}\n${formattedBody}`);
    } else if (section.label) {
      renderedSections.push(`${section.label} ${formattedBody}`);
    } else {
      renderedSections.push(formattedBody);
    }
  }

  const header =
    `Warning: codemode output exceeded budget (~${Math.ceil(rawFullOutput.length / CODEMODE_CHARS_PER_TOKEN)} tokens across ${contentSections.length} sections); ` +
    `preserved ${intactSections}/${contentSections.length} intact and trimmed ${trimmedSections} individually instead of merged middle truncation.` +
    routineHint;

  let text = `${header}\n\n${renderedSections.join("\n")}`;
  if (renderedScriptError) {
    text += `\n\n${renderedScriptError}`;
  }
  text += footer;

  return {
    text,
    stats: {
      mode,
      totalSections: contentSections.length,
      intactSections,
      trimmedSections,
      onlyRoutineRetrieval,
    },
  };
}

function tryResolveJobDir(): string | undefined {
  try {
    return resolveJobDir();
  } catch {
    return undefined;
  }
}

function sanitizePathSegment(value: string | undefined, fallback: string): string {
  const cleaned = (value ?? "")
    .replace(/[^A-Za-z0-9._-]+/g, "_")
    .replace(/^[._]+|[._]+$/g, "")
    .slice(0, 48);
  if (!cleaned || cleaned === "." || cleaned === "..") {
    return fallback;
  }
  return cleaned;
}

function countLines(text: string): number {
  if (!text) return 0;
  let lines = 1;
  for (let i = 0; i < text.length; i++) {
    if (text.charCodeAt(i) === 10) lines += 1;
  }
  return lines;
}

async function writeToolSpillFile(
  jobDir: string,
  toolName: string,
  toolCallId: string | undefined,
  content: string,
): Promise<string | undefined> {
  try {
    const outDir = path.join(jobDir, "tool-outputs");
    await fs.mkdir(outDir, { recursive: true, mode: 0o700 });
    toolSpillSequence += 1;
    const safeTool = sanitizePathSegment(toolName, "tool");
    const safeId = sanitizePathSegment(toolCallId, String(process.pid));
    const filePath = path.join(outDir, `${toolSpillSequence}_${safeTool}_${safeId}.txt`);
    await fs.writeFile(filePath, content, { encoding: "utf8", mode: 0o600 });
    return filePath;
  } catch {
    return undefined;
  }
}

export function detectOutputDiagnosticHints(text: string, opts?: { pruned?: boolean }): string[] {
  const hints: string[] = [];
  if (!text) return hints;

  let replacementCount = 0;
  for (let i = 0; i < text.length; i++) {
    if (text.charCodeAt(i) === 0xfffd) {
      replacementCount += 1;
    }
  }
  const sampleDenom = Math.max(1, Math.min(text.length, 8_192));
  if (replacementCount >= 8 && (replacementCount >= 20 || replacementCount / sampleDenom >= 0.01)) {
    hints.push(
      `[Encoding hint: detected ${replacementCount} U+FFFD replacement chars (likely GBK/GB18030 or non-UTF-8 encoding); fetch raw bytes and decode explicitly, e.g. python3 with bytes.decode('gb18030', errors='replace').]`,
    );
  }

  if (opts?.pruned || text.length >= 8_000) {
    const headSample = text.slice(0, 4_096).toLowerCase();
    if (headSample.includes("<!doctype html") || headSample.includes("<html")) {
      const tagMatches = headSample.match(
        /<(?:script|style|div|table|tr|td|head|body|meta|link|a|p|span|ul|ol|li|form|input|section|article|header|footer|title|h[1-6])\b/g,
      );
      if (tagMatches && tagMatches.length >= 4) {
        hints.push(
          "[HTML hint: raw HTML output detected; parse only needed text/links from the saved file with a short python3 script instead of dumping raw HTML into context.]",
        );
      }
    }
  }

  return hints;
}

export interface PruneToolResultOptions {
  toolName: string;
  toolCallId?: string;
  existingFullOutputPath?: string;
  jobDir?: string;
}

export interface PruneToolResultOutcome {
  changed: boolean;
  text: string;
  spillPath?: string;
  hints: string[];
  omittedChars: number;
}

export async function pruneToolResultText(
  rawText: string,
  opts: PruneToolResultOptions,
): Promise<PruneToolResultOutcome> {
  const oversized = rawText.length > TOOL_OUTPUT_GUARD_MAX_CHARS;
  const hints = detectOutputDiagnosticHints(rawText, { pruned: oversized });
  if (!oversized) {
    if (hints.length === 0) {
      return { changed: false, text: rawText, hints: [], omittedChars: 0 };
    }
    return {
      changed: true,
      text: `${rawText}\n\n${hints.join("\n")}`,
      hints,
      omittedChars: 0,
    };
  }

  let spillPath: string | undefined;
  const existing = opts.existingFullOutputPath?.trim();
  if (existing) {
    try {
      const st = await fs.stat(existing);
      if (st.isFile()) spillPath = existing;
    } catch {
      spillPath = undefined;
    }
  }
  if (!spillPath && opts.jobDir) {
    spillPath = await writeToolSpillFile(opts.jobDir, opts.toolName, opts.toolCallId, rawText);
  }

  let head = safeSliceHead(rawText, TOOL_OUTPUT_GUARD_HEAD_CHARS);
  const headNl = head.lastIndexOf("\n");
  if (headNl > 512 && head.length - headNl <= 300) {
    head = head.slice(0, headNl);
  }
  let tail = safeSliceTail(rawText, TOOL_OUTPUT_GUARD_TAIL_CHARS);
  const tailNl = tail.indexOf("\n");
  if (tailNl >= 0 && tailNl <= 300 && tail.length - (tailNl + 1) > 512) {
    tail = tail.slice(tailNl + 1);
  }

  const omittedChars = Math.max(0, rawText.length - head.length - tail.length);
  const totalLines = countLines(rawText);
  const marker = `\n…[${omittedChars} chars omitted from ${rawText.length} chars / ${totalLines} lines]…\n`;
  const footer = spillPath
    ? `\n\n[Full output saved: ${spillPath} (use read with offset/limit or python3/grep instead of reloading all)]`
    : "";
  const hintBlock = hints.length > 0 ? `\n\n${hints.join("\n")}` : "";

  return {
    changed: true,
    text: `${head}${marker}${tail}${footer}${hintBlock}`,
    spillPath,
    hints,
    omittedChars,
  };
}

function canonicalStringify(value: unknown, depth = 0): string {
  if (value === null || value === undefined) return "null";
  if (typeof value === "string") return JSON.stringify(value);
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (depth >= 6) return "\"[max-depth]\"";
  if (Array.isArray(value)) {
    return "[" + value.map((item) => canonicalStringify(item, depth + 1)).join(",") + "]";
  }
  if (typeof value === "object") {
    const obj = value as Record<string, unknown>;
    const keys = Object.keys(obj).sort();
    return (
      "{" +
      keys
        .filter((k) => obj[k] !== undefined)
        .map((k) => `${JSON.stringify(k)}:${canonicalStringify(obj[k], depth + 1)}`)
        .join(",") +
      "}"
    );
  }
  return JSON.stringify(String(value));
}

function canonicalInputSignature(toolName: string, input: unknown): string {
  return `${toolName}:${canonicalStringify(input ?? {}).slice(0, 512)}`;
}

function normalizeOutputFingerprint(text: string): string {
  const norm = text.replace(/\s+/g, " ").trim();
  if (norm.length <= 512) {
    return `${norm}:${norm.length}`;
  }
  return `${norm.slice(0, 256)}..${norm.slice(-256)}:${norm.length}`;
}

export interface LoopGuardObservation {
  toolName: string;
  input: unknown;
  text: string;
  isError?: boolean;
}

export interface LoopGuardTrigger {
  kind: "consecutive_failures" | "failure_streak" | "repeated_call";
  count: number;
  hint: string;
}

interface LoopGuardEntry {
  toolName: string;
  inputSig: string;
  outputSig: string;
  isError: boolean;
}

export class ToolLoopGuard {
  private consecutiveErrorsByTool = new Map<string, number>();
  private totalConsecutiveErrors = 0;
  private history: LoopGuardEntry[] = [];

  observe(obs: LoopGuardObservation): LoopGuardTrigger | undefined {
    const toolName = obs.toolName || "unknown";
    const isError = Boolean(obs.isError);
    const inputSig = canonicalInputSignature(toolName, obs.input);
    const outputSig = normalizeOutputFingerprint(obs.text ?? "");

    if (isError) {
      const toolCount = (this.consecutiveErrorsByTool.get(toolName) ?? 0) + 1;
      this.consecutiveErrorsByTool.set(toolName, toolCount);
      this.totalConsecutiveErrors += 1;
      this.pushHistory({ toolName, inputSig, outputSig, isError: true });

      if (toolCount >= LOOP_GUARD_FAILURE_THRESHOLD) {
        return {
          kind: "consecutive_failures",
          count: toolCount,
          hint: `[Loop guard: ${toolName} failed ${toolCount} times in a row. Stop repeating the same invocation—change strategy (e.g. inspect headers/encoding/selectors with a minimal test script, use a different approach, or explain the blocker to the user).]`,
        };
      }
      if (this.totalConsecutiveErrors >= LOOP_GUARD_STREAK_THRESHOLD) {
        return {
          kind: "failure_streak",
          count: this.totalConsecutiveErrors,
          hint: `[Loop guard: ${this.totalConsecutiveErrors} consecutive tool failures across tools. Pause and diagnose the root cause or change approach before making more attempts.]`,
        };
      }
      return undefined;
    }

    this.consecutiveErrorsByTool.set(toolName, 0);
    this.totalConsecutiveErrors = 0;

    if (MUTATING_TOOLS.has(toolName)) {
      // Editing or writing files changes workspace state, so prior read/test
      // cycles must not be flagged as no-progress repetition.
      this.history = [];
      return undefined;
    }

    this.pushHistory({ toolName, inputSig, outputSig, isError: false });

    let consecutiveSameInput = 0;
    for (let i = this.history.length - 1; i >= 0; i--) {
      const entry = this.history[i];
      if (!entry.isError && entry.toolName === toolName && entry.inputSig === inputSig) {
        consecutiveSameInput += 1;
      } else {
        break;
      }
    }
    if (consecutiveSameInput >= LOOP_GUARD_REPEAT_THRESHOLD) {
      return {
        kind: "repeated_call",
        count: consecutiveSameInput,
        hint: `[Loop guard: ${toolName} was called ${consecutiveSameInput} consecutive times with identical arguments. Stop repeating the same call—use existing results, try a different approach, or explain the blocker.]`,
      };
    }

    let identicalInputOutput = 0;
    for (const entry of this.history) {
      if (!entry.isError && entry.toolName === toolName && entry.inputSig === inputSig && entry.outputSig === outputSig) {
        identicalInputOutput += 1;
      }
    }
    if (identicalInputOutput >= LOOP_GUARD_REPEAT_THRESHOLD) {
      return {
        kind: "repeated_call",
        count: identicalInputOutput,
        hint: `[Loop guard: ${toolName} returned identical results ${identicalInputOutput} times for the same arguments with no file edits in between. Stop polling/retrying the same call and advance or report the blocker.]`,
      };
    }

    return undefined;
  }

  private pushHistory(entry: LoopGuardEntry) {
    this.history.push(entry);
    if (this.history.length > LOOP_GUARD_HISTORY_SIZE) {
      this.history.splice(0, this.history.length - LOOP_GUARD_HISTORY_SIZE);
    }
  }
}

const TOOL_SEARCH_STOP_WORDS = new Set([
  "a",
  "an",
  "and",
  "are",
  "as",
  "at",
  "be",
  "by",
  "for",
  "from",
  "in",
  "is",
  "it",
  "of",
  "on",
  "or",
  "that",
  "the",
  "this",
  "to",
  "with",
]);

const TOOL_SEARCH_GENERIC_TERMS = new Set([
  "vantry",
  "tool",
  "local",
  "command",
  "cmd",
  "run",
  "exec",
  "execute",
  "open",
  "launch",
  "start",
  "panel",
  "ui",
  "native",
  "interactive",
  "user",
  "input",
  "output",
  "call",
  "use",
  "get",
  "set",
  "help",
  "file",
  "system",
  "app",
  "session",
  "work",
  "workspace",
  "current",
  "mode",
  "client",
  "host",
  "window",
  "tab",
  "action",
  "do",
  "make",
  "new",
  "manage",
  "control",
  "direct",
  "directly",
]);

function stemSearchTerm(term: string): string {
  if (term.length > 4 && term.endsWith("ies")) return `${term.slice(0, -3)}y`;
  if (term.length > 4 && /(ches|shes|sses|xes|zes)$/.test(term)) return term.slice(0, -2);
  if (term.length > 3 && term.endsWith("s") && !term.endsWith("ss")) return term.slice(0, -1);
  return term;
}

function tokenizeSearchText(text: string): string[] {
  const normalized = text
    .normalize("NFC")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1 $2")
    .toLowerCase()
    .replace(/[^\p{L}\p{M}\p{N}]+/gu, " ");
  return normalized
    .split(/\s+/)
    .filter((word) => word.length > 0 && !TOOL_SEARCH_STOP_WORDS.has(word))
    .map((word) => (/^[a-z0-9]+$/.test(word) ? stemSearchTerm(word) : word));
}

function normalizeSearchToolName(name: string): string {
  return name.trim().toLowerCase().replace(/[^a-z0-9_$]/g, "_");
}

function collectSchemaSearchTerms(schema: unknown, out: string[], depth = 0): void {
  if (!isRecord(schema) || depth > 5) return;
  if (typeof schema.description === "string") out.push(schema.description);
  if (isRecord(schema.properties)) {
    for (const [key, prop] of Object.entries(schema.properties)) {
      out.push(key);
      collectSchemaSearchTerms(prop, out, depth + 1);
    }
  }
  if (schema.items !== undefined) collectSchemaSearchTerms(schema.items, out, depth + 1);
}

export interface SearchableToolSummary {
  name: string;
  description?: string;
  parameters?: unknown;
  exposure?: string;
}

export interface ToolSearchGuardDecision {
  allow: boolean;
  reason?: string;
}

/**
 * Prevents Pi's BM25 tool_search from activating unrelated deferred vantry_*
 * tools (and busting the prompt KV cache) when a query only overlaps generic
 * boilerplate/brand terms like "vantry", "local", "launch", or "input".
 */
export function evaluateToolSearchGuard(
  query: string,
  allTools: ReadonlyArray<SearchableToolSummary>,
  activeToolNames: ReadonlyArray<string>,
): ToolSearchGuardDecision {
  const trimmed = query.trim();
  if (!trimmed || allTools.length === 0) {
    return { allow: true };
  }
  const activeSet = new Set(activeToolNames);
  const inactiveSearchable = allTools.filter(
    (t) => (t.exposure === "deferred" || t.exposure === "codemode") && !activeSet.has(t.name),
  );
  if (inactiveSearchable.length === 0) {
    return {
      allow: false,
      reason: "All searchable tools are already loaded; no deferred tools remain to load.",
    };
  }

  const exactQuery = normalizeSearchToolName(trimmed);
  for (const tool of inactiveSearchable) {
    const normalized = normalizeSearchToolName(tool.name);
    if (normalized === exactQuery || normalized.endsWith(`__${exactQuery}`)) {
      return { allow: true };
    }
  }

  const queryTerms = [...new Set(tokenizeSearchText(trimmed))];
  if (queryTerms.length === 0) {
    return { allow: true };
  }
  const specificTerms = queryTerms.filter((term) => !TOOL_SEARCH_GENERIC_TERMS.has(term));

  const toolTermsOf = (tool: SearchableToolSummary): Set<string> => {
    const parts: string[] = [tool.name, tool.description ?? ""];
    collectSchemaSearchTerms(tool.parameters, parts);
    return new Set(tokenizeSearchText(parts.join(" ")));
  };

  if (specificTerms.length > 0) {
    for (const tool of inactiveSearchable) {
      const terms = toolTermsOf(tool);
      if (specificTerms.some((term) => terms.has(term))) {
        return { allow: true };
      }
    }
    const activeMatches = allTools
      .filter((t) => activeSet.has(t.name) && t.name !== "tool_search")
      .filter((t) => {
        const terms = toolTermsOf(t);
        return specificTerms.some((term) => terms.has(term));
      })
      .map((t) => t.name);
    if (activeMatches.length > 0) {
      return {
        allow: false,
        reason: `No inactive deferred tools match [${specificTerms.join(", ")}]. Already-loaded tools matching your query: ${activeMatches.join(", ")}. Call them directly instead of tool_search.`,
      };
    }
    return {
      allow: false,
      reason: `No deferred tools match [${specificTerms.join(", ")}]. Deferred tools only cover Vantry orchestration (delegate/thread/task/environment) and configured MCP servers; use your already-loaded tools directly (bash, vantry_terminal_run, vantry_request_secret, read/grep/find/edit/write).`,
    };
  }

  return {
    allow: false,
    reason: `Query ${JSON.stringify(trimmed)} contains only generic terms and would load unrelated orchestration tools. Search by a specific capability or tool name (e.g. "delegate", "thread_read", "environment", or an MCP tool name), or use already-loaded tools directly (bash, vantry_terminal_run, vantry_request_secret).`,
  };
}

export interface ContinuityGuardInput {
  assistantText: string;
  lastUserText: string;
  sawMidRunUserMessage: boolean;
  toolCallsInAttempt: number;
  toolCallsSinceLastUser: number;
}

export interface ContinuityGuardDecision {
  shouldContinue: boolean;
  reason?: "steer_dropped_execution" | "dangling_next_step";
  instruction?: string;
}

const USER_STOP_INTENT_RE =
  /(?:^(?:停|停止|先停|别做|先别|算了|不用做|不用了|取消|暂停|只回答|先回答|stop|cancel|abort|pause|never\s*mind|don't\s+(?:do|run|change)|just\s+(?:answer|explain))\b|(?:停下|别继续|先别做|不要执行|暂停执行|停止执行|stop\s+here|don't\s+continue))/i;

const ASSISTANT_USER_HANDOFF_RE =
  /(?:[?？]\s*$|请确认|确认后|请回复|回复[「“"']|告诉我|请先|请你|需要你|你来|请选择|是否要|要不要|是否继续|你确定|麻烦你|切到.{0,12}窗口|完成后再|please confirm|let me know|would you like|do you want|confirm if|should i proceed|tell me when)/i;

const DANGLING_NEXT_STEP_RE =
  /(?:(?:^|[。！!\n])\s*(?:接下来|下一步|随后|紧接着|下面|现在)(?:我|我会|我将|我来|只做|继续|直接|先|再|去|开始|着手|补|改|跑|测|验证|执行|打通|开启|重启|启动)|(?:^|[。！!\n])\s*(?:后续|接下来)(?:应|应该|需要|将|会)(?:让程序|直接|改用|改为|自动|接入|补齐|完善)|(?:^|[.!\n])\s*(?:next,?\s+i(?:'ll| will)|i(?:'ll| will)\s+(?:now|next|proceed to|continue to|go ahead and)|let me\s+(?:now|next|proceed to))\b)/i;

const CONDITIONAL_USER_GATE_RE =
  /(?:完成后|确认后|等你|你确认|如果|若|once you|after you|when you|if you)/i;

const STEER_CORRECTION_ACK_RE =
  /(?:是我选错|我误用|我理解错|说错了|搞错了|正确做法是|应该直接|不用走|不走|跳过|绕过|改为|接下来|下一步|my mistake|wrong approach|wrong path|instead|skip|bypass)/i;

const ALREADY_COMPLETED_RE =
  /(?:全部完成|已完成全部|已经全部完成|均已通过|all done|everything is complete)/i;

/**
 * Detects high-confidence premature stops before agent settlement:
 * 1) A mid-run steer redirected the agent, it acknowledged/corrected course with
 *    text only (0 tools), and tried to settle without executing the fix.
 * 2) The final reply ends by narrating a concrete next action the agent itself
 *    will take ("接下来只做...", "I'll now...") without calling any tool or
 *    waiting on user input.
 */
export function evaluateContinuityGuard(input: ContinuityGuardInput): ContinuityGuardDecision {
  const text = input.assistantText.trim();
  if (!text) {
    return { shouldContinue: false };
  }
  if (USER_STOP_INTENT_RE.test(input.lastUserText.trim())) {
    return { shouldContinue: false };
  }
  if (/```(?:ask|dispatch)\b/.test(text)) {
    return { shouldContinue: false };
  }

  const paragraphs = text
    .split(/\n\s*\n/)
    .map((p) => p.trim())
    .filter(Boolean);
  const tail = paragraphs.length > 0 ? paragraphs[paragraphs.length - 1] : text;
  if (ASSISTANT_USER_HANDOFF_RE.test(tail)) {
    return { shouldContinue: false };
  }

  const danglingMatch = DANGLING_NEXT_STEP_RE.exec(tail);
  if (danglingMatch) {
    const lineStart = tail.lastIndexOf("\n", danglingMatch.index);
    const sentencePrefix = tail.slice(lineStart + 1, danglingMatch.index);
    if (!CONDITIONAL_USER_GATE_RE.test(sentencePrefix)) {
      return {
        shouldContinue: true,
        reason: "dangling_next_step",
        instruction:
          "Your previous message ended by stating a next step you intend to take instead of executing it. Execute that step now and verify the result before finishing; only stop if genuinely blocked on user input, approval, or an unrecoverable error.",
      };
    }
  }

  if (
    input.sawMidRunUserMessage &&
    input.toolCallsInAttempt > 0 &&
    input.toolCallsSinceLastUser === 0 &&
    STEER_CORRECTION_ACK_RE.test(text) &&
    !ALREADY_COMPLETED_RE.test(tail)
  ) {
    return {
      shouldContinue: true,
      reason: "steer_dropped_execution",
      instruction:
        "You answered the user's mid-run steering message without resuming execution. Do not stop after explaining or apologizing: immediately carry out the corrected next step and verify the outcome. Only stop if genuinely blocked waiting on user input, approval, or an explicit stop instruction.",
    };
  }

  return { shouldContinue: false };
}

function extractMessagePlainText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const chunks: string[] = [];
  for (const part of content) {
    if (isRecord(part) && part.type === "text" && typeof part.text === "string") {
      chunks.push(part.text);
    }
  }
  return chunks.join("\n");
}

function messageHasToolCall(content: unknown): boolean {
  if (!Array.isArray(content)) return false;
  return content.some((part) => isRecord(part) && part.type === "toolCall");
}

export default function vantryToolsExtension(pi: ExtensionAPI) {
  const isDelegatedChild = Boolean(readEnv(ENV_PARENT_TASK_ID));
  const directOrchestration = !isDelegatedChild && isTruthyEnv(ENV_DIRECT_ORCHESTRATION);
  const directTools = directOrchestration
    ? new Set(["vantry_orchestrator_capabilities", "vantry_delegate_task", "vantry_task_status"])
    : new Set<string>();
  const loopGuard = new ToolLoopGuard();

  let toolCallsInAttempt = 0;
  let toolCallsSinceLastUser = 0;
  let sawFirstUserMessage = false;
  let sawMidRunUserMessage = false;
  let lastUserText = "";
  let continuityNudgeCount = 0;

  if (typeof pi.on === "function") {
    pi.on("agent_start", () => {
      toolCallsInAttempt = 0;
      toolCallsSinceLastUser = 0;
      sawFirstUserMessage = false;
      sawMidRunUserMessage = false;
      lastUserText = "";
      continuityNudgeCount = 0;
    });

    pi.on("message_end", (event) => {
      const msg = (event as { message?: unknown }).message;
      if (!isRecord(msg) || msg.role !== "user") return;
      if (sawFirstUserMessage) {
        sawMidRunUserMessage = true;
      }
      sawFirstUserMessage = true;
      toolCallsSinceLastUser = 0;
      lastUserText = extractMessagePlainText(msg.content);
    });

    pi.on("tool_execution_start", (event) => {
      if ((event as { parentToolCallId?: unknown }).parentToolCallId) return;
      toolCallsInAttempt += 1;
      toolCallsSinceLastUser += 1;
    });

    pi.on("before_agent_start", (event) => {
      const opts = event.systemPromptOptions;
      if (opts.selectedTools.includes("codemode") || opts.toolGuidelines.codemode) {
        opts.toolGuidelines.codemode = [...VANTRY_CODEMODE_GUIDELINES];
        opts.toolSnippets.codemode = VANTRY_CODEMODE_SNIPPET;

        // pi-rust calls `tools.apply_to_prompt` after `before_agent_start`, overwriting
        // `toolSnippets` and `toolGuidelines`, while preserving `sections` overrides in place.
        // Render `tools` and `rules` sections here so Vantry's codemode guidance always sticks.
        if (!opts.customPrompt && isRecord(opts.sections)) {
          const hidden = new Set(Array.isArray(opts.hiddenTools) ? opts.hiddenTools : []);
          const declared = opts.selectedTools.filter((name) => !hidden.has(name));
          const visibleTools = declared
            .map((name) => {
              const snippet = typeof opts.toolSnippets?.[name] === "string" ? opts.toolSnippets[name].trim() : "";
              return snippet ? `- ${name}: ${snippet}` : "";
            })
            .filter(Boolean);
          const toolsBody = visibleTools.length > 0 ? visibleTools.join("\n") : "(none)";
          opts.sections.tools = `${toolsBody}\n\nIn addition to the tools above, you may have access to other custom tools depending on the project.`;

          const rules: string[] = [];
          const addRule = (rule: unknown) => {
            if (typeof rule !== "string") return;
            const trimmed = rule.trim();
            if (trimmed && !rules.includes(trimmed)) rules.push(trimmed);
          };
          const hasTool = (name: string) => declared.includes(name);
          const hasBash = hasTool("bash");
          const hasPowerShell = hasTool("powershell");
          if ((hasBash || hasPowerShell) && !hasTool("grep") && !hasTool("find") && !hasTool("ls")) {
            addRule(
              hasBash && hasPowerShell
                ? "Use bash or PowerShell for file operations like listing, searching, and finding files"
                : hasPowerShell
                  ? "Use PowerShell for file operations like listing, searching, and finding files"
                  : "Use bash for file operations like ls, rg, find",
            );
          }
          for (const name of declared) {
            const list = opts.toolGuidelines?.[name];
            if (Array.isArray(list)) {
              for (const rule of list) addRule(rule);
            }
          }
          if (Array.isArray(opts.promptGuidelines)) {
            for (const rule of opts.promptGuidelines) addRule(rule);
          }
          addRule("Be concise in your responses");
          addRule("Show file paths clearly when working with files");
          opts.sections.rules = rules.map((r) => `- ${r}`).join("\n");
        }
      }
    });

    pi.on("tool_call", (event) => {
      if (event.toolName !== "tool_search") return;
      if (typeof pi.getAllTools !== "function" || typeof pi.getActiveTools !== "function") return;
      const query = isRecord(event.input) && typeof event.input.query === "string" ? event.input.query : "";
      if (!query.trim()) return;
      const decision = evaluateToolSearchGuard(query, pi.getAllTools(), pi.getActiveTools());
      if (!decision.allow) {
        return { block: true, reason: decision.reason };
      }
    });

    pi.on("agent_before_settle", (event) => {
      if (continuityNudgeCount >= 1) return;
      if (readEnv(ENV_INTERACTION_MODE)?.toLowerCase() === "plan") return;
      if (event.outcome !== "completed" || !event.context?.canContinue) return;
      const messages = Array.isArray(event.context.contextMessages) ? event.context.contextMessages : [];
      let lastAssistant: Record<string, unknown> | undefined;
      for (let i = messages.length - 1; i >= 0; i--) {
        const candidate = messages[i];
        if (isRecord(candidate) && candidate.role === "assistant") {
          lastAssistant = candidate;
          break;
        }
      }
      if (!lastAssistant) return;
      const stopReason = typeof lastAssistant.stopReason === "string" ? lastAssistant.stopReason : "stop";
      if (stopReason !== "stop" || messageHasToolCall(lastAssistant.content)) return;
      const assistantText = extractMessagePlainText(lastAssistant.content);
      const decision = evaluateContinuityGuard({
        assistantText,
        lastUserText,
        sawMidRunUserMessage,
        toolCallsInAttempt,
        toolCallsSinceLastUser,
      });
      if (!decision.shouldContinue || !decision.instruction) return;
      continuityNudgeCount += 1;
      return {
        entries: [
          ...event.entries,
          {
            type: "custom_message" as const,
            customType: "vantry_continuity_guard",
            content: decision.instruction,
            display: false,
          },
        ],
        continue: true,
      };
    });

    pi.on("tool_result", async (event) => {
      if (event.parentToolCallId) return;

      let content = Array.isArray(event.content) ? [...event.content] : [];
      let details: Record<string, unknown> | undefined = isRecord(event.details) ? { ...event.details } : undefined;
      let modified = false;

      if (event.toolName === "codemode") {
        if (isRecord(event.details) && typeof event.details.fullOutputPath === "string") {
          const fullOutputPath = event.details.fullOutputPath.trim();
          if (fullOutputPath) {
            try {
              const stat = await fs.stat(fullOutputPath);
              if (stat.isFile() && stat.size <= MAX_CODEMODE_REBALANCE_FILE_BYTES) {
                const rawFullOutput = await fs.readFile(fullOutputPath, "utf8");
                const calls = Array.isArray(event.details.calls)
                  ? event.details.calls.filter(isRecord).map((c) => ({
                      name: typeof c.name === "string" ? c.name : undefined,
                      status: typeof c.status === "string" ? c.status : undefined,
                    }))
                  : [];
                const rebalanced = rebalanceCodemodeFullOutput(rawFullOutput, {
                  fullOutputPath,
                  maxOutputTokens: parseCodemodeMaxOutputTokens(event.input?.code),
                  isError: event.isError,
                  calls,
                });
                if (rebalanced) {
                  const hasContent = content.length >= 2;
                  const headerBlock = hasContent ? content[0] : { type: "text" as const, text: "" };
                  const originalBodyBlock = hasContent ? content[1] : content[0];
                  const trailingBlocks = hasContent ? content.slice(2) : [];
                  let preservedImageLabels = "";
                  if (trailingBlocks.some((b) => b.type === "image") && originalBodyBlock?.type === "text") {
                    const lines = originalBodyBlock.text.split("\n");
                    const imageLines: string[] = [];
                    for (let i = lines.length - 1; i >= 0; i--) {
                      if (/^\[Image\b/.test(lines[i])) imageLines.unshift(lines[i]);
                      else break;
                    }
                    if (imageLines.length > 0) preservedImageLabels = "\n" + imageLines.join("\n");
                  }
                  const newBodyBlock = { type: "text" as const, text: rebalanced.text + preservedImageLabels };
                  content = hasContent ? [headerBlock, newBodyBlock, ...trailingBlocks] : [newBodyBlock, ...trailingBlocks];
                  details = { ...(details ?? {}), rebalanced: rebalanced.stats };
                  modified = true;
                }
              }
            } catch {
              // Fail open: preserve Pi's original output if fullOutputPath cannot be read.
            }
          }
        }
      } else if (!TOOL_OUTPUT_SKIP_PRUNE.has(event.toolName) && !event.toolName.startsWith("vantry_")) {
        const textIndices: number[] = [];
        const textChunks: string[] = [];
        for (let i = 0; i < content.length; i++) {
          const block = content[i];
          if (block?.type === "text" && typeof block.text === "string") {
            textIndices.push(i);
            textChunks.push(block.text);
          }
        }
        const combinedText = textChunks.join("\n");
        if (combinedText.length > 0) {
          const existingFullOutputPath =
            isRecord(event.details) && typeof event.details.fullOutputPath === "string"
              ? event.details.fullOutputPath
              : undefined;
          const pruned = await pruneToolResultText(combinedText, {
            toolName: event.toolName,
            toolCallId: typeof (event as { toolCallId?: unknown }).toolCallId === "string"
              ? (event as { toolCallId?: string }).toolCallId
              : undefined,
            existingFullOutputPath,
            jobDir: tryResolveJobDir(),
          });
          if (pruned.changed) {
            const nonTextBlocks = content.filter((_, idx) => !textIndices.includes(idx));
            content = [{ type: "text" as const, text: pruned.text }, ...nonTextBlocks];
            details = {
              ...(details ?? {}),
              vantryPruned: {
                originalChars: combinedText.length,
                omittedChars: pruned.omittedChars,
                ...(pruned.spillPath ? { spillPath: pruned.spillPath } : {}),
                ...(pruned.hints.length > 0 ? { hints: pruned.hints.length } : {}),
              },
            };
            modified = true;
          }
        }
      }

      const observedText = content
        .filter((b): b is { type: "text"; text: string } => b?.type === "text" && typeof b.text === "string")
        .map((b) => b.text)
        .join("\n");
      const guard = loopGuard.observe({
        toolName: event.toolName,
        input: event.input,
        text: observedText,
        isError: event.isError,
      });
      if (guard) {
        if (event.toolName.startsWith("vantry_") && !event.isError) {
          // Keep content[0].text as pure JSON for non-error Vantry tools.
          content = [...content, { type: "text" as const, text: guard.hint }];
        } else {
          let lastTextIdx = -1;
          for (let i = content.length - 1; i >= 0; i--) {
            if (content[i]?.type === "text" && typeof content[i].text === "string") {
              lastTextIdx = i;
              break;
            }
          }
          if (lastTextIdx >= 0) {
            const prev = content[lastTextIdx] as { type: "text"; text: string };
            content[lastTextIdx] = { type: "text" as const, text: prev.text ? `${prev.text}\n\n${guard.hint}` : guard.hint };
          } else {
            content = [{ type: "text" as const, text: guard.hint }, ...content];
          }
        }
        details = { ...(details ?? {}), vantryLoopGuard: { kind: guard.kind, count: guard.count } };
        modified = true;
      }

      if (!modified) return;
      return details !== undefined ? { content, details } : { content };
    });
  }
  const registerTool: ExtensionAPI["registerTool"] = (tool) => {
    pi.registerTool({
      ...tool,
      exposure: MODEL_ONLY_UI_TOOLS.has(tool.name) ? "model-only" : directTools.has(tool.name) ? "direct" : "deferred",
      namespace: {
        name: "vantry",
        description: "Delegated child tasks, peer threads, and remote machines",
        instructions: "Orchestration tools for delegation, peer threads, and paired environments. Keep environmentId on remote requests. Async child tasks wake the parent automatically.",
      },
      outputSchema: Type.Record(Type.String(), Type.Unknown()),
    });
  };
  registerTool({
    name: "vantry_environment_list",
    label: "Vantry Linked Environments",
    description: "List saved paired machines and SSH environments usable for cross-machine Agent delegation. This does not install, pair, or start a remote Core.",
    parameters: Type.Object({}),
    async execute(_toolCallId, _params, signal, _onUpdate, ctx) {
      const response = await callCoreSocket<Record<string, unknown>>("POST", "/v1/workbench", { action: "orchestration.environments" }, signal ?? ctx.signal);
      return toolJsonResult(typeof response.output === "string" ? JSON.parse(response.output) : response);
    },
  });

  registerTool({
    name: "vantry_orchestrator_capabilities",
    label: "Vantry Orchestrator Capabilities",
    description:
      "Inspect inherited session settings, available Pi models, thinking options, approval policies, and batch limits before delegating with custom model/thinking overrides.",
    promptSnippet: "List Vantry Pi models, thinking options, and orchestration capabilities",
    parameters: orchestratorCapabilitiesSchema,
    async execute(_toolCallId, params: OrchestratorCapabilitiesInput, signal, _onUpdate, ctx) {
      const body: Record<string, unknown> = {};
      const callerThreadId = readEnv(ENV_THREAD_ID);
      if (callerThreadId) {
        body.callerThreadId = callerThreadId;
      }
      const search = params.search?.trim();
      if (search) {
        body.search = search;
      }
      if (params.environmentId?.trim()) body.environmentId = params.environmentId.trim();
      const capabilities = await callCoreSocket<Record<string, unknown>>(
        "POST",
        "/v1/orchestration/capabilities",
        body,
        signal ?? ctx.signal,
      );
      return toolJsonResult(capabilities);
    },
  });

  registerTool({
    name: "vantry_delegate_task",
    label: "Vantry Delegate Task",
    description:
      "Delegate a durable child Pi task through Vantry Core (supports shared or isolated git worktree checkouts, async or wait mode). Do NOT poll async tasks — finish your turn and Vantry Core wakes this session automatically when child work settles.",
    promptSnippet: "Delegate a durable child Pi task through Vantry Core",
    promptGuidelines: [
      "Ordinary coding tasks need no orchestration tools. Delegate only when requested or allowed, using vantry_delegate_task (durable, shown under this thread, wakes parent on settle).",
      "On vantry_delegate_task or vantry_orchestrator_capabilities error, quote the error; never fall back to native subagents, scripted workflows, other CLIs, or another model. Do not poll async tasks.",
    ],
    parameters: delegateTaskSchema,
    async execute(_toolCallId, params: DelegateTaskInput, signal, _onUpdate, ctx) {
      const rawTask = params.task?.trim() ? params.task : params.message?.trim() ? params.message : "";
      if (!rawTask.trim()) {
        throw new Error("vantry_delegate_task requires a non-empty task (or message)");
      }
      const resumePolicy = validateEnumValue("resumePolicy", params.resumePolicy ?? "auto", RESUME_POLICIES) ?? "auto";
      const role = validateEnumValue("role", params.role, DELEGATE_ROLES);
      const isolation = validateEnumValue("isolation", params.isolation, ISOLATION_MODES);
      const mode = validateEnumValue("mode", params.mode, DELEGATE_MODES);
      const timeoutMs = validatePositiveInt("timeoutMs", params.timeoutMs);
      const approvalPolicy = validateEnumValue("approvalPolicy", params.approvalPolicy, APPROVAL_POLICIES);
      const interactionMode = validateEnumValue("interactionMode", params.interactionMode, INTERACTION_MODES);

      const parentRunId = requireEnv(ENV_RUN_ID);
      const parentAttemptId = readEnv(ENV_ATTEMPT_ID);
      const callerThreadId = readEnv(ENV_THREAD_ID);
      const title = params.title?.trim();
      const model = params.model?.trim();
      const thinking = params.thinking?.trim();
      const clientRequestId = params.clientRequestId?.trim();
      const activeSignal = signal ?? ctx.signal;

      if (resolveSocketPath()) {
        const body: Record<string, unknown> = {
          parentRunId,
          task: rawTask,
          resumePolicy,
        };
        if (params.environmentId?.trim()) body.environmentId = params.environmentId.trim();
        if (params.workspace) body.workspace = params.workspace;
        if (clientRequestId) body.clientRequestId = clientRequestId;
        if (callerThreadId) body.callerThreadId = callerThreadId;
        if (parentAttemptId) body.parentAttemptId = parentAttemptId;
        if (title) body.title = title;
        if (role) body.role = role;
        if (params.provider?.trim()) body.providerId = params.provider.trim();
        if (model) body.model = model;
        if (thinking) body.thinking = thinking;
        if (isolation) body.isolation = isolation;
        if (mode) body.mode = mode;
        if (timeoutMs !== undefined) body.timeoutMs = timeoutMs;
        if (approvalPolicy) body.approvalPolicy = approvalPolicy;
        if (interactionMode) body.interactionMode = interactionMode;

        const detail = await callCoreSocket<Record<string, unknown>>(
          "POST",
          "/v1/orchestration/delegate",
          body,
          activeSignal,
        );
        return toolJsonResult(detail);
      }

      if (params.environmentId?.trim()) throw new Error("Remote delegation requires the Vantry Core socket; it never falls back to local CLI execution");
      const args = ["delegate", "--parent-run", parentRunId];
      if (parentAttemptId) {
        args.push("--parent-attempt", parentAttemptId);
      }
      args.push("--message", rawTask, "--resume-policy", resumePolicy);
      if (params.provider?.trim()) args.push("--provider", params.provider.trim());
      if (title) {
        args.push("--title", title);
      }
      if (model) {
        args.push("--model", model);
      }
      if (clientRequestId) {
        args.push("--command-id", clientRequestId);
      }
      if (approvalPolicy === "auto" || approvalPolicy === "ask") {
        args.push("--approval", approvalPolicy);
      }

      const receipt = await execVantryCtl(args, activeSignal);
      return toolJsonResult(receipt);
    },
  });

  registerTool({
    name: "vantry_task_status",
    label: "Vantry Task Status",
    description:
      "Read delegated tasks and cohort status associated with the current Vantry parent thread and run. Do not busy-poll running async tasks.",
    promptSnippet: "Inspect delegated child tasks and delivery status for the current Vantry thread",
    parameters: taskStatusSchema,
    async execute(_toolCallId, params: TaskStatusInput, signal, _onUpdate, ctx) {
      const threadId = readEnv(ENV_THREAD_ID);
      const runId = readEnv(ENV_RUN_ID);
      const filterTaskId = params.taskId?.trim();
      const activeSignal = signal ?? ctx.signal;

      if (resolveSocketPath()) {
        const query: Record<string, unknown> = {};
        if (threadId) query.callerThreadId = threadId;
        // A task remains addressable after a new user turn creates another run.
        // Preserve current-run scoping only when listing without an explicit ID.
        if (runId && !filterTaskId) query.parentRunId = runId;
        if (filterTaskId) query.taskId = filterTaskId;
        if (params.acknowledge === true) query.acknowledge = true;

        const result = await callCoreSocket<Record<string, unknown>>(
          "POST",
          "/v1/orchestration/task-status",
          query,
          activeSignal,
        );
        return toolJsonResult(result);
      }

      const snapshot = await execVantryCtl(["status"], activeSignal);
      const rawTasks = Array.isArray(snapshot.tasks) ? snapshot.tasks : [];
      const rawCohorts = Array.isArray(snapshot.cohorts) ? snapshot.cohorts : [];

      const matchedParentRuns = new Set<string>();
      if (runId) {
        matchedParentRuns.add(runId);
      }

      const tasks: Array<Record<string, unknown>> = [];
      for (const item of rawTasks) {
        if (!isRecord(item)) continue;
        const belongsToCurrent =
          threadId || runId
            ? (threadId !== undefined && item.parentThreadId === threadId) ||
              (runId !== undefined && item.parentRunId === runId)
            : true;
        if (!belongsToCurrent) continue;
        if (filterTaskId && item.id !== filterTaskId) continue;

        if (typeof item.parentRunId === "string" && item.parentRunId.length > 0) {
          matchedParentRuns.add(item.parentRunId);
        }

        const resultPreview = truncateText(item.result, MAX_RESULT_PREVIEW_CHARS);
        const errorPreview = truncateText(item.error, MAX_RESULT_PREVIEW_CHARS);
        const summary: Record<string, unknown> = {
          id: item.id,
          title: item.title,
          parentThreadId: item.parentThreadId,
          parentRunId: item.parentRunId,
          childThreadId: item.childThreadId,
          childRunId: item.childRunId,
          generation: item.generation,
          status: item.status,
          deliveryStatus: item.deliveryStatus,
          ...(item.deliveryMessageId ? { deliveryMessageId: item.deliveryMessageId } : {}),
          ...(resultPreview.text !== undefined ? { result: resultPreview.text } : {}),
          ...(resultPreview.truncated ? { resultTruncated: true } : {}),
          ...(errorPreview.text !== undefined ? { error: errorPreview.text } : {}),
          ...(errorPreview.truncated ? { errorTruncated: true } : {}),
          updatedAt: item.updatedAt,
        };
        tasks.push(summary);
      }

      const cohorts: Array<Record<string, unknown>> = [];
      for (const item of rawCohorts) {
        if (!isRecord(item)) continue;
        if (
          matchedParentRuns.size > 0 &&
          (typeof item.parentRunId !== "string" || !matchedParentRuns.has(item.parentRunId))
        ) {
          continue;
        }
        cohorts.push({
          parentRunId: item.parentRunId,
          generation: item.generation,
          resumePolicy: item.resumePolicy,
          status: item.status,
          ...(item.deliveryMessageId ? { deliveryMessageId: item.deliveryMessageId } : {}),
          ...(item.continuationAttemptId ? { continuationAttemptId: item.continuationAttemptId } : {}),
          updatedAt: item.updatedAt,
        });
      }

      const payload = {
        threadId: threadId ?? null,
        runId: runId ?? null,
        taskCount: tasks.length,
        tasks,
        cohorts,
      };
      return toolJsonResult(payload);
    },
  });

  registerTool({
    name: "vantry_task_cancel",
    label: "Vantry Cancel Task",
    description:
      "Cancel a delegated Vantry child task and its active or queued descendant runs. Always use vantry_task_cancel (never vantry_thread_manage interrupt) to stop a delegated task.",
    promptSnippet: "Cancel a delegated Vantry child task",
    parameters: taskCancelSchema,
    async execute(_toolCallId, params: TaskCancelInput, signal, _onUpdate, ctx) {
      const taskId = params.taskId?.trim();
      if (!taskId) {
        throw new Error("vantry_task_cancel requires a non-empty taskId");
      }
      const reason = params.reason?.trim();
      const clientRequestId = params.clientRequestId?.trim();
      const callerThreadId = readEnv(ENV_THREAD_ID);
      const activeSignal = signal ?? ctx.signal;

      if (resolveSocketPath()) {
        const body: Record<string, unknown> = { taskId };
        if (clientRequestId) body.clientRequestId = clientRequestId;
        if (callerThreadId) body.callerThreadId = callerThreadId;
        if (reason) body.reason = reason;
        const result = await callCoreSocket<Record<string, unknown>>(
          "POST",
          "/v1/orchestration/task-cancel",
          body,
          activeSignal,
        );
        return toolJsonResult(result);
      }

      const args = ["cancel-task", "--task", taskId];
      if (reason) {
        args.push("--reason", reason);
      }
      if (clientRequestId) {
        args.push("--command-id", clientRequestId);
      }
      const receipt = await execVantryCtl(args, activeSignal);
      return toolJsonResult(receipt);
    },
  });

  registerTool({
    name: "vantry_task_followup",
    label: "Vantry Task Follow-up",
    description: "Send follow-up instructions to an existing Vantry delegated child task.",
    promptSnippet: "Send follow-up instructions to an existing delegated Vantry task",
    parameters: taskFollowupSchema,
    async execute(_toolCallId, params: TaskFollowupInput, signal, _onUpdate, ctx) {
      const taskId = params.taskId?.trim();
      const message = params.message?.trim();
      if (!taskId) {
        throw new Error("vantry_task_followup requires a non-empty taskId");
      }
      if (!message) {
        throw new Error("vantry_task_followup requires a non-empty message");
      }
      const activeSignal = signal ?? ctx.signal;

      if (resolveSocketPath()) {
        const receipt = await dispatchCoreCommand(
          {
            kind: "task.followup",
            taskId,
            message: params.message,
          },
          params.clientRequestId,
          activeSignal,
        );
        return toolJsonResult(receipt);
      }

      const args = ["followup", "--task", taskId, "--message", params.message];
      if (params.clientRequestId?.trim()) {
        args.push("--command-id", params.clientRequestId.trim());
      }
      const receipt = await execVantryCtl(args, activeSignal);
      return toolJsonResult(receipt);
    },
  });

  registerTool({
    name: "vantry_task_ack",
    label: "Vantry Task Acknowledge",
    description:
      "Manually acknowledge a Vantry task cohort delivery using its generation and deliveryMessageId.",
    promptSnippet: "Acknowledge manual Vantry child task delivery",
    parameters: taskAckSchema,
    async execute(_toolCallId, params: TaskAckInput, signal, _onUpdate, ctx) {
      const parentRunId = requireEnv(ENV_RUN_ID);
      const deliveryMessageId = params.deliveryMessageId?.trim();
      if (!Number.isInteger(params.generation) || params.generation <= 0) {
        throw new Error("vantry_task_ack requires a positive integer generation");
      }
      if (!deliveryMessageId) {
        throw new Error("vantry_task_ack requires a non-empty deliveryMessageId");
      }
      const activeSignal = signal ?? ctx.signal;

      if (resolveSocketPath()) {
        const receipt = await dispatchCoreCommand(
          {
            kind: "task.acknowledge",
            parentRunId,
            generation: params.generation,
            deliveryMessageId,
          },
          params.clientRequestId,
          activeSignal,
        );
        return toolJsonResult(receipt);
      }

      const args = [
        "ack",
        "--parent-run",
        parentRunId,
        "--generation",
        String(params.generation),
        "--delivery",
        deliveryMessageId,
      ];
      if (params.clientRequestId?.trim()) {
        args.push("--command-id", params.clientRequestId.trim());
      }
      const receipt = await execVantryCtl(args, activeSignal);
      return toolJsonResult(receipt);
    },
  });

  registerTool({
    name: "vantry_thread_list",
    label: "Vantry List Threads",
    description:
      "List Vantry Pi sessions with status, workspace, branch, settled/snoozed state, and markdown thread links.",
    promptSnippet: "List Vantry Pi sessions with status and workspace filters",
    parameters: threadListSchema,
    async execute(_toolCallId, params: ThreadListInput, signal, _onUpdate, ctx) {
      const cursor = validateNonNegativeInt("cursor", params.cursor);
      const limit = validatePositiveInt("limit", params.limit);
      const callerThreadId = readEnv(ENV_THREAD_ID);

      const query: Record<string, unknown> = {};
      if (callerThreadId) query.callerThreadId = callerThreadId;
      if (params.workspace?.trim()) query.workspace = params.workspace.trim();
      if (params.allProjects === true) query.allProjects = true;
      if (params.includeSubagents === true) query.includeSubagents = true;
      if (typeof params.settled === "boolean") query.settled = params.settled;
      if (typeof params.snoozed === "boolean") query.snoozed = params.snoozed;
      if (Array.isArray(params.statuses) && params.statuses.length > 0) {
        const cleaned = params.statuses.map((s) => s.trim()).filter((s) => s.length > 0);
        if (cleaned.length > 0) {
          query.statuses = cleaned;
        }
      }
      if (params.titleContains?.trim()) query.titleContains = params.titleContains.trim();
      if (cursor !== undefined) query.cursor = cursor;
      if (limit !== undefined) query.limit = limit;

      const result = await callCoreSocket<Record<string, unknown>>(
        "POST",
        "/v1/orchestration/threads/list",
        { ...query, ...(params.environmentId?.trim() ? { environmentId: params.environmentId.trim() } : {}) },
        signal ?? ctx.signal,
      );
      return toolJsonResult(result);
    },
  });

  registerTool({
    name: "vantry_thread_search",
    label: "Vantry Search Threads",
    description:
      "Search across Vantry Pi session titles, git branches, workspaces, user prompts, and assistant outputs.",
    promptSnippet: "Search across Vantry Pi sessions by keyword",
    parameters: threadSearchSchema,
    async execute(_toolCallId, params: ThreadSearchInput, signal, _onUpdate, ctx) {
      const queryText = params.query?.trim();
      if (!queryText) {
        throw new Error("vantry_thread_search requires a non-empty query");
      }
      const limit = validatePositiveInt("limit", params.limit);
      const body: Record<string, unknown> = { query: queryText };
      if (params.includeArchived === true) {
        body.includeArchived = true;
      }
      if (limit !== undefined) {
        body.limit = limit;
      }
      const result = await callCoreSocket<unknown>(
        "POST",
        "/v1/orchestration/threads/search",
        body,
        signal ?? ctx.signal,
      );
      return toolJsonResult(result);
    },
  });

  registerTool({
    name: "vantry_thread_read",
    label: "Vantry Read Thread",
    description:
      "Read another Vantry Pi session's messages or activity timeline with paginated afterPosition/beforePosition cursors.",
    promptSnippet: "Read paginated messages or activity from a Vantry Pi session",
    parameters: threadReadSchema,
    async execute(_toolCallId, params: ThreadReadInput, signal, _onUpdate, ctx) {
      const threadId = params.threadId?.trim();
      if (!threadId) {
        throw new Error("vantry_thread_read requires a non-empty threadId");
      }
      const view = validateEnumValue("view", params.view, THREAD_READ_VIEWS);
      const afterPosition = validateNonNegativeInt("afterPosition", params.afterPosition);
      const beforePosition = validateNonNegativeInt("beforePosition", params.beforePosition);
      const limit = validatePositiveInt("limit", params.limit);
      const runLimit = validatePositiveInt("runLimit", params.runLimit);
      const maxCharsPerItem = validatePositiveInt("maxCharsPerItem", params.maxCharsPerItem);
      const textOffset = validateNonNegativeInt("textOffset", params.textOffset);
      const callerThreadId = readEnv(ENV_THREAD_ID);

      const query: Record<string, unknown> = { threadId };
      if (callerThreadId) query.callerThreadId = callerThreadId;
      if (view) query.view = view;
      if (afterPosition !== undefined) query.afterPosition = afterPosition;
      if (beforePosition !== undefined) query.beforePosition = beforePosition;
      if (limit !== undefined) query.limit = limit;
      if (runLimit !== undefined) query.runLimit = runLimit;
      if (maxCharsPerItem !== undefined) query.maxCharsPerItem = maxCharsPerItem;
      if (params.itemId?.trim()) query.itemId = params.itemId.trim();
      if (textOffset !== undefined) query.textOffset = textOffset;

      const result = await callCoreSocket<Record<string, unknown>>(
        "POST",
        "/v1/orchestration/threads/read",
        { ...query, ...(params.environmentId?.trim() ? { environmentId: params.environmentId.trim() } : {}) },
        signal ?? ctx.signal,
      );
      return toolJsonResult(result);
    },
  });

  registerTool({
    name: "vantry_thread_send",
    label: "Vantry Send To Thread",
    description:
      "Send a message, question, reply, or steering instruction directly to another Vantry Pi session for Pi-to-Pi inter-session conversation.",
    promptSnippet: "Send a message or steer instruction directly to another Vantry Pi session",
    parameters: threadSendSchema,
    async execute(_toolCallId, params: ThreadSendInput, signal, _onUpdate, ctx) {
      const targetThreadId = params.threadId?.trim();
      if (!targetThreadId) {
        throw new Error("vantry_thread_send requires a non-empty threadId");
      }
      if (!params.message || params.message.trim().length === 0) {
        throw new Error("vantry_thread_send requires a non-empty message");
      }
      const mode = validateEnumValue("mode", params.mode, THREAD_SEND_MODES);
      const callerThreadId = readEnv(ENV_THREAD_ID);
      const clientRequestId = params.clientRequestId?.trim();

      const body: Record<string, unknown> = {
        targetThreadId,
        message: params.message,
      };
      if (clientRequestId) body.clientRequestId = clientRequestId;
      if (callerThreadId) body.callerThreadId = callerThreadId;
      if (mode) body.mode = mode;
      if (params.notifyOnComplete === true) body.notifyOnComplete = true;

      const result = await callCoreSocket<Record<string, unknown>>(
        "POST",
        "/v1/orchestration/threads/send",
        { ...body, ...(params.environmentId?.trim() ? { environmentId: params.environmentId.trim() } : {}) },
        signal ?? ctx.signal,
      );
      return toolJsonResult(result);
    },
  });

  registerTool({
    name: "vantry_thread_wait",
    label: "Vantry Wait Thread",
    description:
      "Event-driven wait for a peer Vantry Pi session's active or queued run to settle (or pause for a question/approval) when this turn needs its output.",
    promptSnippet: "Wait for another Vantry Pi session's run to settle",
    parameters: threadWaitSchema,
    async execute(_toolCallId, params: ThreadWaitInput, signal, _onUpdate, ctx) {
      const threadId = params.threadId?.trim();
      if (!threadId) {
        throw new Error("vantry_thread_wait requires a non-empty threadId");
      }
      const timeoutMs = validatePositiveInt("timeoutMs", params.timeoutMs);
      const callerThreadId = readEnv(ENV_THREAD_ID);

      const query: Record<string, unknown> = { threadId };
      if (callerThreadId) query.callerThreadId = callerThreadId;
      if (params.runId?.trim()) query.runId = params.runId.trim();
      if (timeoutMs !== undefined) query.timeoutMs = timeoutMs;
      if (params.untilDelegatedWorkSettled === true) {
        query.untilDelegatedWorkSettled = true;
      }

      const result = await callCoreSocket<Record<string, unknown>>(
        "POST",
        "/v1/orchestration/threads/wait",
        { ...query, ...(params.environmentId?.trim() ? { environmentId: params.environmentId.trim() } : {}) },
        signal ?? ctx.signal,
      );
      return toolJsonResult(result);
    },
  });

  registerTool({
    name: "vantry_thread_launch",
    label: "Vantry Launch Thread",
    description:
      "Launch a new independent top-level Vantry Pi session (optionally in a git worktree or scratch workspace) and optionally start its first prompt.",
    promptSnippet: "Launch a new top-level Vantry Pi session",
    parameters: threadLaunchSchema,
    async execute(_toolCallId, params: ThreadLaunchInput, signal, _onUpdate, ctx) {
      const spec = serializeThreadLaunchSpec(params, ctx?.cwd);
      const callerThreadId = readEnv(ENV_THREAD_ID);
      const callerRunId = readEnv(ENV_RUN_ID);
      const clientRequestId = params.clientRequestId?.trim();

      const body: Record<string, unknown> = { ...spec };
      if (clientRequestId) body.clientRequestId = clientRequestId;
      if (callerThreadId) body.callerThreadId = callerThreadId;
      if (callerRunId) body.callerRunId = callerRunId;
      if (params.notifyOnComplete === true) body.notifyOnComplete = true;

      const launched = await callCoreSocket<Record<string, unknown>>(
        "POST",
        "/v1/orchestration/threads/launch",
        body,
        signal ?? ctx.signal,
      );
      return toolJsonResult(launched);
    },
  });

  registerTool({
    name: "vantry_create_threads",
    label: "Vantry Create Threads Batch",
    description:
      "Create and optionally start up to 20 independent peer Vantry Pi sessions in a single batch call.",
    promptSnippet: "Launch up to 20 peer Vantry Pi sessions in parallel",
    parameters: createThreadsSchema,
    async execute(_toolCallId, params: CreateThreadsInput, signal, _onUpdate, ctx) {
      if (!Array.isArray(params.threads) || params.threads.length === 0) {
        throw new Error("vantry_create_threads requires at least 1 thread specification");
      }
      if (params.threads.length > MAX_BATCH_THREADS) {
        throw new Error(`vantry_create_threads supports at most ${MAX_BATCH_THREADS} threads per call`);
      }

      const specs = params.threads.map((item) => serializeThreadLaunchSpec(item, ctx?.cwd));
      const callerThreadId = readEnv(ENV_THREAD_ID);
      const callerRunId = readEnv(ENV_RUN_ID);
      const clientRequestId = params.clientRequestId?.trim();

      const body: Record<string, unknown> = { threads: specs };
      if (clientRequestId) body.clientRequestId = clientRequestId;
      if (callerThreadId) body.callerThreadId = callerThreadId;
      if (callerRunId) body.callerRunId = callerRunId;

      const result = await callCoreSocket<Record<string, unknown>>(
        "POST",
        "/v1/orchestration/threads/create-batch",
        body,
        signal ?? ctx.signal,
      );
      return toolJsonResult(result);
    },
  });

  registerTool({
    name: "vantry_thread_transfer",
    label: "Vantry Thread Transfer",
    description:
      "Branch a new Pi session from settled history ('fork') or merge a session's latest completed output back into its origin/target thread ('merge_back').",
    promptSnippet: "Fork a Vantry Pi session or merge its completed result back",
    parameters: threadTransferSchema,
    async execute(_toolCallId, params: ThreadTransferInput, signal, _onUpdate, ctx) {
      const action = validateEnumValue("action", params.action, THREAD_TRANSFER_ACTIONS);
      if (!action) {
        throw new Error("vantry_thread_transfer requires action ('fork' or 'merge_back')");
      }
      const sourceThreadId = params.threadId?.trim() || readEnv(ENV_THREAD_ID);
      if (!sourceThreadId) {
        throw new Error(`vantry_thread_transfer ${action} requires threadId`);
      }
      const activeSignal = signal ?? ctx.signal;

      if (action === "fork") {
        const command: Record<string, unknown> = {
          kind: "thread.fork",
          threadId: sourceThreadId,
        };
        const sourceRunId = params.sourceRunId?.trim();
        if (sourceRunId) {
          command.sourceRunId = sourceRunId;
        }
        const title = params.title?.trim();
        if (title) {
          command.title = title;
        }
        const model = params.model?.trim();
        if (model) {
          command.model = model;
        }
        if (params.workspaceKind === "worktree") {
          command.workspace = { kind: "worktree" };
        }
        const receipt = await dispatchCoreCommand(command, params.clientRequestId, activeSignal);
        return toolJsonResult(receipt);
      }

      const command: Record<string, unknown> = {
        kind: "thread.merge_back",
        threadId: sourceThreadId,
      };
      const targetThreadId = params.targetThreadId?.trim();
      if (targetThreadId) {
        command.targetThreadId = targetThreadId;
      }
      const receipt = await dispatchCoreCommand(command, params.clientRequestId, activeSignal);
      return toolJsonResult(receipt);
    },
  });

  registerTool({
    name: "vantry_thread_manage",
    label: "Vantry Manage Thread",
    description:
      "Manage a non-delegated Vantry Pi session (rename, pin, unpin, settle, unsettle, snooze, unsnooze, set_model, interrupt, respond_request). Never use interrupt on a delegated child task; use vantry_task_cancel instead.",
    promptSnippet: "Manage thread metadata, model, snooze/settle state, or dialog responses",
    parameters: threadManageSchema,
    async execute(_toolCallId, params: ThreadManageInput, signal, _onUpdate, ctx) {
      const action = validateEnumValue("action", params.action, THREAD_MANAGE_ACTIONS);
      if (!action) {
        throw new Error("vantry_thread_manage requires a valid action");
      }
      const activeSignal = signal ?? ctx.signal;
      const explicitThreadId = params.threadId?.trim();
      const defaultThreadId = explicitThreadId || readEnv(ENV_THREAD_ID);

      switch (action) {
        case "rename": {
          if (!defaultThreadId) {
            throw new Error("vantry_thread_manage rename requires threadId");
          }
          const title = params.title?.trim();
          if (!title) {
            throw new Error("vantry_thread_manage rename requires a non-empty title");
          }
          const receipt = await dispatchCoreCommand(
            { kind: "thread.update", threadId: defaultThreadId, title },
            params.clientRequestId,
            activeSignal,
          );
          return toolJsonResult(receipt);
        }
        case "pin":
        case "unpin": {
          if (!defaultThreadId) {
            throw new Error(`vantry_thread_manage ${action} requires threadId`);
          }
          const receipt = await dispatchCoreCommand(
            { kind: "thread.update", threadId: defaultThreadId, pinned: action === "pin" },
            params.clientRequestId,
            activeSignal,
          );
          return toolJsonResult(receipt);
        }
        case "settle":
        case "unsettle": {
          if (!defaultThreadId) {
            throw new Error(`vantry_thread_manage ${action} requires threadId`);
          }
          const receipt = await dispatchCoreCommand(
            { kind: "thread.update", threadId: defaultThreadId, archived: action === "settle" },
            params.clientRequestId,
            activeSignal,
          );
          return toolJsonResult(receipt);
        }
        case "snooze": {
          if (!defaultThreadId) {
            throw new Error("vantry_thread_manage snooze requires threadId");
          }
          const rawUntil = params.snoozedUntil?.trim();
          if (!rawUntil) {
            throw new Error("vantry_thread_manage snooze requires snoozedUntil (RFC3339 timestamp)");
          }
          const parsedMs = Date.parse(rawUntil);
          if (Number.isNaN(parsedMs)) {
            throw new Error(`Invalid snoozedUntil timestamp: ${JSON.stringify(rawUntil)}`);
          }
          const receipt = await dispatchCoreCommand(
            {
              kind: "thread.update",
              threadId: defaultThreadId,
              snoozedUntil: new Date(parsedMs).toISOString(),
            },
            params.clientRequestId,
            activeSignal,
          );
          return toolJsonResult(receipt);
        }
        case "unsnooze": {
          if (!defaultThreadId) {
            throw new Error("vantry_thread_manage unsnooze requires threadId");
          }
          const receipt = await dispatchCoreCommand(
            {
              kind: "thread.update",
              threadId: defaultThreadId,
              snoozedUntil: "0001-01-01T00:00:00Z",
            },
            params.clientRequestId,
            activeSignal,
          );
          return toolJsonResult(receipt);
        }
        case "set_model": {
          if (!defaultThreadId) {
            throw new Error("vantry_thread_manage set_model requires threadId");
          }
          const model = params.model?.trim();
          if (!model) {
            throw new Error("vantry_thread_manage set_model requires a non-empty model");
          }
          const receipt = await dispatchCoreCommand(
            { kind: "thread.model", threadId: defaultThreadId, model },
            params.clientRequestId,
            activeSignal,
          );
          return toolJsonResult(receipt);
        }
        case "interrupt": {
          let runId = params.runId?.trim();
          const targetThreadId = explicitThreadId;
          if (!runId && !targetThreadId) {
            throw new Error("vantry_thread_manage interrupt requires runId or threadId");
          }
          if (!runId && targetThreadId) {
            const readRes = await callCoreSocket<Record<string, unknown>>(
              "POST",
              "/v1/orchestration/threads/read",
              { threadId: targetThreadId, runLimit: 10, limit: 1 },
              activeSignal,
            );
            const recentRuns = Array.isArray(readRes.recentRuns) ? readRes.recentRuns : [];
            const activeRun = recentRuns.find(
              (r) =>
                isRecord(r) &&
                typeof r.runId === "string" &&
                (r.status === "running" || r.status === "queued" || r.status === "waiting_children"),
            ) as Record<string, unknown> | undefined;
            if (activeRun && typeof activeRun.runId === "string") {
              runId = activeRun.runId;
            } else {
              throw new Error(`No active run found to interrupt on thread ${targetThreadId}`);
            }
          }
          const command: Record<string, unknown> = {
            kind: "run.stop",
            runId,
          };
          if (targetThreadId) {
            command.threadId = targetThreadId;
          }
          const receipt = await dispatchCoreCommand(command, params.clientRequestId, activeSignal);
          return toolJsonResult(receipt);
        }
        case "respond_request": {
          const requestId = params.requestId?.trim();
          if (!requestId) {
            throw new Error("vantry_thread_manage respond_request requires a non-empty requestId");
          }
          const hasCancel = params.cancelled === true;
          const hasConfirm = typeof params.confirmed === "boolean";
          const hasValue = params.value !== undefined;
          const count = (hasCancel ? 1 : 0) + (hasConfirm ? 1 : 0) + (hasValue ? 1 : 0);
          if (count !== 1) {
            throw new Error(
              "vantry_thread_manage respond_request requires exactly one of confirmed, value, or cancelled=true",
            );
          }
          const command: Record<string, unknown> = {
            kind: "request.respond",
            requestId,
          };
          if (explicitThreadId) {
            command.threadId = explicitThreadId;
          }
          if (params.runId?.trim()) {
            command.runId = params.runId.trim();
          }
          if (hasCancel) {
            command.cancelled = true;
          } else if (hasConfirm) {
            command.confirmed = params.confirmed;
          } else {
            command.value = params.value;
          }
          const receipt = await dispatchCoreCommand(command, params.clientRequestId, activeSignal);
          return toolJsonResult(receipt);
        }
      }
    },
  });

  registerTool({
    name: "vantry_request_secret",
    label: "Vantry Request Secret",
    description:
      "Collect a sensitive secret (API key, token, password, or sudo password) through Vantry's native input dialog and store it in a 0600 one-time file without exposing plaintext in chat, logs, or session history.",
    promptSnippet: "Request a secret via Vantry's native dialog and write it to a 0600 file",
    promptGuidelines: [
      "Use vantry_request_secret for user secrets (0600 file); never ask for plaintext secrets or print secret-file contents.",
    ],
    // Strip Pi codemode's "Codemode: tools.<name>(args)..." description suffix from
    // routine retrieval tools so the model invokes read/grep/find/ls directly in parallel,
    // while keeping them callable inside codemode when scripts genuinely need them.
    prepareLoadout(loadout) {
      const descriptions: Record<string, string> = {};
      for (const declared of loadout.declared) {
        if (DIRECT_RETRIEVAL_TOOLS.has(declared.name)) {
          descriptions[declared.name] = declared.description;
        }
      }
      return Object.keys(descriptions).length > 0 ? { descriptions } : undefined;
    },
    parameters: requestSecretSchema,
    async execute(_toolCallId, params: RequestSecretInput, signal, _onUpdate, ctx) {
      const prompt = params.prompt?.trim();
      if (!prompt) {
        throw new Error("vantry_request_secret requires a non-empty prompt");
      }
      const envVarName = params.envVarName?.trim();
      if (envVarName !== undefined && envVarName.length > 0 && !ENV_VAR_NAME_PATTERN.test(envVarName)) {
        throw new Error(`Invalid envVarName ${JSON.stringify(envVarName)}; expected [A-Za-z_][A-Za-z0-9_]*`);
      }
      if (!ctx.hasUI) {
        throw new Error("Vantry secret input dialog UI is unavailable");
      }

      const jobDir = resolveJobDir();
      const activeSignal = signal ?? ctx.signal;
      const secretValue = activeSignal
        ? await ctx.ui.input(prompt, "", { signal: activeSignal })
        : await ctx.ui.input(prompt, "");

      if (secretValue === undefined || secretValue.length === 0) {
        throw new Error("Secret input was cancelled or empty");
      }

      const secretFilePath = await writeOneTimeSecretFile(jobDir, secretValue);
      const quotedPath = shellSingleQuote(secretFilePath);
      const usageHint =
        envVarName && envVarName.length > 0
          ? `Pass via environment without printing: ${envVarName}="$(cat ${quotedPath})" <command>, then remove with rm -f ${quotedPath}. Never echo or log the secret.`
          : `Consume from file without printing (e.g. sudo -S -p '' <command> < ${quotedPath} or SECRET="$(cat ${quotedPath})" <command>), then remove with rm -f ${quotedPath}. Never echo or log the secret.`;

      const result = { secretFilePath, usageHint };
      return toolJsonResult(result);
    },
  });

  registerTool({
    name: "vantry_terminal_run",
    label: "Vantry Interactive Terminal",
    description:
      "Run an interactive shell command inside Vantry's built-in Terminal (real PTY in the workspace directory) so the user can complete login, 2FA/OTP, SSH prompts, sudo, or TTY wizards directly beside the chat. Resumes automatically when the command exits without exposing terminal secrets to chat or logs.",
    promptSnippet: "Run an interactive TTY command in Vantry's built-in Terminal and resume on exit",
    promptGuidelines: [
      "Use vantry_terminal_run for interactive TTY commands (login, 2FA, SSH, setup wizards) instead of osascript, .command wrappers, or /usr/bin/script.",
    ],
    parameters: terminalRunSchema,
    async execute(_toolCallId, params: TerminalRunInput, signal, _onUpdate, ctx) {
      const command = params.command?.trim();
      if (!command) {
        throw new Error("vantry_terminal_run requires a non-empty command");
      }
      if (!ctx.hasUI) {
        throw new Error("Vantry interactive terminal UI is unavailable");
      }
      const baseCwd = ctx.cwd ? path.resolve(ctx.cwd) : process.cwd();
      const rawCwd = params.cwd?.trim();
      const resolvedCwd = rawCwd ? (path.isAbsolute(rawCwd) ? path.resolve(rawCwd) : path.resolve(baseCwd, rawCwd)) : baseCwd;
      const label = params.title?.trim() || command.split(/\r?\n/, 1)[0].slice(0, 60);
      const payload = JSON.stringify({
        kind: VANTRY_TERMINAL_REQUEST_KIND,
        version: 1,
        title: label,
        command,
        cwd: resolvedCwd,
      });
      const activeSignal = signal ?? ctx.signal;
      const rawResponse = activeSignal
        ? await ctx.ui.editor(`交互终端 · ${label}`, payload, { signal: activeSignal })
        : await ctx.ui.editor(`交互终端 · ${label}`, payload);

      if (rawResponse === undefined) {
        return toolJsonResult({
          status: "cancelled",
          title: label,
          command,
          cwd: resolvedCwd,
        });
      }
      const trimmed = rawResponse.trim();
      if (trimmed.startsWith("{")) {
        try {
          const parsed = JSON.parse(trimmed) as unknown;
          if (isRecord(parsed)) {
            return toolJsonResult({
              status: typeof parsed.status === "string" ? parsed.status : "exited",
              ...(typeof parsed.exitCode === "number" ? { exitCode: parsed.exitCode } : {}),
              title: label,
              cwd: typeof parsed.cwd === "string" && parsed.cwd ? parsed.cwd : resolvedCwd,
              ...(typeof parsed.note === "string" && parsed.note ? { note: parsed.note } : {}),
            });
          }
        } catch {
          // Fall back to plain-text completion result below.
        }
      }
      return toolJsonResult({
        status: "completed",
        title: label,
        cwd: resolvedCwd,
        ...(trimmed ? { note: trimmed } : {}),
      });
    },
  });
}
