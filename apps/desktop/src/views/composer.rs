//! The composer: a floating card with the native multiline input (IME
//! safe: the platform commits a composition before Return can submit;
//! Enter sends, Shift+Enter breaks the line), the plan-mode switch, the
//! steer/queue choice while a run goes, and send / stop / retry.

use nsh_core::{LineHeight, prelude::*};

use super::{ghost_button, primary_button};
use crate::{
    controller,
    state::{InteractionMode, NoticeLevel, PrimaryAction, RunningAction},
    theme::{PALETTE, metrics},
};

pub(crate) fn composer() -> impl IntoElement {
    let draft = computed(|| controller::state().with(|s| s.draft()));
    let status = computed(|| controller::state().with(|s| s.composer_status()));
    let notice = computed(|| controller::state().with(|s| s.notice.clone()));
    let running_action = computed(|| controller::state().with(|s| s.running_action));
    let plan = computed(move || draft.with(|d| d.mode == InteractionMode::Plan));
    let running = computed(move || status.with(|s| s.can_stop));

    view()
        .w_full()
        .flex_shrink_0()
        .flex_row()
        .justify_center()
        .px_6()
        .pt_2()
        .pb_4()
        .bg(PALETTE.canvas)
        .child(
            view()
                .flex_1()
                .max_w(px(metrics::READING_WIDTH))
                .flex_col()
                .gap_2()
                .child(when(
                    move || notice.with(Option::is_some),
                    move || notice_line(notice),
                ))
                .child(
                    view()
                        .flex_col()
                        .gap_2()
                        .p_3()
                        .rounded(px(metrics::COMPOSER_RADIUS))
                        .bg(PALETTE.card)
                        .border_1()
                        .border_color(move || {
                            if plan.get() {
                                PALETTE.accent
                            } else {
                                PALETTE.line
                            }
                        })
                        .shadow_md()
                        .child(
                            nsh_base::input()
                                .multiline(true)
                                .submit_behavior(SubmitBehavior::Submit)
                                .value(move || draft.with(|d| d.text.clone()))
                                .on_change(|text: &str| controller::set_draft(text))
                                .on_submit(|_: &str| controller::submit())
                                .placeholder("问点什么… @ 文件、$ 技能、/ 命令")
                                .placeholder_color(move || color(PALETTE.faint))
                                .caret_color(move || color(PALETTE.accent))
                                .auto_focus(true)
                                .auto_capitalize(AutoCapitalize::None)
                                .auto_correct(false)
                                .min_h(px(44.0))
                                .max_h(px(220.0))
                                .px_1()
                                .text_size(px(metrics::BODY + 0.5))
                                .line_height(LineHeight::Multiple(1.5))
                                .text_color(PALETTE.ink),
                        )
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .child(
                                    nsh_base::button()
                                        .accessibility_label("规划模式")
                                        .selected(move || plan.get())
                                        .on_press(controller::toggle_mode)
                                        .px_3()
                                        .py_1()
                                        .rounded_full()
                                        .border_1()
                                        .border_color(move || {
                                            if plan.get() {
                                                PALETTE.accent
                                            } else {
                                                PALETTE.line
                                            }
                                        })
                                        .active(|s| s.opacity(0.7))
                                        .child(
                                            text(move || {
                                                if plan.get() {
                                                    "规划 · 开"
                                                } else {
                                                    "规划"
                                                }
                                            })
                                            .text_size(px(12.0))
                                            .text_color(move || {
                                                if plan.get() {
                                                    PALETTE.accent
                                                } else {
                                                    PALETTE.muted
                                                }
                                            }),
                                        ),
                                )
                                .child(when(
                                    move || running.get(),
                                    move || running_action_switch(running_action),
                                ))
                                .child(view().flex_1())
                                .child(when(
                                    move || status.with(|s| s.retry.is_some()),
                                    move || {
                                        view()
                                            .flex_row()
                                            .gap_2()
                                            .child(ghost_button("放弃", move || {
                                                if let Some(key) = status.get_untracked().retry {
                                                    controller::abandon(key);
                                                }
                                            }))
                                            .child(ghost_button("重试", move || {
                                                if let Some(key) = status.get_untracked().retry {
                                                    controller::retry(key);
                                                }
                                            }))
                                    },
                                ))
                                .child(when(
                                    move || running.get(),
                                    || {
                                        nsh_base::button()
                                            .accessibility_label("停止")
                                            .on_press(controller::stop)
                                            .px_3()
                                            .py_1()
                                            .rounded_full()
                                            .bg(PALETTE.soft)
                                            .border_1()
                                            .border_color(PALETTE.line)
                                            .active(|s| s.opacity(0.7))
                                            .child(
                                                text("■ 停止")
                                                    .text_size(px(12.0))
                                                    .text_color(PALETTE.ink),
                                            )
                                    },
                                ))
                                .child(
                                    primary_button(
                                        move || {
                                            status.with(|s| {
                                                if s.sending {
                                                    "发送中…"
                                                } else {
                                                    s.primary.label()
                                                }
                                            })
                                        },
                                        controller::submit,
                                    )
                                    .disabled(move || status.with(|s| !s.can_submit)),
                                ),
                        ),
                ),
        )
}

fn running_action_switch(action: Memo<RunningAction>) -> impl IntoElement {
    let option = move |label: &'static str, value: RunningAction| {
        nsh_base::button()
            .selected(move || action.get() == value)
            .on_press(move || controller::set_running_action(value))
            .px_2()
            .py_0p5()
            .rounded_full()
            .bg(move || {
                if action.get() == value {
                    PALETTE.card
                } else {
                    AdaptiveColor::TRANSPARENT
                }
            })
            .child(text(label).text_size(px(11.5)).text_color(move || {
                if action.get() == value {
                    PALETTE.ink
                } else {
                    PALETTE.muted
                }
            }))
    };
    view()
        .flex_row()
        .items_center()
        .p_0p5()
        .rounded_full()
        .bg(PALETTE.soft)
        .child(option(PrimaryAction::Steer.label(), RunningAction::Steer))
        .child(option(PrimaryAction::Queue.label(), RunningAction::Queue))
}

fn notice_line(notice: Memo<Option<crate::state::Notice>>) -> impl IntoElement {
    let level_color = move || match notice.with(|n| n.as_ref().map(|n| n.level)) {
        Some(NoticeLevel::Error) => PALETTE.danger,
        Some(NoticeLevel::Warn) => PALETTE.warning,
        _ => PALETTE.muted,
    };
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_3()
        .child(
            text(move || notice.with(|n| n.as_ref().map(|n| n.text.clone()).unwrap_or_default()))
                .flex_1()
                .select_text()
                .text_size(px(12.5))
                .text_color(level_color),
        )
        .child(
            nsh_base::button()
                .accessibility_label("关闭提示")
                .on_press(controller::dismiss_notice)
                .px_1()
                .active(|s| s.opacity(0.6))
                .child(text("✕").text_size(px(11.0)).text_color(PALETTE.faint)),
        )
}
