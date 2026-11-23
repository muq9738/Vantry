package desktop

import (
	"encoding/csv"
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
)

func chartSpecToCSV(spec *chartSpec) string {
	if spec == nil {
		return ""
	}
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	switch spec.Type {
	case "stats":
		_ = w.Write([]string{"label", "value", "delta", "note"})
		for _, it := range spec.Items {
			_ = w.Write([]string{it.Label, it.Value, it.Delta, it.Note})
		}
	case "breakdown":
		_ = w.Write([]string{"series", "subtitle", "value", "badge"})
		for sIdx, s := range spec.Series {
			_ = w.Write([]string{
				seriesName(spec, sIdx),
				s.Subtitle,
				strconv.FormatFloat(breakdownSeriesTotal(s), 'f', -1, 64),
				s.Badge,
			})
		}
	case "scatter", "bubble":
		_ = w.Write([]string{"series", "label", "x", "y", "r"})
		for sIdx, s := range spec.Series {
			name := seriesName(spec, sIdx)
			for _, pt := range s.Points {
				_ = w.Write([]string{
					name,
					pt.Label,
					strconv.FormatFloat(pt.X, 'f', -1, 64),
					strconv.FormatFloat(pt.Y, 'f', -1, 64),
					strconv.FormatFloat(pt.R, 'f', -1, 64),
				})
			}
		}
	default:
		header := make([]string, 0, len(spec.Series)+1)
		header = append(header, "category")
		for i := range spec.Series {
			header = append(header, seriesName(spec, i))
		}
		_ = w.Write(header)
		for i, label := range spec.Labels {
			row := make([]string, 0, len(spec.Series)+1)
			row = append(row, label)
			for s := range spec.Series {
				v := math.NaN()
				if i < len(spec.Series[s].Data) {
					v = spec.Series[s].Data[i]
				}
				if math.IsNaN(v) {
					row = append(row, "")
				} else {
					row = append(row, strconv.FormatFloat(v, 'f', -1, 64))
				}
			}
			_ = w.Write(row)
		}
	}
	w.Flush()
	return sb.String()
}

func svgHex(c ui.Color) string {
	return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
}

func svgRGBA(c ui.Color, alpha float64) string {
	return fmt.Sprintf("rgba(%d,%d,%d,%.3f)", c.R, c.G, c.B, alpha)
}

