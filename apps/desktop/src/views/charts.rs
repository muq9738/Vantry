//! Native GPU-accelerated charts & Mermaid diagrams via NSH `Canvas` (`Painter`),
//! matching Vantry MyGo (`v0.1.42`) visual block parity:
//! - Charts (`chart` / `vantry-chart`): bar, hbar, stacked, stacked100, line, area,
//!   pie, donut, combo, scatter, bubble, heatmap, breakdown, stats + Studio JSON &
//!   compact multi-series pipe tables.
//! - Diagrams (`mermaid`, `flow`, `state`, `sequence`, `er`, `class`): layered
//!   flowcharts & state machines (with subgraphs and shapes), sequence diagrams,
//!   pie diagrams, and ER/class entity diagrams — all with preview/source toggle
//!   and expand modal.

use std::collections::{HashMap, HashSet, VecDeque};
use std::f32::consts::{FRAC_PI_2, TAU};

use nsh_core::{
    LineHeight,
    canvas::{CanvasTextAlign, CanvasTextBaseline, LineCap, LineJoin},
    prelude::*,
};
use serde_json::Value;

use super::{badge, icon_button, link_button};
use crate::{
    controller,
    icons::{Icon, icon},
    theme::palette,
};

const MONO: &str = "Menlo";

const SERIES_PALETTE: [u32; 8] = [
    0x4F7DF3, // cobalt
    0x2FB380, // emerald
    0xE88B36, // amber
    0xA26CF5, // violet
    0xE6597A, // rose
    0x1EA7C9, // cyan
    0xC49A2B, // ochre
    0x78849E, // slate
];

fn series_color(idx: usize) -> AdaptiveColor {
    let hex = SERIES_PALETTE[idx % SERIES_PALETTE.len()];
    AdaptiveColor::rgb(hex, hex)
}

fn hsla(c: AdaptiveColor) -> Hsla {
    color(c)
}

fn truncate_chars(s: &str, max: usize) -> String {
    let chars: Vec<char> = s.chars().collect();
    if chars.len() <= max {
        s.to_owned()
    } else if max <= 1 {
        "…".to_owned()
    } else {
        let mut out: String = chars[..max - 1].iter().collect();
        out.push('…');
        out
    }
}

fn approx_text_width(s: &str, font_size: f32) -> f32 {
    s.chars()
        .map(|c| {
            if c.is_ascii() {
                font_size * 0.56
            } else {
                font_size * 0.98
            }
        })
        .sum()
}

fn format_num(v: f32) -> String {
    if !v.is_finite() {
        return "—".to_owned();
    }
    let abs = v.abs();
    if abs >= 1_000_000.0 {
        format!("{:.1}M", v / 1_000_000.0)
    } else if abs >= 10_000.0 {
        format!("{:.1}k", v / 1_000.0)
    } else if (v - v.round()).abs() < 0.005 {
        format!("{:.0}", v)
    } else if abs >= 100.0 {
        format!("{:.1}", v)
    } else {
        format!("{:.2}", v).trim_end_matches('0').trim_end_matches('.').to_owned()
    }
}

// ===========================================================================
// 1. Chart Spec & Parser (Studio JSON + Compact Multi-Series Pipe Table)
// ===========================================================================

#[derive(Debug, Clone)]
pub(crate) struct ChartSeries {
    pub name: String,
    pub subtitle: String,
    pub badge: String,
    pub kind: String, // bar | line | area
    pub data: Vec<f32>,
    pub sparkline: Vec<f32>,
    pub points: Vec<(String, f32, f32, f32)>, // label, x, y, r
}

#[derive(Debug, Clone)]
pub(crate) struct ChartRefLine {
    pub value: f32,
    pub label: String,
    pub tone: String,
}

#[derive(Debug, Clone)]
pub(crate) struct ChartStatItem {
    pub label: String,
    pub value: String,
    pub delta: String,
    pub note: String,
}

#[derive(Debug, Clone)]
pub(crate) struct ChartSpec {
    pub kind: String,
    pub title: String,
    pub subtitle: String,
    pub hero: String,
    pub hero_label: String,
    pub badge: String,
    pub badge_tone: String,
    pub delta: String,
    pub style: String, // dots | tiles
    pub unit: String,
    pub unit_right: String,
    pub center_label: String,
    pub labels: Vec<String>,
    pub highlights: Vec<bool>,
    pub series: Vec<ChartSeries>,
    pub ref_lines: Vec<ChartRefLine>,
    pub items: Vec<ChartStatItem>,
    pub height: f32,
}

fn parse_fence_attrs(info: &str) -> (HashMap<String, String>, Vec<String>) {
    let mut attrs = HashMap::new();
    let mut words = Vec::new();
    let mut s = info.trim();
    while !s.is_empty() {
        s = s.trim_start();
        if s.is_empty() {
            break;
        }
        let eq = s.find('=');
        let sp = s.find(char::is_whitespace);
        if let Some(eq_pos) = eq
            && eq_pos > 0
            && sp.is_none_or(|sp_pos| eq_pos < sp_pos)
        {
            let key = s[..eq_pos].trim().to_ascii_lowercase();
            s = &s[eq_pos + 1..];
            let val = if let Some(quote) = s.chars().next().filter(|c| *c == '"' || *c == '\'') {
                s = &s[quote.len_utf8()..];
                if let Some(end) = s.find(quote) {
                    let v = &s[..end];
                    s = &s[end + quote.len_utf8()..];
                    v.to_owned()
                } else {
                    let v = s.to_owned();
                    s = "";
                    v
                }
            } else if let Some(end) = s.find(char::is_whitespace) {
                let v = s[..end].to_owned();
                s = &s[end..];
                v
            } else {
                let v = s.to_owned();
                s = "";
                v
            };
            attrs.insert(key, val);
        } else if let Some(end) = sp {
            words.push(s[..end].to_owned());
            s = &s[end..];
        } else {
            words.push(s.to_owned());
            s = "";
        }
    }
    (attrs, words)
}

fn parse_number_cell(raw: &str) -> Option<f32> {
    let trimmed = raw
        .trim()
        .trim_end_matches('%')
        .replace(',', "");
    if trimmed.is_empty() || trimmed == "-" || trimmed.eq_ignore_ascii_case("null") || trimmed.eq_ignore_ascii_case("nan") {
        return None;
    }
    trimmed.parse::<f32>().ok()
}

pub(crate) fn parse_chart_spec(args: &str, source: &str) -> Option<ChartSpec> {
    let trimmed = source.trim();
    if trimmed.is_empty() {
        return None;
    }
    if let Ok(val) = serde_json::from_str::<Value>(trimmed)
        && val.is_object()
    {
        return parse_chart_json(&val);
    }
    parse_chart_pipe(args, trimmed)
}

fn parse_chart_json(val: &Value) -> Option<ChartSpec> {
    let mut kind = val["type"]
        .as_str()
        .unwrap_or("")
        .trim()
        .to_ascii_lowercase();
    let stack = val["stack"].as_str().unwrap_or("").to_ascii_lowercase();
    if stack == "100%" || stack == "percent" {
        kind = "stacked100".into();
    } else if stack == "normal" && (kind.is_empty() || kind == "bar") {
        kind = "stacked".into();
    }
    let labels: Vec<String> = val["labels"]
        .as_array()
        .or_else(|| val["x"].as_array())
        .map(|arr| {
            arr.iter()
                .map(|v| {
                    v.as_str()
                        .map(str::to_owned)
                        .unwrap_or_else(|| v.to_string())
                })
                .collect()
        })
        .unwrap_or_default();

    let mut series: Vec<ChartSeries> = Vec::new();
    if let Some(arr) = val["series"].as_array() {
        for s in arr {
            let data = s["data"]
                .as_array()
                .map(|d| {
                    d.iter()
                        .map(|v| v.as_f64().map_or(f32::NAN, |n| n as f32))
                        .collect()
                })
                .unwrap_or_default();
            let sparkline = s["sparkline"]
                .as_array()
                .map(|d| {
                    d.iter()
                        .filter_map(|v| v.as_f64().map(|n| n as f32))
                        .collect()
                })
                .unwrap_or_default();
            let points = s["points"]
                .as_array()
                .map(|pts| {
                    pts.iter()
                        .map(|p| {
                            (
                                p["label"].as_str().unwrap_or("").to_owned(),
                                p["x"].as_f64().unwrap_or(0.0) as f32,
                                p["y"].as_f64().unwrap_or(0.0) as f32,
                                p["r"].as_f64().unwrap_or(5.0) as f32,
                            )
                        })
                        .collect()
                })
                .unwrap_or_default();
            series.push(ChartSeries {
                name: s["name"].as_str().unwrap_or("").to_owned(),
                subtitle: s["subtitle"].as_str().unwrap_or("").to_owned(),
                badge: s["badge"].as_str().unwrap_or("").to_owned(),
                kind: s["kind"].as_str().unwrap_or("").to_ascii_lowercase(),
                data,
                sparkline,
                points,
            });
        }
    } else if let Some(vals) = val["values"].as_array() {
        series.push(ChartSeries {
            name: val["title"].as_str().unwrap_or("数值").to_owned(),
            subtitle: String::new(),
            badge: String::new(),
            kind: String::new(),
            data: vals
                .iter()
                .map(|v| v.as_f64().map_or(f32::NAN, |n| n as f32))
                .collect(),
            sparkline: Vec::new(),
            points: Vec::new(),
        });
    }

    let items: Vec<ChartStatItem> = val["items"]
        .as_array()
        .map(|arr| {
            arr.iter()
                .map(|it| ChartStatItem {
                    label: it["label"].as_str().unwrap_or("").to_owned(),
                    value: it["value"]
                        .as_str()
                        .map(str::to_owned)
                        .unwrap_or_else(|| {
                            it["value"]
                                .as_f64()
                                .map(|n| format_num(n as f32))
                                .unwrap_or_default()
                        }),
                    delta: it["delta"].as_str().unwrap_or("").to_owned(),
                    note: it["note"].as_str().unwrap_or("").to_owned(),
                })
                .collect()
        })
        .unwrap_or_default();

    let ref_lines: Vec<ChartRefLine> = val["refLines"]
        .as_array()
        .map(|arr| {
            arr.iter()
                .filter_map(|r| {
                    Some(ChartRefLine {
                        value: r["value"].as_f64()? as f32,
                        label: r["label"].as_str().unwrap_or("").to_owned(),
                        tone: r["tone"].as_str().unwrap_or("warning").to_owned(),
                    })
                })
                .collect()
        })
        .unwrap_or_default();

    if kind.is_empty() {
        kind = if !items.is_empty() && series.is_empty() {
            "stats".into()
        } else if series.iter().any(|s| !s.points.is_empty()) {
            "scatter".into()
        } else {
            "bar".into()
        };
    }

    Some(ChartSpec {
        kind,
        title: val["title"].as_str().unwrap_or("数据图表").to_owned(),
        subtitle: val["subtitle"].as_str().unwrap_or("").to_owned(),
        hero: val["hero"].as_str().unwrap_or("").to_owned(),
        hero_label: val["heroLabel"].as_str().unwrap_or("").to_owned(),
        badge: val["badge"].as_str().unwrap_or("").to_owned(),
        badge_tone: val["badgeTone"].as_str().unwrap_or("accent").to_owned(),
        delta: val["delta"].as_str().unwrap_or("").to_owned(),
        style: val["style"].as_str().unwrap_or("tiles").to_ascii_lowercase(),
        unit: val["unit"].as_str().unwrap_or("").to_owned(),
        unit_right: val["unitRight"].as_str().unwrap_or("").to_owned(),
        center_label: val["centerLabel"].as_str().unwrap_or("").to_owned(),
        labels,
        highlights: Vec::new(),
        series,
        ref_lines,
        items,
        height: val["height"].as_f64().unwrap_or(0.0) as f32,
    })
}

