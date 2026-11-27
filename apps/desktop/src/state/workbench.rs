//! The dock's state: the file tree and its read-only preview, the Git / PR
//! panel, terminal sessions, and the preview panel. Plain transitions: an
//! intent returns what to load (a [`Load`]); results come back through the
//! `on_*` methods, which ignore answers to requests that were superseded.

use std::collections::{BTreeMap, BTreeSet};

use vantry_protocol::{GitFile, GitState};

use super::markdown::MarkdownDoc;
use crate::bridge::workbench::{DirEntry, FileContent, PrSummary, SearchHit};

/// A tab of the dock.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Default)]
pub enum DockTab {
    #[default]
    Files,
    Summary,
    Git,
    Terminal,
    Plugins,
    Mcp,
}

impl DockTab {
    pub const ALL: [DockTab; 6] = [
        DockTab::Summary,
        DockTab::Files,
        DockTab::Git,
        DockTab::Terminal,
        DockTab::Plugins,
        DockTab::Mcp,
    ];

    pub fn label(self) -> &'static str {
        match self {
            DockTab::Summary => "会话概览",
            DockTab::Files => "文件",
            DockTab::Git => "Git",
            DockTab::Terminal => "终端",
            DockTab::Mcp => "MCP",
            DockTab::Plugins => "插件",
        }
    }

    pub fn wire(self) -> &'static str {
        match self {
            DockTab::Summary => "summary",
            DockTab::Files => "files",
            DockTab::Git => "git",
            DockTab::Terminal => "terminal",
            DockTab::Mcp => "mcp",
            DockTab::Plugins => "plugins",
        }
    }

    pub fn parse(text: &str) -> Self {
        DockTab::ALL
            .into_iter()
            .find(|t| t.wire() == text)
            .unwrap_or_default()
    }
}

/// What the controller should load for an intent.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Load {
    Dir(String),
    File(String),
    GitStatus,
    Diff { path: Option<String>, staged: bool },
    Pr,
}

/// The file tree.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FilesState {
    /// Folders shown open.
    pub expanded: BTreeSet<String>,
    /// Loaded folders' entries ("" is the root).
    pub children: BTreeMap<String, Vec<DirEntry>>,
    pub loading: BTreeSet<String>,
    /// Monotonic counter bumped whenever the visible tree rows may change.
    pub tree_revision: u64,
    pub selected: Option<String>,
    pub loading_file: bool,
    pub show_tree: bool,
    pub source_paths: BTreeSet<String>,
    pub wrap: bool,
    /// Optional 1-based inclusive line range to highlight & scroll to.
    pub highlight_lines: Option<(usize, usize)>,
    pub preview: Option<FileContent>,
    pub error: Option<String>,
    pub search: FileSearch,
}

impl Default for FilesState {
    fn default() -> Self {
        Self {
            expanded: BTreeSet::new(),
            children: BTreeMap::new(),
            loading: BTreeSet::new(),
            tree_revision: 0,
            selected: None,
            loading_file: false,
            show_tree: false,
            source_paths: BTreeSet::new(),
            wrap: true,
            highlight_lines: None,
            preview: None,
            error: None,
            search: FileSearch::default(),
        }
    }
}

/// The file panel's search (names first, then lines).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct FileSearch {
    pub query: String,
    /// Bumped per query: an older answer is dropped.
    pub generation: u64,
    pub loading: bool,
    pub results: Vec<SearchHit>,
    pub error: Option<String>,
}

impl FileSearch {
    /// A new query; its generation (the answer must carry it back).
    pub fn set_query(&mut self, query: &str) -> u64 {
        self.query = query.to_owned();
        self.generation += 1;
        self.error = None;
        if query.trim().is_empty() {
            self.loading = false;
            self.results.clear();
        } else {
            self.loading = true;
        }
        self.generation
    }

    pub fn on_results(&mut self, generation: u64, result: Result<Vec<SearchHit>, String>) {
        if generation != self.generation {
            return;
        }
        self.loading = false;
        match result {
            Ok(results) => self.results = results,
            Err(error) => {
                self.results.clear();
                self.error = Some(error);
            }
        }
    }

    /// Whether the tree gives way to the results.
    pub fn active(&self) -> bool {
        !self.query.trim().is_empty()
    }
}

/// A visible row of the tree.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TreeRow {
    pub entry: DirEntry,
    pub depth: usize,
    pub expanded: bool,
    pub loading: bool,
}

impl FilesState {
    /// The rows shown: the root's entries and those of open folders.
    pub fn rows(&self) -> Vec<TreeRow> {
        let mut rows = Vec::new();
        self.push_rows(&mut rows, "", 0);
        rows
    }

    fn push_rows(&self, rows: &mut Vec<TreeRow>, dir: &str, depth: usize) {
        let Some(entries) = self.children.get(dir) else {
            return;
        };
        for entry in entries {
            let expanded = entry.is_dir && self.expanded.contains(&entry.path);
            rows.push(TreeRow {
                entry: entry.clone(),
                depth,
                expanded,
                loading: self.loading.contains(&entry.path),
            });
            if expanded && depth < 32 {
                self.push_rows(rows, &entry.path, depth + 1);
            }
        }
    }

