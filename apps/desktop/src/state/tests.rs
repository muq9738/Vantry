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
            TimelineItem::Steer {
                text, delivered, ..
            } => format!("steer:{}:{text}", if *delivered { "ok" } else { "err" }),
            TimelineItem::Reasoning { text, .. } => format!("think:{text}"),
            TimelineItem::Text { text, .. } => format!("text:{text}"),
            TimelineItem::Tools { calls, .. } => calls
                .iter()
                .map(|c| format!("tool:{}:{}", c.name, c.status.label()))
                .collect::<Vec<_>>()
                .join("|"),
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
        TimelineItem::Tools { calls, .. } => {
            assert_eq!(calls.len(), 1);
            assert_eq!(calls[0].status, ToolStatus::Completed);
            assert_eq!(calls[0].summary, "ls -la…");
            assert_eq!(calls[0].output, "a\nb");
        }
        other => panic!("expected a tool group, got {other:?}"),
    }
}

#[test]
fn consecutive_tools_share_one_group_whose_id_stays_its_first_call() {
    let mut state = live_state();
    state.apply_event(&ev(11, "run.output", "t1", "r1", json!({ "delta": "先查看。" })));
    let at = "先查看。".len() as i64;
    let call = |id: &str, name: &str, offset: i64| ToolCall {
        id: id.into(),
        run_id: "r1".into(),
        thread_id: "t1".into(),
        name: name.into(),
        status: "completed".into(),
        output_offset: offset,
        ..ToolCall::default()
    };
    state.apply_event(&ev(12, "tool.updated", "t1", "r1", json!({ "tool": call("k1", "bash", at) })));
    let before: Vec<String> = state.timeline().iter().map(|i| i.id().to_owned()).collect();
    state.apply_event(&ev(13, "tool.updated", "t1", "r1", json!({ "tool": call("k2", "read", at) })));
    // The second call joins the first one's row: the list keeps its keys.
    let after: Vec<String> = state.timeline().iter().map(|i| i.id().to_owned()).collect();
    assert_eq!(before, after);
    assert_eq!(
        texts(&state),
        vec!["user:你好", "text:先查看。", "tool:bash:已完成|tool:read:已完成"]
    );
    // Reply text between calls starts a new group.
    state.apply_event(&ev(14, "run.output", "t1", "r1", json!({ "delta": "再试。" })));
    let later = ("先查看。".len() + "再试。".len()) as i64;
    state.apply_event(&ev(15, "tool.updated", "t1", "r1", json!({ "tool": call("k3", "grep", later) })));
    assert_eq!(
        texts(&state),
        vec![
            "user:你好",
            "text:先查看。",
            "tool:bash:已完成|tool:read:已完成",
            "text:再试。",
            "tool:grep:已完成"
        ]
    );
    assert_eq!(ToolKind::of("bash"), ToolKind::Command);
    assert_eq!(ToolKind::of("read"), ToolKind::Read);
    assert_eq!(ToolKind::of("mcp__magpie__search"), ToolKind::Other);
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
fn an_approval_request_queues_and_shows_only_in_the_timeline() {
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
    assert!(state.modal_request().is_none());
    let rows = state.timeline();
    let Some(TimelineItem::Approval { view, .. }) = rows.last() else {
        panic!("missing inline approval");
    };
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
fn an_inline_approval_waits_in_the_background_and_leaves_forms_accessible() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    state.select_thread(None);
    assert!(state.modal_request().is_none());
    assert_eq!(state.thread_status("t1"), ThreadStatus::NeedsApproval);

    state.select_thread(Some("t1".into()));
    let form = PendingRequest {
        method: "input".into(),
        title: "请输入备注".into(),
        ..approval_request("form", "pending")
    };
    state.apply_event(&request_event(12, &form));
    assert_eq!(state.modal_request().unwrap().request_id, "form");
    state.defer_request("form");
    assert!(state.modal_request().is_none());
    assert_eq!(state.approval_queue().len(), 2);
}

#[test]
fn an_approval_without_a_loaded_run_can_still_be_answered() {
    let mut state = live_state();
    let request = PendingRequest {
        run_id: "loading".into(),
        ..approval_request("q1", "pending")
    };
    state.apply_event(&request_event(11, &request));
    assert_eq!(state.modal_request().unwrap().request_id, "q1");
}

#[test]
fn bash_approval_displays_the_command_and_preserves_additional_arguments() {
    let command = "ls -la; rg -i \"qmt\" .\nls -lh";
    let mut req = approval_request("q1", "pending");
    req.title = format!("允许执行 bash？\n{}", json!({"command": command}));
    assert_eq!(approval_view(&req).body, command);
    let args = json!({"command": command, "timeout": 60}).to_string();
    req.title = format!("允许执行 bash？\n{args}");
    let view = approval_view(&req);
    assert_eq!(view.body, command);
    assert_eq!(view.details, "timeout: 60");
}

#[test]
fn approval_text_shows_what_runs_including_its_end_and_undrawn_characters() {
    // The approval extension cut a long summary mid-string: the command still reads as one.
    let mut req = approval_request("q1", "pending");
    req.title = "允许执行 bash？\n{\"command\":\"echo hi\\nrm -rf /tmp/x\\u00".into();
    let view = approval_view(&req);
    assert_eq!(view.body, "echo hi\nrm -rf /tmp/x");
    assert!(view.details.contains("开头"));
    // A zero-width space and a bidi override are named, not hidden.
    let text = approval::approval_text("ls\u{200B}x\u{202E}y", 100, 10);
    assert_eq!(text, "ls⟨U+200B⟩x⟨U+202E⟩y");
    // Too long: the beginning and the end, and how much is left out between.
    let long = format!("{}{}", "a".repeat(50), "TAIL");
    let cut = approval::approval_text(&long, 20, 4);
    assert!(cut.starts_with(&"a".repeat(16)));
    assert!(cut.ends_with("TAIL"));
    assert!(cut.contains("中间省略 34 个字符"));
    // Risk reads the whole command, not only what is shown.
    req.title = format!(
        "允许执行 bash？\n{}",
        json!({"command": format!("{} && rm -rf ~", "echo ok; ".repeat(3000))})
    );
    let view = approval_view(&req);
    assert_eq!(view.risk, Risk::High);
    assert!(view.body.ends_with("&& rm -rf ~"));
}

#[test]
fn an_answer_refused_because_another_client_answered_first_settles_the_card() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    let command = state.respond("q1", &ApprovalDecision::Allow).unwrap();
    // The Core takes one answer per request: this one lost the race.
    state.on_command_result(
        &command.command_id,
        Err(BridgeError::new(ErrorCode::Conflict, "request is no longer pending")),
    );
    assert_eq!(state.requests[0].status, crate::state::SETTLED_ELSEWHERE);
    assert!(state.pending.is_empty());
    assert_eq!(state.notice.as_ref().unwrap().level, NoticeLevel::Info);
    assert!(state.respond("q1", &ApprovalDecision::Deny).is_err(), "not answerable again");
    assert_eq!(state.thread_status("t1"), ThreadStatus::Running);
    let rows = state.timeline();
    assert!(rows.iter().any(|row| matches!(row, TimelineItem::Approval { view, .. }
        if view.status == crate::state::SETTLED_ELSEWHERE && !view.answerable())));
    // The Core's word replaces the local guess.
    state.apply_event(&request_event(12, &approval_request("q1", "responded")));
    assert!(state.timeline().iter().all(|row| !matches!(row, TimelineItem::Approval { .. })));
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
    // Keep the pending answer on its original surface until Core settles it.
    assert!(state.modal_request().is_none());
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
        Err(BridgeError::new(ErrorCode::InvalidCommand, "value not in options")),
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
    let form = |id| PendingRequest {
        method: "input".into(),
        ..approval_request(id, "pending")
    };
    let other = PendingRequest {
        thread_id: "t2".into(),
        run_id: "r2".into(),
        created_at: "2026-11-23T00:00:00Z".into(),
        ..form("q-other")
    };
    state.apply_event(&request_event(12, &other));
    state.apply_event(&request_event(13, &form("q-mine")));
    state.select_thread(Some("t1".into()));
    assert_eq!(state.modal_request().unwrap().request_id, "q-mine");
    state.select_thread(Some("t2".into()));
    assert_eq!(state.modal_request().unwrap().request_id, "q-other");
}

