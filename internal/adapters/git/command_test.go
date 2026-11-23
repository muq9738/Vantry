package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A launcher may already point Git at its own repository. Vantry's Git
// commands must keep operating on the checkout they were given.
func TestCommandIgnoresInheritedRepositoryRouting(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	decoy, decoyRun := repository(t)
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(decoy, ".git", "objects"))

	put(t, filepath.Join(root, "staged.txt"), "staged\n")
	run("add", "staged.txt")
	if topLevel := run("rev-parse", "--show-toplevel"); resolvedPath(t, topLevel) != resolvedPath(t, root) {
		t.Fatalf("Git targeted another checkout: %q", topLevel)
	}
	state, err := Inspect(ctx, root)
	if err != nil || state.Branch != "main" || !strings.Contains(state.Status, "staged.txt") {
		t.Fatalf("inspection left the requested checkout: %+v %v", state, err)
	}
	if dirty := decoyRun("status", "--porcelain"); dirty != "" {
		t.Fatalf("requested command touched the launcher repository: %q", dirty)
	}
	if topLevel := decoyRun("rev-parse", "--show-toplevel"); resolvedPath(t, topLevel) != resolvedPath(t, decoy) {
		t.Fatalf("launcher repository moved: %q", topLevel)
	}

	// Hooks stay enabled: an inherited routing environment must not be the
	// reason a commit succeeds.
	hooks := filepath.Join(root, ".git", "test-hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nprintf hook-ran > '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.hooksPath", hooks)
	run("commit", "-m", "commit through Vantry")
	if data, err := os.ReadFile(marker); err != nil || string(data) != "hook-ran" {
		t.Fatalf("pre-commit hook did not run: %q, %v", data, err)
	}
	if status := run("status", "--porcelain"); status != "" {
		t.Fatalf("commit did not land in the requested checkout: %q", status)
	}
}

func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
