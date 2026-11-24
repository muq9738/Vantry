//! Wire types corresponding to `pkg/provider` in Vantry v0.1.42.
use super::*;

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ControlResponse {
    pub id: String,
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub attachments: Vec<Attachment>,
    #[serde(
        rename = "requestId",
        default,
        skip_serializing_if = "String::is_empty"
    )]
    pub request_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub value: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub confirmed: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub cancelled: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ControlResult {
    pub id: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ProviderRequest {
    #[serde(rename = "ThreadID", default)]
    pub thread_id: String,
    #[serde(rename = "SenderThreadID", default)]
    pub sender_thread_id: String,
    #[serde(rename = "SenderTitle", default)]
    pub sender_title: String,
    #[serde(rename = "ParentTaskID", default)]
    pub parent_task_id: String,
    #[serde(rename = "RunID", default)]
    pub run_id: String,
    #[serde(rename = "AttemptID", default)]
    pub attempt_id: String,
    #[serde(rename = "Message", default)]
    pub message: String,
    #[serde(rename = "Attachments", default)]
    pub attachments: Option<Vec<Attachment>>,
    #[serde(rename = "ProviderID", default)]
    pub provider_id: String,
    #[serde(rename = "Model", default)]
    pub model: String,
    #[serde(rename = "Workspace", default)]
    pub workspace: Option<WorkspaceSpec>,
    #[serde(rename = "ApprovalPolicy", default)]
    pub approval_policy: String,
    #[serde(rename = "InteractionMode", default)]
    pub interaction_mode: String,
    #[serde(rename = "AfterSequence", default)]
    pub after_sequence: i64,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct ProviderEvent {
    #[serde(rename = "occurredAt", default)]
    #[serde(serialize_with = "go_time")]
    pub occurred_at: String,
    pub sequence: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub delta: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub r#type: String,
    #[serde(
        default,
        deserialize_with = "present_json",
        skip_serializing_if = "Option::is_none"
    )]
    pub data: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SteeringResult {
    pub status: SteeringStatus,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub disposition: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}
