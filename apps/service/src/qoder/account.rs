//! Persist credentials in the format used by existing installations. A callback
//! from an earlier login cannot replace a newer account's refreshed credential.
use super::plugin::Plugin;
use anyhow::{Context, Result, ensure};
use pi_js::{HostFunction, HostValue, JsError};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    fs,
    path::PathBuf,
    sync::{Arc, Mutex, MutexGuard},
    time::Duration,
};
use vantry_platform::hostfs;

#[derive(Default, Serialize, Deserialize)]
#[serde(default)]
struct Credential {
    generation: u64,
    auth: Value,
}
#[derive(Default)]
struct Status {
    credential: Credential,
    login: Value,
    models: Vec<Value>,
    models_at: String,
    models_error: String,
    usage: Value,
    usage_at: String,
    usage_error: String,
    expired: bool,
}
#[derive(Clone)]
pub struct Account {
    status: Arc<Mutex<Status>>,
    file: PathBuf,
    refresh: Arc<tokio::sync::Mutex<()>>,
    login: Arc<tokio::sync::Mutex<()>>,
}
impl Account {
    pub fn open(file: PathBuf, empty: bool) -> Result<Self> {
        let credential = if empty {
            Credential::default()
        } else {
            match fs::read(&file) {
                Ok(bytes) => {
                    serde_json::from_slice(&bytes).context("invalid Qoder credential file")?
                }
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => Credential::default(),
                Err(e) => return Err(e.into()),
            }
        };
        Ok(Self {
            status: Arc::new(Mutex::new(Status {
                credential,
                ..Default::default()
            })),
            file,
            refresh: Default::default(),
            login: Default::default(),
        })
    }
    fn state(&self) -> MutexGuard<'_, Status> {
        self.status.lock().unwrap()
    }
    pub fn epoch(&self) -> u64 {
        self.state().credential.generation
    }
    pub fn signed_in(&self) -> bool {
        !self.state().credential.auth.is_null()
    }
    pub fn expired(&self, epoch: u64) {
        let mut s = self.state();
        if epoch == s.credential.generation {
            s.expired = true;
        }
    }
    fn save(&self, s: &mut Status, auth: Value, generation: u64) -> Result<()> {
        let cred = Credential { auth, generation };
        hostfs::atomic_write(&self.file, &serde_json::to_vec(&cred)?)?;
        s.credential = cred;
        Ok(())
    }
    pub fn callbacks(&self, generation: u64) -> (HostFunction, HostFunction) {
        let read = self.clone();
        let get = HostFunction::sync(move |_| {
            let s = read.state();
            if s.credential.generation == generation && !s.credential.auth.is_null() {
                Ok(s.credential.auth.clone().into())
            } else {
                Ok(HostValue::Undefined)
            }
        });
        let write = self.clone();
        let set = HostFunction::sync(move |args| {
            let mut s = write.state();
            let input = args
                .first()
                .and_then(HostValue::as_json)
                .ok_or_else(|| JsError::new("invalid credential update"))?;
            if s.credential.generation == generation
                && !s.credential.auth.is_null()
                && input["path"]["id"] == super::ID
                && input["body"]["type"] == "oauth"
            {
                write
                    .save(&mut s, input["body"].clone(), generation)
                    .map_err(|_| JsError::new("cannot persist credential"))?;
            }
            Ok(HostValue::Undefined)
        });
        (get, set)
    }
    pub fn snapshot(&self) -> Value {
        let s = self.state();
        let auth = &s.credential.auth;
        let label = ["cardName", "email", "name", "accountId"]
            .iter()
            .find_map(|key| auth[key].as_str().filter(|v| !v.is_empty()))
            .unwrap_or("");
        json!({"signedIn":!auth.is_null(),"expired":s.expired,"account":{"label":label},"login":s.login,
            "models":s.models,"modelsAt":s.models_at,"modelsError":s.models_error,"usage":s.usage,"usageAt":s.usage_at,"usageError":s.usage_error})
    }
    pub fn pending(&self) -> bool {
        self.state().login["status"] == "pending"
    }
    pub fn cancel(&self) {
        let mut s = self.state();
        if s.login["status"] == "pending" {
            s.login = json!({"status":"cancelled"});
        }
    }
    pub async fn logout(&self) -> Result<()> {
        let _login = self.login.lock().await;
        {
            let mut s = self.state();
            let generation = s.credential.generation + 1;
            match fs::remove_file(&self.file) {
                Ok(()) => {}
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => return Err(e.into()),
            }
            *s = Status {
                credential: Credential {
                    generation,
                    auth: Value::Null,
                },
                ..Default::default()
            };
        }
        Ok(())
    }
    pub async fn login(&self, bridge: &Plugin, input: Value) -> Result<Value> {
        let _login = self.login.lock().await;
        if self.pending() {
            return Ok(self.state().login.clone());
        }
        let flow = bridge.authorize(input["pat"].as_str()).await?;
        let url = flow.get("url").await?.into_json();
        let href = reqwest::Url::parse(url.as_str().context("missing authorization URL")?)?;
        ensure!(
            href.scheme() == "https"
                && href
                    .host_str()
                    .is_some_and(|h| h == "qoder.com" || h.ends_with(".qoder.com")),
            "unexpected authorization site"
        );
        let id = uuid::Uuid::new_v4().to_string();
        let login = json!({"id":id,"status":"pending","url":url});
        self.state().login = login.clone();
        let account = self.clone();
        let bridge = bridge.clone();
        tokio::spawn(async move {
            let result = tokio::time::timeout(
                Duration::from_secs(15 * 60),
                flow.call_method("callback", vec![]),
            )
            .await;
            let result = async {
                let mut cred = result.context("授权超时")??.into_json();
                ensure!(
                    cred["type"] == "success"
                        && cred["access"].as_str().is_some_and(|s| !s.is_empty()),
                    "授权未完成"
                );
                let login_lock = account.login.lock().await;
                {
                    let mut s = account.state();
                    if s.login["id"] != id || s.login["status"] != "pending" {
                        return Ok(());
                    }
                    let generation = s.credential.generation + 1;
                    cred["type"] = json!("oauth");
                    account.save(&mut s, cred, generation)?;
                    s.models.clear();
                    s.usage = Value::Null;
                    s.models_at.clear();
                    s.usage_at.clear();
                    s.expired = false;
                }
                drop(login_lock);
                account.refresh(&bridge).await;
                let mut s = account.state();
                if s.login["id"] == id {
                    s.login = json!({"status":"authorized"});
                }
                Ok::<_, anyhow::Error>(())
            }
            .await;
            if let Err(error) = result {
                let mut s = account.state();
                if s.login["id"] == id {
                    s.login = json!({"status":"failed","error":error.to_string()});
                }
            }
        });
        Ok(login)
    }
    pub async fn refresh(&self, bridge: &Plugin) {
        let _refresh = self.refresh.lock().await;
        if !self.signed_in() {
            return;
        }
        let generation = self.epoch();
        let (models, usage) = tokio::join!(bridge.models(), bridge.usage());
        let mut s = self.state();
        if s.credential.generation != generation {
            return;
        }
        let at = chrono::Utc::now().to_rfc3339();
        match models {
            Ok(raw) => {
                let listing = raw.into_json();
                s.models = listing.as_object().into_iter().flat_map(|models| models.values())
                    .filter(|model| model["id"].is_string())
                    .map(|m| json!({
                        "id": m["id"], "name": m["name"],
                        "context": m["limit"]["context"], "output": m["limit"]["output"],
                        "images": m["capabilities"]["input"]["image"], "reasoning": m["capabilities"]["reasoning"],
                        "free": m["free"], "rate": m["rate"]
                    })).collect();
                s.models_at = at.clone();
                s.models_error = if s.models.is_empty() {
                    "未取得可用模型".into()
                } else {
                    String::new()
                };
            }
            Err(e) => s.models_error = e.to_string(),
        }
        match usage {
            Ok(v) => {
                let v = v.into_json();
                if v["signIn"] == "expired" {
                    s.expired = true;
                } else if v["signIn"] == "renewed" {
                    s.expired = false;
                }
                if let Some(e) = v["error"].as_str() {
                    s.usage_error = e.into()
                } else {
                    s.usage = json!({"plan":v["plan"],"until":v["until"],"windows":v["windows"]});
                    s.usage_at = at;
                    s.usage_error.clear();
                }
            }
            Err(e) => s.usage_error = e.to_string(),
        }
    }
    pub async fn renew(&self, bridge: &Plugin) {
        let original = self.state().credential.auth.clone();
        if !original["expires"]
            .as_i64()
            .is_some_and(|v| v - chrono::Utc::now().timestamp_millis() < 600_000)
        {
            return;
        }
        let generation = self.epoch();
        if let Ok(value) = bridge.renew().await
            && let Some(changes) = value.as_json().and_then(Value::as_object)
        {
            let mut s = self.state();
            if s.credential.generation != generation || s.credential.auth != original {
                return;
            }
            let mut cred = original;
            if let Some(fields) = cred.as_object_mut() {
                fields.extend(changes.clone());
                if self.save(&mut s, cred, generation).is_ok() {
                    s.expired = false;
                }
            }
        }
    }
}
