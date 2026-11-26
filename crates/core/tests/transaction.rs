use rusqlite::{Connection, params};
use std::{
    path::Path,
    sync::{Arc, Barrier},
};
use vantry_core::Core;
use vantry_protocol::{Command, CommandKind, ErrorCode};
#[test]
fn concurrent_retries_and_failed_transaction_leave_one_receipt() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::open(dir.path()).unwrap();
    let gate = Arc::new(Barrier::new(4));
    let command = Command::new("same", CommandKind::CreateThread);
    let receipts = std::thread::scope(|scope| {
        let jobs = (0..4)
            .map(|_| {
                let gate = gate.clone();
                let c = &core;
                let cmd = &command;
                scope.spawn(move || {
                    gate.wait();
                    c.dispatch(cmd).unwrap()
                })
            })
            .collect::<Vec<_>>();
        jobs.into_iter()
            .map(|job| job.join().unwrap())
            .collect::<Vec<_>>()
    });
    assert!(receipts.iter().all(|r| r == &receipts[0]));
    let db = Connection::open(dir.path().join("state.sqlite")).unwrap();
    let send = Command {
        thread_id: receipts[0].thread_id.clone(),
        message: "atomic".into(),
        ..Command::new("send", CommandKind::SendMessage)
    };
    db.execute_batch("CREATE TRIGGER fail_receipt BEFORE INSERT ON command_receipts BEGIN SELECT RAISE(ABORT,'test failpoint'); END").unwrap();
    assert!(core.dispatch(&send).is_err());
    for table in ["runs", "attempts", "effect_outbox"] {
        assert_eq!(
            db.query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |r| r
                .get::<_, i64>(0))
                .unwrap(),
            0
        );
    }
    assert_eq!(core.snapshot().unwrap().sequence, 1);
    db.execute_batch("DROP TRIGGER fail_receipt").unwrap();
    assert!(core.dispatch(&send).unwrap().accepted);
}
#[test]
fn completed_steer_fallback_and_stop_cancel_fifo() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::open(dir.path()).unwrap();
    let db = Connection::open(dir.path().join("state.sqlite")).unwrap();
    let th = core
        .dispatch(&Command::new("create", CommandKind::CreateThread))
        .unwrap()
        .thread_id;
    db.execute("INSERT INTO runs(id,thread_id,attempt_id,active_attempt_id,status,input,created_at,updated_at) VALUES('past',?,'old','','completed','old',1,2)", [&th]).unwrap();
    let steer = Command {
        thread_id: th.clone(),
        run_id: "past".into(),
        parent_attempt_id: "old".into(),
        message: "next".into(),
        ..Command::new("steer", CommandKind::SteerRun)
    };
    assert_eq!(
        core.dispatch(&steer).unwrap_err().code(),
        ErrorCode::Conflict
    );
    let next = core
        .dispatch(&Command {
            fallback_to_queue: true,
            ..steer
        })
        .unwrap();
    assert!(!next.run_id.is_empty());
    let queued = core
        .dispatch(&Command {
            thread_id: th.clone(),
            message: "must stay cancelled".into(),
            ..Command::new("queue", CommandKind::QueueMessage)
        })
        .unwrap();
    core.dispatch(&Command {
        run_id: next.run_id,
        ..Command::new("stop", CommandKind::StopRun)
    })
    .unwrap();
    let snap = core.snapshot().unwrap();
    assert!(
        snap.inputs
            .iter()
            .any(|i| i.id == queued.input_id && i.status == "cancelled")
    );
    assert_eq!(snap.runs.unwrap().len(), 2);
    core.drain_and_shutdown().unwrap();
}
#[test]
fn user_steer_queues_inactive_targets_and_preserves_strict_conflicts() {
    for status in [
        "completed",
        "cancelled",
        "failed",
        "interrupted",
        "queued",
        "waiting_children",
    ] {
        let dir = tempfile::tempdir().unwrap();
        let core = Core::open(dir.path()).unwrap();
        let thread = core
            .dispatch(&Command::new("create", CommandKind::CreateThread))
            .unwrap()
            .thread_id;
        let db = Connection::open(dir.path().join("state.sqlite")).unwrap();
        db.execute("INSERT INTO runs(id,thread_id,attempt_id,active_attempt_id,status,input,created_at,updated_at) VALUES('target',?,'old','',?,'test',1,2)", params![thread, status]).unwrap();
        let steer = Command {
            thread_id: thread,
            run_id: "target".into(),
            message: "next".into(),
            ..Command::new("user", CommandKind::SteerMessage)
        };
        let anchored = Command {
            parent_attempt_id: "old".into(),
            ..steer.clone()
        };
        let inter = Command {
            kind: CommandKind::InterThreadSend,
            delivery_mode: "steer".into(),
            ..steer.clone()
        };
        for strict in [anchored, inter] {
            assert_eq!(
                core.dispatch(&strict).unwrap_err().code(),
                ErrorCode::Conflict,
                "{status}"
            );
        }
        let receipt = core.dispatch(&steer).unwrap();
        if matches!(status, "queued" | "waiting_children") {
            assert!(receipt.run_id.is_empty());
            assert!(
                core.snapshot()
                    .unwrap()
                    .inputs
                    .iter()
                    .any(|i| i.id == receipt.input_id && i.is_queued())
            );
        } else {
            assert!(!receipt.run_id.is_empty());
            assert_ne!(receipt.run_id, "target");
            db.execute(
                "UPDATE runs SET status='running' WHERE id=?",
                [&receipt.run_id],
            )
            .unwrap();
            let redirected = core
                .dispatch(&Command {
                    command_id: "next".into(),
                    ..steer
                })
                .unwrap();
            assert_eq!(redirected.run_id, receipt.run_id);
            assert!(
                core.snapshot()
                    .unwrap()
                    .inputs
                    .iter()
                    .any(|i| i.id == redirected.input_id && i.kind == "steer")
            );
        }
    }
}

