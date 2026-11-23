package desktop

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/egoist/mygo/ui/markdown"
)

// Native MyGo structured visual blocks, absorbing QingYunA/answer-me-with-html's
// compact Markdown fenced-block vocabulary without any HTML or WebView:
// callout, kv, timeline, tree (with +/-/~ change filters), limits, api, annot,
// ask (interactive selection -> composer reply), flow, state, sequence, er, class.

// parseFenceInfoAttrs splits a fenced code block's info string into key=value
// attributes and bare modifier words (after the language name).
func parseFenceInfoAttrs(info string) (map[string]string, []string) {
	attrs := map[string]string{}
	var words []string
	s := strings.TrimSpace(info)
	for len(s) > 0 {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		sp := strings.IndexFunc(s, unicode.IsSpace)
		if eq > 0 && (sp < 0 || eq < sp) {
			key := strings.ToLower(strings.TrimSpace(s[:eq]))
			s = s[eq+1:]
			var val string
			if len(s) > 0 && (s[0] == '"' || s[0] == '\'') {
				quote := s[0]
				s = s[1:]
				end := strings.IndexByte(s, quote)
				if end >= 0 {
					val = s[:end]
					s = s[end+1:]
				} else {
					val = s
					s = ""
				}
			} else {
				end := strings.IndexFunc(s, unicode.IsSpace)
				if end >= 0 {
					val = s[:end]
					s = s[end:]
				} else {
					val = s
					s = ""
				}
			}
			attrs[key] = val
			continue
		}
		if sp >= 0 {
			words = append(words, s[:sp])
			s = s[sp:]
		} else {
			words = append(words, s)
			s = ""
		}
	}
	return attrs, words
}

// splitPipeFields splits a line on unescaped '|', preserving '\|' as '|'.
func splitPipeFields(line string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if line[i] == '|' {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(line[i])
	}
	out = append(out, strings.TrimSpace(cur.String()))
	return out
}

func nonBlankLines(code string) []string {
	var out []string
	for _, line := range strings.Split(code, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, strings.TrimRight(line, "\r"))
	}
	return out
}

func splitDeltaMarker(trimmed string) (delta string, rest string) {
	switch {
	case strings.HasPrefix(trimmed, "+ "):
		return "added", strings.TrimSpace(trimmed[2:])
	case strings.HasPrefix(trimmed, "- "):
		return "removed", strings.TrimSpace(trimmed[2:])
	case strings.HasPrefix(trimmed, "~ "):
		return "changed", strings.TrimSpace(trimmed[2:])
	}
	return "", trimmed
}

func deltaTone(p palette, delta string) (ui.Color, string) {
	switch delta {
	case "added":
		return p.success, "+"
	case "removed":
		return p.danger, "−"
	case "changed":
		return p.warning, "~"
	}
	return p.muted, ""
}

// ------------------------------------------------------------------ callout

func (w *workbench) calloutBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在整理要点…", "Formatting callout…"))
		return
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(b.Info), b.Language))
	kind := "info"
	title := ""
	if fields := strings.Fields(rest); len(fields) > 0 {
		first := strings.ToLower(fields[0])
		switch first {
		case "info", "note", "tip":
			kind = "info"
			title = strings.TrimSpace(rest[len(fields[0]):])
		case "ok", "success", "check":
			kind = "ok"
			title = strings.TrimSpace(rest[len(fields[0]):])
		case "warn", "warning", "caution":
			kind = "warn"
			title = strings.TrimSpace(rest[len(fields[0]):])
		case "err", "error", "danger":
			kind = "err"
			title = strings.TrimSpace(rest[len(fields[0]):])
		default:
			title = rest
		}
	}
	tone, icon, defaultTitle := p.accentInk, lucide("info"), w.tr("说明", "Note")
	switch kind {
	case "ok":
		tone, icon, defaultTitle = p.success, lucide("circle-check"), w.tr("结论", "Conclusion")
	case "warn":
		tone, icon, defaultTitle = p.warning, lucide("triangle-alert"), w.tr("注意", "Warning")
	case "err":
		tone, icon, defaultTitle = p.danger, alertIcon, w.tr("风险提示", "Alert")
	}
	if title == "" {
		title = defaultTitle
	}
	body := strings.TrimSpace(b.Code())
	ui.Row(c).FillWidth().Radius(12).Background(tone.Alpha(0.08)).Border(1, tone.Alpha(0.28)).Clip().Children(func() {
		ui.Box(c).Width(3.5).AlignSelf(ui.Stretch).Background(tone).Shrink(0)
		ui.Column(c).Grow(1).MinWidth(0).Padding(11, 14).Gap(5).Children(func() {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icon).FontSize(14).TextColor(tone).Shrink(0)
				ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink).Wrap().Grow(1)
			})
			if body != "" {
				markdown.View(c, body, w.replyMarkdown(w.selected))
			}
		})
	})
}

// ----------------------------------------------------------------------- kv

