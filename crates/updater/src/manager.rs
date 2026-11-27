use crate::{
    Asset, Channel, DeltaStep, InstallPlan, MAX_ASSET, Release, bundled_token_file,
    combine_release_notes, compare_versions, normalize_version, parse_releases, plan_delta_chain,
    read_update_credentials, resolve_current_app_bundle, update_token_file,
};
use anyhow::{Context, Result, bail, ensure};
use reqwest::{Client, Response, StatusCode, Url};
use std::{
    io::{BufWriter, Seek, SeekFrom, Write},
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};
use tokio::sync::{Semaphore, mpsc, watch};
use tokio::task::JoinSet;
pub use vantry_protocol::AppUpdateInfo;

/// Default GitHub release download proxies tried with hedged racing for mainland China users
/// (public repositories only), always including direct `github.com` as fallback.
const DEFAULT_CN_GITHUB_PROXIES: &[&str] = &[
    "https://gh-proxy.org/",
    "https://gh-proxy.com/",
    "https://ghfast.top/",
];
const PROXY_CONNECT_RESPONSE_TIMEOUT: Duration = Duration::from_secs(8);
const DIRECT_CONNECT_RESPONSE_TIMEOUT: Duration = Duration::from_secs(20);
const HEDGE_DELAY: Duration = Duration::from_millis(1_500);
const CHUNK_READ_TIMEOUT: Duration = Duration::from_secs(12);
const LOW_SPEED_GRACE: Duration = Duration::from_secs(5);
const MIN_PROXY_SPEED_BPS: u64 = 64 * 1024;
const PROGRESS_PUBLISH_INTERVAL: Duration = Duration::from_millis(120);
const MAX_CONCURRENT_DELTA_DOWNLOADS: usize = 4;
const DOWNLOAD_WRITE_BUFFER: usize = 256 * 1024;

/// Heuristic local check (zero network calls) for whether the host is in mainland China.
fn is_cn_region() -> bool {
    #[cfg(unix)]
    if let Ok(target) = std::fs::read_link("/etc/localtime") {
        let s = target.to_string_lossy();
        if s.contains("Asia/Shanghai")
            || s.contains("Asia/Chongqing")
            || s.contains("Asia/Harbin")
            || s.contains("Asia/Urumqi")
            || s.ends_with("/PRC")
        {
            return true;
        }
    }
    for var in ["LC_ALL", "LC_MESSAGES", "LANG"] {
        if let Ok(val) = std::env::var(var) {
            let lower = val.to_ascii_lowercase();
            if lower.starts_with("zh_cn") || lower.starts_with("zh-cn") || lower.starts_with("zh-hans") {
                return true;
            }
        }
    }
    false
}

fn normalize_proxy_prefix(raw: &str) -> Option<String> {
    let trimmed = raw.trim();
    if trimmed.is_empty()
        || matches!(
            trimmed.to_ascii_lowercase().as_str(),
            "none" | "off" | "direct" | "0" | "false"
        )
    {
        return None;
    }
    let mut prefix = trimmed.to_owned();
    if !prefix.ends_with('/') {
        prefix.push('/');
    }
    Some(prefix)
}

/// Resolve GitHub download proxy prefixes from explicit option -> `VANTRY_GITHUB_PROXY` env -> CN region detection.
pub fn resolve_github_proxies(explicit: Option<&str>) -> Vec<String> {
    if let Some(raw) = explicit {
        return normalize_proxy_prefix(raw).into_iter().collect();
    }
    if let Ok(env_val) = std::env::var("VANTRY_GITHUB_PROXY") {
        return normalize_proxy_prefix(&env_val).into_iter().collect();
    }
    if is_cn_region() {
        DEFAULT_CN_GITHUB_PROXIES
            .iter()
            .map(|s| (*s).to_owned())
            .collect()
    } else {
        Vec::new()
    }
}

/// Build ordered download candidate URLs `(url, is_proxy)` for an asset.
/// Proxies are only applied to public GitHub HTTPS URLs; direct URL is always the final fallback.
pub fn github_download_candidates(url: &Url, auth: bool, proxies: &[String]) -> Vec<(Url, bool)> {
    let is_public_github = !auth
        && url.scheme() == "https"
        && matches!(
            url.host_str(),
            Some("github.com" | "objects.githubusercontent.com" | "release-assets.githubusercontent.com")
        );
    let mut out = Vec::with_capacity(proxies.len() + 1);
    if is_public_github {
        for prefix in proxies {
            if let Ok(proxied) = Url::parse(&format!("{prefix}{url}")) {
                out.push((proxied, true));
            }
        }
    }
    out.push((url.clone(), false));
    out
}

