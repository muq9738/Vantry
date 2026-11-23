package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type checkpointRepo struct {
	t    *testing.T
	path string
}

func newCheckpointRepo(t *testing.T) checkpointRepo {
	t.Helper()
	r := checkpointRepo{t: t, path: t.TempDir()}
	r.git("init", "-b", "main")
	r.git("config", "user.email", "test@example.invalid")
	r.git("config", "user.name", "Vantry Test")
	r.git("config", "commit.gpgsign", "false")
	r.write("tracked.txt", "base\n")
	r.write("deleted.txt", "remove later\n")
	r.write(".gitignore", "ignored.txt\nignored-dir/\n")
	r.git("add", "-A")
	r.git("commit", "-m", "base")
	return r
}

func (r checkpointRepo) git(args ...string) string {
	r.t.Helper()
	out, err := command(context.Background(), r.path, args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.TrimSuffix(out, "\n")
}

func (r checkpointRepo) write(name, text string) {
	r.t.Helper()
	path := filepath.Join(r.path, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		r.t.Fatal(err)
	}
}

func (r checkpointRepo) read(name string) string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.path, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(data)
}

func (r checkpointRepo) index() []byte {
	r.t.Helper()
	path := r.git("rev-parse", "--git-path", "index")
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.path, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return data
}