func (w *workbench) kvBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在生成键值卡片…", "Building key-value block…"))
		return
	}
	attrs, _ := parseFenceInfoAttrs(b.Info)
	cols := 2
	if n, err := strconv.Atoi(attrs["cols"]); err == nil && n >= 1 && n <= 4 {
		cols = n
	}
	type kvCell struct {
		wide       bool
		key, value string
	}
	var cells []kvCell
	for _, raw := range nonBlankLines(b.Code()) {
		line := strings.TrimSpace(raw)
		wide := strings.HasPrefix(line, "*")
		if wide {
			line = strings.TrimSpace(line[1:])
		}
		sep := strings.IndexAny(line, ":：")
		if sep <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:sep])
		r := line[sep:]
		if strings.HasPrefix(r, "：") {
			r = r[len("："):]
		} else {
			r = r[1:]
		}
		cells = append(cells, kvCell{wide: wide, key: k, value: strings.TrimSpace(r)})
	}
	if len(cells) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	ui.Column(c).FillWidth().Radius(12).Background(p.raised).Border(1, p.line).Padding(14, 16).Gap(10).Children(func() {
		if title := attrs["title"]; title != "" {
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink)
		}
		for i := 0; i < len(cells); {
			if cells[i].wide || cols == 1 {
				cell := cells[i]
				box := ui.Column(c.Key("kv:"+strconv.Itoa(i))).FillWidth().Gap(3).Padding(8, 10).Radius(8).Background(p.soft)
				box.Children(func() {
					ui.Text(c, cell.key).FontSize(11).FontWeight(500).TextColor(p.muted)
					valSize := float32(13.5)
					if cell.wide {
						valSize = 15
					}
					ui.Text(c, cell.value).FontSize(valSize).FontWeight(600).TextColor(p.ink).Wrap().Selectable()
				})
				i++
				continue
			}
			end := min(i+cols, len(cells))
			for k := i; k < end; k++ {
				if cells[k].wide {
					end = k
					break
				}
			}
			rowSlice := cells[i:end]
			base := i
			ui.Row(c.Key("kvrow:" + strconv.Itoa(base))).FillWidth().Gap(8).Wrap().AlignItems(ui.Stretch).Children(func() {
				for j, cell := range rowSlice {
					cell := cell
					ui.Column(c.Key("c:"+strconv.Itoa(j))).Grow(1).Basis(180).MinWidth(140).Gap(3).Padding(8, 10).Radius(8).Background(p.soft).Children(func() {
						ui.Text(c, cell.key).FontSize(11).FontWeight(500).TextColor(p.muted)
						ui.Text(c, cell.value).FontSize(13).FontWeight(500).TextColor(p.ink).Wrap().Selectable()
					})
				}
			})
			i = end
		}
	})
}

// ----------------------------------------------------------------- timeline

func (w *workbench) timelineBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在生成时间线…", "Building timeline…"))
		return
	}
	attrs, words := parseFenceInfoAttrs(b.Info)
	forceV, forceH := false, false
	for _, word := range words {
		switch strings.ToLower(word) {
		case "v", "vertical":
			forceV = true
		case "h", "horizontal":
			forceH = true
		}
	}
	type tlItem struct {
		when, title, detail string
		hi                  bool
	}
	var items []tlItem
	for _, raw := range nonBlankLines(b.Code()) {
		parts := splitPipeFields(raw)
		if len(parts) < 2 {
			continue
		}
		when := parts[0]
		hi := strings.HasPrefix(when, "*")
		if hi {
			when = strings.TrimSpace(when[1:])
		}
		detail := ""
		if len(parts) >= 3 {
			detail = strings.Join(parts[2:], " · ")
		}
		items = append(items, tlItem{when: when, title: parts[1], detail: detail, hi: hi})
	}
	if len(items) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	winW, _ := c.Size()
	vertical := forceV || (!forceH && (len(items) > 5 || winW < 820))
	ui.Column(c).FillWidth().Radius(12).Background(p.raised).Border(1, p.lineSoft).Padding(14, 16).Gap(10).Children(func() {
		if title := attrs["title"]; title != "" {
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.ink)
		}
		if !vertical {
			ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Start).Children(func() {
				for idx, it := range items {
					it, idx := it, idx
					dotColor := p.line
					whenColor := p.muted
					if it.hi {
						dotColor = p.accentInk
						whenColor = p.accentInk
					}
					ui.Column(c.Key("tlh:" + strconv.Itoa(idx))).Grow(1).Basis(0).MinWidth(0).Gap(5).Children(func() {
						ui.Text(c, it.when).Font("Menlo").FontSize(11).FontWeight(600).TextColor(whenColor).SingleLine().Ellipsis("…")
						ui.Row(c).FillWidth().Height(12).AlignItems(ui.Center).Children(func() {
							ui.Box(c).Size(8, 8).Radius(4).Background(dotColor).Shrink(0)
							if idx < len(items)-1 {
								ui.Box(c).Grow(1).Height(1.5).Background(p.lineSoft).Margin(0, 0, 0, 4)
							}
						})
						ui.Text(c, it.title).FontSize(12.5).FontWeight(600).TextColor(p.ink).Wrap()
						if it.detail != "" {
							ui.Text(c, it.detail).FontSize(11.5).TextColor(p.muted).Wrap()
						}
					})
				}
			})
			return
		}
		ui.Column(c).FillWidth().Gap(0).Children(func() {
			for idx, it := range items {
				it, idx := it, idx
				dotColor := p.muted
				whenColor := p.muted
				if it.hi {
					dotColor = p.accentInk
					whenColor = p.accentInk
				}
				ui.Row(c.Key("tlv:" + strconv.Itoa(idx))).FillWidth().Gap(12).AlignItems(ui.Stretch).Children(func() {
					ui.Text(c, it.when).Font("Menlo").FontSize(11).FontWeight(600).TextColor(whenColor).Width(88).Shrink(0).TextAlign(ui.End).Padding(2, 0, 0)
					ui.Column(c).Width(12).AlignItems(ui.Center).Shrink(0).Children(func() {
						ui.Box(c).Size(8, 8).Radius(4).Background(dotColor).Margin(5, 0, 0).Shrink(0)
						if idx < len(items)-1 {
							ui.Box(c).Width(1.5).Grow(1).Background(p.lineSoft).Margin(2, 0)
						}
					})
					ui.Column(c).Grow(1).MinWidth(0).Gap(2).Padding(0, 0, 12).Children(func() {
						ui.Text(c, it.title).FontSize(13).FontWeight(600).TextColor(p.ink).Wrap().Selectable()
						if it.detail != "" {
							ui.Text(c, it.detail).FontSize(12).TextColor(p.muted).Wrap().Selectable()
						}
					})
				})
			}
		})
	})
}

