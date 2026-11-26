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
                        "/repos/test/app/releases?per_page=100" => &metadata,
                        "/delta" => &b"bad"[..],
                        "/zip" => &zip,
                        _ => anyhow::bail!("unexpected request: {path}"),
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
            "/repos/test/app/releases?per_page=100",
            "/repos/test/app/releases?per_page=100",
            "/delta",
            "/zip"
        ]
    );
    m.apply(&Default::default())?;
    assert_eq!(*state.borrow(), UpdateState::Idle);
    assert_eq!(fs::read(target.join("new"))?, b"updated");
    Ok(())
}

#[tokio::test]
async fn multi_version_delta_chain_and_live_progress() -> Result<()> {
    let root = tempfile::tempdir()?;
    let v1 = root.path().join("v1");
    let v2 = root.path().join("v2");
    let v3 = root.path().join("v3");
    let target = root.path().join("installed");
    for (dir, body, extra) in [
        (&v1, b"version-1-payload".as_slice(), None),
        (&v2, b"version-2-payload-improved".as_slice(), Some(("mid", b"v2-note".as_slice()))),
        (&v3, b"version-3-payload-final-release".as_slice(), Some(("final", b"v3-note".as_slice()))),
    ] {
        fs::create_dir_all(dir)?;
        fs::write(dir.join("app.bin"), body)?;
        if let Some((name, content)) = extra {
            fs::write(dir.join(name), content)?;
        }
    }
    fs::create_dir_all(&target)?;
    fs::write(target.join("app.bin"), b"version-1-payload")?;

    let mut d12 = Vec::new();
    write_delta(&mut d12, "0.2.10", "0.2.11", &v1, &v2)?;
    let mut d23 = Vec::new();
    write_delta(&mut d23, "0.2.11", "0.2.12", &v2, &v3)?;

    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let base = format!("http://{}", listener.local_addr()?);
    let full_size = (d12.len() + d23.len() + 50_000) as u64;
    let metadata = serde_json::to_vec(&serde_json::json!([
        {
            "tag_name": "v0.2.12",
            "body": "- Final performance fix",
            "assets": [
                {
                    "name": "vantry-0.2.11-to-0.2.12-linux-amd64.delta",
                    "size": d23.len(),
                    "browser_download_url": format!("{base}/d23")
                },
                {
                    "name": "vantry-0.2.12-linux-amd64.zip",
                    "size": full_size,
                    "browser_download_url": format!("{base}/full.zip")
                }
            ]
        },
        {
            "tag_name": "v0.2.11",
            "body": "- Intermediate delta release",
            "assets": [
                {
                    "name": "vantry-0.2.10-to-0.2.11-linux-amd64.delta",
                    "size": d12.len(),
                    "browser_download_url": format!("{base}/d12")
                }
            ]
        },
        {
            "tag_name": "v0.2.10",
            "body": "- Base version",
            "assets": []
        }
    ]))?;

    let d12_clone = d12.clone();
    let d23_clone = d23.clone();
    let server = tokio::spawn(async move {
        for _ in 0..3 {
            let (mut socket, _) = listener.accept().await?;
            let mut req = Vec::new();
            while !req.ends_with(b"\r\n\r\n") {
                req.push(socket.read_u8().await?);
            }
            let text = String::from_utf8(req)?;
            let path = text.split_whitespace().nth(1).unwrap();
            let body: &[u8] = match path {
                "/repos/test/app/releases?per_page=100" => &metadata,
                "/d12" => &d12_clone,
                "/d23" => &d23_clone,
                other => anyhow::bail!("unexpected path: {other}"),
            };
            socket
                .write_all(
                    format!(
                        "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
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
        local_token: None,
        bundled_token: None,
        target: Some(target.clone()),
        current_version: "0.2.10".into(),
        platform: "linux-amd64".into(),
        ..Default::default()
    })?;
    let info_rx = m.info_rx();

    let checked = m.check().await?;
    assert!(checked.available);
    assert_eq!(checked.latest_version, "0.2.12");
    assert!(checked.delta_available);
    assert_eq!(checked.delta_steps, 2);
    assert_eq!(checked.delta_size as usize, d12.len() + d23.len());
    assert!(checked.notes.contains("Final performance fix"));
    assert!(checked.notes.contains("Intermediate delta release"));
    assert!(!checked.notes.contains("Base version"));

    let mut progress_ticks = 0usize;
    let plan = m.download_and_stage(|done, total| {
        assert!(done <= total);
        progress_ticks += 1;
    })
    .await?;
    assert!(plan.used_delta);
    assert!(progress_ticks > 0);
    assert_eq!(info_rx.borrow().phase, "ready");
    assert!(info_rx.borrow().staged);

    server.await??;

    m.apply(&Default::default())?;
    assert_eq!(
        fs::read(target.join("app.bin"))?,
        b"version-3-payload-final-release"
    );
    assert_eq!(fs::read(target.join("final"))?, b"v3-note");
    assert!(!target.join("mid").exists());
    Ok(())
}
