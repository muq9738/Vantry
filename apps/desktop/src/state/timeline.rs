//! A thread's timeline: runs laid out as rows with stable ids, kept
//! incrementally per run.
//!
//! [`TimelineIndex`] holds every run's rows (its *segment*) and, per thread,
//! the order of its runs. When the Core supplies causal [`RunBlock`]s ordered
//! by their monotonic `seq` axis, every row — user input, multi-segment
//! reasoning, grouped tool calls, streaming text, mid-run steers, approvals,
//! and terminal status — is projected strictly in `seq` order without slicing
//! `run.output` by `output_offset`. When talking to an older Core that omits
//! `blocks`, a legacy fallback projects the run's fields cleanly.
//!
//! A streamed text or reasoning delta extends the run's active tail block in
//! place (re-parsing only the last Markdown block for text) without rebuilding
//! the segment or bumping [`TimelineIndex::structure`].

use std::{
    collections::{HashMap, HashSet},
    sync::Arc,
};

use serde_json::Value;
use vantry_protocol::{Input, PendingRequest, Run, RunBlock, RunBlockKind, RunStatus, ToolCall};

use super::{
    approval::{ApprovalView, approval_view},
    markdown::MarkdownDoc,
};

/// A tool call's state.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ToolStatus {
    Running,
    /// Waiting for the user's approval.
    AwaitingApproval,
    Completed,
    Failed,
    Interrupted,
}

impl ToolStatus {
    fn of(status: &str) -> ToolStatus {
        match status {
            "completed" => ToolStatus::Completed,
            "failed" => ToolStatus::Failed,
            "interrupted" => ToolStatus::Interrupted,
            _ => ToolStatus::Running,
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            ToolStatus::Running => "运行中",
            ToolStatus::AwaitingApproval => "等待确认",
            ToolStatus::Completed => "已完成",
            ToolStatus::Failed => "失败",
            ToolStatus::Interrupted => "已中断",
        }
    }
}

/// One tool call of a [`TimelineItem::Tools`] group.
#[derive(Debug, Clone, PartialEq)]
pub struct ToolEntry {
    pub id: String,
    pub name: String,
    pub summary: String,
    pub status: ToolStatus,
    /// The arguments, pretty JSON (the expanded entry).
    pub arguments: String,
    pub output: String,
}

/// What kind of work a tool did, for a group's one-line summary.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum ToolKind {
    Command,
    Read,
    Edit,
    Search,
    /// The agent's task list (`vantry_todo_write`).
    Plan,
    Other,
}

impl ToolKind {
    pub fn of(name: &str) -> ToolKind {
        if super::todo::is_todo_tool(name) {
            return ToolKind::Plan;
        }
        match name.to_ascii_lowercase().as_str() {
            "bash" | "shell" | "terminal" | "exec" | "exec_command" | "run" | "command"
            | "vantry_terminal_run" => ToolKind::Command,
            "read" | "read_file" | "view" | "cat" | "open" => ToolKind::Read,
            "write" | "edit" | "multi_edit" | "multiedit" | "apply_patch" | "patch"
            | "str_replace" | "create" => ToolKind::Edit,
            "grep" | "find" | "ls" | "glob" | "rg" | "search" | "web_search"
            | "websearch" | "tool_search" => ToolKind::Search,
            _ => ToolKind::Other,
        }
    }
}

/// One row of the timeline.
#[derive(Debug, Clone, PartialEq)]
pub enum TimelineItem {
    /// The user's initial message (the run's input). `plan`: sent in plan mode.
    User {
        id: String,
        run_id: String,
        text: Arc<str>,
        plan: bool,
        attachments: Vec<vantry_protocol::Attachment>,
    },
    /// A mid-run steering message inserted at its causal timeline position.
    Steer {
        id: String,
        run_id: String,
        text: Arc<str>,
        attachments: Vec<vantry_protocol::Attachment>,
        delivered: bool,
        error: String,
    },
    /// One reasoning/thinking segment (collapsible; a run may have several).
    Reasoning {
        id: String,
        text: Arc<String>,
        streaming: bool,
    },
    /// One reply text block, as Markdown.
    Text {
        id: String,
        run_id: String,
        text: Arc<String>,
        doc: Arc<MarkdownDoc>,
        streaming: bool,
        is_last: bool,
    },
    /// Consecutive tool calls (no reply text, reasoning, or steer between them),
    /// shown as one collapsible group. Its id is its first call's, so a call
    /// joining the group keeps the list's keys.
    Tools { id: String, calls: Vec<ToolEntry> },
    /// An unanswered (or failed) extension request, answerable in place.
    Approval { id: String, view: ApprovalView },
    /// The run's end when it did not complete cleanly.
    Status {
        id: String,
        text: String,
        error: bool,
    },
}

