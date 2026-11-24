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
    ModelCatalog, ModelInfo, PiRuntimeStatus, PluginInfo, PluginNavItem,
    WorkbenchRequest, WorkbenchResponse,
};

use super::{
    DirEntry, FileContent, PrSummary, SearchHit, SettingsData, TerminalSink, WorkbenchBridge,
    actions::{
        self, EnvironmentEntry, ModelUsage, ProjectEntry, RdPermissions, RdStatus, RemoteClient,
        RemotePeer, RemoteStatus, SandboxInfo, UsageSummary, names,
    },
    mime_of, preview_of, sort_entries,
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
    branch: String,
    branches: Vec<String>,
    /// What the settings show; actions change it.
    settings: SettingsData,
    /// How many times each plugin's panel action ran.
    plugin_runs: BTreeMap<String, u32>,
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
        repo.branch = "feat/demo".into();
        repo.branches = vec!["main".into(), "feat/demo".into()];
        repo.settings = demo_settings();
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
            branch: repo.branch.clone(),
            remote_branch: format!("origin/{}", repo.branch),
            ahead: repo.commits.len().saturating_sub(2) as i64,
            branches: Some(repo.branches.clone()),
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

    fn settings(&self, _workspace: String) -> BridgeFuture<SettingsData> {
        ok(self.repo.lock().unwrap().settings.clone())
    }

    fn workbench(&self, request: WorkbenchRequest) -> BridgeFuture<WorkbenchResponse> {
        let result = self.repo.lock().unwrap().action(&request);
        Box::pin(async move { result })
    }

    fn git_checkout(&self, branch: String) -> BridgeFuture<()> {
        let mut repo = self.repo.lock().unwrap();
        if !repo.branches.contains(&branch) {
            return Box::pin(async move {
                Err(BridgeError::new(ErrorCode::NotFound, format!("没有分支 {branch}")))
            });
        }
        repo.branch = branch;
        ok(())
    }

    fn git_discard(&self, paths: Vec<String>) -> BridgeFuture<()> {
        let mut repo = self.repo.lock().unwrap();
        for path in paths {
            let untracked = repo.changes.get(&path).is_some_and(|(i, _)| i == "?");
            if untracked {
                repo.changes.remove(&path);
                repo.files.remove(&path);
            } else if let Some(entry) = repo.changes.get_mut(&path) {
                entry.1 = " ".into();
                if entry.0 == " " {
                    repo.changes.remove(&path);
                }
            }
        }
        ok(())
    }

    fn search_files(&self, query: String) -> BridgeFuture<Vec<SearchHit>> {
        let repo = self.repo.lock().unwrap();
        let needle = query.trim().to_lowercase();
        if needle.is_empty() {
            return ok(Vec::new());
        }
        let mut hits: Vec<SearchHit> = repo
            .files
            .keys()
            .filter(|p| p.to_lowercase().contains(&needle))
            .map(|p| SearchHit { path: p.clone(), ..SearchHit::default() })
            .collect();
        for (path, text) in &repo.files {
            for (n, line) in text.lines().enumerate() {
                if line.to_lowercase().contains(&needle) {
                    hits.push(SearchHit {
                        path: path.clone(),
                        line: line.trim().to_owned(),
                        line_number: n + 1,
                    });
                }
            }
        }
        hits.truncate(super::MAX_SEARCH_HITS);
        ok(hits)
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

/// The demo's settings: something in every section.
fn demo_settings() -> SettingsData {
    let model = |id: &str, name: &str, provider: &str| ModelInfo {
        id: id.into(),
        provider: provider.into(),
        name: name.into(),
        reasoning: true,
        context_window: 200_000,
        ..ModelInfo::default()
    };
    SettingsData {
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
    pi: Some(PiRuntimeStatus {
        active_source: "builtin".into(),
        active_version: "0.12.31".into(),
        ..PiRuntimeStatus::default()
    }),
    sandbox: Some(SandboxInfo {
        mode: "workspace-write".into(),
        supported: true,
        env_override: false,
    }),
    usage: Some(UsageSummary {
        threads: 6,
        runs: 23,
        completed_runs: 20,
        failed_runs: 1,
        input_tokens: 412_000,
        output_tokens: 96_500,
        cache_read_tokens: 880_000,
        total_tokens: 1_388_500,
        cache_hit_rate: 0.68,
        total_cost_usd: 3.42,
        avg_ttft_ms: 840,
        models: vec![
            ModelUsage {
                model: "anthropic/claude-sonnet-5-5".into(),
                runs: 17,
                completed: 16,
                total_tokens: 1_020_000,
                cost_usd: 2.31,
                avg_ttft_ms: 790,
                ..ModelUsage::default()
            },
            ModelUsage {
                model: "openai/gpt-5.5".into(),
                runs: 6,
                completed: 4,
                failed: 1,
                total_tokens: 368_500,
                cost_usd: 1.11,
                avg_ttft_ms: 980,
                ..ModelUsage::default()
            },
        ],
        ..UsageSummary::default()
    }),
    remote: Some(RemoteStatus {
        enabled: true,
        connected: true,
        relay_connected: true,
        name: "本机".into(),
        relay: "https://relay.example".into(),
        peers: Some(vec![RemotePeer {
            id: "peer-studio".into(),
            name: "工作室 Mac".into(),
            reachable: Some(true),
            mode: "direct".into(),
            latency_ms: 9,
            ..RemotePeer::default()
        }]),
        clients: Some(vec![RemoteClient {
            id: "client-phone".into(),
            name: "iPhone".into(),
            created_at: "2026-11-01T09:00:00Z".into(),
        }]),
        ..RemoteStatus::default()
    }),
    rd: Some(RdStatus {
        permissions: Some(RdPermissions {
            screen: Some(true),
            accessibility: Some(true),
        }),
        ..RdStatus::default()
    }),
    environments: vec![EnvironmentEntry {
        id: "env-build".into(),
        name: "构建机".into(),
        kind: "peer".into(),
        host: "build-box".into(),
        ..EnvironmentEntry::default()
    }],
    projects: vec![ProjectEntry {
        id: "demo".into(),
        name: "demo".into(),
        path: "/mock/demo".into(),
    }],
    degraded: Vec::new(),
    }
}

fn invalid(message: impl Into<String>) -> BridgeError {
    BridgeError::new(ErrorCode::InvalidCommand, message)
}

fn output_of<T: serde::Serialize>(value: &T) -> WorkbenchResponse {
    WorkbenchResponse {
        output: serde_json::to_string(value).unwrap_or_default(),
        ..WorkbenchResponse::default()
    }
}

/// The usage meter plugin's panel after `runs` refreshes.
fn usage_panel(runs: u32) -> String {
    serde_json::json!({
        "version": 1,
        "title": "用量统计",
        "badge": if runs == 0 { "就绪" } else { "已刷新" },
        "tone": "success",
        "widgets": [
            {"kind": "kv", "title": "本周", "entries": [
                {"key": "令牌", "value": format!("{}k", 1388 + runs * 12)},
                {"key": "费用", "value": "$3.42"},
                {"key": "刷新次数", "value": runs.to_string()}
            ]},
            {"kind": "group", "title": "导出", "children": [
                {"kind": "form", "fields": [{"key": "days", "label": "天数", "value": "7"}]},
                {"kind": "actions", "actions": [
                    {"id": "refresh", "label": "刷新", "tone": "primary", "input": {"op": "refresh"}},
                    {"id": "export", "label": "导出 CSV", "input": {"op": "export"}}
                ]}
            ]},
            {"kind": "list", "title": "最近项目", "items": ["demo", "vantry"]}
        ]
    })
    .to_string()
}

impl Repo {
    /// One workbench action on the demo's state.
    fn action(&mut self, request: &WorkbenchRequest) -> Result<WorkbenchResponse, BridgeError> {
        let mut out = WorkbenchResponse::default();
        let settings = &mut self.settings;
        match request.action.as_str() {
            names::MODELS => out.catalog = Some(settings.models.clone()),
            names::PI_STATUS => out.pi_runtime = settings.pi.clone(),
            names::PI_SETUP_SAVE => {
                let input = request.input.clone().unwrap_or_default();
                if input["apiKey"].as_str().is_none_or(str::is_empty) {
                    return Err(invalid("请填写 API Key"));
                }
                // The key is checked, never kept by the demo.
                if !request.model.is_empty() {
                    settings.models.default = request.model.clone();
                    settings.models.configured_default = request.model.clone();
                }
                out.catalog = Some(settings.models.clone());
                out.output = "Pi 配置已保存。".into();
            }
            names::MCP_LIST => out.mcp = Some(settings.mcp.clone()),
            names::MCP_UPSERT => {
                let input = request.input.clone().unwrap_or_default();
                let servers = settings.mcp.servers.get_or_insert_with(Vec::new);
                match servers.iter_mut().find(|s| s.name == request.name) {
                    Some(server) => {
                        if let Some(enabled) = input["enabled"].as_bool() {
                            server.enabled = enabled;
                        }
                        if let Some(url) = input["url"].as_str() {
                            server.endpoint = url.to_owned();
                        }
                    }
                    None => {
                        if request.name.is_empty() {
                            return Err(invalid("缺少服务器名称"));
                        }
                        servers.push(MCPServerInfo {
                            name: request.name.clone(),
                            scope: input["scope"].as_str().unwrap_or("user").to_owned(),
                            transport: input["transport"].as_str().unwrap_or("stdio").to_owned(),
                            endpoint: input["url"].as_str().unwrap_or_default().to_owned(),
                            enabled: input["enabled"].as_bool().unwrap_or(true),
                            exposure: "codemode".into(),
                            ..MCPServerInfo::default()
                        });
                    }
                }
                out.mcp = Some(settings.mcp.clone());
            }
            names::MCP_REMOVE => {
                let servers = settings.mcp.servers.get_or_insert_with(Vec::new);
                let before = servers.len();
                servers.retain(|s| s.name != request.name);
                if servers.len() == before {
                    return Err(BridgeError::new(ErrorCode::NotFound, format!("没有 MCP 服务器 {}", request.name)));
                }
                out.mcp = Some(settings.mcp.clone());
            }
            names::MCP_PROBE => {
                for server in settings.mcp.servers.iter_mut().flatten() {
                    if request.name.is_empty() || server.name == request.name {
                        if server.enabled {
                            server.live_state = "ready".into();
                            server.live_error.clear();
                            if server.tools.is_empty() {
                                server.tools = vec!["ping".into()];
                            }
                        } else {
                            server.live_state = "disabled".into();
                        }
                    }
                }
                settings.mcp.probed = true;
                out.mcp = Some(settings.mcp.clone());
            }
            names::MCP_TRUST => {
                settings.mcp.project_trusted = true;
                out.mcp = Some(settings.mcp.clone());
            }
            names::SANDBOX_GET => out = output_of(&settings.sandbox.clone().unwrap_or_default()),
            names::SANDBOX_SET => {
                let mode = request.message.clone();
                if !actions::SANDBOX_MODES.iter().any(|(m, _, _)| *m == mode) {
                    return Err(invalid("沙箱模式参数无效"));
                }
                let sandbox = settings.sandbox.get_or_insert_with(SandboxInfo::default);
                sandbox.mode = mode;
                out = output_of(sandbox);
            }
            names::PLUGINS_LIST => out.plugins = settings.plugins.clone(),
            names::PLUGINS_ENABLE | names::PLUGINS_DISABLE => {
                let plugin = settings
                    .plugins
                    .iter_mut()
                    .find(|p| p.id == request.plugin_id)
                    .ok_or_else(|| BridgeError::new(ErrorCode::NotFound, "插件不存在"))?;
                plugin.disabled = request.action == names::PLUGINS_DISABLE;
                out.plugins = settings.plugins.clone();
            }
            names::PLUGINS_PANEL | names::PLUGINS_RUN => {
                let plugin = settings
                    .plugins
                    .iter()
                    .find(|p| p.id == request.plugin_id)
                    .ok_or_else(|| BridgeError::new(ErrorCode::NotFound, "插件不存在"))?;
                if plugin.disabled {
                    return Err(BridgeError::new(ErrorCode::Conflict, format!("插件「{}」已停用", plugin.name)));
                }
                let runs = self.plugin_runs.entry(request.plugin_id.clone()).or_default();
                if request.action == names::PLUGINS_RUN {
                    let op = request.input.as_ref().and_then(|i| i["op"].as_str()).unwrap_or("refresh");
                    if op == "export" {
                        let days = request
                            .input
                            .as_ref()
                            .and_then(|i| i["values"]["days"].as_str())
                            .unwrap_or("7");
                        out.output = format!("已导出最近 {days} 天的用量到 usage.csv");
                        return Ok(out);
                    }
                    *runs += 1;
                }
                out.output = usage_panel(*runs);
            }
            names::REMOTE_STATUS | names::REMOTE_PEERS_LIST => out = output_of(&settings.remote.clone().unwrap_or_default()),
            names::REMOTE_PAIR_CREATE => {
                let remote = settings.remote.get_or_insert_with(RemoteStatus::default);
                remote.enabled = true;
                remote.pairing_code = format!("SESSION-{}", &uuid::Uuid::new_v4().simple().to_string()[..6].to_uppercase());
                remote.pairing_state = "waiting".into();
                out = output_of(remote);
            }
            names::REMOTE_PAIR_REDEEM => {
                if !request.message.starts_with("SESSION-") && !request.message.starts_with("vantry://") {
                    return Err(invalid("配对码无效或已过期"));
                }
                let remote = settings.remote.get_or_insert_with(RemoteStatus::default);
                let id = format!("peer-{}", remote.peers().len() + 1);
                remote.peers.get_or_insert_with(Vec::new).push(RemotePeer {
                    name: if request.name.is_empty() { id.clone() } else { request.name.clone() },
                    id,
                    reachable: Some(true),
                    ..RemotePeer::default()
                });
                out = output_of(remote);
            }
            names::REMOTE_PEERS_REVOKE => {
                let remote = settings.remote.get_or_insert_with(RemoteStatus::default);
                remote.peers.get_or_insert_with(Vec::new).retain(|p| p.id != request.name);
                remote.clients.get_or_insert_with(Vec::new).retain(|c| c.id != request.name);
                out = output_of(remote);
            }
            names::RD_STATUS => out = output_of(&settings.rd.clone().unwrap_or_default()),
            names::RD_SESSION_START => {
                if request.name.is_empty() {
                    return Err(invalid("请选择要连接的主机"));
                }
                let rd = settings.rd.get_or_insert_with(RdStatus::default);
                rd.session_peer = request.name.clone();
                rd.session_state = "connected".into();
                rd.session_error.clear();
                rd.width = 1920;
                rd.height = 1080;
                rd.fps = 30;
                out = output_of(rd);
            }
            names::RD_SESSION_STOP => {
                let rd = settings.rd.get_or_insert_with(RdStatus::default);
                rd.session_peer.clear();
                rd.session_state = "idle".into();
                out = output_of(rd);
            }
            names::UPDATE_STATUS | names::UPDATE_CHECK => out.update = Some(settings.update.clone()),
            names::UPDATE_DOWNLOAD => {
                if !settings.update.available {
                    return Err(invalid("当前已是最新版本"));
                }
                settings.update.staged = true;
                settings.update.staged_version = settings.update.latest_version.clone();
                settings.update.used_delta = settings.update.delta_available;
                settings.update.can_auto_restart = true;
                out.update = Some(settings.update.clone());
                out.output = "更新已下载并完成签名校验，点击「重启并更新」即可安装。".into();
            }
            names::UPDATE_INSTALL => {
                if !settings.update.staged {
                    return Err(invalid("请先下载更新"));
                }
                out.update = Some(settings.update.clone());
                out.output = "演示模式：不会真正重启。".into();
            }
            names::USAGE_SUMMARY => out = output_of(&settings.usage.clone().unwrap_or_default()),
            names::ENVIRONMENTS_LIST => out = output_of(&serde_json::json!({ "environments": settings.environments })),
            names::ENVIRONMENTS_UPSERT => {
                let entry: EnvironmentEntry = request
                    .input
                    .clone()
                    .and_then(|v| serde_json::from_value(v).ok())
                    .ok_or_else(|| invalid("环境配置无效"))?;
                settings.environments.retain(|e| e.id != entry.id);
                settings.environments.push(entry);
                out = output_of(&serde_json::json!({ "environments": settings.environments }));
            }
            names::ENVIRONMENTS_REMOVE => {
                settings.environments.retain(|e| e.id != request.name);
                out = output_of(&serde_json::json!({ "environments": settings.environments }));
            }
            names::PROJECTS_LIST => out = output_of(&serde_json::json!({ "projects": settings.projects })),
            names::PROJECTS_ADD => {
                if request.workspace.is_empty() {
                    return Err(invalid("缺少项目目录"));
                }
                settings.projects.push(ProjectEntry {
                    id: request.workspace.clone(),
                    name: request.name.clone(),
                    path: request.workspace.clone(),
                });
                out = output_of(&serde_json::json!({ "projects": settings.projects }));
            }
            names::PROJECTS_REMOVE => {
                settings.projects.retain(|p| p.id != request.name);
                out = output_of(&serde_json::json!({ "projects": settings.projects }));
            }
            names::ATTACHMENT_IMPORT => {
                use base64::Engine;
                let data = base64::engine::general_purpose::STANDARD
                    .decode(&request.data)
                    .map_err(|_| invalid("附件数据无效"))?;
                out.attachment = Some(Attachment {
                    id: format!("att-{}", uuid::Uuid::new_v4().simple()),
                    mime: mime_of(&request.name).into(),
                    name: request.name.clone(),
                    size: data.len() as i64,
                });
            }
            _ => return Err(invalid("未知工作台操作")),
        }
        Ok(out)
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
    async fn demo_actions_change_what_settings_show() {
        let bench = MockWorkbench::demo();
        let run = |r| bench.workbench(r);
        let mut data = bench.settings(String::new()).await.unwrap();
        // MCP: switch off, add, probe, remove.
        let response = run(actions::mcp_set_enabled("magpie", false)).await.unwrap();
        data.absorb(names::MCP_UPSERT, &response);
        assert!(!data.mcp.servers.as_ref().unwrap()[0].enabled);
        let spec = actions::McpServerSpec { name: "docs".into(), transport: "http".into(), url: "https://d.example/mcp".into(), enabled: true, ..Default::default() };
        data.absorb(names::MCP_UPSERT, &run(actions::mcp_upsert(&spec)).await.unwrap());
        data.absorb(names::MCP_PROBE, &run(actions::mcp_named(names::MCP_PROBE, "docs")).await.unwrap());
        let docs = data.mcp.servers.as_ref().unwrap().iter().find(|s| s.name == "docs").unwrap();
        assert_eq!(docs.live_state, "ready");
        data.absorb(names::MCP_REMOVE, &run(actions::mcp_named(names::MCP_REMOVE, "docs")).await.unwrap());
        assert_eq!(data.mcp.servers.as_ref().unwrap().len(), 2);
        assert!(run(actions::mcp_named(names::MCP_REMOVE, "docs")).await.is_err());

        // Plugins: disable blocks the panel, enable brings it back, a run refreshes it.
        data.absorb(names::PLUGINS_DISABLE, &run(actions::plugin_toggle("usage-meter", false)).await.unwrap());
        assert!(data.plugins.iter().find(|p| p.id == "usage-meter").unwrap().disabled);
        assert_eq!(run(actions::plugin_panel("usage-meter", "main")).await.unwrap_err().code, ErrorCode::Conflict);
        run(actions::plugin_toggle("usage-meter", true)).await.unwrap();
        let panel = run(actions::plugin_panel("usage-meter", "main")).await.unwrap();
        assert_eq!(actions::PanelView::parse(&panel.output).unwrap().badge, "就绪");
        let ran = run(actions::plugin_run("usage-meter", "main", serde_json::json!({"op":"refresh"}))).await.unwrap();
        assert_eq!(actions::PanelView::parse(&ran.output).unwrap().badge, "已刷新");
        let export = run(actions::plugin_run("usage-meter", "main", serde_json::json!({"op":"export","values":{"days":"30"}}))).await.unwrap();
        assert!(export.output.contains("30"));

        // Pairing: create a code, redeem one, revoke the peer.
        let created = run(actions::request(names::REMOTE_PAIR_CREATE)).await.unwrap();
        assert!(RemoteStatus::read(&created).unwrap().pairing_code.starts_with("SESSION-"));
        assert!(run(actions::pair_redeem("nonsense", "")).await.is_err());
        data.absorb(names::REMOTE_PAIR_REDEEM, &run(actions::pair_redeem("SESSION-XY", "家里")).await.unwrap());
        assert!(data.peers.iter().any(|p| p.name == "家里"));
        data.absorb(names::REMOTE_PEERS_REVOKE, &run(actions::peer_revoke("peer-studio")).await.unwrap());
        assert!(data.peers.iter().all(|p| p.id != "peer-studio"));

        // Sandbox, update, remote desktop, usage.
        assert!(run(actions::sandbox_set("chaos")).await.is_err());
        data.absorb(names::SANDBOX_SET, &run(actions::sandbox_set("off")).await.unwrap());
        assert_eq!(data.sandbox.as_ref().unwrap().mode, "off");
        assert!(run(actions::request(names::UPDATE_INSTALL)).await.is_err(), "nothing staged");
        data.absorb(names::UPDATE_DOWNLOAD, &run(actions::request(names::UPDATE_DOWNLOAD)).await.unwrap());
        assert!(data.update.staged);
        run(actions::request(names::UPDATE_INSTALL)).await.unwrap();
        let options = actions::RdSessionOptions { peer: "peer-1".into(), resolution: "auto".into(), quality: "balanced".into(), control: true, clipboard: true };
        data.absorb(names::RD_SESSION_START, &run(actions::rd_session_start(&options)).await.unwrap());
        assert_eq!(data.rd.as_ref().unwrap().session_state, "connected");
        data.absorb(names::RD_SESSION_STOP, &run(actions::rd_session_stop("peer-1")).await.unwrap());
        assert!(data.rd.as_ref().unwrap().session_peer.is_empty());
        assert!(run(actions::pi_setup_save("openai", "", "openai/gpt-5.5")).await.is_err());
        data.absorb(names::PI_SETUP_SAVE, &run(actions::pi_setup_save("openai", "sk-x", "openai/gpt-5.5")).await.unwrap());
        assert_eq!(data.models.default, "openai/gpt-5.5");
        assert_eq!(run(actions::request("nope")).await.unwrap_err().code, ErrorCode::InvalidCommand);
    }

    #[tokio::test]
    async fn demo_git_switches_branches_discards_and_searches() {
        let bench = MockWorkbench::demo();
        bench.git_checkout("main".into()).await.unwrap();
        assert_eq!(bench.git_status().await.unwrap().branch, "main");
        assert!(bench.git_checkout("ghost".into()).await.is_err());
        bench.git_discard(vec!["src/main.rs".into(), "docs/notes.md".into()]).await.unwrap();
        assert!(bench.git_status().await.unwrap().files.unwrap().is_empty());
        let hits = bench.search_files("add".into()).await.unwrap();
        assert!(hits.iter().any(|h| h.path == "src/lib.rs" && h.line_number == 1));
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
