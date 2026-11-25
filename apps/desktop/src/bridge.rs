//! The desktop's only ways out: [`CoreBridge`] to the Core and
//! [`WorkbenchBridge`] to the workspace (files, Git, terminal, settings).
//!
//! The UI never talks to a socket directly. It holds `Arc<dyn …>` bridges
//! and runs their futures on the runtime's threads
//! (`nsh_core::prelude::background`), so the engine thread never blocks.
//!
//! Core implementations:
//! - [`ClientBridge`]: protocol v1 through the real `vantry_client::CoreClient`
//!   on the Core's Unix socket (`<data dir>/core.sock`);
//! - [`MockBridge`]: an in-memory Core for `--mock` runs and tests;
//! - [`RemoteBridge`]: the paired Core reached through `peer-proxy`.
//!
//! Every wire type is `vantry_protocol`'s; this module only adds the
//! desktop's error classification ([`BridgeError::outcome_unknown`]) and the
//! event type names it folds ([`events`]).

mod client;
mod mock;
pub mod workbench;

use std::{fmt, future::Future, pin::Pin, sync::Arc};

pub use client::ClientBridge;
pub use mock::MockBridge;
pub use vantry_protocol as protocol;
use vantry_protocol::{
    Command, CoreEvent, Health, Receipt, Snapshot, ThreadDetail, ThreadSummaries,
};
pub use workbench::{CoreWorkbench, LocalWorkbench, MockWorkbench, WorkbenchBridge};

/// A bridge call's future: `Send`, so it runs on the runtime's threads.
pub type BridgeFuture<T> = Pin<Box<dyn Future<Output = Result<T, BridgeError>> + Send + 'static>>;

/// Where a subscription delivers events, in order.
pub type EventSink = tokio::sync::mpsc::UnboundedSender<CoreEvent>;

/// The Core's protocol v1, as the desktop uses it.
pub trait CoreBridge: Send + Sync + 'static {
    /// What the header shows about the connection ("本机 Core", "演示数据").
    fn label(&self) -> String;
    /// `GET /v1/health`.
    fn health(&self) -> BridgeFuture<Health>;
    /// `GET /v1/snapshot`.
    fn snapshot(&self) -> BridgeFuture<Snapshot>;
    /// `GET /v1/threads/summaries`: a cheap projection for the first sidebar.
    fn thread_summaries(&self) -> BridgeFuture<ThreadSummaries>;
    /// `GET /v1/threads/detail`: the selected conversation's bounded detail.
    fn thread_detail(&self, thread_id: String, limit_runs: i64) -> BridgeFuture<ThreadDetail>;
    /// `POST /v1/commands`: one command, its receipt. A retry must reuse the
    /// same command (same `commandId`, same content).
    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt>;
    /// `GET /v1/events?after=<cursor>`: replays committed events after
    /// `after`, then continues live, sending each to `sink`. Resolves when the
    /// stream ends: `Ok` when the Core closed it, an error otherwise
    /// ([`ErrorCode::ResyncRequired`] when the cursor is no longer served).
    fn subscribe(&self, after: i64, sink: EventSink) -> BridgeFuture<()>;
}

/// Event types the desktop folds (`internal/core/store.go`).
pub mod events {
    use vantry_protocol::RunStatus;

    pub const THREAD_CREATED: &str = "thread.created";
    pub const THREAD_UPDATED: &str = "thread.updated";
    pub const THREAD_DELETED: &str = "thread.deleted";
    pub const THREAD_REWOUND: &str = "thread.rewound";
    pub const RUN_QUEUED: &str = "run.queued";
    pub const RUN_STARTED: &str = "run.started";
    pub const RUN_OUTPUT: &str = "run.output";
    pub const RUN_REASONING: &str = "run.reasoning";
    pub const RUN_TELEMETRY: &str = "run.telemetry";
    pub const RUN_WAITING: &str = "run.waiting_children";
    pub const RUN_COMPLETED: &str = "run.completed";
    pub const RUN_CANCELLED: &str = "run.cancelled";
    pub const RUN_INTERRUPTED: &str = "run.interrupted";
    pub const RUN_FAILED: &str = "run.failed";
    pub const TOOL_UPDATED: &str = "tool.updated";
    pub const REQUEST_UPDATED: &str = "request.updated";
    pub const INPUT_UPDATED: &str = "input.updated";
    pub const MESSAGE_UPDATED: &str = "message.updated";
    pub const STEERING_UPDATED: &str = "message.steering";
    pub const TASK_CREATED: &str = "task.created";
    pub const TASK_UPDATED: &str = "task.updated";