    /// The root, when it was never loaded.
    pub fn open(&mut self) -> Option<Load> {
        if self.children.contains_key("") || self.loading.contains("") {
            return None;
        }
        self.loading.insert(String::new());
        Some(Load::Dir(String::new()))
    }

    /// Reloads the root and every open folder.
    pub fn refresh(&mut self) -> Vec<Load> {
        self.children.clear();
        let mut loads = vec![Load::Dir(String::new())];
        self.loading.insert(String::new());
        for dir in &self.expanded {
            self.loading.insert(dir.clone());
            loads.push(Load::Dir(dir.clone()));
        }
        loads
    }

    /// Opens or closes folder `path`, loading it the first time.
    pub fn toggle_dir(&mut self, path: &str) -> Option<Load> {
        self.tree_revision += 1;
        if self.expanded.remove(path) {
            return None;
        }
        self.expanded.insert(path.to_owned());
        if self.children.contains_key(path) || self.loading.contains(path) {
            return None;
        }
        self.loading.insert(path.to_owned());
        Some(Load::Dir(path.to_owned()))
    }

    pub fn on_dir(&mut self, path: &str, result: Result<Vec<DirEntry>, String>) {
        self.loading.remove(path);
        self.tree_revision += 1;
        match result {
            Ok(entries) => {
                self.children.insert(path.to_owned(), entries);
                self.error = None;
            }
            Err(error) => {
                self.expanded.remove(path);
                self.error = Some(error);
            }
        }
    }

    /// Shows file `path` in the preview.
    pub fn select_file(&mut self, path: &str) -> Option<Load> {
        self.select_file_at(path, None)
    }

    /// Shows file `path` in the preview, optionally highlighting/jumping to `lines`.
    pub fn select_file_at(&mut self, path: &str, lines: Option<(usize, usize)>) -> Option<Load> {
        self.highlight_lines = lines;
        if lines.is_some()
            && (crate::bridge::workbench::is_markdown_ext(path)
                || crate::bridge::workbench::is_svg_ext(path)
                || crate::bridge::workbench::is_html_ext(path))
        {
            self.source_paths.insert(path.to_owned());
        }
        if self.selected.as_deref() == Some(path) && self.preview.is_some() {
            return None;
        }
        self.selected = Some(path.to_owned());
        self.loading_file = true;
        self.preview = None;
        self.error = None;
        Some(Load::File(path.to_owned()))
    }

    pub fn on_file(&mut self, path: &str, result: Result<FileContent, String>) {
        if self.selected.as_deref() != Some(path) {
            return; // Another file was picked meanwhile.
        }
        self.loading_file = false;
        match result {
            Ok(content) => {
                self.preview = Some(content);
                self.error = None;
            }
            Err(error) => self.error = Some(error),
        }
    }
}

/// A changed file, as the Git panel lists it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Change {
    pub path: String,
    /// `M`, `A`, `D`, `R`, `?`, `U`.
    pub code: String,
    pub staged: bool,
}

#[derive(Debug, Copy, Clone, PartialEq, Eq, Default)]
pub enum GitPage { #[default] Commit, Log, Compare }
#[derive(Debug, Copy, Clone, PartialEq, Eq)]
pub enum GitDialog { Expanded, Changes }

/// The Git / PR panel.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct GitPanel {
    pub page: GitPage,
    pub changes_patch: Option<Result<String,String>>,
    pub changes_split: bool,
    pub dialog: Option<GitDialog>,
    pub compare: String,
    pub compare_reverse: bool,
    pub log_branch: String,
    pub log_path: String,
    pub log_author: String,
    pub log_date: String,
    pub compare_files: bool,
    pub query: String,
    pub commits: Vec<crate::bridge::workbench::history::CommitRow>,
    pub revision: Option<crate::bridge::workbench::history::Revision>,
    pub revision_files: Vec<String>,
    pub revision_path: String,
    pub revision_diff: String,
    /// Monotonic counter bumped whenever `revision_diff` changes.
    pub revision_diff_version: u64,
    pub history_request: u64,
    pub side_by_side: bool,
    pub hunk: usize,
    pub status: Option<GitState>,
    /// Monotonic counter bumped whenever `status` changes.
    pub status_version: u64,
    pub loading: bool,
    /// The change whose diff shows.
    pub selected: Option<(String, bool)>,
    pub diff: String,
    /// Monotonic counter bumped whenever `diff` changes.
    pub diff_version: u64,
    pub commit_message: String,
    pub pr: Option<PrSummary>,
    /// A stage / unstage / commit is running.
    pub busy: bool,
    pub error: Option<String>,
    pub last_commit: Option<String>,
    /// A path whose unstaged changes wait for the discard confirmation
    /// (`""` = every unstaged change).
    pub confirm_discard: Option<String>,
    /// The branch list is open.
    pub branch_menu: bool,
    pub branch_filter: &'static str,
    pub branch_catalog: Option<vantry_platform::git::refs::Catalog>,
    pub branch_feedback: String,
    pub branch_output: String,
    pub unmerged_branch: Option<String>,
}

