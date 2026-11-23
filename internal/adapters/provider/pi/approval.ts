import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import * as fs from "node:fs";
import * as path from "node:path";

const FILE_EDIT_TOOLS = new Set(["write", "edit"]);
const SHELL_TOOLS = new Set(["bash", "powershell", "vantry_terminal_run"]);

type NormalizedApprovalPolicy = "auto" | "auto-accept-edits" | "ask";

function currentApprovalPolicy(): NormalizedApprovalPolicy {
  const raw = (process.env.VANTRY_APPROVAL_POLICY ?? "").trim().toLowerCase();
  switch (raw) {
    case "auto":
    case "full-access":
      return "auto";
    case "auto-accept-edits":
      return "auto-accept-edits";
    case "":
    case "ask":
    case "approval-required":
    default:
      // Fail closed when loaded without an explicit auto policy.
      return "ask";
  }
}

function isPlanMode(): boolean {
  return (process.env.VANTRY_INTERACTION_MODE ?? "").trim().toLowerCase() === "plan";
}

function extractShellCommand(input: unknown): string {
  if (typeof input !== "object" || input === null) {
    return "";
  }
  const command = (input as { command?: unknown }).command;
  return typeof command === "string" ? command : "";
}

const DESTRUCTIVE_PATTERNS: readonly RegExp[] = [
  /(?:^|[;&|()\s`])(?:sudo|su|rm|rmdir|mv|cp|ln|mkdir|touch|chmod|chown|chgrp|truncate|dd|shred|unlink|tee|patch|mkfs|fdisk|diskutil|kill|killall|pkill|reboot|shutdown|launchctl|systemctl)\b/,
  /\bsed\s+(?:-[A-Za-z]*i|--in-place)\b/,
  /\bperl\s+-[A-Za-z]*i\b/,
  /\bgit\s+(?:commit|push|reset|checkout|switch|clean|merge|rebase|cherry-pick|revert|stash|apply|am|rm|mv|tag\s+-d|branch\s+-[dD])\b/,
  /\b(?:npm|pnpm|yarn|bun)\s+(?:install|i|add|remove|uninstall|ci|publish|update|link)\b/,
  /\bpip3?\s+(?:install|uninstall)\b/,
  /\bgo\s+(?:install|get|mod\s+tidy)\b/,
  /\bcargo\s+(?:install|add|remove)\b/,
  /\bbrew\s+(?:install|uninstall|upgrade|reinstall)\b/,
];

function stripBenignNullRedirects(command: string): string {
  return command
    .replace(/(?:[0-9]*>&[0-9]+|&?>\s*\/dev\/null|[0-9]+>\s*\/dev\/null)/g, " ")
    .replace(/[-=]&?>/g, " ");
}

function isDestructiveShellCommand(command: string): boolean {
  const trimmed = command.trim();
  if (trimmed.length === 0) {
    return false;
  }
  const sanitized = stripBenignNullRedirects(trimmed);
  if (/(?:^|[^0-9<>])>>?\s*\S/.test(sanitized)) {
    return true;
  }
  return DESTRUCTIVE_PATTERNS.some((pattern) => pattern.test(sanitized));
}

const SIMPLE_READONLY_COMMANDS = new Set([
  "ls",
  "ll",
  "pwd",
  "cat",
  "head",
  "tail",
  "wc",
  "file",
  "stat",
  "which",
  "type",
  "uname",
  "date",
  "dirname",
  "basename",
  "realpath",
  "readlink",
  "tree",
  "rg",
  "grep",
  "egrep",
  "fgrep",
  "ag",
  "fd",
  "cut",
  "uniq",
  "tr",
  "printf",
  "echo",
  "true",
  "false",
  "test",
  "[",
  "printenv",
  "whoami",
  "id",
  "df",
  "du",
  "ps",
  "lsof",
]);

const GIT_READONLY_SUBCOMMANDS = new Set([
  "status",
  "diff",
  "log",
  "show",
  "rev-parse",
  "ls-files",
  "ls-tree",
  "cat-file",
  "describe",
  "shortlog",
  "blame",
  "grep",
  "version",
  "help",
]);

function splitShellSegments(command: string): string[] | null {
  const trimmed = command.trim();
  if (!trimmed || /[\r\n]/.test(trimmed)) {
    return null;
  }
  const segments: string[] = [];
  let cur = "";
  let inSingle = false;
  let inDouble = false;

  const isNullRedirectAt = (idx: number): number => {
    const rest = trimmed.slice(idx);
    const m = rest.match(/^(?:[0-9]+>&[0-9]+|&>\s*\/dev\/null|[0-9]*>\s*\/dev\/null)(?=$|[\s;|&])/);
    return m ? m[0].length : 0;
  };

  for (let i = 0; i < trimmed.length; i++) {
    const ch = trimmed[i];
    if (inSingle) {
      cur += ch;
      if (ch === "'") {
        inSingle = false;
      }
      continue;
    }
    if (inDouble) {
      if (ch === "\\" && i + 1 < trimmed.length) {
        cur += ch + trimmed[i + 1];
        i++;
        continue;
      }
      if (ch === "`") {
        return null;
      }
      if (ch === "$" && (trimmed[i + 1] === "(" || trimmed[i + 1] === "{")) {
        return null;
      }
      cur += ch;
      if (ch === '"') {
        inDouble = false;
      }
      continue;
    }

    if (ch === "\\" && i + 1 < trimmed.length) {
      cur += ch + trimmed[i + 1];
      i++;
      continue;
    }
    if (ch === "'") {
      inSingle = true;
      cur += ch;
      continue;
    }
    if (ch === '"') {
      inDouble = true;
      cur += ch;
      continue;
    }

    if (ch === "`" || ch === "(" || ch === ")" || ch === "{" || ch === "}") {
      return null;
    }
    if (ch === "$" && (trimmed[i + 1] === "(" || trimmed[i + 1] === "{")) {
      return null;
    }

    const redirLen = isNullRedirectAt(i);
    if (redirLen > 0 && (cur === "" || /\s$/.test(cur))) {
      i += redirLen - 1;
      cur += " ";
      continue;
    }
    if (ch === ">" || ch === "<") {
      return null;
    }

    if (ch === ";") {
      if (cur.trim().length === 0) {
        return null;
      }
      segments.push(cur.trim());
      cur = "";
      continue;
    }
    if (ch === "|" || ch === "&") {
      const next = trimmed[i + 1];
      if (ch === "&" && next !== "&") {
        return null;
      }
      if (cur.trim().length === 0) {
        return null;
      }
      segments.push(cur.trim());
      cur = "";
      if (next === ch) {
        i++;
      }
      continue;
    }
    cur += ch;
  }

  if (inSingle || inDouble || cur.trim().length === 0) {
    return null;
  }
  segments.push(cur.trim());
  return segments;
}

