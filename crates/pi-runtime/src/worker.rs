use crate::{disk, ui::WorkerUi};
use anyhow::{Context, Result, bail, ensure};
use chrono::Utc;
use pi_ai::{Message, StopReason, auth::oauth::base64url_encode};
use pi_coding_agent::{
    agent_session::{AgentSession, DiagnosticLevel},
    app::{AppOptions, create_app_runtime},
    extension_ui::ExtensionMode,
    extensions::ExtensionBindings,
    session::SessionManager,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::HashSet,
    fs::{self, File},
    io::{Seek, SeekFrom, Write},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio_util::sync::CancellationToken;
use vantry_protocol::{ControlResponse, ProviderEvent, ProviderRequest};

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct JobRequest {
    pub data_dir: PathBuf,
    pub request: ProviderRequest,
    #[serde(default)]
    pub executable: PathBuf,
    #[serde(default)]
    pub socket_path: PathBuf,
    #[serde(default)]
    pub enable_native_subagents: bool,
    #[serde(default)]
    pub default_model: String,
    #[serde(default)]
    pub expected_model: String,
    #[serde(default)]
    pub thinking_level: String,
    #[serde(default)]
    pub enable_user_extensions: bool,
    #[serde(rename = "enableUserMCP", default)]
    pub enable_user_mcp: bool,
    #[serde(default)]
    pub sandbox_mode: String,
    #[serde(default)]
    pub interaction_mode: String,
}
pub(crate) fn valid_id(value: &str) -> Result<()> {
    ensure!(
        !matches!(value, "" | "." | "..")
            && value
                .bytes()
                .all(|c| c.is_ascii_alphanumeric() || b".-_".contains(&c)),
        "invalid job identifier"
    );
    Ok(())
}
pub(crate) fn lock_file(path: &Path) -> Result<File> {
    let mut opts = File::options();
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    Ok(opts
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(path)?)
}
impl JobRequest {
    pub(crate) fn dir(&self) -> Result<PathBuf> {
        valid_id(&self.request.attempt_id)?;
        valid_id(&self.request.thread_id)?;
        valid_id(&self.request.run_id)?;
        ensure!(self.data_dir.is_absolute(), "dataDir must be absolute");
        Ok(self.data_dir.join("jobs").join(&self.request.attempt_id))
    }
}
/// Publish the same immutable request and pre-launch intent as the Go scheduler.
pub fn prepare_job(job: &JobRequest) -> Result<PathBuf> {
    let dir = job.dir()?;
    disk::directory(&dir)?;
    let lock = lock_file(&dir.join("worker.lock"))?;
    lock.try_lock()?;
    prepare_locked(job, &dir)?;
    Ok(dir)
}
pub(crate) fn prepare_locked(job: &JobRequest, dir: &Path) -> Result<()> {
    let path = dir.join("request.json");
    if path.try_exists()? {
        ensure!(
            disk::read::<JobRequest>(&path)? == *job,
            "conflicting attempt request"
        );
    } else {
        disk::write_json(&path, job)?;
    }
    if !dir.join("started.json").try_exists()? {
        disk::write_json(
            &dir.join("started.json"),
            &json!({"attemptId":job.request.attempt_id,"state":"launching","updatedAt":Utc::now()}),
        )?;
    }
    Ok(())
}

struct Writer {
    file: File,
    seq: i64,
    last_sync: Instant,
    first_text: bool,
    private_inputs: HashSet<String>,
    error: Option<String>,
}
pub(crate) struct Journal {
    writer: Mutex<Writer>,
    pub(crate) failed: CancellationToken,
}
fn input_dialog(data: &Value) -> Option<&str> {
    if data["type"] == "extension_ui_request" && data["method"] == "input" {
        data["id"].as_str()
    } else {
        None
    }
}
impl Journal {
    fn open(dir: &Path, recover: bool, handled: &mut HashSet<PathBuf>) -> Result<Arc<Self>> {
        let path = dir.join("events.jsonl");
        let mut private_inputs = HashSet::new();
        let (file, seq) = if recover && path.try_exists()? {
            let mut reader = crate::JobEvents::open(dir, 0)?;
            while let Some(event) = reader.next_event()? {
                if let Some(id) = event.data.as_ref().and_then(input_dialog) {
                    private_inputs.insert(id.to_owned());
                }
                if event.r#type == "control_result" {
                    let id = event
                        .data
                        .as_ref()
                        .and_then(|d| d["id"].as_str())
                        .context("control result missing ID")?;
                    handled.insert(
                        dir.join("controls")
                            .join(format!("{}.json", base64url_encode(id.as_bytes()))),
                    );
                }
            }
            let mut file = File::options().write(true).open(path)?;
            let (offset, tail) = reader.tail();
            if !tail.is_empty() {
                disk::atomic(&dir.join("events.partial"), tail)?;
                file.set_len(offset).context("truncate incomplete journal record")?;
            }
            file.seek(SeekFrom::End(0))?;
            (file, reader.cursor())
        } else {
            (disk::new_file(&path)?, 0)
        };
        Ok(Arc::new(Self {
            writer: Mutex::new(Writer {
                file,
                seq,
                last_sync: Instant::now(),
                first_text: true,
                private_inputs,
                error: None,
            }),
            failed: CancellationToken::new(),
        }))
    }
    pub(crate) fn record(&self, data: Value) {
        let mut w = self.writer.lock().unwrap();
        if w.error.is_some() {
            return;
        }
        if let Some(id) = input_dialog(&data) {
            w.private_inputs.insert(id.to_owned());
        }
        let mut kind = data["type"].as_str().unwrap_or("");
        let mut delta = "";
        if kind == "message_update" {
            let part = &data["assistantMessageEvent"];
            match part["type"].as_str() {
                Some("text_delta") => {
                    kind = "text_delta";
                    delta = part["delta"].as_str().unwrap_or("");
                }
                Some("thinking_delta") => kind = "thinking_delta",
                Some("error") => kind = "error",
                _ => {}
            }
        }
        let sync = match kind {
            "text_delta" => std::mem::take(&mut w.first_text),
            "thinking_delta" | "message_update" => false,
            _ => true,
        } || w.last_sync.elapsed() >= Duration::from_millis(40);
        let mut event = ProviderEvent {
            occurred_at: Utc::now().to_rfc3339(),
            sequence: w.seq + 1,
            r#type: kind.to_owned(),
            delta: delta.to_owned(),
            data: Some(data),
        };
        if event.r#type == "control_result" {
            if let Some(Value::Object(value)) = &mut event.data {
                value.remove("type");
            }
        }
        let append = (|| -> Result<()> {
            let mut line = serde_json::to_vec(&event)?;
            line.push(b'\n');
            w.file.write_all(&line)?;
            if sync {
                w.file.sync_data()?;
                w.last_sync = Instant::now();
            }
            w.seq += 1;
            Ok(())
        })();
        if let Err(e) = append {
            w.error = Some(e.to_string());
            self.failed.cancel();
        }
    }
    fn finish(&self) -> Result<i64> {
        let w = self.writer.lock().unwrap();
        if let Some(e) = &w.error {
            bail!("job journal: {e}");
        }
        w.file.sync_all()?;
        Ok(w.seq)
    }
}

