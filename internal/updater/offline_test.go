package updater

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestOfflineInstallRefusesDesktopAndBusyOrUncooperativeCore(t *testing.T) {
	for _, name := range []string{"desktop-open", "core-busy", "drain-refused", "process-not-exited", "idle-core"} {
		t.Run(name, func(t *testing.T) {
			target := newTargetApp(t)
			staged := filepath.Join(t.TempDir(), "Vantry.app")
			writeFile(t, filepath.Join(staged, "Contents", "Resources", "new.txt"), "new")
			dir, err := os.MkdirTemp("/tmp", "vantry-update-core-")
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(dir, "core.sock"))
			if err != nil {
				os.RemoveAll(dir)
				t.Fatal(err)
			}
			var drained atomic.Bool
			var drainCalls atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/status":
					status := api.ServerStatus{PID: 4321, State: "ready"}
					if name == "core-busy" {
						status.ActiveRuns = 1
					}
					_ = json.NewEncoder(w).Encode(status)
				case "/v1/admin/shutdown-if-idle":
					drainCalls.Add(1)
					if name == "drain-refused" {
						http.Error(w, "busy", 409)
						return
					}
					drained.Store(true)
					w.WriteHeader(200)
				default:
					http.NotFound(w, r)
				}
			})}
			done := make(chan struct{})
			go func() { defer close(done); _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close(); <-done; _ = os.RemoveAll(dir) })
			processes := func(context.Context, string) ([]appProcess, error) {
				if drained.Load() && name != "process-not-exited" {
					return nil, nil
				}
				rows := []appProcess{{pid: 4321, command: target + "/Contents/MacOS/Vantry"}}
				if name == "desktop-open" {
					rows = append(rows, appProcess{pid: 5678, command: target + "/Contents/MacOS/Vantry"})
				}
				return rows, nil
			}
			manager := NewManager(Options{Runner: (&runnerLog{}).run})
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			err = manager.applyOffline(ctx, staged, target, dir, processes)
			if name == "idle-core" {
				if err != nil {
					t.Fatal(err)
				}
				if got := readFile(t, filepath.Join(target, "Contents", "Resources", "new.txt")); got != "new" {
					t.Fatal(got)
				}
				if _, err := os.Stat(staged); !os.IsNotExist(err) {
					t.Fatal("staged app not installed")
				}
			} else {
				if err == nil {
					t.Fatal("application replaced before old processes exited")
				}
				if got := readFile(t, filepath.Join(target, "Contents", "Resources", "old.txt")); got != "old" {
					t.Fatal("old application changed")
				}
				if _, err := os.Stat(staged); err != nil {
					t.Fatal("failed install discarded the staged bundle")
				}
			}
			if (name == "desktop-open" || name == "core-busy") && drainCalls.Load() != 0 {
				t.Fatal("shutdown was requested with a live desktop or busy Core")
			}
		})
	}
}

