//! The Stardust composer (`v0.1.42` layout & interactions):
//! - Floating 16px rounded card over the reading column (`760px`),
//! - Live `@` file mention, `$` skill, and `/` slash command completion popovers,
//! - Queue editor & attachment chips with crisp Lucide icons,
//! - Bottom toolbar matching `v0.1.42`: `+` attach, `[list-checks] 规划`,
//!   tinted permission pill (`自动` soft blue / `改文件自动` / `询问`),
//!   unified `π model | ● ▾` capsule with interactive Thinking Effort popover
//!   (`低 / 中 / 高 / 超高 / Max`), and circular `↑` / `■` primary button,
//! - Quiet host + workspace context strip and turn pill beneath the card.

use std::path::Path;

use nsh_core::{LineHeight, prelude::*};
use vantry_protocol::Attachment;

use super::{dot, ghost_button, link_button, pill};
use crate::{
    bridge::workbench::actions::{split_thinking, thinking_label},
    controller,
    icons::{Icon, icon},
    state::{
        APPROVAL_POLICIES, InteractionMode, NoticeLevel, PrimaryAction, QueueItem, QueueItemKind,
        thread_links::{compose, draft_parts, with_body},
        RunningAction, ui::SettingsPage, workbench::DockTab,
    },
    theme::{metrics, palette},
};

