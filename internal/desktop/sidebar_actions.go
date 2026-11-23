package desktop

import (
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// threadDrag carries the machine that owns the thread. A drop can only act on
// a thread of its own Core - there is no drag that moves a thread between
// machines - so the owner rides inside the payload rather than the row ID.
type threadDrag struct {
	owner *workbench
	id    string
}

// Drops use the same durable thread command as the context menu, dispatched
// to the machine that owns the dragged thread. A drag preview never mutates
// the projection or changes the selected thread.
func (w *workbench) dropThread(item threadDrag, destination string) bool {
	owner, id := item.owner, item.id
	if owner == nil {
		owner = w
	}
	th, ok := owner.threadByID(id)
	if !ok {
		return false
	}
	cmd := api.Command{ThreadID: id}
	switch destination {
	case sectionPinned:
		if th.Pinned && !th.Archived {
			return false
		}
		pin, archive := true, false
		cmd.Pinned, cmd.Archived = &pin, &archive
	case sectionSettled:
		if th.Archived || owner.threadWorking(id) {
			return false
		}
		archive := true
		cmd.Archived = &archive
	case "active":
		if !th.Archived && !th.Pinned {
			return false
		}
		pin, archive := false, false
		cmd.Pinned, cmd.Archived = &pin, &archive
	default:
		return false
	}
	owner.updateThread(cmd)
	return true
}

func (w *workbench) threadDropTarget(c *ui.Context, p palette, box ui.Element, destination string) {
	if item, over := ui.DragOver[threadDrag](box); over {
		allowed := destination != sectionSettled || item.owner == nil || !item.owner.threadWorking(item.id)
		if allowed {
			box.Border(1, p.accent)
		} else {
			box.Border(1, p.warning)
		}
	}
	if item, dropped := ui.Drop[threadDrag](box); dropped {
		w.dropThread(item, destination)
	}
}

// A drag gets a temporary, clearly labelled destination panel. It overlays the
// sidebar rather than inserting permanent-looking lifecycle buttons or moving
// the settled shelf and utility footer underneath the pointer.
func (w *workbench) sidebarDropShelf(c *ui.Context, p palette, sidebar ui.Element, wasDragging bool) {
	if !w.widgets.draggingThread && !wasDragging {
		return
	}
	bounds := sidebar.Bounds()
	if bounds.W <= 0 || bounds.H <= 0 {
		return
	}
	ui.Overlay(c, func() {
		ui.Column(c.Key("thread-drop-destinations")).Label(w.tr("拖放会话", "Move thread")).
			Absolute().Left(bounds.X+8).Top(bounds.Y+bounds.H-202).Width(bounds.W-16).
			Padding(8).Gap(3).Radius(10).Background(p.raised).Border(1, p.line).
			Shadow(0, 4, 16, 0, ui.Hex("#00000030")).Children(func() {
			if !w.widgets.draggingThread {
				// Drop is delivered on the release frame, after Dragging is
				// already false. Keep the targets alive for that frame only.
				c.After(0)
			}
			ui.Text(c, w.tr("松开以移动会话 · Esc 取消", "Release to move · Esc to cancel")).FontSize(11).TextColor(p.muted).SingleLine().Padding(3, 4, 5)
			for _, destination := range []struct {
				key, label string
				icon       *ui.SVG
			}{{sectionPinned, w.tr("置顶", "Pin"), pinIcon}, {"active", w.tr("移回活跃", "Unsettle"), unsettleIcon}, {sectionSettled, w.tr("归档", "Settle"), settleIcon}} {
				box := ui.Row(c.Key(destination.key)).Label(w.tr("拖放至", "Drop to ")+destination.label).
					FillWidth().Height(30).Padding(0, 8).Gap(7).AlignItems(ui.Center).Radius(6)
				if _, over := ui.DragOver[threadDrag](box); over {
					box.Background(p.selected)
				}
				w.threadDropTarget(c, p, box, destination.key)
				box.Children(func() {
					ui.Icon(c, destination.icon).FontSize(12).TextColor(p.muted).Shrink(0)
					ui.Text(c, destination.label).FontSize(12).TextColor(p.ink).SingleLine()
				})
			}
		})
	})
}
