use crate::{
    scheduler::{enqueue_effect, provider_request},
    store::projection::*,
    *,
};
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::json;
use vantry_protocol::*;
mod lifecycle;
use CommandKind::*;

pub(crate) fn apply(db: &Connection, c: &Command, options: &CoreOptions) -> Result<Receipt> {
    let missing = match c.kind {
        UpdateThread | SetThreadModel | ForkThread | RewindThread | DeleteThread
        | MergeBackThread | SendMessage | QueueMessage | SteerMessage | SteerRun
            if c.thread_id.trim().is_empty() =>
        {
            Some("threadId")
        }
        StopRun if c.run_id.trim().is_empty() => Some("runId"),
        RewindThread if first(&c.run_id, &c.source_run_id).trim().is_empty() => Some("runId"),
        RespondRequest if c.request_id.trim().is_empty() => Some("requestId"),
        FollowUpTask | CancelTask | DisposeTask if c.task_id.trim().is_empty() => Some("taskId"),
        DelegateTask | AcknowledgeTask if c.parent_run_id.trim().is_empty() => Some("parentRunId"),
        AcknowledgeTask | DisposeTask if c.generation <= 0 => Some("generation"),
        AcknowledgeTask | DisposeTask if c.delivery_message_id.trim().is_empty() => {
            Some("deliveryMessageId")
        }
        InterThreadSend if first(&c.target_thread_id, &c.thread_id).trim().is_empty() => {
            Some("targetThreadId")
        }
        _ => None,
    };
    if let Some(name) = missing {
        return Err(invalid(&format!("{name} is required")));
    }
    if !matches!(c.interaction_mode.trim(), "" | "plan" | "default") {
        return Err(invalid("unsupported interactionMode"));
    }
    if !matches!(
        c.approval_policy.as_str(),
        "" | "auto" | "ask" | "auto-accept-edits"
    ) {
        return Err(invalid("unsupported approvalPolicy"));
    }
    if c.attachments.len() > crate::attachments::MAX_ATTACHMENTS {
        return Err(invalid("at most 32 attachments are supported"));
    }
    match c.kind {
        CreateThread | OpenProjectChat => create_thread(db, c, options),
        UpdateThread | SetThreadModel => update_thread(db, c, options),
        DeleteThread | ForkThread | MergeBackThread | RewindThread => {
            lifecycle::apply(db, c, options)
        }
        DelegateTask | FollowUpTask | AcknowledgeTask | UpgradeTaskWake | DisposeTask
        | CancelTask => tasks::command(db, c),
        EditInput | ReorderInput | CancelInput | CancelMessage | PromoteInputToSteer => {
            edit_queue(db, c)
        }
        RespondRequest => respond(db, c),
        StopRun => {
            let r = resolve_run(db, c)?;
            cancel(db, &r)?;
            Ok(receipt(&r.thread_id, &r.id))
        }
        SendMessage | QueueMessage | InterThreadSend | SteerMessage | SteerRun => send(db, c),
        Unspecified => Err(invalid("kind is required")),
    }
}
pub(crate) fn receipt(thread: &str, run: &str) -> Receipt {
    Receipt {
        thread_id: thread.into(),
        run_id: run.into(),
        ..Default::default()
    }
}
pub(crate) fn active(db: &Connection, thread: &str) -> Result<Option<String>> {
    Ok(db
        .query_row(
            "SELECT id FROM runs WHERE thread_id=? AND status IN ('queued','running',
             'waiting_children')",
            [thread],
            |r| r.get(0),
        )
        .optional()?)
}
fn resolve_run(db: &Connection, c: &Command) -> Result<Run> {
    if c.run_id.is_empty() {
        return Err(invalid("runId is required"));
    }
    let r = run(db, &c.run_id)?;
    if !c.thread_id.is_empty() && r.thread_id != c.thread_id {
        return Err(conflict("run belongs to another thread"));
    }
    if !c.parent_attempt_id.is_empty() && c.parent_attempt_id != r.attempt_id {
        return Err(conflict("attempt is no longer current"));
    }
    Ok(r)
}
pub(crate) fn snooze(s: Option<&str>) -> Result<i64> {
    match s {
        None | Some("0001-01-01T00:00:00Z") => Ok(0),
        Some(s) => chrono::DateTime::parse_from_rfc3339(s)
            .ok()
            .and_then(|t| t.timestamp_nanos_opt())
            .ok_or_else(|| invalid("invalid snoozedUntil")),
    }
}
pub(crate) fn insert_thread(db: &Connection, t: &Thread) -> Result<()> {
    db.execute(
        "INSERT INTO threads(id,title,created_at,provider_id,model,workspace,approval_policy,
             interaction_mode,parent_thread_id,parent_task_id,created_by_thread_id,
             created_by_run_id,project_path,forked_from_thread_id,forked_from_run_id,snoozed_until)
             VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        params![
            t.id,
            t.title,
            now(),
            t.provider_id,
            t.model,
            t.workspace
                .as_ref()
                .map(serde_json::to_string)
                .transpose()?
                .unwrap_or_default(),
            t.approval_policy,
            t.interaction_mode,
            t.parent_thread_id,
            t.parent_task_id,
            t.created_by_thread_id,
            t.created_by_run_id,
            t.project_path,
            t.forked_from_thread_id,
            t.forked_from_run_id,
            snooze(t.snoozed_until.as_deref())?
        ],
    )?;
    thread_event(db, &t.id, "thread.created")?;
    Ok(())
}
fn create_thread(db: &Connection, c: &Command, options: &CoreOptions) -> Result<Receipt> {
    let project = if c.kind == OpenProjectChat {
        let w = c
            .workspace
            .as_ref()
            .ok_or_else(|| invalid("project workspace is required"))?;
        let p = first(&w.repository, &w.path);
        if p.is_empty() {
            return Err(invalid("project path is required"));
        }
        if let Some(existing) = db
            .query_row("SELECT id FROM threads WHERE project_path=?", [p], |r| {
                r.get::<_, String>(0)
            })
            .optional()?
        {
            return Ok(receipt(&existing, ""));
        }
        p
    } else {
        ""
    };
    let t = Thread {
        id: first(&c.thread_id, &id("th")).into(),
        title: c.title.clone(),
        provider_id: first(&c.provider_id, &options.provider_id).into(),
        model: first(&c.model, &options.model).into(),
        workspace: c.workspace.clone().or_else(|| options.workspace.clone()),
        approval_policy: first(&c.approval_policy, "auto").into(),
        interaction_mode: first(&c.interaction_mode, "default").into(),
        created_by_run_id: c.created_by_run_id.clone(),
        created_by_thread_id: c.created_by_thread_id.clone(),
        snoozed_until: c.snoozed_until.clone(),
        project_path: project.into(),
        ..Default::default()
    };
    insert_thread(db, &t)?;
    Ok(receipt(&t.id, ""))
}
fn switched_provider(
    t: &Thread,
    model: &str,
    requested: &str,
    options: &CoreOptions,
) -> Result<String> {
    if !requested.is_empty() {
        if !matches!(requested, "controlled" | "pi")
            || requested == "controlled"
                && t.provider_id != "controlled"
                && options.provider_id != "controlled"
        {
            return Err(invalid("unsupported provider switch"));
        }
        return Ok(requested.into());
    }
    if t.provider_id == "controlled" && model != t.model && options.provider_id == "controlled" {
        return Err(invalid("controlled threads do not support model switching"));
    }
    if (t.provider_id == "controlled" && model != t.model)
        || !matches!(t.provider_id.as_str(), "pi" | "controlled")
    {
        return Ok(options.provider_id.clone());
    }
    Ok(t.provider_id.clone())
}
fn update_thread(db: &Connection, c: &Command, options: &CoreOptions) -> Result<Receipt> {
    let t = thread(db, &c.thread_id)?;
    if let Some(change) = c.read_state {
        let current = t.unread_sequence.unwrap_or(0);
        if c.kind != UpdateThread || change.observed_sequence < 0 {
            return Err(invalid("invalid thread read state"));
        }
        if change.observed_sequence == current && change.unread != (current > 0) {
            let next = if change.unread { sequence(db)? + 1 } else { 0 };
            set_unread_sequence(db, &t.id, next)?;
            thread_event(db, &t.id, "thread.updated")?;
        }
        return Ok(receipt(&t.id, ""));
    }
    if c.kind == UpdateThread
        && (!c.model.is_empty() || !c.interaction_mode.is_empty() || c.archived == Some(true))
        && (active(db, &t.id)?.is_some()
            || !queued(db, &t.id)?.is_empty()
            || (c.archived == Some(true) && tasks::has_active_for_thread(db, &t.id)?))
    {
        return Err(conflict("thread has active work"));
    }
    if !c.title.is_empty() && c.title.trim().is_empty() {
        return Err(invalid("empty thread title"));
    }
    if c.kind == SetThreadModel {
        db.execute(
            "UPDATE threads SET model=?,provider_id=? WHERE id=?",
            params![
                first(&c.model, &options.model),
                switched_provider(&t, first(&c.model, &options.model), &c.provider_id, options)?,
                t.id
            ],
        )?;
    } else {
        db.execute(
            "UPDATE threads SET title=?,model=?,provider_id=?,interaction_mode=?,archived=?,
             pinned=?,snoozed_until=? WHERE id=?",
            params![
                first(c.title.trim(), &t.title),
                first(&c.model, &t.model),
                if c.model.is_empty() {
                    t.provider_id.clone()
                } else {
                    switched_provider(&t, &c.model, &c.provider_id, options)?
                },
                first(&c.interaction_mode, &t.interaction_mode),
                c.archived.unwrap_or(t.archived),
                c.pinned.unwrap_or(t.pinned),
                snooze(c.snoozed_until.as_deref().or(t.snoozed_until.as_deref()))?,
                t.id
            ],
        )?;
    }
    thread_event(db, &t.id, "thread.updated")?;
    Ok(receipt(&t.id, ""))
}
pub(crate) fn attempt(
    db: &Connection,
    thread: &str,
    run: &str,
    attempt: &str,
    message: &str,
) -> Result<()> {
    db.execute(
        "INSERT INTO attempts(id,run_id,ordinal,status,input,created_at,updated_at) SELECT ?,?,
             COALESCE(MAX(ordinal),0)+1,'queued',?,?,? FROM attempts WHERE run_id=?",
        params![attempt, run, message, now(), now(), run],
    )?;
    enqueue_effect(db, "provider.run", thread, run, attempt, message)?;
    Ok(())
}
pub(crate) fn create_run(
    db: &Connection,
    t: &Thread,
    message: &str,
    sender: &str,
    attachments: &[Attachment],
) -> Result<Run> {
    if t.archived {
        return Err(conflict("thread is archived"));
    }
    let run_id = id("run");
    let att = id("att");
    let ns = now();
    if t.snoozed_until.is_some() {
        db.execute("UPDATE threads SET snoozed_until=0 WHERE id=?", [&t.id])?;
        thread_event(db, &t.id, "thread.updated")?;
    }
    db.execute(
        "INSERT INTO runs(id,thread_id,attempt_id,active_attempt_id,status,input,created_at,
             updated_at,provider_id,model,workspace,parent_task_id,approval_policy,attachments,
             interaction_mode,sender_thread_id) VALUES(?,?,?,?,'queued',?,?,?,?,?,?,?,?,?,?,?)",
        params![
            run_id,
            t.id,
            att,
            att,
            message,
            ns,
            ns,
            t.provider_id,
            t.model,
            t.workspace
                .as_ref()
                .map(serde_json::to_string)
                .transpose()?
                .unwrap_or_default(),
            t.parent_task_id,
            t.approval_policy,
            serde_json::to_string(attachments)?,
            t.interaction_mode,
            sender
        ],
    )?;
    attempt(db, &t.id, &run_id, &att, message)?;
    let r = run(db, &run_id)?;
    let user_block = crate::store::blocks::record_user(db, &r)?;
    run_event_with_block(db, &run_id, "run.queued", user_block.as_ref())?;
    Ok(r)
}
fn queued(db: &Connection, thread: &str) -> Result<Vec<String>> {
    strings(
        db,
        "SELECT id FROM inputs WHERE thread_id=? AND kind='queue' AND status='queued' ORDER BY
             queue_order,created_at,id",
        [thread],
    )
}
fn order(db: &Connection, ids: &[String]) -> Result<()> {
    for (pos, key) in ids.iter().enumerate() {
        if db.execute(
            "UPDATE inputs SET queue_order=?,updated_at=? WHERE id=? AND queue_order<>?",
            params![pos as i64, now(), key, pos as i64],
        )? > 0
        {
            input_event(db, key)?;
        }
    }
    Ok(())
}
fn config(t: &Thread) -> InputConfiguration {
    InputConfiguration {
        provider_id: t.provider_id.clone(),
        model: t.model.clone(),
        interaction_mode: t.interaction_mode.clone(),
        approval_policy: t.approval_policy.clone(),
    }
}
pub(crate) fn settle_waiting_run(db: &Connection, run_id: &str) -> Result<bool> {
    if db.execute(
        "UPDATE runs SET status='completed',updated_at=? WHERE id=? AND status='waiting_children'",
        params![now(), run_id],
    )? > 0
    {
        crate::store::blocks::clear_status(db, run_id)?;
        crate::store::blocks::close_streaming(db, run_id)?;
        run_event(db, run_id, "run.completed")?;
        Ok(true)
    } else {
        Ok(false)
    }
}
pub(crate) fn dequeue(db: &Connection, thread_id: &str) -> Result<()> {
    let Some(key) = queued(db, thread_id)?.into_iter().next() else {
        return Ok(());
    };
    if let Some(active_id) = active(db, thread_id)? {
        if run(db, &active_id)?.status == RunStatus::Waiting {
            // A waiting_children run has no active provider attempt. Settle it cleanly
            // without cancelling background children so the user's message runs immediately
            // (matching T3 Code's non-blocking parent thread semantics).
            settle_waiting_run(db, &active_id)?;
        } else {
            return Ok(());
        }
    }
    let i = input(db, &key)?;
    let mut t = thread(db, thread_id)?;
    if t.archived {
        return Ok(());
    }
    if let Some(cfg) = i.configuration {
        t.provider_id = cfg.provider_id;
        t.model = cfg.model;
        t.approval_policy = cfg.approval_policy;
        t.interaction_mode = first(&cfg.interaction_mode, "default").into();
    }
    let r = create_run(db, &t, &i.message, &i.sender_thread_id, &i.attachments)?;
    db.execute(
        "UPDATE inputs SET status='started',run_id=?,attempt_id=?,updated_at=? WHERE id=?",
        params![r.id, r.attempt_id, now(), key],
    )?;
    input_event(db, &key)?;
    order(db, &queued(db, thread_id)?)?;
    Ok(())
}
pub(crate) fn queue(db: &Connection, t: &Thread, c: &Command) -> Result<Receipt> {
    let key = id("inp");
    let pos = queued(db, &t.id)?.len() as i64;
    db.execute(
        "INSERT INTO inputs(id,thread_id,kind,message,command_id,status,attachments,created_at,
             updated_at,queue_order,configuration,sender_thread_id) VALUES(?,?,'queue',?,?,'queued',
             ?,?,?,?,?,?)",
        params![
            key,
            t.id,
            c.message,
            c.command_id,
            serde_json::to_string(&c.attachments)?,
            now(),
            now(),
            pos,
            serde_json::to_string(&config(t))?,
            c.sender_thread_id
        ],
    )?;
    input_event(db, &key)?;
    dequeue(db, &t.id)?;
    Ok(input_receipt(&input(db, &key)?))
}
fn input_receipt(i: &Input) -> Receipt {
    Receipt {
        input_id: i.id.clone(),
        message_id: i.id.clone(),
        ..receipt(&i.thread_id, &i.run_id)
    }
}
fn send(db: &Connection, c: &Command) -> Result<Receipt> {
    if c.message.trim().is_empty() && (c.kind != SendMessage || c.attachments.is_empty()) {
        return Err(invalid("message is required"));
    }
    let key = if c.kind == InterThreadSend {
        first(&c.target_thread_id, &c.thread_id)
    } else {
        &c.thread_id
    };
    let mut t = thread(db, key)?;
    if t.archived {
        return Err(conflict("thread is archived"));
    }
    if c.kind == InterThreadSend && !c.sender_thread_id.is_empty() {
        ceiling(&thread(db, &c.sender_thread_id)?, &t)?;
    }
    t.interaction_mode = first(&c.interaction_mode, &t.interaction_mode).into();
    match c.kind {
        SendMessage => {
            if let Some(active_id) = active(db, key)?
                && run(db, &active_id)?.status == RunStatus::Waiting
            {
                settle_waiting_run(db, &active_id)?;
            }
            let r = create_run(db, &t, &c.message, &c.sender_thread_id, &c.attachments)?;
            Ok(receipt(key, &r.id))
        }
        QueueMessage => queue(db, &t, c),
        _ => {
            let inter = c.kind == InterThreadSend;
            let mode = if inter {
                first(&c.delivery_mode, "auto")
            } else {
                "steer"
            };
            if !matches!(mode, "queue" | "steer" | "auto") {
                return Err(invalid("unsupported delivery mode"));
            }
            let target = if !c.run_id.is_empty() {
                Some(c.run_id.clone())
            } else {
                if !c.parent_attempt_id.is_empty() {
                    return Err(invalid("runId required with attempt"));
                }
                active(db, key)?
            };
            if mode != "queue" {
                if let Some(target) = target {
                    let mut r = run(db, &target)?;
                    if r.thread_id != key {
                        return Err(conflict("run belongs to another thread"));
                    }
                    let user_steer = c.kind == SteerMessage && c.parent_attempt_id.is_empty();
                    if user_steer
                        && r.status.terminal()
                        && let Some(current) = active(db, key)?
                    {
                        r = run(db, &current)?;
                    }
                    if r.status == RunStatus::Running {
                        if !c.parent_attempt_id.is_empty() && c.parent_attempt_id != r.attempt_id {
                            return Err(conflict("stale attempt"));
                        }
                        let input = id("inp");
                        db.execute(
                            "INSERT INTO inputs(id,thread_id,run_id,attempt_id,kind,message,command_id,status,
             attachments,created_at,updated_at,queue_order,sender_thread_id) VALUES(?,?,?,?,'steer',
             ?,?,'pending',?,?,?,0,?)",
                            params![input, key, r.id, r.attempt_id, c.message, c.command_id, serde_json::to_string(&c.attachments)?, now(), now(), c.sender_thread_id],
                        )?;
                        let mut rec = steer(db, &input, &r)?;
                        if inter {
                            rec.delivery = "steered".into();
                        }
                        return Ok(rec);
                    }
                    if mode == "steer"
                        && !(user_steer
                            || c.fallback_to_queue
                            || !inter
                                && r.status == RunStatus::Completed
                                && c.parent_attempt_id.is_empty())
                    {
                        return Err(conflict("target has no running attempt"));
                    }
                } else if inter && mode == "steer" && !c.fallback_to_queue {
                    return Err(conflict("no active run"));
                }
            }
            let mut rec = queue(db, &t, c)?;
            if inter {
                rec.delivery = if rec.run_id.is_empty() {
                    "queued"
                } else {
                    "started"
                }
                .into();
            }
            Ok(rec)
        }
    }
}
pub(crate) fn ceiling(parent: &Thread, target: &Thread) -> Result<()> {
    let rank = |s: &str| match s {
        "ask" => 0,
        "auto-accept-edits" => 1,
        _ => 2,
    };
    if rank(&target.approval_policy) > rank(&parent.approval_policy)
        || parent.interaction_mode == "plan" && target.interaction_mode != "plan"
    {
        return Err(conflict("requested permissions exceed caller thread"));
    }
    Ok(())
}
fn steer(db: &Connection, key: &str, r: &Run) -> Result<Receipt> {
    let i = input(db, key)?;
    control(
        db,
        r,
        ControlResponse {
            id: key.into(),
            kind: "steer".into(),
            message: i.message.clone(),
            attachments: i.attachments.clone(),
            ..Default::default()
        },
    )?;
    input_event(db, key)?;
    Ok(input_receipt(&i))
}
fn edit_queue(db: &Connection, c: &Command) -> Result<Receipt> {
    let key = first(&c.input_id, &c.message_id);
    if key.is_empty() {
        return Err(invalid("inputId required"));
    }
    let i = input(db, key)?;
    if !c.thread_id.is_empty() && c.thread_id != i.thread_id {
        return Err(conflict("input belongs to another thread"));
    }
    if matches!(c.kind, CancelMessage | CancelInput) && i.status == "cancelled" {
        return Ok(input_receipt(&i));
    }
    if i.kind != "queue" || i.status != "queued" {
        return Err(conflict("input is not queued"));
    }
    match c.kind {
        EditInput => {
            if c.message.trim().is_empty() {
                return Err(invalid("message required"));
            }
            db.execute(
                "UPDATE inputs SET message=?,attachments=?,updated_at=? WHERE id=?",
                params![
                    c.message,
                    serde_json::to_string(if c.value == "replace-attachments" {
                        &c.attachments
                    } else {
                        &i.attachments
                    })?,
                    now(),
                    key
                ],
            )?;
            input_event(db, key)?;
        }
        ReorderInput => {
            let mut ids = queued(db, &i.thread_id)?;
            if c.position < 0 || c.position as usize >= ids.len() {
                return Err(invalid("position outside queue"));
            }
            ids.retain(|s| s != key);
            ids.insert(c.position as usize, key.into());
            order(db, &ids)?;
        }
        PromoteInputToSteer => {
            let target = active(db, &i.thread_id)?;
            if !c.run_id.is_empty() && target.as_deref() != Some(c.run_id.as_str()) {
                return Err(conflict("target run has changed"));
            }
            if let Some(target) = target {
                let r = run(db, &target)?;
                if r.status == RunStatus::Waiting {
                    settle_waiting_run(db, &r.id)?;
                    db.execute("UPDATE inputs SET queue_order=-1 WHERE id=?", [key])?;
                    dequeue(db, &i.thread_id)?;
                } else if r.status != RunStatus::Running {
                    return Err(conflict("no running attempt"));
                } else {
                    db.execute(
                        "UPDATE inputs SET kind='steer',status='pending',run_id=?,attempt_id=?,command_id=?,
                 queue_order=0,updated_at=? WHERE id=?",
                        params![r.id, r.attempt_id, c.command_id, now(), key],
                    )?;
                    steer(db, key, &r)?;
                }
            } else {
                db.execute("UPDATE inputs SET queue_order=-1 WHERE id=?", [key])?;
                dequeue(db, &i.thread_id)?;
            }
            order(db, &queued(db, &i.thread_id)?)?;
        }
        _ => {
            db.execute(
                "UPDATE inputs SET status='cancelled',error='queue cancelled by input.cancel',
             updated_at=? WHERE id=?",
                params![now(), key],
            )?;
            input_event(db, key)?;
            order(db, &queued(db, &i.thread_id)?)?;
        }
    }
    Ok(input_receipt(&input(db, key)?))
}
pub(crate) fn control(db: &Connection, r: &Run, control: ControlResponse) -> Result<()> {
    let req = provider_request(db, r)?;
    enqueue_effect(
        db,
        "provider.control",
        &r.thread_id,
        &r.id,
        &r.attempt_id,
        &json!({"request":req,"control":control}).to_string(),
    )
}
fn respond(db: &Connection, c: &Command) -> Result<Receipt> {
    let req: PendingRequest = one(
        db,
        "SELECT * FROM provider_requests WHERE id=?",
        [&c.request_id],
    )?;
    let r = run(db, &req.run_id)?;
    let expires: i64 = db.query_row(
        "SELECT expires_at FROM provider_requests WHERE id=?",
        [&req.id],
        |r| r.get(0),
    )?;
    if r.status != RunStatus::Running
        || r.attempt_id != req.attempt_id
        || req.status != "pending"
        || expires != 0 && expires <= now()
    {
        return Err(conflict("request is no longer pending"));
    }
    if req.method == "select" && !c.cancelled && !req.options.contains(&c.value) {
        return Err(invalid("value not in options"));
    }
    db.execute(
        "UPDATE provider_requests SET status='responding',updated_at=? WHERE id=?",
        params![now(), req.id],
    )?;
    request_event(db, &req.id)?;
    control(
        db,
        &r,
        ControlResponse {
            id: req.id.clone(),
            request_id: req.provider_request_id,
            kind: "response".into(),
            value: c.value.clone(),
            confirmed: c.confirmed,
            cancelled: c.cancelled,
            ..Default::default()
        },
    )?;
    Ok(Receipt {
        request_id: req.id,
        ..receipt(&r.thread_id, &r.id)
    })
}
pub(crate) fn cancel(db: &Connection, r: &Run) -> Result<()> {
    if r.status.terminal() {
        return Ok(());
    }
    cancel_queue(db, &r.thread_id)?;
    if r.status == RunStatus::Running {
        let req = provider_request(db, r)?;
        enqueue_effect(
            db,
            "provider.stop",
            &r.thread_id,
            &r.id,
            &r.attempt_id,
            &serde_json::to_string(&req)?,
        )?;
    }
    db.execute(
        "UPDATE effect_outbox SET status='cancelled' WHERE run_id=? AND kind<>'provider.stop'
             AND status IN ('pending','claimed')",
        [&r.id],
    )?;
    tasks::dispose_parent_deliveries(db, r)?;
    crate::scheduler::finish(db, r, "cancelled", "")?;
    for key in strings(db, "SELECT id FROM tasks WHERE parent_run_id=?", [&r.id])? {
        tasks::cancel_task(db, &task(db, &key)?)?;
    }
    Ok(())
}
pub(crate) fn cancel_queue(db: &Connection, thread_id: &str) -> Result<()> {
    for key in queued(db, thread_id)? {
        db.execute(
            "UPDATE inputs SET status='cancelled',error='queue cancelled by run.stop',updated_at=?
             WHERE id=?",
            params![now(), key],
        )?;
        input_event(db, &key)?;
    }
    Ok(())
}
