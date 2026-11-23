package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDirectoryPrepareNormalizeRecoverAndConflict(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	directory := filepath.Join(root, "shared")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := api.WorkspaceSpec{Kind: KindDirectory, Path: directory}
	prepared, err := Prepare(context.Background(), dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	canonicalDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Path != canonicalDirectory {
		t.Fatalf("prepared path = %q, want %q", prepared.Path, canonicalDirectory)
	}
	again, err := Prepare(context.Background(), dataDir, spec, "thread-1")
	if err != nil || again.Path != prepared.Path {
		t.Fatalf("repeat prepare = %+v, %v", again, err)
	}
	conflicting := spec
	conflicting.Path = root
	if _, err := Prepare(context.Background(), dataDir, conflicting, "thread-1"); err == nil {
		t.Fatal("conflicting directory intent was accepted")
	}
}

func TestWorktreePrepareCrashRecoveryPreservesDirtyFiles(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initRepository(t, repository)
	dataDir := filepath.Join(root, "data")
	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: repository, Branch: "vantry/thread-1", BaseRef: "HEAD"}
	prepared, err := Prepare(context.Background(), dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Path == repository || prepared.Path == "" {
		t.Fatalf("worktree fell back to main checkout: %+v", prepared)
	}
	dirtyPath := filepath.Join(prepared.Path, "dirty.txt")
	if err := os.WriteFile(dirtyPath, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate Core dying after git worktree add but before prepared.json.
	if err := os.Remove(filepath.Join(dataDir, "workspaces", "thread-1", "prepared.json")); err != nil {
		t.Fatal(err)
	}
	recovered, err := Prepare(context.Background(), dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Path != prepared.Path {
		t.Fatalf("recovered path = %q, want %q", recovered.Path, prepared.Path)
	}
	if data, err := os.ReadFile(dirtyPath); err != nil || string(data) != "keep me" {
		t.Fatalf("dirty file was not preserved: %q, %v", data, err)
	}
	conflict := spec
	conflict.Branch = "vantry/other"
	if _, err := Prepare(context.Background(), dataDir, conflict, "thread-1"); err == nil {
		t.Fatal("conflicting worktree intent was accepted")
	}
	if data, err := os.ReadFile(dirtyPath); err != nil || string(data) != "keep me" {
		t.Fatalf("conflict changed dirty worktree: %q, %v", data, err)
	}
}

func TestFailedWorktreePreparationPersistsIntentAndNeverFallsBack(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	repository := filepath.Join(root, "not-a-repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: repository}
	if _, err := Prepare(context.Background(), dataDir, spec, "thread-1"); err == nil {
		t.Fatal("non-git repository preparation succeeded")
	}
	intentPath := filepath.Join(dataDir, "workspaces", "thread-1", "intent.json")
	data, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("intent was not persisted before failure: %v", err)
	}
	var recorded intent
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Spec.Kind != KindWorktree || recorded.Spec.Repository != canonicalRepository {
		t.Fatalf("recorded intent = %+v", recorded)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "workspaces", "thread-1", "prepared.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preparation produced a ready record: %v", err)
	}
}

func TestUnownedExistingWorktreePathIsPreserved(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initRepository(t, repository)
	dataDir := filepath.Join(root, "data")
	path := filepath.Join(root, "existing")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "unowned.txt")
	if err := os.WriteFile(marker, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: repository, Path: path}
	if _, err := Prepare(context.Background(), dataDir, spec, "thread-1"); err == nil {
		t.Fatal("unowned directory was adopted as a worktree")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "do not delete" {
		t.Fatalf("unowned path was changed: %q, %v", data, err)
	}
}

func TestDirectoryLockSerializesIndependentOwners(t *testing.T) {
	root := t.TempDir()
	first, err := LockDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := LockDirectory(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want deadline", err)
	}
	Unlock(first)
	second, err := LockDirectory(context.Background(), root)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	Unlock(second)
}

func initRepository(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "", "init", "-b", "main", path)
	runGitTest(t, path, "config", "user.name", "Vantry Test")
	runGitTest(t, path, "config", "user.email", "vantry@example.invalid")
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, path, "add", "README.md")
	runGitTest(t, path, "commit", "-m", "initial")
}

func runGitTest(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
