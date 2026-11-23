package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestWorktreeInspectMergeAndCleanup(t *testing.T) {
	ctx := context.Background()
	repo, run := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "base.txt")
	run("commit", "-m", "base")

	dataDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dataDir, 0o700)
	threadID := "th_wt_1"
	spec := &api.WorkspaceSpec{
		Kind:       workspace.KindWorktree,
		Repository: repo,
		BaseRef:    "HEAD",
	}
	prepared, err := workspace.Prepare(ctx, dataDir, *spec, threadID)
	if err != nil {
		t.Fatalf("prepare managed worktree: %v", err)
	}
	wtPath := prepared.Path

	// Dirty worktree blocks merge & cleanup.
	if err := os.WriteFile(filepath.Join(wtPath, "wt.txt"), []byte("worktree change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wtStatus, err := InspectWorktree(ctx, dataDir, threadID, wtPath, spec)
	if err != nil {
		t.Fatalf("InspectWorktree dirty: %v", err)
	}
	if !wtStatus.IsWorktree || !wtStatus.Managed || !wtStatus.Exists || !wtStatus.Dirty || wtStatus.CanMerge {
		t.Fatalf("unexpected dirty worktree status: %+v", wtStatus)
	}
	if _, err := MergeAndCleanupWorktree(ctx, dataDir, threadID, wtPath, spec); err == nil {
		t.Fatal("dirty worktree should refuse merge")
	}

	// Commit worktree change; now CanMerge should become true.
	if _, err := command(ctx, wtPath, "add", "wt.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, wtPath, "commit", "-m", "wt commit"); err != nil {
		t.Fatal(err)
	}
	wtStatus, err = InspectWorktree(ctx, dataDir, threadID, wtPath, spec)
	if err != nil {
		t.Fatalf("InspectWorktree clean: %v", err)
	}
	if !wtStatus.CanMerge || wtStatus.Dirty || wtStatus.UnmergedCommits != 1 || wtStatus.BaseBranch != "main" {
		t.Fatalf("unexpected clean worktree status: %+v", wtStatus)
	}

	res, err := MergeAndCleanupWorktree(ctx, dataDir, threadID, wtPath, spec)
	if err != nil {
		t.Fatalf("MergeAndCleanupWorktree: %v", err)
	}
	if !res.CleanedUp || !res.BranchDeleted || res.TargetBranch != "main" {
		t.Fatalf("unexpected merge result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(repo, "wt.txt")); err != nil {
		t.Fatalf("merged file missing in main checkout: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists after cleanup: %v", err)
	}
}

func TestWorktreeMergeConflictPreservesWorktree(t *testing.T) {
	ctx := context.Background()
	repo, run := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "shared.txt")
	run("commit", "-m", "base")

	dataDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dataDir, 0o700)
	threadID := "th_wt_conflict"
	spec := &api.WorkspaceSpec{
		Kind:       workspace.KindWorktree,
		Repository: repo,
		BaseRef:    "HEAD",
	}
	prepared, err := workspace.Prepare(ctx, dataDir, *spec, threadID)
	if err != nil {
		t.Fatalf("prepare worktree: %v", err)
	}
	wtPath := prepared.Path

	// Diverging edits on main and worktree.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("main line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("commit", "-am", "main edit")

	if err := os.WriteFile(filepath.Join(wtPath, "shared.txt"), []byte("wt line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, wtPath, "commit", "-am", "wt edit"); err != nil {
		t.Fatal(err)
	}

	if _, err := MergeAndCleanupWorktree(ctx, dataDir, threadID, wtPath, spec); err == nil {
		t.Fatal("conflicting merge unexpectedly succeeded")
	}
	if st := run("status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("main repo left dirty after aborted conflict: %s", st)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should be preserved after conflict: %v", err)
	}
	if _, err := CleanupWorktreeOnly(ctx, dataDir, threadID, wtPath, spec, true); err != nil {
		t.Fatalf("CleanupWorktreeOnly: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed by CleanupWorktreeOnly: %v", err)
	}
}
