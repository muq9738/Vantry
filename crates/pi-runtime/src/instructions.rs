//! Vantry worker addendum and on-demand reference files matching Go v0.1.42.
use crate::{JobRequest, disk, worker::valid_id};
use anyhow::Result;
use std::path::{Path, PathBuf};

const WORKER_INSTRUCTIONS_FILE: &str = "instructions.md";
const VISUAL_BLOCKS_FILE: &str = "visual-blocks.md";
const ORCHESTRATION_FILE: &str = "orchestration.md";
const CLI_FALLBACK_FILE: &str = "cli-fallback.md";
const INSTRUCTIONS_DIR: &str = "instructions";

const VISUAL_REPLY_INSTRUCTIONS: &str = r##"## Native Visuals & Structured Replies (NSH GPU + Offline Inline Previews)

Vantry renders structured Markdown fenced blocks directly in-app (native GPU charts, interactive cards & math + offline sandboxed previews for Mermaid, SVG, and interactive HTML). NEVER start a local HTTP server or fetch external CDNs just to show visuals.

Proactively include visual blocks whenever they clarify the answer (and always when the user asks for charts, comparisons, architecture, flows, interactive prototypes, or visual summaries). Put the core conclusion first; do not restate every number already shown in a visual block. Simple one-line factual answers stay plain text.

Choose the fenced block by the shape of the information:

1. **Trends, numeric comparisons, benchmarks, scatter/combo/heatmaps, shares, breakdowns** -> ```chart
   - Compact pipe-table form (preferred for brevity):
     ```chart bar unit=ms title="延迟对比"
                | p50 | p95
     * Redis    | 12  | 40
     Memcached  | 9   | 31
     Postgres   | 28  | 95
     ```
     Types: `bar` (add `horizontal`, `stacked`, or `100%`), `line` / `area` (auto ISO date/time axes), `pie`, `donut`, `combo`, `heatmap`, `breakdown`. Multi-series `hbar` is supported. Prefix a row with `*` to highlight it; add `ref=100` or `unitRight=%` when useful.
   - Or Studio JSON form: `{"type":"bar|stacked|stacked100|hbar|line|area|pie|donut|scatter|bubble|combo|heatmap|breakdown|stats","title":"...","hero":"...","heroLabel":"...","badge":"...","badgeTone":"success|warning|danger|accent|neutral","delta":"...","style":"dots|tiles","unit":"ms","unitRight":"%","labels":[...],"refLines":[{"value":100,"label":"SLA"}],"series":[{"name":"...","subtitle":"...","badge":"...","kind":"bar|line|area","axis":"left|right","data":[...],"sparkline":[...]}]}`

2. **Architecture, flows, sequences, state machines, ER/class diagrams, Gantt, mindmaps, Sankey, XY charts**:
   - Standard ```mermaid supports ALL Mermaid diagram types (`flowchart`, `sequenceDiagram`, `stateDiagram-v2`, `erDiagram`, `classDiagram`, `gantt`, `mindmap`, `gitGraph`, `pie`, `xychart-beta`, `sankey-beta`, `quadrantChart`, `architecture-beta`).
   - Concise aliases also work: ```flow [LR], ```state [LR], ```sequence [num], ```er, ```class.

3. **Custom vector diagrams & interactive prototypes** -> ```svg / ```html / ```artifact
   - Use ```svg (`title="..."`) for bespoke technical illustrations.
   - Use ```html or ```artifact (`title="..."`) for self-contained offline interactive widgets. Keep zero external URLs/CDNs/fetches.

4. **Mathematical equations**:
   - Inline math `$E = mc^2$` and display blocks `$$\sum_{i=1}^{n} x_i$$` render natively.

5. **Directory layout, module hierarchy & refactor change plans** -> ```tree
   - Indentation sets depth; `name | short note`; prefix changed lines with `+ ` (added), `- ` (removed), or `~ ` (modified).

6. **Phases, milestones, incident or release chronology** -> ```timeline [h|v]
   - `time | title | optional detail`; prefix `*` on the current/key milestone.

7. **Quotas, budgets, SLA / capacity thresholds** -> ```limits
   - `Latency p95 | 142 / 200 | ms`.

