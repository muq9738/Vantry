package desktop

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// sshPhase is where one SSH terminal stands, as its tab and its status bar
// show it.
type sshPhase int

const (
	// sshConnecting is a session whose ssh has neither printed anything nor
	// ended yet.
	sshConnecting sshPhase = iota
	// sshPrompting waits for the user: a password, a passphrase, or a
	// confirmation of an unknown host key.
	sshPrompting
	// sshReady is a session that got through: output is flowing.
	sshReady
	// sshEnded is a session that ended normally.
	sshEnded
	// sshFailed is one that could not connect, with the reason read from
	// what ssh printed.
	sshFailed
)

// sshFailure says in words why a session stopped: a title for the status
// bar, the detail of what to do, and the raw ssh line for the log.
type sshFailure struct {
	title  string
	detail string
	raw    string
}

// sshSession is one machine's SSH terminal: the pty Vantry runs ssh in, the
// terminal view that shows it, and the state of the connection. It exists
// because a session is more than a program: a machine whose connection fails
// says why, and offers to connect again, instead of showing a blank screen.
type sshSession struct {
	target string
	// name is what the tab shows: the alias, or the host of a manual entry.
	name string
	argv []string

	term *terminal.Terminal
	run  *programRun

	started time.Time
	// clicked is when the user asked for this command, for 复制命令.
	command string

	mu        sync.Mutex
	phase     sshPhase
	failure   *sshFailure
	code      int
	endedAt   time.Time
	sawOutput bool
	prompted  bool
	promptAt  int
	settled   bool
}

// sshArgv is the command of an SSH terminal: interactive, so that a password
// or a host key question can be answered in the terminal itself, with a
// connect timeout so an unreachable machine fails in seconds instead of
// hanging, and with the line kept for 复制命令.
func sshCommandLine(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if arg == "" || strings.ContainsAny(arg, " \t\"'$`\\|&;<>()*?[]#") {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		} else {
			quoted[i] = arg
		}
	}
	return strings.Join(quoted, " ")
}

// openSSHTerminal opens a terminal on target in the dock's terminal tab, or
// shows the one already open. It reports whether a session is there.
func (w *workbench) openSSHTerminal(target string) bool {
	return w.openSSHEntry(w.sshEntry(strings.TrimSpace(target)))
}

// openSSHEntry opens a machine's terminal, or shows the one already open.
func (w *workbench) openSSHEntry(e sshEntry) bool {
	if !validSSHTarget(e.target) {
		w.setToolFeedback("remote", notice{Text: w.tr("主机名只能包含字母、数字和 . _ @ : -", "Host names may contain letters, digits and . _ @ : - only"), Level: noticeError})
		return false
	}
	if w.ssh.sessions == nil {
		w.ssh.sessions = map[string]*sshSession{}
	}
	if session := w.ssh.sessions[e.key]; session != nil {
		w.termTab.active = e.key
		w.features.panel = "terminal"
		return true
	}
	session := w.startSSHSession(e)
	w.ssh.sessions[e.key] = session
	if !containsString(w.ssh.order, e.key) {
		w.ssh.order = append(w.ssh.order, e.key)
	}
	w.termTab.active = e.key
	w.features.panel = "terminal"
	w.setToolFeedback("remote", notice{})
	w.recordHostUse(e)
	return true
}

// startSSHSession runs ssh in an embedded terminal and tracks the session.
// A native terminal that cannot start at all still gets a session, whose
// status bar says the reason.
func (w *workbench) startSSHSession(e sshEntry) *sshSession {
	session := &sshSession{
		target:  e.key,
		name:    e.name,
		argv:    e.sshArgv(),
		started: time.Now(),
		phase:   sshConnecting,
	}
	session.command = sshCommandLine(session.argv)
	options := terminal.Options{Scrollback: 4 << 20}
	term, run, err := w.launchTerminal(options, session.argv, func(code int) string {
		return session.ending(w)
	})
	if err != nil {
		session.mu.Lock()
		session.phase = sshFailed
		session.settled = true
		session.failure = &sshFailure{
			title:  w.tr("无法启动终端", "The terminal could not start"),
			detail: describe(w.tr("检查 ssh 是否可用", "Check that ssh is available"), err),
		}
		session.mu.Unlock()
		return session
	}
	session.term = term
	session.run = run
	if run != nil {
		go func() {
			<-run.Exited()
			// The last bytes of output may still be on their way: let the
			// reader deliver them before the reason is drawn from them.
			time.Sleep(300 * time.Millisecond)
			w.post(func() { session.settle(w) })
		}()
	}
	return session
}

// reconnectSSHTerminal replaces a session's terminal with a new ssh.
func (w *workbench) reconnectSSHTerminal(target string) {
	old := w.ssh.sessions[target]
	if old != nil {
		_ = old.close()
	}
	session := w.startSSHSession(w.sshEntry(target))
	if old != nil {
		session.name = old.name
	}
	w.ssh.sessions[target] = session
	w.termTab.active = target
	w.features.panel = "terminal"
}

