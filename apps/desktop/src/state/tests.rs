//! The state machine: event folding, intents and their results, the
//! connection's recovery.

use serde_json::json;

use super::*;
use vantry_protocol::{
    CommandKind, CoreEvent, PendingRequest, Receipt, Run, RunStatus, Snapshot, Thread, ToolCall,
};

use crate::bridge::{BridgeError, ErrorCode};

fn thread(id: &str, created_at: &str) -> Thread {
    Thread {
        id: id.into(),
        title: format!("会话 {id}"),
        created_at: created_at.into(),
        ..Thread::default()
    }
}

fn run(id: &str, thread_id: &str, status: RunStatus) -> Run {
    Run {
        id: id.into(),
        thread_id: thread_id.into(),
        status,
        input: "你好".into(),
        created_at: format!("2026-11-23T00:00:0{}Z", id.len() % 10),
        ..Run::default()
    }
}

fn ev(sequence: i64, kind: &str, thread_id: &str, run_id: &str, data: serde_json::Value) -> CoreEvent {
    CoreEvent {
        sequence,
        id: format!("e{sequence}"),
        r#type: kind.into(),
        thread_id: thread_id.into(),
        run_id: run_id.into(),
        data,
        ..CoreEvent::default()
    }
}

/// A loaded state showing thread `t1` with run `r1` running.
fn live_state() -> AppState {
    let mut state = AppState::default();
    state.apply_snapshot(Snapshot {
        sequence: 10,
        threads: Some(vec![thread("t1", "2026-11-23T00:00:00Z")]),
        runs: Some(vec![run("r1", "t1", RunStatus::Running)]),
        ..Snapshot::default()
    });
    state.on_live();
    state
}

fn approval_request(id: &str, status: &str) -> PendingRequest {
    PendingRequest {
        id: id.into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        method: "select".into(),
        title: "允许执行 bash？\nrm -rf build".into(),
        options: vec!["允许本次".into(), "本会话允许 bash".into(), "拒绝".into()],
        status: status.into(),
        created_at: "2026-11-23T00:00:01Z".into(),
        ..PendingRequest::default()
    }
}

fn request_event(sequence: i64, request: &PendingRequest) -> CoreEvent {
    ev(
        sequence,
        "request.updated",
        "t1",
        "r1",
        json!({ "request": serde_json::to_value(request).unwrap() }),
    )
}

fn accepted(command: &Command) -> Receipt {
    Receipt {
        command_id: command.command_id.clone(),
        accepted: true,
        thread_id: command.thread_id.clone(),
        ..Receipt::default()
    }
}

fn texts(state: &AppState) -> Vec<String> {
    state
        .timeline()
        .iter()
        .map(|item| match item {
            TimelineItem::User { text, .. } => format!("user:{text}"),
            TimelineItem::Reasoning { text, .. } => format!("think:{text}"),
            TimelineItem::Text {
                run_id, start, end, ..
            } => format!("text:{}", state.text_of(run_id, *start, *end)),
            TimelineItem::Tool { name, status, .. } => format!("tool:{name}:{}", status.label()),
            TimelineItem::Approval { view, .. } => format!("approval:{}", view.request_id),
            TimelineItem::Status { text, .. } => format!("status:{text}"),
        })
        .collect()
}

// ------------------------------------------------------------- streaming

#[test]
fn deltas_append_in_order_and_replays_are_ignored() {
    let mut state = live_state();
    let first = ev(11, "run.output", "t1", "r1", json!({ "delta": "你好，" }));
    let second = ev(12, "run.output", "t1", "r1", json!({ "delta": "世界" }));
    assert_eq!(state.apply_event(&first), Applied::Changed);
    assert_eq!(state.apply_event(&second), Applied::Changed);
    // A resubscription replays both: nothing doubles.
    assert_eq!(state.apply_event(&first), Applied::Stale);
    assert_eq!(state.apply_event(&second), Applied::Stale);
    assert_eq!(state.runs[0].output, "你好，世界");
    assert_eq!(state.sequence, 12);
    assert_eq!(state.stats.stale, 2);
}

