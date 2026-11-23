//! Protocol v1 over HTTP/1.1 on the Core's Unix socket.
//!
//! A deliberately small client: one connection per call (`Connection:
//! close`), `Content-Length`, chunked and read-to-EOF bodies, NDJSON event
//! streaming. It exists so the desktop runs against today's Core without
//! waiting on `vantry-client`; it speaks exactly what
//! `internal/adapters/transport/local/server.go` serves.

use std::path::PathBuf;

use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};

use super::{
    BridgeError, BridgeFuture, CoreBridge, ErrorCode, EventSink,
    wire::{ApiError, Command, Health, Receipt, Snapshot, StreamEnvelope},
};

/// The largest status line + headers accepted.
const MAX_HEAD_BYTES: usize = 64 << 10;
/// The largest JSON body accepted (a long history's snapshot).
const MAX_BODY_BYTES: usize = 256 << 20;
/// The largest single NDJSON event record (`maxEventBytes` is 4 MiB).
const MAX_EVENT_BYTES: usize = 4 << 20;

/// Protocol v1 on `<data dir>/core.sock`.
#[derive(Debug, Clone)]
pub struct UnixHttpBridge {
    socket: PathBuf,
}

impl UnixHttpBridge {
    pub fn new(socket: PathBuf) -> Self {
        UnixHttpBridge { socket }
    }

    /// The socket it connects to.
    pub fn socket(&self) -> &std::path::Path {
        &self.socket
    }
}

impl CoreBridge for UnixHttpBridge {
    fn label(&self) -> String {
        "本机 Core".to_owned()
    }

    fn health(&self) -> BridgeFuture<Health> {
        let socket = self.socket.clone();
        Box::pin(async move { get_json(&socket, "/v1/health").await })
    }

    fn snapshot(&self) -> BridgeFuture<Snapshot> {
        let socket = self.socket.clone();
        Box::pin(async move { get_json(&socket, "/v1/snapshot").await })
    }

    fn dispatch(&self, command: Command) -> BridgeFuture<Receipt> {
        let socket = self.socket.clone();
        Box::pin(async move {
            let body = serde_json::to_vec(&command)
                .map_err(|e| BridgeError::protocol(format!("encode command: {e}")))?;
            let mut response = request(&socket, "POST", "/v1/commands", Some(&body)).await?;
            let bytes = response.body.read_all(MAX_BODY_BYTES).await?;
            decode_json(response.status, &bytes)
        })
    }

    fn subscribe(&self, after: i64, sink: EventSink) -> BridgeFuture<()> {
        let socket = self.socket.clone();
        Box::pin(async move {
            let path = format!("/v1/events?after={}", after.max(0));
            let mut response = request(&socket, "GET", &path, None).await?;
            if response.status != 200 {
                let bytes = response.body.read_all(MAX_BODY_BYTES).await?;
                return Err(api_error(response.status, &bytes));
            }
            let mut lines = NdjsonLines::default();
            while let Some(chunk) = response.body.next_chunk().await? {
                for line in lines.push(&chunk)? {
                    if let Some(envelope) = parse_envelope(&line)? {
                        match envelope {
                            Ok(event) => {
                                if sink.send(event).is_err() {
                                    // The UI dropped the subscription.
                                    return Ok(());
                                }
                            }
                            Err(err) => return Err(err),
                        }
                    }
                }
            }
            if !lines.rest.iter().all(u8::is_ascii_whitespace) {
                return Err(BridgeError::unavailable("event stream ended mid-record"));
            }
            Ok(())
        })
    }
}

/// One NDJSON line: `None` for a blank line, an event, or the stream's error.
pub(crate) fn parse_envelope(
    line: &[u8],
) -> Result<Option<Result<super::wire::Event, BridgeError>>, BridgeError> {
    if line.iter().all(u8::is_ascii_whitespace) {
        return Ok(None);
    }
    let envelope: StreamEnvelope = serde_json::from_slice(line)
        .map_err(|e| BridgeError::protocol(format!("decode event record: {e}")))?;
    match (envelope.event, envelope.error) {
        (Some(event), _) => Ok(Some(Ok(event))),
        (None, Some(error)) => Ok(Some(Err(BridgeError::new(
            ErrorCode::parse(&error.code),
            error.message,
        )))),
        (None, None) => Err(BridgeError::protocol(
            "event record has neither event nor error",
        )),
    }
}

