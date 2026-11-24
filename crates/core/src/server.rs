//! Private HTTP/1.1 transport. The event log is authoritative and watch notifications
//! only wake readers, so commits racing replay are observed in sequence order.
use crate::*;
use bytes::Bytes;
use futures::stream;
use http::{Request, Response, StatusCode};
use http_body_util::{BodyExt, Full, Limited, StreamBody, combinators::UnsyncBoxBody};
use hyper::{
    body::{Frame, Incoming},
    server::conn::http1,
    service::service_fn,
};
use hyper_util::rt::TokioIo;
use serde_json::json;
use std::{
    collections::VecDeque,
    convert::Infallible,
    fs, io,
    path::PathBuf,
    sync::Arc,
};
#[cfg(unix)]
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};
use tokio::{net::TcpListener, task::JoinSet};
use tokio_util::sync::CancellationToken;
use vantry_protocol::*;

/// Optional service-layer handler mounted onto the Core Unix socket for
/// workbench actions, orchestration queries, and extra capabilities.
#[async_trait::async_trait]
pub trait ServiceExtension: Send + Sync + 'static {
    async fn workbench(&self, request: WorkbenchRequest) -> Result<WorkbenchResponse>;

    async fn extra_route(
        &self,
        _method: &str,
        _path: &str,
        _query: Option<&str>,
        _body: Bytes,
    ) -> Option<Result<serde_json::Value>> {
        None
    }

    fn capabilities(&self) -> Vec<String> {
        Vec::new()
    }
}

type Body = UnsyncBoxBody<Bytes, Infallible>;
enum Listener {
    #[cfg(unix)]
    Unix {
        listener: UnixListener,
        identity: (u64, u64),
    },
    Tcp {
        listener: TcpListener,
        endpoint: Arc<LocalTcpEndpoint>,
    },
}
pub struct Server {
    listener: Listener,
    path: PathBuf,
    core: Core,
    extension: Option<Arc<dyn ServiceExtension>>,
}
impl Server {
    pub async fn bind(core: Core) -> Result<Self> {
        Self::bind_inner(core, None).await
    }

    pub async fn bind_with_extension(
        core: Core,
        extension: Arc<dyn ServiceExtension>,
    ) -> Result<Self> {
        Self::bind_inner(core, Some(extension)).await
    }

    /// Bind on a private loopback TCP port and publish a token-authenticated
    /// `LocalTcpEndpoint` descriptor at `<data_dir>/core.sock`. Used on Windows
    /// and for cross-platform loopback transport verification.
    pub async fn bind_tcp(core: Core) -> Result<Self> {
        Self::bind_tcp_inner(core, None).await
    }

    pub async fn bind_tcp_with_extension(
        core: Core,
        extension: Arc<dyn ServiceExtension>,
    ) -> Result<Self> {
        Self::bind_tcp_inner(core, Some(extension)).await
    }

    #[cfg(unix)]
    async fn bind_inner(
        core: Core,
        extension: Option<Arc<dyn ServiceExtension>>,
    ) -> Result<Self> {
        let path = core.directory().join("core.sock");
        if fs::metadata(core.directory())?.mode() & 0o077 != 0 {
            return Err(invalid("Core socket requires a private data directory"));
        }
        if let Ok(info) = fs::symlink_metadata(&path) {
            if !info.file_type().is_socket() {
                return Err(conflict("core.sock path is occupied"));
            }
            match UnixStream::connect(&path).await {
                Ok(_) => return Err(conflict("socket is in use")),
                Err(e) if e.kind() == io::ErrorKind::ConnectionRefused => {
                    let current = fs::symlink_metadata(&path)?;
                    if (current.dev(), current.ino()) != (info.dev(), info.ino()) {
                        return Err(conflict("socket changed during bind"));
                    }
                    fs::remove_file(&path)?;
                }
                Err(e) => return Err(e.into()),
            }
        }
        let listener = UnixListener::bind(&path)?;
        let info = fs::symlink_metadata(&path)?;
        let server = Self {
            listener: Listener::Unix {
                listener,
                identity: (info.dev(), info.ino()),
            },
            path,
            core,
            extension,
        };
        fs::set_permissions(&server.path, fs::Permissions::from_mode(0o600))?;
        Ok(server)
    }

    #[cfg(not(unix))]
    async fn bind_inner(
        core: Core,
        extension: Option<Arc<dyn ServiceExtension>>,
    ) -> Result<Self> {
        Self::bind_tcp_inner(core, extension).await
    }