fn parse_chart_pipe(args: &str, source: &str) -> Option<ChartSpec> {
    let (attrs, words) = parse_fence_attrs(args);
    let mut kind = String::new();
    let mut horizontal = false;
    let mut stacked = false;
    let mut stacked100 = false;
    for w in &words {
        match w.to_ascii_lowercase().as_str() {
            "horizontal" | "hbar" => horizontal = true,
            "stacked" | "stack" => stacked = true,
            "100%" | "stacked100" | "percent" => stacked100 = true,
            t @ ("bar" | "line" | "area" | "pie" | "donut" | "combo" | "scatter" | "bubble"
            | "heatmap" | "breakdown" | "stats") => {
                kind = t.to_owned();
            }
            _ => {}
        }
    }
    if stacked100 {
        kind = "stacked100".into();
    } else if stacked {
        kind = "stacked".into();
    } else if horizontal {
        kind = "hbar".into();
    } else if kind.is_empty() {
        kind = "bar".into();
    }

    let mut series_names: Vec<String> = Vec::new();
    let mut labels: Vec<String> = Vec::new();
    let mut highlights: Vec<bool> = Vec::new();
    let mut rows: Vec<Vec<f32>> = Vec::new();

    for raw in source.lines() {
        let trimmed = raw.trim();
        if trimmed.is_empty() || !trimmed.contains('|') {
            continue;
        }
        // Skip markdown table separator rows like |---|---|
        if trimmed
            .chars()
            .all(|c| c == '|' || c == '-' || c == ':' || c.is_whitespace())
        {
            continue;
        }
        let highlight = trimmed.starts_with('*');
        let cleaned = trimmed.trim_start_matches('*').trim();
        let parts: Vec<&str> = cleaned.split('|').map(str::trim).collect();
        if parts.len() < 2 {
            continue;
        }
        // Header row: first cell empty or non-numeric rest cells
        let value_cells = &parts[1..];
        let any_num = value_cells.iter().any(|c| parse_number_cell(c).is_some());
        if !any_num && labels.is_empty() && series_names.is_empty() {
            series_names = value_cells
                .iter()
                .map(|c| c.trim().to_owned())
                .filter(|c| !c.is_empty())
                .collect();
            continue;
        }
        if !any_num {
            continue;
        }
        let label = parts[0].trim();
        if label.is_empty() {
            continue;
        }
        labels.push(label.to_owned());
        highlights.push(highlight);
        rows.push(
            value_cells
                .iter()
                .map(|c| parse_number_cell(c).unwrap_or(f32::NAN))
                .collect(),
        );
    }

    if labels.is_empty() {
        return None;
    }

    let col_count = rows
        .iter()
        .map(Vec::len)
        .max()
        .unwrap_or(1)
        .max(series_names.len());
    let mut series = Vec::with_capacity(col_count);
    for col in 0..col_count {
        let name = series_names
            .get(col)
            .cloned()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| {
                if col_count == 1 {
                    attrs.get("unit").cloned().unwrap_or_default()
                } else {
                    format!("系列 {}", col + 1)
                }
            });
        let data: Vec<f32> = rows
            .iter()
            .map(|r| r.get(col).copied().unwrap_or(f32::NAN))
            .collect();
        series.push(ChartSeries {
            name,
            subtitle: String::new(),
            badge: String::new(),
            kind: String::new(),
            data,
            sparkline: Vec::new(),
            points: Vec::new(),
        });
    }

    let mut ref_lines = Vec::new();
    if let Some(r) = attrs.get("ref").and_then(|v| v.parse::<f32>().ok()) {
        ref_lines.push(ChartRefLine {
            value: r,
            label: attrs
                .get("reflabel")
                .cloned()
                .unwrap_or_else(|| format!("参考线 {}", format_num(r))),
            tone: "warning".into(),
        });
    }

    Some(ChartSpec {
        kind,
        title: attrs
            .get("title")
            .cloned()
            .unwrap_or_else(|| "数据对比".to_owned()),
        subtitle: attrs.get("subtitle").cloned().unwrap_or_default(),
        hero: String::new(),
        hero_label: String::new(),
        badge: String::new(),
        badge_tone: "accent".into(),
        delta: String::new(),
        style: attrs
            .get("style")
            .cloned()
            .unwrap_or_else(|| "tiles".into()),
        unit: attrs.get("unit").cloned().unwrap_or_default(),
        unit_right: attrs.get("unitright").cloned().unwrap_or_default(),
        center_label: attrs.get("center").cloned().unwrap_or_default(),
        labels,
        highlights,
        series,
        ref_lines,
        items: Vec::new(),
        height: attrs
            .get("height")
            .and_then(|v| v.parse::<f32>().ok())
            .unwrap_or(0.0),
    })
}

// ===========================================================================
// 2. GPU Canvas Chart Painters
// ===========================================================================

fn chart_canvas_size(spec: &ChartSpec, expanded: bool) -> (f32, f32) {
    let width = if expanded { 820.0 } else { 640.0 };
    let base_h = match spec.kind.as_str() {
        "pie" | "donut" => 240.0,
        "hbar" | "breakdown" => {
            let rows = spec.labels.len().max(1) as f32;
            let per_row = if spec.series.len() > 1 {
                20.0 * spec.series.len() as f32 + 12.0
            } else {
                32.0
            };
            (rows * per_row + 32.0).clamp(140.0, 440.0)
        }
        "heatmap" => {
            let rows = spec.series.len().max(1) as f32;
            (rows * 34.0 + 54.0).clamp(150.0, 420.0)
        }
        _ => 240.0,
    };
    let height = if spec.height > 80.0 {
        spec.height
    } else if expanded {
        (base_h * 1.35).min(520.0)
    } else {
        base_h
    };
    (width, height)
}

fn nice_max(raw: f32) -> f32 {
    if !raw.is_finite() || raw <= 0.0 {
        return 10.0;
    }
    let exp = 10f32.powf(raw.log10().floor());
    let frac = raw / exp;
    let nice = if frac <= 1.0 {
        1.0
    } else if frac <= 1.5 {
        1.5
    } else if frac <= 2.0 {
        2.0
    } else if frac <= 2.5 {
        2.5
    } else if frac <= 5.0 {
        5.0
    } else {
        10.0
    };
    nice * exp
}

fn paint_chart(spec: &ChartSpec, w: f32, h: f32) -> Painter {
    match spec.kind.as_str() {
        "pie" | "donut" => paint_pie_chart(spec, w, h, spec.kind == "donut"),
        "hbar" | "breakdown" => paint_hbar_chart(spec, w, h),
        "line" | "area" => paint_line_area_chart(spec, w, h, spec.kind == "area"),
        "stacked" | "stacked100" => paint_stacked_bar_chart(spec, w, h, spec.kind == "stacked100"),
        "combo" => paint_combo_chart(spec, w, h),
        "scatter" | "bubble" => paint_scatter_chart(spec, w, h, spec.kind == "bubble"),
        "heatmap" => paint_heatmap_chart(spec, w, h),
        _ => paint_bar_chart(spec, w, h),
    }
}

fn paint_grid_and_y_axis(
    mut p: Painter,
    plot_x: f32,
    plot_y: f32,
    plot_w: f32,
    plot_h: f32,
    min_v: f32,
    max_v: f32,
    unit: &str,
) -> Painter {
    let colors = palette();
    let grid = hsla(colors.line_soft);
    let label_c = hsla(colors.faint);
    let steps = 4usize;
    p = p
        .font("10px sans-serif")
        .text_align(CanvasTextAlign::Right)
        .text_baseline(CanvasTextBaseline::Middle);
    for i in 0..=steps {
        let frac = i as f32 / steps as f32;
        let y = plot_y + plot_h * (1.0 - frac);
        let val = min_v + (max_v - min_v) * frac;
        p = p
            .stroke_color(grid)
            .line_width(1.0)
            .begin_path()
            .move_to(plot_x, y)
            .line_to(plot_x + plot_w, y)
            .stroke();
        let lbl = if unit.is_empty() {
            format_num(val)
        } else {
            format!("{}{unit}", format_num(val))
        };
        p = p.fill_color(label_c).fill_text(&lbl, plot_x - 8.0, y);
    }
    p
}

fn paint_ref_lines(
    mut p: Painter,
    spec: &ChartSpec,
    plot_x: f32,
    plot_y: f32,
    plot_w: f32,
    plot_h: f32,
    min_v: f32,
    max_v: f32,
) -> Painter {
    let colors = palette();
    let range = (max_v - min_v).max(1e-6);
    for r in &spec.ref_lines {
        if r.value < min_v || r.value > max_v {
            continue;
        }
        let y = plot_y + plot_h * (1.0 - (r.value - min_v) / range);
        let tone = match r.tone.as_str() {
            "danger" => colors.danger,
            "success" => colors.success,
            _ => colors.warning,
        };
        p = p
            .save()
            .stroke_color(hsla(tone))
            .line_width(1.25)
            .line_dash(&[px(5.0), px(4.0)])
            .begin_path()
            .move_to(plot_x, y)
            .line_to(plot_x + plot_w, y)
            .stroke()
            .restore();
        if !r.label.is_empty() {
            p = p
                .font("500 10px sans-serif")
                .text_align(CanvasTextAlign::Right)
                .text_baseline(CanvasTextBaseline::Bottom)
                .fill_color(hsla(tone))
                .fill_text(&r.label, plot_x + plot_w - 4.0, y - 3.0);
        }
    }
    p
}

fn paint_bar_chart(spec: &ChartSpec, w: f32, h: f32) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let pad_l = 52.0;
    let pad_r = 16.0;
    let pad_t = 18.0;
    let pad_b = 34.0;
    let plot_w = (w - pad_l - pad_r).max(60.0);
    let plot_h = (h - pad_t - pad_b).max(60.0);

    let mut raw_max = 0.0f32;
    for s in &spec.series {
        for &v in &s.data {
            if v.is_finite() {
                raw_max = raw_max.max(v);
            }
        }
    }
    for r in &spec.ref_lines {
        raw_max = raw_max.max(r.value);
    }
    let max_v = nice_max(raw_max);

    p = paint_grid_and_y_axis(p, pad_l, pad_t, plot_w, plot_h, 0.0, max_v, &spec.unit);

    let n_groups = spec.labels.len().max(1);
    let n_series = spec.series.len().max(1);
    let group_w = plot_w / n_groups as f32;
    let inner_w = (group_w * 0.72).max(8.0);
    let gap = if n_series > 1 { 3.0 } else { 0.0 };
    let bar_w = ((inner_w - gap * (n_series as f32 - 1.0)) / n_series as f32).clamp(4.0, 42.0);
    let cluster_w = bar_w * n_series as f32 + gap * (n_series as f32 - 1.0);

    for (gi, lbl) in spec.labels.iter().enumerate() {
        let gx = pad_l + gi as f32 * group_w + (group_w - cluster_w) * 0.5;
        let highlighted = spec.highlights.get(gi).copied().unwrap_or(false);
        for (si, s) in spec.series.iter().enumerate() {
            let v = s.data.get(gi).copied().unwrap_or(f32::NAN);
            if !v.is_finite() {
                continue;
            }
            let frac = (v / max_v).clamp(0.0, 1.0);
            let bh = (frac * plot_h).max(2.0);
            let bx = gx + si as f32 * (bar_w + gap);
            let by = pad_t + plot_h - bh;
            let fill = if n_series == 1 && highlighted {
                hsla(colors.accent)
            } else {
                hsla(series_color(si))
            };
            let rect = Bounds::new(point(px(bx), px(by)), size(px(bar_w), px(bh)));
            p = p
                .fill_color(fill)
                .begin_path()
                .round_rect(rect, px(3.5))
                .fill();
            if n_groups * n_series <= 12 {
                p = p
                    .font("10px sans-serif")
                    .text_align(CanvasTextAlign::Center)
                    .text_baseline(CanvasTextBaseline::Bottom)
                    .fill_color(hsla(colors.muted))
                    .fill_text(&format_num(v), bx + bar_w * 0.5, by - 3.0);
            }
        }
        let cx = pad_l + (gi as f32 + 0.5) * group_w;
        p = p
            .font(if highlighted { "600 10.5px sans-serif" } else { "10.5px sans-serif" })
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Top)
            .fill_color(hsla(if highlighted { colors.accent } else { colors.muted }))
            .fill_text_max_width(&truncate_chars(lbl, 14), cx, pad_t + plot_h + 8.0, px(group_w - 4.0));
    }

    paint_ref_lines(p, spec, pad_l, pad_t, plot_w, plot_h, 0.0, max_v)
}

