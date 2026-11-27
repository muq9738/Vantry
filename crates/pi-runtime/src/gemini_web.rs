use crate::{JobRequest, agent_dir, attachments, disk, worker::Journal};
use anyhow::{Context as _, Result, bail, ensure};
use futures::StreamExt;
use pi_ai::{AssistantMessageEvent, Context, Message, StopReason, StreamOptions};
use pi_coding_agent::{
    agent_session::{ModelRuntime, ModelRuntimeOptions, auth::auth_stream_fn},
    settings::{SettingsManager, SettingsOptions},
};
use serde_json::json;
use std::{path::Path, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

/// Forward one turn to the selected Gemini Web bridge using its configured credentials.
pub(super) async fn run(job: &JobRequest, dir: &Path, journal: &Journal) -> Result<()> {
    let (provider, id) = job
        .expected_model
        .split_once('/')
        .context("缺少 Gemini 模型标识")?;
    let agent = agent_dir();
    let runtime = Arc::new(ModelRuntime::create(ModelRuntimeOptions::for_agent_dir(&agent)).await);
    let model = runtime
        .get_model(provider, id)
        .context("找不到已选择的 Gemini 模型")?;
    let settings = Arc::new(SettingsManager::create(
        &agent,
        &agent,
        SettingsOptions {
            project_trusted: false,
        },
    ));
    let stream = auth_stream_fn(runtime, settings, None, Default::default());
    let req = &job.request;
    let context = Context {
        system_prompt: None,
        messages: vec![attachments::user_message(
            &job.data_dir,
            &req.message,
            req.attachments.as_deref().unwrap_or_default(),
        )?],
        tools: vec![],
    };
    let cancel = CancellationToken::new();
    let _guard = cancel.clone().drop_guard();
    let options = StreamOptions {
        cancel,
        reasoning: job.thinking_level.parse().ok(),
        max_retries: Some(0),
        ..Default::default()
    };
    let stop = dir.join("stop.json");
    if stop.try_exists()? {
        return Ok(());
    }
    disk::write_json(
        &dir.join("prompt-intent.json"),
        &json!({"attemptId":req.attempt_id}),
    )?;
    let mut response = stream(&model, &context, options);
    let mut poll = tokio::time::interval(Duration::from_millis(150));
    let mut emitted_text = false;
    loop {
        let event = tokio::select! {
            _ = journal.failed.cancelled() => bail!("Gemini event journal unavailable"),
            _ = poll.tick() => { if stop.try_exists()? { return Ok(()); } continue; },
            event = response.next() => event.context("Gemini 回复未完整结束")?,
        };
        let (kind, delta) = match event {
            AssistantMessageEvent::TextDelta { delta, .. } => {
                emitted_text |= !delta.is_empty();
                ("text_delta", delta)
            }
            AssistantMessageEvent::ThinkingDelta { delta, .. } => ("thinking_delta", delta),
            AssistantMessageEvent::Done { message, reason } => {
                ensure!(
                    matches!(reason, StopReason::Stop | StopReason::Length),
                    "Gemini 返回了未请求的操作"
                );
                if !emitted_text && !message.text().is_empty() {
                    journal.record(json!({"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":message.text()}}));
                }
                journal.record(json!({"type":"message_end","message":Message::Assistant(message)}));
                return Ok(());
            }
            AssistantMessageEvent::Error { error, .. } => bail!(
                "{}",
                error.error_message.as_deref().unwrap_or("Gemini 请求失败")
            ),
            _ => continue,
        };
        journal.record(
            json!({"type":"message_update","assistantMessageEvent":{"type":kind,"delta":delta}}),
        );
    }
}
