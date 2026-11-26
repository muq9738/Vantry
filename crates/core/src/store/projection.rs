//! Convert the baseline SQL column representations to the v1 DTOs. Unknown SQL
//! columns are ignored by serde, which keeps private projection fields private.
use crate::{Result, id, now, timestamp};
use rusqlite::{Connection, Params, Row, params, types::ValueRef};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::{Map, Value, json};
use vantry_protocol::*;
fn camel(column: &str) -> String {
    let mut pieces = column.split('_');
    let mut key = pieces.next().unwrap().to_owned();
    for piece in pieces {
        let mut chars = piece.chars();
        if let Some(c) = chars.next() {
            key.push(c.to_ascii_uppercase());
            key.push_str(chars.as_str());
        }
    }
    key
}
fn decode<T: DeserializeOwned>(row: &Row) -> Result<T> {
    let mut obj = Map::new();
    for (i, col) in row.as_ref().column_names().iter().copied().enumerate() {
        let key = match col {
            "queue_order" => "position".into(),
            "source_sequence" => "sequence".into(),
            _ => camel(col),
        };
        let value = match (col, row.get_ref(i)?) {
            ("snoozed_until", ValueRef::Integer(0)) => continue,
            (
                "timestamp" | "created_at" | "updated_at" | "snoozed_until",
                ValueRef::Integer(ns),
            ) => json!(timestamp(ns)),
            ("accepted" | "pinned" | "archived", ValueRef::Integer(n)) => json!(n != 0),
            (
                "workspace" | "configuration" | "attachments" | "arguments" | "options" | "data",
                ValueRef::Text(s) | ValueRef::Blob(s),
            ) => {
                if s.is_empty() || s == b"null" {
                    continue;
                }
                serde_json::from_slice(s)?
            }
            (_, ValueRef::Text(_)) => json!(row.get::<_, String>(i)?),
            (_, ValueRef::Integer(n)) => json!(n),
            (_, ValueRef::Real(n)) => json!(n),
            (_, ValueRef::Null) => Value::Null,
            (_, ValueRef::Blob(b)) => json!(b),
        };
        obj.insert(key, value);
    }
    Ok(serde_json::from_value(Value::Object(obj))?)
}
pub(crate) fn rows<T: DeserializeOwned>(
    db: &Connection,
    sql: &str,
    args: impl Params,
) -> Result<Vec<T>> {
    let mut stmt = db.prepare(sql)?;
    let mut rows = stmt.query(args)?;
    let mut out = vec![];
    while let Some(row) = rows.next()? {
        out.push(decode(row)?);
    }
    Ok(out)
}
pub(crate) fn one<T: DeserializeOwned>(db: &Connection, sql: &str, args: impl Params) -> Result<T> {
    let mut stmt = db.prepare(sql)?;
    let mut rows = stmt.query(args)?;
    decode(rows.next()?.ok_or(rusqlite::Error::QueryReturnedNoRows)?)
}
const THREAD_ROWS: &str = "SELECT threads.*, COALESCE(m.int_value, 0) AS unread_sequence
    FROM threads LEFT JOIN core_metadata m ON m.key = 'thread.unread:' || threads.id";

pub(crate) fn thread(db: &Connection, key: &str) -> Result<Thread> {
    one(db, &format!("{THREAD_ROWS} WHERE threads.id=?"), [key])
}

