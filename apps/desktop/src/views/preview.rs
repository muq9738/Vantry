//! Unified native preview views shared by the Preview Dialog and the Files dock:
//! - Syntax-colored, line-numbered code viewer with word-wrap and target-line highlight/scroll
//! - Zoomable image & SVG viewer with dimensions, size, and copy-image support
//! - Rendered Markdown and sandboxed offline HTML preview with source toggle
//! - Unified diff viewer
//! - Metadata + quick-open card for binary files (PDF, archives, media, Office docs)

use nsh_core::{LineHeight, prelude::*};

use super::{badge, caption, ghost_button, icon_button, link_button, markdown, small_icon_button};
use crate::{
    controller,
    icons::Icon,
    state::workbench::PreviewContent,
    theme::palette,
};

const MONO: &str = "Menlo";
const CODE_FONT_SIZE: f32 = 12.0;
const CODE_LINE_HEIGHT_MULT: f32 = 1.5;
/// Approximate line height in points (`12.0 * 1.5`) used to scroll near a target line.
const CODE_LINE_PT: f32 = CODE_FONT_SIZE * CODE_LINE_HEIGHT_MULT;
/// Syntax highlighting budget: beyond this, render fast plain line-numbered text.
const MAX_HIGHLIGHT_LINES: usize = 1500;
const MAX_HIGHLIGHT_BYTES: usize = 120 * 1024;

// Adaptive syntax palette harmonized with Stardust light & dark themes.
const SYN_COMMENT: AdaptiveColor = AdaptiveColor::rgb(0x6E7781, 0x7A828E);
const SYN_STRING: AdaptiveColor = AdaptiveColor::rgb(0x1F7A3E, 0x86D99C);
const SYN_KEYWORD: AdaptiveColor = AdaptiveColor::rgb(0x9A3478, 0xE084C4);
const SYN_NUMBER: AdaptiveColor = AdaptiveColor::rgb(0xB45309, 0xF0A960);
const SYN_TYPE: AdaptiveColor = AdaptiveColor::rgb(0x1D5FB8, 0x7AB8F5);
const SYN_ATTR: AdaptiveColor = AdaptiveColor::rgb(0x7C4DFF, 0xB89CF5);
const LINE_HL_BG: AdaptiveColor = AdaptiveColor::rgb(0xF7E8DE, 0x382820);

/// Human-readable byte size (e.g. `248 KB`, `2.4 MB`).
pub(crate) fn format_bytes(bytes: u64) -> String {
    const KB: f64 = 1024.0;
    const MB: f64 = 1024.0 * 1024.0;
    if bytes == 0 {
        "0 B".into()
    } else if (bytes as f64) < KB {
        format!("{bytes} B")
    } else if (bytes as f64) < MB {
        format!("{:.1} KB", bytes as f64 / KB)
    } else {
        format!("{:.1} MB", bytes as f64 / MB)
    }
}

pub(crate) fn format_line_badge(lines: (usize, usize)) -> String {
    if lines.0 == lines.1 {
        format!("L{}", lines.0)
    } else {
        format!("L{}-L{}", lines.0, lines.1)
    }
}

