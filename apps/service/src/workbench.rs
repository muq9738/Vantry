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
    invitations: vantry_connectivity::bootstrap::Publisher,
    updater: AsyncMutex<UpdateManager>,
    plugins: Mutex<plugins::Host>,
    desktop: AsyncMutex<Option<(DesktopClient, String, String)>>,
    environment: String,
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
            identity,
            updater,
            plugins,
            desktop: AsyncMutex::new(None),
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
        vantry_pi_runtime::model_catalog(
            &vantry_pi_runtime::agent_dir(),
            Some(if model.is_empty() {
                &self.options.model
            } else {
                model
            }),
            selection.as_ref(),
        )
        .await
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
            "attachment.import" | "attachment.read" => return settings::attachment(data, &r),
            "usage.summary" => {
                out.output = crate::usage::summary(&self.core, &self.options.model, &r.thread_id)?;
            }
            "workspace" | "files.list" | "files.read" | "reply.image" | "terminal.open" => {
                return tokio::task::block_in_place(|| self.workspace(r));
            }
            s if s.starts_with("git.") => return tokio::task::block_in_place(|| self.git(r)),
            "projects.list" | "projects.add" | "projects.remove" => return self.projects(r),
            "environments.list" | "environments.upsert" | "environments.remove" => {
                return self.environments(r);
            }
            "environment.info" => {
                let host = self.identity.snapshot().host;
                out.output = json!({
                    "id": self.environment, "name": host.name,
                    "os": std::env::consts::OS, "arch": std::env::consts::ARCH,
                    "home": std::env::var_os("HOME").map(PathBuf::from),
                    "providerId": self.options.provider_id, "defaultModel": self.options.model,
                    "workspace": self.options.workspace
                })
                .to_string();
            }
            "environment.directories" => return self.directories(r),
            "ssh.hosts" => {
                out.ssh_hosts = vantry_platform::sshconfig::discover(
                    &PathBuf::from(std::env::var_os("HOME").context("missing home")?),
                    false,
                )?
            }
            s if s.starts_with("plugins.") => {
                return tokio::task::block_in_place(|| self.plugins(r));
            }
            "remote.status" | "remote.peers.list" => return self.remote_status(),
            "remote.pair.create" => return self.pair().await,
            "remote.pair.redeem" => return self.redeem(r).await,
            "remote.peers.revoke" => return self.revoke(r),
            "rd.status" | "rd.session.status" | "rd.session.start" | "rd.session.stop" => {
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

        let root = self.root(&r)?.context("workspace required")?;
        out.workspace = root.display().to_string();
        let repo = git::Repository::discover(&root)?;
        let root = root.canonicalize()?;
        let prefix = root.strip_prefix(repo.root())?;
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
        if !matches!(s, "git.status" | "git.branches" | "git.diff") && !r.thread_id.is_empty() {
            ensure!(
                !self
                    .core
                    .snapshot()?
                    .runs
                    .into_iter()
                    .flatten()
                    .any(|run| run.thread_id == r.thread_id && !run.status.terminal()),
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
            "git.checkout" => repo.switch(&r.branch, false)?,
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
            _ => bail!("unknown Git action"),
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
        out.git = Some(GitState {
            root: repo.root().display().to_string(),
            branch: repo.branch().unwrap_or_default(),
            status: if files.is_empty() { "clean" } else { "dirty" }.into(),
            files: Some(files),
            branches: Some(repo.branches()?),
            diff: repo.diff(false, &diff_paths)?,
            ..Default::default()
        });

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
            cat.upsert(serde_json::from_value(input.clone())?)?;
        } else if action == "environments.remove" {
            cat.remove(input["id"].as_str().unwrap_or(&r.name))?;
        }
        out.output = json!({
        "environments":cat.load()?}
        )
        .to_string();

        Ok(out)
    }
    fn directories(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();

        let dir = if r.workspace.is_empty() {
            PathBuf::from(std::env::var_os("HOME").context("HOME required")?)
        } else {
            PathBuf::from(r.workspace)
        };
        ensure!(dir.is_absolute(), "absolute path required");
        let mut entries = vec![];
        for e in fs::read_dir(&dir)? {
            let e = e?;
            let name = e.file_name().to_string_lossy().into_owned();
            if !name.starts_with('.') && e.file_type()?.is_dir() {
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
            "plugins.uninstall" | "plugins.remove" => host.uninstall(&r.plugin_id)?,
            "plugins.enable" | "plugins.disable" => {
                host.set_enabled(&r.plugin_id, s == "plugins.enable")?
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
                "id":p.id,"name":p.pair.name,"direct":p.pair.direct}
                )
            })
            .collect();
        let clients: Vec<_> = cfg
            .clients
            .iter()
            .map(|c| json!({"id":c.id,"name":c.name,"createdAt":c.created_at}))
            .collect();
        out.output =
            json!({"identity":self.identity.public_identity(),"peers":peers,"clients":clients})
                .to_string();

        Ok(out)
    }
    async fn pair(&self) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let key = self.identity.snapshot().registration_key;
        let invite = self.invitations.issue(&key).await?;
        out.output = json!({"code":invite.code,"link":invite.link(),"expiresAt":invite.expires_at})
            .to_string();
        Ok(out)
    }
    async fn redeem(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let input = r.input.as_ref().unwrap_or(&Value::Null);

        let stop = CancellationToken::new();
        let id = if let Some(relay) = input["relay"].as_str() {
            vantry_connectivity::bootstrap::redeem_code(
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
                2 => peer::redeem(&pair, &self.identity, &r.name, Route::Auto, stop).await?,
                _ => self.identity.save_peer(pair)?,
            }
        };
        out.output = json!({
        "id":id}
        )
        .to_string();

        Ok(out)
    }
    fn revoke(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let input = r.input.as_ref().unwrap_or(&Value::Null);

        let id = input["id"].as_str().unwrap_or(&r.name);
        let removed = self.identity.remove_peer(id)?;
        let revoked = self.identity.revoke_client(id)?;
        out.output = json!({
        "revoked":removed||revoked}
        )
        .to_string();

        Ok(out)
    }
    async fn desktop(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let input = r.input.as_ref().unwrap_or(&Value::Null);
        let action = r.action.as_str();

        let mut session = self.desktop.lock().await;
        if action == "rd.session.start" {
            ensure!(session.is_none(), "desktop session active");
            let client = DesktopClient::new(input["relay"].as_str().unwrap_or(&r.name))?;
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
            out.output = v.to_string();
        } else if let Some((client, id, token)) = session.as_ref() {
            if action == "rd.session.stop" {
                client.delete(id, token).await?;
                *session = None;
                out.output = json!({
                "active":false}
                )
                .to_string();
            } else {
                out.output = client.status(id, token).await?.to_string();
            }
        } else {
            out.output = json!({
            "active":false}
            )
            .to_string();
        }

        Ok(out)
    }
    async fn update(&self, r: WorkbenchRequest) -> Result<WorkbenchResponse> {
        let mut out = WorkbenchResponse::default();
        let action = r.action.as_str();

        let mut mgr = self.updater.lock().await;
        match action {
            "update.check" => {
                mgr.check().await?;
            }
            "update.download" | "update.install" => {
                mgr.download_and_stage(|_, _| {}).await?;
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
