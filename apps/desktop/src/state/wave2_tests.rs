//! Wave 2's state flows: the incremental timeline, thread management,
//! the queue, attachments, the sidebar's groups and tree, and an end-to-end
//! run against the in-memory Core.

use std::time::Duration;

use serde_json::json;
use vantry_protocol::{
    Attachment, Command, CommandKind, CoreEvent, Input, Receipt, Run, RunStatus, Snapshot, Task,
    Thread, ToolCall,
};

use super::{
    sidebar::{Group, SidebarFilter, StatusFilter},
    *,
};
use crate::bridge::{BridgeError, CoreBridge, ErrorCode, MockBridge, events};

fn thread(id: &str, created_at: &str) -> Thread {
    Thread {
        id: id.into(),
        title: format!("会话 {id}"),
        created_at: created_at.into(),
        ..Thread::default()
    }
}

fn run(id: &str, thread_id: &str, status: RunStatus, created_at: &str) -> Run {
    Run {
        id: id.into(),
        thread_id: thread_id.into(),
        status,
        input: format!("问 {id}"),
        created_at: created_at.into(),
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

fn loaded(threads: Vec<Thread>, runs: Vec<Run>) -> AppState {
    let mut state = AppState::default();
    state.apply_snapshot(Snapshot {
        sequence: 10,
        threads: Some(threads),
        runs: Some(runs),
        ..Snapshot::default()
    });
    state.on_live();
    state
}

fn accepted(command: &Command) -> Receipt {
    Receipt {
        command_id: command.command_id.clone(),
        accepted: true,
        thread_id: command.thread_id.clone(),
        ..Receipt::default()
    }
}

/// The rows of `state`'s shown thread as rebuilt from scratch.
fn rebuilt(state: &AppState) -> Vec<TimelineItem> {
    let mut fresh = state.timeline.clone();
    fresh.rebuild_all(state.runs.iter().map(|run| RunParts {
        run,
        blocks: state.blocks.iter().filter(|b| b.run_id == run.id).collect(),
        tools: state.tools.iter().filter(|t| t.run_id == run.id).collect(),
        requests: state.requests.iter().filter(|r| r.run_id == run.id).collect(),
        inputs: state.inputs.iter().filter(|i| i.run_id == run.id).collect(),
    }));
    let thread = state.selected.clone().unwrap();
    fresh.rows(&thread).cloned().collect()
}

// ------------------------------------------------------- incremental timeline

#[test]
fn streamed_deltas_extend_one_row_without_rebuilding_or_rekeying() {
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![run("r1", "t1", RunStatus::Running, "2026-11-23T00:00:01Z")],
    );
    state.apply_event(&ev(11, events::RUN_OUTPUT, "t1", "r1", json!({ "delta": "# 标题\n\n" })));
    let rebuilds = state.timeline.rebuilds;
    let structure = state.timeline.structure("t1");
    let sidebar = state.sidebar_revision;
    let reply = "第一段，**粗体**。\n\n```rust\nfn main() {}\n```\n\n- 一\n- 二\n";
    for (n, ch) in reply.chars().enumerate() {
        state.apply_event(&ev(12 + n as i64, events::RUN_OUTPUT, "t1", "r1", json!({ "delta": ch.to_string() })));
    }
    assert_eq!(state.timeline.rebuilds, rebuilds, "deltas take the fast path");
    assert_eq!(state.timeline.structure("t1"), structure, "the keys stay");
    assert_eq!(state.sidebar_revision, sidebar, "the sidebar is not woken");
    assert_eq!(state.timeline(), rebuilt(&state));
    let Some(TimelineItem::Text { doc, streaming: true, .. }) = state.timeline().last().cloned() else {
        panic!("{:?}", state.timeline())
    };
    assert_eq!(doc.blocks.len(), 4, "heading, paragraph, code, list");
}

#[test]
fn a_tool_mid_stream_starts_a_new_row_once_then_streams_fast_again() {
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![run("r1", "t1", RunStatus::Running, "2026-11-23T00:00:01Z")],
    );
    state.apply_event(&ev(11, events::RUN_OUTPUT, "t1", "r1", json!({ "delta": "先看看。" })));
    let before = state.timeline.structure("t1");
    let tool = ToolCall {
        id: "k1".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        name: "bash".into(),
        status: "running".into(),
        output_offset: "先看看。".len() as i64,
        ..ToolCall::default()
    };
    state.apply_event(&ev(12, events::TOOL_UPDATED, "t1", "r1", json!({ "tool": tool })));
    assert!(state.timeline.structure("t1") > before, "a tool row is new");
    // Text after the tool is a new row (one rebuild), then fast again.
    state.apply_event(&ev(13, events::RUN_OUTPUT, "t1", "r1", json!({ "delta": "结" })));
    let after_new_row = state.timeline.structure("t1");
    let rebuilds = state.timeline.rebuilds;
    state.apply_event(&ev(14, events::RUN_OUTPUT, "t1", "r1", json!({ "delta": "果" })));
    state.apply_event(&ev(15, events::RUN_OUTPUT, "t1", "r1", json!({ "delta": "。" })));
    assert_eq!(state.timeline.rebuilds, rebuilds);
    assert_eq!(state.timeline.structure("t1"), after_new_row);
    assert_eq!(state.timeline(), rebuilt(&state));
    let ids: Vec<String> = state.timeline().iter().map(|i| i.id().to_owned()).collect();
    assert_eq!(ids, ["user:r1", "text:r1:0", "tool:k1", "text:r1:1"]);
}

