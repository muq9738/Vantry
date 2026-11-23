package desktop

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestModelPickerUsesPiOnly(t *testing.T) {
	service := newFakeQueryService()
	service.models = []api.ModelInfo{
		{Provider: "openai", ID: "gpt-5", Name: "GPT-5"},
		{Provider: "anthropic", ID: "claude-sonnet-4", Name: "Claude Sonnet 4"},
	}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{})
	tt := ui.NewTester(w.view, 1080, 760)
	w.widgets.modelPickerOpen = true
	tt.Frame()
	for _, text := range []string{"Pi 模型列表 · 2 个", "GPT-5", "Claude Sonnet 4"} {
		if !tt.HasText(text) {
			t.Errorf("picker missing %q: %q", text, tt.Texts())
		}
	}
	for _, retired := range []string{"Antigravity", "Qoder"} {
		if tt.HasText(retired) {
			t.Errorf("picker unexpectedly showed retired provider %q", retired)
		}
	}
	mustClick(t, tt, "openai/gpt-5")
	if w.threadProvider(w.selected) != "pi" || w.currentModel() != "openai/gpt-5" {
		t.Fatalf("selection provider=%s model=%s", w.threadProvider(w.selected), w.currentModel())
	}
	w.setDraft(w.selected, "use pi")
	if !w.send(w.selected) {
		t.Fatal("send did not dispatch")
	}
	commands := service.dispatched()
	if len(commands) == 0 || commands[0].Kind != api.CreateThread || commands[0].ProviderID != "pi" || commands[0].Model != "openai/gpt-5" {
		t.Fatalf("create = %+v", commands)
	}
	sections := w.modelSections(strings.ToLower("sonnet"))
	if len(sections) != 1 || sections[0].id != "pi" || len(sections[0].models) != 1 {
		t.Fatalf("search sections = %+v", sections)
	}
}

func TestProviderSettingsPiOnlyAndCodemode(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "providers"
	tt := ui.NewTester(w.view, 1100, 900)
	tt.Frame()
	for _, text := range []string{"Pi 执行器", "pi --mode rpc", "codemode", "测试连接", "管理 MCP"} {
		if !tt.HasText(text) {
			t.Errorf("settings missing %q: %v", text, tt.Texts())
		}
	}
	for _, forbidden := range []string{"Antigravity", "Qoder", "自定义 ACP"} {
		if tt.HasText(forbidden) {
			t.Errorf("settings still mention retired provider %q", forbidden)
		}
	}
	mustClick(t, tt, "管理 MCP")
	tt.Frame()
	if w.widgets.settingsOpen || w.features.panel != "mcp" {
		t.Fatalf("clicking Manage MCP did not switch to MCP dock: settingsOpen=%v panel=%q", w.widgets.settingsOpen, w.features.panel)
	}
}
