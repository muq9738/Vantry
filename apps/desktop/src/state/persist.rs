//! `desktop-state.json`: what the window keeps across restarts — the
//! selected thread, the sidebar and dock (open, tab), the theme, every
//! thread's draft and the sidebar's filters, the new-thread defaults.
//!
//! Bidirectionally compatible with Vantry `v0.1.42`:
//! - `drafts` is serialized as `map<threadId, string>` (so Go 0.1.42 unmarshals
//!   it cleanly) while also deserializing structured `{text, plan}` drafts from
//!   earlier Rust previews; plan-mode flags are stored alongside in `draftPlans`.
//! - `selected`, `themeSet`/`dark`, `activeTurnDefault`, and `defaults` stay in
//!   sync with 0.1.42 field names while accepting earlier Rust aliases.
//! - Unknown 0.1.42 fields (`remoteFavorites`, `manualHosts`, `locale`,
//!   `subagentPanels`, `askAnswers`, …) are preserved across
//!   atomic saves so switching between 0.1.42 and NSH loses nothing.
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
use serde_json::Value;

use super::{
    AppState, Draft, DraftMention, InteractionMode, RunningAction,
    sidebar::StatusFilter,
    ui::{ThemePreference, UiState},
    workbench::DockTab,
};

/// The file's name in the data directory.
pub const FILE_NAME: &str = "desktop-state.json";
/// The layout this desktop writes.
pub const VERSION: u32 = 1;

/// Kept draft text and references to attachments stored by the Core.
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct SavedDraft {
    pub text: String,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub plan: bool,
    pub attachments: Vec<vantry_protocol::Attachment>,
    pub mentions: Vec<DraftMention>,
}

impl From<&Draft> for SavedDraft {
    fn from(draft: &Draft) -> Self {
        Self {
            text: draft.text.clone(),
            plan: draft.mode == InteractionMode::Plan,
            attachments: draft.attachments.clone(),
            mentions: draft.mentions.clone(),
        }
    }
}

/// On-disk draft value: either a plain `v0.1.42` string or an earlier Rust
/// `{ text, plan }` object. Always written back as a plain string for `v0.1.42`.
#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(untagged)]
enum WireDraft {
    Text(String),
    Structured(SavedDraft),
}

