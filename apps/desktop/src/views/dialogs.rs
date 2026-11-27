//! Small dialogs: renaming a thread, and confirming a delete or a rewind.

use nsh_core::prelude::*;

use super::{ghost_button, primary_button};
use crate::{
    controller,
    state::ui::Confirm,
    theme::{metrics, palette},
};

fn frame(body: impl IntoElement, on_cancel: impl Fn() + 'static) -> impl IntoElement {
    let colors = palette();
    nsh_base::dialog()
        .animation(ModalAnimation::Fade)
        .on_cancel(on_cancel)
        .child(
            view()
                .flex_1()
                .items_center()
                .justify_center()
                .bg(colors.scrim)
                .p_6()
                .child(
                    view()
                        .w_full()
                        .max_w(px(metrics::MODAL_WIDTH - 80.0))
                        .flex_col()
                        .gap_3()
                        .p_5()
                        .rounded(px(14.0))
                        .bg(colors.card)
                        .border_1()
                        .border_color(colors.line)
                        .shadow_xl()
                        .child(body),
                ),
        )
}

pub(crate) fn rename_dialog() -> impl IntoElement {
    let colors = palette();
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
                            .text_color(colors.ink),
                    )
                    .child(
                        view()
                            .px_3()
                            .py_2()
                            .rounded(px(8.0))
                            .border_1()
                            .border_color(colors.line)
                            .child(
                                nsh_base::input()
                                    .value(move || value.get())
                                    .on_change(|t: &str| controller::set_rename_text(t))
                                    .on_submit(|_: &str| controller::finish_rename(true))
                                    .auto_focus(true)
                                    .text_size(px(13.5))
                                    .text_color(colors.ink),
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
    let colors = palette();
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
                            .text_color(colors.ink),
                    )
                    .child(text(detail).text_size(px(13.0)).text_color(colors.muted))
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

pub(crate) fn attachment_preview_dialog() -> impl IntoElement {
    let colors = palette();
    let active = computed(|| controller::ui().with(|u| u.attachment_preview.clone()));
    dynamic(move || {
        active.get().map(|(item, label)| {
            let lang = controller::ui().with(|u| u.locale);
            let preview = signal(None::<String>);
            let source = controller::state().with_untracked(|s| {
                s.drafts
                    .get(&s.draft_key())
                    .and_then(|d| d.image_paths.get(&item.id))
                    .cloned()
            });
            let modal_title = if label.is_empty() || label == item.name {
                item.name.clone()
            } else {
                format!("{label} · {}", item.name)
            };
            let item_name = item.name.clone();
            let item_for_load = item.clone();
            spawn(async move {
                preview.set(controller::attachment_preview(item_for_load, source).await);
            });
            nsh_base::dialog()
                .animation(ModalAnimation::Fade)
                .on_cancel(controller::close_attachment_preview)
                .child(
                    view()
                        .flex_1()
                        .items_center()
                        .justify_center()
                        .bg(colors.scrim)
                        .p_6()
                        .on_press(controller::close_attachment_preview)
                        .child(
                            view()
                                .flex_col()
                                .p_3()
                                .gap_2()
                                .rounded(px(14.0))
                                .bg(colors.raised)
                                .border_1()
                                .border_color(colors.line)
                                .shadow_xl()
                                .on_press(|| {})
                                .child(
                                    view()
                                        .flex_row()
                                        .items_center()
                                        .gap_2()
                                        .px_1()
                                        .child(
                                            text(modal_title)
                                                .flex_1()
                                                .number_of_lines(1u32)
                                                .text_size(px(12.5))
                                                .font_weight(FontWeight::MEDIUM)
                                                .text_color(colors.ink),
                                        )
                                        .child(when(
                                            move || preview.get().is_some(),
                                            move || {
                                                let name = item_name.clone();
                                                super::link_button(lang.label("复制图片"), move || {
                                                    if let Some(src) = preview.get_untracked() {
                                                        controller::copy_image_preview(src, name.clone());
                                                    }
                                                })
                                            },
                                        ))
                                        .child(super::icon_button(
                                            crate::icons::Icon::X,
                                            lang.label("关闭"),
                                            controller::close_attachment_preview,
                                        )),
                                )
                                .child(dynamic(move || match preview.get() {
                                    Some(src) => image(src)
                                        .w(move || px(crate::window_size().width.0 * 0.72))
                                        .h(move || px(crate::window_size().height.0 * 0.66))
                                        .object_fit(nsh_core::ResizeMode::Contain)
                                        .into_element(),
                                    None => super::caption(lang.label("加载中…")).into_element(),
                                })),
                        ),
                )
                .into_element()
        })
    })
}
