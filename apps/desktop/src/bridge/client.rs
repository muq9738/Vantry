//! [`CoreBridge`] over the real `vantry_client::CoreClient`: protocol v1 on
//! the Core's Unix socket.
//!
//! The client owns HTTP, NDJSON framing and the cursor; this adapter only
//! maps its errors ([`BridgeError::from`]) and forwards events to the sink.

use std::path::{Path, PathBuf};

use vantry_protocol::{Command, Health, Receipt, Snapshot, ThreadDetail, ThreadSummaries};

use super::{BridgeError, BridgeFuture, CoreBridge, EventSink};

/// The local Core, through `vantry-client`.
#[derive(Debug, Clone)]
pub struct ClientBridge {
    socket: PathBuf,
}

impl ClientBridge {
    pub fn new(socket: PathBuf) -> Self {
        ClientBridge { socket }
    }

    /// The socket it connects to.
    pub fn socket(&self) -> &Path {
        &self.socket
    }

    fn client(&self) -> vantry_client::CoreClient {
        vantry_client::CoreClient::new(self.socket.clone())
    }
}

impl CoreBridge for ClientBridge {
    fn label(&self) -> String {
        "本机 Core".to_owned()
    }

    fn health(&self) -> BridgeFuture<Health> {
        let client = self.client();
        Box::pin(async move { client.health().await.map_err(BridgeError::from) })
    }

    fn status(&self) -> BridgeFuture<Option<vantry_protocol::ServerStatus>> {
        let client = self.client();
        Box::pin(async move {
            client
                .capabilities()
                .await
                .map(Some)
                .map_err(BridgeError::from)
        })
    }

    fn snapshot(&self) -> BridgeFuture<Snapshot> {
        let client = self.client();
        Box::pin(async move { client.snapshot().await.map_err(BridgeError::from) })
    }

    fn thread_summaries(&self) -> BridgeFuture<ThreadSummaries> {
        let client = self.client();
        Box::pin(async move { client.thread_summaries().await.map_err(BridgeError::from) })
    }

    fn thread_detail(&self, thread_id: String, limit_runs: i64) -> BridgeFuture<ThreadDetail> {
        let client = self.client();
        Box::pin(async move {
            client
                .thread_detail(&thread_id, limit_runs)
                .await
                .map_err(BridgeError::from)
        })
    }

    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt> {
        let client = self.client();
        Box::pin(async move { client.dispatch(&command).await.map_err(BridgeError::from) })
    }

    fn subscribe(&self, after: i64, sink: EventSink) -> BridgeFuture<()> {
        let client = self.client();
        Box::pin(async move {
            let mut stream = client
                .subscribe(after.max(0))
                .await
                .map_err(BridgeError::from)?;
            loop {
                tokio::select! {
                    // The UI dropped the subscription: disconnect.
                    () = sink.closed() => return Ok(()),
                    next = stream.next_event() => match next.map_err(BridgeError::from)? {
                        Some(event) => {
                            if sink.send(event).is_err() {
                                return Ok(());
                            }
                        }
                        None => return Ok(()),
                    },
                }
            }
        })
    }
}

#[cfg(all(test, unix))]
mod tests {
    //! A fake Core on a Unix socket in a temporary directory: plain
    //! HTTP/1.1 written by hand, so the real client is what is tested.

    use std::time::Duration;

