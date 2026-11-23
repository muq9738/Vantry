package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

// A ```vantry-ui (or ```ui) block is JSON for native interactive cards in
// a reply: tabs, a metrics grid, a sortable table, steps, key/values,
// progress meters and action buttons, drawn with MyGo's own components on
// the GPU, with no HTML or web view.
type uiDoc struct {
	Title, Subtitle string
	Blocks          []uiBlock
}

type uiBlock struct {
	Type, Title, Text, Tone string
	Items                   []uiItem
	Columns                 []string
	Rows                    [][]string
	Tabs                    []uiTab
	Children                []uiBlock
}

type uiItem struct {
	Label, Value, Delta, Note, Tone, Status, Time, Text, Command, Kind string
	Progress                                                           float64
}

type uiTab struct {
	Label  string
	Blocks []uiBlock
}

type parsedUI struct {
	doc *uiDoc
	err error
}

func parseUIDoc(code string) (*uiDoc, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(code))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("vantry-ui 不是有效的 JSON：%w", err)
	}
	value = plainNumbers(value)
	doc := &uiDoc{}
	switch v := value.(type) {
	case []any:
		doc.Blocks = decodeUIBlocks(v)
	case map[string]any:
		if _, typed := v["type"]; typed {
			doc.Blocks = []uiBlock{decodeUIBlock(v)}
			break
		}
		doc.Title, doc.Subtitle = stringField(v, "title"), stringField(v, "subtitle", "description")
		for _, key := range []string{"components", "blocks", "children", "items"} {
			if list, ok := v[key].([]any); ok {
				doc.Blocks = decodeUIBlocks(list)
				break
			}
		}
	default:
		return nil, errors.New("vantry-ui 需要对象或数组")
	}
	if len(doc.Blocks) == 0 {
		return nil, errors.New("vantry-ui 没有可显示的组件")
	}
	return doc, nil
}

// plainNumbers turns json.Number into float64 so the shared field helpers
// read them.
func plainNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		f, _ := v.Float64()
		return f
	case []any:
		for i := range v {
			v[i] = plainNumbers(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = plainNumbers(v[k])
		}
	}
	return value
}

func decodeUIBlocks(list []any) []uiBlock {
	var blocks []uiBlock
	for _, item := range list {
		switch v := item.(type) {
		case map[string]any:
			blocks = append(blocks, decodeUIBlock(v))
		case string:
			blocks = append(blocks, uiBlock{Type: "text", Text: v})
		}
	}
	return blocks
}

