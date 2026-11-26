use std::collections::BTreeSet;

#[cfg(test)]
mod tests;

use nsh_core::prelude::*;
use vantry_platform::git::refs::{Branch, Catalog};

use crate::{
    controller,
    icons::{Icon, icon},
    theme::palette,
    views::{caption, ghost_button, icon_button, primary_button},
};

#[derive(Clone, Debug, PartialEq, Eq)]
struct Action {
    op: &'static str,
    label: &'static str,
    target: String,
    value: String,
    prompt: &'static str,
}
impl Action {
    fn new(op: &'static str, label: &'static str, target: &str) -> Self {
        Self {
            op,
            label,
            target: target.into(),
            value: String::new(),
            prompt: "",
        }
    }
    fn input(mut self, prompt: &'static str) -> Self {
        self.prompt = prompt;
        self
    }
    fn value(mut self, value: &str) -> Self {
        self.value = value.into();
        self
    }
}
fn short_ref(reference: &str) -> &str {
    reference
        .strip_prefix("refs/heads/")
        .or_else(|| reference.strip_prefix("refs/remotes/"))
        .or_else(|| reference.strip_prefix("refs/tags/"))
        .unwrap_or(reference)
}
fn full(b: &Branch) -> String {
    format!(
        "refs/{}/{}",
        if b.remote { "remotes" } else { "heads" },
        b.name
    )
}
fn local_actions(b: &Branch, c: &Catalog, top: bool, demo: bool) -> Vec<Action> {
    let target = full(b);
    let a = |op, label| Action::new(op, label, &target);
    let current = !b.remote && c.current == b.name;
    let mut actions = Vec::new();
    if !top && !current {
        actions.push(a("checkout", "检出分支"));
    }
    if top {
        actions.push(a("commit", "提交更改"));
    }
    if demo {
        return actions;
    }
    if top && !c.remotes.is_empty() {
        actions.push(a("fetch", "抓取远程更新"));
    }
    if !b.remote && !b.upstream.is_empty() {
        actions.push(a("update", if top { "更新项目" } else { "更新分支" }));
        if top {
            actions
                .push(a("compare", "传入 / 传出提交").value(&format!("{}@{{upstream}}", b.name)));
        }
    }
    if !b.remote && !c.remotes.is_empty() {
        let mut push = a("push", "推送");
        if b.upstream.is_empty() {
            if c.remotes.len() == 1 {
                push = push.value(&c.remotes[0]);
            } else {
                push = push.input("远端名称");
            }
        }
        actions.push(push);
    }
    actions.push(a("create", "新建分支").input("新分支名"));
    actions.push(a("worktree", "新建工作树").input("新目录与分支名"));
    if top {
        actions.push(a("revision", "检出标签或修订").input("标签、提交或修订"));
        return actions;
    }
    if !current {
        actions.push(a("compare", "与当前分支比较"));
        if !c.current.is_empty() {
            if !b.remote {
                actions.push(a("checkout-rebase", "检出并变基到当前分支"));
            }
            actions.push(a("rebase", "将当前分支变基到此分支"));
            actions.push(a("merge", "合并到当前分支"));
        }
    }
    actions.push(a("diff", "与工作区比较差异"));
    actions.push(a("history", "显示历史"));
    if !b.remote {
        if c.branches.iter().any(|r| r.remote) {
            actions.push(a("track", "设置跟踪分支").input("远端分支，例如 origin/main"));
        }
        actions.push(a("rename", "重命名分支").input("新的分支名"));
    }
    actions.push(if b.favorite {
        a("unfavorite", "移出收藏")
    } else {
        a("favorite", "加入收藏")
    });
    if !b.remote && !current {
        actions.push(a("delete", "删除分支"));
    }
    actions
}