// --------------------------------------------------------------------- tree

type treeViewState struct{ mode int } // 0: all, 1: before, 2: after

func (w *workbench) treeBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在生成结构树…", "Building hierarchy tree…"))
		return
	}
	attrs, _ := parseFenceInfoAttrs(b.Info)
	type treeNode struct {
		depth       int
		label, note string
		hi          bool
		delta       string // "", "added", "removed", "changed"
	}
	var rawNodes []treeNode
	var indentStops []int
	hasDelta := false
	for _, raw := range nonBlankLines(b.Code()) {
		indent := 0
		for _, ch := range raw {
			if ch == ' ' {
				indent++
			} else if ch == '\t' {
				indent += 2
			} else {
				break
			}
		}
		depth := 0
		for depth < len(indentStops) && indent > indentStops[depth] {
			depth++
		}
		if depth == len(indentStops) {
			indentStops = append(indentStops, indent)
		} else {
			indentStops = indentStops[:depth+1]
			indentStops[depth] = indent
		}
		trimmed := strings.TrimSpace(raw)
		delta, body := splitDeltaMarker(trimmed)
		if delta != "" {
			hasDelta = true
		}
		hi := strings.HasPrefix(body, "*")
		if hi {
			body = strings.TrimSpace(body[1:])
		}
		parts := splitPipeFields(body)
		label := strings.ReplaceAll(parts[0], "`", "")
		note := ""
		if len(parts) >= 2 {
			note = strings.Join(parts[1:], " · ")
		}
		rawNodes = append(rawNodes, treeNode{depth: depth, label: label, note: note, hi: hi, delta: delta})
	}
	if len(rawNodes) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	// Propagate + / - from ancestor directories to unannotated children.
	var stack []string
	for i := range rawNodes {
		d := rawNodes[i].depth
		if d < len(stack) {
			stack = stack[:d]
		}
		for len(stack) < d {
			stack = append(stack, "")
		}
		if rawNodes[i].delta == "" && d > 0 {
			if anc := stack[d-1]; anc == "added" || anc == "removed" {
				rawNodes[i].delta = anc
			}
		}
		stack = append(stack, rawNodes[i].delta)
	}
	card := ui.Column(c).FillWidth().Radius(12).Background(p.raised).Border(1, p.lineSoft)
	state := ui.Local(card, treeViewState{}, func() *treeViewState { return &treeViewState{} })
	card.Children(func() {
		if attrs["title"] != "" || hasDelta {
			ui.Row(c).Height(34).Padding(0, 10, 0, 14).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
				ui.Icon(c, folderIcon).FontSize(13).TextColor(p.muted)
				ui.Text(c, nonEmptyString(attrs["title"], w.tr("目录与模块结构", "Structure"))).FontSize(12.5).FontWeight(600).TextColor(p.ink).Grow(1)
				if hasDelta {
					mode := (*state).mode
					ui.Segmented(c.Key("tree-mode"), &mode, w.tr("变更", "Changes"), w.tr("变更前", "Before"), w.tr("变更后", "After")).Label(w.tr("结构视图", "Structure view"))
					(*state).mode = mode
				}
			})
		}
		ui.Column(c).Padding(10, 14).Gap(3).Children(func() {
			for idx, node := range rawNodes {
				if (*state).mode == 1 && node.delta == "added" {
					continue
				}
				if (*state).mode == 2 && node.delta == "removed" {
					continue
				}
				node, idx := node, idx
				row := ui.Row(c.Key("tn:"+strconv.Itoa(idx))).MinHeight(23).Padding(2, 6, 2, float32(node.depth)*18+6).Gap(6).Radius(6).AlignItems(ui.Center)
				tone := p.ink
				if (*state).mode == 0 && node.delta != "" {
					dt, mark := deltaTone(p, node.delta)
					row.Background(dt.Alpha(0.08))
					tone = dt
					row.Children(func() {
						ui.Text(c, mark).Font("Menlo").FontSize(11.5).FontWeight(700).TextColor(dt).Shrink(0)
						ui.Text(c, node.label).Font("Menlo").FontSize(12).FontWeight(500).TextColor(tone).Selectable()
						if node.note != "" {
							ui.Text(c, "— "+node.note).FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
						}
					})
					continue
				}
				if node.hi {
					row.Background(p.accent.Alpha(0.1))
					tone = p.accentInk
				}
				row.Children(func() {
					prefix := "├─"
					if node.depth == 0 {
						prefix = "▸"
					}
					ui.Text(c, prefix).Font("Menlo").FontSize(11).TextColor(p.faint).Shrink(0)
					weight := 400
					if node.depth == 0 || node.hi {
						weight = 600
					}
					ui.Text(c, node.label).Font("Menlo").FontSize(12).FontWeight(weight).TextColor(tone).Selectable()
					if node.note != "" {
						ui.Text(c, "· "+node.note).FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
					}
				})
			}
		})
	})
}