#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
struct WireDefaults {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    workspace: Option<Value>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    approval_policy: String,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

/// On-disk JSON shape compatible with both `v0.1.42` and NSH desktop.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
struct WireDesktopState {
    version: u32,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    selected: String,
    #[serde(default, skip_serializing)]
    selected_thread: Option<String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    drafts: BTreeMap<String, Value>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    draft_plans: BTreeMap<String, bool>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    draft_attachments: BTreeMap<String, Vec<vantry_protocol::Attachment>>,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    draft_mentions: BTreeMap<String, Vec<DraftMention>>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    local_order: Vec<String>,
    theme_set: Option<bool>,
    locale: super::language::Language,
    send_with_enter: Option<bool>,
    manual_delivery: bool,
    theme_preset: crate::theme::ThemePreset,
    #[serde(skip_serializing_if = "BTreeMap::is_empty")]
    sections: BTreeMap<String, bool>,
    dark: bool,
    #[serde(default, skip_serializing)]
    theme: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    active_turn_default: String,
    #[serde(default, skip_serializing)]
    running_action: String,
    #[serde(default)]
    defaults: WireDefaults,
    #[serde(default, skip_serializing)]
    default_approval_policy: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    default_model: String,
    sidebar_open: Option<bool>,
    dock_open: Option<bool>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    dock_tab: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    sidebar_query: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    sidebar_status: String,
    show_archived: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    collapsed: Vec<String>,
    extensions_enabled: Option<bool>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    mcp_disabled: Vec<String>,
    #[serde(flatten)]
    extra: BTreeMap<String, Value>,
}

/// The file's contents.
#[derive(Debug, Clone, PartialEq)]
pub struct DesktopStateFile {
    pub version: u32,
    pub selected_thread: Option<String>,
    pub local_order: Vec<String>,
    pub sidebar_open: bool,
    pub dock_open: bool,
    pub dock_tab: String,
    pub locale: super::language::Language,
    pub send_with_enter: bool,
    pub manual_delivery: bool,
    pub theme: String,
    pub theme_preset: crate::theme::ThemePreset,
    pub project_open: BTreeMap<String, bool>,
    /// By thread id; `""` is the new thread's.
    pub drafts: BTreeMap<String, SavedDraft>,
    pub sidebar_query: String,
    pub sidebar_status: String,
    pub show_archived: bool,
    pub collapsed: Vec<String>,
    /// `steer` or `queue`: what sending does while a run goes.
    pub running_action: String,
    pub default_approval_policy: String,
    pub default_model: String,
    pub default_workspace: Option<Value>,
    pub extensions_enabled: bool,
    pub mcp_disabled: Vec<String>,
}

impl Default for DesktopStateFile {
    fn default() -> Self {
        let ui = UiState::default();
        DesktopStateFile {
            version: VERSION,
            selected_thread: None,
            local_order: Vec::new(),
            sidebar_open: ui.sidebar_open,
            dock_open: ui.dock_open,
            dock_tab: ui.dock_tab.wire().to_owned(),
            theme: ui.theme.wire().to_owned(),
            theme_preset: ui.theme_preset,
            locale: ui.locale,
            send_with_enter: ui.send_with_enter,
            manual_delivery: ui.manual_delivery,
            project_open: BTreeMap::new(),
            drafts: BTreeMap::new(),
            sidebar_query: String::new(),
            sidebar_status: StatusFilter::All.wire().to_owned(),
            show_archived: false,
            collapsed: Vec::new(),
            running_action: "steer".to_owned(),
            default_approval_policy: ui.default_policy.clone(),
            default_model: String::new(),
            default_workspace: None,
            extensions_enabled: true,
            mcp_disabled: Vec::new(),
        }
    }
}

impl DesktopStateFile {
    fn from_wire(wire: WireDesktopState) -> Self {
        let defaults = Self::default();
        let selected_thread = if !wire.selected.trim().is_empty() {
            Some(wire.selected)
        } else {
            wire.selected_thread.filter(|s| !s.trim().is_empty())
        };
        let mut drafts = BTreeMap::new();
        for (key, value) in wire.drafts {
            if let Ok(parsed) = serde_json::from_value::<WireDraft>(value) {
                let mut draft = match parsed {
                    WireDraft::Text(text) => SavedDraft {
                        text,
                        ..Default::default()
                    },
                    WireDraft::Structured(saved) => saved,
                };
                if wire.draft_plans.get(&key).copied().unwrap_or(false) {
                    draft.plan = true;
                }
                if !draft.text.trim().is_empty() || draft.plan {
                    drafts.insert(key, draft);
                }
            }
        }
        for (key, plan) in wire.draft_plans {
            if plan {
                drafts.entry(key).or_insert_with(|| SavedDraft {
                    plan: true,
                    ..Default::default()
                });
            }
        }
        for (key, items) in wire.draft_attachments {
            if !items.is_empty() {
                drafts.entry(key).or_default().attachments = items;
            }
        }
        for (key, mentions) in wire.draft_mentions {
            if let Some(draft) = drafts.get_mut(&key) {
                draft.mentions = mentions
                    .into_iter()
                    .filter(|m| !m.display.is_empty() && draft.text.contains(&m.display))
                    .collect();
            }
        }
        let theme = match wire.theme_set {
            Some(true) => if wire.dark { "dark" } else { "light" }.to_owned(),
            Some(false) => "system".to_owned(),
            None if !wire.theme.is_empty() => wire.theme,
            None => defaults.theme,
        };
        let running_action = if !wire.active_turn_default.is_empty() {
            wire.active_turn_default
        } else if !wire.running_action.is_empty() {
            wire.running_action
        } else {
            defaults.running_action
        };
        let default_approval_policy = if !wire.defaults.approval_policy.is_empty() {
            wire.defaults.approval_policy
        } else if !wire.default_approval_policy.is_empty() {
            wire.default_approval_policy
        } else {
            defaults.default_approval_policy
        };
        let default_model = if !wire.default_model.is_empty() {
            wire.default_model
        } else {
            defaults.default_model
        };
        DesktopStateFile {
            version: if wire.version == 0 {
                VERSION
            } else {
                wire.version
            },
            selected_thread,
            local_order: wire.local_order,
            sidebar_open: wire.sidebar_open.unwrap_or(defaults.sidebar_open),
            dock_open: wire.dock_open.unwrap_or(defaults.dock_open),
            dock_tab: if wire.dock_tab.is_empty() {
                defaults.dock_tab
            } else {
                wire.dock_tab
            },
            theme,
            theme_preset: wire.theme_preset,
            locale: wire.locale,
            send_with_enter: wire.send_with_enter.unwrap_or(true),
            manual_delivery: wire.manual_delivery,
            project_open: wire
                .sections
                .into_iter()
                .filter_map(|(key, open)| {
                    key.strip_prefix("cat:").map(|path| (path.to_owned(), open))
                })
                .collect(),
            drafts,
            sidebar_query: wire.sidebar_query,
            sidebar_status: if wire.sidebar_status.is_empty() {
                defaults.sidebar_status
            } else {
                wire.sidebar_status
            },
            show_archived: wire.show_archived,
            collapsed: wire.collapsed,
            running_action,
            default_approval_policy,
            default_model,
            default_workspace: wire.defaults.workspace,
            extensions_enabled: wire
                .extensions_enabled
                .unwrap_or(defaults.extensions_enabled),
            mcp_disabled: wire.mcp_disabled,
        }
    }

