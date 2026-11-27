use anyhow::{Result, anyhow, ensure};
use futures::{StreamExt, stream::FuturesUnordered};
use serde::{Deserialize, Serialize};
use std::{io::ErrorKind, net::SocketAddr, path::Path, sync::Arc};
use url::Url;
use vantry_connectivity::{
    alma::{Activity, Host, desktop_client_url, new_code},
    transport::relay_url,
};
use vantry_platform::hostfs;

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct RelaySettings {
    relay: String,
    #[serde(default)]
    registration_key: Option<String>,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Settings {
    #[serde(flatten)]
    primary: RelaySettings,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    fallbacks: Vec<RelaySettings>,
    #[serde(default)]
    code: String,
}

struct Endpoint {
    relay: Url,
    registration_key: Option<String>,
    client_url: Url,
    activity: Arc<Activity>,
}

pub struct DesktopRelays {
    endpoints: Vec<Endpoint>,
    code: String,
}

impl DesktopRelays {
    pub fn public_relay(&self) -> &str {
        self.endpoints
            .iter()
            .find(|e| e.activity.relay_online())
            .unwrap_or(&self.endpoints[0])
            .relay
            .as_str()
    }

    pub fn client_urls(&self) -> Vec<String> {
        self.endpoints
            .iter()
            .map(|e| e.client_url.to_string())
            .collect()
    }

    pub fn online(&self) -> bool {
        self.endpoints.iter().any(|e| e.activity.relay_online())
    }

    pub fn status(&self) -> Vec<serde_json::Value> {
        self.endpoints.iter().map(|e| {
            serde_json::json!({"relay":e.relay.as_str(),"connected":e.activity.relay_online()})
        }).collect()
    }

    pub async fn wait_for_registration(&self) -> Result<()> {
        let mut pending: FuturesUnordered<_> = self
            .endpoints
            .iter()
            .map(|e| e.activity.wait_for_registration())
            .collect();
        let mut last_error = None;
        while let Some(result) = pending.next().await {
            match result {
                Ok(()) => return Ok(()),
                Err(e) => last_error = Some(e),
            }
        }
        Err(last_error.unwrap_or_else(|| anyhow!("未配置电脑中继")))
    }

    pub fn load(data: &Path) -> Result<Option<Self>> {
        let path = Path::new("desktop-alma-relay.json");
        let bytes = match hostfs::read_file(data, path, 16 * 1024) {
            Ok(bytes) => bytes,
            Err(e) if e.kind() == ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e.into()),
        };
        let mut settings: Settings = serde_json::from_slice(&bytes)?;
        let new_route = settings.code.is_empty();
        if new_route {
            settings.code = new_code()?;
        }
        let mut endpoints = Vec::<Endpoint>::new();
        for entry in std::iter::once(&settings.primary).chain(&settings.fallbacks) {
            let relay = relay_url(&entry.relay, false)?;
            let client_url = desktop_client_url(&relay, &settings.code)?;
            if endpoints.iter().any(|e| e.client_url == client_url) {
                continue;
            }
            endpoints.push(Endpoint {
                relay,
                client_url,
                registration_key: entry.registration_key.clone(),
                activity: Arc::default(),
            });
        }
        ensure!(endpoints.len() <= 8, "最多支持 8 个电脑中继");
        if new_route {
            hostfs::atomic_write(&data.join(path), &serde_json::to_vec(&settings)?)?;
        }
        Ok(Some(Self {
            endpoints,
            code: settings.code,
        }))
    }

    pub fn hosts(&self, route: &str, mut gateway: SocketAddr) -> Result<Vec<Host>> {
        if gateway.ip().is_unspecified() {
            gateway.set_ip(if gateway.is_ipv4() {
                std::net::Ipv4Addr::LOCALHOST.into()
            } else {
                std::net::Ipv6Addr::LOCALHOST.into()
            });
        }
        self.endpoints
            .iter()
            .map(|e| {
                Host::desktop(
                    e.relay.clone(),
                    self.code.clone(),
                    e.registration_key.clone(),
                    route,
                    gateway,
                    e.activity.clone(),
                )
            })
            .collect()
    }
}
