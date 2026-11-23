//! The header: the sidebar toggle, `workspace / thread` breadcrumb with its
//! status capsule and the orchestration capsule (subtasks), the session's
//! actions (rename, fork, merge back), the connection state, the dock's
//! tool switches and the settings entry.

use nsh_core::prelude::*;

use super::{dot, ghost_button, link_button, pill, run_status_color, status_color, status_label};
use crate::{
    controller,
    state::{
        Connection, ThreadStatus,
        sidebar::task_badge,
        ui::{MainView, SettingsPage},
        workbench::DockTab,
    },
    theme::{PALETTE, metrics},
};

pub(crate) fn header() -> impl IntoElement {
    let workspace = computed(|| controller::state().with(|s| s.workspace_label.clone()));
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
    let revision = computed(|| controller::state().with(|s| s.sidebar_revision));
    let subtasks = computed(move || {
        revision.get();
        let id = selected.get();
        controller::state().with_untracked(|s| id.map(|id| s.subtasks(&id)).unwrap_or_default())
    });
    let can_merge = computed(move || {
        revision.get();
        let id = selected.get();
        controller::state().with_untracked(|s| id.is_some_and(|id| s.can_merge_back(&id)))
    });
    let connection = computed(|| controller::state().with(|s| s.connection.clone()));
    let connection_color = move || match connection.get() {
        Connection::Live => PALETTE.success,
        Connection::Connecting => PALETTE.faint,
        Connection::Reconnecting { .. } | Connection::Resyncing { .. } => PALETTE.warning,
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
            _ => s.endpoint_label.clone(),
        })
    };
    let dock = computed(|| controller::ui().with(|u| u.dock_open.then_some(u.dock_tab)));
    let tasks_open = signal(false);
    let thread_id = move || selected.get_untracked().unwrap_or_default();

    view()
        .h(px(metrics::HEADER_HEIGHT))
        .flex_shrink_0()
        .flex_row()
        .items_center()
        .gap_3()
        .px_5()
        .border_b_1()
        .border_b_color(PALETTE.line_soft)
        .child(link_button("☰", controller::toggle_sidebar).accessibility_label("切换侧栏"))
        .child(
            text(move || workspace.get())
                .number_of_lines(1u32)
                .text_size(px(13.0))
                .text_color(PALETTE.muted),
        )
        .child(text("/").text_size(px(13.0)).text_color(PALETTE.faint))
        .child(
            text(move || match view_kind.get() {
                MainView::Conversation => title.get(),
                MainView::RemoteDesktop => "远程桌面".to_owned(),
                MainView::Plugin(id) => format!("插件 · {id}"),
            })
            .flex_shrink(1.0)
            .number_of_lines(1u32)
            .text_size(px(14.0))
            .font_weight(FontWeight::SEMIBOLD)
            .text_color(PALETTE.ink),
        )
        .child(when(
            move || status.get() != ThreadStatus::Idle,
            move || {
                view()
                    .flex_row()
                    .items_center()
                    .gap_1()
                    .px_2()
                    .py_0p5()
                    .rounded_full()
                    .bg(PALETTE.soft)
                    .child(dot(move || status_color(status.get())))
                    .child(
                        text(move || status_label(status.get()))
                            .text_size(px(11.0))
                            .text_color(PALETTE.muted),
                    )
            },
        ))
        .child(when(
            move || subtasks.with(|t| !t.is_empty()),
            move || {
                view()
                    .relative()
                    .child(
                        nsh_base::button()
                            .accessibility_label("任务统筹")
                            .on_press(move || tasks_open.set(!tasks_open.get_untracked()))
                            .px_2()
                            .py_0p5()
                            .rounded_full()
                            .border_1()
                            .border_color(PALETTE.line)
                            .active(|s| s.opacity(0.7))
                            .child(
                                text(move || {
                                    subtasks.with(|t| {
                                        let done = t.iter().filter(|(_, _, s)| s.terminal()).count();
                                        format!("子任务 {done}/{}", t.len())
                                    })
                                })
                                .text_size(px(11.0))
                                .text_color(PALETTE.muted),
                            ),
                    )
                    .child(when(
                        move || tasks_open.get(),
                        move || {
                            view()
                                .absolute()
                                .top(px(30.0))
                                .left(px(0.0))
                                .w(px(280.0))
                                .flex_col()
                                .gap_1()
                                .p_2()
                                .rounded(px(10.0))
                                .bg(PALETTE.card)
                                .border_1()
                                .border_color(PALETTE.line)
                                .shadow_md()
                                .child(each(
                                    move || subtasks.get(),
                                    |t: &(String, String, vantry_protocol::RunStatus)| t.0.clone(),
                                    move |task, _| {
                                        let id = move || task.with_untracked(|t| t.0.clone());
                                        nsh_base::button()
                                            .on_press(move || {
                                                tasks_open.set(false);
                                                controller::select_thread(id());
                                            })
                                            .flex_row()
                                            .items_center()
                                            .gap_2()
                                            .px_2()
                                            .py_1()
                                            .rounded(px(6.0))
                                            .hover(|s| s.bg(PALETTE.soft))
                                            .child(dot(move || run_status_color(task.with(|t| t.2))))
                                            .child(
                                                text(move || task.with(|t| t.1.clone()))
                                                    .flex_1()
                                                    .number_of_lines(1u32)
                                                    .text_size(px(12.5))
                                                    .text_color(PALETTE.ink),
                                            )
                                            .child(
                                                text(move || task_badge(task.with(|t| t.2)))
                                                    .text_size(px(10.5))
                                                    .text_color(move || run_status_color(task.with(|t| t.2))),
                                            )
                                    },
                                ))
                        },
                    ))
            },
        ))
        .child(view().flex_1())
        .child(when(
            move || selected.with(Option::is_some) && view_kind.get() == MainView::Conversation,
            move || {
                view()
                    .flex_row()
                    .gap_0p5()
                    .child(link_button("重命名", move || controller::begin_rename(thread_id())))
                    .child(link_button("分叉", move || controller::fork(thread_id(), None)))
                    .child(when(
                        move || can_merge.get(),
                        move || link_button("合并回原会话", move || controller::merge_back(thread_id())),
                    ))
            },
        ))
        .child(
            view()
                .flex_row()
                .items_center()
                .p_0p5()
                .rounded_full()
                .bg(PALETTE.soft)
                .child(dock_pill(DockTab::Files, dock))
                .child(dock_pill(DockTab::Git, dock))
                .child(dock_pill(DockTab::Terminal, dock))
                .child(dock_pill(DockTab::Preview, dock)),
        )
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .py_1()
                .rounded_full()
                .border_1()
                .border_color(PALETTE.line)
                .tooltip(text(connection_reason).text_size(px(12.0)))
                .child(dot(connection_color))
                .child(
                    text(connection_text)
                        .text_size(px(11.5))
                        .text_color(PALETTE.muted),
                ),
        )
        .child(ghost_button("设置", || {
            controller::open_settings(SettingsPage::Models)
        }))
}

fn dock_pill(tab: DockTab, dock: Memo<Option<DockTab>>) -> impl IntoElement {
    pill(tab.label(), move || dock.get() == Some(tab), move || controller::toggle_dock(tab))
}
