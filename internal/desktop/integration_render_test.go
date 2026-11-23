package desktop

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestIntegrationReviewFrames(t *testing.T) {
	dir := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if dir == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR to export review frames")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"worktree", "git-expanded", "commit"} {
		for _, size := range []struct {
			name string
			w, h int
			dark bool
		}{{"1080", 1080, 700, true}, {"640", 640, 600, false}} {
			t.Run(mode+"-"+size.name, func(t *testing.T) {
				w := nativeReviewWorkbench()
				w.runs[0].Status = api.RunCompleted
				w.widgets.themeSet, w.widgets.dark = true, size.dark
				if mode == "worktree" {
					w.beginProjectChoice()
					w.projectChoice.mode = 1
					w.projectChoice.base = "main"
					w.projectChoice.branch = "feature/native-ui"
					w.widgets.projectPickerOpen = true
				} else {
					w.features.panel = "git"
					state := w.features.git["t1"]
					state.Files = append(state.Files, api.GitFile{Path: "internal/desktop/chrome.go", Index: "M", Worktree: " "})
					state.Diff = "[已暂存 / staged]\ndiff --git a/internal/desktop/chrome.go b/internal/desktop/chrome.go\n--- a/internal/desktop/chrome.go\n+++ b/internal/desktop/chrome.go\n@@ -1 +1 @@\n-old icons\n+consistent icons\n" + state.Diff
					if mode == "commit" {
						w.gitView.commitOpen = true
					} else {
						w.gitView.expanded = true
					}
				}
				tt := ui.NewTester(w.view, size.w, size.h)
				if mode == "commit" {
					tt.Type("统一输入区和侧栏图标")
				}
				file, err := os.Create(filepath.Join(dir, "integration-"+mode+"-"+size.name+".png"))
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := png.Encode(file, tt.Image()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
