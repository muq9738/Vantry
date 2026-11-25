//! The strip under the composer card (`本机 · 项目 · 本地目录`) and the two
//! pickers it opens, as MyGo had them: the machine the draft runs on, and
//! T3 Code's folder palette over that machine's directories (↑↓ to move,
//! Enter to go into a folder, ⌘Enter to use the one the field names,
//! Backspace at a `/` to climb, Esc to close; a filter starting with `.`
//! shows hidden folders).

use std::collections::HashMap;

use nsh_core::prelude::*;

use super::{caption, ghost_button, link_button, primary_button};
use crate::{
    controller::{self, Folder, Listing},
    icons::{Icon, icon},
    state::ui::SettingsPage,
    theme::palette,
};

// ------------------------------------------------------------------ paths

/// `p` with the home directory written `~`.
pub(crate) fn tilde(p: &str, home: &str) -> String {
    if home.is_empty() {
        return p.to_owned();
    }
    if p == home {
        return "~".to_owned();
    }
    match p.strip_prefix(home) {
        Some(rest) if rest.starts_with('/') => format!("~{rest}"),
        _ => p.to_owned(),
    }
}

/// `p` with a leading `~` written out.
pub(crate) fn expand(p: &str, home: &str) -> String {
    if home.is_empty() {
        return p.to_owned();
    }
    if p == "~" {
        return home.to_owned();
    }
    match p.strip_prefix("~/") {
        Some(rest) => format!("{}/{rest}", home.trim_end_matches('/')),
        None => p.to_owned(),
    }
}

/// The field's directory to list and the filter typed after its last `/`.
pub(crate) fn split_input(input: &str, home: &str) -> (String, String) {
    let full = expand(input.trim(), home);
    if full.is_empty() {
        return (home.to_owned(), String::new());
    }
    // A drive's root keeps its slash (`C:` alone is a relative path).
    let root = |dir: &str| {
        if dir.is_empty() {
            "/".to_owned()
        } else if dir.ends_with(':') {
            format!("{dir}/")
        } else {
            dir.to_owned()
        }
    };
    if full.ends_with('/') {
        return (root(full.trim_end_matches('/')), String::new());
    }
    match full.rfind('/') {
        None => (home.to_owned(), full),
        Some(i) => (root(&full[..i]), full[i + 1..].to_owned()),
    }
}

/// A directory as the field shows it: `~/Develop/`.
pub(crate) fn dir_input(dir: &str, home: &str) -> String {
    let shown = tilde(dir, home);
    if shown.ends_with('/') { shown } else { format!("{shown}/") }
}

/// One row of the palette: a folder, or `..` (the parent).
#[derive(Debug, Clone, PartialEq)]
pub(crate) struct Row {
    pub name: String,
    pub path: String,
    pub parent: bool,
}

/// The parent link and the folders matching `filter`: names starting with
/// it first, then names containing it. Hidden folders only for a filter
/// that starts with `.`.
pub(crate) fn rows(listing: &Listing, filter: &str) -> Vec<Row> {
    let mut rows = Vec::new();
    if filter.is_empty() && !listing.parent.is_empty() && listing.parent != listing.path {
        rows.push(Row { name: "..".into(), path: listing.parent.clone(), parent: true });
    }
    let lower = filter.to_lowercase();
    let show_hidden = filter.starts_with('.');
    let mut contains = Vec::new();
    for Folder { name, path } in &listing.folders {
        if name.starts_with('.') && !show_hidden {
            continue;
        }
        let key = name.to_lowercase();
        let row = Row { name: name.clone(), path: path.clone(), parent: false };
        if lower.is_empty() || key.starts_with(&lower) {
            rows.push(row);
        } else if key.contains(&lower) {
            contains.push(row);
        }
    }
    rows.extend(contains);
    rows
}

