//go:build windows

package app

import (
	"os"

	"github.com/muq9738/Vantry/internal/hostfs"
)

func prepareDirectory(path string) error { return hostfs.EnsurePrivateDir(path) }

func acquireLock(path string) (*os.File, error) {
	file, err := hostfs.OpenNoFollow(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := hostfs.TryLock(file); err != nil {
		_ = file.Close()
		if hostfs.LockBusy(err) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return file, nil
}

func releaseLock(file *os.File) {
	_ = hostfs.Unlock(file)
	_ = file.Close()
}
