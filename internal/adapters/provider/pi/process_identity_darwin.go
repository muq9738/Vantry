//go:build darwin

package pi

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// The kernel creation time remains stable when Node changes process.title.
// A reused PID is not sufficient authority to signal a process.
func processIdentity(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("darwin:%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), nil
}
