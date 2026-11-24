//! Public bootstrap exchange. Device bearer credentials are issued only over the inner TLS connection.
use crate::{
    identity::{IdentityStore, Pair},
    pairing::Pairing,
    peer::{self, Route},
    transport::{http_client, relay_url},
};
use anyhow::{Result, anyhow, ensure};
use serde_json::{Value, json};
use std::{sync::Arc, time::Duration};
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;
use url::Url;
fn private_http() -> bool {
    std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1")
}
async fn post(
    client: &reqwest::Client,
    base: &Url,
    path: &str,
    bearer: Option<&str>,
    body: Value,
) -> Result<reqwest::Response> {
    let mut url = base.clone();
    url.set_path(&format!("{}{}", base.path().trim_end_matches('/'), path));
    let mut req = client.post(url).timeout(Duration::from_secs(5)).json(&body);
    if let Some(secret) = bearer {
        req = req.bearer_auth(secret)
    }
    let res = req
        .send()
        .await
        .map_err(|_| anyhow!("pairing relay unavailable"))?;
    ensure!(
        res.status().is_success(),
        "pairing relay rejected request (HTTP {})",
        res.status()
    );
    Ok(res)
}
async fn result(mut res: reqwest::Response) -> Result<Pairing> {
    let mut data = Vec::new();
    while let Some(chunk) = res
        .chunk()
        .await
        .map_err(|_| anyhow!("pairing response interrupted"))?
    {
        ensure!(
            data.len() + chunk.len() <= 32 << 10,
            "pairing response too large"
        );
        data.extend(chunk);
    }
    serde_json::from_slice(&data).map_err(|_| anyhow!("invalid relay pairing response"))
}
struct Registration {
    base: Url,
    key: String,
    id: String,
}
pub struct Publisher {
    identity: Arc<IdentityStore>,
    client: reqwest::Client,
    registration: Mutex<Option<Registration>>,
}
impl Publisher {
    pub fn new(identity: Arc<IdentityStore>) -> Result<Self> {
        Ok(Self {
            identity,
            client: http_client()?,
            registration: Mutex::new(None),
        })
    }
    async fn revoke_remote(&self, r: &Registration) -> Result<()> {
        post(
            &self.client,
            &r.base,
            "/pairings/revoke",
            Some(&r.key),
            json!({"id":r.id}),
        )
        .await?;
        Ok(())
    }
    pub async fn revoke(&self) -> Result<()> {
        let mut g = self.registration.lock().await;
        self.identity.revoke_code();
        if let Some(r) = g.as_ref() {
            self.revoke_remote(r).await?;
        }
        *g = None;
        Ok(())
    }
    pub async fn issue(&self, key: &str) -> Result<Pair> {
        let mut g = self.registration.lock().await;
        let local = self.identity.issue()?;
        if let Some(r) = g.as_ref() {
            if self.revoke_remote(r).await.is_ok() {
                *g = None;
            }
        }
        if local.relay.is_empty() {
            return Ok(local);
        }
        let base = relay_url(&local.relay, private_http())?;
        let mut public = local.clone();
        public.code.clear();
        public.expires_at = None;
        let response=post(&self.client,&base,"/pairings",Some(key),json!({"scope":"session","name":public.name,"expiresAt":local.expires_at,"payload":public})).await;
        let response = match response {
            Ok(r) => r,
            Err(_) if !local.direct.is_empty() => return Ok(local),
            Err(e) => return Err(e),
        };
        let entry = result(response).await?;
        let registration = Registration {
            base,
            key: key.into(),
            id: entry.id.clone(),
        };
        let paired = self.identity.use_relay_code(&local, entry);
        if paired.is_err() {
            self.revoke_remote(&registration).await?;
        } else {
            *g = Some(registration)
        }
        paired
    }
}
pub async fn redeem_code(
    relay: &str,
    code: &str,
    local: &IdentityStore,
    name: &str,
    stop: CancellationToken,
) -> Result<String> {
    let base = relay_url(relay, private_http())?;
    let entry = result(
        post(
            &http_client()?,
            &base,
            "/pairings/redeem",
            None,
            json!({"scope":"session","code":code,"name":name}),
        )
        .await?,
    )
    .await?;
    ensure!(entry.scope == "session", "wrong pairing scope");
    let mut pair: Pair =
        serde_json::from_value(entry.payload.ok_or_else(|| anyhow!("missing bootstrap"))?)
            .map_err(|_| anyhow!("invalid bootstrap"))?;
    if pair.v == 1 {
        return local.save_peer(pair);
    }
    ensure!(pair.v == 2 && pair.secret.is_empty(), "invalid bootstrap");
    pair.code = code.trim().to_ascii_uppercase();
    pair.expires_at = Some(entry.expires_at);
    if pair.relay.is_empty() {
        pair.relay = relay.into()
    }
    peer::redeem(&pair, local, name, Route::Auto, stop).await
}
