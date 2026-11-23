package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/plugins"
	"github.com/muq9738/Vantry/pkg/api"
)

const stageRemoteDesktop = "plugin:herdr-bridge\x00dashboard"

func remoteDesktopPluginFixture(t *testing.T, w *workbench) {
	t.Helper()
	// The contributed screen page is independent of the IDE machine connection UI.
	w.features.plugins = append(w.features.plugins, api.PluginInfo{ID: "herdr-bridge", Name: "远程桌面", Navigation: []api.PluginNavItem{{Title: "远程桌面", Page: "dashboard", NativePanel: "remote-desktop"}}, Settings: []api.PluginSettingSpec{{Key: "defaultQuality", Value: "balanced"}}})
}

func TestLegacyScreenPanelIsOptional(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	tt := ui.NewTester(w.view, 1440, 900)
	if _, ok := tt.Access("远程桌面"); ok {
		t.Fatal("remote desktop exists before any plugin contributes it")
	}
	if _, ok := tt.Access("打开远程桌面"); ok {
		t.Fatal("title bar has an independent remote desktop entry")
	}
	remoteDesktopPluginFixture(t, w)
	tt.Frame()
	rdControl, ok := tt.Access("远程桌面")
	if !ok {
		t.Fatal("missing top-right remote desktop button")
	}
	if rdControl.Bounds.X < 700 || rdControl.Bounds.Y > 60 {
		t.Fatalf("remote desktop must live in the top-right toolbar, never the left sidebar: %+v", rdControl.Bounds)
	}
	mustClick(t, tt, "远程桌面")
	if w.stage != stageRemoteDesktop || w.rd.legacyRelay != "" || w.sharedRelay() != "" || w.rd.quality != 1 {
		t.Fatalf("plugin route/defaults: stage=%q relay=%q quality=%d", w.stage, w.sharedRelay(), w.rd.quality)
	}
	w.stage = ""
	w.features.plugins = nil
	tt.Frame()
	if _, ok := tt.Access("远程桌面"); ok {
		t.Fatal("remote desktop remains after plugin removal")
	}
}

func TestNativePluginOutputRendersActualWidgets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/plugin-ui\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture")
	if err := os.MkdirAll(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "plugin.json"), []byte(`{"id":"fixture","name":"Renderer fixture","runtime":"native","navigation":[{"title":"Fixture","page":"home"}],"pages":{"home":{"vantryPanel":{"version":1,"widgets":[{"kind":"kv","entries":[{"key":"module","value":"module example.com/plugin-ui"}]}]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	host := plugins.Host{Directory: dir, WorkspaceDir: root}
	output, err := host.Run(context.Background(), "fixture", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	w, _ := v3Workbench(t, nil)
	_, p := workbenchTheme(false, false, false)
	tt := ui.NewTester(func(c *ui.Context) { w.pluginOutputView(c, p, output) }, 1000, 900)
	if !strings.Contains(strings.Join(tt.Texts(), "\n"), "example.com/plugin-ui") {
		t.Fatalf("actual plugin widgets disappeared during UI decode: %q", tt.Texts())
	}
}

func TestSSHDiscoveryFailureDoesNotClaimConfigIsEmpty(t *testing.T) {
	w, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{}, &api.Error{Code: api.InvalidCommand, Message: "未知工作台操作"}
	})
	w.openDock("remote")
	tt := ui.NewTester(w.view, 1440, 900)
	if tt.HasText("还没有主机") || !tt.HasText("读不到 SSH 配置") {
		t.Fatalf("failed discovery is presented as an empty config: %q", tt.Texts())
	}
}
