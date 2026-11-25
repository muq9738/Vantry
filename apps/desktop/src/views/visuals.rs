//! Native NSH renderers for Vantry structured & interactive reply blocks:
//! - Interactive: `ask` (single/multi choice), `dispatch` (multi-agent confirmation), `vantry-ui`
//! - Data & specs: `chart` (pipe-table & Studio JSON), `limits`, `kv`, `callout`, `timeline`, `tree`, `api`
//! - Diagrams & artifacts: `mermaid`/`flow`/`state`/`sequence`/`er`/`class`, `math`, `svg`/`html`/`artifact`

use nsh_core::{LineHeight, prelude::*};
use serde_json::Value;

use super::{badge, ghost_button, link_button, primary_button};
use crate::icons::Icon;
use crate::{
    controller,
    theme::{metrics, palette},
};

const MONO: &str = "Menlo";

/// Try to render a fenced code block as a structured/interactive Vantry visual block.
/// Returns `None` for ordinary code languages so they stay in `code_view`.
pub(crate) fn render_fenced(lang_info: &str, source: &str, closed: bool) -> Option<AnyElement> {
    let trimmed = lang_info.trim();
    let (kind, args) = match trimmed.split_once(char::is_whitespace) {
        Some((k, rest)) => (k.to_ascii_lowercase(), rest.trim()),
        None => (trimmed.to_ascii_lowercase(), ""),
    };
    match kind.as_str() {
        "ask" => Some(ask_card(args, source, closed).into_element()),
        "dispatch" | "orchestrate" => Some(dispatch_card(source, closed).into_element()),
        "callout" => Some(callout_card(args, source).into_element()),
        "kv" => Some(kv_card(source).into_element()),
        "limits" => Some(limits_card(source).into_element()),
        "timeline" => Some(timeline_card(source).into_element()),
        "tree" => Some(tree_card(source).into_element()),
        "api" => Some(api_card(source).into_element()),
        "chart" | "vantry-chart" => {
            Some(super::charts::render_chart_card(args, source).into_element())
        }
        "vantry-ui" => Some(vantry_ui_card(source).into_element()),
        "mermaid" | "flow" | "state" | "sequence" | "er" | "class" => {
            Some(super::charts::render_diagram_card(&kind, args, source).into_element())
        }
        "math" | "latex" | "tex" => Some(math_card(source).into_element()),
        "svg" | "html" | "artifact" => Some(artifact_card(&kind, args, source).into_element()),
        _ => None,
    }
}

pub(crate) fn card_shell(header: impl IntoElement, body: impl IntoElement) -> View {
    let colors = palette();
    view()
        .flex_col()
        .rounded(px(metrics::CARD_RADIUS))
        .bg(colors.card)
        .border_1()
        .border_color(colors.line)
        .overflow_hidden()
        .child(header)
        .child(body)
}

pub(crate) fn card_header(tag: &str, title: &str, source: String) -> View {
    let colors = palette();
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .px_3()
        .py_1p5()
        .bg(colors.soft)
        .border_b_1()
        .border_b_color(colors.line_soft)
        .child(badge(tag.to_owned(), colors.accent))
        .child(
            text(title.to_owned())
                .flex_1()
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(px(12.5))
                .text_color(colors.ink),
        )
        .child(link_button("复制", move || controller::copy(source.clone())))
}

// ---------------------------------------------------------------------------
// 1. Interactive `ask` card
// ---------------------------------------------------------------------------

#[derive(Clone)]
struct AskOption {
    label: String,
    note: String,
    recommended: bool,
}

fn parse_ask(source: &str) -> (String, Vec<AskOption>) {
    let mut question = String::new();
    let mut options = Vec::new();
    for raw in source.lines() {
        let line = raw.trim();
        if line.is_empty() {
            continue;
        }
        if let Some(rest) = line
            .strip_prefix("* ")
            .or_else(|| line.strip_prefix("- "))
        {
            let recommended = line.starts_with("* ");
            let (label, note) = match rest.split_once('|') {
                Some((l, n)) => (l.trim().to_owned(), n.trim().to_owned()),
                None => (rest.trim().to_owned(), String::new()),
            };
            if !label.is_empty() {
                options.push(AskOption {
                    label,
                    note,
                    recommended,
                });
            }
        } else if question.is_empty() && options.is_empty() {
            question = line.to_owned();
        }
    }
    (question, options)
}

