//! A plugin's native panel (`MainView::Plugin`): what the Core's plugin
//! host returns for `plugins.panel`: a title with its status badge, then
//! widgets (text, Markdown, badges, key / value cards, lists, code, groups,
//! forms and action buttons). Pressing an action sends `plugins.run` with
//! its input and the form's values; a panel in the answer replaces the
//! shown one, text goes to the output log under it.

use nsh_core::{LineHeight, prelude::*};

use super::{badge, caption, ghost_button, link_button, primary_button};
use crate::{
    bridge::workbench::actions::{PanelAction, PanelField, PanelWidget},
    controller,
    state::{markdown::MarkdownDoc, ui::MainView},
    theme::{metrics, palette},
};

mod quota;

const MONO: &str = "Menlo";

fn tone_color(tone: &str) -> AdaptiveColor {
    let colors = palette();
    match tone {
        "success" | "ok" => colors.success,
        "warning" | "warn" => colors.warning,
        "danger" | "error" => colors.danger,
        "info" | "accent" => colors.accent,
        _ => colors.muted,
    }
}

pub(crate) fn plugin_panel(id: String) -> impl IntoElement {
    let colors = palette();
    let remaining = signal(false);
    let panel = computed(|| controller::ui().with(|u| u.plugin_panel.clone()));
    let view_memo = computed(move || panel.with(|p| p.as_ref().and_then(|p| p.view.clone())));
    let loading = computed(move || panel.with(|p| p.as_ref().is_some_and(|p| p.loading)));
    let error = computed(move || panel.with(|p| p.as_ref().and_then(|p| p.error.clone())));
    let plain = computed(move || panel.with(|p| p.as_ref().and_then(|p| p.text.clone())));
    let log = computed(move || panel.with(|p| p.as_ref().map(|p| p.log.clone()).unwrap_or_default()));
    let title = computed(move || {
        view_memo.with(|v| v.as_ref().map(|v| v.title.clone()).filter(|t| !t.is_empty()))
            .unwrap_or_else(|| id.clone())
    });
    view()
        .flex_1()
        .flex_col()
        .bg(colors.canvas)
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_5()
                .py_2()
                .border_b_1()
                .border_b_color(colors.line_soft)
                .child(
                    text(move || title.get())
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_size(px(15.0))
                        .text_color(colors.ink),
                )
                .child(when(
                    move || view_memo.with(|v| v.as_ref().is_some_and(|v| !v.badge.is_empty())),
                    move || {
                        badge(
                            move || view_memo.with(|v| v.as_ref().map(|v| v.badge.clone()).unwrap_or_default()),
                            move || view_memo.with(|v| tone_color(v.as_ref().map_or("", |v| v.tone.as_str()))),
                        )
                    },
                ))
                .child(when(move || loading.get(), || caption("加载中…")))
                .child(view().flex_1())
                .child(link_button("刷新", controller::reload_plugin_panel).disabled(move || loading.get()))
                .child(link_button("返回对话", || controller::show_view(MainView::Conversation))),
        )
        .child(
            scroll().flex_1().child(
                view()
                    .w_full()
                    .flex_row()
                    .justify_center()
                    .child(
                        view()
                            .flex_1()
                            .max_w(px(metrics::READING_WIDTH))
                            .flex_col()
                            .gap_3()
                            .px_6()
                            .py_4()
                            .child(when(
                                move || error.with(Option::is_some),
                                move || {
                                    view()
                                        .flex_col()
                                        .gap_2()
                                        .p_4()
                                        .rounded(px(metrics::CARD_RADIUS))
                                        .bg(colors.approval)
                                        .border_1()
                                        .border_color(colors.approval_border)
                                        .child(text("插件面板加载失败").font_weight(FontWeight::SEMIBOLD).text_size(px(13.0)).text_color(colors.danger))
                                        .child(text(move || error.get().unwrap_or_default()).select_text().text_size(px(12.0)).text_color(colors.ink))
                                        .child(view().flex_row().gap_2().child(ghost_button("重试", controller::reload_plugin_panel)).child(ghost_button("打开插件设置", || {
                                            controller::select_dock_tab(crate::state::workbench::DockTab::Plugins)
                                        })))
                                },
                            ))
                            .child(when(
                                move || plain.with(Option::is_some),
                                move || {
                                    text(move || plain.get().unwrap_or_default())
                                        .select_text()
                                        .font_family(MONO)
                                        .text_size(px(12.0))
                                        .line_height(LineHeight::Multiple(1.5))
                                        .text_color(colors.ink)
                                },
                            ))
                            .child(dynamic(move || match view_memo.get() {
                                Some(panel) => {
                                    let mut column = view().flex_col().gap_3();
                                    for widget in panel.widgets {
                                        column = column.child(render(widget, remaining));
                                    }
                                    if !panel.actions.is_empty() {
                                        column = column.child(actions_row(panel.actions));
                                    }
                                    column.into_element()
                                }
                                None => view().into_element(),
                            }))
                            .child(when(
                                move || log.with(|l| !l.is_empty()),
                                move || {
                                    view()
                                        .flex_col()
                                        .gap_1()
                                        .p_3()
                                        .rounded(px(10.0))
                                        .bg(colors.code)
                                        .child(caption("输出"))
                                        .child(each(
                                            move || log.get().into_iter().enumerate().rev().collect::<Vec<_>>(),
                                            |(n, _): &(usize, crate::state::ui::PanelLog)| *n,
                                            move |line, _| {
                                                text(move || line.with(|(_, l)| l.text.clone()))
                                                    .select_text()
                                                    .font_family(MONO)
                                                    .text_size(px(11.5))
                                                    .text_color(move || if line.with(|(_, l)| l.ok) { colors.ink } else { colors.danger })
                                            },
                                        ))
                                },
                            )),
                    ),
            ),
        )
}

