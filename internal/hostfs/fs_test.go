package hostfs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestIndependentLockHandlesAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	first, err := OpenNoFollow(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenNoFollow(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := TryLock(first); err != nil {
		t.Fatal(err)
	}
	defer Unlock(first)
	if err := TryLock(second); !LockBusy(err) {
		t.Fatalf("second writer acquired a held lock: %v", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(second); err != nil {
		t.Fatalf("released lock is unavailable: %v", err)
	}
	_ = Unlock(second)
}

func TestNoFollowRejectsFinalSymlinkAndRootEscape(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(outside, link); err != nil {
		// Tests never request elevation to create a Windows symlink.
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skipf("symlink creation requires Developer Mode for this account: %v", err)
		}
		t.Fatal(err)
	}
	if file, err := OpenNoFollow(link, os.O_RDONLY, 0); err == nil {
		file.Close()
		t.Fatal("opened substituted symlink")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if file, err := OpenRootNoFollow(root, "link"); err == nil {
		file.Close()
		t.Fatal("opened a symlink outside the root")
	}
	if file, err := OpenRootNoFollow(root, "../secret"); err == nil {
		file.Close()
		t.Fatal("opened a parent traversal outside the root")
	}
}

func TestPrivateDirectoryAndDurableReplacement(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(directory); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(directory, true); err != nil {
		t.Fatal(err)
	}
	oldPath, newPath := filepath.Join(directory, "pending"), filepath.Join(directory, "published")
	if err := os.WriteFile(oldPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameDurable(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(newPath)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement = %q, %v", data, err)
	}
	if _, err := os.Lstat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file still exists: %v", err)
	}
}