pub(crate) fn set_unread_sequence(db: &Connection, thread_id: &str, sequence: i64) -> Result<()> {
    let key = format!("thread.unread:{thread_id}");
    if sequence == 0 {
        db.execute("DELETE FROM core_metadata WHERE key=?", [key])?;
    } else {
        db.execute("INSERT INTO core_metadata(key,int_value) VALUES(?,?)
            ON CONFLICT(key) DO UPDATE SET int_value=excluded.int_value", params![key, sequence])?;
    }
    Ok(())
}
pub(crate) fn run(db: &Connection, key: &str) -> Result<Run> {
    one(db, "SELECT * FROM runs WHERE id=?", [key])
}
pub(crate) fn input(db: &Connection, key: &str) -> Result<Input> {
    one(db, "SELECT * FROM inputs WHERE id=?", [key])
}
pub(crate) fn task(db: &Connection, key: &str) -> Result<Task> {
    one(db, "SELECT * FROM tasks WHERE id=?", [key])
}
pub(crate) fn cohort(db: &Connection, parent: &str, generation: i64) -> Result<Cohort> {
    one(
        db,
        "SELECT * FROM cohorts WHERE parent_run_id=? AND generation=?",
        params![parent, generation],
    )
}
pub(crate) fn strings(db: &Connection, sql: &str, args: impl Params) -> Result<Vec<String>> {
    Ok(db
        .prepare(sql)?
        .query_map(args, |r| r.get(0))?
        .collect::<rusqlite::Result<_>>()?)
}
pub(crate) fn event(
    db: &Connection,
    kind: &str,
    thread: &str,
    run: &str,
    attempt: &str,
    data: impl Serialize,
) -> Result<i64> {
    db.execute(
        "INSERT INTO events(id,type,thread_id,run_id,attempt_id,timestamp,data) VALUES(?,?,?,?,
             ?,?,?)",
        params![
            id("evt"),
            kind,
            thread,
            run,
            attempt,
            now(),
            serde_json::to_vec(&data)?
        ],
    )?;
    Ok(db.last_insert_rowid())
}
pub(crate) fn sequence(db: &Connection) -> Result<i64> {
    Ok(db.query_row(
        "SELECT MAX(COALESCE(MAX(sequence),0),(SELECT int_value FROM core_metadata WHERE key='pruned_through_sequence'))
             FROM events",
        [],
        |r| r.get(0),
    )?)
}
pub(crate) fn run_event(db: &Connection, key: &str, kind: &str) -> Result<i64> {
    run_event_with_block(db, key, kind, None)
}
pub(crate) fn run_event_with_block(
    db: &Connection,
    key: &str,
    kind: &str,
    block: Option<&RunBlock>,
) -> Result<i64> {
    let r = run(db, key)?;
    let data = match block {
        Some(block) => json!({"run": r, "block": block}),
        None => json!({"run": r}),
    };
    let seq = event(db, kind, &r.thread_id, key, &r.attempt_id, data)?;
    if matches!(kind, "run.completed" | "run.failed" | "run.interrupted") {
        set_unread_sequence(db, &r.thread_id, seq)?;
    }
    Ok(seq)
}
pub(crate) fn run_event_with_blocks(
    db: &Connection,
    key: &str,
    kind: &str,
    blocks: &[RunBlock],
) -> Result<i64> {
    let r = run(db, key)?;
    let data = if blocks.is_empty() {
        json!({"run": r})
    } else {
        json!({"run": r, "blocks": blocks})
    };
    let seq = event(db, kind, &r.thread_id, key, &r.attempt_id, data)?;
    if matches!(kind, "run.completed" | "run.failed" | "run.interrupted") {
        set_unread_sequence(db, &r.thread_id, seq)?;
    }
    Ok(seq)
}
pub(crate) fn thread_event(db: &Connection, key: &str, kind: &str) -> Result<i64> {
    event(db, kind, key, "", "", json!({"thread":thread(db,key)?}))
}
fn legacy_queue(i: &Input) -> QueuedMessage {
    QueuedMessage {
        id: i.id.clone(),
        thread_id: i.thread_id.clone(),
        message: i.message.clone(),
        attachments: i.attachments.clone(),
        status: if i.status == "queued" {
            "pending".into()
        } else {
            i.status.clone()
        },
        run_id: i.run_id.clone(),
        created_at: i.created_at.clone(),
    }
}
fn legacy_steer(i: &Input, disposition: String) -> SteeringMessage {
    SteeringMessage {
        id: i.id.clone(),
        thread_id: i.thread_id.clone(),
        command_id: i.command_id.clone(),
        message: i.message.clone(),
        attachments: i.attachments.clone(),
        status: match i.status.as_str() {
            "delivered" => SteeringStatus::Accepted,
            "pending" => SteeringStatus::Pending,
            "unknown" => SteeringStatus::Unknown,
            _ => SteeringStatus::Rejected,
        },
        run_id: i.run_id.clone(),
        attempt_id: i.attempt_id.clone(),
        created_at: i.created_at.clone(),
        updated_at: i.updated_at.clone(),
        error: i.error.clone(),
        disposition,
    }
}
pub(crate) fn input_event(db: &Connection, key: &str) -> Result<()> {
    let i = input(db, key)?;
    let block = super::blocks::upsert_steer(db, &i)?;
    let data = match block {
        Some(block) => json!({"input": i, "block": block}),
        None => json!({"input": i}),
    };
    event(
        db,
        "input.updated",
        &i.thread_id,
        &i.run_id,
        &i.attempt_id,
        data,
    )?;
    if i.kind == "queue" {
        event(
            db,
            "message.updated",
            &i.thread_id,
            &i.run_id,
            &i.attempt_id,
            json!({"message":legacy_queue(&i)}),
        )?;
    } else {
        let disposition =
            db.query_row("SELECT disposition FROM inputs WHERE id=?", [key], |r| {
                r.get(0)
            })?;
        event(
            db,
            "message.steering",
            &i.thread_id,
            &i.run_id,
            &i.attempt_id,
            json!({"steering":legacy_steer(&i,disposition)}),
        )?;
    }
    Ok(())
}
pub(crate) fn request_event(db: &Connection, key: &str) -> Result<()> {
    let r: PendingRequest = one(db, "SELECT * FROM provider_requests WHERE id=?", [key])?;
    let block = super::blocks::upsert_approval(db, &r)?;
    event(
        db,
        "request.updated",
        &r.thread_id,
        &r.run_id,
        &r.attempt_id,
        json!({"request": r, "block": block}),
    )?;
    Ok(())
}
pub(crate) fn snapshot(db: &Connection) -> Result<Snapshot> {
    let mut snap = Snapshot {
        sequence: sequence(db)?,
        threads: Some(rows(
            db,
            &format!("{THREAD_ROWS} ORDER BY threads.created_at,threads.id"),
            [],
        )?),
        runs: Some(rows(db, "SELECT * FROM runs ORDER BY created_at,id", [])?),
        tasks: Some(rows(db, "SELECT * FROM tasks ORDER BY created_at,id", [])?),
        cohorts: Some(rows(
            db,
            "SELECT * FROM cohorts ORDER BY created_at,parent_run_id,generation",
            [],
        )?),
        inputs: rows(db, "SELECT * FROM inputs ORDER BY created_at,id", [])?,
        tools: rows(db, "SELECT * FROM tool_calls ORDER BY updated_at,id", [])?,
        requests: rows(
            db,
            "SELECT * FROM provider_requests ORDER BY created_at,id",
            [],
        )?,
        blocks: rows(
            db,
            "SELECT * FROM run_blocks ORDER BY run_id,seq,id",
            [],
        )?,
        project_updates: rows(
            db,
            "SELECT * FROM project_updates ORDER BY source_sequence",
            [],
        )?,
        ..Default::default()
    };
    for i in &snap.inputs {
        if i.kind == "queue" {
            snap.messages.push(legacy_queue(i));
        } else if i.kind == "steer" {
            snap.steering.push(legacy_steer(
                i,
                db.query_row("SELECT disposition FROM inputs WHERE id=?", [&i.id], |r| {
                    r.get(0)
                })?,
            ));
        }
    }
    for r in snap.runs.iter_mut().flatten() {
        r.telemetry = crate::telemetry::read(db, &r.id)?;
    }
    Ok(snap)
}

