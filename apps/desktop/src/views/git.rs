use nsh_core::{
    element::desktop::{MenuItem, popup_menu},
    prelude::*,
};

use super::{
    badge, caption, ghost_button, icon_button, link_button, pill, segment, segments,
    small_icon_button,
};
use crate::{
    bridge::workbench::history::{CommitRow, Revision, hunks},
    controller,
    icons::{Icon, icon},
    state::workbench::{GitDialog, GitPage},
    theme::palette,
};

const MONO: &str = "Menlo";
const MAX_DIFF_BYTES: usize = 180 * 1024;
const COMMIT_PAGE_SIZE: usize = 100;

fn clip_patch(patch: &str) -> (String, bool) {
    if patch.len() <= MAX_DIFF_BYTES {
        return (patch.to_owned(), false);
    }
    let mut end = MAX_DIFF_BYTES;
    while end > 0 && !patch.is_char_boundary(end) {
        end -= 1;
    }
    if let Some(nl) = patch[..end].rfind('\n') {
        end = nl + 1;
    }
    (patch[..end].to_owned(), true)
}

pub(super) fn git_tools() -> impl IntoElement {
    let colors = palette();
    let page = computed(|| controller::bench().with(|b| b.git.page));
    let dialog = computed(|| controller::bench().with(|b| b.git.dialog));
    let busy = computed(|| controller::bench().with(|b| b.git.busy || b.git.loading));
    view()
        .flex_1()
        .min_h_0()
        .flex_col()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .px_3()
                .py_1p5()
                .border_b_1()
                .border_b_color(colors.line_soft)
                .child(
                    segments()
                        .child(segment(
                            "提交",
                            move || page.get() == GitPage::Commit,
                            || controller::git_page(GitPage::Commit, None),
                        ))
                        .child(segment(
                            "日志",
                            move || page.get() == GitPage::Log,
                            || controller::git_page(GitPage::Log, None),
                        ))
                        .child(when(
                            || controller::bench().with(|b| !b.git.compare.is_empty()),
                            move || {
                                segment(
                                    "比较",
                                    move || page.get() == GitPage::Compare,
                                    || controller::git_page(GitPage::Compare, None),
                                )
                            },
                        )),
                )
                .child(view().flex_1())
                .child(
                    view().tooltip(super::tip(crate::tr("抓取远程更新 (Fetch)"))).child(
                        small_icon_button(Icon::RefreshCw, "抓取", || {
                            controller::git_sync_action("git.fetch")
                        })
                        .disabled(move || busy.get()),
                    ),
                )
                .child(
                    view().tooltip(super::tip(crate::tr("拉取 (Pull)"))).child(
                        small_icon_button(Icon::Download, "拉取", || {
                            controller::git_sync_action("git.pull")
                        })
                        .disabled(move || busy.get()),
                    ),
                )
                .child(
                    view().tooltip(super::tip(crate::tr("推送 (Push)"))).child(
                        small_icon_button(Icon::Upload, "推送", || {
                            controller::git_sync_action("git.push")
                        })
                        .disabled(move || busy.get()),
                    ),
                )
                .child(view().w(px(1.0)).h(px(14.0)).mx_0p5().bg(colors.line_soft))
                .child(when(
                    move || dialog.get().is_none(),
                    || {
                        view().tooltip(super::tip(crate::tr("展开 Git 面板"))).child(small_icon_button(
                            Icon::Maximize2,
                            "展开 Git 面板",
                            || controller::git_dialog(Some(GitDialog::Expanded)),
                        ))
                    },
                ))
                .child(view().tooltip(super::tip(crate::tr("文件变更"))).child(small_icon_button(
                    Icon::FileDiff,
                    "文件变更",
                    || controller::git_dialog(Some(GitDialog::Changes)),
                ))),
        )
        .child(dynamic(move || {
            if page.get() == GitPage::Commit {
                super::dock::git_panel().into_element()
            } else {
                history(page).into_element()
            }
        }))
}

