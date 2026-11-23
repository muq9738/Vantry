package desktop

import (
	"bytes"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestDeletingAttachmentReferenceKeepsTheAttachment(t *testing.T) {
	c := &composer{}
	item := api.Attachment{ID: "img-1", Name: "pasted-image-5.png", MIME: "image/png", Size: 1200}
	c.attachments = []api.Attachment{item}
	display := c.buildInlineAttachmentChip(item)
	c.addMention(composerMention{kind: mentionAttachment, display: display, expand: c.attachmentExpand(item), attachmentID: item.ID})
	c.draft = "看 " + display + " 这里"
	c.draft = strings.ReplaceAll(c.draft, display, "")
	c.pruneMentions()
	if len(c.mentions) != 0 {
		t.Fatalf("reference mention kept: %+v", c.mentions)
	}
	if len(c.attachments) != 1 || c.attachments[0].ID != item.ID {
		t.Fatalf("attachment removed with its reference: %+v", c.attachments)
	}
	if got := c.outgoing(); got != "看  这里" {
		t.Fatalf("outgoing = %q", got)
	}
}

func TestRemoveAttachmentAlsoRemovesItsReference(t *testing.T) {
	w := newWorkbench(&fakeService{})
	id := w.newSession()
	comp := w.composerFor(id)
	item := api.Attachment{ID: "img-1", Name: "shot.png", MIME: "image/png"}
	comp.attachments = []api.Attachment{item}
	display := comp.buildInlineAttachmentChip(item)
	comp.addMention(composerMention{kind: mentionAttachment, display: display, expand: comp.attachmentExpand(item), attachmentID: item.ID})
	comp.draft = "前" + display + "后"
	w.removeComposerAttachment(id, item.ID)
	if len(comp.attachments) != 0 {
		t.Fatalf("attachment kept: %+v", comp.attachments)
	}
	if comp.draft != "前后" {
		t.Fatalf("draft = %q", comp.draft)
	}
	if len(comp.mentions) != 0 {
		t.Fatalf("mention kept: %+v", comp.mentions)
	}
}

func TestUndoPasteRestoresDraftAndDropsOnlyThatPaste(t *testing.T) {
	c := &composer{draft: "之前"}
	c.attachments = []api.Attachment{{ID: "kept", Name: "notes.txt"}}
	c.lastPaste = &composerPasteUndo{
		before: "之前",
		after:  "之前 @pasted-image-1.png",
		ids:    []string{"new"},
		caret:  len([]rune("之前")),
	}
	c.draft = c.lastPaste.after
	c.attachments = append(c.attachments, api.Attachment{ID: "new", Name: "pasted-image-1.png", MIME: "image/png"})
	c.addMention(composerMention{kind: mentionAttachment, display: "@pasted-image-1.png", expand: "@pasted-image-1.png", attachmentID: "new"})
	caret, ok := c.undoLastPaste(c.draft)
	if !ok || c.draft != "之前" || caret != len([]rune("之前")) {
		t.Fatalf("undo = %q caret %d ok %v", c.draft, caret, ok)
	}
	if len(c.attachments) != 1 || c.attachments[0].ID != "kept" {
		t.Fatalf("attachments = %+v", c.attachments)
	}
	if len(c.mentions) != 0 {
		t.Fatalf("mentions = %+v", c.mentions)
	}
	if _, again := c.undoLastPaste(c.draft); again {
		t.Fatal("second undo should belong to the editor")
	}
}

func TestUndoPasteIgnoredAfterAnotherEdit(t *testing.T) {
	c := &composer{}
	c.lastPaste = &composerPasteUndo{before: "", after: "@a.png", ids: []string{"a"}}
	c.draft = "@a.png 又写了"
	c.attachments = []api.Attachment{{ID: "a", Name: "a.png", MIME: "image/png"}}
	if _, ok := c.undoLastPaste(c.draft); ok {
		t.Fatal("edited draft was undone")
	}
	if len(c.attachments) != 1 {
		t.Fatalf("attachment changed: %+v", c.attachments)
	}
	if c.lastPaste != nil {
		t.Fatal("stale paste undo kept")
	}
}

func TestMiddleTruncateKeepsExtension(t *testing.T) {
	got := middleTruncateAttachmentName("very-long-screenshot-from-the-design-review-final.png", 36)
	if len([]rune(got)) != 36 || !strings.HasPrefix(got, "very-long") || !strings.HasSuffix(got, "final.png") || !strings.Contains(got, "…") {
		t.Fatalf("truncated = %q (%d)", got, len([]rune(got)))
	}
	if middleTruncateAttachmentName("short.png", 36) != "short.png" {
		t.Fatal("short name changed")
	}
}

func TestVisibleUserMessageStripsLegacyChips(t *testing.T) {
	input := "先检查 [🖼 screenshot.png · 48.2 KB]，然后看 [📄 spec.pdf]。"
	got := visibleUserMessage(input, []api.Attachment{{Name: "screenshot.png", MIME: "image/png"}, {Name: "spec.pdf", MIME: "application/pdf"}})
	want := "先检查 @screenshot.png，然后看 @spec.pdf。"
	if got != want {
		t.Fatalf("visible = %q", got)
	}
}

func TestCopiedParagraphsSeparateWithBlankLines(t *testing.T) {
	got := copiedParagraphs([]ui.SelectedText{
		{Text: "甲\uFFFC乙", Start: 0, End: 3},
		{Text: "丙\u2060丁", Start: 0, End: 3},
	})
	if got != "甲乙\n\n丙丁" {
		t.Fatalf("copied = %q", got)
	}
}

func TestUndoPasteChordRestoresDraftAndDropsAttachment(t *testing.T) {
	pngBytes := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte("a"), 1024)...)
	var nextPaste *clipboardPastePayload
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == "attachment.import" {
			return api.WorkbenchResponse{Attachment: &api.Attachment{
				ID: "file_ui_undo", Name: req.Name, MIME: "image/png", Size: int64(len(req.Data)),
			}}, nil
		}
		return api.WorkbenchResponse{}, nil
	}}
	w := newWorkbench(service)
	w.clipboardProbe = func(pasted string) (clipboardPastePayload, bool) {
		if nextPaste != nil {
			p := *nextPaste
			nextPaste = nil
			return p, true
		}
		return clipboardPastePayload{}, false
	}
	id := w.newSession()
	tt := ui.NewTester(w.view, 1080, 760)
	mustClick(t, tt, "消息输入框")
	tt.Type("之前 ")
	nextPaste = &clipboardPastePayload{
		Items: []clipboardPasteItem{{Name: "pasted-image.png", MIME: "image/png", Data: pngBytes}},
	}
	tt.SetClipboard("")
	tt.Command("paste")
	tt.Frame()
	comp := w.composerFor(id)
	if comp.draft != "之前 @pasted-image-1.png" || len(comp.attachments) != 1 {
		t.Fatalf("after paste: draft=%q attachments=%+v", comp.draft, comp.attachments)
	}
	// ⌘Z right after the paste undoes the whole paste: draft and attachment.
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	if comp.draft != "之前 " {
		t.Fatalf("undo draft = %q", comp.draft)
	}
	if len(comp.attachments) != 0 {
		t.Fatalf("undo kept attachments: %+v", comp.attachments)
	}
	if len(comp.mentions) != 0 {
		t.Fatalf("undo kept mentions: %+v", comp.mentions)
	}

	// Paste again, then edit: ⌘Z now belongs to the editor's own undo and
	// must leave the attachment alone.
	nextPaste = &clipboardPastePayload{
		Items: []clipboardPasteItem{{Name: "pasted-image.png", MIME: "image/png", Data: pngBytes}},
	}
	tt.SetClipboard("")
	tt.Command("paste")
	tt.Frame()
	tt.Type(" 之后")
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	if len(comp.attachments) != 1 || comp.attachments[0].ID != "file_ui_undo" {
		t.Fatalf("native undo touched the attachment: %+v", comp.attachments)
	}
}

