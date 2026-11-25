//! Device-side relay registration, TLS routing and authenticated Core proxy.
use crate::{
    identity::{Configuration, IdentityStore, Pair, client_tls},
    pairing::token,
    relay::{Open, Rate},
    transport::{
        self, HandshakeStatus, Heartbeat, Stream, Tasks, byte_stream, relay_url, send, websocket,
    },
    web::*,
};
use anyhow::{Result, anyhow, bail, ensure};
use bytes::Bytes;
use futures::{StreamExt, stream::FuturesUnordered};
use http::{Method, Request, Uri};
use http_body_util::{BodyExt, Limited};
use hyper::{body::Incoming, service::service_fn};
use hyper_util::rt::TokioIo;
use rand::Rng;
use rustls::pki_types::ServerName;
use serde::Deserialize;
use serde_json::{Value, json};
use std::{
    convert::Infallible,
    net::SocketAddr,
    path::PathBuf,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    net::{TcpListener, TcpStream},
    sync::{Semaphore, watch},
    time::{Instant, timeout},
};
use tokio_rustls::{TlsAcceptor, TlsConnector};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
fn private_http() -> bool {
    std::env::var("VANTRY_ALLOW_INSECURE_LAN_RELAY").as_deref() == Ok("1")
}
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum HostState {
    Connecting,
    Online,
    Denied,
    Conflict,
    Reconnecting,
    Stopped,
}
pub struct HostConfig {
    pub relay: String,
    pub route: String,
    pub key: String,
    pub instance: String,
    /// Local authenticated TLS gateway. Never point this at an unprotected Core endpoint.
    pub gateway: SocketAddr,
    pub t3: Option<SocketAddr>,
}
impl HostConfig {
    pub async fn run(self, state: watch::Sender<HostState>, stop: CancellationToken) -> Result<()> {
        let base = relay_url(&self.relay, private_http())?;
        let client = transport::http_client()?;
        let instance = if self.instance.is_empty() {
            token()
        } else {
            self.instance.clone()
        };
        let tasks = Tasks::new(stop.child_token());
        let slots = Arc::new(Semaphore::new(128));
        let mut delay = 500;
        state.send_replace(HostState::Connecting);
        loop {
            let query = [
                ("route", self.route.as_str()),
                ("instance", instance.as_str()),
            ];
            let dial = websocket(&client, &base, "/relay/host", &query, Some(&self.key), 4096);
            let result = tokio::select! {_=stop.cancelled()=>break,r=dial=>r};
            match result {
                Ok(mut ws) => {
                    let start = Instant::now();
                    state.send_replace(HostState::Online);
                    let mut heartbeat = Heartbeat::new();
                    loop {
                        let frame = tokio::select! {_=stop.cancelled()=>break,t=heartbeat.tick()=>{if t.is_err()||send(&mut ws,Message::Ping(Bytes::new())).await.is_err(){break}continue},f=ws.next()=>f};
                        let Some(Ok(frame)) = frame else { break };
                        heartbeat.activity();
                        if let Message::Ping(b) = frame {
                            if send(&mut ws, Message::Pong(b)).await.is_err() {
                                break;
                            }
                            continue;
                        }
                        if frame.is_close() {
                            break;
                        }
                        if !frame.is_text() && !frame.is_binary() {
                            continue;
                        }
                        let Ok(open) = serde_json::from_slice::<Open>(&frame.into_data()) else {
                            break;
                        };
                        let target = match open.channel.as_str() {
                            "core" | "stream" => Some(self.gateway),
                            "http" | "t3" => self.t3,
                            _ => None,
                        };
                        let Some(target) = target else { continue };
                        let Ok(permit) = slots.clone().try_acquire_owned() else {
                            continue;
                        };
                        let (base, client, route, key) = (
                            base.clone(),
                            client.clone(),
                            self.route.clone(),
                            self.key.clone(),
                        );
                        tasks.spawn(async move {
                            let _permit = permit;
                            let setup = async {
                                let socket = TcpStream::connect(target).await?;
                                let ws = websocket(
                                    &client,
                                    &base,
                                    "/relay/attach",
                                    &[("route", &route), ("id", &open.id)],
                                    Some(&key),
                                    1 << 20,
                                )
                                .await?;
                                Ok::<_, anyhow::Error>((socket, ws))
                            };
                            if let Ok(Ok((socket, ws))) =
                                timeout(Duration::from_secs(15), setup).await
                            {
                                let _ = transport::bridge(ws, socket).await;
                            }
                        });
                    }
                    if start.elapsed() >= Duration::from_secs(30) {
                        delay = 500
                    }
                    state.send_replace(HostState::Reconnecting);
                }
                Err(e) => {
                    match e.downcast_ref::<HandshakeStatus>().map(|e| e.0) {
                        Some(401 | 403) => {
                            state.send_replace(HostState::Denied);
                            tasks.close().await;
                            return Ok(());
                        }
                        Some(409) => state.send_replace(HostState::Conflict),
                        _ => state.send_replace(HostState::Reconnecting),
                    };
                }
            }
            let wait = rand::thread_rng().gen_range(delay * 3 / 4..=delay * 5 / 4);
            tokio::select! {_=stop.cancelled()=>break,_=tokio::time::sleep(Duration::from_millis(wait))=>{}}
            delay = (delay * 2).min(15000);
        }
        tasks.close().await;
        state.send_replace(HostState::Stopped);
        Ok(())
    }
}

