//! Encrypted LAN-direct & WebSocket media/control transport for Remote Desktop.
//!
//! - [`DesktopHost`] generates an ephemeral self-signed TLS certificate (`rcgen`)
//!   and random session bearer token, serves authenticated WebSocket media/control
//!   sessions on a local TLS listener, publishes a `vantry-lan://` ticket with the
//!   pinned certificate SHA-256 fingerprint, broadcasts video frames, and receives
//!   pointer/keyboard/wheel/clipboard input events from approved viewers.
//! - [`DesktopViewer`] connects to a [`LanTicket`], verifies the pinned certificate
//!   fingerprint (rejecting MITM), authenticates with the session token, exposes
//!   the latest received [`DesktopFrame`] via a watch channel, and transmits
//!   [`InputEvent`] messages over the live WebSocket.
use crate::{
    pairing::{matches_secret, token},
    transport::{Stream, Tasks, accepted, upgrade},
    web::{Reply, bearer, response},
};
use anyhow::{Context, Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use bytes::Bytes;
use futures::{SinkExt, StreamExt};
use http::{Method, Request};
use hyper::{body::Incoming, server::conn::http1, service::service_fn};
use hyper_util::rt::TokioIo;
use ring::digest::{SHA256, digest};
use rustls::{
    ClientConfig, DigitallySignedStruct, Error as TlsError, ServerConfig, SignatureScheme,
    client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier},
    pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, ServerName, UnixTime},
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    convert::Infallible,
    net::SocketAddr,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    net::{TcpListener, TcpStream},
    sync::{mpsc, watch},
};
use tokio_rustls::{TlsAcceptor, TlsConnector};
use tokio_tungstenite::{client_async, tungstenite::Message};
use tokio_util::sync::CancellationToken;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct LanTicket {
    pub addr: String,
    pub token: String,
    pub cert_sha256: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,
}

impl LanTicket {
    pub fn link(&self) -> String {
        format!(
            "vantry-lan://{}",
            URL_SAFE_NO_PAD.encode(serde_json::to_vec(self).unwrap())
        )
    }

