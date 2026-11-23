//go:build linux

package pi

import (
	"fmt"
	"os"
	"strings"
)

func processIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return "", fmt.Errorf("invalid process stat for pid %d", pid)
	}
	fields := strings.Fields(string(data)[end+2:])
	if len(fields) <= 19 {
		return "", fmt.Errorf("missing process start time for pid %d", pid)
	}
	return "linux:" + fields[19], nil
}
