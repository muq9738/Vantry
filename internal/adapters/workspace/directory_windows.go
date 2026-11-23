//go:build windows

package workspace

import (
	"os"

	"github.com/muq9738/Vantry/internal/hostfs"
)

func openWorkspaceDirectory(path string) (*os.File, error) { return hostfs.OpenDirectoryLock(path) }
