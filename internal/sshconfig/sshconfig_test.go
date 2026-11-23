package sshconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录 %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s: %v", path, err)
	}
}

func findHost(t *testing.T, hosts []api.SSHHostInfo, alias string) api.SSHHostInfo {
	t.Helper()
	for _, host := range hosts {
		if host.Alias == alias {
			return host
		}
	}
	t.Fatalf("未找到主机 %q，实际为 %+v", alias, hosts)
	return api.SSHHostInfo{}
}

func TestDiscoverIncludesMultiAliasAndDirectives(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".ssh", "config"), `
Include config.d/*.conf
Include ~/.orbstack/ssh/config

Host *
    ServerAliveInterval 30
    ForwardAgent yes

Host devbox devbox-win windows-dev shanghai
    HostName 10.0.0.5
    Port 2222
    User root
    ProxyJump bastion
    ForwardAgent yes
    IdentityFile ~/.ssh/id_ed25519

Host tokyo-a tokyo
    Port 22
`)
	writeConfig(t, filepath.Join(home, ".ssh", "config.d", "extra.conf"), `
Host staging
    Port 2200
Include config.d/nested/*.conf
`)
	writeConfig(t, filepath.Join(home, ".ssh", "config.d", "nested", "inner.conf"), `
Host nested-a nested-b
    ForwardAgent no
`)
	writeConfig(t, filepath.Join(home, ".orbstack", "ssh", "config"), `
Host orbstack-test
    Port 2223
`)

	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hosts) != 5 {
		t.Fatalf("主机数量 = %d，期望 5：%+v", len(hosts), hosts)
	}

	staging := findHost(t, hosts, "staging")
	if staging.Port != 2200 || staging.SourceFile != "~/.ssh/config.d/extra.conf" {
		t.Fatalf("staging = %+v", staging)
	}
	nested := findHost(t, hosts, "nested-a")
	if got, want := strings.Join(nested.Aliases, ","), "nested-a,nested-b"; got != want {
		t.Fatalf("nested Aliases = %q，期望 %q", got, want)
	}
	orbstack := findHost(t, hosts, "orbstack-test")
	if orbstack.Port != 2223 || orbstack.SourceFile != "~/.orbstack/ssh/config" {
		t.Fatalf("orbstack = %+v", orbstack)
	}

	devbox := findHost(t, hosts, "devbox")
	if got, want := strings.Join(devbox.Aliases, ","), "devbox,devbox-win,windows-dev,shanghai"; got != want {
		t.Fatalf("devbox Aliases = %q，期望 %q", got, want)
	}
	if devbox.Port != 2222 || devbox.ProxyJump != "bastion" || !devbox.ForwardAgent || !devbox.HasIdentityFile {
		t.Fatalf("devbox = %+v", devbox)
	}
	if devbox.SourceFile != "~/.ssh/config" {
		t.Fatalf("devbox SourceFile = %q", devbox.SourceFile)
	}
	if devbox.Reachable != nil || devbox.LatencyMs != 0 || devbox.RemoteSocket != "" {
		t.Fatalf("Discover 不应探测主机: %+v", devbox)
	}

	// Port 22 is the default and stays unset; Host * defaults must not leak.
	tokyo := findHost(t, hosts, "tokyo-a")
	if tokyo.Port != 0 || tokyo.ForwardAgent {
		t.Fatalf("tokyo-a = %+v", tokyo)
	}
	if got, want := strings.Join(tokyo.Aliases, ","), "tokyo-a,tokyo"; got != want {
		t.Fatalf("tokyo-a Aliases = %q，期望 %q", got, want)
	}
}

func TestDiscoverKeyValueQuotesAndComments(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".ssh", "config"), `
# 整行注释
Host=eqhost
    Port=2201
    ProxyJump="jump.example.com"
    IdentityFile = "~/.ssh/id work"   # 行内注释
    ForwardAgent=TRUE

Host web
    Port 22
`)
	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("主机数量 = %d：%+v", len(hosts), hosts)
	}
	eq := findHost(t, hosts, "eqhost")
	if eq.Port != 2201 || eq.ProxyJump != "jump.example.com" || !eq.ForwardAgent || !eq.HasIdentityFile {
		t.Fatalf("eqhost = %+v", eq)
	}
	web := findHost(t, hosts, "web")
	if web.Port != 0 {
		t.Fatalf("web Port = %d，默认端口应为 0", web.Port)
	}
}