#[test]
fn bare_text_chunk_and_full_output_shapes_fold() {
    let mut state = live_state();
    state.apply_event(&ev(11, "run.output", "t1", "r1", json!("A")));
    state.apply_event(&ev(12, "run.output", "t1", "r1", json!({ "text": "B" })));
    state.apply_event(&ev(13, "run.output", "t1", "r1", json!({ "chunk": "C" })));
    assert_eq!(state.runs[0].output, "ABC");
    // A full output that the streamed text already starts with is not appended.
    state.apply_event(&ev(14, "run.output", "t1", "r1", json!({ "output": "AB" })));
    assert_eq!(state.runs[0].output, "ABC");
    // One that differs reconciles it.
    state.apply_event(&ev(
        15,
        "run.output",
        "t1",
        "r1",
        json!({ "output": "XYZ" }),
    ));
    assert_eq!(state.runs[0].output, "XYZ");
}

#[test]
fn output_for_an_unknown_run_only_moves_the_cursor() {
    let mut state = live_state();
    let outcome = state.apply_event(&ev(11, "run.output", "t1", "nope", json!({ "delta": "x" })));
    assert_eq!(outcome, Applied::Ignored);
    assert_eq!(state.sequence, 11);
    assert_eq!(state.runs[0].output, "");
}

#[test]
fn reasoning_streams_then_text_takes_over() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.reasoning",
        "t1",
        "r1",
        json!({ "reasoningDelta": "想" }),
    ));
    state.apply_event(&ev(
        12,
        "run.reasoning",
        "t1",
        "r1",
        json!({ "reasoningDelta": "一想" }),
    ));
    let timeline = state.timeline();
    assert!(matches!(
        &timeline[1],
        TimelineItem::Reasoning { text, streaming: true, .. } if &**text == "想一想"
    ));
    state.apply_event(&ev(13, "run.output", "t1", "r1", json!({ "delta": "好" })));
    let timeline = state.timeline();
    assert!(matches!(
        &timeline[1],
        TimelineItem::Reasoning {
            streaming: false,
            ..
        }
    ));
    assert!(matches!(
        &timeline[2],
        TimelineItem::Text {
            streaming: true,
            ..
        }
    ));
}

#[test]
fn run_status_events_merge_without_losing_streamed_text() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "部分回复" }),
    ));
    // run.completed carries the run without output.
    let mut done = run("r1", "t1", RunStatus::Completed);
    done.output.clear();
    state.apply_event(&ev(12, "run.completed", "t1", "r1", json!({ "run": done })));
    assert_eq!(state.runs[0].status, RunStatus::Completed);
    assert_eq!(state.runs[0].output, "部分回复");
    // A late "running" event does not reopen it.
    let late = run("r1", "t1", RunStatus::Running);
    state.apply_event(&ev(13, "run.started", "t1", "r1", json!(late)));
    assert_eq!(state.runs[0].status, RunStatus::Completed);
}

#[test]
fn a_run_event_without_status_takes_the_events() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.queued",
        "t1",
        "r2",
        json!({ "run": { "id": "r2", "input": "下一条" } }),
    ));
    let r2 = state.runs.iter().find(|r| r.id == "r2").unwrap();
    assert_eq!(r2.status, RunStatus::Queued);
    assert_eq!(r2.thread_id, "t1");
}

// ------------------------------------------------------------------ tools

