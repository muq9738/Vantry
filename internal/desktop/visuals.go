package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/egoist/mygo/ui/markdown/parser"
	"github.com/muq9738/Vantry/internal/desktop/webcanvas"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	maxInlineWebCanvases = 2
	inlineDefaultHeight  = float32(320)
	inlineMinHeight      = float32(180)
	inlineMaxHeight      = float32(720)
)

type inlineWebSlot struct {
	key        string
	canvas     *webcanvas.Canvas
	height     float32
	lastUsed   uint64
	renderDark bool
	selElem    string
	selText    string
	errText    string
}

// visualState keeps what visual replies cost to make: parsed chart specs and
// laid-out diagrams, by their source.
type visualState struct {
	markdowns map[string]*markdown.Options
	charts    visualCache[parsedChart]
	diagrams  visualCache[*mmDiagram]
	uis       visualCache[parsedUI]
	// expanded is the visual shown full size, by its fence and source.
	expanded     bool
	expandedLang string
	expandedInfo string
	expandedCode string
	zoom         float32
	// lightbox is the sent-image viewer: the message's images and the one shown.
	lightbox      []api.Attachment
	lightboxIndex int
	// askAnswers survives a restart: thread + message + block text.
	askAnswers       map[string]askAnswerRecord
	renderingMessage string
	askHotkey        askHotkey
	askCustomFocus   ui.Handle
	// L2 offscreen snapshot image cache decoded once per content+theme hash.
	snapshots map[string]ui.ImageSource
	// 2-slot LRU pool for live interactive inline WKWebView HTML cards.
	inlineSlots    []*inlineWebSlot
	inlineOrder    uint64
	seenInlineHTML map[string]bool
}

type diagramView struct {
	source    bool
	collapsed bool
	wantLive  bool
}

// markdownOptions sets the reply look once per palette: replies keep their
// parse and layout caches while the theme is unchanged.
func (w *workbench) markdownOptions(p palette) {
	o := &w.widgets.markdown
	if o.Code == nil {
		o.Code = map[string]func(*ui.Context, *markdown.CodeBlockView){
			"mermaid":      w.mermaidBlock,
			"svg":          w.svgBlock,
			"html":         w.htmlArtifactBlock,
			"vantry-html":  w.htmlArtifactBlock,
			"artifact":     w.htmlArtifactBlock,
			"chart":        w.chartBlock,
			"vantry-chart": w.chartBlock,
			"vantry-ui":    w.uiBlockView,
			"ui":           w.uiBlockView,
			"callout":      w.calloutBlock,
			"kv":           w.kvBlock,
			"timeline":     w.timelineBlock,
			"tree":         w.treeBlock,
			"limits":       w.limitsBlock,
			"api":          w.apiBlock,
			"annot":        w.annotBlock,
			"ask":          w.askBlock,
			"dispatch":     w.dispatchBlock,
			"orchestrate":  w.dispatchBlock,
			"flow":         w.flowAliasBlock,
			"state":        w.flowAliasBlock,
			"sequence":     w.sequenceAliasBlock,
			"er":           w.erAndClassBlock,
			"class":        w.erAndClassBlock,
		}
		// Keep token arrival immediate. Incremental Markdown already avoids
		// re-parsing completed blocks; pacing every word adds whole-view frames.
		o.Reveal = &markdown.Reveal{Unit: markdown.AtOnce, Fade: -1}
		o.Extensions = parser.Default | parser.ExtDollarMath
		o.Math = w.mathBlock
		o.InlineMath = w.inlineMathObject
	}
	if o.Theme == nil || w.widgets.markdownPalette != p {
		w.widgets.markdownPalette = p
		// T3 Code / Codex reading typography: crisp primary ink, comfortable
		// 1.68 line height, restrained heading scale, and recessed code blocks.
		o.Theme = &markdown.Theme{
			FontSize: 15, LineHeight: 1.68, CodeLineHeight: 1.55,
			Text: p.ink.Mix(p.canvas, 0.04), Muted: p.muted, Link: p.accentInk, Strong: p.ink, StrongWeight: 620, LinkWeight: 500,
			BlockSpacing: 0.92, ItemSpacing: 0.32, ListIndent: 1.38,
			Code: p.ink.Mix(p.canvas, 0.05), CodeBackground: p.soft, CodeSize: 0.87,
			CodeBlockBackground: p.code, CodeBlockBorder: p.lineSoft, CodeBlockRadius: 10, CodeBlockPadding: 0.9,
			QuoteText: p.muted, QuoteBar: p.line, QuoteBarWidth: 2,
			Rule:        p.lineSoft,
			TableBorder: p.lineSoft, TableHeader: p.soft, TableCellPadding: [2]float32{0.82, 0.5},
			Headings: [6]markdown.HeadingStyle{
				{Size: 1.28, Weight: 650, Top: 1.35, Bottom: 0.45},
				{Size: 1.14, Weight: 630, Top: 1.25, Bottom: 0.4},
				{Size: 1.04, Weight: 610, Top: 1.15, Bottom: 0.35},
				{Size: 1, Weight: 600, Top: 1.05, Bottom: 0.3},
				{Size: 0.95, Weight: 600, Top: 1.0, Bottom: 0.3},
				{Size: 0.92, Weight: 600, Top: 1.0, Bottom: 0.3},
			},
		}
	}
}

