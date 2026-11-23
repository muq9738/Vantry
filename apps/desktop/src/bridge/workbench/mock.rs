//! An in-memory workspace: `--mock` and tests. A small Cargo project, a Git
//! index whose files move between unstaged and staged, a commit log, a
//! terminal that understands a few commands (and one that runs until
//! interrupted), and settings catalogs with something in each.

use std::{
    collections::BTreeMap,
    path::PathBuf,
    sync::{Arc, Mutex},
    time::Duration,
};

use tokio::sync::Notify;
use vantry_protocol::{
    AppUpdateInfo, Attachment, EnvironmentInfo, GitFile, GitState, MCPCatalog, MCPServerInfo,
    ModelCatalog, ModelInfo, PluginInfo, PluginNavItem, SSHHostInfo,
};

use super::{
    DirEntry, FileContent, PrSummary, SettingsData, TerminalSink, WorkbenchBridge, mime_of,
    preview_of, sort_entries,
};
use crate::bridge::{BridgeError, BridgeFuture, ErrorCode};

#[derive(Default)]
struct Repo {
    files: BTreeMap<String, String>,
    /// path → (index code, worktree code)
    changes: BTreeMap<String, (String, String)>,
    commits: Vec<String>,
    /// Sessions with an interrupt pending.
    interrupted: Vec<String>,
}

/// The in-memory workspace. Clones share it.
#[derive(Clone)]
pub struct MockWorkbench {
    repo: Arc<Mutex<Repo>>,
    wake: Arc<Notify>,
}

impl Default for MockWorkbench {
    fn default() -> Self {
        Self::demo()
    }
}

impl MockWorkbench {
    pub fn demo() -> Self {
        let mut repo = Repo::default();
        for (path, text) in [
            ("Cargo.toml", "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2024\"\n"),
            ("README.md", "# demo\n\n一个演示工程。\n"),
            ("src/main.rs", "fn main() {\n    println!(\"hello, vantry\");\n}\n"),
            ("src/lib.rs", "pub fn add(a: i32, b: i32) -> i32 {\n    a + b\n}\n"),
            ("docs/notes.md", "## 笔记\n\n- 第一条\n- 第二条\n"),
        ] {
            repo.files.insert(path.into(), text.into());
        }
        repo.changes.insert("src/main.rs".into(), (" ".into(), "M".into()));
        repo.changes.insert("docs/notes.md".into(), ("?".into(), "?".into()));
        repo.commits = vec!["a1b2c3d 初始化工程".into(), "d4e5f6a 添加 lib".into()];
        MockWorkbench {
            repo: Arc::new(Mutex::new(repo)),
            wake: Arc::new(Notify::new()),
        }
    }

    fn diff_of(path: &str, text: &str) -> String {
        let mut out = format!("diff --git a/{path} b/{path}\n--- a/{path}\n+++ b/{path}\n@@ -1,3 +1,3 @@\n");
        for (n, line) in text.lines().enumerate() {
            if n == 1 {
                out.push_str("-    println!(\"hello\");\n");
                out.push_str(&format!("+{line}\n"));
            } else {
                out.push_str(&format!(" {line}\n"));
            }
        }
        out
    }
}

fn ok<T: Send + 'static>(value: T) -> BridgeFuture<T> {
    Box::pin(async move { Ok(value) })
}

impl WorkbenchBridge for MockWorkbench {
    fn label(&self) -> String {
        "demo".into()
    }

    fn root(&self) -> String {
        "/mock/demo".into()
    }

    fn list_dir(&self, path: String) -> BridgeFuture<Vec<DirEntry>> {
        let repo = self.repo.lock().unwrap();
        let prefix = if path.is_empty() {
            String::new()
        } else {
            format!("{}/", path.trim_end_matches('/'))
        };
        let mut entries: BTreeMap<String, DirEntry> = BTreeMap::new();
        for (file, text) in &repo.files {
            let Some(rest) = file.strip_prefix(&prefix) else {
                continue;
            };
            let (name, is_dir) = match rest.split_once('/') {
                Some((dir, _)) => (dir.to_owned(), true),
                None => (rest.to_owned(), false),
            };
            entries.entry(name.clone()).or_insert(DirEntry {
                path: format!("{prefix}{name}"),
                name,
                is_dir,
                size: if is_dir { 0 } else { text.len() as u64 },
            });
        }
        if entries.is_empty() && !path.is_empty() {
            return Box::pin(async { Err(BridgeError::new(ErrorCode::NotFound, "no such folder")) });
        }
        let mut entries: Vec<DirEntry> = entries.into_values().collect();
        sort_entries(&mut entries);
        ok(entries)
    }

