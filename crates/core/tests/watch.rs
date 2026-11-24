use vantry_core::Core;
use vantry_protocol::{Command, CommandKind, RunStatus};

#[test]
fn register_watch_delivers_on_terminal_run_and_immediately_if_already_done() {
    let dir = tempfile::tempdir().unwrap();
    let core = Core::open(dir.path().join("data")).unwrap();

    let watcher = core
        .dispatch(&Command {
            thread_id: "watcher".into(),
            title: "Watcher".into(),
            provider_id: "controlled".into(),
            ..Command::new("c1", CommandKind::CreateThread)
        })
        .unwrap();
    let target = core
        .dispatch(&Command {
            thread_id: "target".into(),
            title: "Target".into(),
            provider_id: "controlled".into(),
            ..Command::new("c2", CommandKind::CreateThread)
        })
        .unwrap();
    let run = core
        .dispatch(&Command {
            thread_id: target.thread_id.clone(),
            message: "work".into(),
            ..Command::new("c3", CommandKind::SendMessage)
        })
        .unwrap();

    // Register watch while target run is still queued/non-terminal.
    let watch_id = core
        .register_watch(&watcher.thread_id, &target.thread_id, &run.run_id)
        .unwrap();
    assert!(watch_id.starts_with("watch_"));

    // Cancel target run -> terminal state triggers watch delivery to watcher thread!
    core.dispatch(&Command {
        run_id: run.run_id.clone(),
        ..Command::new("c4", CommandKind::StopRun)
    })
    .unwrap();

    let snap = core.snapshot().unwrap();
    let target_run = snap
        .runs
        .iter()
        .flatten()
        .find(|r| r.id == run.run_id)
        .unwrap();
    assert_eq!(target_run.status, RunStatus::Cancelled);

    // Watcher should have received the auto-wakeup run/input from target!
    let watcher_has_wakeup = snap.runs.iter().flatten().any(|r| {
        r.thread_id == watcher.thread_id && r.input.contains("Vantry 自动唤醒")
    }) || snap.inputs.iter().any(|i| {
        i.thread_id == watcher.thread_id && i.message.contains("Vantry 自动唤醒")
    });
    assert!(
        watcher_has_wakeup,
        "expected watch delivery on watcher thread"
    );

    // Registering a second watch on an already-terminal run delivers immediately!
    let _watch_id2 = core
        .register_watch(&watcher.thread_id, &target.thread_id, &run.run_id)
        .unwrap();
    let snap2 = core.snapshot().unwrap();
    let wakeup_count = snap2
        .runs
        .iter()
        .flatten()
        .filter(|r| r.thread_id == watcher.thread_id && r.input.contains("Vantry 自动唤醒"))
        .count()
        + snap2
            .inputs
            .iter()
            .filter(|i| i.thread_id == watcher.thread_id && i.message.contains("Vantry 自动唤醒"))
            .count();
    assert!(wakeup_count >= 2, "expected immediate delivery for already-terminal run");
}
