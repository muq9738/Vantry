use bytes::{Buf, Bytes};
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Method, Request, body::Incoming, client::conn::http1};
use hyper_util::rt::TokioIo;
use serde::Deserialize;
use serde::de::DeserializeOwned;
use std::path::{Path, PathBuf};
use tokio::{net::UnixStream, task::JoinHandle};
use vantry_protocol::*;

#[derive(Debug, thiserror::Error)]
pub enum ClientError {
    #[error("Core requires a new snapshot: {0}")]
    ResyncRequired(ResyncRequired),
    #[error("Core returned {code:?}: {message}")]
    Api { code: ErrorCode, message: String },
    #[error(transparent)]
    Transport(#[from] anyhow::Error),
}
impl From<ApiError> for ClientError {
    fn from(e: ApiError) -> Self {
        if e.code == ErrorCode::ResyncRequired {
            Self::ResyncRequired(ResyncRequired { message: e.message })
        } else {
            Self::Api {
                code: e.code,
                message: e.message,
            }
        }
    }
}
type Result<T> = std::result::Result<T, ClientError>;

struct Driver(JoinHandle<std::result::Result<(), hyper::Error>>);
impl Drop for Driver {
    fn drop(&mut self) {
        self.0.abort();
    }
}

#[derive(Debug, Clone)]
pub struct CoreClient {
    socket: PathBuf,
}
impl CoreClient {
    pub fn new(socket: impl Into<PathBuf>) -> Self {
        Self {
            socket: socket.into(),
        }
    }
    pub fn from_data_dir(dir: impl AsRef<Path>) -> Self {
        Self::new(dir.as_ref().join("core.sock"))
    }

    async fn request(
        &self,
        method: Method,
        path: &str,
        body: Vec<u8>,
    ) -> Result<(Incoming, Driver)> {
        let request = Request::builder()
            .method(method)
            .uri(path)
            .header("host", "vantry.local")
            .header("content-type", "application/json")
            .body(Full::new(Bytes::from(body)))
            .map_err(anyhow::Error::from)?;
        let io = UnixStream::connect(&self.socket)
            .await
            .map_err(anyhow::Error::from)?;
        let (mut sender, conn) = http1::handshake(TokioIo::new(io))
            .await
            .map_err(anyhow::Error::from)?;
        let driver = Driver(tokio::spawn(conn));
        let response = sender
            .send_request(request)
            .await
            .map_err(anyhow::Error::from)?;
        if !response.status().is_success() {
            let bytes = Limited::new(response.into_body(), 1 << 20)
                .collect()
                .await
                .map_err(anyhow::Error::from_boxed)?
                .to_bytes();
            let error: ApiError = serde_json::from_slice(&bytes).map_err(anyhow::Error::from)?;
            return Err(error.into());
        }
        Ok((response.into_body(), driver))
    }
    async fn json<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        payload: Vec<u8>,
    ) -> Result<T> {
        let (body, _driver) = self.request(method, path, payload).await?;
        let bytes = body
            .collect()
            .await
            .map_err(anyhow::Error::from)?
            .to_bytes();
        Ok(serde_json::from_slice(&bytes).map_err(anyhow::Error::from)?)
    }
    pub async fn health(&self) -> Result<Health> {
        self.json(Method::GET, "/v1/health", vec![]).await
    }
    async fn get_projection<T: DeserializeOwned>(&self, baseline: &str, alias: &str) -> Result<T> {
        match self.json(Method::GET, baseline, vec![]).await {
            Err(ClientError::Api {
                code: ErrorCode::NotFound,
                ..
            }) => self.json(Method::GET, alias, vec![]).await,
            result => result,
        }
    }
    pub async fn capabilities(&self) -> Result<Capabilities> {
        self.get_projection("/v1/status", "/v1/capabilities").await
    }
    pub async fn snapshot(&self) -> Result<Snapshot> {
        self.get_projection("/v1/snapshot", "/v1/state").await
    }
    pub async fn dispatch(&self, command: &Command) -> Result<Receipt> {
        self.json(
            Method::POST,
            "/v1/commands",
            serde_json::to_vec(command).map_err(anyhow::Error::from)?,
        )
        .await
    }
    pub async fn workbench(&self, request: &WorkbenchRequest) -> Result<WorkbenchResponse> {
        self.json(
            Method::POST,
            "/v1/workbench",
            serde_json::to_vec(request).map_err(anyhow::Error::from)?,
        )
        .await
    }
    pub async fn thread_summaries(&self) -> Result<ThreadSummaries> {
        self.json(Method::GET, "/v1/threads/summaries", vec![])
            .await
    }
    pub async fn thread_detail(&self, thread_id: &str, limit_runs: i64) -> Result<ThreadDetail> {
        let path = if limit_runs > 0 {
            format!("/v1/threads/detail?threadId={thread_id}&limitRuns={limit_runs}")
        } else {
            format!("/v1/threads/detail?threadId={thread_id}")
        };
        self.json(Method::GET, &path, vec![]).await
    }
    pub async fn shutdown_if_idle(&self) -> Result<serde_json::Value> {
        self.json(Method::POST, "/v1/admin/shutdown-if-idle", vec![])
            .await
    }
    pub async fn get_json<T: DeserializeOwned>(&self, path: &str) -> Result<T> {
        self.json(Method::GET, path, vec![]).await
    }
    pub async fn post_json<Req: serde::Serialize, Res: DeserializeOwned>(
        &self,
        path: &str,
        request: &Req,
    ) -> Result<Res> {
        self.json(
            Method::POST,
            path,
            serde_json::to_vec(request).map_err(anyhow::Error::from)?,
        )
        .await
    }
    pub async fn subscribe(&self, since: i64) -> Result<EventStream> {
        let (body, driver) = self
            .request(Method::GET, &format!("/v1/events?after={since}"), vec![])
            .await?;
        Ok(EventStream {
            body,
            driver: Some(driver),
            chunk: Bytes::new(),
            line: vec![],
            cursor: since,
        })
    }
}