function splitShellWords(segment: string): string[] | null {
  const words: string[] = [];
  let cur = "";
  let hasToken = false;
  let inSingle = false;
  let inDouble = false;

  for (let i = 0; i < segment.length; i++) {
    const ch = segment[i];
    if (inSingle) {
      if (ch === "'") {
        inSingle = false;
      } else {
        cur += ch;
      }
      hasToken = true;
      continue;
    }
    if (inDouble) {
      if (ch === "\\" && i + 1 < segment.length) {
        cur += segment[i + 1];
        i++;
      } else if (ch === '"') {
        inDouble = false;
      } else {
        cur += ch;
      }
      hasToken = true;
      continue;
    }
    if (/\s/.test(ch)) {
      if (hasToken) {
        words.push(cur);
        cur = "";
        hasToken = false;
      }
      continue;
    }
    if (ch === "'" || ch === '"') {
      inSingle = ch === "'";
      inDouble = ch === '"';
      hasToken = true;
      continue;
    }
    if (ch === "\\" && i + 1 < segment.length) {
      cur += segment[i + 1];
      i++;
      hasToken = true;
      continue;
    }
    cur += ch;
    hasToken = true;
  }
  if (inSingle || inDouble) {
    return null;
  }
  if (hasToken) {
    words.push(cur);
  }
  return words.length > 0 ? words : null;
}