func (w *workbench) chartBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, "正在生成图表…")
		return
	}
	code := b.Code()
	info := b.Info
	cacheKey := info + "\x00" + code
	parsed := w.visuals.charts.get(cacheKey, func() parsedChart {
		spec, err := parseChartSpecWithInfo(info, code)
		return parsedChart{spec, err}
	})
	if parsed.err != nil {
		visualError(c, p, parsed.err.Error(), code)
		return
	}
	renderChartWithOptions(c, p, parsed.spec, func() {
		if iconButton(c, p, "expand", "放大", expandIcon).Size(24, 24).Padding(5).Clicked() {
			w.expandVisualWithInfo("chart", info, code)
		}
		if iconButton(c, p, "copy-chart", "复制图表数据", copyIcon).Size(24, 24).Padding(5).Clicked() {
			c.WriteClipboard(code)
		}
	}, nil)
}

func snapshotCacheKey(kind, source string, dark bool) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + fmt.Sprint(dark) + "\x00" + source))
	return kind + ":" + hex.EncodeToString(sum[:12])
}

func (w *workbench) snapshotImage(kind, source string, dark bool) (ui.ImageSource, bool) {
	if w.webSnapshotter == nil {
		return nil, false
	}
	key := snapshotCacheKey(kind, source, dark)
	if img, ok := w.visuals.snapshots[key]; ok {
		return img, true
	}
	if res, ok := w.webSnapshotter.Get(key); ok {
		if res.Err != nil || len(res.PNG) == 0 {
			return nil, false
		}
		if img, err := ui.DecodeImage(res.PNG); err == nil {
			if w.visuals.snapshots == nil {
				w.visuals.snapshots = map[string]ui.ImageSource{}
			}
			w.visuals.snapshots[key] = img
			return img, true
		}
		return nil, false
	}
	w.webSnapshotter.Request(webcanvas.SnapshotRequest{
		Key: key, Kind: kind, Source: source, Dark: dark,
	})
	return nil, false
}

func clipRunes(s string, limit int) string {
	r := []rune(strings.TrimSpace(s))
	if limit <= 0 || len(r) <= limit {
		return string(r)
	}
	return string(r[:limit]) + "…"
}

func inlineHTMLKey(source string) string {
	sum := sha256.Sum256([]byte("html\x00" + source))
	return hex.EncodeToString(sum[:12])
}

func (s *visualState) findInlineSlot(key string) *inlineWebSlot {
	for _, slot := range s.inlineSlots {
		if slot.key == key {
			return slot
		}
	}
	return nil
}

