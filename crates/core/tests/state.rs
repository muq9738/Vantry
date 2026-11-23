use async_trait::async_trait;
use serde_json::{Value, json};
use std::{
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::{mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use vantry_core::{
    Core, DurableProvider, ProviderError,
    runner::{Emit, ProviderResult, StartMode},
};
use vantry_protocol::*;

struct Execution {
    req: ProviderRequest,
    mode: StartMode,
    emit: Emit,
    done: oneshot::Sender<ProviderResult>,
}
struct Provider {
    runs: mpsc::UnboundedSender<Execution>,
    controls: mpsc::UnboundedSender<ControlResponse>,
    recoverable: bool,
    stops: Mutex<Vec<String>>,
}
#[async_trait]
impl DurableProvider for Provider {
    async fn run_events(
        &self,
        req: ProviderRequest,
        mode: StartMode,
        emit: Emit,
    ) -> ProviderResult {
        let (done, wait) = oneshot::channel();
        self.runs
            .send(Execution {
                req,
                mode,
                emit,
                done,
            })
            .unwrap_or_else(|_| panic!("dropped test receiver"));
        wait.await.unwrap_or(Err(ProviderError::Detached))
    }
    async fn recoverable(&self, _: &ProviderRequest) -> bool {
        self.recoverable
    }
    async fn stop(&self, r: &ProviderRequest) -> ProviderResult {
        self.stops.lock().unwrap().push(r.attempt_id.clone());
        Ok(())
    }
    async fn control(&self, _: &ProviderRequest, c: &ControlResponse) -> ProviderResult {
        self.controls.send(c.clone()).unwrap();
        Ok(())
    }
}
struct Harness {
    core: Core,
    runs: mpsc::UnboundedReceiver<Execution>,
    controls: mpsc::UnboundedReceiver<ControlResponse>,
    provider: Arc<Provider>,
    stop: CancellationToken,
    worker: tokio::task::JoinHandle<vantry_core::Result<()>>,
}
impl Harness {
    fn open(dir: &std::path::Path, recoverable: bool) -> Self {
        let core = Core::open(dir).unwrap();
        let (tx, runs) = mpsc::unbounded_channel();
        let (cx, controls) = mpsc::unbounded_channel();
        let provider = Arc::new(Provider {
            runs: tx,
            controls: cx,
            recoverable,
            stops: Default::default(),
        });
        let stop = CancellationToken::new();
        let worker = tokio::spawn({
            let p = provider.clone();
            let c = core.clone();
            let token = stop.clone();
            async move { c.run_scheduler(p, token).await }
        });
        Self {
            core,
            runs,
            controls,
            provider,
            stop,
            worker,
        }
    }
    async fn next(&mut self) -> Execution {
        tokio::time::timeout(Duration::from_secs(5), self.runs.recv())
            .await
            .unwrap()
            .unwrap()
    }
    async fn control(&mut self) -> ControlResponse {
        tokio::time::timeout(Duration::from_secs(5), self.controls.recv())
            .await
            .unwrap()
            .unwrap()
    }
    async fn wait(&self, predicate: impl Fn(&Snapshot) -> bool) {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                if predicate(&self.core.snapshot().unwrap()) {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
    }
    async fn close(self) {
        self.stop.cancel();
        self.worker.await.unwrap().unwrap();
    }
}
fn cmd(core: &Core, kind: &str, args: Value) -> Receipt {
    let mut v = args;
    v["kind"] = kind.into();
    v["version"] = 1.into();
    v["commandId"] = uuid::Uuid::new_v4().to_string().into();
    let command: Command = serde_json::from_value(v).unwrap();
    let accepted = core
        .dispatch(&command)
        .unwrap_or_else(|e| panic!("{kind}: {e}"));
    assert_eq!(core.dispatch(&command).unwrap(), accepted);
    let conflict = Command {
        reason: "different semantic payload".into(),
        ..command
    };
    assert_eq!(
        core.dispatch(&conflict).unwrap_err().code(),
        ErrorCode::Conflict
    );
    accepted
}
fn emit(e: &Execution, sequence: i64, kind: &str, data: Value, delta: &str) {
    (e.emit)(ProviderEvent {
        sequence,
        r#type: kind.into(),
        delta: delta.into(),
        data: Some(data),
        ..Default::default()
    })
    .unwrap();
}
fn status(s: &Snapshot, key: &str) -> RunStatus {
    s.runs
        .as_ref()
        .unwrap()
        .iter()
        .find(|r| r.id == key)
        .unwrap()
        .status
}
#[tokio::test]
async fn commands_queue_steer_approvals_and_thread_history() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let project = cmd(
        &core,
        "project.chat.open",
        json!({"workspace":{"kind":"directory","path":"/example"}}),
    );
    assert_eq!(
        cmd(
            &core,
            "project.chat.open",
            json!({"workspace":{"kind":"directory","path":"/example"}})
        )
        .thread_id,
        project.thread_id
    );
    let th = cmd(
        &core,
        "thread.create",
        json!({"title":"start","model":"first"}),
    )
    .thread_id;
    cmd(
        &core,
        "thread.update",
        json!({"threadId":th,"title":"updated","pinned":true}),
    );
    let send = cmd(
        &core,
        "message.send",
        json!({"threadId":th,"message":"question"}),
    );
    let e = h.next().await;
    assert_eq!(e.req.message, "question");
    assert!(core.drain_and_shutdown().is_err());
    let q = |m: &str| cmd(&core, "message.queue", json!({"threadId":th,"message":m})).input_id;
    let a = q("one");
    let b = q("two");
    let d = q("three");
    let f = q("four");
    let g = q("five");
    cmd(&core, "thread.model", json!({"threadId":th,"model":"next"}));
    cmd(&core, "input.edit", json!({"inputId":a,"message":"edited"}));
    cmd(&core, "input.reorder", json!({"inputId":b,"position":0}));
    cmd(&core, "message.cancel", json!({"messageId":d}));
    cmd(&core, "input.cancel", json!({"inputId":f}));
    cmd(&core, "input.promote_steer", json!({"inputId":g}));
    let ctrl = h.control().await;
    assert_eq!(ctrl.id, g);
    emit(
        &e,
        1,
        "control_result",
        json!({"id":g,"status":"delivered"}),
        "",
    );
    for (index, kind) in ["run.steer", "message.steer"].iter().enumerate() {
        let r = cmd(
            &core,
            kind,
            json!({"threadId":th,"runId":send.run_id,"message":"new direction"}),
        );
        assert_eq!(h.control().await.id, r.input_id);
        emit(
            &e,
            2 + index as i64,
            "control_result",
            json!({"id":r.input_id,"status":"delivered"}),
            "",
        );
    }
    for (index, confirmed) in [true, false].into_iter().enumerate() {
        let source = format!("ask{index}");
        let request = format!("{}:{source}", e.req.attempt_id);
        let seq = 4 + index as i64 * 2;
        emit(
            &e,
            seq,
            "extension_ui_request",
            json!({"id":source,"method":"confirm","title":"Allow?"}),
            "",
        );
        cmd(
            &core,
            "request.respond",
            json!({"requestId":request,"confirmed":confirmed}),
        );
        let control = h.control().await;
        assert_eq!(control.confirmed, confirmed);
        assert_eq!(control.request_id, source);
        emit(
            &e,
            seq + 1,
            "control_result",
            json!({"id":request,"status":"delivered"}),
            "",
        );
    }
    emit(
        &e,
        8,
        "extension_ui_request",
        json!({"id":"late","method":"confirm","timeout":1}),
        "",
    );
    assert!(h.control().await.cancelled);
    assert_eq!(
        core.snapshot()
            .unwrap()
            .requests
            .iter()
            .filter(|r| r.status == "responded")
            .count(),
        2
    );
    assert!(
        core.snapshot()
            .unwrap()
            .requests
            .iter()
            .any(|r| r.status == "expired")
    );
    emit(
        &e,
        9,
        "tool_execution_start",
        json!({"toolCallId":"t","toolName":"read","args":{"path":"x"}}),
        "",
    );
    emit(
        &e,
        10,
        "tool_execution_end",
        json!({"toolCallId":"t","result":"text"}),
        "",
    );
    emit(
        &e,
        11,
        "thinking_delta",
        json!({"assistantMessageEvent":{"delta":"thought"}}),
        "",
    );
    emit(&e, 12, "", Value::Null, "reply");
    emit(
        &e,
        13,
        "message_end",
        json!({"message":{"usage":{"input":10,"output":3,"reasoning":1,"cost":{"total":0.003}}}}),
        "",
    );
    emit(
        &e,
        14,
        "turn_end",
        json!({"message":{"usage":{"input":10,"output":3,"cost":{"total":0.003}}}}),
        "",
    );
    e.done.send(Ok(())).unwrap();
    let second = h.next().await;
    assert_eq!(second.req.message, "two");
    assert_eq!(second.req.model, "first");
    second.done.send(Ok(())).unwrap();
    let third = h.next().await;
    assert_eq!(third.req.message, "edited");
    third.done.send(Ok(())).unwrap();
    h.wait(|s| s.runs.as_ref().unwrap().iter().all(|r| r.status.terminal()))
        .await;
    let snap = core.snapshot().unwrap();
    let first_run = &snap.runs.unwrap()[0];
    assert_eq!(first_run.telemetry.as_ref().unwrap().total_tokens, 13);
    let summary = core.usage_summary(Some(&th)).unwrap();
    assert_eq!(summary.total_tokens, 13);
    assert_eq!(summary.turn_count, 1);
    assert_eq!(summary.total_cost_usd, 0.003);
    assert!(summary.total_duration_ms > 0);
    let ledger: Value =
        serde_json::from_slice(&std::fs::read(dir.path().join("usage-ledger.json")).unwrap())
            .unwrap();
    assert_eq!(ledger["records"][0]["attemptId"], e.req.attempt_id);
    assert_eq!(first_run.reasoning, "thought");
    assert_eq!(snap.tools[0].status, "completed");
    let fork = cmd(
        &core,
        "thread.fork",
        json!({"threadId":th,"sourceRunId":send.run_id}),
    );
    let fork_run = core
        .snapshot()
        .unwrap()
        .runs
        .unwrap()
        .into_iter()
        .find(|r| r.thread_id == fork.thread_id)
        .unwrap();
    assert_eq!(fork_run.output, "reply");
    let merge = cmd(
        &core,
        "thread.merge_back",
        json!({"threadId":fork.thread_id}),
    );
    let delivery = h.next().await;
    assert_eq!(delivery.req.run_id, merge.run_id);
    assert!(delivery.req.message.contains("reply"));
    cmd(
        &core,
        "run.stop",
        json!({"threadId":th,"runId":merge.run_id}),
    );
    h.wait(|_| !h.provider.stops.lock().unwrap().is_empty())
        .await;
    delivery
        .done
        .send(Err(ProviderError::ExecutionCancelled))
        .unwrap();
    cmd(
        &core,
        "thread.rewind",
        json!({"threadId":fork.thread_id,"runId":fork_run.id}),
    );
    let sent = cmd(
        &core,
        "thread.inter_send",
        json!({"targetThreadId":fork.thread_id,"senderThreadId":th,"message":"hello"}),
    );
    assert_eq!(sent.delivery, "started");
    let inter = h.next().await;
    assert_eq!(inter.req.sender_thread_id, th);
    inter.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &sent.run_id).terminal()).await;
    cmd(&core, "thread.delete", json!({"threadId":fork.thread_id}));
    assert!(
        !core
            .snapshot()
            .unwrap()
            .threads
            .unwrap()
            .iter()
            .any(|t| t.id == fork.thread_id)
    );
    h.close().await;
    core.drain_and_shutdown().unwrap();
}

