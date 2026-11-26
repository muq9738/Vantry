//! Federated cross-environment task delegation, durable remote-task ledger,
//! event streaming, restart recovery, and follow-up routing.
use anyhow::{Context, Result, ensure};
use chrono::Utc;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    fs,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::Mutex as AsyncMutex;
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    identity::{Configuration, IdentityStore, Pair},
    peer::{PeerConnection, PeerEventStream, Route},
};
use vantry_core::{
    ProviderError,
    runner::{Emit, ProviderResult},
};
use vantry_platform::hostfs;
use vantry_protocol::*;

const LEDGER_FILE: &str = "remote-tasks.json";

#[derive(Clone)]
pub struct RemoteTarget {
    pair: Pair,
    cfg: Configuration,
}

impl RemoteTarget {
    async fn connection(&self) -> Result<PeerConnection> {
        PeerConnection::connect(&self.pair, Some(&self.cfg), Route::Auto, CancellationToken::new()).await
    }

    pub async fn get_json(&self, path: &str) -> Result<Value> {
        self.connection().await?.get_json(path).await
    }

    pub async fn post_json(&self, path: &str, payload: Value) -> Result<Value> {
        self.connection().await?.post_json(path, payload).await
    }

    pub async fn dispatch(&self, cmd: &Command) -> Result<Receipt> {
        let val = self.post_json("/v1/commands", serde_json::to_value(cmd)?).await?;
        Ok(serde_json::from_value(val)?)
    }

    pub async fn snapshot(&self) -> Result<Snapshot> {
        let val = self.get_json("/v1/snapshot").await?;
        Ok(serde_json::from_value(val)?)
    }

    pub async fn workbench(&self, req: &WorkbenchRequest) -> Result<WorkbenchResponse> {
        let val = self
            .post_json("/v1/workbench", serde_json::to_value(req)?)
            .await?;
        Ok(serde_json::from_value(val)?)
    }

    pub async fn environment_info(&self) -> Result<EnvironmentInfo> {
        let res = self
            .workbench(&WorkbenchRequest {
                action: "environment.info".into(),
                ..Default::default()
            })
            .await?;
        let info: EnvironmentInfo =
            serde_json::from_str(&res.output).context("invalid remote environment.info")?;
        ensure!(!info.id.is_empty(), "empty remote environment ID");
        Ok(info)
    }