func (s *visualState) destroyInlineCanvases() {
	for _, slot := range s.inlineSlots {
		if slot != nil && slot.canvas != nil {
			slot.canvas.Destroy()
		}
	}
	s.inlineSlots = nil
	s.seenInlineHTML = nil
}

func (w *workbench) ensureInlineSlot(key, source string, dark bool) *inlineWebSlot {
	s := &w.visuals
	if s.seenInlineHTML == nil {
		s.seenInlineHTML = make(map[string]bool)
	}
	s.seenInlineHTML[key] = true
	if slot := s.findInlineSlot(key); slot != nil {
		s.inlineOrder++
		slot.lastUsed = s.inlineOrder
		return slot
	}
	if len(s.inlineSlots) >= maxInlineWebCanvases {
		evictIdx := 0
		for i := 1; i < len(s.inlineSlots); i++ {
			if s.inlineSlots[i].lastUsed < s.inlineSlots[evictIdx].lastUsed {
				evictIdx = i
			}
		}
		if s.inlineSlots[evictIdx].canvas != nil {
			s.inlineSlots[evictIdx].canvas.Destroy()
		}
		s.inlineSlots = append(s.inlineSlots[:evictIdx], s.inlineSlots[evictIdx+1:]...)
	}
	s.inlineOrder++
	slot := &inlineWebSlot{
		key:        key,
		height:     inlineDefaultHeight,
		lastUsed:   s.inlineOrder,
		renderDark: dark,
	}
	slot.canvas = webcanvas.NewCanvas(func(evt webcanvas.BridgeEvent) {
		w.post(func() {
			switch evt.Type {
			case "resize":
				if evt.Height > 0 {
					h := float32(evt.Height)
					h = max(inlineMinHeight, min(inlineMaxHeight, h))
					slot.height = h
				}
			case "select":
				slot.selElem = evt.Selector
				slot.selText = evt.Text
			case "error":
				slot.errText = evt.Message
			case "ready":
				slot.errText = ""
			}
		})
	})
	_ = slot.canvas.Render(webcanvas.Payload{
		Kind:   "html",
		Source: source,
		Dark:   dark,
	})
	s.inlineSlots = append(s.inlineSlots, slot)
	return slot
}

func studioVisualFrame(c *ui.Context, p palette) ui.Element {
	shadow := ui.RGBA(0, 0, 0, 0.04)
	if p.dark {
		shadow = ui.RGBA(0, 0, 0, 0.24)
	}
	return ui.Column(c).FillWidth().Radius(16).Background(p.raised).Border(1, p.lineSoft).Shadow(0, 4, 14, 0, shadow)
}

func (w *workbench) svgBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, "正在生成 SVG…")
		return
	}
	raw := b.Code()
	clean := webcanvas.SanitizeSVG(raw)
	attrs, _ := parseFenceInfoAttrs(b.Info)
	title := nonEmptyString(attrs["title"], "矢量图 SVG")
	if m := reHTMLTitle.FindStringSubmatch(clean); len(m) == 2 && strings.TrimSpace(m[1]) != "" && attrs["title"] == "" {
		title = strings.TrimSpace(m[1])
	}
	frame := studioVisualFrame(c, p)
	view := *ui.Local(frame, diagramView{}, func() *diagramView { return &diagramView{} })
	frame.Children(func() {
		header := ui.Row(c).Height(38).Padding(0, 10, 0, 16).Gap(4).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft)
		header.Children(func() {
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
			lbl := "查看源码"
			if view.source {
				lbl = "预览图"
			}
			if quietButton(c, p, "svg-toggle-src", lbl).Height(24).Clicked() {
				view.source = !view.source
			}
			if iconButton(c, p, "copy-svg", "复制 SVG", copyIcon).Size(24, 24).Padding(5).Clicked() {
				c.WriteClipboard(clean)
			}
		})
		if view.source {
			b.Body(c)
			return
		}
		if img, ok := w.snapshotImage("svg", clean, p.dark); ok {
			ui.Column(c).Padding(12).AlignItems(ui.Center).Children(func() {
				ui.Image(c, img).Fit(ui.Contain).MaxWidth(712).MaxHeight(420)
			})
			return
		}
		ui.Column(c).Padding(12, 16).Gap(4).Children(func() {
			ui.Text(c, w.tr("离线安全 SVG 图形（已移除潜在脚本）", "Sanitized offline SVG graphic")).FontSize(11.5).TextColor(p.accentInk)
			ui.Text(c, fmt.Sprintf("%d 字符 · 可查看源码或复制 SVG", len([]rune(clean)))).FontSize(11).TextColor(p.muted).Wrap()
		})
	})
}

