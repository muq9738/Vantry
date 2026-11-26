//! The subscription card: the account and plan on top, the total allowance
//! as one prominent figure, the other windows as equal compact rows under
//! it, and the model catalog, refresh time and account actions in one
//! footer. One switch flips every window between used and remaining.

use nsh_core::prelude::*;

use crate::{
    bridge::workbench::actions::{PanelAction, QuotaCard, QuotaWindow},
    controller,
    icons::{Icon, icon},
    state::ui::SettingsPage,
    theme::palette,
};

const TRACK: AdaptiveColor = AdaptiveColor::rgb(0x4F46E5, 0xA5B4FC);
/// Below this card width the hero figure shrinks.
const COMPACT: f32 = 440.0;

// Magpie formats credit balances with grouping and up to two fractional digits.
fn count(value: f64) -> String {
    let decimal = format!("{:.2}", value.max(0.0));
    let (whole, fraction) = decimal.split_once('.').unwrap_or((&decimal, ""));
    let mut result = String::new();
    for (i, digit) in whole.chars().enumerate() {
        if i > 0 && (whole.len() - i).is_multiple_of(3) {
            result.push(',');
        }
        result.push(digit);
    }
    let fraction = fraction.trim_end_matches('0');
    if !fraction.is_empty() {
        result.push('.');
        result.push_str(fraction);
    }
    result
}

fn unit(window: &QuotaWindow) -> &str {
    if window.unit.eq_ignore_ascii_case("credits") {
        "积分"
    } else {
        &window.unit
    }
}

/// What a window shows in the chosen mode: the figure, what it is out of,
/// and the share for the track.
#[derive(Debug, Clone, PartialEq)]
struct Reading {
    value: String,
    of: String,
    percent: Option<f64>,
}

impl Reading {
    fn share(&self) -> f32 {
        self.percent.map_or(0.0, |p| p as f32 / 100.0)
    }
    fn percent_label(&self) -> String {
        self.percent
            .map(|p| format!("{}%", count((p * 10.0).round() / 10.0)))
            .unwrap_or_default()
    }
}

/// `with_unit`: the hero carries the unit; detail rows leave it out so the
/// card does not repeat it.
fn reading(window: &QuotaWindow, remaining: bool, with_unit: bool) -> Reading {
    let totals = window.amount.zip(window.limit.filter(|n| *n > 0.0));
    let percent = window
        .used
        .or_else(|| totals.map(|(n, limit)| n * 100.0 / limit))
        .map(|n| {
            let n = n.clamp(0.0, 100.0);
            if remaining { 100.0 - n } else { n }
        });
    let suffix = if with_unit && !unit(window).is_empty() {
        format!(" {}", unit(window))
    } else {
        String::new()
    };
    let (value, of) = if let Some((amount, limit)) = totals {
        let n = if remaining { limit - amount } else { amount };
        (count(n), format!("/ {}{suffix}", count(limit)))
    } else if !window.display.is_empty() && (!remaining || percent.is_none()) {
        let display = window
            .display
            .strip_suffix(" credits")
            .map(|n| format!("{n} 积分"))
            .unwrap_or_else(|| window.display.clone());
        (display, String::new())
    } else if let Some(p) = percent {
        (format!("{}%", count((p * 10.0).round() / 10.0)), String::new())
    } else {
        ("—".into(), "暂无读数".into())
    };
    // A percentage figure already says it all.
    let percent = percent.filter(|_| !value.ends_with('%'));
    Reading { value, of, percent }
}

fn track(height: f32, share: impl Fn() -> f32 + Clone + 'static) -> View {
    let colors = palette();
    let rest = share.clone();
    view()
        .w_full()
        .h(px(height))
        .flex_row()
        .rounded_full()
        .overflow_hidden()
        .bg(colors.line)
        .child(view().flex_grow(share).h_full().rounded_full().bg(TRACK))
        .child(view().flex_grow(move || 1.0 - rest()))
}

