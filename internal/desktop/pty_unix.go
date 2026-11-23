//go:build darwin || linux

package desktop

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ptyProcess runs one program in a pseudo-terminal that Vantry creates
// itself, instead of letting the terminal library create it.
//
// The reason is a runtime detail of the terminal library: it opens the
// master with os.OpenFile, Go puts that descriptor in non-blocking mode for
// its poller, and reading a pty master that way returns EAGAIN on macOS
// instead of waiting for the program. The library treats the first read
// error as the end of the program: it closes the master, which hangs the
// program up (SIGHUP, exit 129) before a byte of its output is shown — the
// screen keeps the one line "[Process exited with code 129]". Opening the
// master and the slave with syscall.Open leaves both in blocking mode, so a
// read waits for output, a session lives until its program really ends, and
// nothing is dropped from the screen.
type ptyProcess struct {
	master *os.File
	cmd    *exec.Cmd
	done   chan struct{}
	code   int
	once   sync.Once
}

// ptySpec is the program to run and where. When path is set, path is executed
// with argv as its full argv slice (so argv[0] may be "-zsh" / "-bash" for a
// POSIX login shell without passing it as a CLI flag). When path is empty,
// argv[0] is resolved through exec.LookPath.
type ptySpec struct {
	path       string
	argv       []string
	dir        string
	env        []string
	cols, rows int
}

// startPtyProcess runs spec in a new pseudo-terminal.
func startPtyProcess(spec ptySpec) (*ptyProcess, error) {
	program := spec.path
	if program == "" {
		if len(spec.argv) == 0 {
			return nil, errors.New("pty: no program")
		}
		program = spec.argv[0]
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return nil, err
	}
	args := spec.argv
	if len(args) == 0 {
		args = []string{path}
	}
	master, slaveName, err := openPtyMaster()
	if err != nil {
		return nil, err
	}
	slaveFD, err := syscall.Open(slaveName, syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	slave := os.NewFile(uintptr(slaveFD), slaveName)
	if spec.cols <= 0 || spec.rows <= 0 {
		spec.cols, spec.rows = 80, 24
	}
	_ = setPtySize(master, spec.cols, spec.rows)
	cmd := &exec.Cmd{
		Path: path, Args: args, Dir: spec.dir, Env: spec.env,
		Stdin: slave, Stdout: slave, Stderr: slave,
		// A session of its own whose controlling terminal is the slave, so
		// that the size change and the hangup of the terminal reach the
		// program, as in any terminal app.
		SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0},
	}
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return nil, err
	}
	// The child has its own copies.
	slave.Close()
	p := &ptyProcess{master: master, cmd: cmd, done: make(chan struct{}), code: -1}
	go func() {
		err := cmd.Wait()
		p.code = exitCode(err)
		close(p.done)
	}()
	return p, nil
}

// exitCode is the program's code: 128 plus the signal that killed it, as a
// shell reports it, or -1 when it could not be waited for.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return -1
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exit.ExitCode()
}

// Read reads the program's output: it waits until some is there, which is
// what the terminal expects of a Conn.
func (p *ptyProcess) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	switch {
	case errors.Is(err, os.ErrClosed), errors.Is(err, syscall.EBADF):
		return 0, io.EOF
	case errors.Is(err, syscall.EIO):
		// The last process with the terminal closed it, which Linux
		// reports as EIO rather than as the end of the file.
		return 0, io.EOF
	}
	return n, err
}

// Write types b into the terminal.
func (p *ptyProcess) Write(b []byte) (int, error) {
	n, err := p.master.Write(b)
	if errors.Is(err, os.ErrClosed) {
		return n, io.EOF
	}
	return n, err
}

// Resize sets the size of the terminal, which tells the program with
// SIGWINCH; ssh forwards it to the remote pty.
func (p *ptyProcess) Resize(cols, rows int) error {
	return setPtySize(p.master, cols, rows)
}

// ResizePixels sets the size and the size of a cell in pixels.
func (p *ptyProcess) ResizePixels(cols, rows, cellWidth, cellHeight int) error {
	return setPtyPixelSize(p.master, cols, rows, cols*cellWidth, rows*cellHeight)
}

// Close hangs up the program, as closing a terminal window does, and kills
// the ones that ignore it.
func (p *ptyProcess) Close() error {
	p.once.Do(func() {
		if p.master != nil {
			_ = p.master.Close()
		}
		go func() {
			select {
			case <-p.done:
			case <-time.After(time.Second):
				p.kill()
			}
		}()
	})
	return nil
}

// kill kills the program and its process group.
func (p *ptyProcess) kill() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = p.cmd.Process.Kill()
	}
}

// Exited is closed once the program ended.
func (p *ptyProcess) Exited() <-chan struct{} { return p.done }

// Wait waits for the program to end and returns its exit code.
func (p *ptyProcess) Wait() int {
	<-p.done
	return p.code
}

// CodeAfter waits up to d for the program to end and returns its code, or -1
// when it is still running: the end of the output can come before the kernel
// has the program's status.
func (p *ptyProcess) CodeAfter(d time.Duration) int {
	select {
	case <-p.done:
		return p.code
	case <-time.After(d):
		return -1
	}
}

// Code is the exit code, or -1 while the program runs.
func (p *ptyProcess) Code() int {
	select {
	case <-p.done:
		return p.code
	default:
		return -1
	}
}

// setPtySize sets the size of a pty in cells.
func setPtySize(master *os.File, cols, rows int) error {
	return setPtyPixelSize(master, cols, rows, 0, 0)
}

// setPtyPixelSize sets the size of a pty in cells and in pixels.
func setPtyPixelSize(master *os.File, cols, rows, width, height int) error {
	ws := &unix.Winsize{Row: clampPty(rows), Col: clampPty(cols), Xpixel: clampPty(width), Ypixel: clampPty(height)}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws)
}

func clampPty(v int) uint16 { return uint16(max(0, min(v, 0xFFFF))) }