func (w *workbench) htmlArtifactBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, "正在构建交互产物…")
		return
	}
	raw := b.Code()
	attrs, _ := parseFenceInfoAttrs(b.Info)
	title := nonEmptyString(attrs["title"], "交互式 HTML 产物")
	if m := reHTMLTitle.FindStringSubmatch(raw); len(m) == 2 && strings.TrimSpace(m[1]) != "" && attrs["title"] == "" {
		title = strings.TrimSpace(m[1])
	}
	key := inlineHTMLKey(raw)
	liveCapable := w.window != nil && mygo.WebViewAvailable()
	frame := studioVisualFrame(c, p)
	view := *ui.Local(frame, diagramView{}, func() *diagramView { return &diagramView{} })
	var slot *inlineWebSlot
	if liveCapable && !view.source {
		slot = w.visuals.findInlineSlot(key)
		if slot == nil && !view.collapsed && (!w.visuals.seenInlineHTML[key] || view.wantLive) {
			view.wantLive = false
			slot = w.ensureInlineSlot(key, raw, p.dark)
		}
	}
	liveShowing := liveCapable && !view.source && !view.collapsed && slot != nil

	frame.Children(func() {
		header := ui.Row(c).Height(38).Padding(0, 10, 0, 16).Gap(4).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft)
		header.Children(func() {
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
			badgeBg := p.accent.Alpha(0.12)
			badgeInk := p.accentInk
			if liveShowing {
				badgeBg = p.success.Alpha(0.12)
				badgeInk = p.success
			}
			ui.Text(c, w.tr("离线沙箱 HTML", "Offline Sandbox")).FontSize(10).FontWeight(600).TextColor(badgeInk).Padding(2, 7).Radius(999).Background(badgeBg).NoWrap()
			if liveCapable {
				liveLbl := "实时交互"
				if liveShowing {
					liveLbl = "折叠实时"
				}
				if quietButton(c, p, "html-toggle-live", liveLbl).Height(24).Clicked() {
					if liveShowing {
						view.collapsed = true
					} else {
						view.source = false
						view.collapsed = false
						view.wantLive = true
						slot = w.ensureInlineSlot(key, raw, p.dark)
						liveShowing = slot != nil
					}
				}
			}
			lbl := "查看源码"
			if view.source {
				lbl = "预览"
			}
			if quietButton(c, p, "html-toggle-src", lbl).Height(24).Clicked() {
				view.source = !view.source
			}
			if iconButton(c, p, "copy-html", "复制 HTML", copyIcon).Size(24, 24).Padding(5).Clicked() {
				c.WriteClipboard(raw)
			}
		})
		if view.source {
			b.Body(c)
			return
		}
		if liveShowing {
			if wv, err := slot.canvas.EnsureInline(w.window, svgHex(p.raised)); err == nil && wv != nil {
				if slot.renderDark != p.dark {
					slot.renderDark = p.dark
					slot.canvas.SetTheme(p.dark)
				}
				h := max(inlineMinHeight, min(inlineMaxHeight, slot.height))
				ui.Column(c).FillWidth().Padding(8).Gap(6).Children(func() {
					ui.WebView(c, wv).FillWidth().Height(h)
					if slot.errText != "" || slot.selElem != "" {
						status := slot.errText
						statusColor := p.danger
						if slot.errText == "" {
							statusColor = p.muted
							status = "已点击元素：" + slot.selElem
							if slot.selText != "" {
								status += " · " + clipRunes(slot.selText, 48)
							}
						}
						ui.Text(c, status).FontSize(11).TextColor(statusColor).SingleLine().Ellipsis("…")
					}
				})
				return
			}
		}
		if img, ok := w.snapshotImage("html", raw, p.dark); ok {
			ui.Column(c).Padding(10).AlignItems(ui.Center).Children(func() {
				ui.Image(c, img).Fit(ui.Contain).MaxWidth(712).MaxHeight(400).Radius(10)
			})
			return
		}
		ui.Column(c).Padding(12, 16).Gap(4).Children(func() {
			ui.Text(c, w.tr("预览生成中；可先查看源码或复制 HTML。", "Preparing the preview; view or copy the HTML meanwhile.")).
				FontSize(11.5).TextColor(p.muted).Wrap()
		})
	})
}

