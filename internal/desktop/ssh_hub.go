package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	actionSSHHosts = "ssh.hosts"
	actionSSHProbe = "ssh.probe"
)

// manualSSHHost is a machine the user added by hand: where it is, who to be
// there, and which key to offer. No credential is kept.
type manualSSHHost struct {
	Name     string `json:"name,omitempty"`
	Host     string `json:"host"`
	User     string `json:"user,omitempty"`
	Port     int    `json:"port,omitempty"`
	Identity string `json:"identity,omitempty"`
}

// input is the probe request of a machine added here.
func (h manualSSHHost) input() json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"host": h.Host, "user": h.User, "port": h.Port, "identity": h.Identity})
	return raw
}

// target is what ssh is asked to connect to.
func (h manualSSHHost) target() string {
	if host := strings.TrimSpace(h.Host); host != "" && strings.TrimSpace(h.User) != "" {
		return strings.TrimSpace(h.User) + "@" + host
	}
	return strings.TrimSpace(h.Host)
}

// title is the name the list shows.
func (h manualSSHHost) title() string {
	if name := strings.TrimSpace(h.Name); name != "" {
		return name
	}
	return strings.TrimSpace(h.Host)
}

// hostProbe is what a connection test found: whether the machine answered,
// how fast, what it runs, and whether Vantry is there.
type hostProbe struct {
	tested    bool
	reachable bool
	// kind names a failure: "auth", "hostkey", "dns", "refused", "timeout",
	// "route", "handshake", "closed" or "other".
	kind     string
	message  string
	latency  int
	remoteOS string
	socket   string
	herdr    bool
	at       time.Time
}

// sshHubState is the SSH & remote machine hub: the hosts of ~/.ssh/config
// (from Core's ssh.hosts), the machines the user added, saved targets, and
// the SSH terminals opened from here.
type sshHubState struct {
	hosts    []api.SSHHostInfo
	loaded   bool
	loading  bool
	err      string
	search   string
	selected string
	// manual are the machines added here, recent when each machine was last
	// connected to, and probes what testing them found.
	manual []manualSSHHost
	recent map[string]time.Time
	probes map[string]hostProbe
	// add is the add-or-edit form.
	add          sshHostForm
	advancedOpen bool
	// sessions are the SSH terminals opened here, shown in the dock's
	// terminal tab.
	sessions map[string]*sshSession
	order    []string
	// connectAfter is the host whose probe should open its remote Vantry.
	connectAfter string
}

// sshHostForm is the add-or-edit form of a machine, with the errors of its
// fields and the result of its connection test.
type sshHostForm struct {
	open    bool
	editing string // the host of the entry being edited, "" for a new one
	name    string
	host    string
	user    string
	port    string
	key     string
	errors  sshHostFormErrors
	testing bool
	tested  bool
	result  hostProbe
}

// sshHostFormErrors are the messages under the form's fields.
type sshHostFormErrors struct {
	host, user, port, key string
}

// sshEntry is one machine of the list: name is what the user reads, target
// what ssh connects to.
type sshEntry struct {
	name, target string
	host         api.SSHHostInfo
	socket       string
	source       string // "ssh", "manual", "favorite", "core"
	favorite     int    // index in remote.saved, or -1
	manual       int    // index in ssh.manual, or -1
	// identity is the key file of a machine added here.
	identity string
	// key identifies the machine in the list and in its terminal tab.
	key string
}

var sshTargetPattern = regexp.MustCompile(`^[A-Za-z0-9._@:\[\]%+-]+$`)

func validSSHTarget(target string) bool {
	return target != "" && !strings.HasPrefix(target, "-") && sshTargetPattern.MatchString(target)
}

// hostKey identifies a machine: its destination and, since two ports tell
// two machines apart, the port when it is not 22.
func hostKey(target string, port int) string {
	if port > 0 && port != 22 {
		return fmt.Sprintf("%s:%d", target, port)
	}
	return target
}

// hostPort is the port of a machine, as ssh would use it.
func hostPort(port int) int {
	if port > 0 {
		return port
	}
	return 22
}

// loadSSHHosts reads the hosts of the SSH configuration through Core, which
// owns the user's home directory.
func (w *workbench) loadSSHHosts(force bool) {
	if (w.ssh.loaded || w.ssh.loading) && !force {
		return
	}
	if w.ssh.loading {
		return
	}
	w.ssh.loading = true
	w.workbenchCall("ssh-hosts", api.WorkbenchRequest{Action: actionSSHHosts}, func(response api.WorkbenchResponse, err error) {
		w.ssh.loading = false
		w.ssh.loaded = err == nil
		if err != nil {
			w.ssh.err = describe(w.tr("读取 ~/.ssh/config 失败", "Reading ~/.ssh/config failed"), err)
			return
		}
		w.ssh.err = ""
		w.ssh.hosts = append([]api.SSHHostInfo(nil), response.SSHHosts...)
	})
}

