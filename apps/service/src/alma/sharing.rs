use super::pairing_link;
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{io::ErrorKind, net::SocketAddr, path::Path, sync::Arc};
use tokio::{net::TcpListener, sync::Mutex, task::JoinSet};
use tokio_util::sync::CancellationToken;
use vantry_client::CoreClient;
use vantry_connectivity::{
    alma::{Activity, Host, new_code},
    transport::relay_url,
};
use vantry_core::Core;
use vantry_platform::hostfs;

const FILE: &str = "alma-mobile-share.json";

#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Share {
    id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    operation_scope: Option<String>,
    thread_id: String,
    code: String,
    port: u16,
    relay: String,
    key: String,
}

impl Share {
    fn operation_scope(&self) -> &str {
        self.operation_scope.as_deref().unwrap_or(&self.id)
    }
}

struct Live {
    relay: Option<url::Url>,
    bound: SocketAddr,
    title: String,
    activity: Arc<Activity>,
    stop: CancellationToken,
    tasks: JoinSet<()>,
}

impl Live {
    async fn close(mut self) {
        self.stop.cancel();
        while self.tasks.join_next().await.is_some() {}
    }
}

#[derive(Default)]
struct State {
    share: Option<Share>,
    live: Option<Live>,
    error: String,
}

pub struct Sharing {
    core: Core,
    state: Mutex<State>,
}

fn lan_url(bound: SocketAddr) -> Option<url::Url> {
    let address = vantry_connectivity::lan::listener_addresses(bound).ok()?.into_iter().next()?;
    url::Url::parse(&format!("http://{address}")).ok()
}

impl Sharing {
    pub fn new(core: Core) -> Self {
        Self {
            core,
            state: Default::default(),
        }
    }

    fn save(&self, share: Option<&Share>) -> Result<()> {
        Ok(hostfs::atomic_write(
            &self.core.directory().join(FILE),
            &serde_json::to_vec(&share)?,
        )?)
    }

    async fn start(&self, share: &mut Share) -> Result<Live> {
        let thread = self.core.thread_detail(&share.thread_id, 1)?.thread;
        ensure!(!thread.archived, "请先恢复已归档的会话");
        ensure!(
            URL_SAFE_NO_PAD.decode(&share.code)?.len() == 32
                && uuid::Uuid::parse_str(&share.id).is_ok(),
            "配对记录无效"
        );
        let listener = TcpListener::bind(("0.0.0.0", share.port))
            .await
            .context("无法打开 Alma 局域网端口")?;
        let bound = listener.local_addr()?;
        share.port = bound.port();
        let relay = if share.relay.is_empty() {
            None
        } else {
            Some(relay_url(&share.relay, false)?)
        };
        let mut host = Host::new(
            match &relay {
                Some(url) => url.clone(),
                None => url::Url::parse(&format!("http://{bound}"))?,
            },
            share.code.clone(),
            Some(share.key.clone()).filter(|k| !k.is_empty()),
            share.operation_scope().into(),
            share.thread_id.clone(),
            CoreClient::from_data_dir(self.core.directory()),
        )?;
        host.set_lan_listener(bound);
        let mut live = Live {
            relay,
            bound,
            title: thread.title,
            activity: host.activity.clone(),
            stop: CancellationToken::new(),
            tasks: JoinSet::new(),
        };
        if !share.relay.is_empty() {
            let relay = host.clone();
            let stop = live.stop.clone();
            live.tasks.spawn(async move {
                let _ = relay.run(stop).await;
            });
        }
        let stop = live.stop.clone();
        live.tasks.spawn(async move {
            let _ = host.serve_lan(listener, stop).await;
        });
        Ok(live)
    }

    pub async fn restore(&self) {
        let mut state = self.state.lock().await;
        let result: Result<()> = async {
            let bytes = match hostfs::read_file(self.core.directory(), Path::new(FILE), 32 * 1024) {
                Ok(b) => b,
                Err(e) if e.kind() == ErrorKind::NotFound => return Ok(()),
                Err(e) => return Err(e.into()),
            };
            state.share = serde_json::from_slice(&bytes)?;
            if let Some(share) = &mut state.share {
                state.live = Some(self.start(share).await?);
            }
            Ok(())
        }
        .await;
        if let Err(e) = result {
            state.error = format!("恢复 Alma 共享失败：{e}");
        }
    }

