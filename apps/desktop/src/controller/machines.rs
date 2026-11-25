//! One window, independently owned Core connections. Sidebar filters never change the stage.
use super::*;
use crate::bridge::workbench::actions::RemotePeer;
use crate::cli::{LaunchConfig, Target};
use nsh_core::element::create_root;

#[derive(Clone, Copy, PartialEq)]
pub(crate) struct Session {
    pub state: Signal<AppState>,
    pub ui: Signal<UiState>,
    pub bench: Signal<WorkbenchState>,
    pub(super) bridges: Signal<Bridges>,
    pub(super) saves: Signal<SaveSlot>,
    pub(super) caret: Signal<CaretRequest>,
    pub(super) position: Signal<Option<(String, u32)>>,
}

#[derive(Clone, PartialEq)]
pub(crate) struct Machine {
    pub id: String,
    pub name: String,
    pub owner: Owner,
    pub session: Session,
    config: std::rc::Rc<LaunchConfig>,
    started: Signal<bool>,
    loading: Signal<bool>,
    pub failure: Signal<String>,
    pub revision: Memo<u64>,
}

pub(crate) fn records() -> Signal<Vec<Machine>> {
    global::<Vec<Machine>>()
}
#[derive(Default, Clone, PartialEq)]
pub(crate) struct Navigation {
    pub active: String,
    pub filter: Option<String>,
}
pub(crate) fn navigation() -> Signal<Navigation> {
    global::<Navigation>()
}

fn peer_name(id: &str, name: &str) -> String {
    if !name.trim().is_empty() && name != id {
        return name.to_owned();
    }
    format!("配对主机 · {}", id.chars().take(8).collect::<String>())
}

fn register(config: LaunchConfig, name: String) -> Machine {
    let id = match &config.target {
        Target::Local => String::new(),
        Target::Peer { id, .. } => id.clone(),
    };
    let (owner, (session, started, loading, failure, revision)) = create_root(|| {
        let session = Session {
            state: signal(AppState::default()),
            ui: signal(UiState::default()),
            bench: signal(WorkbenchState::default()),
            bridges: signal(Bridges::default()),
            saves: signal(SaveSlot::default()),
            caret: signal(CaretRequest::default()),
            position: signal(None),
        };
        provide_context(session);
        (
            session,
            signal(false),
            signal(false),
            signal(String::new()),
            computed(move || session.state.with(|s| s.sidebar_revision)),
        )
    });
    let record = Machine {
        id,
        name,
        owner,
        session,
        config: std::rc::Rc::new(config),
        started,
        loading,
        failure,
        revision,
    };
    records().update(|list| list.push(record.clone()));
    record
}

pub(crate) fn by_id(id: &str) -> Option<Machine> {
    records().with_untracked(|list| list.iter().find(|m| m.id == id).cloned())
}

fn start_record(m: &Machine) {
    if !m.started.get_untracked() {
        m.started.set(true);
        m.owner.run(|| start(&m.config));
    }
}

pub(crate) fn init(config: LaunchConfig) {
    let mut local = config.clone();
    if matches!(config.target, Target::Peer { .. }) {
        local.target = Target::Local;
        local.endpoint = Endpoint::Local {
            socket: local.data_dir.join("core.sock"),
        };
    }
    let root = register(local, crate::tr("本机").to_owned());
    start_record(&root);
    if let Target::Peer { id, .. } = &config.target {
        let peer = register(config.clone(), peer_name(id, ""));
        navigation().update(|n| n.active = id.clone());
        start_record(&peer);
    }
    let hosts = records();
    cleanup(move || {
        hosts.with_untracked(|list| {
            for m in list {
                m.owner.dispose();
            }
        })
    });
}

/// Adopt the local Core's paired-host catalog. A closed host only needs a sidebar projection.
pub(super) fn sync(peers: &[RemotePeer]) {
    let Some(local) = by_id("") else { return };
    let removed: Vec<_> = records().with_untracked(|list| {
        list.iter()
            .filter(|m| !m.id.is_empty() && !peers.iter().any(|p| p.id == m.id))
            .cloned()
            .collect()
    });
    for m in removed {
        navigation().update(|n| {
            if n.active == m.id {
                n.active.clear();
            }
            if n.filter.as_ref() == Some(&m.id) {
                n.filter = None;
            }
        });
        records().update(|list| list.retain(|v| v.id != m.id));
        m.owner.dispose();
    }
    for p in peers.iter().filter(|p| !p.id.is_empty()) {
        let name = peer_name(&p.id, &p.name);
        if let Some(m) = by_id(&p.id) {
            if m.name != name {
                if m.started.get_untracked() {
                    m.session.state.update(|s| s.endpoint_label = name.clone());
                    m.session
                        .ui
                        .update(|u| u.remote_target = Some(name.clone()));
                }
                records().update(|list| {
                    if let Some(r) = list.iter_mut().find(|r| r.id == p.id) {
                        r.name = name;
                    }
                });
            }
            continue;
        }
        let mut config = (*local.config).clone();
        config.workspace = None;
        config.endpoint = Endpoint::Peer {
            id: p.id.clone(),
            mode: "auto".into(),
        };
        config.target = Target::Peer {
            id: p.id.clone(),
            mode: "auto".into(),
        };
        let machine = register(config, name);
        refresh_summary(machine);
    }
}

pub(crate) fn refresh_summary(m: Machine) {
    if m.started.get_untracked() || m.loading.get_untracked() {
        return;
    }
    m.loading.set(true);
    m.failure.set(String::new());
    m.owner.run(|| {
        spawn(async move {
            let remote = bridge::RemoteBridge::new(
                m.config.endpoint.clone(),
                Some(m.config.data_dir.clone()),
            );
            let result = call(remote.thread_summaries()).await;
            if !m.started.get_untracked() {
                match result {
                    Ok(projection) => m.session.state.update(|s| s.apply_summaries(projection)),
                    Err(e) => m.failure.set(e.message),
                }
            }
            m.loading.set(false);
        })
    });
}

pub(crate) fn activate(id: &str, action: impl FnOnce()) {
    let Some(m) = by_id(id) else { return };
    let old = navigation().with_untracked(|n| n.active.clone());
    if old != id {
        start_record(&m);
        if let Some(previous) = by_id(&old) {
            previous.session.ui.with_untracked(|from| {
                m.session.ui.update(|to| {
                    to.filter = from.filter.clone();
                    to.sidebar_open = from.sidebar_open;
                    to.theme = from.theme;
                    to.theme_preset = from.theme_preset;
                    to.locale = from.locale;
                    to.send_with_enter = from.send_with_enter;
                })
            });
        }
        navigation().update(|n| n.active = id.to_owned());
    }
    m.owner.run(action);
}

pub(crate) fn run_on(id: &str, action: impl FnOnce()) {
    if let Some(m) = by_id(id) {
        start_record(&m);
        m.owner.run(action);
    }
}

pub(crate) fn draft_target() -> String {
    navigation().with(|n| n.filter.clone().unwrap_or_else(|| n.active.clone()))
}
pub(crate) fn can_new_draft() -> bool {
    by_id(&draft_target())
        .is_some_and(|m| m.id.is_empty() || (m.failure.with(String::is_empty) && !m.loading.get()))
}
pub(crate) fn new_draft() {
    if can_new_draft() {
        activate(&draft_target(), super::new_thread);
    }
}