    /// The status a `run.<status>` event type names.
    pub fn run_status(kind: &str) -> Option<RunStatus> {
        Some(match kind {
            RUN_QUEUED => RunStatus::Queued,
            RUN_STARTED => RunStatus::Running,
            RUN_WAITING => RunStatus::Waiting,
            RUN_COMPLETED => RunStatus::Completed,
            RUN_CANCELLED => RunStatus::Cancelled,
            RUN_INTERRUPTED => RunStatus::Interrupted,
            RUN_FAILED => RunStatus::Failed,
            _ => return None,
        })
    }

    /// The event type announcing `status`.
    pub fn of_run_status(status: RunStatus) -> &'static str {
        match status {
            RunStatus::Running => RUN_STARTED,
            RunStatus::Waiting => RUN_WAITING,
            RunStatus::Completed => RUN_COMPLETED,
            RunStatus::Cancelled => RUN_CANCELLED,
            RunStatus::Interrupted => RUN_INTERRUPTED,
            RunStatus::Failed => RUN_FAILED,
            RunStatus::Queued | RunStatus::Unspecified => RUN_QUEUED,
        }
    }
}

/// Stable error codes (`api.ErrorCode`), plus the desktop's transport ones.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ErrorCode {
    InvalidCommand,
    VersionMismatch,
    NotReady,
    NotFound,
    Conflict,
    ResyncRequired,
    Internal,
    /// The socket could not be reached or the connection broke.
    Unavailable,
    /// The response was not protocol v1.
    Protocol,
    /// A code the desktop does not know (an empty one included).
    Other(String),
}

impl ErrorCode {
    /// The code of `api.Error.code`.
    pub fn parse(code: &str) -> ErrorCode {
        match code {
            "invalid_command" => ErrorCode::InvalidCommand,
            "version_mismatch" => ErrorCode::VersionMismatch,
            "not_ready" => ErrorCode::NotReady,
            "not_found" => ErrorCode::NotFound,
            "conflict" => ErrorCode::Conflict,
            "resync_required" => ErrorCode::ResyncRequired,
            "internal_error" => ErrorCode::Internal,
            other => ErrorCode::Other(other.to_owned()),
        }
    }

    /// The protocol's code.
    pub fn from_protocol(code: protocol::ErrorCode) -> ErrorCode {
        use protocol::ErrorCode as P;
        match code {
            P::InvalidCommand => ErrorCode::InvalidCommand,
            P::VersionMismatch => ErrorCode::VersionMismatch,
            P::NotReady => ErrorCode::NotReady,
            P::NotFound => ErrorCode::NotFound,
            P::Conflict => ErrorCode::Conflict,
            P::ResyncRequired => ErrorCode::ResyncRequired,
            P::InternalError => ErrorCode::Internal,
            P::Unspecified => ErrorCode::Other(String::new()),
        }
    }

    fn as_str(&self) -> &str {
        match self {
            ErrorCode::InvalidCommand => "invalid_command",
            ErrorCode::VersionMismatch => "version_mismatch",
            ErrorCode::NotReady => "not_ready",
            ErrorCode::NotFound => "not_found",
            ErrorCode::Conflict => "conflict",
            ErrorCode::ResyncRequired => "resync_required",
            ErrorCode::Internal => "internal_error",
            ErrorCode::Unavailable => "unavailable",
            ErrorCode::Protocol => "protocol",
            ErrorCode::Other(code) if code.is_empty() => "unknown",
            ErrorCode::Other(code) => code,
        }
    }
}

/// A failed bridge call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BridgeError {
    pub code: ErrorCode,
    pub message: String,
}

impl BridgeError {
    pub fn new(code: ErrorCode, message: impl Into<String>) -> Self {
        BridgeError {
            code,
            message: message.into(),
        }
    }

    pub fn unavailable(message: impl Into<String>) -> Self {
        Self::new(ErrorCode::Unavailable, message)
    }

