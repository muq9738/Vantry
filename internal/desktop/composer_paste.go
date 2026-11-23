package desktop

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/adapters/attachments"
	"github.com/muq9738/Vantry/internal/hostplatform"
	"github.com/muq9738/Vantry/pkg/api"
)

type clipboardPasteItem struct {
	Name string
	MIME string
	Data []byte
}

type clipboardPastePayload struct {
	Files        []string
	Items        []clipboardPasteItem
	UseHostMedia bool
	ReportEmpty  bool
}

const (
	formatPDF      transfer.Format = "application/pdf"
	formatAdobePDF transfer.Format = "com.adobe.pdf"
	formatJPEG     transfer.Format = "image/jpeg"
	formatWebP     transfer.Format = "image/webp"
	formatGIF      transfer.Format = "image/gif"
)

// probeClipboardPaste checks whether the current paste gesture carries files,
// images, or PDFs rather than ordinary text. It runs synchronously on the UI
// thread so OnPaste can immediately return true (suppressing raw filename text)
// or false (letting TextAreaBase insert plain text with native undo).
//
// When ignorePlainText is false (normal ⌘V), non-empty plain text on the
// clipboard without copied Finder files always pastes as text so rich apps
// (Numbers, Excel, WPS, Keynote, Pages, Preview) that place both plain text
// and PDF/TIFF previews on NSPasteboard never swallow text pastes.
func (w *workbench) probeClipboardPaste(pasted string) (clipboardPastePayload, bool) {
	if w.clipboardProbe != nil {
		return w.clipboardProbe(pasted)
	}
	return probeSystemClipboard(pasted, false)
}

func probeSystemClipboard(pasted string, ignorePlainText bool) (clipboardPastePayload, bool) {
	var payload clipboardPastePayload
	var matched bool
	func() {
		defer func() {
			// In headless ui.NewTester runs, mygo.Clipboard panics with needsApp.
			_ = recover()
		}()
		if files, err := mygo.Clipboard.ReadFiles(); err == nil && len(files) > 0 {
			var regular []string
			for _, f := range files {
				if info, statErr := os.Stat(f); statErr == nil && info.Mode().IsRegular() {
					regular = append(regular, f)
				}
			}
			if len(regular) > 0 {
				payload = clipboardPastePayload{Files: regular}
				matched = true
				return
			}
		}
		if !ignorePlainText && pasted != "" {
			return
		}
		formats := mygo.Clipboard.Formats()
		if len(formats) == 0 {
			return
		}
		for _, pdfFmt := range []transfer.Format{formatPDF, formatAdobePDF} {
			if slices.Contains(formats, pdfFmt) {
				if data, err := mygo.Clipboard.ReadFormat(pdfFmt); err == nil && len(data) > 0 && bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) {
					payload = clipboardPastePayload{
						Items: []clipboardPasteItem{{Name: "pasted-document.pdf", MIME: "application/pdf", Data: data}},
					}
					matched = true
					return
				}
			}
		}
		for _, img := range []struct {
			format transfer.Format
			ext    string
			mime   string
		}{
			{transfer.PNG, ".png", "image/png"},
			{formatJPEG, ".jpg", "image/jpeg"},
			{formatWebP, ".webp", "image/webp"},
			{formatGIF, ".gif", "image/gif"},
		} {
			if slices.Contains(formats, img.format) {
				if data, err := mygo.Clipboard.ReadFormat(img.format); err == nil && len(data) > 0 {
					payload = clipboardPastePayload{
						Items: []clipboardPasteItem{{Name: "pasted-image" + img.ext, MIME: img.mime, Data: data}},
					}
					matched = true
					return
				}
			}
		}
		hasNonTextFormat := false
		for _, f := range formats {
			if f != transfer.Text && f != transfer.HTML && f != transfer.URIList {
				hasNonTextFormat = true
				break
			}
		}
		if hasNonTextFormat {
			// Native pasteboard carries non-text media (e.g. Apple PDF pasteboard type):
			// coerce via hostplatform in the background.
			payload = clipboardPastePayload{UseHostMedia: true}
			matched = true
		}
	}()
	return payload, matched
}

