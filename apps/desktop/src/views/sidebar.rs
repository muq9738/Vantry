//! The sidebar: search, status filters, new thread, the pinned / active /
//! snoozed / archived groups with delegated subtasks nested under their
//! parent (task badges, fold, jump), the selected row's actions, and the
//! footer (settings, theme, dock, remote desktop).
//!
//! Its rows are rebuilt only when [`AppState::sidebar_revision`] or the
//! filter changes, never per streamed character.
//!
//! [`AppState::sidebar_revision`]: crate::state::AppState::sidebar_revision

use nsh_core::prelude::*;

use super::{badge, caption, dot, link_button, pill, run_status_color, status_color, status_label};
use crate::{
    controller,
    state::{
        ThreadStatus,
        sidebar::{SidebarRow, SidebarSection, StatusFilter, task_badge},
        ui::{MainView, SettingsPage},
        workbench::DockTab,
    },
    theme::{PALETTE, metrics},
};

/// A flattened entry: a group header or a row.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Entry {
    Header {
        group: &'static str,
        count: usize,
        folded: bool,
        archived: bool,
    },
    Row(SidebarRow),
}

fn entries(sections: Vec<SidebarSection>) -> Vec<Entry> {
    let mut out = Vec::new();
    for section in sections {
        out.push(Entry::Header {
            group: section.group.label(),
            count: section.count,
            folded: section.folded,
            archived: section.group == crate::state::sidebar::Group::Archived,
        });
        out.extend(section.rows.into_iter().map(Entry::Row));
    }
    out
}

fn entry_key(entry: &Entry) -> String {
    match entry {
        Entry::Header { group, .. } => format!("group:{group}"),
        Entry::Row(row) => format!("row:{}", row.id),
    }
}

pub(crate) fn sidebar() -> impl IntoElement {
    let revision = computed(|| controller::state().with(|s| s.sidebar_revision));
    let filter = computed(|| controller::ui().with(|u| u.filter.clone()));
    let list = computed(move || {
        revision.get();
        let filter = filter.get();
        controller::state().with_untracked(|s| entries(s.sidebar_sections(&filter, chrono::Utc::now())))
    });
    let selected = computed(|| controller::state().with(|s| s.selected.clone()));
    let new_selected = computed(move || selected.get().is_none());
    let attention = computed(move || {
        revision.get();
        controller::state().with_untracked(|s| {
            s.thread_statuses()
                .values()
                .filter(|st| matches!(st, ThreadStatus::NeedsApproval))
                .count()
        })
    });
    let query = computed(|| controller::ui().with(|u| u.filter.query.clone()));
    let status = computed(|| controller::ui().with(|u| u.filter.status));

    view()
        .w(px(metrics::SIDEBAR_WIDTH))
        .flex_shrink_0()
        .flex_col()
        .bg(PALETTE.sidebar)
        .border_r_1()
        .border_r_color(PALETTE.line)
        // The macOS window buttons sit over the top of the sidebar.
        .child(
            view()
                .h(px(metrics::HEADER_HEIGHT))
                .flex_row()
                .items_end()
                .px_4()
                .pb_2()
                .child(
                    text("Vantry")
                        .flex_1()
                        .text_size(px(15.0))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(PALETTE.ink),
                )
                .child(when(
                    move || attention.get() > 0,
                    move || {
                        view().px_2().rounded_full().bg(PALETTE.accent).child(
                            text(move || format!("待我处理 {}", attention.get()))
                                .text_size(px(10.5))
                                .text_color(PALETTE.on_accent),
                        )
                    },
                )),
        )
        .child(
            view()
                .mx_3()
                .px_3()
                .py_1p5()
                .rounded(px(9.0))
                .bg(PALETTE.card)
                .border_1()
                .border_color(PALETTE.line)
                .child(
                    nsh_base::input()
                        .value(move || query.get())
                        .on_change(|text: &str| controller::set_query(text))
                        .placeholder("搜索会话")
                        .placeholder_color(move || color(PALETTE.faint))
                        .auto_correct(false)
                        .text_size(px(12.5))
                        .text_color(PALETTE.ink),
                ),
        )
        .child(
            view()
                .flex_row()
                .gap_1()
                .px_3()
                .pt_2()
                .child(filter_pill(StatusFilter::All, status))
                .child(filter_pill(StatusFilter::Running, status))
                .child(filter_pill(StatusFilter::Attention, status)),
        )
        .child(
            nsh_base::button()
                .accessibility_label("新建会话")
                .on_press(controller::new_thread)
                .mx_3()
                .mt_2()
                .px_3()
                .py_2()
                .flex_row()
                .items_center()
                .gap_2()
                .rounded(px(metrics::CARD_RADIUS))
                .bg(move || {
                    if new_selected.get() {
                        PALETTE.card
                    } else {
                        AdaptiveColor::TRANSPARENT
                    }
                })
                .hover(|s| s.bg(PALETTE.soft))
                .active(|s| s.opacity(0.7))
                .child(
                    text("＋")
                        .text_color(PALETTE.accent)
                        .font_weight(FontWeight::SEMIBOLD),
                )
                .child(text("新建会话").text_size(px(13.0)).text_color(PALETTE.ink)),
        )
        .child(
            scroll().flex_1().child(
                view()
                    .flex_col()
                    .gap_0p5()
                    .px_2()
                    .pt_2()
                    .pb_3()
                    .child(each(
                        move || list.get(),
                        entry_key,
                        move |entry, _| entry_view(entry, selected),
                    ))
                    .child(when(
                        move || list.with(Vec::is_empty),
                        move || {
                            text(move || {
                                if query.with(|q| q.trim().is_empty())
                                    && status.get() == StatusFilter::All
                                {
                                    "还没有会话。写下第一条消息即可开始。"
                                } else {
                                    "没有匹配的会话。"
                                }
                            })
                            .text_size(px(12.0))
                            .text_color(PALETTE.faint)
                            .px_3()
                            .py_2()
                        },
                    )),
            ),
        )
        .child(footer())
}