fn ask_card(args: &str, source: &str, closed: bool) -> impl IntoElement {
    let colors = palette();
    let multi = args
        .split_whitespace()
        .any(|t| t.eq_ignore_ascii_case("multi"));
    let (question, options) = parse_ask(source);
    let default_selected: Vec<bool> = options.iter().map(|o| o.recommended).collect();
    let selected = signal(default_selected);
    let custom = signal(String::new());
    let submitted = signal(false);

    let mut body = view().flex_col().gap_2().p_3();
    if !question.is_empty() {
        body = body.child(
            text(question.clone())
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(px(14.0))
                .text_color(colors.ink),
        );
    }

    for (idx, opt) in options.iter().enumerate() {
        let label = opt.label.clone();
        let note = opt.note.clone();
        let recommended = opt.recommended;
        let on = move || selected.with(|v| v.get(idx).copied().unwrap_or(false));
        let on_bg = on;
        let on_border = on;
        let on_mark = on;
        let len = options.len();
        body = body.child(
            nsh_base::button()
                .on_press(move || {
                    if submitted.get_untracked() {
                        return;
                    }
                    selected.update(|v| {
                        if v.len() < len {
                            v.resize(len, false);
                        }
                        if multi {
                            v[idx] = !v[idx];
                        } else {
                            for (i, slot) in v.iter_mut().enumerate() {
                                *slot = i == idx;
                            }
                        }
                    });
                })
                .flex_row()
                .items_center()
                .gap_2()
                .px_3()
                .py_2()
                .rounded(px(8.0))
                .border_1()
                .border_color(move || if on_border() { colors.accent } else { colors.line })
                .bg(move || if on_bg() { colors.soft } else { colors.card })
                .child(
                    text(move || {
                        if multi {
                            if on_mark() { "☑" } else { "☐" }
                        } else if on_mark() {
                            "●"
                        } else {
                            "○"
                        }
                    })
                    .text_size(px(13.0))
                    .text_color(colors.accent),
                )
                .child(
                    view()
                        .flex_1()
                        .flex_col()
                        .gap_0p5()
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_1p5()
                                .child(
                                    text(label)
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_size(px(13.0))
                                        .text_color(colors.ink),
                                )
                                .child(when(
                                    move || recommended,
                                    move || badge("推荐", colors.success),
                                )),
                        )
                        .child(when(
                            move || !note.is_empty(),
                            {
                                let note = opt.note.clone();
                                move || {
                                    text(note.clone())
                                        .text_size(px(11.5))
                                        .text_color(colors.muted)
                                }
                            },
                        )),
                ),
        );
    }

    let opts_for_submit = options.clone();
    let q_for_submit = question.clone();
    body = body
        .child(
            nsh_base::input()
                .value(move || custom.get())
                .on_change(move |v: &str| custom.set(v.to_owned()))
                .placeholder("补充说明或自定义回答（可选）")
                .px_2p5()
                .py_1p5()
                .rounded(px(6.0))
                .bg(colors.soft)
                .border_1()
                .border_color(colors.line_soft)
                .text_size(px(12.5))
                .text_color(colors.ink),
        )
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .child(
                    text(move || {
                        if submitted.get() {
                            "已发送选择".to_owned()
                        } else if multi {
                            "可多选后点击提交".to_owned()
                        } else {
                            "点击选项后提交回答".to_owned()
                        }
                    })
                    .flex_1()
                    .text_size(px(11.5))
                    .text_color(colors.muted),
                )
                .child(
                    primary_button("提交选择", move || {
                        if submitted.get_untracked() {
                            return;
                        }
                        let picks: Vec<String> = selected.with_untracked(|flags| {
                            opts_for_submit
                                .iter()
                                .enumerate()
                                .filter(|(i, _)| flags.get(*i).copied().unwrap_or(false))
                                .map(|(_, o)| o.label.clone())
                                .collect()
                        });
                        let extra = custom.get_untracked();
                        let extra = extra.trim();
                        if picks.is_empty() && extra.is_empty() {
                            return;
                        }
                        let mut reply = String::new();
                        if !q_for_submit.is_empty() {
                            reply.push_str(&format!("关于「{}」：", q_for_submit));
                        }
                        if !picks.is_empty() {
                            reply.push_str(&picks.join("、"));
                        }
                        if !extra.is_empty() {
                            if !picks.is_empty() {
                                reply.push_str("；补充：");
                            }
                            reply.push_str(extra);
                        }
                        submitted.set(true);
                        controller::send_quick_reply(reply);
                    })
                    .disabled(move || {
                        !closed
                            || submitted.get()
                            || (selected.with(|v| !v.iter().any(|b| *b))
                                && custom.with(|s| s.trim().is_empty()))
                    }),
                ),
        );

    card_shell(
        card_header(
            if multi { "多选问答" } else { "决策问答" },
            if question.is_empty() {
                "请选择处理方式"
            } else {
                &question
            },
            source.to_owned(),
        ),
        body,
    )
}

// ---------------------------------------------------------------------------
// 2. Interactive `dispatch` orchestration confirmation card
// ---------------------------------------------------------------------------

#[derive(Clone, PartialEq, Eq)]
struct DispatchTask {
    title: String,
    role: String,
    model: String,
    isolation: String,
    note: String,
    count: usize,
    checked: bool,
}

fn parse_dispatch(source: &str) -> (String, Vec<DispatchTask>) {
    let mut title = String::new();
    let mut tasks = Vec::new();
    for raw in source.lines() {
        let line = raw.trim();
        if line.is_empty() {
            continue;
        }
        if let Some(rest) = line
            .strip_prefix("* ")
            .or_else(|| line.strip_prefix("- "))
        {
            let checked = line.starts_with("* ");
            let mut parts = rest.split('|').map(str::trim);
            let task_title = parts.next().unwrap_or("").to_owned();
            if task_title.is_empty() {
                continue;
            }
            let mut role = String::new();
            let mut model = String::new();
            let mut isolation = "shared".to_owned();
            let mut count = 1usize;
            let mut notes = Vec::new();
            for p in parts {
                if let Some(v) = p.strip_prefix("role=") {
                    role = v.trim().to_owned();
                } else if let Some(v) = p.strip_prefix("model=") {
                    model = v.trim().to_owned();
                } else if let Some(v) = p.strip_prefix("isolation=") {
                    isolation = v.trim().to_owned();
                } else if let Some(v) = p.strip_prefix("count=") {
                    count = v.trim().parse::<usize>().unwrap_or(1).clamp(1, 8);
                } else if !p.is_empty() {
                    notes.push(p);
                }
            }
            tasks.push(DispatchTask {
                title: task_title,
                role,
                model,
                isolation,
                note: notes.join(" · "),
                count,
                checked,
            });
        } else if title.is_empty() && tasks.is_empty() {
            title = line.to_owned();
        }
    }
    (title, tasks)
}

