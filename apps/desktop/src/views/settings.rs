//! Settings, backed by the Core's workbench (`views` read the loaded
//! [`SettingsData`]; every change is a workbench action through the
//! controller, whose answer updates what is shown):
//!
//! - 常规与模型: the model catalog, default model and thinking level, the
//!   built-in Pi runtime and its quick setup (the API key is typed masked
//!   and leaves the desktop with the request);
//! - MCP 服务器: the catalog, a switch per server, probe, add and remove,
//!   trusting the project's configuration;
//! - 插件: the installed plugins, enable / disable, their native panels;
//! - 远端互联与配对: remote access, one-time pairing codes, redeeming a
//!   code, paired machines and allowed devices (revocable), SSH hosts and
//!   environments;
//! - 安全与沙盒: the sandbox mode and the default approval policy;
//! - 应用更新: check, download (delta or full), install;
//! - 用量统计: tokens, cost, latency, per model;
//! - 外观: the Stardust theme.
//!
//! Forms sit outside the data-driven parts, so a catalog refresh never
//! takes the focus from what is being typed.

use nsh_core::{LineHeight, prelude::*};
use vantry_protocol::{MCPServerInfo, PluginInfo, SSHHostInfo};

use super::{badge, caption, ghost_button, link_button, pill, primary_button, switch_row};
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
    theme::{PALETTE, metrics},
};

const MONO: &str = "Menlo";

/// The loaded settings (none while loading or failed).
type Data = Memo<Option<SettingsData>>;

pub(crate) fn settings() -> impl IntoElement {
    let page = computed(|| controller::ui().with(|u| u.settings));
    dynamic(move || page.get().map(|_| dialog(page).into_element()))
}

/// Sections each page shows (for the "not served" note).
fn sections_of(page: SettingsPage) -> &'static [&'static str] {
    match page {
        SettingsPage::Models => &[names::MODELS, names::PI_STATUS],
        SettingsPage::Extensions => &[names::MCP_LIST],
        SettingsPage::Plugins => &[names::PLUGINS_LIST],
        SettingsPage::Remote => &[
            names::REMOTE_STATUS,
            names::SSH_HOSTS,
            names::ENVIRONMENTS_LIST,
            names::PROJECTS_LIST,
        ],
        SettingsPage::Permissions => &[names::SANDBOX_GET],
        SettingsPage::About => &[names::UPDATE_STATUS],
        SettingsPage::Usage => &[names::USAGE_SUMMARY],
        SettingsPage::Appearance => &[],
    }
}

