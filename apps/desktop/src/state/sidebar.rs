//! The sidebar's sections: search and status filters, the pinned / active /
//! snoozed / archived groups, and delegated subtasks nested under their
//! parent with their task's status.
//!
//! Computed in one pass over runs, requests and tasks (not per thread), so
//! hundreds of threads cost little; the view recomputes them only when
//! [`AppState::sidebar_revision`] or the filter changes.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use chrono::{DateTime, Datelike, Utc};
use vantry_protocol::{RunStatus, Thread, WorkspaceSpec};

use super::{AppState, ThreadStatus};

/// Which threads the status filter keeps.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum StatusFilter {
    #[default]
    All,
    Running,
    /// Waiting on the user: approvals and failures.
    Attention,
}

impl StatusFilter {
    pub fn label(self) -> &'static str {
        match self {
            StatusFilter::All => "全部",
            StatusFilter::Running => "运行中",
            StatusFilter::Attention => "待处理",
        }
    }

    pub fn wire(self) -> &'static str {
        match self {
            StatusFilter::All => "all",
            StatusFilter::Running => "running",
            StatusFilter::Attention => "attention",
        }
    }

    pub fn parse(text: &str) -> Self {
        match text {
            "running" => StatusFilter::Running,
            "attention" => StatusFilter::Attention,
            _ => StatusFilter::All,
        }
    }

    fn keeps(self, status: ThreadStatus) -> bool {
        match self {
            StatusFilter::All => true,
            StatusFilter::Running => status == ThreadStatus::Running,
            StatusFilter::Attention => {
                matches!(status, ThreadStatus::NeedsApproval | ThreadStatus::Failed)
            }
        }
    }
}

/// The sidebar's filter.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct SidebarFilter {
    pub query: String,
    pub status: StatusFilter,
    /// Whether the archived group is open.
    pub show_archived: bool,
    /// Parents whose subtasks are folded.
    pub collapsed: BTreeSet<String>,
    /// Explicit project header choices, shared with v0.1.42's sections.cat:<path>.
    pub project_open: BTreeMap<String, bool>,
    /// Projects showing more than the initial five families, for this window.
    pub show_all: BTreeSet<String>,
    pub project: Option<String>,
}

/// A sidebar group.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Group {
    Running,
    Attention,
    Pinned,
    Active,
    Snoozed,
    Archived,
}

impl Group {
    pub fn label(self) -> &'static str {
        match self {
            Group::Running => "运行中",
            Group::Attention => "待我处理",
            Group::Pinned => "已置顶",
            Group::Active => "会话",
            Group::Snoozed => "已延后",
            Group::Archived => "已归档",
        }
    }
}

/// One row: a thread, or a subtask under its parent.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SidebarRow {
    pub id: String,
    pub title: String,
    pub status: ThreadStatus,
    /// 0 for a top-level thread, 1+ for subtasks.
    pub depth: u8,
    /// Its subtasks (shown or folded).
    pub children: usize,
    pub collapsed: bool,
    /// A subtask's task status (the badge).
    pub task_status: Option<RunStatus>,
    pub pinned: bool,
    pub archived: bool,
    pub snoozed: bool,
    pub branch: String,
    pub pi: bool,
    /// Short model name for the subtitle line (`gpt-5.4`, `opus`, …).
    pub model: String,
    /// Compact date label (`今天`, `1月2日`).
    pub date_label: String,
    /// Workspace folder name (`Vantry`).
    pub workspace_name: String,
    /// Full project directory key for copy-path / filter / new-thread-in-project.
    pub project_key: String,
    /// Parent thread ID if this row is a delegated subtask.
    pub parent_thread_id: String,
    pub unread: bool,
    pub has_completed_run: bool,
}

/// A group and its rows.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SidebarSection {
    pub group: Group,
    /// The full project key. An empty key means an unassigned conversation.
    pub project: Option<String>,
    pub hidden: usize,
    pub rows: Vec<SidebarRow>,
    /// Top-level threads in it.
    pub count: usize,
    /// Rows are hidden (the archived group closed).
    pub folded: bool,
}

/// A task status's badge text.
pub fn task_badge(status: RunStatus) -> &'static str {
    match status {
        RunStatus::Queued => "排队",
        RunStatus::Running => "运行中",
        RunStatus::Waiting => "等待子任务",
        RunStatus::Completed => "完成",
        RunStatus::Cancelled => "已取消",
        RunStatus::Interrupted => "中断",
        RunStatus::Failed => "失败",
        RunStatus::Unspecified => "",
    }
}