/// Splits a byte stream into lines, holding a partial last line.
#[derive(Default)]
pub(crate) struct NdjsonLines {
    rest: Vec<u8>,
}

impl NdjsonLines {
    pub(crate) fn push(&mut self, chunk: &[u8]) -> Result<Vec<Vec<u8>>, BridgeError> {
        self.rest.extend_from_slice(chunk);
        let mut lines = Vec::new();
        while let Some(end) = self.rest.iter().position(|&b| b == b'\n') {
            let line: Vec<u8> = self.rest.drain(..=end).collect();
            lines.push(line);
        }
        if self.rest.len() > MAX_EVENT_BYTES {
            return Err(BridgeError::protocol("event record exceeds 4 MiB"));
        }
        Ok(lines)
    }
}

async fn get_json<T: serde::de::DeserializeOwned>(
    socket: &std::path::Path,
    path: &str,
) -> Result<T, BridgeError> {
    let mut response = request(socket, "GET", path, None).await?;
    let bytes = response.body.read_all(MAX_BODY_BYTES).await?;
    decode_json(response.status, &bytes)
}

fn decode_json<T: serde::de::DeserializeOwned>(
    status: u16,
    bytes: &[u8],
) -> Result<T, BridgeError> {
    if !(200..300).contains(&status) {
        return Err(api_error(status, bytes));
    }
    serde_json::from_slice(bytes)
        .map_err(|e| BridgeError::protocol(format!("decode response (HTTP {status}): {e}")))
}

/// The `api.Error` of a failed response, or a protocol error naming the status.
fn api_error(status: u16, bytes: &[u8]) -> BridgeError {
    match serde_json::from_slice::<ApiError>(bytes) {
        Ok(error) if !error.code.is_empty() => {
            BridgeError::new(ErrorCode::parse(&error.code), error.message)
        }
        _ => BridgeError::protocol(format!(
            "HTTP {status}: {}",
            String::from_utf8_lossy(&bytes[..bytes.len().min(256)])
        )),
    }
}

type Stream = BufReader<tokio::net::UnixStream>;

struct Response {
    status: u16,
    body: Body,
}

async fn request(
    socket: &std::path::Path,
    method: &str,
    path: &str,
    body: Option<&[u8]>,
) -> Result<Response, BridgeError> {
    let stream = tokio::net::UnixStream::connect(socket)
        .await
        .map_err(|e| BridgeError::unavailable(format!("connect {}: {e}", socket.display())))?;
    let mut stream = BufReader::new(stream);
    let mut head = format!(
        "{method} {path} HTTP/1.1\r\nHost: localhost\r\nUser-Agent: vantry-desktop\r\nAccept: application/json, application/x-ndjson\r\nConnection: close\r\n"
    );
    if let Some(body) = body {
        head.push_str("Content-Type: application/json\r\n");
        head.push_str(&format!("Content-Length: {}\r\n", body.len()));
    }
    head.push_str("\r\n");
    let writer = stream.get_mut();
    writer.write_all(head.as_bytes()).await.map_err(io_error)?;
    if let Some(body) = body {
        writer.write_all(body).await.map_err(io_error)?;
    }
    writer.flush().await.map_err(io_error)?;
    read_response(stream).await
}

fn io_error(err: std::io::Error) -> BridgeError {
    BridgeError::unavailable(err.to_string())
}

async fn read_line<R: AsyncBufRead + Unpin>(
    reader: &mut R,
    budget: &mut usize,
) -> Result<String, BridgeError> {
    let mut line = Vec::new();
    let n = reader
        .read_until(b'\n', &mut line)
        .await
        .map_err(io_error)?;
    if n == 0 {
        return Err(BridgeError::unavailable(
            "connection closed in the response head",
        ));
    }
    *budget = budget
        .checked_sub(n)
        .ok_or_else(|| BridgeError::protocol("response head too large"))?;
    while matches!(line.last(), Some(b'\n' | b'\r')) {
        line.pop();
    }
    String::from_utf8(line).map_err(|_| BridgeError::protocol("response head is not UTF-8"))
}