    fn read_file(&self, path: String) -> BridgeFuture<FileContent> {
        let repo = self.repo.lock().unwrap();
        match repo.files.get(&path) {
            Some(text) => ok(preview_of(&path, text.as_bytes())),
            None => Box::pin(async { Err(BridgeError::new(ErrorCode::NotFound, "no such file")) }),
        }
    }

    fn git_status(&self) -> BridgeFuture<GitState> {
        let repo = self.repo.lock().unwrap();
        let files = repo
            .changes
            .iter()
            .map(|(path, (index, worktree))| GitFile {
                path: path.clone(),
                index: index.clone(),
                worktree: worktree.clone(),
                ..GitFile::default()
            })
            .collect();
        ok(GitState {
            files: Some(files),
            root: "/mock/demo".into(),
            branch: "feat/demo".into(),
            remote_branch: "origin/feat/demo".into(),
            ahead: repo.commits.len().saturating_sub(2) as i64,
            branches: Some(vec!["main".into(), "feat/demo".into()]),
            ..GitState::default()
        })
    }

    fn git_diff(&self, path: Option<String>, staged: bool) -> BridgeFuture<String> {
        let repo = self.repo.lock().unwrap();
        let mut out = String::new();
        for (file, (index, worktree)) in &repo.changes {
            if path.as_ref().is_some_and(|p| p != file) {
                continue;
            }
            let side = if staged { index } else { worktree };
            if side == " " || side.is_empty() || (staged && side == "?") {
                continue;
            }
            let text = repo.files.get(file).cloned().unwrap_or_default();
            if side == "?" {
                out.push_str(&format!("diff --git a/{file} b/{file}\nnew file\n--- /dev/null\n+++ b/{file}\n"));
                for line in text.lines() {
                    out.push_str(&format!("+{line}\n"));
                }
            } else {
                out.push_str(&Self::diff_of(file, &text));
            }
        }
        ok(out)
    }

    fn git_stage(&self, paths: Vec<String>) -> BridgeFuture<()> {
        let mut repo = self.repo.lock().unwrap();
        for path in paths {
            if let Some((index, worktree)) = repo.changes.get_mut(&path) {
                *index = if worktree == "?" { "A".into() } else { worktree.clone() };
                *worktree = " ".into();
            }
        }
        ok(())
    }

    fn git_unstage(&self, paths: Vec<String>) -> BridgeFuture<()> {
        let mut repo = self.repo.lock().unwrap();
        for path in paths {
            if let Some((index, worktree)) = repo.changes.get_mut(&path)
                && index != " "
                && index != "?"
            {
                *worktree = if index == "A" { "?".into() } else { index.clone() };
                *index = if worktree == "?" { "?".into() } else { " ".into() };
            }
        }
        ok(())
    }

    fn git_commit(&self, message: String) -> BridgeFuture<String> {
        let mut repo = self.repo.lock().unwrap();
        let staged: Vec<String> = repo
            .changes
            .iter()
            .filter(|(_, (index, _))| index != " " && index != "?")
            .map(|(path, _)| path.clone())
            .collect();
        if staged.is_empty() {
            return Box::pin(async {
                Err(BridgeError::new(ErrorCode::Conflict, "没有已暂存的改动"))
            });
        }
        if message.trim().is_empty() {
            return Box::pin(async {
                Err(BridgeError::new(ErrorCode::InvalidCommand, "提交说明不能为空"))
            });
        }
        for path in staged {
            let keep = repo
                .changes
                .get(&path)
                .is_some_and(|(_, worktree)| worktree != " ");
            if keep {
                if let Some(entry) = repo.changes.get_mut(&path) {
                    entry.0 = " ".into();
                }
            } else {
                repo.changes.remove(&path);
            }
        }
        let line = format!(
            "{:07x} {}",
            0x1000000 + repo.commits.len() * 7919,
            message.lines().next().unwrap_or("")
        );
        repo.commits.push(line.clone());
        ok(line)
    }

    fn pr_summary(&self) -> BridgeFuture<PrSummary> {
        let repo = self.repo.lock().unwrap();
        let log: Vec<String> = repo.commits.iter().rev().cloned().collect();
        ok(super::local::pr_from_log(
            "feat/demo".into(),
            "origin/main".into(),
            &log.join("\n"),
        ))
    }