/// One widget (recursively for groups).
fn render(widget: PanelWidget, remaining: Signal<bool>) -> AnyElement {
    let colors = palette();
    let heading = (!widget.title.is_empty()).then(|| caption(widget.title.clone()));
    let body: AnyElement = match widget.kind.as_str() {
        "quotaCard" => return quota::card(widget.quota.unwrap_or_default(), widget.actions, remaining),
        "modelCatalog" => view().flex_row().items_center().gap_2()
            .child(text(widget.text.clone()).flex_1().text_size(px(14.0)).text_color(colors.ink))
            .child(link_button("查看模型目录", || controller::open_settings(crate::state::ui::SettingsPage::Models)))
            .into_element(),
        "meter" => {
            let share = f32::from(widget.percent.min(100)) / 100.0;
            view().flex_col().gap_2()
                .child(view().flex_row().items_center().gap_3()
                    .child(text(widget.text.clone()).flex_1().font_weight(FontWeight::SEMIBOLD).text_size(px(18.0)).text_color(colors.ink))
                    .child(caption(format!("已用 {}%", widget.percent))))
                .child(view().h(px(5.0)).flex_row().rounded_full().bg(colors.soft)
                    .child(view().flex_grow(share).h(px(5.0)).rounded_full().bg(colors.accent))
                    .child(view().flex_grow(1.0 - share)))
                .children((!widget.detail.is_empty()).then(||caption(widget.detail.clone())))
                .into_element()
        }
        "markdown" => super::markdown::static_document(&MarkdownDoc::parse(&widget.text)).into_element(),
        "badge" => view()
            .flex_row()
            .child(badge(widget.text.clone(), tone_color(&widget.tone)))
            .into_element(),
        "kv" => {
            let mut rows = view().flex_1().min_w(px(240.0)).flex_col().gap_1();
            for entry in widget.entries {
                rows = rows.child(
                    view()
                        .flex_row()
                        .gap_3()
                        .child(text(entry.key).w(px(120.0)).text_size(px(12.0)).text_color(colors.muted))
                        .child(text(entry.value).flex_1().select_text().text_size(px(12.5)).text_color(colors.ink)),
                );
            }
            // Its actions sit inside the card, beside the rows they act on.
            let actions = (!widget.actions.is_empty()).then(|| actions_row(widget.actions));
            return view()
                .flex_col()
                .gap_1()
                .children(heading)
                .child(
                    view()
                        .flex_row()
                        .flex_wrap()
                        .items_center()
                        .gap_3()
                        .px_4()
                        .py_3()
                        .rounded(px(metrics::CARD_RADIUS))
                        .bg(colors.card)
                        .border_1()
                        .border_color(colors.line)
                        .child(rows)
                        .children(actions),
                )
                .into_element();
        }
        "list" => {
            let mut list = view().flex_col().gap_0p5();
            for item in widget.items {
                list = list.child(
                    view()
                        .flex_row()
                        .gap_2()
                        .child(text("•").text_size(px(12.5)).text_color(colors.faint))
                        .child(text(item).flex_1().select_text().text_size(px(12.5)).text_color(colors.ink)),
                );
            }
            list.into_element()
        }
        "code" => scroll()
            .horizontal()
            .rounded(px(8.0))
            .bg(colors.code)
            .child(
                text(widget.text.clone())
                    .select_text()
                    .font_family(MONO)
                    .text_size(px(11.5))
                    .line_height(LineHeight::Multiple(1.45))
                    .text_color(colors.ink)
                    .px_3()
                    .py_2(),
            )
            .into_element(),
        "group" => {
            let mut group = view()
                .flex_col()
                .gap_2()
                .p_3()
                .rounded(px(metrics::CARD_RADIUS))
                .border_1()
                .border_color(colors.line);
            if !widget.text.is_empty() {
                group = group.child(text(widget.text.clone()).text_size(px(12.0)).text_color(colors.muted));
            }
            for child in widget.children {
                group = group.child(render(child, remaining));
            }
            if !widget.actions.is_empty() {
                group = group.child(actions_row(widget.actions));
            }
            return view()
                .flex_col()
                .gap_1()
                .children(heading)
                .child(group)
                .into_element();
        }
        "form" => {
            let mut form = view().flex_col().gap_2();
            for f in widget.fields {
                form = form.child(form_field(f));
            }
            form.into_element()
        }
        "actions" => actions_row(widget.actions.clone()).into_element(),
        // text, and kinds this desktop does not know yet.
        _ => text(widget.text.clone())
            .select_text()
            .text_size(px(12.5))
            .line_height(LineHeight::Multiple(1.5))
            .text_color(if widget.tone.is_empty() { colors.ink } else { tone_color(&widget.tone) })
            .into_element(),
    };
    let extra = (!widget.actions.is_empty() && widget.kind != "actions").then(|| actions_row(widget.actions));
    view()
        .flex_col()
        .gap_1()
        .children(heading)
        .child(body)
        .children(extra)
        .into_element()
}