func numberField(m map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return v, true
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "%"), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func decodeUIItems(raw any) []uiItem {
	var items []uiItem
	switch list := raw.(type) {
	case []any:
		for _, entry := range list {
			switch v := entry.(type) {
			case string:
				items = append(items, uiItem{Label: v, Text: v})
			case map[string]any:
				item := uiItem{
					Label:   stringField(v, "label", "title", "name", "key"),
					Value:   scalarText(firstPresent(v, "value", "text")),
					Delta:   stringField(v, "delta", "change", "trend"),
					Note:    stringField(v, "note", "hint", "caption"),
					Tone:    strings.ToLower(stringField(v, "tone", "variant", "color")),
					Status:  strings.ToLower(stringField(v, "status", "state")),
					Time:    stringField(v, "time", "date", "at"),
					Text:    stringField(v, "text", "description", "detail", "body"),
					Command: stringField(v, "command", "copy", "prefill", "draft", "url"),
					Kind:    strings.ToLower(stringField(v, "kind", "action")),
				}
				if firstPresent(v, "value", "text") == nil {
					item.Value = ""
				}
				if _, ok := v["prefill"]; ok && item.Kind == "" {
					item.Kind = "prefill"
				}
				if _, ok := v["draft"]; ok && item.Kind == "" {
					item.Kind = "prefill"
				}
				if value, ok := numberField(v, "progress", "value", "percent"); ok {
					max, hasMax := numberField(v, "max", "total")
					switch {
					case hasMax && max > 0:
						item.Progress = value / max
					case value > 1:
						item.Progress = value / 100
					default:
						item.Progress = value
					}
				}
				items = append(items, item)
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(list))
		for key := range list {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			items = append(items, uiItem{Label: key, Value: scalarText(list[key])})
		}
	}
	return items
}

func decodeUIBlock(m map[string]any) uiBlock {
	block := uiBlock{
		Type:  strings.ToLower(stringField(m, "type", "kind")),
		Title: stringField(m, "title", "label"),
		Text:  stringField(m, "text", "markdown", "content", "description"),
		Tone:  strings.ToLower(stringField(m, "tone", "variant")),
	}
	items := firstPresent(m, "items", "metrics", "steps", "values", "actions", "buttons", "pairs", "data")
	switch block.Type {
	case "metrics", "stats", "stat":
		block.Type = "metrics"
		block.Items = decodeUIItems(items)
	case "table":
		if cols, ok := firstPresent(m, "columns", "headers").([]any); ok {
			for _, col := range cols {
				if colMap, ok := col.(map[string]any); ok {
					block.Columns = append(block.Columns, stringField(colMap, "label", "title", "key", "name"))
				} else {
					block.Columns = append(block.Columns, scalarText(col))
				}
			}
		}
		if rows, ok := firstPresent(m, "rows", "data", "items").([]any); ok {
			for _, row := range rows {
				var cells []string
				switch r := row.(type) {
				case []any:
					for _, cell := range r {
						cells = append(cells, scalarText(cell))
					}
				case map[string]any:
					if len(block.Columns) == 0 {
						for key := range r {
							block.Columns = append(block.Columns, key)
						}
						sort.Strings(block.Columns)
					}
					for _, col := range block.Columns {
						cells = append(cells, scalarText(r[col]))
					}
				}
				block.Rows = append(block.Rows, cells)
			}
		}
	case "steps", "timeline":
		block.Items = decodeUIItems(items)
	case "kv", "keyvalue", "properties":
		block.Type = "kv"
		block.Items = decodeUIItems(items)
	case "progress", "meters", "meter":
		block.Type = "progress"
		block.Items = decodeUIItems(items)
		if len(block.Items) == 0 {
			if value, ok := numberField(m, "value", "progress", "percent"); ok {
				single := decodeUIItems([]any{m})
				if len(single) > 0 {
					single[0].Label = nonEmptyString(stringField(m, "label"), block.Title)
					block.Items = single
					block.Title = ""
				}
				_ = value
			}
		}
	case "actions", "buttons":
		block.Type = "actions"
		block.Items = decodeUIItems(items)
	case "badges", "badge", "tags":
		block.Type = "badges"
		block.Items = decodeUIItems(items)
		if len(block.Items) == 0 && block.Text != "" {
			block.Items = []uiItem{{Label: block.Text, Tone: block.Tone}}
		}
	case "tabs":
		if tabs, ok := firstPresent(m, "tabs", "items").([]any); ok {
			for _, t := range tabs {
				tab, ok := t.(map[string]any)
				if !ok {
					continue
				}
				entry := uiTab{Label: stringField(tab, "label", "title", "name")}
				if children, ok := firstPresent(tab, "components", "blocks", "children", "content").([]any); ok {
					entry.Blocks = decodeUIBlocks(children)
				} else if text := stringField(tab, "text", "markdown"); text != "" {
					entry.Blocks = []uiBlock{{Type: "markdown", Text: text}}
				}
				block.Tabs = append(block.Tabs, entry)
			}
		}
	case "card", "section", "group":
		block.Type = "card"
		if children, ok := firstPresent(m, "components", "children", "blocks").([]any); ok {
			block.Children = decodeUIBlocks(children)
		}
	case "markdown", "md":
		block.Type = "markdown"
	case "callout", "alert", "note":
		block.Type = "callout"
	default:
		if block.Type == "" {
			block.Type = "text"
		}
	}
	return block
}

// uiBlockView is the markdown code handler for ```vantry-ui and ```ui.
func (w *workbench) uiBlockView(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr("正在生成交互卡片…", "Building the interactive card…"))
		return
	}
	code := b.Code()
	parsed := w.visuals.uis.get(code, func() parsedUI {
		doc, err := parseUIDoc(code)
		return parsedUI{doc, err}
	})
	if parsed.err != nil {
		visualError(c, p, parsed.err.Error(), code)
		return
	}
	w.renderUIDoc(c, p, parsed.doc, code)
}