/// An independent, single-use worker. A second invocation never repeats an uncertain prompt.
pub async fn run_worker(dir: &Path) -> Result<()> {
    let dir = dir.canonicalize()?;
    let job: JobRequest = disk::read(&dir.join("request.json"))?;
    ensure!(
        job.dir()?.canonicalize()? == dir,
        "job directory and attempt identity differ"
    );
    let lock = lock_file(&dir.join("worker.lock"))?;
    lock.try_lock().context("worker already active")?;
    if dir.join("result.json").try_exists()? {
        let result: Value = disk::read(&dir.join("result.json"))?;
        ensure!(
            result["attemptId"] == job.request.attempt_id,
            "result belongs to a different attempt"
        );
        return Ok(());
    }
    let marker: Value = disk::read(&dir.join("started.json"))?;
    ensure!(
        marker["attemptId"] == job.request.attempt_id,
        "missing matching launch intent"
    );
    let claim = dir.join("worker.json");
    let recover = claim.try_exists()?;
    if recover {
        let previous: Value = disk::read(&claim)?;
        ensure!(
            previous["attemptId"] == job.request.attempt_id,
            "worker claim belongs to a different attempt"
        );
    } else {
        disk::write_json(
            &claim,
            &json!({"attemptId":job.request.attempt_id,"pid":std::process::id(),"updatedAt":Utc::now()}),
        )?;
    }
    let mut handled = HashSet::new();
    let journal = Journal::open(&dir, recover, &mut handled)?;
    let stopped = dir.join("stop.json");
    let outcome = if recover {
        Err(anyhow::anyhow!(
            "worker exited after claiming attempt; initial prompt will not be repeated"
        ))
    } else if stopped.try_exists()? {
        Ok(())
    } else if job.request.provider_id == "controlled" {
        crate::ControlledTestProvider::execute(&dir, &job, &journal, &mut handled).await
    } else {
        execute(&dir, &job, &journal, &mut handled).await
    };
    if outcome
        .as_ref()
        .is_err_and(|e| e.is::<crate::controlled::Crash>())
    {
        journal.finish()?;
        return outcome;
    }
    let control_lock = lock_file(&dir.join("control.lock"))?;
    control_lock.lock()?;
    disk::write_json(&dir.join("closing.json"), &json!({}))?;
    let finalize = consume_controls(&dir, &mut handled, None, &journal, &job.data_dir).await;
    let outcome = outcome.and(finalize);
    let seq = journal.finish()?;
    let (status, error) = if stopped.try_exists()? {
        ("cancelled", None)
    } else {
        match &outcome {
            Ok(()) => ("completed", None),
            Err(e) => (
                if recover { "interrupted" } else { "failed" },
                Some(format!("{e:#}")),
            ),
        }
    };
    let mut result = json!({"attemptId":job.request.attempt_id,"status":status,"lastSequence":seq,"updatedAt":Utc::now()});
    if let Some(message) = error {
        result["error"] = json!(message);
    }
    disk::write_json(&dir.join("result.json"), &result)?;
    outcome
}

