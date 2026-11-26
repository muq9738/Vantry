//! Software Update modal dialog and shared progress bar:
//! - Version jump header (`0.2.13 → 0.2.16`) with multi-step incremental delta badge
//! - Scrollable bullet release notes across all skipped versions
//! - Live progress bar during download / incremental patching / signature verification
//! - Green completion banner ("下载完成！准备安装。") matching the desktop update flow
//! - Footer actions: "跳过此版本", "稍后", and "立即更新" / "立即重启"

use nsh_core::prelude::*;
use vantry_protocol::AppUpdateInfo;

use super::ghost_button;
use crate::{
    bridge::workbench::actions::names,
    controller,
    icons::{Icon, icon},
    theme::palette,
};

/// Sky-blue primary CTA color matching the update dialog's action pill.
const UPDATE_CTA_BG: AdaptiveColor = AdaptiveColor::rgb(0x2563EB, 0x60A5FA);
const UPDATE_CTA_INK: AdaptiveColor = AdaptiveColor::rgb(0xFFFFFF, 0x0B1220);
const SUCCESS_BORDER: AdaptiveColor = AdaptiveColor::rgb(0xBBF7D0, 0x244B34);
const DANGER_BG: AdaptiveColor = AdaptiveColor::rgb(0xFEF2F2, 0x341B1E);
const DANGER_BORDER: AdaptiveColor = AdaptiveColor::rgb(0xFECACA, 0x5C282D);

/// True when `current` is at least `target` (numeric dot-separated prefix,
/// stripping any leading `v` and build metadata).
pub(crate) fn version_at_least(current: &str, target: &str) -> bool {
    fn parse(v: &str) -> Option<(Vec<u64>, bool)> {
        let v = v.trim().trim_start_matches(['v', 'V']);
        let v = v.split('+').next().unwrap_or(v);
        let (core, pre) = match v.split_once('-') {
            Some((c, p)) => (c, !p.is_empty()),
            None => (v, false),
        };
        let parts: Vec<u64> = core
            .split('.')
            .map(|p| p.parse::<u64>().ok())
            .collect::<Option<Vec<_>>>()?;
        (!parts.is_empty()).then_some((parts, pre))
    }
    let (Some((a, a_pre)), Some((b, b_pre))) = (parse(current), parse(target)) else {
        return current.trim().trim_start_matches(['v', 'V'])
            == target.trim().trim_start_matches(['v', 'V']);
    };
    let len = a.len().max(b.len());
    for i in 0..len {
        let av = a.get(i).copied().unwrap_or(0);
        let bv = b.get(i).copied().unwrap_or(0);
        if av != bv {
            return av > bv;
        }
    }
    !a_pre || b_pre
}

/// True when `update` represents a newer version than the running desktop binary.
pub(crate) fn has_newer_update(update: &AppUpdateInfo) -> bool {
    if !update.available || update.latest_version.trim().is_empty() {
        return false;
    }
    !version_at_least(env!("CARGO_PKG_VERSION"), &update.latest_version)
}

/// Human-readable MB string (e.g. `3.2 MB`).
pub(crate) fn format_mb(bytes: i64) -> String {
    let mb = (bytes.max(0) as f64) / 1_000_000.0;
    if mb >= 100.0 {
        format!("{mb:.0} MB")
    } else if mb >= 10.0 {
        format!("{mb:.1} MB")
    } else {
        format!("{mb:.1} MB")
    }
}

/// Summary badge text describing incremental (single or multi-step) vs full package size.
pub(crate) fn update_size_badge(update: &AppUpdateInfo) -> String {
    if update.delta_available && (!update.staged || update.used_delta) {
        let saved = update.asset_size.saturating_sub(update.delta_size);
        let prefix = if update.delta_steps > 1 {
            format!("增量 {} 步 · {}", update.delta_steps, format_mb(update.delta_size))
        } else {
            format!("增量 {}", format_mb(update.delta_size))
        };
        if saved > 0 {
            format!("{prefix}，节省 {}", format_mb(saved))
        } else {
            prefix
        }
    } else if update.asset_size > 0 {
        format!("完整包 {}", format_mb(update.asset_size))
    } else {
        String::new()
    }
}