pub(crate) fn dialog() -> impl IntoElement {
    let kind = computed(|| controller::bench().with(|b| b.git.dialog));
    dynamic(move || {
        kind.get().map(|kind| {
            let colors = palette();
            nsh_base::dialog()
                .on_cancel(|| controller::git_dialog(None))
                .child(
                    view()
                        .flex_1()
                        .p_5()
                        .bg(colors.scrim)
                        .items_center()
                        .justify_center()
                        .child(
                            view()
                                .w_full()
                                .h_full()
                                .max_w(px(if kind == GitDialog::Changes {
                                    960.0
                                } else {
                                    1200.0
                                }))
                                .max_h(move || {
                                    px(if kind == GitDialog::Changes {
                                        680.0
                                    } else {
                                        crate::window_size().height.0 - 40.0
                                    })
                                })
                                .bg(colors.canvas)
                                .border_1()
                                .border_color(colors.line)
                                .shadow_xl()
                                .rounded(px(14.0))
                                .overflow_hidden()
                                .flex_col()
                                .child(
                                    view()
                                        .flex_row()
                                        .items_center()
                                        .px_4()
                                        .py_2p5()
                                        .border_b_1()
                                        .border_b_color(colors.line_soft)
                                        .child(
                                            text(if kind == GitDialog::Expanded {
                                                "Git 面板"
                                            } else {
                                                "文件变更"
                                            })
                                            .flex_1()
                                            .text_size(px(14.0))
                                            .font_weight(FontWeight::SEMIBOLD)
                                            .text_color(colors.ink),
                                        )
                                        .child(icon_button(Icon::X, "关闭", || {
                                            controller::git_dialog(None)
                                        })),
                                )
                                .child(if kind == GitDialog::Expanded {
                                    git_tools().into_element()
                                } else {
                                    changes().into_element()
                                }),
                        ),
                )
                .into_element()
        })
    })
}

fn changes() -> impl IntoElement {
    let colors = palette();
    let split = computed(|| controller::bench().with(|b| b.git.changes_split));
    view()
        .flex_1()
        .min_h_0()
        .flex_col()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .py_2()
                .border_b_1()
                .border_b_color(colors.line_soft)
                .child(
                    segments()
                        .child(segment(
                            "统一",
                            move || !split.get(),
                            || controller::bench().update(|b| b.git.changes_split = false),
                        ))
                        .child(segment(
                            "并排",
                            move || split.get(),
                            || controller::bench().update(|b| b.git.changes_split = true),
                        )),
                ),
        )
        .child(dynamic(move || {
            match controller::bench().with(|b| b.git.changes_patch.clone()) {
                Some(Ok(patch)) if !patch.is_empty() => {
                    let (clipped, truncated) = clip_patch(&patch);
                    view()
                        .flex_1()
                        .min_h_0()
                        .flex_col()
                        .child(when(truncated, move || {
                            text("差异内容较大，已截断前 180 KB 以保持流畅。")
                                .px_3()
                                .py_1()
                                .bg(colors.approval)
                                .text_size(px(11.5))
                                .text_color(colors.warning)
                        }))
                        .child(
                            nsh_base::diff_view()
                                .patch(clipped)
                                .layout(move || {
                                    if split.get() {
                                        nsh_base::DiffLayout::Split
                                    } else {
                                        nsh_base::DiffLayout::Unified
                                    }
                                })
                                .flex_1(),
                        )
                        .into_element()
                }
                Some(Ok(_)) => view()
                    .flex_1()
                    .items_center()
                    .justify_center()
                    .child(caption("没有文件变更"))
                    .into_element(),
                Some(Err(e)) => view().p_3().child(caption(e)).into_element(),
                None => view()
                    .flex_1()
                    .items_center()
                    .justify_center()
                    .child(caption("正在读取变更…"))
                    .into_element(),
            }
        }))
}