fn paint_stacked_bar_chart(spec: &ChartSpec, w: f32, h: f32, percent: bool) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let pad_l = 52.0;
    let pad_r = 16.0;
    let pad_t = 18.0;
    let pad_b = 34.0;
    let plot_w = (w - pad_l - pad_r).max(60.0);
    let plot_h = (h - pad_t - pad_b).max(60.0);

    let n_groups = spec.labels.len().max(1);
    let totals: Vec<f32> = (0..n_groups)
        .map(|gi| {
            spec.series
                .iter()
                .map(|s| s.data.get(gi).copied().unwrap_or(0.0).max(0.0))
                .filter(|v| v.is_finite())
                .sum()
        })
        .collect();
    let max_v = if percent {
        100.0
    } else {
        nice_max(totals.iter().copied().fold(0.0f32, f32::max))
    };
    let unit = if percent { "%" } else { &spec.unit };
    p = paint_grid_and_y_axis(p, pad_l, pad_t, plot_w, plot_h, 0.0, max_v, unit);

    let group_w = plot_w / n_groups as f32;
    let bar_w = (group_w * 0.56).clamp(10.0, 48.0);
    for (gi, lbl) in spec.labels.iter().enumerate() {
        let bx = pad_l + gi as f32 * group_w + (group_w - bar_w) * 0.5;
        let total = totals[gi].max(1e-6);
        let mut acc = 0.0f32;
        for (si, s) in spec.series.iter().enumerate() {
            let v = s.data.get(gi).copied().unwrap_or(0.0);
            if !v.is_finite() || v <= 0.0 {
                continue;
            }
            let scaled = if percent { (v / total) * 100.0 } else { v };
            let seg_h = (scaled / max_v * plot_h).max(1.0);
            let by = pad_t + plot_h - ((acc + scaled) / max_v * plot_h);
            let rect = Bounds::new(point(px(bx), px(by)), size(px(bar_w), px(seg_h)));
            p = p
                .fill_color(hsla(series_color(si)))
                .begin_path()
                .round_rect(rect, px(2.0))
                .fill();
            acc += scaled;
        }
        let cx = pad_l + (gi as f32 + 0.5) * group_w;
        p = p
            .font("10.5px sans-serif")
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Top)
            .fill_color(hsla(colors.muted))
            .fill_text_max_width(&truncate_chars(lbl, 14), cx, pad_t + plot_h + 8.0, px(group_w - 4.0));
    }
    paint_ref_lines(p, spec, pad_l, pad_t, plot_w, plot_h, 0.0, max_v)
}

fn paint_hbar_chart(spec: &ChartSpec, w: f32, h: f32) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let pad_l = 128.0;
    let pad_r = 56.0;
    let pad_t = 12.0;
    let pad_b = 14.0;
    let plot_w = (w - pad_l - pad_r).max(80.0);
    let plot_h = (h - pad_t - pad_b).max(40.0);

    let mut raw_max = 0.0f32;
    for s in &spec.series {
        for &v in &s.data {
            if v.is_finite() {
                raw_max = raw_max.max(v);
            }
        }
    }
    let max_v = nice_max(raw_max);
    let n_rows = spec.labels.len().max(1);
    let n_series = spec.series.len().max(1);
    let row_h = plot_h / n_rows as f32;
    let bar_h = ((row_h * 0.68) / n_series as f32).clamp(6.0, 16.0);
    let gap = if n_series > 1 { 3.0 } else { 0.0 };
    let cluster_h = bar_h * n_series as f32 + gap * (n_series as f32 - 1.0);

    for (ri, lbl) in spec.labels.iter().enumerate() {
        let ry = pad_t + ri as f32 * row_h;
        let cy = ry + row_h * 0.5;
        let hi = spec.highlights.get(ri).copied().unwrap_or(false);
        p = p
            .font(if hi { "600 11.5px sans-serif" } else { "11.5px sans-serif" })
            .text_align(CanvasTextAlign::Right)
            .text_baseline(CanvasTextBaseline::Middle)
            .fill_color(hsla(if hi { colors.accent } else { colors.ink }))
            .fill_text_max_width(&truncate_chars(lbl, 18), pad_l - 10.0, cy, px(pad_l - 16.0));
        let top = cy - cluster_h * 0.5;
        for (si, s) in spec.series.iter().enumerate() {
            let v = s.data.get(ri).copied().unwrap_or(f32::NAN);
            if !v.is_finite() {
                continue;
            }
            let by = top + si as f32 * (bar_h + gap);
            // Track background
            let track = Bounds::new(point(px(pad_l), px(by)), size(px(plot_w), px(bar_h)));
            p = p
                .fill_color(hsla(colors.soft))
                .begin_path()
                .round_rect(track, px(bar_h * 0.45))
                .fill();
            let bw = ((v / max_v).clamp(0.0, 1.0) * plot_w).max(4.0);
            let fill = if n_series == 1 && hi {
                hsla(colors.accent)
            } else {
                hsla(series_color(si))
            };
            let bar = Bounds::new(point(px(pad_l), px(by)), size(px(bw), px(bar_h)));
            p = p
                .fill_color(fill)
                .begin_path()
                .round_rect(bar, px(bar_h * 0.45))
                .fill();
            let val_txt = format!("{}{}", format_num(v), spec.unit);
            p = p
                .font("10.5px Menlo")
                .text_align(CanvasTextAlign::Left)
                .text_baseline(CanvasTextBaseline::Middle)
                .fill_color(hsla(colors.muted))
                .fill_text(&val_txt, pad_l + bw + 6.0, by + bar_h * 0.5);
        }
    }
    p
}

fn paint_line_area_chart(spec: &ChartSpec, w: f32, h: f32, area: bool) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let pad_l = 52.0;
    let pad_r = 18.0;
    let pad_t = 18.0;
    let pad_b = 34.0;
    let plot_w = (w - pad_l - pad_r).max(60.0);
    let plot_h = (h - pad_t - pad_b).max(60.0);

    let mut min_v = 0.0f32;
    let mut max_v = 0.0f32;
    for s in &spec.series {
        for &v in &s.data {
            if v.is_finite() {
                min_v = min_v.min(v);
                max_v = max_v.max(v);
            }
        }
    }
    for r in &spec.ref_lines {
        max_v = max_v.max(r.value);
    }
    max_v = nice_max(max_v);
    let range = (max_v - min_v).max(1e-6);

    p = paint_grid_and_y_axis(p, pad_l, pad_t, plot_w, plot_h, min_v, max_v, &spec.unit);

    let n = spec
        .labels
        .len()
        .max(spec.series.iter().map(|s| s.data.len()).max().unwrap_or(0))
        .max(1);
    let x_at = |i: usize| -> f32 {
        if n <= 1 {
            pad_l + plot_w * 0.5
        } else {
            pad_l + (i as f32 / (n - 1) as f32) * plot_w
        }
    };
    let y_at = |v: f32| -> f32 { pad_t + plot_h * (1.0 - (v - min_v) / range) };
    let base_y = y_at(0.0f32.clamp(min_v, max_v));

    for (si, s) in spec.series.iter().enumerate() {
        let pts: Vec<(f32, f32)> = s
            .data
            .iter()
            .enumerate()
            .filter_map(|(i, &v)| v.is_finite().then(|| (x_at(i), y_at(v))))
            .collect();
        if pts.is_empty() {
            continue;
        }
        let col = series_color(si);
        if area && pts.len() >= 2 {
            p = p.save().fill_color(hsla(col.opacity(0.18))).begin_path();
            p = p.move_to(pts[0].0, base_y);
            for &(x, y) in &pts {
                p = p.line_to(x, y);
            }
            p = p.line_to(pts[pts.len() - 1].0, base_y).close_path().fill().restore();
        }
        p = p
            .stroke_color(hsla(col))
            .line_width(2.2)
            .line_cap(LineCap::Round)
            .line_join(LineJoin::Round)
            .begin_path()
            .move_to(pts[0].0, pts[0].1);
        for &(x, y) in &pts[1..] {
            p = p.line_to(x, y);
        }
        p = p.stroke();
        if pts.len() <= 24 {
            for &(x, y) in &pts {
                p = p
                    .fill_color(hsla(colors.card))
                    .stroke_color(hsla(col))
                    .line_width(2.0)
                    .begin_path()
                    .arc(x, y, 3.3, 0.0, TAU)
                    .fill()
                    .stroke();
            }
        }
    }

    // X-axis labels (thin out when dense)
    let step = (n / 8).max(1);
    for (i, lbl) in spec.labels.iter().enumerate() {
        if i % step != 0 && i + 1 != spec.labels.len() {
            continue;
        }
        p = p
            .font("10px sans-serif")
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Top)
            .fill_color(hsla(colors.muted))
            .fill_text(&truncate_chars(lbl, 12), x_at(i), pad_t + plot_h + 8.0);
    }

    paint_ref_lines(p, spec, pad_l, pad_t, plot_w, plot_h, min_v, max_v)
}

fn paint_combo_chart(spec: &ChartSpec, w: f32, h: f32) -> Painter {
    let mut bar_spec = spec.clone();
    bar_spec.series = spec
        .series
        .iter()
        .filter(|s| s.kind.is_empty() || s.kind == "bar")
        .cloned()
        .collect();
    if bar_spec.series.is_empty() {
        return paint_line_area_chart(spec, w, h, false);
    }
    let mut p = paint_bar_chart(&bar_spec, w, h);
    let pad_l = 52.0;
    let pad_r = 16.0;
    let pad_t = 18.0;
    let pad_b = 34.0;
    let plot_w = (w - pad_l - pad_r).max(60.0);
    let plot_h = (h - pad_t - pad_b).max(60.0);
    let n_groups = spec.labels.len().max(1);
    let group_w = plot_w / n_groups as f32;

    for (si, s) in spec.series.iter().enumerate() {
        if s.kind != "line" && s.kind != "area" {
            continue;
        }
        let max_v = nice_max(
            s.data
                .iter()
                .copied()
                .filter(|v| v.is_finite())
                .fold(0.0f32, f32::max),
        );
        let pts: Vec<(f32, f32)> = s
            .data
            .iter()
            .enumerate()
            .filter_map(|(i, &v)| {
                v.is_finite().then(|| {
                    let x = pad_l + (i as f32 + 0.5) * group_w;
                    let y = pad_t + plot_h * (1.0 - (v / max_v).clamp(0.0, 1.0));
                    (x, y)
                })
            })
            .collect();
        if pts.is_empty() {
            continue;
        }
        let col = hsla(series_color(si));
        p = p
            .stroke_color(col)
            .line_width(2.2)
            .line_cap(LineCap::Round)
            .line_join(LineJoin::Round)
            .begin_path()
            .move_to(pts[0].0, pts[0].1);
        for &(x, y) in &pts[1..] {
            p = p.line_to(x, y);
        }
        p = p.stroke();
        for &(x, y) in &pts {
            p = p
                .fill_color(col)
                .begin_path()
                .arc(x, y, 3.5, 0.0, TAU)
                .fill();
        }
    }
    p
}