#[tokio::test]
async fn generations_deliver_only_once_and_preserve_parent_input() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let th = cmd(&core, "thread.create", json!({})).thread_id;
    let parent = cmd(
        &core,
        "message.send",
        json!({"threadId":th,"message":"parent"}),
    )
    .run_id;
    let p = h.next().await;
    let child = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId":parent,"parentAttemptId":p.req.attempt_id,"message":"child","resumePolicy":"manual"}),
    );
    let e = h.next().await;
    emit(&e, 1, "", Value::Null, "result one");
    e.done.send(Ok(())).unwrap();
    p.done.send(Ok(())).unwrap();
    h.wait(|s| {
        status(s, &parent) == RunStatus::Waiting
            && s.cohorts.as_ref().unwrap()[0].status == DeliveryStatus::Delivered
    })
    .await;
    cmd(
        &core,
        "task.acknowledge",
        json!({"parentRunId":parent,"generation":1,"deliveryMessageId":child.delivery_message_id}),
    );
    let continuation = h.next().await;
    assert!(continuation.req.message.contains("result one"));
    assert_eq!(continuation.req.run_id, parent);
    cmd(
        &core,
        "task.acknowledge",
        json!({"parentRunId":parent,"generation":1,"deliveryMessageId":child.delivery_message_id}),
    );
    continuation.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Completed).await;
    let next = cmd(
        &core,
        "task.followup",
        json!({"taskId":child.task_id,"generation":1,"message":"again"}),
    );
    assert_eq!(next.generation, 2);
    assert_ne!(next.delivery_message_id, child.delivery_message_id);
    let follow = h.next().await;
    cmd(&core, "task.wake_upgrade", json!({"taskId":child.task_id}));
    emit(&follow, 1, "", Value::Null, "second result");
    follow.done.send(Ok(())).unwrap();
    let resumed = h.next().await;
    assert!(resumed.req.message.contains("second result"));
    resumed.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Completed).await;
    let third = cmd(
        &core,
        "task.followup",
        json!({"taskId":child.task_id,"generation":2,"message":"cancel","resumePolicy":"manual"}),
    );
    let pending = h.next().await;
    cmd(&core, "task.cancel", json!({"taskId":third.task_id}));
    pending
        .done
        .send(Err(ProviderError::ExecutionCancelled))
        .unwrap();
    cmd(
        &core,
        "task.dispose",
        json!({"taskId":third.task_id,"generation":3,"deliveryMessageId":third.delivery_message_id}),
    );
    let fourth = cmd(
        &core,
        "task.followup",
        json!({"taskId":child.task_id,"generation":3,"message":"discard this result","resumePolicy":"manual"}),
    );
    let last = h.next().await;
    last.done.send(Ok(())).unwrap();
    h.wait(|s| s.cohorts.as_ref().unwrap()[3].status == DeliveryStatus::Delivered)
        .await;
    cmd(
        &core,
        "task.dispose",
        json!({"taskId":fourth.task_id,"generation":4,"deliveryMessageId":fourth.delivery_message_id}),
    );
    let snap = core.snapshot().unwrap();
    assert_eq!(status(&snap, &parent), RunStatus::Completed);
    assert_eq!(snap.runs.unwrap()[0].input, "parent");
    assert_eq!(snap.cohorts.unwrap()[2].status, DeliveryStatus::Disposed);
    h.close().await;
}

