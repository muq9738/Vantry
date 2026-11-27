//! The workbench's views. Each module is one region of the window; they
//! read the states through `computed` memos and change them only through
//! the controller's intents.
//!
//! Layout (desktop-ui.md): sidebar | (header / timeline or remote desktop or
//! plugin panel / composer) | dock, with the approval modal, settings and
//! the small dialogs over everything.

pub(crate) mod approval_modal;
pub(crate) mod charts;
pub(crate) mod child_panel;
pub(crate) mod composer;
pub(crate) mod dialogs;
pub(crate) mod dock;
pub(crate) mod git;
pub(crate) mod header;
pub(crate) mod markdown;
pub(crate) mod pickers;
pub(crate) mod plugin_panel;
pub(crate) mod preview;
mod reference_input;
pub(crate) use reference_input::remember_copy;
pub(crate) mod remote_desktop;
pub(crate) mod settings;
pub(crate) mod sidebar;
pub(crate) mod timeline;
pub(crate) mod update_dialog;
pub(crate) mod visuals;

use nsh_core::prelude::*;
use vantry_protocol::RunStatus;

use crate::{
    icons::{Icon, icon},
    state::ThreadStatus,
    theme::palette,
};

/// Keep the same native editor mounted as a draft becomes its first conversation.
pub(crate) fn conversation(sheet: Memo<bool>) -> impl IntoElement {
    let colors = palette();
    let home = computed(move || {
        !sheet.get() && crate::controller::state().with(|s| !s.has_conversation())
    });
    let project = computed(|| crate::controller::state().with(|s| s.current_project_key()));
    let dragging = signal(false);
    composer::drop_target(view(), dragging)
        .flex_1()
        .min_w_0()
        .min_h_0()
        .flex_col()
        .child(when(move || home.get(), move || view().flex_1().min_h_0()))
        .child(when(
            move || home.get(),
            move || {
                view()
                    .items_center()
                    .flex_col()
                    .gap_1p5()
                    .px_6()
                    .pb(px(18.0))
                    .child(
                        text(move || {
                            project.with(|key| {
                                if key.is_empty() {
                                    "Vantry".to_owned()
                                } else {
                                    crate::state::sidebar::project_name(key).to_owned()
                                }
                            })
                        })
                        .text_size(px(26.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.ink),
                    )
                    .child(when(
                        move || !project.with(String::is_empty),
                        || {
                            link_button("不关联项目", || {
                                crate::controller::state().update(|s| s.new_thread.workspace = None)
                            })
                        },
                    ))
            },
        ))
        .child(when(move || sheet.get(), dock::dock))
        .child(when(
            move || !home.get() && !sheet.get(),
            timeline::timeline,
        ))
        .child(composer::composer(dragging))
        .child(when(
            move || home.get(),
            || view().flex_1().min_h_0().mb_8(),
        ))
}

/// A tooltip's bubble: `nsh` shows a tooltip element as it is, so a bare
/// `text` would float as unstyled (black) text over the window.
pub(crate) fn tip(label: impl IntoReactive<SharedString>) -> View {
    let bubble = AdaptiveColor::rgb(0x2B2A27, 0x3A3935);
    let edge = AdaptiveColor::rgb(0x2B2A27, 0x4A4843);
    let ink = AdaptiveColor::rgb(0xF5F4EF, 0xECEAE4);
    view()
        .px_2()
        .py_1()
        .rounded(px(6.0))
        .bg(bubble)
        .border_1()
        .border_color(edge)
        .shadow_md()
        .child(
            text(label)
                .number_of_lines(2u32)
                .max_w(px(320.0))
                .text_size(px(11.5))
                .font_weight(FontWeight::MEDIUM)
                .text_color(ink),
        )
}

/// A crisp 28×28 icon-only toolbar button with subtle hover fill and tooltip,
/// matching Stardust `v0.1.42`.
pub(crate) fn icon_button(
    glyph: Icon,
    label: &'static str,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let colors = palette();
    nsh_base::button()
        .accessibility_label(crate::tr(label))
        .on_press(on_press)
        .w(px(28.0))
        .h(px(28.0))
        .items_center()
        .justify_center()
        .rounded(px(7.0))
        .hover(|s| s.bg(colors.soft))
        .active(|s| s.opacity(0.65))
        .disabled_style(|s| s.opacity(0.35))
        .child(icon(glyph, 15.0, colors.muted))
}

/// A compact 24×24 subtle icon button (message action bars, inline controls).
pub(crate) fn small_icon_button(
    glyph: Icon,
    label: &'static str,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let colors = palette();
    nsh_base::button()
        .accessibility_label(label)
        .on_press(on_press)
        .w(px(24.0))
        .h(px(24.0))
        .items_center()
        .justify_center()
        .rounded(px(6.0))
        .hover(|s| s.bg(colors.soft))
        .active(|s| s.opacity(0.6))
        .disabled_style(|s| s.opacity(0.35))
        .child(icon(glyph, 13.5, colors.muted))
}

/// An icon-only segmented pill (header dock switches, compact toggles).
pub(crate) fn icon_pill(
    glyph: Icon,
    label: &'static str,
    on: impl Fn() -> bool + Clone + 'static,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let colors = palette();
    let (a, b, c) = (on.clone(), on.clone(), on);
    nsh_base::button()
        .accessibility_label(crate::tr(label))
        .selected(a)
        .on_press(on_press)
        .w(px(28.0))
        .h(px(24.0))
        .items_center()
        .justify_center()
        .rounded(px(6.0))
        .bg(move || {
            if b() {
                colors.card
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .hover(|s| s.bg(colors.card))
        .active(|s| s.opacity(0.7))
        .child(icon(glyph, 14.0, move || {
            if c() { colors.accent } else { colors.muted }
        }))
}

/// The dot color of a thread's status.
pub(crate) fn status_color(status: ThreadStatus) -> AdaptiveColor {
    let colors = palette();
    match status {
        ThreadStatus::Idle => colors.faint,
        ThreadStatus::Running => colors.accent,
        ThreadStatus::NeedsApproval => colors.warning,
        ThreadStatus::Failed => colors.danger,
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
    let colors = palette();
    match status {
        RunStatus::Completed => colors.success,
        RunStatus::Failed | RunStatus::Interrupted => colors.danger,
        RunStatus::Running | RunStatus::Waiting | RunStatus::Queued => colors.accent,
        RunStatus::Cancelled | RunStatus::Unspecified => colors.faint,
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
    let colors = palette();
    nsh_base::button()
        .on_press(on_press)
        .flex_row()
        .items_center()
        .px_3()
        .py_1()
        .rounded(px(8.0))
        .border_1()
        .border_color(colors.line)
        .hover(|s| s.bg(colors.soft))
        .active(|s| s.opacity(0.7))
        .disabled_style(|s| s.opacity(0.45))
        .child(text(label).text_size(px(12.5)).text_color(colors.ink))
}

/// The ember primary button.
pub(crate) fn primary_button(
    label: impl IntoReactive<SharedString>,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let colors = palette();
    nsh_base::button()
        .on_press(on_press)
        .flex_row()
        .items_center()
        .px_4()
        .py_1()
        .rounded(px(8.0))
        .bg(colors.accent)
        .active(|s| s.opacity(0.75))
        .disabled_style(|s| s.opacity(0.4))
        .child(
            text(label)
                .text_size(px(12.5))
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(colors.on_accent),
        )
}

/// A borderless small text button (row actions, toolbar items).
pub(crate) fn link_button(
    label: impl IntoReactive<SharedString>,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let colors = palette();
    nsh_base::button()
        .on_press(on_press)
        .px_1p5()
        .py_0p5()
        .rounded(px(6.0))
        .hover(|s| s.bg(colors.soft))
        .active(|s| s.opacity(0.6))
        .disabled_style(|s| s.opacity(0.35))
        .child(text(label).text_size(px(11.5)).text_color(colors.muted))
}

/// A pill that shows whether it is on (tabs, segmented switches, filters).
pub(crate) fn pill(
    label: impl IntoReactive<SharedString>,
    on: impl Fn() -> bool + Clone + 'static,
    on_press: impl Fn() + 'static,
) -> nsh_base::ButtonElement {
    let colors = palette();
    let (a, b, c, d) = (on.clone(), on.clone(), on.clone(), on);
    nsh_base::button()
        .selected(a)
        .on_press(on_press)
        .px_2p5()
        .py_0p5()
        .rounded_full()
        .bg(move || {
            if b() {
                colors.card
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .border_1()
        .border_color(move || {
            if c() {
                colors.line
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .active(|s| s.opacity(0.7))
        .child(
            text(label).text_size(px(11.5)).text_color(
                move || {
                    if d() { colors.ink } else { colors.muted }
                },
            ),
        )
}

pub(crate) fn segments() -> nsh_base::ToggleGroupElement {
    nsh_base::toggle_group()
        .flex_row()
        .items_center()
        .p_0p5()
        .rounded(px(8.0))
        .bg(palette().soft)
}

pub(crate) fn segment(
    label: &'static str,
    selected: impl Fn() -> bool + Copy + 'static,
    select: impl Fn() + 'static,
) -> nsh_base::ToggleElement {
    let colors = palette();
    nsh_base::toggle()
        .accessibility_label(label)
        .pressed(selected)
        .on_change(move |_: &bool| select())
        .h(px(26.0))
        .px_3()
        .items_center()
        .justify_center()
        .rounded(px(6.0))
        .bg(move || {
            if selected() {
                colors.card
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .child(
            text(label)
                .text_size(px(12.0))
                .font_weight(FontWeight::MEDIUM)
                .text_color(move || if selected() { colors.ink } else { colors.muted }),
        )
}

/// A small rounded badge.
pub(crate) fn badge(
    label: impl IntoReactive<SharedString>,
    color: impl IntoReactive<AdaptiveColor>,
) -> View {
    let colors = palette();
    view()
        .px_1p5()
        .rounded(px(5.0))
        .bg(colors.soft)
        .child(text(label).text_size(px(10.0)).text_color(color))
}

/// A section's small caption.
pub(crate) fn caption(label: impl IntoReactive<SharedString>) -> Text {
    let colors = palette();
    text(label)
        .text_size(px(11.0))
        .font_weight(FontWeight::MEDIUM)
        .text_color(colors.faint)
}

/// An on/off switch row: label, optional hint, its state.
pub(crate) fn switch_row(
    label: impl Into<SharedString>,
    hint: impl Into<SharedString>,
    on: impl Fn() -> bool + Clone + 'static,
    toggle: impl Fn() + 'static,
) -> View {
    let colors = palette();
    let label = label.into();
    let switch_label = label.clone();
    let hint = hint.into();
    let (a, b, c) = (on.clone(), on.clone(), on);
    let mut left = view().flex_1().flex_col().gap_0p5().child(
        text(label)
            .text_size(px(13.0))
            .font_weight(FontWeight::MEDIUM)
            .text_color(colors.ink),
    );
    if !hint.is_empty() {
        left = left.child(
            text(hint)
                .number_of_lines(1u32)
                .text_size(px(11.5))
                .text_color(colors.faint),
        );
    }
    view()
        .flex_row()
        .items_center()
        .gap_3()
        .py_1()
        .child(left)
        .child(
            nsh_base::button()
                .accessibility_label(switch_label)
                .selected(a)
                .on_press(toggle)
                .w(px(36.0))
                .h(px(20.0))
                .p_0p5()
                .rounded_full()
                .flex_row()
                .justify_start()
                .bg(move || if b() { colors.accent } else { colors.line })
                .active(|s| s.opacity(0.75))
                .child(when(c, || view().w(px(16.0))))
                .child(
                    view()
                        .w(px(16.0))
                        .h(px(16.0))
                        .rounded_full()
                        .bg(colors.on_accent),
                ),
        )
}
