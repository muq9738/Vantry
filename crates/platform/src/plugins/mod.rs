//! Declarative panels, trusted local process plugins, and packaged native helpers.
mod archive;
mod panel;
pub use panel::validate_panel;
mod rpc;
use crate::{hostfs, hostplatform, hostprocess, workspace};
use anyhow::{Context, Result, bail, ensure};
pub use rpc::RpcError;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::{BTreeMap, HashMap, HashSet},
    fs,
    path::{Path, PathBuf},
    process::Command,
    time::Duration,
};
use vantry_protocol::{PluginNavItem, PluginSettingSpec};
const MAX_JSON: usize = 1 << 20;
fn id_ok(id: &str) -> Result<()> {
    hostfs::component(id)?;
    ensure!(
        id.len() <= 128 && !id.starts_with('.') && !id.chars().any(char::is_whitespace),
        "invalid plugin id"
    );
    Ok(())
}
#[derive(Default, Debug, Clone)]
pub struct Permissions {
    pub workspace_read: bool,
    pub network: Vec<String>,
    pub process: Vec<String>,
}
impl Permissions {
    fn parse(value: &Value) -> Result<Self> {
        let mut p = Self::default();
        let mut append = |s: &str| -> Result<()> {
            if s == "workspace:read" {
                p.workspace_read = true;
            } else if let Some(host) = s.strip_prefix("network:") {
                let host = host.trim();
                ensure!(
                    !host.is_empty()
                        && !host
                            .chars()
                            .any(|c| c.is_whitespace() || c.is_control() || c == '/')
                        && (host == "*"
                            || !host.contains('*')
                            || host
                                .strip_prefix("*.")
                                .is_some_and(|rest| !rest.is_empty() && !rest.contains('*'))),
                    "invalid network permission"
                );
                p.network.push(host.to_ascii_lowercase());
            } else if let Some(cmd) = s.strip_prefix("process:") {
                hostfs::component(cmd)?;
                p.process.push(cmd.into());
            } else {
                bail!("unknown plugin permission {s}");
            }
            Ok(())
        };
        match value {
            Value::Null => (),
            Value::Array(v) => {
                for item in v {
                    append(item.as_str().context("invalid permission")?)?;
                }
            }
            Value::Object(map) => {
                for (k, v) in map {
                    match k.as_str() {
                        "workspaceRead" | "workspace:read" => {
                            if v.as_bool().context("invalid workspace permission")? {
                                append("workspace:read")?;
                            }
                        }
                        "network" if v.is_boolean() => {
                            if v == true {
                                append("network:*")?;
                            }
                        }
                        "process" | "network" if v.is_null() => (),
                        "process" | "network" => {
                            for item in v.as_array().context("expected permission list")? {
                                append(&format!(
                                    "{k}:{}",
                                    item.as_str().context("expected string")?
                                ))?;
                            }
                        }
                        _ => bail!("unknown permission {k}"),
                    }
                }
            }
            _ => bail!("invalid permissions"),
        };
        Ok(p)
    }
    pub fn allows_host(&self, host: &str) -> bool {
        let host = host.to_ascii_lowercase();
        self.network.iter().any(|p| {
            p == "*"
                || p == &host
                || p.strip_prefix("*.")
                    .is_some_and(|domain| host.ends_with(&format!(".{domain}")))
        })
    }
}
fn null_as_default<'de, D, T>(deserializer: D) -> std::result::Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(deserializer)?.unwrap_or_default())
}

