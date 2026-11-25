//! The model-only native dialog also supplies the static retrieval-tool descriptions to Pi.
use crate::{disk, ui::WorkerUi};
use anyhow::{Context, Result, ensure};
use async_trait::async_trait;
use pi_agent::{
    AgentTool, AgentToolResult, ToolExposure, ToolLoadout, ToolLoadoutChanges, ToolNamespace,
    ToolUpdateCallback,
};
use pi_coding_agent::extension_ui::{DialogOptions, ExtensionUi};
use serde_json::{Value, json};
use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
    sync::{Arc, LazyLock},
};
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

pub(crate) struct RequestSecret {
    ui: Arc<WorkerUi>,
    dir: PathBuf,
}

impl RequestSecret {
    pub(crate) fn new(ui: Arc<WorkerUi>, job: &Path) -> Self {
        Self {
            ui,
            dir: job.join("secrets"),
        }
    }

    fn write_secret(&self, value: &str) -> Result<PathBuf> {
        disk::directory(&self.dir)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&self.dir, fs::Permissions::from_mode(0o700))?;
        }
        let path = self.dir.join(format!("secret-{}.secret", Uuid::new_v4()));
        let mut file = disk::new_file(&path)?;
        let written = (|| -> std::io::Result<()> {
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                file.set_permissions(fs::Permissions::from_mode(0o600))?;
            }
            file.write_all(value.as_bytes())
        })();
        if let Err(e) = written {
            drop(file);
            let _ = fs::remove_file(&path);
            return Err(e.into());
        }
        Ok(path)
    }
}

#[async_trait]
impl AgentTool for RequestSecret {
    fn name(&self) -> &str {
        "vantry_request_secret"
    }
    fn label(&self) -> &str {
        "Vantry Request Secret"
    }
    fn description(&self) -> &str {
        "Collect a sensitive secret (API key, token, password, or sudo password) through Vantry's native input dialog and store it in a 0600 one-time file without exposing plaintext in chat, logs, or session history."
    }
    fn exposure(&self) -> ToolExposure {
        ToolExposure::ModelOnly
    }
    fn namespace(&self) -> Option<&ToolNamespace> {
        static NAMESPACE: LazyLock<ToolNamespace> = LazyLock::new(|| {
            ToolNamespace {
            name: "vantry".into(), description: Some("Delegated child tasks, peer threads, and remote machines".into()),
            instructions: Some("Orchestration tools for delegation, peer threads, and paired environments. Keep environmentId on remote requests. Async child tasks wake the parent automatically.".into()),
            ..Default::default()
        }
        });
        Some(&NAMESPACE)
    }
    fn prompt_snippet(&self) -> Option<&str> {
        Some("Request a secret via Vantry's native dialog and write it to a 0600 file")
    }
    fn prompt_guidelines(&self) -> Vec<String> {
        vec!["Use vantry_request_secret for user secrets (0600 file); never ask for plaintext secrets or print secret-file contents.".into()]
    }
    fn parameters(&self) -> Value {
        json!({"type":"object","properties":{
            "prompt":{"type":"string","description":"Prompt shown in Vantry's native dialog explaining what secret is requested"},
            "envVarName":{"type":"string","description":"Optional environment variable name hint (e.g. OPENAI_API_KEY)"}
        },"required":["prompt"]})
    }
    fn output_schema(&self) -> Option<Value> {
        Some(json!({"type":"object","additionalProperties":{}}))
    }
    fn prepare_loadout(&self, loadout: &ToolLoadout) -> Option<ToolLoadoutChanges> {
        // Preserve native tool descriptions without crossing the synchronous JS bridge on each refresh.
        let descriptions = loadout
            .declared
            .iter()
            .filter(|tool| matches!(tool.name(), "read" | "grep" | "find" | "ls"))
            .map(|tool| (tool.name().to_owned(), tool.description().to_owned()))
            .collect();
        Some(ToolLoadoutChanges {
            descriptions,
            ..Default::default()
        })
    }
    async fn execute(
        &self,
        _: &str,
        args: Value,
        cancel: CancellationToken,
        _: ToolUpdateCallback,
    ) -> Result<AgentToolResult> {
        let prompt = args["prompt"].as_str().unwrap_or_default().trim();
        ensure!(
            !prompt.is_empty(),
            "vantry_request_secret requires a non-empty prompt"
        );
        let env = args["envVarName"].as_str().unwrap_or_default().trim();
        if !env.is_empty() {
            let mut chars = env.bytes();
            ensure!(
                chars
                    .next()
                    .is_some_and(|c| c.is_ascii_alphabetic() || c == b'_')
                    && chars.all(|c| c.is_ascii_alphanumeric() || c == b'_'),
                "Invalid envVarName; expected [A-Za-z_][A-Za-z0-9_]*"
            );
        }
        let value = self
            .ui
            .input(
                prompt,
                Some(""),
                DialogOptions {
                    secret: true,
                    cancel: Some(cancel),
                    ..Default::default()
                },
            )
            .await
            .filter(|value| !value.is_empty())
            .context("Secret input was cancelled or empty")?;
        let path = self.write_secret(&value)?;
        let quoted = format!("'{}'", path.to_string_lossy().replace('\'', "'\\''"));
        let usage = if env.is_empty() {
            format!(
                "Consume from file without printing (e.g. sudo -S -p '' <command> < {quoted} or SECRET=\"$(cat {quoted})\" <command>), then remove with rm -f {quoted}. Never echo or log the secret."
            )
        } else {
            format!(
                "Pass via environment without printing: {env}=\"$(cat {quoted})\" <command>, then remove with rm -f {quoted}. Never echo or log the secret."
            )
        };
        let result = json!({"secretFilePath":path,"usageHint":usage});
        Ok(AgentToolResult::text(result.to_string())
            .with_details(result.clone())
            .with_structured_content(result))
    }
}
