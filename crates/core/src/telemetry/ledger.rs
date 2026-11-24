use crate::{Core, Result, invalid, now, timestamp};
use rusqlite::{Connection, OptionalExtension, params};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    io::{ErrorKind, Write},
    path::Path,
};
use vantry_protocol::{ProviderRequest, RunTelemetry};

#[derive(Default, Debug, Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", default)]
pub struct UsageRecord {
    pub run_id: String,
    pub attempt_id: String,
    pub thread_id: String,
    pub provider_id: String,
    pub model: String,
    pub started_at: String,
    pub completed_at: String,
    #[serde(flatten)]
    pub metrics: RunTelemetry,
    pub cost: Cost,
}
#[derive(Default, Debug, Clone, Deserialize, Serialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Cost {
    pub input: f64,
    pub output: f64,
    pub cache_read: f64,
    pub cache_write: f64,
    pub total: f64,
}
#[derive(Default, Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct UsageSummary {
    pub run_count: usize,
    pub attempt_count: usize,
    pub total_duration_ms: i64,
    pub total_cost_usd: f64,
    pub total_tokens: i64,
    pub input_tokens: i64,
    pub output_tokens: i64,
    pub cache_read_tokens: i64,
    pub cache_write_tokens: i64,
    pub reasoning_tokens: i64,
    pub turn_count: i64,
    pub avg_ttft_ms: i64,
    pub cache_hit_rate: f64,
}
impl Core {
    pub fn usage_summary(&self, thread_id: Option<&str>) -> Result<UsageSummary> {
        self.read(|db| {
            let mut sum = UsageSummary::default();
            let mut runs = BTreeSet::new();
            let mut ttft = 0;
            let mut measured = 0;
            for record in records(db, None)? {
                if thread_id.is_some_and(|id| record.thread_id != id) {
                    continue;
                }
                let m = record.metrics;
                runs.insert(record.run_id);
                sum.attempt_count += 1;
                sum.total_cost_usd += m.cost_usd;
                sum.total_duration_ms += m.duration_ms;
                sum.input_tokens += m.input_tokens;
                sum.output_tokens += m.output_tokens;
                sum.cache_read_tokens += m.cache_read_tokens;
                sum.cache_write_tokens += m.cache_write_tokens;
                sum.reasoning_tokens += m.reasoning_tokens;
                sum.total_tokens += m.total_tokens;
                sum.turn_count += m.turn_count;
                if m.ttft_ms > 0 {
                    ttft += m.ttft_ms;
                    measured += 1;
                }
            }
            sum.run_count = runs.len();
            if measured > 0 {
                sum.avg_ttft_ms = ttft / measured;
            }
            let prompt = sum.input_tokens + sum.cache_read_tokens + sum.cache_write_tokens;
            if prompt > 0 {
                sum.cache_hit_rate = sum.cache_read_tokens as f64 / prompt as f64;
            }
            Ok(sum)
        })
    }
}
fn prefix(run: &str) -> String {
    format!("usage.record.{}:", json!(run))
}
fn key(run: &str, attempt: &str) -> String {
    format!("{}{}", prefix(run), json!(attempt))
}
fn records(db: &Connection, run: Option<&str>) -> Result<Vec<UsageRecord>> {
    let lower = run.map(prefix).unwrap_or_else(|| "usage.record.".into());
    let upper = format!("{lower}\u{10ffff}");
    let mut stmt =
        db.prepare("SELECT text_value FROM core_metadata WHERE key>=? AND key<? ORDER BY rowid")?;
    let raw = stmt.query_map(params![lower, upper], |r| r.get::<_, String>(0))?;
    let mut result = Vec::new();
    for r in raw {
        result.push(serde_json::from_str(&r?)?);
    }
    Ok(result)
}
pub(super) fn read(db: &Connection, run: &str) -> Result<Option<RunTelemetry>> {
    let mut iter = records(db, Some(run))?.into_iter();
    let Some(first) = iter.next() else {
        return Ok(None);
    };
    let mut total = first.metrics;
    for r in iter {
        let m = r.metrics;
        total.duration_ms += m.duration_ms;
        total.cost_usd += m.cost_usd;
        total.cost_estimated |= m.cost_estimated;
        total.input_tokens += m.input_tokens;
        total.output_tokens += m.output_tokens;
        total.cache_read_tokens += m.cache_read_tokens;
        total.cache_write_tokens += m.cache_write_tokens;
        total.reasoning_tokens += m.reasoning_tokens;
        total.total_tokens += m.total_tokens;
        total.retry_count += m.retry_count;
        total.turn_count += m.turn_count;
        if m.last_context_tokens > 0 {
            total.last_context_tokens = m.last_context_tokens;
        }
        if total.ttft_ms == 0 {
            total.ttft_ms = m.ttft_ms;
        }
        total.phase = m.phase;
    }
    Ok(Some(total))
}
pub(super) fn record(db: &Connection, r: &ProviderRequest) -> Result<UsageRecord> {
    let raw: Option<String> = db
        .query_row(
            "SELECT text_value FROM core_metadata WHERE key=?",
            [key(&r.run_id, &r.attempt_id)],
            |r| r.get(0),
        )
        .optional()?;
    if let Some(raw) = raw {
        return Ok(serde_json::from_str(&raw)?);
    }
    let start: i64 = db.query_row(
        "SELECT created_at FROM attempts WHERE id=?",
        [&r.attempt_id],
        |r| r.get(0),
    )?;
    Ok(UsageRecord {
        run_id: r.run_id.clone(),
        attempt_id: r.attempt_id.clone(),
        thread_id: r.thread_id.clone(),
        provider_id: r.provider_id.clone(),
        model: r.model.clone(),
        started_at: timestamp(start),
        ..Default::default()
    })
}
pub(super) fn persist(db: &Connection, r: &UsageRecord, finalized: bool) -> Result<()> {
    db.execute("INSERT INTO core_metadata(key,text_value,int_value) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET text_value=excluded.text_value,int_value=excluded.int_value", params![key(&r.run_id, &r.attempt_id), serde_json::to_string(r)?, finalized])?;
    if finalized {
        db.execute(
            "INSERT OR REPLACE INTO core_metadata(key,int_value) VALUES('usage.export.pending',1)",
            [],
        )?;
    }
    Ok(())
}
fn load_file(directory: &Path) -> Result<Value> {
    match fs::read(directory.join("usage-ledger.json")) {
        Ok(data) if !data.iter().all(u8::is_ascii_whitespace) => Ok(serde_json::from_slice(&data)?),
        Ok(_) => Ok(json!({"version":1,"records":[]})),
        Err(e) if e.kind() == ErrorKind::NotFound => Ok(json!({"version":1,"records":[]})),
        Err(e) => Err(e.into()),
    }
}
pub(crate) fn import(db: &mut Connection, dir: &Path) -> Result<()> {
    let ledger = load_file(dir)?;
    let tx = db.transaction()?;
    if let Some(items) = ledger["records"].as_array() {
        for value in items {
            let mut r: UsageRecord = serde_json::from_value(value.clone())?;
            if r.run_id.is_empty() {
                continue;
            }
            if r.metrics.cost_usd == 0. {
                r.metrics.cost_usd = r.cost.total;
            }
            if r.metrics.total_tokens == 0 {
                r.metrics.total_tokens = r.metrics.input_tokens
                    + r.metrics.output_tokens
                    + r.metrics.cache_read_tokens
                    + r.metrics.cache_write_tokens;
            }
            let key = key(&r.run_id, &r.attempt_id);
            tx.execute("INSERT INTO core_metadata(key,text_value,int_value) VALUES(?,?,0) ON CONFLICT(key) DO UPDATE SET text_value=excluded.text_value WHERE core_metadata.int_value=0", params![key, serde_json::to_string(&r)?])?;
        }
    }
    tx.commit()?;
    flush(db, dir)
}
/// SQLite owns unexported records. If interrupted after rename, replay upserts
/// the same (runId,attemptId) in the Go ledger and then clears the pending bit.
pub(crate) fn flush(db: &Connection, dir: &Path) -> Result<()> {
    let dirty: bool = db.query_row(
        "SELECT EXISTS(SELECT 1 FROM core_metadata WHERE key='usage.export.pending')",
        [],
        |r| r.get(0),
    )?;
    if !dirty {
        return Ok(());
    }
    let mut ledger = load_file(dir)?;
    if ledger["records"].is_null() {
        ledger["records"] = json!([]);
    }
    let list = ledger["records"]
        .as_array_mut()
        .ok_or_else(|| invalid("invalid usage ledger records"))?;
    let mut positions = BTreeMap::new();
    for (index, r) in list.iter().enumerate() {
        positions.insert(
            key(
                r["runId"].as_str().unwrap_or_default(),
                r["attemptId"].as_str().unwrap_or_default(),
            ),
            index,
        );
    }
    let mut stmt = db.prepare(
        "SELECT key,text_value FROM core_metadata WHERE key GLOB 'usage.record.*' AND int_value=1",
    )?;
    let mut rows = stmt.query([])?;
    while let Some(row) = rows.next()? {
        let key: String = row.get(0)?;
        let raw: String = row.get(1)?;
        let v = serde_json::from_str(&raw)?;
        if let Some(index) = positions.get(&key) {
            list[*index] = v;
        } else {
            list.push(v);
        }
    }
    drop(rows);
    drop(stmt);
    ledger["updatedAt"] = json!(timestamp(now()));
    let tmp = dir.join(format!(".usage-ledger-{}.tmp", uuid::Uuid::new_v4()));
    let write = (|| -> Result<()> {
        let mut opts = fs::File::options();
        opts.create_new(true).write(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            opts.mode(0o600);
        }
        let mut file = opts.open(&tmp)?;
        serde_json::to_writer(&mut file, &ledger)?;
        file.write_all(b"\n")?;
        file.sync_all()?;
        drop(file);
        fs::rename(&tmp, dir.join("usage-ledger.json"))?;
        Ok(())
    })();
    if write.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    write?;
    let tx = db.unchecked_transaction()?;
    tx.execute(
        "UPDATE core_metadata SET int_value=0 WHERE key GLOB 'usage.record.*' AND int_value=1",
        [],
    )?;
    tx.execute(
        "DELETE FROM core_metadata WHERE key='usage.export.pending'",
        [],
    )?;
    tx.commit()?;
    Ok(())
}
