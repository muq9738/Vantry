package workspace

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/muq9738/Vantry/internal/hostfs"
)

// LockDirectory serializes writers using the directory's stable inode. It does
// not add lock files to the user's checkout. The returned file must be released
// with Unlock after the provider process and its tools have finished.
func LockDirectory(ctx context.Context, path string) (*os.File, error) {
	return LockDirectoryNotify(ctx, path, nil)
}

// LockDirectoryNotify behaves like LockDirectory and invokes onBusy once if the
// directory lock is currently held by another process.
func LockDirectoryNotify(ctx context.Context, path string, onBusy func()) (*os.File, error) {
	absolute, err := absoluteClean(path)
	if err != nil {
		return nil, err
	}
	file, err := openWorkspaceDirectory(absolute)
	if err != nil {
		return nil, fmt.Errorf("open workspace for writing lock: %w", err)
	}
	notified := false
	for {
		err = hostfs.TryLock(file)
		if err == nil {
			return file, nil
		}
		if !hostfs.LockBusy(err) {
			_ = file.Close()
			return nil, err
		}
		if !notified && onBusy != nil {
			notified = true
			onBusy()
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func lockFile(ctx context.Context, path string) (*os.File, error) {
	file, err := openPreparationLock(path)
	if err != nil {
		return nil, err
	}
	for {
		err = hostfs.TryLock(file)
		if err == nil {
			return file, nil
		}
		if !hostfs.LockBusy(err) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Unlock releases a workspace or preparation lock. Lock files remain on disk;
// unlinking one would allow another process to lock a different inode.
func Unlock(file *os.File) {
	if file == nil {
		return
	}
	_ = hostfs.Unlock(file)
	_ = file.Close()
}
