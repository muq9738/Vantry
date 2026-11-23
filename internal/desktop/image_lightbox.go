package desktop

import (
	"fmt"

	"github.com/egoist/mygo/ui"
)

// imageLightbox shows a sent image at window size. Left/right arrows step
// through the other images of the same message; Esc or a click outside
// closes it.
func (w *workbench) imageLightbox(c *ui.Context, p palette) {
	items := w.visuals.lightbox
	if len(items) == 0 {
		return
	}
	idx := min(max(w.visuals.lightboxIndex, 0), len(items)-1)
	item := items[idx]
	open := true
	width, height := c.Size()
	step := func(d int) {
		w.visuals.lightboxIndex = (idx + d + len(items)) % len(items)
	}
	ui.DialogBase(c, &open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#000000B8"))
		panel.Padding(0).Background(ui.Transparent)
		if len(items) > 1 {
			if c.Shortcut(0, ui.KeyLeft) {
				step(-1)
			}
			if c.Shortcut(0, ui.KeyRight) {
				step(1)
			}
		}
		ui.Column(c).Gap(10).AlignItems(ui.Center).Children(func() {
			if bitmap := attachmentPreview(item); bitmap != nil {
				ui.Image(c, bitmap).Fit(ui.Contain).MaxWidth(width - 120).MaxHeight(height - 120).Radius(10)
			} else {
				w.loadAttachmentPreview(item)
				ui.Text(c, w.tr3("正在载入图片…", "Loading image…", "画像を読み込み中…")).FontSize(13).TextColor(ui.Hex("#FFFFFFB0"))
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Padding(4, 6, 4, 12).Radius(10).Background(ui.Hex("#1C1C1CE6")).Children(func() {
				label := item.Name
				if len(items) > 1 {
					label = fmt.Sprintf("%d / %d · %s", idx+1, len(items), item.Name)
				}
				ui.Text(c, label).FontSize(12).TextColor(ui.Hex("#FFFFFFD0")).SingleLine().Ellipsis("…").MaxWidth(min(520, width*0.5))
				if len(items) > 1 {
					if lightboxButton(c, "lightbox-prev", w.tr3("上一张", "Previous", "前へ"), backIcon).Clicked() {
						step(-1)
					}
					if lightboxButton(c, "lightbox-next", w.tr3("下一张", "Next", "次へ"), lucide("arrow-right")).Clicked() {
						step(1)
					}
				}
				if lightboxButton(c, "lightbox-close", w.tr3("关闭", "Close", "閉じる"), closeIcon).Clicked() {
					open = false
				}
			})
		})
	})
	if !open {
		w.visuals.lightbox = nil
	}
}

func lightboxButton(c *ui.Context, key, label string, icon *ui.SVG) ui.Element {
	btn := ui.Button(c.Key(key), "").Label(label).Tooltip(label).Size(26, 26).Padding(0).Radius(7).Border(0, ui.Transparent).Background(ui.Transparent).Justify(ui.Center)
	if btn.Hovered() {
		btn.Background(ui.Hex("#FFFFFF1F"))
	}
	btn.Children(func() { ui.Icon(c, icon).FontSize(14).TextColor(ui.Hex("#FFFFFFE0")) })
	return btn
}