#[test]
fn tools_start_and_finish_in_place_between_text_segments() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "先查看。" }),
    ));
    let offset = "先查看。".len();
    let tool = ToolCall {
        id: "k1".into(),
        run_id: "r1".into(),
        thread_id: "t1".into(),
        name: "bash".into(),
        arguments: Some(json!({ "command": "ls -la\npwd" })),
        status: "running".into(),
        output_offset: offset as i64,
        ..ToolCall::default()
    };
    state.apply_event(&ev(12, "tool.updated", "t1", "r1", json!({ "tool": tool })));
    state.apply_event(&ev(
        13,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "看完了。" }),
    ));
    assert_eq!(
        texts(&state),
        vec![
            "user:你好",
            "text:先查看。",
            "tool:bash:运行中",
            "text:看完了。"
        ]
    );
    let ids: Vec<String> = state.timeline().iter().map(|i| i.id().to_owned()).collect();
    assert_eq!(ids, vec!["user:r1", "text:r1:0", "tool:k1", "text:r1:1"]);

    let mut done = tool.clone();
    done.status = "completed".into();
    done.output = "a\nb".into();
    state.apply_event(&ev(14, "tool.updated", "t1", "r1", json!({ "tool": done })));
    let timeline = state.timeline();
    // Same ids: the list updates the rows in place.
    let after: Vec<String> = timeline.iter().map(|i| i.id().to_owned()).collect();
    assert_eq!(after, ids);
    match &timeline[2] {
        TimelineItem::Tool {
            status,
            summary,
            output,
            ..
        } => {
            assert_eq!(*status, ToolStatus::Completed);
            assert_eq!(summary, "ls -la…");
            assert_eq!(output, "a\nb");
        }
        other => panic!("expected a tool, got {other:?}"),
    }
}

#[test]
fn a_tool_offset_inside_a_character_snaps_to_its_start() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "中文" }),
    ));
    let tool = ToolCall {
        id: "k1".into(),
        run_id: "r1".into(),
        name: "read".into(),
        status: "completed".into(),
        output_offset: 4, // inside "文"
        ..ToolCall::default()
    };
    state.apply_event(&ev(12, "tool.updated", "t1", "r1", json!({ "tool": tool })));
    assert_eq!(
        texts(&state),
        vec!["user:你好", "text:中", "tool:read:已完成", "text:文"]
    );
}

#[test]
fn tools_with_unknown_offsets_follow_the_text() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "回复" }),
    ));
    // An older record without `outputOffset` (and other fields): unknown.
    let tool = json!({ "id": "k1", "runId": "r1", "name": "grep", "status": "failed" });
    state.apply_event(&ev(12, "tool.updated", "t1", "r1", json!({ "tool": tool })));
    assert_eq!(state.tools[0].output_offset, -1);
    assert_eq!(
        texts(&state),
        vec!["user:你好", "text:回复", "tool:grep:失败"]
    );
}

// --------------------------------------------------------------- approvals

#[test]
fn an_approval_request_queues_and_shows_in_the_modal() {
    let mut state = live_state();
    let tool = ToolCall {
        id: "k1".into(),
        run_id: "r1".into(),
        name: "bash".into(),
        status: "running".into(),
        output_offset: 0,
        ..ToolCall::default()
    };
    state.apply_event(&ev(11, "tool.updated", "t1", "r1", json!({ "tool": tool })));
    state.apply_event(&request_event(12, &approval_request("q1", "pending")));
    assert_eq!(state.thread_status("t1"), ThreadStatus::NeedsApproval);
    assert_eq!(state.approval_queue().len(), 1);
    let view = state.modal_request().unwrap();
    assert_eq!(view.request_id, "q1");
    assert_eq!(view.headline, "允许执行 bash？");
    assert_eq!(view.body, "rm -rf build");
    assert_eq!(view.risk, Risk::High);
    assert!(view.is_approval());
    assert_eq!(view.allow_session.as_deref(), Some("本会话允许 bash"));
    assert_eq!(
        texts(&state),
        vec!["user:你好", "tool:bash:等待确认", "approval:q1"]
    );
}