func TestCheckpointCaptureDiffAndRestorePreserveGitState(t *testing.T) {
	ctx := context.Background()
	r := newCheckpointRepo(t)
	r.write("tracked.txt", "stashed\n")
	r.git("stash", "push", "-m", "user stash")
	r.write("tracked.txt", "staged\n")
	r.git("add", "tracked.txt")
	r.write("tracked.txt", "未暂存的中文\n")
	r.write("untracked.txt", "new at capture\n")
	r.write("ignored.txt", "private\n")
	if err := os.Remove(filepath.Join(r.path, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	index, head, branch, stash := r.index(), r.git("rev-parse", "HEAD"), r.git("symbolic-ref", "HEAD"), r.git("stash", "list")
	checkpoint, err := CaptureCheckpoint(ctx, r.path, "first", "中文快照")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.ID != "first" || checkpoint.Ref != checkpointRefPrefix+"first" || checkpoint.Label != "中文快照" || checkpoint.CreatedAt.IsZero() {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	if got := r.git("show", checkpoint.Ref+":tracked.txt"); got != "未暂存的中文" {
		t.Fatalf("captured staged content instead of worktree: %q", got)
	}
	if got := r.git("show", checkpoint.Ref+":untracked.txt"); got != "new at capture" {
		t.Fatalf("untracked file not captured: %q", got)
	}
	if files := r.git("ls-tree", "-r", "--name-only", checkpoint.Ref); strings.Contains(files, "ignored.txt") || strings.Contains(files, "deleted.txt") {
		t.Fatalf("ignored/deleted files captured: %s", files)
	}
	checkState := func() {
		t.Helper()
		if !bytes.Equal(index, r.index()) || head != r.git("rev-parse", "HEAD") || branch != r.git("symbolic-ref", "HEAD") || stash != r.git("stash", "list") {
			t.Fatal("checkpoint changed user index, HEAD, branch or stash")
		}
	}
	checkState()
	if _, err := CaptureCheckpoint(ctx, r.path, "first", "replacement"); err == nil {
		t.Fatal("duplicate checkpoint ID overwrote existing ref")
	}
	list, err := ListCheckpoints(ctx, r.path)
	if err != nil || len(list) != 1 || list[0] != checkpoint {
		t.Fatalf("checkpoint list = %+v, %v", list, err)
	}
	if diff, err := DiffCheckpoint(ctx, r.path, "first"); err != nil || diff != "" {
		t.Fatalf("unchanged checkpoint diff = %q, %v", diff, err)
	}
	r.write("tracked.txt", "after capture\n")
	r.write("later.txt", "later untracked\n")
	if err := os.Remove(filepath.Join(r.path, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	diff, err := DiffCheckpoint(ctx, r.path, "first")
	if err != nil || !strings.Contains(diff, "+after capture") || !strings.Contains(diff, "later.txt") || !strings.Contains(diff, "untracked.txt") {
		t.Fatalf("worktree diff = %q, %v", diff, err)
	}
	checkState()
	result, err := RestoreCheckpoint(ctx, r.path, "first", CheckpointRestoreOptions{AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Backup.ID, "pre-restore-") {
		t.Fatalf("missing pre-restore backup: %+v", result)
	}
	if r.read("tracked.txt") != "未暂存的中文\n" || r.read("untracked.txt") != "new at capture\n" || r.read("ignored.txt") != "private\n" {
		t.Fatal("restore lost snapshot or ignored contents")
	}
	if _, err := os.Stat(filepath.Join(r.path, "later.txt")); !os.IsNotExist(err) {
		t.Fatalf("post-checkpoint untracked file not removed: %v", err)
	}
	checkState()
	if _, err := RestoreCheckpoint(ctx, r.path, result.Backup.ID, CheckpointRestoreOptions{AllowPrimaryCheckout: true}); err != nil {
		t.Fatal(err)
	}
	if r.read("tracked.txt") != "after capture\n" || r.read("later.txt") != "later untracked\n" {
		t.Fatal("pre-restore backup cannot undo restoration")
	}
	checkState()
}

func TestCheckpointRefusesPrimaryCheckoutAndPendingOperations(t *testing.T) {
	ctx := context.Background()
	r := newCheckpointRepo(t)
	if _, err := CaptureCheckpoint(ctx, r.path, "safe", ""); err != nil {
		t.Fatal(err)
	}
	r.write("tracked.txt", "must survive\n")
	if _, err := RestoreCheckpoint(ctx, r.path, "safe", CheckpointRestoreOptions{}); err == nil || !strings.Contains(err.Error(), "AllowPrimaryCheckout") {
		t.Fatalf("primary checkout restore = %v", err)
	}
	gitDir := r.git("rev-parse", "--absolute-git-dir")
	for _, operation := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "rebase-merge", "rebase-apply", "sequencer"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(gitDir, operation)
			if err := os.WriteFile(path, []byte("pending"), 0600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			if _, err := RestoreCheckpoint(ctx, r.path, "safe", CheckpointRestoreOptions{AllowPrimaryCheckout: true}); err == nil || !strings.Contains(err.Error(), operation) {
				t.Fatalf("pending operation restore = %v", err)
			}
			if r.read("tracked.txt") != "must survive\n" {
				t.Fatal("rejected restore overwrote files")
			}
		})
	}
	list, err := ListCheckpoints(ctx, r.path)
	if err != nil || len(list) != 1 {
		t.Fatalf("rejected restores created backups: %+v %v", list, err)
	}
}

func TestCheckpointRestoreLinkedWorktree(t *testing.T) {
	ctx := context.Background()
	primary := newCheckpointRepo(t)
	path := filepath.Join(t.TempDir(), "linked")
	primary.git("worktree", "add", "-b", "vantry/test", path)
	r := checkpointRepo{t: t, path: path}
	r.write("tracked.txt", "worktree checkpoint\n")
	if _, err := CaptureCheckpoint(ctx, path, "linked", "linked"); err != nil {
		t.Fatal(err)
	}
	r.write("tracked.txt", "worktree later\n")
	if _, err := RestoreCheckpoint(ctx, path, "linked", CheckpointRestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.read("tracked.txt") != "worktree checkpoint\n" || primary.read("tracked.txt") != "base\n" {
		t.Fatal("linked worktree restore affected primary checkout")
	}
}

func TestCheckpointRestoreProtectsIgnoredObstructions(t *testing.T) {
	ctx := context.Background()
	r := newCheckpointRepo(t)
	r.write("ignored.txt", "tracked target\n")
	r.git("add", "-f", "ignored.txt")
	r.git("commit", "-m", "track previously ignored path")
	if _, err := CaptureCheckpoint(ctx, r.path, "target", ""); err != nil {
		t.Fatal(err)
	}
	r.git("rm", "ignored.txt")
	r.git("commit", "-m", "untrack ignored path")
	r.write("ignored.txt", "private data outside snapshot\n")
	result, err := RestoreCheckpoint(ctx, r.path, "target", CheckpointRestoreOptions{AllowPrimaryCheckout: true})
	if err == nil {
		t.Fatal("restore overwrote an ignored file absent from the backup")
	}
	if result.Backup.Ref == "" || r.read("ignored.txt") != "private data outside snapshot\n" {
		t.Fatalf("obstructed restore did not preserve ignored data and backup: %+v %v", result, err)
	}
}

func TestCheckpointCapturesIgnoredStagedAdditionsAndAssumedUnchanged(t *testing.T) {
	ctx := context.Background()
	r := newCheckpointRepo(t)
	r.git("update-index", "--assume-unchanged", "tracked.txt")
	r.write("tracked.txt", "changed despite index flag\n")
	r.write("ignored.txt", "staged ignored file\n")
	r.git("add", "-f", "ignored.txt")
	index := r.index()
	checkpoint, err := CaptureCheckpoint(ctx, r.path, "staged-ignored", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.git("show", checkpoint.Ref+":tracked.txt") != "changed despite index flag" || r.git("show", checkpoint.Ref+":ignored.txt") != "staged ignored file" {
		t.Fatal("capture omitted a tracked working file")
	}
	if !bytes.Equal(index, r.index()) {
		t.Fatal("capture cleared flags or changed the user's index")
	}
}

func TestCheckpointRespectsIgnoredStagedDeletion(t *testing.T) {
	ctx := context.Background()
	r := newCheckpointRepo(t)
	r.write("ignored.txt", "once tracked\n")
	r.git("add", "-f", "ignored.txt")
	r.git("commit", "-m", "tracked ignored path")
	r.git("rm", "--cached", "ignored.txt")
	r.write("ignored.txt", "now private and untracked\n")
	index := r.index()
	checkpoint, err := CaptureCheckpoint(ctx, r.path, "staged-deletion", "")
	if err != nil {
		t.Fatal(err)
	}
	if files := r.git("ls-tree", "-r", "--name-only", checkpoint.Ref); strings.Contains(files, "ignored.txt") {
		t.Fatalf("capture ignored the current index's staged deletion: %s", files)
	}
	if !bytes.Equal(index, r.index()) || r.read("ignored.txt") != "now private and untracked\n" {
		t.Fatal("capture changed a staged deletion or an ignored file")
	}
}

func TestCheckpointUnbornRepositoryAndInvalidIDs(t *testing.T) {
	ctx := context.Background()
	r := checkpointRepo{t: t, path: t.TempDir()}
	r.git("init", "-b", "main")
	r.write("中文.txt", "初始内容\n")
	if _, err := CaptureCheckpoint(ctx, r.path, "unborn", "initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.path, ".git", "index")); !os.IsNotExist(err) {
		t.Fatalf("capture created user index in unborn repo: %v", err)
	}
	for _, id := range []string{"", "../main", "x/y", "bad..id", "bad\nref", "HEAD@{1}"} {
		if _, err := CaptureCheckpoint(ctx, r.path, id, "bad"); err == nil {
			t.Fatalf("invalid ID %q accepted", id)
		}
	}
	if _, err := DiffCheckpoint(ctx, r.path, "missing"); err == nil {
		t.Fatal("missing checkpoint diff accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := CaptureCheckpoint(cancelled, r.path, "cancelled", ""); err == nil {
		t.Fatal("cancelled capture succeeded")
	}
}

func TestRunCheckpointIDAndIsGitWorkspace(t *testing.T) {
	ctx := context.Background()
	r := newCheckpointRepo(t)
	nested := filepath.Join(r.path, "sub", "dir")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if !IsGitWorkspace(r.path) || !IsGitWorkspace(nested) {
		t.Fatalf("expected git repo and nested dir to be detected")
	}
	plain := t.TempDir()
	if IsGitWorkspace(plain) || IsGitWorkspace("") {
		t.Fatalf("expected non-git and empty dirs to return false")
	}

	for _, raw := range []string{"run_123", "run_th1:run_src", "../a..b/c", "", "foo.lock", "HEAD@{1}"} {
		id := RunCheckpointID(raw)
		if id == "" || strings.Contains(id, "/") || strings.Contains(id, "..") {
			t.Fatalf("RunCheckpointID(%q) = %q invalid", raw, id)
		}
		cp, err := CaptureCheckpoint(ctx, r.path, id, "turn snapshot "+id)
		if err != nil {
			t.Fatalf("CaptureCheckpoint(%q -> %q): %v", raw, id, err)
		}
		if cp.ID != id {
			t.Fatalf("captured ID = %q, want %q", cp.ID, id)
		}
	}
}
