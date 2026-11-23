//! An in-memory Core: `vantry --mock` and tests.
//!
//! It keeps the protocol's guarantees the desktop relies on: strictly
//! increasing event sequences, replay after a cursor and then live delivery,
//! `resync_required` for a cursor it never issued, idempotent command ids.
//! A message gets a scripted reply that streams reasoning and text, runs one
//! tool behind an approval request, and finishes; `run.stop` cancels it.

use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::Duration,
};

use serde_json::json;
use tokio::sync::{Notify, oneshot};

use super::{
    BridgeError, BridgeFuture, CoreBridge, ErrorCode, EventSink,
    wire::{
        Command, Event, Health, ProviderRequest, Receipt, Run, RunStatus, Snapshot, Thread,
        ToolCall, event, kind,
    },
};

/// The in-memory Core. Cheap to clone; clones share the Core.
#[derive(Clone)]
pub struct MockBridge {
    core: Arc<Mutex<MockCore>>,
    /// Wakes scripted replies waiting on a request or a stop.
    wake: Arc<Notify>,
    /// Between streamed pieces of a scripted reply (zero in tests).
    step: Duration,
    /// Whether messages get a scripted reply.
    scripted: bool,
}

struct Subscriber {
    sink: EventSink,
    end: Option<oneshot::Sender<BridgeError>>,
}

#[derive(Default)]
struct MockCore {
    sequence: i64,
    next_id: u64,
    threads: Vec<Thread>,
    runs: Vec<Run>,
    tools: Vec<ToolCall>,
    requests: Vec<ProviderRequest>,
    events: Vec<Event>,
    receipts: HashMap<String, (Command, Receipt)>,
    subscribers: Vec<Subscriber>,
    commands: Vec<Command>,
}

impl MockCore {
    fn id(&mut self, prefix: &str) -> String {
        self.next_id += 1;
        format!("{prefix}-{}", self.next_id)
    }

    fn emit(&mut self, kind: &str, thread_id: &str, run_id: &str, data: serde_json::Value) -> i64 {
        self.sequence += 1;
        let event = Event {
            sequence: self.sequence,
            id: format!("evt-{}", self.sequence),
            kind: kind.to_owned(),
            thread_id: thread_id.to_owned(),
            run_id: run_id.to_owned(),
            attempt_id: if run_id.is_empty() {
                String::new()
            } else {
                format!("{run_id}-a1")
            },
            timestamp: String::new(),
            data,
        };
        self.events.push(event.clone());
        self.subscribers
            .retain(|subscriber| subscriber.sink.send(event.clone()).is_ok());
        self.sequence
    }

    fn run_mut(&mut self, run_id: &str) -> Option<&mut Run> {
        self.runs.iter_mut().find(|run| run.id == run_id)
    }

    fn set_run_status(&mut self, run_id: &str, status: RunStatus, error: &str) {
        let Some(run) = self.run_mut(run_id) else {
            return;
        };
        if run.status.terminal() {
            return;
        }
        run.status = status;
        run.error = error.to_owned();
        let run = run.clone();
        let kind = match status {
            RunStatus::Running => event::RUN_STARTED,
            RunStatus::Completed => event::RUN_COMPLETED,
            RunStatus::Cancelled => event::RUN_CANCELLED,
            RunStatus::Failed => event::RUN_FAILED,
            RunStatus::Interrupted => event::RUN_INTERRUPTED,
            RunStatus::Waiting => event::RUN_WAITING,
            RunStatus::Queued | RunStatus::Unknown => event::RUN_QUEUED,
        };
        let data = serde_json::to_value(&run).unwrap_or_default();
        self.emit(kind, &run.thread_id, &run.id, json!({ "run": data }));
    }

    fn output(&mut self, run_id: &str, delta: &str) {
        let Some(run) = self.run_mut(run_id) else {
            return;
        };
        if run.status.terminal() {
            return;
        }
        run.output.push_str(delta);
        let thread_id = run.thread_id.clone();
        self.emit(
            event::RUN_OUTPUT,
            &thread_id,
            run_id,
            json!({ "delta": delta }),
        );
    }

    fn reasoning(&mut self, run_id: &str, delta: &str) {
        let Some(run) = self.run_mut(run_id) else {
            return;
        };
        if run.status.terminal() {
            return;
        }
        run.reasoning.push_str(delta);
        let thread_id = run.thread_id.clone();
        self.emit(
            event::RUN_REASONING,
            &thread_id,
            run_id,
            json!({ "reasoningDelta": delta }),
        );
    }

