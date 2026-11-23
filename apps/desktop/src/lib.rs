//! Vantry's native desktop on NSH (pure Rust UI set).
//!
//! Modules:
//! - [`cli`]: the `vantry` command line (0.1.42 compatible) → [`cli::LaunchConfig`];
//! - [`bridge`]: [`bridge::CoreBridge`], protocol v1 to the Core (Unix socket
//!   HTTP) and the in-memory [`bridge::MockBridge`];
//! - [`state`]: [`state::AppState`] and every transition, without I/O;
//! - [`theme`]: Stardust tokens and palette;
//! - `controller`: the state's signal, the connection loop, intents;
//! - `views`: sidebar, header, timeline, composer, approval modal.

pub mod bridge;
pub mod cli;
mod controller;
pub mod state;
pub mod theme;
mod views;

use std::sync::OnceLock;

use nsh_core::prelude::*;

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

/// The window: sidebar | header / timeline / composer, the approval modal
/// over it all.
#[component]
pub fn App() -> impl IntoElement {
    theme::install();
    controller::start(&launch_config());
    view()
        .flex_1()
        .flex_row()
        .bg(theme::PALETTE.canvas)
        .child(views::sidebar::sidebar())
        .child(
            view()
                .flex_1()
                .flex_col()
                .child(views::header::header())
                .child(views::timeline::timeline())
                .child(views::composer::composer()),
        )
        .child(views::approval_modal::approval_modal())
}
