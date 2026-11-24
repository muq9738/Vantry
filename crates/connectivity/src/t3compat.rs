//! T3 orchestration compatibility adapter at the network boundary.
//! All conversation state and execution remain owned by Vantry Core.
use crate::{
    pairing::{matches_secret, token},
    transport::{Tasks, accepted, upgrade},
    web::{Reply, bearer, json, response},
};
use anyhow::{Context, Result, anyhow, ensure};
use base64::{
    Engine,
    engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD},
};
use bytes::Bytes;
use chrono::{DateTime, Duration as ChronoDuration, Utc};
use futures::{SinkExt, StreamExt};
use http::{HeaderMap, Method, Request};
use http_body_util::{BodyExt, Full, Limited};
use hyper::{
    body::Incoming,
    client::conn::http1 as client_http1,
    server::conn::http1 as server_http1,
    service::service_fn,
};
use hyper_util::rt::TokioIo;
use ring::{digest, hmac};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::{BTreeMap, HashMap},
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

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct LegacySession {
    pub id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub scopes: String,
    pub created_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
}

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct AuthState {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    environment_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    legacy_signing_key: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    legacy_sessions: Vec<LegacySession>,
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
        if let Some(client) = state
            .clients
            .iter()
            .find(|c| c.expires_at > now && matches_secret(&c.token, candidate))
        {
            return Some(client.clone());
        }
        verify_legacy_session(&state, candidate, now)
    }

    fn session_cookie_name(&self) -> String {
        let env_id = self.0.state.lock().unwrap().environment_id.trim().to_owned();
        if !env_id.is_empty() {
            let d = digest::digest(&digest::SHA256, env_id.as_bytes());
            let mut hex = String::with_capacity(12);
            for byte in &d.as_ref()[..6] {
                use std::fmt::Write;
                let _ = write!(&mut hex, "{byte:02x}");
            }
            return format!("t3_session_{hex}");
        }
        "vantry_session".to_owned()
    }

    fn auth_descriptor(&self) -> Value {
        json!({
            "policy": "remote-reachable",
            "bootstrapMethods": ["one-time-token"],
            "sessionMethods": ["bearer-access-token", "browser-session-cookie"],
            "sessionCookieName": self.session_cookie_name(),
        })
    }

    fn well_known_descriptor(&self) -> Value {
        let arch = match std::env::consts::ARCH {
            "x86_64" => "x64",
            "aarch64" => "arm64",
            _ => "other",
        };
        let os = match std::env::consts::OS {
            "macos" => "darwin",
            other => other,
        };
        json!({
            "environmentId": self.environment_id(),
            "label": self.0.opts.name,
            "name": self.0.opts.name,
            "platform": {
                "os": os,
                "arch": arch,
            },
            "serverVersion": self.0.opts.version,
            "version": self.0.opts.version,
            "protocolVersion": 1,
            "orchestrationProtocolVersion": 2,
            "provider": self.0.opts.provider,
            "model": self.0.opts.model,
            "scopes": SCOPES,
            "capabilities": {
                "remoteDesktop": true,
                "repositoryIdentity": false,
                "connectionProbe": true,
                "attachmentUploads": false,
                "questionAttachments": false,
                "pullRequests": false,
                "threadStop": true,
                "threadPinning": true,
                "threadSettlement": true,
                "threadSnooze": true,
                "agentActivityPublishing": false,
            },
        })
    }

    fn exchange_token(&self, body: &Value) -> Result<Value, u16> {
        let grant = body["grant_type"].as_str().unwrap_or("");
        let subject_token = body["subject_token"]
            .as_str()
            .or_else(|| body["token"].as_str())
            .unwrap_or("");
        let subject_type = body["subject_token_type"].as_str().unwrap_or("");
        let name = body["client_label"]
            .as_str()
            .or_else(|| body["client_name"].as_str())
            .or_else(|| body["name"].as_str())
            .unwrap_or("T3 客户端")
            .trim()
            .to_owned();
        let wanted_scope = body["scope"].as_str().unwrap_or("").trim();
        let granted_scope = if wanted_scope.is_empty() {
            SCOPES.to_owned()
        } else {
            wanted_scope.to_owned()
        };
        if grant != TOKEN_GRANT && !subject_token.is_empty() && subject_type.is_empty() {
            // Allow simplified bootstrap exchange with token field.
        } else if grant != TOKEN_GRANT {
            return Err(400);
        }

        if subject_type == ACCESS_TYPE {
            let client = self.authorize(subject_token).ok_or(401u16)?;
            let ticket_val = token();
            let expires = Utc::now() + ChronoDuration::minutes(5);
            self.0.tickets.lock().unwrap().insert(
                ticket_val.clone(),
                WsTicket {
                    _client_id: client.id.clone(),
                    expires,
                },
            );
            return Ok(json!({
                "access_token": client.token,
                "issued_token_type": ACCESS_TYPE,
                "token_type": "Bearer",
                "scope": if client.scopes.is_empty() { SCOPES } else { &client.scopes },
                "expires_in": (client.expires_at - Utc::now()).num_seconds().max(0),
                "ticket": ticket_val,
                "environmentId": self.environment_id(),
            }));
        }

        if !subject_type.is_empty() && subject_type != BOOTSTRAP_TYPE {
            return Err(400);
        }
        let normalized = subject_token.trim().to_ascii_uppercase();
        let now = Utc::now();
        let valid_code = {
            let mut codes = self.0.codes.lock().unwrap();
            codes.retain(|_, v| v.expires > now);
            let matched = codes
                .values()
                .find(|v| matches_secret(&v.code, subject_token) || matches_secret(&v.code, &normalized))
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
            id: token(),
            name: if name.is_empty() {
                "T3 客户端".into()
            } else {
                name
            },
            token: token(),
            created_at: now,
            expires_at: now + ChronoDuration::days(365),
            scopes: granted_scope,
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
                expires: now + ChronoDuration::minutes(5),
            },
        );
        Ok(json!({
            "access_token": client.token,
            "issued_token_type": ACCESS_TYPE,
            "token_type": "Bearer",
            "scope": client.scopes,
            "expires_in": 365 * 86400,
            "client_id": client.id,
            "ticket": ticket_val,
            "environmentId": self.environment_id(),
        }))
    }

    pub async fn bind_loopback(&self) -> Result<(TcpListener, SocketAddr)> {
        self.bind_configured(None).await
    }

    pub async fn bind_configured(&self, configured: Option<&str>) -> Result<(TcpListener, SocketAddr)> {
        let listener = if let Some(addr) = configured.map(str::trim).filter(|s| !s.is_empty()) {
            match TcpListener::bind(addr).await {
                Ok(l) => l,
                Err(_) => TcpListener::bind("127.0.0.1:0").await?,
            }
        } else {
            TcpListener::bind("127.0.0.1:0").await?
        };
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
                        let origin = req
                            .headers()
                            .get("origin")
                            .and_then(|v| v.to_str().ok())
                            .map(str::trim)
                            .filter(|s| !s.is_empty())
                            .map(str::to_owned);
                        let mut reply = this
                            .handle(req, group)
                            .await
                            .unwrap_or_else(|code| match code {
                                401 => auth_error_reply(),
                                other => response(other, Bytes::new()),
                            });
                        apply_cors_headers(&mut reply, origin.as_deref());
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
        if method == Method::OPTIONS {
            return Ok(response(204, Bytes::new()));
        }
        if path == "/ws" && method == Method::GET {
            let query = req.uri().query().unwrap_or("");
            let ticket = url::form_urlencoded::parse(query.as_bytes()).find_map(|(k, v)| {
                matches!(k.as_ref(), "ticket" | "wsTicket").then(|| v.into_owned())
            });
            let auth_ok = if let Some(t) = ticket {
                let now = Utc::now();
                let mut map = self.0.tickets.lock().unwrap();
                map.retain(|_, v| v.expires > now);
                map.remove(&t).is_some()
            } else {
                let (tok, _) = extract_request_token(req.headers());
                self.authorize(&tok).is_some()
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
            return Ok(reply);
        }

        if method == Method::GET && path == "/.well-known/t3/environment" {
            return Ok(json(200, self.well_known_descriptor()));
        }
        if method == Method::POST && path == "/oauth/token" {
            let body = decode_json_or_form(req).await?;
            return Ok(json(200, self.exchange_token(&body)?));
        }

        let (req_token, auth_method) = extract_request_token(req.headers());
        let client = self.authorize(&req_token);

        if method == Method::GET && path == "/api/auth/session" {
            let mut result = json!({
                "authenticated": client.is_some(),
                "auth": self.auth_descriptor(),
            });
            if let Some(c) = client {
                let (granted_scopes, granted_perms) = session_scopes_and_permissions(&c.scopes);
                result["scopes"] = json!(granted_scopes);
                result["sessionMethod"] = json!(if auth_method.is_empty() {
                    "bearer-access-token"
                } else {
                    auth_method
                });
                result["expiresAt"] = json!(utc_iso(c.expires_at));
                if !granted_perms.is_empty() {
                    result["permissions"] = json!(granted_perms);
                }
            }
            return Ok(json(200, result));
        }

        if method == Method::POST && path == "/api/auth/browser-session" {
            let body = decode_json_or_form(req).await?;
            let credential = body["credential"].as_str().unwrap_or("").trim();
            if credential.is_empty() {
                return Err(400);
            }
            let label = body["label"].as_str().unwrap_or("T3 浏览器会话").trim();
            let exchanged = self.exchange_token(&json!({
                "grant_type": TOKEN_GRANT,
                "subject_token": credential,
                "subject_token_type": BOOTSTRAP_TYPE,
                "client_label": label,
            }))?;
            let access = exchanged["access_token"].as_str().unwrap_or_default();
            let cookie_name = self.session_cookie_name();
            let expires = Utc::now() + ChronoDuration::days(30);
            let mut reply = json(
                200,
                json!({
                    "authenticated": true,
                    "scopes": ["orchestration:read", "orchestration:operate"],
                    "sessionMethod": "browser-session-cookie",
                    "expiresAt": utc_iso(expires),
                }),
            );
            if let Ok(val) = format!(
                "{cookie_name}={access}; Path=/; Max-Age={}; HttpOnly; SameSite=Lax",
                30 * 86400
            )
            .parse()
            {
                reply.headers_mut().insert("set-cookie", val);
            }
            return Ok(reply);
        }

        let Some(client) = client else {
            return Err(401);
        };

        if matches!(
            path.as_str(),
            "/api/auth/websocket-ticket"
                | "/api/auth/websocket-ticket/rpc"
                | "/api/auth/websocket-ticket/media"
        ) {
            if method != Method::POST {
                return Err(405);
            }
            let key = token();
            let expiry = Utc::now() + ChronoDuration::minutes(5);
            {
                let mut tickets = self.0.tickets.lock().unwrap();
                let now = Utc::now();
                tickets.retain(|_, v| v.expires > now);
                if tickets.len() >= 1024 {
                    return Err(429);
                }
                tickets.insert(
                    key.clone(),
                    WsTicket {
                        _client_id: client.id.clone(),
                        expires: expiry,
                    },
                );
            }
            return Ok(json(
                200,
                json!({
                    "ticket": key,
                    "expiresAt": utc_iso(expiry),
                }),
            ));
        }

        if method == Method::GET && path == "/api/orchestration/shell" {
            let snap = core_http_json(&self.0.opts.core_socket, Method::GET, "/v1/snapshot", None)
                .await
                .map_err(|_| 502u16)?;
            return Ok(json(200, build_t3_shell_snapshot(&snap, &self.0.opts)));
        }

        if method == Method::GET
            && let Some(tail) = path.strip_prefix("/api/orchestration/threads/")
        {
            let (id, suffix) = tail.split_once('/').unwrap_or((tail, ""));
            let snap = core_http_json(&self.0.opts.core_socket, Method::GET, "/v1/snapshot", None)
                .await
                .map_err(|_| 502u16)?;
            let seq = snap["sequence"].as_i64().unwrap_or(0);
            let Some(projection) = build_t3_thread_projection_opt(&snap, id, &self.0.opts) else {
                return Ok(json(404, json!({"message": "会话不存在"})));
            };
            let result = match suffix {
                "" => json!({
                    "snapshotSequence": seq,
                    "projection": projection,
                }),
                "bounded" => json!({
                    "snapshotSequence": seq,
                    "projection": projection,
                    "historyCursor": Value::Null,
                    "hasMoreHistory": false,
                    "latestLocalTurnOrdinal": Value::Null,
                }),
                "history" => json!({
                    "snapshotSequence": seq,
                    "items": [],
                    "nextCursor": Value::Null,
                    "hasMoreHistory": false,
                }),
                "transcript" => json!({
                    "threadId": id,
                    "title": projection["thread"]["title"],
                    "updatedAt": projection["updatedAt"],
                    "items": projection["visibleTurnItems"],
                }),
                _ => return Err(404),
            };
            return Ok(json(200, result));
        }

        Err(404)
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
        let active_reqs: Arc<Mutex<HashMap<String, CancellationToken>>> =
            Arc::new(Mutex::new(HashMap::new()));
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
            let Ok(parsed) = serde_json::from_str::<Value>(&text) else {
                continue;
            };
            let frames = match parsed {
                Value::Array(arr) => arr,
                other => vec![other],
            };
            for req in frames {
                // Effect-RPC wire protocol (`_tag` / `tag` / `payload`) used by T3 clients.
                if let Some(tag) = req.get("_tag").and_then(Value::as_str) {
                    match tag {
                        "Ping" => {
                            if tx
                                .send(Message::text(json!({"_tag": "Pong"}).to_string()))
                                .await
                                .is_err()
                            {
                                stop.cancel();
                                break;
                            }
                        }
                        "Ack" | "Pong" | "Eof" | "ResendPreviousSeq" => {}
                        "Interrupt" => {
                            if let Some(req_id) = req.get("requestId") {
                                let key = req_id.to_string();
                                if let Some(tok) = active_reqs.lock().unwrap().remove(&key) {
                                    tok.cancel();
                                }
                            }
                        }
                        "Request" => {
                            let req_id = req.get("id").cloned().unwrap_or(Value::Null);
                            let method = req["tag"].as_str().unwrap_or("").to_owned();
                            let payload = req.get("payload").cloned().unwrap_or(json!({}));
                            if req_id.is_null() || method.is_empty() {
                                continue;
                            }
                            let key = req_id.to_string();
                            let req_stop = stop.child_token();
                            {
                                let mut map = active_reqs.lock().unwrap();
                                if map.contains_key(&key) || map.len() >= 256 {
                                    let _ = tx.try_send(Message::text(
                                        t3_exit(&req_id, Err(anyhow!("重复或超限的 RPC 请求")))
                                            .to_string(),
                                    ));
                                    continue;
                                }
                                map.insert(key.clone(), req_stop.clone());
                            }
                            let this = self.clone();
                            let sock = core_socket.clone();
                            let tx_clone = tx.clone();
                            let reqs_map = active_reqs.clone();
                            tokio::spawn(async move {
                                this.handle_effect_rpc(
                                    sock, &req_id, &method, payload, tx_clone, req_stop,
                                )
                                .await;
                                reqs_map.lock().unwrap().remove(&key);
                            });
                        }
                        _ => {}
                    }
                    continue;
                }

                // Backwards-compatible JSON-RPC 2.0 dispatch (`method` / `params`).
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
                        core_http_json(&core_socket, Method::POST, "/v1/commands", Some(params))
                            .await
                    }
                    "workbench" => {
                        core_http_json(&core_socket, Method::POST, "/v1/workbench", Some(params))
                            .await
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
                    stop.cancel();
                    break;
                }
            }
        }
        stop.cancel();
        let _ = writer.await;
    }

    fn t3_descriptor(&self) -> Value {
        json!({
            "environmentId": self.environment_id(),
            "label": self.0.opts.name,
            "platform": {
                "os": std::env::consts::OS,
                "arch": std::env::consts::ARCH,
            },
            "version": self.0.opts.version,
        })
    }

    fn t3_server_config(&self) -> Value {
        json!({
            "environment": self.t3_descriptor(),
            "providers": [{
                "provider": self.0.opts.provider,
                "enabled": true,
                "installed": true,
                "models": if self.0.opts.model.is_empty() { json!([]) } else { json!([{"id": self.0.opts.model, "name": self.0.opts.model}]) },
            }],
            "cwd": self.0.opts.data_dir,
        })
    }

    async fn handle_effect_rpc(
        &self,
        core_socket: PathBuf,
        req_id: &Value,
        method: &str,
        payload: Value,
        tx: mpsc::Sender<Message>,
        req_stop: CancellationToken,
    ) {
        let send_chunk = |val: Value| {
            let msg = json!({"_tag": "Chunk", "requestId": req_id, "values": [val]});
            tx.send(Message::text(msg.to_string()))
        };
        match method {
            "subscribeServerConfig" => {
                let _ = send_chunk(json!({
                    "version": 1,
                    "type": "snapshot",
                    "config": self.t3_server_config(),
                }))
                .await;
                req_stop.cancelled().await;
                return;
            }
            "subscribeServerLifecycle" => {
                let env = self.t3_descriptor();
                let _ = send_chunk(json!({
                    "version": 1,
                    "sequence": 1,
                    "type": "welcome",
                    "payload": {
                        "environment": env,
                        "cwd": self.0.opts.data_dir,
                        "projectName": self.0.opts.name,
                        "bootstrapStatus": "complete",
                    }
                }))
                .await;
                let _ = send_chunk(json!({
                    "version": 1,
                    "sequence": 2,
                    "type": "ready",
                    "payload": {
                        "at": Utc::now().to_rfc3339(),
                        "environment": self.t3_descriptor(),
                    }
                }))
                .await;
                req_stop.cancelled().await;
                return;
            }
            "subscribeProjectClones" => {
                let _ = send_chunk(json!([])).await;
                req_stop.cancelled().await;
                return;
            }
            "subscribeVcsBranch" => {
                let _ = send_chunk(json!({"refName": null, "isRepo": false})).await;
                req_stop.cancelled().await;
                return;
            }
            "subscribeVcsStatus" => {
                let _ = send_chunk(json!({
                    "_tag": "snapshot",
                    "local": {
                        "isRepo": false,
                        "hasPrimaryRemote": false,
                        "isDefaultRef": false,
                        "refName": null,
                        "hasWorkingTreeChanges": false,
                        "workingTree": {"files": [], "insertions": 0, "deletions": 0},
                    },
                    "remote": null,
                }))
                .await;
                req_stop.cancelled().await;
                return;
            }
            "subscribeTerminalMetadata" => {
                let _ = send_chunk(json!({"type": "snapshot", "terminals": []})).await;
                req_stop.cancelled().await;
                return;
            }
            "subscribeWorktreeSetup" => {
                let _ = send_chunk(Value::Null).await;
                req_stop.cancelled().await;
                return;
            }
            "subscribeDiscoveredLocalServers" => {
                let _ = send_chunk(json!({"servers": [], "scannedAt": Utc::now().to_rfc3339()}))
                    .await;
                req_stop.cancelled().await;
                return;
            }
            "scheduledTasks.subscribe" => {
                let _ = send_chunk(json!({"tasks": []})).await;
                req_stop.cancelled().await;
                return;
            }
            "subscribePreviewEvents" => {
                req_stop.cancelled().await;
                return;
            }
            "orchestration.subscribeShell" | "orchestration.subscribeThread" => {
                let filter_thread = payload["threadId"].as_str().unwrap_or("").to_owned();
                let snap = core_http_json(&core_socket, Method::GET, "/v1/snapshot", None).await;
                let after = snap
                    .as_ref()
                    .ok()
                    .and_then(|s| s["sequence"].as_i64())
                    .unwrap_or(0);
                if let Ok(s) = snap {
                    let initial = if method == "orchestration.subscribeThread" && !filter_thread.is_empty() {
                        build_t3_thread_projection(&s, &filter_thread, &self.0.opts)
                    } else {
                        build_t3_shell_snapshot(&s, &self.0.opts)
                    };
                    let _ = send_chunk(json!({
                        "type": "snapshot",
                        "snapshotSequence": after,
                        "snapshot": initial,
                    }))
                    .await;
                }
                let _ = stream_t3_effect_events(
                    &core_socket,
                    after,
                    req_id.clone(),
                    filter_thread,
                    tx,
                    req_stop,
                )
                .await;
                return;
            }
            _ => {}
        }

        let result: Result<Value> = match method {
            "orchestration.getShellSnapshot" => {
                core_http_json(&core_socket, Method::GET, "/v1/snapshot", None)
                    .await
                    .map(|s| build_t3_shell_snapshot(&s, &self.0.opts))
            }
            "orchestration.getArchivedShellSnapshot" => {
                core_http_json(&core_socket, Method::GET, "/v1/snapshot", None)
                    .await
                    .map(|s| {
                        let shell = build_t3_shell_snapshot(&s, &self.0.opts);
                        json!({
                            "schemaVersion": 2,
                            "snapshotSequence": shell["snapshotSequence"],
                            "projects": shell["projects"],
                            "threads": shell["archivedThreads"],
                        })
                    })
            }
            "orchestration.getOlderSettledThreads" => {
                core_http_json(&core_socket, Method::GET, "/v1/snapshot", None)
                    .await
                    .map(|s| {
                        json!({
                            "snapshotSequence": s["sequence"].as_i64().unwrap_or(0),
                            "threads": [],
                            "counts": [],
                        })
                    })
            }
            "orchestration.getThreadProjection" => {
                let tid = payload["threadId"].as_str().unwrap_or("");
                core_http_json(&core_socket, Method::GET, "/v1/snapshot", None)
                    .await
                    .map(|s| build_t3_thread_projection(&s, tid, &self.0.opts))
            }
            "orchestration.getThreadSnapshot" => {
                let tid = payload["threadId"].as_str().unwrap_or("");
                core_http_json(&core_socket, Method::GET, "/v1/snapshot", None)
                    .await
                    .map(|s| {
                        json!({
                            "snapshotSequence": s["sequence"].as_i64().unwrap_or(0),
                            "projection": build_t3_thread_projection(&s, tid, &self.0.opts),
                        })
                    })
            }
            "orchestration.getThreadTitles" => {
                let ids: Vec<String> = payload["threadIds"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .filter_map(|v| v.as_str().map(str::to_owned))
                    .collect();
                core_http_json(&core_socket, Method::GET, "/v1/snapshot", None)
                    .await
                    .map(|s| {
                        let mut map = serde_json::Map::new();
                        for t in s["threads"].as_array().into_iter().flatten() {
                            if let Some(id) = t["id"].as_str()
                                && (ids.is_empty() || ids.iter().any(|wanted| wanted == id))
                            {
                                map.insert(
                                    id.to_owned(),
                                    Value::String(
                                        t["title"].as_str().unwrap_or_default().to_owned(),
                                    ),
                                );
                            }
                        }
                        Value::Object(map)
                    })
            }
            "orchestration.dispatchCommand" => {
                let cmd = payload
                    .get("command")
                    .filter(|v| v.is_object())
                    .cloned()
                    .unwrap_or(payload);
                core_http_json(&core_socket, Method::POST, "/v1/commands", Some(cmd)).await
            }
            "orchestration.launchThread" => {
                core_http_json(
                    &core_socket,
                    Method::POST,
                    "/v1/orchestration/threads/launch",
                    Some(payload),
                )
                .await
            }
            "server.getConfig" | "getServerConfig" => Ok(self.t3_server_config()),
            "server.refreshProviders" => {
                let cfg = self.t3_server_config();
                Ok(json!({"providers": cfg["providers"]}))
            }
            "server.probe" | "server.reportClientActivity" => Ok(json!({})),
            other => Err(anyhow!("unknown T3 RPC method {other}")),
        };
        let _ = tx
            .send(Message::text(t3_exit(req_id, result).to_string()))
            .await;
    }
}