#[test]
fn a_rewritten_output_is_parsed_afresh() {
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![run("r1", "t1", RunStatus::Running, "2026-11-23T00:00:01Z")],
    );
    state.apply_event(&ev(11, events::RUN_OUTPUT, "t1", "r1", json!({ "delta": "# 旧标题\n\n旧段落" })));
    // The Core's full output replaces what was streamed (same length or
    // longer, different text).
    state.apply_event(&ev(12, events::RUN_OUTPUT, "t1", "r1", json!({ "output": "新的段落，完全不同，而且更长一些。" })));
    assert_eq!(state.timeline(), rebuilt(&state));
    let mut done = run("r1", "t1", RunStatus::Completed, "2026-11-23T00:00:01Z");
    done.output = "- 列表一\n- 列表二，替换了之前的全部内容".into();
    state.apply_event(&ev(13, events::RUN_COMPLETED, "t1", "r1", json!({ "run": done })));
    assert_eq!(state.timeline(), rebuilt(&state));
    assert!(matches!(
        state.timeline().last(),
        Some(TimelineItem::Text { doc, .. }) if matches!(&*doc.blocks[0].block, markdown::Block::List { .. })
    ));
}

#[test]
fn reasoning_deltas_update_in_place() {
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![run("r1", "t1", RunStatus::Running, "2026-11-23T00:00:01Z")],
    );
    state.apply_event(&ev(11, events::RUN_REASONING, "t1", "r1", json!({ "reasoningDelta": "想" })));
    let rebuilds = state.timeline.rebuilds;
    state.apply_event(&ev(12, events::RUN_REASONING, "t1", "r1", json!({ "reasoningDelta": "一想" })));
    assert_eq!(state.timeline.rebuilds, rebuilds);
    assert!(matches!(
        &state.timeline()[1],
        TimelineItem::Reasoning { text, streaming: true, .. } if &**text == "想一想"
    ));
}

#[test]
fn runs_keep_their_creation_order_and_rows_are_found_by_index() {
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![
            run("late", "t1", RunStatus::Completed, "2026-11-23T00:00:09Z"),
            run("early", "t1", RunStatus::Completed, "2026-11-23T00:00:01Z"),
        ],
    );
    let ids: Vec<String> = state.timeline().iter().map(|i| i.id().to_owned()).collect();
    assert_eq!(ids, ["user:early", "user:late"]);
    // A run arriving later in between goes in its place.
    let middle = run("middle", "t1", RunStatus::Queued, "2026-11-23T00:00:05Z");
    state.apply_event(&ev(11, events::RUN_QUEUED, "t1", "middle", json!({ "run": middle })));
    let ids: Vec<String> = (0..state.timeline.len("t1"))
        .map(|i| state.timeline.item("t1", i).unwrap().id().to_owned())
        .collect();
    assert_eq!(ids, ["user:early", "user:middle", "user:late"]);
    assert_eq!(state.timeline.keys("t1"), ids);
    assert!(state.timeline.item("t1", 3).is_none());
    assert_eq!(state.thread_runs("t1").len(), 3);
}

#[test]
fn a_rewind_removes_the_target_and_later_runs() {
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![
            run("a", "t1", RunStatus::Completed, "2026-11-23T00:00:01Z"),
            run("b", "t1", RunStatus::Completed, "2026-11-23T00:00:02Z"),
            run("c", "t1", RunStatus::Completed, "2026-11-23T00:00:03Z"),
        ],
    );
    state.apply_event(&ev(
        11,
        events::THREAD_REWOUND,
        "t1",
        "b",
        json!({ "threadId": "t1", "runId": "b", "removedRunIds": ["b", "c"] }),
    ));
    assert_eq!(state.runs.len(), 1);
    assert_eq!(state.timeline.keys("t1"), ["user:a"]);
    // An older record without the list removes the target and what follows.
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![
            run("a", "t1", RunStatus::Completed, "2026-11-23T00:00:01Z"),
            run("b", "t1", RunStatus::Completed, "2026-11-23T00:00:02Z"),
        ],
    );
    state.apply_event(&ev(11, events::THREAD_REWOUND, "t1", "a", json!({})));
    assert!(state.runs.is_empty());
    assert_eq!(state.timeline.len("t1"), 0);
}

// -------------------------------------------------------------- thread actions

