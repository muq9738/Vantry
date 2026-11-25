//! Markdown blocks as native views: headings, paragraphs with inline spans
//! (strong, emphasis, strike, code, links), lists with their markers and
//! task boxes, fenced code with its language label and a copy button, diff
//! blocks with added / removed / hunk lines colored, quotes, rules, tables.
//!
//! A document is a keyed list of blocks (keyed by where they start), so a
//! streamed reply re-renders only the block that grew.

use std::sync::Arc;

use nsh_core::{LineHeight, prelude::*};

use crate::{
    controller,
    state::markdown::{Block, DiffKind, Inline, ListItem, MarkdownDoc, Spanned},
    theme::{metrics, palette},
};

const MONO: &str = "Menlo";

/// The blocks of `doc` (a memo of the document).
pub(crate) fn document(doc: Memo<Option<Arc<MarkdownDoc>>>) -> impl IntoElement {
    let blocks = computed(move || doc.with(|d| d.as_ref().map(|d| d.blocks.clone()).unwrap_or_default()));
    view()
        .w_full()
        .min_w(px(0.0))
        .flex_col()
        .gap_2()
        .child(each(
            move || blocks.get(),
            |b: &Spanned| b.start,
            |block, _| {
                let shown = computed(move || block.with(|b| b.block.clone()));
                dynamic(move || block_view(&shown.get()).into_element())
            },
        ))
}

/// A document that does not change (previews, settings notes).
pub(crate) fn static_document(doc: &MarkdownDoc) -> impl IntoElement {
    let mut column = view().w_full().min_w(px(0.0)).flex_col().gap_2();
    for spanned in &doc.blocks {
        column = column.child(block_view(&spanned.block));
    }
    column
}

fn block_view(block: &Block) -> AnyElement {
    let colors = palette();
    match block {
        Block::Heading { level, inlines } => {
            let size = match level {
                1 => 20.0,
                2 => 17.5,
                3 => 15.5,
                _ => 14.5,
            };
            inline_text(inlines)
                .text_size(px(size))
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(colors.ink)
                .mt_1()
                .into_element()
        }
        Block::Paragraph(inlines) => body(inline_text(inlines)).into_element(),
        Block::List { items, .. } => list_view(items).into_element(),
        Block::Code { lang, code, closed } => {
            if let Some(el) = super::visuals::render_fenced(lang, code, *closed) {
                el
            } else {
                code_view(lang, code, *closed).into_element()
            }
        }
        Block::Diff { lines, .. } => {
            let source: String = lines
                .iter()
                .map(|l| l.text.as_str())
                .collect::<Vec<_>>()
                .join("\n");
            let mut column = view().flex_col().py_2();
            for line in lines {
                let (fill, ink) = diff_colors(line.kind);
                column = column.child(
                    text(if line.text.is_empty() { " ".to_owned() } else { line.text.clone() })
                        .select_text()
                        .font_family(MONO)
                        .text_size(px(12.0))
                        .line_height(LineHeight::Multiple(1.45))
                        .text_color(ink)
                        .bg(fill)
                        .px_3(),
                );
            }
            code_frame("diff", source, column).into_element()
        }
        Block::Quote(lines) => {
            let mut column = view().flex_col().gap_1();
            for line in lines {
                column = column.child(
                    inline_text(line)
                        .select_text()
                        .text_size(px(metrics::BODY))
                        .line_height(LineHeight::Multiple(1.6))
                        .text_color(colors.muted),
                );
            }
            view()
                .pl_3()
                .border_l(px(3.0))
                .border_l_color(colors.line)
                .child(column)
                .into_element()
        }
        Block::Rule => view()
            .h(px(1.0))
            .my_2()
            .bg(colors.line)
            .into_element(),
        Block::Table { header, rows } => table_view(header, rows).into_element(),
    }
}

fn body(text: Text) -> Text {
    let colors = palette();
    text.select_text()
        .text_size(px(metrics::BODY + 0.5))
        .line_height(LineHeight::Multiple(1.65))
        .text_color(colors.ink)
}

