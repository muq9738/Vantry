//! Settings: providers and models, user extensions and each MCP server,
//! the Stardust theme (light / dark / system), the default permission for
//! new threads, remote environments and paired peers, and the update.

use nsh_core::{LineHeight, prelude::*};
use vantry_protocol::{EnvironmentInfo, MCPServerInfo, SSHHostInfo};

use super::{badge, caption, ghost_button, link_button, pill, switch_row};
use crate::{
    bridge::workbench::SettingsData,
    controller,
    state::{
        APPROVAL_POLICIES,
        ui::{Catalogs, SettingsPage, ThemePreference},
    },
    theme::{PALETTE, metrics},
};

pub(crate) fn settings() -> impl IntoElement {
    let page = computed(|| controller::ui().with(|u| u.settings));
    dynamic(move || page.get().map(|_| dialog(page).into_element()))
}

fn dialog(page: Memo<Option<SettingsPage>>) -> impl IntoElement {
    let catalogs = computed(|| controller::ui().with(|u| u.catalogs.clone()));
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
                        .max_w(px(820.0))
                        .h(px(560.0))
                        .flex_row()
                        .rounded(px(14.0))
                        .bg(PALETTE.canvas)
                        .border_1()
                        .border_color(PALETTE.line)
                        .shadow_xl()
                        .overflow_hidden()
                        .child(
                            view()
                                .w(px(200.0))
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
                                .child(ghost_button("完成", controller::close_settings)),
                        )
                        .child(
                            scroll().flex_1().child(view().flex_col().gap_3().p_5().child(dynamic(move || {
                                let data = match catalogs.get() {
                                    Catalogs::Loaded(data) => Some(*data),
                                    _ => None,
                                };
                                match page.get() {
                                    Some(SettingsPage::Models) => models_page(data, catalogs).into_element(),
                                    Some(SettingsPage::Extensions) => extensions_page(data).into_element(),
                                    Some(SettingsPage::Appearance) => appearance_page().into_element(),
                                    Some(SettingsPage::Permissions) => permissions_page().into_element(),
                                    Some(SettingsPage::Remote) => remote_page(data).into_element(),
                                    Some(SettingsPage::About) | None => about_page(data).into_element(),
                                }
                            }))),
                        ),
                ),
        )
}

fn title(label: &str) -> Text {
    text(label.to_owned())
        .font_weight(FontWeight::SEMIBOLD)
        .text_size(px(16.0))
        .text_color(PALETTE.ink)
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

fn loading_line(catalogs: Memo<Catalogs>) -> impl IntoElement {
    text(move || match catalogs.get() {
        Catalogs::Loading => "正在读取…".to_owned(),
        Catalogs::Failed(e) => format!("读取失败：{e}"),
        _ => String::new(),
    })
    .text_size(px(12.0))
    .text_color(PALETTE.faint)
}

fn models_page(data: Option<SettingsData>, catalogs: Memo<Catalogs>) -> impl IntoElement {
    let current = computed(|| controller::state().with(|s| s.new_thread.model.clone()));
    let provider = computed(|| controller::state().with(|s| s.new_thread.provider_id.clone()));
    let mut column = view()
        .flex_col()
        .gap_2()
        .child(title("模型与 Provider"))
        .child(
            text(move || format!("执行器：{}（新会话使用）", provider.get()))
                .text_size(px(12.5))
                .text_color(PALETTE.muted),
        )
        .child(
            text(move || {
                let model = current.get();
                format!("新会话默认模型：{}", if model.is_empty() { "Pi 默认".to_owned() } else { model })
            })
            .text_size(px(12.5))
            .text_color(PALETTE.muted),
        )
        .child(
            view()
                .flex_row()
                .gap_2()
                .child(link_button("刷新目录", controller::reload_catalogs)),
        );
    let Some(data) = data else {
        return column.child(loading_line(catalogs));
    };
    for warning in &data.models.warnings {
        column = column.child(text(warning.clone()).text_size(px(12.0)).text_color(PALETTE.warning));
    }
    let models = data.models.models.clone().unwrap_or_default();
    if models.is_empty() {
        column = column.child(
            text("目录为空：可在输入区的模型选择器中直接输入 provider/model。")
                .text_size(px(12.0))
                .text_color(PALETTE.faint),
        );
    }
    for model in models {
        let id = model.id.clone();
        let on = {
            let id = id.clone();
            move || current.get() == id
        };
        column = column.child(
            card().child(
                view()
                    .flex_row()
                    .items_center()
                    .gap_2()
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
                                .font_family("Menlo")
                                .text_size(px(11.0))
                                .text_color(PALETTE.faint),
                            ),
                    )
                    .child(pill(
                        {
                            let on = on.clone();
                            move || if on() { "默认" } else { "设为默认" }
                        },
                        on,
                        move || controller::set_default_model(id.clone()),
                    )),
            ),
        );
    }
    if !data.models.thinking_levels.is_empty() {
        column = column.child(caption(format!("思考强度：{}", data.models.thinking_levels.join(" / "))));
    }
    column
}

