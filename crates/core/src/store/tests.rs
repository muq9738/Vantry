use super::schema::DDLS;
use crate::{Core, ErrorCode};
use rusqlite::Connection;
use vantry_protocol::{Command, CommandKind};
#[test]
fn every_published_version_preserves_history_and_upgrades_idempotently() {
    for v in 1..=11 {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("state.sqlite");
        let db = Connection::open(&path).unwrap();
        for ddl in &DDLS[..v] {
            db.execute_batch(ddl).unwrap();
        }
        db.pragma_update(None, "user_version", v as i64).unwrap();
        db.execute_batch(
            "INSERT INTO threads(id,title,created_at) VALUES('old','旧会话',100); INSERT INTO
             command_receipts(command_id,kind,fingerprint,thread_id,sequence,accepted,created_at)
             VALUES('before','thread.create','','old',1,1,100)",
        )
        .unwrap();
        drop(db);
        for _ in 0..2 {
            let core = Core::open(dir.path()).unwrap();
            assert_eq!(core.snapshot().unwrap().threads.unwrap()[0].title, "旧会话");
            assert_eq!(
                core.dispatch(&Command::new("before", CommandKind::CreateThread))
                    .unwrap_err()
                    .code(),
                ErrorCode::Conflict
            );
            let db = Connection::open(&path).unwrap();
            assert_eq!(
                db.query_row("PRAGMA user_version", [], |r| r.get::<_, i64>(0))
                    .unwrap(),
                11
            );
            assert_eq!(
                db.query_row("PRAGMA integrity_check", [], |r| r.get::<_, String>(0))
                    .unwrap(),
                "ok"
            );
        }
    }
}
#[test]
fn divergent_v3_and_v4_branches_merge_existing_tables() {
    for branch in 0..3 {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("state.sqlite");
        let db = Connection::open(&path).unwrap();
        db.execute_batch(DDLS[0]).unwrap();
        db.execute_batch(DDLS[1]).unwrap();
        db.execute_batch(if branch == 0 {
            include_str!("../../tests/fixtures/macbookV3.sql")
        } else {
            include_str!("../../tests/fixtures/localV3.sql")
        })
        .unwrap();
        db.execute_batch(
            "INSERT INTO threads(id,title,created_at) VALUES('th','legacy',1); INSERT INTO runs(id,
             thread_id,attempt_id,active_attempt_id,status,input,created_at,updated_at) VALUES('run',
             'th','att','','completed','old',1,1); INSERT INTO attempts(id,run_id,ordinal,status,
             created_at,updated_at) VALUES('att','run',1,'completed',1,1)",
        )
        .unwrap();
        if branch == 0 {
            db.execute_batch(
                "INSERT INTO inputs(id,thread_id,kind,message,status,created_at,updated_at) VALUES('q',
             'th','queue','old','queued',1,1)",
            )
            .unwrap();
        } else {
            db.execute_batch(
                "INSERT INTO queued_messages(id,thread_id,message,status,created_at) VALUES('q','th',
             'old','pending',1)",
            )
            .unwrap();
        }
        if branch == 2 {
            db.execute_batch(include_str!("../../tests/fixtures/localV4.sql"))
                .unwrap();
            db.execute_batch(
                "INSERT INTO steering_messages VALUES('s','c','th','run','att','old','[]','sending','',
             '',2,2)",
            )
            .unwrap();
        }
        db.pragma_update(None, "user_version", if branch == 2 { 4 } else { 3 })
            .unwrap();
        drop(db);
        for _ in 0..2 {
            let core = Core::open(dir.path()).unwrap();
            let snap = core.snapshot().unwrap();
            assert_eq!(snap.inputs[0].status, "queued");
            assert_eq!(snap.inputs[0].position, 0);
            assert_eq!(snap.messages[0].status, "pending");
            if branch == 2 {
                assert_eq!(snap.inputs[1].status, "unknown");
            }
        }
    }
}
#[test]
fn backup_reopens_with_receipts_after_log_pruning() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::open(dir.path()).unwrap();
    assert!(Core::open(dir.path()).is_err());
    let c = Command {
        title: "<旧&新>".into(),
        ..Command::new("create", CommandKind::CreateThread)
    };
    let accepted = core.dispatch(&c).unwrap();
    core.prune_events(0).unwrap();
    assert!(core.events_after(0, 3).unwrap().is_empty());
    let copy = tempfile::tempdir().unwrap();
    core.backup_to(copy.path().join("state.sqlite")).unwrap();
    let restored = Core::open(copy.path()).unwrap();
    assert_eq!(restored.snapshot().unwrap(), core.snapshot().unwrap());
    assert_eq!(restored.dispatch(&c).unwrap(), accepted);
    let payload: String = Connection::open(copy.path().join("state.sqlite"))
        .unwrap()
        .query_row("SELECT payload FROM command_receipts", [], |r| r.get(0))
        .unwrap();
    assert_eq!(
        payload,
        r#"{"version":1,"commandId":"create","kind":"thread.create","title":"\u003c旧\u0026新\u003e"}"#
    );
    assert!(
        restored
            .dispatch(&Command::new("next", CommandKind::CreateThread))
            .unwrap()
            .sequence
            > accepted.sequence
    );
}
#[test]
fn writer_can_commit_during_consistent_backup() {
    let source = tempfile::tempdir().unwrap();
    let destination = tempfile::tempdir().unwrap();
    let core = Core::open(source.path()).unwrap();
    let c = core.clone();
    let (ready, started) = std::sync::mpsc::sync_channel(0);
    std::thread::scope(|scope| {
        let writer = scope.spawn(move || {
            for n in 0..60 {
                let cmd = Command {
                    title: "保留历史".repeat(512),
                    ..Command::new(format!("write-{n}"), CommandKind::CreateThread)
                };
                c.dispatch(&cmd).unwrap();
                if n == 1 {
                    ready.send(()).unwrap();
                }
            }
        });
        started.recv().unwrap();
        core.backup_to(destination.path().join("state.sqlite"))
            .unwrap();
        writer.join().unwrap();
    });
    let copy = Core::open(destination.path()).unwrap();
    let snap = copy.snapshot().unwrap();
    let count = snap.threads.unwrap().len() as i64;
    assert!((2..=60).contains(&count));
    assert_eq!(count, snap.sequence);
    let db = Connection::open(destination.path().join("state.sqlite")).unwrap();
    let receipts: i64 = db.query_row("SELECT COUNT(*) FROM command_receipts c JOIN events e ON e.sequence=c.sequence AND e.thread_id=c.thread_id", [], |r| r.get(0)).unwrap();
    assert_eq!(count, receipts);
}
#[test]
fn newer_schema_is_refused_before_wal() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("state.sqlite");
    let db = Connection::open(&path).unwrap();
    db.pragma_update(None, "user_version", 12).unwrap();
    assert!(Core::open(dir.path()).is_err());
    assert_eq!(
        db.query_row("PRAGMA journal_mode", [], |r| r.get::<_, String>(0))
            .unwrap(),
        "delete"
    );
}
