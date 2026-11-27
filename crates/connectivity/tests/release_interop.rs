//! Set VANTRY_GO_BASELINE to a v0.1.42 Vantry binary to exercise the release client and relay.
#![cfg(unix)]
use anyhow::{Result, anyhow, ensure};
use serde_json::{Value, json};
use std::{
    io::{BufRead, BufReader},
    path::Path,
    process::{Child, Command, Stdio},
    sync::Arc,
    time::Duration,
};
use tempfile::TempDir;
use tokio::{
    net::{TcpListener, UnixStream},
    sync::watch,
    time::timeout,
};
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    bootstrap::Publisher,
    identity::IdentityStore,
    pairing::token,
    peer::{HostConfig, HostState, serve_gateway},
};
struct ChildProcess(Child);
impl Drop for ChildProcess {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
fn process(binary: &Path, dir: &Path) -> Command {
    let mut cmd = Command::new(binary);
    cmd.current_dir(dir)
        .env_clear()
        .env("HOME", dir)
        .env("VANTRY_DATA_DIR", dir)
        .env("TMPDIR", dir)
        .env("PATH", "/usr/bin:/bin")
        .env("MYGO_AUTOMATION", "1")
        .env("PI_CODING_AGENT_DIR", dir.join("pi"))
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    cmd
}
async fn action(socket: &Path, value: Value) -> Result<Value> {
    use bytes::Bytes;
    use http_body_util::{BodyExt, Full};
    let io = UnixStream::connect(socket).await?;
    let (mut tx, conn) =
        hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(io)).await?;
    let driver = tokio::spawn(conn);
    let req = http::Request::post("/v1/workbench")
        .header("host", "localhost")
        .header("content-type", "application/json")
        .header("connection", "close")
        .body(Full::new(Bytes::from(value.to_string())))?;
    let r = tx.send_request(req).await?;
    ensure!(
        r.status() == 200,
        "baseline workbench request returned {}",
        r.status()
    );
    let result: Value = serde_json::from_slice(&r.into_body().collect().await?.to_bytes())?;
    driver.await??;
    Ok(serde_json::from_str(
        result["output"]
            .as_str()
            .ok_or_else(|| anyhow!("missing workbench output"))?,
    )?)
}
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn released_peer_redeems_rust_code_through_released_relay() -> Result<()> {
    let Some(binary) = std::env::var_os("VANTRY_GO_BASELINE") else {
        return Ok(());
    };
    timeout(Duration::from_secs(25), interop(Path::new(&binary))).await?
}
async fn interop(binary: &Path) -> Result<()> {
    let dir = TempDir::new()?;
    let path = dir.path().join("relay.json");
    let key = token();
    std::fs::write(
        &path,
        serde_json::to_vec(&json!({"listen":"127.0.0.1:0","sessions":{"registrationKey":key}}))?,
    )?;
    let mut relay = ChildProcess(
        process(binary, dir.path())
            .args(["relay", "serve", "--config"])
            .arg(&path)
            .stderr(Stdio::piped())
            .spawn()?,
    );
    let stderr = relay.0.stderr.take().unwrap();
    let reader = tokio::task::spawn_blocking(move || {
        let mut first = String::new();
        BufReader::new(stderr).read_line(&mut first)?;
        Ok::<_, std::io::Error>(first)
    });
    let first = timeout(Duration::from_secs(5), reader).await???;
    let address = first
        .split_whitespace()
        .nth(4)
        .ok_or_else(|| anyhow!("baseline relay failed to start"))?
        .parse::<std::net::SocketAddr>()?;
    ensure!(address.ip().is_loopback());
    let relay_url = format!("http://{address}");
    let core_dir = dir.path().join("go");
    use std::os::unix::fs::DirBuilderExt;
    std::fs::DirBuilder::new().mode(0o700).create(&core_dir)?;
    let mut core = ChildProcess(
        process(binary, &core_dir)
            .args(["core", "--provider", "controlled", "--data-dir"])
            .arg(&core_dir)
            .spawn()?,
    );
    let socket = core_dir.join("core.sock");
    while !socket.exists() {
        ensure!(
            core.0.try_wait()?.is_none(),
            "baseline Core failed to start"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let stop = CancellationToken::new();
    let host = Arc::new(IdentityStore::open(&dir.path().join("rust"), "Rust host")?);
    host.set_routes(vec![], vec![relay_url.clone()])?;
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let gateway = listener.local_addr()?;
    let server = tokio::spawn(serve_gateway(
        listener,
        host.clone(),
        socket.clone(),
        false,
        stop.clone(),
    ));
    let (cfg, mut state) = watch::channel(HostState::Connecting);
    let host_task = tokio::spawn(
        HostConfig {
            relay: relay_url.clone(),
            key: key.clone(),
            route: host.snapshot().host.route,
            instance: token(),
            gateway,
            http_backend: None,
        }
        .run(cfg, stop.clone()),
    );
    state.wait_for(|s| *s == HostState::Online).await?;
    let publisher = Publisher::new(host.clone())?;
    let pair = publisher.issue(&key).await?;
    let status = action(
        &socket,
        json!({"action":"access.pair","message":pair.code,"name":relay_url}),
    )
    .await?;
    let id = status["peers"][0]["id"]
        .as_str()
        .ok_or_else(|| anyhow!("missing paired device"))?;
    let health = action(
        &socket,
        json!({"action":"access.check","name":id,"message":"relay"}),
    )
    .await?;
    ensure!(
        health["peers"][0]["reachable"] == true,
        "baseline peer health failed"
    );
    ensure!(health["peers"][0]["mode"] == "relay");
    drop(relay);
    state.wait_for(|s| *s != HostState::Online).await?;
    let listener = TcpListener::bind(address).await?;
    let relay_config = vantry_connectivity::relay::SessionConfig {
        registration_key: key,
        ..Default::default()
    };
    let relay = Arc::new(vantry_connectivity::relay::Relay::new(
        Some(relay_config),
        None,
    )?);
    let replacement = tokio::spawn(vantry_connectivity::serve(
        listener,
        relay,
        None,
        stop.clone(),
    ));
    state.wait_for(|s| *s == HostState::Online).await?;
    let health = action(
        &socket,
        json!({"action":"access.check","name":id,"message":"relay"}),
    )
    .await?;
    ensure!(health["peers"][0]["reachable"] == true && health["peers"][0]["mode"] == "relay");
    let client = host.snapshot().clients[0].id.clone();
    host.revoke_client(&client)?;
    let status = action(
        &socket,
        json!({"action":"access.check","name":id,"message":"relay"}),
    )
    .await;
    ensure!(status.is_err() || status?["peers"][0]["reachable"] == false);
    stop.cancel();
    host_task.await??;
    server.await??;
    replacement.await??;
    drop(core);
    dir.close()?;
    Ok(())
}