/// The used / remaining switch for the whole card.
fn mode_switch(remaining: Signal<bool>) -> View {
    let colors = palette();
    let segment = move |label: &'static str, value: bool| {
        let on = move || remaining.get() == value;
        nsh_base::button()
            .on_press(move || remaining.set(value))
            .accessibility_label(format!("显示{label}额度"))
            .px_2()
            .py_0p5()
            .rounded(px(5.0))
            .bg(move || if on() { colors.raised } else { colors.soft })
            .child(
                text(label)
                    .text_size(px(11.5))
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(move || if on() { colors.ink } else { colors.muted }),
            )
    };
    view()
        .flex_row()
        .flex_shrink_0()
        .p(px(2.0))
        .gap(px(2.0))
        .rounded(px(7.0))
        .bg(colors.soft)
        .child(segment("已用", false))
        .child(segment("剩余", true))
}

fn header(data: &QuotaCard, show_switch: bool, remaining: Signal<bool>) -> View {
    let colors = palette();
    let initial = data
        .account
        .chars()
        .find(|c| c.is_alphanumeric())
        .map(|c| c.to_uppercase().to_string())
        .unwrap_or_else(|| "?".into());
    let mut meta = view().flex_row().flex_wrap().items_center().gap_x_2().gap_y_0p5();
    if !data.plan.is_empty() {
        meta = meta.child(super::super::badge(data.plan.clone(), colors.accent));
    }
    if !data.expires.is_empty() {
        meta = meta.child(
            text(format!("{} 到期", data.expires))
                .text_size(px(11.5))
                .text_color(colors.muted),
        );
    }
    let mut row = view()
        .flex_row()
        .items_center()
        .gap_3()
        .child(
            view()
                .size(px(34.0))
                .flex_shrink_0()
                .items_center()
                .justify_center()
                .rounded_full()
                .bg(colors.soft)
                .child(
                    text(initial)
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_size(px(15.0))
                        .text_color(TRACK),
                ),
        )
        .child(
            view()
                .flex_1()
                .min_w(px(0.0))
                .flex_col()
                .gap_0p5()
                .child(
                    text(data.account.clone())
                        .select_text()
                        .truncate()
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_size(px(15.0))
                        .text_color(colors.ink),
                )
                .child(meta),
        );
    if show_switch {
        row = row.child(mode_switch(remaining));
    }
    row
}

fn hero(window: QuotaWindow, remaining: Signal<bool>, compact: Signal<bool>) -> View {
    let colors = palette();
    let readout = computed({
        let window = window.clone();
        move || reading(&window, remaining.get(), true)
    });
    let has_track = readout.get_untracked().percent.is_some();
    let figure = view()
        .flex_row()
        .flex_wrap()
        .items_baseline()
        .gap_x_1p5()
        .child(
            text(move || readout.get().value)
                .select_text()
                .tabular_nums()
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(move || px(if compact.get() { 24.0 } else { 30.0 }))
                .text_color(colors.ink),
        )
        .child(
            text(move || readout.get().of)
                .tabular_nums()
                .text_size(px(13.0))
                .text_color(colors.muted),
        );
    let mut top = view()
        .flex_row()
        .flex_wrap()
        .items_end()
        .justify_between()
        .gap_x_3()
        .gap_y_1()
        .child(
            view()
                .flex_col()
                .gap_0p5()
                .child(text(window.name).text_size(px(12.0)).text_color(colors.muted))
                .child(figure),
        );
    if has_track {
        top = top.child(
            text(move || {
                let r = readout.get();
                format!("{} {}", if remaining.get() { "剩余" } else { "已用" }, r.percent_label())
            })
            .mb_1()
            .tabular_nums()
            .font_weight(FontWeight::SEMIBOLD)
            .text_size(px(12.5))
            .text_color(TRACK),
        );
    }
    let mut column = view().flex_col().gap_2().child(top);
    if has_track {
        column = column.child(track(6.0, move || readout.get().share()));
    }
    if !window.reset.is_empty() {
        column = column.child(
            text(format!("{} 重置", window.reset))
                .text_size(px(11.0))
                .text_color(colors.faint),
        );
    }
    column
}

