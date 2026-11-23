package git

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitOperationsAndFilteredLog(t *testing.T) {
	root, run := repository(t)
	ctx := context.Background()
	base := run("rev-parse", "HEAD")
	put(t, filepath.Join(root, "a.txt"), "a\n")
	run("add", "a.txt")
	run("commit", "-m", "add a")
	second := run("rev-parse", "HEAD")

	// Filters: path and author narrow the history.
	commits, err := LogFiltered(ctx, root, LogFilter{Path: "a.txt"})
	if err != nil || len(commits) != 1 || commits[0].ID != second {
		t.Fatalf("path filter = %+v, %v", commits, err)
	}
	commits, err = LogFiltered(ctx, root, LogFilter{Author: "nobody-here"})
	if err != nil || len(commits) != 0 {
		t.Fatalf("author filter = %+v, %v", commits, err)
	}
	if _, err := LogFiltered(ctx, root, LogFilter{Path: "../outside"}); err == nil {
		t.Fatal("a path outside the checkout was accepted")
	}

	// Details carry the message, address, branches and HEAD mark.
	detail, err := InspectCommit(ctx, root, second)
	if err != nil || detail.Body != "add a" || detail.AuthorEmail != "test@example.invalid" || !detail.IsHead || len(detail.Branches) == 0 {
		t.Fatalf("detail extras = %+v, %v", detail.CommitExtras, err)
	}

	// Patch reads without changing anything.
	patch, err := CommitOperation(ctx, root, second, "patch", nil)
	if err != nil || !strings.Contains(patch, "Subject: [PATCH] add a") {
		t.Fatalf("patch = %q, %v", patch, err)
	}
	input, _ := json.Marshal(commitOpInput{Name: "v1"})
	if _, err := CommitOperation(ctx, root, base, "tag", input); err != nil || run("rev-parse", "v1^{commit}") != base {
		t.Fatalf("tag: %v", err)
	}
	input, _ = json.Marshal(commitOpInput{Name: "from-base"})
	if _, err := CommitOperation(ctx, root, base, "branch", input); err != nil || run("rev-parse", "from-base") != base {
		t.Fatalf("branch: %v", err)
	}
	input, _ = json.Marshal(commitOpInput{Message: "add a file"})
	if _, err := CommitOperation(ctx, root, second, "reword", input); err != nil || run("log", "-1", "--format=%s") != "add a file" {
		t.Fatalf("reword: %v", err)
	}
	if _, err := CommitOperation(ctx, root, base, "undo", nil); err == nil {
		t.Fatal("undo accepted a commit that is not HEAD")
	}
	head := run("rev-parse", "HEAD")
	if _, err := CommitOperation(ctx, root, head, "undo", nil); err != nil || run("rev-parse", "HEAD") != base {
		t.Fatalf("undo: %v", err)
	}
	if !strings.Contains(run("status", "--porcelain"), "A  a.txt") {
		t.Fatal("undo did not keep the change staged")
	}
	run("commit", "-m", "again")
	again := run("rev-parse", "HEAD")
	if _, err := CommitOperation(ctx, root, again, "revert", nil); err != nil || !strings.HasPrefix(run("log", "-1", "--format=%s"), "Revert") {
		t.Fatalf("revert: %v", err)
	}
	if _, err := CommitOperation(ctx, root, base, "reset-hard", nil); err != nil || run("rev-parse", "HEAD") != base {
		t.Fatalf("reset: %v", err)
	}
	if _, err := CommitOperation(ctx, root, again, "cherry-pick", nil); err != nil || run("log", "-1", "--format=%s") != "again" {
		t.Fatalf("cherry-pick: %v", err)
	}
	if _, err := CommitOperation(ctx, root, base, "checkout", nil); err != nil || run("rev-parse", "HEAD") != base {
		t.Fatalf("checkout: %v", err)
	}
	if _, err := CommitOperation(ctx, root, base, "explode", nil); err == nil {
		t.Fatal("unknown operation accepted")
	}
}
