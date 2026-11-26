//! [`WorkbenchBridge`]: what the dock and settings need from the workspace
//! and the machine, beside the Core: the file tree and file contents, Git
//! (status, diff, stage, commit, PR summary), terminal sessions, the
//! settings' catalogs and attachments.
//!
//! - [`LocalWorkbench`] works on the local workspace directory (files read
//!   directly, `git` and the shell as child processes that never outlive
//!   their call);
//! - [`CoreWorkbench`] asks the Core (`POST /v1/workbench`) for models,
//!   Pi, MCP, plugins, remote access, the remote desktop, updates, usage,
//!   environments and projects, and works on the local directory for files,
//!   Git and the terminal; a Core without the workbench (or an action)
//!   falls back to [`LocalWorkbench`], so the window never goes blank;
//! - [`MockWorkbench`] is an in-memory workspace for `--mock` and tests.
//!
//! [`WorkbenchBridge::workbench`] is the generic action call; [`actions`]
//! builds its requests and reads its outputs.
//!
//! Types the protocol already has (`GitState`, `ModelCatalog`,
//! `PluginInfo`, `MCPCatalog`, `AppUpdateInfo`,
//! `EnvironmentInfo`, `Attachment`) are the protocol's.

pub mod actions;
pub mod history;
mod core_bench;
mod local;
mod mock;

use std::path::{Component, Path, PathBuf};

pub use core_bench::{CoreWorkbench, MAX_ATTACHMENT_BYTES};
pub use local::LocalWorkbench;
pub use mock::MockWorkbench;
use vantry_protocol::{
    AppUpdateInfo, Attachment, EnvironmentInfo, GitState, MCPCatalog, ModelCatalog,
    PiRuntimeStatus, PluginInfo, WorkbenchRequest, WorkbenchResponse,
};

use self::actions::{
    EnvironmentEntry, ProjectEntry, RdStatus, RemoteStatus, SandboxInfo, UsageSummary,
};

use super::{BridgeError, BridgeFuture, ErrorCode};

/// Where a terminal command's output goes, chunk by chunk.
pub type TerminalSink = tokio::sync::mpsc::UnboundedSender<String>;

/// One entry of a workspace directory.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DirEntry {
    pub name: String,
    /// Relative to the workspace root, `/`-separated.
    pub path: String,
    pub is_dir: bool,
    pub size: u64,
}

/// A file's contents for the read-only preview.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct FileContent {
    pub path: String,
    pub text: String,
    /// Only the first [`MAX_PREVIEW_BYTES`] are shown.
    pub truncated: bool,
    /// Not text: nothing to show.
    pub binary: bool,
}

/// The largest part of a file the preview reads.
pub const MAX_PREVIEW_BYTES: usize = 512 << 10;

/// What a pull request would say about the branch.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct PrSummary {
    pub branch: String,
    pub base: String,
    pub title: String,
    pub body: String,
    /// `<short id> <subject>`, newest first.
    pub commits: Vec<String>,
}

/// What the settings view shows.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct SettingsData {
    pub models: ModelCatalog,
    pub plugins: Vec<PluginInfo>,
    pub mcp: MCPCatalog,
    pub update: AppUpdateInfo,
    /// Paired machines (remote environments).
    pub peers: Vec<EnvironmentInfo>,
    /// The built-in Pi runtime (`pi.status`).
    pub pi: Option<PiRuntimeStatus>,
    pub sandbox: Option<SandboxInfo>,
    pub usage: Option<UsageSummary>,
    /// Remote access and pairing (`remote.status`).
    pub remote: Option<RemoteStatus>,
    /// The remote desktop (`rd.status`).
    pub rd: Option<RdStatus>,
    pub environments: Vec<EnvironmentEntry>,
    pub projects: Vec<ProjectEntry>,
    /// Sections the Core did not serve, and why (shown as a quiet note;
    /// what the desktop could read itself is shown instead).
    pub degraded: Vec<(String, String)>,
}