fn paint_pie_chart(spec: &ChartSpec, w: f32, h: f32, donut: bool) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let values: Vec<f32> = spec
        .series
        .first()
        .map(|s| {
            s.data
                .iter()
                .map(|v| if v.is_finite() { v.max(0.0) } else { 0.0 })
                .collect()
        })
        .unwrap_or_default();
    let total: f32 = values.iter().sum();
    if total <= 0.0 {
        return p;
    }

    let radius = (h * 0.42).min(w * 0.24).clamp(48.0, 108.0);
    let cx = (radius + 36.0).min(w * 0.34);
    let cy = h * 0.5;
    let mut start = -FRAC_PI_2;
    for (i, &v) in values.iter().enumerate() {
        let sweep = (v / total) * TAU;
        if sweep <= 0.001 {
            continue;
        }
        let col = hsla(series_color(i));
        p = p
            .fill_color(col)
            .begin_path()
            .move_to(cx, cy)
            .arc(cx, cy, radius, start, start + sweep)
            .close_path()
            .fill();
        start += sweep;
    }
    if donut {
        p = p
            .fill_color(hsla(colors.card))
            .begin_path()
            .arc(cx, cy, radius * 0.56, 0.0, TAU)
            .fill();
        let center = if spec.center_label.is_empty() {
            format!("{}{}", format_num(total), spec.unit)
        } else {
            spec.center_label.clone()
        };
        p = p
            .font("600 13px sans-serif")
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Middle)
            .fill_color(hsla(colors.ink))
            .fill_text(&truncate_chars(&center, 12), cx, cy);
    }

    // Right-side legend
    let lx = cx + radius + 34.0;
    let max_items = values.len().min(8);
    let row_h = 22.0f32;
    let top_y = (cy - (max_items as f32 * row_h) * 0.5).max(16.0);
    for (i, &v) in values.iter().take(max_items).enumerate() {
        let y = top_y + i as f32 * row_h + 10.0;
        let lbl = spec
            .labels
            .get(i)
            .cloned()
            .unwrap_or_else(|| format!("#{}", i + 1));
        let pct = (v / total) * 100.0;
        let dot_rect = Bounds::new(point(px(lx), px(y - 4.5)), size(px(9.0), px(9.0)));
        p = p
            .fill_color(hsla(series_color(i)))
            .begin_path()
            .round_rect(dot_rect, px(2.5))
            .fill()
            .font("11.5px sans-serif")
            .text_align(CanvasTextAlign::Left)
            .text_baseline(CanvasTextBaseline::Middle)
            .fill_color(hsla(colors.ink))
            .fill_text(
                &format!("{}  {}{} ({:.1}%)", truncate_chars(&lbl, 16), format_num(v), spec.unit, pct),
                lx + 15.0,
                y,
            );
    }
    p
}

fn paint_scatter_chart(spec: &ChartSpec, w: f32, h: f32, bubble: bool) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let pad_l = 52.0;
    let pad_r = 22.0;
    let pad_t = 18.0;
    let pad_b = 34.0;
    let plot_w = (w - pad_l - pad_r).max(60.0);
    let plot_h = (h - pad_t - pad_b).max(60.0);

    let mut min_x = f32::INFINITY;
    let mut max_x = f32::NEG_INFINITY;
    let mut min_y = f32::INFINITY;
    let mut max_y = f32::NEG_INFINITY;
    let mut max_r = 1.0f32;
    for s in &spec.series {
        for &(_, x, y, r) in &s.points {
            min_x = min_x.min(x);
            max_x = max_x.max(x);
            min_y = min_y.min(y);
            max_y = max_y.max(y);
            max_r = max_r.max(r);
        }
    }
    if !min_x.is_finite() {
        return p;
    }
    min_x = min_x.min(0.0);
    min_y = min_y.min(0.0);
    max_x = nice_max(max_x);
    max_y = nice_max(max_y);
    let rx = (max_x - min_x).max(1e-6);
    let ry = (max_y - min_y).max(1e-6);

    p = paint_grid_and_y_axis(p, pad_l, pad_t, plot_w, plot_h, min_y, max_y, &spec.unit);
    for (si, s) in spec.series.iter().enumerate() {
        let col = series_color(si);
        for (lbl, x, y, r) in &s.points {
            let px_x = pad_l + ((x - min_x) / rx) * plot_w;
            let px_y = pad_t + plot_h * (1.0 - (y - min_y) / ry);
            let rad = if bubble {
                ((r / max_r).clamp(0.15, 1.0) * 16.0).max(4.5)
            } else {
                5.0
            };
            p = p
                .fill_color(hsla(col.opacity(0.65)))
                .stroke_color(hsla(col))
                .line_width(1.5)
                .begin_path()
                .arc(px_x, px_y, rad, 0.0, TAU)
                .fill()
                .stroke();
            if !lbl.is_empty() {
                p = p
                    .font("9.5px sans-serif")
                    .text_align(CanvasTextAlign::Center)
                    .text_baseline(CanvasTextBaseline::Bottom)
                    .fill_color(hsla(colors.muted))
                    .fill_text(&truncate_chars(lbl, 10), px_x, px_y - rad - 2.0);
            }
        }
    }
    p
}

fn paint_heatmap_chart(spec: &ChartSpec, w: f32, h: f32) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let pad_l = 96.0;
    let pad_r = 16.0;
    let pad_t = 28.0;
    let pad_b = 14.0;
    let plot_w = (w - pad_l - pad_r).max(80.0);
    let plot_h = (h - pad_t - pad_b).max(40.0);

    let rows = spec.series.len().max(1);
    let cols = spec
        .labels
        .len()
        .max(spec.series.iter().map(|s| s.data.len()).max().unwrap_or(1))
        .max(1);
    let mut max_v = 0.0f32;
    for s in &spec.series {
        for &v in &s.data {
            if v.is_finite() {
                max_v = max_v.max(v);
            }
        }
    }
    let max_v = max_v.max(1e-6);
    let cw = plot_w / cols as f32;
    let ch = plot_h / rows as f32;
    let dots = spec.style == "dots";

    for (ci, lbl) in spec.labels.iter().enumerate() {
        p = p
            .font("10px sans-serif")
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Bottom)
            .fill_color(hsla(colors.muted))
            .fill_text(&truncate_chars(lbl, 10), pad_l + (ci as f32 + 0.5) * cw, pad_t - 6.0);
    }

    for (ri, s) in spec.series.iter().enumerate() {
        let cy = pad_t + (ri as f32 + 0.5) * ch;
        p = p
            .font("10.5px sans-serif")
            .text_align(CanvasTextAlign::Right)
            .text_baseline(CanvasTextBaseline::Middle)
            .fill_color(hsla(colors.ink))
            .fill_text(&truncate_chars(&s.name, 14), pad_l - 8.0, cy);
        for ci in 0..cols {
            let v = s.data.get(ci).copied().unwrap_or(f32::NAN);
            if !v.is_finite() {
                continue;
            }
            let intensity = (v / max_v).clamp(0.08, 1.0);
            let cx = pad_l + (ci as f32 + 0.5) * cw;
            if dots {
                let rad = (cw.min(ch) * 0.42 * intensity.sqrt()).max(3.5);
                p = p
                    .fill_color(hsla(colors.accent.opacity(0.25 + 0.7 * intensity)))
                    .begin_path()
                    .arc(cx, cy, rad, 0.0, TAU)
                    .fill();
            } else {
                let rect = Bounds::new(
                    point(px(pad_l + ci as f32 * cw + 2.0), px(pad_t + ri as f32 * ch + 2.0)),
                    size(px((cw - 4.0).max(4.0)), px((ch - 4.0).max(4.0))),
                );
                p = p
                    .fill_color(hsla(colors.accent.opacity(0.14 + 0.78 * intensity)))
                    .begin_path()
                    .round_rect(rect, px(4.0))
                    .fill()
                    .font("10px Menlo")
                    .text_align(CanvasTextAlign::Center)
                    .text_baseline(CanvasTextBaseline::Middle)
                    .fill_color(hsla(if intensity > 0.55 {
                        colors.on_accent
                    } else {
                        colors.ink
                    }))
                    .fill_text(&format_num(v), cx, cy);
            }
        }
    }
    p
}

fn paint_sparkline(values: &[f32], w: f32, h: f32, col: AdaptiveColor) -> Painter {
    let mut p = Painter::new();
    if values.len() < 2 {
        return p;
    }
    let min = values.iter().copied().fold(f32::INFINITY, f32::min);
    let max = values.iter().copied().fold(f32::NEG_INFINITY, f32::max);
    let range = (max - min).max(1e-6);
    p = p
        .stroke_color(hsla(col))
        .line_width(1.75)
        .line_cap(LineCap::Round)
        .line_join(LineJoin::Round)
        .begin_path();
    for (i, &v) in values.iter().enumerate() {
        let x = 2.0 + (i as f32 / (values.len() - 1) as f32) * (w - 4.0);
        let y = 2.0 + (1.0 - (v - min) / range) * (h - 4.0);
        if i == 0 {
            p = p.move_to(x, y);
        } else {
            p = p.line_to(x, y);
        }
    }
    p.stroke()
}

