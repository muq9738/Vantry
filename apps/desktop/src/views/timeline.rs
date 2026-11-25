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
    state::{
        ApprovalDecision, ApprovalView, Risk, TimelineItem, ToolEntry, ToolKind, ToolStatus,
        language::Language, markdown::MarkdownDoc,
    },
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
    Tools,
    Approval,
    Status,
}

fn kind_of(item: &TimelineItem) -> Kind {
    match item {
        TimelineItem::User { .. } => Kind::User,
        TimelineItem::Reasoning { .. } => Kind::Reasoning,
        TimelineItem::Text { .. } => Kind::Text,
        TimelineItem::Tools { .. } => Kind::Tools,
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
        .min_w(px(0.0))
        .relative()
        .overflow_hidden()
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
        .min_w(px(0.0))
        .flex_row()
        .justify_center()
        .px_6()
        .py_1p5()
        .child(
            view()
                .flex_1()
                .min_w(px(0.0))
                .max_w(px(metrics::READING_WIDTH))
                .child(dynamic(move || match kind.get() {
                    Some(Kind::User) => user_row(item).into_element(),
                    Some(Kind::Reasoning) => reasoning_row(item).into_element(),
                    Some(Kind::Text) => text_row(item).into_element(),
                    Some(Kind::Tools) => tools_row(item).into_element(),
                    Some(Kind::Approval) => approval_row(item).into_element(),
                    Some(Kind::Status) => status_row(item).into_element(),
                    None => view().into_element(),
                })),
        )
}

type ItemMemo = Memo<Option<TimelineItem>>;

fn attachment_short_name(
    item: &vantry_protocol::Attachment,
    image_index: usize,
    lang: crate::state::language::Language,
) -> String {
    if item.mime.starts_with("image/") && image_index > 0 {
        lang.image_label(image_index)
    } else {
        crate::state::draft::middle_truncate_name(&item.name, 20)
    }
}

/// Replaces `@attachment-name` / `图1` / `图 1` / `Image 1` / `画像1` references in a sent
/// message with highlighted, clickable hyperlink tokens (`vantry-attachment://<id>`),
/// adapting display labels to the active UI language.
fn highlight_attachment_mentions(
    inlines: Vec<crate::state::markdown::Inline>,
    files: &[vantry_protocol::Attachment],
    lang: crate::state::language::Language,
) -> Vec<crate::state::markdown::Inline> {
    if files.is_empty() {
        return inlines;
    }
    let mut pairs: Vec<(String, String, String)> = Vec::new();
    let mut img_seq = 0usize;
    for file in files {
        let url = format!("vantry-attachment://{}", file.id);
        let is_image = file.mime.starts_with("image/");
        let display = if is_image {
            img_seq += 1;
            format!("@{}", lang.image_label(img_seq))
        } else {
            format!("@{}", crate::state::draft::middle_truncate_name(&file.name, 20))
        };
        let mut push_alias = |needle: String| {
            if !needle.is_empty() && !pairs.iter().any(|(k, _, _)| k == &needle) {
                pairs.push((needle, display.clone(), url.clone()));
            }
        };
        push_alias(format!("@{}", file.name));
        push_alias(display.clone());
        if is_image {
            let n = img_seq;
            for alias in [
                format!("@图{n}"),
                format!("@图 {n}"),
                format!("@圖{n}"),
                format!("@圖 {n}"),
                format!("@Image {n}"),
                format!("@Image{n}"),
                format!("@image {n}"),
                format!("@画像{n}"),
                format!("@画像 {n}"),
                format!("图{n}"),
                format!("图 {n}"),
                format!("圖{n}"),
                format!("圖 {n}"),
                format!("Image {n}"),
                format!("画像{n}"),
            ] {
                push_alias(alias);
            }
        }
    }
    pairs.sort_by_key(|(k, _, _)| std::cmp::Reverse(k.len()));

    let mut out = Vec::with_capacity(inlines.len());
    for inline in inlines {
        let crate::state::markdown::Inline::Text(text) = inline else {
            out.push(inline);
            continue;
        };
        let mut rest = text.as_str();
        while let Some((at, (needle, display, url))) = pairs
            .iter()
            .filter_map(|p| crate::state::draft::mention_at(rest, &p.0).map(|pos| (pos, p)))
            .min_by_key(|(pos, _)| *pos)
        {
            if at > 0 {
                out.push(crate::state::markdown::Inline::Text(rest[..at].into()));
            }
            out.push(crate::state::markdown::Inline::Link {
                text: display.clone(),
                url: url.clone(),
            });
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
    let revision = computed(|| controller::state().with(|s| s.sidebar_revision));
    let inserted = computed(move || {
        revision.get();
        let id = user.with(|u| u.as_ref().map(|u| u.0.clone()));
        controller::state().with_untracked(|s| s.inputs.iter().filter(|i| {
            Some(i.run_id.as_str()) == id.as_deref() && i.kind == "steer" && matches!(i.status.as_str(), "delivered" | "failed")
        }).map(|i| (i.id.clone(), i.message.clone(), i.status == "delivered", i.error.clone())).collect::<Vec<_>>())
    });
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
        .min_w(px(0.0))
        .flex_row()
        .justify_end()
        .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
        .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
        .child(
            view()
                .w_full()
                .min_w(px(0.0))
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
                        .w_full()
                        .min_w(px(0.0))
                        .flex_row()
                        .justify_end()
                        .items_end()
                        .gap_1p5()
                        .child(when(move || !visible_text.get().trim().is_empty(), move ||
                            view()
                                .flex_shrink(1.0)
                                .min_w(px(0.0))
                                .max_w(px(520.0))
                                .px_4()
                                .py_2p5()
                                .rounded(px(16.0))
                                .bg(colors.user)
                                .overflow_hidden()
                                .child(dynamic(move || {
                                    let text = visible_text.get();
                                    let files = attachments();
                                    let lang = controller::ui().with(|u| u.locale);
                                    let (skill, body) = crate::state::thread_links::skill_prefix(&text);
                                    let mut inlines = highlight_attachment_mentions(
                                        crate::state::thread_links::message_inlines(body),
                                        &files,
                                        lang,
                                    );
                                    if let Some(name) = skill {
                                        inlines.insert(0, crate::state::markdown::Inline::Text(" ".into()));
                                        inlines.insert(0, crate::state::markdown::Inline::Code(name.to_owned()));
                                    }
                                    markdown::inline_text(&inlines)
                                        .w_full()
                                        .min_w(px(0.0))
                                        .select_text()
                                        .text_size(px(metrics::BODY))
                                        .line_height(LineHeight::Multiple(1.55))
                                        .text_color(colors.ink)
                                }))
                        ))
                        .child(
                            view()
                                .flex_shrink_0()
                                .pb_1()
                                .opacity(move || if hovered.get() { 1.0_f32 } else { 0.45_f32 })
                                .child(small_icon_button(Icon::Copy, "复制消息", move || {
                                    controller::copy_message(text_of(), attachments());
                                })),
                        ),
                )
                .child(when(
                    move || !attachments().is_empty(),
                    move || {
                        let numbered = computed(move || {
                            let lang = controller::ui().with(|u| u.locale);
                            let mut img_seq = 0usize;
                            attachments()
                                .into_iter()
                                .map(|item| {
                                    let lbl = if item.mime.starts_with("image/") {
                                        img_seq += 1;
                                        attachment_short_name(&item, img_seq, lang)
                                    } else {
                                        attachment_short_name(&item, 0, lang)
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
                .child(each(move || inserted.get(), |i| i.0.clone(), |item, _| {
                    view().flex_col().items_end().gap_1().child(super::caption(move || item.with(|(_, _, ok, error)| {
                        if *ok { "已插入本轮".to_owned() } else { format!("插入失败 · {error}") }
                    }))).child(text(move || item.with(|i| i.1.clone())).select_text().text_color(colors.ink).text_size(px(metrics::BODY)))
                }))
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
    // Only the reply's last text carries the actions, and they act on the
    // whole reply, as Claude's desktop does: text between tool calls stays
    // quiet instead of repeating a bar after every step.
    let last = computed(move || {
        let Some(TimelineItem::Text { run_id, end, .. }) = item.get() else {
            return false;
        };
        controller::state().with(|s| s.run(&run_id).is_some_and(|r| r.output.len() == end))
    });
    let reply = move || range().map(|(run, _, end)| (run, 0, end));
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
            move || !streaming() && last.get(),
            move || {
                view()
                    .flex_row()
                    .items_center()
                    .gap_1()
                    .opacity(move || if hovered.get() { 1.0_f32 } else { 0.55_f32 })
                    .child(small_icon_button(Icon::Copy, "复制回复", move || {
                        if let Some((run, start, end)) = reply() {
                            controller::copy_reply(run, start, end);
                        }
                    }))
                    .child(small_icon_button(Icon::Quote, "引用到输入框", move || {
                        if let Some((run, start, end)) = reply() {
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
                        if let Some((run, start, end)) = reply() {
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

fn tool_status_label(status: ToolStatus, lang: Language) -> &'static str {
    match (status, lang) {
        (ToolStatus::Running, Language::En) => "Running",
        (ToolStatus::Running, Language::Ja) => "実行中",
        (ToolStatus::AwaitingApproval, Language::En) => "Needs approval",
        (ToolStatus::AwaitingApproval, Language::Ja) => "承認待ち",
        (ToolStatus::Completed, Language::En) => "Done",
        (ToolStatus::Completed, Language::Ja) => "完了",
        (ToolStatus::Failed, Language::En) => "Failed",
        (ToolStatus::Failed, Language::Ja) => "失敗",
        (ToolStatus::Interrupted, Language::En) => "Interrupted",
        (ToolStatus::Interrupted, Language::Ja) => "中断",
        (status, _) => status.label(),
    }
}

fn tool_glyph(name: &str) -> Icon {
    match ToolKind::of(name) {
        ToolKind::Command => Icon::Terminal,
        ToolKind::Read | ToolKind::Edit => Icon::FileText,
        ToolKind::Search => Icon::Search,
        ToolKind::Other => Icon::Wrench,
    }
}

fn live(status: ToolStatus) -> bool {
    matches!(status, ToolStatus::Running | ToolStatus::AwaitingApproval)
}

/// A finished group in one line, as Claude's desktop sums up its steps:
/// "运行了 4 个命令，读取了 2 个文件".
fn tools_headline(calls: &[ToolEntry], lang: Language) -> String {
    let mut counts: Vec<(ToolKind, usize)> = Vec::new();
    for call in calls {
        let kind = ToolKind::of(&call.name);
        match counts.iter_mut().find(|(k, _)| *k == kind) {
            Some((_, n)) => *n += 1,
            None => counts.push((kind, 1)),
        }
    }
    let phrase = |kind: ToolKind, n: usize| -> String {
        let s = if n == 1 { "" } else { "s" };
        match (lang, kind) {
            (Language::En, ToolKind::Command) => format!("ran {n} command{s}"),
            (Language::En, ToolKind::Read) => format!("read {n} file{s}"),
            (Language::En, ToolKind::Edit) => format!("edited {n} file{s}"),
            (Language::En, ToolKind::Search) => format!("searched {n} time{s}"),
            (Language::En, ToolKind::Other) => format!("used {n} tool{s}"),
            (Language::Ja, ToolKind::Command) => format!("{n} 件のコマンドを実行"),
            (Language::Ja, ToolKind::Read) => format!("{n} 件のファイルを読み込み"),
            (Language::Ja, ToolKind::Edit) => format!("{n} 件のファイルを編集"),
            (Language::Ja, ToolKind::Search) => format!("{n} 回検索"),
            (Language::Ja, ToolKind::Other) => format!("{n} 個のツールを使用"),
            (Language::Zh, ToolKind::Command) => format!("运行了 {n} 个命令"),
            (Language::Zh, ToolKind::Read) => format!("读取了 {n} 个文件"),
            (Language::Zh, ToolKind::Edit) => format!("编辑了 {n} 个文件"),
            (Language::Zh, ToolKind::Search) => format!("搜索了 {n} 次"),
            (Language::Zh, ToolKind::Other) => format!("调用了 {n} 个工具"),
        }
    };
    let parts: Vec<String> = counts.into_iter().map(|(k, n)| phrase(k, n)).collect();
    let mut line = parts.join(match lang {
        Language::En => ", ",
        Language::Ja => "、",
        Language::Zh => "，",
    });
    if lang == Language::En
        && let Some(first) = line.get(..1)
    {
        line = format!("{}{}", first.to_uppercase(), &line[1..]);
    }
    let failed = calls
        .iter()
        .filter(|c| matches!(c.status, ToolStatus::Failed | ToolStatus::Interrupted))
        .count();
    if failed > 0 {
        line.push_str(&match lang {
            Language::En => format!(" · {failed} failed"),
            Language::Ja => format!(" · {failed} 件失敗"),
            Language::Zh => format!(" · {failed} 个未成功"),
        });
    }
    line
}

/// Consecutive tool calls as one quiet, collapsible line (Claude's desktop):
/// while they run it names the current call; once done it sums them up.
/// Opened, it lists the calls, each opening to its arguments and output.
fn tools_row(item: ItemMemo) -> impl IntoElement {
    let colors = palette();
    let lang = use_context::<Language>().unwrap_or_default();
    let calls = computed(move || match item.get() {
        Some(TimelineItem::Tools { calls, .. }) => calls,
        _ => Vec::new(),
    });
    let open = signal(false);
    let hovered = signal(false);
    let single = computed(move || calls.with(|c| c.len() == 1));
    let running = computed(move || calls.with(|c| c.iter().any(|t| live(t.status))));
    let awaiting = computed(move || {
        calls.with(|c| c.iter().any(|t| t.status == ToolStatus::AwaitingApproval))
    });
    // The call the line names: the running one, else the last.
    let current = computed(move || {
        calls.with(|c| {
            c.iter()
                .rev()
                .find(|t| live(t.status))
                .or(c.last())
                .map(|t| (t.name.clone(), t.summary.clone()))
                .unwrap_or_default()
        })
    });
    let headline = computed(move || calls.with(|c| tools_headline(c, lang)));
    let ink = move || if hovered.get() || open.get() { colors.ink } else { colors.muted };
    // A single call or a running group names its (current) call; a finished
    // group sums itself up.
    let named = move || single.get() || running.get();
    let primary = move || {
        if !named() {
            return headline.get();
        }
        let name = current.with(|(name, _)| name.clone());
        if single.get() {
            name
        } else {
            let word = match lang {
                Language::En => "Running",
                Language::Ja => "実行中",
                Language::Zh => "正在运行",
            };
            format!("{word} · {name}")
        }
    };
    let secondary = move || {
        if named() {
            current.with(|(_, summary)| summary.clone())
        } else {
            String::new()
        }
    };
    // The line is as wide as its text (its chevron right after it) and
    // shrinks inside the row, so a long command ends in an ellipsis.
    let line = nsh_base::button()
        .accessibility_label(match lang {
            Language::En => "Show tool calls",
            Language::Ja => "ツール呼び出しを表示",
            Language::Zh => "展开工具调用",
        })
        .on_press(move || open.set(!open.get_untracked()))
        .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
        .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
        .flex_shrink(1.0)
        .min_w(px(0.0))
        .flex_row()
        .items_center()
        .gap_2()
        .py_0p5()
        .child(dynamic(move || {
            if awaiting.get() {
                dot(colors.warning).into_element()
            } else if running.get() {
                dot(colors.accent).animate_pulse().into_element()
            } else {
                let glyph = current.with(|(name, _)| tool_glyph(name));
                icon(glyph, 13.0, colors.faint).into_element()
            }
        }))
        // Plain texts (no `dynamic` box around them), so a long
        // command shrinks with an ellipsis inside the column.
        .child(
            text(primary)
                .flex_shrink_0()
                .text_size(px(12.5))
                .text_color(ink),
        )
        .child(
            text(secondary)
                .flex_shrink(1.0)
                .min_w(px(0.0))
                .number_of_lines(1u32)
                .font_family("Menlo")
                .text_size(px(11.5))
                .text_color(colors.faint),
        )
        .child(dynamic(move || {
            icon(
                if open.get() { Icon::ChevronDown } else { Icon::ChevronRight },
                12.0,
                colors.faint,
            )
            .into_element()
        }));
    view()
        .flex_col()
        .gap_1()
        .child(view().w_full().min_w(px(0.0)).flex_row().child(line))
        .child(when(
            move || open.get(),
            move || dynamic(move || {
                if single.get() {
                    let entry = computed(move || calls.with(|c| c.first().cloned()));
                    tool_details(entry).into_element()
                } else {
                    view()
                        .flex_col()
                        .gap_0p5()
                        .ml(px(6.0))
                        .pl_3()
                        .border_l(px(1.5))
                        .border_l_color(colors.line)
                        .child(each(
                            move || calls.get(),
                            |call: &ToolEntry| call.id.clone(),
                            move |call, _| tool_entry_row(call, lang),
                        ))
                        .into_element()
                }
            }),
        ))
}

/// One call of an opened group: a line that opens to its details.
fn tool_entry_row(call: ReadSignal<ToolEntry>, lang: Language) -> impl IntoElement {
    let colors = palette();
    let open = signal(false);
    let status = move || call.with(|c| c.status);
    view()
        .flex_col()
        .gap_1()
        .child(
            nsh_base::button()
                .accessibility_label(call.with_untracked(|c| c.name.clone()))
                .on_press(move || open.set(!open.get_untracked()))
                .flex_row()
                .items_center()
                .gap_1p5()
                .py_0p5()
                .min_w(px(0.0))
                .rounded(px(6.0))
                .hover(|s| s.bg(colors.soft))
                .child(dynamic(move || {
                    if live(status()) {
                        dot(move || tool_status_color(status())).into_element()
                    } else {
                        icon(call.with(|c| tool_glyph(&c.name)), 12.0, colors.faint).into_element()
                    }
                }))
                .child(
                    text(move || call.with(|c| c.name.clone()))
                        .flex_shrink_0()
                        .font_weight(FontWeight::MEDIUM)
                        .text_size(px(12.0))
                        .text_color(colors.muted),
                )
                .child(
                    text(move || call.with(|c| c.summary.clone()))
                        .flex_1()
                        .min_w(px(0.0))
                        .number_of_lines(1u32)
                        .font_family("Menlo")
                        .text_size(px(11.5))
                        .text_color(colors.faint),
                )
                .child(when(
                    move || status() != ToolStatus::Completed,
                    move || {
                        text(move || tool_status_label(status(), lang))
                            .flex_shrink_0()
                            .text_size(px(11.0))
                            .text_color(move || tool_status_color(status()))
                    },
                ))
                .child(dynamic(move || {
                    icon(
                        if open.get() { Icon::ChevronDown } else { Icon::ChevronRight },
                        11.0,
                        colors.faint,
                    )
                    .into_element()
                })),
        )
        .child(when(
            move || open.get(),
            move || {
                let entry = computed(move || Some(call.get()));
                tool_details(entry)
            },
        ))
}

/// A call's arguments and output, in a quiet code box.
fn tool_details(entry: Memo<Option<ToolEntry>>) -> impl IntoElement {
    let colors = palette();
    let arguments = move || entry.with(|e| e.as_ref().map(|e| e.arguments.clone()).unwrap_or_default());
    let output = move || entry.with(|e| e.as_ref().map(|e| e.output.clone()).unwrap_or_default());
    view()
        .flex_col()
        .rounded(px(8.0))
        .bg(colors.code)
        .border_1()
        .border_color(colors.line_soft)
        .overflow_hidden()
        .child(when(
            move || !arguments().is_empty(),
            move || {
                view().px_3().py_2().child(
                    text(arguments)
                        .select_text()
                        .font_family("Menlo")
                        .text_size(px(11.5))
                        .line_height(LineHeight::Multiple(1.45))
                        .text_color(colors.muted),
                )
            },
        ))
        .child(when(
            move || !output().is_empty(),
            move || {
                view()
                    .px_3()
                    .py_2()
                    .border_t_1()
                    .border_t_color(colors.line_soft)
                    .child(
                        text(output)
                            .select_text()
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