// closeSSHTerminal closes one session and its terminal.
func (w *workbench) closeSSHTerminal(target string) {
	if session := w.ssh.sessions[target]; session != nil {
		_ = session.close()
		delete(w.ssh.sessions, target)
	}
	for i, t := range w.ssh.order {
		if t == target {
			w.ssh.order = append(w.ssh.order[:i:i], w.ssh.order[i+1:]...)
			break
		}
	}
	if w.termTab.active == target {
		w.termTab.active = ""
		if n := len(w.ssh.order); n > 0 {
			w.termTab.active = w.ssh.order[n-1]
		}
	}
}

// closeSSHTerminals closes every SSH terminal, as closing the window does.
func (w *workbench) closeSSHTerminals() {
	for _, session := range w.ssh.sessions {
		_ = session.close()
	}
}

// close closes the terminal, which hangs up ssh.
func (s *sshSession) close() error {
	if s.term == nil {
		return nil
	}
	err := s.term.Close()
	s.mu.Lock()
	s.settled = true
	if s.phase == sshConnecting || s.phase == sshReady || s.phase == sshPrompting {
		s.phase = sshEnded
	}
	s.mu.Unlock()
	return err
}

// ending is the line written where the session's output stops: the reason in
// words, so it stays in the scrollback after the status bar moves on.
func (s *sshSession) ending(w *workbench) string {
	s.settle(w)
	s.mu.Lock()
	failure, code := s.failure, s.code
	s.mu.Unlock()
	switch {
	case failure != nil:
		return w.tr("连接已断开 · ", "Disconnected · ") + failure.title
	case code == 0:
		return w.tr("连接已结束", "Session closed")
	default:
		return fmt.Sprintf(w.tr("连接已结束（退出码 %d）", "Session closed (exit code %d)"), code)
	}
}

// settle reads the state of a session once it ended: what ssh printed and
// the code it exited with say why, in words.
func (s *sshSession) settle(w *workbench) {
	s.mu.Lock()
	if s.settled {
		s.mu.Unlock()
		return
	}
	s.settled = true
	tail := ""
	if s.run != nil {
		tail = s.run.Tail()
		s.code = s.run.CodeAfter(2 * time.Second)
	}
	elapsed := time.Since(s.started)
	s.mu.Unlock()

	failure := w.classifySSHFailure(tail, s.code, elapsed)
	s.mu.Lock()
	s.endedAt = time.Now()
	s.failure = failure
	switch {
	case failure != nil:
		s.phase = sshFailed
	case s.code == 0:
		s.phase = sshEnded
	default:
		s.phase = sshEnded
	}
	s.mu.Unlock()
}

// refresh reads what the session's output says about its state, while it is
// still being made: a question from ssh means the user has something to
// answer, any other output that the session is through.
func (s *sshSession) refresh() {
	s.mu.Lock()
	busy := !s.settled && (s.phase == sshConnecting || s.phase == sshPrompting)
	s.mu.Unlock()
	if !busy || s.run == nil {
		return
	}
	tail := strings.ToLower(s.run.Tail())
	if strings.TrimSpace(tail) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sawOutput = true
	// The prompt stays in the tail: a session counts as through once the
	// output grew beyond it, as when the shell it started answers.
	if sshPromptPattern(tail) && (!s.prompted || len(tail) <= s.promptAt+8) {
		s.prompted = true
		s.promptAt = len(tail)
		s.phase = sshPrompting
		return
	}
	s.phase = sshReady
}

