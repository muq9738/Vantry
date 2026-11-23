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
pub(crate) fn thread(db: &Connection, key: &str) -> Result<Thread> {
    one(db, "SELECT * FROM threads WHERE id=?", [key])
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
    let r = run(db, key)?;
    event(db, kind, &r.thread_id, key, &r.attempt_id, json!({"run":r}))
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
    event(
        db,
        "input.updated",
        &i.thread_id,
        &i.run_id,
        &i.attempt_id,
        json!({"input":i}),
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
    event(
        db,
        "request.updated",
        &r.thread_id,
        &r.run_id,
        &r.attempt_id,
        json!({"request":r}),
    )?;
    Ok(())
}
pub(crate) fn snapshot(db: &Connection) -> Result<Snapshot> {
    let mut snap = Snapshot {
        sequence: sequence(db)?,
        threads: Some(rows(
            db,
            "SELECT * FROM threads ORDER BY created_at,id",
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
