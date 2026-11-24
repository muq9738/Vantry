//! Client for the 0.1.42 desktop sharing contract and loopback ICE/TURN endpoint.
use crate::{
    pairing::Pairing,
    transport::{self, Tasks, Ws, http_client, relay_url, websocket},
};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Utc};
use reqwest::Method;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{net::SocketAddr, sync::Arc, time::Duration};
use tokio::{net::TcpListener, sync::Semaphore};
use tokio_util::sync::CancellationToken;
use url::Url;
#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Ticket {
    pub relay: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub code: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub session_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub token: String,
}
impl Ticket {
    pub fn link(&self) -> String {
        format!(
            "vantry://desktop#{}",
            URL_SAFE_NO_PAD.encode(serde_json::to_vec(self).unwrap())
        )
    }
    pub fn parse(s: &str) -> Result<Self> {
        let s = s
            .trim()
            .strip_prefix("vantry://desktop#")
            .ok_or_else(|| anyhow!("invalid desktop sharing link"))?;
        ensure!(s.len() <= 8192, "sharing link too long");
        let t: Self = serde_json::from_slice(
            &URL_SAFE_NO_PAD
                .decode(s)
                .map_err(|_| anyhow!("invalid sharing link"))?,
        )
        .map_err(|_| anyhow!("invalid sharing link"))?;
        base(&t.relay)?;
        if t.code.is_empty() {
            valid_id(&t.session_id)?;
            ensure!(t.token.len() >= 32, "invalid viewer ticket")
        } else {
            ensure!(
                t.code.starts_with("DESK-") && t.expires_at.is_some_and(|t| t > Utc::now()),
                "invalid or expired sharing code"
            )
        }
        Ok(t)
    }
}
fn base(s: &str) -> Result<Url> {
    relay_url(
        s,
        std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1"),
    )
}
fn valid_id(id: &str) -> Result<()> {
    ensure!(
        !id.is_empty()
            && id.len() <= 256
            && id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-'),
        "invalid desktop session ID"
    );
    Ok(())
}
#[derive(Clone)]
pub struct DesktopClient {
    base: Url,
    client: reqwest::Client,
}
impl DesktopClient {
    pub fn with_trust_anchor(relay: &str, root: reqwest::Certificate) -> Result<Self> {
        Ok(Self {
            base: base(relay)?,
            client: reqwest::Client::builder()
                .http1_only()
                .redirect(reqwest::redirect::Policy::none())
                .connect_timeout(Duration::from_secs(10))
                .add_root_certificate(root)
                .build()?,
        })
    }
    pub fn new(relay: &str) -> Result<Self> {
        Ok(Self {
            base: base(relay)?,
            client: http_client()?,
        })
    }
    async fn request(
        &self,
        method: Method,
        path: &str,
        key: Option<&str>,
        data: Option<Value>,
    ) -> Result<Value> {
        let mut url = self.base.clone();
        url.set_path(&format!(
            "{}/desktop{}",
            self.base.path().trim_end_matches('/'),
            path
        ));
        let mut req = self
            .client
            .request(method, url)
            .timeout(Duration::from_secs(20));
        if let Some(k) = key {
            req = req.bearer_auth(k)
        }
        if let Some(v) = data {
            req = req.json(&v)
        }
        let mut res = req
            .send()
            .await
            .map_err(|_| anyhow!("desktop relay unavailable"))?;
        ensure!(
            res.status().is_success(),
            "desktop relay rejected request (HTTP {})",
            res.status()
        );
        if res.status() == 204 {
            return Ok(Value::Null);
        }
        let mut buf = vec![];
        while let Some(chunk) = res
            .chunk()
            .await
            .map_err(|_| anyhow!("desktop response interrupted"))?
        {
            ensure!(
                buf.len() + chunk.len() <= 1 << 20,
                "desktop response too large"
            );
            buf.extend(chunk)
        }
        Ok(serde_json::from_slice(&buf).map_err(|_| anyhow!("invalid desktop response"))?)
    }
    pub async fn create(&self, key: &str, video: Value) -> Result<Value> {
        self.request(
            Method::POST,
            "/v1/sessions",
            Some(key),
            Some(json!({"video":video})),
        )
        .await
    }
    pub async fn invite(
        &self,
        key: &str,
        id: &str,
        viewer_token: &str,
        name: &str,
    ) -> Result<Pairing> {
        valid_id(id)?;
        let payload = json!({"relay":self.base.as_str(),"sessionId":id,"token":viewer_token});
        let v=self.request(Method::POST,"/pairings",Some(key),Some(json!({"scope":"desktop","name":name,"payload":payload,"expiresAt":Utc::now()+chrono::Duration::minutes(5)}))).await?;
        Ok(serde_json::from_value(v)?)
    }
    pub async fn revoke_code(&self, key: &str, id: &str) -> Result<bool> {
        Ok(self
            .request(
                Method::POST,
                "/pairings/revoke",
                Some(key),
                Some(json!({"id":id})),
            )
            .await?["revoked"]
            == true)
    }
    pub async fn redeem(&self, code: &str) -> Result<Ticket> {
        let v = self
            .request(
                Method::POST,
                "/pairings/redeem",
                None,
                Some(json!({"scope":"desktop","code":code})),
            )
            .await?;
        let t: Ticket = serde_json::from_value(v["payload"].clone())
            .map_err(|_| anyhow!("invalid desktop ticket"))?;
        Ticket::parse(&t.link())
    }
    pub async fn request_access(&self, id: &str, token: &str, label: &str) -> Result<()> {
        valid_id(id)?;
        self.request(
            Method::POST,
            &format!("/v1/sessions/{id}/request"),
            Some(token),
            Some(json!({"label":label})),
        )
        .await?;
        Ok(())
    }
    pub async fn approve(&self, id: &str, host_token: &str, allow: bool) -> Result<()> {
        valid_id(id)?;
        self.request(
            Method::POST,
            &format!("/v1/sessions/{id}/approve"),
            Some(host_token),
            Some(json!({"allow":allow})),
        )
        .await?;
        Ok(())
    }
    pub async fn status(&self, id: &str, token: &str) -> Result<Value> {
        valid_id(id)?;
        self.request(
            Method::GET,
            &format!("/v1/sessions/{id}"),
            Some(token),
            None,
        )
        .await
    }
    pub async fn delete(&self, id: &str, token: &str) -> Result<()> {
        valid_id(id)?;
        self.request(
            Method::DELETE,
            &format!("/v1/sessions/{id}"),
            Some(token),
            None,
        )
        .await?;
        Ok(())
    }
    pub async fn signal(&self, id: &str, role: &str, token: &str) -> Result<Ws> {
        valid_id(id)?;
        ensure!(matches!(role, "host" | "viewer"), "invalid role");
        websocket(
            &self.client,
            &self.base,
            &format!("/desktop/v1/sessions/{id}/signal"),
            &[("role", role)],
            Some(token),
            64 << 10,
        )
        .await
    }
    pub async fn turn_bridge(
        &self,
        session: &mut Value,
        token: &str,
        stop: CancellationToken,
    ) -> Result<TurnBridge> {
        let id = session["sessionId"]
            .as_str()
            .ok_or_else(|| anyhow!("missing session ID"))?;
        valid_id(id)?;
        let path = format!("/desktop/v1/sessions/{id}/turn");
        ensure!(
            session["transport"] == "wss-tcp" && session["iceTransportPolicy"] == "relay",
            "unsupported ICE transport"
        );
        let ice = session["iceServers"]
            .as_array_mut()
            .filter(|v| !v.is_empty())
            .ok_or_else(|| anyhow!("no TURN server"))?;
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let address = listener.local_addr()?;
        for server in ice {
            server["urls"] = json!([format!("turn:{address}?transport=tcp")]);
        }
        let stop = stop.child_token();
        let tasks = Tasks::new(stop.clone());
        let group = tasks.clone();
        let relay = self.clone();
        let token = token.to_owned();
        let slots = Arc::new(Semaphore::new(128));
        tasks.spawn(async move {
            while let Ok((socket, _)) = listener.accept().await {
                let Ok(permit) = slots.clone().try_acquire_owned() else {
                    continue;
                };
                let (relay, path, token) = (relay.clone(), path.clone(), token.clone());
                group.spawn(async move {
                    let _permit = permit;
                    if let Ok(ws) = websocket(
                        &relay.client,
                        &relay.base,
                        &path,
                        &[],
                        Some(&token),
                        1 << 20,
                    )
                    .await
                    {
                        let _ = transport::bridge(ws, socket).await;
                    }
                });
            }
        });
        Ok(TurnBridge { address, tasks })
    }
}
pub struct TurnBridge {
    pub address: SocketAddr,
    tasks: Tasks,
}
impl TurnBridge {
    pub async fn close(self) {
        self.tasks.close().await;
    }
}
impl Drop for TurnBridge {
    fn drop(&mut self) {
        self.tasks.stop.cancel()
    }
}