fn last_component(path: &str) -> String {
    let trimmed = path.trim_end_matches('/');
    match trimmed.rsplit('/').next() {
        Some(name) if !name.is_empty() => name.to_owned(),
        _ => "/".to_owned(),
    }
}

// ------------------------------------------------------------------ strip

/// The quiet strip under the card: the machine (opens the machine picker),
/// the project folder (opens the folder palette), its mode, and the branch.
pub(crate) fn context_strip(
    endpoint: Memo<String>,
    workspace: Memo<String>,
    branch: Memo<String>,
    turn_count: Memo<usize>,
    compact: Memo<bool>,
) -> impl IntoElement {
    let colors = palette();
    let proxy = computed(|| {
        controller::state().with(|s| {
            s.current_workspace()
                .is_some_and(|w| !w.environment_id.is_empty())
        })
    });
    let worktree = computed(|| {
        controller::state().with(|s| {
            s.current_workspace()
                .is_some_and(|w| w.kind == "worktree" || !w.repository.is_empty())
        })
    });
    let strip_button = |label: &'static str| {
        nsh_base::button()
            .accessibility_label(label)
            .flex_row()
            .items_center()
            .gap_1()
            .h(px(24.0))
            .px_1p5()
            .min_w(px(0.0))
            .flex_shrink(1.0)
            .rounded(px(6.0))
            .hover(move |s| s.bg(colors.soft))
            .active(|s| s.opacity(0.7))
            .disabled_style(|s| s.opacity(0.6))
    };
    let host = strip_button("运行机器")
        .on_press(controller::open_machine_picker)
        .max_w(px(200.0))
        .child(icon(Icon::Server, 12.0, colors.faint))
        .child(
            text(move || endpoint.get())
                .number_of_lines(1u32)
                .min_w(px(0.0))
                .flex_shrink(1.0)
                .text_size(px(11.0))
                .text_color(colors.muted),
        )
        .child(icon(Icon::ChevronDown, 10.0, colors.faint));
    let folder = strip_button("项目与工作区")
        .on_press(controller::open_folder_picker)
        .disabled(move || proxy.get())
        .child(dynamic(move || {
            icon(if worktree.get() { Icon::GitFork } else { Icon::Folder }, 12.0, colors.faint)
        }))
        .child(
            text(move || {
                let ws = workspace.get();
                if ws.is_empty() {
                    "选择项目".to_owned()
                } else {
                    crate::state::sidebar::project_name(&ws).to_owned()
                }
            })
            .number_of_lines(1u32)
            .min_w(px(0.0))
            .flex_shrink(1.0)
            .text_size(px(11.0))
            .text_color(colors.muted),
        )
        .child(when(
            move || !compact.get() && !workspace.with(String::is_empty),
            move || {
                text(move || {
                    if proxy.get() {
                        "· 远端工作区"
                    } else if worktree.get() {
                        "· 隔离分支"
                    } else {
                        "· 本地目录"
                    }
                })
                .flex_shrink_0()
                .text_size(px(11.0))
                .text_color(colors.faint)
            },
        ))
        .child(icon(Icon::ChevronDown, 10.0, colors.faint));
    view()
        .flex_col()
        .gap_1()
        .child(
            view()
                .h(px(26.0))
                .min_w(px(0.0))
                .flex_row()
                .items_center()
                .gap_0p5()
                .child(host)
                .child(view().w(px(1.0)).h(px(12.0)).mx_0p5().flex_shrink_0().bg(colors.line))
                .child(folder)
                .child(view().flex_1().min_w(px(4.0)))
                .child(when(
                    move || branch.with(|b| !b.is_empty()),
                    move || {
                        view()
                            .flex_row()
                            .items_center()
                            .gap_1()
                            .min_w(px(0.0))
                            .flex_shrink(1.0)
                            .max_w(px(180.0))
                            .px_1()
                            .child(icon(Icon::GitBranch, 12.0, colors.faint))
                            .child(
                                text(move || branch.get())
                                    .number_of_lines(1u32)
                                    .min_w(px(0.0))
                                    .flex_shrink(1.0)
                                    .text_size(px(11.0))
                                    .text_color(colors.faint),
                            )
                    },
                )),
        )
        .child(when(
            move || turn_count.get() > 0,
            move || {
                view()
                    .w_full()
                    .h(px(26.0))
                    .flex_row()
                    .items_center()
                    .justify_center()
                    .child(
                        view()
                            .flex_row()
                            .items_center()
                            .gap_1p5()
                            .px_2p5()
                            .py_0p5()
                            .rounded_full()
                            .bg(colors.soft)
                            .child(super::dot(colors.success))
                            .child(
                                text(move || format!("第 {} 轮", turn_count.get()))
                                    .text_size(px(11.0))
                                    .text_color(colors.muted),
                            ),
                    )
            },
        ))
}