#[test]
fn renames_and_pins_show_at_once_and_come_back_when_refused() {
    let mut state = loaded(vec![thread("t1", "2026-11-23T00:00:00Z")], vec![]);
    let rename = state.rename_thread("t1", "  新标题 ").unwrap();
    assert_eq!(rename.kind, CommandKind::UpdateThread);
    assert_eq!(rename.title, "新标题");
    assert_eq!(state.thread("t1").unwrap().title, "新标题");
    assert!(state.rename_thread("t1", "新标题").is_none(), "nothing to change");
    let pin = state.set_pinned("t1", true).unwrap();
    assert_eq!(pin.pinned, Some(true));
    state.on_command_result(
        &rename.command_id,
        Err(BridgeError::new(ErrorCode::InvalidCommand, "title too long")),
    );
    assert_eq!(state.thread("t1").unwrap().title, "会话 t1", "restored");
    assert!(state.thread("t1").unwrap().pinned, "the pin is another command");
    // An unknown outcome keeps the change and retries the same command.
    state.on_command_result(&pin.command_id, Err(BridgeError::unavailable("reset")));
    assert!(state.thread("t1").unwrap().pinned);
    let again = state.retry("thread:t1:pin").unwrap();
    assert_eq!(again, pin);
    state.on_command_result(&pin.command_id, Ok(accepted(&pin)));
    assert!(state.pending.is_empty());
}

#[test]
fn archiving_the_shown_thread_moves_the_selection_and_snoozing_round_trips() {
    let mut state = loaded(
        vec![thread("a", "2026-11-20T00:00:00Z"), thread("b", "2026-11-22T00:00:00Z")],
        vec![],
    );
    assert_eq!(state.selected.as_deref(), Some("b"));
    let archive = state.set_archived("b", true).unwrap();
    assert_eq!(archive.archived, Some(true));
    assert_eq!(state.selected.as_deref(), Some("a"));
    let snooze = state.snooze("a", Some("2099-01-01T00:00:00Z".into())).unwrap();
    assert_eq!(snooze.snoozed_until.as_deref(), Some("2099-01-01T00:00:00Z"));
    let wake = state.snooze("a", None).unwrap();
    assert_eq!(wake.snoozed_until.as_deref(), Some(""), "an empty time wakes it");
    assert_eq!(state.thread("a").unwrap().snoozed_until, None);
}

#[test]
fn delete_rewind_fork_and_merge_back() {
    let mut fork_source = thread("t1", "2026-11-23T00:00:00Z");
    fork_source.forked_from_thread_id = String::new();
    let mut fork = thread("f1", "2026-11-23T01:00:00Z");
    fork.forked_from_thread_id = "t1".into();
    let mut state = loaded(
        vec![fork_source, fork],
        vec![
            run("r1", "t1", RunStatus::Completed, "2026-11-23T00:00:01Z"),
            run("r2", "t1", RunStatus::Running, "2026-11-23T00:00:02Z"),
        ],
    );
    // Not while it runs.
    assert!(state.delete_thread("t1").is_none());
    assert!(state.rewind("t1", "r1").is_none());
    assert!(state.notice.as_ref().unwrap().text.contains("停止"));
    state.apply_event(&ev(11, events::RUN_COMPLETED, "t1", "r2", json!({ "run": run("r2", "t1", RunStatus::Completed, "2026-11-23T00:00:02Z") })));

    let rewind = state.rewind("t1", "r2").unwrap();
    assert_eq!((rewind.kind, rewind.run_id.as_str()), (CommandKind::RewindThread, "r2"));
    assert!(state.rewind("t1", "r2").is_none(), "already on its way");
    assert!(state.rewind("t1", "missing").is_none());

    // Forking defaults to the latest finished run; the new thread shows.
    let fork_cmd = state.fork("t1", None).unwrap();
    assert_eq!(fork_cmd.run_id, "r2");
    state.on_command_result(
        &fork_cmd.command_id,
        Ok(Receipt { thread_id: "t9".into(), ..accepted(&fork_cmd) }),
    );
    assert_eq!(state.selected.as_deref(), Some("t9"));
    assert!(state.thread("t9").is_some());

    assert!(!state.can_merge_back("t1"));
    assert!(state.can_merge_back("f1"));
    assert!(state.merge_back("t1").is_none());
    let merge = state.merge_back("f1").unwrap();
    state.on_command_result(
        &merge.command_id,
        Ok(Receipt { thread_id: "t1".into(), ..accepted(&merge) }),
    );
    assert_eq!(state.selected.as_deref(), Some("t1"));

    let delete = state.delete_thread("f1").unwrap();
    assert_eq!(delete.kind, CommandKind::DeleteThread);
    state.on_command_result(&delete.command_id, Ok(accepted(&delete)));
    assert!(state.notice.as_ref().unwrap().text.contains("已删除"));
}