impl TimelineItem {
    /// Its stable id (the list's key).
    pub fn id(&self) -> &str {
        match self {
            TimelineItem::User { id, .. }
            | TimelineItem::Steer { id, .. }
            | TimelineItem::Reasoning { id, .. }
            | TimelineItem::Text { id, .. }
            | TimelineItem::Tools { id, .. }
            | TimelineItem::Approval { id, .. }
            | TimelineItem::Status { id, .. } => id,
        }
    }
}

/// One run's rows.
#[derive(Debug, Clone, PartialEq, Default)]
struct Segment {
    thread_id: String,
    created_at: String,
    items: Vec<TimelineItem>,
    /// Active streaming text row `(index, block_id)` in block-driven mode.
    tail_text: Option<(usize, String)>,
    /// Active streaming reasoning row `(index, block_id)` in block-driven mode.
    tail_reasoning: Option<(usize, String)>,
    /// Legacy fallback: index of the last text row + byte start when extending
    /// `run.output` in place.
    legacy_tail: Option<(usize, usize)>,
    /// Legacy fallback: output length the rows were built from.
    output_len: usize,
    /// When true, next rebuild must parse Markdown afresh (output was rewritten).
    fresh_text: bool,
}

/// One thread's run order and row positions.
#[derive(Debug, Clone, PartialEq, Default)]
struct ThreadRows {
    /// Run ids, oldest first.
    runs: Vec<String>,
    /// `starts[i]`: the index of run `i`'s first row; one more entry holds
    /// the total.
    starts: Vec<usize>,
    /// Bumped when the thread's row ids change.
    structure: u64,
}

/// Every run's rows and every thread's order.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct TimelineIndex {
    segments: HashMap<String, Segment>,
    threads: HashMap<String, ThreadRows>,
    /// Counts every change of any row (diagnostics, tests).
    pub revision: u64,
    /// Counts segment rebuilds (tests check the fast paths avoid them).
    pub rebuilds: u64,
}

/// What a run's rows are built from. Callers pass only rows belonging to `run.id`.
pub struct RunParts<'a> {
    pub run: &'a Run,
    pub blocks: Vec<&'a RunBlock>,
    pub tools: Vec<&'a ToolCall>,
    pub requests: Vec<&'a PendingRequest>,
    pub inputs: Vec<&'a Input>,
}