impl SettingsData {
    /// Folds an action's response into what is shown (catalogs the Core
    /// returns with its answer replace the old ones).
    pub fn absorb(&mut self, action: &str, response: &WorkbenchResponse) {
        use actions::names;
        if let Some(mcp) = &response.mcp {
            self.mcp = mcp.clone();
        }
        if let Some(catalog) = &response.catalog {
            self.models = catalog.clone();
        }
        if let Some(update) = &response.update {
            self.update = update.clone();
        }
        if let Some(pi) = &response.pi_runtime {
            self.pi = Some(pi.clone());
        }
        if action.starts_with("plugins.") && action != names::PLUGINS_RUN && action != names::PLUGINS_PANEL {
            self.plugins = response.plugins.clone();
        }
        match action {
            names::SANDBOX_GET | names::SANDBOX_SET => {
                if let Ok(info) = actions::read_output::<SandboxInfo>(response) {
                    self.sandbox = Some(info);
                }
            }
            names::USAGE_SUMMARY => {
                if let Ok(usage) = actions::read_output::<UsageSummary>(response) {
                    self.usage = Some(usage);
                }
            }
            names::REMOTE_STATUS
            | names::REMOTE_PAIR_CREATE
            | names::REMOTE_PAIR_REDEEM
            | names::REMOTE_PEERS_LIST
            | "remote.access.approval" | "remote.requests.approve" | "remote.requests.deny"
            | names::REMOTE_PEERS_REVOKE => {
                if let Ok(status) = RemoteStatus::read(response) {
                    // The paired machines are the remote desktop's peers.
                    self.peers = status
                        .peers()
                        .iter()
                        .map(|p| EnvironmentInfo {
                            id: p.id.clone(),
                            name: if p.name.is_empty() { p.id.clone() } else { p.name.clone() },
                            ..EnvironmentInfo::default()
                        })
                        .collect();
                    self.remote = Some(status);
                }
            }
            names::RD_STATUS | names::RD_SESSION_START | names::RD_SESSION_STOP => {
                if let Ok(status) = RdStatus::read(response) {
                    self.rd = Some(status);
                }
            }
            names::ENVIRONMENTS_LIST | names::ENVIRONMENTS_UPSERT | names::ENVIRONMENTS_REMOVE => {
                if let Ok(list) = EnvironmentEntry::read_list(response) {
                    self.environments = list;
                }
            }
            names::PROJECTS_LIST | names::PROJECTS_ADD | names::PROJECTS_REMOVE => {
                if let Ok(list) = ProjectEntry::read_list(response) {
                    self.projects = list;
                }
            }
            _ => {}
        }
        self.degraded.retain(|(section, _)| section != action);
    }
}

/// What a file search found.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct SearchHit {
    /// Relative to the workspace root.
    pub path: String,
    /// The matching line (empty for a name match).
    pub line: String,
    pub line_number: usize,
}

