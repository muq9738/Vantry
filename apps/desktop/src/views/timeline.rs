//! The timeline: a virtual list keyed by the rows' stable ids, resting at
//! the bottom like a chat and following streamed text while the reader is
//! at the end; scrolled up, it stays put and offers "跳到最新".

use nsh_core::{LineHeight, ListAlignment, prelude::*};

use crate::{
    controller,
    state::{ApprovalView, Risk, TimelineItem, ToolStatus},
    theme::{PALETTE, metrics},
};

/// Within this distance of the end, new content keeps the list at the end.
const FOLLOW_THRESHOLD: f32 = 120.0;

/// A row's kind: a cell is rebuilt only when the kind at its position
/// changes; streaming updates rebind its text in place.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Kind {
    User,
    Reasoning,
    Text,
    Tool,
    Approval,
    Status,
}

fn kind_of(item: &TimelineItem) -> Kind {
    match item {
        TimelineItem::User { .. } => Kind::User,
        TimelineItem::Reasoning { .. } => Kind::Reasoning,
        TimelineItem::Text { .. } => Kind::Text,
        TimelineItem::Tool { .. } => Kind::Tool,
        TimelineItem::Approval { .. } => Kind::Approval,
        TimelineItem::Status { .. } => Kind::Status,
    }
}

pub(crate) fn timeline() -> impl IntoElement {
    let items = computed(|| controller::state().with(|s| s.timeline()));
    let has_thread = computed(|| controller::state().with(|s| s.selected.is_some()));
    let at_end = signal(true);
    let list = element_ref();

    view()
        .flex_1()
        .relative()
        .bg(PALETTE.canvas)
        .child(
            virtual_list(move || items.with(Vec::len), move |at| row(at, items))
                .keys(move || {
                    items.with(|items| items.iter().map(|i| i.id().to_owned()).collect::<Vec<_>>())
                })
                .estimated_item_size(px(72.0))
                .alignment(ListAlignment::Bottom)
                .follow_end(px(FOLLOW_THRESHOLD))
                .content_padding_y(px(20.0))
                .empty(empty_state(has_thread))
                .on_scroll(move |e: &ScrollEvent| {
                    let distance = e.content_size.height.0 - (e.offset.y.0 + e.size.height.0);
                    let near = distance <= FOLLOW_THRESHOLD;
                    if at_end.get_untracked() != near {
                        at_end.set(near);
                    }
                })
                .element_ref(&list)
                .flex_1(),
        )
        .child(when(
            move || !at_end.get() && items.with(|i| !i.is_empty()),
            move || {
                view().absolute().bottom_3().right_6().child(
                    nsh_base::button()
                        .on_press(move || {
                            list.scroll_to_end(true);
                            at_end.set(true);
                        })
                        .px_3()
                        .py_1()
                        .rounded_full()
                        .bg(PALETTE.card)
                        .border_1()
                        .border_color(PALETTE.line)
                        .shadow_sm()
                        .active(|s| s.opacity(0.7))
                        .child(
                            text("跳到最新 ↓")
                                .text_size(px(12.0))
                                .text_color(PALETTE.ink),
                        ),
                )
            },
        ))
}

fn empty_state(has_thread: Memo<bool>) -> impl IntoElement {
    view()
        .flex_1()
        .items_center()
        .justify_center()
        .py_16()
        .gap_2()
        .child(
            text(move || {
                if has_thread.get() {
                    "这个会话还没有消息"
                } else {
                    "开始一段新对话"
                }
            })
            .text_size(px(22.0))
            .font_weight(FontWeight::SEMIBOLD)
            .text_color(PALETTE.ink),
        )
        .child(
            text("Enter 发送，Shift+Enter 换行；运行中发送会追加给当前任务。")
                .text_size(px(13.0))
                .text_color(PALETTE.muted),
        )
}

