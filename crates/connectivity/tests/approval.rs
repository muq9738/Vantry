#![cfg(unix)]
//! New-device approval (T3 Code's "Require approval for new devices") over a
//! real TLS gateway: the waiting device and the host see one verification
//! code, credentials go only to the approved device, once.
use anyhow::{Result, ensure};
use std::{sync::Arc, time::Duration};
use tempfile::TempDir;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, UnixListener},
    time::timeout,
};
use tokio_util::sync::CancellationToken;
use vantry_connectivity::{
    identity::{DeviceRequestPoll, IdentityStore},
    peer::{PeerConnection, Redemption, Route, poll_redemption, redeem_start, serve_gateway},
};

async fn health_core(listener: UnixListener, stop: CancellationToken) {
    loop {
        let mut socket = tokio::select! {
            _ = stop.cancelled() => break,
            r = listener.accept() => r.unwrap().0,
        };
        tokio::spawn(async move {
            let mut buf = vec![];
            while buf.len() < 32768 && !buf.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                if socket.read_exact(&mut byte).await.is_err() {
                    return;
                }
                buf.push(byte[0]);
            }
            let _ = socket
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 15\r\n\r\n{\"version\":\"x\"}")
                .await;
        });
    }
}

#[tokio::test]
async fn approval_gates_new_devices() -> Result<()> {
    timeout(Duration::from_secs(30), run()).await?
}

async fn run() -> Result<()> {
    let tmp = TempDir::new()?;
    let stop = CancellationToken::new();
    let sock = tmp.path().join("core.sock");
    let core = tokio::spawn(health_core(UnixListener::bind(&sock)?, stop.clone()));
    let host = Arc::new(IdentityStore::open(&tmp.path().join("host"), "Studio")?);
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let gw = tokio::spawn(serve_gateway(listener, host.clone(), sock, false, stop.clone()));
    host.set_routes(vec![addr.to_string()], vec![])?;
    host.set_require_approval(true)?;
    ensure!(host.require_approval());

    // A device redeems a valid code and waits with a verification code.
    let guest = IdentityStore::open(&tmp.path().join("guest"), "Laptop")?;
    let bootstrap = host.issue()?;
    let Redemption::Pending(pending) =
        redeem_start(&bootstrap, &guest, "Laptop", Route::Direct, stop.clone()).await?
    else {
        anyhow::bail!("expected the device to wait for approval");
    };
    let requests = host.device_requests();
    ensure!(requests.len() == 1, "one waiting device");
    ensure!(requests[0].verification_code == pending.verification_code);
    ensure!(requests[0].name == "Laptop");
    ensure!(pending.verification_code.len() == 9 && pending.verification_code.as_bytes()[4] == b'-');
    ensure!(host.snapshot().clients.is_empty(), "no credential before approval");

    // The one-time code is spent.
    ensure!(
        redeem_start(&bootstrap, &guest, "Again", Route::Direct, stop.clone())
            .await
            .is_err()
    );
    // Asking again keeps waiting; a wrong claim learns nothing.
    ensure!(matches!(
        poll_redemption(&pending, &guest, Route::Direct, stop.clone()).await?,
        Redemption::Pending(_)
    ));
    ensure!(host.poll_device_request(&pending.request_id, "wrong-claim-wrong-claim-wrong-claim") == DeviceRequestPoll::Gone);

    // Approved: the device collects its credential once and connects with it.
    ensure!(host.approve_device(&pending.request_id)?);
    ensure!(host.snapshot().clients.len() == 1);
    ensure!(host.device_requests().is_empty());
    let Redemption::Paired(peer_id) =
        poll_redemption(&pending, &guest, Route::Direct, stop.clone()).await?
    else {
        anyhow::bail!("expected the approved device to pair");
    };
    ensure!(matches!(
        poll_redemption(&pending, &guest, Route::Direct, stop.clone()).await?,
        Redemption::Expired
    ));
    let saved = guest.snapshot();
    ensure!(saved.peers.len() == 1 && saved.peers[0].id == peer_id);
    let client = PeerConnection::connect(&saved.peers[0].pair, None, Route::Direct, stop.clone()).await?;
    ensure!(client.health().await?["version"] == "x");

    // Denied: the second device stays out.
    let other = IdentityStore::open(&tmp.path().join("other"), "Tablet")?;
    let code = host.issue()?;
    let Redemption::Pending(waiting) =
        redeem_start(&code, &other, "Tablet", Route::Direct, stop.clone()).await?
    else {
        anyhow::bail!("expected a second waiting device");
    };
    ensure!(waiting.verification_code != pending.verification_code);
    ensure!(host.deny_device(&waiting.request_id));
    ensure!(!host.approve_device(&waiting.request_id)?, "a denied device cannot be approved later");
    ensure!(matches!(
        poll_redemption(&waiting, &other, Route::Direct, stop.clone()).await?,
        Redemption::Denied
    ));
    ensure!(other.snapshot().peers.is_empty());
    ensure!(host.snapshot().clients.len() == 1);

    // Without approval a code pairs at once, as before.
    host.set_require_approval(false)?;
    let third = IdentityStore::open(&tmp.path().join("third"), "Phone")?;
    let direct = host.issue()?;
    ensure!(matches!(
        redeem_start(&direct, &third, "Phone", Route::Direct, stop.clone()).await?,
        Redemption::Paired(_)
    ));
    ensure!(host.snapshot().clients.len() == 2);

    stop.cancel();
    let _ = gw.await;
    let _ = core.await;
    Ok(())
}