func (w *workbench) handleComposerPaste(target string, pasted string) bool {
	if target == "" {
		return false
	}
	payload, ok := w.probeClipboardPaste(pasted)
	if !ok {
		return false
	}
	comp := w.composerFor(target)
	w.pastePayloadAt(target, payload, comp.liveAnchor, comp.liveCaret)
	return true
}

func (w *workbench) pasteFromClipboardMenu(target string) {
	if target == "" {
		return
	}
	comp := w.composerFor(target)
	anchor, caret := comp.liveAnchor, comp.liveCaret
	if w.clipboardProbe != nil {
		if payload, ok := w.clipboardProbe(""); ok {
			payload.ReportEmpty = true
			w.pastePayloadAt(target, payload, anchor, caret)
			return
		}
	} else if payload, ok := probeSystemClipboard("", true); ok {
		payload.ReportEmpty = true
		w.pastePayloadAt(target, payload, anchor, caret)
		return
	}
	w.pastePayloadAt(target, clipboardPastePayload{UseHostMedia: true, ReportEmpty: true}, anchor, caret)
}

func (w *workbench) pastePayloadAt(target string, payload clipboardPastePayload, anchor, caret int) {
	comp := w.composerFor(target)
	if target == "" || comp.uploading {
		return
	}
	service, ok := w.service.(api.WorkbenchService)
	if !ok {
		comp.feedback = notice{Text: w.tr("当前服务不支持添加附件，请更新本地服务。", "The current service does not support attachments."), Level: noticeError}
		return
	}
	planned := len(payload.Files) + len(payload.Items)
	if payload.UseHostMedia && planned == 0 {
		planned = 1
	}
	if planned+len(comp.attachments) > 8 {
		comp.feedback = notice{Text: w.tr("每条消息最多添加 8 个附件。", "Up to 8 attachments per message."), Level: noticeWarn}
		return
	}

	// Assign sequential names on the UI thread for unnamed clipboard images/PDFs.
	items := make([]clipboardPasteItem, len(payload.Items))
	for i, it := range payload.Items {
		comp.pasteSeq++
		name := it.Name
		switch {
		case strings.HasPrefix(it.MIME, "application/pdf") || strings.EqualFold(filepath.Ext(name), ".pdf"):
			if name == "" || name == "pasted-document.pdf" || name == "clipboard.pdf" {
				name = fmt.Sprintf("pasted-document-%d.pdf", comp.pasteSeq)
			}
		case strings.HasPrefix(it.MIME, "image/"):
			ext := filepath.Ext(name)
			if ext == "" {
				ext = ".png"
			}
			if name == "" || strings.HasPrefix(name, "pasted-image") || name == "clipboard.png" {
				name = fmt.Sprintf("pasted-image-%d%s", comp.pasteSeq, ext)
			}
		}
		items[i] = clipboardPasteItem{Name: name, MIME: it.MIME, Data: it.Data}
	}
	nextSeq := comp.pasteSeq + 1

	comp.uploading = true
	w.start(func() {
		var imported []api.Attachment
		var failure string
		usedHostSeq := false

		if payload.UseHostMedia && len(payload.Files) == 0 && len(items) == 0 {
			ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
			readMedia := hostplatform.ReadClipboardMedia
			if w.hostClipboardMedia != nil {
				readMedia = w.hostClipboardMedia
			}
			data, mimeType, fileName, found, err := readMedia(ctx)
			cancel()
			if err != nil {
				failure = describe(w.tr("读取剪贴板失败", "Failed to read clipboard"), err)
			} else if found && len(data) > 0 {
				usedHostSeq = true
				name := fileName
				if mimeType == "application/pdf" {
					name = fmt.Sprintf("pasted-document-%d.pdf", nextSeq)
				} else {
					name = fmt.Sprintf("pasted-image-%d.png", nextSeq)
				}
				items = append(items, clipboardPasteItem{Name: name, MIME: mimeType, Data: data})
			}
		}

		for _, path := range payload.Files {
			if failure != "" {
				break
			}
			att, errMsg := w.readAndUploadLocalFile(service, path, false)
			if errMsg != "" {
				failure = errMsg
				break
			}
			imported = append(imported, att)
		}

		for _, item := range items {
			if failure != "" {
				break
			}
			if len(item.Data) == 0 || len(item.Data) > attachments.MaxSize {
				failure = item.Name + w.tr(" 超出 8 MiB 附件大小限制。", " exceeds the 8 MiB attachment limit.")
				break
			}
			att, errMsg := w.uploadAttachmentBytes(service, item.Name, item.Data)
			if errMsg != "" {
				failure = errMsg
				break
			}
			imported = append(imported, att)
		}

		w.post(func() {
			comp.uploading = false
			if usedHostSeq {
				comp.pasteSeq = nextSeq
			}
			if len(imported) > 0 {
				w.insertPastedAttachmentsAt(target, imported, anchor, caret)
			}
			if failure != "" {
				comp.feedback = notice{Text: failure, Level: noticeError}
			} else if len(imported) > 0 {
				comp.feedback = notice{}
			} else if payload.ReportEmpty {
				comp.feedback = notice{
					Text:  w.tr3("剪贴板中没有可粘贴的图片或 PDF。", "No image or PDF found on the clipboard.", "クリップボードに画像や PDF がありません。"),
					Level: noticeWarn,
				}
			}
		})
	})
}

