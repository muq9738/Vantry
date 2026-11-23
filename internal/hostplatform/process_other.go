//go:build !unix

package hostplatform

import "os"

// terminateProcess has no graceful signal on these hosts; childProcess.Stop
// kills the child directly.
func terminateProcess(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}
