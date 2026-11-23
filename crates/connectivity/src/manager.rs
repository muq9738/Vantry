//! Route selection and live status for one saved peer. Each dial uses the current paired identity.
use crate::{
    identity::IdentityStore,
    peer::{PeerConnection, Route},
};
use anyhow::{Result, anyhow, ensure};
use serde_json::Value;
use std::{sync::Arc, time::Duration};
use tokio::{
    sync::{Mutex, watch},
    time::timeout,
};
use tokio_util::sync::CancellationToken;
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum State {
    Idle,
    Connecting,
    Online {
        mode: &'static str,
        handshake: Duration,
    },
    Offline,
    Closed,
}
pub struct PeerManager {
    store: Arc<IdentityStore>,
    id: String,
    route: Route,
    stop: CancellationToken,
    state: watch::Sender<State>,
    dial: Mutex<()>,
}
impl PeerManager {
    pub fn new(
        store: Arc<IdentityStore>,
        id: String,
        route: Route,
        stop: CancellationToken,
    ) -> Self {
        Self {
            store,
            id,
            route,
            stop: stop.child_token(),
            state: watch::channel(State::Idle).0,
            dial: Mutex::new(()),
        }
    }
    pub fn subscribe(&self) -> watch::Receiver<State> {
        self.state.subscribe()
    }
    pub fn close(&self) {
        self.stop.cancel();
        self.state.send_replace(State::Closed);
    }
    pub async fn connect(&self) -> Result<PeerConnection> {
        let _guard = self.dial.lock().await;
        ensure!(!self.stop.is_cancelled(), "peer manager closed");
        let cfg = self.store.snapshot();
        let p = cfg
            .peers
            .iter()
            .find(|p| p.id == self.id)
            .ok_or_else(|| anyhow!("unknown peer"))?;
        self.state.send_replace(State::Connecting);
        let result =
            PeerConnection::connect(&p.pair, Some(&cfg), self.route.clone(), self.stop.clone())
                .await;
        let state = match &result {
            Ok(c) => State::Online {
                mode: c.mode,
                handshake: c.handshake,
            },
            Err(_) => State::Offline,
        };
        self.state.send_modify(|s| {
            if *s != State::Closed {
                *s = state
            }
        });
        result
    }
    pub async fn health(&self) -> Result<Value> {
        let conn = self.connect().await?;
        let result = timeout(Duration::from_secs(15), conn.health()).await;
        let value = match result {
            Ok(Ok(v)) => v,
            _ => {
                self.state.send_modify(|s| {
                    if *s != State::Closed {
                        *s = State::Offline
                    }
                });
                return Err(anyhow!("peer health check failed"));
            }
        };
        let refresh = async {
            let c = self.connect().await?;
            let identity = c.get_json("/v1/access/identity").await?;
            self.store.sync_identity(&self.id, identity)
        };
        let _ = timeout(Duration::from_secs(3), refresh).await;
        Ok(value)
    }
    pub async fn version(&self) -> Result<Value> {
        self.connect().await?.version().await
    }
    pub async fn check_update(&self) -> Result<Value> {
        self.connect().await?.check_update().await
    }
}
impl Drop for PeerManager {
    fn drop(&mut self) {
        self.close()
    }
}