    pub fn protocol(message: impl Into<String>) -> Self {
        Self::new(ErrorCode::Protocol, message)
    }

    /// The Core said the event cursor is gone: reload the snapshot.
    pub fn is_resync(&self) -> bool {
        self.code == ErrorCode::ResyncRequired
    }

    /// Whether the outcome of a command is unknown (it may have committed):
    /// retry with the same command, never a new one. Transport failures, a
    /// broken response and the Core's internal errors are unknown; so is a
    /// code the desktop does not know (it cannot rule a commit out).
    pub fn outcome_unknown(&self) -> bool {
        matches!(
            self.code,
            ErrorCode::Unavailable | ErrorCode::Protocol | ErrorCode::Internal | ErrorCode::Other(_)
        )
    }
}

impl From<vantry_client::ClientError> for BridgeError {
    /// `ResyncRequired` keeps its meaning (reload the snapshot); the Core's
    /// API errors keep their code; a transport failure (refused, reset,
    /// undecodable) is [`ErrorCode::Unavailable`], whose outcome is unknown.
    fn from(error: vantry_client::ClientError) -> Self {
        use vantry_client::ClientError as E;
        match error {
            E::ResyncRequired(resync) => {
                BridgeError::new(ErrorCode::ResyncRequired, resync.message)
            }
            E::Api { code, message } => BridgeError::new(ErrorCode::from_protocol(code), message),
            E::Transport(error) => BridgeError::unavailable(format!("{error:#}")),
        }
    }
}

impl fmt::Display for BridgeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code.as_str(), self.message)
    }
}

impl std::error::Error for BridgeError {}

/// How the desktop reaches its Core.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Endpoint {
    /// The local Core's socket.
    Local { socket: std::path::PathBuf },
    /// The in-memory demo Core.
    Mock,
    /// A paired peer (`--peer`), `mode` auto | direct | relay.
    Peer { id: String, mode: String },
}

impl Endpoint {
    /// Whether the Core runs on another machine.
    pub fn is_remote(&self) -> bool {
        matches!(self, Endpoint::Peer { .. })
    }
}

/// The Core bridge for `endpoint`.
pub fn connect(endpoint: &Endpoint) -> Arc<dyn CoreBridge> {
    match endpoint {
        Endpoint::Local { socket } => Arc::new(ClientBridge::new(socket.clone())),
        Endpoint::Mock => Arc::new(MockBridge::demo()),
        Endpoint::Peer { .. } => {
            Arc::new(RemoteBridge::new(endpoint.clone(), None))
        }
    }
}

struct TunnelHandle {
    dir: std::path::PathBuf,
    socket: std::path::PathBuf,
    child: std::sync::Mutex<Option<std::process::Child>>,
}

impl Drop for TunnelHandle {
    fn drop(&mut self) {
        if let Ok(mut guard) = self.child.lock()
            && let Some(mut child) = guard.take()
        {
            let _ = child.kill();
            let _ = child.wait();
        }
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

/// Remote Core bridge for a paired host via the authenticated peer proxy.
///
/// The tunnel child is started lazily on first contact, automatically
/// restarted if it exits, and cleanly terminated when the bridge drops.
#[derive(Clone)]
pub struct RemoteBridge {
    endpoint: Endpoint,
    data_dir: Option<std::path::PathBuf>,
    tunnel: Arc<TunnelHandle>,
}

impl std::fmt::Debug for RemoteBridge {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RemoteBridge")
            .field("endpoint", &self.endpoint)
            .field("socket", &self.tunnel.socket)
            .finish()
    }
}

impl RemoteBridge {
    pub fn new(endpoint: Endpoint, data_dir: Option<std::path::PathBuf>) -> Self {
        let dir = std::env::temp_dir().join(format!(
            "vantry-remote-{}",
            uuid::Uuid::new_v4().simple()
        ));
        let socket = dir.join("core.sock");
        RemoteBridge {
            endpoint,
            data_dir,
            tunnel: Arc::new(TunnelHandle {
                dir,
                socket,
                child: std::sync::Mutex::new(None),
            }),
        }
    }

    /// Local proxy socket path forwarded to the remote Core.
    pub fn socket(&self) -> std::path::PathBuf {
        self.tunnel.socket.clone()
    }