// ---------------------------------------------------------------- dialogs

/// The scrim and the floating panel near the top of the window.
fn palette_frame(
    width: f32,
    body: impl IntoElement,
    on_cancel: impl Fn() + Copy + 'static,
) -> impl IntoElement {
    let colors = palette();
    nsh_base::dialog()
        .animation(ModalAnimation::Fade)
        .on_cancel(on_cancel)
        .child(
            view()
                .flex_1()
                .items_center()
                .bg(colors.scrim)
                .px_4()
                .pt(move || px((crate::window_size().height.0 * 0.14).clamp(24.0, 120.0)))
                .on_press(on_cancel)
                .child(
                    view()
                        .w_full()
                        .max_w(px(width))
                        .flex_col()
                        .rounded(px(14.0))
                        .bg(colors.raised)
                        .border_1()
                        .border_color(colors.line)
                        .shadow_xl()
                        .overflow_hidden()
                        .on_press(|| {})
                        .child(body),
                ),
        )
}

fn key_hint(keys: &'static str, label: &'static str) -> impl IntoElement {
    let colors = palette();
    view()
        .flex_row()
        .items_center()
        .gap_1()
        .flex_shrink_0()
        .child(
            view()
                .h(px(18.0))
                .min_w(px(18.0))
                .px_1()
                .items_center()
                .justify_center()
                .rounded(px(4.0))
                .border_1()
                .border_color(colors.line)
                .bg(colors.soft)
                .child(text(keys).text_size(px(10.5)).text_color(colors.muted)),
        )
        .child(text(label).text_size(px(11.0)).text_color(colors.faint))
}

/// The machines this window knows: choosing another starts a draft there.
pub(crate) fn machine_picker() -> impl IntoElement {
    let open = computed(|| controller::ui().with(|u| u.machine_picker));
    dynamic(move || open.get().then(|| machine_panel().into_element()))
}

