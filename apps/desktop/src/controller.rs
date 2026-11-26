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

pub(crate) mod machines;

fn session() -> Option<machines::Session> { use_context() }

/// The conversation.
pub(crate) fn state() -> Signal<AppState> {
    session().map(|c| c.state).unwrap_or_else(global::<AppState>)
}

/// The window.
pub(crate) fn ui() -> Signal<UiState> {
    session().map(|c| c.ui).unwrap_or_else(global::<UiState>)
}

/// The dock.
pub(crate) fn bench() -> Signal<WorkbenchState> {
    session().map(|c| c.bench).unwrap_or_else(global::<WorkbenchState>)
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
    session().map(|c| c.bridges).unwrap_or_else(global::<Bridges>).with_untracked(f)
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

/// The local machine's workbench, even when the user is currently viewing a
/// paired remote machine (desktop app updates always apply to this machine).
fn local_workbench() -> Option<(Arc<dyn WorkbenchBridge>, bool)> {
    machines::by_id("")
        .and_then(|m| {
            m.session
                .bridges
                .with_untracked(|b| b.workbench.clone().map(|w| (w, b.mock)))
        })
        .or_else(|| bridges(|b| b.workbench.clone().map(|w| (w, b.mock))))
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
    session().map(|c| c.bridges).unwrap_or_else(global::<Bridges>).set(Bridges {
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
    let label = current_machine().and_then(|id| machines::by_id(&id)).map_or_else(|| core.label(), |m| m.name);
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
    let scope = current_machine().unwrap_or_default();
    effect(move || {
        if machines::navigation().with(|n| n.active != scope) { return; }
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
    let needs_boot = match call(core.health()).await {
        Err(BridgeError { code: ErrorCode::Unavailable, .. }) => true,
        Ok(_) => matches!(
            background(autostart::needs_upgrade_restart(core.clone())).await,
            Ok(true)
        ),
        Err(_) => false,
    };
    if needs_boot {
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

#[derive(Default, Clone, PartialEq)]
struct SaveSlot {
    last: Option<persist::DesktopStateFile>,
    generation: u64,
}


/// Saves `file` after [`SAVE_AFTER`] unless a newer one replaces it.
fn schedule_save(file: persist::DesktopStateFile) {
    let slot = session().map(|s| s.saves).unwrap_or_else(global::<SaveSlot>);
    let mut generation = None;
    slot.update(|s| {
        if s.last.as_ref() == Some(&file) { return; }
        s.generation = s.generation.wrapping_add(1);
        if s.last.is_some() { generation = Some(s.generation); }
        s.last = Some(file.clone());
    });
    let Some(generation) = generation else {
        return;
    };
    let Some(path) = bridges(|b| b.state_path.clone()) else {
        return;
    };
    spawn_for_window(async move {
        timer(SAVE_AFTER).await;
        let current = slot.with_untracked(|s| s.generation);
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

async fn read_projection<T: Send + 'static>(
    future: bridge::BridgeFuture<T>,
) -> Result<T, BridgeError> {
    call(Box::pin(async move {
        tokio::time::timeout(vantry_protocol::EVENT_IDLE_TIMEOUT, future)
            .await
            .map_err(|_| BridgeError::unavailable("读取 Core 状态超时"))?
    }))
    .await
}

/// Loads the snapshot, follows the event stream, and recovers: resubscribes
/// after the last applied event, or reloads the snapshot on
/// `resync_required`, backing off between attempts.
async fn run_connection(core: Arc<dyn CoreBridge>) {
    let mut need_snapshot = true;
    loop {
        if need_snapshot {
            // Fast path: load cheap sidebar summaries + bounded detail for the
            // active thread, then subscribe directly to `/v1/events` without
            // downloading the multi-megabyte full `/v1/snapshot`. Fall back to
            // `/v1/snapshot` only on older Cores that do not serve summaries.
            match read_projection(core.thread_summaries()).await {
                Ok(summaries) => {
                    let mut selected = None;
                    state().update(|s| {
                        s.apply_summaries(summaries);
                        selected = s.selected.clone();
                    });
                    if let Some(id) = selected {
                        let mut request = 0;
                        state().update(|s| request = s.begin_detail_load(&id));
                        load_detail(&core, id, request).await;
                    }
                    state().update(AppState::on_live);
                    need_snapshot = false;
                    catalogs_after_connect();
                }
                Err(error) if error.code == ErrorCode::NotFound => {
                    match read_projection(core.snapshot()).await {
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
                            if let Some(
                                Recovery::Resync { delay } | Recovery::Resubscribe { delay, .. },
                            ) = recovery
                            {
                                timer(delay).await;
                            }
                            continue;
                        }
                    }
                }
                Err(error) => {
                    if error.code == ErrorCode::Unauthorized && let Some(peer) = bridges(|b| b.peer_id.clone()) {
                        if machines::navigation().with_untracked(|n| n.active == peer) && ui().with_untracked(|u| u.forms.repair_peer != peer) {
                            ui().update(|u| { u.forms.repair_peer = peer; u.forms.pair_dialog = true; });
                        }
                    }
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
                        acknowledge_visible_completion(&batch);
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
            Some(Recovery::Resubscribe { delay, .. }) => {
                timer(delay).await;
                let selected = state().with_untracked(|s| {
                    s.selected.clone().filter(|_| !s.snapshot_loaded)
                });
                if let Some(id) = selected {
                    let mut request = 0;
                    state().update(|s| request = s.begin_detail_load(&id));
                    let core = core.clone();
                    spawn_for_window(async move { load_detail(&core, id, request).await });
                }
            }
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
        acknowledge_visible_completion(&batch);
    }
}

fn acknowledge_visible_completion(events: &[CoreEvent]) {
    if machines::navigation().with_untracked(|n| Some(&n.active).filter(|id| !id.is_empty()).cloned() != current_machine()) || !ui().with_untracked(|u| u.view == MainView::Conversation) { return; }
    let selected = state().with_untracked(|s| {
        let thread = s.selected_thread()?;
        let seq = thread.unread_sequence?;
        events.iter().any(|event| event.thread_id == thread.id && event.sequence == seq
            && matches!(event.r#type.as_str(), "run.completed" | "run.failed" | "run.interrupted"))
            .then(|| thread.id.clone())
    });
    if let Some(id) = selected { set_thread_read_state(&id, false); }
}

async fn load_detail(core: &Arc<dyn CoreBridge>, id: String, request: u64) {
    while state().with_untracked(|s| s.current_detail_request(request, &id)) {
        let full = state().with_untracked(|s| s.snapshot_loaded);
        let mut stale = false;
        let result = if full {
            read_projection(core.snapshot()).await.map(|snapshot| {
                state().update(|s| {
                    stale = snapshot.sequence < s.sequence;
                    if !stale && s.current_detail_request(request, &id) {
                        s.apply_snapshot(snapshot);
                    }
                });
            })
        } else {
            read_projection(core.thread_detail(id.clone(), 20))
                .await
                .map(|detail| {
                    state().update(|s| {
                        stale = detail.sequence < s.sequence;
                        s.apply_thread_detail(request, detail);
                    });
                })
        };
        if stale {
            timer(Duration::from_millis(150)).await;
            continue;
        }
        if let Err(error) = result {
            state().update(|s| s.detail_load_failed(request, &id, &error));
        }
        break;
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
        let kind = command.kind;
        let thread = command.thread_id.clone();
        let result = call(core.dispatch(command)).await;
        let mut follow_up = None;
        let mut refresh = None;
        state().update(|s| {
            refresh = s.begin_conflict_refresh(kind, &thread, &result);
            follow_up = s.on_command_result(&id, result);
        });
        if let Some(request) = refresh {
            load_detail(&core, thread, request).await;
        }
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
    if let Some(att_id) = trimmed.strip_prefix("vantry-attachment://") {
        let att_id = att_id.split(['?', '#']).next().unwrap_or("").trim();
        let lang = ui().with_untracked(|u| u.locale);
        let found = state().with_untracked(|s| {
            // Search runs in the currently selected thread first, then all runs, then drafts.
            let find_in = |atts: &[vantry_protocol::Attachment]| -> Option<(vantry_protocol::Attachment, String)> {
                let mut img_seq = 0usize;
                for a in atts {
                    let is_generic = a.mime.starts_with("image/")
                        && crate::state::draft::is_generic_image_name(&a.name);
                    if is_generic {
                        img_seq += 1;
                    }
                    if a.id == att_id {
                        let lbl = if is_generic {
                            lang.image_label(img_seq)
                        } else {
                            crate::state::draft::middle_truncate_name(&a.name, 24)
                        };
                        return Some((a.clone(), lbl));
                    }
                }
                None
            };
            if let Some(tid) = s.selected.as_deref() {
                for run in s.runs.iter().filter(|r| r.thread_id == tid) {
                    if let Some(hit) = find_in(&run.attachments) {
                        return Some(hit);
                    }
                }
            }
            for draft in s.drafts.values() {
                if let Some(hit) = find_in(&draft.attachments) {
                    return Some(hit);
                }
            }
            None
        });
        if let Some((item, label)) = found {
            open_attachment_preview(item, label);
        } else {
            warn("找不到该附件。");
        }
        return;
    }
    if let Some(rest) = trimmed.strip_prefix("vantry-thread://") {
        let thread_id = rest.split(['?', '#']).next().unwrap_or("").trim();
        let environment = rest.split_once('?').and_then(|(_, query)| {
            query
                .split('&')
                .find_map(|p| p.strip_prefix("environmentId="))
        });
        let scope = match environment.filter(|v| !v.is_empty()) {
            Some(env) if machines::by_id(env).is_some() => env.to_owned(),
            Some("local") => String::new(),
            Some(env)
                if std::fs::read_to_string(
                    crate::launch_config().data_dir.join("environment-id"),
                )
                .is_ok_and(|local| local.trim() == env) =>
            {
                String::new()
            }
            Some(_) => {
                warn("会话所属主机尚未配对。");
                return;
            }
            None => current_machine().unwrap_or_default(),
        };
        machines::activate(&scope, || {
            if state().with_untracked(|s| s.thread(thread_id).is_some()) {
                select_thread(thread_id.to_owned());
            } else {
                warn("找不到链接中的会话。");
            }
        });
        return;
    }
    if trimmed.starts_with("http://") || trimmed.starts_with("https://") {
        drop(nsh_core::element::open_url(trimmed));
        return;
    }
    let file = if trimmed.starts_with("file://") {
        url::Url::parse(trimmed)
            .ok()
            .and_then(|url| url.to_file_path().ok())
    } else if std::path::Path::new(trimmed).is_absolute() {
        Some(PathBuf::from(trimmed))
    } else {
        None
    };
    if let Some(file) = file {
        let path = file.to_string_lossy();
        let path = path
            .rsplit_once(':')
            .filter(|(_, line)| line.parse::<u32>().is_ok())
            .map_or(path.as_ref(), |(p, _)| p);
        let root = bench().with_untracked(|b| b.root.clone());
        let label = std::path::Path::new(path);
        if !remote_project()
            && label.extension().and_then(|s| s.to_str()).is_some_and(|s| {
                matches!(
                    s.to_ascii_lowercase().as_str(),
                    "png" | "jpeg" | "jpg" | "webp" | "gif"
                )
            })
        {
            drop(nsh_core::element::open_path(label));
        } else if !root.is_empty()
            && let Ok(relative) = label.strip_prefix(root)
            && !relative.as_os_str().is_empty()
        {
            select_dock_tab(DockTab::Files);
            open_search_hit(relative.to_string_lossy().replace('\\', "/"));
        } else if !remote_project() {
            drop(nsh_core::element::open_path(path));
        } else {
            warn("文件位于远端工作区之外。");
        }
    }
}

/// Render an offline `svg`/`html`/`artifact` block into a local temp HTML file
/// (zero servers, zero external URLs) and open it when clicked.
pub(crate) fn open_offline_artifact(kind: String, title: String, source: String) {
    let doc = crate::views::visuals::artifact_document(&kind, &title, &source, 1.0, nsh_core::element::dark());
    let path = std::env::temp_dir().join(format!(
        "vantry-artifact-{}.html",
        uuid::Uuid::new_v4().simple()
    ));
    if std::fs::write(&path, doc.as_bytes()).is_ok() {
        drop(nsh_core::element::open_path(path));
    }
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
        u.expanded_task_parent = Some(parent.clone());
        u.child_panel = Some((parent, child.clone()));
        u.dock_open = true;
        u.dock_maximized = false;
        u.view = MainView::Conversation;
    });
    // Summaries mode carries only the selected thread's runs and tools: load
    // the child's bounded detail beside it, keeping the parent selected.
    if state().with_untracked(|s| s.needs_child_detail(&child))
        && let Some(core) = core()
    {
        spawn_for_window(async move {
            if let Ok(detail) = call(core.thread_detail(child, 20)).await {
                state().update(|s| {
                    s.merge_child_detail(detail);
                });
            }
        });
    }
}

/// Stops child `child`'s active run.
pub(crate) fn stop_child(child: String) {
    intent(move |s| s.stop_thread(&child));
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
    set_thread_read_state(&id, false);
    if state().with_untracked(|s|s.tasks.iter().any(|t|Some(&t.parent_thread_id)==s.selected.as_ref() && t.child_thread_id==id)) { open_child(id); return; }
    if ui().with_untracked(|u|u.child_panel.is_some()){close_dock();}
    let mut request = None;
    state().update(|s| {
        if s.snapshot_loaded {
            s.select_thread(Some(id.clone()));
        } else {
            request = Some(s.begin_detail_load(&id));
        }
    });
    if let Some(request) = request
        && let Some(core) = core()
    {
        let thread_id = id.clone();
        spawn_for_window(async move {
            load_detail(&core, thread_id, request).await;
        });
    }
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

/// Starts a fresh thread bound to `project_key` (matching MyGo's project `+` button).
pub(crate) fn new_thread_in_project(project_key: String) {
    if project_key.is_empty() {
        start_in_workspace(None);
        return;
    }
    let spec = state().with_untracked(|s| {
        s.threads
            .iter()
            .find(|t| {
                crate::state::sidebar::workspace_key(t.workspace.as_ref(), &t.project_path)
                    == project_key
            })
            .and_then(|t| t.workspace.clone())
            .map(|mut w| {
                if w.kind == "worktree" || !w.repository.is_empty() {
                    w.path.clear();
                    w.branch.clear();
                }
                w
            })
            .unwrap_or_else(|| vantry_protocol::WorkspaceSpec {
                kind: "directory".into(),
                path: project_key,
                ..Default::default()
            })
    });
    start_in_workspace(Some(spec));
}

pub(crate) fn new_thread_in_thread_project(id: String) {
    let key = state().with_untracked(|s| {
        s.thread(&id)
            .map(|t| crate::state::sidebar::workspace_key(t.workspace.as_ref(), &t.project_path))
            .unwrap_or_default()
    });
    new_thread_in_project(key);
}

pub(crate) fn copy_thread_path(id: String) {
    let path = state().with_untracked(|s| {
        s.thread(&id)
            .map(|t| crate::state::sidebar::workspace_key(t.workspace.as_ref(), &t.project_path))
            .unwrap_or_default()
    });
    if !path.is_empty() {
        copy(path);
    }
}

pub(crate) fn toggle_unread(id: String) {
    let unread = state().with_untracked(|s| !s.unread.contains(&id));
    set_thread_read_state(&id, unread);
}

pub(crate) fn mark_all_read() {
    let ids = state().with_untracked(|s| s.unread.clone());
    for id in ids { set_thread_read_state(&id, false); }
}

fn set_thread_read_state(id: &str, unread: bool) {
    let observed = state().with_untracked(|s| s.thread(id).and_then(|t| t.unread_sequence));
    if let Some(sequence) = observed {
        if (sequence > 0) == unread { return; }
        let Some(core) = core() else { return; };
        let mut command = Command::new(bridge::new_command_id("read"), vantry_protocol::CommandKind::UpdateThread);
        command.thread_id = id.to_owned();
        command.read_state = Some(vantry_protocol::ThreadReadChange { unread, observed_sequence: sequence });
        spawn_for_window(async move {
            if let Err(error) = call(core.dispatch(command)).await { warn(error.message); }
        });
    } else {
        state().update(|s| {
            if unread { s.unread.insert(id.to_owned()); } else { s.unread.remove(id); }
            s.sidebar_revision += 1;
        });
    }
}

pub(crate) fn open_thread_in_terminal(id: String) {
    select_thread(id);
    select_dock_tab(DockTab::Terminal);
}

pub(crate) fn set_draft(text: &str) {
    // The controlled input echoes its value: skip writes that change nothing.
    if state().with_untracked(|s| s.draft().text == text) {
        return;
    }
    let text = text.to_owned();
    state().update(move |s| s.set_draft_text(&text));
}

/// Sets the draft to pasted `text` whose caret is `caret` (UTF-16 in the
/// body): Markdown thread links in it become `@title` references where they
/// stand. Returns the caret, moved with the text around it.
pub(crate) fn paste_draft_text(text: &str, caret: u32) -> u32 {
    let mut next = caret;
    let text = text.to_owned();
    state().update(|s| {
        s.set_draft_text(&text);
        let key = s.draft_key();
        if let Some(draft) = s.drafts.get_mut(&key) {
            next = draft.collapse_thread_links(Some(caret)).1.unwrap_or(caret);
        }
    });
    note_composer_caret(next);
    next
}

fn caret_position() -> Signal<Option<(String, u32)>> {
    session().map(|s| s.position).unwrap_or_else(global::<Option<(String, u32)>>)
}

/// The composer reports where its caret is (a selection's end).
pub(crate) fn note_composer_caret(caret: u32) {
    let key = state().with_untracked(|s| s.draft_key());
    caret_position().set(Some((key, caret)));
}

/// Where draft `key`'s next reference goes: the caret if the composer
/// reported one for it.
fn composer_caret(key: &str) -> Option<u32> {
    caret_position().with_untracked(|c| {
        c.as_ref()
            .filter(|(k, _)| k == key)
            .map(|(_, caret)| *caret)
    })
}

/// Asks the composer to put its caret at `caret` (draft `key`), after text
/// was inserted for it.
#[derive(Debug, Clone, Default, PartialEq)]
pub(crate) struct CaretRequest {
    pub key: String,
    pub caret: u32,
    pub nonce: u64,
}

pub(crate) fn caret_request() -> Signal<CaretRequest> {
    session().map(|c| c.caret).unwrap_or_else(global::<CaretRequest>)
}

fn move_composer_caret(key: &str, caret: u32) {
    caret_position().set(Some((key.to_owned(), caret)));
    let request = caret_request();
    let nonce = request.get_untracked().nonce.wrapping_add(1);
    request.set(CaretRequest {
        key: key.to_owned(),
        caret,
        nonce,
    });
}

/// Adds an imported attachment to draft `key` with its reference at the
/// composer's caret, which then moves past it.
fn attach_to_draft(key: &str, attachment: vantry_protocol::Attachment, path: Option<PathBuf>) {
    let lang = ui().with_untracked(|u| u.locale);
    let caret = composer_caret(key);
    let mut next = None;
    state().update(|s| {
        next = s.add_attachment_with_mention_for_lang(key.to_owned(), attachment, path, lang, caret)
    });
    if let Some(next) = next {
        move_composer_caret(key, next);
    }
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
    let scope = current_machine().or_else(|| std::fs::read_to_string(crate::launch_config().data_dir.join("environment-id")).ok());
    let mut url = format!("{}{id}", vantry_protocol::THREAD_SCHEME);
    if let Some(scope) = scope.filter(|s| !s.trim().is_empty()) { url.push_str(&format!("?environmentId={}", scope.trim())); }
    let escape = |s: &str| {
        s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;").replace('"', "&quot;")
    };
    let mut item = ClipboardItem::default();
    item.text = vantry_protocol::thread_markdown(&title, &url);
    item.html = Some(format!("<a href=\"{}\">{}</a>", escape(&url), escape(&title)));
    drop(set_clipboard_item(item));
}

/// Copies `text` to the clipboard and says so.
pub(crate) fn copy(text: String) {
    drop(set_clipboard(text));
    state().update(|s| s.notice = Some(Notice::new(NoticeLevel::Info, "已复制。")));
}

/// Converts a `data:` URI or `file://` URL into a base64 `data:` URI accepted by `set_clipboard_image`.
fn ensure_image_data_uri(src: &str) -> Option<String> {
    if src.starts_with("data:") {
        return Some(src.to_owned());
    }
    let path = decode_file_uri(src);
    let bytes = std::fs::read(&path).ok()?;
    let mime = nsh_core::platform::clipboard::image_mime(&bytes).unwrap_or("image/png");
    Some(nsh_core::platform::clipboard::data_uri(mime, &bytes))
}

#[cfg(target_os = "macos")]
const MACOS_PASTEBOARD_SCRIPT: &str = r#"
ObjC.import('AppKit');
ObjC.import('Foundation');
function run(argv) {
  var pb = $.NSPasteboard.generalPasteboard;
  var txt = $.NSString.stringWithContentsOfFileEncodingError(argv[0], $.NSUTF8StringEncoding, $());
  var html = $.NSString.stringWithContentsOfFileEncodingError(argv[1], $.NSUTF8StringEncoding, $());
  var items = [];
  var first = $.NSPasteboardItem.alloc.init;
  if (txt && txt.js) first.setStringForType(txt, $.NSPasteboardTypeString);
  if (html && html.js) first.setStringForType(html, $.NSPasteboardTypeHTML);
  items.push(first);
  for (var i = 2; i < argv.length; i++) {
    var data = $.NSData.dataWithContentsOfFile(argv[i]);
    if (!data || data.length == 0) continue;
    var item = (i == 2) ? first : $.NSPasteboardItem.alloc.init;
    var img = $.NSImage.alloc.initWithData(data);
    var tiff = img ? img.TIFFRepresentation : $();
    var png = data;
    if (tiff && tiff.length > 0) {
      var rep = $.NSBitmapImageRep.imageRepWithData(tiff);
      if (rep) {
        var encoded = rep.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $({}));
        if (encoded && encoded.length > 0) png = encoded;
      }
      item.setDataForType(tiff, $.NSPasteboardTypeTIFF);
    }
    item.setDataForType(png, $.NSPasteboardTypePNG);
    if (i != 2) items.push(item);
  }
  pb.clearContents;
  if (!pb.writeObjects($(items))) throw new Error('pasteboard refused the items');
}
"#;

/// Puts `text`, its `html` and every image on the macOS pasteboard: the
/// text, HTML and first image as one item (a chat app pastes them
/// together), each further image as an item of its own, so a paste keeps
/// all of them.
#[cfg(target_os = "macos")]
fn write_macos_text_and_image_pasteboard(text: &str, html: &str, image_data_uris: &[String]) -> bool {
    let tmp = std::env::temp_dir().join(format!("vantry-clip-{}", uuid::Uuid::new_v4().simple()));
    let txt_path = tmp.with_extension("txt");
    let html_path = tmp.with_extension("html");
    let mut files = vec![txt_path.clone(), html_path.clone()];
    let mut ok = std::fs::write(&txt_path, text.as_bytes()).is_ok()
        && std::fs::write(&html_path, html.as_bytes()).is_ok();
    for (i, uri) in image_data_uris.iter().enumerate() {
        let Some((_, bytes)) = nsh_core::platform::clipboard::decode_data_uri(uri) else {
            continue;
        };
        let path = tmp.with_extension(format!("{i}.img"));
        ok &= std::fs::write(&path, &bytes).is_ok();
        files.push(path);
    }
    if ok && files.len() > 2 {
        ok = std::process::Command::new("osascript")
            .args(["-l", "JavaScript", "-e", MACOS_PASTEBOARD_SCRIPT, "--"])
            .args(&files)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .is_ok_and(|s| s.success());
    } else {
        ok = false;
    }
    for file in files {
        let _ = std::fs::remove_file(file);
    }
    ok
}

/// Copies a user message preserving its exact original text AND, if it has image
/// attachments, writes native image data (`PNG`/`TIFF`) alongside plain text & HTML
/// so pasting into chat apps, rich editors, image apps, or Vantry itself keeps both.
pub(crate) fn copy_message(text: String, attachments: Vec<vantry_protocol::Attachment>) {
    let lang = ui().with_untracked(|u| u.locale);
    // The message names its images `@图1`, `@图2`… in their order: a paste
    // back into a composer puts each image where its name was.
    let images: Vec<(vantry_protocol::Attachment, Vec<String>)> = attachments
        .iter()
        .filter(|a| a.mime.starts_with("image/"))
        .enumerate()
        .map(|(i, item)| {
            let n = i + 1;
            let mut names = vec![
                format!("@{}", lang.image_label(n)),
                format!("@图{n}"),
                format!("@圖{n}"),
                format!("@Image {n}"),
                format!("@画像{n}"),
            ];
            if !item.name.is_empty() {
                names.push(format!("@{}", item.name));
            }
            names.dedup();
            (item.clone(), names)
        })
        .collect();
    if images.is_empty() {
        copy_with_images(text, attachments, None);
        return;
    }
    let id = crate::views::remember_copy(&text, &images, Default::default());
    copy_with_images(text, attachments, Some(id));
}

pub(crate) fn copy_with_images(text: String, attachments: Vec<vantry_protocol::Attachment>, selection_id: Option<String>) {
    let images: Vec<vantry_protocol::Attachment> = attachments
        .into_iter()
        .filter(|a| a.mime.starts_with("image/"))
        .collect();
    if images.is_empty() {
        copy(text);
        return;
    }
    let sources: Vec<(vantry_protocol::Attachment, Option<PathBuf>)> = state().with_untracked(|s| {
        let draft = s.drafts.get(&s.draft_key());
        images
            .into_iter()
            .map(|item| {
                let path = draft.and_then(|d| d.image_paths.get(&item.id)).cloned();
                (item, path)
            })
            .collect()
    });
    if let Some(id) = &selection_id {
        let mut clip = ClipboardItem::default();
        clip.text = text.clone();
        let escaped = text.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;");
        clip.html = Some(format!("<div data-vantry-selection=\"{id}\">{escaped}</div>"));
        drop(set_clipboard_item(clip));
    } else if !text.trim().is_empty() {
        drop(set_clipboard(text.clone()));
    }
    state().update(|s| s.notice = Some(Notice::new(NoticeLevel::Info, "已复制消息与图片。")));
    spawn_for_window(async move {
        let escape = |s: &str| {
            s.replace('&', "&amp;")
                .replace('<', "&lt;")
                .replace('>', "&gt;")
                .replace('"', "&quot;")
        };
        let mut html = match &selection_id {
            Some(id) => format!("<div data-vantry-selection=\"{id}\">"),
            None => String::from("<div>"),
        };
        if !text.is_empty() {
            html.push_str("<p>");
            html.push_str(&escape(&text).replace('\n', "<br>"));
            html.push_str("</p>");
        }
        let mut uris: Vec<String> = Vec::new();
        for (item, path) in sources {
            if let Some(src) = attachment_preview(item.clone(), path).await
                && let Some(data_uri) = ensure_image_data_uri(&src)
            {
                uris.push(data_uri.clone());
                html.push_str(&format!(
                    "<p><img src=\"{}\" alt=\"{}\" /></p>",
                    escape(&data_uri),
                    escape(&item.name)
                ));
            }
        }
        html.push_str("</div>");
        let Some(first_uri) = uris.first().cloned() else {
            warn("无法读取所选图片，剪贴板已保留文字。");
            return;
        };
        if let Some(id) = selection_id {
            let marker = format!("data-vantry-selection=\"{id}\"");
            if arboard::Clipboard::new().and_then(|mut c| c.get().html()).is_ok_and(|html| !html.contains(&marker)) {
                return;
            }
        }
        if text.trim().is_empty() && uris.len() == 1 {
            set_clipboard_image(first_uri);
            return;
        }
        #[cfg(target_os = "macos")]
        {
            let text_clone = text.clone();
            let html_clone = html.clone();
            let all = uris.clone();
            let wrote = tokio::task::spawn_blocking(move || {
                write_macos_text_and_image_pasteboard(&text_clone, &html_clone, &all)
            })
            .await
            .unwrap_or(false);
            if wrote {
                return;
            }
        }
        if !uris.is_empty() {
            let mut clip = ClipboardItem::default();
            clip.text = text;
            clip.html = Some(html);
            drop(set_clipboard_item(clip));
        }
    });
}

/// Copies a single image preview (`data:...` or `file://...`) natively as PNG/TIFF to the system clipboard.
pub(crate) fn copy_image_preview(src: String, _name: String) {
    if let Some(data_uri) = ensure_image_data_uri(&src) {
        set_clipboard_image(data_uri);
        state().update(|s| s.notice = Some(Notice::new(NoticeLevel::Info, "已复制图片。")));
    } else {
        warn("无法读取图片数据以复制。");
    }
}

/// Loads (local or remote) and copies an image attachment natively to the system clipboard.
pub(crate) fn copy_attachment_image(item: vantry_protocol::Attachment) {
    let source = state().with_untracked(|s| {
        s.drafts
            .get(&s.draft_key())
            .and_then(|d| d.image_paths.get(&item.id))
            .cloned()
    });
    spawn_for_window(async move {
        if let Some(src) = attachment_preview(item.clone(), source).await {
            copy_image_preview(src, item.name);
        } else {
            warn("无法读取图片数据以复制。");
        }
    });
}

pub(crate) fn open_attachment_preview(item: vantry_protocol::Attachment, label: String) {
    ui().update(move |u| u.attachment_preview = Some((item, label)));
}

pub(crate) fn close_attachment_preview() {
    ui().update(|u| u.attachment_preview = None);
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
    let mut seq = state().with_untracked(|s| {
        s.drafts
            .get(&key)
            .map_or(0, |d| d.attachments.len())
    });
    let mut images = Vec::new();
    for file in paste.files.iter().filter(|f| f.mime.starts_with("image/")) {
        if !matches!(file.mime.as_str(), "image/png" | "image/jpeg" | "image/gif" | "image/webp") {
            warn("仅支持 PNG、JPEG、GIF 或 WebP 图片。");
            continue;
        }
        if !(1..=8 << 20).contains(&file.data.len()) {
            warn("图片须在 8 MB 以内。");
            continue;
        }
        seq += 1;
        let ext = match file.mime.as_str() {
            "image/jpeg" => "jpg",
            "image/gif" => "gif",
            "image/webp" => "webp",
            _ => "png",
        };
        let name = if file.name.trim().is_empty()
            || crate::state::draft::is_generic_image_name(&file.name)
        {
            format!("pasted-image-{seq}.{ext}")
        } else {
            file.name.clone()
        };
        images.push((name, file.data.clone()));
    }
    if images.is_empty() {
        return;
    }
    state().update(|s| s.begin_attachment_import(&key));
    spawn_for_window(async move {
        for (name, bytes) in images {
            let mut req = actions::request(names::ATTACHMENT_IMPORT);
            req.name = name;
            req.data = base64::engine::general_purpose::STANDARD.encode(bytes);
            match call(bridge.workbench(req)).await {
                Ok(response) => match response.attachment {
                    Some(item) => attach_to_draft(&key, item, None),
                    None => warn("Core 没有返回图片附件。"),
                },
                Err(err) => warn(format!("无法粘贴图片：{}", err.message)),
            }
        }
        state().update(|s| s.finish_attachment_import(&key));
    });
}

fn decode_file_uri(uri: &str) -> PathBuf {
    let raw = uri.strip_prefix("file://").unwrap_or(uri);
    let raw = raw.strip_prefix("localhost").unwrap_or(raw);
    let bytes = raw.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%'
            && i + 2 < bytes.len()
            && let (Some(h), Some(l)) = (
                (bytes[i + 1] as char).to_digit(16),
                (bytes[i + 2] as char).to_digit(16),
            )
        {
            out.push(((h << 4) | l) as u8);
            i += 3;
            continue;
        }
        out.push(bytes[i]);
        i += 1;
    }
    PathBuf::from(String::from_utf8_lossy(&out).into_owned())
}

/// Attaches local `file://` URIs pasted from Finder.
pub(crate) fn paste_file_uris(uris: &[String]) {
    let Some(workbench) = workbench() else {
        return;
    };
    let paths: Vec<PathBuf> = uris
        .iter()
        .map(|u| decode_file_uri(u))
        .filter(|p| p.is_file())
        .collect();
    if paths.is_empty() {
        return;
    }
    let key = state().with_untracked(|s| s.draft_key());
    state().update(|s| s.begin_attachment_import(&key));
    spawn_for_window(async move {
        for path in paths {
            match call(workbench.attach(path.clone())).await {
                Ok(attachment) => attach_to_draft(&key, attachment, Some(path)),
                Err(error) => warn(format!("无法附加文件：{}", error.message)),
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
                        Ok(attachment) => attach_to_draft(&key, attachment, Some(path)),
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

thread_local! {
    /// Image previews already read, by attachment id: a thumbnail made again
    /// (a re-render, another draft, the sent message) shows at once.
    static PREVIEWS: RefCell<(std::collections::HashMap<String, String>, usize)> =
        RefCell::new((std::collections::HashMap::new(), 0));
}

/// The previews kept, in bytes of their data URLs.
const PREVIEW_CACHE_BYTES: usize = 48 << 20;

/// An image attachment's preview, retried while the Core is not answering
/// yet (a thumbnail restored as the app or its Core starts); `None` when it
/// still has none after that.
pub(crate) async fn attachment_preview_retrying(
    item: vantry_protocol::Attachment,
    source: Option<PathBuf>,
) -> Option<String> {
    for attempt in 0..7u32 {
        if let Some(src) = attachment_preview(item.clone(), source.clone()).await {
            return Some(src);
        }
        if !item.mime.starts_with("image/") || !(1..=(8 << 20)).contains(&item.size) {
            return None;
        }
        timer(Duration::from_millis(400 << attempt.min(4))).await;
    }
    None
}

pub(crate) async fn attachment_preview(
    item: vantry_protocol::Attachment,
    source: Option<PathBuf>,
) -> Option<String> {
    if !item.mime.starts_with("image/") || !(1..=(8 << 20)).contains(&item.size) {
        return None;
    }
    let key = format!("{}\x1f{}", current_machine().unwrap_or_default(), item.id);
    if let Some(src) = PREVIEWS.with(|p| p.borrow().0.get(&key).cloned()) {
        return Some(src);
    }
    let src = read_attachment_preview(&item, source).await?;
    PREVIEWS.with(|p| {
        let (map, bytes) = &mut *p.borrow_mut();
        if *bytes + src.len() > PREVIEW_CACHE_BYTES {
            map.clear();
            *bytes = 0;
        }
        *bytes += src.len();
        if let Some(old) = map.insert(key, src.clone()) {
            *bytes -= old.len();
        }
    });
    Some(src)
}

async fn read_attachment_preview(
    item: &vantry_protocol::Attachment,
    source: Option<PathBuf>,
) -> Option<String> {
    let item = item.clone();
    if let Some(workbench) = workbench() {
        let mut request = actions::request(names::ATTACHMENT_READ);
        request.thread_id = state().with_untracked(|s| s.selected.clone().unwrap_or_default());
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

pub(crate) fn current_machine() -> Option<String> { bridges(|b| b.peer_id.clone()) }

// --------------------------------------------------------------------- ui

pub(crate) fn narrow_window() -> bool {
    crate::window_size().width.0 < crate::theme::metrics::NARROW_WIDTH
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
    let local_update_bench = current_machine()
        .is_some()
        .then(|| local_workbench().map(|(w, _)| w))
        .flatten();
    let workspace = bench().with_untracked(|b| b.root.clone());
    spawn_for_window(async move {
        let mut result = call(workbench.settings(workspace)).await.map_err(|e| e.message);
        if let (Ok(data), Some(local_wb)) = (&mut result, local_update_bench)
            && let Ok(resp) = call(local_wb.workbench(actions::request(names::UPDATE_STATUS))).await
        {
            data.absorb(names::UPDATE_STATUS, &resp);
        }
        if current_machine().is_none() && !bridges(|b| b.mock)
            && let Ok(data) = &result && let Some(remote) = &data.remote && remote.error.is_empty() {
            machines::sync(remote.peers());
        }
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

pub(crate) async fn refresh_device_requests() {
    let Some((bridge, _)) = local_workbench() else { return };
    if let Ok(response) = call(bridge.workbench(actions::request(names::REMOTE_STATUS))).await
        && let Ok(status) = actions::RemoteStatus::read(&response) {
        ui().update(|u| {
            if let crate::state::ui::Catalogs::Loaded(data) = &mut u.catalogs
                && let Some(remote) = Arc::make_mut(data).remote.as_mut() {
                remote.requests = status.requests;
                remote.require_approval = status.require_approval;
            }
        });
    }
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
    let Some((workbench, _)) = local_workbench() else { return };
    if !ui().with_untracked(|u| !u.op_busy("pair.redeem")) { return; }
    ui().update(|u| { u.begin_op("pair.redeem"); });
    let mut request = actions::pair_redeem(&code, &name);
    if !code.trim().starts_with("vantry://") {
        request.input = Some(serde_json::json!({"code":code.trim()}));
    }
    spawn_for_window(async move {
        let result = async {
            loop {
                let response = call(workbench.workbench(request)).await.map_err(|e| e.message)?;
                let reply: serde_json::Value = serde_json::from_str(&response.output).map_err(|_| "配对响应无效".to_owned())?;
                match reply["status"].as_str() {
                    Some("pending") => {
                        let handle = reply["handle"].as_str().filter(|h| !h.is_empty()).ok_or("缺少配对请求编号")?.to_owned();
                        ui().update(|u| u.forms.pair_wait = format!("请在 {} 上核对并批准 {}", reply["hostName"].as_str().unwrap_or("主机"), reply["verificationCode"].as_str().unwrap_or("")));
                        timer(Duration::from_secs(3)).await;
                        if !ui().with_untracked(|u| u.forms.pair_dialog) { return Err("配对已取消".to_owned()); }
                        request = actions::request("remote.pair.status");
                        request.input = Some(serde_json::json!({"handle":handle}));
                    }
                    Some("denied") => return Err("主机已拒绝配对".into()),
                    Some("expired") => return Err("请求已过期，请重新生成配对码".into()),
                    Some("paired") | None => return Ok(response),
                    _ => return Err("无法识别配对状态".into()),
                }
            }
        }.await;
        let success = result.is_ok();
        if let Some(id) = result.as_ref().ok().and_then(|r| serde_json::from_str::<serde_json::Value>(&r.output).ok()).and_then(|v| v["id"].as_str().map(str::to_owned)) {
            if let Some(m) = machines::by_id(&id) { m.owner.run(|| { if let Some(c) = core() { c.reload_credentials(); } ui().update(|u| u.forms.repair_peer.clear()); }); }
        }
        ui().update(|u| {
            u.forms.pair_wait.clear();
            if success { u.forms.pair_dialog = false; }
            u.end_op("pair.redeem", "remote.pair.result", result, "配对");
        });
        if success { machines::run_on("", reload_catalogs); }
    });
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
    let Some((workbench, is_mock)) = local_workbench() else {
        return;
    };
    let mut go = false;
    ui().update(|u| go = u.begin_op("update"));
    if !go {
        return;
    }
    let staged_version = ui().with_untracked(|u| {
        u.data()
            .map(|d| d.update.staged_version.clone())
            .unwrap_or_default()
    });
    let mut request = actions::request(action);
    if action == names::UPDATE_INSTALL {
        request.input = Some(serde_json::json!({
            "version": staged_version,
            "desktopPid": std::process::id(),
            "allowBusyCore": true,
        }));
    }
    let done = match action {
        names::UPDATE_CHECK => "检查完成",
        names::UPDATE_DOWNLOAD => "更新已下载",
        _ => "重启更新",
    };
    let active_ui = ui();
    let local_ui = machines::by_id("").map(|m| m.session.ui);
    spawn_for_window(async move {
        let result = call(workbench.workbench(request)).await.map_err(|e| e.message);
        if action == names::UPDATE_INSTALL && !is_mock {
            match result {
                Ok(_) => {
                    // Keep the button disabled and show clear progress while quitting so
                    // apply-update can swap the bundle and reopen the new version.
                    active_ui.update(|u| {
                        u.forms.feedback = Some(crate::state::ui::Feedback {
                            ok: true,
                            text: "正在重启并安装更新…".into(),
                        });
                    });
                    timer(Duration::from_millis(120)).await;
                    bridge::stop_all_tunnels();
                    nsh_core::element::desktop::quit();
                }
                Err(error) => {
                    active_ui.update(move |u| u.end_op("update", action, Err(error), done));
                }
            }
            return;
        }
        if let Ok(resp) = &result
            && let Some(local_ui) = local_ui
            && local_ui != active_ui
        {
            let resp = resp.clone();
            local_ui.update(move |u| {
                if let crate::state::ui::Catalogs::Loaded(data) = &mut u.catalogs {
                    std::sync::Arc::make_mut(data).absorb(action, &resp);
                }
            });
        }
        active_ui.update(move |u| u.end_op("update", action, result, done));
    });
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
    if matches!(tab, DockTab::Summary | DockTab::Plugins | DockTab::Mcp) { load_settings_catalogs(); }
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
mod pickers;
pub(crate) use pickers::*;
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
            u.settings.is_some()
                || u.rename.is_some()
                || u.confirm.is_some()
                || u.machine_picker
                || u.folder_picker,
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