fn dispatch_card(source: &str, closed: bool) -> impl IntoElement {
    let colors = palette();
    let (title, tasks) = parse_dispatch(source);
    let items = signal(tasks.clone());
    let submitted = signal(false);

    let mut body = view().flex_col().gap_2().p_3();
    for (idx, task) in tasks.iter().enumerate() {
        let checked = move || items.with(|v| v.get(idx).is_some_and(|t| t.checked));
        let count_val = move || items.with(|v| v.get(idx).map(|t| t.count).unwrap_or(1));
        let t_title = task.title.clone();
        let t_role = task.role.clone();
        let t_model = task.model.clone();
        let t_iso = task.isolation.clone();
        let t_note = task.note.clone();

        body = body.child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_2p5()
                .py_2()
                .rounded(px(8.0))
                .border_1()
                .border_color(move || if checked() { colors.accent } else { colors.line_soft })
                .bg(colors.soft)
                .child(
                    nsh_base::button()
                        .on_press(move || {
                            if !submitted.get_untracked() {
                                items.update(|v| {
                                    if let Some(t) = v.get_mut(idx) {
                                        t.checked = !t.checked;
                                    }
                                });
                            }
                        })
                        .px_1()
                        .child(
                            text(move || if checked() { "☑" } else { "☐" })
                                .text_size(px(14.0))
                                .text_color(colors.accent),
                        ),
                )
                .child(
                    view()
                        .flex_1()
                        .flex_col()
                        .gap_1()
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_1p5()
                                .flex_wrap()
                                .child(
                                    text(t_title)
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_size(px(13.0))
                                        .text_color(colors.ink),
                                )
                                .child(when(
                                    {
                                        let r = t_role.clone();
                                        move || !r.is_empty()
                                    },
                                    {
                                        let r = t_role.clone();
                                        move || badge(r.clone(), colors.accent)
                                    },
                                ))
                                .child(when(
                                    {
                                        let m = t_model.clone();
                                        move || !m.is_empty()
                                    },
                                    {
                                        let m = t_model.clone();
                                        move || badge(m.clone(), colors.muted)
                                    },
                                ))
                                .child(when(
                                    {
                                        let iso = t_iso.clone();
                                        move || iso == "worktree"
                                    },
                                    move || badge("worktree", colors.warning),
                                )),
                        )
                        .child(when(
                            {
                                let n = t_note.clone();
                                move || !n.is_empty()
                            },
                            {
                                let n = t_note.clone();
                                move || {
                                    text(n.clone())
                                        .text_size(px(11.5))
                                        .text_color(colors.muted)
                                }
                            },
                        )),
                )
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_1()
                        .child(link_button("−", move || {
                            if !submitted.get_untracked() {
                                items.update(|v| {
                                    if let Some(t) = v.get_mut(idx) {
                                        t.count = t.count.saturating_sub(1).max(1);
                                    }
                                });
                            }
                        }))
                        .child(
                            text(move || format!("×{}", count_val()))
                                .font_family(MONO)
                                .text_size(px(11.5))
                                .text_color(colors.ink),
                        )
                        .child(link_button("＋", move || {
                            if !submitted.get_untracked() {
                                items.update(|v| {
                                    if let Some(t) = v.get_mut(idx) {
                                        t.count = (t.count + 1).min(8);
                                    }
                                });
                            }
                        })),
                ),
        );
    }

    body = body.child(
        view()
            .flex_row()
            .items_center()
            .gap_2()
            .pt_1()
            .child(
                text(move || {
                    if submitted.get() {
                        "已确认派工方案".to_owned()
                    } else {
                        let active = items.with(|v| v.iter().filter(|t| t.checked).count());
                        format!("已勾选 {active} 项子任务")
                    }
                })
                .flex_1()
                .text_size(px(11.5))
                .text_color(colors.muted),
            )
            .child(
                ghost_button("直接在本会话执行", move || {
                    if submitted.get_untracked() {
                        return;
                    }
                    submitted.set(true);
                    controller::send_quick_reply(
                        "无需派出子代理，请直接在当前会话按顺序完成以上工作。".to_owned(),
                    );
                })
                .disabled(move || !closed || submitted.get()),
            )
            .child(
                primary_button("确认并派工", move || {
                    if submitted.get_untracked() {
                        return;
                    }
                    let lines: Vec<String> = items.with_untracked(|v| {
                        v.iter()
                            .filter(|t| t.checked)
                            .map(|t| {
                                let mut meta = Vec::new();
                                if !t.role.is_empty() {
                                    meta.push(format!("role={}", t.role));
                                }
                                if !t.model.is_empty() {
                                    meta.push(format!("model={}", t.model));
                                }
                                meta.push(format!("count={}", t.count));
                                meta.push(format!("isolation={}", t.isolation));
                                format!("- {} ({})", t.title, meta.join(", "))
                            })
                            .collect()
                    });
                    if lines.is_empty() {
                        return;
                    }
                    submitted.set(true);
                    controller::send_quick_reply(format!(
                        "确认派工，请立即并行派出以下子任务：\n{}",
                        lines.join("\n")
                    ));
                })
                .disabled(move || {
                    !closed
                        || submitted.get()
                        || items.with(|v| !v.iter().any(|t| t.checked))
                }),
            ),
    );

    card_shell(
        card_header(
            "任务派工",
            if title.is_empty() {
                "确认并行子任务配置"
            } else {
                &title
            },
            source.to_owned(),
        ),
        body,
    )
}

