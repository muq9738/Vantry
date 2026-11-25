//! Wave 4's state flows: request kinds (approval, secret, terminal), the
//! local Core's start, the dock's search, branch switch and discard, and
//! the thinking level riding on the model.

use vantry_protocol::{
    GitFile, GitState, PendingRequest, Receipt, Run, RunStatus, Snapshot, Thread, ThreadDetail,
    ThreadSummary, ThreadSummaries, ToolCall,
};

use super::{
    sidebar::SidebarFilter,
    workbench::{FileSearch, GitPanel},
    *,
};
use crate::bridge::{BridgeError, workbench::SearchHit};

fn request(id: &str, method: &str, title: &str) -> PendingRequest {
    PendingRequest {
        id: id.into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        method: method.into(),
        title: title.into(),
        status: "pending".into(),
        created_at: "2026-11-23T08:00:00Z".into(),
        ..PendingRequest::default()
    }
}

fn with_requests(requests: Vec<PendingRequest>, tools: Vec<ToolCall>) -> AppState {
    let mut state = AppState::default();
    state.apply_snapshot(Snapshot {
        sequence: 3,
        threads: Some(vec![Thread {
            id: "t1".into(),
            title: "会话".into(),
            model: "anthropic/opus".into(),
            ..Thread::default()
        }]),
        runs: Some(vec![Run {
            id: "r1".into(),
            thread_id: "t1".into(),
            status: RunStatus::Running,
            ..Run::default()
        }]),
        requests,
        tools,
        ..Snapshot::default()
    });
    state.on_live();
    state.select_thread(Some("t1".into()));
    state
}

fn bounded_detail(thread_id: &str, run_id: &str) -> ThreadDetail {
    ThreadDetail {
        sequence: 7,
        thread: Thread {
            id: thread_id.into(),
            title: format!("会话 {thread_id}"),
            created_at: "2026-11-23T08:00:00Z".into(),
            unread_sequence: Some(7),
            ..Thread::default()
        },
        runs: Some(vec![Run {
            id: run_id.into(),
            thread_id: thread_id.into(),
            status: RunStatus::Completed,
            input: "首条消息".into(),
            output: "已完成".into(),
            created_at: "2026-11-23T08:00:01Z".into(),
            ..Run::default()
        }]),
        ..ThreadDetail::default()
    }
}

fn detail_variant(thread_id: &str, run_id: &str, sequence: i64, truncated: bool) -> ThreadDetail {
    let mut detail = bounded_detail(thread_id, run_id);
    detail.sequence = sequence;
    detail.truncated_runs = truncated;
    detail
}

fn summary(thread_id: &str, active_runs: i64) -> ThreadSummary {
    ThreadSummary {
        id: thread_id.into(),
        title: format!("会话 {thread_id}"),
        created_at: "2026-11-23T08:00:00Z".into(),
        updated_at: "2026-11-23T08:00:00Z".into(),
        active_runs,
        ..ThreadSummary::default()
    }
}

#[test]
fn summaries_paint_sidebar_before_authoritative_snapshot() {
    let mut state = AppState::default();
    state.apply_summaries(ThreadSummaries {
        sequence: 7,
        threads: Some(vec![ThreadSummary {
            id: "t1".into(),
            title: "首屏会话".into(),
            created_at: "2026-11-23T08:00:00Z".into(),
            updated_at: "2026-11-23T08:00:02Z".into(),
            unread_sequence: Some(7),
            active_runs: 1,
            pending_requests: 1,
            latest_status: Some(RunStatus::Running),
            ..ThreadSummary::default()
        }]),
    });

    assert!(!state.snapshot_loaded);
    assert_eq!(state.selected.as_deref(), Some("t1"));
    assert!(state.unread.contains("t1"));
    assert_eq!(state.thread_status("t1"), ThreadStatus::NeedsApproval);
    assert_eq!(
        state.sidebar_sections(&SidebarFilter::default(), chrono::Utc::now())[0].rows[0].title,
        "首屏会话"
    );
    state.set_draft_text("提前输入");
    assert!(!state.composer_status().can_submit);

    let token = state.begin_detail_load("t1");
    assert!(state.apply_thread_detail(token, bounded_detail("t1", "r1")));
    assert!(state.has_conversation());
    assert_eq!(state.timeline().len(), 2);
    assert!(
        state.notice.is_none(),
        "loading notice should clear once bounded detail is applied"
    );
    assert!(!state.snapshot_loaded, "full snapshot still owns global actions");
}

