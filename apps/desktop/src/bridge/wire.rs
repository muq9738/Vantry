//! Vantry protocol v1 wire types, as far as the desktop consumes them.
//!
//! These mirror the Go baseline `pkg/api` (v0.1.42) field by field so the
//! desktop can talk to an existing Core today. Once `vantry-protocol` ships its
//! DTOs, these become re-exports of that crate; the field names and JSON
//! shapes are the contract and must not drift from it.

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// Protocol version every command carries (`api.ProtocolVersion`), as
/// `vantry-protocol` declares it.
pub const PROTOCOL_VERSION: u32 = vantry_protocol::PROTOCOL_VERSION;

fn is_false(value: &bool) -> bool {
    !*value
}

/// Command kinds the desktop sends (`api.CommandKind`).
pub mod kind {
    pub const CREATE_THREAD: &str = "thread.create";
    pub const SEND_MESSAGE: &str = "message.send";
    pub const QUEUE_MESSAGE: &str = "message.queue";
    pub const STEER_MESSAGE: &str = "message.steer";
    pub const STOP_RUN: &str = "run.stop";
    pub const RESPOND_REQUEST: &str = "request.respond";
}

/// Event types the desktop folds (`internal/core/store.go`).
pub mod event {
    pub const THREAD_CREATED: &str = "thread.created";
    pub const THREAD_UPDATED: &str = "thread.updated";
    pub const THREAD_DELETED: &str = "thread.deleted";
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
}

/// `api.WorkspaceSpec`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct WorkspaceSpec {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub path: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub repository: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub branch: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub base_ref: String,
}

/// `api.Command`: only the fields the desktop sets. The Go server decodes
/// with `DisallowUnknownFields`, so every field here exists there, and empty
/// ones are omitted exactly as `omitempty` does.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Command {
    pub version: u32,
    pub command_id: String,
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub thread_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub title: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
    #[serde(skip_serializing_if = "is_false")]
    pub fallback_to_queue: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub request_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub value: String,
    #[serde(skip_serializing_if = "is_false")]
    pub confirmed: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub cancelled: bool,
}

impl Command {
    /// A v1 command of `kind` with `command_id`.
    pub fn new(kind: &str, command_id: impl Into<String>) -> Self {
        Command {
            version: PROTOCOL_VERSION,
            command_id: command_id.into(),
            kind: kind.to_owned(),
            ..Command::default()
        }
    }
}

/// `api.Receipt`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Receipt {
    pub command_id: String,
    pub accepted: bool,
    pub thread_id: String,
    pub run_id: String,
    pub sequence: i64,
    pub message_id: String,
    pub input_id: String,
    pub request_id: String,
    pub delivery: String,
}

/// `api.Thread`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Thread {
    pub id: String,
    pub title: String,
    pub created_at: String,
    pub project_path: String,
    pub provider_id: String,
    pub model: String,
    pub workspace: Option<WorkspaceSpec>,
    pub parent_thread_id: String,
    pub pinned: bool,
    pub archived: bool,
    pub approval_policy: String,
    pub interaction_mode: String,
}

/// `api.RunStatus`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum RunStatus {
    #[default]
    Queued,
    Running,
    #[serde(rename = "waiting_children")]
    Waiting,
    Completed,
    Cancelled,
    Interrupted,
    Failed,
    /// A status a newer Core added: shown as is, never treated as terminal.
    #[serde(other)]
    Unknown,
}

impl RunStatus {
    /// `RunStatus.Terminal()`.
    pub fn terminal(self) -> bool {
        matches!(
            self,
            RunStatus::Completed
                | RunStatus::Cancelled
                | RunStatus::Interrupted
                | RunStatus::Failed
        )
    }

    /// The status a `run.<status>` event type names.
    pub fn from_event_type(kind: &str) -> Option<RunStatus> {
        Some(match kind {
            event::RUN_QUEUED => RunStatus::Queued,
            event::RUN_STARTED => RunStatus::Running,
            event::RUN_WAITING => RunStatus::Waiting,
            event::RUN_COMPLETED => RunStatus::Completed,
            event::RUN_CANCELLED => RunStatus::Cancelled,
            event::RUN_INTERRUPTED => RunStatus::Interrupted,
            event::RUN_FAILED => RunStatus::Failed,
            _ => return None,
        })
    }
}

/// `api.RunTelemetry` (the fields the status line shows).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct RunTelemetry {
    pub phase: String,
    pub compacting: bool,
    pub retrying: bool,
    pub turn_count: i64,
    pub total_tokens: i64,
    pub last_context_tokens: i64,
    pub duration_ms: i64,
}

/// `api.Run`.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Run {
    pub id: String,
    pub thread_id: String,
    pub attempt_id: String,
    pub status: RunStatus,
    pub input: String,
    pub output: String,
    pub reasoning: String,
    pub interaction_mode: String,
    pub error: String,
    pub created_at: String,
    pub updated_at: String,
    pub model: String,
    pub telemetry: Option<RunTelemetry>,
}

/// `api.ToolCall`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct ToolCall {
    pub id: String,
    pub thread_id: String,
    pub run_id: String,
    pub attempt_id: String,
    pub name: String,
    pub arguments: Option<Value>,
    pub output: String,
    /// running | completed | failed | interrupted
    pub status: String,
    pub updated_at: String,
    /// Byte length of the run's output when the call started; -1 unknown
    /// (older records omit it, and 0 is a real offset).
    pub output_offset: i64,
}

impl Default for ToolCall {
    fn default() -> Self {
        ToolCall {
            id: String::new(),
            thread_id: String::new(),
            run_id: String::new(),
            attempt_id: String::new(),
            name: String::new(),
            arguments: None,
            output: String::new(),
            status: String::new(),
            updated_at: String::new(),
            output_offset: -1,
        }
    }
}

/// `api.ProviderRequest`: an extension UI request (approval, choice, input).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct ProviderRequest {
    pub id: String,
    pub provider_request_id: String,
    pub thread_id: String,
    pub run_id: String,
    pub attempt_id: String,
    /// confirm | select | input | editor
    pub method: String,
    pub title: String,
    pub message: String,
    pub options: Vec<String>,
    pub prefill: String,
    /// pending | responding | responded | expired | failed
    pub status: String,
    pub error: String,
    pub created_at: String,
    pub updated_at: String,
}

/// `api.Snapshot` (the parts the desktop shows).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Snapshot {
    pub sequence: i64,
    pub threads: Vec<Thread>,
    pub runs: Vec<Run>,
    pub tools: Vec<ToolCall>,
    pub requests: Vec<ProviderRequest>,
}

/// `api.Event`: global, strictly increasing `sequence`.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Event {
    pub sequence: i64,
    pub id: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub thread_id: String,
    pub run_id: String,
    pub attempt_id: String,
    pub timestamp: String,
    pub data: Value,
}

/// `api.Health`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct Health {
    pub version: u32,
    pub state: String,
}

/// `api.Error`: `{ "code", "message" }`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct ApiError {
    pub code: String,
    pub message: String,
}

/// One NDJSON record of `/v1/events` (`streamEnvelope`).
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(default)]
pub struct StreamEnvelope {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub event: Option<Event>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<ApiError>,
}
