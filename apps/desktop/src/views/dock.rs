//! The workbench dock on the right: tabs for files (tree and read-only
//! preview), Git / PR (branch, staged and unstaged changes, unified diff,
//! stage / unstage / commit, the PR summary), the terminal (session tabs, a
//! scrolling output, the command line with interrupt and clear), the
//! preview (Markdown, code, diffs) and the plugins' panel slot.

use nsh_core::{LineHeight, prelude::*};

use super::{
    badge, caption, ghost_button, icon_button, link_button, markdown, pill,
    primary_button,
};
use crate::{
    bridge::workbench::SearchHit,
    controller,
    icons::Icon,
    state::{
        workbench::{Change, DockTab, FileSearch, PreviewContent, TreeRow},
    },
    theme::palette,
};

const MONO: &str = "Menlo";

pub(crate) fn dock() -> impl IntoElement {
    let child = computed(|| {
        controller::ui()
            .with(|u| u.child_panel.clone())
            .filter(|(parent, _)| controller::state().with(|s| s.selected.as_ref() == Some(parent)))
    });
    dynamic(move || match child.get() {
        Some((_, id)) => child_panel(id).into_element(),
        None => tool_dock().into_element(),
    })
}

fn child_panel(id: String) -> impl IntoElement {
    let colors = palette();
    let create = id.is_empty();
    let draft = signal(String::new());
    let target = stored(id);
    let value = move || {
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
    };
    let send = move || {
        if create {
            let message = draft.get_untracked();
            if !message.trim().is_empty() {
                controller::delegate_child(message);
            }
        } else {
            controller::submit_child(&target.get());
        }
    };
    view()
        .flex_1()
        .flex_col()
        .bg(colors.sidebar)
        .p_3()
        .gap_2()
        .child(
            view()
                .flex_row()
                .items_center()
                .child(
                    caption(move || {
                        if create {
                            "派出子代理".to_owned()
                        } else {
                            controller::state().with(|s| {
                                s.thread(&target.get())
                                    .map(|t| t.title.clone())
                                    .unwrap_or_else(|| "子任务".into())
                            })
                        }
                    })
                    .flex_1(),
                )
                .child(icon_button(
                    Icon::X,
                    "关闭子代理分栏",
                    controller::close_dock,
                )),
        )
        .child(
            scroll().flex_1().child(
                text(move || {
                    controller::state().with(|s| {
                        s.runs
                            .iter()
                            .filter(|r| r.thread_id == target.get())
                            .map(|r| format!("{}\n{}", r.input, r.output))
                            .collect::<Vec<_>>()
                            .join("\n\n")
                    })
                })
                .text_size(px(13.0))
                .text_color(colors.ink)
                .select_text(),
            ),
        )
        .child(
            nsh_base::input()
                .label("子任务消息")
                .value(value)
                .on_change(move |s: &str| {
                    if create {
                        draft.set(s.to_owned());
                    } else {
                        controller::state()
                            .update(|a| a.drafts.entry(target.get()).or_default().text = s.into());
                    }
                })
                .on_submit(move |_: &str| send())
                .placeholder("子任务消息")
                .bg(colors.card)
                .p_2()
                .text_color(colors.ink),
        )
        .child(
            primary_button(
                if create {
                    "派出子代理"
                } else {
                    "发送到子任务"
                },
                send,
            )
            .disabled(move || value().trim().is_empty()),
        )
}

fn tool_dock() -> impl IntoElement {
    let colors = palette();
    let tab = computed(|| controller::ui().with(|u| u.dock_tab));
    let roomy = signal(false);
    let has_project = computed(|| controller::bench().with(|b| !b.root.is_empty()));
    view()
        .flex_1()
        .min_w_0()
        .min_h_0()
        .flex_col()
        .bg(colors.canvas)
        .on_layout(move |b: &Bounds<Points>| roomy.set(b.size.width.0 >= 480.0))
        .border_l_1()
        .border_l_color(colors.line)
        .child(
            view()
                .h(px(crate::theme::metrics::HEADER_HEIGHT))
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .border_b_1()
                .border_b_color(colors.line_soft)
                .window_drag(true)
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_0p5()
                        .p_0p5()
                        .rounded(px(8.0))
                        .bg(colors.soft)
                        .children(DockTab::ALL.map(|t| {
                            let glyph = match t {
                                DockTab::Files => Icon::FileText,
                                DockTab::Git => Icon::GitBranch,
                                DockTab::Terminal => Icon::SquareTerminal,
                                DockTab::Mcp => Icon::Server,
                                DockTab::Plugins => Icon::Blocks,
                            };
                            let label = crate::tr(t.label());
                            view().tooltip(text(label)).child(
                                nsh_base::button()
                                    .accessibility_label(label)
                                    .selected(move || tab.get() == t)
                                    .on_press(move || controller::select_dock_tab(t))
                                    .flex_row()
                                    .items_center()
                                    .justify_center()
                                    .gap_1()
                                    .px_2p5()
                                    .h(px(26.0))
                                    .rounded(px(6.0))
                                    .border_1()
                                    .border_color(move || {
                                        if tab.get() == t {
                                            colors.line_soft
                                        } else {
                                            AdaptiveColor::TRANSPARENT
                                        }
                                    })
                                    .bg(move || {
                                        if tab.get() == t {
                                            colors.card
                                        } else {
                                            AdaptiveColor::TRANSPARENT
                                        }
                                    })
                                    .hover(|s| s.bg(colors.card))
                                    .child(crate::icons::icon(glyph, 13.5, move || {
                                        if tab.get() == t {
                                            colors.accent
                                        } else {
                                            colors.muted
                                        }
                                    }))
                                    .child(when(
                                        move || roomy.get() || tab.get() == t,
                                        move || {
                                            text(label)
                                                .text_size(px(12.0))
                                                .font_weight(FontWeight::MEDIUM)
                                                .text_color(move || {
                                                    if tab.get() == t {
                                                        colors.ink
                                                    } else {
                                                        colors.muted
                                                    }
                                                })
                                        },
                                    )),
                            )
                        })),
                )
                .child(view().flex_1())
                .child(when(
                    || {
                        let w = nsh_core::element::window().width.0;
                        w - if !controller::narrow_window() && controller::sidebar_visible() {
                            crate::theme::metrics::sidebar_width(w)
                        } else {
                            0.0
                        } >= 900.0
                    },
                    || {
                        dynamic(|| {
                            let max = controller::ui().with(|u| u.dock_maximized);
                            icon_button(
                                if max {
                                    Icon::Minimize2
                                } else {
                                    Icon::Maximize2
                                },
                                if max {
                                    "还原工具坞"
                                } else {
                                    "最大化工具坞"
                                },
                                || {
                                    controller::ui()
                                        .update(|u| u.dock_maximized = !u.dock_maximized)
                                },
                            )
                            .into_element()
                        })
                    },
                ))
                .child(icon_button(Icon::X, "收起工具坞", controller::close_dock)),
        )
        .child(dynamic(move || match tab.get() {
            DockTab::Files | DockTab::Git if !has_project.get() => {
                let remote = controller::remote_project();
                view()
                    .flex_1()
                    .items_center()
                    .justify_center()
                    .gap_2()
                    .px_4()
                    .child(caption(if remote {
                        "请在远程环境中查看项目文件"
                    } else {
                        "选择项目后可查看文件和 Git 变更"
                    }))
                    .child(when(
                        move || !remote,
                        || ghost_button("为新会话选择项目", controller::choose_project),
                    ))
                    .into_element()
            }
            DockTab::Files => files_panel().into_element(),
            DockTab::Git => super::git::git_tools().into_element(),
            DockTab::Terminal => terminal_panel().into_element(),
            DockTab::Mcp => super::settings::dock_catalog(true).into_element(),
            DockTab::Plugins => super::settings::dock_catalog(false).into_element(),
        }))
}

