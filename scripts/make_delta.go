//go:build ignore

// Generate a self-verifying binary delta package between two Vantry builds.
// Run from the repository root:
//
//	go run scripts/make_delta.go -from 0.1.27 -to 0.1.28 -old <old-dir> -new <new-dir> -out <delta-path>
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/muq9738/Vantry/internal/updater"
)

func main() {
	from := flag.String("from", "", "source version (e.g. 0.1.27)")
	to := flag.String("to", "", "target version (e.g. 0.1.28)")
	oldDir := flag.String("old", "", "source app bundle or directory")
	newDir := flag.String("new", "", "target app bundle or directory")
	outPath := flag.String("out", "", "output .delta path")
	flag.Parse()

	if *from == "" || *to == "" || *oldDir == "" || *newDir == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: make_delta -from <ver> -to <ver> -old <dir> -new <dir> -out <file>")
		os.Exit(2)
	}
	if err := run(*from, *to, *oldDir, *newDir, *outPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(from, to, oldDir, newDir, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".delta-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := updater.WriteDelta(tmp, from, to, oldDir, newDir); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return err
	}
	var newSize int64
	_ = filepath.WalkDir(newDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, statErr := d.Info(); statErr == nil {
				newSize += fi.Size()
			}
		}
		return nil
	})
	fmt.Printf("%s: %d bytes (vs %d bytes uncompressed target)\n", outPath, info.Size(), newSize)
	return nil
}
