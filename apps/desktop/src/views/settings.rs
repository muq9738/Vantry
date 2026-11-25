//! Six preference pages and the catalog forms shared with the workbench dock.

use nsh_core::{LineHeight, element::desktop::{MenuItem,popup_menu}, prelude::*};
use vantry_protocol::{MCPServerInfo, PluginInfo};

use super::{badge, caption, ghost_button, link_button, pill, primary_button, segment, segments, switch_row};
use crate::{
    bridge::workbench::{
        SettingsData,
        actions::{
            self, EnvironmentEntry, ModelUsage, ProjectEntry, RemoteClient, RemotePeer,
            SANDBOX_MODES, compact_count, names,
        },
    },
    controller,
    state::{
        APPROVAL_POLICIES,
        ui::{Catalogs, MainView, SettingsPage, ThemePreference},
    },
    theme::{metrics, palette},
};

const MONO: &str = "Menlo";

type Data = Memo<Option<std::sync::Arc<SettingsData>>>;

pub(crate) fn settings() -> impl IntoElement {
    let page = computed(|| controller::ui().with(|u| u.settings));
    when(move || page.get().is_some(), move || dialog(page))
}

/// Sections each page shows (for the "not served" note).
fn sections_of(page: SettingsPage) -> &'static [&'static str] {
    match page {
        SettingsPage::Models => &[names::MODELS, names::PI_STATUS],
        SettingsPage::Remote => &[
            names::REMOTE_STATUS,
            names::ENVIRONMENTS_LIST,
            names::PROJECTS_LIST,
        ],
        SettingsPage::Permissions => &[names::SANDBOX_GET],
        SettingsPage::About => &[names::UPDATE_STATUS],
        SettingsPage::Usage => &[names::USAGE_SUMMARY],
        SettingsPage::Appearance | SettingsPage::General => &[],
    }
}

fn dialog(page: Memo<Option<SettingsPage>>) -> impl IntoElement {
    let colors = palette();
    let catalogs = computed(|| controller::ui().with(|u| u.catalogs.clone()));
    let data: Data = computed(move || match catalogs.get() {
        Catalogs::Loaded(data) => Some(data),
        _ => None,
    });
    nsh_base::dialog()
        .animation(ModalAnimation::Fade)
        .on_cancel(controller::close_settings)
        .child(
            view()
                .flex_1()
                .items_center()
                .justify_center()
                .bg(colors.scrim)
                .px(px(16.0))
                .py(px(20.0))
                .child(
                    view()
                        .w_full()
                        .max_w(px(940.0))
                        .h(move || {
                            px((nsh_core::element::window().height.0 - 40.0).clamp(1.0, 700.0))
                        })
                        .flex_row()
                        .style_when(compact, |s| s.flex_col())
                        .rounded(px(14.0))
                        .bg(colors.canvas)
                        .border_1()
                        .border_color(colors.line)
                        .shadow_xl()
                        .overflow_hidden()
                        .child(when(
                            move || page.get() != Some(SettingsPage::Usage),
                            move || nav(page, catalogs),
                        ))
                        .child(
                            view()
                                .flex_1()
                                .flex_col()
                                .min_w_0()
                                .min_h_0()
                                .relative()
                                .child(
                                    row()
                                        .h(px(56.0))
                                        .pl(px(28.0))
                                        .pr_4()
                                        .border_b_1()
                                        .border_b_color(colors.line_soft)
                                        .child(
                                            text(move || {
                                                crate::tr(
                                                    page.get()
                                                        .unwrap_or(SettingsPage::General)
                                                        .label(),
                                                )
                                            })
                                            .flex_1()
                                            .text_size(px(17.0))
                                            .font_weight(FontWeight::SEMIBOLD)
                                            .text_color(colors.ink),
                                        )
                                        .child(super::icon_button(
                                            crate::icons::Icon::X,
                                            "关闭设置",
                                            controller::close_settings,
                                        )),
                                )
                                .child(dynamic(move || {
                                    page.get().map(|current| {
                                        let body = match current {
                                            SettingsPage::General => general_page().into_element(),
                                            SettingsPage::Models => {
                                                models_page(data, catalogs).into_element()
                                            }
                                            SettingsPage::Remote => {
                                                remote_page(data).into_element()
                                            }
                                            SettingsPage::Permissions => {
                                                security_page(data).into_element()
                                            }
                                            SettingsPage::About => update_page(data).into_element(),
                                            SettingsPage::Usage => usage_page(data).into_element(),
                                            SettingsPage::Appearance => {
                                                appearance_page().into_element()
                                            }
                                        };
                                        scroll()
                                            .flex_1()
                                            .child(
                                                view()
                                                    .flex_col()
                                                    .gap_3()
                                                    .px(px(28.0))
                                                    .pb_8()
                                                    .pt_1()
                                                    .child(degraded_note(page, data))
                                                    .child(body),
                                            )
                                            .into_element()
                                    })
                                }))
                                // Floats over the page: showing it never moves the page.
                                .child(feedback_toast()),
                        ),
                ),
        )
}

fn compact() -> bool {
    nsh_core::element::window().width.0 - 32.0 < 680.0
}

fn nav_items(page: Memo<Option<SettingsPage>>) -> impl Iterator<Item = impl IntoElement> {
    SettingsPage::ALL.into_iter().map(move |p| {
        let colors = palette();
        let glyph = match p {
            SettingsPage::General => crate::icons::Icon::Settings,
            SettingsPage::Appearance => crate::icons::Icon::Sun,
            SettingsPage::Permissions => crate::icons::Icon::Shield,
            SettingsPage::Models => crate::icons::Icon::Bot,
            SettingsPage::Remote => crate::icons::Icon::Server,
            _ => crate::icons::Icon::CircleHelp,
        };
        nsh_base::button()
            .flex_row()
            .items_center()
            .gap_2()
            .selected(move || page.get() == Some(p))
            .on_press(move || controller::open_settings(p))
            .h(px(34.0))
            .px_3()
            .min_w(px(110.0))
            .rounded(px(8.0))
            .bg(move || {
                if page.get() == Some(p) {
                    colors.card
                } else {
                    AdaptiveColor::TRANSPARENT
                }
            })
            .hover(|s| s.bg(colors.soft))
            .child(crate::icons::icon(glyph, 14.0, move || {
                if page.get() == Some(p) {
                    colors.ink
                } else {
                    colors.muted
                }
            }))
            .child(
                text(crate::tr(p.label()))
                    .text_size(px(13.0))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(move || {
                        if page.get() == Some(p) {
                            colors.ink
                        } else {
                            colors.muted
                        }
                    }),
            )
    })
}