#[derive(Clone)]
pub enum Route {
    Auto,
    Direct,
    Relay,
}
pub struct PeerConnection {
    pub mode: &'static str,
    pub handshake: Duration,
    stream: Stream,
    tasks: Tasks,
    secret: String,
}
impl PeerConnection {
    pub async fn connect(
        pair: &Pair,
        identity: Option<&Configuration>,
        route: Route,
        stop: CancellationToken,
    ) -> Result<Self> {
        Self::connect_with_relay_delay(pair, identity, route, Duration::from_millis(450), stop)
            .await
    }

    pub(crate) async fn connect_with_relay_delay(
        pair: &Pair,
        identity: Option<&Configuration>,
        route: Route,
        auto_relay_delay: Duration,
        stop: CancellationToken,
    ) -> Result<Self> {
        let tls = TlsConnector::from(client_tls(pair, identity)?);
        let tasks = Tasks::new(stop.child_token());
        let client = transport::http_client()?;
        let start = Instant::now();
        let candidates = async {
            let mut attempts = FuturesUnordered::new();
            let mut targets = Vec::new();
            if !matches!(route, Route::Relay) {
                for (i, address) in pair.direct.iter().take(8).enumerate() {
                    targets.push(("direct", address.clone(), i as u64 * 150));
                }
            }
            if !matches!(route, Route::Direct) {
                let delay = if matches!(route, Route::Auto) && !pair.direct.is_empty() {
                    auto_relay_delay.as_millis() as u64
                } else {
                    0
                };
                for (i, url) in std::iter::once(&pair.relay).chain(&pair.relays).filter(|url| !url.is_empty()).take(8).enumerate() {
                    if !targets.iter().any(|(mode, target, _)| *mode == "relay" && target == url) {
                        targets.push(("relay", url.clone(), delay + i as u64 * 150));
                    }
                }
            }
            let slots = Arc::new(Semaphore::new(4));
            for (mode, target, delay) in targets {
                let tls = tls.clone();
                let tasks = &tasks;
                let client = &client;
                let slots = slots.clone();
                attempts.push(async move {
                    tokio::time::sleep(Duration::from_millis(delay)).await;
                    let _permit = if mode == "direct" {
                        Some(slots.acquire().await?)
                    } else {
                        None
                    };
                    let dial = async {
                        let raw: Stream = match mode {
                            "direct" => Box::new(TcpStream::connect(target).await?),
                            _ => {
                                let u = relay_url(&target, private_http())?;
                                let ws = websocket(
                                    client,
                                    &u,
                                    "/relay/connect",
                                    &[("route", &pair.route)],
                                    None,
                                    1 << 20,
                                )
                                .await?;
                                Box::new(byte_stream(ws, tasks))
                            }
                        };
                        let socket = tls
                            .connect(ServerName::try_from("vantry.remote")?, raw)
                            .await?;
                        Ok::<_, anyhow::Error>((mode, Box::new(socket) as Stream))
                    };
                    timeout(Duration::from_secs(12), dial).await?
                });
            }
            while let Some(result) = attempts.next().await {
                if result.is_ok() {
                    return result;
                }
            }
            bail!("peer unavailable or TLS authentication failed")
        };
        let (mode, stream) = tokio::select! {_=stop.cancelled()=>bail!("connection cancelled"),v=timeout(Duration::from_secs(15),candidates)=>v??};
        Ok(Self {
            mode,
            stream,
            tasks,
            handshake: start.elapsed(),
            secret: pair.secret.clone(),
        })
    }
    pub async fn request(
        self,
        method: Method,
        path: &str,
        value: Option<Value>,
    ) -> Result<hyper::Response<Incoming>> {
        let uri: Uri = path.parse()?;
        ensure!(
            path.starts_with('/') && uri.authority().is_none(),
            "invalid Core path"
        );
        let (req, body) = (
            Request::builder()
                .method(method)
                .uri(uri)
                .header("host", "vantry.remote")
                .header("content-type", "application/json"),
            value.map(|v| v.to_string()).unwrap_or_default(),
        );
        let req = if self.secret.is_empty() {
            req
        } else {
            req.header("authorization", format!("Bearer {}", self.secret))
        };
        let (mut tx, conn) =
            hyper::client::conn::http1::handshake(TokioIo::new(self.stream)).await?;
        self.tasks.spawn(async move {
            let _ = conn.with_upgrades().await;
        });
        Ok(tx.send_request(req.body(full(body.into()))?).await?)
    }
    pub async fn get_json(self, path: &str) -> Result<Value> {
        let res = self.request(Method::GET, path, None).await?;
        let status = res.status();
        let bytes = Limited::new(res.into_body(), 256 << 20)
            .collect()
            .await
            .map_err(|_| anyhow!("invalid Core response body"))?
            .to_bytes();
        if !status.is_success() {
            if let Ok(err) = serde_json::from_slice::<Value>(&bytes)
                && let Some(msg) = err["message"].as_str()
            {
                bail!("Core rejected request (HTTP {status}): {msg}");
            }
            bail!("Core rejected request (HTTP {status})");
        }
        Ok(serde_json::from_slice(&bytes)?)
    }
    pub async fn post_json(self, path: &str, value: Value) -> Result<Value> {
        let res = self.request(Method::POST, path, Some(value)).await?;
        let status = res.status();
        let bytes = Limited::new(res.into_body(), 256 << 20)
            .collect()
            .await
            .map_err(|_| anyhow!("invalid Core response body"))?
            .to_bytes();
        if !status.is_success() {
            if let Ok(err) = serde_json::from_slice::<Value>(&bytes)
                && let Some(msg) = err["message"].as_str()
            {
                bail!("Core rejected request (HTTP {status}): {msg}");
            }
            bail!("Core rejected request (HTTP {status})");
        }
        Ok(serde_json::from_slice(&bytes)?)
    }
    pub async fn subscribe_events(self, after: i64) -> Result<PeerEventStream> {
        let tasks = self.tasks.clone();
        let res = self
            .request(Method::GET, &format!("/v1/events?after={after}"), None)
            .await?;
        let status = res.status();
        if !status.is_success() {
            let bytes = Limited::new(res.into_body(), 64 << 10)
                .collect()
                .await
                .map(|b| b.to_bytes())
                .unwrap_or_default();
            if let Ok(err) = serde_json::from_slice::<Value>(&bytes)
                && let Some(msg) = err["message"].as_str()
            {
                bail!("Core rejected event stream (HTTP {status}): {msg}");
            }
            bail!("Core rejected event stream (HTTP {status})");
        }
        Ok(PeerEventStream {
            body: res.into_body(),
            buf: Vec::new(),
            _tasks: tasks,
        })
    }
    pub async fn proxy_http(self, mut req: Request<Incoming>) -> Result<Reply> {
        if !self.secret.is_empty() {
            req.headers_mut().insert(
                "authorization",
                http::HeaderValue::from_str(&format!("Bearer {}", self.secret))?,
            );
        }
        req.headers_mut()
            .insert("host", http::HeaderValue::from_static("vantry.remote"));
        let req = req.map(|b| b.map_err(Into::into).boxed_unsync());
        proxy(req, self.stream, &self.tasks).await
    }
    pub async fn check_update(self) -> Result<Value> {
        let response = self
            .request(
                Method::POST,
                "/v1/workbench",
                Some(json!({"action":"update.check"})),
            )
            .await?;
        ensure!(response.status().is_success(), "peer update query failed");
        let body = Limited::new(response.into_body(), 1 << 20)
            .collect()
            .await
            .map_err(|_| anyhow!("invalid update response"))?;
        Ok(serde_json::from_slice(&body.to_bytes())?)
    }
    pub async fn version(self) -> Result<Value> {
        self.get_json("/v1/status").await
    }
    pub async fn health(self) -> Result<Value> {
        self.get_json("/v1/health").await
    }
}

