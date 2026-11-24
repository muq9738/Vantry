use anyhow::Result;
use std::{
    fs,
    io::{Cursor, Write},
    sync::{Arc, Mutex},
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
};
use vantry_updater::*;
#[tokio::test]
async fn rejected_token_delta_fallback_reuse_and_apply() -> Result<()> {
    let root = tempfile::tempdir()?;
    let target = root.path().join("target");
    fs::create_dir(&target)?;
    fs::write(target.join("old"), "old")?;
    let local = root.path().join("local");
    fs::write(&local, "fixture-update")?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&local, fs::Permissions::from_mode(0o600))?;
    }
    let mut zip = zip::ZipWriter::new(Cursor::new(Vec::new()));
    zip.start_file("release/new", zip::write::SimpleFileOptions::default())?;
    zip.write_all(b"updated")?;
    let zip = zip.finish()?.into_inner();
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let base = format!("http://{}", listener.local_addr()?);
    let metadata = serde_json::to_vec(
        &serde_json::json!({"tag_name":"v2","assets":[{"name":"vantry-1-to-2-linux-amd64.delta","size":3,"browser_download_url":format!("{base}/delta")},{"name":"vantry-2-linux-amd64.zip","size":zip.len(),"browser_download_url":format!("{base}/zip")}]}),
    )?;
    let hits = Arc::new(Mutex::new(Vec::new()));
    let observed = hits.clone();
    let server = tokio::spawn(async move {
        for _ in 0..4 {
            let (mut socket, _) = listener.accept().await?;
            let mut req = Vec::new();
            while !req.ends_with(b"\r\n\r\n") {
                req.push(socket.read_u8().await?);
            }
            let text = String::from_utf8(req)?.to_lowercase();
            let path = text.split_whitespace().nth(1).unwrap();
            observed.lock().unwrap().push(path.to_owned());
            let (status, body) = if text.contains("authorization: bearer fixture-update") {
                ("403 Forbidden", &b""[..])
            } else {
                (
                    "200 OK",
                    match path {
                        "/repos/test/app/releases/latest" => &metadata,
                        "/delta" => &b"bad"[..],
                        "/zip" => &zip,
                        _ => anyhow::bail!("unexpected request"),
                    },
                )
            };
            socket
                .write_all(
                    format!(
                        "HTTP/1.1 {status}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    )
                    .as_bytes(),
                )
                .await?;
            socket.write_all(body).await?;
        }
        Ok::<_, anyhow::Error>(())
    });
    let mut m = UpdateManager::new(UpdateOptions {
        source: ReleaseSource::GitHub {
            api_base: base,
            repo: "test/app".into(),
        },
        local_token: Some(local),
        bundled_token: None,
        target: Some(target.clone()),
        current_version: "1".into(),
        platform: "linux-amd64".into(),
        ..Default::default()
    })?;
    let state = m.subscribe();
    assert_eq!(*state.borrow(), UpdateState::Idle);
    assert!(m.check().await?.delta_available);
    assert_eq!(*state.borrow(), UpdateState::Available);
    assert!(!m.download_and_stage(|_, _| {}).await?.used_delta);
    assert!(target.join("old").exists());
    assert_eq!(*state.borrow(), UpdateState::ReadyToInstall);
    assert!(m.current_info().staged);
    let staged = m.current_info().staged_path;
    assert_eq!(
        m.download_and_stage(|_, _| {}).await?.staged().to_str(),
        Some(staged.as_str())
    );
    server.await??;
    assert_eq!(
        &hits.lock().unwrap()[..],
        [
            "/repos/test/app/releases/latest",
            "/repos/test/app/releases/latest",
            "/delta",
            "/zip"
        ]
    );
    m.apply(&Default::default())?;
    assert_eq!(*state.borrow(), UpdateState::Idle);
    assert_eq!(fs::read(target.join("new"))?, b"updated");
    Ok(())
}
