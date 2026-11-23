package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitTestCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBranchOpsUpdateCompareRebaseAndRename(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	repo := filepath.Join(tmp, "repo")
	peer := filepath.Join(tmp, "peer")

	runGitTestCmd(t, tmp, "init", "--bare", bare)
	runGitTestCmd(t, tmp, "clone", bare, repo)
	runGitTestCmd(t, repo, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCmd(t, repo, "add", "a.txt")
	runGitTestCmd(t, repo, "commit", "-m", "init main")
	runGitTestCmd(t, repo, "push", "-u", "origin", "main")

	// Create a feature branch in repo.
	runGitTestCmd(t, repo, "checkout", "-b", "feature/ui")
	if err := os.WriteFile(filepath.Join(repo, "ui.txt"), []byte("ui change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCmd(t, repo, "add", "ui.txt")
	runGitTestCmd(t, repo, "commit", "-m", "feat: ui")

	// Advance origin/main from a peer clone so repo's non-checked-out `main` is behind.
	runGitTestCmd(t, tmp, "clone", "-b", "main", bare, peer)
	if err := os.WriteFile(filepath.Join(peer, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCmd(t, peer, "add", "a.txt")
	runGitTestCmd(t, peer, "commit", "-m", "update main upstream")
	runGitTestCmd(t, peer, "push", "origin", "main")

	// 1. UpdateBranch fast-forwards non-checked-out `main` without leaving `feature/ui`.
	if _, err := UpdateBranch(ctx, repo, "main"); err != nil {
		t.Fatalf("UpdateBranch(main) failed: %v", err)
	}
	cur := runGitTestCmd(t, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if cur != "feature/ui" {
		t.Fatalf("UpdateBranch switched away from current branch: got %q", cur)
	}
	mainHead := runGitTestCmd(t, repo, "rev-parse", "main")
	peerHead := runGitTestCmd(t, peer, "rev-parse", "main")
	if mainHead != peerHead {
		t.Fatalf("non-checked-out main was not fast-forwarded: got %s want %s", mainHead, peerHead)
	}

	// 2. CompareBranches shows 1 incoming commit from main and 1 local commit on feature/ui.
	cmp, err := CompareBranches(ctx, repo, "feature/ui", "main")
	if err != nil {
		t.Fatalf("CompareBranches failed: %v", err)
	}
	if len(cmp.AheadCommits) != 1 || cmp.AheadCommits[0].Subject != "update main upstream" {
		t.Fatalf("unexpected ahead commits: %+v", cmp.AheadCommits)
	}
	if len(cmp.BehindCommits) != 1 || cmp.BehindCommits[0].Subject != "feat: ui" {
		t.Fatalf("unexpected behind commits: %+v", cmp.BehindCommits)
	}
	if len(cmp.Files) != 2 || !strings.Contains(cmp.Diff, "a.txt") {
		t.Fatalf("unexpected compare files/diff: files=%+v diff=%q", cmp.Files, cmp.Diff)
	}

	// 3. RebaseBranch rebases feature/ui cleanly onto main.
	if _, err := RebaseBranch(ctx, repo, "main"); err != nil {
		t.Fatalf("RebaseBranch failed: %v", err)
	}
	cmpAfter, err := CompareBranches(ctx, repo, "feature/ui", "main")
	if err != nil {
		t.Fatalf("CompareBranches after rebase failed: %v", err)
	}
	if len(cmpAfter.AheadCommits) != 0 || len(cmpAfter.BehindCommits) != 1 {
		t.Fatalf("expected 0 ahead / 1 behind after rebase: %+v", cmpAfter)
	}

	// 4. RenameBranch renames current branch cleanly.
	if _, err := RenameBranch(ctx, repo, "feature/ui", "feature/idea-git"); err != nil {
		t.Fatalf("RenameBranch failed: %v", err)
	}
	if got := runGitTestCmd(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/idea-git" {
		t.Fatalf("current branch after rename = %q", got)
	}
}