/// A cell at `at`: its parts follow the item now at that position.
fn row(at: Position, items: Memo<Vec<TimelineItem>>) -> impl IntoElement {
    let item = computed(move || items.with(|items| items.get(at.index()).cloned()));
    let kind = computed(move || item.with(|i| i.as_ref().map(kind_of)));
    view()
        .w_full()
        .flex_row()
        .justify_center()
        .px_6()
        .py_1p5()
        .child(
            view()
                .flex_1()
                .max_w(px(metrics::READING_WIDTH))
                .child(dynamic(move || match kind.get() {
                    Some(Kind::User) => user_row(item).into_element(),
                    Some(Kind::Reasoning) => reasoning_row(item).into_element(),
                    Some(Kind::Text) => text_row(item).into_element(),
                    Some(Kind::Tool) => tool_row(item).into_element(),
                    Some(Kind::Approval) => approval_row(item).into_element(),
                    Some(Kind::Status) => status_row(item).into_element(),
                    None => view().into_element(),
                })),
        )
}

type ItemMemo = Memo<Option<TimelineItem>>;

fn user_row(item: ItemMemo) -> impl IntoElement {
    let text_of = move || match item.get() {
        Some(TimelineItem::User { text, .. }) => text,
        _ => String::new(),
    };
    let plan = move || matches!(item.get(), Some(TimelineItem::User { plan: true, .. }));
    view().w_full().flex_row().justify_end().child(
        view()
            .max_w(px(560.0))
            .flex_col()
            .items_end()
            .gap_1()
            .child(when(plan, || {
                text("规划模式")
                    .text_size(px(10.5))
                    .text_color(PALETTE.accent)
            }))
            .child(
                view()
                    .px_4()
                    .py_2p5()
                    .rounded(px(14.0))
                    .bg(PALETTE.user)
                    .child(
                        text(text_of)
                            .select_text()
                            .text_size(px(metrics::BODY))
                            .line_height(LineHeight::Multiple(1.55))
                            .text_color(PALETTE.ink),
                    ),
            ),
    )
}

fn reasoning_row(item: ItemMemo) -> impl IntoElement {
    let streaming = move || {
        matches!(
            item.get(),
            Some(TimelineItem::Reasoning {
                streaming: true,
                ..
            })
        )
    };
    view()
        .flex_col()
        .gap_1()
        .pl_3()
        .border_l(px(2.0))
        .border_l_color(PALETTE.line)
        .child(
            text(move || {
                if streaming() {
                    "思考中…"
                } else {
                    "思考"
                }
            })
            .text_size(px(11.5))
            .font_weight(FontWeight::MEDIUM)
            .text_color(PALETTE.faint),
        )
        .child(
            text(move || match item.get() {
                Some(TimelineItem::Reasoning { text, .. }) => text,
                _ => String::new(),
            })
            .select_text()
            .number_of_lines(6u32)
            .italic()
            .text_size(px(13.0))
            .line_height(LineHeight::Multiple(1.5))
            .text_color(PALETTE.muted),
        )
}

fn text_row(item: ItemMemo) -> impl IntoElement {
    let streaming = move || {
        matches!(
            item.get(),
            Some(TimelineItem::Text {
                streaming: true,
                ..
            })
        )
    };
    view()
        .flex_col()
        .child(
            text(move || match item.get() {
                Some(TimelineItem::Text { text, .. }) => text,
                _ => String::new(),
            })
            .select_text()
            .text_size(px(metrics::BODY + 0.5))
            .line_height(LineHeight::Multiple(1.65))
            .text_color(PALETTE.ink),
        )
        .child(when(streaming, || {
            view()
                .mt_1()
                .w(px(8.0))
                .h(px(8.0))
                .rounded_full()
                .bg(PALETTE.accent)
                .animate_pulse()
        }))
}

fn tool_status_color(status: ToolStatus) -> AdaptiveColor {
    match status {
        ToolStatus::Running => PALETTE.accent,
        ToolStatus::AwaitingApproval => PALETTE.warning,
        ToolStatus::Completed => PALETTE.success,
        ToolStatus::Failed | ToolStatus::Interrupted => PALETTE.danger,
    }
}

