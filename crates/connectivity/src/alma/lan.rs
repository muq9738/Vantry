use super::{Endpoint, HANDSHAKE, Host, MAX_FRAME, PhoneWire, crypto, serve_phone};
use anyhow::{Result, bail};
use std::sync::Arc;
use tokio::{net::TcpListener, task::JoinSet, time::timeout};
use tokio_tungstenite::{
    accept_hdr_async_with_config,
    tungstenite::{
        handshake::server::{Request, Response},
        protocol::WebSocketConfig,
    },
};
use tokio_util::sync::CancellationToken;

impl Host {
    pub fn set_lan_listener(&mut self, bound: std::net::SocketAddr) {
        if let Endpoint::Phone(scope) = &mut self.endpoint {
            scope.lan = Some(bound);
        }
    }

    /// Alma initially dials the relay-shaped address from the QR. Its saved LAN
    /// candidate uses the shorter path. Both require the same E2E handshake.
    pub async fn serve_lan(self, listener: TcpListener, stop: CancellationToken) -> Result<()> {
        let Endpoint::Phone(scope) = self.endpoint else { bail!("Expected an Alma mobile host"); };
        let route: Arc<str> = format!("/c/{}/__alma_e2e__", crypto::routing(&self.code)?).into();
        let mut sessions = JoinSet::new();
        loop {
            tokio::select! {
                biased;
                _ = stop.cancelled() => break,
                Some(_) = sessions.join_next(), if !sessions.is_empty() => (),
                accepted = listener.accept() => {
                    let Ok((socket, _)) = accepted else {
                        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
                        continue;
                    };
                    if sessions.len() >= 16 { continue; }
                    let route = route.clone();
                    let (scope, code, activity) = (scope.clone(), self.code.clone(), self.activity.clone());
                    sessions.spawn(async move {
                        let check = move |req: &Request, res: Response| {
                            if req.uri().path() == route.as_ref() || req.uri().path() == "/__alma_e2e__" {
                                Ok(res)
                            } else {
                                Err(http::Response::builder().status(404).body(None).expect("static response"))
                            }
                        };
                        let config = WebSocketConfig::default()
                            .max_message_size(Some(MAX_FRAME)).max_frame_size(Some(MAX_FRAME));
                        let Ok(Ok(ws)) = timeout(HANDSHAKE, accept_hdr_async_with_config(socket, check, Some(config))).await else { return };
                        let _ = serve_phone(PhoneWire::Lan(ws), &code, scope, activity).await;
                    });
                }
            }
        }
        sessions.shutdown().await;
        Ok(())
    }
}