/// Active trigger token at the end of the composer draft (`@file`, `$skill`, `/cmd`).
#[derive(Debug, Clone, PartialEq, Eq)]
enum TriggerKind {
    File,
    Skill,
    Slash,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct CompletionTrigger {
    kind: TriggerKind,
    /// Byte offset where the trigger (`@`, `$`, `/`) begins in the draft.
    start: usize,
    end: usize,
    /// Query text after the trigger character.
    query: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct Suggestion {
    key: String,
    label: String,
    detail: String,
    insert: String,
    kind: TriggerKind,
}

/// AppKit, GTK and Win32 report the native selection in UTF-16 units.
fn caret_byte(text: &str, selection: Option<(u32, u32)>) -> Option<usize> {
    let Some((start, end)) = selection else {
        return Some(text.len());
    };
    if start != end {
        return None;
    }
    let mut units = 0;
    for (byte, ch) in text.char_indices() {
        if units == start {
            return Some(byte);
        }
        units += ch.len_utf16() as u32;
        if units > start {
            return None;
        }
    }
    (units == start).then_some(text.len())
}

fn active_trigger(draft: &str, selection: Option<(u32, u32)>) -> Option<CompletionTrigger> {
    let caret = caret_byte(draft, selection)?;
    let before = &draft[..caret];
    let start = before
        .char_indices()
        .rev()
        .find(|(_, c)| c.is_whitespace())
        .map_or(0, |(i, c)| i + c.len_utf8());
    let mut token = before[start..].chars();
    let kind = match token.next()? {
        '@' => TriggerKind::File,
        '$' => TriggerKind::Skill,
        '/' if before[..start]
            .rsplit('\n')
            .next()
            .unwrap_or("")
            .trim()
            .is_empty() =>
        {
            TriggerKind::Slash
        }
        _ => return None,
    };
    let end = draft[caret..]
        .find(char::is_whitespace)
        .map_or(draft.len(), |i| caret + i);
    Some(CompletionTrigger {
        kind,
        start,
        end,
        query: token.as_str().to_owned(),
    })
}

fn replace_trigger(draft: &str, trigger: &CompletionTrigger, insert: &str) -> (String, u32) {
    let suffix = &draft[trigger.end..];
    let reuse_space = insert.ends_with(' ') && suffix.starts_with([' ', '\t']);
    let insert = if reuse_space {
        insert.trim_end()
    } else {
        insert
    };
    let mut result = draft[..trigger.start].to_owned();
    result.push_str(insert);
    let caret = result.encode_utf16().count() as u32 + u32::from(reuse_space);
    result.push_str(suffix);
    (result, caret)
}

fn slash_suggestions(query: &str) -> Vec<Suggestion> {
    const COMMANDS: &[(&str, &str)] = &[
        ("/skills", "选择这台机器上的 Pi 技能"),
        ("/plan", "切换规划模式（只分析不改代码）"),
        ("/new", "新建空白会话"),
        ("/fork", "从当前会话分叉"),
        ("/git", "打开 Git 工作台"),
        ("/files", "打开文件浏览器"),
        ("/terminal", "打开终端"),
        ("/settings", "打开设置中心"),
        ("/clear", "清空当前草稿"),
    ];
    let q = query.trim().to_ascii_lowercase();
    COMMANDS
        .iter()
        .filter(|(cmd, desc)| {
            q.is_empty()
                || cmd.trim_start_matches('/').contains(&q)
                || desc.to_ascii_lowercase().contains(&q)
        })
        .map(|(cmd, desc)| Suggestion {
            key: format!("slash:{cmd}"),
            label: (*cmd).to_owned(),
            detail: (*desc).to_owned(),
            insert: (*cmd).to_owned(),
            kind: TriggerKind::Slash,
        })
        .collect()
}

fn skill_suggestions(skills: &[(String, String)], query: &str) -> Vec<Suggestion> {
    let query = query.trim_start_matches("skill:").to_lowercase();
    let mut matches: Vec<_> = skills.iter().filter_map(|(name, description)| {
        let label = name.to_lowercase();
        let rank = if label == query { 0 } else if label.starts_with(&query) { 1 }
            else if label.contains(&query) { 2 } else if description.to_lowercase().contains(&query) { 3 } else { return None };
        Some((rank, Suggestion { key: format!("skill:{name}"), label: name.clone(), detail: description.clone(), insert: name.clone(), kind: TriggerKind::Skill }))
    }).collect();
    matches.sort_by_key(|(rank, _)| *rank);
    matches.into_iter().map(|(_, item)| item).collect()
}

fn discover_files(workspace: &str, query: &str) -> Vec<Suggestion> {
    if workspace.is_empty() {
        return Vec::new();
    }
    let root = Path::new(workspace);
    let q = query.trim().to_ascii_lowercase();
    let mut out = Vec::new();
    let mut stack = vec![(root.to_path_buf(), 0usize)];
    let mut visited = 0usize;
    while let Some((dir, depth)) = stack.pop() {
        if depth > 4 || visited > 400 || out.len() >= 10 {
            break;
        }
        let Ok(entries) = std::fs::read_dir(&dir) else {
            continue;
        };
        let mut children: Vec<_> = entries.flatten().collect();
        children.sort_by_key(|e| e.file_name());
        for entry in children {
            visited += 1;
            let name = entry.file_name().to_string_lossy().into_owned();
            if name.starts_with('.')
                || matches!(name.as_str(), "target" | "node_modules" | "dist" | "vendor")
            {
                continue;
            }
            let path = entry.path();
            let rel = path
                .strip_prefix(root)
                .map(|p| p.display().to_string())
                .unwrap_or_else(|_| name.clone());
            let is_dir = entry.file_type().is_ok_and(|t| t.is_dir());
            if q.is_empty() || rel.to_ascii_lowercase().contains(&q) {
                out.push(Suggestion {
                    key: format!("file:{rel}"),
                    label: format!("@{rel}"),
                    detail: if is_dir {
                        "目录".into()
                    } else {
                        "文件".into()
                    },
                    insert: format!("@{rel} "),
                    kind: TriggerKind::File,
                });
                if out.len() >= 10 {
                    break;
                }
            }
            if is_dir && depth < 3 {
                stack.push((path, depth + 1));
            }
        }
    }
    out
}

type Completions = Option<(String, CompletionTrigger, Vec<Suggestion>)>;

fn choose_suggestion(state: Memo<Completions>, index: usize, editor: ElementRef) {
    let Some((draft, trigger, items)) = state.get_untracked() else {
        return;
    };
    let Some(item) = items.get(index) else { return };
    let slash = item.kind == TriggerKind::Slash;
    let insert = match item.kind {
        TriggerKind::Slash if item.insert == "/skills" => "/skill:",
        TriggerKind::Slash | TriggerKind::Skill => "",
        TriggerKind::File => &item.insert,
    };
    let (text, caret) = replace_trigger(&draft, &trigger, insert);
    let source = controller::state().with_untracked(|s| {
        let source = s.draft().text;
        if item.kind == TriggerKind::Skill {
            let (_, refs, _) = draft_parts(&source);
            compose(Some(&item.insert), refs.iter().map(|r| r.source), &text)
        } else { with_body(&source, &text) }
    });
    controller::set_draft(&source);
    editor.focus();
    editor.set_selection(caret, caret);
    if slash {
        match item.insert.as_str() {
            "/plan" => controller::toggle_mode(),
            "/new" => controller::new_thread(),
            "/fork" => {
                if let Some(id) = controller::state().with_untracked(|s| s.selected.clone()) {
                    controller::fork(id, None);
                }
            }
            "/git" => controller::select_dock_tab(DockTab::Git),
            "/files" => controller::select_dock_tab(DockTab::Files),
            "/terminal" => controller::select_dock_tab(DockTab::Terminal),
            "/settings" => controller::open_settings(SettingsPage::General),
            "/clear" => controller::set_draft(""),
            _ => {}
        }
    }
}

fn reference_chips(links: Vec<(String, String)>) -> impl IntoElement {
    let mut row = view().flex_row().flex_wrap().gap_2();
    for (index, (title, url)) in links.into_iter().enumerate() {
        let open = nsh_base::button()
            .accessibility_label(title.clone())
            .on_press(move || controller::open_link(url.clone()))
            .child(text(title).underline().text_color(palette().info_ink));
        let remove = super::small_icon_button(Icon::X, "移除会话引用", move || {
            let source = controller::state().with_untracked(|s| s.draft().text);
            let (skill, mut links, body) = draft_parts(&source);
            if index < links.len() {
                links.remove(index);
                controller::set_draft(&compose(skill, links.iter().map(|link| link.source), body));
            }
        });
        row = row.child(view().flex_row().items_center().gap_1().child(open).child(remove));
    }
    row
}

fn paste_into_composer(paste: &PasteEvent, editor: ElementRef, selection: Option<(u32, u32)>) {
    if paste.files.iter().any(|f| f.mime.starts_with("image/")) {
        controller::paste_images(paste);
        return;
    }
    let copied = paste.text.trim();
    if let Some(link) = vantry_protocol::ThreadLink::parse(copied)
        && link.source == copied
    {
        let source = controller::state().with_untracked(|s| s.draft().text);
        let (skill, mut links, body) = draft_parts(&source);
        let (start, end) = selection.unwrap_or_default();
        let mut body = body.to_owned();
        if let (Some(a), Some(b)) = (caret_byte(&body, Some((start, start))), caret_byte(&body, Some((end, end)))) {
            body.replace_range(a.min(b)..a.max(b), "");
        }
        links.push(link);
        controller::set_draft(&compose(skill, links.iter().map(|link| link.source), &body));
        if start != end { editor.set_selection(start.min(end), start.min(end)); }
    } else if !paste.text.is_empty() {
        editor.insert_text(paste.text.clone());
    }
}

pub(crate) fn composer() -> impl IntoElement {
    let colors = palette();
    let draft = computed(|| controller::state().with(|s| s.draft()));
    let body = computed(move || draft.with(|d| draft_parts(&d.text).2.to_owned()));
    let selected_skill = computed(move || draft.with(|d| draft_parts(&d.text).0.map(str::to_owned)));
    let references = computed(move || draft.with(|d| {
        draft_parts(&d.text).1.into_iter().map(|link| (link.title, link.url.to_owned())).collect::<Vec<_>>()
    }));
    let status = computed(|| controller::state().with(|s| s.composer_status()));
    let notice = computed(|| controller::state().with(|s| s.notice.clone()));
    let running_action = computed(|| controller::state().with(|s| s.running_action));
    let queue = computed(|| controller::state().with(|s| s.queue_items()));
    let plan = computed(move || draft.with(|d| d.mode == InteractionMode::Plan));
    let attachments = computed(move || draft.with(|d| d.attachments.clone()));
    let importing =
        computed(|| controller::state().with(|s| s.attachment_imports.contains_key(&s.draft_key())));
    let running = computed(move || status.with(|s| s.can_stop));
    let editing = computed(move || status.with(|s| s.editing));
    let workspace = computed(|| controller::state().with(|s| s.current_project_key()));
    let branch = computed(|| {
        controller::state().with(|s| {
            s.current_workspace()
                .map(|w| w.branch.clone())
                .unwrap_or_default()
        })
    });
    let root = computed(|| {
        if controller::remote_project() {
            return None;
        }
        Some(controller::bench().with(|b| b.root.clone()))
    });
    let endpoint = computed(|| controller::state().with(|s| s.endpoint_label.clone()));
    let has_session = computed(|| controller::state().with(|s| s.has_conversation()));
    let turn_count = computed(|| {
        controller::state().with(|s| {
            s.selected
                .as_deref()
                .map_or(0, |id| s.runs.iter().filter(|r| r.thread_id == id).count())
        })
    });
    let effort_open = signal(false);

    let editor = element_ref();
    let card = element_ref();
    let focused = signal(false);
    let composing = signal(false);
    let selection = signal(None::<(u32, u32)>);
    let dismissed = signal(false);
    let active = signal(0usize);
    let thread = computed(|| controller::state().with(|s| s.draft_key()));
    let overlay_open = computed(|| controller::ui().with(|u|u.settings.is_some() || u.dock_open));
    let previous = stored(false);
    effect(move || { let open=overlay_open.get(); if previous.replace(open) && !open {editor.focus();} });
    effect(move || {
        thread.get();
        editor.focus();
        selection.set(None);
        dismissed.set(false);
        active.set(0);
    });
    let send_with_enter = computed(|| controller::ui().with(|u| u.send_with_enter));
    effect(move || {
        if focused.get() && !send_with_enter.get() && !composing.get() {
            for key in ["ctrl+enter", "mod+enter"] {
                on_key_with(key, KeyOptions { when: nsh_core::keymap::When::Always, ..Default::default() }, |_| controller::submit());
            }
        }
    });
    let skill_context = computed(move || {
        let trigger = active_trigger(&body.get(), selection.get())?;
        if matches!(trigger.kind, TriggerKind::File) { return None; }
        Some(controller::state().with(|s| (s.selected.clone().unwrap_or_default(), s.current_workspace().map(|w| w.path.clone()).unwrap_or_default())))
    });
    let skills = signal(Vec::<(String,String)>::new());
    effect(move || {
        let ctx = skill_context.get();
        skills.set(Vec::new());
        if let Some((id, workspace)) = ctx.clone() {
            spawn(async move {
                let result = controller::composer_skills(id, workspace).await;
                if skill_context.get_untracked() == ctx { skills.set(result); }
            });
        }
    });
    let completion = computed(move || {
        if dismissed.get() || composing.get() {
            return None;
        }
        let text = body.get();
        let trigger = active_trigger(&text, selection.get())?;
        let items = match trigger.kind {
            TriggerKind::Slash => {
                let mut choices = slash_suggestions(&trigger.query);
                choices.extend(skill_suggestions(&skills.get(), &trigger.query));
                choices
            }
            TriggerKind::Skill => skill_suggestions(&skills.get(), &trigger.query),
            TriggerKind::File => discover_files(&root.get()?, &trigger.query),
        };
        (!items.is_empty()).then_some((text, trigger, items))
    });
    let count =
        computed(move || completion.with(|c| c.as_ref().map_or(0, |(_, _, items)| items.len())));
    effect(move || {
        let n = count.get();
        if n == 0 || !focused.get() {
            return;
        }
        let options = KeyOptions {
            when: nsh_core::keymap::When::Always,
            for_focus: true,
            ..Default::default()
        };
        for (key, step) in [("up", n - 1), ("down", 1)] {
            on_key_with(
                key,
                KeyOptions {
                    repeat: true,
                    ..options
                },
                move |_| {
                    active.set((active.get_untracked() + step) % n);
                },
            );
        }
        on_key_with("enter", options, move |_| {
            choose_suggestion(completion, active.get_untracked(), editor)
        });
        on_key_with("escape", options, move |_| dismissed.set(true));
    });

    view()
        .w_full()
        .flex_shrink_0()
        .flex_row()
        .justify_center()
        .px(move || px(if has_session.get() { 28.0 } else { 24.0 }))
        .pt_2()
        .pb(move || px(if has_session.get() { 18.0 } else { 12.0 }))
        .bg(colors.canvas)
        .child(
            view()
                .flex_1()
                .max_w(px(metrics::READING_WIDTH))
                .flex_col()
                .gap_1p5()
                .child(when(
                    move || notice.with(Option::is_some),
                    move || notice_line(notice),
                ))
                .child(when(
                    move || queue.with(|q| !q.is_empty()),
                    move || queue_list(queue),
                ))
                // Popovers above the composer card: @/$/slash completion, thinking effort, model picker
                .child(when(
                    move || completion.with(Option::is_some),
                    move || {
                        overlay()
                            .anchor(&card)
                            .placement(Placement::Top)
                            .align(Align::Start)
                            .offset(px(5.0))
                            .margin(px(8.0))
                            .w(px(360.0))
                            .on_press_outside(move |_: &Point<Points>| dismissed.set(true))
                            .on_cancel(move || dismissed.set(true))
                            .child(completion_popover(completion, active, editor))
                    },
                ))
                .child(when(
                    move || effort_open.get(),
                    move || effort_popover(effort_open),
                ))
                .child(model_picker())
                // Main floating Stardust composer card
                .child(
                    view()
                        .element_ref(&card)
                        .flex_col()
                        .gap_2()
                        .px_3p5()
                        .py_3()
                        .rounded(px(metrics::COMPOSER_RADIUS))
                        .bg(colors.raised)
                        .border_1()
                        .border_color(move || {
                            if editing.get() || plan.get() {
                                colors.accent
                            } else {
                                colors.line
                            }
                        })
                        .shadow_md()
                        .child(when(
                            move || editing.get(),
                            || {
                                view()
                                    .flex_row()
                                    .items_center()
                                    .gap_2()
                                    .child(
                                        text("正在编辑排队消息")
                                            .flex_1()
                                            .text_size(px(11.5))
                                            .text_color(colors.accent),
                                    )
                                    .child(link_button("取消编辑", controller::cancel_queue_edit))
                            },
                        ))
                        .child(when(
                            move || attachments.with(|a| !a.is_empty()),
                            move || attachment_chips(attachments),
                        ))
                        .child(when(
                            move || importing.get(),
                            move || super::caption("正在添加附件…"),
                        ))
                        .child(when(move || selected_skill.with(Option::is_some), move || {
                            view().flex_row().items_center().gap_1()
                                .child(icon(Icon::Sparkles, 13.0, colors.info_ink))
                                .child(text(move || selected_skill.get().unwrap_or_default()).text_color(colors.info_ink))
                                .child(super::small_icon_button(Icon::X, "移除技能", || {
                                    let source = controller::state().with_untracked(|s| s.draft().text);
                                    controller::set_draft(crate::state::thread_links::skill_prefix(&source).1);
                                }))
                        }))
                        .child(when(
                            move || references.with(|links| !links.is_empty()),
                            move || dynamic(move || reference_chips(references.get())),
                        ))
                        .child(
                            nsh_base::input()
                                .element_ref(&editor)
                                .label("消息输入框")
                                .on_focus(move || focused.set(true))
                                .on_blur(move || focused.set(false))
                                .on_composition_change(move |e: &Composition| {
                                    composing.set(e.composing)
                                })
                                .on_selection_change(move |e: &Selection| {
                                    let next = Some((e.start, e.end));
                                    if selection.get_untracked() != next {
                                        selection.set(next);
                                        active.set(0);
                                        dismissed.set(false);
                                    }
                                })
                                .multiline(true)
                                .submit_behavior(move || if send_with_enter.get() { SubmitBehavior::Submit } else { SubmitBehavior::Newline })
                                .value(move || body.get())
                                .paste_behavior(PasteBehavior::Report)
                                .on_paste(move |paste: &PasteEvent| {
                                    paste_into_composer(paste, editor, selection.get_untracked());
                                })
                                .on_change(move |text: &str| {
                                    dismissed.set(false);
                                    active.set(0);
                                    let source = controller::state().with_untracked(|s| with_body(&s.draft().text, text));
                                    controller::set_draft(&source);
                                })
                                .on_submit(move |_: &str| {
                                    if composing.get_untracked() {
                                        return;
                                    }
                                    if count.get_untracked() > 0 {
                                        choose_suggestion(
                                            completion,
                                            active.get_untracked(),
                                            editor,
                                        );
                                    } else {
                                        controller::submit();
                                    }
                                })
                                .placeholder("问点什么… @ 文件、$ 技能、/ 命令")
                                .placeholder_color(move || color(colors.faint))
                                .caret_color(move || color(colors.accent))
                                .auto_focus(true)
                                .auto_capitalize(AutoCapitalize::None)
                                .auto_correct(false)
                                .min_h(px(44.0))
                                .max_h(px(220.0))
                                .px_0p5()
                                .text_size(px(metrics::BODY + 0.5))
                                .line_height(LineHeight::Multiple(1.5))
                                .text_color(colors.ink),
                        )
                        .child(toolbar(status, plan, running, running_action, effort_open))
                        .child(when(
                            move || running.get(),
                            move || {
                                text(move || match running_action.get() {
                                    RunningAction::Steer => {
                                        "运行中 · 发送将作为纠偏追加给当前任务 · Esc 停止"
                                    }
                                    RunningAction::Queue => {
                                        "运行中 · 发送将加入排队并在本轮结束后执行 · Esc 停止"
                                    }
                                })
                                .text_size(px(11.0))
                                .text_color(colors.faint)
                            },
                        )),
                )
                // Quiet host / workspace context strip + turn indicator below the card (matches v0.1.42)
                .child(context_strip(endpoint, workspace, branch, turn_count)),
        )
}

fn context_strip(
    endpoint: Memo<String>,
    workspace: Memo<String>,
    branch: Memo<String>,
    turn_count: Memo<usize>,
) -> impl IntoElement {
    let colors = palette();
    view()
        .flex_col()
        .gap_1()
        .px_1()
        .child(
            view()
                .h(px(26.0))
                .flex_row()
                .items_center()
                .gap_2()
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_1()
                        .child(icon(Icon::Server, 12.0, colors.faint))
                        .child(
                            text(move || endpoint.get())
                                .text_size(px(11.0))
                                .text_color(colors.faint),
                        ),
                )
                .child(text("|").text_size(px(11.0)).text_color(colors.line))
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_1()
                        .child(icon(Icon::Folder, 12.0, colors.faint))
                        .child(
                            text(move || {
                                let ws = workspace.get();
                                if ws.is_empty() {
                                    "无项目".to_owned()
                                } else {
                                    crate::state::sidebar::project_name(&ws).to_owned()
                                }
                            })
                            .text_size(px(11.0))
                            .text_color(colors.faint),
                        ),
                )
                .child(when(
                    move || !workspace.with(String::is_empty),
                    move || {
                        text(|| {
                            controller::state().with(|s| {
                                if s.current_workspace().is_some_and(|w| {
                                    w.kind == "worktree" || !w.repository.is_empty()
                                }) {
                                    "· 隔离分支"
                                } else {
                                    "· 本地目录"
                                }
                            })
                        })
                        .text_size(px(11.0))
                        .text_color(colors.faint)
                    },
                ))
                .child(view().flex_1())
                .child(when(
                    move || branch.with(|b| !b.is_empty()),
                    move || {
                        view()
                            .flex_row()
                            .items_center()
                            .gap_1()
                            .child(icon(Icon::GitBranch, 12.0, colors.faint))
                            .child(
                                text(move || branch.get())
                                    .number_of_lines(1u32)
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
                            .child(dot(colors.success))
                            .child(
                                text(move || format!("第 {} 轮", turn_count.get()))
                                    .text_size(px(11.0))
                                    .text_color(colors.muted),
                            ),
                    )
            },
        ))
}

fn toolbar(
    status: Memo<crate::state::ComposerStatus>,
    plan: Memo<bool>,
    running: Memo<bool>,
    running_action: Memo<RunningAction>,
    effort_open: Signal<bool>,
) -> impl IntoElement {
    let colors = palette();
    let policy = computed(|| controller::state().with(|s| s.current_policy()));
    let model = computed(|| controller::state().with(|s| s.current_model()));
    let thinking = computed(move || {
        let (levels, models) = controller::ui().with(|u| u.thinking_catalog());
        let current = model.get();
        let (_, level) = split_thinking(&current, &models);
        (levels, level.to_owned())
    });
    let can_submit = move || status.with(|s| s.can_submit);
    let show_stop = move || running.get() && !can_submit();

    view()
        .flex_row()
        .items_center()
        .gap_2()
        // Left: clean `+` attach button & `[list-checks] 规划` pill
        .child(
            nsh_base::button()
                .accessibility_label("添加附件")
                .on_press(controller::pick_attachments)
                .w(px(28.0))
                .h(px(28.0))
                .items_center()
                .justify_center()
                .rounded_full()
                .hover(|s| s.bg(colors.soft))
                .active(|s| s.opacity(0.65))
                .child(icon(Icon::Plus, 16.0, colors.muted)),
        )
        .child(
            nsh_base::button()
                .accessibility_label("规划模式")
                .selected(move || plan.get())
                .on_press(controller::toggle_mode)
                .flex_row()
                .items_center()
                .gap_1p5()
                .px_2p5()
                .py_1()
                .rounded_full()
                .border_1()
                .border_color(move || {
                    if plan.get() {
                        colors.accent
                    } else {
                        colors.line
                    }
                })
                .bg(move || {
                    if plan.get() {
                        colors.approval
                    } else {
                        AdaptiveColor::TRANSPARENT
                    }
                })
                .hover(|s| s.bg(colors.soft))
                .active(|s| s.opacity(0.7))
                .child(icon(Icon::ListChecks, 13.5, move || {
                    if plan.get() {
                        colors.accent
                    } else {
                        colors.muted
                    }
                }))
                .child(text("规划").text_size(px(12.0)).text_color(move || {
                    if plan.get() {
                        colors.accent
                    } else {
                        colors.ink
                    }
                })),
        )
        .child(when(
            move || running.get(),
            move || running_action_switch(running_action),
        ))
        .child(view().flex_1())
        // Right: tinted permission pill, unified `π model | ● ▾` capsule, circular send/stop
        .child(policy_switch(policy))
        .child(
            view()
                .flex_row()
                .items_center()
                .rounded_full()
                .bg(colors.soft)
                .border_1()
                .border_color(colors.line_soft)
                .child(
                    nsh_base::button()
                        .accessibility_label("选择模型")
                        .on_press(move || {
                            effort_open.set(false);
                            controller::toggle_model_picker();
                        })
                        .flex_row()
                        .items_center()
                        .gap_1p5()
                        .max_w(px(190.0))
                        .pl_2p5()
                        .pr_2()
                        .py_1()
                        .rounded_full()
                        .hover(|s| s.bg(colors.card))
                        .active(|s| s.opacity(0.7))
                        .child(
                            text("π")
                                .text_size(px(12.5))
                                .font_weight(FontWeight::BOLD)
                                .text_color(colors.accent),
                        )
                        .child(
                            text(move || {
                                let raw = model.get();
                                let (_, models) =
                                    controller::ui().with_untracked(|u| u.thinking_catalog());
                                let (base, _) = split_thinking(&raw, &models);
                                if base.is_empty() {
                                    "选择模型".to_owned()
                                } else {
                                    base.rsplit('/').next().unwrap_or(base).to_owned()
                                }
                            })
                            .number_of_lines(1u32)
                            .text_size(px(12.0))
                            .font_weight(FontWeight::MEDIUM)
                            .text_color(colors.ink),
                        ),
                )
                .child(view().w(px(1.0)).h(px(14.0)).bg(colors.line))
                .child(
                    nsh_base::button()
                        .accessibility_label("推理强度")
                        .on_press(move || {
                            effort_open.set(!effort_open.get_untracked());
                        })
                        .flex_row()
                        .items_center()
                        .gap_1()
                        .pl_2()
                        .pr_2p5()
                        .py_1()
                        .rounded_full()
                        .hover(|s| s.bg(colors.card))
                        .active(|s| s.opacity(0.7))
                        .child(dot(move || {
                            effort_dot_color(&thinking.with(|t| t.1.clone()))
                        }))
                        .child(when(
                            move || !thinking.with(|t| t.1.is_empty()),
                            move || {
                                text(move || thinking_label(&thinking.with(|t| t.1.clone())))
                                    .text_size(px(11.0))
                                    .text_color(colors.muted)
                            },
                        ))
                        .child(icon(Icon::ChevronDown, 12.0, colors.faint)),
                ),
        )
        .child(when(
            move || status.with(|s| s.retry.is_some()),
            move || {
                view()
                    .flex_row()
                    .gap_1p5()
                    .child(ghost_button("放弃", move || {
                        if let Some(key) = status.get_untracked().retry {
                            controller::abandon(key);
                        }
                    }))
                    .child(ghost_button("重试", move || {
                        if let Some(key) = status.get_untracked().retry {
                            controller::retry(key);
                        }
                    }))
            },
        ))
        // Circular send / stop button (exact v0.1.42 Stardust feel)
        .child(
            nsh_base::button()
                .accessibility_label(move || {
                    Some(SharedString::from(if show_stop() {
                        "停止"
                    } else {
                        "发送"
                    }))
                })
                .on_press(move || {
                    if show_stop() {
                        controller::stop();
                    } else {
                        effort_open.set(false);
                        controller::submit();
                    }
                })
                .disabled(move || !show_stop() && !can_submit())
                .w(px(30.0))
                .h(px(30.0))
                .items_center()
                .justify_center()
                .rounded_full()
                .bg(move || {
                    if show_stop() {
                        colors.ink
                    } else if can_submit() {
                        colors.accent
                    } else {
                        colors.soft
                    }
                })
                .active(|s| s.opacity(0.75))
                .child(dynamic(move || {
                    if show_stop() {
                        icon(Icon::Square, 12.0, colors.canvas).into_element()
                    } else {
                        icon(
                            Icon::ArrowUp,
                            15.0,
                            if can_submit() {
                                colors.on_accent
                            } else {
                                colors.faint
                            },
                        )
                        .into_element()
                    }
                })),
        )
}

fn effort_dot_color(level: &str) -> AdaptiveColor {
    let colors = palette();
    match level {
        "off" | "minimal" | "low" => colors.faint,
        "medium" => colors.info_ink,
        "high" => colors.warning,
        "xhigh" | "max" => colors.accent,
        _ => colors.muted,
    }
}

/// Interactive Thinking Effort popover above the composer (`v0.1.42` style).
fn effort_popover(open: Signal<bool>) -> impl IntoElement {
    let colors = palette();
    let model = computed(|| controller::state().with(|s| s.current_model()));
    let catalog = computed(|| controller::ui().with(|u| u.thinking_catalog()));
    let current_level = computed(move || {
        let (_, models) = catalog.get();
        split_thinking(&model.get(), &models).1.to_owned()
    });
    let levels = computed(move || {
        let (mut list, _) = catalog.get();
        if list.is_empty() {
            list = vec!["low".into(), "medium".into(), "high".into(), "xhigh".into()];
        }
        list
    });

    view().w_full().flex_row().justify_end().child(
        view()
            .w(px(330.0))
            .flex_col()
            .gap_2()
            .p_3()
            .rounded(px(14.0))
            .bg(colors.card)
            .border_1()
            .border_color(colors.line)
            .shadow_md()
            .child(
                view()
                    .flex_row()
                    .items_center()
                    .gap_1p5()
                    .child(dot(move || effort_dot_color(&current_level.get())))
                    .child(
                        text(move || {
                            let label = thinking_label(&current_level.get());
                            format!("推理强度 · {label}")
                        })
                        .flex_1()
                        .text_size(px(12.5))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.ink),
                    )
                    .child(
                        nsh_base::button()
                            .accessibility_label("关闭")
                            .on_press(move || open.set(false))
                            .w(px(22.0))
                            .h(px(22.0))
                            .items_center()
                            .justify_center()
                            .rounded(px(6.0))
                            .hover(|s| s.bg(colors.soft))
                            .child(icon(Icon::X, 12.0, colors.faint)),
                    ),
            )
            .child(
                view()
                    .flex_row()
                    .items_center()
                    .gap_1()
                    .p_1()
                    .rounded(px(10.0))
                    .bg(colors.soft)
                    .child(
                        nsh_base::button()
                            .on_press(move || {
                                controller::set_thinking_level(String::new());
                                open.set(false);
                            })
                            .flex_1()
                            .py_1()
                            .items_center()
                            .justify_center()
                            .rounded(px(7.0))
                            .bg(move || {
                                if current_level.with(|l| l.is_empty()) {
                                    colors.card
                                } else {
                                    AdaptiveColor::TRANSPARENT
                                }
                            })
                            .child(text("默认").text_size(px(11.5)).text_color(move || {
                                if current_level.with(|l| l.is_empty()) {
                                    colors.ink
                                } else {
                                    colors.muted
                                }
                            })),
                    )
                    .child(each(
                        move || levels.get(),
                        |l: &String| l.clone(),
                        move |lvl, _| {
                            let value = move || lvl.get_untracked();
                            let selected = move || current_level.get() == lvl.get();
                            nsh_base::button()
                                .on_press(move || {
                                    controller::set_thinking_level(value());
                                    open.set(false);
                                })
                                .flex_1()
                                .py_1()
                                .items_center()
                                .justify_center()
                                .rounded(px(7.0))
                                .bg(move || {
                                    if selected() {
                                        colors.card
                                    } else {
                                        AdaptiveColor::TRANSPARENT
                                    }
                                })
                                .child(
                                    text(move || thinking_label(&lvl.get()))
                                        .text_size(px(11.5))
                                        .text_color(move || {
                                            if selected() { colors.ink } else { colors.muted }
                                        }),
                                )
                        },
                    )),
            ),
    )
}

