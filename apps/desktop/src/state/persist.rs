//! `desktop-state.json`: what the window keeps across restarts — the
//! selected thread, the sidebar and dock (open, tab), the theme, every
//! thread's draft and the sidebar's filters, the new-thread defaults.
//!
//! Written atomically: a temporary file in the same directory (0600 on
//! Unix), flushed and synced, then renamed over the old one, so a crash
//! leaves either the old or the new state, never half of one. Unsent
//! commands are never kept: a restart does not resend anything.

use std::{
    collections::BTreeMap,
    io::Write,
    path::{Path, PathBuf},
    sync::atomic::{AtomicU64, Ordering},
};

use serde::{Deserialize, Serialize};

use super::{
    AppState, Draft, InteractionMode, RunningAction,
    sidebar::StatusFilter,
    ui::{ThemePreference, UiState},
    workbench::DockTab,
};

/// The file's name in the data directory.
pub const FILE_NAME: &str = "desktop-state.json";
/// The layout this desktop writes.
pub const VERSION: u32 = 1;

/// A thread's kept draft (attachments are the Core's and not kept).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct SavedDraft {
    pub text: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub plan: bool,
}

/// The file's contents.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct DesktopStateFile {
    pub version: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub selected_thread: Option<String>,
    pub sidebar_open: bool,
    pub dock_open: bool,
    pub dock_tab: String,
    pub theme: String,
    /// By thread id; `""` is the new thread's.
    pub drafts: BTreeMap<String, SavedDraft>,
    pub sidebar_query: String,
    pub sidebar_status: String,
    pub show_archived: bool,
    pub collapsed: Vec<String>,
    /// `steer` or `queue`: what sending does while a run goes.
    pub running_action: String,
    pub default_approval_policy: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub default_model: String,
    pub extensions_enabled: bool,
    pub mcp_disabled: Vec<String>,
}

impl Default for DesktopStateFile {
    fn default() -> Self {
        let ui = UiState::default();
        DesktopStateFile {
            version: VERSION,
            selected_thread: None,
            sidebar_open: ui.sidebar_open,
            dock_open: ui.dock_open,
            dock_tab: ui.dock_tab.wire().to_owned(),
            theme: ui.theme.wire().to_owned(),
            drafts: BTreeMap::new(),
            sidebar_query: String::new(),
            sidebar_status: StatusFilter::All.wire().to_owned(),
            show_archived: false,
            collapsed: Vec::new(),
            running_action: "steer".to_owned(),
            default_approval_policy: ui.default_policy.clone(),
            default_model: String::new(),
            extensions_enabled: true,
            mcp_disabled: Vec::new(),
        }
    }
}

/// What to keep of `app` and `ui`.
pub fn capture(app: &AppState, ui: &UiState) -> DesktopStateFile {
    let mut drafts: BTreeMap<String, SavedDraft> = app
        .drafts
        .iter()
        .filter(|(_, d)| !d.text.trim().is_empty() || d.mode == InteractionMode::Plan)
        .map(|(key, d)| {
            (
                key.clone(),
                SavedDraft {
                    text: d.text.clone(),
                    plan: d.mode == InteractionMode::Plan,
                },
            )
        })
        .collect();
    // An edit of a queued message is not a draft: keep what it replaced.
    if let Some(edit) = &app.queue_edit {
        drafts.remove(&edit.thread_id);
        if !edit.saved.text.trim().is_empty() {
            drafts.insert(
                edit.thread_id.clone(),
                SavedDraft {
                    text: edit.saved.text.clone(),
                    plan: edit.saved.mode == InteractionMode::Plan,
                },
            );
        }
    }
    DesktopStateFile {
        version: VERSION,
        selected_thread: app.selected.clone(),
        sidebar_open: ui.sidebar_open,
        dock_open: ui.dock_open,
        dock_tab: ui.dock_tab.wire().to_owned(),
        theme: ui.theme.wire().to_owned(),
        drafts,
        sidebar_query: ui.filter.query.clone(),
        sidebar_status: ui.filter.status.wire().to_owned(),
        show_archived: ui.filter.show_archived,
        collapsed: ui.filter.collapsed.iter().cloned().collect(),
        running_action: match app.running_action {
            RunningAction::Steer => "steer",
            RunningAction::Queue => "queue",
        }
        .to_owned(),
        default_approval_policy: ui.default_policy.clone(),
        default_model: app.new_thread.model.clone(),
        extensions_enabled: ui.extensions_enabled,
        mcp_disabled: ui.mcp_disabled.iter().cloned().collect(),
    }
}