    async fn bind_tcp_inner(
        core: Core,
        extension: Option<Arc<dyn ServiceExtension>>,
    ) -> Result<Self> {
        let path = core.directory().join("core.sock");
        if let Ok(existing) = fs::read(&path) {
            if let Some(ep) = LocalTcpEndpoint::parse_bytes(&existing) {
                if tokio::net::TcpStream::connect(&ep.addr).await.is_ok() {
                    return Err(conflict("socket is in use"));
                }
                let _ = fs::remove_file(&path);
            } else {
                return Err(conflict("core.sock path is occupied"));
            }
        }
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let addr = listener.local_addr()?.to_string();
        let token = format!("{}{}", uuid::Uuid::new_v4().simple(), uuid::Uuid::new_v4().simple());
        let endpoint = Arc::new(LocalTcpEndpoint::new(addr, token));
        let payload = serde_json::to_vec(endpoint.as_ref())?;
        let tmp = core
            .directory()
            .join(format!(".core-sock-{}", uuid::Uuid::new_v4().simple()));
        {
            let mut opts = fs::OpenOptions::new();
            opts.write(true).create_new(true);
            #[cfg(unix)]
            opts.mode(0o600);
            let mut f = opts.open(&tmp)?;
            io::Write::write_all(&mut f, &payload)?;
            f.sync_all()?;
        }
        if let Err(e) = fs::rename(&tmp, &path) {
            let _ = fs::remove_file(&tmp);
            return Err(e.into());
        }
        Ok(Self {
            listener: Listener::Tcp { listener, endpoint },
            path,
            core,
            extension,
        })
    }