8. **Metadata / spec header** -> ```kv [cols=2] (`Key: Value`, `* Wide Key: Value`).
9. **Key takeaway / risk / warning** -> ```callout <info|ok|warn|err> Title with Markdown body.
10. **HTTP / RPC interface contracts** -> ```api (`POST /v1/tasks`, indented `request` / `response 200` and `field type required | description`).
11. **Interactive multi-tab cards / sortable tables / action buttons** -> ```vantry-ui JSON (`metrics`, `table`, `tabs`, `steps`, `kv`, `progress`, `actions`).
12. **Structured user decision** -> ```ask [multi] (question on line 1, then `* Recommended option | note` and `- Alternative | note`; the user can click to reply directly in Vantry).
13. **Semi-automatic orchestration confirmation** -> ```dispatch (optional title on line 1, then `* Task title | role=research|design|implementation|test|review|general | model=<model-id> | count=1 | isolation=shared|worktree | short note`; prefix `* ` for checked or `- ` for unchecked).
"##;

struct ReferencePaths {
    visual: PathBuf,
    orchestration: PathBuf,
    cli_fallback: PathBuf,
}

fn flag(key: &str, fallback: bool) -> bool {
    match std::env::var(key)
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase()
        .as_str()
    {
        "1" | "true" | "yes" | "on" => true,
        "0" | "false" | "no" | "off" => false,
        _ => fallback,
    }
}

fn effective_user_extensions(job: &JobRequest) -> bool {
    flag("VANTRY_PI_USER_EXTENSIONS", job.enable_user_extensions)
}

fn effective_user_mcp(job: &JobRequest) -> bool {
    flag("VANTRY_PI_USER_MCP", job.enable_user_mcp)
}

fn effective_native_subagents(job: &JobRequest) -> bool {
    flag("VANTRY_PI_NATIVE_SUBAGENTS", job.enable_native_subagents)
}

fn is_delegated_child(job: &JobRequest) -> bool {
    !job.request.parent_task_id.trim().is_empty()
}

fn safe_thread_segment(thread_id: &str) -> &str {
    let trimmed = thread_id.trim();
    if !trimmed.is_empty() && valid_id(trimmed).is_ok() {
        trimmed
    } else {
        "default"
    }
}

fn is_subpath(base: &Path, target: &Path) -> bool {
    let Ok(abs_base) = std::path::absolute(base) else {
        return false;
    };
    let Ok(abs_target) = std::path::absolute(target) else {
        return false;
    };
    abs_target.starts_with(&abs_base) && abs_target != abs_base
}

fn resolve_reference_paths(job: &JobRequest, job_dir: &Path) -> ReferencePaths {
    if !job.data_dir.as_os_str().is_empty() && is_subpath(&job.data_dir, job_dir) {
        let inst_dir = job.data_dir.join(INSTRUCTIONS_DIR);
        let thread_dir = inst_dir.join(safe_thread_segment(&job.request.thread_id));
        ReferencePaths {
            visual: inst_dir.join(VISUAL_BLOCKS_FILE),
            orchestration: thread_dir.join(ORCHESTRATION_FILE),
            cli_fallback: thread_dir.join(CLI_FALLBACK_FILE),
        }
    } else {
        ReferencePaths {
            visual: job_dir.join(VISUAL_BLOCKS_FILE),
            orchestration: job_dir.join(ORCHESTRATION_FILE),
            cli_fallback: job_dir.join(CLI_FALLBACK_FILE),
        }
    }
}

fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

fn native_subagent_policy(job: &JobRequest) -> &'static str {
    if effective_native_subagents(job) {
        "- Native child tools (pi-subagents) are enabled by the user for this Core. Use them only in the foreground for same-provider, same-model work inside this turn, and wait with their native tools. This Pi process exits when the turn ends, so never start an async or background native child: it cannot wake this thread. Background, long, cross-model, isolated-checkout or other-machine work uses vantry_delegate_task."
    } else {
        "- Delegation goes through Vantry only. Pi-native child tools (subagent, subagent_supervisor, bg_wait) are disabled in Vantry sessions; do not look for or emulate them. Use vantry_delegate_task: the child is durable, shown under this thread, and Core wakes this thread when it settles. Use isolation: \"worktree\" for independent parallel edits."
    }
}

fn mcp_codemode_guidance(job: &JobRequest) -> &'static str {
    if effective_user_mcp(job) {
        "\nExternal MCP (codemode default): check <mcp_servers>, discover schemas via describeNamespace(\"mcp__<server>\") / searchTools, call tools.mcp__<server>__<tool>(...), and filter/trim large payloads before text()."
    } else {
        ""
    }
}

