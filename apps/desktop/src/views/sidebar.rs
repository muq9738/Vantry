//! The Stardust sidebar (`v0.1.42` layout): transparent draggable top chrome,
//! compact search + filter / add-project / new-thread icon row, project-badged
//! thread cards with model + date subtitle, pinned archived drawer, and a
//! quiet Lucide icon-only footer.

use nsh_core::{
    element::desktop::{MenuItem, popup_menu, popup_menu_at, title_bar},
    prelude::*,
};

use super::{
    badge, caption, dot, icon_button, run_status_color, small_icon_button, status_color,
    status_label,
};
use crate::{
    controller,
    icons::{Icon, icon, project_badge},
    state::{
        ThreadStatus,
        sidebar::{
            Group, InboxItem, InboxKind, SidebarRow, SidebarSection, StatusFilter, project_name,
            project_section_key, task_badge,
        },
        ui::{MainView, SettingsPage, ThemePreference},
        workbench::DockTab,
    },
    theme::{metrics, palette},
};

/// A flattened entry: a group header or a row (archived is rendered in its own
/// dedicated bottom row just above the footer, matching `v0.1.42`).
#[derive(Debug, Clone, PartialEq, Eq)]
enum Entry {
    Header(Group, usize),
    Project {
        key: String,
        count: usize,
        folded: bool,
    },
    More(String, usize),
    Categories,
    Row(SidebarRow),
}

fn entries(sections: Vec<SidebarSection>) -> (Vec<Entry>, usize, Vec<SidebarRow>) {
    let mut out = Vec::new();
    let mut archived_count = 0;
    let mut archived_rows = Vec::new();
    let mut projects_started = false;
    for section in sections {
        if section.group == Group::Archived {
            archived_count = section.count;
            archived_rows = section.rows;
        } else if let Some(key) = section.project {
            if !projects_started && !out.is_empty() {
                out.push(Entry::Categories);
            }
            projects_started = true;
            out.push(Entry::Project {
                key: key.clone(),
                count: section.count,
                folded: section.folded,
            });
            out.extend(section.rows.into_iter().map(Entry::Row));
            if section.hidden > 0 {
                out.push(Entry::More(key, section.hidden));
            }
        } else {
            out.push(Entry::Header(section.group, section.count));
            out.extend(section.rows.into_iter().map(Entry::Row));
        }
    }
    (out, archived_count, archived_rows)
}

fn entry_key(entry: &Entry) -> String {
    match entry {
        Entry::Header(group, _) => format!("group:{group:?}"),
        Entry::Categories => "categories".to_owned(),
        Entry::Project { key, .. } => format!("project:{key}"),
        Entry::More(key, _) => format!("more:{key}"),
        Entry::Row(row) => format!("row:{}", row.id),
    }
}

