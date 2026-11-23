use crate::{
    commands::{attempt, cancel, ceiling, create_run, dequeue, insert_thread, receipt},
    store::projection::*,
    *,
};
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::json;
use vantry_protocol::*;
pub(crate) mod watches;
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
    let old: Option<i64> = db
        .query_row(
            "SELECT generation FROM cohorts WHERE parent_run_id=? AND status IN ('pending',
             'delivered') ORDER BY generation DESC LIMIT 1",
            [&parent.id],
            |r| r.get(0),
        )
        .optional()?;
    let g = if let Some(g) = old {
        let group = cohort(db, &parent.id, g)?;
        if !policy.is_empty() && group.resume_policy != policy {
            return Err(conflict("existing generation has different resume policy"));
        }
        if group.status == DeliveryStatus::Delivered {
            db.execute("UPDATE cohorts SET status='pending',updated_at=? WHERE parent_run_id=? AND generation=?", params![now(), parent.id, g])?;
            group_event(db, parent, g, "cohort.updated")?;
        }
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
                first(policy, inherited),
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
            if parent.status != RunStatus::Completed || latest != parent.id {
                return Err(conflict("only the latest completed parent can resume"));
            }
            db.execute(
                "UPDATE runs SET status='waiting_children',updated_at=? WHERE id=?",
                params![now(), parent.id],
            )?;
            run_event(db, &parent.id, "run.waiting_children")?;
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
pub(crate) fn cancel_task(db: &Connection, t: &Task) -> Result<()> {
    if t.delivery_status != DeliveryStatus::Disposed {
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
    let mut live = false;
    for t in &tasks {
        if t.delivery_status == DeliveryStatus::Disposed {
            continue;
        }
        let r = match run(db, &t.child_run_id) {
            Ok(r) => r,
            Err(e) if e.code() == ErrorCode::NotFound => {
                remaining += 1;
                live |= !t.status.terminal();
                continue;
            }
            Err(e) => return Err(e),
        };
        let (status, result, error) = (
            serde_json::to_value(r.status)?,
            if r.status.terminal() {
                &r.output
            } else {
                &t.result
            },
            if r.status.terminal() {
                &r.error
            } else {
                &t.error
            },
        );
        if t.status != r.status || result != &t.result || error != &t.error {
            db.execute(
                "UPDATE tasks SET status=?,result=?,error=?,updated_at=? WHERE id=?",
                params![status.as_str(), result, error, now(), t.id],
            )?;
            task_event(db, &t.id, "task.updated")?;
        }
        live |= !r.status.terminal();
        remaining += 1;
    }
    if live {
        return Ok(());
    }
    if remaining == 0 {
        update_group(db, p, g, "disposed")?;
        if p.status == RunStatus::Waiting && !has_open(db, &p.id)? {
            db.execute("UPDATE runs SET status='completed',updated_at=? WHERE id=? AND status='waiting_children'", params![now(), p.id])?;
            run_event(db, &p.id, "run.completed")?;
            watches::deliver(db, &run(db, &p.id)?)?;
            sync_child(db, &p.id)?;
            dequeue(db, &p.thread_id)?;
        }
        return Ok(());
    }
    let group = cohort(db, &p.id, g)?;
    if group.status == DeliveryStatus::Acknowledged {
        return continuation(db, &p.id);
    }
    if group.status == DeliveryStatus::Disposed {
        return Ok(());
    }
    for t in tasks {
        if t.delivery_status == DeliveryStatus::Pending {
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
    let group = db
        .query_row(
            "SELECT parent_run_id,generation FROM tasks WHERE child_run_id=?",
            [child],
            |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)?)),
        )
        .optional()?;
    if let Some((p, g)) = group {
        sync(db, &run(db, &p)?, g)?;
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
pub(crate) fn continuation(db: &Connection, parent: &str) -> Result<()> {
    let p = run(db, parent)?;
    if !matches!(p.status, RunStatus::Completed | RunStatus::Waiting) {
        return Ok(());
    }
    let tasks: Vec<Task> = rows(
        db,
        "SELECT t.* FROM tasks t JOIN cohorts c USING(parent_run_id,generation) WHERE
             parent_run_id=? AND c.status='acknowledged' AND c.continuation_attempt_id='' AND
             t.delivery_status='acknowledged' ORDER BY t.generation,t.created_at,t.id",
        [parent],
    )?;
    if tasks.is_empty() {
        return Ok(());
    }
    let att = id("att");
    let mut prompt = "父任务等待期间，Vantry 收到并已接受以下子任务结果：\n".to_string();
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
        prompt.push_str(&format!("\n### 子任务 {}\n- 任务 ID：{}\n{link}- generation：{}\n- 终态：{}\n\n结果：\n{result}\n", t.title, t.id, t.generation, serde_json::to_value(t.status)?.as_str().unwrap_or("")));
    }
    let sep = if p.output.is_empty() || p.output.ends_with("\n\n") {
        ""
    } else if p.output.ends_with('\n') {
        "\n"
    } else {
        "\n\n"
    };
    attempt(db, &p.thread_id, parent, &att, &prompt)?;
    db.execute(
        "UPDATE runs SET attempt_id=?,active_attempt_id=?,status='queued',updated_at=?,
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
    let groups: Vec<Cohort> = rows(
        db,
        "SELECT * FROM cohorts WHERE parent_run_id=? AND status='acknowledged' AND
             continuation_attempt_id=''",
        [parent],
    )?;
    for g in groups {
        db.execute(
            "UPDATE cohorts SET continuation_attempt_id=?,updated_at=? WHERE parent_run_id=? AND
             generation=?",
            params![att, now(), parent, g.generation],
        )?;
        group_event(db, &p, g.generation, "cohort.updated")?;
    }
    run_event(db, parent, "run.queued")?;
    Ok(())
}