impl GitPanel {
    /// The changes, staged first; a file changed in both shows twice.
    pub fn changes(&self) -> Vec<Change> {
        let Some(files) = self.status.as_ref().and_then(|s| s.files.as_ref()) else {
            return Vec::new();
        };
        let mut staged = Vec::new();
        let mut unstaged = Vec::new();
        for GitFile {
            path,
            index,
            worktree,
            ..
        } in files
        {
            let untracked = index == "?" && worktree == "?";
            if !untracked && index != " " && !index.is_empty() {
                staged.push(Change {
                    path: path.clone(),
                    code: index.clone(),
                    staged: true,
                });
            }
            if untracked || (worktree != " " && !worktree.is_empty()) {
                unstaged.push(Change {
                    path: path.clone(),
                    code: if untracked { "?".into() } else { worktree.clone() },
                    staged: false,
                });
            }
        }
        staged.extend(unstaged);
        staged
    }

    pub fn staged_count(&self) -> usize {
        self.changes().iter().filter(|c| c.staged).count()
    }

    pub fn refresh(&mut self) -> Load {
        self.loading = true;
        Load::GitStatus
    }

    pub fn on_status(&mut self, result: Result<GitState, String>) -> Option<Load> {
        self.loading = false;
        self.status_version += 1;
        match result {
            Ok(state) => {
                self.status = Some(state);
                self.error = None;
                // Keep the shown diff in step with the files.
                match self.selected.clone() {
                    Some((path, staged))
                        if self.changes().iter().any(|c| c.path == path && c.staged == staged) =>
                    {
                        Some(Load::Diff {
                            path: Some(path),
                            staged,
                        })
                    }
                    Some(_) => {
                        self.selected = None;
                        self.diff.clear();
                        self.diff_version += 1;
                        None
                    }
                    None => None,
                }
            }
            Err(error) => {
                self.error = Some(error);
                None
            }
        }
    }

    pub fn select(&mut self, path: &str, staged: bool) -> Load {
        self.hunk = 0;
        self.selected = Some((path.to_owned(), staged));
        self.diff.clear();
        self.diff_version += 1;
        Load::Diff {
            path: Some(path.to_owned()),
            staged,
        }
    }

    pub fn on_diff(&mut self, path: Option<&str>, staged: bool, result: Result<String, String>) {
        let current = self.selected.as_ref().map(|(p, s)| (p.as_str(), *s));
        if path.is_some() && current != path.map(|p| (p, staged)) {
            return;
        }
        match result {
            Ok(diff) => {
                self.diff = diff;
                self.diff_version += 1;
            }
            Err(error) => self.error = Some(error),
        }
    }

    /// Starts a stage / unstage / commit; false while another runs.
    pub fn begin(&mut self) -> bool {
        if self.busy {
            return false;
        }
        self.busy = true;
        self.error = None;
        true
    }

    /// The commit's message, when it can commit now.
    pub fn commit_ready(&self) -> Option<String> {
        let message = self.commit_message.trim();
        (!self.busy && !message.is_empty() && self.staged_count() > 0).then(|| message.to_owned())
    }

    /// The local branches other than the current one.
    pub fn other_branches(&self) -> Vec<String> {
        let Some(status) = &self.status else {
            return Vec::new();
        };
        status
            .branches
            .iter()
            .flatten()
            .filter(|b| **b != status.branch)
            .cloned()
            .collect()
    }

    /// The remote branches known to Git.
    pub fn remote_branches(&self) -> Vec<String> {
        self.status
            .as_ref()
            .map(|s| s.remote_branches.clone())
            .unwrap_or_default()
    }

    /// The paths a discard of `target` (a path, or `""` for every unstaged
    /// change) throws away.
    pub fn discard_paths(&self, target: &str) -> Vec<String> {
        let mut paths: Vec<String> = self
            .changes()
            .into_iter()
            .filter(|c| !c.staged && (target.is_empty() || c.path == target))
            .map(|c| c.path)
            .collect();
        paths.dedup();
        paths
    }

    /// A stage / unstage / commit ended; the status reloads.
    pub fn on_done(&mut self, result: Result<Option<String>, String>) -> Load {
        self.busy = false;
        self.branch_menu = false;
        match result {
            Ok(Some(commit)) => {
                self.last_commit = Some(commit);
                self.commit_message.clear();
            }
            Ok(None) => {}
            Err(error) => self.error = Some(error),
        }
        self.refresh()
    }

    pub fn on_pr(&mut self, result: Result<PrSummary, String>) {
        match result {
            Ok(pr) => self.pr = Some(pr),
            Err(error) => self.error = Some(error),
        }
    }
}

/// The most a session keeps of its output (older text is dropped).
pub const MAX_TERMINAL_BYTES: usize = 256 << 10;

/// One terminal tab.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TerminalSession {
    pub id: String,
    pub title: String,
    pub output: String,
    pub input: String,
    pub running: bool,
    pub last_exit: Option<i32>,
    pub history: Vec<String>,
    /// Where ↑ / ↓ is in the history.
    pub history_at: Option<usize>,
    /// Thread and checkout in which this interactive shell was started.
    pub shell_scope: Option<(String, String)>,
    /// Associated `vantry_terminal_run` request ID when launched for a tool task.
    pub request_id: Option<String>,
    /// Working directory reported back to the worker when the tool task exits.
    pub task_cwd: String,
}

/// The terminal panel.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct TerminalState {
    pub sessions: Vec<TerminalSession>,
    pub active: usize,
    next: u32,
}