#[test]
fn late_detail_from_an_old_selection_is_discarded() {
    let mut state = AppState::default();
    state.apply_summaries(ThreadSummaries {
        sequence: 1,
        threads: Some(vec![
            ThreadSummary {
                id: "t1".into(),
                title: "一".into(),
                created_at: "2026-11-23T08:00:00Z".into(),
                ..ThreadSummary::default()
            },
            ThreadSummary {
                id: "t2".into(),
                title: "二".into(),
                created_at: "2026-11-23T08:00:01Z".into(),
                ..ThreadSummary::default()
            },
        ]),
    });
    let first = state.begin_detail_load("t1");
    let second = state.begin_detail_load("t2");
    assert!(!state.apply_thread_detail(first, bounded_detail("t1", "r1")));
    assert!(state.runs.is_empty());
    assert!(state.apply_thread_detail(second, bounded_detail("t2", "r2")));
    assert_eq!(state.selected.as_deref(), Some("t2"));
    assert_eq!(state.runs[0].thread_id, "t2");
}

#[test]
fn summary_stage_allows_a_local_draft_but_blocks_before_summary() {
    let mut state = AppState::default();
    state.set_draft_text("先发一条");
    assert!(!state.composer_status().can_submit);

    state.apply_summaries(ThreadSummaries {
        sequence: 9,
        threads: Some(vec![summary("t1", 0)]),
    });
    assert!(state.selected.is_none(), "the non-blank local draft stays shown");
    assert!(state.composer_status().can_submit);
    let create = state.submit().expect("summary is enough for a new thread");
    assert_eq!(create.kind, CommandKind::CreateThread);
    assert!(state.submit().is_none(), "the pending receipt prevents a duplicate");
}

#[test]
fn complete_detail_can_send_early_but_waits_after_an_accepted_receipt() {
    let mut state = AppState::default();
    state.apply_summaries(ThreadSummaries {
        sequence: 7,
        threads: Some(vec![summary("t1", 0)]),
    });
    let token = state.begin_detail_load("t1");
    assert!(state.apply_thread_detail(token, detail_variant("t1", "r1", 7, false)));
    state.set_draft_text("早发消息");
    let send = state.submit().expect("complete detail is authoritative enough");
    assert_eq!(send.kind, CommandKind::SendMessage);
    assert!(state.submit().is_none(), "the in-flight receipt is single-shot");

    assert!(state
        .on_command_result(
            &send.command_id,
            Ok(Receipt {
                accepted: true,
                thread_id: "t1".into(),
                ..Receipt::default()
            }),
        )
        .is_none());
    state.set_draft_text("不要重复发");
    assert!(!state.composer_status().can_submit);
    assert!(state.submit().is_none(), "wait for detail/snapshot after receipt");
}

#[test]
fn truncated_detail_stays_blocked_unless_same_sequence_proves_idle() {
    let mut state = AppState::default();
    state.apply_summaries(ThreadSummaries {
        sequence: 7,
        threads: Some(vec![summary("t1", 1)]),
    });
    let token = state.begin_detail_load("t1");
    assert!(state.apply_thread_detail(token, detail_variant("t1", "r1", 7, true)));
    state.set_draft_text("活跃未知");
    assert!(!state.composer_status().can_submit);

    state.apply_summaries(ThreadSummaries {
        sequence: 8,
        threads: Some(vec![summary("t1", 0)]),
    });
    let token = state.begin_detail_load("t1");
    assert!(state.apply_thread_detail(token, detail_variant("t1", "r2", 8, true)));
    state.set_draft_text("摘要确认空闲");
    assert!(state.composer_status().can_submit);
}

