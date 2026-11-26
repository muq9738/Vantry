//! The window's own state: theme, the sidebar's filter, the dock, the
//! settings view and its catalogs, the plugin panel, the remote desktop
//! view, and the small dialogs (rename, model picker). None of it is the
//! Core's; what lasts goes to `desktop-state.json` ([`super::persist`]).

use std::collections::{BTreeMap, BTreeSet};

use serde_json::Value;
use vantry_protocol::WorkbenchResponse;

use super::{
    sidebar::{SidebarFilter, StatusFilter},
    workbench::DockTab,
};
use crate::bridge::workbench::{
    SettingsData,
    actions::{McpServerSpec, PanelAction, PanelView, RdStatus},
};

/// Light, dark, or the system's.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum ThemePreference {
    #[default]
    System,
    Light,
    Dark,
}

impl ThemePreference {
    pub fn label(self) -> &'static str {
        match self {
            ThemePreference::System => "跟随系统",
            ThemePreference::Light => "浅色",
            ThemePreference::Dark => "深色",
        }
    }

    pub fn wire(self) -> &'static str {
        match self {
            ThemePreference::System => "system",
            ThemePreference::Light => "light",
            ThemePreference::Dark => "dark",
        }
    }

    pub fn parse(text: &str) -> Self {
        match text {
            "light" => ThemePreference::Light,
            "dark" => ThemePreference::Dark,
            _ => ThemePreference::System,
        }
    }

    /// The next one (the sidebar's quick switch).
    pub fn next(self) -> Self {
        match self {
            ThemePreference::System => ThemePreference::Light,
            ThemePreference::Light => ThemePreference::Dark,
            ThemePreference::Dark => ThemePreference::System,
        }
    }

    pub const ALL: [ThemePreference; 3] = [
        ThemePreference::System,
        ThemePreference::Dark,
        ThemePreference::Light,
    ];
}

/// A page of the settings view.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SettingsPage {
    General,
    Models,
    /// Remote access, pairing and environments.
    Remote,
    /// Sandbox and the default approval policy.
    Permissions,
    /// The updater.
    About,
    Usage,
    Appearance,
}

impl SettingsPage {
    pub const ALL: [SettingsPage; 6] = [
        Self::General, Self::Appearance, Self::Permissions, Self::Models, Self::Remote, Self::About,
    ];

    pub fn label(self) -> &'static str {
        match self {
            SettingsPage::General => "通用",
            SettingsPage::Models => "提供方",
            SettingsPage::Remote => "环境",
            SettingsPage::Permissions => "Agent 与权限",
            SettingsPage::About => "关于与更新",
            SettingsPage::Usage => "用量统计",
            SettingsPage::Appearance => "外观",
        }
    }
}

/// A secret typed into a form (an API key): kept only until it is sent,
/// never printed (`Debug` shows its length only).
#[derive(Clone, PartialEq, Eq, Default)]
pub struct Secret(String);

impl Secret {
    pub fn new(text: impl Into<String>) -> Self {
        Secret(text.into())
    }

    pub fn expose(&self) -> &str {
        &self.0
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    pub fn clear(&mut self) {
        self.0.clear();
    }
}

impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Secret(<{} chars>)", self.0.chars().count())
    }
}

/// The result line of the last settings operation.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Feedback {
    pub ok: bool,
    pub text: String,
}

/// The settings' forms and running operations.
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct AlmaShareStatus {
    pub enabled: bool,
    pub share_id: String,
    pub thread_id: String,
    pub title: String,
    pub ready: bool,
    pub relay: String,
    pub relay_rejected: bool,
    pub phones: usize,
    pub error: String,
}

#[derive(Debug, Clone, Default, PartialEq)]
pub struct AlmaPairingForm {
    pub dialog_id: u64,
    pub thread_id: String,
    pub title: String,
    pub status: Option<AlmaShareStatus>,
    pub link: Secret,
    pub relay: String,
    pub key: Secret,
    pub advanced: bool,
    pub confirm_stop: bool,
    pub busy: bool,
    pub refreshing: bool,
    pub error: String,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct SettingsForms {
    pub alma: Option<AlmaPairingForm>,
    /// Operations under way, by key (`mcp.probe:figma`): their buttons wait.
    pub busy: BTreeSet<String>,
    pub feedback: Option<Feedback>,
    /// The add-MCP-server form, when open.
    pub mcp_draft: Option<McpServerSpec>,
    /// The pairing code (or link) to redeem, and a name for the machine.
    pub pair_code: String,
    pub pair_dialog: bool,
    pub pair_wait: String,
    pub repair_peer: String,
    pub pair_name: String,
    /// Pi quick setup: provider, key (secret), default model.
    pub pi_provider: String,
    pub pi_key: Secret,
    pub pi_model: String,
    /// A peer whose revocation waits for its confirmation.
    pub confirm_revoke: Option<String>,
}

/// One line of a plugin panel's output log.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PanelLog {
    pub ok: bool,
    pub text: String,
}

