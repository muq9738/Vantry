package hostplatform

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

type inhibitorCounts struct {
	builds   int
	launches int
	stops    int
	live     int
	peak     int
}

// inhibitorHarness records the subprocess lifecycle a guard drives, including
// how many inhibitors are alive at once.
type inhibitorHarness struct {
	mu        sync.Mutex
	counts    inhibitorCounts
	launchErr error
	lastSpec  CommandSpec
}

func (h *inhibitorHarness) command() (CommandSpec, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts.builds++
	return CommandSpec{Name: "test-inhibitor", Args: []string{"--hold"}}, true
}

func (h *inhibitorHarness) launch(spec CommandSpec) (Process, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts.launches++
	h.lastSpec = spec
	if h.launchErr != nil {
		return nil, h.launchErr
	}
	h.counts.live++
	if h.counts.live > h.counts.peak {
		h.counts.peak = h.counts.live
	}
	return &harnessProcess{harness: h}, nil
}

func (h *inhibitorHarness) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts.stops++
	h.counts.live--
}

func (h *inhibitorHarness) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.launchErr = err
}

func (h *inhibitorHarness) snapshot() inhibitorCounts {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts
}

type harnessProcess struct {
	harness *inhibitorHarness
	once    sync.Once
}

func (p *harnessProcess) Stop() {
	p.once.Do(p.harness.release)
}

func newHarnessGuard(harness *inhibitorHarness) *KeepAwake {
	return NewKeepAwake(KeepAwakeOptions{Command: harness.command, Launch: harness.launch})
}

func TestKeepAwakeHoldsInhibitorWhileAnyTokenIsActive(t *testing.T) {
	harness := &inhibitorHarness{}
	guard := newHarnessGuard(harness)

	if err := guard.Acquire("run-1"); err != nil {
		t.Fatal(err)
	}
	if counts := harness.snapshot(); counts.launches != 1 || counts.stops != 0 || counts.live != 1 {
		t.Fatalf("first hold did not start exactly one inhibitor: %+v", counts)
	}
	if !guard.Active() || guard.Holds() != 1 {
		t.Fatalf("active=%v holds=%d", guard.Active(), guard.Holds())
	}

	if err := guard.Acquire("run-2"); err != nil {
		t.Fatal(err)
	}
	if counts := harness.snapshot(); counts.launches != 1 {
		t.Fatalf("second hold spawned another inhibitor: %+v", counts)
	}
	if guard.Holds() != 2 {
		t.Fatalf("holds=%d", guard.Holds())
	}

	guard.Release("run-1")
	guard.Release("run-1") // a duplicate release must not drop run-2's hold
	if counts := harness.snapshot(); counts.stops != 0 || counts.live != 1 {
		t.Fatalf("inhibitor stopped while a hold remained: %+v", counts)
	}
	if guard.Holds() != 1 {
		t.Fatalf("holds=%d", guard.Holds())
	}

	guard.Release("run-2")
	if counts := harness.snapshot(); counts.stops != 1 || counts.live != 0 {
		t.Fatalf("last release did not stop the inhibitor: %+v", counts)
	}
	if guard.Active() || guard.Holds() != 0 {
		t.Fatalf("active=%v holds=%d", guard.Active(), guard.Holds())
	}

	if err := guard.Acquire("run-3"); err != nil {
		t.Fatal(err)
	}
	if counts := harness.snapshot(); counts.launches != 2 || counts.live != 1 {
		t.Fatalf("later work did not start a fresh inhibitor: %+v", counts)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if counts := harness.snapshot(); counts.stops != 2 || counts.live != 0 {
		t.Fatalf("close leaked the inhibitor: %+v", counts)
	}
}

func TestKeepAwakeConcurrentHoldsNeverOverlapInhibitors(t *testing.T) {
	harness := &inhibitorHarness{}
	guard := newHarnessGuard(harness)

	const workers = 24
	const rounds = 20
	var group sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			for round := 0; round < rounds; round++ {
				token := fmt.Sprintf("run-%d-%d", worker, round)
				if err := guard.Acquire(token); err != nil {
					t.Errorf("acquire %s: %v", token, err)
					return
				}
				guard.Release(token)
			}
		}(worker)
	}
	close(start)
	group.Wait()

	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	counts := harness.snapshot()
	if counts.live != 0 {
		t.Fatalf("inhibitor leaked after concurrent holds: %+v", counts)
	}
	if counts.launches != counts.stops {
		t.Fatalf("unbalanced lifecycle: %+v", counts)
	}
	if counts.peak > 1 {
		t.Fatalf("concurrent holds spawned overlapping inhibitors: %+v", counts)
	}
	if guard.Holds() != 0 || guard.Active() {
		t.Fatalf("holds=%d active=%v", guard.Holds(), guard.Active())
	}
}

