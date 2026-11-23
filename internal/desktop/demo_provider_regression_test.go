package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// An older build saved the offline demo executor in desktop-state.json, so
// every new thread became a demo whose model could not be chosen.
func TestSavedDemoExecutorIsIgnoredAndDemoThreadCanChooseModel(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "desktop-state.json")
	raw, _ := json.Marshal(map[string]any{"version": desktopStateVersion, "defaults": map[string]any{"providerId": "controlled"}})
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	w := newWorkbench(newFakeQueryService())
	w.defaults = Options{StatePath: statePath, ProviderID: "pi", ApprovalPolicy: "auto"}
	if err := w.restoreState(); err != nil {
		t.Fatal(err)
	}
	if w.defaults.ProviderID != "pi" {
		t.Fatalf("saved demo executor restored: %q", w.defaults.ProviderID)
	}
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{{ID: "demo", Title: "Demo", ProviderID: "controlled", Model: "m"}}})
	w.selectThread("demo")
	if !w.modelSwitchable("demo") || w.piThread("demo") {
		t.Fatal("a mistaken demo thread must offer the model picker without being treated as a Pi run")
	}
	tt := ui.NewTester(w.view, 1080, 760)
	if info, ok := tt.Access("选择模型"); !ok || info.Disabled {
		t.Fatalf("model picker unavailable for the demo thread: %+v %v", info, ok)
	}
	if _, ok := tt.Access("思考强度"); !ok {
		t.Fatal("thinking effort button missing")
	}
	w.widgets.settingsOpen = true
	tt.Frame()
	if _, ok := tt.Access("离线演示"); ok {
		t.Fatal("settings still offer the offline demo executor")
	}
}
