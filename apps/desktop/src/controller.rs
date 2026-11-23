//! The NSH side of the states: three surface-wide signals ([`AppState`]
//! the conversation, [`UiState`] the window, [`WorkbenchState`] the dock),
//! the bridges, the connection loop, persistence, and the intents views
//! call.
//!
//! Bridge calls run on the runtime's threads (`background`) and their
//! results come back to the engine thread, where they update a signal;
//! views read the signals through `computed` memos, so a change reaches only
//! what it changes. A terminal's output updates the dock's signal only, so
//! it never wakes the timeline.

use std::{cell::RefCell, path::PathBuf, sync::Arc, time::Duration};

use nsh_core::prelude::*;
use vantry_protocol::{Command, CoreEvent};

use crate::{
    bridge::{
        self, BridgeError, CoreBridge, Endpoint, ErrorCode, LocalWorkbench, MockWorkbench,
        WorkbenchBridge,
    },
    cli::LaunchConfig,
    state::{
        AppState, ApprovalDecision, DecisionError, Notice, NoticeLevel, Recovery, RunningAction,
        persist,
        sidebar::StatusFilter,
        ui::{Confirm, MainView, SettingsPage, ThemePreference, UiState},
        workbench::{DockTab, Load, WorkbenchState},
    },
};

/// How long a fresh subscription must stay up before the header says it is
/// live (a refused connection fails well within this).
const LIVE_AFTER: Duration = Duration::from_millis(300);
/// Changes are saved this long after the last one.
const SAVE_AFTER: Duration = Duration::from_millis(600);

/// The conversation.
pub(crate) fn state() -> Signal<AppState> {
    global::<AppState>()
}

/// The window.
pub(crate) fn ui() -> Signal<UiState> {
    global::<UiState>()
}

/// The dock.
pub(crate) fn bench() -> Signal<WorkbenchState> {
    global::<WorkbenchState>()
}

/// The bridges and where state is saved, once [`start`] made them.
#[derive(Default)]
struct Bridges {
    core: Option<Arc<dyn CoreBridge>>,
    workbench: Option<Arc<dyn WorkbenchBridge>>,
    state_path: Option<PathBuf>,
    mock: bool,
}

impl PartialEq for Bridges {
    fn eq(&self, _: &Self) -> bool {
        false
    }
}

fn bridges<R>(f: impl FnOnce(&Bridges) -> R) -> R {
    global::<Bridges>().with_untracked(f)
}

fn core() -> Option<Arc<dyn CoreBridge>> {
    bridges(|b| b.core.clone())
}

fn workbench() -> Option<Arc<dyn WorkbenchBridge>> {
    bridges(|b| b.workbench.clone())
}

/// Connects to the Core `config` names, restores `desktop-state.json` and
/// keeps the states in step for as long as the calling component lives.
pub(crate) fn start(config: &LaunchConfig) {
    let core = bridge::connect(&config.endpoint);
    let mock = config.endpoint == Endpoint::Mock;
    let workbench: Arc<dyn WorkbenchBridge> = if mock {
        Arc::new(MockWorkbench::demo())
    } else {
        Arc::new(LocalWorkbench::new(
            config.workspace_root(),
            config.model.clone(),
        ))
    };
    let state_path = config.state_path();
    global::<Bridges>().set(Bridges {
        core: Some(core.clone()),
        workbench: Some(workbench.clone()),
        state_path: state_path.clone(),
        mock,
    });

    let saved = state_path.as_deref().map(persist::load);
    let label = core.label();
    let workspace = config.workspace_label();
    let root = workbench.root();
    let remote_target = config.endpoint.is_remote().then(|| label.clone());
    let mut restore_error = None;
    state().update(|s| {
        s.connecting(label);
        s.workspace_label = workspace;
        s.new_thread.provider_id = config.provider_id.clone();
        s.new_thread.model = config.model.clone().unwrap_or_default();
        s.new_thread.workspace = config.workspace.clone();
        s.new_thread.approval_policy = "ask".to_owned();
        ui().update(|u| {
            u.remote_target = remote_target;
            match saved {
                Some(Ok(Some(file))) => persist::restore(&file, s, u),
                Some(Err(error)) => restore_error = Some(error.to_string()),
                _ => {}
            }
        });
        if let Some(error) = &restore_error {
            s.notice = Some(Notice::new(
                NoticeLevel::Warn,
                format!("无法读取 desktop-state.json（{error}），已使用默认设置。"),
            ));
        }
    });
    bench().update(|b| b.root = root);
    // A dock left open shows its tab's content again.
    let (dock_open, tab) = ui().with_untracked(|u| (u.dock_open, u.dock_tab));
    if dock_open {
        show_tab(tab);
    }

    // The theme follows the preference.
    effect(|| {
        let theme = ui().with(|u| u.theme);
        set_color_scheme(match theme {
            ThemePreference::System => Appearance::System,
            ThemePreference::Light => Appearance::Light,
            ThemePreference::Dark => Appearance::Dark,
        });
    });
    // What lasts is saved shortly after it changes; nothing when the
    // restored file could not be read (it stays for the user to look at).
    if restore_error.is_none() && state_path.is_some() {
        effect(|| {
            let file = state().with(|s| ui().with(|u| persist::capture(s, u)));
            schedule_save(file);
        });
    }
    spawn(run_connection(core));
}

