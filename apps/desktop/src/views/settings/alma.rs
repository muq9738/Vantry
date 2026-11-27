use super::*;
use crate::{
    controller::alma,
    state::ui::{AlmaPairingForm, Secret},
};

pub(super) fn qr(link: &str, width: i32) -> Option<impl IntoElement> {
    use qrcodegen::{QrCode, QrCodeEcc};
    let qr = QrCode::encode_text(link, QrCodeEcc::Medium)
        .or_else(|_| QrCode::encode_text(link, QrCodeEcc::Low))
        .ok()?;
    let scale = (width / (qr.size() + 8)).max(1) as f32;
    let size = (qr.size() + 8) as f32 * scale;
    let mut painter = Painter::new()
        .fill_color(white())
        .fill_rect(0., 0., size, size)
        .fill_color(black());
    for y in 0..qr.size() {
        let mut x = 0;
        while x < qr.size() {
            if !qr.get_module(x, y) {
                x += 1;
                continue;
            }
            let start = x;
            while x < qr.size() && qr.get_module(x, y) {
                x += 1;
            }
            painter = painter.fill_rect(
                (start + 4) as f32 * scale,
                (y + 4) as f32 * scale,
                (x - start) as f32 * scale,
                scale,
            );
        }
    }
    Some(canvas(painter).w(px(size)).h(px(size)).flex_shrink_0())
}

pub(super) fn settings_entry() -> impl IntoElement {
    let selected = computed(|| {
        controller::state().with(|s| {
            s.selected_thread()
                .filter(|t| !t.archived)
                .map(|t| (t.id.clone(), t.title.clone()))
        })
    });
    card().child(title("Alma iOS")).child(
        row()
            .child(
                hint(move || {
                    selected
                        .get()
                        .map(|(_, title)| format!("当前会话 · {title}"))
                        .unwrap_or_else(|| "先在侧栏选择一条本机会话".into())
                })
                .flex_1(),
            )
            .child(ghost_button(
                move || {
                    if selected.get().is_some() {
                        "手机配对…"
                    } else {
                        "查看手机共享…"
                    }
                },
                move || {
                    let (id, title) = selected.get().unwrap_or_default();
                    alma::open(id, title);
                },
            )),
    )
}

fn read<T: Default>(f: impl FnOnce(&AlmaPairingForm) -> T) -> T {
    controller::ui().with(|u| u.forms.alma.as_ref().map(f).unwrap_or_default())
}

fn pairing_busy() -> bool {
    read(|f| f.busy)
}

fn relay_fields() -> impl IntoElement {
    view().flex_col().gap_2()
        .child(field(|| read(|f| f.relay.clone()), |v| alma::edit(|f| f.relay = v.into()), "alma-relay 的 HTTPS 地址"))
        .child(secret_field(|| read(|f| f.key.expose().to_owned()), |v| alma::edit(|f| f.key = Secret::new(v)), "注册凭据（中继要求时填写）"))
}