impl TerminalState {
    /// Opens a session and shows it; returns its id.
    pub fn open(&mut self) -> String {
        self.next += 1;
        let id = format!("term-{}", self.next);
        self.sessions.push(TerminalSession {
            id: id.clone(),
            title: format!("终端 {}", self.next),
            output: String::new(),
            input: String::new(),
            running: false,
            last_exit: None,
            history: Vec::new(),
            history_at: None,
            shell_scope: None,
            request_id: None,
            task_cwd: String::new(),
        });
        self.active = self.sessions.len() - 1;
        id
    }

    /// Prefer the active shell when multiple tabs belong to this thread.
    pub fn select_shell(&mut self, scope: &(String, String)) -> String {
        if let Some(session) = self.current().filter(|s| s.shell_scope.as_ref() == Some(scope)) {
            return session.id.clone();
        }
        if let Some(pos) = self.sessions.iter().position(|s| s.shell_scope.as_ref() == Some(scope)) {
            self.active = pos;
            return self.sessions[pos].id.clone();
        }
        self.open()
    }

    /// Opens or activates a dedicated terminal tab for a `vantry_terminal_run` request.
    pub fn open_task(&mut self, request_id: &str, title: &str, cwd: &str) -> String {
        let id = format!("task-{request_id}");
        if let Some(pos) = self.sessions.iter().position(|s| s.id == id) {
            self.active = pos;
            return id;
        }
        let label = if title.trim().is_empty() {
            "交互终端".to_owned()
        } else {
            title.trim().to_owned()
        };
        self.sessions.push(TerminalSession {
            id: id.clone(),
            title: label,
            output: String::new(),
            input: String::new(),
            running: false,
            last_exit: None,
            history: Vec::new(),
            history_at: None,
            shell_scope: None,
            request_id: Some(request_id.to_owned()),
            task_cwd: cwd.to_owned(),
        });
        self.active = self.sessions.len() - 1;
        id
    }

    /// The shown session (opening one when there is none).
    pub fn ensure(&mut self) -> &mut TerminalSession {
        if self.sessions.is_empty() {
            self.open();
        }
        self.active = self.active.min(self.sessions.len() - 1);
        &mut self.sessions[self.active]
    }

    pub fn current(&self) -> Option<&TerminalSession> {
        self.sessions.get(self.active)
    }

    pub fn current_mut(&mut self) -> Option<&mut TerminalSession> {
        self.sessions.get_mut(self.active)
    }

    fn session_mut(&mut self, id: &str) -> Option<&mut TerminalSession> {
        self.sessions.iter_mut().find(|s| s.id == id)
    }

    /// Closes session `id`; returns whether it was running (interrupt it).
    pub fn close(&mut self, id: &str) -> bool {
        let Some(at) = self.sessions.iter().position(|s| s.id == id) else {
            return false;
        };
        let running = self.sessions.remove(at).running;
        if self.active >= at && self.active > 0 {
            self.active -= 1;
        }
        running
    }

    pub fn select(&mut self, id: &str) {
        if let Some(at) = self.sessions.iter().position(|s| s.id == id) {
            self.active = at;
        }
    }

    pub fn set_input(&mut self, text: &str) {
        let session = self.ensure();
        session.input = text.to_owned();
        session.history_at = None;
    }

    /// Runs the shown session's input: returns (session, command) to run,
    /// `None` while it runs something or the input is blank.
    pub fn submit(&mut self) -> Option<(String, String)> {
        let session = self.ensure();
        let command = session.input.trim().to_owned();
        if session.running || command.is_empty() {
            return None;
        }
        session.input.clear();
        session.history_at = None;
        if session.history.last() != Some(&command) {
            session.history.push(command.clone());
        }
        if command == "clear" {
            session.output.clear();
            return None;
        }
        push_capped(&mut session.output, &format!("$ {command}\n"));
        session.running = true;
        session.last_exit = None;
        Some((session.id.clone(), command))
    }

    /// Steps through the shown session's history (`-1` older, `1` newer).
    pub fn recall(&mut self, step: i32) {
        let session = self.ensure();
        if session.history.is_empty() {
            return;
        }
        let last = session.history.len() - 1;
        let at = match (session.history_at, step < 0) {
            (None, true) => Some(last),
            (None, false) => None,
            (Some(at), true) => Some(at.saturating_sub(1)),
            (Some(at), false) if at < last => Some(at + 1),
            (Some(_), false) => None,
        };
        session.history_at = at;
        session.input = at.map(|i| session.history[i].clone()).unwrap_or_default();
    }

    pub fn on_output(&mut self, id: &str, chunk: &str) {
        if let Some(session) = self.session_mut(id) {
            push_capped(&mut session.output, chunk);
        }
    }

    pub fn on_exit(&mut self, id: &str, result: Result<i32, String>) {
        if let Some(session) = self.session_mut(id) {
            session.running = false;
            match result {
                Ok(code) => {
                    session.last_exit = Some(code);
                    if code != 0 {
                        push_capped(&mut session.output, &format!("[退出码 {code}]\n"));
                    }
                }
                Err(error) => push_capped(&mut session.output, &format!("[错误] {error}\n")),
            }
        }
    }

    pub fn clear(&mut self) {
        if let Some(session) = self.sessions.get_mut(self.active) {
            session.output.clear();
        }
    }

    /// The shown session's id when it runs something (to interrupt).
    pub fn interruptible(&self) -> Option<String> {
        self.current().filter(|s| s.running).map(|s| s.id.clone())
    }
}

