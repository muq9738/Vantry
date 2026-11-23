//! Vantry v1 contracts. Required Go slices retain the distinction between null and [].
use serde::{Deserialize, Serialize};

mod api;
mod provider;
pub use api::*;
pub use provider::*;

pub const PROTOCOL_VERSION: u32 = 1;
pub const EMBEDDED_PI_CAPABILITY: &str = "provider.pi.embedded.v1";
pub const LEGACY_PI_RUNTIME_CAPABILITY: &str = "provider.pi.runtime.v1";

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