pub(crate) fn sidebar() -> impl IntoElement {
    let colors = palette();
    let revision = computed(|| controller::state().with(|s| s.sidebar_revision));
    let filter = computed(|| controller::ui().with(|u| u.filter.clone()));
    let computed_list = computed(move || {
        revision.get();
        let filter = filter.get();
        controller::state()
            .with_untracked(|s| entries(s.sidebar_sections(&filter, chrono::Utc::now())))
    });
    let local_drafts = computed(move || {
        let filter = filter.get();
        controller::state().with(|s| {
            let project = crate::state::sidebar::workspace_key(s.new_thread.workspace.as_ref(), "");
            if filter.status != StatusFilter::All
                || filter.project.as_ref().is_some_and(|p| p != &project)
            {
                return Vec::new();
            }
            let query = filter.query.trim().to_lowercase();
            s.local_order
                .iter()
                .rev()
                .filter_map(|id| {
                    let title = s
                        .drafts
                        .get(id)
                        .map(|d| crate::state::title_from_message(&d.text))
                        .unwrap_or_else(|| "新会话".into());
                    (query.is_empty()
                        || title.to_lowercase().contains(&query)
                        || project.to_lowercase().contains(&query))
                    .then(|| (id.clone(), title))
                })
                .collect()
        })
    });
    let list = computed(move || computed_list.with(|(rows, _, _)| rows.clone()));
    let archived_count = computed(move || computed_list.with(|(_, count, _)| *count));
    let archived_rows = computed(move || computed_list.with(|(_, _, rows)| rows.clone()));
    let show_archived = computed(move || {
        filter.with(|f| {
            f.show_archived || !f.query.trim().is_empty() || f.status != StatusFilter::All
        })
    });
    let selected = computed(|| controller::state().with(|s| s.selected.clone()));
    let inbox = computed(move || {
        revision.get();
        controller::state().with_untracked(|s| s.inbox())
    });
    let query = computed(move || filter.with(|f| f.query.clone()));
    let filtered =
        computed(move || filter.with(|f| f.status != StatusFilter::All || f.project.is_some()));
    let search_ref = element_ref();
    let search_ref_fx = search_ref.clone();
    effect(move || {
        let nonce = controller::search_focus_nonce().get();
        if nonce > 0 {
            search_ref_fx.focus();
        }
    });

    view()
        .w(|| px(metrics::sidebar_width(nsh_core::element::window().width.0)))
        .flex_shrink_0()
        .flex_col()
        .bg(colors.sidebar)
        .border_r_1()
        .border_r_color(colors.line)
        // Top title bar: integrates macOS traffic lights cleanly into the sidebar.
        .child(
            view()
                .h(px(metrics::HEADER_HEIGHT))
                .flex_row()
                .items_center()
                .gap_1p5()
                .pl(move || {
                    let inset = title_bar().left.0;
                    px(if inset > 0.0 { inset + 6.0 } else { 12.0 })
                })
                .pr_3()
                .window_drag(true)
                .child(icon_button(
                    Icon::PanelLeft,
                    "收起侧栏",
                    controller::toggle_sidebar,
                ))
                .child(
                    text("Vantry")
                        .flex_1()
                        .text_size(px(14.5))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.ink),
                )
                .child(when(
                    move || inbox.with(|items| !items.is_empty()),
                    move || {
                        nsh_base::button()
                            .accessibility_label("待处理收件箱")
                            .on_press(move || inbox_menu(inbox.get_untracked()))
                            .flex_row()
                            .items_center()
                            .gap_1()
                            .px_2()
                            .h(px(24.0))
                            .rounded_full()
                            .bg(colors.soft)
                            .child(icon(Icon::Inbox, 13.0, colors.warning))
                            .child(
                                text(move || inbox.with(|items| items.len().to_string()))
                                    .text_size(px(11.0))
                                    .text_color(colors.ink),
                            )
                    },
                )),
        )
        // Search box + compact icon actions (filter, add project, new thread)
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .px_3()
                .pb_2()
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
                                .element_ref(&search_ref)
                                .flex_1()
                                .value(move || query.get())
                                .on_change(|text: &str| controller::set_query(text))
                                .placeholder("搜索项目、会话")
                                .placeholder_color(move || color(colors.faint))
                                .auto_correct(false)
                                .text_size(px(12.0))
                                .text_color(colors.ink),
                        )
                        .child(when(
                            move || query.with(|q| q.is_empty()),
                            || text("⌘K").text_size(px(10.0)).text_color(colors.faint),
                        )),
                )
                .child(
                    nsh_base::button()
                        .accessibility_label("按项目或状态筛选")
                        .on_press(filter_menu)
                        .w(px(26.0))
                        .h(px(26.0))
                        .items_center()
                        .justify_center()
                        .rounded(px(6.0))
                        .hover(|s| s.bg(colors.soft))
                        .active(|s| s.opacity(0.65))
                        .child(icon(Icon::ListFilter, 14.0, move || {
                            if filtered.get() {
                                colors.accent
                            } else {
                                colors.muted
                            }
                        })),
                )
                .child(
                    icon_button(Icon::FolderPlus, "添加项目", controller::choose_project)
                        .disabled(|| controller::ui().with(|u| u.remote_target.is_some())),
                )
                .child(icon_button(
                    Icon::SquarePen,
                    "新建会话 (⌘N)",
                    controller::new_thread,
                )),
        )
        // Scrollable thread list
        .child(
            scroll().flex_1().child(
                view()
                    .flex_col()
                    .gap_1()
                    .px_2p5()
                    .pt_1()
                    .pb_3()
                    .child(each(
                        move || local_drafts.get(),
                        |(id, _)| id.clone(),
                        move |item, _| {
                            let id = move || item.with_untracked(|v| v.0.clone());
                            let selected = computed(move || {
                                controller::state().with(|s| {
                                    s.selected.is_none()
                                        && s.local_selected.as_deref() == Some(&id())
                                })
                            });
                            let hovered = signal(false);
                            view()
                                .relative()
                                .h(px(52.0))
                                .rounded(px(10.0))
                                .border_1()
                                .bg(move || {
                                    if selected.get() {
                                        colors.card
                                    } else {
                                        AdaptiveColor::TRANSPARENT
                                    }
                                })
                                .border_color(move || {
                                    if selected.get() {
                                        colors.line
                                    } else {
                                        AdaptiveColor::TRANSPARENT
                                    }
                                })
                                .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
                                .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
                                .child(
                                    nsh_base::button()
                                        .on_press(move || controller::select_thread(id()))
                                        .selected(move || selected.get())
                                        .h_full()
                                        .px_3()
                                        .justify_center()
                                        .gap_1()
                                        .child(
                                            text(move || item.with(|v| v.1.clone()))
                                                .number_of_lines(1u32)
                                                .text_size(px(13.0))
                                                .font_weight(FontWeight::MEDIUM)
                                                .text_color(colors.ink),
                                        )
                                        .child(
                                            view()
                                                .flex_row()
                                                .items_center()
                                                .gap_1()
                                                .child(icon(Icon::Sparkles, 11.0, colors.faint))
                                                .child(
                                                    text(|| {
                                                        controller::state().with(|s| {
                                                            let model = &s.new_thread.model;
                                                            if model.is_empty() {
                                                                "默认模型".to_owned()
                                                            } else {
                                                                model
                                                                    .rsplit('/')
                                                                    .next()
                                                                    .unwrap_or(model)
                                                                    .to_owned()
                                                            }
                                                        })
                                                    })
                                                    .text_size(px(11.0))
                                                    .text_color(colors.faint),
                                                ),
                                        ),
                                )
                                .child(when(
                                    move || hovered.get(),
                                    move || {
                                        view().absolute().top_1().right_1().child(
                                            small_icon_button(Icon::X, "丢弃草稿", move || {
                                                controller::state()
                                                    .update(|s| s.discard_local_draft(&id()))
                                            }),
                                        )
                                    },
                                ))
                        },
                    ))
                    .child(each(
                        move || list.get(),
                        entry_key,
                        move |entry, _| entry_view(entry, selected),
                    ))
                    .child(when(
                        move || list.with(Vec::is_empty) && local_drafts.with(Vec::is_empty),
                        move || {
                            view()
                                .flex_col()
                                .items_center()
                                .gap_1p5()
                                .px_3()
                                .py_8()
                                .child(icon(Icon::Sparkles, 18.0, colors.faint))
                                .child(
                                    text(move || {
                                        if query.with(|q| q.trim().is_empty()) && !filtered.get() {
                                            "还没有会话，写下第一条消息即可开始"
                                        } else {
                                            "没有匹配的会话"
                                        }
                                    })
                                    .text_size(px(12.0))
                                    .text_color(colors.faint),
                                )
                        },
                    )),
            ),
        )
        // Dedicated archived bar right above the footer (matches v0.1.42)
        .child(archived_drawer(
            archived_count,
            show_archived,
            archived_rows,
            selected,
        ))
        // Minimal Lucide icon-only footer (matches v0.1.42)
        .child(footer())
}

