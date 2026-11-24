//! The timeline: a virtual list over the shown thread's rows, keyed by
//! their stable ids, opening at the latest message and following
//! streamed text while the reader is at the end; scrolled up, it stays put
//! and offers "跳到最新".
//!
//! The list re-keys only when the thread's row ids change
//! ([`crate::state::TimelineIndex::structure`]); a streamed delta changes
//! one row's memo, whose Markdown re-renders only its last block.

use std::sync::Arc;

use nsh_core::{LineHeight, ListAlignment, prelude::*};

use super::{dot, ghost_button, markdown, primary_button, small_icon_button};
use crate::{
    controller,
    icons::{Icon, icon},
    state::{ApprovalDecision, ApprovalView, Risk, TimelineItem, ToolStatus, markdown::MarkdownDoc},
    theme::{metrics, palette},
};

/// Within this distance of the end, new content keeps the list at the end.
const FOLLOW_THRESHOLD: f32 = 120.0;

/// A row's kind: a cell is rebuilt only when the kind at its position
/// changes; streaming updates rebind its parts in place.
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
    let thread = computed(|| controller::state().with(|s| s.selected.clone()));
    // Each thread opens at its latest turn. Stream updates keep its native scroll position.
    dynamic(move || {
        thread.get();
        untracked(|| thread_timeline(thread))
    })
}

fn thread_timeline(thread: Memo<Option<String>>) -> impl IntoElement {
    let colors = palette();
    // The thread and its structure revision: the keys follow this only.
    let structure = computed(move || {
        let thread = thread.get();
        controller::state().with(|s| {
            thread.map(|t| {
                let revision = s.timeline.structure(&t);
                (t, revision)
            })
        })
    });
    let keys = computed(move || {
        let current = structure.get();
        controller::state().with_untracked(|s| {
            current
                .map(|(t, _)| s.timeline.keys(&t))
                .unwrap_or_default()
        })
    });
    let at_end = signal(true);
    let list = element_ref();
    let opening = std::cell::Cell::new(true);

    view()
        .flex_1()
        .relative()
        .bg(colors.canvas)
        .child(
            virtual_list(move || keys.with(Vec::len), move |at| row(at, thread))
                .keys(move || keys.get())
                .estimated_item_size(px(72.0))
                .alignment(ListAlignment::Top)
                .follow_end(px(FOLLOW_THRESHOLD))
                .content_padding_y(px(20.0))
                .on_layout(move |bounds: &Bounds<Points>| {
                    if bounds.size.height.0 > 0.0 && opening.replace(false) {
                        list.scroll_to_end(false);
                    }
                })
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
            move || !at_end.get() && keys.with(|k| !k.is_empty()),
            move || {
                view().absolute().bottom_3().right_6().child(
                    nsh_base::button()
                        .on_press(move || {
                            list.scroll_to_end(true);
                            at_end.set(true);
                        })
                        .flex_row()
                        .items_center()
                        .gap_1()
                        .px_3()
                        .py_1p5()
                        .rounded_full()
                        .bg(colors.card)
                        .border_1()
                        .border_color(colors.line)
                        .shadow_sm()
                        .active(|s| s.opacity(0.7))
                        .child(icon(Icon::ArrowDown, 13.0, colors.ink))
                        .child(text("跳到最新").text_size(px(12.0)).text_color(colors.ink)),
                )
            },
        ))
}