#[test]
fn a_request_put_aside_leaves_the_modal_but_stays_queued() {
    let mut state = live_state();
    let first = PendingRequest {
        method: "editor".into(),
        ..approval_request("q1", "pending")
    };
    state.apply_event(&request_event(11, &first));
    let second = PendingRequest {
        id: "q2".into(),
        created_at: "2026-11-23T00:00:02Z".into(),
        ..first
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
    let create = state.submit("fixture/model").unwrap();
    assert_eq!(create.kind, CommandKind::CreateThread);
    assert!(create.title.is_empty(), "Pi generates the title from the first task");
    assert_eq!(create.interaction_mode, "plan");
    assert!(state.draft().text.is_empty());
    assert!(state.composer_status().sending);
    assert!(state.submit("fixture/model").is_none(), "one submission at a time");

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
    let send = state.submit("fixture/model").unwrap();
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
    let send = state.submit("fixture/model").unwrap();
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
fn running_mode_picker_tracks_whether_the_run_accepts_steering() {
    let mut state = live_state();
    assert!(state.composer_status().can_choose_running_action);
    state.runs[0].status = RunStatus::Waiting;
    assert!(!state.composer_status().can_choose_running_action);
    assert_eq!(state.composer_status().primary, PrimaryAction::Queue);
    state.runs[0].status = RunStatus::Completed;
    assert!(!state.composer_status().can_choose_running_action);
    assert_eq!(state.composer_status().primary, PrimaryAction::Send);
}

#[test]
fn while_running_the_draft_steers_or_queues() {
    let mut state = live_state();
    assert_eq!(state.composer_status().primary, PrimaryAction::Steer);
    state.set_draft_text("改成中文");
    let steer = state.submit("fixture/model").unwrap();
    assert_eq!(steer.kind, CommandKind::SteerMessage);
    assert_eq!(steer.run_id, "r1");
    state.on_command_result(&steer.command_id, Ok(accepted(&steer)));

    state.set_running_action(RunningAction::Queue);
    assert_eq!(state.composer_status().primary, PrimaryAction::Queue);
    state.set_draft_text("之后再做");
    let queue = state.submit("fixture/model").unwrap();
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

#[test]
fn decoupled_run_blocks_preserve_causal_order_multi_reasoning_and_mid_run_steer() {
    let mut state = AppState::default();
    state.apply_snapshot(Snapshot {
        sequence: 10,
        threads: Some(vec![thread("t1", "2026-11-23T00:00:00Z")]),
        ..Snapshot::default()
    });
    state.on_live();

    // 1. Run queued with initial User block (seq=1).
    let r1 = run("r1", "t1", RunStatus::Queued);
    let user_block = RunBlock {
        id: "user:r1".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        seq: 1,
        kind: RunBlockKind::User,
        text: "分析代码并修复 bug".into(),
        status: "completed".into(),
        ..RunBlock::default()
    };
    state.apply_event(&ev(
        11,
        "run.queued",
        "t1",
        "r1",
        json!({ "run": r1, "block": user_block }),
    ));
    let mut r1_running = r1.clone();
    r1_running.status = RunStatus::Running;
    state.apply_event(&ev(12, "run.started", "t1", "r1", json!({ "run": r1_running })));

    // 2. First reasoning segment (seq=2), streaming fast path on second delta.
    state.apply_event(&ev(
        13,
        "run.reasoning",
        "t1",
        "r1",
        json!({ "reasoningDelta": "先看", "blockId": "reasoning:r1:2", "seq": 2 }),
    ));
    let rebuilds_after_r1 = state.timeline.rebuilds;
    let structure_after_r1 = state.timeline.structure("t1");
    state.apply_event(&ev(
        14,
        "run.reasoning",
        "t1",
        "r1",
        json!({ "reasoningDelta": "架构。", "blockId": "reasoning:r1:2", "seq": 2 }),
    ));
    assert_eq!(state.timeline.rebuilds, rebuilds_after_r1, "reasoning delta uses O(1) fast path");
    assert_eq!(state.timeline.structure("t1"), structure_after_r1);

    // 3. First text segment (seq=3), streaming fast path on second delta.
    state.apply_event(&ev(
        15,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "我先", "blockId": "text:r1:3", "seq": 3 }),
    ));
    let rebuilds_after_t1 = state.timeline.rebuilds;
    let structure_after_t1 = state.timeline.structure("t1");
    state.apply_event(&ev(
        16,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "查一下。", "blockId": "text:r1:3", "seq": 3 }),
    ));
    assert_eq!(state.timeline.rebuilds, rebuilds_after_t1, "text delta uses O(1) fast path");
    assert_eq!(state.timeline.structure("t1"), structure_after_t1);

    // 4. Two consecutive tool calls (seq=4, seq=5) with output_offset intentionally stale (-1).
    let tool1 = ToolCall {
        id: "k1".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        name: "read".into(),
        status: "completed".into(),
        output_offset: -1,
        ..ToolCall::default()
    };
    let tool1_block = RunBlock {
        id: "tool:k1".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        seq: 4,
        kind: RunBlockKind::Tool,
        ref_id: "k1".into(),
        status: "completed".into(),
        ..RunBlock::default()
    };
    state.apply_event(&ev(
        17,
        "tool.updated",
        "t1",
        "r1",
        json!({ "tool": tool1, "block": tool1_block }),
    ));
    let tool2 = ToolCall {
        id: "k2".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        name: "grep".into(),
        status: "running".into(),
        output_offset: -1,
        ..ToolCall::default()
    };
    let tool2_block = RunBlock {
        id: "tool:k2".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        seq: 5,
        kind: RunBlockKind::Tool,
        ref_id: "k2".into(),
        status: "running".into(),
        ..RunBlock::default()
    };
    state.apply_event(&ev(
        18,
        "tool.updated",
        "t1",
        "r1",
        json!({ "tool": tool2, "block": tool2_block }),
    ));

    // 5. Mid-run steer delivered (seq=6) -> lands BELOW existing tools!
    let steer_input = Input {
        id: "inp-1".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        kind: "steer".into(),
        message: "只改 core，不动测试".into(),
        status: "delivered".into(),
        ..Input::default()
    };
    let steer_block = RunBlock {
        id: "steer:inp-1".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        seq: 6,
        kind: RunBlockKind::Steer,
        ref_id: "inp-1".into(),
        text: "只改 core，不动测试".into(),
        status: "delivered".into(),
        ..RunBlock::default()
    };
    state.apply_event(&ev(
        19,
        "input.updated",
        "t1",
        "r1",
        json!({ "input": steer_input, "block": steer_block }),
    ));

    // Tool 2 finishes AFTER steer arrived -> updates in place at seq=5 (stays ABOVE steer!).
    let mut tool2_done = tool2.clone();
    tool2_done.status = "completed".into();
    let mut tool2_block_done = tool2_block.clone();
    tool2_block_done.status = "completed".into();
    state.apply_event(&ev(
        20,
        "tool.updated",
        "t1",
        "r1",
        json!({ "tool": tool2_done, "block": tool2_block_done }),
    ));

    // 6. Second reasoning segment (seq=7) -> independent block AND streaming=true even though run.output is non-empty!
    state.apply_event(&ev(
        21,
        "run.reasoning",
        "t1",
        "r1",
        json!({ "reasoningDelta": "根据引导聚焦 core。", "blockId": "reasoning:r1:7", "seq": 7 }),
    ));
    let mid = state.timeline();
    assert!(
        matches!(&mid[1], TimelineItem::Reasoning { text, streaming: false, .. } if text.as_str() == "先看架构。"),
        "first reasoning must be closed: {:?}",
        mid[1]
    );
    assert!(
        matches!(&mid[5], TimelineItem::Reasoning { text, streaming: true, .. } if text.as_str() == "根据引导聚焦 core。"),
        "second reasoning must stream independently of run.output: {:?}",
        mid[5]
    );

    // 7. Second text segment (seq=8) + run completion.
    state.apply_event(&ev(
        22,
        "run.output",
        "t1",
        "r1",
        json!({ "delta": "好的，已只修改 core。", "blockId": "text:r1:8", "seq": 8 }),
    ));
    let mut r1_done = r1_running.clone();
    r1_done.status = RunStatus::Completed;
    state.apply_event(&ev(23, "run.completed", "t1", "r1", json!({ "run": r1_done })));

    assert_eq!(
        texts(&state),
        vec![
            "user:分析代码并修复 bug",
            "think:先看架构。",
            "text:我先查一下。",
            "tool:read:已完成|tool:grep:已完成",
            "steer:ok:只改 core，不动测试",
            "think:根据引导聚焦 core。",
            "text:好的，已只修改 core。",
        ]
    );
    let final_rows = state.timeline();
    assert!(matches!(&final_rows[2], TimelineItem::Text { is_last: false, streaming: false, .. }));
    assert!(matches!(&final_rows[6], TimelineItem::Text { is_last: true, streaming: false, .. }));
    assert_eq!(state.reply_text("r1"), "我先查一下。\n\n好的，已只修改 core。");

    // 8. Live-folded timeline matches a cold snapshot rebuild 1:1.
    let mut cold = AppState::default();
    cold.apply_snapshot(Snapshot {
        sequence: state.sequence,
        threads: Some(state.threads.clone()),
        runs: Some(state.runs.clone()),
        tools: state.tools.clone(),
        requests: state.requests.clone(),
        inputs: state.inputs.clone(),
        blocks: state.blocks.clone(),
        ..Snapshot::default()
    });
    assert_eq!(state.timeline(), cold.timeline());
}

