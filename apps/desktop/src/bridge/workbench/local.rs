//! The local workspace: files read directly, `git` and the shell as child
//! processes. Every child is `kill_on_drop`, awaited by the call that
//! started it, and killed by an interrupt; none outlives its call.

use std::{
    collections::HashMap,
    path::PathBuf,
    process::Stdio,
    sync::{Arc, Mutex},
};

use tokio::{io::AsyncReadExt, process::Command, sync::oneshot};
use vantry_protocol::{
    AppUpdateInfo, Attachment, GitState, MCPCatalog, ModelCatalog, ModelInfo, WorkbenchRequest,
    WorkbenchResponse,
};

use super::{
    DirEntry, FileContent, MAX_SEARCH_HITS, PrSummary, SearchHit, SettingsData, TerminalSink,
    WorkbenchBridge, actions::names, mime_of, parse_git_status, preview_of, resolve_in,
    sort_entries,
};
use crate::bridge::{BridgeError, BridgeFuture, ErrorCode};

/// The local workspace at `root`.
#[derive(Clone)]
pub struct LocalWorkbench {
    root: PathBuf,
    /// The model the command line named (the catalog's default).
    model: Option<String>,
    /// Running terminal commands' kill switches, by session.
    running: Arc<Mutex<HashMap<String, oneshot::Sender<()>>>>,
}

impl LocalWorkbench {
    pub fn new(root: PathBuf, model: Option<String>) -> Self {
        LocalWorkbench {
            root,
            model,
            running: Arc::new(Mutex::new(HashMap::new())),
        }
    }

    fn git(&self, args: Vec<String>) -> BridgeFuture<String> {
        let root = self.root.clone();
        Box::pin(async move { run_git(&root, &args).await })
    }
}

/// Runs `git -C root args…` and gives its stdout; a failure carries stderr.
async fn run_git(root: &std::path::Path, args: &[String]) -> Result<String, BridgeError> {
    let output = Command::new("git")
        .arg("-C")
        .arg(root)
        .args(args)
        .stdin(Stdio::null())
        .kill_on_drop(true)
        .output()
        .await
        .map_err(|e| BridgeError::unavailable(format!("无法运行 git：{e}")))?;
    if output.status.success() {
        Ok(String::from_utf8_lossy(&output.stdout).into_owned())
    } else {
        let stderr = String::from_utf8_lossy(&output.stderr).trim().to_owned();
        Err(BridgeError::new(
            ErrorCode::Other("git".into()),
            if stderr.is_empty() {
                format!("git {} 失败", args.first().map_or("", String::as_str))
            } else {
                stderr
            },
        ))
    }
}

fn io_error(error: std::io::Error) -> BridgeError {
    let code = match error.kind() {
        std::io::ErrorKind::NotFound => ErrorCode::NotFound,
        _ => ErrorCode::Unavailable,
    };
    BridgeError::new(code, error.to_string())
}

fn dashdash(mut args: Vec<String>, paths: Vec<String>) -> Vec<String> {
    args.push("--".into());
    args.extend(paths);
    args
}

impl WorkbenchBridge for LocalWorkbench {
    fn label(&self) -> String {
        self.root
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_else(|| self.root.display().to_string())
    }

    fn root(&self) -> String {
        self.root.display().to_string()
    }

    fn list_dir(&self, path: String) -> BridgeFuture<Vec<DirEntry>> {
        let root = self.root.clone();
        Box::pin(async move {
            let dir = resolve_in(&root, &path)?;
            let mut reader = tokio::fs::read_dir(&dir).await.map_err(io_error)?;
            let mut entries = Vec::new();
            while let Some(entry) = reader.next_entry().await.map_err(io_error)? {
                let name = entry.file_name().to_string_lossy().into_owned();
                if name == ".git" {
                    continue;
                }
                let meta = entry.metadata().await.map_err(io_error)?;
                entries.push(DirEntry {
                    path: if path.is_empty() {
                        name.clone()
                    } else {
                        format!("{}/{name}", path.trim_end_matches('/'))
                    },
                    name,
                    is_dir: meta.is_dir(),
                    size: meta.len(),
                });
            }
            sort_entries(&mut entries);
            Ok(entries)
        })
    }

    fn read_file(&self, path: String) -> BridgeFuture<FileContent> {
        let root = self.root.clone();
        Box::pin(async move {
            let file = resolve_in(&root, &path)?;
            let handle = tokio::fs::File::open(&file).await.map_err(io_error)?;
            let mut bytes = Vec::new();
            handle
                .take(super::MAX_PREVIEW_BYTES as u64 + 1)
                .read_to_end(&mut bytes)
                .await
                .map_err(io_error)?;
            Ok(preview_of(&path, &bytes))
        })
    }