/// The most lines a panel's log keeps.
pub const MAX_PANEL_LOG: usize = 50;

/// A plugin's native panel (`MainView::Plugin`).
#[derive(Debug, Clone, PartialEq, Default)]
pub struct PluginPanel {
    pub plugin_id: String,
    pub page: String,
    pub loading: bool,
    pub view: Option<PanelView>,
    /// The plugin answered plain text instead of a panel.
    pub text: Option<String>,
    pub error: Option<String>,
    /// The action running now (its id).
    pub running: Option<String>,
    /// The form fields' values.
    pub values: BTreeMap<String, String>,
    pub log: Vec<PanelLog>,
}

impl PluginPanel {
    pub fn new(plugin_id: &str, page: &str) -> Self {
        PluginPanel {
            plugin_id: plugin_id.to_owned(),
            page: page.to_owned(),
            loading: true,
            ..PluginPanel::default()
        }
    }

    /// The panel loaded (or not).
    pub fn on_loaded(&mut self, result: Result<String, String>) {
        self.loading = false;
        match result {
            Ok(output) => {
                self.error = None;
                self.show(&output);
            }
            Err(error) => self.error = Some(error),
        }
    }

    /// Shows `output`: a panel replaces the shown one (form values the user
    /// typed stay), text shows as is.
    fn show(&mut self, output: &str) {
        match PanelView::parse(output) {
            Some(view) => {
                for field in view.all_fields() {
                    self.values.entry(field.key).or_insert(field.value);
                }
                self.view = Some(view);
                self.text = None;
            }
            None => self.text = Some(output.to_owned()),
        }
    }

    /// The action `id` of the shown panel.
    pub fn action(&self, id: &str) -> Option<PanelAction> {
        self.view.as_ref()?.all_actions().into_iter().find(|a| a.id == id)
    }

    /// Starts action `id`: its input with the form's values, unless another
    /// runs or the panel has no such action.
    pub fn begin_run(&mut self, id: &str) -> Option<Value> {
        if self.running.is_some() {
            return None;
        }
        let action = self.action(id)?;
        self.running = Some(id.to_owned());
        Some(crate::bridge::workbench::actions::action_input(&action, &self.values))
    }

    /// Action `id` answered: a panel refreshes the view, text goes to the
    /// log; an error goes to the log too.
    pub fn on_ran(&mut self, id: &str, result: Result<String, String>) {
        if self.running.as_deref() == Some(id) {
            self.running = None;
        }
        let label = self.action(id).map(|a| a.label).unwrap_or_else(|| id.to_owned());
        let line = match result {
            Ok(output) => {
                if PanelView::parse(&output).is_some() {
                    self.show(&output);
                    PanelLog { ok: true, text: format!("{label}：已完成") }
                } else {
                    let text = output.trim();
                    PanelLog {
                        ok: true,
                        text: if text.is_empty() { format!("{label}：已完成") } else { format!("{label}：{text}") },
                    }
                }
            }
            Err(error) => PanelLog { ok: false, text: format!("{label} 失败：{error}") },
        };
        self.log.push(line);
        if self.log.len() > MAX_PANEL_LOG {
            let drop = self.log.len() - MAX_PANEL_LOG;
            self.log.drain(..drop);
        }
    }
}

/// What fills the main column.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum MainView {
    #[default]
    Conversation,
    RemoteDesktop,
    /// A plugin's panel (its id).
    Plugin(String),
}

/// The settings' catalogs, as loaded.
#[derive(Debug, Clone, PartialEq, Default)]
pub enum Catalogs {
    #[default]
    NotLoaded,
    Loading,
    Loaded(std::sync::Arc<SettingsData>),
    Failed(String),
}

/// The remote desktop view's connection.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum RemoteLink {
    #[default]
    Idle,
    Connecting { peer: String },
    /// Frames flow (Wave 3's media stream).
    Connected { peer: String },
    Failed { peer: String, reason: String },
}

/// The stream's resolution.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Resolution {
    #[default]
    Auto,
    Hd,
    FullHd,
    Native,
}

