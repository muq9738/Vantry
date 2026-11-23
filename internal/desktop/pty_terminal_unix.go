//go:build darwin || linux

package desktop

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
)

// programRun is the terminal.Options.Conn of a program Vantry runs in its
// own pty (see ptyProcess): what the program writes is read here, typed
// keys are written back, and the end of the program is reported once, in
// words, so that the screen keeps both its output and the reason it stopped
// — instead of the terminal library's bare "[Process exited with code N]".
type programRun struct {
	proc  *ptyProcess
	tail  *tailBuffer
	final func(code int) string

	mu      sync.Mutex
	pending []byte
	ended   bool
	closed  chan struct{}
	once    sync.Once
}

func newProgramRun(proc *ptyProcess, final func(code int) string) *programRun {
	return &programRun{proc: proc, tail: newTailBuffer(8 << 10), final: final, closed: make(chan struct{})}
}

// Read waits for the program's output. After the program ends it delivers
// the closing line once and then waits for the terminal to be closed: a
// terminal keeps the screen of the program it ran, as terminal apps do.
func (r *programRun) Read(b []byte) (int, error) {
	for {
		if n := r.takePending(b); n > 0 {
			return n, nil
		}
		n, err := r.proc.Read(b)
		if n > 0 {
			r.tail.Write(b[:n])
			return n, nil
		}
		if err != nil {
			if r.markEnded() {
				continue
			}
			<-r.closed
			return 0, io.EOF
		}
	}
}

// takePending serves the closing line once.
func (r *programRun) takePending(b []byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return 0
	}
	n := copy(b, r.pending)
	r.pending = r.pending[n:]
	return n
}

// markEnded reports whether this call is the one that ended the run, and
// queues the line that explains it.
func (r *programRun) markEnded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return false
	}
	r.ended = true
	if r.final != nil {
		// The end of the output can come before the kernel has the
		// program's status: the closing line needs the code.
		if line := r.final(r.CodeAfter(2 * time.Second)); line != "" {
			r.pending = []byte("\r\n\x1b[2m" + line + "\x1b[0m\r\n")
		}
	}
	return true
}

func (r *programRun) Write(b []byte) (int, error) { return r.proc.Write(b) }

func (r *programRun) Resize(cols, rows int) error { return r.proc.Resize(cols, rows) }

func (r *programRun) ResizePixels(cols, rows, cellWidth, cellHeight int) error {
	return r.proc.ResizePixels(cols, rows, cellWidth, cellHeight)
}

func (r *programRun) Close() error {
	r.once.Do(func() {
		close(r.closed)
		_ = r.proc.Close()
	})
	return nil
}

// Code is the program's exit code, or -1 while it runs.
func (r *programRun) Code() int { return r.proc.Code() }

// CodeAfter waits up to d for the program to end and returns its exit code.
func (r *programRun) CodeAfter(d time.Duration) int { return r.proc.CodeAfter(d) }

// Exited is closed once the program ended.
func (r *programRun) Exited() <-chan struct{} { return r.proc.Exited() }

// Tail is the end of what the program wrote.
func (r *programRun) Tail() string { return r.tail.String() }

// launchTerminal shows argv in an embedded terminal. Tests replace the
// terminal factory, so no process starts; in the app the program runs in a
// pty of Vantry's own and the terminal reads it through a Conn, which is
// what keeps a session alive on macOS (see ptyProcess).
//
// The returned run is nil when the terminal factory is the test's; final,
// when set, says in words why the program ended.
func (w *workbench) launchTerminal(options terminal.Options, argv []string, final func(code int) string) (*terminal.Terminal, *programRun, error) {
	if w.newTerminal != nil {
		options.Command = argv
		term, err := w.newTerminal(options)
		return term, nil, err
	}
	dir := options.Dir
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	var path string
	if len(argv) == 0 {
		path, argv = loginShellCommand()
	}
	extraEnv := append([]string(nil), options.Env...)
	if path != "" {
		extraEnv = append(extraEnv, "SHELL="+path)
	}
	env := terminalEnvironment(extraEnv)
	proc, err := startPtyProcess(ptySpec{path: path, argv: argv, dir: dir, env: env, cols: 80, rows: 24})
	if err != nil {
		return nil, nil, err
	}
	run := newProgramRun(proc, final)
	shown := options
	// Conn ignores the program, the directory and the environment: the pty
	// above has them, and the zsh shell integration of the library only
	// installs for a program it starts itself.
	shown.Command, shown.Dir, shown.Env, shown.ShellIntegration = nil, "", nil, false
	shown.Conn = run
	term, err := terminal.New(shown)
	if err != nil {
		_ = run.Close()
		return nil, nil, err
	}
	return term, run, nil
}

// loginShellCommand returns the user's shell path and POSIX login-shell argv
// (argv[0] prefixed with '-', e.g. path="/bin/zsh", argv=["-zsh"]), matching
// Terminal.app, iTerm2, Ghostty, and MyGo's terminal plugin so zsh, bash,
// fish, and sh all start as interactive login shells without bogus CLI flags.
func loginShellCommand() (string, []string) {
	return resolveLoginShell(os.Getenv("SHELL"), runtime.GOOS, exec.LookPath)
}

func resolveLoginShell(envShell, goos string, lookPath func(string) (string, error)) (string, []string) {
	var candidates []string
	if s := strings.TrimSpace(envShell); s != "" {
		candidates = append(candidates, s)
	}
	if goos == "darwin" {
		candidates = append(candidates, "/bin/zsh", "/bin/bash", "/bin/sh")
	} else {
		candidates = append(candidates, "/bin/bash", "/bin/zsh", "/bin/sh")
	}
	for _, cand := range candidates {
		if p, err := lookPath(cand); err == nil && p != "" {
			return p, []string{"-" + filepath.Base(p)}
		}
	}
	fallback := "/bin/sh"
	if goos == "darwin" {
		fallback = "/bin/zsh"
	}
	return fallback, []string{"-" + filepath.Base(fallback)}
}

// terminalEnvironment is the process environment with what a terminal sets:
// a colour terminal, a UTF-8 locale when the app has none, and extra.
func terminalEnvironment(extra []string) []string {
	env := os.Environ()
	set := func(kv string) {
		k, _, _ := strings.Cut(kv, "=")
		for i, e := range env {
			if strings.HasPrefix(e, k+"=") {
				env[i] = kv
				return
			}
		}
		env = append(env, kv)
	}
	set("TERM=xterm-256color")
	set("COLORTERM=truecolor")
	set("TERM_PROGRAM=Vantry")
	if os.Getenv("LANG") == "" && os.Getenv("LC_ALL") == "" && os.Getenv("LC_CTYPE") == "" {
		set("LANG=en_US.UTF-8")
	}
	for _, kv := range extra {
		set(kv)
	}
	return env
}
