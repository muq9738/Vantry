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
async fn streaming_batches_ready_deltas_but_flushes_before_tools() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let thread = cmd(&h.core, "thread.create", json!({})).thread_id;
    let run = cmd(
        &h.core,
        "message.send",
        json!({"threadId":thread,"message":"stream"}),
    )
    .run_id;
    let e = h.next().await;

    emit(
        &e,
        1,
        "thinking_delta",
        json!({"assistantMessageEvent":{"delta":"thought"}}),
        "",
    );
    let snapshot = h.core.snapshot().unwrap();
    assert_eq!(snapshot.runs.as_ref().unwrap()[0].reasoning, "thought");
    emit(&e, 2, "text_delta", Value::Null, "a");
    assert_eq!(h.core.snapshot().unwrap().runs.unwrap()[0].output, "a");
    emit(&e, 3, "text_delta", Value::Null, "b");
    emit(&e, 4, "text_delta", Value::Null, "c");

    emit(
        &e,
        5,
        "tool_execution_start",
        json!({"toolCallId":"tool-1","toolName":"read","args":{"path":"note.txt"}}),
        "",
    );
    let snapshot = h.core.snapshot().unwrap();
    assert_eq!(snapshot.runs.unwrap()[0].output, "abc");
    assert_eq!(snapshot.tools[0].status, "running");

    e.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &run).terminal()).await;
    h.close().await;
}
#[tokio::test]
async fn sparse_stream_delta_flushes_without_a_following_event() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let thread = cmd(&h.core, "thread.create", json!({})).thread_id;
    let run = cmd(
        &h.core,
        "message.send",
        json!({"threadId":thread,"message":"sparse"}),
    )
    .run_id;
    let e = h.next().await;

    emit(&e, 1, "text_delta", Value::Null, "a");
    emit(&e, 2, "text_delta", Value::Null, "b");
    tokio::time::sleep(Duration::from_millis(20)).await;
    assert_eq!(h.core.snapshot().unwrap().runs.unwrap()[0].output, "ab");

    e.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &run).terminal()).await;
    h.close().await;
}
#[tokio::test]
async fn read_receipts_follow_results_and_ignore_a_stale_device() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let id = cmd(&core, "thread.create", json!({})).thread_id;
    let marker = || core.snapshot().unwrap().threads.unwrap()[0].unread_sequence.unwrap();
    let set = |unread, seen| {
        cmd(&core, "thread.update", json!({"threadId":id,"readState":{"unread":unread,"observedSequence":seen}}));
    };
    assert_eq!(marker(), 0);
    let mut results = Vec::new();
    for _ in 0..2 {
        let r = cmd(&core, "message.send", json!({"threadId":id,"message":"fixture"}));
        h.next().await.done.send(Ok(())).unwrap();
        h.wait(|s| status(s, &r.run_id) == RunStatus::Completed).await;
        results.push(marker());
    }
    assert!(results[1] > results[0] && results[0] > 0);
    set(false, results[0]);
    assert_eq!(marker(), results[1]);
    set(false, results[1]);
    assert_eq!(marker(), 0);
    set(true, 0);
    let explicit = marker();
    assert!(explicit > results[1]);
    h.close().await;
    drop(core);
    let reopened = Core::open(dir.path()).unwrap();
    assert_eq!(reopened.snapshot().unwrap().threads.unwrap()[0].unread_sequence, Some(explicit));
}

