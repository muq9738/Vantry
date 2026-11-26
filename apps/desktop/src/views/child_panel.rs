//! A delegated child thread beside its parent: the dock's drawer (a header
//! with its status, its turns as structured rows — the delegated
//! prompt, thinking, tool calls, the Markdown reply, errors — and a compact
//! composer) and the child rows in the composer status popover.
//!
//! The turns are the child's rows of the shared [`crate::state::TimelineIndex`],
//! so streamed Markdown re-renders incrementally as in the main timeline.

use nsh_core::{LineHeight, prelude::*};
use vantry_protocol::RunStatus;

use super::{dot, icon_button, markdown, run_status_color, timeline};
use crate::{
    controller,
    icons::{Icon, icon},
    state::{AppState, TimelineItem, sidebar::task_badge, workbench::DockTab},
    theme::palette,
};

// ------------------------------------------------------------------ helpers

/// A leading `[Role: xxx]` tag of a delegated prompt and the rest.
pub(crate) fn split_role(input: &str) -> (Option<String>, &str) {
    let trimmed = input.trim_start();
    let Some(rest) = trimmed.strip_prefix('[') else {
        return (None, input);
    };
    let Some(end) = rest.find(']') else {
        return (None, input);
    };
    let tag = &rest[..end];
    let Some((key, role)) = tag.split_once(':') else {
        return (None, input);
    };
    let role = role.trim();
    if !key.trim().eq_ignore_ascii_case("role") || role.is_empty() || role.contains('\n') {
        return (None, input);
    }
    (Some(role.to_owned()), rest[end + 1..].trim_start())
}

/// A run's model id as a short label (no provider path or routing group).
pub(crate) fn short_model(model: &str) -> String {
    if model.trim().is_empty() {
        return String::new();
    }
    super::composer::clean_model_label(model.trim(), "").0
}

/// The child's status: its active run, else the task's, else its last run's.
pub(crate) fn child_status(s: &AppState, child: &str) -> RunStatus {
    if let Some(run) = s.active_run(child) {
        return run.status;
    }
    s.tasks
        .iter()
        .filter(|t| t.child_thread_id == child && t.status != RunStatus::Unspecified)
        .max_by(|a, b| a.updated_at.cmp(&b.updated_at))
        .map(|t| t.status)
        .or_else(|| {
            s.runs
                .iter()
                .filter(|r| r.thread_id == child)
                .max_by(|a, b| a.created_at.cmp(&b.created_at))
                .map(|r| r.status)
        })
        .unwrap_or(RunStatus::Unspecified)
}

/// The model of the child's latest run that names one.
pub(crate) fn child_model(s: &AppState, child: &str) -> String {
    s.runs
        .iter()
        .filter(|r| r.thread_id == child && !r.model.is_empty())
        .max_by(|a, b| a.created_at.cmp(&b.created_at))
        .map(|r| short_model(&r.model))
        .unwrap_or_default()
}

fn child_title(s: &AppState, child: &str) -> String {
    s.tasks
        .iter()
        .find(|t| t.child_thread_id == child && !t.title.trim().is_empty())
        .map(|t| t.title.clone())
        .or_else(|| s.thread(child).map(crate::state::sidebar::display_title))
        .unwrap_or_else(|| "子任务".to_owned())
}

/// Whether a status still works (sorts first, pulses).
pub(crate) fn active_status(status: RunStatus) -> bool {
    matches!(status, RunStatus::Running | RunStatus::Queued | RunStatus::Waiting)
}

/// A status's short label ("" when unknown).
fn status_text(status: RunStatus) -> &'static str {
    task_badge(status)
}

/// The card's counts: "1 运行中 · 2 完成".
pub(crate) fn subagent_summary(statuses: &[RunStatus]) -> String {
    let running = statuses.iter().filter(|s| active_status(**s)).count();
    let done = statuses.iter().filter(|s| **s == RunStatus::Completed).count();
    let failed = statuses
        .iter()
        .filter(|s| matches!(s, RunStatus::Failed | RunStatus::Interrupted))
        .count();
    let mut parts = Vec::new();
    if running > 0 {
        parts.push(format!("{running} 运行中"));
    }
    if done > 0 {
        parts.push(format!("{done} 完成"));
    }
    if failed > 0 {
        parts.push(format!("{failed} 失败"));
    }
    if parts.is_empty() {
        parts.push(format!("{} 个", statuses.len()));
    }
    parts.join(" · ")
}