impl TimelineIndex {
    /// Rebuilds everything (a snapshot), parsing every text afresh. `parts`
    /// in the Core's order.
    pub fn rebuild_all<'a>(&mut self, parts: impl IntoIterator<Item = RunParts<'a>>) {
        self.segments.clear();
        let old_threads = std::mem::take(&mut self.threads);
        let mut order: HashMap<String, Vec<(String, String)>> = HashMap::new();
        for part in parts {
            let segment = build_segment(&part, None);
            order
                .entry(part.run.thread_id.clone())
                .or_default()
                .push((part.run.created_at.clone(), part.run.id.clone()));
            self.segments.insert(part.run.id.clone(), segment);
        }
        for (thread_id, mut runs) in order {
            // Stable: the Core's order, refined by creation time (unknown last).
            runs.sort_by(|a, b| sort_key(&a.0).cmp(sort_key(&b.0)));
            let structure = old_threads.get(&thread_id).map_or(0, |t| t.structure) + 1;
            let mut rows = ThreadRows {
                runs: runs.into_iter().map(|(_, id)| id).collect(),
                structure,
                ..ThreadRows::default()
            };
            reindex(&mut rows, &self.segments);
            self.threads.insert(thread_id, rows);
        }
        self.rebuilds += 1;
        self.revision += 1;
    }

    /// Rebuilds run `parts.run`'s rows, reusing its Markdown where text blocks
    /// kept their identity and prefix.
    pub fn refresh_run(&mut self, parts: RunParts<'_>) {
        let run = parts.run;
        let previous = self.segments.get(&run.id);
        let old_ids = previous.map(|s| ids_of(&s.items));
        let segment = build_segment(&parts, previous);
        let new_ids = ids_of(&segment.items);
        let is_new = previous.is_none();
        let thread_id = run.thread_id.clone();
        let count_changed = old_ids.as_ref().map(Vec::len) != Some(new_ids.len());
        self.segments.insert(run.id.clone(), segment);
        self.rebuilds += 1;
        self.revision += 1;
        let rows = self.threads.entry(thread_id).or_default();
        if is_new {
            let key = sort_key(&run.created_at);
            let segments = &self.segments;
            let at = rows
                .runs
                .iter()
                .rposition(|id| {
                    segments
                        .get(id)
                        .is_some_and(|s| sort_key(&s.created_at) <= key)
                })
                .map_or(0, |p| p + 1);
            rows.runs.insert(at, run.id.clone());
        }
        if is_new || old_ids.as_ref() != Some(&new_ids) {
            rows.structure += 1;
        }
        if is_new || count_changed {
            reindex(rows, &self.segments);
        }
    }

    /// Fast path (block-driven): appends `delta` to run `run_id`'s open
    /// streaming text block `block_id` in place without rebuilding or re-keying.
    pub fn append_text_block(&mut self, run_id: &str, block_id: &str, delta: &str) -> bool {
        let Some(segment) = self.segments.get_mut(run_id) else {
            return false;
        };
        let Some((idx, ref tail_id)) = segment.tail_text else {
            return false;
        };
        if tail_id != block_id {
            return false;
        }
        let Some(TimelineItem::Text { id, text, doc, .. }) = segment.items.get_mut(idx) else {
            return false;
        };
        if id != block_id {
            return false;
        }
        let text = Arc::make_mut(text);
        text.push_str(delta);
        Arc::make_mut(doc).update(text);
        self.revision += 1;
        true
    }

    /// Fast path (block-driven): appends `delta` to run `run_id`'s open
    /// streaming reasoning block `block_id` in place without rebuilding.
    pub fn append_reasoning_block(&mut self, run_id: &str, block_id: &str, delta: &str) -> bool {
        let Some(segment) = self.segments.get_mut(run_id) else {
            return false;
        };
        let Some((idx, ref tail_id)) = segment.tail_reasoning else {
            return false;
        };
        if tail_id != block_id {
            return false;
        }
        let Some(TimelineItem::Reasoning { id, text, .. }) = segment.items.get_mut(idx) else {
            return false;
        };
        if id != block_id {
            return false;
        }
        Arc::make_mut(text).push_str(delta);
        self.revision += 1;
        true
    }

    /// Legacy fast path (older Core without `RunBlock`s): extends `run`'s last
    /// text row with the output it gained; returns false if caller must refresh.
    pub fn append_output(&mut self, run: &Run) -> bool {
        let Some(segment) = self.segments.get_mut(&run.id) else {
            return false;
        };
        let Some((tail, start)) = segment.legacy_tail else {
            return false;
        };
        if run.output.len() < segment.output_len || !run.output.is_char_boundary(start) {
            return false;
        }
        let Some(TimelineItem::Text { text, doc, .. }) = segment.items.get_mut(tail) else {
            return false;
        };
        let slice = &run.output[start..];
        let buf = Arc::make_mut(text);
        buf.clear();
        buf.push_str(slice);
        Arc::make_mut(doc).update(buf);
        segment.output_len = run.output.len();
        self.revision += 1;
        true
    }

    /// Run `run_id`'s output was rewritten (not extended): its Markdown is
    /// parsed afresh on the next refresh instead of being followed.
    pub fn forget_text(&mut self, run_id: &str) {
        if let Some(segment) = self.segments.get_mut(run_id) {
            segment.tail_text = None;
            segment.legacy_tail = None;
            segment.fresh_text = true;
        }
    }

    /// Legacy fast path (older Core without `RunBlock`s): replaces run `run`'s
    /// single reasoning row in place.
    pub fn update_reasoning(&mut self, run: &Run) -> bool {
        let Some(segment) = self.segments.get_mut(&run.id) else {
            return false;
        };
        let Some(TimelineItem::Reasoning { text, .. }) = segment
            .items
            .iter_mut()
            .find(|i| matches!(i, TimelineItem::Reasoning { .. }))
        else {
            return false;
        };
        let buf = Arc::make_mut(text);
        buf.clear();
        buf.push_str(&run.reasoning);
        self.revision += 1;
        true
    }

    /// Forgets runs `ids` (a rewind, a deleted thread).
    pub fn remove_runs(&mut self, ids: &HashSet<String>) {
        let mut touched = HashSet::new();
        for id in ids {
            if let Some(segment) = self.segments.remove(id) {
                touched.insert(segment.thread_id);
            }
        }
        for thread_id in touched {
            if let Some(rows) = self.threads.get_mut(&thread_id) {
                rows.runs.retain(|r| !ids.contains(r));
                rows.structure += 1;
                reindex(rows, &self.segments);
            }
        }
        self.revision += 1;
    }

    /// Forgets thread `id` and its runs.
    pub fn remove_thread(&mut self, id: &str) {
        if let Some(rows) = self.threads.remove(id) {
            for run in rows.runs {
                self.segments.remove(&run);
            }
        }
        self.revision += 1;
    }

    /// The number of rows of `thread`.
    pub fn len(&self, thread: &str) -> usize {
        self.threads
            .get(thread)
            .and_then(|t| t.starts.last().copied())
            .unwrap_or(0)
    }

    pub fn is_empty(&self, thread: &str) -> bool {
        self.len(thread) == 0
    }

    /// Bumped when `thread`'s row ids change.
    pub fn structure(&self, thread: &str) -> u64 {
        self.threads.get(thread).map_or(0, |t| t.structure)
    }

    /// The row ids of `thread`, in order.
    pub fn keys(&self, thread: &str) -> Vec<String> {
        self.rows(thread).map(|i| i.id().to_owned()).collect()
    }

    /// Row `index` of `thread`.
    pub fn item(&self, thread: &str, index: usize) -> Option<&TimelineItem> {
        let rows = self.threads.get(thread)?;
        let run = match rows.starts.binary_search(&index) {
            Ok(mut at) => {
                while at + 1 < rows.starts.len() && rows.starts[at + 1] == index {
                    at += 1;
                }
                at
            }
            Err(at) => at.checked_sub(1)?,
        };
        let id = rows.runs.get(run)?;
        self.segments
            .get(id)?
            .items
            .get(index - rows.starts[run])
    }

    /// Every row of `thread`, in order.
    pub fn rows<'a>(&'a self, thread: &str) -> impl Iterator<Item = &'a TimelineItem> + 'a {
        let runs = self
            .threads
            .get(thread)
            .map(|t| t.runs.clone())
            .unwrap_or_default();
        runs.into_iter()
            .filter_map(move |id| self.segments.get(&id))
            .flat_map(|s| s.items.iter())
    }

    /// Run `run_id`'s rows (a child's turn in the dock).
    pub fn run_items(&self, run_id: &str) -> &[TimelineItem] {
        self.segments.get(run_id).map_or(&[], |s| s.items.as_slice())
    }

    /// The run ids of `thread`, oldest first.
    pub fn run_order(&self, thread: &str) -> &[String] {
        self.threads.get(thread).map_or(&[], |t| t.runs.as_slice())
    }

    /// One anchor per non-empty run of `thread`, oldest first, for the left
    /// turn-navigation dot rail.
    pub fn turns(&self, thread: &str) -> Vec<TurnAnchor> {
        let Some(rows) = self.threads.get(thread) else {
            return Vec::new();
        };
        let mut out = Vec::with_capacity(rows.runs.len());
        for (i, id) in rows.runs.iter().enumerate() {
            let start = rows.starts[i];
            let end = rows.starts[i + 1];
            if start == end {
                continue;
            }
            let preview = self
                .segments
                .get(id)
                .and_then(|seg| {
                    seg.items.iter().find_map(|item| match item {
                        TimelineItem::User { text, .. } => turn_preview(text),
                        _ => None,
                    })
                })
                .unwrap_or_default();
            out.push(TurnAnchor {
                run_id: id.clone(),
                row_index: start,
                preview,
            });
        }
        out
    }
}

