//! Optional read-only migration smoke test on an operator-selected existing backup.
use vantry_core::Core;
use vantry_protocol::{Command, CommandKind};
#[test]
fn archived_sqlite_snapshot_opens_on_a_private_copy() {
    let Some(source) = std::env::var_os("VANTRY_ARCHIVED_DB") else {
        return;
    };
    let root = tempfile::tempdir().unwrap();
    let copy = root.path().join("state.sqlite");
    let archived = std::path::PathBuf::from(source);
    let mut wal = archived.as_os_str().to_os_string();
    wal.push("-wal");
    assert!(
        !std::path::Path::new(&wal).exists(),
        "select a standalone offline backup"
    );
    std::fs::copy(archived, &copy).unwrap();
    let db = rusqlite::Connection::open(&copy).unwrap();
    let expected: i64 = db
        .query_row("SELECT count(*) FROM threads", [], |r| r.get(0))
        .unwrap();
    let core = Core::with_retention(root.path(), -1).unwrap();
    let snap = core.snapshot().unwrap();
    assert_eq!(snap.threads.unwrap().len() as i64, expected);
    let accepted = core
        .dispatch(&Command::new(
            uuid::Uuid::new_v4().to_string(),
            CommandKind::CreateThread,
        ))
        .unwrap();
    assert!(accepted.sequence > snap.sequence);
    assert!(core.events_after(snap.sequence, 1).unwrap()[0].sequence == accepted.sequence);
    drop(core);
    let core = Core::open(root.path()).unwrap();
    assert_eq!(
        core.snapshot().unwrap().threads.unwrap().len() as i64,
        expected + 1
    );
    assert_eq!(
        db.query_row("PRAGMA integrity_check", [], |r| r.get::<_, String>(0))
            .unwrap(),
        "ok"
    );
}