impl Resolution {
    pub const ALL: [Resolution; 4] = [
        Resolution::Auto,
        Resolution::Hd,
        Resolution::FullHd,
        Resolution::Native,
    ];

    pub fn label(self) -> &'static str {
        match self {
            Resolution::Auto => "自动",
            Resolution::Hd => "1280×720",
            Resolution::FullHd => "1920×1080",
            Resolution::Native => "原生",
        }
    }
}

/// The stream's quality.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Quality {
    Low,
    #[default]
    Balanced,
    High,
}

impl Quality {
    pub const ALL: [Quality; 3] = [Quality::Low, Quality::Balanced, Quality::High];

    pub fn label(self) -> &'static str {
        match self {
            Quality::Low => "流畅",
            Quality::Balanced => "均衡",
            Quality::High => "清晰",
        }
    }

    pub fn wire(self) -> &'static str {
        match self {
            Quality::Low => "low",
            Quality::Balanced => "balanced",
            Quality::High => "high",
        }
    }
}

impl Resolution {
    pub fn wire(self) -> &'static str {
        match self {
            Resolution::Auto => "auto",
            Resolution::Hd => "1280x720",
            Resolution::FullHd => "1920x1080",
            Resolution::Native => "native",
        }
    }
}

/// The remote desktop view.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RemoteDesktopState {
    pub link: RemoteLink,
    pub resolution: Resolution,
    pub quality: Quality,
    /// What the Core says (`rd.status` / the session's answer).
    pub status: Option<RdStatus>,
    /// A stop is on its way.
    pub stopping: bool,
    /// Keyboard and pointer go to the remote machine.
    pub control: bool,
    /// The clipboard syncs both ways.
    pub clipboard: bool,
    /// Scale the picture to the view (else 1:1 with scrolling).
    pub fit: bool,
    /// Latest live frame data URI received from the viewer channel.
    pub frame_uri: Option<String>,
    pub frame_seq: u64,
    pub frame_size: (u32, u32),
    /// Rendered canvas dimensions in points for normalizing pointer coordinates.
    pub canvas_size: (u32, u32),
    /// Direct `vantry-lan://...` link input.
    pub direct_link: String,
    /// Active host sharing LAN link when this machine is sharing.
    pub host_lan_link: String,
}

impl RemoteDesktopState {
    /// Starts connecting to `peer`; false when already on it.
    pub fn connect(&mut self, peer: &str) -> bool {
        match &self.link {
            RemoteLink::Connecting { peer: p } | RemoteLink::Connected { peer: p } if p == peer => {
                false
            }
            _ => {
                self.link = RemoteLink::Connecting {
                    peer: peer.to_owned(),
                };
                true
            }
        }
    }

    /// The media bridge answered for `peer`: frames flow, or why not.
    pub fn on_result(&mut self, peer: &str, result: Result<(), String>) {
        if !matches!(&self.link, RemoteLink::Connecting { peer: p } if p == peer) {
            return;
        }
        self.link = match result {
            Ok(()) => RemoteLink::Connected {
                peer: peer.to_owned(),
            },
            Err(reason) => RemoteLink::Failed {
                peer: peer.to_owned(),
                reason,
            },
        };
        if !matches!(self.link, RemoteLink::Connected { .. }) {
            self.control = false;
        }
    }

    pub fn disconnect(&mut self) {
        self.link = RemoteLink::Idle;
        self.control = false;
        self.stopping = false;
    }

    /// The peer of the session that runs or starts, if any.
    pub fn peer(&self) -> Option<&str> {
        match &self.link {
            RemoteLink::Connecting { peer } | RemoteLink::Connected { peer } => Some(peer),
            _ => None,
        }
    }

    /// The session's options for `rd.session.start`.
    pub fn session_options(&self, peer: &str) -> crate::bridge::workbench::actions::RdSessionOptions {
        crate::bridge::workbench::actions::RdSessionOptions {
            peer: peer.to_owned(),
            resolution: self.resolution.wire().to_owned(),
            quality: self.quality.wire().to_owned(),
            control: self.control,
            clipboard: self.clipboard,
        }
    }

    /// The Core's status: a session it reports ended ends here too.
    pub fn on_status(&mut self, status: RdStatus) {
        if let RemoteLink::Connected { peer } = &self.link
            && !status.session_error.is_empty()
        {
            self.link = RemoteLink::Failed {
                peer: peer.clone(),
                reason: status.session_error.clone(),
            };
            self.control = false;
        }
        self.status = Some(status);
    }

