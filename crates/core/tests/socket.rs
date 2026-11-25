#![cfg(unix)]
use bytes::Bytes;
use http::{Request, StatusCode};
use http_body_util::{BodyExt, Full};
use hyper::{body::Incoming, client::conn::http1};
use hyper_util::rt::TokioIo;
use serde_json::{Value, json};
use std::{path::Path, time::Duration};
use tokio::net::UnixStream;
use tokio_util::sync::CancellationToken;
use vantry_core::{Core, server::Server};
async fn request(
    path: &Path,
    url: &str,
    body: Option<Value>,
) -> (http::Response<Incoming>, tokio::task::JoinHandle<()>) {
    let io = UnixStream::connect(path).await.unwrap();
    let (mut client, conn) = http1::handshake(TokioIo::new(io)).await.unwrap();
    let task = tokio::spawn(async move {
        let _ = conn.await;
    });
    let req = Request::builder()
        .method(if body.is_some() { "POST" } else { "GET" })
        .uri(url)
        .header("host", "localhost")
        .body(Full::new(Bytes::from(
            body.map(|v| v.to_string()).unwrap_or_default(),
        )))
        .unwrap();
    (client.send_request(req).await.unwrap(), task)
}
async fn json_req(path: &Path, url: &str, body: Option<Value>) -> (StatusCode, Value) {
    let (res, handle) = request(path, url, body).await;
    let code = res.status();
    let bytes = res.into_body().collect().await.unwrap().to_bytes();
    handle.await.unwrap();
    (code, serde_json::from_slice(&bytes).unwrap())
}
#[tokio::test]
async fn replay_live_handoff_and_http_resync() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::with_retention(dir.path().join("data"), 2).unwrap();
    let socket = core.directory().join("core.sock");
    let server = Server::bind(core.clone()).await.unwrap();
    let stop = CancellationToken::new();
    let serving = tokio::spawn(server.serve(stop.clone()));
    let send = |id: &str| {
        Some(
            json!({"version":1,"kind":"thread.create","commandId":id,"threadId":id,"title":"你好"}),
        )
    };
    let mut bad = send("invalid").unwrap();
    bad["unrecognized"] = true.into();
    assert_eq!(json_req(&socket, "/v1/commands", Some(bad)).await.0, 400);
    let (code, first) = json_req(&socket, "/v1/commands", send("one")).await;
    assert_eq!(code, 200);
    assert_eq!(first["sequence"], 1);
    let (res, connection) = request(&socket, "/v1/events?after=0", None).await;
    assert_eq!(res.headers()["content-type"], "application/x-ndjson");
    let mut stream = res.into_body();
    for seq in 1..=4 {
        if seq > 1 {
            assert_eq!(
                json_req(&socket, "/v1/commands", send(&seq.to_string()))
                    .await
                    .0,
                200
            );
        }
        let frame = tokio::time::timeout(Duration::from_secs(2), stream.frame())
            .await
            .unwrap()
            .unwrap()
            .unwrap()
            .into_data()
            .unwrap();
        assert_eq!(frame.last(), Some(&b'\n'));
        let line: Value = serde_json::from_slice(&frame).unwrap();
        assert_eq!(line["event"]["sequence"], seq);
        assert_eq!(line["event"]["data"]["thread"]["title"], "你好");
    }
    let (code, error) = json_req(&socket, "/v1/events?after=1", None).await;
    assert_eq!(code, 409);
    assert_eq!(error["code"], "resync_required");
    assert_eq!(json_req(&socket, "/v1/events?after=-1", None).await.0, 400);
    assert_eq!(
        json_req(&socket, "/v1/health", None).await.1["state"],
        "ready"
    );
    let snapshot = json_req(&socket, "/v1/snapshot", None).await.1;
    assert_eq!(json_req(&socket, "/v1/state", None).await.1, snapshot);
    assert_eq!(snapshot["threads"].as_array().unwrap().len(), 4);
    let status = json_req(&socket, "/v1/status", None).await.1;
    assert_eq!(json_req(&socket, "/v1/capabilities", None).await.1, status);
    assert_eq!(status["version"], env!("CARGO_PKG_VERSION"));
    assert!(
        status["capabilities"]
            .as_array()
            .unwrap()
            .contains(&json!("provider.pi.embedded.v1"))
    );
    // Verify gzip compression on a large (>1KB) snapshot when Accept-Encoding: gzip is sent.
    {
        let long_title = "压缩测试".repeat(400);
        let (code, _) = json_req(
            &socket,
            "/v1/commands",
            Some(json!({"version":1,"kind":"thread.create","commandId":"gz1","threadId":"gz1","title":long_title})),
        )
        .await;
        assert_eq!(code, 200);
        let large_snapshot = json_req(&socket, "/v1/snapshot", None).await.1;

        let io = UnixStream::connect(&socket).await.unwrap();
        let (mut client, conn) = http1::handshake(TokioIo::new(io)).await.unwrap();
        let task = tokio::spawn(async move {
            let _ = conn.await;
        });
        let req = Request::builder()
            .method("GET")
            .uri("/v1/snapshot")
            .header("host", "localhost")
            .header("accept-encoding", "gzip")
            .body(Full::new(Bytes::new()))
            .unwrap();
        let res = client.send_request(req).await.unwrap();
        drop(client);
        assert_eq!(res.status(), StatusCode::OK);
        assert_eq!(res.headers()["content-encoding"], "gzip");
        let gz_bytes = res.into_body().collect().await.unwrap().to_bytes();
        task.await.unwrap();
        let mut decoder = flate2::read::GzDecoder::new(gz_bytes.as_ref());
        let mut plain = Vec::new();
        std::io::Read::read_to_end(&mut decoder, &mut plain).unwrap();
        assert!(gz_bytes.len() < plain.len());
        let decoded: Value = serde_json::from_slice(&plain).unwrap();
        assert_eq!(decoded, large_snapshot);
    }
    drop(stream);
    let (status, drain) = json_req(&socket, "/v1/admin/shutdown-if-idle", Some(json!({}))).await;
    assert_eq!(status, 200);
    assert_eq!(drain["state"], "draining");
    serving.await.unwrap().unwrap();
    connection.await.unwrap();
    assert!(!socket.exists());
}
