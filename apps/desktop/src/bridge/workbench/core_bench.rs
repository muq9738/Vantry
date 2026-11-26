//! The workbench backed by the local Core: settings, plugins, remote
//! access, the remote desktop, updates, usage and attachments go through
//! `POST /v1/workbench` (`vantry_client::CoreClient::workbench`); files,
//! Git and terminal sessions use the local checkout or the paired Core that owns it.
//!
//! A Core that does not serve an action falls back, in order: the 0.1.42
//! name of the action ([`actions::baseline_alias`]), then what
//! [`LocalWorkbench`] answers itself. A Core without the workbench at all
//! (`not_ready`, `unknown endpoint`) or not running keeps the window working
//! on local data.

use std::{collections::HashMap, path::PathBuf, sync::{Arc, Mutex}, time::Duration};

mod terminal;

use base64::Engine;
use vantry_protocol::{
    Attachment, GitState, WorkbenchRequest, WorkbenchResponse,
};

use super::{
    DirEntry, FileContent, LocalWorkbench, PrSummary, SearchHit, SettingsData, TerminalSink,
    WorkbenchBridge,
    actions::{self, names},
};
use crate::bridge::{BridgeError, BridgeFuture, ErrorCode};

/// The largest file sent to the Core as an attachment (its body limit is
/// 16 MiB; base64 grows the file by a third).
pub const MAX_ATTACHMENT_BYTES: u64 = 11 << 20;

/// How long the settings wait for one section.
const SECTION_TIMEOUT: Duration = Duration::from_secs(12);

/// The sections the settings load, in order.
const SECTIONS: &[&str] = &[
    names::MODELS,
    names::PI_STATUS,
    names::MCP_LIST,
    names::PLUGINS_LIST,
    names::SANDBOX_GET,
    names::REMOTE_STATUS,
    names::RD_STATUS,
    names::UPDATE_STATUS,
    names::USAGE_SUMMARY,
    names::ENVIRONMENTS_LIST,
    names::PROJECTS_LIST,
];

/// The Core's workbench, with the local workspace beside it.
#[derive(Clone)]
pub struct CoreWorkbench {
    socket: PathBuf,
    local: LocalWorkbench,
    model: Option<String>,
    /// Route checkout and terminal operations through the selected Core.
    core_fs: bool,
    peer: bool,
    workspace_override: Option<String>,
    terminal_thread: String,
    terminal_cwd: String,
    terminal_ids: Arc<Mutex<HashMap<String, String>>>,
}

impl CoreWorkbench {
    /// The Core on `socket`; `local` serves files, Git, the terminal and
    /// what the Core does not.
    pub fn new(socket: PathBuf, local: LocalWorkbench, model: Option<String>) -> Self {
        CoreWorkbench {
            socket,
            local,
            model,
            core_fs: false,
            peer: false,
            workspace_override: None,
            terminal_thread: String::new(),
            terminal_cwd: String::new(),
            terminal_ids: Default::default(),
        }
    }

    /// Route checkout and terminal operations through a paired Core.
    pub fn for_core_workspace(
        socket: PathBuf,
        workspace: String,
        local: LocalWorkbench,
        model: Option<String>,
    ) -> Self {
        CoreWorkbench {
            core_fs: true,
            peer: true,
            workspace_override: Some(workspace),
            ..Self::new(socket, local, model)
        }
    }

    pub(crate) fn on_peer(mut self) -> Self {
        self.peer = true;
        self
    }

    fn ws_request(&self, action: &str) -> WorkbenchRequest {
        let mut req = actions::request(action);
        if let Some(ws) = &self.workspace_override {
            req.workspace = ws.clone();
        }
        req
    }

    /// One action on the Core, without fallbacks.
    async fn ask(socket: PathBuf, request: WorkbenchRequest) -> Result<WorkbenchResponse, BridgeError> {
        vantry_client::CoreClient::new(socket)
            .workbench(&request)
            .await
            .map_err(BridgeError::from)
    }