/// A cell at `at`: its parts follow the row now at that position.
fn row(at: Position, thread: Memo<Option<String>>) -> impl IntoElement {
    let item = computed(move || {
        let thread = thread.get();
        controller::state().with(|s| {
            thread.and_then(|t| s.timeline.item(&t, at.index()).cloned())
        })
    });
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

fn attachment_short_name(item: &vantry_protocol::Attachment, image_index: usize) -> String {
    if item.mime.starts_with("image/") && crate::state::draft::is_generic_image_name(&item.name) {
        format!("图{image_index}")
    } else {
        crate::state::draft::middle_truncate_name(&item.name, 20)
    }
}

/// Replaces `@attachment-name` references in a sent message with highlighted inline tokens.
fn highlight_attachment_mentions(
    inlines: Vec<crate::state::markdown::Inline>,
    files: &[vantry_protocol::Attachment],
) -> Vec<crate::state::markdown::Inline> {
    if files.is_empty() {
        return inlines;
    }
    let mut pairs: Vec<(String, String)> = Vec::new();
    let mut img_seq = 0usize;
    for file in files {
        let short = if file.mime.starts_with("image/")
            && crate::state::draft::is_generic_image_name(&file.name)
        {
            img_seq += 1;
            format!("@图{img_seq}")
        } else {
            format!("@{}", crate::state::draft::middle_truncate_name(&file.name, 20))
        };
        let raw = format!("@{}", file.name);
        pairs.push((raw, short.clone()));
        if !pairs.iter().any(|(k, _)| k == &short) {
            pairs.push((short.clone(), short));
        }
    }
    pairs.sort_by_key(|(k, _)| std::cmp::Reverse(k.len()));

    let mut out = Vec::with_capacity(inlines.len());
    for inline in inlines {
        let crate::state::markdown::Inline::Text(text) = inline else {
            out.push(inline);
            continue;
        };
        let mut rest = text.as_str();
        while let Some((at, (needle, display))) = pairs
            .iter()
            .filter_map(|p| rest.find(&p.0).map(|pos| (pos, p)))
            .min_by_key(|(pos, _)| *pos)
        {
            if at > 0 {
                out.push(crate::state::markdown::Inline::Text(rest[..at].into()));
            }
            out.push(crate::state::markdown::Inline::Code(display.clone()));
            rest = &rest[at + needle.len()..];
        }
        if !rest.is_empty() {
            out.push(crate::state::markdown::Inline::Text(rest.into()));
        }
    }
    out
}

fn user_row(item: ItemMemo) -> impl IntoElement {
    let colors = palette();
    let user = computed(move || match item.get() {
        Some(TimelineItem::User {
            run_id,
            text,
            plan,
            attachments,
            ..
        }) => Some((run_id, text, plan, attachments)),
        _ => None,
    });
    let text_of = move || user.with(|u| u.as_ref().map(|u| u.1.to_string()).unwrap_or_default());
    let plan = move || user.with(|u| u.as_ref().is_some_and(|u| u.2));
    let attachments = move || user.with(|u| u.as_ref().map(|u| u.3.clone()).unwrap_or_default());
    let visible_text = computed(move || text_of());
    let run_id = move || user.with_untracked(|u| u.as_ref().map(|u| u.0.clone()).unwrap_or_default());
    // Rewind and fork need the run finished and the thread idle.
    let settled = computed(move || {
        let id = user.with(|u| u.as_ref().map(|u| u.0.clone()));
        controller::state().with(|s| {
            id.and_then(|id| s.run(&id).map(|r| (r.status.terminal(), r.thread_id.clone())))
                .is_some_and(|(done, thread)| done && s.active_run(&thread).is_none())
        })
    });
    let thread_id = move || controller::state().with_untracked(|s| s.selected.clone().unwrap_or_default());
    let hovered = signal(false);
    view()
        .w_full()
        .flex_row()
        .justify_end()
        .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
        .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
        .child(
            view()
                .max_w(px(560.0))
                .flex_col()
                .items_end()
                .gap_1()
                .child(when(plan, || {
                    text("规划模式")
                        .text_size(px(10.5))
                        .text_color(colors.accent)
                }))
                .child(
                    view()
                        .flex_row()
                        .items_end()
                        .gap_1p5()
                        .child(when(move || !visible_text.get().trim().is_empty(), move ||
                            view()
                                .px_4()
                                .py_2p5()
                                .rounded(px(16.0))
                                .bg(colors.user)
                                .child(dynamic(move || {
                                    let text = visible_text.get();
                                    let files = attachments();
                                    let (skill, body) = crate::state::thread_links::skill_prefix(&text);
                                    let mut inlines = highlight_attachment_mentions(
                                        crate::state::thread_links::message_inlines(body),
                                        &files,
                                    );
                                    if let Some(name) = skill {
                                        inlines.insert(0, crate::state::markdown::Inline::Text(" ".into()));
                                        inlines.insert(0, crate::state::markdown::Inline::Code(name.to_owned()));
                                    }
                                    markdown::inline_text(&inlines)
                                        .select_text()
                                        .text_size(px(metrics::BODY))
                                        .line_height(LineHeight::Multiple(1.55))
                                        .text_color(colors.ink)
                                }))
                        ))
                        .child(
                            view()
                                .pb_1()
                                .opacity(move || if hovered.get() { 1.0_f32 } else { 0.45_f32 })
                                .child(small_icon_button(Icon::Copy, "复制消息", move || {
                                    controller::copy(text_of());
                                })),
                        ),
                )
                .child(when(
                    move || !attachments().is_empty(),
                    move || {
                        let numbered = computed(move || {
                            let mut img_seq = 0usize;
                            attachments()
                                .into_iter()
                                .map(|item| {
                                    let lbl = if item.mime.starts_with("image/")
                                        && crate::state::draft::is_generic_image_name(&item.name)
                                    {
                                        img_seq += 1;
                                        attachment_short_name(&item, img_seq)
                                    } else {
                                        attachment_short_name(&item, 0)
                                    };
                                    (item, lbl)
                                })
                                .collect::<Vec<_>>()
                        });
                        view().flex_row().flex_wrap().items_center().gap_1p5().child(each(
                            move || numbered.get(),
                            |(item, _): &(vantry_protocol::Attachment, String)| item.id.clone(),
                            |entry, _| {
                                dynamic(move || {
                                    let (item, label) = entry.get();
                                    if item.mime.starts_with("image/") {
                                        super::composer::attachment_thumbnail_labeled(
                                            item,
                                            Some(label),
                                        )
                                        .into_element()
                                    } else {
                                        super::caption(format!("@{label}")).into_element()
                                    }
                                })
                            },
                        ))
                    },
                ))
                .child(when(
                    move || hovered.get() && settled.get(),
                    move || {
                        view()
                            .flex_row()
                            .gap_0p5()
                            .pr_7()
                            .child(small_icon_button(Icon::GitFork, "从此分叉", move || {
                                controller::fork(thread_id(), Some(run_id()));
                            }))
                            .child(small_icon_button(Icon::Undo2, "回退到此前", move || {
                                controller::ask_rewind(thread_id(), run_id());
                            }))
                    },
                )),
        )
}

fn reasoning_row(item: ItemMemo) -> impl IntoElement {
    let colors = palette();
    let reasoning = computed(move || match item.get() {
        Some(TimelineItem::Reasoning {
            text, streaming, ..
        }) => Some((text, streaming)),
        _ => None,
    });
    let streaming = move || reasoning.with(|r| r.as_ref().is_some_and(|r| r.1));
    let open = signal(false);
    view()
        .flex_col()
        .gap_1()
        .pl_3()
        .border_l(px(2.0))
        .border_l_color(colors.line)
        .child(
            nsh_base::button()
                .accessibility_label("展开思考")
                .on_press(move || open.set(!open.get_untracked()))
                .flex_row()
                .items_center()
                .gap_1p5()
                .active(|s| s.opacity(0.6))
                .child(icon(Icon::Brain, 13.0, colors.faint))
                .child(
                    text(move || if streaming() { "思考中…" } else { "已思考" })
                        .text_size(px(11.5))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.faint),
                )
                .child(dynamic(move || {
                    icon(
                        if open.get() {
                            Icon::ChevronDown
                        } else {
                            Icon::ChevronRight
                        },
                        12.0,
                        colors.faint,
                    )
                    .into_element()
                })),
        )
        .child(
            text(move || {
                reasoning.with(|r| r.as_ref().map(|r| r.0.to_string()).unwrap_or_default())
            })
            .select_text()
            .number_of_lines(move || if open.get() { 0u32 } else { 2u32 })
            .italic()
            .text_size(px(13.0))
            .line_height(LineHeight::Multiple(1.5))
            .text_color(colors.muted),
        )
}