async fn read_response(mut stream: Stream) -> Result<Response, BridgeError> {
    let mut budget = MAX_HEAD_BYTES;
    let status_line = read_line(&mut stream, &mut budget).await?;
    let mut parts = status_line.splitn(3, ' ');
    let (Some(version), Some(code)) = (parts.next(), parts.next()) else {
        return Err(BridgeError::protocol(format!(
            "bad status line {status_line:?}"
        )));
    };
    if !version.starts_with("HTTP/1.") {
        return Err(BridgeError::protocol(format!(
            "not HTTP/1.x: {status_line:?}"
        )));
    }
    let status: u16 = code
        .parse()
        .map_err(|_| BridgeError::protocol(format!("bad status code {code:?}")))?;
    let mut chunked = false;
    let mut length = None;
    loop {
        let line = read_line(&mut stream, &mut budget).await?;
        if line.is_empty() {
            break;
        }
        let Some((name, value)) = line.split_once(':') else {
            return Err(BridgeError::protocol(format!("bad header {line:?}")));
        };
        let value = value.trim();
        if name.eq_ignore_ascii_case("transfer-encoding") {
            chunked = value.to_ascii_lowercase().contains("chunked");
        } else if name.eq_ignore_ascii_case("content-length") {
            length = Some(
                value
                    .parse::<usize>()
                    .map_err(|_| BridgeError::protocol(format!("bad content-length {value:?}")))?,
            );
        } else if name.eq_ignore_ascii_case("content-encoding")
            && !value.eq_ignore_ascii_case("identity")
        {
            // The local socket never compresses (only network transports do).
            return Err(BridgeError::protocol(format!(
                "unsupported content-encoding {value}"
            )));
        }
    }
    let kind = if chunked {
        BodyKind::Chunked { done: false }
    } else if let Some(length) = length {
        BodyKind::Length(length)
    } else {
        BodyKind::Eof
    };
    Ok(Response {
        status,
        body: Body { stream, kind },
    })
}

enum BodyKind {
    Chunked { done: bool },
    Length(usize),
    Eof,
}

struct Body {
    stream: Stream,
    kind: BodyKind,
}

impl Body {
    /// The next piece of the body as it arrives; `None` at its end.
    async fn next_chunk(&mut self) -> Result<Option<Vec<u8>>, BridgeError> {
        match &mut self.kind {
            BodyKind::Chunked { done: true } => Ok(None),
            BodyKind::Chunked { done } => {
                let mut budget = MAX_HEAD_BYTES;
                let size_line = read_line(&mut self.stream, &mut budget).await?;
                let size_text = size_line.split(';').next().unwrap_or("").trim();
                let size = usize::from_str_radix(size_text, 16)
                    .map_err(|_| BridgeError::protocol(format!("bad chunk size {size_line:?}")))?;
                if size == 0 {
                    // Trailers, up to the blank line.
                    loop {
                        if read_line(&mut self.stream, &mut budget).await?.is_empty() {
                            break;
                        }
                    }
                    *done = true;
                    return Ok(None);
                }
                if size > MAX_EVENT_BYTES * 2 {
                    return Err(BridgeError::protocol("chunk too large"));
                }
                let mut data = vec![0; size];
                self.stream.read_exact(&mut data).await.map_err(io_error)?;
                let mut crlf = [0u8; 2];
                self.stream.read_exact(&mut crlf).await.map_err(io_error)?;
                if &crlf != b"\r\n" {
                    return Err(BridgeError::protocol("chunk not terminated by CRLF"));
                }
                Ok(Some(data))
            }
            BodyKind::Length(0) => Ok(None),
            BodyKind::Length(remaining) => {
                let mut data = vec![0; (*remaining).min(64 << 10)];
                let n = self.stream.read(&mut data).await.map_err(io_error)?;
                if n == 0 {
                    return Err(BridgeError::unavailable("connection closed mid-body"));
                }
                *remaining -= n;
                data.truncate(n);
                Ok(Some(data))
            }
            BodyKind::Eof => {
                let mut data = vec![0; 64 << 10];
                let n = self.stream.read(&mut data).await.map_err(io_error)?;
                if n == 0 {
                    return Ok(None);
                }
                data.truncate(n);
                Ok(Some(data))
            }
        }
    }

    async fn read_all(&mut self, limit: usize) -> Result<Vec<u8>, BridgeError> {
        let mut all = Vec::new();
        while let Some(chunk) = self.next_chunk().await? {
            all.extend_from_slice(&chunk);
            if all.len() > limit {
                return Err(BridgeError::protocol("response body too large"));
            }
        }
        Ok(all)
    }
}

