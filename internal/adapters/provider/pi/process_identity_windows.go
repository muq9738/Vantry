//go:build windows

package pi

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func processIdentity(pid int) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(process)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("windows:%08x%08x", created.HighDateTime, created.LowDateTime), nil
}