// chartSpecToSVG exports any L1 chartSpec into a standalone, theme-faithful
// vector SVG document suitable for saving or rasterizing to PNG.
func chartSpecToSVG(spec *chartSpec, p palette) string {
	if spec == nil {
		return ""
	}
	const width = 760.0
	height := float64(max(spec.Height, 240)) + 90
	if spec.Type == "hbar" {
		height = math.Max(height, float64(len(spec.Labels))*34+110)
	}
	if spec.Type == "heatmap" {
		height = math.Max(height, float64(len(spec.Series))*36+120)
	}
	if spec.Type == "breakdown" {
		height = math.Max(height, float64(len(spec.Series))*52+130)
	}
	if spec.Type == "stats" {
		height = 220
	}

	colors := chartColors(p)
	bg := svgHex(p.raised)
	ink := svgHex(p.ink)
	muted := svgHex(p.muted)
	line := svgHex(p.lineSoft)
	border := svgHex(p.line)

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:%s">`,
		width, height, width, height, bg)
	fmt.Fprintf(&sb, `<rect x="0.5" y="0.5" width="%.0f" height="%.0f" rx="12" fill="%s" stroke="%s"/>`,
		width-1, height-1, bg, border)

	topY := 28.0
	if spec.HeroLabel != "" || spec.Title != "" {
		heading := nonEmptyString(spec.HeroLabel, spec.Title)
		fmt.Fprintf(&sb, `<text x="24" y="34" font-size="15" font-weight="600" fill="%s">%s</text>`, ink, html.EscapeString(heading))
		topY = 52
	}
	if spec.Hero != "" {
		fmt.Fprintf(&sb, `<text x="24" y="%.0f" font-size="28" font-weight="700" fill="%s">%s</text>`, topY+24, ink, html.EscapeString(spec.Hero))
		topY += 36
	}
	if spec.Subtitle != "" || spec.Badge != "" || spec.Delta != "" {
		sub := strings.TrimSpace(strings.Join([]string{spec.Badge, spec.Delta, spec.Subtitle}, " · "))
		sub = strings.Trim(sub, " ·")
		fmt.Fprintf(&sb, `<text x="24" y="%.0f" font-size="12" fill="%s">%s</text>`, topY+4, muted, html.EscapeString(sub))
		topY += 20
	}

	padL, padR, padT, padB := 64.0, 28.0, topY+12, 46.0
	if spec.Type == "combo" {
		padR = 64.0
	}
	if spec.Type == "hbar" || spec.Type == "heatmap" {
		padL = 120.0
	}
	if len(spec.Series) > 1 && spec.Type != "pie" && spec.Type != "donut" {
		padB += 24
	}
	plotW := width - padL - padR
	plotH := height - padT - padB

	switch spec.Type {
	case "stats":
		n := max(1, len(spec.Items))
		cols := min(4, n)
		cardW := (width - 48 - float64(cols-1)*12) / float64(cols)
		for i, item := range spec.Items {
			col := i % cols
			row := i / cols
			cx := 24 + float64(col)*(cardW+12)
			cy := topY + 12 + float64(row)*92
			fmt.Fprintf(&sb, `<rect x="%.1f" y="%.1f" width="%.1f" height="80" rx="10" fill="%s" stroke="%s"/>`,
				cx, cy, cardW, svgHex(p.soft), border)
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="11" fill="%s">%s</text>`,
				cx+12, cy+22, muted, html.EscapeString(item.Label))
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="20" font-weight="700" fill="%s">%s</text>`,
				cx+12, cy+48, ink, html.EscapeString(item.Value))
			if item.Delta != "" {
				fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="11" fill="%s">%s</text>`,
					cx+12, cy+66, svgHex(p.accentInk), html.EscapeString(item.Delta))
			}
		}

	case "pie", "donut":
		total := 0.0
		vals := make([]float64, len(spec.Labels))
		for i, v := range spec.Series[0].Data {
			if !math.IsNaN(v) && v > 0 {
				vals[i] = v
				total += v
			}
		}
		if total > 0 {
			cx, cy := 180.0, padT+plotH/2
			outerR := math.Min(110, plotH*0.44)
			innerR := 0.0
			if spec.Type == "donut" {
				innerR = outerR * 0.56
			}
			angle := -math.Pi / 2
			for i, v := range vals {
				if v <= 0 {
					continue
				}
				span := (v / total) * 2 * math.Pi
				if span >= 2*math.Pi-1e-4 {
					span = 2*math.Pi - 1e-4
				}
				a0, a1 := angle, angle+span
				large := 0
				if span > math.Pi {
					large = 1
				}
				x0, y0 := cx+outerR*math.Cos(a0), cy+outerR*math.Sin(a0)
				x1, y1 := cx+outerR*math.Cos(a1), cy+outerR*math.Sin(a1)
				col := svgHex(colors[i%len(colors)])
				if innerR > 0 {
					ix0, iy0 := cx+innerR*math.Cos(a1), cy+innerR*math.Sin(a1)
					ix1, iy1 := cx+innerR*math.Cos(a0), cy+innerR*math.Sin(a0)
					fmt.Fprintf(&sb, `<path d="M%.2f %.2f A%.2f %.2f 0 %d 1 %.2f %.2f L%.2f %.2f A%.2f %.2f 0 %d 0 %.2f %.2f Z" fill="%s"/>`,
						x0, y0, outerR, outerR, large, x1, y1, ix0, iy0, innerR, innerR, large, ix1, iy1, col)
				} else {
					fmt.Fprintf(&sb, `<path d="M%.2f %.2f L%.2f %.2f A%.2f %.2f 0 %d 1 %.2f %.2f Z" fill="%s"/>`,
						cx, cy, x0, y0, outerR, outerR, large, x1, y1, col)
				}
				angle += span
			}
			if innerR > 0 {
				centerText := spec.CenterLabel
				if centerText == "" {
					centerText = formatChartValue(total, spec.Unit)
				}
				fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="14" font-weight="600" fill="%s">%s</text>`,
					cx, cy+5, ink, html.EscapeString(centerText))
			}
			ly := padT + 14
			for i, lbl := range spec.Labels {
				col := svgHex(colors[i%len(colors)])
				pct := vals[i] / total * 100
				fmt.Fprintf(&sb, `<rect x="340" y="%.1f" width="10" height="10" rx="3" fill="%s"/>`, ly-9, col)
				fmt.Fprintf(&sb, `<text x="358" y="%.1f" font-size="12" fill="%s">%s</text>`, ly, ink, html.EscapeString(lbl))
				fmt.Fprintf(&sb, `<text x="640" y="%.1f" text-anchor="end" font-size="12" fill="%s">%s</text>`, ly, ink, html.EscapeString(formatChartValue(vals[i], spec.Unit)))
				fmt.Fprintf(&sb, `<text x="720" y="%.1f" text-anchor="end" font-size="11" fill="%s">%.1f%%</text>`, ly, muted, pct)
				ly += 24
				if ly > height-24 {
					break
				}
			}
		}

	case "hbar":
		lo, hi := seriesExtent(spec, false)
		x := chart.NewLinear(lo, hi).Nice(4)
		for _, tick := range valueTicks(x, spec.Unit) {
			px := padL + float64(tick.At)*plotW
			fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, px, padT, px, padT+plotH, line)
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="%s">%s</text>`, px, padT+plotH+16, muted, html.EscapeString(tick.Label))
		}
		nRows := len(spec.Labels)
		nSer := len(spec.Series)
		rowH := plotH / float64(max(1, nRows))
		for i, lbl := range spec.Labels {
			y0 := padT + float64(i)*rowH
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="11" fill="%s">%s</text>`, padL-8, y0+rowH/2+4, ink, html.EscapeString(lbl))
			barH := (rowH * 0.72) / float64(max(1, nSer))
			for s := range spec.Series {
				v := spec.Series[s].Data[i]
				if math.IsNaN(v) {
					continue
				}
				sp := x.Span(0, v)
				bx := padL + float64(sp.Lo)*plotW
				bw := math.Max(1.5, float64(sp.Hi-sp.Lo)*plotW)
				by := y0 + rowH*0.14 + float64(s)*barH
				fmt.Fprintf(&sb, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="%s"/>`,
					bx, by, bw, math.Max(3, barH-2), svgHex(colors[s%len(colors)]))
			}
		}

	case "scatter", "bubble":
		xLo, xHi, yLo, yHi, _, rHi := scatterExtents(spec, nil)
		xs := chart.NewLinear(xLo, xHi).Nice(5)
		ys := chart.NewLinear(yLo, yHi).Nice(4)
		for _, tick := range ys.Ticks(4) {
			py := padT + (1-float64(tick.At))*plotH
			fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, padL, py, padL+plotW, py, line)
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="%s">%s</text>`, padL-8, py+3, muted, html.EscapeString(formatChartValue(tick.Value, spec.Unit)))
		}
		for _, tick := range xs.Ticks(5) {
			px := padL + float64(tick.At)*plotW
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="%s">%s</text>`, px, padT+plotH+16, muted, html.EscapeString(formatChartValue(tick.Value, "")))
		}
		for sIdx, s := range spec.Series {
			col := colors[sIdx%len(colors)]
			for _, pt := range s.Points {
				px := padL + float64(xs.At(pt.X))*plotW
				py := padT + (1-float64(ys.At(pt.Y)))*plotH
				rad := 5.0
				if spec.Type == "bubble" && rHi > 0 {
					rad = 4.0 + 14.0*math.Sqrt(max(0.0, pt.R)/rHi)
				}
				fmt.Fprintf(&sb, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s" stroke="%s" stroke-width="1.5"/>`,
					px, py, rad, svgRGBA(col, 0.72), svgHex(col))
			}
		}

	case "breakdown":
		totals := make([]float64, len(spec.Series))
		grand := 0.0
		for i, s := range spec.Series {
			v := breakdownSeriesTotal(s)
			totals[i] = v
			grand += v
		}
		if grand > 0 {
			barX, barY, barW := 24.0, topY+12, width-48.0
			cx := barX
			for i := range spec.Series {
				sw := (totals[i] / grand) * barW
				if sw < 2 {
					continue
				}
				fmt.Fprintf(&sb, `<rect x="%.1f" y="%.1f" width="%.1f" height="10" rx="5" fill="%s"/>`,
					cx, barY, max(2.0, sw-2), svgHex(colors[i%len(colors)]))
				cx += sw
			}
			ry := barY + 34
			for i, s := range spec.Series {
				col := svgHex(colors[i%len(colors)])
				share := totals[i] / grand * 100
				fmt.Fprintf(&sb, `<circle cx="30" cy="%.1f" r="4.5" fill="%s"/>`, ry-4, col)
				fmt.Fprintf(&sb, `<text x="44" y="%.1f" font-size="12.5" font-weight="600" fill="%s">%s</text>`, ry, ink, html.EscapeString(seriesName(spec, i)))
				sub := nonEmptyString(s.Subtitle, fmt.Sprintf("占 %.1f%%", share))
				fmt.Fprintf(&sb, `<text x="44" y="%.1f" font-size="10.5" fill="%s">%s</text>`, ry+15, muted, html.EscapeString(sub))
				fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="13" font-weight="700" fill="%s">%s</text>`,
					width-24, ry+4, ink, html.EscapeString(formatChartValue(totals[i], spec.Unit)))
				ry += 44
			}
		}

	case "heatmap":
		lo, hi := seriesExtent(spec, false)
		rows := len(spec.Series)
		cols := len(spec.Labels)
		cellW := plotW / float64(max(1, cols))
		cellH := plotH / float64(max(1, rows))
		dotMode := !strings.EqualFold(strings.TrimSpace(spec.Style), "tiles")
		dotAccent := colors[0]
		rad := math.Min(8.0, math.Min(cellW, cellH)*0.34)
		for r, s := range spec.Series {
			cy := padT + float64(r)*cellH
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="11" fill="%s">%s</text>`, padL-8, cy+cellH/2+4, muted, html.EscapeString(seriesName(spec, r)))
			for colIdx, v := range s.Data {
				cx := padL + float64(colIdx)*cellW
				if !dotMode {
					if math.IsNaN(v) {
						continue
					}
					t := 0.5
					if hi > lo {
						t = (v - lo) / (hi - lo)
					}
					fill := p.soft.Mix(dotAccent, float32(max(0.14, min(1.0, t))))
					fmt.Fprintf(&sb, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="%s"/>`,
						cx+2, cy+2, max(2.0, cellW-4), max(2.0, cellH-4), svgHex(fill))
					continue
				}
				px, py := cx+cellW/2, cy+cellH/2
				if math.IsNaN(v) {
					fmt.Fprintf(&sb, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="none" stroke="%s" stroke-dasharray="2 2"/>`, px, py, rad, svgHex(p.faint))
					continue
				}
				fill := p.lineSoft
				if v != 0 || lo != 0 {
					t := 0.5
					if hi > lo {
						t = (v - lo) / (hi - lo)
					}
					fill = p.soft.Mix(dotAccent, float32(max(0.25, min(1.0, t))))
				}
				fmt.Fprintf(&sb, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s"/>`, px, py, rad, svgHex(fill))
			}
		}
		for colIdx, lbl := range spec.Labels {
			cx := padL + (float64(colIdx)+0.5)*cellW
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="%s">%s</text>`, cx, padT+plotH+16, muted, html.EscapeString(lbl))
		}

	default:
		// bar, stacked, stacked100, line, area, combo
		isStacked := spec.Type == "stacked" || spec.Type == "stacked100"
		isPercent := spec.Type == "stacked100"
		lo, hi := seriesExtent(spec, isStacked && !isPercent)
		if isPercent {
			lo, hi = 0, 1
		}
		y := chart.NewLinear(lo, hi).Nice(4)
		for _, tick := range y.Ticks(4) {
			py := padT + (1-float64(tick.At))*plotH
			lbl := formatChartValue(tick.Value, spec.Unit)
			if isPercent {
				lbl = fmt.Sprintf("%.0f%%", tick.Value*100)
			}
			fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, padL, py, padL+plotW, py, line)
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" fill="%s">%s</text>`, padL-8, py+3, muted, html.EscapeString(lbl))
		}
		n := len(spec.Labels)
		stepX := plotW / float64(max(1, n))
		for i, lbl := range spec.Labels {
			if n > 12 && i%((n+11)/12) != 0 {
				continue
			}
			px := padL + (float64(i)+0.5)*stepX
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="%s">%s</text>`, px, padT+plotH+16, muted, html.EscapeString(lbl))
		}
		if spec.Type == "line" || spec.Type == "area" {
			for sIdx, s := range spec.Series {
				col := svgHex(colors[sIdx%len(colors)])
				var pts []string
				for i, v := range s.Data {
					if math.IsNaN(v) {
						continue
					}
					px := padL + (float64(i)+0.5)*stepX
					py := padT + (1-float64(y.At(v)))*plotH
					pts = append(pts, fmt.Sprintf("%.1f,%.1f", px, py))
				}
				if len(pts) > 1 {
					if spec.Type == "area" {
						baseY := padT + plotH
						firstX := padL + 0.5*stepX
						lastX := padL + (float64(len(s.Data))-0.5)*stepX
						fmt.Fprintf(&sb, `<polygon points="%.1f,%.1f %s %.1f,%.1f" fill="%s"/>`,
							firstX, baseY, strings.Join(pts, " "), lastX, baseY, svgRGBA(colors[sIdx%len(colors)], 0.16))
					}
					fmt.Fprintf(&sb, `<polyline fill="none" stroke="%s" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" points="%s"/>`,
						col, strings.Join(pts, " "))
				}
			}
		} else {
			var stacks [][]chart.Segment
			if isStacked {
				how := chart.StackStandard
				if isPercent {
					how = chart.StackNormalized
				}
				stacks = chart.Stack(seriesValues(spec), how)
			}
			nSer := max(1, len(spec.Series))
			for i := range spec.Labels {
				bandW := stepX * 0.72
				bx0 := padL + float64(i)*stepX + stepX*0.14
				for sIdx, s := range spec.Series {
					v := s.Data[i]
					if math.IsNaN(v) {
						continue
					}
					var sp chart.Span
					bx, bw := bx0, bandW
					if isStacked {
						sp = y.Span(stacks[sIdx][i].Lo, stacks[sIdx][i].Hi)
					} else {
						sp = y.Span(0, v)
						bw = bandW / float64(nSer)
						bx = bx0 + float64(sIdx)*bw + 1
						bw = max(2.0, bw-2)
					}
					by := padT + (1-float64(sp.Hi))*plotH
					bh := max(1.5, float64(sp.Hi-sp.Lo)*plotH)
					fmt.Fprintf(&sb, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="4" fill="%s"/>`,
						bx, by, bw, bh, svgHex(colors[sIdx%len(colors)]))
				}
			}
		}
		for _, ref := range spec.RefLines {
			ry := padT + (1-float64(y.At(ref.Value)))*plotH
			col := svgHex(p.warning)
			if ref.Tone == "danger" {
				col = svgHex(p.danger)
			} else if ref.Tone == "success" {
				col = svgHex(p.success)
			}
			fmt.Fprintf(&sb, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-dasharray="5 4" stroke-width="1.5"/>`,
				padL, ry, padL+plotW, ry, col)
			if ref.Label != "" {
				fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" text-anchor="end" font-size="10" font-weight="600" fill="%s">%s</text>`,
					padL+plotW-4, ry-5, col, html.EscapeString(ref.Label))
			}
		}
	}

	if len(spec.Series) > 1 && spec.Type != "pie" && spec.Type != "donut" {
		lx := padL
		ly := height - 16
		for sIdx := range spec.Series {
			name := seriesName(spec, sIdx)
			fmt.Fprintf(&sb, `<rect x="%.1f" y="%.1f" width="8" height="8" rx="2" fill="%s"/>`, lx, ly-8, svgHex(colors[sIdx%len(colors)]))
			fmt.Fprintf(&sb, `<text x="%.1f" y="%.1f" font-size="11" fill="%s">%s</text>`, lx+13, ly, muted, html.EscapeString(name))
			lx += float64(len([]rune(name)))*10 + 34
			if lx > width-80 {
				break
			}
		}
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}