pub struct PeerEventStream {
    body: Incoming,
    buf: Vec<u8>,
    _tasks: Tasks,
}

impl PeerEventStream {
    pub async fn next_event(&mut self) -> Result<Option<vantry_protocol::CoreEvent>> {
        fn parse_line(trimmed: &str) -> Result<vantry_protocol::CoreEvent> {
            let val: Value = serde_json::from_str(trimmed)?;
            if let Some(err) = val.get("error") {
                let msg = err["message"].as_str().unwrap_or("event stream error");
                bail!("{msg}");
            }
            if let Some(ev) = val.get("event") {
                return Ok(serde_json::from_value(ev.clone())?);
            }
            Ok(serde_json::from_value(val)?)
        }
        loop {
            if let Some(pos) = self.buf.iter().position(|&b| b == b'\n') {
                let line: Vec<u8> = self.buf.drain(..=pos).collect();
                let trimmed = std::str::from_utf8(&line)?.trim();
                if trimmed.is_empty() {
                    continue;
                }
                return Ok(Some(parse_line(trimmed)?));
            }
            match self.body.frame().await {
                Some(Ok(frame)) => {
                    if let Ok(data) = frame.into_data() {
                        self.buf.extend_from_slice(&data);
                    }
                }
                Some(Err(e)) => return Err(anyhow!("peer event stream error: {e}")),
                None => {
                    if self.buf.is_empty() {
                        return Ok(None);
                    }
                    let tail = std::mem::take(&mut self.buf);
                    let trimmed = std::str::from_utf8(&tail)?.trim();
                    if trimmed.is_empty() {
                        return Ok(None);
                    }
                    return Ok(Some(parse_line(trimmed)?));
                }
            }
        }
    }
}
pub async fn redeem(
    bootstrap: &Pair,
    local: &IdentityStore,
    name: &str,
    route: Route,
    stop: CancellationToken,
) -> Result<String> {
    bootstrap.validate(private_http())?;
    ensure!(bootstrap.v == 2, "expected pairing invitation");
    let c = PeerConnection::connect(bootstrap, None, route, stop).await?;
    let res=c.request(Method::POST,"/pairings/redeem",Some(json!({"code":bootstrap.code,"name":name,"scope":"session","certificate":local.snapshot().host.certificate}))).await?;
    ensure!(res.status() == 200, "device rejected pairing");
    let v: Value = serde_json::from_slice(
        &Limited::new(res.into_body(), 32 << 10)
            .collect()
            .await
            .map_err(|_| anyhow!("invalid pairing response body"))?
            .to_bytes(),
    )?;
    let p: Pair = serde_json::from_value(v["payload"].clone())
        .map_err(|_| anyhow!("invalid pairing response"))?;
    ensure!(
        p.v == 1
            && p.route == bootstrap.route
            && (bootstrap.device_id.is_empty() || bootstrap.device_id == p.device_id)
            && (p.certificate == bootstrap.certificate
                || p.certificate == bootstrap.next_certificate),
        "peer identity changed"
    );
    local.save_peer(p)
}