/// Puts `file` back into fresh states (before the first snapshot: the
/// selection is checked when the snapshot arrives).
pub fn restore(file: &DesktopStateFile, app: &mut AppState, ui: &mut UiState) {
    app.selected = file.selected_thread.clone();
    for (key, draft) in &file.drafts {
        app.drafts.insert(
            key.clone(),
            Draft {
                text: draft.text.clone(),
                mode: if draft.plan {
                    InteractionMode::Plan
                } else {
                    InteractionMode::Default
                },
                attachments: Vec::new(),
            },
        );
    }
    app.running_action = if file.running_action == "queue" {
        RunningAction::Queue
    } else {
        RunningAction::Steer
    };
    if !file.default_model.is_empty() && app.new_thread.model.is_empty() {
        app.new_thread.model = file.default_model.clone();
    }
    if !file.default_approval_policy.is_empty() {
        app.new_thread.approval_policy = file.default_approval_policy.clone();
        ui.default_policy = file.default_approval_policy.clone();
    }
    ui.sidebar_open = file.sidebar_open;
    ui.dock_open = file.dock_open;
    ui.dock_tab = DockTab::parse(&file.dock_tab);
    ui.theme = ThemePreference::parse(&file.theme);
    ui.filter.query = file.sidebar_query.clone();
    ui.filter.status = StatusFilter::parse(&file.sidebar_status);
    ui.filter.show_archived = file.show_archived;
    ui.filter.collapsed = file.collapsed.iter().cloned().collect();
    ui.extensions_enabled = file.extensions_enabled;
    ui.mcp_disabled = file.mcp_disabled.iter().cloned().collect();
}

/// Reads the file: `None` when there is none yet; an error when it is not
/// readable (the caller starts fresh and says so, keeping the file).
pub fn load(path: &Path) -> std::io::Result<Option<DesktopStateFile>> {
    let bytes = match std::fs::read(path) {
        Ok(bytes) => bytes,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e),
    };
    let file: DesktopStateFile = serde_json::from_slice(&bytes)
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;
    if file.version > VERSION {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!("desktop-state.json version {} is newer than {VERSION}", file.version),
        ));
    }
    Ok(Some(file))
}

/// Writes `file` to `path` atomically.
pub fn save_atomic(path: &Path, file: &DesktopStateFile) -> std::io::Result<()> {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let dir = path.parent().unwrap_or(Path::new("."));
    create_private_dir(dir)?;
    let bytes = serde_json::to_vec_pretty(file).map_err(std::io::Error::other)?;
    let name = path
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| FILE_NAME.to_owned());
    let temp: PathBuf = dir.join(format!(
        ".{name}.tmp-{}-{}",
        std::process::id(),
        COUNTER.fetch_add(1, Ordering::Relaxed)
    ));
    let result = (|| {
        let mut options = std::fs::OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let mut out = options.open(&temp)?;
        out.write_all(&bytes)?;
        out.write_all(b"\n")?;
        out.sync_all()?;
        drop(out);
        std::fs::rename(&temp, path)
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&temp);
    }
    result
}