pub(crate) fn dialog() -> impl IntoElement {
    when(
        || controller::ui().with(|u| u.forms.alma.is_some()),
        || {
            let active = computed(|| read(|f| f.status.as_ref().is_some_and(|s| s.enabled)));
            let loaded = computed(|| read(|f| f.status.is_some()));
            let own = computed(|| {
                read(|f| {
                    f.status
                        .as_ref()
                        .is_some_and(|s| s.thread_id == f.thread_id)
                })
            });
            let advanced = computed(|| read(|f| f.advanced));
            let link = computed(|| {
                read(|f| {
                    if f.status.as_ref().is_some_and(|s| s.ready) {
                        f.link.clone()
                    } else {
                        Secret::default()
                    }
                })
            });
            let problem = computed(|| {
                read(|f| {
                    if f.error.is_empty() {
                        f.status
                            .as_ref()
                            .map(|s| s.error.clone())
                            .unwrap_or_default()
                    } else {
                        f.error.clone()
                    }
                })
            });
            let confirm = computed(|| read(|f| f.confirm_stop));
            let panel = card()
                .w_full()
                .max_w(px(460.))
                .p_5()
                .gap_3()
                .child(
                    row()
                        .child(
                            text("Alma iOS 配对")
                                .text_color(palette().ink)
                                .font_weight(FontWeight::SEMIBOLD)
                                .flex_1(),
                        )
                        .child(crate::views::icon_button(
                            crate::icons::Icon::X,
                            "关闭",
                            alma::close,
                        )),
                )
                .child(text(|| read(|f| f.title.clone())).text_color(palette().ink).text_size(px(13.)))
                .child(when(
                    move || loaded.get() && !active.get() && read(|f| !f.thread_id.is_empty()),
                    move || {
                        view()
                            .flex_col()
                            .gap_2()
                            .child(hint("同一 Wi-Fi 可直接扫码。在外网连接请填写 alma-relay 的地址。"))
                            .child(link_button(
                                move || {
                                    if advanced.get() {
                                        "收起中继设置"
                                    } else {
                                        "使用跨网中继…"
                                    }
                                },
                                || alma::edit(|f| f.advanced = !f.advanced),
                            ))
                            .child(when(
                                move || advanced.get(),
                                relay_fields,
                            ))
                            .child(
                                primary_button("生成配对码", || {
                                    alma::request("remote.pair.alma.create")
                                })
                                .disabled(move || {
                                    pairing_busy()
                                        || (advanced.get() && read(|f| f.relay.trim().is_empty()))
                                }),
                            )
                    },
                ))
                .child(when(
                    move || loaded.get() && !active.get() && read(|f| f.thread_id.is_empty()),
                    || hint("尚未共享会话。先在侧栏选择一条本机会话，再生成配对码。"),
                ))
                .child(when(
                    move || active.get(),
                    move || {
                        view()
                            .flex_col()
                            .gap_2()
                            .child(hint(move || {
                                read(|f| {
                                    let Some(s) = &f.status else {
                                        return String::new();
                                    };
                                    if !own.get() {
                                        return format!(
                                            "正在共享「{}」。停止后可改为共享当前会话。",
                                            if s.title.is_empty() {
                                                &s.thread_id
                                            } else {
                                                &s.title
                                            }
                                        );
                                    }
                                    if s.relay_rejected {
                                        return "中继拒绝注册，请核对中继地址和注册凭据。".into();
                                    }
                                    if s.phones > 0 {
                                        return format!("已连接 {} 个手机会话", s.phones);
                                    }
                                    if !s.ready {
                                        return "等待共享服务就绪".into();
                                    }
                                    if s.relay.is_empty() {
                                        "等待手机扫码 · 同一 Wi-Fi".into()
                                    } else {
                                        "中继已就绪，等待手机扫码".into()
                                    }
                                })
                            }))
                            .child(dynamic(move || {
                                let code = link.get();
                                if code.is_empty() {
                                    return view().into_element();
                                }
                                let mut area = view().flex_col().items_center().gap_2();
                                if let Some(image) = qr(code.expose(), 256) {
                                    area = area.child(image);
                                }
                                area.child(link_button("复制配对链接", move || {
                                    controller::copy(code.expose().to_owned())
                                }))
                                .into_element()
                            }))
                            .child(when(
                                move || {
                                    own.get()
                                        && read(|f| {
                                            f.status.as_ref().is_some_and(|s| !s.error.is_empty())
                                        })
                                },
                                || {
                                    ghost_button("重新开启共享", || {
                                        alma::request("remote.pair.alma.create")
                                    })
                                    .disabled(pairing_busy)
                                },
                            ))
                            .child(when(move || own.get() && !confirm.get(), move || {
                                view().flex_col().gap_2()
                                    .child(link_button(move || if advanced.get() { "取消更改" } else { "重新配对…" }, || alma::edit(|f| {
                                        f.advanced = !f.advanced;
                                        f.relay = f.status.as_ref().map(|s| s.relay.clone()).unwrap_or_default();
                                        f.key.clear();
                                    })).disabled(pairing_busy))
                                    .child(when(move || advanced.get(), || view().flex_col().gap_2()
                                        .child(relay_fields())
                                        .child(hint("公网中继连接成功后，旧码失效。在 Alma 打开新链接即可。"))
                                        .child(primary_button("重新配对并生成新码", || alma::request("remote.pair.alma.replace"))
                                            .disabled(|| pairing_busy() || read(|f| f.relay.trim().is_empty())))))
                            }))
                            .child(when(
                                move || !confirm.get(),
                                || {
                                    ghost_button("停止共享…", || {
                                        alma::edit(|f| f.confirm_stop = true)
                                    })
                                },
                            ))
                            .child(when(
                                move || confirm.get(),
                                || {
                                    row()
                                        .child(hint("手机将断开，旧码随即失效。").flex_1())
                                        .child(link_button("取消", || {
                                            alma::edit(|f| f.confirm_stop = false)
                                        }))
                                        .child(
                                            ghost_button("停止共享", || {
                                                alma::request("remote.pair.alma.revoke")
                                            })
                                            .disabled(pairing_busy),
                                        )
                                },
                            ))
                    },
                ))
                .child(when(
                    move || !problem.get().is_empty(),
                    move || hint(move || problem.get()).text_color(palette().danger),
                ))
                .child(when(
                    move || active.get() || !loaded.get(),
                    move || {
                        link_button(
                            move || {
                                if pairing_busy() {
                                    "正在读取…"
                                } else {
                                    "刷新连接状态"
                                }
                            },
                            || alma::request("remote.pair.alma.show"),
                        )
                        .disabled(pairing_busy)
                    },
                ));
            nsh_base::dialog().on_cancel(alma::close).child(
                view()
                    .flex_1()
                    .items_center()
                    .justify_center()
                    .bg(palette().scrim)
                    .p_3()
                    .child(panel),
            )
        },
    )
}