/// The workspace and the machine, for the dock and settings.
pub trait WorkbenchBridge: Send + Sync + 'static {
    /// The workspace's name.
    fn label(&self) -> String;
    /// The workspace's root (absolute where it is a local directory).
    fn root(&self) -> String;
    /// The entries of directory `path` (relative; empty is the root):
    /// folders first, then files, by name.
    fn list_dir(&self, path: String) -> BridgeFuture<Vec<DirEntry>>;
    /// File `path`'s contents, at most [`MAX_PREVIEW_BYTES`].
    fn read_file(&self, path: String) -> BridgeFuture<FileContent>;
    /// Branch, upstream distance, changed files.
    fn git_status(&self) -> BridgeFuture<GitState>;
    /// The unified diff of `path` (all files when `None`), staged or not.
    fn git_diff(&self, path: Option<String>, staged: bool) -> BridgeFuture<String>;
    fn git_log(&self, _branch: String, _path:String, _compare: bool) -> BridgeFuture<Vec<history::CommitRow>> { Box::pin(async {Err(BridgeError::unavailable("当前工作区没有本地 Git 日志"))}) }
    fn revision_files(&self, _revision: history::Revision) -> BridgeFuture<Vec<String>> { Box::pin(async {Err(BridgeError::unavailable("当前工作区没有本地 Git 对象"))}) }
    fn revision_diff(&self, _revision: history::Revision, _path: String) -> BridgeFuture<String> { Box::pin(async {Err(BridgeError::unavailable("当前工作区没有本地 Git 对象"))}) }
    fn git_apply_hunk(&self, _patch: String, _reverse: bool) -> BridgeFuture<()> { Box::pin(async {Err(BridgeError::unavailable("当前工作区不支持按块暂存"))}) }
    fn git_stage(&self, paths: Vec<String>) -> BridgeFuture<()>;
    fn git_unstage(&self, paths: Vec<String>) -> BridgeFuture<()>;
    /// Commits what is staged; resolves to the commit's summary line.
    fn git_commit(&self, message: String) -> BridgeFuture<String>;
    fn pr_summary(&self) -> BridgeFuture<PrSummary>;
    /// Runs `command` in terminal session `session` (in the workspace),
    /// streaming its output to `sink`; resolves to its exit code (-1 when
    /// it was interrupted or killed by a signal).
    fn terminal_run(&self, session: String, command: String, sink: TerminalSink)
    -> BridgeFuture<i32>;
    /// Interrupts what session `session` runs (nothing when it runs nothing).
    fn terminal_interrupt(&self, session: String) -> BridgeFuture<()>;
    fn terminal_close(&self, session: String) -> BridgeFuture<()> {
        self.terminal_interrupt(session)
    }
    /// Sends interactive input bytes to a running terminal session's PTY.
    fn terminal_write(&self, _session: String, _data: String) -> BridgeFuture<()> {
        Box::pin(async { Ok(()) })
    }
    /// Resizes a running terminal session's PTY window.
    fn terminal_resize(&self, _session: String, _rows: u16, _cols: u16) -> BridgeFuture<()> {
        Box::pin(async { Ok(()) })
    }
    /// Models, plugins, MCP servers, updates and peers.
    fn settings(&self, workspace: String) -> BridgeFuture<SettingsData>;
    /// Makes file `path` an attachment for the next message.
    fn attach(&self, path: PathBuf) -> BridgeFuture<Attachment>;
    /// A Core workbench action (`POST /v1/workbench`); [`actions`] builds
    /// the requests. Without a Core that serves it: `not_ready`.
    fn workbench(&self, request: WorkbenchRequest) -> BridgeFuture<WorkbenchResponse> {
        let action = request.action;
        Box::pin(async move {
            Err(BridgeError::new(
                ErrorCode::NotReady,
                format!("「{action}」需要连接支持工作台扩展的 Core"),
            ))
        })
    }
    /// Switches the workspace to branch `branch`.
    fn git_checkout(&self, branch: String) -> BridgeFuture<()> {
        let _ = branch;
        Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "此工作区不支持切换分支")) })
    }
    /// Throws away the unstaged changes of `paths` (untracked files are
    /// removed).
    fn git_discard(&self, paths: Vec<String>) -> BridgeFuture<()> {
        let _ = paths;
        Box::pin(async { Err(BridgeError::new(ErrorCode::NotReady, "此工作区不支持撤销改动")) })
    }
    /// Files whose path contains `query`, then lines that do (at most
    /// [`MAX_SEARCH_HITS`]).
    fn search_files(&self, query: String) -> BridgeFuture<Vec<SearchHit>> {
        let _ = query;
        Box::pin(async { Ok(Vec::new()) })
    }
}

/// The most hits a search returns.
pub const MAX_SEARCH_HITS: usize = 80;