// sshPromptPattern matches the questions ssh asks on the terminal: a
// password, a key's passphrase, or an unknown host key.
func sshPromptPattern(lower string) bool {
	if !strings.HasSuffix(strings.TrimRight(lower, " "), ":") && !strings.Contains(lower, "(yes/no") {
		return false
	}
	for _, word := range []string{"password", "passphrase", "yes/no", "verification code", "enter "} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// view is what the tab and the status bar draw: the phase and, for a failed
// session, why.
func (s *sshSession) view() (sshPhase, *sshFailure, int) {
	if s == nil {
		return sshEnded, nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase, s.failure, s.code
}

// dotColor is the tab's status dot: connecting, waiting for the user, live,
// or stopped.
func sshDotColor(p palette, phase sshPhase) ui.Color {
	switch phase {
	case sshReady:
		return p.success
	case sshPrompting:
		return p.warning
	case sshConnecting:
		return p.faint
	case sshFailed:
		return p.danger
	default:
		return p.faint
	}
}

// classifySSHFailure turns what ssh printed, and the code it exited with,
// into a reason a person can act on. It reads only the end of the output,
// which is where ssh says why it stopped.
func (w *workbench) classifySSHFailure(tail string, code int, elapsed time.Duration) *sshFailure {
	lower := strings.ToLower(tail)
	raw := lastSSHLine(tail)
	has := func(words ...string) bool {
		for _, word := range words {
			if strings.Contains(lower, word) {
				return true
			}
		}
		return false
	}
	failure := func(title, detail string) *sshFailure {
		return &sshFailure{title: title, detail: detail, raw: raw}
	}
	switch {
	case has("remote host identification has changed", "host key verification failed", "no matching host key", "host key for"):
		return failure(
			w.tr("主机身份校验失败", "Host identity check failed"),
			w.tr("这台主机的密钥与已知记录不一致，可能是重装了系统。确认无误后请在 ~/.ssh/known_hosts 中删除旧记录再连接。", "This host's key differs from the one on record, as after a reinstall. Check it, then remove the old entry from ~/.ssh/known_hosts."))
	case has("permission denied", "authentication failed", "too many authentication failures", "no more authentication methods"):
		return failure(
			w.tr("认证失败", "Authentication failed"),
			w.tr("服务器拒绝了你的密钥或密码。检查该主机的密钥、ssh-agent，或改用密码登录。", "The server refused your key or password. Check the host's key, your ssh-agent, or sign in with a password."))
	case has("could not resolve hostname", "name or service not known", "nodename nor servname"):
		return failure(
			w.tr("无法解析主机名", "Host name not resolved"),
			w.tr("检查 ~/.ssh/config 中的 HostName，或这台机器的 DNS 与网络。", "Check HostName in ~/.ssh/config, or this machine's DNS and network."))
	case has("connection refused"):
		return failure(
			w.tr("主机拒绝连接", "Connection refused"),
			w.tr("目标机器的 SSH 服务没有在监听该端口；确认 sshd 已启动、端口正确。", "Nothing is listening on that port there; check that sshd runs and the port is right."))
	case has("connection timed out", "operation timed out", "timed out"):
		return failure(
			w.tr("连接超时", "Connection timed out"),
			w.tr("机器不可达。检查网络、防火墙、安全组，以及在系统设置中允许 Vantry 访问本地网络。", "The machine is unreachable. Check the network, firewall, security group, and that Vantry may access the local network."))
	case has("no route to host", "network is unreachable", "host is down"):
		return failure(
			w.tr("网络不可达", "Network unreachable"),
			w.tr("本机到这台机器之间没有可用路由；确认它在同一网络或代理配置正确。", "There is no route to that machine; check that it is on your network or that the proxy is right."))
	case has("kex_exchange_identification", "banner exchange", "protocol version"):
		return failure(
			w.tr("握手失败", "Handshake failed"),
			w.tr("对端不是 SSH 服务，或中间设备中断了连接。确认端口指向 sshd。", "The other end is not an SSH server, or something in between interrupts it. Check that the port is sshd."))
	case has("connection closed by ", "connection reset by peer", "broken pipe", "client_loop", "disconnected from"):
		return failure(
			w.tr("连接被远端断开", "Closed by the remote host"),
			w.tr("服务器主动结束了连接；可能是会话数限制、空闲超时或服务器重启。", "The server ended the session: a session limit, an idle timeout, or a restart."))
	case has("pty allocation request failed", "administratively prohibited"):
		return failure(
			w.tr("服务器拒绝了会话", "The server refused the session"),
			w.tr("服务器的 sshd 配置不允许分配终端，或该账号被限制登录。", "The server's sshd does not allow a terminal, or this account may not sign in."))
	case code == 129:
		return failure(
			w.tr("会话被中断", "The session was interrupted"),
			w.tr("ssh 被挂断信号结束，通常是终端被关闭或系统休眠。重新连接即可。", "ssh was ended by a hangup, as when a terminal closes or the Mac sleeps. Connect again."))
	case code == 255:
		return failure(
			w.tr("连接失败", "Connection failed"),
			w.tr("ssh 未能建立连接。上方保留了它的原始提示。", "ssh could not connect. Its own message is above."))
	case code == 130:
		return failure(
			w.tr("已取消", "Cancelled"),
			w.tr("连接在建立前被中断。", "The connection was interrupted before it was made."))
	case code != 0 && elapsed < 3*time.Second:
		return failure(
			w.tr("连接立即结束", "The session ended at once"),
			w.tr("ssh 启动后马上退出；上方保留了它的原始输出。", "ssh exited right after it started; its own output is above."))
	}
	return nil
}

// lastSSHLine is the last line of ssh's own output, kept for the details of
// a failed connection: it shows the server's words, not Vantry's.
func lastSSHLine(tail string) string {
	lines := strings.Split(strings.TrimSpace(stripANSI(tail)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		return line
	}
	return ""
}

// stripANSI drops the escape sequences of a screen, so that the lines of
// output read as text.
func stripANSI(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b {
			out.WriteByte(text[i])
			continue
		}
		i++
		if i >= len(text) {
			break
		}
		switch text[i] {
		case '[':
			for i++; i < len(text); i++ {
				if text[i] >= 0x40 && text[i] <= 0x7e {
					break
				}
			}
		case ']':
			for i++; i < len(text); i++ {
				if text[i] == 0x07 {
					break
				}
				if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '\\' {
					i++
					break
				}
			}
		}
	}
	return out.String()
}