fn t3_exit(req_id: &Value, result: Result<Value>) -> Value {
    let exit = match result {
        Ok(val) => json!({"_tag": "Success", "value": val}),
        Err(err) => json!({
            "_tag": "Failure",
            "cause": [{"_tag": "Die", "defect": {"name": "Error", "message": err.to_string()}}]
        }),
    };
    json!({"_tag": "Exit", "requestId": req_id, "exit": exit})
}

fn utc_iso(t: DateTime<Utc>) -> String {
    t.format("%Y-%m-%dT%H:%M:%S%.3fZ").to_string()
}

fn auth_error_reply() -> Reply {
    json(
        401,
        json!({
            "_tag": "EnvironmentAuthInvalidError",
            "code": "auth_invalid",
            "reason": "invalid_credential",
            "traceId": token(),
        }),
    )
}

fn apply_cors_headers(reply: &mut Reply, origin: Option<&str>) {
    let headers = reply.headers_mut();
    if let Some(o) = origin {
        if let Ok(v) = o.parse() {
            headers.insert("access-control-allow-origin", v);
            headers.insert("access-control-allow-credentials", "true".parse().unwrap());
            headers.insert("vary", "Origin".parse().unwrap());
        }
    } else {
        headers.insert("access-control-allow-origin", "*".parse().unwrap());
    }
    headers.insert(
        "access-control-allow-headers",
        "Authorization, Content-Type, DPoP, x-t3-orchestration-protocol"
            .parse()
            .unwrap(),
    );
    headers.insert(
        "access-control-allow-methods",
        "GET, POST, OPTIONS".parse().unwrap(),
    );
    headers.insert("cache-control", "no-store".parse().unwrap());
}