fn nav(page: Memo<Option<SettingsPage>>, catalogs: Memo<Catalogs>) -> impl IntoElement {
    let colors = palette();
    dynamic(move || {
        if compact() {
            scroll()
                .horizontal()
                .h(px(54.0))
                .flex_shrink_0()
                .bg(colors.sidebar)
                .child(row().p_2().children(nav_items(page)))
                .into_element()
        } else {
            view()
                .w(px(216.0))
                .flex_shrink_0()
                .flex_col()
                .gap_0p5()
                .p_3()
                .bg(colors.sidebar)
                .border_r_1()
                .border_r_color(colors.line)
                .child(
                    text(crate::tr("设置"))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_size(px(15.0))
                        .text_color(colors.ink)
                        .px_2()
                        .pb_2(),
                )
                .children(nav_items(page))
                .child(view().flex_1())
                .child(
                    text(move || match catalogs.get() {
                        Catalogs::Loading => "正在读取…".to_owned(),
                        Catalogs::Failed(e) => format!("读取失败：{e}"),
                        _ => String::new(),
                    })
                    .px_2()
                    .text_size(px(11.0))
                    .text_color(colors.faint),
                )
                .child(caption(concat!("Vantry v", env!("CARGO_PKG_VERSION"))))
                .into_element()
        }
    })
}

/// The last operation's outcome as a small pill; a success leaves by itself.
fn feedback_pill(feedback: Memo<Option<crate::state::ui::Feedback>>) -> View {
    let colors = palette();
    let ok = move || feedback.with(|f| f.as_ref().is_some_and(|f| f.ok));
    view()
        .flex_row()
        .items_center()
        .gap_2()
        .pl_3()
        .pr_1()
        .py_1()
        .max_w(px(520.0))
        .rounded(px(10.0))
        .bg(move || if ok() { colors.raised } else { colors.approval })
        .border_1()
        .border_color(move || if ok() { colors.line } else { colors.approval_border })
        .shadow_md()
        .child(super::dot(move || if ok() { colors.success } else { colors.danger }))
        .child(
            text(move || feedback.with(|f| f.as_ref().map(|f| f.text.clone()).unwrap_or_default()))
                .flex_shrink(1.0)
                .min_w(px(0.0))
                .number_of_lines(2u32)
                .select_text()
                .text_size(px(12.0))
                .text_color(colors.ink),
        )
        .child(super::small_icon_button(
            crate::icons::Icon::X,
            "关闭提示",
            controller::dismiss_feedback,
        ))
}

fn feedback_memo() -> Memo<Option<crate::state::ui::Feedback>> {
    let feedback = computed(|| controller::ui().with(|u| u.forms.feedback.clone()));
    // A success dismisses itself after a moment; an error stays until closed.
    effect(move || {
        let Some(shown) = feedback.get() else { return };
        if !shown.ok {
            return;
        }
        spawn(async move {
            timer(std::time::Duration::from_millis(2600)).await;
            if feedback.get_untracked().as_ref() == Some(&shown) {
                controller::dismiss_feedback();
            }
        });
    });
    feedback
}

/// The outcome floating at the bottom of the settings page.
fn feedback_toast() -> impl IntoElement {
    let feedback = feedback_memo();
    when(
        move || feedback.with(Option::is_some),
        move || {
            view()
                .absolute()
                .left_0()
                .right_0()
                .bottom(px(18.0))
                .flex_row()
                .justify_center()
                .px_6()
                .child(feedback_pill(feedback))
        },
    )
}

/// The outcome inline (the dock's plugin and MCP lists).
fn feedback_line() -> impl IntoElement {
    let feedback = feedback_memo();
    when(
        move || feedback.with(Option::is_some),
        move || view().flex_row().child(feedback_pill(feedback)),
    )
}

/// Which of the page's sections the Core did not serve.
fn degraded_note(page: Memo<Option<SettingsPage>>, data: Data) -> impl IntoElement {
    let colors = palette();
    let note = computed(move || {
        let Some(page) = page.get() else {
            return String::new();
        };
        data.with(|d| {
            let Some(d) = d else {
                return String::new();
            };
            let missing: Vec<String> = d
                .degraded
                .iter()
                .filter(|(section, _)| sections_of(page).contains(&section.as_str()))
                .map(|(section, why)| format!("{section}（{why}）"))
                .collect();
            if missing.is_empty() {
                String::new()
            } else {
                format!("部分设置暂不可用：{}", missing.join("、"))
            }
        })
    });
    when(
        move || note.with(|n| !n.is_empty()),
        move || {
            text(move || note.get())
                .px_3()
                .py_1p5()
                .rounded(px(8.0))
                .bg(colors.soft)
                .text_size(px(11.5))
                .line_height(LineHeight::Multiple(1.45))
                .text_color(colors.muted)
        },
    )
}

fn title(label: &str) -> Text {
    let colors = palette();
    text(crate::tr(label).to_owned())
        .font_weight(FontWeight::MEDIUM)
        .text_size(px(12.5))
        .text_color(colors.muted)
        .pt_1()
}

fn hint(label: impl IntoReactive<SharedString>) -> Text {
    let colors = palette();
    text(label)
        .text_size(px(12.0))
        .line_height(LineHeight::Multiple(1.45))
        .text_color(colors.faint)
}

fn card() -> View {
    let colors = palette();
    view()
        .flex_col()
        .gap_1p5()
        .px_4()
        .py_3()
        .rounded(px(10.0))
        .bg(colors.raised)
        .border_1()
        .border_color(colors.line_soft)
}

/// Whether settings operation `key` runs.
fn busy(key: impl Into<String>) -> impl Fn() -> bool + Clone + 'static {
    let key = key.into();
    move || controller::ui().with(|u| u.op_busy(&key))
}

/// A one-line text field.
fn field(
    value: impl Fn() -> String + 'static,
    on_change: impl Fn(&str) + 'static,
    placeholder: &'static str,
) -> impl IntoElement {
    let colors = palette();
    nsh_base::input()
        .value(value)
        .on_change(on_change)
        .placeholder(placeholder)
        .placeholder_color(move || color(colors.faint))
        .auto_correct(false)
        .auto_capitalize(AutoCapitalize::None)
        .flex_1()
        .h(px(32.0))
        .px_2p5()
        .rounded(px(8.0))
        .bg(colors.canvas)
        .border_1()
        .border_color(colors.line)
        .text_size(px(12.5))
        .text_color(colors.ink)
}

/// A field whose text is hidden (keys, pairing codes).
fn secret_field(
    value: impl Fn() -> String + 'static,
    on_change: impl Fn(&str) + 'static,
    placeholder: &'static str,
) -> impl IntoElement {
    let colors = palette();
    nsh_base::input()
        .masked(true)
        .value(value)
        .on_change(on_change)
        .placeholder(placeholder)
        .placeholder_color(move || color(colors.faint))
        .auto_correct(false)
        .auto_capitalize(AutoCapitalize::None)
        .flex_1()
        .h(px(32.0))
        .px_2p5()
        .rounded(px(8.0))
        .bg(colors.canvas)
        .border_1()
        .border_color(colors.line)
        .text_size(px(12.5))
        .text_color(colors.ink)
}

fn row() -> View {
    view().flex_row().items_center().gap_2()
}

fn setting_label(label: &'static str) -> Text {
    text(crate::tr(label)).text_size(px(13.5)).text_color(palette().ink)
}