    /// What the status line adds: size, frame rate, missing permissions.
    pub fn detail(&self) -> String {
        let Some(status) = &self.status else {
            return String::new();
        };
        let mut parts = Vec::new();
        if matches!(self.link, RemoteLink::Connected { .. }) && status.width > 0 {
            parts.push(format!("{}×{}", status.width, status.height));
            if status.fps > 0 {
                parts.push(format!("{} fps", status.fps));
            }
        }
        let missing = status.missing_permissions();
        if !missing.is_empty() {
            parts.push(format!("需授权：{}", missing.join("、")));
        }
        parts.join(" · ")
    }

    /// Input control only makes sense while connected.
    pub fn toggle_control(&mut self) -> bool {
        if matches!(self.link, RemoteLink::Connected { .. }) {
            self.control = !self.control;
        }
        self.control
    }

    pub fn status_label(&self) -> String {
        match &self.link {
            RemoteLink::Idle => "未连接".to_owned(),
            RemoteLink::Connecting { peer } => format!("正在连接 {peer}…"),
            RemoteLink::Connected { peer } => format!("已连接 {peer}"),
            RemoteLink::Failed { peer, reason } => format!("{peer}：{reason}"),
        }
    }
}

/// The rename dialog.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Rename {
    pub thread_id: String,
    pub text: String,
}

/// A destructive action waiting for its confirmation.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Confirm {
    Delete { thread_id: String, title: String },
    Rewind { thread_id: String, run_id: String },
}

/// The window's state.
#[derive(Debug, Clone, PartialEq)]
pub struct UiState {
    pub theme: ThemePreference,
    pub theme_preset: crate::theme::ThemePreset,
    pub sidebar_open: bool,
    /// Full-window navigation while the main stage is narrow.
    pub mobile_nav: bool,
    pub filter: SidebarFilter,
    pub dock_open: bool,
    pub dock_tab: DockTab,
    pub dock_widths: std::collections::BTreeMap<DockTab, f32>,
    pub dock_maximized: bool,
    pub locale: super::language::Language,
    pub send_with_enter: bool,
    pub manual_delivery: bool,
    pub child_panel: Option<(String, String)>,
    pub expanded_task_parent: Option<String>,
    pub settings: Option<SettingsPage>,
    pub catalogs: Catalogs,
    pub view: MainView,
    pub rename: Option<Rename>,
    pub confirm: Option<Confirm>,
    pub attachment_preview: Option<(vantry_protocol::Attachment, String)>,
    pub model_picker: bool,
    /// The composer's machine picker (`本机` under the card) is open.
    pub machine_picker: bool,
    /// The composer's project folder palette is open.
    pub folder_picker: bool,
    /// The model picker's search (and free entry).
    pub model_query: String,
    /// Default policy for new threads.
    pub default_policy: String,
    /// User extensions on (the Pi runtime loads them).
    pub extensions_enabled: bool,
    /// MCP servers switched off here.
    pub mcp_disabled: BTreeSet<String>,
    pub remote: RemoteDesktopState,
    /// The remote target named on the command line, if any.
    pub remote_target: Option<String>,
    pub forms: SettingsForms,
    /// The plugin panel shown (`MainView::Plugin`).
    pub plugin_panel: Option<PluginPanel>,
    /// Software Update modal dialog visibility.
    pub update_dialog: bool,
    /// Version the user explicitly chose "跳过此版本" for (persisted).
    pub skipped_update_version: String,
    /// Latest live update info (synced from startup check, settings load, and progress polling).
    pub update_info: Option<vantry_protocol::AppUpdateInfo>,
    /// Last error from an update operation, shown inside the update dialog/card.
    pub update_error: Option<String>,
}

impl Default for UiState {
    fn default() -> Self {
        UiState {
            theme: ThemePreference::System,
            theme_preset: Default::default(),
            sidebar_open: true,
            mobile_nav: false,
            filter: SidebarFilter::default(),
            dock_open: false,
            dock_tab: DockTab::Files,
            dock_widths: Default::default(),
            dock_maximized: false,
            locale: Default::default(),
            send_with_enter: true,
            manual_delivery: false,
            child_panel: None,
            expanded_task_parent: None,
            settings: None,
            catalogs: Catalogs::NotLoaded,
            view: MainView::Conversation,
            rename: None,
            confirm: None,
            attachment_preview: None,
            model_picker: false,
            machine_picker: false,
            folder_picker: false,
            model_query: String::new(),
            default_policy: "ask".to_owned(),
            extensions_enabled: true,
            mcp_disabled: BTreeSet::new(),
            remote: RemoteDesktopState {
                fit: true,
                clipboard: true,
                ..RemoteDesktopState::default()
            },
            remote_target: None,
            forms: SettingsForms::default(),
            plugin_panel: None,
            update_dialog: false,
            skipped_update_version: String::new(),
            update_info: None,
            update_error: None,
        }
    }
}