#[test]
fn allowing_sends_the_option_and_the_core_closes_the_request() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    let command = state.respond("q1", &ApprovalDecision::Allow).unwrap();
    assert_eq!(command.kind, CommandKind::RespondRequest);
    assert_eq!(command.request_id, "q1");
    assert_eq!(command.run_id, "r1");
    assert_eq!(command.value, "允许本次");
    assert!(!command.confirmed && !command.cancelled);
    // Responding: no second answer, the modal keeps it until the Core speaks.
    assert_eq!(state.requests[0].status, "responding");
    assert!(state.in_flight("request:q1"));
    assert_eq!(
        state.respond("q1", &ApprovalDecision::Deny),
        Err(DecisionError::NotPending)
    );
    assert!(
        state
            .on_command_result(&command.command_id, Ok(accepted(&command)))
            .is_none()
    );
    state.apply_event(&request_event(12, &approval_request("q1", "responded")));
    assert!(state.modal_request().is_none());
    assert!(state.pending.is_empty());
    assert_eq!(state.thread_status("t1"), ThreadStatus::Running);
}

#[test]
fn denying_and_allowing_for_the_session_send_their_options() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    let deny = state.respond("q1", &ApprovalDecision::Deny).unwrap();
    assert_eq!(deny.value, "拒绝");

    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    let session = state
        .respond("q1", &ApprovalDecision::AllowSession)
        .unwrap();
    assert_eq!(session.value, "本会话允许 bash");
}

#[test]
fn confirm_select_and_input_requests_answer_their_own_way() {
    let confirm = PendingRequest {
        method: "confirm".into(),
        ..approval_request("q1", "pending")
    };
    let allow = approval::respond_command(&confirm, &ApprovalDecision::Allow).unwrap();
    assert!(allow.confirmed && !allow.cancelled && allow.value.is_empty());
    let deny = approval::respond_command(&confirm, &ApprovalDecision::Deny).unwrap();
    assert!(!deny.confirmed && !deny.cancelled);

    let select = PendingRequest {
        options: vec!["A".into(), "B".into()],
        ..approval_request("q2", "pending")
    };
    assert!(!approval_view(&select).is_approval());
    let chosen = approval::respond_command(&select, &ApprovalDecision::Choose("B".into())).unwrap();
    assert_eq!(chosen.value, "B");
    assert_eq!(
        approval::respond_command(&select, &ApprovalDecision::Choose("C".into())),
        Err(DecisionError::Invalid("请选择请求提供的选项"))
    );
    let cancelled = approval::respond_command(&select, &ApprovalDecision::Cancel).unwrap();
    assert!(cancelled.cancelled);

    let input = PendingRequest {
        method: "input".into(),
        ..approval_request("q3", "pending")
    };
    let typed = approval::respond_command(&input, &ApprovalDecision::Submit("值".into())).unwrap();
    assert_eq!(typed.value, "值");
    assert_eq!(
        approval::respond_command(&input, &ApprovalDecision::Allow),
        Err(DecisionError::Invalid("该请求不支持此操作"))
    );

    let answered = approval_request("q4", "responded");
    assert_eq!(
        approval::respond_command(&answered, &ApprovalDecision::Allow),
        Err(DecisionError::NotPending)
    );
}

#[test]
fn a_rejected_answer_reopens_the_request_and_an_unknown_one_retries_the_same_command() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    let command = state.respond("q1", &ApprovalDecision::Allow).unwrap();
    state.on_command_result(
        &command.command_id,
        Err(BridgeError::new(ErrorCode::Conflict, "request expired")),
    );
    assert_eq!(state.requests[0].status, "pending");
    assert!(state.pending.is_empty());
    assert_eq!(state.notice.as_ref().unwrap().level, NoticeLevel::Error);

    let command = state.respond("q1", &ApprovalDecision::Deny).unwrap();
    state.on_command_result(
        &command.command_id,
        Err(BridgeError::unavailable("socket closed")),
    );
    // Outcome unknown: still responding, a retry resends the very same command.
    assert_eq!(state.requests[0].status, "responding");
    assert_eq!(state.notice.as_ref().unwrap().level, NoticeLevel::Warn);
    let again = state.retry("request:q1").unwrap();
    assert_eq!(again, command);
    assert!(state.retry("request:q1").is_none(), "in flight again");
}

