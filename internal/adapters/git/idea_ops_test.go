package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestIdeaInspectHistoryTrackingStashesAndUntrackedDiff(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	base := run("rev-parse", "HEAD")
	bare := bareRemote(t)
	run("remote", "add", "origin", bare)
	run("push", "-u", "origin", "main")
	other, otherRun := cloneFromBare(t, bare)
	put(t, filepath.Join(other, "remote.txt"), "remote change\n")
	otherRun("add", "remote.txt")
	otherRun("commit", "-m", "remote change")
	otherRun("push")
	run("fetch", "origin")
	run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	put(t, filepath.Join(root, "file.txt"), "local commit\n")
	run("commit", "-am", "本地 change")
	run("tag", "v1")
	local := run("rev-parse", "HEAD")
	put(t, filepath.Join(root, "file.txt"), "default stash\n")
	run("stash", "push")
	put(t, filepath.Join(root, "file.txt"), "named stash\n")
	if _, err := StashPush(ctx, root, "draft work", false); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "new.txt"), "新文件\nsecond line\n")
	state, err := Inspect(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Ahead != 1 || state.Behind != 1 || state.RemoteBranch != "origin/main" {
		t.Fatalf("tracking: %+v", state)
	}
	if !slices.Equal(state.RemoteBranches, []string{"origin/main"}) {
		t.Fatalf("remote branches: %v", state.RemoteBranches)
	}
	if len(state.Commits) != 2 || state.Commits[0].ID != local || state.Commits[0].ShortID == "" || state.Commits[0].Subject != "本地 change" || state.Commits[0].Author != "Vantry Test" || state.Commits[0].AuthorTime.IsZero() {
		t.Fatalf("commits: %+v", state.Commits)
	}
	if !slices.Equal(state.Commits[0].Parents, []string{base}) || len(state.Commits[1].Parents) != 0 || !slices.Contains(state.Commits[0].Refs, "HEAD -> main") || !slices.Contains(state.Commits[0].Refs, "tag: v1") {
		t.Fatalf("commit parents/refs: %+v", state.Commits)
	}
	if len(state.Stashes) != 2 || state.Stashes[0].Ref != "stash@{0}" || state.Stashes[0].Branch != "main" || state.Stashes[0].Message != "On main: draft work" || state.Stashes[1].Branch != "main" || !strings.HasPrefix(state.Stashes[1].Message, "WIP on main:") {
		t.Fatalf("stashes: %+v", state.Stashes)
	}
	want := "diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+新文件\n+second line\n"
	if !strings.Contains(state.Diff, want) || !strings.Contains(state.Status, "?? new.txt") || len(state.Files) != 1 || state.Truncated {
		t.Fatalf("untracked preview: %+v", state)
	}
	if staged := run("diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("inspection staged files: %s", staged)
	}
}

func TestIdeaInspectUntrackedPreviewBoundaries(t *testing.T) {
	root, _ := repository(t)
	put(t, filepath.Join(root, "tail.txt"), "last line")
	put(t, filepath.Join(root, "binary.dat"), "hidden\x00binary")
	put(t, filepath.Join(root, "invalid.dat"), "invalid\xfftext")
	put(t, filepath.Join(root, "new\nline.txt"), "quoted\n")
	outside := filepath.Join(t.TempDir(), "private.txt")
	put(t, outside, "private content must stay outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "outside.txt")); err != nil {
		t.Fatal(err)
	}
	state, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.Diff, "+last line\n\\ No newline at end of file\n") || !strings.Contains(state.Diff, `diff --git "a/new\nline.txt" "b/new\nline.txt"`) {
		t.Fatalf("text preview: %q", state.Diff)
	}
	for _, unwanted := range []string{"hidden", "invalid", "private content"} {
		if strings.Contains(state.Diff, unwanted) {
			t.Fatalf("non-text/symlink content leaked: %q", state.Diff)
		}
	}
}

