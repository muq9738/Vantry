//! The desktop's state and every change to it, as plain Rust.
//!
//! [`AppState`] is a value: the UI keeps it in one signal and replaces it
//! through these methods (`signal.update(|s| s.apply_event(&e))`), so what
//! the views read is always a consistent whole, and every transition here
//! is tested without a window. Nothing in this module does I/O: intents
//! return the [`Command`] to dispatch, and results come back through
//! [`AppState::on_command_result`].

pub mod approval;
pub mod timeline;

#[cfg(test)]
mod tests;

use std::{
    collections::{BTreeMap, BTreeSet},
    time::Duration,
};

use serde_json::Value;

pub use self::{
    approval::{ApprovalDecision, ApprovalView, DecisionError, Risk, approval_view},
    timeline::{TimelineItem, ToolStatus, build_timeline},
};
use crate::bridge::{
    BridgeError, new_command_id,
    wire::{
        Command, Event, ProviderRequest, Receipt, Run, RunStatus, RunTelemetry, Snapshot, Thread,
        ToolCall, event, kind,
    },
};

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

/// One thread's unsent input.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Draft {
    pub text: String,
    pub mode: InteractionMode,
}

/// The connection to the Core.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Connection {
    /// Loading the first snapshot.
    Connecting,
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
    fn new(level: NoticeLevel, text: impl Into<String>) -> Self {
        Notice {
            level,
            text: text.into(),
        }
    }
}

/// What a pending command is for.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Purpose {
    /// Creates the thread a draft's first message goes to; the message
    /// follows once it is accepted.
    CreateThread { then_send: Command, draft: Draft },
    /// A message (send, queue or steer) from the draft of `draft_key`.
    Message { draft_key: String, draft: Draft },
    /// Stops a run.
    Stop,
    /// Answers an extension request.
    Respond { request_id: String },
}

/// A command sent or waiting for a retry. A retry resends exactly this
/// command (same id, same content): the Core returns the original receipt
/// if the first try committed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PendingCommand {
    /// What it is about: `submit:<draft key>`, `stop:<run>`, `request:<id>`.
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
    /// A run is active and can be stopped.
    pub can_stop: bool,
    /// A submission from this draft is on its way.
    pub sending: bool,
    /// A submission's outcome is unknown: its key, for a retry.
    pub retry: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PrimaryAction {
    Send,
    Steer,
    Queue,
}

