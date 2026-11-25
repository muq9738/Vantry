//! Vantry's native desktop on NSH (pure Rust UI set).
//!
//! Modules:
//! - [`cli`]: the `vantry` command line (0.1.42 compatible) → [`cli::LaunchConfig`];
//! - [`bridge`]: [`bridge::CoreBridge`] over the real `vantry_client::CoreClient`
//!   (and the in-memory [`bridge::MockBridge`], the `--peer`
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
pub mod icons;
pub mod service;
pub mod state;
pub mod theme;
mod views;

use std::sync::OnceLock;

use nsh_core::{
    desktop::{TitleBarOptions, TitleBarStyle, WindowAttributes},
    element::desktop::current_window,
    prelude::*,
};

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
    let mut title_bar = TitleBarOptions::default();
    title_bar.style = TitleBarStyle::Hidden;
    title_bar.height = Some(px(theme::metrics::HEADER_HEIGHT));
    let mut attrs = WindowAttributes::default();
    attrs.title_bar = Some(title_bar);
    current_window().set(attrs);
    controller::start(&launch_config());
    let preset = computed(|| controller::ui().with(|u| (u.theme_preset,u.locale)));
    dynamic(move || {
        let (preset,locale) = preset.get();
        untracked(|| {
            provide_context(locale);
            theme::install_preset(preset);
            workbench()
        })
    })
}

pub(crate) fn tr(label: &str) -> &str {
    use_context::<state::language::Language>().unwrap_or_default().label(label)
}

fn workbench() -> impl IntoElement {
    let sidebar_open = computed(controller::sidebar_visible);
    let nav_only = computed(move || controller::narrow_window() && sidebar_open.get());
    let stage_width = computed(move || {
        let width = nsh_core::element::window().width.0;
        if !controller::narrow_window() && sidebar_open.get() {
            width - theme::metrics::sidebar_width(width)
        } else {
            width
        }
    });
    let dock_open = computed(|| controller::ui().with(|u| u.dock_open));
    let main = computed(|| controller::ui().with(|u| u.view.clone()));
    let sheet = computed(move || {
        dock_open.get() && stage_width.get() < theme::metrics::DOCK_SIDE_BREAKPOINT
    });
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
    let maximized = computed(move || {
        dock_open.get() && !sheet.get() && controller::ui().with(|u| u.dock_maximized)
    });
    on_key("mod+,", |_| {
        controller::open_settings(state::ui::SettingsPage::General)
    });
    on_key("mod+k", |_| controller::focus_search());
    on_key("mod+n", |_| controller::new_thread());
    view()
        .flex_1()
        .flex_row()
        .bg(theme::palette().canvas)
        .child(when(move || sidebar_open.get(), views::sidebar::sidebar))
        .child(when(
            move || !nav_only.get(),
            move || {
                view().flex_1().min_w_0().flex_col().child(
                    view()
                        .flex_1()
                        .min_h_0()
                        .flex_row()
                        .child(
                            view()
                                .flex_1()
                                .min_w_0()
                                .flex_col()
                                .child(views::header::header())
                                .child(dynamic(move || match main.get() {
                                    MainView::Conversation if maximized.get() => {
                                        views::dock::dock().into_element()
                                    }
                                    MainView::Conversation => {
                                        views::conversation(sheet).into_element()
                                    }
                                    MainView::RemoteDesktop => {
                                        views::remote_desktop::remote_desktop().into_element()
                                    }
                                    MainView::Plugin(id) => {
                                        views::plugin_panel::plugin_panel(id).into_element()
                                    }
                                })),
                        )
                        .child(when(
                            move || {
                                main.get() == MainView::Conversation
                                    && dock_open.get()
                                    && !sheet.get()
                                    && !maximized.get()
                            },
                            move || {
                                let width = computed(move || {
                                    controller::ui().with(|u| {
                                        let default = if matches!(
                                            u.dock_tab,
                                            state::workbench::DockTab::Git
                                                | state::workbench::DockTab::Terminal
                                        ) {
                                            520.0
                                        } else {
                                            440.0
                                        };
                                        u.dock_widths
                                            .get(&u.dock_tab)
                                            .copied()
                                            .unwrap_or(default)
                                            .min(stage_width.get() - 380.0)
                                            .max(340.0)
                                    })
                                });
                                let start = signal(0.0);
                                let click = signal(None::<std::time::Instant>);
                                view()
                                    .relative()
                                    .w(move || px(width.get()))
                                    .flex_shrink_0()
                                    .child(views::dock::dock())
                                    .child(
                                        nsh_base::resize_handle()
                                            .edge(nsh_base::HandleEdge::Leading)
                                            .accessibility_label("调整工具坞宽度")
                                            .on_drag_start(move || start.set(width.get_untracked()))
                                            .on_drag_move(move |offset: &Points| {
                                                controller::ui().update(|u| {
                                                    u.dock_widths.insert(
                                                        u.dock_tab,
                                                        (start.get_untracked() - offset.0).clamp(
                                                            340.0,
                                                            (stage_width.get_untracked() - 380.0)
                                                                .max(340.0),
                                                        ),
                                                    );
                                                })
                                            })
                                            .on_drag_end(move |offset: &Points| {
                                                let now = std::time::Instant::now();
                                                if offset.0.abs() < 2.0
                                                    && click.get_untracked().is_some_and(|last| {
                                                        now.duration_since(last).as_millis() < 500
                                                    })
                                                {
                                                    controller::ui().update(|u| {
                                                        u.dock_widths.remove(&u.dock_tab);
                                                    });
                                                    click.set(None);
                                                } else {
                                                    click
                                                        .set((offset.0.abs() < 2.0).then_some(now));
                                                }
                                            }),
                                    )
                            },
                        )),
                )
            },
        ))
        .child(views::approval_modal::approval_modal())
        .child(views::dock::preview_dialog())
        .child(views::git::dialog())
        .child(views::settings::settings())
        .child(views::dialogs::rename_dialog())
        .child(views::dialogs::confirm_dialog())
        .child(views::dialogs::attachment_preview_dialog())
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