func (w *workbench) renderUIDoc(c *ui.Context, p palette, doc *uiDoc, code string) {
	card := ui.Column(c).FillWidth().Gap(12).Padding(14, 16).Radius(12).Background(p.raised).Border(1, p.line)
	card.Role(ui.RoleGroup).Label(nonEmptyString(doc.Title, w.tr("交互卡片", "Interactive card")))
	card.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Start).Children(func() {
			ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
				if doc.Title != "" {
					ui.Text(c, doc.Title).FontSize(14).Bold().TextColor(p.ink).Wrap()
				}
				if doc.Subtitle != "" {
					ui.Text(c, doc.Subtitle).FontSize(11).TextColor(p.muted).Wrap()
				}
			})
			copy := iconButton(c, p, "copy-ui", w.tr("复制卡片数据", "Copy card data"), copyIcon).Size(24, 24).Padding(5)
			shown := float32(0)
			if card.Hovered() {
				shown = 1
			}
			copy.Opacity(copy.AnimateTo("tools", shown, controlEase()))
			if copy.Clicked() {
				c.WriteClipboard(code)
			}
		})
		w.uiBlocks(c, p, doc.Blocks, 0)
	})
}

func (w *workbench) uiBlocks(c *ui.Context, p palette, blocks []uiBlock, depth int) {
	for i, block := range blocks {
		w.uiBlockNode(c.Key(fmt.Sprintf("ui:%d:%d", depth, i)), p, block, depth)
	}
}

type uiTabsState struct{ selected int }
type uiTableState struct {
	column int
	desc   bool
}

