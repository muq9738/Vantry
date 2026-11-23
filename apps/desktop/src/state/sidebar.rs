//! The sidebar's sections: search and status filters, the pinned / active /
//! snoozed / archived groups, and delegated subtasks nested under their
//! parent with their task's status.
//!
//! Computed in one pass over runs, requests and tasks (not per thread), so
//! hundreds of threads cost little; the view recomputes them only when
//! [`AppState::sidebar_revision`] or the filter changes.

use std::collections::{BTreeSet, HashMap};

use vantry_protocol::{RunStatus, Thread};

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
}

/// A sidebar group.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Group {
    Pinned,
    Active,
    Snoozed,
    Archived,
}

impl Group {
    pub fn label(self) -> &'static str {
        match self {
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
}

/// A group and its rows.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SidebarSection {
    pub group: Group,
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

impl AppState {
    /// Every thread's status in one pass.
    pub fn thread_statuses(&self) -> HashMap<&str, ThreadStatus> {
        let mut out: HashMap<&str, ThreadStatus> = HashMap::new();
        let mut last: HashMap<&str, RunStatus> = HashMap::new();
        for run in &self.runs {
            if !run.status.terminal() {
                out.insert(run.thread_id.as_str(), ThreadStatus::Running);
            }
            last.insert(run.thread_id.as_str(), run.status);
        }
        for (thread, status) in last {
            if matches!(status, RunStatus::Failed | RunStatus::Interrupted) {
                out.entry(thread).or_insert(ThreadStatus::Failed);
            }
        }
        for request in self.requests.iter().filter(|r| r.status == "pending") {
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
        let status_of = |id: &str| statuses.get(id).copied().unwrap_or(ThreadStatus::Idle);
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
        let matches = |t: &Thread| {
            (query.is_empty() || t.title.to_lowercase().contains(&query))
                && filter.status.keeps(status_of(&t.id))
        };
        // Whether `t` or a descendant matches.
        fn subtree_matches<'a>(
            t: &'a Thread,
            children: &HashMap<&str, Vec<&'a Thread>>,
            matches: &dyn Fn(&Thread) -> bool,
            depth: u8,
        ) -> bool {
            matches(t)
                || (depth < 8
                    && children.get(t.id.as_str()).is_some_and(|list| {
                        list.iter()
                            .any(|c| subtree_matches(c, children, matches, depth + 1))
                    }))
        }
        let filtering = !query.is_empty() || filter.status != StatusFilter::All;

        let mut tops: Vec<&Thread> = self
            .threads
            .iter()
            .filter(|t| t.parent_thread_id.is_empty() || !ids.contains(t.parent_thread_id.as_str()))
            .collect();
        tops.sort_by(|a, b| b.created_at.cmp(&a.created_at).then(b.id.cmp(&a.id)));

        let mut sections: Vec<SidebarSection> = [Group::Pinned, Group::Active, Group::Snoozed, Group::Archived]
            .into_iter()
            .map(|group| SidebarSection {
                group,
                rows: Vec::new(),
                count: 0,
                folded: group == Group::Archived && !filter.show_archived && !filtering,
            })
            .collect();
        for top in tops {
            if !subtree_matches(top, &children, &matches, 0) {
                continue;
            }
            let snoozed = is_snoozed(top.snoozed_until.as_deref(), now);
            let group = if top.archived {
                Group::Archived
            } else if top.pinned {
                Group::Pinned
            } else if snoozed {
                Group::Snoozed
            } else {
                Group::Active
            };
            let section = &mut sections[group as usize];
            section.count += 1;
            if section.folded {
                continue;
            }
            self.push_tree(
                &mut section.rows,
                top,
                0,
                &children,
                &task_status,
                &status_of,
                filter,
                filtering.then_some(&matches as &dyn Fn(&Thread) -> bool),
                now,
            );
        }
        sections
            .into_iter()
            .filter(|s| s.count > 0)
            .collect()
    }

    #[allow(clippy::too_many_arguments)]
    fn push_tree(
        &self,
        rows: &mut Vec<SidebarRow>,
        thread: &Thread,
        depth: u8,
        children: &HashMap<&str, Vec<&Thread>>,
        task_status: &HashMap<&str, RunStatus>,
        status_of: &dyn Fn(&str) -> ThreadStatus,
        filter: &SidebarFilter,
        only: Option<&dyn Fn(&Thread) -> bool>,
        now: chrono::DateTime<chrono::Utc>,
    ) {
        let kids = children.get(thread.id.as_str()).map_or(&[][..], Vec::as_slice);
        let collapsed = filter.collapsed.contains(&thread.id) && only.is_none();
        rows.push(SidebarRow {
            id: thread.id.clone(),
            title: display_title(thread),
            status: status_of(&thread.id),
            depth,
            children: kids.len(),
            collapsed,
            task_status: if depth > 0 {
                task_status.get(thread.id.as_str()).copied()
            } else {
                None
            },
            pinned: thread.pinned,
            archived: thread.archived,
            snoozed: is_snoozed(thread.snoozed_until.as_deref(), now),
        });
        if collapsed || depth >= 8 {
            return;
        }
        for child in kids {
            if let Some(only) = only
                && !only(child)
                && !children.get(child.id.as_str()).is_some_and(|c| !c.is_empty())
            {
                continue;
            }
            self.push_tree(
                rows,
                child,
                depth + 1,
                children,
                task_status,
                status_of,
                filter,
                only,
                now,
            );
        }
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