func (w *workbench) readAndUploadLocalFile(service api.WorkbenchService, path string, allowEmpty bool) (api.Attachment, string) {
	base := filepath.Base(path)
	file, err := os.Open(path)
	if err != nil {
		return api.Attachment{}, w.tr("无法打开 ", "Cannot open ") + base
	}
	info, err := file.Stat()
	minSize := int64(1)
	if allowEmpty {
		minSize = 0
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < minSize || info.Size() > attachments.MaxSize {
		file.Close()
		return api.Attachment{}, base + w.tr(" 不是 8 MiB 以内的普通文件。", " must be a regular file up to 8 MiB.")
	}
	data, err := io.ReadAll(io.LimitReader(file, attachments.MaxSize+1))
	file.Close()
	if err != nil || (!allowEmpty && len(data) == 0) || len(data) > attachments.MaxSize {
		return api.Attachment{}, w.tr("读取 ", "Failed to read ") + base + w.tr(" 失败，或文件超过 8 MiB。", " or file exceeds 8 MiB.")
	}
	return w.uploadAttachmentBytes(service, base, data)
}

func (w *workbench) uploadAttachmentBytes(service api.WorkbenchService, name string, data []byte) (api.Attachment, string) {
	ctx, cancel := context.WithTimeout(w.ctx, 35*time.Second)
	defer cancel()
	response, err := service.Workbench(ctx, api.WorkbenchRequest{Action: "attachment.import", Name: name, Data: data})
	if err != nil {
		return api.Attachment{}, describe(w.tr("上传 ", "Failed to import ")+name, err)
	}
	if response.Attachment == nil || response.Attachment.ID == "" {
		return api.Attachment{}, w.tr("本地服务未确认附件 ", "Service did not confirm attachment ") + name
	}
	att := *response.Attachment
	if att.Size == 0 {
		att.Size = int64(len(data))
	}
	if att.Name == "" {
		att.Name = name
	}
	rememberAttachmentPreview(att, data)
	return att, ""
}

func (w *workbench) insertPastedAttachmentsAt(target string, imported []api.Attachment, anchor, caret int) {
	// Keep both T3-style thumbnail cards and an inline reference at the
	// insertion point. The same attachment ID binds the two representations.
	comp := w.composerFor(target)
	before := comp.draft
	comp.attachments = append(comp.attachments, imported...)
	var references []string
	var ids []string
	for _, item := range imported {
		reference := comp.buildInlineAttachmentChip(item)
		comp.addMention(composerMention{kind: mentionAttachment, display: reference, expand: comp.attachmentExpand(item), attachmentID: item.ID})
		references = append(references, reference)
		ids = append(ids, item.ID)
	}
	inserted := strings.Join(references, " ")
	runes := []rune(comp.draft)
	a := min(max(0, min(anchor, caret)), len(runes))
	z := min(max(0, max(anchor, caret)), len(runes))
	comp.draft = string(runes[:a]) + inserted + string(runes[z:])
	comp.caretAfter = a + utf8.RuneCountInString(inserted)
	comp.liveAnchor, comp.liveCaret = comp.caretAfter, comp.caretAfter
	comp.caretRestore = true
	comp.lastPaste = &composerPasteUndo{before: before, after: comp.draft, ids: ids, anchor: anchor, caret: caret}
	w.widgets.composerFocus.Focus()
	w.saveState()
}

// composerPasteUndo is one attachment paste, kept until the draft changes
// for any other reason. ⌘Z while it is current restores the previous draft
// and drops only the attachments that paste added.
type composerPasteUndo struct {
	before, after string
	ids           []string
	anchor, caret int
}

func (c *composer) dropPastedAttachments(ids []string) {
	if len(ids) == 0 {
		return
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	rem := c.attachments[:0]
	for _, item := range c.attachments {
		if !drop[item.ID] {
			rem = append(rem, item)
		}
	}
	c.attachments = rem
	kept := c.mentions[:0]
	for _, m := range c.mentions {
		if m.attachmentID != "" && drop[m.attachmentID] && !strings.Contains(c.draft, m.display) {
			continue
		}
		kept = append(kept, m)
	}
	c.mentions = kept
}

// undoLastPaste restores the draft from before the last attachment paste
// when current is still exactly that paste's result.
func (c *composer) undoLastPaste(current string) (caret int, ok bool) {
	snap := c.lastPaste
	if snap == nil || current != snap.after {
		if snap != nil && current != snap.after {
			c.lastPaste = nil
		}
		return 0, false
	}
	ids := append([]string(nil), snap.ids...)
	caret = snap.caret
	c.draft = snap.before
	c.lastPaste = nil
	c.pruneMentions()
	c.dropPastedAttachments(ids)
	return caret, true
}

func (w *workbench) undoComposerPaste(target string, k ui.TextKey) bool {
	comp := w.composerFor(target)
	if comp.lastPaste == nil {
		return false
	}
	current := k.Text()
	if current == "" {
		current = comp.draft
	}
	caret, ok := comp.undoLastPaste(current)
	if !ok {
		return false
	}
	k.SetText(comp.draft)
	comp.caretAfter = caret
	comp.liveAnchor, comp.liveCaret = caret, caret
	comp.caretRestore = true
	w.saveState()
	return true
}

func (w *workbench) removeComposerAttachment(target, attachmentID string) {
	comp := w.composerFor(target)
	removed := false
	for i, current := range comp.attachments {
		if current.ID == attachmentID {
			comp.attachments = append(comp.attachments[:i], comp.attachments[i+1:]...)
			removed = true
			break
		}
	}
	nextDraft := comp.draft
	kept := comp.mentions[:0]
	for _, m := range comp.mentions {
		if m.kind == mentionAttachment && m.attachmentID == attachmentID {
			nextDraft = strings.ReplaceAll(nextDraft, m.display, "")
			removed = true
			continue
		}
		kept = append(kept, m)
	}
	comp.mentions = kept
	if comp.lastPaste != nil && nextDraft != comp.lastPaste.after {
		comp.lastPaste = nil
	}
	comp.draft = nextDraft
	if comp.previewAttachment == attachmentID {
		comp.previewAttachment = ""
	}
	if removed {
		w.saveState()
	}
}