impl PrimaryAction {
    pub fn label(self) -> &'static str {
        match self {
            PrimaryAction::Send => "发送",
            PrimaryAction::Steer => "追加",
            PrimaryAction::Queue => "排队",
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

/// The desktop's whole state.
#[derive(Debug, Clone, PartialEq)]
pub struct AppState {
    /// The last applied event's sequence: the subscription's cursor.
    pub sequence: i64,
    pub threads: Vec<Thread>,
    pub runs: Vec<Run>,
    pub tools: Vec<ToolCall>,
    pub requests: Vec<ProviderRequest>,
    /// The thread shown; `None` is the new thread's draft.
    pub selected: Option<String>,
    /// Drafts by thread id ([`NEW_THREAD`] for the new one).
    pub drafts: BTreeMap<String, Draft>,
    pub running_action: RunningAction,
    pub connection: Connection,
    pub snapshot_loaded: bool,
    pub notice: Option<Notice>,
    pub pending: Vec<PendingCommand>,
    /// Requests the user put aside ("稍后处理"): the modal skips them, the
    /// timeline keeps showing them.
    pub deferred: BTreeSet<String>,
    /// Who the desktop talks to ("本机 Core").
    pub endpoint_label: String,
    /// The workspace the header names.
    pub workspace_label: String,
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
            selected: None,
            drafts: BTreeMap::new(),
            running_action: RunningAction::default(),
            connection: Connection::Connecting,
            snapshot_loaded: false,
            notice: None,
            pending: Vec::new(),
            deferred: BTreeSet::new(),
            endpoint_label: String::new(),
            workspace_label: String::new(),
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

/// A title from a first message: its first line, at most 40 characters.
pub fn title_from_message(text: &str) -> String {
    let line = text.trim().lines().next().unwrap_or("").trim();
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

// ------------------------------------------------------------------ queries

impl AppState {
    /// The key of the shown thread's draft.
    pub fn draft_key(&self) -> String {
        self.selected
            .clone()
            .unwrap_or_else(|| NEW_THREAD.to_owned())
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

    /// Sidebar threads: not archived, newest first.
    pub fn visible_threads(&self) -> Vec<&Thread> {
        let mut threads: Vec<&Thread> = self.threads.iter().filter(|t| !t.archived).collect();
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
        let mut runs: Vec<&Run> = self
            .runs
            .iter()
            .filter(|r| r.thread_id == thread_id)
            .collect();
        // Stable: runs arrive in creation order; created_at refines it.
        runs.sort_by(|a, b| a.created_at.cmp(&b.created_at));
        runs
    }

    /// The thread's run that is not finished yet (the latest one).
    pub fn active_run(&self, thread_id: &str) -> Option<&Run> {
        self.thread_runs(thread_id)
            .into_iter()
            .rev()
            .find(|r| !r.status.terminal())
    }

    pub fn thread_status(&self, thread_id: &str) -> ThreadStatus {
        if self
            .requests
            .iter()
            .any(|r| r.thread_id == thread_id && r.status == "pending")
        {
            return ThreadStatus::NeedsApproval;
        }
        let runs = self.thread_runs(thread_id);
        if runs.iter().any(|r| !r.status.terminal()) {
            return ThreadStatus::Running;
        }
        match runs.last() {
            Some(run) if matches!(run.status, RunStatus::Failed | RunStatus::Interrupted) => {
                ThreadStatus::Failed
            }
            _ => ThreadStatus::Idle,
        }
    }

    /// The shown thread's timeline.
    pub fn timeline(&self) -> Vec<TimelineItem> {
        let Some(thread_id) = self.selected.as_deref() else {
            return Vec::new();
        };
        build_timeline(&self.thread_runs(thread_id), &self.tools, &self.requests)
    }

    /// Unanswered requests, oldest first: the approval queue.
    pub fn approval_queue(&self) -> Vec<&ProviderRequest> {
        let mut queue: Vec<&ProviderRequest> = self
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
        let queue: Vec<&ProviderRequest> = self
            .approval_queue()
            .into_iter()
            .filter(|r| !self.deferred.contains(&r.id))
            .collect();
        let selected = self.selected.as_deref();
        queue
            .iter()
            .find(|r| Some(r.thread_id.as_str()) == selected)
            .or_else(|| queue.first())
            .map(|r| approval_view(r))
    }

    fn pending_by_key(&self, key: &str) -> Option<&PendingCommand> {
        self.pending.iter().find(|p| p.key == key)
    }

    /// Whether the command with `key` is on its way.
    pub fn in_flight(&self, key: &str) -> bool {
        self.pending_by_key(key).is_some_and(|p| p.in_flight)
    }

    pub fn composer_status(&self) -> ComposerStatus {
        let draft = self.draft();
        let submit_key = format!("submit:{}", self.draft_key());
        let pending = self.pending_by_key(&submit_key);
        let active = self.selected.as_deref().and_then(|id| self.active_run(id));
        let primary = match active {
            Some(run) if run.status == RunStatus::Running => match self.running_action {
                RunningAction::Steer => PrimaryAction::Steer,
                RunningAction::Queue => PrimaryAction::Queue,
            },
            Some(_) => PrimaryAction::Queue,
            None => PrimaryAction::Send,
        };
        ComposerStatus {
            can_submit: !draft.text.trim().is_empty() && pending.is_none() && self.snapshot_loaded,
            primary,
            can_stop: active.is_some_and(|run| !self.in_flight(&format!("stop:{}", run.id))),
            sending: pending.is_some_and(|p| p.in_flight),
            retry: pending
                .filter(|p| !p.in_flight && p.unconfirmed.is_some())
                .map(|p| p.key.clone()),
        }
    }
}

// ------------------------------------------------------------------ folding

impl AppState {
    /// Replaces the Core's entities with `snapshot`: the first load and every
    /// resync. Local intents (drafts, pending commands) stay.
    pub fn apply_snapshot(&mut self, snapshot: Snapshot) {
        self.sequence = snapshot.sequence;
        self.threads = snapshot.threads;
        self.runs = snapshot.runs;
        self.tools = snapshot.tools;
        self.requests = snapshot.requests;
        self.snapshot_loaded = true;
        if let Some(id) = &self.selected
            && self.thread(id).is_none()
        {
            self.selected = None;
        }
        if self.selected.is_none()
            && self
                .drafts
                .get(NEW_THREAD)
                .is_none_or(|d| d.text.is_empty())
        {
            self.selected = self.visible_threads().first().map(|t| t.id.clone());
        }
    }

    /// Folds one event. Deltas are appended exactly once: an event at or
    /// before the applied sequence is a replay and changes nothing.
    pub fn apply_event(&mut self, event: &Event) -> Applied {
        if event.sequence <= self.sequence {
            self.stats.stale += 1;
            return Applied::Stale;
        }
        // The cursor follows every committed event, shown or not, so a
        // resubscription never replays what was already seen.
        self.sequence = event.sequence;
        let changed = match event.kind.as_str() {
            event::RUN_OUTPUT => self.fold_output(event),
            event::RUN_REASONING => self.fold_reasoning(event),
            event::RUN_TELEMETRY => self.fold_telemetry(event),
            event::THREAD_CREATED | event::THREAD_UPDATED => self.fold_thread(event),
            event::THREAD_DELETED => self.fold_thread_deleted(event),
            event::TOOL_UPDATED => self.fold_tool(event),
            event::REQUEST_UPDATED => self.fold_request(event),
            other => match RunStatus::from_event_type(other) {
                Some(status) => self.fold_run(event, Some(status)),
                None if other.starts_with("run.") => self.fold_run(event, None),
                None => false,
            },
        };
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
    fn fold_output(&mut self, event: &Event) -> bool {
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
        let Some(run) = self.run_mut(&event.run_id) else {
            return false;
        };
        match (delta, full) {
            (Some(delta), _) if !delta.is_empty() => {
                run.output.push_str(&delta);
                true
            }
            (_, Some(full)) if !full.is_empty() => {
                if !run.output.starts_with(&full) {
                    run.output = full;
                }
                true
            }
            (None, None) => false,
            _ => true,
        }
    }

    fn fold_reasoning(&mut self, event: &Event) -> bool {
        let Some(delta) = event.data.get("reasoningDelta").and_then(Value::as_str) else {
            return false;
        };
        let delta = delta.to_owned();
        match self.run_mut(&event.run_id) {
            Some(run) => {
                run.reasoning.push_str(&delta);
                true
            }
            None => false,
        }
    }

    fn fold_telemetry(&mut self, event: &Event) -> bool {
        let value = event.data.get("telemetry").unwrap_or(&event.data);
        let Ok(telemetry) = serde_json::from_value::<RunTelemetry>(value.clone()) else {
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
    fn fold_run(&mut self, event: &Event, status: Option<RunStatus>) -> bool {
        let value = event.data.get("run").unwrap_or(&event.data);
        let Ok(mut run) = serde_json::from_value::<Run>(value.clone()) else {
            return false;
        };
        if run.id.is_empty() {
            run.id = event.run_id.clone();
        }
        if run.thread_id.is_empty() {
            run.thread_id = event.thread_id.clone();
        }
        // A bare run without a `status` field takes the event's.
        let has_status = value.get("status").is_some();
        if !has_status {
            match status {
                Some(status) => run.status = status,
                None => return false,
            }
        }
        if run.id.is_empty() || run.thread_id.is_empty() {
            return false;
        }
        match self.run_mut(&run.id) {
            Some(existing) => merge_run(existing, run),
            None => self.runs.push(run),
        }
        true
    }

    fn fold_thread(&mut self, event: &Event) -> bool {
        let value = event.data.get("thread").unwrap_or(&event.data);
        let Ok(thread) = serde_json::from_value::<Thread>(value.clone()) else {
            return false;
        };
        if thread.id.is_empty() {
            return false;
        }
        match self.threads.iter_mut().find(|t| t.id == thread.id) {
            Some(existing) => *existing = thread,
            None => self.threads.push(thread),
        }
        true
    }

    fn fold_thread_deleted(&mut self, event: &Event) -> bool {
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
        self.drafts.remove(&id);
        if self.selected.as_deref() == Some(id.as_str()) {
            self.selected = None;
        }
        true
    }

    fn fold_tool(&mut self, event: &Event) -> bool {
        let Some(value) = event.data.get("tool") else {
            return false;
        };
        let Ok(mut tool) = serde_json::from_value::<ToolCall>(value.clone()) else {
            return false;
        };
        if tool.thread_id.is_empty() {
            tool.thread_id = event.thread_id.clone();
        }
        if tool.run_id.is_empty() {
            tool.run_id = event.run_id.clone();
        }
        if tool.id.is_empty() || tool.run_id.is_empty() {
            return false;
        }
        match self.tools.iter_mut().find(|t| t.id == tool.id) {
            Some(existing) => *existing = tool,
            None => self.tools.push(tool),
        }
        true
    }

    fn fold_request(&mut self, event: &Event) -> bool {
        let Some(value) = event.data.get("request") else {
            return false;
        };
        let Ok(mut request) = serde_json::from_value::<ProviderRequest>(value.clone()) else {
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
        match self.requests.iter_mut().find(|r| r.id == request.id) {
            Some(existing) => *existing = request,
            None => self.requests.push(request),
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
    let telemetry = incoming.telemetry.clone().or(existing.telemetry.take());
    // A terminal status never goes back to an active one (a late event).
    let status = if existing.status.terminal() && !incoming.status.terminal() {
        existing.status
    } else {
        incoming.status
    };
    *existing = Run {
        output,
        reasoning,
        input,
        created_at,
        telemetry,
        status,
        ..incoming
    };
}

// ------------------------------------------------------------------ intents

impl AppState {
    pub fn select_thread(&mut self, id: Option<String>) {
        self.selected = id.filter(|id| self.thread(id).is_some());
        self.notice = None;
    }

    /// Opens the new thread's draft.
    pub fn new_thread(&mut self) {
        self.selected = None;
        self.notice = None;
    }

    pub fn set_draft_text(&mut self, text: &str) {
        let key = self.draft_key();
        self.drafts.entry(key).or_default().text = text.to_owned();
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
                    text: String::new(),
                    mode: draft.mode,
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
    /// says). Returns the command to dispatch; the draft moves into the
    /// pending command until the Core accepts it.
    pub fn submit(&mut self) -> Option<Command> {
        if !self.composer_status().can_submit {
            return None;
        }
        let draft_key = self.draft_key();
        let key = format!("submit:{draft_key}");
        let draft = self.take_draft();
        let text = draft.text.trim().to_owned();
        let Some(thread_id) = self.selected.clone() else {
            let mut send = Command::new(kind::SEND_MESSAGE, new_command_id("send"));
            send.message = text.clone();
            send.interaction_mode = draft.mode.wire().to_owned();
            let mut create = Command::new(kind::CREATE_THREAD, new_command_id("thread"));
            create.title = title_from_message(&text);
            create.provider_id = "pi".to_owned();
            create.interaction_mode = draft.mode.wire().to_owned();
            self.notice = Some(Notice::new(NoticeLevel::Info, "正在创建会话…"));
            return Some(self.track(
                key,
                create,
                Purpose::CreateThread {
                    then_send: send,
                    draft,
                },
            ));
        };
        let active = self.active_run(&thread_id).cloned();
        let command = match active {
            Some(run)
                if run.status == RunStatus::Running
                    && self.running_action == RunningAction::Steer =>
            {
                let mut steer = Command::new(kind::STEER_MESSAGE, new_command_id("steer"));
                steer.run_id = run.id;
                steer
            }
            Some(_) => Command::new(kind::QUEUE_MESSAGE, new_command_id("queue")),
            None => Command::new(kind::SEND_MESSAGE, new_command_id("send")),
        };
        let mut command = command;
        command.thread_id = thread_id;
        command.message = text;
        command.interaction_mode = draft.mode.wire().to_owned();
        self.notice = None;
        Some(self.track(key, command, Purpose::Message { draft_key, draft }))
    }

    /// Stops the shown thread's active run.
    pub fn stop(&mut self) -> Option<Command> {
        let thread_id = self.selected.clone()?;
        let run = self.active_run(&thread_id)?.clone();
        let key = format!("stop:{}", run.id);
        if self.in_flight(&key) {
            return None;
        }
        let mut command = Command::new(kind::STOP_RUN, new_command_id("stop"));
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
        if let Some(r) = self.requests.iter_mut().find(|r| r.id == request_id) {
            r.status = "responding".to_owned();
        }
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

    /// Gives up a pending command whose outcome is unknown, restoring its
    /// draft (the Core may still have it: the timeline will tell).
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
            Purpose::CreateThread { draft, .. } => self.restore_draft(NEW_THREAD.to_owned(), draft),
            Purpose::Message { draft_key, draft } => self.restore_draft(draft_key, draft),
            Purpose::Respond { request_id } => {
                if let Some(r) = self.requests.iter_mut().find(|r| r.id == request_id)
                    && r.status == "responding"
                {
                    r.status = "pending".to_owned();
                }
            }
            Purpose::Stop => {}
        }
    }

    /// Puts an unsent draft back, unless the user wrote a new one meanwhile.
    fn restore_draft(&mut self, key: String, draft: Draft) {
        let slot = self.drafts.entry(key).or_default();
        if slot.text.trim().is_empty() {
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
                match pending.purpose {
                    Purpose::CreateThread {
                        mut then_send,
                        draft,
                    } => {
                        let thread_id = receipt.thread_id.clone();
                        if thread_id.is_empty() {
                            self.notice =
                                Some(Notice::new(NoticeLevel::Error, "Core 未返回新会话 ID。"));
                            self.restore_draft(NEW_THREAD.to_owned(), draft);
                            return None;
                        }
                        // The thread may not have arrived as an event yet:
                        // show it now; its event replaces this.
                        if self.thread(&thread_id).is_none() {
                            self.threads.push(Thread {
                                id: thread_id.clone(),
                                title: pending.command.title.clone(),
                                provider_id: pending.command.provider_id.clone(),
                                ..Thread::default()
                            });
                        }
                        if self.selected.is_none() {
                            self.selected = Some(thread_id.clone());
                        }
                        if draft.mode != InteractionMode::Default {
                            self.drafts.entry(thread_id.clone()).or_default().mode = draft.mode;
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
                        if receipt.delivery == "queued"
                            || pending.command.kind == kind::QUEUE_MESSAGE
                        {
                            self.notice = Some(Notice::new(NoticeLevel::Info, "已加入队列。"));
                        }
                        None
                    }
                    Purpose::Stop | Purpose::Respond { .. } => None,
                }
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
}

fn action_label(command: &Command) -> &'static str {
    match command.kind.as_str() {
        kind::CREATE_THREAD => "创建会话",
        kind::SEND_MESSAGE => "发送",
        kind::QUEUE_MESSAGE => "排队",
        kind::STEER_MESSAGE => "追加",
        kind::STOP_RUN => "停止",
        kind::RESPOND_REQUEST => "请求响应",
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

    /// The snapshot is in and the subscription runs.
    pub fn on_live(&mut self) {
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
        self.notice = Some(Notice::new(
            NoticeLevel::Warn,
            format!("无法读取 Core 快照（{}），正在重试…", error.message),
        ));
        Recovery::Resync {
            delay: reconnect_delay(attempt),
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
