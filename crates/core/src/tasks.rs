use crate::{
    commands::{attempt, cancel, ceiling, create_run, dequeue, insert_thread, receipt},
    store::projection::*,
    *,
};
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::json;
use vantry_protocol::*;
pub(crate) mod watches;

pub const DELEGATED_WAKE_PREFIX: &str = "父任务等待期间，Vantry 收到并已接受以下子任务结果：";
fn task_event(db: &Connection, key: &str, kind: &str) -> Result<()> {
    let t = task(db, key)?;
    event(
        db,
        kind,
        &t.parent_thread_id,
        &t.parent_run_id,
        &db.query_row(
            "SELECT parent_attempt_id FROM tasks WHERE id=?",
            [key],
            |r| r.get::<_, String>(0),
        )?,
        json!({"task":t}),
    )?;
    Ok(())
}
fn group_event(db: &Connection, parent: &Run, g: i64, kind: &str) -> Result<()> {
    event(
        db,
        kind,
        &parent.thread_id,
        &parent.id,
        &parent.attempt_id,
        json!({"cohort":cohort(db,&parent.id,g)?}),
    )?;
    Ok(())
}
fn child_workspace(w: &WorkspaceSpec) -> WorkspaceSpec {
    if w.kind == "worktree" {
        return WorkspaceSpec {
            kind: w.kind.clone(),
            repository: w.repository.clone(),
            environment_id: w.environment_id.clone(),
            base_ref: first(&w.branch, &w.base_ref).into(),
            ..Default::default()
        };
    }
    if w.kind == "directory" && w.environment_id.is_empty() {
        if let Ok(path) = std::fs::canonicalize(&w.path) {
            if let Some(root) = path.ancestors().find(|p| p.join(".git").exists()) {
                return WorkspaceSpec {
                    kind: "worktree".into(),
                    repository: root.to_string_lossy().into(),
                    ..Default::default()
                };
            }
        }
    }
    w.clone()
}
fn open_group(db: &Connection, parent: &Run, policy: &str, inherited: &str) -> Result<Cohort> {
    if !matches!(policy, "auto" | "manual" | "") {
        return Err(invalid("unsupported resume policy"));
    }
    let effective = first(policy, inherited);
    let old: Option<i64> = db
        .query_row(
            "SELECT generation FROM cohorts WHERE parent_run_id=? AND resume_policy=? AND
             status='pending' AND continuation_attempt_id='' ORDER BY generation DESC LIMIT 1",
            params![&parent.id, effective],
            |r| r.get(0),
        )
        .optional()?;
    let g = if let Some(g) = old {
        g
    } else {
        let g: i64 = db.query_row(
            "SELECT COALESCE(MAX(generation),0)+1 FROM cohorts WHERE parent_run_id=?",
            [&parent.id],
            |r| r.get(0),
        )?;
        db.execute(
            "INSERT INTO cohorts(parent_run_id,generation,resume_policy,delivery_message_id,status,
             created_at,updated_at) VALUES(?,?,?,?,'pending',?,?)",
            params![
                parent.id,
                g,
                effective,
                id("msg"),
                now(),
                now()
            ],
        )?;
        group_event(db, parent, g, "cohort.created")?;
        g
    };
    cohort(db, &parent.id, g)
}
pub(crate) fn command(db: &Connection, c: &Command) -> Result<Receipt> {
    use CommandKind::*;
    let prev = if !c.task_id.is_empty() {
        Some(task(db, &c.task_id)?)
    } else {
        None
    };
    let parent_id = prev
        .as_ref()
        .map_or(c.parent_run_id.as_str(), |t| &t.parent_run_id);
    if parent_id.is_empty() {
        return Err(invalid("parent run or task is required"));
    }
    let parent = run(db, parent_id)?;
    if (!c.thread_id.is_empty() && c.thread_id != parent.thread_id)
        || (!c.parent_run_id.is_empty() && c.parent_run_id != parent.id)
        || (!c.parent_attempt_id.is_empty() && c.parent_attempt_id != parent.attempt_id)
    {
        return Err(conflict("parent identity mismatch"));
    }
    if let Some(t) = &prev {
        if c.generation != 0 && c.generation != t.generation {
            return Err(conflict("task generation mismatch"));
        }
    }
    if matches!(c.kind, DelegateTask | FollowUpTask) {
        if c.message.trim().is_empty() {
            return Err(invalid("task message required"));
        }
        if parent.status.terminal() {
            let latest: String = db.query_row(
                "SELECT id FROM runs WHERE thread_id=? ORDER BY created_at DESC,id DESC LIMIT 1",
                [&parent.thread_id],
                |r| r.get(0),
            )?;
            if parent.status != RunStatus::Completed
                || (c.kind == DelegateTask && latest != parent.id)
            {
                return Err(conflict("only the latest completed parent can resume"));
            }
            if latest == parent.id && commands::active(db, &parent.thread_id)?.is_none() {
                db.execute(
                    "UPDATE runs SET status='waiting_children',updated_at=? WHERE id=?",
                    params![now(), parent.id],
                )?;
                run_event(db, &parent.id, "run.waiting_children")?;
            }
        }
        let parent_thread = thread(db, &parent.thread_id)?;
        let mut inherited = "auto".to_string();
        let (task_id, child) = if c.kind == FollowUpTask {
            let t = prev.as_ref().ok_or_else(|| invalid("taskId required"))?;
            let g = cohort(db, parent_id, t.generation)?;
            if !matches!(
                t.delivery_status,
                DeliveryStatus::Acknowledged | DeliveryStatus::Disposed
            ) || !matches!(
                g.status,
                DeliveryStatus::Acknowledged | DeliveryStatus::Disposed
            ) || g.status == DeliveryStatus::Acknowledged && g.continuation_attempt_id.is_empty()
            {
                return Err(conflict("previous delivery not consumed"));
            }
            inherited = g.resume_policy;
            (t.id.clone(), thread(db, &t.child_thread_id)?)
        } else {
            let task_id = id("task");
            let provider = first(&c.provider_id, &parent_thread.provider_id);
            let model = if provider == parent_thread.provider_id {
                first(&c.model, &parent_thread.model)
            } else {
                &c.model
            };
            let child = Thread {
                id: id("th"),
                title: if c.title.trim().is_empty() {
                    let text = c.message.replace('\n', " ");
                    let mut chars = text.trim().chars();
                    let mut title: String = chars.by_ref().take(60).collect();
                    if chars.next().is_some() {
                        title.push('…');
                    }
                    title
                } else {
                    c.title.clone()
                },
                parent_thread_id: parent.thread_id.clone(),
                parent_task_id: task_id.clone(),
                created_by_thread_id: parent.thread_id.clone(),
                created_by_run_id: parent.id.clone(),
                provider_id: provider.into(),
                model: model.into(),
                workspace: c
                    .workspace
                    .clone()
                    .or_else(|| parent_thread.workspace.as_ref().map(child_workspace)),
                interaction_mode: first(&c.interaction_mode, &parent_thread.interaction_mode)
                    .into(),
                approval_policy: first(&c.approval_policy, &parent_thread.approval_policy).into(),
                ..Default::default()
            };
            if !c.thread_id.is_empty() {
                ceiling(&parent_thread, &child)?;
            }
            (task_id, child)
        };
        let g = open_group(db, &parent, &c.resume_policy, &inherited)?;
        if c.kind == DelegateTask {
            insert_thread(db, &child)?;
        }
        let r = create_run(db, &child, &c.message, "", &[])?;
        let seq = sequence(db)?;
        if c.kind == DelegateTask {
            db.execute(
                "INSERT INTO tasks(id,parent_thread_id,parent_run_id,parent_attempt_id,child_thread_id,
             child_run_id,title,generation,status,delivery_status,delivery_message_id,created_at,
             updated_at) VALUES(?,?,?,?,?,?,?,?,'queued','pending',?,?,?)",
                params![task_id, parent.thread_id, parent.id, parent.attempt_id, child.id, r.id, child.title, g.generation, g.delivery_message_id, now(), now()],
            )?;
            task_event(db, &task_id, "task.created")?;
        } else {
            db.execute(
                "UPDATE tasks SET child_run_id=?,parent_attempt_id=?,generation=?,status='queued',
             delivery_status='pending',delivery_message_id=?,result='',error='',updated_at=? WHERE
             id=?",
                params![
                    r.id,
                    parent.attempt_id,
                    g.generation,
                    g.delivery_message_id,
                    now(),
                    task_id
                ],
            )?;
            task_event(db, &task_id, "task.updated")?;
        }
        return Ok(Receipt {
            task_id,
            generation: g.generation,
            delivery_message_id: g.delivery_message_id,
            sequence: seq,
            ..receipt(&child.id, &r.id)
        });
    }
    let g = if let Some(t) = &prev {
        t.generation
    } else if c.generation == 0 && c.kind == UpgradeTaskWake {
        db.query_row(
            "SELECT MAX(generation) FROM cohorts WHERE parent_run_id=?",
            [parent_id],
            |r| r.get(0),
        )?
    } else {
        c.generation
    };
    let group = cohort(db, parent_id, g)?;
    if matches!(c.kind, AcknowledgeTask | DisposeTask)
        && (c.generation <= 0 || group.delivery_message_id != c.delivery_message_id)
    {
        return Err(conflict("delivery identity mismatch"));
    }
    match c.kind {
        CancelTask => {
            let t = prev.as_ref().ok_or_else(|| invalid("taskId required"))?;
            cancel_task(db, t)?;
        }
        UpgradeTaskWake => {
            if db.execute(
                "UPDATE cohorts SET resume_policy='auto',updated_at=? WHERE parent_run_id=? AND
             generation=? AND resume_policy='manual' AND status IN ('pending','delivered')",
                params![now(), parent.id, g],
            )? > 0
            {
                group_event(db, &parent, g, "cohort.updated")?;
            }
        }
        AcknowledgeTask => {
            sync(db, &parent, g)?;
            let group = cohort(db, parent_id, g)?;
            if matches!(
                group.status,
                DeliveryStatus::Pending | DeliveryStatus::Disposed
            ) {
                return Err(conflict("delivery is not available"));
            }
            ack(db, &parent, g)?;
        }
        DisposeTask => {
            let t = prev.as_ref().ok_or_else(|| invalid("taskId required"))?;
            if !t.status.terminal()
                || t.delivery_status == DeliveryStatus::Acknowledged
                    && !group.continuation_attempt_id.is_empty()
            {
                return Err(conflict("result cannot be disposed"));
            }
            if t.delivery_status != DeliveryStatus::Disposed {
                db.execute(
                    "UPDATE tasks SET delivery_status='disposed',updated_at=? WHERE id=?",
                    params![now(), t.id],
                )?;
                task_event(db, &t.id, "task.updated")?;
            }
        }
        _ => unreachable!(),
    };
    sync(db, &parent, g)?;
    Ok(Receipt {
        task_id: c.task_id.clone(),
        generation: g,
        delivery_message_id: group.delivery_message_id,
        ..receipt(&parent.thread_id, parent_id)
    })
}
pub(crate) fn dispose_parent_deliveries(db: &Connection, p: &Run) -> Result<()> {
    for key in strings(
        db,
        "SELECT id FROM tasks WHERE parent_run_id=? AND delivery_status<>'disposed'",
        [&p.id],
    )? {
        db.execute(
            "UPDATE tasks SET delivery_status='disposed',updated_at=? WHERE id=?",
            params![now(), key],
        )?;
        task_event(db, &key, "task.updated")?;
    }
    for g in db
        .prepare("SELECT generation FROM cohorts WHERE parent_run_id=? AND status<>'disposed'")?
        .query_map([&p.id], |r| r.get::<_, i64>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?
    {
        update_group(db, p, g, "disposed")?;
    }
    Ok(())
}

pub(crate) fn cancel_task(db: &Connection, t: &Task) -> Result<()> {
    if t.delivery_status != DeliveryStatus::Disposed || !t.status.terminal() {
        db.execute(
            "UPDATE tasks SET delivery_status='disposed',status=CASE WHEN status IN ('queued',
             'running','waiting_children') THEN 'cancelled' ELSE status END,updated_at=? WHERE id=?",
            params![now(), t.id],
        )?;
        task_event(db, &t.id, "task.updated")?;
    }
    commands::cancel_queue(db, &t.child_thread_id)?;
    if let Some(active) = commands::active(db, &t.child_thread_id)? {
        cancel(db, &run(db, &active)?)?;
    }
    sync(db, &run(db, &t.parent_run_id)?, t.generation)
}
fn update_group(db: &Connection, p: &Run, g: i64, status: &str) -> Result<()> {
    if db.execute(
        "UPDATE cohorts SET status=?,updated_at=? WHERE parent_run_id=? AND generation=? AND
             status<>?",
        params![status, now(), p.id, g, status],
    )? > 0
    {
        group_event(db, p, g, "cohort.updated")?;
    }
    Ok(())
}
fn ack(db: &Connection, p: &Run, g: i64) -> Result<()> {
    for t in strings(
        db,
        "SELECT id FROM tasks WHERE parent_run_id=? AND generation=? AND delivery_status='delivered'",
        params![p.id, g],
    )? {
        db.execute(
            "UPDATE tasks SET delivery_status='acknowledged',updated_at=? WHERE id=?",
            params![now(), t],
        )?;
        task_event(db, &t, "task.updated")?;
    }
    update_group(db, p, g, "acknowledged")?;
    continuation(db, &p.id)
}
fn child_still_working(db: &Connection, child_thread_id: &str, r: &Run) -> Result<bool> {
    if !r.status.terminal() {
        return Ok(true);
    }
    if r.status == RunStatus::Cancelled {
        return Ok(false);
    }
    if has_active_for_thread(db, child_thread_id)? {
        return Ok(true);
    }
    Ok(commands::active(db, child_thread_id)?.is_some())
}
pub(crate) fn sync(db: &Connection, p: &Run, g: i64) -> Result<()> {
    let tasks: Vec<Task> = rows(
        db,
        "SELECT * FROM tasks WHERE parent_run_id=? AND generation=? ORDER BY created_at,id",
        params![p.id, g],
    )?;
    if tasks.is_empty() {
        return Ok(());
    }
    let mut remaining = 0;
    let mut live_ids: Vec<String> = Vec::new();
    let mut ready_ids: Vec<String> = Vec::new();
    for t in &tasks {
        if t.delivery_status == DeliveryStatus::Disposed {
            continue;
        }
        let r = match run(db, &t.child_run_id) {
            Ok(r) => r,
            Err(e) if e.code() == ErrorCode::NotFound => {
                remaining += 1;
                if !t.status.terminal() {
                    live_ids.push(t.id.clone());
                } else if t.delivery_status == DeliveryStatus::Pending {
                    ready_ids.push(t.id.clone());
                }
                continue;
            }
            Err(e) => return Err(e),
        };
        let working = child_still_working(db, &t.child_thread_id, &r)?;
        let effective_status = if working && r.status.terminal() {
            RunStatus::Waiting
        } else {
            r.status
        };
        let (status, result, error) = (
            serde_json::to_value(effective_status)?,
            if !working {
                &r.output
            } else {
                &t.result
            },
            if !working {
                &r.error
            } else {
                &t.error
            },
        );
        if t.status != effective_status || result != &t.result || error != &t.error {
            db.execute(
                "UPDATE tasks SET status=?,result=?,error=?,updated_at=? WHERE id=?",
                params![status.as_str(), result, error, now(), t.id],
            )?;
            task_event(db, &t.id, "task.updated")?;
        }
        if working {
            live_ids.push(t.id.clone());
        } else if t.delivery_status == DeliveryStatus::Pending {
            ready_ids.push(t.id.clone());
        }
        remaining += 1;
    }
    if remaining == 0 {
        update_group(db, p, g, "disposed")?;
        if p.status == RunStatus::Waiting && !has_open(db, &p.id)? {
            db.execute("UPDATE runs SET status='completed',updated_at=? WHERE id=? AND status='waiting_children'", params![now(), p.id])?;
            run_event(db, &p.id, "run.completed")?;
            watches::deliver(db, &run(db, &p.id)?)?;
            sync_child(db, &p.id)?;
            deliver_pending_for_thread(db, &p.thread_id)?;
            dequeue(db, &p.thread_id)?;
        }
        return Ok(());
    }
    let group = cohort(db, &p.id, g)?;
    if group.status == DeliveryStatus::Disposed {
        return Ok(());
    }
    if !live_ids.is_empty() {
        // Align with T3 Code's incremental delegated completion delivery:
        // for auto-resume cohorts, do NOT block completed children behind still-running
        // siblings. Move live siblings to a fresh pending generation and deliver ready
        // terminal children immediately (coalescing happens automatically if a wake is queued).
        if group.resume_policy == "auto"
            && group.status == DeliveryStatus::Pending
            && !ready_ids.is_empty()
        {
            let next_gen: i64 = db.query_row(
                "SELECT COALESCE(MAX(generation),0)+1 FROM cohorts WHERE parent_run_id=?",
                [&p.id],
                |r| r.get(0),
            )?;
            let next_msg = id("msg");
            let ts = now();
            db.execute(
                "INSERT INTO cohorts(parent_run_id,generation,resume_policy,delivery_message_id,status,
                 created_at,updated_at) VALUES(?,?,'auto',?,'pending',?,?)",
                params![p.id, next_gen, next_msg, ts, ts],
            )?;
            group_event(db, p, next_gen, "cohort.created")?;
            for live_id in &live_ids {
                db.execute(
                    "UPDATE tasks SET generation=?,delivery_message_id=?,updated_at=? WHERE id=?",
                    params![next_gen, next_msg, now(), live_id],
                )?;
                task_event(db, live_id, "task.updated")?;
            }
        } else {
            return Ok(());
        }
    }
    if group.status == DeliveryStatus::Acknowledged {
        return continuation(db, &p.id);
    }
    for t in &tasks {
        if t.delivery_status == DeliveryStatus::Pending && !live_ids.iter().any(|id| id == &t.id) {
            db.execute(
                "UPDATE tasks SET delivery_status='delivered',updated_at=? WHERE id=?",
                params![now(), t.id],
            )?;
            task_event(db, &t.id, "task.updated")?;
        }
    }
    if group.resume_policy == "auto" {
        ack(db, p, g)
    } else {
        update_group(db, p, g, "delivered")
    }
}
pub(crate) fn sync_child(db: &Connection, child: &str) -> Result<()> {
    let mut synced_parent: Option<(String, i64)> = None;
    let group = db
        .query_row(
            "SELECT parent_run_id,generation FROM tasks WHERE child_run_id=?",
            [child],
            |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)?)),
        )
        .optional()?;
    if let Some((p, g)) = group {
        synced_parent = Some((p.clone(), g));
        sync(db, &run(db, &p)?, g)?;
    }
    // If a delegated child had a wake run (e.g. after its own grandchild finished),
    // promote that settled run as the task's latest child_run_id and notify the parent.
    if let Ok(r) = run(db, child)
        && let Ok(t) = thread(db, &r.thread_id)
        && !t.parent_task_id.is_empty()
        && let Ok(parent_task) = task(db, &t.parent_task_id)
        && parent_task.delivery_status == DeliveryStatus::Pending
        && parent_task.child_run_id != r.id
    {
        db.execute(
            "UPDATE tasks SET child_run_id=?,updated_at=? WHERE id=?",
            params![r.id, now(), parent_task.id],
        )?;
        task_event(db, &parent_task.id, "task.updated")?;
        if synced_parent.as_ref() != Some(&(parent_task.parent_run_id.clone(), parent_task.generation))
        {
            sync(db, &run(db, &parent_task.parent_run_id)?, parent_task.generation)?;
        }
    }
    Ok(())
}
pub(crate) fn has_open(db: &Connection, parent: &str) -> Result<bool> {
    Ok(db.query_row(
        "SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_run_id=? AND delivery_status IN ('pending',
             'delivered'))",
        [parent],
        |r| r.get(0),
    )?)
}
pub(crate) fn has_active_for_thread(db: &Connection, thread_id: &str) -> Result<bool> {
    Ok(db.query_row(
        "SELECT EXISTS(
            SELECT 1 FROM tasks t
            WHERE t.parent_thread_id=?1
              AND (
                t.status IN ('queued','running','waiting_children')
                OR t.delivery_status IN ('pending','delivered')
                OR EXISTS(
                    SELECT 1 FROM cohorts c
                    WHERE c.parent_run_id=t.parent_run_id
                      AND c.generation=t.generation
                      AND c.status='acknowledged'
                      AND c.continuation_attempt_id=''
                )
              )
        )",
        [thread_id],
        |r| r.get(0),
    )?)
}
fn build_continuation_prompt(db: &Connection, parent_thread_id: &str, tasks: &[Task]) -> Result<String> {
    let mut prompt = format!("{DELEGATED_WAKE_PREFIX}\n");
    for t in tasks {
        let title = first(t.title.trim(), &t.child_thread_id)
            .replace('[', "(")
            .replace(']', ")")
            .replace(['\n', '\r'], " ");
        let link = if t.child_thread_id.is_empty() {
            String::new()
        } else {
            format!(
                "- 会话链接：[{title}](vantry-thread://{})\n",
                t.child_thread_id
            )
        };
        let result = if !t.result.trim().is_empty() {
            t.result.clone()
        } else if !t.error.trim().is_empty() {
            format!("（无输出）错误：{}", t.error)
        } else {
            "（无输出）".into()
        };
        let chars: Vec<char> = result.chars().collect();
        let result = if chars.len() <= 4000 {
            result
        } else {
            format!(
                "{}\n... (已省略 {} 字符；需要完整内容请调用 vantry_thread_read) ...\n{}",
                chars[..2400].iter().collect::<String>(),
                chars.len() - 4000,
                chars[chars.len() - 1600..].iter().collect::<String>()
            )
        };
        prompt.push_str(&format!(
            "\n### 子任务 {}\n- 任务 ID：{}\n{link}- generation：{}\n- 终态：{}\n\n结果：\n{result}\n",
            t.title,
            t.id,
            t.generation,
            serde_json::to_value(t.status)?.as_str().unwrap_or("")
        ));
    }
    let running_siblings: Vec<Task> = rows(
        db,
        "SELECT * FROM tasks WHERE parent_thread_id=? AND delivery_status='pending'
         AND status IN ('queued','running','waiting_children') ORDER BY created_at, id",
        [parent_thread_id],
    )?;
    if !running_siblings.is_empty() {
        prompt.push_str(&format!(
            "\n---\n注：当前仍有 {} 个并行子任务正在运行：",
            running_siblings.len()
        ));
        for (i, sib) in running_siblings.iter().enumerate() {
            if i > 0 {
                prompt.push_str("、");
            }
            let title = first(sib.title.trim(), &sib.child_thread_id)
                .replace('[', "(")
                .replace(']', ")")
                .replace(['\n', '\r'], " ");
            if sib.child_thread_id.is_empty() {
                prompt.push_str(&title);
            } else {
                prompt.push_str(&format!("[{title}](vantry-thread://{})", sib.child_thread_id));
            }
        }
        prompt.push_str("。它们完成后 Vantry Core 会再次自动唤醒本会话，无需轮询或重复派发。\n");
    }
    Ok(prompt)
}
fn try_coalesce_queued_wake(
    db: &Connection,
    p: &Run,
    active_run: &Run,
    groups: &[Cohort],
) -> Result<bool> {
    let att = &active_run.attempt_id;
    if att.is_empty() {
        return Ok(false);
    }
    let is_unclaimed_wake: bool = db.query_row(
        "SELECT EXISTS(SELECT 1 FROM cohorts WHERE continuation_attempt_id=?1)
         AND EXISTS(SELECT 1 FROM effect_outbox WHERE attempt_id=?1 AND kind='provider.run'
                    AND status='pending' AND attempt_count=0)",
        [att],
        |r| r.get(0),
    )?;
    if !is_unclaimed_wake {
        return Ok(false);
    }
    for g in groups {
        db.execute(
            "UPDATE cohorts SET continuation_attempt_id=?,updated_at=? WHERE parent_run_id=? AND
             generation=?",
            params![att, now(), p.id, g.generation],
        )?;
        group_event(db, p, g.generation, "cohort.updated")?;
    }
    let all_tasks: Vec<Task> = rows(
        db,
        "SELECT t.* FROM tasks t JOIN cohorts c USING(parent_run_id,generation)
         WHERE c.continuation_attempt_id=? AND t.delivery_status='acknowledged'
         ORDER BY t.created_at, t.generation, t.id",
        [att],
    )?;
    let prompt = build_continuation_prompt(db, &p.thread_id, &all_tasks)?;
    db.execute(
        "UPDATE attempts SET input=?,updated_at=? WHERE id=?",
        params![prompt, now(), att],
    )?;
    db.execute(
        "UPDATE effect_outbox SET input=?,updated_at=? WHERE attempt_id=? AND kind='provider.run' AND
         status='pending'",
        params![prompt, now(), att],
    )?;
    if active_run.id != p.id && active_run.input.starts_with(DELEGATED_WAKE_PREFIX) {
        db.execute(
            "UPDATE runs SET input=?,updated_at=? WHERE id=?",
            params![prompt, now(), active_run.id],
        )?;
        run_event(db, &active_run.id, "run.queued")?;
    }
    Ok(true)
}
pub(crate) fn continuation(db: &Connection, parent: &str) -> Result<()> {
    let p = run(db, parent)?;
    let parent_thread = match thread(db, &p.thread_id) {
        Ok(t) if !t.archived => t,
        Ok(_) => return Ok(()),
        Err(e) if e.code() == ErrorCode::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };
    let groups: Vec<Cohort> = rows(
        db,
        "SELECT * FROM cohorts WHERE parent_run_id=? AND status='acknowledged' AND
             continuation_attempt_id='' ORDER BY generation",
        [parent],
    )?;
    if groups.is_empty() {
        return Ok(());
    }
    let tasks: Vec<Task> = rows(
        db,
        "SELECT t.* FROM tasks t JOIN cohorts c USING(parent_run_id,generation) WHERE
             t.parent_run_id=? AND c.status='acknowledged' AND c.continuation_attempt_id='' AND
             t.delivery_status='acknowledged' ORDER BY t.generation,t.created_at,t.id",
        [parent],
    )?;
    if tasks.is_empty() {
        return Ok(());
    }
    if let Some(active_id) = commands::active(db, &p.thread_id)? {
        let active_run = run(db, &active_id)?;
        match active_run.status {
            RunStatus::Running => return Ok(()),
            RunStatus::Queued => {
                let _ = try_coalesce_queued_wake(db, &p, &active_run, &groups)?;
                return Ok(());
            }
            RunStatus::Waiting => {
                if active_run.id != p.id {
                    commands::settle_waiting_run(db, &active_run.id)?;
                }
            }
            _ => {}
        }
    }
    let p = run(db, parent)?;
    let latest: Option<String> = db
        .query_row(
            "SELECT id FROM runs WHERE thread_id=? ORDER BY created_at DESC,id DESC LIMIT 1",
            [&p.thread_id],
            |r| r.get(0),
        )
        .optional()?;
    let prompt = build_continuation_prompt(db, &p.thread_id, &tasks)?;
    let can_continue_in_place = latest.as_deref() == Some(p.id.as_str())
        && matches!(p.status, RunStatus::Completed | RunStatus::Waiting);
    if can_continue_in_place {
        let att = id("att");
        let sep = if p.output.is_empty() || p.output.ends_with("\n\n") {
            ""
        } else if p.output.ends_with('\n') {
            "\n"
        } else {
            "\n\n"
        };
        attempt(db, &p.thread_id, parent, &att, &prompt)?;
        crate::store::blocks::clear_status(db, parent)?;
        crate::store::blocks::close_streaming(db, parent)?;
        db.execute(
            "UPDATE runs SET attempt_id=?,active_attempt_id=?,status='queued',error='',updated_at=?,
                 output=output||? WHERE id=?",
            params![att, att, now(), sep, parent],
        )?;
        if !sep.is_empty() {
            event(
                db,
                "run.output",
                &p.thread_id,
                parent,
                &att,
                json!({"delta":sep}),
            )?;
        }
        for g in &groups {
            db.execute(
                "UPDATE cohorts SET continuation_attempt_id=?,updated_at=? WHERE parent_run_id=? AND
                     generation=?",
                params![att, now(), parent, g.generation],
            )?;
            group_event(db, &p, g.generation, "cohort.updated")?;
        }
        run_event(db, parent, "run.queued")?;
    } else {
        if p.status == RunStatus::Waiting {
            commands::settle_waiting_run(db, &p.id)?;
        }
        let wake = create_run(db, &parent_thread, &prompt, "", &[])?;
        for g in &groups {
            db.execute(
                "UPDATE cohorts SET continuation_attempt_id=?,updated_at=? WHERE parent_run_id=? AND
                     generation=?",
                params![wake.attempt_id, now(), parent, g.generation],
            )?;
            group_event(db, &p, g.generation, "cohort.updated")?;
        }
    }
    Ok(())
}
pub(crate) fn deliver_pending_for_thread(db: &Connection, thread_id: &str) -> Result<()> {
    let pending_groups: Vec<(String, i64)> = db
        .prepare(
            "SELECT c.parent_run_id, c.generation
             FROM cohorts c
             JOIN runs r ON r.id = c.parent_run_id
             WHERE r.thread_id=?
               AND (c.status IN ('pending','delivered')
                    OR (c.status='acknowledged' AND c.continuation_attempt_id=''))
             ORDER BY c.created_at, c.generation",
        )?
        .query_map([thread_id], |r| Ok((r.get(0)?, r.get(1)?)))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for (parent_run_id, generation) in pending_groups {
        if let Ok(p) = run(db, &parent_run_id) {
            sync(db, &p, generation)?;
        }
    }
    Ok(())
}
pub(crate) fn has_sweepable(db: &Connection) -> Result<bool> {
    Ok(db.query_row(
        "SELECT EXISTS(
            SELECT 1
            FROM cohorts c
            JOIN runs pr ON pr.id = c.parent_run_id
            JOIN threads pt ON pt.id = pr.thread_id
            WHERE pt.archived = 0
              AND (
                (c.status='acknowledged' AND c.continuation_attempt_id=''
                 AND NOT EXISTS(
                     SELECT 1 FROM runs ar
                     WHERE ar.thread_id = pr.thread_id
                       AND ar.status IN ('queued','running')
                 ))
                OR (c.status='pending' AND c.resume_policy='auto'
                    AND EXISTS(
                        SELECT 1 FROM tasks t
                        JOIN runs cr ON cr.id = t.child_run_id
                        WHERE t.parent_run_id = c.parent_run_id
                          AND t.generation = c.generation
                          AND t.delivery_status = 'pending'
                          AND cr.status IN ('completed','failed','cancelled','interrupted')
                    ))
              )
        )",
        [],
        |r| r.get(0),
    )?)
}
pub(crate) fn sweep_ready(db: &Connection) -> Result<()> {
    let candidates: Vec<(String, i64)> = db
        .prepare(
            "SELECT DISTINCT c.parent_run_id, c.generation
             FROM cohorts c
             JOIN runs pr ON pr.id = c.parent_run_id
             JOIN threads pt ON pt.id = pr.thread_id
             WHERE pt.archived = 0
               AND (
                 (c.status='acknowledged' AND c.continuation_attempt_id=''
                  AND NOT EXISTS(
                      SELECT 1 FROM runs ar
                      WHERE ar.thread_id = pr.thread_id
                        AND ar.status IN ('queued','running')
                  ))
                 OR (c.status='pending' AND c.resume_policy='auto'
                     AND EXISTS(
                         SELECT 1 FROM tasks t
                         JOIN runs cr ON cr.id = t.child_run_id
                         WHERE t.parent_run_id = c.parent_run_id
                           AND t.generation = c.generation
                           AND t.delivery_status = 'pending'
                           AND cr.status IN ('completed','failed','cancelled','interrupted')
                     ))
               )
             ORDER BY c.created_at, c.generation",
        )?
        .query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for (parent_run_id, generation) in candidates {
        if let Ok(p) = run(db, &parent_run_id) {
            sync(db, &p, generation)?;
        }
    }
    Ok(())
}
