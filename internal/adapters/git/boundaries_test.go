package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

func repository(t *testing.T) (string, func(...string) string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "absent-config"))
	run := func(args ...string) string {
		t.Helper()
		out, err := command(context.Background(), root, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	run("init", "-b", "main")
	run("config", "user.name", "Vantry Test")
	run("config", "user.email", "test@example.invalid")
	run("config", "commit.gpgsign", "false")
	put(t, filepath.Join(root, "file.txt"), "base\n")
	run("add", "file.txt")
	run("commit", "-m", "base")
	return root, run
}

func put(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func feature(t *testing.T, root string, run func(...string) string, diverge bool) {
	t.Helper()
	run("switch", "-c", "feature")
	put(t, filepath.Join(root, "feature.txt"), "feature\n")
	run("add", "feature.txt")
	run("commit", "-m", "feature")
	run("switch", "main")
	if diverge {
		put(t, filepath.Join(root, "main.txt"), "main\n")
		run("add", "main.txt")
		run("commit", "-m", "main")
	}
}

func hook(t *testing.T, root string, run func(...string) string, name, body string) {
	t.Helper()
	directory := filepath.Join(root, ".git", "test-hooks")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run("config", "core.hooksPath", directory)
}

func TestInspectSeparatesIndexAndWorkingTreeAndReportsUntracked(t *testing.T) {
	root, run := repository(t)
	put(t, filepath.Join(root, "file.txt"), "staged\n")
	run("add", "file.txt")
	put(t, filepath.Join(root, "file.txt"), "base\n")
	put(t, filepath.Join(root, "untracked.txt"), "personal\n")
	run("config", "status.showUntrackedFiles", "no")
	state, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.Diff, "+staged") || !strings.Contains(state.Diff, "-staged") {
		t.Fatalf("staged and unstaged changes cancelled out: %q", state.Diff)
	}
	if !strings.Contains(state.Status, "?? untracked.txt") || !strings.Contains(state.Diff, "未跟踪") {
		t.Fatalf("untracked content was hidden: %+v", state)
	}
}

func TestInspectBoundsLargeDiffWithVisibleNotice(t *testing.T) {
	for _, test := range []struct{ name, content string }{
		{"many-lines", strings.Repeat("x\n", 12000)},
		{"long-line", strings.Repeat("x", 2<<20) + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _ := repository(t)
			put(t, filepath.Join(root, "file.txt"), test.content)
			captured, err := command(context.Background(), root, "diff", "--no-color", "--", "file.txt")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(captured, "已截断") || len(captured) > (1<<20)+1024 || strings.Count(captured, "\n") > 2001 {
				t.Fatal("Git subprocess capture grew beyond its display limit")
			}
			state, err := Inspect(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(state.Diff, "已截断") {
				t.Fatal("oversized diff has no truncation notice")
			}
			if len(state.Diff) > (1<<20)+1024 || strings.Count(state.Diff, "\n") > 2100 {
				t.Fatalf("unbounded display: %d bytes, %d lines", len(state.Diff), strings.Count(state.Diff, "\n"))
			}
		})
	}
}

func TestMergeRefusesHiddenUntrackedFiles(t *testing.T) {
	root, run := repository(t)
	feature(t, root, run, false)
	put(t, filepath.Join(root, "personal.txt"), "keep\n")
	run("config", "status.showUntrackedFiles", "no")
	if _, err := Merge(context.Background(), root, "feature"); err == nil {
		t.Fatal("merge accepted a dirty workspace hidden by status configuration")
	}
	if _, err := os.Stat(filepath.Join(root, "feature.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected merge changed the target")
	}
}

func TestMergeLocksCheckoutRootFromSubdirectory(t *testing.T) {
	root, run := repository(t)
	feature(t, root, run, false)
	subdirectory := filepath.Join(root, "nested")
	if err := os.Mkdir(subdirectory, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := workspace.LockDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Unlock(lock)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := Merge(ctx, subdirectory, "feature"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("root lock/cancellation was not respected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "feature.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("occupied checkout was changed")
	}
}

func TestGitUsesRequestedCheckoutDespiteInheritedRouting(t *testing.T) {
	root, run := repository(t)
	feature(t, root, run, false)
	other, otherRun := repository(t)
	otherRun("branch", "-m", "other")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	state, err := Inspect(context.Background(), root)
	if err != nil || state.Branch != "main" {
		t.Fatalf("wrong checkout: %+v, %v", state, err)
	}
	if _, err := Merge(context.Background(), root, "feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "feature.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("merge touched the other checkout")
	}
}

func TestFailedHookKeepsItsStagedEdits(t *testing.T) {
	root, run := repository(t)
	feature(t, root, run, true)
	hook(t, root, run, "pre-merge-commit", "printf 'hook edit\\n' > file.txt\ngit add file.txt\necho 'hook rejected merge' >&2\nexit 17")
	_, err := Merge(context.Background(), root, "feature")
	if err == nil || !strings.Contains(err.Error(), "hook rejected merge") {
		t.Fatalf("hook failure was not reported: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "file.txt"))
	if err != nil || string(data) != "hook edit\n" {
		t.Fatalf("hook edit was discarded: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "MERGE_HEAD")); err != nil {
		t.Fatalf("failed hook's pending merge was discarded: %v", err)
	}
}

func TestMergeDoesNotReportNoCommitOrSquashAsCompleted(t *testing.T) {
	for _, option := range []string{"--no-commit", "--squash"} {
		t.Run(option, func(t *testing.T) {
			root, run := repository(t)
			feature(t, root, run, true)
			run("config", "branch.main.mergeOptions", option)
			if _, err := Merge(context.Background(), root, "feature"); err == nil {
				t.Fatal("incomplete merge reported success")
			}
			if _, err := os.Stat(filepath.Join(root, "feature.txt")); err != nil {
				t.Fatal("incomplete merge edits were discarded")
			}
		})
	}
}

func TestMergeValidatesLocalSourceAndTarget(t *testing.T) {
	root, run := repository(t)
	feature(t, root, run, false)
	run("tag", "tag-only", "feature")
	for _, source := range []string{"", "--ff", "missing", "tag-only", "main", "HEAD", "@{-1}", "feature~1", "../feature"} {
		if _, err := Merge(context.Background(), root, source); err == nil {
			t.Errorf("invalid source accepted: %q", source)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "feature.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid source changed the target")
	}
	run("switch", "--detach")
	if _, err := Merge(context.Background(), root, "feature"); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("detached target accepted: %v", err)
	}
}

func TestInspectAndMergeUseLinkedWorktree(t *testing.T) {
	root, run := repository(t)
	feature(t, root, run, false)
	worktree := filepath.Join(t.TempDir(), "isolated")
	run("worktree", "add", "-b", "isolated", worktree, "main")
	state, err := Inspect(context.Background(), worktree)
	if err != nil || state.Branch != "isolated" {
		t.Fatalf("linked worktree status: %+v, %v", state, err)
	}
	if _, err := Merge(context.Background(), worktree, "feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "feature.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "feature.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("merge changed the main checkout")
	}
}

func TestMergePreservesIgnoredFilesAndUnfinishedOperations(t *testing.T) {
	t.Run("ignored-file", func(t *testing.T) {
		root, run := repository(t)
		feature(t, root, run, false)
		put(t, filepath.Join(root, ".git", "info", "exclude"), "feature.txt\n")
		put(t, filepath.Join(root, "feature.txt"), "ignored personal work\n")
		if _, err := Merge(context.Background(), root, "feature"); err == nil {
			t.Fatal("ignored file was overwritten")
		}
		data, err := os.ReadFile(filepath.Join(root, "feature.txt"))
		if err != nil || string(data) != "ignored personal work\n" {
			t.Fatalf("ignored content changed: %q, %v", data, err)
		}
	})
	t.Run("clean-paused-operation", func(t *testing.T) {
		root, run := repository(t)
		feature(t, root, run, false)
		if err := os.Mkdir(filepath.Join(root, ".git", "rebase-merge"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := Merge(context.Background(), root, "feature"); err == nil || !strings.Contains(err.Error(), "rebase") {
			t.Fatalf("paused operation accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".git", "rebase-merge")); err != nil {
			t.Fatal("preexisting operation was removed")
		}
	})
}

func TestMergeRespectsSigningAndFastForwardConfiguration(t *testing.T) {
	t.Run("signing", func(t *testing.T) {
		root, run := repository(t)
		feature(t, root, run, true)
		program := filepath.Join(root, ".git", "reject-signing")
		if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf invoked > .git/signing-called\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
		run("config", "gpg.program", program)
		run("config", "commit.gpgsign", "true")
		if _, err := Merge(context.Background(), root, "feature"); err == nil {
			t.Fatal("failed signing reported success")
		}
		if _, err := os.Stat(filepath.Join(root, ".git", "signing-called")); err != nil {
			t.Fatal("signing was bypassed")
		}
		if run("config", "commit.gpgsign") != "true" {
			t.Fatal("signing configuration changed")
		}
	})
	t.Run("ff-only", func(t *testing.T) {
		root, run := repository(t)
		feature(t, root, run, true)
		run("config", "merge.ff", "only")
		if _, err := Merge(context.Background(), root, "feature"); err == nil {
			t.Fatal("merge.ff=only was bypassed")
		}
		if run("status", "--porcelain") != "" {
			t.Fatal("failed ff-only merge changed files")
		}
	})
}

func TestMergeReportsPostHookState(t *testing.T) {
	t.Run("changed-branch", func(t *testing.T) {
		root, run := repository(t)
		feature(t, root, run, false)
		hook(t, root, run, "post-merge", "git switch feature")
		if _, err := Merge(context.Background(), root, "feature"); err == nil {
			t.Fatal("hook changed the target without a warning")
		}
		if run("branch", "--show-current") != "feature" {
			t.Fatal("hook branch change was rolled back")
		}
	})
	t.Run("dirty-after-commit", func(t *testing.T) {
		root, run := repository(t)
		feature(t, root, run, false)
		hook(t, root, run, "post-merge", "printf 'hook edit\\n' > file.txt")
		output, err := Merge(context.Background(), root, "feature")
		if err != nil || !strings.Contains(output, "已保留") {
			t.Fatalf("committed merge with hook edits: %s, %v", output, err)
		}
		data, err := os.ReadFile(filepath.Join(root, "file.txt"))
		if err != nil || string(data) != "hook edit\n" {
			t.Fatalf("hook edit changed: %q, %v", data, err)
		}
	})
}

func TestInspectUnbornRepository(t *testing.T) {
	root, run := repository(t)
	run("switch", "--orphan", "unborn")
	put(t, filepath.Join(root, "first.txt"), "staged\n")
	run("add", "first.txt")
	put(t, filepath.Join(root, "first.txt"), "unstaged\n")
	state, err := Inspect(context.Background(), root)
	if err != nil || state.Branch != "unborn" || !strings.Contains(state.Diff, "+staged") || !strings.Contains(state.Diff, "+unstaged") {
		t.Fatalf("unborn diff: %+v, %v", state, err)
	}
	if _, err := Merge(context.Background(), root, "main"); err == nil {
		t.Fatal("unborn target accepted")
	}
}

func TestCancelledMergeStopsHookAndPreservesCancellation(t *testing.T) {
	for _, name := range []string{"pre-merge-commit", "post-merge"} {
		t.Run(name, func(t *testing.T) { cancelledMergeAtHook(t, name) })
	}
}

func cancelledMergeAtHook(t *testing.T, name string) {
	root, run := repository(t)
	feature(t, root, run, true)
	hook(t, root, run, name, "echo $$ > .git/hook.pid\nexec sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := Merge(ctx, root, "feature")
		finished <- err
	}()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var pid int
waitHook:
	for {
		select {
		case err := <-finished:
			t.Fatalf("merge ended before hook: %v", err)
		case <-deadline:
			t.Fatal("hook did not start")
		case <-tick.C:
			data, err := os.ReadFile(filepath.Join(root, ".git", "hook.pid"))
			if err == nil {
				pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
				if err == nil && pid > 0 {
					break waitHook
				}
			}
		}
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		if name == "post-merge" && !strings.Contains(err.Error(), "来源分支已合入") {
			t.Fatalf("cancelled operation concealed a completed merge: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled merge did not return")
	}
	// An orphan hook must not keep writing after the checkout lock is released.
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("cancelled hook is still running: %v", err)
	}
	state, err := Inspect(context.Background(), root)
	if err != nil || state.Branch != "main" || state.Conflicts {
		t.Fatalf("status after cancellation: %+v, %v", state, err)
	}
	if name == "post-merge" && state.Status != "" {
		t.Fatalf("cancelled post-merge hook changed committed files: %+v", state)
	}
}
