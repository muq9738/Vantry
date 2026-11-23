package desktop

import "github.com/egoist/mygo/ui"

// Native settings primitives. A settings page is a column
// of sections; each section is a titled, hairline-bordered group whose rows
// put a title and a muted description on the left and the control on the
// right, divided by a softer hairline. Plugin and remote-control pages use
// the same kit so every configuration surface reads alike.

// settingsHeading is a section title above a group, with an optional action
// at its right edge, using the section's shared typography and minimum height.
func settingsHeading(c *ui.Context, p palette, title, hint string, action func()) {
	ui.Row(c).Padding(0, 4).Gap(12).AlignItems(ui.End).MinHeight(28).Children(func() {
		ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, title).FontSize(14).FontWeight(600).TextColor(p.ink).SingleLine()
			if hint != "" {
				ui.Text(c, hint).FontSize(12).TextColor(p.muted).Wrap()
			}
		})
		if action != nil {
			action()
		}
	})
}

// settingsGroup is the bordered card; rows inside call settingsRow with
// first=true for the top row so only later rows draw a divider.
func settingsGroup(c *ui.Context, p palette, key string, rows func()) ui.Element {
	group := ui.Column(c.Key(key)).FillWidth().Radius(12).Border(1, p.line).Background(groupFill(p))
	group.Children(rows)
	return group
}

// groupFill lifts a group one step off the page it sits on: a lighter
// graphite on the dark #111 page, white on the light zinc-25 page.
func groupFill(p palette) ui.Color {
	if p.dark {
		return ui.Hex("#171717")
	}
	return ui.Hex("#FFFFFF")
}

// pageFill and navFill are the settings window's two panes.
func pageFill(p palette) ui.Color {
	if p.dark {
		return p.raised
	}
	return p.canvas
}

func navFill(p palette) ui.Color {
	if p.dark {
		return ui.Hex("#0B0B0B")
	}
	return ui.Hex("#F6F6F7")
}

// settingsRow lays out one preference. control may be nil for a plain
// informational row; it is placed at the trailing edge and never shrinks.
func settingsRow(c *ui.Context, p palette, key string, first bool, title, description string, control func()) ui.Element {
	row := ui.Row(c.Key(key)).FillWidth().MinHeight(56).Padding(12, 16).Gap(16).AlignItems(ui.Center)
	if !first {
		row.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
	}
	row.Children(func() {
		ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, title).FontSize(13).FontWeight(500).TextColor(p.ink).Wrap()
			if description != "" {
				ui.Text(c, description).FontSize(12).TextColor(p.muted).Wrap()
			}
		})
		if control != nil {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Shrink(0).Children(control)
		}
	})
	return row
}

// settingsStatus is a small state readout: a colored dot or icon and text,
// including the allowed state beside a granted permission.
func settingsStatus(c *ui.Context, icon *ui.SVG, text string, tone ui.Color) {
	ui.Row(c).Gap(5).AlignItems(ui.Center).Shrink(0).Children(func() {
		if icon != nil {
			ui.Icon(c, icon).FontSize(13).TextColor(tone)
		} else {
			ui.Box(c).Size(6, 6).Radius(3).Background(tone).Shrink(0)
		}
		ui.Text(c, text).FontSize(12).FontWeight(500).TextColor(tone).NoWrap()
	})
}

// outlineButton is the compact secondary action.
func outlineButton(c *ui.Context, p palette, key, label string) ui.Element {
	return kitButton(c, p, key, label, controlOutline, controlSM)
}

// primaryAction is the single emphasized action in a settings row.
func primaryAction(c *ui.Context, p palette, key, label string) ui.Element {
	return kitButton(c, p, key, label, controlPrimary, controlSM)
}

// navItem is one entry of a settings-style left navigation column.
func navItem(c *ui.Context, p palette, key, label string, icon *ui.SVG, active bool) ui.Element {
	item := ui.Button(c.Key(key), "").Label(label).FillWidth().Height(32).Padding(0, 10).Radius(8).
		Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
	if active {
		item.Background(p.selected).TextColor(p.ink)
	} else if item.Hovered() {
		item.Background(p.rowHover).TextColor(p.ink)
	}
	item.Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Grow(1).Children(func() {
			ui.Icon(c, icon).FontSize(15)
			weight := 400
			if active {
				weight = 500
			}
			ui.Text(c, label).FontSize(13).FontWeight(weight).SingleLine()
		})
	})
	return item
}