/// Whether a thread snoozed until `until` still sleeps at `now` (RFC 3339).
pub fn is_snoozed(until: Option<&str>, now: chrono::DateTime<chrono::Utc>) -> bool {
    let Some(until) = until.filter(|u| !u.is_empty()) else {
        return false;
    };
    match chrono::DateTime::parse_from_rfc3339(until) {
        Ok(until) => until.with_timezone(&chrono::Utc) > now,
        // An unreadable time still says the user put it away.
        Err(_) => true,
    }
}

/// A title for an untitled thread.
pub fn display_title(thread: &Thread) -> String {
    if thread.title.trim().is_empty() {
        "未命名会话".to_owned()
    } else {
        thread.title.clone()
    }
}

fn compact_model(model: &str) -> String {
    let trimmed = model.trim();
    if trimmed.is_empty() {
        return String::new();
    }
    trimmed
        .rsplit_once('/')
        .map_or(trimmed, |(_, id)| id)
        .to_owned()
}

fn timestamp(value: &str) -> Option<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(value)
        .ok()
        .map(|at| at.to_utc())
}

fn compact_date(at: Option<DateTime<Utc>>, now: DateTime<Utc>) -> String {
    let Some(at) = at else { return String::new() };
    let age = now - at;
    let local = at.with_timezone(&chrono::Local);
    if age.num_minutes() < 1 {
        "刚刚".into()
    } else if age.num_hours() < 1 {
        format!("{}m", age.num_minutes())
    } else if age.num_days() < 1 {
        format!("{}h", age.num_hours())
    } else if age.num_days() < 7 {
        format!("{}d", age.num_days())
    } else if local.year() == now.with_timezone(&chrono::Local).year() {
        local.format("%-m月%-d日").to_string()
    } else {
        local.format("%Y/%-m/%-d").to_string()
    }
}

/// Lexical identity works for remote paths and for checkouts that no longer exist.
pub fn workspace_key(spec: Option<&WorkspaceSpec>, fallback: &str) -> String {
    let raw = spec
        .into_iter()
        .flat_map(|w| [w.repository.trim(), w.path.trim()])
        .find(|s| !s.is_empty())
        .unwrap_or(fallback.trim());
    if raw.is_empty() {
        return String::new();
    }
    let path = raw.replace('\\', "/");
    let mut parts: Vec<&str> = Vec::new();
    for part in path.split('/') {
        match part {
            "" | "." => {}
            ".." if parts
                .last()
                .is_some_and(|p| *p != ".." && !p.ends_with(':')) =>
            {
                parts.pop();
            }
            ".." if path.starts_with('/') || parts.last().is_some_and(|p| p.ends_with(':')) => {}
            _ => parts.push(part),
        }
    }
    if parts.ends_with(&["chat"]) && parts.contains(&"workspaces") {
        return String::new();
    }
    let prefix = if path.starts_with("//") {
        "//"
    } else if path.starts_with('/') {
        "/"
    } else {
        ""
    };
    format!("{prefix}{}", parts.join("/"))
}

pub fn project_name(key: &str) -> &str {
    if key.is_empty() {
        return "无项目";
    }
    key.rsplit('/').find(|name| !name.is_empty()).unwrap_or(key)
}

pub fn project_section_key(key: &str) -> &str {
    if key.is_empty() {
        "__unassigned__"
    } else {
        key
    }
}

fn thread_project(thread: &Thread) -> String {
    workspace_key(thread.workspace.as_ref(), &thread.project_path)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Ord, PartialOrd)]
pub enum InboxKind {
    Approval,
    Failed,
    Reply,
}

