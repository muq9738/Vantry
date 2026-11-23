//go:build windows

package hostfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/hostprocess"
	"golang.org/x/sys/windows"
)

func TestPrivateDirectoryRejectsEveryoneACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(path, true); err == nil {
		t.Fatal("accepted a private directory with an Everyone grant")
	}
	if err := EnsurePrivateDir(path); err == nil {
		t.Fatal("silently accepted or rewrote an existing shared directory")
	}
}

func TestWindowsFilesystemHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "hostfs-lock-helper" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestExecutionJobBlocksNewWriterWithoutAWorkerFileLock(t *testing.T) {
	file, err := OpenNoFollow(filepath.Join(t.TempDir(), "writer.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := hostprocess.New(os.Args[0], "-test.run=^TestWindowsFilesystemHelper$", "--", "hostfs-lock-helper")
	cmd.SetJobIdentity(file.Name())
	defer cmd.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(file); !LockBusy(err) {
		t.Fatalf("a live execution did not block its next writer: %v", err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := TryLock(file); err != nil {
		t.Fatalf("terminated execution still blocks the writer: %v", err)
	}
	_ = Unlock(file)
}
