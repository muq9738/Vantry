//! The desktop's state and every change to it, as plain Rust.
//!
//! [`AppState`] is the conversation: the Core's entities (threads, runs,
//! tools, requests, inputs, tasks), the incremental [`TimelineIndex`], the
//! drafts and the commands in flight. The UI keeps it in one signal and
//! changes it through these methods (`signal.update(|s| s.apply_event(&e))`),
//! so what the views read is always a consistent whole, and every
//! transition is tested without a window. Nothing here does I/O: intents
//! return the [`Command`] to dispatch, and results come back through
//! [`AppState::on_command_result`].
//!
//! The dock and settings have their own states ([`workbench`], [`ui`]) so a
//! terminal's output never wakes the timeline; [`persist`] saves what
//! `desktop-state.json` keeps.

mod actions;
pub mod draft;
pub use draft::{Draft, DraftMention};
pub mod approval;
pub mod markdown;
pub(crate) mod thread_links;
pub mod language;
pub mod persist;
pub mod sidebar;
pub mod timeline;
pub mod todo;
pub mod ui;
pub mod workbench;

#[cfg(test)]
mod tests;
#[cfg(test)]
mod wave2_tests;
#[cfg(test)]
mod wave4_tests;

use std::{
    collections::{BTreeMap, BTreeSet, HashSet},
    time::Duration,
};

use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;
use vantry_protocol::{
    Command, CommandKind, CoreEvent, Input, PendingRequest, Receipt, Run, RunStatus,
    RunTelemetry, Snapshot, Task, Thread, ThreadDetail, ThreadSummary, ThreadSummaries, ToolCall,
    WorkspaceSpec,
};

pub use self::{
    actions::{QueueItem, QueueItemKind},
    approval::{
        ApprovalDecision, ApprovalView, DecisionError, RequestKind, Risk, approval_view,
        approval_view_with,
    },
    timeline::{RunParts, TimelineIndex, TimelineItem, ToolEntry, ToolKind, ToolStatus},
};
use crate::bridge::{BridgeError, events, new_command_id};

/// The draft key of the unsaved new thread (it has no id until its first
/// message creates it).
pub const NEW_THREAD: &str = "";

/// `interactionMode`: how the next message runs.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum InteractionMode {
    #[default]
    Default,
    /// Plan first, change nothing.
    Plan,
}

impl InteractionMode {
    /// The command's `interactionMode` (empty inherits the thread's).
    pub fn wire(self) -> &'static str {
        match self {
            InteractionMode::Default => "",
            InteractionMode::Plan => "plan",
        }
    }

    pub fn toggled(self) -> Self {
        match self {
            InteractionMode::Default => InteractionMode::Plan,
            InteractionMode::Plan => InteractionMode::Default,
        }
    }
}

/// What sending does while the thread's run is still going.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum RunningAction {
    /// Hand the message to the running attempt (`message.steer`).
    #[default]
    Steer,
    /// Queue it after the run (`message.queue`).
    Queue,
}

/// How new threads start (the composer's model and permission choices
/// before the thread exists).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct NewThreadConfig {
    pub provider_id: String,
    pub model: String,
    pub approval_policy: String,
    pub workspace: Option<WorkspaceSpec>,
}

/// The approval policies the composer offers: wire value, label.
pub const APPROVAL_POLICIES: &[(&str, &str)] = &[
    ("auto", "自动"),
    ("auto-accept-edits", "改文件自动"),
    ("ask", "询问"),
];

/// A policy's label ("询问"), or the value itself when unknown.
pub fn policy_label(policy: &str) -> String {
    APPROVAL_POLICIES
        .iter()
        .find(|(value, _)| *value == policy)
        .map_or_else(
            || {
                if policy.is_empty() {
                    "默认权限".to_owned()
                } else {
                    policy.to_owned()
                }
            },
            |(_, label)| (*label).to_owned(),
        )
}

/// The connection to the Core.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Connection {
    /// Loading the first snapshot.
    Connecting,
    /// The local Core did not answer: `vantry-service ensure-core` runs.
    Starting { detail: String },
    /// Snapshot loaded, events streaming.
    Live,
    /// The stream broke; resubscribing after the last applied event.
    Reconnecting { attempt: u32, reason: String },
    /// The Core no longer serves our cursor: reloading the snapshot.
    Resyncing { attempt: u32, reason: String },
}

impl Connection {
    pub fn label(&self) -> &'static str {
        match self {
            Connection::Connecting => "连接中",
            Connection::Starting { .. } => "正在启动本地 Core…",
            Connection::Live => "已连接",
            Connection::Reconnecting { .. } => "重连中",
            Connection::Resyncing { .. } => "重新同步",
        }
    }

    fn attempt(&self) -> u32 {
        match self {
            Connection::Reconnecting { attempt, .. } | Connection::Resyncing { attempt, .. } => {
                *attempt
            }
            _ => 0,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NoticeLevel {
    Info,
    Warn,
    Error,
}

/// A one-line message over the composer.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Notice {
    pub level: NoticeLevel,
    pub text: String,
}

impl Notice {
    pub fn new(level: NoticeLevel, text: impl Into<String>) -> Self {
        Notice {
            level,
            text: text.into(),
        }
    }
}

/// What a pending command is for, and what to undo when it fails.
#[derive(Debug, Clone, PartialEq)]
pub enum Purpose {
    /// Creates the thread a draft's first message goes to; the message
    /// follows once it is accepted.
    CreateThread { draft_key: String, then_send: Box<Command>, draft: Draft },
    /// A message (send, queue or steer) from the draft of `draft_key`.
    Message { draft_key: String, draft: Draft },
    /// Stops a run.
    Stop,
    /// Answers an extension request.
    Respond { request_id: String },
    /// Changes one of a thread's fields; that field of `previous` comes
    /// back when it fails (other changes made meanwhile stay).
    ThreadUpdate {
        previous: Box<Thread>,
        field: &'static str,
    },
    /// Deletes a thread.
    Delete,
    /// Forks, merges back: the accepted receipt's thread is shown.
    OpenResult,
    /// Rewinds a thread.
    Rewind,
    /// Changes a queued input; `previous` copies are restored on failure.
    Queue { previous: Vec<Input> },
}

/// A command sent or waiting for a retry. A retry resends exactly this
/// command (same id, same content): the Core returns the original receipt
/// if the first try committed.
#[derive(Debug, Clone, PartialEq)]
pub struct PendingCommand {
    /// What it is about: `submit:<draft key>`, `stop:<run>`,
    /// `request:<id>`, `thread:<id>:<field>`, `queue:<input>:<action>`.
    pub key: String,
    pub command: Command,
    pub purpose: Purpose,
    pub in_flight: bool,
    /// Why its outcome is unknown (shown with a retry), when it is.
    pub unconfirmed: Option<String>,
}

/// Where the subscription goes after its stream ended.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Recovery {
    /// Subscribe again after the last applied event.
    Resubscribe { after: i64, delay: Duration },
    /// Load the snapshot again, then subscribe after it.
    Resync { delay: Duration },
}

/// What an event did.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Applied {
    Changed,
    /// At or before the applied sequence (a replay): ignored.
    Stale,
    /// Not for anything the desktop shows (or malformed): cursor only.
    Ignored,
}

/// A sidebar row's state indicator.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ThreadStatus {
    Idle,
    Running,
    NeedsApproval,
    Failed,
}

/// The composer's buttons.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ComposerStatus {
    /// The draft can be sent now.
    pub can_submit: bool,
    /// What the primary button does.
    pub primary: PrimaryAction,
    /// The current run accepts either steering or queued input.
    pub can_choose_running_action: bool,
    /// A run is active and can be stopped.
    pub can_stop: bool,
    /// A submission from this draft is on its way.
    pub sending: bool,
    /// A submission's outcome is unknown: its key, for a retry.
    pub retry: Option<String>,
    /// The composer edits a queued message.
    pub editing: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PrimaryAction {
    Send,
    Steer,
    Queue,
    /// Saves an edited queued message.
    Save,
}

impl PrimaryAction {
    pub fn label(self) -> &'static str {
        match self {
            PrimaryAction::Send => "发送",
            PrimaryAction::Steer => "立即插入",
            PrimaryAction::Queue => "下一轮",
            PrimaryAction::Save => "保存",
        }
    }
}

/// Counters of what the event stream did (diagnostics).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct FoldStats {
    pub applied: u64,
    pub stale: u64,
    pub ignored: u64,
}

/// A queued message being edited in the composer.
#[derive(Debug, Clone, PartialEq)]
pub struct QueueEdit {
    pub input_id: String,
    pub thread_id: String,
    /// The draft the composer held before; it comes back afterwards.
    pub saved: Draft,
}