func TestDiscoverSkipsWildcardsMatchAndGitForges(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".ssh", "config"), `
Host *
    Port 9999
    ForwardAgent yes

Host github.com
    Port 1234

Host gitlab.com bitbucket.org
    Port 1235

Host *.corp
    Port 1236

Host qmt
    Port 2202
Match host *.internal
    ProxyJump hidden-jump
    ForwardAgent yes
    IdentityFile ~/.ssh/internal

Host web*.example.com good-host
    Port 2203
`)
	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("主机数量 = %d，期望 2：%+v", len(hosts), hosts)
	}
	qmt := findHost(t, hosts, "qmt")
	if qmt.Port != 2202 || qmt.ForwardAgent || qmt.HasIdentityFile || qmt.ProxyJump != "" {
		t.Fatalf("Match 块污染了 qmt: %+v", qmt)
	}
	good := findHost(t, hosts, "good-host")
	if good.Port != 2203 {
		t.Fatalf("good-host = %+v", good)
	}

	withForges, err := DiscoverWith(home, Options{IncludeGitForges: true})
	if err != nil {
		t.Fatalf("DiscoverWith: %v", err)
	}
	if len(withForges) != 4 {
		t.Fatalf("包含 Git 托管时主机数量 = %d：%+v", len(withForges), withForges)
	}
	if github := findHost(t, withForges, "github.com"); github.Port != 1234 {
		t.Fatalf("github.com = %+v", github)
	}
}

func TestDiscoverQuotedAndMultipleIncludePatterns(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".ssh", "config"), `
Include "config.d/*.conf" ~/.other/ssh/config
Host root
`)
	writeConfig(t, filepath.Join(home, ".ssh", "config.d", "one.conf"), "Host included-one\n")
	writeConfig(t, filepath.Join(home, ".other", "ssh", "config"), "Host included-two\n")
	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hosts) != 3 {
		t.Fatalf("主机数量 = %d：%+v", len(hosts), hosts)
	}
	findHost(t, hosts, "included-one")
	two := findHost(t, hosts, "included-two")
	if two.SourceFile != "~/.other/ssh/config" {
		t.Fatalf("included-two SourceFile = %q", two.SourceFile)
	}
	findHost(t, hosts, "root")
}

func TestDiscoverMissingConfigIsEmpty(t *testing.T) {
	home := t.TempDir()
	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if hosts == nil || len(hosts) != 0 {
		t.Fatalf("hosts = %#v，期望空切片", hosts)
	}
}

func TestDiscoverIncludeCycleAndMissingInclude(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".ssh", "config"), `
Include config
Include missing.conf
Include config.d/cycle.conf
Host root
    Port 2204
`)
	writeConfig(t, filepath.Join(home, ".ssh", "config.d", "cycle.conf"), `
Include ../config
Host cycle
`)
	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("主机数量 = %d，期望 2：%+v", len(hosts), hosts)
	}
	findHost(t, hosts, "root")
	findHost(t, hosts, "cycle")
}

func TestDiscoverIncludeDepthLimit(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".ssh", "config"), "Include chain/f1.conf\n")
	dir := filepath.Join(home, ".ssh", "chain")
	for i := 1; i <= 9; i++ {
		next := ""
		if i < 9 {
			next = "Include chain/f" + strconv.Itoa(i+1) + ".conf\n"
		}
		writeConfig(t, filepath.Join(dir, "f"+strconv.Itoa(i)+".conf"),
			"Host h"+strconv.Itoa(i)+"\n"+next)
	}
	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hosts) != maxIncludeDepth {
		t.Fatalf("主机数量 = %d，期望 %d：%+v", len(hosts), maxIncludeDepth, hosts)
	}
	findHost(t, hosts, "h"+strconv.Itoa(maxIncludeDepth))
	for _, tooDeep := range []string{"h7", "h8", "h9"} {
		for _, host := range hosts {
			if host.Alias == tooDeep {
				t.Fatalf("超过深度限制的文件仍被包含: %s", tooDeep)
			}
		}
	}
}

func TestDiscoverDeduplicatesPrimaryAlias(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, ".ssh", "config"), `
Host dup dup-other
    Port 2205
Host dup
    Port 2206
Host another
    Port 2207
`)
	hosts, err := Discover(home)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("主机数量 = %d：%+v", len(hosts), hosts)
	}
	if hosts[0].Alias != "dup" || hosts[0].Port != 2205 {
		t.Fatalf("hosts[0] = %+v", hosts[0])
	}
	if hosts[1].Alias != "another" || hosts[1].Port != 2207 {
		t.Fatalf("hosts[1] = %+v", hosts[1])
	}
}

func TestValidateAlias(t *testing.T) {
	valid := []string{"qmt", "qmt-window", "user@host", "host.example.com", "host_1", "192.168.1.10"}
	for _, alias := range valid {
		if err := ValidateAlias(alias); err != nil {
			t.Fatalf("ValidateAlias(%q) = %v，期望 nil", alias, err)
		}
	}
	invalid := []string{"", "-oProxyCommand=touch /tmp/pwned", "-host", "host name", "host;rm -rf /", "host$(id)", "主机", strings.Repeat("a", 256)}
	for _, alias := range invalid {
		if err := ValidateAlias(alias); err == nil {
			t.Fatalf("ValidateAlias(%q) = nil，期望错误", alias)
		}
	}
}