/// Direct SQL projection for `/v1/threads/summaries` without loading run bodies,
/// tool calls, or per-run telemetry.
pub(crate) fn thread_summaries(db: &Connection) -> Result<ThreadSummaries> {
    use std::collections::HashMap;
    let seq = sequence(db)?;
    let raw_threads: Vec<Thread> = rows(db, "SELECT * FROM threads ORDER BY created_at,id", [])?;

    #[derive(Default)]
    struct RunStats {
        run_count: i64,
        active_runs: i64,
        latest_run_id: String,
        latest_status: Option<RunStatus>,
        max_updated_at: String,
    }
    let mut stats: HashMap<String, RunStats> = HashMap::new();
    {
        let mut stmt = db.prepare(
            "SELECT id, thread_id, status, updated_at FROM runs ORDER BY created_at, id",
        )?;
        let mut q = stmt.query([])?;
        while let Some(row) = q.next()? {
            let run_id: String = row.get(0)?;
            let thread_id: String = row.get(1)?;
            let status_str: String = row.get(2)?;
            let updated_ns: i64 = row.get(3)?;
            let updated_at = timestamp(updated_ns);
            let status: Option<RunStatus> =
                serde_json::from_value(Value::String(status_str)).ok();
            let entry = stats.entry(thread_id).or_default();
            entry.run_count += 1;
            if matches!(
                status,
                Some(RunStatus::Queued | RunStatus::Running | RunStatus::Waiting)
            ) {
                entry.active_runs += 1;
            }
            entry.latest_run_id = run_id;
            entry.latest_status = status;
            if updated_at > entry.max_updated_at {
                entry.max_updated_at = updated_at;
            }
        }
    }

    let mut pending_inputs: HashMap<String, i64> = HashMap::new();
    {
        let mut stmt = db.prepare(
            "SELECT thread_id, COUNT(*) FROM inputs WHERE status IN ('queued','pending','') GROUP BY thread_id",
        )?;
        let mut q = stmt.query([])?;
        while let Some(row) = q.next()? {
            pending_inputs.insert(row.get(0)?, row.get(1)?);
        }
    }

    let mut pending_requests: HashMap<String, i64> = HashMap::new();
    {
        let mut stmt = db.prepare(
            "SELECT thread_id, COUNT(*) FROM provider_requests WHERE status IN ('pending','responding','') GROUP BY thread_id",
        )?;
        let mut q = stmt.query([])?;
        while let Some(row) = q.next()? {
            pending_requests.insert(row.get(0)?, row.get(1)?);
        }
    }

    let threads = raw_threads
        .into_iter()
        .map(|thread| {
            let st = stats.remove(&thread.id).unwrap_or_default();
            let updated_at = if !st.max_updated_at.is_empty() && st.max_updated_at > thread.created_at
            {
                st.max_updated_at
            } else {
                thread.created_at.clone()
            };
            let pi = pending_inputs.remove(&thread.id).unwrap_or(0);
            let pr = pending_requests.remove(&thread.id).unwrap_or(0);
            ThreadSummary {
                unread_sequence: thread.unread_sequence,
                project_path: thread.project_path,
                id: thread.id,
                title: thread.title,
                provider_id: thread.provider_id,
                model: thread.model,
                workspace: thread.workspace,
                parent_thread_id: thread.parent_thread_id,
                parent_task_id: thread.parent_task_id,
                created_by_thread_id: thread.created_by_thread_id,
                pinned: thread.pinned,
                archived: thread.archived,
                snoozed_until: thread.snoozed_until,
                approval_policy: thread.approval_policy,
                created_at: thread.created_at,
                updated_at,
                run_count: st.run_count,
                active_runs: st.active_runs,
                pending_inputs: pi,
                pending_requests: pr,
                latest_run_id: st.latest_run_id,
                latest_status: st.latest_status,
            }
        })
        .collect();

    Ok(ThreadSummaries {
        sequence: seq,
        threads: Some(threads),
    })
}