// ---------------------------------------------------------------------------
// 3. Callout, KV, Limits, Timeline, Tree, API, Chart, Vantry-UI, Diagrams
// ---------------------------------------------------------------------------

fn callout_card(args: &str, source: &str) -> impl IntoElement {
    let colors = palette();
    let mut parts = args.splitn(2, char::is_whitespace);
    let tone = parts.next().unwrap_or("info").to_ascii_lowercase();
    let title = parts.next().unwrap_or("").trim();
    let (accent, label) = match tone.as_str() {
        "ok" | "success" => (colors.success, "通过"),
        "warn" | "warning" => (colors.warning, "注意"),
        "err" | "error" | "danger" => (colors.danger, "风险"),
        _ => (colors.accent, "提示"),
    };
    view()
        .flex_col()
        .gap_1p5()
        .p_3()
        .rounded(px(metrics::CARD_RADIUS))
        .bg(colors.soft)
        .border_1()
        .border_color(colors.line_soft)
        .border_l(px(3.5))
        .border_l_color(accent)
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .child(badge(label, accent))
                .child(
                    text(if title.is_empty() {
                        label.to_owned()
                    } else {
                        title.to_owned()
                    })
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_size(px(13.0))
                    .text_color(colors.ink),
                ),
        )
        .child(
            text(source.trim().to_owned())
                .select_text()
                .text_size(px(13.0))
                .line_height(LineHeight::Multiple(1.55))
                .text_color(colors.ink),
        )
}

fn kv_card(source: &str) -> impl IntoElement {
    let colors = palette();
    let mut rows = view().flex_col().gap_1p5().p_3();
    for line in source.lines() {
        let l = line.trim().trim_start_matches('*').trim();
        if l.is_empty() {
            continue;
        }
        if let Some((k, v)) = l.split_once(':') {
            rows = rows.child(
                view()
                    .flex_row()
                    .items_start()
                    .gap_3()
                    .py_1()
                    .border_b_1()
                    .border_b_color(colors.line_soft)
                    .child(
                        text(k.trim().to_owned())
                            .w(px(140.0))
                            .font_weight(FontWeight::MEDIUM)
                            .text_size(px(12.0))
                            .text_color(colors.muted),
                    )
                    .child(
                        text(v.trim().to_owned())
                            .select_text()
                            .flex_1()
                            .text_size(px(12.5))
                            .text_color(colors.ink),
                    ),
            );
        }
    }
    card_shell(card_header("规格参数", "关键字段", source.to_owned()), rows)
}

fn limits_card(source: &str) -> impl IntoElement {
    let colors = palette();
    let mut body = view().flex_col().gap_2p5().p_3();
    for line in source.lines() {
        let l = line.trim();
        if l.is_empty() {
            continue;
        }
        let parts: Vec<&str> = l.split('|').map(str::trim).collect();
        let label = parts.first().copied().unwrap_or(l);
        let ratio_str = parts.get(1).copied().unwrap_or("");
        let unit = parts.get(2).copied().unwrap_or("");
        let (cur, max) = ratio_str
            .split_once('/')
            .and_then(|(a, b)| Some((a.trim().parse::<f32>().ok()?, b.trim().parse::<f32>().ok()?)))
            .unwrap_or((0.0, 100.0));
        let pct = if max > 0.0 {
            (cur / max).clamp(0.0, 1.0)
        } else {
            0.0
        };
        let bar_color = if pct >= 0.9 {
            colors.danger
        } else if pct >= 0.75 {
            colors.warning
        } else {
            colors.success
        };
        let width_px = (pct * 220.0).max(4.0);
        body = body.child(
            view()
                .flex_col()
                .gap_1()
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(
                            text(label.to_owned())
                                .flex_1()
                                .font_weight(FontWeight::MEDIUM)
                                .text_size(px(12.5))
                                .text_color(colors.ink),
                        )
                        .child(
                            text(format!("{ratio_str} {unit}").trim().to_owned())
                                .font_family(MONO)
                                .text_size(px(11.5))
                                .text_color(colors.muted),
                        )
                        .child(badge(format!("{:.0}%", pct * 100.0), bar_color)),
                )
                .child(
                    view()
                        .w(px(220.0))
                        .h(px(6.0))
                        .rounded_full()
                        .bg(colors.soft)
                        .overflow_hidden()
                        .child(view().w(px(width_px)).h_full().rounded_full().bg(bar_color)),
                ),
        );
    }
    card_shell(card_header("配额阈值", "容量与 SLA 指标", source.to_owned()), body)
}