#[cfg(test)]
mod tests {
    use std::{
        path::PathBuf,
        sync::atomic::{AtomicU32, Ordering},
    };

    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        net::UnixListener,
    };

    use super::*;
    use crate::bridge::wire::{Command, kind};

    /// A private directory for one test's socket, removed on drop (after
    /// the server task, which the test's runtime ends first).
    struct TempSocket {
        dir: PathBuf,
    }

    impl TempSocket {
        fn new() -> Self {
            static N: AtomicU32 = AtomicU32::new(0);
            let dir = std::env::temp_dir().join(format!(
                "vantry-desktop-test-{}-{}",
                std::process::id(),
                N.fetch_add(1, Ordering::Relaxed)
            ));
            std::fs::create_dir_all(&dir).unwrap();
            TempSocket { dir }
        }

        fn path(&self) -> PathBuf {
            self.dir.join("core.sock")
        }
    }

    impl Drop for TempSocket {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    /// Serves one connection: reads the request head (and body), answers
    /// `response`, returns what was requested.
    async fn serve_once(listener: &UnixListener, response: Vec<u8>) -> String {
        let (mut conn, _) = listener.accept().await.unwrap();
        let mut request = Vec::new();
        let mut buf = [0u8; 4096];
        loop {
            let n = conn.read(&mut buf).await.unwrap();
            request.extend_from_slice(&buf[..n]);
            let text = String::from_utf8_lossy(&request).to_string();
            if let Some(head_end) = text.find("\r\n\r\n") {
                let length = text[..head_end]
                    .lines()
                    .find_map(|l| l.strip_prefix("Content-Length: "))
                    .map(|v| v.trim().parse::<usize>().unwrap())
                    .unwrap_or(0);
                if request.len() >= head_end + 4 + length {
                    break;
                }
            }
            if n == 0 {
                break;
            }
        }
        conn.write_all(&response).await.unwrap();
        conn.shutdown().await.unwrap();
        String::from_utf8(request).unwrap()
    }

    fn chunked(status: &str, content_type: &str, pieces: &[&str]) -> Vec<u8> {
        let mut out = format!(
            "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nTransfer-Encoding: chunked\r\n\r\n"
        );
        for piece in pieces {
            out.push_str(&format!("{:x}\r\n{piece}\r\n", piece.len()));
        }
        out.push_str("0\r\n\r\n");
        out.into_bytes()
    }

    fn fixed(status: &str, body: &str) -> Vec<u8> {
        format!(
            "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
            body.len()
        )
        .into_bytes()
    }

    #[tokio::test]
    async fn snapshot_reads_a_chunked_body() {
        let temp = TempSocket::new();
        let listener = UnixListener::bind(temp.path()).unwrap();
        let bridge = UnixHttpBridge::new(temp.path());
        let body = r#"{"sequence":7,"threads":[{"id":"t1","title":"你好","createdAt":"2026-11-23T00:00:00Z"}],"runs":[],"tasks":[],"cohorts":[]}"#;
        let (half_a, half_b) = body.split_at(20);
        let server = tokio::spawn(async move {
            serve_once(
                &listener,
                chunked("200 OK", "application/json", &[half_a, half_b]),
            )
            .await
        });
        let snapshot = bridge.snapshot().await.unwrap();
        let request = server.await.unwrap();
        assert!(request.starts_with("GET /v1/snapshot HTTP/1.1\r\n"));
        assert!(request.contains("Host: localhost"));
        assert_eq!(snapshot.sequence, 7);
        assert_eq!(snapshot.threads[0].title, "你好");
    }

    #[tokio::test]
    async fn dispatch_posts_the_exact_command_and_reads_the_receipt() {
        let temp = TempSocket::new();
        let listener = UnixListener::bind(temp.path()).unwrap();
        let bridge = UnixHttpBridge::new(temp.path());
        let server = tokio::spawn(async move {
            serve_once(
                &listener,
                fixed(
                    "200 OK",
                    r#"{"commandId":"send-1","accepted":true,"threadId":"t1","runId":"r9","sequence":12}"#,
                ),
            )
            .await
        });
        let mut command = Command::new(kind::SEND_MESSAGE, "send-1");
        command.thread_id = "t1".into();
        command.message = "第一行\n第二行".into();
        let receipt = bridge.dispatch(command).await.unwrap();
        let request = server.await.unwrap();
        assert!(request.starts_with("POST /v1/commands HTTP/1.1\r\n"));
        let body = &request[request.find("\r\n\r\n").unwrap() + 4..];
        let sent: serde_json::Value = serde_json::from_str(body).unwrap();
        assert_eq!(
            sent,
            serde_json::json!({
                "version": 1,
                "commandId": "send-1",
                "kind": "message.send",
                "threadId": "t1",
                "message": "第一行\n第二行"
            })
        );
        assert!(receipt.accepted);
        assert_eq!(receipt.run_id, "r9");
    }

    #[tokio::test]
    async fn api_errors_keep_their_code() {
        let temp = TempSocket::new();
        let listener = UnixListener::bind(temp.path()).unwrap();
        let bridge = UnixHttpBridge::new(temp.path());
        let server = tokio::spawn(async move {
            serve_once(
                &listener,
                fixed(
                    "409 Conflict",
                    r#"{"code":"conflict","message":"commandId reused with other content"}"#,
                ),
            )
            .await
        });
        let err = bridge
            .dispatch(Command::new(kind::STOP_RUN, "stop-1"))
            .await
            .unwrap_err();
        server.await.unwrap();
        assert_eq!(err.code, ErrorCode::Conflict);
        assert!(!err.outcome_unknown());
    }

    #[tokio::test]
    async fn subscribe_streams_events_across_chunk_boundaries_then_reports_resync() {
        let temp = TempSocket::new();
        let listener = UnixListener::bind(temp.path()).unwrap();
        let bridge = UnixHttpBridge::new(temp.path());
        let first = r#"{"event":{"sequence":8,"id":"e8","type":"run.output","threadId":"t1","runId":"r1","timestamp":"2026-11-23T00:00:00Z","data":{"delta":"你"}}}"#;
        let second = r#"{"event":{"sequence":9,"id":"e9","type":"run.output","threadId":"t1","runId":"r1","timestamp":"2026-11-23T00:00:00Z","data":{"delta":"好"}}}"#;
        let error = r#"{"error":{"code":"resync_required","message":"slow consumer"}}"#;
        let stream = format!("{first}\n{second}\n{error}\n");
        // Split inside a multi-byte character and inside a record.
        let cut = stream.find('你').unwrap() + 1;
        let bytes = stream.into_bytes();
        let (a, b) = (bytes[..cut].to_vec(), bytes[cut..].to_vec());
        let server = tokio::spawn(async move {
            let parts = [a.as_slice(), b.as_slice()];
            let mut out =
                b"HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n".to_vec();
            for part in parts {
                out.extend_from_slice(format!("{:x}\r\n", part.len()).as_bytes());
                out.extend_from_slice(part);
                out.extend_from_slice(b"\r\n");
            }
            out.extend_from_slice(b"0\r\n\r\n");
            serve_once(&listener, out).await
        });
        let (sink, mut events) = tokio::sync::mpsc::unbounded_channel();
        let err = bridge.subscribe(7, sink).await.unwrap_err();
        let request = server.await.unwrap();
        assert!(request.starts_with("GET /v1/events?after=7 HTTP/1.1\r\n"));
        assert!(err.is_resync());
        let e8 = events.recv().await.unwrap();
        let e9 = events.recv().await.unwrap();
        assert_eq!((e8.sequence, e9.sequence), (8, 9));
        assert_eq!(e8.data["delta"], "你");
        assert!(events.try_recv().is_err());
    }

    #[tokio::test]
    async fn a_missing_socket_is_unavailable() {
        let temp = TempSocket::new();
        let bridge = UnixHttpBridge::new(temp.path());
        let err = bridge.health().await.unwrap_err();
        assert_eq!(err.code, ErrorCode::Unavailable);
    }

    #[test]
    fn ndjson_lines_hold_partial_records() {
        let mut lines = NdjsonLines::default();
        assert!(lines.push(b"{\"a\":").unwrap().is_empty());
        let out = lines.push(b"1}\n{\"b\":2}\n{").unwrap();
        assert_eq!(out, vec![b"{\"a\":1}\n".to_vec(), b"{\"b\":2}\n".to_vec()]);
        assert_eq!(lines.rest, b"{");
        assert!(parse_envelope(b"  \n").unwrap().is_none());
    }
}