/// `@` file / `$` skill / `/` slash command completion popover above the composer.
fn completion_popover(
    state: Memo<Completions>,
    active: Signal<usize>,
    editor: ElementRef,
) -> impl IntoElement {
    let colors = palette();
    let header_label = move || {
        state.with(|s| match s.as_ref().map(|(_, t, _)| &t.kind) {
            Some(TriggerKind::File) => "引用工作区文件 (@)",
            Some(TriggerKind::Skill) => "插入技能 ($)",
            Some(TriggerKind::Slash) => "快捷命令 (/)",
            None => "",
        })
    };
    let items = computed(move || {
        state.with(|s| {
            s.as_ref()
                .map(|(_, _, items)| items.clone())
                .unwrap_or_default()
        })
    });

    view()
        .flex_col()
        .gap_1()
        .p_2()
        .rounded(px(12.0))
        .bg(colors.card)
        .border_1()
        .border_color(colors.line)
        .shadow_md()
        .child(super::caption(header_label))
        .child(
            scroll()
                .max_h(px(210.0))
                .child(view().flex_col().gap_0p5().child(each(
                    move || items.get(),
                    |item: &Suggestion| item.key.clone(),
                    move |item, position| {
                        let selected = computed(move || active.get() == position.get());
                        let row = element_ref();
                        effect(move || {
                            if selected.get() {
                                row.reveal(
                                    nsh_core::RevealOptions::default()
                                        .vertical(nsh_core::ScrollAlignment::Nearest)
                                        .animated(false),
                                );
                            }
                        });
                        let glyph = match item.with_untracked(|i| i.kind.clone()) {
                            TriggerKind::File => Icon::FileText,
                            TriggerKind::Skill => Icon::Sparkles,
                            TriggerKind::Slash => Icon::Zap,
                        };
                        nsh_base::button()
                            .element_ref(&row)
                            .selected(move || selected.get())
                            .on_press(move || {
                                choose_suggestion(state, position.get_untracked(), editor)
                            })
                            .bg(move || {
                                if selected.get() {
                                    colors.soft
                                } else {
                                    AdaptiveColor::TRANSPARENT
                                }
                            })
                            .w_full()
                            .flex_row()
                            .items_center()
                            .gap_2()
                            .px_2p5()
                            .py_1p5()
                            .rounded(px(7.0))
                            .hover(|s| s.bg(colors.soft))
                            .active(|s| s.opacity(0.75))
                            .child(icon(glyph, 13.5, colors.muted))
                            .child(
                                text(move || item.with(|i| i.label.clone()))
                                    .font_family("Menlo")
                                    .text_size(px(12.0))
                                    .text_color(colors.ink),
                            )
                            .child(
                                text(move || item.with(|i| i.detail.clone()))
                                    .flex_1()
                                    .number_of_lines(1u32)
                                    .text_size(px(11.0))
                                    .text_color(colors.faint),
                            )
                    },
                ))),
        )
}