#[derive(Debug, Clone)]
pub enum ReleaseSource {
    GitHub { api_base: String, repo: String },
    Manifest(Url),
}
impl Default for ReleaseSource {
    fn default() -> Self {
        Self::GitHub {
            api_base: "https://api.github.com".into(),
            repo: "muq9738/Vantry".into(),
        }
    }
}
#[derive(Clone)]
pub struct UpdateOptions {
    pub source: ReleaseSource,
    pub channel: Channel,
    pub current_version: String,
    pub platform: String,
    pub slug: String,
    pub target: Option<PathBuf>,
    /// Optional dedicated credential supplied by the embedding application.
    pub token: Option<String>,
    pub local_token: Option<PathBuf>,
    pub bundled_token: Option<PathBuf>,
    pub data_dir: Option<PathBuf>,
    /// Optional GitHub download proxy prefix override (`None` = auto-detect CN region / `VANTRY_GITHUB_PROXY`;
    /// `"none"` / `"direct"` disables proxying).
    pub github_proxy: Option<String>,
}
impl Default for UpdateOptions {
    fn default() -> Self {
        let exe = std::env::current_exe().ok();
        let os = match std::env::consts::OS {
            "macos" => "darwin",
            other => other,
        };
        let arch = match std::env::consts::ARCH {
            "aarch64" => "arm64",
            "x86_64" => "amd64",
            other => other,
        };
        Self {
            source: ReleaseSource::default(),
            channel: Channel::Stable,
            current_version: env!("CARGO_PKG_VERSION").into(),
            platform: format!("{os}-{arch}"),
            slug: "vantry".into(),
            target: exe
                .as_deref()
                .and_then(|path| {
                    resolve_current_app_bundle(path).or_else(|| {
                        // The packaged Windows Core lives directly under Vantry/libexec.
                        let libexec = path.parent()?;
                        let bundle = libexec.parent()?;
                        (cfg!(windows)
                            && libexec.ends_with("libexec")
                            && bundle.join("Vantry.exe").is_file())
                        .then_some(bundle)
                    })
                })
                .map(Path::to_owned),
            token: None,
            local_token: update_token_file(),
            bundled_token: exe.as_deref().and_then(bundled_token_file),
            data_dir: None,
            github_proxy: None,
        }
    }
}
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub enum UpdateState {
    Idle,
    Checking,
    Available,
    Downloading,
    ReadyToInstall,
    Applying,
    Failed(String),
}
struct PendingState {
    sender: watch::Sender<UpdateState>,
    active: UpdateState,
    previous: UpdateState,
}
impl Drop for PendingState {
    fn drop(&mut self) {
        self.sender.send_if_modified(|current| {
            if *current == self.active {
                *current = self.previous.clone();
                true
            } else {
                false
            }
        });
    }
}
/// Mutable operations serialize per manager. A watch receiver observes progress states independently.
pub struct UpdateManager {
    opts: UpdateOptions,
    client: Client,
    state: watch::Sender<UpdateState>,
    info_tx: watch::Sender<AppUpdateInfo>,
    info: AppUpdateInfo,
    release: Option<Release>,
    releases: Vec<Release>,
    delta_chain: Vec<DeltaStep>,
    token: Option<String>,
    plan: Option<InstallPlan>,
    installer: Option<PathBuf>,
}
impl Drop for UpdateManager {
    fn drop(&mut self) {
        // The manual installer runs after Desktop and Core exit. Superseded
        // staging directories still drop normally when the plan is replaced.
        if self.installer.is_some()
            && let Some(plan) = self.plan.as_mut()
        {
            plan.retain_for_worker();
        }
    }
}
impl UpdateManager {
    pub fn new(opts: UpdateOptions) -> Result<Self> {
        let client = Client::builder()
            .user_agent(format!("Vantry-Updater/{}", opts.current_version))
            .connect_timeout(Duration::from_secs(30))
            .build()?;
        let initial_info = AppUpdateInfo {
            current_version: opts.current_version.clone(),
            phase: "idle".into(),
            ..Default::default()
        };
        let (info_tx, _) = watch::channel(initial_info.clone());
        Ok(Self {
            opts,
            client,
            state: watch::channel(UpdateState::Idle).0,
            info_tx,
            info: initial_info,
            release: None,
            releases: Vec::new(),
            delta_chain: Vec::new(),
            token: None,
            plan: None,
            installer: None,
        })
    }
    pub fn subscribe(&self) -> watch::Receiver<UpdateState> {
        self.state.subscribe()
    }
    /// Subscribe to live [`AppUpdateInfo`] snapshots (including download progress) without locking the manager.
    pub fn info_rx(&self) -> watch::Receiver<AppUpdateInfo> {
        self.info_tx.subscribe()
    }
    fn publish_info(&self) {
        self.info_tx.send_replace(self.current_info());
    }
    pub fn current_info(&self) -> AppUpdateInfo {
        let mut info = self.info.clone();
        if let Some(p) = self
            .plan
            .as_ref()
            .filter(|p| p.version == info.latest_version && p.staged().is_dir())
        {
            info.staged = true;
            info.staged_version = p.version.clone();
            info.staged_path = p.staged().display().to_string();
            info.target_path = p.target().display().to_string();
            info.used_delta = p.used_delta;
            info.installer_path = self
                .installer
                .as_ref()
                .map(|p| p.display().to_string())
                .unwrap_or_default();
            info.can_auto_restart = self.installer.is_some()
                && self.opts.platform.starts_with("darwin-")
                && p.target()
                    .parent()
                    .is_some_and(|d| tempfile::tempdir_in(d).is_ok());
            if info.phase.is_empty() || info.phase == "available" || info.phase == "idle" {
                info.phase = "ready".into();
            }
        }
        info
    }
    fn set(&self, state: UpdateState) {
        self.state.send_replace(state);
    }
    fn begin(&self, active: UpdateState) -> PendingState {
        let previous = self.state.send_replace(active.clone());
        PendingState {
            sender: self.state.clone(),
            active,
            previous,
        }
    }
    fn metadata_url(&self) -> Result<Url> {
        Ok(match &self.opts.source {
            ReleaseSource::Manifest(url) => url.clone(),
            ReleaseSource::GitHub { api_base, repo } => {
                format!(
                    "{}/repos/{}/releases?per_page=100",
                    api_base.trim_end_matches('/'),
                    repo.trim_matches('/')
                )
                .parse()?
            }
        })
    }
    fn request(&self, url: Url, metadata: bool, auth: bool) -> reqwest::RequestBuilder {
        let mut req = self.client.get(url).header(
            "Accept",
            if metadata {
                "application/vnd.github+json"
            } else {
                "application/octet-stream"
            },
        );
        if metadata {
            req = req
                .header("X-GitHub-Api-Version", "2022-11-28")
                .timeout(Duration::from_secs(30));
        }
        if auth && let Some(token) = &self.token {
            req = req.bearer_auth(token);
        }
        req
    }
    async fn metadata_list(&mut self) -> Result<Vec<Release>> {
        let url = self.metadata_url()?;
        let github = matches!(self.opts.source, ReleaseSource::GitHub { .. });
        self.token = if github && self.opts.token.is_some() {
            self.opts
                .token
                .as_deref()
                .map(str::trim)
                .filter(|t| !t.is_empty())
                .map(str::to_owned)
        } else if github {
            read_update_credentials(
                self.opts.local_token.as_deref(),
                self.opts.bundled_token.as_deref(),
            )?
        } else {
            None
        };
        let mut resp = loop {
            let resp = self
                .request(url.clone(), true, self.token.is_some())
                .send()
                .await?;
            if resp.status() == StatusCode::NOT_FOUND
                && let ReleaseSource::GitHub { api_base, repo } = &self.opts.source
            {
                let repo_url = format!(
                    "{}/repos/{}",
                    api_base.trim_end_matches('/'),
                    repo.trim_matches('/')
                )
                .parse()?;
                if self
                    .request(repo_url, true, self.token.is_some())
                    .send()
                    .await?
                    .status()
                    == StatusCode::OK
                {
                    bail!("仓库尚未发布正式 Release");
                }
            }
            if self.token.is_some() && denied(resp.status()) {
                if self.opts.token.is_some() {
                    self.opts.token = Some(String::new());
                }
                self.token = None;
                continue;
            }
            if github && denied(resp.status()) {
                bail!("GitHub repository unavailable; check the dedicated read-only credential");
            }
            break resp.error_for_status()?;
        };
        let mut body = Vec::new();
        while let Some(chunk) = resp.chunk().await? {
            ensure!(
                chunk.len() <= crate::release::MAX_METADATA - body.len(),
                "release metadata too large"
            );
            body.extend_from_slice(&chunk);
        }
        let releases = parse_releases(&body, self.opts.channel)?;
        if releases.is_empty() {
            if github {
                bail!("仓库尚未发布正式 Release");
            }
            bail!("no release for update channel");
        }
        Ok(releases)
    }
    pub async fn check(&mut self) -> Result<AppUpdateInfo> {
        let _pending = self.begin(UpdateState::Checking);
        self.info.phase = "checking".into();
        self.info.error.clear();
        self.publish_info();
        self.release = None;
        self.releases.clear();
        self.delta_chain.clear();
        let releases = match self.metadata_list().await {
            Ok(rs) => rs,
            Err(e) => {
                self.info.phase = "failed".into();
                self.info.error = e.to_string();
                self.publish_info();
                self.set(UpdateState::Failed(e.to_string()));
                return Err(e);
            }
        };
        let r = releases.last().cloned().context("no release for update channel")?;
        let latest = r.version().to_owned();
        let available = compare_versions(&latest, &self.opts.current_version).is_gt();
        let notes = if available {
            combine_release_notes(&releases, &self.opts.current_version, &latest)
        } else {
            let mut n = r.body.as_str();
            if n.len() > 64 << 10 {
                n = &n[..n.floor_char_boundary(64 << 10)];
            }
            n.to_owned()
        };
        let mut info = AppUpdateInfo {
            current_version: self.opts.current_version.clone(),
            latest_version: latest.clone(),
            available,
            notes,
            published_at: r.published_at.clone().unwrap_or_default(),
            release_url: r.html_url.clone(),
            phase: if available { "available" } else { "idle" }.into(),
            ..Default::default()
        };
        if info.release_url.is_empty()
            && let ReleaseSource::GitHub { repo, .. } = &self.opts.source
        {
            info.release_url = format!("https://github.com/{repo}/releases/tag/{}", r.tag_name);
        }
        let mut chain = Vec::new();
        if available {
            let (full, _) = r.assets_for(
                &self.opts.slug,
                &self.opts.platform,
                &self.opts.current_version,
            );
            let full_size = full.as_ref().map(|a| a.size);
            if let Some(a) = full {
                info.asset_name = a.name;
                info.asset_size = a.size.try_into().unwrap_or(i64::MAX);
            }
            if self.opts.target.as_ref().is_some_and(|p| p.is_dir()) {
                chain = plan_delta_chain(
                    &releases,
                    &self.opts.slug,
                    &self.opts.platform,
                    &self.opts.current_version,
                    &latest,
                    full_size,
                );
                if !chain.is_empty() {
                    let total_delta: u64 = chain.iter().map(|s| s.asset.size).sum();
                    info.delta_available = true;
                    info.delta_size = total_delta.try_into().unwrap_or(i64::MAX);
                    info.delta_steps = chain.len() as i64;
                }
            }
        }
        self.info = info;
        self.release = Some(r);
        self.releases = releases;
        self.delta_chain = chain;
        let info = self.current_info();
        if info.staged {
            self.info.phase = "ready".into();
        }
        self.publish_info();
        self.set(if info.staged {
            UpdateState::ReadyToInstall
        } else if available {
            UpdateState::Available
        } else {
            UpdateState::Idle
        });
        Ok(self.current_info())
    }
    async fn download_asset(
        &mut self,
        a: &Asset,
        on_chunk: &mut impl FnMut(u64, u64, u64),
    ) -> Result<(tempfile::NamedTempFile, u64)> {
        let origin = self.metadata_url()?;
        let proxies = resolve_github_proxies(self.opts.github_proxy.as_deref());
        let auth_denied = AtomicBool::new(false);
        let result = download_asset_with_client(
            &self.client,
            &origin,
            self.token.as_deref(),
            &proxies,
            a,
            &auth_denied,
            on_chunk,
        )
        .await;
        if auth_denied.load(Ordering::Relaxed) {
            self.token = None;
        }
        result
    }
    async fn fetch_full(
        &mut self,
        a: &Asset,
        progress: &mut impl FnMut(u64, u64),
    ) -> Result<InstallPlan> {
        self.info.phase = "downloading".into();
        self.info.downloaded_bytes = 0;
        self.info.total_bytes = a.size.try_into().unwrap_or(i64::MAX);
        self.info.speed_bps = 0;
        self.info.delta_step = 0;
        self.info.delta_steps = 0;
        self.info.error.clear();
        self.publish_info();

        let info_tx = self.info_tx.clone();
        let mut snapshot = self.info.clone();
        let mut last_publish = Instant::now();
        let (file, _) = self
            .download_asset(a, &mut |done, total, speed_bps| {
                snapshot.downloaded_bytes = done.try_into().unwrap_or(i64::MAX);
                snapshot.total_bytes = total.try_into().unwrap_or(i64::MAX);
                snapshot.speed_bps = speed_bps.try_into().unwrap_or(i64::MAX);
                if done >= total || last_publish.elapsed() >= PROGRESS_PUBLISH_INTERVAL {
                    last_publish = Instant::now();
                    info_tx.send_replace(snapshot.clone());
                }
                progress(done, total);
            })
            .await?;

        self.info.downloaded_bytes = snapshot.downloaded_bytes;
        self.info.total_bytes = snapshot.total_bytes;
        self.info.speed_bps = 0;
        self.info.phase = "verifying".into();
        self.publish_info();

        let target = self.opts.target.clone();
        let app = self.opts.platform.starts_with("darwin-")
            && target
                .as_ref()
                .is_none_or(|p| p.extension().is_some_and(|e| e.eq_ignore_ascii_case("app")));
        let name = a.name.clone();
        let from = self.opts.current_version.clone();
        let to = self.info.latest_version.clone();
        tokio::task::spawn_blocking(move || {
            InstallPlan::from_asset(file.path(), &name, target.as_deref(), app, &from, &to)
        })
        .await?
    }
    async fn fetch_delta_chain(
        &mut self,
        chain: &[DeltaStep],
        progress: &mut impl FnMut(u64, u64),
    ) -> Result<InstallPlan> {
        let target = self
            .opts
            .target
            .clone()
            .context("delta requires a base installation")?;
        let total_steps = chain.len();
        let total_bytes: u64 = chain.iter().map(|s| s.asset.size).sum();

        self.info.phase = "downloading".into();
        self.info.downloaded_bytes = 0;
        self.info.total_bytes = total_bytes.try_into().unwrap_or(i64::MAX);
        self.info.speed_bps = 0;
        self.info.delta_steps = total_steps as i64;
        self.info.delta_step = 1;
        self.info.error.clear();
        self.publish_info();

        let origin = self.metadata_url()?;
        let proxies = Arc::new(resolve_github_proxies(self.opts.github_proxy.as_deref()));
        let token = self.token.clone();
        let auth_denied = Arc::new(AtomicBool::new(false));
        let combined_done = Arc::new(AtomicU64::new(0));
        let step_speeds: Arc<Vec<AtomicU64>> =
            Arc::new((0..total_steps).map(|_| AtomicU64::new(0)).collect());
        let completed_steps = Arc::new(AtomicUsize::new(0));
        let last_publish = Arc::new(Mutex::new(Instant::now()));
        let info_tx = self.info_tx.clone();
        let base_snapshot = Arc::new(self.info.clone());
        let sem = Arc::new(Semaphore::new(MAX_CONCURRENT_DELTA_DOWNLOADS.min(total_steps.max(1))));
        let (prog_tx, mut prog_rx) = mpsc::unbounded_channel::<(u64, u64)>();

        let mut join_set = JoinSet::new();
        for (idx, step) in chain.iter().enumerate() {
            let client = self.client.clone();
            let origin = origin.clone();
            let token = token.clone();
            let proxies = proxies.clone();
            let auth_denied = auth_denied.clone();
            let combined_done = combined_done.clone();
            let step_speeds = step_speeds.clone();
            let completed_steps = completed_steps.clone();
            let last_publish = last_publish.clone();
            let info_tx = info_tx.clone();
            let base_snapshot = base_snapshot.clone();
            let sem = sem.clone();
            let prog_tx = prog_tx.clone();
            let asset = step.asset.clone();
            let from = step.from.clone();
            let to = step.to.clone();

            join_set.spawn(async move {
                let _permit = sem
                    .acquire_owned()
                    .await
                    .map_err(|_| anyhow::anyhow!("download semaphore closed"))?;
                let mut prev_step_done = 0u64;
                let (file, step_bytes) = download_asset_with_client(
                    &client,
                    &origin,
                    token.as_deref(),
                    &proxies,
                    &asset,
                    &auth_denied,
                    &mut |done, step_total, speed_bps| {
                        let now_done = if done >= prev_step_done {
                            let delta = done - prev_step_done;
                            prev_step_done = done;
                            if delta > 0 {
                                combined_done.fetch_add(delta, Ordering::Relaxed) + delta
                            } else {
                                combined_done.load(Ordering::Relaxed)
                            }
                        } else {
                            let rollback = prev_step_done - done;
                            prev_step_done = done;
                            combined_done
                                .fetch_sub(rollback.min(combined_done.load(Ordering::Relaxed)), Ordering::Relaxed)
                                .saturating_sub(rollback)
                        };
                        step_speeds[idx].store(speed_bps, Ordering::Relaxed);
                        let now_total = if total_bytes > 0 {
                            total_bytes.max(now_done)
                        } else {
                            now_done.max(step_total)
                        };
                        let _ = prog_tx.send((now_done, now_total));
                        let should_publish = now_done >= now_total
                            || last_publish
                                .lock()
                                .ok()
                                .is_some_and(|mut t| {
                                    if t.elapsed() >= PROGRESS_PUBLISH_INTERVAL {
                                        *t = Instant::now();
                                        true
                                    } else {
                                        false
                                    }
                                });
                        if should_publish {
                            let total_speed: u64 =
                                step_speeds.iter().map(|s| s.load(Ordering::Relaxed)).sum();
                            let mut snap = (*base_snapshot).clone();
                            snap.downloaded_bytes = now_done.try_into().unwrap_or(i64::MAX);
                            snap.total_bytes = now_total.try_into().unwrap_or(i64::MAX);
                            snap.speed_bps = total_speed.try_into().unwrap_or(i64::MAX);
                            snap.delta_step = (completed_steps.load(Ordering::Relaxed) + 1)
                                .min(total_steps) as i64;
                            info_tx.send_replace(snap);
                        }
                    },
                )
                .await?;
                step_speeds[idx].store(0, Ordering::Relaxed);
                let done_steps = completed_steps.fetch_add(1, Ordering::Relaxed) + 1;
                let now_done = combined_done.load(Ordering::Relaxed);
                let total_speed: u64 =
                    step_speeds.iter().map(|s| s.load(Ordering::Relaxed)).sum();
                let mut snap = (*base_snapshot).clone();
                snap.downloaded_bytes = now_done.try_into().unwrap_or(i64::MAX);
                snap.total_bytes = total_bytes.max(now_done).try_into().unwrap_or(i64::MAX);
                snap.speed_bps = total_speed.try_into().unwrap_or(i64::MAX);
                snap.delta_step = done_steps.min(total_steps) as i64;
                info_tx.send_replace(snap);
                Ok::<_, anyhow::Error>((idx, file, from, to, step_bytes))
            });
        }
        drop(prog_tx);

        let mut ordered: Vec<Option<(tempfile::NamedTempFile, String, String)>> =
            (0..total_steps).map(|_| None).collect();
        let mut downloaded_total = 0u64;
        let mut last_reported_done = 0u64;
        let mut first_err: Option<anyhow::Error> = None;

        while !join_set.is_empty() {
            tokio::select! {
                Some((done, total)) = prog_rx.recv() => {
                    if done >= last_reported_done {
                        last_reported_done = done;
                        progress(done, total.max(done));
                    }
                }
                joined = join_set.join_next() => {
                    match joined {
                        Some(Ok(Ok((idx, file, from, to, bytes)))) => {
                            downloaded_total = downloaded_total.saturating_add(bytes);
                            ordered[idx] = Some((file, from, to));
                        }
                        Some(Ok(Err(e))) => {
                            if first_err.is_none() {
                                first_err = Some(e);
                                join_set.abort_all();
                            }
                        }
                        Some(Err(e)) => {
                            if first_err.is_none() && !e.is_cancelled() {
                                first_err = Some(e.into());
                                join_set.abort_all();
                            }
                        }
                        None => break,
                    }
                }
            }
        }
        while let Ok((done, total)) = prog_rx.try_recv() {
            if done >= last_reported_done {
                last_reported_done = done;
                progress(done, total.max(done));
            }
        }
        if auth_denied.load(Ordering::Relaxed) {
            self.token = None;
        }
        if let Some(e) = first_err {
            return Err(e);
        }
        let temp_files: Vec<(tempfile::NamedTempFile, String, String)> = ordered
            .into_iter()
            .map(|slot| slot.context("missing downloaded delta step"))
            .collect::<Result<_>>()?;

        self.info.downloaded_bytes = downloaded_total.try_into().unwrap_or(i64::MAX);
        self.info.total_bytes = total_bytes
            .max(downloaded_total)
            .try_into()
            .unwrap_or(i64::MAX);
        self.info.speed_bps = 0;
        self.info.phase = "applying_delta".into();
        self.info.delta_step = 1;
        self.publish_info();

        let app = self.opts.platform.starts_with("darwin-")
            && target.extension().is_some_and(|e| e.eq_ignore_ascii_case("app"));
        let mut apply_snapshot = self.info.clone();
        let apply_tx = self.info_tx.clone();
        let plan = tokio::task::spawn_blocking(move || {
            let paths: Vec<(PathBuf, String, String)> = temp_files
                .iter()
                .map(|(f, from, to)| (f.path().to_path_buf(), from.clone(), to.clone()))
                .collect();
            let result = InstallPlan::from_delta_chain(&paths, &target, app, |step, total| {
                apply_snapshot.phase = "applying_delta".into();
                apply_snapshot.delta_step = step as i64;
                apply_snapshot.delta_steps = total as i64;
                apply_tx.send_replace(apply_snapshot.clone());
            });
            drop(temp_files);
            result
        })
        .await??;

        self.info.phase = "verifying".into();
        self.publish_info();
        Ok(plan)
    }
    pub async fn download_and_stage(
        &mut self,
        mut progress: impl FnMut(u64, u64),
    ) -> Result<&InstallPlan> {
        if self.release.is_none() {
            self.check().await?;
        }
        ensure!(self.info.available, "already up to date");
        if self.plan.as_ref().is_some_and(|p| {
            p.version == self.info.latest_version
                && crate::install::verify(p.staged(), p.target()).is_ok()
        }) {
            self.info.phase = "ready".into();
            self.publish_info();
            self.set(UpdateState::ReadyToInstall);
            let p = self.plan.as_ref().unwrap();
            let size = if p.used_delta {
                self.info.delta_size
            } else {
                self.info.asset_size
            } as u64;
            progress(size, size);
            return Ok(p);
        }
        let _pending = self.begin(UpdateState::Downloading);
        let (full, _) = self.release.as_ref().unwrap().assets_for(
            &self.opts.slug,
            &self.opts.platform,
            &self.opts.current_version,
        );
        let chain = self.delta_chain.clone();
        let result = async {
            if !chain.is_empty() && self.info.delta_available {
                match self.fetch_delta_chain(&chain, &mut progress).await {
                    Ok(p) => return Ok(p),
                    Err(e) if full.is_none() => return Err(e),
                    Err(_) => {
                        self.info.delta_available = false;
                    }
                }
            }
            self.fetch_full(&full.context("no matching release asset")?, &mut progress)
                .await
        }
        .await
        .and_then(|p| {
            self.installer = if self.opts.platform.starts_with("darwin-")
                && p.target()
                    .extension()
                    .is_some_and(|e| e.eq_ignore_ascii_case("app"))
            {
                Some(p.installer(self.opts.data_dir.as_deref().unwrap_or(Path::new("")))?)
            } else {
                None
            };
            Ok(p)
        });
        match result {
            Ok(p) => {
                self.plan = Some(p);
                self.info.phase = "ready".into();
                self.info.speed_bps = 0;
                self.info.error.clear();
                self.publish_info();
                self.set(UpdateState::ReadyToInstall);
                Ok(self.plan.as_ref().unwrap())
            }
            Err(e) => {
                self.info.phase = "failed".into();
                self.info.speed_bps = 0;
                self.info.error = e.to_string();
                self.publish_info();
                self.set(UpdateState::Failed(e.to_string()));
                Err(e)
            }
        }
    }
    pub fn apply(&mut self, opts: &crate::ApplyOptions) -> Result<()> {
        let plan = self.plan.as_ref().context("download an update first")?;
        self.info.phase = "applying".into();
        self.publish_info();
        self.set(UpdateState::Applying);
        match crate::apply_staged(plan.staged(), plan.target(), opts) {
            Ok(()) => {
                self.opts.current_version = plan.version.clone();
                self.info = AppUpdateInfo {
                    current_version: plan.version.clone(),
                    phase: "idle".into(),
                    ..Default::default()
                };
                self.plan = None;
                self.release = None;
                self.releases.clear();
                self.delta_chain.clear();
                self.publish_info();
                self.set(UpdateState::Idle);
                Ok(())
            }
            Err(e) => {
                self.info.phase = "failed".into();
                self.info.error = e.to_string();
                self.publish_info();
                self.set(UpdateState::Failed(e.to_string()));
                Err(e)
            }
        }
    }
    pub fn launch_apply(
        &mut self,
        version: &str,
        desktop_pid: u32,
        allow_busy_core: bool,
    ) -> Result<()> {
        let plan = self.plan.as_ref().context("download an update first")?;
        ensure!(
            version.is_empty() || normalize_version(version) == normalize_version(&plan.version),
            "staged version mismatch"
        );
        let data = self.opts.data_dir.as_deref().unwrap_or(Path::new(""));
        self.info.phase = "applying".into();
        self.publish_info();
        self.set(UpdateState::Applying);
        crate::launch_apply(
            self.plan.as_mut().unwrap(),
            data,
            desktop_pid,
            allow_busy_core,
        )
        .inspect_err(|e| {
            self.info.phase = "failed".into();
            self.info.error = e.to_string();
            self.publish_info();
            self.set(UpdateState::Failed(e.to_string()));
        })?;
        Ok(())
    }
}
fn denied(status: StatusCode) -> bool {
    matches!(
        status,
        StatusCode::FORBIDDEN | StatusCode::NOT_FOUND | StatusCode::UNAUTHORIZED
    )
}

