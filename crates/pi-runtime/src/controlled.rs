//! Offline, scripted provider using the same durable job, journal and control paths as Pi.
use crate::{
    JobRequest, disk,
    ui::WorkerUi,
    worker::{self, Active, Journal},
};
use anyhow::{Result, ensure};
use pi_coding_agent::extension_ui::DialogOptions;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::HashSet,
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ControlledStep {
    Text {
        delta: String,
    },
    Thinking {
        delta: String,
    },
    Tool {
        name: String,
        id: String,
        arguments: Value,
        result: Value,
    },
    Approval {
        title: String,
        message: String,
    },
    AwaitSteer,
    Delay {
        millis: u64,
    },
    Error {
        message: String,
    },
    Crash,
}
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub struct ControlledTestProvider {
    pub steps: Vec<ControlledStep>,
}
#[derive(thiserror::Error, Debug)]
#[error("controlled worker crash")]
pub(crate) struct Crash;
impl ControlledTestProvider {
    /// Atomically publish the script before the request becomes launchable.
    pub fn prepare(&self, job: &JobRequest) -> Result<PathBuf> {
        ensure!(
            job.request.provider_id == "controlled",
            "expected controlled provider"
        );
        let dir = job.dir()?;
        disk::directory(&dir)?;
        let lock = worker::lock_file(&dir.join("worker.lock"))?;
        lock.try_lock()?;
        let path = dir.join("controlled.json");
        if path.try_exists()? {
            ensure!(
                disk::read::<Self>(&path)? == *self,
                "conflicting controlled script"
            );
        } else {
            ensure!(
                !dir.join("request.json").try_exists()?,
                "attempt was already prepared"
            );
            disk::write_json(&path, self)?;
        }
        worker::prepare_locked(job, &dir)?;
        Ok(dir)
    }
    fn echo(message: &str) -> Self {
        let text =
            format!("这是 Vantry 的本机受控 Provider。\n\n已收到：{message}\n\n任务已完成。");
        Self {
            steps: text
                .chars()
                .collect::<Vec<_>>()
                .chunks(12)
                .map(|s| ControlledStep::Text {
                    delta: s.iter().collect(),
                })
                .collect(),
        }
    }
    pub(crate) async fn execute(
        dir: &Path,
        job: &JobRequest,
        journal: &Arc<Journal>,
        handled: &mut HashSet<PathBuf>,
    ) -> Result<()> {
        let script = dir.join("controlled.json");
        let provider = if script.try_exists()? {
            disk::read(&script)?
        } else {
            Self::echo(&job.request.message)
        };
        let ui = WorkerUi::new(journal.clone(), CancellationToken::new());
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        disk::write_json(
            &dir.join("prompt-intent.json"),
            &json!({"attemptId":job.request.attempt_id,"updatedAt":chrono::Utc::now()}),
        )?;
        let delta_event = |kind: &str, delta: String| {
            journal.record(json!({
                "type":"message_update", "assistantMessageEvent":{"type":kind,"delta":delta},
            }))
        };
        let work = async {
            for (index, step) in provider.steps.into_iter().enumerate() {
                match step {
                    ControlledStep::Text { delta } => delta_event("text_delta", delta),
                    ControlledStep::Thinking { delta } => delta_event("thinking_delta", delta),
                    ControlledStep::Tool {
                        name,
                        id,
                        arguments,
                        result,
                    } => {
                        journal.record(json!({"type":"tool_execution_start", "toolName":name, "toolCallId":id, "args":arguments}));
                        journal.record(json!({"type":"tool_execution_end", "toolName":name, "toolCallId":id, "result":result, "isError":false}));
                    }
                    ControlledStep::Approval { title, message } => {
                        let reply = ui
                            .ask(
                                format!("controlled-approval-{index}"),
                                json!({"method":"confirm","title":title,"message":message}),
                                DialogOptions::default(),
                                None,
                            )
                            .await;
                        ensure!(
                            reply.is_some_and(|r| r.confirmed),
                            "controlled approval denied"
                        );
                    }
                    ControlledStep::AwaitSteer => {
                        let message = rx
                            .recv()
                            .await
                            .ok_or_else(|| anyhow::anyhow!("steering closed"))?;
                        journal.record(json!({"type":"message_start", "message":{"role":"user", "content":message}}));
                    }
                    ControlledStep::Delay { millis } => {
                        tokio::time::sleep(Duration::from_millis(millis)).await
                    }
                    ControlledStep::Error { message } => anyhow::bail!("{message}"),
                    ControlledStep::Crash => return Err(Crash.into()),
                }
                tokio::task::yield_now().await;
            }
            Ok(())
        };
        let monitor = async {
            loop {
                if dir.join("stop.json").try_exists()? {
                    return Ok(());
                }
                worker::consume_controls(
                    dir,
                    handled,
                    Some((Active::Controlled(&tx), &ui)),
                    journal,
                    &job.data_dir,
                )
                .await?;
                tokio::select! {_=journal.failed.cancelled()=>anyhow::bail!("journal write failed"),_=tokio::time::sleep(Duration::from_millis(10))=>{}}
            }
        };
        tokio::select! {biased; r=monitor=>r,r=work=>r}
    }
}
