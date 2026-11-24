use anyhow::{Result, ensure};
use futures::{SinkExt, StreamExt};
use serde_json::{Value, json};
use std::{sync::Arc, time::Duration};
use tokio::{net::TcpListener, sync::watch, time::timeout};
use tokio_tungstenite::{accept_async, tungstenite::Message};
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    pairing::token,
    peer::{HostConfig, HostState},
    relay::{Credential, Relay, SessionConfig, Upstream},
    serve,
    transport::{HandshakeStatus, websocket},
};
#[tokio::test]
async fn http_upgrade_routes_and_scoped_registration() -> Result<()> {
    timeout(Duration::from_secs(6), run_upgrades()).await?
}
async fn run_upgrades() -> Result<()> {
    let backend = TcpListener::bind("127.0.0.1:0").await?;
    let addr = backend.local_addr()?;
    let upstream = tokio::spawn(async move {
        for _ in 0..2 {
            let mut ws = accept_async(backend.accept().await?.0).await?;
            let msg = ws.next().await.unwrap()?;
            ws.send(msg).await?;
        }
        Ok::<_, anyhow::Error>(())
    });
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let relay_addr = listener.local_addr()?;
    let route = token();
    let key = token();
    let cfg = SessionConfig {
        host_credentials: vec![Credential {
            key: key.clone(),
            routes: vec![route.clone()],
        }],
        routes: [("localhost".into(), route.clone())].into(),
        http_upstreams: vec![Upstream {
            host: "127.0.0.1".into(),
            prefix: "/legacy".into(),
            target: format!("http://{addr}"),
        }],
        ..Default::default()
    };
    let stop = CancellationToken::new();
    let server = tokio::spawn(serve(
        listener,
        Arc::new(Relay::new(Some(cfg), None)?),
        None,
        stop.clone(),
    ));
    let client = reqwest::Client::new();
    let base = format!("http://{relay_addr}");
    let err = websocket(
        &client,
        &base.parse()?,
        "/relay/host",
        &[("route", &token())],
        Some(&key),
        4096,
    )
    .await
    .err()
    .unwrap();
    ensure!(err.downcast_ref::<HandshakeStatus>().unwrap().0 == 401);
    let (tx, mut rx) = watch::channel(HostState::Connecting);
    let host = tokio::spawn(
        HostConfig {
            relay: base.clone(),
            route: route.clone(),
            key: key.clone(),
            instance: token(),
            gateway: addr,
            t3: Some(addr),
        }
        .run(tx, stop.clone()),
    );
    rx.wait_for(|s| *s == HostState::Online).await?;
    let err = websocket(
        &client,
        &base.parse()?,
        "/relay/host",
        &[("route", &route)],
        Some(&key),
        4096,
    )
    .await
    .err()
    .unwrap();
    ensure!(err.downcast_ref::<HandshakeStatus>().unwrap().0 == 409);
    for (url, path) in [
        (format!("http://localhost:{}", relay_addr.port()), "/ws"),
        (base.clone(), "/legacy/ws"),
    ] {
        let mut ws = websocket(&client, &url.parse()?, path, &[], None, 65536).await?;
        ws.send(Message::text("stream frame")).await?;
        ensure!(ws.next().await.unwrap()?.into_text()? == "stream frame");
    }
    upstream.await??;
    stop.cancel();
    host.await??;
    server.await??;
    Ok(())
}
#[tokio::test]
async fn public_route_reuses_its_http_tunnel() -> Result<()> {
    timeout(Duration::from_secs(5), async {
        use bytes::Bytes;
        use http_body_util::Full;
        use hyper_util::rt::TokioIo;
        use std::sync::atomic::{AtomicUsize, Ordering};
        let stop = CancellationToken::new();
        let local = TcpListener::bind("127.0.0.1:0").await?;
        let address = local.local_addr()?;
        let count = Arc::new(AtomicUsize::new(0));
        let accepted = count.clone();
        let done = stop.clone();
        let backend = tokio::spawn(async move {
            let mut workers = tokio::task::JoinSet::new();
            loop {
                let socket =
                    tokio::select! {_=done.cancelled()=>break,v=local.accept()=>v.unwrap().0};
                accepted.fetch_add(1, Ordering::Relaxed);
                workers.spawn(async move {
                    let service = hyper::service::service_fn(|_| async {
                        Ok::<_, std::convert::Infallible>(http::Response::new(Full::new(
                            Bytes::from_static(b"OK"),
                        )))
                    });
                    let _ = hyper::server::conn::http1::Builder::new()
                        .serve_connection(TokioIo::new(socket), service)
                        .await;
                });
            }
            workers.shutdown().await;
        });
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let base = format!("http://{}", listener.local_addr()?);
        let route = token();
        let key = token();
        let cfg = SessionConfig {
            registration_key: key.clone(),
            routes: [("vantry.test".into(), route.clone())].into(),
            ..Default::default()
        };
        let relay = tokio::spawn(serve(
            listener,
            Arc::new(Relay::new(Some(cfg), None)?),
            None,
            stop.clone(),
        ));
        let (tx, mut rx) = watch::channel(HostState::Connecting);
        let host = tokio::spawn(
            HostConfig {
                relay: base.clone(),
                route,
                key,
                instance: token(),
                gateway: address,
                t3: Some(address),
            }
            .run(tx, stop.clone()),
        );
        rx.wait_for(|s| *s == HostState::Online).await?;
        let client = reqwest::Client::new();
        for _ in 0..3 {
            ensure!(
                client
                    .get(format!("{base}/environment"))
                    .header("Host", "vantry.test")
                    .send()
                    .await?
                    .text()
                    .await?
                    == "OK"
            );
        }
        ensure!(count.load(Ordering::Relaxed) == 1);
        stop.cancel();
        host.await??;
        relay.await??;
        backend.await?;
        Ok::<_, anyhow::Error>(())
    })
    .await?
}
#[tokio::test]
async fn redeem_is_single_use_scoped_and_rate_limited() -> Result<()> {
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let base = format!("http://{}", listener.local_addr()?);
    let key = token();
    let scoped = token();
    let desktop = token();
    let cfg = SessionConfig {
        registration_key: key.clone(),
        host_credentials: vec![Credential {
            key: scoped.clone(),
            routes: vec![token()],
        }],
        ..Default::default()
    };
    let stop = CancellationToken::new();
    let relay = tokio::spawn(serve(
        listener,
        Arc::new(Relay::new(Some(cfg), Some(desktop.clone()))?),
        None,
        stop.clone(),
    ));
    let client = reqwest::Client::new();
    let request = json!({"scope":"session","payload":{"public":"routing"}});
    let url = format!("{base}/pairings");
    ensure!(
        client
            .post(&url)
            .bearer_auth(&desktop)
            .json(&request)
            .send()
            .await?
            .status()
            == 401
    );
    let response = client
        .post(&url)
        .bearer_auth(&scoped)
        .json(&request)
        .send()
        .await?;
    ensure!(response.status() == 201);
    let entry: Value = response.json().await?;
    ensure!(entry.get("payload").is_none());
    let id = entry["id"].as_str().unwrap();
    ensure!(
        client
            .get(&url)
            .query(&[("id", id)])
            .bearer_auth(&key)
            .send()
            .await?
            .status()
            == 404
    );
    let input = json!({"scope":"session","code":entry["code"],"name":"test"});
    ensure!(
        client
            .post(format!("{base}/desktop/pairings/redeem"))
            .json(&input)
            .send()
            .await?
            .status()
            == 400
    );
    let redeem = format!("{url}/redeem");
    let a = client.post(&redeem).json(&input);
    let b = client.post(&redeem).json(&input);
    let (a, b) = tokio::try_join!(a.send(), b.send())?;
    let mut statuses = [a.status().as_u16(), b.status().as_u16()];
    statuses.sort();
    ensure!(statuses == [200, 400]);
    let status: Value = client
        .get(&url)
        .bearer_auth(&scoped)
        .query(&[("id", id)])
        .send()
        .await?
        .json()
        .await?;
    ensure!(
        status["status"] == "paired"
            && status["pairedWith"] == "test"
            && status.get("payload").is_none()
    );
    for i in 2..21 {
        let r = client
            .post(&redeem)
            .header("X-Forwarded-For", format!("10.0.0.{i}"))
            .json(&input)
            .send()
            .await?;
        ensure!(r.status() == if i < 20 { 400 } else { 429 })
    }
    stop.cancel();
    relay.await??;
    Ok(())
}