/// Computes `(fraction 0.0..=1.0, stage_label, detail_label)` for an in-progress update.
pub(crate) fn progress_metrics(update: &AppUpdateInfo) -> (f32, String, String) {
    let phase = update.phase.as_str();
    let total = update.total_bytes.max(0);
    let done = update.downloaded_bytes.clamp(0, total.max(update.downloaded_bytes));
    match phase {
        "applying_delta" => {
            let total_steps = update.delta_steps.max(1);
            let step = update.delta_step.clamp(1, total_steps);
            let frac = 0.85 + 0.10 * ((step as f32) / (total_steps as f32));
            let label = if total_steps > 1 {
                format!("正在应用增量补丁 ({step}/{total_steps})…")
            } else {
                "正在应用增量补丁…".to_owned()
            };
            (frac.clamp(0.05, 0.96), label, format!("{:.0}%", frac * 100.0))
        }
        "verifying" => (
            0.97,
            "正在校验签名与文件完整性…".to_owned(),
            "97%".to_owned(),
        ),
        "ready" => (
            1.0,
            "下载完成！准备安装。".to_owned(),
            "100%".to_owned(),
        ),
        _ => {
            // Downloading (single or multi-step delta, or full archive).
            let raw_frac = if total > 0 {
                (done as f32 / total as f32).clamp(0.0, 1.0)
            } else {
                0.08
            };
            let display_frac = (raw_frac * 0.85).max(if done > 0 { 0.03 } else { 0.02 });
            let label = if update.delta_steps > 1 {
                let step = update.delta_step.clamp(1, update.delta_steps);
                format!("正在下载增量更新 ({step}/{})…", update.delta_steps)
            } else if update.delta_available {
                "正在下载增量更新…".to_owned()
            } else {
                "正在下载更新…".to_owned()
            };
            let detail = if total > 0 {
                format!(
                    "{:.0}% · {} / {}",
                    raw_frac * 100.0,
                    format_mb(done),
                    format_mb(total)
                )
            } else if done > 0 {
                format_mb(done)
            } else {
                "准备中…".to_owned()
            };
            (display_frac, label, detail)
        }
    }
}

/// Reusable progress card used in both the Update Modal and Settings -> About & Updates.
pub(crate) fn progress_banner(update: AppUpdateInfo) -> View {
    let colors = palette();
    let (share, label, detail) = progress_metrics(&update);
    let share = share.clamp(0.02, 1.0);
    view()
        .w_full()
        .flex_col()
        .gap_2()
        .px_3p5()
        .py_2p5()
        .rounded(px(10.0))
        .bg(colors.soft)
        .border_1()
        .border_color(colors.line)
        .child(
            view()
                .flex_row()
                .items_center()
                .gap_2()
                .child(
                    text(label)
                        .flex_1()
                        .text_size(px(12.5))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.ink),
                )
                .child(
                    text(detail)
                        .tabular_nums()
                        .text_size(px(11.5))
                        .text_color(colors.muted),
                ),
        )
        .child(
            view()
                .w_full()
                .h(px(6.0))
                .flex_row()
                .rounded_full()
                .overflow_hidden()
                .bg(colors.line)
                .child(
                    view()
                        .flex_grow(share)
                        .h_full()
                        .rounded_full()
                        .bg(UPDATE_CTA_BG),
                )
                .child(view().flex_grow((1.0 - share).max(0.0))),
        )
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum NoteLine {
    Heading(String),
    Bullet(String),
    Paragraph(String),
}

fn parse_note_lines(notes: &str) -> Vec<NoteLine> {
    let mut out = Vec::new();
    for raw in notes.lines() {
        let line = raw.trim();
        if line.is_empty() {
            continue;
        }
        if let Some(rest) = line.strip_prefix("### ") {
            out.push(NoteLine::Heading(rest.trim().to_owned()));
        } else if let Some(rest) = line.strip_prefix("## ") {
            out.push(NoteLine::Heading(rest.trim().to_owned()));
        } else if let Some(rest) = line.strip_prefix("# ") {
            out.push(NoteLine::Heading(rest.trim().to_owned()));
        } else if let Some(rest) = line
            .strip_prefix("- ")
            .or_else(|| line.strip_prefix("* "))
            .or_else(|| line.strip_prefix("• "))
        {
            let text = rest.trim();
            if !text.is_empty() {
                out.push(NoteLine::Bullet(text.to_owned()));
            }
        } else {
            out.push(NoteLine::Paragraph(line.to_owned()));
        }
    }
    out
}

fn notes_list(notes: &str) -> View {
    let colors = palette();
    let lines = parse_note_lines(notes);
    let mut list = view().flex_col().gap_2();
    for entry in lines {
        match entry {
            NoteLine::Heading(title) => {
                list = list.child(
                    text(title)
                        .pt_1()
                        .text_size(px(12.0))
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(colors.muted),
                );
            }
            NoteLine::Bullet(item) => {
                list = list.child(
                    view()
                        .flex_row()
                        .items_start()
                        .gap_2()
                        .child(
                            text("•")
                                .text_size(px(13.0))
                                .text_color(colors.muted),
                        )
                        .child(
                            text(item)
                                .flex_1()
                                .text_size(px(13.0))
                                .text_color(colors.ink),
                        ),
                );
            }
            NoteLine::Paragraph(p) => {
                list = list.child(
                    text(p)
                        .text_size(px(13.0))
                        .text_color(colors.ink),
                );
            }
        }
    }
    list
}