/// `path` (relative, `/`-separated) under `root`, refusing anything that
/// leaves it (absolute paths, `..`).
pub fn resolve_in(root: &Path, path: &str) -> Result<PathBuf, BridgeError> {
    let relative = Path::new(path);
    let mut out = root.to_path_buf();
    for component in relative.components() {
        match component {
            Component::Normal(part) => out.push(part),
            Component::CurDir => {}
            _ => {
                return Err(BridgeError::new(
                    ErrorCode::InvalidCommand,
                    format!("路径不在工作区内：{path}"),
                ));
            }
        }
    }
    Ok(out)
}

/// A MIME type from a file name's extension (attachments).
pub fn mime_of(name: &str) -> &'static str {
    let ext = name
        .rsplit_once('.')
        .map(|(_, ext)| ext.to_ascii_lowercase())
        .unwrap_or_default();
    match ext.as_str() {
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "pdf" => "application/pdf",
        "json" => "application/json",
        "md" | "markdown" => "text/markdown",
        "txt" | "log" | "rs" | "go" | "ts" | "js" | "py" | "toml" | "yaml" | "yml" | "sh" => {
            "text/plain"
        }
        _ => "application/octet-stream",
    }
}

/// Whether `bytes` look like text (no NUL in what was read).
pub fn looks_binary(bytes: &[u8]) -> bool {
    bytes.iter().take(8192).any(|b| *b == 0)
}

/// `bytes` as preview text: lossy UTF-8, cut at [`MAX_PREVIEW_BYTES`].
pub fn preview_of(path: &str, bytes: &[u8]) -> FileContent {
    if looks_binary(bytes) {
        return FileContent {
            path: path.to_owned(),
            binary: true,
            ..FileContent::default()
        };
    }
    let truncated = bytes.len() > MAX_PREVIEW_BYTES;
    let shown = &bytes[..bytes.len().min(MAX_PREVIEW_BYTES)];
    FileContent {
        path: path.to_owned(),
        text: String::from_utf8_lossy(shown).into_owned(),
        truncated,
        binary: false,
    }
}

/// Sorts directory entries: folders first, then by name.
pub fn sort_entries(entries: &mut [DirEntry]) {
    entries.sort_by(|a, b| {
        b.is_dir
            .cmp(&a.is_dir)
            .then_with(|| a.name.to_lowercase().cmp(&b.name.to_lowercase()))
    });
}

/// Parses `git status --porcelain=v1 -b` into the protocol's state.
pub fn parse_git_status(text: &str) -> GitState {
    let mut state = GitState {
        files: Some(Vec::new()),
        status: text.to_owned(),
        ..GitState::default()
    };
    let nul = text.contains('\0');
    let records: Box<dyn Iterator<Item=&str>> = if nul {Box::new(text.split_terminator('\0'))}else{Box::new(text.lines())};
    let mut records=records.peekable();
    while let Some(line)=records.next() {
        if let Some(head) = line.strip_prefix("## ") {
            let (branches, tracking) = match head.split_once(" [") {
                Some((b, t)) => (b, t.trim_end_matches(']')),
                None => (head, ""),
            };
            match branches.split_once("...") {
                Some((local, remote)) => {
                    state.branch = local.to_owned();
                    state.remote_branch = remote.to_owned();
                }
                None => {
                    state.branch = branches
                        .trim_start_matches("No commits yet on ")
                        .to_owned();
                }
            }
            for part in tracking.split(", ") {
                if let Some(n) = part.strip_prefix("ahead ") {
                    state.ahead = n.trim().parse().unwrap_or(0);
                } else if let Some(n) = part.strip_prefix("behind ") {
                    state.behind = n.trim().parse().unwrap_or(0);
                }
            }
            continue;
        }
        if line.len() < 4 {
            continue;
        }
        let (codes, path) = line.split_at(2);
        let path = path.strip_prefix(' ').unwrap_or(path);
        let index = codes[..1].to_owned();
        let worktree = codes[1..2].to_owned();
        if index == "U" || worktree == "U" || codes == "AA" || codes == "DD" {
            state.conflicts = true;
        }
        let (original, path) = if nul {
            let original = if matches!(index.as_str(),"R"|"C") || matches!(worktree.as_str(),"R"|"C") { records.next().unwrap_or_default() } else {""};
            (original.to_owned(),path.to_owned())
        } else {match path.split_once(" -> ") {
            Some((from, to)) => (unquote(from),unquote(to)),
            None => (String::new(),unquote(path)),
        }};
        if let Some(files) = state.files.as_mut() {
            files.push(vantry_protocol::GitFile {
                path,
                original_path: original,
                index,
                worktree,
            });
        }
    }
    state
}