/// Parse an HTTP `Content-Range: bytes <start>-<end>/<total>` header.
fn parse_content_range(value: &str) -> Option<(u64, u64, u64)> {
    let rest = value.trim().strip_prefix("bytes ")?;
    let (range, total) = rest.split_once('/')?;
    let (start, end) = range.split_once('-')?;
    let start: u64 = start.trim().parse().ok()?;
    let end: u64 = end.trim().parse().ok()?;
    let total: u64 = total.trim().parse().ok()?;
    (end >= start && end < total).then_some((start, end, total))
}

fn validate_response(
    resp: &Response,
    resume_offset: u64,
    declared_size: u64,
) -> Result<(bool, u64)> {
    match resp.status() {
        StatusCode::PARTIAL_CONTENT if resume_offset > 0 => {
            let range_hdr = resp
                .headers()
                .get(reqwest::header::CONTENT_RANGE)
                .and_then(|v| v.to_str().ok())
                .context("206 response missing Content-Range")?;
            let (start, _end, total) =
                parse_content_range(range_hdr).context("invalid Content-Range")?;
            ensure!(
                start == resume_offset
                    && total <= MAX_ASSET
                    && (declared_size == 0 || total == declared_size),
                "unexpected Content-Range {range_hdr}"
            );
            Ok((true, total))
        }
        StatusCode::OK => {
            let size = resp.content_length().unwrap_or(declared_size);
            ensure!(
                size <= MAX_ASSET && (declared_size == 0 || size == declared_size),
                "release size mismatch"
            );
            Ok((false, size))
        }
        other => bail!("asset download returned {other}"),
    }
}