impl InboxKind {
    pub fn label(self) -> &'static str {
        match self {
            Self::Approval => "待确认",
            Self::Failed => "失败或中断",
            Self::Reply => "新回复",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InboxItem {
    pub thread: String,
    pub request: Option<String>,
    pub kind: InboxKind,
    pub title: String,
    pub summary: String,
    at: Option<DateTime<Utc>>,
}

fn subtree_any(
    t: &Thread,
    children: &HashMap<&str, Vec<&Thread>>,
    matches: &dyn Fn(&Thread) -> bool,
    depth: u8,
) -> bool {
    matches(t)
        || (depth < 8
            && children.get(t.id.as_str()).is_some_and(|list| {
                list.iter()
                    .any(|child| subtree_any(child, children, matches, depth + 1))
            }))
}

fn family_activity(
    t: &Thread,
    children: &HashMap<&str, Vec<&Thread>>,
    activity: &HashMap<&str, Option<DateTime<Utc>>>,
    depth: u8,
) -> Option<DateTime<Utc>> {
    let own = activity.get(t.id.as_str()).copied().flatten();
    if depth >= 8 {
        return own;
    }
    children
        .get(t.id.as_str())
        .into_iter()
        .flatten()
        .fold(own, |latest, child| {
            latest.max(family_activity(child, children, activity, depth + 1))
        })
}

struct SidebarTree<'a> {
    children: HashMap<&'a str, Vec<&'a Thread>>,
    task_status: HashMap<&'a str, RunStatus>,
    statuses: HashMap<&'a str, ThreadStatus>,
    activity: HashMap<&'a str, Option<DateTime<Utc>>>,
    unread: &'a BTreeSet<String>,
    has_terminal_run: HashMap<&'a str, bool>,
    filter: &'a SidebarFilter,
    now: DateTime<Utc>,
}

impl SidebarTree<'_> {
    fn status(&self, id: &str) -> ThreadStatus {
        self.statuses.get(id).copied().unwrap_or(ThreadStatus::Idle)
    }

    fn append_family(
        &self,
        rows: &mut Vec<SidebarRow>,
        thread: &Thread,
        depth: u8,
        latest: Option<DateTime<Utc>>,
        show_project: bool,
        only: Option<&dyn Fn(&Thread) -> bool>,
    ) {
        let kids = self
            .children
            .get(thread.id.as_str())
            .map_or(&[][..], Vec::as_slice);
        let collapsed = self.filter.collapsed.contains(&thread.id) && only.is_none();
        rows.push(SidebarRow {
            id: thread.id.clone(),
            title: display_title(thread),
            status: self.status(&thread.id),
            depth,
            children: kids.len(),
            collapsed,
            task_status: if depth > 0 {
                self.task_status.get(thread.id.as_str()).copied()
            } else {
                None
            },
            pinned: thread.pinned,
            archived: thread.archived,
            snoozed: is_snoozed(thread.snoozed_until.as_deref(), self.now),
            branch: thread
                .workspace
                .as_ref()
                .map(|w| w.branch.clone())
                .unwrap_or_default(),
            pi: thread.provider_id.is_empty() || thread.provider_id == "pi",
            model: compact_model(&thread.model),
            date_label: compact_date(latest, self.now),
            workspace_name: if show_project {
                let project = thread_project(thread);
                if project.is_empty() {
                    String::new()
                } else {
                    project_name(&project).to_owned()
                }
            } else {
                String::new()
            },
            project_key: thread_project(thread),
            parent_thread_id: thread.parent_thread_id.clone(),
            unread: self.unread.contains(&thread.id),
            has_completed_run: self
                .has_terminal_run
                .get(thread.id.as_str())
                .copied()
                .unwrap_or(false),
        });
        if collapsed || depth >= 8 {
            return;
        }
        for child in kids {
            if let Some(only) = only
                && !subtree_any(child, &self.children, only, depth + 1)
            {
                continue;
            }
            self.append_family(
                rows,
                child,
                depth + 1,
                self.activity.get(child.id.as_str()).copied().flatten(),
                show_project,
                only,
            );
        }
    }
}

impl AppState {
    pub fn current_project_key(&self) -> String {
        self.selected_thread()
            .map(thread_project)
            .unwrap_or_else(|| workspace_key(self.new_thread.workspace.as_ref(), ""))
    }

    pub fn project_keys(&self) -> BTreeSet<String> {
        let mut keys: BTreeSet<_> = self.threads.iter().map(thread_project).collect();
        if !self.local_order.is_empty() {
            keys.insert(workspace_key(self.new_thread.workspace.as_ref(), ""));
        }
        keys
    }

