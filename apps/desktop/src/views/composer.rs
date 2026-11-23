//! The composer: the queue over the card (queued messages to edit, move,
//! promote to a steer or cancel; undelivered steers), attachment chips, the
//! native multiline input (IME safe: the platform commits a composition
//! before Return can submit; Enter sends, Shift+Enter breaks the line), and
//! the toolbar: attach, plan mode, permission policy, model picker, the
//! steer/queue choice while a run goes, and send / stop / retry / save.

use nsh_core::{LineHeight, prelude::*};
use vantry_protocol::Attachment;

use super::{ghost_button, link_button, pill, primary_button};
use crate::{
    bridge::workbench::actions::{split_thinking, thinking_label},
    controller,
    state::{
        APPROVAL_POLICIES, InteractionMode, NoticeLevel, PrimaryAction, QueueItem, QueueItemKind,
        RunningAction, policy_label,
    },
    theme::{PALETTE, metrics},
};

pub(crate) fn composer() -> impl IntoElement {
    let draft = computed(|| controller::state().with(|s| s.draft()));
    let status = computed(|| controller::state().with(|s| s.composer_status()));
    let notice = computed(|| controller::state().with(|s| s.notice.clone()));
    let running_action = computed(|| controller::state().with(|s| s.running_action));
    let queue = computed(|| controller::state().with(|s| s.queue_items()));
    let plan = computed(move || draft.with(|d| d.mode == InteractionMode::Plan));
    let attachments = computed(move || draft.with(|d| d.attachments.clone()));
    let running = computed(move || status.with(|s| s.can_stop));
    let editing = computed(move || status.with(|s| s.editing));

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
                .child(when(
                    move || queue.with(|q| !q.is_empty()),
                    move || queue_list(queue),
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
                            if editing.get() || plan.get() {
                                PALETTE.accent
                            } else {
                                PALETTE.line
                            }
                        })
                        .shadow_md()
                        .child(when(
                            move || editing.get(),
                            || {
                                view()
                                    .flex_row()
                                    .items_center()
                                    .gap_2()
                                    .child(
                                        text("正在编辑排队消息")
                                            .flex_1()
                                            .text_size(px(11.5))
                                            .text_color(PALETTE.accent),
                                    )
                                    .child(link_button("取消编辑", controller::cancel_queue_edit))
                            },
                        ))
                        .child(when(
                            move || attachments.with(|a| !a.is_empty()),
                            move || attachment_chips(attachments),
                        ))
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
                        .child(toolbar(status, plan, running, running_action))
                        .child(when(
                            move || running.get(),
                            move || {
                                text(move || match running_action.get() {
                                    RunningAction::Steer => "运行中 · 发送的内容会作为纠偏追加给当前任务；Esc 停止",
                                    RunningAction::Queue => "运行中 · 发送的内容会排队，当前任务结束后依次发送；Esc 停止",
                                })
                                .text_size(px(11.0))
                                .text_color(PALETTE.faint)
                            },
                        )),
                )
                .child(model_picker()),
        )
}

fn toolbar(
    status: Memo<crate::state::ComposerStatus>,
    plan: Memo<bool>,
    running: Memo<bool>,
    running_action: Memo<RunningAction>,
) -> impl IntoElement {
    let policy = computed(|| controller::state().with(|s| s.current_policy()));
    let model = computed(|| controller::state().with(|s| s.current_model()));
    // The thinking level, when the catalog offers levels.
    let thinking = computed(move || {
        let (levels, models) = controller::ui().with(|u| u.thinking_catalog());
        if levels.is_empty() {
            return None;
        }
        Some(thinking_label(split_thinking(&model.get(), &models).1))
    });
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .child(
            nsh_base::button()
                .accessibility_label("添加附件")
                .on_press(controller::pick_attachments)
                .w(px(28.0))
                .h(px(28.0))
                .items_center()
                .justify_center()
                .rounded_full()
                .border_1()
                .border_color(PALETTE.line)
                .active(|s| s.opacity(0.7))
                .child(text("+").text_size(px(15.0)).text_color(PALETTE.muted)),
        )
        .child(
            nsh_base::button()
                .accessibility_label("规划模式")
                .selected(move || plan.get())
                .on_press(controller::toggle_mode)
                .px_3()
                .py_1()
                .rounded_full()
                .border_1()
                .border_color(move || if plan.get() { PALETTE.accent } else { PALETTE.line })
                .active(|s| s.opacity(0.7))
                .child(
                    text(move || if plan.get() { "规划 · 开" } else { "规划" })
                        .text_size(px(12.0))
                        .text_color(move || if plan.get() { PALETTE.accent } else { PALETTE.muted }),
                ),
        )
        .child(when(
            move || running.get(),
            move || running_action_switch(running_action),
        ))
        .child(view().flex_1())
        .child(policy_switch(policy))
        .child(
            nsh_base::button()
                .accessibility_label("选择模型")
                .on_press(controller::toggle_model_picker)
                .max_w(px(200.0))
                .px_3()
                .py_1()
                .rounded_full()
                .bg(PALETTE.soft)
                .active(|s| s.opacity(0.7))
                .child(
                    text(move || {
                        let model = model.get();
                        if model.is_empty() {
                            "默认模型 ▾".to_owned()
                        } else {
                            format!("{} ▾", model.rsplit('/').next().unwrap_or(&model))
                        }
                    })
                    .number_of_lines(1u32)
                    .text_size(px(12.0))
                    .text_color(PALETTE.ink),
                ),
        )
        .child(when(
            move || thinking.with(Option::is_some),
            move || {
                nsh_base::button()
                    .accessibility_label("思考档位")
                    .on_press(controller::cycle_thinking)
                    .px_3()
                    .py_1()
                    .rounded_full()
                    .border_1()
                    .border_color(PALETTE.line)
                    .active(|s| s.opacity(0.7))
                    .child(
                        text(move || format!("思考 · {}", thinking.get().unwrap_or_default()))
                            .text_size(px(12.0))
                            .text_color(PALETTE.muted),
                    )
            },
        ))
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
                    .child(text("■ 停止 · Esc").text_size(px(12.0)).text_color(PALETTE.ink))
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
        )
}