fn detail(window: QuotaWindow, with_unit: bool, remaining: Signal<bool>) -> View {
    let colors = palette();
    let readout = computed({
        let window = window.clone();
        move || reading(&window, remaining.get(), with_unit)
    });
    let has_track = readout.get_untracked().percent.is_some();
    let mut value = view()
        .flex_row()
        .items_baseline()
        .gap_1p5()
        .child(
            text(move || {
                let r = readout.get();
                if r.of.is_empty() { r.value } else { format!("{} {}", r.value, r.of) }
            })
            .tabular_nums()
            .font_weight(FontWeight::MEDIUM)
            .text_size(px(12.5))
            .text_color(colors.ink),
        );
    if has_track {
        value = value.child(
            text(move || readout.get().percent_label())
                .tabular_nums()
                .text_size(px(11.0))
                .text_color(colors.muted),
        );
    }
    let mut cell = view()
        .basis(px(200.0))
        .flex_grow(1.0)
        .min_w(px(0.0))
        .flex_col()
        .gap_1p5()
        .child(
            view()
                .flex_row()
                .items_baseline()
                .justify_between()
                .gap_2()
                .child(text(window.name).text_size(px(12.0)).text_color(colors.muted))
                .child(value),
        );
    if has_track {
        cell = cell.child(track(4.0, move || readout.get().share()));
    }
    if !window.reset.is_empty() {
        cell = cell.child(
            text(format!("{} 重置", window.reset))
                .text_size(px(11.0))
                .text_color(colors.faint),
        );
    }
    cell
}

fn models_link(n: u64) -> impl IntoElement {
    let colors = palette();
    nsh_base::button()
        .on_press(|| controller::open_settings(SettingsPage::Models))
        .accessibility_label(format!("{n} 个可用模型，查看模型目录"))
        .flex_row()
        .items_center()
        .gap_1p5()
        .px_1p5()
        .py_1()
        .rounded(px(6.0))
        .hover(|s| s.bg(colors.soft))
        .child(icon(Icon::Layers, 13.0, colors.muted))
        .child(text(format!("{n} 个可用模型")).text_size(px(12.0)).text_color(colors.ink))
        .child(text("查看目录").text_size(px(12.0)).text_color(colors.accent))
        .child(icon(Icon::ChevronRight, 12.0, colors.accent))
}

pub(super) fn card(data: QuotaCard, actions: Vec<PanelAction>, remaining: Signal<bool>) -> AnyElement {
    let colors = palette();
    let compact = signal(false);
    let mut windows = data.windows.clone();
    let lead = windows
        .iter()
        .position(|w| w.name == "总额度")
        .or_else(|| windows.iter().position(|w| w.amount.is_some() && w.limit.is_some()))
        .unwrap_or(0);
    let lead = (!windows.is_empty()).then(|| windows.remove(lead));
    let switchable = lead.iter().chain(&windows).any(|w| reading(w, false, true).percent.is_some());
    let mut panel = view()
        .w_full()
        .on_layout(move |b: &Bounds<Points>| compact.set(b.size.width.0 < COMPACT))
        .flex_col()
        .px_4()
        .py_3p5()
        .gap_4()
        .rounded(px(10.0))
        .border_1()
        .border_color(colors.line)
        .bg(colors.raised)
        .child(header(&data, switchable, remaining));
    let lead_unit = lead.as_ref().map(|w| unit(w).to_owned()).unwrap_or_default();
    if let Some(window) = lead {
        panel = panel.child(hero(window, remaining, compact));
    }
    if !windows.is_empty() {
        panel = panel.child(
            view()
                .w_full()
                .flex_row()
                .flex_wrap()
                .gap_x_6()
                .gap_y_3()
                .pt_3()
                .border_t_1()
                .border_t_color(colors.line_soft)
                .children(windows.into_iter().map(|w| {
                    let with_unit = unit(&w) != lead_unit;
                    detail(w, with_unit, remaining)
                })),
        );
    }
    if !data.message.is_empty() {
        panel = panel.child(text(data.message).text_size(px(12.0)).text_color(colors.warning));
    }
    let mut footer = view()
        .flex_row()
        .flex_wrap()
        .items_center()
        .gap_x_2()
        .gap_y_2()
        .pt_3()
        .border_t_1()
        .border_t_color(colors.line_soft);
    if let Some(n) = data.models {
        footer = footer.child(models_link(n));
    }
    footer = footer.child(view().flex_1());
    if !data.updated.is_empty() {
        footer = footer.child(
            text(format!("更新于 {}", data.updated))
                .text_size(px(11.0))
                .text_color(colors.faint),
        );
    }
    if !actions.is_empty() {
        footer = footer.child(super::actions_row(actions));
    }
    panel.child(footer).into_element()
}