    async fn subscribe(&self, after: i64) -> Result<PeerEventStream> {
        self.connection().await?.subscribe_events(after).await
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EnvironmentInfo {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub os: String,
    #[serde(default)]
    pub arch: String,
    #[serde(default)]
    pub home: String,
    #[serde(default)]
    pub provider_id: String,
    #[serde(default)]
    pub default_model: String,
    #[serde(default)]
    pub workspace: Option<WorkspaceSpec>,
}

/// Resolve a saved pairing or its remote environment identifier.
pub async fn resolve_target(
    data_dir: &Path,
    identity: &Arc<IdentityStore>,
    env_id: &str,
) -> Result<RemoteTarget> {
    let cfg = identity.snapshot();
    if let Some(peer) = cfg
        .peers
        .iter()
        .find(|p| p.id == env_id || p.pair.device_id == env_id || p.pair.name == env_id)
    {
        return Ok(RemoteTarget {
            pair: peer.pair.clone(),
            cfg,
        });
    }
    let catalog = vantry_platform::environments::Catalog::open(data_dir)?;
    let envs = catalog.load()?;
    if let Some(env) = envs.iter().find(|e| e.kind == vantry_platform::environments::Kind::Remote && (e.id == env_id || e.name == env_id)) {
        if let Some(peer) = cfg
            .peers
            .iter()
            .find(|p| p.id == env.id || p.pair.name == env.name || p.id == env.host)
        {
            return Ok(RemoteTarget {
                pair: peer.pair.clone(),
                cfg,
            });
        }
    }
    // Fallback: match the remote Core's own environment ID by probing saved peers.
    for peer in &cfg.peers {
        let candidate = RemoteTarget {
            pair: peer.pair.clone(),
            cfg: cfg.clone(),
        };
        if let Ok(info) = candidate.environment_info().await
            && info.id == env_id
        {
            return Ok(candidate);
        }
    }
    Err(vantry_core::Error::Api(
        ErrorCode::NotFound,
        format!("目标执行环境未连接: {env_id}"),
    )
    .into())
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RemoteTaskHandle {
    pub local_attempt_id: String,
    pub local_thread_id: String,
    pub environment_id: String,
    pub remote_thread_id: String,
    pub remote_run_id: String,
    pub initial_sequence: i64,
    pub cursor: i64,
    pub status: RunStatus,
    #[serde(default)]
    pub output: String,
    #[serde(default)]
    pub reasoning: String,
    #[serde(default)]
    pub error: String,
    pub created_at: String,
    pub updated_at: String,
}

#[derive(Debug, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LedgerDoc {
    version: u32,
    tasks: HashMap<String, RemoteTaskHandle>,
}

pub struct Federator {
    data_dir: PathBuf,
    identity: Arc<IdentityStore>,
    ledger: Mutex<HashMap<String, RemoteTaskHandle>>,
    attempts: Mutex<HashMap<String, Arc<AsyncMutex<()>>>>,
}

impl Federator {
    pub fn open(data_dir: PathBuf, identity: Arc<IdentityStore>) -> Result<Self> {
        let path = data_dir.join(LEDGER_FILE);
        let tasks = match fs::read(&path) {
            Ok(bytes) => serde_json::from_slice::<LedgerDoc>(&bytes)
                .map(|doc| doc.tasks)
                .unwrap_or_default(),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => HashMap::new(),
            Err(e) => return Err(e.into()),
        };
        Ok(Self {
            data_dir,
            identity,
            ledger: Mutex::new(tasks),
            attempts: Mutex::new(HashMap::new()),
        })
    }

    fn persist_locked(&self, tasks: &HashMap<String, RemoteTaskHandle>) -> Result<()> {
        let doc = LedgerDoc {
            version: 1,
            tasks: tasks.clone(),
        };
        let bytes = serde_json::to_vec_pretty(&doc)?;
        hostfs::atomic_write(&self.data_dir.join(LEDGER_FILE), &bytes)?;
        Ok(())
    }

    pub fn get_task(&self, attempt_id: &str) -> Option<RemoteTaskHandle> {
        self.ledger.lock().unwrap().get(attempt_id).cloned()
    }

    pub fn find_by_local_thread(&self, local_thread_id: &str) -> Option<RemoteTaskHandle> {
        self.ledger
            .lock()
            .unwrap()
            .values()
            .filter(|h| h.local_thread_id == local_thread_id)
            .max_by(|a, b| a.updated_at.cmp(&b.updated_at))
            .cloned()
    }

    fn attempt_lock(&self, attempt_id: &str) -> Arc<AsyncMutex<()>> {
        let mut map = self.attempts.lock().unwrap();
        map.entry(attempt_id.to_owned())
            .or_insert_with(|| Arc::new(AsyncMutex::new(())))
            .clone()
    }

    fn remote_thread_id(local_thread_id: &str) -> String {
        format!("remote_{local_thread_id}")
    }

    async fn ensure_remote(
        &self,
        request: &ProviderRequest,
    ) -> Result<(RemoteTarget, RemoteTaskHandle)> {
        let ws = request
            .workspace
            .as_ref()
            .context("missing remote workspace")?;
        let env_id = ws.environment_id.as_str();
        ensure!(!env_id.is_empty(), "missing target environment ID");
        let target = resolve_target(&self.data_dir, &self.identity, env_id).await?;
        if let Some(existing) = self.get_task(&request.attempt_id) {
            return Ok((target, existing));
        }
        let remote_thread = Self::remote_thread_id(&request.thread_id);
        let existing_thread = self
            .ledger
            .lock()
            .unwrap()
            .values()
            .any(|h| h.remote_thread_id == remote_thread && h.environment_id == env_id);
        let mut remote_ws = ws.clone();
        remote_ws.environment_id.clear();

        if !existing_thread {
            let create_cmd = Command {
                version: 1,
                command_id: format!("remote:{}:create", request.attempt_id),
                kind: CommandKind::CreateThread,
                thread_id: remote_thread.clone(),
                title: "Remote task".into(),
                workspace: Some(remote_ws),
                provider_id: request.provider_id.clone(),
                model: request.model.clone(),
                approval_policy: request.approval_policy.clone(),
                interaction_mode: request.interaction_mode.clone(),
                ..Default::default()
            };
            target.dispatch(&create_cmd).await?;
        }

        let send_cmd = Command {
            version: 1,
            command_id: format!("remote:{}:send", request.attempt_id),
            kind: CommandKind::SendMessage,
            thread_id: remote_thread.clone(),
            message: request.message.clone(),
            ..Default::default()
        };
        let receipt = target.dispatch(&send_cmd).await?;
        ensure!(
            !receipt.run_id.is_empty(),
            "remote Core did not return a run ID for delegated prompt"
        );

        let now = Utc::now().to_rfc3339();
        let handle = RemoteTaskHandle {
            local_attempt_id: request.attempt_id.clone(),
            local_thread_id: request.thread_id.clone(),
            environment_id: env_id.to_owned(),
            remote_thread_id: remote_thread,
            remote_run_id: receipt.run_id,
            initial_sequence: receipt.sequence.saturating_sub(1),
            cursor: receipt.sequence.saturating_sub(1),
            status: RunStatus::Running,
            output: String::new(),
            reasoning: String::new(),
            error: String::new(),
            created_at: now.clone(),
            updated_at: now,
        };
        {
            let mut map = self.ledger.lock().unwrap();
            map.insert(request.attempt_id.clone(), handle.clone());
            self.persist_locked(&map)?;
        }
        Ok((target, handle))
    }

    pub async fn run(&self, request: &ProviderRequest, emit: Emit) -> ProviderResult {
        if request.attachments.as_ref().is_some_and(|a| !a.is_empty()) {
            return Err(ProviderError::Failed(
                "远端任务不接受本机附件句柄，请在目标环境添加附件".into(),
            ));
        }
        let lock = self.attempt_lock(&request.attempt_id);
        let (target, handle) = {
            let _guard = lock.lock().await;
            self.ensure_remote(request)
                .await
                .map_err(|e| ProviderError::Failed(e.to_string()))?
        };

        let mut cursor = handle.initial_sequence;
        let mut ordinal: i64 = 0;
        let mut emitted_text = request.after_sequence > 0;
        let mut last_snapshot_check = tokio::time::Instant::now() - Duration::from_secs(2);

        loop {
            // Check snapshot first if handle is already terminal or reconnect fallback.
            if last_snapshot_check.elapsed() >= Duration::from_millis(800) {
                last_snapshot_check = tokio::time::Instant::now();
                if let Ok(Ok(snap)) =
                    tokio::time::timeout(Duration::from_secs(10), target.snapshot()).await
                    && let Some(run) = snap
                        .runs
                        .iter()
                        .flatten()
                        .find(|r| r.id == handle.remote_run_id)
                    && run.status.terminal()
                {
                    if !emitted_text && !run.output.is_empty() {
                        ordinal += 1;
                        let ev = ProviderEvent {
                            sequence: ordinal,
                            occurred_at: Utc::now().to_rfc3339(),
                            r#type: "text_delta".into(),
                            delta: run.output.clone(),
                            data: None,
                        };
                        let _ = emit(ev);
                    }
                    self.record_terminal(&request.attempt_id, snap.sequence, run);
                    return terminal_result(run);
                }
            }

            let Ok(mut stream) = target.subscribe(cursor).await else {
                tokio::time::sleep(Duration::from_millis(200)).await;
                continue;
            };

            loop {
                let next =
                    tokio::time::timeout(Duration::from_secs(1), stream.next_event()).await;
                match next {
                    Ok(Ok(Some(event))) => {
                        if event.sequence <= cursor {
                            continue;
                        }
                        cursor = event.sequence;
                        if event.run_id != handle.remote_run_id {
                            continue;
                        }
                        ordinal += 1;
                        if ordinal > request.after_sequence {
                            let pe = map_remote_event(&event, ordinal);
                            if pe.r#type == "text_delta" && !pe.delta.is_empty() {
                                emitted_text = true;
                            }
                            emit(pe).map_err(|err| match err.code() {
                                ErrorCode::Conflict | ErrorCode::NotReady => {
                                    ProviderError::Detached
                                }
                                _ => ProviderError::Interrupted(err.to_string()),
                            })?;
                        }
                        if event.r#type.starts_with("run.")
                            && let Ok(run) = serde_json::from_value::<Run>(event.data["run"].clone())
                            && run.status.terminal()
                        {
                            if !emitted_text && !run.output.is_empty() {
                                ordinal += 1;
                                let _ = emit(ProviderEvent {
                                    sequence: ordinal,
                                    occurred_at: Utc::now().to_rfc3339(),
                                    r#type: "text_delta".into(),
                                    delta: run.output.clone(),
                                    data: None,
                                });
                            }
                            self.record_terminal(&request.attempt_id, cursor, &run);
                            return terminal_result(&run);
                        }
                    }
                    Ok(Ok(None)) | Ok(Err(_)) => {
                        // Stream disconnected; break to snapshot check + reconnect.
                        break;
                    }
                    Err(_) => {
                        // Periodic snapshot reconciliation while stream is quiet.
                        if let Ok(Ok(snap)) =
                            tokio::time::timeout(Duration::from_secs(10), target.snapshot()).await
                            && let Some(run) = snap
                                .runs
                                .iter()
                                .flatten()
                                .find(|r| r.id == handle.remote_run_id)
                            && run.status.terminal()
                        {
                            if !emitted_text && !run.output.is_empty() {
                                ordinal += 1;
                                let _ = emit(ProviderEvent {
                                    sequence: ordinal,
                                    occurred_at: Utc::now().to_rfc3339(),
                                    r#type: "text_delta".into(),
                                    delta: run.output.clone(),
                                    data: None,
                                });
                            }
                            self.record_terminal(&request.attempt_id, snap.sequence.max(cursor), run);
                            return terminal_result(run);
                        }
                    }
                }
            }
            tokio::time::sleep(Duration::from_millis(150)).await;
        }
    }

    fn record_terminal(&self, attempt_id: &str, cursor: i64, run: &Run) {
        let mut map = self.ledger.lock().unwrap();
        if let Some(h) = map.get_mut(attempt_id) {
            h.cursor = cursor;
            h.status = run.status;
            h.output = run.output.clone();
            h.reasoning = run.reasoning.clone();
            h.error = run.error.clone();
            h.updated_at = Utc::now().to_rfc3339();
            let _ = self.persist_locked(&map);
        }
    }

    pub async fn stop(&self, request: &ProviderRequest) -> ProviderResult {
        let lock = self.attempt_lock(&request.attempt_id);
        let _guard = lock.lock().await;
        let ws = request
            .workspace
            .as_ref()
            .ok_or_else(|| ProviderError::Failed("missing remote workspace".into()))?;
        let target = resolve_target(&self.data_dir, &self.identity, &ws.environment_id)
            .await
            .map_err(|e| ProviderError::Failed(e.to_string()))?;
        if let Some(handle) = self.get_task(&request.attempt_id) {
            let cmd = Command {
                version: 1,
                command_id: format!("remote-stop:{}:{}", request.attempt_id, handle.remote_run_id),
                kind: CommandKind::StopRun,
                thread_id: handle.remote_thread_id,
                run_id: handle.remote_run_id,
                ..Default::default()
            };
            target
                .dispatch(&cmd)
                .await
                .map_err(|e| ProviderError::Failed(e.to_string()))?;
            return Ok(());
        }
        // Fallback: stop any active run on the deterministic remote thread.
        let remote_thread = Self::remote_thread_id(&request.thread_id);
        if let Ok(snap) = target.snapshot().await {
            for run in snap
                .runs
                .iter()
                .flatten()
                .filter(|r| r.thread_id == remote_thread && !r.status.terminal())
            {
                let cmd = Command {
                    version: 1,
                    command_id: format!("remote-stop:{}:{}", request.attempt_id, run.id),
                    kind: CommandKind::StopRun,
                    thread_id: run.thread_id.clone(),
                    run_id: run.id.clone(),
                    ..Default::default()
                };
                let _ = target.dispatch(&cmd).await;
            }
        }
        Ok(())
    }

    pub async fn control(
        &self,
        request: &ProviderRequest,
        control: &ControlResponse,
    ) -> ProviderResult {
        if !control.attachments.is_empty() {
            return Err(ProviderError::Failed(
                "远端控制消息不接受本机附件句柄".into(),
            ));
        }
        let handle = self
            .get_task(&request.attempt_id)
            .ok_or_else(|| ProviderError::Failed("remote task handle not found".into()))?;
        let target = resolve_target(&self.data_dir, &self.identity, &handle.environment_id)
            .await
            .map_err(|e| ProviderError::Failed(e.to_string()))?;
        let cmd = match control.kind.as_str() {
            "steer" => Command {
                version: 1,
                command_id: format!("remote-steer:{}", control.id),
                kind: CommandKind::InterThreadSend,
                thread_id: handle.remote_thread_id.clone(),
                target_thread_id: handle.remote_thread_id,
                message: control.message.clone(),
                delivery_mode: "steer".into(),
                fallback_to_queue: true,
                ..Default::default()
            },
            "response" => Command {
                version: 1,
                command_id: format!("remote-resp:{}", control.id),
                kind: CommandKind::RespondRequest,
                thread_id: handle.remote_thread_id,
                run_id: handle.remote_run_id,
                request_id: control.request_id.clone(),
                confirmed: control.confirmed,
                cancelled: control.cancelled,
                value: control.value.clone(),
                ..Default::default()
            },
            other => {
                return Err(ProviderError::Failed(format!(
                    "unsupported remote control kind: {other}"
                )));
            }
        };
        target
            .dispatch(&cmd)
            .await
            .map_err(|e| ProviderError::Failed(e.to_string()))?;
        Ok(())
    }
}

fn map_remote_event(event: &CoreEvent, ordinal: i64) -> ProviderEvent {
    let data = &event.data;
    match event.r#type.as_str() {
        "run.output" => {
            let delta = data["delta"].as_str().unwrap_or_default().to_owned();
            ProviderEvent {
                sequence: ordinal,
                occurred_at: event.timestamp.clone(),
                r#type: "text_delta".into(),
                delta,
                data: None,
            }
        }
        "run.reasoning" => {
            let delta = data["reasoningDelta"].as_str().unwrap_or_default();
            ProviderEvent {
                sequence: ordinal,
                occurred_at: event.timestamp.clone(),
                r#type: "thinking_delta".into(),
                delta: String::new(),
                data: Some(json!({"assistantMessageEvent": {"delta": delta}})),
            }
        }
        "tool.updated" | "request.updated" | "input.updated" => ProviderEvent {
            sequence: ordinal,
            occurred_at: event.timestamp.clone(),
            r#type: format!("remote.{}", event.r#type),
            delta: String::new(),
            data: Some(data.clone()),
        },
        _ => ProviderEvent {
            sequence: ordinal,
            occurred_at: event.timestamp.clone(),
            r#type: "remote_event".into(),
            delta: String::new(),
            data: None,
        },
    }
}

fn terminal_result(run: &Run) -> ProviderResult {
    match run.status {
        RunStatus::Completed => Ok(()),
        RunStatus::Cancelled => Err(ProviderError::ExecutionCancelled),
        RunStatus::Interrupted => Err(ProviderError::Interrupted(run.error.clone())),
        _ => Err(ProviderError::Failed(format!(
            "远端任务 {}: {}",
            serde_json::to_string(&run.status).unwrap_or_default(),
            run.error
        ))),
    }
}