func (w *workbench) uiBlockNode(c *ui.Context, p palette, block uiBlock, depth int) {
	frame := ui.Column(c).FillWidth().Gap(8)
	frame.Children(func() {
		if block.Title != "" && block.Type != "card" && block.Type != "callout" {
			ui.Text(c, block.Title).FontSize(12).Bold().TextColor(p.ink)
		}
		switch block.Type {
		case "metrics":
			w.uiMetrics(c, p, block.Items)
		case "table":
			w.uiTable(c, p, block)
		case "steps", "timeline":
			w.uiSteps(c, p, block.Items)
		case "kv":
			ui.Column(c).FillWidth().Radius(8).Border(1, p.lineSoft).Background(p.surface).Children(func() {
				for i, item := range block.Items {
					row := ui.Row(c.Key(fmt.Sprint(i))).Gap(10).Padding(6, 10).AlignItems(ui.Start)
					if i > 0 {
						row.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
					}
					row.Children(func() {
						ui.Text(c, item.Label).FontSize(11).TextColor(p.muted).Width(130).Shrink(0).Wrap()
						ui.Text(c, item.Value).FontSize(12).TextColor(p.ink).Grow(1).MinWidth(0).Wrap().Selectable()
					})
				}
			})
		case "progress":
			for i, item := range block.Items {
				ui.Column(c.Key(fmt.Sprint(i))).Gap(4).Children(func() {
					ui.Row(c).Gap(8).Children(func() {
						ui.Text(c, item.Label).FontSize(11).TextColor(p.ink).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
						ui.Text(c, fmt.Sprintf("%.0f%%", math.Round(min(max(item.Progress, 0), 1)*100))).Font("Menlo").FontSize(11).TextColor(p.muted)
					})
					tone := toneColor(p, item.Tone)
					if item.Tone == "" {
						tone = p.accent
					}
					value := float32(min(max(item.Progress, 0), 1))
					ui.Box(c).FillWidth().Height(8).Radius(4).Background(p.lineSoft).Role(ui.RoleProgress).Label(item.Label).
						Value(fmt.Sprintf("%.0f%%", value*100)).Draw(func(painter *ui.Painter, r ui.Rect) {
						if value > 0 {
							painter.Fill(ui.Rect{X: r.X, Y: r.Y, W: max(r.H, r.W*value), H: r.H}, tone, r.H/2)
						}
					})
				})
			}
		case "actions":
			ui.Row(c).Gap(8).Wrap().Children(func() {
				for i, item := range block.Items {
					w.uiAction(c.Key(fmt.Sprint(i)), p, item, i == 0)
				}
			})
		case "badges":
			ui.Row(c).Gap(6).Wrap().Children(func() {
				for _, item := range block.Items {
					pill(c, p, nonEmptyString(item.Label, item.Value), toneColor(p, item.Tone))
				}
			})
		case "tabs":
			state := ui.Local(frame, uiTabsState{}, func() *uiTabsState { return &uiTabsState{} })
			w.uiTabs(c, p, block, *state, depth)
		case "card":
			ui.Column(c).FillWidth().Gap(8).Padding(10, 12).Radius(10).Background(p.surface).Border(1, p.lineSoft).Children(func() {
				if block.Title != "" {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						if block.Tone != "" {
							ui.Box(c).Size(6, 6).Radius(3).Background(toneColor(p, block.Tone))
						}
						ui.Text(c, block.Title).FontSize(12).Bold().TextColor(p.ink)
					})
				}
				if block.Text != "" {
					ui.Text(c, block.Text).FontSize(11).TextColor(p.muted).Wrap()
				}
				if depth < 4 {
					w.uiBlocks(c, p, block.Children, depth+1)
				}
			})
		case "callout":
			tone := toneColor(p, nonEmptyString(block.Tone, "info"))
			ui.Row(c).FillWidth().Gap(8).Padding(9, 12).Radius(9).Background(tone.Alpha(0.08)).Border(1, tone.Alpha(0.3)).Children(func() {
				ui.Icon(c, alertIcon).FontSize(13).TextColor(tone)
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
					if block.Title != "" {
						ui.Text(c, block.Title).FontSize(12).Bold().TextColor(p.ink)
					}
					ui.Text(c, block.Text).FontSize(12).TextColor(p.ink).Wrap()
				})
			})
		case "markdown":
			markdown.View(c, block.Text, w.replyMarkdown(w.selected))
		default:
			ui.Text(c, block.Text).FontSize(12).TextColor(p.ink).Wrap().Selectable()
		}
	})
}

func (w *workbench) uiMetrics(c *ui.Context, p palette, items []uiItem) {
	width, _ := c.Size()
	columns := 4
	if width < 1100 {
		columns = 3
	}
	if width < 860 {
		columns = 2
	}
	columns = max(1, min(columns, len(items)))
	for start := 0; start < len(items); start += columns {
		end := min(start+columns, len(items))
		ui.Row(c.Key(fmt.Sprint(start))).Gap(10).FillWidth().AlignItems(ui.Stretch).Children(func() {
			for _, item := range items[start:end] {
				ui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(4).Padding(10, 12).Radius(10).Background(p.surface).Border(1, p.lineSoft).Children(func() {
					ui.Text(c, item.Label).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
					value := ui.Text(c, item.Value).FontSize(20).Bold().TextColor(p.ink).SingleLine().Ellipsis("…")
					if item.Tone != "" {
						value.TextColor(toneColor(p, item.Tone))
					}
					if item.Delta != "" {
						color := p.muted
						switch {
						case strings.HasPrefix(item.Delta, "+") || strings.HasPrefix(item.Delta, "↑"):
							color = p.success
						case strings.HasPrefix(item.Delta, "-") || strings.HasPrefix(item.Delta, "−") || strings.HasPrefix(item.Delta, "↓"):
							color = p.danger
						}
						ui.Text(c, item.Delta).FontSize(11).TextColor(color)
					}
					if item.Note != "" {
						ui.Text(c, item.Note).FontSize(10).TextColor(p.muted).Wrap()
					}
				})
			}
		})
	}
}