// ------------------------------------------------------------------- limits

func (w *workbench) limitsBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在计算指标阈值…", "Building limits card…"))
		return
	}
	attrs, _ := parseFenceInfoAttrs(b.Info)
	type limitItem struct {
		label, rawVal, unit string
		ratio               float64
		hasRatio            bool
	}
	var items []limitItem
	for _, raw := range nonBlankLines(b.Code()) {
		parts := splitPipeFields(raw)
		if len(parts) < 2 {
			continue
		}
		unit := ""
		if len(parts) >= 3 {
			unit = parts[2]
		}
		spec := parts[1]
		item := limitItem{label: strings.TrimPrefix(parts[0], "*"), rawVal: spec, unit: unit}
		if slash := strings.IndexByte(spec, '/'); slash > 0 {
			cur, err1 := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(spec[:slash]), ",", ""), 64)
			lim, err2 := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(spec[slash+1:]), ",", ""), 64)
			if err1 == nil && err2 == nil && lim > 0 {
				item.ratio = cur / lim
				item.hasRatio = true
			}
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	shadowCol := ui.RGBA(0, 0, 0, 0.04)
	if p.dark {
		shadowCol = ui.RGBA(0, 0, 0, 0.22)
	}
	ui.Column(c).FillWidth().Radius(16).Background(p.raised).Border(1, p.lineSoft).Shadow(0, 2, 10, 0, shadowCol).Padding(16, 18).Gap(12).Children(func() {
		if title := attrs["title"]; title != "" {
			ui.Text(c, title).FontSize(13.5).FontWeight(600).TextColor(p.ink)
		}
		for idx, it := range items {
			it, idx := it, idx
			ui.Column(c.Key("lim:" + strconv.Itoa(idx))).FillWidth().Gap(7).Padding(10, 12).Radius(12).Background(p.soft).Border(1, p.lineSoft).Children(func() {
				tone := p.success
				statusLabel := "充裕"
				if it.hasRatio {
					switch {
					case it.ratio > 1.0:
						tone = p.danger
						statusLabel = "超限"
					case it.ratio >= 0.8:
						tone = p.warning
						statusLabel = "接近上限"
					case it.ratio >= 0.55:
						statusLabel = "健康"
					}
				} else {
					tone = p.accentInk
					statusLabel = ""
				}
				clamped := float32(math.Max(0, math.Min(it.ratio, 1.0)))
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, strings.TrimSpace(it.label)).FontSize(12.5).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…")
					if statusLabel != "" {
						ui.Text(c, statusLabel).FontSize(10).FontWeight(600).TextColor(tone).Padding(2, 7).Radius(99).Background(tone.Alpha(0.13)).NoWrap()
					}
					ui.Spacer(c)
					valText := it.rawVal
					if it.unit != "" {
						valText += " " + it.unit
					}
					ui.Text(c, valText).Font("Menlo").FontSize(11.5).FontWeight(600).FontFeatures("tnum").TextColor(tone).NoWrap()
					if it.hasRatio {
						// Compact circular ring gauge badge inspired by Studio quota cards
						trackSlice := chart.Pie([]float64{1}, 0)[0]
						rem := max(0.0, 1.0-float64(clamped))
						ring := ui.Box(c).Size(38, 38).Shrink(0)
						ring.Draw(func(painter *ui.Painter, r ui.Rect) {
							cx, cy := r.X+r.W/2, r.Y+r.H/2
							painter.FillPath(chart.Sector(cx, cy, 14.5, 18.0, trackSlice), p.lineSoft)
							if clamped > 0 {
								sl := chart.Pie([]float64{float64(clamped), rem}, 0)
								painter.FillPath(chart.Sector(cx, cy, 14.5, 18.0, sl[0]), tone)
							}
						})
						ring.Children(func() {
							ui.Column(c).Fill().Justify(ui.Center).AlignItems(ui.Center).Children(func() {
								ui.Text(c, fmt.Sprintf("%.0f%%", it.ratio*100)).FontSize(9).Bold().FontFeatures("tnum").TextColor(p.ink).NoWrap()
							})
						})
					}
				})
				if it.hasRatio {
					// 20 discrete segmented pills
					const segments = 20
					filledSegs := int(math.Round(float64(clamped) * segments))
					if clamped > 0 && filledSegs == 0 {
						filledSegs = 1
					}
					ui.Box(c).FillWidth().Height(8).Draw(func(painter *ui.Painter, r ui.Rect) {
						const gap = float32(2.5)
						segW := (r.W - gap*float32(segments-1)) / float32(segments)
						if segW < 2 {
							painter.Fill(r, p.lineSoft, 4)
							if clamped > 0 {
								painter.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W * clamped, H: r.H}, tone, 4)
							}
							return
						}
						for i := 0; i < segments; i++ {
							sx := r.X + float32(i)*(segW+gap)
							col := p.lineSoft.Alpha(0.75)
							if i < filledSegs {
								col = tone
							}
							painter.Fill(ui.Rect{X: sx, Y: r.Y, W: segW, H: r.H}, col, 2.5)
						}
					})
				}
			})
		}
	})
}