/// Auto / edits auto / ask, as a cycling pill.
fn policy_switch(policy: Memo<String>) -> impl IntoElement {
    nsh_base::button()
        .accessibility_label("权限")
        .on_press(move || {
            let current = policy.get_untracked();
            let at = APPROVAL_POLICIES
                .iter()
                .position(|(value, _)| *value == current)
                .map_or(0, |i| (i + 1) % APPROVAL_POLICIES.len());
            controller::set_policy(APPROVAL_POLICIES[at].0.to_owned());
        })
        .px_3()
        .py_1()
        .rounded_full()
        .bg(PALETTE.approval)
        .active(|s| s.opacity(0.7))
        .child(
            text(move || format!("权限：{}", policy_label(&policy.get())))
                .text_size(px(12.0))
                .text_color(PALETTE.warning),
        )
}

fn running_action_switch(action: Memo<RunningAction>) -> impl IntoElement {
    view()
        .flex_row()
        .items_center()
        .p_0p5()
        .rounded_full()
        .bg(PALETTE.soft)
        .child(pill(
            PrimaryAction::Steer.label(),
            move || action.get() == RunningAction::Steer,
            || controller::set_running_action(RunningAction::Steer),
        ))
        .child(pill(
            PrimaryAction::Queue.label(),
            move || action.get() == RunningAction::Queue,
            || controller::set_running_action(RunningAction::Queue),
        ))
}

fn attachment_chips(attachments: Memo<Vec<Attachment>>) -> impl IntoElement {
    view().flex_row().flex_wrap().gap_1().child(each(
        move || attachments.get(),
        |a: &Attachment| a.id.clone(),
        |attachment, _| {
            let id = move || attachment.with_untracked(|a| a.id.clone());
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .pl_2()
                .pr_1()
                .py_0p5()
                .rounded(px(8.0))
                .bg(PALETTE.soft)
                .child(
                    text(move || {
                        attachment.with(|a| {
                            let icon = if a.mime.starts_with("image/") { "🖼" } else { "📄" };
                            format!("{icon} {}", a.name)
                        })
                    })
                    .number_of_lines(1u32)
                    .max_w(px(220.0))
                    .text_size(px(11.5))
                    .text_color(PALETTE.ink),
                )
                .child(
                    nsh_base::button()
                        .accessibility_label("移除附件")
                        .on_press(move || controller::remove_attachment(id()))
                        .px_1()
                        .active(|s| s.opacity(0.6))
                        .child(text("✕").text_size(px(10.0)).text_color(PALETTE.faint)),
                )
        },
    ))
}

fn queue_list(queue: Memo<Vec<QueueItem>>) -> impl IntoElement {
    view()
        .flex_col()
        .gap_1()
        .p_2()
        .rounded(px(12.0))
        .bg(PALETTE.soft)
        .child(super::caption(move || {
            queue.with(|q| {
                let queued = q.iter().filter(|i| i.kind == QueueItemKind::Queued).count();
                format!("排队 {queued} 条 · 当前任务结束后依次发送")
            })
        }))
        .child(each(
            move || queue.get(),
            |item: &QueueItem| item.id.clone(),
            |item, _| queue_row(item),
        ))
}