fn extract_request_token(headers: &HeaderMap) -> (String, &'static str) {
    let b = bearer(headers).trim();
    if !b.is_empty() {
        return (b.to_owned(), "bearer-access-token");
    }
    for header in headers.get_all("cookie") {
        let Ok(raw) = header.to_str() else { continue };
        for part in raw.split(';') {
            let Some((name, val)) = part.trim().split_once('=') else {
                continue;
            };
            let name = name.trim();
            let val = val.trim();
            if !val.is_empty() && (name == "vantry_session" || name.starts_with("t3_session")) {
                return (val.to_owned(), "browser-session-cookie");
            }
        }
    }
    (String::new(), "")
}

async fn decode_json_or_form(req: Request<Incoming>) -> Result<Value, u16> {
    let is_form = req
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|ct| ct.to_ascii_lowercase().contains("application/x-www-form-urlencoded"));
    let body = tokio::time::timeout(
        std::time::Duration::from_secs(10),
        Limited::new(req.into_body(), 32 << 10).collect(),
    )
    .await
    .map_err(|_| 408u16)?
    .map_err(|_| 400u16)?
    .to_bytes();
    if is_form || (!body.is_empty() && !body.starts_with(b"{")) {
        let mut map = serde_json::Map::new();
        for (k, v) in url::form_urlencoded::parse(&body) {
            map.insert(k.into_owned(), Value::String(v.into_owned()));
        }
        if !map.is_empty() {
            return Ok(Value::Object(map));
        }
    }
    serde_json::from_slice(&body).map_err(|_| 400)
}