#[cfg(test)]
mod tests {
    use super::*;
    use nsh_core::{ApproxTextEngine, Core, MountItem, NodeId, element::Root};
    use std::collections::HashMap;

    fn window(name: &str, amount: f64, limit: f64) -> QuotaWindow {
        QuotaWindow {
            name: name.into(),
            amount: Some(amount),
            limit: Some(limit),
            unit: "credits".into(),
            ..Default::default()
        }
    }

    #[component]
    fn QuotaFixture() -> impl IntoElement {
        card(
            QuotaCard {
                account: "test@example.com".into(),
                plan: "Teams".into(),
                expires: "12-31".into(),
                updated: "11:02".into(),
                models: Some(26),
                windows: vec![
                    window("个人额度", 12_000.0, 15_000.0),
                    window("总额度", 16_750.0, 23_000.0),
                    window("共享额度", 4_750.0, 8_000.0),
                ],
                ..Default::default()
            },
            vec![PanelAction { id: "refresh".into(), label: "刷新额度".into(), ..Default::default() }],
            signal(false),
        )
    }

    struct Mounted {
        ui: Core<ApproxTextEngine>,
        parents: HashMap<NodeId, NodeId>,
        frames: HashMap<NodeId, Bounds<Points>>,
        labels: HashMap<String, NodeId>,
    }

    impl Mounted {
        fn new(width: f32) -> Self {
            let mut ui = Core::new(ApproxTextEngine, Size::new(px(width), px(480.0)));
            ui.mount_root(&Root::of::<QuotaFixture>());
            let mut m = Self { ui, parents: HashMap::new(), frames: HashMap::new(), labels: HashMap::new() };
            m.settle();
            m
        }
        fn settle(&mut self) {
            use nsh_core::element::JsEvent;
            for _ in 0..4 {
                let mut events = Vec::new();
                self.ui.take_events(&mut events);
                for event in events {
                    match event {
                        JsEvent::LayoutListener { host, id, listener, frame } => {
                            self.ui.component_layout(host, id, &listener, frame)
                        }
                        JsEvent::Event(id, kind, value) => {
                            self.ui.component_event(id, kind, &value);
                        }
                        _ => {}
                    }
                }
                for item in self.ui.commit().into_iter().flat_map(|batch| batch.items) {
                    match item {
                        MountItem::Insert { parent, child, .. } => {
                            self.parents.insert(child, parent);
                        }
                        MountItem::Text { id, text, .. } => {
                            self.labels.insert(text.visible_text().into_owned(), id);
                        }
                        MountItem::Frame { id, frame } => {
                            self.frames.insert(id, frame);
                        }
                        _ => {}
                    }
                }
            }
        }
        fn has(&self, label: &str) -> bool {
            self.labels.contains_key(label)
        }
        /// Window-space frame of `id`.
        fn frame(&self, mut id: NodeId) -> (f32, f32, f32, f32) {
            let size = self.frames[&id].size;
            let (mut x, mut y) = (0.0, 0.0);
            loop {
                if let Some(frame) = self.frames.get(&id) {
                    x += frame.origin.x.0;
                    y += frame.origin.y.0;
                }
                let Some(parent) = self.parents.get(&id) else { break };
                id = *parent;
            }
            (x, y, size.width.0, size.height.0)
        }
        fn at(&self, label: &str) -> (f32, f32, f32, f32) {
            self.frame(self.labels[label])
        }
        fn up(&self, label: &str, levels: usize) -> NodeId {
            (0..levels).fold(self.labels[label], |id, _| self.parents[&id])
        }
        fn press(&mut self, label: &str) {
            let mut id = self.labels[label];
            while !self.ui.listens(id, nsh_core::EventKind::Press) {
                id = self.parents[&id];
            }
            self.ui.component_event(id, nsh_core::EventKind::Press, &nsh_core::EventPayload::None);
            self.settle();
        }
    }

