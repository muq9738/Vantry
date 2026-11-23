//! The approval modal: the first unanswered extension request (the shown
//! thread's first), with what it will do, a risk hint, and its answers.
//! Each answer sends `request.respond` with the request's id; the modal
//! stays until the Core's `request.updated` settles the request. Escape
//! puts it aside ("稍后处理"); the timeline keeps its card.

use nsh_core::{LineHeight, prelude::*};

use super::{ghost_button, primary_button};
use crate::{
    controller,
    state::{ApprovalDecision, ApprovalView, Risk},
    theme::{PALETTE, metrics},
};

pub(crate) fn approval_modal() -> impl IntoElement {
    let current = computed(|| controller::state().with(|s| s.modal_request()));
    let current_id = computed(move || current.with(|v| v.as_ref().map(|v| v.request_id.clone())));
    // Rebuilt per request, so an input's text starts from its prefill.
    dynamic(move || {
        current_id
            .get()
            .map(|id| modal_for(id, current).into_element())
    })
}

fn risk_color(risk: Risk) -> AdaptiveColor {
    match risk {
        Risk::Low => PALETTE.success,
        Risk::Medium => PALETTE.warning,
        Risk::High => PALETTE.danger,
    }
}

fn modal_for(request_id: String, current: Memo<Option<ApprovalView>>) -> impl IntoElement {
    let view_of = move || current.get();
    let field = move |f: fn(&ApprovalView) -> String| {
        move || current.with(|v| v.as_ref().map(f).unwrap_or_default())
    };
    let busy = computed(move || current.with(|v| v.as_ref().is_none_or(|v| !v.answerable())));
    let retry_key = format!("request:{request_id}");
    let unconfirmed = computed({
        let key = retry_key.clone();
        move || {
            controller::state().with(|s| {
                s.pending
                    .iter()
                    .any(|p| p.key == key && !p.in_flight && p.unconfirmed.is_some())
            })
        }
    });
    let thread_title = computed(move || {
        controller::state().with(|s| {
            let thread_id = current.with(|v| v.as_ref().map(|v| v.thread_id.clone()))?;
            if s.selected.as_deref() == Some(thread_id.as_str()) {
                return None;
            }
            s.thread(&thread_id).map(|t| t.title.clone())
        })
    });
    let queue_len = computed(|| controller::state().with(|s| s.approval_queue().len()));
    let value = signal(
        current.with_untracked(|v| v.as_ref().map(|v| v.prefill.clone()).unwrap_or_default()),
    );
    let id = request_id.clone();
    let answer = move |decision: ApprovalDecision| controller::respond(id.clone(), decision);

    let defer_id = request_id.clone();
    nsh_base::dialog()
        .animation(ModalAnimation::Fade)
        .on_cancel(move || controller::defer_request(defer_id.clone()))
        .child(
            view()
                .flex_1()
                .items_center()
                .justify_center()
                .bg(PALETTE.scrim)
                .p_6()
                .child(
                    view()
                        .w_full()
                        .max_w(px(metrics::MODAL_WIDTH))
                        .flex_col()
                        .gap_3()
                        .p_5()
                        .rounded(px(14.0))
                        .bg(PALETTE.approval)
                        .border_1()
                        .border_color(PALETTE.approval_border)
                        .shadow_xl()
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .child(
                                    text(move || {
                                        if busy.get() {
                                            "正在提交响应…".to_owned()
                                        } else {
                                            "需要你的确认".to_owned()
                                        }
                                    })
                                    .font_weight(FontWeight::SEMIBOLD)
                                    .text_size(px(13.0))
                                    .text_color(PALETTE.warning),
                                )
                                .child(view().flex_1())
                                .child(when(
                                    move || queue_len.get() > 1,
                                    move || {
                                        text(move || format!("共 {} 项待处理", queue_len.get()))
                                            .text_size(px(11.5))
                                            .text_color(PALETTE.muted)
                                    },
                                )),
                        )
                        .child(when(
                            move || thread_title.with(Option::is_some),
                            move || {
                                text(move || {
                                    format!(
                                        "来自会话「{}」",
                                        thread_title.get().unwrap_or_default()
                                    )
                                })
                                .text_size(px(12.0))
                                .text_color(PALETTE.muted)
                            },
                        ))
                        .child(
                            text(field(|v| v.headline.clone()))
                                .select_text()
                                .font_weight(FontWeight::MEDIUM)
                                .text_size(px(15.0))
                                .text_color(PALETTE.ink),
                        )
                        .child(when(
                            move || {
                                current.with(|v| v.as_ref().is_some_and(|v| !v.body.is_empty()))
                            },
                            move || {
                                scroll()
                                    .max_h(px(220.0))
                                    .rounded(px(8.0))
                                    .bg(PALETTE.code)
                                    .child(
                                        text(field(|v| v.body.clone()))
                                            .select_text()
                                            .font_family("Menlo")
                                            .text_size(px(12.0))
                                            .line_height(LineHeight::Multiple(1.5))
                                            .text_color(PALETTE.ink)
                                            .px_3()
                                            .py_2(),
                                    )
                            },
                        ))
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .child(super::dot(move || {
                                    risk_color(
                                        current.with(|v| v.as_ref().map_or(Risk::Low, |v| v.risk)),
                                    )
                                }))
                                .child(
                                    text(move || {
                                        current.with(|v| v.as_ref().map_or("", |v| v.risk.hint()))
                                    })
                                    .text_size(px(12.0))
                                    .text_color(PALETTE.muted),
                                ),
                        )
                        .child(when(
                            move || {
                                current.with(|v| v.as_ref().is_some_and(|v| !v.error.is_empty()))
                            },
                            move || {
                                text(field(|v| v.error.clone()))
                                    .text_size(px(12.0))
                                    .text_color(PALETTE.danger)
                            },
                        ))
                        .child(answers(view_of(), value, busy, answer))
                        .child(when(
                            move || unconfirmed.get(),
                            move || {
                                let key = retry_key.clone();
                                view()
                                    .flex_row()
                                    .items_center()
                                    .gap_2()
                                    .child(
                                        text("响应未确认，重试将沿用原来的回答。")
                                            .flex_1()
                                            .text_size(px(11.5))
                                            .text_color(PALETTE.warning),
                                    )
                                    .child(ghost_button("重试", move || {
                                        controller::retry(key.clone())
                                    }))
                            },
                        ))
                        .child(
                            view()
                                .flex_row()
                                .justify_start()
                                .child(ghost_button("稍后处理", move || {
                                    controller::defer_request(request_id.clone())
                                })),
                        ),
                ),
        )
}