fn plan_mode_instructions(interaction_mode: &str) -> &'static str {
    if interaction_mode != "plan" {
        return "";
    }
    r#"## Plan Mode (Read-Only Deep Investigation)

- You are in read-only Plan mode (`interaction_mode == "plan"`): write/edit tools are blocked and mutating shell commands require explicit approval. Investigate strictly read-only without modifying files or state.
- Trace the complete chain before concluding or proposing edits: **configuration & data sources** (defaults, env/config files, loaders, persistence/API state) -> **form / UI / handler state & validation** (parsing, normalization, error paths, edge cases) -> **submission, side effects & persistence** (dispatch, IPC/RPC, storage, rollback/refresh).
- Never stop at surface symptoms or guess unread layers; inspect the concrete files and symbols across every stage first."#
}

fn visual_reply_index(ref_path: &Path) -> String {
    format!(
        "## Native Visuals & Structured Replies (On-Demand)\n\n\
Vantry renders native GPU blocks & offline sandboxed inline previews—NEVER start a local HTTP server or load external CDNs for visuals. Simple factual answers stay plain text; lead with conclusions.\n\
Triggers: charts (`chart`); full diagrams (`mermaid`, `flow`, `state`, `sequence`, `er`, `class`); custom vector/interactive prototypes (`svg`, offline `html`/`artifact`); math (`$...$`, `$$...$$`); `tree`, `timeline`, `limits`, `kv`, `callout`, `api`, `vantry-ui`, `ask`, `dispatch`.\n\
Before emitting any visual or `dispatch` block, read {} for exact syntax and schemas.",
        ref_path.display()
    )
}

fn cli_fallback_instructions(job: &JobRequest, exe: &Path) -> String {
    let executable = shell_quote(&exe.to_string_lossy());
    let data_dir = shell_quote(&job.data_dir.to_string_lossy());
    let run_id = shell_quote(&job.request.run_id);
    let attempt_id = shell_quote(&job.request.attempt_id);
    format!(
        "## CLI Fallback (Only If Native vantry_* Tools Are Unavailable)\n\n\
{executable} ctl --data-dir {data_dir} delegate --parent-run {run_id} --parent-attempt {attempt_id} --message 'clear child instructions' --provider pi --resume-policy auto\n\
Add --title 'child title' and --model provider/model when needed. The parent attempt ID prevents stale callbacks.\n\n\
Useful fallback follow-up commands:\n\
{executable} ctl --data-dir {data_dir} status\n\
{executable} ctl --data-dir {data_dir} followup --task TASK_ID --message 'follow-up instructions'\n\
{executable} ctl --data-dir {data_dir} ack --parent-run {run_id} --generation N --delivery DELIVERY_MESSAGE_ID\n\
Use the same --data-dir for every command. Do not write Core's database or job journals directly."
    )
}

fn child_delegation_instructions(job: &JobRequest) -> String {
    let mcp_note = if effective_user_mcp(job) {
        "\n- External MCP servers use codemode by default: discover tools with describeNamespace(\"mcp__<server>\") / searchTools, call tools.mcp__<server>__<tool>(...), and filter/trim large responses inside the script before calling text()."
    } else {
        ""
    };
    format!(
        "You are a Vantry delegated child session (task {}, thread {}).\n\
- Complete the assigned task directly; do not spawn further subagents unless explicitly instructed.\n\
- For ordinary multi-file read / grep / find / ls, call native tools directly in parallel—do NOT wrap routine file reads or greps in codemode.\n\
- Use codemode only when scripting/filtering large JSON or large lists (e.g. list_projects, large API/MCP payloads) or batching multi-subtask orchestration (Promise.allSettled), and always filter/trim data inside the script before calling text(). Use tool_search for deferred tools.{mcp_note}\n\
- Return a concise summary of results, changed file paths, and verification evidence (avoid dumping raw logs or redundant visual blocks).\n\
- For credentials/passwords, call vantry_request_secret; never ask for plaintext secrets or print secret-file contents.",
        job.request.parent_task_id, job.request.thread_id
    )
}

