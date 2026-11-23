package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckReloadsDedicatedTokenAfterMissingInvalidOrRotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-token")
	expected := "dedicated-token-one"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expected {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRelease(t, w, githubRelease{TagName: "v0.2.0", Assets: []githubAsset{{Name: "vantry-0.2.0-darwin-arm64.zip", Size: 100}}})
	}))
	defer server.Close()
	manager := NewManager(Options{APIBase: server.URL, TokenFile: path, CurrentVersion: "0.1.18", GOOS: "darwin", GOARCH: "arm64"})
	ctx := context.Background()
	if _, err := manager.Check(ctx); err == nil {
		t.Fatal("private release was accessible without a token")
	}
	if err := os.WriteFile(path, []byte(expected), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := manager.Check(ctx); err != nil || !info.Available {
		t.Fatalf("token added after startup was ignored: %+v %v", info, err)
	}
	expected = "dedicated-token-two"
	if err := os.WriteFile(path, []byte(expected), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Check(ctx); err != nil {
		t.Fatalf("rotated token was ignored: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Check(ctx); err == nil {
		t.Fatal("insecure token permissions were ignored")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Check(ctx); err != nil {
		t.Fatalf("fixed token permissions require an app restart: %v", err)
	}
}