fn text_row(item: ItemMemo) -> impl IntoElement {
    let colors = palette();
    let doc = computed(move || match item.get() {
        Some(TimelineItem::Text { doc, .. }) => Some(doc),
        _ => None::<Arc<MarkdownDoc>>,
    });
    let streaming = move || {
        matches!(
            item.get(),
            Some(TimelineItem::Text {
                streaming: true,
                ..
            })
        )
    };
    let range = move || match item.get_untracked() {
        Some(TimelineItem::Text {
            run_id, start, end, ..
        }) => Some((run_id, start, end)),
        _ => None,
    };
    let settled = computed(move || {
        let run = match item.get() {
            Some(TimelineItem::Text { run_id, .. }) => Some(run_id),
            _ => None,
        };
        controller::state().with(|s| {
            run.and_then(|id| s.run(&id).map(|r| (r.status.terminal(), r.thread_id.clone())))
                .is_some_and(|(done, thread)| done && s.active_run(&thread).is_none())
        })
    });
    let thread_id = move || controller::state().with_untracked(|s| s.selected.clone().unwrap_or_default());
    let hovered = signal(false);
    view()
        .flex_col()
        .gap_1p5()
        .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
        .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
        .child(markdown::document(doc))
        .child(when(streaming, || {
            view()
                .mt_1()
                .w(px(8.0))
                .h(px(8.0))
                .rounded_full()
                .bg(colors.accent)
                .animate_pulse()
        }))
        // Quiet v0.1.42 reply action bar: copy / quote / fork / rewind / preview
        .child(when(
            move || !streaming(),
            move || {
                view()
                    .flex_row()
                    .items_center()
                    .gap_1()
                    .opacity(move || if hovered.get() { 1.0_f32 } else { 0.55_f32 })
                    .child(small_icon_button(Icon::Copy, "复制回复", move || {
                        if let Some((run, start, end)) = range() {
                            controller::copy_reply(run, start, end);
                        }
                    }))
                    .child(small_icon_button(Icon::Quote, "引用到输入框", move || {
                        if let Some((run, start, end)) = range() {
                            let snippet = controller::state()
                                .with_untracked(|s| s.text_of(&run, start, end));
                            controller::quote_to_draft(snippet);
                        }
                    }))
                    .child(
                        small_icon_button(Icon::GitFork, "从此分叉", move || {
                            if let Some((run, _, _)) = range() {
                                controller::fork(thread_id(), Some(run));
                            }
                        })
                        .disabled(move || !settled.get()),
                    )
                    .child(
                        small_icon_button(Icon::Undo2, "回退到此前", move || {
                            if let Some((run, _, _)) = range() {
                                controller::ask_rewind(thread_id(), run);
                            }
                        })
                        .disabled(move || !settled.get()),
                    )
                    .child(small_icon_button(Icon::Eye, "在预览中打开", move || {
                        if let Some((run, start, end)) = range() {
                            let text = controller::state()
                                .with_untracked(|s| s.text_of(&run, start, end));
                            controller::preview_text(String::new(), text);
                        }
                    }))
            },
        ))
}