fn general_page() -> impl IntoElement {
    let enter = computed(|| controller::ui().with(|u| u.send_with_enter));
    let colors = palette();
    view()
        .flex_col()
        .gap_3()
        .child(
            card()
                .gap_0()
                .py_1()
                .child(
                    row()
                        .h(px(46.0))
                        .child(
                            setting_label("界面语言")
                                .flex_1()
                                .font_weight(FontWeight::MEDIUM),
                        )
                        .child(
                            nsh_base::button()
                                .on_press(|| {
                                    let items = crate::state::language::Language::ALL
                                        .into_iter()
                                        .map(|language| {
                                            MenuItem::new(language.name())
                                                .checked(
                                                    controller::ui()
                                                        .with_untracked(|u| u.locale == language),
                                                )
                                                .on_select(move || {
                                                    controller::ui()
                                                        .update(|u| u.locale = language)
                                                })
                                        })
                                        .collect();
                                    drop(popup_menu(items));
                                })
                                .flex_row()
                                .items_center()
                                .gap_2()
                                .h(px(30.0))
                                .px_3()
                                .rounded(px(8.0))
                                .bg(colors.canvas)
                                .border_1()
                                .border_color(colors.line)
                                .hover(|s| s.bg(colors.soft))
                                .child(
                                    text(|| controller::ui().with(|u| u.locale.name()))
                                        .text_size(px(12.5))
                                        .text_color(colors.ink),
                                )
                                .child(crate::icons::icon(
                                    crate::icons::Icon::ChevronDown,
                                    12.0,
                                    colors.faint,
                                )),
                        ),
                )
                .child(
                    row()
                        .h(px(46.0))
                        .border_t_1()
                        .border_t_color(colors.line_soft)
                        .child(
                            setting_label("发送快捷键")
                                .flex_1()
                                .font_weight(FontWeight::MEDIUM),
                        )
                        .child(segments().children([false, true].map(move |value| {
                            segment(
                                crate::tr(if value {
                                    "Enter"
                                } else {
                                    "⌘/Ctrl+Enter"
                                }),
                                move || enter.get() == value,
                                move || controller::ui().update(|u| u.send_with_enter = value),
                            )
                        }))),
                )
                .child(
                    view()
                        .border_t_1()
                        .border_t_color(colors.line_soft)
                        .py_1()
                        .child(switch_row(
                            crate::tr("子任务自动续行"),
                            "",
                            || controller::ui().with(|u| !u.manual_delivery),
                            || controller::ui().update(|u| u.manual_delivery = !u.manual_delivery),
                        )),
                ),
        )
}

pub(crate) fn dock_catalog(mcp: bool) -> impl IntoElement {
    let data = computed(|| {
        controller::ui().with(|u| match &u.catalogs {
            Catalogs::Loaded(d) => Some(d.clone()),
            _ => None,
        })
    });
    scroll().flex_1().child(
        view()
            .flex_col()
            .gap_2()
            .p_3()
            .child(feedback_line())
            .child(if mcp {
                mcp_page(data).into_element()
            } else {
                plugins_page(data).into_element()
            })
            .child(link_button("刷新", controller::reload_catalogs)),
    )
}

