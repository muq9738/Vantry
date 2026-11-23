package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

func bareRemote(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := command(context.Background(), bare, "init", "--bare", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	return bare
}

func cloneFromBare(t *testing.T, bare string) (string, func(...string) string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	if _, err := command(context.Background(), filepath.Dir(dir), "clone", bare, dir); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := command(context.Background(), dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	run("config", "user.name", "Vantry Test")
	run("config", "user.email", "test@example.invalid")
	run("config", "commit.gpgsign", "false")
	return dir, run
}

func TestRemotePushFetchPullFFOnlyAndAheadBehind(t *testing.T) {
	ctx := context.Background()
	rootA, runA := repository(t)
	bare := bareRemote(t)
	runA("remote", "add", "origin", bare)

	// Before setting upstream, AheadBehind returns empty upstream without error.
	ahead, behind, upstream, err := AheadBehind(ctx, rootA)
	if err != nil || ahead != 0 || behind != 0 || upstream != "" {
		t.Fatalf("expected no upstream initially, got ahead=%d behind=%d upstream=%q err=%v", ahead, behind, upstream, err)
	}

	// Publish main with setUpstream=true to local bare remote.
	if _, err := Push(ctx, rootA, "origin", "main", true); err != nil {
		t.Fatalf("initial Push with setUpstream failed: %v", err)
	}
	ahead, behind, upstream, err = AheadBehind(ctx, rootA)
	if err != nil || ahead != 0 || behind != 0 || upstream != "origin/main" {
		t.Fatalf("unexpected tracking after push: ahead=%d behind=%d upstream=%q err=%v", ahead, behind, upstream, err)
	}

	// Clone second working tree from bare remote and push two commits.
	rootB, runB := cloneFromBare(t, bare)
	put(t, filepath.Join(rootB, "second.txt"), "two\n")
	runB("add", "second.txt")
	runB("commit", "-m", "second commit")
	put(t, filepath.Join(rootB, "third.txt"), "three\n")
	runB("add", "third.txt")
	runB("commit", "-m", "third commit")
	if _, err := Push(ctx, rootB, "", "", false); err != nil {
		t.Fatalf("Push from cloneB failed: %v", err)
	}

	// Fetch in rootA updates remote-tracking ref and AheadBehind shows behind=2.
	if _, err := Fetch(ctx, rootA, "origin"); err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	ahead, behind, upstream, err = AheadBehind(ctx, rootA)
	if err != nil || ahead != 0 || behind != 2 || upstream != "origin/main" {
		t.Fatalf("expected behind=2 after fetch, got ahead=%d behind=%d upstream=%q err=%v", ahead, behind, upstream, err)
	}

	// Fast-forward pull in rootA brings both commits cleanly.
	if _, err := PullFFOnly(ctx, rootA, "origin", "main"); err != nil {
		t.Fatalf("PullFFOnly failed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(rootA, "third.txt")); err != nil || string(data) != "three\n" {
		t.Fatalf("expected third.txt after PullFFOnly, got %q, %v", data, err)
	}
	ahead, behind, upstream, err = AheadBehind(ctx, rootA)
	if err != nil || ahead != 0 || behind != 0 || upstream != "origin/main" {
		t.Fatalf("expected synced after PullFFOnly, got ahead=%d behind=%d upstream=%q err=%v", ahead, behind, upstream, err)
	}

	// Diverge rootA (ahead=1) and rootB (pushed to origin -> behind=1).
	put(t, filepath.Join(rootA, "local-a.txt"), "from A\n")
	runA("add", "local-a.txt")
	runA("commit", "-m", "commit A")

	put(t, filepath.Join(rootB, "remote-b.txt"), "from B\n")
	runB("add", "remote-b.txt")
	runB("commit", "-m", "commit B")
	if _, err := Push(ctx, rootB, "origin", "main", false); err != nil {
		t.Fatalf("Push from cloneB failed: %v", err)
	}

	if _, err := Fetch(ctx, rootA, ""); err != nil {
		t.Fatalf("Fetch default remote failed: %v", err)
	}
	ahead, behind, upstream, err = AheadBehind(ctx, rootA)
	if err != nil || ahead != 1 || behind != 1 || upstream != "origin/main" {
		t.Fatalf("expected diverged 1/1, got ahead=%d behind=%d upstream=%q err=%v", ahead, behind, upstream, err)
	}

	// PullFFOnly must refuse diverged history even if user configured pull.rebase=true.
	runA("config", "pull.rebase", "true")
	if _, err := PullFFOnly(ctx, rootA, "origin", "main"); err == nil || !strings.Contains(err.Error(), "快进拉取") {
		t.Fatalf("expected PullFFOnly failure on diverged history, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootA, "remote-b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed PullFFOnly mutated working tree")
	}

	// Non-fast-forward Push must fail (never force-pushes).
	if _, err := Push(ctx, rootA, "origin", "main", false); err == nil {
		t.Fatal("diverged Push succeeded unexpectedly")
	}
}

func TestPullFFOnlyAndRemoteOpsRefusePendingMergeOrRebaseAndUnsafeInputs(t *testing.T) {
	ctx := context.Background()
	root, run := repository(t)
	bare := bareRemote(t)
	run("remote", "add", "origin", bare)
	if _, err := Push(ctx, root, "origin", "main", true); err != nil {
		t.Fatal(err)
	}

	for _, marker := range []struct {
		name  string
		isDir bool
	}{
		{name: "MERGE_HEAD", isDir: false},
		{name: "rebase-merge", isDir: true},
	} {
		t.Run(marker.name, func(t *testing.T) {
			target := filepath.Join(root, ".git", marker.name)
			if marker.isDir {
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, target, "pending\n")
			}
			defer os.RemoveAll(target)

			if _, err := PullFFOnly(ctx, root, "origin", "main"); err == nil || !strings.Contains(err.Error(), marker.name) {
				t.Fatalf("PullFFOnly did not diagnose pending %s: %v", marker.name, err)
			}
			if _, err := Push(ctx, root, "origin", "main", false); err == nil || !strings.Contains(err.Error(), marker.name) {
				t.Fatalf("Push did not diagnose pending %s: %v", marker.name, err)
			}
			if _, err := StashPush(ctx, root, "wip", true); err == nil || !strings.Contains(err.Error(), marker.name) {
				t.Fatalf("StashPush did not diagnose pending %s: %v", marker.name, err)
			}
			if _, err := StashPop(ctx, root); err == nil || !strings.Contains(err.Error(), marker.name) {
				t.Fatalf("StashPop did not diagnose pending %s: %v", marker.name, err)
			}
		})
	}

	// Reject dangerous remote/branch arguments and ext:: remote helper URLs.
	for _, badRemote := range []string{"--upload-pack=evil", "missing-remote", "ext::sh -c evil"} {
		if _, err := Fetch(ctx, root, badRemote); err == nil {
			t.Fatalf("Fetch accepted invalid remote %q", badRemote)
		}
	}
	run("remote", "add", "unsafe", "ext::sh -c echo% p")
	if _, err := Fetch(ctx, root, "unsafe"); err == nil || !strings.Contains(err.Error(), "helper") {
		t.Fatalf("Fetch accepted ext:: helper remote: %v", err)
	}
	for _, badBranch := range []string{"--force", "+main", ":main", "../escape"} {
		if _, err := Push(ctx, root, "origin", badBranch, false); err == nil {
			t.Fatalf("Push accepted invalid branch %q", badBranch)
		}
	}

	// Detached HEAD behavior.
	run("switch", "--detach")
	ahead, behind, upstream, err := AheadBehind(ctx, root)
	if err != nil || ahead != 0 || behind != 0 || upstream != "" {
		t.Fatalf("AheadBehind on detached HEAD: %d %d %q %v", ahead, behind, upstream, err)
	}
	if _, err := PullFFOnly(ctx, root, "origin", "main"); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("PullFFOnly accepted detached HEAD: %v", err)
	}
}

func TestStashPushListPopAndLockBoundaries(t *testing.T) {
	ctx := context.Background()
	root, _ := repository(t)

	// Empty stash list returns non-nil empty slice; StashPop & clean StashPush reject cleanly.
	list, err := StashList(ctx, root)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("expected empty stash slice, got %#v, %v", list, err)
	}
	if _, err := StashPop(ctx, root); err == nil || !strings.Contains(err.Error(), "没有可恢复") {
		t.Fatalf("expected StashPop error on empty stack, got %v", err)
	}
	if _, err := StashPush(ctx, root, "nothing", false); err == nil || !strings.Contains(err.Error(), "没有可贮藏") {
		t.Fatalf("expected StashPush error on clean workspace, got %v", err)
	}

	// Untracked-only file is ignored when includeUntracked=false, stashed when true.
	put(t, filepath.Join(root, "untracked.txt"), "draft\n")
	if _, err := StashPush(ctx, root, "untracked-ignored", false); err == nil {
		t.Fatal("StashPush without includeUntracked accepted untracked-only workspace")
	}
	if _, err := StashPush(ctx, root, "-leading-dash-msg", true); err != nil {
		t.Fatalf("StashPush with includeUntracked failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "untracked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("untracked file still present after stash")
	}

	// Second stash with tracked edit.
	put(t, filepath.Join(root, "file.txt"), "edited tracked\n")
	if _, err := StashPush(ctx, root, "tracked-wip", false); err != nil {
		t.Fatalf("StashPush tracked edit failed: %v", err)
	}

	list, err = StashList(ctx, root)
	if err != nil || len(list) != 2 {
		t.Fatalf("expected 2 stash entries, got %+v, %v", list, err)
	}
	if list[0].Index != 0 || list[0].Ref != "stash@{0}" || list[0].Branch != "main" || !strings.Contains(list[0].Message, "tracked-wip") || list[0].CreatedAt.IsZero() {
		t.Fatalf("unexpected stash[0]: %+v", list[0])
	}
	if list[1].Index != 1 || list[1].Ref != "stash@{1}" || !strings.Contains(list[1].Message, "-leading-dash-msg") {
		t.Fatalf("unexpected stash[1]: %+v", list[1])
	}

	// Workspace lock blocks mutating StashPop while allowing read-only StashList.
	lock, err := workspace.LockDirectory(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StashList(ctx, root); err != nil {
		workspace.Unlock(lock)
		t.Fatalf("read-only StashList blocked by workspace lock: %v", err)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	_, lockErr := StashPop(shortCtx, root)
	cancel()
	workspace.Unlock(lock)
	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Fatalf("expected lock timeout on StashPop, got: %v", lockErr)
	}

	// Pop newest stash (tracked-wip), then oldest stash (untracked.txt).
	if _, err := StashPop(ctx, root); err != nil {
		t.Fatalf("first StashPop failed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(data) != "edited tracked\n" {
		t.Fatalf("tracked edit not restored: %q, %v", data, err)
	}
	// Reset tracked edit so second pop runs cleanly.
	put(t, filepath.Join(root, "file.txt"), "base\n")
	if _, err := StashPop(ctx, root); err != nil {
		t.Fatalf("second StashPop failed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "untracked.txt")); err != nil || string(data) != "draft\n" {
		t.Fatalf("untracked file not restored: %q, %v", data, err)
	}
	list, err = StashList(ctx, root)
	if err != nil || len(list) != 0 {
		t.Fatalf("expected empty stash list after popping all entries, got %+v, %v", list, err)
	}
}
