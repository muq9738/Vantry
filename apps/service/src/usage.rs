use anyhow::Result;
use serde::Serialize;
use serde_json::json;
use std::collections::BTreeMap;
use vantry_core::Core;
use vantry_protocol::RunStatus;

#[derive(Default, Serialize)]
#[serde(rename_all = "camelCase")]
struct ModelUsage {
    model: String,
    runs: usize,
    completed: usize,
    failed: usize,
    input_chars: usize,
    output_chars: usize,
    input_tokens: i64,
    output_tokens: i64,
    cache_read_tokens: i64,
    total_tokens: i64,
    cost_usd: f64,
    avg_ttft_ms: i64,
    cache_hit_rate: f64,
    #[serde(skip)]
    measured: i64,
}
pub fn summary(core: &Core, default_model: &str, thread: &str) -> Result<String> {
    let snapshot = core.snapshot()?;
    let selected = |id: &str| thread.is_empty() || thread == id;
    let totals = core.usage_summary((!thread.is_empty()).then_some(thread))?;
    let mut models = BTreeMap::new();
    let (mut active, mut completed, mut failed, mut count) = (0, 0, 0, 0);
    for run in snapshot
        .runs
        .iter()
        .flatten()
        .filter(|r| selected(&r.thread_id))
    {
        let key = if !run.model.is_empty() {
            &run.model
        } else if !default_model.is_empty() {
            default_model
        } else {
            "pi/default"
        };
        let row = models.entry(key).or_insert_with(|| ModelUsage {
            model: key.into(),
            ..Default::default()
        });
        count += 1;
        row.runs += 1;
        row.input_chars += run.input.len();
        row.output_chars += run.output.len();
        match run.status {
            RunStatus::Completed => {
                row.completed += 1;
                completed += 1
            }
            RunStatus::Failed => {
                row.failed += 1;
                failed += 1
            }
            status if !status.terminal() => active += 1,
            _ => {}
        }
        if let Some(m) = &run.telemetry {
            row.input_tokens += m.input_tokens;
            row.output_tokens += m.output_tokens;
            row.cache_read_tokens += m.cache_read_tokens;
            row.total_tokens += m.total_tokens;
            row.cost_usd += m.cost_usd;
            if m.ttft_ms > 0 {
                row.avg_ttft_ms += m.ttft_ms;
                row.measured += 1;
            }
        }
    }
    let models: Vec<_> = models
        .into_values()
        .map(|mut row| {
            if row.measured > 0 {
                row.avg_ttft_ms /= row.measured;
            }
            let input = row.cache_read_tokens + row.input_tokens;
            if input > 0 {
                row.cache_hit_rate = row.cache_read_tokens as f64 / input as f64;
            }
            row
        })
        .collect();
    let mut result = serde_json::to_value(totals)?;
    result["threads"] = json!(
        snapshot
            .threads
            .iter()
            .flatten()
            .filter(|t| selected(&t.id))
            .count()
    );
    result["runs"] = count.into();
    result["activeRuns"] = active.into();
    result["completedRuns"] = completed.into();
    result["failedRuns"] = failed.into();
    result["tasks"] = json!(
        snapshot
            .tasks
            .iter()
            .flatten()
            .filter(|t| selected(&t.parent_thread_id) || selected(&t.child_thread_id))
            .count()
    );
    result["models"] = json!(models);
    Ok(result.to_string())
}