/// The desktop's conversation state.
#[derive(Debug, Clone, PartialEq)]
pub struct AppState {
    /// The last applied event's sequence: the subscription's cursor.
    pub sequence: i64,
    pub threads: Vec<Thread>,
    pub runs: Vec<Run>,
    pub tools: Vec<ToolCall>,
    pub requests: Vec<PendingRequest>,
    pub inputs: Vec<Input>,
    pub tasks: Vec<Task>,
    /// Every run's rows, kept incrementally.
    pub timeline: TimelineIndex,
    /// The thread shown; `None` is the new thread's draft.
    pub selected: Option<String>,
    /// Drafts by thread id ([`NEW_THREAD`] for the new one).
    pub drafts: BTreeMap<String, Draft>,
    /// Unsent sessions in the v0.1.42 sidebar, separate from Core thread ids.
    pub local_order: Vec<String>,
    pub attachment_imports: BTreeMap<String, usize>,
    pub local_selected: Option<String>,
    local_serial: u64,
    pub running_action: RunningAction,
    pub new_thread: NewThreadConfig,
    pub queue_edit: Option<QueueEdit>,
    pub connection: Connection,
    pub snapshot_loaded: bool,
    pub notice: Option<Notice>,
    pub pending: Vec<PendingCommand>,
    /// Requests the user put aside ("稍后处理"): the modal skips them, the
    /// timeline keeps showing them.
    pub deferred: BTreeSet<String>,
    /// Results delivered in the background since this window connected.
    pub unread: BTreeSet<String>,
    /// Who the desktop talks to ("本机 Core").
    pub endpoint_label: String,
    /// The workspace the header names.
    pub workspace_label: String,
    /// Bumped when what the sidebar shows may have changed (not on output
    /// deltas), so its rows are not rebuilt per streamed character.
    pub sidebar_revision: u64,
    /// Statuses projected by the fast summaries response until the full
    /// snapshot/detail replaces them with authoritative rows.
    summary_statuses: BTreeMap<String, ThreadStatus>,
    /// Updated-at timestamps projected by the fast summaries response.
    summary_activity: BTreeMap<String, String>,
    /// Invalidates a late bounded-detail response after another selection or
    /// an authoritative snapshot wins the race.
    detail_request: u64,
    /// The summary cursor and active-run counts used to decide whether a
    /// truncated detail is safe to submit against.
    summary_sequence: Option<i64>,
    summary_active_runs: BTreeMap<String, i64>,
    /// `(thread_id, detail_sequence, truncated_runs)` for the selected detail.
    detail_readiness: Option<(String, i64, bool)>,
    /// A receipt was accepted, but no authoritative snapshot/event has
    /// arrived yet. Keep the composer blocked so a second send cannot race it.
    awaiting_refresh: BTreeSet<String>,
    /// Child threads whose bounded detail was merged in summaries mode.
    child_details: BTreeSet<String>,
    pub stats: FoldStats,
}

impl Default for AppState {
    fn default() -> Self {
        AppState {
            sequence: 0,
            threads: Vec::new(),
            runs: Vec::new(),
            tools: Vec::new(),
            requests: Vec::new(),
            inputs: Vec::new(),
            tasks: Vec::new(),
            timeline: TimelineIndex::default(),
            selected: None,
            drafts: BTreeMap::new(),
            local_order: Vec::new(),
            attachment_imports: BTreeMap::new(),
            local_selected: None,
            local_serial: 0,
            running_action: RunningAction::default(),
            new_thread: NewThreadConfig {
                provider_id: "pi".to_owned(),
                ..NewThreadConfig::default()
            },
            queue_edit: None,
            connection: Connection::Connecting,
            snapshot_loaded: false,
            notice: None,
            pending: Vec::new(),
            deferred: BTreeSet::new(),
            unread: BTreeSet::new(),
            endpoint_label: String::new(),
            workspace_label: String::new(),
            sidebar_revision: 0,
            summary_statuses: BTreeMap::new(),
            summary_activity: BTreeMap::new(),
            detail_request: 0,
            summary_sequence: None,
            summary_active_runs: BTreeMap::new(),
            detail_readiness: None,
            awaiting_refresh: BTreeSet::new(),
            child_details: BTreeSet::new(),
            stats: FoldStats::default(),
        }
    }
}

/// The delay before reconnect attempt `attempt` (1-based): 250 ms doubling,
/// at most 8 s.
pub fn reconnect_delay(attempt: u32) -> Duration {
    let exp = attempt.saturating_sub(1).min(5);
    Duration::from_millis((250u64 << exp).min(8_000))
}

/// The delay before retrying a failed full snapshot (1-based): 1 s doubling,
/// at most 30 s, so a congested relay is not saturated by back-to-back retries.
pub fn snapshot_retry_delay(attempt: u32) -> Duration {
    let exp = attempt.saturating_sub(1).min(5);
    Duration::from_millis((1_000u64 << exp).min(30_000))
}

/// A title from a first message: its first line, at most 40 characters.
pub fn title_from_message(text: &str) -> String {
    // What the text reads as: no `/skill:` prefix, thread links as their titles.
    let (_, body) = thread_links::skill_prefix(text.trim());
    let readable: String = thread_links::message_inlines(body)
        .into_iter()
        .map(|inline| match inline {
            markdown::Inline::Link { text, .. } => text,
            markdown::Inline::Text(text)
            | markdown::Inline::Strong(text)
            | markdown::Inline::Emphasis(text)
            | markdown::Inline::Strike(text)
            | markdown::Inline::Code(text) => text,
        })
        .collect();
    let line = readable.trim().lines().next().unwrap_or("").trim();
    let mut title: String = line.chars().take(40).collect();
    if line.chars().count() > 40 {
        title.push('…');
    }
    if title.is_empty() {
        "新会话".to_owned()
    } else {
        title
    }
}

/// `T` from an event's JSON: as is, else laid over `T::default()` (a newer
/// or older Core leaving out fields the desktop does not need).
pub(crate) fn decode<T: DeserializeOwned + Serialize + Default>(value: &Value) -> Option<T> {
    if let Ok(decoded) = serde_json::from_value::<T>(value.clone()) {
        return Some(decoded);
    }
    let Value::Object(fields) = value else {
        return None;
    };
    let mut base = serde_json::to_value(T::default()).ok()?;
    let Value::Object(map) = &mut base else {
        return None;
    };
    for (key, field) in fields {
        map.insert(key.clone(), field.clone());
    }
    serde_json::from_value(base).ok()
}

/// Go's zero time (`0001-01-01T00:00:00Z`) is no time.
fn clear_zero_time(time: &mut String) {
    if time.starts_with("0001-01-01") {
        time.clear();
    }
}

fn thread_from_summary(summary: ThreadSummary) -> Thread {
    Thread {
        unread_sequence: summary.unread_sequence,
        project_path: summary.project_path,
        id: summary.id,
        title: summary.title,
        created_at: summary.created_at,
        provider_id: summary.provider_id,
        model: summary.model,
        workspace: summary.workspace,
        parent_thread_id: summary.parent_thread_id,
        parent_task_id: summary.parent_task_id,
        created_by_thread_id: summary.created_by_thread_id,
        pinned: summary.pinned,
        archived: summary.archived,
        snoozed_until: summary.snoozed_until,
        approval_policy: summary.approval_policy,
        ..Thread::default()
    }
}

fn status_from_summary(summary: &ThreadSummary) -> ThreadStatus {
    if summary.pending_requests > 0 {
        ThreadStatus::NeedsApproval
    } else if summary.active_runs > 0 {
        ThreadStatus::Running
    } else if matches!(summary.latest_status, Some(RunStatus::Failed | RunStatus::Interrupted)) {
        ThreadStatus::Failed
    } else {
        ThreadStatus::Idle
    }
}

// ------------------------------------------------------------------ queries

impl AppState {
    /// The key of the shown thread's draft.
    pub fn draft_key(&self) -> String {
        self.selected
            .clone()
            .or_else(|| self.local_selected.clone())
            .unwrap_or_default()
    }

    /// The shown thread's draft.
    pub fn draft(&self) -> Draft {
        self.drafts
            .get(&self.draft_key())
            .cloned()
            .unwrap_or_default()
    }

    pub fn thread(&self, id: &str) -> Option<&Thread> {
        self.threads.iter().find(|t| t.id == id)
    }

    pub fn selected_thread(&self) -> Option<&Thread> {
        self.selected.as_deref().and_then(|id| self.thread(id))
    }

    pub fn has_conversation(&self) -> bool {
        self.selected
            .as_deref()
            .is_some_and(|id| !self.timeline.run_order(id).is_empty())
            || self
                .pending_by_key(&format!("submit:{}", self.draft_key()))
                .is_some()
    }

    pub fn current_workspace(&self) -> Option<&WorkspaceSpec> {
        match self.selected_thread() {
            Some(thread) => thread.workspace.as_ref(),
            None => self.new_thread.workspace.as_ref(),
        }
    }

    /// The checkout whose files are actually on this machine. A repository groups
    /// worktrees in the sidebar, but file and Git operations use the worktree itself.
    pub fn checkout_path(&self) -> Option<&str> {
        let workspace = self.current_workspace();
        if workspace.is_some_and(|w| !w.environment_id.is_empty()) {
            return None;
        }
        workspace
            .into_iter()
            .flat_map(|w| [w.path.as_str(), w.repository.as_str()])
            .chain(self.selected_thread().map(|t| t.project_path.as_str()))
            .map(str::trim)
            .find(|s| !s.is_empty())
    }