fn files_panel() -> impl IntoElement {
    let colors = palette();
    let tree_rev = computed(|| controller::bench().with(|b| b.files.tree_revision));
    let rows = computed(move || {
        let _ = tree_rev.get();
        controller::bench().with_untracked(|b| b.files.rows())
    });
    let selected = computed(|| controller::bench().with(|b| b.files.selected.clone()));
    let preview = computed(|| controller::bench().with(|b| b.files.preview.clone()));
    let error = computed(|| controller::bench().with(|b| b.files.error.clone()));
    let root = computed(|| controller::bench().with(|b| b.root.clone()));
    let search = computed(|| controller::bench().with(|b| b.files.search.clone()));
    let searching = move || search.with(|s| s.active());
    let tree = computed(move ||preview.with(Option::is_none) || controller::bench().with(|b|b.files.show_tree));
    let file = move ||selected.get_untracked().unwrap_or_default();
    let markdown = computed(move ||selected.with(|p|p.as_ref().is_some_and(|p|p.to_lowercase().ends_with(".md") || p.to_lowercase().ends_with(".markdown"))));
    let source = computed(move ||controller::bench().with(|b|b.files.selected.as_ref().is_some_and(|p|b.files.source_paths.contains(p))));
    view()
        .flex_1()
        .flex_col()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .px_3()
                .py_1p5()
                .child(
                    crate::icons::icon(Icon::Folder, 14.0, colors.muted),
                )
                .child(view().flex_1().min_w_0().tooltip(text(move ||root.get())).child(
                    text(move || root.with(|p| std::path::Path::new(p).file_name().map(|s|s.to_string_lossy().into_owned()).unwrap_or_else(||p.clone())))
                        .number_of_lines(1u32).font_weight(FontWeight::SEMIBOLD)
                        .text_size(px(13.0)).text_color(colors.ink)))
                .child(view().tooltip(text("刷新")).child(icon_button(Icon::RefreshCw, "刷新", controller::refresh_files)))
                .child(view().tooltip(text("在访达中显示")).child(icon_button(Icon::ExternalLink, "在访达中显示", controller::reveal_workspace))),
        )
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .mx_3()
                .mb_1()
                .px_2()
                .py_0p5()
                .rounded(px(7.0))
                .bg(colors.card)
                .border_1()
                .border_color(colors.line)
                .child(crate::icons::icon(Icon::Search, 13.0, colors.muted))
                .child(
                    nsh_base::input()
                        .value(move || search.with(|s| s.query.clone()))
                        .on_change(|t: &str| controller::set_file_search(t))
                        .placeholder("搜索文件名或内容")
                        .placeholder_color(move || color(colors.faint))
                        .auto_correct(false)
                        .auto_capitalize(AutoCapitalize::None)
                        .flex_1()
                        .text_size(px(12.0))
                        .text_color(colors.ink),
                )
                .child(when(
                    move || search.with(|s| s.active()),
                    || link_button("✕", || controller::set_file_search("")).accessibility_label("清除搜索"),
                )),
        )
        .child(when(
            move || error.with(Option::is_some),
            move || {
                text(move || error.get().unwrap_or_default())
                    .px_3()
                    .text_size(px(11.5))
                    .text_color(colors.danger)
            },
        ))
        .child(view().flex_row().flex_1().min_h_0().gap_2().p_2()
        .child(when(move ||tree.get(),move ||view().w(px(188.0)).flex_shrink_0().flex_col().bg(colors.sidebar).border_1().border_color(colors.line_soft).rounded(px(9.0)).overflow_hidden()
        .child(when(searching, move || search_results(search, selected, preview)))
        .child(when(move || !searching(), move || {
            scroll().flex_1().child(view().flex_col().p_1().child(each(
                move || rows.get(),
                |r: &TreeRow| r.entry.path.clone(),
                move |row, _| tree_row(row, selected),
            )))
        }))))
        .child(when(move ||preview.with(Option::is_none),||view().flex_1().items_center().justify_center().child(caption("选择左侧文件以阅读内容"))))
        .child(when(
            move || preview.with(Option::is_some),
            move || {
                view()
                    .flex_1()
                    .flex_col()
                    .border_t_1()
                    .border_t_color(colors.line)
                    .child(
                        view()
                            .flex_row()
                            .items_center()
                            .px_3()
                            .py_1()
                            .child(
                                text(move || selected.get().unwrap_or_default())
                                    .flex_1()
                                    .number_of_lines(1u32)
                                    .font_family(MONO)
                                    .text_size(px(11.5))
                                    .text_color(colors.ink),
                            )
                            .child(link_button(move ||if tree.get(){"隐藏目录"}else{"显示目录"},||controller::bench().update(|b|b.files.show_tree=!b.files.show_tree)))
                            .child(when(move ||markdown.get(),move ||link_button(move ||if source.get(){"阅读预览"}else{"查看源码"},move ||controller::bench().update(|b|{let p=file();if !b.files.source_paths.remove(&p){b.files.source_paths.insert(p);}}))))
                            .child(view().tooltip(text("复制路径")).child(super::small_icon_button(Icon::Copy,"复制路径",move ||controller::copy(file()))))
                            .child(view().tooltip(text("引用到输入框")).child(super::small_icon_button(Icon::Quote,"引用到输入框",move ||controller::quote_to_draft(file())))),
                    )
                    .child(when(move ||markdown.get() && !source.get(),move ||scroll().flex_1().child(dynamic(move ||markdown::static_document(&crate::state::markdown::MarkdownDoc::parse(&preview.with(|p|p.as_ref().map(|p|p.text.clone()).unwrap_or_default()))).into_element()))))
                    .child(when(move ||!markdown.get() || source.get(),move ||scroll().flex_1().child(code_lines(move || {
                        preview.with(|p| match p {
                            Some(file) if file.binary => "（二进制文件，不预览）".to_owned(),
                            Some(file) if file.truncated => format!("{}\n…（已截断）", file.text),
                            Some(file) => file.text.clone(),
                            None => String::new(),
                        })
                    }))))
            },
        )))
}