// compareCells sorts numbers as numbers and the rest as text.
func compareCells(a, b string) int {
	fa, errA := strconv.ParseFloat(strings.TrimSuffix(strings.ReplaceAll(a, ",", ""), "%"), 64)
	fb, errB := strconv.ParseFloat(strings.TrimSuffix(strings.ReplaceAll(b, ",", ""), "%"), 64)
	if errA == nil && errB == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

func (w *workbench) uiTable(c *ui.Context, p palette, block uiBlock) {
	columns := len(block.Columns)
	for _, row := range block.Rows {
		columns = max(columns, len(row))
	}
	if columns == 0 {
		return
	}
	table := ui.Column(c).FillWidth().Radius(8).Border(1, p.lineSoft).Background(p.surface).Role(ui.RoleTable).Label(nonEmptyString(block.Title, w.tr("表格", "Table")))
	state := *ui.Local(table, uiTableState{}, func() *uiTableState { return &uiTableState{column: -1} })
	rows := append([][]string(nil), block.Rows...)
	if state.column >= 0 {
		col, desc := state.column, state.desc
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := "", ""
			if col < len(rows[i]) {
				a = rows[i][col]
			}
			if col < len(rows[j]) {
				b = rows[j][col]
			}
			if desc {
				return compareCells(a, b) > 0
			}
			return compareCells(a, b) < 0
		})
	}
	table.Children(func() {
		ui.Row(c.Key("head")).Padding(0, 4).Background(p.soft).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Role(ui.RoleRow).Children(func() {
			for i := 0; i < columns; i++ {
				name := ""
				if i < len(block.Columns) {
					name = block.Columns[i]
				}
				arrow := ""
				if state.column == i {
					arrow = " ▲"
					if state.desc {
						arrow = " ▼"
					}
				}
				head := ui.Button(c.Key(fmt.Sprintf("col:%d", i)), "").Label(w.tr("按 ", "Sort by ")+name+w.tr(" 排序", "")).Grow(1).Basis(0).MinWidth(0).Height(28).Padding(0, 6).
					Border(0, p.line).Background(ui.Transparent).TextColor(p.muted).Role(ui.RoleColumnHeader)
				head.Children(func() {
					ui.Text(c, name+arrow).FontSize(11).Bold().SingleLine().Ellipsis("…")
				})
				if head.Clicked() {
					if state.column == i {
						state.desc = !state.desc
					} else {
						state.column, state.desc = i, false
					}
				}
			}
		})
		for r, row := range rows {
			line := ui.Row(c.Key(fmt.Sprintf("row:%d", r))).Padding(0, 4).Role(ui.RoleRow)
			if r > 0 {
				line.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
			}
			line.Children(func() {
				for i := 0; i < columns; i++ {
					cell := ""
					if i < len(row) {
						cell = row[i]
					}
					ui.Text(c, cell).FontSize(12).TextColor(p.ink).Grow(1).Basis(0).MinWidth(0).Padding(6, 6).Wrap().Selectable().Role(ui.RoleCell)
				}
			})
		}
	})
}