fn session_scopes_and_permissions(raw: &str) -> (Vec<String>, Vec<String>) {
    const LEGACY_ONLY: &[&str] = &[
        "orchestration:read",
        "orchestration:operate",
        "terminal:operate",
        "review:write",
        "access:read",
        "access:write",
        "relay:read",
        "relay:write",
    ];
    let mut all = Vec::new();
    let mut legacy = Vec::new();
    let mut has_fine = false;
    for sc in raw.split_whitespace() {
        if all.iter().any(|s: &String| s == sc) {
            continue;
        }
        all.push(sc.to_owned());
        if LEGACY_ONLY.contains(&sc) {
            legacy.push(sc.to_owned());
        } else {
            has_fine = true;
        }
    }
    if all.is_empty() {
        let default = vec!["orchestration:read".into(), "orchestration:operate".into()];
        return (default, Vec::new());
    }
    if legacy.is_empty() {
        legacy = vec!["orchestration:read".into(), "orchestration:operate".into()];
    }
    if has_fine {
        (legacy, all)
    } else {
        (legacy, Vec::new())
    }
}

fn verify_legacy_session(state: &AuthState, candidate: &str, now: DateTime<Utc>) -> Option<T3Client> {
    if state.legacy_signing_key.is_empty() || state.legacy_sessions.is_empty() {
        return None;
    }
    let key = STANDARD.decode(&state.legacy_signing_key).ok()?;
    if key.len() < 16 {
        return None;
    }
    let (payload_b64, sig_b64) = candidate.split_once('.')?;
    if payload_b64.is_empty() || sig_b64.is_empty() || sig_b64.contains('.') {
        return None;
    }
    let got_sig = URL_SAFE_NO_PAD.decode(sig_b64).ok()?;
    let mac_key = hmac::Key::new(hmac::HMAC_SHA256, &key);
    hmac::verify(&mac_key, payload_b64.as_bytes(), &got_sig).ok()?;
    let raw_claims = URL_SAFE_NO_PAD.decode(payload_b64).ok()?;
    let claims: Value = serde_json::from_slice(&raw_claims).ok()?;
    let sid = claims["sid"].as_str()?;
    let kind = claims["kind"].as_str().unwrap_or("");
    if sid.is_empty() || (!kind.is_empty() && kind != "session") {
        return None;
    }
    if let Some(exp) = claims["exp"].as_i64().filter(|&e| e > 0) {
        let exp_dt = if exp > 1_000_000_000_000 {
            DateTime::from_timestamp_millis(exp)
        } else {
            DateTime::from_timestamp(exp, 0)
        };
        if let Some(dt) = exp_dt
            && now >= dt
        {
            return None;
        }
    }
    let legacy = state
        .legacy_sessions
        .iter()
        .find(|s| s.id == sid && s.expires_at > now)?;
    let sc = if !legacy.scopes.is_empty() {
        legacy.scopes.clone()
    } else {
        claims["scopes"]
            .as_array()
            .map(|arr| {
                arr.iter()
                    .filter_map(Value::as_str)
                    .collect::<Vec<_>>()
                    .join(" ")
            })
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| SCOPES.to_owned())
    };
    Some(T3Client {
        id: legacy.id.clone(),
        name: if legacy.name.is_empty() {
            claims["label"].as_str().unwrap_or("T3 客户端").to_owned()
        } else {
            legacy.name.clone()
        },
        token: candidate.to_owned(),
        created_at: legacy.created_at,
        expires_at: legacy.expires_at,
        scopes: sc,
    })
}

