//go:build windows

package hostfs

import "golang.org/x/sys/windows"

// The caller has flushed and closed the temporary file. Windows does not
// support fsync on a read-only directory handle; request a synchronous,
// write-through replacement instead of silently ignoring that failure.
func RenameDurable(oldPath, newPath string) error {
	oldName, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newName, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(oldName, newName, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