fn timeline_card(source: &str) -> impl IntoElement {
    let colors = palette();
    let mut body = view().flex_col().gap_2().p_3();
    for raw in source.lines() {
        let trimmed = raw.trim();
        if trimmed.is_empty() {
            continue;
        }
        let active = trimmed.starts_with('*');
        let line = trimmed.trim_start_matches('*').trim();
        let parts: Vec<&str> = line.split('|').map(str::trim).collect();
        let when_str = parts.first().copied().unwrap_or("");
        let title = parts.get(1).copied().unwrap_or(when_str);
        let detail = parts.get(2).copied().unwrap_or("");
        let has_detail = !detail.is_empty();
        let detail_owned = detail.to_owned();
        body = body.child(
            view()
                .flex_row()
                .items_start()
                .gap_2p5()
                .child(
                    view()
                        .w(px(10.0))
                        .pt_1()
                        .items_center()
                        .child(super::dot(if active {
                            colors.accent
                        } else {
                            colors.muted
                        })),
                )
                .child(
                    view()
                        .flex_1()
                        .flex_col()
                        .gap_0p5()
                        .child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .child(
                                    text(when_str.to_owned())
                                        .font_family(MONO)
                                        .text_size(px(11.5))
                                        .text_color(if active { colors.accent } else { colors.muted }),
                                )
                                .child(
                                    text(title.to_owned())
                                        .font_weight(FontWeight::SEMIBOLD)
                                        .text_size(px(12.5))
                                        .text_color(colors.ink),
                                ),
                        )
                        .child(when(
                            has_detail,
                            move || {
                                text(detail_owned.clone())
                                    .text_size(px(12.0))
                                    .text_color(colors.muted)
                            },
                        )),
                ),
        );
    }
    card_shell(card_header("时间线", "阶段与里程碑", source.to_owned()), body)
}

fn tree_card(source: &str) -> impl IntoElement {
    let colors = palette();
    let mut body = view().flex_col().gap_0p5().p_3();
    for raw in source.lines() {
        if raw.trim().is_empty() {
            continue;
        }
        let indent = raw.chars().take_while(|c| *c == ' ' || *c == '\t').count();
        let trimmed = raw.trim();
        let (tag_color, prefix, rest) = if let Some(r) = trimmed.strip_prefix("+ ") {
            (colors.success, "+ ", r)
        } else if let Some(r) = trimmed.strip_prefix("- ") {
            (colors.danger, "- ", r)
        } else if let Some(r) = trimmed.strip_prefix("~ ") {
            (colors.warning, "~ ", r)
        } else {
            (colors.ink, "", trimmed)
        };
        let (name, note) = match rest.split_once('|') {
            Some((n, d)) => (n.trim(), d.trim()),
            None => (rest, ""),
        };
        let has_note = !note.is_empty();
        let note_owned = note.to_owned();
        body = body.child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .pl(px((indent as f32) * 8.0))
                .child(
                    text(format!("{prefix}{name}"))
                        .select_text()
                        .font_family(MONO)
                        .text_size(px(12.0))
                        .text_color(tag_color),
                )
                .child(when(has_note, move || {
                    text(note_owned.clone())
                        .text_size(px(11.5))
                        .text_color(colors.muted)
                })),
        );
    }
    card_shell(card_header("目录结构", "文件与模块层级", source.to_owned()), body)
}

fn api_card(source: &str) -> impl IntoElement {
    let colors = palette();
    let mut lines = source.lines().map(str::trim).filter(|l| !l.is_empty());
    let first = lines.next().unwrap_or("API");
    let (method, path) = first.split_once(' ').unwrap_or(("RPC", first));
    let method_color = match method.to_ascii_uppercase().as_str() {
        "GET" => colors.success,
        "POST" => colors.accent,
        "PUT" | "PATCH" => colors.warning,
        "DELETE" => colors.danger,
        _ => colors.muted,
    };
    let mut body = view().flex_col().gap_1p5().p_3().child(
        view()
            .flex_row()
            .items_center()
            .gap_2()
            .child(badge(method.to_uppercase(), method_color))
            .child(
                text(path.to_owned())
                    .select_text()
                    .font_family(MONO)
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_size(px(13.0))
                    .text_color(colors.ink),
            ),
    );
    for l in lines {
        body = body.child(
            text(l.to_owned())
                .select_text()
                .font_family(MONO)
                .text_size(px(12.0))
                .text_color(colors.muted),
        );
    }
    card_shell(card_header("接口契约", first, source.to_owned()), body)
}