fn dialog(page: Memo<Option<SettingsPage>>) -> impl IntoElement {
    let catalogs = computed(|| controller::ui().with(|u| u.catalogs.clone()));
    let data: Data = computed(move || match catalogs.get() {
        Catalogs::Loaded(data) => Some(*data),
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
                .bg(PALETTE.scrim)
                .p_6()
                .child(
                    view()
                        .w_full()
                        .max_w(px(900.0))
                        .h(px(620.0))
                        .flex_row()
                        .rounded(px(14.0))
                        .bg(PALETTE.canvas)
                        .border_1()
                        .border_color(PALETTE.line)
                        .shadow_xl()
                        .overflow_hidden()
                        .child(nav(page, catalogs))
                        .child(
                            view()
                                .flex_1()
                                .flex_col()
                                .child(feedback_line())
                                .child(
                                    scroll().flex_1().child(
                                        view()
                                            .flex_col()
                                            .gap_3()
                                            .p_5()
                                            .child(degraded_note(page, data))
                                            .child(dynamic(move || match page.get() {
                                                Some(SettingsPage::Models) => models_page(data, catalogs).into_element(),
                                                Some(SettingsPage::Extensions) => mcp_page(data).into_element(),
                                                Some(SettingsPage::Plugins) => plugins_page(data).into_element(),
                                                Some(SettingsPage::Remote) => remote_page(data).into_element(),
                                                Some(SettingsPage::Permissions) => security_page(data).into_element(),
                                                Some(SettingsPage::About) => update_page(data).into_element(),
                                                Some(SettingsPage::Usage) => usage_page(data).into_element(),
                                                Some(SettingsPage::Appearance) | None => appearance_page().into_element(),
                                            })),
                                    ),
                                ),
                        ),
                ),
        )
}

fn nav(page: Memo<Option<SettingsPage>>, catalogs: Memo<Catalogs>) -> impl IntoElement {
    view()
        .w(px(196.0))
        .flex_col()
        .gap_0p5()
        .p_3()
        .bg(PALETTE.sidebar)
        .border_r_1()
        .border_r_color(PALETTE.line)
        .child(
            text("设置")
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(px(15.0))
                .text_color(PALETTE.ink)
                .px_2()
                .pb_2(),
        )
        .children(SettingsPage::ALL.map(|p| {
            nsh_base::button()
                .selected(move || page.get() == Some(p))
                .on_press(move || controller::open_settings(p))
                .px_2()
                .py_1p5()
                .rounded(px(8.0))
                .bg(move || if page.get() == Some(p) { PALETTE.card } else { AdaptiveColor::TRANSPARENT })
                .hover(|s| s.bg(PALETTE.soft))
                .child(text(p.label()).text_size(px(13.0)).text_color(PALETTE.ink))
        }))
        .child(view().flex_1())
        .child(
            text(move || match catalogs.get() {
                Catalogs::Loading => "正在读取…".to_owned(),
                Catalogs::Failed(e) => format!("读取失败：{e}"),
                _ => String::new(),
            })
            .px_2()
            .text_size(px(11.0))
            .text_color(PALETTE.faint),
        )
        .child(
            view()
                .flex_row()
                .gap_1()
                .child(link_button("刷新", controller::reload_catalogs))
                .child(view().flex_1())
                .child(ghost_button("完成", controller::close_settings)),
        )
}

/// The last operation's outcome, over the page.
fn feedback_line() -> impl IntoElement {
    let feedback = computed(|| controller::ui().with(|u| u.forms.feedback.clone()));
    when(
        move || feedback.with(Option::is_some),
        move || {
            let ok = move || feedback.with(|f| f.as_ref().is_some_and(|f| f.ok));
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .px_5()
                .py_1p5()
                .bg(move || if ok() { PALETTE.soft } else { PALETTE.approval })
                .border_b_1()
                .border_b_color(PALETTE.line_soft)
                .child(super::dot(move || if ok() { PALETTE.success } else { PALETTE.danger }))
                .child(
                    text(move || feedback.with(|f| f.as_ref().map(|f| f.text.clone()).unwrap_or_default()))
                        .flex_1()
                        .select_text()
                        .text_size(px(12.0))
                        .text_color(PALETTE.ink),
                )
                .child(link_button("✕", controller::dismiss_feedback).accessibility_label("关闭提示"))
        },
    )
}

/// Which of the page's sections the Core did not serve.
fn degraded_note(page: Memo<Option<SettingsPage>>, data: Data) -> impl IntoElement {
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
                format!("Core 暂未提供：{}；以下显示本机可读的信息。", missing.join("、"))
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
                .bg(PALETTE.soft)
                .text_size(px(11.5))
                .line_height(LineHeight::Multiple(1.45))
                .text_color(PALETTE.muted)
        },
    )
}

fn title(label: &str) -> Text {
    text(label.to_owned())
        .font_weight(FontWeight::SEMIBOLD)
        .text_size(px(16.0))
        .text_color(PALETTE.ink)
}

fn hint(label: impl IntoReactive<SharedString>) -> Text {
    text(label)
        .text_size(px(12.0))
        .line_height(LineHeight::Multiple(1.45))
        .text_color(PALETTE.muted)
}

fn card() -> View {
    view()
        .flex_col()
        .gap_1()
        .p_3()
        .rounded(px(metrics::CARD_RADIUS))
        .bg(PALETTE.card)
        .border_1()
        .border_color(PALETTE.line)
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
    nsh_base::input()
        .value(value)
        .on_change(on_change)
        .placeholder(placeholder)
        .placeholder_color(move || color(PALETTE.faint))
        .auto_correct(false)
        .auto_capitalize(AutoCapitalize::None)
        .flex_1()
        .px_2()
        .py_1()
        .rounded(px(7.0))
        .bg(PALETTE.canvas)
        .border_1()
        .border_color(PALETTE.line)
        .text_size(px(12.5))
        .text_color(PALETTE.ink)
}