#[test]
fn request_kinds_are_told_apart() {
    let mut approval = request("q1", "select", "允许执行 bash？");
    approval.options = vec!["允许本次".into(), "本会话允许 bash".into(), "拒绝".into()];
    assert_eq!(approval_view(&approval).kind, RequestKind::Approval);
    assert_eq!(approval_view(&request("q2", "confirm", "继续？")).kind, RequestKind::Approval);
    let mut choice = request("q3", "select", "选一个");
    choice.options = vec!["甲".into(), "乙".into()];
    assert_eq!(approval_view(&choice).kind, RequestKind::Choice);
    assert_eq!(approval_view(&request("q4", "input", "你的名字？")).kind, RequestKind::Text);
    assert_eq!(approval_view(&request("q5", "input", "请输入 GitHub Token")).kind, RequestKind::Secret);
    assert_eq!(approval_view(&request("q6", "input", "sudo 密码")).kind, RequestKind::Secret);
    let terminal = request("q7", "editor", "交互终端 · npm login");
    assert_eq!(approval_view(&terminal).kind, RequestKind::Terminal);
    assert_eq!(approval_view(&request("q8", "editor", "编辑提交说明")).kind, RequestKind::Text);
}

#[test]
fn a_prompt_inside_the_secret_tool_is_a_secret_and_never_prefilled() {
    let mut ask = request("q1", "input", "数据库连接串");
    ask.prefill = "postgres://user:hunter2@db".into();
    let plain = with_requests(vec![ask.clone()], Vec::new());
    let shown = plain.modal_request().unwrap();
    assert_eq!(shown.kind, RequestKind::Text);
    assert_eq!(shown.prefill, ask.prefill);

    let tool = ToolCall {
        id: "tool1".into(),
        thread_id: "t1".into(),
        run_id: "r1".into(),
        name: approval::SECRET_TOOL.into(),
        status: "running".into(),
        ..ToolCall::default()
    };
    let mut state = with_requests(vec![ask], vec![tool.clone()]);
    assert!(state.inside_secret_tool("r1"));
    let shown = state.modal_request().unwrap();
    assert_eq!(shown.kind, RequestKind::Secret);
    assert!(shown.prefill.is_empty(), "a secret is never prefilled");

    // The answer is the typed value; nothing of it lands in a notice.
    let command = state.respond("q1", &ApprovalDecision::Submit("s3cr3t".into())).unwrap();
    assert_eq!(command.value, "s3cr3t");
    assert!(state.notice.as_ref().is_none_or(|n| !n.text.contains("s3cr3t")));

    // Once the tool finished, a later prompt is plain again.
    let mut done = tool;
    done.status = "completed".into();
    let state = with_requests(vec![request("q2", "input", "备注")], vec![done]);
    assert!(!state.inside_secret_tool("r1"));
}

#[test]
fn starting_the_local_core_shows_then_settles() {
    let mut state = AppState::default();
    state.connecting("本机 Core");
    state.starting_core("/app/vantry-service ensure-core");
    assert!(matches!(state.connection, Connection::Starting { .. }));
    assert_eq!(state.connection.label(), "正在启动本地 Core…");
    state.apply_snapshot(Snapshot::default());
    state.on_live();
    assert_eq!(state.connection, Connection::Live);
    assert_eq!(state.notice.as_ref().unwrap().text, "本地 Core 已启动。");

    // A failed start stays on screen while the connection retries.
    let mut state = AppState::default();
    state.starting_core("x");
    state.on_core_start_failed("没有找到 vantry-service");
    assert_eq!(state.connection, Connection::Connecting);
    state.on_snapshot_failed(&BridgeError::unavailable("connection refused"));
    let notice = state.notice.clone().unwrap();
    assert_eq!(notice.level, NoticeLevel::Error);
    assert!(notice.text.contains("vantry-service"));
    // Without such an error, the retry says so.
    let mut state = AppState::default();
    state.on_snapshot_failed(&BridgeError::unavailable("connection refused"));
    assert_eq!(state.notice.unwrap().level, NoticeLevel::Warn);
}