pub(crate) fn render_chart_card(args: &str, source: &str) -> impl IntoElement {
    let colors = palette();
    let Some(spec) = parse_chart_spec(args, source) else {
        return super::visuals::card_shell(
            super::visuals::card_header("图表", "数据图表", source.to_owned()),
            view().p_3().child(
                text(source.trim().to_owned())
                    .select_text()
                    .font_family(MONO)
                    .text_size(px(12.0))
                    .text_color(colors.muted),
            ),
        );
    };

    let show_source = signal(false);
    let expanded = signal(false);
    let copy_src = source.to_owned();
    let raw_src = source.trim().to_owned();
    let kind_tag = match spec.kind.as_str() {
        "hbar" => "横向柱状图",
        "stacked" => "堆叠柱状图",
        "stacked100" => "百分比堆叠图",
        "line" => "折线图",
        "area" => "面积图",
        "pie" => "饼图",
        "donut" => "环形图",
        "combo" => "组合图",
        "scatter" => "散点图",
        "bubble" => "气泡图",
        "heatmap" => "热力图",
        "stats" | "breakdown" => "指标卡",
        _ => "柱状图",
    };

    let header = view()
        .w_full()
        .min_w_0()
        .flex_row()
        .items_center()
        .gap_2()
        .px_3()
        .py_1p5()
        .bg(colors.soft)
        .border_b_1()
        .border_b_color(colors.line_soft)
        .child(badge(kind_tag, colors.accent))
        .child(
            text(spec.title.clone())
                .flex_1()
                .min_w_0()
                .number_of_lines(1u32)
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(px(12.5))
                .text_color(colors.ink),
        )
        .child(link_button(
            move || if show_source.get() { "图表" } else { "源码" },
            move || show_source.set(!show_source.get_untracked()),
        ))
        .child(
            nsh_base::button()
                .accessibility_label("放大图表")
                .on_press(move || expanded.set(true))
                .p_1()
                .rounded(px(5.0))
                .hover(|s| s.bg(colors.card))
                .child(icon(Icon::Maximize2, 12.5, colors.muted)),
        )
        .child(link_button("复制", move || controller::copy(copy_src.clone())));

    let spec_inline = spec.clone();
    let spec_modal = spec.clone();
    let title_modal = spec.title.clone();

    let body = view()
        .w_full()
        .min_w_0()
        .flex_col()
        .child(when(move || expanded.get(), move || {
            let spec_m = spec_modal.clone();
            let title_m = title_modal.clone();
            let (mw, mh) = chart_canvas_size(&spec_m, true);
            nsh_base::dialog()
                .animation(ModalAnimation::Fade)
                .on_cancel(move || expanded.set(false))
                .child(
                    view()
                        .flex_1()
                        .items_center()
                        .justify_center()
                        .bg(colors.scrim)
                        .p_6()
                        .on_press(move || expanded.set(false))
                        .child(
                            view()
                                .flex_col()
                                .gap_2()
                                .p_4()
                                .rounded(px(14.0))
                                .bg(colors.card)
                                .border_1()
                                .border_color(colors.line)
                                .shadow_xl()
                                .child(
                                    view()
                                        .flex_row()
                                        .items_center()
                                        .gap_2()
                                        .child(
                                            text(title_m)
                                                .flex_1()
                                                .font_weight(FontWeight::SEMIBOLD)
                                                .text_size(px(14.0))
                                                .text_color(colors.ink),
                                        )
                                        .child(icon_button(Icon::X, "关闭", move || {
                                            expanded.set(false)
                                        })),
                                )
                                .child(scroll().horizontal().child(
                                    canvas(paint_chart(&spec_m, mw, mh)).w(px(mw)).h(px(mh)),
                                )),
                        ),
                )
        }))
        .child(dynamic(move || {
            if show_source.get() {
                return view()
                    .w_full()
                    .min_w_0()
                    .p_3()
                    .child(
                        text(raw_src.clone())
                            .select_text()
                            .font_family(MONO)
                            .text_size(px(12.0))
                            .line_height(LineHeight::Multiple(1.5))
                            .text_color(colors.ink),
                    )
                    .into_element();
            }
            let mut content = view().w_full().min_w_0().flex_col().gap_2().p_3();
            if !spec_inline.subtitle.is_empty() {
                content = content.child(
                    text(spec_inline.subtitle.clone())
                        .text_size(px(12.0))
                        .text_color(colors.muted),
                );
            }
            if !spec_inline.hero.is_empty() {
                let delta_txt = spec_inline.delta.clone();
                let badge_txt = spec_inline.badge.clone();
                let badge_c = match spec_inline.badge_tone.as_str() {
                    "success" => colors.success,
                    "warning" => colors.warning,
                    "danger" => colors.danger,
                    "neutral" => colors.muted,
                    _ => colors.accent,
                };
                content = content.child(
                    view()
                        .flex_row()
                        .items_baseline()
                        .flex_wrap()
                        .gap_2()
                        .child(
                            text(spec_inline.hero.clone())
                                .font_weight(FontWeight::BOLD)
                                .text_size(px(22.0))
                                .text_color(colors.ink),
                        )
                        .child(when(!spec_inline.hero_label.is_empty(), {
                            let hl = spec_inline.hero_label.clone();
                            move || text(hl.clone()).text_size(px(12.0)).text_color(colors.muted)
                        }))
                        .child(when(!delta_txt.is_empty(), move || {
                            badge(delta_txt.clone(), colors.accent)
                        }))
                        .child(when(!badge_txt.is_empty(), move || {
                            badge(badge_txt.clone(), badge_c)
                        })),
                );
            }
            if !spec_inline.items.is_empty() {
                let mut rows = view().w_full().min_w_0().flex_col().gap_1p5();
                for it in &spec_inline.items {
                    let delta = it.delta.clone();
                    let note = it.note.clone();
                    rows = rows.child(
                        view()
                            .w_full()
                            .min_w_0()
                            .flex_row()
                            .items_center()
                            .gap_2()
                            .py_1()
                            .border_b_1()
                            .border_b_color(colors.line_soft)
                            .child(
                                text(it.label.clone())
                                    .flex_1()
                                    .min_w_0()
                                    .text_size(px(12.5))
                                    .text_color(colors.muted),
                            )
                            .child(
                                text(it.value.clone())
                                    .font_family(MONO)
                                    .font_weight(FontWeight::SEMIBOLD)
                                    .text_size(px(13.0))
                                    .text_color(colors.ink),
                            )
                            .child(when(!delta.is_empty(), move || {
                                badge(delta.clone(), colors.accent)
                            }))
                            .child(when(!note.is_empty(), move || {
                                text(note.clone()).text_size(px(11.5)).text_color(colors.faint)
                            })),
                    );
                }
                content = content.child(rows);
            }
            if spec_inline.series.iter().any(|s| !s.sparkline.is_empty()) {
                let mut rows = view().w_full().min_w_0().flex_col().gap_1p5();
                for (si, s) in spec_inline.series.iter().enumerate() {
                    if s.sparkline.is_empty() {
                        continue;
                    }
                    let last = s.sparkline.last().copied().unwrap_or(0.0);
                    rows = rows.child(
                        view()
                            .flex_row()
                            .items_center()
                            .gap_2()
                            .child(
                                text(s.name.clone())
                                    .flex_1()
                                    .text_size(px(12.5))
                                    .text_color(colors.ink),
                            )
                            .child(
                                canvas(paint_sparkline(&s.sparkline, 110.0, 24.0, series_color(si)))
                                    .w(px(110.0))
                                    .h(px(24.0)),
                            )
                            .child(
                                text(format!("{}{}", format_num(last), spec_inline.unit))
                                    .font_family(MONO)
                                    .text_size(px(12.0))
                                    .text_color(colors.muted),
                            ),
                    );
                }
                content = content.child(rows);
            } else if !spec_inline.series.is_empty()
                && (spec_inline.series.iter().any(|s| !s.data.is_empty())
                    || spec_inline.series.iter().any(|s| !s.points.is_empty()))
            {
                let (cw, ch) = chart_canvas_size(&spec_inline, false);
                content = content.child(
                    scroll()
                        .w_full()
                        .min_w_0()
                        .horizontal()
                        .child(canvas(paint_chart(&spec_inline, cw, ch)).w(px(cw)).h(px(ch))),
                );
                if spec_inline.series.len() > 1
                    && spec_inline.kind != "pie"
                    && spec_inline.kind != "donut"
                    && spec_inline.kind != "heatmap"
                {
                    let mut legend = view().flex_row().flex_wrap().items_center().gap_3().pt_1();
                    for (si, s) in spec_inline.series.iter().enumerate() {
                        if s.name.is_empty() {
                            continue;
                        }
                        legend = legend.child(
                            view()
                                .flex_row()
                                .items_center()
                                .gap_1()
                                .child(
                                    view()
                                        .w(px(9.0))
                                        .h(px(9.0))
                                        .rounded(px(2.5))
                                        .bg(series_color(si)),
                                )
                                .child(
                                    text(s.name.clone())
                                        .text_size(px(11.0))
                                        .text_color(colors.muted),
                                ),
                        );
                    }
                    content = content.child(legend);
                }
            }
            content.into_element()
        }));

    super::visuals::card_shell(header, body)
}

// ===========================================================================
// 3. Native Mermaid & Diagram Parser + GPU Canvas Painter
// ===========================================================================

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum NodeShape {
    Rect,
    Rounded,
    Stadium,
    Diamond,
    Circle,
}

#[derive(Debug, Clone)]
struct FlowNode {
    id: String,
    label: String,
    shape: NodeShape,
    group: Option<usize>,
    rank: usize,
    order: usize,
    x: f32,
    y: f32,
    w: f32,
    h: f32,
}

#[derive(Debug, Clone)]
struct FlowEdge {
    from: usize,
    to: usize,
    label: String,
    dashed: bool,
    thick: bool,
}

#[derive(Debug, Clone)]
struct FlowGroup {
    title: String,
    nodes: Vec<usize>,
}

#[derive(Debug, Clone)]
struct FlowGraph {
    horizontal: bool,
    nodes: Vec<FlowNode>,
    edges: Vec<FlowEdge>,
    groups: Vec<FlowGroup>,
    width: f32,
    height: f32,
}

#[derive(Debug, Clone)]
struct SeqMessage {
    from: usize,
    to: usize,
    label: String,
    dashed: bool,
}

#[derive(Debug, Clone)]
struct SeqDiagram {
    participants: Vec<String>,
    messages: Vec<SeqMessage>,
    autonumber: bool,
    width: f32,
    height: f32,
}

#[derive(Debug, Clone)]
struct BoxEntity {
    name: String,
    members: Vec<String>,
}

#[derive(Debug, Clone)]
struct EntityDiagram {
    entities: Vec<BoxEntity>,
    relations: Vec<(usize, usize, String)>,
    width: f32,
    height: f32,
}

#[derive(Debug, Clone)]
enum ParsedDiagram {
    Flow(FlowGraph),
    Sequence(SeqDiagram),
    Pie(ChartSpec),
    Entities(EntityDiagram),
    Unsupported,
}

fn parse_node_token(raw: &str) -> (String, Option<(String, NodeShape)>) {
    let s = raw.trim().trim_end_matches(';').trim();
    if s == "[*]" {
        return ("__start__".into(), Some(("●".into(), NodeShape::Circle)));
    }
    let patterns: [(&str, &str, NodeShape); 6] = [
        ("([", "])", NodeShape::Stadium),
        ("((", "))", NodeShape::Circle),
        ("[(", ")]", NodeShape::Rounded),
        ("[", "]", NodeShape::Rect),
        ("(", ")", NodeShape::Rounded),
        ("{", "}", NodeShape::Diamond),
    ];
    for (open, close, shape) in patterns {
        if let Some(pos) = s.find(open)
            && s.ends_with(close)
            && pos + open.len() <= s.len() - close.len()
        {
            let id = s[..pos].trim();
            let label = s[pos + open.len()..s.len() - close.len()]
                .trim()
                .trim_matches('"')
                .trim_matches('\'');
            if !id.is_empty() {
                return (
                    id.to_owned(),
                    Some((
                        if label.is_empty() {
                            id.to_owned()
                        } else {
                            label.replace("<br/>", " ").replace("<br>", " ")
                        },
                        shape,
                    )),
                );
            }
        }
    }
    let clean = s.trim_matches('"').trim_matches('\'');
    (clean.to_owned(), None)
}

fn split_flow_edge(line: &str) -> Option<(String, String, String, bool, bool)> {
    // Support: A -->|label| B, A -.->|label| B, A ==>|label| B, A -- label --> B, A --> B, A -> B
    let arrows: [(&str, bool, bool); 6] = [
        ("-.->", true, false),
        ("==>", false, true),
        ("-->", false, false),
        ("--->", false, false),
        ("->", false, false),
        ("---", false, false),
    ];
    // Handle `A -- label --> B`
    if let Some(first_dash) = line.find(" -- ")
        && let Some(arrow_rel) = line[first_dash + 4..].find(" --> ")
    {
        let left = line[..first_dash].trim().to_owned();
        let label = line[first_dash + 4..first_dash + 4 + arrow_rel]
            .trim()
            .trim_matches('"')
            .to_owned();
        let right = line[first_dash + 4 + arrow_rel + 5..].trim().to_owned();
        if !left.is_empty() && !right.is_empty() {
            return Some((left, right, label, false, false));
        }
    }
    for (arrow, dashed, thick) in arrows {
        if let Some(pos) = line.find(arrow) {
            let left = line[..pos].trim();
            let mut right = line[pos + arrow.len()..].trim();
            let mut label = String::new();
            if let Some(rest) = right.strip_prefix('|')
                && let Some(end) = rest.find('|')
            {
                label = rest[..end].trim().trim_matches('"').to_owned();
                right = rest[end + 1..].trim();
            } else if let Some((target, lbl)) = right.split_once(':')
                && !target.trim().is_empty()
                && !target.contains('[')
                && !target.contains('(')
                && !target.contains('{')
            {
                label = lbl.trim().to_owned();
                right = target.trim();
            }
            if !left.is_empty() && !right.is_empty() {
                return Some((left.to_owned(), right.to_owned(), label, dashed, thick));
            }
        }
    }
    None
}

