//! The remote desktop view: the session's state from the Core
//! (`rd.status`, `rd.session.start` / `rd.session.stop`), a control bar
//! (resolution, quality, fit, keyboard and pointer control, clipboard,
//! disconnect, back to the conversation), missing permissions, and the
//! canvas the media stream draws into. Paired hosts come from the remote
//! access settings; a failed start shows why and offers a retry.

use nsh_core::prelude::*;

use super::{caption, dot, ghost_button, link_button, pill, primary_button};
use crate::{
    controller,
    state::ui::{Catalogs, Quality, RemoteLink, Resolution},
    theme::PALETTE,
};

/// The canvas's anchor for the media stream (Wave 3 binds frames to it).
pub(crate) const CANVAS_ID: &str = "vantry-remote-desktop-canvas";

pub(crate) fn remote_desktop() -> impl IntoElement {
    let remote = computed(|| controller::ui().with(|u| u.remote.clone()));
    let peers = computed(|| {
        controller::ui().with(|u| match &u.catalogs {
            Catalogs::Loaded(data) => data
                .peers
                .iter()
                .map(|p| (p.id.clone(), p.name.clone()))
                .collect::<Vec<_>>(),
            _ => Vec::new(),
        })
    });
    effect(|| {
        if controller::ui().with(|u| matches!(u.catalogs, Catalogs::NotLoaded)) {
            controller::load_settings_catalogs();
        }
    });
    let connected = move || matches!(remote.with(|r| r.link.clone()), RemoteLink::Connected { .. });
    let busy = move || matches!(remote.with(|r| r.link.clone()), RemoteLink::Connecting { .. });
    let link_color = move || match remote.with(|r| r.link.clone()) {
        RemoteLink::Connected { .. } => PALETTE.success,
        RemoteLink::Connecting { .. } => PALETTE.accent,
        RemoteLink::Failed { .. } => PALETTE.danger,
        RemoteLink::Idle => PALETTE.faint,
    };
    view()
        .flex_1()
        .flex_col()
        .bg(PALETTE.canvas)
        .child(
            view()
                .flex_row()
                .flex_wrap()
                .items_center()
                .gap_2()
                .px_5()
                .py_2()
                .border_b_1()
                .border_b_color(PALETTE.line_soft)
                .child(dot(link_color))
                .child(
                    text(move || remote.with(|r| r.status_label()))
                        .number_of_lines(1u32)
                        .text_size(px(12.5))
                        .text_color(PALETTE.ink),
                )
                .child(
                    text(move || remote.with(|r| r.detail()))
                        .number_of_lines(1u32)
                        .text_size(px(11.5))
                        .text_color(PALETTE.muted),
                )
                .child(view().flex_1())
                .children(Resolution::ALL.map(|res| {
                    pill(
                        res.label(),
                        move || remote.with(|r| r.resolution == res),
                        move || controller::remote_update(move |r| r.resolution = res),
                    )
                }))
                .child(view().w(px(6.0)))
                .children(Quality::ALL.map(|q| {
                    pill(
                        q.label(),
                        move || remote.with(|r| r.quality == q),
                        move || controller::remote_update(move |r| r.quality = q),
                    )
                }))
                .child(view().w(px(6.0)))
                .child(pill(
                    "适应窗口",
                    move || remote.with(|r| r.fit),
                    || controller::remote_update(|r| r.fit = !r.fit),
                ))
                .child(
                    pill(
                        "键鼠控制",
                        move || remote.with(|r| r.control),
                        || controller::remote_update(|r| {
                            r.toggle_control();
                        }),
                    )
                    .disabled(move || !connected()),
                )
                .child(pill(
                    "剪贴板同步",
                    move || remote.with(|r| r.clipboard),
                    || controller::remote_update(|r| r.clipboard = !r.clipboard),
                ))
                .child(when(move || connected() || busy(), || ghost_button("断开", controller::remote_disconnect)))
                .child(link_button("退出远程桌面", controller::remote_exit)),
        )
        .child(when(
            move || remote.with(|r| r.status.as_ref().is_some_and(|s| !s.missing_permissions().is_empty())),
            move || {
                view()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .px_5()
                    .py_1p5()
                    .bg(PALETTE.approval)
                    .child(dot(PALETTE.warning))
                    .child(
                        text(move || {
                            remote.with(|r| {
                                let missing = r.status.as_ref().map(|s| s.missing_permissions().join("、")).unwrap_or_default();
                                format!("共享端缺少权限：{missing}。在系统设置 › 隐私与安全性中授权后重新连接。")
                            })
                        })
                        .flex_1()
                        .text_size(px(12.0))
                        .text_color(PALETTE.ink),
                    )
                    .child(link_button("重新检查", controller::refresh_rd_status))
            },
        ))
        .child(
            view()
                .flex_1()
                .items_center()
                .justify_center()
                .p_6()
                .child(dynamic(move || {
                    if connected() {
                        canvas_view(remote).into_element()
                    } else {
                        peer_picker(peers, busy).into_element()
                    }
                })),
        )
}

