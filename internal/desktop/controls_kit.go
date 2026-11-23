package desktop

import (
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// Shared control chrome. Buttons, fields, selects, segments and badges share
// one radius, focus ring and motion so settings, dialogs and the composer
// read as one system. Callers may still override size after the helper
// returns; color and motion stay here.

type controlVariant int

const (
	controlGhost controlVariant = iota
	controlPrimary
	controlSecondary
	controlOutline
	controlDestructive
)

type controlSize int

const (
	controlSM controlSize = iota
	controlMD
)

func controlEase() ui.Animation {
	return ui.Animation{Duration: 120 * time.Millisecond, Ease: ui.EaseOut}
}

// kitButton is a labeled button in one of the shared variants.
func kitButton(c *ui.Context, p palette, key, label string, variant controlVariant, size controlSize) ui.Element {
	button := ui.Button(c.Key(key), label).Label(label)
	return styleControl(button, p, variant, size, false)
}

// kitIconButton is a square ghost (or other) button that only shows an icon.
// The label is its accessible name and tooltip.
func kitIconButton(c *ui.Context, p palette, key, label string, icon *ui.SVG, variant controlVariant) ui.Element {
	button := ui.Button(c.Key(key), "").Label(label).Tooltip(label)
	button = styleControl(button, p, variant, controlSM, true)
	button.Children(func() { ui.Icon(c, icon).FontSize(16) })
	return button
}

func styleControl(button ui.Element, p palette, variant controlVariant, size controlSize, icon bool) ui.Element {
	height, font, pad, radius := float32(28), float32(12), float32(10), float32(8)
	if size == controlMD {
		height, font, pad, radius = 32, 13, 12, 8
	}
	if icon {
		height, pad = 30, 7
	}
	rest, hover, press, text := controlColors(p, variant)
	if button.Pressed() {
		rest = press
	} else if button.Hovered() {
		rest = hover
		if variant == controlGhost || variant == controlOutline {
			text = p.ink
		}
	}
	button.Height(height).Padding(0, pad).FontSize(font).Radius(radius).Shrink(0).
		Border(controlBorder(variant), controlBorderColor(p, variant, button)).
		Background(rest).
		TextColor(text)
	if button.FocusVisible() {
		button.Shadow(0, 0, 0, 2, p.accent.Alpha(0.45))
	}
	return button
}

func controlBorder(variant controlVariant) float32 {
	if variant == controlOutline || variant == controlPrimary || variant == controlDestructive {
		return 1
	}
	return 0
}

func controlBorderColor(p palette, variant controlVariant, button ui.Element) ui.Color {
	switch variant {
	case controlPrimary:
		return p.accent
	case controlDestructive:
		return p.danger
	case controlOutline:
		if button.Hovered() {
			return p.muted.Alpha(0.45)
		}
		return p.line
	default:
		return p.line
	}
}

func controlColors(p palette, variant controlVariant) (rest, hover, press, text ui.Color) {
	white := ui.Hex("#FFFFFF")
	switch variant {
	case controlPrimary:
		return p.accent, p.accent.Mix(white, 0.12), p.accent.Mix(ui.Hex("#000000"), 0.12), white
	case controlDestructive:
		return p.danger, p.danger.Mix(white, 0.1), p.danger.Mix(ui.Hex("#000000"), 0.12), white
	case controlSecondary:
		return p.soft, p.selected, p.selected.Mix(p.ink, 0.06), p.ink
	case controlOutline:
		return p.raised, p.soft, p.selected, p.ink
	default:
		return ui.Transparent, p.soft, p.selected, p.muted
	}
}

// kitField paints a text input with the shared height, radius, fill and
// focus or error ring. Pass the input already given its value binding.
func kitField(field ui.Element, p palette, invalid bool) ui.Element {
	field.MinHeight(32).Radius(8).Padding(0, 10).FontSize(13).
		Border(1, p.line).Background(fieldFill(p)).TextColor(p.ink)
	switch {
	case invalid:
		field.BorderColor(p.danger).Shadow(0, 0, 0, 3, p.danger.Alpha(0.18))
	case field.FocusVisible():
		field.BorderColor(p.accent).Shadow(0, 0, 0, 3, p.accent.Alpha(0.22))
	case field.Hovered():
		field.BorderColor(p.muted.Alpha(0.4))
	}
	return field
}

func fieldFill(p palette) ui.Color {
	if p.dark {
		return ui.Hex("#161616")
	}
	return ui.Hex("#FFFFFF")
}

// kitBadge is a compact status chip.
func kitBadge(c *ui.Context, text string, tone ui.Color) {
	ui.Row(c).Height(18).Padding(0, 6).Radius(6).AlignItems(ui.Center).Background(tone.Alpha(0.12)).Shrink(0).Children(func() {
		ui.Text(c, text).FontSize(10).FontWeight(600).TextColor(tone).NoWrap()
	})
}

// kitIndex is the numbered key cap on an ask option (1, 2, 3…).
func kitIndex(c *ui.Context, p palette, n string, selected bool) {
	bg, fg := p.selected, p.ink
	if selected {
		bg, fg = p.accent, ui.Hex("#FFFFFF")
	}
	ui.Row(c).Size(22, 22).Radius(7).Justify(ui.Center).AlignItems(ui.Center).Background(bg).Shrink(0).Children(func() {
		ui.Text(c, n).FontSize(11).FontWeight(650).TextColor(fg)
	})
}

type kitOption struct {
	ID, Label, Detail, Group string
	Disabled                 bool
}

// kitSelectTrigger is the closed face of a select. The caller toggles open
// and fills the label; kitMenu draws the popup.
func kitSelectTrigger(c *ui.Context, p palette, key, label string, open bool) ui.Element {
	button := kitButton(c, p, key, "", controlGhost, controlSM).Label(label).Expanded(open)
	if open {
		button.Background(p.soft).TextColor(p.ink)
	}
	return button
}

// kitMenu is a themed select popup: optional search, group titles, a check
// on the current row, secondary detail, and vertical keyboard focus.
func kitMenu(c *ui.Context, p palette, anchor ui.Element, open *bool, menuLabel string, options []kitOption, current string, searchable bool, onPick func(id string)) {
	if open == nil || !*open {
		return
	}
	width, height := c.Size()
	ui.PopoverBase(c, anchor, open, func(panel ui.Element) {
		panel.AttachTo(anchor, ui.AnchorTopStart, ui.AnchorBottomStart).Margin(0, 0, 6).
			MinWidth(min(280, width-24)).MaxWidth(min(float32(360), width-16)).
			Padding(6).Radius(12).Background(p.raised).Border(1, p.line).
			Shadow(0, 12, 32, 0, ui.Hex("#00000047"))
		if panel.Shortcut(0, ui.KeyEscape) {
			*open = false
			return
		}
		query := ""
		ui.Column(c).Gap(4).Children(func() {
			if searchable {
				box := ui.Column(c)
				q := ui.Local(box, "kit-select-query", func() string { return "" })
				box.Children(func() {
					kitField(ui.TextInput(c.Key("kit-select-search"), q).Label(menuLabel).Placeholder(menuLabel).FillWidth().AutoFocus(), p, false)
				})
				query = strings.ToLower(strings.TrimSpace(*q))
			}
			limit := min(float32(320), height*0.45)
			ui.Scroll(c.Key("kit-select-scroll")).MaxHeight(max(float32(44), limit)).Children(func() {
				ui.Column(c).Gap(1).Role(ui.RoleListBox).Label(menuLabel).FocusGroup(ui.Vertical).Children(func() {
					shown := 0
					lastGroup := ""
					for _, opt := range options {
						if query != "" && !strings.Contains(strings.ToLower(opt.Label+" "+opt.Detail+" "+opt.Group+" "+opt.ID), query) {
							continue
						}
						opt := opt
						if opt.Group != "" && opt.Group != lastGroup {
							lastGroup = opt.Group
							ui.Text(c, opt.Group).FontSize(11).FontWeight(600).TextColor(p.faint).Padding(8, 8, 2)
						}
						chosen := opt.ID == current
						row := ui.Button(c.Key("kit-opt:"+opt.ID), "").Label(opt.Label).Role(ui.RoleOption).AccessSelected(chosen).
							FillWidth().Padding(7, 8).Radius(8).Justify(ui.Start).
							Border(0, p.line).Background(ui.Transparent)
						if opt.Disabled {
							row.Disabled(true)
						}
						switch {
						case chosen:
							row.Background(p.selected)
						case row.Hovered():
							row.Background(p.soft)
						}
						if row.FocusVisible() {
							row.Shadow(0, 0, 0, 2, p.accent.Alpha(0.4))
						}
						row.Children(func() {
							ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
								ui.Text(c, opt.Label).FontSize(13).FontWeight(500).TextColor(p.ink).Wrap()
								if opt.Detail != "" && opt.Detail != opt.Label {
									ui.Text(c, opt.Detail).FontSize(11.5).TextColor(p.muted).Wrap()
								}
							})
							if chosen {
								ui.Icon(c, checkIcon).FontSize(14).TextColor(p.accentInk).Shrink(0)
							}
						})
						if row.Clicked() && !opt.Disabled {
							onPick(opt.ID)
							*open = false
						}
						shown++
					}
					if shown == 0 {
						ui.Text(c, "—").FontSize(12).TextColor(p.muted).Padding(8, 8)
					}
				})
			})
		})
	})
}

