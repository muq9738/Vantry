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

func TestStardustV2MultiThemeAndSliderPersistence(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "desktop-state.json")
	w := designReviewWorkbench()
	w.defaults.StatePath = statePath
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "appearance"

	tt := ui.NewTester(w.view, 1280, 820)
	tt.Frame()

	// Default palette is Graphite.
	if w.widgets.resolvedPalette.preset != "graphite" {
		t.Fatalf("default preset = %q, want graphite", w.widgets.resolvedPalette.preset)
	}

	// Switch theme preset to Ocean
	mustClick(t, tt, "深海")
	tt.Frame()

	if w.widgets.themePreset != "ocean" {
		t.Fatalf("themePreset = %q, want ocean", w.widgets.themePreset)
	}
	if w.widgets.resolvedPalette.preset != "ocean" {
		t.Fatalf("resolvedPalette.preset = %q, want ocean", w.widgets.resolvedPalette.preset)
	}

	// Restore into a fresh workbench and verify persistence
	w2 := designReviewWorkbench()
	w2.defaults.StatePath = statePath
	if err := w2.restoreState(); err != nil {
		t.Fatalf("restoreState failed: %v", err)
	}
	if w2.widgets.themePreset != "ocean" {
		t.Fatalf("restored themePreset = %q, want ocean", w2.widgets.themePreset)
	}
}

func TestStardustV2ScratchWorkspaceShowsNoProject(t *testing.T) {
	scratch := &api.WorkspaceSpec{Kind: "directory", Path: "/tmp/vantry-data/workspaces/t3_abc/chat"}
	if !isScratchWorkspacePath(scratch.Path) {
		t.Fatal("expected scratch path to be detected")
	}
	if workspaceKey(scratch) != "" {
		t.Fatalf("expected empty workspaceKey for scratch path, got %q", workspaceKey(scratch))
	}
	if workspaceName(scratch) != "" {
		t.Fatalf("expected empty workspaceName for scratch path, got %q", workspaceName(scratch))
	}
	w := designReviewWorkbench()
	w.threads[0].Workspace = scratch
	if got := w.workspaceLabel(); got != "无项目" {
		t.Fatalf("workspaceLabel = %q, want 无项目", got)
	}
	if got := w.selectedWorkspace(); got != nil {
		t.Fatalf("selectedWorkspace for scratch = %+v, want nil", got)
	}
}

func TestStardustV2UnifiedPopoverStatsAndUsageTabs(t *testing.T) {
	w := designReviewWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Frame()

	// 1. Unified model popover shows native effort selector and permission bar
	w.widgets.modelPickerOpen = true
	tt.Frame()
	if !tt.HasText("推理强度") || !tt.HasText("访问权限") {
		t.Fatalf("unified model popover missing effort selector or permission bar: %v", tt.Texts())
	}
	w.widgets.modelPickerOpen = false
	tt.Frame()

	// 2. Bottom stats popover toggles open on click
	mustClick(t, tt, "会话概览与令牌统计")
	if !w.widgets.statsPopoverOpen {
		t.Fatal("clicking stats footer did not open statsPopoverOpen")
	}
	if !tt.HasText("会话概览 · 令牌与缓存") {
		t.Fatalf("stats popover missing title: %v", tt.Texts())
	}
	w.widgets.statsPopoverOpen = false
	tt.Frame()

	// 3. Usage dashboard multi-tab switching
	w.openUsage()
	tt.Frame()
	if !tt.HasText("7 日活跃与费用趋势") || !tt.HasText("效率与结构圆环") {
		t.Fatalf("usage dashboard missing hero cards: %v", tt.Texts())
	}
	mustClick(t, tt, "费用")
	if w.widgets.usageTab != "cost" || !tt.HasText("按模型费用分布") {
		t.Fatalf("usage cost tab failed: tab=%q", w.widgets.usageTab)
	}
	mustClick(t, tt, "令牌")
	if w.widgets.usageTab != "tokens" || !tt.HasText("输出占比") {
		t.Fatalf("usage tokens tab failed: tab=%q", w.widgets.usageTab)
	}
	mustClick(t, tt, "模型")
	if w.widgets.usageTab != "models" {
		t.Fatalf("usage models tab failed: tab=%q", w.widgets.usageTab)
	}
}

// TestStardustV2VisualExport renders verification screenshots into
// VANTRY_STARDUST_V2_RENDER_DIR for human & agent visual inspection.
func TestStardustV2VisualExport(t *testing.T) {
	dir := os.Getenv("VANTRY_STARDUST_V2_RENDER_DIR")
	if dir == "" {
		t.Skip("set VANTRY_STARDUST_V2_RENDER_DIR to render verification PNGs")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	savePNG := func(name string, dark bool, preset string, setup func(w *workbench, tt *ui.Tester)) {
		t.Helper()
		w := designReviewWorkbench()
		w.widgets.themeSet = true
		w.widgets.dark = dark
		w.widgets.themePreset = preset
		w.features.catalog.Models = []api.ModelInfo{
			{Provider: "anthropic", ID: "claude-opus-4-6", Name: "Claude Opus 4.6", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 1000000},
			{Provider: "anthropic", ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 1000000},
			{Provider: "openai", ID: "gpt-5.4-codex", Name: "GPT-5.4 Codex", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 400000},
			{Provider: "google", ID: "gemini-3.1-pro", Name: "Gemini 3.1 Pro", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 1000000},
			{Provider: "deepseek", ID: "deepseek-r2", Name: "DeepSeek R2", Reasoning: true, Input: []string{"text"}, ContextWindow: 128000},
		}
		w.models = append([]api.ModelInfo(nil), w.features.catalog.Models...)
		w.defaults.Model = "anthropic/claude-opus-4-6"
		w.threads[0].Model = "anthropic/claude-opus-4-6:max"
		w.acceptThinking("anthropic/claude-opus-4-6", []string{"low", "medium", "high", "xhigh", "max"})

		tt := ui.NewTesterWith(w.view, 1440, 900, ui.TesterOptions{Dark: dark})
		tt.SetTitleBar(ui.TitleBar{Height: 44, Left: 78})
		tt.Advance(time.Second)
		if setup != nil {
			setup(w, tt)
		}
		tt.Advance(500 * time.Millisecond)
		tt.Frame()

		outPath := filepath.Join(dir, name+".png")
		f, err := os.Create(outPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}

	savePNG("model-popover-graphite-dark", true, "graphite", func(w *workbench, tt *ui.Tester) {
		w.widgets.modelPickerOpen = true
	})
	savePNG("appearance-settings-dark", true, "graphite", func(w *workbench, tt *ui.Tester) {
		w.widgets.settingsOpen = true
		w.widgets.settingsPage = "appearance"
	})
}
