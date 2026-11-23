package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func initTestRepo(t *testing.T) (string, func(...string) string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := command(ctx, root, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Vantry Test")
	run("config", "commit.gpgsign", "false")
	return root, run
}

func TestApplyHunkStageUnstageAndRevert(t *testing.T) {
	ctx := context.Background()
	root, run := initTestRepo(t)
	filePath := filepath.Join(root, "app.txt")

	var origLines []string
	for i := 1; i <= 30; i++ {
		origLines = append(origLines, "line-"+string(rune('A'+(i-1)%26)))
	}
	origLines[1] = "header-orig"
	origLines[27] = "footer-orig"
	if err := os.WriteFile(filePath, []byte(strings.Join(origLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "app.txt")
	run("commit", "-m", "initial")

	// Modify two separated regions so git produces two distinct hunks.
	edited := append([]string(nil), origLines...)
	edited[1] = "header-new"
	edited[27] = "footer-new"
	if err := os.WriteFile(filePath, []byte(strings.Join(edited, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Stage only hunk 0.
	if _, err := ApplyHunk(ctx, root, HunkRequest{
		Branch:    "main",
		FilePath:  "app.txt",
		Staged:    false,
		HunkIndex: 0,
		Action:    "stage",
	}); err != nil {
		t.Fatalf("stage hunk 0: %v", err)
	}
	stagedDiff := run("diff", "--cached", "--", "app.txt")
	if !strings.Contains(stagedDiff, "+header-new") || strings.Contains(stagedDiff, "+footer-new") {
		t.Fatalf("unexpected staged diff after hunk 0 stage:\n%s", stagedDiff)
	}
	unstagedDiff := run("diff", "--", "app.txt")
	if strings.Contains(unstagedDiff, "+header-new") || !strings.Contains(unstagedDiff, "+footer-new") {
		t.Fatalf("unexpected unstaged diff after hunk 0 stage:\n%s", unstagedDiff)
	}

	// Stale header expectation must be rejected safely.
	if _, err := ApplyHunk(ctx, root, HunkRequest{
		Branch:         "main",
		FilePath:       "app.txt",
		Staged:         false,
		HunkIndex:      0,
		Action:         "revert",
		ExpectedHeader: "@@ -999,1 +999,1 @@",
	}); err == nil {
		t.Fatal("stale header was unexpectedly accepted")
	}

	// Revert remaining unstaged hunk (footer-new -> footer-orig).
	if _, err := ApplyHunk(ctx, root, HunkRequest{
		Branch:    "main",
		FilePath:  "app.txt",
		Staged:    false,
		HunkIndex: 0,
		Action:    "revert",
	}); err != nil {
		t.Fatalf("revert remaining hunk: %v", err)
	}
	afterRevert, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterRevert), "header-new") || !strings.Contains(string(afterRevert), "footer-orig") || strings.Contains(string(afterRevert), "footer-new") {
		t.Fatalf("unexpected file after hunk revert:\n%s", string(afterRevert))
	}

	// Unstage hunk 0.
	if _, err := ApplyHunk(ctx, root, HunkRequest{
		Branch:    "main",
		FilePath:  "app.txt",
		Staged:    true,
		HunkIndex: 0,
		Action:    "unstage",
	}); err != nil {
		t.Fatalf("unstage hunk 0: %v", err)
	}
	if cached := run("diff", "--cached", "--", "app.txt"); strings.TrimSpace(cached) != "" {
		t.Fatalf("staged diff remained after unstage:\n%s", cached)
	}
}
