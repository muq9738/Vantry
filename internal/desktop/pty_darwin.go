//go:build darwin

package desktop

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

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
	var name [128]byte
	for _, step := range []struct {
		req uintptr
		arg unsafe.Pointer
	}{{unix.TIOCPTYGRANT, nil}, {unix.TIOCPTYUNLK, nil}, {unix.TIOCPTYGNAME, unsafe.Pointer(&name[0])}} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), step.req, uintptr(step.arg)); errno != 0 {
			master.Close()
			return nil, "", fmt.Errorf("pty: %w", errno)
		}
	}
	slave := make([]byte, 0, len(name))
	for _, b := range name {
		if b == 0 {
			break
		}
		slave = append(slave, b)
	}
	return master, string(slave), nil
}
