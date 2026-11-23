package desktop

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestProviderViewsRender(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name          string
		width, height int
		dark          bool
	}{{"light", 1200, 950, false}, {"dark", 1080, 800, true}, {"narrow", 720, 780, false}} {
		for _, settings := range []bool{false, true} {
			t.Run(spec.name+map[bool]string{true: "-providers", false: "-model-groups"}[settings], func(t *testing.T) {
				service := newFakeQueryService()
				service.models = []api.ModelInfo{
					{Provider: "openai", ID: "gpt-5", Name: "GPT-5"},
					{Provider: "anthropic", ID: "claude-sonnet-4", Name: "Claude Sonnet 4"},
				}
				w := newWorkbench(service)
				w.defaults.StatePath = filepath.Join(t.TempDir(), "desktop-state.json")
				if settings {
					w.providers.health = &piHealth{OK: true, Version: "1.5.2", Detail: "Pi CLI 1.5.2 · 2 个模型就绪", ModelCount: 2}
				}
				w.applySnapshot(api.Snapshot{})
				w.widgets.modelPickerOpen = !settings
				w.widgets.settingsOpen = settings
				w.widgets.settingsPage = "providers"
				tt := ui.NewTesterWith(w.view, spec.width, spec.height, ui.TesterOptions{Dark: spec.dark})
				tt.Advance(time.Second)
				f, err := os.Create(filepath.Join(output, t.Name()[len("TestProviderViewsRender/"):]+".png"))
				if err != nil {
					t.Fatal(err)
				}
				if err = png.Encode(f, tt.Image()); err != nil {
					t.Fatal(err)
				}
				if err = f.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
