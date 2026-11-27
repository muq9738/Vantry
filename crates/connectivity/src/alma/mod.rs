//! Alma relay registration for mobile E2E sessions and native desktop TLS streams.
mod bootstrap;
mod chat;
mod crypto;
mod lan;
pub use crypto::new_code;

pub const DEVICE_NAME: &str = "vantry";

use crate::transport::{self, Ws};
use anyhow::{Result, anyhow, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use chat::{Chat, Scope};
use futures::{SinkExt, StreamExt};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    net::SocketAddr,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpStream,
    sync::{mpsc, watch},
    task::{AbortHandle, JoinSet},
    time::{Instant, interval, timeout},
};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
use url::Url;
use vantry_client::{ClientError, CoreClient};

const MAX_FRAME: usize = 4 << 20;
const HANDSHAKE: Duration = Duration::from_secs(15);

#[derive(Clone, Copy, Default, PartialEq)]
enum Registration {
    #[default]
    Offline,
    Online,
    Rejected,
}

#[derive(Default)]
pub struct Activity {
    relay: watch::Sender<Registration>,
    phones: AtomicUsize,
}

impl Activity {
    pub fn relay_online(&self) -> bool {
        *self.relay.borrow() == Registration::Online
    }
    pub fn rejected(&self) -> bool {
        *self.relay.borrow() == Registration::Rejected
    }
    pub async fn wait_for_registration(&self) -> Result<()> {
        let mut state = self.relay.subscribe();
        let outcome = timeout(HANDSHAKE, state.wait_for(|r| *r != Registration::Offline))
            .await.map_err(|_| anyhow!("Alma 中继注册超时"))??;
        ensure!(*outcome == Registration::Online, "Alma 中继拒绝注册，请核对注册凭据");
        Ok(())
    }
    pub fn phones(&self) -> usize {
        self.phones.load(Ordering::Relaxed)
    }
}

struct ConnectedPhone(Arc<Activity>);
impl Drop for ConnectedPhone {
    fn drop(&mut self) {
        self.0.phones.fetch_sub(1, Ordering::Relaxed);
    }
}

#[derive(Clone)]
enum Endpoint {
    Phone(Scope),
    Desktop { path: String, gateway: SocketAddr },
}

impl Endpoint {
    fn accepts(&self, path: &str) -> bool {
        match self {
            Self::Phone(_) => path == "/__alma_e2e__",
            Self::Desktop { path: expected, .. } => path == expected,
        }
    }
}

/// Base URL used by native clients, which append their usual TLS tunnel path.
pub fn desktop_client_url(relay: &Url, code: &str) -> Result<Url> {
    let mut client = relay.clone();
    client.path_segments_mut().map_err(|_| anyhow!("Invalid Alma relay URL"))?
        .pop_if_empty().push("c").push(&crypto::routing(code)?);
    Ok(client)
}

pub fn is_desktop_client_url(raw: &str) -> bool {
    let Ok(url) = transport::relay_url(raw, false) else { return false; };
    let Some(mut path) = url.path_segments() else { return false; };
    let Some(token) = path.next_back() else { return false; };
    path.next_back() == Some("c") && token.len() == 22
        && token.bytes().all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_'))
}

#[derive(Clone)]
pub struct Host {
    relay: Url,
    registration: String,
    code: Arc<str>,
    key: Option<String>,
    endpoint: Endpoint,
    pub activity: Arc<Activity>,
}

impl Host {
    pub fn new(
        relay: Url,
        code: String,
        key: Option<String>,
        pairing: String,
        thread: String,
        core: CoreClient,
    ) -> Result<Self> {
        Ok(Self {
            registration: crypto::registration(&code)?,
            relay,
            code: code.into(),
            key,
            activity: Default::default(),
            endpoint: Endpoint::Phone(Scope {
                pairing,
                thread,
                core,
                lan: None,
            }),
        })
    }

    pub fn desktop(relay: Url, code: String, key: Option<String>, route: &str,
                   gateway: SocketAddr, activity: Arc<Activity>) -> Result<Self> {
        let query = url::form_urlencoded::Serializer::new(String::new())
            .append_pair("route", route).finish();
        Ok(Self {
            registration: crypto::registration(&code)?,
            relay, code: code.into(), key, activity,
            endpoint: Endpoint::Desktop { path: format!("/relay/connect?{query}"), gateway },
        })
    }

    pub async fn run(self, stop: CancellationToken) -> Result<()> {
        let client = transport::http_client()?;
        let mut delay = Duration::from_secs(1);
        loop {
            let start = Instant::now();
            let result = tokio::select! {
                _ = stop.cancelled() => Ok(()),
                result = self.connection(&client) => result,
            };
            if !self.activity.rejected() {
                self.activity.relay.send_replace(Registration::Offline);
            }
            if result.is_ok() {
                return Ok(());
            }
            tracing::warn!("Alma relay disconnected");
            if start.elapsed() > Duration::from_secs(30) {
                delay = Duration::from_secs(1);
            }
            tokio::select! { _ = stop.cancelled() => return Ok(()), _ = tokio::time::sleep(delay) => {} }
            delay = (delay * 2).min(Duration::from_secs(30));
        }
    }