#[tokio::test]
async fn image_only_message_reaches_the_provider_without_placeholder_text() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let th = cmd(&h.core, "thread.create", json!({})).thread_id;
    let image = json!({"id":"file_img", "name":"image.png", "mime":"image/png", "size": 8});
    let dir = h.core.directory().join("attachments/file_img");
    std::fs::create_dir_all(dir.join("content")).unwrap();
    std::fs::write(dir.join("metadata.json"), image.to_string()).unwrap();
    std::fs::write(dir.join("content/image.png"), b"\x89PNG\r\n\x1a\n").unwrap();
    cmd(&h.core, "message.send", json!({"threadId": th, "attachments": [image]}));
    let e = h.next().await;
    assert!(e.req.message.is_empty());
    assert_eq!(e.req.attachments.unwrap()[0].id, "file_img");
    e.done.send(Ok(())).unwrap();
    h.close().await;
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
    let mut stale = Command::new("promote-stale", vantry_protocol::CommandKind::PromoteInputToSteer);
    stale.input_id = g.clone();
    stale.thread_id = th.clone();
    stale.run_id = "previous-run".into();
    assert!(core.dispatch(&stale).is_err());
    cmd(&core, "input.promote_steer", json!({"inputId":g,"runId":send.run_id}));
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
        emit(&old, 2, "text_delta", Value::Null, "buffered");
        let att = old.req.attempt_id.clone();
        assert!(
            (old.emit)(ProviderEvent {
                sequence: 4,
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

#[tokio::test]
async fn parallel_auto_children_deliver_incrementally_without_waiting_for_slow_sibling() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let th = cmd(&core, "thread.create", json!({})).thread_id;
    let parent = cmd(
        &core,
        "message.send",
        json!({"threadId": th, "message": "launch two"}),
    )
    .run_id;
    let p = h.next().await;
    let c1 = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "fast child", "message": "fast", "resumePolicy": "auto"}),
    );
    let _c2 = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "slow child", "message": "slow", "resumePolicy": "auto"}),
    );
    let e1 = h.next().await;
    let e2 = h.next().await;
    let (fast, slow) = if e1.req.run_id == c1.run_id {
        (e1, e2)
    } else {
        (e2, e1)
    };
    p.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Waiting).await;

    // Fast child finishes while slow child is STILL running -> parent wakes immediately!
    emit(&fast, 1, "", Value::Null, "fast output");
    fast.done.send(Ok(())).unwrap();
    let wake1 = h.next().await;
    assert_eq!(wake1.req.run_id, parent);
    assert!(wake1.req.message.contains("fast output"));
    assert!(
        wake1.req.message.contains("slow child"),
        "wake prompt should note still-running sibling: {}",
        wake1.req.message
    );
    assert_ne!(wake1.req.attempt_id, p.req.attempt_id);
    wake1.done.send(Ok(())).unwrap();
    // Slow child is still running -> parent returns cleanly to waiting_children.
    h.wait(|s| status(s, &parent) == RunStatus::Waiting).await;

    // Slow child finishes -> parent wakes again with slow child's result!
    emit(&slow, 1, "", Value::Null, "slow output");
    slow.done.send(Ok(())).unwrap();
    let wake2 = h.next().await;
    assert_eq!(wake2.req.run_id, parent);
    assert!(wake2.req.message.contains("slow output"));
    assert!(!wake2.req.message.contains("fast output"));
    wake2.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Completed).await;
    h.close().await;
}

#[tokio::test]
async fn user_message_while_waiting_children_runs_immediately_and_child_wakes_parent_in_new_run() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let th = cmd(&core, "thread.create", json!({})).thread_id;
    let parent = cmd(
        &core,
        "message.send",
        json!({"threadId": th, "message": "delegate one"}),
    )
    .run_id;
    let p = h.next().await;
    let c1 = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "bg child", "message": "bg", "resumePolicy": "auto"}),
    );
    let child = h.next().await;
    p.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Waiting).await;

    // User sends a new message while parent is waiting_children:
    // parent settles to completed, user run starts immediately, and child keeps running.
    let user_msg = cmd(
        &core,
        "message.queue",
        json!({"threadId": th, "message": "side question"}),
    );
    assert!(!user_msg.run_id.is_empty());
    assert_ne!(user_msg.run_id, parent);
    let user_exec = h.next().await;
    assert_eq!(user_exec.req.run_id, user_msg.run_id);
    assert_eq!(user_exec.req.message, "side question");

    // Child completes WHILE the user's new run is actively executing (must not crash or violate unique index!).
    emit(&child, 1, "", Value::Null, "bg child result");
    child.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &c1.run_id) == RunStatus::Completed)
        .await;

    // As soon as the user's run settles, parent automatically wakes in a fresh run at the bottom!
    user_exec.done.send(Ok(())).unwrap();
    let wake = h.next().await;
    assert_eq!(wake.req.thread_id, th);
    assert_ne!(wake.req.run_id, parent);
    assert_ne!(wake.req.run_id, user_msg.run_id);
    assert!(wake.req.message.contains("bg child result"));
    wake.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &wake.req.run_id) == RunStatus::Completed)
        .await;
    h.close().await;
}