fn tool_status_color(status: ToolStatus) -> AdaptiveColor {
    let colors = palette();
    match status {
        ToolStatus::Running => colors.accent,
        ToolStatus::AwaitingApproval => colors.warning,
        ToolStatus::Completed => colors.success,
        ToolStatus::Failed | ToolStatus::Interrupted => colors.muted,
    }
}

fn tool_row(item: ItemMemo) -> impl IntoElement {
    let colors = palette();
    let tool = computed(move || match item.get() {
        Some(TimelineItem::Tool {
            name,
            summary,
            status,
            arguments,
            output,
            ..
        }) => Some((name, summary, status, arguments, output)),
        _ => None,
    });
    let status = move || tool.with(|t| t.as_ref().map_or(ToolStatus::Running, |t| t.2));
    let field = move |f: fn(&(String, String, ToolStatus, String, String)) -> String| {
        move || tool.with(|t| t.as_ref().map(f).unwrap_or_default())
    };
    let open = signal(false);
    view()
        .flex_col()
        .rounded(px(metrics::CARD_RADIUS))
        .border_1()
        .border_color(colors.line)
        .overflow_hidden()
        .child(
            nsh_base::button()
                .accessibility_label("展开工具调用")
                .on_press(move || open.set(!open.get_untracked()))
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .py_2()
                .hover(|s| s.bg(colors.soft))
                .child(dot(move || tool_status_color(status())))
                .child(dynamic(move || {
                    let name = tool
                        .with(|t| t.as_ref().map(|t| t.0.to_ascii_lowercase()).unwrap_or_default());
                    let glyph = match name.as_str() {
                        "bash" | "shell" | "terminal" | "vantry_terminal_run" => Icon::Terminal,
                        "read" | "write" | "edit" => Icon::FileText,
                        "grep" | "find" | "ls" => Icon::Search,
                        _ => Icon::Wrench,
                    };
                    icon(glyph, 13.5, colors.muted).into_element()
                }))
                .child(
                    text(field(|t| t.0.clone()))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_size(px(12.5))
                        .text_color(colors.ink),
                )
                .child(
                    text(field(|t| t.1.clone()))
                        .flex_1()
                        .number_of_lines(1u32)
                        .font_family("Menlo")
                        .text_size(px(12.0))
                        .text_color(colors.muted),
                )
                .child(when(move || open.get() || matches!(status(), ToolStatus::Running | ToolStatus::AwaitingApproval), move ||
                    text(move || status().label()).text_size(px(11.0)).text_color(move || tool_status_color(status()))
                ))
                .child(dynamic(move || {
                    icon(
                        if open.get() {
                            Icon::ChevronDown
                        } else {
                            Icon::ChevronRight
                        },
                        13.0,
                        colors.faint,
                    )
                    .into_element()
                })),
        )
        .child(when(
            move || open.get() && tool.with(|t| t.as_ref().is_some_and(|t| !t.3.is_empty())),
            move || {
                view().px_3().py_2().border_t_1().border_t_color(colors.line_soft).child(
                    text(field(|t| t.3.clone()))
                        .select_text()
                        .font_family("Menlo")
                        .text_size(px(11.5))
                        .line_height(LineHeight::Multiple(1.45))
                        .text_color(colors.muted),
                )
            },
        ))
        .child(when(
            move || open.get() && tool.with(|t| t.as_ref().is_some_and(|t| !t.4.is_empty())),
            move || {
                view().px_3().py_2().bg(colors.code).child(
                    text(field(|t| t.4.clone()))
                        .select_text()
                        .number_of_lines(move || if open.get() { 0u32 } else { 6u32 })
                        .font_family("Menlo")
                        .text_size(px(11.5))
                        .line_height(LineHeight::Multiple(1.45))
                        .text_color(colors.muted),
                )
            },
        ))
}