#[test]
fn models_and_policies_go_to_the_thread_or_to_new_threads() {
    let mut state = loaded(vec![thread("t1", "2026-11-23T00:00:00Z")], vec![]);
    let model = state.set_model("anthropic/opus").unwrap();
    assert_eq!(model.kind, CommandKind::SetThreadModel);
    assert_eq!(state.current_model(), "anthropic/opus");
    let policy = state.set_policy("auto").unwrap();
    assert_eq!(policy.approval_policy, "auto");
    assert_eq!(state.current_policy(), "auto");
    assert_eq!(policy_label("auto-accept-edits"), "改文件自动");
    state.new_thread();
    assert!(state.set_model("openai/gpt").is_none());
    assert!(state.set_policy("ask").is_none());
    assert_eq!(state.current_model(), "openai/gpt");
    // The new thread is created with them.
    state.set_draft_text("开始");
    let create = state.submit("fixture/model").unwrap();
    assert_eq!(create.kind, CommandKind::CreateThread);
    assert_eq!((create.model.as_str(), create.approval_policy.as_str()), ("openai/gpt", "ask"));
}

#[test]
fn attachments_ride_with_the_message() {
    let mut state = loaded(vec![thread("t1", "2026-11-23T00:00:00Z")], vec![]);
    let attachment = Attachment {
        id: "att-1".into(),
        name: "shot.png".into(),
        mime: "image/png".into(),
        size: 10,
    };
    state.add_attachment(attachment.clone());
    state.add_attachment(attachment.clone());
    assert_eq!(state.draft().attachments.len(), 1, "once");
    assert!(state.composer_status().can_submit, "an attachment alone can be sent");
    let send = state.submit("fixture/model").unwrap();
    assert_eq!(send.attachments, vec![attachment]);
    assert!(state.draft().attachments.is_empty());
    state.add_attachment(Attachment { id: "x".into(), ..Attachment::default() });
    state.remove_attachment("x");
    assert!(state.draft().attachments.is_empty());
}

// ------------------------------------------------------------------- queue

fn queued(id: &str, position: i64, text: &str) -> Input {
    Input {
        id: id.into(),
        thread_id: "t1".into(),
        kind: "queue".into(),
        message: text.into(),
        status: "queued".into(),
        position,
        ..Input::default()
    }
}

fn input_event(sequence: i64, input: &Input) -> CoreEvent {
    ev(sequence, events::INPUT_UPDATED, "t1", "", json!({ "input": input }))
}

fn queue_state() -> AppState {
    let mut state = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![run("r1", "t1", RunStatus::Running, "2026-11-23T00:00:01Z")],
    );
    state.apply_event(&input_event(11, &queued("a", 0, "甲")));
    state.apply_event(&input_event(12, &queued("b", 1, "乙")));
    state.apply_event(&input_event(13, &queued("c", 2, "丙")));
    state
}

#[test]
fn the_queue_lists_in_order_and_reorders_optimistically() {
    let mut state = queue_state();
    let texts = |s: &AppState| s.queue_items().iter().map(|i| i.text.clone()).collect::<Vec<_>>();
    assert_eq!(texts(&state), ["甲", "乙", "丙"]);
    let items = state.queue_items();
    assert_eq!((items[0].index, items[2].count), (0, 3));
    assert!(state.move_queued("a", -1).is_none(), "already first");
    let reorder = state.move_queued("c", -2).unwrap();
    assert_eq!((reorder.kind, reorder.position), (CommandKind::ReorderInput, 0));
    assert_eq!(texts(&state), ["丙", "甲", "乙"]);
    assert!(state.queue_items()[0].busy);
    state.on_command_result(
        &reorder.command_id,
        Err(BridgeError::new(ErrorCode::Conflict, "already started")),
    );
    assert_eq!(texts(&state), ["甲", "乙", "丙"], "restored");
    // The Core's word replaces it.
    let mut started = queued("a", 0, "甲");
    started.status = "started".into();
    state.apply_event(&input_event(14, &started));
    assert_eq!(texts(&state), ["乙", "丙"]);
}

#[test]
fn editing_a_queued_message_uses_the_composer_and_restores_the_draft() {
    let mut state = queue_state();
    state.set_draft_text("我的草稿");
    state.begin_queue_edit("b");
    assert_eq!(state.draft().text, "乙");
    let status = state.composer_status();
    assert!(status.editing);
    assert_eq!(status.primary, PrimaryAction::Save);
    assert!(state.queue_items()[1].editing);
    state.set_draft_text("乙（改）");
    let edit = state.submit("fixture/model").unwrap();
    assert_eq!(edit.kind, CommandKind::EditInput);
    assert_eq!((edit.input_id.as_str(), edit.message.as_str()), ("b", "乙（改）"));
    assert_eq!(state.draft().text, "我的草稿", "the draft is back");
    assert_eq!(state.queue_items()[1].text, "乙（改）");
    state.on_command_result(
        &edit.command_id,
        Err(BridgeError::new(ErrorCode::Conflict, "started")),
    );
    assert_eq!(state.queue_items()[1].text, "乙", "restored on refusal");

    // Cancelling the edit changes nothing; an input that starts ends it.
    state.begin_queue_edit("c");
    state.end_queue_edit();
    assert_eq!(state.draft().text, "我的草稿");
    state.begin_queue_edit("c");
    let mut started = queued("c", 2, "丙");
    started.status = "started".into();
    state.apply_event(&input_event(20, &started));
    assert!(state.queue_edit.is_none());
    assert_eq!(state.draft().text, "我的草稿");
}

