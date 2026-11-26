mod alma;
mod ctl;
mod federation;
mod image;
mod mcp_probe;
mod orchestration;
mod plugin_source;
mod provider;
mod qoder;
#[cfg(target_os = "macos")]
mod rd_macos;
mod relay;
mod settings;
mod usage;
mod workbench;
mod workbench_git;
#[cfg(unix)]
mod workbench_terminal;
use anyhow::{Context, Result, ensure};
use clap::{Args, Parser, Subcommand};
use std::{
    ffi::OsString,
    path::{Path, PathBuf},
    process::Stdio,
    sync::Arc,
    time::Duration,
};
use tokio_util::sync::CancellationToken;
use vantry_client::CoreClient;
use vantry_core::{Core, CoreOptions, server::Server};
use vantry_platform::{hostfs, hostplatform::Paths};
use vantry_protocol::WorkspaceSpec;

#[derive(Parser)]
#[command(
    name = "vantry-service",
    version,
    about = "Vantry Core, provider worker and local control plane"
)]
struct Cli {
    #[command(subcommand)]
    command: Role,
}
#[derive(Subcommand)]
enum Role {
    Core(CoreArgs),
    EnsureCore(CoreArgs),
    Ctl(ctl::Ctl),
    Relay(relay::Args),
    ProviderWorker(JobDir),
    WorkerControl(JobDir),
    WorkerStop(JobDir),
    SmokeWorker,
    #[command(hide = true)]
    ModelPluginHost(qoder::host::HostArgs),
    PiSetup,
    ApplyUpdate {
        #[arg(trailing_var_arg = true, allow_hyphen_values = true)]
        args: Vec<String>,
    },
    /// Expose a local Core socket proxying a paired remote peer over mTLS/relay.
    PeerProxy(PeerProxyArgs),
}
#[derive(Args)]
struct PeerProxyArgs {
    #[arg(long)]
    data_dir: Option<PathBuf>,
    #[arg(long, allow_hyphen_values = true)]
    peer: String,
    #[arg(long, default_value = "auto")]
    mode: String,
    #[arg(long)]
    socket: PathBuf,
}
#[derive(Args)]
struct JobDir {
    #[arg(long)]
    job_dir: PathBuf,
}