// ------------------------------------------------- one entry, any client

#[test]
fn another_threads_request_never_covers_the_screen_and_is_pointed_to() {
    let mut state = live_state();
    state.apply_event(&ev(
        11,
        "thread.created",
        "t2",
        "",
        json!({ "thread": thread("t2", "2026-11-23T01:00:00Z") }),
    ));
    state.select_thread(Some("t1".into()));
    let secret = PendingRequest {
        thread_id: "t2".into(),
        run_id: "r2".into(),
        method: "input".into(),
        title: "请输入 API 密码".into(),
        ..approval_request("q-secret", "pending")
    };
    state.apply_event(&request_event(12, &secret));
    let approval = PendingRequest {
        id: "q-bash".into(),
        created_at: "2026-11-23T00:00:05Z".into(),
        ..approval_request("q-bash", "pending")
    };
    let approval = PendingRequest { thread_id: "t2".into(), run_id: "r2".into(), ..approval };
    state.apply_event(&request_event(13, &approval));
    assert!(state.modal_request().is_none(), "the thread being read stays readable");
    let attention = state.background_attention();
    assert_eq!(attention.len(), 1);
    assert_eq!(attention[0].thread_id, "t2");
    assert_eq!(attention[0].count, 2);
    assert_eq!(attention[0].kind, RequestKind::Secret);
    assert_eq!(attention[0].title, "会话 t2");

    // Opening it from its card shows it, whichever thread is shown; Escape puts it back.
    state.reopen_request("q-secret");
    assert_eq!(state.modal_request().unwrap().request_id, "q-secret");
    state.defer_request("q-secret");
    assert!(state.modal_request().is_none());

    // Shown: its form opens the modal, its approval stays on its card.
    state.select_thread(Some("t2".into()));
    assert!(state.background_attention().is_empty());
    state.reopen_request("q-secret");
    assert_eq!(state.modal_request().unwrap().request_id, "q-secret");

    // Answered elsewhere: the modal lets go.
    state.apply_event(&request_event(14, &PendingRequest { status: "responded".into(), ..secret }));
    assert!(state.focused_request.is_none());
    // Its approval has no card while run r2 is not loaded: the modal is its one entry until then.
    assert_eq!(state.modal_request().unwrap().request_id, "q-bash");
}