#[test]
fn promoting_needs_a_running_run_and_cancel_is_per_input() {
    let mut state = queue_state();
    let promote = state.promote_queued("a").unwrap();
    assert_eq!(promote.kind, CommandKind::PromoteInputToSteer);
    assert_eq!(promote.run_id, "r1");
    assert!(state.promote_queued("a").is_none());
    let cancel = state.cancel_queued("b").unwrap();
    assert_eq!((cancel.kind, cancel.input_id.as_str()), (CommandKind::CancelInput, "b"));
    // A queued run (not running yet) takes no steer.
    let mut waiting = loaded(
        vec![thread("t1", "2026-11-23T00:00:00Z")],
        vec![run("r1", "t1", RunStatus::Queued, "2026-11-23T00:00:01Z")],
    );
    waiting.apply_event(&input_event(11, &queued("a", 0, "甲")));
    assert!(!waiting.queue_items()[0].can_insert);
    assert!(waiting.promote_queued("a").is_none());
    assert!(waiting.notice.is_some());
    // Undelivered steers show with their state.
    let mut steer = queued("s", 0, "快一点");
    steer.kind = "steer".into();
    steer.status = "unknown".into();
    state.apply_event(&input_event(30, &steer));
    let last = state.queue_items().pop().unwrap();
    assert_eq!(last.kind, QueueItemKind::Steer);
    assert!(last.status.contains("交付不明"));
}

// ------------------------------------------------------------------ sidebar

#[test]
fn same_project_path_on_two_machines_has_independent_sidebar_preferences() {
    use super::sidebar::scoped_project;
    let mut filter = SidebarFilter::default();
    let peer_key = scoped_project("peer", "/repo");
    filter.project_open.insert(peer_key.clone(), false);
    filter.show_all.insert(peer_key.clone());
    let local = filter.for_machine("").unwrap();
    assert!(local.project_open.is_empty() && local.show_all.is_empty());
    filter.project = Some(peer_key);
    assert!(filter.for_machine("").is_none());
    let peer = filter.for_machine("peer").unwrap();
    assert_eq!(peer.project.as_deref(), Some("/repo"));
    assert_eq!(peer.project_open.get("/repo"), Some(&false));
    assert!(peer.show_all.contains("/repo"));
}

#[test]
fn sidebar_groups_roots_and_finds_their_subtasks() {
    let now = chrono::DateTime::parse_from_rfc3339("2026-11-23T12:00:00Z").unwrap().with_timezone(&chrono::Utc);
    let mut pinned = thread("pinned", "2026-11-01T00:00:00Z");
    pinned.pinned = true;
    let parent = thread("parent", "2026-11-22T00:00:00Z");
    let mut child = thread("child", "2026-11-22T01:00:00Z");
    child.parent_thread_id = "parent".into();
    child.title = "子任务：测试".into();
    let mut snoozed = thread("snoozed", "2026-11-21T00:00:00Z");
    snoozed.snoozed_until = Some("2026-11-24T00:00:00Z".into());
    let mut woke = thread("woke", "2026-11-20T00:00:00Z");
    woke.snoozed_until = Some("2026-11-23T00:00:00Z".into());
    let mut archived = thread("archived", "2026-11-19T00:00:00Z");
    archived.archived = true;
    let mut state = loaded(
        vec![pinned, parent, child, snoozed, woke, archived],
        vec![run("rc", "child", RunStatus::Failed, "2026-11-22T01:00:01Z")],
    );
    state.apply_event(&ev(
        11,
        events::TASK_CREATED,
        "parent",
        "",
        json!({ "task": Task {
            id: "task-1".into(),
            parent_thread_id: "parent".into(),
            child_thread_id: "child".into(),
            status: RunStatus::Failed,
            ..Task::default()
        } }),
    ));
    let mut filter = SidebarFilter::default();
    let sections = state.sidebar_sections(&filter, now);
    let groups: Vec<Group> = sections.iter().map(|s| s.group).collect();
    assert_eq!(groups, [Group::Pinned, Group::Active, Group::Snoozed, Group::Archived]);
    let active: Vec<_> = sections[1].rows.iter().map(|r| r.id.as_str()).collect();
    assert_eq!(active, ["parent", "woke"]);
    assert!(sections[3].folded && sections[3].rows.is_empty());
    assert_eq!(sections[3].count, 1);
    assert_eq!(state.subtasks("parent").len(), 1);

    // A child match opens its detail from the parent row.
    filter.query = "测试".into();
    let sections = state.sidebar_sections(&filter, now);
    let ids: Vec<&str> = sections.iter().flat_map(|s| s.rows.iter().map(|r| r.id.as_str())).collect();
    assert_eq!(ids, ["parent"]);
    assert_eq!(sections[0].rows[0].matched_child.as_deref(), Some("child"));
    filter.query = "archived".into();
    let sections = state.sidebar_sections(&filter, now);
    assert_eq!(sections.len(), 1);
    assert!(!sections[0].folded, "a search shows archived matches");

    // Attention keeps failures (with their parent).
    filter.query.clear();
    filter.status = StatusFilter::Attention;
    let sections = state.sidebar_sections(&filter, now);
    let ids: Vec<&str> = sections.iter().flat_map(|s| s.rows.iter().map(|r| r.id.as_str())).collect();
    assert_eq!(ids, ["parent"]);
}

