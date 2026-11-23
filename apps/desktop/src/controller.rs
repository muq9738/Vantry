//! The NSH side of the state: one surface-wide signal holding [`AppState`],
//! the bridge, the connection loop, and the intents views call.
//!
//! Bridge calls run on the runtime's threads (`background`) and their
//! results come back to the engine thread, where they update the signal;
//! views read the signal through `computed` memos, so a change reaches only
//! what it changes.

use std::{sync::Arc, time::Duration};

use nsh_core::prelude::*;

use crate::{
    bridge::{self, BridgeError, CoreBridge, ErrorCode, wire::Command},
    cli::LaunchConfig,
    state::{AppState, ApprovalDecision, DecisionError, Recovery, RunningAction},
};

/// How long a fresh subscription must stay up before the header says it is
/// live (a refused connection fails well within this).
const LIVE_AFTER: Duration = Duration::from_millis(300);

/// The surface's state.
pub(crate) fn state() -> Signal<AppState> {
    global::<AppState>()
}

/// The bridge, once [`start`] made it.
#[derive(Default)]
struct BridgeSlot(Option<Arc<dyn CoreBridge>>);

fn bridge() -> Option<Arc<dyn CoreBridge>> {
    global::<BridgeSlot>().with_untracked(|slot| slot.0.clone())
}

/// Connects to the Core `config` names and keeps the state in step with it
/// for as long as the calling component lives.
pub(crate) fn start(config: &LaunchConfig) {
    let bridge = bridge::connect(&config.endpoint);
    global::<BridgeSlot>().set(BridgeSlot(Some(bridge.clone())));
    let label = bridge.label();
    let workspace = config.workspace_label();
    state().update(|s| {
        s.connecting(label);
        s.workspace_label = workspace;
    });
    spawn(run_connection(bridge));
}

/// A bridge call's result, with a runtime failure (a panicked task) as an
/// error whose outcome is unknown.
async fn call<T: Send + 'static>(future: bridge::BridgeFuture<T>) -> Result<T, BridgeError> {
    match background(future).await {
        Ok(result) => result,
        Err(join) => Err(BridgeError::new(ErrorCode::Internal, join.to_string())),
    }
}

/// Loads the snapshot, follows the event stream, and recovers: resubscribes
/// after the last applied event, or reloads the snapshot on
/// `resync_required`, backing off between attempts.
async fn run_connection(bridge: Arc<dyn CoreBridge>) {
    let mut need_snapshot = true;
    loop {
        if need_snapshot {
            match call(bridge.snapshot()).await {
                Ok(snapshot) => {
                    state().update(|s| {
                        s.apply_snapshot(snapshot);
                        s.on_live();
                    });
                    need_snapshot = false;
                }
                Err(error) => {
                    let mut recovery = None;
                    state().update(|s| recovery = Some(s.on_snapshot_failed(&error)));
                    if let Some(Recovery::Resync { delay } | Recovery::Resubscribe { delay, .. }) =
                        recovery
                    {
                        timer(delay).await;
                    }
                    continue;
                }
            }
        }
        let after = state().with_untracked(|s| s.sequence);
        let (sink, mut events) = tokio::sync::mpsc::unbounded_channel();
        let mut stream = std::pin::pin!(call(bridge.subscribe(after, sink)));
        let mut live = false;
        let result = loop {
            tokio::select! {
                result = &mut stream => {
                    // Fold what arrived before the stream ended.
                    drain(&mut events);
                    break result;
                }
                event = events.recv() => {
                    if let Some(event) = event {
                        let mut batch = vec![event];
                        while let Ok(more) = events.try_recv() {
                            batch.push(more);
                        }
                        state().update(|s| {
                            for event in &batch {
                                s.apply_event(event);
                            }
                            if !live {
                                s.on_live();
                            }
                        });
                        live = true;
                    }
                }
                () = timer(LIVE_AFTER), if !live => {
                    state().update(AppState::on_live);
                    live = true;
                }
            }
        };
        let mut recovery = None;
        state().update(|s| recovery = Some(s.on_stream_ended(result)));
        match recovery {
            Some(Recovery::Resync { delay }) => {
                need_snapshot = true;
                timer(delay).await;
            }
            Some(Recovery::Resubscribe { delay, .. }) => timer(delay).await,
            None => {}
        }
    }
}

fn drain(events: &mut tokio::sync::mpsc::UnboundedReceiver<bridge::wire::Event>) {
    let mut batch = Vec::new();
    while let Ok(event) = events.try_recv() {
        batch.push(event);
    }
    if !batch.is_empty() {
        state().update(|s| {
            for event in &batch {
                s.apply_event(event);
            }
        });
    }
}

/// Sends `command`; its result goes back into the state, which may name a
/// follow-up (the first message after its thread was created).
fn dispatch(command: Command) {
    let Some(bridge) = bridge() else {
        return;
    };
    spawn(async move {
        let id = command.command_id.clone();
        let result = call(bridge.dispatch(command)).await;
        let mut follow_up = None;
        state().update(|s| follow_up = s.on_command_result(&id, result));
        if let Some(next) = follow_up {
            dispatch(next);
        }
    });
}

/// Runs an intent that may produce a command, and dispatches it.
fn intent(f: impl FnOnce(&mut AppState) -> Option<Command>) {
    let mut command = None;
    state().update(|s| command = f(s));
    if let Some(command) = command {
        dispatch(command);
    }
}

// ----------------------------------------------------------------- intents

pub(crate) fn submit() {
    intent(AppState::submit);
}

pub(crate) fn stop() {
    intent(AppState::stop);
}

pub(crate) fn retry(key: String) {
    intent(move |s| s.retry(&key));
}

pub(crate) fn abandon(key: String) {
    state().update(move |s| s.abandon(&key));
}

pub(crate) fn respond(request_id: String, decision: ApprovalDecision) {
    intent(move |s| match s.respond(&request_id, &decision) {
        Ok(command) => Some(command),
        Err(DecisionError::Invalid(why)) => {
            s.notice = Some(crate::state::Notice {
                level: crate::state::NoticeLevel::Warn,
                text: why.to_owned(),
            });
            None
        }
        Err(DecisionError::NotPending) => None,
    });
}

pub(crate) fn select_thread(id: String) {
    state().update(move |s| s.select_thread(Some(id)));
}

pub(crate) fn new_thread() {
    state().update(AppState::new_thread);
}

pub(crate) fn set_draft(text: &str) {
    // The controlled input echoes its value: skip writes that change nothing.
    if state().with_untracked(|s| s.draft().text == text) {
        return;
    }
    let text = text.to_owned();
    state().update(move |s| s.set_draft_text(&text));
}

pub(crate) fn toggle_mode() {
    state().update(AppState::toggle_mode);
}

pub(crate) fn set_running_action(action: RunningAction) {
    state().update(move |s| s.set_running_action(action));
}

pub(crate) fn dismiss_notice() {
    state().update(AppState::dismiss_notice);
}

pub(crate) fn defer_request(id: String) {
    state().update(move |s| s.defer_request(&id));
}

pub(crate) fn reopen_request(id: String) {
    state().update(move |s| s.reopen_request(&id));
}
