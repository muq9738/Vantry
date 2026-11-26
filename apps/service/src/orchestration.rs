use crate::{ctl::identity, workbench::Workbench};
use anyhow::{Context, Result, bail, ensure};
use serde_json::{Value, json};
use std::time::Duration;
use vantry_client::CoreClient;
use vantry_core::{Core, Error};
use vantry_protocol::*;
fn missing(what: &str) -> Error {
    Error::Api(ErrorCode::NotFound, format!("{what} not found"))
}
fn thread<'a>(s: &'a Snapshot, id: &str) -> Result<&'a Thread> {
    ensure!(!id.is_empty(), "threadId required");
    Ok(s.threads
        .iter()
        .flatten()
        .find(|t| t.id == id)
        .ok_or_else(|| missing("thread"))?)
}
fn runs<'a>(s: &'a Snapshot, id: &str) -> Vec<&'a Run> {
    let mut v: Vec<_> = s
        .runs
        .iter()
        .flatten()
        .filter(|r| r.thread_id == id)
        .collect();
    v.sort_by_key(|r| (&r.created_at, &r.id));
    v
}
fn link(t: &Thread) -> String {
    let name = if t.title.trim().is_empty() {
        &t.id
    } else {
        t.title.trim()
    };
    thread_markdown(name, &format!("{THREAD_SCHEME}{}", t.id))
}
fn status(s: RunStatus) -> String {
    json!(s).as_str().unwrap_or_default().into()
}
fn bound(n: i64, default: usize, max: usize) -> usize {
    if n <= 0 {
        default
    } else {
        (n as usize).min(max)
    }
}
fn item(s: &Snapshot, t: &Thread) -> ThreadListItem {
    let runs = runs(s, &t.id);
    let latest = runs
        .iter()
        .rev()
        .find(|r| !r.status.terminal())
        .or(runs.last());
    let ws = t.workspace.as_ref();
    let pending_inputs = s
        .inputs
        .iter()
        .filter(|i| {
            i.thread_id == t.id
                && (matches!(i.status.as_str(), "pending" | "queued")
                    || i.status == "started"
                        && runs
                            .iter()
                            .any(|r| r.id == i.run_id && !r.status.terminal()))
        })
        .count() as i64;
    let pending_requests = s
        .requests
        .iter()
        .filter(|r| r.thread_id == t.id && matches!(r.status.as_str(), "pending" | "responding"))
        .count() as i64;
    ThreadListItem {
        thread_id: t.id.clone(),
        title: t.title.clone(),
        link: link(t),
        status: latest
            .map(|r| status(r.status))
            .unwrap_or_else(|| "idle".into()),
        model: t.model.clone(),
        approval_policy: t.approval_policy.clone(),
        interaction_mode: t.interaction_mode.clone(),
        workspace_path: ws
            .map(|w| {
                if w.path.is_empty() {
                    w.repository.clone()
                } else {
                    w.path.clone()
                }
            })
            .unwrap_or_default(),
        branch: ws.map(|w| w.branch.clone()).unwrap_or_default(),
        is_worktree: ws.is_some_and(|w| w.kind == "worktree"),
        parent_thread_id: t.parent_thread_id.clone(),
        created_by_thread_id: t.created_by_thread_id.clone(),
        pinned: t.pinned,
        settled: runs.iter().all(|r| r.status.terminal())
            && pending_inputs == 0
            && pending_requests == 0
            && !s
                .tasks
                .iter()
                .flatten()
                .any(|task| task.parent_thread_id == t.id && !task.status.terminal()),
        snoozed: t
            .snoozed_until
            .as_ref()
            .and_then(|v| chrono::DateTime::parse_from_rfc3339(v).ok())
            .is_some_and(|at| at > chrono::Utc::now()),
        snoozed_until: t.snoozed_until.clone(),
        run_count: runs.len() as i64,
        pending_inputs,
        pending_requests,
        updated_at: runs
            .iter()
            .map(|r| &r.updated_at)
            .chain([&t.created_at])
            .max()
            .unwrap()
            .clone(),
        ..Default::default()
    }
}
fn list(s: &Snapshot, mut q: ThreadListQuery, default: Option<&WorkspaceSpec>) -> Result<Value> {
    ensure!(q.cursor >= 0 && q.limit >= 0, "negative limit or cursor");
    if !q.all_projects && q.workspace.is_empty() {
        let spec = if q.caller_thread_id.is_empty() {
            default
        } else {
            thread(s, &q.caller_thread_id)?.workspace.as_ref()
        };
        if let Some(w) = spec {
            q.workspace = if w.path.is_empty() {
                w.repository.clone()
            } else {
                w.path.clone()
            };
        }
    }
    let mut items: Vec<_> = s
        .threads
        .iter()
        .flatten()
        .filter(|t| {
            (q.include_subagents || t.parent_thread_id.is_empty())
                && (q.all_projects
                    || q.workspace.is_empty()
                    || t.workspace
                        .as_ref()
                        .is_some_and(|w| w.path == q.workspace || w.repository == q.workspace))
                && t.title
                    .to_lowercase()
                    .contains(&q.title_contains.to_lowercase())
        })
        .map(|t| item(s, t))
        .filter(|t| {
            q.settled.is_none_or(|v| v == t.settled)
                && q.snoozed.is_none_or(|v| v == t.snoozed)
                && (q.statuses.is_empty() || q.statuses.contains(&t.status))
        })
        .collect();
    items.sort_by(|a, b| (&b.updated_at, &b.thread_id).cmp(&(&a.updated_at, &a.thread_id)));
    let total = items.len() as i64;
    let page: Vec<_> = items
        .into_iter()
        .skip(q.cursor as usize)
        .take(bound(q.limit, 25, 100))
        .collect();
    let next = q.cursor + page.len() as i64;
    Ok(json!(ThreadListResult {
        current_thread_id: q.caller_thread_id,
        total,
        threads: Some(page),
        next_cursor: (next < total).then_some(next)
    }))
}
fn read(s: &Snapshot, q: ThreadReadQuery) -> Result<Value> {
    ensure!(
        matches!(q.view.as_str(), "" | "messages" | "activity")
            && [
                q.limit,
                q.run_limit,
                q.text_offset,
                q.max_chars_per_item,
                q.after_position.unwrap_or(0),
                q.before_position.unwrap_or(0)
            ]
            .iter()
            .all(|n| *n >= 0),
        "invalid timeline query"
    );
    let t = thread(s, &q.thread_id)?;
    let history = runs(s, &t.id);
    let mut recent = vec![];
    let mut items = vec![];
    let mut position = 0;
    for (i, r) in history.iter().enumerate() {
        recent.push(ThreadRunSummary {
            ordinal: (i + 1) as i64,
            run_id: r.id.clone(),
            created_at: r.created_at.clone(),
            updated_at: r.updated_at.clone(),
            status: r.status,
            model: r.model.clone(),
            sender_thread_id: r.sender_thread_id.clone(),
        });
        let mut push =
            |id: String, kind: &str, who: &str, title: &str, body: &str, st: String, at: &str| {
                let pos = position;
                position += 1;
                if q.after_position.is_some_and(|n| pos <= n)
                    || q.before_position.is_some_and(|n| pos >= n)
                    || (!q.item_id.is_empty() && q.item_id != id)
                {
                    return;
                }
                let mut chars = body.chars().skip(q.text_offset as usize);
                let text: String = chars
                    .by_ref()
                    .take(bound(q.max_chars_per_item, 4000, 30000))
                    .collect();
                let next = q.text_offset + text.chars().count() as i64;
                let truncated = chars.next().is_some();
                items.push(ThreadTimelineItem {
                    position: pos,
                    run_id: r.id.clone(),
                    item_id: id,
                    r#type: kind.into(),
                    created_by: who.into(),
                    sender_thread_id: if who == "user" {
                        r.sender_thread_id.clone()
                    } else {
                        String::new()
                    },
                    title: title.into(),
                    text,
                    status: st,
                    updated_at: at.into(),
                    text_truncated: truncated,
                    next_text_offset: truncated.then_some(next),
                });
            };
        push(
            format!("{}:user", r.id),
            "user_message",
            "user",
            "",
            &r.input,
            status(r.status),
            &r.created_at,
        );
        if q.view == "activity" {
            if !r.reasoning.is_empty() {
                push(
                    format!("{}:reasoning", r.id),
                    "reasoning",
                    "agent",
                    "",
                    &r.reasoning,
                    status(r.status),
                    &r.updated_at,
                );
            }
            for tool in s.tools.iter().filter(|v| v.run_id == r.id) {
                push(
                    tool.id.clone(),
                    "tool_call",
                    "system",
                    &tool.name,
                    &format!(
                        "{}\n{}",
                        tool.arguments.as_ref().unwrap_or(&Value::Null),
                        tool.output
                    ),
                    tool.status.clone(),
                    &tool.updated_at,
                );
            }
            for req in s.requests.iter().filter(|v| v.run_id == r.id) {
                push(
                    req.id.clone(),
                    "approval_request",
                    "system",
                    &req.title,
                    &req.message,
                    req.status.clone(),
                    &req.updated_at,
                );
            }
            for task in s.tasks.iter().flatten().filter(|v| v.parent_run_id == r.id) {
                push(
                    task.id.clone(),
                    "subagent",
                    "agent",
                    &task.title,
                    &task.result,
                    status(task.status),
                    &task.updated_at,
                );
            }
        }
        push(
            format!("{}:assistant", r.id),
            "assistant_message",
            "agent",
            "",
            &r.output,
            status(r.status),
            &r.updated_at,
        );
        if q.view == "activity" && !r.error.is_empty() {
            push(
                format!("{}:error", r.id),
                "error",
                "system",
                "",
                &r.error,
                status(r.status),
                &r.updated_at,
            );
        }
    }
    let limit = bound(q.limit, 20, 100);
    if q.after_position.is_none() {
        items.drain(..items.len().saturating_sub(limit));
    } else {
        items.truncate(limit);
    }
    recent.drain(..recent.len().saturating_sub(bound(q.run_limit, 10, 100)));
    let earlier = items.first().filter(|i| i.position > 0).map(|i| i.position);
    let next = items
        .last()
        .filter(|i| i.position + 1 < position)
        .map(|i| i.position);
    Ok(json!(ThreadReadResult {
        thread: item(s, t),
        recent_runs: Some(recent),
        items: Some(items),
        has_more: next.is_some() || earlier.is_some(),
        next_position: next,
        earlier_position: earlier
    }))
}
fn format_env_thread_link(env_id: &str, link: &str) -> String {
    if env_id.is_empty() || link.contains("environmentId=") {
        return link.to_owned();
    }
    if let Some(pos) = link.rfind("](vantry-thread://") {
        let prefix = &link[..pos];
        let rest = &link[pos + 2..];
        if let Some(end) = rest.rfind(')') {
            let url = &rest[..end];
            let sep = if url.contains('?') { '&' } else { '?' };
            return format!("{prefix}]({url}{sep}environmentId={env_id})");
        }
    }
    link.to_owned()
}