func TestNoReleaseIsReportedSeparatelyFromAuthentication(t *testing.T) {
	var writes atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if r.URL.Path == "/repos/"+DefaultRepo {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"private":true}`))
			return
		}
		http.NotFound(w, r)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	m := newTestManager(t, "http://"+listener.Addr().String(), Options{Token: "dedicated-read-token"})
	_, err = m.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "尚未发布正式 Release") {
		t.Fatalf("wrong diagnosis: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("updater made a write request")
	}
}

func TestInstallerQuotesPathsAndIsPrivate(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "it's a space", "Vantry.app")
	path, err := prepareInstaller(root, bundle, "/Applications/Vantry.app", "/tmp/core's data")
	if err != nil {
		t.Fatal(err)
	}
	text := readFile(t, path)
	if !strings.Contains(text, "'\\''") || !strings.Contains(text, "apply-update") || strings.Contains(text, "mv ") {
		t.Fatal("installer does not invoke guarded apply mode")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("installer command is not private")
	}
}

func TestOfflineApplyWaitsForDesktopExitThenDrainsIdleCore(t *testing.T) {
	target := newTargetApp(t)
	staged := filepath.Join(t.TempDir(), "Vantry.app")
	writeFile(t, filepath.Join(staged, "Contents", "Resources", "new.txt"), "new-v2")
	dir, err := os.MkdirTemp("/tmp", "vantry-wait-core-")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "core.sock"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	var drained atomic.Bool
	var desktopExited atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(api.ServerStatus{PID: 4321, State: "ready"})
		case "/v1/admin/shutdown-if-idle":
			drained.Store(true)
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done; _ = os.RemoveAll(dir) })

	time.AfterFunc(120*time.Millisecond, func() { desktopExited.Store(true) })
	processes := func(context.Context, string) ([]appProcess, error) {
		if drained.Load() {
			return nil, nil
		}
		rows := []appProcess{{pid: 4321, command: target + "/Contents/MacOS/vantry"}}
		if !desktopExited.Load() {
			rows = append(rows, appProcess{pid: 5678, command: target + "/Contents/MacOS/vantry"})
		}
		return rows, nil
	}
	manager := NewManager(Options{Runner: (&runnerLog{}).run})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = manager.applyOfflineWithOptions(ctx, staged, target, dir, ApplyOptions{WaitPID: 5678, WaitTimeout: time.Second}, processes)
	if err != nil {
		t.Fatalf("applyOfflineWithOptions: %v", err)
	}
	if !desktopExited.Load() || !drained.Load() {
		t.Fatalf("desktopExited=%v drained=%v", desktopExited.Load(), drained.Load())
	}
	if got := readFile(t, filepath.Join(target, "Contents", "Resources", "new.txt")); got != "new-v2" {
		t.Fatalf("target content = %q", got)
	}
}

func TestOfflineApplyWaitTimeoutRefusesIfDesktopStaysOpen(t *testing.T) {
	target := newTargetApp(t)
	staged := filepath.Join(t.TempDir(), "Vantry.app")
	writeFile(t, filepath.Join(staged, "Contents", "Resources", "new.txt"), "new")
	var drainCalls atomic.Int32
	dir, err := os.MkdirTemp("/tmp", "vantry-wait-timeout-")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "core.sock"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(api.ServerStatus{PID: 4321, State: "ready"})
		case "/v1/admin/shutdown-if-idle":
			drainCalls.Add(1)
			w.WriteHeader(200)
		}
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done; _ = os.RemoveAll(dir) })

	processes := func(context.Context, string) ([]appProcess, error) {
		return []appProcess{
			{pid: 4321, command: target + "/Contents/MacOS/vantry"},
			{pid: 5678, command: target + "/Contents/MacOS/vantry"},
		}, nil
	}
	manager := NewManager(Options{Runner: (&runnerLog{}).run})
	err = manager.applyOfflineWithOptions(context.Background(), staged, target, dir, ApplyOptions{WaitPID: 5678, WaitTimeout: 80 * time.Millisecond}, processes)
	if err == nil || !strings.Contains(err.Error(), "未在规定时间内退出") {
		t.Fatalf("expected wait timeout error, got: %v", err)
	}
	if drainCalls.Load() != 0 {
		t.Fatal("Core shutdown was requested while desktop was still open")
	}
	if got := readFile(t, filepath.Join(target, "Contents", "Resources", "old.txt")); got != "old" {
		t.Fatal("target app changed despite timeout")
	}
}

func TestLaunchApplyVerifiesBundleAndPassesWaitPID(t *testing.T) {
	target := newTargetApp(t)
	staged := filepath.Join(filepath.Dir(target), ".vantry-update-test", "payload", "Vantry.app")
	writeFile(t, filepath.Join(staged, "Contents", "Info.plist"), `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleExecutable</key><string>Vantry</string></dict></plist>`)
	exePath := filepath.Join(staged, "Contents", "MacOS", "Vantry")
	writeFile(t, exePath, "binary")
	_ = os.Chmod(exePath, 0o755)

	runner := &runnerLog{}
	var launchedExe string
	var launchedArgs []string
	mgr := NewManager(Options{
		GOOS:          "darwin",
		GOARCH:        "arm64",
		DataDir:       "/tmp/vantry-data",
		TargetAppPath: target,
		Runner:        runner.run,
		LaunchRunner: func(_ context.Context, exe string, args ...string) error {
			launchedExe = exe
			launchedArgs = append([]string(nil), args...)
			return nil
		},
	})
	mgr.mu.Lock()
	mgr.staged = &StagedUpdate{
		Version:    "0.1.28",
		StagedPath: staged,
		TargetPath: target,
	}
	mgr.mu.Unlock()

	if _, err := mgr.LaunchApply(context.Background(), "0.1.28", 9876); err != nil {
		t.Fatalf("LaunchApply: %v", err)
	}
	if launchedExe != exePath {
		t.Fatalf("launchedExe = %q, want %q", launchedExe, exePath)
	}
	want := []string{"apply-update", "--staged", staged, "--target", target, "--data-dir", "/tmp/vantry-data", "--wait-pid", "9876", "--wait-timeout", "20s"}
	if !slices.Equal(launchedArgs, want) {
		t.Fatalf("launchedArgs = %v, want %v", launchedArgs, want)
	}
	if len(runner.all()) != 1 || !strings.HasPrefix(runner.all()[0], "codesign --verify --deep --strict ") {
		t.Fatalf("expected codesign verification before launch, got: %v", runner.all())
	}
}