/// Auto / edits auto / ask tinted pill matching `v0.1.42`'s soft-blue `自动`.
fn policy_switch(policy: Memo<String>) -> impl IntoElement {
    let colors = palette();
    let style = move || match policy.get().as_str() {
        "auto" => (colors.info_bg, colors.info_ink, Icon::ShieldCheck, "自动"),
        "auto-accept-edits" => (
            colors.success_bg,
            colors.success,
            Icon::Shield,
            "改文件自动",
        ),
        _ => (colors.approval, colors.warning, Icon::ShieldOff, "询问"),
    };
    nsh_base::button()
        .accessibility_label("权限策略")
        .on_press(move || {
            let current = policy.get_untracked();
            let at = APPROVAL_POLICIES
                .iter()
                .position(|(value, _)| *value == current)
                .map_or(0, |i| (i + 1) % APPROVAL_POLICIES.len());
            controller::set_policy(APPROVAL_POLICIES[at].0.to_owned());
        })
        .flex_row()
        .items_center()
        .gap_1()
        .px_2p5()
        .py_1()
        .rounded_full()
        .bg(move || style().0)
        .active(|s| s.opacity(0.75))
        .child(dynamic(move || {
            let (_, fg, glyph, _) = style();
            icon(glyph, 13.0, fg).into_element()
        }))
        .child(
            text(move || style().3)
                .text_size(px(12.0))
                .font_weight(FontWeight::MEDIUM)
                .text_color(move || style().1),
        )
}

