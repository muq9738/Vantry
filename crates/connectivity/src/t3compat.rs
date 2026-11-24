//! T3 orchestration compatibility adapter at the network boundary.
//! All conversation state and execution remain owned by Vantry Core.
use crate::{
    pairing::{matches_secret, token},
    transport::{Tasks, accepted, upgrade},
    web::{Reply, bearer, decode, json, response},
};
use anyhow::{Context, Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use bytes::Bytes;
use chrono::{DateTime, Duration as ChronoDuration, Utc};
use futures::{SinkExt, StreamExt};
use http::{Method, Request};
use http_body_util::{BodyExt, Full, Limited};
use hyper::{
    body::Incoming,
    client::conn::http1 as client_http1,
    server::conn::http1 as server_http1,
    service::service_fn,
};
use hyper_util::rt::TokioIo;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    convert::Infallible,
    fs,
    io::Write,
    net::SocketAddr,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};
use tokio::{net::TcpListener, sync::mpsc};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
use url::Url;

pub const SCOPES: &str = "orchestration:read orchestration:operate";
pub const TOKEN_GRANT: &str = "urn:ietf:params:oauth:grant-type:token-exchange";
pub const BOOTSTRAP_TYPE: &str = "urn:t3:params:oauth:token-type:environment-bootstrap";
pub const ACCESS_TYPE: &str = "urn:t3:params:oauth:token-type:access_token";

#[derive(Clone)]
pub struct T3Options {
    pub data_dir: PathBuf,
    pub core_socket: PathBuf,
    pub environment_id: String,
    pub name: String,
    pub version: String,
    pub provider: String,
    pub model: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct T3Client {
    pub id: String,
    pub name: String,
    pub token: String,
    pub expires_at: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub scopes: String,
}

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct AuthState {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    environment_id: String,
    #[serde(default)]
    clients: Vec<T3Client>,
}

struct BootstrapCode {
    code: String,
    expires: DateTime<Utc>,
}

struct WsTicket {
    _client_id: String,
    expires: DateTime<Utc>,
}

struct Inner {
    opts: T3Options,
    state: Mutex<AuthState>,
    codes: Mutex<HashMap<String, BootstrapCode>>,
    tickets: Mutex<HashMap<String, WsTicket>>,
    sessions: Mutex<Vec<CancellationToken>>,
}

#[derive(Clone)]
pub struct T3Server(Arc<Inner>);

impl T3Server {
    pub fn open(opts: T3Options) -> Result<Self> {
        let path = opts.data_dir.join("t3-access.json");
        let mut state = match fs::read(&path) {
            Ok(bytes) if !bytes.is_empty() => serde_json::from_slice::<AuthState>(&bytes)?,
            Ok(_) => AuthState::default(),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => AuthState::default(),
            Err(e) => return Err(e.into()),
        };
        if state.environment_id.is_empty() {
            state.environment_id = opts.environment_id.clone();
        }
        Ok(Self(Arc::new(Inner {
            opts,
            state: Mutex::new(state),
            codes: Mutex::default(),
            tickets: Mutex::default(),
            sessions: Mutex::default(),
        })))
    }

    pub fn environment_id(&self) -> String {
        let s = self.0.state.lock().unwrap();
        if !s.environment_id.is_empty() {
            s.environment_id.clone()
        } else {
            self.0.opts.environment_id.clone()
        }
    }

    fn save_locked(&self, state: &AuthState) -> Result<()> {
        fs::create_dir_all(&self.0.opts.data_dir)?;
        let data = serde_json::to_vec_pretty(state)?;
        let tmp = self
            .0
            .opts
            .data_dir
            .join(format!(".t3-access-{}", uuid::Uuid::new_v4().simple()));
        let mut f = {
            let mut o = fs::OpenOptions::new();
            o.write(true).create_new(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt;
                o.mode(0o600);
            }
            o.open(&tmp)?
        };
        let res = (|| {
            f.write_all(&data)?;
            f.sync_all()?;
            fs::rename(&tmp, self.0.opts.data_dir.join("t3-access.json"))
        })();
        if res.is_err() {
            let _ = fs::remove_file(&tmp);
        }
        Ok(res?)
    }