    /// Threads not archived, pinned first, newest first.
    pub fn visible_threads(&self) -> Vec<&Thread> {
        let mut threads: Vec<&Thread> = self.threads.iter().filter(|t| !t.archived && t.parent_thread_id.is_empty()).collect();
        threads.sort_by(|a, b| {
            b.pinned
                .cmp(&a.pinned)
                .then_with(|| b.created_at.cmp(&a.created_at))
                .then_with(|| b.id.cmp(&a.id))
        });
        threads
    }

    /// A thread's runs, oldest first.
    pub fn thread_runs(&self, thread_id: &str) -> Vec<&Run> {
        let order = self.timeline.run_order(thread_id);
        if order.len()
            == self
                .runs
                .iter()
                .filter(|r| r.thread_id == thread_id)
                .count()
        {
            return order
                .iter()
                .filter_map(|id| self.runs.iter().find(|r| r.id == *id))
                .collect();
        }
        let mut runs: Vec<&Run> = self
            .runs
            .iter()
            .filter(|r| r.thread_id == thread_id)
            .collect();
        runs.sort_by(|a, b| a.created_at.cmp(&b.created_at));
        runs
    }

    pub fn run(&self, id: &str) -> Option<&Run> {
        self.runs.iter().find(|r| r.id == id)
    }

    /// The thread's run that is not finished yet (the latest one).
    pub fn active_run(&self, thread_id: &str) -> Option<&Run> {
        self.runs
            .iter()
            .filter(|r| r.thread_id == thread_id && !r.status.terminal())
            .last()
    }

    pub fn thread_status(&self, thread_id: &str) -> ThreadStatus {
        if self
            .requests
            .iter()
            .any(|r| r.thread_id == thread_id && r.status == "pending")
        {
            return ThreadStatus::NeedsApproval;
        }
        let mut last = None;
        for run in self.runs.iter().filter(|r| r.thread_id == thread_id) {
            if !run.status.terminal() {
                return ThreadStatus::Running;
            }
            last = Some(run);
        }
        match last {
            Some(run) if matches!(run.status, RunStatus::Failed | RunStatus::Interrupted) => {
                ThreadStatus::Failed
            }
            _ => self
                .summary_statuses
                .get(thread_id)
                .copied()
                .unwrap_or(ThreadStatus::Idle),
        }
    }

    /// The shown thread's rows (tests; the view reads them by index).
    pub fn timeline(&self) -> Vec<TimelineItem> {
        match self.selected.as_deref() {
            Some(thread) => self.timeline.rows(thread).cloned().collect(),
            None => Vec::new(),
        }
    }

    /// The source text of a text row (copying).
    pub fn text_of(&self, run_id: &str, start: usize, end: usize) -> String {
        self.run(run_id)
            .and_then(|r| r.output.get(start..end.min(r.output.len())))
            .unwrap_or_default()
            .to_owned()
    }

    /// Unanswered requests, oldest first: the approval queue.
    pub fn approval_queue(&self) -> Vec<&PendingRequest> {
        let mut queue: Vec<&PendingRequest> = self
            .requests
            .iter()
            .filter(|r| r.status == "pending" || r.status == "responding")
            .collect();
        queue.sort_by(|a, b| a.created_at.cmp(&b.created_at));
        queue
    }

    /// The request the modal shows: the shown thread's first, else any
    /// thread's first, skipping those put aside.
    pub fn modal_request(&self) -> Option<ApprovalView> {
        let queue: Vec<&PendingRequest> = self
            .approval_queue()
            .into_iter()
            .filter(|r| !self.deferred.contains(&r.id))
            .collect();
        let selected = self.selected.as_deref();
        queue
            .iter()
            .find(|r| Some(r.thread_id.as_str()) == selected)
            .or_else(|| queue.first())
            .map(|r| approval_view_with(r, self.inside_secret_tool(&r.run_id)))
    }

    /// Whether run `run_id` is inside the secret-input tool now.
    pub fn inside_secret_tool(&self, run_id: &str) -> bool {
        self.tools.iter().any(|t| {
            t.run_id == run_id
                && t.name == approval::SECRET_TOOL
                && !matches!(t.status.as_str(), "completed" | "failed" | "error" | "cancelled")
        })
    }

    fn pending_by_key(&self, key: &str) -> Option<&PendingCommand> {
        self.pending.iter().find(|p| p.key == key)
    }

    /// Whether the command with `key` is on its way.
    pub fn in_flight(&self, key: &str) -> bool {
        self.pending_by_key(key).is_some_and(|p| p.in_flight)
    }

    /// Whether any command whose key starts with `prefix` is pending.
    pub fn busy(&self, prefix: &str) -> bool {
        self.pending.iter().any(|p| p.key.starts_with(prefix))
    }

    /// Whether a selected existing thread has enough projection/detail data
    /// to accept an early command while the full snapshot is still loading.
    /// Complete detail is always safe; truncated detail is safe only when its
    /// sequence is exactly the summary sequence and both sources say no run is
    /// active.
    fn early_ready_for(&self, thread_id: &str) -> bool {
        if self.snapshot_loaded {
            return true;
        }
        if self.awaiting_refresh.contains(thread_id) {
            return false;
        }
        // A child shown in the drawer beside its parent: its detail was merged
        // and the live stream keeps it current, as for a truncated selection.
        if self.child_details.contains(thread_id) {
            return matches!(self.connection, Connection::Live);
        }
        let Some(summary_sequence) = self.summary_sequence else {
            return false;
        };
        let Some((detail_id, detail_sequence, truncated)) = &self.detail_readiness else {
            return false;
        };
        if detail_id != thread_id || *detail_sequence < summary_sequence {
            return false;
        }
        if !truncated || matches!(self.connection, Connection::Live) {
            return true;
        }
        *detail_sequence == summary_sequence
            && self.summary_active_runs.get(thread_id) == Some(&0)
            && self.active_run(thread_id).is_none()
    }

    fn early_submit_ready(&self) -> bool {
        if self.snapshot_loaded {
            return true;
        }
        match self.selected.as_deref() {
            Some(thread_id) => self.early_ready_for(thread_id),
            // A local draft has no Core thread/run state to race. It becomes
            // safe as soon as the summaries response established the Core
            // sequence; `apply_summaries` preserves a non-blank new draft.
            None => self.summary_sequence.is_some(),
        }
    }

    /// Pending commands whose outcome is unknown (retry / give up).
    pub fn unconfirmed(&self) -> Vec<&PendingCommand> {
        self.pending
            .iter()
            .filter(|p| !p.in_flight && p.unconfirmed.is_some())
            .collect()
    }

    pub fn composer_status(&self) -> ComposerStatus {
        let draft = self.draft();
        let editing = self
            .queue_edit
            .as_ref()
            .is_some_and(|e| Some(&e.thread_id) == self.selected.as_ref());
        let submit_key = format!("submit:{}", self.draft_key());
        let pending = self.pending_by_key(&submit_key);
        let active = self.selected.as_deref().and_then(|id| self.active_run(id));
        let primary = if editing {
            PrimaryAction::Save
        } else {
            match active {
                Some(run) if run.status == RunStatus::Running => match self.running_action {
                    RunningAction::Steer => PrimaryAction::Steer,
                    RunningAction::Queue => PrimaryAction::Queue,
                },
                Some(_) => PrimaryAction::Queue,
                None => PrimaryAction::Send,
            }
        };
        let has_content = !draft.text.trim().is_empty()
            || (!editing && !draft.attachments.is_empty() && primary == PrimaryAction::Send);
        ComposerStatus {
            can_submit: has_content
                && (editing || pending.is_none())
                && self.early_submit_ready()
                && !self.attachment_imports.contains_key(&self.draft_key()),
            primary,
            can_choose_running_action: !editing && active.is_some_and(|run| run.status == RunStatus::Running),
            can_stop: active.is_some_and(|run| !self.in_flight(&format!("stop:{}", run.id))),
            sending: !editing && pending.is_some_and(|p| p.in_flight),
            retry: pending
                .filter(|p| !p.in_flight && p.unconfirmed.is_some())
                .map(|p| p.key.clone()),
            editing,
        }
    }

    /// The model the composer shows: the thread's, or the new thread's.
    pub fn current_model(&self) -> String {
        match self.selected_thread() {
            Some(thread) => thread.model.clone(),
            None => self.new_thread.model.clone(),
        }
    }

    /// The approval policy the composer shows.
    pub fn current_policy(&self) -> String {
        match self.selected_thread() {
            Some(thread) if thread.approval_policy.is_empty() => "auto".to_owned(),
            Some(thread) => thread.approval_policy.clone(),
            None => self.new_thread.approval_policy.clone(),
        }
    }

    /// Rebuilds run `run_id`'s rows.
    fn refresh_run(&mut self, run_id: &str) {
        let Some(run) = self.runs.iter().find(|r| r.id == run_id) else {
            return;
        };
        let parts = RunParts {
            run,
            tools: self.tools.iter().filter(|t| t.run_id == run_id).collect(),
            requests: self
                .requests
                .iter()
                .filter(|r| r.run_id == run_id)
                .collect(),
        };
        self.timeline.refresh_run(parts);
    }