async fn execute(
    dir: &Path,
    job: &JobRequest,
    journal: &Arc<Journal>,
    handled: &mut HashSet<PathBuf>,
) -> Result<()> {
    let req = &job.request;
    ensure!(
        req.after_sequence == 0,
        "existing provider cursor requires reattachment"
    );
    ensure!(
        req.provider_id.is_empty() || req.provider_id == "pi",
        "unsupported execution provider"
    );
    let cwd = crate::workspace::cwd(job)?;
    let stop_file = dir.join("stop.json");
    let (_workspace_lock, waited_for_lock) = crate::workspace::lock_workspace_dir(
        &cwd,
        &stop_file,
        &journal.failed,
        if req.workspace.as_ref().is_some_and(|w| w.kind == "directory") {
            crate::workspace::WorkspaceLease::Shared
        } else {
            crate::workspace::WorkspaceLease::Exclusive
        },
        || {
            journal.record(json!({
                "type": "workspace_lock_wait",
                "path": cwd,
            }));
        },
    )
    .await?;
    if waited_for_lock {
        journal.record(json!({
            "type": "workspace_lock_acquired",
            "path": cwd,
        }));
    }
    if stop_file.try_exists()? {
        return Ok(());
    }
    let sessions = job.data_dir.join("sessions");
    disk::directory(&sessions)?;
    let _session_lock = crate::session::lock_thread(&sessions, &req.thread_id)?;
    let agent = crate::agent_dir();
    match crate::sandbox::mode(job)?.as_str() {
        "" | "off" => {}
        "workspace-write" => {
            disk::directory(&agent)?;
            crate::WorkspaceSandbox::new(&cwd, &[dir, &sessions, &agent])?.enter()?;
        }
        _ => bail!("unsupported sandbox mode"),
    }
    let (message, images) = crate::attachment_payload(
        &job.data_dir,
        &req.message,
        req.attachments.as_deref().unwrap_or_default(),
    )?;
    let manager = SessionManager::open_with(
        &sessions.join(format!("{}.jsonl", req.thread_id)),
        Some(&sessions),
        Some(&cwd),
    )?;
    let mut options = AppOptions {
        agent_dir: agent.clone(),
        bind_mode: Some(ExtensionMode::Rpc),
        extension_mode: Some(ExtensionMode::Rpc),
        settings: Some(crate::mcp::worker_settings(&cwd, &agent, job)),
        ..Default::default()
    };
    let model = [&req.model, &job.expected_model, &job.default_model]
        .into_iter()
        .find(|v| !v.is_empty());
    let mut requested_thinking = job.thinking_level.trim().parse::<pi_ai::ThinkingLevel>().ok();
    if let Some(model) = model {
        let (base_model, suffix_thinking) = crate::split_model_thinking(model);
        if suffix_thinking.is_some() {
            requested_thinking = suffix_thinking;
        }
        if let Some((provider, id)) = base_model.split_once('/') {
            options.provider = Some(provider.into());
            options.model = Some(id.into());
        } else {
            options.model = Some(base_model.to_owned());
        }
    }
    options.thinking = requested_thinking;
    crate::mcp::configure(&mut options, job);
    // Give the stock approval extension the same per-attempt environment as the Go worker.
    // Module-local bindings avoid changing process-wide environment during a live session.
    let env_mode = std::env::var("VANTRY_INTERACTION_MODE").unwrap_or_default();
    let interaction = [&req.interaction_mode, &job.interaction_mode, &env_mode]
        .into_iter()
        .map(|s| s.trim().to_ascii_lowercase())
        .find(|s| matches!(s.as_str(), "default" | "plan"))
        .unwrap_or_default();
    let socket = if job.socket_path.as_os_str().is_empty() {
        job.data_dir.join("core.sock")
    } else {
        job.socket_path.clone()
    };
    let exe = if job.executable.as_os_str().is_empty() {
        std::env::current_exe()?
    } else {
        job.executable.clone()
    };
    let instructions_path = crate::instructions::write_worker_instruction_files(
        job,
        dir,
        &socket,
        &exe,
        &interaction,
    )?;
    options.resources.append_system_prompt =
        Some(vec![instructions_path.to_string_lossy().into_owned()]);
    let policy = if req.approval_policy.is_empty() {
        "auto"
    } else {
        &req.approval_policy
    };
    let env = json!({
        "VANTRY_APPROVAL_POLICY": policy,
        "VANTRY_INTERACTION_MODE": interaction,
        "VANTRY_DATA_DIR": job.data_dir,
        "VANTRY_THREAD_ID": req.thread_id,
        "VANTRY_RUN_ID": req.run_id,
        "VANTRY_ATTEMPT_ID": req.attempt_id,
        "VANTRY_PARENT_TASK_ID": req.parent_task_id,
        "VANTRY_EXECUTABLE": exe,
        "VANTRY_SOCKET_PATH": socket,
        "VANTRY_JOB_DIR": dir,
    });
    for (filename, source) in [
        (
            "vantry_tools.ts",
            include_str!("../extensions/vantry_tools.ts"),
        ),
        (
            "approval.ts",
            include_str!("../extensions/approval.ts"),
        ),
    ] {
        let path = dir.join(filename);
        let source = format!(
            "const process = {{env: {env}, pid: {}, cwd: () => {}}};\n{source}",
            std::process::id(),
            json!(cwd)
        );
        disk::atomic(&path, source.as_bytes())?;
        options
            .resources
            .extension_sources
            .push(path.to_string_lossy().into_owned());
    }
    let runtime = create_app_runtime(options, manager).await?;
    let session = runtime.session();
    let stop = CancellationToken::new();
    let ui = Arc::new(WorkerUi::new(journal.clone(), stop.clone()));
    let sink = journal.clone();
    let subscription = session.subscribe(move |e| {
        if e.is_json_event() {
            sink.record(e.to_json());
        }
    });
    let work = async {
        for d in runtime.diagnostics() {
            if d.level == DiagnosticLevel::Error {
                bail!("Pi startup: {}", d.message);
            }
        }
        if !job.expected_model.is_empty() {
            let (expected_base, _) = crate::split_model_thinking(&job.expected_model);
            let selected = session.model().context("no selected model")?;
            ensure!(
                format!("{}/{}", selected.provider, selected.id) == expected_base,
                "selected model differs from requested model"
            );
        }
        session
            .bind_extensions(ExtensionBindings {
                ui: ui.clone(),
                mode: ExtensionMode::Rpc,
                ..Default::default()
            })
            .await;
        session.register_tool(
            Arc::new(crate::secret_tool::RequestSecret::new(ui.clone(), dir)),
            pi_coding_agent::agent_session::ToolSource::sdk("vantry_request_secret"),
        );
        session.register_tool(
            Arc::new(crate::todo_tool::TodoWrite),
            pi_coding_agent::agent_session::ToolSource::sdk(crate::todo_tool::NAME),
        );
        ensure!(
            ui.approval_ready(),
            "approval extension failed to initialize"
        );
        ensure!(
            !stop.is_cancelled() && !dir.join("stop.json").try_exists()?,
            "attempt stopped"
        );
        disk::write_json(
            &dir.join("prompt-intent.json"),
            &json!({"attemptId":req.attempt_id,"updatedAt":Utc::now()}),
        )?;
        session.prompt(&message, images).await?;
        session.wait_for_idle().await;
        let mut retries = 0;
        loop {
            let Some(Message::Assistant(last)) = session.messages().last().cloned() else {
                break;
            };
            if !matches!(last.stop_reason, StopReason::Error | StopReason::Aborted) {
                break;
            }
            if retries < BLANK_FAILURE_RETRIES
                && blank_upstream_failure(&last)
                && !stop.is_cancelled()
                && !dir.join("stop.json").try_exists()?
            {
                // Pi's own retry only knows named transient errors. A relay that
                // drops the stream reports nothing; the failed reply is left out
                // of the next request's context, so the model continues cleanly.
                retries += 1;
                tokio::time::sleep(Duration::from_secs(1 << retries)).await;
                session.prompt(BLANK_FAILURE_CONTINUE, Vec::new()).await?;
                session.wait_for_idle().await;
                continue;
            }
            bail!(
                "{}",
                last.error_message.as_deref().unwrap_or("Pi turn failed")
            );
        }
        Ok(())
    };
    tokio::pin!(work);
    let monitor = async {
        loop {
            tokio::select! {
                _ = journal.failed.cancelled() => bail!("journal write failed"),
                _ = tokio::time::sleep(Duration::from_millis(25)) => {},
            }
            if dir.join("stop.json").try_exists()? {
                return Ok(());
            }
            consume_controls(
                dir,
                handled,
                Some((Active::Pi(&session), &ui)),
                journal,
                &job.data_dir,
            )
            .await?;
        }
    };
    let result = tokio::select! {
        biased;
        result = &mut work => result,
        result = monitor => {
            stop.cancel();
            let _ = tokio::join!(session.abort(), &mut work);
            result
        }
    };
    stop.cancel();
    session.unsubscribe(subscription);
    runtime.dispose().await;
    result
}

