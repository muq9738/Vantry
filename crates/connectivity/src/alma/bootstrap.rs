use super::chat::Scope;
use serde_json::{Value, json};
use vantry_client::ClientError;

// Alma selects a model by provider and ID. Vantry resolves execution against
// the shared thread, including threads that inherit their provider's default.
pub(super) fn model_id(model: &str) -> &str {
    if model.is_empty() { "default" } else { model }
}

pub(super) fn selection(model: &str) -> String {
    format!("vantry:{}", model_id(model))
}

impl Scope {
    /// Alma iOS loads these resources alongside the thread list. Construct the
    /// mobile view from this share, without exposing desktop-wide settings.
    pub(super) async fn bootstrap(&self, path: &str) -> Result<Option<Value>, ClientError> {
        let body = match path {
            "/api/settings" | "/api/models" | "/api/workspaces" => {
                let thread = self.core.thread_detail(&self.thread, 1).await?.thread;
                match path {
                    "/api/settings" => json!({
                        "general":{"theme":"system","language":"system"},
                        "chat":{"defaultModel":selection(&thread.model),"streamResponse":true,"enableMarkdown":true},
                        "ui":{}
                    }),
                    "/api/models" => {
                        let id = model_id(&thread.model);
                        let name = if thread.model.is_empty() {
                            "Vantry 默认模型"
                        } else {
                            id
                        };
                        json!({"data":[{"id":id,"name":name,"providerId":"vantry",
                            "providerName":"Vantry","providerType":"custom","providerIcon":null,
                            "capabilities":{"streaming":true,"functionCalling":false,"vision":false,"reasoning":false}}]})
                    }
                    _ => json!([{"id":self.pairing,"name":"Vantry","path":"","isDefault":true,
                        "isTemporary":false,"isSession":false,"isWorktree":false,"showInList":true,
                        "autoWorktree":false,"threadCount":1,"createdAt":thread.created_at,"updatedAt":thread.created_at}]),
                }
            }
            "/api/thread-labels" | "/api/handoff/user-questions" => json!([]),
            "/api/boards" => json!({"boards":[]}),
            // The phone can retain the previous computer's default chat. Indicate
            // this host's own entry point without accepting any other thread ID.
            "/api/personal" => json!({
                "threadId":self.thread,
                "settings":{"threadId":self.thread,"reportResults":false,"proactive":false,
                    "permissions":{"web":"ask","browser":"ask","files":"ask","send":"ask","spend":"ask"},
                    "dailyBudgetUsd":0,"routing":{"email":false,"telegram":false,"notifications":false,
                        "mutedApps":[],"autoHandle":false},"ackReaction":""},
                "today":{"spentUsd":0,"budgetUsd":0,"over":false},"running":0,"pendingApprovals":0,
                "notifications":{"status":"unavailable","apps":[]}
            }),
            "/api/personal/runs" => json!({"runs":[],"approvals":[],"herdrChats":[]}),
            "/api/herdr/info" => {
                json!({"online":false,"host":"Vantry","offline_reason":"Vantry chat host"})
            }
            "/api/herdr/mutes" => json!({"workspaces":[],"tabs":[]}),
            "/api/herdr/pins" => json!({"tabs":[]}),
            "/api/update/status" => json!({"name":"Vantry","version":env!("CARGO_PKG_VERSION"),
                "autoUpdateSupported":false,"autoUpdateStatus":{"status":"idle"}}),
            "/api/alma/vitals" => json!({"fatigue":0,"level":"awake","isNight":false,
                "mood":null,"energy":7,"valence":7,"updated":null}),
            "/api/health" => json!({"status":"ok","name":"Vantry","timestamp":chrono::Utc::now()}),
            _ => return Ok(None),
        };
        Ok(Some(body))
    }
}
