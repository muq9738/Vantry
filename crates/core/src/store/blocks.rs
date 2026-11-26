//! Durable causal timeline blocks (`run_blocks`) ordered by a single
//! monotonic sequence axis (`seq`) per run.
use crate::{Result, now};
use rusqlite::{Connection, OptionalExtension, params};
use vantry_protocol::{Input, PendingRequest, Run, RunBlock, RunStatus, ToolCall};

use super::projection::{one, rows};

pub(crate) fn next_seq(db: &Connection, run_id: &str) -> Result<i64> {
    Ok(db.query_row(
        "SELECT COALESCE(MAX(seq), 0) + 1 FROM run_blocks WHERE run_id=?",
        [run_id],
        |r| r.get(0),
    )?)
}

pub(crate) fn block(db: &Connection, id: &str) -> Result<RunBlock> {
    one(db, "SELECT * FROM run_blocks WHERE id=?", [id])
}

pub(crate) fn for_run(db: &Connection, run_id: &str) -> Result<Vec<RunBlock>> {
    rows(
        db,
        "SELECT * FROM run_blocks WHERE run_id=? ORDER BY seq, id",
        [run_id],
    )
}

pub(crate) fn close_streaming(db: &Connection, run_id: &str) -> Result<()> {
    db.execute(
        "UPDATE run_blocks SET status='completed', updated_at=? WHERE run_id=? AND status='streaming'",
        params![now(), run_id],
    )?;
    Ok(())
}

pub(crate) fn close_streaming_kind(db: &Connection, run_id: &str, kind: &str) -> Result<()> {
    db.execute(
        "UPDATE run_blocks SET status='completed', updated_at=? WHERE run_id=? AND kind=? AND status='streaming'",
        params![now(), run_id, kind],
    )?;
    Ok(())
}

pub(crate) fn clear_status(db: &Connection, run_id: &str) -> Result<()> {
    db.execute(
        "DELETE FROM run_blocks WHERE run_id=? AND kind='status'",
        [run_id],
    )?;
    Ok(())
}

pub(crate) fn record_user(db: &Connection, r: &Run) -> Result<Option<RunBlock>> {
    if r.input.is_empty() && r.attachments.is_empty() {
        return Ok(None);
    }
    let id = format!("user:{}", r.id);
    if let Some(existing) = db
        .query_row("SELECT id FROM run_blocks WHERE id=?", [&id], |row| {
            row.get::<_, String>(0)
        })
        .optional()?
    {
        return Ok(Some(block(db, &existing)?));
    }
    let seq = next_seq(db, &r.id)?;
    let ts = now();
    db.execute(
        "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
         status, error, attachments, created_at, updated_at)
         VALUES(?, ?, ?, ?, ?, 'user', '', ?, 'completed', '', ?, ?, ?)",
        params![
            id,
            r.thread_id,
            r.id,
            r.attempt_id,
            seq,
            r.input,
            serde_json::to_string(&r.attachments)?,
            ts,
            ts
        ],
    )?;
    Ok(Some(block(db, &id)?))
}

/// Appends `delta` to the run's trailing streaming block of `kind` (`"text"` or
/// `"reasoning"`), or closes any open streaming block and creates a new block
/// with the next monotonic `seq`.
pub(crate) fn append_stream(
    db: &Connection,
    thread_id: &str,
    run_id: &str,
    attempt_id: &str,
    kind: &str,
    delta: &str,
) -> Result<(String, i64)> {
    let ts = now();
    let tail: Option<(String, String, String, i64)> = db
        .query_row(
            "SELECT id, kind, status, seq FROM run_blocks WHERE run_id=? ORDER BY seq DESC LIMIT 1",
            [run_id],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
        )
        .optional()?;
    if let Some((id, tail_kind, status, seq)) = tail
        && tail_kind == kind
        && status == "streaming"
    {
        db.execute(
            "UPDATE run_blocks SET text=text||?, updated_at=? WHERE id=?",
            params![delta, ts, id],
        )?;
        return Ok((id, seq));
    }
    close_streaming(db, run_id)?;
    let seq = next_seq(db, run_id)?;
    let id = format!("{kind}:{run_id}:{seq}");
    db.execute(
        "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
         status, error, attachments, created_at, updated_at)
         VALUES(?, ?, ?, ?, ?, ?, '', ?, 'streaming', '', '[]', ?, ?)",
        params![id, thread_id, run_id, attempt_id, seq, kind, delta, ts, ts],
    )?;
    Ok((id, seq))
}