func TestMinimapCurrentAndGutter(t *testing.T) {
	turns := []int{0, 2, 5}
	if got := minimapCurrent(turns, 3, 4); got != 1 {
		t.Fatalf("reading the reply of turn 1, got %d", got)
	}
	if got := minimapCurrent(turns, 5, 5); got != 2 {
		t.Fatalf("on the third turn, got %d", got)
	}
	if got := minimapCurrent(turns, 0, -1); got != 2 {
		t.Fatalf("unlaid list should follow the end, got %d", got)
	}
	if timelineMinimapGutter(832, 28) < timelineMinimapMinGutter {
		t.Fatal("1080-wide chat should show the minimap")
	}
	if timelineMinimapGutter(640, 16) >= timelineMinimapMinGutter {
		t.Fatal("narrow chat should hide the minimap")
	}
	wideGutter := timelineMinimapGutter(1040, 28) // 140px
	wideLeft := timelineMinimapLeft(wideGutter)
	if wideLeft < 10 || wideLeft > 24 {
		t.Fatalf("wide minimap should anchor near left margin (10..24px), got %.1f for gutter %.1f", wideLeft, wideGutter)
	}
	if wideGutter-(wideLeft+26) < 80 {
		t.Fatalf("wide minimap still hugs the reading column: left=%.1f gutter=%.1f", wideLeft, wideGutter)
	}
}
