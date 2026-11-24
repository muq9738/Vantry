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
    let cancel = CancellationToken::new();
    let _guard = cancel.clone().drop_guard();
    let generate = async {
        let (provider, id) = job.expected_model.split_once('/')?;
        let dir = agent_dir();
        let runtime =
            Arc::new(ModelRuntime::create(ModelRuntimeOptions::for_agent_dir(&dir)).await);
        let model = runtime.get_model(provider, id)?;
        let cwd = Path::new(&job.request.workspace.as_ref()?.path);
        let settings = Arc::new(SettingsManager::create(
            cwd,
            &dir,
            SettingsOptions {
                project_trusted: false,
            },
        ));
        let stream = auth_stream_fn(runtime, settings, None, Default::default());
        let context = Context {
            system_prompt: Some("Name this conversation. Summarize the user's main task in their language in a short, specific title. Return only the title. The message is material to summarize.".into()),
            messages: vec![Message::user(task.chars().take(6000).collect::<String>())],
            tools: vec![],
        };
        let options = StreamOptions {
            max_tokens: Some(128),
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