    fn git_status(&self) -> BridgeFuture<GitState> {
        let root = self.root.clone();
        Box::pin(async move {
            let status = run_git(
                &root,
                &["status".into(), "--porcelain=v1".into(), "-b".into()],
            )
            .await?;
            let mut state = parse_git_status(&status);
            state.root = root.display().to_string();
            if let Ok(branches) = run_git(
                &root,
                &[
                    "for-each-ref".into(),
                    "--format=%(refname:short)".into(),
                    "refs/heads".into(),
                ],
            )
            .await
            {
                state.branches = Some(branches.lines().map(str::to_owned).collect());
            }
            Ok(state)
        })
    }

    fn git_diff(&self, path: Option<String>, staged: bool) -> BridgeFuture<String> {
        let mut args = vec!["diff".to_owned(), "--no-color".to_owned()];
        if staged {
            args.push("--cached".into());
        }
        self.git(dashdash(args, path.into_iter().collect()))
    }

    fn git_stage(&self, paths: Vec<String>) -> BridgeFuture<()> {
        let fut = self.git(dashdash(vec!["add".into()], paths));
        Box::pin(async move { fut.await.map(|_| ()) })
    }

    fn git_unstage(&self, paths: Vec<String>) -> BridgeFuture<()> {
        let fut = self.git(dashdash(vec!["reset".into(), "-q".into()], paths));
        Box::pin(async move { fut.await.map(|_| ()) })
    }

    fn git_commit(&self, message: String) -> BridgeFuture<String> {
        let fut = self.git(vec!["commit".into(), "-q".into(), "-m".into(), message]);
        let root = self.root.clone();
        Box::pin(async move {
            fut.await?;
            run_git(
                &root,
                &["log".into(), "-1".into(), "--pretty=format:%h %s".into()],
            )
            .await
        })
    }

    fn pr_summary(&self) -> BridgeFuture<PrSummary> {
        let root = self.root.clone();
        Box::pin(async move {
            let branch = run_git(
                &root,
                &["rev-parse".into(), "--abbrev-ref".into(), "HEAD".into()],
            )
            .await?
            .trim()
            .to_owned();
            let upstream = run_git(
                &root,
                &[
                    "rev-parse".into(),
                    "--abbrev-ref".into(),
                    "--symbolic-full-name".into(),
                    "@{upstream}".into(),
                ],
            )
            .await
            .map(|s| s.trim().to_owned())
            .unwrap_or_default();
            let range = if upstream.is_empty() {
                vec!["-n".to_owned(), "10".to_owned()]
            } else {
                vec![format!("{upstream}..HEAD")]
            };
            let mut args = vec!["log".to_owned(), "--pretty=format:%h %s".to_owned()];
            args.extend(range);
            let log = run_git(&root, &args).await.unwrap_or_default();
            Ok(pr_from_log(branch, upstream, &log))
        })
    }

    fn terminal_run(
        &self,
        session: String,
        command: String,
        sink: TerminalSink,
    ) -> BridgeFuture<i32> {
        let root = self.root.clone();
        let running = self.running.clone();
        Box::pin(async move {
            let mut child = shell(&command)
                .current_dir(&root)
                .stdin(Stdio::null())
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .kill_on_drop(true)
                .spawn()
                .map_err(|e| BridgeError::unavailable(format!("无法启动 shell：{e}")))?;
            let (kill, killed) = oneshot::channel();
            if let Some(previous) = running.lock().unwrap().insert(session.clone(), kill) {
                let _ = previous.send(());
            }
            let stdout = child.stdout.take();
            let stderr = child.stderr.take();
            let pump = |pipe: Option<tokio::process::ChildStdout>, sink: TerminalSink| async move {
                if let Some(mut pipe) = pipe {
                    let mut buf = [0u8; 4096];
                    while let Ok(n) = pipe.read(&mut buf).await {
                        if n == 0 || sink.send(String::from_utf8_lossy(&buf[..n]).into_owned()).is_err() {
                            break;
                        }
                    }
                }
            };
            let pump_err = |pipe: Option<tokio::process::ChildStderr>, sink: TerminalSink| async move {
                if let Some(mut pipe) = pipe {
                    let mut buf = [0u8; 4096];
                    while let Ok(n) = pipe.read(&mut buf).await {
                        if n == 0 || sink.send(String::from_utf8_lossy(&buf[..n]).into_owned()).is_err() {
                            break;
                        }
                    }
                }
            };
            let out = pump(stdout, sink.clone());
            let err = pump_err(stderr, sink.clone());
            let wait = async {
                let ((), (), status) = tokio::join!(out, err, child.wait());
                status
            };
            let code = tokio::select! {
                status = wait => status.ok().and_then(|s| s.code()).unwrap_or(-1),
                _ = killed => {
                    let _ = child.start_kill();
                    let _ = child.wait().await;
                    let _ = sink.send("^C\n".to_owned());
                    -1
                }
            };
            running.lock().unwrap().remove(&session);
            Ok(code)
        })
    }

