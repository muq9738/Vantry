//go:build linux

package desktop

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPtyMaster opens a new pseudo-terminal and returns its master and the
// path of its slave, with both descriptors in blocking mode (see
// ptyProcess).
func openPtyMaster() (*os.File, string, error) {
	fd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("pty: %w", err)
	}
	master := os.NewFile(uintptr(fd), "/dev/ptmx")
	// Unlock the slave: 0 clears TIOCSPTLCK's lock.
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("pty: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, "", fmt.Errorf("pty: %w", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n), nil
}
