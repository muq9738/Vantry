package app

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestReplyImageReadsWorkspaceAndRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write := func(path string) {
		t.Helper()
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "plot.png"))
	write(filepath.Join(outside, "other.png"))
	if err := os.Symlink(filepath.Join(outside, "other.png"), filepath.Join(root, "link.png")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"plot.png", filepath.Join(root, "plot.png")} {
		data, err := readReplyImage(root, path)
		if err != nil || len(data) == 0 {
			t.Fatalf("valid image %q: %v", path, err)
		}
	}
	for _, path := range []string{filepath.Join(outside, "other.png"), "../other.png", "link.png", "."} {
		if _, err := readReplyImage(root, path); err == nil {
			t.Errorf("read outside/special path %q", path)
		}
	}
}