#[test]
fn a_resync_keeps_an_answer_in_flight_and_drops_one_another_client_settled() {
    let mut state = live_state();
    state.apply_event(&request_event(11, &approval_request("q1", "pending")));
    let command = state.respond("q1", &ApprovalDecision::Allow).unwrap();
    state.on_command_result(&command.command_id, Err(BridgeError::unavailable("socket closed")));
    assert_eq!(state.requests[0].status, "responding");

    // Reconnected; the Core still waits on it: the answer stays ours to retry.
    let resync = |status: &str| Snapshot {
        sequence: 20,
        threads: Some(vec![thread("t1", "2026-11-23T00:00:00Z")]),
        runs: Some(vec![run("r1", "t1", RunStatus::Running)]),
        requests: vec![approval_request("q1", status)],
        ..Snapshot::default()
    };
    state.apply_snapshot(resync("pending"));
    assert_eq!(state.requests[0].status, "responding", "no second answer meanwhile");
    assert!(state.respond("q1", &ApprovalDecision::Deny).is_err());
    let again = state.retry("request:q1").unwrap();
    assert_eq!(again, command, "same command id: the Core takes it once");
    state.on_command_result(&command.command_id, Err(BridgeError::unavailable("socket closed")));

    // Another client answered while this one was away: the answer is over.
    state.apply_snapshot(resync("responded"));
    assert!(state.pending_by_key("request:q1").is_none());
    assert!(state.timeline().iter().all(|row| !matches!(row, TimelineItem::Approval { .. })));
}

#[test]
fn a_summary_resync_points_to_threads_waiting_before_their_detail_loads() {
    let mut state = AppState::default();
    let summary = |id: &str, pending| ThreadSummary {
        id: id.into(),
        title: format!("会话 {id}"),
        created_at: "2026-11-23T08:00:00Z".into(),
        updated_at: "2026-11-23T08:00:00Z".into(),
        pending_requests: pending,
        ..ThreadSummary::default()
    };
    state.apply_summaries(ThreadSummaries {
        sequence: 7,
        threads: Some(vec![summary("t1", 0), summary("t2", 1)]),
    });
    state.select_thread(Some("t1".into()));
    let attention = state.background_attention();
    assert_eq!(attention.len(), 1);
    assert_eq!((attention[0].thread_id.as_str(), attention[0].count), ("t2", 0));
    assert!(state.modal_request().is_none());
}