const SCRATCH_PROJECT_IDENTITY: &str = "vantry:scratch";

fn t3_project_id(identity: &str) -> String {
    format!("workspace_{}", URL_SAFE_NO_PAD.encode(identity.as_bytes()))
}

fn is_scratch_path(path: &str, data_dir: &Path) -> bool {
    let trimmed = path.trim();
    if trimmed.is_empty() {
        return true;
    }
    let normalized = trimmed.replace('\\', "/");
    let p = Path::new(trimmed);
    if p.file_name().and_then(|s| s.to_str()) != Some("chat") {
        return false;
    }
    let ws_root = data_dir.join("workspaces");
    if p.parent().and_then(|d| d.parent()) == Some(ws_root.as_path()) {
        return true;
    }
    normalized.ends_with("/chat") && normalized.contains("/workspaces/")
}

fn thread_workspace_path(thread: &Value) -> String {
    let ws = &thread["workspace"];
    for key in ["repository", "path"] {
        if let Some(s) = ws[key].as_str().map(str::trim).filter(|s| !s.is_empty()) {
            return s.to_owned();
        }
    }
    thread["projectPath"]
        .as_str()
        .map(str::trim)
        .unwrap_or_default()
        .to_owned()
}

fn thread_project_identity(thread: &Value, opts: &T3Options) -> (String, String) {
    let env_id = thread["workspace"]["environmentId"]
        .as_str()
        .map(str::trim)
        .unwrap_or("");
    let raw = thread_workspace_path(thread);
    if !env_id.is_empty() {
        let path = if raw.is_empty() {
            opts.data_dir.display().to_string()
        } else {
            raw
        };
        return (format!("environment:{env_id}\n{path}"), path);
    }
    if is_scratch_path(&raw, &opts.data_dir) {
        return (SCRATCH_PROJECT_IDENTITY.to_owned(), String::new());
    }
    (raw.clone(), raw)
}

fn t3_runtime_mode(policy: &str) -> &'static str {
    match policy {
        "auto" => "full-access",
        "auto-accept-edits" => "auto-accept-edits",
        _ => "approval-required",
    }
}

fn t3_run_status(status: &str) -> &str {
    match status {
        "waiting_children" => "waiting",
        "" => "idle",
        other => other,
    }
}

fn is_terminal_run_status(status: &str) -> bool {
    matches!(status, "completed" | "failed" | "cancelled" | "interrupted")
}