    fn rebuild_timeline(&mut self) {
        let mut timeline = std::mem::take(&mut self.timeline);
        let mut tools_by_run = std::collections::HashMap::<&str, Vec<&ToolCall>>::new();
        for tool in &self.tools {
            tools_by_run
                .entry(tool.run_id.as_str())
                .or_default()
                .push(tool);
        }
        let mut requests_by_run =
            std::collections::HashMap::<&str, Vec<&PendingRequest>>::new();
        for request in &self.requests {
            requests_by_run
                .entry(request.run_id.as_str())
                .or_default()
                .push(request);
        }
        timeline.rebuild_all(self.runs.iter().map(|run| RunParts {
            run,
            tools: tools_by_run.remove(run.id.as_str()).unwrap_or_default(),
            requests: requests_by_run.remove(run.id.as_str()).unwrap_or_default(),
        }));
        self.timeline = timeline;
    }
}

// ------------------------------------------------------------------ folding

impl AppState {
    /// Replaces the Core's entities with `snapshot`: the first load and every
    /// resync. Local intents (drafts, pending commands) stay.
    pub fn apply_summaries(&mut self, summaries: ThreadSummaries) {
        self.sequence = summaries.sequence;
        let summaries = summaries.threads.unwrap_or_default();
        self.threads = summaries.iter().cloned().map(thread_from_summary).collect();
        self.runs.clear();
        self.tools.clear();
        self.requests.clear();
        self.inputs.clear();
        self.tasks.clear();
        self.timeline = TimelineIndex::default();
        self.summary_statuses = summaries
            .iter()
            .map(|summary| (summary.id.clone(), status_from_summary(summary)))
            .collect();
        self.summary_activity = summaries
            .iter()
            .map(|summary| (summary.id.clone(), summary.updated_at.clone()))
            .collect();
        self.detail_request = self.detail_request.wrapping_add(1);
        self.summary_sequence = Some(self.sequence);
        self.summary_active_runs = summaries
            .iter()
            .map(|summary| (summary.id.clone(), summary.active_runs))
            .collect();
        self.detail_readiness = None;
        for thread in &mut self.threads {
            clear_zero_time(&mut thread.created_at);
            if let Some(seq) = thread.unread_sequence {
                if seq > 0 {
                    self.unread.insert(thread.id.clone());
                } else {
                    self.unread.remove(&thread.id);
                }
            }
        }
        let existing: HashSet<_> = self.threads.iter().map(|thread| thread.id.as_str()).collect();
        self.unread.retain(|id| existing.contains(id.as_str()));
        if let Some(id) = &self.selected
            && !existing.contains(id.as_str())
        {
            self.selected = None;
        }
        if self.selected.is_none()
            && self.local_selected.is_none()
            && self
                .drafts
                .get(NEW_THREAD)
                .is_none_or(|draft| draft.is_blank() && draft.mode == InteractionMode::Default)
        {
            self.selected = self.visible_threads().first().map(|t| t.id.clone());
        }
        self.snapshot_loaded = false;
        self.child_details.clear();
        self.sidebar_revision += 1;
        if self.selected.is_some() {
            self.notice = Some(Notice::new(NoticeLevel::Info, "正在加载完整会话…"));
        } else if self.notice.as_ref().is_some_and(|notice| {
            notice.text == "正在加载完整会话…" || notice.text == "正在加载会话内容…"
        }) {
            self.notice = None;
        }
    }

    /// Starts a bounded-detail request for a selected summary. The token is
    /// checked when the response returns, so switching away and back cannot
    /// let an older response overwrite the newer selection.
    pub fn begin_detail_load(&mut self, id: &str) -> u64 {
        self.detail_request = self.detail_request.wrapping_add(1);
        let request = self.detail_request;
        self.detail_readiness = None;
        self.select_thread(Some(id.to_owned()));
        self.notice = Some(Notice::new(NoticeLevel::Info, "正在加载会话内容…"));
        request
    }

    pub fn current_detail_request(&self, request: u64, id: &str) -> bool {
        self.detail_request == request && self.selected.as_deref() == Some(id)
    }

    pub fn begin_conflict_refresh(
        &mut self,
        kind: CommandKind,
        thread: &str,
        result: &Result<Receipt, BridgeError>,
    ) -> Option<u64> {
        if self.selected.as_deref() == Some(thread)
            && matches!(
                kind,
                CommandKind::SteerMessage | CommandKind::SteerRun | CommandKind::StopRun
            )
            && result
                .as_ref()
                .is_err_and(|e| e.code == crate::bridge::ErrorCode::Conflict)
        {
            self.detail_request = self.detail_request.wrapping_add(1);
            return Some(self.detail_request);
        }
        None
    }

    pub fn detail_load_failed(&mut self, request: u64, id: &str, error: &BridgeError) {
        if self.current_detail_request(request, id) {
            self.notice = Some(Notice::new(
                NoticeLevel::Error,
                format!("无法加载会话内容：{}", error.message),
            ));
        }
    }

    /// Applies bounded detail only when it still belongs to the current
    /// selection. The later full snapshot remains the authoritative source.
    pub fn apply_thread_detail(&mut self, request: u64, detail: ThreadDetail) -> bool {
        if !self.current_detail_request(request, &detail.thread.id)
            || detail.sequence < self.sequence
        {
            return false;
        }
        let ThreadDetail {
            sequence,
            mut thread,
            runs,
            tasks,
            inputs,
            tools,
            requests,
            truncated_runs,
            ..
        } = detail;
        clear_zero_time(&mut thread.created_at);
        if let Some(existing) = self.threads.iter_mut().find(|t| t.id == thread.id) {
            *existing = thread.clone();
        } else {
            self.threads.push(thread.clone());
        }
        let id = thread.id;
        self.detail_readiness = Some((id.clone(), sequence, truncated_runs));
        self.summary_statuses.remove(&id);
        self.summary_activity.remove(&id);
        self.runs.retain(|run| run.thread_id != id);
        self.tools.retain(|tool| tool.thread_id != id);
        self.requests.retain(|request| request.thread_id != id);
        self.inputs.retain(|input| input.thread_id != id);
        self.tasks.retain(|task| task.parent_thread_id != id && task.child_thread_id != id);
        let mut runs = runs.unwrap_or_default();
        for run in &mut runs {
            clear_zero_time(&mut run.created_at);
        }
        self.runs.extend(runs);
        self.tools.extend(tools.unwrap_or_default());
        self.requests.extend(requests.unwrap_or_default());
        self.inputs.extend(inputs.unwrap_or_default());
        self.tasks.extend(tasks.unwrap_or_default());
        // Keep `self.sequence` anchored to `summary_sequence` when summaries
        // already established the event stream cursor, so subscribing after
        // `self.sequence` will not skip events for other threads that landed
        // between `/v1/threads/summaries` and `/v1/threads/detail`.
        if self.summary_sequence.is_none() {
            self.sequence = self.sequence.max(sequence);
        }
        if self.notice.as_ref().is_some_and(|notice| {
            notice.text == "正在加载完整会话…" || notice.text == "正在加载会话内容…"
        }) {
            self.notice = None;
        }
        self.rebuild_timeline();
        self.sidebar_revision += 1;
        true
    }

    /// Whether opening child `child` needs its bounded detail: summaries
    /// mode carries only the selected thread's runs and tools.
    pub fn needs_child_detail(&self, child: &str) -> bool {
        !self.snapshot_loaded && !child.is_empty() && !self.child_details.contains(child)
    }

    /// Merges a child thread's bounded detail without touching the
    /// selection: the parent stays shown, rows the event stream already
    /// carries are kept (runs join, the newer text wins).
    pub fn merge_child_detail(&mut self, detail: ThreadDetail) -> bool {
        if self.snapshot_loaded || self.selected.as_deref() == Some(detail.thread.id.as_str()) {
            return false;
        }
        let ThreadDetail {
            mut thread,
            runs,
            tasks,
            inputs,
            tools,
            requests,
            ..
        } = detail;
        clear_zero_time(&mut thread.created_at);
        let id = thread.id.clone();
        if !self.threads.iter().any(|t| t.id == id) {
            self.threads.push(thread);
        }
        for mut run in runs.unwrap_or_default() {
            clear_zero_time(&mut run.created_at);
            match self.runs.iter_mut().find(|r| r.id == run.id) {
                Some(existing) => merge_run(existing, run),
                None => self.runs.push(run),
            }
        }
        for tool in tools.unwrap_or_default() {
            if !self.tools.iter().any(|t| t.id == tool.id) {
                self.tools.push(tool);
            }
        }
        for request in requests.unwrap_or_default() {
            if !self.requests.iter().any(|r| r.id == request.id) {
                self.requests.push(request);
            }
        }
        for input in inputs.unwrap_or_default() {
            if !self.inputs.iter().any(|i| i.id == input.id) {
                self.inputs.push(input);
            }
        }
        for task in tasks.unwrap_or_default() {
            if !self.tasks.iter().any(|t| t.id == task.id) {
                self.tasks.push(task);
            }
        }
        self.child_details.insert(id);
        self.rebuild_timeline();
        self.sidebar_revision += 1;
        true
    }

