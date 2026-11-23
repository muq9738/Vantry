package attachments

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUploadedCopyAndMetadataAreIndependent(t *testing.T) {
	store := Store{Directory: filepath.Join(t.TempDir(), "store")}
	for _, name := range []string{"截图.png", "metadata.json", "../notes.txt"} {
		item, err := store.Import(name, []byte("uploaded data"))
		if err != nil {
			t.Fatal(err)
		}
		path, err := store.Resolve(item)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "uploaded data" {
			t.Fatalf("upload: %s %v", data, err)
		}
		changed := item
		changed.Name = "other.txt"
		if _, err := store.Resolve(changed); err == nil {
			t.Fatal("client changed file metadata")
		}
		changed = item
		changed.ID = "../../outside"
		if _, err := store.Resolve(changed); err == nil {
			t.Fatal("path traversal accepted")
		}
	}
	if _, err := store.Import("large.txt", make([]byte, MaxSize+1)); err == nil {
		t.Fatal("oversized upload accepted")
	}
}