#[derive(Args, Clone, Default)]
pub struct WorkspaceArgs {
    #[arg(long)]
    workspace: Option<PathBuf>,
    #[arg(long)]
    workspace_kind: Option<String>,
    #[arg(long)]
    repository: Option<PathBuf>,
    #[arg(long)]
    branch: Option<String>,
    #[arg(long)]
    base_ref: Option<String>,
}
impl WorkspaceArgs {
    fn spec(&self) -> Result<Option<WorkspaceSpec>> {
        if self.workspace.is_none()
            && self.repository.is_none()
            && self.workspace_kind.is_none()
            && self.branch.is_none()
            && self.base_ref.is_none()
        {
            return Ok(None);
        }
        let kind = self
            .workspace_kind
            .as_deref()
            .unwrap_or(if self.repository.is_some() {
                "worktree"
            } else {
                "directory"
            });
        ensure!(
            match kind {
                "directory" =>
                    self.workspace.is_some()
                        && self.repository.is_none()
                        && self.branch.is_none()
                        && self.base_ref.is_none(),
                "worktree" => self.repository.is_some(),
                _ => false,
            },
            "invalid workspace specification"
        );
        let absolute = |p: Option<&PathBuf>| -> Result<String> {
            Ok(p.map(std::path::absolute)
                .transpose()?
                .map(|p| p.display().to_string())
                .unwrap_or_default())
        };
        Ok(Some(WorkspaceSpec {
            kind: kind.into(),
            path: absolute(self.workspace.as_ref())?,
            repository: absolute(self.repository.as_ref())?,
            branch: self.branch.clone().unwrap_or_default(),
            base_ref: self.base_ref.clone().unwrap_or_default(),
            ..Default::default()
        }))
    }
}
#[derive(Args, Clone)]
struct CoreArgs {
    #[arg(long)]
    data_dir: Option<PathBuf>,
    #[arg(long)]
    socket: Option<PathBuf>,
    #[arg(long,value_parser=["pi","controlled"])]
    provider: Option<String>,
    #[arg(long)]
    model: Option<String>,
    #[arg(long,default_value="80ms",value_parser=parse_duration)]
    controlled_delay: Duration,
    #[arg(long)]
    pi_executable: Option<PathBuf>,
    #[arg(long)]
    magpie_settings: Option<PathBuf>,
    #[arg(long)]
    listen_remote: Option<String>,
    /// Alma-compatible relay base URL for opt-in mobile access.
    #[arg(long, requires = "alma_thread")]
    alma_relay: Option<String>,
    /// The single Core thread shared by this Alma pairing.
    #[arg(long, requires = "alma_relay")]
    alma_thread: Option<String>,
    /// Invalidate the previous Alma pairing when starting this host.
    #[arg(long, requires = "alma_relay")]
    alma_reset_pairing: bool,
    #[command(flatten)]
    workspace: WorkspaceArgs,
}
fn parse_duration(s: &str) -> Result<Duration, String> {
    if s == "0" {
        return Ok(Duration::ZERO);
    }
    let mut remaining = s;
    let mut total = 0.;
    while !remaining.is_empty() {
        let n = remaining
            .find(|c: char| !c.is_ascii_digit() && c != '.')
            .ok_or("duration needs a unit")?;
        let value: f64 = remaining[..n].parse().map_err(|_| "invalid duration")?;
        remaining = &remaining[n..];
        let (unit, scale) = [
            ("ms", 0.001),
            ("us", 0.000001),
            ("µs", 0.000001),
            ("ns", 0.000000001),
            ("s", 1.),
            ("m", 60.),
            ("h", 3600.),
        ]
        .into_iter()
        .find(|(u, _)| remaining.starts_with(u))
        .ok_or("invalid duration unit")?;
        remaining = &remaining[unit.len()..];
        total += value * scale;
    }
    Duration::try_from_secs_f64(total).map_err(|e| e.to_string())
}
fn data_dir(path: Option<&Path>) -> Result<PathBuf> {
    match path {
        Some(path) => Ok(std::path::absolute(path)?),
        None => Ok(Paths::discover()?.data),
    }
}
pub(crate) fn detach_tokio_command(cmd: &mut tokio::process::Command) -> &mut tokio::process::Command {
    #[cfg(unix)]
    {
        cmd.process_group(0);
    }
    #[cfg(windows)]
    {
        const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW);
    }
    cmd
}

#[cfg(unix)]
fn signal() -> Result<impl Future<Output = ()>> {
    use tokio::signal::unix::{SignalKind, signal};
    let mut term = signal(SignalKind::terminate())?;
    let mut int = signal(SignalKind::interrupt())?;
    Ok(async move {
        tokio::select! {_=term.recv()=>{},_=int.recv()=>{}}
    })
}

