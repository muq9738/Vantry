use serde::Deserialize;
use serde_json::json;
use std::{path::PathBuf, process::Stdio, time::Duration};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use vantry_core::{
    Core, ProviderError,
    runner::{Emit, ProviderResult, StartMode},
};
use vantry_platform::hostfs;
use vantry_protocol::{GEMINI_WRITING_MODEL, ProviderEvent, ProviderRequest};

#[derive(Deserialize, PartialEq)]
#[serde(rename_all = "lowercase")]
enum Delivery {
    Synthetic,
    Drive,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Config {
    thread_id: String,
    node: PathBuf,
    adapter: PathBuf,
    delivery: Delivery,
}

fn fail(e: impl std::fmt::Display) -> ProviderError {
    ProviderError::Failed(e.to_string())
}

#[derive(Deserialize)]
#[serde(tag = "kind", rename_all = "camelCase", deny_unknown_fields)]
enum Record {
    Submitted,
    Observing,
    Delivering,
    Synthetic {
        text: String,
    },
    Archive {
        title: String,
        chars: usize,
        #[serde(rename = "fileId")]
        file_id: String,
    },
    Problem {
        code: String,
    },
}

pub async fn run(
    core: &Core,
    job: &std::path::Path,
    req: &ProviderRequest,
    mode: StartMode,
    emit: Emit,
) -> ProviderResult {
    let data = core.directory();
    let path = std::path::Path::new("gemini-writing.json");
    let cfg: Config = serde_json::from_slice(
        &hostfs::read_file(data, path, 32 * 1024).map_err(|_| fail("此 Core 尚未配置写作入口"))?,
    )
    .map_err(|_| fail("写作配置无效"))?;
    if req.thread_id != cfg.thread_id
        || req.model != GEMINI_WRITING_MODEL
        || !cfg.node.is_absolute()
        || !cfg.adapter.is_absolute()
        || req.attachments.as_ref().is_some_and(|a| !a.is_empty())
        || req
            .workspace
            .as_ref()
            .is_some_and(|w| !w.environment_id.is_empty())
    {
        return Err(fail("写作入口只接受绑定的本机线程、模型和文本要求"));
    }
    let run_id = &req.run_id;
    let (operation, message) = core.writing_input(req).map_err(fail)?;
    if req.after_sequence >= 4 {
        return Ok(());
    }
    hostfs::ensure_private_dir(job).map_err(fail)?;
    let stop = job.join("stop.json");
    if stop.exists() {
        return Err(ProviderError::ExecutionCancelled);
    }
    let mut child = tokio::process::Command::new(cfg.node)
        .arg(cfg.adapter)
        .arg(data.join(path))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .kill_on_drop(true)
        .spawn()
        .map_err(|_| fail("无法启动本机写作适配器"))?;
    let input = json!({"thread":req.thread_id,"run":run_id,"operation":operation,"message":message,
        "observeOnly":mode == StartMode::Attach,"stop":stop});
    child
        .stdin
        .take()
        .unwrap()
        .write_all(&serde_json::to_vec(&input).map_err(fail)?)
        .await
        .map_err(fail)?;
    let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();
    let mut last = 0;
    let mut check = tokio::time::interval(Duration::from_secs(1));
    loop {
        let line = tokio::select! {
            line = lines.next_line() => line.map_err(fail)?,
            _ = check.tick() => {
                if stop.exists() { return Err(ProviderError::ExecutionCancelled); }
                continue;
            }
        };
        let Some(line) = line else { break };
        let record: Record =
            serde_json::from_str(&line).map_err(|_| fail("写作适配器返回格式无效"))?;
        let (sequence, progress, delta) = match record {
            Record::Submitted => (1, "已提交", String::new()),
            Record::Observing => (2, "正在读取网页回复", String::new()),
            Record::Delivering => (
                3,
                if cfg.delivery == Delivery::Drive {
                    "正在归档"
                } else {
                    "正在交付"
                },
                String::new(),
            ),
            Record::Synthetic { text }
                if cfg.delivery == Delivery::Synthetic && !text.trim().is_empty() =>
            {
                (4, "", text)
            }
            Record::Archive {
                title,
                chars,
                file_id,
            } if cfg.delivery == Delivery::Drive
                && chars > 0
                && !file_id.is_empty()
                && file_id
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b)) =>
            {
                (
                    4,
                    "",
                    format!(
                        "{title}（{chars} 字）\n\n[打开 Drive 成稿](https://drive.google.com/open?id={file_id})"
                    ),
                )
            }
            Record::Problem { code } => {
                let reason = match code.as_str() {
                    "preflight" => {
                        return Err(fail("发送前核对未通过，请检查账号、档位、会话及归档配置。"));
                    }
                    "busy" => {
                        return Err(fail("这个账号已有未交付的写作。请从原消息取回那次运行。"));
                    }
                    "archive" => "归档待核对",
                    "incomplete" => "网页回复尚未完整",
                    "missing" => {
                        return Err(ProviderError::Interrupted(
                            "找不到原操作的网页提交记录，请核对原会话。".into(),
                        ));
                    }
                    _ => "提交状态待核对",
                };
                return Err(ProviderError::Interrupted(format!(
                    "{reason}。可发送「取回写作 {run_id}」重新观察原网页。"
                )));
            }
            _ => return Err(fail("写作适配器与线程的交付方式不匹配")),
        };
        if sequence != last + 1 {
            return Err(fail("写作事件顺序无效"));
        }
        last = sequence;
        if sequence > req.after_sequence {
            let mut event = ProviderEvent {
                sequence,
                delta,
                ..Default::default()
            };
            if !progress.is_empty() {
                event.r#type = "writing_progress".into();
                event.data = Some(json!({"status":progress}));
            } else {
                event.r#type = "writing_result".into();
            }
            emit(event).map_err(|_| ProviderError::Detached)?;
        }
    }
    if last != 4 || !child.wait().await.map_err(fail)?.success() {
        return Err(ProviderError::Interrupted(format!(
            "写作观察中断，可发送「取回写作 {run_id}」"
        )));
    }
    Ok(())
}
