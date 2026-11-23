//go:build unix

package workspace

import (
	"os"
)

func openWorkspaceDirectory(path string) (*os.File, error) { return os.Open(path) }