    /// One action with its fallbacks (alias, then local).
    async fn call(
        socket: PathBuf,
        local: LocalWorkbench,
        request: WorkbenchRequest,
    ) -> Result<WorkbenchResponse, BridgeError> {
        let error = match Self::ask(socket.clone(), request.clone()).await {
            Ok(response) => return Ok(response),
            Err(error) => error,
        };
        if actions::unknown_action(&error)
            && let Some(alias) = actions::baseline_alias(&request)
        {
            match Self::ask(socket, alias).await {
                Ok(response) => return Ok(response),
                Err(alias_error) if !actions::unsupported(&alias_error) => return Err(alias_error),
                Err(_) => {}
            }
        }
        if actions::unsupported(&error) {
            // What the desktop can answer itself; else the Core's reason.
            return local.workbench(request).await.map_err(|_| error);
        }
        Err(error)
    }
}

impl WorkbenchBridge for CoreWorkbench {
    fn label(&self) -> String {
        self.local.label()
    }

    fn root(&self) -> String {
        self.workspace_override
            .clone()
            .unwrap_or_else(|| self.local.root())
    }

    fn list_dir(&self, path: String) -> BridgeFuture<Vec<DirEntry>> {
        if !self.core_fs {
            return self.local.list_dir(path);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("files.list");
        if !path.is_empty() {
            req.paths = vec![path];
        }
        Box::pin(async move {
            let res = Self::ask(socket, req).await?;
            let val: serde_json::Value = serde_json::from_str(&res.output)
                .map_err(|e| BridgeError::unavailable(format!("无效目录列表：{e}")))?;
            let mut out = Vec::new();
            for e in val["entries"].as_array().into_iter().flatten() {
                out.push(DirEntry {
                    name: e["name"].as_str().unwrap_or("").to_owned(),
                    path: e["path"].as_str().unwrap_or("").to_owned(),
                    is_dir: e["isDir"].as_bool().unwrap_or(false),
                    size: e["size"].as_u64().unwrap_or(0),
                });
            }
            Ok(out)
        })
    }

    fn read_file(&self, path: String) -> BridgeFuture<FileContent> {
        if !self.core_fs {
            return self.local.read_file(path);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("files.read");
        req.paths = vec![path.clone()];
        Box::pin(async move {
            let res = Self::ask(socket, req).await?;
            Ok(FileContent {
                path,
                text: res.output,
                truncated: false,
                binary: false,
            })
        })
    }

    fn git_status(&self) -> BridgeFuture<GitState> {
        if !self.core_fs {
            return self.local.git_status();
        }
        let socket = self.socket.clone();
        let req = self.ws_request("git.status");
        Box::pin(async move {
            let res = Self::ask(socket, req).await?;
            res.git
                .ok_or_else(|| BridgeError::unavailable("远端 Core 未返回 Git 状态"))
        })
    }

    fn git_diff(&self, path: Option<String>, staged: bool) -> BridgeFuture<String> {
        if !self.core_fs {
            return self.local.git_diff(path, staged);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("git.diff");
        if let Some(p) = path {
            req.paths = vec![p];
        }
        req.input = Some(serde_json::json!({"staged": staged}));
        Box::pin(async move {
            let res = Self::ask(socket, req).await?;
            Ok(res.output)
        })
    }

    fn git_stage(&self, paths: Vec<String>) -> BridgeFuture<()> {
        if !self.core_fs {
            return self.local.git_stage(paths);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("git.stage");
        req.paths = paths;
        Box::pin(async move {
            Self::ask(socket, req).await?;
            Ok(())
        })
    }

    fn git_unstage(&self, paths: Vec<String>) -> BridgeFuture<()> {
        if !self.core_fs {
            return self.local.git_unstage(paths);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("git.unstage");
        req.paths = paths;
        Box::pin(async move {
            Self::ask(socket, req).await?;
            Ok(())
        })
    }

    fn git_commit(&self, message: String) -> BridgeFuture<String> {
        if !self.core_fs {
            return self.local.git_commit(message);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("git.commit");
        req.message = message;
        Box::pin(async move {
            let res = Self::ask(socket, req).await?;
            Ok(res.output)
        })
    }

    fn pr_summary(&self) -> BridgeFuture<PrSummary> {
        self.local.pr_summary()
    }

    fn terminal_run(&self, session: String, command: String, sink: TerminalSink) -> BridgeFuture<i32> {
        if self.core_fs {
            return self.remote_terminal(session, command, sink);
        }
        self.local.terminal_run(session, command, sink)
    }

    fn terminal_interrupt(&self, session: String) -> BridgeFuture<()> {
        if self.core_fs {
            return self.terminal_control(names::TERMINAL_INTERRUPT, session, serde_json::json!({}));
        }
        self.local.terminal_interrupt(session)
    }

    fn terminal_close(&self, session: String) -> BridgeFuture<()> {
        if self.core_fs {
            return self.terminal_control(names::TERMINAL_CLOSE, session, serde_json::json!({}));
        }
        self.local.terminal_close(session)
    }

    fn terminal_write(&self, session: String, data: String) -> BridgeFuture<()> {
        if self.core_fs {
            return self.terminal_control(names::TERMINAL_WRITE, session, serde_json::json!({"data": data}));
        }
        self.local.terminal_write(session, data)
    }

    fn terminal_resize(&self, session: String, rows: u16, cols: u16) -> BridgeFuture<()> {
        if self.core_fs {
            return self.terminal_control(names::TERMINAL_RESIZE, session, serde_json::json!({"rows": rows, "cols": cols}));
        }
        self.local.terminal_resize(session, rows, cols)
    }

    fn git_checkout(&self, branch: String) -> BridgeFuture<()> {
        if !self.core_fs {
            return self.local.git_checkout(branch);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("git.checkout");
        req.branch = branch;
        Box::pin(async move {
            Self::ask(socket, req).await?;
            Ok(())
        })
    }

    fn git_discard(&self, paths: Vec<String>) -> BridgeFuture<()> {
        if !self.core_fs {
            return self.local.git_discard(paths);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("git.discard");
        req.paths = paths;
        Box::pin(async move {
            Self::ask(socket, req).await?;
            Ok(())
        })
    }

    fn search_files(&self, query: String) -> BridgeFuture<Vec<SearchHit>> {
        if !self.core_fs {
            return self.local.search_files(query);
        }
        let socket = self.socket.clone();
        let mut req = self.ws_request("files.search");
        req.message = query;
        Box::pin(async move {
            let res = Self::ask(socket, req).await?;
            let val: serde_json::Value = serde_json::from_str(&res.output)
                .map_err(|e| BridgeError::unavailable(format!("无效搜索结果：{e}")))?;
            let mut hits = Vec::new();
            for item in val["results"].as_array().into_iter().flatten() {
                hits.push(SearchHit {
                    path: item["path"].as_str().unwrap_or("").to_owned(),
                    line_number: 1,
                    line: item["name"].as_str().unwrap_or("").to_owned(),
                });
            }
            Ok(hits)
        })
    }

    /// Every section at once; each one the Core does not serve keeps what
    /// the desktop knows itself and is named in `degraded`.
    fn settings(&self, workspace: String) -> BridgeFuture<SettingsData> {
        let socket = self.socket.clone();
        let local = self.local.clone();
        let model = self.model.clone();
        Box::pin(async move {
            let mut data = local.settings(workspace.clone()).await?;
            let mut jobs = tokio::task::JoinSet::new();
            for (index, action) in SECTIONS.iter().enumerate() {
                let mut request = actions::request(action);
                if *action == names::MCP_LIST { request.workspace.clone_from(&workspace); }
                if *action == names::MODELS {
                    request.model = model.clone().unwrap_or_default();
                }
                let (socket, local) = (socket.clone(), local.clone());
                jobs.spawn(async move {
                    let result = match tokio::time::timeout(
                        SECTION_TIMEOUT,
                        Self::call(socket, local, request),
                    )
                    .await
                    {
                        Ok(result) => result,
                        Err(_) => Err(BridgeError::unavailable("读取超时")),
                    };
                    (index, result)
                });
            }
            let mut results: Vec<(usize, Result<WorkbenchResponse, BridgeError>)> = Vec::new();
            while let Some(joined) = jobs.join_next().await {
                if let Ok(result) = joined {
                    results.push(result);
                }
            }
            // Folded in a fixed order, so the outcome does not depend on
            // which answer came first.
            results.sort_by_key(|(index, _)| *index);
            for (index, result) in results {
                let action = SECTIONS[index];
                match result {
                    Ok(response) => data.absorb(action, &response),
                    Err(error) => data.degraded.push((action.to_owned(), error.message)),
                }
            }
            Ok(data)
        })
    }

    /// The file goes to the Core's attachment store (`attachment.import`);
    /// a Core without it gets the local reference.
    fn attach(&self, path: PathBuf) -> BridgeFuture<Attachment> {
        let socket = self.socket.clone();
        let peer = self.peer;
        let local = self.local.clone();
        Box::pin(async move {
            let meta = tokio::fs::metadata(&path)
                .await
                .map_err(|e| BridgeError::new(ErrorCode::NotFound, e.to_string()))?;
            if !meta.is_file() {
                return Err(BridgeError::new(ErrorCode::InvalidCommand, "只能附加文件"));
            }
            if meta.len() > MAX_ATTACHMENT_BYTES {
                return Err(BridgeError::new(
                    ErrorCode::InvalidCommand,
                    format!("附件不能超过 {} MB", MAX_ATTACHMENT_BYTES >> 20),
                ));
            }
            let bytes = tokio::fs::read(&path)
                .await
                .map_err(|e| BridgeError::unavailable(e.to_string()))?;
            let mut request = actions::request(names::ATTACHMENT_IMPORT);
            request.name = path
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            request.data = base64::engine::general_purpose::STANDARD.encode(&bytes);
            match Self::ask(socket, request).await {
                Ok(WorkbenchResponse {
                    attachment: Some(attachment),
                    ..
                }) => Ok(attachment),
                Ok(_) => Err(BridgeError::protocol("Core 没有返回附件信息")),
                Err(error) if !peer && actions::unsupported(&error) => local.attach(path).await,
                Err(error) => Err(error),
            }
        })
    }

    fn workbench(&self, mut request: WorkbenchRequest) -> BridgeFuture<WorkbenchResponse> {
        if request.action.starts_with("git.refs.") {
            if !self.core_fs { return self.local.workbench(request); }
            request.workspace = self.root();
            return Box::pin(Self::ask(self.socket.clone(), request));
        }
        let socket = self.socket.clone();
        let local = self.local.clone();
        if self.peer && request.action == names::ATTACHMENT_IMPORT {
            return Box::pin(Self::ask(socket, request));
        }
        Box::pin(Self::call(socket, local, request))
    }
}

#[cfg(all(test, unix))]
mod tests {
    //! A fake Core on a Unix socket: plain HTTP/1.1 by hand, so the real
    //! `vantry_client` is what is exercised.

    use std::sync::{Arc, Mutex};

    use serde_json::{Value, json};
    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        net::{UnixListener, UnixStream},
    };

    use super::*;

    type Route = Arc<dyn Fn(&Value) -> (u16, Value) + Send + Sync>;

    async fn read_body(stream: &mut UnixStream) -> Option<(String, Vec<u8>)> {
        let mut buf = Vec::new();
        let mut chunk = [0u8; 8192];
        let head_end = loop {
            let n = stream.read(&mut chunk).await.ok()?;
            if n == 0 {
                return None;
            }
            buf.extend_from_slice(&chunk[..n]);
            if let Some(at) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                break at + 4;
            }
        };
        let head = String::from_utf8_lossy(&buf[..head_end]).to_string();
        let length = head
            .lines()
            .find_map(|l| {
                let (name, value) = l.split_once(':')?;
                name.eq_ignore_ascii_case("content-length")
                    .then(|| value.trim().parse::<usize>().ok())?
            })
            .unwrap_or(0);
        while buf.len() < head_end + length {
            let n = stream.read(&mut chunk).await.ok()?;
            if n == 0 {
                return None;
            }
            buf.extend_from_slice(&chunk[..n]);
        }
        let line = head.lines().next().unwrap_or_default().to_owned();
        Some((line, buf[head_end..head_end + length].to_vec()))
    }

    /// Serves every connection with `route` until the returned handle is
    /// aborted; records each request's action.
    fn serve(listener: UnixListener, route: Route, seen: Arc<Mutex<Vec<String>>>) -> tokio::task::JoinHandle<()> {
        tokio::spawn(async move {
            loop {
                let Ok((mut stream, _)) = listener.accept().await else {
                    return;
                };
                let route = route.clone();
                let seen = seen.clone();
                tokio::spawn(async move {
                    let Some((line, body)) = read_body(&mut stream).await else {
                        return;
                    };
                    assert_eq!(line, "POST /v1/workbench HTTP/1.1");
                    let request: Value = serde_json::from_slice(&body).unwrap();
                    seen.lock().unwrap().push(request["action"].as_str().unwrap_or_default().to_owned());
                    let (status, value) = route(&request);
                    let body = value.to_string();
                    let head = format!(
                        "HTTP/1.1 {status} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
                        body.len()
                    );
                    let _ = stream.write_all(head.as_bytes()).await;
                    let _ = stream.write_all(body.as_bytes()).await;
                    let _ = stream.shutdown().await;
                });
            }
        })
    }

    fn api_error(code: &str, message: &str) -> (u16, Value) {
        (400, json!({ "code": code, "message": message }))
    }

    struct Fixture {
        dir: tempfile::TempDir,
        bench: CoreWorkbench,
        seen: Arc<Mutex<Vec<String>>>,
        server: tokio::task::JoinHandle<()>,
    }

    impl Fixture {
        fn new(route: Route) -> Fixture {
            let dir = tempfile::tempdir().unwrap();
            let socket = dir.path().join("core.sock");
            let listener = UnixListener::bind(&socket).unwrap();
            let seen = Arc::new(Mutex::new(Vec::new()));
            let server = serve(listener, route, seen.clone());
            let local = LocalWorkbench::new(dir.path().to_path_buf(), Some("openai/gpt".into()));
            let bench = CoreWorkbench::new(socket, local, Some("openai/gpt".into()));
            Fixture { dir, bench, seen, server }
        }

        fn seen(&self) -> Vec<String> {
            self.seen.lock().unwrap().clone()
        }

        async fn finish(self) {
            self.server.abort();
            let _ = self.server.await;
            drop(self.bench);
            self.dir.close().unwrap();
        }
    }

    #[tokio::test]
    async fn terminal_routes_by_workspace_and_keeps_remote_controls_on_the_same_session() {
        let remote = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let finished = remote.clone();
        let fx = Fixture::new(Arc::new(move |r| {
            assert_eq!(r["workspace"], "/peer/repo");
            assert_eq!(r["threadId"], "origin");
            let action = r["action"].as_str().unwrap();
            let input = &r["input"];
            if action != names::TERMINAL_OPEN { assert_eq!(input["sessionId"], "server-id"); }
            let output = match action {
                names::TERMINAL_OPEN => { assert_eq!(input["cwd"], "sub"); json!({"sessionId":"server-id"}) }
                names::TERMINAL_READ => {
                    let done = finished.load(std::sync::atomic::Ordering::Acquire);
                    if input["offset"] == 0 {
                        json!({"startOffset":0,"offset":1,"data":"5A==","exited":false,"exitCode":null})
                    } else {
                        json!({"startOffset":1,"offset":if done {3} else {1},"data":if done {"uK0="} else {""},"exited":done,"exitCode":4})
                    }
                }
                names::TERMINAL_WRITE => { assert_eq!(input["data"], "x"); json!({}) }
                names::TERMINAL_RESIZE => { assert_eq!(input["cols"], 90); json!({}) }
                names::TERMINAL_INTERRUPT => json!({}),
                names::TERMINAL_CLOSE => { finished.store(true,std::sync::atomic::Ordering::Release); json!({}) }
                _ => panic!("unexpected {action}"),
            };
            (200, json!({"output":output.to_string()}))
        }));
        let (sink, _) = tokio::sync::mpsc::unbounded_channel();
        assert_eq!(fx.bench.terminal_run("local".into(), "exit 9".into(), sink).await.unwrap(), 9);
        assert!(fx.seen().is_empty());
        let wb = CoreWorkbench::for_core_workspace(fx.bench.socket.clone(), "/peer/repo".into(), fx.bench.local.clone(), None)
            .terminal_scope("origin".into(), "sub".into());
        let (sink, mut output) = tokio::sync::mpsc::unbounded_channel();
        let id = String::from("tab");
        let job = tokio::spawn(wb.terminal_run(id.clone(), "some-command".into(), sink));
        assert_eq!(output.recv().await.as_deref(), Some(""));
        wb.terminal_write(id.clone(), "x".into()).await.unwrap();
        wb.terminal_resize(id.clone(), 24, 90).await.unwrap();
        wb.terminal_interrupt(id.clone()).await.unwrap();
        wb.terminal_close(id).await.unwrap();
        assert_eq!(job.await.unwrap().unwrap(), 4);
        assert_eq!(output.recv().await.as_deref(), Some("中"));
        fx.finish().await;
    }

    #[tokio::test]
    async fn terminal_on_old_peer_returns_upgrade_error() {
        let fx = Fixture::new(Arc::new(|_| api_error("not_found", "unknown workbench action terminal.session.open")));
        let wb = CoreWorkbench::for_core_workspace(fx.bench.socket.clone(), "/peer/repo".into(), fx.bench.local.clone(), None);
        let (sink, _) = tokio::sync::mpsc::unbounded_channel();
        let err = wb.terminal_run("tab".into(), "exit 0".into(), sink).await.unwrap_err();
        assert!(err.message.contains("升级"));
        assert_eq!(fx.seen(), [names::TERMINAL_OPEN]);
        fx.finish().await;
    }

    #[tokio::test]
    async fn branch_requests_use_the_project_host_even_if_the_core_rejects_them() {
        let fx = Fixture::new(Arc::new(|request: &Value| {
            assert_eq!(request["workspace"], "/paired/project");
            api_error("invalid_command", "unknown action on paired Core")
        }));
        assert!(
            std::process::Command::new("git")
                .current_dir(fx.dir.path())
                .args(["init", "-q"])
                .status()
                .unwrap()
                .success()
        );
        let list = actions::request("git.refs.list");
        assert!(fx.bench.workbench(list.clone()).await.is_ok());
        assert!(fx.seen().is_empty());
        let remote = CoreWorkbench::for_core_workspace(
            fx.bench.socket.clone(),
            "/paired/project".into(),
            fx.bench.local.clone(),
            None,
        );
        let error = remote.workbench(list).await.unwrap_err();
        assert!(error.message.contains("paired Core"));
        assert_eq!(fx.seen(), ["git.refs.list"]);
        fx.finish().await;
    }

    #[tokio::test]
    async fn actions_round_trip_through_the_real_client() {
        let fx = Fixture::new(Arc::new(|request: &Value| match request["action"].as_str().unwrap() {
            "mcp.list" => (200, json!({ "mcp": { "projectTrusted": true, "codemodeDefault": true, "notice": "",
                "servers": [{ "name": "magpie", "scope": "user", "transport": "stdio", "enabled": true, "exposure": "codemode" }] } })),
            "sandbox.set" => {
                assert_eq!(request["input"]["mode"], "workspace-write");
                (200, json!({ "output": "{\"mode\":\"workspace-write\",\"supported\":true}" }))
            }
            _ => api_error("invalid_command", "未知工作台操作"),
        }));
        let response = fx.bench.workbench(actions::request(names::MCP_LIST)).await.unwrap();
        assert_eq!(response.mcp.unwrap().servers.unwrap()[0].name, "magpie");
        let response = fx.bench.workbench(actions::sandbox_set("workspace-write")).await.unwrap();
        let info: actions::SandboxInfo = actions::read_output(&response).unwrap();
        assert!(info.supported);
        assert_eq!(fx.seen(), ["mcp.list", "sandbox.set"]);
        fx.finish().await;
    }

    #[tokio::test]
    async fn an_unknown_action_retries_under_its_baseline_name() {
        let fx = Fixture::new(Arc::new(|request: &Value| match request["action"].as_str().unwrap() {
            "access.code" => (200, json!({ "output": "{\"enabled\":true,\"pairingCode\":\"SESSION-42\"}" })),
            "access.pair" => api_error("invalid_command", "配对码已过期"),
            _ => api_error("invalid_command", "未知工作台操作"),
        }));
        let response = fx.bench.workbench(actions::request(names::REMOTE_PAIR_CREATE)).await.unwrap();
        assert_eq!(actions::RemoteStatus::read(&response).unwrap().pairing_code, "SESSION-42");
        // The alias's own error is the answer (not a local fallback).
        let err = fx.bench.workbench(actions::pair_redeem("SESSION-1", "x")).await.unwrap_err();
        assert_eq!(err.message, "配对码已过期");
        // No alias and no local answer: the Core's reason.
        let err = fx.bench.workbench(actions::request(names::PROJECTS_LIST)).await.unwrap_err();
        assert!(err.message.contains("未知工作台操作"));
        assert_eq!(fx.seen(), ["remote.pair.create", "access.code", "remote.pair.redeem", "access.pair", "projects.list"]);
        fx.finish().await;
    }

    #[tokio::test]
    async fn a_core_without_the_workbench_falls_back_to_local_data() {
        let fx = Fixture::new(Arc::new(|_: &Value| {
            (503, json!({ "code": "not_ready", "message": "workbench service is not mounted on this Core" }))
        }));
        let models = fx.bench.workbench(actions::request(names::MODELS)).await.unwrap();
        assert_eq!(models.catalog.unwrap().default, "openai/gpt");
        let err = fx.bench.workbench(actions::request(names::PLUGINS_LIST)).await.unwrap_err();
        assert_eq!(err.code, ErrorCode::NotReady);
        let settings = fx.bench.settings(String::new()).await.unwrap();
        assert_eq!(settings.models.default, "openai/gpt", "the local catalog stays");
        assert_eq!(settings.update.current_version, env!("CARGO_PKG_VERSION"));
        let degraded: Vec<&str> = settings.degraded.iter().map(|(s, _)| s.as_str()).collect();
        assert!(degraded.contains(&"plugins.list") && degraded.contains(&"rd.status"));
        assert!(!degraded.contains(&"models"), "served locally");
        fx.finish().await;
    }

    #[tokio::test]
    async fn settings_fold_every_section_the_core_serves() {
        let fx = Fixture::new(Arc::new(|request: &Value| match request["action"].as_str().unwrap() {
            "models" => {
                assert_eq!(request["model"], "openai/gpt");
                (200, json!({ "catalog": { "models": [{ "id": "a/b", "provider": "a", "name": "B", "reasoning": true, "input": null, "contextWindow": 1000, "maxTokens": 10 }],
                    "default": "a/b", "thinkingLevels": ["low", "high"] } }))
            }
            "plugins.list" => (200, json!({ "plugins": [{ "id": "usage", "name": "用量" }] })),
            "remote.status" => (200, json!({ "output": "{\"enabled\":true,\"peers\":[{\"id\":\"p1\",\"name\":\"工作室\"}]}" })),
            "mcp.list" => {
                assert_eq!(request["workspace"], "/work/current");
                (200, json!({"mcp": {}}))
            }
            "usage.summary" => (200, json!({ "output": "{\"threads\":2,\"totalTokens\":42}" })),
            "rd.status" => (500, json!({ "code": "internal_error", "message": "helper crashed" })),
            _ => api_error("invalid_command", "未知工作台操作"),
        }));
        let settings = fx.bench.settings("/work/current".into()).await.unwrap();
        assert_eq!(settings.models.thinking_levels, ["low", "high"]);
        assert_eq!(settings.plugins[0].id, "usage");
        assert_eq!(settings.peers[0].name, "工作室");
        assert_eq!(settings.usage.as_ref().unwrap().total_tokens, 42);
        assert!(settings.degraded.iter().any(|(s, why)| s == "rd.status" && why == "helper crashed"));
        fx.finish().await;
    }

    #[tokio::test]
    async fn attachments_are_imported_into_the_core() {
        let fx = Fixture::new(Arc::new(|request: &Value| {
            assert_eq!(request["action"], "attachment.import");
            assert_eq!(request["name"], "note.txt");
            let data = base64::engine::general_purpose::STANDARD
                .decode(request["data"].as_str().unwrap())
                .unwrap();
            assert_eq!(data, b"hello");
            (200, json!({ "attachment": { "id": "att-core-1", "name": "note.txt", "mime": "text/plain", "size": 5 } }))
        }));
        let file = fx.dir.path().join("note.txt");
        std::fs::write(&file, "hello").unwrap();
        let attachment = fx.bench.attach(file).await.unwrap();
        assert_eq!(attachment.id, "att-core-1");
        assert!(fx.bench.attach(fx.dir.path().to_path_buf()).await.is_err(), "a folder");
        fx.finish().await;
    }

    #[tokio::test]
    async fn a_missing_core_still_attaches_and_lists_locally() {
        let dir = tempfile::tempdir().unwrap();
        let local = LocalWorkbench::new(dir.path().to_path_buf(), None);
        let bench = CoreWorkbench::new(dir.path().join("absent.sock"), local, None);
        std::fs::write(dir.path().join("a.md"), "# a").unwrap();
        let attachment = bench.attach(dir.path().join("a.md")).await.unwrap();
        assert_eq!(attachment.mime, "text/markdown");
        assert_eq!(bench.list_dir(String::new()).await.unwrap()[0].name, "a.md");
        let peer = bench.on_peer();
        assert!(peer.attach(dir.path().join("a.md")).await.is_err(), "peer cannot use this Mac's fallback attachment");
        drop(peer);
        dir.close().unwrap();
    }
}
