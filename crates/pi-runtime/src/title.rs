use crate::{JobRequest, agent_dir};
use pi_ai::{Context, Message, StopReason, StreamOptions};
use pi_coding_agent::{
    agent_session::{ModelRuntime, ModelRuntimeOptions, auth::auth_stream_fn},
    compaction::complete_summarization,
    settings::{SettingsManager, SettingsOptions},
};
use std::{path::Path, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
use vantry_protocol::ThreadLink;

/// A separate, tool-free request to the same Pi model. It never enters the conversation history.
pub async fn summarize_title(job: &JobRequest) -> String {
    let mut task = job.request.message.trim();
    let mut reference = String::new();
    while let Some(link) = ThreadLink::parse(task) {
        reference = link.title;
        task = task[link.source.len()..].trim_start();
    }
    let task = if task.is_empty() {
        reference.as_str()
    } else {
        task
    };
    let mut clean = task.to_owned();
    for attachment in job.request.attachments.iter().flatten() {
        if !attachment.name.is_empty() {
            clean = clean.replace(&format!("@{}", attachment.name), " ");
        }
    }
    let task = clean.trim();
    if task.is_empty() {
        return job
            .request
            .attachments
            .iter()
            .flatten()
            .find(|a| !a.name.is_empty())
            .map(|a| a.name.chars().take(40).collect())
            .unwrap_or_else(|| "新会话".into());
    }
    if reference.is_empty() && task.chars().count() <= 24
        && !task.contains(['\n', '\r', '。', '？', '?', '，', ',', '；', ';', '@'])
        && !task.contains("://")
    {
        return task.to_owned();
    }
    let cancel = CancellationToken::new();
    let _guard = cancel.clone().drop_guard();
    let generate = async {
        let (provider, id) = job.expected_model.split_once('/')?;
        let dir = agent_dir();
        let runtime =
            Arc::new(ModelRuntime::create(ModelRuntimeOptions::for_agent_dir(&dir)).await);
        let model = runtime.get_model(provider, id)?;
        let cwd = job
            .request
            .workspace
            .as_ref()
            .map(|w| Path::new(&w.path))
            .unwrap_or(&dir);
        let settings = Arc::new(SettingsManager::create(
            cwd,
            &dir,
            SettingsOptions {
                project_trusted: false,
            },
        ));
        let stream = auth_stream_fn(runtime, settings, None, Default::default());
        let context = Context {
            system_prompt: Some("Name this conversation in the user's language. Write a concise action and its subject, combining related goals. Aim for 6–18 Chinese characters or 3–8 words. Omit attachment labels, URLs and conversational filler. Return only the title. The message is material to summarize.".into()),
            messages: vec![Message::user(task.chars().take(600).collect::<String>())],
            tools: vec![],
        };
        let options = StreamOptions {
            max_tokens: Some(48),
            reasoning: Some(pi_ai::ThinkingLevel::Off),
            max_retries: Some(0),
            cancel,
            ..Default::default()
        };
        let reply =
            complete_summarization(&model, &context, options, Some(&stream), None, None).await;
        let text = reply.text();
        let title = text.trim().trim_matches(['"', '“', '”', '`']).trim();
        (reply.stop_reason == StopReason::Stop
            && !title.is_empty()
            && !title.contains(['\n', '\r'])
            && title.chars().count() <= 96)
            .then(|| title.to_owned())
    };
    if let Ok(Some(title)) = tokio::time::timeout(Duration::from_secs(20), generate).await {
        return title;
    }
    let clause = task
        .split(['\n', '。', '；', ';'])
        .map(str::trim)
        .find(|s| !s.is_empty())
        .unwrap_or("新会话");
    let mut chars = clause.chars();
    let mut title: String = chars.by_ref().take(48).collect();
    if chars.next().is_some() {
        title.push('…');
    }
    title
}