fn open_branch_filter_menu(current: String, local: Vec<String>, remote: Vec<String>) {
    let mut items = vec![
        MenuItem::new("当前分支 (HEAD)")
            .checked(current.is_empty())
            .on_select(|| controller::git_page(GitPage::Log, Some(String::new()))),
    ];
    if !local.is_empty() {
        items.push(MenuItem::separator());
        for b in local {
            let checked = current == b;
            let pick = b.clone();
            items.push(
                MenuItem::new(b)
                    .checked(checked)
                    .on_select(move || controller::git_page(GitPage::Log, Some(pick.clone()))),
            );
        }
    }
    if !remote.is_empty() {
        items.push(MenuItem::separator());
        for b in remote {
            let checked = current == b;
            let pick = b.clone();
            items.push(
                MenuItem::new(b)
                    .checked(checked)
                    .on_select(move || controller::git_page(GitPage::Log, Some(pick.clone()))),
            );
        }
    }
    drop(popup_menu(items));
}

fn history(page: Memo<GitPage>) -> impl IntoElement {
    let colors = palette();
    let branch = computed(|| controller::bench().with(|b| b.git.log_branch.clone()));
    let status_ver = computed(|| controller::bench().with(|b| b.git.status_version));
    let query = computed(|| controller::bench().with(|b| b.git.query.clone()));
    let author_filter = computed(|| controller::bench().with(|b| b.git.log_author.clone()));
    let date_filter = computed(|| controller::bench().with(|b| b.git.log_date.clone()));
    let path = computed(|| controller::bench().with(|b| b.git.log_path.clone()));
    let filters_open = signal(false);
    let commit_limit = signal(COMMIT_PAGE_SIZE);
    let has_extra_filters = computed(move || {
        !author_filter.with(String::is_empty)
            || !date_filter.with(String::is_empty)
            || !path.with(String::is_empty)
    });
    let rows = computed(|| controller::bench().with(|b| b.git.commits.clone()));
    let visible = computed(move || {
        let is_log = page.get() == GitPage::Log;
        let needle = if is_log {
            query.get().to_lowercase()
        } else {
            String::new()
        };
        let author_needle = if is_log {
            author_filter.get().to_lowercase()
        } else {
            String::new()
        };
        let date_needle = if is_log {
            date_filter.get().to_lowercase()
        } else {
            String::new()
        };
        rows.with(|rows| {
            rows.iter()
                .filter(|row| {
                    let q_ok = needle.is_empty()
                        || [&row.subject, &row.author, &row.date, &row.id]
                            .iter()
                            .any(|s| s.to_lowercase().contains(&needle));
                    let a_ok = author_needle.is_empty()
                        || row.author.to_lowercase().contains(&author_needle);
                    let d_ok = date_needle.is_empty()
                        || row.date.to_lowercase().contains(&date_needle);
                    q_ok && a_ok && d_ok
                })
                .cloned()
                .collect::<Vec<_>>()
        })
    });
    let visible_slice = computed(move || {
        let limit = commit_limit.get();
        visible.with(|all| all.iter().take(limit).cloned().collect::<Vec<_>>())
    });
    let files = computed(|| controller::bench().with(|b| b.git.revision_files.clone()));
    let rev = computed(|| controller::bench().with(|b| b.git.revision.clone()));
    let show_files = computed(move || {
        page.get() == GitPage::Log
            || controller::bench().with(|b| {
                b.git.compare_files || matches!(b.git.revision, Some(Revision::Commit(_)))
            })
    });
    let selection = computed(|| controller::bench().with(|b| b.git.revision_path.clone()));

    scroll().flex_1().child(
        view()
            .flex_col()
            .p_3()
            .gap_2p5()
            .child(when(
                move || page.get() == GitPage::Log,
                move || {
                    view()
                        .flex_col()
                        .gap_2()
                        // Row 1: Search bar + commit count + refresh
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_1p5()
                                .child(
                                    view()
                                        .flex_1()
                                        .h(px(30.0))
                                        .flex_row()
                                        .items_center()
                                        .gap_1p5()
                                        .px_2p5()
                                        .rounded(px(8.0))
                                        .bg(colors.card)
                                        .border_1()
                                        .border_color(colors.line)
                                        .child(icon(Icon::Search, 13.0, colors.faint))
                                        .child(
                                            nsh_base::input()
                                                .flex_1()
                                                .placeholder("搜索提交、作者或哈希…")
                                                .placeholder_color(move || color(colors.faint))
                                                .value(move || query.get())
                                                .on_change(|s: &str| {
                                                    controller::bench()
                                                        .update(|b| b.git.query = s.into())
                                                })
                                                .text_size(px(12.0))
                                                .text_color(colors.ink),
                                        )
                                        .child(when(
                                            move || query.with(|q| !q.is_empty()),
                                            || {
                                                small_icon_button(Icon::X, "清除搜索", || {
                                                    controller::bench()
                                                        .update(|b| b.git.query.clear())
                                                })
                                            },
                                        )),
                                )
                                .child(
                                    text(move || format!("{} 个提交", visible.with(Vec::len)))
                                        .text_size(px(11.0))
                                        .text_color(colors.faint),
                                )
                                .child(small_icon_button(Icon::RefreshCw, "刷新日志", || {
                                    controller::git_page(GitPage::Log, None)
                                })),
                        )
                        // Row 2: Compact filter chips (Branch menu + expandable Author/Date/Path)
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .flex_wrap()
                                .gap_1p5()
                                .child(
                                    nsh_base::button()
                                        .accessibility_label("按分支筛选")
                                        .on_press(move || {
                                            let _ = status_ver.get_untracked();
                                            let (local, remote) =
                                                controller::bench().with_untracked(|b| {
                                                    let l = b
                                                        .git
                                                        .status
                                                        .as_ref()
                                                        .and_then(|s| s.branches.clone())
                                                        .unwrap_or_default();
                                                    let r = b.git.remote_branches();
                                                    (l, r)
                                                });
                                            open_branch_filter_menu(
                                                branch.get_untracked(),
                                                local,
                                                remote,
                                            );
                                        })
                                        .flex_row()
                                        .items_center()
                                        .gap_1()
                                        .h(px(26.0))
                                        .px_2p5()
                                        .rounded(px(7.0))
                                        .bg(colors.card)
                                        .border_1()
                                        .border_color(colors.line)
                                        .hover(|s| s.bg(colors.soft))
                                        .child(icon(Icon::GitBranch, 12.0, colors.muted))
                                        .child(
                                            text(move || {
                                                let b = branch.get();
                                                if b.is_empty() {
                                                    "分支 · HEAD".to_owned()
                                                } else {
                                                    b
                                                }
                                            })
                                            .max_w(px(160.0))
                                            .number_of_lines(1u32)
                                            .text_size(px(11.5))
                                            .text_color(colors.ink),
                                        )
                                        .child(icon(Icon::ChevronDown, 11.0, colors.faint)),
                                )
                                .child(
                                    nsh_base::button()
                                        .accessibility_label("作者、日期与路径筛选")
                                        .on_press(move || {
                                            filters_open.set(!filters_open.get_untracked())
                                        })
                                        .flex_row()
                                        .items_center()
                                        .gap_1()
                                        .h(px(26.0))
                                        .px_2p5()
                                        .rounded(px(7.0))
                                        .bg(move || {
                                            if filters_open.get() || has_extra_filters.get() {
                                                colors.info_bg
                                            } else {
                                                colors.card
                                            }
                                        })
                                        .border_1()
                                        .border_color(move || {
                                            if filters_open.get() || has_extra_filters.get() {
                                                colors.accent
                                            } else {
                                                colors.line
                                            }
                                        })
                                        .hover(|s| s.bg(colors.soft))
                                        .child(icon(Icon::SlidersHorizontal, 12.0, move || {
                                            if has_extra_filters.get() {
                                                colors.info_ink
                                            } else {
                                                colors.muted
                                            }
                                        }))
                                        .child(
                                            text("高级筛选")
                                                .text_size(px(11.5))
                                                .text_color(move || {
                                                    if has_extra_filters.get() {
                                                        colors.info_ink
                                                    } else {
                                                        colors.ink
                                                    }
                                                }),
                                        ),
                                )
                                .child(when(
                                    move || has_extra_filters.get(),
                                    || {
                                        link_button("重置筛选", || {
                                            controller::bench().update(|b| {
                                                b.git.log_author.clear();
                                                b.git.log_date.clear();
                                                b.git.log_path.clear();
                                            });
                                            controller::git_page(GitPage::Log, None);
                                        })
                                    },
                                )),
                        )
                        // Collapsible Author / Date / Path filter bar
                        .child(when(
                            move || filters_open.get(),
                            move || {
                                view()
                                    .flex_col()
                                    .gap_1p5()
                                    .p_2()
                                    .rounded(px(9.0))
                                    .bg(colors.soft)
                                    .border_1()
                                    .border_color(colors.line_soft)
                                    .child(
                                        view()
                                            .flex_row()
                                            .gap_1p5()
                                            .child(
                                                nsh_base::input()
                                                    .flex_1()
                                                    .placeholder("作者")
                                                    .placeholder_color(move || color(colors.faint))
                                                    .value(move || author_filter.get())
                                                    .on_change(|s: &str| {
                                                        controller::bench()
                                                            .update(|b| b.git.log_author = s.into())
                                                    })
                                                    .px_2()
                                                    .py_1()
                                                    .rounded(px(7.0))
                                                    .bg(colors.card)
                                                    .border_1()
                                                    .border_color(colors.line)
                                                    .text_size(px(12.0))
                                                    .text_color(colors.ink),
                                            )
                                            .child(
                                                nsh_base::input()
                                                    .flex_1()
                                                    .placeholder("日期 (如 2026-11)")
                                                    .placeholder_color(move || color(colors.faint))
                                                    .value(move || date_filter.get())
                                                    .on_change(|s: &str| {
                                                        controller::bench()
                                                            .update(|b| b.git.log_date = s.into())
                                                    })
                                                    .px_2()
                                                    .py_1()
                                                    .rounded(px(7.0))
                                                    .bg(colors.card)
                                                    .border_1()
                                                    .border_color(colors.line)
                                                    .text_size(px(12.0))
                                                    .text_color(colors.ink),
                                            ),
                                    )
                                    .child(
                                        nsh_base::input()
                                            .placeholder("按文件路径筛选 · Enter 应用")
                                            .placeholder_color(move || color(colors.faint))
                                            .value(move || path.get())
                                            .on_change(|s: &str| {
                                                controller::bench()
                                                    .update(|b| b.git.log_path = s.into())
                                            })
                                            .on_submit(|_: &str| {
                                                controller::git_page(GitPage::Log, None)
                                            })
                                            .px_2()
                                            .py_1()
                                            .rounded(px(7.0))
                                            .bg(colors.card)
                                            .border_1()
                                            .border_color(colors.line)
                                            .text_size(px(12.0))
                                            .text_color(colors.ink),
                                    )
                            },
                        ))
                },
            ))
            // Compare header bar
            .child(when(
                move || page.get() == GitPage::Compare,
                move || {
                    view()
                        .flex_col()
                        .gap_2()
                        .p_2p5()
                        .rounded(px(10.0))
                        .bg(colors.soft)
                        .border_1()
                        .border_color(colors.line_soft)
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .child(icon(Icon::GitBranch, 13.0, colors.accent))
                                .child(
                                    text(|| {
                                        controller::bench().with(|b| {
                                            let head = b
                                                .git
                                                .status
                                                .as_ref()
                                                .map(|s| s.branch.as_str())
                                                .unwrap_or("HEAD");
                                            if b.git.compare_reverse {
                                                format!("{} → {}", b.git.compare, head)
                                            } else {
                                                format!("{head} → {}", b.git.compare)
                                            }
                                        })
                                    })
                                    .flex_1()
                                    .number_of_lines(1u32)
                                    .font_family(MONO)
                                    .text_size(px(12.0))
                                    .font_weight(FontWeight::MEDIUM)
                                    .text_color(colors.ink),
                                )
                                .child(
                                    nsh_base::button()
                                        .accessibility_label("交换方向")
                                        .on_press(controller::swap_compare_direction)
                                        .flex_row()
                                        .items_center()
                                        .gap_1()
                                        .px_2()
                                        .py_1()
                                        .rounded(px(6.0))
                                        .bg(colors.card)
                                        .border_1()
                                        .border_color(colors.line)
                                        .hover(|s| s.bg(colors.soft))
                                        .child(icon(Icon::ArrowLeftRight, 12.0, colors.muted))
                                        .child(
                                            text("交换方向")
                                                .text_size(px(11.5))
                                                .text_color(colors.ink),
                                        ),
                                ),
                        )
                        .child(
                            segments()
                                .child(segment(
                                    "提交差异",
                                    || controller::bench().with(|b| !b.git.compare_files),
                                    || controller::git_page(GitPage::Compare, None),
                                ))
                                .child(segment(
                                    "文件差异",
                                    || controller::bench().with(|b| b.git.compare_files),
                                    || {
                                        controller::bench()
                                            .update(|b| b.git.compare_files = true);
                                        let branch = controller::bench()
                                            .with_untracked(|b| b.git.compare.clone());
                                        controller::git_revision(Revision::Compare(branch));
                                    },
                                )),
                        )
                },
            ))
            // Commit list table
            .child(when(
                move || {
                    page.get() == GitPage::Log
                        || !controller::bench().with(|b| b.git.compare_files)
                },
                move || {
                    view()
                        .flex_col()
                        .rounded(px(10.0))
                        .border_1()
                        .border_color(colors.line_soft)
                        .bg(colors.canvas)
                        .overflow_hidden()
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .px_2p5()
                                .py_1p5()
                                .bg(colors.sidebar)
                                .border_b_1()
                                .border_b_color(colors.line_soft)
                                .child(caption("提交").flex_1())
                                .child(caption("日期").w(px(76.0)))
                                .child(caption("哈希").w(px(56.0))),
                        )
                        .child(each(
                            move || visible_slice.get(),
                            |r: &CommitRow| r.id.clone(),
                            move |r, _| {
                                let is_selected = move || {
                                    rev.with(|s| {
                                        matches!(s, Some(Revision::Commit(id)) if r.with(|c| c.id == *id))
                                    })
                                };
                                nsh_base::button()
                                    .accessibility_label(move || {
                                        r.with(|c| Some(SharedString::from(c.subject.clone())))
                                    })
                                    .on_press(move || {
                                        controller::git_revision(Revision::Commit(
                                            r.with_untracked(|c| c.id.clone()),
                                        ))
                                    })
                                    .flex_row()
                                    .items_center()
                                    .gap_2()
                                    .px_2p5()
                                    .py_1p5()
                                    .border_b_1()
                                    .border_b_color(colors.line_soft)
                                    .bg(move || {
                                        if is_selected() {
                                            colors.info_bg
                                        } else {
                                            AdaptiveColor::TRANSPARENT
                                        }
                                    })
                                    .hover(|s| s.bg(colors.soft))
                                    .child(
                                        text(move || {
                                            r.with(|c| {
                                                c.graph
                                                    .replace('*', "●")
                                                    .replace('|', "│")
                                                    .replace('/', "╱")
                                                    .replace('\\', "╲")
                                            })
                                        })
                                        .font_family(MONO)
                                        .text_color(colors.accent)
                                        .text_size(px(11.0)),
                                    )
                                    .child(
                                        view()
                                            .flex_1()
                                            .min_w_0()
                                            .flex_col()
                                            .gap_0p5()
                                            .child(
                                                text(move || r.with(|c| c.subject.clone()))
                                                    .text_size(px(12.5))
                                                    .font_weight(FontWeight::MEDIUM)
                                                    .text_color(colors.ink)
                                                    .number_of_lines(1u32),
                                            )
                                            .child(when(
                                                move || r.with(|c| !c.refs.trim().is_empty()),
                                                move || {
                                                    badge(
                                                        move || r.with(|c| c.refs.clone()),
                                                        colors.info_ink,
                                                    )
                                                },
                                            )),
                                    )
                                    .child(
                                        text(move || r.with(|c| c.date.clone()))
                                            .w(px(76.0))
                                            .number_of_lines(1u32)
                                            .text_size(px(11.0))
                                            .text_color(colors.faint),
                                    )
                                    .child(
                                        text(move || {
                                            r.with(|c| {
                                                c.id.get(..7).unwrap_or(&c.id).to_owned()
                                            })
                                        })
                                        .w(px(56.0))
                                        .font_family(MONO)
                                        .text_size(px(11.0))
                                        .text_color(colors.muted),
                                    )
                            },
                        ))
                        .child(when(
                            move || visible.with(Vec::len) > commit_limit.get(),
                            move || {
                                view().p_2().items_center().child(ghost_button(
                                    move || {
                                        format!(
                                            "加载更多（还有 {} 条）",
                                            visible.with(Vec::len).saturating_sub(commit_limit.get())
                                        )
                                    },
                                    move || {
                                        commit_limit
                                            .set(commit_limit.get_untracked() + COMMIT_PAGE_SIZE)
                                    },
                                ))
                            },
                        ))
                },
            ))
            // Selected commit metadata card
            .child(dynamic(move || {
                let detail = rev.get().and_then(|r| {
                    rows.with(|rows| {
                        rows.iter()
                            .find(|c| r == Revision::Commit(c.id.clone()))
                            .cloned()
                    })
                });
                detail.map(|c| {
                    view()
                        .flex_col()
                        .gap_1()
                        .p_3()
                        .rounded(px(10.0))
                        .bg(colors.card)
                        .border_1()
                        .border_color(colors.line)
                        .child(
                            text(c.subject)
                                .select_text()
                                .text_size(px(13.0))
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_color(colors.ink),
                        )
                        .child(
                            text(format!(
                                "{} · {} · {}",
                                c.id.get(..8).unwrap_or(&c.id),
                                c.author,
                                c.date
                            ))
                            .select_text()
                            .font_family(MONO)
                            .text_size(px(11.0))
                            .text_color(colors.muted),
                        )
                        .into_element()
                })
            }))
            // Changed files + diff for selected commit/compare
            .child(when(
                move || show_files.get() && files.with(|f| !f.is_empty()),
                move || {
                    view()
                        .flex_col()
                        .gap_2()
                        .child(
                            view()
                                .flex_col()
                                .p_1p5()
                                .gap_0p5()
                                .rounded(px(10.0))
                                .border_1()
                                .border_color(colors.line_soft)
                                .bg(colors.canvas)
                                .child(dynamic(move || {
                                    view()
                                        .flex_col()
                                        .gap_0p5()
                                        .children(files.get().into_iter().take(120).map(
                                            move |path| {
                                                let name = path.clone();
                                                let active = path.clone();
                                                pill(
                                                    name,
                                                    move || selection.get() == active,
                                                    move || {
                                                        controller::git_revision_file(path.clone())
                                                    },
                                                )
                                            },
                                        ))
                                        .into_element()
                                })),
                        )
                        .child(
                            view()
                                .h(move || {
                                    px((crate::window_size().height.0 * 0.4).max(200.0))
                                })
                                .rounded(px(10.0))
                                .border_1()
                                .border_color(colors.line_soft)
                                .overflow_hidden()
                                .bg(colors.code)
                                .child(diff_view(false)),
                        )
                },
            ))
            .child(when(
                || controller::bench().with(|b| b.git.error.is_some()),
                move || {
                    text(|| {
                        controller::bench()
                            .with(|b| b.git.error.clone().unwrap_or_default())
                    })
                    .select_text()
                    .text_size(px(11.5))
                    .text_color(colors.danger)
                },
            )),
    )
}

