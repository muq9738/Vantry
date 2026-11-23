//go:build unix

package projects

import (
	"errors"
	"os"
)

func prepareStoreDirectory(absolute string) error {
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("project data directory must be a private directory (0700)")
	}
	return nil
}
