use crate::web::{Reply, response};
use anyhow::{Result, anyhow, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use bytes::Bytes;
use futures::{SinkExt, StreamExt};
use http::{HeaderMap, Method, Version};
use hyper::{Request, body::Incoming};
use hyper_util::rt::TokioIo;
use std::{net::IpAddr, sync::Arc, time::Duration};
use tokio::{
    io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt, DuplexStream},
    sync::watch,
    time::{Instant, timeout},
};
use tokio_tungstenite::{
    WebSocketStream,
    tungstenite::{
        Message,
        handshake::{client::generate_key, derive_accept_key},
        protocol::{Role, WebSocketConfig},
    },
};
use tokio_util::sync::CancellationToken;
use url::Url;

pub trait Socket: AsyncRead + AsyncWrite + Unpin + Send + 'static {}
impl<T: AsyncRead + AsyncWrite + Unpin + Send + 'static> Socket for T {}
pub type Stream = Box<dyn Socket>;
pub type Ws = WebSocketStream<Stream>;
pub(crate) type Upgrade = hyper::upgrade::OnUpgrade;

/// All accepted connections and upgraded transports are joined on shutdown.
#[derive(Clone)]
pub(crate) struct Tasks {
    pub stop: CancellationToken,
    active: Arc<watch::Sender<()>>,
}
impl Tasks {
    pub fn new(stop: CancellationToken) -> Self {
        Self {
            stop,
            active: Arc::new(watch::channel(()).0),
        }
    }
    pub fn spawn(&self, future: impl Future<Output = ()> + Send + 'static) {
        let guard = self.active.subscribe();
        let stop = self.stop.clone();
        tokio::spawn(async move {
            let _guard = guard;
            tokio::select! {biased;_=stop.cancelled()=>{},_=future=>{}}
        });
    }
    pub fn owned(&self, future: impl Future<Output = ()> + Send + 'static) {
        let guard = self.active.subscribe();
        tokio::spawn(async move {
            let _guard = guard;
            future.await;
        });
    }
    pub async fn close(&self) {
        self.stop.cancel();
        self.active.closed().await;
    }
}
pub fn relay_url(raw: &str, allow_private: bool) -> Result<Url> {
    let u = Url::parse(raw.trim()).map_err(|_| anyhow!("invalid relay URL"))?;
    ensure!(
        u.has_host()
            && u.username().is_empty()
            && u.password().is_none()
            && u.query().is_none()
            && u.fragment().is_none(),
        "invalid relay URL"
    );
    let host = u.host_str().unwrap_or("").trim_matches(['[', ']']);
    ensure!(
        u.scheme() == "https" || u.scheme() == "http" && plain_host(host, allow_private),
        "HTTPS required"
    );
    Ok(u)
}
pub(crate) fn plain_host(host: &str, private: bool) -> bool {
    host == "localhost"
        || host.parse::<IpAddr>().is_ok_and(|ip| {
            ip.is_loopback()
                || private
                    && match ip {
                        IpAddr::V4(ip) => ip.is_private(),
                        IpAddr::V6(ip) => ip.is_unique_local(),
                    }
        })
}
pub(crate) fn http_client() -> Result<reqwest::Client> {
    Ok(reqwest::Client::builder()
        .http1_only()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(10))
        .build()?)
}
pub(crate) fn header_token(headers: &HeaderMap, name: &str, token: &str) -> bool {
    headers
        .get_all(name)
        .iter()
        .filter_map(|h| h.to_str().ok())
        .flat_map(|s| s.split(','))
        .any(|s| s.trim().eq_ignore_ascii_case(token))
}
fn config(limit: usize) -> WebSocketConfig {
    WebSocketConfig::default()
        .max_message_size(Some(limit))
        .max_frame_size(Some(limit))
        .write_buffer_size(0)
}
pub(crate) fn upgrade(req: &mut Request<Incoming>) -> Result<(Reply, Upgrade), u16> {
    let h = req.headers();
    if req.method() != Method::GET
        || req.version() != Version::HTTP_11
        || !header_token(h, "connection", "upgrade")
        || !header_token(h, "upgrade", "websocket")
        || h.get("sec-websocket-version").and_then(|x| x.to_str().ok()) != Some("13")
    {
        return Err(400);
    }
    if let Some(origin) = h.get("origin") {
        let origin = Url::parse(origin.to_str().map_err(|_| 403u16)?).map_err(|_| 403u16)?;
        let host = h.get("host").and_then(|v| v.to_str().ok()).unwrap_or("");
        if !origin[url::Position::BeforeHost..url::Position::AfterPort].eq_ignore_ascii_case(host) {
            return Err(403);
        }
    }
    let key = h.get("sec-websocket-key").ok_or(400u16)?;
    if !STANDARD.decode(key).is_ok_and(|v| v.len() == 16) {
        return Err(400);
    }
    let mut res = response(101, Bytes::new());
    let headers = res.headers_mut();
    headers.insert("connection", "Upgrade".parse().unwrap());
    headers.insert("upgrade", "websocket".parse().unwrap());
    headers.insert(
        "sec-websocket-accept",
        derive_accept_key(key.as_bytes()).parse().unwrap(),
    );
    Ok((res, hyper::upgrade::on(req)))
}
pub(crate) async fn accepted(on: Upgrade, limit: usize) -> Result<Ws> {
    let io = timeout(Duration::from_secs(10), on).await??;
    Ok(WebSocketStream::from_raw_socket(
        Box::new(TokioIo::new(io)) as Stream,
        Role::Server,
        Some(config(limit)),
    )
    .await)
}
#[derive(Debug, thiserror::Error)]
#[error("relay WebSocket handshake failed (HTTP {0})")]
pub struct HandshakeStatus(pub u16);
pub async fn websocket(
    client: &reqwest::Client,
    base: &Url,
    path: &str,
    query: &[(&str, &str)],
    bearer: Option<&str>,
    limit: usize,
) -> Result<Ws> {
    let mut url = base.clone();
    url.set_path(&format!("{}{}", base.path().trim_end_matches('/'), path));
    url.query_pairs_mut().extend_pairs(query);
    let key = generate_key();
    let mut req = client
        .get(url)
        .header("Upgrade", "websocket")
        .header("Connection", "Upgrade")
        .header("Sec-WebSocket-Version", "13")
        .header("Sec-WebSocket-Key", &key);
    if let Some(secret) = bearer {
        req = req.bearer_auth(secret)
    }
    let res = timeout(Duration::from_secs(12), req.send())
        .await?
        .map_err(|_| anyhow!("relay unavailable"))?;
    if res.status() != 101 {
        return Err(HandshakeStatus(res.status().as_u16()).into());
    }
    ensure!(
        header_token(res.headers(), "upgrade", "websocket")
            && header_token(res.headers(), "connection", "upgrade")
            && res
                .headers()
                .get("sec-websocket-accept")
                .and_then(|v| v.to_str().ok())
                == Some(&derive_accept_key(key.as_bytes())),
        "invalid WebSocket response"
    );
    let io = res
        .upgrade()
        .await
        .map_err(|_| anyhow!("relay upgrade failed"))?;
    Ok(
        WebSocketStream::from_raw_socket(Box::new(io) as Stream, Role::Client, Some(config(limit)))
            .await,
    )
}
pub(crate) async fn send(ws: &mut Ws, m: Message) -> Result<()> {
    timeout(Duration::from_secs(10), ws.send(m)).await??;
    Ok(())
}
/// Ping and pong travel on the same socket as the stream without changing application deadlines.
pub(crate) struct Heartbeat {
    interval: tokio::time::Interval,
    last: Instant,
}
impl Heartbeat {
    pub fn new() -> Self {
        Self {
            interval: tokio::time::interval_at(
                Instant::now() + Duration::from_secs(20),
                Duration::from_secs(20),
            ),
            last: Instant::now(),
        }
    }
    pub fn activity(&mut self) {
        self.last = Instant::now();
    }
    pub async fn tick(&mut self) -> Result<()> {
        self.interval.tick().await;
        if self.last.elapsed() > Duration::from_secs(65) {
            bail!("relay heartbeat expired")
        }
        Ok(())
    }
}
pub async fn bridge(ws: Ws, socket: impl Socket) -> Result<()> {
    let (mut sink, mut source) = ws.split();
    let (mut reader, mut writer) = tokio::io::split(socket);
    let (control, mut control_rx) = tokio::sync::mpsc::channel(4);
    let activity = std::sync::atomic::AtomicU64::new(0);
    let start = Instant::now();
    let receive = async {
        while let Some(message) = source.next().await {
            activity.store(
                start.elapsed().as_secs(),
                std::sync::atomic::Ordering::Relaxed,
            );
            match message? {
                Message::Binary(data) => writer.write_all(&data).await?,
                Message::Ping(data) => control.send(Message::Pong(data)).await?,
                Message::Close(_) => break,
                _ => {}
            }
        }
        Ok::<_, anyhow::Error>(())
    };
    let transmit = async {
        let mut buf = vec![0; 64 << 10];
        let period = Duration::from_secs(20);
        let mut timer = tokio::time::interval_at(start + period, period);
        loop {
            let message = tokio::select! {
                _ = timer.tick() => {
                    let last=activity.load(std::sync::atomic::Ordering::Relaxed);
                    ensure!(start.elapsed().as_secs().saturating_sub(last)<=65,"relay heartbeat expired");
                    Message::Ping(Bytes::new())
                }
                control = control_rx.recv() => match control {Some(frame)=>frame,None=>break},
                n = reader.read(&mut buf) => {let n=n?;if n==0{break}Message::binary(Bytes::copy_from_slice(&buf[..n]))}
            };
            timeout(Duration::from_secs(10), sink.send(message)).await??;
        }
        Ok::<_, anyhow::Error>(())
    };
    tokio::select! { result = receive => result, result = transmit => result }
}
pub(crate) fn byte_stream(ws: Ws, tasks: &Tasks) -> DuplexStream {
    let (a, b) = tokio::io::duplex(64 * 1024);
    tasks.spawn(async move {
        let _ = bridge(ws, b).await;
    });
    a
}
