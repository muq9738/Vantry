//! Opt-in Alma mobile host. The relay transports opaque phone sessions.
mod bootstrap;
mod chat;
mod crypto;
mod lan;
pub use crypto::new_code;

use crate::transport::{self, Ws};
use anyhow::{Result, anyhow, bail, ensure};
use base64::{Engine, engine::general_purpose::STANDARD};
use chat::{Chat, Scope};
use futures::{SinkExt, StreamExt};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio::{
    sync::mpsc,
    task::{AbortHandle, JoinSet},
    time::{Instant, interval, timeout},
};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
use url::Url;
use vantry_client::{ClientError, CoreClient};

const MAX_FRAME: usize = 4 << 20;
const HANDSHAKE: Duration = Duration::from_secs(15);

#[derive(Default)]
pub struct Activity {
    relay_online: AtomicBool,
    rejected: AtomicBool,
    phones: AtomicUsize,
}

impl Activity {
    pub fn relay_online(&self) -> bool {
        self.relay_online.load(Ordering::Relaxed)
    }
    pub fn rejected(&self) -> bool {
        self.rejected.load(Ordering::Relaxed)
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
pub struct Host {
    relay: Url,
    registration: String,
    code: Arc<str>,
    key: Option<String>,
    scope: Scope,
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
            scope: Scope {
                pairing,
                thread,
                core,
                lan: None,
            },
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
            self.activity.relay_online.store(false, Ordering::Relaxed);
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
        let mut register = json!({"t":"register","tokenHash":self.registration,"name":"Vantry","ver":env!("CARGO_PKG_VERSION"),"proto":1});
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
                                self.activity.rejected.store(true, Ordering::Relaxed);
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
                        self.activity.relay_online.store(true, Ordering::Relaxed);
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
                            if frame["path"] != "/__alma_e2e__" || sessions.len() >= 16 || sessions.contains_key(id) {
                                write(&mut ws, json!({"t":"werr","id":id,"message":"Encrypted session unavailable"})).await?;
                                continue;
                            }
                            let (input, inbox) = mpsc::channel(8);
                            next_session += 1;
                            let responder = Responder { id: id.into(), number: next_session, out: out.clone() };
                            let code = self.code.clone();
                            let scope = self.scope.clone();
                            let activity = self.activity.clone();
                            let task = tasks.spawn(async move {
                                let wire = PhoneWire::Relay(&responder, inbox);
                                let _ = serve_phone(wire, &code, scope, activity).await;
                                let _ = responder.reply(json!({"t":"wclose","id":responder.id,"code":1008})).await;
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