/// The top-level preview modal dialog (images, code, Markdown, SVG, HTML, diffs, binary files).
pub(crate) fn preview_dialog() -> impl IntoElement {
    let open = computed(|| controller::bench().with(|b| b.preview.content.is_some()));
    dynamic(move || {
        open.get().then(|| {
            let colors = palette();
            let close = controller::close_preview;
            let title = computed(|| controller::bench().with(|b| b.preview.title.clone()));
            let file_path = computed(|| controller::bench().with(|b| b.preview.file_path.clone()));
            let highlight = computed(|| controller::bench().with(|b| b.preview.highlight_lines));
            let content = computed(|| controller::bench().with(|b| b.preview.content.clone()));
            let source = computed(|| controller::bench().with(|b| b.preview.source));
            let wrap = computed(|| controller::bench().with(|b| b.preview.wrap));
            let truncated = computed(|| controller::bench().with(|b| b.preview.truncated));
            let raw = computed(|| controller::bench().with(|b| b.preview.raw().to_owned()));

            let has_source_toggle = computed(move || {
                matches!(
                    content.get(),
                    Some(
                        PreviewContent::Markdown(_)
                            | PreviewContent::Svg { .. }
                            | PreviewContent::Html { .. }
                    )
                )
            });
            let is_code_view = computed(move || {
                matches!(content.get(), Some(PreviewContent::Code { .. }))
                    || (has_source_toggle.get() && source.get())
            });
            let image_copy_src = computed(move || match content.get() {
                Some(PreviewContent::Image { src, .. }) | Some(PreviewContent::Svg { src, .. }) => {
                    Some(src)
                }
                _ => None,
            });
            let has_text_copy = computed(move || {
                !raw.with(String::is_empty)
                    && !matches!(
                        content.get(),
                        Some(PreviewContent::Image { .. } | PreviewContent::Binary { .. })
                    )
            });
            let meta_badge = computed(move || {
                let mut parts = Vec::new();
                if let Some(lines) = highlight.get() {
                    parts.push(format_line_badge(lines));
                }
                match content.get() {
                    Some(PreviewContent::Image {
                        size, dimensions, ..
                    }) => {
                        if let Some((w, h)) = dimensions {
                            parts.push(format!("{w} × {h}"));
                        }
                        if size > 0 {
                            parts.push(format_bytes(size));
                        }
                    }
                    Some(PreviewContent::Svg { size, .. }) => {
                        parts.push("SVG".into());
                        if size > 0 {
                            parts.push(format_bytes(size));
                        }
                    }
                    Some(PreviewContent::Html { size, .. }) => {
                        parts.push("HTML".into());
                        if size > 0 {
                            parts.push(format_bytes(size));
                        }
                    }
                    Some(PreviewContent::Code { lang, .. }) if !lang.is_empty() => {
                        parts.push(lang.to_uppercase());
                    }
                    Some(PreviewContent::Diff(_)) => parts.push("DIFF".into()),
                    Some(PreviewContent::Binary { size, .. }) if size > 0 => {
                        parts.push(format_bytes(size));
                    }
                    _ => {}
                }
                parts.join(" · ")
            });

            nsh_base::dialog()
                .animation(ModalAnimation::Fade)
                .on_cancel(close)
                .child(
                    view()
                        .flex_1()
                        .items_center()
                        .justify_center()
                        .bg(colors.scrim)
                        .p_5()
                        .on_press(close)
                        .child(
                            view()
                                .w(move || {
                                    px((crate::window_size().width.0 - 80.0).clamp(420.0, 1080.0))
                                })
                                .h(move || {
                                    px((crate::window_size().height.0 - 80.0).clamp(320.0, 780.0))
                                })
                                .flex_col()
                                .rounded(px(14.0))
                                .bg(colors.canvas)
                                .border_1()
                                .border_color(colors.line)
                                .shadow_xl()
                                .overflow_hidden()
                                .on_press(|| {})
                                .child(
                                    // Top toolbar
                                    view()
                                        .w_full()
                                        .min_w_0()
                                        .flex_row()
                                        .items_center()
                                        .gap_1p5()
                                        .px_3p5()
                                        .py_2()
                                        .bg(colors.soft)
                                        .border_b_1()
                                        .border_b_color(colors.line_soft)
                                        .child(dynamic(move || {
                                            let glyph = match content.get() {
                                                Some(
                                                    PreviewContent::Image { .. }
                                                    | PreviewContent::Svg { .. },
                                                ) => Icon::Eye,
                                                Some(PreviewContent::Diff(_)) => Icon::FileDiff,
                                                Some(PreviewContent::Code { .. }) => Icon::Code,
                                                Some(PreviewContent::Binary { .. }) => Icon::File,
                                                _ => Icon::FileText,
                                            };
                                            crate::icons::icon(glyph, 14.0, colors.accent)
                                                .into_element()
                                        }))
                                        .child(
                                            view()
                                                .flex_1()
                                                .min_w_0()
                                                .tooltip(super::tip(move || {
                                                    file_path
                                                        .get()
                                                        .unwrap_or_else(|| title.get())
                                                }))
                                                .child(
                                                    text(move || {
                                                        let t = title.get();
                                                        if t.is_empty() {
                                                            "预览".to_owned()
                                                        } else {
                                                            t
                                                        }
                                                    })
                                                    .number_of_lines(1u32)
                                                    .font_weight(FontWeight::SEMIBOLD)
                                                    .text_size(px(13.0))
                                                    .text_color(colors.ink),
                                                ),
                                        )
                                        .child(when(
                                            move || !meta_badge.with(String::is_empty),
                                            move || {
                                                text(move || meta_badge.get())
                                                    .font_family(MONO)
                                                    .text_size(px(11.0))
                                                    .text_color(colors.muted)
                                                    .px_1p5()
                                                    .py_0p5()
                                                    .rounded(px(5.0))
                                                    .bg(colors.card)
                                            },
                                        ))
                                        .child(when(
                                            move || is_code_view.get(),
                                            move || {
                                                link_button(
                                                    move || {
                                                        if wrap.get() {
                                                            "已折行"
                                                        } else {
                                                            "折行"
                                                        }
                                                    },
                                                    controller::toggle_preview_wrap,
                                                )
                                            },
                                        ))
                                        .child(when(
                                            move || has_source_toggle.get(),
                                            move || {
                                                link_button(
                                                    move || {
                                                        if source.get() {
                                                            "渲染预览"
                                                        } else {
                                                            "查看源码"
                                                        }
                                                    },
                                                    controller::toggle_preview_source,
                                                )
                                            },
                                        ))
                                        .child(when(
                                            move || image_copy_src.with(Option::is_some),
                                            move || {
                                                link_button("复制图片", move || {
                                                    if let Some(src) =
                                                        image_copy_src.get_untracked()
                                                    {
                                                        controller::copy_image_preview(
                                                            src,
                                                            title.get_untracked(),
                                                        );
                                                    }
                                                })
                                            },
                                        ))
                                        .child(when(
                                            move || has_text_copy.get(),
                                            move || {
                                                link_button("复制", move || {
                                                    controller::copy(raw.get_untracked())
                                                })
                                            },
                                        ))
                                        .child(when(
                                            move || file_path.with(Option::is_some),
                                            move || {
                                                view()
                                                    .flex_row()
                                                    .items_center()
                                                    .gap_0p5()
                                                    .child(small_icon_button(
                                                        Icon::Copy,
                                                        "复制路径",
                                                        move || {
                                                            if let Some(p) =
                                                                file_path.get_untracked()
                                                            {
                                                                controller::copy(p);
                                                            }
                                                        },
                                                    ))
                                                    .child(small_icon_button(
                                                        Icon::FolderOpen,
                                                        "在访达中显示",
                                                        move || {
                                                            if let Some(p) =
                                                                file_path.get_untracked()
                                                            {
                                                                controller::reveal_local_path(p);
                                                            }
                                                        },
                                                    ))
                                                    .child(small_icon_button(
                                                        Icon::ExternalLink,
                                                        "使用默认应用打开",
                                                        move || {
                                                            if let Some(p) =
                                                                file_path.get_untracked()
                                                            {
                                                                controller::open_local_path(p);
                                                            }
                                                        },
                                                    ))
                                            },
                                        ))
                                        .child(icon_button(Icon::X, "关闭预览", close)),
                                )
                                .child(
                                    view().flex_1().min_w_0().min_h_0().flex_col().child(dynamic(
                                        move || {
                                            render_preview_body(
                                                content.get(),
                                                source.get(),
                                                wrap.get(),
                                                truncated.get(),
                                                title.get(),
                                                file_path.get(),
                                                highlight.get(),
                                                true,
                                            )
                                        },
                                    )),
                                ),
                        ),
                )
                .into_element()
        })
    })
}