/// Race untried candidate URLs using staggered hedging so a stalled or slow-to-connect
/// proxy never blocks the download for 15 seconds at 0%.
async fn race_candidates(
    client: &Client,
    candidates: &[(Url, bool)],
    tried: &mut [bool],
    resume_offset: u64,
    declared_size: u64,
) -> Result<(usize, Response, bool, u64)> {
    let pending: Vec<usize> = (0..candidates.len()).filter(|&i| !tried[i]).collect();
    ensure!(!pending.is_empty(), "no remaining download candidates");

    let mut next_pos = 0usize;
    let mut set: JoinSet<(usize, Result<(Response, bool, u64)>)> = JoinSet::new();
    let mut last_err: Option<anyhow::Error> = None;

    let spawn_one = |idx: usize, set: &mut JoinSet<(usize, Result<(Response, bool, u64)>)>| {
        let client = client.clone();
        let (url, is_proxy) = candidates[idx].clone();
        let timeout = if is_proxy {
            PROXY_CONNECT_RESPONSE_TIMEOUT
        } else {
            DIRECT_CONNECT_RESPONSE_TIMEOUT
        };
        set.spawn(async move {
            let mut req = client
                .get(url.clone())
                .header("Accept", "application/octet-stream");
            if resume_offset > 0 {
                req = req.header(reqwest::header::RANGE, format!("bytes={resume_offset}-"));
            }
            let res = match tokio::time::timeout(timeout, req.send()).await {
                Ok(Ok(resp)) => match validate_response(&resp, resume_offset, declared_size) {
                    Ok((resumed, total)) => Ok((resp, resumed, total)),
                    Err(e) => Err(e),
                },
                Ok(Err(e)) => Err(e.into()),
                Err(_) => Err(anyhow::anyhow!("request timed out: {url}")),
            };
            (idx, res)
        });
    };

    // Launch the first candidate immediately.
    spawn_one(pending[next_pos], &mut set);
    next_pos += 1;

    let hedge = tokio::time::sleep(HEDGE_DELAY);
    tokio::pin!(hedge);

    while !set.is_empty() || next_pos < pending.len() {
        if set.is_empty() && next_pos < pending.len() {
            spawn_one(pending[next_pos], &mut set);
            next_pos += 1;
            hedge.as_mut().reset(tokio::time::Instant::now() + HEDGE_DELAY);
        }
        tokio::select! {
            _ = &mut hedge, if next_pos < pending.len() => {
                spawn_one(pending[next_pos], &mut set);
                next_pos += 1;
                hedge.as_mut().reset(tokio::time::Instant::now() + HEDGE_DELAY);
            }
            joined = set.join_next() => {
                match joined {
                    Some(Ok((idx, Ok((resp, resumed, total))))) => {
                        set.abort_all();
                        return Ok((idx, resp, resumed, total));
                    }
                    Some(Ok((idx, Err(e)))) => {
                        tried[idx] = true;
                        last_err = Some(e);
                        // Immediately launch the next unstarted candidate on fast failure.
                        if next_pos < pending.len() {
                            spawn_one(pending[next_pos], &mut set);
                            next_pos += 1;
                            hedge.as_mut().reset(tokio::time::Instant::now() + HEDGE_DELAY);
                        }
                    }
                    Some(Err(e)) => {
                        if !e.is_cancelled() {
                            last_err = Some(e.into());
                        }
                    }
                    None => break,
                }
            }
        }
    }

    Err(last_err.unwrap_or_else(|| anyhow::anyhow!("asset download failed")))
}