    /// The private invitation is returned only to an explicit local action for its thread.
    fn view(&self, state: &State, reveal: Option<&str>) -> Value {
        let Some(share) = &state.share else {
            return json!({"enabled":false,"error":state.error});
        };
        let mut out = json!({"enabled":true,"shareId":share.id,"threadId":share.thread_id,
            "relay":share.relay,"error":state.error,"ready":false,"phones":0});
        if let Some(live) = &state.live {
            out["title"] = live.title.clone().into();
            let endpoint = live.relay.clone().or_else(|| lan_url(live.bound));
            out["ready"] = (endpoint.is_some() && (live.relay.is_none() || live.activity.relay_online())).into();
            out["relayRejected"] = live.activity.rejected().into();
            out["phones"] = live.activity.phones().into();
            if reveal == Some(share.thread_id.as_str()) && let Some(endpoint) = endpoint {
                out["link"] = pairing_link(&endpoint, &share.code).to_string().into();
            }
        }
        out
    }

    pub async fn status(&self, reveal: Option<&str>) -> Value {
        self.view(&*self.state.lock().await, reveal)
    }

    pub async fn create(
        &self,
        thread: &str,
        relay: &str,
        key: &str,
        replacing: Option<&str>,
    ) -> Result<Value> {
        ensure!(
            !thread.is_empty()
                && thread.len() <= 255
                && thread
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"-._~".contains(&b)),
            "请选择一条已保存的 Vantry 会话"
        );
        let mut state = self.state.lock().await;
        if let Some(id) = replacing {
            ensure!(
                state
                    .share
                    .as_ref()
                    .is_some_and(|s| s.id == id && s.thread_id == thread),
                "配对已变更，请刷新后重试"
            );
        } else if let Some(saved) = &state.share {
            ensure!(
                saved.thread_id == thread,
                "已有其他会话正在共享，请先停止原共享"
            );
            if state.live.is_none() {
                let mut saved = saved.clone();
                let live = self.start(&mut saved).await?;
                state.live = Some(live);
                state.error.clear();
            }
            return Ok(self.view(&state, Some(thread)));
        }
        let relay = relay.trim();
        ensure!(
            relay.len() <= 2048 && key.len() <= 16000 && (key.is_empty() || !relay.is_empty()),
            "无效的中继设置"
        );
        if !relay.is_empty() {
            relay_url(relay, false)?;
        }
        let mut share = Share {
            id: uuid::Uuid::new_v4().to_string(),
            operation_scope: state.share.as_ref().map(|old| old.operation_scope().to_owned()),
            thread_id: thread.into(),
            code: new_code()?,
            port: 0,
            relay: relay.into(),
            key: key.trim().into(),
        };
        let live = self.start(&mut share).await?;
        // Leave the previous pairing usable until its replacement has registered and is durable.
        if replacing.is_some()
            && !share.relay.is_empty()
            && let Err(error) = live.activity.wait_for_registration().await
        {
            live.close().await;
            return Err(error);
        }
        if let Err(error) = self.save(Some(&share)) {
            live.close().await;
            return Err(error);
        }
        let old = state.live.replace(live);
        state.share = Some(share);
        state.error.clear();
        if let Some(old) = old {
            old.close().await;
        }
        Ok(self.view(&state, Some(thread)))
    }

    pub async fn revoke(&self, id: &str) -> Result<Value> {
        let mut state = self.state.lock().await;
        ensure!(
            state.share.as_ref().is_some_and(|s| s.id == id),
            "配对已变更，请刷新后重试"
        );
        self.save(None)?;
        if let Some(live) = state.live.take() {
            live.close().await;
        }
        state.share = None;
        state.error.clear();
        Ok(self.view(&state, None))
    }

    pub async fn shutdown(&self) {
        if let Some(live) = self.state.lock().await.live.take() {
            live.close().await;
        }
    }
}