fn filter_pill(value: StatusFilter, current: Memo<StatusFilter>) -> impl IntoElement {
    pill(
        value.label(),
        move || current.get() == value,
        move || controller::set_status_filter(value),
    )
}

fn entry_view(entry: ReadSignal<Entry>, selected: Memo<Option<String>>) -> AnyElement {
    let is_header = entry.with_untracked(|e| matches!(e, Entry::Header { .. }));
    if is_header {
        return group_header(entry).into_element();
    }
    let row = computed(move || match entry.get() {
        Entry::Row(row) => Some(row),
        Entry::Header { .. } => None,
    });
    thread_row(row, selected).into_element()
}

fn group_header(entry: ReadSignal<Entry>) -> impl IntoElement {
    let info = move || match entry.get() {
        Entry::Header {
            group,
            count,
            folded,
            archived,
        } => (group, count, folded, archived),
        Entry::Row(_) => ("", 0, false, false),
    };
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_2()
        .pt_3()
        .pb_1()
        .child(caption(move || {
            let (group, count, _, _) = info();
            format!("{group} · {count}")
        }))
        .child(view().flex_1())
        .child(when(
            move || info().3,
            move || {
                link_button(
                    move || if info().2 { "展开" } else { "收起" },
                    controller::toggle_archived_group,
                )
            },
        ))
}

