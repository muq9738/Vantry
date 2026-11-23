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
//! - [`RemoteBridge`]: the `--remote` / `--peer` route, until the remote
//!   transport lands (it reports `not_ready`, the window still opens).
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
use vantry_protocol::{Command, CoreEvent, Health, Receipt, Snapshot};
pub use workbench::{LocalWorkbench, MockWorkbench, WorkbenchBridge};

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
    /// A Core over SSH (`--remote`).
    Ssh {
        host: String,
        remote_socket: Option<String>,
    },
    /// A paired peer (`--peer`), `mode` auto | direct | relay.
    Peer { id: String, mode: String },
}

impl Endpoint {
    /// Whether the Core runs on another machine.
    pub fn is_remote(&self) -> bool {
        matches!(self, Endpoint::Ssh { .. } | Endpoint::Peer { .. })
    }
}

/// The Core bridge for `endpoint`.
pub fn connect(endpoint: &Endpoint) -> Arc<dyn CoreBridge> {
    match endpoint {
        Endpoint::Local { socket } => Arc::new(ClientBridge::new(socket.clone())),
        Endpoint::Mock => Arc::new(MockBridge::demo()),
        Endpoint::Ssh { .. } | Endpoint::Peer { .. } => {
            Arc::new(RemoteBridge::new(endpoint.clone()))
        }
    }
}

/// The `--remote` / `--peer` route. The remote transport (SSH forwarding,
/// pairing, relay) is not in the desktop yet: every call answers
/// `not_ready` with what it would connect to, so the window opens, shows
/// the target and keeps retrying instead of exiting.
#[derive(Debug, Clone)]
pub struct RemoteBridge {
    endpoint: Endpoint,
}

impl RemoteBridge {
    pub fn new(endpoint: Endpoint) -> Self {
        RemoteBridge { endpoint }
    }

    fn not_ready<T: Send + 'static>(&self) -> BridgeFuture<T> {
        let message = format!("{} 的远程传输尚未接入 NSH 桌面", self.label());
        Box::pin(async move { Err(BridgeError::new(ErrorCode::NotReady, message)) })
    }
}

impl CoreBridge for RemoteBridge {
    fn label(&self) -> String {
        match &self.endpoint {
            Endpoint::Ssh {
                host,
                remote_socket,
            } => match remote_socket {
                Some(socket) => format!("SSH · {host}:{socket}"),
                None => format!("SSH · {host}"),
            },
            Endpoint::Peer { id, mode } => format!("配对主机 · {id}（{mode}）"),
            Endpoint::Local { socket } => socket.display().to_string(),
            Endpoint::Mock => "演示 Core".to_owned(),
        }
    }

    fn health(&self) -> BridgeFuture<Health> {
        self.not_ready()
    }

    fn snapshot(&self) -> BridgeFuture<Snapshot> {
        self.not_ready()
    }

    fn dispatch(&self, _command: Command) -> BridgeFuture<Receipt> {
        self.not_ready()
    }

    fn subscribe(&self, _after: i64, _sink: EventSink) -> BridgeFuture<()> {
        self.not_ready()
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
    async fn remote_endpoints_route_to_a_not_ready_bridge() {
        let bridge = connect(&Endpoint::Ssh {
            host: "build-box".into(),
            remote_socket: Some("/run/vantry/core.sock".into()),
        });
        assert_eq!(bridge.label(), "SSH · build-box:/run/vantry/core.sock");
        let err = bridge.snapshot().await.unwrap_err();
        assert_eq!(err.code, ErrorCode::NotReady);
        assert!(!err.outcome_unknown());
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
