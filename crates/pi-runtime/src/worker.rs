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
    io::Write,
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
    pub default_model: String,
    #[serde(default)]
    pub expected_model: String,
    #[serde(default)]
    pub enable_user_extensions: bool,
    #[serde(rename = "enableUserMCP", default)]
    pub enable_user_mcp: bool,
    #[serde(default)]
    pub sandbox_mode: String,
    #[serde(default)]
    pub interaction_mode: String,
}
fn valid_id(value: &str) -> Result<()> {
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
    Ok(dir)
}

struct Writer {
    file: File,
    seq: i64,
    last_sync: Instant,
    first_text: bool,
    error: Option<String>,
}
pub(crate) struct Journal {
    writer: Mutex<Writer>,
    failed: CancellationToken,
}
impl Journal {
    fn open(dir: &Path, recover: bool, handled: &mut HashSet<PathBuf>) -> Result<Arc<Self>> {
        let path = dir.join("events.jsonl");
        let (file, seq) = if recover && path.try_exists()? {
            let mut reader = crate::JobEvents::open(dir, 0)?;
            while let Some(event) = reader.next_event()? {
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
            let file = File::options().append(true).open(path)?;
            let (offset, tail) = reader.tail();
            if !tail.is_empty() {
                disk::atomic(&dir.join("events.partial"), tail)?;
                file.set_len(offset)?;
            }
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
    } else {
        execute(&dir, &job, &journal, &mut handled).await
    };
    let control_lock = lock_file(&dir.join("control.lock"))?;
    control_lock.lock()?;
    disk::write_json(&dir.join("closing.json"), &json!({}))?;
    let finalize = consume_controls(&dir, &mut handled, None, &journal).await;
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
    ensure!(
        job.sandbox_mode.is_empty() || job.sandbox_mode == "off",
        "requested sandbox requires a sandboxed launcher"
    );
    ensure!(
        req.attachments.iter().flatten().next().is_none(),
        "attachment execution is unavailable in P0"
    );
    let workspace = req.workspace.as_ref().context("missing workspace")?;
    ensure!(
        workspace.kind == "directory"
            && workspace.environment_id.is_empty()
            && Path::new(&workspace.path).is_absolute(),
        "expected prepared local directory"
    );
    let cwd = Path::new(&workspace.path).canonicalize()?;
    ensure!(cwd.is_dir(), "workspace is not a directory");
    let sessions = job.data_dir.join("sessions");
    disk::directory(&sessions)?;
    let session_lock = lock_file(&sessions.join(format!("{}.lock", req.thread_id)))?;
    session_lock.try_lock().context("thread session in use")?;
    let manager = SessionManager::open_with(
        &sessions.join(format!("{}.jsonl", req.thread_id)),
        Some(&sessions),
        Some(&cwd),
    )?;
    let mut options = AppOptions {
        agent_dir: crate::agent_dir(),
        bind_mode: Some(ExtensionMode::Rpc),
        extension_mode: Some(ExtensionMode::Rpc),
        ..Default::default()
    };
    let model = [&job.expected_model, &req.model, &job.default_model]
        .into_iter()
        .find(|v| !v.is_empty());
    if let Some(model) = model {
        if let Some((provider, id)) = model.split_once('/') {
            options.provider = Some(provider.into());
            options.model = Some(id.into());
        } else {
            options.model = Some(model.clone());
        }
    }
    options.resources.no_extensions = !job.enable_user_extensions;
    if !job.enable_user_mcp {
        options
            .resources
            .disabled_builtin_extensions
            .push("mcp".into());
    }
    // Give the stock approval extension the same per-attempt environment as the Go worker.
    // Module-local bindings avoid changing process-wide environment during a live session.
    let interaction = if req.interaction_mode.is_empty() {
        &job.interaction_mode
    } else {
        &req.interaction_mode
    };
    let env = json!({"VANTRY_APPROVAL_POLICY":req.approval_policy,"VANTRY_INTERACTION_MODE":interaction,"VANTRY_DATA_DIR":job.data_dir,"VANTRY_THREAD_ID":req.thread_id});
    let approval = dir.join("approval.ts");
    disk::atomic(
        &approval,
        format!(
            "const process = {{env: {env}, pid: {}}};\n{}",
            std::process::id(),
            include_str!("../../../internal/adapters/provider/pi/approval.ts")
        )
        .as_bytes(),
    )?;
    options
        .resources
        .extension_sources
        .push(approval.to_string_lossy().into_owned());
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
            let selected = session.model().context("no selected model")?;
            ensure!(
                format!("{}/{}", selected.provider, selected.id) == job.expected_model,
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
        session.prompt(&req.message, vec![]).await?;
        session.wait_for_idle().await;
        if let Some(Message::Assistant(last)) = session.messages().last() {
            if matches!(last.stop_reason, StopReason::Error | StopReason::Aborted) {
                bail!(
                    "{}",
                    last.error_message.as_deref().unwrap_or("Pi turn failed")
                );
            }
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
            consume_controls(dir, handled, Some((&session, &ui)), journal).await?;
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

async fn consume_controls(
    dir: &Path,
    handled: &mut HashSet<PathBuf>,
    active: Option<(&AgentSession, &WorkerUi)>,
    journal: &Journal,
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
        let sent = dir.join("control-sent");
        let uncertain = sent.join(&name).try_exists()?;
        let outcome = if uncertain {
            Err(anyhow::anyhow!("prior delivery unconfirmed"))
        } else if let Some((session, ui)) = active {
            disk::directory(&sent)?;
            disk::write_json(&sent.join(&name), &json!({"id":id}))?;
            match c.kind.as_str() {
                "response" => ui.respond(c),
                "steer"
                    if session.is_streaming()
                        && !c.message.trim().is_empty()
                        && c.attachments.is_empty() =>
                {
                    session.steer(&c.message, vec![]).await.map(|_| ())
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