/// Appends a paragraph separator ONLY if the run's latest block is an open
/// streaming text block; returns `Some((block_id, seq))` if extended.
pub(crate) fn append_open_text_separator(
    db: &Connection,
    run_id: &str,
    sep: &str,
) -> Result<Option<(String, i64)>> {
    let tail: Option<(String, String, String, i64)> = db
        .query_row(
            "SELECT id, kind, status, seq FROM run_blocks WHERE run_id=? ORDER BY seq DESC LIMIT 1",
            [run_id],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
        )
        .optional()?;
    if let Some((id, kind, status, seq)) = tail
        && kind == "text"
        && status == "streaming"
    {
        db.execute(
            "UPDATE run_blocks SET text=text||?, updated_at=? WHERE id=?",
            params![sep, now(), id],
        )?;
        return Ok(Some((id, seq)));
    }
    Ok(None)
}

pub(crate) fn upsert_tool(db: &Connection, tool: &ToolCall) -> Result<RunBlock> {
    let ts = now();
    let existing: Option<String> = db
        .query_row(
            "SELECT id FROM run_blocks WHERE run_id=? AND kind='tool' AND ref_id=?",
            params![tool.run_id, tool.id],
            |row| row.get(0),
        )
        .optional()?;
    if let Some(id) = existing {
        db.execute(
            "UPDATE run_blocks SET status=?, updated_at=? WHERE id=?",
            params![tool.status, ts, id],
        )?;
        return block(db, &id);
    }
    close_streaming(db, &tool.run_id)?;
    let seq = next_seq(db, &tool.run_id)?;
    let id = format!("tool:{}", tool.id);
    db.execute(
        "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
         status, error, attachments, created_at, updated_at)
         VALUES(?, ?, ?, ?, ?, 'tool', ?, '', ?, '', '[]', ?, ?)",
        params![
            id,
            tool.thread_id,
            tool.run_id,
            tool.attempt_id,
            seq,
            tool.id,
            tool.status,
            ts,
            ts
        ],
    )?;
    block(db, &id)
}

pub(crate) fn upsert_steer(db: &Connection, input: &Input) -> Result<Option<RunBlock>> {
    if input.kind != "steer"
        || input.run_id.is_empty()
        || !matches!(input.status.as_str(), "delivered" | "failed")
    {
        return Ok(None);
    }
    let ts = now();
    let existing: Option<String> = db
        .query_row(
            "SELECT id FROM run_blocks WHERE run_id=? AND kind='steer' AND ref_id=?",
            params![input.run_id, input.id],
            |row| row.get(0),
        )
        .optional()?;
    if let Some(id) = existing {
        db.execute(
            "UPDATE run_blocks SET status=?, error=?, updated_at=? WHERE id=?",
            params![input.status, input.error, ts, id],
        )?;
        return Ok(Some(block(db, &id)?));
    }
    close_streaming(db, &input.run_id)?;
    let seq = next_seq(db, &input.run_id)?;
    let id = format!("steer:{}", input.id);
    db.execute(
        "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
         status, error, attachments, created_at, updated_at)
         VALUES(?, ?, ?, ?, ?, 'steer', ?, ?, ?, ?, ?, ?, ?)",
        params![
            id,
            input.thread_id,
            input.run_id,
            input.attempt_id,
            seq,
            input.id,
            input.message,
            input.status,
            input.error,
            serde_json::to_string(&input.attachments)?,
            ts,
            ts
        ],
    )?;
    Ok(Some(block(db, &id)?))
}