function stripEnvPrefixes(words: string[]): string[] {
  let idx = 0;
  while (idx < words.length && /^[A-Za-z_][A-Za-z0-9_]*=/.test(words[idx])) {
    idx++;
  }
  return words.slice(idx);
}

function isReadOnlyGitArgs(args: string[]): boolean {
  let i = 0;
  while (i < args.length && args[i].startsWith("-")) {
    const opt = args[i];
    if (opt === "--no-pager" || opt === "--bare" || opt === "--no-replace-objects" || opt === "--literal-pathspecs") {
      i++;
      continue;
    }
    if (opt === "-C" || opt === "--git-dir" || opt === "--work-tree") {
      i += 2;
      continue;
    }
    if (opt.startsWith("--git-dir=") || opt.startsWith("--work-tree=")) {
      i++;
      continue;
    }
    // Reject -c / --exec-path / unknown global flags fail-closed.
    return false;
  }
  if (i >= args.length) {
    return false;
  }
  const sub = args[i];
  const rest = args.slice(i + 1);
  if (GIT_READONLY_SUBCOMMANDS.has(sub)) {
    return true;
  }
  switch (sub) {
    case "reflog":
      return !rest.some((a) => a === "expire" || a === "delete");
    case "remote":
      return (
        rest.length === 0 ||
        rest.every((a) => a === "-v" || a === "--verbose") ||
        rest[0] === "show" ||
        rest[0] === "get-url"
      );
    case "branch": {
      const mutating = new Set([
        "-d",
        "-D",
        "-m",
        "-M",
        "-c",
        "-C",
        "-f",
        "--delete",
        "--move",
        "--copy",
        "--force",
        "--set-upstream-to",
        "--unset-upstream",
        "--edit-description",
      ]);
      if (rest.some((a) => mutating.has(a) || a.startsWith("--set-upstream-to=") || a.startsWith("--delete"))) {
        return false;
      }
      if (rest.length === 0) {
        return true;
      }
      return rest.some((a) =>
        [
          "--show-current",
          "-a",
          "--all",
          "-r",
          "--remotes",
          "-v",
          "-vv",
          "--verbose",
          "-l",
          "--list",
          "--contains",
          "--no-contains",
          "--merged",
          "--no-merged",
        ].includes(a),
      );
    }
    case "tag":
      if (rest.length === 0) {
        return true;
      }
      return rest.every((a) => a === "-l" || a === "--list" || a === "-n" || a === "--sort" || a.startsWith("--sort=")) ||
        (rest.includes("-l") || rest.includes("--list"));
    case "stash":
      return rest[0] === "list" || rest[0] === "show";
    case "config":
      return (
        rest.some((a) => a === "--get" || a === "--get-all" || a === "--get-regexp" || a === "--list" || a === "-l") &&
        !rest.some((a) => a === "--unset" || a === "--replace-all" || a === "--add" || a === "--edit" || a === "-e")
      );
    default:
      return false;
  }
}

