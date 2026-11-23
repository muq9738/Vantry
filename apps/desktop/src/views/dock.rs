//! The workbench dock on the right: tabs for files (tree and read-only
//! preview), Git / PR (branch, staged and unstaged changes, unified diff,
//! stage / unstage / commit, the PR summary), the terminal (session tabs, a
//! scrolling output, the command line with interrupt and clear), the
//! preview (Markdown, code, diffs) and the plugins' panel slot.

use nsh_core::{LineHeight, prelude::*};

use super::{badge, caption, ghost_button, link_button, markdown, pill, primary_button};
use crate::{
    controller,
    state::{
        ui::Catalogs,
        workbench::{Change, DockTab, PreviewContent, TreeRow},
    },
    theme::PALETTE,
};

/// The dock's width.
const DOCK_WIDTH: f32 = 380.0;
const MONO: &str = "Menlo";

pub(crate) fn dock() -> impl IntoElement {
    let tab = computed(|| controller::ui().with(|u| u.dock_tab));
    view()
        .w(px(DOCK_WIDTH))
        .flex_shrink_0()
        .flex_col()
        .bg(PALETTE.sidebar)
        .border_l_1()
        .border_l_color(PALETTE.line)
        .child(
            view()
                .h(px(crate::theme::metrics::HEADER_HEIGHT))
                .flex_row()
                .items_center()
                .gap_0p5()
                .px_2()
                .border_b_1()
                .border_b_color(PALETTE.line_soft)
                .child(
                    scroll().horizontal().flex_1().child(
                        view().flex_row().gap_0p5().children(DockTab::ALL.map(|t| {
                            pill(t.label(), move || tab.get() == t, move || controller::select_dock_tab(t))
                        })),
                    ),
                )
                .child(link_button("✕", controller::close_dock).accessibility_label("收起工具坞")),
        )
        .child(dynamic(move || match tab.get() {
            DockTab::Files => files_panel().into_element(),
            DockTab::Git => git_panel().into_element(),
            DockTab::Terminal => terminal_panel().into_element(),
            DockTab::Preview => preview_panel().into_element(),
            DockTab::Plugins => plugins_panel().into_element(),
        }))
}

// -------------------------------------------------------------------- files

fn files_panel() -> impl IntoElement {
    let rows = computed(|| controller::bench().with(|b| b.files.rows()));
    let selected = computed(|| controller::bench().with(|b| b.files.selected.clone()));
    let preview = computed(|| controller::bench().with(|b| b.files.preview.clone()));
    let error = computed(|| controller::bench().with(|b| b.files.error.clone()));
    let root = computed(|| controller::bench().with(|b| b.root.clone()));
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
                    text(move || root.get())
                        .flex_1()
                        .number_of_lines(1u32)
                        .font_family(MONO)
                        .text_size(px(11.0))
                        .text_color(PALETTE.faint),
                )
                .child(link_button("刷新", controller::refresh_files))
                .child(link_button("在访达中显示", controller::reveal_workspace)),
        )
        .child(when(
            move || error.with(Option::is_some),
            move || {
                text(move || error.get().unwrap_or_default())
                    .px_3()
                    .text_size(px(11.5))
                    .text_color(PALETTE.danger)
            },
        ))
        .child(
            scroll().flex_1().max_h(move || {
                px(if preview.with(Option::is_some) { 260.0 } else { 10_000.0 })
            }).child(view().flex_col().px_1().pb_2().child(each(
                move || rows.get(),
                |r: &TreeRow| r.entry.path.clone(),
                move |row, _| tree_row(row, selected),
            ))),
        )
        .child(when(
            move || preview.with(Option::is_some),
            move || {
                view()
                    .flex_1()
                    .flex_col()
                    .border_t_1()
                    .border_t_color(PALETTE.line)
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
                                    .text_color(PALETTE.ink),
                            )
                            .child(link_button("预览", controller::preview_file)),
                    )
                    .child(scroll().flex_1().child(code_lines(move || {
                        preview.with(|p| match p {
                            Some(file) if file.binary => "（二进制文件，不预览）".to_owned(),
                            Some(file) if file.truncated => format!("{}\n…（已截断）", file.text),
                            Some(file) => file.text.clone(),
                            None => String::new(),
                        })
                    })))
            },
        ))
}