/// One row of the composer's sub-agent card.
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct SubagentRow {
    pub id: String,
    pub title: String,
    pub status: RunStatus,
    pub model: String,
}

/// The selected thread's children, working ones first.
pub(crate) fn subagent_rows(s: &AppState, parent: &str) -> Vec<SubagentRow> {
    let mut rows: Vec<SubagentRow> = s
        .subtasks(parent)
        .into_iter()
        .map(|(id, title, status)| {
            let live = child_status(s, &id);
            SubagentRow {
                model: child_model(s, &id),
                status: if live == RunStatus::Unspecified { status } else { live },
                title,
                id,
            }
        })
        .collect();
    rows.sort_by_key(|r| !active_status(r.status));
    rows
}

// ------------------------------------------------------------------ drawer

pub(crate) fn child_panel(id: String) -> impl IntoElement {
    let colors = palette();
    let create = id.is_empty();
    let target = stored(id);
    view()
        .flex_1()
        .min_h_0()
        .min_w_0()
        .flex_col()
        .bg(colors.sidebar)
        .child(header(target, create))
        .child(if create {
            intro().into_element()
        } else {
            conversation(target).into_element()
        })
        .child(mini_composer(target, create))
}

fn header(target: Stored<String>, create: bool) -> impl IntoElement {
    let colors = palette();
    let status = computed(move || {
        if create {
            RunStatus::Unspecified
        } else {
            controller::state().with(|s| child_status(s, &target.get()))
        }
    });
    let title = computed(move || {
        if create {
            "派出子代理".to_owned()
        } else {
            controller::state().with(|s| child_title(s, &target.get()))
        }
    });
    let running = computed(move || {
        !create && controller::state().with(|s| s.active_run(&target.get()).is_some())
    });
    let meta = view()
        .flex_row()
        .items_center()
        .gap_1p5()
        .min_w_0()
        .child(when(
            move || !status_text(status.get()).is_empty(),
            move || {
                view()
                    .flex_row()
                    .items_center()
                    .gap_1()
                    .px_1p5()
                    .py(px(1.0))
                    .rounded(px(5.0))
                    .bg(colors.soft)
                    .child(dynamic(move || {
                        let d = dot(run_status_color(status.get()));
                        if active_status(status.get()) {
                            d.animate_pulse().into_element()
                        } else {
                            d.into_element()
                        }
                    }))
                    .child(
                        text(move || status_text(status.get()))
                            .text_size(px(10.5))
                            .font_weight(FontWeight::MEDIUM)
                            .text_color(move || run_status_color(status.get())),
                    )
            },
        ))
        .child(when(
            move || create,
            || {
                text("委派一个独立任务，结果会回到当前会话")
                    .number_of_lines(1u32)
                    .text_size(px(11.0))
                    .text_color(colors.faint)
            },
        ));
    view()
        .flex_shrink_0()
        .flex_row()
        .items_center()
        .gap_2()
        .px_4()
        .py_1p5()
        .border_b_1()
        .border_b_color(colors.line_soft)
        .child(
            view()
                .w(px(26.0))
                .h(px(26.0))
                .flex_shrink_0()
                .items_center()
                .justify_center()
                .rounded(px(7.0))
                .bg(colors.info_bg)
                .child(icon(Icon::Bot, 14.0, colors.info_ink)),
        )
        .child(
            view()
                .flex_1()
                .min_w_0()
                .flex_row()
                .items_center()
                .gap_2()
                .child(
                    text(move || title.get())
                        .flex_shrink(1.0)
                        .min_w_0()
                        .number_of_lines(1u32)
                        .text_size(px(13.5))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.ink),
                )
                .child(meta),
        )
        .child(when(
            move || running.get(),
            move || {
                nsh_base::button()
                    .accessibility_label("停止子任务")
                    .on_press(move || controller::stop_child(target.get()))
                    .flex_row()
                    .items_center()
                    .gap_1()
                    .h(px(24.0))
                    .px_2()
                    .rounded(px(7.0))
                    .border_1()
                    .border_color(colors.line)
                    .hover(|s| s.bg(colors.soft))
                    .active(|s| s.opacity(0.65))
                    .child(icon(Icon::Square, 10.0, colors.ink))
                    .child(text("停止").text_size(px(11.5)).text_color(colors.ink))
            },
        ))
        .child(icon_button(Icon::ArrowLeft, "返回会话概览", || {
            controller::select_dock_tab(DockTab::Summary)
        }))
        .child(icon_button(Icon::X, "关闭", controller::close_dock))
}

