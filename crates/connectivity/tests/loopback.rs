#![cfg(unix)]
use anyhow::{Result, ensure};
use futures::{SinkExt, StreamExt};
use http::Method;
use http_body_util::BodyExt;
use serde_json::{Value, json};
use std::{sync::Arc, time::Duration};
use tempfile::TempDir;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, UnixListener},
    sync::watch,
    time::timeout,
};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    identity::{IdentityStore, Pair},
    pairing::token,
    peer::{HostConfig, HostState, PeerConnection, Route, redeem, serve_gateway},
    relay::{Relay, SessionConfig},
    serve,
    transport::websocket,
};
#[tokio::test]
async fn two_devices_relay_stream_reconnect_and_revoke() -> Result<()> {
    timeout(Duration::from_secs(20), run_two_devices()).await?
}
async fn core_fixture(listener: UnixListener, stop: CancellationToken) {
    let mut children = tokio::task::JoinSet::new();
    loop {
        let socket = tokio::select! {
            _ = stop.cancelled() => break,
            result = listener.accept() => result.unwrap().0,
            Some(_) = children.join_next() => continue,
        };
        children.spawn(async move {
            let mut socket = socket;
            let mut buf = vec![];
            while buf.len() < 32768 && !buf.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                if socket.read_exact(&mut byte).await.is_err() {
                    return;
                }
                buf.push(byte[0]);
            }
            if buf.starts_with(b"GET /v1/health ") {
                let _ = socket
                    .write_all(
                        b"HTTP/1.1 200 OK\r\nContent-Length: 17\r\n\r\n{\"version\":\"old\"}",
                    )
                    .await;
            } else if buf.starts_with(b"GET /v1/events?") {
                assert!(String::from_utf8_lossy(&buf).contains("x-vantry-event-keepalive: 1"));
                let body = "\n{\"keepalive\":true}\n{\"event\":{\"sequence\":4,\"id\":\"e\",\"type\":\"run.cancelled\",\"threadId\":\"t\",\"timestamp\":\"now\",\"data\":{}}}\n";
                socket.write_all(format!("HTTP/1.1 200 OK\r\nx-vantry-event-keepalive: 1\r\nContent-Length: {}\r\n\r\n{body}", body.len()).as_bytes()).await.unwrap();
            } else {
                let _ = socket
                    .write_all(
                        b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nfirst\n\r\n",
                    )
                    .await;
                let mut byte = [0];
                let _ = socket.read(&mut byte).await;
            }
        });
    }
    children.shutdown().await;
}
async fn run_two_devices() -> Result<()> {
    let tmp = TempDir::new()?;
    let path = tmp.path().join("core.sock");
    let local = UnixListener::bind(&path)?;
    let stop = CancellationToken::new();
    let core = tokio::spawn(core_fixture(local, stop.clone()));
    let host = Arc::new(IdentityStore::open(&tmp.path().join("host"), "Host")?);
    let guest = IdentityStore::open(&tmp.path().join("guest"), "Guest")?;
    let gateway_listener = TcpListener::bind("127.0.0.1:0").await?;
    let gateway = gateway_listener.local_addr()?;
    let gw = tokio::spawn(serve_gateway(
        gateway_listener,
        host.clone(),
        path.clone(),
        false,
        stop.clone(),
    ));
    let (key, host_key) = (token(), token());
    let cfg = SessionConfig {
        registration_key: key.clone(),
        host_credentials: vec![vantry_connectivity::relay::Credential {
            key: host_key,
            routes: vec!["route_0123456789abcdef".into()],
        }],
        ..Default::default()
    };
    let relay_listener = TcpListener::bind("127.0.0.1:0").await?;
    let relay_addr = relay_listener.local_addr()?;
    let base = format!("http://{relay_addr}");
    let first_stop = stop.child_token();
    let first = tokio::spawn(serve(
        relay_listener,
        Arc::new(Relay::new(Some(cfg.clone()), None)?),
        None,
        first_stop.clone(),
    ));
    host.set_routes(vec![], vec![base.clone()])?;
    let bootstrap = host.issue()?;
    ensure!(bootstrap.secret.is_empty());
    ensure!(Pair::parse(&bootstrap.link(), false)?.v == 2);
    let (tx, mut status) = watch::channel(HostState::Connecting);
    let agent = tokio::spawn(
        HostConfig {
            relay: base.clone(),
            route: bootstrap.route.clone(),
            key: key.clone(),
            instance: bootstrap.device_id.clone(),
            gateway,
            t3: None,
        }
        .run(tx, stop.clone()),
    );
    status.wait_for(|s| *s == HostState::Online).await?;
    let id = redeem(&bootstrap, &guest, "Guest", Route::Relay, stop.clone()).await?;
    ensure!(
        redeem(&bootstrap, &guest, "Guest", Route::Relay, stop.clone())
            .await
            .is_err()
    );
    let cfg_guest = IdentityStore::open(&tmp.path().join("guest"), "ignored")?.snapshot();
    ensure!(cfg_guest.peers[0].id == id);
    let peer = &cfg_guest.peers[0].pair;
    let client = PeerConnection::connect(peer, None, Route::Auto, stop.clone()).await?;
    ensure!(client.mode == "relay");
    ensure!(client.health().await?["version"] == "old");
    let client = PeerConnection::connect(peer, None, Route::Relay, stop.clone()).await?;
    let mut stream = client.subscribe_events(3).await?;
    ensure!(stream.next_event().await?.unwrap().sequence == 4);
    ensure!(stream.next_event().await?.is_none());
    let publisher = vantry_connectivity::bootstrap::Publisher::new(host.clone())?;
    let short = publisher.issue(&key).await?;
    let other = IdentityStore::open(&tmp.path().join("other"), "Second")?;
    vantry_connectivity::bootstrap::redeem_code(&base, &short.code, &other, "Second", stop.clone())
        .await?;
    ensure!(other.snapshot().peers.len() == 1);
    ensure!(
        vantry_connectivity::bootstrap::redeem_code(
            &base,
            &short.code,
            &other,
            "Second",
            stop.clone()
        )
        .await
        .is_err()
    );
    publisher.revoke().await?;
    first_stop.cancel();
    first.await??;
    status.wait_for(|s| *s != HostState::Online).await?;
    let listener = TcpListener::bind(relay_addr).await?;
    let second = tokio::spawn(serve(
        listener,
        Arc::new(Relay::new(Some(cfg), None)?),
        None,
        stop.clone(),
    ));
    status.wait_for(|s| *s == HostState::Online).await?;
    let strict_listener = TcpListener::bind("127.0.0.1:0").await?;
    let mut direct = peer.clone();
    direct.direct = vec![strict_listener.local_addr()?.to_string()];
    let strict = tokio::spawn(serve_gateway(
        strict_listener,
        host.clone(),
        path,
        true,
        stop.clone(),
    ));
    let ok =
        PeerConnection::connect(&direct, Some(&cfg_guest), Route::Direct, stop.clone()).await?;
    ensure!(ok.health().await?["version"] == "old");
    let stranger = IdentityStore::open(&tmp.path().join("stranger"), "other")?.snapshot();
    if let Ok(conn) =
        PeerConnection::connect(&direct, Some(&stranger), Route::Direct, stop.clone()).await
    {
        ensure!(conn.health().await.is_err());
    }
    let c = PeerConnection::connect(peer, None, Route::Relay, stop.clone()).await?;
    let mut body = c
        .request(Method::GET, "/v1/stream", None)
        .await?
        .into_body();
    let frame = timeout(Duration::from_secs(2), body.frame())
        .await?
        .unwrap()?
        .into_data()
        .unwrap();
    ensure!(frame == "first\n");
    let id = &host.snapshot().clients[0].id;
    ensure!(host.revoke_client(id)?);
    let tail = timeout(Duration::from_secs(2), body.frame()).await?;
    ensure!(tail.is_none() || tail.unwrap().is_err());
    let c = PeerConnection::connect(peer, None, Route::Relay, stop.clone()).await?;
    ensure!(c.request(Method::GET, "/v1/health", None).await?.status() == 401);
    stop.cancel();
    agent.await??;
    gw.await??;
    strict.await??;
    second.await??;
    core.await?;
    drop(body);
    tmp.close()?;
    Ok(())
}