fn unquote(path: &str) -> String {
    path.strip_prefix('"')
        .and_then(|p| p.strip_suffix('"'))
        .unwrap_or(path)
        .to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nul_status_preserves_unicode_spaces_newlines_and_rename_order() {
        let state=parse_git_status("## main\0R  新 名\0old name\0??  a\nb -> c\0");
        let files=state.files.unwrap();
        assert_eq!((&files[0].path,&files[0].original_path),(&"新 名".to_owned(),&"old name".to_owned()));
        assert_eq!(files[1].path," a\nb -> c");
    }

    #[test]
    fn paths_stay_inside_the_workspace() {
        let root = Path::new("/w");
        assert_eq!(resolve_in(root, "src/main.rs").unwrap(), Path::new("/w/src/main.rs"));
        assert_eq!(resolve_in(root, "").unwrap(), Path::new("/w"));
        assert!(resolve_in(root, "../etc/passwd").is_err());
        assert!(resolve_in(root, "/etc/passwd").is_err());
        assert!(resolve_in(root, "src/../../x").is_err());
    }

    #[test]
    fn git_status_parses_branch_tracking_and_files() {
        let state = parse_git_status(
            "## main...origin/main [ahead 2, behind 1]\nM  src/lib.rs\n M README.md\nR  old.rs -> new.rs\n?? notes.txt\nUU conflict.rs\n",
        );
        assert_eq!(state.branch, "main");
        assert_eq!(state.remote_branch, "origin/main");
        assert_eq!((state.ahead, state.behind), (2, 1));
        assert!(state.conflicts);
        let files = state.files.unwrap();
        assert_eq!(files.len(), 5);
        assert_eq!((files[0].index.as_str(), files[0].worktree.as_str()), ("M", " "));
        assert_eq!((files[1].index.as_str(), files[1].worktree.as_str()), (" ", "M"));
        assert_eq!(files[2].original_path, "old.rs");
        assert_eq!(files[2].path, "new.rs");
        assert_eq!(files[3].index, "?");
        let fresh = parse_git_status("## No commits yet on main\n");
        assert_eq!(fresh.branch, "main");
    }

    #[test]
    fn previews_cut_and_detect_binary() {
        assert!(preview_of("a.bin", b"ab\0cd").binary);
        let big = vec![b'x'; MAX_PREVIEW_BYTES + 10];
        let preview = preview_of("big.txt", &big);
        assert!(preview.truncated);
        assert_eq!(preview.text.len(), MAX_PREVIEW_BYTES);
        assert_eq!(mime_of("Shot.PNG"), "image/png");
        assert_eq!(mime_of("notes"), "application/octet-stream");
    }

    #[test]
    fn folders_sort_first() {
        let mut entries = vec![
            DirEntry { name: "b.rs".into(), path: "b.rs".into(), is_dir: false, size: 1 },
            DirEntry { name: "src".into(), path: "src".into(), is_dir: true, size: 0 },
            DirEntry { name: "A.md".into(), path: "A.md".into(), is_dir: false, size: 1 },
        ];
        sort_entries(&mut entries);
        let names: Vec<&str> = entries.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(names, ["src", "A.md", "b.rs"]);
    }
}