// probeSSHEntry tests one machine and keeps the answer for its row: an
// alias is probed by name, a machine added here by its address and key.
func (w *workbench) probeSSHEntry(e sshEntry) {
	request := api.WorkbenchRequest{Action: actionSSHProbe, Name: e.target}
	if e.source == "manual" {
		request.Input = manualSSHHost{Name: e.name, Host: e.host.HostName, User: e.host.User, Port: e.host.Port, Identity: e.identity}.input()
	}
	w.workbenchCall("ssh-probe:"+e.key, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.recordProbe(e.key, hostProbe{tested: true, kind: "other", message: describe(w.tr("检测失败", "The test failed"), err), at: time.Now()})
			w.setToolFeedback("remote", notice{Text: describe(w.tr("检测 ", "Check ")+e.name, err), Level: noticeWarn})
			return
		}
		for _, host := range response.SSHHosts {
			probe := probeFromHost(host)
			w.recordProbe(e.key, probe)
			if e.source == "manual" {
				continue
			}
			if host.Alias == "" {
				host.Alias = e.target
			}
			w.mergeSSHHost(host)
		}
		if w.ssh.connectAfter == e.target {
			w.ssh.connectAfter = ""
			e.socket = nonEmptyString(w.probeFor(e).socket, e.socket)
			w.connectRemoteVantryProbed(e)
		}
	})
}

// probeFromHost is what a probe answer says, for the row of a machine.
func probeFromHost(host api.SSHHostInfo) hostProbe {
	probe := hostProbe{
		tested: true, latency: host.LatencyMs, remoteOS: host.RemoteOS,
		socket: host.RemoteSocket, herdr: host.HasHerdr, at: time.Now(),
	}
	if host.Reachable != nil {
		probe.reachable = *host.Reachable
	}
	probe.kind, probe.message = host.ProbeFailure, host.ProbeError
	return probe
}

func (w *workbench) recordProbe(key string, probe hostProbe) {
	if w.ssh.probes == nil {
		w.ssh.probes = map[string]hostProbe{}
	}
	w.ssh.probes[key] = probe
}

// probeFor is what testing a machine found: what Core reported for a host of
// the configuration, or what this window tested itself.
func (w *workbench) probeFor(e sshEntry) hostProbe {
	if probe, ok := w.ssh.probes[e.key]; ok {
		return probe
	}
	if e.host.Reachable == nil {
		return hostProbe{}
	}
	return probeFromHost(e.host)
}

// probing says whether a machine's test is running.
func (w *workbench) probing(e sshEntry) bool {
	return w.features.busy["ssh-probe:"+e.key]
}

// mergeSSHHost folds a probe answer into the known hosts, keeping the
// configuration fields the probe does not repeat.
func (w *workbench) mergeSSHHost(update api.SSHHostInfo) {
	for i, host := range w.ssh.hosts {
		if host.Alias != update.Alias {
			continue
		}
		if update.Reachable != nil {
			host.Reachable = update.Reachable
			host.ProbeError = update.ProbeError
			host.ProbeFailure = update.ProbeFailure
		}
		if update.LatencyMs > 0 {
			host.LatencyMs = update.LatencyMs
		}
		if update.RemoteSocket != "" {
			host.RemoteSocket = update.RemoteSocket
		}
		if update.RemoteOS != "" {
			host.RemoteOS = update.RemoteOS
		}
		host.HasHerdr = host.HasHerdr || update.HasHerdr
		w.ssh.hosts[i] = host
		return
	}
	w.ssh.hosts = append(w.ssh.hosts, update)
}

// sshEntries merges the configuration's hosts with the machines added here,
// saved targets and Core environments: one row per destination, filtered by
// the search.
func (w *workbench) sshEntries() []sshEntry {
	var entries []sshEntry
	index := map[string]int{}
	add := func(e sshEntry) {
		if containsString(w.remote.removed, e.key) || containsString(w.remote.removed, e.target) {
			return
		}
		if i, ok := index[e.key]; ok {
			if entries[i].socket == "" {
				entries[i].socket = e.socket
			}
			if e.favorite >= 0 {
				entries[i].favorite = e.favorite
				if e.name != "" && e.name != e.target {
					entries[i].name = e.name
				}
			}
			return
		}
		index[e.key] = len(entries)
		entries = append(entries, e)
	}
	for _, host := range w.ssh.hosts {
		if host.Alias == "" {
			continue
		}
		add(sshEntry{
			name: host.Alias, target: host.Alias, host: host, socket: host.RemoteSocket,
			source: "ssh", favorite: -1, manual: -1, key: hostKey(host.Alias, host.Port),
		})
	}
	for i, host := range w.ssh.manual {
		e := sshEntry{
			name: host.title(), target: host.target(), source: "manual", favorite: -1, manual: i,
			host:     api.SSHHostInfo{Alias: host.target(), HostName: host.Host, User: host.User, Port: host.Port},
			identity: host.Identity,
		}
		e.key = hostKey(e.target, host.Port)
		if probe, ok := w.ssh.probes[e.key]; ok && probe.socket != "" {
			e.socket = probe.socket
		}
		add(e)
	}
	for i, f := range w.remote.saved {
		add(sshEntry{
			name: f.title(), target: f.Host, host: api.SSHHostInfo{Alias: f.Host}, socket: f.Socket,
			source: "favorite", favorite: i, manual: -1, key: hostKey(f.Host, 0),
		})
	}
	for _, f := range w.remote.core {
		add(sshEntry{
			name: f.title(), target: f.Host, host: api.SSHHostInfo{Alias: f.Host}, socket: f.Socket,
			source: "core", favorite: -1, manual: -1, key: hostKey(f.Host, 0),
		})
	}
	query := strings.ToLower(strings.TrimSpace(w.ssh.search))
	if query == "" {
		return entries
	}
	var out []sshEntry
	for _, e := range entries {
		hay := strings.ToLower(strings.Join([]string{e.name, e.target, e.host.User, e.host.HostName,
			strings.Join(e.host.Aliases, " "), e.host.ProxyJump, w.sourceLabel(e)}, " "))
		if strings.Contains(hay, query) {
			out = append(out, e)
		}
	}
	return out
}