    fn upsert_tool(&mut self, tool: ToolCall) {
        match self.tools.iter_mut().find(|t| t.id == tool.id) {
            Some(existing) => *existing = tool.clone(),
            None => self.tools.push(tool.clone()),
        }
        let data = serde_json::to_value(&tool).unwrap_or_default();
        self.emit(
            event::TOOL_UPDATED,
            &tool.thread_id,
            &tool.run_id,
            json!({ "tool": data }),
        );
    }

    fn upsert_request(&mut self, request: ProviderRequest) {
        match self.requests.iter_mut().find(|r| r.id == request.id) {
            Some(existing) => *existing = request.clone(),
            None => self.requests.push(request.clone()),
        }
        let data = serde_json::to_value(&request).unwrap_or_default();
        self.emit(
            event::REQUEST_UPDATED,
            &request.thread_id,
            &request.run_id,
            json!({ "request": data }),
        );
    }

    fn snapshot(&self) -> Snapshot {
        Snapshot {
            sequence: self.sequence,
            threads: self.threads.clone(),
            runs: self.runs.clone(),
            tools: self.tools.clone(),
            requests: self.requests.clone(),
        }
    }
}

/// What a scripted reply needs from a dispatched message.
struct Reply {
    run_id: String,
    message: String,
}

impl MockBridge {
    /// An empty Core whose messages get scripted replies, `step` apart.
    pub fn new(step: Duration) -> Self {
        MockBridge {
            core: Arc::new(Mutex::new(MockCore::default())),
            wake: Arc::new(Notify::new()),
            step,
            scripted: true,
        }
    }

    /// An empty Core that only records commands and answers receipts.
    pub fn quiet() -> Self {
        MockBridge {
            scripted: false,
            ..Self::new(Duration::ZERO)
        }
    }

    /// The `--mock` Core: one finished conversation to look at.
    pub fn demo() -> Self {
        let bridge = Self::new(Duration::from_millis(45));
        {
            let mut core = bridge.core.lock().unwrap();
            let thread = Thread {
                id: "thread-demo".into(),
                title: "欢迎使用 Vantry（演示数据）".into(),
                created_at: "2026-11-23T08:00:00Z".into(),
                provider_id: "pi".into(),
                ..Thread::default()
            };
            core.threads.push(thread.clone());
            core.emit(
                event::THREAD_CREATED,
                &thread.id,
                "",
                json!({ "thread": serde_json::to_value(&thread).unwrap_or_default() }),
            );
            core.runs.push(Run {
                id: "run-demo".into(),
                thread_id: thread.id.clone(),
                attempt_id: "run-demo-a1".into(),
                status: RunStatus::Completed,
                input: "这是什么？".into(),
                output: "这是 Vantry 的 NSH 原生桌面演示。发送一条消息，会看到流式回复、工具卡片和一次审批。".into(),
                reasoning: "用户想了解界面。".into(),
                created_at: "2026-11-23T08:00:01Z".into(),
                ..Run::default()
            });
        }
        bridge
    }

    /// Without scripted replies, with no delay: tests drive it by hand.
    pub fn commands(&self) -> Vec<Command> {
        self.core.lock().unwrap().commands.clone()
    }

    /// The current sequence.
    pub fn sequence(&self) -> i64 {
        self.core.lock().unwrap().sequence
    }

    /// Ends every live subscription with `error` (a dropped connection, a
    /// slow consumer's `resync_required`).
    pub fn drop_subscribers(&self, error: BridgeError) {
        let mut core = self.core.lock().unwrap();
        for mut subscriber in core.subscribers.drain(..) {
            if let Some(end) = subscriber.end.take() {
                let _ = end.send(error.clone());
            }
        }
    }

    /// Emits an event as the Core would (tests).
    pub fn emit(&self, kind: &str, thread_id: &str, run_id: &str, data: serde_json::Value) -> i64 {
        self.core
            .lock()
            .unwrap()
            .emit(kind, thread_id, run_id, data)
    }