    pub fn pairing_link(&self, base: &str, routes: Option<&Value>) -> Result<(String, String)> {
        let mut u = Url::parse(base).context("T3 配对需要根路径 HTTP/HTTPS 入口")?;
        ensure!(
            matches!(u.scheme(), "http" | "https")
                && u.has_host()
                && u.username().is_empty()
                && u.query().is_none()
                && u.fragment().is_none()
                && matches!(u.path(), "" | "/"),
            "T3 配对需要根路径 HTTP/HTTPS 入口"
        );
        let raw = token().to_ascii_uppercase();
        let bootstrap = format!(
            "SESSION-{}-{}",
            &raw[..6.min(raw.len())],
            &raw[6.min(raw.len())..12.min(raw.len())]
        );
        {
            let mut codes = self.0.codes.lock().unwrap();
            codes.clear();
            codes.insert(
                bootstrap.clone(),
                BootstrapCode {
                    code: bootstrap.clone(),
                    expires: Utc::now() + ChronoDuration::minutes(5),
                },
            );
        }
        u.set_path("/");
        let mut frag = format!("token={bootstrap}");
        if let Some(routes) = routes {
            let encoded = URL_SAFE_NO_PAD.encode(serde_json::to_vec(routes)?);
            frag.push_str(&format!(
                "&routes={encoded}&environment={}",
                self.environment_id()
            ));
        }
        u.set_fragment(Some(&frag));
        Ok((bootstrap, u.to_string()))
    }

    pub fn revoke_all(&self) -> Result<()> {
        let mut state = self.0.state.lock().unwrap();
        state.clients.clear();
        self.save_locked(&state)?;
        self.0.codes.lock().unwrap().clear();
        self.0.tickets.lock().unwrap().clear();
        for tok in self.0.sessions.lock().unwrap().drain(..) {
            tok.cancel();
        }
        Ok(())
    }

    pub fn clients(&self) -> Vec<T3Client> {
        let now = Utc::now();
        self.0
            .state
            .lock()
            .unwrap()
            .clients
            .iter()
            .filter(|c| c.expires_at > now)
            .cloned()
            .collect()
    }

    fn authorize(&self, candidate: &str) -> Option<T3Client> {
        if candidate.is_empty() {
            return None;
        }
        let now = Utc::now();
        let state = self.0.state.lock().unwrap();
        state
            .clients
            .iter()
            .find(|c| c.expires_at > now && matches_secret(&c.token, candidate))
            .cloned()
    }

    fn exchange_token(&self, body: &Value) -> Result<Value, u16> {
        let grant = body["grant_type"].as_str().unwrap_or("");
        let subject_token = body["subject_token"]
            .as_str()
            .or_else(|| body["token"].as_str())
            .unwrap_or("");
        let subject_type = body["subject_token_type"].as_str().unwrap_or("");
        let name = body["client_name"]
            .as_str()
            .or_else(|| body["name"].as_str())
            .unwrap_or("T3 Client")
            .trim()
            .to_owned();
        if grant != TOKEN_GRANT && !subject_token.is_empty() && subject_type.is_empty() {
            // Allow simplified bootstrap exchange with token field.
        } else if grant != TOKEN_GRANT {
            return Err(400);
        }

        if subject_type == ACCESS_TYPE {
            let client = self.authorize(subject_token).ok_or(401u16)?;
            let ticket_val = token();
            let expires = Utc::now() + ChronoDuration::seconds(60);
            self.0.tickets.lock().unwrap().insert(
                ticket_val.clone(),
                WsTicket {
                    _client_id: client.id.clone(),
                    expires,
                },
            );
            return Ok(json!({
                "access_token": client.token,
                "token_type": "Bearer",
                "scope": SCOPES,
                "expires_in": (client.expires_at - Utc::now()).num_seconds().max(0),
                "ticket": ticket_val,
                "environmentId": self.environment_id(),
            }));
        }

        if !subject_type.is_empty() && subject_type != BOOTSTRAP_TYPE {
            return Err(400);
        }
        let now = Utc::now();
        let valid_code = {
            let mut codes = self.0.codes.lock().unwrap();
            codes.retain(|_, v| v.expires > now);
            let matched = codes
                .values()
                .find(|v| matches_secret(&v.code, subject_token))
                .map(|v| v.code.clone());
            if let Some(k) = matched {
                codes.remove(&k);
                true
            } else {
                false
            }
        };
        if !valid_code {
            return Err(401);
        }
        let client = T3Client {
            id: format!("t3_{}", uuid::Uuid::new_v4().simple()),
            name: if name.is_empty() {
                "T3 Client".into()
            } else {
                name
            },
            token: token(),
            created_at: now,
            expires_at: now + ChronoDuration::days(30),
            scopes: SCOPES.into(),
        };
        let ticket_val = token();
        {
            let mut state = self.0.state.lock().unwrap();
            state.clients.push(client.clone());
            self.save_locked(&state).map_err(|_| 500u16)?;
        }
        self.0.tickets.lock().unwrap().insert(
            ticket_val.clone(),
            WsTicket {
                _client_id: client.id.clone(),
                expires: now + ChronoDuration::seconds(60),
            },
        );
        Ok(json!({
            "access_token": client.token,
            "token_type": "Bearer",
            "scope": SCOPES,
            "expires_in": 30 * 86400,
            "client_id": client.id,
            "ticket": ticket_val,
            "environmentId": self.environment_id(),
        }))
    }

