package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBundledCredentialSupportsFreshInstallAndLocalOverride(t *testing.T) {
	dir := t.TempDir()
	local, bundled := filepath.Join(dir, "local-token"), filepath.Join(dir, "bundle-token")
	writeFile(t, bundled, "bundled-readonly-token") // Distribution resources can be 0644.
	expected := "bundled-readonly-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expected {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeRelease(t, w, githubRelease{TagName: "v0.2.0"})
	}))
	defer server.Close()
	manager := NewManager(Options{APIBase: server.URL, TokenFile: local, BundledTokenFile: bundled})
	check := func() {
		t.Helper()
		if _, err := manager.Check(context.Background()); err != nil {
			t.Fatalf("credential source not applied: %v", err)
		}
	}
	check() // New computer: no local credential file.
	expected = "local-override-token"
	if err := os.WriteFile(local, []byte(expected), 0o600); err != nil {
		t.Fatal(err)
	}
	check() // Local override becomes effective without restarting Core.
	if err := os.WriteFile(local, []byte("invalid token value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Check(context.Background()); err == nil {
		t.Fatal("invalid local override silently used the bundled token")
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	expected = "bundled-readonly-token"
	check() // Removing the override restores the distributed credential.
}

func TestFreshInstallDownloadsPrivateAssetWithBundledCredential(t *testing.T) {
	const token = "bundled-readonly-token"
	archive := zipBytes(t, map[string]string{
		"Vantry.app/Contents/Info.plist":             "new",
		"Vantry.app/Contents/MacOS/Vantry":           "binary",
		"Vantry.app/Contents/Resources/update-token": token,
	}, map[string]os.FileMode{"Vantry.app/Contents/MacOS/Vantry": 0o755})
	downloaded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{TagName: "v0.2.0", Assets: []githubAsset{{Name: "vantry-0.2.0-darwin-arm64.zip", Size: int64(len(archive)), URL: "http://" + r.Host + "/asset"}}})
		case "/asset":
			downloaded = true
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	target := newTargetApp(t)
	bundled := filepath.Join(target, "Contents", "Resources", "update-token")
	writeFile(t, bundled, token)
	runner := &runnerLog{}
	manager := NewManager(Options{APIBase: server.URL, CurrentVersion: "0.1.21", GOOS: "darwin", GOARCH: "arm64", TargetAppPath: target, TokenFile: filepath.Join(t.TempDir(), "missing-token"), BundledTokenFile: bundled, Runner: runner.run})
	staged, err := manager.DownloadAndStage(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !downloaded || staged.InstallerPath == "" || staged.Installed {
		t.Fatalf("private asset was not staged for offline installation: %+v", staged)
	}
	if got := readFile(t, filepath.Join(staged.StagedPath, "Contents", "Resources", "update-token")); got != token {
		t.Fatal("staged app lost its distributed update credential")
	}
}

func TestUpdateTokenFollowsPackagedExecutable(t *testing.T) {
	for _, tc := range []struct{ executable, credential string }{
		{"/Applications/Vantry.app/Contents/MacOS/Vantry", "/Applications/Vantry.app/Contents/Resources/update-token"},
		{"/portable/Vantry.exe", "/portable/update-token"},
	} {
		if got := updateTokenBesideExecutable(filepath.FromSlash(tc.executable)); got != filepath.FromSlash(tc.credential) {
			t.Fatalf("wrong credential location for %s: %s", tc.executable, got)
		}
	}
}
