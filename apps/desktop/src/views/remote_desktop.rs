//! The remote desktop view's shell: the connection state, a toolbar
//! (peer, connect / disconnect, resolution, fit, input control, clipboard),
//! and the canvas container where Wave 3's media stream will draw frames.
//! Until then the demo shows a placeholder picture and a real peer reports
//! that the stream is not wired.

use nsh_core::prelude::*;

use super::{caption, dot, ghost_button, link_button, pill, primary_button};
use crate::{
    controller,
    state::ui::{Catalogs, MainView, RemoteLink, Resolution},
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
                .child(view().flex_1())
                .children(Resolution::ALL.map(|res| {
                    pill(
                        res.label(),
                        move || remote.with(|r| r.resolution == res),
                        move || controller::remote_update(move |r| r.resolution = res),
                    )
                }))
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
                .child(when(connected, || ghost_button("断开", controller::remote_disconnect)))
                .child(link_button("返回对话", || controller::show_view(MainView::Conversation))),
        )
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

/// The canvas container: a 16:10 frame that the media stream fills.
fn canvas_view(remote: Memo<crate::state::ui::RemoteDesktopState>) -> impl IntoElement {
    view()
        .test_id(CANVAS_ID)
        .w_full()
        .max_w(px(960.0))
        .aspect_ratio(1.6)
        .items_center()
        .justify_center()
        .rounded(px(10.0))
        .bg(AdaptiveColor::rgb(0x0F1012, 0x0B0C0E))
        .border_1()
        .border_color(move || if remote.with(|r| r.control) { PALETTE.accent } else { PALETTE.line })
        .child(
            text(move || {
                remote.with(|r| {
                    format!(
                        "演示画面 · {} · {}{}",
                        r.resolution.label(),
                        if r.fit { "适应窗口" } else { "1:1" },
                        if r.control { " · 键鼠已接管" } else { "" }
                    )
                })
            })
            .text_size(px(13.0))
            .text_color(AdaptiveColor::rgb(0xA9A6A1, 0xA9A6A1)),
        )
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
            move || peers.with(Vec::is_empty),
            || {
                text("还没有已配对的主机。可在 设置 › 远端与配对 中查看。")
                    .text_size(px(12.0))
                    .text_color(PALETTE.faint)
            },
        ))
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