#[test]
fn file_search_drops_stale_answers() {
    let mut search = FileSearch::default();
    let first = search.set_query("ma");
    let second = search.set_query("main");
    assert!(search.loading && search.active());
    search.on_results(first, Ok(vec![SearchHit { path: "stale".into(), ..SearchHit::default() }]));
    assert!(search.results.is_empty() && search.loading, "an older query's answer is dropped");
    search.on_results(second, Ok(vec![SearchHit { path: "src/main.rs".into(), ..SearchHit::default() }]));
    assert_eq!(search.results[0].path, "src/main.rs");
    assert!(!search.loading);
    let third = search.set_query("x");
    search.on_results(third, Err("io".into()));
    assert_eq!(search.error.as_deref(), Some("io"));
    search.set_query("  ");
    assert!(!search.active() && search.results.is_empty() && !search.loading);
}

#[test]
fn git_branches_and_discards() {
    let mut git = GitPanel::default();
    assert!(git.other_branches().is_empty());
    git.on_status(Ok(GitState {
        branch: "feat".into(),
        branches: Some(vec!["main".into(), "feat".into(), "fix".into()]),
        files: Some(vec![
            GitFile { path: "a.rs".into(), index: "M".into(), worktree: "M".into(), ..GitFile::default() },
            GitFile { path: "b.rs".into(), index: " ".into(), worktree: "M".into(), ..GitFile::default() },
            GitFile { path: "c.rs".into(), index: "M".into(), worktree: " ".into(), ..GitFile::default() },
            GitFile { path: "new.txt".into(), index: "?".into(), worktree: "?".into(), ..GitFile::default() },
        ]),
        ..GitState::default()
    }));
    assert_eq!(git.other_branches(), ["main", "fix"]);
    assert_eq!(git.discard_paths(""), ["a.rs", "b.rs", "new.txt"], "staged-only files stay");
    assert_eq!(git.discard_paths("b.rs"), ["b.rs"]);
    assert!(git.discard_paths("c.rs").is_empty());
    git.branch_menu = true;
    git.begin();
    git.on_done(Ok(None));
    assert!(!git.branch_menu, "a finished operation closes the branch list");
}

#[test]
fn thinking_rides_on_the_thread_model() {
    use crate::bridge::workbench::actions::{next_thinking, split_thinking, with_thinking};
    let mut state = with_requests(Vec::new(), Vec::new());
    let models = vec!["anthropic/opus".to_owned()];
    let levels = vec!["low".to_owned(), "high".to_owned()];
    let current = state.current_model();
    let (base, level) = split_thinking(&current, &models);
    let next = with_thinking(base, &next_thinking(level, &levels));
    let command = state.set_model(&next).unwrap();
    assert_eq!(command.model, "anthropic/opus:low");
    assert_eq!(state.current_model(), "anthropic/opus:low");
}

#[test]
fn terminal_run_payload_parses_structured_command_and_cwd() {
    let mut req = request("q9", "editor", "SSH 登录");
    req.prefill = serde_json::json!({
        "kind": "vantry_terminal_run",
        "version": 1,
        "title": "GitHub 登录",
        "command": "gh auth login",
        "cwd": "/tmp/demo"
    })
    .to_string();
    let view = approval_view(&req);
    assert_eq!(view.kind, RequestKind::Terminal);
    assert_eq!(view.headline, "GitHub 登录");
    let tr = view.terminal_run.expect("parsed terminal payload");
    assert_eq!(tr.command, "gh auth login");
    assert_eq!(tr.cwd, "/tmp/demo");
}

#[test]
fn visual_fences_preserve_arguments_while_code_keeps_language() {
    use super::markdown::{Block, MarkdownDoc};
    let doc = MarkdownDoc::parse(
        "```ask multi\n选择方案\n* A | 推荐\n- B\n```\n\n```chart bar unit=ms\nRedis | 12\n```\n\n```rust no_run\nfn main() {}\n```\n",
    );
    assert_eq!(doc.blocks.len(), 3);
    assert!(matches!(&*doc.blocks[0].block, Block::Code { lang, .. } if lang == "ask multi"));
    assert!(matches!(&*doc.blocks[1].block, Block::Code { lang, .. } if lang == "chart bar unit=ms"));
    assert!(matches!(&*doc.blocks[2].block, Block::Code { lang, .. } if lang == "rust"));
}
