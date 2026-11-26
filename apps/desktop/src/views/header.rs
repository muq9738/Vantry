//! The Stardust header (`v0.1.42` layout): transparent draggable title bar,
//! `[monogram] workspace / thread` breadcrumb with a subtle status capsule and
//! subtasks dropdown, and a crisp Lucide icon toolbar on the right (search,
//! segmented dock switches, overflow menu, dock toggle).

use nsh_core::{
    element::desktop::{MenuItem, current_window, popup_menu},
    prelude::*,
};

use super::{dot, icon_button, icon_pill, status_color, status_label};
use crate::{
    controller,
    icons::{Icon, icon, project_badge},
    state::{
        Connection, ThreadStatus,
        ui::{MainView, SettingsPage},
        workbench::DockTab,
    },
    theme::{metrics, palette},
};

pub(crate) fn header() -> impl IntoElement {
    let window = current_window();
    let colors = palette();
    let sidebar_open = computed(controller::sidebar_visible);
    let workspace = computed(|| controller::state().with(|s| s.current_project_key()));
    let branch = computed(|| {
        controller::bench().with(|b|b.git.status.as_ref().map(|s|s.branch.clone())).unwrap_or_else(||controller::state().with(|s|s.current_workspace().map(|w|w.branch.clone()).unwrap_or_default()))
    });
    let selected = computed(|| controller::state().with(|s| s.selected.clone()));
    let title = computed(|| {
        controller::state().with(|s| match s.selected_thread() {
            Some(thread) if !thread.title.trim().is_empty() => thread.title.clone(),
            Some(_) => "未命名会话".to_owned(),
            None => "新会话".to_owned(),
        })
    });
    let view_kind = computed(|| controller::ui().with(|u| u.view.clone()));
    let status = computed(|| {
        controller::state().with(|s| {
            s.selected
                .as_deref()
                .map_or(ThreadStatus::Idle, |id| s.thread_status(id))
        })
    });
    let has_completed = computed(|| {
        controller::state().with(|s| {
            s.selected.as_deref().is_some_and(|id| {
                s.runs
                    .iter()
                    .any(|r| r.thread_id == id && r.status == vantry_protocol::RunStatus::Completed)
            })
        })
    });
    let revision = computed(|| controller::state().with(|s| s.sidebar_revision));
    let can_merge = computed(move || {
        revision.get();
        let id = selected.get();
        controller::state().with_untracked(|s| id.is_some_and(|id| s.can_merge_back(&id)))
    });
    let connection = computed(|| controller::state().with(|s| s.connection.clone()));
    let connection_color = move || match connection.get() {
        Connection::Live => colors.success,
        Connection::Connecting => colors.faint,
        Connection::Starting { .. } => colors.accent,
        Connection::Reconnecting { .. } | Connection::Resyncing { .. } => colors.warning,
    };
    let connection_text = move || {
        controller::state().with(|s| match &s.connection {
            Connection::Reconnecting { attempt, .. } | Connection::Resyncing { attempt, .. } => {
                format!("{} · 第 {attempt} 次", s.connection.label())
            }
            other => other.label().to_owned(),
        })
    };
    let connection_reason = move || {
        controller::state().with(|s| match &s.connection {
            Connection::Reconnecting { reason, .. } | Connection::Resyncing { reason, .. } => {
                reason.clone()
            }
            Connection::Starting { detail } => detail.clone(),
            _ => s.endpoint_label.clone(),
        })
    };
    let dock = computed(|| controller::ui().with(|u| u.dock_open.then_some(u.dock_tab)));
    let remote = computed(|| controller::ui().with(|u| u.remote_target.clone()));
    let more_open = signal(false);
    let thread_id = move || selected.get_untracked().unwrap_or_default();

    view()
        .h(px(metrics::HEADER_HEIGHT))
        .flex_shrink_0()
        .flex_row()
        .items_center()
        .gap_2()
        .pl(move || {
            if sidebar_open.get() {
                px(20.0)
            } else {
                let inset = window.title_bar().left.0;
                px((inset + 10.0).max(20.0))
            }
        })
        .pr_4()
        .border_b_1()
        .border_b_color(colors.line_soft)
        .bg(colors.canvas)
        .window_drag(true)
        // Sidebar toggle appears in the header when the sidebar is collapsed.
        .child(when(
            move || !sidebar_open.get(),
            || icon_button(Icon::PanelLeft, "展开侧栏", controller::toggle_sidebar),
        ))
        // Project monogram badge + breadcrumb
        .child(when(
            move || workspace.with(|key| !key.is_empty()),
            move || {
                let name = computed(move || {
                    workspace.with(|key| crate::state::sidebar::project_name(key).to_owned())
                });
                view()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .child(dynamic(move || project_badge(&name.get())))
                    .child(
                        text(move || name.get())
                            .number_of_lines(1u32)
                            .text_size(px(13.0))
                            .text_color(colors.muted),
                    )
                    .child(when(
                        move || {
                            view_kind.get() == MainView::Conversation
                                && branch.with(|name| !name.is_empty())
                        },
                        move || {
                            nsh_base::button()
                                .accessibility_label("分支操作")
                                .on_press(controller::open_branch_menu)
                                .flex_row()
                                .items_center()
                                .gap_1()
                                .h(px(22.0))
                                .px_2()
                                .rounded(px(6.0))
                                .bg(colors.soft)
                                .hover(|s| s.bg(colors.card))
                                .active(|s| s.opacity(0.7))
                                .child(icon(Icon::GitBranch, 11.5, colors.muted))
                                .child(
                                    text(move || branch.get())
                                        .max_w(px(140.0))
                                        .number_of_lines(1u32)
                                        .font_family("Menlo")
                                        .text_size(px(11.0))
                                        .text_color(colors.ink),
                                )
                        },
                    ))
                    .child(text("/").text_size(px(13.0)).text_color(colors.faint))
            },
        ))
        .child(
            text(move || match view_kind.get() {
                MainView::Conversation => title.get(),
                MainView::RemoteDesktop => "远程桌面".to_owned(),
                MainView::Plugin(id) => format!("插件 · {id}"),
            })
            .flex_shrink(1.0)
            .number_of_lines(1u32)
            .text_size(px(13.5))
            .font_weight(FontWeight::SEMIBOLD)
            .text_color(colors.ink),
        )
        // Subtle status capsule (matches v0.1.42's `● 已完成` green pill & live states)
        .child(when(
            move || {
                view_kind.get() == MainView::Conversation
                    && (status.get() != ThreadStatus::Idle || has_completed.get())
            },
            move || {
                let completed = move || status.get() == ThreadStatus::Idle && has_completed.get();
                view()
                    .flex_row()
                    .items_center()
                    .gap_1()
                    .px_2()
                    .py_0p5()
                    .rounded_full()
                    .bg(move || {
                        if completed() {
                            colors.success_bg
                        } else {
                            colors.soft
                        }
                    })
                    .child(dot(move || {
                        if completed() {
                            colors.success
                        } else {
                            status_color(status.get())
                        }
                    }))
                    .child(
                        text(move || {
                            if completed() {
                                "已完成"
                            } else {
                                status_label(status.get())
                            }
                        })
                        .text_size(px(11.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(move || {
                            if completed() {
                                colors.success
                            } else {
                                colors.muted
                            }
                        }),
                    )
            },
        ))
        .child(view().flex_1())
        // Connection status badge: shown prominently only when remote or reconnecting
        .child(when(
            move || remote.with(Option::is_some) || connection.get() != Connection::Live,
            move || {
                view()
                    .flex_row()
                    .items_center()
                    .gap_1p5()
                    .px_2p5()
                    .py_1()
                    .rounded_full()
                    .border_1()
                    .border_color(colors.line)
                    .tooltip(super::tip(connection_reason))
                    .child(dot(connection_color))
                    .child(
                        text(move || {
                            if let Some(target) = remote.get() {
                                format!("{target} · {}", connection_text())
                            } else {
                                connection_text()
                            }
                        })
                        .text_size(px(11.0))
                        .text_color(colors.muted),
                    )
            },
        ))
        // Right icon toolbar matching v0.1.42
        .child(icon_button(
            Icon::Search,
            "搜索会话 (⌘K)",
            controller::focus_search,
        ))
        .child(when(
            move || crate::window_size().width.0 >= 760.0 && dock.get().is_none(),
            move || {
                view()
                    .flex_row()
                    .items_center()
                    .gap_0p5()
                    .p_0p5()
                    .rounded(px(8.0))
                    .bg(colors.soft)
                    .child(dock_icon_pill(Icon::FileText, "文件", DockTab::Files, dock))
                    .child(dock_icon_pill(Icon::GitBranch, "Git", DockTab::Git, dock))
                    .child(dock_icon_pill(
                        Icon::SquareTerminal,
                        "终端",
                        DockTab::Terminal,
                        dock,
                    ))
                    .child(dock_icon_pill(Icon::Blocks, "插件", DockTab::Plugins, dock))
                    .child(dock_icon_pill(Icon::Server, "MCP", DockTab::Mcp, dock))
            },
        ))
        .child(when(
            || crate::window_size().width.0 < 760.0,
            move || {
                icon_button(Icon::Blocks, "工具", move || {
                    let items = DockTab::ALL
                        .into_iter()
                        .map(|tab| {
                            MenuItem::new(tab.label())
                                .checked(dock.get_untracked() == Some(tab))
                                .on_select(move || controller::toggle_dock(tab))
                        })
                        .collect();
                    drop(popup_menu(items));
                })
            },
        ))
        // Overflow menu (`…`) for session & app actions
        .child(
            view()
                .relative()
                .child(icon_button(Icon::Ellipsis, "更多操作", move || {
                    more_open.set(!more_open.get_untracked());
                }))
                .child(when(
                    move || more_open.get(),
                    move || {
                        view()
                            .absolute()
                            .top(px(34.0))
                            .right(px(0.0))
                            .w(px(180.0))
                            .flex_col()
                            .gap_0p5()
                            .p_1p5()
                            .rounded(px(10.0))
                            .bg(colors.card)
                            .border_1()
                            .border_color(colors.line)
                            .shadow_md()
                            .child(when(
                                move || selected.with(Option::is_some),
                                move || {
                                    view()
                                        .flex_col()
                                        .gap_0p5()
                                        .child(menu_item(Icon::Copy, "复制会话链接", move || {
                                            more_open.set(false);
                                            controller::copy_thread_link(thread_id());
                                        }))
                                        .child(menu_item(
                                            Icon::Pencil,
                                            "重命名会话",
                                            move || {
                                                more_open.set(false);
                                                controller::begin_rename(thread_id());
                                            },
                                        ))
                                        .child(menu_item(
                                            Icon::GitFork,
                                            "分叉当前会话",
                                            move || {
                                                more_open.set(false);
                                                controller::fork(thread_id(), None);
                                            },
                                        ))
                                        .child(when(
                                            move || can_merge.get(),
                                            move || {
                                                menu_item(
                                                    Icon::GitPullRequest,
                                                    "合并回原会话",
                                                    move || {
                                                        more_open.set(false);
                                                        controller::merge_back(thread_id());
                                                    },
                                                )
                                            },
                                        ))
                                },
                            ))
                            .child(menu_item(Icon::Monitor, "远程桌面", move || {
                                more_open.set(false);
                                let next = if view_kind.get_untracked() == MainView::RemoteDesktop {
                                    MainView::Conversation
                                } else {
                                    MainView::RemoteDesktop
                                };
                                controller::show_view(next);
                            }))
                            .child(menu_item(Icon::Settings, "设置", move || {
                                more_open.set(false);
                                controller::open_settings(SettingsPage::General);
                            }))
                    },
                )),
        )
        // Right panel open/close toggle
        .child(icon_button(
            Icon::PanelRight,
            "会话概览",
            || controller::toggle_dock(DockTab::Summary),
        ))
}

fn dock_icon_pill(
    glyph: Icon,
    label: &'static str,
    tab: DockTab,
    dock: Memo<Option<DockTab>>,
) -> impl IntoElement {
    icon_pill(
        glyph,
        label,
        move || dock.get() == Some(tab),
        move || controller::toggle_dock(tab),
    )
}

fn menu_item(
    glyph: Icon,
    label: &'static str,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let colors = palette();
    nsh_base::button()
        .on_press(on_press)
        .w_full()
        .flex_row()
        .items_center()
        .gap_2()
        .px_2p5()
        .py_1p5()
        .rounded(px(6.0))
        .hover(|s| s.bg(colors.soft))
        .active(|s| s.opacity(0.7))
        .child(icon(glyph, 14.0, colors.muted))
        .child(text(label).text_size(px(12.5)).text_color(colors.ink))
}
