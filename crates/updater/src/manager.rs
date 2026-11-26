use crate::{
    Asset, Channel, DeltaStep, InstallPlan, MAX_ASSET, Release, bundled_token_file,
    combine_release_notes, compare_versions, normalize_version, parse_releases, plan_delta_chain,
    read_update_credentials, resolve_current_app_bundle, update_token_file,
};
use anyhow::{Context, Result, bail, ensure};
use reqwest::{Client, Response, StatusCode, Url};
use std::{
    io::Write,
    path::{Path, PathBuf},
    time::Duration,
};
use tokio::sync::watch;
pub use vantry_protocol::AppUpdateInfo;

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
                chunk.len() <= (1 << 20) - body.len(),
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
        on_chunk: &mut impl FnMut(u64, u64),
    ) -> Result<(tempfile::NamedTempFile, u64)> {
        ensure!(a.size <= MAX_ASSET, "asset too large");
        let origin = self.metadata_url()?;
        let mut resp: Response = loop {
            let auth = self.token.is_some();
            let raw = if (auth && !a.url.is_empty()) || a.browser_download_url.is_empty() {
                &a.url
            } else {
                &a.browser_download_url
            };
            ensure!(!raw.is_empty(), "missing asset URL");
            let url = origin.join(raw)?;
            let auth = auth && url.origin() == origin.origin();
            let resp = self.request(url, false, auth).send().await?;
            if auth && denied(resp.status()) {
                self.token = None;
                continue;
            }
            ensure!(
                matches!(resp.status(), StatusCode::OK | StatusCode::PARTIAL_CONTENT),
                "asset download returned {}",
                resp.status()
            );
            break resp;
        };
        let size = resp.content_length().unwrap_or(a.size);
        ensure!(
            size <= MAX_ASSET && (a.size == 0 || size == a.size),
            "release size mismatch"
        );
        let mut file = tempfile::NamedTempFile::new()?;
        let mut bytes = 0;
        while let Some(chunk) = resp.chunk().await? {
            bytes += chunk.len() as u64;
            ensure!(
                bytes <= MAX_ASSET && (size == 0 || bytes <= size),
                "download exceeds declared size"
            );
            file.write_all(&chunk)?;
            on_chunk(bytes, size);
        }
        ensure!(size == 0 || bytes == size, "incomplete download");
        on_chunk(bytes, bytes);
        Ok((file, bytes))
    }
    async fn fetch_full(
        &mut self,
        a: &Asset,
        progress: &mut impl FnMut(u64, u64),
    ) -> Result<InstallPlan> {
        self.info.phase = "downloading".into();
        self.info.downloaded_bytes = 0;
        self.info.total_bytes = a.size.try_into().unwrap_or(i64::MAX);
        self.info.delta_step = 0;
        self.info.delta_steps = 0;
        self.info.error.clear();
        self.publish_info();

        let info_tx = self.info_tx.clone();
        let mut snapshot = self.info.clone();
        let (file, _) = self
            .download_asset(a, &mut |done, total| {
                snapshot.downloaded_bytes = done.try_into().unwrap_or(i64::MAX);
                snapshot.total_bytes = total.try_into().unwrap_or(i64::MAX);
                info_tx.send_replace(snapshot.clone());
                progress(done, total);
            })
            .await?;

        self.info.downloaded_bytes = snapshot.downloaded_bytes;
        self.info.total_bytes = snapshot.total_bytes;
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
        self.info.delta_steps = total_steps as i64;
        self.info.delta_step = 1;
        self.info.error.clear();
        self.publish_info();

        let info_tx = self.info_tx.clone();
        let mut snapshot = self.info.clone();
        let mut temp_files = Vec::with_capacity(total_steps);
        let mut downloaded_before: u64 = 0;

        for (idx, step) in chain.iter().enumerate() {
            snapshot.delta_step = (idx + 1) as i64;
            info_tx.send_replace(snapshot.clone());
            let (file, step_bytes) = self
                .download_asset(&step.asset, &mut |done, step_total| {
                    let combined_done = downloaded_before.saturating_add(done);
                    let combined_total = if total_bytes > 0 {
                        total_bytes.max(combined_done)
                    } else {
                        downloaded_before.saturating_add(step_total)
                    };
                    snapshot.downloaded_bytes = combined_done.try_into().unwrap_or(i64::MAX);
                    snapshot.total_bytes = combined_total.try_into().unwrap_or(i64::MAX);
                    info_tx.send_replace(snapshot.clone());
                    progress(combined_done, combined_total);
                })
                .await?;
            downloaded_before = downloaded_before.saturating_add(step_bytes);
            temp_files.push((file, step.from.clone(), step.to.clone()));
        }

        self.info.downloaded_bytes = downloaded_before.try_into().unwrap_or(i64::MAX);
        self.info.total_bytes = total_bytes
            .max(downloaded_before)
            .try_into()
            .unwrap_or(i64::MAX);
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
                self.info.error.clear();
                self.publish_info();
                self.set(UpdateState::ReadyToInstall);
                Ok(self.plan.as_ref().unwrap())
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