fn machine_panel() -> impl IntoElement {
    let colors = palette();
    let active = computed(|| controller::machines::navigation().with(|n| n.active.clone()));
    let machines = computed(|| {
        controller::machines::records().with(|list| {
            list.iter()
                .map(|m| (m.id.clone(), m.name.clone(), m.failure.get()))
                .collect::<Vec<_>>()
        })
    });
    let fixed = controller::conversation_fixed();
    let body = view()
        .flex_col()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_4()
                .pt_3p5()
                .pb_2()
                .child(icon(Icon::Server, 15.0, colors.muted))
                .child(
                    text("运行机器")
                        .flex_1()
                        .text_size(px(14.0))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.ink),
                )
                .child(super::icon_button(Icon::X, "关闭", controller::close_machine_picker)),
        )
        .child(when(fixed, || {
            caption("当前会话固定在它的机器上；选择其他机器会在那里新建会话。")
                .px_4()
                .pb_2()
        }))
        .child(
            view().flex_col().px_2().pb_2().gap_0p5().child(each(
                move || machines.get(),
                |m: &(String, String, String)| m.0.clone(),
                move |machine, _| {
                    let id = move || machine.with_untracked(|m| m.0.clone());
                    let current = move || active.get() == machine.with(|m| m.0.clone());
                    nsh_base::button()
                        .accessibility_label(move || Some(SharedString::from(machine.with(|m| m.1.clone()))))
                        .selected(current)
                        .on_press(move || controller::use_machine(id()))
                        .flex_row()
                        .items_center()
                        .gap_2p5()
                        .px_2p5()
                        .py_2()
                        .rounded(px(9.0))
                        .bg(move || if current() { colors.soft } else { AdaptiveColor::TRANSPARENT })
                        .hover(move |s| s.bg(colors.soft))
                        .active(|s| s.opacity(0.75))
                        .child(dynamic(move || {
                            let local = machine.with(|m| m.0.is_empty());
                            icon(if local { Icon::Monitor } else { Icon::Server }, 15.0, colors.muted)
                        }))
                        .child(
                            view()
                                .flex_1()
                                .min_w(px(0.0))
                                .flex_col()
                                .gap_0p5()
                                .child(
                                    text(move || machine.with(|m| m.1.clone()))
                                        .number_of_lines(1u32)
                                        .text_size(px(13.0))
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_color(colors.ink),
                                )
                                .child(
                                    text(move || {
                                        machine.with(|m| {
                                            if !m.2.is_empty() {
                                                format!("连接失败 · {}", m.2)
                                            } else if m.0.is_empty() {
                                                "这台电脑".to_owned()
                                            } else {
                                                "配对主机".to_owned()
                                            }
                                        })
                                    })
                                    .number_of_lines(1u32)
                                    .text_size(px(11.0))
                                    .text_color(move || {
                                        if machine.with(|m| !m.2.is_empty()) {
                                            colors.warning
                                        } else {
                                            colors.faint
                                        }
                                    }),
                                ),
                        )
                        .child(when(current, move || icon(Icon::Check, 14.0, colors.accent)))
                },
            )),
        )
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_4()
                .py_2p5()
                .border_t_1()
                .border_t_color(colors.line_soft)
                .child(caption("机器在「设置 · 远程」中配对").flex_1())
                .child(ghost_button("连接其他机器…", || {
                    controller::close_machine_picker();
                    controller::machines::activate("", || {
                        controller::open_settings(SettingsPage::Remote)
                    });
                })),
        );
    palette_frame(440.0, body, controller::close_machine_picker)
}

/// The folder palette of the machine the draft runs on.
pub(crate) fn folder_picker() -> impl IntoElement {
    let open = computed(|| controller::ui().with(|u| u.folder_picker));
    dynamic(move || open.get().then(|| folder_panel().into_element()))
}

