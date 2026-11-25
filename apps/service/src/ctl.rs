use crate::{WorkspaceArgs, data_dir, parse_duration};
use anyhow::{Context, Result, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde_json::{Value, json};
use std::{
    io::{self, Read, Write},
    path::PathBuf,
    time::Duration,
};
use vantry_client::CoreClient;
use vantry_protocol::{Command, CommandKind::*, WorkbenchRequest};
pub fn print<T: serde::Serialize>(value: &T) -> Result<()> {
    let mut out = io::stdout().lock();
    serde_json::to_writer(&mut out, value)?;
    writeln!(out)?;
    out.flush()?;
    Ok(())
}
pub fn identity(id: String) -> String {
    if id.is_empty() {
        format!("cmd_{}", uuid::Uuid::new_v4().simple())
    } else {
        id
    }
}
#[derive(clap::Args)]
pub struct Ctl {
    action: String,
    /// Workbench action or JSON request
    workbench_action: Option<String>,
    #[arg(long)]
    data_dir: Option<PathBuf>,
    #[arg(long, default_value = "")]
    command_id: String,
    #[arg(long, default_value = "")]
    thread: String,
    #[arg(long, default_value = "")]
    run: String,
    #[arg(long, default_value = "")]
    title: String,
    #[arg(long, default_value = "")]
    message: String,
    #[arg(long, default_value_t = 0)]
    after: i64,
    #[arg(long, default_value = "")]
    provider: String,
    #[arg(long, default_value = "")]
    model: String,
    #[arg(long, default_value = "")]
    parent_run: String,
    #[arg(long, default_value = "")]
    parent_attempt: String,
    #[arg(long, default_value = "")]
    task: String,
    #[arg(long, default_value_t = 0)]
    generation: i64,
    #[arg(long, default_value = "")]
    delivery: String,
    #[arg(long, default_value = "")]
    resume_policy: String,
    #[arg(long, default_value = "")]
    message_id: String,
    #[arg(long)]
    file: Option<PathBuf>,
    #[arg(long, default_value = "")]
    plugin: String,
    #[arg(long, default_value = "")]
    approval: String,
    #[arg(long, default_value = "")]
    target: String,
    #[arg(long, default_value = "")]
    from: String,
    #[arg(long, default_value = "")]
    delivery_mode: String,
    #[arg(long, default_value = "")]
    reason: String,
    #[arg(long,value_parser=parse_duration)]
    duration: Option<Duration>,
    #[arg(long)]
    until: Option<String>,
    #[arg(long, default_value_t = 0)]
    limit: u32,
    #[arg(long,require_equals=true,num_args=0..=1,default_missing_value="true")]
    settled: Option<bool>,
    #[arg(long, default_value = "")]
    view: String,
    #[arg(long)]
    after_pos: Option<i64>,
    #[arg(long,value_parser=parse_duration,default_value="60s")]
    timeout: Duration,
    #[arg(long,require_equals=true,num_args=0..=1,default_missing_value="true",default_value="true",action=clap::ArgAction::Set)]
    force: bool,
    #[arg(long, default_value = "")]
    input: String,
    #[arg(long, default_value = "")]
    attempt: String,
    #[arg(long, default_value = "")]
    request: String,
    #[arg(long)]
    value: Option<String>,
    #[arg(long,require_equals=true,num_args=0..=1,default_missing_value="true")]
    confirm: Option<bool>,
    #[arg(long,require_equals=true,num_args=0..=1,default_missing_value="true")]
    cancel: Option<bool>,
    #[arg(long, default_value_t = 0)]
    position: i64,
    #[command(flatten)]
    workspace: WorkspaceArgs,
    #[arg(long)]
    json: Option<String>,
    #[arg(long = "action")]
    named_action: Option<String>,
    #[arg(long, default_value = "", allow_hyphen_values = true)]
    name: String,
    #[arg(long = "path")]
    paths: Vec<String>,
}
impl Ctl {
    pub async fn run(self) -> Result<()> {
        let client = CoreClient::from_data_dir(data_dir(self.data_dir.as_deref())?);
        ensure!(
            self.workbench_action.is_none() || self.action == "workbench",
            "unexpected positional argument"
        );
        ensure!(
            self.after >= 0 && !self.timeout.is_zero(),
            "invalid event cursor or timeout"
        );
        ensure!(
            matches!(self.provider.as_str(), "" | "controlled" | "pi"),
            "unknown provider"
        );
        ensure!(
            matches!(self.resume_policy.as_str(), "" | "manual" | "auto"),
            "unknown resume policy"
        );
        ensure!(
            matches!(self.delivery_mode.as_str(), "" | "auto" | "queue" | "steer"),
            "unknown delivery mode"
        );
        ensure!(
            matches!(
                self.approval.as_str(),
                "" | "ask" | "auto" | "auto-accept-edits"
            ),
            "unknown approval policy"
        );
        match self.action.as_str() {
            "stop" | "steer" => ensure!(
                !self.thread.is_empty() && !self.run.is_empty(),
                "--thread and --run required"
            ),
            "merge-back" => ensure!(!self.target.is_empty(), "--target required"),
            "rename" => ensure!(!self.title.is_empty(), "--title required"),
            "model" => ensure!(!self.model.is_empty(), "--model required"),
            _ => {}
        }
        match self.action.as_str() {
            "health" => print(&client.health().await?),
            "status" | "snapshot" => print(&client.snapshot().await?),
            "capabilities" => print(&client.capabilities().await?),
            "shutdown-if-idle" => print(&client.shutdown_if_idle().await?),
            "diff" => {
                ensure!(!self.thread.is_empty(), "diff requires --thread");
                let mut encoded = String::new();
                for b in self.thread.bytes() {
                    if b.is_ascii_alphanumeric() || b"-._~".contains(&b) {
                        encoded.push(char::from(b));
                    } else {
                        use std::fmt::Write;
                        write!(&mut encoded, "%{b:02X}")?;
                    }
                }
                print(
                    &client
                        .get_json::<Value>(&format!("/v1/workspace?thread={encoded}"))
                        .await?,
                )
            }
            "watch" => {
                let mut stream = client.subscribe(self.after).await?;
                while let Some(event) = stream.next_event().await? {
                    print(&event)?
                }
                Ok(())
            }
            "threads" | "thread-read" | "thread-send" | "thread-wait" => {
                let (route, body) = match self.action.as_str() {
                    "threads" => ("list", json!({"limit":self.limit,"settled":self.settled})),
                    "thread-read" => (
                        "read",
                        json!({"threadId":self.thread,"view":self.view,"afterPosition":self.after_pos,"limit":self.limit}),
                    ),
                    "thread-send" => (
                        "send",
                        json!({"clientRequestId":identity(self.command_id),"targetThreadId":self.thread,"callerThreadId":self.from,"message":self.message,"mode":self.delivery_mode}),
                    ),
                    _ => (
                        "wait",
                        json!({"threadId":self.thread,"runId":self.run,"timeoutMs":i64::try_from(self.timeout.as_millis())?}),
                    ),
                };
                print(
                    &client
                        .post_json::<_, Value>(&format!("/v1/orchestration/threads/{route}"), &body)
                        .await?,
                )
            }
            "workbench" | "models" | "plugins" | "plugin-run" | "git" | "merge" | "workspace"
            | "upload" | "terminal" => {
                let mut req = WorkbenchRequest {
                    thread_id: self.thread,
                    model: self.model,
                    plugin_id: self.plugin,
                    name: self.name,
                    message: self.message,
                    paths: self.paths,
                    branch: self.workspace.branch.unwrap_or_default(),
                    workspace: self
                        .workspace
                        .workspace
                        .map(|p| p.display().to_string())
                        .unwrap_or_default(),
                    ..Default::default()
                };
                req.action = match self.action.as_str() {
                    "workbench" => {
                        if let Some(raw) = self.json.or_else(|| {
                            self.workbench_action.clone().filter(|s| s.starts_with('{'))
                        }) {
                            req = serde_json::from_str(&raw)?;
                            req.action.clone()
                        } else if let Some(action) = self.named_action.or(self.workbench_action) {
                            if !self.input.is_empty() {
                                req.input = Some(serde_json::from_str(&self.input)?);
                            }
                            action
                        } else {
                            req = serde_json::from_reader(io::stdin().lock())?;
                            req.action.clone()
                        }
                    }
                    "plugins" => "plugins.list".into(),
                    "plugin-run" => {
                        req.input = Some(json!({"message":req.message,"threadId":req.thread_id}));
                        "plugins.run".into()
                    }
                    "git" => "git.status".into(),
                    "merge" => "git.merge".into(),
                    "terminal" => "terminal.open".into(),
                    "upload" => {
                        let path = self.file.context("upload requires --file")?;
                        req.name = path
                            .file_name()
                            .context("invalid file")?
                            .to_string_lossy()
                            .into();
                        let mut bytes = vec![];
                        std::fs::File::open(path)?
                            .take((8 << 20) + 1)
                            .read_to_end(&mut bytes)?;
                        ensure!(bytes.len() <= 8 << 20, "attachment exceeds 8 MiB");
                        req.data = STANDARD.encode(bytes);
                        "attachment.import".into()
                    }
                    s => s.into(),
                };
                print(&client.workbench(&req).await?)
            }
            _ => {
                let mut c = Command {
                    version: 1,
                    command_id: identity(self.command_id),
                    thread_id: self.thread,
                    provider_id: self.provider,
                    model: self.model,
                    workspace: self.workspace.spec()?,
                    parent_run_id: self.parent_run,
                    parent_attempt_id: self.parent_attempt,
                    task_id: self.task,
                    generation: self.generation,
                    delivery_message_id: self.delivery,
                    resume_policy: self.resume_policy,
                    approval_policy: self.approval,
                    ..Default::default()
                };
                c.kind = match self.action.as_str() {
                    "create" => {
                        c.title = self.title;
                        if c.thread_id.is_empty() {
                            c.thread_id = format!("thread_{}", c.command_id);
                        }
                        CreateThread
                    }
                    "delete" => {
                        c.force = self.force;
                        DeleteThread
                    }
                    "send" => {
                        c.message = self.message;
                        SendMessage
                    }
                    "queue" => {
                        c.message = self.message;
                        QueueMessage
                    }
                    "cancel-message" => {
                        c.message_id = self.message_id;
                        CancelMessage
                    }
                    "steer" => {
                        c.message = self.message;
                        c.run_id = self.run;
                        if self.attempt.is_empty() {
                            SteerMessage
                        } else {
                            c.parent_attempt_id = self.attempt;
                            SteerRun
                        }
                    }
                    "stop" => {
                        c.run_id = self.run;
                        StopRun
                    }
                    "fork" => {
                        c.title = self.title;
                        c.source_run_id = self.run;
                        ForkThread
                    }
                    "rewind" => {
                        c.source_run_id = self.run;
                        c.position = self.position;
                        RewindThread
                    }
                    "merge-back" => {
                        c.source_run_id = self.run;
                        c.target_thread_id = self.target;
                        MergeBackThread
                    }
                    "snooze" => {
                        c.snoozed_until = Some(match (self.until, self.duration) {
                            (Some(v), None) => {
                                chrono::DateTime::parse_from_rfc3339(&v)?.to_rfc3339()
                            }
                            (None, Some(d)) if !d.is_zero() => chrono::Utc::now()
                                .checked_add_signed(chrono::TimeDelta::from_std(d)?)
                                .context("invalid deadline")?
                                .to_rfc3339(),
                            _ => bail!("snooze requires --until or --duration"),
                        });
                        UpdateThread
                    }
                    "unsnooze" => {
                        c.snoozed_until = Some("0001-01-01T00:00:00Z".into());
                        UpdateThread
                    }
                    "cancel-task" => {
                        c.reason = self.reason;
                        CancelTask
                    }
                    "delegate" => {
                        c.message = self.message;
                        c.title = self.title;
                        DelegateTask
                    }
                    "followup" => {
                        c.message = self.message;
                        FollowUpTask
                    }
                    "ack" => AcknowledgeTask,
                    "dispose" => DisposeTask,
                    "rename" => {
                        c.title = self.title;
                        UpdateThread
                    }
                    "pin" | "unpin" => {
                        c.pinned = Some(self.action == "pin");
                        UpdateThread
                    }
                    "archive" | "restore" => {
                        c.archived = Some(self.action == "archive");
                        UpdateThread
                    }
                    "model" => SetThreadModel,
                    "cancel-input" => {
                        c.input_id = self.input;
                        CancelInput
                    }
                    "respond" => {
                        ensure!(
                            [
                                self.value.is_some(),
                                self.confirm.is_some(),
                                self.cancel.is_some()
                            ]
                            .into_iter()
                            .filter(|v| *v)
                            .count()
                                == 1,
                            "choose one dialog response"
                        );
                        c.request_id = self.request;
                        c.value = self.value.unwrap_or_default();
                        c.confirmed = self.confirm.unwrap_or_default();
                        c.cancelled = self.cancel.unwrap_or_default();
                        RespondRequest
                    }
                    _ => bail!("unknown ctl action {}", self.action),
                };
                print(&client.dispatch(&c).await?)
            }
        }
    }
}
