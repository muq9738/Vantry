use super::schema::DDLS;
use crate::{Core, ErrorCode};
use rusqlite::Connection;
use vantry_protocol::{Command, CommandKind, ThreadDetailQuery};

#[test]
fn thread_detail_returns_requested_send_receipts_only_for_visible_runs() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::open(dir.path()).unwrap();
    for thread in ["one", "two"] {
        let mut create = Command::new(format!("create/{thread}"), CommandKind::CreateThread);
        create.thread_id = thread.into();
        core.dispatch(&create).unwrap();
    }
    let send = |thread: &str, command_id: &str| {
        let mut cmd = Command::new(command_id, CommandKind::SendMessage);
        cmd.thread_id = thread.into();
        cmd.message = "test".into();
        let run_id = core.dispatch(&cmd).unwrap().run_id;
        let mut stop = Command::new(format!("{command_id}/stop"), CommandKind::StopRun);
        stop.run_id = run_id.clone();
        core.dispatch(&stop).unwrap();
        run_id
    };
    let first = send("one", "client/a/first");
    send("one", "client/b/other");
    send("two", "client/a/elsewhere");
    let last = send("one", "client/a/last");
    assert!(
        core.thread_detail("one", 0)
            .unwrap()
            .send_command_ids
            .is_empty()
    );
    let query = |limit_runs| {
        core.query_thread_detail(&ThreadDetailQuery {
            thread_id: "one".into(),
            limit_runs,
            command_prefix: "client/a/".into(),
        })
        .unwrap()
        .send_command_ids
    };
    assert_eq!(query(1), [(last.clone(), "client/a/last".into())].into());
    assert_eq!(
        query(0),
        [
            (first, "client/a/first".into()),
            (last, "client/a/last".into()),
        ]
        .into()
    );
}

#[test]
fn every_published_version_preserves_history_and_upgrades_idempotently() {
    for v in 1..=12 {
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
                12
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
    db.pragma_update(None, "user_version", 13).unwrap();
    assert!(Core::open(dir.path()).is_err());
    assert_eq!(
        db.query_row("PRAGMA journal_mode", [], |r| r.get::<_, String>(0))
            .unwrap(),
        "delete"
    );
}
#[test]
fn upgrading_v11_backfills_historical_run_blocks() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("state.sqlite");
    let db = Connection::open(&path).unwrap();
    for ddl in &DDLS[..11] {
        db.execute_batch(ddl).unwrap();
    }
    db.pragma_update(None, "user_version", 11i64).unwrap();
    db.execute_batch(
        "INSERT INTO threads(id,title,created_at) VALUES('th1','历史会话',10);
         INSERT INTO runs(id,thread_id,attempt_id,active_attempt_id,status,input,output,reasoning,error,created_at,updated_at)
           VALUES('run1','th1','att1','','completed','问问题','前半段后半段','历史思考','',11,20);
         INSERT INTO tool_calls(id,thread_id,run_id,attempt_id,name,arguments,output,status,updated_at,output_offset)
           VALUES('tc1','th1','run1','att1','read','{}','ok','completed',15,9);
         INSERT INTO inputs(id,thread_id,run_id,attempt_id,kind,message,attachments,command_id,status,disposition,error,created_at,updated_at,queue_order)
           VALUES('st1','th1','run1','att1','steer','中途引导','[]','cmd1','delivered','','',16,17,0);",
    )
    .unwrap();
    drop(db);

    let core = Core::open(dir.path()).unwrap();
    let snap = core.snapshot().unwrap();
    let summary: Vec<(vantry_protocol::RunBlockKind, String)> = snap
        .blocks
        .iter()
        .filter(|b| b.run_id == "run1")
        .map(|b| (b.kind, b.text.clone()))
        .collect();
    assert_eq!(
        summary,
        vec![
            (vantry_protocol::RunBlockKind::User, "问问题".into()),
            (vantry_protocol::RunBlockKind::Reasoning, "历史思考".into()),
            (vantry_protocol::RunBlockKind::Text, "前半段".into()),
            (vantry_protocol::RunBlockKind::Tool, "".into()),
            (vantry_protocol::RunBlockKind::Text, "后半段".into()),
            (vantry_protocol::RunBlockKind::Steer, "中途引导".into()),
        ]
    );
}