/// What the search found: file names, then matching lines.
fn search_results(
    search: Memo<FileSearch>,
    selected: Memo<Option<String>>,
    preview: Memo<Option<crate::bridge::workbench::FileContent>>,
) -> impl IntoElement {
    let colors = palette();
    let hits = computed(move || search.with(|s| s.results.clone().into_iter().enumerate().collect::<Vec<_>>()));
    scroll()
        .flex_1()
        .max_h(move || px(if preview.with(Option::is_some) { 260.0 } else { 10_000.0 }))
        .child(
            view()
                .flex_col()
                .px_1()
                .pb_2()
                .child(
                    caption(move || {
                        search.with(|s| {
                            if s.loading {
                                "搜索中…".to_owned()
                            } else if let Some(error) = &s.error {
                                format!("搜索失败：{error}")
                            } else if s.results.is_empty() {
                                "没有匹配的文件或内容。".to_owned()
                            } else {
                                format!("{} 条结果", s.results.len())
                            }
                        })
                    })
                    .px_2()
                    .py_1(),
                )
                .child(each(
                    move || hits.get(),
                    |(n, _): &(usize, SearchHit)| *n,
                    move |hit, _| {
                        let path = move || hit.with_untracked(|(_, h)| h.path.clone());
                        let on = move || selected.with(|s| s.as_deref() == Some(hit.with(|(_, h)| h.path.clone()).as_str()));
                        nsh_base::button()
                            .on_press(move || controller::open_search_hit(path()))
                            .flex_col()
                            .px_2()
                            .py_0p5()
                            .rounded(px(6.0))
                            .bg(move || if on() { colors.card } else { AdaptiveColor::TRANSPARENT })
                            .hover(|s| s.bg(colors.soft))
                            .child(
                                text(move || {
                                    hit.with(|(_, h)| {
                                        if h.line_number > 0 { format!("{}:{}", h.path, h.line_number) } else { h.path.clone() }
                                    })
                                })
                                .number_of_lines(1u32)
                                .font_family(MONO)
                                .text_size(px(11.5))
                                .text_color(colors.ink),
                            )
                            .child(when(
                                move || hit.with(|(_, h)| !h.line.is_empty()),
                                move || {
                                    text(move || hit.with(|(_, h)| h.line.clone()))
                                        .number_of_lines(1u32)
                                        .font_family(MONO)
                                        .text_size(px(11.0))
                                        .text_color(colors.muted)
                                },
                            ))
                    },
                )),
        )
}

fn tree_row(row: ReadSignal<TreeRow>, selected: Memo<Option<String>>) -> impl IntoElement {
    let colors = palette();
    let path = move || row.with_untracked(|r| r.entry.path.clone());
    let is_dir = move || row.with(|r| r.entry.is_dir);
    let on = move || selected.with(|s| s.as_deref() == Some(row.with(|r| r.entry.path.clone()).as_str()));
    nsh_base::button()
        .on_press(move || {
            if is_dir() {
                controller::toggle_dir(path());
            } else {
                controller::select_file(path());
            }
        })
        .flex_row()
        .items_center()
        .gap_1()
        .py_1()
        .pr_2()
        .pl(move || px(6.0 + row.with(|r| r.depth) as f32 * 13.0))
        .rounded(px(6.0))
        .bg(move || if on() { colors.info_bg } else { AdaptiveColor::TRANSPARENT })
        .hover(|s| s.bg(colors.soft))
        .child(
            text(move || {
                row.with(|r| match (r.entry.is_dir, r.expanded, r.loading) {
                    (true, _, true) => "…",
                    (true, true, _) => "▾",
                    (true, false, _) => "▸",
                    (false, ..) => " ",
                })
            })
            .w(px(11.0))
            .text_size(px(10.0))
            .text_color(colors.faint),
        )
        .child(dynamic(move || {
            let (dir, open) = row.with(|r| (r.entry.is_dir, r.expanded));
            let glyph = match (dir, open) {
                (true, true) => Icon::FolderOpen,
                (true, false) => Icon::Folder,
                (false, _) => Icon::File,
            };
            crate::icons::icon(glyph, 13.0, if dir { colors.accent } else { colors.faint })
                .into_element()
        }))
        .child(
            text(move || row.with(|r| r.entry.name.clone()))
                .flex_1()
                .number_of_lines(1u32)
                .text_size(px(12.0))
                .text_color(move || if is_dir() || on() { colors.ink } else { colors.muted }),
        )
}

/// Source text with line numbers.
fn code_lines(source: impl Fn() -> String + 'static) -> impl IntoElement {
    let colors = palette();
    let numbered = computed(move || {
        let text = source();
        let width = text.lines().count().max(1).to_string().len();
        text.lines()
            .enumerate()
            .map(|(n, line)| format!("{:>width$}  {line}", n + 1))
            .collect::<Vec<_>>()
            .join("\n")
    });
    scroll().horizontal().child(
        text(move || numbered.get())
            .select_text()
            .font_family(MONO)
            .text_size(px(11.5))
            .line_height(LineHeight::Multiple(1.45))
            .text_color(colors.ink)
            .px_3()
            .py_2(),
    )
}

