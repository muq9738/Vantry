//! The sidebar: new thread, the thread list with status dots, the footer.

use nsh_core::prelude::*;

use super::{dot, status_color, status_label};
use crate::{
    controller,
    state::ThreadStatus,
    theme::{PALETTE, metrics},
};

/// One sidebar row's data.
#[derive(Debug, Clone, PartialEq)]
struct ThreadRow {
    id: String,
    title: String,
    status: ThreadStatus,
}

pub(crate) fn sidebar() -> impl IntoElement {
    let rows = computed(|| {
        controller::state().with(|s| {
            s.visible_threads()
                .into_iter()
                .map(|t| ThreadRow {
                    id: t.id.clone(),
                    title: if t.title.trim().is_empty() {
                        "未命名会话".to_owned()
                    } else {
                        t.title.clone()
                    },
                    status: s.thread_status(&t.id),
                })
                .collect::<Vec<_>>()
        })
    });
    let selected = computed(|| controller::state().with(|s| s.selected.clone()));
    let new_selected = computed(move || selected.get().is_none());
    let count = computed(move || rows.with(Vec::len));
    let attention = computed(move || {
        rows.with(|rows| {
            rows.iter()
                .filter(|r| r.status == ThreadStatus::NeedsApproval)
                .count()
        })
    });

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
                        .text_size(px(15.0))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(PALETTE.ink),
                ),
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
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_4()
                .pt_4()
                .pb_1()
                .child(
                    text(move || format!("会话 · {}", count.get()))
                        .text_size(px(11.5))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(PALETTE.faint),
                )
                .child(when(
                    move || attention.get() > 0,
                    move || {
                        view().px_2().rounded_full().bg(PALETTE.accent).child(
                            text(move || format!("待处理 {}", attention.get()))
                                .text_size(px(10.5))
                                .text_color(PALETTE.on_accent),
                        )
                    },
                )),
        )
        .child(
            scroll().flex_1().child(
                view()
                    .flex_col()
                    .gap_1()
                    .px_2()
                    .pb_3()
                    .child(each(
                        move || rows.get(),
                        |row: &ThreadRow| row.id.clone(),
                        move |row, _| thread_row(row, selected),
                    ))
                    .child(when(
                        move || count.get() == 0,
                        || {
                            text("还没有会话。写下第一条消息即可开始。")
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

fn thread_row(row: ReadSignal<ThreadRow>, selected: Memo<Option<String>>) -> impl IntoElement {
    let is_selected = computed(move || {
        selected.with(|s| s.as_deref() == Some(row.with(|r| r.id.clone()).as_str()))
    });
    nsh_base::button()
        .on_press(move || controller::select_thread(row.with_untracked(|r| r.id.clone())))
        .selected(move || is_selected.get())
        .flex_row()
        .items_center()
        .gap_2()
        .px_3()
        .py_2()
        .rounded(px(metrics::CARD_RADIUS))
        .bg(move || {
            if is_selected.get() {
                PALETTE.card
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .hover(|s| s.bg(PALETTE.soft))
        .active(|s| s.opacity(0.75))
        .child(dot(move || status_color(row.with(|r| r.status))))
        .child(
            text(move || row.with(|r| r.title.clone()))
                .flex_1()
                .number_of_lines(1u32)
                .text_size(px(13.0))
                .text_color(PALETTE.ink),
        )
        .child(when(
            move || row.with(|r| r.status != ThreadStatus::Idle),
            move || {
                text(move || status_label(row.with(|r| r.status)))
                    .text_size(px(10.5))
                    .text_color(move || status_color(row.with(|r| r.status)))
            },
        ))
}

fn footer() -> impl IntoElement {
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_4()
        .py_3()
        .border_t_1()
        .border_t_color(PALETTE.line_soft)
        .child(
            text(move || controller::state().with(|s| s.endpoint_label.clone()))
                .flex_1()
                .number_of_lines(1u32)
                .text_size(px(11.5))
                .text_color(PALETTE.faint),
        )
}