    fn terminal_interrupt(&self, session: String) -> BridgeFuture<()> {
        let kill = self.running.lock().unwrap().remove(&session);
        Box::pin(async move {
            if let Some(kill) = kill {
                let _ = kill.send(());
            }
            Ok(())
        })
    }

    fn settings(&self) -> BridgeFuture<SettingsData> {
        let model = self.model.clone();
        Box::pin(async move {
            let ssh_hosts = local_ssh_hosts().await;
            Ok(SettingsData {
                models: local_catalog(model.as_deref()),
                mcp: MCPCatalog {
                    notice: "MCP 服务器由 Core 读取并管理；连接 Core 后在此显示。".into(),
                    servers: Some(Vec::new()),
                    ..MCPCatalog::default()
                },
                update: local_update(),
                ssh_hosts,
                ..SettingsData::default()
            })
        })
    }

    /// What the desktop answers without a Core: the command line's model,
    /// the SSH config's hosts, this build's version; nothing else.
    fn workbench(&self, request: WorkbenchRequest) -> BridgeFuture<WorkbenchResponse> {
        let model = self.model.clone();
        Box::pin(async move {
            let mut response = WorkbenchResponse::default();
            match request.action.as_str() {
                names::MODELS => response.catalog = Some(local_catalog(model.as_deref())),
                names::SSH_HOSTS => response.ssh_hosts = local_ssh_hosts().await,
                names::UPDATE_STATUS => response.update = Some(local_update()),
                action => {
                    return Err(BridgeError::new(
                        ErrorCode::NotReady,
                        format!("「{action}」需要连接支持工作台扩展的 Core"),
                    ));
                }
            }
            Ok(response)
        })
    }

    fn git_checkout(&self, branch: String) -> BridgeFuture<()> {
        let root = self.root.clone();
        Box::pin(async move {
            let branch = branch.trim().to_owned();
            if branch.is_empty() || branch.starts_with('-') {
                return Err(BridgeError::new(ErrorCode::InvalidCommand, "分支名无效"));
            }
            run_git(&root, &["checkout".into(), "-q".into(), branch]).await.map(|_| ())
        })
    }

    fn git_discard(&self, paths: Vec<String>) -> BridgeFuture<()> {
        let root = self.root.clone();
        Box::pin(async move {
            if paths.is_empty() {
                return Ok(());
            }
            let status = run_git(
                &root,
                &dashdash(vec!["status".into(), "--porcelain=v1".into()], paths.clone()),
            )
            .await?;
            let untracked: Vec<String> = parse_git_status(&status)
                .files
                .unwrap_or_default()
                .into_iter()
                .filter(|f| f.index == "?")
                .map(|f| f.path)
                .collect();
            let tracked: Vec<String> = paths.into_iter().filter(|p| !untracked.contains(p)).collect();
            if !tracked.is_empty() {
                run_git(&root, &dashdash(vec!["checkout".into()], tracked)).await?;
            }
            if !untracked.is_empty() {
                run_git(&root, &dashdash(vec!["clean".into(), "-f".into(), "-q".into()], untracked)).await?;
            }
            Ok(())
        })
    }

    fn search_files(&self, query: String) -> BridgeFuture<Vec<SearchHit>> {
        let root = self.root.clone();
        Box::pin(async move {
            tokio::task::spawn_blocking(move || search_tree(&root, &query))
                .await
                .map_err(|e| BridgeError::new(ErrorCode::Internal, e.to_string()))
        })
    }

