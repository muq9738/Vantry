//! The header: `workspace / thread` breadcrumb with its status capsule, the
//! connection state, the settings entry.

use nsh_core::prelude::*;

use super::{dot, ghost_button, status_color, status_label};
use crate::{
    controller,
    state::{Connection, ThreadStatus},
    theme::{PALETTE, metrics},
};

pub(crate) fn header() -> impl IntoElement {
    let workspace = computed(|| controller::state().with(|s| s.workspace_label.clone()));
    let title = computed(|| {
        controller::state().with(|s| match s.selected_thread() {
            Some(thread) if !thread.title.trim().is_empty() => thread.title.clone(),
            Some(_) => "未命名会话".to_owned(),
            None => "新会话".to_owned(),
        })
    });
    let status = computed(|| {
        controller::state().with(|s| {
            s.selected
                .as_deref()
                .map_or(ThreadStatus::Idle, |id| s.thread_status(id))
        })
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

    view()
        .h(px(metrics::HEADER_HEIGHT))
        .flex_shrink_0()
        .flex_row()
        .items_center()
        .gap_3()
        .px_5()
        .border_b_1()
        .border_b_color(PALETTE.line_soft)
        .child(
            text(move || workspace.get())
                .number_of_lines(1u32)
                .text_size(px(13.0))
                .text_color(PALETTE.muted),
        )
        .child(text("/").text_size(px(13.0)).text_color(PALETTE.faint))
        .child(
            text(move || title.get())
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
        .child(view().flex_1())
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
            controller::state().update(|s| {
                s.notice = Some(crate::state::Notice {
                    level: crate::state::NoticeLevel::Info,
                    text: "设置面板将在后续版本接入（模型、权限、主题）。".to_owned(),
                });
            });
        }))
}