fn main_worker_instructions(job: &JobRequest, refs: &ReferencePaths) -> String {
    let ext = effective_user_extensions(job);
    let mcp = effective_user_mcp(job);
    let ecosystem_note = if ext || mcp {
        format!("User Pi: extensions={ext}, mcp={mcp} (codemode default).")
    } else {
        "User extensions/MCP off unless enabled.".to_string()
    };
    format!(
        "Vantry durable Pi worker. {ecosystem_note}{}\n\n\
Execution:\n\
- Finish tasks end-to-end and verify in this turn; inspect local code/state first and never stop after narrating a next step you can run now.\n\
- On a mid-run steer, answer concisely and immediately continue completing the active goal unless told to stop.\n\
- Interactive TTY (login/2FA/SSH/wizards): call vantry_terminal_run directly (built-in PTY; resumes on exit) instead of osascript, .command wrappers, or /usr/bin/script.\n\
- Secrets: call vantry_request_secret directly (0600 file; unavailable in scripts).\n\n\
Workspace & delegation:\n\
- Ordinary coding tasks need no Vantry orchestration tools.\n\
{}\n\
- Read {} before delegating, showing a ```dispatch card, managing peer threads, or targeting another machine.\n\
- On orchestration/model error, quote it and stop. Never fall back to native subagents, scripted workflows, other CLIs, or another model. Run a scripted workflow only when the user asks for one.\n\n\
Tools & codemode:\n\
- Call native read/grep/find/ls directly in parallel—do NOT wrap routine reads/greps in codemode.\n\
- Web/HTTP: inspect status/headers/charset (e.g. GBK/GB18030) via short curl/python3 scripts; never dump raw HTML.\n\
- Use codemode only for large JSON/lists (list_projects, large API/MCP payloads), batch Promise.allSettled, or codemode MCP/Vantry tools; trim before text().\n\
- Context: thread={}",
        mcp_codemode_guidance(job),
        native_subagent_policy(job),
        refs.orchestration.display(),
        job.request.thread_id,
    )
}

fn orchestration_reference_instructions(
    job: &JobRequest,
    refs: &ReferencePaths,
    socket: &Path,
) -> String {
    format!(
        "## Vantry Multi-Agent Orchestration & Peer Sessions\n\n\
{}\n\
- Ordinary sessions defer orchestration tools by default: call tools.vantry_* directly in codemode or discover them with tool_search.\n\
- Delegate only when requested or when clearly multi-part work benefits from parallel execution; keep simple work in this thread. Run a scripted workflow only when the user asks for one.\n\
- Dispatch confirmation: when delegation is warranted or models/roles/parallelism are specified, filter models via codemode before text() and show a ```dispatch card (check {} for schema). Wait for confirmation unless told to dispatch immediately.\n\
- Batch dispatch: after confirmation, launch parallel children in ONE codemode call via Promise.allSettled over tools.vantry_delegate_task(...) and trim returned fields before text(). Put shared context in one file; children share builtin codemode and tool_search.\n\
- Keep local work local; saved machine links are no reason to move it. For another machine, query vantry_environment_list and pass environmentId to vantry_orchestrator_capabilities and vantry_delegate_task (remote defaults/paths belong to that target).\n\
- Call vantry_orchestrator_capabilities once per thread before first delegation (again only after an error or user-named provider change). Never switch provider/model or machine unasked.\n\
- If vantry_orchestrator_capabilities or vantry_delegate_task fails, or the model is unavailable, quote the error and stop that path. Never fall back to native subagents, scripted workflows, other CLIs, or another model.\n\
- Use vantry_thread_launch and vantry_create_threads only for requested top-level sessions, not subagents. When fewer or no delegates are requested, cancel extras without replacement; follow a running delegate's next instruction.\n\
- Durable children: vantry_delegate_task defaults to async (do NOT poll vantry_task_status or sleep; wait for Core wakeup). Inspect via vantry_task_status; cancel with vantry_task_cancel, never thread interrupt.\n\
- Low-frequency tools (discover via tool_search or codemode searchTools/describeTool/describeNamespace(\"vantry\")): vantry_environment_list, vantry_thread_list, vantry_thread_search, vantry_thread_read, vantry_thread_send, vantry_thread_wait, vantry_thread_launch, vantry_create_threads, vantry_thread_transfer, vantry_thread_manage, vantry_task_followup, vantry_task_ack.\n\
- Codemode orchestration rules: await all calls, keep dependent actions sequential, preserve environmentId, request IDs, approvals, and cancellation. Scripts do not replace Core's durable task lifecycle.\n\
- Thread references: use the returned markdown Link (including ?environmentId=); local form [Title](vantry-thread://<threadId>).\n\
- Thread context: thread={}, socket={}\n\
- CLI fallback (only if native vantry_* tools are unavailable): read {} for exact vantry ctl commands. Do not write Core's database or job journals directly.",
        native_subagent_policy(job),
        refs.visual.display(),
        job.request.thread_id,
        socket.display(),
        refs.cli_fallback.display(),
    )
}