fn folder_panel() -> impl IntoElement {
    let colors = palette();
    let local = controller::browsing_local();
    let home = signal(if local { controller::local_home() } else { String::new() });
    let input = signal(String::new());
    let listing = signal(None::<Listing>);
    let failure = signal(String::new());
    let loading = signal(false);
    let index = signal(0usize);
    let cache = stored(HashMap::<String, Listing>::new());
    let field = element_ref();
    let machine = computed(|| controller::state().with(|s| s.endpoint_label.clone()));

    let caret_to_end = move || {
        spawn(async move {
            timer(std::time::Duration::from_millis(16)).await;
            let len = input.get_untracked().encode_utf16().count() as u32;
            field.focus();
            field.set_selection(len, len);
        });
    };
    let set_field = move |next: String| {
        input.set(next);
        index.set(0);
        caret_to_end();
    };

    // Where it opens: the current project's parent, else home (a paired
    // machine's home is what its Core lists for an empty path).
    let start = controller::folder_picker_start();
    if local || !start.is_empty() {
        let h = home.get_untracked();
        input.set(dir_input(if start.is_empty() { &h } else { &start }, &h));
    }
    if !local {
        spawn(async move {
            if let Ok(found) = controller::list_folders(String::new()).await {
                home.set(found.path.clone());
                cache.with(|c| {
                    c.insert(found.path.clone(), found.clone());
                });
                if input.get_untracked().is_empty() {
                    input.set(dir_input(&found.path, &found.path));
                } else {
                    // Rewrite the opening path with `~` now that home is known.
                    let shown = dir_input(&expand(&input.get_untracked(), ""), &found.path);
                    input.set(shown);
                }
                caret_to_end();
            }
        });
    } else {
        caret_to_end();
    }

    let split = computed(move || split_input(&input.get(), &home.get()));
    let dir = computed(move || split.with(|s| s.0.clone()));
    effect(move || {
        let dir = dir.get();
        if dir.is_empty() {
            return;
        }
        if let Some(hit) = cache.with(|c| c.get(&dir).cloned()) {
            failure.set(String::new());
            listing.set(Some(hit));
            return;
        }
        loading.set(true);
        spawn(async move {
            let result = controller::list_folders(dir.clone()).await;
            if dir != split.with_untracked(|s| s.0.clone()) {
                return;
            }
            loading.set(false);
            match result {
                Ok(found) => {
                    cache.with(|c| {
                        c.insert(dir.clone(), found.clone());
                    });
                    failure.set(String::new());
                    listing.set(Some(found));
                }
                Err(error) => {
                    failure.set(format!("读取目录失败：{error}"));
                    listing.set(Some(Listing { path: dir.clone(), ..Default::default() }));
                }
            }
        });
    });
    let shown_rows = computed(move || {
        let filter = split.with(|s| s.1.clone());
        let current = dir.get();
        listing.with(|l| match l {
            Some(l) if l.path == current || l.path.trim_end_matches('/') == current => rows(l, &filter),
            _ => Vec::new(),
        })
    });
    let enter = move |row: Row| {
        set_field(dir_input(&row.path, &home.get_untracked()));
    };
    // ⌘Enter / the button: the highlighted folder while filtering, else the
    // folder the field names.
    let choose = move || {
        let (dir, filter) = split.get_untracked();
        let list = shown_rows.get_untracked();
        let path = if !filter.is_empty() && !list.is_empty() {
            list[index.get_untracked().min(list.len() - 1)].path.clone()
        } else {
            dir
        };
        if !path.is_empty() {
            controller::use_folder(path);
        }
    };
    let target_name = computed(move || {
        let (dir, filter) = split.get();
        let list = shown_rows.get();
        if !filter.is_empty() && !list.is_empty() {
            list[index.get().min(list.len() - 1)].name.clone()
        } else {
            last_component(&dir)
        }
    });

    let keys = KeyOptions {
        when: nsh_core::keymap::When::Always,
        for_focus: true,
        ..Default::default()
    };
    let n = computed(move || shown_rows.with(Vec::len));
    on_key_with("up", KeyOptions { repeat: true, ..keys }, move |_| {
        let n = n.get_untracked();
        if n > 0 {
            index.set((index.get_untracked() + n - 1) % n);
        }
    });
    on_key_with("down", KeyOptions { repeat: true, ..keys }, move |_| {
        let n = n.get_untracked();
        if n > 0 {
            index.set((index.get_untracked() + 1) % n);
        }
    });
    on_key_with("mod+enter", keys, move |_| choose());
    on_key_with("escape", keys, |_| controller::close_folder_picker());

    let header = view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_4()
        .pt_3p5()
        .pb_1()
        .child(icon(Icon::FolderOpen, 15.0, colors.muted))
        .child(
            text("选择项目目录")
                .text_size(px(14.0))
                .font_weight(FontWeight::SEMIBOLD)
                .text_color(colors.ink),
        )
        .child(
            text(move || format!("· {}", machine.get()))
                .flex_1()
                .number_of_lines(1u32)
                .text_size(px(11.5))
                .text_color(colors.faint),
        )
        .child(super::icon_button(Icon::X, "关闭", controller::close_folder_picker));
    let field_row = view()
        .mx_3()
        .my_2()
        .px_3()
        .h(px(36.0))
        .flex_row()
        .items_center()
        .gap_2()
        .rounded(px(9.0))
        .bg(colors.canvas)
        .border_1()
        .border_color(colors.accent)
        .child(icon(Icon::Search, 13.0, colors.faint))
        .child(
            nsh_base::input()
                .element_ref(&field)
                .label("目录路径")
                .value(move || input.get())
                .on_change(move |next: &str| {
                    let previous = input.get_untracked();
                    // Backspace at a `/` climbs one level, as T3 Code's does.
                    if previous.ends_with('/')
                        && previous != "/"
                        && previous != "~/"
                        && next == &previous[..previous.len() - 1]
                    {
                        let h = home.get_untracked();
                        let (dir, _) = split_input(&previous, &h);
                        let trimmed = dir.trim_end_matches('/');
                        let parent = match trimmed.rfind('/') {
                            Some(0) | None => "/".to_owned(),
                            Some(i) if trimmed[..i].ends_with(':') => trimmed[..=i].to_owned(),
                            Some(i) => trimmed[..i].to_owned(),
                        };
                        set_field(dir_input(&parent, &h));
                        return;
                    }
                    input.set(next.to_owned());
                    index.set(0);
                })
                .on_submit(move |_: &str| {
                    let list = shown_rows.get_untracked();
                    if let Some(row) = list.get(index.get_untracked().min(list.len().saturating_sub(1))) {
                        enter(row.clone());
                    }
                })
                .placeholder("~/")
                .placeholder_color(move || color(colors.faint))
                .caret_color(move || color(colors.accent))
                .auto_focus(true)
                .auto_capitalize(AutoCapitalize::None)
                .auto_correct(false)
                .flex_1()
                .font_family("Menlo")
                .text_size(px(12.5))
                .text_color(colors.ink),
        )
        .child(when(move || loading.get(), || caption("读取中…")));
    let list = scroll()
        .max_h(move || px((crate::window_size().height.0 * 0.46).clamp(160.0, 380.0)))
        .child(
            view()
                .flex_col()
                .px_2()
                .pb_1()
                .gap(px(1.0))
                .child(when(
                    move || !failure.with(String::is_empty),
                    move || {
                        text(move || failure.get())
                            .px_2()
                            .py_1()
                            .text_size(px(11.5))
                            .text_color(colors.warning)
                    },
                ))
                .child(when(
                    move || n.get() == 0 && !loading.get() && failure.with(String::is_empty),
                    move || {
                        caption(move || {
                            if split.with(|s| s.1.is_empty()) {
                                "没有子目录"
                            } else {
                                "没有匹配的目录（输入 . 开头可查看隐藏目录）"
                            }
                        })
                        .px_2()
                        .py_2()
                    },
                ))
                .child(each(
                    move || shown_rows.get().into_iter().enumerate().collect::<Vec<_>>(),
                    |(i, row): &(usize, Row)| format!("{i}\u{1f}{}", row.path),
                    move |entry, _| {
                        let at = move || entry.with(|e| e.0);
                        let on = move || index.get() == at();
                        nsh_base::button()
                            .accessibility_label(move || Some(SharedString::from(entry.with(|e| e.1.name.clone()))))
                            .on_press(move || enter(entry.with_untracked(|e| e.1.clone())))
                            .on_mouse_enter(move |_: &MouseEvent| index.set(at()))
                            .flex_row()
                            .items_center()
                            .gap_2()
                            .h(px(30.0))
                            .px_2p5()
                            .rounded(px(7.0))
                            .bg(move || if on() { colors.soft } else { AdaptiveColor::TRANSPARENT })
                            .child(dynamic(move || {
                                let (parent, hidden) =
                                    entry.with(|e| (e.1.parent, e.1.name.starts_with('.')));
                                icon(
                                    if parent { Icon::ArrowUp } else { Icon::Folder },
                                    13.0,
                                    if hidden { colors.faint } else { colors.muted },
                                )
                            }))
                            .child(
                                text(move || {
                                    entry.with(|e| {
                                        if e.1.parent { "上一级".to_owned() } else { e.1.name.clone() }
                                    })
                                })
                                .flex_1()
                                .number_of_lines(1u32)
                                .text_size(px(12.5))
                                .text_color(move || {
                                    if entry.with(|e| e.1.name.starts_with('.')) {
                                        colors.muted
                                    } else {
                                        colors.ink
                                    }
                                }),
                            )
                            .child(when(on, move || icon(Icon::ChevronRight, 12.0, colors.faint)))
                    },
                )),
        );
    let footer = view()
        .flex_row()
        .items_center()
        .gap_3()
        .px_4()
        .py_2p5()
        .border_t_1()
        .border_t_color(colors.line_soft)
        .child(key_hint("↑↓", "选择"))
        .child(key_hint("↵", "进入"))
        .child(key_hint("⌘↵", "使用"))
        .child(view().flex_1())
        .child(when(local, || link_button("系统选择器…", controller::pick_folder_natively)))
        .child(
            primary_button(move || format!("使用「{}」", target_name.get()), choose)
                .disabled(move || split.with(|s| s.0.is_empty())),
        );
    let body = view()
        .flex_col()
        .child(header)
        .child(field_row)
        .child(list)
        .child(footer);
    palette_frame(600.0, body, controller::close_folder_picker)
}

