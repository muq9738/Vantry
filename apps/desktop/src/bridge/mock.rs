//! An in-memory Core: `vantry --mock` and tests.
//!
//! It keeps the protocol's guarantees the desktop relies on: strictly
//! increasing event sequences, replay after a cursor and then live delivery,
//! `resync_required` for a cursor it never issued, idempotent command ids
//! (the same id with other content is a conflict). A message gets a scripted
//! reply that streams reasoning and Markdown text, runs one tool behind an
//! approval request, and finishes; `run.stop` cancels it. Queued inputs wait
//! for the run and start after it; they can be edited, reordered, promoted
//! to a steer or cancelled. Threads can be renamed, pinned, snoozed,
//! archived, deleted, forked, rewound and merged back.

use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::Duration,
};

use serde_json::json;
use tokio::sync::{Notify, oneshot};
use vantry_protocol::{
    Command, CommandKind, CoreEvent, DeliveryStatus, Health, Input, PendingRequest,
    PROTOCOL_VERSION, Receipt, Run, RunBlock, RunBlockKind, RunStatus, Snapshot, Task, Thread,
    ThreadDetail, ThreadSummary, ThreadSummaries, ToolCall,
};

use super::{BridgeError, BridgeFuture, CoreBridge, ErrorCode, EventSink, events};

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
    blocks: Vec<RunBlock>,
    tools: Vec<ToolCall>,
    requests: Vec<PendingRequest>,
    inputs: Vec<Input>,
    tasks: Vec<Task>,
    events: Vec<CoreEvent>,
    receipts: HashMap<String, (Command, Receipt)>,
    subscribers: Vec<Subscriber>,
    commands: Vec<Command>,
}

fn value<T: serde::Serialize>(item: &T) -> serde_json::Value {
    serde_json::to_value(item).unwrap_or_default()
}

fn not_found(what: &str) -> BridgeError {
    BridgeError::new(ErrorCode::NotFound, format!("unknown {what}"))
}

fn invalid(message: &str) -> BridgeError {
    BridgeError::new(ErrorCode::InvalidCommand, message)
}

impl MockCore {
    fn id(&mut self, prefix: &str) -> String {
        self.next_id += 1;
        format!("{prefix}-{}", self.next_id)
    }

