package desktop

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

const timelineMinimapMinGutter float32 = 34

// timelineMinimapGutter is the space to the left of the centered reading
// column. The minimap lives there, and hides when a narrow window leaves
// too little of it.
func timelineMinimapGutter(chatW, horizontalPad float32) float32 {
	inner := chatW - 2*horizontalPad
	if chatW <= 0 || inner <= 0 {
		return 0
	}
	content := min(readingWidth, inner)
	return (chatW - content) / 2
}

func userTurnIndexes(entries []conversationEntry) []int {
	turns := make([]int, len(entries))
	for i := range entries {
		turns[i] = i
	}
	return turns
}

// minimapCurrent is the user turn whose message is in view, or the one being
// read when the viewport sits in its reply. first/last come from ListState.Visible.
func minimapCurrent(turns []int, first, last int) int {
	if len(turns) == 0 {
		return 0
	}
	if last < first {
		return len(turns) - 1
	}
	preceding := 0
	for i, row := range turns {
		if row >= first {
			if row <= last {
				return i
			}
			return preceding
		}
		preceding = i
	}
	return preceding
}

var previewMarkup = strings.NewReplacer("**", "", "__", "", "`", "", "#", "", "> ", "", "|", " ", "*", "")

// replyPreviewLines is a plain-text gist of a reply for the hover card:
// Markdown markers dropped, lines joined, cut at about 90 characters.
func replyPreviewLines(output string) string {
	var kept []string
	inFence := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence || line == "" || strings.Trim(line, "-|: ") == "" {
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(previewMarkup.Replace(line), "-0123456789. "))
		if line != "" {
			kept = append(kept, line)
		}
		if len(kept) == 3 {
			break
		}
	}
	return truncateRunes(strings.Join(kept, " "), 80)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

func (w *workbench) timelineMinimap(c *ui.Context, p palette, entries []conversationEntry, state *ui.ListState, horizontal float32) {
	turns := userTurnIndexes(entries)
	if len(turns) < 3 {
		return
	}
	gutter := timelineMinimapGutter(w.chatColumnWidth(c), horizontal)
	if gutter < timelineMinimapMinGutter {
		return
	}
	first, last := state.Visible()
	current := minimapCurrent(turns, first, last)
	hoverIndex := w.timelineNav.minimapHover - 1
	seen := 0
	left := max(float32(2), gutter-34)
	// A quiet column of hairline ticks, one per user turn. The current turn
	// is a longer ink tick; neighbours of the hovered tick grow a little so
	// the pointer reads like a magnifier. Step arrows only appear on hover.
	rail := ui.Column(c.Key("timeline-minimap")).Absolute().Left(left).Top(48).Bottom(24).Width(26).Justify(ui.Center).AlignItems(ui.End).Gap(2)
	rail.Label(w.tr3("对话轮次", "Conversation turns", "会話のターン"))
	active := rail.Hovered() || rail.FocusWithin()
	rail.Children(func() {
		step := func(key, label string, icon *ui.SVG, disabled bool, to int) {
			btn := ui.Button(c.Key(key), "").Label(label).Tooltip(label).Size(20, 16).Padding(0).Radius(5).
				Border(0, p.line).Background(ui.Transparent).Justify(ui.Center)
			if btn.Hovered() && !disabled {
				btn.Background(p.rowHover)
			}
			btn.Opacity(btn.Animate("minimap-step", map[bool]float32{true: 1, false: 0}[active && !disabled], 140*time.Millisecond))
			btn.Disabled(disabled || !active)
			btn.Children(func() { ui.Icon(c, icon).FontSize(12).TextColor(p.muted) })
			if btn.Clicked() {
				w.jumpUserTurn(turns, to)
			}
		}
		step("minimap-prev", w.tr3("上一轮", "Previous turn", "前のターン"), upIcon, current <= 0, current-1)
		ui.Column(c.Key("minimap-dots")).Gap(0).AlignItems(ui.End).Padding(2, 0).Children(func() {
			for i, row := range turns {
				i, row := i, row
				width := float32(8)
				color := p.faint.Alpha(0.35)
				if hoverIndex >= 0 {
					switch absInt(i - hoverIndex) {
					case 0:
						width = 18
					case 1:
						width = 12
					}
				}
				if i == current {
					color = p.ink.Alpha(0.85)
					width = max(width, 16)
				}
				entry := entries[row]
				label := fmt.Sprintf(w.tr3("跳到第 %d 轮", "Jump to turn %d", "%d 番目のターンへ"), i+1)
				// The hit target is a full-width 8px band; the visible tick sits at its right edge.
				tick := ui.Button(c.Key("minimap-dot:"+entry.id), "").Label(label).Width(26).Height(8).Padding(0).
					Border(0, p.line).Background(ui.Transparent).Justify(ui.End).AlignItems(ui.Center)
				if tick.Hovered() {
					seen = i + 1
					color = p.ink
				}
				hovering := seen == i+1
				tick.Children(func() {
					ui.Box(c).Width(tick.Animate("minimap-w", width, 120*time.Millisecond)).Height(2).Radius(1).Background(color)
					if hovering {
						w.minimapPreview(c, p, entries, turns, i)
					}
				})
				if tick.Clicked() {
					w.jumpUserTurn(turns, i)
				}
			}
		})
		step("minimap-next", w.tr3("下一轮", "Next turn", "次のターン"), downIcon, current >= len(turns)-1, current+1)
		w.timelineNav.minimapHover = seen
		if active {
			if c.Shortcut(0, ui.KeyUp) {
				w.jumpUserTurn(turns, current-1)
			}
			if c.Shortcut(0, ui.KeyDown) {
				w.jumpUserTurn(turns, current+1)
			}
		}
	})
}

func (w *workbench) minimapPreview(c *ui.Context, p palette, entries []conversationEntry, turns []int, index int) {
	if index < 0 || index >= len(turns) {
		return
	}
	entry := entries[turns[index]]
	title := strings.TrimSpace(entry.inputText())
	title = strings.ReplaceAll(title, "\n", " ")
	if title == "" {
		title = w.tr("用户消息", "User message")
	}
	card := ui.Column(c.Key("minimap-preview")).Absolute().Left(38).Top(-22).Width(280).Padding(9, 12).Gap(3).Radius(10).Background(p.raised).Border(1, p.line).Shadow(0, 10, 28, 0, ui.Hex("#00000040")).PassThrough()
	card.Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, previewMarkup.Replace(title)).FontSize(13).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
			ui.Text(c, fmt.Sprintf("%d/%d", index+1, len(turns))).FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
		})
		if summary := replyPreviewLines(entry.run.Output); summary != "" && entry.pending == nil {
			ui.Text(c, summary).FontSize(12).LineHeight(1.4).TextColor(p.muted).Wrap().MaxLines(2).Ellipsis("…")
		}
	})
}

func (w *workbench) jumpUserTurn(turns []int, index int) {
	if len(turns) == 0 {
		return
	}
	if index < 0 {
		index = 0
	}
	if index >= len(turns) {
		index = len(turns) - 1
	}
	w.logState(w.selected).ScrollTo(turns[index], ui.Start)
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