/// A text block of inline spans.
pub(crate) fn inline_text(inlines: &[Inline]) -> Text {
    let colors = palette();
    let mut block = text("");
    for inline in inlines {
        block = block.child(match inline {
            Inline::Text(t) => text(t.clone()),
            Inline::Strong(t) => text(t.clone()).font_weight(FontWeight::SEMIBOLD),
            Inline::Emphasis(t) => text(t.clone()).italic(),
            Inline::Strike(t) => text(t.clone()).line_through(),
            Inline::Code(t) => text(t.clone())
                .font_family(MONO)
                .text_size(px(12.5))
                .bg(colors.code)
                .rounded(px(4.0))
                .text_color(colors.ink),
            Inline::Link { text: label, url } => {
                let href = url.clone();
                if url.starts_with("vantry-attachment://") {
                    // The reference reads in the theme's tint, as the composer draws it.
                    text(format!(" {label} "))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.info_ink)
                        .bg(colors.info_bg)
                        .rounded(px(4.0))
                        .cursor(Cursor::Pointer)
                        .tooltip(super::tip(format!("点击预览 {label}")))
                        .on_press(move || controller::open_link(href.clone()))
                } else {
                    text(label.clone())
                        .underline()
                        .text_color(if url.starts_with(vantry_protocol::THREAD_SCHEME) {
                            colors.info_ink
                        } else {
                            colors.accent
                        })
                        .cursor(Cursor::Pointer)
                        .tooltip(super::tip(url.clone()))
                        .on_press(move || controller::open_link(href.clone()))
                }
            }
        });
    }
    block
}

fn list_view(items: &[ListItem]) -> impl IntoElement {
    let colors = palette();
    let mut column = view().w_full().min_w(px(0.0)).flex_col().gap_1();
    for item in items {
        let marker = match item.checked {
            Some(true) => "☑".to_owned(),
            Some(false) => "☐".to_owned(),
            None => item.marker.clone(),
        };
        column = column.child(
            view()
                .w_full()
                .min_w(px(0.0))
                .flex_row()
                .gap_2()
                .pl(px(f32::from(item.depth) * 18.0))
                .child(
                    text(marker)
                        .min_w(px(18.0))
                        .text_size(px(metrics::BODY))
                        .line_height(LineHeight::Multiple(1.65))
                        .text_color(colors.muted),
                )
                .child(body(inline_text(&item.inlines)).flex_1().min_w(px(0.0))),
        );
    }
    column
}

fn is_prose_fence(lang: &str) -> bool {
    matches!(
        lang.trim().to_ascii_lowercase().as_str(),
        "" | "text" | "txt" | "plain" | "plaintext" | "markdown" | "md" | "prompt" | "prose"
    )
}