    pub fn apply_snapshot(&mut self, snapshot: Snapshot) {
        self.sequence = snapshot.sequence;
        self.threads = snapshot.threads.unwrap_or_default();
        self.runs = snapshot.runs.unwrap_or_default();
        self.tasks = snapshot.tasks.unwrap_or_default();
        self.tools = snapshot.tools;
        self.requests = snapshot.requests;
        self.inputs = snapshot.inputs;
        self.summary_statuses.clear();
        self.summary_activity.clear();
        self.detail_request = self.detail_request.wrapping_add(1);
        self.summary_sequence = None;
        self.summary_active_runs.clear();
        self.detail_readiness = None;
        self.awaiting_refresh.clear();
        for thread in &mut self.threads {
            clear_zero_time(&mut thread.created_at);
            if let Some(seq) = thread.unread_sequence {
                if seq > 0 {
                    self.unread.insert(thread.id.clone());
                } else {
                    self.unread.remove(&thread.id);
                }
            }
        }
        let existing: HashSet<_> = self.threads.iter().map(|thread| thread.id.as_str()).collect();
        self.unread.retain(|id| existing.contains(id.as_str()));
        for run in &mut self.runs {
            clear_zero_time(&mut run.created_at);
        }
        self.snapshot_loaded = true;
        if self.notice.as_ref().is_some_and(|notice| {
            notice.text == "正在加载完整会话…" || notice.text == "正在加载会话内容…"
        }) {
            self.notice = None;
        }
        self.rebuild_timeline();
        self.sidebar_revision += 1;
        if let Some(id) = &self.selected
            && self.thread(id).is_none()
        {
            self.selected = None;
        }
        if self.selected.is_none()
            && self.local_selected.is_none()
            && self
                .drafts
                .get(NEW_THREAD)
                .is_none_or(|d| d.is_blank() && d.mode == InteractionMode::Default)
        {
            self.selected = self.visible_threads().first().map(|t| t.id.clone());
        }
        if self.selected.is_none()
            && self.local_selected.is_none()
            && self.pending_by_key("submit:").is_none()
        {
            self.new_thread();
        }
        if let Some(edit) = &self.queue_edit
            && !self
                .inputs
                .iter()
                .any(|i| i.id == edit.input_id && i.is_queued())
        {
            self.end_queue_edit();
        }
    }

