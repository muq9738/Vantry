//! The desktop's only way to the Core: [`CoreBridge`].
//!
//! The UI never talks to a socket directly. It holds an
//! `Arc<dyn CoreBridge>` and runs its futures on the runtime's threads
//! (`nsh_core::prelude::background`), so the engine thread never blocks.
//!
//! Implementations:
//! - [`UnixHttpBridge`]: protocol v1 over HTTP/1.1 on the Core's Unix socket
//!   (`<data dir>/core.sock`), self-contained so the desktop builds and runs
//!   against an existing Core today.
//! - [`MockBridge`]: an in-memory Core for `--mock` runs and tests.
//!
//! When `vantry-client` lands, its client implements this trait (or the
//! trait moves there) and the desktop switches by constructing it in
//! [`connect`]; nothing above this module changes.

mod mock;
mod unix_http;
pub mod wire;

use std::{
    fmt,
    future::Future,
    pin::Pin,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::{SystemTime, UNIX_EPOCH},
};

// `vantry-client` is the bridge's successor (see the module docs); it stays
// linked so switching is a change in `connect`, not in the manifest.
use vantry_client as _;

pub use mock::MockBridge;
pub use unix_http::UnixHttpBridge;
use wire::{Command, Event, Health, Receipt, Snapshot};

/// A bridge call's future: `Send`, so it runs on the runtime's threads.
pub type BridgeFuture<T> = Pin<Box<dyn Future<Output = Result<T, BridgeError>> + Send + 'static>>;

/// Where a subscription delivers events, in order.
pub type EventSink = tokio::sync::mpsc::UnboundedSender<Event>;

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
    /// A code a newer Core added.
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
    /// retry with the same command, never a new one.
    pub fn outcome_unknown(&self) -> bool {
        matches!(
            self.code,
            ErrorCode::Unavailable | ErrorCode::Protocol | ErrorCode::Internal
        )
    }
}

impl fmt::Display for BridgeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let code = match &self.code {
            ErrorCode::InvalidCommand => "invalid_command",
            ErrorCode::VersionMismatch => "version_mismatch",
            ErrorCode::NotReady => "not_ready",
            ErrorCode::NotFound => "not_found",
            ErrorCode::Conflict => "conflict",
            ErrorCode::ResyncRequired => "resync_required",
            ErrorCode::Internal => "internal_error",
            ErrorCode::Unavailable => "unavailable",
            ErrorCode::Protocol => "protocol",
            ErrorCode::Other(code) => code,
        };
        write!(f, "{code}: {}", self.message)
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
}

/// The bridge for `endpoint`.
pub fn connect(endpoint: &Endpoint) -> Arc<dyn CoreBridge> {
    match endpoint {
        Endpoint::Local { socket } => Arc::new(UnixHttpBridge::new(socket.clone())),
        Endpoint::Mock => Arc::new(MockBridge::demo()),
    }
}

/// A new command id: `<prefix>-<millis>-<pid>-<counter>`. Unique per process
/// and across restarts; it names a command, it is not derived from content.
pub fn new_command_id(prefix: &str) -> String {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or_default();
    let n = COUNTER.fetch_add(1, Ordering::Relaxed);
    format!("{prefix}-{millis}-{}-{n}", std::process::id())
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
        assert_eq!(err.to_string(), "resync_required: cursor gone");
    }
}
