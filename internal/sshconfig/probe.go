package sshconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	defaultConnectTimeout = 4 * time.Second
	defaultProbeTimeout   = 15 * time.Second
	maxProbeOutput        = 64 << 10
	maxAliasLength        = 255
)

// Probe markers keep a login shell's own output from being mistaken for a
// socket or an installed helper.
const (
	socketMarker = "vantry-socket "
	herdrMarker  = "vantry-herdr"
	osMarker     = "vantry-os "
)

// probeScript is one remote shell script: it tests the candidate Core
// sockets, reports whether herdr is installed, and names the remote system.
// It reads no secret and prints only what the probe needs.
const probeScript = `for p in "$HOME/.config/Vantry/core.sock" "$HOME/Library/Application Support/Vantry/core.sock" "${XDG_CONFIG_HOME:-$HOME/.config}/Vantry/core.sock"; do if test -S "$p"; then printf 'vantry-socket %s\n' "$p"; fi; done; if command -v herdr >/dev/null 2>&1; then printf 'vantry-herdr\n'; fi; os=$(uname -srm 2>/dev/null || uname -s 2>/dev/null || true); if test -n "$os"; then printf 'vantry-os %s\n' "$os"; fi; exit 0`

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

// ProbeOptions names the host to probe and allows the binary and timeouts to
// be replaced, which tests use. The zero options except Alias probe with ssh.
type ProbeOptions struct {
	Alias string
	// Host, User, Port and IdentityFile probe a machine that is not in the
	// SSH configuration, as the add-host form does; Alias alone probes an
	// alias.
	Host           string
	User           string
	Port           int
	IdentityFile   string
	SSHBinary      string
	ConnectTimeout time.Duration
	Timeout        time.Duration
}

// Probe asks alias, non-interactively, whether it is reachable and whether
// it runs a Core socket or herdr. It never prompts for a password and never
// exposes the host's stderr.
func Probe(ctx context.Context, alias string) (api.SSHHostInfo, error) {
	return ProbeWith(ctx, ProbeOptions{Alias: alias})
}