impl UiState {
    /// Opens the dock on `tab`, or closes it when `tab` is already shown.
    pub fn toggle_dock_tab(&mut self, tab: DockTab) {
        if self.dock_open && self.dock_tab == tab {
            self.dock_maximized = false;
            self.dock_open = false;
        } else {
            self.dock_open = true;
            self.dock_tab = tab;
        }
    }

    /// Opens settings on `page`; true when the catalogs should load.
    pub fn open_settings(&mut self, page: SettingsPage) -> bool {
        self.settings = Some(page);
        if matches!(self.catalogs, Catalogs::NotLoaded | Catalogs::Failed(_)) {
            self.catalogs = Catalogs::Loading;
            return true;
        }
        false
    }

    pub fn on_catalogs(&mut self, result: Result<SettingsData, String>) {
        self.catalogs = match result {
            Ok(data) => {
                if data.update.available || !data.update.latest_version.is_empty() {
                    self.update_info = Some(data.update.clone());
                }
                Catalogs::Loaded(std::sync::Arc::new(data))
            }
            Err(reason) => Catalogs::Failed(reason),
        };
    }

    /// Updates live update info both in `UiState` and in loaded `SettingsData`.
    pub fn sync_update_info(&mut self, info: vantry_protocol::AppUpdateInfo) {
        if !info.error.is_empty() {
            self.update_error = Some(info.error.clone());
        } else if info.phase != "failed" {
            self.update_error = None;
        }
        self.update_info = Some(info.clone());
        if let Catalogs::Loaded(data) = &mut self.catalogs {
            std::sync::Arc::make_mut(data).update = info;
        }
    }

    /// Latest known update info, preferring live `update_info` over cached catalog data.
    pub fn active_update(&self) -> Option<&vantry_protocol::AppUpdateInfo> {
        self.update_info
            .as_ref()
            .or_else(|| self.data().map(|d| &d.update))
    }

    /// Switches MCP server `name` on or off here.
    pub fn toggle_mcp(&mut self, name: &str) {
        if !self.mcp_disabled.remove(name) {
            self.mcp_disabled.insert(name.to_owned());
        }
    }

    /// Whether MCP server `name` is on (its catalog state, unless switched
    /// off here).
    pub fn mcp_enabled(&self, name: &str, catalog_enabled: bool) -> bool {
        catalog_enabled && !self.mcp_disabled.contains(name)
    }

    pub fn set_query(&mut self, query: &str) {
        self.filter.query = query.to_owned();
    }

    pub fn set_status_filter(&mut self, status: StatusFilter) {
        self.filter.status = status;
    }

    pub fn begin_rename(&mut self, thread_id: &str, title: &str) {
        self.rename = Some(Rename {
            thread_id: thread_id.to_owned(),
            text: title.to_owned(),
        });
    }

    /// Closes the rename dialog, giving what to rename.
    pub fn finish_rename(&mut self) -> Option<Rename> {
        self.rename
            .take()
            .filter(|r| !r.text.trim().is_empty())
    }

    /// Shows plugin `id`'s panel (or the remote desktop for its page); true
    /// when the panel should load.
    pub fn open_plugin(&mut self, id: &str, page: &str) -> bool {
        self.settings = None;
        if page == "remote-desktop" || id == "remote-desktop" || id == "vantry-remote-desktop" {
            self.view = MainView::RemoteDesktop;
            return false;
        }
        self.view = MainView::Plugin(id.to_owned());
        let page = if page.is_empty() { "main" } else { page };
        if self
            .plugin_panel
            .as_ref()
            .is_some_and(|p| p.plugin_id == id && p.page == page && (p.loading || p.view.is_some()))
        {
            return false;
        }
        self.plugin_panel = Some(PluginPanel::new(id, page));
        true
    }

    /// Starts settings operation `key`; false while it runs.
    pub fn begin_op(&mut self, key: &str) -> bool {
        if !self.forms.busy.insert(key.to_owned()) {
            return false;
        }
        self.forms.feedback = None;
        true
    }

