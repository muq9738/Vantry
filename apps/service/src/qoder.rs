//! Account and lifecycle management for the imported Qoder model provider.
//! The service hosts the upstream plugin on Pi's embedded QuickJS engine.
mod account;
pub mod host;
mod plugin;
use anyhow::{Context, Result, anyhow, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha512};
use std::{
    fs,
    io::Read,
    path::{Path, PathBuf},
    process::Stdio,
    sync::{Arc, OnceLock},
    time::Duration,
};
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;
use vantry_platform::hostfs;
use vantry_protocol::{PluginInfo, PluginNavItem, WorkbenchRequest, WorkbenchResponse};

pub const ID: &str = "qoder";
const NAME: &str = "Qoder 国际版";
const DESCRIPTION: &str =
    "使用自己的 Qoder 国际版账号（qoder.com）为 Pi 提供模型，任务仍由 Pi 执行";
const PROTOCOL: u64 = 2;
pub const SOURCE: &str = "https://github.com/magpie-community/plugins/tree/main/packages/qoder";
const ENTRY: &str = "opencode.mjs";
const PACKAGE_FILES: &[&str] = &["index.mjs", ENTRY, "package.json", "LICENSE-CLIProxyAPI"];
const LATEST_URL: &str =
    "https://registry.npmjs.org/@magpie-community%2Fopencode-qoder-auth/latest";
const PACKAGE_NAME: &str = "@magpie-community/opencode-qoder-auth";
const CHECK_EVERY: Duration = Duration::from_secs(24 * 60 * 60);
const TICK: Duration = Duration::from_secs(5 * 60);
/// A new plugin version is switched to once no model request has run this long.
const QUIET_BEFORE_SWITCH: Duration = Duration::from_secs(2 * 60);
const MAX_OUTPUT_TOKENS: u64 = 32_000;

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
struct State {
    installed: bool,
    enabled: bool,
    /// The plugin version the host loads.
    current: String,
    /// The version before the last automatic switch, kept for rollback.
    previous: String,
    /// A verified version waiting for an idle moment.
    pending: String,
    /// Versions that failed verification or start; not tried again automatically.
    rejected: Vec<String>,
    latest: String,
    last_check: String,
    check_error: String,
    notice: String,
    source: String,
    /// The account's models as Pi sees them (`models.json`).
    pi_models: Vec<Value>,
    credit_multipliers: std::collections::BTreeMap<String, f64>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
struct HostInfo {
    protocol: u64,
    pid: u32,
    port: u16,
    control_token: String,
    plugin_version: String,
}

pub struct Manager {
    data: PathBuf,
    dir: PathBuf,
    /// Serializes install, host start, switches and removal.
    op: Mutex<()>,
    /// Serializes update downloads (kept apart so a slow download never blocks a run).
    update: Mutex<()>,
    web: reqwest::Client,
    local: reqwest::Client,
}

static MANAGER: OnceLock<Arc<Manager>> = OnceLock::new();

/// The plugin manager of this Core's data directory.
pub fn manager(data: &Path) -> Arc<Manager> {
    MANAGER
        .get_or_init(|| {
            let ua = format!("Vantry/{}", env!("CARGO_PKG_VERSION"));
            Arc::new(Manager {
                data: data.to_owned(),
                dir: data.join("model-plugins").join(ID),
                op: Mutex::new(()),
                update: Mutex::new(()),
                web: reqwest::Client::builder()
                    .user_agent(ua.clone())
                    .https_only(true)
                    .connect_timeout(Duration::from_secs(15))
                    .build()
                    .unwrap_or_default(),
                local: reqwest::Client::builder()
                    .user_agent(ua)
                    .no_proxy()
                    .build()
                    .unwrap_or_default(),
            })
        })
        .clone()
}

fn valid_version(v: &str) -> bool {
    !v.is_empty()
        && v.len() <= 64
        && v.as_bytes()[0].is_ascii_digit()
        && v.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
}
/// `a.b.c` without a pre-release tag.
fn release(v: &str) -> Option<[u64; 3]> {
    let mut out = [0; 3];
    let mut parts = v.split('.');
    for slot in &mut out {
        *slot = parts.next()?.parse().ok()?;
    }
    parts.next().is_none().then_some(out)
}
fn newer(candidate: &str, current: &str) -> bool {
    match (release(candidate), release(current)) {
        (Some(a), Some(b)) => a > b,
        (Some(_), None) => true,
        _ => false,
    }
}
fn now() -> String {
    chrono::Utc::now().to_rfc3339()
}
fn short_time(ts: &str) -> String {
    chrono::DateTime::parse_from_rfc3339(ts)
        .map(|t| {
            t.with_timezone(&chrono::Local)
                .format("%Y-%m-%d %H:%M")
                .to_string()
        })
        .unwrap_or_else(|_| ts.chars().take(16).collect::<String>().replace('T', " "))
}
fn due(last: &str, every: Duration) -> bool {
    chrono::DateTime::parse_from_rfc3339(last).map_or(true, |t| {
        chrono::Utc::now()
            .signed_duration_since(t)
            .to_std()
            .unwrap_or_default()
            >= every
    })
}

impl Manager {
    fn state(&self) -> State {
        fs::read(self.dir.join("state.json"))
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default()
    }
    fn save(&self, state: &State) -> Result<()> {
        self.private_dirs()?;
        hostfs::atomic_write(
            &self.dir.join("state.json"),
            &serde_json::to_vec_pretty(state)?,
        )?;
        Ok(())
    }
    fn private_dirs(&self) -> Result<()> {
        hostfs::ensure_private_dir(&self.data.join("model-plugins"))?;
        hostfs::ensure_private_dir(&self.dir)?;
        hostfs::ensure_private_dir(&self.dir.join("versions"))?;
        Ok(())
    }
    fn version_dir(&self, version: &str) -> PathBuf {
        self.dir.join("versions").join(version)
    }
    fn has_version(&self, version: &str) -> bool {
        valid_version(version) && self.version_dir(version).join(ENTRY).is_file()
    }
    /// Places `files` as plugin version `version` (all files appear at once).
    async fn place_version(&self, version: &str, files: &[(&str, &[u8])]) -> Result<()> {
        ensure!(valid_version(version), "invalid plugin version");
        self.private_dirs()?;
        let dest = self.version_dir(version);
        if dest.join(ENTRY).is_file() {
            for (name, bytes) in files.iter().filter(|(name, _)| name.ends_with(".mjs")) {
                ensure!(
                    fs::read(dest.join(name))?.as_slice() == *bytes,
                    "同一版本的插件代码与已安装内容不一致"
                );
            }
            return self.selftest(&dest.join(ENTRY)).await;
        }
        let stage = self
            .dir
            .join("versions")
            .join(format!(".stage-{}", uuid::Uuid::new_v4().simple()));
        hostfs::ensure_private_dir(&stage)?;
        let placed = async {
            for (name, bytes) in files {
                hostfs::atomic_write(&stage.join(name), bytes)?;
            }
            self.selftest(&stage.join(ENTRY)).await?;
            if dest.exists() {
                fs::remove_dir_all(&dest)?;
            }
            fs::rename(&stage, &dest)?;
            Ok::<_, anyhow::Error>(())
        }
        .await;
        if placed.is_err() {
            let _ = fs::remove_dir_all(&stage);
        }
        placed
    }
    fn inference_token(&self) -> Result<String> {
        let path = self.dir.join("inference-token");
        if let Ok(saved) = fs::read_to_string(&path) {
            let saved = saved.trim();
            if saved.len() >= 32 {
                return Ok(saved.to_owned());
            }
        }
        self.private_dirs()?;
        let token = format!(
            "vq-{}{}",
            uuid::Uuid::new_v4().simple(),
            uuid::Uuid::new_v4().simple()
        );
        hostfs::atomic_write(&path, token.as_bytes())?;
        Ok(token)
    }