#[test]
fn switching_unsent_drafts_does_not_redirect_a_late_create_or_its_edits() {
    let mut state = loaded(vec![thread("existing", "")], vec![]);
    state.start_local_draft();
    let first = state.draft_key();
    state.start_local_draft();
    assert_eq!(state.draft_key(), first, "reuse the empty draft");
    state.set_draft_text("first send");
    let create = state.submit("fixture/model").unwrap();
    state.set_draft_text("written while creating");
    state.start_local_draft();
    let second = state.draft_key();
    state.set_draft_text("second draft");
    let send = state
        .on_command_result(
            &create.command_id,
            Ok(Receipt {
                thread_id: "created".into(),
                ..accepted(&create)
            }),
        )
        .unwrap();
    assert_eq!(send.thread_id, "created");
    assert_eq!(send.message, "first send");
    assert_eq!(state.draft_key(), second);
    assert_eq!(state.draft().text, "second draft");
    assert_eq!(state.drafts["created"].text, "written while creating");
    assert_eq!(state.local_order, [second.clone()]);
    let rejected = state.submit("fixture/model").unwrap();
    state.select_thread(Some("existing".into()));
    state.on_command_result(
        &rejected.command_id,
        Ok(Receipt {
            accepted: false,
            ..accepted(&rejected)
        }),
    );
    assert_eq!(state.drafts[&second].text, "second draft");
    assert_eq!(state.selected.as_deref(), Some("existing"));
    state.discard_local_draft(&second);
    assert!(state.local_order.is_empty());
}

#[test]
fn attachment_import_finishes_in_the_draft_that_opened_the_picker() {
    let mut state = loaded(vec![thread("other", "")], vec![]);
    state.start_local_draft();
    let original = state.draft_key();
    state.begin_attachment_import(&original);
    state.set_draft_text("待上传");
    assert!(!state.composer_status().can_submit);
    state.select_thread(Some("other".into()));
    let image = Attachment {
        id: "file_1".into(),
        name: "样图.png".into(),
        mime: "image/png".into(),
        size: 50,
    };
    state.add_attachment_to(
        original.clone(),
        image.clone(),
        Some("/tmp/样图.png".into()),
    );
    assert!(state.draft().attachments.is_empty());
    state.finish_attachment_import(&original);
    state.select_thread(Some(original));
    assert_eq!(state.draft().attachments, [image]);
    assert!(state.composer_status().can_submit);
}