    /// Operation `key` (`action`) ended: its response goes into the shown
    /// catalogs, its outcome into the feedback line.
    pub fn end_op(
        &mut self,
        key: &str,
        action: &str,
        result: Result<WorkbenchResponse, String>,
        done: &str,
    ) {
        self.forms.busy.remove(key);
        match result {
            Ok(response) => {
                if let Catalogs::Loaded(data) = &mut self.catalogs {
                    std::sync::Arc::make_mut(data).absorb(action, &response);
                }
                if action == crate::bridge::workbench::actions::names::RD_STATUS
                    || action.starts_with("rd.session")
                {
                    if let Ok(status) = RdStatus::read(&response) {
                        self.remote.on_status(status);
                    }
                }
                let text = if response.output.trim().is_empty() || response.output.trim_start().starts_with('{') {
                    done.to_owned()
                } else {
                    response.output.trim().to_owned()
                };
                if !text.is_empty() {
                    self.forms.feedback = Some(Feedback { ok: true, text });
                }
            }
            Err(error) => {
                self.forms.feedback = Some(Feedback {
                    ok: false,
                    text: if done.is_empty() { error } else { format!("{done}失败：{error}") },
                })
            }
        }
    }

    /// Whether operation `key` runs.
    pub fn op_busy(&self, key: &str) -> bool {
        self.forms.busy.contains(key)
    }

    /// The loaded settings, if they are.
    pub fn data(&self) -> Option<&SettingsData> {
        match &self.catalogs {
            Catalogs::Loaded(data) => Some(data),
            _ => None,
        }
    }

    /// The catalog's thinking levels and model ids (the composer's
    /// thinking switch).
    pub fn thinking_catalog(&self) -> (Vec<String>, Vec<String>) {
        match self.data() {
            Some(data) => (
                data.models.thinking_levels.clone(),
                data.models.models.iter().flatten().map(vantry_protocol::ModelInfo::key).collect(),
            ),
            None => (Vec::new(), Vec::new()),
        }
    }

    /// The model picker's choices: catalog models matching the query (id or
    /// name), then the query itself as a free entry when it is not one.
    pub fn model_choices(&self) -> Vec<(String, String)> {
        let Catalogs::Loaded(data) = &self.catalogs else {
            return self.free_model_entry();
        };
        let query = self.model_query.trim().to_lowercase();
        let mut out: Vec<(String, String)> = data
            .models
            .models
            .iter()
            .flatten()
            .filter(|m| {
                if query.is_empty() {
                    return true;
                }
                let key = m.key();
                key.to_lowercase().contains(&query)
                    || m.id.to_lowercase().contains(&query)
                    || m.provider.to_lowercase().contains(&query)
                    || m.name.to_lowercase().contains(&query)
            })
            .map(|m| {
                let key = m.key();
                let label = if m.name.is_empty() {
                    key.clone()
                } else {
                    m.name.clone()
                };
                (key, label)
            })
            .collect();
        if !out.iter().any(|(id, _)| id.eq_ignore_ascii_case(self.model_query.trim())) {
            out.extend(self.free_model_entry());
        }
        out
    }

    fn free_model_entry(&self) -> Vec<(String, String)> {
        let query = self.model_query.trim();
        if query.is_empty() {
            Vec::new()
        } else {
            vec![(query.to_owned(), format!("使用 “{query}”"))]
        }
    }
}

#[cfg(test)]
mod tests {
    use vantry_protocol::{ModelCatalog, ModelInfo};

    use super::*;
    use crate::bridge::workbench::actions::names;

    fn panel_json(badge: &str) -> String {
        serde_json::json!({
            "title": "用量", "badge": badge,
            "widgets": [
                {"kind": "form", "fields": [{"key": "days", "value": "7"}]},
                {"kind": "actions", "actions": [{"id": "refresh", "label": "刷新", "input": {"op": "refresh"}}]}
            ]
        })
        .to_string()
    }