pub(super) fn diff_view(worktree: bool) -> impl IntoElement {
    // Gate diff reads on monotonic revision counters so unrelated bench()
    // mutations (typing commit message, opening branch menu, terminal output)
    // never clone or compare large diff strings.
    let diff_ver = computed(move || {
        controller::bench().with(|b| {
            if worktree {
                b.git.diff_version
            } else {
                b.git.revision_diff_version
            }
        })
    });
    let has_diff = computed(move || {
        let _ = diff_ver.get();
        controller::bench().with_untracked(|b| {
            let patch = if worktree {
                &b.git.diff
            } else {
                &b.git.revision_diff
            };
            !patch.is_empty()
        })
    });
    let split = computed(|| controller::bench().with(|b| b.git.side_by_side));
    let index = computed(|| controller::bench().with(|b| b.git.hunk));
    let colors = palette();
    let count = computed(move || {
        let _ = diff_ver.get();
        controller::bench().with_untracked(|b| {
            let patch = if worktree {
                &b.git.diff
            } else {
                &b.git.revision_diff
            };
            hunks(patch).1.len()
        })
    });
    let current = computed(move || index.get().min(count.get().saturating_sub(1)));
    let shown = computed(move || {
        let _ = diff_ver.get();
        let idx = current.get();
        controller::bench().with_untracked(|b| {
            let patch = if worktree {
                &b.git.diff
            } else {
                &b.git.revision_diff
            };
            let (header, blocks) = hunks(patch);
            let visible = match blocks.get(idx) {
                Some(block) => format!("{header}{block}"),
                None => patch.clone(),
            };
            let (clipped, truncated) = clip_patch(&visible);
            (SharedString::from(clipped), truncated)
        })
    });
    when(
        move || has_diff.get(),
        move || {
            view()
                .flex_1()
                .min_h_0()
                .min_w_0()
                .flex_col()
                .child(
                    view()
                        .flex_row()
                        .flex_wrap()
                        .items_center()
                        .gap_1p5()
                        .px_2p5()
                        .py_1p5()
                        .bg(colors.sidebar)
                        .border_b_1()
                        .border_b_color(colors.line_soft)
                        .child(
                            segments()
                                .child(segment(
                                    "统一",
                                    move || !split.get(),
                                    || controller::bench().update(|b| b.git.side_by_side = false),
                                ))
                                .child(segment(
                                    "并排",
                                    move || split.get(),
                                    || controller::bench().update(|b| b.git.side_by_side = true),
                                )),
                        )
                        .child(when(
                            move || count.get() > 1,
                            move || {
                                view()
                                    .flex_row()
                                    .items_center()
                                    .gap_0p5()
                                    .child(
                                        small_icon_button(Icon::ChevronUp, "上一块", move || {
                                            let next = current.get_untracked().saturating_sub(1);
                                            controller::bench().update(|b| b.git.hunk = next);
                                        })
                                        .disabled(move || current.get() == 0),
                                    )
                                    .child(
                                        text(move || {
                                            format!("{} / {}", current.get() + 1, count.get())
                                        })
                                        .text_size(px(11.0))
                                        .font_family(MONO)
                                        .text_color(colors.muted),
                                    )
                                    .child(
                                        small_icon_button(Icon::ChevronDown, "下一块", move || {
                                            let next = current.get_untracked() + 1;
                                            controller::bench().update(|b| b.git.hunk = next);
                                        })
                                        .disabled(move || current.get() + 1 >= count.get()),
                                    )
                            },
                        ))
                        .child(view().flex_1())
                        .child(when(
                            move || worktree && count.get() > 0,
                            || {
                                ghost_button(
                                    || {
                                        controller::bench().with(|b| {
                                            if b.git.selected.as_ref().is_some_and(|s| s.1) {
                                                "取消暂存此块"
                                            } else {
                                                "暂存此块"
                                            }
                                        })
                                    },
                                    controller::stage_hunk,
                                )
                                .disabled(|| controller::bench().with(|b| b.git.busy))
                            },
                        )),
                )
                .child(when(
                    move || shown.with(|(_, truncated)| *truncated),
                    move || {
                        text("当前差异块超过 180 KB，已截断显示以保持流畅。")
                            .px_2p5()
                            .py_1()
                            .bg(colors.approval)
                            .text_size(px(11.0))
                            .text_color(colors.warning)
                    },
                ))
                .child(
                    view().flex_1().min_w_0().min_h_0().bg(colors.code).child(
                        nsh_base::diff_view()
                            .patch(move || Some(shown.with(|(s, _)| s.clone())))
                            .layout(move || {
                                if split.get() {
                                    nsh_base::DiffLayout::Split
                                } else {
                                    nsh_base::DiffLayout::Unified
                                }
                            })
                            .size_full(),
                    ),
                )
        },
    )
}