// ---------------------------------------------------------------------- api

func (w *workbench) apiBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在整理接口契约…", "Formatting API contract…"))
		return
	}
	type apiField struct {
		name, typ, req, desc, delta string
	}
	type apiSection struct {
		title, delta string
		fields       []apiField
	}
	type apiEndpoint struct {
		method, path, delta string
		sections            []apiSection
	}
	var endpoints []apiEndpoint
	for _, raw := range nonBlankLines(b.Code()) {
		indented := len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t')
		trimmed := strings.TrimSpace(raw)
		delta, line := splitDeltaMarker(trimmed)
		if !indented {
			fields := strings.Fields(line)
			if len(fields) >= 2 && isAPIMethod(fields[0]) {
				endpoints = append(endpoints, apiEndpoint{method: strings.ToUpper(fields[0]), path: strings.Join(fields[1:], " "), delta: delta})
				continue
			}
			if len(endpoints) == 0 {
				endpoints = append(endpoints, apiEndpoint{method: "API", path: line, delta: delta})
				continue
			}
			ep := &endpoints[len(endpoints)-1]
			ep.sections = append(ep.sections, apiSection{title: line, delta: delta})
			continue
		}
		if len(endpoints) == 0 {
			endpoints = append(endpoints, apiEndpoint{method: "API", path: "Contract"})
		}
		ep := &endpoints[len(endpoints)-1]
		parts := splitPipeFields(line)
		specWords := strings.Fields(parts[0])
		if len(specWords) == 1 && len(parts) == 1 && isAPISectionKeyword(specWords[0]) {
			ep.sections = append(ep.sections, apiSection{title: line, delta: delta})
			continue
		}
		if len(ep.sections) == 0 {
			ep.sections = append(ep.sections, apiSection{title: "fields"})
		}
		sec := &ep.sections[len(ep.sections)-1]
		f := apiField{delta: delta}
		if len(specWords) > 0 {
			f.name = specWords[0]
			rest := specWords[1:]
			if len(rest) > 0 {
				last := strings.ToLower(rest[len(rest)-1])
				if last == "required" || last == "optional" {
					f.req = last
					rest = rest[:len(rest)-1]
				}
			}
			f.typ = strings.Join(rest, " ")
		}
		if len(parts) > 1 {
			f.desc = strings.Join(parts[1:], " · ")
		}
		sec.fields = append(sec.fields, f)
	}
	if len(endpoints) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	ui.Column(c).FillWidth().Gap(10).Children(func() {
		for idx, ep := range endpoints {
			ep, idx := ep, idx
			methodTone := apiMethodColor(p, ep.method)
			ui.Column(c.Key("ep:"+strconv.Itoa(idx))).FillWidth().Radius(12).Background(p.raised).Border(1, p.lineSoft).Clip().Children(func() {
				ui.Row(c).Padding(9, 14).Gap(8).AlignItems(ui.Center).Background(p.soft).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
					if ep.delta != "" {
						dt, mark := deltaTone(p, ep.delta)
						ui.Text(c, mark).Font("Menlo").FontSize(12).FontWeight(700).TextColor(dt)
					}
					ui.Row(c).Height(20).Padding(0, 7).Radius(5).Background(methodTone.Alpha(0.16)).AlignItems(ui.Center).Shrink(0).Children(func() {
						ui.Text(c, ep.method).Font("Menlo").FontSize(10.5).FontWeight(700).TextColor(methodTone).NoWrap()
					})
					ui.Text(c, ep.path).Font("Menlo").FontSize(12.5).FontWeight(600).TextColor(p.ink).Selectable().Grow(1).SingleLine().Ellipsis("…")
				})
				for sIdx, sec := range ep.sections {
					sec, sIdx := sec, sIdx
					secCol := ui.Column(c.Key("sec:"+strconv.Itoa(sIdx))).FillWidth().Padding(8, 14).Gap(5)
					if sIdx > 0 {
						secCol.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
					}
					secCol.Children(func() {
						ui.Text(c, sec.title).Font("Menlo").FontSize(11).FontWeight(600).TextColor(p.muted)
						for fIdx, f := range sec.fields {
							f, fIdx := f, fIdx
							ui.Row(c.Key("f:"+strconv.Itoa(fIdx))).FillWidth().Gap(8).Padding(3, 6).Radius(5).AlignItems(ui.Center).Wrap().Children(func() {
								if f.delta != "" {
									dt, mark := deltaTone(p, f.delta)
									ui.Text(c, mark).Font("Menlo").FontSize(11).FontWeight(700).TextColor(dt)
								}
								ui.Text(c, f.name).Font("Menlo").FontSize(12).FontWeight(600).TextColor(p.ink).Selectable()
								if f.typ != "" {
									ui.Text(c, f.typ).Font("Menlo").FontSize(11).TextColor(p.accentInk)
								}
								if f.req != "" {
									reqTone := p.faint
									if f.req == "required" {
										reqTone = p.warning
									}
									ui.Text(c, f.req).FontSize(10.5).FontWeight(500).TextColor(reqTone)
								}
								if f.desc != "" {
									ui.Text(c, f.desc).FontSize(12).TextColor(p.muted).Grow(1).MinWidth(120).Wrap().Selectable()
								}
							})
						}
					})
				}
			})
		}
	})
}

func isAPIMethod(word string) bool {
	switch strings.ToUpper(word) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "RPC", "WS", "EVENT":
		return true
	}
	return false
}