/// The new-child mode's guidance.
fn intro() -> impl IntoElement {
    let colors = palette();
    let tip_row = |glyph: Icon, line: &'static str| {
        view()
            .flex_row()
            .items_start()
            .gap_2()
            .child(view().pt(px(2.0)).child(icon(glyph, 13.0, colors.faint)))
            .child(
                text(line)
                    .flex_1()
                    .min_w_0()
                    .text_size(px(12.5))
                    .line_height(LineHeight::Multiple(1.55))
                    .text_color(colors.muted),
            )
    };
    scroll().flex_1().min_h_0().child(
        view().px_4().py_5().child(
            view()
                .flex_col()
                .gap_3()
                .px_4()
                .py_4()
                .rounded(px(12.0))
                .bg(colors.raised)
                .border_1()
                .border_color(colors.line)
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(icon(Icon::Sparkles, 14.0, colors.accent))
                        .child(
                            text("派出一个子代理")
                                .text_size(px(13.5))
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_color(colors.ink),
                        ),
                )
                .child(tip_row(Icon::Layers, "子代理在独立的子会话中运行，不打断当前对话。"))
                .child(tip_row(Icon::ListChecks, "写清目标、范围与完成标准；可用 [Role: xxx] 指定角色。"))
                .child(tip_row(Icon::Inbox, "完成后结果会自动回到当前会话。")),
        ),
    )
}

/// The child's turns, oldest first, following new turns to the end.
fn conversation(target: Stored<String>) -> impl IntoElement {
    let colors = palette();
    let runs = computed(move || {
        controller::state().with(|s| s.timeline.run_order(&target.get()).to_vec())
    });
    let loading = computed(move || {
        controller::state().with(|s| s.needs_child_detail(&target.get()))
            && runs.with(Vec::is_empty)
    });
    let list = element_ref();
    let count = computed(move || runs.with(Vec::len));
    effect(move || {
        count.get();
        spawn(async move {
            timer(std::time::Duration::from_millis(32)).await;
            list.scroll_to_end(false);
        });
    });
    scroll().flex_1().min_h_0().element_ref(&list).child(
        view()
            .w_full()
            .min_w_0()
            .flex_col()
            .gap_3()
            .px_3()
            .py_2()
            .child(when(
                move || runs.with(Vec::is_empty),
                move || {
                    view()
                        .flex_row()
                        .items_center()
                        .justify_center()
                        .gap_2()
                        .py_8()
                        .child(dynamic(move || {
                            if loading.get() {
                                dot(colors.accent).animate_pulse().into_element()
                            } else {
                                icon(Icon::Inbox, 14.0, colors.faint).into_element()
                            }
                        }))
                        .child(
                            text(move || {
                                if loading.get() { "正在加载子任务记录…" } else { "子任务还没有记录" }
                            })
                            .text_size(px(12.5))
                            .text_color(colors.faint),
                        )
                },
            ))
            .child(each(
                move || runs.get(),
                |id: &String| id.clone(),
                move |run, index| turn_view(target, run.get_untracked(), index),
            )),
    )
}