#[cfg(not(unix))]
fn signal() -> Result<impl Future<Output = ()>> {
    Ok(async move {
        let _ = tokio::signal::ctrl_c().await;
    })
}
async fn serve(args: CoreArgs) -> Result<()> {
    let data = data_dir(args.data_dir.as_deref())?;
    hostfs::ensure_private_dir(&data)?;
    let provider_id = args
        .provider
        .or_else(|| std::env::var("VANTRY_PROVIDER").ok())
        .unwrap_or_else(|| "pi".into());
    ensure!(
        matches!(provider_id.as_str(), "controlled" | "pi"),
        "unknown provider"
    );
    let model = args
        .model
        .or_else(|| std::env::var("VANTRY_MODEL").ok())
        .unwrap_or_default();
    let options = CoreOptions {
        provider_id: provider_id.clone(),
        model: model.clone(),
        workspace: args.workspace.spec()?,
        ..Default::default()
    };
    let core = Core::open_with_options(&data, options.clone())?;
    let data = core.directory().to_owned();
    let ext = Arc::new(workbench::Workbench::new(
        core.clone(),
        options,
        args.magpie_settings.clone(),
    )?);
    let provider = Arc::new(provider::Provider {
        core: core.clone(),
        data: data.clone(),
        default_provider: provider_id,
        model,
        delay: args.controlled_delay,
        magpie: args.magpie_settings,
        federator: Some(ext.federator.clone()),
    });
    let stop = CancellationToken::new();
    let shutdown = signal()?;
    let saved = ext.identity.snapshot();
    let mut relay_targets = Vec::new();
    if saved.enabled {
        if !saved.host.relay.is_empty() {
            relay_targets.push((saved.host.relay.clone(), saved.registration_key.clone()));
        }
        for endpoint in &saved.relays {
            if !endpoint.url.is_empty() && !relay_targets.iter().any(|(url, _)| url == &endpoint.url) {
                let key = if endpoint.registration_key.is_empty() { &saved.registration_key } else { &endpoint.registration_key };
                relay_targets.push((endpoint.url.clone(), key.clone()));
            }
        }
    }
    let remote = async {
        let wait_for_pair = args.listen_remote.is_none() && !saved.enabled && saved.t3_listen().is_empty();
        if wait_for_pair {
            tokio::select! {
                _ = stop.cancelled() => return Ok::<(), anyhow::Error>(()),
                _ = ext.start_remote.notified() => {}
            }
        }
        let listener = if let Some(addr) = &args.listen_remote {
            tokio::net::TcpListener::bind(addr).await?
        } else {
            match tokio::net::TcpListener::bind(("0.0.0.0", saved.direct_port())).await {
                Ok(l) => l,
                Err(_) => tokio::net::TcpListener::bind("0.0.0.0:0").await?,
            }
        };
        let bound = listener.local_addr()?;
        let direct = if bound.ip().is_unspecified() {
            if_addrs::get_if_addrs()?.into_iter().map(|i| i.ip())
                .filter(|ip| !ip.is_loopback() && ip.is_ipv4() == bound.is_ipv4())
                .map(|ip| std::net::SocketAddr::new(ip, bound.port()).to_string()).collect()
        } else { vec![bound.to_string()] };
        ext.identity.remember_listener(direct, bound.port(), wait_for_pair)?;
        let t3 = vantry_connectivity::t3compat::T3Server::open(
            vantry_connectivity::t3compat::T3Options {
                data_dir: data.clone(),
                core_socket: data.join("core.sock"),
                environment_id: ext.environment.clone(),
                name: ext.identity.snapshot().host.name,
                version: env!("CARGO_PKG_VERSION").into(),
                provider: ext.options.provider_id.clone(),
                model: ext.options.model.clone(),
                identity: Some(ext.identity.clone()),
            },
        )?;
        let configured_t3 = saved.t3_listen();
        let (t3_listener, t3_addr) = t3
            .bind_configured((!configured_t3.is_empty()).then_some(configured_t3))
            .await?;
        let t3_stop = stop.clone();
        ext.mobile_server.send_replace(Some(t3.clone()));
        let t3_task = tokio::spawn(async move {
            let _ = t3.serve(t3_listener, t3_stop).await;
        });
        let mut gateway = listener.local_addr()?;
        if gateway.ip().is_unspecified() {
            gateway.set_ip(if gateway.is_ipv4() {
                std::net::Ipv4Addr::LOCALHOST.into()
            } else {
                std::net::Ipv6Addr::LOCALHOST.into()
            });
        }
        let relay_stop = stop.child_token();
        let mut hosts = tokio::task::JoinSet::new();
        for (relay, key) in relay_targets {
            let host = vantry_connectivity::peer::HostConfig {
                relay,
                key,
                gateway,
                route: saved.host.route.clone(),
                instance: saved.host.device_id.clone(),
                t3: Some(t3_addr),
            };
            let (status, _) = tokio::sync::watch::channel(vantry_connectivity::peer::HostState::Connecting);
            let owner = relay_stop.clone();
            hosts.spawn(async move {
                if host.run(status, owner).await.is_err() {
                    tracing::warn!("saved IDE relay could not start");
                }
            });
        }
        let res = vantry_connectivity::peer::serve_gateway_with_t3(
            listener,
            ext.identity.clone(),
            data.join("core.sock"),
            false,
            Some(t3_addr),
            stop.clone(),
        )
        .await;
        relay_stop.cancel();
        while hosts.join_next().await.is_some() {}
        t3_task.abort();
        let _ = t3_task.await;
        res
    };
    let warm_pi = provider.default_provider == "pi";
    let scheduler = core.run_scheduler(provider, stop.clone());
    let server = Server::bind_with_extension(core.clone(), ext.clone())
        .await?
        .serve(stop.clone());
    ext.alma.restore().await;
    let alma_task = if let (Some(relay), Some(thread)) = (&args.alma_relay, &args.alma_thread) {
        let host = alma::prepare(&data, relay, thread, args.alma_reset_pairing)?;
        let owner = stop.clone();
        Some(tokio::spawn(async move {
            if host.run(owner).await.is_err() {
                tracing::warn!("Alma mobile host stopped");
            }
        }))
    } else {
        None
    };
    qoder::spawn_background(data.clone(), stop.clone());
    if warm_pi {
        let stop = stop.clone();
        let settings = data.join("pi-runtime.json");
        tokio::spawn(async move {
            if let Ok(settings) = crate::settings::document(&settings) {
                let enabled = settings["enableUserMCP"].as_bool().unwrap_or(true);
                vantry_pi_runtime::mcp::warm_local_discovery(
                    &vantry_pi_runtime::agent_dir(),
                    enabled,
                    stop,
                )
                .await;
            }
        });
    }
    tokio::pin!(remote, scheduler, server);
    tokio::select! {
    biased;
     r=&mut scheduler=>{
    stop.cancel();
    r?;
    server.await?;
    remote.await?}
    ,r=&mut server=>{
    stop.cancel();
    r?;
    scheduler.await?;
    remote.await?}
    ,r=&mut remote=>{
    stop.cancel();
    r?;
    server.await?;
    scheduler.await?}
    ,_=shutdown=>{
    stop.cancel();
    server.await?;
    scheduler.await?;
    remote.await?}
    }

    ext.alma.shutdown().await;
    qoder::shutdown(&data).await;
    if let Some(task) = alma_task {
        task.await?;
    }
    Ok(())
}
async fn ensure_core(args: CoreArgs, raw: &[OsString]) -> Result<()> {
    let data = data_dir(args.data_dir.as_deref())?;
    let client = match args.socket.as_deref() {
        Some(socket) => CoreClient::new(socket),
        None => CoreClient::from_data_dir(&data),
    };
    let expected_version = env!("CARGO_PKG_VERSION");
    let ready = async || {
        tokio::time::timeout(Duration::from_secs(1), client.health())
            .await
            .ok()
            .and_then(Result::ok)
            .filter(|h| h.state == "ready")
    };
    if let Some(health) = ready().await {
        let status = tokio::time::timeout(Duration::from_secs(2), client.capabilities())
            .await
            .ok()
            .and_then(Result::ok);
        if status
            .as_ref()
            .is_some_and(|s| s.version == expected_version)
        {
            return ctl::print(&health);
        }
        let active_runs = status.as_ref().map_or(0, |s| s.active_runs);
        if active_runs > 0 {
            tracing::warn!(
                running_version = status.as_ref().map_or("", |s| s.version.as_str()),
                expected_version,
                active_runs,
                "existing Core version differs but has active runs; leaving it running"
            );
            return ctl::print(&health);
        }
        let _ = tokio::time::timeout(Duration::from_secs(2), client.shutdown_if_idle()).await;
        let stop_deadline = tokio::time::Instant::now() + Duration::from_secs(8);
        while tokio::time::Instant::now() < stop_deadline {
            let still_up = tokio::time::timeout(Duration::from_millis(250), client.health())
                .await
                .is_ok_and(|r| r.is_ok());
            if !still_up {
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        let lock_deadline = tokio::time::Instant::now() + Duration::from_secs(3);
        while tokio::time::Instant::now() < lock_deadline {
            if vantry_core::lock::InstanceLock::acquire(&data).is_ok() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }
    hostfs::ensure_private_dir(&data)?;
    let mut cmd = tokio::process::Command::new(std::env::current_exe()?);
    cmd.arg("core")
        .args(raw)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    detach_tokio_command(&mut cmd);
    let mut child = cmd.spawn()?;
    let deadline = tokio::time::Instant::now() + Duration::from_secs(15);
    loop {
        if let Some(health) = ready().await {
            return ctl::print(&health);
        }
        if let Some(status) = child.try_wait()? {
            // Concurrent ensure-core callers can race for Core's instance lock.
            if let Some(health) = ready().await {
                return ctl::print(&health);
            }
            if let Ok(_available) = vantry_core::lock::InstanceLock::acquire(&data) {
                anyhow::bail!("Core exited with {status} before becoming ready");
            }
        }
        if tokio::time::Instant::now() >= deadline {
            if child.try_wait()?.is_none() {
                child.kill().await?;
            }
            anyhow::bail!("Core did not become ready in {}", data.display());
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}
fn arguments() -> Vec<OsString> {
    let mut value = false;
    std::env::args_os()
        .enumerate()
        .map(|(i, arg)| {
            if i == 0 || std::mem::take(&mut value) {
                return arg;
            }
            let Some(s) = arg.to_str() else { return arg };
            let Some(key) = s.strip_prefix('-').map(|s| s.trim_start_matches('-')) else {
                return arg;
            };
            if key.is_empty() || key == "h" || key == "V" {
                return arg;
            }
            value = !key.contains('=')
                && !matches!(
                    key,
                    "settled" | "force" | "confirm" | "cancel" | "help" | "version"
                );
            format!("--{key}").into()
        })
        .collect()
}
#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .init();
    let raw = arguments();
    match Cli::parse_from(&raw).command {
        Role::ModelPluginHost(args) => qoder::host::run(args).await?,
        Role::Core(a) => serve(a).await?,
        Role::EnsureCore(a) => ensure_core(a, &raw[2..]).await?,
        Role::Ctl(c) => c.run().await?,
        Role::Relay(a) => relay::run(a).await?,
        Role::ProviderWorker(j) => vantry_pi_runtime::run_worker(&j.job_dir).await?,
        Role::WorkerStop(j) => vantry_pi_runtime::request_stop(&j.job_dir)?,
        Role::WorkerControl(j) => vantry_pi_runtime::deliver_control(
            &j.job_dir,
            &serde_json::from_reader(std::io::stdin().lock())?,
        )?,
        Role::SmokeWorker => {
            let dir =
                vantry_pi_runtime::prepare_job(&serde_json::from_reader(std::io::stdin().lock())?)?;
            vantry_pi_runtime::run_worker(&dir).await?
        }
        Role::PiSetup => vantry_pi_runtime::save_setup(
            &vantry_pi_runtime::agent_dir(),
            &serde_json::from_reader(std::io::stdin().lock())?,
        )?,
        Role::ApplyUpdate { args } => {
            vantry_updater::run_apply(&args, std::io::stdout()).context("apply-update")?
        }
        Role::PeerProxy(p) => peer_proxy(p).await?,
    }
    Ok(())
}

async fn peer_proxy(args: PeerProxyArgs) -> Result<()> {
    let data = data_dir(args.data_dir.as_deref())?;
    let store = vantry_connectivity::identity::IdentityStore::open(&data, "Vantry")?;
    let cfg = store.snapshot();
    let peer = cfg
        .peers
        .iter()
        .find(|p| p.id == args.peer || p.pair.name == args.peer)
        .cloned()
        .context("unknown paired peer")?;
    let route = match args.mode.as_str() {
        "direct" => vantry_connectivity::peer::Route::Direct,
        "relay" => vantry_connectivity::peer::Route::Relay,
        _ => vantry_connectivity::peer::Route::Auto,
    };
    if let Some(parent) = args.socket.parent() {
        hostfs::ensure_private_dir(parent)?;
    }
    let stop = CancellationToken::new();
    let shutdown = signal()?;
    let proxy_stop = stop.clone();
    let proxy = vantry_connectivity::peer::serve_peer_proxy(
        args.socket,
        cfg,
        peer.pair,
        route,
        proxy_stop,
    );
    tokio::pin!(proxy, shutdown);
    tokio::select! {
        r = &mut proxy => r?,
        _ = &mut shutdown => {
            stop.cancel();
            proxy.await?;
        }
    }
    Ok(())
}

#[cfg(test)]
#[test]
fn accepts_peer_ids_starting_with_a_dash() {
    let peer = "-fixture_peer";
    assert!(
        Cli::try_parse_from([
            "vantry-service",
            "ctl",
            "workbench",
            "remote.peers.revoke",
            "--name",
            peer
        ])
        .is_ok()
    );
    assert!(
        Cli::try_parse_from([
            "vantry-service",
            "peer-proxy",
            "--peer",
            peer,
            "--socket",
            "peer.sock"
        ])
        .is_ok()
    );
}
