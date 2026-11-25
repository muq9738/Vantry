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
    pub identity: Option<Arc<crate::identity::IdentityStore>>,
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
    client_id: String,
    expires: DateTime<Utc>,
}

struct Inner {
    opts: T3Options,
    state: Mutex<AuthState>,
    codes: Mutex<HashMap<String, BootstrapCode>>,
    tickets: Mutex<HashMap<String, WsTicket>>,
    sessions: Mutex<Vec<(String, CancellationToken)>>,
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

    /// Mint the invitation on the same adapter that consumes it at /oauth/token.
    pub fn mobile_pairing_link(&self) -> Result<Option<String>> {
        let Some(identity) = &self.0.opts.identity else { return Ok(None) };
        let cfg = identity.snapshot();
        let routes = cfg.mobile_routes();
        let public = crate::transport::relay_url(cfg.public_url(), false).ok().filter(|url| {
            url.scheme() == "https" && url.path() == "/"
                && url.host_str().is_some_and(|h| {
                    let host = h.trim_end_matches('.').trim_matches(['[', ']']);
                    host != "localhost" && !host.ends_with(".localhost")
                        && !host.parse::<std::net::IpAddr>().is_ok_and(|ip| ip.is_loopback() || ip.is_unspecified())
                })
        });
        let base = public.as_ref().map(Url::as_str)
            .or_else(|| routes.as_ref().and_then(|r| r["direct"][0].as_str()));
        base.map(|url| self.pairing_link(url, routes.as_ref()).map(|(_, link)| link)).transpose()
    }