fn chart_card(args: &str, source: &str) -> impl IntoElement {
    let colors = palette();
    // Try Studio JSON first, then compact pipe-table form.
    if let Ok(val) = serde_json::from_str::<Value>(source.trim()) {
        let title = val["title"].as_str().unwrap_or("数据图表");
        let unit = val["unit"].as_str().unwrap_or("");
        let mut body = view().flex_col().gap_2().p_3();
        if let Some(hero) = val["hero"].as_str() {
            let hero_label = val["heroLabel"].as_str().unwrap_or("");
            let delta = val["delta"].as_str().unwrap_or("");
            let delta_owned = delta.to_owned();
            let has_delta = !delta_owned.is_empty();
            body = body.child(
                view()
                    .flex_row()
                    .items_baseline()
                    .gap_2()
                    .child(
                        text(hero.to_owned())
                            .font_weight(FontWeight::BOLD)
                            .text_size(px(22.0))
                            .text_color(colors.ink),
                    )
                    .child(
                        text(hero_label.to_owned())
                            .text_size(px(12.0))
                            .text_color(colors.muted),
                    )
                    .child(when(has_delta, move || {
                        badge(delta_owned.clone(), colors.accent)
                    })),
            );
        }
        if let Some(items) = val["items"].as_array() {
            for item in items {
                let lbl = item["label"].as_str().unwrap_or("");
                let v = item["value"].as_str().unwrap_or("");
                let note = item["note"].as_str().unwrap_or("");
                body = body.child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(text(lbl.to_owned()).flex_1().text_size(px(12.5)).text_color(colors.muted))
                        .child(text(v.to_owned()).font_family(MONO).font_weight(FontWeight::SEMIBOLD).text_size(px(13.0)).text_color(colors.ink))
                        .child(text(note.to_owned()).text_size(px(11.5)).text_color(colors.faint)),
                );
            }
        } else if let (Some(labels), Some(series)) =
            (val["labels"].as_array(), val["series"].as_array())
            && let Some(first_series) = series.first()
            && let Some(data) = first_series["data"].as_array()
        {
            let nums: Vec<f32> = data
                .iter()
                .map(|v| v.as_f64().unwrap_or(0.0) as f32)
                .collect();
            let max = nums
                .iter()
                .copied()
                .fold(0.0f32, f32::max)
                .max(1.0);
            for (i, lbl_val) in labels.iter().enumerate() {
                let lbl = lbl_val.as_str().unwrap_or("");
                let num = nums.get(i).copied().unwrap_or(0.0);
                let w = ((num / max).clamp(0.0, 1.0) * 200.0).max(4.0);
                body = body.child(bar_row(lbl, num, unit, w, false));
            }
        }
        return card_shell(card_header("图表", title, source.to_owned()), body);
    }

    // Pipe-table form
    let mut rows: Vec<(String, f32, bool)> = Vec::new();
    let unit = args
        .split_whitespace()
        .find_map(|tok| tok.strip_prefix("unit="))
        .unwrap_or("");
    let title = args
        .split_once("title=")
        .map(|(_, rest)| rest.trim().trim_matches('"'))
        .unwrap_or("数据对比");
    for raw in source.lines() {
        let trimmed = raw.trim();
        if trimmed.is_empty() || !trimmed.contains('|') {
            continue;
        }
        let highlight = trimmed.starts_with('*');
        let line = trimmed.trim_start_matches('*').trim();
        let parts: Vec<&str> = line.split('|').map(str::trim).collect();
        if parts.len() >= 2
            && !parts[0].is_empty()
            && let Ok(num) = parts[1].parse::<f32>()
        {
            rows.push((parts[0].to_owned(), num, highlight));
        }
    }
    let max = rows
        .iter()
        .map(|(_, v, _)| *v)
        .fold(0.0f32, f32::max)
        .max(1.0);
    let mut body = view().flex_col().gap_1p5().p_3();
    for (lbl, num, hi) in rows {
        let w = ((num / max).clamp(0.0, 1.0) * 200.0).max(4.0);
        body = body.child(bar_row(&lbl, num, unit, w, hi));
    }
    card_shell(card_header("图表", title, source.to_owned()), body)
}

fn bar_row(label: &str, value: f32, unit: &str, width: f32, highlight: bool) -> View {
    let colors = palette();
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .child(
            text(label.to_owned())
                .w(px(120.0))
                .font_weight(if highlight {
                    FontWeight::SEMIBOLD
                } else {
                    FontWeight::NORMAL
                })
                .text_size(px(12.0))
                .text_color(if highlight { colors.accent } else { colors.ink }),
        )
        .child(
            view()
                .w(px(200.0))
                .h(px(10.0))
                .rounded(px(4.0))
                .bg(colors.soft)
                .overflow_hidden()
                .child(
                    view()
                        .w(px(width))
                        .h_full()
                        .rounded(px(4.0))
                        .bg(if highlight {
                            colors.accent
                        } else {
                            colors.muted
                        }),
                ),
        )
        .child(
            text(format!("{value:.1}{unit}"))
                .font_family(MONO)
                .text_size(px(11.5))
                .text_color(colors.muted),
        )
}

fn vantry_ui_card(source: &str) -> impl IntoElement {
    let colors = palette();
    let Ok(val) = serde_json::from_str::<Value>(source.trim()) else {
        return card_shell(
            card_header("交互卡片", "Vantry UI", source.to_owned()),
            view().p_3().child(
                text(source.to_owned())
                    .font_family(MONO)
                    .text_size(px(12.0))
                    .text_color(colors.ink),
            ),
        );
    };
    let title = val["title"].as_str().unwrap_or("交互卡片");
    let mut body = view().flex_col().gap_2().p_3();
    if let Some(metrics_arr) = val["metrics"].as_array() {
        let mut row = view().flex_row().flex_wrap().gap_2();
        for m in metrics_arr {
            row = row.child(
                view()
                    .flex_col()
                    .p_2()
                    .rounded(px(8.0))
                    .bg(colors.soft)
                    .min_w(px(100.0))
                    .child(
                        text(m["label"].as_str().unwrap_or("").to_owned())
                            .text_size(px(11.0))
                            .text_color(colors.muted),
                    )
                    .child(
                        text(m["value"].as_str().unwrap_or("").to_owned())
                            .font_weight(FontWeight::SEMIBOLD)
                            .text_size(px(15.0))
                            .text_color(colors.ink),
                    ),
            );
        }
        body = body.child(row);
    }
    if let Some(actions) = val["actions"].as_array() {
        let mut row = view().flex_row().flex_wrap().gap_2();
        for a in actions {
            let label = a["label"].as_str().unwrap_or("执行").to_owned();
            let prompt = a["prompt"]
                .as_str()
                .or_else(|| a["message"].as_str())
                .unwrap_or(&label)
                .to_owned();
            row = row.child(ghost_button(label, move || {
                controller::send_quick_reply(prompt.clone())
            }));
        }
        body = body.child(row);
    }
    card_shell(card_header("交互卡片", title, source.to_owned()), body)
}