/// One turn: its meta line over its rows.
fn turn_view(target: Stored<String>, run_id: String, index: ReadSignal<usize>) -> impl IntoElement {
    let colors = palette();
    let id = stored(run_id);
    let meta = computed(move || {
        controller::state().with(|s| {
            s.run(&id.get())
                .map(|r| (r.status, short_model(&r.model), r.error.clone()))
                .unwrap_or((RunStatus::Unspecified, String::new(), String::new()))
        })
    });
    let items = computed(move || {
        controller::state().with(|s| s.timeline.run_items(&id.get()).to_vec())
    });
    // An error the status row does not already say (a completed run's note).
    let loose_error = computed(move || {
        let (status, _, error) = meta.get();
        let said = items.with(|i| i.iter().any(|i| matches!(i, TimelineItem::Status { .. })));
        if said || error.trim().is_empty() || matches!(status, RunStatus::Failed | RunStatus::Interrupted) {
            String::new()
        } else {
            error
        }
    });
    view()
        .w_full()
        .min_w_0()
        .flex_col()
        .gap_1p5()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .child(
                    text(move || format!("第 {} 轮", index.get() + 1))
                        .flex_shrink_0()
                        .text_size(px(10.5))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.faint),
                )
                .child(view().flex_1().h(px(1.0)).bg(colors.line_soft))
                .child(
                    text(move || {
                        let (status, model, _) = meta.get();
                        [status_text(status).to_owned(), model]
                            .into_iter()
                            .filter(|p| !p.is_empty())
                            .collect::<Vec<_>>()
                            .join(" · ")
                    })
                    .flex_shrink(1.0)
                    .min_w_0()
                    .number_of_lines(1u32)
                    .text_size(px(10.5))
                    .text_color(colors.faint),
                ),
        )
        .child(each(
            move || items.get(),
            |item: &TimelineItem| item.id().to_owned(),
            move |item, _| turn_row(item, target, id, index),
        ))
        .child(when(
            move || !loose_error.with(String::is_empty),
            move || error_card(move || loose_error.get()),
        ))
}

fn turn_row(
    item: ReadSignal<TimelineItem>,
    target: Stored<String>,
    run: Stored<String>,
    index: ReadSignal<usize>,
) -> impl IntoElement {
    let memo = computed(move || Some(item.get()));
    let kind = computed(move || {
        item.with(|i| match i {
            TimelineItem::User { .. } => 0u8,
            TimelineItem::Reasoning { .. } => 1,
            TimelineItem::Text { .. } => 2,
            TimelineItem::Tools { .. } => 3,
            TimelineItem::Approval { .. } => 4,
            TimelineItem::Status { .. } => 5,
        })
    });
    dynamic(move || match kind.get() {
        0 => prompt_card(memo, target, run, index).into_element(),
        1 => timeline::reasoning_row(memo, 0).into_element(),
        2 => reply(memo).into_element(),
        3 => timeline::tools_row(memo).into_element(),
        4 => timeline::approval_row(memo).into_element(),
        _ => status_line(memo).into_element(),
    })
}

