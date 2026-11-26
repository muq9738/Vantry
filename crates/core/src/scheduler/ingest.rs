use super::*;
use serde_json::{Value, json};
fn text<'a>(v: &'a Value, key: &str) -> &'a str {
    v[key].as_str().unwrap_or_default()
}
pub(super) fn project(db: &Connection, r: &ProviderRequest, e: &ProviderEvent) -> Result<()> {
    let data = e.data.as_ref().unwrap_or(&Value::Null);
    match e.r#type.as_str() {
        "extension_ui_request" => dialog(db, r, e, data),
        "extension_ui_expired" => {
            let key = format!("{}:{}", r.attempt_id, text(data, "id"));
            if db.execute("UPDATE provider_requests SET status='expired',updated_at=? WHERE id=? AND status IN ('pending','responding')", params![now(), key])? > 0 {
                request_event(db, &key)?;
            }
            Ok(())
        }
        "control_result" => control_result(db, r, data),
        "tool_execution_start" | "tool_execution_update" | "tool_execution_end" => {
            tool(db, r, e, data)
        }
        "thinking_delta" => append(
            db,
            r,
            data.pointer("/assistantMessageEvent/delta")
                .and_then(Value::as_str)
                .unwrap_or(&e.delta),
            true,
        ),
        "message_start"
            if data.pointer("/message/role").and_then(Value::as_str) == Some("assistant") =>
        {
            crate::store::blocks::close_streaming_kind(db, &r.run_id, "reasoning")?;
            let (len, tail, tools): (i64, String, bool) = db.query_row(
                "SELECT length(CAST(output AS BLOB)),substr(output,-2),EXISTS(SELECT 1 FROM tool_calls
             WHERE run_id=runs.id AND output_offset=length(CAST(runs.output AS BLOB))) FROM runs
             WHERE id=?",
                [&r.run_id],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
            )?;
            if len > 0 && tail != "\n\n" && !tools {
                let sep = if tail.ends_with('\n') { "\n" } else { "\n\n" };
                append_separator(db, r, sep)?;
            }
            Ok(())
        }
        _ => append(db, r, &e.delta, false),
    }
}
fn append(db: &Connection, r: &ProviderRequest, delta: &str, reasoning: bool) -> Result<()> {
    if delta.is_empty() {
        return Ok(());
    }
    let (column, kind, block_kind, key) = if reasoning {
        ("reasoning", "run.reasoning", "reasoning", "reasoningDelta")
    } else {
        ("output", "run.output", "text", "delta")
    };
    db.execute(
        &format!("UPDATE runs SET {column}={column}||?,updated_at=? WHERE id=?"),
        params![delta, now(), r.run_id],
    )?;
    let (block_id, seq) = crate::store::blocks::append_stream(
        db,
        &r.thread_id,
        &r.run_id,
        &r.attempt_id,
        block_kind,
        delta,
    )?;
    event(
        db,
        kind,
        &r.thread_id,
        &r.run_id,
        &r.attempt_id,
        json!({(key): delta, "blockId": block_id, "seq": seq}),
    )?;
    Ok(())
}
fn append_separator(db: &Connection, r: &ProviderRequest, sep: &str) -> Result<()> {
    db.execute(
        "UPDATE runs SET output=output||?,updated_at=? WHERE id=?",
        params![sep, now(), r.run_id],
    )?;
    let data = match crate::store::blocks::append_open_text_separator(db, &r.run_id, sep)? {
        Some((block_id, seq)) => json!({"delta": sep, "blockId": block_id, "seq": seq}),
        None => json!({"delta": sep}),
    };
    event(db, "run.output", &r.thread_id, &r.run_id, &r.attempt_id, data)?;
    Ok(())
}
fn dialog(db: &Connection, r: &ProviderRequest, e: &ProviderEvent, d: &Value) -> Result<()> {
    let method = text(d, "method").to_ascii_lowercase();
    let source = text(d, "id");
    if source.is_empty() || !matches!(method.as_str(), "confirm" | "select" | "input" | "editor") {
        return Ok(());
    }
    let key = format!("{}:{source}", r.attempt_id);
    let nested = &d["params"];
    let field = |name: &str| first(text(d, name), text(nested, name));
    let options = d
        .get("options")
        .or_else(|| nested.get("options"))
        .cloned()
        .unwrap_or(json!([]));
    let timeout = d["timeout"]
        .as_i64()
        .or_else(|| nested["timeout"].as_i64())
        .unwrap_or_default();
    let occurred = chrono::DateTime::parse_from_rfc3339(&e.occurred_at)
        .ok()
        .and_then(|t| t.timestamp_nanos_opt())
        .unwrap_or_else(now);
    let expires = if timeout > 0 {
        occurred.saturating_add(timeout.saturating_mul(1_000_000))
    } else {
        0
    };
    if db.execute(
        "INSERT OR IGNORE INTO provider_requests(id,provider_request_id,thread_id,run_id,
             attempt_id,method,title,message,options,prefill,status,expires_at,created_at,
             updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'pending',?,?,?)",
        params![
            key,
            source,
            r.thread_id,
            r.run_id,
            r.attempt_id,
            method,
            first(field("title"), &method),
            field("message"),
            options.to_string(),
            field("prefill"),
            expires,
            now(),
            now()
        ],
    )? > 0
    {
        request_event(db, &key)?;
    }
    Ok(())
}
fn control_result(db: &Connection, r: &ProviderRequest, d: &Value) -> Result<()> {
    let key = text(d, "id");
    let status = match text(d, "status") {
        "delivered" => "delivered",
        "unknown" => "unknown",
        _ => "failed",
    };
    if db.execute(
        "UPDATE inputs SET status=?,error=?,updated_at=? WHERE id=? AND attempt_id=? AND kind='steer'
             AND status IN ('pending','unknown')",
        params![status, text(d, "error"), now(), key, r.attempt_id],
    )? > 0
    {
        input_event(db, key)?;
    } else if db.execute(
        "UPDATE provider_requests SET status=?,error=?,updated_at=? WHERE id=? AND attempt_id=?
             AND status IN ('pending','responding')",
        params![if status == "delivered" { "responded" } else { "failed" }, text(d, "error"), now(), key, r.attempt_id],
    )? > 0
    {
        request_event(db, key)?;
    }
    Ok(())
}
fn tool(db: &Connection, r: &ProviderRequest, e: &ProviderEvent, d: &Value) -> Result<()> {
    let call = first(text(d, "toolCallId"), text(d, "tool_call_id"));
    if call.is_empty() {
        return Ok(());
    }
    let key = format!("{}:{call}", r.attempt_id);
    let name = first(text(d, "toolName"), text(d, "tool_name"));
    let args = d
        .get("args")
        .or(d.get("arguments"))
        .filter(|v| !v.is_null())
        .map(serde_json::to_vec)
        .transpose()?
        .unwrap_or_default();
    let out = d
        .get("result")
        .filter(|v| !v.is_null())
        .or(d.get("partialResult"))
        .map(display_output)
        .unwrap_or_default();
    let status = match e.r#type.as_str() {
        "tool_execution_end" if d["isError"] == true => "failed",
        "tool_execution_end" => "completed",
        _ => "running",
    };
    db.execute(
        "INSERT INTO tool_calls(id,thread_id,run_id,attempt_id,name,arguments,output,status,
             updated_at,output_offset) SELECT ?,?,?,?,?,?,?,?,?,length(CAST(output AS BLOB)) FROM
             runs WHERE id=? ON CONFLICT(id) DO UPDATE SET status=excluded.status,
             updated_at=excluded.updated_at,name=COALESCE(NULLIF(excluded.name,''),tool_calls.name),
             arguments=CASE WHEN length(excluded.arguments)>0 THEN excluded.arguments ELSE
             tool_calls.arguments END,output=COALESCE(NULLIF(excluded.output,''),tool_calls.output)",
        params![key, r.thread_id, r.run_id, r.attempt_id, name, args, out, status, now(), r.run_id],
    )?;
    tool_event(db, &key)
}
fn display_output(value: &Value) -> String {
    let text = match value {
        Value::String(s) => s.clone(),
        Value::Null => return String::new(),
        _ => {
            let parts: String = value
                .get("content")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|p| p.get("text").and_then(Value::as_str))
                .collect();
            if parts.is_empty() {
                value.to_string()
            } else {
                parts
            }
        }
    };
    const LIMIT: usize = 32 * 1024;
    if text.len() <= LIMIT {
        return text;
    }
    let mut head = text.floor_char_boundary(12 * 1024);
    if let Some(idx) = text.as_bytes()[head - 256..head]
        .iter()
        .rposition(|&b| b == b'\n')
    {
        head = head - 256 + idx;
    }
    let budget = LIMIT - head - "\n…(9999999999 bytes truncated)…\n".len();
    let mut tail = text.ceil_char_boundary(text.len() - budget);
    if let Some(idx) = text.as_bytes()[tail..(tail + 256).min(text.len())]
        .iter()
        .position(|&b| b == b'\n')
    {
        if text.len() - tail - idx > 513 {
            tail += idx + 1;
        }
    }
    format!(
        "{}\n…({} bytes truncated)…\n{}",
        &text[..head],
        tail - head,
        &text[tail..]
    )
}
pub(super) fn tool_event(db: &Connection, key: &str) -> Result<()> {
    let t: ToolCall = one(db, "SELECT * FROM tool_calls WHERE id=?", [key])?;
    let block = crate::store::blocks::upsert_tool(db, &t)?;
    event(
        db,
        "tool.updated",
        &t.thread_id,
        &t.run_id,
        &t.attempt_id,
        json!({"tool": t, "block": block}),
    )?;
    Ok(())
}
