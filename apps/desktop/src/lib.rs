//! Vantry's native desktop on NSH (pure Rust UI set).
//!
//! Modules:
//! - [`cli`]: the `vantry` command line (0.1.42 compatible) → [`cli::LaunchConfig`];
//! - [`bridge`]: [`bridge::CoreBridge`] over the real `vantry_client::CoreClient`
//!   (and the in-memory [`bridge::MockBridge`], the `--remote` / `--peer`
//!   route), and [`bridge::WorkbenchBridge`] for files, Git, terminal and
//!   settings;
//! - [`state`]: the conversation ([`state::AppState`], with its incremental
//!   timeline and Markdown), the window ([`state::ui::UiState`]), the dock
//!   ([`state::workbench::WorkbenchState`]) and `desktop-state.json`
//!   ([`state::persist`]), all without I/O;
//! - [`theme`]: Stardust tokens and palette;
//! - `controller`: the signals, the connection loop, persistence, intents;
//! - `views`: sidebar, header, timeline, composer, dock, settings, plugin
//!   panels, remote desktop, dialogs, approval modal;
//! - [`service`]: the `vantry-service` / `vantry-relay` binaries (0.1.42
//!   service modes forwarded, `ensure-core`), and `autostart`, which starts
//!   the local Core when the window opens and it does not answer.

mod autostart;
pub mod bridge;
pub mod cli;
mod controller;
pub mod service;
pub mod state;
pub mod theme;
mod views;

use std::sync::OnceLock;

use nsh_core::prelude::*;

use crate::state::ui::MainView;

static LAUNCH: OnceLock<cli::LaunchConfig> = OnceLock::new();

/// Sets what the window connects to; call before the window starts. Returns
/// false when it was already set.
pub fn set_launch_config(config: cli::LaunchConfig) -> bool {
    LAUNCH.set(config).is_ok()
}

/// The launch config, or the default local one (`nsh run`, tests).
fn launch_config() -> cli::LaunchConfig {
    LAUNCH.get().cloned().unwrap_or_else(|| {
        let data_dir = cli::default_data_dir(&|name| std::env::var(name).ok())
            .unwrap_or_else(|_| std::path::PathBuf::from("."));
        cli::LaunchConfig::default_for(data_dir)
    })
}

/// The app's UI set (`nsh.json` `"rust"`).
#[native_module("vantry/native")]
fn native(registry: &mut Registry) {
    registry.register::<App>();
}

/// The window: sidebar | header / (timeline + composer | remote desktop |
/// plugin panel) | dock; the approval modal, settings and dialogs over it.
#[component]
pub fn App() -> impl IntoElement {
    theme::install();
    controller::start(&launch_config());
    let sidebar_open = computed(|| controller::ui().with(|u| u.sidebar_open));
    let dock_open = computed(|| controller::ui().with(|u| u.dock_open));
    let main = computed(|| controller::ui().with(|u| u.view.clone()));
    // Escape stops the shown run (or closes the model picker); dialogs and
    // inputs still see it.
    on_key_with(
        "escape",
        KeyOptions {
            pass_through: true,
            ..KeyOptions::default()
        },
        |_| controller::escape(),
    );
    view()
        .flex_1()
        .flex_row()
        .bg(theme::PALETTE.canvas)
        .child(when(move || sidebar_open.get(), views::sidebar::sidebar))
        .child(
            view()
                .flex_1()
                .flex_col()
                .child(views::header::header())
                .child(dynamic(move || match main.get() {
                    MainView::Conversation => view()
                        .flex_1()
                        .flex_col()
                        .child(views::timeline::timeline())
                        .child(views::composer::composer())
                        .into_element(),
                    MainView::RemoteDesktop => views::remote_desktop::remote_desktop().into_element(),
                    MainView::Plugin(id) => views::plugin_panel::plugin_panel(id).into_element(),
                })),
        )
        .child(when(move || dock_open.get(), views::dock::dock))
        .child(views::approval_modal::approval_modal())
        .child(views::settings::settings())
        .child(views::dialogs::rename_dialog())
        .child(views::dialogs::confirm_dialog())
}

/// The desktop links NSH, the protocol and the client only: never the Core
/// or the Pi engine (the architecture's red line).
#[cfg(test)]
mod manifest_guard {
    #[test]
    fn the_manifest_names_no_core_or_pi_crate() {
        let manifest = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/Cargo.toml")).unwrap();
        for forbidden in ["vantry-core", "vantry-pi-runtime", "pi-coding-agent"] {
            assert!(!manifest.contains(forbidden), "apps/desktop must not depend on {forbidden}");
        }
        assert!(manifest.contains("vantry-client") && manifest.contains("vantry-protocol"));
    }
}
