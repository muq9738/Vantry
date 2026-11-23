#![cfg(unix)]
use async_trait::async_trait;
use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper_util::rt::TokioIo;
use serde_json::{Value, json};
use std::{path::Path, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
use vantry_core::{
    Core, DurableProvider,
    runner::{Emit, ProviderResult, StartMode},
};
use vantry_protocol::{ControlResponse, ProviderEvent, ProviderRequest};
struct Echo;
#[async_trait]
impl DurableProvider for Echo {
    async fn recoverable(&self, _: &ProviderRequest) -> bool {
        false
    }
    async fn stop(&self, _: &ProviderRequest) -> ProviderResult {
        Ok(())
    }
    async fn control(&self, _: &ProviderRequest, _: &ControlResponse) -> ProviderResult {
        Ok(())
    }
    async fn run_events(&self, r: ProviderRequest, mode: StartMode, emit: Emit) -> ProviderResult {
        assert_eq!(mode, StartMode::Submit);
        emit(ProviderEvent {
            sequence: 1,
            delta: r.message,
            ..Default::default()
        })
        .unwrap();
        Ok(())
    }
}
async fn post(path: &Path, url: &str, data: Value) -> Value {
    let stream = tokio::net::UnixStream::connect(path).await.unwrap();
    let (mut sender, conn) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
        .await
        .unwrap();
    let driver = tokio::spawn(conn);
    let req = http::Request::builder()
        .method("POST")
        .uri(url)
        .header("host", "localhost")
        .header("connection", "close")
        .body(Full::new(Bytes::from(data.to_string())))
        .unwrap();
    let res = sender.send_request(req).await.unwrap();
    let status = res.status();
    let data = res.into_body().collect().await.unwrap().to_bytes();
    driver.await.unwrap().unwrap();
    assert_eq!(status, 200);
    serde_json::from_slice(&data).unwrap()
}
#[tokio::test]
async fn command_reaches_provider_and_completion_reaches_socket_subscriber() {
    tokio::time::timeout(Duration::from_secs(8), async {
        let root = tempfile::tempdir().unwrap();
        let core = Core::open(root.path().join("private")).unwrap();
        let socket = core.directory().join("core.sock");
        let serve = tokio::spawn({
            let core = core.clone();
            async move { core.serve(Arc::new(Echo), CancellationToken::new()).await }
        });
        while !socket.exists() {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        let created = post(&socket, "/v1/commands", json!({"version":1,"kind":"thread.create","commandId":"create"})).await;
        let io = tokio::net::UnixStream::connect(&socket).await.unwrap();
        let (mut tx, conn) = hyper::client::conn::http1::handshake(TokioIo::new(io)).await.unwrap();
        let driver = tokio::spawn(conn);
        let mut body = tx.send_request(http::Request::builder().uri("/v1/events?after=1").header("host", "localhost").body(Full::new(Bytes::new())).unwrap()).await.unwrap().into_body();
        let sent = post(&socket, "/v1/commands", json!({"version":1,"kind":"message.send","commandId":"send","threadId":created["threadId"],"message":"socket reply"})).await;
        let mut seen = 1;
        let mut text = String::new();
        let mut buf = vec![];
        'records: while let Some(frame) = body.frame().await {
            buf.extend_from_slice(&frame.unwrap().into_data().unwrap());
            while let Some(end) = buf.iter().position(|&x| x == b'\n') {
                let v: Value = serde_json::from_slice(&buf[..end]).unwrap();
                buf.drain(..=end);
                let ev = &v["event"];
                seen += 1;
                assert_eq!(ev["sequence"], seen);
                assert_eq!(ev["runId"], sent["runId"]);
                if ev["type"] == "run.output" {
                    text.push_str(ev["data"]["delta"].as_str().unwrap());
                }
                if ev["type"] == "run.completed" {
                    assert_eq!(ev["data"]["run"]["output"], "socket reply");
                    break 'records;
                }
            }
        }
        assert_eq!(text, "socket reply");
        assert_eq!(post(&socket, "/v1/admin/shutdown-if-idle", json!({})).await["state"], "draining");
        serve.await.unwrap().unwrap();
        drop(body);
        drop(tx);
        driver.await.unwrap().unwrap();
        assert!(!socket.exists());
    })
    .await
    .unwrap();
}