struct PeerProxyPool {
    cfg: Configuration,
    pair: Pair,
    route: Route,
    stop: CancellationToken,
    idle: Mutex<std::collections::VecDeque<(Instant, Sender, Tasks)>>,
    relay_fast_until: Mutex<Option<Instant>>,
}

impl PeerProxyPool {
    fn new(cfg: Configuration, pair: Pair, route: Route, stop: CancellationToken) -> Arc<Self> {
        Arc::new(Self {
            cfg,
            pair,
            route,
            stop,
            idle: Mutex::default(),
            relay_fast_until: Mutex::default(),
        })
    }

    fn relay_delay(&self) -> Duration {
        let fast = self
            .relay_fast_until
            .lock()
            .unwrap()
            .is_some_and(|until| Instant::now() < until);
        if fast {
            Duration::ZERO
        } else {
            Duration::from_millis(450)
        }
    }

    async fn dial(&self) -> Result<PeerConnection> {
        let conn = PeerConnection::connect_with_relay_delay(
            &self.pair,
            Some(&self.cfg),
            self.route.clone(),
            self.relay_delay(),
            self.stop.clone(),
        )
        .await?;
        if conn.mode == "relay" {
            *self.relay_fast_until.lock().unwrap() =
                Some(Instant::now() + Duration::from_secs(60));
        } else {
            *self.relay_fast_until.lock().unwrap() = None;
        }
        Ok(conn)
    }