/// A folded instruction or follow-up in the child's timeline.
fn prompt_card(
    item: timeline::ItemMemo,
    target: Stored<String>,
    run: Stored<String>,
    index: ReadSignal<usize>,
) -> impl IntoElement {
    let colors = palette();
    let source = computed(move || match item.get() {
        Some(TimelineItem::User { text, .. }) => text.to_string(),
        _ => String::new(),
    });
    let role = computed(move || source.with(|s| split_role(s).0.unwrap_or_default()));
    let model = computed(move || {
        controller::state().with(|s| {
            s.run(&run.get())
                .map(|r| short_model(&r.model))
                .unwrap_or_default()
        })
    });
    let open = signal(false);
    let tag = move |label: Memo<String>| {
        when(
            move || !label.with(String::is_empty),
            move || {
                text(move || label.get())
                    .number_of_lines(1u32)
                    .max_w(px(96.0))
                    .px_1p5()
                    .rounded(px(4.0))
                    .bg(colors.soft)
                    .text_size(px(10.5))
                    .text_color(colors.muted)
            },
        )
    };
    view()
        .min_w_0()
        .flex_col()
        .child(
            nsh_base::button()
                .accessibility_label("展开或收起指令全文")
                .on_press(move || open.set(!open.get_untracked()))
                .w_full()
                .min_w_0()
                .flex_row()
                .items_center()
                .gap_1p5()
                .px_2()
                .h(px(30.0))
                .rounded(px(6.0))
                .bg(colors.raised)
                .hover(|s| s.bg(colors.soft))
                .active(|s| s.opacity(0.75))
                .child(dynamic(move || {
                    icon(
                        if open.get() {
                            Icon::ChevronDown
                        } else {
                            Icon::ChevronRight
                        },
                        12.0,
                        colors.muted,
                    )
                }))
                .child(
                    text(move || {
                        if index.get() == 0 {
                            controller::state().with(|s| child_title(s, &target.get()))
                        } else {
                            "补充信息".to_owned()
                        }
                    })
                    .flex_1()
                    .min_w_0()
                    .number_of_lines(1u32)
                    .text_size(px(12.5))
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_color(colors.ink),
                )
                .child(tag(role))
                .child(tag(model)),
        )
        .child(when(
            move || open.get(),
            move || {
                view()
                    .flex_col()
                    .gap_1()
                    .px_2()
                    .py_2()
                    .child(dynamic(move || {
                        source.with(|s| {
                            markdown::static_document(&crate::state::markdown::MarkdownDoc::parse(
                                split_role(s).1,
                            ))
                            .into_element()
                        })
                    }))
                    .child(view().flex_row().justify_end().child(super::small_icon_button(
                        Icon::Copy,
                        "复制全文",
                        move || controller::copy(source.get_untracked()),
                    )))
            },
        ))
}

/// The assistant's reply, rendered as Markdown.
fn reply(item: timeline::ItemMemo) -> impl IntoElement {
    let colors = palette();
    let doc = computed(move || match item.get() {
        Some(TimelineItem::Text { doc, .. }) => Some(doc),
        _ => None,
    });
    let streaming = move || matches!(item.get(), Some(TimelineItem::Text { streaming: true, .. }));
    view()
        .w_full()
        .min_w_0()
        .flex_col()
        .gap_1p5()
        .child(markdown::document(doc, false))
        .child(when(streaming, move || {
            view()
                .w(px(7.0))
                .h(px(7.0))
                .rounded_full()
                .bg(colors.accent)
                .animate_pulse()
        }))
}

fn status_line(item: timeline::ItemMemo) -> impl IntoElement {
    let colors = palette();
    let status = computed(move || match item.get() {
        Some(TimelineItem::Status { text, error, .. }) => (text, error),
        _ => (String::new(), false),
    });
    dynamic(move || {
        let (line, error) = status.get();
        if error {
            error_card(move || status.get().0).into_element()
        } else {
            view()
                .flex_row()
                .items_center()
                .gap_1p5()
                .child(icon(Icon::Square, 10.0, colors.faint))
                .child(text(line).text_size(px(12.0)).text_color(colors.faint))
                .into_element()
        }
    })
}

/// A soft error note: a quiet card with a danger mark.
fn error_card(message: impl Fn() -> String + 'static) -> impl IntoElement {
    let colors = palette();
    view()
        .w_full()
        .min_w_0()
        .flex_row()
        .items_start()
        .gap_2()
        .px_3()
        .py_2()
        .rounded(px(10.0))
        .bg(colors.soft)
        .border_1()
        .border_color(colors.line)
        .child(view().pt(px(2.0)).child(icon(Icon::CircleAlert, 13.0, colors.danger)))
        .child(
            text(message)
                .flex_1()
                .min_w_0()
                .select_text()
                .text_size(px(12.5))
                .line_height(LineHeight::Multiple(1.5))
                .text_color(colors.ink),
        )
}