    /// Folds one event. Deltas are appended exactly once: an event at or
    /// before the applied sequence is a replay and changes nothing.
    pub fn apply_event(&mut self, event: &CoreEvent) -> Applied {
        if event.sequence <= self.sequence {
            self.stats.stale += 1;
            return Applied::Stale;
        }
        // The cursor follows every committed event, shown or not, so a
        // resubscription never replays what was already seen.
        self.sequence = event.sequence;
        if !event.thread_id.is_empty() {
            self.awaiting_refresh.remove(&event.thread_id);
            if let Some((detail_id, detail_seq, _)) = &mut self.detail_readiness
                && detail_id == &event.thread_id
            {
                // The detail can be ahead of the global cursor. Its run and tool
                // projections already include these records (including output deltas).
                if event.sequence <= *detail_seq
                    && (event.r#type.starts_with("run.") || event.r#type == events::TOOL_UPDATED)
                {
                    self.stats.stale += 1;
                    return Applied::Stale;
                }
                *detail_seq = (*detail_seq).max(event.sequence);
            }
        }
        let kind = event.r#type.as_str();
        let changed = match kind {
            events::RUN_OUTPUT => self.fold_output(event),
            events::RUN_REASONING => self.fold_reasoning(event),
            events::RUN_TELEMETRY => self.fold_telemetry(event),
            events::THREAD_CREATED | events::THREAD_UPDATED => self.fold_thread(event),
            events::THREAD_DELETED => self.fold_thread_deleted(event),
            events::THREAD_REWOUND => self.fold_rewound(event),
            events::TOOL_UPDATED => self.fold_tool(event),
            events::REQUEST_UPDATED => self.fold_request(event),
            events::INPUT_UPDATED => self.fold_input(event),
            events::TASK_CREATED | events::TASK_UPDATED => self.fold_task(event),
            other => match events::run_status(other) {
                Some(status) => self.fold_run(event, Some(status)),
                None if other.starts_with("run.") => self.fold_run(event, None),
                None => false,
            },
        };
        let quiet = matches!(
            kind,
            events::RUN_OUTPUT | events::RUN_REASONING | events::RUN_TELEMETRY
        );
        if changed && !quiet {
            self.sidebar_revision += 1;
        }
        if changed {
            self.stats.applied += 1;
            Applied::Changed
        } else {
            self.stats.ignored += 1;
            Applied::Ignored
        }
    }

    fn run_mut(&mut self, id: &str) -> Option<&mut Run> {
        self.runs.iter_mut().find(|r| r.id == id)
    }

    /// `decodeOutput`: a delta (`delta` / `text` / `chunk` / bare string) is
    /// appended; a full `output` without one reconciles the run's output.
    fn fold_output(&mut self, event: &CoreEvent) -> bool {
        let (delta, full) = match &event.data {
            Value::String(text) => (Some(text.clone()), None),
            Value::Object(map) => {
                let delta = ["delta", "text", "chunk"]
                    .iter()
                    .find_map(|k| map.get(*k).and_then(Value::as_str).map(str::to_owned));
                let full = map.get("output").and_then(Value::as_str).map(str::to_owned);
                (delta, full)
            }
            _ => (None, None),
        };
        let run_id = event.run_id.clone();
        let Some(run) = self.run_mut(&run_id) else {
            return false;
        };
        let appended = match (delta, full) {
            (Some(delta), _) if !delta.is_empty() => {
                run.output.push_str(&delta);
                true
            }
            (_, Some(full)) if !full.is_empty() => {
                if run.output.starts_with(&full) {
                    return true;
                }
                // A rewrite, not an extension: rebuild the rows.
                let extends = full.starts_with(&run.output);
                run.output = full;
                if !extends {
                    self.timeline.forget_text(&run_id);
                }
                extends
            }
            (None, None) => return false,
            _ => return true,
        };
        let fast = appended && {
            let run = self.runs.iter().find(|r| r.id == run_id);
            run.is_some_and(|run| self.timeline.append_output(run))
        };
        if !fast {
            self.refresh_run(&run_id);
        }
        true
    }

    fn fold_reasoning(&mut self, event: &CoreEvent) -> bool {
        let Some(delta) = event.data.get("reasoningDelta").and_then(Value::as_str) else {
            return false;
        };
        let delta = delta.to_owned();
        let run_id = event.run_id.clone();
        match self.run_mut(&run_id) {
            Some(run) => {
                run.reasoning.push_str(&delta);
                let updated = {
                    let run = self.runs.iter().find(|r| r.id == run_id);
                    run.is_some_and(|run| self.timeline.update_reasoning(run))
                };
                if !updated {
                    self.refresh_run(&run_id);
                }
                true
            }
            None => false,
        }
    }

    fn fold_telemetry(&mut self, event: &CoreEvent) -> bool {
        let value = event.data.get("telemetry").unwrap_or(&event.data);
        let Some(telemetry) = decode::<RunTelemetry>(value) else {
            return false;
        };
        match self.run_mut(&event.run_id) {
            Some(run) => {
                run.telemetry = Some(telemetry);
                true
            }
            None => false,
        }
    }

    /// `decodeRun`: the run bare or as `{ "run": … }`.
    fn fold_run(&mut self, event: &CoreEvent, status: Option<RunStatus>) -> bool {
        let value = event.data.get("run").unwrap_or(&event.data);
        let Some(mut run) = decode::<Run>(value) else {
            return false;
        };
        if run.id.is_empty() {
            run.id = event.run_id.clone();
        }
        if run.thread_id.is_empty() {
            run.thread_id = event.thread_id.clone();
        }
        clear_zero_time(&mut run.created_at);
        clear_zero_time(&mut run.updated_at);
        // A run without a `status` field takes the event's.
        let has_status = value
            .get("status")
            .and_then(Value::as_str)
            .is_some_and(|s| !s.is_empty());
        if !has_status {
            match status {
                Some(status) => run.status = status,
                None => return false,
            }
        }
        if run.id.is_empty() || run.thread_id.is_empty() {
            return false;
        }
        let id = run.id.clone();
        if matches!(event.r#type.as_str(), "run.completed" | "run.failed" | "run.interrupted")
            && let Some(thread) = self.threads.iter_mut().find(|t| t.id == run.thread_id)
            && thread.unread_sequence.is_some()
        {
            thread.unread_sequence = Some(event.sequence);
            self.unread.insert(thread.id.clone());
        }
        if matches!(
            run.status,
            RunStatus::Completed | RunStatus::Failed | RunStatus::Interrupted
        ) && self.selected.as_deref() != Some(&run.thread_id)
            && self
                .run(&id)
                .is_none_or(|previous| !previous.status.terminal())
        {
            self.unread.insert(run.thread_id.clone());
        }
        let mut rewritten = false;
        match self.run_mut(&id) {
            Some(existing) => {
                rewritten = !run.output.is_empty()
                    && !run.output.starts_with(&existing.output)
                    && !existing.output.starts_with(&run.output);
                merge_run(existing, run);
            }
            None => self.runs.push(run),
        }
        if rewritten {
            self.timeline.forget_text(&id);
        }
        self.refresh_run(&id);
        true
    }

    fn fold_thread(&mut self, event: &CoreEvent) -> bool {
        let value = event.data.get("thread").unwrap_or(&event.data);
        let Some(mut thread) = decode::<Thread>(value) else {
            return false;
        };
        if thread.id.is_empty() {
            return false;
        }
        clear_zero_time(&mut thread.created_at);
        if let Some(seq) = thread.unread_sequence {
            if seq > 0 { self.unread.insert(thread.id.clone()); }
            else { self.unread.remove(&thread.id); }
        }
        match self.threads.iter_mut().find(|t| t.id == thread.id) {
            Some(existing) => *existing = thread,
            None => self.threads.push(thread),
        }
        true
    }

    fn fold_thread_deleted(&mut self, event: &CoreEvent) -> bool {
        let id = event
            .data
            .get("threadId")
            .and_then(Value::as_str)
            .unwrap_or(&event.thread_id)
            .to_owned();
        if id.is_empty() {
            return false;
        }
        self.threads.retain(|t| t.id != id);
        self.runs.retain(|r| r.thread_id != id);
        self.tools.retain(|t| t.thread_id != id);
        self.requests.retain(|r| r.thread_id != id);
        self.inputs.retain(|i| i.thread_id != id);
        self.drafts.remove(&id);
        self.unread.remove(&id);
        self.timeline.remove_thread(&id);
        if self.selected.as_deref() == Some(id.as_str()) {
            self.selected = None;
        }
        if self.queue_edit.as_ref().is_some_and(|e| e.thread_id == id) {
            self.queue_edit = None;
        }
        true
    }

    /// `thread.rewound`: the target run and every later one are gone.
    fn fold_rewound(&mut self, event: &CoreEvent) -> bool {
        let mut removed: HashSet<String> = event
            .data
            .get("removedRunIds")
            .and_then(Value::as_array)
            .map(|ids| {
                ids.iter()
                    .filter_map(Value::as_str)
                    .map(str::to_owned)
                    .collect()
            })
            .unwrap_or_default();
        let target = event
            .data
            .get("runId")
            .and_then(Value::as_str)
            .unwrap_or(&event.run_id)
            .to_owned();
        if removed.is_empty() && !target.is_empty() {
            // Older records: the target and what followed it.
            let order: Vec<String> = self
                .thread_runs(&event.thread_id)
                .iter()
                .map(|r| r.id.clone())
                .collect();
            if let Some(at) = order.iter().position(|id| *id == target) {
                removed.extend(order[at..].iter().cloned());
            }
        }
        if removed.is_empty() {
            return false;
        }
        self.runs.retain(|r| !removed.contains(&r.id));
        self.tools.retain(|t| !removed.contains(&t.run_id));
        self.requests.retain(|r| !removed.contains(&r.run_id));
        self.timeline.remove_runs(&removed);
        true
    }

    fn fold_tool(&mut self, event: &CoreEvent) -> bool {
        let Some(value) = event.data.get("tool") else {
            return false;
        };
        let Some(mut tool) = decode::<ToolCall>(value) else {
            return false;
        };
        if value.get("outputOffset").is_none() {
            tool.output_offset = -1;
        }
        if tool.thread_id.is_empty() {
            tool.thread_id = event.thread_id.clone();
        }
        if tool.run_id.is_empty() {
            tool.run_id = event.run_id.clone();
        }
        if tool.id.is_empty() || tool.run_id.is_empty() {
            return false;
        }
        let run_id = tool.run_id.clone();
        match self.tools.iter_mut().find(|t| t.id == tool.id) {
            Some(existing) => *existing = tool,
            None => self.tools.push(tool),
        }
        self.refresh_run(&run_id);
        true
    }

    fn fold_request(&mut self, event: &CoreEvent) -> bool {
        let Some(value) = event.data.get("request") else {
            return false;
        };
        let Some(mut request) = decode::<PendingRequest>(value) else {
            return false;
        };
        if request.thread_id.is_empty() {
            request.thread_id = event.thread_id.clone();
        }
        if request.run_id.is_empty() {
            request.run_id = event.run_id.clone();
        }
        if request.id.is_empty() {
            return false;
        }
        // The Core's word on the request ends a local "responding".
        if request.status != "pending" {
            let key = format!("request:{}", request.id);
            self.pending.retain(|p| p.key != key);
            if request.status != "responding" {
                self.deferred.remove(&request.id);
            }
        }
        let run_id = request.run_id.clone();
        match self.requests.iter_mut().find(|r| r.id == request.id) {
            Some(existing) => *existing = request,
            None => self.requests.push(request),
        }
        self.refresh_run(&run_id);
        true
    }

    /// `input.updated`: the queue's and steers' one authority.
    fn fold_input(&mut self, event: &CoreEvent) -> bool {
        let Some(value) = event.data.get("input") else {
            return false;
        };
        let Some(mut input) = decode::<Input>(value) else {
            return false;
        };
        if input.thread_id.is_empty() {
            input.thread_id = event.thread_id.clone();
        }
        if input.id.is_empty() {
            return false;
        }
        if !input.is_queued()
            && self
                .queue_edit
                .as_ref()
                .is_some_and(|e| e.input_id == input.id)
        {
            self.end_queue_edit();
            self.notice = Some(Notice::new(
                NoticeLevel::Info,
                "这条排队消息已开始或已取消，编辑已结束。",
            ));
        }
        match self.inputs.iter_mut().find(|i| i.id == input.id) {
            Some(existing) => *existing = input,
            None => self.inputs.push(input),
        }
        true
    }

    fn fold_task(&mut self, event: &CoreEvent) -> bool {
        let Some(value) = event.data.get("task") else {
            return false;
        };
        let Some(task) = decode::<Task>(value) else {
            return false;
        };
        if task.id.is_empty() {
            return false;
        }
        let parent_run_id = task.parent_run_id.clone();
        match self.tasks.iter_mut().find(|t| t.id == task.id) {
            Some(existing) => *existing = task,
            None => self.tasks.push(task),
        }
        if !parent_run_id.is_empty() && self.run(&parent_run_id).is_some() {
            self.refresh_run(&parent_run_id);
        }
        true
    }
}

/// `joinRun`: the newer record wins, but text it does not carry (an event
/// with an empty `output`) keeps what was streamed.
fn merge_run(existing: &mut Run, incoming: Run) {
    let output = if incoming.output.is_empty() || existing.output.starts_with(&incoming.output) {
        std::mem::take(&mut existing.output)
    } else {
        incoming.output.clone()
    };
    let reasoning =
        if incoming.reasoning.is_empty() || existing.reasoning.starts_with(&incoming.reasoning) {
            std::mem::take(&mut existing.reasoning)
        } else {
            incoming.reasoning.clone()
        };
    let input = if incoming.input.is_empty() {
        std::mem::take(&mut existing.input)
    } else {
        incoming.input.clone()
    };
    let created_at = if incoming.created_at.is_empty() {
        std::mem::take(&mut existing.created_at)
    } else {
        incoming.created_at.clone()
    };
    let attachments = if incoming.attachments.is_empty() {
        std::mem::take(&mut existing.attachments)
    } else {
        incoming.attachments.clone()
    };
    let telemetry = incoming.telemetry.clone().or(existing.telemetry.take());
    // A terminal status never goes back to an active one for the same attempt
    // (a stale late event), whereas a new continuation attempt on the same run
    // legitimately moves it back to queued/running/waiting.
    let new_attempt = !incoming.attempt_id.is_empty()
        && !existing.attempt_id.is_empty()
        && incoming.attempt_id != existing.attempt_id;
    let status = if !new_attempt && existing.status.terminal() && !incoming.status.terminal() {
        existing.status
    } else {
        incoming.status
    };
    *existing = Run {
        output,
        reasoning,
        input,
        created_at,
        attachments,
        telemetry,
        status,
        ..incoming
    };
}

// ------------------------------------------------------------------ intents

impl AppState {
    pub fn select_thread(&mut self, id: Option<String>) {
        let previous = self.selected.clone();
        self.local_selected = id
            .as_ref()
            .filter(|id| self.local_order.contains(id))
            .cloned();
        self.selected = id.filter(|id| self.thread(id).is_some());
        if self.selected != previous {
            self.detail_readiness = None;
        }
        let local_read_state = self.selected_thread().is_some_and(|t| t.unread_sequence.is_none());
        if local_read_state && self.selected.as_ref().is_some_and(|id| self.unread.remove(id)) {
            self.sidebar_revision += 1;
        }
        self.notice = None;
    }

    /// Opens the new thread's draft.
    pub fn new_thread(&mut self) {
        self.selected = None;
        self.detail_readiness = None;
        if let Some(previous) = self.drafts.remove(NEW_THREAD) {
            let id = self.allocate_local_draft();
            self.drafts.insert(id.clone(), previous);
            self.local_selected = Some(id);
        } else {
            self.local_selected = Some(
                self.local_order
                    .last()
                    .cloned()
                    .unwrap_or_else(|| self.allocate_local_draft()),
            );
        }
        self.notice = None;
    }

    pub fn set_draft_text(&mut self, text: &str) {
        let key = self.draft_key();
        let draft = self.drafts.entry(key).or_default();
        draft.text = text.to_owned();
        draft
            .mentions
            .retain(|m| !m.display.is_empty() && text.contains(&m.display));
    }

    pub fn toggle_mode(&mut self) {
        let key = self.draft_key();
        let draft = self.drafts.entry(key).or_default();
        draft.mode = draft.mode.toggled();
    }

    pub fn set_running_action(&mut self, action: RunningAction) {
        self.running_action = action;
    }

    pub fn dismiss_notice(&mut self) {
        self.notice = None;
    }

    /// Puts request `id` aside: the modal moves on to the next one.
    pub fn defer_request(&mut self, id: &str) {
        self.deferred.insert(id.to_owned());
    }

    /// Brings a request put aside back into the modal.
    pub fn reopen_request(&mut self, id: &str) {
        self.deferred.remove(id);
    }

    fn take_draft(&mut self) -> Draft {
        let key = self.draft_key();
        let draft = self.drafts.remove(&key).unwrap_or_default();
        // Keep the mode for the next message.
        if draft.mode != InteractionMode::Default {
            self.drafts.insert(
                key,
                Draft {
                    mode: draft.mode,
                    ..Draft::default()
                },
            );
        }
        draft
    }

    fn track(&mut self, key: String, command: Command, purpose: Purpose) -> Command {
        self.pending.retain(|p| p.key != key);
        self.pending.push(PendingCommand {
            key,
            command: command.clone(),
            purpose,
            in_flight: true,
            unconfirmed: None,
        });
        command
    }

    /// Sends the shown draft: creates the thread first when it is new; while
    /// a run is going, steers it or queues (as [`Self::running_action`]
    /// says); saves the queued message being edited. Returns the command to
    /// dispatch; the draft moves into the pending command until the Core
    /// accepts it.
    pub fn submit(&mut self) -> Option<Command> {
        let status = self.composer_status();
        if !status.can_submit {
            return None;
        }
        if status.editing {
            return self.save_queue_edit();
        }
        let draft_key = self.draft_key();
        let key = format!("submit:{draft_key}");
        let draft = self.take_draft();
        let text = draft.outgoing().trim().to_owned();
        let Some(thread_id) = self.selected.clone() else {
            let mut send = Command::new(new_command_id("send"), CommandKind::SendMessage);
            send.message = text.clone();
            send.interaction_mode = draft.mode.wire().to_owned();
            send.attachments = draft.attachments.clone();
            let mut create = Command::new(new_command_id("thread"), CommandKind::CreateThread);
            if self.new_thread.provider_id == "controlled" {
                create.title = draft.sidebar_title();
            }
            create.provider_id = self.new_thread.provider_id.clone();
            create.model = self.new_thread.model.clone();
            create.approval_policy = self.new_thread.approval_policy.clone();
            create.workspace = self.new_thread.workspace.clone();
            create.interaction_mode = draft.mode.wire().to_owned();
            self.notice = Some(Notice::new(NoticeLevel::Info, "正在创建会话…"));
            return Some(self.track(
                key,
                create,
                Purpose::CreateThread {
                    draft_key,
                    then_send: Box::new(send),
                    draft,
                },
            ));
        };
        Some(self.send_existing(thread_id, key, draft_key, draft, text))
    }

    /// Submit a child's own draft while its parent stays selected in the sidebar.
    pub fn submit_child(&mut self, child: &str) -> Option<Command> {
        let parent=self.selected.as_deref()?;
        if !self.tasks.iter().any(|t| t.parent_thread_id==parent && t.child_thread_id==child) {return None;}
        if !self.early_ready_for(child) {return None;}
        let key=format!("submit:{child}");
        if self.pending_by_key(&key).is_some() || self.drafts.get(child)?.is_blank() {return None;}
        let draft=self.drafts.remove(child)?;
        let text=draft.outgoing().trim().to_owned();
        Some(self.send_existing(child.into(),key,child.into(),draft,text))
    }

    fn send_existing(&mut self, thread_id:String, key:String, draft_key:String, draft:Draft, text:String) -> Command {
        let active = self.active_run(&thread_id).cloned();
        let mut command = match active {
            Some(run)
                if run.status == RunStatus::Running
                    && self.running_action == RunningAction::Steer =>
            {
                let mut steer = Command::new(new_command_id("steer"), CommandKind::SteerMessage);
                steer.run_id = run.id;
                steer
            }
            Some(_) => Command::new(new_command_id("queue"), CommandKind::QueueMessage),
            None => Command::new(new_command_id("send"), CommandKind::SendMessage),
        };
        command.thread_id = thread_id;
        command.message = text;
        command.interaction_mode = draft.mode.wire().to_owned();
        command.attachments = draft.attachments.clone();
        self.notice = None;
        self.track(key, command, Purpose::Message { draft_key, draft })
    }

    /// Stops the shown thread's active run.
    pub fn stop(&mut self) -> Option<Command> {
        let thread_id = self.selected.clone()?;
        self.stop_thread(&thread_id)
    }

    /// Stops thread `thread_id`'s active run (a child shown in the dock).
    pub fn stop_thread(&mut self, thread_id: &str) -> Option<Command> {
        let run = self.active_run(thread_id)?.clone();
        let key = format!("stop:{}", run.id);
        if self.in_flight(&key) {
            return None;
        }
        let mut command = Command::new(new_command_id("stop"), CommandKind::StopRun);
        command.thread_id = run.thread_id;
        command.run_id = run.id;
        Some(self.track(key, command, Purpose::Stop))
    }

    /// Answers request `request_id`. The request shows as responding until
    /// the Core's `request.updated` says what happened.
    pub fn respond(
        &mut self,
        request_id: &str,
        decision: &ApprovalDecision,
    ) -> Result<Command, DecisionError> {
        let key = format!("request:{request_id}");
        if self.in_flight(&key) {
            return Err(DecisionError::NotPending);
        }
        let request = self
            .requests
            .iter()
            .find(|r| r.id == request_id)
            .cloned()
            .ok_or(DecisionError::NotPending)?;
        let mut command = approval::respond_command(&request, decision)?;
        command.command_id = new_command_id("request");
        let run_id = request.run_id.clone();
        if let Some(r) = self.requests.iter_mut().find(|r| r.id == request_id) {
            r.status = "responding".to_owned();
        }
        self.refresh_run(&run_id);
        Ok(self.track(
            key,
            command,
            Purpose::Respond {
                request_id: request_id.to_owned(),
            },
        ))
    }

    /// The same command again, for a pending command whose outcome is
    /// unknown.
    pub fn retry(&mut self, key: &str) -> Option<Command> {
        let pending = self
            .pending
            .iter_mut()
            .find(|p| p.key == key && !p.in_flight)?;
        pending.in_flight = true;
        pending.unconfirmed = None;
        Some(pending.command.clone())
    }

    /// Gives up a pending command whose outcome is unknown, restoring what
    /// it took (the Core may still have it: the timeline will tell).
    pub fn abandon(&mut self, key: &str) {
        let Some(index) = self
            .pending
            .iter()
            .position(|p| p.key == key && !p.in_flight)
        else {
            return;
        };
        let pending = self.pending.remove(index);
        self.restore(pending.purpose);
    }

    fn restore(&mut self, purpose: Purpose) {
        match purpose {
            Purpose::CreateThread { draft_key, draft, .. } => self.restore_draft(draft_key, draft),
            Purpose::Message { draft_key, draft } => self.restore_draft(draft_key, draft),
            Purpose::Respond { request_id } => {
                let mut run_id = None;
                if let Some(r) = self.requests.iter_mut().find(|r| r.id == request_id)
                    && r.status == "responding"
                {
                    r.status = "pending".to_owned();
                    run_id = Some(r.run_id.clone());
                }
                if let Some(run_id) = run_id {
                    self.refresh_run(&run_id);
                }
            }
            Purpose::ThreadUpdate { previous, field } => {
                if let Some(t) = self.threads.iter_mut().find(|t| t.id == previous.id) {
                    match field {
                        "title" => t.title = previous.title,
                        "pin" => t.pinned = previous.pinned,
                        "archive" => t.archived = previous.archived,
                        "snooze" => t.snoozed_until = previous.snoozed_until,
                        "policy" => t.approval_policy = previous.approval_policy,
                        "model" => {
                            t.model = previous.model;
                            t.provider_id = previous.provider_id;
                        }
                        _ => *t = *previous,
                    }
                    self.sidebar_revision += 1;
                }
            }
            Purpose::Queue { previous } => {
                for input in previous {
                    if let Some(i) = self.inputs.iter_mut().find(|i| i.id == input.id) {
                        *i = input;
                    }
                }
            }
            Purpose::Stop | Purpose::Delete | Purpose::OpenResult | Purpose::Rewind => {}
        }
    }

    /// Puts an unsent draft back, unless the user wrote a new one meanwhile.
    fn restore_draft(&mut self, key: String, draft: Draft) {
        let slot = self.drafts.entry(key).or_default();
        if slot.is_blank() {
            *slot = draft;
        }
    }

    /// The outcome of dispatching command `command_id`. Returns a follow-up
    /// command to dispatch (the message after its thread was created).
    pub fn on_command_result(
        &mut self,
        command_id: &str,
        result: Result<Receipt, BridgeError>,
    ) -> Option<Command> {
        let index = self
            .pending
            .iter()
            .position(|p| p.command.command_id == command_id)?;
        match result {
            Ok(receipt) if receipt.accepted => {
                let pending = self.pending.remove(index);
                self.on_accepted(pending, receipt)
            }
            Ok(_) => {
                let pending = self.pending.remove(index);
                self.notice = Some(Notice::new(
                    NoticeLevel::Error,
                    format!("{}未被 Core 接受。", action_label(&pending.command)),
                ));
                self.restore(pending.purpose);
                None
            }
            Err(err) if err.outcome_unknown() => {
                let pending = &mut self.pending[index];
                pending.in_flight = false;
                pending.unconfirmed = Some(err.to_string());
                let label = action_label(&pending.command);
                self.notice = Some(Notice::new(
                    NoticeLevel::Warn,
                    format!("{label}结果待确认，可重试原操作。"),
                ));
                None
            }
            Err(err) => {
                let pending = self.pending.remove(index);
                self.notice = Some(Notice::new(
                    NoticeLevel::Error,
                    format!("{}失败：{}", action_label(&pending.command), err.message),
                ));
                self.restore(pending.purpose);
                None
            }
        }
    }

    fn on_accepted(&mut self, pending: PendingCommand, receipt: Receipt) -> Option<Command> {
        match pending.purpose {
            Purpose::CreateThread { draft_key, then_send, draft } => {
                let mut then_send = *then_send;
                let thread_id = receipt.thread_id.clone();
                if thread_id.is_empty() {
                    self.notice = Some(Notice::new(NoticeLevel::Error, "Core 未返回新会话 ID。"));
                    self.restore_draft(draft_key, draft);
                    return None;
                }
                self.show_new_thread(&thread_id, &pending.command);
                if !self.snapshot_loaded {
                    self.detail_readiness = Some((thread_id.clone(), self.sequence, false));
                }
                if self.selected.is_none() && self.draft_key() == draft_key {
                    self.select_thread(Some(thread_id.clone()));
                }
                self.local_order.retain(|id| id != &draft_key);
                if let Some(later_edits) = self.drafts.remove(&draft_key) {
                    self.restore_draft(thread_id.clone(), later_edits);
                }
                then_send.thread_id = thread_id.clone();
                self.notice = None;
                Some(self.track(
                    format!("submit:{thread_id}"),
                    then_send,
                    Purpose::Message {
                        draft_key: thread_id,
                        draft,
                    },
                ))
            }
            Purpose::Message { .. } => {
                if !self.snapshot_loaded {
                    let thread_id = if receipt.thread_id.is_empty() {
                        pending.command.thread_id.clone()
                    } else {
                        receipt.thread_id.clone()
                    };
                    if !thread_id.is_empty() {
                        self.awaiting_refresh.insert(thread_id);
                    }
                }
                if receipt.delivery == "queued"
                    || pending.command.kind == CommandKind::QueueMessage
                {
                    self.notice = Some(Notice::new(NoticeLevel::Info, "已加入队列。"));
                }
                None
            }
            Purpose::OpenResult => {
                let thread_id = receipt.thread_id.clone();
                if !thread_id.is_empty() {
                    self.show_new_thread(&thread_id, &pending.command);
                    self.select_thread(Some(thread_id));
                }
                self.notice = Some(Notice::new(
                    NoticeLevel::Info,
                    match pending.command.kind {
                        CommandKind::ForkThread => "已分叉为新会话。",
                        _ => "已合并回原会话。",
                    },
                ));
                None
            }
            Purpose::Rewind => {
                self.notice = Some(Notice::new(NoticeLevel::Info, "已回退到所选轮次之前。"));
                None
            }
            Purpose::Delete => {
                self.notice = Some(Notice::new(NoticeLevel::Info, "会话已删除。"));
                None
            }
            Purpose::Stop
            | Purpose::Respond { .. }
            | Purpose::ThreadUpdate { .. }
            | Purpose::Queue { .. } => None,
        }
    }

    /// A thread a receipt named may not have arrived as an event yet: show
    /// it now; its event replaces this.
    fn show_new_thread(&mut self, thread_id: &str, command: &Command) {
        if self.thread(thread_id).is_none() {
            self.threads.push(Thread {
                id: thread_id.to_owned(),
                title: command.title.clone(),
                provider_id: command.provider_id.clone(),
                model: command.model.clone(),
                approval_policy: command.approval_policy.clone(),
                workspace: command.workspace.clone(),
                ..Thread::default()
            });
            self.sidebar_revision += 1;
        }
    }
}

fn action_label(command: &Command) -> &'static str {
    match command.kind {
        CommandKind::CreateThread => "创建会话",
        CommandKind::SendMessage => "发送",
        CommandKind::QueueMessage => "排队",
        CommandKind::SteerMessage | CommandKind::SteerRun => "追加",
        CommandKind::StopRun => "停止",
        CommandKind::RespondRequest => "请求响应",
        CommandKind::UpdateThread => "更新会话",
        CommandKind::DeleteThread => "删除会话",
        CommandKind::ForkThread => "分叉",
        CommandKind::RewindThread => "回退",
        CommandKind::MergeBackThread => "合并回原会话",
        CommandKind::SetThreadModel => "切换模型",
        CommandKind::EditInput => "编辑排队消息",
        CommandKind::ReorderInput => "调整队列",
        CommandKind::PromoteInputToSteer => "改为追加",
        CommandKind::CancelInput | CommandKind::CancelMessage => "取消排队",
        _ => "操作",
    }
}

// --------------------------------------------------------------- connection

impl AppState {
    /// Before the first snapshot.
    pub fn connecting(&mut self, endpoint_label: impl Into<String>) {
        self.endpoint_label = endpoint_label.into();
        self.connection = Connection::Connecting;
    }

