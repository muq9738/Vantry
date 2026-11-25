//! A thread's timeline: runs laid out as rows with stable ids, kept
//! incrementally per run.
//!
//! [`TimelineIndex`] holds every run's rows (its *segment*) and, per thread,
//! the order of its runs. An event rebuilds only the segment of the run it
//! touched; a streamed output delta does less: it extends the run's last
//! text row and re-parses only that row's last Markdown block. A thread's
//! row ids (the virtual list's keys) change only when a segment's ids do,
//! which [`TimelineIndex::structure`] counts, so the list re-keys on a new
//! row, not on every delta.

use std::{
    collections::{HashMap, HashSet},
    sync::Arc,
};

use serde_json::Value;
use vantry_protocol::{PendingRequest, Run, RunStatus, ToolCall};

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
    Other,
}

impl ToolKind {
    pub fn of(name: &str) -> ToolKind {
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
    /// The user's message (the run's input). `plan`: sent in plan mode.
    User {
        id: String,
        run_id: String,
        text: Arc<str>,
        plan: bool,
        attachments: Vec<vantry_protocol::Attachment>,
    },
    /// The model's thinking (collapsible).
    Reasoning {
        id: String,
        text: Arc<str>,
        streaming: bool,
    },
    /// Reply text between tool calls, as Markdown. `start..end` is its
    /// byte range in the run's output.
    Text {
        id: String,
        run_id: String,
        start: usize,
        end: usize,
        doc: Arc<MarkdownDoc>,
        streaming: bool,
    },
    /// Consecutive tool calls (no reply text between them), shown as one
    /// collapsible group as Claude's desktop does. Its id is its first
    /// call's, so a call joining the group keeps the list's keys.
    Tools { id: String, calls: Vec<ToolEntry> },
    /// An unanswered (or failed) extension request, answerable in place.
    Approval { id: String, view: ApprovalView },
    /// The run's end when it did not complete.
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
    /// The index of the last text row when new output can extend it in
    /// place (it is the last content row and every tool's offset is known).
    tail: Option<usize>,
    /// Whether the run is active (its last text row streams).
    active: bool,
    /// The output length the rows were built from.
    output_len: usize,
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

/// What a run's rows are built from. `tools` and `requests` are already
/// grouped by `run.id`; callers must not pass rows belonging to another run.
pub struct RunParts<'a> {
    pub run: &'a Run,
    pub tools: Vec<&'a ToolCall>,
    pub requests: Vec<&'a PendingRequest>,
}

impl TimelineIndex {
    /// Rebuilds everything (a snapshot), parsing every text afresh. `parts`
    /// in the Core's order.
    pub fn rebuild_all<'a>(&mut self, parts: impl IntoIterator<Item = RunParts<'a>>) {
        self.segments.clear();
        let old_threads = std::mem::take(&mut self.threads);
        let mut order: HashMap<String, Vec<(String, String)>> = HashMap::new();
        for part in parts {
            // A snapshot's text is parsed afresh: it may differ from what
            // was streamed.
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

    /// Rebuilds run `parts.run`'s rows, reusing its Markdown where its text
    /// rows kept their place.
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
            // After every run created at or before it.
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

    /// Extends run `run`'s last text row with the output it gained, when
    /// the rows allow it; false when the caller must [`Self::refresh_run`].
    pub fn append_output(&mut self, run: &Run) -> bool {
        let Some(segment) = self.segments.get_mut(&run.id) else {
            return false;
        };
        let Some(tail) = segment.tail else {
            return false;
        };
        if run.output.len() < segment.output_len {
            return false;
        }
        let Some(TimelineItem::Text { start, end, doc, .. }) = segment.items.get_mut(tail) else {
            return false;
        };
        if !run.output.is_char_boundary(*start) {
            return false;
        }
        *end = run.output.len();
        Arc::make_mut(doc).update(&run.output[*start..]);
        segment.output_len = run.output.len();
        self.revision += 1;
        true
    }

    /// Run `run_id`'s output was rewritten (not extended): its Markdown is
    /// parsed afresh on the next refresh instead of being followed.
    pub fn forget_text(&mut self, run_id: &str) {
        if let Some(segment) = self.segments.get_mut(run_id) {
            segment.tail = None;
            for item in &mut segment.items {
                if let TimelineItem::Text { start, .. } = item {
                    *start = usize::MAX;
                }
            }
        }
    }

    /// Replaces run `run`'s reasoning text, when it has a reasoning row.
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
        *text = Arc::from(run.reasoning.as_str());
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
        // The run whose rows contain `index`.
        let run = match rows.starts.binary_search(&index) {
            Ok(mut at) => {
                // Skip runs without rows (equal starts).
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

    /// The run ids of `thread`, oldest first.
    pub fn run_order(&self, thread: &str) -> &[String] {
        self.threads.get(thread).map_or(&[], |t| t.runs.as_slice())
    }
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
    matches!(r.status.as_str(), "pending" | "responding" | "failed")
}

fn build_segment(parts: &RunParts<'_>, previous: Option<&Segment>) -> Segment {
    let run = parts.run;
    let active = !run.status.terminal();
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
            text: Arc::from(run.reasoning.as_str()),
            streaming: active && run.output.is_empty(),
        });
    }
    let pending: Vec<&PendingRequest> = parts
        .requests
        .iter()
        .copied()
        .filter(|r| shown_request(r))
        .collect();

    // Tools sit at the reply's byte offset where they started; unknown
    // offsets (older records) go after the text.
    let mut run_tools: Vec<(usize, &ToolCall)> = parts
        .tools
        .iter()
        .copied()
        .enumerate()
        .collect();
    let offset_of = |tool: &ToolCall| -> usize {
        if tool.output_offset < 0 {
            run.output.len()
        } else {
            floor_char_boundary(&run.output, tool.output_offset as usize)
        }
    };
    run_tools.sort_by_key(|(order, tool)| (offset_of(tool), *order));
    let all_known = run_tools.iter().all(|(_, t)| t.output_offset >= 0);

    let previous_doc = |id: &str, start: usize| -> Option<Arc<MarkdownDoc>> {
        previous?.items.iter().find_map(|item| match item {
            TimelineItem::Text {
                id: old, start: s, doc, ..
            } if old == id && *s == start => Some(doc.clone()),
            _ => None,
        })
    };
    let mut cursor = 0;
    let mut segment_no = 0;
    let mut tail: Option<usize> = None;
    let push_text = |items: &mut Vec<TimelineItem>, from: usize, to: usize, n: &mut usize| {
        let text = &run.output[from..to];
        if !text.trim().is_empty() {
            let id = format!("text:{}:{}", run.id, n);
            let doc = match previous_doc(&id, from) {
                Some(mut doc) => {
                    if doc.len != text.len() {
                        Arc::make_mut(&mut doc).update(text);
                    }
                    doc
                }
                None => Arc::new(MarkdownDoc::parse(text)),
            };
            items.push(TimelineItem::Text {
                id,
                run_id: run.id.clone(),
                start: from,
                end: to,
                doc,
                streaming: false,
            });
        }
        *n += 1;
    };
    for (_, tool) in &run_tools {
        let at = offset_of(tool).max(cursor);
        push_text(&mut items, cursor, at, &mut segment_no);
        cursor = at;
        let awaiting = tool.status == "running" && pending.iter().any(|r| r.status == "pending");
        let entry = ToolEntry {
            id: format!("tool:{}", tool.id),
            name: tool.name.clone(),
            summary: summarize_arguments(tool.arguments.as_ref()),
            status: if awaiting {
                ToolStatus::AwaitingApproval
            } else {
                ToolStatus::of(&tool.status)
            },
            arguments: pretty_arguments(tool.arguments.as_ref()),
            output: tail_lines(&tool.output, 40),
        };
        // A call right after another (no reply text between) joins its group.
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
    // New output extends the last row only when it is text and no tool can
    // land after it; otherwise it starts a row (a rebuild).
    if items.len() > before && all_known {
        tail = Some(items.len() - 1);
    }
    // The last text row streams while the run is active.
    if active && let Some(TimelineItem::Text { streaming, .. }) = items.last_mut() {
        *streaming = true;
    }
    for request in pending {
        items.push(TimelineItem::Approval {
            id: format!("request:{}", request.id),
            view: approval_view(request),
        });
    }
    let end = match run.status {
        RunStatus::Cancelled => Some(("已停止".to_owned(), false)),
        RunStatus::Interrupted => Some((
            non_empty(&run.error, "执行中断，现场已保留；可重新发送。"),
            true,
        )),
        RunStatus::Failed => Some((non_empty(&run.error, "执行失败"), true)),
        _ => None,
    };
    if let Some((text, error)) = end {
        items.push(TimelineItem::Status {
            id: format!("status:{}", run.id),
            text,
            error,
        });
    }
    Segment {
        thread_id: run.thread_id.clone(),
        created_at: run.created_at.clone(),
        items,
        // Only an active run's tail grows.
        tail: if active { tail } else { None },
        active,
        output_len: run.output.len(),
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