/// The Software Update modal dialog ("软件更新").
pub(crate) fn update_dialog() -> impl IntoElement {
    let colors = palette();
    let open = computed(|| controller::ui().with(|u| u.update_dialog));
    let update = computed(|| controller::ui().with(|u| u.active_update().cloned()));
    let busy = computed(|| controller::ui().with(|u| u.op_busy("update")));
    let error = computed(|| controller::ui().with(|u| u.update_error.clone()));

    dynamic(move || {
        if !open.get() {
            return None;
        }
        let info = update.get()?;
        let current = env!("CARGO_PKG_VERSION").to_owned();
        let latest = info.latest_version.clone();
        let size_badge = update_size_badge(&info);
        let is_busy = busy.get();
        let phase = info.phase.as_str();
        let is_progress = is_busy
            || matches!(phase, "downloading" | "applying_delta" | "verifying");
        let is_applying = phase == "applying";
        let is_ready = info.staged && !is_progress && !is_applying;
        let err_msg = error.get().or_else(|| (!info.error.is_empty()).then(|| info.error.clone()));
        let notes = info.notes.clone();
        let can_auto_restart = info.can_auto_restart;
        let manual_path = if info.installer_path.is_empty() {
            info.staged_path.clone()
        } else {
            info.installer_path.clone()
        };

        let status_banner: Option<View> = if is_applying {
            Some(
                view()
                    .w_full()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .px_3p5()
                    .py_2p5()
                    .rounded(px(10.0))
                    .bg(colors.info_bg)
                    .child(icon(Icon::RefreshCw, 15.0, colors.info_ink))
                    .child(
                        text("正在重启并安装更新…")
                            .text_size(px(13.0))
                            .font_weight(FontWeight::MEDIUM)
                            .text_color(colors.info_ink),
                    ),
            )
        } else if is_progress {
            Some(progress_banner(info.clone()))
        } else if is_ready {
            Some(
                view()
                    .w_full()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .px_3p5()
                    .py_2p5()
                    .rounded(px(10.0))
                    .bg(colors.success_bg)
                    .border_1()
                    .border_color(SUCCESS_BORDER)
                    .child(icon(Icon::CircleCheck, 15.0, colors.success))
                    .child(
                        text(if can_auto_restart {
                            "下载完成！准备安装。"
                        } else {
                            "下载完成！请复制安装路径手动安装。"
                        })
                        .flex_1()
                        .text_size(px(13.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(colors.success),
                    ),
            )
        } else if let Some(msg) = err_msg {
            Some(
                view()
                    .w_full()
                    .flex_row()
                    .items_center()
                    .gap_2()
                    .px_3p5()
                    .py_2p5()
                    .rounded(px(10.0))
                    .bg(DANGER_BG)
                    .border_1()
                    .border_color(DANGER_BORDER)
                    .child(icon(Icon::CircleAlert, 15.0, colors.danger))
                    .child(
                        text(msg)
                            .flex_1()
                            .text_size(px(12.5))
                            .text_color(colors.danger),
                    ),
            )
        } else {
            None
        };

        let primary_cta = if is_ready && !can_auto_restart {
            let path = manual_path.clone();
            nsh_base::button()
                .on_press(move || controller::copy(path.clone()))
                .h(px(32.0))
                .px_4()
                .flex_row()
                .items_center()
                .gap_1p5()
                .rounded_full()
                .bg(UPDATE_CTA_BG)
                .hover(|s| s.opacity(0.9))
                .active(|s| s.opacity(0.75))
                .child(icon(Icon::Copy, 13.5, UPDATE_CTA_INK))
                .child(
                    text("复制安装路径")
                        .text_size(px(13.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(UPDATE_CTA_INK),
                )
        } else if is_ready {
            nsh_base::button()
                .on_press(|| controller::update_op(names::UPDATE_INSTALL))
                .disabled(is_busy || is_applying)
                .h(px(32.0))
                .px_4()
                .flex_row()
                .items_center()
                .gap_1p5()
                .rounded_full()
                .bg(UPDATE_CTA_BG)
                .hover(|s| s.opacity(0.9))
                .active(|s| s.opacity(0.75))
                .disabled_style(|s| s.opacity(0.45))
                .child(icon(Icon::RotateCcw, 13.5, UPDATE_CTA_INK))
                .child(
                    text("立即重启")
                        .text_size(px(13.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(UPDATE_CTA_INK),
                )
        } else {
            let label = if is_applying {
                "正在重启…"
            } else if is_progress {
                if phase == "applying_delta" {
                    "应用补丁中…"
                } else if phase == "verifying" {
                    "校验中…"
                } else {
                    "下载中…"
                }
            } else {
                "立即更新"
            };
            nsh_base::button()
                .on_press(|| controller::update_op(names::UPDATE_DOWNLOAD))
                .disabled(is_progress || is_applying)
                .h(px(32.0))
                .px_4()
                .flex_row()
                .items_center()
                .gap_1p5()
                .rounded_full()
                .bg(UPDATE_CTA_BG)
                .hover(|s| s.opacity(0.9))
                .active(|s| s.opacity(0.75))
                .disabled_style(|s| s.opacity(0.45))
                .child(icon(Icon::Download, 13.5, UPDATE_CTA_INK))
                .child(
                    text(label)
                        .text_size(px(13.0))
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(UPDATE_CTA_INK),
                )
        };

        Some(
            nsh_base::dialog()
                .animation(ModalAnimation::Fade)
                .on_cancel(controller::dismiss_update_dialog)
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
                                .max_w(px(460.0))
                                .flex_col()
                                .rounded(px(14.0))
                                .bg(colors.card)
                                .border_1()
                                .border_color(colors.line)
                                .shadow_xl()
                                .overflow_hidden()
                                // Main body
                                .child(
                                    view()
                                        .flex_col()
                                        .gap_3p5()
                                        .p_5()
                                        // Title + subtitle
                                        .child(
                                            view()
                                                .flex_col()
                                                .gap_1()
                                                .child(
                                                    text("软件更新")
                                                        .font_weight(FontWeight::SEMIBOLD)
                                                        .text_size(px(16.0))
                                                        .text_color(colors.ink),
                                                )
                                                .child(
                                                    text("发现新版本可用")
                                                        .text_size(px(13.0))
                                                        .text_color(colors.muted),
                                                ),
                                        )
                                        // Version transition row
                                        .child(
                                            view()
                                                .flex_row()
                                                .items_center()
                                                .gap_2()
                                                .child(
                                                    text(current)
                                                        .tabular_nums()
                                                        .text_size(px(13.5))
                                                        .text_color(colors.muted),
                                                )
                                                .child(
                                                    text("→")
                                                        .text_size(px(13.0))
                                                        .text_color(colors.faint),
                                                )
                                                .child(
                                                    text(latest)
                                                        .tabular_nums()
                                                        .font_weight(FontWeight::SEMIBOLD)
                                                        .text_size(px(13.5))
                                                        .text_color(colors.ink),
                                                )
                                                .child(view().flex_1())
                                                .child(when(!size_badge.is_empty(), {
                                                    let badge_text = size_badge.clone();
                                                    move || {
                                                        view()
                                                            .px_2()
                                                            .py_0p5()
                                                            .rounded_full()
                                                            .bg(colors.soft)
                                                            .child(
                                                                text(badge_text.clone())
                                                                    .text_size(px(11.0))
                                                                    .text_color(colors.muted),
                                                            )
                                                    }
                                                })),
                                        )
                                        // Release notes
                                        .child(when(!notes.trim().is_empty(), {
                                            let notes = notes.clone();
                                            move || {
                                                view()
                                                    .flex_col()
                                                    .gap_2()
                                                    .child(
                                                        text("更新内容")
                                                            .text_size(px(12.0))
                                                            .text_color(colors.muted),
                                                    )
                                                    .child(
                                                        scroll()
                                                            .max_h(px(210.0))
                                                            .child(notes_list(&notes)),
                                                    )
                                            }
                                        }))
                                        // Status / progress banner
                                        .child(when(status_banner.is_some(), move || {
                                            status_banner.clone().unwrap()
                                        })),
                                )
                                // Footer bar
                                .child(
                                    view()
                                        .flex_row()
                                        .items_center()
                                        .gap_2()
                                        .px_5()
                                        .py_3p5()
                                        .bg(colors.soft)
                                        .border_t_1()
                                        .border_t_color(colors.line)
                                        .child(
                                            nsh_base::button()
                                                .on_press(controller::skip_update_version)
                                                .disabled(is_applying)
                                                .px_1p5()
                                                .py_1()
                                                .rounded(px(6.0))
                                                .hover(|s| s.bg(colors.card))
                                                .active(|s| s.opacity(0.65))
                                                .child(
                                                    text("跳过此版本")
                                                        .text_size(px(13.0))
                                                        .text_color(colors.muted),
                                                ),
                                        )
                                        .child(view().flex_1())
                                        .child(ghost_button("稍后", controller::dismiss_update_dialog))
                                        .child(primary_cta),
                                ),
                        ),
                )
                .into_element(),
        )
    })
}