#[test]
fn the_modal_prefers_the_shown_threads_request() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "thread.created",
        "t2",
        "",
        json!({ "thread": thread("t2", "2026-11-23T01:00:00Z") }),
    ));
    let other = PendingRequest {
        thread_id: "t2".into(),
        created_at: "2026-11-23T00:00:00Z".into(),
        ..approval_request("q-other", "pending")
    };
    state.apply_event(&request_event(12, &other));
    state.apply_event(&request_event(13, &approval_request("q-mine", "pending")));
    state.select_thread(Some("t1".into()));
    assert_eq!(state.modal_request().unwrap().request_id, "q-mine");
    state.select_thread(Some("t2".into()));
    assert_eq!(state.modal_request().unwrap().request_id, "q-other");
}

#[test]
fn a_request_put_aside_leaves_the_modal_but_stays_queued() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    let second = PendingRequest {
        created_at: "2026-11-23T00:00:02Z".into(),
        ..approval_request("q2", "pending")
    };
    state.apply_event(&request_event(12, &second));
    assert_eq!(state.modal_request().unwrap().request_id, "q1");
    state.defer_request("q1");
    assert_eq!(state.modal_request().unwrap().request_id, "q2");
    state.defer_request("q2");
    assert!(state.modal_request().is_none());
    assert_eq!(state.approval_queue().len(), 2);
    state.reopen_request("q2");
    assert_eq!(state.modal_request().unwrap().request_id, "q2");
    // Once the Core settles it, the aside mark goes too.
    state.apply_event(&request_event(13, &approval_request("q1", "expired")));
    assert!(!state.deferred.contains("q1"));
}

// ------------------------------------------------------------------ sending

#[test]
fn the_first_message_creates_the_thread_then_sends() {
    let mut state = AppState::default();
    state.apply_snapshot(Snapshot::default());
    state.on_live();
    assert!(state.selected.is_none());
    state.set_draft_text("  帮我看看这个仓库\n第二行  ");
    state.toggle_mode();
    let create = state.submit().unwrap();
    assert_eq!(create.kind, CommandKind::CreateThread);
    assert_eq!(create.title, "帮我看看这个仓库");
    assert_eq!(create.interaction_mode, "plan");
    assert!(state.draft().text.is_empty());
    assert!(state.composer_status().sending);
    assert!(state.submit().is_none(), "one submission at a time");

    let receipt = Receipt {
        thread_id: "t9".into(),
        ..accepted(&create)
    };
    let send = state
        .on_command_result(&create.command_id, Ok(receipt))
        .unwrap();
    assert_eq!(send.kind, CommandKind::SendMessage);
    assert_eq!(send.thread_id, "t9");
    assert_eq!(send.message, "帮我看看这个仓库\n第二行");
    assert_eq!(send.interaction_mode, "plan");
    assert_eq!(state.selected.as_deref(), Some("t9"));
    assert_eq!(state.draft().mode, InteractionMode::Plan);
    assert!(
        state
            .on_command_result(&send.command_id, Ok(accepted(&send)))
            .is_none()
    );
    assert!(state.pending.is_empty());
}

#[test]
fn a_rejected_send_restores_the_draft() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.completed",
        "t1",
        "r1",
        json!({ "run": run("r1", "t1", RunStatus::Completed) }),
    ));
    state.set_draft_text("再来一次");
    let send = state.submit().unwrap();
    assert_eq!(send.kind, CommandKind::SendMessage);
    state.on_command_result(
        &send.command_id,
        Err(BridgeError::new(ErrorCode::InvalidCommand, "bad")),
    );
    assert_eq!(state.draft().text, "再来一次");
    assert!(state.notice.as_ref().unwrap().text.contains("发送失败"));
}