    pub fn inbox(&self) -> Vec<InboxItem> {
        let titles: HashMap<_, _> = self.threads.iter().map(|t| (t.id.as_str(), t)).collect();
        let mut items: Vec<_> = self
            .requests
            .iter()
            .filter(|r| r.status == "pending")
            .map(|r| InboxItem {
                thread: r.thread_id.clone(),
                request: Some(r.id.clone()),
                kind: InboxKind::Approval,
                title: titles
                    .get(r.thread_id.as_str())
                    .map_or_else(|| r.thread_id.clone(), |t| display_title(t)),
                summary: r.title.clone(),
                at: timestamp(&r.created_at),
            })
            .collect();
        let runs: HashMap<_, _> = self.runs.iter().map(|r| (r.id.as_str(), r)).collect();
        for id in &self.unread {
            let last_result = self
                .timeline
                .run_order(id)
                .iter()
                .rev()
                .filter_map(|id| runs.get(id.as_str()))
                .find(|r| {
                    matches!(
                        r.status,
                        RunStatus::Completed | RunStatus::Failed | RunStatus::Interrupted
                    )
                });
            let (Some(thread), Some(run)) = (titles.get(id.as_str()), last_result) else {
                continue;
            };
            let failed = matches!(run.status, RunStatus::Failed | RunStatus::Interrupted);
            let detail = if failed { &run.error } else { &run.output };
            items.push(InboxItem {
                thread: id.clone(),
                request: None,
                kind: if failed {
                    InboxKind::Failed
                } else {
                    InboxKind::Reply
                },
                title: display_title(thread),
                summary: detail
                    .lines()
                    .next()
                    .unwrap_or("")
                    .chars()
                    .take(64)
                    .collect(),
                at: timestamp(&run.updated_at).max(timestamp(&run.created_at)),
            });
        }
        items.sort_by(|a, b| {
            a.kind
                .cmp(&b.kind)
                .then(b.at.cmp(&a.at))
                .then(a.title.cmp(&b.title))
        });
        items
    }

    /// Every thread's status in one pass.
    pub fn thread_statuses(&self) -> HashMap<&str, ThreadStatus> {
        let mut out: HashMap<&str, ThreadStatus> = self
            .summary_statuses
            .iter()
            .map(|(id, status)| (id.as_str(), *status))
            .collect();
        for run in &self.runs {
            let id = run.thread_id.as_str();
            // A real row supersedes its fast-summary projection, including a
            // completed run that no longer has an active status.
            out.remove(id);
            if !run.status.terminal() {
                out.insert(id, ThreadStatus::Running);
            } else if matches!(run.status, RunStatus::Failed | RunStatus::Interrupted)
                && self.timeline.run_order(id).last() == Some(&run.id)
            {
                out.entry(id).or_insert(ThreadStatus::Failed);
            }
        }
        for request in self.requests.iter().filter(|r| r.status == "pending") {
            out.remove(request.thread_id.as_str());
            out.insert(request.thread_id.as_str(), ThreadStatus::NeedsApproval);
        }
        out
    }