/// Appends `chunk`, dropping the oldest output past
/// [`MAX_TERMINAL_BYTES`] (at a line, then a char boundary).
fn push_capped(output: &mut String, chunk: &str) {
    output.push_str(chunk);
    if output.len() <= MAX_TERMINAL_BYTES {
        return;
    }
    let mut cut = output.len() - MAX_TERMINAL_BYTES;
    while !output.is_char_boundary(cut) {
        cut += 1;
    }
    if let Some(newline) = output[cut..].find('\n') {
        cut += newline + 1;
    }
    output.replace_range(..cut, "");
}

/// What the preview panel shows.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PreviewContent {
    /// Loading asynchronously.
    Loading,
    /// Load failed with a human-readable error.
    Error(String),
    /// Rendered Markdown (a document, a reply).
    Markdown(MarkdownDoc),
    /// Source with its language (syntax-colored and line-numbered in the view).
    Code { lang: String, text: String },
    /// A unified diff.
    Diff(String),
    /// A raster image (`data:image/...;base64,...`).
    Image {
        src: String,
        mime: String,
        size: u64,
        dimensions: Option<(u32, u32)>,
    },
    /// An SVG image (rendered graphic ↔ XML source).
    Svg {
        src: String,
        xml: String,
        size: u64,
    },
    /// An HTML document (sandboxed render ↔ source).
    Html {
        source: String,
        size: u64,
    },
    /// A binary file that cannot be rendered inline (PDF, ZIP, Office, etc.).
    Binary {
        mime: String,
        size: u64,
    },
}

/// The preview modal/panel state.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PreviewState {
    pub title: String,
    /// Full local path when backed by a local file (enables reveal/open externally).
    pub file_path: Option<String>,
    /// Optional 1-based inclusive line range to highlight & scroll to.
    pub highlight_lines: Option<(usize, usize)>,
    pub content: Option<PreviewContent>,
    /// Rendered view (Markdown / SVG / HTML) shows as source.
    pub source: bool,
    /// Wrap long lines in code view.
    pub wrap: bool,
    /// Whether text content was truncated at the preview byte limit.
    pub truncated: bool,
    /// Monotonic counter so stale async loads are ignored.
    pub generation: u64,
    raw: String,
}

impl Default for PreviewState {
    fn default() -> Self {
        Self {
            title: String::new(),
            file_path: None,
            highlight_lines: None,
            content: None,
            source: false,
            wrap: true,
            truncated: false,
            generation: 0,
            raw: String::new(),
        }
    }
}

impl PreviewState {
    /// Begins an async file/attachment preview load and returns its generation token.
    pub fn begin_loading(
        &mut self,
        title: &str,
        file_path: Option<String>,
        highlight_lines: Option<(usize, usize)>,
    ) -> u64 {
        self.generation = self.generation.wrapping_add(1);
        self.title = title.to_owned();
        self.file_path = file_path;
        self.highlight_lines = highlight_lines;
        self.content = Some(PreviewContent::Loading);
        self.source = false;
        self.truncated = false;
        self.raw.clear();
        self.generation
    }

    /// Shows `text` from `title` (a file name picks Markdown, HTML, diff, or code).
    pub fn show(&mut self, title: &str, text: &str) {
        self.generation = self.generation.wrapping_add(1);
        self.file_path = None;
        self.highlight_lines = None;
        self.truncated = false;
        self.apply_text(title, text, false);
    }

    /// Populates the preview from a loaded [`FileContent`].
    pub fn show_file(
        &mut self,
        title: &str,
        file: &FileContent,
        file_path: Option<String>,
        highlight_lines: Option<(usize, usize)>,
    ) {
        self.generation = self.generation.wrapping_add(1);
        self.title = title.to_owned();
        self.file_path = file_path;
        self.highlight_lines = highlight_lines;
        self.truncated = file.truncated;
        self.apply_file_content(title, file);
    }

    /// Finishes an async load if `generation` is still current.
    pub fn finish_file(
        &mut self,
        generation: u64,
        title: &str,
        file_path: Option<String>,
        result: Result<FileContent, String>,
    ) {
        if self.generation != generation {
            return;
        }
        self.title = title.to_owned();
        if file_path.is_some() {
            self.file_path = file_path;
        }
        match result {
            Ok(file) => {
                self.truncated = file.truncated;
                self.apply_file_content(title, &file);
            }
            Err(error) => {
                self.raw.clear();
                self.content = Some(PreviewContent::Error(error));
            }
        }
    }

    fn apply_file_content(&mut self, title: &str, file: &FileContent) {
        let name = if title.is_empty() { &file.path } else { title };
        if crate::bridge::workbench::is_svg_ext(name)
            && let Some(src) = &file.image_data_uri
        {
            self.raw = file.text.clone();
            self.source = self.highlight_lines.is_some();
            self.content = Some(PreviewContent::Svg {
                src: src.clone(),
                xml: file.text.clone(),
                size: file.size,
            });
            return;
        }
        if let Some(src) = &file.image_data_uri {
            self.raw.clear();
            self.source = false;
            self.content = Some(PreviewContent::Image {
                src: src.clone(),
                mime: file.mime.clone(),
                size: file.size,
                dimensions: file.dimensions,
            });
            return;
        }
        if file.binary {
            self.raw.clear();
            self.source = false;
            self.content = Some(PreviewContent::Binary {
                mime: if file.mime.is_empty() {
                    crate::bridge::workbench::mime_of(name).into()
                } else {
                    file.mime.clone()
                },
                size: file.size,
            });
            return;
        }
        self.apply_text(name, &file.text, self.highlight_lines.is_some());
    }