    fn attach(&self, path: PathBuf) -> BridgeFuture<Attachment> {
        Box::pin(async move {
            let meta = tokio::fs::metadata(&path).await.map_err(io_error)?;
            if !meta.is_file() {
                return Err(BridgeError::new(
                    ErrorCode::InvalidCommand,
                    "只能附加文件",
                ));
            }
            let name = path
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            Ok(Attachment {
                id: format!("att-{}", uuid::Uuid::new_v4().simple()),
                mime: mime_of(&name).to_owned(),
                name,
                size: meta.len() as i64,
            })
        })
    }
}

/// The command line's model as the catalog.
fn local_catalog(model: Option<&str>) -> ModelCatalog {
    ModelCatalog {
        default: model.unwrap_or_default().to_owned(),
        models: Some(
            model
                .iter()
                .map(|id| ModelInfo {
                    id: (*id).to_owned(),
                    provider: id.split('/').next().unwrap_or_default().to_owned(),
                    name: (*id).to_owned(),
                    ..ModelInfo::default()
                })
                .collect(),
        ),
        ..ModelCatalog::default()
    }
}

fn local_update() -> AppUpdateInfo {
    AppUpdateInfo {
        current_version: env!("CARGO_PKG_VERSION").into(),
        ..AppUpdateInfo::default()
    }
}

async fn local_ssh_hosts() -> Vec<vantry_protocol::SSHHostInfo> {
    match std::env::var_os("HOME") {
        Some(home) => tokio::fs::read_to_string(PathBuf::from(home).join(".ssh/config"))
            .await
            .map(|text| parse_ssh_hosts(&text))
            .unwrap_or_default(),
        None => Vec::new(),
    }
}

/// Folders a search skips.
const SKIPPED_DIRS: &[&str] = &[".git", "node_modules", "target", ".agent", "dist", "build"];
/// The most files a search looks at, and the largest it reads.
const MAX_SEARCH_FILES: usize = 4000;
const MAX_SEARCH_BYTES: u64 = 512 << 10;

/// Paths containing `query` (case-insensitive), then matching lines.
pub(crate) fn search_tree(root: &std::path::Path, query: &str) -> Vec<SearchHit> {
    let needle = query.trim().to_lowercase();
    if needle.is_empty() {
        return Vec::new();
    }
    let mut files = Vec::new();
    let mut stack = vec![(root.to_path_buf(), String::new())];
    while let Some((dir, rel)) = stack.pop() {
        let Ok(reader) = std::fs::read_dir(&dir) else {
            continue;
        };
        let mut entries: Vec<_> = reader.flatten().collect();
        entries.sort_by_key(|e| e.file_name());
        for entry in entries {
            let name = entry.file_name().to_string_lossy().into_owned();
            let path = if rel.is_empty() { name.clone() } else { format!("{rel}/{name}") };
            let Ok(kind) = entry.file_type() else { continue };
            if kind.is_dir() {
                if !SKIPPED_DIRS.contains(&name.as_str()) {
                    stack.push((entry.path(), path));
                }
            } else if kind.is_file() && files.len() < MAX_SEARCH_FILES {
                files.push((entry.path(), path));
            }
        }
    }
    files.sort_by(|a, b| a.1.cmp(&b.1));
    let mut hits: Vec<SearchHit> = files
        .iter()
        .filter(|(_, path)| path.to_lowercase().contains(&needle))
        .take(MAX_SEARCH_HITS)
        .map(|(_, path)| SearchHit { path: path.clone(), ..SearchHit::default() })
        .collect();
    for (file, path) in &files {
        if hits.len() >= MAX_SEARCH_HITS {
            break;
        }
        if std::fs::metadata(file).map_or(true, |m| m.len() > MAX_SEARCH_BYTES) {
            continue;
        }
        let Ok(bytes) = std::fs::read(file) else { continue };
        if super::looks_binary(&bytes) {
            continue;
        }
        for (n, line) in String::from_utf8_lossy(&bytes).lines().enumerate() {
            if line.to_lowercase().contains(&needle) {
                hits.push(SearchHit {
                    path: path.clone(),
                    line: line.trim().chars().take(200).collect(),
                    line_number: n + 1,
                });
                if hits.len() >= MAX_SEARCH_HITS {
                    break;
                }
            }
        }
    }
    hits
}

/// The platform's shell running `command`.
fn shell(command: &str) -> Command {
    if cfg!(windows) {
        let mut c = Command::new("cmd");
        c.arg("/C").arg(command);
        c
    } else {
        let mut c = Command::new("sh");
        c.arg("-c").arg(command);
        c
    }
}