func isAPISectionKeyword(word string) bool {
	switch strings.ToLower(word) {
	case "path", "query", "headers", "request", "response", "body", "params", "returns", "errors":
		return true
	}
	return false
}

func apiMethodColor(p palette, method string) ui.Color {
	switch method {
	case "GET":
		return p.running
	case "POST":
		return p.success
	case "PUT", "PATCH":
		return p.warning
	case "DELETE":
		return p.danger
	default:
		return p.accentInk
	}
}

// -------------------------------------------------------------------- annot

type annotSpan struct {
	text, note string
	bad        bool
}

func (w *workbench) annotBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在渲染批注…", "Building annotations…"))
		return
	}
	type annotGroup struct {
		title, meta string
		sentences   [][]annotSpan
		captions    []string
	}
	var groups []annotGroup
	ensure := func() *annotGroup {
		if len(groups) == 0 {
			groups = append(groups, annotGroup{})
		}
		return &groups[len(groups)-1]
	}
	for _, raw := range nonBlankLines(b.Code()) {
		t := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(t, "#"):
			parts := splitPipeFields(strings.TrimSpace(strings.TrimLeft(t, "#")))
			meta := ""
			if len(parts) > 1 {
				meta = parts[1]
			}
			groups = append(groups, annotGroup{title: parts[0], meta: meta})
		case strings.HasPrefix(t, ">"):
			g := ensure()
			g.captions = append(g.captions, strings.TrimSpace(t[1:]))
		default:
			g := ensure()
			g.sentences = append(g.sentences, parseAnnotSpans(t))
		}
	}
	if len(groups) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	ui.Column(c).FillWidth().Radius(12).Background(p.raised).Border(1, p.lineSoft).Padding(14, 16).Gap(12).Children(func() {
		for gIdx, g := range groups {
			g, gIdx := g, gIdx
			col := ui.Column(c.Key("ag:" + strconv.Itoa(gIdx))).FillWidth().Gap(8)
			if gIdx > 0 {
				col.Padding(10, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
			}
			col.Children(func() {
				if g.title != "" || g.meta != "" {
					ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, g.title).FontSize(12.5).FontWeight(600).TextColor(p.ink).Grow(1)
						if g.meta != "" {
							ui.Text(c, g.meta).Font("Menlo").FontSize(11).TextColor(p.faint)
						}
					})
				}
				for sIdx, spans := range g.sentences {
					spans, sIdx := spans, sIdx
					ui.Row(c.Key("s:" + strconv.Itoa(sIdx))).FillWidth().Gap(4).Wrap().AlignItems(ui.End).Children(func() {
						for pIdx, sp := range spans {
							sp, pIdx := sp, pIdx
							if sp.note == "" {
								ui.Text(c.Key("p:"+strconv.Itoa(pIdx)), sp.text).FontSize(13.5).LineHeight(1.6).TextColor(p.ink).Selectable()
								continue
							}
							tone := p.accentInk
							if sp.bad {
								tone = p.danger
							}
							ui.Column(c.Key("p:"+strconv.Itoa(pIdx))).Gap(2).Padding(3, 7).Radius(6).Background(tone.Alpha(0.1)).Border(1, tone.Alpha(0.3)).Children(func() {
								ui.Text(c, sp.text).FontSize(13).FontWeight(600).TextColor(p.ink).Selectable()
								ui.Text(c, sp.note).FontSize(10.5).FontWeight(500).TextColor(tone)
							})
						}
					})
				}
				for _, capText := range g.captions {
					ui.Text(c, capText).FontSize(11.5).TextColor(p.muted).Wrap()
				}
			})
		}
	})
}

func parseAnnotSpans(s string) []annotSpan {
	var out []annotSpan
	for len(s) > 0 {
		open := strings.IndexByte(s, '[')
		if open < 0 {
			out = append(out, annotSpan{text: s})
			break
		}
		closeB := strings.IndexByte(s[open+1:], ']')
		if closeB < 0 {
			out = append(out, annotSpan{text: s})
			break
		}
		closeIdx := open + 1 + closeB
		if closeIdx+1 >= len(s) || s[closeIdx+1] != '{' {
			out = append(out, annotSpan{text: s[:closeIdx+1]})
			s = s[closeIdx+1:]
			continue
		}
		closeNote := strings.IndexByte(s[closeIdx+2:], '}')
		if closeNote < 0 {
			out = append(out, annotSpan{text: s})
			break
		}
		if open > 0 {
			out = append(out, annotSpan{text: s[:open]})
		}
		word := s[open+1 : closeIdx]
		note := strings.TrimSpace(s[closeIdx+2 : closeIdx+2+closeNote])
		bad := strings.HasPrefix(note, "!")
		if bad {
			note = strings.TrimSpace(note[1:])
		}
		out = append(out, annotSpan{text: word, note: note, bad: bad})
		s = s[closeIdx+2+closeNote+1:]
	}
	return out
}

// -------------------------------------------- flow / state / sequence / er / class

func (w *workbench) flowAliasBlock(c *ui.Context, b *markdown.CodeBlockView) {
	if b.Open {
		visualPending(c, w.widgets.markdownPalette, w.tr("正在绘制流程图…", "Drawing diagram…"))
		return
	}
	code := translateConciseFlowToMermaid(b.Language, b.Info, b.Code())
	w.renderMermaidSource(c, b, code)
}