pub(super) fn git_panel() -> impl IntoElement {
    let viewport = signal(Size::new(px(0.0), px(0.0)));
    let wide = move || viewport.get().width.0 >= 720.0;
    let colors = palette();
    let status_ver = computed(|| controller::bench().with(|b| b.git.status_version));
    let status = computed(move || {
        let _ = status_ver.get();
        controller::bench().with_untracked(|b| b.git.status.clone())
    });
    let changes = computed(move || {
        let _ = status_ver.get();
        controller::bench().with_untracked(|b| b.git.changes())
    });
    let selected = computed(|| controller::bench().with(|b| b.git.selected.clone()));
    let diff_ver = computed(|| controller::bench().with(|b| b.git.diff_version));
    let has_diff = computed(move || {
        let _ = diff_ver.get();
        controller::bench().with_untracked(|b| !b.git.diff.is_empty())
    });
    let message = computed(|| controller::bench().with(|b| b.git.commit_message.clone()));
    let can_commit = computed(|| controller::bench().with(|b| b.git.commit_ready().is_some()));
    let busy = computed(|| controller::bench().with(|b| b.git.busy || b.git.loading));
    let error = computed(|| controller::bench().with(|b| b.git.error.clone()));
    let last_commit = computed(|| controller::bench().with(|b| b.git.last_commit.clone()));
    let pr = computed(|| controller::bench().with(|b| b.git.pr.clone()));
    let branch_menu = computed(|| controller::bench().with(|b| b.git.branch_menu));
    let branch_anchor = element_ref();
    let filter = signal(String::new());
    let branches = computed(move || {
        let _ = status_ver.get();
        let needle = filter.get().to_lowercase();
        controller::bench().with_untracked(|b| {
            b.git
                .other_branches()
                .into_iter()
                .filter(|s| s.to_lowercase().contains(&needle))
                .collect::<Vec<_>>()
        })
    });
    let remote_branches = computed(move || {
        let _ = status_ver.get();
        let needle = filter.get().to_lowercase();
        controller::bench().with_untracked(|b| {
            b.git
                .remote_branches()
                .into_iter()
                .filter(|s| s.to_lowercase().contains(&needle))
                .collect::<Vec<_>>()
        })
    });
    let confirm_discard = computed(|| controller::bench().with(|b| b.git.confirm_discard.clone()));
    let branch_line = move || {
        status.with(|s| match s {
            Some(s) => {
                let mut line = if s.branch.is_empty() {
                    "（无分支）".to_owned()
                } else {
                    s.branch.clone()
                };
                if s.ahead > 0 {
                    line.push_str(&format!(" ↑{}", s.ahead));
                }
                if s.behind > 0 {
                    line.push_str(&format!(" ↓{}", s.behind));
                }
                if s.conflicts {
                    line.push_str(" · 有冲突");
                }
                line
            }
            None => "读取中…".to_owned(),
        })
    };
    scroll()
        .flex_1()
        .on_layout(move |bounds: &Bounds<Points>| viewport.set(bounds.size))
        .child(
            view()
                .min_h(move || viewport.get().height)
                .flex_col()
                .gap_2p5()
                .p_3()
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(
                            nsh_base::button()
                                .element_ref(&branch_anchor)
                                .accessibility_label("切换分支")
                                .on_press(controller::toggle_branch_menu)
                                .flex_1()
                                .min_w_0()
                                .h(px(30.0))
                                .flex_row()
                                .items_center()
                                .gap_1p5()
                                .px_2p5()
                                .rounded(px(8.0))
                                .bg(colors.card)
                                .border_1()
                                .border_color(colors.line)
                                .hover(|s| s.bg(colors.soft))
                                .child(crate::icons::icon(Icon::GitBranch, 13.0, colors.accent))
                                .child(
                                    text(branch_line)
                                        .flex_1()
                                        .number_of_lines(1u32)
                                        .font_family(MONO)
                                        .text_size(px(12.0))
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_color(colors.ink),
                                )
                                .child(crate::icons::icon(Icon::ChevronDown, 12.0, colors.faint)),
                        )
                        .child(
                            view().tooltip(text("刷新 Git 状态")).child(
                                super::small_icon_button(
                                    Icon::RefreshCw,
                                    "刷新",
                                    controller::refresh_git,
                                )
                                .disabled(move || busy.get()),
                            ),
                        ),
                )
                .child(when(
                    move || branch_menu.get(),
                    move || {
                        overlay()
                            .anchor(&branch_anchor)
                            .placement(Placement::Bottom)
                            .align(Align::Start)
                            .offset(px(5.0))
                            .w(px(340.0))
                            .on_cancel(controller::toggle_branch_menu)
                            .on_press_outside(|_: &Point<Points>| {
                                controller::bench().update(|b| b.git.branch_menu = false)
                            })
                            .child(
                                view()
                                    .w_full()
                                    .flex_col()
                                    .gap_1p5()
                                    .p_2()
                                    .rounded(px(12.0))
                                    .bg(colors.card)
                                    .border_1()
                                    .border_color(colors.line)
                                    .shadow_lg()
                                    .overflow_hidden()
                                    .child(
                                        view()
                                            .flex_row()
                                            .items_center()
                                            .gap_1p5()
                                            .h(px(30.0))
                                            .px_2p5()
                                            .rounded(px(8.0))
                                            .bg(colors.soft)
                                            .border_1()
                                            .border_color(colors.line_soft)
                                            .child(crate::icons::icon(
                                                Icon::Search,
                                                13.0,
                                                colors.faint,
                                            ))
                                            .child(
                                                nsh_base::input()
                                                    .flex_1()
                                                    .value(move || filter.get())
                                                    .on_change(move |s: &str| filter.set(s.into()))
                                                    .placeholder("搜索分支")
                                                    .placeholder_color(move || color(colors.faint))
                                                    .text_size(px(12.0))
                                                    .text_color(colors.ink),
                                            ),
                                    )
                                    .child(
                                        scroll().max_h(px(280.0)).child(
                                            view()
                                                .w_full()
                                                .flex_col()
                                                .gap_0p5()
                                                .child(caption("本地分支").px_2().pt_1())
                                                .child(when(
                                                    move || branches.with(Vec::is_empty),
                                                    || {
                                                        text("没有其他本地分支。")
                                                            .px_2()
                                                            .py_1()
                                                            .text_size(px(11.5))
                                                            .text_color(colors.faint)
                                                    },
                                                ))
                                                .child(each(
                                                    move || branches.get(),
                                                    |b: &String| format!("local:{b}"),
                                                    move |branch, _| {
                                                        view()
                                                            .w_full()
                                                            .flex_row()
                                                            .items_center()
                                                            .gap_1()
                                                            .rounded(px(7.0))
                                                            .hover(|s| s.bg(colors.soft))
                                                            .child(
                                                                nsh_base::button()
                                                                    .on_press(move || {
                                                                        controller::git_checkout(
                                                                            branch.get_untracked(),
                                                                        )
                                                                    })
                                                                    .disabled(move || busy.get())
                                                                    .flex_1()
                                                                    .min_w_0()
                                                                    .flex_row()
                                                                    .items_center()
                                                                    .gap_1p5()
                                                                    .px_2()
                                                                    .py_1()
                                                                    .rounded(px(6.0))
                                                                    .child(crate::icons::icon(
                                                                        Icon::GitBranch,
                                                                        12.0,
                                                                        colors.muted,
                                                                    ))
                                                                    .child(
                                                                        text(move || branch.get())
                                                                            .flex_1()
                                                                            .number_of_lines(1u32)
                                                                            .font_family(MONO)
                                                                            .text_size(px(11.5))
                                                                            .text_color(colors.ink),
                                                                    ),
                                                            )
                                                            .child(
                                                                nsh_base::button()
                                                                    .accessibility_label(
                                                                        "与当前分支比较",
                                                                    )
                                                                    .on_press(move || {
                                                                        controller::git_page(
                                                                            crate::state::workbench::GitPage::Compare,
                                                                            Some(branch.get_untracked()),
                                                                        )
                                                                    })
                                                                    .flex_shrink_0()
                                                                    .px_2()
                                                                    .py_0p5()
                                                                    .mr_1()
                                                                    .rounded(px(5.0))
                                                                    .bg(colors.soft)
                                                                    .hover(|s| s.bg(colors.info_bg))
                                                                    .child(
                                                                        text("比较")
                                                                            .text_size(px(11.0))
                                                                            .text_color(
                                                                                colors.muted,
                                                                            ),
                                                                    ),
                                                            )
                                                    },
                                                ))
                                                .child(when(
                                                    move || {
                                                        remote_branches.with(|v| !v.is_empty())
                                                    },
                                                    move || {
                                                        view()
                                                            .w_full()
                                                            .flex_col()
                                                            .gap_0p5()
                                                            .pt_1p5()
                                                            .mt_1()
                                                            .border_t_1()
                                                            .border_t_color(colors.line_soft)
                                                            .child(caption("远程分支").px_2())
                                                            .child(each(
                                                                move || remote_branches.get(),
                                                                |b: &String| format!("remote:{b}"),
                                                                move |branch, _| {
                                                                    view()
                                                                        .w_full()
                                                                        .flex_row()
                                                                        .items_center()
                                                                        .gap_1()
                                                                        .rounded(px(7.0))
                                                                        .hover(|s| {
                                                                            s.bg(colors.soft)
                                                                        })
                                                                        .child(
                                                                            nsh_base::button()
                                                                                .on_press(move || {
                                                                                    controller::git_page(
                                                                                        crate::state::workbench::GitPage::Log,
                                                                                        Some(branch.get_untracked()),
                                                                                    )
                                                                                })
                                                                                .flex_1()
                                                                                .min_w_0()
                                                                                .flex_row()
                                                                                .items_center()
                                                                                .gap_1p5()
                                                                                .px_2()
                                                                                .py_1()
                                                                                .child(
                                                                                    crate::icons::icon(
                                                                                        Icon::GitBranch,
                                                                                        12.0,
                                                                                        colors.faint,
                                                                                    ),
                                                                                )
                                                                                .child(
                                                                                    text(move || {
                                                                                        branch.get()
                                                                                    })
                                                                                    .flex_1()
                                                                                    .number_of_lines(
                                                                                        1u32,
                                                                                    )
                                                                                    .font_family(
                                                                                        MONO,
                                                                                    )
                                                                                    .text_size(px(
                                                                                        11.5,
                                                                                    ))
                                                                                    .text_color(
                                                                                        colors.muted,
                                                                                    ),
                                                                                ),
                                                                        )
                                                                },
                                                            ))
                                                    },
                                                )),
                                        ),
                                    ),
                            )
                    },
                ))
            .child(when(
                move || confirm_discard.with(Option::is_some),
                move || {
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .p_2()
                        .rounded(px(8.0))
                        .bg(colors.approval)
                        .border_1()
                        .border_color(colors.approval_border)
                        .child(
                            text(move || {
                                confirm_discard.with(|c| match c.as_deref() {
                                    Some("") | None => "撤销全部未暂存改动？未跟踪文件将被删除，无法恢复。".to_owned(),
                                    Some(path) => format!("撤销 {path} 的未暂存改动？无法恢复。"),
                                })
                            })
                            .flex_1()
                            .text_size(px(11.5))
                            .text_color(colors.ink),
                        )
                        .child(link_button("取消", || controller::ask_discard(None)))
                        .child(
                            link_button("撤销", move || {
                                let target = confirm_discard.get_untracked().unwrap_or_default();
                                let paths = controller::bench().with_untracked(|b| b.git.discard_paths(&target));
                                controller::git_discard(paths);
                            })
                            .disabled(move || busy.get()),
                        )
                },
            ))
            .child(when(
                move || error.with(Option::is_some),
                move || {
                    text(move || error.get().unwrap_or_default())
                        .select_text()
                        .text_size(px(11.5))
                        .text_color(colors.danger)
                },
            ))
            .child(
                view()
                    .flex_col()
                    .flex_grow(1.0)
                    .style_when(wide, |s| s.flex_row())
                    .gap_2p5()
                    .child(when(
                        move || changes.with(|c| !c.is_empty()),
                        move || {
                            view()
                                .style_when(wide, |s| s.w(px(320.0)).flex_shrink_0())
                                .rounded(px(10.0))
                                .border_1()
                                .border_color(colors.line_soft)
                                .bg(colors.canvas)
                                .overflow_hidden()
                                .child(
                                    scroll()
                                        .max_h(move || {
                                            px(if wide() {
                                                viewport.get().height.0
                                            } else {
                                                (viewport.get().height.0 * 0.34).max(150.0)
                                            })
                                        })
                                        .child(
                                            view()
                                                .flex_col()
                                                .gap_1p5()
                                                .p_1p5()
                                                .child(change_group(
                                                    "已暂存",
                                                    true,
                                                    false,
                                                    changes,
                                                    selected,
                                                    busy,
                                                ))
                                                .child(change_group(
                                                    "更改",
                                                    false,
                                                    false,
                                                    changes,
                                                    selected,
                                                    busy,
                                                ))
                                                .child(change_group(
                                                    "未版本控制",
                                                    false,
                                                    true,
                                                    changes,
                                                    selected,
                                                    busy,
                                                )),
                                        ),
                                )
                        },
                    ))
                    .child(when(
                        move || changes.with(Vec::is_empty) && status.with(Option::is_some),
                        || {
                            view()
                                .p_4()
                                .rounded(px(10.0))
                                .bg(colors.soft)
                                .items_center()
                                .child(
                                    text("工作区干净，没有待提交变更。")
                                        .text_size(px(12.0))
                                        .text_color(colors.muted),
                                )
                        },
                    ))
                    .child(when(
                        move || has_diff.get(),
                        move || {
                            view()
                                .flex_grow(1.0)
                                .style_when(wide, |s| s.flex_1())
                                .min_w_0()
                                .flex_col()
                                .gap_1()
                                .child(
                                    view()
                                        .flex_row()
                                        .items_center()
                                        .gap_1p5()
                                        .px_0p5()
                                        .child(crate::icons::icon(
                                            Icon::FileDiff,
                                            13.0,
                                            colors.muted,
                                        ))
                                        .child(
                                            text(move || {
                                                selected.with(|s| {
                                                    s.as_ref()
                                                        .map(|(path, _)| path.clone())
                                                        .unwrap_or_default()
                                                })
                                            })
                                            .flex_1()
                                            .number_of_lines(1u32)
                                            .font_family(MONO)
                                            .text_size(px(12.0))
                                            .font_weight(FontWeight::SEMIBOLD)
                                            .text_color(colors.ink),
                                        ),
                                )
                                .child(
                                    view()
                                        .flex_grow(1.0)
                                        .min_h(px(180.0))
                                        .border_1()
                                        .border_color(colors.line_soft)
                                        .overflow_hidden()
                                        .rounded(px(10.0))
                                        .bg(colors.code)
                                        .child(super::git::diff_view(true)),
                                )
                        },
                    )),
            )
            .child(
                view()
                    .flex_col()
                    .gap_1()
                    .pt_2().border_t_1().border_t_color(colors.line_soft)
                    .child(
                        view()
                            .px_2()
                            .py_1()
                            .rounded(px(8.0))
                            .bg(colors.card)
                            .border_1()
                            .border_color(colors.line)
                            .child(
                                nsh_base::input()
                                    .multiline(true)
                                    .value(move || message.get())
                                    .on_change(|t: &str| controller::set_commit_message(t))
                                    .placeholder("提交说明")
                                    .placeholder_color(move || color(colors.faint))
                                    .min_h(px(44.0))
                                    .text_size(px(12.5))
                                    .text_color(colors.ink),
                            ),
                    )
                    .child(
                        view()
                            .flex_row()
                            .gap_2()
                            .child(
                                text(move || last_commit.get().map(|c| format!("已提交 {c}")).unwrap_or_default())
                                    .flex_1()
                                    .number_of_lines(1u32)
                                    .text_size(px(11.0))
                                    .text_color(colors.success),
                            )
                            .child(ghost_button("PR 摘要", controller::load_pr))
                            .child(
                                primary_button("创建本地提交", controller::commit)
                                    .disabled(move || !can_commit.get()),
                            ),
                    ),
            )
            .child(when(
                move || pr.with(Option::is_some),
                move || {
                    let field = move |f: fn(&crate::bridge::workbench::PrSummary) -> String| {
                        move || pr.with(|p| p.as_ref().map(f).unwrap_or_default())
                    };
                    view()
                        .flex_col()
                        .gap_1()
                        .p_2()
                        .rounded(px(8.0))
                        .bg(colors.card)
                        .border_1()
                        .border_color(colors.line)
                        .child(caption(field(|p| format!("PR · {} → {}", p.branch, if p.base.is_empty() { "（无上游）" } else { &p.base }))))
                        .child(
                            text(field(|p| p.title.clone()))
                                .select_text()
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_size(px(13.0))
                                .text_color(colors.ink),
                        )
                        .child(
                            text(field(|p| p.body.clone()))
                                .select_text()
                                .text_size(px(12.0))
                                .text_color(colors.muted),
                        )
                        .child(link_button("复制 PR 文本", move || {
                            if let Some(p) = pr.get_untracked() {
                                controller::copy(format!("{}\n\n{}", p.title, p.body));
                            }
                        }))
                },
            )),
    )
}