func TestKeepAwakeSetActiveTracksAnonymousHold(t *testing.T) {
	harness := &inhibitorHarness{}
	guard := newHarnessGuard(harness)

	guard.SetActive(true)
	guard.SetActive(true)
	if counts := harness.snapshot(); counts.launches != 1 || counts.live != 1 {
		t.Fatalf("SetActive(true) did not start exactly one inhibitor: %+v", counts)
	}
	if guard.Holds() != 1 {
		t.Fatalf("holds=%d", guard.Holds())
	}

	if err := guard.Acquire("run-1"); err != nil {
		t.Fatal(err)
	}
	guard.SetActive(false)
	if counts := harness.snapshot(); counts.stops != 0 {
		t.Fatalf("anonymous release dropped the token hold: %+v", counts)
	}
	if guard.Holds() != 1 {
		t.Fatalf("holds=%d", guard.Holds())
	}

	guard.Release("run-1")
	if counts := harness.snapshot(); counts.stops != 1 || counts.live != 0 {
		t.Fatalf("last hold did not stop the inhibitor: %+v", counts)
	}
	if guard.Active() {
		t.Fatal("inhibitor still active")
	}
}

func TestKeepAwakeDegradesWithoutInhibitorTool(t *testing.T) {
	guard := NewKeepAwake(KeepAwakeOptions{
		Command: func() (CommandSpec, bool) { return CommandSpec{}, false },
		Launch: func(CommandSpec) (Process, error) {
			t.Fatal("launch ran for a host without an inhibitor tool")
			return nil, nil
		},
	})
	if err := guard.Acquire("run-1"); err != nil {
		t.Fatalf("missing tool must not fail a run: %v", err)
	}
	if guard.Active() || guard.Holds() != 1 || guard.LastError() != nil {
		t.Fatalf("active=%v holds=%d lastErr=%v", guard.Active(), guard.Holds(), guard.LastError())
	}
	guard.Release("run-1")
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestKeepAwakeReportsStartFailureAndRetries(t *testing.T) {
	harness := &inhibitorHarness{}
	failure := errors.New("spawn failed")
	harness.fail(failure)
	guard := newHarnessGuard(harness)
	defer guard.Close()

	if err := guard.Acquire("run-1"); !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	if counts := harness.snapshot(); counts.launches != 1 || counts.live != 0 {
		t.Fatalf("failed launch left a live inhibitor: %+v", counts)
	}
	if !errors.Is(guard.LastError(), failure) || guard.Active() {
		t.Fatalf("lastErr=%v active=%v", guard.LastError(), guard.Active())
	}
	if guard.Holds() != 1 {
		t.Fatalf("hold was not kept for the failed run: %d", guard.Holds())
	}

	harness.fail(nil)
	if err := guard.Acquire("run-2"); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if counts := harness.snapshot(); counts.launches != 2 || counts.live != 1 {
		t.Fatalf("retry did not start an inhibitor: %+v", counts)
	}
	if guard.LastError() != nil || !guard.Active() {
		t.Fatalf("lastErr=%v active=%v", guard.LastError(), guard.Active())
	}
}

func TestKeepAwakeCloseReapsAndRejectsNewHolds(t *testing.T) {
	harness := &inhibitorHarness{}
	guard := newHarnessGuard(harness)

	if err := guard.Acquire("run-1"); err != nil {
		t.Fatal(err)
	}
	if err := guard.Acquire("run-2"); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if counts := harness.snapshot(); counts.stops != 1 || counts.live != 0 {
		t.Fatalf("close did not reap the inhibitor exactly once: %+v", counts)
	}
	if err := guard.Acquire("run-3"); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("close is not idempotent: %v", err)
	}
	guard.Release("run-1") // must not panic or stop anything twice
	if counts := harness.snapshot(); counts.stops != 1 {
		t.Fatalf("duplicate stop after close: %+v", counts)
	}
	if guard.Holds() != 0 || guard.Active() {
		t.Fatalf("holds=%d active=%v", guard.Holds(), guard.Active())
	}
}

func TestKeepAwakeRejectsEmptyToken(t *testing.T) {
	guard := newHarnessGuard(&inhibitorHarness{})
	if err := guard.Acquire(""); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestSleepInhibitorCommandBindsCurrentProcess(t *testing.T) {
	spec, ok := sleepInhibitorCommand("darwin", 4212, func(string) (string, error) {
		t.Fatal("darwin uses the absolute caffeinate path")
		return "", nil
	})
	if !ok {
		t.Fatal("darwin must inhibit sleep")
	}
	if spec.Name != "/usr/bin/caffeinate" {
		t.Fatalf("name=%q", spec.Name)
	}
	if len(spec.Args) != 3 || spec.Args[0] != "-i" || spec.Args[1] != "-w" || spec.Args[2] != "4212" {
		t.Fatalf("caffeinate must watch our pid: %v", spec.Args)
	}
}

func TestSleepInhibitorCommandLinuxRequiresTools(t *testing.T) {
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	if _, ok := sleepInhibitorCommand("linux", 1, missing); ok {
		t.Fatal("linux without systemd-inhibit must degrade quietly")
	}

	spec, ok := sleepInhibitorCommand("linux", 1, func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	})
	if !ok {
		t.Fatal("linux with systemd-inhibit must inhibit sleep")
	}
	if spec.Name != "/usr/bin/systemd-inhibit" {
		t.Fatalf("name=%q", spec.Name)
	}
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{"--what=idle:sleep", "--mode=block", "--who=Vantry", "/usr/bin/sleep"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %v missing %q", spec.Args, want)
		}
	}
}

func TestSleepInhibitorCommandUnsupportedPlatform(t *testing.T) {
	if _, ok := sleepInhibitorCommand("windows", 1, func(string) (string, error) {
		return "", exec.ErrNotFound
	}); ok {
		t.Fatal("unsupported platform claimed an inhibitor")
	}
}