fn tree_row(row: ReadSignal<TreeRow>, selected: Memo<Option<String>>) -> impl IntoElement {
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
        .py_0p5()
        .pr_2()
        .pl(move || px(8.0 + row.with(|r| r.depth) as f32 * 14.0))
        .rounded(px(6.0))
        .bg(move || if on() { PALETTE.card } else { AdaptiveColor::TRANSPARENT })
        .hover(|s| s.bg(PALETTE.soft))
        .child(
            text(move || {
                row.with(|r| match (r.entry.is_dir, r.expanded, r.loading) {
                    (true, _, true) => "…",
                    (true, true, _) => "▾",
                    (true, false, _) => "▸",
                    (false, ..) => " ",
                })
            })
            .w(px(12.0))
            .text_size(px(10.5))
            .text_color(PALETTE.faint),
        )
        .child(
            text(move || row.with(|r| r.entry.name.clone()))
                .flex_1()
                .number_of_lines(1u32)
                .text_size(px(12.5))
                .text_color(move || if is_dir() { PALETTE.ink } else { PALETTE.muted }),
        )
}

/// Source text with line numbers.
fn code_lines(source: impl Fn() -> String + 'static) -> impl IntoElement {
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
            .text_color(PALETTE.ink)
            .px_3()
            .py_2(),
    )
}

// ---------------------------------------------------------------------- git