/// Scoped SQL query for `/v1/threads/detail` reading only the target thread's rows.
pub(crate) fn thread_detail(
    db: &Connection,
    thread_id: &str,
    limit_runs: i64,
    command_prefix: &str,
) -> Result<ThreadDetail> {
    let seq = sequence(db)?;
    let thread: Thread = match thread(db, thread_id) {
        Ok(t) => t,
        Err(crate::Error::Sql(rusqlite::Error::QueryReturnedNoRows)) => {
            return Err(crate::Error::Api(
                ErrorCode::NotFound,
                format!("thread {thread_id} not found"),
            ));
        }
        Err(e) => return Err(e),
    };
    let run_count: i64 = db.query_row(
        "SELECT COUNT(*) FROM runs WHERE thread_id=?",
        [thread_id],
        |r| r.get(0),
    )?;
    let (mut runs, truncated_runs) = if limit_runs > 0 && run_count > limit_runs {
        let mut tail: Vec<Run> = rows(
            db,
            "SELECT * FROM runs WHERE thread_id=? ORDER BY created_at DESC, id DESC LIMIT ?",
            params![thread_id, limit_runs],
        )?;
        tail.reverse();
        (tail, true)
    } else {
        (
            rows(
                db,
                "SELECT * FROM runs WHERE thread_id=? ORDER BY created_at, id",
                [thread_id],
            )?,
            false,
        )
    };
    for r in &mut runs {
        r.telemetry = crate::telemetry::read(db, &r.id)?;
    }
    let tasks = rows(
        db,
        "SELECT * FROM tasks WHERE parent_thread_id=? OR child_thread_id=? ORDER BY created_at, id",
        params![thread_id, thread_id],
    )?;
    let inputs = rows(
        db,
        "SELECT * FROM inputs WHERE thread_id=? ORDER BY created_at, id",
        [thread_id],
    )?;
    let tools = rows(
        db,
        "SELECT * FROM tool_calls WHERE thread_id=? ORDER BY updated_at, id",
        [thread_id],
    )?;
    let requests = rows(
        db,
        "SELECT * FROM provider_requests WHERE thread_id=? ORDER BY created_at, id",
        [thread_id],
    )?;
    let mut blocks: Vec<RunBlock> = rows(
        db,
        "SELECT * FROM run_blocks WHERE thread_id=? ORDER BY run_id, seq, id",
        [thread_id],
    )?;
    let mut send_command_ids = std::collections::BTreeMap::new();
    if !command_prefix.is_empty() {
        let mut stmt = db.prepare(
            "SELECT run_id,command_id FROM command_receipts WHERE command_id GLOB ?1
             AND thread_id=?2 AND kind='message.send' AND accepted=1 AND run_id<>''",
        )?;
        send_command_ids = stmt
            .query_map(params![format!("{command_prefix}*"), thread_id], |r| {
                Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?))
            })?
            .collect::<rusqlite::Result<_>>()?;
    }
    if truncated_runs {
        let keep: std::collections::HashSet<&str> = runs.iter().map(|r| r.id.as_str()).collect();
        blocks.retain(|b| keep.contains(b.run_id.as_str()));
        send_command_ids.retain(|id, _| keep.contains(id.as_str()));
    }
    let project_updates = rows(
        db,
        "SELECT * FROM project_updates ORDER BY source_sequence",
        [],
    )?;
    Ok(ThreadDetail {
        send_command_ids,
        project_updates,
        sequence: seq,
        thread,
        runs: Some(runs),
        tasks: Some(tasks),
        inputs: Some(inputs),
        tools: Some(tools),
        requests: Some(requests),
        blocks,
        run_count,
        truncated_runs,
    })
}