    #[test]
    fn total_leads_and_details_share_the_row() {
        for (width, wide) in [(712.0, true), (330.0, false)] {
            let m = Mounted::new(width);
            // The total is the one big figure, with the unit once on the card.
            assert!(m.has("16,750") && m.has("/ 23,000 积分") && m.has("已用 72.8%"), "width={width}");
            assert!(m.has("12,000 / 15,000") && m.has("4,750 / 8,000"), "details drop the unit");
            assert_eq!(m.labels.keys().filter(|l| l.contains("积分")).count(), 1);
            let (hero, personal, shared) = (m.at("16,750"), m.at("个人额度"), m.at("共享额度"));
            assert!(hero.3 > personal.3 * 1.6, "hero figure is prominent: {hero:?} {personal:?}");
            assert!(personal.1 > hero.1 && shared.1 > hero.1, "details sit under the total");
            // detail cell = label → top row → cell; its row is the cell's parent.
            let (cell_a, cell_b) = (m.frame(m.up("个人额度", 2)), m.frame(m.up("共享额度", 2)));
            let row = m.frame(m.parents[&m.up("个人额度", 2)]);
            if wide {
                assert_eq!(personal.1, shared.1, "width={width}: side by side");
                assert!((cell_a.2 - cell_b.2).abs() < 1.0, "equal cells: {cell_a:?} {cell_b:?}");
                assert!((cell_b.0 + cell_b.2 - (row.0 + row.2)).abs() < 1.0, "no gap on the right: {cell_b:?} {row:?}");
            } else {
                assert!(personal.0 == shared.0 && shared.1 > personal.1, "width={width}: stacked");
                assert!((cell_a.2 - row.2).abs() < 1.0, "full-width rows: {cell_a:?} {row:?}");
            }
            // Model link, refresh time and the refresh button close the card together.
            let (models, updated, refresh) = (m.at("26 个可用模型"), m.at("更新于 11:02"), m.at("刷新额度"));
            assert!(models.1 > personal.1 && updated.1 > shared.1 && refresh.1 > shared.1);
            if wide {
                assert!(models.0 < updated.0 && updated.0 < refresh.0, "{models:?} {updated:?} {refresh:?}");
                assert!((models.1 + models.3 / 2.0 - (refresh.1 + refresh.3 / 2.0)).abs() < 3.0, "one footer line");
            }
        }
    }

    #[test]
    fn one_switch_flips_every_window() {
        let mut m = Mounted::new(712.0);
        m.press("剩余");
        assert!(m.has("6,250") && m.has("剩余 27.2%"), "hero shows remaining");
        assert!(m.has("3,000 / 15,000") && m.has("20%"));
        assert!(m.has("3,250 / 8,000") && m.has("40.6%"));
        m.press("已用");
        assert!(m.has("16,750") && m.has("已用 72.8%") && m.has("80%"));
    }

    #[test]
    fn readings_use_reported_counts() {
        let mut w = QuotaWindow {
            amount: Some(16_750.0),
            limit: Some(23_000.0),
            used: Some(72.826),
            unit: "credits".into(),
            ..Default::default()
        };
        let used = reading(&w, false, true);
        assert_eq!((used.value.as_str(), used.of.as_str()), ("16,750", "/ 23,000 积分"));
        assert_eq!(used.percent_label(), "72.8%");
        let left = reading(&w, true, false);
        assert_eq!((left.value.as_str(), left.of.as_str()), ("6,250", "/ 23,000"));
        assert_eq!(left.percent_label(), "27.2%");
        w.amount = w.limit;
        w.used = None;
        assert_eq!(reading(&w, true, true).share(), 0.0);
        let empty = reading(&QuotaWindow::default(), false, true);
        assert_eq!((empty.value.as_str(), empty.percent), ("—", None));
        let shown = QuotaWindow { display: "120 credits".into(), used: Some(40.0), ..Default::default() };
        assert_eq!(reading(&shown, false, true).value, "120 积分");
        assert_eq!(reading(&shown, true, true).value, "60%");
    }
}