/// The drawer's compact composer: the main composer's card, smaller.
fn mini_composer(target: Stored<String>, create: bool) -> impl IntoElement {
    let colors = palette();
    let draft = signal(String::new());
    let value = computed(move || {
        if create {
            draft.get()
        } else {
            controller::state().with(|s| {
                s.drafts
                    .get(&target.get())
                    .map(|d| d.text.clone())
                    .unwrap_or_default()
            })
        }
    });
    let running = computed(move || {
        !create && controller::state().with(|s| s.active_run(&target.get()).is_some())
    });
    let can_send = computed(move || !value.with(|v| v.trim().is_empty()));
    let show_stop = move || running.get() && !can_send.get();
    let send = move || {
        if create {
            let message = draft.get_untracked();
            if !message.trim().is_empty() {
                controller::delegate_child(message);
                draft.set(String::new());
            }
        } else {
            controller::submit_child(&target.get());
        }
    };
    view()
        .flex_shrink_0()
        .px_3()
        .pt_2()
        .pb_3()
        .child(
            view()
                .flex_col()
                .gap_1p5()
                .px_3()
                .pt_2p5()
                .pb_2()
                .rounded(px(12.0))
                .bg(colors.raised)
                .border_1()
                .border_color(colors.line)
                .shadow_sm()
                .child(
                    nsh_base::input()
                        .label(if create { "子代理任务" } else { "子任务消息" })
                        .multiline(true)
                        .scrollbar(false)
                        .submit_behavior(SubmitBehavior::Submit)
                        .value(move || value.get())
                        .on_change(move |s: &str| {
                            if create {
                                draft.set(s.to_owned());
                            } else {
                                controller::state().update(|a| {
                                    a.drafts.entry(target.get()).or_default().text = s.into()
                                });
                            }
                        })
                        .on_submit(move |_: &str| send())
                        .placeholder(move || {
                            if create {
                                "描述要委派的任务…"
                            } else if running.get() {
                                "补充信息，插入到当前子任务…"
                            } else {
                                "继续给子任务发消息…"
                            }
                        })
                        .placeholder_color(move || color(colors.faint))
                        .caret_color(move || color(colors.accent))
                        .min_h(px(22.0))
                        .max_h(px(120.0))
                        .text_size(px(13.0))
                        .line_height(LineHeight::Multiple(1.5))
                        .text_color(colors.ink),
                )
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(
                            text(move || {
                                if create {
                                    "Enter 派出"
                                } else if running.get() {
                                    "运行中 · Enter 插入"
                                } else {
                                    "Enter 发送"
                                }
                            })
                            .flex_1()
                            .min_w_0()
                            .number_of_lines(1u32)
                            .text_size(px(10.5))
                            .text_color(colors.faint),
                        )
                        .child(
                            nsh_base::button()
                                .accessibility_label(move || {
                                    Some(SharedString::from(if show_stop() {
                                        "停止子任务"
                                    } else if create {
                                        "派出子代理"
                                    } else {
                                        "发送到子任务"
                                    }))
                                })
                                .on_press(move || {
                                    if show_stop() {
                                        controller::stop_child(target.get());
                                    } else {
                                        send();
                                    }
                                })
                                .disabled(move || !show_stop() && !can_send.get())
                                .flex_shrink_0()
                                .w(px(26.0))
                                .h(px(26.0))
                                .items_center()
                                .justify_center()
                                .rounded_full()
                                .bg(move || {
                                    if show_stop() {
                                        colors.ink
                                    } else if can_send.get() {
                                        colors.accent
                                    } else {
                                        colors.soft
                                    }
                                })
                                .active(|s| s.opacity(0.75))
                                .child(dynamic(move || {
                                    if show_stop() {
                                        icon(Icon::Square, 10.0, colors.canvas).into_element()
                                    } else {
                                        icon(
                                            Icon::Send,
                                            14.0,
                                            if can_send.get() { colors.on_accent } else { colors.faint },
                                        )
                                        .into_element()
                                    }
                                })),
                        ),
                ),
        )
}