    fn apply_text(&mut self, title: &str, text: &str, prefer_source: bool) {
        let lower = title.to_lowercase();
        self.title = title.to_owned();
        self.raw = text.to_owned();
        self.source = false;
        self.content = Some(if crate::bridge::workbench::is_markdown_ext(&lower) || lower.is_empty() {
            self.source = prefer_source;
            PreviewContent::Markdown(MarkdownDoc::parse(text))
        } else if crate::bridge::workbench::is_html_ext(&lower) {
            self.source = prefer_source;
            PreviewContent::Html {
                source: text.to_owned(),
                size: text.len() as u64,
            }
        } else if crate::bridge::workbench::is_diff_ext(&lower) || text.starts_with("diff --git") {
            PreviewContent::Diff(text.to_owned())
        } else {
            PreviewContent::Code {
                lang: crate::bridge::workbench::ext_lower(&lower),
                text: text.to_owned(),
            }
        });
    }

    /// Rendered view ↔ source (Markdown, SVG, HTML).
    pub fn toggle_source(&mut self) {
        if self.has_source_toggle() || self.source {
            self.source = !self.source;
        }
    }

    pub fn has_source_toggle(&self) -> bool {
        matches!(
            self.content,
            Some(
                PreviewContent::Markdown(_)
                    | PreviewContent::Svg { .. }
                    | PreviewContent::Html { .. }
            )
        )
    }

    pub fn toggle_wrap(&mut self) {
        self.wrap = !self.wrap;
    }

    pub fn close(&mut self) {
        let next_gen = self.generation.wrapping_add(1);
        *self = PreviewState {
            generation: next_gen,
            ..PreviewState::default()
        };
    }

    pub fn raw(&self) -> &str {
        &self.raw
    }
}

/// The dock.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct WorkbenchState {
    pub root: String,
    pub project_generation: u64,
    commit_drafts: BTreeMap<String, String>,
    pub files: FilesState,
    pub git: GitPanel,
    pub terminal: TerminalState,
    pub preview: PreviewState,
}

impl WorkbenchState {
    pub fn select_project(&mut self, root: String) {
        if self.root == root {
            return;
        }
        if !self.git.commit_message.is_empty() {
            self.commit_drafts.insert(
                self.root.clone(),
                std::mem::take(&mut self.git.commit_message),
            );
        }
        let next_tree = self.files.tree_revision.wrapping_add(1);
        let next_status = self.git.status_version.wrapping_add(1);
        let next_diff = self.git.diff_version.wrapping_add(1);
        let next_rev_diff = self.git.revision_diff_version.wrapping_add(1);
        self.files = FilesState {
            tree_revision: next_tree,
            ..Default::default()
        };
        self.git = GitPanel {
            commit_message: self.commit_drafts.remove(&root).unwrap_or_default(),
            changes_split: true,
            status_version: next_status,
            diff_version: next_diff,
            revision_diff_version: next_rev_diff,
            ..Default::default()
        };
        self.preview = PreviewState::default();
        self.root = root;
        self.project_generation = self.project_generation.wrapping_add(1);
    }

    pub fn apply_project_result(&mut self, generation: u64, apply: impl FnOnce(&mut Self)) {
        if self.project_generation == generation {
            apply(self);
        }
    }

    /// What showing `tab` needs loaded first.
    pub fn on_tab(&mut self, tab: DockTab) -> Vec<Load> {
        match tab {
            DockTab::Files => self.files.open().into_iter().collect(),
            DockTab::Git if self.git.status.is_none() && !self.git.loading => {
                vec![self.git.refresh()]
            }
            DockTab::Terminal => {
                self.terminal.ensure();
                Vec::new()
            }
            _ => Vec::new(),
        }
    }