func (w *workbench) renderExtendedMermaidCard(c *ui.Context, b *markdown.CodeBlockView, code string) {
	p := w.widgets.markdownPalette
	title := nonEmptyString(mermaidFirstTitle(code), "Mermaid 扩展图形")
	frame := studioVisualFrame(c, p)
	view := *ui.Local(frame, diagramView{}, func() *diagramView { return &diagramView{} })
	frame.Children(func() {
		header := ui.Row(c).Height(36).Padding(0, 8, 0, 14).Gap(4).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft)
		header.Children(func() {
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
			lbl := "查看源码"
			if view.source {
				lbl = "查看图"
			}
			if quietButton(c, p, "ext-mmd-toggle-src", lbl).Height(24).Clicked() {
				view.source = !view.source
			}
			if iconButton(c, p, "copy-ext-mmd", "复制图形源码", copyIcon).Size(24, 24).Padding(5).Clicked() {
				c.WriteClipboard(code)
			}
		})
		if view.source {
			b.Body(c)
			return
		}
		if img, ok := w.snapshotImage("mermaid", code, p.dark); ok {
			ui.Column(c).Padding(12).AlignItems(ui.Center).Children(func() {
				ui.Image(c, img).Fit(ui.Contain).MaxWidth(712).MaxHeight(420)
			})
			return
		}
		ui.Column(c).Padding(12, 14).Gap(4).Children(func() {
			ui.Text(c, w.tr("离线 Mermaid v11 图形（甘特图 / 脑图 / Git 图 / 时序 / 象限图等）", "Offline Mermaid v11 diagram")).FontSize(11.5).TextColor(p.accentInk)
			ui.Text(c, w.tr("预览生成中；可先查看源码或复制图形源码。", "Preparing the preview; view or copy the diagram source meanwhile.")).FontSize(11).TextColor(p.muted).Wrap()
		})
	})
}