#[test]
fn an_unconfirmed_send_offers_a_retry_and_abandoning_restores_the_draft() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "run.completed",
        "t1",
        "r1",
        json!({ "run": run("r1", "t1", RunStatus::Completed) }),
    ));
    state.set_draft_text("可能已送达");
    let send = state.submit().unwrap();
    state.on_command_result(&send.command_id, Err(BridgeError::unavailable("eof")));
    let status = state.composer_status();
    assert_eq!(status.retry.as_deref(), Some("submit:t1"));
    assert!(!status.can_submit);
    // The user types meanwhile; abandoning keeps the new text.
    state.set_draft_text("新草稿");
    state.abandon("submit:t1");
    assert_eq!(state.draft().text, "新草稿");
    assert!(state.pending.is_empty());
}

#[test]
fn while_running_the_draft_steers_or_queues() {
    let mut state = live_state();
    assert_eq!(state.composer_status().primary, PrimaryAction::Steer);
    state.set_draft_text("改成中文");
    let steer = state.submit().unwrap();
    assert_eq!(steer.kind, CommandKind::SteerMessage);
    assert_eq!(steer.run_id, "r1");
    state.on_command_result(&steer.command_id, Ok(accepted(&steer)));

    state.set_running_action(RunningAction::Queue);
    assert_eq!(state.composer_status().primary, PrimaryAction::Queue);
    state.set_draft_text("之后再做");
    let queue = state.submit().unwrap();
    assert_eq!(queue.kind, CommandKind::QueueMessage);
    assert!(queue.run_id.is_empty());
    state.on_command_result(&queue.command_id, Ok(accepted(&queue)));
    assert_eq!(state.notice.as_ref().unwrap().text, "已加入队列。");
}

#[test]
fn stop_targets_the_active_run_once() {
    let mut state = live_state();
    assert!(state.composer_status().can_stop);
    let stop = state.stop().unwrap();
    assert_eq!(stop.kind, CommandKind::StopRun);
    assert_eq!(stop.run_id, "r1");
    assert!(state.stop().is_none(), "already stopping");
    assert!(!state.composer_status().can_stop);
    state.on_command_result(&stop.command_id, Ok(accepted(&stop)));
    state.apply_event(&ev(
        11,
        "run.cancelled",
        "t1",
        "r1",
        json!({ "run": run("r1", "t1", RunStatus::Cancelled) }),
    ));
    assert!(state.stop().is_none());
    assert_eq!(texts(&state).last().unwrap(), "status:已停止");
}

#[test]
fn drafts_are_per_thread() {
    let mut state = live_state();
    state.set_draft_text("t1 的草稿");
    state.new_thread();
    assert!(state.draft().text.is_empty());
    state.set_draft_text("新会话草稿");
    state.select_thread(Some("t1".into()));
    assert_eq!(state.draft().text, "t1 的草稿");
    state.new_thread();
    assert_eq!(state.draft().text, "新会话草稿");
}

// ------------------------------------------------------------- connection

#[test]
fn a_broken_stream_resubscribes_after_the_cursor_with_backoff() {
    let mut state = live_state();
    state.apply_event(&ev(11, "run.output", "t1", "r1", json!({ "delta": "x" })));
    let first = state.on_stream_ended(Err(BridgeError::unavailable("reset")));
    assert_eq!(
        first,
        Recovery::Resubscribe {
            after: 11,
            delay: Duration::from_millis(250)
        }
    );
    assert!(matches!(
        state.connection,
        Connection::Reconnecting { attempt: 1, .. }
    ));
    let second = state.on_stream_ended(Ok(()));
    assert_eq!(
        second,
        Recovery::Resubscribe {
            after: 11,
            delay: Duration::from_millis(500)
        }
    );
    state.on_live();
    assert_eq!(state.connection, Connection::Live);
    assert!(state.notice.as_ref().unwrap().text.contains("不会自动重发"));
}