fn code_view(lang: &str, code: &str, closed: bool) -> impl IntoElement {
    let colors = palette();
    let prose = is_prose_fence(lang);
    let label = if lang.trim().is_empty() {
        if prose { "text" } else { "代码" }
    } else {
        lang.trim()
    };
    let wrapped = signal(prose);
    let source_text = if code.is_empty() {
        " ".to_owned()
    } else {
        code.to_owned()
    };
    let copy_source = code.to_owned();
    let frame = view()
        .w_full()
        .min_w(px(0.0))
        .flex_col()
        .rounded(px(metrics::CARD_RADIUS))
        .bg(colors.code)
        .border_1()
        .border_color(colors.line_soft)
        .overflow_hidden()
        .child(
            view()
                .w_full()
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .py_1p5()
                .bg(colors.soft)
                .border_b_1()
                .border_b_color(colors.line_soft)
                .child(
                    text(label.to_owned())
                        .flex_1()
                        .font_family(MONO)
                        .text_size(px(11.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.faint),
                )
                .child(
                    nsh_base::button()
                        .on_press(move || wrapped.set(!wrapped.get_untracked()))
                        .px_1p5()
                        .py_0p5()
                        .rounded(px(5.0))
                        .hover(|s| s.bg(colors.card))
                        .active(|s| s.opacity(0.6))
                        .child(
                            text(move || if wrapped.get() { "已折行" } else { "折行" })
                                .text_size(px(11.0))
                                .text_color(move || {
                                    if wrapped.get() {
                                        colors.accent
                                    } else {
                                        colors.faint
                                    }
                                }),
                        ),
                )
                .child(super::link_button("复制", move || {
                    controller::copy(copy_source.clone())
                })),
        )
        .child(dynamic(move || {
            let content_text = {
                let t = text(source_text.clone())
                    .select_text()
                    .text_color(colors.ink);
                if prose {
                    t.text_size(px(13.0))
                        .line_height(LineHeight::Multiple(1.62))
                } else {
                    t.font_family(MONO)
                        .text_size(px(12.5))
                        .line_height(LineHeight::Multiple(1.52))
                }
            };
            if wrapped.get() {
                view()
                    .w_full()
                    .min_w(px(0.0))
                    .px_3p5()
                    .py_2p5()
                    .child(content_text.w_full().min_w(px(0.0)))
                    .into_element()
            } else {
                scroll()
                    .w_full()
                    .min_w(px(0.0))
                    .horizontal()
                    .child(view().px_3p5().py_2p5().child(content_text))
                    .into_element()
            }
        }));
    if closed {
        frame
    } else {
        frame.opacity(0.92)
    }
}

/// A code well: its label and copy button over `content`, scrolling
/// sideways for long lines.
fn code_frame(label: &str, source: String, content: impl IntoElement) -> View {
    let colors = palette();
    view()
        .w_full()
        .min_w(px(0.0))
        .flex_col()
        .rounded(px(metrics::CARD_RADIUS))
        .bg(colors.code)
        .border_1()
        .border_color(colors.line_soft)
        .overflow_hidden()
        .child(
            view()
                .w_full()
                .flex_row()
                .items_center()
                .px_3()
                .py_1p5()
                .bg(colors.soft)
                .border_b_1()
                .border_b_color(colors.line_soft)
                .child(
                    text(label.to_owned())
                        .flex_1()
                        .font_family(MONO)
                        .text_size(px(11.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.faint),
                )
                .child(super::link_button("复制", move || controller::copy(source.clone()))),
        )
        .child(scroll().w_full().min_w(px(0.0)).horizontal().child(content))
}

fn diff_colors(kind: DiffKind) -> (AdaptiveColor, AdaptiveColor) {
    let colors = palette();
    match kind {
        DiffKind::Added => (
            AdaptiveColor::rgb(0xE6F6EA, 0x15301E),
            AdaptiveColor::rgb(0x1A6B34, 0x7EE2A0),
        ),
        DiffKind::Removed => (
            AdaptiveColor::rgb(0xFCE8E8, 0x3A1A1C),
            AdaptiveColor::rgb(0xA4262C, 0xFF9CA0),
        ),
        DiffKind::Hunk => (AdaptiveColor::TRANSPARENT, colors.accent),
        DiffKind::Meta => (AdaptiveColor::TRANSPARENT, colors.faint),
        DiffKind::Context => (AdaptiveColor::TRANSPARENT, colors.muted),
    }
}

/// A unified diff (the Git panel, previews), colored per line.
pub(crate) fn diff_text(diff: &str) -> impl IntoElement {
    let mut column = view().flex_col().py_1();
    for line in crate::state::markdown::diff_lines(diff).into_iter().take(4000) {
        let (fill, ink) = diff_colors(line.kind);
        column = column.child(
            text(if line.text.is_empty() { " ".to_owned() } else { line.text })
                .select_text()
                .font_family(MONO)
                .text_size(px(11.5))
                .line_height(LineHeight::Multiple(1.4))
                .text_color(ink)
                .bg(fill)
                .px_2(),
        );
    }
    scroll().horizontal().child(column)
}

fn table_view(header: &[Vec<Inline>], rows: &[Vec<Vec<Inline>>]) -> impl IntoElement {
    let colors = palette();
    let columns = rows
        .iter()
        .map(Vec::len)
        .max()
        .unwrap_or(0)
        .max(header.len());
    let width = signal(0u32);
    // Sample enough rows to distinguish short keys from prose without measuring every cell.
    let weights: Vec<f32> = (0..columns)
        .map(|col| {
            std::iter::once(header)
                .chain(rows.iter().take(32).map(Vec::as_slice))
                .filter_map(|row| row.get(col))
                .map(|cell| {
                    crate::state::markdown::plain(cell)
                        .chars()
                        .take(48)
                        .map(|c| if c.is_ascii() { 1 } else { 2 })
                        .sum::<usize>()
                        .clamp(10, 48)
                })
                .max()
                .unwrap_or(10) as f32
        })
        .collect();
    let total = weights.iter().sum::<f32>().max(1.0);
    let row_view = |cells: &[Vec<Inline>], heading: bool| {
        let mut row = view().flex_row().items_start();
        if heading {
            row = row.bg(colors.soft);
        } else {
            row = row.border_t_1().border_t_color(colors.line_soft);
        }
        for (i, weight) in weights.iter().enumerate() {
            let share = weight / total;
            let value = cells.get(i).map(Vec::as_slice).unwrap_or_default();
            let label = inline_text(value)
                .select_text()
                .w_full()
                .text_color(colors.ink)
                .text_size(px(if heading { 12.5 } else { 14.0 }))
                .line_height(LineHeight::Multiple(1.5));
            row = row.child(
                view()
                    .flex_shrink_0()
                    .px_3()
                    .py_2()
                    .w(move || {
                        px(76.0
                            + (width.get() as f32 - 2.0 - 76.0 * columns as f32).max(0.0) * share)
                    })
                    .child(if heading {
                        label.font_weight(FontWeight::SEMIBOLD)
                    } else {
                        label
                    }),
            );
        }
        row
    };
    let mut grid = view()
        .flex_col()
        .rounded(px(8.0))
        .border_1()
        .border_color(colors.line)
        .child(row_view(header, true));
    for row in rows {
        grid = grid.child(row_view(row, false));
    }
    scroll()
        .horizontal()
        .w_full()
        .min_w_0()
        .on_layout(move |b: &Bounds<Points>| width.set(b.size.width.0.floor() as u32))
        .child(grid)
}