fn running_action_switch(action: Memo<RunningAction>) -> impl IntoElement {
    let colors = palette();
    view()
        .flex_row()
        .items_center()
        .p_0p5()
        .rounded_full()
        .bg(colors.soft)
        .child(pill(
            PrimaryAction::Steer.label(),
            move || action.get() == RunningAction::Steer,
            || controller::set_running_action(RunningAction::Steer),
        ))
        .child(pill(
            PrimaryAction::Queue.label(),
            move || action.get() == RunningAction::Queue,
            || controller::set_running_action(RunningAction::Queue),
        ))
}

pub(super) fn attachment_thumbnail(item: Attachment) -> impl IntoElement {
    let colors = palette();
    let preview = signal(None::<String>);
    let source = controller::state().with_untracked(|s| {
        s.drafts
            .get(&s.draft_key())
            .and_then(|d| d.image_paths.get(&item.id))
            .cloned()
    });
    let show = signal(false);
    let name = item.name.clone();
    spawn(async move {
        preview.set(controller::attachment_preview(item, source).await);
    });
    view().on_press(move ||show.set(true)).label("预览图片附件")
        .child(when(move ||show.get(),move ||{
            let name=name.clone();
            nsh_base::dialog().on_cancel(move ||show.set(false)).child(view().flex_1().items_center().justify_center().bg(colors.scrim)
                .child(view().flex_col().p_3().gap_2().rounded(px(16.0)).bg(colors.card)
                    .child(view().flex_row().items_center().child(super::caption(name).flex_1()).child(super::icon_button(Icon::X,"关闭图片预览",move ||show.set(false))))
                    .child(dynamic(move || match preview.get(){Some(src)=>image(src).w(move ||px(nsh_core::element::window().width.0*0.72)).h(move ||px(nsh_core::element::window().height.0*0.62)).object_fit(nsh_core::ResizeMode::Contain).into_element(),None=>super::caption("正在载入图片…").into_element()}))))
        }))
        .w(px(46.0))
        .h(px(38.0))
        .rounded(px(5.0))
        .overflow_hidden()
        .items_center()
        .justify_center()
        .child(dynamic(move || match preview.get() {
            Some(src) => image(src)
                .w_full()
                .h_full()
                .object_fit(nsh_core::ResizeMode::Cover)
                .into_element(),
            None => icon(Icon::File, 14.0, colors.muted).into_element(),
        }))
}