/// Child rows in the composer status popover.
pub(crate) fn subagent_list(rows: Memo<Vec<SubagentRow>>, open: Signal<bool>) -> impl IntoElement {
    let colors = palette();
    let open_child = computed(|| controller::ui().with(|u| u.child_panel.as_ref().map(|c| c.1.clone())));
    view().flex_col().child(each(
        move || rows.get(),
        |r: &SubagentRow| r.id.clone(),
        move |row, _| {
            let current = move || row.with(|r| open_child.with(|c| c.as_deref() == Some(r.id.as_str())));
            nsh_base::button()
                .accessibility_label(move || {
                    Some(SharedString::from(
                        row.with(|r| format!("打开子代理 {}", r.title)),
                    ))
                })
                .on_press(move || {
                    open.set(false);
                    controller::open_child(row.with_untracked(|r| r.id.clone()));
                })
                .w_full()
                .flex_row()
                .items_center()
                .gap_2()
                .px_1p5()
                .py(px(5.0))
                .rounded(px(7.0))
                .bg(move || {
                    if current() {
                        colors.soft
                    } else {
                        AdaptiveColor::TRANSPARENT
                    }
                })
                .hover(|s| s.bg(colors.soft))
                .active(|s| s.opacity(0.7))
                .child(dynamic(move || {
                    let status = row.with(|r| r.status);
                    let d = dot(run_status_color(status));
                    if active_status(status) {
                        d.animate_pulse().into_element()
                    } else {
                        d.into_element()
                    }
                }))
                .child(
                    text(move || row.with(|r| r.title.clone()))
                        .flex_1()
                        .min_w_0()
                        .number_of_lines(1u32)
                        .text_size(px(12.0))
                        .text_color(move || if current() { colors.ink } else { colors.muted }),
                )
                .child(
                    text(move || {
                        row.with(|r| {
                            if r.model.is_empty() || !active_status(r.status) {
                                status_text(r.status).to_owned()
                            } else {
                                r.model.clone()
                            }
                        })
                    })
                    .flex_shrink(1.0)
                    .max_w(px(96.0))
                    .number_of_lines(1u32)
                    .text_size(px(10.5))
                    .text_color(move || row.with(|r| run_status_color(r.status))),
                )
        },
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn role_tags_split_from_the_prompt() {
        assert_eq!(
            split_role("[Role: design]\n\n你是设计专家"),
            (Some("design".to_owned()), "你是设计专家")
        );
        assert_eq!(split_role("  [role:qa] 测试"), (Some("qa".to_owned()), "测试"));
        assert_eq!(split_role("[Note] 不是角色"), (None, "[Note] 不是角色"));
        assert_eq!(split_role("普通消息"), (None, "普通消息"));
        assert_eq!(split_role("[Role: ] 空"), (None, "[Role: ] 空"));
    }

    #[test]
    fn instruction_summary_uses_the_task_title() {
        let mut state = AppState::default();
        state.threads.push(vantry_protocol::Thread {
            id: "child".into(),
            title: "会话".into(),
            ..Default::default()
        });
        assert_eq!(child_title(&state, "child"), "会话");
        state.tasks.push(vantry_protocol::Task {
            child_thread_id: "child".into(),
            title: "验证发布包".into(),
            ..Default::default()
        });
        assert_eq!(child_title(&state, "child"), "验证发布包");
        assert_eq!(child_title(&state, "unknown"), "子任务");
    }

    #[test]
    fn models_lose_their_routing_path() {
        assert_eq!(
            short_model("magpie/qmt-ai/group/deepseek-v4.1-flash"),
            "deepseek-v4.1-flash"
        );
        assert_eq!(short_model("claude-opus-5-5"), "claude-opus-5-5");
        assert_eq!(short_model("  "), "");
    }

    #[test]
    fn the_card_counts_working_and_finished_children() {
        assert_eq!(
            subagent_summary(&[RunStatus::Running, RunStatus::Completed, RunStatus::Completed]),
            "1 运行中 · 2 完成"
        );
        assert_eq!(
            subagent_summary(&[RunStatus::Queued, RunStatus::Failed]),
            "1 运行中 · 1 失败"
        );
        assert_eq!(subagent_summary(&[RunStatus::Unspecified]), "1 个");
    }
}