func (w *workbench) sequenceAliasBlock(c *ui.Context, b *markdown.CodeBlockView) {
	if b.Open {
		visualPending(c, w.widgets.markdownPalette, w.tr("正在绘制时序图…", "Drawing sequence diagram…"))
		return
	}
	code := translateConciseSequenceToMermaid(b.Info, b.Code())
	w.renderMermaidSource(c, b, code)
}

func translateConciseFlowToMermaid(lang, info, body string) string {
	trimmed := strings.TrimSpace(body)
	first := ""
	if fields := strings.Fields(trimmed); len(fields) > 0 {
		first = strings.ToLower(fields[0])
	}
	if first == "flowchart" || first == "graph" || strings.HasPrefix(first, "statediagram") {
		return body
	}
	_, words := parseFenceInfoAttrs(info)
	dir := "TD"
	for _, w := range words {
		up := strings.ToUpper(w)
		if up == "LR" || up == "RL" || up == "BT" || up == "TD" || up == "TB" {
			dir = up
		}
	}
	if strings.EqualFold(lang, "state") {
		var sb strings.Builder
		sb.WriteString("stateDiagram-v2\n")
		if dir == "LR" {
			sb.WriteString("  direction LR\n")
		}
		for _, raw := range nonBlankLines(body) {
			_, line := splitDeltaMarker(strings.TrimSpace(raw))
			line = strings.ReplaceAll(line, " -> ", " --> ")
			sb.WriteString("  " + line + "\n")
		}
		return sb.String()
	}
	var sb strings.Builder
	sb.WriteString("flowchart " + dir + "\n")
	ids := map[string]string{}
	nextID := 0
	nodeRef := func(token string) string {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "*"))
		if t == "" {
			return "N0"
		}
		if id, ok := ids[t]; ok {
			return id
		}
		nextID++
		id := fmt.Sprintf("N%d", nextID)
		ids[t] = id
		switch {
		case strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}"):
			return fmt.Sprintf("%s{%q}", id, strings.Trim(t, "{}"))
		case strings.HasPrefix(t, "[(") && strings.HasSuffix(t, ")]"):
			return fmt.Sprintf("%s[(%q)]", id, strings.TrimSuffix(strings.TrimPrefix(t, "[("), ")]"))
		case strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")"):
			return fmt.Sprintf("%s([%q])", id, strings.Trim(t, "()"))
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			return fmt.Sprintf("%s[%q]", id, strings.Trim(t, "[]"))
		default:
			return fmt.Sprintf("%s[%q]", id, t)
		}
	}
	for _, raw := range nonBlankLines(body) {
		_, line := splitDeltaMarker(strings.TrimSpace(raw))
		if strings.HasPrefix(strings.ToLower(line), "group ") {
			continue
		}
		arrow := "-->"
		sep := ""
		switch {
		case strings.Contains(line, "-->"):
			sep, arrow = "-->", "-.->"
		case strings.Contains(line, "->"):
			sep, arrow = "->", "-->"
		}
		if sep == "" {
			sb.WriteString("  " + nodeRef(line) + "\n")
			continue
		}
		parts := strings.SplitN(line, sep, 2)
		lhs := parts[0]
		rhs := parts[1]
		edgeLabel := ""
		if colon := strings.IndexByte(rhs, ':'); colon >= 0 {
			edgeLabel = strings.TrimSpace(rhs[colon+1:])
			rhs = rhs[:colon]
		}
		for _, fromTok := range strings.Split(lhs, "&") {
			for _, toTok := range strings.Split(rhs, "&") {
				f := nodeRef(fromTok)
				t := nodeRef(toTok)
				if edgeLabel != "" {
					sb.WriteString(fmt.Sprintf("  %s %s|%s| %s\n", f, arrow, strings.ReplaceAll(edgeLabel, "|", "/"), t))
				} else {
					sb.WriteString(fmt.Sprintf("  %s %s %s\n", f, arrow, t))
				}
			}
		}
	}
	return sb.String()
}

func translateConciseSequenceToMermaid(info, body string) string {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(strings.ToLower(trimmed), "sequencediagram") {
		return body
	}
	_, words := parseFenceInfoAttrs(info)
	var sb strings.Builder
	sb.WriteString("sequenceDiagram\n")
	for _, w := range words {
		if strings.EqualFold(w, "num") || strings.EqualFold(w, "autonumber") {
			sb.WriteString("  autonumber\n")
		}
	}
	for _, raw := range nonBlankLines(body) {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "==") && strings.HasSuffix(line, "=="):
			phase := strings.TrimSpace(strings.Trim(line, "="))
			if phase != "" {
				sb.WriteString("  Note over " + phase + "\n")
			}
		case strings.HasPrefix(strings.ToLower(line), "note "):
			rest := strings.TrimSpace(line[5:])
			if colon := strings.IndexByte(rest, ':'); colon > 0 {
				sb.WriteString("  Note over " + strings.TrimSpace(rest[:colon]) + ": " + strings.TrimSpace(rest[colon+1:]) + "\n")
			}
		case strings.Contains(line, "-->") && !strings.Contains(line, "-->>"):
			sb.WriteString("  " + strings.Replace(line, "-->", "-->>", 1) + "\n")
		case strings.Contains(line, "->") && !strings.Contains(line, "->>"):
			sb.WriteString("  " + strings.Replace(line, "->", "->>", 1) + "\n")
		default:
			sb.WriteString("  " + line + "\n")
		}
	}
	return sb.String()
}

