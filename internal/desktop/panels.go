package desktop

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func (w *workbench) sessionPanel(c *ui.Context, p palette) {
	thread, ok := w.selectedThread()
	if !ok {
		ui.Text(c, w.tr("首次发送后即可管理会话。", "Thread options are available after the first message.")).TextColor(p.muted)
		return
	}
	ui.Checkbox(c, &w.widgets.manualDelivery, w.tr("手动确认子任务结果", "Confirm subtask results manually")).Description(w.tr("关闭时子任务完成后自动续行父任务", "When off, the parent continues automatically after subtasks finish"))
	ui.Row(c).Gap(8).Children(func() {
		pinLabel := w.tr("置顶", "Pin")
		if thread.Pinned {
			pinLabel = w.tr("取消置顶", "Unpin")
		}
		if ui.Button(c, pinLabel).Clicked() {
			value := !thread.Pinned
			w.updateThread(api.Command{Pinned: &value})
		}
		archiveLabel := w.tr("归档会话", "Settle thread")
		if thread.Archived {
			archiveLabel = w.tr("移回活跃", "Unsettle")
		}
		if ui.Button(c, archiveLabel).Clicked() {
			value := !thread.Archived
			w.updateThread(api.Command{Archived: &value})
		}
		if ui.Button(c, w.tr("重命名", "Rename")).Clicked() {
			w.features.rename = thread.Title
			w.features.showRename = true
		}
		fork := ui.Button(c, w.tr("分叉会话", "Fork")).Label(w.tr("从此处分叉会话", "Fork from here")).Disabled(w.sessions.forking[thread.ID+":"])
		fork.Description(w.tr("复制当前历史为新会话，原会话不受影响", "Copy this history into a new thread; this one is unchanged"))
		if fork.Clicked() {
			w.forkThread(thread.ID, "")
		}
		ui.Spacer(c)
		remove := ui.Button(c, w.tr("删除会话", "Delete thread")).TextColor(p.danger).Disabled(w.sessions.deleting[thread.ID])
		remove.Description(w.tr("立即删除会话及其历史，无需二次确认", "Deletes the thread and its history at once, without a second confirmation"))
		if remove.Clicked() {
			w.requestDelete(thread.ID)
		}
	})
	if w.features.showRename {
		ui.Row(c).Gap(8).Children(func() {
			ui.TextInput(c.Key("rename:"+thread.ID), &w.features.rename).Label(w.tr("会话标题", "Thread title")).AutoFocus().Grow(1)
			if ui.Button(c, w.tr("保存标题", "Save title")).Disabled(strings.TrimSpace(w.features.rename) == "").Clicked() {
				w.updateThread(api.Command{Title: w.features.rename})
				w.features.showRename = false
			}
			if ui.Button(c, w.tr("取消重命名", "Cancel rename")).Clicked() {
				w.features.showRename = false
			}
		})
	}
	if thread.Workspace != nil {
		path := thread.Workspace.Path
		if path == "" {
			path = thread.Workspace.Repository
		}
		ui.Text(c, path).FontSize(11).TextColor(p.muted).Wrap().Selectable()
	}
	ui.Text(c, w.tr("归档会保留历史；有运行中任务或排队消息时请先处理。", "Settling keeps the history; handle running tasks or queued messages first.")).FontSize(11).TextColor(p.muted).Wrap()
}

func (w *workbench) queuedView(c *ui.Context, p palette) {
	pending := []api.QueuedMessage{}
	for _, message := range w.messages {
		if message.ThreadID != w.selected || message.Status != "pending" {
			continue
		}
		projected := false
		for _, input := range w.inputs {
			if input.ID == message.ID {
				projected = true
				break
			}
		}
		if !projected {
			pending = append(pending, message)
		}
	}
	if len(pending) == 0 {
		return
	}
	_, height := c.Size()
	limit := float32(76)
	if height < 560 {
		limit = 38
	}
	ui.Scroll(c.Key("queued-messages:"+w.selected)).FillWidth().MaxHeight(limit).Padding(0, 4, 6).Shrink(0).Children(func() {
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			for _, message := range pending {
				message := message
				ui.Row(c.Key(message.ID)).FillWidth().Height(32).Padding(0, 6, 0, 10).Gap(8).Radius(10).
					Background(p.raised).Border(1, p.lineSoft).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, queueIcon).FontSize(12).TextColor(p.faint).Shrink(0)
					ui.Text(c, w.tr("待发送", "Queued")).FontSize(11).FontWeight(500).TextColor(p.faint).NoWrap().Shrink(0)
					ui.Text(c, titleFromInstruction(message.Message)).FontSize(12).TextColor(p.ink).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
					withdraw := quietButton(c, p, "withdraw:"+message.ID, w.tr("撤回", "Withdraw")).
						Label(w.tr("撤回 ", "Withdraw ") + message.ID).Height(22).Padding(0, 8).Radius(6).FontSize(11)
					if withdraw.Clicked() {
						w.taskAction("cancel-message:"+message.ID, api.Command{Kind: api.CancelMessage, ThreadID: message.ThreadID, MessageID: message.ID})
					}
				})
			}
		})
	})
}