    pub async fn serve(self, stop: CancellationToken) -> Result<()> {
        let mut conns = JoinSet::new();
        let closing = CancellationToken::new();
        loop {
            match &self.listener {
                #[cfg(unix)]
                Listener::Unix { listener, .. } => {
                    tokio::select! {
                        _=stop.cancelled()=>break,
                        _=self.core.0.shutdown.cancelled()=>break,
                        Some(_)=conns.join_next(),if !conns.is_empty()=>{},
                        conn=listener.accept()=>{
                            let (conn,_)=conn?;
                            let core=self.core.clone();
                            let extension=self.extension.clone();
                            let done=closing.clone();
                            conns.spawn(async move {
                                let service=service_fn(move|req|{
                                    let core=core.clone();
                                    let extension=extension.clone();
                                    async move { Ok::<_,Infallible>(route(core,extension,req).await.unwrap_or_else(error_response)) }
                                });
                                let connection=http1::Builder::new().serve_connection(TokioIo::new(conn),service);
                                tokio::pin!(connection);
                                tokio::select! {
                                    _=&mut connection=>{},
                                    _=done.cancelled()=>{
                                        connection.as_mut().graceful_shutdown();
                                        let _=tokio::time::timeout(std::time::Duration::from_secs(1),connection).await;
                                    }
                                }
                            });
                        }
                    }
                }
                Listener::Tcp { listener, endpoint } => {
                    tokio::select! {
                        _=stop.cancelled()=>break,
                        _=self.core.0.shutdown.cancelled()=>break,
                        Some(_)=conns.join_next(),if !conns.is_empty()=>{},
                        conn=listener.accept()=>{
                            let (conn,_)=conn?;
                            let core=self.core.clone();
                            let extension=self.extension.clone();
                            let endpoint=endpoint.clone();
                            let done=closing.clone();
                            conns.spawn(async move {
                                let service=service_fn(move|req|{
                                    let core=core.clone();
                                    let extension=extension.clone();
                                    let endpoint=endpoint.clone();
                                    async move {
                                        let authorized = req
                                            .headers()
                                            .get(LOCAL_TOKEN_HEADER)
                                            .and_then(|v| v.to_str().ok())
                                            .is_some_and(|t| endpoint.verify_token(t));
                                        if !authorized {
                                            return Ok::<_,Infallible>(response(
                                                StatusCode::UNAUTHORIZED,
                                                api_error(Error::Api(ErrorCode::NotReady, "missing or invalid local token".into())),
                                            ));
                                        }
                                        Ok::<_,Infallible>(route(core,extension,req).await.unwrap_or_else(error_response))
                                    }
                                });
                                let connection=http1::Builder::new().serve_connection(TokioIo::new(conn),service);
                                tokio::pin!(connection);
                                tokio::select! {
                                    _=&mut connection=>{},
                                    _=done.cancelled()=>{
                                        connection.as_mut().graceful_shutdown();
                                        let _=tokio::time::timeout(std::time::Duration::from_secs(1),connection).await;
                                    }
                                }
                            });
                        }
                    }
                }
            }
        }
        closing.cancel();
        while conns.join_next().await.is_some() {}
        Ok(())
    }
}
impl Drop for Server {
    fn drop(&mut self) {
        match &self.listener {
            #[cfg(unix)]
            Listener::Unix { identity, .. } => {
                if let Ok(m) = fs::symlink_metadata(&self.path) {
                    if (m.dev(), m.ino()) == *identity {
                        let _ = fs::remove_file(&self.path);
                    }
                }
            }
            Listener::Tcp { endpoint, .. } => {
                if let Ok(bytes) = fs::read(&self.path)
                    && LocalTcpEndpoint::parse_bytes(&bytes).as_ref() == Some(endpoint.as_ref())
                {
                    let _ = fs::remove_file(&self.path);
                }
            }
        }
    }
}
fn response(status: StatusCode, v: impl serde::Serialize) -> Response<Body> {
    Response::builder()
        .status(status)
        .header("content-type", "application/json")
        .body(Full::new(Bytes::from(serde_json::to_vec(&v).expect("protocol JSON"))).boxed_unsync())
        .unwrap()
}
fn api_error(e: Error) -> ApiError {
    ApiError {
        code: e.code(),
        message: e.to_string(),
    }
}
fn error_response(e: Error) -> Response<Body> {
    let status = match e.code() {
        ErrorCode::InvalidCommand | ErrorCode::VersionMismatch => StatusCode::BAD_REQUEST,
        ErrorCode::Conflict | ErrorCode::ResyncRequired => StatusCode::CONFLICT,
        ErrorCode::NotFound => StatusCode::NOT_FOUND,
        ErrorCode::NotReady => StatusCode::SERVICE_UNAVAILABLE,
        _ => StatusCode::INTERNAL_SERVER_ERROR,
    };
    response(status, api_error(e))
}
async fn route(
    core: Core,
    extension: Option<Arc<dyn ServiceExtension>>,
    req: Request<Incoming>,
) -> Result<Response<Body>> {
    let path = req.uri().path().to_owned();
    let query = req.uri().query().map(str::to_owned);
    let method_str = req.method().as_str().to_owned();
    let expected_method = match path.as_str() {
        "/v1/commands" | "/v1/admin/shutdown-if-idle" | "/v1/workbench" => Some("POST"),
        "/v1/health"
        | "/v1/status"
        | "/v1/capabilities"
        | "/v1/state"
        | "/v1/snapshot"
        | "/v1/threads/summaries"
        | "/v1/threads/detail"
        | "/v1/events" => Some("GET"),
        _ => None,
    };
    if let Some(method) = expected_method {
        if method_str != method {
            let mut res = response(
                StatusCode::METHOD_NOT_ALLOWED,
                api_error(invalid("method not allowed")),
            );
            res.headers_mut().insert("allow", method.parse().unwrap());
            return Ok(res);
        }
        if !matches!(path.as_str(), "/v1/events" | "/v1/threads/detail") && query.is_some() {
            return Err(invalid("unexpected query parameter"));
        }
    } else if let Some(ext) = extension {
        let body = Limited::new(req.into_body(), 16 << 20)
            .collect()
            .await
            .map_err(|_| invalid("invalid or oversized request body"))?
            .to_bytes();
        if let Some(result) = ext
            .extra_route(&method_str, &path, query.as_deref(), body)
            .await
        {
            return Ok(response(StatusCode::OK, result?));
        }
        return Err(Error::Api(ErrorCode::NotFound, "unknown endpoint".into()));
    } else {
        return Err(Error::Api(ErrorCode::NotFound, "unknown endpoint".into()));
    }

    let ok = StatusCode::OK;
    Ok(match path.as_str() {
        "/v1/health" => response(
            ok,
            Health {
                version: 1,
                state: core.status()?.state,
            },
        ),
        "/v1/capabilities" | "/v1/status" => {
            let mut status = core.status()?;
            if let Some(ext) = &extension {
                let extra = ext.capabilities();
                if !extra.is_empty() {
                    let caps = status.capabilities.get_or_insert_with(Vec::new);
                    for cap in extra {
                        if !caps.contains(&cap) {
                            caps.push(cap);
                        }
                    }
                }
            }
            response(ok, status)
        }
        "/v1/snapshot" | "/v1/state" => response(ok, core.snapshot()?),
        "/v1/threads/summaries" => response(ok, core.thread_summaries()?),
        "/v1/threads/detail" => {
            let (thread_id, limit_runs) = parse_thread_detail_query(query.as_deref())?;
            response(ok, core.thread_detail(&thread_id, limit_runs)?)
        }
        "/v1/workbench" => {
            let Some(ext) = extension else {
                return Err(Error::Api(
                    ErrorCode::NotReady,
                    "workbench service is not mounted on this Core".into(),
                ));
            };
            let body = Limited::new(req.into_body(), 16 << 20)
                .collect()
                .await
                .map_err(|_| invalid("invalid or oversized workbench body"))?
                .to_bytes();
            let request: WorkbenchRequest =
                serde_json::from_slice(&body).map_err(|e| invalid(&e.to_string()))?;
            response(ok, ext.workbench(request).await?)
        }
        "/v1/commands" => {
            let body = Limited::new(req.into_body(), 1 << 20)
                .collect()
                .await
                .map_err(|_| invalid("invalid or oversized request body"))?
                .to_bytes();
            #[derive(serde::Deserialize)]
            struct Envelope {
                #[serde(flatten)]
                command: Command,
                #[serde(flatten)]
                unknown: std::collections::BTreeMap<String, serde::de::IgnoredAny>,
            }
            let envelope: Envelope =
                serde_json::from_slice(&body).map_err(|e| invalid(&e.to_string()))?;
            if let Some(key) = envelope.unknown.keys().next() {
                return Err(invalid(&format!("unknown command field {key}")));
            }
            response(ok, core.dispatch(&envelope.command)?)
        }
        "/v1/admin/shutdown-if-idle" => {
            core.drain_and_shutdown()?;
            response(ok, json!({"state":"draining"}))
        }
        _ => {
            let after = query
                .as_deref()
                .and_then(|q| q.strip_prefix("after="))
                .and_then(|s| s.parse::<i64>().ok())
                .ok_or_else(|| invalid("events requires one integer after cursor"))?;
            events(core, after)?
        }
    })
}