fn models_page(data: Data, catalogs: Memo<Catalogs>) -> impl IntoElement {
    let colors = palette();
    let current = computed(|| controller::state().with(|s| s.new_thread.model.clone()));
    let pi_provider = computed(|| controller::ui().with(|u| u.forms.pi_provider.clone()));
    let pi_model = computed(|| controller::ui().with(|u| u.forms.pi_model.clone()));
    let pi_key_set = computed(|| controller::ui().with(|u| !u.forms.pi_key.is_empty()));
    let saving = computed(busy("pi.setup"));
    let setup = signal(false);
    let catalog_open = signal(false);
    let model_count = computed(move || {
        data.with(|d| {
            d.as_ref()
                .and_then(|d| d.models.models.as_ref())
                .map_or(0, Vec::len)
        })
    });
    view()
        .flex_col()
        .gap_3()
        .child(dynamic(move || {
            let pi = data.with(|d| d.as_ref().and_then(|d| d.pi.clone()));
            match pi {
                Some(pi) => {
                    let ext_on = pi.enable_user_extensions.unwrap_or(true);
                    let mcp_on = pi.enable_user_mcp.unwrap_or(true);
                    card()
                        .gap_2p5()
                        .child(
                            row()
                                .child(super::dot(if pi.compat_blocked {
                                    colors.danger
                                } else {
                                    colors.success
                                }))
                                .child(
                                    text(format!(
                                        "Pi · {}{}",
                                        if pi.active_source.is_empty() {
                                            "内置".to_owned()
                                        } else {
                                            pi.active_source.clone()
                                        },
                                        if pi.active_version.is_empty() {
                                            String::new()
                                        } else {
                                            format!(" {}", pi.active_version)
                                        }
                                    ))
                                    .flex_1()
                                    .font_weight(FontWeight::MEDIUM)
                                    .text_size(px(13.0))
                                    .text_color(colors.ink),
                                )
                                .child(when(pi.update_available, {
                                    let latest = pi.latest_version.clone();
                                    move || badge(format!("可更新 {latest}"), colors.accent)
                                }))
                                .child(ghost_button("配置密钥", move || {
                                    setup.set(!setup.get_untracked())
                                }))
                                .child(super::small_icon_button(
                                    crate::icons::Icon::RefreshCw,
                                    "刷新",
                                    controller::reload_catalogs,
                                )),
                        )
                        .child(when(!pi.compat_reason.is_empty(), {
                            let reason = pi.compat_reason.clone();
                            move || hint(reason.clone())
                        }))
                        .child(
                            row()
                                .child(pill(
                                    if ext_on { "扩展：开" } else { "扩展：关" },
                                    move || ext_on,
                                    move || controller::toggle_pi_setting(Some(!ext_on), None),
                                ))
                                .child(pill(
                                    if mcp_on { "MCP：开" } else { "MCP：关" },
                                    move || mcp_on,
                                    move || controller::toggle_pi_setting(None, Some(!mcp_on)),
                                ))
                                .child(view().flex_1())
                                .child(link_button("管理 MCP", || {
                                    controller::close_settings();
                                    controller::select_dock_tab(crate::state::workbench::DockTab::Mcp);
                                })),
                        )
                        .child(
                            view()
                                .flex_col()
                                .gap_0p5()
                                .pt_2()
                                .border_t_1()
                                .border_t_color(colors.line_soft)
                                .child(
                                    text(pi.active_path.clone())
                                        .number_of_lines(1u32)
                                        .font_family(MONO)
                                        .text_size(px(11.0))
                                        .text_color(colors.faint),
                                )
                                .child(
                                    text(pi.agent_dir.clone())
                                        .number_of_lines(1u32)
                                        .font_family(MONO)
                                        .text_size(px(11.0))
                                        .text_color(colors.faint),
                                ),
                        )
                        .into_element()
                }
                None => card().child(hint("Pi 不可用")).into_element(),
            }
        }))
        .child(when(move || setup.get(), move || {
            card()
                .gap_2()
                .child(row().child(field(
                    move || pi_provider.get(),
                    |t: &str| controller::set_pi_field(0, t),
                    "Provider（如 anthropic）",
                )))
                .child(row().child(secret_field(
                    move || {
                        if pi_key_set.get() {
                            controller::ui().with_untracked(|u| u.forms.pi_key.expose().to_owned())
                        } else {
                            String::new()
                        }
                    },
                    |t: &str| controller::set_pi_field(1, t),
                    "API Key",
                )))
                .child(
                    row()
                        .child(field(
                            move || pi_model.get(),
                            |t: &str| controller::set_pi_field(2, t),
                            "默认模型（可选）",
                        ))
                        .child(
                            primary_button(
                                move || if saving.get() { "保存中…" } else { "保存" },
                                controller::pi_setup_save,
                            )
                            .disabled(busy("pi.setup")),
                        ),
                )
        }))
        .child(
            nsh_base::button()
                .on_press(move || catalog_open.set(!catalog_open.get_untracked()))
                .w_full()
                .h(px(40.0))
                .px_4()
                .rounded(px(10.0))
                .bg(colors.raised)
                .border_1()
                .border_color(colors.line_soft)
                .hover(|s| s.bg(colors.soft))
                .flex_row()
                .items_center()
                .gap_2()
                .child(
                    text("模型目录")
                        .flex_1()
                        .font_weight(FontWeight::MEDIUM)
                        .text_size(px(13.0))
                        .text_color(colors.ink),
                )
                .child(
                    text(move || format!("{}", model_count.get()))
                        .text_size(px(12.0))
                        .text_color(colors.faint),
                )
                .child(dynamic(move || {
                    crate::icons::icon(
                        if catalog_open.get() {
                            crate::icons::Icon::ChevronDown
                        } else {
                            crate::icons::Icon::ChevronRight
                        },
                        13.0,
                        colors.faint,
                    )
                })),
        )
        .child(dynamic(move || {
            if !catalog_open.get(){return view().into_element();}
            let Some(data) = data.get() else {
                return text(move || match catalogs.get() {
                    Catalogs::Failed(e) => format!("读取失败：{e}"),
                    _ => "正在读取模型目录…".to_owned(),
                })
                .text_size(px(12.0))
                .text_color(colors.faint)
                .into_element();
            };
            let ids: Vec<String> = data
                .models
                .models
                .iter()
                .flatten()
                .map(vantry_protocol::ModelInfo::key)
                .collect();
            let mut column = view().flex_col().gap_2();
            for warning in &data.models.warnings {
                column = column.child(text(warning.clone()).text_size(px(12.0)).text_color(colors.warning));
            }
            if !data.models.thinking_levels.is_empty() {
                let levels = data.models.thinking_levels.clone();
                let ids = ids.clone();
                column = column.child(
                    row()
                        .flex_wrap()
                        .child(caption("新会话思考档位"))
                        .children(std::iter::once(String::new()).chain(levels).map(move |level| {
                            let ids_on = ids.clone();
                            let ids_set = ids.clone();
                            let compare = level.clone();
                            pill(
                                actions::thinking_label(&level),
                                move || actions::split_thinking(&current.get(), &ids_on).1 == compare,
                                move || {
                                    let value = current.get_untracked();
                                    let base = actions::split_thinking(&value, &ids_set).0.to_owned();
                                    if !base.is_empty() {
                                        controller::set_default_model(actions::with_thinking(&base, &level));
                                    }
                                },
                            )
                        })),
                );
            }
            let models = data.models.models.clone().unwrap_or_default();
            if models.is_empty() {
                column = column.child(hint("目录为空：可在输入区的模型选择器中直接输入 provider/model。"));
            }
            for model in models {
                let id = model.key();
                let ids = ids.clone();
                let on = {
                    let id = id.clone();
                    move || {
                        let cur = current.get();
                        let base = actions::split_thinking(&cur, &ids).0;
                        base == id
                            || id
                                .split_once('/')
                                .is_some_and(|(_, local)| local == base)
                    }
                };
                column = column.child(
                    card().child(
                        row()
                            .child(
                                view()
                                    .flex_1()
                                    .flex_col()
                                    .child(text(if model.name.is_empty() { id.clone() } else { model.name.clone() }).text_size(px(13.0)).text_color(colors.ink))
                                    .child(
                                        text(format!(
                                            "{} · 上下文 {}k{}",
                                            id,
                                            model.context_window / 1000,
                                            if model.reasoning { " · 推理" } else { "" }
                                        ))
                                        .font_family(MONO)
                                        .text_size(px(11.0))
                                        .text_color(colors.faint),
                                    ),
                            )
                            .child(when(data.models.default == id, || badge("Core 默认", colors.muted)))
                            .child(pill(
                                {
                                    let on = on.clone();
                                    move || if on() { "新会话默认" } else { "设为默认" }
                                },
                                on,
                                move || controller::set_default_model(id.clone()),
                            )),
                    ),
                );
            }
            column.into_element()
        }))
}

fn mcp_page(data: Data) -> impl IntoElement {
    let colors = palette();
    let enabled = computed(|| controller::ui().with(|u| u.extensions_enabled));
    let draft_open = computed(|| controller::ui().with(|u| u.forms.mcp_draft.is_some()));
    view()
        .flex_col()
        .gap_2()
        .child(title("MCP 服务器"))
        .child(card().child(switch_row(
            "用户扩展",
            "加载 ~/.pi/agent 下的 TypeScript 扩展与技能",
            move || enabled.get(),
            controller::toggle_extensions,
        )))
        .child(
            row()
                .child(view().flex_1())
                .child(link_button("全部探活", || controller::mcp_probe(String::new())).disabled(busy("mcp.probe:")))
                .child(ghost_button(move || if draft_open.get() { "收起表单" } else { "新增服务器" }, controller::mcp_draft_toggle)),
        )
        .child(when(move || draft_open.get(), mcp_form))
        .child(dynamic(move || {
            let Some(data) = data.get() else {
                return hint("正在读取 MCP 目录…").into_element();
            };
            let mcp = &data.mcp;
            let mut column = view().flex_col().gap_2();
            if !mcp.notice.is_empty() {
                column = column.child(hint(mcp.notice.clone()));
            }
            if !mcp.user_file.is_empty() {
                column = column.child(caption(format!("用户配置：{}", mcp.user_file)));
            }
            if !mcp.project_file.is_empty() {
                column = column.child(
                    row()
                        .child(caption(format!("项目配置：{}", mcp.project_file)).flex_1())
                        .child(if mcp.project_trusted {
                            badge("已信任", colors.success).into_element()
                        } else {
                            link_button("信任项目配置", controller::mcp_trust).disabled(busy("mcp.trust")).into_element()
                        }),
                );
            }
            if !mcp.probe_error.is_empty() {
                column = column.child(text(mcp.probe_error.clone()).text_size(px(11.5)).text_color(colors.danger));
            }
            let servers = mcp.servers.as_deref().unwrap_or_default();
            if servers.is_empty() {
                column = column.child(hint("还没有 MCP 服务器。"));
            }
            for server in servers {
                column = column.child(mcp_row(server.clone()));
            }
            column.into_element()
        }))
}