/// Renders the body for any [`PreviewContent`] variant (shared by modal & dock).
#[allow(clippy::too_many_arguments)]
pub(crate) fn render_preview_body(
    content: Option<PreviewContent>,
    show_source: bool,
    wrap: bool,
    truncated: bool,
    title: String,
    file_path: Option<String>,
    highlight_lines: Option<(usize, usize)>,
    is_modal: bool,
) -> AnyElement {
    let colors = palette();
    match (content, show_source) {
        (None, _) => view()
            .flex_1()
            .items_center()
            .justify_center()
            .p_6()
            .child(caption("选择左侧文件以阅读内容"))
            .into_element(),
        (Some(PreviewContent::Loading), _) => view()
            .flex_1()
            .items_center()
            .justify_center()
            .p_6()
            .child(caption("正在加载预览…"))
            .into_element(),
        (Some(PreviewContent::Error(err)), _) => view()
            .flex_1()
            .items_center()
            .justify_center()
            .flex_col()
            .gap_3()
            .p_6()
            .child(crate::icons::icon(Icon::TriangleAlert, 24.0, colors.danger))
            .child(
                text(err)
                    .text_size(px(13.0))
                    .text_color(colors.danger)
                    .text_center(),
            )
            .child(file_path.map(|p| {
                let p_open = p.clone();
                view()
                    .flex_row()
                    .gap_2()
                    .child(ghost_button("使用默认应用打开", move || {
                        controller::open_local_path(p_open.clone())
                    }))
                    .child(ghost_button("在访达中显示", move || {
                        controller::reveal_local_path(p.clone())
                    }))
            }))
            .into_element(),
        (Some(PreviewContent::Image { src, .. }), _) => {
            image_viewer(src, is_modal).into_element()
        }
        (Some(PreviewContent::Svg { src, .. }), false) => {
            image_viewer(src, is_modal).into_element()
        }
        (Some(PreviewContent::Svg { xml, .. }), true) => {
            code_viewer(xml, "xml".into(), wrap, truncated, highlight_lines).into_element()
        }
        (Some(PreviewContent::Html { source, .. }), false) => {
            html_viewer(title, source).into_element()
        }
        (Some(PreviewContent::Html { source, .. }), true) => {
            code_viewer(source, "html".into(), wrap, truncated, highlight_lines).into_element()
        }
        (Some(PreviewContent::Markdown(doc)), false) => scroll()
            .flex_1()
            .min_w_0()
            .min_h_0()
            .child(view().w_full().min_w_0().px_5().py_4().child(markdown::static_document(&doc)))
            .into_element(),
        (Some(PreviewContent::Markdown(_)), true) => {
            let raw = controller::bench().with_untracked(|b| b.preview.raw().to_owned());
            code_viewer(raw, "md".into(), wrap, truncated, highlight_lines).into_element()
        }
        (Some(PreviewContent::Code { lang, text }), _) => {
            code_viewer(text, lang, wrap, truncated, highlight_lines).into_element()
        }
        (Some(PreviewContent::Diff(diff)), _) => scroll()
            .flex_1()
            .min_w_0()
            .min_h_0()
            .child(view().p_3().child(markdown::diff_text(&diff)))
            .into_element(),
        (Some(PreviewContent::Binary { mime, size }), _) => {
            binary_file_card(title, file_path, mime, size).into_element()
        }
    }
}

