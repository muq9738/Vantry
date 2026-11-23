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
    os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt},
    path::PathBuf,
};
use tokio::{
    net::{UnixListener, UnixStream},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;
use vantry_protocol::*;
type Body = UnsyncBoxBody<Bytes, Infallible>;
pub struct Server {
    listener: UnixListener,
    path: PathBuf,
    identity: (u64, u64),
    core: Core,
}
impl Server {
    pub async fn bind(core: Core) -> Result<Self> {
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
            listener,
            path,
            identity: (info.dev(), info.ino()),
            core,
        };
        fs::set_permissions(&server.path, fs::Permissions::from_mode(0o600))?;
        Ok(server)
    }
    pub async fn serve(self, stop: CancellationToken) -> Result<()> {
        let mut conns = JoinSet::new();
        let closing = CancellationToken::new();
        loop {
            tokio::select! {
                _=stop.cancelled()=>break,
                _=self.core.0.shutdown.cancelled()=>break,
                Some(_)=conns.join_next(),if !conns.is_empty()=>{},
                conn=self.listener.accept()=>{
                    let (conn,_)=conn?;
                    let core=self.core.clone();
                    let done=closing.clone();
                    conns.spawn(async move {
                        let service=service_fn(move|req|{
                            let core=core.clone();
                            async move { Ok::<_,Infallible>(route(core,req).await.unwrap_or_else(error_response)) }
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
        closing.cancel();
        while conns.join_next().await.is_some() {}
        Ok(())
    }
}
impl Drop for Server {
    fn drop(&mut self) {
        if let Ok(m) = fs::symlink_metadata(&self.path) {
            if (m.dev(), m.ino()) == self.identity {
                let _ = fs::remove_file(&self.path);
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
async fn route(core: Core, req: Request<Incoming>) -> Result<Response<Body>> {
    let path = req.uri().path();
    let method = match path {
        "/v1/commands" | "/v1/admin/shutdown-if-idle" => "POST",
        "/v1/health" | "/v1/status" | "/v1/capabilities" | "/v1/state" | "/v1/snapshot"
        | "/v1/events" => "GET",
        _ => return Err(Error::Api(ErrorCode::NotFound, "unknown endpoint".into())),
    };
    if req.method().as_str() != method {
        let mut res = response(
            StatusCode::METHOD_NOT_ALLOWED,
            api_error(invalid("method not allowed")),
        );
        res.headers_mut().insert("allow", method.parse().unwrap());
        return Ok(res);
    }
    if path != "/v1/events" && req.uri().query().is_some() {
        return Err(invalid("unexpected query parameter"));
    }
    let ok = StatusCode::OK;
    Ok(match path {
        "/v1/health" => response(
            ok,
            Health {
                version: 1,
                state: core.status()?.state,
            },
        ),
        "/v1/capabilities" | "/v1/status" => response(ok, core.status()?),
        "/v1/snapshot" | "/v1/state" => response(ok, core.snapshot()?),
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
            let after = req
                .uri()
                .query()
                .and_then(|q| q.strip_prefix("after="))
                .and_then(|s| s.parse::<i64>().ok())
                .ok_or_else(|| invalid("events requires one integer after cursor"))?;
            events(core, after)?
        }
    })
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