    fn to_wire(&self, base: Option<WireDesktopState>) -> WireDesktopState {
        let mut wire = base.unwrap_or_default();
        wire.version = VERSION;
        wire.selected = self.selected_thread.clone().unwrap_or_default();
        wire.selected_thread = None;
        wire.drafts = self
            .drafts
            .iter()
            .filter(|(_, d)| !d.text.trim().is_empty())
            .map(|(k, d)| (k.clone(), Value::String(d.text.clone())))
            .collect();
        wire.local_order.clone_from(&self.local_order);
        wire.draft_attachments = self
            .drafts
            .iter()
            .filter(|(_, d)| !d.attachments.is_empty())
            .map(|(key, d)| (key.clone(), d.attachments.clone()))
            .collect();
        wire.draft_mentions = self
            .drafts
            .iter()
            .filter(|(_, d)| !d.mentions.is_empty())
            .map(|(key, d)| (key.clone(), d.mentions.clone()))
            .collect();
        wire.draft_plans = self
            .drafts
            .iter()
            .filter(|(_, d)| d.plan)
            .map(|(k, _)| (k.clone(), true))
            .collect();
        wire.theme_preset = self.theme_preset;
        wire.locale = self.locale;
        wire.send_with_enter = Some(self.send_with_enter);
        wire.manual_delivery = self.manual_delivery;
        for (key, open) in &self.project_open {
            wire.sections.insert(format!("cat:{key}"), *open);
        }
        match ThemePreference::parse(&self.theme) {
            ThemePreference::System => {
                wire.theme_set = Some(false);
                wire.dark = false;
            }
            ThemePreference::Light => {
                wire.theme_set = Some(true);
                wire.dark = false;
            }
            ThemePreference::Dark => {
                wire.theme_set = Some(true);
                wire.dark = true;
            }
        }
        wire.active_turn_default = self.running_action.clone();
        wire.running_action.clear();
        wire.defaults.approval_policy = self.default_approval_policy.clone();
        wire.default_approval_policy.clear();
        wire.default_model = self.default_model.clone();
        // Preserve extended fields on an existing workspace definition.
        wire.defaults.workspace = match (&self.default_workspace, wire.defaults.workspace.take()) {
            (Some(Value::Object(new)), Some(Value::Object(mut previous)))
                if ["kind", "path", "repository", "environmentId"]
                    .iter()
                    .all(|key| new.get(*key) == previous.get(*key)) =>
            {
                for key in [
                    "kind",
                    "path",
                    "repository",
                    "branch",
                    "baseRef",
                    "environmentId",
                ] {
                    previous.remove(key);
                }
                previous.extend(new.clone());
                Some(Value::Object(previous))
            }
            (workspace, _) => workspace.clone(),
        };
        wire.sidebar_open = Some(self.sidebar_open);
        wire.dock_open = Some(self.dock_open);
        wire.dock_tab = self.dock_tab.clone();
        wire.sidebar_query = self.sidebar_query.clone();
        wire.sidebar_status = self.sidebar_status.clone();
        wire.show_archived = self.show_archived;
        wire.collapsed = self.collapsed.clone();
        wire.extensions_enabled = Some(self.extensions_enabled);
        wire.mcp_disabled = self.mcp_disabled.clone();
        wire
    }
}

/// What to keep of `app` and `ui`.
pub fn capture(app: &AppState, ui: &UiState) -> DesktopStateFile {
    let mut drafts: BTreeMap<String, SavedDraft> = app
        .drafts
        .iter()
        .filter(|(_, d)| !d.is_blank() || d.mode == InteractionMode::Plan)
        .map(|(key, draft)| (key.clone(), SavedDraft::from(draft)))
        .collect();
    // A queued-message edit temporarily borrows the composer.
    if let Some(edit) = &app.queue_edit {
        drafts.remove(&edit.thread_id);
        if !edit.saved.is_blank() || edit.saved.mode == InteractionMode::Plan {
            drafts.insert(edit.thread_id.clone(), SavedDraft::from(&edit.saved));
        }
    }
    DesktopStateFile {
        version: VERSION,
        selected_thread: app.selected.clone().or_else(|| app.local_selected.clone()),
        local_order: app.local_order.clone(),
        sidebar_open: ui.sidebar_open,
        dock_open: ui.dock_open,
        dock_tab: ui.dock_tab.wire().to_owned(),
        theme: ui.theme.wire().to_owned(),
        theme_preset: ui.theme_preset,
        project_open: ui.filter.project_open.clone(),
        locale: ui.locale,
        send_with_enter: ui.send_with_enter,
        manual_delivery: ui.manual_delivery,
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
        default_workspace: app
            .new_thread
            .workspace
            .as_ref()
            .and_then(|w| serde_json::to_value(w).ok()),
        extensions_enabled: ui.extensions_enabled,
        mcp_disabled: ui.mcp_disabled.iter().cloned().collect(),
    }
}

/// Puts `file` back into fresh states (before the first snapshot: the
/// selection is checked when the snapshot arrives).
pub fn restore(file: &DesktopStateFile, app: &mut AppState, ui: &mut UiState) {
        ui.locale = file.locale;
    ui.send_with_enter = file.send_with_enter;
    ui.manual_delivery = file.manual_delivery;
    for id in &file.local_order {
        app.restore_local_draft(id);
    }
    if let Some(id) = &file.selected_thread
        && app.restore_local_draft(id)
    {
        app.local_selected = Some(id.clone());
        app.selected = None;
    } else {
        app.local_selected = None;
        app.selected = file.selected_thread.clone();
    }
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
                attachments: draft.attachments.clone(),
                mentions: draft.mentions.clone(),
                ..Default::default()
            },
        );
    }
    app.running_action = if file.running_action == "queue" {
        RunningAction::Queue
    } else {
        RunningAction::Steer
    };
    if app.new_thread.workspace.is_none() {
        app.new_thread.workspace = file
            .default_workspace
            .clone()
            .and_then(|value| serde_json::from_value(value).ok());
    }
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
    ui.theme_preset = file.theme_preset;
    ui.filter.project_open.clone_from(&file.project_open);
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
    let wire: WireDesktopState = serde_json::from_slice(&bytes)
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;
    if wire.version > VERSION {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            format!(
                "desktop-state.json version {} is newer than {VERSION}",
                wire.version
            ),
        ));
    }
    Ok(Some(DesktopStateFile::from_wire(wire)))
}