    pub fn parse(raw: &str) -> Result<Self> {
        let encoded = raw
            .trim()
            .strip_prefix("vantry-lan://")
            .ok_or_else(|| anyhow!("invalid vantry-lan:// link"))?;
        ensure!(encoded.len() <= 8192, "LAN link too long");
        let bytes = URL_SAFE_NO_PAD
            .decode(encoded)
            .map_err(|_| anyhow!("invalid LAN link encoding"))?;
        let ticket: Self =
            serde_json::from_slice(&bytes).map_err(|_| anyhow!("invalid LAN link payload"))?;
        let _: SocketAddr = ticket
            .addr
            .parse()
            .map_err(|_| anyhow!("invalid LAN host address"))?;
        ensure!(ticket.token.len() >= 16, "invalid LAN session token");
        ensure!(ticket.cert_sha256.len() == 64, "invalid LAN cert fingerprint");
        Ok(ticket)
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DesktopFrame {
    pub seq: u64,
    pub width: u32,
    pub height: u32,
    pub mime: String,
    pub data_b64: String,
}

impl DesktopFrame {
    pub fn data_uri(&self) -> String {
        format!("data:{};base64,{}", self.mime, self.data_b64)
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct InputEvent {
    pub kind: String,
    #[serde(default)]
    pub x: f64,
    #[serde(default)]
    pub y: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub button: String,
    #[serde(default)]
    pub dx: f64,
    #[serde(default)]
    pub dy: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub key: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub code: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub text: String,
    #[serde(default)]
    pub shift: bool,
    #[serde(default)]
    pub ctrl: bool,
    #[serde(default)]
    pub alt: bool,
    #[serde(default)]
    pub meta: bool,
}

fn sha256_hex(bytes: &[u8]) -> String {
    digest(&SHA256, bytes)
        .as_ref()
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn ephemeral_tls() -> Result<(Arc<ServerConfig>, String)> {
    let cert = rcgen::generate_simple_self_signed(vec![
        "vantry.lan".to_owned(),
        "localhost".to_owned(),
    ])?;
    let der = cert.cert.der().to_vec();
    let fingerprint = sha256_hex(&der);
    let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(
        cert.key_pair.serialize_der(),
    ));
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let cfg = ServerConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()?
        .with_no_client_auth()
        .with_single_cert(vec![CertificateDer::from(der)], key)?;
    Ok((Arc::new(cfg), fingerprint))
}

#[derive(Debug)]
struct PinnedFingerprintVerifier {
    expected_hex: String,
    provider: Arc<rustls::crypto::CryptoProvider>,
}

impl ServerCertVerifier for PinnedFingerprintVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> std::result::Result<ServerCertVerified, TlsError> {
        let got = sha256_hex(end_entity.as_ref());
        if matches_secret(&self.expected_hex, &got) {
            Ok(ServerCertVerified::assertion())
        } else {
            Err(TlsError::General("LAN desktop certificate fingerprint mismatch".into()))
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> std::result::Result<HandshakeSignatureValid, TlsError> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> std::result::Result<HandshakeSignatureValid, TlsError> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

pub struct DesktopHost {
    ticket: LanTicket,
    frames: watch::Sender<Option<DesktopFrame>>,
    viewer_count: Arc<Mutex<usize>>,
    stop: CancellationToken,
}

impl DesktopHost {
    pub async fn start(
        bind_addr: &str,
        host_name: &str,
        on_input: Arc<dyn Fn(InputEvent) + Send + Sync>,
    ) -> Result<Self> {
        let (tls_cfg, cert_sha256) = ephemeral_tls()?;
        let acceptor = TlsAcceptor::from(tls_cfg);
        let listener = TcpListener::bind(bind_addr).await?;
        let local_addr = listener.local_addr()?;
        let secret = token();
        let ticket = LanTicket {
            addr: local_addr.to_string(),
            token: secret.clone(),
            cert_sha256,
            name: host_name.to_owned(),
        };
        let (frames, _) = watch::channel::<Option<DesktopFrame>>(None);
        let viewer_count = Arc::new(Mutex::new(0usize));
        let stop = CancellationToken::new();

        let srv_stop = stop.clone();
        let srv_frames = frames.clone();
        let srv_viewers = viewer_count.clone();
        tokio::spawn(async move {
            let tasks = Tasks::new(srv_stop.clone());
            loop {
                let (tcp, _) = tokio::select! {
                    _ = srv_stop.cancelled() => break,
                    r = listener.accept() => match r {
                        Ok(pair) => pair,
                        Err(_) => break,
                    }
                };
                let acceptor = acceptor.clone();
                let secret = secret.clone();
                let frames = srv_frames.clone();
                let viewers = srv_viewers.clone();
                let on_input = on_input.clone();
                let group = tasks.clone();
                tasks.spawn(async move {
                    let Ok(tls) = acceptor.accept(tcp).await else {
                        return;
                    };
                    let svc = service_fn(move |req| {
                        let secret = secret.clone();
                        let frames = frames.clone();
                        let viewers = viewers.clone();
                        let on_input = on_input.clone();
                        let group = group.clone();
                        async move {
                            let reply = handle_host_http(
                                req, &secret, frames, viewers, on_input, group,
                            )
                            .unwrap_or_else(|code| response(code, Bytes::new()));
                            Ok::<_, Infallible>(reply)
                        }
                    });
                    let _ = http1::Builder::new()
                        .serve_connection(TokioIo::new(tls), svc)
                        .with_upgrades()
                        .await;
                });
            }
        });

        Ok(Self {
            ticket,
            frames,
            viewer_count,
            stop,
        })
    }

    pub fn ticket(&self) -> &LanTicket {
        &self.ticket
    }

    pub fn publish_frame(&self, frame: DesktopFrame) {
        self.frames.send_replace(Some(frame));
    }

    pub fn latest_frame(&self) -> Option<DesktopFrame> {
        self.frames.borrow().clone()
    }

    pub fn viewer_count(&self) -> usize {
        *self.viewer_count.lock().unwrap()
    }

    pub fn stop(&self) {
        self.stop.cancel();
    }
}

impl Drop for DesktopHost {
    fn drop(&mut self) {
        self.stop.cancel();
    }
}

fn handle_host_http(
    mut req: Request<Incoming>,
    secret: &str,
    frames: watch::Sender<Option<DesktopFrame>>,
    viewers: Arc<Mutex<usize>>,
    on_input: Arc<dyn Fn(InputEvent) + Send + Sync>,
    tasks: Tasks,
) -> Result<Reply, u16> {
    if req.uri().path() != "/desktop/lan/ws" || req.method() != Method::GET {
        return Err(404);
    }
    if !matches_secret(bearer(req.headers()), secret) {
        return Err(401);
    }
    let (reply, up) = upgrade(&mut req)?;
    let stop = tasks.stop.clone();
    tasks.spawn(async move {
        let Ok(ws) = accepted(up, 16 << 20).await else {
            return;
        };
        {
            *viewers.lock().unwrap() += 1;
        }
        let (mut sink, mut stream) = ws.split();
        let mut rx = frames.subscribe();
        // Send initial frame immediately if one is already published.
        let initial = rx.borrow_and_update().clone();
        if let Some(frame) = initial {
            let msg = json!({"type": "frame", "frame": frame}).to_string();
            if sink.send(Message::text(msg)).await.is_err() {
                *viewers.lock().unwrap() -= 1;
                return;
            }
        }
        loop {
            tokio::select! {
                _ = stop.cancelled() => break,
                changed = rx.changed() => {
                    if changed.is_err() {
                        break;
                    }
                    let frame = rx.borrow_and_update().clone();
                    if let Some(frame) = frame {
                        let msg = json!({"type": "frame", "frame": frame}).to_string();
                        if sink.send(Message::text(msg)).await.is_err() {
                            break;
                        }
                    }
                }
                incoming = stream.next() => match incoming {
                    Some(Ok(Message::Text(text))) => {
                        if let Ok(v) = serde_json::from_str::<Value>(&text)
                            && v["type"] == "input"
                            && let Ok(ev) = serde_json::from_value::<InputEvent>(v["event"].clone())
                        {
                            on_input(ev);
                        }
                    }
                    Some(Ok(Message::Close(_))) | None | Some(Err(_)) => break,
                    _ => {}
                }
            }
        }
        let _ = sink.close().await;
        *viewers.lock().unwrap() -= 1;
    });
    Ok(reply)
}

pub struct DesktopViewer {
    latest: watch::Receiver<Option<DesktopFrame>>,
    input_tx: mpsc::Sender<InputEvent>,
    stop: CancellationToken,
    target_label: String,
}

impl DesktopViewer {
    pub async fn connect_lan(ticket: &LanTicket) -> Result<Self> {
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let verifier = Arc::new(PinnedFingerprintVerifier {
            expected_hex: ticket.cert_sha256.clone(),
            provider: provider.clone(),
        });
        let tls_cfg = ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()?
            .dangerous()
            .with_custom_certificate_verifier(verifier)
            .with_no_client_auth();
        let connector = TlsConnector::from(Arc::new(tls_cfg));
        let tcp = tokio::time::timeout(Duration::from_secs(8), TcpStream::connect(&ticket.addr))
            .await
            .context("timed out connecting to LAN desktop host")??;
        let tls: Stream = Box::new(
            connector
                .connect(ServerName::try_from("vantry.lan")?, tcp)
                .await
                .context("LAN desktop TLS handshake failed")?,
        );
        let req = Request::builder()
            .method(Method::GET)
            .uri(format!("wss://{}/desktop/lan/ws", ticket.addr))
            .header("host", &ticket.addr)
            .header("authorization", format!("Bearer {}", ticket.token))
            .header("connection", "Upgrade")
            .header("upgrade", "websocket")
            .header("sec-websocket-version", "13")
            .header(
                "sec-websocket-key",
                tokio_tungstenite::tungstenite::handshake::client::generate_key(),
            )
            .body(())?;
        let (ws, _) = client_async(req, tls)
            .await
            .context("LAN desktop WebSocket upgrade failed")?;
        let (mut sink, mut stream) = ws.split();
        let (frame_tx, frame_rx) = watch::channel::<Option<DesktopFrame>>(None);
        let (input_tx, mut input_rx) = mpsc::channel::<InputEvent>(128);
        let stop = CancellationToken::new();
        let task_stop = stop.clone();
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = task_stop.cancelled() => break,
                    Some(ev) = input_rx.recv() => {
                        let msg = json!({"type": "input", "event": ev}).to_string();
                        if sink.send(Message::text(msg)).await.is_err() {
                            break;
                        }
                    }
                    incoming = stream.next() => match incoming {
                        Some(Ok(Message::Text(text))) => {
                            if let Ok(v) = serde_json::from_str::<Value>(&text)
                                && v["type"] == "frame"
                                && let Ok(frame) = serde_json::from_value::<DesktopFrame>(v["frame"].clone())
                            {
                                frame_tx.send_replace(Some(frame));
                            }
                        }
                        Some(Ok(Message::Close(_))) | None | Some(Err(_)) => break,
                        _ => {}
                    }
                }
            }
            let _ = sink.close().await;
        });
        Ok(Self {
            latest: frame_rx,
            input_tx,
            stop,
            target_label: if ticket.name.is_empty() {
                ticket.addr.clone()
            } else {
                ticket.name.clone()
            },
        })
    }

    pub fn latest_frame(&self) -> Option<DesktopFrame> {
        self.latest.borrow().clone()
    }

    pub async fn send_input(&self, event: InputEvent) -> Result<()> {
        self.input_tx
            .send(event)
            .await
            .map_err(|_| anyhow!("remote desktop viewer disconnected"))
    }

    pub fn target(&self) -> &str {
        &self.target_label
    }

    pub fn disconnect(&self) {
        self.stop.cancel();
    }
}

impl Drop for DesktopViewer {
    fn drop(&mut self) {
        self.stop.cancel();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn lan_host_and_viewer_stream_frames_and_input_over_pinned_tls() -> Result<()> {
        let received = Arc::new(Mutex::new(Vec::<InputEvent>::new()));
        let sink = received.clone();
        let host = DesktopHost::start(
            "127.0.0.1:0",
            "Test Host",
            Arc::new(move |ev| sink.lock().unwrap().push(ev)),
        )
        .await?;
        let link = host.ticket().link();
        let parsed = LanTicket::parse(&link)?;
        assert_eq!(parsed, *host.ticket());

        // Wrong fingerprint must be rejected.
        let mut bad = parsed.clone();
        bad.cert_sha256 = "0".repeat(64);
        assert!(DesktopViewer::connect_lan(&bad).await.is_err());

        // Valid ticket connects, receives frames, and delivers input events.
        let viewer = DesktopViewer::connect_lan(&parsed).await?;
        host.publish_frame(DesktopFrame {
            seq: 1,
            width: 1280,
            height: 800,
            mime: "image/png".into(),
            data_b64: "iVBORw0KGgo=".into(),
        });
        let mut got = None;
        for _ in 0..50 {
            if let Some(f) = viewer.latest_frame() {
                got = Some(f);
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        let frame = got.expect("viewer should receive published frame");
        assert_eq!(frame.seq, 1);
        assert_eq!(frame.width, 1280);
        assert!(frame.data_uri().starts_with("data:image/png;base64,"));

        viewer
            .send_input(InputEvent {
                kind: "mouse_move".into(),
                x: 0.5,
                y: 0.25,
                button: String::new(),
                dx: 0.0,
                dy: 0.0,
                key: String::new(),
                code: String::new(),
                text: String::new(),
                shift: false,
                ctrl: false,
                alt: false,
                meta: false,
            })
            .await?;
        for _ in 0..50 {
            if !received.lock().unwrap().is_empty() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        let events = received.lock().unwrap();
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].kind, "mouse_move");
        assert_eq!(events[0].x, 0.5);
        drop(events);

        viewer.disconnect();
        host.stop();
        Ok(())
    }
}