const CHANGE_PAGE_SIZE: usize = 120;

fn change_group(
    label: &'static str,
    staged: bool,
    untracked: bool,
    changes: Memo<Vec<Change>>,
    selected: Memo<Option<(String, bool)>>,
    busy: Memo<bool>,
) -> impl IntoElement {
    let colors = palette();
    let limit = signal(CHANGE_PAGE_SIZE);
    let rows = computed(move || {
        changes.with(|c| {
            c.iter()
                .filter(|c| c.staged == staged && (c.code == "?") == untracked)
                .cloned()
                .collect::<Vec<_>>()
        })
    });
    let visible_rows = computed(move || {
        let cap = limit.get();
        rows.with(|r| r.iter().take(cap).cloned().collect::<Vec<_>>())
    });
    when(
        move || rows.with(|r| !r.is_empty()),
        move || {
            view()
                .flex_col()
                .gap_0p5()
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_1()
                        .px_1p5()
                        .py_1()
                        .child(
                            text(label)
                                .text_size(px(11.5))
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_color(colors.muted),
                        )
                        .child(
                            text(move || rows.with(Vec::len).to_string())
                                .px_1p5()
                                .py_0p5()
                                .rounded_full()
                                .bg(colors.soft)
                                .text_size(px(10.5))
                                .text_color(colors.faint),
                        )
                        .child(view().flex_1())
                        .child(when(!staged && !untracked, move || {
                            view().tooltip(text("撤销所有未暂存更改")).child(
                                super::small_icon_button(
                                    Icon::Undo2,
                                    "撤销所有未暂存更改",
                                    || controller::ask_discard(Some(String::new())),
                                )
                                .disabled(move || busy.get()),
                            )
                        }))
                        .child(
                            link_button(if staged { "全部取消暂存" } else { "全部暂存" }, move || {
                                let paths =
                                    rows.with_untracked(|r| r.iter().map(|c| c.path.clone()).collect());
                                controller::stage(paths, staged);
                            })
                            .disabled(move || busy.get()),
                        ),
                )
                .child(each(
                    move || visible_rows.get(),
                    |c: &Change| c.path.clone(),
                    move |change, _| {
                        let path = move || change.with_untracked(|c| c.path.clone());
                        let on = move || {
                            selected.with(|s| {
                                s.as_ref().is_some_and(|(p, st)| {
                                    *st == staged && *p == change.with(|c| c.path.clone())
                                })
                            })
                        };
                        view()
                            .flex_row()
                            .items_center()
                            .gap_0p5()
                            .pr_1()
                            .rounded(px(7.0))
                            .bg(move || {
                                if on() {
                                    colors.info_bg
                                } else {
                                    AdaptiveColor::TRANSPARENT
                                }
                            })
                            .hover(|s| s.bg(colors.soft))
                            .child(
                                nsh_base::button()
                                    .on_press(move || controller::select_change(path(), staged))
                                    .flex_1()
                                    .min_w_0()
                                    .flex_row()
                                    .items_center()
                                    .gap_2()
                                    .px_2()
                                    .min_h(px(26.0))
                                    .py_0p5()
                                    .child(badge(
                                        move || change.with(|c| c.code.clone()),
                                        move || match change.with(|c| c.code.clone()).as_str() {
                                            "A" | "?" => colors.success,
                                            "D" => colors.danger,
                                            "U" => colors.warning,
                                            _ => colors.accent,
                                        },
                                    ))
                                    .child(
                                        text(move || change.with(|c| c.path.clone()))
                                            .flex_1()
                                            .number_of_lines(1u32)
                                            .font_family(MONO)
                                            .text_size(px(11.5))
                                            .text_color(colors.ink),
                                    ),
                            )
                            .child(when(!staged, move || {
                                super::small_icon_button(Icon::Undo2, "撤销改动", move || {
                                    controller::ask_discard(Some(path()))
                                })
                                .disabled(move || busy.get())
                            }))
                            .child(
                                super::small_icon_button(
                                    if staged { Icon::Minus } else { Icon::Plus },
                                    if staged { "取消暂存" } else { "暂存" },
                                    move || controller::stage(vec![path()], staged),
                                )
                                .disabled(move || busy.get()),
                            )
                    },
                ))
                .child(when(
                    move || rows.with(Vec::len) > limit.get(),
                    move || {
                        view().py_1().items_center().child(link_button(
                            move || {
                                format!(
                                    "显示更多（剩余 {} 项）",
                                    rows.with(Vec::len).saturating_sub(limit.get())
                                )
                            },
                            move || limit.set(limit.get_untracked() + CHANGE_PAGE_SIZE),
                        ))
                    },
                ))
        },
    )
}

