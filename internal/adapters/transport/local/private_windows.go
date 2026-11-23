//go:build windows

package local

import "github.com/muq9738/Vantry/internal/hostfs"

func checkPrivateParent(path string) error { return hostfs.CheckPrivateDir(path, true) }
