package desktop

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
)

type timelineNavigation struct {
	open, outline         bool
	query                 string
	focus                 ui.Handle
	matches               []int
	current               int
	lastQuery, lastThread string
	lastSequence          int64
	// minimapHover is the hovered user-turn index plus one; zero is none.
	minimapHover int
}

func (w *workbench) timelineToolbar(c *ui.Context, p palette, entries []conversationEntry) {
	nav := &w.timelineNav
	bar := ui.Row(c.Key("timeline-navigation")).Height(28).Gap(6).AlignItems(ui.Center)
	if nav.open {
		bar.Padding(0, 28)
	} else {
		bar.Gap(2).Shrink(0)
	}
	bar.Children(func() {
		if !nav.open {
			find := iconButton(c, p, "find-turn", w.tr("查找会话内容", "Find in thread"), searchIcon).Size(28, 28).Padding(6)
			find.Tooltip(w.tr("会话内查找 ⌘F", "Find in thread ⌘F"))
			if find.Clicked() {
				nav.open = true
				nav.focus.Focus()
			}
		} else {
			field := ui.TextInput(c.Key("find-content"), &nav.query).Label(w.tr("会话内查找", "Find in thread")).Placeholder(w.tr("查找消息或回复…", "Find messages or replies…")).Bind(&nav.focus).Grow(1).MaxWidth(320).Height(30).Padding(0, 10)
			field.OnTextKey(func(key ui.TextKey) bool {
				if key.Key == ui.KeyEscape {
					nav.open = false
					w.widgets.composerFocus.Focus()
					return true
				}
				if key.Key == ui.KeyEnter && len(nav.matches) > 0 {
					direction := 1
					if key.Mods == ui.Shift {
						direction = -1
					}
					nav.current = (nav.current + direction + len(nav.matches)) % len(nav.matches)
					w.logState(w.selected).ScrollTo(nav.matches[nav.current], ui.Start)
					return true
				}
				return false
			})
			if nav.query != nav.lastQuery || nav.lastThread != w.selected || nav.lastSequence != w.sequence {
				nav.matches = nav.matches[:0]
				q := strings.ToLower(strings.TrimSpace(nav.query))
				if q != "" {
					for i, entry := range entries {
						input := entry.inputText()
						if strings.Contains(strings.ToLower(input), q) || strings.Contains(strings.ToLower(entry.run.Output), q) {
							nav.matches = append(nav.matches, i)
						}
					}
				}
				nav.current = 0
				nav.lastQuery, nav.lastThread, nav.lastSequence = nav.query, w.selected, w.sequence
				if len(nav.matches) > 0 {
					w.logState(w.selected).ScrollTo(nav.matches[0], ui.Start)
				}
			}
			position := 0
			if len(nav.matches) > 0 {
				position = nav.current + 1
			}
			ui.Text(c, fmt.Sprintf(w.tr("%d / %d 轮", "%d / %d turns"), position, len(nav.matches))).FontSize(11).TextColor(p.muted)
			prev := iconButton(c, p, "prev-match", w.tr("上一个结果", "Previous match"), sendIcon).Size(24, 24).Disabled(len(nav.matches) == 0)
			next := iconButton(c, p, "next-match", w.tr("下一个结果", "Next match"), latestIcon).Size(24, 24).Disabled(len(nav.matches) == 0)
			direction := 0
			if prev.Clicked() || field.Shortcut(ui.Shift, ui.KeyEnter) {
				direction = -1
			}
			if next.Clicked() || field.Shortcut(0, ui.KeyEnter) {
				direction = 1
			}
			if direction != 0 && len(nav.matches) > 0 {
				nav.current = (nav.current + direction + len(nav.matches)) % len(nav.matches)
				w.logState(w.selected).ScrollTo(nav.matches[nav.current], ui.Start)
			}
			if iconButton(c, p, "close-find", w.tr("关闭查找", "Close find"), closeIcon).Size(24, 24).Clicked() {
				nav.open = false
				w.widgets.composerFocus.Focus()
			}
		}
		if nav.open {
			ui.Spacer(c)
		}
		outlineTitle := w.tr(fmt.Sprintf("%d 轮对话", len(entries)), w.turns(len(entries)))
		outlineLabel := w.tr("跳转到对话轮次", "Jump to turn")
		// The outline is an icon: the turn count is its tooltip, not a
		// sentence in the title bar.
		trigger := iconButton(c, p, "turn-outline", outlineLabel, outlineIcon).Size(28, 28).Padding(6).Tooltip(outlineTitle)
		if nav.outline {
			trigger.Background(p.selected).TextColor(p.ink)
		}
		if trigger.Clicked() {
			nav.outline = !nav.outline
		}
		ui.PopoverBase(c, trigger, &nav.outline, func(panel ui.Element) {
			width, height := c.Size()
			panel.Width(min(400, width-32)).Padding(6).Radius(12).Background(p.raised).Border(1, p.line).Shadow(0, 10, 30, 0, ui.Hex("#00000040"))
			ui.Text(c, outlineTitle).FontSize(11).FontWeight(600).TextColor(p.faint).Padding(6, 8, 4)
			ui.Scroll(c).MaxHeight(min(360, height-120)).Children(func() {
				for i, entry := range entries {
					input := entry.inputText()
					item := quietButton(c, p, entry.id, "").Label(fmt.Sprintf("%d. %s", i+1, titleFromInstruction(input))).FillWidth().Height(30).Padding(0, 8).Justify(ui.Start)
					item.Children(func() {
						ui.Row(c).Gap(10).AlignItems(ui.Center).FillWidth().MinWidth(0).Children(func() {
							ui.Text(c, fmt.Sprint(i+1)).FontSize(11).TextColor(p.faint).Width(16).TextAlign(ui.End).Shrink(0)
							ui.Text(c, titleFromInstruction(input)).FontSize(12.5).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
						})
					})
					if item.Clicked() {
						w.logState(w.selected).ScrollTo(i, ui.Start)
						nav.outline = false
					}
				}
			})
		})
	})
}

// Quotes are saved text in the draft. They survive source edits and stay
// useful to the model without a separate citation database.
func (w *workbench) quoteReply(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	comp := w.composerFor(w.selected)
	quote := "> " + strings.ReplaceAll(text, "\n", "\n> ") + "\n\n"
	if comp.draft != "" {
		quote = comp.draft + "\n\n" + quote
	}
	w.setDraft(w.selected, quote)
	w.widgets.composerFocus.Focus()
}

func (w *workbench) replyMenu(c *ui.Context, reply ui.Element) {
	reply.ContextMenu(func(m *ui.Menu) {
		m.EditItems()
		parts := reply.SelectedTexts()
		if m.Item(w.tr("引用所选内容", "Quote selection")).Disabled(len(parts) == 0).Chosen() {
			selected := make([]string, 0, len(parts))
			for _, part := range parts {
				r := []rune(part.Text)
				start, end := max(0, part.Start), min(len(r), part.End)
				if start < end {
					selected = append(selected, string(r[start:end]))
				}
			}
			w.quoteReply(strings.Join(selected, "\n\n"))
		}
	})
}