    async fn connection(&self, client: &reqwest::Client) -> Result<()> {
        let mut ws = transport::websocket(
            client,
            &self.relay,
            "/agent",
            &[("th", &self.registration)],
            None,
            MAX_FRAME,
        )
        .await?;
        let mut register = json!({"t":"register","tokenHash":self.registration,"name":DEVICE_NAME,"ver":env!("CARGO_PKG_VERSION"),"proto":1});
        if let Some(key) = &self.key {
            register["key"] = key.as_str().into();
        }
        write(&mut ws, register).await?;
        let (out, mut replies) = mpsc::channel::<Reply>(32);
        let mut sessions = HashMap::<String, Phone>::new();
        let mut tasks = JoinSet::new();
        let mut next_session = 0u64;
        let mut registered = false;
        let mut last_frame = Instant::now();
        let registration_deadline = last_frame + HANDSHAKE;
        let mut heartbeat = interval(Duration::from_secs(15));
        loop {
            tokio::select! {
                _ = tokio::time::sleep_until(registration_deadline), if !registered => bail!("Alma registration timed out"),
                _ = heartbeat.tick() => {
                    ensure!(last_frame.elapsed() < Duration::from_secs(55), "Alma relay timed out");
                    timeout(HANDSHAKE, ws.send(Message::Ping(Default::default()))).await??;
                }
                Some(_) = tasks.join_next(), if !tasks.is_empty() => {}
                Some(reply) = replies.recv() => {
                    if sessions.get(&reply.id).is_some_and(|p| p.number == reply.number) {
                        if reply.frame["t"] == "wclose" { sessions.remove(&reply.id); }
                        write(&mut ws, reply.frame).await?;
                    }
                }
                packet = ws.next() => {
                    let msg = packet.ok_or_else(|| anyhow!("Alma relay closed"))??;
                    last_frame = Instant::now();
                    let text = match msg {
                        Message::Text(s) => s,
                        Message::Ping(_) => { timeout(HANDSHAKE, ws.flush()).await??; continue; }
                        Message::Pong(_) => continue,
                        Message::Close(info) => {
                            if info.is_some_and(|f| matches!(u16::from(f.code), 4000 | 4002 | 4003 | 4401)) {
                                self.activity.relay.send_replace(Registration::Rejected);
                                tracing::warn!("Alma registration rejected or replaced");
                                return Ok(());
                            }
                            bail!("Alma relay closed");
                        }
                        _ => bail!("Unexpected relay message"),
                    };
                    let frame: Value = serde_json::from_str(&text)?;
                    let kind = frame["t"].as_str().unwrap_or("");
                    if !registered {
                        if kind == "error" {
                            // The following close code distinguishes a full relay from invalid credentials.
                            continue;
                        }
                        ensure!(kind == "registered" && frame["proto"].as_u64().unwrap_or(1) == 1, "Unsupported relay version");
                        registered = true;
                        self.activity.relay.send_replace(Registration::Online);
                        tracing::info!("Alma relay registered with protocol 1");
                        continue;
                    }
                    let id = frame["id"].as_str().unwrap_or("");
                    if id.is_empty() || id.len() > 255 { continue; }
                    match kind {
                        "hreq" => {
                            write(&mut ws, json!({"t":"hres","id":id,"status":403,"headers":{}})).await?;
                            write(&mut ws, json!({"t":"hend","id":id})).await?;
                        }
                        "wopen" => {
                            if !self.endpoint.accepts(frame["path"].as_str().unwrap_or("")) || sessions.len() >= 16 || sessions.contains_key(id) {
                                write(&mut ws, json!({"t":"werr","id":id,"message":"Encrypted session unavailable"})).await?;
                                continue;
                            }
                            let (input, inbox) = mpsc::channel(8);
                            next_session += 1;
                            let responder = Responder { id: id.into(), number: next_session, out: out.clone() };
                            let code = self.code.clone();
                            let endpoint = self.endpoint.clone();
                            let activity = self.activity.clone();
                            let task = tasks.spawn(async move {
                                let wire = PhoneWire::Relay(&responder, inbox);
                                let result = match endpoint {
                                    Endpoint::Phone(scope) => serve_phone(wire, &code, scope, activity).await,
                                    Endpoint::Desktop { gateway, .. } => serve_desktop(wire, gateway).await,
                                };
                                let code = if result.is_ok() { 1000 } else { 1008 };
                                let _ = responder.reply(json!({"t":"wclose","id":responder.id,"code":code})).await;
                            });
                            sessions.insert(id.into(),Phone { number: next_session, input, task });
                            write(&mut ws, json!({"t":"wopened","id":id})).await?;
                        }
                        "wmsg" => {
                            if let Some(phone) = sessions.get(id) {
                                let id = id.to_owned();
                                if phone.input.try_send(frame).is_err() {
                                    if let Some(phone) = sessions.remove(&id) { phone.task.abort(); }
                                    write(&mut ws, json!({"t":"wclose","id":id,"code":1013})).await?;
                                }
                            }
                        }
                        "wclose" => if let Some(phone) = sessions.remove(id) { phone.task.abort(); },
                        _ => {}
                    }
                }
            }
        }
    }
}