function isReadOnlySegment(segment: string): boolean {
  const words = splitShellWords(segment);
  if (!words) {
    return false;
  }
  const cmdWords = stripEnvPrefixes(words);
  if (cmdWords.length === 0) {
    return false;
  }
  const base = path.basename(cmdWords[0]);
  const args = cmdWords.slice(1);
  if (SIMPLE_READONLY_COMMANDS.has(base)) {
    return true;
  }
  switch (base) {
    case "sort":
      return !args.some((a) => a === "-o" || a.startsWith("--output=") || a === "--output");
    case "env":
      return args.every((a) => a === "-0" || a === "--null");
    case "awk":
      return !args.some((a) => /\bsystem\s*\(|\bgetline\b|\|\s*["']|>\s*["']/.test(a));
    case "find":
      return !args.some((a) => /^-(?:exec|execdir|ok|okdir|delete|fprint|fprint0|fprintf|fls)$/i.test(a));
    case "command":
      return args.includes("-v") || args.includes("-V");
    case "git":
      return isReadOnlyGitArgs(args);
    default:
      return false;
  }
}

function isReadOnlyShellCommand(command: string): boolean {
  if (isDestructiveShellCommand(command)) {
    return false;
  }
  const segments = splitShellSegments(command);
  if (!segments || segments.length === 0) {
    return false;
  }
  return segments.every(isReadOnlySegment);
}

function extractScopeKey(toolName: string, input: unknown): string | null {
  if (FILE_EDIT_TOOLS.has(toolName)) {
    return toolName;
  }
  if (!SHELL_TOOLS.has(toolName)) {
    return null;
  }
  const command = extractShellCommand(input);
  if (isDestructiveShellCommand(command)) {
    return null;
  }
  const segments = splitShellSegments(command);
  if (!segments || segments.length === 0) {
    return null;
  }
  let targetExe: string | null = null;
  let roExe: string | null = null;
  let mixedRO = false;
  for (const segment of segments) {
    const words = splitShellWords(segment);
    if (!words) {
      return null;
    }
    const cmdWords = stripEnvPrefixes(words);
    if (cmdWords.length === 0) {
      return null;
    }
    const base = path.basename(cmdWords[0]);
    if (!/^[A-Za-z0-9._+-]+$/.test(base)) {
      return null;
    }
    if (isReadOnlySegment(segment)) {
      if (roExe === null) {
        roExe = base;
      } else if (roExe !== base) {
        mixedRO = true;
      }
      continue;
    }
    if (targetExe !== null && targetExe !== base) {
      return null;
    }
    targetExe = base;
  }
  const chosen = targetExe ?? (!mixedRO ? roExe : null);
  if (!chosen) {
    return null;
  }
  if (toolName === "vantry_terminal_run") {
    return `terminal:${chosen}`;
  }
  return `bash:${chosen}`;
}

function grantsFilePath(): string | null {
  const dataDir = (process.env.VANTRY_DATA_DIR ?? "").trim();
  const threadId = (process.env.VANTRY_THREAD_ID ?? "").trim();
  if (!dataDir || !threadId) {
    return null;
  }
  if (!/^[A-Za-z0-9._-]+$/.test(threadId) || threadId.includes("..") || threadId.endsWith(".lock")) {
    return null;
  }
  return path.join(dataDir, "sessions", `${threadId}.grants.json`);
}

function summarizeInput(input: unknown, maxLen = 4_000): string {
  try {
    return JSON.stringify(input).slice(0, maxLen);
  } catch {
    return String(input).slice(0, maxLen);
  }
}

// Loaded explicitly by Vantry to enforce approval policy and Plan-mode safety.
export default function (pi: ExtensionAPI) {
  const memoryGrants = new Set<string>();

  const hasGrant = (key: string): boolean => {
    const file = grantsFilePath();
    if (!file) {
      return memoryGrants.has(key);
    }
    try {
      const raw = fs.readFileSync(file, "utf8");
      const parsed = JSON.parse(raw) as { grants?: unknown };
      if (Array.isArray(parsed.grants)) {
        return parsed.grants.includes(key);
      }
    } catch {
      // Missing or unreadable grants file fails closed.
    }
    return false;
  };

  const recordGrant = (key: string): void => {
    const file = grantsFilePath();
    if (!file) {
      memoryGrants.add(key);
      return;
    }
    const next = new Set<string>();
    try {
      const raw = fs.readFileSync(file, "utf8");
      const parsed = JSON.parse(raw) as { grants?: unknown };
      if (Array.isArray(parsed.grants)) {
        for (const item of parsed.grants) {
          if (typeof item === "string" && item) {
            next.add(item);
          }
        }
      }
    } catch {
      // New grants file.
    }
    next.add(key);
    try {
      const dir = path.dirname(file);
      fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
      const tmp = path.join(dir, `.tmp-grants-${process.pid}-${Date.now()}.json`);
      fs.writeFileSync(tmp, JSON.stringify({ grants: [...next] }) + "\n", { mode: 0o600 });
      fs.renameSync(tmp, file);
    } catch {
      memoryGrants.add(key);
    }
  };

  pi.on("session_start", async (_event, ctx) => {
    ctx.ui.notify("vantry_approval_ready", "info");
  });

  pi.on("tool_call", async (event, ctx) => {
    // Native Vantry orchestration and secret tools never pass through bash approval,
    // but vantry_terminal_run executes shell commands and shares shell gating.
    if (event.toolName.startsWith("vantry_") && !SHELL_TOOLS.has(event.toolName)) {
      return;
    }

    const planMode = isPlanMode();
    if (planMode && FILE_EDIT_TOOLS.has(event.toolName)) {
      return {
        block: true,
        reason: `当前处于 Plan 规划模式，禁止执行 ${event.toolName} 修改文件；请先输出方案。`,
      };
    }

    const policy = currentApprovalPolicy();
    let requiresConfirm = false;
    let promptTitle = `允许执行 ${event.toolName}？`;

    if (SHELL_TOOLS.has(event.toolName)) {
      const command = extractShellCommand(event.input);
      if (event.toolName === "vantry_terminal_run" && planMode) {
        requiresConfirm = true;
        promptTitle = "当前处于 Plan 规划模式，允许在交互终端执行命令？";
      } else if (policy === "ask") {
        requiresConfirm = true;
      } else if (policy === "auto-accept-edits") {
        if (event.toolName === "vantry_terminal_run" || !isReadOnlyShellCommand(command)) {
          requiresConfirm = true;
          if (planMode && isDestructiveShellCommand(command)) {
            promptTitle = `当前处于 Plan 规划模式，允许执行可能修改环境的 ${event.toolName} 命令？`;
          }
        }
      } else if (planMode && isDestructiveShellCommand(command)) {
        requiresConfirm = true;
        promptTitle = `当前处于 Plan 规划模式，允许执行可能修改环境的 ${event.toolName} 命令？`;
      }
    } else if (FILE_EDIT_TOOLS.has(event.toolName)) {
      if (policy === "ask") {
        requiresConfirm = true;
      }
    }

    if (!requiresConfirm) {
      return;
    }

    const grantScope = !planMode ? extractScopeKey(event.toolName, event.input) : null;

    if (!planMode && grantScope && hasGrant(grantScope)) {
      return;
    }

    if (!ctx.hasUI) {
      return { block: true, reason: "Vantry approval UI unavailable" };
    }
    try {
      const uiAny = ctx.ui as {
        select?: (title: string, options: string[], opts?: unknown) => Promise<string | undefined>;
        confirm: (title: string, message: string, opts?: unknown) => Promise<boolean>;
      };
      if (typeof uiAny.select === "function") {
        const allowOnce = "允许本次";
        const allowSession = grantScope ? `本会话允许 ${grantScope}` : null;
        const deny = "拒绝";
        const options = allowSession ? [allowOnce, allowSession, deny] : [allowOnce, deny];
        const selectTitle = `${promptTitle}\n${summarizeInput(event.input, 1200)}`;
        let selectUnavailable = false;
        let allowedChoice: string | undefined;
        try {
          allowedChoice = ctx.signal
            ? await uiAny.select(selectTitle, options, { signal: ctx.signal })
            : await uiAny.select(selectTitle, options);
        } catch (selectErr) {
          if (!ctx.signal?.aborted && selectErr instanceof TypeError && /not a function/i.test(selectErr.message)) {
            selectUnavailable = true;
          } else {
            throw selectErr;
          }
        }
        if (!selectUnavailable) {
          if (allowedChoice === allowOnce) {
            return;
          }
          if (allowSession && allowedChoice === allowSession && grantScope) {
            recordGrant(grantScope);
            return;
          }
          return { block: true, reason: "Blocked by Vantry user approval" };
        }
      }

      const allowed = ctx.signal
        ? await uiAny.confirm(promptTitle, summarizeInput(event.input), { signal: ctx.signal })
        : await uiAny.confirm(promptTitle, summarizeInput(event.input));
      if (!allowed) {
        return { block: true, reason: "Blocked by Vantry user approval" };
      }
    } catch {
      return { block: true, reason: "Vantry approval failed" };
    }
  });
}