/// Drop the stream to disconnect. The cursor advances only after a complete event is decoded.
pub struct EventStream {
    body: Incoming,
    driver: Option<Driver>,
    chunk: Bytes,
    line: Vec<u8>,
    cursor: i64,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Envelope {
    event: Option<CoreEvent>,
    error: Option<ApiError>,
}
impl EventStream {
    pub fn cursor(&self) -> i64 {
        self.cursor
    }
    pub async fn next_event(&mut self) -> Result<Option<CoreEvent>> {
        if self.driver.is_none() {
            return Ok(None);
        }
        let result = self.read_event().await;
        if let Ok(Some(event)) = &result {
            self.cursor = event.sequence;
        } else {
            self.driver.take();
        }
        result
    }
    async fn read_event(&mut self) -> Result<Option<CoreEvent>> {
        loop {
            if self.chunk.is_empty() {
                match self.body.frame().await {
                    Some(frame) => {
                        self.chunk = frame
                            .map_err(anyhow::Error::from)?
                            .into_data()
                            .unwrap_or_default();
                    }
                    None if self.line.is_empty() => return Ok(None),
                    None => break,
                }
            }
            let newline = self.chunk.iter().position(|b| *b == b'\n');
            let size = newline.unwrap_or(self.chunk.len());
            if self.line.len() + size > 4 << 20 {
                return Err(anyhow::anyhow!("oversized NDJSON event").into());
            }
            self.line.extend_from_slice(&self.chunk.split_to(size));
            if newline.is_some() {
                self.chunk.advance(1);
                break;
            }
        }
        let envelope: Envelope = serde_json::from_slice(&self.line).map_err(anyhow::Error::from)?;
        self.line.clear();
        match (envelope.event, envelope.error) {
            (Some(event), None) => Ok(Some(event)),
            (None, Some(error)) => Err(error.into()),
            _ => Err(anyhow::anyhow!("expected one event or error in NDJSON envelope").into()),
        }
    }
}