    /// The local Core is being started (`vantry-service ensure-core`).
    pub fn starting_core(&mut self, detail: impl Into<String>) {
        self.connection = Connection::Starting {
            detail: detail.into(),
        };
        self.notice = Some(Notice::new(NoticeLevel::Info, "正在启动本地 Core…"));
    }

    /// Starting the local Core failed: say why; the connection keeps
    /// retrying (the user may start it by hand).
    pub fn on_core_start_failed(&mut self, reason: &str) {
        self.connection = Connection::Connecting;
        self.notice = Some(Notice::new(
            NoticeLevel::Error,
            format!("无法启动本地 Core：{reason}"),
        ));
    }

    /// The snapshot is in and the subscription runs.
    pub fn on_live(&mut self) {
        if matches!(self.connection, Connection::Starting { .. }) {
            self.connection = Connection::Live;
            if self
                .notice
                .as_ref()
                .is_none_or(|notice| notice.level != NoticeLevel::Error)
            {
                self.notice = Some(Notice::new(NoticeLevel::Info, "本地 Core 已启动。"));
            }
            return;
        }
        let was_recovering = !matches!(self.connection, Connection::Live | Connection::Connecting);
        self.connection = Connection::Live;
        if was_recovering
            && self
                .notice
                .as_ref()
                .is_some_and(|n| n.level != NoticeLevel::Error)
        {
            self.notice = Some(Notice::new(
                NoticeLevel::Info,
                "已重新连接；未确认的指令不会自动重发。",
            ));
        }
    }