async fn download_candidates_with_resume(
    client: &Client,
    candidates: &[(Url, bool)],
    declared_size: u64,
    on_chunk: &mut impl FnMut(u64, u64, u64),
) -> Result<(tempfile::NamedTempFile, u64)> {
    let mut tried = vec![false; candidates.len()];
    let mut file = tempfile::NamedTempFile::new()?;
    let mut written = 0u64;
    let mut last_err: Option<anyhow::Error> = None;

    while tried.iter().any(|&t| !t) {
        let (idx, mut resp, resumed, size) =
            match race_candidates(client, candidates, &mut tried, written, declared_size).await {
                Ok(winner) => winner,
                Err(e) => {
                    last_err = Some(e);
                    break;
                }
            };
        let is_proxy = candidates[idx].1;
        tried[idx] = true;
        let has_more_candidates = tried.iter().any(|&t| !t);

        if !resumed && written > 0 {
            file.as_file_mut().set_len(0)?;
            file.as_file_mut().seek(SeekFrom::Start(0))?;
            written = 0;
        } else if resumed {
            file.as_file_mut().seek(SeekFrom::Start(written))?;
        }

        let attempt_start = Instant::now();
        let mut attempt_bytes = 0u64;
        let mut speed_window_start = Instant::now();
        let mut speed_window_bytes = 0u64;
        let mut smoothed_bps = 0u64;

        let mut writer = BufWriter::with_capacity(DOWNLOAD_WRITE_BUFFER, file);
        let stream_result: Result<()> = loop {
            let chunk = match tokio::time::timeout(CHUNK_READ_TIMEOUT, resp.chunk()).await {
                Ok(Ok(c)) => c,
                Ok(Err(e)) => break Err(e.into()),
                Err(_) => break Err(anyhow::anyhow!("download stalled")),
            };
            let Some(chunk) = chunk else {
                if size == 0 || written == size {
                    break Ok(());
                }
                break Err(anyhow::anyhow!("incomplete download"));
            };
            let len = chunk.len() as u64;
            written += len;
            attempt_bytes += len;
            speed_window_bytes += len;
            if written > MAX_ASSET || (size > 0 && written > size) {
                break Err(anyhow::anyhow!("download exceeds declared size"));
            }
            if let Err(e) = writer.write_all(&chunk) {
                break Err(e.into());
            }

            let win_dt = speed_window_start.elapsed();
            if win_dt >= Duration::from_millis(250) {
                let inst_bps = ((speed_window_bytes as f64) / win_dt.as_secs_f64()) as u64;
                smoothed_bps = if smoothed_bps == 0 {
                    inst_bps
                } else {
                    (smoothed_bps * 3 + inst_bps) / 4
                };
                speed_window_start = Instant::now();
                speed_window_bytes = 0;
            } else if smoothed_bps == 0 {
                let dt = attempt_start.elapsed().as_secs_f64();
                if dt >= 0.05 {
                    smoothed_bps = (attempt_bytes as f64 / dt) as u64;
                }
            }

            on_chunk(written, size, smoothed_bps);

            if is_proxy && has_more_candidates {
                let elapsed = attempt_start.elapsed();
                if elapsed >= LOW_SPEED_GRACE {
                    let avg_bps = (attempt_bytes as f64 / elapsed.as_secs_f64()) as u64;
                    if avg_bps < MIN_PROXY_SPEED_BPS {
                        break Err(anyhow::anyhow!(
                            "proxy throughput too slow ({avg_bps} B/s), switching mirror"
                        ));
                    }
                }
            }
        };

        writer.flush()?;
        file = writer.into_inner().map_err(|e| e.into_error())?;

        match stream_result {
            Ok(()) => {
                on_chunk(written, written, 0);
                return Ok((file, written));
            }
            Err(e) if has_more_candidates => {
                last_err = Some(e);
                continue;
            }
            Err(e) => return Err(e),
        }
    }

    Err(last_err.unwrap_or_else(|| anyhow::anyhow!("asset download failed")))
}