fn mcp_form() -> impl IntoElement {
    let draft = computed(|| controller::ui().with(|u| u.forms.mcp_draft.clone().unwrap_or_default()));
    let stdio = move || draft.with(|d| d.transport != "http");
    card()
        .gap_2()
        .child(caption("新增 MCP 服务器"))
        .child(row().child(field(
            move || draft.with(|d| d.name.clone()),
            |t: &str| {
                let t = t.to_owned();
                controller::mcp_draft_update(move |d| d.name = t)
            },
            "名称（字母、数字、- _ .）",
        )))
        .child(
            row()
                .child(pill("stdio", stdio, || controller::mcp_draft_update(|d| d.transport = "stdio".into())))
                .child(pill("http", move || !stdio(), || controller::mcp_draft_update(|d| d.transport = "http".into())))
                .child(view().w(px(12.0)))
                .child(pill("用户级", move || draft.with(|d| d.scope != "project"), || controller::mcp_draft_update(|d| d.scope = "user".into())))
                .child(pill("项目级", move || draft.with(|d| d.scope == "project"), || controller::mcp_draft_update(|d| d.scope = "project".into()))),
        )
        .child(dynamic(move || {
            if stdio() {
                row()
                    .child(field(
                        move || draft.with(|d| d.command.clone()),
                        |t: &str| {
                            let t = t.to_owned();
                            controller::mcp_draft_update(move |d| d.command = t)
                        },
                        "启动命令，如 npx -y @scope/server",
                    ))
                    .into_element()
            } else {
                row()
                    .child(field(
                        move || draft.with(|d| d.url.clone()),
                        |t: &str| {
                            let t = t.to_owned();
                            controller::mcp_draft_update(move |d| d.url = t)
                        },
                        "https://… /mcp",
                    ))
                    .into_element()
            }
        }))
        .child(
            row()
                .child(hint(move || draft.with(|d| d.problem().unwrap_or("").to_owned())).flex_1())
                .child(ghost_button("取消", controller::mcp_draft_toggle))
                .child(primary_button("保存", controller::mcp_draft_save).disabled(move || draft.with(|d| d.problem().is_some()))),
        )
}

fn mcp_row(server: MCPServerInfo) -> impl IntoElement {
    let colors = palette();
    let name = server.name.clone();
    let catalog_on = server.enabled;
    let on = {
        let name = name.clone();
        move || controller::ui().with(|u| u.mcp_enabled(&name, catalog_on))
    };
    let detail = format!(
        "{} · {} · {}{}{}",
        server.scope,
        server.transport,
        server.exposure,
        if server.endpoint.is_empty() { String::new() } else { format!(" · {}", server.endpoint) },
        if server.tools.is_empty() { String::new() } else { format!(" · {} 个工具", server.tools.len()) }
    );
    let (state, bad) = if !server.live_error.is_empty() {
        (server.live_error.clone(), true)
    } else if !server.error.is_empty() {
        (server.error.clone(), true)
    } else {
        (server.live_state.clone(), false)
    };
    let toggle_scope=server.scope.clone();
    let toggle_name = name.clone();
    let probe_name = name.clone();
    let remove_name = name.clone();
    card()
        .child(switch_row(name.clone(), detail, on.clone(), move || {
            controller::mcp_set_enabled(toggle_name.clone(),toggle_scope.clone(), !on())
        }))
        .child(
            row()
                .child(when(!state.is_empty(), move || {
                    badge(state.clone(), if bad { colors.danger } else { colors.success })
                }))
                .child(view().flex_1())
                .child(link_button("测试探活", move || controller::mcp_probe(probe_name.clone())).disabled(busy(format!("mcp.probe:{name}"))))
                .child(link_button("删除", move || controller::mcp_remove(remove_name.clone(),server.scope.clone())).disabled(busy(format!("mcp:{}", server.name)))),
        )
}

fn plugins_page(data: Data) -> impl IntoElement {
    let source = signal(String::new());
    view()
        .flex_col()
        .gap_2p5()
        .child(
            row()
                .child(field(
                    move || source.get(),
                    move |s| source.set(s.into()),
                    "本地路径或 GitHub 仓库",
                ))
                .child(
                    ghost_button("安装", move || {
                        let mut request = actions::request("plugins.install");
                        request.name = source.get_untracked().trim().to_owned();
                        controller::settings_op("plugin.install", request, "已安装插件");
                    })
                    .h(px(32.0))
                    .disabled(move || source.get().trim().is_empty() || busy("plugin.install")()),
                ),
        )
        .child(dynamic(move || {
            let Some(data) = data.get() else {
                return hint("读取中…").into_element();
            };
            if data.plugins.is_empty() {
                return hint("无插件").into_element();
            }
            let mut column = view().flex_col().gap_2();
            for plugin in &data.plugins {
                column = column.child(plugin_row(plugin.clone()));
            }
            column.into_element()
        }))
}

fn plugin_row(plugin: PluginInfo) -> impl IntoElement {
    let colors = palette();
    let id = plugin.id.clone();
    let enabled = !plugin.disabled;
    let mut opens: Vec<(String, String)> = plugin
        .navigation
        .iter()
        .map(|n| {
            (
                n.page.clone(),
                if n.title.is_empty() {
                    "打开".to_owned()
                } else {
                    n.title.clone()
                },
            )
        })
        .collect();
    if opens.is_empty() {
        opens.push(("main".to_owned(), "打开".to_owned()));
    }
    let toggle_id = id.clone();
    let subtitle = if !plugin.description.is_empty() {
        plugin.description.clone()
    } else if plugin.name != plugin.id {
        plugin.id.clone()
    } else {
        String::new()
    };
    card()
        .gap_2()
        .child(switch_row(
            plugin.name.clone(),
            subtitle,
            move || enabled,
            move || controller::plugin_set_enabled(toggle_id.clone(), !enabled),
        ))
        .child(when(!plugin.error.is_empty(), {
            let error = plugin.error.clone();
            move || {
                text(error.clone())
                    .text_size(px(11.5))
                    .text_color(colors.danger)
            }
        }))
        .child(
            row()
                .flex_wrap()
                .child(when(!plugin.version.is_empty(), {
                    let v = if plugin.has_update {
                        format!("{} → {}", plugin.version, plugin.latest_version)
                    } else {
                        plugin.version.clone()
                    };
                    let tone = if plugin.has_update {
                        colors.accent
                    } else {
                        colors.faint
                    };
                    move || badge(v.clone(), tone)
                }))
                .child(when(plugin.builtin, || badge("内置", colors.faint)))
                .child(when(plugin.needs_process_grant && !plugin.granted, || {
                    badge("需授权", colors.warning)
                }))
                .child(view().flex_1())
                .children(opens.into_iter().map(|(page, label)| {
                    let id = id.clone();
                    ghost_button(label, move || controller::open_plugin(id.clone(), page.clone()))
                        .disabled(move || !enabled)
                })),
        )
}