// ProbeWith is Probe with ProbeOptions.
func ProbeWith(ctx context.Context, opts ProbeOptions) (api.SSHHostInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	info := api.SSHHostInfo{Alias: opts.Alias, HostName: strings.TrimSpace(opts.Host), User: strings.TrimSpace(opts.User), Port: opts.Port}
	target, err := probeTarget(opts)
	if err != nil {
		return info, err
	}
	if info.Alias == "" {
		info.Alias = target
	}
	if _, ok := ctx.Deadline(); !ok {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = defaultProbeTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	binary := strings.TrimSpace(opts.SSHBinary)
	if binary == "" {
		binary = "ssh"
	}
	connect := opts.ConnectTimeout
	if connect <= 0 {
		connect = defaultConnectTimeout
	}
	seconds := int(connect / time.Second)
	if connect%time.Second != 0 || seconds < 1 {
		seconds = 1
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=" + strconv.Itoa(seconds),
	}
	if opts.Port > 0 {
		args = append(args, "-p", strconv.Itoa(opts.Port))
	}
	if identity := strings.TrimSpace(opts.IdentityFile); identity != "" {
		args = append(args, "-i", identity, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, "--", target, "sh", "-lc", posixQuote(probeScript))
	command := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr cappedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.WaitDelay = time.Second
	started := time.Now()
	err = command.Run()
	info.LatencyMs = int(time.Since(started).Milliseconds())
	reachable := err == nil
	info.Reachable = &reachable
	if err == nil {
		readProbeOutput(&info, stdout.String())
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return info, ctxErr
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			info.ProbeFailure, info.ProbeError = probeFailure(stderr.String())
			// A host that refuses or times out is a result, not a failure
			// of the caller.
			return info, nil
		}
		return info, fmt.Errorf("无法执行 ssh: %w", err)
	}
	return info, nil
}

// probeFailure reports a useful cause without reflecting SSH stderr, which
// may contain private paths or command output from ProxyCommand helpers. The
// kind is what the interface shows beside the host: an auth failure looks
// different from an unreachable machine.
func probeFailure(stderr string) (kind, message string) {
	lower := strings.ToLower(stderr)
	has := func(words ...string) bool {
		for _, word := range words {
			if strings.Contains(lower, word) {
				return true
			}
		}
		return false
	}
	switch {
	case has("permission denied", "authentication failed", "too many authentication failures", "no more authentication methods"):
		return "auth", "SSH 认证失败，请检查该主机的密钥或 SSH Agent。"
	case has("host key verification failed", "remote host identification has changed", "no matching host key"):
		return "hostkey", "SSH 主机身份校验失败，请在终端确认主机配置。"
	case has("could not resolve hostname", "name or service not known", "nodename nor servname"):
		return "dns", "无法解析主机，请检查 SSH 别名和网络。"
	case has("no route to host", "network is unreachable", "operation not permitted"):
		return "route", "网络访问受阻，请检查网络及 macOS 设置中的 Vantry 本地网络权限。"
	case has("connection timed out", "operation timed out"):
		return "timeout", "SSH 连接超时，请检查目标机器、网络及 Vantry 本地网络权限。"
	case has("connection refused"):
		return "refused", "目标机器拒绝 SSH 连接，请检查远程登录是否开启。"
	case has("kex_exchange_identification", "banner exchange", "protocol version"):
		return "handshake", "这个端口上不是 SSH 服务，或中间有设备中断了连接。"
	case has("connection closed by", "connection reset by peer", "disconnected from"):
		return "closed", "远端结束了连接，请确认端口与服务器状态。"
	default:
		return "other", "SSH 连接未成功，请在终端检查该主机连接。"
	}
}

// probeTarget is what ssh connects to: the alias, or user@host of a machine
// that is not in the configuration.
func probeTarget(opts ProbeOptions) (string, error) {
	host := strings.TrimSpace(opts.Host)
	if host == "" {
		if err := ValidateAlias(opts.Alias); err != nil {
			return "", err
		}
		return opts.Alias, nil
	}
	if strings.ContainsAny(host, " \t'\"`$&|;<>()*?[]#~") || strings.HasPrefix(host, "-") {
		return "", errors.New("主机名只能包含字母、数字和 . _ : -")
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return "", errors.New("端口须在 1 到 65535 之间")
	}
	user := strings.TrimSpace(opts.User)
	if strings.ContainsAny(user, " \t'\"`@$&|;<>()*?#~") || strings.HasPrefix(user, "-") {
		return "", errors.New("用户名只能包含字母、数字和 . _ -")
	}
	if user == "" {
		return host, nil
	}
	return user + "@" + host, nil
}

// ValidateAlias accepts an OpenSSH Host alias but no option-shaped string:
// a leading "-" or a shell metacharacter is rejected before ssh runs.
func ValidateAlias(alias string) error {
	if alias == "" {
		return errors.New("SSH 别名不能为空")
	}
	if len(alias) > maxAliasLength {
		return errors.New("SSH 别名过长")
	}
	if strings.HasPrefix(alias, "-") {
		return fmt.Errorf("SSH 别名不能以 “-” 开头: %q", alias)
	}
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("SSH 别名只能包含字母、数字、点、下划线、@ 和连字符: %q", alias)
	}
	return nil
}

// posixQuote wraps a remote script so ssh, which joins the command
// arguments with spaces, hands one single-quoted word to the remote shell.
func posixQuote(script string) string {
	return "'" + strings.ReplaceAll(script, "'", `'\''`) + "'"
}

func readProbeOutput(info *api.SSHHostInfo, output string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, socketMarker):
			socket := strings.TrimSpace(strings.TrimPrefix(line, socketMarker))
			if socket != "" && info.RemoteSocket == "" {
				info.RemoteSocket = socket
			}
		case strings.HasPrefix(line, osMarker):
			if name := strings.TrimSpace(strings.TrimPrefix(line, osMarker)); name != "" && info.RemoteOS == "" {
				info.RemoteOS = name
			}
		case line == herdrMarker:
			info.HasHerdr = true
		}
	}
}

// cappedBuffer keeps the last piece of a command's output, so a chatty
// login shell cannot grow memory without bound.
type cappedBuffer struct {
	buf bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := maxProbeOutput - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return n, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