#[tokio::test]
async fn failed_continuation_does_not_strand_remaining_sibling_child() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let th = cmd(&core, "thread.create", json!({})).thread_id;
    let parent = cmd(
        &core,
        "message.send",
        json!({"threadId": th, "message": "two siblings"}),
    )
    .run_id;
    let p = h.next().await;
    let c1 = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "first", "message": "one", "resumePolicy": "auto"}),
    );
    let _c2 = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "second", "message": "two", "resumePolicy": "auto"}),
    );
    let e1 = h.next().await;
    let e2 = h.next().await;
    let (first, second) = if e1.req.run_id == c1.run_id {
        (e1, e2)
    } else {
        (e2, e1)
    };
    p.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Waiting).await;

    emit(&first, 1, "", Value::Null, "first done");
    first.done.send(Ok(())).unwrap();
    let wake1 = h.next().await;
    assert!(wake1.req.message.contains("first done"));
    // Transient failure on the first continuation attempt.
    wake1
        .done
        .send(Err(ProviderError::Failed("transient api error".into())))
        .unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Failed).await;

    // Second sibling finishes later -> must NOT be stranded; wakes parent in a fresh run!
    emit(&second, 1, "", Value::Null, "second done");
    second.done.send(Ok(())).unwrap();
    let wake2 = h.next().await;
    assert_eq!(wake2.req.thread_id, th);
    assert_ne!(wake2.req.run_id, parent);
    assert!(wake2.req.message.contains("second done"));
    wake2.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &wake2.req.run_id) == RunStatus::Completed)
        .await;
    h.close().await;
}

#[tokio::test]
async fn mixed_auto_and_manual_delegates_and_archive_guard() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let th = cmd(&core, "thread.create", json!({})).thread_id;
    let parent = cmd(
        &core,
        "message.send",
        json!({"threadId": th, "message": "mixed"}),
    )
    .run_id;
    let p = h.next().await;
    let auto_task = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "message": "auto child", "resumePolicy": "auto"}),
    );
    let manual_task = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "message": "manual child", "resumePolicy": "manual"}),
    );
    assert_ne!(auto_task.generation, manual_task.generation);
    let e1 = h.next().await;
    let e2 = h.next().await;
    let (auto_exec, manual_exec) = if e1.req.run_id == auto_task.run_id {
        (e1, e2)
    } else {
        (e2, e1)
    };
    p.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Waiting).await;

    // Archiving a thread with active delegated children must be refused.
    let archive = Command {
        thread_id: th.clone(),
        archived: Some(true),
        ..Command::new("archive-while-delegated", CommandKind::UpdateThread)
    };
    assert_eq!(core.dispatch(&archive).unwrap_err().code(), ErrorCode::Conflict);

    // Auto child finishes and wakes immediately without waiting for manual child.
    emit(&auto_exec, 1, "", Value::Null, "auto ok");
    auto_exec.done.send(Ok(())).unwrap();
    let wake = h.next().await;
    assert!(wake.req.message.contains("auto ok"));
    wake.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Waiting).await;

    // Manual child completes -> waits in delivered until disposed.
    emit(&manual_exec, 1, "", Value::Null, "manual ok");
    manual_exec.done.send(Ok(())).unwrap();
    h.wait(|s| {
        s.cohorts
            .as_ref()
            .unwrap()
            .iter()
            .any(|c| c.generation == manual_task.generation && c.status == DeliveryStatus::Delivered)
    })
    .await;
    cmd(
        &core,
        "task.dispose",
        json!({"taskId": manual_task.task_id, "generation": manual_task.generation, "deliveryMessageId": manual_task.delivery_message_id}),
    );
    h.wait(|s| status(s, &parent) == RunStatus::Completed).await;

    // Once all delegated work settled, archiving succeeds.
    core.dispatch(&Command {
        thread_id: th,
        archived: Some(true),
        ..Command::new("archive-after-settled", CommandKind::UpdateThread)
    })
    .unwrap();
    h.close().await;
}