    /// Opens the selected file in the preview dialog.
    pub fn preview_selected_file(&mut self) -> bool {
        let Some(file) = self.files.preview.clone() else {
            return false;
        };
        let local_path = (!self.root.is_empty()).then(|| {
            std::path::Path::new(&self.root)
                .join(&file.path)
                .to_string_lossy()
                .into_owned()
        });
        let lines = self.files.highlight_lines;
        self.preview.show_file(&file.path, &file, local_path, lines);
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn switching_checkouts_keeps_commit_drafts_and_rejects_late_results() {
        let mut state = WorkbenchState::default();
        state.select_project("/work/one".into());
        let first = state.project_generation;
        state.git.commit_message = "第一次修改".into();
        state.select_project("/work/two".into());
        state.select_project("/work/one".into());
        assert_eq!(state.git.commit_message, "第一次修改");
        state.apply_project_result(first, |b| b.files.error = Some("late".into()));
        assert!(state.files.error.is_none());
        let current = state.project_generation;
        state.select_project("/work/one".into());
        state.apply_project_result(current, |b| b.files.error = Some("current".into()));
        assert_eq!(state.files.error.as_deref(), Some("current"));
    }

    #[test]
    fn terminal_command_selects_only_a_shell_in_the_matching_checkout_and_thread() {
        let mut state = TerminalState::default();
        let owner = ("thread-a".into(), "/checkout".into());
        let first = state.select_shell(&owner);
        state.ensure().shell_scope = Some(owner.clone());
        let second = state.open();
        state.ensure().shell_scope = Some(owner.clone());
        assert_eq!(state.select_shell(&owner), second);
        state.open_task("request", "login", "/checkout");
        assert_eq!(state.select_shell(&owner), first);
        for scope in [("thread-b".into(), "/checkout".into()), ("thread-a".into(), "/other".into())] {
            let id = state.select_shell(&scope);
            assert_ne!(id, first);
            assert_ne!(id, second);
            assert!(!id.starts_with("task-"));
        }
        state.close(&first);
        assert_eq!(state.select_shell(&owner), second);
    }

    fn entry(path: &str, is_dir: bool) -> DirEntry {
        DirEntry {
            name: path.rsplit('/').next().unwrap().into(),
            path: path.into(),
            is_dir,
            size: 0,
        }
    }

    #[test]
    fn the_tree_loads_folders_once_and_lists_open_ones() {
        let mut files = FilesState::default();
        assert_eq!(files.open(), Some(Load::Dir(String::new())));
        assert_eq!(files.open(), None, "already loading");
        files.on_dir("", Ok(vec![entry("src", true), entry("README.md", false)]));
        assert_eq!(files.rows().len(), 2);
        assert_eq!(files.toggle_dir("src"), Some(Load::Dir("src".into())));
        assert!(files.rows()[0].loading);
        files.on_dir("src", Ok(vec![entry("src/main.rs", false)]));
        let rows = files.rows();
        assert_eq!(rows.len(), 3);
        assert_eq!((rows[1].entry.path.as_str(), rows[1].depth), ("src/main.rs", 1));
        assert_eq!(files.toggle_dir("src"), None, "closing loads nothing");
        assert_eq!(files.rows().len(), 2);
        assert_eq!(files.toggle_dir("src"), None, "reopening uses the cache");
        assert_eq!(files.refresh().len(), 2);
        files.on_dir("missing", Err("gone".into()));
        assert_eq!(files.error.as_deref(), Some("gone"));
    }

    #[test]
    fn a_late_file_answer_for_another_file_is_ignored() {
        let mut files = FilesState::default();
        files.select_file("a.rs");
        files.select_file("b.rs");
        files.on_file("a.rs", Ok(FileContent { path: "a.rs".into(), text: "a".into(), ..Default::default() }));
        assert!(files.preview.is_none());
        files.on_file("b.rs", Ok(FileContent { path: "b.rs".into(), text: "b".into(), ..Default::default() }));
        assert_eq!(files.preview.as_ref().unwrap().text, "b");
        assert_eq!(files.select_file("b.rs"), None);
        let mut bench = WorkbenchState { files, ..Default::default() };
        assert!(bench.preview_selected_file());
        assert!(matches!(bench.preview.content, Some(PreviewContent::Code { ref lang, .. }) if lang == "rs"));
    }

    #[test]
    fn git_changes_split_staged_and_unstaged() {
        let mut git = GitPanel::default();
        assert_eq!(git.refresh(), Load::GitStatus);
        let state = crate::bridge::workbench::parse_git_status(
            "## main\nMM src/lib.rs\nA  new.rs\n?? notes.txt\n",
        );
        assert_eq!(git.on_status(Ok(state.clone())), None);
        let changes = git.changes();
        let summary: Vec<(String, bool)> = changes.iter().map(|c| (c.path.clone(), c.staged)).collect();
        assert_eq!(
            summary,
            [
                ("src/lib.rs".to_owned(), true),
                ("new.rs".to_owned(), true),
                ("src/lib.rs".to_owned(), false),
                ("notes.txt".to_owned(), false),
            ]
        );
        assert_eq!(git.staged_count(), 2);
        // A selected diff reloads with the status, and goes with its file.
        git.select("notes.txt", false);
        git.on_diff(Some("other"), false, Ok("x".into()));
        assert_eq!(git.diff, "", "an answer for another file");
        git.on_diff(Some("notes.txt"), false, Ok("+hi".into()));
        assert_eq!(git.diff, "+hi");
        assert!(matches!(git.on_status(Ok(state)), Some(Load::Diff { .. })));
        let clean = crate::bridge::workbench::parse_git_status("## main\n");
        assert_eq!(git.on_status(Ok(clean)), None);
        assert!(git.selected.is_none());
    }

    #[test]
    fn commits_need_a_message_and_staged_files() {
        let mut git = GitPanel::default();
        git.on_status(Ok(crate::bridge::workbench::parse_git_status("## main\n M a.rs\n")));
        git.commit_message = "修复".into();
        assert_eq!(git.commit_ready(), None, "nothing staged");
        git.on_status(Ok(crate::bridge::workbench::parse_git_status("## main\nM  a.rs\n")));
        assert_eq!(git.commit_ready().as_deref(), Some("修复"));
        assert!(git.begin());
        assert!(!git.begin(), "one at a time");
        assert_eq!(git.commit_ready(), None);
        assert_eq!(git.on_done(Ok(Some("abc 修复".into()))), Load::GitStatus);
        assert_eq!(git.last_commit.as_deref(), Some("abc 修复"));
        assert!(git.commit_message.is_empty());
        git.on_done(Err("hook failed".into()));
        assert_eq!(git.error.as_deref(), Some("hook failed"));
    }

    #[test]
    fn terminal_sessions_run_one_command_at_a_time() {
        let mut term = TerminalState::default();
        let first = term.ensure().id.clone();
        term.set_input("  ");
        assert_eq!(term.submit(), None);
        term.set_input("echo hi");
        assert_eq!(term.submit(), Some((first.clone(), "echo hi".into())));
        term.set_input("ls");
        assert_eq!(term.submit(), None, "busy");
        assert_eq!(term.interruptible(), Some(first.clone()));
        term.on_output(&first, "hi\n");
        term.on_exit(&first, Ok(0));
        assert_eq!(term.current().unwrap().output, "$ echo hi\nhi\n");
        term.set_input("false");
        term.submit();
        term.on_exit(&first, Ok(1));
        assert!(term.current().unwrap().output.ends_with("[退出码 1]\n"));
        term.recall(-1);
        assert_eq!(term.current().unwrap().input, "false");
        term.recall(-1);
        assert_eq!(term.current().unwrap().input, "echo hi");
        term.recall(1);
        term.recall(1);
        assert_eq!(term.current().unwrap().input, "");
        term.set_input("clear");
        assert_eq!(term.submit(), None);
        assert_eq!(term.current().unwrap().output, "");

        let second = term.open();
        assert_eq!(term.current().unwrap().id, second);
        term.select(&first);
        assert_eq!(term.active, 0);
        term.set_input("sleep 9");
        term.submit();
        assert!(term.close(&first), "closing a running session interrupts it");
        assert_eq!(term.sessions.len(), 1);
        term.on_output(&first, "late"); // A closed session's output goes nowhere.
        term.clear();
    }

    #[test]
    fn terminal_output_is_capped_at_a_line() {
        let mut out = String::new();
        let line = "x".repeat(1023) + "\n";
        for _ in 0..300 {
            push_capped(&mut out, &line);
        }
        assert!(out.len() <= MAX_TERMINAL_BYTES);
        assert!(out.starts_with('x'));
        let mut wide = "汉".repeat(MAX_TERMINAL_BYTES / 3 + 10);
        push_capped(&mut wide, "");
        assert!(wide.len() <= MAX_TERMINAL_BYTES);
    }

    #[test]
    fn the_preview_picks_markdown_diff_or_code() {
        let mut preview = PreviewState::default();
        preview.show("README.md", "# 标题");
        assert!(matches!(preview.content, Some(PreviewContent::Markdown(_))));
        preview.toggle_source();
        assert!(preview.source);
        preview.show("x.patch", "-a\n+b");
        assert!(matches!(preview.content, Some(PreviewContent::Diff(_))));
        preview.toggle_source();
        assert!(!preview.source, "only Markdown has a source view");
        preview.show("main.rs", "fn main() {}");
        assert_eq!(preview.raw(), "fn main() {}");
        assert_eq!(DockTab::parse("terminal"), DockTab::Terminal);
        assert_eq!(DockTab::parse("?"), DockTab::Files);
    }

    #[test]
    fn preview_handles_images_svg_html_binary_and_line_highlights() {
        let mut preview = PreviewState::default();
        let img = FileContent {
            path: "shot.png".into(),
            binary: true,
            image_data_uri: Some("data:image/png;base64,AAAA".into()),
            size: 1024,
            mime: "image/png".into(),
            dimensions: Some((800, 600)),
            ..Default::default()
        };
        preview.show_file("shot.png", &img, Some("/tmp/shot.png".into()), None);
        assert!(matches!(
            preview.content,
            Some(PreviewContent::Image {
                dimensions: Some((800, 600)),
                size: 1024,
                ..
            })
        ));
        assert_eq!(preview.file_path.as_deref(), Some("/tmp/shot.png"));

        let svg = FileContent {
            path: "icon.svg".into(),
            text: "<svg/>".into(),
            image_data_uri: Some("data:image/svg+xml;base64,PHN2Zy8+".into()),
            size: 6,
            mime: "image/svg+xml".into(),
            ..Default::default()
        };
        preview.show_file("icon.svg", &svg, None, None);
        assert!(matches!(preview.content, Some(PreviewContent::Svg { .. })));
        assert!(!preview.source);
        preview.toggle_source();
        assert!(preview.source);

        let md = FileContent {
            path: "guide.md".into(),
            text: "# Heading\nline 2\nline 3".into(),
            ..Default::default()
        };
        preview.show_file("guide.md", &md, None, Some((2, 3)));
        assert!(matches!(preview.content, Some(PreviewContent::Markdown(_))));
        assert!(preview.source, "targeting lines in Markdown opens source view");
        assert_eq!(preview.highlight_lines, Some((2, 3)));

        let pdf = FileContent {
            path: "report.pdf".into(),
            binary: true,
            size: 4096,
            mime: "application/pdf".into(),
            ..Default::default()
        };
        preview.show_file("report.pdf", &pdf, Some("/tmp/report.pdf".into()), None);
        assert!(matches!(
            preview.content,
            Some(PreviewContent::Binary { size: 4096, .. })
        ));
    }
}
