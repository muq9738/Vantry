use bytes::Bytes;
use http::{Request, StatusCode};
use http_body_util::{BodyExt, Full};
use hyper::client::conn::http1;
use hyper_util::rt::TokioIo;
use serde_json::{Value, json};
use tokio::net::TcpStream;
use tokio_util::sync::CancellationToken;
use vantry_core::{Core, server::Server};
use vantry_protocol::{LOCAL_TOKEN_HEADER, LocalTcpEndpoint};

#[tokio::test]
async fn tcp_local_transport_requires_token_and_serves_protocol() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::open(dir.path().join("Vantry 数据目录 #1")).unwrap();
    let socket_file = core.directory().join("core.sock");
    let server = Server::bind_tcp(core.clone()).await.unwrap();
    let ep = LocalTcpEndpoint::parse_bytes(&std::fs::read(&socket_file).unwrap()).unwrap();
    let stop = CancellationToken::new();
    let serving = tokio::spawn(server.serve(stop.clone()));

    // Without token: rejected with 401.
    {
        let io = TcpStream::connect(&ep.addr).await.unwrap();
        let (mut tx, conn) = http1::handshake(TokioIo::new(io)).await.unwrap();
        let driver = tokio::spawn(conn);
        let req = Request::builder()
            .method("GET")
            .uri("/v1/health")
            .header("host", "vantry.local")
            .body(Full::new(Bytes::new()))
            .unwrap();
        let res = tx.send_request(req).await.unwrap();
        assert_eq!(res.status(), StatusCode::UNAUTHORIZED);
        let _ = res.into_body().collect().await;
        drop(tx);
        let _ = driver.await;
    }

    // With valid token: succeeds and dispatches command.
    {
        let io = TcpStream::connect(&ep.addr).await.unwrap();
        let (mut tx, conn) = http1::handshake(TokioIo::new(io)).await.unwrap();
        let driver = tokio::spawn(conn);
        let req = Request::builder()
            .method("POST")
            .uri("/v1/commands")
            .header("host", "vantry.local")
            .header(LOCAL_TOKEN_HEADER, &ep.token)
            .body(Full::new(Bytes::from(
                json!({"version":1,"kind":"thread.create","commandId":"tcp-1","threadId":"tcp-1","title":"Windows 中文"})
                    .to_string(),
            )))
            .unwrap();
        let res = tx.send_request(req).await.unwrap();
        assert_eq!(res.status(), StatusCode::OK);
        let bytes = res.into_body().collect().await.unwrap().to_bytes();
        let receipt: Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(receipt["threadId"], "tcp-1");
        drop(tx);
        let _ = driver.await;
    }

    stop.cancel();
    serving.await.unwrap().unwrap();
    assert!(!socket_file.exists(), "core.sock descriptor must be cleaned up on drop");
}