    pub fn revoke_all(&self) -> Result<()> {
        let mut state = self.0.state.lock().unwrap();
        state.clients.clear();
        self.save_locked(&state)?;
        self.0.codes.lock().unwrap().clear();
        self.0.tickets.lock().unwrap().clear();
        for (_, tok) in self.0.sessions.lock().unwrap().drain(..) {
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

    pub fn paired_devices(&self) -> Vec<Value> {
        let now = Utc::now();
        let state = self.0.state.lock().unwrap();
        state.clients.iter().filter(|c| c.expires_at > now)
            .map(|c| json!({"id":c.id,"name":c.name,"createdAt":c.created_at}))
            .chain(state.legacy_sessions.iter().filter(|c| c.expires_at > now)
                .map(|c| json!({"id":c.id,"name":c.name,"createdAt":c.created_at})))
            .collect()
    }

    pub fn revoke_client(&self, id: &str) -> Result<bool> {
        let mut state = self.0.state.lock().unwrap();
        let mut next = state.clone();
        next.clients.retain(|c| c.id != id);
        next.legacy_sessions.retain(|c| c.id != id);
        if next.clients.len() == state.clients.len() && next.legacy_sessions.len() == state.legacy_sessions.len() {
            return Ok(false);
        }
        self.save_locked(&next)?;
        *state = next;
        self.0.tickets.lock().unwrap().retain(|_, t| t.client_id != id);
        self.0.sessions.lock().unwrap().retain(|(owner, stop)| {
            if owner == id { stop.cancel(); }
            !stop.is_cancelled()
        });
        Ok(true)
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

    fn connection_routes(&self) -> Option<Value> {
        self.0
            .opts
            .identity
            .as_ref()
            .and_then(|id| id.snapshot().mobile_routes())
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
        let mut out = json!({
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
        });
        if let Some(routes) = self.connection_routes() {
            out["connectionRoutes"] = routes;
        }
        out
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
                    client_id: client.id.clone(),
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
                client_id: client.id.clone(),
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
            let client_id = if let Some(t) = ticket {
                let now = Utc::now();
                let mut map = self.0.tickets.lock().unwrap();
                map.retain(|_, v| v.expires > now);
                map.remove(&t).map(|ticket| ticket.client_id)
            } else {
                let (tok, _) = extract_request_token(req.headers());
                self.authorize(&tok).map(|client| client.id)
            };
            let client_id = client_id.ok_or(401u16)?;
            let (reply, up) = upgrade(&mut req)?;
            let session_stop = tasks.stop.child_token();
            {
                let state = self.0.state.lock().unwrap();
                let now = Utc::now();
                if !state.clients.iter().any(|c| c.id == client_id && c.expires_at > now)
                    && !state.legacy_sessions.iter().any(|c| c.id == client_id && c.expires_at > now) {
                    return Err(401);
                }
                self.0.sessions.lock().unwrap().push((client_id, session_stop.clone()));
            }
            let this = self.clone();
            tasks.spawn(async move {
                if let Ok(ws) = accepted(up, 64 << 20).await {
                    this.drive_ws(ws, session_stop.clone()).await;
                }
                session_stop.cancel();
                this.0.sessions.lock().unwrap().retain(|(_, stop)| !stop.is_cancelled());
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
                if let Some(routes) = self.connection_routes() {
                    result["connectionRoutes"] = routes;
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
                        client_id: client.id.clone(),
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
                    "workbench" if crate::peer::host_pairing_action(params["action"].as_str().unwrap_or("")) => Err(anyhow::anyhow!("pairing management requires the local host")),
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
        let arch = match std::env::consts::ARCH {
            "x86_64" => "x64",
            "aarch64" => "arm64",
            _ => "other",
        };
        let os = match std::env::consts::OS {
            "macos" => "darwin",
            other => other,
        };
        let mut out = json!({
            "environmentId": self.environment_id(),
            "label": self.0.opts.name,
            "platform": {
                "os": os,
                "arch": arch,
            },
            "version": self.0.opts.version,
        });
        if let Some(routes) = self.connection_routes() {
            out["connectionRoutes"] = routes;
        }
        out
    }

    async fn t3_server_config(&self, core_socket: &Path) -> Value {
        let provider = if self.0.opts.provider.trim().is_empty() {
            "pi"
        } else {
            self.0.opts.provider.as_str()
        };
        let mut entries: Vec<Value> = Vec::new();
        let mut has_default = false;
        if let Ok(Value::Array(list)) =
            core_http_json(core_socket, Method::GET, "/v1/models", None).await
        {
            for item in list {
                let id = item["id"].as_str().unwrap_or("").trim();
                let p = item["provider"].as_str().unwrap_or("").trim();
                let slug = if !id.is_empty() {
                    if !p.is_empty() && !id.starts_with(&format!("{p}/")) {
                        format!("{p}/{id}")
                    } else {
                        id.to_owned()
                    }
                } else {
                    continue;
                };
                let name = item["name"]
                    .as_str()
                    .map(str::trim)
                    .filter(|s| !s.is_empty())
                    .unwrap_or(&slug)
                    .to_owned();
                let is_default = !self.0.opts.model.is_empty() && slug == self.0.opts.model;
                if is_default {
                    has_default = true;
                }
                entries.push(json!({
                    "slug": slug,
                    "name": name,
                    "isCustom": false,
                    "capabilities": {"optionDescriptors": []},
                    "isDefault": is_default,
                }));
            }
        }
        if entries.is_empty() && !self.0.opts.model.is_empty() {
            has_default = true;
            entries.push(json!({
                "slug": self.0.opts.model,
                "name": self.0.opts.model,
                "isCustom": false,
                "capabilities": {"optionDescriptors": []},
                "isDefault": true,
            }));
        }
        if !entries.is_empty() && !has_default {
            entries[0]["isDefault"] = json!(true);
        }
        let now_iso = utc_iso(Utc::now());
        json!({
            "environment": self.t3_descriptor(),
            "auth": self.auth_descriptor(),
            "cwd": self.0.opts.data_dir,
            "keybindingsConfigPath": self.0.opts.data_dir.join("keybindings.json"),
            "keybindings": [],
            "issues": [],
            "providers": [{
                "instanceId": provider,
                "driver": "pi",
                "displayName": "Pi",
                "continuation": {"groupKey": format!("pi:instance:{provider}")},
                "showInteractionModeToggle": false,
                "reportsContextWindow": true,
                "supportsTextGeneration": true,
                "supportedRuntimeModes": ["approval-required", "auto-accept-edits", "full-access"],
                "setup": {"canAuthenticate": false, "canInstall": false},
                "enabled": true,
                "installed": true,
                "version": if self.0.opts.version.is_empty() { "0.1.0" } else { self.0.opts.version.as_str() },
                "status": "ready",
                "auth": {"status": "authenticated", "type": "pi"},
                "checkedAt": now_iso,
                "models": entries,
                "slashCommands": [],
                "skills": [],
            }],
            "availableEditors": [],
            "settings": {},
            "observability": {
                "logsDirectoryPath": self.0.opts.data_dir,
                "localTracingEnabled": false,
                "otlpTracesEnabled": false,
                "otlpMetricsEnabled": false,
            },
            "serverTime": now_iso,
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
                let cfg = self.t3_server_config(&core_socket).await;
                let _ = send_chunk(json!({
                    "version": 1,
                    "type": "snapshot",
                    "config": cfg,
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
                        "at": utc_iso(Utc::now()),
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
                let _ = send_chunk(json!({"servers": [], "scannedAt": utc_iso(Utc::now())}))
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
                let is_shell = method == "orchestration.subscribeShell";
                let filter_thread = payload["threadId"].as_str().unwrap_or("").to_owned();
                let snap = match mobile_snapshot(&core_socket, (!is_shell).then_some(filter_thread.as_str())).await {
                    Ok(s) => s,
                    Err(err) => {
                        let _ = tx
                            .send(Message::text(t3_exit(req_id, Err(err)).to_string()))
                            .await;
                        return;
                    }
                };
                let after = snap["sequence"].as_i64().unwrap_or(0);
                let initial_frame = if is_shell {
                    json!({
                        "kind": "snapshot",
                        "snapshot": build_t3_shell_snapshot(&snap, &self.0.opts),
                    })
                } else {
                    let Some(proj) = build_t3_thread_projection_opt(&snap, &filter_thread, &self.0.opts) else {
                        let _ = tx
                            .send(Message::text(t3_exit(req_id, Err(anyhow!("会话不存在"))).to_string()))
                            .await;
                        return;
                    };
                    json!({
                        "kind": "snapshot",
                        "snapshotSequence": after,
                        "projection": proj,
                        "historyCursor": Value::Null,
                        "hasMoreHistory": false,
                        "latestLocalTurnOrdinal": Value::Null,
                    })
                };
                if send_chunk(initial_frame).await.is_err() {
                    return;
                }
                if send_chunk(json!({"kind": "synchronized"})).await.is_err() {
                    return;
                }
                let result = stream_t3_effect_events(
                    &core_socket,
                    &self.0.opts,
                    after,
                    req_id.clone(),
                    is_shell,
                    filter_thread,
                    tx.clone(),
                    req_stop.clone(),
                )
                .await;
                if !req_stop.is_cancelled() {
                    let failure = result.err().unwrap_or_else(|| anyhow!("Core subscription closed"));
                    let _ = tx.send(Message::text(t3_exit(req_id, Err(failure)).to_string())).await;
                }
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
                dispatch_t3_command(&core_socket, &self.0.opts, &cmd).await
            }
            "orchestration.launchThread" => {
                launch_t3_thread(&core_socket, &self.0.opts, &payload).await
            }
            "server.getConfig" | "getServerConfig" => {
                Ok(self.t3_server_config(&core_socket).await)
            }
            "server.refreshProviders" => {
                let cfg = self.t3_server_config(&core_socket).await;
                Ok(json!({"providers": cfg["providers"]}))
            }
            "server.probe" | "server.reportClientActivity" | "preview.reportProfiles" => {
                Ok(json!({}))
            }
            "server.connectionProbe" => Ok(json!({"serverTime": utc_iso(Utc::now())})),
            "server.discoverSourceControl" => Ok(json!({
                "versionControlSystems": [],
                "sourceControlProviders": [],
            })),
            "server.getSettings" => Ok(json!({})),
            "peerLinks.list" => Ok(json!({"links": []})),
            "vcs.listRefs" => Ok(json!({
                "isAtRepositoryRoot": false,
                "refs": [],
                "isRepo": false,
                "hasPrimaryRemote": false,
                "nextCursor": Value::Null,
                "totalCount": 0,
            })),
            "vcs.refreshStatus" => Ok(json!({
                "isRepo": false,
                "hasPrimaryRemote": false,
                "isDefaultRef": false,
                "refName": Value::Null,
                "hasWorkingTreeChanges": false,
                "workingTree": {"files": [], "insertions": 0, "deletions": 0},
                "hasUpstream": false,
                "aheadCount": 0,
                "behindCount": 0,
                "pr": Value::Null,
            })),
            "preview.list" => Ok(json!({
                "sessions": [],
                "serverEpoch": self.environment_id(),
                "revision": 0,
            })),
            "filesystem.getMetadata" => {
                let count = payload["paths"].as_array().map_or(0, Vec::len);
                Ok(json!({"entries": vec![Value::Null; count]}))
            }
            "filesystem.browse" => browse_filesystem(&payload, &self.0.opts),
            "filesystem.readFile" => read_filesystem_file(&payload),
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
        "unreadSequence": thread["unreadSequence"],
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
        let mut active_run = None;
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
            if run["status"].as_str().is_some_and(|s| !is_terminal_run_status(s)) {
                active_run = Some(run);
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
            if let Some(active) = active_run {
                row["activeRunId"] = active["id"].clone();
                row["status"] = json!(t3_run_status(active["status"].as_str().unwrap_or("running")));
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
    let mut runtime_requests_out = Vec::new();
    let inputs = snap["inputs"].as_array().map(Vec::as_slice).unwrap_or_default();

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
        let input_id = inputs.iter().find(|input| input["threadId"] == thread_id && input["kind"] == "queue" && input["runId"] == rid)
            .and_then(|input| input["id"].as_str()).unwrap_or(rid);
        let user_msg_id = format!("{input_id}/user");
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
                let out = run["output"].as_str().filter(|text| !text.is_empty())
                    .or_else(|| run["error"].as_str()).unwrap_or("");
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
            turn_items_out.push(item);
        }
        if let Some(text) = run["reasoning"].as_str().filter(|v| !v.is_empty()) {
            turn_items_out.push(json!({"id":format!("{rid}/reasoning"),"threadId":thread_id,"runId":rid,
                "type":"reasoning","text":text,"status":t3_status,"streaming": !is_terminal_run_status(status),
                "startedAt":created,"updatedAt":updated}));
        }
    }

    for input in inputs.iter().filter(|i| i["threadId"] == thread_id) {
        let queued = input["kind"] == "queue" && input["status"] == "queued";
        let steer = input["kind"] == "steer" && input["status"] != "cancelled";
        if !queued && !steer { continue; }
        let id = input["id"].as_str().unwrap_or_default();
        let message_id = if queued { format!("{id}/user") } else { format!("{id}/steer") };
        let run_id = if queued { json!(format!("input/{id}")) } else { input["runId"].clone() };
        if queued {
            runs_out.push(json!({"id":run_id,"status":"queued","queuePosition":input["position"].as_i64().unwrap_or(0)+1,
                "ordinal":runs_out.len()+1,"requestedAt":input["createdAt"],"userMessageId":message_id,"threadId":thread_id}));
        }
        messages_out.push(json!({"id":message_id,"runId":run_id,"role":"user","text":input["message"],
            "createdAt":input["createdAt"],"updatedAt":input["updatedAt"],"streaming":false}));
        turn_items_out.push(json!({"id":message_id,"threadId":thread_id,"runId":run_id,"type":"user_message",
            "text":input["message"],"inputIntent":if queued {"queued_turn"} else {"steer"},
            "status":input["status"],"startedAt":input["createdAt"],"updatedAt":input["updatedAt"]}));
    }
    for tool in snap["tools"].as_array().into_iter().flatten().filter(|t| t["threadId"] == thread_id) {
        turn_items_out.push(json!({"id":tool["id"],"threadId":thread_id,"runId":tool["runId"],
            "type":"dynamic_tool","toolName":tool["name"],"input":tool["arguments"],"output":tool["output"],
            "status":tool["status"],"updatedAt":tool["updatedAt"]}));
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
        let status = match req["status"].as_str().unwrap_or("") {
            "responded" => "resolved", "failed" => "expired", other => other,
        };
        let prompt = req["message"].as_str().filter(|s| !s.is_empty()).or(req["title"].as_str()).unwrap_or("");
        let mut item = json!({"id":req["id"],"requestId":req["id"],"threadId":thread_id,"runId":req["runId"],
            "type":"user_input_request","status":if status == "pending" {"waiting"} else {"completed"},
            "startedAt":req["createdAt"],"updatedAt":req["updatedAt"]});
        if kind == "permission" {
            item["type"] = json!("approval_request");
            item["prompt"] = json!(prompt);
            item["reason"] = req["title"].clone();
            item["options"] = json!([{"decision":"accept","label":"Allow once"},{"decision":"decline","label":"Decline"}]);
        } else {
            let options: Vec<_> = req["options"].as_array().into_iter().flatten()
                .filter_map(Value::as_str).map(|s| json!({"label":s,"description":""})).collect();
            item["questions"] = json!([{"id":req["id"],"header":req["title"],"question":prompt,
                "options":options,"multiSelect":false,"allowCustomAnswer":req["method"] != "select"}]);
        }
        turn_items_out.push(item);
        runtime_requests_out.push(json!({
            "id": req["id"],
            "threadId": thread_id,
            "runId": req["runId"],
            "kind": kind,
            "status": status,
            "title": req["title"],
            "prompt": prompt,
            "createdAt": req["createdAt"],
            "resolvedAt": if status == "pending" || status == "responding" { Value::Null } else { req["updatedAt"].clone() },
        }));
    }

    let run_order: HashMap<_, _> = runs_out.iter().enumerate()
        .filter_map(|(i,r)| r["id"].as_str().map(|id|(id,i))).collect();
    turn_items_out.sort_by(|a,b| {
        let key = |v: &Value| {
            let rank = match v["type"].as_str() {
                Some("user_message") if v["inputIntent"] != "steer" => 0,
                Some("reasoning") => 1, Some("assistant_message") => 3, _ => 2,
            };
            (run_order.get(v["runId"].as_str().unwrap_or("")).copied().unwrap_or(usize::MAX),rank)
        };
        key(a).cmp(&key(b)).then_with(|| a["startedAt"].as_str().or(a["updatedAt"].as_str())
            .cmp(&b["startedAt"].as_str().or(b["updatedAt"].as_str())))
    });
    let visible_out: Vec<_> = turn_items_out.iter_mut().enumerate().map(|(pos,item)| {
        if let Some(at) = item["updatedAt"].as_str() && at > updated_at.as_str() { updated_at = at.to_owned(); }
        item["ordinal"] = json!(pos);
        json!({"position":pos,"visibility":"local","sourceThreadId":thread_id,"sourceItemId":item["id"],"item":item})
    }).collect();
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

fn t3_policy_from_runtime_mode(mode: &str) -> Result<&'static str> {
    match mode.trim() {
        "" | "full-access" | "auto" => Ok("auto"),
        "approval-required" | "ask" => Ok("ask"),
        "auto-accept-edits" => Ok("auto-accept-edits"),
        _ => Err(anyhow!("Vantry 暂不支持此运行权限模式")),
    }
}

fn answer_value_text(val: &Value) -> String {
    match val {
        Value::String(s) => s.clone(),
        Value::Array(arr) => arr
            .iter()
            .map(answer_value_text)
            .filter(|s| !s.is_empty())
            .collect::<Vec<_>>()
            .join("\n"),
        Value::Object(map) => {
            if let Some(a) = map.get("answers") {
                return answer_value_text(a);
            }
            if let Some(v) = map.get("value") {
                return answer_value_text(v);
            }
            val.to_string()
        }
        Value::Null => String::new(),
        other => other.to_string(),
    }
}

fn ensure_scratch_workspace_path(opts: &T3Options, thread_id: &str, fallback_id: &str) -> Result<String> {
    let raw = if !thread_id.trim().is_empty() {
        thread_id.trim()
    } else if !fallback_id.trim().is_empty() {
        fallback_id.trim()
    } else {
        "scratch"
    };
    let safe = raw.replace(['/', '\\'], "_").replace("..", "_");
    let safe = if safe.is_empty() || safe == "." {
        "scratch".to_owned()
    } else {
        safe
    };
    let dir = opts.data_dir.join("workspaces").join(safe).join("chat");
    fs::create_dir_all(&dir)?;
    Ok(dir.display().to_string())
}

fn resolve_t3_workspace_path(snap: &Value, wire: &Value, opts: &T3Options) -> Result<String> {
    let strategy = wire.get("workspaceStrategy").unwrap_or(&Value::Null);
    let strategy_type = strategy["type"].as_str().unwrap_or("");
    let no_project = wire["noProject"].as_bool().unwrap_or(false);
    let proj_id = wire["projectId"].as_str().unwrap_or("");
    if matches!(strategy_type, "scratch" | "none")
        || no_project
        || proj_id == t3_project_id(SCRATCH_PROJECT_IDENTITY)
    {
        return ensure_scratch_workspace_path(
            opts,
            wire["threadId"].as_str().unwrap_or(""),
            wire["commandId"].as_str().unwrap_or(""),
        );
    }
    let explicit = wire["workspacePath"]
        .as_str()
        .filter(|s| !s.trim().is_empty())
        .or_else(|| wire["workspaceRoot"].as_str().filter(|s| !s.trim().is_empty()))
        .or_else(|| strategy["path"].as_str().filter(|s| !s.trim().is_empty()));
    if let Some(raw) = explicit {
        let trimmed = raw.trim();
        ensure!(
            !trimmed.contains('\0') && !trimmed.contains("environment:"),
            "不支持在该目录启动会话"
        );
        let expanded = expand_home_path(trimmed)?;
        ensure!(expanded.is_dir(), "所选目录不存在或不是有效文件夹");
        return Ok(expanded.display().to_string());
    }
    if !proj_id.is_empty() {
        for t in snap["threads"].as_array().into_iter().flatten() {
            let (identity, root) = thread_project_identity(t, opts);
            if t3_project_id(&identity) == proj_id && !root.is_empty() {
                return Ok(root);
            }
        }
    }
    ensure_scratch_workspace_path(
        opts,
        wire["threadId"].as_str().unwrap_or(""),
        wire["commandId"].as_str().unwrap_or(""),
    )
}

async fn dispatch_t3_command(core_socket: &Path, opts: &T3Options, wire: &Value) -> Result<Value> {
    let kind = wire["type"].as_str().unwrap_or("");
    let command_id = wire["commandId"].as_str().unwrap_or("").trim();
    ensure!(!command_id.is_empty(), "commandId 不能为空");
    let thread_id = wire["threadId"].as_str().unwrap_or("");
    let run_id = wire["runId"].as_str().unwrap_or("");
    if wire["attachments"].as_array().is_some_and(|a| !a.is_empty()) {
        return Err(anyhow!("此连接尚不支持附件发送"));
    }
    if wire["context"].as_array().is_some_and(|a| !a.is_empty()) {
        return Err(anyhow!("此连接尚不支持附加消息上下文"));
    }

    let mut cmd = json!({
        "version": 1,
        "commandId": command_id,
        "threadId": thread_id,
    });
    if !run_id.is_empty() {
        cmd["runId"] = json!(run_id);
    }
    if let Some(model) = wire.get("modelSelection").and_then(Value::as_object) {
        let p = model
            .get("instanceId")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .or_else(|| model.get("provider").and_then(Value::as_str))
            .unwrap_or("");
        let m = model.get("model").and_then(Value::as_str).unwrap_or("");
        if !p.is_empty() {
            cmd["providerId"] = json!(p);
        }
        if !m.is_empty() {
            cmd["model"] = json!(m);
        }
    }

    match kind {
        "thread.read-state.set" => {
            let change: vantry_protocol::ThreadReadChange = serde_json::from_value(wire["readState"].clone())?;
            cmd["kind"] = json!("thread.update");
            cmd["readState"] = json!(change);
        }
        "thread.create" => {
            let snap = core_http_json(core_socket, Method::GET, "/v1/snapshot", None).await?;
            let path = resolve_t3_workspace_path(&snap, wire, opts)?;
            let wt = wire["worktreePath"].as_str().unwrap_or("");
            let branch = wire["branch"].as_str().unwrap_or("");
            cmd["kind"] = json!("thread.create");
            cmd["title"] = json!(wire["title"].as_str().unwrap_or(""));
            cmd["interactionMode"] = json!(wire["interactionMode"].as_str().unwrap_or(""));
            cmd["approvalPolicy"] = json!(t3_policy_from_runtime_mode(
                wire["runtimeMode"].as_str().unwrap_or("")
            )?);
            cmd["workspace"] = if !wt.is_empty() {
                json!({"kind": "worktree", "path": wt, "repository": path, "branch": branch})
            } else {
                json!({"kind": "directory", "path": path, "branch": branch})
            };
        }
        "message.dispatch" => {
            cmd["message"] = json!(wire["text"].as_str().unwrap_or(""));
            if let Some(mid) = wire["messageId"].as_str().filter(|s| !s.is_empty()) {
                cmd["messageId"] = json!(mid);
            }
            let mode = wire["dispatchMode"]["type"].as_str().unwrap_or("");
            match mode {
                "steer_active" => {
                    cmd["kind"] = json!("run.steer");
                    if let Some(target) = wire["dispatchMode"]["targetRunId"].as_str() {
                        cmd["runId"] = json!(target);
                    }
                }
                "queue_after_active" => {
                    cmd["kind"] = json!("message.queue");
                }
                "" | "start_immediately" | "defer_start" => {
                    cmd["kind"] = json!("message.send");
                }
                _ => return Err(anyhow!("尚不支持此消息投递方式")),
            }
        }
        "queued-run.cancel" | "queued-run.edit" | "queued-message.promote-to-steer" => {
            let queued = if kind == "queued-message.promote-to-steer" {
                wire["queuedRunId"].as_str().unwrap_or("")
            } else {
                run_id
            };
            let input_id = queued
                .strip_prefix("input/")
                .ok_or_else(|| anyhow!("该运行不属于待处理输入队列"))?;
            cmd["inputId"] = json!(input_id);
            cmd.as_object_mut().unwrap().remove("runId");
            match kind {
                "queued-run.cancel" => cmd["kind"] = json!("input.cancel"),
                "queued-run.edit" => {
                    cmd["kind"] = json!("input.edit");
                    cmd["message"] = json!(wire["text"].as_str().unwrap_or(""));
                }
                _ => {
                    cmd["kind"] = json!("input.promote_steer");
                    if let Some(target) = wire["targetRunId"].as_str() {
                        cmd["runId"] = json!(target);
                    }
                }
            }
        }
        "thread.model-selection.set" => {
            cmd["kind"] = json!("thread.model");
        }
        "thread.runtime-mode.set" => {
            cmd["kind"] = json!("thread.update");
            cmd["approvalPolicy"] = json!(t3_policy_from_runtime_mode(
                wire["runtimeMode"].as_str().unwrap_or("")
            )?);
        }
        "thread.interaction-mode.set" => {
            cmd["kind"] = json!("thread.update");
            cmd["interactionMode"] = json!(wire["interactionMode"].as_str().unwrap_or(""));
        }
        "thread.metadata.update" => {
            cmd["kind"] = json!("thread.update");
            cmd["title"] = json!(wire["title"].as_str().unwrap_or(""));
        }
        "thread.archive" | "thread.unarchive" | "thread.settle" | "thread.auto-settle"
        | "thread.unsettle" => {
            cmd["kind"] = json!("thread.update");
            let archived = matches!(kind, "thread.archive" | "thread.settle" | "thread.auto-settle");
            cmd["archived"] = json!(archived);
        }
        "thread.pin" | "thread.unpin" => {
            cmd["kind"] = json!("thread.update");
            cmd["pinned"] = json!(kind == "thread.pin");
        }
        "thread.snooze" => {
            cmd["kind"] = json!("thread.update");
            let raw = wire["snoozedUntil"].as_str().unwrap_or("");
            let dt = DateTime::parse_from_rfc3339(raw)
                .map_err(|_| anyhow!("无效的延后时间格式"))?
                .with_timezone(&Utc);
            cmd["snoozedUntil"] = json!(utc_iso(dt));
        }
        "thread.unsnooze" => {
            cmd["kind"] = json!("thread.update");
            cmd["snoozedUntil"] = json!("0001-01-01T00:00:00Z");
        }
        "thread.visit"
        | "thread.mark-unread"
        | "thread.auto-settle.set"
        | "thread.pin.reorder"
        | "thread.active.reorder" => {
            let snap = core_http_json(core_socket, Method::GET, "/v1/snapshot", None).await?;
            return Ok(json!({"sequence": snap["sequence"].as_i64().unwrap_or(0)}));
        }
        "thread.delete" => {
            cmd["kind"] = json!("thread.delete");
        }
        "run.interrupt" | "thread.stop" => {
            cmd["kind"] = json!("run.stop");
            if run_id.is_empty() {
                let snap = core_http_json(core_socket, Method::GET, "/v1/snapshot", None).await?;
                let active_rid = snap["runs"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .find(|r| {
                        r["threadId"].as_str() == Some(thread_id)
                            && !is_terminal_run_status(r["status"].as_str().unwrap_or(""))
                    })
                    .and_then(|r| r["id"].as_str());
                if let Some(rid) = active_rid {
                    cmd["runId"] = json!(rid);
                } else {
                    return Ok(json!({"sequence": snap["sequence"].as_i64().unwrap_or(0)}));
                }
            }
        }
        "runtime-request.respond" => {
            cmd["kind"] = json!("request.respond");
            let req_id = wire["requestId"].as_str().unwrap_or("");
            cmd["requestId"] = json!(req_id);
            let snap = core_http_json(core_socket, Method::GET, "/v1/snapshot", None).await?;
            let found = snap["requests"]
                .as_array()
                .into_iter()
                .flatten()
                .find(|r| r["id"].as_str() == Some(req_id) && r["threadId"].as_str() == Some(thread_id))
                .ok_or_else(|| anyhow!("请求不属于此会话或已不存在"))?;
            cmd["runId"] = json!(found["runId"].as_str().unwrap_or(""));
            if found["method"].as_str() == Some("confirm") {
                match wire["decision"].as_str().unwrap_or("") {
                    "accept" => cmd["confirmed"] = json!(true),
                    "decline" => cmd["confirmed"] = json!(false),
                    "cancel" => cmd["cancelled"] = json!(true),
                    _ => return Err(anyhow!("只支持本次允许、拒绝或取消")),
                }
            } else if let Some(answers) = wire["answers"].as_object() {
                if let Some(v) = answers.get(req_id) {
                    cmd["value"] = json!(answer_value_text(v));
                } else if answers.len() == 1 {
                    let (_, v) = answers.iter().next().unwrap();
                    cmd["value"] = json!(answer_value_text(v));
                } else {
                    return Err(anyhow!("请回答此请求对应的问题"));
                }
            } else {
                return Err(anyhow!("请回答此请求对应的问题"));
            }
        }
        other => return Err(anyhow!("Vantry 尚不支持 T3 操作 {other:?}")),
    }

    let receipt = core_http_json(core_socket, Method::POST, "/v1/commands", Some(cmd)).await?;
    Ok(json!({"sequence": receipt["sequence"].as_i64().unwrap_or(0)}))
}

async fn launch_t3_thread(core_socket: &Path, opts: &T3Options, input: &Value) -> Result<Value> {
    let command_id = input["commandId"].as_str().unwrap_or("").trim();
    ensure!(!command_id.is_empty(), "commandId 不能为空");
    let thread_id = input["threadId"]
        .as_str()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_owned)
        .unwrap_or_else(|| format!("t3_{command_id}"));
    let mut resumed = false;
    if input["reuseExistingThread"].as_bool().unwrap_or(false)
        && let Ok(snap) = core_http_json(core_socket, Method::GET, "/v1/snapshot", None).await
    {
        resumed = snap["threads"]
            .as_array()
            .into_iter()
            .flatten()
            .any(|t| t["id"].as_str() == Some(thread_id.as_str()));
    }
    if !resumed {
        let mut create = input.clone();
        if let Some(obj) = create.as_object_mut() {
            obj.insert("type".into(), json!("thread.create"));
            obj.insert("threadId".into(), json!(thread_id));
            obj.insert("commandId".into(), json!(format!("{command_id}/create")));
            if let Some(strategy) = input.get("workspaceStrategy").and_then(Value::as_object) {
                if let Some(b) = strategy.get("branch").and_then(Value::as_str).filter(|s| !s.is_empty())
                    && !obj.contains_key("branch")
                {
                    obj.insert("branch".into(), json!(b));
                }
                if let Some(p) = strategy.get("path").and_then(Value::as_str).filter(|s| !s.is_empty())
                    && !obj.contains_key("workspacePath")
                {
                    obj.insert("workspacePath".into(), json!(p));
                }
                if strategy.get("type").and_then(Value::as_str) == Some("existing_worktree")
                    && let Some(wt) = strategy.get("worktreePath").and_then(Value::as_str).filter(|s| !s.is_empty())
                {
                    obj.insert("worktreePath".into(), json!(wt));
                }
            }
        }
        dispatch_t3_command(core_socket, opts, &create).await?;
    }
    if let Some(initial) = input.get("initialMessage").and_then(Value::as_object)
        && !initial.is_empty()
    {
        let msg_id = initial
            .get("messageId")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .map(str::to_owned)
            .unwrap_or_else(|| format!("{command_id}/message"));
        let message = json!({
            "type": "message.dispatch",
            "commandId": format!("{command_id}/message"),
            "threadId": thread_id,
            "messageId": msg_id,
            "text": initial.get("text").and_then(Value::as_str).unwrap_or(""),
            "attachments": initial.get("attachments").cloned().unwrap_or_else(|| json!([])),
            "dispatchMode": {"type": "start_immediately"},
        });
        dispatch_t3_command(core_socket, opts, &message).await?;
    }
    let snap = core_http_json(core_socket, Method::GET, "/v1/snapshot", None).await?;
    let seq = snap["sequence"].as_i64().unwrap_or(0);
    let projection = build_t3_thread_projection_opt(&snap, &thread_id, opts)
        .ok_or_else(|| anyhow!("会话不存在"))?;
    Ok(json!({
        "threadId": thread_id,
        "projection": projection,
        "resumed": resumed,
        "sequence": seq,
    }))
}

fn expand_home_path(raw: &str) -> Result<PathBuf> {
    let trimmed = raw.trim();
    ensure!(
        !trimmed.is_empty() && !trimmed.contains('\0') && !trimmed.contains("environment:"),
        "无效的路径"
    );
    let home = std::env::var_os("HOME").map(PathBuf::from);
    let expanded = if trimmed == "~" {
        home.ok_or_else(|| anyhow!("无法解析用户主目录"))?
    } else if let Some(rest) = trimmed.strip_prefix("~/").or_else(|| trimmed.strip_prefix("~\\")) {
        home.ok_or_else(|| anyhow!("无法解析用户主目录"))?.join(rest)
    } else {
        PathBuf::from(trimmed)
    };
    Ok(std::path::absolute(&expanded)?)
}

fn browse_filesystem(input: &Value, opts: &T3Options) -> Result<Value> {
    let raw = input["path"]
        .as_str()
        .filter(|s| !s.trim().is_empty())
        .or_else(|| input["directory"].as_str().filter(|s| !s.trim().is_empty()))
        .or_else(|| input["cwd"].as_str().filter(|s| !s.trim().is_empty()));
    let expanded = match raw {
        Some(p) => expand_home_path(p)?,
        None => std::env::var_os("HOME")
            .map(PathBuf::from)
            .unwrap_or_else(|| opts.data_dir.clone()),
    };
    let show_hidden = input["showHidden"].as_bool().unwrap_or(false);
    let mut items: Vec<(String, String, Value)> = Vec::new();
    for entry in fs::read_dir(&expanded).with_context(|| format!("无法读取目录 {}", expanded.display()))? {
        let Ok(entry) = entry else { continue };
        let name = entry.file_name().to_string_lossy().into_owned();
        if !show_hidden && name.starts_with('.') {
            continue;
        }
        let full = entry.path();
        if !full.is_dir() {
            continue;
        }
        let is_git = full.join(".git").exists();
        items.push((
            name.to_ascii_lowercase(),
            name.clone(),
            json!({
                "name": name,
                "path": full.display().to_string(),
                "isDirectory": true,
                "isGitRepo": is_git,
            }),
        ));
    }
    items.sort_by(|a, b| a.0.cmp(&b.0).then_with(|| a.1.cmp(&b.1)));
    items.truncate(500);
    let entries: Vec<Value> = items.into_iter().map(|(_, _, v)| v).collect();
    let parent = expanded
        .parent()
        .filter(|p| *p != expanded.as_path())
        .map(|p| Value::String(p.display().to_string()))
        .unwrap_or(Value::Null);
    Ok(json!({
        "path": expanded.display().to_string(),
        "parentPath": parent,
        "entries": entries,
    }))
}

fn read_filesystem_file(input: &Value) -> Result<Value> {
    let raw_path = input["path"].as_str().unwrap_or("").trim();
    ensure!(!raw_path.is_empty(), "文件路径不能为空");
    let full = if Path::new(raw_path).is_absolute() || raw_path.starts_with('~') {
        expand_home_path(raw_path)?
    } else if let Some(cwd) = input["cwd"].as_str().filter(|s| !s.trim().is_empty()) {
        std::path::absolute(expand_home_path(cwd)?.join(raw_path))?
    } else {
        expand_home_path(raw_path)?
    };
    let meta = fs::metadata(&full).with_context(|| format!("无法读取文件 {}", full.display()))?;
    let name = full
        .file_name()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_else(|| full.display().to_string());
    if meta.is_dir() {
        return Ok(json!({
            "path": full.display().to_string(),
            "name": name,
            "kind": "directory",
            "mimeType": "inode/directory",
            "encoding": "utf-8",
            "content": "",
            "size": 0,
            "truncated": false,
            "isDirectory": true,
        }));
    }
    let max_bytes = input["maxBytes"]
        .as_u64()
        .map(|n| (n as usize).clamp(1, 1024 * 1024))
        .unwrap_or(524_288);
    let ext = full
        .extension()
        .and_then(|e| e.to_str())
        .unwrap_or("")
        .to_ascii_lowercase();
    let (kind, mime, is_binary) = match ext.as_str() {
        "png" => ("image", "image/png", true),
        "jpg" | "jpeg" => ("image", "image/jpeg", true),
        "gif" => ("image", "image/gif", true),
        "webp" => ("image", "image/webp", true),
        "svg" => ("image", "image/svg+xml", false),
        "pdf" => ("pdf", "application/pdf", true),
        "md" | "markdown" => ("markdown", "text/markdown", false),
        _ => ("text", "text/plain", false),
    };
    let bytes = fs::read(&full)?;
    let size = bytes.len() as i64;
    if is_binary {
        let cap = if kind == "pdf" { 12 * 1024 * 1024 } else { 8 * 1024 * 1024 };
        let truncated = bytes.len() > cap;
        let slice = &bytes[..bytes.len().min(cap)];
        return Ok(json!({
            "path": full.display().to_string(),
            "name": name,
            "kind": kind,
            "mimeType": mime,
            "encoding": "base64",
            "content": STANDARD.encode(slice),
            "size": size,
            "truncated": truncated,
            "isDirectory": false,
        }));
    }
    let truncated = bytes.len() > max_bytes;
    let slice = &bytes[..bytes.len().min(max_bytes)];
    let text = String::from_utf8_lossy(slice).into_owned();
    let line_count = text.lines().count();
    Ok(json!({
        "path": full.display().to_string(),
        "name": name,
        "kind": kind,
        "mimeType": mime,
        "language": if ext.is_empty() { Value::Null } else { Value::String(ext) },
        "encoding": "utf-8",
        "content": text,
        "size": size,
        "truncated": truncated,
        "isDirectory": false,
        "lineCount": line_count,
    }))
}

async fn stream_t3_effect_events(
    core: &Path,
    opts: &T3Options,
    after: i64,
    req_id: Value,
    is_shell: bool,
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
    let mut dirty = false;
    let mut wire_seq = after;
    let mut covered_core = after;
    let mut ticker = tokio::time::interval(std::time::Duration::from_millis(80));
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);

    loop {
        tokio::select! {
            _ = stop.cancelled() => break,
            _ = ticker.tick(), if dirty => {
                dirty = false;
                {
                    let snap = mobile_snapshot(core, (!is_shell).then_some(filter_thread.as_str())).await?;
                    let core_seq = snap["sequence"].as_i64().unwrap_or(covered_core);
                    let next_seq = core_seq.max(wire_seq + 1);
                    let frame = if is_shell {
                        let mut shell_snap = build_t3_shell_snapshot(&snap, opts);
                        shell_snap["snapshotSequence"] = json!(next_seq);
                        Some(json!({
                            "kind": "snapshot",
                            "snapshot": shell_snap,
                        }))
                    } else {
                        build_t3_thread_projection_opt(&snap, &filter_thread, opts).map(|proj| json!({
                            "kind": "snapshot",
                            "snapshotSequence": next_seq,
                            "projection": proj,
                            "historyCursor": Value::Null,
                            "hasMoreHistory": false,
                            "latestLocalTurnOrdinal": Value::Null,
                        }))
                    };
                    if let Some(f) = frame {
                        let msg = json!({
                            "_tag": "Chunk",
                            "requestId": req_id,
                            "values": [f],
                        });
                        if tx.send(Message::text(msg.to_string())).await.is_err() {
                            driver.abort();
                            return Ok(());
                        }
                        wire_seq = next_seq;
                        covered_core = core_seq;
                    }
                }
            }
            f = body.frame() => {
                let frame = match f {
                    Some(Ok(frame)) => frame,
                    _ => break,
                };
                if let Some(data) = frame.data_ref() {
                    buf.extend_from_slice(data);
                    while let Some(pos) = buf.iter().position(|&b| b == b'\n') {
                        let line = buf.drain(..=pos).collect::<Vec<_>>();
                        if let Ok(env) = serde_json::from_slice::<Value>(&line) {
                            ensure!(env.get("error").is_none(), "Core event cursor expired");
                            let ev = env.get("event").unwrap_or(&env);
                            let seq = ev["sequence"].as_i64().unwrap_or(0);
                            if seq > covered_core
                                && (is_shell
                                    || filter_thread.is_empty()
                                    || ev["threadId"].as_str() == Some(filter_thread.as_str()))
                            {
                                dirty = true;
                            }
                        }
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

async fn mobile_snapshot(core: &Path, thread: Option<&str>) -> Result<Value> {
    let Some(id) = thread else {
        return core_http_json(core, Method::GET, "/v1/snapshot", None).await;
    };
    let query = url::form_urlencoded::Serializer::new(String::new()).append_pair("threadId", id).finish();
    let mut detail = core_http_json(core, Method::GET, &format!("/v1/threads/detail?{query}"), None).await?;
    let thread = detail["thread"].take();
    detail["threads"] = json!([thread]);
    Ok(detail)
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
            identity: None,
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
        assert_eq!(
            cfg_json["exit"]["value"]["providers"][0]["instanceId"],
            "controlled"
        );
        assert!(
            cfg_json["exit"]["value"]["providers"][0]["supportedRuntimeModes"]
                .as_array()
                .is_some_and(|m| m.iter().any(|v| v == "full-access"))
        );
        assert_eq!(
            cfg_json["exit"]["value"]["providers"][0]["models"][0]["slug"],
            "local/fixture"
        );
        assert_eq!(
            cfg_json["exit"]["value"]["providers"][0]["models"][0]["isDefault"],
            true
        );

        server.revoke_all()?;
        assert!(server.clients().is_empty());
        stop.cancel();
        handle.await??;
        Ok(())
    }
}