fn decorate_remote_links(val: &mut Value, env_id: &str) {
    match val {
        Value::Object(map) => {
            if let Some(Value::String(s)) = map.get_mut("link") {
                *s = format_env_thread_link(env_id, s);
            }
            for (_, v) in map.iter_mut() {
                decorate_remote_links(v, env_id);
            }
        }
        Value::Array(arr) => {
            for v in arr.iter_mut() {
                decorate_remote_links(v, env_id);
            }
        }
        _ => {}
    }
}

fn details(w: &Workbench, s: &Snapshot, t: &Task) -> Result<TaskDetail> {
    let child = thread(s, &t.child_thread_id)?;
    let requests: Vec<_> = s
        .requests
        .iter()
        .filter(|r| {
            r.thread_id == child.id && matches!(r.status.as_str(), "pending" | "responding")
        })
        .collect();
    let child_attempt_id = s
        .runs
        .iter()
        .flatten()
        .find(|r| r.id == t.child_run_id)
        .map(|r| r.attempt_id.clone())
        .unwrap_or_default();
    let remote = (!child_attempt_id.is_empty())
        .then(|| w.federator.get_task(&child_attempt_id))
        .flatten()
        .or_else(|| w.federator.find_by_local_thread(&t.child_thread_id));
    let environment_id = remote
        .as_ref()
        .map(|h| h.environment_id.clone())
        .or_else(|| {
            child
                .workspace
                .as_ref()
                .map(|ws| ws.environment_id.clone())
                .filter(|s| !s.is_empty())
        })
        .unwrap_or_default();
    let remote_thread_id = remote
        .as_ref()
        .map(|h| h.remote_thread_id.clone())
        .unwrap_or_default();
    let remote_run_id = remote
        .as_ref()
        .map(|h| h.remote_run_id.clone())
        .unwrap_or_default();
    let has_pending_question = requests.iter().any(|r| r.method != "confirm");
    let pending_approval = requests.iter().find(|r| r.method == "confirm").map(|r| {
        PendingApprovalSummary {
            request_id: r.id.clone(),
            method: r.method.clone(),
            title: r.title.clone(),
            run_id: r.run_id.clone(),
        }
    });
    let has_pending_child_runs = s
        .tasks
        .iter()
        .flatten()
        .any(|v| v.parent_thread_id == child.id && !v.status.terminal());
    let next = if pending_approval.is_some() || has_pending_question {
        "Child is waiting for user approval or input.".to_owned()
    } else if !t.status.terminal() || has_pending_child_runs {
        "Completion wakes this thread, so end your turn. Do not wait on it or poll it.".to_owned()
    } else {
        String::new()
    };
    Ok(TaskDetail {
        task_id: t.id.clone(),
        title: t.title.clone(),
        parent_run_id: t.parent_run_id.clone(),
        parent_thread_id: t.parent_thread_id.clone(),
        child_thread_id: t.child_thread_id.clone(),
        child_run_id: t.child_run_id.clone(),
        child_attempt_id,
        environment_id,
        remote_thread_id,
        remote_run_id,
        link: link(child),
        model: child.model.clone(),
        status: t.status,
        work_state: status(t.status),
        delivery_status: t.delivery_status,
        has_pending_question,
        pending_approval,
        has_pending_child_runs,
        summary: t.result.clone(),
        error: t.error.clone(),
        next,
        updated_at: t.updated_at.clone(),
        ..Default::default()
    })
}
async fn wait(core: &Core, q: ThreadWaitQuery) -> Result<ThreadWaitResult> {
    ensure!(q.timeout_ms >= 0, "negative timeout");
    let deadline = tokio::time::Instant::now()
        + Duration::from_millis(bound(q.timeout_ms, 120000, 600000) as u64);
    let mut target = q.run_id;
    let client = CoreClient::from_data_dir(core.directory());
    let mut stream = None;
    loop {
        let s = core.snapshot()?;
        let t = thread(&s, &q.thread_id)?;
        let runs = runs(&s, &t.id);
        let selected = if target.is_empty() {
            runs.last().copied()
        } else {
            Some(
                *runs
                    .iter()
                    .find(|r| r.id == target)
                    .ok_or_else(|| missing("run"))?,
            )
        };
        let mut result = ThreadWaitResult {
            thread_id: t.id.clone(),
            link: link(t),
            status: "idle".into(),
            ..Default::default()
        };
        let Some(r) = selected else { return Ok(result) };
        target.clone_from(&r.id);
        result.run_id = r.id.clone();
        result.status = status(r.status);
        result.summary.clone_from(&r.output);
        for req in s.requests.iter().filter(|v| {
            v.thread_id == t.id && matches!(v.status.as_str(), "pending" | "responding")
        }) {
            if req.method == "confirm" {
                result
                    .pending_approval
                    .get_or_insert_with(|| PendingApprovalSummary {
                        request_id: req.id.clone(),
                        method: req.method.clone(),
                        title: req.title.clone(),
                        run_id: req.run_id.clone(),
                    });
            } else if matches!(req.method.as_str(), "select" | "input" | "editor") {
                result.has_pending_question = true;
            }
        }
        if result.pending_approval.is_some() || result.has_pending_question {
            return Ok(result);
        }
        result.error = r.error.clone();
        let settled = !q.until_delegated_work_settled
            || !s
                .tasks
                .iter()
                .flatten()
                .any(|v| v.parent_thread_id == t.id && !v.status.terminal());
        if r.status.terminal() && settled {
            return Ok(result);
        }
        if tokio::time::Instant::now() >= deadline {
            result.timed_out = true;
            if let Some(task) = s.tasks.iter().flatten().find(|v| {
                !q.caller_thread_id.is_empty()
                    && v.parent_thread_id == q.caller_thread_id
                    && v.child_thread_id == q.thread_id
                    && v.child_run_id == r.id
            }) {
                core.dispatch(&Command {
                    parent_run_id: task.parent_run_id.clone(),
                    task_id: task.id.clone(),
                    generation: task.generation,
                    ..command(
                        CommandKind::UpgradeTaskWake,
                        format!("wake:{}:{}", task.id, task.generation),
                    )
                })?;
                result.auto_wake_registered = true;
            }
            return Ok(result);
        }
        if stream.is_none() {
            stream = Some(client.subscribe(s.sequence).await?);
        }
        match tokio::time::timeout_at(deadline, stream.as_mut().unwrap().next_event()).await {
            Ok(Ok(Some(_))) | Err(_) => {}
            Ok(Err(vantry_client::ClientError::ResyncRequired(_))) => stream = None,
            Ok(Ok(None)) => bail!("event stream closed"),
            Ok(Err(e)) => return Err(e.into()),
        }
    }
}
fn broker_id(operation: &str, caller: &str, id: &str) -> String {
    if id.is_empty() {
        identity(String::new())
    } else {
        format!("{operation}:{caller}:{id}")
    }
}
fn rank(policy: &str) -> usize {
    match policy {
        "ask" => 0,
        "auto-accept-edits" => 1,
        _ => 2,
    }
}
fn launch(w: &Workbench, s: &Snapshot, q: ThreadLaunchRequest) -> Result<LaunchedThread> {
    let value = serde_json::to_string(&q)?;
    let caller = if q.caller_thread_id.is_empty() {
        None
    } else {
        Some(thread(s, &q.caller_thread_id)?)
    };
    if !q.caller_run_id.is_empty() {
        ensure!(
            s.runs
                .iter()
                .flatten()
                .any(|r| r.id == q.caller_run_id && r.thread_id == q.caller_thread_id),
            "callerRunId does not belong to caller"
        );
    }
    let id = broker_id("launch", &q.caller_thread_id, &q.client_request_id);
    let mut spec = q.spec;
    ensure!(
        spec.environment_id.is_empty()
            || spec.environment_id == "local"
            || spec.environment_id == w.environment,
        "remote environment launch must be routed via environmentId"
    );
    let mut workspace = spec.workspace.take();
    if spec.scratch {
        ensure!(
            workspace.is_none(),
            "scratch and workspace are mutually exclusive"
        );
        use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
        let component = URL_SAFE_NO_PAD.encode(&id);
        ensure!(component.len() < 240, "scratch request ID too long");
        let path = w
            .core
            .directory()
            .join("workspaces")
            .join(component)
            .join("chat");
        workspace = Some(WorkspaceSpec {
            kind: "directory".into(),
            path: path.display().to_string(),
            ..Default::default()
        });
    } else if workspace.is_none() {
        workspace = caller.and_then(|c| c.workspace.clone());
    }
    if let Some(c) = caller {
        if spec.approval_policy.is_empty() {
            spec.approval_policy.clone_from(&c.approval_policy)
        }
        if spec.interaction_mode.is_empty() {
            spec.interaction_mode.clone_from(&c.interaction_mode)
        }
        ensure!(
            rank(&spec.approval_policy) <= rank(&c.approval_policy)
                && (c.interaction_mode != "plan" || spec.interaction_mode == "plan"),
            "requested permissions exceed caller permissions"
        );
        if spec.model.is_empty() {
            spec.model.clone_from(&c.model)
        }
    }
    if !spec.thinking.is_empty() {
        ensure!(!spec.model.is_empty(), "thinking level requires a model");
        spec.model = format!("{}:{}", spec.model, spec.thinking);
    }
    let c = Command {
        kind: CommandKind::CreateThread,
        version: 1,
        command_id: id.clone(),
        title: spec.title,
        workspace,
        provider_id: caller.map(|c| c.provider_id.clone()).unwrap_or_default(),
        model: spec.model,
        approval_policy: spec.approval_policy,
        interaction_mode: spec.interaction_mode,
        created_by_thread_id: q.caller_thread_id.clone(),
        created_by_run_id: q.caller_run_id,
        value,
        ..Default::default()
    };
    let receipt = w.core.dispatch(&c)?;
    if spec.scratch {
        vantry_platform::hostfs::ensure_private_dir(std::path::Path::new(
            &c.workspace.as_ref().unwrap().path,
        ))?;
    }
    let sent = if spec.message.trim().is_empty() {
        None
    } else {
        Some(w.core.dispatch(&Command {
            thread_id: receipt.thread_id.clone(),
            sender_thread_id: q.caller_thread_id.clone(),
            message: spec.message,
            ..command(CommandKind::SendMessage, format!("{id}:message"))
        })?)
    };
    let snapshot = w.core.snapshot()?;
    let t = thread(&snapshot, &receipt.thread_id)?;
    let run = sent
        .as_ref()
        .and_then(|sent| snapshot.runs.iter().flatten().find(|r| r.id == sent.run_id));
    if q.notify_on_complete && !q.caller_thread_id.is_empty() {
        w.core.register_watch(
            &q.caller_thread_id,
            &t.id,
            run.map(|r| r.id.as_str()).unwrap_or(""),
        )?;
    }
    Ok(LaunchedThread {
        thread_id: t.id.clone(),
        title: t.title.clone(),
        link: link(t),
        model: t.model.clone(),
        run_id: run.map(|r| r.id.clone()).unwrap_or_default(),
        status: run.map(|r| r.status).unwrap_or_default(),
        ..Default::default()
    })
}
fn command(kind: CommandKind, id: String) -> Command {
    Command {
        version: 1,
        command_id: identity(id),
        kind,
        ..Default::default()
    }
}
async fn forward_remote_orchestration(
    w: &Workbench,
    env_id: &str,
    path: &str,
    mut payload: Value,
) -> Result<Value> {
    if let Some(obj) = payload.as_object_mut() {
        obj.remove("environmentId");
        // Caller thread/run IDs belong to the local Core, not the remote peer.
        obj.remove("callerThreadId");
        obj.remove("callerRunId");
        if let Some(spec) = obj.get_mut("spec").and_then(Value::as_object_mut) {
            spec.remove("environmentId");
        }
    }
    let target = crate::federation::resolve_target(w.core.directory(), &w.identity, env_id).await?;
    let mut result = target
        .post_json(&format!("/v1/orchestration/{path}"), payload)
        .await?;
    decorate_remote_links(&mut result, env_id);
    if let Some(obj) = result.as_object_mut()
        && matches!(path, "threads/launch" | "threads/send" | "threads/wait")
    {
        obj.entry("environmentId").or_insert_with(|| json!(env_id));
    }
    Ok(result)
}