fn attachment_chips(attachments: Memo<Vec<Attachment>>) -> impl IntoElement {
    let colors = palette();
    view().flex_row().flex_wrap().gap_1p5().child(each(
        move || attachments.get(),
        |a: &Attachment| a.id.clone(),
        move |attachment, _| {
            let id = move || attachment.with_untracked(|a| a.id.clone());
            view()
                .flex_row()
                .items_center()
                .gap_1()
                .pl_2()
                .pr_1()
                .py_0p5()
                .rounded(px(8.0))
                .bg(colors.soft)
                .child(dynamic(move || {
                    let item = attachment.get();
                    if item.mime.starts_with("image/") {
                        attachment_thumbnail(item).into_element()
                    } else {
                        icon(Icon::Paperclip, 12.0, colors.muted).into_element()
                    }
                }))
                .child(when(move || attachment.with(|a| !a.mime.starts_with("image/")), move ||
                    text(move || attachment.with(|a| a.name.clone()))
                        .number_of_lines(1u32)
                        .max_w(px(220.0))
                        .text_size(px(11.5))
                        .text_color(colors.ink)
                ))
                .child(
                    nsh_base::button()
                        .accessibility_label("移除附件")
                        .on_press(move || controller::remove_attachment(id()))
                        .w(px(18.0))
                        .h(px(18.0))
                        .items_center()
                        .justify_center()
                        .rounded(px(4.0))
                        .hover(|s| s.bg(colors.card))
                        .active(|s| s.opacity(0.6))
                        .child(icon(Icon::X, 11.0, colors.faint)),
                )
        },
    ))
}