fn approval_row(item: ItemMemo) -> impl IntoElement {
    let colors = palette();
    let view_of = computed(move || match item.get() {
        Some(TimelineItem::Approval { view, .. }) => Some(view),
        _ => None,
    });
    let headline =
        move || view_of.with(|v| v.as_ref().map(|v| v.headline.clone()).unwrap_or_default());
    let body = move || view_of.with(|v| v.as_ref().map(|v| v.body.clone()).unwrap_or_default());
    let state_text = move || {
        view_of.with(|v| match v.as_ref().map(|v| v.status.as_str()) {
            Some("responding") => "正在提交响应…".to_owned(),
            Some("failed") => "响应失败".to_owned(),
            _ => "需要你的确认".to_owned(),
        })
    };
    let risk = move || view_of.with(|v| v.as_ref().map_or(Risk::Low, |v: &ApprovalView| v.risk));
    let answerable = move || view_of.with(|v| v.as_ref().is_some_and(ApprovalView::answerable));
    let quick = move || view_of.with(|v| v.as_ref().is_some_and(|v| v.is_approval() && v.answerable()));
    let session = move || view_of.with(|v| v.as_ref().is_some_and(|v| v.allow_session.is_some()));
    let id = move || view_of.with_untracked(|v| v.as_ref().map(|v| v.request_id.clone()).unwrap_or_default());
    view()
        .flex_col()
        .gap_2()
        .px_4()
        .py_3()
        .rounded(px(12.0))
        .bg(colors.approval)
        .border_1()
        .border_color(colors.approval_border)
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1p5()
                .child(icon(Icon::ShieldCheck, 14.0, colors.warning))
                .child(
                    text(state_text)
                        .text_size(px(12.0))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.warning),
                ),
        )
        .child(
            text(headline)
                .select_text()
                .number_of_lines(2u32)
                .text_size(px(13.5))
                .text_color(colors.ink),
        )
        .child(when(
            move || !body().is_empty(),
            move || {
                text(body)
                    .select_text()
                    .number_of_lines(4u32)
                    .font_family("Menlo")
                    .text_size(px(12.0))
                    .text_color(colors.ink)
                    .bg(colors.code)
                    .rounded(px(6.0))
                    .px_2()
                    .py_1()
            },
        ))
        .child(
            text(move || risk().hint())
                .text_size(px(11.5))
                .text_color(colors.muted),
        )
        .child(
            view()
                .flex_row()
                .gap_2()
                .child(view().flex_1())
                .child(when(quick, move || {
                    view()
                        .flex_row()
                        .gap_2()
                        .child(ghost_button("拒绝", move || {
                            controller::respond(id(), ApprovalDecision::Deny)
                        }))
                        .child(when(session, move || {
                            ghost_button("本会话允许", move || {
                                controller::respond(id(), ApprovalDecision::AllowSession)
                            })
                        }))
                        .child(primary_button("仅允许本次", move || {
                            controller::respond(id(), ApprovalDecision::Allow)
                        }))
                }))
                .child(when(
                    move || answerable() && !quick(),
                    move || ghost_button("处理", move || controller::reopen_request(id())),
                )),
        )
}

fn status_row(item: ItemMemo) -> impl IntoElement {
    let colors = palette();
    let error = move || matches!(item.get(), Some(TimelineItem::Status { error: true, .. }));
    text(move || match item.get() {
        Some(TimelineItem::Status { text, .. }) => text,
        _ => String::new(),
    })
    .select_text()
    .text_size(px(12.5))
    .text_color(move || {
        if error() {
            colors.danger
        } else {
            colors.faint
        }
    })
}