/// Write worker instruction files into `<dataDir>/instructions/` (or `<jobDir>`)
/// and return the path to `<jobDir>/instructions.md`.
pub(crate) fn write_worker_instruction_files(
    job: &JobRequest,
    job_dir: &Path,
    socket: &Path,
    exe: &Path,
    interaction_mode: &str,
) -> Result<PathBuf> {
    let refs = resolve_reference_paths(job, job_dir);
    if !is_delegated_child(job) {
        if let Some(parent) = refs.visual.parent() {
            disk::directory(parent)?;
        }
        disk::atomic(&refs.visual, VISUAL_REPLY_INSTRUCTIONS.as_bytes())?;
        if let Some(parent) = refs.orchestration.parent() {
            disk::directory(parent)?;
        }
        disk::atomic(
            &refs.orchestration,
            orchestration_reference_instructions(job, &refs, socket).as_bytes(),
        )?;
        disk::atomic(
            &refs.cli_fallback,
            cli_fallback_instructions(job, exe).as_bytes(),
        )?;
    }

    let body = if is_delegated_child(job) {
        child_delegation_instructions(job)
    } else {
        format!(
            "{}\n\n{}",
            main_worker_instructions(job, &refs),
            visual_reply_index(&refs.visual)
        )
    };
    let full = match plan_mode_instructions(interaction_mode) {
        "" => body,
        plan => format!("{plan}\n\n{body}"),
    };
    let path = job_dir.join(WORKER_INSTRUCTIONS_FILE);
    disk::atomic(&path, full.as_bytes())?;
    Ok(path)
}

#[cfg(test)]
mod tests {
    use super::*;
    use vantry_protocol::ProviderRequest;

    #[test]
    fn writes_main_and_reference_instruction_files() -> Result<()> {
        let tmp = tempfile::tempdir()?;
        let data = tmp.path().canonicalize()?;
        let job_dir = data.join("jobs").join("att_1");
        std::fs::create_dir_all(&job_dir)?;
        let job = JobRequest {
            data_dir: data.clone(),
            request: ProviderRequest {
                thread_id: "th_abc".into(),
                run_id: "run_1".into(),
                attempt_id: "att_1".into(),
                ..Default::default()
            },
            executable: PathBuf::from("/usr/local/bin/vantry-service"),
            socket_path: data.join("core.sock"),
            enable_native_subagents: false,
            default_model: "openai/gpt-5".into(),
            expected_model: "openai/gpt-5".into(),
            thinking_level: "high".into(),
            enable_user_extensions: true,
            enable_user_mcp: true,
            sandbox_mode: String::new(),
            interaction_mode: "plan".into(),
        };
        let path = write_worker_instruction_files(
            &job,
            &job_dir,
            &job.socket_path,
            &job.executable,
            "plan",
        )?;
        let text = std::fs::read_to_string(&path)?;
        assert!(text.contains("Vantry durable Pi worker"));
        assert!(text.contains("visual-blocks.md"));
        assert!(text.contains("orchestration.md"));
        assert!(text.contains("Plan Mode"));
        assert!(data.join("instructions/visual-blocks.md").is_file());
        assert!(data.join("instructions/th_abc/orchestration.md").is_file());
        assert!(data.join("instructions/th_abc/cli-fallback.md").is_file());

        let (base, lvl) = crate::split_model_thinking("openai/o3:high");
        assert_eq!(base, "openai/o3");
        assert_eq!(lvl.map(|l| l.as_str()), Some("high"));
        let (base2, lvl2) = crate::split_model_thinking("openai/gpt-4o");
        assert_eq!(base2, "openai/gpt-4o");
        assert!(lvl2.is_none());
        Ok(())
    }
}