fn thread_row(row: Memo<Option<SidebarRow>>, selected: Memo<Option<String>>) -> impl IntoElement {
    let id = move || row.with_untracked(|r| r.as_ref().map(|r| r.id.clone()).unwrap_or_default());
    let is_selected = computed(move || {
        let id = row.with(|r| r.as_ref().map(|r| r.id.clone()));
        selected.with(|s| s.is_some() && *s == id)
    });
    let field = move |f: fn(&SidebarRow) -> String| move || row.with(|r| r.as_ref().map(f).unwrap_or_default());
    let status = move || row.with(|r| r.as_ref().map_or(ThreadStatus::Idle, |r| r.status));
    let depth = move || row.with(|r| r.as_ref().map_or(0, |r| r.depth));
    let children = move || row.with(|r| r.as_ref().map_or(0, |r| r.children));
    let collapsed = move || row.with(|r| r.as_ref().is_some_and(|r| r.collapsed));
    let task = move || row.with(|r| r.as_ref().and_then(|r| r.task_status));
    let pinned = move || row.with(|r| r.as_ref().is_some_and(|r| r.pinned));
    let archived = move || row.with(|r| r.as_ref().is_some_and(|r| r.archived));
    let snoozed = move || row.with(|r| r.as_ref().is_some_and(|r| r.snoozed));

    view()
        .flex_col()
        .rounded(px(metrics::CARD_RADIUS))
        .bg(move || {
            if is_selected.get() {
                PALETTE.card
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .child(
            view()
                .flex_row()
                .items_center()
                .pl(move || px(8.0 + f32::from(depth()) * 14.0))
                .child(when(
                    move || children() > 0,
                    move || {
                        nsh_base::button()
                            .accessibility_label("展开子任务")
                            .on_press(move || controller::toggle_collapsed(id()))
                            .px_1()
                            .active(|s| s.opacity(0.6))
                            .child(
                                text(move || if collapsed() { "▸" } else { "▾" })
                                    .text_size(px(10.0))
                                    .text_color(PALETTE.faint),
                            )
                    },
                ))
                .child(when(
                    move || children() == 0 && depth() > 0,
                    || text("└").text_size(px(10.0)).text_color(PALETTE.faint).px_1(),
                ))
                .child(
                    nsh_base::button()
                        .on_press(move || controller::select_thread(id()))
                        .selected(move || is_selected.get())
                        .flex_1()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .pr_2()
                        .py_1p5()
                        .rounded(px(metrics::CARD_RADIUS))
                        .hover(|s| s.bg(PALETTE.soft))
                        .active(|s| s.opacity(0.75))
                        .child(dot(move || status_color(status())))
                        .child(
                            text(field(|r| r.title.clone()))
                                .flex_1()
                                .number_of_lines(1u32)
                                .text_size(px(13.0))
                                .text_color(move || {
                                    if archived() || snoozed() {
                                        PALETTE.muted
                                    } else {
                                        PALETTE.ink
                                    }
                                }),
                        )
                        .child(when(
                            move || task().is_some(),
                            move || {
                                let status = move || task().unwrap_or_default();
                                badge(move || task_badge(status()), move || run_status_color(status()))
                            },
                        ))
                        .child(when(
                            move || task().is_none() && status() != ThreadStatus::Idle,
                            move || {
                                text(move || status_label(status()))
                                    .text_size(px(10.5))
                                    .text_color(move || status_color(status()))
                            },
                        ))
                        .child(when(
                            move || children() > 0 && collapsed(),
                            move || badge(move || format!("{}", children()), PALETTE.muted),
                        )),
                ),
        )
        .child(when(
            move || is_selected.get(),
            move || {
                view()
                    .flex_row()
                    .flex_wrap()
                    .gap_0p5()
                    .px_2()
                    .pb_1()
                    .pl(move || px(24.0 + f32::from(depth()) * 14.0))
                    .child(link_button("重命名", move || controller::begin_rename(id())))
                    .child(link_button(
                        move || if pinned() { "取消置顶" } else { "置顶" },
                        move || controller::toggle_pin(id()),
                    ))
                    .child(link_button(
                        move || if snoozed() { "唤醒" } else { "延后 1 天" },
                        move || controller::snooze(id(), if snoozed() { None } else { Some(24) }),
                    ))
                    .child(link_button(
                        move || if archived() { "取消归档" } else { "归档" },
                        move || controller::toggle_archive(id()),
                    ))
                    .child(link_button("删除", move || controller::ask_delete(id())))
            },
        ))
}

fn footer() -> impl IntoElement {
    let theme = computed(|| controller::ui().with(|u| u.theme));
    let dock_open = computed(|| controller::ui().with(|u| u.dock_open));
    let remote = computed(|| controller::ui().with(|u| u.view == MainView::RemoteDesktop));
    view()
        .flex_col()
        .gap_1()
        .px_3()
        .py_2()
        .border_t_1()
        .border_t_color(PALETTE.line_soft)
        .child(
            view()
                .flex_row()
                .flex_wrap()
                .gap_0p5()
                .child(link_button("设置", || controller::open_settings(SettingsPage::Models)))
                .child(link_button("Git", || controller::toggle_dock(DockTab::Git)))
                .child(link_button("终端", || controller::toggle_dock(DockTab::Terminal)))
                .child(link_button(
                    move || if dock_open.get() { "收起工具坞" } else { "工具坞" },
                    move || {
                        if dock_open.get_untracked() {
                            controller::close_dock();
                        } else {
                            controller::toggle_dock(DockTab::Files);
                        }
                    },
                ))
                .child(link_button(
                    move || if remote.get() { "返回对话" } else { "远程桌面" },
                    move || {
                        controller::show_view(if remote.get_untracked() {
                            MainView::Conversation
                        } else {
                            MainView::RemoteDesktop
                        })
                    },
                ))
                .child(link_button(
                    move || format!("主题：{}", theme.get().label()),
                    controller::cycle_theme,
                )),
        )
        .child(
            text(move || controller::state().with(|s| s.endpoint_label.clone()))
                .number_of_lines(1u32)
                .text_size(px(11.0))
                .text_color(PALETTE.faint),
        )
}