#[tokio::test]
async fn siblings_finishing_while_parent_runs_coalesce_into_single_continuation() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let th = cmd(&core, "thread.create", json!({})).thread_id;
    let parent = cmd(
        &core,
        "message.send",
        json!({"threadId": th, "message": "coalesce"}),
    )
    .run_id;
    let p = h.next().await;
    let c1 = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "A", "message": "a", "resumePolicy": "auto"}),
    );
    let c2 = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "B", "message": "b", "resumePolicy": "auto"}),
    );
    let e1 = h.next().await;
    let e2 = h.next().await;
    // Both children complete WHILE parent turn 1 is still running.
    emit(&e1, 1, "", Value::Null, "alpha");
    e1.done.send(Ok(())).unwrap();
    emit(&e2, 1, "", Value::Null, "beta");
    e2.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &c1.run_id) == RunStatus::Completed && status(s, &c2.run_id) == RunStatus::Completed)
        .await;

    // Parent turn 1 ends -> receives BOTH results in a SINGLE coalesced wake!
    p.done.send(Ok(())).unwrap();
    let wake = h.next().await;
    assert_eq!(wake.req.run_id, parent);
    assert!(wake.req.message.contains("alpha"));
    assert!(wake.req.message.contains("beta"));
    wake.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Completed).await;
    h.close().await;
}

#[tokio::test]
async fn grandchild_delegation_keeps_parent_waiting_until_child_synthesizes() {
    let dir = tempfile::tempdir().unwrap();
    let mut h = Harness::open(dir.path(), true);
    let core = h.core.clone();
    let th = cmd(&core, "thread.create", json!({})).thread_id;
    let parent = cmd(
        &core,
        "message.send",
        json!({"threadId": th, "message": "root"}),
    )
    .run_id;
    let p = h.next().await;
    let child_rec = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": parent, "parentAttemptId": p.req.attempt_id, "title": "child", "message": "spawn grandchild", "resumePolicy": "auto"}),
    );
    let child_exec = h.next().await;
    let grandchild_rec = cmd(
        &core,
        "task.delegate",
        json!({"parentRunId": child_rec.run_id, "parentAttemptId": child_exec.req.attempt_id, "title": "grandchild", "message": "leaf", "resumePolicy": "auto"}),
    );
    let grandchild_exec = h.next().await;
    p.done.send(Ok(())).unwrap();
    emit(&child_exec, 1, "", Value::Null, "interim child text");
    child_exec.done.send(Ok(())).unwrap();

    // Child is waiting_children -> parent MUST still be waiting_children (no premature wake!).
    h.wait(|s| {
        status(s, &child_rec.run_id) == RunStatus::Waiting
            && status(s, &parent) == RunStatus::Waiting
    })
    .await;

    // Grandchild finishes -> wakes child -> child emits final synthesized output -> wakes parent!
    emit(&grandchild_exec, 1, "", Value::Null, "leaf answer");
    grandchild_exec.done.send(Ok(())).unwrap();
    let child_wake = h.next().await;
    assert_eq!(child_wake.req.run_id, child_rec.run_id);
    assert!(child_wake.req.message.contains("leaf answer"));
    emit(&child_wake, 1, "", Value::Null, "final synthesized answer");
    child_wake.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &grandchild_rec.run_id) == RunStatus::Completed)
        .await;

    let parent_wake = h.next().await;
    assert_eq!(parent_wake.req.run_id, parent);
    assert!(parent_wake.req.message.contains("final synthesized answer"));
    parent_wake.done.send(Ok(())).unwrap();
    h.wait(|s| status(s, &parent) == RunStatus::Completed).await;
    h.close().await;
}