/// Interactive image viewer with Fit / Zoom controls (`50%` .. `300%`).
pub(crate) fn image_viewer(src: String, is_modal: bool) -> impl IntoElement {
    let colors = palette();
    // `None` = fit to viewport (`ResizeMode::Contain`), `Some(scale)` = explicit zoom multiplier.
    let zoom = signal(None::<f32>);
    let viewport = signal(Size::new(px(640.0), px(460.0)));
    let zoom_label = computed(move || match zoom.get() {
        None => "适应".to_owned(),
        Some(z) => format!("{:.0}%", z * 100.0),
    });

    view()
        .flex_1()
        .min_w_0()
        .min_h_0()
        .flex_col()
        .child(
            view()
                .flex_1()
                .min_w_0()
                .min_h_0()
                .bg(colors.code)
                .on_layout(move |b: &Bounds<Points>| {
                    if b.size.width.0 > 0.0 && b.size.height.0 > 0.0 {
                        viewport.set(b.size);
                    }
                })
                .child(dynamic(move || match zoom.get() {
                    None => view()
                        .flex_1()
                        .w_full()
                        .h_full()
                        .items_center()
                        .justify_center()
                        .p_3()
                        .child(
                            image(src.clone())
                                .w(move || px((viewport.get().width.0 - 24.0).max(120.0)))
                                .h(move || {
                                    let max_h = if is_modal {
                                        viewport.get().height.0 - 24.0
                                    } else {
                                        (viewport.get().height.0 - 24.0).max(200.0)
                                    };
                                    px(max_h.max(120.0))
                                })
                                .object_fit(nsh_core::ResizeMode::Contain),
                        )
                        .into_element(),
                    Some(scale) => {
                        let base_w = (viewport.get().width.0 - 24.0).max(240.0);
                        let base_h = (viewport.get().height.0 - 24.0).max(200.0);
                        scroll()
                            .flex_1()
                            .min_w_0()
                            .min_h_0()
                            .child(
                                scroll().horizontal().child(
                                    view()
                                        .p_4()
                                        .items_center()
                                        .justify_center()
                                        .child(
                                            image(src.clone())
                                                .w(px(base_w * scale))
                                                .h(px(base_h * scale))
                                                .object_fit(nsh_core::ResizeMode::Contain),
                                        ),
                                ),
                            )
                            .into_element()
                    }
                })),
        )
        .child(
            view()
                .flex_row()
                .items_center()
                .justify_center()
                .gap_1p5()
                .px_3()
                .py_1p5()
                .bg(colors.soft)
                .border_t_1()
                .border_t_color(colors.line_soft)
                .child(small_icon_button(Icon::Minus, "缩小", move || {
                    let cur = zoom.get_untracked().unwrap_or(1.0);
                    zoom.set(Some((cur - 0.25).max(0.5)));
                }))
                .child(
                    nsh_base::button()
                        .accessibility_label("适应窗口")
                        .on_press(move || zoom.set(None))
                        .px_2()
                        .py_0p5()
                        .rounded(px(5.0))
                        .hover(|s| s.bg(colors.card))
                        .child(
                            text(move || zoom_label.get())
                                .font_family(MONO)
                                .text_size(px(11.0))
                                .text_color(colors.muted),
                        ),
                )
                .child(small_icon_button(Icon::Plus, "放大", move || {
                    let cur = zoom.get_untracked().unwrap_or(1.0);
                    zoom.set(Some((cur + 0.25).min(3.0)));
                })),
        )
}

/// Offline sandboxed HTML renderer using the same CSP-hardened iframe as artifact cards.
pub(crate) fn html_viewer(title: String, source: String) -> impl IntoElement {
    let doc = computed(move || {
        super::visuals::artifact_document(
            "html",
            &title,
            &source,
            1.0,
            nsh_core::element::dark(),
        )
    });
    nsh_core::element::web_view_html(move || doc.get(), None)
        .incognito(true)
        .flex_1()
        .w_full()
        .min_h(px(260.0))
}

/// Clean metadata + quick-open card for PDFs, archives, executables, and other binary files.
pub(crate) fn binary_file_card(
    title: String,
    file_path: Option<String>,
    mime: String,
    size: u64,
) -> impl IntoElement {
    let colors = palette();
    let ext = crate::bridge::workbench::ext_lower(&title);
    let kind_badge = if ext.is_empty() {
        "BINARY".to_owned()
    } else {
        ext.to_uppercase()
    };
    let name = std::path::Path::new(&title)
        .file_name()
        .map(|s| s.to_string_lossy().into_owned())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| title.clone());
    let detail = {
        let mut parts = Vec::new();
        if !mime.is_empty() {
            parts.push(mime);
        }
        if size > 0 {
            parts.push(format_bytes(size));
        }
        parts.join(" · ")
    };

    view()
        .flex_1()
        .min_w_0()
        .min_h_0()
        .items_center()
        .justify_center()
        .p_6()
        .child(
            view()
                .max_w(px(440.0))
                .w_full()
                .flex_col()
                .items_center()
                .gap_3()
                .p_6()
                .rounded(px(12.0))
                .bg(colors.soft)
                .border_1()
                .border_color(colors.line_soft)
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(crate::icons::icon(Icon::File, 22.0, colors.accent))
                        .child(badge(kind_badge, colors.accent)),
                )
                .child(
                    text(name)
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_size(px(14.5))
                        .text_color(colors.ink)
                        .text_center(),
                )
                .child(when(
                    {
                        let empty = detail.is_empty();
                        move || !empty
                    },
                    move || {
                        text(detail.clone())
                            .font_family(MONO)
                            .text_size(px(11.5))
                            .text_color(colors.muted)
                    },
                ))
                .child(caption(if file_path.is_some() {
                    "此文件类型暂不支持内嵌渲染，可用系统默认应用打开。"
                } else {
                    "此远端二进制文件暂不支持内嵌渲染。"
                }))
                .child(file_path.map(|path| {
                    let open_p = path.clone();
                    let reveal_p = path.clone();
                    let copy_p = path;
                    view()
                        .flex_row()
                        .flex_wrap()
                        .justify_center()
                        .gap_2()
                        .pt_1()
                        .child(super::primary_button("使用默认应用打开", move || {
                            controller::open_local_path(open_p.clone())
                        }))
                        .child(ghost_button("在访达中显示", move || {
                            controller::reveal_local_path(reveal_p.clone())
                        }))
                        .child(ghost_button("复制路径", move || {
                            controller::copy(copy_p.clone())
                        }))
                })),
        )
}