/// One conversation turn's jump anchor in the flattened timeline.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TurnAnchor {
    pub run_id: String,
    pub row_index: usize,
    pub preview: String,
}

/// The active turn index for the left dot rail given the list's viewable row
/// range and whether the viewport is scrolled to the bottom.
pub fn active_turn(
    turns: &[TurnAnchor],
    viewable: Option<(usize, usize)>,
    at_end: bool,
) -> Option<usize> {
    if turns.is_empty() {
        return None;
    }
    if at_end {
        return Some(turns.len() - 1);
    }
    let Some((first, _)) = viewable else {
        return Some(turns.len() - 1);
    };
    Some(turns.iter().rposition(|t| t.row_index <= first).unwrap_or(0))
}

fn turn_preview(text: &str) -> Option<String> {
    let line = text.lines().map(str::trim).find(|l| !l.is_empty())?;
    let mut short: String = line.chars().take(36).collect();
    if line.chars().count() > 36 {
        short.push('…');
    }
    Some(short)
}

fn ids_of(items: &[TimelineItem]) -> Vec<String> {
    items.iter().map(|i| i.id().to_owned()).collect()
}

/// Empty creation times sort last.
fn sort_key(created_at: &str) -> &str {
    if created_at.is_empty() || created_at.starts_with("0001-") {
        "\u{10FFFF}"
    } else {
        created_at
    }
}