    fn pop_idle(&self) -> Option<(Sender, Tasks)> {
        let mut idle = self.idle.lock().unwrap();
        while let Some((at, tx, tasks)) = idle.pop_front() {
            if at.elapsed() < Duration::from_secs(45) && !tx.is_closed() {
                return Some((tx, tasks));
            }
        }
        None
    }

    fn recycle(self: &Arc<Self>, mut tx: Sender, tasks: Tasks) {
        let pool = Arc::clone(self);
        let stop = self.stop.clone();
        tokio::spawn(async move {
            tokio::select! {
                _ = stop.cancelled() => {}
                ready = tx.ready() => {
                    if ready.is_ok() {
                        let mut idle = pool.idle.lock().unwrap();
                        idle.retain(|(at, s, _)| at.elapsed() < Duration::from_secs(45) && !s.is_closed());
                        if idle.len() >= 4 {
                            idle.pop_front();
                        }
                        idle.push_back((Instant::now(), tx, tasks));
                    }
                }
            }
        });
    }

    async fn handle(self: Arc<Self>, mut req: Request<Incoming>) -> Reply {
        let is_stream = req.uri().path() == "/v1/events"
            || req.headers().contains_key("upgrade");
        if is_stream {
            return match self.dial().await {
                Ok(conn) => conn.proxy_http(req).await.unwrap_or_else(|e| {
                    json(502, json!({"code":"not_ready","message":e.to_string()}))
                }),
                Err(e) => json(503, json!({"code":"not_ready","message":e.to_string()})),
            };
        }

        if !self.pair.secret.is_empty()
            && let Ok(val) = http::HeaderValue::from_str(&format!("Bearer {}", self.pair.secret))
        {
            req.headers_mut().insert("authorization", val);
        }
        req.headers_mut()
            .insert("host", http::HeaderValue::from_static("vantry.remote"));

        let (parts, body) = req.into_parts();
        let body_bytes = match Limited::new(body, 16 << 20).collect().await {
            Ok(collected) => collected.to_bytes(),
            Err(_) => {
                return json(
                    413,
                    json!({"code":"invalid_command","message":"request body too large"}),
                );
            }
        };

        let build = |parts: &http::request::Parts, bytes: &Bytes| {
            let mut builder = Request::builder()
                .method(parts.method.clone())
                .uri(parts.uri.clone())
                .version(parts.version);
            if let Some(headers) = builder.headers_mut() {
                *headers = parts.headers.clone();
            }
            builder.body(full(bytes.clone())).unwrap()
        };

        if let Some((mut tx, tasks)) = self.pop_idle() {
            if let Ok(reply) = forward(build(&parts, &body_bytes), &mut tx, &tasks).await {
                if reply.status() != 101 {
                    self.recycle(tx, tasks);
                }
                return reply;
            }
        }

        let conn = match self.dial().await {
            Ok(conn) => conn,
            Err(e) => return json(503, json!({"code":"not_ready","message":e.to_string()})),
        };
        let tasks = conn.tasks;
        let mut tx = match sender(conn.stream, &tasks).await {
            Ok(tx) => tx,
            Err(e) => return json(502, json!({"code":"not_ready","message":e.to_string()})),
        };
        match forward(build(&parts, &body_bytes), &mut tx, &tasks).await {
            Ok(reply) => {
                if reply.status() != 101 {
                    self.recycle(tx, tasks);
                }
                reply
            }
            Err(e) => json(502, json!({"code":"not_ready","message":e.to_string()})),
        }
    }
}

