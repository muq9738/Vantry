use super::*;

/// Register a durable v11 watch inside a write transaction. If the target run
/// has already reached a terminal state, deliver immediately so callers never
/// miss a completion that raced registration.
pub(crate) fn register(
    db: &Connection,
    watcher_thread_id: &str,
    target_thread_id: &str,
    target_run_id: &str,
) -> Result<String> {
    if watcher_thread_id.trim().is_empty() || target_thread_id.trim().is_empty() {
        return Err(invalid("watcher and target thread IDs are required"));
    }
    if watcher_thread_id == target_thread_id {
        return Err(invalid("a thread cannot watch itself"));
    }
    let _ = thread(db, watcher_thread_id)?;
    let _ = thread(db, target_thread_id)?;
    if !target_run_id.is_empty() {
        let r = run(db, target_run_id)?;
        if r.thread_id != target_thread_id {
            return Err(invalid("targetRunId does not belong to targetThreadId"));
        }
    }
    let watch_id = id("watch");
    let ts = now();
    db.execute(
        "INSERT INTO thread_watches(id,watcher_thread_id,target_thread_id,target_run_id,status,created_at,updated_at) VALUES(?,?,?,?,'pending',?,?)",
        params![watch_id, watcher_thread_id, target_thread_id, target_run_id, ts, ts],
    )?;
    if !target_run_id.is_empty() {
        let r = run(db, target_run_id)?;
        if r.status.terminal() {
            deliver(db, &r)?;
        }
    }
    Ok(watch_id)
}

/// A pending v11 watch and its wake-up input transition in the same transaction.
pub(crate) fn deliver(db: &Connection, r: &Run) -> Result<()> {
    if !r.status.terminal() {
        return Ok(());
    }
    let watches = db.prepare("SELECT id,watcher_thread_id FROM thread_watches WHERE status='pending' AND target_thread_id=? AND (target_run_id='' OR target_run_id=?) ORDER BY created_at,id")?.query_map(params![r.thread_id, r.id], |row| Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?)))?.collect::<rusqlite::Result<Vec<_>>>()?;
    if watches.is_empty() {
        return Ok(());
    }
    let target = thread(db, &r.thread_id)?;
    let label = first(target.title.trim(), &target.id)
        .replace('[', "(")
        .replace(']', ")")
        .replace(['\r', '\n'], " ");
    let mut result = r.output.trim().to_owned();
    if result.is_empty() {
        result = "（无输出）".into();
    }
    if !r.error.trim().is_empty() {
        result.push_str("\n\n错误：");
        result.push_str(&r.error);
    }
    let msg = format!(
        "Vantry 自动唤醒：关注的会话 [{label}](vantry-thread://{}) 已结束当前轮次。\n- 会话 ID：{}\n- 运行 ID：{}\n- 终态：{}\n\n结果：\n{}\n",
        r.thread_id,
        r.thread_id,
        r.id,
        serde_json::to_value(r.status)?.as_str().unwrap_or_default(),
        result.chars().take(4000).collect::<String>()
    );
    for (key, watcher) in watches {
        let target = match thread(db, &watcher) {
            Ok(t) if !t.archived => Some(t),
            Ok(_) => None,
            Err(e) if e.code() == ErrorCode::NotFound => None,
            Err(e) => return Err(e),
        };
        let state = if target.is_some() {
            "delivered"
        } else {
            "disposed"
        };
        db.execute(
            "UPDATE thread_watches SET status=?,updated_at=? WHERE id=?",
            params![state, now(), key],
        )?;
        if let Some(t) = target {
            commands::queue(
                db,
                &t,
                &Command {
                    sender_thread_id: r.thread_id.clone(),
                    message: msg.clone(),
                    ..Command::new(format!("watch:{key}"), CommandKind::InterThreadSend)
                },
            )?;
        }
    }
    Ok(())
}
pub(crate) fn recover(db: &Connection) -> Result<()> {
    for key in strings(
        db,
        "SELECT DISTINCT r.id FROM thread_watches w JOIN runs r ON w.target_thread_id=r.thread_id AND (w.target_run_id='' OR w.target_run_id=r.id) WHERE w.status='pending' AND r.status IN ('completed','failed','interrupted','cancelled') ORDER BY r.updated_at,r.id",
        [],
    )? {
        deliver(db, &run(db, &key)?)?;
    }
    Ok(())
}