    fn terminal_run(
        &self,
        session: String,
        command: String,
        sink: TerminalSink,
    ) -> BridgeFuture<i32> {
        let this = self.clone();
        Box::pin(async move {
            this.repo.lock().unwrap().interrupted.retain(|s| *s != session);
            let command = command.trim().to_owned();
            let (program, rest) = command.split_once(' ').unwrap_or((command.as_str(), ""));
            match program {
                "echo" => {
                    let _ = sink.send(format!("{rest}\n"));
                    Ok(0)
                }
                "ls" => {
                    let entries = this.list_dir(rest.trim().to_owned()).await?;
                    for entry in entries {
                        let _ = sink.send(format!(
                            "{}{}\n",
                            entry.name,
                            if entry.is_dir { "/" } else { "" }
                        ));
                    }
                    Ok(0)
                }
                "pwd" => {
                    let _ = sink.send(format!("{}\n", this.root()));
                    Ok(0)
                }
                "cat" => match this.read_file(rest.trim().to_owned()).await {
                    Ok(file) => {
                        let _ = sink.send(file.text);
                        Ok(0)
                    }
                    Err(err) => {
                        let _ = sink.send(format!("cat: {}\n", err.message));
                        Ok(1)
                    }
                },
                // Streams until interrupted.
                "yes" | "watch" | "tail" => {
                    let mut n = 0u64;
                    loop {
                        let notified = this.wake.notified();
                        if this.repo.lock().unwrap().interrupted.contains(&session) {
                            let _ = sink.send("^C\n".into());
                            this.repo.lock().unwrap().interrupted.retain(|s| *s != session);
                            return Ok(-1);
                        }
                        n += 1;
                        if sink.send(format!("y {n}\n")).is_err() {
                            return Ok(-1);
                        }
                        tokio::select! {
                            () = notified => {}
                            () = tokio::time::sleep(Duration::from_millis(200)) => {}
                        }
                    }
                }
                "" => Ok(0),
                other => {
                    let _ = sink.send(format!("mock: {other}: command not found\n"));
                    Ok(127)
                }
            }
        })
    }

    fn terminal_interrupt(&self, session: String) -> BridgeFuture<()> {
        self.repo.lock().unwrap().interrupted.push(session);
        self.wake.notify_waiters();
        ok(())
    }

    fn settings(&self) -> BridgeFuture<SettingsData> {
        let model = |id: &str, name: &str, provider: &str| ModelInfo {
            id: id.into(),
            provider: provider.into(),
            name: name.into(),
            reasoning: true,
            context_window: 200_000,
            ..ModelInfo::default()
        };
        ok(SettingsData {
            models: ModelCatalog {
                models: Some(vec![
                    model("anthropic/claude-sonnet-5-5", "Claude Sonnet 5.5", "anthropic"),
                    model("anthropic/claude-opus-5-5", "Claude Opus 5.5", "anthropic"),
                    model("openai/gpt-5.5", "GPT-5.5", "openai"),
                ]),
                default: "anthropic/claude-sonnet-5-5".into(),
                thinking_levels: vec!["low".into(), "medium".into(), "high".into()],
                ..ModelCatalog::default()
            },
            plugins: vec![
                PluginInfo {
                    id: "remote-desktop".into(),
                    name: "远程桌面".into(),
                    version: "0.3.0".into(),
                    description: "查看并控制已配对主机的屏幕".into(),
                    builtin: true,
                    navigation: vec![PluginNavItem {
                        title: "远程桌面".into(),
                        page: "remote-desktop".into(),
                        native_panel: "remote-desktop".into(),
                        ..PluginNavItem::default()
                    }],
                    ..PluginInfo::default()
                },
                PluginInfo {
                    id: "usage-meter".into(),
                    name: "用量统计".into(),
                    version: "1.2.0".into(),
                    description: "按项目汇总令牌与费用".into(),
                    has_update: true,
                    latest_version: "1.3.0".into(),
                    ..PluginInfo::default()
                },
            ],
            mcp: MCPCatalog {
                user_file: "~/.pi/agent/mcp.json".into(),
                project_trusted: true,
                codemode_default: true,
                servers: Some(vec![
                    MCPServerInfo {
                        name: "magpie".into(),
                        scope: "user".into(),
                        transport: "stdio".into(),
                        enabled: true,
                        exposure: "codemode".into(),
                        live_state: "ready".into(),
                        tools: vec!["web_search".into(), "read_url".into()],
                        ..MCPServerInfo::default()
                    },
                    MCPServerInfo {
                        name: "figma".into(),
                        scope: "project".into(),
                        transport: "http".into(),
                        endpoint: "http://127.0.0.1:3845/mcp".into(),
                        enabled: false,
                        exposure: "direct".into(),
                        ..MCPServerInfo::default()
                    },
                ]),
                notice: String::new(),
                ..MCPCatalog::default()
            },
            update: AppUpdateInfo {
                current_version: env!("CARGO_PKG_VERSION").into(),
                latest_version: "0.1.43".into(),
                available: true,
                delta_available: true,
                delta_size: 3_200_000,
                notes: "NSH 桌面工作台预览".into(),
                ..AppUpdateInfo::default()
            },
            peers: vec![EnvironmentInfo {
                id: "peer-studio".into(),
                name: "工作室 Mac".into(),
                os: "darwin".into(),
                arch: "arm64".into(),
                provider_id: "pi".into(),
                ..EnvironmentInfo::default()
            }],
            ssh_hosts: vec![SSHHostInfo {
                alias: "build-box".into(),
                host_name: "10.0.0.8".into(),
                user: "ci".into(),
                reachable: Some(true),
                latency_ms: 18,
                ..SSHHostInfo::default()
            }],
        })
    }