/// A pull request's text from `git log --pretty='%h %s'`.
pub(crate) fn pr_from_log(branch: String, base: String, log: &str) -> PrSummary {
    let commits: Vec<String> = log.lines().filter(|l| !l.trim().is_empty()).map(str::to_owned).collect();
    let subject = |line: &String| line.split_once(' ').map_or(line.clone(), |(_, s)| s.to_owned());
    let title = commits.last().map(subject).unwrap_or_else(|| branch.clone());
    let body = if commits.is_empty() {
        "（没有尚未推送的提交）".to_owned()
    } else {
        commits
            .iter()
            .rev()
            .map(|c| format!("- {}", subject(c)))
            .collect::<Vec<_>>()
            .join("\n")
    };
    PrSummary {
        branch,
        base,
        title,
        body,
        commits,
    }
}

/// `Host` entries of an OpenSSH config (no wildcards).
pub(crate) fn parse_ssh_hosts(text: &str) -> Vec<vantry_protocol::SSHHostInfo> {
    let mut hosts: Vec<vantry_protocol::SSHHostInfo> = Vec::new();
    // Whether the current `Host` block named a usable alias.
    let mut current = false;
    for line in text.lines() {
        let line = line.trim();
        let Some((key, value)) = line.split_once(char::is_whitespace) else {
            continue;
        };
        let value = value.trim();
        match key.to_ascii_lowercase().as_str() {
            "host" => {
                current = false;
                for alias in value.split_whitespace().filter(|a| !a.contains(['*', '?', '!'])) {
                    hosts.push(vantry_protocol::SSHHostInfo {
                        alias: alias.to_owned(),
                        ..Default::default()
                    });
                    current = true;
                }
            }
            _ if !current => {}
            "hostname" => {
                if let Some(last) = hosts.last_mut() {
                    last.host_name = value.to_owned();
                }
            }
            "user" => {
                if let Some(last) = hosts.last_mut() {
                    last.user = value.to_owned();
                }
            }
            "port" => {
                if let Some(last) = hosts.last_mut() {
                    last.port = value.parse().unwrap_or(0);
                }
            }
            _ => {}
        }
    }
    hosts
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;

    #[tokio::test]
    async fn lists_and_reads_inside_the_root_only() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir(dir.path().join("src")).unwrap();
        std::fs::write(dir.path().join("src/main.rs"), "fn main() {}\n").unwrap();
        std::fs::write(dir.path().join("README.md"), "# hi\n").unwrap();
        let bench = LocalWorkbench::new(dir.path().to_path_buf(), None);
        let root = bench.list_dir(String::new()).await.unwrap();
        let names: Vec<&str> = root.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(names, ["src", "README.md"]);
        let src = bench.list_dir("src".into()).await.unwrap();
        assert_eq!(src[0].path, "src/main.rs");
        let file = bench.read_file("src/main.rs".into()).await.unwrap();
        assert_eq!(file.text, "fn main() {}\n");
        assert!(bench.read_file("../outside".into()).await.is_err());
        assert_eq!(
            bench.read_file("missing.rs".into()).await.unwrap_err().code,
            ErrorCode::NotFound
        );
        let attachment = bench.attach(dir.path().join("README.md")).await.unwrap();
        assert_eq!((attachment.name.as_str(), attachment.size), ("README.md", 5));
        assert_eq!(attachment.mime, "text/markdown");
        drop(bench);
        dir.close().unwrap();
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn terminal_commands_stream_exit_and_interrupt() {
        let dir = tempfile::tempdir().unwrap();
        let bench = LocalWorkbench::new(dir.path().to_path_buf(), None);
        let (sink, mut out) = tokio::sync::mpsc::unbounded_channel();
        let code = bench
            .terminal_run("s1".into(), "echo hello; exit 3".into(), sink)
            .await
            .unwrap();
        assert_eq!(code, 3);
        let mut text = String::new();
        while let Ok(chunk) = out.try_recv() {
            text.push_str(&chunk);
        }
        assert_eq!(text, "hello\n");

        // A long command is killed by an interrupt and its call returns.
        let (sink, _out) = tokio::sync::mpsc::unbounded_channel();
        let long = tokio::spawn(bench.terminal_run("s2".into(), "sleep 30".into(), sink));
        // Wait until it registered.
        for _ in 0..200 {
            if bench.running.lock().unwrap().contains_key("s2") {
                break;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        bench.terminal_interrupt("s2".into()).await.unwrap();
        let code = tokio::time::timeout(Duration::from_secs(5), long)
            .await
            .expect("the interrupted command returns")
            .unwrap()
            .unwrap();
        assert_eq!(code, -1);
        assert!(bench.running.lock().unwrap().is_empty());
        drop(bench);
        dir.close().unwrap();
    }

    #[test]
    fn search_finds_names_then_lines_and_skips_noise() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("src")).unwrap();
        std::fs::create_dir_all(dir.path().join("node_modules/x")).unwrap();
        std::fs::write(dir.path().join("src/greet.rs"), "fn hello() {}\n// Greeting here\n").unwrap();
        std::fs::write(dir.path().join("README.md"), "say hello\n").unwrap();
        std::fs::write(dir.path().join("node_modules/x/greet.js"), "greet").unwrap();
        std::fs::write(dir.path().join("blob.bin"), b"greet\0\0").unwrap();
        let hits = search_tree(dir.path(), "GREET");
        assert_eq!(hits[0].path, "src/greet.rs");
        assert_eq!(hits[0].line_number, 0, "a name match first");
        assert!(hits.iter().any(|h| h.line_number == 2 && h.line.contains("Greeting")));
        assert!(hits.iter().all(|h| !h.path.starts_with("node_modules") && h.path != "blob.bin"));
        assert!(search_tree(dir.path(), "  ").is_empty());
        dir.close().unwrap();
    }

    #[tokio::test]
    async fn without_a_core_only_local_actions_answer() {
        let dir = tempfile::tempdir().unwrap();
        let bench = LocalWorkbench::new(dir.path().to_path_buf(), Some("openai/gpt".into()));
        let models = bench.workbench(super::super::actions::request(names::MODELS)).await.unwrap();
        assert_eq!(models.catalog.unwrap().default, "openai/gpt");
        let update = bench.workbench(super::super::actions::request(names::UPDATE_STATUS)).await.unwrap();
        assert_eq!(update.update.unwrap().current_version, env!("CARGO_PKG_VERSION"));
        let err = bench.workbench(super::super::actions::request(names::PLUGINS_LIST)).await.unwrap_err();
        assert_eq!(err.code, ErrorCode::NotReady);
        dir.close().unwrap();
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn git_checkout_and_discard_work_on_a_real_repository() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().to_path_buf();
        let git = |args: &[&str]| {
            let status = std::process::Command::new("git")
                .arg("-C")
                .arg(&root)
                .args(args)
                .env("GIT_CONFIG_NOSYSTEM", "1")
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status()
                .unwrap();
            assert!(status.success(), "git {args:?}");
        };
        git(&["init", "-q", "-b", "main"]);
        git(&["config", "user.email", "t@example.invalid"]);
        git(&["config", "user.name", "t"]);
        git(&["config", "commit.gpgsign", "false"]);
        std::fs::write(root.join("a.txt"), "one\n").unwrap();
        git(&["add", "a.txt"]);
        git(&["commit", "-q", "-m", "init"]);
        git(&["branch", "feat"]);
        let bench = LocalWorkbench::new(root.clone(), None);
        bench.git_checkout("feat".into()).await.unwrap();
        assert_eq!(bench.git_status().await.unwrap().branch, "feat");
        assert!(bench.git_checkout("--orphan".into()).await.is_err());
        std::fs::write(root.join("a.txt"), "changed\n").unwrap();
        std::fs::write(root.join("new.txt"), "fresh\n").unwrap();
        bench.git_discard(vec!["a.txt".into(), "new.txt".into()]).await.unwrap();
        assert_eq!(std::fs::read_to_string(root.join("a.txt")).unwrap(), "one\n");
        assert!(!root.join("new.txt").exists());
        assert!(bench.git_status().await.unwrap().files.unwrap().is_empty());
        drop(bench);
        dir.close().unwrap();
    }

    #[test]
    fn pr_text_and_ssh_hosts_parse() {
        let pr = pr_from_log("feat/x".into(), "origin/main".into(), "b2 second\na1 first\n");
        assert_eq!(pr.title, "first");
        assert_eq!(pr.body, "- first\n- second");
        assert_eq!(pr.commits.len(), 2);
        let empty = pr_from_log("main".into(), String::new(), "");
        assert_eq!(empty.title, "main");

        let hosts = parse_ssh_hosts(
            "Host build devbox\n  HostName 10.0.0.2\n  User ci\n  Port 2222\nHost *\n  User x\n",
        );
        assert_eq!(hosts.len(), 2);
        assert_eq!(hosts[1].alias, "devbox");
        assert_eq!((hosts[1].host_name.as_str(), hosts[1].port), ("10.0.0.2", 2222));
    }
}
