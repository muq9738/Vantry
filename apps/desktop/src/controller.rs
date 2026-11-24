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
use vantry_protocol::{Command, CoreEvent, WorkbenchRequest};

use crate::{
    autostart,
    bridge::{
        self, BridgeError, CoreBridge, CoreWorkbench, Endpoint, ErrorCode, LocalWorkbench,
        MockWorkbench, WorkbenchBridge,
        workbench::actions::{self, names},
    },
    cli::LaunchConfig,
    service,
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
    owner: Option<Owner>,
    core: Option<Arc<dyn CoreBridge>>,
    workbench: Option<Arc<dyn WorkbenchBridge>>,
    core_socket: Option<PathBuf>,
    peer_id: Option<String>,
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

/// Runs a job for the lifetime of the window. The initiating menu or reactive
/// effect can disappear before the job completes.
fn spawn_for_window(future: impl std::future::Future<Output = ()> + 'static) {
    if let Some(owner) = bridges(|b| b.owner) {
        owner.run(|| {
            spawn(future);
        });
    }
}

fn core() -> Option<Arc<dyn CoreBridge>> {
    bridges(|b| b.core.clone())
}

fn workbench() -> Option<Arc<dyn WorkbenchBridge>> {
    bridges(|b| b.workbench.clone())
}

pub(crate) fn remote_project() -> bool {
    ui().with(|u| u.remote_target.is_some())
        || state().with(|s| {
            s.current_workspace()
                .is_some_and(|w| !w.environment_id.is_empty())
        })
}

/// File and Git jobs keep the checkout and generation they started with.
fn project_workbench() -> Option<(u64, Arc<dyn WorkbenchBridge>)> {
    let (root, generation) = bench().with_untracked(|b| (b.root.clone(), b.project_generation));
    if root.is_empty() {
        return None;
    }
    let bridge: Arc<dyn WorkbenchBridge> = if bridges(|b| b.mock) {
        workbench()?
    } else if remote_project() {
        let sock = bridges(|b| b.core_socket.clone())?;
        Arc::new(CoreWorkbench::for_core_workspace(
            sock,
            root.clone(),
            LocalWorkbench::new(PathBuf::from(&root), None),
            None,
        ))
    } else {
        Arc::new(LocalWorkbench::new(PathBuf::from(root), None))
    };
    Some((generation, bridge))
}

/// Connects to the Core `config` names, restores `desktop-state.json` and
/// keeps the states in step for as long as the calling component lives.
pub(crate) fn start(config: &LaunchConfig) {
    let mock = config.endpoint == Endpoint::Mock;
    let local = LocalWorkbench::new(config.workspace_root(), config.model.clone());
    let (core, workbench, core_socket): (
        Arc<dyn CoreBridge>,
        Arc<dyn WorkbenchBridge>,
        Option<PathBuf>,
    ) = match &config.endpoint {
        Endpoint::Mock => (
            bridge::connect(&config.endpoint),
            Arc::new(MockWorkbench::demo()),
            None,
        ),
        // Settings, plugins, remote access … from the Core; files, Git and
        // the terminal on this machine's workspace.
        Endpoint::Local { socket } => (
            bridge::connect(&config.endpoint),
            Arc::new(CoreWorkbench::new(
                socket.clone(),
                local,
                config.model.clone(),
            )),
            Some(socket.clone()),
        ),
        Endpoint::Peer { .. } => {
            let remote = bridge::RemoteBridge::new(config.endpoint.clone(), Some(config.data_dir.clone()));
            let sock = remote.socket();
            (
                Arc::new(remote),
                Arc::new(CoreWorkbench::new(sock.clone(), local, config.model.clone())),
                Some(sock),
            )
        }
    };
    let state_path = config.state_path();
    global::<Bridges>().set(Bridges {
        owner: Some(Owner::current()),
        core: Some(core.clone()),
        workbench: Some(workbench.clone()),
        core_socket,
        peer_id: match &config.endpoint {
            Endpoint::Peer { id, .. } => Some(id.clone()),
            _ => None,
        },
        state_path: state_path.clone(),
        mock,
    });

    let saved = state_path.as_deref().map(persist::load);
    let label = core.label();
    let workspace = config.workspace_label();
    let demo_root = mock.then(|| workbench.root());
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
    let project = computed(move || {
        // When viewing a federated child thread on a local Core whose checkout
        // belongs to another peer, keep local filesystem isolated; when connected
        // Expose the paired Core checkout through its workbench.
        let connected_remote = ui().with(|u| u.remote_target.is_some());
        if !connected_remote
            && state().with(|s| {
                s.current_workspace()
                    .is_some_and(|w| !w.environment_id.is_empty())
            })
        {
            return String::new();
        }
        state()
            .with(|s| s.checkout_path().map(str::to_owned))
            .or_else(|| demo_root.clone())
            .unwrap_or_default()
    });
    effect(move || {
        let root = project.get();
        untracked(|| {
            bench().update(|b| b.select_project(root));
            if let Some(tab) = ui().with_untracked(|u| u.dock_open.then_some(u.dock_tab)) {
                show_tab(tab);
            }
        });
    });

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
    if service::should_autostart(config) {
        let command = service::ensure_core_command(config, &service::Locator::from_env());
        spawn(boot_then_connect(core, command));
    } else {
        spawn(run_connection(core));
    }
}

/// Starts the local Core when it does not answer, then connects.
async fn boot_then_connect(
    core: Arc<dyn CoreBridge>,
    command: Result<service::ServiceCommand, service::NotFound>,
) {
    let down = matches!(
        call(core.health()).await,
        Err(BridgeError { code: ErrorCode::Unavailable, .. })
    );
    if down {
        let detail = match &command {
            Ok(command) => format!("{} ensure-core", command.program.display()),
            Err(_) => "未找到 vantry-service".to_owned(),
        };
        state().update(move |s| s.starting_core(detail));
        let result = background(autostart::ensure_core(core.clone(), command, || {})).await;
        let failure = match result {
            Ok(Ok(_)) => None,
            Ok(Err(reason)) => Some(reason),
            Err(join) => Some(join.to_string()),
        };
        if let Some(reason) = failure {
            state().update(move |s| s.on_core_start_failed(&reason));
        }
    }
    run_connection(core).await;
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
    spawn_for_window(async move {
        timer(SAVE_AFTER).await;
        let current = SAVES.with(|slot| slot.borrow().generation);
        if current != generation {
            return;
        }
        let result = blocking(move || persist::save_atomic(&path, &file)).await;
        if let Ok(Err(error)) | Err(error) = result.map_err(std::io::Error::other) {
            eprintln!("vantry: desktop-state.json: {error}");
        }
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
                    catalogs_after_connect();
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

/// Once the Core answers: the catalogs load (the composer's thinking
/// switch needs them), and ones read while it was down load again.
fn catalogs_after_connect() {
    let stale = ui().with_untracked(|u| match &u.catalogs {
        crate::state::ui::Catalogs::NotLoaded | crate::state::ui::Catalogs::Failed(_) => true,
        crate::state::ui::Catalogs::Loaded(data) => !data.degraded.is_empty() && !bridges(|b| b.mock),
        crate::state::ui::Catalogs::Loading => false,
    });
    if stale {
        ui().update(|u| u.catalogs = crate::state::ui::Catalogs::NotLoaded);
        load_catalogs(false);
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
    spawn_for_window(async move {
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

/// Send an immediate reply from an interactive in-message block (`ask`, `dispatch`, `vantry-ui`).
pub(crate) fn send_quick_reply(message: String) {
    let text = message.trim().to_owned();
    if text.is_empty() {
        return;
    }
    state().update(|s| {
        s.set_draft_text(&text);
    });
    submit();
}

/// Open a clickable link from a Markdown message (`vantry-thread://`, `file://`, `http(s)://`).
pub(crate) fn open_link(url: String) {
    let trimmed = url.trim();
    if let Some(rest) = trimmed.strip_prefix("vantry-thread://") {
        let thread_id = rest.split(['?', '#']).next().unwrap_or("").trim();
        let environment = rest.split_once('?').and_then(|(_, query)| {
            query.split('&').find_map(|p| p.strip_prefix("environmentId="))
        });
        if let Some(env) = environment.filter(|v| !v.is_empty())
            && !bridges(|b| match &b.peer_id {
                Some(peer) => peer == env,
                None => env == "local" || b.core_socket.as_deref().and_then(|p| p.parent())
                    .and_then(|p| std::fs::read_to_string(p.join("environment-id")).ok())
                    .is_some_and(|id| id.trim() == env),
            })
        {
            warn("请在链接所属的机器中打开会话。");
        } else if state().with_untracked(|s| s.thread(thread_id).is_some()) {
            select_thread(thread_id.to_owned());
        } else {
            warn("找不到链接中的会话。");
        }
        return;
    }
    if let Some(rest) = trimmed.strip_prefix("file://") {
        let path_part = rest.split('#').next().unwrap_or(rest);
        let root = bench().with_untracked(|b| b.root.clone());
        if !root.is_empty()
            && let Ok(rel) = std::path::Path::new(path_part).strip_prefix(&root)
        {
            let rel_str = rel.to_string_lossy().replace('\\', "/");
            if !rel_str.is_empty() {
                open_search_hit(rel_str);
                return;
            }
        }
        open_external_target(path_part.to_owned());
        return;
    }
    if trimmed.starts_with("http://") || trimmed.starts_with("https://") {
        open_external_target(trimmed.to_owned());
    }
}

/// Render an offline `svg`/`html`/`artifact` block into a local temp HTML file
/// (zero servers, zero external URLs) and open it when clicked.
pub(crate) fn open_offline_artifact(kind: String, title: String, source: String) {
    let doc = if kind == "svg" {
        format!(
            "<!doctype html><meta charset=\"utf-8\"><title>{title}</title>\
             <style>body{{margin:0;padding:24px;background:#111318;color:#e5e7eb;display:flex;justify-content:center;align-items:center;min-height:100vh;font-family:system-ui}}svg{{max-width:100%;height:auto}}</style>{source}"
        )
    } else if source.to_ascii_lowercase().contains("<html") {
        source
    } else {
        format!(
            "<!doctype html><meta charset=\"utf-8\"><title>{title}</title>\
             <style>:root{{--v-bg:#0f1117;--v-surface:#161a23;--v-surface-2:#1d2230;--v-ink:#e6e8ef;--v-muted:#9aa3b2;--v-line:rgba(255,255,255,.1);--v-accent:#60a5fa;--v-good:#34d399;--v-warn:#fbbf24;--v-bad:#f87171}}body{{margin:0;padding:20px;background:var(--v-bg);color:var(--v-ink);font-family:system-ui,-apple-system,sans-serif}}.tnum{{font-variant-numeric:tabular-nums}}</style>{source}"
        )
    };
    let path = std::env::temp_dir().join(format!(
        "vantry-artifact-{}.html",
        uuid::Uuid::new_v4().simple()
    ));
    if std::fs::write(&path, doc.as_bytes()).is_ok() {
        open_external_target(path.display().to_string());
    }
}

fn open_external_target(target: String) {
    std::thread::spawn(move || {
        #[cfg(target_os = "macos")]
        let _ = std::process::Command::new("open").arg(&target).status();
        #[cfg(target_os = "linux")]
        let _ = std::process::Command::new("xdg-open").arg(&target).status();
        #[cfg(target_os = "windows")]
        let _ = std::process::Command::new("cmd")
            .args(["/C", "start", "", &target])
            .status();
    });
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

pub(crate) fn open_child(child: String) {
    let Some(parent) = state().with_untracked(|s| s.selected.clone()) else {
        return;
    };
    ui().update(|u| {
        u.child_panel = Some((parent, child));
        u.dock_open = true;
        u.dock_maximized = false;
        u.view = MainView::Conversation;
    });
}

pub(crate) fn submit_child(child: &str) {
    intent(|s| s.submit_child(child));
}

pub(crate) fn delegate_child(message: String) {
    let Some(core) = core() else { return };
    let Some(run) = state().with_untracked(|s| {
        s.runs
            .iter()
            .filter(|r| Some(&r.thread_id) == s.selected.as_ref())
            .max_by_key(|r| &r.created_at)
            .cloned()
    }) else {
        return;
    };
    let parent = run.thread_id.clone();
    let mut cmd = Command::new(
        bridge::new_command_id("delegate"),
        vantry_protocol::CommandKind::DelegateTask,
    );
    cmd.thread_id = parent.clone();
    cmd.parent_run_id = run.id;
    cmd.parent_attempt_id = run.attempt_id;
    cmd.message = message;
    cmd.resume_policy =
        ui().with_untracked(|u| if u.manual_delivery { "manual" } else { "auto" }.into());
    spawn_for_window(async move {
        match call(core.dispatch(cmd)).await {
            Ok(r) if state().with_untracked(|s| s.selected.as_ref() == Some(&parent)) => {
                open_child(r.thread_id)
            }
            Ok(_) => {}
            Err(e) => warn(e.message),
        }
    });
}

pub(crate) fn select_thread(id: String) {
    if state().with_untracked(|s|s.tasks.iter().any(|t|Some(&t.parent_thread_id)==s.selected.as_ref() && t.child_thread_id==id)) { open_child(id); return; }
    if ui().with_untracked(|u|u.child_panel.is_some()){close_dock();}
    state().update(move |s| s.select_thread(Some(id)));
    ui().update(|u| {
        u.mobile_nav = false;
        if u.view != MainView::Conversation {
            u.view = MainView::Conversation;
        }
    });
}

pub(crate) fn new_thread() {
    state().update(AppState::start_local_draft);
    ui().update(|u| {
        u.mobile_nav = false;
        u.view = MainView::Conversation;
    });
}

pub(crate) fn choose_project() {
    spawn_for_window(async {
        match pick_paths(nsh_core::file_dialog::OpenDialog::folders().title("添加项目")).await {
            Ok(Some(paths)) => {
                if let Some(path) = paths.first() {
                    start_in_workspace(Some(vantry_protocol::WorkspaceSpec {
                        kind: "directory".into(),
                        path: path.to_string_lossy().into_owned(),
                        ..Default::default()
                    }));
                }
            }
            Ok(None) => {}
            Err(error) => warn(format!("无法打开项目选择器：{error}")),
        }
    });
}

pub(crate) fn start_in_workspace(workspace: Option<vantry_protocol::WorkspaceSpec>) {
    state().update(|s| {
        s.start_local_draft();
        s.new_thread.workspace = workspace;
    });
    ui().update(|u| {
        u.view = MainView::Conversation;
        u.mobile_nav = false;
        u.filter.project = None;
    });
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

pub(crate) fn copy_thread_link(id: String) {
    let title = state().with_untracked(|s| s.thread(&id).map(crate::state::sidebar::display_title));
    let Some(title) = title else { return };
    let mut url = format!("{}{id}", vantry_protocol::THREAD_SCHEME);
    if let Some(peer) = bridges(|b| b.peer_id.clone()) {
        url.push_str(&format!("?environmentId={peer}"));
    }
    let escape = |s: &str| {
        s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;").replace('"', "&quot;")
    };
    let mut item = ClipboardItem::default();
    item.text = vantry_protocol::thread_markdown(&title, &url);
    item.html = Some(format!("<a href=\"{}\">{}</a>", escape(&url), escape(&title)));
    set_clipboard_item(item);
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

pub(crate) async fn composer_skills(thread_id: String, workspace: String) -> Vec<(String,String)> {
    let Some(bridge) = workbench() else { return Vec::new() };
    let request = WorkbenchRequest { action: "skills.list".into(), thread_id, workspace, ..Default::default() };
    let result = call(bridge.workbench(request)).await;
    let result = match result {
        Ok(value) => value,
        Err(err) => { warn(format!("读取 Pi 技能失败：{}", err.message)); return Vec::new(); }
    };
    let catalog = serde_json::from_str::<serde_json::Value>(&result.output).unwrap_or_default();
    catalog["skills"].as_array().into_iter().flatten().filter_map(|skill| {
        Some((skill["name"].as_str()?.to_owned(), skill["description"].as_str().unwrap_or_default().to_owned()))
    }).collect()
}

pub(crate) fn paste_images(paste: &PasteEvent) {
    use base64::Engine;
    let Some(bridge) = workbench() else { return };
    let key = state().with_untracked(|s| s.draft_key());
    let mut images = Vec::new();
    for file in paste.files.iter().filter(|f| f.mime.starts_with("image/")) {
        if !matches!(file.mime.as_str(), "image/png" | "image/jpeg" | "image/gif" | "image/webp") {
            warn("请粘贴 PNG、JPEG、GIF 或 WebP 图片。");
            continue;
        }
        if !(1..=8 << 20).contains(&file.data.len()) { warn("图片须在 8 MB 以内。"); continue; }
        images.push((file.name.clone(),file.data.clone()));
    }
    if images.is_empty() { return; }
    state().update(|s| s.begin_attachment_import(&key));
    spawn_for_window(async move {
        for (name, bytes) in images {
            let mut req = actions::request(names::ATTACHMENT_IMPORT);
            req.name = name;
            req.data = base64::engine::general_purpose::STANDARD.encode(bytes);
            match call(bridge.workbench(req)).await {
                Ok(response) => match response.attachment {
                    Some(item) => state().update(|s| s.add_attachment_to(key.clone(), item, None)),
                    None => warn("Core 没有返回图片附件。"),
                },
                Err(err) => warn(format!("无法粘贴图片：{}", err.message)),
            }
        }
        state().update(|s| s.finish_attachment_import(&key));
    });
}

/// Picks files and attaches them to the shown draft.
pub(crate) fn pick_attachments() {
    let Some(workbench) = workbench() else {
        return;
    };
    if state().with_untracked(|s| s.attachment_imports.contains_key(&s.draft_key())) {
        return;
    }
    let key = state().with_untracked(|s| s.draft_key());
    state().update(|s| {
        s.begin_attachment_import(&key);
    });
    spawn_for_window(async move {
        let picked = pick_paths(
            nsh_core::file_dialog::OpenDialog::files()
                .multiple(true)
                .title("添加附件"),
        )
        .await;
        match picked {
            Ok(Some(paths)) => {
                for path in paths {
                    match call(workbench.attach(path.clone())).await {
                        Ok(attachment) => state()
                            .update(|s| s.add_attachment_to(key.clone(), attachment, Some(path))),
                        Err(error) => warn(format!("无法附加文件：{}", error.message)),
                    }
                }
            }
            Ok(None) => {}
            Err(error) => warn(format!("无法打开文件选择器：{error}")),
        }
        state().update(|s| {
            s.finish_attachment_import(&key);
        });
    });
}

pub(crate) async fn attachment_preview(
    item: vantry_protocol::Attachment,
    source: Option<PathBuf>,
) -> Option<String> {
    if !item.mime.starts_with("image/") || !(1..=(8 << 20)).contains(&item.size) {
        return None;
    }
    if let Some(workbench) = workbench() {
        let mut request = actions::request(names::ATTACHMENT_READ);
        request.input = Some(serde_json::to_value(&item).ok()?);
        if let Ok(response) = call(workbench.workbench(request)).await
            && !response.data.is_empty()
        {
            return Some(format!("data:{};base64,{}", item.mime, response.data));
        }
    }
    source.and_then(|path| path.to_str().map(nsh_core::platform::clipboard::file_url))
}

pub(crate) fn remove_attachment(id: String) {
    state().update(move |s| s.remove_attachment(&id));
}

// --------------------------------------------------------------------- ui

pub(crate) fn narrow_window() -> bool {
    nsh_core::element::window().width.0 < crate::theme::metrics::NARROW_WIDTH
}

pub(crate) fn sidebar_visible() -> bool {
    let narrow = narrow_window();
    ui().with(|u| if narrow { u.mobile_nav } else { u.sidebar_open })
}

pub(crate) fn toggle_sidebar() {
    let narrow = narrow_window();
    ui().update(|u| {
        if narrow {
            u.mobile_nav = !u.mobile_nav
        } else {
            u.sidebar_open = !u.sidebar_open
        }
    });
}

pub(crate) fn search_focus_nonce() -> Signal<u64> {
    global::<u64>()
}

pub(crate) fn focus_search() {
    let narrow = narrow_window();
    ui().update(|u| {
        if narrow {
            u.mobile_nav = true
        } else {
            u.sidebar_open = true
        }
    });
    let nonce = search_focus_nonce();
    nonce.set(nonce.get_untracked().wrapping_add(1));
}

pub(crate) fn quote_to_draft(snippet: String) {
    let trimmed = snippet.trim();
    if trimmed.is_empty() {
        return;
    }
    let quoted: String = trimmed
        .lines()
        .take(6)
        .map(|l| format!("> {l}"))
        .collect::<Vec<_>>()
        .join("\n");
    let current = state().with_untracked(|s| s.draft().text);
    let next = if current.trim().is_empty() {
        format!("{quoted}\n\n")
    } else {
        format!("{current}\n\n{quoted}\n\n")
    };
    set_draft(&next);
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
    let workspace = bench().with_untracked(|b| b.root.clone());
    spawn_for_window(async move {
        let result = call(workbench.settings(workspace)).await.map_err(|e| e.message);
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

pub(crate) fn show_view(view: MainView) {
    let remote = view == MainView::RemoteDesktop;
    ui().update(move |u| {
        u.mobile_nav = false;
        u.view = view;
    });
    if remote {
        refresh_rd_status();
    }
}

pub(crate) fn open_plugin(id: String, page: String) {
    let mut load = false;
    ui().update(|u| {
        u.mobile_nav = false;
        load = u.open_plugin(&id, &page);
    });
    if load {
        load_plugin_panel();
    } else if ui().with_untracked(|u| u.view == MainView::RemoteDesktop) {
        refresh_rd_status();
    }
}

// ------------------------------------------------------------- settings ops

/// Runs workbench action `request` as settings operation `key`: its
/// buttons wait, its answer updates the shown catalogs, `done` (or the
/// Core's message) is the feedback line.
pub(crate) fn settings_op(key: impl Into<String>, mut request: WorkbenchRequest, done: &'static str) {
    if request.action.starts_with("mcp.") {request.workspace=bench().with_untracked(|b|b.root.clone());}
    let key = key.into();
    let Some(workbench) = workbench() else {
        return;
    };
    let mut go = false;
    ui().update(|u| go = u.begin_op(&key));
    if !go {
        return;
    }
    let action = request.action.clone();
    spawn_for_window(async move {
        let result = call(workbench.workbench(request)).await.map_err(|e| e.message);
        ui().update(move |u| u.end_op(&key, &action, result, done));
    });
}

pub(crate) fn dismiss_feedback() {
    ui().update(|u| u.forms.feedback = None);
}

pub(crate) fn mcp_set_enabled(name: String, scope:String, enabled: bool) {
    let Some(workbench) = workbench() else {
        return;
    };
    let key = format!("mcp:{name}");
    let mut go = false;
    // The switch moves at once; a refusal moves it back.
    let mut flipped = false;
    ui().update(|u| {
        go = u.begin_op(&key);
        if go && u.mcp_enabled(&name, true) != enabled {
            u.toggle_mcp(&name);
            flipped = true;
        }
    });
    if !go {
        return;
    }
    let mut request = actions::mcp_set_enabled(&name, enabled);
    request.workspace=bench().with_untracked(|b|b.root.clone());
    request.input.as_mut().expect("mcp.upsert has input")["scope"]=scope.into();
    spawn_for_window(async move {
        let result = call(workbench.workbench(request)).await.map_err(|e| e.message);
        let failed = result.is_err();
        ui().update(move |u| {
            if failed && flipped {
                u.toggle_mcp(&name);
            }
            u.end_op(
                &key,
                names::MCP_UPSERT,
                result,
                if enabled { "已启用 MCP 服务器" } else { "已停用 MCP 服务器" },
            );
        });
    });
}

pub(crate) fn mcp_probe(name: String) {
    settings_op(format!("mcp.probe:{name}"), actions::mcp_named(names::MCP_PROBE, &name), "探活完成");
}

pub(crate) fn mcp_remove(name: String,scope:String) {
    let mut request=actions::mcp_named(names::MCP_REMOVE,&name);request.input=Some(serde_json::json!({"scope":scope}));
    settings_op(format!("mcp:{name}"), request, "已删除 MCP 服务器");
}

pub(crate) fn mcp_trust() {
    let mut request=actions::request(names::MCP_TRUST);request.input=Some(serde_json::json!({"trusted":true}));
    settings_op("mcp.trust",request,"已信任项目 MCP 配置");
}

/// Opens (or closes) the add-server form.
pub(crate) fn mcp_draft_toggle() {
    ui().update(|u| {
        u.forms.mcp_draft = match u.forms.mcp_draft {
            Some(_) => None,
            None => Some(actions::McpServerSpec {
                transport: "stdio".into(),
                enabled: true,
                scope: "user".into(),
                ..Default::default()
            }),
        }
    });
}

pub(crate) fn mcp_draft_update(f: impl FnOnce(&mut actions::McpServerSpec) + 'static) {
    ui().update(move |u| {
        if let Some(draft) = u.forms.mcp_draft.as_mut() {
            f(draft);
        }
    });
}

pub(crate) fn mcp_draft_save() {
    let Some(draft) = ui().with_untracked(|u| u.forms.mcp_draft.clone()) else {
        return;
    };
    if let Some(problem) = draft.problem() {
        ui().update(move |u| {
            u.forms.feedback = Some(crate::state::ui::Feedback { ok: false, text: problem.to_owned() })
        });
        return;
    }
    ui().update(|u| u.forms.mcp_draft = None);
    settings_op("mcp.add", actions::mcp_upsert(&draft), "已保存 MCP 服务器");
}

pub(crate) fn plugin_set_enabled(id: String, enabled: bool) {
    settings_op(
        format!("plugin:{id}"),
        actions::plugin_toggle(&id, enabled),
        if enabled { "已启用插件" } else { "已停用插件" },
    );
}

pub(crate) fn sandbox_set(mode: String) {
    settings_op("sandbox", actions::sandbox_set(&mode), "已切换沙盒模式");
}

pub(crate) fn pair_create() {
    settings_op("pair.create", actions::request(names::REMOTE_PAIR_CREATE), "已生成一次性配对码");
}

pub(crate) fn set_pair_field(code: bool, text: &str) {
    let text = text.to_owned();
    ui().update(move |u| {
        let field = if code { &mut u.forms.pair_code } else { &mut u.forms.pair_name };
        if *field != text {
            *field = text;
        }
    });
}

pub(crate) fn pair_redeem() {
    let (code, name) = ui().with_untracked(|u| (u.forms.pair_code.clone(), u.forms.pair_name.clone()));
    if code.trim().is_empty() {
        ui().update(|u| {
            u.forms.feedback = Some(crate::state::ui::Feedback { ok: false, text: "请输入配对码或连接链接".into() })
        });
        return;
    }
    // The code is single use: it leaves the form as it is sent.
    ui().update(|u| u.forms.pair_code.clear());
    settings_op("pair.redeem", actions::pair_redeem(&code, &name), "配对成功");
}

pub(crate) fn ask_revoke(id: Option<String>) {
    ui().update(move |u| u.forms.confirm_revoke = id);
}

pub(crate) fn peer_revoke(id: String) {
    ui().update(|u| u.forms.confirm_revoke = None);
    settings_op(format!("revoke:{id}"), actions::peer_revoke(&id), "已撤销配对");
}

pub(crate) fn environment_remove(id: String) {
    settings_op(format!("env:{id}"), actions::named(names::ENVIRONMENTS_REMOVE, &id), "已移除环境");
}

pub(crate) fn project_remove(id: String) {
    settings_op(format!("project:{id}"), actions::named(names::PROJECTS_REMOVE, &id), "已移除项目");
}

/// Picks a folder and adds it as a project.
pub(crate) fn project_add() {
    spawn(async move {
        let picked = pick_paths(
            nsh_core::file_dialog::OpenDialog::folders().title("添加项目目录"),
        )
        .await;
        match picked {
            Ok(Some(paths)) => {
                for path in paths {
                    settings_op(
                        "project.add",
                        actions::project_add(&path.to_string_lossy()),
                        "已添加项目",
                    );
                }
            }
            Ok(None) => {}
            Err(error) => warn(format!("无法打开文件夹选择器：{error}")),
        }
    });
}

pub(crate) fn update_op(action: &'static str) {
    let done = match action {
        names::UPDATE_CHECK => "检查完成",
        names::UPDATE_DOWNLOAD => "更新已下载",
        _ => "正在安装更新",
    };
    settings_op("update", actions::request(action), done);
}

pub(crate) fn usage_refresh() {
    settings_op("usage", actions::request(names::USAGE_SUMMARY), "");
}

pub(crate) fn set_pi_field(field: u8, text: &str) {
    let text = text.to_owned();
    ui().update(move |u| match field {
        0 => u.forms.pi_provider = text,
        1 => u.forms.pi_key = crate::state::ui::Secret::new(text),
        _ => u.forms.pi_model = text,
    });
}

/// Saves the Pi quick setup; the key leaves the desktop's memory as it is
/// sent.
pub(crate) fn toggle_pi_setting(enable_user_extensions: Option<bool>, enable_user_mcp: Option<bool>) {
    let mut input = serde_json::Map::new();
    if let Some(v) = enable_user_extensions {
        input.insert("enableUserExtensions".into(), v.into());
    }
    if let Some(v) = enable_user_mcp {
        input.insert("enableUserMCP".into(), v.into());
    }
    let mut request = actions::request("pi.settings.set");
    request.input = Some(serde_json::Value::Object(input));
    settings_op("pi.settings", request, "Pi 运行时开关已更新");
}

pub(crate) fn pi_setup_save() {
    let mut request = None;
    ui().update(|u| {
        let forms = &mut u.forms;
        if forms.pi_provider.trim().is_empty() || forms.pi_key.is_empty() {
            forms.feedback = Some(crate::state::ui::Feedback {
                ok: false,
                text: "请填写 Provider 与 API Key".into(),
            });
            return;
        }
        request = Some(actions::pi_setup_save(&forms.pi_provider, forms.pi_key.expose(), &forms.pi_model));
        forms.pi_key.clear();
    });
    if let Some(request) = request {
        settings_op("pi.setup", request, "Pi 配置已保存");
    }
}

// ------------------------------------------------------------- plugin panel

fn load_plugin_panel() {
    let Some(workbench) = workbench() else {
        return;
    };
    let Some((id, page)) = ui().with_untracked(|u| {
        u.plugin_panel.as_ref().map(|p| (p.plugin_id.clone(), p.page.clone()))
    }) else {
        return;
    };
    spawn_for_window(async move {
        let result = call(workbench.workbench(actions::plugin_panel(&id, &page)))
            .await
            .map(|r| r.output)
            .map_err(|e| e.message);
        ui().update(move |u| {
            if let Some(panel) = u.plugin_panel.as_mut().filter(|p| p.plugin_id == id && p.page == page) {
                panel.on_loaded(result);
            }
        });
    });
}

pub(crate) fn reload_plugin_panel() {
    ui().update(|u| {
        if let Some(panel) = u.plugin_panel.as_mut() {
            panel.loading = true;
            panel.error = None;
        }
    });
    load_plugin_panel();
}

pub(crate) fn set_panel_value(key: String, value: &str) {
    if ui().with_untracked(|u| {
        u.plugin_panel.as_ref().is_some_and(|p| p.values.get(&key).map(String::as_str) == Some(value))
    }) {
        return;
    }
    let value = value.to_owned();
    ui().update(move |u| {
        if let Some(panel) = u.plugin_panel.as_mut() {
            panel.values.insert(key, value);
        }
    });
}

/// Runs the shown panel's action `action_id` (`plugins.run`).
pub(crate) fn run_panel_action(action_id: String) {
    let Some(workbench) = workbench() else {
        return;
    };
    let mut run = None;
    ui().update(|u| {
        if let Some(panel) = u.plugin_panel.as_mut()
            && let Some(input) = panel.begin_run(&action_id)
        {
            run = Some(actions::plugin_run(&panel.plugin_id, &panel.page, input));
        }
    });
    let Some(request) = run else {
        return;
    };
    let id = request.plugin_id.clone();
    spawn(async move {
        let result = call(workbench.workbench(request)).await.map(|r| r.output).map_err(|e| e.message);
        ui().update(move |u| {
            if let Some(panel) = u.plugin_panel.as_mut().filter(|p| p.plugin_id == id) {
                panel.on_ran(&action_id, result);
            }
        });
    });
}

// ---------------------------------------------------------- remote desktop

/// Connects the remote desktop view to `peer` (`rd.session.start` with
/// the toolbar's resolution, quality, control and clipboard).
pub(crate) fn remote_connect(peer: String) {
    let Some(workbench) = workbench() else {
        return;
    };
    let mut start = None;
    ui().update(|u| {
        if u.remote.connect(&peer) {
            start = Some(u.remote.session_options(&peer));
        }
    });
    let Some(options) = start else {
        return;
    };
    spawn(async move {
        let result = call(workbench.workbench(actions::rd_session_start(&options))).await;
        let connected_ok = result.as_ref().is_ok_and(|response| {
            actions::RdStatus::read(response)
                .map(|s| s.session_error.is_empty())
                .unwrap_or(true)
        });
        ui().update(move |u| match result {
            Ok(response) => {
                let status = actions::RdStatus::read(&response).unwrap_or_default();
                let outcome = if status.session_error.is_empty() {
                    Ok(())
                } else {
                    Err(status.session_error.clone())
                };
                u.remote.on_result(&peer, outcome);
                u.remote.status = Some(status);
            }
            Err(error) => u.remote.on_result(&peer, Err(error.message)),
        });
        if connected_ok {
            start_rd_frame_loop(workbench);
        }
    });
}

fn start_rd_frame_loop(workbench: Arc<dyn WorkbenchBridge>) {
    spawn(async move {
        loop {
            let active = ui().with_untracked(|u| {
                matches!(u.remote.link, crate::state::ui::RemoteLink::Connected { .. })
            });
            if !active {
                break;
            }
            if let Ok(resp) = call(workbench.workbench(actions::request("rd.viewer.frame"))).await
                && let Ok(val) = serde_json::from_str::<serde_json::Value>(&resp.output)
            {
                let seq = val["seq"].as_u64().unwrap_or(0);
                if seq > 0
                    && let Some(uri) = val["dataUri"].as_str()
                {
                    let uri = uri.to_owned();
                    let w = val["width"].as_u64().unwrap_or(1280) as u32;
                    let h = val["height"].as_u64().unwrap_or(800) as u32;
                    ui().update(move |u| {
                        if seq > u.remote.frame_seq {
                            u.remote.frame_seq = seq;
                            u.remote.frame_size = (w, h);
                            u.remote.frame_uri = Some(uri);
                        }
                    });
                }
            }
            tokio::time::sleep(Duration::from_millis(80)).await;
        }
    });
}

pub(crate) fn remote_send_input(event: serde_json::Value) {
    let allow = ui().with_untracked(|u| {
        u.remote.control && matches!(u.remote.link, crate::state::ui::RemoteLink::Connected { .. })
    });
    if !allow {
        return;
    }
    let Some(workbench) = workbench() else {
        return;
    };
    let mut req = actions::request("rd.viewer.input");
    req.input = Some(event);
    spawn(async move {
        let _ = call(workbench.workbench(req)).await;
    });
}

pub(crate) fn remote_host_toggle() {
    let Some(workbench) = workbench() else {
        return;
    };
    let sharing = ui().with_untracked(|u| !u.remote.host_lan_link.is_empty());
    spawn(async move {
        if sharing {
            let _ = call(workbench.workbench(actions::request(names::RD_SESSION_STOP))).await;
            ui().update(|u| {
                u.remote.host_lan_link.clear();
            });
        } else {
            let mut req = actions::request(names::RD_SESSION_START);
            req.input = Some(serde_json::json!({"capture": true}));
            if let Ok(resp) = call(workbench.workbench(req)).await
                && let Ok(v) = serde_json::from_str::<serde_json::Value>(&resp.output)
            {
                let link = v["lanLink"].as_str().unwrap_or("").to_owned();
                ui().update(move |u| {
                    u.remote.host_lan_link = link;
                });
            }
        }
    });
}

/// Ends the session (`rd.session.stop`); the view returns to the picker
/// whatever the Core answers.
pub(crate) fn remote_disconnect() {
    let peer = ui().with_untracked(|u| u.remote.peer().map(str::to_owned));
    ui().update(|u| {
        u.remote.disconnect();
        u.remote.frame_uri = None;
        u.remote.frame_seq = 0;
    });
    let (Some(peer), Some(workbench)) = (peer, workbench()) else {
        return;
    };
    spawn(async move {
        if let Ok(response) = call(workbench.workbench(actions::rd_session_stop(&peer))).await
            && let Ok(status) = actions::RdStatus::read(&response)
        {
            ui().update(move |u| u.remote.status = Some(status));
        }
    });
}

/// Leaves the remote desktop for the conversation, ending its session.
pub(crate) fn remote_exit() {
    remote_disconnect();
    show_view(MainView::Conversation);
}

/// Reads `rd.status` (permissions, sharing, a session the Core still runs).
pub(crate) fn refresh_rd_status() {
    let Some(workbench) = workbench() else {
        return;
    };
    spawn(async move {
        if let Ok(response) = call(workbench.workbench(actions::request(names::RD_STATUS))).await
            && let Ok(status) = actions::RdStatus::read(&response)
        {
            ui().update(move |u| u.remote.on_status(status));
        }
    });
}

pub(crate) fn remote_update(f: impl FnOnce(&mut crate::state::ui::RemoteDesktopState) + 'static) {
    ui().update(move |u| f(&mut u.remote));
}

// --------------------------------------------------------------------- dock

/// Shows the dock on `tab` (or hides it when it shows that tab).
pub(crate) fn toggle_dock(tab: DockTab) {
    let mut open = false;
    ui().update(|u| {
        u.mobile_nav = false;
        if u.view != MainView::Conversation {
            u.view = MainView::Conversation;
            u.dock_open = false;
        }
        u.toggle_dock_tab(tab);
        open = u.dock_open;
    });
    if open {
        show_tab(tab);
    }
}

pub(crate) fn close_dock() {
    ui().update(|u| {
        u.dock_open = false;
        u.dock_maximized = false;
        u.child_panel = None;
    });
}

pub(crate) fn select_dock_tab(tab: DockTab) {
    ui().update(move |u| {
        u.mobile_nav = false;
        u.view = MainView::Conversation;
        u.dock_open = true;
        u.dock_tab = tab;
    });
    show_tab(tab);
}

fn show_tab(tab: DockTab) {
    ui().update(|u| u.child_panel = None);
    if matches!(tab, DockTab::Plugins | DockTab::Mcp) { load_settings_catalogs(); }
    if matches!(tab, DockTab::Git | DockTab::Files) && bench().with_untracked(|b| b.root.is_empty())
    {
        return;
    }
    let mut loads = Vec::new();
    bench().update(|b| loads = b.on_tab(tab));
    run_loads(loads);
}

/// Runs what the dock asked to load.
fn run_loads(loads: Vec<Load>) {
    let Some((project, workbench)) = project_workbench() else {
        return;
    };
    for load in loads {
        let workbench = workbench.clone();
        spawn_for_window(async move {
            match load {
                Load::Dir(path) => {
                    let result = call(workbench.list_dir(path.clone()))
                        .await
                        .map_err(|e| e.message);
                    bench().update(move |b| {
                        b.apply_project_result(project, |b| b.files.on_dir(&path, result))
                    });
                }
                Load::File(path) => {
                    let result = call(workbench.read_file(path.clone()))
                        .await
                        .map_err(|e| e.message);
                    bench().update(move |b| {
                        b.apply_project_result(project, |b| b.files.on_file(&path, result))
                    });
                }
                Load::GitStatus => {
                    let result = call(workbench.git_status()).await.map_err(|e| e.message);
                    let mut next = None;
                    bench().update(|b| {
                        b.apply_project_result(project, |b| {
                            next = b.git.on_status(result);
                            if next.is_none() && b.git.error.is_none() && b.git.selected.is_none() && let Some(first)=b.git.changes().first() {
                                next=Some(b.git.select(&first.path,first.staged));
                            }
                        })
                    });
                    run_loads(next.into_iter().collect());
                }
                Load::Diff { path, staged } => {
                    let result = call(workbench.git_diff(path.clone(), staged))
                        .await
                        .map_err(|e| e.message);
                    bench().update(move |b| {
                        b.apply_project_result(project, |b| {
                            b.git.on_diff(path.as_deref(), staged, result)
                        })
                    });
                }
                Load::Pr => {
                    let result = call(workbench.pr_summary()).await.map_err(|e| e.message);
                    bench()
                        .update(move |b| b.apply_project_result(project, |b| b.git.on_pr(result)));
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

mod shell;
pub(crate) use shell::*;
mod git_history;
pub(crate) use git_history::*;

/// Shows a reply or document in a read-only dialog.
pub(crate) fn preview_text(title: String, text: String) {
    bench().update(move |b| b.preview.show(&title, &text));
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
    let Some((project, workbench)) = project_workbench() else {
        return;
    };
    let mut go = false;
    bench().update(|b| go = b.git.begin());
    if !go {
        return;
    }
    spawn_for_window(async move {
        let future = if staged {
            workbench.git_unstage(paths)
        } else {
            workbench.git_stage(paths)
        };
        let result = call(future).await.map(|()| None).map_err(|e| e.message);
        let mut next = None;
        bench().update(|b| b.apply_project_result(project, |b| next = Some(b.git.on_done(result))));
        run_loads(next.into_iter().collect());
    });
}

pub(crate) fn commit() {
    let Some((project, workbench)) = project_workbench() else {
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
    spawn_for_window(async move {
        let result = call(workbench.git_commit(message))
            .await
            .map(Some)
            .map_err(|e| e.message);
        let mut next = None;
        bench().update(|b| b.apply_project_result(project, |b| next = Some(b.git.on_done(result))));
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
    if running && !shell_close(&id) {
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
    // If the active tab is already running a PTY process, send the typed line
    // directly to its stdin instead of starting a second command.
    let mut interactive: Option<(String, String)> = None;
    bench().update(|b| {
        if let Some(cur) = b.terminal.current_mut()
            && cur.running
            && !cur.input.is_empty()
        {
            let text = std::mem::take(&mut cur.input);
            cur.output.push_str(&format!("{text}\n"));
            interactive = Some((cur.id.clone(), format!("{text}\n")));
        }
    });
    if let Some((session, line)) = interactive {
        spawn(async move {
            let _ = call(workbench.terminal_write(session, line)).await;
        });
        return;
    }
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

// ------------------------------------------------------------ git extras

/// Switches the workspace's branch.
pub(crate) fn git_checkout(branch: String) {
    let Some((project, workbench)) = project_workbench() else {
        return;
    };
    let mut go = false;
    bench().update(|b| go = b.git.begin());
    if !go {
        return;
    }
    spawn_for_window(async move {
        let result = call(workbench.git_checkout(branch))
            .await
            .map(|()| None)
            .map_err(|e| e.message);
        let mut next = None;
        bench().update(|b| b.apply_project_result(project, |b| next = Some(b.git.on_done(result))));
        let mut loads: Vec<Load> = next.into_iter().collect();
        // The tree may differ on the other branch.
        bench().update(|b| b.apply_project_result(project, |b| loads.extend(b.files.refresh())));
        run_loads(loads);
    });
}

/// Asks before throwing away `path`'s unstaged changes.
pub(crate) fn ask_discard(path: Option<String>) {
    bench().update(move |b| b.git.confirm_discard = path);
}

pub(crate) fn git_discard(paths: Vec<String>) {
    let Some((project, workbench)) = project_workbench() else {
        return;
    };
    let mut go = false;
    bench().update(|b| {
        b.git.confirm_discard = None;
        go = b.git.begin();
    });
    if !go {
        return;
    }
    spawn_for_window(async move {
        let result = call(workbench.git_discard(paths))
            .await
            .map(|()| None)
            .map_err(|e| e.message);
        let mut next = None;
        bench().update(|b| b.apply_project_result(project, |b| next = Some(b.git.on_done(result))));
        run_loads(next.into_iter().collect());
    });
}

pub(crate) fn toggle_branch_menu() {
    bench().update(|b| b.git.branch_menu = !b.git.branch_menu);
}

// ------------------------------------------------------------- file search

/// Searches the workspace (names, then lines) shortly after typing stops.
pub(crate) fn set_file_search(text: &str) {
    if bench().with_untracked(|b| b.files.search.query == text) {
        return;
    }
    let text = text.to_owned();
    let mut generation = 0;
    bench().update(|b| generation = b.files.search.set_query(&text));
    if text.trim().is_empty() {
        return;
    }
    let Some((project, workbench)) = project_workbench() else {
        return;
    };
    set_timeout(Duration::from_millis(220), move || {
        if bench().with_untracked(|b| {
            b.project_generation != project || b.files.search.generation != generation
        }) {
            return;
        }
        let query = text.clone();
        spawn_for_window(async move {
            let result = call(workbench.search_files(query))
                .await
                .map_err(|e| e.message);
            bench().update(move |b| {
                b.apply_project_result(project, |b| b.files.search.on_results(generation, result))
            });
        });
    });
}

/// Opens a search hit in the preview.
pub(crate) fn open_search_hit(path: String) {
    select_file(path);
}

// ----------------------------------------------------------------- composer

/// Cycles the thinking level of the shown model (`model:level`).
#[allow(dead_code)]
pub(crate) fn cycle_thinking() {
    let (levels, models) = ui().with_untracked(|u| u.thinking_catalog());
    if levels.is_empty() {
        load_catalogs(false);
        return;
    }
    let current = state().with_untracked(|s| s.current_model());
    let (base, level) = actions::split_thinking(&current, &models);
    if base.is_empty() {
        return;
    }
    let next = actions::with_thinking(base, &actions::next_thinking(level, &levels));
    intent(move |s| s.set_model(&next));
}

pub(crate) fn set_thinking_level(level: String) {
    let (_, models) = ui().with_untracked(|u| u.thinking_catalog());
    let current = state().with_untracked(|s| s.current_model());
    let (base, _) = actions::split_thinking(&current, &models);
    if base.is_empty() {
        return;
    }
    let next = actions::with_thinking(base, &level);
    intent(move |s| s.set_model(&next));
}

/// Escape in the window: closes the model picker, else stops the shown
/// thread's run (when nothing modal is open).
pub(crate) fn escape() {
    let (picker, modal_ui) = ui().with_untracked(|u| {
        (
            u.model_picker,
            u.settings.is_some() || u.rename.is_some() || u.confirm.is_some(),
        )
    });
    if picker {
        toggle_model_picker();
        return;
    }
    if modal_ui || state().with_untracked(|s| s.modal_request().is_some()) {
        return;
    }
    if ui().with_untracked(|u| u.dock_open && u.dock_tab != DockTab::Terminal) { close_dock(); return; }
    if narrow_window() && ui().with_untracked(|u| u.mobile_nav) {
        ui().update(|u| u.mobile_nav = false);
        return;
    }
    if state().with_untracked(|s| s.composer_status().can_stop) {
        stop();
    }
}
