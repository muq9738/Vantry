package desktop

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// Editing a queued message uses the main composer, as T3 Code does. Keep the
// user's unsent draft separately and restore it on either save or cancel.
type queueComposerEdit struct {
	input       api.Input
	draft       string
	attachments []api.Attachment
	mentions    []composerMention
	plan        bool
}

func (w *workbench) beginQueueComposerEdit(input api.Input) {
	if !editableInput(input) {
		return
	}
	w.cancelQueueComposerEdit()
	comp := w.composerFor(input.ThreadID)
	w.queue.composer = &queueComposerEdit{input: input, draft: comp.draft,
		attachments: append([]api.Attachment(nil), comp.attachments...), mentions: append([]composerMention(nil), comp.mentions...), plan: comp.plan}
	w.queue.begin(input)
	comp.draft, comp.attachments, comp.mentions = strings.TrimPrefix(input.Message, planModePrefix), append([]api.Attachment(nil), input.Attachments...), nil
	comp.plan = strings.HasPrefix(input.Message, planModePrefix)
	for _, item := range input.Attachments {
		ref := comp.buildInlineAttachmentChip(item)
		if strings.Contains(comp.draft, ref) {
			comp.addMention(composerMention{kind: mentionAttachment, display: ref, expand: ref, attachmentID: item.ID})
		}
	}
	w.widgets.composerFocus.Focus()
}

func (w *workbench) editingQueue(target string) bool {
	return w.queue.composer != nil && w.queue.composer.input.ThreadID == target
}

func (w *workbench) cancelQueueComposerEdit() {
	edit := w.queue.composer
	if edit == nil {
		return
	}
	comp := w.composerFor(edit.input.ThreadID)
	comp.draft, comp.attachments, comp.mentions = edit.draft, edit.attachments, edit.mentions
	comp.plan = edit.plan
	w.queue.end(edit.input.ID)
	w.queue.composer = nil
	w.widgets.composerFocus.Focus()
	w.saveState()
}

func (w *workbench) saveQueueComposerEdit(target string) bool {
	if !w.editingQueue(target) {
		return false
	}
	edit := w.queue.composer
	comp := w.composerFor(target)
	if comp.uploading {
		return false
	}
	message := strings.TrimSpace(comp.outgoing())
	if message == "" && len(comp.attachments) > 0 {
		message = "请查看附件。"
	}
	if message == "" {
		return false
	}
	key := "edit:" + edit.input.ID
	delete(w.controls, key)
	if !w.dispatchControl(key, "更新排队消息", "排队消息已更新。", api.Command{
		Kind: api.EditInput, ThreadID: target, InputID: edit.input.ID, Message: applyInteractionMode(comp.plan, message),
		Attachments: append([]api.Attachment(nil), comp.attachments...), Value: "replace-attachments",
	}, false) {
		return false
	}
	w.cancelQueueComposerEdit()
	return true
}

func (w *workbench) queueComposerActions(c *ui.Context, p palette, comp *composer, target string) {
	if quietButton(c, p, "cancel-queue-edit", w.tr("取消编辑", "Cancel editing")).Height(28).Clicked() {
		w.cancelQueueComposerEdit()
	}
	save := ui.PrimaryButton(c.Key("save-queue-edit"), w.tr("更新排队消息", "Update queued message")).Label(w.tr("保存排队消息", "Save queued message")).Height(28).Padding(0, 10).Radius(14).FontSize(12)
	save.Disabled(comp.uploading || strings.TrimSpace(comp.draft) == "" && len(comp.attachments) == 0)
	if save.Clicked() {
		w.saveQueueComposerEdit(target)
	}
}
