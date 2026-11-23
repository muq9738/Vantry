package desktop

import (
	"strings"
	"unicode/utf16"

	"github.com/egoist/mygo/ui"
)

const (
	composerChipPadTop    float32 = 10
	composerChipPadRight  float32 = 12
	composerChipPadBottom float32 = 6
	composerChipPadLeft   float32 = 12
	composerChipFontSize  float32 = 14
	composerChipLeading   float32 = 1.5
)

// paintComposerChips draws a rounded wash behind each mention. TextRange can
// only recolor glyphs, so the wash is a separate shape aligned with the
// field's own line breaks. It is skipped once the field scrolls or the text
// is taller than the visible rows: estimated line heights would miss.
func paintComposerChips(p *ui.Painter, r ui.Rect, comp *composer, pal palette) {
	if comp == nil || comp.draft == "" || len(comp.mentions) == 0 {
		return
	}
	lines := &comp.chipLines
	if lines.Count() == 0 {
		return
	}
	visible := r.H - composerChipPadTop - composerChipPadBottom
	if visible <= 0 || comp.chipScroll.Y > 0.5 || lines.Height() > visible+0.5 {
		return
	}
	ranges := comp.mentionRanges(pal)
	if len(ranges) == 0 {
		return
	}
	font := ui.Font{Size: composerChipFontSize, LineHeight: composerChipLeading}
	lineStart := 0
	logical := strings.Split(comp.draft, "\n")
	for i, line := range logical {
		if i >= lines.Count() {
			break
		}
		rows := lines.Rows(i)
		if len(rows) == 0 {
			rows = []int{0}
		}
		lineRunes := []rune(line)
		lineH := lines.Top(i+1) - lines.Top(i)
		rowH := lineH / float32(len(rows))
		for row, start := range rows {
			end := len(lineRunes)
			if row+1 < len(rows) {
				end = rows[row+1]
			}
			if start < 0 {
				start = 0
			}
			if end > len(lineRunes) {
				end = len(lineRunes)
			}
			if start > end {
				start = end
			}
			paintChipRow(p, r, font, string(lineRunes[start:end]), lineStart+start, ranges, composerChipPadTop+lines.Top(i)+float32(row)*rowH-comp.chipScroll.Y)
		}
		lineStart += len(lineRunes) + 1
	}
}

func paintChipRow(p *ui.Painter, field ui.Rect, font ui.Font, row string, rowStart int, ranges []ui.TextRange, top float32) {
	if row == "" {
		return
	}
	var spans []ui.Span
	at := 0
	rowEnd := rowStart + len([]rune(row))
	for _, rg := range ranges {
		if rg.End <= rowStart || rg.Start >= rowEnd || rg.Color.A == 0 {
			continue
		}
		from := max(rg.Start, rowStart) - rowStart
		to := min(rg.End, rowEnd) - rowStart
		if from > at {
			spans = append(spans, ui.Span{Text: runeSlice(row, at, from)})
		}
		spans = append(spans, ui.Span{Text: runeSlice(row, from, to), Weight: rg.Weight, Color: rg.Color})
		at = to
	}
	if len(spans) == 0 {
		return
	}
	if at < len([]rune(row)) {
		spans = append(spans, ui.Span{Text: runeSlice(row, at, len([]rune(row)))})
	}
	layout := ui.ShapeRichText(spans, font, 0)
	originX := field.X + composerChipPadLeft
	originY := field.Y + top
	cursor := 0
	for _, sp := range spans {
		n := utf16Len(sp.Text)
		if sp.Color.A > 0 && n > 0 {
			for _, box := range layout.SelectionRects(ui.TextInputRange{Start: cursor, End: cursor + n}) {
				box.X += originX - 2
				box.Y += originY + 1
				box.W += 4
				box.H -= 2
				if box.W > 1 && box.H > 1 {
					p.Fill(box, sp.Color.Alpha(0.16), 5)
				}
			}
		}
		cursor += n
	}
}

func runeSlice(s string, from, to int) string {
	runes := []rune(s)
	if from < 0 {
		from = 0
	}
	if to > len(runes) {
		to = len(runes)
	}
	if from >= to {
		return ""
	}
	return string(runes[from:to])
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
