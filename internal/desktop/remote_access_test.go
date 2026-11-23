package desktop

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

func remoteAccessPlugin(t *testing.T) api.PluginInfo {
	t.Helper()
	return api.PluginInfo{ID: "legacy-ide-panel", Name: "远程访问", Navigation: []api.PluginNavItem{{Title: "远程访问", Page: "dashboard", NativePanel: "remote-access"}}}
}

func TestLegacyPanelContractPairsWithoutSSHOrScreenCapture(t *testing.T) {
	status := remoteaccess.Status{State: "off", Name: "Mac Studio", Peers: []remoteaccess.PeerInfo{}}
	w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action == "access.pair" {
			status.Peers = []remoteaccess.PeerInfo{{ID: "peer-1", Name: "Mac mini", Relay: "https://relay.example.com"}}
		}
		data, _ := json.Marshal(status)
		return api.WorkbenchResponse{Output: string(data)}, nil
	})
	plugin := remoteAccessPlugin(t)
	w.features.plugins = []api.PluginInfo{plugin}
	w.setToolFeedback("remote", notice{Text: "SSH-only feedback", Level: noticeInfo})
	tt := ui.NewTester(w.view, 1440, 900)
	if _, ok := tt.Access("远程访问"); ok {
		t.Fatal("plugin pages must not appear in the left sidebar")
	}
	w.openPluginPage(plugin, plugin.Navigation[0])
	tt.Frame()
	if tt.HasText("SSH-only feedback") {
		t.Fatal("SSH state leaked into the relay plugin")
	}
	if !tt.HasText("设备") || !tt.HasText("优先局域网直连，跨网自动使用中继。") {
		t.Fatalf("remote access page missing: %q", tt.Texts())
	}
	for _, action := range service.actions() {
		if strings.HasPrefix(action, "ssh.") || strings.HasPrefix(action, "rd.") {
			t.Fatalf("remote access touched unrelated transport: %s", action)
		}
	}
	mustClick(t, tt, "添加配对设备")
	mustClick(t, tt, "会话配对码或链接")
	tt.Type("vantry://connect#test-link")
	mustClick(t, tt, "确认配对")
	tt.Frame()
	if !tt.HasText("Mac mini") {
		t.Fatal("paired host not displayed")
	}
	if r, ok := service.find("access.pair"); !ok || r.Message != "vantry://connect#test-link" {
		t.Fatal("pair link not submitted to host service")
	}
	mustClick(t, tt, "远程访问设置")
	tt.Frame()
	if !tt.HasText("跨网中继地址") || tt.HasText("画质") || tt.HasText("SSH") || tt.HasText("远程桌面目标") {
		t.Fatalf("wrong host controls: %q", tt.Texts())
	}
}

func TestSSHItemRemovalSurvivesRefreshAndRestart(t *testing.T) {
	answer := func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{SSHHosts: []api.SSHHostInfo{{Alias: "retired"}, {Alias: "mini"}}}, nil
	}
	w, _ := v3Workbench(t, answer)
	w.defaults.StatePath = filepath.Join(t.TempDir(), "desktop-state.json")
	w.openDock("remote")
	tt := ui.NewTester(w.view, 1280, 820)
	mustClick(t, tt, "主机 retired")
	w.removeHost(w.sshEntry("retired"))
	tt.Frame()
	if _, ok := tt.Access("主机 retired"); ok {
		t.Fatal("removed host remains visible")
	}
	w.loadSSHHosts(true)
	tt.Frame()
	if _, ok := tt.Access("主机 retired"); ok {
		t.Fatal("refresh resurrected removed host")
	}
	restored, _ := v3Workbench(t, answer)
	restored.defaults.StatePath = w.defaults.StatePath
	if err := restored.restoreState(); err != nil {
		t.Fatal(err)
	}
	restored.loadSSHHosts(true)
	for _, entry := range restored.sshEntries() {
		if entry.target == "retired" {
			t.Fatal("restart resurrected removed host")
		}
	}
	restored.restoreSSHEntries()
	found := false
	for _, entry := range restored.sshEntries() {
		found = found || entry.target == "retired"
	}
	if !found {
		t.Fatal("restore action did not recover auto-discovered host")
	}
}

func TestRemoteAccessLayouts(t *testing.T) {
	for _, width := range []int{1440, 900, 640} {
		w, _ := v3Workbench(t, nil)
		plugin := remoteAccessPlugin(t)
		w.features.plugins = []api.PluginInfo{plugin}
		w.access.loaded = true
		w.access.name = "Mac Studio"
		w.access.status = remoteaccess.Status{State: "online", Connected: true, Enabled: true, Peers: []remoteaccess.PeerInfo{{ID: "mini", Name: "Mac mini", Relay: "https://relay.example.com"}}}
		w.stage = pluginStage(plugin.ID, "dashboard")
		for tab := 0; tab < 2; tab++ {
			w.access.settings = tab == 1
			tt := ui.NewTester(w.view, width, 900)
			tt.Frame()
			if !tt.HasText("远程访问") {
				t.Fatalf("remote access missing at %d", width)
			}
		}
	}
}