fn form_field(f: PanelField) -> impl IntoElement {
    let colors = palette();
    let key = f.key.clone();
    let read_key = f.key.clone();
    let value = move || {
        controller::ui().with(|u| {
            u.plugin_panel
                .as_ref()
                .and_then(|p| p.values.get(&read_key).cloned())
                .unwrap_or_default()
        })
    };
    view()
        .flex_col()
        .gap_0p5()
        .child(caption(if f.label.is_empty() { f.key.clone() } else { f.label.clone() }))
        .child(
            nsh_base::input()
                .masked(f.kind == "secret")
                .multiline(f.kind == "multiline")
                .value(value)
                .on_change(move |t: &str| controller::set_panel_value(key.clone(), t))
                .placeholder(f.placeholder.clone())
                .placeholder_color(move || color(colors.faint))
                .auto_correct(false)
                .px_2()
                .py_1()
                .min_h(px(if f.kind == "multiline" { 72.0 } else { 28.0 }))
                .rounded(px(7.0))
                .bg(colors.card)
                .border_1()
                .border_color(colors.line)
                .text_size(px(12.5))
                .text_color(colors.ink),
        )
}

fn actions_row(actions: Vec<PanelAction>) -> View {
    let running = computed(|| controller::ui().with(|u| u.plugin_panel.as_ref().and_then(|p| p.running.clone())));
    view().flex_row().flex_wrap().gap_2().children(actions.into_iter().map(move |action| {
        let id = action.id.clone();
        let label = action.label.clone();
        let running_id = action.id.clone();
        let shown = move || {
            if running.with(|r| r.as_deref() == Some(running_id.as_str())) {
                format!("{label}…")
            } else {
                label.clone()
            }
        };
        let busy = move || running.with(Option::is_some);
        let press = move || controller::run_panel_action(id.clone());
        match action.tone.as_str() {
            "primary" => primary_button(shown, press).disabled(busy).into_element(),
            _ => ghost_button(shown, press).disabled(busy).into_element(),
        }
    }))
}