#[derive(Default, Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Manifest {
    #[serde(deserialize_with = "null_as_default")]
    pub id: String,
    #[serde(deserialize_with = "null_as_default")]
    pub name: String,
    #[serde(deserialize_with = "null_as_default")]
    pub version: String,
    #[serde(deserialize_with = "null_as_default")]
    pub min_app_version: String,
    #[serde(deserialize_with = "null_as_default")]
    pub runtime: String,
    #[serde(deserialize_with = "null_as_default")]
    pub entry: String,
    #[serde(deserialize_with = "null_as_default")]
    pub main: String,
    #[serde(deserialize_with = "null_as_default")]
    pub command: Vec<String>,
    #[serde(deserialize_with = "null_as_default")]
    pub description: String,
    #[serde(deserialize_with = "null_as_default")]
    pub repo: String,
    pub builtin: bool,
    pub timeout_ms: u64,
    pub r#async: bool,
    pub permissions: Value,
    #[serde(deserialize_with = "null_as_default")]
    pub capabilities: Vec<String>,
    #[serde(deserialize_with = "null_as_default")]
    pub settings: Vec<PluginSettingSpec>,
    #[serde(deserialize_with = "null_as_default")]
    pub navigation: Vec<PluginNavItem>,
    #[serde(deserialize_with = "null_as_default")]
    pub pages: BTreeMap<String, Value>,
}
fn version(s: &str) -> Option<[u32; 3]> {
    let base = s.trim_start_matches(['v', 'V']).split(['-', '+']).next()?;
    let mut v = [0; 3];
    for (i, part) in base.split('.').enumerate() {
        *v.get_mut(i)? = part.parse().ok()?;
    }
    Some(v)
}
pub fn remote_desktop_descriptor() -> Manifest {
    serde_json::from_str(include_str!("remote-desktop.json")).expect("static desktop manifest")
}
impl Manifest {
    fn load(dir: &Path, host_version: &str, id: Option<&str>) -> Result<Self> {
        let raw = hostfs::read_file(dir, Path::new("plugin.json"), 64 << 10)?;
        let mut m: Self = serde_json::from_slice(&raw)?;
        if m.id.is_empty() && !m.main.is_empty() {
            m.id = id.unwrap_or_default().into();
        }
        id_ok(&m.id)?;
        ensure!(
            id.is_none_or(|id| id == m.id) && !m.name.trim().is_empty(),
            "invalid manifest identity"
        );
        if m.id == "vantry-remote-desktop" {
            let mut native = remote_desktop_descriptor();
            if !m.version.is_empty() {
                native.version = m.version;
            }
            if !m.repo.is_empty() {
                native.repo = m.repo;
            }
            if let Some(old) = m
                .settings
                .iter()
                .find(|s| s.key == "defaultQuality" && !s.default.is_empty())
                && let Some(quality) = native.settings.iter_mut().find(|s| s.key == "quality")
            {
                quality.default = old.default.clone();
            }
            native.min_app_version = m.min_app_version;
            m = native;
        }
        ensure!(
            m.main.is_empty() || m.entry.is_empty() || m.main == m.entry,
            "conflicting plugin entries"
        );
        if let (Some(host), Some(min)) = (version(host_version), version(&m.min_app_version)) {
            ensure!(host >= min, "plugin requires {}", m.min_app_version);
        }
        m.grants()?;
        if m.runtime.is_empty() && m.main.is_empty() && m.entry.is_empty() {
            m.runtime = "native".into();
        }
        ensure!(
            m.timeout_ms
                <= if m.runtime == "process" || m.r#async || m.extended_timeout() {
                    30000
                } else {
                    2000
                },
            "plugin timeout exceeds invocation limit"
        );
        let mut keys = HashSet::new();
        for s in &m.settings {
            ensure!(
                !s.key.trim().is_empty()
                    && keys.insert(&s.key)
                    && !s.label.trim().is_empty()
                    && matches!(
                        s.r#type.as_str(),
                        "" | "string" | "select" | "boolean" | "number" | "password"
                    ),
                "invalid plugin setting"
            );
        }
        for nav in &m.navigation {
            ensure!(
                !nav.page.trim().is_empty()
                    && !nav.title.trim().is_empty()
                    && matches!(
                        nav.native_panel.as_str(),
                        "" | "remote-desktop" | "remote-access"
                    ),
                "invalid navigation item"
            );
        }
        match m.runtime.as_str() {
            "process" => {
                m.command(dir)?;
            }
            "native" => {
                ensure!(m.command.is_empty(), "native plugin cannot run commands");
                for nav in &m.navigation {
                    ensure!(
                        !nav.native_panel.is_empty() || m.pages.contains_key(&nav.page),
                        "missing page"
                    );
                }
                for panel in m.pages.values() {
                    validate_panel(panel)?;
                }
            }
            _ => bail!("script plugin runtime is inactive"),
        };
        Ok(m)
    }
    pub fn grants(&self) -> Result<Permissions> {
        Permissions::parse(&self.permissions)
    }
    fn extended_timeout(&self) -> bool {
        self.permissions.is_object()
            || !self.main.is_empty()
            || !self.version.is_empty()
            || !self.min_app_version.is_empty()
            || !self.settings.is_empty()
            || !self.navigation.is_empty()
            || self.permissions.as_array().is_some_and(|list| {
                list.iter()
                    .filter_map(Value::as_str)
                    .any(|s| s.starts_with("network:") || s.starts_with("process:"))
            })
    }
    fn timeout(&self) -> Duration {
        Duration::from_millis(if self.timeout_ms > 0 {
            self.timeout_ms
        } else if self.extended_timeout() {
            10000
        } else {
            2000
        })
    }
    fn command(&self, dir: &Path) -> Result<Command> {
        let first = self
            .command
            .first()
            .context("process plugin needs command")?;
        ensure!(
            self.command.iter().all(|a| !a.contains('\0')) && !first.trim().is_empty(),
            "invalid command"
        );
        if !self.entry.is_empty() {
            hostfs::open_file(dir, Path::new(&self.entry))?;
        }
        let exe = if first.contains(['/', '\\']) {
            let rel = hostfs::relative(Path::new(first))?;
            hostfs::open_file(dir, rel)?;
            let path = dir.join(rel);
            ensure!(hostplatform::executable(&path), "entry is not executable");
            path
        } else {
            let mut script = None;
            let mut after = false;
            for a in &self.command[1..] {
                if !after {
                    if a == "--" {
                        after = true;
                        continue;
                    }
                    ensure!(
                        !matches!(a.as_str(), "-c" | "-e" | "-m" | "--eval")
                            && !a.starts_with("--eval="),
                        "inline commands are prohibited"
                    );
                    if a.starts_with('-') && a != "-" {
                        continue;
                    }
                }
                script = Some(a);
                break;
            }
            let script = script.context("interpreter needs a local script")?;
            ensure!(script != "-", "invalid script");
            hostfs::open_file(dir, Path::new(script))?;
            hostplatform::which(first).context("interpreter unavailable")?
        };
        let mut command = Command::new(exe);
        hostprocess::clean_environment(&mut command);
        command.current_dir(dir).args(&self.command[1..]);
        Ok(command)
    }
}

