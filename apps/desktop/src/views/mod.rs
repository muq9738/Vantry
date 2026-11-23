//! The workbench's views. Each module is one region of the window; they
//! read the states through `computed` memos and change them only through
//! the controller's intents.
//!
//! Layout (desktop-ui.md): sidebar | (header / timeline or remote desktop or
//! plugin panel / composer) | dock, with the approval modal, settings and
//! the small dialogs over everything.

pub(crate) mod approval_modal;
pub(crate) mod composer;
pub(crate) mod dialogs;
pub(crate) mod dock;
pub(crate) mod header;
pub(crate) mod markdown;
pub(crate) mod plugin_panel;
pub(crate) mod remote_desktop;
pub(crate) mod settings;
pub(crate) mod sidebar;
pub(crate) mod timeline;

use nsh_core::prelude::*;
use vantry_protocol::RunStatus;

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

/// A task status's badge color.
pub(crate) fn run_status_color(status: RunStatus) -> AdaptiveColor {
    match status {
        RunStatus::Completed => PALETTE.success,
        RunStatus::Failed | RunStatus::Interrupted => PALETTE.danger,
        RunStatus::Running | RunStatus::Waiting | RunStatus::Queued => PALETTE.accent,
        RunStatus::Cancelled | RunStatus::Unspecified => PALETTE.faint,
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

/// A borderless small text button (row actions, toolbar items).
pub(crate) fn link_button(
    label: impl IntoReactive<SharedString>,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    nsh_base::button()
        .on_press(on_press)
        .px_1p5()
        .py_0p5()
        .rounded(px(6.0))
        .hover(|s| s.bg(PALETTE.soft))
        .active(|s| s.opacity(0.6))
        .disabled_style(|s| s.opacity(0.35))
        .child(text(label).text_size(px(11.5)).text_color(PALETTE.muted))
}

/// A pill that shows whether it is on (tabs, segmented switches, filters).
pub(crate) fn pill(
    label: impl IntoReactive<SharedString>,
    on: impl Fn() -> bool + Clone + 'static,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let (a, b, c, d) = (on.clone(), on.clone(), on.clone(), on);
    nsh_base::button()
        .selected(a)
        .on_press(on_press)
        .px_2p5()
        .py_0p5()
        .rounded_full()
        .bg(move || {
            if b() {
                PALETTE.card
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .border_1()
        .border_color(move || {
            if c() {
                PALETTE.line
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .active(|s| s.opacity(0.7))
        .child(text(label).text_size(px(11.5)).text_color(move || {
            if d() {
                PALETTE.ink
            } else {
                PALETTE.muted
            }
        }))
}

/// A small rounded badge.
pub(crate) fn badge(
    label: impl IntoReactive<SharedString>,
    color: impl IntoReactive<AdaptiveColor>,
) -> View {
    view()
        .px_1p5()
        .rounded(px(5.0))
        .bg(PALETTE.soft)
        .child(text(label).text_size(px(10.0)).text_color(color))
}

/// A section's small caption.
pub(crate) fn caption(label: impl IntoReactive<SharedString>) -> Text {
    text(label)
        .text_size(px(11.0))
        .font_weight(FontWeight::MEDIUM)
        .text_color(PALETTE.faint)
}

/// An on/off switch row: label, hint, its state.
pub(crate) fn switch_row(
    label: impl IntoReactive<SharedString>,
    hint: impl IntoReactive<SharedString>,
    on: impl Fn() -> bool + Clone + 'static,
    toggle: impl Fn() + 'static,
) -> View {
    let (a, b, c) = (on.clone(), on.clone(), on);
    view()
        .flex_row()
        .items_center()
        .gap_3()
        .py_1p5()
        .child(
            view()
                .flex_1()
                .flex_col()
                .child(text(label).text_size(px(13.0)).text_color(PALETTE.ink))
                .child(text(hint).text_size(px(11.5)).text_color(PALETTE.muted)),
        )
        .child(
            nsh_base::button()
                .selected(a)
                .on_press(toggle)
                .w(px(38.0))
                .h(px(22.0))
                .p_0p5()
                .rounded_full()
                .flex_row()
                .justify_start()
                .bg(move || if b() { PALETTE.accent } else { PALETTE.line })
                .active(|s| s.opacity(0.75))
                .child(when(c, || view().w(px(16.0))))
                .child(view().w(px(18.0)).h(px(18.0)).rounded_full().bg(PALETTE.card)),
        )
}