    /// The sidebar's sections under `filter` at `now`. Groups without rows
    /// are left out, except Archived, which shows its count while folded.
    pub fn sidebar_sections(
        &self,
        filter: &SidebarFilter,
        now: chrono::DateTime<chrono::Utc>,
    ) -> Vec<SidebarSection> {
        let statuses = self.thread_statuses();
        let ids: BTreeSet<&str> = self.threads.iter().map(|t| t.id.as_str()).collect();
        // Children by parent (only parents that exist).
        let mut children: HashMap<&str, Vec<&Thread>> = HashMap::new();
        for thread in &self.threads {
            if !thread.parent_thread_id.is_empty() && ids.contains(thread.parent_thread_id.as_str())
            {
                children
                    .entry(thread.parent_thread_id.as_str())
                    .or_default()
                    .push(thread);
            }
        }
        for list in children.values_mut() {
            list.sort_by(|a, b| a.created_at.cmp(&b.created_at).then(a.id.cmp(&b.id)));
        }
        let task_status: HashMap<&str, RunStatus> = self
            .tasks
            .iter()
            .map(|t| (t.child_thread_id.as_str(), t.status))
            .collect();
        let query = filter.query.trim().to_lowercase();
        let filtering = !query.is_empty() || filter.status != StatusFilter::All;
        let mut activity: HashMap<_, _> = self
            .threads
            .iter()
            .map(|t| (t.id.as_str(), timestamp(&t.created_at)))
            .collect();
        for (id, at) in &self.summary_activity {
            activity.insert(id.as_str(), timestamp(at));
        }
        for (id, at) in self
            .runs
            .iter()
            .map(|r| {
                (
                    r.thread_id.as_str(),
                    timestamp(&r.updated_at).max(timestamp(&r.created_at)),
                )
            })
            .chain(self.tasks.iter().map(|t| {
                (
                    t.parent_thread_id.as_str(),
                    timestamp(&t.updated_at).max(timestamp(&t.created_at)),
                )
            }))
        {
            activity
                .entry(id)
                .and_modify(|latest| *latest = (*latest).max(at))
                .or_insert(at);
        }
        let mut has_terminal_run: HashMap<&str, bool> = HashMap::new();
        for run in &self.runs {
            if run.status.terminal() {
                has_terminal_run.insert(run.thread_id.as_str(), true);
            }
        }
        let tree = SidebarTree {
            children,
            task_status,
            statuses,
            activity,
            unread: &self.unread,
            has_terminal_run,
            filter,
            now,
        };
        let matches_filter = |t: &Thread, by_project: bool| {
            (by_project || query.is_empty() || display_title(t).to_lowercase().contains(&query))
                && (filter.status.keeps(tree.status(&t.id))
                    || (filter.status == StatusFilter::Attention && self.unread.contains(&t.id)))
        };
        let mut families = Vec::new();
        for top in self
            .threads
            .iter()
            .filter(|t| t.parent_thread_id.is_empty() || !ids.contains(t.parent_thread_id.as_str()))
        {
            let key = thread_project(top);
            if filter
                .project
                .as_ref()
                .is_some_and(|selected| selected != &key)
            {
                continue;
            }
            let by_project = !query.is_empty()
                && (key.to_lowercase().contains(&query)
                    || project_name(&key).to_lowercase().contains(&query));
            let matches = |t: &Thread| matches_filter(t, by_project);
            if filtering && !subtree_any(top, &tree.children, &matches, 0) {
                continue;
            }
            let group = if top.archived {
                Group::Archived
            } else if is_snoozed(top.snoozed_until.as_deref(), now) {
                Group::Snoozed
            } else if subtree_any(
                top,
                &tree.children,
                &|t| tree.status(&t.id) == ThreadStatus::NeedsApproval,
                0,
            ) {
                Group::Attention
            } else if subtree_any(
                top,
                &tree.children,
                &|t| tree.status(&t.id) == ThreadStatus::Running,
                0,
            ) {
                Group::Running
            } else if self.unread.contains(&top.id)
                || (top.unread_sequence.is_none() && tree.status(&top.id) == ThreadStatus::Failed)
            {
                Group::Attention
            } else if top.pinned {
                Group::Pinned
            } else {
                Group::Active
            };
            families.push((
                top,
                group,
                key,
                by_project,
                family_activity(top, &tree.children, &tree.activity, 0),
            ));
        }
        // Families and project headers follow their latest activity, including delegated work.
        families.sort_by(|a, b| b.4.cmp(&a.4).then(a.2.cmp(&b.2)).then(a.0.id.cmp(&b.0.id)));
        let mut indices = HashMap::new();
        let mut sections = Vec::<SidebarSection>::new();
        for (top, group, key, by_project, latest) in families {
            let project = (group == Group::Active).then_some(key);
            let index = *indices.entry((group, project.clone())).or_insert_with(|| {
                let folded = !filtering
                    && match &project {
                        Some(key) => {
                            filter.project_open.get(project_section_key(key)) == Some(&false)
                        }
                        None => group == Group::Archived && !filter.show_archived,
                    };
                sections.push(SidebarSection {
                    group,
                    project,
                    rows: Vec::new(),
                    count: 0,
                    hidden: 0,
                    folded,
                });
                sections.len() - 1
            });
            let section = &mut sections[index];
            section.count += 1;
            if section.folded {
                continue;
            }
            if section.count > 5
                && !filtering
                && section
                    .project
                    .as_ref()
                    .is_some_and(|p| !filter.show_all.contains(p))
            {
                section.hidden += 1;
                continue;
            }
            let matches = |t: &Thread| matches_filter(t, by_project);
            tree.append_family(
                &mut section.rows,
                top,
                0,
                latest,
                group != Group::Active,
                filtering.then_some(&matches as &dyn Fn(&Thread) -> bool),
            );
        }
        sections.sort_by_key(|s| (s.group, s.project.as_deref() == Some("")));
        sections
    }

    /// Subtasks of thread `id` with their task status (the header's
    /// orchestration capsule).
    pub fn subtasks(&self, id: &str) -> Vec<(String, String, RunStatus)> {
        let mut out: Vec<(String, String, RunStatus)> = self
            .tasks
            .iter()
            .filter(|t| t.parent_thread_id == id)
            .map(|t| {
                let title = self
                    .thread(&t.child_thread_id)
                    .map(display_title)
                    .unwrap_or_else(|| t.title.clone());
                (t.child_thread_id.clone(), title, t.status)
            })
            .collect();
        out.sort_by(|a, b| a.1.cmp(&b.1));
        out
    }
}
