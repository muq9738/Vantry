package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
)

func TestCoreStartupReportsNewerDatabaseWithoutWaitingForTimeout(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(directory, "state.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE future_records (id TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO future_records VALUES ('keep', 'history'); PRAGMA user_version = 99;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(directory, "core.log")
	oldLog := "previous launch diagnostic must not be shown\n"
	if err := os.WriteFile(logPath, []byte(oldLog), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCoreProcess$")
	command.Env = append(os.Environ(), "VANTRY_TEST_CORE_DIR="+directory)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	reaped := make(chan struct{})
	go func() {
		exited <- command.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		select {
		case <-reaped:
			return
		default:
			_ = command.Process.Kill()
		}
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Error("Core fixture did not exit before temporary directory cleanup")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	client, err := waitForCore(ctx, Config{DataDir: directory}, local.Dial(filepath.Join(directory, "core.sock")), exited, int64(len(oldLog)))
	if client != nil {
		_ = client.Close()
		t.Fatal("attached to Core despite incompatible database")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("lost process exit error: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("startup failure took %s instead of reporting the process exit", elapsed)
	}
	for _, text := range []string{"database schema 99 is newer than supported 11", dbPath, "数据库备份", logPath} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("startup error missing %q: %v", text, err)
		}
	}
	if strings.Contains(err.Error(), strings.TrimSpace(oldLog)) {
		t.Fatalf("startup error included an earlier launch: %v", err)
	}
	<-reaped
	// The refusal must preserve future data, its version, and journal settings.
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var mode, value string
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 99 {
		t.Fatalf("version=%d, error=%v", version, err)
	}
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("journal mode changed: %s, error=%v", mode, err)
	}
	if err := db.QueryRow("SELECT value FROM future_records WHERE id = 'keep'").Scan(&value); err != nil || value != "history" {
		t.Fatalf("future data changed: %q, error=%v", value, err)
	}
}

func TestCoreStartupLogIsBoundedAndExcludesOlderLaunches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.log")
	old := "old diagnostic\n"
	current := strings.Repeat("x", 10*1024) + "\nnew diagnostic\n"
	if err := os.WriteFile(path, []byte(old+current), 0o600); err != nil {
		t.Fatal(err)
	}
	got := coreStartupLog(path, int64(len(old)))
	if len(got) > 8*1024 || strings.Contains(got, "old diagnostic") || !strings.HasSuffix(got, "new diagnostic") {
		t.Fatalf("unexpected startup log: length=%d", len(got))
	}
}
