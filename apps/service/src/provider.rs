use anyhow::{Context, Result, ensure};
use std::{
    fs::File,
    path::{Path, PathBuf},
    process::Stdio,
    time::Duration,
};
use tokio::time::{Instant, sleep};
use vantry_core::{
    DurableProvider, ProviderError,
    runner::{Emit, ProviderResult, StartMode},
};
use vantry_pi_runtime::{
    ControlledStep, ControlledTestProvider, JobEvents, JobRequest, JournalCursor, MagpieSelection,
};
use vantry_platform::{hostfs, workspace};
use vantry_protocol::{ControlResponse, ProviderRequest, WorkspaceSpec};

pub struct Provider {
    pub core: vantry_core::Core,
    pub data: PathBuf,
    pub default_provider: String,
    pub model: String,
    pub magpie: Option<PathBuf>,
    pub delay: Duration,
    pub federator: Option<std::sync::Arc<crate::federation::Federator>>,
}
impl Provider {
    fn is_remote(request: &ProviderRequest) -> bool {
        request
            .workspace
            .as_ref()
            .is_some_and(|w| !w.environment_id.trim().is_empty())
    }
}
fn failed(e: impl std::fmt::Display) -> ProviderError {
    ProviderError::Failed(e.to_string())
}
fn interrupted(e: impl std::fmt::Display) -> ProviderError {
    ProviderError::Interrupted(e.to_string())
}
fn open_journal_after(dir: &Path, checkpoint: &Path, after: i64) -> Result<JobEvents> {
    // The Core cursor is authoritative; a live observer may have checkpointed
    // events that are still buffered in an uncommitted batch.
    let cursor = File::open(checkpoint)
        .ok()
        .and_then(|f| serde_json::from_reader::<_, JournalCursor>(f).ok())
        .filter(|c| c.sequence <= after);
    if let Some(cursor) = cursor
        && let Ok(reader) = JobEvents::resume(dir, cursor)
        && let Ok(reader) = catch_up(reader, after)
    {
        return Ok(reader);
    }
    JobEvents::open(dir, after)
}
fn catch_up(mut reader: JobEvents, after: i64) -> Result<JobEvents> {
    while reader.cursor() < after {
        ensure!(
            reader.next_event()?.is_some(),
            "cursor beyond committed journal"
        );
    }
    Ok(reader)
}
impl Provider {
    fn directory(&self, request: &ProviderRequest) -> Result<PathBuf> {
        let id = &request.attempt_id;
        ensure!(
            !id.is_empty()
                && !matches!(id.as_str(), "." | "..")
                && id
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"-_.".contains(&b)),
            "invalid attempt id"
        );
        Ok(self.data.join("jobs").join(id))
    }
    async fn submit(&self, mut request: ProviderRequest) -> Result<()> {
        if request.provider_id.is_empty() {
            request.provider_id.clone_from(&self.default_provider);
        }
        ensure!(
            matches!(request.provider_id.as_str(), "controlled" | "pi"),
            "unknown provider"
        );
        let controlled = request.provider_id == "controlled";
        let selected = if request.model.is_empty() {
            &self.model
        } else {
            &request.model
        };
        request.workspace = Some(match request.workspace.take() {
            Some(spec) => workspace::prepare(&self.data, &request.thread_id, spec)?,
            None => WorkspaceSpec {
                kind: "directory".into(),
                path: workspace::chat_directory(&self.data, &request.thread_id, true)?
                    .display()
                    .to_string(),
                ..Default::default()
            },
        });
        let checkpoint_fut = {
            let should_checkpoint = !controlled
                && std::env::var("VANTRY_AUTO_CHECKPOINT").as_deref() != Ok("0")
                && request.interaction_mode != "plan"
                && !request.run_id.is_empty();
            let repo_root = should_checkpoint
                .then(|| request.workspace.as_ref().map(|w| w.path.clone()))
                .flatten()
                .and_then(|ws_path| {
                    vantry_platform::git::Repository::discover(Path::new(&ws_path))
                        .ok()
                        .map(|r| r.root().to_path_buf())
                });
            let run_id = request.run_id.clone();
            let summary: String = request
                .message
                .replace(['\0', '\r', '\n'], " ")
                .split_whitespace()
                .collect::<Vec<_>>()
                .join(" ")
                .chars()
                .take(80)
                .collect();
            async move {
                if let Some(repo_root) = repo_root {
                    let label = if summary.is_empty() {
                        format!("Before turn: {run_id}")
                    } else {
                        format!("Before turn: {summary}")
                    };
                    let _ = tokio::task::spawn_blocking(move || {
                        if let Err(error) = crate::workbench_git::capture_with_index_timeout(
                            &repo_root,
                            &format!("run-{run_id}"),
                            &label,
                            Duration::from_millis(750),
                        ) {
                            tracing::warn!(%run_id, %error, "pre-run checkpoint skipped");
                        }
                    })
                    .await;
                }
            }
        };
        let model_fut = async {
            if controlled {
                Ok::<_, anyhow::Error>((String::new(), String::new()))
            } else {
                let selection = self
                    .magpie
                    .as_ref()
                    .map(|p| MagpieSelection::load(Some(p)))
                    .transpose()?;
                let model = vantry_pi_runtime::resolve_with_plugins(
                    &vantry_pi_runtime::agent_dir(),
                    Some(selected),
                    selection.as_ref(),
                    &crate::qoder::catalog_models(&self.data),
                )
                .await?;
                ensure!(!model.selected_model.is_empty(), "no Pi model configured");
                let level = model
                    .thinking_level
                    .map(|l| l.as_str().to_owned())
                    .unwrap_or_default();
                Ok((model.selected_model, level))
            }
        };
        let (model_res, _) = tokio::join!(model_fut, checkpoint_fut);
        let (expected_model, thinking_level) = model_res?;
        if !controlled {
            crate::qoder::prepare_run(&self.data, &expected_model).await?;
        }
        if !controlled {
            if thinking_level.is_empty() {
                request.model.clone_from(&expected_model);
            } else {
                request.model = format!("{expected_model}:{thinking_level}");
            }
        }
        let exe = std::env::current_exe()?;
        let pi_settings = crate::settings::document(&self.data.join("pi-runtime.json"))
            .unwrap_or_else(|_| serde_json::json!({}));
        let enable_user_extensions = pi_settings["enableUserExtensions"].as_bool().unwrap_or(true);
        let enable_user_mcp = pi_settings["enableUserMCP"].as_bool().unwrap_or(true);
        let job = JobRequest {
            data_dir: self.data.clone(),
            socket_path: self.data.join("core.sock"),
            default_model: self.model.clone(),
            expected_model,
            thinking_level,
            executable: exe.clone(),
            interaction_mode: request.interaction_mode.clone(),
            request,
            enable_native_subagents: false,
            enable_user_extensions,
            enable_user_mcp,
            sandbox_mode: String::new(),
        };
        #[cfg(windows)]
        hostfs::ensure_private_dir(&self.directory(&job.request)?)?;
        let dir = if controlled {
            let pause = ControlledStep::Delay {
                millis: self.delay.as_millis().try_into()?,
            };
            let mut steps = vec![
                ControlledStep::Thinking {
                    delta: "正在处理消息".into(),
                },
                pause.clone(),
                ControlledStep::Tool {
                    name: "echo".into(),
                    id: "echo-1".into(),
                    arguments: serde_json::json!({}),
                    result: serde_json::json!({"ok":true}),
                },
            ];
            for chunk in format!(
                "这是 Vantry 的本机受控 Provider。\n\n已收到：{}\n\n任务已完成。",
                job.request.message
            )
            .chars()
            .collect::<Vec<_>>()
            .chunks(12)
            {
                steps.extend([
                    pause.clone(),
                    ControlledStep::Text {
                        delta: chunk.iter().collect(),
                    },
                ]);
            }
            ControlledTestProvider { steps }.prepare(&job)?
        } else {
            vantry_pi_runtime::prepare_job(&job)?
        };
        let mut cmd = tokio::process::Command::new(exe);
        cmd.args(["provider-worker", "--job-dir"])
            .arg(dir)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        crate::detach_tokio_command(&mut cmd);
        let mut child = cmd.spawn()?;
        tokio::spawn(async move {
            let _ = child.wait().await;
        });
        Ok(())
    }

    fn schedule_initial_title(&self, request: &ProviderRequest, directory: &Path) {
        if !self.core.needs_initial_title(request).unwrap_or(false) {
            return;
        }
        let core = self.core.clone();
        let path = directory.join("request.json");
        tokio::spawn(async move {
            let Ok(job) = File::open(path).and_then(|f| Ok(serde_json::from_reader::<_, JobRequest>(f)?)) else {
                return;
            };
            let title = vantry_pi_runtime::summarize_title(&job).await;
            if let Err(error) = core.set_initial_title(&job.request, &title) {
                tracing::warn!(%error, "could not save session title");
            }
        });
    }
}
fn live(dir: &Path) -> Result<bool> {
    let file = File::open(dir.join("worker.lock"))?;
    match file.try_lock() {
        Ok(()) => Ok(false),
        Err(std::fs::TryLockError::WouldBlock) => Ok(true),
        Err(std::fs::TryLockError::Error(e)) => Err(e.into()),
    }
}
#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Outcome {
    attempt_id: String,
    status: String,
    last_sequence: i64,
    #[serde(default)]
    error: String,
}