    async fn ensure_tunnel(&self) -> Result<std::path::PathBuf, BridgeError> {
        let socket = self.tunnel.socket.clone();
        // Fast path: child already alive and socket exists.
        {
            let mut guard = self.tunnel.child.lock().unwrap();
            if let Some(child) = guard.as_mut() {
                match child.try_wait() {
                    Ok(None) if socket.exists() => return Ok(socket),
                    Ok(None) => {}
                    _ => {
                        *guard = None;
                    }
                }
            }
        }

        let mut needs_spawn = false;
        {
            let guard = self.tunnel.child.lock().unwrap();
            if guard.is_none() {
                needs_spawn = true;
            }
        }

        if needs_spawn {
            let child = match &self.endpoint {
                Endpoint::Peer { id, mode } => {
                    let locator = crate::service::Locator::from_env();
                    let exe = locator.find("vantry-service").ok_or_else(|| {
                        BridgeError::new(
                            ErrorCode::NotReady,
                            "找不到 vantry-service 可执行文件，无法建立配对主机代理",
                        )
                    })?;
                    let _ = std::fs::remove_file(&socket);
                    let mut command = std::process::Command::new(exe);
                    command.arg("peer-proxy");
                    if let Some(dir) = &self.data_dir {
                        command.arg("--data-dir").arg(dir);
                    }
                    command.args([
                            "--peer",
                            id,
                            "--mode",
                            mode,
                            "--socket",
                        ])
                        .arg(&socket)
                        .stdin(std::process::Stdio::null())
                        .stdout(std::process::Stdio::null())
                        .stderr(std::process::Stdio::null())
                        .spawn()
                        .map_err(|e| {
                            BridgeError::unavailable(format!("无法启动 peer-proxy：{e}"))
                        })?
                }
                _ => {
                    return Err(BridgeError::new(
                        ErrorCode::NotReady,
                        "当前配置不是远程端点",
                    ));
                }
            };
            *self.tunnel.child.lock().unwrap() = Some(child);
        }

        // Wait briefly for the tunnel socket to appear (or child to exit early).
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(8);
        loop {
            if socket.exists() {
                return Ok(socket);
            }
            {
                let mut guard = self.tunnel.child.lock().unwrap();
                if let Some(child) = guard.as_mut()
                    && let Ok(Some(status)) = child.try_wait()
                {
                    *guard = None;
                    return Err(BridgeError::unavailable(format!(
                        "{} 连接进程提前退出（{status}）",
                        self.label()
                    )));
                }
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(BridgeError::unavailable(format!(
                    "等待 {} 隧道就绪超时",
                    self.label()
                )));
            }
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        }
    }
}

impl CoreBridge for RemoteBridge {
    fn label(&self) -> String {
        match &self.endpoint {
            Endpoint::Peer { id, mode } => format!("配对主机 · {id}（{mode}）"),
            Endpoint::Local { socket } => socket.display().to_string(),
            Endpoint::Mock => "演示 Core".to_owned(),
        }
    }

    fn health(&self) -> BridgeFuture<Health> {
        let this = self.clone();
        Box::pin(async move {
            let sock = this.ensure_tunnel().await?;
            vantry_client::CoreClient::new(sock)
                .health()
                .await
                .map_err(BridgeError::from)
        })
    }

    fn snapshot(&self) -> BridgeFuture<Snapshot> {
        let this = self.clone();
        Box::pin(async move {
            let sock = this.ensure_tunnel().await?;
            vantry_client::CoreClient::new(sock)
                .snapshot()
                .await
                .map_err(BridgeError::from)
        })
    }

    fn thread_summaries(&self) -> BridgeFuture<ThreadSummaries> {
        let this = self.clone();
        Box::pin(async move {
            let sock = this.ensure_tunnel().await?;
            vantry_client::CoreClient::new(sock)
                .thread_summaries()
                .await
                .map_err(BridgeError::from)
        })
    }

    fn thread_detail(&self, thread_id: String, limit_runs: i64) -> BridgeFuture<ThreadDetail> {
        let this = self.clone();
        Box::pin(async move {
            let sock = this.ensure_tunnel().await?;
            vantry_client::CoreClient::new(sock)
                .thread_detail(&thread_id, limit_runs)
                .await
                .map_err(BridgeError::from)
        })
    }

    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt> {
        let this = self.clone();
        Box::pin(async move {
            let sock = this.ensure_tunnel().await?;
            vantry_client::CoreClient::new(sock)
                .dispatch(&command)
                .await
                .map_err(BridgeError::from)
        })
    }