fn parse_flow_graph(source: &str, default_horizontal: bool) -> Option<FlowGraph> {
    let mut horizontal = default_horizontal;
    let mut nodes: Vec<FlowNode> = Vec::new();
    let mut index: HashMap<String, usize> = HashMap::new();
    let mut edges: Vec<FlowEdge> = Vec::new();
    let mut groups: Vec<FlowGroup> = Vec::new();
    let mut group_stack: Vec<usize> = Vec::new();

    fn ensure_node(
        raw: &str,
        group: Option<usize>,
        nodes: &mut Vec<FlowNode>,
        index: &mut HashMap<String, usize>,
        groups: &mut [FlowGroup],
    ) -> Option<usize> {
        let (id, info) = parse_node_token(raw);
        if id.is_empty() {
            return None;
        }
        if let Some(&idx) = index.get(&id) {
            if let Some((lbl, shape)) = info {
                nodes[idx].label = lbl;
                nodes[idx].shape = shape;
            }
            if nodes[idx].group.is_none()
                && let Some(g) = group
            {
                nodes[idx].group = Some(g);
                if !groups[g].nodes.contains(&idx) {
                    groups[g].nodes.push(idx);
                }
            }
            return Some(idx);
        }
        let (label, shape) = info.unwrap_or_else(|| (id.clone(), NodeShape::Rounded));
        let idx = nodes.len();
        nodes.push(FlowNode {
            id: id.clone(),
            label,
            shape,
            group,
            rank: 0,
            order: 0,
            x: 0.0,
            y: 0.0,
            w: 0.0,
            h: 0.0,
        });
        index.insert(id, idx);
        if let Some(g) = group {
            groups[g].nodes.push(idx);
        }
        Some(idx)
    }

    for raw in source.lines() {
        let line = raw.trim();
        if line.is_empty() || line.starts_with("%%") {
            continue;
        }
        let lower = line.to_ascii_lowercase();
        if lower.starts_with("flowchart ") || lower.starts_with("graph ") {
            if lower.contains(" lr") || lower.contains(" rl") {
                horizontal = true;
            } else if lower.contains(" td") || lower.contains(" tb") || lower.contains(" bt") {
                horizontal = false;
            }
            continue;
        }
        if lower.starts_with("statediagram") || lower.starts_with("direction ") {
            if lower.contains("lr") || lower.contains("rl") {
                horizontal = true;
            }
            continue;
        }
        if lower.starts_with("classdef ") || lower.starts_with("style ") || lower.starts_with("linkstyle ") {
            continue;
        }
        if let Some(rest) = line.strip_prefix("subgraph ") {
            let title = rest
                .split_once('[')
                .map(|(_, r)| r.trim_end_matches(']').trim())
                .unwrap_or(rest.trim())
                .trim_matches('"')
                .to_owned();
            let gi = groups.len();
            groups.push(FlowGroup {
                title,
                nodes: Vec::new(),
            });
            group_stack.push(gi);
            continue;
        }
        if lower == "end" {
            group_stack.pop();
            continue;
        }

        let current_group = group_stack.last().copied();
        if let Some((left, right, label, dashed, thick)) = split_flow_edge(line) {
            let a = ensure_node(&left, current_group, &mut nodes, &mut index, &mut groups);
            let b = ensure_node(&right, current_group, &mut nodes, &mut index, &mut groups);
            if let (Some(a), Some(b)) = (a, b) {
                edges.push(FlowEdge {
                    from: a,
                    to: b,
                    label,
                    dashed,
                    thick,
                });
            }
        } else if line.contains('[') || line.contains('(') || line.contains('{') {
            ensure_node(line, current_group, &mut nodes, &mut index, &mut groups);
        }
    }

    if nodes.is_empty() {
        return None;
    }

    // Topological longest-path rank assignment (cycle-safe)
    let n = nodes.len();
    let mut indeg = vec![0usize; n];
    let mut adj: Vec<Vec<usize>> = vec![Vec::new(); n];
    for e in &edges {
        if e.from != e.to {
            adj[e.from].push(e.to);
            indeg[e.to] += 1;
        }
    }
    let mut q: VecDeque<usize> = (0..n).filter(|&i| indeg[i] == 0).collect();
    if q.is_empty() {
        q.push_back(0);
    }
    let mut visited = HashSet::new();
    while let Some(u) = q.pop_front() {
        if !visited.insert(u) {
            continue;
        }
        let next_rank = nodes[u].rank + 1;
        for &v in &adj[u] {
            if !visited.contains(&v) {
                nodes[v].rank = nodes[v].rank.max(next_rank);
                indeg[v] = indeg[v].saturating_sub(1);
                if indeg[v] == 0 {
                    q.push_back(v);
                }
            }
        }
    }

    // Measure node sizes
    for node in &mut nodes {
        let tw = approx_text_width(&truncate_chars(&node.label, 24), 12.0);
        match node.shape {
            NodeShape::Circle => {
                let d = (tw + 20.0).clamp(32.0, 72.0);
                node.w = d;
                node.h = d;
            }
            NodeShape::Diamond => {
                node.w = (tw + 44.0).clamp(88.0, 190.0);
                node.h = 52.0;
            }
            _ => {
                node.w = (tw + 28.0).clamp(76.0, 200.0);
                node.h = 38.0;
            }
        }
    }

    let max_rank = nodes.iter().map(|n| n.rank).max().unwrap_or(0);
    let mut layers: Vec<Vec<usize>> = vec![Vec::new(); max_rank + 1];
    for (i, node) in nodes.iter().enumerate() {
        layers[node.rank].push(i);
    }
    for layer in &mut layers {
        for (ord, &idx) in layer.iter().enumerate() {
            nodes[idx].order = ord;
        }
    }

    let pad = 28.0f32;
    let gap_main = 56.0f32;
    let gap_cross = 26.0f32;
    let mut width = 0.0f32;
    let mut height = 0.0f32;

    if horizontal {
        let mut x = pad;
        let max_layer_h: f32 = layers
            .iter()
            .map(|l| {
                l.iter().map(|&i| nodes[i].h).sum::<f32>()
                    + gap_cross * (l.len().saturating_sub(1)) as f32
            })
            .fold(0.0f32, f32::max);
        for layer in &layers {
            if layer.is_empty() {
                continue;
            }
            let col_w = layer.iter().map(|&i| nodes[i].w).fold(0.0f32, f32::max);
            let layer_h: f32 = layer.iter().map(|&i| nodes[i].h).sum::<f32>()
                + gap_cross * (layer.len().saturating_sub(1)) as f32;
            let mut y = pad + (max_layer_h - layer_h) * 0.5;
            for &i in layer {
                nodes[i].x = x + col_w * 0.5;
                nodes[i].y = y + nodes[i].h * 0.5;
                y += nodes[i].h + gap_cross;
            }
            x += col_w + gap_main;
        }
        width = (x - gap_main + pad).max(260.0);
        height = (max_layer_h + pad * 2.0).max(130.0);
    } else {
        let mut y = pad;
        let max_layer_w: f32 = layers
            .iter()
            .map(|l| {
                l.iter().map(|&i| nodes[i].w).sum::<f32>()
                    + gap_cross * (l.len().saturating_sub(1)) as f32
            })
            .fold(0.0f32, f32::max);
        for layer in &layers {
            if layer.is_empty() {
                continue;
            }
            let row_h = layer.iter().map(|&i| nodes[i].h).fold(0.0f32, f32::max);
            let layer_w: f32 = layer.iter().map(|&i| nodes[i].w).sum::<f32>()
                + gap_cross * (layer.len().saturating_sub(1)) as f32;
            let mut x = pad + (max_layer_w - layer_w) * 0.5;
            for &i in layer {
                nodes[i].x = x + nodes[i].w * 0.5;
                nodes[i].y = y + row_h * 0.5;
                x += nodes[i].w + gap_cross;
            }
            y += row_h + gap_main;
        }
        width = (max_layer_w + pad * 2.0).max(280.0);
        height = (y - gap_main + pad).max(140.0);
    }

    Some(FlowGraph {
        horizontal,
        nodes,
        edges,
        groups,
        width,
        height,
    })
}

fn parse_sequence_diagram(source: &str, default_num: bool) -> Option<SeqDiagram> {
    let mut participants: Vec<String> = Vec::new();
    let mut p_idx: HashMap<String, usize> = HashMap::new();
    let mut messages: Vec<SeqMessage> = Vec::new();
    let mut autonumber = default_num;

    let mut ensure_p = |raw: &str| -> usize {
        let name = raw.trim().trim_matches('"').to_owned();
        if let Some(&idx) = p_idx.get(&name) {
            return idx;
        }
        let idx = participants.len();
        participants.push(name.clone());
        p_idx.insert(name, idx);
        idx
    };

    for raw in source.lines() {
        let line = raw.trim();
        if line.is_empty() || line.starts_with("%%") {
            continue;
        }
        let lower = line.to_ascii_lowercase();
        if lower == "sequencediagram" {
            continue;
        }
        if lower == "autonumber" {
            autonumber = true;
            continue;
        }
        if let Some(rest) = line
            .strip_prefix("participant ")
            .or_else(|| line.strip_prefix("actor "))
        {
            let name = rest.split(" as ").next().unwrap_or(rest).trim();
            if !name.is_empty() {
                ensure_p(name);
            }
            continue;
        }
        for (arrow, dashed) in [("-->>", true), ("->>", false), ("-->", true), ("->", false)] {
            if let Some(pos) = line.find(arrow) {
                let left = line[..pos].trim();
                let rest = line[pos + arrow.len()..].trim();
                let (right, label) = match rest.split_once(':') {
                    Some((r, l)) => (r.trim(), l.trim().to_owned()),
                    None => (rest.trim(), String::new()),
                };
                if !left.is_empty() && !right.is_empty() {
                    let from = ensure_p(left);
                    let to = ensure_p(right);
                    messages.push(SeqMessage {
                        from,
                        to,
                        label,
                        dashed,
                    });
                }
                break;
            }
        }
    }

    if participants.is_empty() {
        return None;
    }
    let col_w = 152.0f32;
    let width = (participants.len() as f32 * col_w + 48.0).max(320.0);
    let height = (72.0 + messages.len().max(1) as f32 * 44.0 + 36.0).max(160.0);
    Some(SeqDiagram {
        participants,
        messages,
        autonumber,
        width,
        height,
    })
}

fn parse_entity_diagram(source: &str) -> Option<EntityDiagram> {
    let mut entities: Vec<BoxEntity> = Vec::new();
    let mut idx_map: HashMap<String, usize> = HashMap::new();
    let mut relations: Vec<(usize, usize, String)> = Vec::new();
    let mut current: Option<usize> = None;

    fn ensure_ent(
        name: &str,
        entities: &mut Vec<BoxEntity>,
        idx_map: &mut HashMap<String, usize>,
    ) -> usize {
        let key = name.trim().trim_matches('"').to_owned();
        if let Some(&i) = idx_map.get(&key) {
            return i;
        }
        let i = entities.len();
        entities.push(BoxEntity {
            name: key.clone(),
            members: Vec::new(),
        });
        idx_map.insert(key, i);
        i
    }

    for raw in source.lines() {
        let line = raw.trim();
        if line.is_empty() || line.starts_with("%%") {
            continue;
        }
        let lower = line.to_ascii_lowercase();
        if lower == "erdiagram" || lower == "classdiagram" {
            continue;
        }
        if line == "}" {
            current = None;
            continue;
        }
        if let Some(ei) = current {
            entities[ei].members.push(line.to_owned());
            continue;
        }
        if let Some(head) = line.strip_suffix('{') {
            let name = head
                .trim()
                .strip_prefix("class ")
                .unwrap_or(head.trim())
                .trim();
            if !name.is_empty() {
                current = Some(ensure_ent(name, &mut entities, &mut idx_map));
            }
            continue;
        }
        // Relationship lines: A ||--o{ B : label or A <|-- B : label
        for tok in ["||--o{", "}o--||", "||--|{", "}|--||", "||--||", "<|--", "--|>", "-->", "--"] {
            if let Some(pos) = line.find(tok) {
                let left = line[..pos].trim();
                let rest = line[pos + tok.len()..].trim();
                let (right, label) = match rest.split_once(':') {
                    Some((r, l)) => (r.trim(), l.trim().trim_matches('"').to_owned()),
                    None => (rest.trim(), String::new()),
                };
                if !left.is_empty() && !right.is_empty() {
                    let a = ensure_ent(left, &mut entities, &mut idx_map);
                    let b = ensure_ent(right, &mut entities, &mut idx_map);
                    relations.push((a, b, label));
                }
                break;
            }
        }
    }

    if entities.is_empty() {
        return None;
    }
    let cols = entities.len().min(3).max(1);
    let rows = entities.len().div_ceil(cols);
    let max_members = entities.iter().map(|e| e.members.len()).max().unwrap_or(0);
    let box_h = 34.0 + (max_members.min(8) as f32) * 18.0 + 12.0;
    let width = (cols as f32 * 210.0 + 36.0).max(300.0);
    let height = (rows as f32 * (box_h + 40.0) + 32.0).max(150.0);
    Some(EntityDiagram {
        entities,
        relations,
        width,
        height,
    })
}