fn filter_menu() {
    let filter = controller::ui().with_untracked(|u| u.filter.clone());
    let mut options = vec![
        MenuItem::new("全部项目")
            .checked(filter.project.is_none())
            .on_select(|| {
                controller::ui().update(|u| u.filter.project = None);
            }),
    ];
    let keys = controller::state().with_untracked(|s| s.project_keys());
    for key in &keys {
        let name = project_name(key);
        let label = if keys.iter().any(|k| k != key && project_name(k) == name) {
            key.as_str()
        } else {
            name
        };
        let selected = filter.project.as_ref() == Some(key);
        let key = key.clone();
        options.push(MenuItem::new(label).checked(selected).on_select(move || {
            controller::ui().update(|u| u.filter.project = Some(key.clone()));
        }));
    }
    options.push(MenuItem::separator());
    for status in [
        StatusFilter::All,
        StatusFilter::Running,
        StatusFilter::Attention,
    ] {
        options.push(
            MenuItem::new(status.label())
                .checked(filter.status == status)
                .on_select(move || controller::set_status_filter(status)),
        );
    }
    drop(popup_menu(options));
}

fn inbox_menu(items: Vec<InboxItem>) {
    let mut options = Vec::new();
    if items.iter().any(|item| item.kind != InboxKind::Approval) {
        options.push(MenuItem::new("全部已读").on_select(|| {
            controller::state().update(|s| {
                s.unread.clear();
                s.sidebar_revision += 1;
            });
        }));
        options.push(MenuItem::separator());
    }
    for item in items {
        let summary = item
            .summary
            .lines()
            .next()
            .unwrap_or("")
            .chars()
            .take(64)
            .collect::<String>();
        let label = format!("{} · {}  {}", item.kind.label(), item.title, summary);
        options.push(MenuItem::new(label).on_select(move || {
            controller::select_thread(item.thread.clone());
            if let Some(id) = &item.request {
                controller::state().update(|s| {
                    s.deferred.remove(id);
                });
            }
        }));
    }
    drop(popup_menu(options));
}

