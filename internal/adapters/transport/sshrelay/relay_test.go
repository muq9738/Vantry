package sshrelay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/core"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// The executable on PATH is a controlled child of this test binary. It really
// forwards bytes between two UDS listeners, but never reads SSH configuration,
// contacts a remote account, or starts a remote command.
func TestSSHHelperProcess(t *testing.T) {
	if os.Getenv("VANTRY_SSH_HELPER") != "1" {
		return
	}
	log, err := os.OpenFile(os.Getenv("VANTRY_SSH_ATTEMPTS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(log).Encode(sshAttempt{PID: os.Getpid(), Args: os.Args})
	_ = log.Close()
	switch os.Getenv("VANTRY_SSH_MODE") {
	case "fail":
		fmt.Fprintln(os.Stderr, "Host key verification failed (controlled test)")
		os.Exit(255)
	case "exit":
		os.Exit(0)
	case "hang":
		fmt.Fprintln(os.Stderr, "Connection timed out (controlled test)")
		<-time.After(time.Hour)
		os.Exit(1)
	}
	var forward string
	for i, arg := range os.Args {
		if arg == "-L" && i+1 < len(os.Args) {
			forward = os.Args[i+1]
		}
	}
	localPath, remotePath, ok := strings.Cut(forward, ":")
	if !ok {
		os.Exit(2)
	}
	// Leave the pathname behind after SIGKILL, just as a crashed ssh can.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: localPath, Net: "unix"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	listener.SetUnlinkOnClose(false)
	for {
		incoming, err := listener.Accept()
		if err != nil {
			os.Exit(2)
		}
		go func() {
			defer incoming.Close()
			outgoing, err := net.Dial("unix", remotePath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "open remote socket:", err)
				return
			}
			defer outgoing.Close()
			copied := make(chan struct{})
			go func() {
				_, _ = io.Copy(outgoing, incoming)
				_ = outgoing.Close()
				close(copied)
			}()
			_, _ = io.Copy(incoming, outgoing)
			_ = incoming.Close()
			<-copied
		}()
	}
}

type sshAttempt struct {
	PID  int
	Args []string
}

func sshEnvironment(t *testing.T, mode string) string {
	t.Helper()
	// Short socket paths are required on macOS; do not use the user's home.
	root, err := os.MkdirTemp("/tmp", "vr-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexport VANTRY_SSH_HELPER=1\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestSSHHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// No real ssh fallback is available in these tests.
	t.Setenv("PATH", bin)
	t.Setenv("TMPDIR", root)
	t.Setenv("VANTRY_SSH_MODE", mode)
	t.Setenv("VANTRY_SSH_ATTEMPTS", filepath.Join(root, "attempts.jsonl"))
	return root
}

func attempts(root string) []sshAttempt {
	file, err := os.Open(filepath.Join(root, "attempts.jsonl"))
	if err != nil {
		return nil
	}
	defer file.Close()
	var out []sshAttempt
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var item sshAttempt
		if json.Unmarshal(scanner.Bytes(), &item) == nil {
			out = append(out, item)
		}
	}
	return out
}

func waitFor(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", description)
}

func serve(t *testing.T, root string, service api.Service) string {
	t.Helper()
	path := filepath.Join(root, "remote.sock")
	server, err := local.Listen(path, service)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return path
}

type controlledProvider struct {
	started chan struct{}
	finish  chan struct{}
	calls   atomic.Int32
}

func (p *controlledProvider) Run(ctx context.Context, _ provider.Request, emit func(string) error) error {
	if p.calls.Add(1) == 1 {
		close(p.started)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.finish:
		return emit("finished while disconnected")
	}
}

func newCore(t *testing.T, root string) (*core.Core, *controlledProvider) {
	t.Helper()
	p := &controlledProvider{started: make(chan struct{}), finish: make(chan struct{})}
	c, err := core.Open(context.Background(), core.Options{DBPath: filepath.Join(root, "core.db"), Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c, p
}

func event(t *testing.T, sub *api.Subscription) api.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events:
		if !ok {
			t.Fatal("subscription closed before expected event")
		}
		return ev
	case err := <-sub.Errors:
		t.Fatalf("subscription failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("event timeout")
	}
	return api.Event{}
}

func awaitClosed(t *testing.T, sub *api.Subscription) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for range sub.Events {
		}
		for range sub.Errors {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription did not close")
	}
}

func dispatch(t *testing.T, service api.Service, command api.Command) api.Receipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command.Version = api.ProtocolVersion
	receipt, err := service.Dispatch(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestRelayReconnectReplaysCursorAndKeepsCoreTaskAlive(t *testing.T) {
	root := sshEnvironment(t, "forward")
	c, p := newCore(t, root)
	remoteSocket := serve(t, root, c)
	r, err := Connect(context.Background(), "controlled-host", remoteSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	info, err := os.Stat(r.directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private relay directory: %v %v", info, err)
	}
	initial := attempts(root)
	if len(initial) != 1 {
		t.Fatalf("attempts: %+v", initial)
	}
	args := strings.Join(initial[0].Args, " ")
	for _, part := range []string{"-N -T -S none", "BatchMode=yes", "StrictHostKeyChecking=yes", "ForkAfterAuthentication=no", "-- controlled-host"} {
		if !strings.Contains(args, part) {
			t.Errorf("missing %q in arguments", part)
		}
	}
	sub, err := r.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	thread := dispatch(t, r, api.Command{Kind: api.CreateThread, CommandID: "create", Title: "relay test"})
	send := api.Command{Kind: api.SendMessage, CommandID: "send", ThreadID: thread.ThreadID, Message: "continue offline"}
	receipt := dispatch(t, r, send)
	var cursor int64
	for {
		ev := event(t, sub)
		if ev.Sequence != cursor+1 {
			t.Fatalf("cursor gap: %d -> %d", cursor, ev.Sequence)
		}
		cursor = ev.Sequence
		if ev.Type == "run.started" {
			break
		}
	}
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never started")
	}
	t.Setenv("VANTRY_SSH_MODE", "fail")
	if err := syscall.Kill(initial[0].PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, sub)
	waitFor(t, "failed reconnect diagnostic", func() bool {
		err := r.LastError()
		return err != nil && strings.Contains(err.Error(), "Host key verification failed")
	})
	close(p.finish)
	var completed api.Snapshot
	waitFor(t, "Core task completion while SSH is disconnected", func() bool {
		completed, err = c.Snapshot(context.Background())
		return err == nil && len(completed.Runs) == 1 && completed.Runs[0].Status == api.RunCompleted
	})
	if completed.Runs[0].Output != "finished while disconnected" {
		t.Fatalf("task output: %+v", completed.Runs[0])
	}
	t.Setenv("VANTRY_SSH_MODE", "forward")
	waitFor(t, "SSH forwarding to recover after a failed reconnect", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := r.Health(ctx)
		return err == nil && len(attempts(root)) >= 3
	})
	replayed, err := r.Subscribe(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer replayed.Close()
	for cursor < completed.Sequence {
		ev := event(t, replayed)
		if ev.Sequence != cursor+1 {
			t.Fatalf("reconnected cursor gap: %d -> %d", cursor, ev.Sequence)
		}
		cursor = ev.Sequence
	}
	if again := dispatch(t, r, send); again != receipt || p.calls.Load() != 1 {
		t.Fatalf("retry duplicated accepted work: %+v / %+v (%d calls)", again, receipt, p.calls.Load())
	}
	dispatch(t, r, api.Command{Kind: api.CreateThread, CommandID: "live", Title: "live after replay"})
	if ev := event(t, replayed); ev.Sequence != cursor+1 || ev.Type != "thread.created" {
		t.Fatalf("live continuation after replay: %+v", ev)
	}
	if _, err := r.Subscribe(context.Background(), -1); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("cursor error did not survive forwarding: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	awaitClosed(t, replayed)
	if _, err := os.Stat(r.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relay directory not removed: %v", err)
	}
	if health, err := c.Health(context.Background()); err != nil || health.State != "ready" {
		t.Fatalf("relay close stopped Core: %+v %v", health, err)
	}
}

type blockingService struct {
	api.Service
	entered  chan struct{}
	canceled chan struct{}
}

func (s blockingService) Snapshot(ctx context.Context) (api.Snapshot, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.canceled)
	return api.Snapshot{}, ctx.Err()
}

func TestRelayLifetimeCancellationClosesRequestsAndCleansSocket(t *testing.T) {
	root := sshEnvironment(t, "forward")
	c, p := newCore(t, root)
	service := blockingService{Service: c, entered: make(chan struct{}), canceled: make(chan struct{})}
	remoteSocket := serve(t, root, service)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := Connect(ctx, "controlled-host", remoteSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	thread := dispatch(t, r, api.Command{Kind: api.CreateThread, CommandID: "create"})
	dispatch(t, r, api.Command{Kind: api.SendMessage, CommandID: "send", ThreadID: thread.ThreadID, Message: "stay alive"})
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never started")
	}
	subCtx, cancelSub := context.WithCancel(context.Background())
	sub, err := r.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancelSub()
	awaitClosed(t, sub)
	result := make(chan error, 1)
	go func() { _, err := r.Snapshot(context.Background()); result <- err }()
	select {
	case <-service.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach remote service")
	}
	cancel() // Cleanup must not depend on an explicit Close call.
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("relay lifecycle did not stop")
	}
	if err := <-result; err == nil {
		t.Fatal("active HTTP request succeeded after cancellation")
	}
	select {
	case <-service.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("remote HTTP request was not canceled")
	}
	if _, err := os.Stat(r.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancellation left relay directory: %v", err)
	}
	close(p.finish)
	waitFor(t, "provider survives UI lifecycle cancellation", func() bool {
		snapshot, err := c.Snapshot(context.Background())
		return err == nil && len(snapshot.Runs) == 1 && snapshot.Runs[0].Status == api.RunCompleted
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestRelayStartupFailureDiagnosticsAndCleanup(t *testing.T) {
	for _, test := range []struct {
		name, mode, want string
		missingSSH       bool
		deadline         bool
	}{
		{name: "host trust", mode: "fail", want: "Host key verification failed"},
		{name: "clean exit is unexpected", mode: "exit", want: "退出码 0"},
		{name: "missing executable", mode: "forward", want: "启动 SSH", missingSSH: true},
		{name: "remote socket missing", mode: "forward", want: "open remote socket", deadline: true},
		{name: "handshake hang", mode: "hang", want: "Connection timed out", deadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := sshEnvironment(t, test.mode)
			if test.missingSSH {
				if err := os.Remove(filepath.Join(root, "bin", "ssh")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r, err := Connect(ctx, "controlled-host", filepath.Join(root, "missing.sock"))
			if r != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Connect = %v, %v; want %q", r, err, test.want)
			}
			if test.deadline != errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline diagnostic: %v", err)
			}
			paths, _ := filepath.Glob(filepath.Join(root, "vantry-relay-*"))
			if len(paths) != 0 {
				t.Fatalf("failed startup left sockets: %v", paths)
			}
			for _, attempt := range attempts(root) {
				if err := syscall.Kill(attempt.PID, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("SSH process not reaped: pid %d, %v", attempt.PID, err)
				}
			}
		})
	}
}

type unhealthyService struct {
	api.Service
	health api.Health
}

func (s unhealthyService) Health(context.Context) (api.Health, error) { return s.health, nil }

func TestRelayReportsCoreHealth(t *testing.T) {
	for _, test := range []struct {
		name, state, want string
		version           int
	}{
		{name: "protocol", version: api.ProtocolVersion + 1, state: "ready", want: "version_mismatch"},
		{name: "recovering", version: api.ProtocolVersion, state: "recovering", want: "recovering"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := sshEnvironment(t, "forward")
			path := serve(t, root, unhealthyService{health: api.Health{Version: test.version, State: test.state}})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := Connect(ctx, "controlled-host", path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("health diagnostic = %v; want %q", err, test.want)
			}
		})
	}
}

func TestRelayValidationAndCanceledStartup(t *testing.T) {
	root := sshEnvironment(t, "forward")
	for _, pair := range [][2]string{{"", "/core.sock"}, {"-bad", "/core.sock"}, {"host\nother", "/core.sock"}, {"host", "relative.sock"}, {"host", "/bad:socket"}} {
		if err := Validate(pair[0], pair[1]); err == nil {
			t.Fatalf("accepted invalid address: %q", pair)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Connect(ctx, "controlled-host", "/core.sock"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup: %v", err)
	}
	if len(attempts(root)) != 0 {
		t.Fatal("canceled startup launched SSH")
	}
}

func TestDiagnosticTailIsBounded(t *testing.T) {
	var tail diagnosticTail
	_, _ = tail.Write([]byte(strings.Repeat("x", diagnosticLimit*2)))
	_, _ = tail.Write([]byte("\x00\x1b useful failure\n"))
	got := tail.String()
	if len(got) > diagnosticLimit || !strings.HasSuffix(got, " useful failure") || strings.ContainsAny(got, "\x00\x1b") {
		t.Fatalf("invalid bounded diagnostic: length %d", len(got))
	}
}