fn t3_model_selection(provider: &str, model: &str, opts: &T3Options) -> Value {
    let p = if !provider.is_empty() {
        provider
    } else if !opts.provider.is_empty() {
        opts.provider.as_str()
    } else {
        "pi"
    };
    let m = if !model.is_empty() {
        model
    } else if !opts.model.is_empty() {
        opts.model.as_str()
    } else {
        "default"
    };
    json!({"instanceId": p, "model": m})
}

fn project_thread_base(thread: &Value, opts: &T3Options) -> Value {
    let id = thread["id"].as_str().unwrap_or_default();
    let title = thread["title"]
        .as_str()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or("新会话");
    let created = thread["createdAt"].as_str().unwrap_or("");
    let provider = thread["providerId"].as_str().unwrap_or("");
    let model = thread["model"].as_str().unwrap_or("");
    let policy = thread["approvalPolicy"].as_str().unwrap_or("");
    let interaction = thread["interactionMode"]
        .as_str()
        .filter(|s| !s.is_empty())
        .unwrap_or("default");
    let (identity, _) = thread_project_identity(thread, opts);
    let parent_raw = thread["parentThreadId"].as_str().unwrap_or("");
    let forked_from = thread["forkedFromThreadId"].as_str().unwrap_or("");
    let (parent, relationship) = if !forked_from.is_empty() {
        (forked_from, Value::String("fork".into()))
    } else if !parent_raw.is_empty() {
        (parent_raw, Value::String("subagent".into()))
    } else {
        ("", Value::Null)
    };
    let archived = thread["archived"].as_bool().unwrap_or(false);
    let pinned = thread["pinned"].as_bool().unwrap_or(false);
    let snoozed = thread["snoozedUntil"]
        .as_str()
        .filter(|s| !s.is_empty())
        .map(|s| Value::String(s.to_owned()))
        .unwrap_or(Value::Null);
    let branch = thread["workspace"]["branch"]
        .as_str()
        .filter(|s| !s.is_empty())
        .map(|s| Value::String(s.to_owned()))
        .unwrap_or(Value::Null);
    let worktree = if thread["workspace"]["kind"].as_str() == Some("worktree") {
        thread["workspace"]["path"]
            .as_str()
            .filter(|s| !s.is_empty())
            .map(|s| Value::String(s.to_owned()))
            .unwrap_or(Value::Null)
    } else {
        Value::Null
    };
    let eff_provider = if !provider.is_empty() {
        provider
    } else if !opts.provider.is_empty() {
        opts.provider.as_str()
    } else {
        "pi"
    };
    json!({
        "id": id,
        "projectId": t3_project_id(&identity),
        "title": title,
        "providerInstanceId": eff_provider,
        "modelSelection": t3_model_selection(provider, model, opts),
        "runtimeMode": t3_runtime_mode(policy),
        "interactionMode": interaction,
        "branch": branch,
        "worktreePath": worktree,
        "createdBy": "user",
        "creationSource": "server",
        "lineage": {
            "parentThreadId": if parent.is_empty() { Value::Null } else { Value::String(parent.to_owned()) },
            "relationshipToParent": relationship,
            "rootThreadId": if parent.is_empty() { id } else { parent },
        },
        "forkedFrom": Value::Null,
        "activeProviderThreadId": Value::Null,
        "createdAt": created,
        "updatedAt": created,
        "archivedAt": if archived { Value::String(created.to_owned()) } else { Value::Null },
        "settledOverride": if archived { Value::String("settled".into()) } else { Value::Null },
        "settledAt": if archived { Value::String(created.to_owned()) } else { Value::Null },
        "lastVisitedAt": Value::Null,
        "deletedAt": Value::Null,
        "pinnedAt": if pinned { Value::String(created.to_owned()) } else { Value::Null },
        "snoozedUntil": snoozed,
    })
}

fn build_t3_shell_snapshot(snap: &Value, opts: &T3Options) -> Value {
    let seq = snap["sequence"].as_i64().unwrap_or(0);
    let runs = snap["runs"].as_array().map(Vec::as_slice).unwrap_or(&[]);
    let tools = snap["tools"].as_array().map(Vec::as_slice).unwrap_or(&[]);
    let requests = snap["requests"].as_array().map(Vec::as_slice).unwrap_or(&[]);
    let inputs = snap["inputs"].as_array().map(Vec::as_slice).unwrap_or(&[]);

    let mut project_meta: BTreeMap<String, (String, String)> = BTreeMap::new();
    let mut active = Vec::new();
    let mut archived = Vec::new();

    for t in snap["threads"].as_array().into_iter().flatten() {
        let tid = t["id"].as_str().unwrap_or_default();
        let created = t["createdAt"].as_str().unwrap_or("").to_owned();
        let (identity, root) = thread_project_identity(t, opts);
        project_meta
            .entry(identity)
            .or_insert_with(|| (root, created.clone()));

        let mut row = project_thread_base(t, opts);
        row["status"] = json!("idle");
        row["latestRunId"] = Value::Null;
        row["activeRunId"] = Value::Null;
        row["pendingRuntimeRequest"] = Value::Null;
        row["latestVisibleMessage"] = Value::Null;
        row["latestUserMessageAt"] = Value::Null;
        row["hasActionableProposedPlan"] = json!(false);

        let mut count = 0usize;
        let mut latest_run: Option<&Value> = None;
        for run in runs.iter().filter(|r| r["threadId"].as_str() == Some(tid)) {
            count += 1;
            if !run["output"].as_str().unwrap_or("").is_empty() {
                count += 1;
            }
            if !run["reasoning"].as_str().unwrap_or("").is_empty() {
                count += 1;
            }
            let cur = run["createdAt"].as_str().unwrap_or("");
            if latest_run.is_none_or(|prev| cur > prev["createdAt"].as_str().unwrap_or("")) {
                latest_run = Some(run);
            }
        }
        count += tools.iter().filter(|x| x["threadId"].as_str() == Some(tid)).count();
        count += requests.iter().filter(|x| x["threadId"].as_str() == Some(tid)).count();
        count += inputs
            .iter()
            .filter(|x| {
                x["threadId"].as_str() == Some(tid)
                    && ((x["kind"].as_str() == Some("queue") && x["status"].as_str() == Some("queued"))
                        || (x["kind"].as_str() == Some("steer") && x["status"].as_str() != Some("cancelled")))
            })
            .count();
        row["itemCount"] = json!(count);
        row["visibleItemCount"] = json!(count);

        if let Some(run) = latest_run {
            let status = run["status"].as_str().unwrap_or("idle");
            let rid = run["id"].as_str().unwrap_or("");
            let r_updated = run["updatedAt"].as_str().unwrap_or("");
            let r_created = run["createdAt"].as_str().unwrap_or("");
            row["status"] = json!(t3_run_status(status));
            row["latestRunId"] = json!(rid);
            if !r_updated.is_empty() {
                row["updatedAt"] = json!(r_updated);
            }
            if !r_created.is_empty() {
                row["latestUserMessageAt"] = json!(r_created);
            }
            if !is_terminal_run_status(status) {
                row["activeRunId"] = json!(rid);
            }
            let output = run["output"].as_str().unwrap_or("");
            let (role, text, msg_id) = if !output.is_empty() {
                ("assistant", output, format!("{rid}/assistant"))
            } else {
                ("user", run["input"].as_str().unwrap_or(""), format!("{rid}/user"))
            };
            row["latestVisibleMessage"] = json!({
                "id": msg_id,
                "role": role,
                "text": text,
                "updatedAt": r_updated,
            });
        }

        if let Some(req) = requests
            .iter()
            .find(|r| r["threadId"].as_str() == Some(tid) && r["status"].as_str() == Some("pending"))
        {
            let kind = if req["method"].as_str() == Some("confirm") {
                "permission"
            } else {
                "user_input"
            };
            row["pendingRuntimeRequest"] = json!({
                "id": req["id"],
                "kind": kind,
                "createdAt": req["createdAt"],
            });
        }

        if t["archived"].as_bool().unwrap_or(false) {
            archived.push(row);
        } else {
            active.push(row);
        }
    }

    let mut projects = Vec::with_capacity(project_meta.len());
    for (identity, (root, created)) in project_meta {
        if identity == SCRATCH_PROJECT_IDENTITY {
            projects.push(json!({
                "id": t3_project_id(&identity),
                "title": "No project",
                "workspaceRoot": "",
                "isScratch": true,
                "defaultModelSelection": t3_model_selection("", "", opts),
                "scripts": [],
                "createdAt": created,
                "updatedAt": created,
            }));
        } else {
            let title = Path::new(&root)
                .file_name()
                .and_then(|s| s.to_str())
                .filter(|s| !s.is_empty())
                .unwrap_or("Vantry");
            projects.push(json!({
                "id": t3_project_id(&identity),
                "title": title,
                "workspaceRoot": root,
                "defaultModelSelection": t3_model_selection("", "", opts),
                "scripts": [],
                "createdAt": created,
                "updatedAt": created,
            }));
        }
    }

    json!({
        "schemaVersion": 2,
        "snapshotSequence": seq,
        "projects": projects,
        "threads": active,
        "archivedThreads": archived,
    })
}