fn reindex(rows: &mut ThreadRows, segments: &HashMap<String, Segment>) {
    rows.starts.clear();
    let mut at = 0;
    for id in &rows.runs {
        rows.starts.push(at);
        at += segments.get(id).map_or(0, |s| s.items.len());
    }
    rows.starts.push(at);
}

/// Request statuses shown in the timeline.
fn shown_request(r: &PendingRequest) -> bool {
    matches!(r.status.as_str(), "pending" | "responding" | "failed" | super::SETTLED_ELSEWHERE)
}

fn build_tool_entry(tool: &ToolCall, has_pending_approval: bool) -> ToolEntry {
    let awaiting = tool.status == "running" && has_pending_approval;
    ToolEntry {
        id: format!("tool:{}", tool.id),
        name: tool.name.clone(),
        summary: if super::todo::is_todo_tool(&tool.name) {
            super::todo::summary(tool.arguments.as_ref())
                .unwrap_or_else(|| summarize_arguments(tool.arguments.as_ref()))
        } else {
            summarize_arguments(tool.arguments.as_ref())
        },
        status: if awaiting {
            ToolStatus::AwaitingApproval
        } else {
            ToolStatus::of(&tool.status)
        },
        arguments: pretty_arguments(tool.arguments.as_ref()),
        output: tail_lines(&tool.output, 40),
    }
}

fn status_display(status: RunStatus, error: &str) -> Option<(String, bool)> {
    match status {
        RunStatus::Waiting => Some(("等待子任务完成，结束后将自动唤醒并继续…".to_owned(), false)),
        RunStatus::Cancelled => Some(("已停止".to_owned(), false)),
        RunStatus::Interrupted => Some((
            non_empty(error, "执行中断，现场已保留；可重新发送。"),
            true,
        )),
        RunStatus::Failed => Some((non_empty(error, "执行失败"), true)),
        _ => None,
    }
}

fn status_from_str(status: &str, fallback: RunStatus) -> RunStatus {
    match status {
        "waiting_children" => RunStatus::Waiting,
        "cancelled" => RunStatus::Cancelled,
        "interrupted" => RunStatus::Interrupted,
        "failed" => RunStatus::Failed,
        "completed" => RunStatus::Completed,
        "running" => RunStatus::Running,
        "queued" => RunStatus::Queued,
        _ => fallback,
    }
}

fn reuse_markdown(previous: Option<&Segment>, id: &str, text: &str) -> Arc<MarkdownDoc> {
    if let Some(prev) = previous.filter(|s| !s.fresh_text)
        && let Some((old_text, mut doc)) = prev.items.iter().find_map(|item| match item {
            TimelineItem::Text {
                id: old_id,
                text: old_text,
                doc,
                ..
            } if old_id == id => Some((old_text.as_str(), doc.clone())),
            _ => None,
        })
        && text.starts_with(old_text)
    {
        if doc.len != text.len() {
            Arc::make_mut(&mut doc).update(text);
        }
        return doc;
    }
    Arc::new(MarkdownDoc::parse(text))
}