/// Syntax-highlighted, line-numbered code viewer with optional target line range highlight
/// and automatic scroll-to-line.
pub(crate) fn code_viewer(
    source: String,
    lang: String,
    wrap: bool,
    truncated: bool,
    highlight_lines: Option<(usize, usize)>,
) -> impl IntoElement {
    let colors = palette();
    let scroller = element_ref();

    if let Some((start, _)) = highlight_lines
        && start > 1
    {
        let scroller_ref = scroller.clone();
        let target_y = (start.saturating_sub(4) as f32) * CODE_LINE_PT;
        spawn(async move {
            timer(std::time::Duration::from_millis(45)).await;
            scroller_ref.scroll_to(point(px(0.0), px(target_y)), false);
        });
    }

    let content = build_code_text(&source, &lang, highlight_lines, truncated);
    let inner = if wrap {
        view()
            .w_full()
            .min_w_0()
            .px_3p5()
            .py_2p5()
            .child(content.w_full().min_w_0())
            .into_element()
    } else {
        scroll()
            .w_full()
            .min_w_0()
            .horizontal()
            .child(view().px_3p5().py_2p5().child(content))
            .into_element()
    };

    scroll()
        .element_ref(&scroller)
        .flex_1()
        .min_w_0()
        .min_h_0()
        .bg(colors.code)
        .child(inner)
}

/// Builds a single selectable `Text` node with right-aligned line numbers,
/// optional highlight band on target lines, and lightweight syntax coloring.
fn build_code_text(
    source: &str,
    lang: &str,
    highlight_lines: Option<(usize, usize)>,
    truncated: bool,
) -> Text {
    let colors = palette();
    let lines: Vec<&str> = source.lines().collect();
    let total = lines.len().max(1);
    let gutter_w = total.to_string().len().max(2);

    let mut block = text("")
        .select_text()
        .font_family(MONO)
        .text_size(px(CODE_FONT_SIZE))
        .line_height(LineHeight::Multiple(CODE_LINE_HEIGHT_MULT))
        .text_color(colors.ink);

    let do_syntax =
        lines.len() <= MAX_HIGHLIGHT_LINES && source.len() <= MAX_HIGHLIGHT_BYTES;
    let syntax = if do_syntax {
        SyntaxKind::from_lang(lang)
    } else {
        SyntaxKind::Plain
    };

    if !do_syntax && highlight_lines.is_none() {
        let mut plain = String::with_capacity(source.len() + lines.len() * (gutter_w + 3));
        for (idx, line) in lines.iter().enumerate() {
            if idx > 0 {
                plain.push('\n');
            }
            use std::fmt::Write;
            let _ = write!(&mut plain, "{:>width$}  {line}", idx + 1, width = gutter_w);
        }
        if truncated {
            plain.push_str("\n…（文件过大，已截断显示前 512 KB）");
        }
        return block.child(plain);
    }

    let mut in_block_comment = false;
    for (idx, line) in lines.iter().enumerate() {
        let line_no = idx + 1;
        if idx > 0 {
            block = block.child("\n");
        }
        let highlighted =
            highlight_lines.is_some_and(|(s, e)| line_no >= s && line_no <= e);
        let marker = if highlighted { "▸" } else { " " };
        let gutter = format!("{marker}{line_no:>gutter_w$}  ");
        let gutter_el = text(gutter).text_color(if highlighted {
            colors.accent
        } else {
            colors.faint
        });
        block = block.child(if highlighted {
            gutter_el.font_weight(FontWeight::SEMIBOLD).bg(LINE_HL_BG)
        } else {
            gutter_el
        });

        if syntax == SyntaxKind::Plain {
            let body = if line.is_empty() { " " } else { *line };
            let span = text(body.to_owned()).text_color(colors.ink);
            block = block.child(if highlighted {
                span.bg(LINE_HL_BG)
            } else {
                span
            });
        } else {
            let tokens = tokenize_line(line, syntax, &mut in_block_comment);
            if tokens.is_empty() && highlighted {
                block = block.child(text(" ").bg(LINE_HL_BG));
            } else {
                for (tok_kind, slice) in tokens {
                    let color = match tok_kind {
                        Tok::Plain => colors.ink,
                        Tok::Comment => SYN_COMMENT,
                        Tok::String => SYN_STRING,
                        Tok::Keyword => SYN_KEYWORD,
                        Tok::Number => SYN_NUMBER,
                        Tok::Type => SYN_TYPE,
                        Tok::Attr => SYN_ATTR,
                    };
                    let mut span = text(slice.to_owned()).text_color(color);
                    if tok_kind == Tok::Comment {
                        span = span.italic();
                    }
                    if highlighted {
                        span = span.bg(LINE_HL_BG);
                    }
                    block = block.child(span);
                }
            }
        }
    }

    if truncated {
        block = block.child(
            text("\n…（文件过大，已截断显示前 512 KB）").text_color(colors.faint),
        );
    }
    block
}

