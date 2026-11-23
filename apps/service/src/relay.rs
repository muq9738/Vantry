use anyhow::{Context, Result, ensure};
use serde::Deserialize;
use std::{io::Read, path::PathBuf, sync::Arc};
use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    TlsFiles,
    relay::{Relay, SessionConfig},
};
#[path = "../../relay/src/yaml.rs"]
mod yaml;
#[derive(clap::Args)]
pub struct Args {
    #[arg(value_parser=["serve"])]
    command: Option<String>,
    #[arg(long)]
    config: Option<PathBuf>,
    #[arg(long)]
    listen: Option<String>,
}
#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Config {
    listen: String,
    sessions: Option<SessionConfig>,
    desktop: Option<Desktop>,
    certificate: PathBuf,
    private_key: PathBuf,
    acme_hosts: Vec<String>,
    acme_email: String,
    certificate_directory: PathBuf,
    #[serde(rename = "allowInsecurePrivateHTTP")]
    allow_private: bool,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Desktop {
    registration_key: String,
}
pub async fn run(args: Args) -> Result<()> {
    let config = args
        .config
        .or_else(|| std::env::var_os("VANTRY_RELAY_CONFIG").map(Into::into));
    let cfg = if let Some(p) = config {
        let mut body = String::new();
        std::fs::File::open(p)?
            .take(65537)
            .read_to_string(&mut body)?;
        ensure!(body.len() <= 65536, "configuration exceeds 64 KiB");
        if body.trim_start().starts_with('{') {
            serde_json::from_str(&body)?
        } else {
            serde_json::from_value(yaml::parse(&body)?)?
        }
    } else {
        Config {
            sessions: Some(SessionConfig {
                registration_key: std::env::var("VANTRY_RELAY_REGISTRATION_KEY")
                    .context("relay requires --config or VANTRY_RELAY_REGISTRATION_KEY")?,
                ..Default::default()
            }),
            ..Default::default()
        }
    };
    let mut addr = args
        .listen
        .or_else(|| std::env::var("VANTRY_RELAY_LISTEN").ok())
        .unwrap_or(cfg.listen);
    if addr.is_empty() {
        addr = "127.0.0.1:8787".into()
    } else if addr.starts_with(':') {
        addr = format!("0.0.0.0{addr}")
    }
    let listener = TcpListener::bind(addr).await?;
    let ip = listener.local_addr()?.ip();
    let tls = if cfg.certificate.as_os_str().is_empty() && cfg.private_key.as_os_str().is_empty() {
        None
    } else {
        let f = TlsFiles {
            certificate: cfg.certificate,
            private_key: cfg.private_key,
        };
        f.load()?;
        Some(f)
    };
    ensure!(
        tls.is_some()
            || !cfg.acme_hosts.is_empty()
            || ip.is_loopback()
            || cfg.allow_private
                && match ip {
                    std::net::IpAddr::V4(v) => v.is_private(),
                    std::net::IpAddr::V6(v) => v.is_unique_local(),
                },
        "TLS required for relay address"
    );
    let relay = Arc::new(Relay::new(
        cfg.sessions,
        cfg.desktop.map(|d| d.registration_key),
    )?);
    let stop = CancellationToken::new();
    let shutdown = crate::signal()?;
    let serving = async {
        if tls.is_none() && !cfg.acme_hosts.is_empty() {
            let manager = Arc::new(vantry_connectivity::acme::Acme::new(
                cfg.certificate_directory,
                &cfg.acme_hosts,
                cfg.acme_email,
            )?);
            vantry_connectivity::serve_acme(listener, relay, manager, stop.clone()).await
        } else {
            vantry_connectivity::serve(listener, relay, tls, stop.clone()).await
        }
    };
    tokio::pin!(serving);
    tokio::select! {r=&mut serving=>r,_=shutdown=>{stop.cancel();serving.await}}
}
