//go:build windows

package projects

import "github.com/muq9738/Vantry/internal/hostfs"

func prepareStoreDirectory(path string) error { return hostfs.EnsurePrivateDir(path) }
