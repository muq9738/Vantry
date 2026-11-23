package desktop

import (
	"strings"
	"testing"
	"time"
)

// TestClassifySSHFailure reads the reasons ssh prints as a person's words, so
// that a session that cannot connect says why instead of a blank screen.
func TestClassifySSHFailure(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	cases := []struct {
		name    string
		tail    string
		code    int
		elapsed time.Duration
		want    string
	}{
		{"auth", "root@198.51.100.10: Permission denied (publickey,password).\r\n", 255, 800 * time.Millisecond, "认证失败"},
		{"hostkey", "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\r\nIt is possible that someone is doing something nasty!\r\nHost key verification failed.\r\n", 255, 900 * time.Millisecond, "主机身份校验失败"},
		{"changed", "WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!", 255, time.Second, "主机身份校验失败"},
		{"resolve", "ssh: Could not resolve hostname nosuchhost: nodename nor servname provided, or not known", 255, 300 * time.Millisecond, "无法解析主机名"},
		{"refused", "ssh: connect to host 127.0.0.1 port 22: Connection refused", 255, 200 * time.Millisecond, "主机拒绝连接"},
		{"timeout", "ssh: connect to host 10.0.0.9 port 22: Operation timed out", 255, 10 * time.Second, "连接超时"},
		{"route", "ssh: connect to host 10.0.0.9 port 22: No route to host", 255, 2 * time.Second, "网络不可达"},
		{"handshake", "kex_exchange_identification: Connection closed by remote host", 255, time.Second, "握手失败"},
		{"closed", "Connection closed by 198.51.100.10 port 22", 255, 4 * time.Second, "连接被远端断开"},
		{"hangup", "", 129, 400 * time.Millisecond, "会话被中断"},
		{"generic", "", 255, 5 * time.Second, "连接失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := w.classifySSHFailure(tc.tail, tc.code, tc.elapsed)
			if failure == nil {
				t.Fatalf("%q with code %d: no reason", tc.tail, tc.code)
			}
			if failure.title != tc.want {
				t.Fatalf("title = %q, want %q", failure.title, tc.want)
			}
			if failure.detail == "" {
				t.Fatal("a failure must say what to do")
			}
			if tc.tail != "" && failure.raw == "" {
				t.Fatalf("%q: the server's own line was dropped", tc.tail)
			}
		})
	}
	// A session that ends by itself needs no reason.
	if failure := w.classifySSHFailure("logout\r\nConnection to host closed.\r\n", 0, time.Minute); failure != nil {
		t.Fatalf("a normal end reported %q", failure.title)
	}
}

// TestSSHCommandLine keeps the command a user copies exact, and quoted only
// where a shell would need it.
func TestSSHCommandLine(t *testing.T) {
	entry := sshEntry{name: "devbox", target: "devbox", key: "devbox"}
	if got := sshCommandLine(entry.sshArgv()); got != "ssh -o ConnectTimeout=10 -- devbox" {
		t.Fatalf("command = %q", got)
	}
	if got := sshCommandLine([]string{"ssh", "root@host name"}); got != "ssh 'root@host name'" {
		t.Fatalf("quoted command = %q", got)
	}
}

// TestSSHPromptPattern tells a question from output: a password or passphrase
// prompt, or an unknown host key, waits for the user.
func TestSSHPromptPattern(t *testing.T) {
	for _, prompt := range []string{
		"root@198.51.100.10's password:",
		"enter passphrase for key '/users/me/.ssh/id_ed25519':",
		"are you sure you want to continue connecting (yes/no)? ",
		"are you sure you want to continue connecting (yes/no/[fingerprint])?",
	} {
		if !sshPromptPattern(strings.ToLower(prompt)) {
			t.Fatalf("%q is a question", prompt)
		}
	}
	for _, output := range []string{
		"welcome to ubuntu 24.04 lts (gnu/linux 6.8.0) \\n \\l",
		"root@devbox:~# ",
		"",
	} {
		if sshPromptPattern(strings.ToLower(output)) {
			t.Fatalf("%q is not a question", output)
		}
	}
}