fn queue_list(queue: Memo<Vec<QueueItem>>) -> impl IntoElement {
    let colors = palette();
    view()
        .flex_col()
        .gap_1()
        .p_2()
        .rounded(px(12.0))
        .bg(colors.soft)
        .child(super::caption(move || {
            queue.with(|q| {
                let queued = q.iter().filter(|i| i.kind == QueueItemKind::Queued).count();
                format!("排队 {queued} 条 · 当前任务结束后依次发送")
            })
        }))
        .child(each(
            move || queue.get(),
            |item: &QueueItem| item.id.clone(),
            |item, _| queue_row(item),
        ))
}

fn queue_row(item: ReadSignal<QueueItem>) -> impl IntoElement {
    let colors = palette();
    let id = move || item.with_untracked(|i| i.id.clone());
    let queued = move || item.with(|i| i.kind == QueueItemKind::Queued);
    let busy = move || item.with(|i| i.busy);
    let first = move || item.with(|i| i.index == 0);
    let last = move || item.with(|i| i.index + 1 >= i.count);
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_2()
        .py_1()
        .rounded(px(8.0))
        .bg(move || {
            if item.with(|i| i.editing) {
                colors.card
            } else {
                AdaptiveColor::TRANSPARENT
            }
        })
        .child(
            text(move || item.with(|i| i.status.clone()))
                .text_size(px(10.5))
                .text_color(move || {
                    if queued() {
                        colors.faint
                    } else {
                        colors.accent
                    }
                }),
        )
        .child(
            text(move || {
                item.with(|i| {
                    if i.attachments > 0 {
                        format!("{}（附件 {}）", i.text, i.attachments)
                    } else {
                        i.text.clone()
                    }
                })
            })
            .flex_1()
            .number_of_lines(1u32)
            .text_size(px(12.5))
            .text_color(colors.ink),
        )
        .child(when(queued, move || {
            view()
                .flex_row()
                .gap_0p5()
                .child(link_button("编辑", move || controller::edit_queued(id())).disabled(busy))
                .child(
                    link_button("上移", move || controller::move_queued(id(), -1))
                        .disabled(move || busy() || first()),
                )
                .child(
                    link_button("下移", move || controller::move_queued(id(), 1))
                        .disabled(move || busy() || last()),
                )
                .child(
                    link_button("立即追加", move || controller::promote_queued(id()))
                        .disabled(busy),
                )
                .child(link_button("取消", move || controller::cancel_queued(id())).disabled(busy))
        }))
}