fn extensions_page(data: Option<SettingsData>) -> impl IntoElement {
    let enabled = computed(|| controller::ui().with(|u| u.extensions_enabled));
    let mut column = view()
        .flex_col()
        .gap_2()
        .child(title("扩展与 MCP"))
        .child(card().child(switch_row(
            "用户扩展",
            "加载 ~/.pi/agent 下的 TypeScript 扩展与技能",
            move || enabled.get(),
            controller::toggle_extensions,
        )));
    let Some(data) = data else {
        return column;
    };
    if !data.mcp.notice.is_empty() {
        column = column.child(text(data.mcp.notice.clone()).text_size(px(12.0)).text_color(PALETTE.muted));
    }
    if !data.mcp.user_file.is_empty() {
        column = column.child(caption(format!("用户配置：{}", data.mcp.user_file)));
    }
    for server in data.mcp.servers.clone().unwrap_or_default() {
        column = column.child(mcp_row(server));
    }
    column
}

fn mcp_row(server: MCPServerInfo) -> impl IntoElement {
    let name = server.name.clone();
    let catalog_on = server.enabled;
    let on = {
        let name = name.clone();
        move || controller::ui().with(|u| u.mcp_enabled(&name, catalog_on))
    };
    let detail = format!(
        "{} · {} · {}{}",
        server.scope,
        server.transport,
        server.exposure,
        if server.tools.is_empty() {
            String::new()
        } else {
            format!(" · {} 个工具", server.tools.len())
        }
    );
    let state = if !server.live_error.is_empty() {
        server.live_error.clone()
    } else if !server.error.is_empty() {
        server.error.clone()
    } else {
        server.live_state.clone()
    };
    card()
        .child(switch_row(name.clone(), detail, on, move || controller::toggle_mcp(name.clone())))
        .child(when(!state.is_empty(), move || {
            text(state.clone()).text_size(px(11.0)).text_color(PALETTE.faint)
        }))
}