    pub async fn bind_loopback(&self) -> Result<(TcpListener, SocketAddr)> {
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let addr = listener.local_addr()?;
        Ok((listener, addr))
    }

    pub async fn serve(self, listener: TcpListener, stop: CancellationToken) -> Result<()> {
        let tasks = Tasks::new(stop.clone());
        loop {
            let (conn, _) = tokio::select! {
                _ = stop.cancelled() => break,
                r = listener.accept() => r?,
            };
            let this = self.clone();
            let group = tasks.clone();
            tasks.spawn(async move {
                let svc = service_fn(move |req| {
                    let this = this.clone();
                    let group = group.clone();
                    async move {
                        let reply = this
                            .handle(req, group)
                            .await
                            .unwrap_or_else(|code| response(code, Bytes::new()));
                        Ok::<_, Infallible>(reply)
                    }
                });
                let _ = server_http1::Builder::new()
                    .serve_connection(TokioIo::new(conn), svc)
                    .with_upgrades()
                    .await;
            });
        }
        stop.cancel();
        Ok(())
    }

    async fn handle(&self, mut req: Request<Incoming>, tasks: Tasks) -> Result<Reply, u16> {
        let path = req.uri().path().to_owned();
        let method = req.method().clone();
        match (method, path.as_str()) {
            (Method::GET, "/.well-known/t3/environment") => Ok(json(
                200,
                json!({
                    "protocolVersion": 1,
                    "environmentId": self.environment_id(),
                    "name": self.0.opts.name,
                    "version": self.0.opts.version,
                    "os": std::env::consts::OS,
                    "arch": std::env::consts::ARCH,
                    "provider": self.0.opts.provider,
                    "model": self.0.opts.model,
                    "scopes": SCOPES,
                    "capabilities": [
                        "orchestration.v1",
                        "orchestration.snapshot",
                        "orchestration.dispatch",
                        "orchestration.subscribe",
                        "workbench.v1"
                    ]
                }),
            )),
            (Method::POST, "/oauth/token") => {
                let body: Value = decode(req).await?;
                Ok(json(200, self.exchange_token(&body)?))
            }
            (Method::GET, "/ws") => {
                let query = req.uri().query().unwrap_or("");
                let ticket = query.split('&').find_map(|pair| {
                    let (k, v) = pair.split_once('=')?;
                    (k == "ticket").then_some(v.to_owned())
                });
                let auth_ok = if let Some(t) = ticket {
                    let now = Utc::now();
                    let mut map = self.0.tickets.lock().unwrap();
                    map.retain(|_, v| v.expires > now);
                    map.remove(&t).is_some()
                } else {
                    self.authorize(bearer(req.headers())).is_some()
                };
                if !auth_ok {
                    return Err(401);
                }
                let (reply, up) = upgrade(&mut req)?;
                let this = self.clone();
                let session_stop = tasks.stop.child_token();
                self.0.sessions.lock().unwrap().push(session_stop.clone());
                tasks.spawn(async move {
                    let Ok(ws) = accepted(up, 4 << 20).await else {
                        return;
                    };
                    this.drive_ws(ws, session_stop).await;
                });
                Ok(reply)
            }
            _ => Err(404),
        }
    }

