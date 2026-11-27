//! Vantry v1 contracts. Required Go slices retain the distinction between null and [].
use serde::{Deserialize, Serialize};

mod api;
mod provider;
mod thread_link;
pub use thread_link::{THREAD_SCHEME, ThreadLink, ThreadTarget, thread_markdown};
pub use api::*;
pub use provider::*;

pub const PROTOCOL_VERSION: u32 = 1;
pub const GEMINI_WRITING_PROVIDER: &str = "gemini-writing";
pub const GEMINI_WRITING_MODEL: &str = "gemini-writing/neon-high";

/// The bound browser thread and explicitly selected Gemini Web backends forward user text verbatim.
pub fn uses_gemini_web(provider: &str, model: &str) -> bool {
    if provider == GEMINI_WRITING_PROVIDER {
        return true;
    }
    if !matches!(provider, "pi" | "") {
        return false;
    }
    let route = model.strip_prefix("magpie/").unwrap_or(model);
    route.split_once('/').is_some_and(|(backend, id)| {
        matches!(backend, "gemini-web" | "gemini-web-bridge") && !id.is_empty()
    })
}

pub const EMBEDDED_PI_CAPABILITY: &str = "provider.pi.embedded.v1";
pub const LEGACY_PI_RUNTIME_CAPABILITY: &str = "provider.pi.runtime.v1";
/// Opt in to NDJSON keepalive records on `/v1/events`. Supporting servers echo `1`.
pub const EVENT_KEEPALIVE_HEADER: &str = "x-vantry-event-keepalive";
pub const EVENT_IDLE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);
pub const LOCAL_TOKEN_HEADER: &str = "x-vantry-local-token";

/// Loopback TCP descriptor written to `core.sock` on platforms without Unix
/// domain sockets in Rust's async runtime (Windows) or for TCP-forwarded cores.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct LocalTcpEndpoint {
    pub transport: String,
    pub addr: String,
    pub token: String,
}

impl LocalTcpEndpoint {
    pub fn new(addr: impl Into<String>, token: impl Into<String>) -> Self {
        Self {
            transport: "tcp".into(),
            addr: addr.into(),
            token: token.into(),
        }
    }

    pub fn parse_bytes(bytes: &[u8]) -> Option<Self> {
        let ep: Self = serde_json::from_slice(bytes).ok()?;
        if ep.transport == "tcp" && !ep.addr.is_empty() && ep.token.len() >= 16 {
            Some(ep)
        } else {
            None
        }
    }

    pub fn verify_token(&self, candidate: &str) -> bool {
        let a = self.token.as_bytes();
        let b = candidate.as_bytes();
        if a.len() != b.len() || a.is_empty() {
            return false;
        }
        let mut diff = 0u8;
        for (&x, &y) in a.iter().zip(b.iter()) {
            diff |= x ^ y;
        }
        diff == 0
    }
}

pub type Capabilities = ServerStatus;
pub type Message = QueuedMessage;

/// Identity and state of one execution of a run (the v1 snapshot embeds its ID in Run).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Attempt {
    pub id: String,
    pub run_id: String,
    pub ordinal: i64,
    pub status: RunStatus,
    pub input: String,
    pub provider_cursor: i64,
    pub created_at: String,
    pub updated_at: String,
}

/// Data carried by `run.output`, `run.reasoning` and `tool.updated` Core events.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum ReplyPart {
    Text {
        delta: String,
        #[serde(default, skip_serializing_if = "String::is_empty")]
        output: String,
    },
    Reasoning {
        #[serde(rename = "reasoningDelta")]
        reasoning_delta: String,
    },
    Tool {
        tool: ToolCall,
    },
}

/// A subscription failure with ErrorCode::ResyncRequired requires a fresh snapshot.
#[derive(Debug, Clone, thiserror::Error, Serialize, Deserialize)]
#[serde(tag = "code", rename = "resync_required")]
#[error("{message}")]
pub struct ResyncRequired {
    pub message: String,
}

/// Thread created on the execution host for a federated thread on its owner Core.
pub fn federated_thread_id(owner_thread_id: &str) -> String {
    format!("remote_{owner_thread_id}")
}

impl ServerStatus {
    pub fn has_capability(&self, name: &str) -> bool {
        self.capabilities.iter().flatten().any(|s| s == name)
    }
}

impl Command {
    pub fn new(command_id: impl Into<String>, kind: CommandKind) -> Self {
        Self {
            version: PROTOCOL_VERSION.into(),
            command_id: command_id.into(),
            kind,
            ..Self::default()
        }
    }
}

impl RunStatus {
    pub fn terminal(self) -> bool {
        matches!(
            self,
            Self::Cancelled | Self::Completed | Self::Failed | Self::Interrupted
        )
    }
}

fn present_json<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<serde_json::Value>, D::Error> {
    serde_json::Value::deserialize(deserializer).map(Some)
}
fn go_time<S: serde::Serializer>(value: &str, serializer: S) -> Result<S::Ok, S::Error> {
    serializer.serialize_str(if value.is_empty() {
        "0001-01-01T00:00:00Z"
    } else {
        value
    })
}
fn is_zero<T: Default + PartialEq>(value: &T) -> bool {
    value == &T::default()
}
fn unknown_offset() -> i64 {
    -1
}
