//! Small dialogs: renaming a thread, and confirming a delete or a rewind.

use nsh_core::prelude::*;

use super::{ghost_button, primary_button};
use crate::{
    controller,
    state::ui::Confirm,
    theme::{PALETTE, metrics},
};

fn frame(body: impl IntoElement, on_cancel: impl Fn() + 'static) -> impl IntoElement {
    nsh_base::dialog()
        .animation(ModalAnimation::Fade)
        .on_cancel(on_cancel)
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
                        .max_w(px(metrics::MODAL_WIDTH - 80.0))
                        .flex_col()
                        .gap_3()
                        .p_5()
                        .rounded(px(14.0))
                        .bg(PALETTE.card)
                        .border_1()
                        .border_color(PALETTE.line)
                        .shadow_xl()
                        .child(body),
                ),
        )
}

pub(crate) fn rename_dialog() -> impl IntoElement {
    let open = computed(|| controller::ui().with(|u| u.rename.is_some()));
    dynamic(move || {
        open.get().then(|| {
            let value = computed(|| {
                controller::ui().with(|u| u.rename.as_ref().map(|r| r.text.clone()).unwrap_or_default())
            });
            frame(
                view()
                    .flex_col()
                    .gap_3()
                    .child(
                        text("重命名会话")
                            .font_weight(FontWeight::SEMIBOLD)
                            .text_size(px(15.0))
                            .text_color(PALETTE.ink),
                    )
                    .child(
                        view()
                            .px_3()
                            .py_2()
                            .rounded(px(8.0))
                            .border_1()
                            .border_color(PALETTE.line)
                            .child(
                                nsh_base::input()
                                    .value(move || value.get())
                                    .on_change(|t: &str| controller::set_rename_text(t))
                                    .on_submit(|_: &str| controller::finish_rename(true))
                                    .auto_focus(true)
                                    .text_size(px(13.5))
                                    .text_color(PALETTE.ink),
                            ),
                    )
                    .child(
                        view()
                            .flex_row()
                            .gap_2()
                            .child(view().flex_1())
                            .child(ghost_button("取消", || controller::finish_rename(false)))
                            .child(
                                primary_button("保存", || controller::finish_rename(true))
                                    .disabled(move || value.with(|v| v.trim().is_empty())),
                            ),
                    ),
                || controller::finish_rename(false),
            )
            .into_element()
        })
    })
}

pub(crate) fn confirm_dialog() -> impl IntoElement {
    let asked = computed(|| controller::ui().with(|u| u.confirm.clone()));
    dynamic(move || {
        asked.get().map(|confirm| {
            let (headline, detail, action) = match confirm {
                Confirm::Delete { title, .. } => (
                    "删除会话？".to_owned(),
                    format!("「{title}」及其历史将从 Core 删除，无法恢复。"),
                    "删除",
                ),
                Confirm::Rewind { .. } => (
                    "回退到这一轮之前？".to_owned(),
                    "这一轮及之后的回复会从会话中移除；工作区文件不会改动。".to_owned(),
                    "回退",
                ),
            };
            frame(
                view()
                    .flex_col()
                    .gap_3()
                    .child(
                        text(headline)
                            .font_weight(FontWeight::SEMIBOLD)
                            .text_size(px(15.0))
                            .text_color(PALETTE.ink),
                    )
                    .child(text(detail).text_size(px(13.0)).text_color(PALETTE.muted))
                    .child(
                        view()
                            .flex_row()
                            .gap_2()
                            .child(view().flex_1())
                            .child(ghost_button("取消", || controller::confirm(false)))
                            .child(primary_button(action, || controller::confirm(true))),
                    ),
                || controller::confirm(false),
            )
            .into_element()
        })
    })
}
