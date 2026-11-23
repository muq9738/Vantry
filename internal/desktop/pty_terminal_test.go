//go:build darwin || linux

package desktop

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
)

// TestPtyReadsWaitForOutput is the regression test of the SSH terminal that
// stopped at once with "[Process exited with code 129]": a pty master opened
// through Go's own file API answers EAGAIN instead of waiting, and a terminal
// that takes that for the end of its program hangs the program up. Vantry's
// pty blocks until output is there, so a program that speaks after a while is
// heard, and nothing is lost.
func TestPtyReadsWaitForOutput(t *testing.T) {
	proc, err := startPtyProcess(ptySpec{
		argv: []string{"/bin/sh", "-c", "sleep 0.3; echo later; exit 3"},
		env:  []string{"PATH=/usr/bin:/bin"},
		cols: 80, rows: 24,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer proc.Close()
	started := time.Now()
	var out []byte
	buf := make([]byte, 256)
	for {
		n, err := proc.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if err != io.EOF {
				t.Fatalf("read: %v", err)
			}
			break
		}
		if time.Since(started) > 5*time.Second {
			t.Fatal("the program's output never came")
		}
	}
	if got := string(out); !strings.Contains(got, "later") {
		t.Fatalf("output = %q, want the program's line", got)
	}
	if waited := time.Since(started); waited < 250*time.Millisecond {
		t.Fatalf("read returned after %v: it did not wait for the output", waited)
	}
	if code := proc.Wait(); code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

// TestPtyResizeReachesTheProgram checks that a resize is told to the program,
// which is how the remote pty learns the size of the window.
func TestPtyResizeReachesTheProgram(t *testing.T) {
	proc, err := startPtyProcess(ptySpec{
		argv: []string{"/bin/sh", "-c", "sleep 0.2; stty size; exit 0"},
		env:  []string{"PATH=/usr/bin:/bin"},
		cols: 40, rows: 12,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer proc.Close()
	if err := proc.Resize(97, 31); err != nil {
		t.Fatalf("resize: %v", err)
	}
	text := readAll(t, proc)
	if !strings.Contains(text, "31 97") {
		t.Fatalf("stty size = %q, want 31 97", text)
	}
}

// TestProgramRunKeepsTheScreen checks what a terminal shows when its program
// ends: the last line says why, in words, and the screen stays there instead
// of the library's bare exit line.
func TestProgramRunKeepsTheScreen(t *testing.T) {
	proc, err := startPtyProcess(ptySpec{
		argv: []string{"/bin/sh", "-c", "echo bye; exit 2"},
		env:  []string{"PATH=/usr/bin:/bin"},
		cols: 80, rows: 24,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run := newProgramRun(proc, func(code int) string {
		if code != 2 {
			return ""
		}
		return "程序以代码 2 退出"
	})
	text := readUntil(t, run, "退出")
	if !strings.Contains(text, "bye") || !strings.Contains(text, "程序以代码 2 退出") {
		t.Fatalf("screen = %q, want the output and the closing line", text)
	}
	if run.Code() != 2 {
		t.Fatalf("code = %d, want 2", run.Code())
	}
	// After its closing line the terminal waits for the tab to close: a
	// terminal keeps the screen of the program it ran.
	done := make(chan error, 1)
	go func() {
		_, err := run.Read(make([]byte, 16))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("read after the closing line returned %v, want it to wait", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("read after close = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing the terminal did not end its read")
	}
}

// TestProgramRunSaysNothingOfASuccessfulProgram keeps a plain exit quiet: a
// shell closing normally needs no explanation.
func TestProgramRunSaysNothingOfASuccessfulProgram(t *testing.T) {
	proc, err := startPtyProcess(ptySpec{
		argv: []string{"/bin/sh", "-c", "echo done"},
		env:  []string{"PATH=/usr/bin:/bin"},
		cols: 80, rows: 24,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run := newProgramRun(proc, func(code int) string {
		if code == 0 {
			return "进程已退出"
		}
		return ""
	})
	text := readUntil(t, run, "退出")
	if !strings.Contains(text, "done") || !strings.Contains(text, "进程已退出") {
		t.Fatalf("screen = %q", text)
	}
	_ = run.Close()
}

func readAll(t *testing.T, proc *ptyProcess) string {
	t.Helper()
	var out []byte
	buf := make([]byte, 1024)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := proc.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(out)
}

// readUntil reads a terminal's screen until it holds marker: the closing
// line of a program comes before the read that waits for the tab to close.
func readUntil(t *testing.T, run *programRun, marker string) string {
	t.Helper()
	var mu sync.Mutex
	var out []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1024)
		for {
			n, err := run.Read(buf)
			if n > 0 {
				mu.Lock()
				out = append(out, buf[:n]...)
				found := strings.Contains(string(out), marker)
				mu.Unlock()
				if found {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	return string(out)
}

// TestTerminalShowsProgramOutput is the end-to-end test of the reported bug:
// opening an SSH terminal showed one line, "[Process exited with code 129]",
// and nothing else. The terminal now reads a pty of Vantry's own, so the
// program's output is on the screen and its end says so in words.
func TestTerminalShowsProgramOutput(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	w.newTerminal = nil // the app's own terminals, not a pipe
	final := func(code int) string { return "进程已退出" }
	term, run, err := w.launchTerminal(terminal.Options{NoBlink: true}, []string{"/bin/sh", "-c", "printf 'hello from the program\\n'; exit 0"}, final)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if run == nil {
		t.Fatal("the app must run the program in its own pty and track it")
	}
	defer term.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		text := term.Text()
		if strings.Contains(text, "hello from the program") && strings.Contains(text, "进程已退出") {
			if strings.Contains(text, "Process exited") || strings.Contains(text, "129") {
				t.Fatalf("the screen kept the library's exit line: %q", text)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("screen = %q, want the program's output and its end", term.Text())
}

// TestResolveLoginShellHandlesZshBashFishAndFallbacks ensures login shell
// resolution sets argv[0] to "-<shell>" (never passing "-zsh" as argv[1]) and
// falls back cleanly across macOS and Linux when $SHELL is empty or missing.
func TestResolveLoginShellHandlesZshBashFishAndFallbacks(t *testing.T) {
	present := func(available ...string) func(string) (string, error) {
		set := map[string]bool{}
		for _, p := range available {
			set[p] = true
		}
		return func(name string) (string, error) {
			if set[name] {
				return name, nil
			}
			return "", errors.New("not found")
		}
	}
	cases := []struct {
		name      string
		envShell  string
		goos      string
		available []string
		wantPath  string
		wantArg0  string
	}{
		{"macos zsh", "/bin/zsh", "darwin", []string{"/bin/zsh", "/bin/bash", "/bin/sh"}, "/bin/zsh", "-zsh"},
		{"linux bash", "/bin/bash", "linux", []string{"/bin/bash", "/bin/sh"}, "/bin/bash", "-bash"},
		{"homebrew fish", "/opt/homebrew/bin/fish", "darwin", []string{"/opt/homebrew/bin/fish", "/bin/zsh"}, "/opt/homebrew/bin/fish", "-fish"},
		{"missing SHELL macos", "", "darwin", []string{"/bin/zsh", "/bin/sh"}, "/bin/zsh", "-zsh"},
		{"missing SHELL linux", "", "linux", []string{"/bin/bash", "/bin/sh"}, "/bin/bash", "-bash"},
		{"invalid SHELL falls back", "/nonexistent/shell", "linux", []string{"/bin/sh"}, "/bin/sh", "-sh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, argv := resolveLoginShell(tc.envShell, tc.goos, present(tc.available...))
			if path != tc.wantPath || len(argv) != 1 || argv[0] != tc.wantArg0 {
				t.Fatalf("resolveLoginShell(%q, %q) = (%q, %q), want (%q, [%q])", tc.envShell, tc.goos, path, argv, tc.wantPath, tc.wantArg0)
			}
		})
	}
}

// TestLoginShellStartsInteractivelyWithoutBadOption is the regression test for
// "/bin/zsh: bad option: -z": both zsh and bash (when installed) must start as
// real interactive login shells, execute commands, and exit 0.
func TestLoginShellStartsInteractivelyWithoutBadOption(t *testing.T) {
	home := t.TempDir()
	for _, shell := range []string{"/bin/zsh", "/bin/bash"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			path, argv := resolveLoginShell(shell, "darwin", exec.LookPath)
			proc, err := startPtyProcess(ptySpec{
				path: path,
				argv: argv,
				dir:  home,
				env:  []string{"HOME=" + home, "ZDOTDIR=" + home, "PATH=/usr/bin:/bin", "TERM=xterm-256color", "SHELL=" + path},
				cols: 80, rows: 24,
			})
			if err != nil {
				t.Fatalf("start %s: %v", shell, err)
			}
			defer proc.Close()
			if _, err := proc.Write([]byte("printf 'VANTRY_SHELL_OK\\n'; exit 0\n")); err != nil {
				t.Fatalf("write: %v", err)
			}
			out := readAll(t, proc)
			if strings.Contains(out, "bad option") || strings.Contains(out, "invalid option") {
				t.Fatalf("%s reported option error: %q", shell, out)
			}
			if !strings.Contains(out, "VANTRY_SHELL_OK") {
				t.Fatalf("%s did not execute interactive command: %q", shell, out)
			}
			if code := proc.CodeAfter(3 * time.Second); code != 0 {
				t.Fatalf("%s exit code = %d, want 0 (output=%q)", shell, code, out)
			}
		})
	}
}