fn entry_view(entry: ReadSignal<Entry>, selected: Memo<Option<String>>) -> AnyElement {
    let colors = palette();
    match entry.get_untracked() {
        Entry::Categories => caption("分类").px_1p5().pt_2().into_element(),
        Entry::Header(group, _) => view()
            .flex_row()
            .items_center()
            .gap_1p5()
            .px_1p5()
            .h(px(26.0))
            .child(caption(group.label()))
            .child(
                text(move || {
                    entry.with(|e| match e {
                        Entry::Header(_, n) => n.to_string(),
                        _ => String::new(),
                    })
                })
                .text_size(px(11.0))
                .text_color(if group == Group::Attention {
                    colors.warning
                } else {
                    colors.faint
                }),
            )
            .into_element(),
        Entry::Project { key, .. } => {
            let folded =
                computed(move || entry.with(|e| matches!(e, Entry::Project { folded: true, .. })));
            let name = project_name(&key).to_owned();
            let glyph = if key.is_empty() {
                icon(Icon::Folder, 15.0, colors.faint).into_element()
            } else {
                project_badge(&name).into_element()
            };
            nsh_base::button()
                .accessibility_label(name.clone())
                .on_press(move || {
                    controller::ui().update(|u| {
                        u.filter
                            .project_open
                            .insert(project_section_key(&key).to_owned(), folded.get_untracked());
                    })
                })
                .flex_row()
                .items_center()
                .gap_1p5()
                .px_1p5()
                .h(px(28.0))
                .rounded(px(7.0))
                .hover(|s| s.bg(colors.soft))
                .child(dynamic(move || {
                    icon(
                        if folded.get() {
                            Icon::ChevronRight
                        } else {
                            Icon::ChevronDown
                        },
                        11.0,
                        colors.faint,
                    )
                }))
                .child(glyph)
                .child(
                    text(name)
                        .flex_1()
                        .number_of_lines(1u32)
                        .text_size(px(12.0))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.ink),
                )
                .child(caption(move || {
                    entry.with(|e| match e {
                        Entry::Project { count, .. } => count.to_string(),
                        _ => String::new(),
                    })
                }))
                .into_element()
        }
        Entry::More(key, _) => nsh_base::button()
            .on_press(move || {
                controller::ui().update(|u| {
                    u.filter.show_all.insert(key.clone());
                })
            })
            .pl(px(28.0))
            .h(px(24.0))
            .justify_center()
            .child(caption(move || {
                entry.with(|e| match e {
                    Entry::More(_, n) => format!("更多 {n}"),
                    _ => String::new(),
                })
            }))
            .into_element(),
        Entry::Row(_) => {
            let row = computed(move || match entry.get() {
                Entry::Row(row) => Some(row),
                _ => None,
            });
            thread_row(row, selected).into_element()
        }
    }
}

fn thread_menu(row: &SidebarRow) -> Vec<MenuItem> {
    let action = |label: &'static str, apply: fn(String)| {
        let id = row.id.clone();
        MenuItem::new(label).on_select(move || apply(id.clone()))
    };
    let until = if row.snoozed { None } else { Some(24) };
    let snooze_id = row.id.clone();
    vec![
        action("重命名", controller::begin_rename),
        action(
            if row.pinned { "取消置顶" } else { "置顶" },
            controller::toggle_pin,
        ),
        MenuItem::new(if row.snoozed {
            "唤醒"
        } else {
            "延后 1 天"
        })
        .on_select(move || controller::snooze(snooze_id.clone(), until)),
        action(
            if row.archived {
                "取消归档"
            } else {
                "归档"
            },
            controller::toggle_archive,
        ),
        MenuItem::separator(),
        action("删除会话", controller::ask_delete),
    ]
}