/// Expose a local Core endpoint (`socket`: Unix socket on Unix, token-authenticated
/// loopback TCP descriptor on Windows) that proxies all HTTP/NDJSON calls to a paired peer.
pub async fn serve_peer_proxy(
    socket: PathBuf,
    cfg: Configuration,
    pair: Pair,
    route: Route,
    stop: CancellationToken,
) -> Result<()> {
    let pool = PeerProxyPool::new(cfg, pair, route, stop.clone());

    #[cfg(unix)]
    {
        let _ = std::fs::remove_file(&socket);
        let listener = tokio::net::UnixListener::bind(&socket)?;
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&socket, std::fs::Permissions::from_mode(0o600))?;
        loop {
            let (conn, _) = tokio::select! {
                _ = stop.cancelled() => break,
                r = listener.accept() => r?,
            };
            let pool = Arc::clone(&pool);
            tokio::spawn(async move {
                let svc = service_fn(move |req| {
                    let pool = Arc::clone(&pool);
                    async move { Ok::<_, Infallible>(pool.handle(req).await) }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(conn), svc)
                    .await;
            });
        }
        let _ = std::fs::remove_file(&socket);
    }

    #[cfg(not(unix))]
    {
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let tok = format!(
            "{}{}",
            uuid::Uuid::new_v4().simple(),
            uuid::Uuid::new_v4().simple()
        );
        let ep = Arc::new(vantry_protocol::LocalTcpEndpoint::new(
            listener.local_addr()?.to_string(),
            tok,
        ));
        std::fs::write(&socket, serde_json::to_vec(ep.as_ref())?)?;
        loop {
            let (conn, _) = tokio::select! {
                _ = stop.cancelled() => break,
                r = listener.accept() => r?,
            };
            let pool = Arc::clone(&pool);
            let ep = ep.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req| {
                    let authorized = req
                        .headers()
                        .get(vantry_protocol::LOCAL_TOKEN_HEADER)
                        .and_then(|v| v.to_str().ok())
                        .is_some_and(|t| ep.verify_token(t));
                    let pool = Arc::clone(&pool);
                    async move {
                        if !authorized {
                            return Ok::<_, Infallible>(response(401, Bytes::new()));
                        }
                        Ok::<_, Infallible>(pool.handle(req).await)
                    }
                });
                let _ = hyper::server::conn::http1::Builder::new()
                    .serve_connection(TokioIo::new(conn), svc)
                    .await;
            });
        }
        let _ = std::fs::remove_file(&socket);
    }
    Ok(())
}

