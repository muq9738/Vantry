use crate::{Result, now, store::projection::event};
use rusqlite::Connection;
use serde_json::{Value, json};
use vantry_protocol::{ProviderEvent, ProviderRequest, RunTelemetry};
mod ledger;
mod pricing;
pub use ledger::{UsageRecord, UsageSummary};
pub(crate) use ledger::{flush, import};
pub(crate) fn read(db: &Connection, run: &str) -> Result<Option<RunTelemetry>> {
    ledger::read(db, run)
}
fn count(v: &Value, paths: &[&str]) -> i64 {
    paths
        .iter()
        .find_map(|p| v.pointer(p).and_then(Value::as_i64))
        .unwrap_or_default()
        .max(0)
}
/// Normalize a completion's usage. Reasoning is part of output and OpenAI's
/// cached prompt tokens are already included in prompt_tokens.
pub fn add_turn(t: &mut RunTelemetry, v: &Value) {
    let i = count(v, &["/input", "/input_tokens", "/prompt_tokens"]);
    let o = count(v, &["/output", "/output_tokens", "/completion_tokens"]);
    let cr = count(
        v,
        &[
            "/cacheRead",
            "/cache_read_input_tokens",
            "/cached_tokens",
            "/prompt_cache_hit_tokens",
            "/prompt_tokens_details/cached_tokens",
            "/input_tokens_details/cached_tokens",
        ],
    );
    let cw = count(
        v,
        &[
            "/cacheWrite",
            "/cache_creation_input_tokens",
            "/cache_write_input_tokens",
        ],
    );
    let mut total = count(v, &["/totalTokens", "/total_tokens"]);
    if total == 0 {
        let included = v.get("input").is_none()
            && v.get("cacheRead").is_none()
            && v.get("cache_read_input_tokens").is_none()
            && [
                "prompt_tokens",
                "input_tokens_details",
                "prompt_tokens_details",
                "prompt_cache_hit_tokens",
            ]
            .iter()
            .any(|k| v.get(k).is_some());
        total = i + o + cw + if included { 0 } else { cr };
    }
    t.input_tokens += i;
    t.output_tokens += o;
    t.cache_read_tokens += cr;
    t.cache_write_tokens += cw;
    t.total_tokens += total;
    t.reasoning_tokens += count(
        v,
        &[
            "/reasoning",
            "/reasoning_tokens",
            "/completion_tokens_details/reasoning_tokens",
            "/output_tokens_details/reasoning_tokens",
        ],
    );
    t.last_context_tokens = total;
    t.turn_count += 1;
    let cost = v
        .pointer("/cost/total")
        .and_then(Value::as_f64)
        .filter(|x| *x != 0.)
        .or_else(|| {
            v.get("costUsd")
                .or(v.get("total_cost_usd"))
                .and_then(Value::as_f64)
        })
        .unwrap_or_else(|| {
            ["input", "output", "cacheRead", "cacheWrite"]
                .iter()
                .filter_map(|k| v["cost"][k].as_f64())
                .sum()
        });
    t.cost_usd += cost;
}
pub(crate) fn observe(db: &Connection, r: &ProviderRequest, e: &ProviderEvent) -> Result<()> {
    let data = e.data.as_ref().unwrap_or(&Value::Null);
    let kind = e.r#type.as_str();
    let turn_key = format!("usage.turn.{}", r.attempt_id);
    if matches!(kind, "turn_start" | "turn_end" | "message_start") {
        let cleared = db.execute("DELETE FROM core_metadata WHERE key=?", [&turn_key])?;
        if kind == "turn_end" && cleared > 0 {
            return Ok(());
        }
    }
    let usage = if matches!(
        kind,
        "message_end"
            | "turn_end"
            | "usage"
            | "compaction_end"
            | "result"
            | "message_delta"
            | "response.completed"
    ) {
        data.pointer("/message/usage")
            .or_else(|| data.pointer("/result/usage"))
            .or_else(|| data.pointer("/response/usage"))
            .or(data.get("usage"))
            .filter(|v| v.is_object())
    } else {
        None
    };
    let token = !e.delta.is_empty() || matches!(kind, "thinking_delta" | "text_delta");
    if usage.is_none() && !token {
        return Ok(());
    }
    let mut record = ledger::record(db, r)?;
    if record.metrics.ttft_ms > 0 && usage.is_none() {
        return Ok(());
    }
    let elapsed = elapsed_ms(
        &record.started_at,
        chrono::DateTime::parse_from_rfc3339(&e.occurred_at)
            .ok()
            .and_then(|t| t.timestamp_nanos_opt())
            .unwrap_or_else(now),
    );
    record.metrics.duration_ms = elapsed;
    if record.metrics.ttft_ms == 0 && token {
        record.metrics.ttft_ms = elapsed.max(1);
    }
    if let Some(v) = usage {
        if kind == "message_end" {
            db.execute(
                "INSERT OR REPLACE INTO core_metadata(key,int_value) VALUES(?,1)",
                [turn_key],
            )?;
        }
        let response_id = data
            .pointer("/message/responseId")
            .or_else(|| data.pointer("/message/id"))
            .or_else(|| data.pointer("/response/id"))
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty());
        if let Some(response_id) = response_id {
            let changed = db.execute("INSERT INTO core_metadata(key,text_value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET text_value=excluded.text_value WHERE text_value<>excluded.text_value", rusqlite::params![format!("usage.response.{}", r.attempt_id), response_id])?;
            if changed == 0 {
                return Ok(());
            }
        }
        let before = record.metrics.turn_count;
        let mut reported = v.clone();
        if reported.get("cost").is_none()
            && reported.get("total_cost_usd").is_none()
            && reported.get("costUsd").is_none()
        {
            if let Some(total) = data
                .get("total_cost_usd")
                .or(data.get("costUsd"))
                .or_else(|| data.pointer("/result/total_cost_usd"))
                .and_then(Value::as_f64)
            {
                reported["costUsd"] = json!(total);
            } else {
                let model = data
                    .pointer("/message/responseModel")
                    .or_else(|| data.pointer("/message/model"))
                    .and_then(Value::as_str)
                    .unwrap_or(&r.model)
                    .to_lowercase();
                if let Some((_, rate)) =
                    pricing::RATES.iter().find(|(name, _)| model.contains(name))
                {
                    let mut turn = RunTelemetry::default();
                    add_turn(&mut turn, v);
                    reported["cost"] = json!({"input":turn.input_tokens as f64*rate[0]/1e6,"output":turn.output_tokens as f64*rate[1]/1e6,"cacheRead":turn.cache_read_tokens as f64*rate[2]/1e6,"cacheWrite":turn.cache_write_tokens as f64*rate[3]/1e6});
                    record.metrics.cost_estimated = true;
                }
            }
        }
        let v = &reported;
        add_turn(&mut record.metrics, v);
        if kind == "compaction_end" {
            record.metrics.turn_count = before;
        }
        let c = &v["cost"];
        record.cost.input += c["input"].as_f64().unwrap_or_default();
        record.cost.output += c["output"].as_f64().unwrap_or_default();
        record.cost.cache_read += c
            .get("cacheRead")
            .or(c.get("cache_read"))
            .and_then(Value::as_f64)
            .unwrap_or_default();
        record.cost.cache_write += c
            .get("cacheWrite")
            .or(c.get("cache_write"))
            .and_then(Value::as_f64)
            .unwrap_or_default();
        record.cost.total = record.metrics.cost_usd;
    }
    ledger::persist(db, &record, false)?;
    event(
        db,
        "run.telemetry",
        &r.thread_id,
        &r.run_id,
        &r.attempt_id,
        json!({"telemetry":record.metrics}),
    )?;
    Ok(())
}
fn elapsed_ms(start: &str, end: i64) -> i64 {
    chrono::DateTime::parse_from_rfc3339(start)
        .ok()
        .and_then(|t| t.timestamp_nanos_opt())
        .map_or(0, |n| end.saturating_sub(n).max(0) / 1_000_000)
}
pub(crate) fn finish(db: &Connection, r: &ProviderRequest, status: &str) -> Result<()> {
    let mut record = ledger::record(db, r)?;
    if record.metrics.turn_count == 0
        && record.metrics.total_tokens == 0
        && record.metrics.ttft_ms == 0
    {
        return Ok(());
    }
    let end = now();
    record.completed_at = crate::timestamp(end);
    record.metrics.duration_ms = elapsed_ms(&record.started_at, end);
    record.metrics.phase = status.into();
    ledger::persist(db, &record, true)
}