    async fn drive_ws(&self, ws: crate::transport::Ws, stop: CancellationToken) {
        let (tx, mut rx) = mpsc::channel::<Message>(64);
        let (mut sink, mut stream) = ws.split();
        let writer_stop = stop.clone();
        let writer = tokio::spawn(async move {
            loop {
                let msg = tokio::select! {
                    _ = writer_stop.cancelled() => break,
                    m = rx.recv() => match m {
                        Some(m) => m,
                        None => break,
                    }
                };
                if sink.send(msg).await.is_err() {
                    break;
                }
            }
        });
        let core_socket = self.0.opts.core_socket.clone();
        loop {
            let msg = tokio::select! {
                _ = stop.cancelled() => break,
                next = stream.next() => match next {
                    Some(Ok(m)) => m,
                    _ => break,
                }
            };
            let Message::Text(text) = msg else {
                continue;
            };
            let Ok(req) = serde_json::from_str::<Value>(&text) else {
                continue;
            };
            let id = req.get("id").cloned().unwrap_or(Value::Null);
            let method = req["method"].as_str().unwrap_or("");
            let params = req.get("params").cloned().unwrap_or(Value::Null);
            let reply = match method {
                "ping" => Ok(json!({"pong": true})),
                "health" => core_http_json(&core_socket, Method::GET, "/v1/health", None).await,
                "orchestration.snapshot" | "snapshot" => {
                    core_http_json(&core_socket, Method::GET, "/v1/snapshot", None).await
                }
                "orchestration.dispatch" | "command" => {
                    core_http_json(&core_socket, Method::POST, "/v1/commands", Some(params)).await
                }
                "workbench" => {
                    core_http_json(&core_socket, Method::POST, "/v1/workbench", Some(params)).await
                }
                "orchestration.subscribe" | "subscribe" => {
                    let after = params["after"].as_i64().unwrap_or(0);
                    let sock = core_socket.clone();
                    let sub_tx = tx.clone();
                    let sub_stop = stop.clone();
                    tokio::spawn(async move {
                        let _ = stream_core_events(&sock, after, sub_tx, sub_stop).await;
                    });
                    Ok(json!({"subscribed": true, "after": after}))
                }
                other => Err(anyhow!("unknown T3 RPC method {other}")),
            };
            let frame = match reply {
                Ok(result) => json!({"jsonrpc": "2.0", "id": id, "result": result}),
                Err(err) => {
                    json!({"jsonrpc": "2.0", "id": id, "error": {"code": -32000, "message": err.to_string()}})
                }
            };
            if tx.send(Message::text(frame.to_string())).await.is_err() {
                break;
            }
        }
        stop.cancel();
        let _ = writer.await;
    }
}

async fn connect_core(core: &Path) -> Result<(crate::transport::Stream, Option<String>)> {
    if let Ok(bytes) = fs::read(core)
        && let Some(ep) = vantry_protocol::LocalTcpEndpoint::parse_bytes(&bytes)
    {
        let io = tokio::net::TcpStream::connect(&ep.addr).await?;
        return Ok((Box::new(io), Some(ep.token)));
    }
    #[cfg(unix)]
    {
        let io = tokio::net::UnixStream::connect(core).await?;
        Ok((Box::new(io), None))
    }
    #[cfg(not(unix))]
    {
        anyhow::bail!("local Core endpoint unavailable")
    }
}

async fn core_http_json(
    core: &Path,
    method: Method,
    path: &str,
    body: Option<Value>,
) -> Result<Value> {
    let (io, token) = connect_core(core).await?;
    let (mut tx, conn) = client_http1::handshake(TokioIo::new(io)).await?;
    let driver = tokio::spawn(conn);
    let mut builder = Request::builder()
        .method(method)
        .uri(path)
        .header("host", "vantry.local")
        .header("content-type", "application/json");
    if let Some(t) = token {
        builder = builder.header(vantry_protocol::LOCAL_TOKEN_HEADER, t);
    }
    let payload = body.map(|v| v.to_string().into_bytes()).unwrap_or_default();
    let res = tx
        .send_request(builder.body(Full::new(Bytes::from(payload)))?)
        .await?;
    let status = res.status();
    let data = Limited::new(res.into_body(), 16 << 20)
        .collect()
        .await
        .map_err(|_| anyhow!("Core response too large"))?
        .to_bytes();
    drop(tx);
    let _ = driver.await;
    let parsed: Value = serde_json::from_slice(&data)?;
    ensure!(status.is_success(), "Core returned {}: {parsed}", status);
    Ok(parsed)
}

async fn stream_core_events(
    core: &Path,
    after: i64,
    tx: mpsc::Sender<Message>,
    stop: CancellationToken,
) -> Result<()> {
    let (io, token) = connect_core(core).await?;
    let (mut sender, conn) = client_http1::handshake(TokioIo::new(io)).await?;
    let driver = tokio::spawn(conn);
    let mut builder = Request::builder()
        .method(Method::GET)
        .uri(format!("/v1/events?after={after}"))
        .header("host", "vantry.local");
    if let Some(t) = token {
        builder = builder.header(vantry_protocol::LOCAL_TOKEN_HEADER, t);
    }
    let res = sender
        .send_request(builder.body(Full::new(Bytes::new()))?)
        .await?;
    ensure!(res.status().is_success(), "Core event subscribe failed");
    let mut body = res.into_body();
    let mut buf = Vec::new();
    loop {
        let frame = tokio::select! {
            _ = stop.cancelled() => break,
            f = body.frame() => match f {
                Some(Ok(frame)) => frame,
                _ => break,
            }
        };
        if let Some(data) = frame.data_ref() {
            buf.extend_from_slice(data);
            while let Some(pos) = buf.iter().position(|&b| b == b'\n') {
                let line = buf.drain(..=pos).collect::<Vec<_>>();
                if let Ok(env) = serde_json::from_slice::<Value>(&line) {
                    let msg = json!({"jsonrpc": "2.0", "method": "orchestration.event", "params": env});
                    if tx.send(Message::text(msg.to_string())).await.is_err() {
                        drop(driver);
                        return Ok(());
                    }
                }
            }
        }
    }
    driver.abort();
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn t3_well_known_oauth_exchange_and_ws_ping() -> Result<()> {
        let dir = tempfile::tempdir()?;
        let server = T3Server::open(T3Options {
            data_dir: dir.path().to_owned(),
            core_socket: dir.path().join("core.sock"),
            environment_id: "env_test".into(),
            name: "Test Mac".into(),
            version: "0.1.42".into(),
            provider: "controlled".into(),
            model: "local/fixture".into(),
        })?;
        let (code, link) = server.pairing_link("http://127.0.0.1:8080", None)?;
        assert!(link.contains(&format!("token={code}")));
        let (listener, addr) = server.bind_loopback().await?;
        let stop = CancellationToken::new();
        let handle = tokio::spawn(server.clone().serve(listener, stop.clone()));

        let client = reqwest::Client::new();
        let meta: Value = client
            .get(format!("http://{addr}/.well-known/t3/environment"))
            .send()
            .await?
            .json()
            .await?;
        assert_eq!(meta["environmentId"], "env_test");

        let exchanged: Value = client
            .post(format!("http://{addr}/oauth/token"))
            .json(&json!({
                "grant_type": TOKEN_GRANT,
                "subject_token": code,
                "subject_token_type": BOOTSTRAP_TYPE,
                "client_name": "iPhone"
            }))
            .send()
            .await?
            .json()
            .await?;
        let ticket = exchanged["ticket"].as_str().unwrap();
        assert!(!ticket.is_empty());

        let (mut ws, _) =
            tokio_tungstenite::connect_async(format!("ws://{addr}/ws?ticket={ticket}")).await?;
        use futures::SinkExt;
        ws.send(Message::text(r#"{"jsonrpc":"2.0","id":1,"method":"ping"}"#))
            .await?;
        let reply = ws.next().await.unwrap()?;
        let parsed: Value = serde_json::from_str(reply.to_text()?)?;
        assert_eq!(parsed["result"]["pong"], true);

        server.revoke_all()?;
        assert!(server.clients().is_empty());
        stop.cancel();
        handle.await??;
        Ok(())
    }
}