    use serde_json::json;
    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        net::{UnixListener, UnixStream},
        sync::mpsc,
    };
    use vantry_protocol::{CommandKind, CoreEvent};

    use super::*;
    use crate::bridge::ErrorCode;

    /// What the fake Core answers a request with.
    enum Reply {
        Json(u16, serde_json::Value),
        /// Chunked NDJSON records, then the end of the body.
        Stream(Vec<String>),
    }

    /// Reads one request: its request line and body.
    async fn read_request(stream: &mut UnixStream) -> (String, Vec<u8>) {
        let mut buf = Vec::new();
        let mut chunk = [0u8; 4096];
        let head_end = loop {
            let n = stream.read(&mut chunk).await.unwrap();
            assert!(n > 0, "the client closed mid-request");
            buf.extend_from_slice(&chunk[..n]);
            if let Some(at) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                break at + 4;
            }
        };
        let head = String::from_utf8_lossy(&buf[..head_end]).to_string();
        let length = head
            .lines()
            .find_map(|l| {
                let (name, value) = l.split_once(':')?;
                name.eq_ignore_ascii_case("content-length")
                    .then(|| value.trim().parse::<usize>().ok())?
            })
            .unwrap_or(0);
        while buf.len() < head_end + length {
            let n = stream.read(&mut chunk).await.unwrap();
            buf.extend_from_slice(&chunk[..n]);
        }
        let line = head.lines().next().unwrap_or_default().to_owned();
        (line, buf[head_end..head_end + length].to_vec())
    }

    async fn write_reply(stream: &mut UnixStream, reply: Reply) {
        match reply {
            Reply::Json(status, value) => {
                let body = value.to_string();
                let head = format!(
                    "HTTP/1.1 {status} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\n\r\n",
                    body.len()
                );
                stream.write_all(head.as_bytes()).await.unwrap();
                stream.write_all(body.as_bytes()).await.unwrap();
            }
            Reply::Stream(records) => {
                stream
                    .write_all(
                        b"HTTP/1.1 200 OK\r\ncontent-type: application/x-ndjson\r\ntransfer-encoding: chunked\r\n\r\n",
                    )
                    .await
                    .unwrap();
                for record in records {
                    let line = format!("{record}\n");
                    let chunk = format!("{:x}\r\n{line}\r\n", line.len());
                    stream.write_all(chunk.as_bytes()).await.unwrap();
                }
                stream.write_all(b"0\r\n\r\n").await.unwrap();
            }
        }
        stream.flush().await.unwrap();
    }

    /// Serves one connection per reply from `replies`, sending each request
    /// line and body to `seen`.
    fn serve(
        listener: UnixListener,
        replies: Vec<Reply>,
        seen: mpsc::UnboundedSender<(String, Vec<u8>)>,
    ) -> tokio::task::JoinHandle<()> {
        tokio::spawn(async move {
            for reply in replies {
                let (mut stream, _) = listener.accept().await.unwrap();
                let request = read_request(&mut stream).await;
                let _ = seen.send(request);
                write_reply(&mut stream, reply).await;
                let _ = stream.shutdown().await;
            }
        })
    }

    fn event(sequence: i64, kind: &str) -> String {
        json!({ "event": {
            "sequence": sequence,
            "id": format!("evt-{sequence}"),
            "type": kind,
            "threadId": "t1",
            "runId": "r1",
            "timestamp": "2026-11-23T08:00:00Z",
            "data": { "delta": "hi" },
        }})
        .to_string()
    }

    #[tokio::test]
    async fn snapshot_dispatch_and_api_errors_go_through_the_real_client() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("core.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let (seen, mut requests) = mpsc::unbounded_channel();
        let server = serve(
            listener,
            vec![
                Reply::Json(
                    200,
                    json!({ "sequence": 7, "threads": [], "runs": [], "tasks": [], "cohorts": [] }),
                ),
                Reply::Json(
                    200,
                    json!({ "commandId": "send-1", "accepted": true, "threadId": "t1", "runId": "r9", "sequence": 8 }),
                ),
                Reply::Json(
                    409,
                    json!({ "code": "conflict", "message": "commandId reused" }),
                ),
                Reply::Json(
                    500,
                    json!({ "code": "internal_error", "message": "disk full" }),
                ),
            ],
            seen,
        );
        let bridge = ClientBridge::new(socket.clone());

        let snapshot = bridge.snapshot().await.unwrap();
        assert_eq!(snapshot.sequence, 7);
        assert_eq!(requests.recv().await.unwrap().0, "GET /v1/snapshot HTTP/1.1");

        let mut command = Command::new("send-1", CommandKind::SendMessage);
        command.thread_id = "t1".into();
        command.message = "你好".into();
        let receipt = bridge.dispatch(command.clone()).await.unwrap();
        assert_eq!(receipt.run_id, "r9");
        let (line, body) = requests.recv().await.unwrap();
        assert_eq!(line, "POST /v1/commands HTTP/1.1");
        let sent: Command = serde_json::from_slice(&body).unwrap();
        assert_eq!(sent, command);

        let conflict = bridge.dispatch(command.clone()).await.unwrap_err();
        assert_eq!(conflict.code, ErrorCode::Conflict);
        assert!(!conflict.outcome_unknown());

        let internal = bridge.dispatch(command).await.unwrap_err();
        assert_eq!(internal.code, ErrorCode::Internal);
        assert!(internal.outcome_unknown(), "an internal error may have committed");

        server.await.unwrap();
        drop(dir);
    }

    #[tokio::test]
    async fn events_stream_in_order_then_resync_required_ends_the_stream() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("core.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        let (seen, mut requests) = mpsc::unbounded_channel();
        let server = serve(
            listener,
            vec![Reply::Stream(vec![
                event(4, "run.output"),
                event(5, "run.completed"),
                json!({ "error": { "code": "resync_required", "message": "cursor expired" } })
                    .to_string(),
            ])],
            seen,
        );
        let bridge = ClientBridge::new(socket);
        let (sink, mut events) = mpsc::unbounded_channel::<CoreEvent>();
        let result = tokio::time::timeout(Duration::from_secs(5), bridge.subscribe(3, sink))
            .await
            .expect("the stream ends");
        let err = result.unwrap_err();
        assert!(err.is_resync(), "{err}");
        assert_eq!(err.message, "cursor expired");
        assert_eq!(requests.recv().await.unwrap().0, "GET /v1/events?after=3 HTTP/1.1");
        assert_eq!(events.recv().await.unwrap().sequence, 4);
        let second = events.recv().await.unwrap();
        assert_eq!((second.sequence, second.r#type.as_str()), (5, "run.completed"));
        server.await.unwrap();
    }

    #[tokio::test]
    async fn a_missing_socket_is_unavailable_and_its_outcome_unknown() {
        let dir = tempfile::tempdir().unwrap();
        let bridge = ClientBridge::new(dir.path().join("absent.sock"));
        let err = bridge
            .dispatch(Command::new("c1", CommandKind::StopRun))
            .await
            .unwrap_err();
        assert_eq!(err.code, ErrorCode::Unavailable);
        assert!(err.outcome_unknown());
        assert_eq!(bridge.label(), "本机 Core");
        assert!(bridge.socket().ends_with("absent.sock"));
    }
}