    fn attach(&self, path: PathBuf) -> BridgeFuture<Attachment> {
        let name = path
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_else(|| "file".into());
        ok(Attachment {
            id: format!("att-{}", uuid::Uuid::new_v4().simple()),
            mime: mime_of(&name).into(),
            name,
            size: 1024,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn the_tree_lists_by_folder() {
        let bench = MockWorkbench::demo();
        let root = bench.list_dir(String::new()).await.unwrap();
        let names: Vec<&str> = root.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(names[..2], ["docs", "src"]);
        let src = bench.list_dir("src".into()).await.unwrap();
        assert_eq!(src.len(), 2);
        assert!(bench.list_dir("nope".into()).await.is_err());
    }

    #[tokio::test]
    async fn stage_commit_and_unstage_move_files() {
        let bench = MockWorkbench::demo();
        bench.git_stage(vec!["src/main.rs".into()]).await.unwrap();
        let state = bench.git_status().await.unwrap();
        let main = state.files.as_ref().unwrap().iter().find(|f| f.path == "src/main.rs").unwrap();
        assert_eq!((main.index.as_str(), main.worktree.as_str()), ("M", " "));
        assert!(bench.git_diff(None, true).await.unwrap().contains("+++ b/src/main.rs"));
        bench.git_unstage(vec!["src/main.rs".into()]).await.unwrap();
        assert_eq!(bench.git_diff(None, true).await.unwrap(), "");
        assert!(bench.git_commit("x".into()).await.is_err(), "nothing staged");
        bench.git_stage(vec!["src/main.rs".into(), "docs/notes.md".into()]).await.unwrap();
        let line = bench.git_commit("修复问候语".into()).await.unwrap();
        assert!(line.ends_with("修复问候语"));
        assert!(bench.git_status().await.unwrap().files.unwrap().is_empty());
        assert_eq!(bench.pr_summary().await.unwrap().commits.len(), 3);
    }

    #[tokio::test]
    async fn the_terminal_runs_and_interrupts() {
        let bench = MockWorkbench::demo();
        let (sink, mut out) = tokio::sync::mpsc::unbounded_channel();
        assert_eq!(bench.terminal_run("t".into(), "echo 你好".into(), sink.clone()).await.unwrap(), 0);
        assert_eq!(out.recv().await.unwrap(), "你好\n");
        assert_eq!(bench.terminal_run("t".into(), "nope".into(), sink.clone()).await.unwrap(), 127);
        let long = tokio::spawn(bench.terminal_run("t".into(), "yes".into(), sink));
        assert!(out.recv().await.unwrap().contains("command not found"));
        assert_eq!(out.recv().await.unwrap(), "y 1\n");
        bench.terminal_interrupt("t".into()).await.unwrap();
        let code = tokio::time::timeout(Duration::from_secs(5), long).await.unwrap().unwrap().unwrap();
        assert_eq!(code, -1);
    }
}