#[tokio::test]
async fn restart_preserves_cursor_and_handles_unknown_submission() {
    for recoverable in [true, false] {
        let dir = tempfile::tempdir().unwrap();
        let mut first = Harness::open(dir.path(), true);
        let th = cmd(&first.core, "thread.create", json!({})).thread_id;
        let run = cmd(
            &first.core,
            "message.send",
            json!({"threadId":th,"message":"only once"}),
        )
        .run_id;
        let old = first.next().await;
        assert_eq!(old.mode, StartMode::Submit);
        emit(&old, 1, "", Value::Null, "a");
        let att = old.req.attempt_id.clone();
        assert!(
            (old.emit)(ProviderEvent {
                sequence: 3,
                delta: "gap".into(),
                ..Default::default()
            })
            .is_err()
        );
        old.done.send(Err(ProviderError::Detached)).unwrap();
        drop(old.emit);
        first.close().await;
        let mut second = Harness::open(dir.path(), recoverable);
        if recoverable {
            let attached = second.next().await;
            assert_eq!(attached.mode, StartMode::Attach);
            assert_eq!(attached.req.attempt_id, att);
            assert_eq!(attached.req.after_sequence, 1);
            assert!(attached.req.message.is_empty());
            emit(&attached, 1, "", Value::Null, "duplicate");
            emit(&attached, 2, "", Value::Null, "b");
            attached.done.send(Ok(())).unwrap();
        }
        second.wait(|s| status(s, &run).terminal()).await;
        let r = second.core.snapshot().unwrap().runs.unwrap().pop().unwrap();
        assert_eq!(
            r.status,
            if recoverable {
                RunStatus::Completed
            } else {
                RunStatus::Interrupted
            }
        );
        assert_eq!(r.output, if recoverable { "ab" } else { "a" });
        second.close().await;
    }
}