#[test]
fn session_fork_keeps_active_branch_tool_result_and_rewind_discards_tail() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::open(dir.path()).unwrap();
    let thread = core
        .dispatch(&Command::new("create", CommandKind::CreateThread))
        .unwrap()
        .thread_id;
    let db = Connection::open(dir.path().join("state.sqlite")).unwrap();
    for (n, key) in [(1, "first"), (2, "later")] {
        db.execute("INSERT INTO runs(id,thread_id,attempt_id,active_attempt_id,status,input,created_at,updated_at) VALUES(?,?,'att','','completed','history',?,?)", params![key, thread, n, n]).unwrap();
    }
    let sessions = dir.path().join("sessions");
    std::fs::create_dir(&sessions).unwrap();
    let path = sessions.join(format!("{thread}.jsonl"));
    let raw = r#"{"type":"session","version":3,"id":"source"}
{"id":"user1","type":"message","message":{"role":"user"}}
{"id":"discarded","type":"message","parentId":"user1","message":{"role":"assistant"}}
{"id":"retained","type":"message","parentId":"user1","message":{"role":"assistant"}}
{"id":"tool","type":"message","parentId":"retained","message":{"role":"toolResult"}}
{"id":"next","type":"message","parentId":"tool","message":{"role":"user"}}
{"id":"reply","type":"message","parentId":"next","message":{"role":"assistant"}}
"#;
    std::fs::write(&path, raw).unwrap();
    let clone = core
        .dispatch(&Command {
            thread_id: thread.clone(),
            source_run_id: "first".into(),
            ..Command::new("fork", CommandKind::ForkThread)
        })
        .unwrap();
    fn verify(path: &Path) {
        let raw = std::fs::read_to_string(path).unwrap();
        assert!(raw.contains("toolResult"));
        assert!(!raw.contains("discarded"));
        assert!(!raw.contains("\"next\""));
        assert_eq!(raw.lines().count(), 4);
    }
    verify(&sessions.join(format!("{}.jsonl", clone.thread_id)));
    core.dispatch(&Command {
        thread_id: thread.clone(),
        run_id: "later".into(),
        ..Command::new("rewind", CommandKind::RewindThread)
    })
    .unwrap();
    verify(&path);
    core.dispatch(&Command {
        thread_id: thread,
        run_id: "first".into(),
        ..Command::new("clear", CommandKind::RewindThread)
    })
    .unwrap();
    assert!(!path.exists());
}