fn terminal_panel() -> impl IntoElement {
    let colors = palette();
    let sessions = computed(|| {
        controller::bench().with(|b| {
            b.terminal
                .sessions
                .iter()
                .map(|s| (s.id.clone(), s.title.clone(), s.running))
                .collect::<Vec<_>>()
        })
    });
    let active = computed(|| controller::bench().with(|b| b.terminal.current().map(|s| s.id.clone())));
    let active_task = computed(|| {
        let task_info = controller::bench().with(|b| {
            b.terminal
                .current()
                .and_then(|s| s.request_id.clone().map(|req| (req, s.task_cwd.clone(), s.running)))
        })?;
        let pending = controller::state().with(|s| {
            s.requests
                .iter()
                .any(|r| r.id == task_info.0 && r.status == "pending")
        });
        pending.then_some(task_info)
    });
    let output = computed(|| controller::bench().with(|b| b.terminal.current().map(|s| s.output.clone()).unwrap_or_default()));
    let input = computed(|| controller::bench().with(|b| b.terminal.current().map(|s| s.input.clone()).unwrap_or_default()));
    let running = computed(|| controller::bench().with(|b| b.terminal.interruptible().is_some()));
    let list = element_ref();
    // Follow the output to its end.
    effect(move || {
        output.with(|_| ());
        list.scroll_to_end(false);
    });
    view()
        .flex_1()
        .flex_col()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .px_2()
                .py_1()
                .child(
                    scroll().horizontal().flex_1().child(view().flex_row().gap_0p5().child(each(
                        move || sessions.get(),
                        |s: &(String, String, bool)| s.0.clone(),
                        move |session, _| {
                            let id = move || session.with_untracked(|s| s.0.clone());
                            view()
                                .flex_row()
                                .items_center()
                                .child(pill(
                                    move || session.with(|s| if s.2 { format!("● {}", s.1) } else { s.1.clone() }),
                                    move || active.get().as_deref() == Some(session.with(|s| s.0.clone()).as_str()),
                                    move || controller::select_terminal(id()),
                                ))
                                .child(link_button("✕", move || controller::close_terminal(id())).accessibility_label("关闭终端"))
                        },
                    ))),
                )
                .child(link_button("＋", controller::new_terminal).accessibility_label("新建终端")),
        )
        .child(dynamic(move || {
            active_task.get().map(|(req_id, cwd, is_running)| {
                let done_id = req_id.clone();
                let done_cwd = cwd.clone();
                let cancel_id = req_id;
                view()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .px_3()
                    .py_1p5()
                    .bg(colors.approval)
                    .border_b_1()
                    .border_b_color(colors.line)
                    .child(
                        text(if is_running {
                            "交互终端任务运行中，退出后将自动继续…"
                        } else {
                            "等待确认交互终端结果"
                        })
                        .flex_1()
                        .text_size(px(11.5))
                        .text_color(colors.warning),
                    )
                    .child(ghost_button("标记已完成", move || {
                        controller::complete_terminal_task(done_id.clone(), done_cwd.clone())
                    }))
                    .child(ghost_button("取消", move || {
                        controller::cancel_terminal_task(cancel_id.clone())
                    }))
                    .into_element()
            })
        }))
        .child(when(controller::native_shell,move || dynamic(move || active.get().map(|id| shell_screen(id).into_element()))))
        .child(when(|| !controller::native_shell(),move || scroll()
                .element_ref(&list)
                .flex_1()
                .bg(colors.code)
                .child(
                    text(move || {
                        let text = output.get();
                        if text.is_empty() { "（输入命令后按 Enter 运行）".to_owned() } else { text }
                    })
                    .select_text()
                    .font_family(MONO)
                    .text_size(px(11.5))
                    .line_height(LineHeight::Multiple(1.4))
                    .text_color(colors.ink)
                    .px_3()
                    .py_2(),
                )))
        .child(when(|| !controller::native_shell(),move ||
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_2()
                .py_1p5()
                .border_t_1()
                .border_t_color(colors.line)
                .child(text("$").font_family(MONO).text_size(px(12.0)).text_color(colors.accent))
                .child(
                    nsh_base::input()
                        .value(move || input.get())
                        .on_change(|t: &str| controller::set_terminal_input(t))
                        .on_submit(|_: &str| controller::run_terminal())
                        .on_key_down(|e: &KeyEvent| match e.key.as_ref() {
                            "ArrowUp" | "Up" | "up" => controller::recall_terminal(-1),
                            "ArrowDown" | "Down" | "down" => controller::recall_terminal(1),
                            _ => {}
                        })
                        .placeholder(move || if running.get() { "运行中…".to_owned() } else { "命令".to_owned() })
                        .placeholder_color(move || color(colors.faint))
                        .auto_correct(false)
                        .auto_capitalize(AutoCapitalize::None)
                        .flex_1()
                        .font_family(MONO)
                        .text_size(px(12.0))
                        .text_color(colors.ink),
                )
                .child(link_button("中断", controller::interrupt_terminal).disabled(move || !running.get()))
                .child(link_button("清屏", controller::clear_terminal))))
}