func (w *workbench) attachmentView(c *ui.Context, p palette, comp *composer) {
	if comp.uploading && len(comp.attachments) == 0 {
		ui.Text(c, w.tr("正在添加附件…", "Adding attachments…")).FontSize(11).TextColor(p.muted).Padding(4, 10, 0)
	}
	if len(comp.attachments) == 0 && !comp.uploading {
		return
	}
	// T3 Code's composer strip: rounded image tiles, a quiet file chip, and a
	// remove control that appears on hover. Clicking an image opens it large.
	target := w.selected
	ui.Row(c.Key("draft-attachments")).FillWidth().Wrap().Gap(8).Padding(8, 10, 0).Shrink(0).Children(func() {
		if comp.uploading {
			pending := ui.Column(c.Key("tile:uploading")).Size(64, 64).Radius(12).Border(1, p.line).Background(p.soft).Shrink(0).Justify(ui.Center).AlignItems(ui.Center)
			pending.Description(w.tr3("正在上传", "Uploading", "アップロード中"))
			pending.Children(func() {
				ui.Text(c, w.tr3("上传中", "Uploading", "送信中")).FontSize(10).TextColor(p.muted).TextAlign(ui.Center)
			})
		}
		for _, item := range append([]api.Attachment(nil), comp.attachments...) {
			item := item
			if classifyAttachment(item) == attachImage {
				w.imageAttachmentTile(c, p, comp, target, item)
				continue
			}
			icon := fileIcon
			if classifyAttachment(item) == attachPDF {
				icon = pdfAttachmentIcon
			}
			chip := ui.Row(c.Key("chip:"+item.ID)).Height(32).MaxWidth(220).Gap(6).Padding(0, 4, 0, 8).AlignItems(ui.Center).Radius(10).Background(p.soft).Border(1, p.line).Shrink(0)
			chip.Description(item.Name + " · " + formatByteSize(item.Size))
			hovered := chip.Hovered()
			chip.Children(func() {
				ui.Icon(c, icon).FontSize(13).TextColor(p.muted).Shrink(0)
				ui.Text(c, item.Name).FontSize(12).TextColor(p.ink).MinWidth(0).SingleLine().Ellipsis("…")
				remove := w.attachmentRemove(c, p, target, item)
				if !hovered && !remove.Hovered() {
					remove.Opacity(0)
				}
			})
		}
	})
	w.attachmentPreviewDialog(c, p, comp)
}

func (w *workbench) imageAttachmentTile(c *ui.Context, p palette, comp *composer, target string, item api.Attachment) {
	ui.Box(c.Key("frame:"+item.ID)).Size(64, 64).Shrink(0).Children(func() {
		tile := ui.Button(c.Key("tile:"+item.ID), "").Label(w.tr3("查看图片 ", "View image ", "画像を表示 ")+item.Name).Size(64, 64).Padding(0).Radius(12).Clip().Border(1, p.line).Background(p.soft)
		tile.Description(item.Name + " · " + formatByteSize(item.Size))
		hovered := tile.Hovered()
		if bitmap := attachmentPreview(item); bitmap != nil {
			tile.Children(func() {
				ui.Image(c, bitmap).Size(64, 64).Fit(ui.Cover)
			})
		} else {
			w.loadAttachmentPreview(item)
			tile.Children(func() {
				ui.Icon(c, imageAttachmentIcon).FontSize(16).TextColor(p.faint)
			})
		}
		remove := w.attachmentRemove(c, p, target, item).Absolute().Top(4).Right(4).Background(p.raised.Alpha(0.92))
		if !hovered && !remove.Hovered() && !remove.FocusVisible() {
			remove.Opacity(0)
		}
		if tile.Clicked() {
			comp.previewAttachment = item.ID
		}
	})
}

func (w *workbench) attachmentPreviewDialog(c *ui.Context, p palette, comp *composer) {
	if comp.previewAttachment == "" {
		return
	}
	var item api.Attachment
	found := false
	for _, current := range comp.attachments {
		if current.ID == comp.previewAttachment {
			item, found = current, true
			break
		}
	}
	if !found {
		comp.previewAttachment = ""
		return
	}
	open := true
	width, height := c.Size()
	ui.DialogBase(c, &open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000088"))
		panel.Padding(10).Radius(16).Background(p.raised).Border(1, p.line).Shadow(0, 18, 50, 0, ui.Hex("#00000066"))
		ui.Column(c).Gap(8).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, item.Name).FontSize(12).FontWeight(600).SingleLine().Ellipsis("…").MaxWidth(width * 0.5).Grow(1)
				if iconButton(c, p, "close-attachment-preview", w.tr("关闭", "Close"), closeIcon).Clicked() {
					open = false
				}
			})
			if bitmap := attachmentPreview(item); bitmap != nil {
				ui.Image(c, bitmap).Fit(ui.Contain).MaxWidth(width * 0.72).MaxHeight(height * 0.62).Radius(10)
			} else {
				w.loadAttachmentPreview(item)
				ui.Text(c, w.tr3("正在载入图片…", "Loading image…", "画像を読み込み中…")).FontSize(12).TextColor(p.muted)
			}
		})
	})
	if !open {
		comp.previewAttachment = ""
	}
}

func (w *workbench) attachmentRemove(c *ui.Context, p palette, target string, item api.Attachment) ui.Element {
	remove := iconButton(c, p, "remove:"+item.ID, w.tr("移除附件 ", "Remove attachment ")+item.Name, closeIcon).Size(18, 18).Padding(3).Radius(9)
	if remove.Clicked() {
		w.removeComposerAttachment(target, item.ID)
		forgetAttachmentPreview(item.ID)
	}
	return remove
}