async fn download_asset_with_client(
    client: &Client,
    origin: &Url,
    token: Option<&str>,
    proxies: &[String],
    a: &Asset,
    auth_denied: &AtomicBool,
    on_chunk: &mut impl FnMut(u64, u64, u64),
) -> Result<(tempfile::NamedTempFile, u64)> {
    ensure!(a.size <= MAX_ASSET, "asset too large");
    let mut use_token = token
        .filter(|t| !t.is_empty())
        .filter(|_| !auth_denied.load(Ordering::Relaxed));

    // When proxies are configured (e.g. mainland China) and the asset has a public GitHub
    // `browser_download_url`, race the public mirror pool first even if a local/bundled
    // `update-token` exists. Private repositories (404 on public URL) cleanly fall back to
    // authenticated `api.github.com` asset download below.
    if !proxies.is_empty() && !a.browser_download_url.is_empty() {
        if let Ok(browser_url) = origin.join(&a.browser_download_url) {
            let candidates = github_download_candidates(&browser_url, false, proxies);
            if candidates.len() > 1 {
                match download_candidates_with_resume(client, &candidates, a.size, on_chunk).await {
                    Ok(ok) => return Ok(ok),
                    Err(err) if use_token.is_none() || a.url.is_empty() => return Err(err),
                    Err(_) => {
                        // Fall through to authenticated API download for private repositories.
                    }
                }
            }
        }
    }

    let (base_url, send_auth) = loop {
        let auth = use_token.is_some();
        let raw = if (auth && !a.url.is_empty()) || a.browser_download_url.is_empty() {
            &a.url
        } else {
            &a.browser_download_url
        };
        ensure!(!raw.is_empty(), "missing asset URL");
        let url = origin.join(raw)?;
        let send_auth = auth && url.origin() == origin.origin();
        if send_auth {
            let mut req = client
                .get(url.clone())
                .header("Accept", "application/octet-stream");
            if let Some(tok) = use_token {
                req = req.bearer_auth(tok);
            }
            let resp = req.send().await?;
            if denied(resp.status()) {
                auth_denied.store(true, Ordering::Relaxed);
                use_token = None;
                continue;
            }
            let (_, size) = validate_response(&resp, 0, a.size)?;
            return stream_single_response(resp, size, on_chunk).await;
        }
        break (url, false);
    };

    let candidates = github_download_candidates(&base_url, send_auth, proxies);
    download_candidates_with_resume(client, &candidates, a.size, on_chunk).await
}

