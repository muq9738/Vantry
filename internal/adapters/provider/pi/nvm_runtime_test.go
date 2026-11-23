package pi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFinderLaunchFindsExistingNVMPi(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	candidate := filepath.Join(home, ".nvm", "versions", "node", "v26.12.0", "bin", "pi")
	if err := os.MkdirAll(filepath.Dir(candidate), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	resolved := resolvePiExecutable("pi")
	want, _ := filepath.EvalSymlinks(candidate)
	if resolved != want {
		t.Fatalf("Finder runtime resolution=%q want %q", resolved, want)
	}
}