fn parse_mermaid_pie(source: &str) -> Option<ChartSpec> {
    let mut title = "占比分布".to_owned();
    let mut labels = Vec::new();
    let mut data = Vec::new();
    for raw in source.lines() {
        let line = raw.trim();
        if line.is_empty() || line.starts_with("%%") {
            continue;
        }
        if let Some(rest) = line.strip_prefix("pie") {
            if let Some(t) = rest.trim().strip_prefix("title ") {
                title = t.trim().trim_matches('"').to_owned();
            }
            continue;
        }
        if let Some(t) = line.strip_prefix("title ") {
            title = t.trim().trim_matches('"').to_owned();
            continue;
        }
        if let Some((k, v)) = line.split_once(':')
            && let Ok(num) = v.trim().parse::<f32>()
        {
            labels.push(k.trim().trim_matches('"').to_owned());
            data.push(num);
        }
    }
    if labels.is_empty() {
        return None;
    }
    Some(ChartSpec {
        kind: "pie".into(),
        title,
        subtitle: String::new(),
        hero: String::new(),
        hero_label: String::new(),
        badge: String::new(),
        badge_tone: "accent".into(),
        delta: String::new(),
        style: String::new(),
        unit: String::new(),
        unit_right: String::new(),
        center_label: String::new(),
        labels,
        highlights: Vec::new(),
        series: vec![ChartSeries {
            name: String::new(),
            subtitle: String::new(),
            badge: String::new(),
            kind: String::new(),
            data,
            sparkline: Vec::new(),
            points: Vec::new(),
        }],
        ref_lines: Vec::new(),
        items: Vec::new(),
        height: 230.0,
    })
}

fn parse_diagram(kind: &str, args: &str, source: &str) -> ParsedDiagram {
    let first = source
        .lines()
        .map(str::trim)
        .find(|l| !l.is_empty() && !l.starts_with("%%"))
        .unwrap_or("")
        .to_ascii_lowercase();
    let horizontal_hint = args.to_ascii_lowercase().contains("lr");
    let num_hint = args.to_ascii_lowercase().contains("num");

    if kind == "sequence" || first.starts_with("sequencediagram") {
        return parse_sequence_diagram(source, num_hint)
            .map(ParsedDiagram::Sequence)
            .unwrap_or(ParsedDiagram::Unsupported);
    }
    if kind == "er"
        || kind == "class"
        || first.starts_with("erdiagram")
        || first.starts_with("classdiagram")
    {
        return parse_entity_diagram(source)
            .map(ParsedDiagram::Entities)
            .unwrap_or(ParsedDiagram::Unsupported);
    }
    if first.starts_with("pie") {
        return parse_mermaid_pie(source)
            .map(ParsedDiagram::Pie)
            .unwrap_or(ParsedDiagram::Unsupported);
    }
    if kind == "flow"
        || kind == "state"
        || first.starts_with("flowchart")
        || first.starts_with("graph")
        || first.starts_with("statediagram")
    {
        return parse_flow_graph(source, horizontal_hint)
            .map(ParsedDiagram::Flow)
            .unwrap_or(ParsedDiagram::Unsupported);
    }
    ParsedDiagram::Unsupported
}

fn draw_arrowhead(p: Painter, tip_x: f32, tip_y: f32, dx: f32, dy: f32, col: Hsla) -> Painter {
    let len = (dx * dx + dy * dy).sqrt().max(1e-4);
    let ux = dx / len;
    let uy = dy / len;
    let size = 7.5f32;
    let spread = 4.2f32;
    let bx = tip_x - ux * size;
    let by = tip_y - uy * size;
    let px_v = -uy * spread;
    let py_v = ux * spread;
    p.fill_color(col)
        .begin_path()
        .move_to(tip_x, tip_y)
        .line_to(bx + px_v, by + py_v)
        .line_to(bx - px_v, by - py_v)
        .close_path()
        .fill()
}

fn paint_flow_graph(g: &FlowGraph) -> Painter {
    let colors = palette();
    let mut p = Painter::new();

    // 1. Subgraph containers
    for grp in &g.groups {
        if grp.nodes.is_empty() {
            continue;
        }
        let mut x0 = f32::INFINITY;
        let mut y0 = f32::INFINITY;
        let mut x1 = f32::NEG_INFINITY;
        let mut y1 = f32::NEG_INFINITY;
        for &ni in &grp.nodes {
            let n = &g.nodes[ni];
            x0 = x0.min(n.x - n.w * 0.5);
            y0 = y0.min(n.y - n.h * 0.5);
            x1 = x1.max(n.x + n.w * 0.5);
            y1 = y1.max(n.y + n.h * 0.5);
        }
        let rect = Bounds::new(
            point(px(x0 - 12.0), px(y0 - 24.0)),
            size(px(x1 - x0 + 24.0), px(y1 - y0 + 36.0)),
        );
        p = p
            .fill_color(hsla(colors.soft.opacity(0.6)))
            .stroke_color(hsla(colors.line))
            .line_width(1.0)
            .begin_path()
            .round_rect(rect, px(10.0))
            .fill()
            .stroke()
            .font("600 10.5px sans-serif")
            .text_align(CanvasTextAlign::Left)
            .text_baseline(CanvasTextBaseline::Top)
            .fill_color(hsla(colors.muted))
            .fill_text(&grp.title, x0 - 4.0, y0 - 19.0);
    }

    // 2. Edges
    let edge_col = hsla(colors.muted);
    for e in &g.edges {
        let a = &g.nodes[e.from];
        let b = &g.nodes[e.to];
        let (x1, y1, x2, y2) = if g.horizontal {
            if b.x >= a.x {
                (a.x + a.w * 0.5, a.y, b.x - b.w * 0.5, b.y)
            } else {
                (a.x - a.w * 0.5, a.y, b.x + b.w * 0.5, b.y)
            }
        } else if b.y >= a.y {
            (a.x, a.y + a.h * 0.5, b.x, b.y - b.h * 0.5)
        } else {
            (a.x, a.y - a.h * 0.5, b.x, b.y + b.h * 0.5)
        };
        let mx = (x1 + x2) * 0.5;
        let my = (y1 + y2) * 0.5;
        p = p
            .save()
            .stroke_color(edge_col)
            .line_width(if e.thick { 2.4 } else { 1.4 });
        if e.dashed {
            p = p.line_dash(&[px(5.0), px(4.0)]);
        }
        p = p.begin_path().move_to(x1, y1);
        if g.horizontal {
            p = p.bezier_curve_to(point(px(mx), px(y1)), point(px(mx), px(y2)), point(px(x2), px(y2)));
        } else {
            p = p.bezier_curve_to(point(px(x1), px(my)), point(px(x2), px(my)), point(px(x2), px(y2)));
        }
        p = p.stroke().restore();
        let (adx, ady) = if g.horizontal {
            (if x2 >= x1 { 1.0 } else { -1.0 }, 0.0)
        } else {
            (0.0, if y2 >= y1 { 1.0 } else { -1.0 })
        };
        p = draw_arrowhead(p, x2, y2, adx, ady, edge_col);
        if !e.label.is_empty() {
            let lbl = truncate_chars(&e.label, 18);
            let lw = approx_text_width(&lbl, 10.0) + 10.0;
            let pill = Bounds::new(point(px(mx - lw * 0.5), px(my - 9.0)), size(px(lw), px(18.0)));
            p = p
                .fill_color(hsla(colors.card))
                .stroke_color(hsla(colors.line_soft))
                .line_width(1.0)
                .begin_path()
                .round_rect(pill, px(5.0))
                .fill()
                .stroke()
                .font("10px sans-serif")
                .text_align(CanvasTextAlign::Center)
                .text_baseline(CanvasTextBaseline::Middle)
                .fill_color(hsla(colors.muted))
                .fill_text(&lbl, mx, my);
        }
    }

    // 3. Nodes
    for (i, n) in g.nodes.iter().enumerate() {
        let fill = hsla(colors.raised);
        let stroke = hsla(if i == 0 { colors.accent } else { colors.line });
        let rect = Bounds::new(
            point(px(n.x - n.w * 0.5), px(n.y - n.h * 0.5)),
            size(px(n.w), px(n.h)),
        );
        p = p.fill_color(fill).stroke_color(stroke).line_width(1.4);
        match n.shape {
            NodeShape::Diamond => {
                p = p
                    .begin_path()
                    .move_to(n.x, n.y - n.h * 0.5)
                    .line_to(n.x + n.w * 0.5, n.y)
                    .line_to(n.x, n.y + n.h * 0.5)
                    .line_to(n.x - n.w * 0.5, n.y)
                    .close_path()
                    .fill()
                    .stroke();
            }
            NodeShape::Circle => {
                p = p
                    .begin_path()
                    .arc(n.x, n.y, n.w * 0.5, 0.0, TAU)
                    .fill()
                    .stroke();
            }
            NodeShape::Stadium => {
                p = p
                    .begin_path()
                    .round_rect(rect, px(n.h * 0.5))
                    .fill()
                    .stroke();
            }
            NodeShape::Rect => {
                p = p.begin_path().round_rect(rect, px(4.0)).fill().stroke();
            }
            NodeShape::Rounded => {
                p = p.begin_path().round_rect(rect, px(9.0)).fill().stroke();
            }
        }
        p = p
            .font("500 11.5px sans-serif")
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Middle)
            .fill_color(hsla(colors.ink))
            .fill_text_max_width(&truncate_chars(&n.label, 24), n.x, n.y, px(n.w - 14.0));
    }

    p
}

fn paint_sequence_diagram(s: &SeqDiagram) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let col_w = (s.width - 48.0) / s.participants.len().max(1) as f32;
    let x_of = |idx: usize| -> f32 { 24.0 + (idx as f32 + 0.5) * col_w };
    let header_y = 16.0f32;
    let header_h = 32.0f32;

    // Lifelines + participant headers
    for (i, name) in s.participants.iter().enumerate() {
        let cx = x_of(i);
        let bw = (approx_text_width(&truncate_chars(name, 16), 11.5) + 24.0).clamp(72.0, col_w - 14.0);
        p = p
            .save()
            .stroke_color(hsla(colors.line))
            .line_width(1.1)
            .line_dash(&[px(4.0), px(4.0)])
            .begin_path()
            .move_to(cx, header_y + header_h)
            .line_to(cx, s.height - 16.0)
            .stroke()
            .restore();
        let rect = Bounds::new(point(px(cx - bw * 0.5), px(header_y)), size(px(bw), px(header_h)));
        p = p
            .fill_color(hsla(colors.raised))
            .stroke_color(hsla(series_color(i)))
            .line_width(1.4)
            .begin_path()
            .round_rect(rect, px(7.0))
            .fill()
            .stroke()
            .font("600 11.5px sans-serif")
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Middle)
            .fill_color(hsla(colors.ink))
            .fill_text_max_width(&truncate_chars(name, 16), cx, header_y + header_h * 0.5, px(bw - 10.0));
    }

    // Messages
    let msg_col = hsla(colors.ink);
    for (i, m) in s.messages.iter().enumerate() {
        let y = 76.0 + i as f32 * 44.0;
        let x1 = x_of(m.from);
        let x2 = x_of(m.to);
        let label = if s.autonumber {
            format!("{}. {}", i + 1, m.label)
        } else {
            m.label.clone()
        };
        if m.from == m.to {
            p = p
                .save()
                .stroke_color(msg_col)
                .line_width(1.3)
                .begin_path()
                .move_to(x1, y - 6.0)
                .line_to(x1 + 36.0, y - 6.0)
                .line_to(x1 + 36.0, y + 12.0)
                .line_to(x1, y + 12.0)
                .stroke()
                .restore();
            p = draw_arrowhead(p, x1, y + 12.0, -1.0, 0.0, msg_col);
            p = p
                .font("10.5px sans-serif")
                .text_align(CanvasTextAlign::Left)
                .text_baseline(CanvasTextBaseline::Middle)
                .fill_color(hsla(colors.muted))
                .fill_text(&truncate_chars(&label, 22), x1 + 42.0, y + 3.0);
        } else {
            p = p.save().stroke_color(msg_col).line_width(1.4);
            if m.dashed {
                p = p.line_dash(&[px(5.0), px(4.0)]);
            }
            p = p
                .begin_path()
                .move_to(x1, y)
                .line_to(x2, y)
                .stroke()
                .restore();
            p = draw_arrowhead(p, x2, y, x2 - x1, 0.0, msg_col);
            p = p
                .font("10.5px sans-serif")
                .text_align(CanvasTextAlign::Center)
                .text_baseline(CanvasTextBaseline::Bottom)
                .fill_color(hsla(colors.muted))
                .fill_text(&truncate_chars(&label, 30), (x1 + x2) * 0.5, y - 5.0);
        }
    }
    p
}