/// The answer buttons for the request's method.
fn answers(
    request: Option<ApprovalView>,
    value: Signal<String>,
    busy: Memo<bool>,
    answer: impl Fn(ApprovalDecision) + Clone + 'static,
) -> AnyElement {
    let Some(request) = request else {
        return view().into_element();
    };
    if request.is_approval() {
        let deny_label = request.deny.clone().unwrap_or_else(|| "拒绝".to_owned());
        let allow_label = if request.method == "confirm" {
            "仅允许本次".to_owned()
        } else {
            request
                .allow_once
                .clone()
                .unwrap_or_else(|| "允许本次".to_owned())
        };
        let (deny, session, allow) = (answer.clone(), answer.clone(), answer);
        return nsh_base_row()
            .child(view_spacer())
            .child(
                ghost_button(deny_label, move || deny(ApprovalDecision::Deny))
                    .disabled(move || busy.get()),
            )
            .child(when(request.allow_session.is_some(), {
                let label = request.allow_session.clone().unwrap_or_default();
                move || {
                    let session = session.clone();
                    ghost_button(label.clone(), move || {
                        session(ApprovalDecision::AllowSession)
                    })
                    .disabled(move || busy.get())
                }
            }))
            .child(
                primary_button(allow_label, move || allow(ApprovalDecision::Allow))
                    .disabled(move || busy.get()),
            )
            .into_element();
    }
    match request.method.as_str() {
        "select" => {
            let cancel = answer.clone();
            let options = request.options.clone();
            nsh_base_row()
                .flex_wrap()
                .child(
                    options
                        .into_iter()
                        .map(|option| {
                            let choose = answer.clone();
                            let label = option.clone();
                            ghost_button(label, move || {
                                choose(ApprovalDecision::Choose(option.clone()))
                            })
                            .disabled(move || busy.get())
                        })
                        .collect::<Vec<_>>(),
                )
                .child(view_spacer())
                .child(
                    ghost_button("取消请求", move || cancel(ApprovalDecision::Cancel))
                        .disabled(move || busy.get()),
                )
                .into_element()
        }
        _ => {
            // input / editor: text, then submit or cancel.
            let multiline = request.method == "editor";
            let (cancel, submit) = (answer.clone(), answer);
            view_column()
                .child(
                    nsh_base::input()
                        .multiline(multiline)
                        .value(move || value.get())
                        .on_change(move |text: &str| value.set(text.to_owned()))
                        .placeholder("输入回复")
                        .auto_focus(true)
                        .min_h(px(if multiline { 96.0 } else { 34.0 }))
                        .px_3()
                        .py_2()
                        .rounded(px(8.0))
                        .bg(PALETTE.card)
                        .border_1()
                        .border_color(PALETTE.line)
                        .text_size(px(13.0))
                        .text_color(PALETTE.ink),
                )
                .child(
                    nsh_base_row()
                        .child(view_spacer())
                        .child(
                            ghost_button("取消请求", move || cancel(ApprovalDecision::Cancel))
                                .disabled(move || busy.get()),
                        )
                        .child(
                            primary_button("提交响应", move || {
                                submit(ApprovalDecision::Submit(value.get_untracked()))
                            })
                            .disabled(move || busy.get()),
                        ),
                )
                .into_element()
        }
    }
}

fn nsh_base_row() -> View {
    view().flex_row().items_center().gap_2()
}

fn view_column() -> View {
    view().flex_col().gap_2()
}

fn view_spacer() -> View {
    view().flex_1()
}