func fakeSSH(t *testing.T, script string) (binaryPath, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	binaryPath = filepath.Join(dir, "ssh")
	argvLog = filepath.Join(dir, "argv.log")
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatalf("写入假 ssh: %v", err)
	}
	return binaryPath, argvLog
}

func TestProbeWithMockSSH(t *testing.T) {
	binary, argvLog := fakeSSH(t, `#!/bin/sh
printf '%s\n' "$@" > "$VANTRY_FAKE_SSH_LOG"
printf 'vantry-socket /home/remote/.config/Vantry/core.sock\n'
printf 'vantry-herdr\n'
exit 0
`)
	t.Setenv("VANTRY_FAKE_SSH_LOG", argvLog)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := ProbeWith(ctx, ProbeOptions{Alias: "qmt", SSHBinary: binary})
	if err != nil {
		t.Fatalf("ProbeWith: %v", err)
	}
	if info.Alias != "qmt" || info.Reachable == nil || !*info.Reachable {
		t.Fatalf("info = %+v", info)
	}
	if info.RemoteSocket != "/home/remote/.config/Vantry/core.sock" || !info.HasHerdr {
		t.Fatalf("info = %+v", info)
	}
	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("读取参数记录: %v", err)
	}
	args := string(logged)
	if !strings.Contains(args, "\nsh\n-lc\n'") {
		t.Fatalf("ssh 远程命令未作为单个引用脚本传递：\n%s", args)
	}
	for _, want := range []string{
		"BatchMode=yes",
		"StrictHostKeyChecking=accept-new",
		"ConnectTimeout=4",
		"qmt",
		"sh",
		"-lc",
		"test -S",
		"$HOME/.config/Vantry/core.sock",
		"$HOME/Library/Application Support/Vantry/core.sock",
		"${XDG_CONFIG_HOME:-$HOME/.config}/Vantry/core.sock",
		"command -v herdr",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("ssh 参数缺少 %q：\n%s", want, args)
		}
	}
}

func TestProbeUnreachableIsNotAnError(t *testing.T) {
	binary, _ := fakeSSH(t, "#!/bin/sh\nexit 255\n")
	info, err := ProbeWith(context.Background(), ProbeOptions{Alias: "offline", SSHBinary: binary, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("ProbeWith: %v", err)
	}
	if info.Reachable == nil || *info.Reachable {
		t.Fatalf("Reachable = %v，期望 false", info.Reachable)
	}
	if info.HasHerdr || info.RemoteSocket != "" {
		t.Fatalf("info = %+v", info)
	}
}

func TestProbeFailureGivesActionableCauseWithoutPrivateStderr(t *testing.T) {
	binary, _ := fakeSSH(t, "#!/bin/sh\necho 'ssh: connect to host private-name: No route to host; private credential details' >&2\nexit 255\n")
	info, err := ProbeWith(context.Background(), ProbeOptions{Alias: "lan-host", SSHBinary: binary})
	if err != nil || info.Reachable == nil || *info.Reachable {
		t.Fatalf("unexpected probe result: %+v %v", info, err)
	}
	if !strings.Contains(info.ProbeError, "本地网络权限") || strings.Contains(info.ProbeError, "private") {
		t.Fatalf("missing recovery action or stderr leaked: %q", info.ProbeError)
	}
}

func TestProbeRejectsMaliciousAliasWithoutRunningSSH(t *testing.T) {
	binary, argvLog := fakeSSH(t, "#!/bin/sh\ntouch \"$VANTRY_FAKE_SSH_LOG.marker\"\nexit 0\n")
	t.Setenv("VANTRY_FAKE_SSH_LOG", argvLog)
	info, err := ProbeWith(context.Background(), ProbeOptions{
		Alias:     "-oProxyCommand=touch /tmp/pwned",
		SSHBinary: binary,
	})
	if err == nil {
		t.Fatal("恶意别名应被拒绝")
	}
	if info.Reachable != nil {
		t.Fatalf("info = %+v，拒绝时不应探测", info)
	}
	if _, statErr := os.Stat(argvLog + ".marker"); statErr == nil {
		t.Fatal("恶意别名触发了 ssh 执行")
	}
}

func TestProbeMissingSSHBinary(t *testing.T) {
	_, err := ProbeWith(context.Background(), ProbeOptions{
		Alias:     "qmt",
		SSHBinary: filepath.Join(t.TempDir(), "missing-ssh"),
	})
	if err == nil {
		t.Fatal("缺少 ssh 时应返回错误")
	}
}

func TestProbeHonorsContextCancellation(t *testing.T) {
	binary, _ := fakeSSH(t, "#!/bin/sh\nsleep 5\nexit 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	info, err := ProbeWith(ctx, ProbeOptions{Alias: "slow", SSHBinary: binary})
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望超时", err)
	}
	if info.Reachable == nil || *info.Reachable {
		t.Fatalf("info = %+v", info)
	}
}
