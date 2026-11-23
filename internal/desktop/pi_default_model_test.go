package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestRestoringPreferencesCannotOverridePiSettingsModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop-state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"selected":"local.1","localOrder":["local.1"],"defaults":{"providerId":"pi","model":"magpie/old-model"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	service := &fakeService{}
	w := newWorkbench(service)
	w.defaults = Options{StatePath: path, ProviderID: "pi", Model: "magpie/pi-settings-model"}
	if err := w.restoreState(); err != nil {
		t.Fatal(err)
	}
	w.applySnapshot(api.Snapshot{})
	if w.currentModel() != "magpie/pi-settings-model" {
		t.Fatalf("saved UI preference replaced Pi settings: %q", w.currentModel())
	}
	w.setDraft(w.selected, "use the Pi settings model")
	if !w.send(w.selected) {
		t.Fatal("first message was not dispatched")
	}
	commands := service.dispatched()
	if len(commands) == 0 || commands[0].Kind != api.CreateThread || commands[0].Model != "magpie/pi-settings-model" {
		t.Fatalf("new thread did not use Pi settings: %+v", commands)
	}
}

func TestNewSessionUsesPiDefaultInsteadOfPreviousSessionChoice(t *testing.T) {
	service := &fakeService{}
	w := newWorkbench(service)
	w.defaults = Options{ProviderID: "pi", Model: "magpie/pi-settings-model"}
	if err := w.restoreState(); err != nil {
		t.Fatal(err)
	}
	first := w.newSession()
	w.chooseModel("example/manual-model")
	if w.currentModel() != "example/manual-model" {
		t.Fatal("manual choice did not apply to the current draft")
	}
	w.setDraft(first, "keep this draft")
	second := w.newSession()
	if first == second || w.currentModel() != "magpie/pi-settings-model" {
		t.Fatalf("new session inherited another session's choice: first=%s second=%s model=%s", first, second, w.currentModel())
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("choosing a model or creating a draft dispatched a command")
	}
	w.selectThread(first)
	if w.currentModel() != "example/manual-model" {
		t.Fatal("opening another session replaced the original draft's manual choice")
	}
	w.chooseModel("")
	if w.currentModel() != "" {
		t.Fatal("explicit follow-default selection was replaced by a manual model")
	}
}

func TestModelPickerShowsPiDefaultAndOtherAvailableModels(t *testing.T) {
	service := newFakeQueryService()
	service.models = []api.ModelInfo{
		{Provider: "pi", ID: "default", Name: "Pi Settings Default"},
		{Provider: "other", ID: "available", Name: "Other Available Model"},
	}
	w := newWorkbench(service)
	w.defaults.Model = "pi/default"
	if err := w.restoreState(); err != nil {
		t.Fatal(err)
	}
	w.applySnapshot(api.Snapshot{})
	tester := ui.NewTester(w.view, 1080, 700)
	trigger := func(model string) bool {
		info, ok := tester.Access("选择模型")
		return ok && strings.Contains(info.Description, "当前模型："+model+"。")
	}
	if !trigger("Pi Settings Default") {
		t.Error("model trigger does not show the Pi settings default")
	}
	for _, text := range []string{"Pi 模型列表 · 2 个", "使用 Pi 默认模型", "Pi Settings Default", "Other Available Model", "默认"} {
		if !tester.HasText(text) {
			t.Errorf("model picker is missing %q", text)
		}
	}
	mustClick(t, tester, "other/available")
	if w.widgets.modelPickerOpen || !trigger("Other Available Model") {
		t.Fatal("manual model selection is not visible on the trigger")
	}
	mustClick(t, tester, "选择模型")
	mustClick(t, tester, "跟随默认模型")
	if !trigger("Pi Settings Default") || w.currentModel() != "" {
		t.Fatal("follow-default does not display the model from Pi settings")
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("browsing models created a conversation or model request")
	}
}
