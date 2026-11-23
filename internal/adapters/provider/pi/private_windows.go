//go:build windows

package pi

import (
	"os"

	"github.com/muq9738/Vantry/internal/hostfs"
)

func makePrivateDir(path string) error { return hostfs.EnsurePrivateDir(path) }

func openProviderLock(path string) (*os.File, error) {
	return hostfs.OpenNoFollow(path, os.O_CREATE|os.O_RDWR, 0o600)
}
