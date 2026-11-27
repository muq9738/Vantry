use crate::{
    desktop::Desktop,
    pairing::{Pairings, matches_secret, token},
    transport::{Heartbeat, Stream, Tasks, Ws, accepted, send, upgrade},
    web::*,
};
use anyhow::{Result, ensure};
use chrono::{DateTime, Utc};
use futures::StreamExt;
use http::{Method, Request};
use http_body_util::BodyExt;
use hyper::body::Incoming;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::{HashMap, HashSet},
    net::{IpAddr, SocketAddr},
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    sync::{Semaphore, mpsc, oneshot},
    time::{Instant, timeout},
};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
use url::Url;

#[derive(Default, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default, deny_unknown_fields)]
pub struct SessionConfig {
    pub registration_key: String,
    pub host_credentials: Vec<Credential>,
    #[serde(deserialize_with = "null_default")]
    pub routes: HashMap<String, String>,
    pub http_upstreams: Vec<Upstream>,
    pub max_hosts: i32,
    pub max_connections: i32,
    pub max_connections_per_route: i32,
    pub max_pending_per_route: i32,
    pub trusted_proxies: Vec<String>,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Credential {
    pub key: String,
    pub routes: Vec<String>,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Upstream {
    pub host: String,
    pub prefix: String,
    pub target: String,
}
fn null_default<'de, T: Deserialize<'de> + Default, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<T, D::Error> {
    Ok(Option::<T>::deserialize(deserializer)?.unwrap_or_default())
}
fn valid_route(r: &str) -> bool {
    (16..=128).contains(&r.len())
        && r.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}
struct Network(IpAddr, u8);
impl Network {
    fn parse(s: &str) -> Result<Self> {
        let (ip, bits) = s
            .trim()
            .split_once('/')
            .map(|(a, b)| (a, Some(b)))
            .unwrap_or((s.trim(), None));
        let ip: IpAddr = ip.parse()?;
        let max = if ip.is_ipv4() { 32 } else { 128 };
        let bits = bits.map(str::parse).transpose()?.unwrap_or(max);
        ensure!(bits <= max, "invalid trusted proxy");
        Ok(Self(ip, bits))
    }
    fn contains(&self, ip: IpAddr) -> bool {
        match (self.0, ip.to_canonical()) {
            (IpAddr::V4(a), IpAddr::V4(b)) => {
                (u32::from(a) ^ u32::from(b)).leading_zeros() >= self.1.into()
            }
            (IpAddr::V6(a), IpAddr::V6(b)) => {
                (u128::from(a) ^ u128::from(b)).leading_zeros() >= self.1.into()
            }
            _ => false,
        }
    }
}
#[derive(Default)]
pub(crate) struct Rate(HashMap<IpAddr, (Instant, usize)>);
impl Rate {
    pub fn allow(&mut self, ip: IpAddr, max: usize) -> bool {
        if !self.0.contains_key(&ip) && self.0.len() >= 4096 {
            self.0
                .retain(|_, (t, _)| t.elapsed() < Duration::from_secs(60));
            if self.0.len() >= 4096 {
                return false;
            }
        }
        let window = self.0.entry(ip).or_insert((Instant::now(), 0));
        if window.0.elapsed() >= Duration::from_secs(60) {
            *window = (Instant::now(), 0)
        }
        window.1 += 1;
        window.1 <= max
    }
}
#[derive(Serialize, Deserialize)]
pub(crate) struct Open {
    pub id: String,
    pub channel: String,
}
enum Control {
    Open(Open),
    Probe(oneshot::Sender<()>),
}
struct Host {
    instance: String,
    sender: mpsc::Sender<Control>,
    closed: CancellationToken,
    seen: Mutex<Instant>,
    idle_http: Mutex<Vec<(Instant, Sender)>>,
    limits: Arc<RouteLimits>,
}
struct RouteLimits {
    streams: Arc<Semaphore>,
    pending: Arc<Semaphore>,
}
struct Waiting {
    route: String,
    ready: oneshot::Sender<Ws>,
}
#[derive(Default)]
struct Registry {
    hosts: HashMap<String, Arc<Host>>,
    limits: HashMap<String, std::sync::Weak<RouteLimits>>,
    pending: HashMap<String, Waiting>,
    connect_rate: Rate,
}
struct Cleanup<F: FnOnce()>(Option<F>);
impl<F: FnOnce()> Drop for Cleanup<F> {
    fn drop(&mut self) {
        if let Some(f) = self.0.take() {
            f()
        }
    }
}
#[derive(Default)]
struct Codes {
    store: Pairings,
    rate: Rate,
}
pub struct Relay {
    cfg: Option<SessionConfig>,
    desktop_key: Option<String>,
    desktop: Desktop,
    session_codes: Mutex<Codes>,
    desktop_codes: Mutex<Codes>,
    registry: Arc<Mutex<Registry>>,
    global: Arc<Semaphore>,
    proxies: Vec<Network>,
    client: reqwest::Client,
}
impl Relay {
    pub fn new(mut cfg: Option<SessionConfig>, desktop_key: Option<String>) -> Result<Self> {
        ensure!(
            cfg.is_some() || desktop_key.is_some(),
            "enable sessions or desktop"
        );
        if let Some(k) = &desktop_key {
            ensure!(k.len() >= 32, "invalid desktop registration key")
        };
        let mut proxies = vec![];
        let mut max = 256;
        if let Some(c) = &mut cfg {
            ensure!(
                !c.registration_key.is_empty() || !c.host_credentials.is_empty(),
                "missing session registration key"
            );
            let mut keys = HashSet::new();
            if !c.registration_key.is_empty() {
                ensure!(
                    c.registration_key.len() >= 32,
                    "invalid session registration key"
                );
                keys.insert(&c.registration_key);
            }
            for cred in &c.host_credentials {
                ensure!(
                    cred.key.len() >= 32
                        && keys.insert(&cred.key)
                        && !cred.routes.is_empty()
                        && cred.routes.iter().all(|r| valid_route(r)),
                    "invalid host credential"
                );
            }
            for u in &c.http_upstreams {
                let url = Url::parse(&u.target).ok();
                ensure!(
                    !u.host.is_empty()
                        && u.prefix.starts_with('/')
                        && url.is_some_and(|v| v.has_host()
                            && v.username().is_empty()
                            && v.password().is_none()
                            && matches!(v.scheme(), "http" | "https")),
                    "invalid HTTP upstream"
                );
            }
            for p in &c.trusted_proxies {
                proxies.push(Network::parse(p)?);
            }
            if c.max_hosts <= 0 {
                c.max_hosts = 64
            }
            if c.max_connections <= 0 {
                c.max_connections = 256
            }
            if c.max_connections_per_route <= 0 {
                c.max_connections_per_route = c.max_connections.min(64)
            }
            if c.max_pending_per_route <= 0 {
                c.max_pending_per_route = 16
            }
            max = c.max_connections;
        }
        Ok(Self {
            cfg,
            desktop_key,
            desktop: Desktop::new(),
            session_codes: Mutex::default(),
            desktop_codes: Mutex::default(),
            registry: Arc::default(),
            global: Arc::new(Semaphore::new(max as usize)),
            proxies,
            client: crate::transport::http_client()?,
        })
    }
    fn owner(&self, key: &str, route: Option<&str>) -> Option<String> {
        let c = self.cfg.as_ref()?;
        if !key.is_empty() && matches_secret(key, &c.registration_key) {
            return Some("global".into());
        }
        c.host_credentials
            .iter()
            .enumerate()
            .find(|(_, h)| {
                matches_secret(key, &h.key) && route.is_none_or(|r| h.routes.iter().any(|s| s == r))
            })
            .map(|(i, _)| i.to_string())
    }
    fn ip(&self, req: &Request<Incoming>, addr: SocketAddr) -> IpAddr {
        let peer = addr.ip().to_canonical();
        let trusted = |ip| self.proxies.iter().any(|n| n.contains(ip));
        if trusted(peer) {
            let values = req
                .headers()
                .get_all("x-forwarded-for")
                .iter()
                .filter_map(|s| s.to_str().ok())
                .flat_map(|s| s.split(','))
                .collect::<Vec<_>>();
            for s in values.into_iter().rev() {
                let Ok(ip) = s.trim().parse::<IpAddr>() else {
                    break;
                };
                if !trusted(ip) {
                    return ip.to_canonical();
                }
            }
            if let Some(ip) = req
                .headers()
                .get("x-real-ip")
                .and_then(|v| v.to_str().ok()?.trim().parse::<IpAddr>().ok())
            {
                return ip.to_canonical();
            }
        }
        peer
    }
    pub(crate) async fn handle(
        &self,
        mut req: Request<Incoming>,
        addr: SocketAddr,
        tasks: Tasks,
    ) -> Result<Reply, u16> {
        let path = req.uri().path().to_owned();
        if path == "/healthz" {
            return Ok(json(
                200,
                if self.cfg.is_some() {
                    json!({"service":"vantry-relay","protocol":1})
                } else {
                    json!({"service":"vantry-relay"})
                },
            ));
        }
        let ip = self.ip(&req, addr);
        let key = bearer(req.headers());
        if let (Some(path), Some(desktop_key)) = (path.strip_prefix("/desktop/"), &self.desktop_key)
        {
            if matches!(path, "pairings" | "pairings/redeem" | "pairings/revoke") {
                let owner = matches_secret(key, desktop_key).then_some("desktop".into());
                return Self::pair(req, path, "desktop", owner, ip, &self.desktop_codes).await;
            }
            return self.desktop.handle(req, desktop_key, tasks).await;
        }
        let cfg = self.cfg.as_ref().ok_or(404u16)?;
        let route = query(&req, "route");
        match path.as_str() {
            "/pairings" | "/pairings/revoke" | "/pairings/redeem" => {
                let owner = self.owner(key, None);
                Self::pair(req, &path[1..], "session", owner, ip, &self.session_codes).await
            }
            "/relay/host" => {
                self.owner(key, Some(&route)).ok_or(401u16)?;
                if !valid_route(&route) {
                    return Err(400);
                }
                let instance = query(&req, "instance");
                let (res, on) = upgrade(&mut req)?;
                let existing = self.registry.lock().unwrap().hosts.get(&route).cloned();
                if let Some(old) = existing {
                    if (instance.is_empty() || instance != old.instance)
                        && old.seen.lock().unwrap().elapsed() < Duration::from_secs(25)
                    {
                        return Err(409);
                    }
                    let (tx, rx) = oneshot::channel();
                    if old.sender.try_send(Control::Probe(tx)).is_ok()
                        && matches!(timeout(Duration::from_millis(1500), rx).await, Ok(Ok(())))
                    {
                        return Err(409);
                    }
                    old.closed.cancel();
                }
                let (tx, mut rx) = mpsc::channel(128);
                let host = Arc::new(Host {
                    sender: tx,
                    instance,
                    closed: CancellationToken::new(),
                    seen: Mutex::new(Instant::now()),
                    idle_http: Mutex::default(),
                    limits: {
                        let mut reg = self.registry.lock().unwrap();
                        reg.limits.retain(|_, w| w.strong_count() > 0);
                        let weak = reg.limits.entry(route.clone()).or_default();
                        if let Some(limits) = weak.upgrade() {
                            limits
                        } else {
                            let limits = Arc::new(RouteLimits {
                                streams: Arc::new(Semaphore::new(
                                    cfg.max_connections_per_route as usize,
                                )),
                                pending: Arc::new(Semaphore::new(
                                    cfg.max_pending_per_route as usize,
                                )),
                            });
                            *weak = Arc::downgrade(&limits);
                            limits
                        }
                    },
                });
                let registry = self.registry.clone();
                {
                    let mut reg = registry.lock().unwrap();
                    reg.hosts.retain(|_, h| !h.closed.is_cancelled());
                    if reg.hosts.contains_key(&route) {
                        return Err(409);
                    }
                    if reg.hosts.len() >= cfg.max_hosts as usize {
                        return Err(503);
                    }
                    reg.hosts.insert(route.clone(), host.clone());
                }
                let closing = host.clone();
                let guard = Cleanup(Some(move || {
                    closing.closed.cancel();
                    closing.idle_http.lock().unwrap().clear();
                    let mut r = registry.lock().unwrap();
                    if r.hosts
                        .get(&route)
                        .is_some_and(|h| Arc::ptr_eq(h, &closing))
                    {
                        r.hosts.remove(&route);
                    }
                }));
                tasks.spawn(async move {
                    let _registration = guard;
                    let Ok(mut ws) = accepted(on, 4096).await else {return};
                    let mut heartbeat = Heartbeat::new();
                    let mut probe = None;
                    loop {
                        let msg = tokio::select! {
                            _ = host.closed.cancelled() => break,
                            tick = heartbeat.tick() => {
                                if tick.is_err() { break }
                                host.idle_http.lock().unwrap().retain(|(t,s)|t.elapsed()<Duration::from_secs(30)&&!s.is_closed());
                                Message::Ping(Default::default())
                            }
                            operation = rx.recv() => match operation {
                                Some(Control::Open(open)) => Message::text(serde_json::to_string(&open).unwrap()),
                                Some(Control::Probe(tx)) => { probe = Some(tx); Message::Ping(bytes::Bytes::from_static(b"probe")) },
                                None => break,
                            },
                            frame = ws.next() => match frame {
                                Some(Ok(Message::Pong(data))) => {
                                    heartbeat.activity();
                                    *host.seen.lock().unwrap() = Instant::now();
                                    if data.as_ref() == b"probe" && let Some(reply) = probe.take() { let _ = reply.send(()); }
                                    continue;
                                }
                                Some(Ok(Message::Ping(data))) => Message::Pong(data),
                                Some(Ok(Message::Close(_))) | None | Some(Err(_)) => break,
                                _ => continue,
                            },
                        };
                        if send(&mut ws,msg).await.is_err() { break }
                    }
                });
                Ok(res)
            }
            "/relay/attach" => {
                self.owner(key, Some(&route)).ok_or(401u16)?;
                let id = query(&req, "id");
                let (res, on) = upgrade(&mut req)?;
                let mut reg = self.registry.lock().unwrap();
                if !reg.pending.get(&id).is_some_and(|p| p.route == route) {
                    return Err(404);
                }
                let wait = reg.pending.remove(&id).unwrap();
                tasks.spawn(async move {
                    if let Ok(ws) = accepted(on, 1 << 20).await {
                        let _ = wait.ready.send(ws);
                    }
                });
                Ok(res)
            }
            "/relay/connect" => {
                if req.method() != Method::GET {
                    return Err(405);
                }
                let (res, on) = upgrade(&mut req)?;
                if !valid_route(&route) {
                    return Err(400);
                }
                if !self.registry.lock().unwrap().connect_rate.allow(ip, 60) {
                    return Err(429);
                }
                let stream = self.open(&route, "core", &tasks).await?;
                tasks.spawn(async move {
                    if let Ok(ws) = accepted(on, 1 << 20).await {
                        let _ = crate::transport::bridge(ws, stream).await;
                    }
                });
                Ok(res)
            }
            _ => {
                let host = req
                    .headers()
                    .get("host")
                    .and_then(|v| v.to_str().ok())
                    .or_else(|| req.uri().authority().map(|a| a.as_str()))
                    .unwrap_or("")
                    .to_ascii_lowercase();
                let authority = host.parse::<http::uri::Authority>().ok();
                let hostname = authority.as_ref().map(|v| v.host()).unwrap_or(&host);
                if let Some(route) = cfg.routes.get(&host).or_else(|| cfg.routes.get(hostname)) {
                    let host = self
                        .registry
                        .lock()
                        .unwrap()
                        .hosts
                        .get(route)
                        .cloned()
                        .ok_or(502u16)?;
                    let cached = {
                        let mut idle = host.idle_http.lock().unwrap();
                        idle.retain(|(t, s)| {
                            t.elapsed() < Duration::from_secs(30) && !s.is_closed()
                        });
                        idle.pop().map(|(_, s)| s)
                    };
                    let mut tx = if let Some(tx) = cached {
                        tx
                    } else {
                        sender(self.open(route, "http", &tasks).await?, &tasks)
                            .await
                            .map_err(|_| 502u16)?
                    };
                    let reply = forward(
                        req.map(|b| b.map_err(Into::into).boxed_unsync()),
                        &mut tx,
                        &tasks,
                    )
                    .await
                    .map_err(|_| 502u16)?;
                    if reply.status() != 101 {
                        tasks.spawn(async move {tokio::select!{_=host.closed.cancelled()=>{},ready=tx.ready()=>{if ready.is_ok(){let mut idle=host.idle_http.lock().unwrap();if idle.len()<2{idle.push((Instant::now(),tx));}}}}});
                    }
                    return Ok(reply);
                }
                if let Some(u) = cfg
                    .http_upstreams
                    .iter()
                    .filter(|u| {
                        (u.host.eq_ignore_ascii_case(&host)
                            || u.host.eq_ignore_ascii_case(hostname))
                            && path.starts_with(&u.prefix)
                    })
                    .max_by_key(|u| u.prefix.len())
                {
                    return proxy_target(
                        req,
                        &Url::parse(&u.target).unwrap(),
                        &self.client,
                        &tasks,
                        addr,
                    )
                    .await
                    .map_err(|_| 502);
                }
                Err(404)
            }
        }
    }
    async fn open(&self, route: &str, channel: &str, tasks: &Tasks) -> Result<Stream, u16> {
        let id = token();
        let (tx, rx) = oneshot::channel();
        let (host, all, per, waiting) = {
            let mut reg = self.registry.lock().unwrap();
            let host = reg.hosts.get(route).cloned().ok_or(502u16)?;
            let all = self
                .global
                .clone()
                .try_acquire_owned()
                .map_err(|_| 429u16)?;
            let per = host
                .limits
                .streams
                .clone()
                .try_acquire_owned()
                .map_err(|_| 429u16)?;
            let waiting = host
                .limits
                .pending
                .clone()
                .try_acquire_owned()
                .map_err(|_| 429u16)?;
            reg.pending.insert(
                id.clone(),
                Waiting {
                    route: route.into(),
                    ready: tx,
                },
            );
            (host, all, per, waiting)
        };
        let registry = self.registry.clone();
        let key = id.clone();
        let _cleanup = Cleanup(Some(move || {
            registry.lock().unwrap().pending.remove(&key);
        }));
        host.sender
            .try_send(Control::Open(Open {
                id,
                channel: channel.into(),
            }))
            .map_err(|_| 503u16)?;
        let ws = tokio::select! {_=host.closed.cancelled()=>return Err(502),r=timeout(Duration::from_secs(15),rx)=>r.map_err(|_|504u16)?.map_err(|_|502u16)?};
        drop(waiting);
        let (a, b) = tokio::io::duplex(64 << 10);
        let limits = host.limits.clone();
        tasks.spawn(async move {
            let _permits = (all, per, limits);
            let _ = crate::transport::bridge(ws, b).await;
        });
        Ok(Box::new(a))
    }
    async fn pair(
        req: Request<Incoming>,
        path: &str,
        scope: &str,
        owner: Option<String>,
        ip: IpAddr,
        codes: &Mutex<Codes>,
    ) -> Result<Reply, u16> {
        if path == "pairings" && req.method() == Method::GET {
            let owner = owner.ok_or(401u16)?;
            return codes
                .lock()
                .unwrap()
                .store
                .info(&query(&req, "id"), &owner)
                .map(|e| json(200, e))
                .ok_or(404);
        }
        if req.method() != Method::POST {
            return Err(405);
        }
        if path == "pairings/redeem" {
            if !codes.lock().unwrap().rate.allow(ip, 20) {
                return Err(429);
            }
            #[derive(Deserialize)]
            struct Input {
                scope: String,
                code: String,
                #[serde(default)]
                name: String,
            }
            let v: Input = decode(req).await?;
            if v.scope != scope {
                return Err(400);
            }
            let e = codes
                .lock()
                .unwrap()
                .store
                .redeem(scope, &v.code, &v.name)
                .map_err(|_| 400u16)?;
            return Ok(json(200, e));
        }
        let owner = owner.ok_or(401u16)?;
        if path == "pairings/revoke" {
            #[derive(Deserialize)]
            struct Input {
                id: String,
            }
            let v: Input = decode(req).await?;
            return Ok(json(
                200,
                json!({"revoked":codes.lock().unwrap().store.revoke(&v.id,&owner)}),
            ));
        }
        #[derive(Deserialize)]
        #[serde(rename_all = "camelCase")]
        struct Issue {
            scope: String,
            #[serde(default)]
            name: String,
            payload: Value,
            expires_at: Option<DateTime<Utc>>,
        }
        let v: Issue = decode(req).await?;
        if v.scope != scope {
            return Err(400);
        }
        let e = codes
            .lock()
            .unwrap()
            .store
            .issue(scope, &v.name, v.payload, v.expires_at, &owner)
            .map_err(|_| 400u16)?;
        Ok(json(201, e))
    }
}