fn remote_page(data: Data) -> impl IntoElement {
    let colors = palette();
    let target = computed(|| controller::ui().with(|u| u.remote_target.clone()));
    let code = computed(|| controller::ui().with(|u| u.forms.pair_code.clone()));
    let name = computed(|| controller::ui().with(|u| u.forms.pair_name.clone()));
    let redeeming = busy("pair.redeem");
    view()
        .flex_col()
        .gap_3()
        .child(dynamic(move || {
            let remote = data.with(|d| d.as_ref().and_then(|d| d.remote.clone()));
            let Some(remote) = remote else {
                return card().child(hint("未连接 Core")).into_element();
            };
            let ok = remote.error.is_empty() && remote.enabled;
            let mut c = card().gap_2p5().child(
                row()
                    .child(super::dot(if ok {
                        colors.success
                    } else if remote.error.is_empty() {
                        colors.faint
                    } else {
                        colors.danger
                    }))
                    .child(
                        text(remote.summary())
                            .flex_1()
                            .font_weight(FontWeight::MEDIUM)
                            .text_size(px(13.0))
                            .text_color(colors.ink),
                    )
                    .child(
                        text(move || match target.get() {
                            Some(t) => t,
                            None => "本机".to_owned(),
                        })
                        .text_size(px(11.5))
                        .text_color(colors.faint),
                    )
                    .child(
                        ghost_button(
                            move || if busy("pair.create")() { "生成中…" } else { "生成配对码" },
                            controller::pair_create,
                        )
                        .disabled(busy("pair.create")),
                    )
                    .child(super::small_icon_button(
                        crate::icons::Icon::RefreshCw,
                        "刷新",
                        controller::reload_catalogs,
                    )),
            );
            if !remote.t3_pairing_link.is_empty() {
                let link = remote.t3_pairing_link.clone();
                c = c.child(caption("Scan with Vantry on iPhone"));
                let qr = qrcodegen::QrCode::encode_text(&link, qrcodegen::QrCodeEcc::Medium)
                    .or_else(|_| qrcodegen::QrCode::encode_text(&link, qrcodegen::QrCodeEcc::Low));
                if let Ok(qr) = qr {
                    let scale = (320 / (qr.size() + 8)).max(1) as f32;
                    let size = (qr.size() + 8) as f32 * scale;
                    let mut painter = Painter::new().fill_color(white()).fill_rect(0., 0., size, size)
                        .fill_color(black());
                    for y in 0..qr.size() {
                        let mut x = 0;
                        while x < qr.size() {
                            let start = x;
                            while x < qr.size() && qr.get_module(x, y) { x += 1; }
                            if x > start {
                                painter = painter.fill_rect((start + 4) as f32 * scale, (y + 4) as f32 * scale,
                                    (x - start) as f32 * scale, scale);
                            }
                            x += 1;
                        }
                    }
                    c = c.child(canvas(painter).w(px(size)).h(px(size)).flex_shrink_0());
                }
                c = c.child(link_button("Copy iPhone pairing link", move || controller::copy(link.clone())));
            }
            if !remote.pairing_code.is_empty() {
                let code = remote.pairing_code.clone();
                c = c.child(caption("Computer-to-computer pairing code"));
                let copy = code.clone();
                c = c.child(
                    row()
                        .px_3()
                        .py_2()
                        .rounded(px(8.0))
                        .bg(colors.soft)
                        .child(
                            text(code)
                                .select_text()
                                .flex_1()
                                .font_family(MONO)
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_size(px(14.0))
                                .text_color(colors.ink),
                        )
                        .child(when(!remote.pairing_expires_at.is_empty(), {
                            let at = remote.pairing_expires_at.clone();
                            move || caption(format!("至 {}", short_time(&at)))
                        }))
                        .child(link_button("复制", move || controller::copy(copy.clone()))),
                );
            }
            c.into_element()
        }))
        .child(
            card()
                .gap_2()
                .child(row().child(secret_field(
                    move || code.get(),
                    |t: &str| controller::set_pair_field(true, t),
                    "SESSION-… / vantry://connect…",
                )))
                .child(
                    row()
                        .child(field(
                            move || name.get(),
                            |t: &str| controller::set_pair_field(false, t),
                            "主机名称（可选）",
                        ))
                        .child(
                            primary_button(
                                move || if redeeming() { "配对中…" } else { "配对" },
                                controller::pair_redeem,
                            )
                            .h(px(32.0))
                            .disabled(busy("pair.redeem")),
                        ),
                ),
        )
        .child(dynamic(move || {
            let Some(data) = data.get() else {
                return view().into_element();
            };
            let confirm = controller::ui().with_untracked(|u| u.forms.confirm_revoke.clone());
            let remote = data.remote.clone().unwrap_or_default();
            let mut column = view().flex_col().gap_2();
            if !remote.peers().is_empty() {
                column = column.child(title(&format!("已配对主机 ({})", remote.peers().len())));
                for peer in remote.peers() {
                    column = column.child(peer_row(
                        peer.clone(),
                        confirm.as_deref() == Some(peer.id.as_str()),
                    ));
                }
            }
            if !remote.clients().is_empty() {
                column = column.child(title(&format!("授权设备 ({})", remote.clients().len())));
                for client in remote.clients() {
                    column = column.child(client_row(
                        client.clone(),
                        confirm.as_deref() == Some(client.id.as_str()),
                    ));
                }
            }
            if !data.environments.is_empty() {
                column = column.child(title(&format!("远端环境 ({})", data.environments.len())));
                for env in data.environments.clone() {
                    column = column.child(environment_row(env));
                }
            }
            column = column.child(
                row()
                    .pt_1()
                    .child(title(&format!("项目 ({})", data.projects.len())).flex_1())
                    .child(
                        ghost_button("添加目录", controller::project_add)
                            .disabled(busy("project.add")),
                    ),
            );
            for project in data.projects.clone() {
                column = column.child(project_row(project));
            }
            column.into_element()
        }))
}

/// `2026-11-23T08:00:00Z` → `11-23 08:00`.
fn short_time(at: &str) -> String {
    chrono::DateTime::parse_from_rfc3339(at)
        .map(|t| t.with_timezone(&chrono::Local).format("%m-%d %H:%M").to_string())
        .unwrap_or_else(|_| at.to_owned())
}

fn revoke_buttons(id: String, confirming: bool, label: &'static str) -> AnyElement {
    let colors = palette();
    if confirming {
        let yes = id.clone();
        row()
            .child(text("确认撤销？").text_size(px(11.5)).text_color(colors.danger))
            .child(link_button("撤销", move || controller::peer_revoke(yes.clone())).disabled(busy(format!("revoke:{id}"))))
            .child(link_button("取消", || controller::ask_revoke(None)))
            .into_element()
    } else {
        link_button(label, move || controller::ask_revoke(Some(id.clone()))).into_element()
    }
}