#[tokio::test]
async fn desktop_requires_approval_and_relays_signals() -> Result<()> {
    timeout(Duration::from_secs(10), async {
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let base = format!("http://{}", listener.local_addr()?);
        let stop = CancellationToken::new();
        let key = token();
        let server = tokio::spawn(serve(
            listener,
            Arc::new(Relay::new(None, Some(key.clone()))?),
            None,
            stop.clone(),
        ));
        let client = reqwest::Client::new();
        let sessions = format!("{base}/desktop/v1/sessions");
        let r = client
            .post(&sessions)
            .bearer_auth(&key)
            .json(&json!({"video":{"width":800}}))
            .send()
            .await?;
        ensure!(r.status() == 201);
        let info: Value = r.json().await?;
        let id = info["sessionId"].as_str().unwrap();
        let host = info["hostToken"].as_str().unwrap();
        let viewer = info["viewerToken"].as_str().unwrap();
        let path = format!("/desktop/v1/sessions/{id}/signal");
        let url = base.parse()?;
        let signal = |role, secret| websocket(&client, &url, &path, role, Some(secret), 65536);
        let h = [("role", "host")];
        let v = [("role", "viewer")];
        let mut a = signal(&h, host).await?;
        ensure!(signal(&v, viewer).await.is_err());
        ensure!(
            client
                .post(format!("{sessions}/{id}/request"))
                .bearer_auth(viewer)
                .json(&json!({"label":"Guest"}))
                .send()
                .await?
                .status()
                == 200
        );
        ensure!(
            client
                .post(format!("{sessions}/{id}/approve"))
                .bearer_auth(host)
                .json(&json!({"allow":true}))
                .send()
                .await?
                .status()
                == 200
        );
        let mut b = signal(&v, viewer).await?;
        for ws in [&mut a, &mut b] {
            let m = ws.next().await.unwrap()?;
            ensure!(serde_json::from_slice::<Value>(&m.into_data())?["connected"] == true)
        }
        for kind in ["offer", "answer", "ice"] {
            let msg = json!({"type":kind,"sdp":"test"});
            let (from, to) = if kind == "answer" {
                (&mut b, &mut a)
            } else {
                (&mut a, &mut b)
            };
            from.send(Message::text(msg.to_string())).await?;
            let got = to.next().await.unwrap()?;
            ensure!(serde_json::from_slice::<Value>(&got.into_data())? == msg)
        }
        let turn_path = format!("/desktop/v1/sessions/{id}/turn");
        let mut turn = websocket(&client, &url, &turn_path, &[], Some(viewer), 1 << 20).await?;
        let mut binding = vec![0, 1, 0, 0, 0x21, 0x12, 0xa4, 0x42];
        binding.extend([7; 12]);
        turn.send(Message::binary(binding)).await?;
        let msg = turn.next().await.unwrap()?.into_data();
        ensure!(&msg[..2] == [1, 1] && msg[8..20] == [7; 12]);
        client
            .post(format!("{sessions}/{id}/approve"))
            .bearer_auth(host)
            .json(&json!({"allow":false}))
            .send()
            .await?
            .error_for_status()?;
        ensure!(signal(&v, viewer).await.is_err());
        ensure!(
            client
                .delete(format!("{sessions}/{id}"))
                .bearer_auth(host)
                .send()
                .await?
                .status()
                == 204
        );
        stop.cancel();
        server.await??;
        Ok::<_, anyhow::Error>(())
    })
    .await?
}
