mod ctl;
mod image;
mod mcp_probe;
mod orchestration;
mod plugin_source;
mod provider;
mod relay;
mod settings;
mod usage;
mod workbench;
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
    PiSetup,
    ApplyUpdate {
        #[arg(trailing_var_arg = true, allow_hyphen_values = true)]
        args: Vec<String>,
    },
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
fn signal() -> Result<impl Future<Output = ()>> {
    use tokio::signal::unix::{SignalKind, signal};
    let mut term = signal(SignalKind::terminate())?;
    let mut int = signal(SignalKind::interrupt())?;
    Ok(async move {
        tokio::select! {_=term.recv()=>{},_=int.recv()=>{}}
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
        data: data.clone(),
        default_provider: provider_id,
        model,
        delay: args.controlled_delay,
        magpie: args.magpie_settings,
    });
    let stop = CancellationToken::new();
    let shutdown = signal()?;
    let listener = if let Some(addr) = args.listen_remote {
        let listener = tokio::net::TcpListener::bind(addr).await?;
        ext.identity.set_routes(
            vec![listener.local_addr()?.to_string()],
            ext.identity.snapshot().host.relays,
        )?;
        Some(listener)
    } else {
        None
    };
    let remote = async {
        if let Some(listener) = listener {
            vantry_connectivity::peer::serve_gateway(
                listener,
                ext.identity.clone(),
                data.join("core.sock"),
                false,
                stop.clone(),
            )
            .await
        } else {
            stop.cancelled().await;
            Ok(())
        }
    };
    let scheduler = core.run_scheduler(provider, stop.clone());
    let server = Server::bind_with_extension(core.clone(), ext.clone())
        .await?
        .serve(stop.clone());
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

    Ok(())
}
async fn ensure_core(args: CoreArgs, raw: &[OsString]) -> Result<()> {
    let data = data_dir(args.data_dir.as_deref())?;
    let client = match args.socket.as_deref() {
        Some(socket) => CoreClient::new(socket),
        None => CoreClient::from_data_dir(&data),
    };
    let ready = async || {
        tokio::time::timeout(Duration::from_secs(1), client.health())
            .await
            .ok()
            .and_then(Result::ok)
            .filter(|h| h.state == "ready")
    };
    if let Some(health) = ready().await {
        return ctl::print(&health);
    }
    hostfs::ensure_private_dir(&data)?;
    let mut child = tokio::process::Command::new(std::env::current_exe()?)
        .arg("core")
        .args(raw)
        .process_group(0)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()?;
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
    }
    Ok(())
}