func (w *workbench) mermaidBlock(c *ui.Context, b *markdown.CodeBlockView) {
	if b.Open {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	code := b.Code()
	trimmed := strings.ToLower(strings.TrimSpace(code))
	if strings.HasPrefix(trimmed, "erdiagram") {
		w.renderEntityCards(c, w.widgets.markdownPalette, "er", code, b)
		return
	}
	if strings.HasPrefix(trimmed, "classdiagram") {
		w.renderEntityCards(c, w.widgets.markdownPalette, "class", code, b)
		return
	}
	w.renderMermaidSource(c, b, code)
}

func (w *workbench) renderMermaidSource(c *ui.Context, b *markdown.CodeBlockView, code string) {
	p := w.widgets.markdownPalette
	d := w.diagram(c, code)
	if d.err != nil {
		if isExtendedMermaid(code) {
			w.renderExtendedMermaidCard(c, b, code)
			return
		}
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			ui.Text(c, d.err.Error()).FontSize(11).TextColor(p.muted)
			markdown.DefaultCodeBlock(c, b)
		})
		return
	}
	frame := studioVisualFrame(c, p)
	view := *ui.Local(frame, diagramView{}, func() *diagramView { return &diagramView{} })
	frame.Children(func() {
		header := ui.Row(c).Height(38).Padding(0, 10, 0, 16).Gap(2).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft)
		header.Children(func() {
			ui.Text(c, diagramTitle(d)).FontSize(13).FontWeight(600).TextColor(p.ink).Grow(1)
			tools := ui.Row(c).Gap(2).AlignItems(ui.Center).Shrink(0)
			shown := float32(0)
			if frame.Hovered() {
				shown = 1
			}
			tools.Opacity(tools.AnimateTo("tools", shown, controlEase()))
			tools.Children(func() {
				label := "查看源码"
				if view.source {
					label = "查看图"
				}
				if quietButton(c, p, "toggle-source", label).Height(24).Clicked() {
					view.source = !view.source
				}
				if iconButton(c, p, "expand", "放大查看", expandIcon).Size(26, 26).Clicked() {
					w.expandVisual("mermaid", code)
				}
				if iconButton(c, p, "copy-diagram", "复制图形源码", copyIcon).Size(26, 26).Padding(5).Clicked() {
					c.WriteClipboard(b.Code())
				}
			})
		})
		if view.source {
			b.Body(c)
			return
		}
		ui.Column(c).Padding(12, 14).Children(func() { w.diagramCanvas(c, p, d) })
	})
}

func diagramTitle(d *mmDiagram) string {
	switch d.kind {
	case "flowchart":
		return "流程图"
	case "state":
		return "状态图"
	case "sequence":
		return "时序图"
	case "pie":
		return "饼图"
	}
	return "图"
}

func (w *workbench) diagram(c *ui.Context, code string) *mmDiagram {
	return w.visuals.diagrams.get(code, func() *mmDiagram {
		return parseMermaid(code, func(text string, width float32) (float32, float32) {
			return c.MeasureText(width, ui.Span{Text: text, Size: mmFont})
		})
	})
}

func (w *workbench) diagramCanvas(c *ui.Context, p palette, d *mmDiagram) {
	colors := mermaidColors(p)
	switch {
	case d.pie != nil:
		renderChart(c, p, d.pie, nil)
	case d.flow != nil:
		flow := d.flow
		ui.ScrollHorizontal(c).FillWidth().Children(func() {
			ui.Box(c).Size(flow.width, flow.height).Shrink(0).Role(ui.RoleImage).Label(flow.describe()).
				Draw(func(painter *ui.Painter, r ui.Rect) { flow.paint(painter, r, colors) })
		})
	case d.seq != nil:
		seq := d.seq
		ui.ScrollHorizontal(c).FillWidth().Children(func() {
			ui.Box(c).Size(seq.width, seq.height).Shrink(0).Role(ui.RoleImage).Label(seq.describe()).
				Draw(func(painter *ui.Painter, r ui.Rect) { seq.paint(painter, r, colors) })
		})
	}
}

func (w *workbench) expandedDiagram(c *ui.Context, p palette, d *mmDiagram) {
	if d.pie != nil {
		renderChart(c, p, d.pie, nil)
		return
	}
	zoom := max(0.5, w.visuals.zoom)
	var width, height float32
	var paint func(*ui.Painter, ui.Rect, mmColors)
	var label string
	if d.flow != nil {
		width, height, paint, label = d.flow.width, d.flow.height, d.flow.paint, d.flow.describe()
	}
	if d.seq != nil {
		width, height, paint, label = d.seq.width, d.seq.height, d.seq.paint, d.seq.describe()
	}
	if paint == nil {
		return
	}
	colors := mermaidColors(p)
	ui.Box(c).Size(width*zoom, height*zoom).Shrink(0).Role(ui.RoleImage).Label(label).Draw(func(painter *ui.Painter, rect ui.Rect) {
		painter.Transform(ui.Scaling(zoom, zoom), rect.X, rect.Y, func() {
			paint(painter, ui.Rect{X: rect.X, Y: rect.Y, W: width, H: height}, colors)
		})
	})
}