    /// The snapshot could not be loaded.
    pub fn on_snapshot_failed(&mut self, error: &BridgeError) -> Recovery {
        let attempt = self.connection.attempt() + 1;
        self.connection = Connection::Resyncing {
            attempt,
            reason: error.to_string(),
        };
        // An error the user must act on (the Core could not start) stays.
        if self.notice.as_ref().is_none_or(|n| n.level != NoticeLevel::Error) {
            self.notice = Some(Notice::new(
                NoticeLevel::Warn,
                format!("无法读取 Core 快照（{}），正在重试…", error.message),
            ));
        }
        Recovery::Resync {
            delay: snapshot_retry_delay(attempt),
        }
    }

    /// The event stream ended: how to recover.
    pub fn on_stream_ended(&mut self, result: Result<(), BridgeError>) -> Recovery {
        let attempt = self.connection.attempt() + 1;
        match result {
            Err(error) if error.is_resync() => {
                self.connection = Connection::Resyncing {
                    attempt,
                    reason: error.to_string(),
                };
                self.notice = Some(Notice::new(
                    NoticeLevel::Warn,
                    "事件游标已失效，正在重新同步快照…",
                ));
                Recovery::Resync {
                    delay: reconnect_delay(attempt),
                }
            }
            other => {
                let reason = match other {
                    Ok(()) => "Core 关闭了事件流".to_owned(),
                    Err(error) => error.to_string(),
                };
                self.connection = Connection::Reconnecting {
                    attempt,
                    reason: reason.clone(),
                };
                self.notice = Some(Notice::new(
                    NoticeLevel::Warn,
                    format!("与 Core 的连接中断（{reason}），正在重连…"),
                ));
                Recovery::Resubscribe {
                    after: self.sequence,
                    delay: reconnect_delay(attempt),
                }
            }
        }
    }
}