    fn handle(&self, command: Command) -> Result<(Receipt, Option<Reply>), BridgeError> {
        let mut core = self.core.lock().unwrap();
        if command.version != super::wire::PROTOCOL_VERSION {
            return Err(BridgeError::new(
                ErrorCode::VersionMismatch,
                "protocol version 1 is required",
            ));
        }
        if command.command_id.is_empty() {
            return Err(BridgeError::new(
                ErrorCode::InvalidCommand,
                "commandId is required",
            ));
        }
        if let Some((original, receipt)) = core.receipts.get(&command.command_id) {
            if *original == command {
                return Ok((receipt.clone(), None));
            }
            return Err(BridgeError::new(
                ErrorCode::Conflict,
                "commandId was used with different content",
            ));
        }
        core.commands.push(command.clone());
        let mut receipt = Receipt {
            command_id: command.command_id.clone(),
            accepted: true,
            thread_id: command.thread_id.clone(),
            ..Receipt::default()
        };
        let mut reply = None;
        match command.kind.as_str() {
            kind::CREATE_THREAD => {
                let thread = Thread {
                    id: core.id("thread"),
                    title: if command.title.is_empty() {
                        "新会话".into()
                    } else {
                        command.title.clone()
                    },
                    provider_id: command.provider_id.clone(),
                    model: command.model.clone(),
                    interaction_mode: command.interaction_mode.clone(),
                    ..Thread::default()
                };
                core.threads.push(thread.clone());
                receipt.thread_id = thread.id.clone();
                receipt.sequence = core.emit(
                    event::THREAD_CREATED,
                    &thread.id,
                    "",
                    json!({ "thread": serde_json::to_value(&thread).unwrap_or_default() }),
                );
            }
            kind::SEND_MESSAGE | kind::QUEUE_MESSAGE => {
                if !core.threads.iter().any(|t| t.id == command.thread_id) {
                    return Err(BridgeError::new(ErrorCode::NotFound, "unknown thread"));
                }
                let run = Run {
                    id: core.id("run"),
                    thread_id: command.thread_id.clone(),
                    status: RunStatus::Queued,
                    input: command.message.clone(),
                    interaction_mode: command.interaction_mode.clone(),
                    ..Run::default()
                };
                core.runs.push(run.clone());
                receipt.run_id = run.id.clone();
                receipt.sequence = core.emit(
                    event::RUN_QUEUED,
                    &run.thread_id,
                    &run.id,
                    json!({ "run": serde_json::to_value(&run).unwrap_or_default() }),
                );
                reply = Some(Reply {
                    run_id: run.id,
                    message: command.message.clone(),
                });
            }
            kind::STEER_MESSAGE => {
                let message = command.message.clone();
                let run_id = command.run_id.clone();
                let delta = format!("\n\n（已收到调整：{message}）\n\n");
                core.output(&run_id, &delta);
                receipt.run_id = run_id;
                receipt.delivery = "delivered".into();
                receipt.sequence = core.sequence;
            }
            kind::STOP_RUN => {
                core.set_run_status(&command.run_id, RunStatus::Cancelled, "");
                let pending: Vec<ProviderRequest> = core
                    .requests
                    .iter()
                    .filter(|r| r.run_id == command.run_id && r.status == "pending")
                    .cloned()
                    .collect();
                for mut request in pending {
                    request.status = "expired".into();
                    core.upsert_request(request);
                }
                receipt.run_id = command.run_id.clone();
                receipt.sequence = core.sequence;
            }
            kind::RESPOND_REQUEST => {
                let Some(mut request) = core
                    .requests
                    .iter()
                    .find(|r| r.id == command.request_id)
                    .cloned()
                else {
                    return Err(BridgeError::new(ErrorCode::NotFound, "unknown request"));
                };
                if request.status != "pending" {
                    return Err(BridgeError::new(
                        ErrorCode::Conflict,
                        format!("request is {}", request.status),
                    ));
                }
                request.status = "responded".into();
                // The answer rides along where the scripted reply reads it.
                request.prefill = answer_of(&request, &command).to_owned();
                core.upsert_request(request.clone());
                receipt.request_id = request.id;
                receipt.run_id = request.run_id;
                receipt.sequence = core.sequence;
            }
            other => {
                return Err(BridgeError::new(
                    ErrorCode::InvalidCommand,
                    format!("unsupported command kind {other:?}"),
                ));
            }
        }
        core.receipts
            .insert(command.command_id.clone(), (command, receipt.clone()));
        Ok((receipt, reply))
    }

    async fn pause(&self) {
        if !self.step.is_zero() {
            tokio::time::sleep(self.step).await;
        } else {
            tokio::task::yield_now().await;
        }
    }

    fn live(&self, run_id: &str) -> bool {
        let core = self.core.lock().unwrap();
        core.runs
            .iter()
            .any(|run| run.id == run_id && !run.status.terminal())
    }