// ---------------------------------------------------------------- persistence

#[derive(Default)]
struct SaveSlot {
    last: Option<persist::DesktopStateFile>,
    generation: u64,
}

thread_local! {
    static SAVES: RefCell<SaveSlot> = RefCell::new(SaveSlot::default());
}

/// Saves `file` after [`SAVE_AFTER`] unless a newer one replaces it.
fn schedule_save(file: persist::DesktopStateFile) {
    let generation = SAVES.with(|slot| {
        let mut slot = slot.borrow_mut();
        if slot.last.as_ref() == Some(&file) {
            return None;
        }
        // The first capture is what was restored: nothing to write.
        let first = slot.last.is_none();
        slot.last = Some(file.clone());
        slot.generation += 1;
        (!first).then_some(slot.generation)
    });
    let Some(generation) = generation else {
        return;
    };
    let Some(path) = bridges(|b| b.state_path.clone()) else {
        return;
    };
    set_timeout(SAVE_AFTER, move || {
        let current = SAVES.with(|slot| slot.borrow().generation);
        if current != generation {
            return;
        }
        let path = path.clone();
        let file = file.clone();
        spawn(async move {
            let result = blocking(move || persist::save_atomic(&path, &file)).await;
            if let Ok(Err(error)) | Err(error) = result.map_err(std::io::Error::other) {
                eprintln!("vantry: desktop-state.json: {error}");
            }
        });
    });
}