fn diagram_card(kind: &str, source: &str) -> impl IntoElement {
    let colors = palette();
    let label = match kind {
        "mermaid" => "Mermaid 图表",
        "flow" => "流程图",
        "state" => "状态图",
        "sequence" => "时序图",
        "er" => "ER 图",
        "class" => "类图",
        _ => "图表",
    };
    card_shell(
        card_header(kind, label, source.to_owned()),
        scroll().horizontal().child(
            text(source.trim().to_owned())
                .select_text()
                .font_family(MONO)
                .text_size(px(12.0))
                .line_height(LineHeight::Multiple(1.5))
                .text_color(colors.ink)
                .px_3()
                .py_2(),
        ),
    )
}

fn math_card(source: &str) -> impl IntoElement {
    let colors = palette();
    card_shell(
        card_header("公式", "数学表达式", source.to_owned()),
        view().p_3().child(
            text(source.trim().to_owned())
                .select_text()
                .font_family(MONO)
                .text_size(px(13.5))
                .text_color(colors.ink),
        ),
    )
}

fn css_color(c: AdaptiveColor, dark: bool) -> String {
    let c = if dark { c.dark } else { c.light };
    format!(
        "hsl({:.1} {:.1}% {:.1}% / {:.3})",
        c.h * 360.0,
        c.s * 100.0,
        c.l * 100.0,
        c.a
    )
}

/// The page an `svg` / `html` / `artifact` block shows: Vantry's theme as CSS
/// variables, no network (CSP), optional zoom.
pub(crate) fn artifact_document(kind: &str, title: &str, source: &str, zoom: f32, dark: bool) -> String {
    artifact_page(palette(), kind, title, source, zoom, dark)
}