#[cfg(test)]
mod tests {
    use super::*;

    const HOME: &str = "/Users/me";

    #[test]
    fn the_field_names_a_directory_and_a_filter() {
        assert_eq!(split_input("~/Develop/", HOME), ("/Users/me/Develop".into(), "".into()));
        assert_eq!(split_input("~/Develop/Va", HOME), ("/Users/me/Develop".into(), "Va".into()));
        assert_eq!(split_input("~/.co", HOME), ("/Users/me".into(), ".co".into()));
        assert_eq!(split_input("/", HOME), ("/".into(), "".into()));
        assert_eq!(split_input("/us", HOME), ("/".into(), "us".into()));
        assert_eq!(split_input("", HOME), (HOME.into(), "".into()));
        assert_eq!(dir_input("/Users/me/Develop", HOME), "~/Develop/");
        assert_eq!(dir_input(HOME, HOME), "~/");
        assert_eq!(dir_input("/", HOME), "/");
        assert_eq!(tilde("/Users/meow", HOME), "/Users/meow");
        assert_eq!(split_input("C:/", "C:/Users/me"), ("C:/".into(), "".into()));
        assert_eq!(split_input("C:/Us", "C:/Users/me"), ("C:/".into(), "Us".into()));
        assert_eq!(dir_input("C:/Users/me/src", "C:/Users/me"), "~/src/");
    }

    #[test]
    fn hidden_folders_show_for_a_dot_filter_only() {
        let listing = Listing {
            path: HOME.into(),
            parent: "/Users".into(),
            folders: [".config", ".codex", "Code", "Develop", "decode"]
                .iter()
                .map(|n| Folder { name: (*n).into(), path: format!("{HOME}/{n}") })
                .collect(),
        };
        let names = |filter: &str| rows(&listing, filter).into_iter().map(|r| r.name).collect::<Vec<_>>();
        assert_eq!(names(""), ["..", "Code", "Develop", "decode"]);
        assert_eq!(names("."), [".config", ".codex"]);
        assert_eq!(names(".co"), [".config", ".codex"]);
        // Prefix matches first, then names containing the filter.
        assert_eq!(names("de"), ["Develop", "decode", "Code"]);
    }
}