fn peer_row(peer: RemotePeer, confirming: bool) -> impl IntoElement {
    let colors = palette();
    let id = peer.id.clone();
    let reach = match peer.reachable {
        Some(true) if peer.latency_ms > 0 => (format!("在线 · {} ms", peer.latency_ms), colors.success),
        Some(true) => ("在线".to_owned(), colors.success),
        Some(false) => ("离线".to_owned(), colors.muted),
        None => ("未检测".to_owned(), colors.faint),
    };
    let rd_id = id.clone();
    card().child(
        row()
            .child(
                view()
                    .flex_1()
                    .flex_col()
                    .child(text(if peer.name.is_empty() { peer.id.clone() } else { peer.name.clone() }).text_size(px(13.0)).text_color(colors.ink))
                    .child(
                        text(format!(
                            "{}{}{}",
                            peer.id,
                            if peer.mode.is_empty() { String::new() } else { format!(" · {}", peer.mode) },
                            if peer.last_error.is_empty() { String::new() } else { format!(" · {}", peer.last_error) }
                        ))
                        .font_family(MONO)
                        .text_size(px(11.0))
                        .text_color(colors.faint),
                    ),
            )
            .child(badge(reach.0, reach.1))
            .child(link_button("远程桌面", move || {
                controller::close_settings();
                controller::show_view(MainView::RemoteDesktop);
                controller::remote_connect(rd_id.clone());
            }))
            .child(revoke_buttons(id, confirming, "撤销")),
    )
}

fn client_row(client: RemoteClient, confirming: bool) -> impl IntoElement {
    let colors = palette();
    card().child(
        row()
            .child(text(client.name.clone()).flex_1().text_size(px(13.0)).text_color(colors.ink))
            .child(when(!client.created_at.is_empty(), {
                let at = client.created_at.clone();
                move || caption(format!("配对于 {}", short_time(&at)))
            }))
            .child(revoke_buttons(client.id, confirming, "移除")),
    )
}

fn environment_row(env: EnvironmentEntry) -> impl IntoElement {
    let colors = palette();
    let id = env.id.clone();
    card().child(
        row()
            .child(text(if env.name.is_empty() { env.id.clone() } else { env.name.clone() }).text_size(px(13.0)).text_color(colors.ink))
            .child(text(env.target()).flex_1().font_family(MONO).text_size(px(11.0)).text_color(colors.faint))
            .child(link_button("移除", move || controller::environment_remove(id.clone())).disabled(busy(format!("env:{}", env.id)))),
    )
}

fn project_row(project: ProjectEntry) -> impl IntoElement {
    let colors = palette();
    let id = project.id.clone();
    card().child(
        row()
            .child(text(project.name.clone()).text_size(px(13.0)).text_color(colors.ink))
            .child(text(project.path.clone()).flex_1().font_family(MONO).text_size(px(11.0)).text_color(colors.faint))
            .child(link_button("移除", move || controller::project_remove(id.clone())).disabled(busy(format!("project:{}", project.id)))),
    )
}

fn security_page(data: Data) -> impl IntoElement {
    let colors = palette();
    let policy = computed(|| controller::ui().with(|u| u.default_policy.clone()));
    let sandbox = computed(move || data.with(|d| d.as_ref().and_then(|d| d.sandbox.clone())));
    view()
        .flex_col()
        .gap_3()
        .child(title("默认工具权限"))
        .child(
            card().gap_0().py_1().children(APPROVAL_POLICIES.iter().enumerate().map(|(idx, (value, label))| {
                let value = *value;
                let name = match value {
                    "ask" => "逐项询问",
                    "auto-accept-edits" => "改文件自动 · 命令询问",
                    "auto" => "自动执行",
                    _ => *label,
                };
                nsh_base::button()
                    .accessibility_label(name)
                    .selected(move || policy.get() == value)
                    .on_press(move || controller::set_default_policy(value.into()))
                    .h(px(42.0))
                    .px_1()
                    .style_when(move || idx > 0, |s| s.border_t_1().border_t_color(colors.line_soft))
                    .flex_row()
                    .items_center()
                    .child(setting_label(name).flex_1().font_weight(FontWeight::MEDIUM))
                    .child(when(
                        move || policy.get() == value,
                        move || crate::icons::icon(crate::icons::Icon::Check, 15.0, colors.accent),
                    ))
            })),
        )
        .child(title("工作区沙箱"))
        .child(
            card()
                .gap_2()
                .child(when(
                    move || sandbox.with(|s| s.as_ref().is_some_and(|v| v.env_override)),
                    || hint("VANTRY_SANDBOX_MODE 已锁定"),
                ))
                .child(
                    row().children(
                        SANDBOX_MODES
                            .iter()
                            .filter(|(mode, _, _)| *mode != "read-only")
                            .map(|(mode, label, _)| {
                                let mode_on = (*mode).to_owned();
                                let mode_set = (*mode).to_owned();
                                let needs_support = *mode == "workspace-write";
                                pill(
                                    *label,
                                    move || {
                                        sandbox.with(|s| {
                                            s.as_ref().is_some_and(|s| s.mode == mode_on)
                                        })
                                    },
                                    move || controller::sandbox_set(mode_set.clone()),
                                )
                                .disabled(move || {
                                    busy("sandbox")()
                                        || sandbox.with(|s| match s {
                                            None => true,
                                            Some(s) => {
                                                s.env_override
                                                    || (needs_support && !s.supported)
                                            }
                                        })
                                })
                            }),
                    ),
                ),
        )
}

fn update_page(data: Data) -> impl IntoElement {
    let colors = palette();
    let working = busy("update");
    view()
        .flex_col()
        .gap_3()
        .child(dynamic(move || {
            let update = data.with(|d| d.as_ref().map(|d| d.update.clone()));
            let Some(update) = update else {
                return card()
                    .child(
                        row()
                            .child(
                                text(format!("Vantry {}", env!("CARGO_PKG_VERSION")))
                                    .flex_1()
                                    .font_weight(FontWeight::MEDIUM)
                                    .text_size(px(13.5))
                                    .text_color(colors.ink),
                            )
                            .child(
                                ghost_button("检查更新", || controller::update_op(names::UPDATE_CHECK))
                                    .disabled(busy("update")),
                            ),
                    )
                    .into_element();
            };
            let current = if update.current_version.is_empty() {
                env!("CARGO_PKG_VERSION").to_owned()
            } else {
                update.current_version.clone()
            };
            if !update.available {
                return card()
                    .child(
                        row()
                            .child(
                                text(format!("Vantry {current}"))
                                    .font_weight(FontWeight::MEDIUM)
                                    .text_size(px(13.5))
                                    .text_color(colors.ink),
                            )
                            .child(badge("已是最新", colors.success))
                            .child(view().flex_1())
                            .child(when(working.clone(), || hint("检查中…")))
                            .child(
                                ghost_button("检查更新", || controller::update_op(names::UPDATE_CHECK))
                                    .disabled(busy("update")),
                            ),
                    )
                    .into_element();
            }
            let size = if update.delta_available {
                format!("增量 {:.1} MB", update.delta_size as f64 / 1_000_000.0)
            } else if update.asset_size > 0 {
                format!("{:.1} MB", update.asset_size as f64 / 1_000_000.0)
            } else {
                String::new()
            };
            card()
                .gap_2p5()
                .child(
                    row()
                        .child(super::dot(colors.accent))
                        .child(
                            text(format!("Vantry {current} → {}", update.latest_version))
                                .flex_1()
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_size(px(13.5))
                                .text_color(colors.ink),
                        )
                        .child(when(!size.is_empty(), {
                            let size = size.clone();
                            move || badge(size.clone(), colors.muted)
                        }))
                        .child(if update.staged {
                            primary_button("重启更新", || controller::update_op(names::UPDATE_INSTALL))
                                .disabled(busy("update"))
                                .into_element()
                        } else {
                            primary_button("下载更新", || controller::update_op(names::UPDATE_DOWNLOAD))
                                .disabled(busy("update"))
                                .into_element()
                        }),
                )
                .child(when(!update.notes.is_empty(), {
                    let notes = update.notes.clone();
                    move || hint(notes.clone())
                }))
                .into_element()
        }))
}