#[derive(Default, Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Origin {
    pub repo: String,
    pub version: String,
    pub latest_version: String,
}
pub struct Host {
    dir: PathBuf,
    data: PathBuf,
    version: String,
    sessions: HashMap<String, rpc::Rpc>,
}
impl Host {
    pub fn open(directory: &Path, data: &Path, version: &str) -> Result<Self> {
        Ok(Self {
            dir: hostfs::private_root(directory)?,
            data: hostfs::private_root(&data.join(".plugin-data"))?,
            version: version.into(),
            sessions: HashMap::new(),
        })
    }
    pub fn manifest(&self, id: &str) -> Result<Manifest> {
        id_ok(id)?;
        let mut manifest = Manifest::load(&self.dir.join(id), &self.version, Some(id))?;
        if let Some(origin) = self.origin(id)? {
            if !origin.repo.is_empty() {
                manifest.repo = origin.repo;
            }
            if manifest.version.is_empty() {
                manifest.version = origin.version;
            }
        }
        if id != "vantry-remote-desktop"
            && hostfs::read_json::<Value>(&self.dir.join(id).join(".builtin.json"))?.is_some()
        {
            manifest.builtin = true;
        }
        Ok(manifest)
    }
    pub fn origin(&self, id: &str) -> Result<Option<Origin>> {
        id_ok(id)?;
        hostfs::read_json(&self.dir.join(id).join(".origin.json"))
    }
    fn settings(&self, id: &str) -> Result<BTreeMap<String, String>> {
        let state = self.data.join(id).join("settings.json");
        if let Some(settings) = hostfs::read_json(&state)? {
            return Ok(settings);
        }
        Ok(
            hostfs::read_json(&self.dir.join(".plugin-data").join(id).join("settings.json"))?
                .unwrap_or_default(),
        )
    }
    pub fn list(&self) -> Result<Vec<(String, Result<Manifest>)>> {
        let mut all = Vec::new();
        for entry in fs::read_dir(&self.dir)? {
            let e = entry?;
            let id = e.file_name().to_string_lossy().into_owned();
            if !id.starts_with('.') && (e.file_type()?.is_dir() || e.file_type()?.is_symlink()) {
                let m = self.manifest(&id);
                all.push((id, m));
            }
        }
        all.sort_by(|a, b| a.0.cmp(&b.0));
        Ok(all)
    }
    pub fn install_archive(&mut self, path: &Path) -> Result<Manifest> {
        let name = path.file_name().context("missing archive name")?;
        let dir = path
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        self.install(&hostfs::read_file(dir, Path::new(name), 32 << 20)?)
    }
    pub fn install(&mut self, bytes: &[u8]) -> Result<Manifest> {
        let _guard = hostfs::lock_dir(&self.dir, true)?;
        let stage = tempfile::Builder::new()
            .prefix(".install-")
            .tempdir_in(&self.dir)?;
        let contents = stage.path().join("unpacked");
        hostfs::ensure_private_dir(&contents)?;
        let (m, src) = archive::unpack(bytes, &contents, &self.version)?;
        let dest = self.dir.join(&m.id);
        let backup = self.dir.join(format!(".previous-{}", m.id));
        if backup.try_exists()? {
            if dest.try_exists()? {
                fs::remove_dir_all(&backup)?;
            } else {
                fs::rename(&backup, &dest)?;
            }
        }
        self.sessions.remove(&m.id);
        let replace = dest.try_exists()?;
        if replace {
            hostfs::directory_handle(&dest)?;
            fs::rename(&dest, &backup)?;
        }
        if let Err(e) = fs::rename(src, &dest) {
            if replace {
                fs::rename(&backup, &dest)?;
            }
            return Err(e.into());
        }
        if replace {
            fs::remove_dir_all(backup)?;
        }
        Ok(m)
    }
    pub fn uninstall(&mut self, id: &str) -> Result<()> {
        id_ok(id)?;
        let _guard = hostfs::lock_dir(&self.dir, true)?;
        let target = self.dir.join(id);
        hostfs::directory_handle(&target)?;
        self.sessions.remove(id);
        fs::remove_dir_all(target)?;
        let state = self.data.join(id);
        if state.try_exists()? {
            fs::remove_dir_all(state)?;
        }
        Ok(())
    }
    pub fn set_enabled(&mut self, id: &str, on: bool) -> Result<()> {
        self.set_setting(id, "enabled", &on.to_string())
    }
    pub fn set_setting(&mut self, id: &str, key: &str, value: &str) -> Result<()> {
        id_ok(id)?;
        ensure!(!key.trim().is_empty(), "plugin setting key required");
        let _guard = hostfs::lock_dir(&self.dir, true)?;
        let manifest = self.manifest(id)?;
        ensure!(
            key == "enabled" || manifest.settings.iter().any(|s| s.key == key),
            "unknown plugin setting {key}"
        );
        let path = self.data.join(id).join("settings.json");
        let mut settings = self.settings(id)?;
        settings.insert(key.to_owned(), value.to_owned());
        hostfs::write_json(&path, &settings)?;
        if key == "enabled" && value == "false" {
            self.sessions.remove(id);
        }
        Ok(())
    }
    pub fn manifest_info(&self, id: &str) -> Result<Manifest> {
        self.manifest(id)
    }
    fn active(&self, id: &str) -> Result<Manifest> {
        let m = self.manifest(id)?;
        let settings = self.settings(id)?;
        ensure!(
            settings.get("enabled").map(String::as_str).or_else(|| m
                .settings
                .iter()
                .find(|s| s.key == "enabled")
                .map(|s| s.default.as_str()))
                != Some("false"),
            "plugin disabled"
        );
        Ok(m)
    }
    pub fn set_process_grant(&self, id: &str, granted: bool) -> Result<()> {
        self.manifest(id)?;
        hostfs::write_json(
            &self.data.join(id).join("granted.json"),
            &json!({"granted":granted}),
        )
    }
    pub fn authorize_process(&self, id: &str, command: &str) -> Result<()> {
        let p = self.active(id)?.grants()?;
        let state: Value =
            hostfs::read_json(&self.data.join(id).join("granted.json"))?.unwrap_or_default();
        ensure!(
            state["granted"] == true && p.process.iter().any(|c| c == command),
            "process permission denied"
        );
        Ok(())
    }
    pub fn authorize_host(&self, id: &str, host: &str) -> Result<()> {
        ensure!(
            self.active(id)?.grants()?.allows_host(host),
            "network permission denied"
        );
        Ok(())
    }
    pub fn workspace_list(
        &self,
        id: &str,
        root: &Path,
        path: &Path,
    ) -> Result<Vec<workspace::FileEntry>> {
        ensure!(
            self.active(id)?.grants()?.workspace_read,
            "workspace permission denied"
        );
        workspace::list(root, path, 4096)
    }
    pub fn workspace_read(&self, id: &str, root: &Path, path: &Path) -> Result<Vec<u8>> {
        ensure!(
            self.active(id)?.grants()?.workspace_read,
            "workspace permission denied"
        );
        workspace::read(root, path, 256 << 10)
    }
    pub fn invoke(
        &self,
        id: &str,
        root: &Path,
        action: &str,
        page: &str,
        mut input: Value,
    ) -> Result<String> {
        let m = self.active(id)?;
        if m.runtime == "native" {
            ensure!(
                matches!(action, "" | "run" | "page"),
                "unsupported native action"
            );
            let page = if page.is_empty() {
                m.navigation
                    .first()
                    .map(|n| n.page.as_str())
                    .unwrap_or_default()
            } else {
                page
            };
            if let Some(view) = m.pages.get(page) {
                return Ok(view.to_string());
            }
            let nav = m
                .navigation
                .iter()
                .find(|n| n.page == page && !n.native_panel.is_empty())
                .context("unknown native page")?;
            return Ok(json!({"nativePanel":nav.native_panel,"title":nav.title}).to_string());
        }
        if let Some(obj) = input.as_object_mut() {
            if !action.is_empty() {
                obj.entry("action").or_insert_with(|| action.into());
            } else if !page.is_empty() {
                obj.entry("page").or_insert_with(|| page.into());
            }
        }
        ensure!(
            serde_json::to_vec(&input)?.len() <= MAX_JSON,
            "input too large"
        );
        let mut payload =
            serde_json::to_vec(&json!({"version":1,"pluginId":id,"workspace":root,"input":input}))?;
        payload.push(b'\n');
        let out = hostprocess::capture(
            &mut m.command(&self.dir.join(id))?,
            &payload,
            m.timeout(),
            MAX_JSON,
        )?;
        ensure!(
            out.status.success(),
            "plugin exited {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
        let output = String::from_utf8(out.stdout)?;
        let output = output.strip_suffix('\n').unwrap_or(&output);
        Ok(output.strip_suffix('\r').unwrap_or(output).into())
    }
    pub fn start_rpc(&mut self, id: &str) -> Result<u32> {
        let m = self.active(id)?;
        ensure!(
            m.runtime == "process" && !self.sessions.contains_key(id),
            "RPC session unavailable"
        );
        let rpc = rpc::Rpc::start(&mut m.command(&self.dir.join(id))?, m.timeout())?;
        let pid = rpc.pid();
        self.sessions.insert(id.into(), rpc);
        Ok(pid)
    }
    pub fn request(
        &mut self,
        id: &str,
        method: &str,
        params: Value,
        event: impl FnMut(&str, &Value),
    ) -> Result<Value> {
        let result = (|| {
            self.active(id)?;
            self.sessions
                .get_mut(id)
                .context("RPC not running")?
                .request(method, params, event)
        })();
        if result
            .as_ref()
            .is_err_and(|e| e.downcast_ref::<RpcError>().is_none())
        {
            self.sessions.remove(id);
        }
        result
    }
    pub fn notify(&mut self, id: &str, method: &str, params: Value) -> Result<()> {
        self.active(id)?;
        self.sessions
            .get_mut(id)
            .context("RPC not running")?
            .notify(method, params)
    }
    pub fn next_event(&mut self, id: &str, timeout: Duration) -> Result<Option<(String, Value)>> {
        let result = (|| {
            self.active(id)?;
            self.sessions
                .get(id)
                .context("RPC not running")?
                .event(timeout)
        })();
        if result.is_err() {
            self.sessions.remove(id);
        }
        result
    }
    pub fn stop(&mut self, id: &str) {
        self.sessions.remove(id);
    }
}
/// Search only the bundle's Resources directory and platform-specific resource subtree.
pub fn native_helper(resources: &Path) -> Result<PathBuf> {
    let os = if cfg!(target_os = "macos") {
        "darwin"
    } else if cfg!(windows) {
        "windows"
    } else {
        bail!("native desktop helper unsupported on this platform")
    };
    let arch = match std::env::consts::ARCH {
        "aarch64" => "arm64",
        "x86_64" => "amd64",
        _ => bail!("unsupported helper architecture"),
    };
    let name = if cfg!(windows) {
        "vantry-remote-helper.exe"
    } else {
        "vantry-remote-helper"
    };
    for sub in [
        format!("{os}-{arch}"),
        format!("{os}-universal"),
        String::new(),
    ] {
        let rel = Path::new(&sub).join("remote-desktop").join(name);
        if hostfs::open_file(resources, &rel).is_ok()
            && hostplatform::executable(&resources.join(&rel))
        {
            return Ok(resources.join(rel));
        }
    }
    bail!("packaged helper not found")
}
pub fn launch_helper(resources: &Path, args: &[String]) -> Result<hostprocess::Process> {
    let mut cmd = Command::new(native_helper(resources)?);
    hostprocess::clean_environment(&mut cmd);
    cmd.args(args)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped());
    Ok(hostprocess::Process::spawn(&mut cmd)?)
}