/// The model picker: search, the catalog's models, a free entry.
fn model_picker() -> impl IntoElement {
    let colors = palette();
    let open = computed(|| controller::ui().with(|u| u.model_picker));
    let query = computed(|| controller::ui().with(|u| u.model_query.clone()));
    let choices = computed(|| controller::ui().with(|u| u.model_choices()));
    let loading = computed(|| {
        controller::ui().with(|u| matches!(u.catalogs, crate::state::ui::Catalogs::Loading))
    });
    let current = computed(|| controller::state().with(|s| s.current_model()));
    when(
        move || open.get(),
        move || {
            view()
                .flex_col()
                .gap_1()
                .p_2p5()
                .rounded(px(12.0))
                .bg(colors.card)
                .border_1()
                .border_color(colors.line)
                .shadow_md()
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .px_1()
                        .child(icon(Icon::Search, 13.5, colors.faint))
                        .child(
                            nsh_base::input()
                                .value(move || query.get())
                                .on_change(|t: &str| controller::set_model_query(t))
                                .on_submit(move |_: &str| {
                                    if let Some((id, _)) = choices.get_untracked().first() {
                                        controller::set_model(id.clone());
                                    }
                                })
                                .placeholder("搜索或输入 provider/model")
                                .placeholder_color(move || color(colors.faint))
                                .auto_focus(true)
                                .auto_correct(false)
                                .flex_1()
                                .text_size(px(12.5))
                                .text_color(colors.ink),
                        )
                        .child(
                            nsh_base::button()
                                .accessibility_label("关闭")
                                .on_press(controller::toggle_model_picker)
                                .w(px(22.0))
                                .h(px(22.0))
                                .items_center()
                                .justify_center()
                                .rounded(px(6.0))
                                .hover(|s| s.bg(colors.soft))
                                .child(icon(Icon::X, 12.0, colors.faint)),
                        ),
                )
                .child(when(
                    move || loading.get(),
                    || {
                        text("正在读取模型目录…")
                            .text_size(px(11.5))
                            .text_color(colors.faint)
                    },
                ))
                .child(
                    scroll()
                        .max_h(px(220.0))
                        .child(view().flex_col().child(each(
                            move || choices.get(),
                            |c: &(String, String)| c.0.clone(),
                            move |choice, _| {
                                let id = move || choice.with_untracked(|c| c.0.clone());
                                let on = move || current.get() == choice.with(|c| c.0.clone());
                                nsh_base::button()
                                    .on_press(move || controller::set_model(id()))
                                    .flex_row()
                                    .items_center()
                                    .gap_2()
                                    .px_2()
                                    .py_1p5()
                                    .rounded(px(7.0))
                                    .hover(|s| s.bg(colors.soft))
                                    .child(
                                        text(move || choice.with(|c| c.1.clone()))
                                            .flex_1()
                                            .text_size(px(12.5))
                                            .text_color(colors.ink),
                                    )
                                    .child(
                                        text(move || choice.with(|c| c.0.clone()))
                                            .font_family("Menlo")
                                            .text_size(px(11.0))
                                            .text_color(colors.faint),
                                    )
                                    .child(when(on, || icon(Icon::Check, 13.5, colors.accent)))
                            },
                        ))),
                )
        },
    )
}

fn notice_line(notice: Memo<Option<crate::state::Notice>>) -> impl IntoElement {
    let colors = palette();
    let level = move || notice.with(|n| n.as_ref().map(|n| n.level));
    let level_color = move || match level() {
        Some(NoticeLevel::Error) => colors.danger,
        Some(NoticeLevel::Warn) => colors.warning,
        _ => colors.success,
    };
    let bg_color = move || match level() {
        Some(NoticeLevel::Error) | Some(NoticeLevel::Warn) => colors.approval,
        _ => colors.soft,
    };
    let border_color = move || match level() {
        Some(NoticeLevel::Error) | Some(NoticeLevel::Warn) => colors.approval_border,
        _ => colors.line_soft,
    };
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_3()
        .py_1p5()
        .rounded(px(10.0))
        .bg(bg_color)
        .border_1()
        .border_color(border_color)
        .child(dot(level_color))
        .child(
            text(move || notice.with(|n| n.as_ref().map(|n| n.text.clone()).unwrap_or_default()))
                .flex_1()
                .select_text()
                .text_size(px(12.0))
                .text_color(colors.ink),
        )
        .child(
            nsh_base::button()
                .accessibility_label("关闭提示")
                .on_press(controller::dismiss_notice)
                .w(px(20.0))
                .h(px(20.0))
                .items_center()
                .justify_center()
                .rounded(px(5.0))
                .hover(|s| s.bg(colors.card))
                .active(|s| s.opacity(0.6))
                .child(icon(Icon::X, 11.5, colors.faint)),
        )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn completion_uses_the_native_caret_and_keeps_the_rest_of_the_draft() {
        let draft = "🌱 看 @Doc旧 后文";
        let caret = "🌱 看 @Do".encode_utf16().count() as u32;
        let trigger = active_trigger(draft, Some((caret, caret))).unwrap();
        assert_eq!(trigger.query, "Do");
        let (text, pos) = replace_trigger(draft, &trigger, "@Docs/说明.md ");
        assert_eq!(text, "🌱 看 @Docs/说明.md 后文");
        assert_eq!(pos, "🌱 看 @Docs/说明.md ".encode_utf16().count() as u32);
        assert!(active_trigger(&text, Some((pos, pos))).is_none());
        assert!(active_trigger(draft, Some((1, 1))).is_none());
        assert!(active_trigger(draft, Some((caret, caret + 1))).is_none());
        assert!(active_trigger(draft, Some((99, 99))).is_none());
    }

    #[test]
    fn slash_commands_follow_the_current_line() {
        let text = "先保留这行\n  /te";
        let trigger = active_trigger(text, None).unwrap();
        assert_eq!(slash_suggestions(&trigger.query)[0].label, "/terminal");
        assert_eq!(replace_trigger(text, &trigger, "").0, "先保留这行\n  ");
        assert!(active_trigger("正文 /te", None).is_none());
    }

    #[test]
    fn parses_file_skill_and_slash_triggers() {
        assert_eq!(active_trigger("", None), None);
        assert_eq!(active_trigger("hello ", None), None);
        assert_eq!(
            active_trigger("请看 @Cargo", None),
            Some(CompletionTrigger {
                kind: TriggerKind::File,
                start: "请看 ".len(),
                end: "请看 @Cargo".len(),
                query: "Cargo".into(),
            })
        );
        assert_eq!(
            active_trigger("用 $docx", None),
            Some(CompletionTrigger {
                kind: TriggerKind::Skill,
                start: "用 ".len(),
                end: "用 $docx".len(),
                query: "docx".into(),
            })
        );
        assert_eq!(
            active_trigger("/pl", None),
            Some(CompletionTrigger {
                kind: TriggerKind::Slash,
                start: 0,
                end: 3,
                query: "pl".into(),
            })
        );
        assert_eq!(active_trigger("not a /slash", None), None);
        assert!(!slash_suggestions("pl").is_empty());
    }
}
