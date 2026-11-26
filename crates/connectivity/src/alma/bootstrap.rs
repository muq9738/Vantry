use super::chat::Scope;
use serde_json::{Value, json};
use vantry_client::ClientError;
use vantry_protocol::Thread;

// Alma selects a model by provider and ID. Vantry resolves execution against
// the shared thread, including threads that inherit their provider's default.
pub(super) fn model_id(model: &str) -> &str {
    if model.is_empty() { "default" } else { model }
}

pub(super) fn selection(model: &str) -> String {
    format!("vantry:{}", model_id(model))
}

pub(super) struct Choice {
    pub model: String,
    pub provider: String,
    name: String,
    vision: bool,
    reasoning: bool,
}

impl Choice {
    fn row(&self) -> Value {
        json!({"id":model_id(&self.model),"name":self.name,"providerId":"vantry",
            "providerName":"Vantry","providerType":"custom","providerIcon":null,
            "capabilities":{"streaming":true,"functionCalling":false,"vision":self.vision,"reasoning":self.reasoning}})
    }
}

impl Scope {
    pub(super) async fn choices(&self, thread: &Thread) -> Vec<Choice> {
        let writing = thread.provider_id == vantry_protocol::GEMINI_WRITING_PROVIDER;
        let mut choices = vec![Choice {
            model: thread.model.clone(),
            provider: thread.provider_id.clone(),
            name: if writing {
                "Gemini · Neon 高".into()
            } else if thread.model.is_empty() {
                "Vantry 默认模型".into()
            } else {
                thread.model.clone()
            },
            vision: false,
            reasoning: writing,
        }];
        if writing
            || thread
                .workspace
                .as_ref()
                .is_some_and(|w| !w.environment_id.is_empty())
        {
            return choices;
        }
        if !thread.model.is_empty() {
            choices.push(Choice {
                model: String::new(),
                provider: "pi".into(),
                name: "Vantry 默认模型".into(),
                vision: false,
                reasoning: false,
            });
        }
        // A failing local catalog must not take down the phone's entire bootstrap.
        if let Ok(models) = self
            .core
            .get_json::<Vec<vantry_protocol::ModelInfo>>("/v1/models")
            .await
        {
            for model in models {
                let key = model.key();
                if key.is_empty() {
                    continue;
                }
                let choice = Choice {
                    name: if model.name.is_empty() {
                        key.clone()
                    } else {
                        model.name
                    },
                    model: key.clone(),
                    provider: "pi".into(),
                    vision: model.input.iter().flatten().any(|s| s == "image"),
                    reasoning: model.reasoning,
                };
                if key == thread.model {
                    if thread.provider_id == "pi" {
                        choices[0] = choice;
                    }
                } else {
                    choices.push(choice);
                }
            }
        }
        choices
    }

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
                        json!({"data":self.choices(&thread).await.iter().map(Choice::row).collect::<Vec<_>>()})
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