#[test]
fn projects_keep_full_paths_and_group_worktrees_with_the_repository() {
    use super::sidebar::{project_name, workspace_key};
    let checkout = WorkspaceSpec {
        kind: "worktree".into(),
        path: "/tmp/worktrees/a".into(),
        repository: "/one/repo/./".into(),
        branch: "topic".into(),
        ..Default::default()
    };
    assert_eq!(workspace_key(Some(&checkout), ""), "/one/repo");
    assert_eq!(
        workspace_key(None, r"C:\projects\team\..\repo\"),
        "C:/projects/repo"
    );
    assert_eq!(workspace_key(None, "/data/workspaces/session-1/chat"), "");
    assert_eq!(project_name(""), "无项目");
    let now = chrono::Utc::now();
    let mut first = thread("first", "2026-01-02T04:00:00Z");
    first.project_path = "/one/repo".into();
    let mut other = thread("other", "2026-01-03T04:00:00Z");
    other.project_path = "/two/repo".into();
    let mut child = thread("checkout", "2026-01-02T04:00:00Z");
    child.workspace = Some(checkout);
    let mut state = loaded(
        vec![first, other, child],
        vec![run(
            "latest",
            "checkout",
            RunStatus::Completed,
            "2026-01-03T01:00:00-05:00",
        )],
    );
    state.select_thread(Some("checkout".into()));
    assert_eq!(state.checkout_path(), Some("/tmp/worktrees/a"));
    assert_eq!(state.current_project_key(), "/one/repo");
    state.select_thread(Some("other".into()));
    assert_eq!(state.checkout_path(), Some("/two/repo"));
    let mut filter = SidebarFilter::default();
    let sections = state.sidebar_sections(&filter, now);
    assert_eq!(
        sections
            .iter()
            .map(|s| s.project.as_deref())
            .collect::<Vec<_>>(),
        [Some("/one/repo"), Some("/two/repo")]
    );
    assert_eq!(sections[0].rows[0].branch, "topic");
    filter.project_open.insert("/one/repo".into(), false);
    assert!(state.sidebar_sections(&filter, now)[0].folded);
    filter.query = "/one".into();
    let shown = state.sidebar_sections(&filter, now);
    assert_eq!(shown.len(), 1);
    assert_eq!(shown[0].rows.len(), 2);
    assert_eq!(filter.project_open.get("/one/repo"), Some(&false));
    filter.query.clear();
    filter.project = Some("/two/repo".into());
    assert_eq!(state.sidebar_sections(&filter, now)[0].rows[0].id, "other");
}

#[test]
fn project_show_more_does_not_expand_other_projects() {
    let now = chrono::Utc::now();
    let mut threads = Vec::new();
    for project in ["/one", "/two"] {
        for i in 0..7 {
            let mut item = thread(&format!("{project}-{i}"), "2026-01-01T00:00:00Z");
            item.project_path = project.into();
            threads.push(item);
        }
    }
    let state = loaded(threads, vec![]);
    let mut filter = SidebarFilter::default();
    assert!(
        state
            .sidebar_sections(&filter, now)
            .iter()
            .all(|s| (s.rows.len(), s.hidden) == (5, 2))
    );
    filter.show_all.insert("/one".into());
    let sections = state.sidebar_sections(&filter, now);
    assert_eq!((sections[0].rows.len(), sections[1].rows.len()), (7, 5));
}

#[test]
fn inbox_prioritizes_approval_and_tracks_new_background_results_once() {
    use super::sidebar::InboxKind;
    let mut state = loaded(vec![thread("open", ""), thread("other", "")], vec![]);
    state.select_thread(Some("open".into()));
    let reply = run("r1", "other", RunStatus::Completed, "2026-01-02T12:00:00Z");
    state.apply_event(&ev(
        11,
        events::RUN_COMPLETED,
        "other",
        "r1",
        json!({"run": reply}),
    ));
    let own = run("r2", "open", RunStatus::Completed, "2026-01-02T12:10:00Z");
    state.apply_event(&ev(
        12,
        events::RUN_COMPLETED,
        "open",
        "r2",
        json!({"run": own}),
    ));
    let request = PendingRequest {
        id: "q".into(),
        thread_id: "open".into(),
        status: "pending".into(),
        title: "确认操作".into(),
        ..Default::default()
    };
    state.apply_event(&ev(
        13,
        events::REQUEST_UPDATED,
        "open",
        "r2",
        json!({"request": request}),
    ));
    assert_eq!(
        state.inbox().iter().map(|i| i.kind).collect::<Vec<_>>(),
        [InboxKind::Approval, InboxKind::Reply]
    );
    state.select_thread(Some("other".into()));
    state.select_thread(Some("open".into()));
    state.apply_event(&ev(
        14,
        events::RUN_COMPLETED,
        "other",
        "r1",
        json!({"run": reply}),
    ));
    assert_eq!(state.inbox().len(), 1);
    assert_eq!(state.inbox()[0].request.as_deref(), Some("q"));
    let failure = run("r3", "other", RunStatus::Failed, "2026-01-02T12:30:00Z");
    state.apply_event(&ev(
        15,
        events::RUN_FAILED,
        "other",
        "r3",
        json!({"run": failure}),
    ));
    assert_eq!(state.inbox()[1].kind, InboxKind::Failed);
}

// --------------------------------------------------------------- end to end

/// The desktop's loop without a window: commands to the in-memory Core,
/// its events folded, until `done` says the state is there.
async fn pump(
    bridge: &MockBridge,
    state: &mut AppState,
    events: &mut tokio::sync::mpsc::UnboundedReceiver<CoreEvent>,
    done: impl Fn(&AppState) -> bool,
) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    while !done(state) {
        let event = tokio::time::timeout_at(deadline, events.recv())
            .await
            .expect("the state settles in time")
            .expect("the stream is open");
        state.apply_event(&event);
    }
    let _ = bridge;
}

async fn send(bridge: &MockBridge, state: &mut AppState, command: Command) {
    let id = command.command_id.clone();
    let result = bridge.dispatch(command).await;
    if let Some(next) = state.on_command_result(&id, result) {
        Box::pin(send(bridge, state, next)).await;
    }
}

