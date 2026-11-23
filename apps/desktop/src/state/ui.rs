//! The window's own state: theme, the sidebar's filter, the dock, the
//! settings view and its catalogs, the plugin panel, the remote desktop
//! view, and the small dialogs (rename, model picker). None of it is the
//! Core's; what lasts goes to `desktop-state.json` ([`super::persist`]).

use std::collections::BTreeSet;

use super::{
    sidebar::{SidebarFilter, StatusFilter},
    workbench::DockTab,
};
use crate::bridge::workbench::SettingsData;

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
        ThemePreference::Light,
        ThemePreference::Dark,
    ];
}

/// A page of the settings view.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SettingsPage {
    Models,
    Extensions,
    Appearance,
    Permissions,
    Remote,
    About,
}

impl SettingsPage {
    pub const ALL: [SettingsPage; 6] = [
        SettingsPage::Models,
        SettingsPage::Extensions,
        SettingsPage::Appearance,
        SettingsPage::Permissions,
        SettingsPage::Remote,
        SettingsPage::About,
    ];

    pub fn label(self) -> &'static str {
        match self {
            SettingsPage::Models => "模型与 Provider",
            SettingsPage::Extensions => "扩展与 MCP",
            SettingsPage::Appearance => "外观",
            SettingsPage::Permissions => "默认权限",
            SettingsPage::Remote => "远端与配对",
            SettingsPage::About => "关于与更新",
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
    Loaded(Box<SettingsData>),
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

/// The remote desktop view.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct RemoteDesktopState {
    pub link: RemoteLink,
    pub resolution: Resolution,
    /// Keyboard and pointer go to the remote machine.
    pub control: bool,
    /// The clipboard syncs both ways.
    pub clipboard: bool,
    /// Scale the picture to the view (else 1:1 with scrolling).
    pub fit: bool,
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
    pub sidebar_open: bool,
    pub filter: SidebarFilter,
    pub dock_open: bool,
    pub dock_tab: DockTab,
    pub settings: Option<SettingsPage>,
    pub catalogs: Catalogs,
    pub view: MainView,
    pub rename: Option<Rename>,
    pub confirm: Option<Confirm>,
    pub model_picker: bool,
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
}

impl Default for UiState {
    fn default() -> Self {
        UiState {
            theme: ThemePreference::System,
            sidebar_open: true,
            filter: SidebarFilter::default(),
            dock_open: false,
            dock_tab: DockTab::Files,
            settings: None,
            catalogs: Catalogs::NotLoaded,
            view: MainView::Conversation,
            rename: None,
            confirm: None,
            model_picker: false,
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
        }
    }
}

impl UiState {
    /// Opens the dock on `tab`, or closes it when `tab` is already shown.
    pub fn toggle_dock_tab(&mut self, tab: DockTab) {
        if self.dock_open && self.dock_tab == tab {
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
            Ok(data) => Catalogs::Loaded(Box::new(data)),
            Err(reason) => Catalogs::Failed(reason),
        };
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

    pub fn toggle_collapsed(&mut self, id: &str) {
        if !self.filter.collapsed.remove(id) {
            self.filter.collapsed.insert(id.to_owned());
        }
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

    /// Shows plugin `id`'s panel (or the remote desktop for its page).
    pub fn open_plugin(&mut self, id: &str, page: &str) {
        self.view = if page == "remote-desktop" || id == "remote-desktop" {
            MainView::RemoteDesktop
        } else {
            MainView::Plugin(id.to_owned())
        };
        self.settings = None;
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
                query.is_empty()
                    || m.id.to_lowercase().contains(&query)
                    || m.name.to_lowercase().contains(&query)
            })
            .map(|m| {
                (
                    m.id.clone(),
                    if m.name.is_empty() {
                        m.id.clone()
                    } else {
                        m.name.clone()
                    },
                )
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
        ui.settings = Some(SettingsPage::Extensions);
        ui.open_plugin("remote-desktop", "remote-desktop");
        assert_eq!((ui.view.clone(), ui.settings), (MainView::RemoteDesktop, None));
        ui.open_plugin("usage", "main");
        assert_eq!(ui.view, MainView::Plugin("usage".into()));
        ui.set_status_filter(StatusFilter::Running);
        ui.toggle_collapsed("p");
        assert!(ui.filter.collapsed.contains("p"));
    }
}