pub async fn serve_gateway(
    listener: TcpListener,
    identity: Arc<IdentityStore>,
    core: PathBuf,
    mtls: bool,
    stop: CancellationToken,
) -> Result<()> {
    serve_gateway_with_t3(listener, identity, core, mtls, None, stop).await
}
/// The optional loopback T3 adapter enforces its own device and WebSocket ticket authentication.
pub async fn serve_gateway_with_t3(
    listener: TcpListener,
    identity: Arc<IdentityStore>,
    core: PathBuf,
    mtls: bool,
    t3: Option<SocketAddr>,
    stop: CancellationToken,
) -> Result<()> {
    ensure!(
        t3.is_none_or(|a| a.ip().is_loopback()),
        "T3 backend must be local"
    );
    let tasks = Tasks::new(stop.clone());
    let rates = Arc::new(Mutex::new(Rate::default()));
    let result = loop {
        let accepted = tokio::select! {_=stop.cancelled()=>break Ok(()),v=listener.accept()=>v};
        let (socket, peer) = match accepted {
            Ok(v) => v,
            Err(e) => break Err(e.into()),
        };
        let (identity, core, rates, group) =
            (identity.clone(), core.clone(), rates.clone(), tasks.clone());
        tasks.spawn(async move {
            let Ok(cfg) = identity.snapshot().server_tls(mtls) else {
                return;
            };
            let Ok(Ok(tls)) = timeout(
                Duration::from_secs(10),
                TlsAcceptor::from(cfg).accept(socket),
            )
            .await
            else {
                return;
            };
            let handler = service_fn(move |req: Request<Incoming>| {
                let (identity, core, rates, group) =
                    (identity.clone(), core.clone(), rates.clone(), group.clone());
                async move {
                    Ok::<_, Infallible>(
                        gateway_request(req, identity, core, t3, peer, rates, group)
                            .await
                            .unwrap_or_else(error),
                    )
                }
            });
            let _ = http_server()
                .serve_connection_with_upgrades(TokioIo::new(tls), handler)
                .await;
        });
    };
    tasks.close().await;
    result
}
async fn gateway_request(
    mut req: Request<Incoming>,
    identity: Arc<IdentityStore>,
    core: PathBuf,
    t3: Option<SocketAddr>,
    peer: SocketAddr,
    rates: Arc<Mutex<Rate>>,
    group: Tasks,
) -> Result<Reply, u16> {
    let path = req.uri().path().to_owned();
    if path.contains('%')
        || path.contains('\\')
        || path.contains("//")
        || path.split('/').any(|s| s == ".." || s == ".")
    {
        return Err(400);
    }
    if let Some(address) = t3
        && (path.starts_with("/api/")
            || matches!(
                path.as_str(),
                "/.well-known/t3/environment" | "/oauth/token" | "/ws"
            ))
    {
        let io = TcpStream::connect(address).await.map_err(|_| 502u16)?;
        return proxy(
            req.map(|b| b.map_err(Into::into).boxed_unsync()),
            Box::new(io),
            &group,
        )
        .await
        .map_err(|_| 502);
    }
    if path == "/pairings/redeem" {
        if req.method() != Method::POST {
            return Err(405);
        }
        if !rates.lock().unwrap().allow(peer.ip(), 20) {
            return Err(429);
        }
        #[derive(Deserialize)]
        #[serde(default)]
        #[derive(Default)]
        struct Input {
            code: String,
            name: String,
            scope: String,
            certificate: String,
        }
        let v: Input = decode(req).await?;
        if !v.scope.is_empty() && v.scope != "session" {
            return Err(400);
        }
        return identity
            .redeem(&v.code, &v.name, &v.certificate)
            .map(|v| json(200, v))
            .map_err(|_| 400);
    }
    let revoke = identity.authorize(bearer(req.headers())).ok_or(401u16)?;
    if path.starts_with("/v1/admin/") || path == "/v1/admin" {
        return Err(403);
    }
    if path == "/v1/access/identity" && req.method() == Method::GET {
        return Ok(json(200, identity.public_identity()));
    }
    req.headers_mut().remove("authorization");
    let mut request = if path == "/v1/workbench" {
        let (parts, b) = req.into_parts();
        let data = Limited::new(b, 16 << 20)
            .collect()
            .await
            .map_err(|_| 413u16)?
            .to_bytes();
        let command: Value = serde_json::from_slice(&data).map_err(|_| 400u16)?;
        let mut actions = command
            .as_object()
            .ok_or(400u16)?
            .iter()
            .filter(|(key, _)| key.eq_ignore_ascii_case("action"));
        let action = actions.next().and_then(|(_, v)| v.as_str()).ok_or(400u16)?;
        if actions.next().is_some() {
            return Err(400);
        }
        if action.starts_with("rd.") || action.starts_with("plugins.") {
            return Err(403);
        }
        Request::from_parts(parts, full(data))
    } else {
        req.map(|b| b.map_err(Into::into).boxed_unsync())
    };
    let io: crate::transport::Stream = if let Ok(bytes) = std::fs::read(&core)
        && let Some(ep) = vantry_protocol::LocalTcpEndpoint::parse_bytes(&bytes)
    {
        if let Ok(val) = http::HeaderValue::from_str(&ep.token) {
            request
                .headers_mut()
                .insert(vantry_protocol::LOCAL_TOKEN_HEADER, val);
        }
        Box::new(
            tokio::net::TcpStream::connect(&ep.addr)
                .await
                .map_err(|_| 502u16)?,
        )
    } else {
        #[cfg(unix)]
        {
            Box::new(
                tokio::net::UnixStream::connect(core)
                    .await
                    .map_err(|_| 502u16)?,
            )
        }
        #[cfg(not(unix))]
        {
            return Err(502);
        }
    };
    let (a, mut b) = tokio::io::duplex(64 << 10);
    group.spawn(async move {
        let mut io = io;
        tokio::select! {
            _ = revoke.cancelled() => {},
            _ = tokio::io::copy_bidirectional(&mut b, &mut io) => {}
        }
    });
    proxy(request, Box::new(a), &group).await.map_err(|_| 502)
}