pub(crate) fn upsert_approval(db: &Connection, req: &PendingRequest) -> Result<RunBlock> {
    let ts = now();
    let existing: Option<String> = db
        .query_row(
            "SELECT id FROM run_blocks WHERE run_id=? AND kind='approval' AND ref_id=?",
            params![req.run_id, req.id],
            |row| row.get(0),
        )
        .optional()?;
    if let Some(id) = existing {
        db.execute(
            "UPDATE run_blocks SET status=?, error=?, updated_at=? WHERE id=?",
            params![req.status, req.error, ts, id],
        )?;
        return block(db, &id);
    }
    close_streaming(db, &req.run_id)?;
    let seq = next_seq(db, &req.run_id)?;
    let id = format!("request:{}", req.id);
    db.execute(
        "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
         status, error, attachments, created_at, updated_at)
         VALUES(?, ?, ?, ?, ?, 'approval', ?, '', ?, ?, '[]', ?, ?)",
        params![
            id,
            req.thread_id,
            req.run_id,
            req.attempt_id,
            seq,
            req.id,
            req.status,
            req.error,
            ts,
            ts
        ],
    )?;
    block(db, &id)
}

pub(crate) fn set_status(
    db: &Connection,
    r: &Run,
    status: &str,
    error: &str,
) -> Result<Option<RunBlock>> {
    clear_status(db, &r.id)?;
    if !matches!(status, "waiting_children" | "cancelled" | "interrupted" | "failed") {
        return Ok(None);
    }
    let seq = next_seq(db, &r.id)?;
    let ts = now();
    let id = format!("status:{}", r.id);
    db.execute(
        "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
         status, error, attachments, created_at, updated_at)
         VALUES(?, ?, ?, ?, ?, 'status', '', '', ?, ?, '[]', ?, ?)",
        params![id, r.thread_id, r.id, r.attempt_id, seq, status, error, ts, ts],
    )?;
    Ok(Some(block(db, &id)?))
}

/// Copies content blocks (`user`, `reasoning`, `text`, `steer`, `status`) when
/// forking a settled run into a new thread.
pub(crate) fn fork_run_blocks(
    db: &Connection,
    source_run_id: &str,
    target_thread_id: &str,
    target_run_id: &str,
    target_attempt_id: &str,
) -> Result<Vec<RunBlock>> {
    let mut stmt = db.prepare(
        "SELECT kind, text, status, error, attachments, created_at, updated_at
         FROM run_blocks
         WHERE run_id=? AND kind IN ('user', 'reasoning', 'text', 'steer', 'status')
         ORDER BY seq, id",
    )?;
    let source_rows: Vec<(String, String, String, String, String, i64, i64)> = stmt
        .query_map([source_run_id], |row| {
            Ok((
                row.get(0)?,
                row.get(1)?,
                row.get(2)?,
                row.get(3)?,
                row.get(4)?,
                row.get(5)?,
                row.get(6)?,
            ))
        })?
        .collect::<rusqlite::Result<_>>()?;
    let mut seq = 0i64;
    for (kind, text, status, error, attachments, created_at, updated_at) in source_rows {
        seq += 1;
        let id = match kind.as_str() {
            "user" => format!("user:{target_run_id}"),
            "status" => format!("status:{target_run_id}"),
            other => format!("{other}:{target_run_id}:{seq}"),
        };
        db.execute(
            "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
             status, error, attachments, created_at, updated_at)
             VALUES(?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)",
            params![
                id,
                target_thread_id,
                target_run_id,
                target_attempt_id,
                seq,
                kind,
                text,
                status,
                error,
                attachments,
                created_at,
                updated_at
            ],
        )?;
    }
    for_run(db, target_run_id)
}

fn floor_char_boundary(text: &str, index: usize) -> usize {
    if index >= text.len() {
        return text.len();
    }
    let mut i = index;
    while !text.is_char_boundary(i) {
        i -= 1;
    }
    i
}

