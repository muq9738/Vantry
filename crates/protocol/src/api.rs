//! Wire types corresponding to `pkg/api` in Vantry v0.1.42.
use super::*;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
pub enum CommandKind {
    #[default]
    #[serde(rename = "")]
    Unspecified,
    #[serde(rename = "project.chat.open")]
    OpenProjectChat,
    #[serde(rename = "thread.create")]
    CreateThread,
    #[serde(rename = "thread.delete")]
    DeleteThread,
    #[serde(rename = "thread.fork")]
    ForkThread,
    #[serde(rename = "thread.rewind")]
    RewindThread,
    #[serde(rename = "thread.merge_back")]
    MergeBackThread,
    #[serde(rename = "input.edit")]
    EditInput,
    #[serde(rename = "input.reorder")]
    ReorderInput,
    #[serde(rename = "input.promote_steer")]
    PromoteInputToSteer,
    #[serde(rename = "thread.model")]
    SetThreadModel,
    #[serde(rename = "input.cancel")]
    CancelInput,
    #[serde(rename = "run.steer")]
    SteerRun,
    #[serde(rename = "request.respond")]
    RespondRequest,
    #[serde(rename = "message.send")]
    SendMessage,
    #[serde(rename = "run.stop")]
    StopRun,
    #[serde(rename = "task.delegate")]
    DelegateTask,
    #[serde(rename = "task.followup")]
    FollowUpTask,
    #[serde(rename = "task.acknowledge")]
    AcknowledgeTask,
    #[serde(rename = "task.dispose")]
    DisposeTask,
    #[serde(rename = "task.cancel")]
    CancelTask,
    #[serde(rename = "task.wake_upgrade")]
    UpgradeTaskWake,
    #[serde(rename = "thread.inter_send")]
    InterThreadSend,
    #[serde(rename = "thread.update")]
    UpdateThread,
    #[serde(rename = "message.queue")]
    QueueMessage,
    #[serde(rename = "message.cancel")]
    CancelMessage,
    #[serde(rename = "message.steer")]
    SteerMessage,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
pub enum RunStatus {
    #[default]
    #[serde(rename = "")]
    Unspecified,
    #[serde(rename = "queued")]
    Queued,
    #[serde(rename = "running")]
    Running,
    #[serde(rename = "waiting_children")]
    Waiting,
    #[serde(rename = "completed")]
    Completed,
    #[serde(rename = "cancelled")]
    Cancelled,
    #[serde(rename = "interrupted")]
    Interrupted,
    #[serde(rename = "failed")]
    Failed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
pub enum DeliveryStatus {
    #[default]
    #[serde(rename = "")]
    Unspecified,
    #[serde(rename = "pending")]
    Pending,
    #[serde(rename = "delivered")]
    Delivered,
    #[serde(rename = "acknowledged")]
    Acknowledged,
    #[serde(rename = "disposed")]
    Disposed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
pub enum ErrorCode {
    #[default]
    #[serde(rename = "")]
    Unspecified,
    #[serde(rename = "invalid_command")]
    InvalidCommand,
    #[serde(rename = "version_mismatch")]
    VersionMismatch,
    #[serde(rename = "not_ready")]
    NotReady,
    #[serde(rename = "not_found")]
    NotFound,
    #[serde(rename = "conflict")]
    Conflict,
    #[serde(rename = "resync_required")]
    ResyncRequired,
    #[serde(rename = "internal_error")]
    InternalError,
    /// A paired host rejected this device's credential (revoked or reset):
    /// the user pairs again with a new code.
    #[serde(rename = "unauthorized")]
    Unauthorized,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Command {
    pub version: i64,
    pub command_id: String,
    pub kind: CommandKind,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub read_state: Option<ThreadReadChange>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub title: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source_run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub target_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sender_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub created_by_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub created_by_run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delivery_mode: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub position: i64,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub fallback_to_queue: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_attempt_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub task_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub generation: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delivery_message_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resume_policy: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pinned: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub archived: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub snoozed_until: Option<String>,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub force: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message_id: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub attachments: Vec<Attachment>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub input_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub value: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub confirmed: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub cancelled: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Receipt {
    pub command_id: String,
    pub accepted: bool,
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    pub sequence: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub input_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub task_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub generation: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delivery_message_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delivery: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadReadChange {
    pub unread: bool,
    pub observed_sequence: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Thread {
    /// Zero means read. Older Cores omit the shared marker.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub unread_sequence: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_path: String,
    pub id: String,
    pub title: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_task_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub created_by_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub created_by_run_id: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub pinned: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub archived: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub snoozed_until: Option<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub forked_from_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub forked_from_run_id: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceSpec {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub repository: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub branch: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub base_ref: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RunTelemetry {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub phase: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub compacting: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub compaction_reason: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub retrying: bool,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub retry_attempt: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub retry_max_attempts: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub retry_reason: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub retry_wait_seconds: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub retry_count: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub turn_count: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub input_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub output_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cache_read_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cache_write_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub reasoning_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub total_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub last_context_tokens: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cost_usd: f64,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub cost_estimated: bool,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub ttft_ms: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub duration_ms: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Run {
    pub id: String,
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sender_thread_id: String,
    pub attempt_id: String,
    pub status: RunStatus,
    pub input: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub attachments: Vec<Attachment>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    pub output: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reasoning: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub telemetry: Option<RunTelemetry>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Task {
    pub id: String,
    pub parent_thread_id: String,
    pub parent_run_id: String,
    pub child_thread_id: String,
    pub child_run_id: String,
    pub title: String,
    pub generation: i64,
    pub status: RunStatus,
    pub delivery_status: DeliveryStatus,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delivery_message_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub result: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Cohort {
    pub parent_run_id: String,
    pub generation: i64,
    pub resume_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delivery_message_id: String,
    pub status: DeliveryStatus,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub continuation_attempt_id: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
pub enum RunBlockKind {
    #[default]
    #[serde(rename = "")]
    Unspecified,
    #[serde(rename = "user")]
    User,
    #[serde(rename = "reasoning")]
    Reasoning,
    #[serde(rename = "text")]
    Text,
    #[serde(rename = "tool")]
    Tool,
    #[serde(rename = "steer")]
    Steer,
    #[serde(rename = "approval")]
    Approval,
    #[serde(rename = "status")]
    Status,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RunBlock {
    pub id: String,
    pub thread_id: String,
    pub run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub attempt_id: String,
    pub seq: i64,
    pub kind: RunBlockKind,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub ref_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub text: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub attachments: Vec<Attachment>,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Snapshot {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub project_updates: Vec<ProjectUpdate>,
    pub sequence: i64,
    pub threads: Option<Vec<Thread>>,
    pub runs: Option<Vec<Run>>,
    pub tasks: Option<Vec<Task>>,
    pub cohorts: Option<Vec<Cohort>>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub messages: Vec<QueuedMessage>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub steering: Vec<SteeringMessage>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub inputs: Vec<Input>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<ToolCall>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub requests: Vec<PendingRequest>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub blocks: Vec<RunBlock>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CoreEvent {
    pub sequence: i64,
    pub id: String,
    pub r#type: String,
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub attempt_id: String,
    #[serde(serialize_with = "go_time")]
    pub timestamp: String,
    pub data: serde_json::Value,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Health {
    pub version: i64,
    pub state: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ApiError {
    pub code: ErrorCode,
    pub message: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ServerStatus {
    pub protocol_version: i64,
    pub state: String,
    pub schema_version: i64,
    pub pid: i64,
    pub active_runs: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub version: String,
    pub capabilities: Option<Vec<String>>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadSummary {
    /// Zero means read. Older Cores omit the shared marker.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub unread_sequence: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_path: String,
    pub id: String,
    pub title: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_task_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub created_by_thread_id: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub pinned: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub archived: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub snoozed_until: Option<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
    pub run_count: i64,
    pub active_runs: i64,
    pub pending_inputs: i64,
    pub pending_requests: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub latest_run_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub latest_status: Option<RunStatus>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadSummaries {
    pub sequence: i64,
    pub threads: Option<Vec<ThreadSummary>>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadDetailQuery {
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub limit_runs: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadDetail {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub project_updates: Vec<ProjectUpdate>,
    pub sequence: i64,
    pub thread: Thread,
    pub runs: Option<Vec<Run>>,
    pub tasks: Option<Vec<Task>>,
    pub inputs: Option<Vec<Input>>,
    pub tools: Option<Vec<ToolCall>>,
    pub requests: Option<Vec<PendingRequest>>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub blocks: Vec<RunBlock>,
    pub run_count: i64,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub truncated_runs: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EnvironmentInfo {
    pub id: String,
    pub name: String,
    pub os: String,
    pub arch: String,
    pub home: String,
    pub provider_id: String,
    pub default_model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EnvironmentDirectory {
    pub name: String,
    pub path: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EnvironmentDirectories {
    pub path: String,
    pub parent: String,
    pub entries: Option<Vec<EnvironmentDirectory>>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MCPServerInfo {
    pub name: String,
    pub scope: String,
    pub transport: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub endpoint: String,
    pub enabled: bool,
    pub exposure: String,
    #[serde(default, skip_serializing_if = "std::collections::BTreeMap::is_empty")]
    pub tool_exposure: std::collections::BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub description: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub live_state: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub live_error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MCPCatalog {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub workspace: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub user_file: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub project_file: String,
    pub project_trusted: bool,
    pub codemode_default: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub probed: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub probe_error: String,
    pub servers: Option<Vec<MCPServerInfo>>,
    pub notice: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OrchestratorModelOption {
    pub id: String,
    pub values: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub default: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OrchestratorModelCapability {
    pub id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub label: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub options: Vec<OrchestratorModelOption>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OrchestratorCapabilitiesQuery {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub search: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OrchestratorCapabilities {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_thread_id: String,
    pub inherited_provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub inherited_model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub inherited_thinking: String,
    pub approval_policy: String,
    pub interaction_mode: String,
    pub models: Option<Vec<OrchestratorModelCapability>>,
    pub approval_policies: Option<Vec<String>>,
    pub interaction_modes: Option<Vec<String>>,
    pub max_batch_threads: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PendingApprovalSummary {
    pub request_id: String,
    pub method: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub title: String,
    pub run_id: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TaskDetail {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub child_attempt_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub remote_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub remote_run_id: String,
    pub task_id: String,
    pub parent_thread_id: String,
    pub parent_run_id: String,
    pub child_thread_id: String,
    pub link: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub child_run_id: String,
    pub title: String,
    pub status: RunStatus,
    pub work_state: String,
    pub delivery_status: DeliveryStatus,
    pub has_pending_child_runs: bool,
    pub has_pending_question: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pending_approval: Option<PendingApprovalSummary>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub summary: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub wait_timed_out: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub next: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DelegateTaskRequest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    pub parent_run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_attempt_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub title: String,
    pub task: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub role: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub thinking: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub isolation: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mode: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub timeout_ms: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resume_policy: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TaskStatusQuery {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub task_id: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub acknowledge: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TaskStatusResult {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    pub tasks: Option<Vec<TaskDetail>>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub cohorts: Vec<Cohort>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CancelTaskRequest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    pub task_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CancelTaskResult {
    pub task_id: String,
    pub status: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadListItem {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    pub thread_id: String,
    pub title: String,
    pub link: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub workspace_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub branch: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub is_worktree: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub parent_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub created_by_thread_id: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub pinned: bool,
    pub settled: bool,
    pub snoozed: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub snoozed_until: Option<String>,
    pub run_count: i64,
    pub pending_inputs: i64,
    pub pending_requests: i64,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadListQuery {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub workspace: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub all_projects: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub include_subagents: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub settled: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub snoozed: Option<bool>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub statuses: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub title_contains: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cursor: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub limit: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadListResult {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub current_thread_id: String,
    pub threads: Option<Vec<ThreadListItem>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub next_cursor: Option<i64>,
    pub total: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadTimelineItem {
    pub position: i64,
    pub item_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    pub created_by: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sender_thread_id: String,
    pub r#type: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub title: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub text: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub text_truncated: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub next_text_offset: Option<i64>,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadRunSummary {
    pub run_id: String,
    pub ordinal: i64,
    pub status: RunStatus,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sender_thread_id: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadReadQuery {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub view: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub after_position: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub before_position: Option<i64>,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub limit: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub run_limit: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub max_chars_per_item: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub item_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub text_offset: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadReadResult {
    pub thread: ThreadListItem,
    pub recent_runs: Option<Vec<ThreadRunSummary>>,
    pub items: Option<Vec<ThreadTimelineItem>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub next_position: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub earlier_position: Option<i64>,
    pub has_more: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadLaunchSpec {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub title: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub thinking: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub scratch: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadLaunchRequest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_run_id: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub notify_on_complete: bool,
    #[serde(flatten)]
    pub spec: ThreadLaunchSpec,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct LaunchedThread {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    pub thread_id: String,
    pub link: String,
    pub title: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    pub status: RunStatus,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateThreadsRequest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_run_id: String,
    pub threads: Option<Vec<ThreadLaunchSpec>>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateThreadsResult {
    pub threads: Option<Vec<LaunchedThread>>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct InterThreadSendRequest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    pub target_thread_id: String,
    pub message: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mode: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub notify_on_complete: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct InterThreadSendResult {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    pub thread_id: String,
    pub link: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub input_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub task_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub status: Option<RunStatus>,
    pub delivery: String,
    pub sequence: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadWaitQuery {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub caller_thread_id: String,
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub timeout_ms: i64,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub until_delegated_work_settled: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ThreadWaitResult {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub environment_id: String,
    pub thread_id: String,
    pub link: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    pub status: String,
    pub timed_out: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub auto_wake_registered: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub summary: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub has_pending_question: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pending_approval: Option<PendingApprovalSummary>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub next: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PiRuntimeStatus {
    pub active_source: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub active_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub active_version: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub custom_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub system_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub system_version: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub system_is_npm: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub node_version: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub required_node: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub npm_available: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub latest_version: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub update_available: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub compat_verified: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub compat_blocked: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub compat_reason: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub can_upgrade: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub can_install: bool,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub active_runs: i64,
    pub mirror_mode: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub resolved_route: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub npm_registry: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub agent_dir: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub configured: bool,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub model_count: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub default_model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub enable_user_extensions: Option<bool>,
    #[serde(
        rename = "enableUserMCP",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub enable_user_mcp: Option<bool>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PiRuntimeSettingsInput {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub custom_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub mirror_mode: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub npm_registry: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub enable_user_extensions: Option<bool>,
    #[serde(
        rename = "enableUserMCP",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub enable_user_mcp: Option<bool>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PiUpgradeInput {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub version: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub install: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PiSetupPreset {
    pub id: String,
    pub name: String,
    pub provider_id: String,
    pub api: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub default_base_url: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub default_model: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub requires_base_url: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub description: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PiSetupRequest {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub preset: String,
    pub provider_id: String,
    pub api_key: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub base_url: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub api: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub models: Vec<String>,
    pub set_default: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ProjectUpdate {
    pub id: String,
    pub sequence: i64,
    pub thread_id: String,
    pub source_thread_id: String,
    pub run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub attempt_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub request_id: String,
    pub title: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub summary: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct InputConfiguration {
    pub provider_id: String,
    pub model: String,
    pub approval_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub interaction_mode: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Input {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub command_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub configuration: Option<InputConfiguration>,
    pub id: String,
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub sender_thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub attempt_id: String,
    pub kind: String,
    pub message: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub attachments: Vec<Attachment>,
    pub status: String,
    pub position: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

impl Input {
    pub fn is_queued(&self) -> bool {
        self.kind == "queue" && matches!(self.status.as_str(), "queued" | "pending")
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ToolCall {
    pub id: String,
    pub thread_id: String,
    pub run_id: String,
    pub attempt_id: String,
    pub name: String,
    #[serde(
        default,
        deserialize_with = "present_json",
        skip_serializing_if = "Option::is_none"
    )]
    pub arguments: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub output: String,
    pub status: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
    #[serde(default = "unknown_offset")]
    pub output_offset: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PendingRequest {
    pub id: String,
    pub provider_request_id: String,
    pub thread_id: String,
    pub run_id: String,
    pub attempt_id: String,
    pub method: String,
    pub title: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub options: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub prefill: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceFile {
    pub path: String,
    pub status: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkspaceChanges {
    pub path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub branch: String,
    pub is_git: bool,
    pub files: Option<Vec<WorkspaceFile>>,
    pub diff: String,
    pub truncated: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
pub enum SearchField {
    #[default]
    #[serde(rename = "")]
    Unspecified,
    #[serde(rename = "title")]
    Title,
    #[serde(rename = "branch")]
    Branch,
    #[serde(rename = "workspace")]
    Workspace,
    #[serde(rename = "input")]
    Input,
    #[serde(rename = "output")]
    Output,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SearchQuery {
    pub query: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub include_archived: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub limit: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SearchHit {
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    pub title: String,
    pub field: SearchField,
    pub snippet: String,
    pub score: i64,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SkillInfo {
    pub name: String,
    pub description: String,
    pub file_path: String,
    pub source: String,
    pub command: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
pub enum SteeringStatus {
    #[default]
    #[serde(rename = "")]
    Unspecified,
    #[serde(rename = "pending")]
    Pending,
    #[serde(rename = "sending")]
    Sending,
    #[serde(rename = "accepted")]
    Accepted,
    #[serde(rename = "rejected")]
    Rejected,
    #[serde(rename = "unknown")]
    Unknown,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SteeringMessage {
    pub id: String,
    pub command_id: String,
    pub thread_id: String,
    pub run_id: String,
    pub attempt_id: String,
    pub message: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub attachments: Vec<Attachment>,
    pub status: SteeringStatus,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub disposition: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
    #[serde(serialize_with = "go_time")]
    pub updated_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Attachment {
    pub id: String,
    pub name: String,
    pub mime: String,
    pub size: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct QueuedMessage {
    pub id: String,
    pub thread_id: String,
    pub message: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub attachments: Vec<Attachment>,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub run_id: String,
    #[serde(serialize_with = "go_time")]
    pub created_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ModelPluginSource {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credit_multiplier: Option<f64>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ModelInfo {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub plugin_source: Option<ModelPluginSource>,
    pub id: String,
    pub provider: String,
    pub name: String,
    pub reasoning: bool,
    pub input: Option<Vec<String>>,
    pub context_window: i64,
    pub max_tokens: i64,
}

impl ModelInfo {
    /// Canonical `provider/id` key matching Go v0.1.42's `ModelInfo.Key()`.
    pub fn key(&self) -> String {
        let provider = self.provider.trim();
        let id = self.id.trim();
        if provider.is_empty() || id.is_empty() {
            return id.to_owned();
        }
        if id
            .strip_prefix(provider)
            .is_some_and(|rest| rest.starts_with('/'))
        {
            id.to_owned()
        } else {
            format!("{provider}/{id}")
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ModelCatalog {
    pub models: Option<Vec<ModelInfo>>,
    pub default: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub thinking_levels: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub configured_default: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub warnings: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GitFile {
    pub path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub original_path: String,
    pub index: String,
    pub worktree: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GitCommit {
    pub id: String,
    pub short_id: String,
    pub subject: String,
    pub author: String,
    #[serde(serialize_with = "go_time")]
    pub author_time: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub refs: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub parents: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GitStash {
    pub r#ref: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub branch: String,
    pub message: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GitState {
    pub files: Option<Vec<GitFile>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub root: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub truncated: bool,
    pub branch: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub remote_branch: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub ahead: i64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub behind: i64,
    pub status: String,
    pub diff: String,
    pub branches: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub remote_branches: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub stashes: Vec<GitStash>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub commits: Vec<GitCommit>,
    pub conflicts: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PluginSettingOption {
    pub value: String,
    pub label: String,
}

fn deserialize_null_default<'de, D, T>(deserializer: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(deserializer)?.unwrap_or_default())
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PluginSettingSpec {
    pub key: String,
    pub label: String,
    #[serde(default, deserialize_with = "deserialize_null_default", skip_serializing_if = "String::is_empty")]
    pub help: String,
    #[serde(default, deserialize_with = "deserialize_null_default", skip_serializing_if = "String::is_empty")]
    pub r#type: String,
    #[serde(default, deserialize_with = "deserialize_null_default", skip_serializing_if = "String::is_empty")]
    pub default: String,
    #[serde(default, deserialize_with = "deserialize_null_default", skip_serializing_if = "String::is_empty")]
    pub value: String,
    #[serde(default, deserialize_with = "deserialize_null_default", skip_serializing_if = "Vec::is_empty")]
    pub options: Vec<PluginSettingOption>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PluginNavItem {
    pub title: String,
    pub page: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub icon: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub native_panel: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PluginInfo {
    pub id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub version: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub description: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub repo: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub builtin: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub disabled: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub has_update: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub latest_version: String,
    /// An official plugin this Core offers but has not installed yet.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub installable: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub needs_process_grant: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub granted: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub settings: Vec<PluginSettingSpec>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub navigation: Vec<PluginNavItem>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AppUpdateInfo {
    pub current_version: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub latest_version: String,
    pub available: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub delta_available: bool,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub delta_size: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub asset_name: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub asset_size: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub notes: String,
    #[serde(default)]
    #[serde(serialize_with = "go_time")]
    pub published_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub release_url: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub staged: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub staged_version: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub staged_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub target_path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub installer_path: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub used_delta: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub can_auto_restart: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkbenchRequest {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub paths: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
    pub action: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub workspace: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub data: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub branch: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub plugin_id: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub amend: bool,
    #[serde(
        default,
        deserialize_with = "present_json",
        skip_serializing_if = "Option::is_none"
    )]
    pub input: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct WorkbenchResponse {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mcp: Option<MCPCatalog>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub data: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub workspace: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub catalog: Option<ModelCatalog>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub attachment: Option<Attachment>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub git: Option<GitState>,
    // Older clients may still serialize this retired display-only field.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub ssh_hosts: Vec<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub plugins: Vec<PluginInfo>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub update: Option<AppUpdateInfo>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub pi_runtime: Option<PiRuntimeStatus>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub output: String,
}