    #[test]
    fn plugin_panels_load_run_and_log() {
        let mut panel = PluginPanel::new("usage", "main");
        assert!(panel.loading);
        panel.on_loaded(Ok(panel_json("就绪")));
        assert!(!panel.loading);
        assert_eq!(panel.view.as_ref().unwrap().badge, "就绪");
        assert_eq!(panel.values["days"], "7");
        panel.values.insert("days".into(), "30".into());
        let input = panel.begin_run("refresh").unwrap();
        assert_eq!(input["values"]["days"], "30");
        assert_eq!(panel.begin_run("refresh"), None, "one action at a time");
        panel.on_ran("refresh", Ok(panel_json("已刷新")));
        assert_eq!(panel.view.as_ref().unwrap().badge, "已刷新");
        assert_eq!(panel.values["days"], "30", "typed values stay");
        assert!(panel.log.last().unwrap().text.contains("刷新"));
        assert!(panel.begin_run("ghost").is_none());
        panel.begin_run("refresh").unwrap();
        panel.on_ran("refresh", Err("插件崩溃".into()));
        assert!(!panel.log.last().unwrap().ok);
        panel.begin_run("refresh").unwrap();
        panel.on_ran("refresh", Ok("纯文本结果".into()));
        assert_eq!(panel.log.last().unwrap().text, "刷新：纯文本结果");
        for _ in 0..80 {
            panel.on_ran("refresh", Ok(String::new()));
        }
        assert_eq!(panel.log.len(), MAX_PANEL_LOG);
        let mut plain = PluginPanel::new("x", "main");
        plain.on_loaded(Ok("hello".into()));
        assert_eq!(plain.text.as_deref(), Some("hello"));
        plain.on_loaded(Err("not_ready".into()));
        assert_eq!(plain.error.as_deref(), Some("not_ready"));
    }

