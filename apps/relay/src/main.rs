mod yaml;
use anyhow::{Result, anyhow, ensure};
use clap::{Parser, Subcommand};
use serde::{Deserialize, Serialize};
use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    net::IpAddr,
    path::{Path, PathBuf},
    sync::Arc,
};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    TlsFiles,
    pairing::token,
    relay::{Relay, SessionConfig},
};
#[derive(Parser)]
#[command(
    name = "vantry-relay",
    version,
    about = "Vantry standalone session and desktop relay"
)]
struct Cli {
    #[arg(long, global = true)]
    config: Option<PathBuf>,
    #[command(subcommand)]
    command: Option<Command>,
}
#[derive(Subcommand)]
enum Command {
    Serve,
    #[command(alias = "validate", alias = "check")]
    CheckConfig,
    Version,
    Init {
        #[arg(long, default_value = "127.0.0.1:8787")]
        listen: String,
        #[arg(long, default_value = "")]
        certificate: String,
        #[arg(long, default_value = "")]
        private_key: String,
        #[arg(long)]
        desktop: bool,
    },
}
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct DesktopConfig {
    registration_key: String,
}
#[derive(Default, Deserialize, Serialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Config {
    listen: String,
    #[serde(rename = "allowInsecurePrivateHTTP")]
    allow_private: bool,
    certificate: String,
    private_key: String,
    acme_hosts: Vec<String>,
    acme_email: String,
    certificate_directory: String,
    sessions: Option<SessionConfig>,
    desktop: Option<DesktopConfig>,
}
impl Config {
    fn read(path: &Path) -> Result<Self> {
        let mut bytes = vec![];
        File::open(path)?.take(65537).read_to_end(&mut bytes)?;
        ensure!(bytes.len() <= 65536, "relay configuration exceeds 64 KiB");
        let text = std::str::from_utf8(&bytes)
            .map_err(|_| anyhow!("invalid configuration encoding"))?
            .trim();
        let mut cfg: Config = if text.starts_with('{') {
            serde_json::from_str(text)
        } else {
            let value =
                yaml::parse(text).map_err(|_| anyhow!("invalid relay YAML configuration"))?;
            serde_json::from_value(value)
        }
        .map_err(|_| anyhow!("invalid relay configuration fields"))?;
        if let Ok(v) = std::env::var("VANTRY_RELAY_LISTEN") {
            cfg.listen = v
        }
        if std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1") {
            cfg.allow_private = true
        }
        if let (Some(c), Ok(k)) = (
            &mut cfg.sessions,
            std::env::var("VANTRY_RELAY_REGISTRATION_KEY"),
        ) {
            c.registration_key = k
        }
        if let (Some(c), Ok(k)) = (
            &mut cfg.desktop,
            std::env::var("VANTRY_DESKTOP_RELAY_REGISTRATION_KEY"),
        ) {
            c.registration_key = k
        }
        Ok(cfg)
    }
    fn address(&self) -> String {
        if self.listen.is_empty() {
            "127.0.0.1:8787".into()
        } else if self.listen.starts_with(':') {
            format!("0.0.0.0{}", self.listen)
        } else {
            self.listen.clone()
        }
    }
    fn tls(&self) -> Result<Option<TlsFiles>> {
        if !self.certificate.is_empty() || !self.private_key.is_empty() {
            ensure!(
                !self.certificate.is_empty() && !self.private_key.is_empty(),
                "certificate and privateKey must be configured together"
            );
            let f = TlsFiles {
                certificate: self.certificate.clone().into(),
                private_key: self.private_key.clone().into(),
            };
            f.load()
                .map_err(|_| anyhow!("cannot load TLS certificate and key"))?;
            return Ok(Some(f));
        }
        if !self.acme_hosts.is_empty() {
            return Ok(None);
        }
        let addr = self.address();
        let host = addr
            .rsplit_once(':')
            .ok_or_else(|| anyhow!("invalid listen address"))?
            .0
            .trim_matches(['[', ']']);
        ensure!(
            host == "localhost"
                || host.parse::<IpAddr>().is_ok_and(|ip| ip.is_loopback()
                    || self.allow_private
                        && match ip {
                            IpAddr::V4(v) => v.is_private(),
                            IpAddr::V6(v) => v.is_unique_local(),
                        }),
            "TLS required on non-loopback listeners"
        );
        Ok(None)
    }
}
fn shutdown() -> Result<impl Future<Output = Result<()>>> {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let mut term = signal(SignalKind::terminate())?;
        let mut int = signal(SignalKind::interrupt())?;
        Ok(async move {
            tokio::select! { _ = term.recv() => {}, _ = int.recv() => {} }
            Ok(())
        })
    }
    #[cfg(not(unix))]
    {
        Ok(async {
            tokio::signal::ctrl_c().await?;
            Ok(())
        })
    }
}
async fn run(cli: Cli) -> Result<()> {
    if matches!(cli.command, Some(Command::Version)) {
        println!("vantry-relay {}", env!("CARGO_PKG_VERSION"));
        return Ok(());
    }
    let path = cli
        .config
        .or_else(|| std::env::var_os("VANTRY_RELAY_CONFIG").map(PathBuf::from))
        .ok_or_else(|| anyhow!("--config is required"))?;
    if let Some(Command::Init {
        listen,
        certificate,
        private_key,
        desktop,
    }) = cli.command
    {
        let cfg = Config {
            listen,
            certificate,
            private_key,
            sessions: Some(SessionConfig {
                registration_key: token(),
                ..Default::default()
            }),
            desktop: desktop.then(|| DesktopConfig {
                registration_key: token(),
            }),
            ..Default::default()
        };
        let bytes = serde_json::to_vec_pretty(&cfg)?;
        let mut opt = OpenOptions::new();
        opt.create_new(true).write(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            opt.mode(0o600);
        }
        let mut f = opt.open(&path)?;
        f.write_all(&bytes)?;
        f.write_all(b"\n")?;
        f.sync_all()?;
        println!("Created relay configuration {}", path.display());
        return Ok(());
    }
    let cfg = Config::read(&path)?;
    let addr = cfg.address();
    let (host, port) = addr
        .rsplit_once(':')
        .ok_or_else(|| anyhow!("invalid listen address"))?;
    ensure!(
        !host.is_empty()
            && port.parse::<u16>().is_ok()
            && (!host.contains(':') || host.starts_with('[') && host.ends_with(']')),
        "invalid listen address"
    );
    let tls = cfg.tls()?;
    let manager = if tls.is_none() && !cfg.acme_hosts.is_empty() {
        Some(Arc::new(vantry_connectivity::acme::Acme::new(
            cfg.certificate_directory.into(),
            &cfg.acme_hosts,
            cfg.acme_email,
        )?))
    } else {
        None
    };
    let sessions = cfg.sessions.is_some();
    let desktop = cfg.desktop.is_some();
    let relay = Relay::new(cfg.sessions, cfg.desktop.map(|d| d.registration_key))?;
    if matches!(cli.command, Some(Command::CheckConfig)) {
        println!("Configuration valid");
        return Ok(());
    }
    let listener = TcpListener::bind(addr).await?;
    let stop = CancellationToken::new();
    let signal = shutdown()?;
    eprintln!(
        "Vantry relay listening on {} (sessions={sessions} desktop={desktop} TLS={})",
        listener.local_addr()?,
        tls.is_some() || manager.is_some()
    );
    let service = async {
        if let Some(m) = manager {
            vantry_connectivity::serve_acme(listener, Arc::new(relay), m, stop.clone()).await
        } else {
            vantry_connectivity::serve(listener, Arc::new(relay), tls, stop.clone()).await
        }
    };
    tokio::pin!(service);
    tokio::select! {
        result = &mut service => result,
        result = signal => {stop.cancel(); service.await?; result}
    }
}
#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    tracing_subscriber::fmt()
        .with_max_level(tracing::Level::WARN)
        .with_writer(std::io::stderr)
        .init();
    if let Err(e) = run(Cli::parse()).await {
        eprintln!("vantry-relay: {e}");
        std::process::exit(1)
    }
}
