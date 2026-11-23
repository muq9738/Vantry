package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// TestManualHostFormSavesAndPersists adds a machine by hand and finds it
// again after a restart, with its key and port, and without a credential.
func TestManualHostFormSavesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w, _ := v3Workbench(t, nil)
	w.defaults.StatePath = path
	w.openSSHHostForm("")
	w.ssh.add.name, w.ssh.add.host, w.ssh.add.user = "构建机", "10.0.0.12", "root"
	w.ssh.add.port, w.ssh.add.key = "2222", "~/.ssh/id_ed25519"
	w.saveSSHHostForm()
	if w.ssh.add.open {
		t.Fatal("the form stays open after saving")
	}
	entries := w.sshEntries()
	if len(entries) != 1 || entries[0].source != "manual" || entries[0].name != "构建机" {
		t.Fatalf("entries = %+v", entries)
	}
	if got, want := entries[0].destination(), "root@10.0.0.12:2222"; got != want {
		t.Fatalf("destination = %q, want %q", got, want)
	}
	if got := sshCommandLine(entries[0].sshArgv()); got != "ssh -o ConnectTimeout=10 -p 2222 -i ~/.ssh/id_ed25519 -o IdentitiesOnly=yes -- root@10.0.0.12" {
		t.Fatalf("command = %q", got)
	}
	// A second save of the same machine is refused rather than doubled.
	w.openSSHHostForm("")
	w.ssh.add.host, w.ssh.add.user, w.ssh.add.port = "10.0.0.12", "root", "2222"
	w.saveSSHHostForm()
	if len(w.ssh.manual) != 1 {
		t.Fatalf("duplicate saved: %+v", w.ssh.manual)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("state json: %v", err)
	}
	if len(state.ManualHosts) != 1 || state.ManualHosts[0].Identity != "~/.ssh/id_ed25519" {
		t.Fatalf("persisted = %+v", state.ManualHosts)
	}
	restored, _ := v3Workbench(t, nil)
	restored.defaults.StatePath = path
	if err := restored.restoreState(); err != nil {
		t.Fatal(err)
	}
	if len(restored.sshEntries()) != 1 || restored.sshEntries()[0].target != "root@10.0.0.12" {
		t.Fatalf("restored entries = %+v", restored.sshEntries())
	}
}

// TestManualHostFormValidates checks the form's messages: a host is needed,
// an address is not a user, and a port is a port.
func TestManualHostFormValidates(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	w.openSSHHostForm("")
	w.saveSSHHostForm()
	if w.ssh.add.errors.host == "" || len(w.ssh.manual) != 0 {
		t.Fatalf("an empty host was saved: %+v", w.ssh.add)
	}
	w.ssh.add.host, w.ssh.add.user = "root@10.0.0.12", "root"
	w.saveSSHHostForm()
	if w.ssh.add.errors.host == "" {
		t.Fatal("a user in the host field was accepted")
	}
	w.ssh.add.host, w.ssh.add.user, w.ssh.add.port = "10.0.0.12", "root", "70000"
	w.saveSSHHostForm()
	if w.ssh.add.errors.port == "" {
		t.Fatal("an impossible port was accepted")
	}
	w.ssh.add.port = "22"
	w.saveSSHHostForm()
	if len(w.ssh.manual) != 1 {
		t.Fatalf("a valid machine was not saved: %+v", w.ssh.add.errors)
	}
}

// TestSSHEntryShowsResolvedDestination is the fix of a row that read
// ":32222" and nothing else: the destination is user@host:port as ssh
// resolves it, from the configuration or from the machine added by hand.
func TestSSHEntryShowsResolvedDestination(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	configured := sshEntry{name: "orb", target: "orb", host: api.SSHHostInfo{Alias: "orb", User: "root", HostName: "orb.internal", Port: 32222}}
	if got, want := configured.destination(), "root@orb.internal:32222"; got != want {
		t.Fatalf("configured destination = %q, want %q", got, want)
	}
	// A host whose configuration names no user or host is shown with what
	// ssh would use: the local account and the alias itself.
	plain := sshEntry{name: "mini", target: "mini"}
	if got, want := plain.destination(), localUser()+"@mini:22"; got != want {
		t.Fatalf("plain destination = %q, want %q", got, want)
	}
	// Two machines on one address are told apart by their port.
	if hostKey("orb.internal", 32222) == hostKey("orb.internal", 22) {
		t.Fatal("two ports of one address share a key")
	}
	if key := w.sshEntry("mini").key; key != "mini" {
		t.Fatalf("an unknown machine still gets a key: %q", key)
	}
}