fn build_t3_thread_projection(snap: &Value, thread_id: &str, opts: &T3Options) -> Value {
    build_t3_thread_projection_opt(snap, thread_id, opts).unwrap_or_else(|| {
        json!({
            "threadId": thread_id,
            "thread": Value::Null,
            "updatedAt": "",
            "runs": [],
            "attempts": [],
            "nodes": [],
            "subagents": [],
            "providerSessions": [],
            "providerThreads": [],
            "providerTurns": [],
            "runtimeRequests": [],
            "messages": [],
            "plans": [],
            "turnItems": [],
            "checkpointScopes": [],
            "checkpoints": [],
            "contextHandoffs": [],
            "contextTransfers": [],
            "visibleTurnItems": [],
        })
    })
}

fn build_t3_thread_projection_opt(snap: &Value, thread_id: &str, opts: &T3Options) -> Option<Value> {
    let source = snap["threads"]
        .as_array()?
        .iter()
        .find(|t| t["id"].as_str() == Some(thread_id))?;
    let thread_obj = project_thread_base(source, opts);
    let mut updated_at = source["createdAt"].as_str().unwrap_or("").to_owned();

    let mut runs_src: Vec<&Value> = snap["runs"]
        .as_array()
        .into_iter()
        .flatten()
        .filter(|r| r["threadId"].as_str() == Some(thread_id))
        .collect();
    runs_src.sort_by_key(|r| r["createdAt"].as_str().unwrap_or(""));

    let mut runs_out = Vec::new();
    let mut nodes_out = Vec::new();
    let mut messages_out = Vec::new();
    let mut turn_items_out = Vec::new();
    let mut visible_out = Vec::new();
    let mut runtime_requests_out = Vec::new();
    let mut ordinal = 0usize;

    let mut push_item = |item: Value| {
        let pos = visible_out.len();
        let item_id = item["id"].clone();
        visible_out.push(json!({
            "position": pos,
            "visibility": "local",
            "sourceThreadId": thread_id,
            "sourceItemId": item_id,
            "item": item,
        }));
        turn_items_out.push(item);
    };

    for (idx, run) in runs_src.iter().enumerate() {
        let rid = run["id"].as_str().unwrap_or("");
        let status = run["status"].as_str().unwrap_or("idle");
        let t3_status = t3_run_status(status);
        let created = run["createdAt"].as_str().unwrap_or("");
        let updated = run["updatedAt"].as_str().unwrap_or(created);
        if updated > updated_at.as_str() {
            updated_at = updated.to_owned();
        }
        let completed = if is_terminal_run_status(status) {
            Value::String(updated.to_owned())
        } else {
            Value::Null
        };
        let user_msg_id = format!("{rid}/user");
        let provider = run["providerId"].as_str().unwrap_or("");
        let model = run["model"].as_str().unwrap_or("");
        runs_out.push(json!({
            "id": rid,
            "threadId": thread_id,
            "ordinal": idx + 1,
            "providerInstanceId": if provider.is_empty() { opts.provider.as_str() } else { provider },
            "modelSelection": t3_model_selection(provider, model, opts),
            "providerThreadId": Value::Null,
            "userMessageId": user_msg_id,
            "rootNodeId": rid,
            "activeAttemptId": Value::Null,
            "status": t3_status,
            "requestedAt": created,
            "startedAt": created,
            "completedAt": completed,
            "checkpointId": Value::Null,
            "contextHandoffId": Value::Null,
        }));
        nodes_out.push(json!({
            "id": rid,
            "threadId": thread_id,
            "runId": rid,
            "parentNodeId": Value::Null,
            "rootNodeId": rid,
            "kind": "root_turn",
            "status": if t3_status == "queued" { "pending" } else { t3_status },
            "countsForRun": true,
            "providerThreadId": Value::Null,
            "providerTurnId": Value::Null,
            "nativeItemRef": Value::Null,
            "runtimeRequestId": Value::Null,
            "checkpointScopeId": Value::Null,
            "startedAt": Value::Null,
            "completedAt": completed,
        }));

        for role in ["user", "assistant"] {
            let text = if role == "user" {
                run["input"].as_str().unwrap_or("")
            } else {
                let out = run["output"].as_str().unwrap_or("");
                if out.is_empty() {
                    continue;
                }
                out
            };
            let msg_id = if role == "user" {
                user_msg_id.clone()
            } else {
                format!("{rid}/assistant")
            };
            let msg_updated = if role == "user" { created } else { updated };
            let streaming = role == "assistant" && !is_terminal_run_status(status);
            messages_out.push(json!({
                "id": msg_id,
                "threadId": thread_id,
                "runId": rid,
                "nodeId": Value::Null,
                "role": role,
                "text": text,
                "attachments": [],
                "streaming": streaming,
                "createdAt": created,
                "updatedAt": msg_updated,
                "createdBy": "user",
                "creationSource": "server",
            }));
            let mut item = json!({
                "id": msg_id,
                "threadId": thread_id,
                "runId": rid,
                "nodeId": Value::Null,
                "providerThreadId": Value::Null,
                "providerTurnId": Value::Null,
                "nativeItemRef": Value::Null,
                "parentItemId": Value::Null,
                "ordinal": ordinal,
                "status": if streaming { "running" } else { "completed" },
                "title": Value::Null,
                "startedAt": msg_updated,
                "completedAt": if streaming { Value::Null } else { Value::String(msg_updated.to_owned()) },
                "updatedAt": msg_updated,
                "type": format!("{role}_message"),
                "messageId": msg_id,
                "text": text,
                "streaming": streaming,
            });
            if role == "user" {
                item["inputIntent"] = json!("turn_start");
                item["attachments"] = json!([]);
                item["createdBy"] = json!("user");
            }
            ordinal += 1;
            push_item(item);
        }
    }

    for req in snap["requests"]
        .as_array()
        .into_iter()
        .flatten()
        .filter(|r| r["threadId"].as_str() == Some(thread_id))
    {
        let kind = if req["method"].as_str() == Some("confirm") {
            "permission"
        } else {
            "user_input"
        };
        runtime_requests_out.push(json!({
            "id": req["id"],
            "threadId": thread_id,
            "runId": req["runId"],
            "kind": kind,
            "status": req["status"],
            "title": req["title"],
            "prompt": req["prompt"],
            "createdAt": req["createdAt"],
        }));
    }

    Some(json!({
        "threadId": thread_id,
        "thread": thread_obj,
        "updatedAt": updated_at,
        "runs": runs_out,
        "attempts": [],
        "nodes": nodes_out,
        "subagents": [],
        "providerSessions": [],
        "providerThreads": [],
        "providerTurns": [],
        "runtimeRequests": runtime_requests_out,
        "messages": messages_out,
        "plans": [],
        "turnItems": turn_items_out,
        "checkpointScopes": [],
        "checkpoints": [],
        "contextHandoffs": [],
        "contextTransfers": [],
        "visibleTurnItems": visible_out,
    }))
}