    #[test]
    fn settings_operations_fold_responses_and_report() {
        let mut ui = UiState::default();
        ui.on_catalogs(Ok(SettingsData::default()));
        assert!(ui.begin_op("sandbox"));
        assert!(!ui.begin_op("sandbox"), "already running");
        assert!(ui.op_busy("sandbox"));
        let response = WorkbenchResponse {
            output: r#"{"mode":"off","supported":true}"#.into(),
            ..WorkbenchResponse::default()
        };
        ui.end_op("sandbox", names::SANDBOX_SET, Ok(response), "已切换沙盒模式");
        assert!(!ui.op_busy("sandbox"));
        assert_eq!(ui.data().unwrap().sandbox.as_ref().unwrap().mode, "off");
        assert_eq!(ui.forms.feedback.as_ref().unwrap().text, "已切换沙盒模式");
        ui.begin_op("update");
        let response = WorkbenchResponse { output: "更新已下载。".into(), ..WorkbenchResponse::default() };
        ui.end_op("update", names::UPDATE_DOWNLOAD, Ok(response), "下载");
        assert_eq!(ui.forms.feedback.as_ref().unwrap().text, "更新已下载。");
        ui.begin_op("pair");
        ui.end_op("pair", names::REMOTE_PAIR_REDEEM, Err("配对码已过期".into()), "配对");
        let feedback = ui.forms.feedback.clone().unwrap();
        assert!(!feedback.ok && feedback.text == "配对失败：配对码已过期");
        // An rd answer updates the remote desktop's status.
        ui.begin_op("rd");
        let response = WorkbenchResponse { output: r#"{"permissions":{"screen":false}}"#.into(), ..WorkbenchResponse::default() };
        ui.end_op("rd", names::RD_STATUS, Ok(response), "");
        assert_eq!(ui.remote.detail(), "需授权：屏幕录制");
    }

    #[test]
    fn secrets_never_print() {
        let mut forms = SettingsForms::default();
        forms.pi_key = Secret::new("sk-live-123456");
        let printed = format!("{forms:?}");
        assert!(!printed.contains("sk-live"), "{printed}");
        assert!(printed.contains("14 chars"));
        assert_eq!(forms.pi_key.expose(), "sk-live-123456");
        forms.pi_key.clear();
        assert!(forms.pi_key.is_empty());
    }

    #[test]
    fn the_remote_session_carries_its_options_and_reports_failures() {
        let mut remote = RemoteDesktopState { quality: Quality::High, resolution: Resolution::FullHd, ..Default::default() };
        let options = remote.session_options("p1");
        assert_eq!((options.resolution.as_str(), options.quality.as_str()), ("1920x1080", "high"));
        assert!(remote.connect("p1"));
        assert_eq!(remote.peer(), Some("p1"));
        remote.on_result("p1", Ok(()));
        remote.toggle_control();
        remote.on_status(RdStatus { width: 1920, height: 1080, fps: 30, ..RdStatus::default() });
        assert_eq!(remote.detail(), "1920×1080 · 30 fps");
        remote.on_status(RdStatus { session_error: "对方结束了共享".into(), ..RdStatus::default() });
        assert!(matches!(remote.link, RemoteLink::Failed { .. }));
        assert!(!remote.control);
        remote.disconnect();
        assert_eq!(remote.peer(), None);
    }

    #[test]
    fn themes_cycle_and_round_trip() {
        let mut theme = ThemePreference::System;
        for _ in 0..3 {
            assert_eq!(ThemePreference::parse(theme.wire()), theme);
            theme = theme.next();
        }
        assert_eq!(theme, ThemePreference::System);
        assert_eq!(ThemePreference::parse("purple"), ThemePreference::System);
    }

    #[test]
    fn the_dock_toggles_per_tab() {
        let mut ui = UiState::default();
        ui.toggle_dock_tab(DockTab::Git);
        assert!(ui.dock_open);
        assert_eq!(ui.dock_tab, DockTab::Git);
        ui.toggle_dock_tab(DockTab::Terminal);
        assert_eq!((ui.dock_open, ui.dock_tab), (true, DockTab::Terminal));
        ui.toggle_dock_tab(DockTab::Terminal);
        assert!(!ui.dock_open);
    }

    #[test]
    fn settings_load_their_catalogs_once() {
        let mut ui = UiState::default();
        assert!(ui.open_settings(SettingsPage::Models));
        assert!(!ui.open_settings(SettingsPage::About), "already loading");
        ui.on_catalogs(Err("offline".into()));
        assert!(ui.open_settings(SettingsPage::Models), "a failure retries");
        ui.on_catalogs(Ok(SettingsData::default()));
        assert!(!ui.open_settings(SettingsPage::Models));
        ui.toggle_mcp("figma");
        assert!(!ui.mcp_enabled("figma", true));
        ui.toggle_mcp("figma");
        assert!(ui.mcp_enabled("figma", true));
        assert!(!ui.mcp_enabled("figma", false));
    }

    #[test]
    fn the_model_picker_filters_and_offers_free_entry() {
        let mut ui = UiState::default();
        ui.model_query = "custom/model".into();
        assert_eq!(ui.model_choices().len(), 1);
        ui.on_catalogs(Ok(SettingsData {
            models: ModelCatalog {
                models: Some(vec![
                    ModelInfo { id: "anthropic/opus".into(), name: "Opus".into(), ..Default::default() },
                    ModelInfo { id: "openai/gpt".into(), name: "GPT".into(), ..Default::default() },
                ]),
                ..ModelCatalog::default()
            },
            ..SettingsData::default()
        }));
        ui.model_query = "opus".into();
        let choices = ui.model_choices();
        assert_eq!(choices[0].0, "anthropic/opus");
        assert_eq!(choices.len(), 2, "the free entry follows");
        ui.model_query = "anthropic/opus".into();
        assert_eq!(ui.model_choices().len(), 1);
        ui.model_query.clear();
        assert_eq!(ui.model_choices().len(), 2);
    }

    #[test]
    fn the_remote_desktop_connects_fails_and_controls() {
        let mut remote = RemoteDesktopState::default();
        assert!(!remote.toggle_control(), "no control while idle");
        assert!(remote.connect("peer-1"));
        assert!(!remote.connect("peer-1"), "already connecting");
        remote.on_result("peer-2", Ok(()));
        assert!(matches!(remote.link, RemoteLink::Connecting { .. }), "another peer's answer");
        remote.on_result("peer-1", Ok(()));
        assert_eq!(remote.status_label(), "已连接 peer-1");
        assert!(remote.toggle_control());
        assert!(remote.connect("peer-3"));
        remote.on_result("peer-3", Err("媒体流未接入".into()));
        assert!(!remote.control);
        assert!(remote.status_label().contains("媒体流未接入"));
        remote.disconnect();
        assert_eq!(remote.link, RemoteLink::Idle);
    }

    #[test]
    fn rename_and_plugins() {
        let mut ui = UiState::default();
        ui.begin_rename("t1", "旧");
        ui.rename.as_mut().unwrap().text = "  ".into();
        assert_eq!(ui.finish_rename(), None, "a blank title renames nothing");
        ui.begin_rename("t1", "新");
        assert_eq!(ui.finish_rename().unwrap().text, "新");
        ui.settings = Some(SettingsPage::Models);
        assert!(!ui.open_plugin("remote-desktop", "remote-desktop"));
        assert_eq!((ui.view.clone(), ui.settings), (MainView::RemoteDesktop, None));
        assert!(ui.open_plugin("usage", "main"));
        assert_eq!(ui.view, MainView::Plugin("usage".into()));
        assert!(!ui.open_plugin("usage", "main"), "already loading");
        ui.set_status_filter(StatusFilter::Running);
    }
}