fn first_non_empty<'a>(a: &'a str, b: &'a str) -> &'a str {
    if !a.trim().is_empty() { a } else { b }
}

pub async fn call(w: &Workbench, path: &str, body: &[u8]) -> Result<Value> {
    let payload: Value = serde_json::from_slice(body)?;
    ensure!(payload.is_object(), "expected request object");
    let env_id = payload["environmentId"]
        .as_str()
        .filter(|s| !s.is_empty())
        .or_else(|| {
            payload["spec"]["environmentId"]
                .as_str()
                .filter(|s| !s.is_empty())
        })
        .unwrap_or("")
        .to_owned();
    let is_remote = !env_id.is_empty() && env_id != "local" && env_id != w.environment;
    if is_remote && !matches!(path, "delegate" | "tasks/status" | "task-status" | "tasks/cancel" | "task-cancel") {
        return forward_remote_orchestration(w, &env_id, path, payload).await;
    }
    let core = &w.core;
    let s = core.snapshot()?;
    match path {
        "threads/list" => list(
            &s,
            serde_json::from_value(payload)?,
            w.options.workspace.as_ref(),
        ),
        "threads/read" => read(&s, serde_json::from_value(payload)?),
        "threads/wait" => Ok(json!(wait(core, serde_json::from_value(payload)?).await?)),
        "capabilities" => {
            let q: OrchestratorCapabilitiesQuery = serde_json::from_value(payload)?;
            let caller = if q.caller_thread_id.is_empty() {
                None
            } else {
                Some(thread(&s, &q.caller_thread_id)?)
            };
            let model = caller.map(|t| t.model.as_str()).unwrap_or(&w.options.model);
            let catalog = w.models(model).await?;
            let models = catalog
                .models
                .unwrap_or_default()
                .into_iter()
                .filter(|m| {
                    format!("{} {}", m.id, m.name)
                        .to_lowercase()
                        .contains(&q.search.to_lowercase())
                })
                .map(|m| OrchestratorModelCapability {
                    id: format!(
                        "{}/{}",
                        m.provider,
                        m.id.trim_start_matches(&format!("{}/", m.provider))
                    ),
                    label: m.name,
                    ..Default::default()
                })
                .collect();
            Ok(json!(OrchestratorCapabilities {
                parent_thread_id: q.caller_thread_id,
                inherited_provider_id: caller
                    .map(|t| &t.provider_id)
                    .unwrap_or(&w.options.provider_id)
                    .clone(),
                inherited_model: model.into(),
                approval_policy: caller
                    .map(|t| t.approval_policy.clone())
                    .unwrap_or_else(|| "ask".into()),
                interaction_mode: caller
                    .map(|t| t.interaction_mode.clone())
                    .unwrap_or_else(|| "default".into()),
                models: Some(models),
                approval_policies: Some(
                    ["ask", "auto-accept-edits", "auto"]
                        .into_iter()
                        .filter(|p| caller.is_none_or(|c| rank(p) <= rank(&c.approval_policy)))
                        .map(String::from)
                        .collect()
                ),
                interaction_modes: Some(
                    ["plan", "default"]
                        .into_iter()
                        .filter(
                            |p| caller.is_none_or(|c| c.interaction_mode != "plan" || *p == "plan")
                        )
                        .map(String::from)
                        .collect()
                ),
                max_batch_threads: 20,
                ..Default::default()
            }))
        }
        "threads/launch" => Ok(json!(launch(w, &s, serde_json::from_value(payload)?)?)),
        "threads/create-batch" => {
            let q: CreateThreadsRequest = serde_json::from_value(payload)?;
            let specs = q.threads.unwrap_or_default();
            ensure!(
                (1..=20).contains(&specs.len()),
                "batch size must be between 1 and 20"
            );
            let batch = identity(q.client_request_id);
            let mut threads = vec![];
            for (i, spec) in specs.into_iter().enumerate() {
                threads.push(launch(
                    w,
                    &s,
                    ThreadLaunchRequest {
                        caller_thread_id: q.caller_thread_id.clone(),
                        caller_run_id: q.caller_run_id.clone(),
                        client_request_id: format!("{batch}:thread:{i}"),
                        spec,
                        ..Default::default()
                    },
                )?);
            }
            Ok(json!({"threads":threads}))
        }
        "threads/send" => {
            let q: InterThreadSendRequest = serde_json::from_value(payload)?;
            let t = thread(&s, &q.target_thread_id)?;
            let r = core.dispatch(&Command {
                thread_id: t.id.clone(),
                target_thread_id: t.id.clone(),
                message: q.message,
                delivery_mode: q.mode,
                fallback_to_queue: true,
                command_id: broker_id("send", &q.caller_thread_id, &q.client_request_id),
                sender_thread_id: q.caller_thread_id.clone(),
                version: 1,
                kind: CommandKind::InterThreadSend,
                ..Default::default()
            })?;
            if q.notify_on_complete && !q.caller_thread_id.is_empty() {
                core.register_watch(&q.caller_thread_id, &t.id, &r.run_id)?;
            }
            Ok(json!(InterThreadSendResult {
                thread_id: t.id.clone(),
                link: link(t),
                run_id: r.run_id,
                input_id: r.input_id,
                delivery: r.delivery,
                sequence: r.sequence,
                task_id: r.task_id,
                ..Default::default()
            }))
        }
        "delegate" => {
            let q: DelegateTaskRequest = serde_json::from_value(payload.clone())?;
            ensure!(
                matches!(q.mode.as_str(), "" | "async" | "wait")
                    && matches!(q.isolation.as_str(), "" | "shared" | "worktree")
                    && q.timeout_ms >= 0
                    && !q.task.trim().is_empty(),
                "invalid delegation"
            );
            let parent = s
                .runs
                .iter()
                .flatten()
                .find(|r| r.id == q.parent_run_id)
                .ok_or_else(|| missing("parent run"))?;
            ensure!(
                q.caller_thread_id.is_empty() || q.caller_thread_id == parent.thread_id,
                "caller does not own parent run"
            );
            let caller = thread(&s, &parent.thread_id)?;
            let mut provider_id = q.provider_id.clone();
            let mut model = q.model.clone();
            let mut approval_policy = q.approval_policy.clone();
            let mut interaction_mode = q.interaction_mode.clone();
            let ws = if is_remote {
                let target =
                    crate::federation::resolve_target(w.core.directory(), &w.identity, &env_id)
                        .await?;
                let info = target.environment_info().await?;
                if approval_policy.is_empty() {
                    approval_policy.clone_from(&caller.approval_policy);
                }
                if interaction_mode.is_empty() {
                    interaction_mode.clone_from(&caller.interaction_mode);
                }
                ensure!(
                    rank(&approval_policy) <= rank(&caller.approval_policy)
                        && (caller.interaction_mode != "plan" || interaction_mode == "plan"),
                    "远端会话的权限或交互模式高于发起会话"
                );
                provider_id = first_non_empty(&provider_id, &info.provider_id).to_owned();
                let resolved_model = first_non_empty(&model, &info.default_model).to_owned();
                if model.is_empty()
                    && !caller.model.is_empty()
                    && !resolved_model.is_empty()
                    && caller.model != resolved_model
                {
                    return Err(Error::Api(
                        ErrorCode::Conflict,
                        "目标环境的默认模型与父会话不同；请查看目标能力并显式选择 model".into(),
                    )
                    .into());
                }
                if resolved_model.is_empty() && provider_id != "controlled" {
                    return Err(Error::Api(
                        ErrorCode::NotReady,
                        "目标环境未配置默认模型，请显式选择目标模型".into(),
                    )
                    .into());
                }
                model = resolved_model;
                let mut spec = q
                    .workspace
                    .or(info.workspace)
                    .ok_or_else(|| anyhow::anyhow!("远端任务需要目标环境的工作区，请显式指定目标路径"))?;
                if !spec.environment_id.is_empty() && spec.environment_id != env_id {
                    return Err(Error::Api(
                        ErrorCode::Conflict,
                        "工作区不属于目标环境".into(),
                    )
                    .into());
                }
                if spec.kind.is_empty() {
                    spec.kind = if spec.repository.is_empty() {
                        "directory"
                    } else {
                        "worktree"
                    }
                    .into();
                }
                ensure!(
                    matches!(spec.kind.as_str(), "directory" | "worktree"),
                    "远端任务仅支持 directory 或 worktree 工作区"
                );
                ensure!(
                    (spec.path.is_empty() || std::path::Path::new(&spec.path).is_absolute())
                        && (spec.repository.is_empty()
                            || std::path::Path::new(&spec.repository).is_absolute()),
                    "远端工作区必须是目标机器上的绝对路径"
                );
                if q.isolation == "worktree" {
                    spec.kind = "worktree".into();
                    spec.repository = first_non_empty(&spec.repository, &spec.path).to_owned();
                    spec.path.clear();
                    spec.branch.clear();
                    if spec.base_ref.is_empty() {
                        spec.base_ref = "HEAD".into();
                    }
                }
                spec.environment_id = env_id.clone();
                Some(spec)
            } else {
                let mut ws = q.workspace.or(parent.workspace.clone());
                if q.isolation == "worktree" {
                    let spec = ws.context("workspace required")?;
                    let root = if spec.repository.is_empty() {
                        spec.path
                    } else {
                        spec.repository
                    };
                    let repo =
                        vantry_platform::git::Repository::discover(std::path::Path::new(&root))?;
                    ws = Some(WorkspaceSpec {
                        kind: "worktree".into(),
                        repository: repo.root().display().to_string(),
                        base_ref: "HEAD".into(),
                        ..Default::default()
                    });
                }
                ws
            };
            let r = core.dispatch(&Command {
                parent_run_id: q.parent_run_id,
                parent_attempt_id: q.parent_attempt_id,
                thread_id: parent.thread_id.clone(),
                title: q.title,
                message: if q.role.is_empty() {
                    q.task
                } else {
                    format!("[Role: {}]\n\n{}", q.role, q.task)
                },
                model,
                provider_id,
                approval_policy,
                interaction_mode,
                resume_policy: if q.mode == "wait" && q.resume_policy.is_empty() {
                    "manual".into()
                } else {
                    q.resume_policy
                },
                workspace: ws,
                value: payload.to_string(),
                ..command(
                    CommandKind::DelegateTask,
                    broker_id("delegate", &q.caller_thread_id, &q.client_request_id),
                )
            })?;
            let initial = core.snapshot()?;
            let task = initial
                .tasks
                .iter()
                .flatten()
                .find(|v| v.id == r.task_id)
                .ok_or_else(|| missing("task"))?;
            if q.mode == "wait" {
                let waited = wait(
                    core,
                    ThreadWaitQuery {
                        caller_thread_id: parent.thread_id.clone(),
                        thread_id: task.child_thread_id.clone(),
                        run_id: task.child_run_id.clone(),
                        timeout_ms: q.timeout_ms,
                        until_delegated_work_settled: true,
                        ..Default::default()
                    },
                )
                .await?;
                let mut s = core.snapshot()?;
                if !waited.timed_out
                    && !waited.has_pending_question
                    && waited.pending_approval.is_none()
                    && let Some(settled_task) = s
                        .tasks
                        .iter()
                        .flatten()
                        .find(|v| v.id == r.task_id)
                        .cloned()
                    && settled_task.status.terminal()
                    && settled_task.delivery_status == DeliveryStatus::Delivered
                {
                    let is_manual = s.cohorts.iter().flatten().any(|c| {
                        c.parent_run_id == settled_task.parent_run_id
                            && c.generation == settled_task.generation
                            && c.resume_policy == "manual"
                    });
                    if is_manual {
                        let _ = core.dispatch(&Command {
                            task_id: settled_task.id.clone(),
                            parent_run_id: settled_task.parent_run_id.clone(),
                            generation: settled_task.generation,
                            delivery_message_id: settled_task.delivery_message_id.clone(),
                            ..command(
                                CommandKind::DisposeTask,
                                format!(
                                    "wait-observed:{}:{}",
                                    settled_task.id, settled_task.generation
                                ),
                            )
                        });
                        s = core.snapshot()?;
                    }
                }
                let mut d = details(
                    w,
                    &s,
                    s.tasks
                        .iter()
                        .flatten()
                        .find(|v| v.id == r.task_id)
                        .ok_or_else(|| missing("task"))?,
                )?;
                d.wait_timed_out = waited.timed_out;
                if waited.timed_out && waited.auto_wake_registered {
                    d.next = "Wait timed out; auto-wake is now enabled. End your turn and Core will wake this thread when the child settles. Do not poll it.".to_owned();
                }
                return Ok(json!(d));
            }
            Ok(json!(details(w, &initial, task)?))
        }
        "tasks/status" | "task-status" => {
            let q: TaskStatusQuery = serde_json::from_value(payload)?;
            ensure!(
                !q.task_id.is_empty()
                    || !q.parent_run_id.is_empty()
                    || !q.caller_thread_id.is_empty(),
                "task or parent required"
            );
            let selected = |t: &&Task| {
                (q.task_id.is_empty() || t.id == q.task_id)
                    && (q.parent_run_id.is_empty() || t.parent_run_id == q.parent_run_id)
                    && (q.caller_thread_id.is_empty() || t.parent_thread_id == q.caller_thread_id)
            };
            if q.acknowledge {
                for t in s.tasks.iter().flatten().filter(selected).filter(|t| {
                    t.status.terminal() && t.delivery_status == DeliveryStatus::Delivered
                }) {
                    core.dispatch(&Command {
                        parent_run_id: t.parent_run_id.clone(),
                        generation: t.generation,
                        delivery_message_id: t.delivery_message_id.clone(),
                        ..command(
                            CommandKind::AcknowledgeTask,
                            format!("observe-{}-{}", t.parent_run_id, t.generation),
                        )
                    })?;
                }
            }
            let latest = core.snapshot()?;
            let tasks: Vec<_> = latest
                .tasks
                .iter()
                .flatten()
                .filter(selected)
                .map(|t| details(w, &latest, t))
                .collect::<Result<_>>()?;
            if tasks.is_empty() && !q.task_id.is_empty() {
                return Err(missing("task").into());
            }
            let cohorts = latest
                .cohorts
                .unwrap_or_default()
                .into_iter()
                .filter(|c| tasks.iter().any(|t| t.parent_run_id == c.parent_run_id))
                .collect();
            Ok(json!(TaskStatusResult {
                thread_id: q.caller_thread_id,
                run_id: q.parent_run_id,
                tasks: Some(tasks),
                cohorts
            }))
        }
        "tasks/cancel" | "task-cancel" => {
            let q: CancelTaskRequest = serde_json::from_value(payload)?;
            core.dispatch(&Command {
                thread_id: q.caller_thread_id,
                task_id: q.task_id.clone(),
                reason: q.reason,
                ..command(CommandKind::CancelTask, q.client_request_id)
            })?;
            let latest = core.snapshot()?;
            let t = latest
                .tasks
                .iter()
                .flatten()
                .find(|t| t.id == q.task_id)
                .ok_or_else(|| missing("task"))?;
            Ok(json!({"taskId":t.id,"status":t.status}))
        }
        "threads/search" => {
            let q: SearchQuery = serde_json::from_value(payload)?;
            ensure!(!q.query.trim().is_empty() && q.limit >= 0, "invalid search");
            let mut hits = vec![];
            let needle = q.query.to_lowercase();
            for t in s.threads.iter().flatten().filter(|t| {
                (q.include_archived || !t.archived)
                    && (q.thread_id.is_empty() || t.id == q.thread_id)
            }) {
                let mut hit = |text: &str, field: SearchField, run: &str, at: &str| {
                    if text.to_lowercase().contains(&needle) {
                        hits.push(SearchHit {
                            thread_id: t.id.clone(),
                            title: t.title.clone(),
                            run_id: run.into(),
                            field,
                            snippet: text.chars().take(240).collect(),
                            score: if matches!(field, SearchField::Title) {
                                10
                            } else {
                                1
                            },
                            updated_at: at.into(),
                        });
                    }
                };
                hit(&t.title, SearchField::Title, "", &t.created_at);
                if let Some(ws) = &t.workspace {
                    hit(&ws.path, SearchField::Workspace, "", &t.created_at);
                    hit(&ws.branch, SearchField::Branch, "", &t.created_at);
                }
                for r in runs(&s, &t.id) {
                    hit(&r.input, SearchField::Input, &r.id, &r.created_at);
                    hit(&r.output, SearchField::Output, &r.id, &r.updated_at);
                }
            }
            hits.sort_by(|a, b| (b.score, &b.updated_at).cmp(&(a.score, &a.updated_at)));
            hits.truncate(bound(q.limit, 30, 100));
            Ok(json!({"hits":hits}))
        }
        _ => Err(missing("orchestration endpoint").into()),
    }
}