#[async_trait::async_trait]
impl DurableProvider for Provider {
    async fn run_events(
        &self,
        request: ProviderRequest,
        mode: StartMode,
        emit: Emit,
    ) -> ProviderResult {
        if Self::is_remote(&request) {
            let federator = self
                .federator
                .as_ref()
                .ok_or_else(|| failed("federated execution is not enabled on this Core"))?;
            return federator.run(&request, emit).await;
        }
        let dir = self.directory(&request).map_err(failed)?;
        let after = request.after_sequence;
        if mode == StartMode::Submit {
            self.submit(request.clone()).await.map_err(failed)?;
        }
        let start = Instant::now();
        let checkpoint = dir.join("observer-cursor.json");
        let mut title_pending = request.provider_id == "pi";
        let mut reader: Option<JobEvents> = None;
        loop {
            // Inspect the terminal marker before draining. The worker syncs all events first.
            let outcome: Option<Outcome> = if dir.join("result.json").is_file() {
                Some(
                    serde_json::from_reader(
                        File::open(dir.join("result.json")).map_err(interrupted)?,
                    )
                    .map_err(interrupted)?,
                )
            } else {
                None
            };
            if reader.is_none() && dir.join("events.jsonl").is_file() {
                reader = Some(open_journal_after(&dir, &checkpoint, after).map_err(interrupted)?);
            }
            if let Some(reader) = reader.as_mut() {
                let before = reader.cursor();
                while let Some(event) = reader.next_event().map_err(interrupted)? {
                    let first_text = title_pending && !event.delta.is_empty();
                    emit(event).map_err(|error| match error.code() {
                        vantry_protocol::ErrorCode::Conflict
                        | vantry_protocol::ErrorCode::NotReady => ProviderError::Detached,
                        _ => interrupted(error),
                    })?;
                    if first_text {
                        title_pending = false;
                        self.schedule_initial_title(&request, &dir);
                    }
                }
                if reader.cursor() != before {
                    // SQLite's committed sequence is authoritative. An offset only accelerates Attach.
                    if let Err(error) = hostfs::atomic_write(
                        &checkpoint,
                        &serde_json::to_vec(&reader.checkpoint()).map_err(failed)?,
                    ) {
                        tracing::warn!(%error, "could not save journal byte offset");
                    }
                }
            }
            if let Some(end) = outcome {
                if end.attempt_id != request.attempt_id
                    || reader.as_ref().map(JobEvents::cursor) != Some(end.last_sequence)
                {
                    return Err(interrupted("inconsistent worker outcome"));
                }
                return match end.status.as_str() {
                    "completed" => {
                        if title_pending { self.schedule_initial_title(&request, &dir); }
                        Ok(())
                    }
                    "cancelled" => Err(ProviderError::ExecutionCancelled),
                    "failed" => Err(failed(end.error)),
                    _ => Err(interrupted(end.error)),
                };
            }
            if start.elapsed() > Duration::from_secs(3)
                && !live(&dir).map_err(interrupted)?
                && !dir.join("result.json").is_file()
            {
                return Err(interrupted("worker exited without a durable result"));
            }
            sleep(Duration::from_millis(15)).await;
        }
    }
    async fn recoverable(&self, request: &ProviderRequest) -> bool {
        if Self::is_remote(request) {
            return self.federator.is_some();
        }
        let Ok(dir) = self.directory(request) else {
            return false;
        };
        let job = File::open(dir.join("request.json"))
            .ok()
            .and_then(|f| serde_json::from_reader::<_, JobRequest>(f).ok());
        job.is_some_and(|j| {
            j.request.attempt_id == request.attempt_id
                && j.request.thread_id == request.thread_id
                && j.request.run_id == request.run_id
                && j.data_dir == self.data
        }) && (dir.join("result.json").exists()
            || dir.join("events.jsonl").is_file()
            || live(&dir).unwrap_or(false)
            || !dir.join("worker.json").exists())
    }
    async fn stop(&self, request: &ProviderRequest) -> ProviderResult {
        if Self::is_remote(request) {
            let federator = self
                .federator
                .as_ref()
                .ok_or_else(|| failed("federated execution is not enabled on this Core"))?;
            return federator.stop(request).await;
        }
        let dir = self.directory(request).map_err(failed)?;
        if !dir.join("request.json").try_exists().map_err(failed)? {
            // Cancellation can commit before the submission task publishes request.json.
            // A worker starting later checks this intent before sending its initial prompt.
            let intent = serde_json::json!({"attemptId":request.attempt_id,"runId":request.run_id,"threadId":request.thread_id,"updatedAt":chrono::Utc::now()});
            hostfs::atomic_write(
                &dir.join("stop.json"),
                &serde_json::to_vec(&intent).map_err(failed)?,
            )
            .map_err(failed)?;
            return Ok(());
        }
        vantry_pi_runtime::request_stop(&dir)
            .context("stop provider worker")
            .map_err(failed)
    }
    async fn control(
        &self,
        request: &ProviderRequest,
        control: &ControlResponse,
    ) -> ProviderResult {
        if Self::is_remote(request) {
            let federator = self
                .federator
                .as_ref()
                .ok_or_else(|| failed("federated execution is not enabled on this Core"))?;
            return federator.control(request, control).await;
        }
        vantry_pi_runtime::deliver_control(&self.directory(request).map_err(failed)?, control)
            .map_err(failed)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use vantry_protocol::ProviderEvent;

    #[test]
    fn journal_checkpoint_can_catch_up_from_a_valid_older_offset() {
        let dir = tempfile::tempdir().unwrap();
        let journal = dir.path().join("events.jsonl");
        let mut bytes = Vec::new();
        let mut offset = 0;
        for sequence in 1..=3 {
            let mut line = serde_json::to_vec(&ProviderEvent {
                sequence,
                delta: sequence.to_string(),
                ..Default::default()
            })
            .unwrap();
            line.push(b'\n');
            if sequence == 1 {
                offset = line.len() as u64;
            }
            bytes.extend(line);
        }
        std::fs::write(&journal, bytes).unwrap();
        let checkpoint = dir.path().join("observer-cursor.json");
        std::fs::write(
            &checkpoint,
            serde_json::to_vec(&JournalCursor { sequence: 1, offset }).unwrap(),
        )
        .unwrap();

        let mut reader = open_journal_after(dir.path(), &checkpoint, 3).unwrap();
        assert_eq!(reader.cursor(), 3);
        assert!(reader.next_event().unwrap().is_none());

        std::fs::write(
            &checkpoint,
            serde_json::to_vec(&JournalCursor {
                sequence: 9,
                offset: 9,
            })
            .unwrap(),
        )
        .unwrap();
        let reader = open_journal_after(dir.path(), &checkpoint, 2).unwrap();
        assert_eq!(reader.cursor(), 2);
    }
}