// ---------------------------------------------------------------- connection

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
async fn run_connection(core: Arc<dyn CoreBridge>) {
    let mut need_snapshot = true;
    loop {
        if need_snapshot {
            match call(core.snapshot()).await {
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
        let mut stream = std::pin::pin!(call(core.subscribe(after, sink)));
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

fn drain(events: &mut tokio::sync::mpsc::UnboundedReceiver<CoreEvent>) {
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
    let Some(core) = core() else {
        return;
    };
    spawn(async move {
        let id = command.command_id.clone();
        let result = call(core.dispatch(command)).await;
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

fn warn(text: impl Into<String>) {
    let text = text.into();
    state().update(move |s| s.notice = Some(Notice::new(NoticeLevel::Warn, text)));
}

// ------------------------------------------------------------ conversation

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
            s.notice = Some(Notice::new(NoticeLevel::Warn, why));
            None
        }
        Err(DecisionError::NotPending) => None,
    });
}

pub(crate) fn select_thread(id: String) {
    state().update(move |s| s.select_thread(Some(id)));
    ui().update(|u| {
        if u.view != MainView::Conversation {
            u.view = MainView::Conversation;
        }
    });
}

pub(crate) fn new_thread() {
    state().update(AppState::new_thread);
    ui().update(|u| u.view = MainView::Conversation);
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

/// Copies `text` to the clipboard and says so.
pub(crate) fn copy(text: String) {
    set_clipboard(text);
    state().update(|s| s.notice = Some(Notice::new(NoticeLevel::Info, "已复制。")));
}

/// Copies a reply's text row.
pub(crate) fn copy_reply(run_id: String, start: usize, end: usize) {
    let text = state().with_untracked(|s| s.text_of(&run_id, start, end));
    copy(text);
}

// ----------------------------------------------------------------- threads

pub(crate) fn rename(id: String, title: String) {
    intent(move |s| s.rename_thread(&id, &title));
}

pub(crate) fn begin_rename(id: String) {
    let title = state().with_untracked(|s| s.thread(&id).map(|t| t.title.clone()));
    if let Some(title) = title {
        ui().update(move |u| u.begin_rename(&id, &title));
    }
}

pub(crate) fn set_rename_text(text: &str) {
    if ui().with_untracked(|u| u.rename.as_ref().is_some_and(|r| r.text == text)) {
        return;
    }
    let text = text.to_owned();
    ui().update(move |u| {
        if let Some(rename) = u.rename.as_mut() {
            rename.text = text;
        }
    });
}

pub(crate) fn finish_rename(save: bool) {
    let mut done = None;
    ui().update(|u| {
        done = u.finish_rename();
    });
    if save && let Some(rename) = done {
        self::rename(rename.thread_id, rename.text);
    }
}

pub(crate) fn toggle_pin(id: String) {
    intent(move |s| {
        let pinned = s.thread(&id).is_some_and(|t| t.pinned);
        s.set_pinned(&id, !pinned)
    });
}

pub(crate) fn toggle_archive(id: String) {
    intent(move |s| {
        let archived = s.thread(&id).is_some_and(|t| t.archived);
        s.set_archived(&id, !archived)
    });
}

/// Snoozes thread `id` for `hours`, or wakes it (`None`).
pub(crate) fn snooze(id: String, hours: Option<i64>) {
    let until = hours.map(|h| (chrono::Utc::now() + chrono::Duration::hours(h)).to_rfc3339());
    intent(move |s| s.snooze(&id, until));
}

pub(crate) fn ask_delete(id: String) {
    let title = state().with_untracked(|s| s.thread(&id).map(|t| t.title.clone()));
    if let Some(title) = title {
        ui().update(move |u| {
            u.confirm = Some(Confirm::Delete {
                thread_id: id,
                title,
            })
        });
    }
}

pub(crate) fn ask_rewind(thread_id: String, run_id: String) {
    ui().update(move |u| u.confirm = Some(Confirm::Rewind { thread_id, run_id }));
}

/// Answers the confirmation dialog.
pub(crate) fn confirm(yes: bool) {
    let mut asked = None;
    ui().update(|u| asked = u.confirm.take());
    if !yes {
        return;
    }
    match asked {
        Some(Confirm::Delete { thread_id, .. }) => intent(move |s| s.delete_thread(&thread_id)),
        Some(Confirm::Rewind { thread_id, run_id }) => {
            intent(move |s| s.rewind(&thread_id, &run_id))
        }
        None => {}
    }
}

pub(crate) fn fork(id: String, run_id: Option<String>) {
    intent(move |s| s.fork(&id, run_id.as_deref()));
}

pub(crate) fn merge_back(id: String) {
    intent(move |s| s.merge_back(&id));
}

pub(crate) fn set_policy(policy: String) {
    intent(move |s| s.set_policy(&policy));
}

pub(crate) fn set_default_policy(policy: String) {
    ui().update({
        let policy = policy.clone();
        move |u| u.default_policy = policy
    });
    state().update(move |s| s.new_thread.approval_policy = policy);
}

pub(crate) fn set_model(model: String) {
    intent(move |s| s.set_model(&model));
    ui().update(|u| {
        u.model_picker = false;
        u.model_query.clear();
    });
}

/// The model new threads start with.
pub(crate) fn set_default_model(model: String) {
    state().update(move |s| s.new_thread.model = model);
}

pub(crate) fn toggle_model_picker() {
    let open = ui().with_untracked(|u| !u.model_picker);
    ui().update(move |u| u.model_picker = open);
    if open {
        load_catalogs(false);
    }
}

pub(crate) fn set_model_query(text: &str) {
    if ui().with_untracked(|u| u.model_query == text) {
        return;
    }
    let text = text.to_owned();
    ui().update(move |u| u.model_query = text);
}

// ------------------------------------------------------------------- queue

pub(crate) fn edit_queued(id: String) {
    state().update(move |s| s.begin_queue_edit(&id));
}

pub(crate) fn cancel_queue_edit() {
    state().update(AppState::end_queue_edit);
}

pub(crate) fn move_queued(id: String, delta: i64) {
    intent(move |s| s.move_queued(&id, delta));
}

pub(crate) fn promote_queued(id: String) {
    intent(move |s| s.promote_queued(&id));
}

pub(crate) fn cancel_queued(id: String) {
    intent(move |s| s.cancel_queued(&id));
}

// ------------------------------------------------------------- attachments

/// Picks files and attaches them to the shown draft.
pub(crate) fn pick_attachments() {
    let Some(workbench) = workbench() else {
        return;
    };
    spawn(async move {
        let picked = pick_paths(
            nsh_core::file_dialog::OpenDialog::files()
                .multiple(true)
                .title("添加附件"),
        )
        .await;
        let paths = match picked {
            Ok(Some(paths)) => paths,
            Ok(None) => return,
            Err(error) => {
                warn(format!("无法打开文件选择器：{error}"));
                return;
            }
        };
        for path in paths {
            match call(workbench.attach(path)).await {
                Ok(attachment) => state().update(move |s| s.add_attachment(attachment)),
                Err(error) => warn(format!("无法附加文件：{}", error.message)),
            }
        }
    });
}

pub(crate) fn remove_attachment(id: String) {
    state().update(move |s| s.remove_attachment(&id));
}

// --------------------------------------------------------------------- ui

pub(crate) fn toggle_sidebar() {
    ui().update(|u| u.sidebar_open = !u.sidebar_open);
}

pub(crate) fn cycle_theme() {
    ui().update(|u| u.theme = u.theme.next());
}

pub(crate) fn set_theme(theme: ThemePreference) {
    ui().update(move |u| u.theme = theme);
}

pub(crate) fn set_query(text: &str) {
    if ui().with_untracked(|u| u.filter.query == text) {
        return;
    }
    let text = text.to_owned();
    ui().update(move |u| u.set_query(&text));
}

pub(crate) fn set_status_filter(status: StatusFilter) {
    ui().update(move |u| u.set_status_filter(status));
}

pub(crate) fn toggle_archived_group() {
    ui().update(|u| u.filter.show_archived = !u.filter.show_archived);
}

pub(crate) fn toggle_collapsed(id: String) {
    ui().update(move |u| u.toggle_collapsed(&id));
}

pub(crate) fn open_settings(page: SettingsPage) {
    let mut load = false;
    ui().update(|u| load = u.open_settings(page));
    if load {
        load_catalogs(true);
    }
}

pub(crate) fn close_settings() {
    ui().update(|u| u.settings = None);
}

/// Loads the settings' catalogs (models, plugins, MCP, update, peers).
fn load_catalogs(already_marked: bool) {
    if !already_marked {
        let mut go = false;
        ui().update(|u| {
            if matches!(
                u.catalogs,
                crate::state::ui::Catalogs::NotLoaded | crate::state::ui::Catalogs::Failed(_)
            ) {
                u.catalogs = crate::state::ui::Catalogs::Loading;
                go = true;
            }
        });
        if !go {
            return;
        }
    }
    let Some(workbench) = workbench() else {
        return;
    };
    spawn(async move {
        let result = call(workbench.settings()).await.map_err(|e| e.message);
        ui().update(move |u| u.on_catalogs(result));
    });
}

/// Loads the settings' catalogs unless they are loading or loaded.
pub(crate) fn load_settings_catalogs() {
    load_catalogs(false);
}

pub(crate) fn reload_catalogs() {
    ui().update(|u| u.catalogs = crate::state::ui::Catalogs::NotLoaded);
    load_catalogs(false);
}

pub(crate) fn toggle_extensions() {
    ui().update(|u| u.extensions_enabled = !u.extensions_enabled);
}

pub(crate) fn toggle_mcp(name: String) {
    ui().update(move |u| u.toggle_mcp(&name));
}

pub(crate) fn show_view(view: MainView) {
    ui().update(move |u| u.view = view);
}

pub(crate) fn open_plugin(id: String, page: String) {
    ui().update(move |u| u.open_plugin(&id, &page));
}

// ---------------------------------------------------------- remote desktop

/// Connects the remote desktop view to `peer`. The media stream is Wave 3:
/// the demo shows a connected canvas, a real peer reports that it is not
/// wired yet.
pub(crate) fn remote_connect(peer: String) {
    let mut start = false;
    ui().update(|u| start = u.remote.connect(&peer));
    if !start {
        return;
    }
    let mock = bridges(|b| b.mock);
    set_timeout(Duration::from_millis(600), move || {
        let result = if mock {
            Ok(())
        } else {
            Err("远程桌面媒体流将在 Wave 3 接入".to_owned())
        };
        ui().update(move |u| u.remote.on_result(&peer, result));
    });
}

pub(crate) fn remote_disconnect() {
    ui().update(|u| u.remote.disconnect());
}

pub(crate) fn remote_update(f: impl FnOnce(&mut crate::state::ui::RemoteDesktopState) + 'static) {
    ui().update(move |u| f(&mut u.remote));
}

// --------------------------------------------------------------------- dock

/// Shows the dock on `tab` (or hides it when it shows that tab).
pub(crate) fn toggle_dock(tab: DockTab) {
    let mut open = false;
    ui().update(|u| {
        u.toggle_dock_tab(tab);
        open = u.dock_open;
    });
    if open {
        show_tab(tab);
    }
}

pub(crate) fn close_dock() {
    ui().update(|u| u.dock_open = false);
}

pub(crate) fn select_dock_tab(tab: DockTab) {
    ui().update(move |u| {
        u.dock_open = true;
        u.dock_tab = tab;
    });
    show_tab(tab);
}

fn show_tab(tab: DockTab) {
    let mut loads = Vec::new();
    bench().update(|b| loads = b.on_tab(tab));
    run_loads(loads);
}

/// Runs what the dock asked to load.
fn run_loads(loads: Vec<Load>) {
    let Some(workbench) = workbench() else {
        return;
    };
    for load in loads {
        let workbench = workbench.clone();
        spawn(async move {
            match load {
                Load::Dir(path) => {
                    let result = call(workbench.list_dir(path.clone())).await.map_err(|e| e.message);
                    bench().update(move |b| b.files.on_dir(&path, result));
                }
                Load::File(path) => {
                    let result = call(workbench.read_file(path.clone())).await.map_err(|e| e.message);
                    bench().update(move |b| b.files.on_file(&path, result));
                }
                Load::GitStatus => {
                    let result = call(workbench.git_status()).await.map_err(|e| e.message);
                    let mut next = None;
                    bench().update(|b| next = b.git.on_status(result));
                    run_loads(next.into_iter().collect());
                }
                Load::Diff { path, staged } => {
                    let result = call(workbench.git_diff(path.clone(), staged))
                        .await
                        .map_err(|e| e.message);
                    bench().update(move |b| b.git.on_diff(path.as_deref(), staged, result));
                }
                Load::Pr => {
                    let result = call(workbench.pr_summary()).await.map_err(|e| e.message);
                    bench().update(move |b| b.git.on_pr(result));
                }
            }
        });
    }
}

pub(crate) fn toggle_dir(path: String) {
    let mut load = None;
    bench().update(|b| load = b.files.toggle_dir(&path));
    run_loads(load.into_iter().collect());
}

pub(crate) fn select_file(path: String) {
    let mut load = None;
    bench().update(|b| load = b.files.select_file(&path));
    run_loads(load.into_iter().collect());
}

pub(crate) fn refresh_files() {
    let mut loads = Vec::new();
    bench().update(|b| loads = b.files.refresh());
    run_loads(loads);
}

/// Shows the selected file in the preview tab.
pub(crate) fn preview_file() {
    let mut shown = false;
    bench().update(|b| shown = b.preview_selected_file());
    if shown {
        select_dock_tab(DockTab::Preview);
    }
}

/// Shows `text` (a reply, a document) in the preview tab.
pub(crate) fn preview_text(title: String, text: String) {
    bench().update(move |b| b.preview.show(&title, &text));
    select_dock_tab(DockTab::Preview);
}

pub(crate) fn toggle_preview_source() {
    bench().update(|b| b.preview.toggle_source());
}

pub(crate) fn refresh_git() {
    let mut load = None;
    bench().update(|b| load = Some(b.git.refresh()));
    run_loads(load.into_iter().collect());
}

pub(crate) fn select_change(path: String, staged: bool) {
    let mut load = None;
    bench().update(|b| load = Some(b.git.select(&path, staged)));
    run_loads(load.into_iter().collect());
}

pub(crate) fn set_commit_message(text: &str) {
    if bench().with_untracked(|b| b.git.commit_message == text) {
        return;
    }
    let text = text.to_owned();
    bench().update(move |b| b.git.commit_message = text);
}

/// Stages (or unstages) `paths`.
pub(crate) fn stage(paths: Vec<String>, staged: bool) {
    let Some(workbench) = workbench() else {
        return;
    };
    let mut go = false;
    bench().update(|b| go = b.git.begin());
    if !go {
        return;
    }
    spawn(async move {
        let future = if staged {
            workbench.git_unstage(paths)
        } else {
            workbench.git_stage(paths)
        };
        let result = call(future).await.map(|()| None).map_err(|e| e.message);
        let mut next = None;
        bench().update(|b| next = Some(b.git.on_done(result)));
        run_loads(next.into_iter().collect());
    });
}

pub(crate) fn commit() {
    let Some(workbench) = workbench() else {
        return;
    };
    let mut message = None;
    bench().update(|b| {
        message = b.git.commit_ready();
        if message.is_some() {
            b.git.begin();
        }
    });
    let Some(message) = message else {
        return;
    };
    spawn(async move {
        let result = call(workbench.git_commit(message))
            .await
            .map(Some)
            .map_err(|e| e.message);
        let mut next = None;
        bench().update(|b| next = Some(b.git.on_done(result)));
        run_loads(next.into_iter().collect());
    });
}

pub(crate) fn load_pr() {
    run_loads(vec![Load::Pr]);
}

// ----------------------------------------------------------------- terminal

pub(crate) fn new_terminal() {
    bench().update(|b| {
        b.terminal.open();
    });
}

pub(crate) fn select_terminal(id: String) {
    bench().update(move |b| b.terminal.select(&id));
}

pub(crate) fn close_terminal(id: String) {
    let mut running = false;
    bench().update(|b| running = b.terminal.close(&id));
    if running {
        interrupt_session(id);
    }
}

pub(crate) fn set_terminal_input(text: &str) {
    if bench().with_untracked(|b| b.terminal.current().is_some_and(|s| s.input == text)) {
        return;
    }
    let text = text.to_owned();
    bench().update(move |b| b.terminal.set_input(&text));
}

pub(crate) fn recall_terminal(step: i32) {
    bench().update(move |b| b.terminal.recall(step));
}

pub(crate) fn clear_terminal() {
    bench().update(|b| b.terminal.clear());
}

/// Runs the shown session's input, streaming its output into the dock.
pub(crate) fn run_terminal() {
    let Some(workbench) = workbench() else {
        return;
    };
    let mut run = None;
    bench().update(|b| run = b.terminal.submit());
    let Some((session, command)) = run else {
        return;
    };
    let (sink, mut output) = tokio::sync::mpsc::unbounded_channel::<String>();
    let job = background(workbench.terminal_run(session.clone(), command, sink));
    spawn(async move {
        // Output in batches, then the exit.
        while let Some(chunk) = output.recv().await {
            let mut text = chunk;
            while let Ok(more) = output.try_recv() {
                text.push_str(&more);
            }
            let id = session.clone();
            bench().update(move |b| b.terminal.on_output(&id, &text));
        }
        let result = match job.await {
            Ok(result) => result.map_err(|e| e.message),
            Err(join) => Err(join.to_string()),
        };
        bench().update(move |b| b.terminal.on_exit(&session, result));
    });
}

pub(crate) fn interrupt_terminal() {
    if let Some(id) = bench().with_untracked(|b| b.terminal.interruptible()) {
        interrupt_session(id);
    }
}

fn interrupt_session(id: String) {
    if let Some(workbench) = workbench() {
        spawn(async move {
            let _ = call(workbench.terminal_interrupt(id)).await;
        });
    }
}

/// Opens the workspace root in the system's file manager.
pub(crate) fn reveal_workspace() {
    let root = bench().with_untracked(|b| b.root.clone());
    if bridges(|b| b.mock) || root.is_empty() {
        return;
    }
    spawn(async move {
        if let Err(error) = reveal_path(PathBuf::from(root)).await {
            warn(format!("无法打开文件夹：{error}"));
        }
    });
}

/// The plugin panel's settings toggle (a plugin's `disabled` flag is the
/// Core's; here it is the local switch shown until the Core's API lands).
pub(crate) fn plugin_notice(name: String) {
    state().update(move |s| {
        s.notice = Some(Notice::new(
            NoticeLevel::Info,
            format!("插件「{name}」的安装与启停由 Core 插件宿主管理（Wave 3 接入）。"),
        ))
    });
}
