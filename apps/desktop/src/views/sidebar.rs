//! The Stardust sidebar (`v0.1.42` layout): transparent draggable top chrome,
//! compact search + filter / add-project / new-thread icon row, project-badged
//! thread cards with model + date subtitle, pinned archived drawer, and a
//! quiet Lucide icon-only footer.

use nsh_core::{
    element::desktop::{MenuItem, current_window, popup_menu, popup_menu_at},
    prelude::*,
};

use super::{
    caption, dot, icon_button, small_icon_button, status_color,
    status_label,
};
use crate::{
    controller,
    icons::{Icon, icon, project_badge},
    state::{
        ThreadStatus,
        sidebar::{
            Group, InboxItem, InboxKind, SidebarRow, SidebarSection, StatusFilter, project_name,
            project_section_key, scoped_project,
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
        machine: String,
        key: String,
        count: usize,
        folded: bool,
    },
    More(String, String, usize),
    Categories,
    Row(SidebarRow),
}

fn entries(sections: Vec<(String, SidebarSection)>) -> (Vec<Entry>, usize, Vec<SidebarRow>) {
    let mut out = Vec::new();
    let mut archived_count = 0;
    let mut archived_rows = Vec::new();
    let mut projects_started = false;
    for (machine, section) in sections {
        if section.group == Group::Archived {
            archived_count += section.count;
            archived_rows.extend(section.rows);
        } else if let Some(key) = section.project {
            if !projects_started && !out.is_empty() {
                out.push(Entry::Categories);
            }
            projects_started = true;
            out.push(Entry::Project {
                machine: machine.clone(),
                key: key.clone(),
                count: section.count,
                folded: section.folded,
            });
            out.extend(section.rows.into_iter().map(Entry::Row));
            if section.hidden > 0 {
                out.push(Entry::More(machine, key, section.hidden));
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
        Entry::Project { machine, key, .. } => format!("project:{machine}:{key}"),
        Entry::More(machine, key, _) => format!("more:{machine}:{key}"),
        Entry::Row(row) => format!("row:{}:{}", row.machine, row.id),
    }
}

pub(crate) fn sidebar() -> impl IntoElement {
    let window = current_window();
    let colors = palette();
    let revision = computed(|| controller::state().with(|s| s.sidebar_revision));
    let filter = computed(|| controller::ui().with(|u| u.filter.clone()));
    let computed_list = computed(move || {
        let filter = filter.get();
        let only = controller::machines::navigation().with(|n| n.filter.clone());
        let mut groups = std::collections::BTreeMap::new();
        for host in controller::machines::records().get() {
            if only.as_ref().is_some_and(|id| id != &host.id) { continue; }
            let Some(filter) = filter.for_machine(&host.id) else { continue };
            host.revision.get();
            let sections = host.session.state.with_untracked(|s| s.sidebar_sections(&filter, chrono::Utc::now()));
            for mut section in sections {
                for row in &mut section.rows { row.machine.clone_from(&host.id); }
                let scope = if section.project.is_some() { host.id.clone() } else { String::new() };
                let key = (section.group, scope.clone(), section.project.clone());
                groups.entry(key).and_modify(|(_, prev): &mut (String, SidebarSection)| {
                    prev.count += section.count;
                    prev.latest = prev.latest.max(section.latest);
                    prev.rows.append(&mut section.rows);
                }).or_insert((scope, section));
            }
        }
        let mut sections: Vec<_> = groups.into_values().collect();
        sections.sort_by_key(|(_, s)| (s.group, s.project.as_deref() == Some(""), std::cmp::Reverse(s.latest)));
        for (_, section) in &mut sections {
            section.rows.sort_by_key(|r| std::cmp::Reverse(r.latest));
        }
        entries(sections)
    });
    let local_drafts = computed(move || {
        let filter = filter.get();
        let only = controller::machines::navigation().with(|n| n.filter.clone());
        let mut drafts = Vec::new();
        for host in controller::machines::records().get() {
            if only.as_ref().is_some_and(|id| id != &host.id) { continue; }
            let Some(filter) = filter.for_machine(&host.id) else { continue };
            drafts.extend(host.session.state.with(|s| {
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
                    let draft = s.drafts.get(id)?;
                    // Never show an empty placeholder "新会话" card above the project list.
                    if draft.text.trim().is_empty() && draft.attachments.is_empty() {
                        return None;
                    }
                    let title = draft.sidebar_title();
                    (query.is_empty()
                        || title.to_lowercase().contains(&query)
                        || project.to_lowercase().contains(&query))
                    .then(|| (host.id.clone(), id.clone(), title))
                })
                .collect()
        }));
        }
        drafts
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
        .w(|| px(metrics::sidebar_width(crate::window_size().width.0)))
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
                    let inset = window.title_bar().left.0;
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
                    icon_button(Icon::FolderPlus, "添加项目", || controller::machines::activate("", controller::choose_project))
                        .disabled(|| !controller::machines::draft_target().is_empty()),
                )
                .child(icon_button(
                    Icon::SquarePen,
                    "新建会话 (⌘N)",
                    controller::machines::new_draft,
                ).disabled(|| !controller::machines::can_new_draft())),
        )
        // The machine filter belongs to navigation, independently of the open conversation.
        .child(machine_row())
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
                        |(machine, id, _)| (machine.clone(), id.clone()),
                        move |item, _| {
                            let target = move || item.with_untracked(|v| (v.0.clone(),v.1.clone()));
                            let active_scope = controller::current_machine().unwrap_or_default();
                            let selected = computed(move || {
                                controller::state().with(|s| {
                                    let (scope,id) = target();
                                    active_scope == scope && s.selected.is_none() && s.local_selected.as_ref() == Some(&id)
                                })
                            });
                            let hovered = signal(false);
                            view()
                                .relative()
                                .h(px(44.0))
                                .rounded(px(8.0))
                                .bg(move || {
                                    if selected.get() {
                                        colors.card
                                    } else if hovered.get() {
                                        colors.soft
                                    } else {
                                        AdaptiveColor::TRANSPARENT
                                    }
                                })
                                .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
                                .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
                                .child(
                                    nsh_base::button()
                                        .on_press(move || { let (scope,id) = target(); controller::machines::activate(&scope, || controller::select_thread(id)); })
                                        .selected(move || selected.get())
                                        .h_full()
                                        .px_2p5()
                                        .justify_center()
                                        .gap_0p5()
                                        .child(
                                            view()
                                                .flex_row()
                                                .items_center()
                                                .gap_1p5()
                                                .child(dot(colors.accent))
                                                .child(
                                                    text(move || item.with(|v| v.2.clone()))
                                                        .flex_1()
                                                        .number_of_lines(1u32)
                                                        .text_size(px(12.5))
                                                        .font_weight(FontWeight::MEDIUM)
                                                        .text_color(colors.ink),
                                                ),
                                        )
                                        .child(
                                            text("草稿")
                                                .pl(px(13.0))
                                                .text_size(px(10.5))
                                                .text_color(colors.faint),
                                        ),
                                )
                                .child(when(
                                    move || hovered.get(),
                                    move || {
                                        view()
                                            .absolute()
                                            .right_1()
                                            .top(px(10.0))
                                            .child(small_icon_button(Icon::X, "丢弃草稿", move || {
                                                let (scope,id) = target();
                                                controller::machines::run_on(&scope, || controller::state().update(|s| s.discard_local_draft(&id)))
                                            }))
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
                                            "暂无会话"
                                        } else {
                                            "无匹配会话"
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

/// Filter the shared session list or manage a pairing.
fn machine_row() -> impl IntoElement {
    use controller::machines::{by_id, navigation, records};
    let colors = palette();
    let filter = computed(|| navigation().with(|n| n.filter.clone()));
    let label = move || {
        filter
            .get()
            .and_then(|id| {
                records().with(|list| list.iter().find(|r| r.id == id).map(|r| r.name.clone()))
            })
            .unwrap_or_else(|| crate::tr("全部机器").into())
    };
    let failure = computed(move || {
        filter
            .get()
            .and_then(|id| by_id(&id))
            .map(|m| m.failure.get())
            .unwrap_or_default()
    });
    view()
        .flex_col()
        .px_3()
        .pb_1p5()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .child(
                    nsh_base::button()
                        .accessibility_label(crate::tr("筛选机器"))
                        .on_press(machine_menu)
                        .flex_1()
                        .min_w_0()
                        .h(px(26.0))
                        .flex_row()
                        .items_center()
                        .gap_1p5()
                        .px_2()
                        .rounded(px(6.))
                        .hover(|s| s.bg(colors.soft))
                        .child(icon(Icon::Server, 12.5, colors.muted))
                        .child(
                            text(label)
                                .min_w_0()
                                .number_of_lines(1u32)
                                .text_size(px(12.))
                                .text_color(colors.muted),
                        )
                        .child(icon(Icon::ChevronDown, 11., colors.faint)),
                )
                .child(small_icon_button(
                    Icon::Plus,
                    "连接另一台机器",
                    || {
                        controller::machines::activate("", || {
                            controller::open_settings(SettingsPage::Remote)
                        })
                    },
                )),
        )
        .child(when(
            move || !failure.with(String::is_empty),
            move || {
                nsh_base::button()
                    .accessibility_label("重试连接")
                    .on_press(move || {
                        if let Some(m) = filter.get_untracked().and_then(|id| by_id(&id)) {
                            controller::machines::refresh_summary(m);
                        }
                    })
                    .child(
                        text("未连接 · 重试")
                            .tooltip(super::tip(move || failure.get()))
                            .text_size(px(11.))
                            .text_color(colors.warning),
                    )
            },
        ))
}

fn machine_menu() {
    use controller::machines::{navigation, records};
    let current = navigation().with_untracked(|n| n.filter.clone());
    let mut items = vec![
        MenuItem::new(crate::tr("全部机器"))
            .checked(current.is_none())
            .on_select(|| navigation().update(|n| n.filter = None)),
    ];
    for m in records().get_untracked() {
        items.push(
            MenuItem::new(m.name.clone())
                .checked(current.as_ref() == Some(&m.id))
                .on_select(move || {
                    navigation().update(|n| n.filter = Some(m.id.clone()));
                    controller::machines::refresh_summary(m.clone());
                }),
        );
    }
    items.push(MenuItem::separator());
    items.push(MenuItem::new(crate::tr("管理机器连接…")).on_select(|| {
        controller::machines::activate("", || controller::open_settings(SettingsPage::Remote))
    }));
    drop(popup_menu(items));
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
    let only = controller::machines::navigation().with_untracked(|n| n.filter.clone());
    for host in controller::machines::records().get_untracked() {
        if only.as_ref().is_some_and(|id| id != &host.id) {
            continue;
        }
        let keys = host.session.state.with_untracked(|s| s.project_keys());
        for path in &keys {
            let key = scoped_project(&host.id, path);
            let selected = filter.project.as_ref() == Some(&key);
            let short = project_name(path);
            let name = if keys.iter().any(|p| p != path && project_name(p) == short) {
                path.as_str()
            } else {
                short
            };
            let label = if host.id.is_empty() {
                name.to_owned()
            } else {
                format!("{name} · {}", host.name)
            };
            options.push(MenuItem::new(label).checked(selected).on_select(move || {
                controller::ui().update(|u| u.filter.project = Some(key.clone()))
            }));
        }
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
        options.push(MenuItem::new("全部已读").on_select(controller::mark_all_read));
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
        Entry::Project { machine, key, .. } => {
            let folded =
                computed(move || entry.with(|e| matches!(e, Entry::Project { folded: true, .. })));
            let hovered = signal(false);
            let name = if machine.is_empty() { project_name(&key).to_owned() } else { format!("{} · {}", project_name(&key), controller::machines::by_id(&machine).map(|m| m.name).unwrap_or_default()) };
            let menu_machine = machine.clone();
            let toggle_key = scoped_project(&machine, project_section_key(&key));
            let add_key = key.clone();
            let ctx_key = key.clone();
            let glyph = if key.is_empty() {
                icon(Icon::Folder, 14.0, colors.faint).into_element()
            } else {
                project_badge(&name).into_element()
            };
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .px_1p5()
                .mt_1()
                .h(px(28.0))
                .rounded(px(7.0))
                .bg(move || {
                    if hovered.get() {
                        colors.soft
                    } else {
                        AdaptiveColor::TRANSPARENT
                    }
                })
                .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
                .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
                .on_context_menu(move |event: &nsh_core::ContextMenuEvent| {
                    let k = ctx_key.clone();
                    let k_new = k.clone();
                    let scope = menu_machine.clone();
                    let k_copy = k.clone();
                    let k_filter = scoped_project(&scope, &k);
                    let filtered_this =
                        controller::ui().with_untracked(|u| u.filter.project.as_deref() == Some(k_filter.as_str()));
                    let mut items = vec![
                        MenuItem::new("在此项目新建会话")
                            .on_select(move || controller::machines::activate(&scope, || controller::new_thread_in_project(k_new.clone()))),
                    ];
                    if !k.is_empty() {
                        items.push(
                            MenuItem::new("复制路径")
                                .on_select(move || controller::copy(k_copy.clone())),
                        );
                        items.push(
                            MenuItem::new(if filtered_this {
                                "取消项目筛选"
                            } else {
                                "仅看此项目"
                            })
                            .on_select(move || {
                                controller::ui().update(|u| {
                                    u.filter.project = if filtered_this {
                                        None
                                    } else {
                                        Some(k_filter.clone())
                                    };
                                });
                            }),
                        );
                    }
                    drop(popup_menu_at(items, event.page_position));
                })
                .child(
                    nsh_base::button()
                        .accessibility_label(name.clone())
                        .on_press(move || {
                            controller::ui().update(|u| {
                                u.filter.project_open.insert(
                                    toggle_key.clone(),
                                    folded.get_untracked(),
                                );
                            })
                        })
                        .flex_1()
                        .min_w_0()
                        .h_full()
                        .flex_row()
                        .items_center()
                        .gap_1p5()
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
                                .min_w_0()
                                .number_of_lines(1u32)
                                .text_size(px(12.0))
                                .font_weight(FontWeight::MEDIUM)
                                .text_color(colors.muted),
                        ),
                )
                .child(when(
                    move || !hovered.get(),
                    move || {
                        caption(move || {
                            entry.with(|e| match e {
                                Entry::Project { count, .. } => count.to_string(),
                                _ => String::new(),
                            })
                        })
                        .px_1()
                    },
                ))
                .child(when(
                    move || hovered.get(),
                    move || {
                        let k = add_key.clone();
                        let scope = machine.clone();
                        view().tooltip(super::tip(crate::tr("在此项目新建会话"))).child(
                            small_icon_button(Icon::Plus, "在此项目新建会话", move || {
                                controller::machines::activate(&scope, || controller::new_thread_in_project(k.clone()));
                            }),
                        )
                    },
                ))
                .into_element()
        }
        Entry::More(machine, key, _) => nsh_base::button()
            .on_press(move || {
                controller::ui().update(|u| {
                    u.filter.show_all.insert(scoped_project(&machine, &key));
                })
            })
            .pl(px(28.0))
            .h(px(24.0))
            .justify_center()
            .child(caption(move || {
                entry.with(|e| match e {
                    Entry::More(_, _, n) => format!("更多 {n}"),
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
    let navigate = |label: &'static str, apply: fn(String)| {
        let (scope, id) = (row.machine.clone(), row.id.clone());
        MenuItem::new(label).on_select(move || controller::machines::activate(&scope, || apply(id.clone())))
    };
    let mut items = Vec::new();
    // Primary creation / branching actions (restoring full MyGo parity)
    if row.has_completed_run {
        let fork_id = row.id.clone();
        let scope = row.machine.clone();
        items.push(MenuItem::new("从当前会话分叉新会话").on_select(move || {
            controller::machines::activate(&scope, || controller::fork(fork_id.clone(), None));
        }));
    }
    if !row.project_key.is_empty() {
        items.push(navigate("在此项目新建会话", controller::new_thread_in_thread_project));
    }
    if !items.is_empty() {
        items.push(MenuItem::separator());
    }
    items.push(action(
        if row.pinned { "取消置顶" } else { "置顶" },
        controller::toggle_pin,
    ));
    items.push(action(
        if row.archived { "移回活跃" } else { "归档" },
        controller::toggle_archive,
    ));
    if row.snoozed {
        let snooze_id = row.id.clone();
        items.push(MenuItem::new("唤醒").on_select(move || controller::snooze(snooze_id.clone(), None)));
    } else {
        let s1 = row.id.clone();
        let s24 = row.id.clone();
        items.push(
            MenuItem::new("延后 1 小时").on_select(move || controller::snooze(s1.clone(), Some(1))),
        );
        items.push(
            MenuItem::new("延后 1 天").on_select(move || controller::snooze(s24.clone(), Some(24))),
        );
    }
    items.push(MenuItem::separator());
    items.push(navigate("重命名", controller::begin_rename));
    items.push(action(
        if row.unread { "标为已读" } else { "标为未读" },
        controller::toggle_unread,
    ));
    if !row.project_key.is_empty() {
        let key = scoped_project(&row.machine, &row.project_key);
        let stage = controller::machines::navigation().with_untracked(|n| controller::machines::by_id(&n.active)).map(|m| m.session.ui).unwrap_or_else(controller::ui);
        let is_filtered = stage.with_untracked(|u| u.filter.project.as_deref() == Some(key.as_str()));
        items.push(
            MenuItem::new(if is_filtered {
                "取消项目筛选"
            } else {
                "按此项目筛选"
            })
            .on_select(move || {
                stage.update(|u| {
                    u.filter.project = if is_filtered { None } else { Some(key.clone()) };
                });
            }),
        );
    }
    items.push(MenuItem::separator());
    if !row.project_key.is_empty() {
        let path = row.project_key.clone();
        items.push(MenuItem::new("复制路径").on_select(move || controller::copy(path.clone())));
    }
    if !row.branch.is_empty() {
        let branch = row.branch.clone();
        items.push(MenuItem::new("复制分支名").on_select(move || controller::copy(branch.clone())));
    }
    for label in ["复制会话链接", "复制会话标题"] {
        let (id, title) = (row.id.clone(), row.title.clone());
        items.push(MenuItem::new(label).on_select(move || controller::copy_thread_title(id.clone(), title.clone())));
    }
    let raw_id = row.id.clone();
    items.push(MenuItem::new("复制会话 ID").on_select(move || controller::copy(raw_id.clone())));
    if !row.project_key.is_empty() {
        items.push(navigate("在终端中打开", controller::open_thread_in_terminal));
    }
    items.push(MenuItem::separator());
    items.push(navigate("删除会话", controller::ask_delete));
    items
}

fn thread_row(row: Memo<Option<SidebarRow>>, selected: Memo<Option<String>>) -> impl IntoElement {
    let colors = palette();
    let active_machine = controller::current_machine().unwrap_or_default();
    let is_selected = computed(move || {
        row.with(|r| {
            r.as_ref()
                .is_some_and(|r| r.machine == active_machine && selected.with(|s| s.as_ref() == Some(&r.id)))
        })
    });
    let status = move || row.with(|r| r.as_ref().map_or(ThreadStatus::Idle, |r| r.status));
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
        .h(px(48.0))
        .flex_shrink_0()
        .flex_row()
        .items_center()
        .pl_2()
        .pr_2()
        .rounded(px(8.0))
        .bg(background)
        .on_mouse_enter(move |_: &MouseEvent| hovered.set(true))
        .on_mouse_leave(move |_: &MouseEvent| hovered.set(false))
        .on_context_menu(move |event: &nsh_core::ContextMenuEvent| {
            if let Some(row) = row.get_untracked() {
                controller::machines::run_on(&row.machine, || drop(popup_menu_at(thread_menu(&row), event.page_position)));
            }
        })
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
                .on_press(move || {
                    if let Some(r) = row.get_untracked() {
                        controller::machines::activate(&r.machine, || {
                            controller::select_thread(r.id);
                            if let Some(child) = r.matched_child { controller::open_child(child); }
                        });
                    }
                })
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
                            move || status() != ThreadStatus::Idle,
                            move || text(move || status_label(status()))
                                .text_size(px(11.0))
                                .text_color(move || status_color(status())),
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
                    .flex_shrink_0()
                    .flex_row()
                    .items_center()
                    .gap_0p5()
                    .px_0p5()
                    .rounded(px(6.0))
                    .child(small_icon_button(
                        Icon::Archive,
                        "归档",
                        move || { if let Some(r) = row.get_untracked() { controller::machines::run_on(&r.machine, || controller::toggle_archive(r.id)); } },
                    ))
                    .child(small_icon_button(
                        Icon::Ellipsis,
                        "更多",
                        move || {
                            if let Some(row) = row.get_untracked() {
                                controller::machines::run_on(&row.machine, || drop(popup_menu(thread_menu(&row))));
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
                        |r: &SidebarRow| (r.machine.clone(), r.id.clone()),
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
