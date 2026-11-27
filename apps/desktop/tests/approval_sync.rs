//! One approval, several desktops, one real Core (`vantry-service core
//! --provider controlled` over its Unix socket, the same `ClientBridge` the
//! window uses): every client shows the request, any one answers it, the
//! Core takes one answer, the others settle, and a client that was away
//! corrects itself on resync.
//!
//! Needs the service binary: `VANTRY_SERVICE_BIN`, or the workspace's
//! `target/debug/vantry-service` (`cargo build -p vantry-service`). Without
//! it the tests say so and pass vacuously.
#![cfg(unix)]

use std::{
    path::{Path, PathBuf},
    process::{Child, Command as Process, Stdio},
    sync::Arc,
    time::{Duration, Instant},
};

use tokio::sync::mpsc::UnboundedReceiver;
use vantry_desktop::{
    bridge::{BridgeError, ClientBridge, CoreBridge, ErrorCode},
    state::{AppState, ApprovalDecision, SETTLED_ELSEWHERE, ThreadStatus, TimelineItem},
};
use vantry_protocol::{Command, CoreEvent, Receipt};

const MARKER: &str = "[controlled:approval]";

struct Core {
    child: Child,
    _dir: tempfile::TempDir,
    socket: PathBuf,
}

impl Drop for Core {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn service_bin() -> Option<PathBuf> {
    if let Some(path) = std::env::var_os("VANTRY_SERVICE_BIN") {
        return Some(path.into());
    }
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../target/debug/vantry-service");
    path.exists().then_some(path)
}

async fn start_core() -> Option<Core> {
    let Some(exe) = service_bin() else {
        eprintln!("skipped: no vantry-service binary (cargo build -p vantry-service)");
        return None;
    };
    let dir = tempfile::tempdir().unwrap();
    let data = dir.path().join("data");
    let child = Process::new(exe)
        .args([
            "core",
            "--provider",
            "controlled",
            "--controlled-delay",
            "1ms",
            "--data-dir",
        ])
        .arg(&data)
        .env("HOME", dir.path())
        .env("PI_CODING_AGENT_DIR", dir.path().join("pi"))
        .env("VANTRY_DATA_DIR", &data)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let mut core = Core {
        child,
        socket: data.join("core.sock"),
        _dir: dir,
    };
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        assert!(
            core.child.try_wait().unwrap().is_none(),
            "Core exited during startup"
        );
        if tokio::net::UnixStream::connect(&core.socket).await.is_ok() {
            return Some(core);
        }
        assert!(Instant::now() < deadline, "Core listener did not start");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// A desktop without its window: the state, its bridge, its event stream.
struct Desktop {
    bridge: Arc<ClientBridge>,
    state: AppState,
    events: UnboundedReceiver<CoreEvent>,
    stream: tokio::task::JoinHandle<Result<(), BridgeError>>,
}

impl Desktop {
    async fn connect(socket: &Path) -> Self {
        let bridge = Arc::new(ClientBridge::new(socket.to_owned()));
        let mut state = AppState::default();
        state.connecting("本机 Core");
        let (stream, events) = Self::subscribe(&bridge, &mut state).await;
        Desktop {
            bridge,
            state,
            events,
            stream,
        }
    }

    async fn subscribe(
        bridge: &Arc<ClientBridge>,
        state: &mut AppState,
    ) -> (
        tokio::task::JoinHandle<Result<(), BridgeError>>,
        UnboundedReceiver<CoreEvent>,
    ) {
        state.apply_snapshot(bridge.snapshot().await.unwrap());
        state.on_live();
        let (sink, events) = tokio::sync::mpsc::unbounded_channel();
        let stream = tokio::spawn(bridge.subscribe(state.sequence, sink));
        (stream, events)
    }

    /// The connection drops: no more events until [`Self::reconnect`].
    fn disconnect(&mut self) {
        self.stream.abort();
        while self.events.try_recv().is_ok() {}
    }

    /// The window's resync: a fresh snapshot, then the stream from there.
    async fn reconnect(&mut self) {
        let (stream, events) = Self::subscribe(&self.bridge, &mut self.state).await;
        self.stream = stream;
        self.events = events;
    }

    async fn pump(&mut self, what: &str, done: impl Fn(&AppState) -> bool) {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(20);
        while !done(&self.state) {
            let event = tokio::time::timeout_at(deadline, self.events.recv())
                .await
                .unwrap_or_else(|_| panic!("timed out waiting for {what}"))
                .expect("the stream is open");
            self.state.apply_event(&event);
        }
    }

    async fn send(&mut self, command: Command) {
        let id = command.command_id.clone();
        let result = self.bridge.dispatch(command).await;
        if let Some(next) = self.state.on_command_result(&id, result) {
            Box::pin(self.send(next)).await;
        }
    }

    /// The approval card's request id in the shown timeline.
    fn card(&self) -> Option<(String, String)> {
        self.state.timeline().into_iter().find_map(|row| match row {
            TimelineItem::Approval { view, .. } => Some((view.request_id, view.status)),
            _ => None,
        })
    }

    fn request_status(&self, id: &str) -> String {
        self.state
            .requests
            .iter()
            .find(|r| r.id == id)
            .map(|r| r.status.clone())
            .unwrap_or_default()
    }
}

/// Starts a controlled run that waits on an approval, from `a`; both
/// desktops show the same thread. Returns the thread and the request.
async fn ask(a: &mut Desktop, b: &mut Desktop) -> (String, String) {
    a.state.new_thread.provider_id = "controlled".into();
    a.state.set_draft_text(&format!("列出目录 {MARKER}"));
    let create = a.state.submit("controlled").unwrap();
    a.send(create).await;
    let thread = a.state.selected.clone().unwrap();
    a.pump("the approval on A", |s| {
        s.approval_queue().iter().any(|r| r.status == "pending")
    })
    .await;
    let thread_id = thread.clone();
    b.pump("the approval on B", move |s| {
        s.approval_queue()
            .iter()
            .any(|r| r.thread_id == thread_id && r.status == "pending")
    })
    .await;
    b.state.select_thread(Some(thread.clone()));
    let (request, status) = a.card().expect("A shows the approval on its card");
    assert_eq!(status, "pending");
    assert_eq!(b.card().expect("B shows the same card").0, request);
    for desktop in [&*a, &*b] {
        // One entry: the card, not also a dialog.
        assert!(desktop.state.modal_request().is_none());
        assert_eq!(
            desktop.state.thread_status(&thread),
            ThreadStatus::NeedsApproval
        );
        let view = desktop
            .state
            .timeline()
            .into_iter()
            .find_map(|row| match row {
                TimelineItem::Approval { view, .. } => Some(view),
                _ => None,
            })
            .unwrap();
        // The command as it will run, not its JSON.
        assert_eq!(view.body, "ls -la; rg -i \"qmt\" .\nls -lh");
    }
    (thread, request)
}

fn settled(s: &AppState, request: &str) -> bool {
    s.requests
        .iter()
        .any(|r| r.id == request && r.status == "responded")
        && s.runs.iter().all(|r| r.status.terminal())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn two_desktops_answer_at_once_and_the_core_takes_one() {
    let Some(core) = start_core().await else {
        return;
    };
    let mut a = Desktop::connect(&core.socket).await;
    let mut b = Desktop::connect(&core.socket).await;
    let (thread, request) = ask(&mut a, &mut b).await;

    // Both answer at the same moment, differently.
    let allow = a.state.respond(&request, &ApprovalDecision::Allow).unwrap();
    let deny = b.state.respond(&request, &ApprovalDecision::Deny).unwrap();
    let (ra, rb) = tokio::join!(
        a.bridge.dispatch(allow.clone()),
        b.bridge.dispatch(deny.clone())
    );
    let accepted = |r: &Result<Receipt, BridgeError>| r.as_ref().is_ok_and(|r| r.accepted);
    let refused =
        |r: &Result<Receipt, BridgeError>| r.as_ref().is_err_and(|e| e.code == ErrorCode::Conflict);
    assert!(
        accepted(&ra) && refused(&rb) || refused(&ra) && accepted(&rb),
        "exactly one answer is taken: {ra:?} / {rb:?}"
    );
    let a_won = accepted(&ra);
    // Retrying the winner (same command id) is the same answer, not a second one.
    let winner = if a_won { allow.clone() } else { deny.clone() };
    let again = if a_won {
        a.bridge.dispatch(winner.clone()).await
    } else {
        b.bridge.dispatch(winner.clone()).await
    };
    assert!(accepted(&again), "replayed receipt: {again:?}");

    a.state.on_command_result(&allow.command_id, ra);
    b.state.on_command_result(&deny.command_id, rb);
    let loser = if a_won { &b } else { &a };
    // The loser's card settles at once, says why, and cannot answer again.
    assert!(matches!(
        loser.request_status(&request).as_str(),
        "responding" | "responded" | SETTLED_ELSEWHERE
    ));
    assert!(
        loser
            .state
            .notice
            .as_ref()
            .is_some_and(|n| n.text.contains("其他端"))
    );

    a.pump("A to converge", |s| settled(s, &request)).await;
    b.pump("B to converge", |s| settled(s, &request)).await;
    for desktop in [&a, &b] {
        assert!(desktop.card().is_none(), "no card left once settled");
        assert!(desktop.state.pending.is_empty());
        assert!(desktop.state.modal_request().is_none());
        assert_eq!(desktop.request_status(&request), "responded");
    }
    // Both show the same outcome: the winner's answer.
    let run = |d: &Desktop| {
        d.state
            .runs
            .iter()
            .find(|r| r.thread_id == thread)
            .map(|r| r.status)
    };
    assert_eq!(run(&a), run(&b));
    drop(core);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_desktop_that_was_away_corrects_itself_on_resync() {
    let Some(core) = start_core().await else {
        return;
    };
    let mut a = Desktop::connect(&core.socket).await;
    let mut b = Desktop::connect(&core.socket).await;
    let (thread, request) = ask(&mut a, &mut b).await;

    // B answers as its connection drops: the outcome is unknown to it.
    let lost = b.state.respond(&request, &ApprovalDecision::Deny).unwrap();
    b.disconnect();
    b.state.on_command_result(
        &lost.command_id,
        Err(BridgeError::unavailable("socket closed")),
    );
    assert_eq!(b.request_status(&request), "responding");

    // Meanwhile A answers and the run finishes.
    let allow = a.state.respond(&request, &ApprovalDecision::Allow).unwrap();
    a.send(allow).await;
    a.pump("A to finish", |s| settled(s, &request)).await;

    // B comes back: the Core's state replaces its guess, its answer is over.
    b.reconnect().await;
    assert_eq!(b.request_status(&request), "responded");
    assert!(b.state.pending.is_empty(), "the lost answer is not retried");
    assert!(b.state.retry(&format!("request:{request}")).is_none());
    assert!(b.card().is_none());
    assert_ne!(b.state.thread_status(&thread), ThreadStatus::NeedsApproval);
    assert!(b.state.background_attention().is_empty());
    let run = |d: &Desktop| {
        d.state
            .runs
            .iter()
            .find(|r| r.thread_id == thread)
            .map(|r| r.status)
    };
    assert_eq!(run(&a), run(&b));
    drop(core);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_desktop_reading_another_thread_is_pointed_to_the_request_not_interrupted() {
    let Some(core) = start_core().await else {
        return;
    };
    let mut a = Desktop::connect(&core.socket).await;
    let mut b = Desktop::connect(&core.socket).await;
    let (thread, request) = ask(&mut a, &mut b).await;
    // B reads nothing in particular (or another thread): no dialog, a pointer.
    b.state.select_thread(None);
    assert!(b.state.modal_request().is_none());
    let attention = b.state.background_attention();
    assert_eq!(attention.len(), 1);
    assert_eq!(
        (attention[0].thread_id.as_str(), attention[0].count),
        (thread.as_str(), 1)
    );
    // Answered on A: B's pointer goes away by itself.
    let allow = a.state.respond(&request, &ApprovalDecision::Allow).unwrap();
    a.send(allow).await;
    b.pump("B to see the answer", |s| settled(s, &request))
        .await;
    assert!(b.state.background_attention().is_empty());
    drop(core);
}