fn shell_screen(id: String) -> impl IntoElement {
    let term = component_ref::<nsh_base::Terminal>();
    controller::shell_start(id.clone());
    let last = stored(String::new());
    let session = id.clone();
    let output = computed(move || {
        controller::bench().with(|b| {
            b.terminal
                .sessions
                .iter()
                .find(|s| s.id == session)
                .map(|s| s.output.clone())
                .unwrap_or_default()
        })
    });
    effect(move || {
        let output = output.get();
        if let Some(api) = term.api::<nsh_base::TerminalApi>() {
            let previous = last.replace(output.clone());
            if let Some(delta) = output.strip_prefix(&previous) {
                if !delta.is_empty() {
                    api.write(delta.to_owned());
                }
            } else {
                api.reset();
                api.write(output);
            }
        }
    });
    let resize = id.clone();
    nsh_base::terminal()
        .component_ref(&term)
        .flex_1()
        .font_family(MONO)
        .font_size(px(12.0))
        .accessibility_label("终端输入")
        .on_data(move |s: &SharedString| controller::shell_write(&id, s.to_string()))
        .on_resize(move |e: &nsh_base::TerminalEvent| {
            if let nsh_base::TerminalEvent::Resize(cols, rows) = e {
                controller::shell_resize(&resize, *rows as u16, *cols as u16);
            }
        })
}