/// A field whose text is hidden (keys, pairing codes).
fn secret_field(
    value: impl Fn() -> String + 'static,
    on_change: impl Fn(&str) + 'static,
    placeholder: &'static str,
) -> impl IntoElement {
    nsh_base::input()
        .masked(true)
        .value(value)
        .on_change(on_change)
        .placeholder(placeholder)
        .placeholder_color(move || color(PALETTE.faint))
        .auto_correct(false)
        .auto_capitalize(AutoCapitalize::None)
        .flex_1()
        .px_2()
        .py_1()
        .rounded(px(7.0))
        .bg(PALETTE.canvas)
        .border_1()
        .border_color(PALETTE.line)
        .text_size(px(12.5))
        .text_color(PALETTE.ink)
}

fn row() -> View {
    view().flex_row().items_center().gap_2()
}

// --------------------------------------------------------------- general

fn models_page(data: Data, catalogs: Memo<Catalogs>) -> impl IntoElement {
    let current = computed(|| controller::state().with(|s| s.new_thread.model.clone()));
    let provider = computed(|| controller::state().with(|s| s.new_thread.provider_id.clone()));
    let pi_provider = computed(|| controller::ui().with(|u| u.forms.pi_provider.clone()));
    let pi_model = computed(|| controller::ui().with(|u| u.forms.pi_model.clone()));
    let pi_key_set = computed(|| controller::ui().with(|u| !u.forms.pi_key.is_empty()));
    let saving = busy("pi.setup");
    view()
        .flex_col()
        .gap_2()
        .child(title("常规与模型"))
        .child(hint(move || format!("执行器：{}（新会话使用）", provider.get())))
        // The Pi runtime.
        .child(dynamic(move || {
            let pi = data.with(|d| d.as_ref().and_then(|d| d.pi.clone()));
            match pi {
                Some(pi) => card()
                    .child(
                        row()
                            .child(super::dot(if pi.compat_blocked { PALETTE.danger } else { PALETTE.success }))
                            .child(
                                text(format!(
                                    "Pi 运行时 · {}{}",
                                    if pi.active_source.is_empty() { "内置".to_owned() } else { pi.active_source.clone() },
                                    if pi.active_version.is_empty() { String::new() } else { format!(" {}", pi.active_version) }
                                ))
                                .flex_1()
                                .text_size(px(13.0))
                                .text_color(PALETTE.ink),
                            )
                            .child(when(pi.update_available, {
                                let latest = pi.latest_version.clone();
                                move || badge(format!("可更新 {latest}"), PALETTE.accent)
                            })),
                    )
                    .child(when(!pi.compat_reason.is_empty(), {
                        let reason = pi.compat_reason.clone();
                        move || hint(reason.clone())
                    }))
                    .into_element(),
                None => hint("Pi 运行时状态由 Core 提供；连接后在此显示。").into_element(),
            }
        }))
        // Quick setup: provider, key (masked), default model.
        .child(
            card()
                .gap_2()
                .child(caption("快速配置"))
                .child(hint("密钥写入执行所属机器的 Pi auth.json，桌面不保存、不显示。"))
                .child(row().child(field(move || pi_provider.get(), |t: &str| controller::set_pi_field(0, t), "Provider（如 anthropic）")))
                .child(row().child(secret_field(
                    move || if pi_key_set.get() { controller::ui().with_untracked(|u| u.forms.pi_key.expose().to_owned()) } else { String::new() },
                    |t: &str| controller::set_pi_field(1, t),
                    "API Key",
                )))
                .child(row().child(field(move || pi_model.get(), |t: &str| controller::set_pi_field(2, t), "默认模型 provider/model（可选）")))
                .child(
                    row()
                        .child(view().flex_1())
                        .child(primary_button(move || if saving() { "保存中…" } else { "保存配置" }, controller::pi_setup_save).disabled(busy("pi.setup"))),
                ),
        )
        // The catalog.
        .child(dynamic(move || {
            let Some(data) = data.get() else {
                return text(move || match catalogs.get() {
                    Catalogs::Failed(e) => format!("读取失败：{e}"),
                    _ => "正在读取模型目录…".to_owned(),
                })
                .text_size(px(12.0))
                .text_color(PALETTE.faint)
                .into_element();
            };
            let ids: Vec<String> = data.models.models.iter().flatten().map(|m| m.id.clone()).collect();
            let mut column = view().flex_col().gap_2();
            for warning in &data.models.warnings {
                column = column.child(text(warning.clone()).text_size(px(12.0)).text_color(PALETTE.warning));
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
                let id = model.id.clone();
                let ids = ids.clone();
                let on = {
                    let id = id.clone();
                    move || actions::split_thinking(&current.get(), &ids).0 == id
                };
                column = column.child(
                    card().child(
                        row()
                            .child(
                                view()
                                    .flex_1()
                                    .flex_col()
                                    .child(text(if model.name.is_empty() { model.id.clone() } else { model.name.clone() }).text_size(px(13.0)).text_color(PALETTE.ink))
                                    .child(
                                        text(format!(
                                            "{} · 上下文 {}k{}",
                                            model.id,
                                            model.context_window / 1000,
                                            if model.reasoning { " · 推理" } else { "" }
                                        ))
                                        .font_family(MONO)
                                        .text_size(px(11.0))
                                        .text_color(PALETTE.faint),
                                    ),
                            )
                            .child(when(data.models.default == model.id, || badge("Core 默认", PALETTE.muted)))
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

// -------------------------------------------------------------------- MCP

fn mcp_page(data: Data) -> impl IntoElement {
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
            let mcp = data.mcp;
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
                            badge("已信任", PALETTE.success).into_element()
                        } else {
                            link_button("信任项目配置", controller::mcp_trust).disabled(busy("mcp.trust")).into_element()
                        }),
                );
            }
            if !mcp.probe_error.is_empty() {
                column = column.child(text(mcp.probe_error.clone()).text_size(px(11.5)).text_color(PALETTE.danger));
            }
            let servers = mcp.servers.unwrap_or_default();
            if servers.is_empty() {
                column = column.child(hint("还没有 MCP 服务器。"));
            }
            for server in servers {
                column = column.child(mcp_row(server));
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
    let toggle_name = name.clone();
    let probe_name = name.clone();
    let remove_name = name.clone();
    card()
        .child(switch_row(name.clone(), detail, on.clone(), move || {
            controller::mcp_set_enabled(toggle_name.clone(), !on())
        }))
        .child(
            row()
                .child(when(!state.is_empty(), move || {
                    badge(state.clone(), if bad { PALETTE.danger } else { PALETTE.success })
                }))
                .child(view().flex_1())
                .child(link_button("测试探活", move || controller::mcp_probe(probe_name.clone())).disabled(busy(format!("mcp.probe:{name}"))))
                .child(link_button("删除", move || controller::mcp_remove(remove_name.clone())).disabled(busy(format!("mcp:{}", server.name)))),
        )
}

// ---------------------------------------------------------------- plugins

fn plugins_page(data: Data) -> impl IntoElement {
    view()
        .flex_col()
        .gap_2()
        .child(title("插件"))
        .child(hint("插件由 Core 插件宿主运行；原生面板在主区域打开，动作经 plugins.run 回传。"))
        .child(dynamic(move || {
            let Some(data) = data.get() else {
                return hint("正在读取插件…").into_element();
            };
            if data.plugins.is_empty() {
                return hint("没有已安装的插件。").into_element();
            }
            let mut column = view().flex_col().gap_2();
            for plugin in data.plugins {
                column = column.child(plugin_row(plugin));
            }
            column.into_element()
        }))
}

fn plugin_row(plugin: PluginInfo) -> impl IntoElement {
    let id = plugin.id.clone();
    let enabled = !plugin.disabled;
    let mut opens: Vec<(String, String)> = plugin
        .navigation
        .iter()
        .map(|n| (n.page.clone(), if n.title.is_empty() { "打开面板".to_owned() } else { format!("打开 {}", n.title) }))
        .collect();
    if opens.is_empty() {
        opens.push(("main".to_owned(), "打开面板".to_owned()));
    }
    let toggle_id = id.clone();
    card()
        .child(switch_row(
            plugin.name.clone(),
            if plugin.description.is_empty() { plugin.id.clone() } else { plugin.description.clone() },
            move || enabled,
            move || controller::plugin_set_enabled(toggle_id.clone(), !enabled),
        ))
        .child(
            row()
                .flex_wrap()
                .child(badge(
                    if plugin.has_update { format!("{} → {}", plugin.version, plugin.latest_version) } else if plugin.version.is_empty() { "—".to_owned() } else { plugin.version.clone() },
                    if plugin.has_update { PALETTE.accent } else { PALETTE.muted },
                ))
                .child(when(plugin.builtin, || badge("内置", PALETTE.muted)))
                .child(when(plugin.needs_process_grant && !plugin.granted, || badge("需授权进程", PALETTE.warning)))
                .child(when(!plugin.error.is_empty(), {
                    let error = plugin.error.clone();
                    move || badge(error.clone(), PALETTE.danger)
                }))
                .child(view().flex_1())
                .children(opens.into_iter().map(|(page, label)| {
                    let id = id.clone();
                    link_button(label, move || controller::open_plugin(id.clone(), page.clone())).disabled(move || !enabled)
                })),
        )
}

// ----------------------------------------------------------------- remote

fn remote_page(data: Data) -> impl IntoElement {
    let target = computed(|| controller::ui().with(|u| u.remote_target.clone()));
    let code = computed(|| controller::ui().with(|u| u.forms.pair_code.clone()));
    let name = computed(|| controller::ui().with(|u| u.forms.pair_name.clone()));
    let redeeming = busy("pair.redeem");
    view()
        .flex_col()
        .gap_2()
        .child(title("远端互联与配对"))
        .child(hint(move || match target.get() {
            Some(target) => format!("当前连接：{target}"),
            None => "当前连接：本机 Core".to_owned(),
        }))
        // This machine: status and a one-time code.
        .child(dynamic(move || {
            let remote = data.with(|d| d.as_ref().and_then(|d| d.remote.clone()));
            let Some(remote) = remote else {
                return card().child(hint("远端访问状态由 Core 提供；连接后在此显示。")).into_element();
            };
            let ok = remote.error.is_empty() && remote.enabled;
            let mut c = card()
                .gap_2()
                .child(
                    row()
                        .child(super::dot(if ok { PALETTE.success } else if remote.error.is_empty() { PALETTE.faint } else { PALETTE.danger }))
                        .child(text(remote.summary()).flex_1().text_size(px(13.0)).text_color(PALETTE.ink))
                        .child(ghost_button(move || if busy("pair.create")() { "生成中…" } else { "生成配对码" }, controller::pair_create).disabled(busy("pair.create"))),
                );
            if !remote.pairing_code.is_empty() {
                let code = remote.pairing_code.clone();
                let copy = code.clone();
                c = c.child(
                    row()
                        .p_2()
                        .rounded(px(8.0))
                        .bg(PALETTE.soft)
                        .child(
                            text(code)
                                .select_text()
                                .flex_1()
                                .font_family(MONO)
                                .font_weight(FontWeight::SEMIBOLD)
                                .text_size(px(15.0))
                                .text_color(PALETTE.ink),
                        )
                        .child(when(!remote.pairing_expires_at.is_empty(), {
                            let at = remote.pairing_expires_at.clone();
                            move || caption(format!("有效至 {}", short_time(&at)))
                        }))
                        .child(link_button("复制", move || controller::copy(copy.clone()))),
                );
                c = c.child(hint("一次性配对码：在另一台设备的「兑换配对码」中输入，使用后立即失效。"));
            }
            c.into_element()
        }))
        // Redeem a code from another machine.
        .child(
            card()
                .gap_2()
                .child(caption("兑换配对码，添加远端主机"))
                .child(row().child(secret_field(move || code.get(), |t: &str| controller::set_pair_field(true, t), "SESSION-… / vantry://connect…")))
                .child(
                    row()
                        .child(field(move || name.get(), |t: &str| controller::set_pair_field(false, t), "这台主机的名称（可选）"))
                        .child(primary_button(move || if redeeming() { "配对中…" } else { "配对" }, controller::pair_redeem).disabled(busy("pair.redeem"))),
                ),
        )
        .child(dynamic(move || {
            let Some(data) = data.get() else {
                return view().into_element();
            };
            let confirm = controller::ui().with_untracked(|u| u.forms.confirm_revoke.clone());
            let remote = data.remote.clone().unwrap_or_default();
            let mut column = view().flex_col().gap_2();
            column = column.child(caption(format!("已配对主机 · {}", remote.peers().len())));
            if remote.peers().is_empty() {
                column = column.child(hint("还没有已配对的主机。"));
            }
            for peer in remote.peers() {
                column = column.child(peer_row(peer.clone(), confirm.as_deref() == Some(peer.id.as_str())));
            }
            if !remote.clients().is_empty() {
                column = column.child(caption(format!("允许访问本机的设备 · {}", remote.clients().len())));
                for client in remote.clients() {
                    column = column.child(client_row(client.clone(), confirm.as_deref() == Some(client.id.as_str())));
                }
            }
            column = column.child(caption(format!("SSH 主机 · {}", data.ssh_hosts.len())));
            for host in data.ssh_hosts.clone() {
                column = column.child(ssh_row(host));
            }
            column = column.child(caption(format!("远端环境 · {}", data.environments.len())));
            for env in data.environments.clone() {
                column = column.child(environment_row(env));
            }
            column = column.child(
                row()
                    .child(caption(format!("项目 · {}", data.projects.len())).flex_1())
                    .child(link_button("添加项目目录", controller::project_add).disabled(busy("project.add"))),
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
    if confirming {
        let yes = id.clone();
        row()
            .child(text("确认撤销？").text_size(px(11.5)).text_color(PALETTE.danger))
            .child(link_button("撤销", move || controller::peer_revoke(yes.clone())).disabled(busy(format!("revoke:{id}"))))
            .child(link_button("取消", || controller::ask_revoke(None)))
            .into_element()
    } else {
        link_button(label, move || controller::ask_revoke(Some(id.clone()))).into_element()
    }
}

fn peer_row(peer: RemotePeer, confirming: bool) -> impl IntoElement {
    let id = peer.id.clone();
    let reach = match peer.reachable {
        Some(true) if peer.latency_ms > 0 => (format!("在线 · {} ms", peer.latency_ms), PALETTE.success),
        Some(true) => ("在线".to_owned(), PALETTE.success),
        Some(false) => ("离线".to_owned(), PALETTE.muted),
        None => ("未检测".to_owned(), PALETTE.faint),
    };
    let rd_id = id.clone();
    card().child(
        row()
            .child(
                view()
                    .flex_1()
                    .flex_col()
                    .child(text(if peer.name.is_empty() { peer.id.clone() } else { peer.name.clone() }).text_size(px(13.0)).text_color(PALETTE.ink))
                    .child(
                        text(format!(
                            "{}{}{}",
                            peer.id,
                            if peer.mode.is_empty() { String::new() } else { format!(" · {}", peer.mode) },
                            if peer.last_error.is_empty() { String::new() } else { format!(" · {}", peer.last_error) }
                        ))
                        .font_family(MONO)
                        .text_size(px(11.0))
                        .text_color(PALETTE.faint),
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
    card().child(
        row()
            .child(text(client.name.clone()).flex_1().text_size(px(13.0)).text_color(PALETTE.ink))
            .child(when(!client.created_at.is_empty(), {
                let at = client.created_at.clone();
                move || caption(format!("配对于 {}", short_time(&at)))
            }))
            .child(revoke_buttons(client.id, confirming, "移除")),
    )
}

fn ssh_row(host: SSHHostInfo) -> impl IntoElement {
    let detail = format!(
        "{}{}{}",
        if host.user.is_empty() { String::new() } else { format!("{}@", host.user) },
        if host.host_name.is_empty() { host.alias.clone() } else { host.host_name.clone() },
        if host.port > 0 { format!(":{}", host.port) } else { String::new() },
    );
    let reach = match host.reachable {
        Some(true) => format!("可达 · {} ms", host.latency_ms),
        Some(false) => "不可达".to_owned(),
        None => "未检测".to_owned(),
    };
    let ok = host.reachable == Some(true);
    card().child(
        row()
            .child(text(host.alias.clone()).text_size(px(13.0)).text_color(PALETTE.ink))
            .child(text(detail).flex_1().font_family(MONO).text_size(px(11.0)).text_color(PALETTE.faint))
            .child(badge(reach, if ok { PALETTE.success } else { PALETTE.muted })),
    )
}

fn environment_row(env: EnvironmentEntry) -> impl IntoElement {
    let id = env.id.clone();
    card().child(
        row()
            .child(text(if env.name.is_empty() { env.id.clone() } else { env.name.clone() }).text_size(px(13.0)).text_color(PALETTE.ink))
            .child(text(env.target()).flex_1().font_family(MONO).text_size(px(11.0)).text_color(PALETTE.faint))
            .child(link_button("移除", move || controller::environment_remove(id.clone())).disabled(busy(format!("env:{}", env.id)))),
    )
}

fn project_row(project: ProjectEntry) -> impl IntoElement {
    let id = project.id.clone();
    card().child(
        row()
            .child(text(project.name.clone()).text_size(px(13.0)).text_color(PALETTE.ink))
            .child(text(project.path.clone()).flex_1().font_family(MONO).text_size(px(11.0)).text_color(PALETTE.faint))
            .child(link_button("移除", move || controller::project_remove(id.clone())).disabled(busy(format!("project:{}", project.id)))),
    )
}

// --------------------------------------------------------------- security

fn security_page(data: Data) -> impl IntoElement {
    let policy = computed(|| controller::ui().with(|u| u.default_policy.clone()));
    let sandbox = computed(move || data.with(|d| d.as_ref().and_then(|d| d.sandbox.clone())));
    view()
        .flex_col()
        .gap_3()
        .child(title("安全与沙盒"))
        .child(
            card()
                .gap_2()
                .child(caption("执行沙盒"))
                .child(hint(move || match sandbox.get() {
                    Some(s) if s.env_override => "沙盒模式由环境变量 VANTRY_SANDBOX 指定，此处不能修改。".to_owned(),
                    Some(_) => "工具及其子进程继承执行进程的沙盒。".to_owned(),
                    None => "沙盒状态由 Core 提供；连接后可切换。".to_owned(),
                }))
                .children(SANDBOX_MODES.iter().map(|(mode, label, help)| {
                    let mode_on = (*mode).to_owned();
                    let mode_set = (*mode).to_owned();
                    let needs_support = *mode == "workspace-write";
                    row()
                        .child(
                            pill(*label, move || sandbox.with(|s| s.as_ref().is_some_and(|s| s.mode == mode_on)), move || {
                                controller::sandbox_set(mode_set.clone())
                            })
                            .disabled(move || {
                                busy("sandbox")()
                                    || sandbox.with(|s| match s {
                                        None => true,
                                        Some(s) => s.env_override || (needs_support && !s.supported),
                                    })
                            }),
                        )
                        .child(hint(*help))
                })),
        )
        .child(
            card()
                .gap_2()
                .child(caption("新会话默认审批"))
                .child(hint("bash / write / edit 等工具的确认方式；子任务继承父会话。"))
                .child(row().children(APPROVAL_POLICIES.iter().map(|(value, label)| {
                    let value = (*value).to_owned();
                    let compare = value.clone();
                    pill(*label, move || policy.get() == compare, move || controller::set_default_policy(value.clone()))
                }))),
        )
}

// ----------------------------------------------------------------- update

fn update_page(data: Data) -> impl IntoElement {
    let working = busy("update");
    view()
        .flex_col()
        .gap_2()
        .child(title("应用更新"))
        .child(text(format!("Vantry {}（NSH 原生桌面）", env!("CARGO_PKG_VERSION"))).text_size(px(13.0)).text_color(PALETTE.ink))
        .child(dynamic(move || {
            let Some(update) = data.with(|d| d.as_ref().map(|d| d.update.clone())) else {
                return hint("正在读取更新状态…").into_element();
            };
            let size = if update.delta_available {
                format!("增量包 {:.1} MB", update.delta_size as f64 / 1_000_000.0)
            } else if update.asset_size > 0 {
                format!("完整包 {:.1} MB", update.asset_size as f64 / 1_000_000.0)
            } else {
                "完整包".to_owned()
            };
            if !update.available {
                return card()
                    .child(hint(format!(
                        "当前 {} 已是最新版本。",
                        if update.current_version.is_empty() { env!("CARGO_PKG_VERSION") } else { &update.current_version }
                    )))
                    .into_element();
            }
            card()
                .gap_2()
                .child(
                    row()
                        .child(super::dot(PALETTE.accent))
                        .child(text(format!("有新版本 {}", update.latest_version)).flex_1().font_weight(FontWeight::SEMIBOLD).text_size(px(13.0)).text_color(PALETTE.ink))
                        .child(badge(size, PALETTE.muted)),
                )
                .child(when(!update.notes.is_empty(), {
                    let notes = update.notes.clone();
                    move || hint(notes.clone())
                }))
                .child(hint(if update.staged {
                    format!(
                        "已下载 {}{}，{}",
                        update.staged_version,
                        if update.used_delta { "（增量）" } else { "" },
                        if update.can_auto_restart { "可重启安装。" } else { "请退出后运行安装程序。" }
                    )
                } else if update.delta_available {
                    "将优先下载增量包，失败时自动回退完整包。".to_owned()
                } else {
                    "下载后校验签名，再由 vantry-service 安装。".to_owned()
                }))
                .child(
                    row()
                        .child(view().flex_1())
                        .child(if update.staged {
                            primary_button("重启并更新", || controller::update_op(names::UPDATE_INSTALL)).disabled(busy("update")).into_element()
                        } else {
                            primary_button("下载更新", || controller::update_op(names::UPDATE_DOWNLOAD)).disabled(busy("update")).into_element()
                        }),
                )
                .into_element()
        }))
        .child(
            row()
                .child(when(working.clone(), || hint("正在处理更新…")))
                .child(view().flex_1())
                .child(ghost_button("检查更新", || controller::update_op(names::UPDATE_CHECK)).disabled(busy("update"))),
        )
}

// ------------------------------------------------------------------ usage

fn usage_page(data: Data) -> impl IntoElement {
    view()
        .flex_col()
        .gap_2()
        .child(row().child(title("用量统计")).child(view().flex_1()).child(link_button("刷新", controller::usage_refresh).disabled(busy("usage"))))
        .child(dynamic(move || {
            let Some(usage) = data.with(|d| d.as_ref().and_then(|d| d.usage.clone())) else {
                return hint("用量汇总由 Core 提供；连接后在此显示。").into_element();
            };
            let stat = |label: &'static str, value: String| {
                view()
                    .flex_1()
                    .flex_col()
                    .gap_0p5()
                    .p_3()
                    .rounded(px(metrics::CARD_RADIUS))
                    .bg(PALETTE.card)
                    .border_1()
                    .border_color(PALETTE.line)
                    .child(caption(label))
                    .child(text(value).font_weight(FontWeight::SEMIBOLD).text_size(px(17.0)).text_color(PALETTE.ink))
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
    let share = (model.total_tokens as f32 / top as f32).clamp(0.0, 1.0);
    card()
        .child(
            row()
                .child(text(model.model.clone()).flex_1().font_family(MONO).text_size(px(12.0)).text_color(PALETTE.ink))
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
                .bg(PALETTE.soft)
                .child(view().h(px(4.0)).flex_grow(share.max(0.01)).rounded_full().bg(PALETTE.accent))
                .child(view().flex_grow((1.0 - share).max(0.0))),
        )
}

// ------------------------------------------------------------- appearance

fn appearance_page() -> impl IntoElement {
    let theme = computed(|| controller::ui().with(|u| u.theme));
    view()
        .flex_col()
        .gap_3()
        .child(title("外观"))
        .child(hint("Stardust（星尘）：浅色暖纸侧栏与白色阅读画布，深色石墨侧栏与深灰画布。"))
        .child(
            row().children(ThemePreference::ALL.map(|t| {
                pill(t.label(), move || theme.get() == t, move || controller::set_theme(t))
            })),
        )
        .child(
            view()
                .flex_row()
                .gap_3()
                .child(swatch("侧栏", PALETTE.sidebar))
                .child(swatch("画布", PALETTE.canvas))
                .child(swatch("强调", PALETTE.accent))
                .child(swatch("审批", PALETTE.approval)),
        )
}

fn swatch(label: &'static str, fill: AdaptiveColor) -> impl IntoElement {
    view()
        .flex_col()
        .items_center()
        .gap_1()
        .child(view().w(px(44.0)).h(px(28.0)).rounded(px(6.0)).bg(fill).border_1().border_color(PALETTE.line))
        .child(text(label).text_size(px(11.0)).text_color(PALETTE.muted))
}
