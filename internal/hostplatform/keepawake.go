package hostplatform

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
)

const (
	caffeinatePath = "/usr/bin/caffeinate"
	// sleepInhibitorSeconds is only a bound for hosts where the inhibitor
	// cannot watch a process; the guard stops it long before it expires.
	sleepInhibitorSeconds = 86400
)

// ErrClosed is returned by Acquire once the guard has been closed.
var ErrClosed = errors.New("hostplatform: KeepAwake 已关闭")

// CommandBuilder reports the sleep-inhibitor command for this host, or ok=false
// when the host has no supported tool.
type CommandBuilder func() (CommandSpec, bool)

// KeepAwakeOptions configures a guard. The zero value uses the host default
// inhibitor and real subprocesses.
type KeepAwakeOptions struct {
	// Command builds the inhibitor command. Nil uses the platform default.
	Command CommandBuilder
	// Launch starts the inhibitor. Nil runs the command as a subprocess; tests
	// and embedders can observe or replace process control here.
	Launch LaunchFunc
}

// KeepAwake keeps the machine out of idle sleep while at least one background
// run is active. Losing the network mid-stream because a laptop suspended turns
// a long model run into a failed one, and a failover retry cannot recover the
// interrupted stream.
//
// Holds are reference counted by run id: a run Acquire()s with its id and
// Release()s with the same id, so a duplicated release cannot drop another
// run's hold. SetActive adds one anonymous hold for callers that only know
// "something is running". The inhibitor runs exactly while the hold count is
// above zero, and Close always reaps it.
//
// The guard is safe for concurrent use. Start and stop happen under the guard's
// lock, so two runs starting at the same moment can never spawn two inhibitors
// and a stop cannot race a start.
type KeepAwake struct {
	mu      sync.Mutex
	holds   map[string]struct{}
	active  bool
	process Process
	closed  bool
	lastErr error
	build   CommandBuilder
	launch  LaunchFunc
}

// NewKeepAwake returns a guard that holds no inhibitor until the first Acquire
// or SetActive(true).
func NewKeepAwake(options KeepAwakeOptions) *KeepAwake {
	return &KeepAwake{build: options.Command, launch: options.Launch}
}

// Acquire records a hold for token and starts the inhibitor if it is the first
// one. Repeated Acquire calls with the same token are idempotent. A host
// without an inhibitor tool reports no error: the run continues, only sleep is
// not inhibited.
func (k *KeepAwake) Acquire(token string) error {
	if token == "" {
		return errors.New("hostplatform: KeepAwake 令牌不能为空")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return ErrClosed
	}
	if k.holds == nil {
		k.holds = make(map[string]struct{})
	}
	if _, held := k.holds[token]; held {
		return nil
	}
	k.holds[token] = struct{}{}
	if k.process != nil {
		return nil
	}
	// A previous start may have failed; a new hold retries it rather than
	// leaving the machine unprotected for the rest of the session.
	return k.startLocked()
}

// Release drops the hold for token. The inhibitor stops only when the last hold
// is gone; releasing an unknown or already released token is a no-op.
func (k *KeepAwake) Release(token string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, held := k.holds[token]; !held {
		return
	}
	delete(k.holds, token)
	if k.heldLocked() == 0 {
		k.stopLocked()
	}
}

// SetActive adds or removes one anonymous hold, for a caller tracking activity
// as a single flag instead of per-run ids.
func (k *KeepAwake) SetActive(active bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed || k.active == active {
		return
	}
	k.active = active
	if active {
		if k.process == nil {
			// SetActive has no error return: a failed start is reported by
			// LastError and retried on the next activity change.
			_ = k.startLocked()
		}
		return
	}
	if k.heldLocked() == 0 {
		k.stopLocked()
	}
}

// Holds reports the current hold count, including the anonymous SetActive hold.
func (k *KeepAwake) Holds() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.heldLocked()
}

// Active reports whether an inhibitor is currently running.
func (k *KeepAwake) Active() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.process != nil
}

// LastError reports the most recent failure to start the inhibitor. Missing
// tool support is not an error: it degrades to no inhibition.
func (k *KeepAwake) LastError() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lastErr
}

// Close drops every hold, stops and reaps the inhibitor, and makes later
// Acquire calls fail with ErrClosed. It is idempotent.
func (k *KeepAwake) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return nil
	}
	k.closed = true
	k.holds = nil
	k.active = false
	k.stopLocked()
	return nil
}

func (k *KeepAwake) heldLocked() int {
	n := len(k.holds)
	if k.active {
		n++
	}
	return n
}

func (k *KeepAwake) builderLocked() CommandBuilder {
	if k.build != nil {
		return k.build
	}
	return defaultSleepInhibitor
}

func (k *KeepAwake) launcherLocked() LaunchFunc {
	if k.launch != nil {
		return k.launch
	}
	return launchProcess
}

func (k *KeepAwake) startLocked() error {
	if k.process != nil {
		return nil
	}
	spec, ok := k.builderLocked()()
	if !ok {
		// No tool on this host: stay silent and keep the run alive.
		k.lastErr = nil
		return nil
	}
	process, err := k.launcherLocked()(spec)
	if err != nil {
		k.lastErr = err
		return err
	}
	k.lastErr = nil
	k.process = process
	return nil
}

func (k *KeepAwake) stopLocked() {
	process := k.process
	k.process = nil
	if process != nil {
		process.Stop()
	}
}

// sleepInhibitorCommand reports the idle-sleep inhibitor for goos. On darwin
// caffeinate -w watches our own pid: if Vantry is killed, even with SIGKILL,
// the inhibitor exits with it and no orphan keeps the assertion. On linux only
// systemd can block idle and sleep, so a host without systemd-inhibit simply
// runs without inhibition.
func sleepInhibitorCommand(goos string, pid int, lookPath func(string) (string, error)) (CommandSpec, bool) {
	switch goos {
	case "darwin":
		return CommandSpec{Name: caffeinatePath, Args: []string{"-i", "-w", strconv.Itoa(pid)}}, true
	case "linux":
		inhibitor, err := lookPath("systemd-inhibit")
		if err != nil {
			return CommandSpec{}, false
		}
		sleeper, err := lookPath("sleep")
		if err != nil {
			return CommandSpec{}, false
		}
		return CommandSpec{Name: inhibitor, Args: []string{
			"--what=idle:sleep",
			"--who=Vantry",
			"--why=Background run in progress",
			"--mode=block",
			sleeper,
			strconv.Itoa(sleepInhibitorSeconds),
		}}, true
	default:
		return CommandSpec{}, false
	}
}

func defaultSleepInhibitor() (CommandSpec, bool) {
	return sleepInhibitorCommand(runtime.GOOS, os.Getpid(), exec.LookPath)
}