// sshEntry looks a machine up by destination or name, for callers that hold
// only a name, such as a terminal tab.
func (w *workbench) sshEntry(target string) sshEntry {
	for _, e := range w.sshEntries() {
		if e.target == target || e.key == target || e.name == target {
			return e
		}
	}
	return sshEntry{name: target, target: target, host: api.SSHHostInfo{Alias: target}, source: "ssh", favorite: -1, manual: -1, key: target}
}

// sshArgv is the command of a machine's terminal: interactive, so that a
// password or a host key question can be answered in it, and with a connect
// timeout so an unreachable machine fails in seconds instead of hanging.
func (e sshEntry) sshArgv() []string {
	argv := []string{"ssh", "-o", "ConnectTimeout=10"}
	if e.host.Port > 0 && e.host.Port != 22 {
		argv = append(argv, "-p", strconv.Itoa(e.host.Port))
	}
	if e.identity != "" {
		argv = append(argv, "-i", e.identity, "-o", "IdentitiesOnly=yes")
	}
	return append(argv, "--", e.target)
}

// destination is what the row shows: user@host:port, as ssh resolves it, not
// the bare port the configuration happens to name.
func (e sshEntry) destination() string {
	hostname := strings.TrimSpace(e.host.HostName)
	if hostname == "" {
		hostname = e.target
	}
	if i := strings.LastIndex(hostname, "@"); i >= 0 {
		hostname = hostname[i+1:]
	}
	user := strings.TrimSpace(e.host.User)
	if user == "" {
		user = localUser()
	}
	return fmt.Sprintf("%s@%s:%d", user, hostname, hostPort(e.host.Port))
}

// jumpNote says how a machine is reached, when it is not reached directly.
func (e sshEntry) jumpNote() string {
	if e.host.ProxyJump == "" {
		return ""
	}
	return e.host.ProxyJump
}

// sourceLabel names where a machine comes from.
func (w *workbench) sourceLabel(e sshEntry) string {
	switch e.source {
	case "manual":
		return w.tr("手动添加", "Added here")
	case "favorite":
		return w.tr("收藏", "Saved")
	case "core":
		return w.tr("远端 Vantry", "Remote Vantry")
	}
	if e.host.SourceFile != "" {
		return e.host.SourceFile
	}
	return "~/.ssh/config"
}

// recordHostUse remembers when a machine was last connected to.
func (w *workbench) recordHostUse(e sshEntry) {
	if w.ssh.recent == nil {
		w.ssh.recent = map[string]time.Time{}
	}
	w.ssh.recent[e.key] = time.Now()
	w.saveState()
}

// connectRemoteVantry opens the remote Vantry of a machine in a new window.
// Without a known socket it probes the machine first (Core reports the socket
// it finds); only when none is found does the form ask for one.
func (w *workbench) connectRemoteVantry(e sshEntry) {
	w.features.remoteHost = e.target
	w.recordHostUse(e)
	if e.socket == "" && (e.source == "ssh" || e.source == "manual") {
		w.ssh.connectAfter = e.target
		w.setToolFeedback("remote", notice{Text: w.tr("正在检测 "+e.name+" 上的 Vantry…", "Looking for Vantry on "+e.name+"…"), Level: noticeInfo})
		w.probeSSHEntry(e)
		return
	}
	w.connectRemoteVantryProbed(e)
}

func (w *workbench) connectRemoteVantryProbed(e sshEntry) {
	w.features.remoteHost = e.target
	if e.socket == "" {
		w.ssh.advancedOpen = true
		w.setToolFeedback("remote", notice{Text: w.tr("这台机器上还没有找到 Vantry。可以在它的终端里启动 Vantry，或在这里直接填写地址。", "Vantry was not found on this machine. Start Vantry there, or enter its address here."), Level: noticeWarn})
		return
	}
	w.features.remoteSocket = e.socket
	w.connectRemote()
}

// sshRelayHosts offers configured aliases without assuming a relay provider.
func (w *workbench) sshRelayHosts() []string {
	var out []string
	for _, host := range w.ssh.hosts {
		if host.Alias != "" {
			out = append(out, host.Alias)
		}
	}
	return out
}

// localUser is the account ssh uses when the configuration names none.
func localUser() string {
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		return user
	}
	return "user"
}