pub(crate) fn preview_dialog() -> impl IntoElement {
    when(||controller::bench().with(|b|b.preview.content.is_some()),||{
        let close=||controller::bench().update(|b|b.preview=Default::default());
        nsh_base::dialog().on_cancel(close).child(view().flex_1().bg(palette().scrim).items_center().justify_center().p_5().child(view().max_w(px(940.0)).w_full().max_h(px(680.0)).h_full().bg(palette().canvas).rounded(px(12.0)).p_3().child(link_button("关闭预览",close)).child(preview_panel())))
    })
}

fn preview_panel() -> impl IntoElement {
    let colors = palette();
    let title = computed(|| controller::bench().with(|b| b.preview.title.clone()));
    let content = computed(|| controller::bench().with(|b| b.preview.content.clone()));
    let source = computed(|| controller::bench().with(|b| b.preview.source));
    let raw = computed(|| controller::bench().with(|b| b.preview.raw().to_owned()));
    let is_markdown = move || matches!(content.get(), Some(PreviewContent::Markdown(_)));
    view()
        .flex_1()
        .flex_col()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .px_3()
                .py_1p5()
                .child(
                    text(move || {
                        let t = title.get();
                        if t.is_empty() { "预览".to_owned() } else { t }
                    })
                    .flex_1()
                    .number_of_lines(1u32)
                    .text_size(px(12.5))
                    .text_color(colors.ink),
                )
                .child(when(is_markdown, move || {
                    link_button(move || if source.get() { "渲染" } else { "源码" }, controller::toggle_preview_source)
                }))
                .child(link_button("复制", move || controller::copy(raw.get_untracked()))),
        )
        .child(scroll().flex_1().child(view().p_3().child(dynamic(move || {
            match (content.get(), source.get()) {
                (None, _) => text("在文件面板选择文件，或在回复上选择“在预览中打开”。")
                    .text_size(px(12.0))
                    .text_color(colors.faint)
                    .into_element(),
                (Some(PreviewContent::Markdown(_)), true) | (Some(PreviewContent::Code { .. }), _) => {
                    code_lines(move || raw.get()).into_element()
                }
                (Some(PreviewContent::Markdown(doc)), false) => markdown::static_document(&doc).into_element(),
                (Some(PreviewContent::Diff(diff)), _) => markdown::diff_text(&diff).into_element(),
            }
        }))))
}