/// Writes `file` to `path` atomically, preserving any `v0.1.42` fields already
/// present on disk so switching between Go 0.1.42 and NSH loses nothing.
pub fn save_atomic(path: &Path, file: &DesktopStateFile) -> std::io::Result<()> {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let dir = path.parent().unwrap_or(Path::new("."));
    create_private_dir(dir)?;
    let existing = std::fs::read(path)
        .ok()
        .and_then(|bytes| serde_json::from_slice::<WireDesktopState>(&bytes).ok());
    let wire = file.to_wire(existing);
    let bytes = serde_json::to_vec_pretty(&wire).map_err(std::io::Error::other)?;
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
                ..Default::default()
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
        assert_eq!(
            (ui2.dock_open, ui2.dock_tab, ui2.sidebar_open),
            (true, DockTab::Git, false)
        );
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
            let dir_mode = std::fs::metadata(path.parent().unwrap())
                .unwrap()
                .permissions()
                .mode();
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

    #[test]
    fn v0_1_42_desktop_state_loads_and_preserves_unknown_fields_round_trip() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        let baseline = serde_json::json!({
            "version": 1,
            "selected": "th_legacy_1",
            "drafts": {
                "th_legacy_1": "继续排查构建问题",
                "th_legacy_2": "第二条草稿"
            },
            "themeSet": true,
            "dark": true,
            "themePreset": "stardust",
            "activeTurnDefault": "queue",
            "locale": "zh",
            "preferredEditor": "cursor",
            "defaults": {
                "providerId": "pi",
                "approvalPolicy": "auto-accept-edits",
                "workspace": { "kind": "directory", "path": "/tmp/demo" }
            },
            "remoteFavorites": [{ "host": "devbox", "label": "Dev" }],
            "manualHosts": [{ "host": "10.0.0.2" }],
            "subagentPanels": { "th_legacy_1": { "open": true, "threadId": "th_child" } },
            "askAnswers": { "req_1": { "answer": "yes" } }
        });
        std::fs::write(&path, serde_json::to_vec_pretty(&baseline).unwrap()).unwrap();

        let loaded = load(&path).unwrap().unwrap();
        assert_eq!(loaded.selected_thread.as_deref(), Some("th_legacy_1"));
        assert_eq!(loaded.drafts["th_legacy_1"].text, "继续排查构建问题");
        assert!(!loaded.drafts["th_legacy_1"].plan);
        assert_eq!(loaded.theme, "dark");
        assert_eq!(loaded.running_action, "queue");
        assert_eq!(loaded.default_approval_policy, "auto-accept-edits");

        // Save back with a plan flag and a modified selection; v0.1.42 fields must survive
        // and `drafts` must stay map<string, string> so Go 0.1.42 still parses it.
        let mut updated = loaded.clone();
        updated.selected_thread = Some("th_legacy_2".into());
        updated.drafts.get_mut("th_legacy_1").unwrap().plan = true;
        save_atomic(&path, &updated).unwrap();

        let raw: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(raw["selected"], "th_legacy_2");
        assert_eq!(raw["drafts"]["th_legacy_1"], "继续排查构建问题");
        assert_eq!(raw["draftPlans"]["th_legacy_1"], true);
        assert_eq!(raw["themeSet"], true);
        assert_eq!(raw["dark"], true);
        assert_eq!(raw["activeTurnDefault"], "queue");
        assert_eq!(raw["defaults"]["approvalPolicy"], "auto-accept-edits");
        assert_eq!(raw["defaults"]["providerId"], "pi");
        assert_eq!(raw["defaults"]["workspace"]["path"], "/tmp/demo");
        assert_eq!(raw["locale"], "zh");
        assert_eq!(raw["preferredEditor"], "cursor");
        assert_eq!(raw["remoteFavorites"][0]["host"], "devbox");
        assert_eq!(raw["manualHosts"][0]["host"], "10.0.0.2");
        assert_eq!(raw["subagentPanels"]["th_legacy_1"]["threadId"], "th_child");
        assert_eq!(raw["askAnswers"]["req_1"]["answer"], "yes");

        let reloaded = load(&path).unwrap().unwrap();
        assert_eq!(reloaded, updated);
        dir.close().unwrap();
    }

    #[test]
    fn presets_sections_and_workspace_survive_switching_between_desktops() {
        use crate::theme::ThemePreset;
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        let old = serde_json::json!({"version":1, "theme":"dark", "themeSet":true, "dark":false, "themePreset":"ocean",
            "sections":{"cat:/root/demo":false,"pinned":true},
            "defaults":{"workspace":{"kind":"directory","path":"/root/demo","custom":"old"}}});
        std::fs::write(&path, serde_json::to_vec(&old).unwrap()).unwrap();
        let read = load(&path).unwrap().unwrap();
        let (mut app, mut ui) = (AppState::default(), UiState::default());
        restore(&read, &mut app, &mut ui);
        assert_eq!(ui.theme_preset, ThemePreset::Ocean);
        assert_eq!(ui.theme, ThemePreference::Light);
        ui.filter.project_open.insert("/root/demo".into(), true);
        ui.theme = ThemePreference::System;
        for preset in ThemePreset::ALL {
            ui.theme_preset = preset;
            save_atomic(&path, &capture(&app, &ui)).unwrap();
            let raw: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
            assert_eq!(raw["sections"]["pinned"], true);
            assert_eq!(raw["sections"]["cat:/root/demo"], true);
            assert_eq!(raw["defaults"]["workspace"]["custom"], "old");
            assert_eq!(raw["themeSet"], false);
            assert!(raw.get("theme").is_none());
            assert_eq!(load(&path).unwrap().unwrap().theme_preset, preset);
        }
        app.new_thread.workspace.as_mut().unwrap().path = "/elsewhere".into();
        save_atomic(&path, &capture(&app, &ui)).unwrap();
        let raw: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert!(raw["defaults"]["workspace"].get("custom").is_none());
        assert_eq!(
            serde_json::from_value::<ThemePreset>(serde_json::json!("legacy")).unwrap(),
            ThemePreset::Graphite
        );
    }

    #[test]
    fn old_local_drafts_keep_their_attachment_metadata_and_expand_mentions_only_on_send() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        let old = serde_json::json!({"version":1,"selected":"local.7","localOrder":["local.6","local.7"],
            "drafts":{"local.7":"@图 #2 对比 @图 和 @设计"},
            "draftAttachments":{"local.7":[{"id":"f1","name":"图.png","mime":"image/png","size":42}]},
            "draftMentions":{"local.7":[{"kind":3,"display":"@图","expand":"@图.png","attachmentId":"f1"},
                {"kind":3,"display":"@图 #2","expand":"@图2.png","attachmentId":"f2"},
                {"kind":1,"display":"@设计","expand":"[设计](vantry-thread://t1)"}]}});
        std::fs::write(&path, serde_json::to_vec(&old).unwrap()).unwrap();
        let (mut app, mut ui) = (AppState::default(), UiState::default());
        restore(&load(&path).unwrap().unwrap(), &mut app, &mut ui);
        app.apply_snapshot(vantry_protocol::Snapshot::default());
        assert_eq!(app.local_selected.as_deref(), Some("local.7"));
        assert!(app.selected.is_none());
        assert_eq!(
            app.draft().outgoing(),
            "@图2.png 对比 @图.png 和 [设计](vantry-thread://t1)"
        );
        save_atomic(&path, &capture(&app, &ui)).unwrap();
        let raw: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        for key in [
            "selected",
            "localOrder",
            "drafts",
            "draftAttachments",
            "draftMentions",
        ] {
            assert_eq!(raw[key], old[key]);
        }
        app.remove_attachment("f1");
        assert_eq!(app.draft().text, "@图 #2 对比  和 @设计");
        app.start_local_draft();
        assert_eq!(app.draft_key(), "local.8");
    }

    #[test]
    fn earlier_rust_structured_drafts_still_load() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        std::fs::write(
            &path,
            br#"{"version":1,"selectedThread":"t9","drafts":{"t9":{"text":"hello","plan":true}},"runningAction":"queue","defaultApprovalPolicy":"auto"}"#,
        )
        .unwrap();
        let loaded = load(&path).unwrap().unwrap();
        assert_eq!(loaded.selected_thread.as_deref(), Some("t9"));
        assert_eq!(loaded.drafts["t9"].text, "hello");
        assert!(loaded.drafts["t9"].plan);
        assert_eq!(loaded.running_action, "queue");
        assert_eq!(loaded.default_approval_policy, "auto");
        dir.close().unwrap();
    }
}