// styleSegment paints one piece of a segmented control.
func styleSegment(seg ui.Element, p palette, chosen bool) {
	bg := ui.Transparent
	text := p.muted
	if chosen {
		bg = p.raised
		if p.dark {
			bg = p.selected
		}
		text = p.ink
		seg.Shadow(0, 1, 2, 0, ui.Hex("#0000001A"))
	} else if seg.Hovered() {
		bg = p.rowHover
		text = p.ink
	}
	seg.Background(bg).TextColor(text)
	if seg.FocusVisible() {
		seg.Shadow(0, 0, 0, 2, p.accent.Alpha(0.4))
	}
}

// kitSegments is a single-choice pill. on receives the chosen id.
func kitSegments(c *ui.Context, p palette, key, label string, items []kitOption, current string, on func(id string)) {
	ui.Row(c.Key(key)).Height(28).Padding(2).Gap(2).Radius(10).Background(p.soft).Border(1, p.lineSoft).
		AlignItems(ui.Center).Shrink(0).Role(ui.RoleRadioGroup).Label(label).Children(func() {
		for _, item := range items {
			item := item
			chosen := item.ID == current
			seg := ui.Button(c.Key(key+":"+item.ID), "").Label(item.Label).Role(ui.RoleRadio).AccessSelected(chosen).
				Height(24).Padding(0, 10).Radius(8).Border(0, p.line).TextColor(p.muted)
			styleSegment(seg, p, chosen)
			seg.Children(func() { ui.Text(c, item.Label).FontSize(12).FontWeight(500).NoWrap() })
			if seg.Clicked() && !chosen {
				on(item.ID)
			}
		}
	})
}