fn queue_row(item: ReadSignal<QueueItem>) -> impl IntoElement {
    let id = move || item.with_untracked(|i| i.id.clone());
    let queued = move || item.with(|i| i.kind == QueueItemKind::Queued);
    let busy = move || item.with(|i| i.busy);
    let first = move || item.with(|i| i.index == 0);
    let last = move || item.with(|i| i.index + 1 >= i.count);
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_2()
        .py_1()
        .rounded(px(8.0))
        .bg(move || if item.with(|i| i.editing) { PALETTE.card } else { AdaptiveColor::TRANSPARENT })
        .child(
            text(move || item.with(|i| i.status.clone()))
                .text_size(px(10.5))
                .text_color(move || if queued() { PALETTE.faint } else { PALETTE.accent }),
        )
        .child(
            text(move || {
                item.with(|i| {
                    if i.attachments > 0 {
                        format!("{}（附件 {}）", i.text, i.attachments)
                    } else {
                        i.text.clone()
                    }
                })
            })
            .flex_1()
            .number_of_lines(1u32)
            .text_size(px(12.5))
            .text_color(PALETTE.ink),
        )
        .child(when(queued, move || {
            view()
                .flex_row()
                .gap_0p5()
                .child(link_button("编辑", move || controller::edit_queued(id())).disabled(busy))
                .child(
                    link_button("↑", move || controller::move_queued(id(), -1))
                        .accessibility_label("上移")
                        .disabled(move || busy() || first()),
                )
                .child(
                    link_button("↓", move || controller::move_queued(id(), 1))
                        .accessibility_label("下移")
                        .disabled(move || busy() || last()),
                )
                .child(link_button("立即追加", move || controller::promote_queued(id())).disabled(busy))
                .child(link_button("取消", move || controller::cancel_queued(id())).disabled(busy))
        }))
}

/// The model picker: search, the catalog's models, a free entry.
fn model_picker() -> impl IntoElement {
    let open = computed(|| controller::ui().with(|u| u.model_picker));
    let query = computed(|| controller::ui().with(|u| u.model_query.clone()));
    let choices = computed(|| controller::ui().with(|u| u.model_choices()));
    let loading = computed(|| {
        controller::ui().with(|u| matches!(u.catalogs, crate::state::ui::Catalogs::Loading))
    });
    let current = computed(|| controller::state().with(|s| s.current_model()));
    when(
        move || open.get(),
        move || {
            view()
                .flex_col()
                .gap_1()
                .p_2()
                .rounded(px(12.0))
                .bg(PALETTE.card)
                .border_1()
                .border_color(PALETTE.line)
                .shadow_md()
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(
                            nsh_base::input()
                                .value(move || query.get())
                                .on_change(|t: &str| controller::set_model_query(t))
                                .on_submit(move |_: &str| {
                                    if let Some((id, _)) = choices.get_untracked().first() {
                                        controller::set_model(id.clone());
                                    }
                                })
                                .placeholder("搜索或输入 provider/model")
                                .placeholder_color(move || color(PALETTE.faint))
                                .auto_focus(true)
                                .auto_correct(false)
                                .flex_1()
                                .text_size(px(12.5))
                                .text_color(PALETTE.ink),
                        )
                        .child(link_button("关闭", controller::toggle_model_picker)),
                )
                .child(when(
                    move || loading.get(),
                    || text("正在读取模型目录…").text_size(px(11.5)).text_color(PALETTE.faint),
                ))
                .child(
                    scroll().max_h(px(220.0)).child(view().flex_col().child(each(
                        move || choices.get(),
                        |c: &(String, String)| c.0.clone(),
                        move |choice, _| {
                            let id = move || choice.with_untracked(|c| c.0.clone());
                            let on = move || current.get() == choice.with(|c| c.0.clone());
                            nsh_base::button()
                                .on_press(move || controller::set_model(id()))
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .px_2()
                                .py_1()
                                .rounded(px(6.0))
                                .hover(|s| s.bg(PALETTE.soft))
                                .child(
                                    text(move || choice.with(|c| c.1.clone()))
                                        .flex_1()
                                        .text_size(px(12.5))
                                        .text_color(PALETTE.ink),
                                )
                                .child(
                                    text(move || choice.with(|c| c.0.clone()))
                                        .font_family("Menlo")
                                        .text_size(px(11.0))
                                        .text_color(PALETTE.faint),
                                )
                                .child(when(on, || text("✓").text_color(PALETTE.accent)))
                        },
                    ))),
                )
        },
    )
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
