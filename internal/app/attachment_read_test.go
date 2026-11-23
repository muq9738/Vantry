package app

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestReadImportedAttachmentForRestoredPreview(t *testing.T) {
	service := &workbenchService{config: Config{DataDir: t.TempDir()}}
	ctx := context.Background()
	data := []byte("an imported attachment")
	imported, err := service.Workbench(ctx, api.WorkbenchRequest{Action: "attachment.import", Name: "note.txt", Data: data})
	if err != nil || imported.Attachment == nil {
		t.Fatalf("import: %v", err)
	}
	raw, _ := json.Marshal(imported.Attachment)
	response, err := service.Workbench(ctx, api.WorkbenchRequest{Action: "attachment.read", Input: raw})
	if err != nil || !bytes.Equal(response.Data, data) {
		t.Fatalf("attachment read: %q %v", response.Data, err)
	}
	changed := *imported.Attachment
	changed.Name = "different.txt"
	raw, _ = json.Marshal(changed)
	if _, err := service.Workbench(ctx, api.WorkbenchRequest{Action: "attachment.read", Input: raw}); err == nil {
		t.Fatal("changed attachment identity was accepted")
	}
}