    async fn download(&self, url: &str, limit: usize, timeout: Duration) -> Result<Vec<u8>> {
        let mut res = self
            .web
            .get(url)
            .timeout(timeout)
            .send()
            .await?
            .error_for_status()?;
        ensure!(
            res.content_length().is_none_or(|n| n as usize <= limit),
            "download too large"
        );
        let mut out = Vec::new();
        while let Some(chunk) = res.chunk().await? {
            out.extend_from_slice(&chunk);
            ensure!(out.len() <= limit, "download too large");
        }
        Ok(out)
    }
    fn host_info(&self) -> Option<HostInfo> {
        serde_json::from_slice(&fs::read(self.dir.join("host.json")).ok()?).ok()
    }
    async fn control(
        &self,
        host: &HostInfo,
        method: &str,
        path: &str,
        body: Option<Value>,
    ) -> Result<Value> {
        let url = format!("http://127.0.0.1:{}{path}", host.port);
        let request = match method {
            "GET" => self.local.get(url),
            _ => self
                .local
                .post(url)
                .json(&body.unwrap_or_else(|| json!({}))),
        };
        let res = request
            .bearer_auth(&host.control_token)
            .timeout(Duration::from_secs(90))
            .send()
            .await?;
        let status = res.status();
        let value: Value = res.json().await.unwrap_or(Value::Null);
        if !status.is_success() {
            bail!(
                "{}",
                value["error"]["message"]
                    .as_str()
                    .map(str::to_owned)
                    .unwrap_or_else(|| format!("Qoder 模型宿主返回 {status}"))
            );
        }
        Ok(value)
    }
    async fn health(&self, host: &HostInfo) -> Option<Value> {
        let url = format!("http://127.0.0.1:{}/control/health", host.port);
        let res = self
            .local
            .get(url)
            .bearer_auth(&host.control_token)
            .timeout(Duration::from_secs(3))
            .send()
            .await
            .ok()?;
        let value: Value = res.json().await.ok()?;
        (value["ok"] == true && value["pid"].as_u64() == Some(host.pid as u64)).then_some(value)
    }
    /// Asks a host to exit once its streams end and waits up to `wait` for it.
    async fn stop_host(&self, host: &HostInfo, wait: Duration) {
        let _ = self.control(host, "POST", "/control/shutdown", None).await;
        let deadline = tokio::time::Instant::now() + wait;
        while tokio::time::Instant::now() < deadline && self.health(host).await.is_some() {
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }
    async fn spawn_host(&self, state: &State, port: u16) -> Result<HostInfo> {
        ensure!(
            self.has_version(&state.current),
            "插件版本 {} 不完整",
            state.current
        );
        let _ = fs::remove_file(self.dir.join("host.json"));
        let log_path = self.dir.join("host.log");
        if fs::metadata(&log_path).is_ok_and(|m| m.len() > 1 << 20) {
            let _ = fs::remove_file(&log_path);
        }
        let log = fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&log_path)?;
        let mut cmd = tokio::process::Command::new(std::env::current_exe()?);
        cmd.arg("model-plugin-host")
            .arg("--dir")
            .arg(&self.dir)
            .arg("--plugin")
            .arg(self.version_dir(&state.current).join(ENTRY))
            .arg("--plugin-version")
            .arg(&state.current)
            .arg("--port")
            .arg(port.to_string())
            .env("VANTRY_QODER_INFERENCE_TOKEN", self.inference_token()?)
            .current_dir(&self.dir)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(log);
        crate::detach_tokio_command(&mut cmd);
        let mut child = cmd.spawn().context("启动 Qoder 模型宿主失败")?;
        let pid = child.id().unwrap_or_default();
        let deadline = tokio::time::Instant::now() + Duration::from_secs(20);
        loop {
            if let Some(host) = self.host_info().filter(|h| h.pid == pid)
                && self.health(&host).await.is_some()
            {
                tokio::spawn(async move {
                    let _ = child.wait().await;
                });
                return Ok(host);
            }
            let exited = child.try_wait()?;
            if exited.is_some() || tokio::time::Instant::now() >= deadline {
                if exited.is_none() {
                    let _ = child.kill().await;
                }
                let tail = fs::read_to_string(&log_path).unwrap_or_default();
                let tail: Vec<_> = tail.lines().rev().take(4).collect();
                bail!(
                    "Qoder 模型宿主未能启动（插件 {}）：{}",
                    state.current,
                    tail.into_iter().rev().collect::<Vec<_>>().join(" ")
                );
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }
    /// The running host for the current plugin version, started (or rolled
    /// back to the previous version) as needed. Hold `op`.
    async fn ensure_host_locked(&self) -> Result<HostInfo> {
        let mut state = self.state();
        ensure!(state.installed, "Qoder 插件尚未安装");
        ensure!(state.enabled, "Qoder 插件已停用");
        let old = self.host_info();
        if let Some(host) = &old
            && let Some(health) = self.health(host).await
        {
            if host.protocol == PROTOCOL
                && host.plugin_version == state.current
                && health["draining"] != true
            {
                return Ok(host.clone());
            }
            self.stop_host(host, Duration::from_secs(10)).await;
        }
        let port = old.map_or(0, |h| h.port);
        let host = match self.spawn_host(&state, port).await {
            Ok(host) => host,
            Err(error) if self.has_version(&state.previous) && state.previous != state.current => {
                tracing::warn!(%error, "Qoder plugin failed to start; rolling back");
                let failed = std::mem::take(&mut state.current);
                state.current = std::mem::take(&mut state.previous);
                state.rejected.push(failed.clone());
                state.notice = format!("插件 {failed} 启动失败，已回退到 {}", state.current);
                self.save(&state)?;
                self.spawn_host(&state, port).await?
            }
            Err(error) => return Err(error),
        };
        self.sync_pi(&host, &state)?;
        Ok(host)
    }
    pub async fn ensure_host(&self) -> Result<()> {
        let _op = self.op.lock().await;
        self.ensure_host_locked().await.map(drop)
    }

    // ------------------------------------------------------------------ Pi

    fn pi_provider(&self, host: &HostInfo, state: &State) -> Result<Option<Value>> {
        if state.pi_models.is_empty() || !state.enabled {
            return Ok(None);
        }
        Ok(Some(json!({
            "name": NAME,
            "baseUrl": format!("http://127.0.0.1:{}/v1", host.port),
            "api": "openai-completions",
            "apiKey": self.inference_token()?,
            "compat": {
                "supportsStore": false,
                "supportsStrictMode": false,
                "supportsUsageInStreaming": true,
                "maxTokensField": "max_tokens",
            },
            "models": state.pi_models,
        })))
    }
    fn sync_pi(&self, host: &HostInfo, state: &State) -> Result<()> {
        let provider = self.pi_provider(host, state)?;
        vantry_pi_runtime::set_managed_provider(&vantry_pi_runtime::agent_dir(), ID, provider)?;
        Ok(())
    }
    fn pi_models(account: &Value) -> Vec<Value> {
        account["models"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|m| {
                let id = m["id"].as_str().filter(|s| !s.is_empty())?;
                let context = m["context"].as_u64().unwrap_or(0);
                let mut model = json!({
                    "id": id,
                    "name": m["name"].as_str().filter(|s| !s.is_empty()).unwrap_or(id),
                    "reasoning": m["reasoning"] == true,
                    "input": if m["images"] == true { json!(["text", "image"]) } else { json!(["text"]) },
                    "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
                    "maxTokens": if context > 0 { MAX_OUTPUT_TOKENS.min(context / 2).max(1) } else { MAX_OUTPUT_TOKENS },
                });
                if context > 0 {
                    model["contextWindow"] = json!(context);
                }
                Some(model)
            })
            .collect()
    }
    /// Keeps Pi's `qoder` provider in step with what the host reports.
    fn absorb(&self, host: &HostInfo, account: &Value) -> Result<()> {
        let mut state = self.state();
        let signed_in = account["signedIn"] == true;
        let listed = Self::pi_models(account);
        if signed_in
            && listed.is_empty()
            && (account["modelsAt"].as_str().unwrap_or("").is_empty()
                || account["modelsError"]
                    .as_str()
                    .is_some_and(|e| !e.is_empty()))
        {
            return self.sync_pi(host, &state);
        }
        let multipliers = account["models"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|m| {
                let rate = if m["free"] == true {
                    0.0
                } else {
                    m["rate"].as_f64().filter(|r| *r > 0.0)?
                };
                Some((m["id"].as_str()?.to_owned(), rate))
            })
            .collect();
        if listed != state.pi_models || multipliers != state.credit_multipliers {
            state.pi_models = listed;
            state.credit_multipliers = multipliers;
            self.sync_pi(host, &state)?;
            self.save(&state)?;
        }
        Ok(())
    }
    async fn account_call(&self, method: &str, path: &str, body: Option<Value>) -> Result<Value> {
        let _op = self.op.lock().await;
        let host = self.ensure_host_locked().await?;
        let account = self.control(&host, method, path, body).await?;
        if account.get("signedIn").is_some() {
            self.absorb(&host, &account)?;
        }
        Ok(account)
    }

    // -------------------------------------------------------- lifecycle

    /// Resolve the requested Git revision, read the package metadata and validate the imported hooks.
    async fn import_directory(
        &self,
        source: &str,
        dir: &crate::plugin_source::GitHubDirectory,
    ) -> Result<()> {
        let commit = dir.commit(&self.web).await?;
        let raw = self
            .download(
                &dir.raw(&commit, "package.json"),
                1 << 20,
                Duration::from_secs(30),
            )
            .await?;
        let package: Value = serde_json::from_slice(&raw)?;
        ensure!(
            package["name"] == PACKAGE_NAME
                && package["main"].as_str().map(|s| s.trim_start_matches("./")) == Some(ENTRY),
            "该目录缺少 Qoder 模型插件入口"
        );
        let version = package["version"]
            .as_str()
            .filter(|v| release(v).is_some())
            .context("invalid plugin version")?;
        let mut files = vec![("package.json", raw)];
        for name in [ENTRY, "index.mjs", "LICENSE-CLIProxyAPI"] {
            files.push((
                name,
                self.download(&dir.raw(&commit, name), 8 << 20, Duration::from_secs(45))
                    .await?,
            ));
        }
        let refs: Vec<_> = files.iter().map(|(n, b)| (*n, b.as_slice())).collect();
        let _update = self.update.lock().await;
        let _op = self.op.lock().await;
        self.place_version(version, &refs).await?;
        let mut state = self.state();
        if state.installed && self.has_version(&state.current) {
            if newer(version, &state.current) {
                state.pending = version.into();
            }
        } else {
            state.current = version.into();
        }
        state.source = source.to_owned();
        self.enable_install(state).await
    }
    pub async fn install(&self) -> Result<()> {
        let op = self.op.lock().await;
        let state = self.state();
        if self.has_version(&state.current) {
            return self.enable_install(state).await;
        }
        drop(op);
        let source = crate::plugin_source::GitHubDirectory::parse(SOURCE)
            .context("invalid plugin source")?;
        self.import_directory(SOURCE, &source).await
    }
    async fn enable_install(&self, mut state: State) -> Result<()> {
        state.installed = true;
        state.enabled = true;
        state.notice.clear();
        self.save(&state)?;
        self.ensure_host_locked().await?;
        let me = manager(&self.data);
        tokio::spawn(async move {
            if let Err(error) = me.check_update(false).await {
                tracing::warn!(%error, "Qoder plugin update check failed");
            }
        });
        Ok(())
    }
    pub async fn uninstall(&self) -> Result<()> {
        let _update = self.update.lock().await;
        let _op = self.op.lock().await;
        if let Some(host) = self.host_info() {
            self.stop_host(&host, Duration::from_secs(10)).await;
        }
        vantry_pi_runtime::set_managed_provider(&vantry_pi_runtime::agent_dir(), ID, None)?;
        if self.dir.exists() {
            fs::remove_dir_all(&self.dir).context("删除 Qoder 插件目录失败")?;
        }
        Ok(())
    }
    pub async fn set_enabled(&self, on: bool) -> Result<()> {
        let _op = self.op.lock().await;
        let mut state = self.state();
        ensure!(state.installed, "Qoder 插件尚未安装");
        state.enabled = on;
        self.save(&state)?;
        if on {
            self.ensure_host_locked().await?;
        } else {
            if let Some(host) = self.host_info() {
                self.stop_host(&host, Duration::from_secs(10)).await;
            }
            vantry_pi_runtime::set_managed_provider(&vantry_pi_runtime::agent_dir(), ID, None)?;
        }
        Ok(())
    }

    // ------------------------------------------------------------- updates

    /// Checks npm for a newer plugin, verifies and self-tests it, and queues it
    /// for an idle switch. `force` retries versions rejected before.
    pub async fn check_update(&self, force: bool) -> Result<String> {
        let _update = self.update.lock().await;
        ensure!(self.state().installed, "Qoder 插件尚未安装");
        let result = self.fetch_update(force).await;
        let _op = self.op.lock().await;
        let mut state = self.state();
        state.last_check = now();
        match &result {
            Ok((latest, _)) => {
                state.latest.clone_from(latest);
                state.check_error.clear();
            }
            Err(error) => state.check_error = format!("{error:#}"),
        }
        if let Ok((_, Some(version))) = &result {
            state.rejected.retain(|v| v != version);
            state.pending.clone_from(version);
        }
        self.save(&state)?;
        let (latest, queued) = result?;
        Ok(match queued {
            Some(v) => format!("已下载并验证 {v}，空闲时自动切换"),
            None => format!("已是最新版本（{latest}）"),
        })
    }
    async fn fetch_update(&self, force: bool) -> Result<(String, Option<String>)> {
        let meta: Value = serde_json::from_slice(
            &self
                .download(LATEST_URL, 1 << 20, Duration::from_secs(30))
                .await
                .context("查询插件最新版本失败")?,
        )?;
        let latest = meta["version"].as_str().unwrap_or_default().to_owned();
        ensure!(
            meta["name"] == PACKAGE_NAME && valid_version(&latest) && release(&latest).is_some(),
            "插件版本信息无效"
        );
        let state = self.state();
        let target = if state.pending.is_empty() {
            &state.current
        } else {
            &state.pending
        };
        if !newer(&latest, target) || (!force && state.rejected.contains(&latest)) {
            return Ok((latest, None));
        }
        let tarball = meta["dist"]["tarball"].as_str().unwrap_or_default();
        let integrity = meta["dist"]["integrity"].as_str().unwrap_or_default();
        let url = reqwest::Url::parse(tarball)?;
        ensure!(
            url.scheme() == "https" && url.host_str() == Some("registry.npmjs.org"),
            "插件下载地址不受信任"
        );
        let expected = integrity
            .strip_prefix("sha512-")
            .context("插件缺少 sha512 校验值")?;
        let bytes = self
            .download(tarball, 16 << 20, Duration::from_secs(120))
            .await?;
        ensure!(
            STANDARD.encode(Sha512::digest(&bytes)) == expected,
            "插件包校验失败"
        );
        let files = tokio::task::spawn_blocking(move || -> Result<Vec<(String, Vec<u8>)>> {
            let mut archive = tar::Archive::new(flate2::read::GzDecoder::new(&bytes[..]));
            let mut out = vec![];
            for entry in archive.entries()? {
                let mut entry = entry?;
                let path = entry.path()?.into_owned();
                let Some(name) = path
                    .strip_prefix("package")
                    .ok()
                    .and_then(|p| p.to_str())
                    .filter(|n| PACKAGE_FILES.contains(n))
                    .map(str::to_owned)
                else {
                    continue;
                };
                let mut data = vec![];
                entry.by_ref().take(8 << 20).read_to_end(&mut data)?;
                out.push((name, data));
            }
            Ok(out)
        })
        .await??;
        ensure!(
            ["index.mjs", ENTRY]
                .iter()
                .all(|f| files.iter().any(|(n, _)| n == f)),
            "插件包缺少入口文件"
        );
        let refs: Vec<(&str, &[u8])> = files
            .iter()
            .map(|(n, d)| (n.as_str(), d.as_slice()))
            .collect();
        if let Err(error) = self.place_version(&latest, &refs).await {
            let _op = self.op.lock().await;
            let mut state = self.state();
            if !state.rejected.contains(&latest) {
                state.rejected.push(latest.clone());
            }
            self.save(&state)?;
            return Err(error.context(format!("插件 {latest} 未通过兼容性检查")));
        }
        Ok((latest.clone(), Some(latest)))
    }
    async fn selftest(&self, entry: &Path) -> Result<()> {
        let out = tokio::time::timeout(
            Duration::from_secs(30),
            tokio::process::Command::new(std::env::current_exe()?)
                .arg("model-plugin-host")
                .arg("--dir")
                .arg(&self.dir)
                .arg("--selftest")
                .arg("--plugin")
                .arg(entry)
                .current_dir(&self.dir)
                .stdin(Stdio::null())
                .kill_on_drop(true)
                .output(),
        )
        .await
        .context("插件自检超时")??;
        let report: Value = serde_json::from_slice(&out.stdout).unwrap_or(Value::Null);
        ensure!(
            out.status.success() && report["ok"] == true,
            "{}",
            report["error"].as_str().unwrap_or("插件自检失败")
        );
        Ok(())
    }
    /// Switches to the pending version once the host is idle.
    pub async fn try_activate(&self) -> Result<bool> {
        let _update = self.update.lock().await;
        let _op = self.op.lock().await;
        let mut state = self.state();
        if state.pending.is_empty() || !state.installed {
            return Ok(false);
        }
        if !self.has_version(&state.pending) {
            state.pending.clear();
            self.save(&state)?;
            return Ok(false);
        }
        let running = match self.host_info() {
            Some(host) => self.health(&host).await.map(|h| (host, h)),
            None => None,
        };
        if let Some((_, health)) = &running {
            let busy = health["activeStreams"].as_u64().unwrap_or(0) > 0
                || health["loginPending"] == true
                || health["idleMs"]
                    .as_u64()
                    .is_some_and(|ms| ms < QUIET_BEFORE_SWITCH.as_millis() as u64);
            if busy {
                return Ok(false);
            }
        }
        let next = std::mem::take(&mut state.pending);
        state.previous = std::mem::replace(&mut state.current, next);
        state.notice = format!("已自动升级插件：{} → {}", state.previous, state.current);
        self.save(&state)?;
        if let Some((host, _)) = &running {
            self.stop_host(host, Duration::from_secs(10)).await;
        }
        if state.enabled {
            self.ensure_host_locked().await?;
        }
        self.prune();
        Ok(true)
    }
    /// Keeps only the current and previous versions on disk.
    fn prune(&self) {
        let state = self.state();
        for entry in fs::read_dir(self.dir.join("versions"))
            .into_iter()
            .flatten()
            .flatten()
        {
            let name = entry.file_name().to_string_lossy().into_owned();
            if name != state.current && name != state.previous && name != state.pending {
                let _ = fs::remove_dir_all(entry.path());
            }
        }
    }

    // --------------------------------------------------------------- panel

    async fn run(&self, action: &str, values: &Value) -> (String, String) {
        let pat = values["pat"].as_str().unwrap_or_default().trim().to_owned();
        let result: Result<(String, String)> = async {
            Ok(match action {
                "install" => {
                    self.install().await?;
                    ("已安装，可以登录 Qoder".into(), String::new())
                }
                "login" | "login_pat" => {
                    ensure!(
                        action == "login" || !pat.is_empty(),
                        "请输入 Personal Access Token"
                    );
                    let body = if action == "login_pat" {
                        json!({"pat": pat})
                    } else {
                        json!({})
                    };
                    let login = self
                        .account_call("POST", "/control/login", Some(body))
                        .await?;
                    let url = login["url"].as_str().unwrap_or_default().to_owned();
                    (
                        String::new(),
                        if action == "login" {
                            url
                        } else {
                            String::new()
                        },
                    )
                }
                "cancel_login" => {
                    self.account_call("POST", "/control/login/cancel", None)
                        .await?;
                    ("已取消登录".into(), String::new())
                }
                "refresh" => {
                    self.account_call("POST", "/control/refresh", None).await?;
                    (String::new(), String::new())
                }
                "logout" => {
                    self.account_call("POST", "/control/logout", None).await?;
                    ("已退出登录，本机凭据已删除".into(), String::new())
                }
                "check_update" => {
                    let message = self.check_update(true).await?;
                    let switched = self.try_activate().await?;
                    (
                        if switched {
                            self.state().notice
                        } else {
                            message
                        },
                        String::new(),
                    )
                }
                "enable" => {
                    self.set_enabled(true).await?;
                    ("已启用".into(), String::new())
                }
                _ => (String::new(), String::new()),
            })
        }
        .await;
        match result {
            Ok(out) => out,
            Err(error) => (format!("失败：{error:#}"), String::new()),
        }
    }

    async fn panel(&self, notice: &str, open_url: &str) -> String {
        let state = self.state();
        let note = |widgets: &mut Vec<Value>| {
            if !notice.is_empty() {
                let tone = if notice.starts_with("失败") {
                    "danger"
                } else {
                    "info"
                };
                widgets
                    .push(json!({"kind": "text", "key": "notice", "text": notice, "tone": tone}));
            }
        };
        let mut widgets = vec![];
        note(&mut widgets);
        if !state.installed {
            widgets.push(json!({"kind": "markdown", "key": "intro", "markdown": format!("{DESCRIPTION}。安装后在此登录。")}));
            return json!({"title": NAME, "badge": "未安装", "tone": "muted", "widgets": widgets,
                "actions": [{"id": "install", "label": "安装", "tone": "primary"}]})
            .to_string();
        }
        if !state.enabled {
            widgets.push(json!({"kind": "text", "key": "off", "text": "插件已停用，Pi 的模型列表中不再提供 Qoder 模型。"}));
            return json!({"title": NAME, "badge": "已停用", "tone": "muted", "widgets": widgets,
                "actions": [{"id": "enable", "label": "启用", "tone": "primary"}]})
            .to_string();
        }
        let account = self.account_call("GET", "/control/account", None).await;
        let (badge, tone, mut poll) = match &account {
            Err(_) => ("宿主异常", "danger", 0),
            Ok(a) if a["login"]["status"] == "pending" => ("等待授权", "warning", 3000),
            Ok(a) if a["signedIn"] == true && a["expired"] == true => ("需重新登录", "danger", 0),
            Ok(a) if a["signedIn"] == true => ("已登录", "success", 0),
            Ok(_) => ("未登录", "muted", 0),
        };
        let mut actions = vec![];
        match &account {
            Err(error) => {
                widgets.push(
                    json!({"kind": "text", "key": "host-error", "tone": "danger",
                    "text": format!("Qoder 模型宿主未运行：{error:#}")}),
                );
                actions.push(json!({"id": "refresh", "label": "重试", "tone": "primary"}));
            }
            Ok(a) => {
                let login = &a["login"];
                if login["status"] == "pending" {
                    let url = login["url"].as_str().unwrap_or_default();
                    widgets.push(json!({"kind": "markdown", "key": "login", "title": "浏览器授权",
                        "markdown": format!("1. [打开 Qoder 授权页]({url})（已尝试自动打开）\n2. 在浏览器中登录 qoder.com 并授权本设备\n3. 授权完成后此处自动更新；授权页 15 分钟内有效")}));
                    actions.push(json!({"id": "cancel_login", "label": "取消登录"}));
                } else if a["signedIn"] != true {
                    if matches!(login["status"].as_str(), Some("failed" | "expired")) {
                        widgets.push(json!({"kind": "text", "key": "login-error", "tone": "danger",
                            "text": format!("上次登录未完成：{}", login["error"].as_str().unwrap_or("授权失败"))}));
                    }
                    widgets.push(json!({"kind": "markdown", "key": "intro",
                        "markdown": "点「登录 Qoder」后在浏览器中授权。凭据只保存在运行 Core 的这台机器上（仅当前用户可读），模型请求由本机直接发往 Qoder。"}));
                    actions.push(json!({"id": "login", "label": "登录 Qoder", "tone": "primary"}));
                    widgets.push(json!({"kind": "group", "key": "pat", "title": "无法打开浏览器时",
                        "text": "也可以用 qoder.com/account/integrations 生成的 Personal Access Token 登录",
                        "children": [{"kind": "form", "key": "pat-form", "fields": [
                            {"key": "pat", "label": "Personal Access Token", "type": "secret", "placeholder": "pt-..."}]}],
                        "actions": [{"id": "login_pat", "label": "使用 PAT 登录"}]}));
                } else {
                    // The account's actions close its card.
                    let mut account = vec![json!({"id": "refresh", "label": "刷新额度"})];
                    if a["expired"] == true {
                        account
                            .push(json!({"id": "login", "label": "重新登录", "tone": "primary"}));
                    }
                    account.push(
                        json!({"id": "logout", "label": "退出登录", "tone": "danger",
                        "confirm": "退出后将删除本机保存的 Qoder 凭据"}),
                    );
                    Self::account_widgets(a, account, &mut widgets);
                }
            }
        }
        if !open_url.is_empty() {
            poll = 3000;
        }
        let mut plugin = vec![
            json!({"key": "插件版本", "value": if state.pending.is_empty() { state.current.clone() } else { format!("{}（{} 已就绪，空闲时切换）", state.current, state.pending) }}),
            json!({"key": "自动更新", "value": if state.last_check.is_empty() { "尚未检查".to_owned() } else if state.check_error.is_empty() {
                format!("{} 检查，最新 {}", short_time(&state.last_check), if state.latest.is_empty() { "-" } else { &state.latest })
            } else {
                format!("{} 检查失败：{}", short_time(&state.last_check), state.check_error)
            }}),
        ];
        if !state.notice.is_empty() {
            plugin.push(json!({"key": "最近变更", "value": state.notice}));
        }
        widgets.push(json!({"kind": "kv", "key": "plugin", "title": "插件", "entries": plugin,
            "actions": [{"id": "check_update", "label": "立即检查更新"}]}));
        json!({"title": NAME, "badge": badge, "tone": tone, "widgets": widgets, "actions": actions,
            "openUrl": open_url, "pollMs": poll})
        .to_string()
    }

    fn account_widgets(a: &Value, actions: Vec<Value>, widgets: &mut Vec<Value>) {
        let usage = &a["usage"];
        let timestamp = |value: &Value| value.as_str().filter(|v| !v.is_empty()).map(short_time).unwrap_or_default();
        let windows: Vec<_> = usage["windows"].as_array().into_iter().flatten().map(|w| {
            let name = match w["name"].as_str() {
                Some("Total credits") => "总额度",
                Some("Credits") => "个人额度",
                Some("Shared credits") => "共享额度",
                Some(name) if !name.is_empty() => name,
                _ => "额度",
            };
            json!({"name": name, "amount": w["amount"].as_f64(), "limit": w["limit"].as_f64(),
                "used": w["used"].as_f64(), "unit": w["unit"].as_str().unwrap_or_default(),
                "display": w["display"].as_str().unwrap_or_default(), "reset": timestamp(&w["resetsAt"])})
        }).collect();
        let message = if a["expired"] == true { "登录已过期，请重新登录".to_owned() }
            else if let Some(error) = a["usageError"].as_str().filter(|v| !v.is_empty()) { format!("额度刷新失败：{error}") }
            else if windows.is_empty() { "账号尚未返回额度信息".to_owned() } else { String::new() };
        let plan = match usage["plan"].as_str() { Some("teams") => "Teams", other => other.unwrap_or_default() };
        widgets.push(json!({"kind":"quotaCard", "key":"account-usage", "quota":{
            "account":a["account"]["label"].as_str().unwrap_or_default(), "plan":plan,
            "expires":timestamp(&usage["until"]), "updated":timestamp(&a["usageAt"]), "message":message,
            "windows":windows, "models":a["models"].as_array().map_or(0, Vec::len)
        }, "actions":actions}));
        if let Some(error) = a["modelsError"].as_str().filter(|s| !s.is_empty()) {
            widgets
                .push(json!({"kind":"text", "key":"models-error", "tone":"warning", "text":error}));
        }
    }
}

/// The plugin list entry: installable until installed.
pub fn plugin_info(data: &Path) -> PluginInfo {
    let state = manager(data).state();
    let mut info = PluginInfo {
        id: ID.into(),
        name: NAME.into(),
        description: DESCRIPTION.into(),
        repo: if state.source.is_empty() {
            SOURCE.into()
        } else {
            state.source.clone()
        },
        ..Default::default()
    };
    if !state.installed {
        info.installable = true;
        return info;
    }
    info.version = state.current.clone();
    info.disabled = !state.enabled;
    if newer(&state.latest, &state.current) {
        info.latest_version = state.latest;
    }
    info.navigation = vec![PluginNavItem {
        title: "账号与模型".into(),
        page: "main".into(),
        ..Default::default()
    }];
    info
}

/// Registered adapter for the provider-specific hooks of this upstream package.
pub fn directory_import(r: &WorkbenchRequest) -> Option<crate::plugin_source::GitHubDirectory> {
    if r.action != "plugins.install" || !r.data.is_empty() || !r.plugin_id.is_empty() {
        return None;
    }
    let source = if r.name.is_empty() {
        &r.message
    } else {
        &r.name
    };
    let dir = crate::plugin_source::GitHubDirectory::parse(source)?;
    (dir.repo.eq_ignore_ascii_case("magpie-community/plugins") && dir.directory == "packages/qoder")
        .then_some(dir)
}

pub fn catalog_models(
    data: &Path,
) -> std::collections::BTreeMap<String, vantry_protocol::ModelPluginSource> {
    let state = manager(data).state();
    if !state.installed || !state.enabled {
        return Default::default();
    }
    state
        .pi_models
        .iter()
        .filter_map(|m| {
            let id = m["id"].as_str()?;
            Some((
                format!("{ID}/{id}"),
                vantry_protocol::ModelPluginSource {
                    name: NAME.into(),
                    credit_multiplier: state.credit_multipliers.get(id).copied(),
                },
            ))
        })
        .collect()
}

/// Workbench `plugins.*` calls for this plugin. `None` means the caller
/// answers with the refreshed plugin list.
pub async fn workbench(data: &Path, r: &WorkbenchRequest) -> Result<Option<WorkbenchResponse>> {
    let m = manager(data);
    let panel = |output: String| {
        Some(WorkbenchResponse {
            output,
            ..Default::default()
        })
    };
    Ok(match r.action.as_str() {
        "plugins.install" => {
            if let Some(dir) = directory_import(r) {
                let source = if r.name.is_empty() {
                    &r.message
                } else {
                    &r.name
                };
                m.import_directory(source.trim(), &dir).await?;
            } else {
                m.install().await?;
            }
            None
        }
        "plugins.update" => {
            m.check_update(true).await?;
            m.try_activate().await?;
            None
        }
        "plugins.uninstall" | "plugins.remove" => {
            m.uninstall().await?;
            None
        }
        "plugins.enable" | "plugins.disable" => {
            m.set_enabled(r.action == "plugins.enable").await?;
            None
        }
        "plugins.list" => None,
        "plugins.page" | "plugins.panel" => panel(m.panel("", "").await),
        "plugins.run" => {
            let input = r.input.clone().unwrap_or(Value::Null);
            let action = input["action"].as_str().unwrap_or_default();
            let (notice, url) = m.run(action, &input["values"]).await;
            panel(m.panel(&notice, &url).await)
        }
        _ => return Err(anyhow!("Qoder 插件不支持此操作")),
    })
}

/// Gracefully stops the detached Qoder model plugin host when Core shuts down.
pub async fn shutdown(data: &Path) {
    let m = manager(data);
    if let Some(host) = m.host_info()
        && m.health(&host).await.is_some()
    {
        m.stop_host(&host, Duration::from_secs(3)).await;
    }
}

/// Before a run on a `qoder/…` model: the host must be up for Pi to reach it.
pub async fn prepare_run(data: &Path, model: &str) -> Result<()> {
    if model.split_once('/').is_some_and(|(p, _)| p == ID) {
        manager(data)
            .ensure_host()
            .await
            .context("Qoder 模型宿主不可用")?;
    }
    Ok(())
}

/// Starts the host when installed and keeps the plugin up to date.
pub fn spawn_background(data: PathBuf, stop: CancellationToken) {
    tokio::spawn(async move {
        let m = manager(&data);
        let jitter = Duration::from_secs(u64::from(uuid::Uuid::new_v4().as_bytes()[0]) * 14);
        let mut first = true;
        loop {
            let state = m.state();
            if state.installed && state.enabled {
                if let Err(error) = m.account_call("GET", "/control/account", None).await {
                    tracing::warn!(%error, "Qoder model host unavailable");
                }
                if due(
                    &state.last_check,
                    if first {
                        Duration::ZERO
                    } else {
                        CHECK_EVERY + jitter
                    },
                ) && let Err(error) = m.check_update(false).await
                {
                    tracing::warn!(%error, "Qoder plugin update check failed");
                }
                if !m.state().pending.is_empty()
                    && let Err(error) = m.try_activate().await
                {
                    tracing::warn!(%error, "Qoder plugin switch failed");
                }
            }
            first = false;
            tokio::select! {
                _ = stop.cancelled() => break,
                _ = tokio::time::sleep(TICK) => {}
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn versions_compare_as_releases() {
        assert!(newer("0.2.74", "0.2.73"));
        assert!(newer("0.10.0", "0.9.9"));
        assert!(!newer("0.2.73", "0.2.73"));
        assert!(!newer("0.3.0-beta.1", "0.2.73"));
        assert!(newer("0.2.73", ""));
        assert!(valid_version("0.2.73") && !valid_version("../x") && !valid_version(""));
    }

    #[test]
    fn pi_models_follow_the_account_listing() {
        let account = json!({"signedIn": true, "models": [
            {"id": "lmodel", "name": "Claude", "context": 200000, "reasoning": true, "images": true},
            {"id": "tiny", "context": 16000},
            {"id": ""}
        ]});
        let models = Manager::pi_models(&account);
        assert_eq!(models.len(), 2);
        assert_eq!(models[0]["contextWindow"], 200000);
        assert_eq!(models[0]["maxTokens"], 32000);
        assert_eq!(models[0]["input"], json!(["text", "image"]));
        assert_eq!(models[1]["name"], "tiny");
        assert_eq!(models[1]["maxTokens"], 8000);
    }

    #[test]
    fn github_import_selects_the_package_directory() {
        let input = |source: &str| WorkbenchRequest {
            action: "plugins.install".into(),
            name: source.into(),
            ..Default::default()
        };
        let dir = directory_import(&input(SOURCE)).unwrap();
        assert_eq!(dir.directory, "packages/qoder");
        assert_eq!(dir.revision, "main");
        assert!(directory_import(&input("https://github.com/magpie-community/plugins")).is_none());
        assert!(
            directory_import(&input(&SOURCE.replace("packages/qoder", "packages/other"))).is_none()
        );
    }
}
