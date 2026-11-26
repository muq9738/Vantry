//! PTYs owned by this Core. The reader drains independently of clients so a slow
//! or disconnected viewer cannot stall the shell or grow its transcript forever.
use anyhow::{Context, Result, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde_json::{Value, json};
use std::{
    collections::{HashMap, VecDeque},
    fs::File,
    io::{Read, Write},
    path::PathBuf,
    process::Command,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio::sync::watch;
use vantry_platform::pty;
use vantry_protocol::WorkbenchRequest;

const BUFFER: usize = 256 * 1024;
const IDLE: Duration = Duration::from_secs(300);
const RETAIN_EXIT: Duration = Duration::from_secs(30);

#[derive(Default)]
struct Output {
    bytes: VecDeque<u8>,
    end: u64,
    code: Option<i32>,
}

impl Output {
    fn append(&mut self, chunk: &[u8]) {
        self.bytes.extend(chunk);
        self.bytes.drain(..self.bytes.len().saturating_sub(BUFFER));
        self.end += chunk.len() as u64;
    }

    fn since(&self, offset: u64) -> Result<Option<Value>> {
        ensure!(offset <= self.end, "无效终端输出偏移");
        if offset == self.end && self.code.is_none() {
            return Ok(None);
        }
        let first = self.end - self.bytes.len() as u64;
        let start = first.max(offset);
        let bytes: Vec<_> = self
            .bytes
            .iter()
            .skip((start - first) as usize)
            .copied()
            .collect();
        Ok(Some(
            json!({"data": STANDARD.encode(bytes), "startOffset": start, "offset": self.end,
            "exited": self.code.is_some(), "exitCode": self.code}),
        ))
    }
}

struct Terminal {
    shell: Mutex<pty::Session>,
    writer: Mutex<File>,
    output: watch::Sender<Output>,
    touched: Mutex<Instant>,
}

#[derive(Default)]
pub struct Terminals {
    sessions: Arc<Mutex<HashMap<String, Arc<Terminal>>>>,
}

impl Terminals {
    fn open(&self, root: PathBuf, input: &Value) -> Result<Value> {
        let root = root.canonicalize()?;
        let cwd = root
            .join(input["cwd"].as_str().unwrap_or(""))
            .canonicalize()?;
        ensure!(
            cwd.is_dir() && cwd.starts_with(root),
            "终端目录不在工作区内"
        );
        let command = input["command"].as_str().unwrap_or("");
        let mut shell = if command.is_empty() {
            let mut shell =
                Command::new(std::env::var_os("SHELL").unwrap_or_else(|| "/bin/sh".into()));
            shell.arg("-i");
            shell
        } else {
            let mut shell = Command::new("/bin/sh");
            shell.args(["-c", command]);
            shell
        };
        shell.current_dir(&cwd).env("TERM", "xterm-256color");
        let mut sessions = self.sessions.lock().unwrap();
        ensure!(sessions.len() < 128, "终端会话数量已达上限");
        let shell = pty::Session::spawn(shell, 24, 100)?;
        let mut reader = shell.reader()?;
        let terminal = Arc::new(Terminal {
            writer: Mutex::new(shell.reader()?),
            shell: Mutex::new(shell),
            output: watch::channel(Output::default()).0,
            touched: Mutex::new(Instant::now()),
        });
        let id = uuid::Uuid::new_v4().to_string();
        sessions.insert(id.clone(), terminal.clone());
        drop(sessions);
        let output = terminal.output.clone();
        let pump = tokio::task::spawn_blocking(move || {
            let mut buf = [0; 8192];
            loop {
                match reader.read(&mut buf) {
                    Ok(0) => break,
                    Ok(n) => output.send_modify(|out| out.append(&buf[..n])),
                    Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
                    Err(_) => break, // Linux reports EIO when the last slave closes.
                }
            }
        });
        let registry = Arc::downgrade(&self.sessions);
        let key = id.clone();
        tokio::spawn(async move {
            let code = loop {
                let idle = terminal.touched.lock().unwrap().elapsed() >= IDLE;
                let status = tokio::task::block_in_place(|| {
                    let mut shell = terminal.shell.lock().unwrap();
                    if idle || registry.strong_count() == 0 {
                        shell.close().map(Some)
                    } else {
                        shell.try_wait()
                    }
                });
                match status {
                    Ok(Some(status)) => break status.code().unwrap_or(-1),
                    Err(_) => {
                        let _ = terminal.shell.lock().unwrap().close();
                        break -1;
                    }
                    Ok(None) => tokio::time::sleep(Duration::from_millis(200)).await,
                }
            };
            let _ = pump.await;
            terminal.output.send_modify(|out| out.code = Some(code));
            tokio::time::sleep(RETAIN_EXIT).await;
            if let Some(registry) = registry.upgrade() {
                registry.lock().unwrap().remove(&key);
            }
        });
        Ok(json!({"sessionId":id, "cwd":cwd}))
    }

    pub async fn handle(&self, r: &WorkbenchRequest, root: Option<PathBuf>) -> Result<Value> {
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        if r.action == "terminal.session.open" {
            return self.open(root.context("终端需要工作区目录")?, input);
        }
        let id = input["sessionId"].as_str().context("缺少终端会话 ID")?;
        let terminal = self
            .sessions
            .lock()
            .unwrap()
            .get(id)
            .cloned()
            .context("终端会话已关闭或过期")?;
        *terminal.touched.lock().unwrap() = Instant::now();
        match r.action.as_str() {
            "terminal.session.read" => {
                let offset = input["offset"].as_u64().context("缺少终端输出偏移")?;
                let mut reader = terminal.output.subscribe();
                let next = async {
                    loop {
                        if let Some(chunk) = reader.borrow_and_update().since(offset)? {
                            return Ok(chunk);
                        }
                        reader.changed().await?;
                    }
                };
                return tokio::time::timeout(Duration::from_secs(1), next).await.unwrap_or_else(|_| Ok(json!({
                    "data":"", "startOffset":offset, "offset":offset, "exited":false, "exitCode":null
                })));
            }
            "terminal.session.write" => {
                let data = input["data"].as_str().context("缺少终端输入")?;
                tokio::task::block_in_place(|| {
                    terminal.writer.lock().unwrap().write_all(data.as_bytes())
                })?;
            }
            "terminal.session.resize" => {
                let dimension = |key: &str| -> Result<u16> {
                    Ok(input[key].as_u64().context("缺少终端尺寸")?.try_into()?)
                };
                terminal
                    .shell
                    .lock()
                    .unwrap()
                    .resize(dimension("rows")?, dimension("cols")?)?;
            }
            "terminal.session.interrupt" => {
                tokio::task::block_in_place(|| terminal.writer.lock().unwrap().write_all(b"\x03"))?;
            }
            "terminal.session.close" => {
                tokio::task::block_in_place(|| terminal.shell.lock().unwrap().close())?;
            }
            action => bail!("unknown workbench action {action}"),
        }
        Ok(json!({}))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request(action: &str, input: Value) -> WorkbenchRequest {
        WorkbenchRequest {
            action: format!("terminal.session.{action}"),
            input: Some(input),
            ..Default::default()
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn terminal_round_trip_and_confined_cwd() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("workspace");
        std::fs::create_dir_all(root.join("child")).unwrap();
        std::os::unix::fs::symlink(dir.path(), root.join("escape")).unwrap();
        let sessions = Terminals::default();
        for path in ["..", "escape"] {
            assert!(
                sessions
                    .handle(&request("open", json!({"cwd": path})), Some(root.clone()))
                    .await
                    .is_err()
            );
        }
        let opened = sessions.handle(&request("open", json!({"cwd":"child", "command":"read line; printf 'result=%s@%s' \"$line\" \"${PWD##*/}\"; exit 7"})), Some(root)).await.unwrap();
        let id = &opened["sessionId"];
        sessions
            .handle(
                &request("resize", json!({"sessionId": id, "rows": 34, "cols": 80})),
                None,
            )
            .await
            .unwrap();
        sessions
            .handle(
                &request("write", json!({"sessionId": id, "data":"hello\n"})),
                None,
            )
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            let mut output = Vec::new();
            loop {
                let offset = output.len();
                let chunk = sessions
                    .handle(
                        &request("read", json!({"sessionId": id, "offset":offset})),
                        None,
                    )
                    .await
                    .unwrap();
                assert_eq!(chunk["startOffset"], offset);
                output.extend(STANDARD.decode(chunk["data"].as_str().unwrap()).unwrap());
                assert_eq!(chunk["offset"], output.len());
                if chunk["exited"] == true {
                    assert_eq!(chunk["exitCode"], 7);
                    break;
                }
            }
            assert!(
                String::from_utf8(output)
                    .unwrap()
                    .contains("result=hello@child")
            );
        })
        .await
        .unwrap();
    }

    #[test]
    fn terminal_offsets_survive_eviction() {
        let mut out = Output::default();
        out.append(&vec![b'x'; BUFFER]);
        out.append(b"end");
        assert_eq!(out.since(0).unwrap().unwrap()["startOffset"], 3);
        assert_eq!(
            out.since(BUFFER as u64).unwrap().unwrap()["data"],
            STANDARD.encode(b"end")
        );
    }
}