fn appearance_page() -> impl IntoElement {
    let theme = computed(|| controller::ui().with(|u| u.theme));
    view()
        .flex_col()
        .gap_3()
        .child(title("外观"))
        .child(text("Stardust（星尘）：浅色暖纸侧栏与白色阅读画布，深色石墨侧栏与深灰画布。").text_size(px(12.5)).text_color(PALETTE.muted))
        .child(
            view()
                .flex_row()
                .gap_2()
                .children(ThemePreference::ALL.map(|t| {
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

fn permissions_page() -> impl IntoElement {
    let policy = computed(|| controller::ui().with(|u| u.default_policy.clone()));
    view()
        .flex_col()
        .gap_3()
        .child(title("默认权限"))
        .child(
            text("新会话的工具权限；子任务继承。确认模式会在 bash / write / edit 前询问。")
                .text_size(px(12.5))
                .line_height(LineHeight::Multiple(1.5))
                .text_color(PALETTE.muted),
        )
        .child(view().flex_row().gap_2().children(APPROVAL_POLICIES.iter().map(|(value, label)| {
            let value = (*value).to_owned();
            let compare = value.clone();
            pill(*label, move || policy.get() == compare, move || controller::set_default_policy(value.clone()))
        })))
}

fn remote_page(data: Option<SettingsData>) -> impl IntoElement {
    let target = computed(|| controller::ui().with(|u| u.remote_target.clone()));
    let mut column = view()
        .flex_col()
        .gap_2()
        .child(title("远端与配对"))
        .child(
            text(move || match target.get() {
                Some(target) => format!("当前连接：{target}"),
                None => "当前连接：本机 Core".to_owned(),
            })
            .text_size(px(12.5))
            .text_color(PALETTE.muted),
        )
        .child(
            text("用 vantry --remote <主机> 或 --peer <ID> 启动即可切换到远端 Core。")
                .text_size(px(12.0))
                .text_color(PALETTE.faint),
        );
    let Some(data) = data else {
        return column;
    };
    column = column.child(caption(format!("已配对节点 · {}", data.peers.len())));
    for peer in data.peers.clone() {
        column = column.child(peer_row(peer));
    }
    column = column.child(caption(format!("SSH 主机 · {}", data.ssh_hosts.len())));
    for host in data.ssh_hosts.clone() {
        column = column.child(ssh_row(host));
    }
    column
}

fn peer_row(peer: EnvironmentInfo) -> impl IntoElement {
    let id = peer.id.clone();
    card().child(
        view()
            .flex_row()
            .items_center()
            .gap_2()
            .child(
                view()
                    .flex_1()
                    .flex_col()
                    .child(text(peer.name.clone()).text_size(px(13.0)).text_color(PALETTE.ink))
                    .child(text(format!("{} · {}/{}", peer.id, peer.os, peer.arch)).text_size(px(11.0)).text_color(PALETTE.faint)),
            )
            .child(link_button("远程桌面", move || {
                controller::close_settings();
                controller::show_view(crate::state::ui::MainView::RemoteDesktop);
                controller::remote_connect(id.clone());
            })),
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
        view()
            .flex_row()
            .items_center()
            .gap_2()
            .child(text(host.alias.clone()).text_size(px(13.0)).text_color(PALETTE.ink))
            .child(text(detail).flex_1().font_family("Menlo").text_size(px(11.0)).text_color(PALETTE.faint))
            .child(badge(reach, if ok { PALETTE.success } else { PALETTE.muted })),
    )
}

fn about_page(data: Option<SettingsData>) -> impl IntoElement {
    let mut column = view()
        .flex_col()
        .gap_2()
        .child(title("关于与更新"))
        .child(text(format!("Vantry {}（NSH 原生桌面）", env!("CARGO_PKG_VERSION"))).text_size(px(13.0)).text_color(PALETTE.ink));
    let Some(data) = data else {
        return column;
    };
    let update = data.update;
    column = column.child(card().child(if update.available {
        view()
            .flex_col()
            .gap_1()
            .child(
                view()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .child(super::dot(PALETTE.accent))
                    .child(
                        text(format!("有新版本 {}", update.latest_version))
                            .text_size(px(13.0))
                            .font_weight(FontWeight::SEMIBOLD)
                            .text_color(PALETTE.ink),
                    ),
            )
            .child(
                text(if update.delta_available {
                    format!("差分包 {:.1} MB", update.delta_size as f64 / 1_000_000.0)
                } else if update.asset_size > 0 {
                    format!("完整包 {:.1} MB", update.asset_size as f64 / 1_000_000.0)
                } else {
                    "完整包".to_owned()
                })
                .text_size(px(12.0))
                .text_color(PALETTE.muted),
            )
            .child(text(update.notes.clone()).text_size(px(12.0)).text_color(PALETTE.muted))
            .child(
                text(if update.staged {
                    format!("已下载 {}，重启后安装。", update.staged_version)
                } else {
                    "更新由 vantry-service 下载与安装。".to_owned()
                })
                .text_size(px(11.5))
                .text_color(PALETTE.faint),
            )
    } else {
        view().child(
            text(format!("当前 {} 已是最新版本。", update.current_version))
                .text_size(px(12.5))
                .text_color(PALETTE.muted),
        )
    }));
    column.child(link_button("重新检查", controller::reload_catalogs))
}