/// The live remote desktop canvas: renders incoming frames via NSH [`image`]
/// and forwards normalized pointer, wheel and keyboard events when `control`
/// is enabled.
fn canvas_view(remote: Memo<crate::state::ui::RemoteDesktopState>) -> impl IntoElement {
    let normalize = |x: f32, y: f32| -> (f64, f64) {
        let (w, h) = controller::ui().with_untracked(|u| u.remote.canvas_size);
        let w = if w > 1 { w as f64 } else { 960.0 };
        let h = if h > 1 { h as f64 } else { 600.0 };
        ((x as f64 / w).clamp(0.0, 1.0), (y as f64 / h).clamp(0.0, 1.0))
    };
    view()
        .test_id(CANVAS_ID)
        .w_full()
        .max_w(px(960.0))
        .aspect_ratio(1.6)
        .items_center()
        .justify_center()
        .overflow_hidden()
        .rounded(px(10.0))
        .bg(AdaptiveColor::rgb(0x0F1012, 0x0B0C0E))
        .border_1()
        .border_color(move || if remote.with(|r| r.control) { PALETTE.accent } else { PALETTE.line })
        .on_layout(|bounds: &Bounds<Points>| {
            let w = bounds.size.width.0.max(0.0).round() as u32;
            let h = bounds.size.height.0.max(0.0).round() as u32;
            controller::remote_update(move |r| r.canvas_size = (w, h));
        })
        .on_mouse_move(move |e: &MouseEvent| {
            let (x, y) = normalize(e.position.x.0, e.position.y.0);
            controller::remote_send_input(serde_json::json!({
                "kind": "mouse_move",
                "x": x,
                "y": y,
                "shift": e.modifiers.shift,
                "ctrl": e.modifiers.ctrl,
                "alt": e.modifiers.alt,
                "meta": e.modifiers.cmd,
            }));
        })
        .on_press_down(move |e: &PressEvent| {
            let (x, y) = normalize(e.position.x.0, e.position.y.0);
            controller::remote_send_input(serde_json::json!({
                "kind": "mouse_down",
                "x": x,
                "y": y,
                "button": "left",
                "shift": e.modifiers.shift,
                "ctrl": e.modifiers.ctrl,
                "alt": e.modifiers.alt,
                "meta": e.modifiers.cmd,
            }));
        })
        .on_press_up(move |e: &PressEvent| {
            let (x, y) = normalize(e.position.x.0, e.position.y.0);
            controller::remote_send_input(serde_json::json!({
                "kind": "mouse_up",
                "x": x,
                "y": y,
                "button": "left",
                "shift": e.modifiers.shift,
                "ctrl": e.modifiers.ctrl,
                "alt": e.modifiers.alt,
                "meta": e.modifiers.cmd,
            }));
        })
        .on_wheel(move |e: &WheelEvent| {
            let (x, y) = normalize(e.position.x.0, e.position.y.0);
            let (dx, dy) = match e.delta {
                WheelDelta::Points(p) => (p.x.0 as f64, p.y.0 as f64),
                WheelDelta::Lines(p) => (p.x as f64 * 24.0, p.y as f64 * 24.0),
            };
            controller::remote_send_input(serde_json::json!({
                "kind": "wheel",
                "x": x,
                "y": y,
                "dx": dx,
                "dy": dy,
                "shift": e.modifiers.shift,
                "ctrl": e.modifiers.ctrl,
                "alt": e.modifiers.alt,
                "meta": e.modifiers.cmd,
            }));
        })
        .on_key_down(|e: &KeyEvent| {
            controller::remote_send_input(serde_json::json!({
                "kind": "key_down",
                "key": e.key.as_ref(),
                "code": e.code.as_ref(),
                "shift": e.modifiers.shift,
                "ctrl": e.modifiers.ctrl,
                "alt": e.modifiers.alt,
                "meta": e.modifiers.cmd,
            }));
        })
        .on_key_up(|e: &KeyEvent| {
            controller::remote_send_input(serde_json::json!({
                "kind": "key_up",
                "key": e.key.as_ref(),
                "code": e.code.as_ref(),
                "shift": e.modifiers.shift,
                "ctrl": e.modifiers.ctrl,
                "alt": e.modifiers.alt,
                "meta": e.modifiers.cmd,
            }));
        })
        .child(dynamic(move || {
            let uri = remote.with(|r| r.frame_uri.clone());
            if let Some(src) = uri {
                image(src)
                    .w_full()
                    .h_full()
                    .into_element()
            } else {
                text(move || {
                    remote.with(|r| {
                        format!(
                            "{} · {} · {} · {} · 等待首帧…{}",
                            r.peer().unwrap_or("远程主机"),
                            r.resolution.label(),
                            r.quality.label(),
                            if r.fit { "适应窗口" } else { "1:1" },
                            if r.control { " · 键鼠已接管" } else { "" }
                        )
                    })
                })
                .text_size(px(13.0))
                .text_color(AdaptiveColor::rgb(0xA9A6A1, 0xA9A6A1))
                .into_element()
            }
        }))
}