// erAndClassBlock draws ER data models and Class/Interface diagrams natively as
// structured schema cards + relationship summaries.
func (w *workbench) erAndClassBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在绘制模型结构…", "Building model diagram…"))
		return
	}
	w.renderEntityCards(c, p, b.Language, b.Code(), b)
}

func (w *workbench) renderEntityCards(c *ui.Context, p palette, kind, code string, fallback *markdown.CodeBlockView) {
	type member struct {
		text, badge, delta string
	}
	type entity struct {
		name, stereotype, delta string
		members                 []member
	}
	var entities []entity
	entityIdx := map[string]int{}
	var relations []string
	ensureEntity := func(name, stereo, delta string) int {
		name = strings.Trim(strings.TrimSpace(name), "{}")
		if idx, ok := entityIdx[name]; ok {
			return idx
		}
		idx := len(entities)
		entityIdx[name] = idx
		entities = append(entities, entity{name: name, stereotype: stereo, delta: delta})
		return idx
	}
	cur := -1
	inBraces := false
	for _, raw := range nonBlankLines(code) {
		trimmed := strings.TrimSpace(raw)
		lower := strings.ToLower(trimmed)
		if lower == "erdiagram" || lower == "classdiagram" || strings.HasPrefix(lower, "direction ") {
			continue
		}
		if trimmed == "}" {
			inBraces = false
			cur = -1
			continue
		}
		indented := inBraces || len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t')
		delta, line := splitDeltaMarker(trimmed)
		if !indented && (strings.Contains(line, "--") || strings.Contains(line, "..") || strings.Contains(line, "->")) {
			relations = append(relations, line)
			cur = -1
			continue
		}
		if !indented {
			stereo := "Entity"
			if strings.EqualFold(kind, "class") {
				stereo = "Class"
			}
			for _, kw := range []string{"interface ", "class ", "enum ", "struct "} {
				if strings.HasPrefix(strings.ToLower(line), kw) {
					stereo = strings.Title(strings.TrimSpace(kw))
					line = strings.TrimSpace(line[len(kw):])
				}
			}
			if strings.HasSuffix(line, "{") {
				inBraces = true
				line = strings.TrimSpace(strings.TrimSuffix(line, "{"))
			}
			cur = ensureEntity(line, stereo, delta)
			continue
		}
		if cur < 0 {
			continue
		}
		badge := ""
		for _, tag := range []string{"PK", "FK", "UK"} {
			if strings.Contains(line, " "+tag) || strings.HasSuffix(line, tag) {
				badge = tag
				break
			}
		}
		entities[cur].members = append(entities[cur].members, member{text: line, badge: badge, delta: delta})
	}
	if len(entities) == 0 && len(relations) == 0 {
		if fallback != nil {
			markdown.DefaultCodeBlock(c, fallback)
		}
		return
	}
	ui.Column(c).FillWidth().Radius(12).Background(p.raised).Border(1, p.lineSoft).Padding(14, 16).Gap(12).Children(func() {
		if len(entities) > 0 {
			ui.Row(c).FillWidth().Gap(10).Wrap().AlignItems(ui.Start).Children(func() {
				for idx, ent := range entities {
					ent, idx := ent, idx
					ui.Column(c.Key("ent:"+strconv.Itoa(idx))).Grow(1).Basis(210).MinWidth(175).Radius(9).Background(p.soft).Border(1, p.line).Clip().Children(func() {
						ui.Row(c).Padding(7, 10).Gap(6).AlignItems(ui.Center).Background(p.canvas.Alpha(0.35)).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
							if ent.delta != "" {
								dt, mark := deltaTone(p, ent.delta)
								ui.Text(c, mark).Font("Menlo").FontSize(11).FontWeight(700).TextColor(dt)
							}
							ui.Text(c, ent.name).Font("Menlo").FontSize(12).FontWeight(700).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
							ui.Text(c, ent.stereotype).FontSize(10).TextColor(p.faint)
						})
						if len(ent.members) == 0 {
							ui.Text(c, "—").FontSize(11).TextColor(p.faint).Padding(6, 10)
						} else {
							ui.Column(c).Padding(6, 10).Gap(4).Children(func() {
								for mIdx, m := range ent.members {
									m, mIdx := m, mIdx
									ui.Row(c.Key("m:" + strconv.Itoa(mIdx))).Gap(6).AlignItems(ui.Center).Children(func() {
										if m.delta != "" {
											dt, mark := deltaTone(p, m.delta)
											ui.Text(c, mark).Font("Menlo").FontSize(10.5).FontWeight(700).TextColor(dt)
										}
										ui.Text(c, m.text).Font("Menlo").FontSize(11).TextColor(p.muted).Grow(1).SingleLine().Ellipsis("…").Selectable()
										if m.badge != "" {
											microTag(c, p, m.badge, p.accentInk)
										}
									})
								}
							})
						}
					})
				}
			})
		}
		if len(relations) > 0 {
			ui.Column(c).FillWidth().Gap(4).Padding(8, 10).Radius(8).Background(p.soft).Children(func() {
				ui.Text(c, w.tr("关联关系", "Relationships")).FontSize(11).FontWeight(600).TextColor(p.muted)
				for rIdx, rel := range relations {
					ui.Text(c.Key("rel:"+strconv.Itoa(rIdx)), rel).Font("Menlo").FontSize(11.5).TextColor(p.ink).Wrap().Selectable()
				}
			})
		}
	})
}