fn tool_row(item: ItemMemo) -> impl IntoElement {
    let tool = computed(move || match item.get() {
        Some(TimelineItem::Tool {
            name,
            summary,
            status,
            output,
            ..
        }) => Some((name, summary, status, output)),
        _ => None,
    });
    let status = move || tool.with(|t| t.as_ref().map_or(ToolStatus::Running, |t| t.2));
    view()
        .flex_col()
        .rounded(px(metrics::CARD_RADIUS))
        .border_1()
        .border_color(PALETTE.line)
        .overflow_hidden()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .py_2()
                .child(super::dot(move || tool_status_color(status())))
                .child(
                    text(move || {
                        tool.with(|t| t.as_ref().map(|t| t.0.clone()).unwrap_or_default())
                    })
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_size(px(12.5))
                    .text_color(PALETTE.ink),
                )
                .child(
                    text(move || {
                        tool.with(|t| t.as_ref().map(|t| t.1.clone()).unwrap_or_default())
                    })
                    .flex_1()
                    .number_of_lines(1u32)
                    .font_family("Menlo")
                    .text_size(px(12.0))
                    .text_color(PALETTE.muted),
                )
                .child(
                    text(move || status().label())
                        .text_size(px(11.0))
                        .text_color(move || tool_status_color(status())),
                ),
        )
        .child(when(
            move || tool.with(|t| t.as_ref().is_some_and(|t| !t.3.is_empty())),
            move || {
                view().px_3().py_2().bg(PALETTE.code).child(
                    text(move || {
                        tool.with(|t| t.as_ref().map(|t| t.3.clone()).unwrap_or_default())
                    })
                    .select_text()
                    .font_family("Menlo")
                    .text_size(px(11.5))
                    .line_height(LineHeight::Multiple(1.45))
                    .text_color(PALETTE.muted),
                )
            },
        ))
}

fn approval_row(item: ItemMemo) -> impl IntoElement {
    let view_of = computed(move || match item.get() {
        Some(TimelineItem::Approval { view, .. }) => Some(view),
        _ => None,
    });
    let headline =
        move || view_of.with(|v| v.as_ref().map(|v| v.headline.clone()).unwrap_or_default());
    let state_text = move || {
        view_of.with(|v| match v.as_ref().map(|v| v.status.as_str()) {
            Some("responding") => "正在提交响应…".to_owned(),
            Some("failed") => "响应失败".to_owned(),
            _ => "需要你的确认".to_owned(),
        })
    };
    let risk = move || view_of.with(|v| v.as_ref().map_or(Risk::Low, |v: &ApprovalView| v.risk));
    view()
        .flex_row()
        .items_center()
        .gap_3()
        .px_4()
        .py_3()
        .rounded(px(12.0))
        .bg(PALETTE.approval)
        .border_1()
        .border_color(PALETTE.approval_border)
        .child(
            view()
                .flex_1()
                .flex_col()
                .gap_1()
                .child(
                    text(state_text)
                        .text_size(px(12.0))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(PALETTE.warning),
                )
                .child(
                    text(headline)
                        .number_of_lines(2u32)
                        .text_size(px(13.5))
                        .text_color(PALETTE.ink),
                )
                .child(
                    text(move || risk().hint())
                        .text_size(px(11.5))
                        .text_color(PALETTE.muted),
                ),
        )
        .child(super::ghost_button("处理", move || {
            if let Some(view) = view_of.get_untracked() {
                controller::reopen_request(view.request_id);
            }
        }))
}

fn status_row(item: ItemMemo) -> impl IntoElement {
    let error = move || matches!(item.get(), Some(TimelineItem::Status { error: true, .. }));
    text(move || match item.get() {
        Some(TimelineItem::Status { text, .. }) => text,
        _ => String::new(),
    })
    .select_text()
    .text_size(px(12.5))
    .text_color(move || {
        if error() {
            PALETTE.danger
        } else {
            PALETTE.faint
        }
    })
}