const BLANK_FAILURE_RETRIES: u32 = 2;
const BLANK_FAILURE_CONTINUE: &str =
    "（上一次模型请求被上游中断，未返回任何内容。请从中断处继续完成当前任务。）";

/// An upstream error that carried nothing: no output, no tokens, no named
/// cause (for example `Error Code null: Unknown error`). Named errors are
/// Pi's to retry or report; aborts are never retried.
fn blank_upstream_failure(message: &pi_ai::types::AssistantMessage) -> bool {
    let error = message.error_message.as_deref().unwrap_or("").trim();
    message.stop_reason == StopReason::Error
        && message.usage.output == 0
        && message.content.iter().all(|block| match block {
            pi_ai::types::ContentBlock::Text { text, .. } => text.trim().is_empty(),
            pi_ai::types::ContentBlock::Thinking { thinking, .. } => thinking.trim().is_empty(),
            _ => false,
        })
        && (error.is_empty() || error.to_ascii_lowercase().ends_with("unknown error"))
}

#[derive(Clone, Copy)]
pub(crate) enum Active<'a> {
    Pi(&'a AgentSession),
    Controlled(&'a tokio::sync::mpsc::UnboundedSender<String>),
}
pub(crate) async fn consume_controls(
    dir: &Path,
    handled: &mut HashSet<PathBuf>,
    active: Option<(Active<'_>, &WorkerUi)>,
    journal: &Journal,
    data_dir: &Path,
) -> Result<()> {
    let controls = dir.join("controls");
    if !controls.try_exists()? {
        return Ok(());
    }
    let mut pending = vec![];
    for entry in fs::read_dir(controls)? {
        let entry = entry?;
        let path = entry.path();
        if path.extension().is_some_and(|x| x == "json") && !handled.contains(&path) {
            pending.push((entry.metadata()?.modified()?, path));
        }
    }
    pending.sort();
    for (_, path) in pending {
        let c: ControlResponse = disk::read(&path)?;
        let id = c.id.clone();
        let name = format!("{}.json", base64url_encode(id.as_bytes()));
        ensure!(
            !id.is_empty() && id.len() <= 180 && path.file_name().unwrap() == name.as_str(),
            "invalid control identity"
        );
        if c.kind == "response"
            && !c.value.is_empty()
            && journal
                .writer
                .lock()
                .unwrap()
                .private_inputs
                .contains(&c.request_id)
        {
            // Scrub private input even when the dialog expired or the worker is recovering.
            // Retries use the command and request IDs after the value has been consumed.
            let mut consumed = serde_json::to_value(&c)?;
            consumed.as_object_mut().unwrap().remove("value");
            consumed["valueConsumed"] = json!(true);
            disk::write_json(&path, &consumed)?;
        }
        let sent = dir.join("control-sent");
        let uncertain = sent.join(&name).try_exists()?;
        let outcome = if uncertain {
            Err(anyhow::anyhow!("prior delivery unconfirmed"))
        } else if let Some((session, ui)) = active {
            // The Core can accept a steer while Pi is still binding extensions.
            // Leave it durable for the next poll until the prompt can receive it.
            if c.kind == "steer" && matches!(session, Active::Pi(pi) if !pi.is_streaming()) {
                continue;
            }
            disk::directory(&sent)?;
            disk::write_json(&sent.join(&name), &json!({"id":id}))?;
            match c.kind.as_str() {
                "response" => ui.respond(c),
                "steer" if !c.message.trim().is_empty() => {
                    match crate::attachment_payload(data_dir, &c.message, &c.attachments) {
                        Ok((text, images)) => match session {
                            Active::Pi(session) if session.is_streaming() => {
                                session.steer(&text, images).await.map(|_| ())
                            }
                            Active::Controlled(tx) => tx
                                .send(text)
                                .map_err(|_| anyhow::anyhow!("controlled attempt ended")),
                            _ => Err(anyhow::anyhow!("attempt is idle")),
                        },
                        Err(e) => Err(e),
                    }
                }
                _ => Err(anyhow::anyhow!("unsupported control or attempt is idle")),
            }
        } else {
            Err(anyhow::anyhow!("attempt has settled"))
        };
        let mut data = json!({"type":"control_result","id":id,"status":"delivered"});
        if let Err(e) = outcome {
            data["status"] = json!(if uncertain { "unknown" } else { "failed" });
            data["error"] = json!(e.to_string());
        }
        journal.record(data);
        handled.insert(path);
    }
    Ok(())
}

#[cfg(test)]
mod blank_failure_tests {
    use super::*;

    fn reply(content: Value, output: u64, stop: &str, error: Option<&str>) -> pi_ai::types::AssistantMessage {
        // The shape the magpie relay produced in a failed sub-agent job.
        let mut message = json!({
            "role": "assistant", "content": content, "api": "openai-responses",
            "provider": "magpie", "model": "dmit-ai/group/gtp-6-astra",
            "usage": {"input": 0, "output": output, "cacheRead": 0, "cacheWrite": 0, "totalTokens": output,
                "cost": {"input": 0.0, "output": 0.0, "cacheRead": 0.0, "cacheWrite": 0.0, "total": 0.0}},
            "stopReason": stop, "timestamp": 1795680547124u64
        });
        if let Some(error) = error {
            message["errorMessage"] = json!(error);
        }
        serde_json::from_value(message).unwrap()
    }

    #[test]
    fn only_an_empty_unexplained_upstream_error_is_retried() {
        assert!(blank_upstream_failure(&reply(json!([]), 0, "error", Some("Error Code null: Unknown error"))));
        assert!(blank_upstream_failure(&reply(json!([]), 0, "error", None)));
        let named = ["429 Too Many Requests", "insufficient_quota", "invalid api key", "context_length_exceeded"];
        for error in named {
            assert!(!blank_upstream_failure(&reply(json!([]), 0, "error", Some(error))), "{error}");
        }
        assert!(!blank_upstream_failure(&reply(json!([]), 0, "aborted", None)), "a stop is final");
        let partial = json!([{"type": "text", "text": "已完成一半"}]);
        assert!(!blank_upstream_failure(&reply(partial, 12, "error", Some("Unknown error"))));
        assert!(!blank_upstream_failure(&reply(json!([]), 0, "stop", None)));
    }
}