fn create_private_dir(dir: &Path) -> std::io::Result<()> {
    if dir.as_os_str().is_empty() || dir.exists() {
        return Ok(());
    }
    let mut builder = std::fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder.create(dir)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{NEW_THREAD, QueueEdit};

    fn sample() -> (AppState, UiState) {
        let mut app = AppState::default();
        app.selected = Some("t1".into());
        app.drafts.insert(
            "t1".into(),
            Draft {
                text: "未发送的草稿".into(),
                mode: InteractionMode::Plan,
                attachments: Vec::new(),
            },
        );
        app.drafts.insert("t2".into(), Draft::default());
        app.drafts.insert(
            NEW_THREAD.into(),
            Draft {
                text: "新会话草稿".into(),
                ..Draft::default()
            },
        );
        app.running_action = RunningAction::Queue;
        app.new_thread.model = "anthropic/opus".into();
        let mut ui = UiState::default();
        ui.theme = ThemePreference::Dark;
        ui.dock_open = true;
        ui.dock_tab = DockTab::Git;
        ui.sidebar_open = false;
        ui.filter.query = "发布".into();
        ui.filter.status = StatusFilter::Attention;
        ui.filter.show_archived = true;
        ui.filter.collapsed.insert("parent".into());
        ui.default_policy = "auto".into();
        ui.mcp_disabled.insert("figma".into());
        (app, ui)
    }

    #[test]
    fn capture_and_restore_round_trip() {
        let (app, ui) = sample();
        let file = capture(&app, &ui);
        assert_eq!(file.drafts.len(), 2, "blank drafts are not kept");
        assert!(file.drafts["t1"].plan);
        let mut app2 = AppState::default();
        let mut ui2 = UiState::default();
        restore(&file, &mut app2, &mut ui2);
        assert_eq!(app2.selected.as_deref(), Some("t1"));
        assert_eq!(app2.drafts["t1"].text, "未发送的草稿");
        assert_eq!(app2.drafts["t1"].mode, InteractionMode::Plan);
        assert_eq!(app2.running_action, RunningAction::Queue);
        assert_eq!(app2.new_thread.model, "anthropic/opus");
        assert_eq!(app2.new_thread.approval_policy, "auto");
        assert_eq!(ui2.theme, ThemePreference::Dark);
        assert_eq!((ui2.dock_open, ui2.dock_tab, ui2.sidebar_open), (true, DockTab::Git, false));
        assert_eq!(ui2.filter, ui.filter);
        assert!(ui2.mcp_disabled.contains("figma"));
        assert_eq!(capture(&app2, &ui2), file);
    }

    #[test]
    fn a_queue_edit_keeps_the_draft_it_replaced() {
        let (mut app, ui) = sample();
        app.queue_edit = Some(QueueEdit {
            input_id: "in1".into(),
            thread_id: "t1".into(),
            saved: Draft {
                text: "原草稿".into(),
                ..Draft::default()
            },
        });
        let file = capture(&app, &ui);
        assert_eq!(file.drafts["t1"].text, "原草稿");
    }

    #[test]
    fn saves_atomically_and_loads_back() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("nested").join(FILE_NAME);
        assert_eq!(load(&path).unwrap(), None);
        let (app, ui) = sample();
        let file = capture(&app, &ui);
        save_atomic(&path, &file).unwrap();
        assert_eq!(load(&path).unwrap(), Some(file.clone()));
        // Overwrite: no temporary files are left behind.
        let mut second = file.clone();
        second.theme = "light".into();
        save_atomic(&path, &second).unwrap();
        assert_eq!(load(&path).unwrap().unwrap().theme, "light");
        let names: Vec<String> = std::fs::read_dir(path.parent().unwrap())
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        assert_eq!(names, [FILE_NAME]);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = std::fs::metadata(&path).unwrap().permissions().mode();
            assert_eq!(mode & 0o777, 0o600);
            let dir_mode = std::fs::metadata(path.parent().unwrap()).unwrap().permissions().mode();
            assert_eq!(dir_mode & 0o777, 0o700);
        }
        dir.close().unwrap();
    }

    #[test]
    fn unreadable_and_newer_files_are_errors_and_old_fields_default() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        std::fs::write(&path, b"{ not json").unwrap();
        assert!(load(&path).is_err());
        std::fs::write(&path, br#"{"version": 99}"#).unwrap();
        assert!(load(&path).is_err());
        // A file from before a field existed still loads.
        std::fs::write(&path, br#"{"version": 1, "theme": "dark"}"#).unwrap();
        let file = load(&path).unwrap().unwrap();
        assert_eq!(file.theme, "dark");
        assert!(file.sidebar_open);
        assert_eq!(file.default_approval_policy, "ask");
        dir.close().unwrap();
    }
}
