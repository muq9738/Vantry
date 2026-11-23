//! The workbench's views. Each module is one region of the window; they
//! read the state through `computed` memos and change it only through the
//! controller's intents.
//!
//! Layout (desktop-ui.md): sidebar | (header / timeline / composer), with
//! the approval modal over everything. Wave 2 adds the right panel (project,
//! subtasks, tool dock) beside the reading column without moving these.

pub(crate) mod approval_modal;
pub(crate) mod composer;
pub(crate) mod header;
pub(crate) mod sidebar;
pub(crate) mod timeline;

use nsh_core::prelude::*;

use crate::{state::ThreadStatus, theme::PALETTE};

/// The dot color of a thread's status.
pub(crate) fn status_color(status: ThreadStatus) -> AdaptiveColor {
    match status {
        ThreadStatus::Idle => PALETTE.faint,
        ThreadStatus::Running => PALETTE.accent,
        ThreadStatus::NeedsApproval => PALETTE.warning,
        ThreadStatus::Failed => PALETTE.danger,
    }
}

/// What a thread's status says.
pub(crate) fn status_label(status: ThreadStatus) -> &'static str {
    match status {
        ThreadStatus::Idle => "空闲",
        ThreadStatus::Running => "运行中",
        ThreadStatus::NeedsApproval => "待确认",
        ThreadStatus::Failed => "异常",
    }
}

/// A small round dot.
pub(crate) fn dot(color: impl IntoReactive<AdaptiveColor>) -> View {
    view()
        .w(px(7.0))
        .h(px(7.0))
        .rounded_full()
        .flex_shrink_0()
        .bg(color)
}

/// A quiet text button (secondary actions).
pub(crate) fn ghost_button(
    label: impl IntoReactive<SharedString>,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    nsh_base::button()
        .on_press(on_press)
        .flex_row()
        .items_center()
        .px_3()
        .py_1()
        .rounded(px(8.0))
        .border_1()
        .border_color(PALETTE.line)
        .hover(|s| s.bg(PALETTE.soft))
        .active(|s| s.opacity(0.7))
        .disabled_style(|s| s.opacity(0.45))
        .child(text(label).text_size(px(12.5)).text_color(PALETTE.ink))
}

/// The ember primary button.
pub(crate) fn primary_button(
    label: impl IntoReactive<SharedString>,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    nsh_base::button()
        .on_press(on_press)
        .flex_row()
        .items_center()
        .px_4()
        .py_1()
        .rounded(px(8.0))
        .bg(PALETTE.accent)
        .active(|s| s.opacity(0.75))
        .disabled_style(|s| s.opacity(0.4))
        .child(
            text(label)
                .text_size(px(12.5))
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(PALETTE.on_accent),
        )
}
