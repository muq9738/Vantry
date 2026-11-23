package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/muq9738/Vantry/internal/updater"
	"github.com/muq9738/Vantry/pkg/api"
)

func mustZipDir(t *testing.T, root, top string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(raw)
		return err
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

func TestUpdateAuthErrorIsActionableInDesktop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	service := &workbenchService{upd: updater.NewManager(updater.Options{APIBase: server.URL, TokenFinder: func(context.Context) (string, error) { return "", nil }})}
	for _, action := range []string{"update.check", "update.install"} {
		_, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: action})
		if api.CodeOf(err) != api.InvalidCommand || !strings.Contains(err.Error(), "专用凭据") {
			t.Fatalf("%s hides the configuration error: %v", action, err)
		}
	}
}

type fakeUpdateCoreService struct {
	api.Service
	health   api.Health
	snapshot api.Snapshot
}

func (f *fakeUpdateCoreService) Health(context.Context) (api.Health, error) {
	return f.health, nil
}

func (f *fakeUpdateCoreService) Snapshot(context.Context) (api.Snapshot, error) {
	return f.snapshot, nil
}

func TestUpdateApplyRefusesBusyCoreAndLaunchesWhenIdle(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "Vantry.app")
	if err := os.MkdirAll(filepath.Join(target, "Contents", "Resources"), 0o755); err != nil {
		t.Fatal(err)
	}

	zipBytes := func() []byte {
		stagedSrc := filepath.Join(t.TempDir(), "Vantry.app")
		_ = os.MkdirAll(filepath.Join(stagedSrc, "Contents", "MacOS"), 0o755)
		_ = os.WriteFile(filepath.Join(stagedSrc, "Contents", "Info.plist"), []byte(`<plist><dict><key>CFBundleExecutable</key><string>vantry</string></dict></plist>`), 0o644)
		_ = os.WriteFile(filepath.Join(stagedSrc, "Contents", "MacOS", "vantry"), []byte("bin"), 0o755)
		return mustZipDir(t, filepath.Dir(stagedSrc), "Vantry.app")
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + updater.DefaultRepo + "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v0.1.28",
				"assets": []map[string]any{{
					"name":                 "vantry-0.1.28-darwin-arm64.zip",
					"size":                 len(zipBytes),
					"browser_download_url": "http://" + r.Host + "/download/app.zip",
				}},
			})
		case "/download/app.zip":
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var launchCalls atomic.Int32
	mgr := updater.NewManager(updater.Options{
		APIBase:        server.URL,
		GOOS:           "darwin",
		GOARCH:         "arm64",
		CurrentVersion: "0.1.27",
		Token:          "unit-token-1234",
		TargetAppPath:  target,
		Runner:         func(context.Context, string, ...string) error { return nil },
		LaunchRunner: func(context.Context, string, ...string) error {
			launchCalls.Add(1)
			return nil
		},
	})

	core := &fakeUpdateCoreService{
		health: api.Health{State: "ready"},
		snapshot: api.Snapshot{Runs: []api.Run{{ID: "r1", ThreadID: "t1", Status: api.RunRunning}}},
	}
	service := &workbenchService{Service: core, upd: mgr}

	installResp, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "update.install", Name: "0.1.28"})
	if err != nil {
		t.Fatalf("update.install: %v", err)
	}
	if installResp.Update == nil || !installResp.Update.Staged || !installResp.Update.CanAutoRestart {
		t.Fatalf("expected Update.Staged=true & CanAutoRestart=true, got %+v", installResp.Update)
	}
	if strings.Contains(installResp.Output, ".vantry-update-") {
		t.Fatalf("update.install output should not dump raw hidden path: %q", installResp.Output)
	}

	// Refuses when Core has an active run without invoking LaunchRunner.
	_, err = service.Workbench(context.Background(), api.WorkbenchRequest{Action: "update.apply", Name: "0.1.28", Input: []byte(`{"desktopPid":1234}`)})
	if err == nil || api.CodeOf(err) != api.InvalidCommand || !strings.Contains(err.Error(), "活动任务") {
		t.Fatalf("expected busy Core refusal, got: %v", err)
	}
	if launchCalls.Load() != 0 {
		t.Fatal("LaunchRunner called while Core had active runs")
	}

	// Succeeds when Core is ready & idle.
	core.snapshot.Runs[0].Status = api.RunCompleted
	applyResp, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "update.apply", Name: "0.1.28", Input: []byte(`{"desktopPid":1234}`)})
	if err != nil {
		t.Fatalf("update.apply(idle): %v", err)
	}
	if launchCalls.Load() != 1 {
		t.Fatalf("launchCalls = %d, want 1", launchCalls.Load())
	}
	if applyResp.Update == nil || !applyResp.Update.Staged {
		t.Fatalf("unexpected applyResp.Update = %+v", applyResp.Update)
	}
}