func TestIdeaInspectBoundsHugeUntrackedFiles(t *testing.T) {
	for _, content := range []string{strings.Repeat("x", maxUntrackedPreviewBytes+1), strings.Repeat("行", maxUntrackedPreviewBytes/3+2), strings.Repeat("line\n", maxOutputLines+1)} {
		t.Run(strconv.Itoa(len(content)), func(t *testing.T) {
			root, _ := repository(t)
			file, err := os.Create(filepath.Join(root, "large.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(content); err != nil {
				t.Fatal(err)
			}
			if len(content) > maxUntrackedPreviewBytes {
				// A large sparse file exercises the read bound without allocating
				// or writing the rest of its contents.
				if err := file.Truncate(1 << 34); err != nil {
					t.Fatal(err)
				}
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			state, err := Inspect(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !state.Truncated || !strings.Contains(state.Diff, "已截断") || len(state.Diff) > maxUntrackedPreviewBytes+2048 || strings.Count(state.Diff, "\n") > maxOutputLines+1 || !utf8.ValidString(state.Diff) {
				t.Fatalf("unbounded preview: %d bytes, truncated=%v", len(state.Diff), state.Truncated)
			}
		})
	}
}

func TestIdeaCommitCheckboxSelectionAndAmend(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	put(t, filepath.Join(root, "unchecked.txt"), "already staged\n")
	run("add", "unchecked.txt")
	put(t, filepath.Join(root, "file.txt"), "checked edit\n")
	put(t, filepath.Join(root, ":(glob)*.txt"), "literal new file\n")
	put(t, filepath.Join(root, "keep.txt"), "unchecked new file\n")
	selected := []string{"file.txt", ":(glob)*.txt"}
	if _, err := Commit(ctx, root, selected, "checked files", false); err != nil {
		t.Fatal(err)
	}
	if got := run("show", "HEAD:file.txt"); got != "checked edit" {
		t.Fatalf("checked edit not committed: %s", got)
	}
	if got := run("show", "HEAD::(glob)*.txt"); got != "literal new file" {
		t.Fatalf("literal file not committed: %s", got)
	}
	if files := run("ls-tree", "--name-only", "HEAD"); strings.Contains(files, "unchecked.txt") || strings.Contains(files, "keep.txt") {
		t.Fatalf("unchecked file committed: %s", files)
	}
	if staged := run("diff", "--cached", "--name-only"); staged != "unchecked.txt" {
		t.Fatalf("unrelated staged selection changed: %s", staged)
	}
	count := run("rev-list", "--count", "HEAD")
	put(t, filepath.Join(root, "file.txt"), "amended edit\n")
	if _, err := Commit(ctx, root, []string{"file.txt"}, "amended checked files", true); err != nil {
		t.Fatal(err)
	}
	if run("rev-list", "--count", "HEAD") != count || run("show", "HEAD:file.txt") != "amended edit" || run("log", "-1", "--format=%s") != "amended checked files" {
		t.Fatal("amend did not replace the selected commit")
	}
	if run("diff", "--cached", "--name-only") != "unchecked.txt" {
		t.Fatal("amend consumed an unchecked staged file")
	}
	for _, message := range []string{"", "  ", strings.Repeat("x", 16<<10+1)} {
		if _, err := Commit(ctx, root, []string{"keep.txt"}, message, false); err == nil {
			t.Fatalf("invalid message accepted: %q", message[:min(len(message), 10)])
		}
	}
	if run("diff", "--cached", "--name-only") != "unchecked.txt" {
		t.Fatal("invalid commit request staged files")
	}
}

func TestIdeaChangeCommitAmendAndDiscard(t *testing.T) {
	root, run := repository(t)
	put(t, filepath.Join(root, "new.txt"), "selected\n")
	request := api.WorkbenchRequest{Action: "git.commit", Branch: "main", Paths: []string{"new.txt"}, Message: "updated base", Amend: true}
	if _, err := Change(context.Background(), root, request); err != nil {
		t.Fatal(err)
	}
	if run("rev-list", "--count", "HEAD") != "1" || run("show", "HEAD:new.txt") != "selected" {
		t.Fatal("Change ignored Paths or Amend")
	}
	put(t, filepath.Join(root, "new.txt"), "discard this\n")
	request.Action, request.Paths = "git.discard", []string{"new.txt"}
	if _, err := Change(context.Background(), root, request); err != nil {
		t.Fatal(err)
	}
	if run("status", "--porcelain") != "" {
		t.Fatal("Change discard did not restore the worktree")
	}
}

func TestIdeaDiscardTrackedStagedDeletedAndUntrackedPaths(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	put(t, filepath.Join(root, "deleted.txt"), "restore me\n")
	run("add", "deleted.txt")
	run("commit", "-m", "second tracked file")
	put(t, filepath.Join(root, "file.txt"), "staged\n")
	run("add", "file.txt")
	put(t, filepath.Join(root, "file.txt"), "unstaged\n")
	run("rm", "deleted.txt")
	put(t, filepath.Join(root, "added.txt"), "staged addition\n")
	run("add", "added.txt")
	put(t, filepath.Join(root, "draft.txt"), "untracked\n")
	put(t, filepath.Join(root, "keep.txt"), "untouched\n")
	if err := os.Mkdir(filepath.Join(root, "drafts"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "drafts", "nested.txt"), "nested untracked\n")
	if _, err := Discard(ctx, root, []string{"file.txt", "deleted.txt", "added.txt", "draft.txt", "drafts"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("tracked restoration: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "deleted.txt")); err != nil || string(data) != "restore me\n" {
		t.Fatalf("staged deletion restoration: %q, %v", data, err)
	}
	for _, name := range []string{"added.txt", "draft.txt", "drafts"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("discard left %s: %v", name, err)
		}
	}
	if staged := run("diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("discard left index changes: %s", staged)
	}
	if run("status", "--porcelain") != "?? keep.txt" {
		t.Fatal("discard affected an unselected file")
	}
}

func TestIdeaDiscardRejectsTraversalAndEscapingSymlinksBeforeMutation(t *testing.T) {
	root, _ := repository(t)
	put(t, filepath.Join(root, "file.txt"), "must survive\n")
	outside := t.TempDir()
	put(t, filepath.Join(outside, "private.txt"), "private\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ".", "..", "../private.txt", filepath.Join(outside, "private.txt"), "escape/private.txt", "escape/missing.txt", "escape.txt", ".git/config"} {
		if _, err := Discard(context.Background(), root, []string{"file.txt", bad}); err == nil {
			t.Fatalf("unsafe path accepted: %q", bad)
		}
		if data, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(data) != "must survive\n" {
			t.Fatalf("invalid selection caused partial discard: %q, %v", data, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(outside, "private.txt")); err != nil || string(data) != "private\n" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}

func TestIdeaDiscardUsesLiteralPathsAndHandlesUnbornIndex(t *testing.T) {
	root, run := repository(t)
	for _, name := range []string{"*.txt", ":(glob)*", "-dash", "new\nline.txt"} {
		put(t, filepath.Join(root, name), "remove selected literal\n")
	}
	put(t, filepath.Join(root, "keep.txt"), "keep\n")
	if _, err := Discard(context.Background(), root, []string{"*.txt", ":(glob)*", "-dash", "new\nline.txt"}); err != nil {
		t.Fatal(err)
	}
	if run("status", "--porcelain") != "?? keep.txt" {
		t.Fatal("discard interpreted literal selections as patterns")
	}
	run("switch", "--orphan", "unborn")
	put(t, filepath.Join(root, "first.txt"), "new staged file\n")
	run("add", "first.txt")
	if _, err := Discard(context.Background(), root, []string{"first.txt"}); err != nil {
		t.Fatal(err)
	}
	if run("ls-files") != "" {
		t.Fatal("unborn discard left a staged addition")
	}
	if _, err := os.Stat(filepath.Join(root, "first.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unborn discard left file: %v", err)
	}
}

func TestIdeaCheckoutCreateRemoteTrackingAndDeleteBranches(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	base := run("rev-parse", "HEAD")
	if _, err := CheckoutBranch(ctx, root, "feature", true, "HEAD"); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "feature.txt"), "feature\n")
	run("add", "feature.txt")
	run("commit", "-m", "feature")
	if _, err := DeleteBranch(ctx, root, "feature", true); err == nil {
		t.Fatal("deleted the checked-out branch")
	}
	if _, err := CheckoutBranch(ctx, root, "main", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteBranch(ctx, root, "feature", false); err == nil {
		t.Fatal("deleted an unmerged branch without force")
	}
	if _, err := DeleteBranch(ctx, root, "feature", true); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckoutBranch(ctx, root, "from-base", true, base); err != nil {
		t.Fatal(err)
	}
	if run("rev-parse", "HEAD") != base {
		t.Fatal("branch creation ignored start point")
	}
	if _, err := CheckoutBranch(ctx, root, "main", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteBranch(ctx, root, "from-base", false); err != nil {
		t.Fatal(err)
	}
	bare := bareRemote(t)
	run("remote", "add", "origin", bare)
	run("push", "origin", "main:feature/nested")
	if _, err := CheckoutBranch(ctx, root, "origin/feature/nested", false, ""); err != nil {
		t.Fatal(err)
	}
	if run("branch", "--show-current") != "feature/nested" || run("rev-parse", "--abbrev-ref", "@{upstream}") != "origin/feature/nested" {
		t.Fatal("remote checkout did not create a local tracking branch")
	}
	if _, err := CheckoutBranch(ctx, root, "main", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckoutBranch(ctx, root, "origin/feature/nested", false, ""); err != nil {
		t.Fatalf("remote checkout with an existing local branch: %v", err)
	}
	for _, bad := range []string{"", "--force", "../escape", "HEAD", "@{-1}"} {
		if _, err := CheckoutBranch(ctx, root, bad, true, ""); err == nil {
			t.Fatalf("unsafe branch accepted: %q", bad)
		}
		if _, err := DeleteBranch(ctx, root, bad, true); err == nil {
			t.Fatalf("unsafe branch deletion accepted: %q", bad)
		}
	}
	if _, err := CheckoutBranch(ctx, root, "bad-start", true, "--detach"); err == nil {
		t.Fatal("unsafe start point accepted")
	}
}

func TestIdeaStashSelectedPopDropAndValidation(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	for _, value := range []string{"old", "new"} {
		put(t, filepath.Join(root, "file.txt"), value+"\n")
		if _, err := StashPush(ctx, root, value, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"--all", "refs/stash", "stash@{-1}", "stash@{x}", "stash@{0}^", "stash@{0}\n"} {
		if _, err := StashPop(ctx, root, bad); err == nil {
			t.Fatalf("invalid pop ref accepted: %q", bad)
		}
		if _, err := StashDrop(ctx, root, bad); err == nil {
			t.Fatalf("invalid drop ref accepted: %q", bad)
		}
	}
	if _, err := StashPop(ctx, root, "stash@{1}"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(data) != "old\n" {
		t.Fatalf("selected stash not applied: %q, %v", data, err)
	}
	if _, err := StashDrop(ctx, root, ""); err != nil {
		t.Fatal(err)
	}
	if run("stash", "list") != "" {
		t.Fatal("drop left the selected stash")
	}
	if data, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(data) != "old\n" {
		t.Fatalf("drop changed worktree: %q, %v", data, err)
	}
	if _, err := StashPush(ctx, root, "default pop", false); err != nil {
		t.Fatal(err)
	}
	if _, err := StashPop(ctx, root, ""); err != nil {
		t.Fatalf("empty pop ref did not default: %v", err)
	}
}

func TestIdeaLogLiteralMessageAuthorAndBranchFilters(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	run("commit", "--allow-empty", "-m", "Fix [UI].*")
	match := run("rev-parse", "HEAD")
	run("commit", "--allow-empty", "--author", "Special Writer <writer@example.invalid>", "-m", "ordinary subject")
	byAuthor := run("rev-parse", "HEAD")
	run("switch", "-c", "other")
	run("commit", "--allow-empty", "-m", "other-only")
	run("update-ref", "refs/remotes/origin/other", "HEAD")
	run("switch", "main")
	for _, test := range []struct {
		query, branch string
		want          []string
	}{
		{"[ui].*", "main", []string{match}},
		{"special writer", "main", []string{byAuthor}},
		{"Vantry Test", "main", nil},
		{"other-only", "main", []string{}},
		{"OTHER-ONLY", "", []string{run("rev-parse", "other")}},
		{"other-only", "*", []string{run("rev-parse", "other")}},
		{"no match", "*", []string{}},
	} {
		commits, err := Log(ctx, root, test.query, test.branch, 20)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, commit := range commits {
			ids = append(ids, commit.ID)
		}
		if test.want == nil {
			if len(commits) != 2 {
				t.Fatalf("author matches were not deduplicated: %+v", commits)
			}
		} else if !slices.Equal(ids, test.want) {
			t.Fatalf("query %q on %q: %v, want %v", test.query, test.branch, ids, test.want)
		}
	}
	for _, bad := range []string{"--all", "main..other", "missing"} {
		if _, err := Log(ctx, root, "", bad, 10); err == nil {
			t.Fatalf("invalid history revision accepted: %q", bad)
		}
	}
}

func TestIdeaLogAndInspectHistoryLimits(t *testing.T) {
	root, run := repository(t)
	base := run("rev-parse", "HEAD")
	// Native fast-import builds a long real history in one process.
	var input strings.Builder
	for i := 0; i < 505; i++ {
		message := fmt.Sprintf("history %d", i)
		fmt.Fprintf(&input, "commit refs/heads/main\ncommitter Vantry Test <test@example.invalid> %d +0000\ndata %d\n%s\n", time.Now().Unix()+int64(i), len(message), message)
		if i == 0 {
			fmt.Fprintf(&input, "from %s\n", base)
		}
		input.WriteString("\n")
	}
	if _, err := executeWithInput(context.Background(), root, nil, strings.NewReader(input.String()), "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ limit, want int }{{0, 100}, {-1, 100}, {7, 7}, {1000, 500}} {
		commits, err := Log(context.Background(), root, "", "main", test.limit)
		if err != nil || len(commits) != test.want {
			t.Fatalf("limit %d: %d commits, %v", test.limit, len(commits), err)
		}
	}
	state, err := Inspect(context.Background(), root)
	if err != nil || len(state.Commits) != 60 || state.Commits[0].Subject != "history 504" {
		t.Fatalf("inspection history limit: %d commits, %v", len(state.Commits), err)
	}
}

func TestIdeaInspectCommitRootNormalMergeAndBounds(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	base := run("rev-parse", "HEAD")
	detail, err := InspectCommit(ctx, root, base)
	if err != nil || detail.Commit.ID != base || len(detail.Commit.Parents) != 0 || len(detail.Files) != 1 || detail.Files[0].Path != "file.txt" || detail.Files[0].Index != "A" || !strings.Contains(detail.Diff, "+base") || detail.Truncated {
		t.Fatalf("initial commit: %+v, %v", detail, err)
	}
	feature(t, root, run, true)
	main := run("rev-parse", "HEAD")
	detail, err = InspectCommit(ctx, root, main)
	if err != nil || len(detail.Commit.Parents) != 1 || len(detail.Files) != 1 || detail.Files[0].Path != "main.txt" || !strings.Contains(detail.Diff, "+main") {
		t.Fatalf("normal commit: %+v, %v", detail, err)
	}
	run("merge", "--no-edit", "feature")
	detail, err = InspectCommit(ctx, root, "HEAD")
	if err != nil || len(detail.Commit.Parents) != 2 || len(detail.Files) != 2 || !strings.Contains(detail.Diff, "feature.txt") {
		t.Fatalf("merge commit: %+v, %v", detail, err)
	}
	put(t, filepath.Join(root, "file.txt"), strings.Repeat("large line\n", maxOutputLines+100))
	run("commit", "-am", "large change")
	detail, err = InspectCommit(ctx, root, "HEAD")
	if err != nil || !detail.Truncated || !strings.Contains(detail.Diff, "已截断") || len(detail.Diff) > maxOutputBytes+1024 {
		t.Fatalf("bounded commit diff: truncated=%v, %v", detail.Truncated, err)
	}
	for _, bad := range []string{"", "--all", "--output=outside", "HEAD..main", "missing"} {
		if _, err := InspectCommit(ctx, root, bad); err == nil {
			t.Fatalf("invalid commit revision accepted: %q", bad)
		}
	}
}

func TestIdeaInspectCommitRenameAndUnusualPaths(t *testing.T) {
	root, run := repository(t)
	run("config", "diff.renames", "true")
	run("mv", "file.txt", "renamed\n中文.txt")
	run("commit", "-m", "rename")
	detail, err := InspectCommit(context.Background(), root, "HEAD")
	if err != nil || len(detail.Files) != 1 || detail.Files[0].Path != "renamed\n中文.txt" || detail.Files[0].OriginalPath != "file.txt" || detail.Files[0].Index != "R" {
		t.Fatalf("rename paths: %+v, %v", detail.Files, err)
	}
}

func TestIdeaHistoryHandlesUnbornAndDetachedHead(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	run("switch", "--detach")
	state, err := Inspect(ctx, root)
	if err != nil || state.Branch != "detached HEAD" || len(state.Commits) != 1 || state.Ahead != 0 || state.Behind != 0 || state.RemoteBranch != "" {
		t.Fatalf("detached inspection: %+v, %v", state, err)
	}
	run("switch", "--orphan", "unborn")
	state, err = Inspect(ctx, root)
	if err != nil || len(state.Commits) != 0 || state.RemoteBranch != "" {
		t.Fatalf("unborn inspection: %+v, %v", state, err)
	}
	commits, err := Log(ctx, root, "", "", 20)
	if err != nil || len(commits) != 0 {
		t.Fatalf("unborn log: %+v, %v", commits, err)
	}
}

func TestIdeaMutationsRespectCheckoutLockAndCancellation(t *testing.T) {
	root, run := repository(t)
	run("branch", "other")
	put(t, filepath.Join(root, "file.txt"), "keep this edit\n")
	if _, err := StashPush(context.Background(), root, "keep stash", false); err != nil {
		t.Fatal(err)
	}
	lock, err := workspace.LockDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Unlock(lock)
	operations := map[string]func(context.Context) error{
		"commit":   func(ctx context.Context) error { _, err := Commit(ctx, root, nil, "blocked", true); return err },
		"discard":  func(ctx context.Context) error { _, err := Discard(ctx, root, []string{"file.txt"}); return err },
		"checkout": func(ctx context.Context) error { _, err := CheckoutBranch(ctx, root, "other", false, ""); return err },
		"delete":   func(ctx context.Context) error { _, err := DeleteBranch(ctx, root, "other", true); return err },
		"pop":      func(ctx context.Context) error { _, err := StashPop(ctx, root, "stash@{0}"); return err },
		"drop":     func(ctx context.Context) error { _, err := StashDrop(ctx, root, "stash@{0}"); return err },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			if err := operation(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("checkout lock/cancellation ignored: %v", err)
			}
		})
	}
	if _, err := Log(context.Background(), root, "", "", 10); err != nil {
		t.Fatalf("read-only log blocked by checkout lock: %v", err)
	}
	if run("branch", "--show-current") != "main" || run("rev-list", "--count", "HEAD") != "1" || run("stash", "list") == "" {
		t.Fatal("blocked operations changed repository state")
	}
}
