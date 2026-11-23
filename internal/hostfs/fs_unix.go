//go:build unix

// Package hostfs contains the filesystem operations whose security guarantees
// depend on the host kernel.
package hostfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
}

func OpenRootNoFollow(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func OpenDirectoryLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func TryLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func Unlock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func LockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return CheckPrivateDir(path, false)
}

func CheckPrivateDir(path string, owner bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("not a private directory (0700): %s", path)
	}
	if owner {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return fmt.Errorf("directory is not owned by the current user: %s", path)
		}
	}
	return nil
}

func RenameDurable(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(newPath))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func Executable(info os.FileInfo) bool { return info.Mode().Perm()&0o111 != 0 }