/// The native gateway performs the pinned TLS handshake and authenticates every
/// Core request. The relay and its routing token carry only those TLS bytes.
async fn serve_desktop(mut wire: PhoneWire<'_>, gateway: SocketAddr) -> Result<()> {
    let mut socket = TcpStream::connect(gateway).await?;
    let mut buf = vec![0; 32 * 1024];
    loop {
        tokio::select! {
            message = wire.read() => {
                let Message::Binary(bytes) = message? else { bail!("Expected desktop TLS data"); };
                socket.write_all(&bytes).await?;
            }
            count = socket.read(&mut buf) => {
                let count = count?;
                if count == 0 { return Ok(()); }
                wire.send(Message::Binary(buf[..count].to_vec().into())).await?;
            }
        }
    }
}

async fn write(ws: &mut Ws, frame: Value) -> Result<()> {
    timeout(HANDSHAKE, ws.send(Message::Text(frame.to_string().into()))).await??;
    Ok(())
}

struct Phone {
    number: u64,
    input: mpsc::Sender<Value>,
    task: AbortHandle,
}
struct Reply {
    id: String,
    number: u64,
    frame: Value,
}
struct Responder {
    id: String,
    number: u64,
    out: mpsc::Sender<Reply>,
}
impl Responder {
    async fn reply(&self, frame: Value) -> Result<()> {
        self.out
            .send(Reply {
                id: self.id.clone(),
                number: self.number,
                frame,
            })
            .await
            .map_err(|_| anyhow!("Relay connection closed"))
    }
}

enum PhoneWire<'a> {
    Relay(&'a Responder, mpsc::Receiver<Value>),
    Lan(tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>),
}

impl PhoneWire<'_> {
    async fn read(&mut self) -> Result<Message> {
        match self {
            Self::Relay(_, inbox) => {
                let f = inbox
                    .recv()
                    .await
                    .ok_or_else(|| anyhow!("Alma client disconnected"))?;
                if let Some(s) = f["s"].as_str() {
                    return Ok(Message::Text(s.into()));
                }
                Ok(Message::Binary(
                    STANDARD
                        .decode(
                            f["b"]
                                .as_str()
                                .ok_or_else(|| anyhow!("Expected Alma payload"))?,
                        )?
                        .into(),
                ))
            }
            Self::Lan(socket) => loop {
                match socket
                    .next()
                    .await
                    .ok_or_else(|| anyhow!("Alma client disconnected"))??
                {
                    m @ (Message::Text(_) | Message::Binary(_)) => return Ok(m),
                    Message::Ping(_) => socket.flush().await?,
                    Message::Pong(_) => (),
                    _ => bail!("Alma client disconnected"),
                }
            },
        }
    }

    async fn handshake(&mut self) -> Result<Value> {
        let Message::Text(text) = self.read().await? else {
            bail!("Expected Alma handshake");
        };
        ensure!(text.len() <= 2048, "Oversized Alma handshake");
        Ok(serde_json::from_str(&text)?)
    }

    async fn send(&mut self, message: Message) -> Result<()> {
        match self {
            Self::Relay(peer, _) => {
                let frame = match message {
                    Message::Text(s) => json!({"t":"wmsg","id":peer.id,"s":s.as_str()}),
                    Message::Binary(b) => json!({"t":"wmsg","id":peer.id,"b":STANDARD.encode(b)}),
                    _ => bail!("Unexpected Alma payload"),
                };
                peer.reply(frame).await
            }
            Self::Lan(ws) => Ok(timeout(HANDSHAKE, ws.send(message)).await??),
        }
    }
}

async fn serve_phone(
    mut wire: PhoneWire<'_>,
    code: &str,
    scope: Scope,
    activity: Arc<Activity>,
) -> Result<()> {
    let mut keys = timeout(HANDSHAKE, async {
        let hello = wire.handshake().await?;
        let (pending, response) = crypto::Pending::accept(code, &hello)?;
        wire.send(Message::Text(response.to_string().into()))
            .await?;
        pending.confirm(&wire.handshake().await?)
    })
    .await??;
    activity.phones.fetch_add(1, Ordering::Relaxed);
    let _connected = ConnectedPhone(activity);
    let mut chat = Chat::new(scope);
    loop {
        let replies = tokio::select! {
            frame = wire.read() => {
                let Message::Binary(b) = frame? else { bail!("Ciphertext required"); };
                let mut bytes = b.to_vec();
                chat.request(keys.open(&mut bytes)?).await?
            }
            event = chat.next_event() => match event {
                Ok(Some(event)) => chat.event(event)?,
                Err(ClientError::ResyncRequired(_)) => chat.resync().await?,
                Ok(None) => bail!("Core event stream closed"),
                Err(error) => return Err(error.into()),
            },
        };
        for reply in replies {
            let ciphertext = keys.seal(&reply)?;
            ensure!(
                ciphertext.len() < MAX_FRAME * 3 / 4 - 1024,
                "Alma message too large"
            );
            wire.send(Message::Binary(ciphertext.into())).await?;
        }
    }
}
