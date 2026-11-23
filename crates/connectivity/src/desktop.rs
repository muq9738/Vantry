use crate::{
    pairing::{matches_secret, short_label, token},
    transport::{Heartbeat, Tasks, accepted, byte_stream, send, upgrade},
    turn,
    web::*,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use chrono::Utc;
use futures::StreamExt;
use http::{Method, Request};
use hyper::body::Incoming;
use ring::hmac;
use serde::Deserialize;
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    sync::{Semaphore, mpsc},
    time::Instant,
};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
struct Signal {
    out: mpsc::Sender<Message>,
    stop: CancellationToken,
}
struct Room {
    id: String,
    access: [String; 2],
    user: String,
    password: String,
    expires: chrono::DateTime<Utc>,
    video: Value,
    label: String,
    approved: bool,
    rejected: bool,
    seen: Instant,
    signals: [Option<Signal>; 2],
    life: CancellationToken,
    viewer: CancellationToken,
    switch: Arc<turn::Switch>,
}
impl Room {
    fn info(&self, host: bool) -> Value {
        let mut v = json!({"sessionId":self.id,"iceServers":[{"urls":["turn:127.0.0.1:3478?transport=tcp"],"username":self.user,"credential":self.password}],"iceTransportPolicy":"relay","expiresAt":self.expires,"video":self.video,"transport":"wss-tcp"});
        if host {
            v["hostToken"] = json!(self.access[0]);
            v["viewerToken"] = json!(self.access[1]);
        }
        v
    }
    fn disconnect_viewer(&mut self) {
        self.approved = false;
        self.label.clear();
        self.viewer.cancel();
        self.viewer = self.life.child_token();
        if let Some(v) = self.signals[1].take() {
            v.stop.cancel();
            self.notify(0, false)
        }
    }
    fn notify(&self, role: usize, connected: bool) {
        if let Some(p) = &self.signals[role] {
            if p.out
                .try_send(Message::text(
                    json!({"type":"peer","connected":connected}).to_string(),
                ))
                .is_err()
            {
                p.stop.cancel();
            }
        }
    }
    fn credentials(&mut self, secret: &str) {
        self.expires = Utc::now() + chrono::Duration::minutes(10);
        self.user = format!("{}:{}", self.expires.timestamp(), self.id);
        let key = hmac::Key::new(hmac::HMAC_SHA1_FOR_LEGACY_USE_ONLY, secret.as_bytes());
        self.password = STANDARD.encode(hmac::sign(&key, self.user.as_bytes()).as_ref());
    }
}
impl Drop for Room {
    fn drop(&mut self) {
        self.life.cancel()
    }
}
pub(crate) struct Desktop {
    rooms: Arc<Mutex<HashMap<String, Room>>>,
    tunnels: Arc<Semaphore>,
}
impl Desktop {
    pub fn new() -> Self {
        Self {
            rooms: Arc::default(),
            tunnels: Arc::new(Semaphore::new(128)),
        }
    }
    pub async fn handle(
        &self,
        mut req: Request<Incoming>,
        key: &str,
        tasks: Tasks,
    ) -> Result<Reply, u16> {
        let path = req.uri().path().to_string();
        let auth = bearer(req.headers()).to_string();
        if path == "/desktop/v1/sessions" && req.method() == Method::POST {
            if !matches_secret(&auth, key) {
                return Err(401);
            }
            #[derive(Deserialize)]
            struct Create {
                #[serde(default)]
                video: Value,
            }
            let input: Create = decode(req).await?;
            let mut rooms = self.rooms.lock().unwrap();
            rooms.retain(|_, r| {
                r.signals[0].is_some() || r.seen.elapsed() < Duration::from_secs(120)
            });
            if rooms.len() >= 64 {
                return Err(429);
            }
            let id = token();
            let life = tasks.stop.child_token();
            let mut r = Room {
                id: id.clone(),
                access: [token(), token()],
                user: String::new(),
                password: String::new(),
                expires: Utc::now(),
                video: input.video,
                label: String::new(),
                approved: false,
                rejected: false,
                signals: [None, None],
                seen: Instant::now(),
                viewer: life.child_token(),
                life: life.clone(),
                switch: Arc::default(),
            };
            r.credentials(key);
            let reply = json(201, r.info(true));
            rooms.insert(id.clone(), r);
            let registry = self.rooms.clone();
            tasks.spawn(async move {
                loop {
                    tokio::select! { _ = life.cancelled() => break, _ = tokio::time::sleep(Duration::from_secs(30)) => {} }
                    let mut rooms=registry.lock().unwrap();
                    if rooms.get(&id).is_some_and(|r|r.signals[0].is_none()&&r.seen.elapsed()>=Duration::from_secs(120)){
                        rooms.remove(&id);
                        break;
                    }
                }
            });
            return Ok(reply);
        }
        let rest = path.strip_prefix("/desktop/v1/sessions/").ok_or(404u16)?;
        let (id, op) = rest.split_once('/').unwrap_or((rest, ""));
        if op.contains('/') {
            return Err(404);
        }
        let role = {
            let mut rooms = self.rooms.lock().unwrap();
            let r = rooms.get_mut(id).ok_or(401u16)?;
            let role = r
                .access
                .iter()
                .position(|t| matches_secret(t, &auth))
                .ok_or(401u16)?;
            r.seen = Instant::now();
            role
        };
        match (op, req.method().clone(), role) {
            ("", Method::GET, _) => {
                let mut rooms = self.rooms.lock().unwrap();
                let r = rooms.get_mut(id).ok_or(404u16)?;
                if role == 1 && r.rejected {
                    return Err(403);
                }
                if r.expires <= Utc::now() {
                    r.credentials(key)
                }
                let mut v = r.info(false);
                v["approved"] = json!(r.approved);
                if !r.label.is_empty() {
                    v["pendingViewer"] = json!(r.label)
                }
                Ok(json(200, v))
            }
            ("", Method::DELETE, 0) => {
                self.rooms.lock().unwrap().remove(id);
                Ok(response(204, Default::default()))
            }
            ("request", Method::POST, 1) => {
                #[derive(Deserialize)]
                struct Input {
                    #[serde(default)]
                    label: String,
                }
                let v: Input = decode(req).await?;
                let mut rooms = self.rooms.lock().unwrap();
                let r = rooms.get_mut(id).ok_or(404u16)?;
                if r.signals[1].is_some() {
                    return Err(409);
                }
                r.disconnect_viewer();
                r.label = if v.label.is_empty() {
                    "另一台 Vantry".into()
                } else {
                    short_label(&v.label)
                };
                r.rejected = false;
                Ok(json(200, json!({"waiting":true})))
            }
            ("approve", Method::POST, 0) => {
                #[derive(Deserialize)]
                struct Input {
                    allow: bool,
                }
                let v: Input = decode(req).await?;
                let mut rooms = self.rooms.lock().unwrap();
                let r = rooms.get_mut(id).ok_or(404u16)?;
                r.approved = v.allow;
                r.rejected = !v.allow;
                if !v.allow {
                    r.disconnect_viewer()
                } else if r.signals.iter().all(Option::is_some) {
                    r.notify(0, true);
                    r.notify(1, true)
                }
                Ok(json(200, json!({"approved":v.allow})))
            }
            ("signal" | "turn", Method::GET, _) => {
                let mut rooms = self.rooms.lock().unwrap();
                let r = rooms.get_mut(id).ok_or(404u16)?;
                if role == 1 && !r.approved {
                    return Err(403);
                }
                let stop = if role == 1 {
                    r.viewer.child_token()
                } else {
                    r.life.child_token()
                };
                if op == "turn" {
                    let permit = self
                        .tunnels
                        .clone()
                        .try_acquire_owned()
                        .map_err(|_| 429u16)?;
                    if r.expires <= Utc::now() {
                        return Err(401);
                    }
                    let (res, on) = upgrade(&mut req)?;
                    let switch = r.switch.clone();
                    let (user, pass) = (r.user.clone(), r.password.clone());
                    let group = tasks.clone();
                    tasks.spawn(async move {
                        let _permit = permit;
                        let tunnel = async {
                            let ws = accepted(on, 1 << 20).await?;
                            turn::run(byte_stream(ws, &group), switch, user, pass).await
                        };
                        tokio::select! { _ = stop.cancelled() => {}, _ = tunnel => {} }
                    });
                    return Ok(res);
                }
                if query(&req, "role") != ["host", "viewer"][role] {
                    return Err(403);
                }
                let (res, on) = upgrade(&mut req)?;
                let (tx, mut rx) = mpsc::channel(64);
                let current = tx.clone();
                if let Some(previous) = r.signals[role].replace(Signal {
                    out: tx,
                    stop: stop.clone(),
                }) {
                    previous.stop.cancel()
                }
                if r.approved && r.signals[1 - role].is_some() {
                    r.notify(0, true);
                    r.notify(1, true)
                }
                let rooms = self.rooms.clone();
                let id = id.to_owned();
                tasks.spawn(async move {
                    let run = async {
                        let mut ws = accepted(on, 64 << 10).await?;
                        let mut heartbeat = Heartbeat::new();
                        loop {
                            let msg = tokio::select! {
                                tick = heartbeat.tick() => { tick?; Message::Ping(Default::default()) }
                                outgoing = rx.recv() => match outgoing { Some(msg) => msg, None => break },
                                frame = ws.next() => {
                                    let Some(frame) = frame else { break };
                                    heartbeat.activity();
                                    match frame? {
                                        Message::Ping(data) => Message::Pong(data),
                                        Message::Close(_) => break,
                                        msg if msg.is_text() || msg.is_binary() => {
                                            let message: Value = serde_json::from_slice(&msg.into_data())?;
                                            if matches!(message["type"].as_str(), Some("offer" | "answer" | "ice" | "candidate")) {
                                                let rooms = rooms.lock().unwrap();
                                                if let Some(room) = rooms.get(&id)
                                                    && room.approved
                                                    && room.signals[role].as_ref().is_some_and(|p| p.out.same_channel(&current))
                                                    && let Some(other) = &room.signals[1-role]
                                                    && other.out.try_send(Message::text(message.to_string())).is_err()
                                                { other.stop.cancel(); }
                                            }
                                            continue;
                                        }
                                        _ => continue,
                                    }
                                }
                            };
                            send(&mut ws, msg).await?;
                        }
                        Ok::<_,anyhow::Error>(())
                    };
                    tokio::select! { _ = stop.cancelled() => {}, _ = run => {} }
                    let mut rooms = rooms.lock().unwrap();
                    if let Some(room) = rooms.get_mut(&id)
                        && room.signals[role].as_ref().is_some_and(|s| s.out.same_channel(&current)) {
                        room.signals[role]=None;
                        room.seen=Instant::now();
                        if role==1 {room.disconnect_viewer();}
                        room.notify(1-role,false);
                    }
                });
                Ok(res)
            }
            ("request" | "approve" | "signal" | "", _, _) => Err(403),
            ("turn", _, _) => Err(405),
            _ => Err(404),
        }
    }
}
