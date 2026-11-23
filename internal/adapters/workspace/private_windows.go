//go:build windows

package workspace

import (
	"os"

	"github.com/muq9738/Vantry/internal/hostfs"
)

func privateDirectory(path string) error { return hostfs.EnsurePrivateDir(path) }

func openPreparationLock(path string) (*os.File, error) {
	return hostfs.OpenNoFollow(path, os.O_CREATE|os.O_RDWR, 0o600)
}