// --------------------------------------------------------- lightweight syntax

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum SyntaxKind {
    Plain,
    CStyle, // Rust, TS/JS, Go, Java, Swift, C/C++, C#, CSS
    Hash,   // Python, Shell, YAML, TOML, Ruby
    Json,
    Markup, // HTML, XML, SVG, Vue, Svelte
    Sql,
    Markdown,
}

impl SyntaxKind {
    fn from_lang(lang: &str) -> Self {
        match lang.trim().to_ascii_lowercase().as_str() {
            "rs" | "rust" | "ts" | "tsx" | "js" | "jsx" | "mjs" | "cjs" | "go" | "java"
            | "kt" | "kts" | "swift" | "c" | "cc" | "cpp" | "cxx" | "h" | "hpp" | "cs"
            | "php" | "css" | "scss" | "less" | "dart" | "zig" => Self::CStyle,
            "py" | "python" | "sh" | "bash" | "zsh" | "fish" | "toml" | "yaml" | "yml"
            | "rb" | "ruby" | "dockerfile" | "makefile" | "ini" | "conf" | "env" => Self::Hash,
            "json" | "jsonc" | "json5" => Self::Json,
            "html" | "htm" | "xml" | "svg" | "vue" | "svelte" => Self::Markup,
            "sql" => Self::Sql,
            "md" | "markdown" | "mdx" => Self::Markdown,
            _ => Self::Plain,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Tok {
    Plain,
    Comment,
    String,
    Keyword,
    Number,
    Type,
    Attr,
}

fn tokenize_line<'a>(
    line: &'a str,
    kind: SyntaxKind,
    in_block_comment: &mut bool,
) -> Vec<(Tok, &'a str)> {
    let mut out: Vec<(Tok, &'a str)> = Vec::new();
    let bytes = line.as_bytes();
    let len = bytes.len();
    let mut i = 0;

    let push = |out: &mut Vec<(Tok, &'a str)>, tok: Tok, s: &'a str| {
        if s.is_empty() {
            return;
        }
        // Coalesce adjacent plain spans to keep span count small.
        if let Some((last_tok, last_s)) = out.last_mut()
            && *last_tok == tok
        {
            let start = last_s.as_ptr() as usize - line.as_ptr() as usize;
            let end = s.as_ptr() as usize - line.as_ptr() as usize + s.len();
            *last_s = &line[start..end];
            return;
        }
        out.push((tok, s));
    };

    if kind == SyntaxKind::Markdown {
        let trimmed = line.trim_start();
        if trimmed.starts_with('#') {
            push(&mut out, Tok::Keyword, line);
            return out;
        }
        if trimmed.starts_with("```") || trimmed.starts_with("~~~") {
            push(&mut out, Tok::Attr, line);
            return out;
        }
        if trimmed.starts_with('>') {
            push(&mut out, Tok::Comment, line);
            return out;
        }
        push(&mut out, Tok::Plain, line);
        return out;
    }

    while i < len {
        // Ongoing block comment (`/* ... */` or `<!-- ... -->`).
        if *in_block_comment {
            let close = if kind == SyntaxKind::Markup { "-->" } else { "*/" };
            if let Some(rel) = line[i..].find(close) {
                let end = i + rel + close.len();
                push(&mut out, Tok::Comment, &line[i..end]);
                *in_block_comment = false;
                i = end;
            } else {
                push(&mut out, Tok::Comment, &line[i..]);
                return out;
            }
            continue;
        }

        let b = bytes[i];

        // Line & block comments.
        if kind == SyntaxKind::CStyle && line[i..].starts_with("//") {
            push(&mut out, Tok::Comment, &line[i..]);
            return out;
        }
        if kind == SyntaxKind::CStyle && line[i..].starts_with("/*") {
            *in_block_comment = true;
            if let Some(rel) = line[i + 2..].find("*/") {
                let end = i + 2 + rel + 2;
                push(&mut out, Tok::Comment, &line[i..end]);
                *in_block_comment = false;
                i = end;
            } else {
                push(&mut out, Tok::Comment, &line[i..]);
                return out;
            }
            continue;
        }
        if kind == SyntaxKind::Markup && line[i..].starts_with("<!--") {
            *in_block_comment = true;
            if let Some(rel) = line[i + 4..].find("-->") {
                let end = i + 4 + rel + 3;
                push(&mut out, Tok::Comment, &line[i..end]);
                *in_block_comment = false;
                i = end;
            } else {
                push(&mut out, Tok::Comment, &line[i..]);
                return out;
            }
            continue;
        }
        if kind == SyntaxKind::Hash && b == b'#' {
            push(&mut out, Tok::Comment, &line[i..]);
            return out;
        }
        if kind == SyntaxKind::Sql && line[i..].starts_with("--") {
            push(&mut out, Tok::Comment, &line[i..]);
            return out;
        }

        // Rust attributes `#[...]` / decorators `@foo`.
        if kind == SyntaxKind::CStyle && (line[i..].starts_with("#[") || line[i..].starts_with("#![")) {
            let end = line[i..].find(']').map(|r| i + r + 1).unwrap_or(len);
            push(&mut out, Tok::Attr, &line[i..end]);
            i = end;
            continue;
        }
        if matches!(kind, SyntaxKind::CStyle | SyntaxKind::Hash) && b == b'@' && i + 1 < len && bytes[i + 1].is_ascii_alphabetic() {
            let mut end = i + 1;
            while end < len && (bytes[end].is_ascii_alphanumeric() || matches!(bytes[end], b'_' | b'.')) {
                end += 1;
            }
            push(&mut out, Tok::Attr, &line[i..end]);
            i = end;
            continue;
        }

        // Quoted strings (`"`, `'`, `` ` ``). Avoid highlighting Rust lifetimes (`'a`).
        if matches!(b, b'"' | b'\'' | b'`') {
            if b == b'\'' && kind == SyntaxKind::CStyle {
                // Distinguish `'a` lifetime vs `'x'` char literal.
                let is_char_lit = i + 2 < len
                    && ((bytes[i + 1] == b'\\' && i + 3 < len && bytes[i + 3] == b'\'')
                        || (bytes[i + 1] != b'\'' && bytes[i + 2] == b'\''));
                let has_closing_quote = line[i + 1..].find('\'').is_some();
                if !is_char_lit && !has_closing_quote {
                    let mut end = i + 1;
                    while end < len && (bytes[end].is_ascii_alphanumeric() || bytes[end] == b'_') {
                        end += 1;
                    }
                    push(&mut out, Tok::Attr, &line[i..end]);
                    i = end;
                    continue;
                }
            }
            let quote = b;
            let mut end = i + 1;
            let mut escaped = false;
            while end < len {
                let c = bytes[end];
                if escaped {
                    escaped = false;
                } else if c == b'\\' {
                    escaped = true;
                } else if c == quote {
                    end += 1;
                    break;
                }
                end += 1;
            }
            // In JSON, distinguish object keys `"key":` from string values.
            let is_json_key = kind == SyntaxKind::Json
                && line[end..].trim_start().starts_with(':');
            push(
                &mut out,
                if is_json_key { Tok::Type } else { Tok::String },
                &line[i..end],
            );
            i = end;
            continue;
        }

        // Markup tag names `<div`, `</div`.
        if kind == SyntaxKind::Markup && b == b'<' {
            let mut end = i + 1;
            if end < len && bytes[end] == b'/' {
                end += 1;
            }
            let name_start = end;
            while end < len && (bytes[end].is_ascii_alphanumeric() || matches!(bytes[end], b'-' | b'_' | b':')) {
                end += 1;
            }
            if end > name_start {
                push(&mut out, Tok::Plain, &line[i..name_start]);
                push(&mut out, Tok::Keyword, &line[name_start..end]);
                i = end;
                continue;
            }
        }

        // Numeric literals.
        if b.is_ascii_digit()
            && (i == 0 || !(bytes[i - 1].is_ascii_alphanumeric() || bytes[i - 1] == b'_'))
        {
            let mut end = i + 1;
            while end < len
                && (bytes[end].is_ascii_alphanumeric()
                    || matches!(bytes[end], b'.' | b'_'))
            {
                end += 1;
            }
            push(&mut out, Tok::Number, &line[i..end]);
            i = end;
            continue;
        }

        // Identifiers / keywords / PascalCase types.
        if b.is_ascii_alphabetic() || b == b'_' {
            let mut end = i + 1;
            while end < len && (bytes[end].is_ascii_alphanumeric() || bytes[end] == b'_') {
                end += 1;
            }
            let word = &line[i..end];
            let tok = if is_keyword(word, kind) {
                Tok::Keyword
            } else if kind != SyntaxKind::Json
                && word.len() >= 2
                && word.as_bytes()[0].is_ascii_uppercase()
                && word.bytes().any(|c| c.is_ascii_lowercase())
            {
                Tok::Type
            } else {
                Tok::Plain
            };
            push(&mut out, tok, word);
            i = end;
            continue;
        }

        // Advance one UTF-8 character.
        let ch_len = line[i..].chars().next().map_or(1, char::len_utf8);
        push(&mut out, Tok::Plain, &line[i..i + ch_len]);
        i += ch_len;
    }

    out
}

fn is_keyword(word: &str, kind: SyntaxKind) -> bool {
    match kind {
        SyntaxKind::Json => matches!(word, "true" | "false" | "null"),
        SyntaxKind::Sql => matches!(
            word.to_ascii_uppercase().as_str(),
            "SELECT"
                | "FROM"
                | "WHERE"
                | "INSERT"
                | "INTO"
                | "UPDATE"
                | "SET"
                | "DELETE"
                | "CREATE"
                | "TABLE"
                | "INDEX"
                | "ALTER"
                | "DROP"
                | "JOIN"
                | "LEFT"
                | "RIGHT"
                | "INNER"
                | "ON"
                | "AND"
                | "OR"
                | "NOT"
                | "NULL"
                | "AS"
                | "ORDER"
                | "BY"
                | "GROUP"
                | "LIMIT"
                | "VALUES"
                | "PRIMARY"
                | "KEY"
        ),
        SyntaxKind::Hash => matches!(
            word,
            "def"
                | "class"
                | "import"
                | "from"
                | "as"
                | "return"
                | "yield"
                | "if"
                | "elif"
                | "else"
                | "for"
                | "in"
                | "while"
                | "break"
                | "continue"
                | "pass"
                | "try"
                | "except"
                | "finally"
                | "raise"
                | "with"
                | "async"
                | "await"
                | "lambda"
                | "and"
                | "or"
                | "not"
                | "True"
                | "False"
                | "None"
                | "true"
                | "false"
                | "null"
                | "then"
                | "fi"
                | "do"
                | "done"
                | "case"
                | "esac"
                | "export"
                | "local"
                | "function"
        ),
        SyntaxKind::CStyle => matches!(
            word,
            "fn"
                | "pub"
                | "let"
                | "mut"
                | "const"
                | "static"
                | "var"
                | "function"
                | "async"
                | "await"
                | "return"
                | "if"
                | "else"
                | "match"
                | "switch"
                | "case"
                | "default"
                | "for"
                | "while"
                | "loop"
                | "break"
                | "continue"
                | "struct"
                | "enum"
                | "trait"
                | "impl"
                | "type"
                | "interface"
                | "class"
                | "extends"
                | "implements"
                | "use"
                | "mod"
                | "crate"
                | "super"
                | "self"
                | "Self"
                | "this"
                | "import"
                | "export"
                | "from"
                | "as"
                | "where"
                | "in"
                | "of"
                | "new"
                | "try"
                | "catch"
                | "finally"
                | "throw"
                | "package"
                | "func"
                | "defer"
                | "go"
                | "chan"
                | "select"
                | "range"
                | "true"
                | "false"
                | "null"
                | "nil"
                | "undefined"
                | "void"
                | "move"
                | "ref"
                | "unsafe"
                | "extern"
        ),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formats_byte_sizes_and_line_badges() {
        assert_eq!(format_bytes(512), "512 B");
        assert_eq!(format_bytes(2048), "2.0 KB");
        assert_eq!(format_bytes(3 * 1024 * 1024), "3.0 MB");
        assert_eq!(format_line_badge((42, 42)), "L42");
        assert_eq!(format_line_badge((10, 30)), "L10-L30");
    }

    #[test]
    fn tokenizes_rust_and_json_cleanly() {
        let mut blk = false;
        let toks = tokenize_line("pub fn sse_connect(url: &str) -> u32 { // ok", SyntaxKind::CStyle, &mut blk);
        assert!(!blk);
        assert!(toks.iter().any(|(k, s)| *k == Tok::Keyword && *s == "pub"));
        assert!(toks.iter().any(|(k, s)| *k == Tok::Keyword && *s == "fn"));
        assert!(toks.iter().any(|(k, s)| *k == Tok::Comment && *s == "// ok"));

        let jtoks = tokenize_line(r#"  "port": 8888, "ok": true"#, SyntaxKind::Json, &mut blk);
        assert!(jtoks.iter().any(|(k, s)| *k == Tok::Type && *s == "\"port\""));
        assert!(jtoks.iter().any(|(k, s)| *k == Tok::Number && *s == "8888"));
        assert!(jtoks.iter().any(|(k, s)| *k == Tok::Keyword && *s == "true"));
    }

    #[test]
    fn table_links_for_images_and_code_render_pressable_spans() {
        use nsh_core::{ApproxTextEngine, Core, MountItem, element::Root, prelude::Size};

        #[component]
        fn TableSample() -> impl IntoElement {
            let doc = crate::state::markdown::MarkdownDoc::parse(
                "| 实测链路 | 结果 |\n|---|---|\n| 设计列表 | 测试通过。[截图](docs/preview.png) |\n| SSE | 详情见 [SSE 实现](src/main.rs:2) |\n",
            );
            super::markdown::static_document(&doc).into_element()
        }

        let mut core = Core::new(ApproxTextEngine, Size::new(px(900.0), px(600.0)));
        core.mount_root(&Root::of::<TableSample>());
        let batch = core.commit().unwrap();
        let texts: Vec<_> = batch
            .items
            .iter()
            .filter_map(|i| match i {
                MountItem::Text { text, .. } => Some(text.clone()),
                _ => None,
            })
            .collect();
        let shot_cell = texts
            .iter()
            .find(|t| t.text.contains("截图"))
            .expect("table cell containing 截图");
        assert!(
            !shot_cell.links.is_empty(),
            "截图 link inside Markdown table must have a pressable link span"
        );
        let sse_cell = texts
            .iter()
            .find(|t| t.text.contains("SSE 实现"))
            .expect("table cell containing SSE 实现");
        assert!(
            !sse_cell.links.is_empty(),
            "SSE 实现 link inside Markdown table must have a pressable link span"
        );
    }

    #[test]
    fn code_viewer_highlights_target_lines_and_renders_cleanly() {
        use nsh_core::{ApproxTextEngine, Core, MountItem, element::Root, prelude::Size};

        #[component]
        fn CodeSample() -> impl IntoElement {
            code_viewer(
                "fn main() {\n    let port = 8888;\n    println!(\"{port}\");\n}".into(),
                "rs".into(),
                true,
                false,
                Some((2, 2)),
            )
        }

        let mut core = Core::new(ApproxTextEngine, Size::new(px(720.0), px(480.0)));
        core.mount_root(&Root::of::<CodeSample>());
        let batch = core.commit().unwrap();
        let texts: Vec<_> = batch
            .items
            .iter()
            .filter_map(|i| match i {
                MountItem::Text { text, .. } => Some(text.clone()),
                _ => None,
            })
            .collect();
        let code_text = texts
            .iter()
            .find(|t| t.text.contains("let port = 8888"))
            .expect("rendered code text");
        assert!(code_text.text.contains("▸ 2  "), "line 2 should have target marker");
        assert!(
            code_text.spans.len() > 4,
            "expected syntax-highlighted spans, got {}",
            code_text.spans.len()
        );
    }
}