async fn stream_t3_effect_events(
    core: &Path,
    after: i64,
    req_id: Value,
    filter_thread: String,
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
                    let ev = env.get("event").unwrap_or(&env);
                    if !filter_thread.is_empty()
                        && ev["threadId"].as_str() != Some(filter_thread.as_str())
                    {
                        continue;
                    }
                    let msg = json!({
                        "_tag": "Chunk",
                        "requestId": req_id,
                        "values": [{"type": "event", "event": ev}],
                    });
                    if tx.send(Message::text(msg.to_string())).await.is_err() {
                        driver.abort();
                        return Ok(());
                    }
                }
            }
        }
    }
    driver.abort();
    Ok(())
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
    let data = Limited::new(res.into_body(), 256 << 20)
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
        assert_eq!(meta["orchestrationProtocolVersion"], 2);
        assert_eq!(meta["capabilities"]["connectionProbe"], true);

        let unauth_session: Value = client
            .get(format!("http://{addr}/api/auth/session"))
            .send()
            .await?
            .json()
            .await?;
        assert_eq!(unauth_session["authenticated"], false);

        let exchanged: Value = client
            .post(format!("http://{addr}/oauth/token"))
            .form(&[
                ("grant_type", TOKEN_GRANT),
                ("subject_token", code.as_str()),
                ("subject_token_type", BOOTSTRAP_TYPE),
                ("requested_token_type", ACCESS_TYPE),
                ("client_label", "Vantry for iPhone"),
            ])
            .send()
            .await?
            .json()
            .await?;
        let access_token = exchanged["access_token"].as_str().unwrap().to_owned();
        assert!(!access_token.is_empty());

        let auth_session: Value = client
            .get(format!("http://{addr}/api/auth/session"))
            .bearer_auth(&access_token)
            .send()
            .await?
            .json()
            .await?;
        assert_eq!(auth_session["authenticated"], true);
        assert_eq!(auth_session["sessionMethod"], "bearer-access-token");

        let rpc_ticket: Value = client
            .post(format!("http://{addr}/api/auth/websocket-ticket/rpc"))
            .bearer_auth(&access_token)
            .send()
            .await?
            .json()
            .await?;
        let ticket = rpc_ticket["ticket"].as_str().unwrap();
        assert!(!ticket.is_empty());

        let (mut ws, _) = tokio_tungstenite::connect_async(format!(
            "ws://{addr}/ws?orchestrationProtocol=2&wsTicket={ticket}"
        ))
        .await?;
        use futures::SinkExt;
        ws.send(Message::text(r#"{"jsonrpc":"2.0","id":1,"method":"ping"}"#))
            .await?;
        let reply = ws.next().await.unwrap()?;
        let parsed: Value = serde_json::from_str(reply.to_text()?)?;
        assert_eq!(parsed["result"]["pong"], true);

        // Effect-RPC wire protocol (`_tag` / `tag` / `payload`) used by T3 clients.
        ws.send(Message::text(r#"{"_tag":"Ping"}"#)).await?;
        let pong = ws.next().await.unwrap()?;
        let pong_json: Value = serde_json::from_str(pong.to_text()?)?;
        assert_eq!(pong_json["_tag"], "Pong");

        ws.send(Message::text(
            r#"{"_tag":"Request","id":10,"tag":"server.getConfig","payload":{}}"#,
        ))
        .await?;
        let cfg_reply = ws.next().await.unwrap()?;
        let cfg_json: Value = serde_json::from_str(cfg_reply.to_text()?)?;
        assert_eq!(cfg_json["_tag"], "Exit");
        assert_eq!(cfg_json["requestId"], 10);
        assert_eq!(cfg_json["exit"]["_tag"], "Success");
        assert_eq!(
            cfg_json["exit"]["value"]["environment"]["environmentId"],
            "env_test"
        );

        server.revoke_all()?;
        assert!(server.clients().is_empty());
        stop.cancel();
        handle.await??;
        Ok(())
    }
}