fn mark_last_text(items: &mut [TimelineItem]) {
    if let Some(TimelineItem::Text { is_last, .. }) = items
        .iter_mut()
        .rev()
        .find(|item| matches!(item, TimelineItem::Text { .. }))
    {
        *is_last = true;
    }
}

fn build_segment(parts: &RunParts<'_>, previous: Option<&Segment>) -> Segment {
    if !parts.blocks.is_empty() {
        build_from_blocks(parts, previous)
    } else {
        build_legacy(parts, previous)
    }
}

/// Decoupled block-driven projection: every block's order comes exclusively from
/// its monotonic `seq` axis (DP0).
fn build_from_blocks(parts: &RunParts<'_>, previous: Option<&Segment>) -> Segment {
    let run = parts.run;
    let executing = matches!(run.status, RunStatus::Queued | RunStatus::Running);
    let mut blocks: Vec<&RunBlock> = parts.blocks.clone();
    blocks.sort_by(|a, b| a.seq.cmp(&b.seq).then_with(|| a.id.cmp(&b.id)));

    let tools_by_id: HashMap<&str, &ToolCall> =
        parts.tools.iter().map(|t| (t.id.as_str(), *t)).collect();
    let requests_by_id: HashMap<&str, &PendingRequest> =
        parts.requests.iter().map(|r| (r.id.as_str(), *r)).collect();
    let has_pending_approval = parts
        .requests
        .iter()
        .any(|r| shown_request(r) && r.status == "pending");

    let mut items = Vec::with_capacity(blocks.len());
    let mut tail_text: Option<(usize, String)> = None;
    let mut tail_reasoning: Option<(usize, String)> = None;
    let mut shown_requests: HashSet<&str> = HashSet::new();
    let mut has_status_block = false;

    for block in blocks {
        match block.kind {
            RunBlockKind::User => {
                if !block.text.is_empty() || !block.attachments.is_empty() {
                    items.push(TimelineItem::User {
                        id: block.id.clone(),
                        run_id: run.id.clone(),
                        text: Arc::from(block.text.as_str()),
                        plan: run.interaction_mode == "plan",
                        attachments: block.attachments.clone(),
                    });
                }
            }
            RunBlockKind::Reasoning => {
                if !block.text.is_empty() {
                    let streaming = executing && block.status == "streaming";
                    items.push(TimelineItem::Reasoning {
                        id: block.id.clone(),
                        text: Arc::new(block.text.clone()),
                        streaming,
                    });
                    if streaming {
                        tail_reasoning = Some((items.len() - 1, block.id.clone()));
                    }
                }
            }
            RunBlockKind::Text => {
                if !block.text.trim().is_empty() {
                    let streaming = executing && block.status == "streaming";
                    let doc = reuse_markdown(previous, &block.id, &block.text);
                    items.push(TimelineItem::Text {
                        id: block.id.clone(),
                        run_id: run.id.clone(),
                        text: Arc::new(block.text.clone()),
                        doc,
                        streaming,
                        is_last: false,
                    });
                    if streaming {
                        tail_text = Some((items.len() - 1, block.id.clone()));
                    }
                }
            }
            RunBlockKind::Tool => {
                let entry = match tools_by_id.get(block.ref_id.as_str()) {
                    Some(tool) => build_tool_entry(tool, has_pending_approval),
                    None => continue,
                };
                match items.last_mut() {
                    Some(TimelineItem::Tools { calls, .. }) => calls.push(entry),
                    _ => items.push(TimelineItem::Tools {
                        id: block.id.clone(),
                        calls: vec![entry],
                    }),
                }
            }
            RunBlockKind::Steer => {
                items.push(TimelineItem::Steer {
                    id: block.id.clone(),
                    run_id: run.id.clone(),
                    text: Arc::from(block.text.as_str()),
                    attachments: block.attachments.clone(),
                    delivered: block.status == "delivered",
                    error: block.error.clone(),
                });
            }
            RunBlockKind::Approval => {
                if let Some(request) = requests_by_id.get(block.ref_id.as_str())
                    && shown_request(request)
                {
                    shown_requests.insert(request.id.as_str());
                    items.push(TimelineItem::Approval {
                        id: block.id.clone(),
                        view: approval_view(request),
                    });
                }
            }
            RunBlockKind::Status => {
                let st = status_from_str(&block.status, run.status);
                let err = if block.error.is_empty() {
                    &run.error
                } else {
                    &block.error
                };
                if let Some((text, error)) = status_display(st, err) {
                    has_status_block = true;
                    items.push(TimelineItem::Status {
                        id: block.id.clone(),
                        text,
                        error,
                    });
                }
            }
            RunBlockKind::Unspecified => {}
        }
    }

    // Any shown request not yet backed by an Approval block (e.g. optimistic/synthetic).
    for request in &parts.requests {
        if shown_request(request) && !shown_requests.contains(request.id.as_str()) {
            items.push(TimelineItem::Approval {
                id: format!("request:{}", request.id),
                view: approval_view(request),
            });
        }
    }

    if !has_status_block
        && let Some((text, error)) = status_display(run.status, &run.error)
    {
        items.push(TimelineItem::Status {
            id: format!("status:{}", run.id),
            text,
            error,
        });
    }

    mark_last_text(&mut items);

    Segment {
        thread_id: run.thread_id.clone(),
        created_at: run.created_at.clone(),
        items,
        tail_text: if executing { tail_text } else { None },
        tail_reasoning: if executing { tail_reasoning } else { None },
        legacy_tail: None,
        output_len: run.output.len(),
        fresh_text: false,
    }
}