async fn stream_single_response(
    mut resp: Response,
    size: u64,
    on_chunk: &mut impl FnMut(u64, u64, u64),
) -> Result<(tempfile::NamedTempFile, u64)> {
    let file = tempfile::NamedTempFile::new()?;
    let mut writer = BufWriter::with_capacity(DOWNLOAD_WRITE_BUFFER, file);
    let mut bytes = 0u64;
    let start = Instant::now();
    while let Some(chunk) = tokio::time::timeout(CHUNK_READ_TIMEOUT, resp.chunk())
        .await
        .map_err(|_| anyhow::anyhow!("download stalled"))??
    {
        bytes += chunk.len() as u64;
        ensure!(
            bytes <= MAX_ASSET && (size == 0 || bytes <= size),
            "download exceeds declared size"
        );
        writer.write_all(&chunk)?;
        let dt = start.elapsed().as_secs_f64();
        let bps = if dt >= 0.05 { (bytes as f64 / dt) as u64 } else { 0 };
        on_chunk(bytes, size, bps);
    }
    ensure!(size == 0 || bytes == size, "incomplete download");
    writer.flush()?;
    let file = writer.into_inner().map_err(|e| e.into_error())?;
    on_chunk(bytes, bytes, 0);
    Ok((file, bytes))
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        net::TcpListener,
    };

    #[tokio::test]
    async fn failover_resumes_mid_stream_via_http_range() -> Result<()> {
        let payload: Vec<u8> = (0..131_072u32).map(|i| (i * 31 % 251) as u8).collect();
        let split = payload.len() / 2;

        // Server A sends half the body with full Content-Length and drops the connection.
        let listener_a = TcpListener::bind("127.0.0.1:0").await?;
        let url_a = Url::parse(&format!("http://{}/asset", listener_a.local_addr()?))?;
        let payload_a = payload.clone();
        let srv_a = tokio::spawn(async move {
            let (mut sock, _) = listener_a.accept().await?;
            let mut req = Vec::new();
            while !req.ends_with(b"\r\n\r\n") {
                req.push(sock.read_u8().await?);
            }
            sock.write_all(
                format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    payload_a.len()
                )
                .as_bytes(),
            )
            .await?;
            sock.write_all(&payload_a[..split]).await?;
            sock.shutdown().await?;
            Ok::<_, anyhow::Error>(())
        });

        // Server B honors `Range: bytes=<split>-` with 206 Partial Content.
        let listener_b = TcpListener::bind("127.0.0.1:0").await?;
        let url_b = Url::parse(&format!("http://{}/asset", listener_b.local_addr()?))?;
        let payload_b = payload.clone();
        let saw_range = Arc::new(AtomicBool::new(false));
        let saw_range_srv = saw_range.clone();
        let srv_b = tokio::spawn(async move {
            let (mut sock, _) = listener_b.accept().await?;
            let mut req = Vec::new();
            while !req.ends_with(b"\r\n\r\n") {
                req.push(sock.read_u8().await?);
            }
            let text = String::from_utf8(req)?.to_ascii_lowercase();
            let expected = format!("range: bytes={split}-");
            assert!(text.contains(&expected), "missing Range header in: {text}");
            saw_range_srv.store(true, Ordering::Relaxed);
            let tail = &payload_b[split..];
            sock.write_all(
                format!(
                    "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Range: bytes {}-{}/{}\r\nConnection: close\r\n\r\n",
                    tail.len(),
                    split,
                    payload_b.len() - 1,
                    payload_b.len()
                )
                .as_bytes(),
            )
            .await?;
            sock.write_all(tail).await?;
            Ok::<_, anyhow::Error>(())
        });

        let client = Client::builder().build()?;
        let candidates = vec![(url_a, true), (url_b, false)];
        let (file, got) =
            download_candidates_with_resume(&client, &candidates, payload.len() as u64, &mut |_, _, _| {})
                .await?;
        srv_a.await??;
        srv_b.await??;

        assert!(saw_range.load(Ordering::Relaxed));
        assert_eq!(got as usize, payload.len());
        assert_eq!(std::fs::read(file.path())?, payload);
        Ok(())
    }

    #[tokio::test]
    async fn hedged_racing_bypasses_stalled_first_mirror() -> Result<()> {
        let payload = b"fast-mirror-wins-before-slow-timeout".to_vec();

        // Server A accepts TCP and never replies.
        let listener_a = TcpListener::bind("127.0.0.1:0").await?;
        let url_a = Url::parse(&format!("http://{}/slow", listener_a.local_addr()?))?;
        let _srv_a = tokio::spawn(async move {
            if let Ok((_sock, _)) = listener_a.accept().await {
                tokio::time::sleep(Duration::from_secs(30)).await;
            }
        });

        // Server B responds immediately once hedged.
        let listener_b = TcpListener::bind("127.0.0.1:0").await?;
        let url_b = Url::parse(&format!("http://{}/fast", listener_b.local_addr()?))?;
        let payload_b = payload.clone();
        let srv_b = tokio::spawn(async move {
            let (mut sock, _) = listener_b.accept().await?;
            let mut req = Vec::new();
            while !req.ends_with(b"\r\n\r\n") {
                req.push(sock.read_u8().await?);
            }
            sock.write_all(
                format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    payload_b.len()
                )
                .as_bytes(),
            )
            .await?;
            sock.write_all(&payload_b).await?;
            Ok::<_, anyhow::Error>(())
        });

        let client = Client::builder().build()?;
        let start = Instant::now();
        let candidates = vec![(url_a, true), (url_b, false)];
        let (file, got) =
            download_candidates_with_resume(&client, &candidates, payload.len() as u64, &mut |_, _, _| {})
                .await?;
        let elapsed = start.elapsed();
        srv_b.await??;

        assert_eq!(got as usize, payload.len());
        assert_eq!(std::fs::read(file.path())?, payload);
        assert!(
            elapsed < Duration::from_secs(5),
            "expected hedge to win quickly, took {elapsed:?}"
        );
        Ok(())
    }
}
