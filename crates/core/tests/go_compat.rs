//! Set VANTRY_GO_BASELINE to a v0.1.42 executable to exercise both directions of upgrade.
#![cfg(unix)]
use bytes::Bytes;
use http_body_util::{BodyExt, Full};
use hyper_util::rt::TokioIo;
use std::{
    os::unix::fs::PermissionsExt,
    path::Path,
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};
use vantry_core::Core;
use vantry_protocol::*;
struct Baseline(Child);
impl Drop for Baseline {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
impl Baseline {
    async fn start(exe: &Path, root: &Path, dir: &Path) -> Self {
        let mut child = Self(
            Command::new(exe)
                .args(["core", "--provider", "controlled", "--data-dir"])
                .arg(dir)
                .env("HOME", root)
                .env("PI_CODING_AGENT_DIR", root.join("pi"))
                .env("VANTRY_DATA_DIR", dir)
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .unwrap(),
        );
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            assert!(
                child.0.try_wait().unwrap().is_none(),
                "Go Core exited during startup"
            );
            if tokio::net::UnixStream::connect(dir.join("core.sock"))
                .await
                .is_ok()
            {
                return child;
            }
            assert!(Instant::now() < deadline, "Go Core listener did not start");
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }
}
async fn request<T: serde::de::DeserializeOwned>(
    dir: &Path,
    cmd: Option<&vantry_protocol::Command>,
) -> T {
    let stream = tokio::net::UnixStream::connect(dir.join("core.sock"))
        .await
        .unwrap();
    let (mut tx, conn) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
        .await
        .unwrap();
    let handle = tokio::spawn(conn);
    let req = http::Request::builder()
        .method(if cmd.is_some() { "POST" } else { "GET" })
        .uri(if cmd.is_some() {
            "/v1/commands"
        } else {
            "/v1/snapshot"
        })
        .header("host", "localhost")
        .header("connection", "close")
        .body(Full::new(Bytes::from(
            cmd.map(|c| serde_json::to_vec(c).unwrap())
                .unwrap_or_default(),
        )))
        .unwrap();
    let res = tx.send_request(req).await.unwrap();
    assert_eq!(res.status(), 200);
    let data = res.into_body().collect().await.unwrap().to_bytes();
    handle.await.unwrap().unwrap();
    serde_json::from_slice(&data).unwrap()
}
fn schema_contract(directory: &Path) -> Vec<(String, String, String, i64, i64, String)> {
    let db = rusqlite::Connection::open_with_flags(
        directory.join("state.sqlite"),
        rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
    )
    .unwrap();
    let sql = "SELECT s.name,c.name,c.type,c.\"notnull\",c.pk,COALESCE(c.dflt_value,'')
        FROM sqlite_master s JOIN pragma_table_info(s.name) c
        WHERE s.type='table' AND s.name NOT GLOB 'sqlite_*' AND s.name NOT GLOB '*_search_*'
        UNION ALL SELECT s.tbl_name,s.name,COALESCE(c.name,''),i.\"unique\",i.partial,CAST(c.seqno AS TEXT)
        FROM sqlite_master s JOIN pragma_index_list(s.tbl_name) i ON i.name=s.name
        JOIN pragma_index_info(s.name) c WHERE s.type='index' AND s.tbl_name NOT GLOB '*_search_*'
        ORDER BY 1,2,3,4,5,6";
    db.prepare(sql)
        .unwrap()
        .query_map([], |r| {
            Ok((
                r.get(0)?,
                r.get(1)?,
                r.get(2)?,
                r.get(3)?,
                r.get(4)?,
                r.get(5)?,
            ))
        })
        .unwrap()
        .collect::<rusqlite::Result<_>>()
        .unwrap()
}
#[tokio::test]
async fn released_core_reads_rust_projections_and_replays_receipts() {
    let Some(exe) = std::env::var_os("VANTRY_GO_BASELINE") else {
        return;
    };
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");
    std::fs::create_dir(&data).unwrap();
    std::fs::set_permissions(&data, std::fs::Permissions::from_mode(0o700)).unwrap();
    let go = Baseline::start(Path::new(&exe), dir.path(), &data).await;
    let old = vantry_protocol::Command {
        title: "Go <&>".into(),
        provider_id: "controlled".into(),
        ..vantry_protocol::Command::new("go-create", CommandKind::CreateThread)
    };
    let old_receipt: Receipt = request(&data, Some(&old)).await;
    assert!(
        Core::open(&data).is_err(),
        "Go and Rust must exclude each other via core.lock"
    );
    let original: Snapshot = request(&data, None).await;
    drop(go);
    let empty = dir.path().join("fresh");
    drop(Core::open(&empty).unwrap());
    assert_eq!(schema_contract(&data), schema_contract(&empty));
    let core = Core::open(&data).unwrap();
    assert_eq!(core.snapshot().unwrap(), original);
    assert_eq!(core.dispatch(&old).unwrap(), old_receipt);
    let new = vantry_protocol::Command {
        title: "Rust <&>".into(),
        provider_id: "controlled".into(),
        workspace: Some(WorkspaceSpec {
            kind: "directory".into(),
            path: data.to_string_lossy().into(),
            ..Default::default()
        }),
        ..vantry_protocol::Command::new("rust-create", CommandKind::CreateThread)
    };
    let accepted = core.dispatch(&new).unwrap();
    let new_send = vantry_protocol::Command {
        thread_id: accepted.thread_id.clone(),
        message: "离线事务验收 <&>".into(),
        ..vantry_protocol::Command::new("rust-send", CommandKind::SendMessage)
    };
    let sent = core.dispatch(&new_send).unwrap();
    let queue = vantry_protocol::Command {
        thread_id: accepted.thread_id.clone(),
        message: "队列".into(),
        ..vantry_protocol::Command::new("rust-queue", CommandKind::QueueMessage)
    };
    let queued = core.dispatch(&queue).unwrap();
    let stop = vantry_protocol::Command {
        run_id: sent.run_id.clone(),
        ..vantry_protocol::Command::new("rust-stop", CommandKind::StopRun)
    };
    let stopped = core.dispatch(&stop).unwrap();
    let expected = core.snapshot().unwrap();
    drop(core);
    let go = Baseline::start(Path::new(&exe), dir.path(), &data).await;
    let replay: Receipt = request(&data, Some(&new)).await;
    assert_eq!(accepted, replay);
    for (cmd, receipt) in [(&new_send, sent), (&queue, queued), (&stop, stopped)] {
        let replay: Receipt = request(&data, Some(cmd)).await;
        assert_eq!(replay, receipt);
    }
    let actual: Snapshot = request(&data, None).await;
    assert_eq!(actual, expected);
    drop(go);
}