fn git_panel() -> impl IntoElement {
    let status = computed(|| controller::bench().with(|b| b.git.status.clone()));
    let changes = computed(|| controller::bench().with(|b| b.git.changes()));
    let selected = computed(|| controller::bench().with(|b| b.git.selected.clone()));
    let diff = computed(|| controller::bench().with(|b| b.git.diff.clone()));
    let message = computed(|| controller::bench().with(|b| b.git.commit_message.clone()));
    let can_commit = computed(|| controller::bench().with(|b| b.git.commit_ready().is_some()));
    let busy = computed(|| controller::bench().with(|b| b.git.busy || b.git.loading));
    let error = computed(|| controller::bench().with(|b| b.git.error.clone()));
    let last_commit = computed(|| controller::bench().with(|b| b.git.last_commit.clone()));
    let pr = computed(|| controller::bench().with(|b| b.git.pr.clone()));
    let branch_line = move || {
        status.with(|s| match s {
            Some(s) => {
                let mut line = format!("⎇ {}", if s.branch.is_empty() { "（无分支）" } else { &s.branch });
                if !s.remote_branch.is_empty() {
                    line.push_str(&format!(" → {}", s.remote_branch));
                }
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
    scroll().flex_1().child(
        view()
            .flex_col()
            .gap_2()
            .p_3()
            .child(
                view()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .child(
                        text(branch_line)
                            .flex_1()
                            .number_of_lines(1u32)
                            .font_family(MONO)
                            .text_size(px(12.0))
                            .text_color(PALETTE.ink),
                    )
                    .child(link_button("刷新", controller::refresh_git).disabled(move || busy.get())),
            )
            .child(when(
                move || error.with(Option::is_some),
                move || {
                    text(move || error.get().unwrap_or_default())
                        .select_text()
                        .text_size(px(11.5))
                        .text_color(PALETTE.danger)
                },
            ))
            .child(change_group("已暂存", true, changes, selected, busy))
            .child(change_group("未暂存", false, changes, selected, busy))
            .child(when(
                move || changes.with(Vec::is_empty) && status.with(Option::is_some),
                || text("工作区干净。").text_size(px(12.0)).text_color(PALETTE.faint),
            ))
            .child(when(
                move || diff.with(|d| !d.is_empty()),
                move || {
                    view()
                        .flex_col()
                        .gap_1()
                        .child(caption(move || {
                            selected.with(|s| match s {
                                Some((path, staged)) => {
                                    format!("{} · {}", path, if *staged { "已暂存" } else { "未暂存" })
                                }
                                None => "差异".to_owned(),
                            })
                        }))
                        .child(
                            view()
                                .rounded(px(8.0))
                                .bg(PALETTE.code)
                                .max_h(px(320.0))
                                .child(scroll().child(dynamic(move || {
                                    markdown::diff_text(&diff.get()).into_element()
                                }))),
                        )
                },
            ))
            .child(
                view()
                    .flex_col()
                    .gap_1()
                    .child(caption("提交"))
                    .child(
                        view()
                            .px_2()
                            .py_1()
                            .rounded(px(8.0))
                            .bg(PALETTE.card)
                            .border_1()
                            .border_color(PALETTE.line)
                            .child(
                                nsh_base::input()
                                    .multiline(true)
                                    .value(move || message.get())
                                    .on_change(|t: &str| controller::set_commit_message(t))
                                    .placeholder("提交说明")
                                    .placeholder_color(move || color(PALETTE.faint))
                                    .min_h(px(44.0))
                                    .text_size(px(12.5))
                                    .text_color(PALETTE.ink),
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
                                    .text_color(PALETTE.success),
                            )
                            .child(ghost_button("PR 摘要", controller::load_pr))
                            .child(
                                primary_button("提交", controller::commit)
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
                        .bg(PALETTE.card)
                        .border_1()
                        .border_color(PALETTE.line)
                        .child(caption(field(|p| format!("PR · {} → {}", p.branch, if p.base.is_empty() { "（无上游）" } else { &p.base }))))
                        .child(
                            text(field(|p| p.title.clone()))
                                .select_text()
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_size(px(13.0))
                                .text_color(PALETTE.ink),
                        )
                        .child(
                            text(field(|p| p.body.clone()))
                                .select_text()
                                .text_size(px(12.0))
                                .text_color(PALETTE.muted),
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

fn change_group(
    label: &'static str,
    staged: bool,
    changes: Memo<Vec<Change>>,
    selected: Memo<Option<(String, bool)>>,
    busy: Memo<bool>,
) -> impl IntoElement {
    let rows = computed(move || {
        changes.with(|c| c.iter().filter(|c| c.staged == staged).cloned().collect::<Vec<_>>())
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
                        .child(caption(move || format!("{label} · {}", rows.with(Vec::len))).flex_1())
                        .child(
                            link_button(if staged { "全部取消暂存" } else { "全部暂存" }, move || {
                                let paths = rows.with_untracked(|r| r.iter().map(|c| c.path.clone()).collect());
                                controller::stage(paths, staged);
                            })
                            .disabled(move || busy.get()),
                        ),
                )
                .child(each(
                    move || rows.get(),
                    |c: &Change| c.path.clone(),
                    move |change, _| {
                        let path = move || change.with_untracked(|c| c.path.clone());
                        let on = move || {
                            selected.with(|s| s.as_ref().is_some_and(|(p, st)| *st == staged && *p == change.with(|c| c.path.clone())))
                        };
                        view()
                            .flex_row()
                            .items_center()
                            .gap_1()
                            .rounded(px(6.0))
                            .bg(move || if on() { PALETTE.card } else { AdaptiveColor::TRANSPARENT })
                            .child(
                                nsh_base::button()
                                    .on_press(move || controller::select_change(path(), staged))
                                    .flex_1()
                                    .flex_row()
                                    .items_center()
                                    .gap_2()
                                    .px_2()
                                    .py_0p5()
                                    .hover(|s| s.bg(PALETTE.soft))
                                    .child(badge(
                                        move || change.with(|c| c.code.clone()),
                                        move || match change.with(|c| c.code.clone()).as_str() {
                                            "A" | "?" => PALETTE.success,
                                            "D" => PALETTE.danger,
                                            "U" => PALETTE.warning,
                                            _ => PALETTE.accent,
                                        },
                                    ))
                                    .child(
                                        text(move || change.with(|c| c.path.clone()))
                                            .flex_1()
                                            .number_of_lines(1u32)
                                            .font_family(MONO)
                                            .text_size(px(11.5))
                                            .text_color(PALETTE.ink),
                                    ),
                            )
                            .child(
                                link_button(if staged { "−" } else { "+" }, move || {
                                    controller::stage(vec![path()], staged)
                                })
                                .accessibility_label(if staged { "取消暂存" } else { "暂存" })
                                .disabled(move || busy.get()),
                            )
                    },
                ))
        },
    )
}

// ----------------------------------------------------------------- terminal

fn terminal_panel() -> impl IntoElement {
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
        .child(
            scroll()
                .element_ref(&list)
                .flex_1()
                .bg(PALETTE.code)
                .child(
                    text(move || {
                        let text = output.get();
                        if text.is_empty() { "（输入命令后按 Enter 运行）".to_owned() } else { text }
                    })
                    .select_text()
                    .font_family(MONO)
                    .text_size(px(11.5))
                    .line_height(LineHeight::Multiple(1.4))
                    .text_color(PALETTE.ink)
                    .px_3()
                    .py_2(),
                ),
        )
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_2()
                .py_1p5()
                .border_t_1()
                .border_t_color(PALETTE.line)
                .child(text("$").font_family(MONO).text_size(px(12.0)).text_color(PALETTE.accent))
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
                        .placeholder_color(move || color(PALETTE.faint))
                        .auto_correct(false)
                        .auto_capitalize(AutoCapitalize::None)
                        .flex_1()
                        .font_family(MONO)
                        .text_size(px(12.0))
                        .text_color(PALETTE.ink),
                )
                .child(link_button("中断", controller::interrupt_terminal).disabled(move || !running.get()))
                .child(link_button("清屏", controller::clear_terminal)),
        )
}

// ------------------------------------------------------------------ preview

fn preview_panel() -> impl IntoElement {
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
                    .text_color(PALETTE.ink),
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
                    .text_color(PALETTE.faint)
                    .into_element(),
                (Some(PreviewContent::Markdown(_)), true) | (Some(PreviewContent::Code { .. }), _) => {
                    code_lines(move || raw.get()).into_element()
                }
                (Some(PreviewContent::Markdown(doc)), false) => markdown::static_document(&doc).into_element(),
                (Some(PreviewContent::Diff(diff)), _) => markdown::diff_text(&diff).into_element(),
            }
        }))))
}

// ------------------------------------------------------------------ plugins

/// The plugins' panel slot: what is installed, what has a native panel.
fn plugins_panel() -> impl IntoElement {
    let catalogs = computed(|| controller::ui().with(|u| u.catalogs.clone()));
    effect(move || {
        if catalogs.with(|c| matches!(c, Catalogs::NotLoaded)) {
            controller::load_settings_catalogs();
        }
    });
    let plugins = computed(move || match catalogs.get() {
        Catalogs::Loaded(data) => data.plugins.clone(),
        _ => Vec::new(),
    });
    scroll().flex_1().child(
        view()
            .flex_col()
            .gap_2()
            .p_3()
            .child(caption("插件"))
            .child(when(
                move || plugins.with(Vec::is_empty),
                move || {
                    text(move || match catalogs.get() {
                        Catalogs::Loading => "正在读取插件…".to_owned(),
                        Catalogs::Failed(e) => format!("无法读取插件：{e}"),
                        _ => "没有已安装的插件。插件由 Core 插件宿主提供。".to_owned(),
                    })
                    .text_size(px(12.0))
                    .text_color(PALETTE.faint)
                },
            ))
            .child(each(
                move || plugins.get(),
                |p: &vantry_protocol::PluginInfo| p.id.clone(),
                |plugin, _| {
                    let field = move |f: fn(&vantry_protocol::PluginInfo) -> String| move || plugin.with(f);
                    view()
                        .flex_col()
                        .gap_1()
                        .p_2()
                        .rounded(px(10.0))
                        .bg(PALETTE.card)
                        .border_1()
                        .border_color(PALETTE.line)
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .child(
                                    text(field(|p| p.name.clone()))
                                        .flex_1()
                                        .font_weight(FontWeight::SEMIBOLD)
                                        .text_size(px(13.0))
                                        .text_color(PALETTE.ink),
                                )
                                .child(badge(field(|p| if p.has_update { format!("可更新 {}", p.latest_version) } else { p.version.clone() }), PALETTE.muted)),
                        )
                        .child(
                            text(field(|p| p.description.clone()))
                                .text_size(px(12.0))
                                .text_color(PALETTE.muted),
                        )
                        .child(
                            view()
                                .flex_row()
                                .gap_1()
                                .children(plugin.with_untracked(|p| {
                                    p.navigation
                                        .iter()
                                        .map(|nav| {
                                            let (id, page, title) = (p.id.clone(), nav.page.clone(), nav.title.clone());
                                            link_button(format!("打开 {title}"), move || {
                                                controller::open_plugin(id.clone(), page.clone())
                                            })
                                        })
                                        .collect::<Vec<_>>()
                                }))
                                .child(link_button("管理", move || {
                                    controller::plugin_notice(plugin.with_untracked(|p| p.name.clone()))
                                })),
                        )
                },
            )),
    )
}
