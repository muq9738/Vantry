#![cfg(unix)]
use serde_json::{Value, json};
use std::{
    fs,
    path::PathBuf,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::{UnixListener, UnixStream},
    sync::oneshot,
};
use vantry_client::{ClientError, CoreClient, protocol::*};

struct SocketDir(PathBuf);
impl Drop for SocketDir {
    fn drop(&mut self) {
        fs::remove_dir_all(&self.0).unwrap();
    }
}
async fn request(listener: &UnixListener, method: &str, path: &str) -> (UnixStream, Option<Value>) {
    let (io, _) = listener.accept().await.unwrap();
    let mut reader = BufReader::new(io);
    let mut line = String::new();
    reader.read_line(&mut line).await.unwrap();
    assert_eq!(line.trim_end(), format!("{method} {path} HTTP/1.1"));
    let mut len = 0;
    loop {
        line.clear();
        reader.read_line(&mut line).await.unwrap();
        if line == "\r\n" {
            break;
        }
        if let Some(v) = line.to_lowercase().strip_prefix("content-length:") {
            len = v.trim().parse().unwrap();
        }
    }
    let mut body = vec![0; len];
    reader.read_exact(&mut body).await.unwrap();
    (
        reader.into_inner(),
        (!body.is_empty()).then(|| serde_json::from_slice(&body).unwrap()),
    )
}
async fn reply(mut io: UnixStream, status: &str, value: Value) {
    let body = value.to_string();
    io.write_all(format!("HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",body.len()).as_bytes()).await.unwrap();
}
async fn chunk(io: &mut UnixStream, text: &str) {
    io.write_all(format!("{:x}\r\n{text}\r\n", text.len()).as_bytes())
        .await
        .unwrap();
}

#[tokio::test]
async fn v1_socket_command_replay_and_resync() {
    let dir = SocketDir(PathBuf::from("/tmp").join(format!(
            "vnt-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        )));
    fs::create_dir(&dir.0).unwrap();
    let listener = UnixListener::bind(dir.0.join("core.sock")).unwrap();
    let (resume, wait) = oneshot::channel();
    let server = tokio::spawn(async move {
        let (io, _) = request(&listener, "GET", "/v1/health").await;
        reply(io, "200 OK", json!({"version":1,"state":"ready"})).await;
        let (io, _) = request(&listener, "GET", "/v1/status").await;
        reply(io,"200 OK",json!({"protocolVersion":1,"schemaVersion":1,"state":"ready","activeRuns":0,"pid":2,"capabilities":[EMBEDDED_PI_CAPABILITY,LEGACY_PI_RUNTIME_CAPABILITY]})).await;
        let (io, _) = request(&listener, "GET", "/v1/snapshot").await;
        reply(
            io,
            "200 OK",
            json!({"sequence":7,"threads":[],"runs":null,"tasks":[],"cohorts":[]}),
        )
        .await;
        let (io, body) = request(&listener, "POST", "/v1/commands").await;
        assert_eq!(
            body.unwrap(),
            json!({"version":1,"commandId":"c1","kind":"request.respond","requestId":"a1:q1","confirmed":true})
        );
        reply(
            io,
            "200 OK",
            json!({"commandId":"c1","accepted":true,"threadId":"t1","sequence":8}),
        )
        .await;
        let (mut io, _) = request(&listener, "GET", "/v1/events?after=7").await;
        io.write_all(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Type: application/x-ndjson\r\n\r\n").await.unwrap();
        chunk(&mut io, "{\"event\":").await;
        chunk(&mut io,"{\"sequence\":8,\"id\":\"e\",\"threadId\":\"t1\",\"type\":\"run.output\",\"timestamp\":\"2026-01-02T00:00:00Z\",\"data\":{\"delta\":\"文\"}}}\n").await;
        wait.await.unwrap();
        chunk(
            &mut io,
            "{\"error\":{\"code\":\"resync_required\",\"message\":\"lagged\"}}\n",
        )
        .await;
        io.write_all(b"0\r\n\r\n").await.unwrap();
        drop(io);
        let (io, _) = request(&listener, "GET", "/v1/events?after=1").await;
        reply(
            io,
            "409 Conflict",
            json!({"code":"resync_required","message":"cursor pruned"}),
        )
        .await;
        for (baseline, alias, projection) in [
            (
                "/v1/status",
                "/v1/capabilities",
                json!({"protocolVersion":1,"schemaVersion":1,"state":"ready","activeRuns":0,"pid":2,"capabilities":[]}),
            ),
            (
                "/v1/snapshot",
                "/v1/state",
                json!({"sequence":9,"threads":[],"runs":[],"tasks":[],"cohorts":[]}),
            ),
        ] {
            let (io, _) = request(&listener, "GET", baseline).await;
            reply(
                io,
                "404 Not Found",
                json!({"code":"not_found","message":"unknown route"}),
            )
            .await;
            let (io, _) = request(&listener, "GET", alias).await;
            reply(io, "200 OK", projection).await;
        }
    });
    tokio::time::timeout(Duration::from_secs(8), async {
        let client = CoreClient::from_data_dir(&dir.0);
        assert_eq!(client.health().await.unwrap().state, "ready");
        let caps = client.capabilities().await.unwrap();
        assert!(caps.has_capability(EMBEDDED_PI_CAPABILITY));
        assert!(caps.has_capability(LEGACY_PI_RUNTIME_CAPABILITY));
        let sequence = client.snapshot().await.unwrap().sequence;
        let command = Command {
            request_id: "a1:q1".into(),
            confirmed: true,
            ..Command::new("c1", CommandKind::RespondRequest)
        };
        assert!(client.dispatch(&command).await.unwrap().accepted);
        let mut events = client.subscribe(sequence).await.unwrap();
        assert_eq!(
            events.next_event().await.unwrap().unwrap().data["delta"],
            "文"
        );
        assert_eq!(events.cursor(), 8);
        resume.send(()).unwrap();
        assert!(matches!(
            events.next_event().await,
            Err(ClientError::ResyncRequired(_))
        ));
        assert!(events.next_event().await.unwrap().is_none());
        assert!(matches!(
            client.subscribe(1).await,
            Err(ClientError::ResyncRequired(_))
        ));
        assert_eq!(client.capabilities().await.unwrap().protocol_version, 1);
        assert_eq!(client.snapshot().await.unwrap().sequence, 9);
        server.await.unwrap();
    })
    .await
    .unwrap();
}
