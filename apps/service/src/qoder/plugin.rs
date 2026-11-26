//! Typed calls into an imported OpenCode module. Function handles, promises and
//! stream readers stay in QuickJS; no host-side JavaScript or Node executable.
use super::account::Account;
use anyhow::{Context, Result, ensure};
use pi_js::{
    HostValue, JsHandle, JsRef,
    rquickjs::{Object, function::Constructor},
};
use serde_json::{Value, json};
use std::sync::Arc;
use tokio::sync::{Mutex, OnceCell};

#[derive(Clone)]
pub struct Plugin {
    factory: JsRef,
    account: Account,
    bound: Arc<Mutex<Option<Hooks>>>,
}
#[derive(Clone)]
struct Hooks {
    epoch: u64,
    auth: JsRef,
    provider: JsRef,
    get_auth: JsRef,
    methods: JsRef,
    choices: Vec<Value>,
    loader: Arc<OnceCell<JsRef>>,
}
async fn object(handle: &JsHandle, value: Value) -> Result<JsRef> {
    Ok(handle
        .with(move |s| Ok(s.persist(s.from_json(&value)?)))
        .await?)
}
impl Plugin {
    pub fn new(factory: JsRef, account: Account) -> Self {
        Self {
            factory,
            account,
            bound: Default::default(),
        }
    }
    async fn hooks(&self) -> Result<Hooks> {
        let mut cached = self.bound.lock().await;
        let epoch = self.account.epoch();
        if let Some(h) = cached.as_ref().filter(|h| h.epoch == epoch) {
            return Ok(h.clone());
        }
        let (get, set) = self.account.callbacks(epoch);
        let (options, get_auth) = self
            .factory
            .handle()
            .with(move |s| {
                let options = s.from_json(&json!({"client":{"auth":{}}}))?;
                let obj = options.as_object().unwrap();
                let client: Object = obj.get("client").map_err(|e| s.caught(e))?;
                let auth: Object = client.get("auth").map_err(|e| s.caught(e))?;
                auth.set("set", s.host_function(set)?)
                    .map_err(|e| s.caught(e))?;
                Ok((
                    s.persist(options),
                    s.persist(s.host_function(get)?.into_value()),
                ))
            })
            .await?;
        let hooks = self.factory.call_ref(vec![options.into()]).await?;
        let auth = hooks.get_ref("auth").await?;
        ensure!(
            auth.get("provider").await?.as_str() == Some(super::ID),
            "unexpected provider ID"
        );
        let provider = hooks.get_ref("provider").await?;
        for name in ["loader", "refresh", "usage"] {
            ensure!(
                auth.get(name).await?.as_js_ref().is_some(),
                "missing auth.{name}"
            );
        }
        ensure!(
            provider.get("models").await?.as_js_ref().is_some(),
            "missing provider.models"
        );
        let methods = auth.get_ref("methods").await?;
        let choices = methods
            .to_json()
            .await?
            .as_array()
            .context("missing sign-in methods")?
            .clone();
        ensure!(
            choices
                .iter()
                .any(|m| m["type"] == "oauth" && m["prompts"].as_array().is_none_or(Vec::is_empty)),
            "missing browser sign-in"
        );
        let h = Hooks {
            epoch,
            auth,
            provider,
            get_auth,
            methods,
            choices,
            loader: Default::default(),
        };
        *cached = Some(h.clone());
        Ok(h)
    }
    pub async fn check(&self, selftest: bool) -> Result<()> {
        let h = self.hooks().await?;
        if selftest {
            h.auth
                .call_method_ref("loader", vec![h.get_auth.into()])
                .await?;
            self.models().await?;
        }
        Ok(())
    }
    pub async fn authorize(&self, pat: Option<&str>) -> Result<JsRef> {
        let h = self.hooks().await?;
        let index = h
            .choices
            .iter()
            .position(|m| {
                m["type"] == "oauth"
                    && match pat {
                        Some(_) => m["prompts"]
                            .as_array()
                            .is_some_and(|p| p.iter().any(|f| f["key"] == "pat")),
                        None => m["prompts"].as_array().is_none_or(Vec::is_empty),
                    }
            })
            .context("missing requested sign-in method")?;
        Ok(h.methods
            .get_ref(&index.to_string())
            .await?
            .call_method_ref(
                "authorize",
                pat.map(|token| json!({"pat":token}).into())
                    .into_iter()
                    .collect(),
            )
            .await?)
    }
    pub async fn models(&self) -> Result<HostValue> {
        let h = self.hooks().await?;
        let cred = h.get_auth.call(vec![]).await?.into_json();
        Ok(h.provider
            .call_method(
                "models",
                vec![json!({"models":{}}).into(), json!({"auth":cred}).into()],
            )
            .await?)
    }
    pub async fn usage(&self) -> Result<HostValue> {
        let h = self.hooks().await?;
        Ok(h.auth.call_method("usage", vec![h.get_auth.into()]).await?)
    }
    pub async fn renew(&self) -> Result<HostValue> {
        let h = self.hooks().await?;
        let cred = h.get_auth.call(vec![]).await?;
        Ok(h.auth.call_method("refresh", vec![cred]).await?)
    }
    pub async fn abort_controller(&self) -> Result<JsRef> {
        Ok(self
            .factory
            .handle()
            .with(|s| {
                let ctor: Constructor = s
                    .ctx()
                    .globals()
                    .get("AbortController")
                    .map_err(|e| s.caught(e))?;
                let controller: Object = ctor.construct(()).map_err(|e| s.caught(e))?;
                Ok(s.persist(controller.into_value()))
            })
            .await?)
    }
    pub async fn fetch(&self, body: &str, session: String, controller: &JsRef) -> Result<JsRef> {
        let h = self.hooks().await?;
        let loader = h
            .loader
            .get_or_try_init(|| {
                h.auth
                    .call_method_ref("loader", vec![h.get_auth.clone().into()])
            })
            .await?;
        let mut headers = json!({"Content-Type":"application/json"});
        if !session.is_empty() {
            headers["x-magpie-qoder-session"] = session.into();
        }
        let init = object(
            self.factory.handle(),
            json!({"method":"POST","headers":headers,"body":body}),
        )
        .await?;
        init.set("signal", controller.get_ref("signal").await?.into())
            .await?;
        Ok(loader
            .call_method_ref(
                "fetch",
                vec![
                    "https://qoder.invalid/v1/chat/completions".into(),
                    init.into(),
                ],
            )
            .await?)
    }
}