    fn subscribe(&self, after: i64, sink: EventSink) -> BridgeFuture<()> {
        let this = self.clone();
        Box::pin(async move {
            let sock = this.ensure_tunnel().await?;
            let mut stream = vantry_client::CoreClient::new(sock)
                .subscribe(after.max(0))
                .await
                .map_err(BridgeError::from)?;
            loop {
                tokio::select! {
                    () = sink.closed() => return Ok(()),
                    next = stream.next_event() => match next.map_err(BridgeError::from)? {
                        Some(event) => {
                            if sink.send(event).is_err() {
                                return Ok(());
                            }
                        }
                        None => return Ok(()),
                    },
                }
            }
        })
    }
}

/// A new command id: `<prefix>-<uuid v4>`. It names a command; it is never
/// derived from the command's content, and a retry reuses it.
pub fn new_command_id(prefix: &str) -> String {
    format!("{prefix}-{}", uuid::Uuid::new_v4().simple())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn command_ids_are_unique_and_prefixed() {
        let a = new_command_id("send");
        let b = new_command_id("send");
        assert!(a.starts_with("send-"));
        assert_ne!(a, b);
    }

    #[test]
    fn error_codes_parse_and_classify() {
        assert_eq!(
            ErrorCode::parse("resync_required"),
            ErrorCode::ResyncRequired
        );
        assert_eq!(
            ErrorCode::parse("brand_new"),
            ErrorCode::Other("brand_new".into())
        );
        let err = BridgeError::new(ErrorCode::ResyncRequired, "cursor gone");
        assert!(err.is_resync());
        assert!(!err.outcome_unknown());
        assert!(BridgeError::unavailable("eof").outcome_unknown());
        assert!(BridgeError::new(ErrorCode::Internal, "x").outcome_unknown());
        assert!(!BridgeError::new(ErrorCode::Conflict, "x").outcome_unknown());
        assert!(!BridgeError::new(ErrorCode::InvalidCommand, "x").outcome_unknown());
        assert_eq!(err.to_string(), "resync_required: cursor gone");
    }

    #[test]
    fn client_errors_map_to_bridge_errors() {
        use vantry_client::ClientError;
        let resync: BridgeError = ClientError::ResyncRequired(protocol::ResyncRequired {
            message: "cursor 3 expired".into(),
        })
        .into();
        assert!(resync.is_resync());
        assert_eq!(resync.message, "cursor 3 expired");

        let conflict: BridgeError = ClientError::Api {
            code: protocol::ErrorCode::Conflict,
            message: "reused".into(),
        }
        .into();
        assert_eq!(conflict.code, ErrorCode::Conflict);
        assert!(!conflict.outcome_unknown());

        let internal: BridgeError = ClientError::Api {
            code: protocol::ErrorCode::InternalError,
            message: "db".into(),
        }
        .into();
        assert!(internal.outcome_unknown());

        let transport: BridgeError =
            ClientError::Transport(anyhow::anyhow!("connection reset")).into();
        assert_eq!(transport.code, ErrorCode::Unavailable);
        assert!(transport.outcome_unknown());
        assert!(transport.message.contains("connection reset"));
    }

    #[test]
    fn run_status_events_round_trip() {
        use protocol::RunStatus;
        for status in [
            RunStatus::Queued,
            RunStatus::Running,
            RunStatus::Waiting,
            RunStatus::Completed,
            RunStatus::Cancelled,
            RunStatus::Interrupted,
            RunStatus::Failed,
        ] {
            assert_eq!(events::run_status(events::of_run_status(status)), Some(status));
        }
        assert_eq!(events::run_status("run.output"), None);
    }

    #[tokio::test]
    async fn remote_endpoints_validate_and_label_tunnels() {
        let peer = connect(&Endpoint::Peer {
            id: "peer-7".into(),
            mode: "relay".into(),
        });
        assert!(peer.label().contains("peer-7"));
        assert!(Endpoint::Peer {
            id: "x".into(),
            mode: "auto".into()
        }
        .is_remote());
        assert!(!Endpoint::Mock.is_remote());
    }
}