fn usage_page(data: Data) -> impl IntoElement {
    let colors = palette();
    view()
        .flex_col()
        .gap_2()
        .child(row().child(view().flex_1()).child(link_button("刷新", controller::usage_refresh).disabled(busy("usage"))))
        .child(dynamic(move || {
            let Some(usage) = data.with(|d| d.as_ref().and_then(|d| d.usage.clone())) else {
                return hint("暂无用量数据").into_element();
            };
            let stat = |label: &'static str, value: String| {
                view()
                    .flex_1()
                    .flex_col()
                    .gap_0p5()
                    .p_3()
                    .rounded(px(metrics::CARD_RADIUS))
                    .bg(colors.card)
                    .border_1()
                    .border_color(colors.line)
                    .child(caption(label))
                    .child(text(value).font_weight(FontWeight::SEMIBOLD).text_size(px(17.0)).text_color(colors.ink))
            };
            let mut column = view()
                .flex_col()
                .gap_2()
                .child(
                    row()
                        .child(stat("总令牌", compact_count(usage.total_tokens)))
                        .child(stat("费用", format!("${:.2}", usage.total_cost_usd)))
                        .child(stat("缓存命中", format!("{:.0}%", usage.cache_hit_rate * 100.0)))
                        .child(stat("首字延迟", if usage.avg_ttft_ms > 0 { format!("{} ms", usage.avg_ttft_ms) } else { "—".to_owned() })),
                )
                .child(hint(format!(
                    "会话 {} · 任务 {} · 运行 {}（进行中 {}，完成 {}，失败 {}） · 输入 {} / 输出 {} / 缓存读 {}",
                    usage.threads,
                    usage.tasks,
                    usage.runs,
                    usage.active_runs,
                    usage.completed_runs,
                    usage.failed_runs,
                    compact_count(usage.input_tokens),
                    compact_count(usage.output_tokens),
                    compact_count(usage.cache_read_tokens),
                )));
            let models = usage.models_by_tokens();
            let top = models.first().map_or(1, |m| m.total_tokens.max(1));
            for model in models {
                column = column.child(usage_row(model, top));
            }
            column.into_element()
        }))
}

fn usage_row(model: ModelUsage, top: i64) -> impl IntoElement {
    let colors = palette();
    let share = (model.total_tokens as f32 / top as f32).clamp(0.0, 1.0);
    card()
        .child(
            row()
                .child(text(model.model.clone()).flex_1().font_family(MONO).text_size(px(12.0)).text_color(colors.ink))
                .child(caption(format!(
                    "{} 次 · {} · ${:.2}{}",
                    model.runs,
                    compact_count(model.total_tokens),
                    model.cost_usd,
                    if model.avg_ttft_ms > 0 { format!(" · {} ms", model.avg_ttft_ms) } else { String::new() }
                ))),
        )
        .child(
            view()
                .h(px(4.0))
                .flex_row()
                .rounded_full()
                .bg(colors.soft)
                .child(view().h(px(4.0)).flex_grow(share.max(0.01)).rounded_full().bg(colors.accent))
                .child(view().flex_grow((1.0 - share).max(0.0))),
        )
}

fn appearance_page() -> impl IntoElement {
    let colors = palette();
    let theme = computed(|| controller::ui().with(|u| u.theme));
    let preset = computed(|| controller::ui().with(|u| u.theme_preset));
    view()
        .flex_col()
        .gap_3()
        .child(title("主题风格").pt_2())
        .child(
            row()
                .flex_wrap()
                .children(crate::theme::ThemePreset::ALL.map(move |choice| {
                    let sw = choice.colors();
                    nsh_base::button()
                        .basis(px(124.0))
                        .flex_grow(1.0)
                        .max_w(px(170.0))
                        .gap_2()
                        .p_2()
                        .rounded(px(12.0))
                        .border_1()
                        .border_color(move || {
                            if preset.get() == choice {
                                colors.accent
                            } else {
                                colors.line
                            }
                        })
                        .bg(colors.card)
                        .on_press(move || controller::ui().update(|u| u.theme_preset = choice))
                        .child(
                            view()
                                .flex_row()
                                .h(px(72.0))
                                .rounded(px(7.0))
                                .overflow_hidden()
                                .bg(sw.canvas)
                                .child(
                                    view()
                                        .w(px(30.0))
                                        .bg(sw.sidebar)
                                        .p_1()
                                        .child(view().h(px(6.0)).rounded_full().bg(sw.accent)),
                                )
                                .child(
                                    view()
                                        .flex_1()
                                        .p_2()
                                        .gap_1()
                                        .child(view().h(px(4.0)).w(px(30.0)).bg(sw.ink))
                                        .child(view().h(px(4.0)).bg(sw.muted))
                                        .child(
                                            view()
                                                .h(px(13.0))
                                                .bg(sw.card)
                                                .border_1()
                                                .border_color(sw.line),
                                        ),
                                ),
                        )
                        .child(
                            row()
                                .px_1()
                                .child(
                                    text(choice.label())
                                        .flex_1()
                                        .font_weight(FontWeight::MEDIUM)
                                        .text_size(px(13.0))
                                        .text_color(colors.ink),
                                )
                                .child(when(
                                    move || preset.get() == choice,
                                    move || {
                                        crate::icons::icon(
                                            crate::icons::Icon::Check,
                                            14.0,
                                            colors.accent,
                                        )
                                    },
                                )),
                        )
                })),
        )
        .child(
            card().mt_3().child(
                row()
                    .min_h(px(36.0))
                    .flex_wrap()
                    .child(setting_label("明暗模式").flex_1())
                    .child(segments().children(ThemePreference::ALL.map(|t| {
                        segment(
                            crate::tr(t.label()),
                            move || theme.get() == t,
                            move || controller::set_theme(t),
                        )
                    }))),
            ),
        )
}