#[derive(Default)]
struct Folder<'a> {
    name: &'a str,
    children: Vec<Node<'a>>,
}
enum Node<'a> {
    Folder(Folder<'a>),
    Leaf(&'a Branch),
}
impl<'a> Folder<'a> {
    fn insert(&mut self, path: &'a str, b: &'a Branch) {
        if let Some((prefix, rest)) = path.split_once('/') {
            let pos = self
                .children
                .iter()
                .position(|node| matches!(node,Node::Folder(f) if f.name == prefix));
            let pos = pos.unwrap_or_else(|| {
                self.children.push(Node::Folder(Folder {
                    name: prefix,
                    children: Vec::new(),
                }));
                self.children.len() - 1
            });
            if let Node::Folder(f) = &mut self.children[pos] {
                f.insert(rest, b);
            }
        } else {
            self.children.push(Node::Leaf(b));
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
enum Row {
    Group(String),
    Folder(String, String, usize, bool),
    Branch(Branch, usize),
    Action(Action),
}
fn flatten(
    folder: &Folder<'_>,
    key: &str,
    depth: usize,
    open: &BTreeSet<String>,
    search: bool,
    rows: &mut Vec<Row>,
) {
    for node in &folder.children {
        match node {
            Node::Leaf(b) => rows.push(Row::Branch((*b).clone(), depth)),
            Node::Folder(f) => {
                let path = format!("{key}/{}", f.name);
                let expanded = search || open.contains(&path);
                rows.push(Row::Folder(f.name.into(), path.clone(), depth, expanded));
                if expanded {
                    flatten(f, &path, depth + 1, open, search, rows);
                }
            }
        }
    }
}
fn group<'a>(
    rows: &mut Vec<Row>,
    title: &str,
    branches: impl Iterator<Item = &'a Branch>,
    query: &str,
    open: &BTreeSet<String>,
) {
    let mut folder = Folder::default();
    for b in branches.filter(|b| b.name.to_lowercase().contains(query)) {
        folder.insert(&b.name, b);
    }
    if !folder.children.is_empty() {
        rows.push(Row::Group(title.into()));
        flatten(&folder, title, 0, open, !query.is_empty(), rows);
    }
}
fn rows(
    c: &Catalog,
    selected: &str,
    filter: &str,
    open: &BTreeSet<String>,
    demo: bool,
) -> Vec<Row> {
    let query = filter.trim().to_lowercase();
    let mut rows = Vec::new();
    let current = c.branches.iter().find(|b| !b.remote && b.name == c.current);
    let selected = c.branches.iter().find(|b| full(b) == selected);
    let mut actions = if let Some(b) = selected.or(current) {
        local_actions(b, c, selected.is_none(), demo)
    } else {
        vec![
            Action::new("revision", "检出标签或修订", "HEAD").input("标签、提交或修订"),
            Action::new("create", "新建分支", "HEAD").input("新分支名"),
            Action::new("worktree", "新建工作树", "HEAD").input("新目录与分支名"),
            Action::new("commit", "提交更改", "HEAD"),
        ]
    };
    actions.retain(|a| {
        let keywords = match a.op {
            "revision" => "checkout tag or revision",
            "create" => "new branch from",
            "worktree" => "new worktree from",
            "compare" if selected.is_none() => "incoming outgoing commits",
            "compare" => "compare with current",
            "diff" => "show diff with working tree",
            "track" => "tracked branch",
            "update" => "update project pull",
            _ => a.op,
        };
        a.label.contains(&query) || keywords.contains(&query) || a.op.contains(&query)
    });
    rows.extend(actions.into_iter().map(Row::Action));
    if selected.is_some() {
        return rows;
    }
    group(
        &mut rows,
        "收藏",
        c.branches.iter().filter(|b| b.favorite),
        &query,
        open,
    );
    group(
        &mut rows,
        "最近使用",
        c.recent
            .iter()
            .filter_map(|n| c.branches.iter().find(|b| !b.remote && &b.name == n)),
        &query,
        open,
    );
    group(
        &mut rows,
        "本地分支",
        c.branches.iter().filter(|b| !b.remote),
        &query,
        open,
    );
    group(
        &mut rows,
        "远端分支",
        c.branches.iter().filter(|b| b.remote),
        &query,
        open,
    );
    if !demo && !c.tags.is_empty() {
        let tags: Vec<_> = c
            .tags
            .iter()
            .filter(|t| t.to_lowercase().contains(&query))
            .collect();
        if !tags.is_empty() {
            rows.push(Row::Group("标签".into()));
        }
        for tag in tags {
            rows.push(Row::Action(Action::new(
                "checkout",
                "检出标签",
                &format!("refs/tags/{tag}"),
            )));
        }
    }
    rows
}

#[derive(Clone, Copy)]
struct Menu {
    query: Signal<String>,
    selected: Signal<String>,
    open: Signal<BTreeSet<String>>,
    form: Signal<Option<Action>>,
    value: Signal<String>,
}
impl Menu {
    fn choose(self, action: Action) {
        if action.op == "commit" {
            controller::close_branch_menu();
            controller::select_dock_tab(crate::state::workbench::DockTab::Git);
            controller::git_page(crate::state::workbench::GitPage::Commit, None);
        } else if matches!(
            action.op,
            "diff" | "history" | "compare" | "favorite" | "unfavorite"
        ) {
            self.run(action);
        } else {
            self.value.set(action.value.clone());
            self.form.set(Some(action));
        }
    }
    fn run(self, mut a: Action) {
        if !a.prompt.is_empty() {
            a.value = self.value.get_untracked().trim().into();
        }
        if a.op == "revision" {
            a.op = "checkout";
            a.target = a.value.clone();
        }
        if a.op == "track" {
            a.value = format!("refs/remotes/{}", a.value);
        }
        self.form.set(None);
        controller::branch_action(a.op, a.target, a.value, a.label);
    }
}
fn button(
    label: String,
    detail: String,
    glyph: Icon,
    depth: usize,
    cb: impl Fn() + 'static,
) -> impl IntoElement {
    let p = palette();
    nsh_base::button()
        .accessibility_label(label.clone())
        .on_press(cb)
        .w_full()
        .h(px(27.))
        .flex_row()
        .items_center()
        .gap_2()
        .pl(px(9. + 12. * depth as f32))
        .pr_2()
        .rounded(px(5.))
        .hover(|s| s.bg(p.soft))
        .child(icon(glyph, 13., p.muted))
        .child(
            text(label)
                .flex_1()
                .number_of_lines(1u32)
                .text_size(px(12.))
                .text_color(p.ink),
        )
        .child(
            text(detail)
                .max_w(px(270.))
                .number_of_lines(1u32)
                .text_size(px(11.))
                .text_color(p.muted),
        )
}
fn render_row(row: Row, m: Menu, current: &str) -> AnyElement {
    match row {
        Row::Group(title) => caption(title).px_2().pt_2().pb_1().into_element(),
        Row::Folder(label, key, depth, open) => button(
            label,
            String::new(),
            if open {
                Icon::ChevronDown
            } else {
                Icon::ChevronRight
            },
            depth,
            move || {
                m.open.update(|set| {
                    if !set.remove(&key) {
                        set.insert(key.clone());
                    }
                })
            },
        )
        .into_element(),
        Row::Branch(b, depth) => {
            let mut detail = Vec::new();
            let active = !b.remote && b.name == current;
            if active {
                detail.push("当前".to_owned());
            }
            if !b.upstream.is_empty() {
                detail.push(b.upstream.clone());
                detail.push(format!("↑{} ↓{}", b.ahead, b.behind));
            }
            detail.push("›".into());
            button(
                b.name.rsplit('/').next().unwrap_or(&b.name).into(),
                detail.join("  "),
                if active {
                    Icon::Check
                } else if b.favorite {
                    Icon::Pin
                } else {
                    Icon::GitBranch
                },
                depth,
                move || {
                    m.selected.set(full(&b));
                    m.query.set(String::new());
                },
            )
            .into_element()
        }
        Row::Action(a) => {
            let title = a
                .target
                .strip_prefix("refs/tags/")
                .unwrap_or(a.label)
                .into();
            let glyph = match a.op {
                "update" => Icon::Download,
                "push" => Icon::Upload,
                "commit" => Icon::GitCommit,
                "create" => Icon::GitBranch,
                "worktree" => Icon::GitFork,
                "compare" | "diff" => Icon::ArrowLeftRight,
                "delete" => Icon::Trash2,
                _ => Icon::ChevronRight,
            };
            button(title, String::new(), glyph, 0, move || m.choose(a.clone())).into_element()
        }
    }
}

pub(crate) fn dialog() -> impl IntoElement {
    let visible = computed(|| controller::bench().with(|b| b.git.branch_menu));
    when(move || visible.get(), popup)
}
fn popup() -> impl IntoElement {
    let p = palette();
    let m = Menu {
        query: signal(controller::bench().with_untracked(|b| b.git.branch_filter.to_owned())),
        selected: signal(String::new()),
        open: signal(BTreeSet::new()),
        form: signal(None),
        value: signal(String::new()),
    };
    let catalog = computed(|| controller::bench().with(|b| b.git.branch_catalog.clone()));
    let busy = computed(|| controller::bench().with(|b| b.git.busy));
    let feedback = computed(|| controller::bench().with(|b| b.git.branch_feedback.clone()));
    let output = computed(|| {
        controller::bench().with(|b| {
            let (mut text, truncated) = super::clip_patch(&b.git.branch_output);
            if truncated {
                text.push_str("\n… 输出已截断至 180 KB");
            }
            text
        })
    });
    let force = computed(|| controller::bench().with(|b| b.git.unmerged_branch.clone()));
    let demo = matches!(
        crate::launch_config().endpoint,
        crate::bridge::Endpoint::Mock
    );
    let expanded_current = std::cell::RefCell::new(String::new());
    effect(move || {
        if let Some(c) = catalog.get()
            && *expanded_current.borrow() != c.current
        {
            expanded_current.replace(c.current.clone());
            m.open.update(|open| {
                for (i, _) in c.current.match_indices('/') {
                    open.insert(format!("本地分支/{}", &c.current[..i]));
                }
            });
        }
    });
    let shortcut = controller::bench().with_untracked(|b| b.git.branch_filter);
    let handled = std::cell::Cell::new(false);
    effect(move || {
        if shortcut.is_empty() || handled.get() {
            return;
        }
        if let Some(c) = catalog.get() {
            handled.set(true);
            if let Some(Row::Action(action)) = rows(&c, "", shortcut, &BTreeSet::new(), demo)
                .into_iter()
                .next()
            {
                m.choose(action);
            }
        }
    });
    let panel = view()
        .w_full()
        .max_w(px(470.))
        .max_h(move || px((crate::window_size().height.0 - 40.).clamp(160., 600.)))
        .flex_col()
        .p_2()
        .rounded(px(10.))
        .bg(p.card)
        .border_1()
        .border_color(p.line)
        .shadow_xl()
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .border_b_1()
                .border_b_color(p.line_soft)
                .pb_1()
                .child(when(
                    move || m.selected.with(|s| !s.is_empty()) || m.form.with(Option::is_some),
                    move || {
                        icon_button(Icon::ArrowLeft, "返回", move || {
                            if m.form.with_untracked(Option::is_some) {
                                m.form.set(None);
                            } else {
                                m.selected.set(String::new());
                            }
                            m.query.set(String::new());
                        })
                    },
                ))
                .child(icon(Icon::Search, 14., p.muted))
                .child(
                    nsh_base::input()
                        .flex_1()
                        .auto_focus(true)
                        .placeholder("搜索分支、标签或操作")
                        .value(move || m.query.get())
                        .on_change(move |s: &str| m.query.set(s.into()))
                        .text_size(px(13.))
                        .text_color(p.ink),
                )
                .child(icon_button(
                    Icon::X,
                    "关闭分支菜单",
                    controller::close_branch_menu,
                )),
        )
        .child(when(
            move || m.selected.with(|s| !s.is_empty()),
            move || {
                caption(move || short_ref(&m.selected.get()).to_owned())
                    .px_2()
                    .py_1()
            },
        ))
        .child(dynamic(move || {
            if let Some(a) = m.form.get() {
                let confirm = a.clone();
                let has_input = !a.prompt.is_empty();
                let context = if a.op == "force-delete" {
                    "强制删除会丢失未合并提交。"
                } else {
                    ""
                };
                return view()
                    .flex_col()
                    .gap_2()
                    .p_2()
                    .child(
                        text(format!("{} · {}", a.label, short_ref(&a.target))).text_color(p.ink),
                    )
                    .child(caption(format!("{context}{}", a.value)))
                    .child(when(has_input, move || {
                        nsh_base::input()
                            .auto_focus(true)
                            .placeholder(a.prompt)
                            .value(move || m.value.get())
                            .on_change(move |s: &str| m.value.set(s.into()))
                            .text_color(p.ink)
                            .bg(p.soft)
                            .px_2()
                            .py_1()
                    }))
                    .child(
                        view()
                            .flex_row()
                            .gap_2()
                            .child(ghost_button("取消", move || m.form.set(None)))
                            .child(
                                primary_button("确认", move || m.run(confirm.clone())).disabled(
                                    move || {
                                        busy.get()
                                            || (has_input && m.value.with(|s| s.trim().is_empty()))
                                    },
                                ),
                            ),
                    )
                    .into_element();
            }
            let mut list = view().flex_col();
            if let Some(c) = catalog.get() {
                let lines = rows(&c, &m.selected.get(), &m.query.get(), &m.open.get(), demo);
                if lines.is_empty() {
                    list = list.child(caption("没有匹配项"));
                }
                for row in lines {
                    list = list.child(render_row(row, m, &c.current));
                }
            }
            scroll()
                .min_h_0()
                .max_h(move || px((crate::window_size().height.0 - 150.).clamp(110., 500.)))
                .child(list)
                .into_element()
        }))
        .child(when(
            move || feedback.with(|s| !s.is_empty()),
            move || caption(move || feedback.get()).px_2().py_1().select_text(),
        ))
        .child(when(
            move || force.with(Option::is_some),
            move || {
                ghost_button("强制删除…", move || {
                    if let Some(target) = force.get_untracked() {
                        m.choose(Action::new("force-delete", "强制删除分支", &target));
                    }
                })
            },
        ))
        .child(when(
            move || output.with(|s| !s.is_empty()),
            move || {
                scroll().max_h(px(150.)).min_h_0().child(
                    text(move || output.get())
                        .select_text()
                        .font_family("Menlo")
                        .text_size(px(11.))
                        .text_color(p.ink)
                        .p_2(),
                )
            },
        ));
    nsh_base::dialog()
        .on_cancel(controller::close_branch_menu)
        .child(
            view()
                .flex_1()
                .p_3()
                .items_center()
                .justify_center()
                .bg(p.scrim)
                .child(
                    nsh_base::button()
                        .absolute()
                        .top(px(0.))
                        .left(px(0.))
                        .w_full()
                        .h_full()
                        .accessibility_label("关闭")
                        .on_press(controller::close_branch_menu),
                )
                .child(panel),
        )
}
