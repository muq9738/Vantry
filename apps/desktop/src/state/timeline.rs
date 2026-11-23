//! A thread's timeline: runs laid out as rows with stable ids, for a
//! virtual list keyed by them. A row keeps its id while it streams, so the
//! list updates it in place and keeps the scroll anchored.

use serde_json::Value;

use super::approval::{ApprovalView, approval_view};
use crate::bridge::wire::{ProviderRequest, Run, RunStatus, ToolCall};

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

/// One row of the timeline.
#[derive(Debug, Clone, PartialEq)]
pub enum TimelineItem {
    /// The user's message (the run's input). `plan`: sent in plan mode.
    User {
        id: String,
        text: String,
        plan: bool,
    },
    /// The model's thinking.
    Reasoning {
        id: String,
        text: String,
        streaming: bool,
    },
    /// Reply text between tool calls.
    Text {
        id: String,
        text: String,
        streaming: bool,
    },
    /// A tool call.
    Tool {
        id: String,
        name: String,
        summary: String,
        status: ToolStatus,
        output: String,
    },
    /// An unanswered (or failed) extension request, shown in place; the
    /// modal answers it.
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
            | TimelineItem::Tool { id, .. }
            | TimelineItem::Approval { id, .. }
            | TimelineItem::Status { id, .. } => id,
        }
    }
}

/// The rows of `runs` (one thread's, oldest first) with their tools and
/// requests.
pub fn build_timeline(
    runs: &[&Run],
    tools: &[ToolCall],
    requests: &[ProviderRequest],
) -> Vec<TimelineItem> {
    let mut items = Vec::new();
    for run in runs {
        push_run(&mut items, run, tools, requests);
    }
    items
}

fn push_run(
    items: &mut Vec<TimelineItem>,
    run: &Run,
    tools: &[ToolCall],
    requests: &[ProviderRequest],
) {
    let active = !run.status.terminal();
    if !run.input.is_empty() {
        items.push(TimelineItem::User {
            id: format!("user:{}", run.id),
            text: run.input.clone(),
            plan: run.interaction_mode == "plan",
        });
    }
    if !run.reasoning.is_empty() {
        items.push(TimelineItem::Reasoning {
            id: format!("reasoning:{}", run.id),
            text: run.reasoning.clone(),
            streaming: active && run.output.is_empty(),
        });
    }
    let pending: Vec<&ProviderRequest> = requests
        .iter()
        .filter(|r| {
            r.run_id == run.id && matches!(r.status.as_str(), "pending" | "responding" | "failed")
        })
        .collect();

    // Tools sit at the reply's byte offset where they started; unknown
    // offsets (older records) go after the text.
    let mut run_tools: Vec<(usize, &ToolCall)> = tools
        .iter()
        .filter(|t| t.run_id == run.id)
        .enumerate()
        .map(|(order, tool)| (order, tool))
        .collect();
    let offset_of = |tool: &ToolCall| -> usize {
        if tool.output_offset < 0 {
            run.output.len()
        } else {
            floor_char_boundary(&run.output, tool.output_offset as usize)
        }
    };
    run_tools.sort_by_key(|(order, tool)| (offset_of(tool), *order));

    let mut cursor = 0;
    let mut segment = 0;
    let push_text = |items: &mut Vec<TimelineItem>, from: usize, to: usize, segment: &mut usize| {
        let text = &run.output[from..to];
        if !text.trim().is_empty() {
            items.push(TimelineItem::Text {
                id: format!("text:{}:{}", run.id, segment),
                text: text.to_owned(),
                streaming: false,
            });
        }
        *segment += 1;
    };
    for (_, tool) in &run_tools {
        let at = offset_of(tool).max(cursor);
        push_text(items, cursor, at, &mut segment);
        cursor = at;
        let awaiting = tool.status == "running" && pending.iter().any(|r| r.status == "pending");
        items.push(TimelineItem::Tool {
            id: format!("tool:{}", tool.id),
            name: tool.name.clone(),
            summary: summarize_arguments(tool.arguments.as_ref()),
            status: if awaiting {
                ToolStatus::AwaitingApproval
            } else {
                ToolStatus::of(&tool.status)
            },
            output: tail_lines(&tool.output, 12),
        });
    }
    push_text(items, cursor, run.output.len(), &mut segment);
    // The last text row streams while the run is active.
    if active {
        if let Some(TimelineItem::Text { streaming, .. }) = items.last_mut() {
            *streaming = true;
        }
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

/// The last `n` lines of `text`.
fn tail_lines(text: &str, n: usize) -> String {
    let lines: Vec<&str> = text.trim_end().lines().collect();
    if lines.len() <= n {
        return text.trim_end().to_owned();
    }
    format!("…\n{}", lines[lines.len() - n..].join("\n"))
}
