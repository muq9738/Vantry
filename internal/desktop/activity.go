package desktop

import (
	"github.com/egoist/mygo/ui"
	"time"
)

// workbench activity dot. One paint primitive replaces a twelve-path spinner;
// only paint is scheduled, so a live status does not rebuild the workbench.
func activityIndicator(c *ui.Context, color ui.Color) ui.Element {
	return ui.Box(c).Size(12, 12).Shrink(0).Role(ui.RoleStatus).Label("正在处理").Draw(func(p *ui.Painter, r ui.Rect) {
		alpha := float32(1)
		if !p.Preferences().ReduceMotion {
			step := p.Now().UnixMilli() % 1200
			if step > 600 {
				step = 1200 - step
			}
			alpha = 0.5 + float32(step)/1200
			p.After(100 * time.Millisecond)
		}
		p.Fill(ui.Rect{X: r.X + (r.W-6)/2, Y: r.Y + (r.H-6)/2, W: 6, H: 6}, color.Alpha(alpha), 3)
	})
}