#[tokio::test]
async fn a_conversation_against_the_mock_core_streams_asks_and_completes() {
    let bridge = MockBridge::new(Duration::ZERO);
    let mut state = AppState::default();
    state.apply_snapshot(bridge.snapshot().await.unwrap());
    let (sink, mut events) = tokio::sync::mpsc::unbounded_channel();
    let stream = tokio::spawn(bridge.subscribe(state.sequence, sink));

    state.set_draft_text("看看目录");
    let create = state.submit("fixture/model").unwrap();
    send(&bridge, &mut state, create).await;
    let thread = state.selected.clone().unwrap();
    pump(&bridge, &mut state, &mut events, |s| !s.approval_queue().is_empty()).await;
    assert!(state.modal_request().is_none());
    let request = state.timeline().into_iter().find_map(|row| match row {
        TimelineItem::Approval { view, .. } => Some(view),
        _ => None,
    }).expect("inline approval");
    assert!(request.is_approval());
    let respond = state.respond(&request.request_id, &ApprovalDecision::Allow).unwrap();
    send(&bridge, &mut state, respond).await;
    pump(&bridge, &mut state, &mut events, |s| {
        s.active_run(s.selected.as_deref().unwrap_or_default()).is_none()
            && !s.runs.is_empty()
    })
    .await;
    assert_eq!(state.thread_status(&thread), ThreadStatus::Idle);
    // The incremental rows equal rows built from scratch.
    assert_eq!(state.timeline(), rebuilt(&state));
    let rows = state.timeline();
    assert!(rows.iter().any(|r| matches!(r, TimelineItem::Tools { calls, .. }
        if calls.iter().any(|c| c.status == ToolStatus::Completed))));
    let markdown_blocks: usize = rows
        .iter()
        .filter_map(|r| match r {
            TimelineItem::Text { doc, .. } => Some(doc.blocks.len()),
            _ => None,
        })
        .sum();
    assert!(markdown_blocks >= 6, "the demo reply's Markdown parsed");
    assert!(state.pending.is_empty());

    // Queue while a run goes: it waits, then starts after the run.
    state.set_draft_text("第二条");
    let second = state.submit("fixture/model").unwrap();
    send(&bridge, &mut state, second).await;
    pump(&bridge, &mut state, &mut events, |s| !s.approval_queue().is_empty()).await;
    state.set_running_action(RunningAction::Queue);
    state.set_draft_text("排队的");
    let queue = state.submit("fixture/model").unwrap();
    assert_eq!(queue.kind, CommandKind::QueueMessage);
    send(&bridge, &mut state, queue).await;
    pump(&bridge, &mut state, &mut events, |s| s.queue_items().len() == 1).await;
    let stop = state.stop().unwrap();
    send(&bridge, &mut state, stop).await;
    pump(&bridge, &mut state, &mut events, |s| s.queue_items().is_empty()).await;
    assert!(state.inputs.iter().all(|i| i.status == "cancelled"), "stopping cancels the queue");

    drop(events);
    let _ = tokio::time::timeout(Duration::from_secs(5), stream).await;
}

#[tokio::test]
async fn a_dropped_stream_resubscribes_after_the_cursor_without_duplicates() {
    let bridge = MockBridge::quiet();
    let mut state = AppState::default();
    let created = bridge.dispatch(Command::new("c", CommandKind::CreateThread)).await.unwrap();
    let mut send_cmd = Command::new("s", CommandKind::SendMessage);
    send_cmd.thread_id = created.thread_id.clone();
    send_cmd.message = "hi".into();
    let run_id = bridge.dispatch(send_cmd).await.unwrap().run_id;
    state.apply_snapshot(bridge.snapshot().await.unwrap());
    let (sink, mut events) = tokio::sync::mpsc::unbounded_channel();
    let stream = tokio::spawn(bridge.subscribe(state.sequence, sink));
    bridge.emit(events::RUN_OUTPUT, &created.thread_id, &run_id, json!({ "delta": "一" }));
    state.apply_event(&events.recv().await.unwrap());
    bridge.drop_subscribers(BridgeError::unavailable("reset"));
    let ended = stream.await.unwrap();
    let Recovery::Resubscribe { after, .. } = state.on_stream_ended(ended) else {
        panic!("a reset resubscribes")
    };
    bridge.emit(events::RUN_OUTPUT, &created.thread_id, &run_id, json!({ "delta": "二" }));
    let (sink, mut events) = tokio::sync::mpsc::unbounded_channel();
    let stream = tokio::spawn(bridge.subscribe(after - 1, sink));
    // A replay overlapping the cursor folds once.
    for _ in 0..2 {
        state.apply_event(&events.recv().await.unwrap());
    }
    assert_eq!(state.run(&run_id).unwrap().output, "一二");
    assert_eq!(state.stats.stale, 1);
    bridge.drop_subscribers(BridgeError::new(ErrorCode::ResyncRequired, "gone"));
    let ended = stream.await.unwrap();
    assert!(matches!(state.on_stream_ended(ended), Recovery::Resync { .. }));
}