fn parse_thread_detail_query(query: Option<&str>) -> Result<(String, i64)> {
    let mut thread_id = String::new();
    let mut limit_runs = 0i64;
    if let Some(q) = query {
        for pair in q.split('&').filter(|s| !s.is_empty()) {
            let Some((k, v)) = pair.split_once('=') else {
                return Err(invalid("invalid thread detail query"));
            };
            match k {
                "threadId" | "thread" => thread_id = v.to_owned(),
                "limitRuns" | "limit" => {
                    limit_runs = v
                        .parse::<i64>()
                        .map_err(|_| invalid("invalid limitRuns query parameter"))?;
                    if limit_runs < 0 {
                        return Err(invalid("limitRuns cannot be negative"));
                    }
                }
                _ => return Err(invalid("unexpected query parameter")),
            }
        }
    }
    if thread_id.is_empty() {
        return Err(invalid("threadId is required"));
    }
    Ok((thread_id, limit_runs))
}

fn events(core: Core, after: i64) -> Result<Response<Body>> {
    let changed = core.0.changed.subscribe();
    let page = VecDeque::from(core.events_after(after, 128)?);
    let frames = stream::unfold(Some((core, changed, after, page)), |state| async move {
        let (core, mut changed, mut cursor, mut page) = state?;
        loop {
            if let Some(e) = page.pop_front() {
                cursor = e.sequence;
                let line = ndjson(json!({"event":e}));
                return Some((line, Some((core, changed, cursor, page))));
            }
            match core.events_after(cursor, 128) {
                Ok(batch) if batch.is_empty() => {
                    tokio::select! {_=core.0.shutdown.cancelled()=>return None,result=changed.changed()=>{
                    result.ok()?;}}
                }
                Ok(batch) => page = batch.into(),
                Err(e) => return Some((ndjson(json!({"error":api_error(e)})), None)),
            }
        }
    });
    Ok(Response::builder()
        .header("content-type", "application/x-ndjson")
        .header("cache-control", "no-cache")
        .body(StreamBody::new(frames).boxed_unsync())
        .unwrap())
}
fn ndjson(v: serde_json::Value) -> std::result::Result<Frame<Bytes>, Infallible> {
    let mut bytes = v.to_string().into_bytes();
    bytes.push(b'\n');
    Ok(Frame::data(bytes.into()))
}
