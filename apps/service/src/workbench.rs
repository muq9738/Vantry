use crate::{orchestration, settings};
use anyhow::{Context, Result, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde_json::{Value, json};
use std::{
    fs,
    path::{Path, PathBuf},
    process::Command,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::Mutex as AsyncMutex;
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    desktop_client::DesktopClient,
    desktop_media::{DesktopFrame, DesktopHost, DesktopViewer, InputEvent, LanTicket},
    identity::{IdentityStore, Pair},
    peer::{self, Route},
};
use vantry_core::{Core, CoreOptions, server::ServiceExtension};
use vantry_platform::{environments, git, hostfs, hostprocess, plugins, projects, workspace};
use vantry_protocol::*;
use vantry_updater::{UpdateManager, UpdateOptions};
pub struct Workbench {
    pub core: Core,
    pub options: CoreOptions,
    magpie: Option<PathBuf>,
    pub identity: Arc<IdentityStore>,
    pub federator: Arc<crate::federation::Federator>,
    invitations: vantry_connectivity::bootstrap::Publisher,
    pub start_remote: tokio::sync::Notify,
    pub mobile_server: tokio::sync::watch::Sender<Option<vantry_connectivity::t3compat::T3Server>>,
    updater: AsyncMutex<UpdateManager>,
    plugins: Mutex<plugins::Host>,
    desktop: AsyncMutex<Option<(DesktopClient, String, String)>>,
    desktop_host: AsyncMutex<Option<Arc<DesktopHost>>>,
    desktop_viewer: AsyncMutex<Option<Arc<DesktopViewer>>>,
    /// This device waiting for another host's approval, by handle.
    pending_pairings: Mutex<std::collections::HashMap<String, vantry_connectivity::peer::PendingPairing>>,
    pub environment: String,
}
fn user_home_dir() -> Option<PathBuf> {
    std::env::var_os("HOME")
        .or_else(|| std::env::var_os("USERPROFILE"))
        .map(PathBuf::from)
}

pub fn api_error(e: anyhow::Error) -> vantry_core::Error {
    match e.downcast::<vantry_core::Error>() {
        Ok(e) => e,
        Err(e) => vantry_core::Error::Api(ErrorCode::InvalidCommand, e.to_string()),
    }
}
impl Workbench {
    pub fn new(core: Core, options: CoreOptions, magpie: Option<PathBuf>) -> Result<Self> {
        let data = core.directory();
        let identity = Arc::new(IdentityStore::open(data, "Vantry")?);
        let federator = Arc::new(crate::federation::Federator::open(
            data.to_owned(),
            identity.clone(),
        )?);
        let updater = AsyncMutex::new(UpdateManager::new(UpdateOptions {
            data_dir: Some(data.into()),
            ..Default::default()
        })?);
        let plugins = Mutex::new(plugins::Host::open(
            &data.join("plugins"),
            data,
            env!("CARGO_PKG_VERSION"),
        )?);
        let environment = match fs::read_to_string(data.join("environment-id")) {
            Ok(s) => s.trim().to_string(),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                let id = format!("env_{}", uuid::Uuid::new_v4().simple());
                hostfs::atomic_write(&data.join("environment-id"), id.as_bytes())?;
                id
            }
            Err(e) => return Err(e.into()),
        };
        ensure!(!environment.is_empty(), "empty environment ID");
        Ok(Self {
            core,
            options,
            magpie,
            invitations: vantry_connectivity::bootstrap::Publisher::new(identity.clone())?,
            start_remote: tokio::sync::Notify::new(),
            mobile_server: tokio::sync::watch::channel(None).0,
            identity,
            federator,
            updater,
            plugins,
            desktop: AsyncMutex::new(None),
            desktop_host: AsyncMutex::new(None),
            desktop_viewer: AsyncMutex::new(None),
            pending_pairings: Mutex::new(Default::default()),
            environment,
        })
    }
    fn root(&self, r: &WorkbenchRequest) -> Result<Option<PathBuf>> {
        if !r.thread_id.is_empty() {
            let thread = self
                .core
                .snapshot()?
                .threads
                .unwrap_or_default()
                .into_iter()
                .find(|t| t.id == r.thread_id)
                .context("unknown thread")?;
            return Ok(Some(match thread.workspace {
                Some(spec) => workspace::existing(self.core.directory(), &thread.id, spec)?,
                None => workspace::chat_directory(self.core.directory(), &thread.id, true)?,
            }));
        }
        let raw = if r.workspace.is_empty() {
            self.options
                .workspace
                .as_ref()
                .map(|s| s.path.as_str())
                .unwrap_or("")
        } else {
            &r.workspace
        };
        if raw.is_empty() {
            Ok(None)
        } else {
            Ok(Some(Path::new(raw).canonicalize()?))
        }
    }
    pub async fn models(&self, model: &str) -> Result<ModelCatalog> {
        let selection = self
            .magpie
            .as_ref()
            .map(|p| vantry_pi_runtime::MagpieSelection::load(Some(p)))
            .transpose()?;
        Ok(vantry_pi_runtime::resolve_with_plugins(
            &vantry_pi_runtime::agent_dir(),
            Some(if model.is_empty() {
                &self.options.model
            } else {
                model
            }),
            selection.as_ref(),
            &crate::qoder::catalog_models(self.core.directory()),
        ).await?.catalog)
    }
    async fn handle(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let data = self.core.directory();
        let action = r.action.as_str();
        match action {
            "models" => out.catalog = Some(self.models(&r.model).await?),
            "pi.status" | "pi.setup.save" | "pi.settings.set" => return self.pi(r).await,
            "mcp.list" | "mcp.probe" | "mcp.upsert" | "mcp.remove" | "mcp.trust" => {
                out.mcp = Some(settings::mcp(&r, self.root(&r)?).await?)
            }
            "sandbox.get" | "sandbox.set" => out.output = settings::sandbox(data, &r)?,
            "attachment.import" => return settings::attachment(data, &r),
            "attachment.read" => {
                match settings::attachment(data, &r) {
                    Ok(resp) => return Ok(resp),
                    Err(local_err) => {
                        if !r.thread_id.is_empty()
                            && let Ok(snap) = self.core.snapshot()
                            && let Some(env_id) = snap
                                .threads
                                .as_deref()
                                .unwrap_or(&[])
                                .iter()
                                .find(|t| t.id == r.thread_id)
                                .and_then(|t| t.workspace.as_ref())
                                .map(|w| w.environment_id.trim())
                                .filter(|id| !id.is_empty())
                        {
                            let cfg = self.identity.snapshot();
                            if let Some(peer) = cfg.peers.iter().find(|p| p.id == env_id) {
                                let stop = tokio_util::sync::CancellationToken::new();
                                if let Ok(conn) = vantry_connectivity::peer::PeerConnection::connect(
                                    &peer.pair,
                                    Some(&cfg),
                                    vantry_connectivity::peer::Route::Auto,
                                    stop,
                                )
                                .await
                                    && let Ok(val) = conn
                                        .post_json("/v1/workbench", serde_json::to_value(&r)?)
                                        .await
                                    && let Ok(remote_resp) =
                                        serde_json::from_value::<WorkbenchResponse>(val)
                                    && !remote_resp.data.is_empty()
                                {
                                    return Ok(remote_resp);
                                }
                            }
                        }
                        return Err(local_err);
                    }
                }
            }
            "usage.summary" => {
                out.output = crate::usage::summary(&self.core, &self.options.model, &r.thread_id)?;
            }
            "workspace" | "files.list" | "files.read" | "files.search" | "reply.image"
            | "terminal.open" => {
                return tokio::task::block_in_place(|| self.workspace(r));
            }
            "skills.list" => return self.skills(r).await,
            s if s.starts_with("git.") => return tokio::task::block_in_place(|| self.git(r)),
            "projects.list" | "projects.add" | "projects.remove" => return self.projects(r),
            "environments.list" | "environments.upsert" | "environments.remove" => {
                return self.environments(r);
            }
            "orchestration.environments" => {
                let mut entries = vec![];
                for peer in &self.identity.snapshot().peers {
                    entries.push(json!({
                        "environmentId": peer.id,
                        "name": peer.pair.name,
                        "transport": "paired",
                    }));
                }
                if let Ok(catalog) = environments::Catalog::open(data)
                    && let Ok(list) = catalog.load()
                {
                    for env in list.into_iter().filter(|e| e.kind == environments::Kind::Remote) {
                        entries.push(json!({
                            "environmentId": env.id,
                            "name": env.name,
                            "transport": "paired",
                        }));
                    }
                }
                out.output = json!({"environments": entries}).to_string();
            }
            "environment.info" => {
                let host = self.identity.snapshot().host;
                out.output = json!({
                    "id": self.environment, "name": host.name,
                    "os": std::env::consts::OS, "arch": std::env::consts::ARCH,
                    "home": user_home_dir(),
                    "providerId": self.options.provider_id, "defaultModel": self.options.model,
                    "workspace": self.options.workspace
                })
                .to_string();
            }
            "environment.directories" => return self.directories(r),
            s if s.starts_with("plugins.") => {
                if r.plugin_id == crate::qoder::ID || crate::qoder::directory_import(&r).is_some() {
                    let before = crate::qoder::catalog_models(data);
                    let response = crate::qoder::workbench(data, &r).await?;
                    let mut out = if let Some(panel) = response { panel } else {
                        let list = WorkbenchRequest { action: "plugins.list".into(), ..Default::default() };
                        tokio::task::block_in_place(|| self.plugins(list))?
                    };
                    if before != crate::qoder::catalog_models(data) {
                        out.catalog = Some(self.models("").await?);
                    }
                    return Ok(out);
                }
                return tokio::task::block_in_place(|| self.plugins(r));
            }
            "remote.status" | "remote.peers.list" | "access.status" | "access.discover" => return self.remote_status(),
            "rd.permissions" => {
                use vantry_platform::hostplatform::{Capabilities, Permission};
                let caps = Capabilities::probe();
                let access = |p: &Permission| json!({"state": match p {
                    Permission::Granted => "granted", Permission::Denied => "denied",
                    Permission::Unavailable => "unavailable", Permission::Unknown => "unknown",
                }, "canRequest": false, "canOpenSettings": false});
                out.output = json!({"platform": if cfg!(target_os = "macos") { "darwin" } else { caps.os },
                    "screenCapture":access(&caps.screen_capture), "inputControl":access(&caps.accessibility),
                    "canHost":matches!(caps.screen_capture, Permission::Granted) && matches!(caps.accessibility, Permission::Granted)
                }).to_string();
            }
            "remote.pair.create" => return self.pair().await,
            "remote.pair.redeem" => return self.redeem(r).await,
            "remote.pair.status" => return self.pairing_status(r).await,
            "remote.pair.cancel" => {
                let input = r.input.as_ref().unwrap_or(&Value::Null);
                let handle = input["handle"].as_str().unwrap_or_default();
                self.pending_pairings.lock().unwrap().remove(handle);
                out.output = json!({"cancelled": true}).to_string();
            }
            "remote.access.approval" => {
                let input = r.input.as_ref().unwrap_or(&Value::Null);
                let on = input["enabled"].as_bool().context("missing enabled")?;
                self.identity.set_require_approval(on)?;
                return self.remote_status();
            }
            "remote.requests.approve" | "remote.requests.deny" => {
                let input = r.input.as_ref().unwrap_or(&Value::Null);
                let id = input["id"].as_str().unwrap_or(&r.name);
                let done = if action.ends_with("approve") {
                    self.identity.approve_device(id)?
                } else {
                    self.identity.deny_device(id)
                };
                ensure!(done, "该请求已处理或已过期");
                return self.remote_status();
            }
            "remote.peers.revoke" => return self.revoke(r),
            "rd.status"
            | "rd.session.status"
            | "rd.session.start"
            | "rd.session.stop"
            | "rd.viewer.connect"
            | "rd.viewer.frame"
            | "rd.viewer.input"
            | "rd.viewer.disconnect" => {
                return self.desktop(r).await;
            }
            "update.status" | "update.check" | "update.download" | "update.install" => {
                return self.update(r).await;
            }
            _ => {
                return Err(vantry_core::Error::Api(
                    ErrorCode::NotFound,
                    format!("unknown workbench action {action}"),
                )
                .into());
            }
        }
        Ok(out)
    }
    async fn pi(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let data = self.core.directory();
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let action = r.action.as_str();

        let agent = vantry_pi_runtime::agent_dir();
        let path = data.join("pi-runtime.json");
        if action == "pi.setup.save" {
            vantry_pi_runtime::save_setup(&agent, &serde_json::from_value(input.clone())?)?;
        }
        if action == "pi.settings.set" {
            let _guard = hostfs::lock_dir(data, true)?;
            let mut v = settings::document(&path)?;
            let parsed: PiRuntimeSettingsInput = serde_json::from_value(input.clone())?;
            for (key, val) in [
                ("mirrorMode", parsed.mirror_mode),
                ("npmRegistry", parsed.npm_registry),
                ("customPath", parsed.custom_path),
            ] {
                if input.get(key).is_some() {
                    v[key] = val.into();
                }
            }
            if let Some(enabled) = parsed.enable_user_extensions {
                v["enableUserExtensions"] = enabled.into();
            }
            if let Some(enabled) = parsed.enable_user_mcp {
                v["enableUserMCP"] = enabled.into();
            }
            hostfs::atomic_write(&path, &serde_json::to_vec(&v)?)?;
        }
        let config = settings::document(&path)?;
        let catalog = self.models("").await?;
        let count = catalog.models.as_ref().map_or(0, Vec::len);
        out.pi_runtime = Some(PiRuntimeStatus {
            active_source: "embedded".into(),
            active_path: std::env::current_exe()?.display().to_string(),
            active_version: vantry_pi_runtime::embedded_pi_version().into(),
            agent_dir: agent.display().to_string(),
            configured: count > 0,
            model_count: count as i64,
            default_model: catalog.default,
            compat_verified: true,
            active_runs: self.core.status()?.active_runs,
            mirror_mode: config["mirrorMode"].as_str().unwrap_or("auto").into(),
            custom_path: config["customPath"].as_str().unwrap_or_default().into(),
            npm_registry: config["npmRegistry"].as_str().unwrap_or_default().into(),
            enable_user_extensions: Some(config["enableUserExtensions"].as_bool().unwrap_or(true)),
            enable_user_mcp: Some(config["enableUserMCP"].as_bool().unwrap_or(true)),
            ..Default::default()
        });

        Ok(out)
    }
    fn workspace(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let action = r.action.as_str();

        let root = self.root(&r)?.context("workspace required")?;
        out.workspace = root.display().to_string();
        match action {
            "files.list" => {
                let rel = r.paths.first().map(String::as_str).unwrap_or("");
                let mut entries = vec![];
                for entry in workspace::list(&root, Path::new(rel), 500)? {
                    let path = Path::new(rel).join(&entry.name);
                    let size = fs::symlink_metadata(root.join(&path))?.len();
                    entries.push(json!({"name": entry.name.to_string_lossy(), "path": path, "isDir": entry.directory, "size": size}));
                }
                out.output = json!({"entries":entries}).to_string();
            }
            "files.search" => {
                let matches = crate::workbench_git::search_files(&root, &r.message, 40)?;
                out.output = json!({"results": matches}).to_string();
            }
            "files.read" => {
                let bytes = workspace::read(
                    &root,
                    Path::new(r.paths.first().context("file path required")?),
                    2 << 20,
                )?;
                out.output = String::from_utf8(bytes)?
                    .lines()
                    .take(2000)
                    .collect::<Vec<_>>()
                    .join("\n");
            }
            "reply.image" => {
                ensure!(!r.thread_id.is_empty(), "thread ID required");
                let p = Path::new(&r.name);
                let rel = if p.is_absolute() {
                    p.strip_prefix(&root)?
                } else {
                    p
                };
                let bytes = workspace::read(&root, rel, 8 << 20)?;
                crate::image::validate(rel, &bytes)?;
                out.data = STANDARD.encode(bytes);
            }
            "terminal.open" => {
                let mut c = if cfg!(target_os = "macos") {
                    let mut c = Command::new("open");
                    c.args(["-a", "Terminal"]).arg(&root);
                    c
                } else if cfg!(windows) {
                    let mut c = Command::new("cmd");
                    c.args(["/C", "start", "cmd"]).current_dir(&root);
                    c
                } else {
                    let mut c = Command::new("x-terminal-emulator");
                    c.current_dir(&root);
                    c
                };
                let status = c.status()?;
                ensure!(status.success(), "terminal launch failed");
                out.output = json!({
                "opened":true}
                )
                .to_string();
            }
            _ => {}
        }

        Ok(out)
    }
    async fn skills(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let cwd = match self.root(&r)? {
            Some(path) => path,
            None => self.core.directory().to_owned(),
        };
        let skills = vantry_pi_runtime::discover_skills(&vantry_pi_runtime::agent_dir(), &cwd).await?;
        out.workspace = cwd.display().to_string();
        out.output = json!({"skills": skills}).to_string();
        Ok(out)
    }
    fn changes(&self, query: Option<&str>) -> Result<Value> {
        let raw = query
            .and_then(|s| s.strip_prefix("thread="))
            .filter(|s| !s.is_empty() && !s.contains('&'))
            .context("expected a thread query")?;
        let mut bytes = Vec::new();
        let mut chars = raw.bytes();
        while let Some(c) = chars.next() {
            bytes.push(match c {
                b'%' => {
                    let digits = [
                        chars.next().context("incomplete escape")?,
                        chars.next().context("incomplete escape")?,
                    ];
                    u8::from_str_radix(std::str::from_utf8(&digits)?, 16)?
                }
                b'+' => b' ',
                c => c,
            });
        }
        let root = self
            .root(&WorkbenchRequest {
                thread_id: String::from_utf8(bytes)?,
                ..Default::default()
            })?
            .context("workspace required")?
            .canonicalize()?;
        let mut changes = WorkspaceChanges {
            path: root.display().to_string(),
            files: Some(vec![]),
            ..Default::default()
        };
        let repo = match git::Repository::discover(&root) {
            Ok(v) => v,
            Err(e) if e.to_string().contains("not a git repository") => return Ok(json!(changes)),
            Err(e) => return Err(e),
        };
        let prefix = root.strip_prefix(repo.root())?;
        let paths: Vec<_> = prefix
            .to_str()
            .filter(|s| !s.is_empty())
            .into_iter()
            .collect();
        let staged = repo.diff(true, &paths)?;
        let unstaged = repo.diff(false, &paths)?;
        for (label, text) in [("已暂存", staged), ("未暂存", unstaged)] {
            if !text.is_empty() {
                changes.diff.push_str(&format!("{label}\n{text}\n"));
            }
        }
        changes.is_git = true;
        changes.branch = repo.branch().unwrap_or_default();
        changes.files = Some(
            repo.status()?
                .into_iter()
                .filter_map(|f| {
                    let path = Path::new(&f.path)
                        .strip_prefix(prefix)
                        .ok()?
                        .display()
                        .to_string();
                    Some(WorkspaceFile {
                        path,
                        status: format!("{}{}", f.index, f.worktree),
                    })
                })
                .collect(),
        );
        Ok(json!(changes))
    }
    fn git(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let s = r.action.as_str();
        if matches!(
            s,
            "git.worktree.status" | "git.worktree.merge_cleanup" | "git.worktree.cleanup"
        ) {
            let spec = if !r.thread_id.is_empty() {
                self.core
                    .thread_detail(&r.thread_id, 0)
                    .ok()
                    .and_then(|d| d.thread.workspace)
            } else {
                None
            };
            let checkout = self.root(&r).ok().flatten();
            if let Some(ref p) = checkout {
                out.workspace = p.display().to_string();
            }
            out.output = match s {
                "git.worktree.status" => crate::workbench_git::worktree_status(
                    self.core.directory(),
                    &r.thread_id,
                    checkout.as_deref(),
                    spec.as_ref(),
                )?
                .to_string(),
                "git.worktree.merge_cleanup" => {
                    crate::workbench_git::worktree_cleanup(self.core.directory(), &r.thread_id, true)?
                        .to_string()
                }
                "git.worktree.cleanup" => crate::workbench_git::worktree_cleanup(
                    self.core.directory(),
                    &r.thread_id,
                    false,
                )?
                .to_string(),
                _ => unreachable!(),
            };
            return Ok(out);
        }

        let root = self.root(&r)?.context("workspace required")?;
        out.workspace = root.display().to_string();
        if matches!(
            s,
            "git.checkpoints.list"
                | "git.checkpoints.capture"
                | "git.checkpoints.diff"
                | "git.checkpoints.restore"
        ) {
            let repo = match git::Repository::discover(&root) {
                Ok(v) => v,
                Err(_) if s == "git.checkpoints.list" => {
                    out.output = json!({"checkpoints": []}).to_string();
                    return Ok(out);
                }
                Err(e) => return Err(e),
            };
            let repo_root = repo.root();
            match s {
                "git.checkpoints.list" => {
                    let list = crate::workbench_git::list_checkpoints(repo_root)?;
                    out.output = json!({"checkpoints": list}).to_string();
                }
                "git.checkpoints.capture" => {
                    let cp =
                        crate::workbench_git::capture_checkpoint(repo_root, &r.name, &r.message)?;
                    out.output = json!({"checkpoint": cp}).to_string();
                }
                "git.checkpoints.diff" => {
                    out.output = crate::workbench_git::diff_checkpoint(repo_root, &r.name)?;
                }
                "git.checkpoints.restore" => {
                    if !r.thread_id.is_empty() {
                        ensure!(
                            !self
                                .core
                                .thread_detail(&r.thread_id, 50)?
                                .runs
                                .into_iter()
                                .flatten()
                                .any(|run| !run.status.terminal()),
                            "请等待会话任务结束后再恢复工作区快照"
                        );
                    }
                    out.output =
                        crate::workbench_git::restore_checkpoint(repo_root, &r.name)?.to_string();
                }
                _ => unreachable!(),
            }
            return Ok(out);
        }

        let repo = git::Repository::discover(&root)?;
        let root = root.canonicalize()?;
        let prefix = root.strip_prefix(repo.root())?;
        if s == "git.log" {
            let commits = crate::workbench_git::git_log(repo.root(), &r)?;
            out.git = Some(GitState {
                root: repo.root().display().to_string(),
                branch: repo.branch().unwrap_or_default(),
                commits,
                ..Default::default()
            });
            return Ok(out);
        }
        if s == "git.commit_detail" {
            out.output = crate::workbench_git::git_commit_detail(repo.root(), &r.name)?.to_string();
            return Ok(out);
        }
        if s == "git.compare" {
            out.output =
                crate::workbench_git::git_compare(repo.root(), &r.branch, &r.name)?.to_string();
            return Ok(out);
        }
        if s == "git.commit_op" && r.message.trim() == "patch" {
            if let Some(patch) = crate::workbench_git::handle_extra_git_action(&repo, &r, input)? {
                out.output = patch;
                return Ok(out);
            }
        }
        let selected: Vec<_> = r
            .paths
            .iter()
            .map(|p| {
                Ok(prefix
                    .join(hostfs::relative(Path::new(p))?)
                    .to_str()
                    .context("invalid Git path")?
                    .to_owned())
            })
            .collect::<Result<_>>()?;
        let paths: Vec<_> = selected.iter().map(String::as_str).collect();
        let diff_paths = if paths.is_empty() {
            prefix
                .to_str()
                .filter(|s| !s.is_empty())
                .into_iter()
                .collect()
        } else {
            paths.clone()
        };
        if !matches!(s, "git.status" | "git.branches" | "git.diff" | "git.fetch")
            && !r.thread_id.is_empty()
        {
            ensure!(
                !self
                    .core
                    .thread_detail(&r.thread_id, 50)?
                    .runs
                    .into_iter()
                    .flatten()
                    .any(|run| !run.status.terminal()),
                "thread has active runs"
            );
        }
        match s {
            "git.diff" => {
                out.output = repo.diff(input["staged"].as_bool().unwrap_or(false), &diff_paths)?
            }
            "git.stage" => repo.stage(&paths)?,
            "git.unstage" => repo.unstage(&paths)?,
            "git.discard" => repo.discard(&paths)?,
            "git.commit" => out.output = repo.commit(&r.message)?,
            "git.checkout" => {
                let target = if !r.branch.trim().is_empty() {
                    r.branch.trim()
                } else {
                    r.name.trim()
                };
                repo.switch(target, false)?;
            }
            "git.merge" => {
                ensure!(repo.branches()?.contains(&r.branch), "unknown merge branch");
                let result = hostprocess::capture(
                    Command::new("git").arg("-C").arg(repo.root()).args([
                        "merge",
                        "--no-edit",
                        "--",
                        &r.branch,
                    ]),
                    &[],
                    Duration::from_secs(60),
                    1 << 20,
                )?;
                ensure!(
                    result.status.success(),
                    "{}",
                    String::from_utf8_lossy(&result.stderr)
                );
                out.output = String::from_utf8_lossy(&result.stdout).into();
            }
            "git.status" | "git.branches" => {}
            _ => {
                if let Some(text) = crate::workbench_git::handle_extra_git_action(&repo, &r, input)? {
                    out.output = text;
                } else {
                    bail!("unknown Git action");
                }
            }
        };
        let files: Vec<_> = repo
            .status()?
            .into_iter()
            .filter_map(|mut file| {
                file.path = Path::new(&file.path)
                    .strip_prefix(prefix)
                    .ok()?
                    .display()
                    .to_string();
                if !file.original_path.is_empty() {
                    file.original_path = Path::new(&file.original_path)
                        .strip_prefix(prefix)
                        .ok()
                        .map(|p| p.display().to_string())
                        .unwrap_or_default();
                }
                Some(file)
            })
            .collect();
        let mut git_state = GitState {
            root: repo.root().display().to_string(),
            branch: repo.branch().unwrap_or_default(),
            status: if files.is_empty() { "clean" } else { "dirty" }.into(),
            files: Some(files),
            branches: Some(repo.branches()?),
            diff: repo.diff(false, &diff_paths)?,
            ..Default::default()
        };
        crate::workbench_git::enrich_git_state(&repo, &mut git_state);
        out.git = Some(git_state);

        Ok(out)
    }
    fn projects(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let data = self.core.directory();
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let action = r.action.as_str();

        let store = projects::Store::open(data)?;
        if action == "projects.add" {
            let path = input["rootPath"]
                .as_str()
                .or(input["path"].as_str())
                .unwrap_or(&r.workspace);
            store.register(
                Path::new(path),
                input["id"].as_str(),
                projects::Settings {
                    title: input["title"].as_str().map(Into::into),
                    ..Default::default()
                },
                input["initGit"].as_bool().unwrap_or(false),
            )?;
        } else if action == "projects.remove" {
            store.delete(input["id"].as_str().unwrap_or(&r.name))?;
        }
        out.output = json!({
        "projects":store.list()?}
        )
        .to_string();

        Ok(out)
    }
    fn environments(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let data = self.core.directory();
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let action = r.action.as_str();

        let cat = environments::Catalog::open(data)?;
        if action == "environments.upsert" {
            let entry: environments::Entry = serde_json::from_value(input.clone())?;
            ensure!(entry.kind != environments::Kind::Ssh, "SSH environments are unsupported");
            cat.upsert(entry)?;
        } else if action == "environments.remove" {
            cat.remove(input["id"].as_str().unwrap_or(&r.name))?;
        }
        out.output = json!({
        "environments":cat.load()?.into_iter().filter(|e| e.kind != environments::Kind::Ssh).collect::<Vec<_>>()}
        )
        .to_string();

        Ok(out)
    }
    fn directories(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();

        // Hidden folders only for a client that asks (its folder palette
        // shows them once the filter starts with `.`).
        let hidden = r
            .input
            .as_ref()
            .and_then(|v| v.get("hidden"))
            .and_then(Value::as_bool)
            .unwrap_or(false);
        let dir = if r.workspace.is_empty() {
            user_home_dir().context("HOME required")?
        } else {
            PathBuf::from(r.workspace)
        };
        ensure!(dir.is_absolute(), "absolute path required");
        let mut entries = vec![];
        for e in fs::read_dir(&dir)? {
            let e = e?;
            let name = e.file_name().to_string_lossy().into_owned();
            if (hidden || !name.starts_with('.')) && e.path().is_dir() {
                entries.push(json!({
                "name":name,"path":e.path()}
                ));
            }
        }
        entries.sort_by(|a, b| a["name"].as_str().cmp(&b["name"].as_str()));
        out.output = json!({
        "path":dir,"parent":dir.parent().unwrap_or(&dir),"entries":entries}
        )
        .to_string();

        Ok(out)
    }
    fn plugins(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let data = self.core.directory();
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let s = r.action.as_str();

        let root = self.root(&r)?.unwrap_or_else(|| data.to_owned());
        let mut host = self.plugins.lock().unwrap();
        match s {
            "plugins.install" => {
                if !r.data.is_empty() {
                    host.install(&STANDARD.decode(r.data)?)?;
                } else {
                    let source = if r.name.is_empty() {
                        &r.message
                    } else {
                        &r.name
                    };
                    crate::plugin_source::install(&mut host, source, data)?;
                }
            }
            "plugins.update" => {
                let manifest = host.manifest_info(&r.plugin_id)?;
                ensure!(
                    !manifest.repo.trim().is_empty(),
                    "plugin has no configured update repository"
                );
                crate::plugin_source::install(&mut host, &manifest.repo, data)?;
            }
            "plugins.uninstall" | "plugins.remove" => host.uninstall(&r.plugin_id)?,
            "plugins.enable" | "plugins.disable" => {
                host.set_enabled(&r.plugin_id, s == "plugins.enable")?
            }
            "plugins.setting" => {
                let key = input["key"]
                    .as_str()
                    .filter(|k| !k.is_empty())
                    .unwrap_or(&r.name);
                let val = input["value"].as_str().unwrap_or(&r.message);
                host.set_setting(&r.plugin_id, key, val)?;
            }
            "plugins.grant" => {
                host.set_process_grant(&r.plugin_id, input["granted"].as_bool().unwrap_or(r.amend))?
            }
            "plugins.run" | "plugins.panel" | "plugins.page" => {
                out.output = host.invoke(
                    &r.plugin_id,
                    &root,
                    if s == "plugins.run" { "run" } else { "page" },
                    &r.name,
                    input.clone(),
                )?;
                return Ok(out);
            }
            "plugins.list" => {}
            _ => bail!("unknown plugin action"),
        };
        for (id, m) in host.list()? {
            let mut p = PluginInfo {
                id: id.clone(),
                name: id,
                ..Default::default()
            };
            match m {
                Err(e) => p.error = e.to_string(),
                Ok(m) => {
                    p.name = m.name;
                    p.version = m.version;
                    p.description = m.description;
                    p.repo = m.repo;
                    p.builtin = m.builtin;
                    p.navigation = m.navigation;
                    p.settings = m.settings;
                    let state = settings::document(
                        &data.join(".plugin-data").join(&p.id).join("settings.json"),
                    )?;
                    p.disabled = state["enabled"].as_str().or_else(|| {
                        p.settings
                            .iter()
                            .find(|s| s.key == "enabled")
                            .map(|s| s.default.as_str())
                    }) == Some("false");
                }
            }
            out.plugins.push(p);
        }
        out.plugins.push(crate::qoder::plugin_info(data));
        Ok(out)
    }
    fn remote_status(&self) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();

        let cfg = self.identity.snapshot();
        let peers: Vec<_> = cfg
            .peers
            .iter()
            .map(|p| {
                json!({
                "id":p.id,"name":p.pair.name,"deviceId":p.pair.device_id,"direct":p.pair.direct}
                )
            })
            .collect();
        let mut clients: Vec<_> = cfg
            .clients
            .iter()
            .map(|c| json!({"id":c.id,"name":c.name,"createdAt":c.created_at}))
            .collect();
        if let Some(mobile) = self.mobile_server.borrow().as_ref() {
            clients.extend(mobile.paired_devices());
        }
        let requests: Vec<Value> = self
            .identity
            .device_requests()
            .iter()
            .map(|r| r.summary())
            .collect();
        out.output =
            json!({"enabled":cfg.enabled,"name":cfg.host.name,"direct":cfg.host.direct,
                    "identity":self.identity.public_identity(),"peers":peers,"clients":clients,
                    "requireApproval":cfg.require_approval(),"requests":requests})
                .to_string();

        Ok(out)
    }
    async fn pair(&self) -> Result<WorkbenchResponse> {
        let mut ready = self.mobile_server.subscribe();
        self.start_remote.notify_one();
        let mobile = tokio::time::timeout(Duration::from_secs(5), ready.wait_for(Option::is_some))
            .await.context("Remote access could not start")??.clone().context("Remote access stopped")?;
        let cfg = self.identity.snapshot();
        let invite = self.invitations.issue(&cfg.registration_key).await?;
        let mut out = self.remote_status()?;
        let mut status: Value = serde_json::from_str(&out.output)?;
        status["code"] = json!(invite.code);
        status["link"] = json!(invite.link());
        status["expiresAt"] = json!(invite.expires_at);
        if let Some(link) = mobile.mobile_pairing_link()? {
            status["t3PairingLink"] = json!(link);
        }
        out.output = status.to_string();
        Ok(out)
    }
    async fn redeem(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        use vantry_connectivity::peer::Redemption;
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let stop = CancellationToken::new();
        let relay = input["relay"].as_str().map(str::to_owned).or_else(|| {
            input["code"].as_str().and_then(|_| self.identity.snapshot().relay_endpoints().first().map(|e| e.url.clone()))
        });
        if input["code"].is_string() && relay.is_none() { anyhow::bail!("此设备未配置中继，请粘贴完整的电脑配对链接"); }
        let state = if let Some(relay) = relay.as_deref() {
            vantry_connectivity::bootstrap::redeem_code_start(
                relay,
                input["code"].as_str().unwrap_or(&r.message),
                &self.identity,
                &r.name,
                stop,
            )
            .await?
        } else {
            let pair = Pair::parse(
                input["link"].as_str().unwrap_or(&r.message),
                std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1"),
            )?;
            match pair.v {
                2 => peer::redeem_start(&pair, &self.identity, &r.name, Route::Auto, stop).await?,
                _ => Redemption::Paired(self.identity.save_peer(pair)?),
            }
        };
        self.redemption_output(state, None)
    }
    /// One more look at a device waiting for another host's approval.
    async fn pairing_status(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let handle = input["handle"].as_str().unwrap_or_default().to_owned();
        let pending = self.pending_pairings.lock().unwrap().get(&handle).cloned();
        let Some(pending) = pending else {
            let mut out = WorkbenchResponse::default();
            out.output = json!({"status":"expired"}).to_string();
            return Ok(out);
        };
        let state = peer::poll_redemption(&pending, &self.identity, Route::Auto, CancellationToken::new()).await?;
        self.redemption_output(state, Some(handle))
    }
    fn redemption_output(
        &self,
        state: vantry_connectivity::peer::Redemption,
        handle: Option<String>,
    ) -> Result<WorkbenchResponse> {
        use vantry_connectivity::peer::Redemption;
        let mut out = WorkbenchResponse::default();
        let mut pending = self.pending_pairings.lock().unwrap();
        out.output = match state {
            Redemption::Paired(id) => {
                handle.map(|h| pending.remove(&h));
                json!({"status":"paired","id":id})
            }
            Redemption::Pending(p) => {
                let handle = handle.unwrap_or_else(vantry_connectivity::pairing::token);
                let v = json!({"status":"pending","pending":true,"handle":handle,
                    "verificationCode":p.verification_code,"expiresAt":p.expires_at,"hostName":p.host_name});
                pending.retain(|_, p| p.expires_at > chrono::Utc::now());
                pending.insert(handle, p);
                v
            }
            Redemption::Denied => {
                handle.map(|h| pending.remove(&h));
                json!({"status":"denied"})
            }
            Redemption::Expired => {
                handle.map(|h| pending.remove(&h));
                json!({"status":"expired"})
            }
        }
        .to_string();
        Ok(out)
    }
    fn revoke(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let input = r.input.as_ref().unwrap_or(&Value::Null);

        let id = input["id"].as_str().unwrap_or(&r.name);
        let removed = self.identity.remove_peer(id)?;
        let revoked = self.identity.revoke_client(id)?;
        let mobile_revoked = self.mobile_server.borrow().as_ref()
            .map(|server| server.revoke_client(id)).transpose()?.unwrap_or(false);
        out.output = json!({
        "revoked":removed||revoked||mobile_revoked}
        )
        .to_string();

        Ok(out)
    }
    async fn desktop(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let action = r.action.as_str();

        if action == "rd.viewer.frame" {
            let viewer = self.desktop_viewer.lock().await;
            if let Some(v) = viewer.as_ref() {
                if let Some(frame) = v.latest_frame() {
                    out.output = json!({
                        "connected": true,
                        "target": v.target(),
                        "seq": frame.seq,
                        "width": frame.width,
                        "height": frame.height,
                        "mime": frame.mime,
                        "dataUri": frame.data_uri(),
                    })
                    .to_string();
                } else {
                    out.output = json!({"connected": true, "target": v.target(), "seq": 0}).to_string();
                }
            } else {
                out.output = json!({"connected": false, "seq": 0}).to_string();
            }
            return Ok(out);
        }

        if action == "rd.viewer.input" {
            let viewer = self.desktop_viewer.lock().await;
            let v = viewer
                .as_ref()
                .context("no active remote desktop viewer session")?;
            let event: InputEvent = serde_json::from_value(input.clone())
                .context("invalid remote desktop input event")?;
            v.send_input(event).await?;
            out.output = json!({"ok": true}).to_string();
            return Ok(out);
        }

        if action == "rd.viewer.disconnect" {
            if let Some(v) = self.desktop_viewer.lock().await.take() {
                v.disconnect();
            }
            out.output = json!({"connected": false}).to_string();
            return Ok(out);
        }

        if action == "rd.viewer.connect" {
            let target = input["link"]
                .as_str()
                .or_else(|| input["target"].as_str())
                .or_else(|| input["peer"].as_str())
                .filter(|s| !s.is_empty())
                .unwrap_or(&r.name);
            let ticket = LanTicket::parse(target)?;
            let viewer = Arc::new(DesktopViewer::connect_lan(&ticket).await?);
            let label = viewer.target().to_owned();
            if let Some(prev) = self.desktop_viewer.lock().await.replace(viewer) {
                prev.disconnect();
            }
            out.output = json!({"connected": true, "target": label, "active": true}).to_string();
            return Ok(out);
        }

        let mut session = self.desktop.lock().await;
        let mut host_guard = self.desktop_host.lock().await;
        if action == "rd.session.start" {
            // If caller passed a vantry-lan:// link as peer/target, connect as viewer!
            let candidate = input["link"]
                .as_str()
                .or_else(|| input["peer"].as_str())
                .or_else(|| input["target"].as_str())
                .filter(|s| !s.is_empty())
                .unwrap_or(&r.name);
            if candidate.trim().starts_with("vantry-lan://") {
                let ticket = LanTicket::parse(candidate)?;
                let viewer = Arc::new(DesktopViewer::connect_lan(&ticket).await?);
                let label = viewer.target().to_owned();
                if let Some(prev) = self.desktop_viewer.lock().await.replace(viewer) {
                    prev.disconnect();
                }
                out.output = json!({
                    "active": true,
                    "connected": true,
                    "target": label,
                    "sharing": host_guard.is_some(),
                })
                .to_string();
                return Ok(out);
            }

            let relay_url = input["relay"]
                .as_str()
                .filter(|s| !s.is_empty())
                .or_else(|| {
                    let s = r.name.trim();
                    (s.starts_with("http://") || s.starts_with("https://")).then_some(s)
                });
            let mut relay_info = Value::Null;
            if let Some(relay) = relay_url {
                ensure!(session.is_none(), "desktop session active");
                let client = DesktopClient::new(relay)?;
                let v = client
                    .create(
                        input["registrationKey"].as_str().unwrap_or(""),
                        input["video"].clone(),
                    )
                    .await?;
                *session = Some((
                    client,
                    v["sessionId"]
                        .as_str()
                        .context("missing session ID")?
                        .into(),
                    v["hostToken"]
                        .as_str()
                        .context("missing host token")?
                        .into(),
                ));
                relay_info = v;
            }
            if host_guard.is_none() {
                let bind = if std::env::var("VANTRY_RD_LOOPBACK").as_deref() == Ok("1") {
                    "127.0.0.1:0"
                } else {
                    "0.0.0.0:0"
                };
                let host_name = self.identity.snapshot().host.name;
                let allow_input = input["allowInput"].as_bool().unwrap_or(true)
                    && std::env::var("VANTRY_RD_LOOPBACK").as_deref() != Ok("1");
                let host = Arc::new(
                    DesktopHost::start(
                        bind,
                        &host_name,
                        Arc::new(move |ev| {
                            tracing::debug!(kind = %ev.kind, x = ev.x, y = ev.y, "rd input");
                            #[cfg(target_os = "macos")]
                            if allow_input {
                                crate::rd_macos::inject_input(&ev);
                            }
                            #[cfg(not(target_os = "macos"))]
                            let _ = allow_input;
                        }),
                    )
                    .await?,
                );
                #[cfg(target_os = "macos")]
                if input["capture"].as_bool().unwrap_or(false) {
                    let cap_host = host.clone();
                    tokio::spawn(async move {
                        let mut seq = 0u64;
                        let tmp = std::env::temp_dir().join(format!(
                            "vantry-rd-{}.jpg",
                            uuid::Uuid::new_v4().simple()
                        ));
                        while Arc::strong_count(&cap_host) > 1 {
                            if cap_host.viewer_count() > 0 {
                                let out_path = tmp.clone();
                                let ok = tokio::task::spawn_blocking(move || {
                                    Command::new("/usr/sbin/screencapture")
                                        .args(["-x", "-t", "jpg", "-C"])
                                        .arg(&out_path)
                                        .status()
                                        .is_ok_and(|s| s.success())
                                })
                                .await
                                .unwrap_or(false);
                                if ok && let Ok(bytes) = fs::read(&tmp) {
                                    let (width, height) = crate::rd_macos::main_display_size();
                                    seq += 1;
                                    cap_host.publish_frame(DesktopFrame {
                                        seq,
                                        width,
                                        height,
                                        mime: "image/jpeg".into(),
                                        data_b64: STANDARD.encode(bytes),
                                    });
                                }
                            }
                            tokio::time::sleep(Duration::from_millis(120)).await;
                        }
                        let _ = fs::remove_file(&tmp);
                    });
                }
                *host_guard = Some(host);
            }
            let lan_link = host_guard
                .as_ref()
                .map(|h| h.ticket().link())
                .unwrap_or_default();
            if relay_info.is_object() {
                relay_info["lanLink"] = json!(lan_link);
                relay_info["active"] = json!(true);
                out.output = relay_info.to_string();
            } else {
                out.output = json!({
                    "active": true,
                    "sharing": true,
                    "lanLink": lan_link,
                    "viewerCount": host_guard.as_ref().map_or(0, |h| h.viewer_count()),
                })
                .to_string();
            }
        } else if action == "rd.session.stop" {
            if let Some((client, id, token)) = session.take() {
                let _ = client.delete(&id, &token).await;
            }
            if let Some(h) = host_guard.take() {
                h.stop();
            }
            if let Some(v) = self.desktop_viewer.lock().await.take() {
                v.disconnect();
            }
            out.output = json!({"active": false, "sharing": false, "connected": false}).to_string();
        } else if let Some((client, id, token)) = session.as_ref() {
            let mut status = client.status(id, token).await?;
            if let Some(h) = host_guard.as_ref() {
                status["lanLink"] = json!(h.ticket().link());
                status["viewerCount"] = json!(h.viewer_count());
                status["sharing"] = json!(true);
            }
            out.output = status.to_string();
        } else if let Some(h) = host_guard.as_ref() {
            out.output = json!({
                "active": true,
                "sharing": true,
                "lanLink": h.ticket().link(),
                "viewerCount": h.viewer_count(),
                "connected": self.desktop_viewer.lock().await.is_some(),
            })
            .to_string();
        } else {
            out.output = json!({
                "active": self.desktop_viewer.lock().await.is_some(),
                "sharing": false,
                "connected": self.desktop_viewer.lock().await.is_some(),
            })
            .to_string();
        }

        Ok(out)
    }
    async fn update(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let action = r.action.as_str();
        let input = r.input.as_ref().unwrap_or(&Value::Null);

        let mut mgr = self.updater.lock().await;
        match action {
            "update.check" => {
                mgr.check().await?;
            }
            "update.download" => {
                mgr.download_and_stage(|_, _| {}).await?;
            }
            "update.install" => {
                if !matches!(
                    *mgr.subscribe().borrow(),
                    vantry_updater::UpdateState::ReadyToInstall
                ) {
                    mgr.download_and_stage(|_, _| {}).await?;
                }
                let version = input["version"]
                    .as_str()
                    .filter(|s| !s.is_empty())
                    .unwrap_or(&r.name);
                let desktop_pid = input["desktopPid"].as_u64().unwrap_or(0) as u32;
                let allow_busy_core = input["allowBusyCore"].as_bool().unwrap_or(desktop_pid > 0);
                mgr.launch_apply(version, desktop_pid, allow_busy_core)?;
                out.output = json!({
                    "applying": true,
                    "version": version,
                    "allowBusyCore": allow_busy_core,
                })
                .to_string();
            }
            _ => {}
        }
        let mut info = mgr.current_info();
        if info.current_version.is_empty() {
            info.current_version = env!("CARGO_PKG_VERSION").into();
        }
        out.update = Some(info);

        Ok(out)
    }
}
#[async_trait::async_trait]
impl ServiceExtension for Workbench {
    fn capabilities(&self) -> Vec<String> {
        [
            "threads.summaries.v1",
            "threads.detail.v1",
            "orchestration.inter-thread.v1",
            "workbench.v1",
            "orchestration.v1",
            "plugins.v1",
            "updater.v1",
            // The v0.1.42 desktop requires these stable wire identifiers before
            // opening a paired environment, even for a read-only thread list.
            "workbench.v3",
            "workbench.skills.v1",
            "plugins.native-pages",
            "remote-access.alma-relay",
            "remote-access.native.v1",
            "remote-access.direct-discovery",
            "environments.sessions",
            "orchestration.environments.v1",
            "provider.pi.scripted-tools.v1",
            "remote-desktop.permissions.v1",
            "remote.v1",
            EMBEDDED_PI_CAPABILITY,
            LEGACY_PI_RUNTIME_CAPABILITY,
        ]
        .map(String::from)
        .to_vec()
    }
    async fn workbench(&self, r: WorkbenchRequest) -> vantry_core::Result<WorkbenchResponse> {
        self.handle(r).await.map_err(api_error)
    }
    async fn extra_route(
        &self,
        method: &str,
        path: &str,
        query: Option<&str>,
        body: bytes::Bytes,
    ) -> Option<vantry_core::Result<Value>> {
        let result = if let Some(route) = path.strip_prefix("/v1/orchestration/") {
            if method != "POST" || query.is_some() {
                Err(anyhow::anyhow!("expected POST without query"))
            } else {
                orchestration::call(self, route, &body).await
            }
        } else if path == "/v1/models" && method == "GET" && query.is_none() {
            self.models("").await.map(|c| {
                json!(
                    c.models
                        .unwrap_or_default()
                        .into_iter()
                        .map(|mut m| {
                            if !m.id.starts_with(&format!("{}/", m.provider)) {
                                m.id = format!("{}/{}", m.provider, m.id)
                            }
                            m
                        })
                        .collect::<Vec<_>>()
                )
            })
        } else if path == "/v1/workspace" && method == "GET" {
            self.changes(query)
        } else {
            return None;
        };
        Some(result.map_err(api_error))
    }
}
