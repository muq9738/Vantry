package desktop

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestRemoteDesktopStartsWithLANAndDoesNotInventARelay(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	remoteDesktopPluginFixture(t, w)
	w.defaults.RemoteDesktop = &fakeRemoteDesktop{}
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "远程桌面")
	if cfg := w.remoteDesktopConfig(); !cfg.DirectOnly || cfg.RelayOnly || cfg.RelaySSHHost != "" {
		t.Fatal("opening a fresh page silently configured an Internet relay")
	}
	// The plugin keeps no relay address of its own: no URL fields or mode
	// switches, only the shared environment relay, read-only.
	for _, label := range []string{"观看端中继地址", "桌面共享中继地址", "远程连接方式", "本机共享范围"} {
		if _, ok := tt.Access(label); ok {
			t.Fatalf("remote desktop exposed its own relay configuration: %s", label)
		}
	}
	if !tt.HasText("未配置 · 仅局域网") {
		t.Fatalf("missing shared relay row: %q", tt.Texts())
	}
	if strings.Contains(strings.ToLower(strings.Join(tt.Texts(), " ")), "qmt") {
		t.Fatal("private infrastructure appeared in a new user's page")
	}
	mustClick(t, tt, "远程桌面目标")
	tt.Type("vantry-rtc://own-relay/test?token=private-test-only-token")
	mustClick(t, tt, "立即远程连接")
	if w.rd.connecting || w.rd.connected || !strings.Contains(w.rd.err, "中继") {
		t.Fatalf("an unconfigured relay attempted a connection or fallback: %q", w.rd.err)
	}
	w.access.status.Relay = "https://relay.mine.example/"
	tt.Frame()
	if !tt.HasText("https://relay.mine.example") {
		t.Fatalf("shared relay not shown: %q", tt.Texts())
	}
	if cfg := w.remoteDesktopConfig(); !cfg.RelayOnly || cfg.DirectOnly || cfg.RelaySSHHost != "https://relay.mine.example" {
		t.Fatalf("the shared environment relay was not used: %+v", cfg)
	}
}

func TestHostingUsesTheSharedRelayWithoutAskingForOne(t *testing.T) {
	w, fixture := permissionWorkbench(t, "darwin")
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "远程桌面")
	before := fixture.probes
	mustClick(t, tt, "本机被控")
	if !w.rd.permission.intent || fixture.probes <= before {
		t.Fatal("sharing must start without a relay of its own")
	}
	if strings.Contains(w.rd.permission.err, "中继") {
		t.Fatalf("sharing asked for a relay: %q", w.rd.permission.err)
	}
}
