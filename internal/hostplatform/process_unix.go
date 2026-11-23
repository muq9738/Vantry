//go:build unix

package hostplatform

import (
	"os"
	"syscall"
)

// terminateProcess asks a child to release its inhibitor and exit on its own;
// childProcess.Stop escalates to Kill if it does not.
func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Signal(syscall.SIGTERM)
}