    fn emit(&mut self, kind: &str, thread_id: &str, run_id: &str, data: serde_json::Value) -> i64 {
        self.sequence += 1;
        let event = CoreEvent {
            sequence: self.sequence,
            id: format!("evt-{}", self.sequence),
            r#type: kind.to_owned(),
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

    fn thread(&self, id: &str) -> Option<&Thread> {
        self.threads.iter().find(|t| t.id == id)
    }

    fn emit_thread(&mut self, kind: &str, thread: &Thread) -> i64 {
        self.emit(kind, &thread.id, "", json!({ "thread": value(thread) }))
    }

    fn update_thread(&mut self, id: &str, f: impl FnOnce(&mut Thread)) -> Option<i64> {
        let thread = self.threads.iter_mut().find(|t| t.id == id)?;
        f(thread);
        let thread = thread.clone();
        Some(self.emit_thread(events::THREAD_UPDATED, &thread))
    }

    fn run_mut(&mut self, run_id: &str) -> Option<&mut Run> {
        self.runs.iter_mut().find(|run| run.id == run_id)
    }

    fn active_run(&self, thread_id: &str) -> Option<&Run> {
        self.runs
            .iter()
            .rev()
            .find(|r| r.thread_id == thread_id && !r.status.terminal())
    }

    fn next_block_seq(&self, run_id: &str) -> i64 {
        self.blocks
            .iter()
            .filter(|b| b.run_id == run_id)
            .map(|b| b.seq)
            .max()
            .unwrap_or(0)
            + 1
    }

    fn close_streaming_blocks(&mut self, run_id: &str) {
        for block in &mut self.blocks {
            if block.run_id == run_id && block.status == "streaming" {
                block.status = "completed".into();
            }
        }
    }

    fn append_stream_block(
        &mut self,
        thread_id: &str,
        run_id: &str,
        kind: RunBlockKind,
        delta: &str,
    ) -> (String, i64) {
        if let Some(last) = self
            .blocks
            .iter_mut()
            .filter(|b| b.run_id == run_id)
            .max_by_key(|b| b.seq)
            && last.kind == kind
            && last.status == "streaming"
        {
            last.text.push_str(delta);
            return (last.id.clone(), last.seq);
        }
        self.close_streaming_blocks(run_id);
        let seq = self.next_block_seq(run_id);
        let prefix = match kind {
            RunBlockKind::Reasoning => "reasoning",
            _ => "text",
        };
        let id = format!("{prefix}:{run_id}:{seq}");
        self.blocks.push(RunBlock {
            id: id.clone(),
            thread_id: thread_id.to_owned(),
            run_id: run_id.to_owned(),
            attempt_id: format!("{run_id}-a1"),
            seq,
            kind,
            text: delta.to_owned(),
            status: "streaming".into(),
            ..RunBlock::default()
        });
        (id, seq)
    }

    fn push_run(&mut self, thread_id: &str, input: &str, mode: &str) -> Run {
        let run = Run {
            id: self.id("run"),
            thread_id: thread_id.to_owned(),
            status: RunStatus::Queued,
            input: input.to_owned(),
            interaction_mode: mode.to_owned(),
            created_at: format!("2026-11-23T09:{:02}:00Z", self.next_id % 60),
            ..Run::default()
        };
        self.runs.push(run.clone());
        let user_block = if !run.input.is_empty() || !run.attachments.is_empty() {
            let block = RunBlock {
                id: format!("user:{}", run.id),
                thread_id: run.thread_id.clone(),
                run_id: run.id.clone(),
                attempt_id: format!("{}-a1", run.id),
                seq: 1,
                kind: RunBlockKind::User,
                text: run.input.clone(),
                status: "completed".into(),
                attachments: run.attachments.clone(),
                created_at: run.created_at.clone(),
                updated_at: run.created_at.clone(),
                ..RunBlock::default()
            };
            self.blocks.push(block.clone());
            Some(block)
        } else {
            None
        };
        let data = match user_block {
            Some(block) => json!({ "run": value(&run), "block": value(&block) }),
            None => json!({ "run": value(&run) }),
        };
        self.emit(events::RUN_QUEUED, &run.thread_id, &run.id, data);
        run
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
        if status.terminal() || status == RunStatus::Waiting {
            self.close_streaming_blocks(run_id);
        }
        self.blocks
            .retain(|b| !(b.run_id == run_id && b.kind == RunBlockKind::Status));
        let status_block = match status {
            RunStatus::Waiting
            | RunStatus::Cancelled
            | RunStatus::Interrupted
            | RunStatus::Failed => {
                let seq = self.next_block_seq(run_id);
                let block = RunBlock {
                    id: format!("status:{run_id}"),
                    thread_id: run.thread_id.clone(),
                    run_id: run.id.clone(),
                    attempt_id: format!("{run_id}-a1"),
                    seq,
                    kind: RunBlockKind::Status,
                    status: serde_json::to_value(status)
                        .ok()
                        .and_then(|v| v.as_str().map(str::to_owned))
                        .unwrap_or_default(),
                    error: error.to_owned(),
                    ..RunBlock::default()
                };
                self.blocks.push(block.clone());
                Some(block)
            }
            _ => None,
        };
        let data = match status_block {
            Some(block) => json!({ "run": value(&run), "block": value(&block) }),
            None => json!({ "run": value(&run) }),
        };
        self.emit(events::of_run_status(status), &run.thread_id, &run.id, data);
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
        let (block_id, seq) = self.append_stream_block(&thread_id, run_id, RunBlockKind::Text, delta);
        self.emit(
            events::RUN_OUTPUT,
            &thread_id,
            run_id,
            json!({ "delta": delta, "blockId": block_id, "seq": seq }),
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
        let (block_id, seq) =
            self.append_stream_block(&thread_id, run_id, RunBlockKind::Reasoning, delta);
        self.emit(
            events::RUN_REASONING,
            &thread_id,
            run_id,
            json!({ "reasoningDelta": delta, "blockId": block_id, "seq": seq }),
        );
    }

    fn upsert_tool(&mut self, tool: ToolCall) {
        match self.tools.iter_mut().find(|t| t.id == tool.id) {
            Some(existing) => *existing = tool.clone(),
            None => self.tools.push(tool.clone()),
        }
        let block = if let Some(existing) = self
            .blocks
            .iter_mut()
            .find(|b| b.run_id == tool.run_id && b.kind == RunBlockKind::Tool && b.ref_id == tool.id)
        {
            existing.status = tool.status.clone();
            existing.clone()
        } else {
            self.close_streaming_blocks(&tool.run_id);
            let seq = self.next_block_seq(&tool.run_id);
            let block = RunBlock {
                id: format!("tool:{}", tool.id),
                thread_id: tool.thread_id.clone(),
                run_id: tool.run_id.clone(),
                attempt_id: tool.attempt_id.clone(),
                seq,
                kind: RunBlockKind::Tool,
                ref_id: tool.id.clone(),
                status: tool.status.clone(),
                ..RunBlock::default()
            };
            self.blocks.push(block.clone());
            block
        };
        self.emit(
            events::TOOL_UPDATED,
            &tool.thread_id,
            &tool.run_id,
            json!({ "tool": value(&tool), "block": value(&block) }),
        );
    }

    fn upsert_request(&mut self, request: PendingRequest) {
        match self.requests.iter_mut().find(|r| r.id == request.id) {
            Some(existing) => *existing = request.clone(),
            None => self.requests.push(request.clone()),
        }
        let block = if let Some(existing) = self.blocks.iter_mut().find(|b| {
            b.run_id == request.run_id
                && b.kind == RunBlockKind::Approval
                && b.ref_id == request.id
        }) {
            existing.status = request.status.clone();
            existing.error = request.error.clone();
            existing.clone()
        } else {
            self.close_streaming_blocks(&request.run_id);
            let seq = self.next_block_seq(&request.run_id);
            let block = RunBlock {
                id: format!("request:{}", request.id),
                thread_id: request.thread_id.clone(),
                run_id: request.run_id.clone(),
                attempt_id: request.attempt_id.clone(),
                seq,
                kind: RunBlockKind::Approval,
                ref_id: request.id.clone(),
                status: request.status.clone(),
                error: request.error.clone(),
                ..RunBlock::default()
            };
            self.blocks.push(block.clone());
            block
        };
        self.emit(
            events::REQUEST_UPDATED,
            &request.thread_id,
            &request.run_id,
            json!({ "request": value(&request), "block": value(&block) }),
        );
    }

    fn upsert_input(&mut self, input: Input) -> i64 {
        match self.inputs.iter_mut().find(|i| i.id == input.id) {
            Some(existing) => *existing = input.clone(),
            None => self.inputs.push(input.clone()),
        }
        let block = if input.kind == "steer"
            && !input.run_id.is_empty()
            && matches!(input.status.as_str(), "delivered" | "failed")
        {
            if let Some(existing) = self.blocks.iter_mut().find(|b| {
                b.run_id == input.run_id && b.kind == RunBlockKind::Steer && b.ref_id == input.id
            }) {
                existing.status = input.status.clone();
                existing.error = input.error.clone();
                Some(existing.clone())
            } else {
                self.close_streaming_blocks(&input.run_id);
                let seq = self.next_block_seq(&input.run_id);
                let block = RunBlock {
                    id: format!("steer:{}", input.id),
                    thread_id: input.thread_id.clone(),
                    run_id: input.run_id.clone(),
                    attempt_id: input.attempt_id.clone(),
                    seq,
                    kind: RunBlockKind::Steer,
                    ref_id: input.id.clone(),
                    text: input.message.clone(),
                    status: input.status.clone(),
                    error: input.error.clone(),
                    attachments: input.attachments.clone(),
                    ..RunBlock::default()
                };
                self.blocks.push(block.clone());
                Some(block)
            }
        } else {
            None
        };
        let data = match block {
            Some(block) => json!({ "input": value(&input), "block": value(&block) }),
            None => json!({ "input": value(&input) }),
        };
        self.emit(events::INPUT_UPDATED, &input.thread_id, &input.run_id, data)
    }

    /// The thread's queued inputs that have not started, in order.
    fn pending_queue(&self, thread_id: &str) -> Vec<Input> {
        let mut queue: Vec<Input> = self
            .inputs
            .iter()
            .filter(|i| i.thread_id == thread_id && i.is_queued())
            .cloned()
            .collect();
        queue.sort_by_key(|i| i.position);
        queue
    }

    /// Renumbers the thread's queue from 0 in `order`, announcing moves.
    fn renumber(&mut self, order: Vec<Input>) {
        for (position, mut input) in order.into_iter().enumerate() {
            if input.position != position as i64 {
                input.position = position as i64;
                self.upsert_input(input);
            }
        }
    }

    fn input(&self, command: &Command) -> Result<Input, BridgeError> {
        self.inputs
            .iter()
            .find(|i| i.id == command.input_id)
            .cloned()
            .ok_or_else(|| not_found("input"))
    }

    /// Starts the thread's next queued input as a run, when it is idle.
    fn start_next(&mut self, thread_id: &str) -> Option<Reply> {
        if self.active_run(thread_id).is_some() {
            return None;
        }
        let mut next = self.pending_queue(thread_id).into_iter().next()?;
        let run = self.push_run(thread_id, &next.message, "");
        next.status = "started".into();
        next.run_id = run.id.clone();
        self.upsert_input(next.clone());
        let rest = self.pending_queue(thread_id);
        self.renumber(rest);
        Some(Reply {
            run_id: run.id,
            message: next.message,
        })
    }

    fn snapshot(&self) -> Snapshot {
        Snapshot {
            sequence: self.sequence,
            threads: Some(self.threads.clone()),
            runs: Some(self.runs.clone()),
            tasks: Some(self.tasks.clone()),
            cohorts: Some(Vec::new()),
            tools: self.tools.clone(),
            requests: self.requests.clone(),
            inputs: self.inputs.clone(),
            blocks: self.blocks.clone(),
            ..Snapshot::default()
        }
    }

    fn thread_summaries(&self) -> ThreadSummaries {
        let threads = self
            .threads
            .iter()
            .map(|thread| {
                let runs = self.runs.iter().filter(|run| run.thread_id == thread.id);
                let latest = runs.clone().max_by(|a, b| {
                    a.created_at
                        .cmp(&b.created_at)
                        .then(a.id.cmp(&b.id))
                });
                let (latest_run_id, latest_status, updated_at) = latest.map_or_else(
                    || (String::new(), None, thread.created_at.clone()),
                    |run| {
                        (
                            run.id.clone(),
                            Some(run.status),
                            if run.updated_at.is_empty() {
                                run.created_at.clone()
                            } else {
                                run.updated_at.clone()
                            },
                        )
                    },
                );
                let run_count = runs.clone().count() as i64;
                let active_runs = runs
                    .clone()
                    .filter(|run| {
                        matches!(
                            run.status,
                            RunStatus::Queued | RunStatus::Running | RunStatus::Waiting
                        )
                    })
                    .count() as i64;
                let pending_inputs = self
                    .inputs
                    .iter()
                    .filter(|input| {
                        input.thread_id == thread.id
                            && matches!(input.status.as_str(), "queued" | "pending" | "")
                    })
                    .count() as i64;
                let pending_requests = self
                    .requests
                    .iter()
                    .filter(|request| {
                        request.thread_id == thread.id
                            && matches!(request.status.as_str(), "pending" | "responding" | "")
                    })
                    .count() as i64;
                ThreadSummary {
                    unread_sequence: thread.unread_sequence,
                    project_path: thread.project_path.clone(),
                    id: thread.id.clone(),
                    title: thread.title.clone(),
                    provider_id: thread.provider_id.clone(),
                    model: thread.model.clone(),
                    workspace: thread.workspace.clone(),
                    parent_thread_id: thread.parent_thread_id.clone(),
                    parent_task_id: thread.parent_task_id.clone(),
                    created_by_thread_id: thread.created_by_thread_id.clone(),
                    pinned: thread.pinned,
                    archived: thread.archived,
                    snoozed_until: thread.snoozed_until.clone(),
                    approval_policy: thread.approval_policy.clone(),
                    created_at: thread.created_at.clone(),
                    updated_at,
                    run_count,
                    active_runs,
                    pending_inputs,
                    pending_requests,
                    latest_run_id,
                    latest_status,
                }
            })
            .collect();
        ThreadSummaries {
            sequence: self.sequence,
            threads: Some(threads),
        }
    }

    fn thread_detail(&self, thread_id: &str, limit_runs: i64) -> Result<ThreadDetail, BridgeError> {
        let thread = self
            .thread(thread_id)
            .cloned()
            .ok_or_else(|| not_found("thread"))?;
        let all_runs: Vec<Run> = self
            .runs
            .iter()
            .filter(|run| run.thread_id == thread_id)
            .cloned()
            .collect();
        let run_count = all_runs.len() as i64;
        let truncated_runs = limit_runs > 0 && run_count > limit_runs;
        let runs = if truncated_runs {
            all_runs
                .iter()
                .skip(all_runs.len() - limit_runs as usize)
                .cloned()
                .collect()
        } else {
            all_runs
        };
        let blocks = self
            .blocks
            .iter()
            .filter(|block| {
                block.thread_id == thread_id
                    && (!truncated_runs || runs.iter().any(|r| r.id == block.run_id))
            })
            .cloned()
            .collect();
        Ok(ThreadDetail {
            sequence: self.sequence,
            thread,
            runs: Some(runs),
            tasks: Some(
                self.tasks
                    .iter()
                    .filter(|task| {
                        task.parent_thread_id == thread_id || task.child_thread_id == thread_id
                    })
                    .cloned()
                    .collect(),
            ),
            inputs: Some(
                self.inputs
                    .iter()
                    .filter(|input| input.thread_id == thread_id)
                    .cloned()
                    .collect(),
            ),
            tools: Some(
                self.tools
                    .iter()
                    .filter(|tool| tool.thread_id == thread_id)
                    .cloned()
                    .collect(),
            ),
            requests: Some(
                self.requests
                    .iter()
                    .filter(|request| request.thread_id == thread_id)
                    .cloned()
                    .collect(),
            ),
            blocks,
            run_count,
            truncated_runs,
            ..ThreadDetail::default()
        })
    }
}

/// What a scripted reply needs from a dispatched message.
struct Reply {
    run_id: String,
    message: String,
}

/// A demo reply with Vantry's visual blocks (charts, artifacts).
const VISUAL_MARKDOWN: &str = r#"本周构建与延迟概览：

```chart bar unit=ms title="接口延迟对比" ref=60 reflabel="SLA"
           | p50 | p95
* Redis    | 12  | 40
Memcached  | 9   | 31
Postgres   | 28  | 95
Mongo      | 22  | 71
```

```chart line unit=% title="CPU 使用率"
           | 桌面 | 服务
2026-11-18 | 32 | 18
2026-11-19 | 41 | 22
2026-11-20 | 38 | 35
2026-11-21 | 55 | 42
2026-11-22 | 47 | 39
2026-11-23 | 62 | 44
2026-11-24 | 58 | 51
```

```chart
{"type":"combo","title":"发布量与失败率","unit":"次","unitRight":"%","labels":["1月","2月","3月","4月","5月","6月"],"series":[{"name":"发布","data":[12,18,15,22,26,31]},{"name":"失败率","kind":"line","axis":"right","data":[8.5,6.1,7.4,4.2,3.6,2.9]}]}
```

```chart bar title="月度盈亏" unit=k
     | 盈亏
1月  | 12
2月  | -8
3月  | 5
4月  | -14
5月  | 20
6月  | 9
```

```chart donut title="令牌占比" unit=k
          | 用量
Claude    | 420
GPT       | 260
Gemini    | 130
DeepSeek  | 90
Kimi      | 60
Qwen      | 40
GLM       | 30
Grok      | 20
Mistral   | 10
Llama     | 8
```

```chart
{"type":"stats","title":"运行指标","hero":"98.6%","heroLabel":"成功率","delta":"+1.2%","items":[{"label":"平均耗时","value":"3分12秒","delta":"-8%"},{"label":"今日运行","value":"128","delta":"+14"},{"label":"失败","value":"2","delta":"-3"}]}
```

```html title="交互式计数器"
<div style="padding:16px;font-family:system-ui">
  <h3 style="margin:0 0 8px">离线组件</h3>
  <button onclick="this.nextElementSibling.textContent=+this.nextElementSibling.textContent+1" style="padding:6px 12px;border-radius:8px;border:1px solid var(--v-line);background:var(--v-surface);color:var(--v-ink)">+1</button>
  <span class="tnum" style="margin-left:8px">0</span>
</div>
```
"#;

/// The demo reply's Markdown, streamed after the approval.
const DEMO_MARKDOWN: &str = "## 检查结果\n\n工作区是一个 **Cargo** 工程，入口在 `src/main.rs`：\n\n1. 依赖已锁定\n2. 测试可直接运行\n\n```rust\nfn main() {\n    println!(\"hello, vantry\");\n}\n```\n\n建议的改动：\n\n```diff\n--- a/src/main.rs\n+++ b/src/main.rs\n@@ -1,3 +1,3 @@\n fn main() {\n-    println!(\"hello\");\n+    println!(\"hello, vantry\");\n }\n```\n\n> 提示：改动尚未写入，确认后再执行。\n";

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

    /// The `--mock` Core: a finished conversation with delegated subtasks, a
    /// pinned, a snoozed and an archived thread.
    pub fn demo() -> Self {
        let bridge = Self::new(Duration::from_millis(45));
        {
            let mut core = bridge.core.lock().unwrap();
            let thread = |id: &str, title: &str, at: &str| Thread {
                id: id.into(),
                title: title.into(),
                created_at: at.into(),
                provider_id: "pi".into(),
                model: "anthropic/claude-sonnet-5-5".into(),
                approval_policy: "ask".into(),
                ..Thread::default()
            };
            let welcome = Thread {
                pinned: true,
                ..thread(
                    "thread-demo",
                    "欢迎使用 Vantry（演示数据）",
                    "2026-11-23T08:00:00Z",
                )
            };
            let child_ok = Thread {
                parent_thread_id: welcome.id.clone(),
                parent_task_id: "task-1".into(),
                ..thread("thread-child-1", "子任务：检查依赖", "2026-11-23T08:01:00Z")
            };
            let child_failed = Thread {
                parent_thread_id: welcome.id.clone(),
                parent_task_id: "task-2".into(),
                ..thread("thread-child-2", "子任务：运行测试", "2026-11-23T08:02:00Z")
            };
            let snoozed = Thread {
                snoozed_until: Some("2099-01-01T00:00:00Z".into()),
                ..thread("thread-snoozed", "稍后再看：发布清单", "2026-11-22T10:00:00Z")
            };
            let archived = Thread {
                archived: true,
                ..thread("thread-archived", "已归档：旧方案讨论", "2026-11-20T10:00:00Z")
            };
            let visuals = thread("thread-visuals", "可视化回复（演示）", "2026-11-23T09:00:00Z");
            for t in [&welcome, &child_ok, &child_failed, &snoozed, &archived, &visuals] {
                core.threads.push(t.clone());
                core.emit_thread(events::THREAD_CREATED, t);
            }
            core.runs.push(Run {
                id: "run-demo".into(),
                thread_id: welcome.id.clone(),
                attempt_id: "run-demo-a1".into(),
                status: RunStatus::Completed,
                input: "这是什么？".into(),
                output: format!(
                    "这是 Vantry 的 NSH 原生桌面演示。发送一条消息，会看到流式回复、工具卡片和一次审批。\n\n{DEMO_MARKDOWN}"
                ),
                reasoning: "用户想了解界面。".into(),
                created_at: "2026-11-23T08:00:01Z".into(),
                ..Run::default()
            });
            core.runs.push(Run {
                id: "run-visuals".into(),
                thread_id: visuals.id.clone(),
                attempt_id: "run-visuals-a1".into(),
                status: RunStatus::Completed,
                input: "用图表总结一下本周的数据".into(),
                output: VISUAL_MARKDOWN.into(),
                created_at: "2026-11-23T09:00:01Z".into(),
                ..Run::default()
            });
            core.tools.push(ToolCall {
                id: "tool-visuals-todo".into(),
                thread_id: visuals.id.clone(),
                run_id: "run-visuals".into(),
                attempt_id: "run-visuals-a1".into(),
                name: "vantry_todo_write".into(),
                arguments: Some(json!({"title": "周报可视化", "todos": [
                    {"content": "收集本周构建与延迟数据", "status": "completed"},
                    {"content": "生成延迟与 CPU 图表", "status": "completed"},
                    {"content": "汇总发布量与失败率", "activeForm": "正在汇总发布量与失败率", "status": "in_progress"},
                    {"content": "整理令牌占比与运行指标", "status": "pending"},
                    {"content": "输出交互式组件", "status": "pending"}
                ]})),
                output: "Task list updated: 2/5 completed.".into(),
                status: "completed".into(),
                output_offset: 0,
                ..ToolCall::default()
            });
            // Steps before the reply: one quiet, collapsible tool line.
            for (n, (name, args, output)) in [
                ("bash", json!({ "command": "git status --short" }), "M apps/desktop/src/views/timeline.rs"),
                ("read", json!({ "path": "docs/desktop-ui.md" }), "# Desktop UI"),
                ("grep", json!({ "pattern": "TimelineItem" }), "apps/desktop/src/state/timeline.rs"),
                ("bash", json!({ "command": "cargo test -p vantry-desktop" }), "test result: ok"),
            ]
            .into_iter()
            .enumerate()
            {
                core.tools.push(ToolCall {
                    id: format!("tool-demo-{n}"),
                    thread_id: welcome.id.clone(),
                    run_id: "run-demo".into(),
                    attempt_id: "run-demo-a1".into(),
                    name: name.into(),
                    arguments: Some(args),
                    output: output.into(),
                    status: "completed".into(),
                    output_offset: 0,
                    ..ToolCall::default()
                });
            }
            for (child, status, output) in [
                (&child_ok, RunStatus::Completed, "依赖均为最新。"),
                (&child_failed, RunStatus::Failed, ""),
            ] {
                core.runs.push(Run {
                    id: format!("run-{}", child.id),
                    thread_id: child.id.clone(),
                    status,
                    input: child.title.clone(),
                    output: output.into(),
                    error: if status == RunStatus::Failed {
                        "2 个测试失败".into()
                    } else {
                        String::new()
                    },
                    created_at: child.created_at.clone(),
                    ..Run::default()
                });
            }
            for (id, child, status) in [
                ("task-1", &child_ok, RunStatus::Completed),
                ("task-2", &child_failed, RunStatus::Failed),
            ] {
                core.tasks.push(Task {
                    id: id.into(),
                    parent_thread_id: welcome.id.clone(),
                    parent_run_id: "run-demo".into(),
                    child_thread_id: child.id.clone(),
                    child_run_id: format!("run-{}", child.id),
                    title: child.title.clone(),
                    generation: 1,
                    status,
                    delivery_status: DeliveryStatus::Delivered,
                    ..Task::default()
                });
            }
        }
        bridge
    }

    /// Every command it accepted, in order (tests).
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

    /// Finishes run `run_id` as the Core would, starting the next queued
    /// input (tests drive quiet Cores with it).
    pub fn finish_run(&self, run_id: &str, status: RunStatus) {
        let mut core = self.core.lock().unwrap();
        core.set_run_status(run_id, status, "");
        if let Some(thread_id) = core.runs.iter().find(|r| r.id == run_id).map(|r| r.thread_id.clone()) {
            core.start_next(&thread_id);
        }
    }

    fn handle(&self, command: Command) -> Result<(Receipt, Vec<Reply>), BridgeError> {
        let mut core = self.core.lock().unwrap();
        if command.version != i64::from(PROTOCOL_VERSION) {
            return Err(BridgeError::new(
                ErrorCode::VersionMismatch,
                "protocol version 1 is required",
            ));
        }
        if command.command_id.is_empty() {
            return Err(invalid("commandId is required"));
        }
        if let Some((original, receipt)) = core.receipts.get(&command.command_id) {
            if *original == command {
                return Ok((receipt.clone(), Vec::new()));
            }
            return Err(BridgeError::new(
                ErrorCode::Conflict,
                "commandId was used with different content",
            ));
        }
        let needs_thread = !matches!(command.kind, CommandKind::CreateThread);
        if needs_thread && core.thread(&command.thread_id).is_none() {
            return Err(not_found("thread"));
        }
        let mut receipt = Receipt {
            command_id: command.command_id.clone(),
            accepted: true,
            thread_id: command.thread_id.clone(),
            ..Receipt::default()
        };
        let mut replies = Vec::new();
        match command.kind {
            CommandKind::CreateThread => {
                let thread = Thread {
                    id: core.id("thread"),
                    title: if command.title.is_empty() {
                        "新会话".into()
                    } else {
                        command.title.clone()
                    },
                    created_at: format!("2026-11-23T10:{:02}:00Z", core.next_id % 60),
                    provider_id: command.provider_id.clone(),
                    model: command.model.clone(),
                    workspace: command.workspace.clone(),
                    interaction_mode: command.interaction_mode.clone(),
                    approval_policy: command.approval_policy.clone(),
                    ..Thread::default()
                };
                core.threads.push(thread.clone());
                receipt.thread_id = thread.id.clone();
                receipt.sequence = core.emit_thread(events::THREAD_CREATED, &thread);
            }
            CommandKind::UpdateThread => {
                let c = command.clone();
                receipt.sequence = core
                    .update_thread(&command.thread_id, move |t| {
                        if !c.title.trim().is_empty() {
                            t.title = c.title.trim().to_owned();
                        }
                        if let Some(pinned) = c.pinned {
                            t.pinned = pinned;
                        }
                        if let Some(archived) = c.archived {
                            t.archived = archived;
                        }
                        if let Some(until) = &c.snoozed_until {
                            t.snoozed_until = (!until.is_empty()).then(|| until.clone());
                        }
                        if !c.approval_policy.is_empty() {
                            t.approval_policy = c.approval_policy.clone();
                        }
                        if !c.interaction_mode.is_empty() {
                            t.interaction_mode = c.interaction_mode.clone();
                        }
                    })
                    .unwrap_or_default();
            }
            CommandKind::SetThreadModel => {
                let (provider, model) = (command.provider_id.clone(), command.model.clone());
                if model.is_empty() {
                    return Err(invalid("model is required"));
                }
                receipt.sequence = core
                    .update_thread(&command.thread_id, move |t| {
                        if !provider.is_empty() {
                            t.provider_id = provider;
                        }
                        t.model = model;
                    })
                    .unwrap_or_default();
            }
            CommandKind::DeleteThread => {
                let id = command.thread_id.clone();
                if core.active_run(&id).is_some() && !command.force {
                    return Err(BridgeError::new(
                        ErrorCode::Conflict,
                        "thread has an active run",
                    ));
                }
                core.threads.retain(|t| t.id != id);
                core.runs.retain(|r| r.thread_id != id);
                core.blocks.retain(|b| b.thread_id != id);
                core.tools.retain(|t| t.thread_id != id);
                core.requests.retain(|r| r.thread_id != id);
                core.inputs.retain(|i| i.thread_id != id);
                receipt.sequence =
                    core.emit(events::THREAD_DELETED, &id, "", json!({ "threadId": id }));
            }
            CommandKind::ForkThread => {
                let source = core.thread(&command.thread_id).cloned().unwrap_or_default();
                let runs: Vec<Run> = core
                    .runs
                    .iter()
                    .filter(|r| r.thread_id == source.id && r.status.terminal())
                    .cloned()
                    .collect();
                let cut = if command.run_id.is_empty() {
                    runs.len()
                } else {
                    runs.iter()
                        .position(|r| r.id == command.run_id)
                        .map(|i| i + 1)
                        .ok_or_else(|| not_found("run"))?
                };
                let fork = Thread {
                    id: core.id("thread"),
                    title: if command.title.is_empty() {
                        format!("{}（分叉）", source.title)
                    } else {
                        command.title.clone()
                    },
                    created_at: format!("2026-11-23T11:{:02}:00Z", core.next_id % 60),
                    forked_from_thread_id: source.id.clone(),
                    forked_from_run_id: runs.get(cut.wrapping_sub(1)).map(|r| r.id.clone()).unwrap_or_default(),
                    pinned: false,
                    archived: false,
                    snoozed_until: None,
                    ..source.clone()
                };
                core.threads.push(fork.clone());
                core.emit_thread(events::THREAD_CREATED, &fork);
                for run in runs.into_iter().take(cut) {
                    let copy = Run {
                        id: core.id("run"),
                        thread_id: fork.id.clone(),
                        ..run
                    };
                    core.runs.push(copy.clone());
                    core.emit(
                        events::of_run_status(copy.status),
                        &copy.thread_id,
                        &copy.id,
                        json!({ "run": value(&copy) }),
                    );
                }
                receipt.thread_id = fork.id;
                receipt.sequence = core.sequence;
            }
            CommandKind::RewindThread => {
                let target = if command.run_id.is_empty() {
                    command.source_run_id.clone()
                } else {
                    command.run_id.clone()
                };
                if target.is_empty() {
                    return Err(invalid("runId is required"));
                }
                let thread_runs: Vec<String> = core
                    .runs
                    .iter()
                    .filter(|r| r.thread_id == command.thread_id)
                    .map(|r| r.id.clone())
                    .collect();
                let at = thread_runs
                    .iter()
                    .position(|id| *id == target)
                    .ok_or_else(|| not_found("run"))?;
                let removed: Vec<String> = thread_runs[at..].to_vec();
                if core
                    .runs
                    .iter()
                    .any(|r| removed.contains(&r.id) && !r.status.terminal())
                {
                    return Err(BridgeError::new(ErrorCode::Conflict, "stop the run first"));
                }
                core.runs.retain(|r| !removed.contains(&r.id));
                core.blocks.retain(|b| !removed.contains(&b.run_id));
                core.tools.retain(|t| !removed.contains(&t.run_id));
                core.requests.retain(|r| !removed.contains(&r.run_id));
                receipt.run_id = target.clone();
                receipt.sequence = core.emit(
                    events::THREAD_REWOUND,
                    &command.thread_id,
                    &target,
                    json!({ "threadId": command.thread_id, "runId": target, "removedRunIds": removed }),
                );
            }
            CommandKind::MergeBackThread => {
                let fork = core.thread(&command.thread_id).cloned().unwrap_or_default();
                if fork.forked_from_thread_id.is_empty()
                    || core.thread(&fork.forked_from_thread_id).is_none()
                {
                    return Err(BridgeError::new(
                        ErrorCode::Conflict,
                        "thread is not a fork of a live thread",
                    ));
                }
                let summary: Vec<String> = core
                    .runs
                    .iter()
                    .filter(|r| r.thread_id == fork.id && r.status == RunStatus::Completed)
                    .map(|r| format!("- {}", r.input.lines().next().unwrap_or("")))
                    .collect();
                let target = fork.forked_from_thread_id.clone();
                let mut run = core.push_run(&target, &format!("合并分叉「{}」", fork.title), "");
                run.status = RunStatus::Completed;
                run.output = format!("已合并分叉的结论：\n\n{}", summary.join("\n"));
                if let Some(stored) = core.run_mut(&run.id) {
                    *stored = run.clone();
                }
                core.emit(
                    events::RUN_COMPLETED,
                    &target,
                    &run.id,
                    json!({ "run": value(&run) }),
                );
                receipt.thread_id = target;
                receipt.run_id = run.id;
                receipt.sequence = core.sequence;
            }
            CommandKind::SendMessage => {
                let run = core.push_run(
                    &command.thread_id,
                    &command.message,
                    &command.interaction_mode,
                );
                receipt.run_id = run.id.clone();
                receipt.sequence = core.sequence;
                replies.push(Reply {
                    run_id: run.id,
                    message: command.message.clone(),
                });
            }
            CommandKind::QueueMessage => {
                if core.active_run(&command.thread_id).is_some() {
                    let position = core.pending_queue(&command.thread_id).len() as i64;
                    let input = Input {
                        id: core.id("input"),
                        command_id: command.command_id.clone(),
                        thread_id: command.thread_id.clone(),
                        kind: "queue".into(),
                        message: command.message.clone(),
                        attachments: command.attachments.clone(),
                        status: "queued".into(),
                        position,
                        ..Input::default()
                    };
                    receipt.input_id = input.id.clone();
                    receipt.delivery = "queued".into();
                    receipt.sequence = core.upsert_input(input);
                } else {
                    let run = core.push_run(
                        &command.thread_id,
                        &command.message,
                        &command.interaction_mode,
                    );
                    receipt.run_id = run.id.clone();
                    receipt.sequence = core.sequence;
                    replies.push(Reply {
                        run_id: run.id,
                        message: command.message.clone(),
                    });
                }
            }
            CommandKind::SteerMessage | CommandKind::SteerRun => {
                let run_id = command.run_id.clone();
                if core.runs.iter().all(|r| r.id != run_id || r.status.terminal()) {
                    return Err(BridgeError::new(ErrorCode::Conflict, "run is not active"));
                }
                let input = Input {
                    id: core.id("input"),
                    command_id: command.command_id.clone(),
                    thread_id: command.thread_id.clone(),
                    run_id: run_id.clone(),
                    attempt_id: format!("{run_id}-a1"),
                    kind: "steer".into(),
                    message: command.message.clone(),
                    attachments: command.attachments.clone(),
                    status: "delivered".into(),
                    ..Input::default()
                };
                receipt.input_id = input.id.clone();
                core.upsert_input(input);
                let delta = format!("已收到调整：{}。", command.message);
                core.output(&run_id, &delta);
                receipt.run_id = run_id;
                receipt.delivery = "delivered".into();
                receipt.sequence = core.sequence;
            }
            CommandKind::EditInput => {
                let mut input = core.input(&command)?;
                if !input.is_queued() {
                    return Err(BridgeError::new(ErrorCode::Conflict, "input already started"));
                }
                if command.message.trim().is_empty() {
                    return Err(invalid("message is required"));
                }
                input.message = command.message.clone();
                if !command.attachments.is_empty() {
                    input.attachments = command.attachments.clone();
                }
                receipt.input_id = input.id.clone();
                receipt.sequence = core.upsert_input(input);
            }
            CommandKind::ReorderInput => {
                let input = core.input(&command)?;
                let mut queue = core.pending_queue(&input.thread_id);
                let from = queue
                    .iter()
                    .position(|i| i.id == input.id)
                    .ok_or_else(|| BridgeError::new(ErrorCode::Conflict, "input already started"))?;
                let moved = queue.remove(from);
                let to = (command.position.max(0) as usize).min(queue.len());
                queue.insert(to, moved);
                core.renumber(queue);
                receipt.input_id = input.id;
                receipt.sequence = core.sequence;
            }
            CommandKind::PromoteInputToSteer => {
                let mut input = core.input(&command)?;
                let run = core
                    .active_run(&input.thread_id)
                    .filter(|r| r.status == RunStatus::Running && (command.run_id.is_empty() || r.id == command.run_id))
                    .cloned()
                    .ok_or_else(|| BridgeError::new(ErrorCode::Conflict, "no matching running run"))?;
                if !input.is_queued() {
                    return Err(BridgeError::new(ErrorCode::Conflict, "input already started"));
                }
                input.kind = "steer".into();
                input.status = "delivered".into();
                input.run_id = run.id.clone();
                core.upsert_input(input.clone());
                let delta = format!("\n\n（已收到调整：{}）\n\n", input.message);
                core.output(&run.id, &delta);
                let rest = core.pending_queue(&input.thread_id);
                core.renumber(rest);
                receipt.input_id = input.id;
                receipt.run_id = run.id;
                receipt.delivery = "delivered".into();
                receipt.sequence = core.sequence;
            }
            CommandKind::CancelInput | CommandKind::CancelMessage => {
                let id = if command.input_id.is_empty() {
                    command.message_id.clone()
                } else {
                    command.input_id.clone()
                };
                let mut input = core
                    .inputs
                    .iter()
                    .find(|i| i.id == id)
                    .cloned()
                    .ok_or_else(|| not_found("input"))?;
                if input.is_queued() {
                    input.status = "cancelled".into();
                    core.upsert_input(input.clone());
                    let rest = core.pending_queue(&input.thread_id);
                    core.renumber(rest);
                }
                receipt.input_id = input.id;
                receipt.sequence = core.sequence;
            }
            CommandKind::StopRun => {
                core.set_run_status(&command.run_id, RunStatus::Cancelled, "");
                let pending: Vec<PendingRequest> = core
                    .requests
                    .iter()
                    .filter(|r| r.run_id == command.run_id && r.status == "pending")
                    .cloned()
                    .collect();
                for mut request in pending {
                    request.status = "expired".into();
                    core.upsert_request(request);
                }
                // Stopping cancels what was queued behind the run.
                for mut input in core.pending_queue(&command.thread_id) {
                    input.status = "cancelled".into();
                    core.upsert_input(input);
                }
                receipt.run_id = command.run_id.clone();
                receipt.sequence = core.sequence;
            }
            CommandKind::RespondRequest => {
                let Some(mut request) = core
                    .requests
                    .iter()
                    .find(|r| r.id == command.request_id)
                    .cloned()
                else {
                    return Err(not_found("request"));
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
                return Err(invalid(&format!("unsupported command kind {other:?}")));
            }
        }
        core.commands.push(command.clone());
        core.receipts
            .insert(command.command_id.clone(), (command, receipt.clone()));
        Ok((receipt, replies))
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

    async fn stream(&self, run_id: &str, text: &str, piece: usize) -> bool {
        for piece in split_pieces(text, piece) {
            if !self.live(run_id) {
                return false;
            }
            self.pause().await;
            self.core.lock().unwrap().output(run_id, &piece);
        }
        true
    }

    /// Streams a reply to `reply`: reasoning, text, a tool behind an
    /// approval, Markdown, completion; then the thread's next queued input.
    /// Stops where the run was stopped.
    fn script(self, reply: Reply) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send>> {
        Box::pin(async move { self.script_then_next(reply).await })
    }

    async fn script_then_next(self, reply: Reply) {
        let run_id = reply.run_id.clone();
        self.script_run(reply).await;
        let thread_id = {
            let core = self.core.lock().unwrap();
            core.runs
                .iter()
                .find(|r| r.id == run_id)
                .map(|r| r.thread_id.clone())
        };
        let next = thread_id.and_then(|id| self.core.lock().unwrap().start_next(&id));
        if let Some(next) = next {
            tokio::spawn(self.clone().script(next));
        }
    }

    async fn script_run(&self, reply: Reply) {
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
        if !self.stream(&run_id, &opening, 6).await {
            return;
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
            let request = PendingRequest {
                id: core.id("request"),
                provider_request_id: "ui-1".into(),
                thread_id: run.thread_id.clone(),
                run_id: run.id.clone(),
                attempt_id: tool.attempt_id.clone(),
                method: "select".into(),
                title: "允许执行 bash？\nls -la".into(),
                options: vec!["允许本次".into(), "本会话允许 bash".into(), "拒绝".into()],
                status: "pending".into(),
                ..PendingRequest::default()
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
                tool.output =
                    "total 8\ndrwxr-xr-x  4 vantry  staff  128 .\n-rw-r--r--  1 vantry  staff  311 Cargo.toml\n".into();
            } else {
                tool.status = "failed".into();
                tool.output = "用户拒绝执行".into();
            }
            core.upsert_tool(tool);
        }
        let closing = if allowed {
            format!("\n\n{DEMO_MARKDOWN}")
        } else {
            "好的，不执行这条命令。".to_owned()
        };
        if !self.stream(&run_id, &closing, 12).await {
            return;
        }
        self.pause().await;
        self.core
            .lock()
            .unwrap()
            .set_run_status(&run_id, RunStatus::Completed, "");
    }
}

/// The answer a `request.respond` gives, as the Core records it.
fn answer_of<'a>(request: &PendingRequest, command: &'a Command) -> &'a str {
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

    fn thread_summaries(&self) -> BridgeFuture<ThreadSummaries> {
        let core = self.core.clone();
        Box::pin(async move { Ok(core.lock().unwrap().thread_summaries()) })
    }

    fn thread_detail(&self, thread_id: String, limit_runs: i64) -> BridgeFuture<ThreadDetail> {
        let core = self.core.clone();
        Box::pin(async move {
            core.lock()
                .unwrap()
                .thread_detail(&thread_id, limit_runs)
        })
    }

    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt> {
        let this = self.clone();
        Box::pin(async move {
            let (receipt, replies) = this.handle(command)?;
            this.wake.notify_waiters();
            if this.scripted {
                for reply in replies {
                    tokio::spawn(this.clone().script(reply));
                }
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

    async fn next(events: &mut tokio::sync::mpsc::UnboundedReceiver<CoreEvent>) -> CoreEvent {
        tokio::time::timeout(Duration::from_secs(5), events.recv())
            .await
            .expect("an event in time")
            .expect("the stream is open")
    }

    fn cmd(id: &str, kind: CommandKind, thread_id: &str) -> Command {
        let mut command = Command::new(id, kind);
        command.thread_id = thread_id.into();
        command
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
        let mut create = Command::new("thread-1", CommandKind::CreateThread);
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
            .dispatch(Command::new("c1", CommandKind::CreateThread))
            .await
            .unwrap();
        let mut send = cmd("s1", CommandKind::SendMessage, &created.thread_id);
        send.message = "看看目录".into();
        bridge.dispatch(send).await.unwrap();
        let request = loop {
            let event = next(&mut events).await;
            if event.r#type == events::REQUEST_UPDATED {
                break serde_json::from_value::<PendingRequest>(event.data["request"].clone())
                    .unwrap();
            }
        };
        assert_eq!(request.status, "pending");
        let mut respond = cmd("r1", CommandKind::RespondRequest, &request.thread_id);
        respond.run_id = request.run_id.clone();
        respond.request_id = request.id.clone();
        respond.value = "拒绝".into();
        bridge.dispatch(respond).await.unwrap();
        let mut tool_failed = false;
        loop {
            let event = next(&mut events).await;
            if event.r#type == events::TOOL_UPDATED && event.data["tool"]["status"] == "failed" {
                tool_failed = true;
            }
            if event.r#type == events::RUN_COMPLETED {
                break;
            }
        }
        assert!(tool_failed);
    }

    #[tokio::test]
    async fn queued_inputs_wait_reorder_promote_and_start_after_the_run() {
        let bridge = MockBridge::quiet();
        let thread = bridge
            .dispatch(Command::new("c", CommandKind::CreateThread))
            .await
            .unwrap()
            .thread_id;
        let mut send = cmd("s", CommandKind::SendMessage, &thread);
        send.message = "first".into();
        let run = bridge.dispatch(send).await.unwrap().run_id;
        let mut ids = Vec::new();
        for (n, text) in ["a", "b", "c"].iter().enumerate() {
            let mut queue = cmd(&format!("q{n}"), CommandKind::QueueMessage, &thread);
            queue.message = (*text).into();
            let receipt = bridge.dispatch(queue).await.unwrap();
            assert_eq!(receipt.delivery, "queued");
            ids.push(receipt.input_id);
        }
        let mut reorder = cmd("m", CommandKind::ReorderInput, &thread);
        reorder.input_id = ids[2].clone();
        reorder.position = 0;
        bridge.dispatch(reorder).await.unwrap();
        let order: Vec<String> = {
            let core = bridge.core.lock().unwrap();
            core.pending_queue(&thread).iter().map(|i| i.message.clone()).collect()
        };
        assert_eq!(order, ["c", "a", "b"]);

        let mut edit = cmd("e", CommandKind::EditInput, &thread);
        edit.input_id = ids[0].clone();
        edit.message = "a2".into();
        bridge.dispatch(edit).await.unwrap();

        // Promotion needs a running run.
        let mut promote = cmd("p", CommandKind::PromoteInputToSteer, &thread);
        promote.input_id = ids[1].clone();
        assert_eq!(
            bridge.dispatch(promote.clone()).await.unwrap_err().code,
            ErrorCode::Conflict
        );
        bridge.core.lock().unwrap().set_run_status(&run, RunStatus::Running, "");
        promote.command_id = "p2".into();
        bridge.dispatch(promote).await.unwrap();

        bridge.finish_run(&run, RunStatus::Completed);
        let core = bridge.core.lock().unwrap();
        let started = core.inputs.iter().find(|i| i.status == "started").unwrap();
        assert_eq!(started.message, "c");
        let rest: Vec<String> = core.pending_queue(&thread).iter().map(|i| i.message.clone()).collect();
        assert_eq!(rest, ["a2"]);
    }

    #[tokio::test]
    async fn threads_update_fork_rewind_merge_and_delete() {
        let bridge = MockBridge::quiet();
        let thread = bridge
            .dispatch(Command::new("c", CommandKind::CreateThread))
            .await
            .unwrap()
            .thread_id;
        let mut runs = Vec::new();
        for n in 0..3 {
            let mut send = cmd(&format!("s{n}"), CommandKind::SendMessage, &thread);
            send.message = format!("turn {n}");
            let run = bridge.dispatch(send).await.unwrap().run_id;
            bridge.finish_run(&run, RunStatus::Completed);
            runs.push(run);
        }
        let mut update = cmd("u", CommandKind::UpdateThread, &thread);
        update.title = "重命名".into();
        update.pinned = Some(true);
        update.snoozed_until = Some("2099-01-01T00:00:00Z".into());
        bridge.dispatch(update).await.unwrap();
        {
            let core = bridge.core.lock().unwrap();
            let t = core.thread(&thread).unwrap();
            assert_eq!((t.title.as_str(), t.pinned), ("重命名", true));
            assert!(t.snoozed_until.is_some());
        }

        let mut fork = cmd("f", CommandKind::ForkThread, &thread);
        fork.run_id = runs[1].clone();
        let forked = bridge.dispatch(fork).await.unwrap().thread_id;
        {
            let core = bridge.core.lock().unwrap();
            assert_eq!(core.runs.iter().filter(|r| r.thread_id == forked).count(), 2);
            assert_eq!(core.thread(&forked).unwrap().forked_from_thread_id, thread);
        }

        let mut rewind = cmd("r", CommandKind::RewindThread, &thread);
        rewind.run_id = runs[1].clone();
        bridge.dispatch(rewind).await.unwrap();
        assert_eq!(
            bridge.core.lock().unwrap().runs.iter().filter(|r| r.thread_id == thread).count(),
            1
        );

        let merged = bridge
            .dispatch(cmd("mb", CommandKind::MergeBackThread, &forked))
            .await
            .unwrap();
        assert_eq!(merged.thread_id, thread);
        assert_eq!(
            bridge
                .dispatch(cmd("mb2", CommandKind::MergeBackThread, &thread))
                .await
                .unwrap_err()
                .code,
            ErrorCode::Conflict
        );

        bridge
            .dispatch(cmd("d", CommandKind::DeleteThread, &forked))
            .await
            .unwrap();
        assert!(bridge.core.lock().unwrap().thread(&forked).is_none());
    }

    #[test]
    fn the_demo_has_a_task_tree_and_every_group() {
        let bridge = MockBridge::demo();
        let core = bridge.core.lock().unwrap();
        assert_eq!(core.tasks.len(), 2);
        assert!(core.threads.iter().any(|t| t.pinned));
        assert!(core.threads.iter().any(|t| t.archived));
        assert!(core.threads.iter().any(|t| t.snoozed_until.is_some()));
        assert!(core.threads.iter().any(|t| !t.parent_thread_id.is_empty()));
    }
}