/// One-time v11 -> v12 backfill: synthesizes `run_blocks` for historical runs
/// created before `run_blocks` existed.
pub(crate) fn backfill(db: &Connection) -> Result<()> {
    let run_ids: Vec<String> = db
        .prepare(
            "SELECT id FROM runs r
             WHERE NOT EXISTS(SELECT 1 FROM run_blocks b WHERE b.run_id = r.id)
             ORDER BY created_at, id",
        )?
        .query_map([], |row| row.get(0))?
        .collect::<rusqlite::Result<_>>()?;
    for run_id in run_ids {
        let (thread_id, attempt_id, status_str, input_text, output_text, reasoning_text, error_text, attachments_json, created_at, updated_at): (
            String,
            String,
            String,
            String,
            String,
            String,
            String,
            String,
            i64,
            i64,
        ) = db.query_row(
            "SELECT thread_id, attempt_id, status, input, output, reasoning, error, attachments, created_at, updated_at
             FROM runs WHERE id=?",
            [&run_id],
            |row| {
                Ok((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                    row.get(5)?,
                    row.get(6)?,
                    row.get(7)?,
                    row.get(8)?,
                    row.get(9)?,
                ))
            },
        )?;
        let status: RunStatus =
            serde_json::from_value(serde_json::Value::String(status_str.clone()))
                .unwrap_or_default();
        let executing = matches!(status, RunStatus::Queued | RunStatus::Running);
        let mut seq = 0i64;
        let has_attachments = !attachments_json.is_empty() && attachments_json != "[]";
        if !input_text.is_empty() || has_attachments {
            seq += 1;
            db.execute(
                "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                 status, error, attachments, created_at, updated_at)
                 VALUES(?, ?, ?, ?, ?, 'user', '', ?, 'completed', '', ?, ?, ?)",
                params![
                    format!("user:{run_id}"),
                    thread_id,
                    run_id,
                    attempt_id,
                    seq,
                    input_text,
                    if attachments_json.is_empty() { "[]" } else { &attachments_json },
                    created_at,
                    created_at
                ],
            )?;
        }
        if !reasoning_text.is_empty() {
            seq += 1;
            let r_status = if executing && output_text.is_empty() {
                "streaming"
            } else {
                "completed"
            };
            db.execute(
                "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                 status, error, attachments, created_at, updated_at)
                 VALUES(?, ?, ?, ?, ?, 'reasoning', '', ?, ?, '', '[]', ?, ?)",
                params![
                    format!("reasoning:{run_id}:{seq}"),
                    thread_id,
                    run_id,
                    attempt_id,
                    seq,
                    reasoning_text,
                    r_status,
                    created_at,
                    updated_at
                ],
            )?;
        }

        // Legacy tools sorted by output_offset (unknown offsets last).
        let mut tools: Vec<(String, String, String, i64, i64)> = db
            .prepare(
                "SELECT id, attempt_id, status, output_offset, updated_at
                 FROM tool_calls WHERE run_id=? ORDER BY updated_at, id",
            )?
            .query_map([&run_id], |row| {
                Ok((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                ))
            })?
            .collect::<rusqlite::Result<_>>()?;
        let offset_of = |offset: i64| -> usize {
            if offset < 0 {
                output_text.len()
            } else {
                floor_char_boundary(&output_text, offset as usize)
            }
        };
        tools.sort_by_key(|t| offset_of(t.3));

        let mut cursor = 0usize;
        for (tool_id, tool_att, tool_status, tool_offset, tool_updated) in &tools {
            let at = offset_of(*tool_offset).max(cursor);
            let slice = &output_text[cursor..at];
            if !slice.trim().is_empty() {
                seq += 1;
                let id = format!("text:{run_id}:{seq}");
                db.execute(
                    "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                     status, error, attachments, created_at, updated_at)
                     VALUES(?, ?, ?, ?, ?, 'text', '', ?, 'completed', '', '[]', ?, ?)",
                    params![id, thread_id, run_id, attempt_id, seq, slice, created_at, updated_at],
                )?;
            }
            cursor = at;
            seq += 1;
            db.execute(
                "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                 status, error, attachments, created_at, updated_at)
                 VALUES(?, ?, ?, ?, ?, 'tool', ?, '', ?, '', '[]', ?, ?)",
                params![
                    format!("tool:{tool_id}"),
                    thread_id,
                    run_id,
                    tool_att,
                    seq,
                    tool_id,
                    tool_status,
                    tool_updated,
                    tool_updated
                ],
            )?;
        }
        let tail_slice = &output_text[cursor..];
        if !tail_slice.trim().is_empty() {
            seq += 1;
            let id = format!("text:{run_id}:{seq}");
            let t_status = if executing { "streaming" } else { "completed" };
            db.execute(
                "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                 status, error, attachments, created_at, updated_at)
                 VALUES(?, ?, ?, ?, ?, 'text', '', ?, ?, '', '[]', ?, ?)",
                params![
                    id,
                    thread_id,
                    run_id,
                    attempt_id,
                    seq,
                    tail_slice,
                    t_status,
                    created_at,
                    updated_at
                ],
            )?;
        }

        // Delivered/failed steers for this run.
        let steers: Vec<(String, String, String, String, String, String, i64, i64)> = db
            .prepare(
                "SELECT id, attempt_id, message, status, error, attachments, created_at, updated_at
                 FROM inputs
                 WHERE run_id=? AND kind='steer' AND status IN ('delivered', 'failed')
                 ORDER BY created_at, id",
            )?
            .query_map([&run_id], |row| {
                Ok((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                    row.get(5)?,
                    row.get(6)?,
                    row.get(7)?,
                ))
            })?
            .collect::<rusqlite::Result<_>>()?;
        for (inp_id, inp_att, msg, inp_status, inp_err, inp_att_json, inp_created, inp_updated) in
            steers
        {
            seq += 1;
            db.execute(
                "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                 status, error, attachments, created_at, updated_at)
                 VALUES(?, ?, ?, ?, ?, 'steer', ?, ?, ?, ?, ?, ?, ?)",
                params![
                    format!("steer:{inp_id}"),
                    thread_id,
                    run_id,
                    inp_att,
                    seq,
                    inp_id,
                    msg,
                    inp_status,
                    inp_err,
                    if inp_att_json.is_empty() { "[]" } else { &inp_att_json },
                    inp_created,
                    inp_updated
                ],
            )?;
        }

        // Provider approval requests for this run.
        let reqs: Vec<(String, String, String, String, i64, i64)> = db
            .prepare(
                "SELECT id, attempt_id, status, error, created_at, updated_at
                 FROM provider_requests WHERE run_id=? ORDER BY created_at, id",
            )?
            .query_map([&run_id], |row| {
                Ok((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                    row.get(5)?,
                ))
            })?
            .collect::<rusqlite::Result<_>>()?;
        for (req_id, req_att, req_status, req_err, req_created, req_updated) in reqs {
            seq += 1;
            db.execute(
                "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                 status, error, attachments, created_at, updated_at)
                 VALUES(?, ?, ?, ?, ?, 'approval', ?, '', ?, ?, '[]', ?, ?)",
                params![
                    format!("request:{req_id}"),
                    thread_id,
                    run_id,
                    req_att,
                    seq,
                    req_id,
                    req_status,
                    req_err,
                    req_created,
                    req_updated
                ],
            )?;
        }

        if matches!(
            status_str.as_str(),
            "waiting_children" | "cancelled" | "interrupted" | "failed"
        ) {
            seq += 1;
            db.execute(
                "INSERT INTO run_blocks(id, thread_id, run_id, attempt_id, seq, kind, ref_id, text,
                 status, error, attachments, created_at, updated_at)
                 VALUES(?, ?, ?, ?, ?, 'status', '', '', ?, ?, '[]', ?, ?)",
                params![
                    format!("status:{run_id}"),
                    thread_id,
                    run_id,
                    attempt_id,
                    seq,
                    status_str,
                    error_text,
                    updated_at,
                    updated_at
                ],
            )?;
        }
    }
    Ok(())
}