    /// Streams a reply to `reply`: reasoning, text, a tool behind an
    /// approval, more text, completion. Stops where the run was stopped.
    async fn script(self, reply: Reply) {
        let run_id = reply.run_id;
        self.pause().await;
        self.core
            .lock()
            .unwrap()
            .set_run_status(&run_id, RunStatus::Running, "");
        for piece in ["先看看", "需要做什么。"] {
            self.pause().await;
            self.core.lock().unwrap().reasoning(&run_id, piece);
        }
        let opening = format!("收到：「{}」。我先查看一下工作区。", reply.message.trim());
        for piece in split_pieces(&opening, 6) {
            if !self.live(&run_id) {
                return;
            }
            self.pause().await;
            self.core.lock().unwrap().output(&run_id, &piece);
        }
        let (tool_id, request_id) = {
            let mut core = self.core.lock().unwrap();
            let Some(run) = core.runs.iter().find(|r| r.id == run_id).cloned() else {
                return;
            };
            if run.status.terminal() {
                return;
            }
            let tool = ToolCall {
                id: core.id("tool"),
                thread_id: run.thread_id.clone(),
                run_id: run.id.clone(),
                attempt_id: format!("{}-a1", run.id),
                name: "bash".into(),
                arguments: Some(json!({ "command": "ls -la" })),
                status: "running".into(),
                output_offset: run.output.len() as i64,
                ..ToolCall::default()
            };
            core.upsert_tool(tool.clone());
            let request = ProviderRequest {
                id: core.id("request"),
                provider_request_id: "ui-1".into(),
                thread_id: run.thread_id.clone(),
                run_id: run.id.clone(),
                attempt_id: tool.attempt_id.clone(),
                method: "select".into(),
                title: "允许执行 bash？\nls -la".into(),
                options: vec!["允许本次".into(), "本会话允许 bash".into(), "拒绝".into()],
                status: "pending".into(),
                ..ProviderRequest::default()
            };
            core.upsert_request(request.clone());
            (tool.id, request.id)
        };
        // Wait for the answer (or a stop).
        let answer = loop {
            let notified = self.wake.notified();
            {
                let core = self.core.lock().unwrap();
                let request = core.requests.iter().find(|r| r.id == request_id);
                match request {
                    Some(r) if r.status == "responded" => break r.prefill.clone(),
                    Some(r) if r.status != "pending" => return,
                    None => return,
                    _ => {}
                }
            }
            notified.await;
        };
        let allowed = answer.starts_with("允许") || answer.starts_with("本会话允许");
        {
            let mut core = self.core.lock().unwrap();
            let Some(mut tool) = core.tools.iter().find(|t| t.id == tool_id).cloned() else {
                return;
            };
            if allowed {
                tool.status = "completed".into();
                tool.output = "total 8\ndrwxr-xr-x  4 vantry  staff  128 Cargo.toml\n".into();
            } else {
                tool.status = "failed".into();
                tool.output = "用户拒绝执行".into();
            }
            core.upsert_tool(tool);
        }
        let closing = if allowed {
            "命令已执行，工作区里有一个 Cargo 工程。"
        } else {
            "好的，不执行这条命令。"
        };
        for piece in split_pieces(closing, 6) {
            if !self.live(&run_id) {
                return;
            }
            self.pause().await;
            self.core.lock().unwrap().output(&run_id, &piece);
        }
        self.pause().await;
        self.core
            .lock()
            .unwrap()
            .set_run_status(&run_id, RunStatus::Completed, "");
    }
}

/// The answer a `request.respond` gives, as the Core records it.
fn answer_of<'a>(request: &ProviderRequest, command: &'a Command) -> &'a str {
    match request.method.as_str() {
        "confirm" if command.confirmed => "允许",
        "confirm" => "拒绝",
        _ if command.cancelled => "拒绝",
        _ => &command.value,
    }
}

/// `text` in pieces of `n` characters (whole characters, never split).
fn split_pieces(text: &str, n: usize) -> Vec<String> {
    let chars: Vec<char> = text.chars().collect();
    chars.chunks(n).map(|c| c.iter().collect()).collect()
}

impl CoreBridge for MockBridge {
    fn label(&self) -> String {
        "演示 Core（内存）".to_owned()
    }

    fn health(&self) -> BridgeFuture<Health> {
        Box::pin(async {
            Ok(Health {
                version: 1,
                state: "ready".into(),
            })
        })
    }

    fn snapshot(&self) -> BridgeFuture<Snapshot> {
        let core = self.core.clone();
        Box::pin(async move { Ok(core.lock().unwrap().snapshot()) })
    }

    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt> {
        let this = self.clone();
        Box::pin(async move {
            let (receipt, reply) = this.handle(command)?;
            this.wake.notify_waiters();
            if let (Some(reply), true) = (reply, this.scripted) {
                tokio::spawn(this.clone().script(reply));
            }
            Ok(receipt)
        })
    }