// TestHostStatusTextNamesEveryState covers what a row says about a machine.
func TestHostStatusTextNamesEveryState(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	_, p := workbenchTheme(false, false, false)
	entry := sshEntry{name: "orb", target: "orb"}
	online := hostProbe{tested: true, reachable: true, latency: 24}
	if text, color := w.hostStatusText(p, entry, online); text != "在线 24 ms" || color != p.success {
		t.Fatalf("online = %q", text)
	}
	if text, _ := w.hostStatusText(p, entry, hostProbe{}); text != "未检测" {
		t.Fatalf("untested = %q", text)
	}
	if text, _ := w.hostStatusText(p, entry, hostProbe{tested: true, kind: "auth"}); text != "认证失败" {
		t.Fatalf("auth = %q", text)
	}
	if text, _ := w.hostStatusText(p, entry, hostProbe{tested: true, kind: "timeout"}); text != "离线" {
		t.Fatalf("timeout = %q", text)
	}
	if got := latencyText(1681); got != "1.7 s" {
		t.Fatalf("latency = %q", got)
	}
	if got := osLabel("Linux 7.0.0-27-generic x86_64"); got != "Linux 7.0.0-27-generic" {
		t.Fatalf("os = %q", got)
	}
	if got := osLabel("Darwin 25.1.0 arm64"); got != "macOS 25.1.0" {
		t.Fatalf("os = %q", got)
	}
}

// TestTerminalTabsShowEachMachineAndItsState covers the dock's terminal tab:
// one tab per machine with its name and a state dot, the state bar of a
// dropped session with the reason and a way back, and closing a tab.
func TestTerminalTabsShowEachMachineAndItsState(t *testing.T) {
	w, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{SSHHosts: []api.SSHHostInfo{{Alias: "qmt"}, {Alias: "mini"}}}, nil
	})
	var opened []terminal.Options
	w.newTerminal = pipeTerminals(t, &opened)
	w.ssh.loaded = true
	w.ssh.hosts = []api.SSHHostInfo{{Alias: "qmt"}, {Alias: "mini"}}
	tt := ui.NewTester(w.view, 1080, 760)
	for _, host := range []string{"qmt", "mini"} {
		w.openSSHEntry(w.sshEntry(host))
	}
	tt.Frame()
	if w.features.panel != "terminal" || w.termTab.active != "mini" {
		t.Fatalf("panel=%q active=%q", w.features.panel, w.termTab.active)
	}
	for _, label := range []string{"终端 qmt", "终端 mini"} {
		if _, ok := tt.Access(label); !ok {
			t.Fatalf("terminal tab %q missing: %q", label, tt.Texts())
		}
	}
	mustClick(t, tt, "终端 qmt")
	tt.Frame()
	if w.termTab.active != "qmt" {
		t.Fatalf("active = %q", w.termTab.active)
	}
	// A session that drops says why, in words, over the terminal it leaves.
	session := w.ssh.sessions["qmt"]
	session.mu.Lock()
	session.phase, session.settled = sshFailed, true
	session.failure = &sshFailure{title: "认证失败", detail: "服务器拒绝了你的密钥或密码。"}
	session.mu.Unlock()
	tt.Frame()
	for _, text := range []string{"认证失败", "服务器拒绝了你的密钥或密码。", "重新连接", "复制命令"} {
		if !tt.HasText(text) {
			t.Fatalf("the dropped session is missing %q: %q", text, tt.Texts())
		}
	}
	mustClick(t, tt, "复制命令")
	if got := tt.Clipboard(); got != "ssh -o ConnectTimeout=10 -- qmt" {
		t.Fatalf("copied command = %q", got)
	}
	before := len(opened)
	mustClick(t, tt, "重新连接")
	tt.Frame()
	if len(opened) != before+1 || w.ssh.sessions["qmt"] == session {
		t.Fatal("reconnect did not start a new session")
	}
	mustClick(t, tt, "关闭终端 mini")
	tt.Frame()
	if w.ssh.sessions["mini"] != nil || len(w.ssh.order) != 1 || w.ssh.order[0] != "qmt" {
		t.Fatalf("close left sessions=%v order=%v", w.ssh.sessions, w.ssh.order)
	}
	w.closeSSHTerminals()
}