fn paint_entity_diagram(d: &EntityDiagram) -> Painter {
    let colors = palette();
    let mut p = Painter::new();
    let cols = d.entities.len().min(3).max(1);
    let box_w = 186.0f32;
    let gap_x = 24.0f32;
    let gap_y = 32.0f32;
    let max_m = d.entities.iter().map(|e| e.members.len()).max().unwrap_or(0).min(8);
    let box_h = 34.0 + max_m as f32 * 18.0 + 10.0;

    let centers: Vec<(f32, f32)> = (0..d.entities.len())
        .map(|i| {
            let col = i % cols;
            let row = i / cols;
            let x = 18.0 + col as f32 * (box_w + gap_x) + box_w * 0.5;
            let y = 18.0 + row as f32 * (box_h + gap_y) + box_h * 0.5;
            (x, y)
        })
        .collect();

    for &(a, b, ref lbl) in &d.relations {
        let (x1, y1) = centers[a];
        let (x2, y2) = centers[b];
        p = p
            .stroke_color(hsla(colors.muted))
            .line_width(1.3)
            .begin_path()
            .move_to(x1, y1)
            .line_to(x2, y2)
            .stroke();
        if !lbl.is_empty() {
            p = p
                .font("10px sans-serif")
                .text_align(CanvasTextAlign::Center)
                .text_baseline(CanvasTextBaseline::Middle)
                .fill_color(hsla(colors.faint))
                .fill_text(&truncate_chars(lbl, 16), (x1 + x2) * 0.5, (y1 + y2) * 0.5 - 6.0);
        }
    }

    for (i, ent) in d.entities.iter().enumerate() {
        let (cx, cy) = centers[i];
        let x0 = cx - box_w * 0.5;
        let y0 = cy - box_h * 0.5;
        let rect = Bounds::new(point(px(x0), px(y0)), size(px(box_w), px(box_h)));
        let head = Bounds::new(point(px(x0), px(y0)), size(px(box_w), px(28.0)));
        p = p
            .fill_color(hsla(colors.raised))
            .stroke_color(hsla(colors.line))
            .line_width(1.2)
            .begin_path()
            .round_rect(rect, px(8.0))
            .fill()
            .stroke()
            .fill_color(hsla(series_color(i).opacity(0.16)))
            .begin_path()
            .round_rect(head, px(8.0))
            .fill()
            .font("600 11.5px sans-serif")
            .text_align(CanvasTextAlign::Center)
            .text_baseline(CanvasTextBaseline::Middle)
            .fill_color(hsla(colors.ink))
            .fill_text_max_width(&truncate_chars(&ent.name, 20), cx, y0 + 14.0, px(box_w - 12.0));
        for (mi, member) in ent.members.iter().take(8).enumerate() {
            p = p
                .font("10.5px Menlo")
                .text_align(CanvasTextAlign::Left)
                .text_baseline(CanvasTextBaseline::Middle)
                .fill_color(hsla(colors.muted))
                .fill_text_max_width(
                    &truncate_chars(member, 24),
                    x0 + 10.0,
                    y0 + 38.0 + mi as f32 * 18.0,
                    px(box_w - 20.0),
                );
        }
    }
    p
}

pub(crate) fn render_diagram_card(kind: &str, args: &str, source: &str) -> impl IntoElement {
    let colors = palette();
    let label = match kind {
        "mermaid" => "Mermaid 图表",
        "flow" => "流程图",
        "state" => "状态图",
        "sequence" => "时序图",
        "er" => "ER 图",
        "class" => "类图",
        _ => "图表",
    };
    let parsed = parse_diagram(kind, args, source);
    let can_render = !matches!(parsed, ParsedDiagram::Unsupported);
    let show_source = signal(!can_render);
    let expanded = signal(false);
    let copy_src = source.to_owned();
    let raw_src = source.trim().to_owned();

    let header = view()
        .w_full()
        .min_w_0()
        .flex_row()
        .items_center()
        .gap_2()
        .px_3()
        .py_1p5()
        .bg(colors.soft)
        .border_b_1()
        .border_b_color(colors.line_soft)
        .child(badge(kind.to_owned(), colors.accent))
        .child(
            text(label.to_owned())
                .flex_1()
                .min_w_0()
                .font_weight(FontWeight::SEMIBOLD)
                .text_size(px(12.5))
                .text_color(colors.ink),
        )
        .child(when(can_render, move || {
            link_button(
                move || if show_source.get() { "图表" } else { "源码" },
                move || show_source.set(!show_source.get_untracked()),
            )
        }))
        .child(when(can_render, move || {
            nsh_base::button()
                .accessibility_label("放大图表")
                .on_press(move || expanded.set(true))
                .p_1()
                .rounded(px(5.0))
                .hover(|s| s.bg(colors.card))
                .child(icon(Icon::Maximize2, 12.5, colors.muted))
        }))
        .child(link_button("复制", move || controller::copy(copy_src.clone())));

    let paint_parsed = |d: &ParsedDiagram| -> (Painter, f32, f32) {
        match d {
            ParsedDiagram::Flow(g) => (paint_flow_graph(g), g.width, g.height),
            ParsedDiagram::Sequence(s) => (paint_sequence_diagram(s), s.width, s.height),
            ParsedDiagram::Pie(spec) => (paint_pie_chart(spec, 560.0, 230.0, false), 560.0, 230.0),
            ParsedDiagram::Entities(e) => (paint_entity_diagram(e), e.width, e.height),
            ParsedDiagram::Unsupported => (Painter::new(), 1.0, 1.0),
        }
    };

    let parsed_inline = parsed.clone();
    let parsed_modal = parsed;

    let body = view()
        .w_full()
        .min_w_0()
        .flex_col()
        .child(when(move || expanded.get(), move || {
            let (painter, w, h) = paint_parsed(&parsed_modal);
            nsh_base::dialog()
                .animation(ModalAnimation::Fade)
                .on_cancel(move || expanded.set(false))
                .child(
                    view()
                        .flex_1()
                        .items_center()
                        .justify_center()
                        .bg(colors.scrim)
                        .p_6()
                        .on_press(move || expanded.set(false))
                        .child(
                            view()
                                .flex_col()
                                .gap_2()
                                .p_4()
                                .rounded(px(14.0))
                                .bg(colors.card)
                                .border_1()
                                .border_color(colors.line)
                                .shadow_xl()
                                .child(
                                    view()
                                        .flex_row()
                                        .items_center()
                                        .gap_2()
                                        .child(
                                            text(label.to_owned())
                                                .flex_1()
                                                .font_weight(FontWeight::SEMIBOLD)
                                                .text_size(px(14.0))
                                                .text_color(colors.ink),
                                        )
                                        .child(icon_button(Icon::X, "关闭", move || {
                                            expanded.set(false)
                                        })),
                                )
                                .child(
                                    scroll()
                                        .horizontal()
                                        .child(canvas(painter).w(px(w)).h(px(h))),
                                ),
                        ),
                )
        }))
        .child(dynamic(move || {
            if show_source.get() || matches!(parsed_inline, ParsedDiagram::Unsupported) {
                return scroll()
                    .w_full()
                    .min_w_0()
                    .horizontal()
                    .child(
                        text(raw_src.clone())
                            .select_text()
                            .font_family(MONO)
                            .text_size(px(12.0))
                            .line_height(LineHeight::Multiple(1.5))
                            .text_color(colors.ink)
                            .px_3()
                            .py_2(),
                    )
                    .into_element();
            }
            let (painter, w, h) = paint_parsed(&parsed_inline);
            scroll()
                .w_full()
                .min_w_0()
                .horizontal()
                .child(view().p_2().child(canvas(painter).w(px(w)).h(px(h))))
                .into_element()
        }));

    super::visuals::card_shell(header, body)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_multi_series_pipe_chart_and_highlights() {
        let src = "\
          | p50 | p95\n\
* Redis   | 12  | 40\n\
Memcached | 9   | 31\n\
Postgres  | 28  | 95\n";
        let spec = parse_chart_spec("bar unit=ms title=\"延迟对比\" ref=100", src).unwrap();
        assert_eq!(spec.kind, "bar");
        assert_eq!(spec.title, "延迟对比");
        assert_eq!(spec.unit, "ms");
        assert_eq!(spec.labels, vec!["Redis", "Memcached", "Postgres"]);
        assert_eq!(spec.highlights, vec![true, false, false]);
        assert_eq!(spec.series.len(), 2);
        assert_eq!(spec.series[0].name, "p50");
        assert_eq!(spec.series[0].data, vec![12.0, 9.0, 28.0]);
        assert_eq!(spec.series[1].name, "p95");
        assert_eq!(spec.series[1].data, vec![40.0, 31.0, 95.0]);
        assert_eq!(spec.ref_lines.len(), 1);
        assert_eq!(spec.ref_lines[0].value, 100.0);
    }

    #[test]
    fn parses_studio_json_chart_types() {
        let json = r#"{"type":"donut","title":"占比","centerLabel":"100%","labels":["A","B"],"series":[{"data":[60,40]}]}"#;
        let spec = parse_chart_spec("", json).unwrap();
        assert_eq!(spec.kind, "donut");
        assert_eq!(spec.center_label, "100%");
        assert_eq!(spec.series[0].data, vec![60.0, 40.0]);
    }

    #[test]
    fn parses_flowchart_sequence_and_entity_diagrams() {
        let flow = "flowchart LR\n  A[Start] -->|ok| B{Check}\n  B -.-> C((Done))";
        let ParsedDiagram::Flow(g) = parse_diagram("mermaid", "", flow) else {
            panic!("expected flow");
        };
        assert!(g.horizontal);
        assert_eq!(g.nodes.len(), 3);
        assert_eq!(g.edges.len(), 2);
        assert_eq!(g.edges[0].label, "ok");
        assert!(g.edges[1].dashed);

        let seq = "sequenceDiagram\n  autonumber\n  Client->>Core: submit\n  Core-->>Client: stream";
        let ParsedDiagram::Sequence(s) = parse_diagram("mermaid", "", seq) else {
            panic!("expected sequence");
        };
        assert!(s.autonumber);
        assert_eq!(s.participants, vec!["Client", "Core"]);
        assert_eq!(s.messages.len(), 2);

        let er = "erDiagram\n  USER ||--o{ ORDER : places";
        let ParsedDiagram::Entities(e) = parse_diagram("er", "", er) else {
            panic!("expected entities");
        };
        assert_eq!(e.entities.len(), 2);
        assert_eq!(e.relations.len(), 1);
    }
}