fn artifact_page(c: &crate::theme::Palette, kind: &str, title: &str, source: &str, zoom: f32, dark: bool) -> String {
    let css_color = |color| css_color(color, dark);
    let vars = format!(
        ":root{{color-scheme:{scheme};--v-bg:{bg};--v-surface:{surface};--v-surface-2:{surface2};--v-ink:{ink};--v-muted:{muted};--v-faint:{faint};--v-line:{line};--v-accent:{accent};--v-good:{good};--v-warn:{warn};--v-bad:{bad}}}",
        scheme = if dark { "dark" } else { "light" },
        bg = css_color(c.card),
        surface = css_color(c.raised),
        surface2 = css_color(c.soft),
        ink = css_color(c.ink),
        muted = css_color(c.muted),
        faint = css_color(c.faint),
        line = css_color(c.line),
        accent = css_color(c.accent),
        good = css_color(c.success),
        warn = css_color(c.warning),
        bad = css_color(c.danger),
    );
    let csp = "<meta http-equiv=\"Content-Security-Policy\" content=\"default-src 'none'; img-src data: blob:; media-src data: blob:; font-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'\">";
    let base = format!(
        "{vars}html,body{{margin:0;background:transparent;color:var(--v-ink);font:14px/1.55 -apple-system,BlinkMacSystemFont,'PingFang SC',system-ui,sans-serif}}body{{padding:14px 16px;zoom:{zoom}}}*{{box-sizing:border-box}}a{{color:var(--v-accent)}}.tnum{{font-variant-numeric:tabular-nums}}svg{{max-width:100%;height:auto}}table{{border-collapse:collapse}}td,th{{border-bottom:1px solid var(--v-line);padding:4px 8px}}"
    );
    use base64::{Engine, engine::general_purpose::STANDARD};
    let center = if kind == "svg" { "body{display:flex;justify-content:center}" } else { "" };
    let content = format!("<!doctype html><meta charset=utf-8>{csp}<style>{base}{center}</style>{source}");
    let encoded = STANDARD.encode(content);
    let title = title.replace('&', "&amp;").replace('<', "&lt;");
    format!(r#"<!doctype html><meta charset=utf-8><meta http-equiv="Content-Security-Policy" content="default-src 'none'; frame-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; form-action 'none'; base-uri 'none'"><title>{title}</title><style>html,body,iframe{{width:100%;height:100%;border:0;margin:0;display:block}}</style><iframe sandbox="allow-scripts" src="data:text/html;charset=utf-8;base64,{encoded}"></iframe>"#)
}

fn artifact_view(doc: Memo<String>, height: Signal<f32>, max_height: f32) -> AnyElement {
    nsh_core::element::web_view_html(move || doc.get(), None)
        .incognito(true)
        .w_full()
        .h(move || px(height.get().min(max_height)))
        .into_element()
}

fn artifact_card(kind: &str, args: &str, source: &str) -> impl IntoElement {
    let colors = palette();
    let title = args
        .split_once("title=")
        .map(|(_, rest)| rest.trim().trim_matches('"'))
        .filter(|s| !s.is_empty())
        .unwrap_or(if kind == "svg" { "矢量图形" } else { "交互组件" })
        .to_owned();
    let zoom = signal(1.0f32);
    let show_source = signal(false);
    let expanded = signal(false);
    let height = signal(160.0f32);
    let (kind_s, title_s, src_s) = (kind.to_owned(), title.clone(), source.to_owned());
    let doc = computed(move || {
        // Follows light / dark and the zoom.
        artifact_document(&kind_s, &title_s, &src_s, zoom.get(), nsh_core::element::dark())
    });
    let zoom_label = computed(move || format!("{:.0}%", zoom.get() * 100.0));
    let (open_kind, open_title, open_src) = (kind.to_owned(), title.clone(), source.to_owned());
    let copy_src = source.to_owned();
    let header = view()
        .w_full()
        .min_w_0()
        .flex_row()
        .items_center()
        .gap_2()
        .px_3()
        .py_1p5()
        .border_b_1()
        .border_b_color(colors.line_soft)
        .child(badge(kind.to_uppercase(), colors.accent))
        .child(
            text(title.clone())
                .flex_1()
                .min_w_0()
                .number_of_lines(1u32)
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(px(12.5))
                .text_color(colors.ink),
        )
        .child(super::small_icon_button(Icon::Minus, "缩小", move || {
            zoom.set((zoom.get_untracked() - 0.1).max(0.5))
        }))
        .child(
            nsh_base::button()
                .accessibility_label("重置缩放")
                .on_press(move || zoom.set(1.0))
                .px_1()
                .rounded(px(5.0))
                .hover(|s| s.bg(colors.soft))
                .child(
                    text(move || zoom_label.get())
                        .font_family(MONO)
                        .text_size(px(11.0))
                        .text_color(colors.muted),
                ),
        )
        .child(super::small_icon_button(Icon::Plus, "放大", move || {
            zoom.set((zoom.get_untracked() + 0.1).min(2.0))
        }))
        .child(super::small_icon_button(Icon::Code, "查看源码", move || {
            show_source.set(!show_source.get_untracked())
        }))
        .child(super::small_icon_button(Icon::Maximize2, "全屏查看", move || expanded.set(true)))
        .child(super::small_icon_button(Icon::ExternalLink, "在浏览器打开", move || {
            controller::open_offline_artifact(open_kind.clone(), open_title.clone(), open_src.clone())
        }))
        .child(super::small_icon_button(Icon::Copy, "复制源码", move || {
            controller::copy(copy_src.clone())
        }));
    let raw = source.trim().to_owned();
    let modal_title = title.clone();
    let modal_height = signal(480.0f32);
    let body = view()
        .w_full()
        .min_w_0()
        .flex_col()
        .child(when(move || expanded.get(), move || {
            let w = (crate::window_size().width.0 - 120.0).clamp(360.0, 1100.0);
            let max_h = (crate::window_size().height.0 - 140.0).max(240.0);
            nsh_base::dialog()
                .animation(ModalAnimation::Fade)
                .on_cancel(move || expanded.set(false))
                .child(
                    view()
                        .flex_1()
                        .items_center()
                        .justify_center()
                        .bg(colors.scrim)
                        .p_6()
                        .on_press(move || expanded.set(false))
                        .child(
                            view()
                                .w(px(w))
                                .flex_col()
                                .rounded(px(16.0))
                                .bg(colors.card)
                                .border_1()
                                .border_color(colors.line)
                                .shadow_xl()
                                .overflow_hidden()
                                .on_press(|| {})
                                .child(
                                    view()
                                        .flex_row()
                                        .items_center()
                                        .gap_2()
                                        .px_4()
                                        .py_2()
                                        .border_b_1()
                                        .border_b_color(colors.line_soft)
                                        .child(
                                            text(modal_title.clone())
                                                .flex_1()
                                                .font_weight(FontWeight::SEMIBOLD)
                                                .text_size(px(14.0))
                                                .text_color(colors.ink),
                                        )
                                        .child(super::icon_button(Icon::X, "关闭", move || {
                                            expanded.set(false)
                                        })),
                                )
                                .child(artifact_view(doc, modal_height, max_h)),
                        ),
                )
        }))
        .child(dynamic(move || {
            if show_source.get() {
                return view()
                    .w_full()
                    .p_3()
                    .child(
                        text(raw.clone())
                            .select_text()
                            .font_family(MONO)
                            .text_size(px(12.0))
                            .line_height(LineHeight::Multiple(1.5))
                            .text_color(colors.ink),
                    )
                    .into_element();
            }
            artifact_view(doc, height, 640.0)
        }));
    card_shell(header, body)
}

#[cfg(test)]
mod artifact_tests {
    #[test]
    fn artifact_document_adds_csp_and_theme() {
        let doc = super::artifact_page(&crate::theme::PALETTE, "html", "T", "<b>hi</b>", 1.0, true);
        assert!(doc.contains("Content-Security-Policy"));
        assert!(doc.contains(r#"sandbox="allow-scripts""#));
        assert!(!doc.contains("<b>hi</b>"));
    }
}