    fn subscribe(&self, after: i64, sink: EventSink) -> BridgeFuture<()> {
        let core = self.core.clone();
        Box::pin(async move {
            let ended = {
                let mut core = core.lock().unwrap();
                if after > core.sequence || after < 0 {
                    return Err(BridgeError::new(
                        ErrorCode::ResyncRequired,
                        format!("cursor {after} is not served"),
                    ));
                }
                for event in core.events.iter().filter(|e| e.sequence > after) {
                    if sink.send(event.clone()).is_err() {
                        return Ok(());
                    }
                }
                let (end, ended) = oneshot::channel();
                core.subscribers.push(Subscriber {
                    sink: sink.clone(),
                    end: Some(end),
                });
                ended
            };
            tokio::select! {
                () = sink.closed() => Ok(()),
                error = ended => match error {
                    Ok(error) => Err(error),
                    Err(_) => Ok(()),
                },
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn next(events: &mut tokio::sync::mpsc::UnboundedReceiver<Event>) -> Event {
        tokio::time::timeout(Duration::from_secs(5), events.recv())
            .await
            .expect("an event in time")
            .expect("the stream is open")
    }

    #[tokio::test]
    async fn replays_after_the_cursor_then_streams_live() {
        let bridge = MockBridge::quiet();
        bridge.emit("project.updated", "", "", json!({}));
        bridge.emit("project.updated", "", "", json!({}));
        let (sink, mut events) = tokio::sync::mpsc::unbounded_channel();
        let stream = tokio::spawn(bridge.subscribe(1, sink));
        assert_eq!(next(&mut events).await.sequence, 2);
        bridge.emit("project.updated", "", "", json!({}));
        assert_eq!(next(&mut events).await.sequence, 3);
        bridge.drop_subscribers(BridgeError::unavailable("bye"));
        let err = stream.await.unwrap().unwrap_err();
        assert_eq!(err.code, ErrorCode::Unavailable);
    }

    #[tokio::test]
    async fn a_cursor_from_the_future_needs_resync() {
        let bridge = MockBridge::quiet();
        let (sink, _events) = tokio::sync::mpsc::unbounded_channel();
        let err = bridge.subscribe(99, sink).await.unwrap_err();
        assert!(err.is_resync());
    }

    #[tokio::test]
    async fn a_command_id_is_idempotent_and_conflicts_on_other_content() {
        let bridge = MockBridge::quiet();
        let mut create = Command::new(kind::CREATE_THREAD, "thread-1");
        create.title = "A".into();
        let first = bridge.dispatch(create.clone()).await.unwrap();
        let again = bridge.dispatch(create.clone()).await.unwrap();
        assert_eq!(first, again);
        assert_eq!(bridge.commands().len(), 1);
        create.title = "B".into();
        let err = bridge.dispatch(create).await.unwrap_err();
        assert_eq!(err.code, ErrorCode::Conflict);
    }

    #[tokio::test]
    async fn the_scripted_reply_waits_for_approval_and_honours_a_denial() {
        let bridge = MockBridge::new(Duration::ZERO);
        let (sink, mut events) = tokio::sync::mpsc::unbounded_channel();
        let _stream = tokio::spawn(bridge.subscribe(0, sink));
        let created = bridge
            .dispatch(Command::new(kind::CREATE_THREAD, "c1"))
            .await
            .unwrap();
        let mut send = Command::new(kind::SEND_MESSAGE, "s1");
        send.thread_id = created.thread_id.clone();
        send.message = "看看目录".into();
        bridge.dispatch(send).await.unwrap();
        let request = loop {
            let event = next(&mut events).await;
            if event.kind == event::REQUEST_UPDATED {
                break serde_json::from_value::<ProviderRequest>(event.data["request"].clone())
                    .unwrap();
            }
        };
        assert_eq!(request.status, "pending");
        let mut respond = Command::new(kind::RESPOND_REQUEST, "r1");
        respond.thread_id = request.thread_id.clone();
        respond.run_id = request.run_id.clone();
        respond.request_id = request.id.clone();
        respond.value = "拒绝".into();
        bridge.dispatch(respond).await.unwrap();
        let mut tool_failed = false;
        loop {
            let event = next(&mut events).await;
            if event.kind == event::TOOL_UPDATED && event.data["tool"]["status"] == "failed" {
                tool_failed = true;
            }
            if event.kind == event::RUN_COMPLETED {
                break;
            }
        }
        assert!(tool_failed);
    }
}
