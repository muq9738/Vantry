use super::*;
use serde::Deserialize;
use serde_json::{Value, json};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Chunk {
    data: String,
    start_offset: u64,
    offset: u64,
    exited: bool,
    exit_code: Option<i32>,
}

impl CoreWorkbench {
    pub(crate) fn terminal_scope(mut self, thread: String, cwd: String) -> Self {
        self.terminal_thread = thread;
        self.terminal_cwd = cwd;
        self
    }

    async fn ask_terminal(
        &self,
        action: &str,
        input: Value,
    ) -> Result<WorkbenchResponse, BridgeError> {
        let mut req = self.ws_request(action);
        req.thread_id.clone_from(&self.terminal_thread);
        req.input = Some(input);
        Self::ask(self.socket.clone(), req).await.map_err(|err| {
            if actions::unknown_action(&err)
                || err.code == ErrorCode::NotReady
                || (err.code == ErrorCode::NotFound
                    && (err.message.contains("unknown workbench action")
                        || err.message.contains("unknown endpoint")))
            {
                BridgeError::unavailable("远端 Core 不支持内置终端，请升级该主机的 Vantry Core")
            } else {
                err
            }
        })
    }

    pub(super) fn terminal_control(
        &self,
        action: &'static str,
        session: String,
        mut input: Value,
    ) -> BridgeFuture<()> {
        let bench = self.clone();
        Box::pin(async move {
            let id = bench
                .terminal_ids
                .lock()
                .unwrap()
                .get(&session)
                .cloned()
                .ok_or_else(|| BridgeError::unavailable("终端会话尚未就绪或已退出"))?;
            input["sessionId"] = id.into();
            bench.ask_terminal(action, input).await?;
            Ok(())
        })
    }

    pub(super) fn remote_terminal(
        &self,
        session: String,
        command: String,
        sink: TerminalSink,
    ) -> BridgeFuture<i32> {
        let bench = self.clone();
        Box::pin(async move {
            let open = bench
                .ask_terminal(
                    names::TERMINAL_OPEN,
                    json!({"cwd":bench.terminal_cwd,"command":command}),
                )
                .await?;
            let open: Value = actions::read_output(&open)?;
            let id = open["sessionId"]
                .as_str()
                .filter(|s| !s.is_empty())
                .ok_or_else(|| BridgeError::protocol("Core 未返回终端会话 ID"))?;
            bench
                .terminal_ids
                .lock()
                .unwrap()
                .insert(session.clone(), id.to_owned());
            let _ = sink.send(String::new()); // Allow input even for a command that starts silently.
            let result = bench.poll_terminal(id, sink).await;
            bench.terminal_ids.lock().unwrap().remove(&session);
            if result.is_err() {
                let _ = bench
                    .ask_terminal(names::TERMINAL_CLOSE, json!({"sessionId":id}))
                    .await;
            }
            result
        })
    }

    async fn poll_terminal(&self, id: &str, sink: TerminalSink) -> Result<i32, BridgeError> {
        let mut offset = 0;
        let mut pending = Vec::new();
        loop {
            let read = tokio::select! {
                _ = sink.closed() => return Err(BridgeError::unavailable("终端视图已关闭")),
                res = self.ask_terminal(names::TERMINAL_READ, json!({"sessionId":id,"offset":offset})) => res?,
            };
            let chunk: Chunk = serde_json::from_str(&read.output)
                .map_err(|e| BridgeError::protocol(e.to_string()))?;
            let data = base64::engine::general_purpose::STANDARD
                .decode(chunk.data)
                .map_err(|e| BridgeError::protocol(e.to_string()))?;
            if chunk.start_offset < offset
                || chunk.offset.checked_sub(chunk.start_offset) != Some(data.len() as u64)
            {
                return Err(BridgeError::protocol("Core 返回了无效的终端偏移"));
            }
            if chunk.start_offset > offset {
                let _ = sink.send("\r\n[部分终端输出已超出缓冲区]\r\n".into());
                pending.clear();
            }
            offset = chunk.offset;
            pending.extend(data);
            // A character may straddle two reads. Only retain an incomplete final sequence.
            let tail = pending.len().saturating_sub(3);
            let incomplete = (tail..pending.len()).find(|&i| {
                std::str::from_utf8(&pending[i..])
                    .is_err_and(|e| e.valid_up_to() == 0 && e.error_len().is_none())
            });
            let end = if chunk.exited {
                pending.len()
            } else {
                incomplete.unwrap_or(pending.len())
            };
            if end > 0 {
                let _ = sink.send(String::from_utf8_lossy(&pending[..end]).into_owned());
                pending.drain(..end);
            }
            if chunk.exited {
                return chunk
                    .exit_code
                    .ok_or_else(|| BridgeError::protocol("Core 未返回终端退出码"));
            }
        }
    }
}