func visualPending(c *ui.Context, p palette, text string) {
	ui.Row(c).FillWidth().Height(64).Gap(8).Justify(ui.Center).AlignItems(ui.Center).Radius(10).Background(p.soft).Border(1, p.line).Children(func() {
		activityIndicator(c, p.accent)
		ui.Text(c, text).FontSize(12).TextColor(p.muted)
	})
}

func (w *workbench) expandVisual(lang, code string) {
	w.expandVisualWithInfo(lang, "", code)
}

func (w *workbench) expandVisualWithInfo(lang, info, code string) {
	w.visuals.expanded, w.visuals.expandedLang, w.visuals.expandedInfo, w.visuals.expandedCode = true, lang, info, code
	w.visuals.zoom = 1
}

// visualDialog shows a chart or diagram at the window's size.
func (w *workbench) visualDialog(c *ui.Context, p palette) {
	if !w.visuals.expanded {
		return
	}
	width, height := c.Size()
	ui.DialogBase(c, &w.visuals.expanded, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000066"))
		panel.Width(min(1200, width-40)).Height(min(860, height-40)).Radius(14).Background(p.surface).Border(1, p.line)
		ui.Column(c).Fill().Children(func() {
			ui.Row(c).Height(46).Padding(0, 12, 0, 16).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
				title := "图表"
				if w.visuals.expandedLang == "mermaid" {
					title = diagramTitle(w.diagram(c, w.visuals.expandedCode))
				}
				ui.Text(c, title).FontSize(14).Bold().Grow(1)
				if w.visuals.expandedLang == "mermaid" {
					if quietButton(c, p, "zoom-out", "−").Label("缩小图形").Clicked() {
						w.visuals.zoom = max(0.5, w.visuals.zoom-0.25)
					}
					if quietButton(c, p, "zoom-reset", fmt.Sprintf("%.0f%%", w.visuals.zoom*100)).Label("重置缩放").Clicked() {
						w.visuals.zoom = 1
					}
					if quietButton(c, p, "zoom-in", "+").Label("放大图形").Clicked() {
						w.visuals.zoom = min(3, w.visuals.zoom+0.25)
					}
				}
				if iconButton(c, p, "close-visual", "关闭", closeIcon).Clicked() {
					w.visuals.expanded = false
				}
			})
			ui.ScrollBoth(c).Grow(1).Children(func() {
				ui.Column(c).Padding(20, 24).MinWidth(min(1100, width-90)).Children(func() {
					if w.visuals.expandedLang == "mermaid" {
						d := w.diagram(c, w.visuals.expandedCode)
						if d.err == nil {
							w.expandedDiagram(c, p, d)
						}
						return
					}
					w.chartViewWithInfo(c, p, w.visuals.expandedInfo, w.visuals.expandedCode)
				})
			})
		})
	})
}

// isExtendedMermaid reports Mermaid sources the native renderer hands to
// the extended (snapshot) path.
func isExtendedMermaid(code string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(code))
	if trimmed == "" {
		return false
	}
	for _, prefix := range []string{
		"gantt", "mindmap", "timeline", "gitgraph", "quadrantchart",
		"sankey", "xychart", "block-beta", "architecture-beta",
		"c4context", "c4container", "c4component", "c4dynamic", "c4deployment",
		"journey", "requirementdiagram", "zenuml", "kanban", "packet",
		"pie", "flowchart", "graph", "sequencediagram", "statediagram", "erdiagram", "classdiagram",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// mermaidFirstTitle is the diagram's own `title` line, if any.
func mermaidFirstTitle(src string) string {
	for _, ln := range strings.Split(src, "\n") {
		tr := strings.TrimSpace(ln)
		if strings.HasPrefix(strings.ToLower(tr), "title ") {
			return strings.TrimSpace(tr[len("title "):])
		}
	}
	return ""
}

// reHTMLTitle finds an HTML or SVG document's <title>.
var reHTMLTitle = regexp.MustCompile(`(?is)<\s*title[^>]*>(.*?)<\s*/\s*title\s*>`)
