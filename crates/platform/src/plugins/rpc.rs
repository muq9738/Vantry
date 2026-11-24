//! Newline-delimited JSON-RPC 2.0. Reader and writer have bounded queues and lifetime.
use crate::hostprocess::Process;
use anyhow::{Context, Result, ensure};
use serde_json::{Value, json};
use std::{
    io::{self, BufRead, BufReader, Read, Write},
    process::{Command, Stdio},
    sync::mpsc::{self, Receiver, SyncSender},
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};
#[derive(Debug, thiserror::Error)]
#[error("plugin RPC error {code}: {message}")]
pub struct RpcError {
    pub code: i64,
    pub message: String,
    pub data: Option<Value>,
}
pub struct Rpc {
    process: Process,
    inbox: Option<Receiver<io::Result<Value>>>,
    outbox: Option<SyncSender<Vec<u8>>>,
    threads: Vec<JoinHandle<()>>,
    sequence: u64,
    timeout: Duration,
}
impl Rpc {
    pub(super) fn start(command: &mut Command, timeout: Duration) -> Result<Self> {
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null());
        let mut process = Process::spawn(command)?;
        let mut input = process.child.stdin.take().unwrap();
        let mut output = BufReader::new(process.child.stdout.take().unwrap());
        let (tx, inbox) = mpsc::sync_channel(16);
        let (outbox, rx) = mpsc::sync_channel::<Vec<u8>>(1);
        let reader = thread::spawn(move || {
            loop {
                let mut line = Vec::new();
                let result = (|| {
                    let n = output
                        .by_ref()
                        .take((super::MAX_JSON + 1) as u64)
                        .read_until(b'\n', &mut line)?;
                    if n == 0 {
                        return Err(io::ErrorKind::UnexpectedEof.into());
                    }
                    if n > super::MAX_JSON || line.last() != Some(&b'\n') {
                        return Err(io::Error::new(
                            io::ErrorKind::InvalidData,
                            "invalid RPC frame",
                        ));
                    }
                    serde_json::from_slice(&line).map_err(io::Error::other)
                })();
                let failed = result.is_err();
                if tx.send(result).is_err() || failed {
                    break;
                }
            }
        });
        let writer = thread::spawn(move || {
            for msg in rx {
                if input.write_all(&msg).is_err() {
                    break;
                }
            }
        });
        Ok(Self {
            process,
            inbox: Some(inbox),
            outbox: Some(outbox),
            threads: vec![reader, writer],
            sequence: 0,
            timeout,
        })
    }
    pub fn pid(&self) -> u32 {
        self.process.id()
    }
    fn send(&self, frame: Value) -> Result<()> {
        let mut bytes = serde_json::to_vec(&frame)?;
        bytes.push(b'\n');
        ensure!(bytes.len() <= super::MAX_JSON, "RPC frame too large");
        self.outbox
            .as_ref()
            .context("RPC closed")?
            .try_send(bytes)
            .map_err(|_| anyhow::anyhow!("RPC writer unavailable"))
    }
    pub fn notify(&mut self, method: &str, params: Value) -> Result<()> {
        self.send(json!({"jsonrpc":"2.0","method":method,"params":params}))
    }
    pub fn request(
        &mut self,
        method: &str,
        params: Value,
        mut event: impl FnMut(&str, &Value),
    ) -> Result<Value> {
        self.sequence += 1;
        let id = self.sequence;
        self.send(json!({"jsonrpc":"2.0","id":id,"method":method,"params":params}))?;
        let end = Instant::now() + self.timeout;
        loop {
            let remaining = end
                .checked_duration_since(Instant::now())
                .context("RPC timed out")?;
            let msg = self
                .inbox
                .as_ref()
                .context("RPC closed")?
                .recv_timeout(remaining)??;
            ensure!(msg["jsonrpc"] == "2.0", "invalid JSON-RPC version");
            if let Some(method) = msg["method"].as_str() {
                ensure!(msg.get("id").is_none(), "unsolicited plugin request");
                event(method, &msg["params"]);
                continue;
            }
            ensure!(msg["id"].as_u64() == Some(id), "RPC response id mismatch");
            if let Some(err) = msg.get("error") {
                ensure!(
                    msg.get("result").is_none(),
                    "RPC response contains both result and error"
                );
                return Err(RpcError {
                    code: err["code"].as_i64().context("invalid RPC error code")?,
                    message: err["message"]
                        .as_str()
                        .context("invalid RPC error message")?
                        .into(),
                    data: err.get("data").cloned(),
                }
                .into());
            }
            return msg.get("result").cloned().context("missing RPC result");
        }
    }
    pub fn event(&self, timeout: Duration) -> Result<Option<(String, Value)>> {
        let msg = match self
            .inbox
            .as_ref()
            .context("RPC closed")?
            .recv_timeout(timeout)
        {
            Ok(result) => result?,
            Err(mpsc::RecvTimeoutError::Timeout) => return Ok(None),
            Err(e) => return Err(e.into()),
        };
        ensure!(
            msg["jsonrpc"] == "2.0" && msg.get("id").is_none(),
            "unexpected RPC frame outside request"
        );
        let method = msg["method"].as_str().context("missing event method")?;
        Ok(Some((method.into(), msg["params"].clone())))
    }
    pub fn close(&mut self) {
        self.outbox.take();
        self.inbox.take();
        let _ = self.process.stop(Duration::from_millis(150));
        for t in self.threads.drain(..) {
            let _ = t.join();
        }
    }
}
impl Drop for Rpc {
    fn drop(&mut self) {
        self.close();
    }
}