/// Legacy fallback when talking to an older Core that does not supply `RunBlock`s.
fn build_legacy(parts: &RunParts<'_>, previous: Option<&Segment>) -> Segment {
    let run = parts.run;
    let active = !run.status.terminal();
    let executing = matches!(run.status, RunStatus::Queued | RunStatus::Running);
    let mut items = Vec::new();
    if !run.input.is_empty() || !run.attachments.is_empty() {
        items.push(TimelineItem::User {
            id: format!("user:{}", run.id),
            run_id: run.id.clone(),
            text: Arc::from(run.input.as_str()),
            plan: run.interaction_mode == "plan",
            attachments: run.attachments.clone(),
        });
    }
    if !run.reasoning.is_empty() {
        items.push(TimelineItem::Reasoning {
            id: format!("reasoning:{}", run.id),
            text: Arc::new(run.reasoning.clone()),
            streaming: active && run.output.is_empty(),
        });
    }
    let pending: Vec<&PendingRequest> = parts
        .requests
        .iter()
        .copied()
        .filter(|r| shown_request(r))
        .collect();
    let has_pending_approval = pending.iter().any(|r| r.status == "pending");

    let mut run_tools: Vec<(usize, &ToolCall)> =
        parts.tools.iter().copied().enumerate().collect();
    let offset_of = |tool: &ToolCall| -> usize {
        if tool.output_offset < 0 {
            run.output.len()
        } else {
            floor_char_boundary(&run.output, tool.output_offset as usize)
        }
    };
    run_tools.sort_by_key(|(order, tool)| (offset_of(tool), *order));
    let all_known = run_tools.iter().all(|(_, t)| t.output_offset >= 0);

    let mut cursor = 0;
    let mut segment_no = 0;
    let mut legacy_tail: Option<(usize, usize)> = None;
    let push_text = |items: &mut Vec<TimelineItem>, from: usize, to: usize, n: &mut usize| {
        let text = &run.output[from..to];
        if !text.trim().is_empty() {
            let id = format!("text:{}:{}", run.id, n);
            let doc = reuse_markdown(previous, &id, text);
            items.push(TimelineItem::Text {
                id,
                run_id: run.id.clone(),
                text: Arc::new(text.to_owned()),
                doc,
                streaming: false,
                is_last: false,
            });
        }
        *n += 1;
    };
    for (_, tool) in &run_tools {
        let at = offset_of(tool).max(cursor);
        push_text(&mut items, cursor, at, &mut segment_no);
        cursor = at;
        let entry = build_tool_entry(tool, has_pending_approval);
        match items.last_mut() {
            Some(TimelineItem::Tools { calls, .. }) => calls.push(entry),
            _ => items.push(TimelineItem::Tools {
                id: entry.id.clone(),
                calls: vec![entry],
            }),
        }
    }
    let before = items.len();
    push_text(&mut items, cursor, run.output.len(), &mut segment_no);
    if items.len() > before && all_known {
        legacy_tail = Some((items.len() - 1, cursor));
    }
    if executing && let Some(TimelineItem::Text { streaming, .. }) = items.last_mut() {
        *streaming = true;
    }

    // Legacy delivered/failed steers render as first-class Steer rows below existing content.
    let mut steers: Vec<&Input> = parts
        .inputs
        .iter()
        .copied()
        .filter(|i| i.kind == "steer" && matches!(i.status.as_str(), "delivered" | "failed"))
        .collect();
    steers.sort_by(|a, b| a.created_at.cmp(&b.created_at).then_with(|| a.id.cmp(&b.id)));
    for steer in steers {
        items.push(TimelineItem::Steer {
            id: format!("steer:{}", steer.id),
            run_id: run.id.clone(),
            text: Arc::from(steer.message.as_str()),
            attachments: steer.attachments.clone(),
            delivered: steer.status == "delivered",
            error: steer.error.clone(),
        });
    }

    for request in pending {
        items.push(TimelineItem::Approval {
            id: format!("request:{}", request.id),
            view: approval_view(request),
        });
    }
    if let Some((text, error)) = status_display(run.status, &run.error) {
        items.push(TimelineItem::Status {
            id: format!("status:{}", run.id),
            text,
            error,
        });
    }
    mark_last_text(&mut items);

    Segment {
        thread_id: run.thread_id.clone(),
        created_at: run.created_at.clone(),
        items,
        tail_text: None,
        tail_reasoning: None,
        legacy_tail: if active { legacy_tail } else { None },
        output_len: run.output.len(),
        fresh_text: false,
    }
}