fn thread_row(row: Memo<Option<SidebarRow>>, selected: Memo<Option<String>>) -> impl IntoElement {
    let colors = palette();
    let id = move || row.with_untracked(|r| r.as_ref().map(|r| r.id.clone()).unwrap_or_default());
    let is_selected = computed(move || {
        row.with(|r| {
            r.as_ref()
                .is_some_and(|r| selected.with(|s| s.as_ref() == Some(&r.id)))
        })
    });
    let status = move || row.with(|r| r.as_ref().map_or(ThreadStatus::Idle, |r| r.status));
    let task = move || row.with(|r| r.as_ref().and_then(|r| r.task_status));
    let children = move || row.with(|r| r.as_ref().map_or(0, |r| r.children));
    let collapsed = move || row.with(|r| r.as_ref().is_some_and(|r| r.collapsed));
    let label = move |field: fn(&SidebarRow) -> &str| {
        move || row.with(|r| r.as_ref().map_or("", field).to_owned())
    };
    let detail = computed(move || {
        row.with(|r| match r {
            Some(r) if !r.workspace_name.is_empty() => (Icon::Folder, r.workspace_name.clone()),
            Some(r) if !r.branch.is_empty() => (Icon::GitBranch, r.branch.clone()),
            Some(r) if !r.model.is_empty() => (Icon::Sparkles, r.model.clone()),
            _ => (Icon::Sparkles, "默认模型".to_owned()),
        })
    });
    let hovered = signal(false);
    let background = move || {
        if is_selected.get() {
            colors.card
        } else if hovered.get() {
            colors.soft
        } else {
            AdaptiveColor::TRANSPARENT
        }
    };
    view()
        .relative()
        .h(px(52.0))
        .flex_shrink_0()
        .flex_row()
        .items_center()
        .pl(move || px(8.0 + row.with(|r| r.as_ref().map_or(0.0, |r| f32::from(r.depth) * 14.0))))
        .pr_2()
        .rounded(px(10.0))
        .bg(background)
        .border_1()
        .border_color(move || {
            if is_selected.get() {
                colors.line
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
        .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
        .on_context_menu(move |event: &nsh_core::ContextMenuEvent| {
            if let Some(row) = row.get_untracked() {
                drop(popup_menu_at(thread_menu(&row), event.page_position));
            }
        })
        .child(when(
            move || children() > 0,
            move || {
                nsh_base::button()
                    .accessibility_label("展开或折叠子任务")
                    .on_press(move || controller::toggle_collapsed(id()))
                    .w(px(16.0))
                    .h(px(36.0))
                    .items_center()
                    .justify_center()
                    .child(dynamic(move || {
                        icon(
                            if collapsed() {
                                Icon::ChevronRight
                            } else {
                                Icon::ChevronDown
                            },
                            11.0,
                            colors.faint,
                        )
                    }))
            },
        ))
        .child(
            nsh_base::button()
                .flex_1()
                .min_w_0()
                .h_full()
                .justify_center()
                .gap_1()
                .accessibility_label(move || {
                    row.with(|r| r.as_ref().map(|r| SharedString::from(r.title.clone())))
                })
                .on_press(move || controller::select_thread(id()))
                .selected(move || is_selected.get())
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_1p5()
                        .child(
                            view()
                                .w(px(14.0))
                                .h(px(14.0))
                                .items_center()
                                .justify_center()
                                .child(when(
                                    move || status() != ThreadStatus::Idle,
                                    move || dot(move || status_color(status())),
                                )),
                        )
                        .child(
                            text(label(|r| &r.title))
                                .flex_1()
                                .min_w_0()
                                .number_of_lines(1u32)
                                .text_size(px(13.0))
                                .font_weight(FontWeight::MEDIUM)
                                .text_color(colors.ink),
                        )
                        .child(when(
                            move || !hovered.get(),
                            move || {
                                text(label(|r| &r.date_label))
                                    .text_size(px(11.0))
                                    .text_color(colors.faint)
                            },
                        )),
                )
                .child(
                    view()
                        .pl(px(20.0))
                        .flex_row()
                        .items_center()
                        .gap_1()
                        .child(when(
                            move || task().is_some() || status() != ThreadStatus::Idle,
                            move || {
                                text(move || {
                                    task()
                                        .map(task_badge)
                                        .unwrap_or_else(|| status_label(status()))
                                })
                                .text_size(px(11.0))
                                .text_color(move || {
                                    task()
                                        .map(run_status_color)
                                        .unwrap_or_else(|| status_color(status()))
                                })
                            },
                        ))
                        .child(dynamic(move || {
                            icon(detail.with(|d| d.0), 11.0, colors.faint)
                        }))
                        .child(
                            text(move || detail.with(|d| d.1.clone()))
                                .flex_1()
                                .number_of_lines(1u32)
                                .text_size(px(11.0))
                                .text_color(colors.faint),
                        )
                        .child(when(
                            move || children() > 0 && collapsed(),
                            move || badge(move || format!("{}", children()), colors.muted),
                        ))
                        .child(when(
                            move || row.with(|r| r.as_ref().is_some_and(|r| r.pinned)),
                            move || icon(Icon::Pin, 10.5, colors.faint),
                        ))
                        .child(when(
                            move || row.with(|r| r.as_ref().is_some_and(|r| r.pi)),
                            move || text("π").text_size(px(11.0)).text_color(colors.faint),
                        )),
                ),
        )
        .child(when(
            move || hovered.get(),
            move || {
                view()
                    .absolute()
                    .right_1()
                    .top_1()
                    .flex_row()
                    .rounded(px(6.0))
                    .bg(background)
                    .child(small_icon_button(
                        Icon::Archive,
                        "归档或移回会话",
                        move || controller::toggle_archive(id()),
                    ))
                    .child(small_icon_button(
                        Icon::Ellipsis,
                        "更多会话操作",
                        move || {
                            if let Some(row) = row.get_untracked() {
                                drop(popup_menu(thread_menu(&row)));
                            }
                        },
                    ))
            },
        ))
}

fn archived_drawer(
    count: Memo<usize>,
    open: Memo<bool>,
    rows: Memo<Vec<SidebarRow>>,
    selected: Memo<Option<String>>,
) -> impl IntoElement {
    let colors = palette();
    view()
        .flex_col()
        .border_t_1()
        .border_t_color(colors.line_soft)
        .child(
            nsh_base::button()
                .accessibility_label("已归档会话")
                .on_press(controller::toggle_archived_group)
                .w_full()
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .py_2()
                .hover(|s| s.bg(colors.soft))
                .active(|s| s.opacity(0.7))
                .child(icon(Icon::Archive, 14.0, colors.muted))
                .child(
                    text("已归档")
                        .flex_1()
                        .text_size(px(12.5))
                        .text_color(colors.muted),
                )
                .child(
                    view()
                        .px_1p5()
                        .py_0p5()
                        .rounded_full()
                        .bg(colors.soft)
                        .child(
                            text(move || format!("{}", count.get()))
                                .text_size(px(10.5))
                                .text_color(colors.muted),
                        ),
                )
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
            move || open.get() && rows.with(|r| !r.is_empty()),
            move || {
                scroll()
                    .max_h(px(160.0))
                    .child(view().flex_col().gap_0p5().px_2p5().pb_2().child(each(
                        move || rows.get(),
                        |r: &SidebarRow| r.id.clone(),
                        move |row, _| {
                            let memo = computed(move || Some(row.get()));
                            thread_row(memo, selected)
                        },
                    )))
            },
        ))
}

fn footer() -> impl IntoElement {
    let colors = palette();
    let theme = computed(|| controller::ui().with(|u| u.theme));
    let remote = computed(|| controller::ui().with(|u| u.view == MainView::RemoteDesktop));
    view()
        .flex_row()
        .items_center()
        .gap_1()
        .px_2p5()
        .py_2()
        .border_t_1()
        .border_t_color(colors.line_soft)
        .child(icon_button(Icon::Settings, "设置", || {
            controller::open_settings(SettingsPage::General);
        }))
        .child(icon_button(Icon::GitBranch, "Git 工作台", || {
            controller::toggle_dock(DockTab::Git);
        }))
        .child(icon_button(Icon::ChartBar, "用量统计", || {
            controller::open_settings(SettingsPage::Usage);
        }))
        .child(icon_button(Icon::Monitor, "远程桌面", move || {
            controller::show_view(if remote.get_untracked() {
                MainView::Conversation
            } else {
                MainView::RemoteDesktop
            });
        }))
        .child(view().flex_1())
        .child(
            nsh_base::button()
                .accessibility_label("切换明暗主题")
                .on_press(controller::cycle_theme)
                .w(px(28.0))
                .h(px(28.0))
                .items_center()
                .justify_center()
                .rounded(px(7.0))
                .hover(|s| s.bg(colors.soft))
                .active(|s| s.opacity(0.65))
                .child(dynamic(move || {
                    let glyph = match theme.get() {
                        ThemePreference::Dark => Icon::Sun,
                        ThemePreference::Light | ThemePreference::System => Icon::Moon,
                    };
                    icon(glyph, 15.0, colors.muted).into_element()
                })),
        )
}
