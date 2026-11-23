package desktop

import (
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// Reply tables fill the reading column and share its width between columns
// in proportion to their content, wrapping long cells instead of scrolling
// sideways (MyGo's built-in table scrolls, and its scrollbar covers the last
// row). Cells are rendered as Markdown so bold, code and links still work.

const (
	tableMinColumn = float32(64)
	tableMaxWeight = float32(520)
)

// tableCellWeight estimates a cell's natural width in DIPs at 15px text:
// wide (CJK, emoji) runes count a full em, others about half.
func tableCellWeight(src string) float32 {
	var w float32
	for _, r := range src {
		switch {
		case r == '*' || r == '`' || r == '_':
		case r > 0x2E80 || unicode.Is(unicode.So, r):
			w += 15
		default:
			w += 8
		}
	}
	return w
}

type tableGrid struct {
	header []string
	rows   [][]string
	align  []parser.Align
}

func readTable(d *parser.Document, i int) tableGrid {
	b := d.Block(i)
	src := d.Source()
	g := tableGrid{align: b.Align}
	cols := len(b.Align)
	for r := i + 1; r < b.Next; r = d.Block(r).Next {
		row := d.Block(r)
		cells := make([]string, cols)
		k := 0
		for cell := r + 1; cell < row.Next && k < cols; cell = d.Block(cell).Next {
			cb := d.Block(cell)
			if cb.Start >= 0 && cb.End <= len(src) && cb.Start <= cb.End {
				cells[k] = src[cb.Start:cb.End]
			}
			k++
		}
		if row.Header && g.header == nil {
			g.header = cells
		} else {
			g.rows = append(g.rows, cells)
		}
	}
	return g
}

// columnWeights are the flex weights of each column: its widest cell,
// clamped so one long cell cannot starve the rest.
func (g tableGrid) columnWeights() ([]float32, []float32) {
	cols := len(g.align)
	weights := make([]float32, cols)
	mins := make([]float32, cols)
	measure := func(cells []string, extra float32) {
		for k := 0; k < cols && k < len(cells); k++ {
			w := tableCellWeight(cells[k]) + extra
			weights[k] = max(weights[k], w)
		}
	}
	measure(g.header, 4)
	for _, row := range g.rows {
		measure(row, 0)
	}
	for k := range weights {
		// The shortest word-ish run a column needs before it wraps badly.
		mins[k] = min(max(weights[k], 40)+20, tableMinColumn+40)
		weights[k] = min(max(weights[k], tableMinColumn), tableMaxWeight)
	}
	return weights, mins
}

func (w *workbench) replyTableBlock(options *markdown.Options) func(c *ui.Context, d *parser.Document, i int) bool {
	// Cells read one step smaller than prose, like T3 Code's tables.
	cellOptions := &markdown.Options{}
	var cellTheme markdown.Theme
	return func(c *ui.Context, d *parser.Document, i int) bool {
		if d.Block(i).Kind != parser.Table {
			return false
		}
		if options.Theme != nil {
			cellTheme = *options.Theme
			cellTheme.FontSize = max(cellTheme.FontSize-1, 12)
			cellTheme.LineHeight = 1.55
			cellOptions.Theme = &cellTheme
		}
		cellOptions.OnLink, cellOptions.LoadImage, cellOptions.ShareSelection = options.OnLink, options.LoadImage, options.ShareSelection
		g := readTable(d, i)
		if len(g.align) == 0 {
			return false
		}
		p := w.widgets.markdownPalette
		weights, mins := g.columnWeights()
		cell := func(key string, src string, k int, header bool) {
			box := ui.Column(c.Key(key)).Basis(0).Grow(weights[k]).MinWidth(mins[k]).Padding(7, 12)
			if k > 0 {
				box.BorderWidth(0, 0, 0, 1).BorderColor(p.lineSoft)
			}
			switch g.align[k] {
			case parser.AlignCenter:
				box.AlignItems(ui.Center)
			case parser.AlignRight:
				box.AlignItems(ui.End)
			}
			box.Children(func() {
				if header {
					src = "**" + trimTableStrong(src) + "**"
				}
				if src == "" || src == "****" {
					return
				}
				markdown.View(c, src, cellOptions)
			})
		}
		table := ui.Column(c.Key("reply-table:"+strconv.Itoa(d.Block(i).Start))).FillWidth().Radius(10).Border(1, p.lineSoft).Clip().Role(ui.RoleTable)
		table.Children(func() {
			if g.header != nil {
				ui.Row(c.Key("thead")).FillWidth().AlignItems(ui.Stretch).Background(p.soft.Alpha(0.7)).Role(ui.RoleRow).Children(func() {
					for k, src := range g.header {
						cell("th:"+strconv.Itoa(k), src, k, true)
					}
				})
			}
			for r, row := range g.rows {
				rowEl := ui.Row(c.Key("tr:" + strconv.Itoa(r))).FillWidth().AlignItems(ui.Stretch).Role(ui.RoleRow)
				if r > 0 || g.header != nil {
					rowEl.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
				}
				if rowEl.Hovered() {
					rowEl.Background(p.rowHover.Alpha(0.45))
				}
				rowEl.Children(func() {
					for k, src := range row {
						cell("td:"+strconv.Itoa(k), src, k, false)
					}
				})
			}
		})
		return true
	}
}

// trimTableStrong avoids "****x****" when a header cell is already bold.
func trimTableStrong(src string) string {
	for len(src) >= 4 && src[:2] == "**" && src[len(src)-2:] == "**" {
		src = src[2 : len(src)-2]
	}
	if !utf8.ValidString(src) {
		return ""
	}
	return src
}