fn non_empty(text: &str, fallback: &str) -> String {
    if text.trim().is_empty() {
        fallback.to_owned()
    } else {
        text.to_owned()
    }
}

/// The largest char boundary of `text` at or below `index`.
pub(crate) fn floor_char_boundary(text: &str, index: usize) -> usize {
    if index >= text.len() {
        return text.len();
    }
    let mut i = index;
    while !text.is_char_boundary(i) {
        i -= 1;
    }
    i
}

/// A tool's arguments in one line: the argument that says what it does
/// (`command`, `path`, …), else the compact JSON.
pub fn summarize_arguments(arguments: Option<&Value>) -> String {
    const KEYS: &[&str] = &[
        "command",
        "cmd",
        "path",
        "file_path",
        "filePath",
        "pattern",
        "url",
        "query",
        "task",
    ];
    let Some(arguments) = arguments else {
        return String::new();
    };
    let text = match arguments {
        Value::Object(map) => KEYS
            .iter()
            .find_map(|key| map.get(*key).and_then(Value::as_str).map(str::to_owned))
            .unwrap_or_else(|| arguments.to_string()),
        Value::String(text) => text.clone(),
        Value::Null => String::new(),
        other => other.to_string(),
    };
    let line = text.lines().next().unwrap_or("").trim();
    let more = text.lines().nth(1).is_some();
    let mut summary: String = line.chars().take(160).collect();
    if more || line.chars().count() > 160 {
        summary.push('…');
    }
    summary
}

/// A tool's arguments as indented JSON, at most 4000 characters.
fn pretty_arguments(arguments: Option<&Value>) -> String {
    let Some(arguments) = arguments.filter(|a| !a.is_null()) else {
        return String::new();
    };
    let text = serde_json::to_string_pretty(arguments).unwrap_or_default();
    match text.char_indices().nth(4000) {
        Some((cut, _)) => format!("{}…", &text[..cut]),
        None => text,
    }
}

/// The last `n` lines of `text`.
fn tail_lines(text: &str, n: usize) -> String {
    let lines: Vec<&str> = text.trim_end().lines().collect();
    if lines.len() <= n {
        return text.trim_end().to_owned();
    }
    format!("…\n{}", lines[lines.len() - n..].join("\n"))
}
