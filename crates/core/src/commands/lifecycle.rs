use super::*;
pub(super) fn apply(db: &Connection, c: &Command, options: &CoreOptions) -> Result<Receipt> {
    let t = thread(db, &c.thread_id)?;
    match c.kind {
        ForkThread => fork(db, c, t, options),
        MergeBackThread => {
            let target = first(
                &c.target_thread_id,
                first(&t.forked_from_thread_id, &t.parent_thread_id),
            );
            if target.is_empty() || target == t.id {
                return Err(invalid("different target thread required"));
            }
            let target = thread(db, target)?;
            if target.archived {
                return Err(conflict("target archived"));
            }
            let r: Run = one(
                db,
                "SELECT * FROM runs WHERE thread_id=? AND status='completed' ORDER BY created_at DESC,
             id DESC LIMIT 1",
                [&t.id],
            )
            .map_err(|e| if e.code() == ErrorCode::NotFound { conflict("no completed run") } else { e })?;
            let mut delivery = c.clone();
            delivery.message = format!(
                "Result from thread {} ({}), run {}:\n\n{}",
                t.title,
                t.id,
                r.id,
                first(&r.output, "(no output)")
            );
            queue(db, &target, &delivery)
        }
        RewindThread => {
            if active(db, &t.id)?.is_some() {
                return Err(conflict("thread has active work"));
            }
            let key = first(&c.run_id, &c.source_run_id);
            let target = run(db, key)?;
            if target.thread_id != t.id {
                return Err(conflict("run belongs to another thread"));
            }
            let ids = strings(
                db,
                "SELECT id FROM runs WHERE thread_id=? AND (created_at,id)>=(SELECT created_at,id FROM
             runs WHERE id=?) ORDER BY created_at,id",
                params![t.id, key],
            )?;
            for key in &ids {
                delete_run(db, key)?;
            }
            event(
                db,
                "thread.rewound",
                &t.id,
                key,
                "",
                json!({"threadId":t.id,"runId":key,"removedRunIds":ids}),
            )?;
            Ok(receipt(&t.id, key))
        }
        _ => {
            if !c.force {
                let busy: bool = db.query_row(
                    "SELECT EXISTS(SELECT 1 FROM tasks WHERE (parent_thread_id=?1 OR child_thread_id=?1) AND
             (delivery_status NOT IN ('acknowledged','disposed') OR status IN ('queued','running',
             'waiting_children')))",
                    [&t.id],
                    |r| r.get(0),
                )?;
                if busy || active(db, &t.id)?.is_some() {
                    return Err(conflict("thread has unsettled work"));
                }
            } else {
                cancel_queue(db, &t.id)?;
                for key in strings(
                    db,
                    "SELECT id FROM tasks WHERE child_thread_id=?1 OR parent_thread_id=?1",
                    [&t.id],
                )? {
                    tasks::cancel_task(db, &task(db, &key)?)?;
                }
                if let Some(k) = active(db, &t.id)? {
                    cancel(db, &run(db, &k)?)?;
                }
            }
            for key in strings(db, "SELECT id FROM runs WHERE thread_id=?", [&t.id])? {
                delete_run(db, &key)?;
            }
            for table in [
                "inputs",
                "queued_messages",
                "steering_messages",
                "provider_requests",
                "tool_calls",
                "run_blocks",
            ] {
                db.execute(&format!("DELETE FROM {table} WHERE thread_id=?"), [&t.id])?;
            }
            db.execute("DELETE FROM tasks WHERE child_thread_id=?", [&t.id])?;
            db.execute(
                "DELETE FROM thread_watches WHERE watcher_thread_id=?1 OR target_thread_id=?1",
                [&t.id],
            )?;
            set_unread_sequence(db, &t.id, 0)?;
            db.execute("DELETE FROM threads WHERE id=?", [&t.id])?;
            event(db, "thread.deleted", &t.id, "", "", json!({"thread":t}))?;
            Ok(receipt(&t.id, ""))
        }
    }
}
fn delete_run(db: &Connection, key: &str) -> Result<()> {
    for table in [
        "inputs",
        "queued_messages",
        "steering_messages",
        "tool_calls",
        "provider_requests",
        "project_updates",
        "run_blocks",
        "attempts",
    ] {
        db.execute(&format!("DELETE FROM {table} WHERE run_id=?"), [key])?;
    }
    db.execute(
        "DELETE FROM effect_outbox WHERE run_id=? AND NOT(kind='provider.stop' AND status IN ('pending',
             'claimed'))",
        [key],
    )?;
    db.execute("DELETE FROM cohorts WHERE parent_run_id=?", [key])?;
    db.execute("DELETE FROM tasks WHERE parent_run_id=?", [key])?;
    db.execute("DELETE FROM runs WHERE id=?", [key])?;
    Ok(())
}
fn fork(db: &Connection, c: &Command, t: Thread, options: &CoreOptions) -> Result<Receipt> {
    let target = first(&c.source_run_id, &c.run_id);
    let cutoff = if target.is_empty() {
        db.query_row(
            "SELECT id FROM runs WHERE thread_id=? AND status NOT IN ('queued','running',
             'waiting_children') ORDER BY created_at DESC,id DESC LIMIT 1",
            [&t.id],
            |r| r.get::<_, String>(0),
        )
        .optional()?
        .unwrap_or_default()
    } else {
        let r = run(db, target)?;
        if r.thread_id != t.id || !r.status.terminal() {
            return Err(conflict("fork source must be a settled run of this thread"));
        }
        target.into()
    };
    let child = Thread {
        id: id("th"),
        title: first(&c.title, &t.title).into(),
        forked_from_thread_id: t.id.clone(),
        forked_from_run_id: cutoff.clone(),
        provider_id: if c.provider_id.is_empty() && c.model.is_empty() {
            t.provider_id.clone()
        } else {
            switched_provider(&t, first(&c.model, &t.model), &c.provider_id, options)?
        },
        model: first(&c.model, &t.model).into(),
        workspace: c.workspace.clone().or(t.workspace.clone()),
        approval_policy: t.approval_policy,
        interaction_mode: t.interaction_mode,
        ..Default::default()
    };
    insert_thread(db, &child)?;
    for source in strings(
        db,
        "SELECT id FROM runs WHERE thread_id=? AND status='completed' AND (created_at,id)<=(SELECT
             created_at,id FROM runs WHERE id=?) ORDER BY created_at,id",
        params![t.id, cutoff],
    )? {
        let run = format!("run_{}:{source}", child.id);
        let att = format!("att_{}:{source}", child.id);
        db.execute(
            "INSERT INTO runs(id,thread_id,attempt_id,active_attempt_id,status,input,output,
             reasoning,error,created_at,updated_at,provider_id,model,workspace,approval_policy,
             attachments,interaction_mode,sender_thread_id) SELECT ?,?,?,'',status,input,output,
             reasoning,error,created_at,updated_at,provider_id,model,workspace,approval_policy,
             attachments,interaction_mode,sender_thread_id FROM runs WHERE id=?",
            params![run, child.id, att, source],
        )?;
        db.execute(
            "INSERT INTO attempts(id,run_id,ordinal,status,input,created_at,updated_at) SELECT ?,id,
             1,status,input,created_at,updated_at FROM runs WHERE id=?",
            params![att, run],
        )?;
        let forked_blocks = crate::store::blocks::fork_run_blocks(db, &source, &child.id, &run, &att)?;
        run_event_with_blocks(db, &run, "run.completed", &forked_blocks)?;
    }
    Ok(receipt(&child.id, ""))
}