fn peer_picker(peers: Memo<Vec<(String, String)>>, busy: impl Fn() -> bool + Copy + 'static) -> impl IntoElement {
    view()
        .w_full()
        .max_w(px(420.0))
        .flex_col()
        .gap_2()
        .p_5()
        .rounded(px(14.0))
        .bg(PALETTE.card)
        .border_1()
        .border_color(PALETTE.line)
        .child(
            text("连接远程桌面")
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(px(16.0))
                .text_color(PALETTE.ink),
        )
        .child(caption("选择一台已配对的主机"))
        .child(when(
            move || controller::ui().with(|u| matches!(u.remote.link, RemoteLink::Failed { .. })),
            || {
                view()
                    .flex_col()
                    .gap_1()
                    .p_2()
                    .rounded(px(8.0))
                    .bg(PALETTE.approval)
                    .child(
                        text(|| controller::ui().with(|u| u.remote.status_label()))
                            .select_text()
                            .text_size(px(12.0))
                            .text_color(PALETTE.danger),
                    )
                    .child(link_button("重试", || {
                        let peer = controller::ui().with_untracked(|u| match &u.remote.link {
                            RemoteLink::Failed { peer, .. } => Some(peer.clone()),
                            _ => None,
                        });
                        if let Some(peer) = peer {
                            controller::remote_connect(peer);
                        }
                    }))
            },
        ))
        .child(when(
            move || peers.with(Vec::is_empty),
            || {
                text("还没有已配对的主机。在 设置 › 远端互联与配对 中兑换配对码即可添加。")
                    .text_size(px(12.0))
                    .text_color(PALETTE.faint)
            },
        ))
        .child(
            view()
                .flex_col()
                .gap_1p5()
                .pt_2()
                .border_t_1()
                .border_t_color(PALETTE.line_soft)
                .child(caption("局域网直连（vantry-lan://）"))
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(
                            nsh_base::input()
                                .value(|| controller::ui().with(|u| u.remote.direct_link.clone()))
                                .on_change(|t: &str| {
                                    let val = t.to_owned();
                                    controller::remote_update(move |r| r.direct_link = val);
                                })
                                .placeholder("粘贴 vantry-lan:// 连接地址")
                                .placeholder_color(move || color(PALETTE.faint))
                                .flex_1()
                                .text_size(px(12.0))
                                .text_color(PALETTE.ink),
                        )
                        .child(
                            primary_button("直连", || {
                                let target = controller::ui()
                                    .with_untracked(|u| u.remote.direct_link.trim().to_owned());
                                if !target.is_empty() {
                                    controller::remote_connect(target);
                                }
                            })
                            .disabled(busy),
                        ),
                ),
        )
        .child(
            view()
                .flex_col()
                .gap_1p5()
                .pt_2()
                .border_t_1()
                .border_t_color(PALETTE.line_soft)
                .child(
                    view()
                        .flex_row()
                        .items_center()
                        .gap_2()
                        .child(caption("本机共享"))
                        .child(view().flex_1())
                        .child(ghost_button(
                            || {
                                if controller::ui().with(|u| u.remote.host_lan_link.is_empty()) {
                                    "允许此电脑被控制"
                                } else {
                                    "停止共享"
                                }
                            },
                            controller::remote_host_toggle,
                        )),
                )
                .child(when(
                    || controller::ui().with(|u| !u.remote.host_lan_link.is_empty()),
                    || {
                        text(|| controller::ui().with(|u| u.remote.host_lan_link.clone()))
                            .select_text()
                            .text_size(px(11.0))
                            .text_color(PALETTE.muted)
                    },
                )),
        )
        .child(each(
            move || peers.get(),
            |p: &(String, String)| p.0.clone(),
            move |peer, _| {
                let id = move || peer.with_untracked(|p| p.0.clone());
                view()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .child(
                        text(move || peer.with(|p| format!("{}（{}）", p.1, p.0)))
                            .flex_1()
                            .text_size(px(13.0))
                            .text_color(PALETTE.ink),
                    )
                    .child(
                        primary_button(move || if busy() { "连接中…" } else { "连接" }, move || {
                            controller::remote_connect(id())
                        })
                        .disabled(busy),
                    )
            },
        ))
}
