//go:build unix

package local

import (
	"fmt"
	"os"
	"syscall"
)

func checkPrivateParent(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("local: inspect socket parent: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local: socket parent is not a directory: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("local: socket parent is not private (mode %04o): %s", info.Mode().Perm(), path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("local: cannot determine socket parent owner: %s", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("local: socket parent is not owned by the current user: %s", path)
	}
	return nil
}