func (w *workbench) uiSteps(c *ui.Context, p palette, items []uiItem) {
	ui.Column(c).FillWidth().Gap(0).Children(func() {
		for i, item := range items {
			last := i == len(items)-1
			ui.Row(c.Key(fmt.Sprint(i))).Gap(10).AlignItems(ui.Stretch).Children(func() {
				color, glyph := p.faint, ""
				switch item.Status {
				case "done", "complete", "completed", "success", "ok":
					color, glyph = p.success, "✓"
				case "active", "running", "current", "in_progress", "doing":
					color, glyph = p.running, "•"
				case "failed", "error", "blocked":
					color, glyph = p.danger, "!"
				}
				ui.Column(c).Width(18).Shrink(0).AlignItems(ui.Center).Children(func() {
					ui.Row(c).Size(18, 18).Radius(9).Justify(ui.Center).AlignItems(ui.Center).Background(color.Alpha(0.16)).Border(1, color.Alpha(0.6)).Children(func() {
						if glyph != "" {
							ui.Text(c, glyph).FontSize(10).Bold().TextColor(color)
						}
					})
					if !last {
						ui.Box(c).Width(1.5).Grow(1).MinHeight(10).Background(p.line)
					}
				})
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Padding(0, 0, 12, 0).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, nonEmptyString(item.Label, item.Value)).FontSize(12).Bold().TextColor(p.ink).Grow(1).MinWidth(0).Wrap()
						if item.Time != "" {
							ui.Text(c, item.Time).FontSize(10).TextColor(p.faint).NoWrap()
						}
					})
					if item.Text != "" && item.Text != item.Label {
						ui.Text(c, item.Text).FontSize(11).TextColor(p.muted).Wrap()
					}
				})
			})
		}
	})
}

func (w *workbench) uiAction(c *ui.Context, p palette, item uiItem, primary bool) {
	label := nonEmptyString(item.Label, item.Command)
	button := ui.Button(c, "").Label(label).Height(28).Padding(0, 12).Radius(8)
	if primary {
		button.Background(p.accent.Alpha(0.14)).TextColor(p.accentInk).Border(1, p.accent.Alpha(0.35))
	} else {
		button.Background(p.surface).TextColor(p.ink).Border(1, p.line)
	}
	prefill := item.Kind == "prefill" || item.Kind == "draft" || item.Kind == "compose"
	if item.Command != "" {
		button.Tooltip(item.Command)
	}
	button.Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			icon := copyIcon
			if prefill {
				icon = editIcon
			}
			ui.Icon(c, icon).FontSize(12)
			ui.Text(c, label).FontSize(12).NoWrap()
		})
	})
	if !button.Clicked() {
		return
	}
	text := nonEmptyString(item.Command, nonEmptyString(item.Text, item.Label))
	if prefill {
		comp := w.composerFor(w.selected)
		w.setDraft(w.selected, strings.TrimSpace(strings.TrimSpace(comp.draft)+"\n"+text))
		w.widgets.composerFocus.Focus()
		return
	}
	c.WriteClipboard(text)
	w.actionNotice = notice{Text: w.tr("已复制：", "Copied: ") + text, Level: noticeInfo}
}

func (w *workbench) uiTabs(c *ui.Context, p palette, block uiBlock, state *uiTabsState, depth int) {
	if len(block.Tabs) == 0 {
		return
	}
	state.selected = min(max(state.selected, 0), len(block.Tabs)-1)
	ui.Row(c).Gap(14).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Role(ui.RoleTabList).Children(func() {
		for i, tab := range block.Tabs {
			selected := state.selected == i
			button := ui.Button(c.Key(fmt.Sprintf("tab:%d", i)), "").Label(tab.Label).Height(30).Padding(0, 2).Radius(0).
				Border(0, p.line).TextColor(p.muted).Role(ui.RoleTab).Selected(selected).Background(ui.Transparent)
			if selected {
				button.TextColor(p.ink).BorderWidth(0, 0, 2, 0).BorderColor(p.accent).Background(ui.Transparent)
			}
			button.Children(func() { ui.Text(c, tab.Label).FontSize(12).NoWrap() })
			if button.Clicked() {
				state.selected = i
			}
		}
	})
	if depth < 4 {
		w.uiBlocks(c.Key(fmt.Sprintf("tab-body:%d", state.selected)), p, block.Tabs[state.selected].Blocks, depth+1)
	}
}