#[test]
fn resync_required_reloads_the_snapshot_and_keeps_local_intents() {
    let mut state = live_state();
    state.set_draft_text("别丢");
    let recovery = state.on_stream_ended(Err(BridgeError::new(
        ErrorCode::ResyncRequired,
        "slow consumer",
    )));
    assert_eq!(
        recovery,
        Recovery::Resync {
            delay: Duration::from_millis(250)
        }
    );
    assert!(matches!(
        state.connection,
        Connection::Resyncing { attempt: 1, .. }
    ));
    assert!(state.notice.as_ref().unwrap().text.contains("重新同步"));
    let mut done = run("r1", "t1", RunStatus::Completed);
    done.output = "完整回复".into();
    state.apply_snapshot(Snapshot {
        sequence: 40,
        threads: Some(vec![thread("t1", "2026-11-23T00:00:00Z")]),
        runs: Some(vec![done]),
        ..Snapshot::default()
    });
    state.on_live();
    assert_eq!(state.sequence, 40);
    assert_eq!(state.runs[0].output, "完整回复");
    assert_eq!(state.draft().text, "别丢");
    assert_eq!(state.selected.as_deref(), Some("t1"));
    // Events from before the snapshot are stale now.
    assert_eq!(
        state.apply_event(&ev(39, "run.output", "t1", "r1", json!({ "delta": "x" }))),
        Applied::Stale
    );
}

#[test]
fn a_snapshot_without_the_selected_thread_selects_the_newest() {
    let mut state = live_state();
    state.select_thread(Some("t1".into()));
    state.apply_snapshot(Snapshot {
        sequence: 50,
        threads: Some(vec![
            thread("a", "2026-11-20T00:00:00Z"),
            thread("b", "2026-11-22T00:00:00Z"),
        ]),
        ..Snapshot::default()
    });
    assert_eq!(state.selected.as_deref(), Some("b"));
}

#[test]
fn reconnect_delay_doubles_up_to_its_cap() {
    let delays: Vec<u64> = (1..=8)
        .map(|a| reconnect_delay(a).as_millis() as u64)
        .collect();
    assert_eq!(delays, vec![250, 500, 1000, 2000, 4000, 8000, 8000, 8000]);
}

#[test]
fn deleting_a_thread_drops_its_entities_and_selection() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    state.apply_event(&ev(
        12,
        "thread.deleted",
        "t1",
        "",
        json!({ "threadId": "t1" }),
    ));
    assert!(state.threads.is_empty());
    assert!(state.runs.is_empty());
    assert!(state.requests.is_empty());
    assert!(state.selected.is_none());
}

#[test]
fn sidebar_orders_pinned_then_newest_and_hides_archived() {
    let mut state = AppState::default();
    let mut pinned = thread("old-pinned", "2026-01-01T00:00:00Z");
    pinned.pinned = true;
    let mut archived = thread("archived", "2026-12-01T00:00:00Z");
    archived.archived = true;
    state.apply_snapshot(Snapshot {
        sequence: 1,
        threads: Some(vec![
            thread("older", "2026-11-01T00:00:00Z"),
            pinned,
            archived,
            thread("newer", "2026-11-20T00:00:00Z"),
        ]),
        ..Snapshot::default()
    });
    let ids: Vec<&str> = state
        .visible_threads()
        .iter()
        .map(|t| t.id.as_str())
        .collect();
    assert_eq!(ids, vec!["old-pinned", "newer", "older"]);
}

#[test]
fn titles_come_from_the_first_line() {
    assert_eq!(title_from_message("  \n"), "新会话");
    assert_eq!(title_from_message("修复登录\n细节"), "修复登录");
    let long = "长".repeat(50);
    assert_eq!(title_from_message(&long).chars().count(), 41);
}
