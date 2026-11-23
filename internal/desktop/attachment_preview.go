package desktop

import (
	"encoding/json"
	"sync"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// Composer thumbnails come from the bytes this client just imported; Core
// keeps the attachment itself. Bytes are recorded by the upload goroutine and
// decoded once on the UI thread.
var (
	attachmentPreviewData      sync.Map // attachment ID -> []byte
	attachmentPreviewBitmaps   sync.Map // attachment ID -> *ui.Bitmap
	attachmentPreviewAttempted sync.Map // attachment IDs requested from Core
)

func rememberAttachmentPreview(item api.Attachment, data []byte) {
	if item.ID == "" || classifyAttachment(item) != attachImage || len(data) == 0 {
		return
	}
	attachmentPreviewData.Store(item.ID, data)
}

func attachmentPreview(item api.Attachment) *ui.Bitmap {
	if bitmap, ok := attachmentPreviewBitmaps.Load(item.ID); ok {
		return bitmap.(*ui.Bitmap)
	}
	data, ok := attachmentPreviewData.LoadAndDelete(item.ID)
	if !ok {
		return nil
	}
	bitmap, err := ui.DecodeBitmap(data.([]byte))
	if err != nil || bitmap == nil {
		return nil
	}
	attachmentPreviewBitmaps.Store(item.ID, bitmap)
	return bitmap
}

// Restore previews from the owning Core after a window or application restart.
func (w *workbench) loadAttachmentPreview(item api.Attachment) {
	if classifyAttachment(item) != attachImage {
		return
	}
	if _, attempted := attachmentPreviewAttempted.LoadOrStore(item.ID, true); attempted {
		return
	}
	raw, _ := json.Marshal(item)
	w.workbenchCall("attachment-preview:"+item.ID, api.WorkbenchRequest{Action: "attachment.read", Input: raw}, func(response api.WorkbenchResponse, err error) {
		if err == nil {
			rememberAttachmentPreview(item, response.Data)
		}
	})
}

func forgetAttachmentPreview(id string) {
	attachmentPreviewData.Delete(id)
	attachmentPreviewBitmaps.Delete(id)
	attachmentPreviewAttempted.Delete(id)
}
