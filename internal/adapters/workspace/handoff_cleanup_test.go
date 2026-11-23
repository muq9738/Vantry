package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestHandoffPreservesStagedUnstagedUntrackedAndBinaryChanges(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	for name, data := range map[string][]byte{
		".gitignore": []byte("ignored/\n.env\n"), "deleted.txt": []byte("delete\n"),
		"old-name.txt": []byte("rename\n"), "binary.bin": {0, 1, 2}, "exec.sh": []byte("echo base\n"),
	} {
		handoffWriteTest(t, source, name, data)
	}
	runGitTest(t, source, "add", ".")
	runGitTest(t, source, "commit", "-m", "fixtures")
	dataDir := filepath.Join(root, "data")
	if _, err := Prepare(context.Background(), dataDir, api.WorkspaceSpec{Kind: KindDirectory, Path: source}, "session"); err != nil {
		t.Fatal(err)
	}
	handoffWriteTest(t, source, "README.md", []byte("staged\n"))
	handoffWriteTest(t, source, "binary.bin", []byte{0, 2, 3})
	handoffWriteTest(t, source, "new-staged.txt", []byte("new staged\n"))
	runGitTest(t, source, "add", "README.md", "binary.bin", "new-staged.txt")
	handoffWriteTest(t, source, "README.md", []byte("staged\nunstaged\n"))
	handoffWriteTest(t, source, "binary.bin", []byte{0, 2, 3, 4, 0})
	handoffWriteTest(t, source, "new-staged.txt", []byte("new staged\nunstaged too\n"))
	runGitTest(t, source, "rm", "deleted.txt")
	runGitTest(t, source, "mv", "old-name.txt", "renamed.txt")
	if err := os.Chmod(filepath.Join(source, "exec.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"untracked/file with spaces\nand newline.txt", ":(literal)*.txt", "ignored/personal.txt", ".env"} {
		handoffWriteTest(t, source, name, []byte("untracked "+name+"\n"))
	}
	if err := os.Symlink("README.md", filepath.Join(source, "new-link")); err != nil {
		t.Fatal(err)
	}
	before, err := captureHandoff(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := HandoffToWorktree(context.Background(), dataDir, "session", source, "feature/handoff", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != KindWorktree || samePath(spec.Path, source) || spec.Branch != "feature/handoff" || spec.BaseRef != "HEAD" {
		t.Fatalf("handoff spec = %+v", spec)
	}
	if err := verifyHandoff(context.Background(), spec.Path, before); err != nil {
		t.Fatal(err)
	}
	after, err := captureHandoff(context.Background(), spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Staged, after.Staged) || !bytes.Equal(before.Unstaged, after.Unstaged) || !reflect.DeepEqual(before.Untracked, after.Untracked) {
		t.Fatalf("Git changes lost their staged/unstaged distinction\nbefore: %s\nafter: %s", before.Staged, after.Staged)
	}
	if got := handoffReadGitTest(t, source, "status", "--porcelain=v1", "--untracked-files=all"); len(got) != 0 {
		t.Fatalf("source did not become clean: %s", got)
	}
	if got := strings.TrimSpace(string(handoffReadGitTest(t, source, "symbolic-ref", "--short", "HEAD"))); got != "main" {
		t.Fatalf("source branch changed: %s", got)
	}
	for _, name := range []string{"ignored/personal.txt", ".env"} {
		if _, err := os.Stat(filepath.Join(source, name)); err != nil {
			t.Fatalf("ignored source data was removed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(spec.Path, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ignored data copied to target: %v", err)
		}
	}
	resolved, err := Existing(context.Background(), dataDir, spec, "session")
	if err != nil || resolved != spec.Path {
		t.Fatalf("prepared ownership was not published: %s, %v", resolved, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(spec.Path), "handoff-source.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful handoff retained recovery data: %v", err)
	}
}

func TestHandoffWithoutMovingLeavesSourceChangesAndTargetClean(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	handoffWriteTest(t, source, "README.md", []byte("keep source dirty\n"))
	handoffWriteTest(t, source, "untracked.txt", []byte("keep source untracked\n"))
	before, err := captureHandoff(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := HandoffToWorktree(context.Background(), filepath.Join(root, "data"), "session", source, "feature/clean", "main", false)
	if err != nil {
		t.Fatal(err)
	}
	assertHandoffSourceTest(t, source, before)
	if got := handoffReadGitTest(t, spec.Path, "status", "--porcelain"); len(got) != 0 {
		t.Fatalf("target unexpectedly copied source changes: %s", got)
	}
}

func TestHandoffExplicitBaseAndStagedRemovalWithUntrackedReplacement(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	runGitTest(t, source, "branch", "base")
	handoffWriteTest(t, source, "source-only.txt", []byte("committed after base\n"))
	runGitTest(t, source, "add", ".")
	runGitTest(t, source, "commit", "-m", "source-only commit")
	runGitTest(t, source, "rm", "--cached", "README.md")
	handoffWriteTest(t, source, "README.md", []byte("untracked replacement\n"))
	spec, err := HandoffToWorktree(context.Background(), filepath.Join(root, "data"), "session", source, "feature/base", "base", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spec.Path, "source-only.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit base ignored: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(source, "README.md")); err != nil || string(data) != "test\n" {
		t.Fatalf("source HEAD file was removed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(spec.Path, "README.md")); err != nil || string(data) != "untracked replacement\n" {
		t.Fatalf("replacement did not move: %q, %v", data, err)
	}
	if got := handoffReadGitTest(t, source, "status", "--porcelain"); len(got) != 0 {
		t.Fatalf("source not clean: %s", got)
	}
}

func TestHandoffApplyConflictPreservesSourceAndOriginalDirectoryIntent(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	runGitTest(t, source, "branch", "old-base")
	handoffWriteTest(t, source, "README.md", []byte("committed new baseline\n"))
	runGitTest(t, source, "add", ".")
	runGitTest(t, source, "commit", "-m", "new baseline")
	handoffWriteTest(t, source, "README.md", []byte("staged on new baseline\n"))
	runGitTest(t, source, "add", "README.md")
	handoffWriteTest(t, source, "untracked.txt", []byte("keep\n"))
	before, err := captureHandoff(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	directory, err := Prepare(context.Background(), dataDir, api.WorkspaceSpec{Kind: KindDirectory, Path: source}, "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HandoffToWorktree(context.Background(), dataDir, "session", source, "feature/conflict", "old-base", true); err == nil {
		t.Fatal("conflicting patch applied")
	}
	assertHandoffSourceTest(t, source, before)
	if _, err := Existing(context.Background(), dataDir, directory, "session"); err != nil {
		t.Fatalf("failed handoff broke original directory intent: %v", err)
	}
	if exists, err := branchExists(context.Background(), source, "feature/conflict"); err != nil || exists {
		t.Fatalf("failed target branch was not rolled back: %v, %v", exists, err)
	}
}

func TestHandoffSourceIndexLockFailureNeverClearsChanges(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	handoffWriteTest(t, source, "README.md", []byte("staged\n"))
	runGitTest(t, source, "add", "README.md")
	handoffWriteTest(t, source, "README.md", []byte("staged\nunstaged\n"))
	handoffWriteTest(t, source, "untracked.txt", []byte("keep\n"))
	before, err := captureHandoff(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	handoffWriteTest(t, source, ".git/index.lock", []byte("another Git operation"))
	spec, err := HandoffToWorktree(context.Background(), filepath.Join(root, "data"), "session", source, "feature/index-lock", "", true)
	if err == nil || spec.Path == "" {
		t.Fatalf("expected source-clean failure and retained prepared copy: %+v, %v", spec, err)
	}
	assertHandoffSourceTest(t, source, before)
	if err := verifyHandoff(context.Background(), spec.Path, before); err != nil {
		t.Fatalf("target recovery copy lost: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(source, ".git/index.lock")); err != nil || string(data) != "another Git operation" {
		t.Fatalf("another Git operation's lock was removed: %q, %v", data, err)
	}
}

func TestHandoffRollsBackSourceFilesAfterPartialCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires directory permission enforcement")
	}
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	handoffWriteTest(t, source, "README.md", []byte("staged\n"))
	runGitTest(t, source, "add", "README.md")
	handoffWriteTest(t, source, "README.md", []byte("staged\nunstaged\n"))
	handoffWriteTest(t, source, "protected/untracked.txt", []byte("cannot remove\n"))
	protected := filepath.Join(source, "protected")
	if err := os.Chmod(protected, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(protected, 0o700) })
	before, err := captureHandoff(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := HandoffToWorktree(context.Background(), filepath.Join(root, "data"), "session", source, "feature/rollback", "", true)
	if err == nil || spec.Path == "" {
		t.Fatalf("expected permission failure with retained copy: %+v, %v", spec, err)
	}
	assertHandoffSourceTest(t, source, before)
	if err := verifyHandoff(context.Background(), spec.Path, before); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(spec.Path), "handoff-source.json")); err != nil {
		t.Fatalf("durable recovery copy missing: %v", err)
	}
}

func TestHandoffPreservesHookCreatedTargetFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	handoffWriteTest(t, source, "README.md", []byte("source changes\n"))
	hookDir := filepath.Join(source, ".git", "hooks")
	runGitTest(t, source, "config", "core.hooksPath", hookDir)
	handoffWriteTest(t, hookDir, "post-checkout", []byte("#!/bin/sh\nprintf 'hook data\\n' > hook-file.txt\n"))
	if err := os.Chmod(filepath.Join(hookDir, "post-checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	if _, err := HandoffToWorktree(context.Background(), dataDir, "session", source, "feature/hook", "", true); err == nil || !strings.Contains(err.Error(), "hook-created changes") {
		t.Fatalf("dirty hook target was not protected: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dataDir, "workspaces", "session", "worktree", "hook-file.txt")); err != nil || string(data) != "hook data\n" {
		t.Fatalf("hook-created data lost: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(source, "README.md")); err != nil || string(data) != "source changes\n" {
		t.Fatalf("source changed: %q, %v", data, err)
	}
	if err := CleanupManagedWorkspace(context.Background(), dataDir, "session", false); !errors.Is(err, ErrWorkspaceDirty) {
		t.Fatalf("incomplete handoff did not protect hook data: %v", err)
	}
}

func TestHandoffDetectsSourceChangesDuringTargetPreparation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	handoffWriteTest(t, source, "README.md", []byte("original dirty\n"))
	t.Setenv("LANE_HANDOFF_SOURCE", source)
	hookDir := filepath.Join(source, ".git", "hooks")
	runGitTest(t, source, "config", "core.hooksPath", hookDir)
	handoffWriteTest(t, hookDir, "post-checkout", []byte("#!/bin/sh\nprintf 'concurrent edit\\n' > \"$LANE_HANDOFF_SOURCE/README.md\"\n"))
	if err := os.Chmod(filepath.Join(hookDir, "post-checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec, err := HandoffToWorktree(context.Background(), filepath.Join(root, "data"), "session", source, "feature/source-changed", "", true)
	if err == nil || !strings.Contains(err.Error(), "source changed during handoff") {
		t.Fatalf("concurrent source edit was not detected: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(source, "README.md")); err != nil || string(data) != "concurrent edit\n" {
		t.Fatalf("concurrent source edit lost: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(spec.Path, "README.md")); err != nil || string(data) != "original dirty\n" {
		t.Fatalf("original copied edit lost: %q, %v", data, err)
	}
}

func TestHandoffRefusesMetadataInsideSourceBeforeCreatingIt(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	dataDir := filepath.Join(source, "data")
	if _, err := HandoffToWorktree(context.Background(), dataDir, "session", source, "feature/nested", "", true); err == nil {
		t.Fatal("handoff metadata inside source was accepted")
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected handoff created nested data directory: %v", err)
	}
}

func TestHandoffRejectsActiveSourceAndUnfinishedOperations(t *testing.T) {
	for _, operation := range []string{"directory-lock", "MERGE_HEAD", "CHERRY_PICK_HEAD", "rebase-merge", "rebase-apply"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "repo")
			initRepository(t, source)
			handoffWriteTest(t, source, "README.md", []byte("preserve dirty\n"))
			if operation == "directory-lock" {
				lock, err := LockDirectory(context.Background(), source)
				if err != nil {
					t.Fatal(err)
				}
				defer Unlock(lock)
			} else if strings.HasPrefix(operation, "rebase-") {
				if err := os.Mkdir(filepath.Join(source, ".git", operation), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				handoffWriteTest(t, source, ".git/"+operation, handoffReadGitTest(t, source, "rev-parse", "HEAD"))
			}
			if _, err := HandoffToWorktree(context.Background(), filepath.Join(root, "data"), "session", source, "feature/rejected", "", true); err == nil {
				t.Fatal("unsafe handoff accepted")
			}
			if data, err := os.ReadFile(filepath.Join(source, "README.md")); err != nil || string(data) != "preserve dirty\n" {
				t.Fatalf("unsafe handoff changed source: %q, %v", data, err)
			}
		})
	}
}

func TestManagedCleanupRefusesFileAndDirectoryLocksEvenWithForce(t *testing.T) {
	for _, kind := range []string{"workspace.lock", "prepare.lock", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dataDir, spec := managedCleanupFixture(t)
			var lock *os.File
			var err error
			if kind == "directory" {
				lock, err = LockDirectory(context.Background(), spec.Path)
			} else {
				lock, err = lockFile(context.Background(), filepath.Join(filepath.Dir(spec.Path), kind))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer Unlock(lock)
			status, err := InspectManagedWorkspace(context.Background(), dataDir, "owner")
			if err != nil || !status.Exists || !status.Locked {
				t.Fatalf("locked inspection = %+v, %v", status, err)
			}
			for _, force := range []bool{false, true} {
				start := time.Now()
				if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", force); !errors.Is(err, ErrWorkspaceActive) {
					t.Fatalf("cleanup with force=%v ignored active lock: %v", force, err)
				}
				if time.Since(start) > time.Second {
					t.Fatal("cleanup waited on active lock")
				}
			}
			if _, err := os.Stat(spec.Path); err != nil {
				t.Fatalf("active worktree removed: %v", err)
			}
		})
	}
}

func TestManagedCleanupProtectsDirtyAndIgnoredFilesAndForceRemovesOnlyWorktree(t *testing.T) {
	for _, kind := range []string{"staged", "unstaged", "untracked", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			dataDir, spec := managedCleanupFixture(t)
			name := "README.md"
			if kind == "untracked" || kind == "ignored" {
				name = "keep.txt"
			}
			handoffWriteTest(t, spec.Path, name, []byte("precious data\n"))
			if kind == "staged" {
				runGitTest(t, spec.Path, "add", name)
			}
			if kind == "ignored" {
				common := strings.TrimSpace(string(handoffReadGitTest(t, spec.Repository, "rev-parse", "--git-common-dir")))
				if !filepath.IsAbs(common) {
					common = filepath.Join(spec.Repository, common)
				}
				if err := os.WriteFile(filepath.Join(common, "info", "exclude"), []byte("keep.txt\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			status, err := InspectManagedWorkspace(context.Background(), dataDir, "owner")
			if err != nil || !status.Exists || status.Locked || !status.Dirty {
				t.Fatalf("dirty inspection = %+v, %v", status, err)
			}
			if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", false); !errors.Is(err, ErrWorkspaceDirty) {
				t.Fatalf("dirty cleanup = %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(spec.Path, name)); err != nil || string(data) != "precious data\n" {
				t.Fatalf("default cleanup lost changes: %q, %v", data, err)
			}
			if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", true); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Dir(spec.Path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owner metadata retained: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(spec.Repository, "README.md")); err != nil || string(data) != "test\n" {
				t.Fatalf("cleanup modified source: %q, %v", data, err)
			}
			if exists, err := branchExists(context.Background(), spec.Repository, spec.Branch); err != nil || !exists {
				t.Fatalf("cleanup removed branch: %v, %v", exists, err)
			}
		})
	}
}

func TestManagedInspectionCountsUnmergedCommitsAndCleanupRetainsBranch(t *testing.T) {
	dataDir, spec := managedCleanupFixture(t)
	status, err := InspectManagedWorkspace(context.Background(), dataDir, "owner")
	if err != nil || !status.Exists || status.Dirty || status.Locked || status.UnmergedCommits != 0 {
		t.Fatalf("initial inspection = %+v, %v", status, err)
	}
	handoffWriteTest(t, spec.Path, "feature.txt", []byte("committed work\n"))
	runGitTest(t, spec.Path, "add", "feature.txt")
	runGitTest(t, spec.Path, "commit", "-m", "feature")
	status, err = InspectManagedWorkspace(context.Background(), dataDir, "owner")
	if err != nil || status.Dirty || status.UnmergedCommits != 1 {
		t.Fatalf("unmerged inspection = %+v, %v", status, err)
	}
	if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", false); err != nil {
		t.Fatal(err)
	}
	if got := handoffReadGitTest(t, spec.Repository, "show", spec.Branch+":feature.txt"); string(got) != "committed work\n" {
		t.Fatalf("unmerged commit was lost: %q", got)
	}
	if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", false); err != nil {
		t.Fatalf("repeat cleanup = %v", err)
	}
	status, err = InspectManagedWorkspace(context.Background(), dataDir, "owner")
	if err != nil || status.Exists {
		t.Fatalf("inspection after cleanup = %+v, %v", status, err)
	}
}

func TestManagedCleanupRemovesStaleGitRegistration(t *testing.T) {
	dataDir, spec := managedCleanupFixture(t)
	handoffWriteTest(t, spec.Path, "committed.txt", []byte("keep branch reachable\n"))
	runGitTest(t, spec.Path, "add", "committed.txt")
	runGitTest(t, spec.Path, "commit", "-m", "unmerged commit")
	if err := os.RemoveAll(spec.Path); err != nil {
		t.Fatal(err)
	}
	status, err := InspectManagedWorkspace(context.Background(), dataDir, "owner")
	if err != nil || status.Exists || status.UnmergedCommits != 1 {
		t.Fatalf("missing checkout lost branch inspection: %+v, %v", status, err)
	}
	if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", false); err != nil {
		t.Fatal(err)
	}
	listed := handoffReadGitTest(t, spec.Repository, "worktree", "list", "--porcelain", "-z")
	if bytes.Contains(listed, []byte(spec.Path)) {
		t.Fatalf("stale Git registration retained: %s", listed)
	}
}

func TestManagedCleanupDoesNotRequireHistoricalBaseToExist(t *testing.T) {
	dataDir, spec := managedCleanupFixture(t)
	var record prepared
	path := filepath.Join(filepath.Dir(spec.Path), "prepared.json")
	if _, err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	record.Spec.BaseRef = "deleted-base"
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(filepath.Dir(spec.Path), "intent.json"), intent{Owner: record.Owner, Spec: record.Spec}); err != nil {
		t.Fatal(err)
	}
	// The worktree branch is retained, so a missing base cannot lose commits.
	if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", false); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCleanupRefusesUnownedPathsAndGitLockedWorktrees(t *testing.T) {
	for _, kind := range []string{"source-path", "wrong-owner", "different-branch", "git-locked", "worktree-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dataDir, spec := managedCleanupFixture(t)
			ownerDir := filepath.Dir(spec.Path)
			switch kind {
			case "source-path", "wrong-owner":
				var record prepared
				if _, err := readJSON(filepath.Join(ownerDir, "prepared.json"), &record); err != nil {
					t.Fatal(err)
				}
				if kind == "source-path" {
					record.Path, record.Spec.Path = spec.Repository, spec.Repository
				} else {
					record.Owner = "someone-else"
				}
				if err := writeJSON(filepath.Join(ownerDir, "prepared.json"), record); err != nil {
					t.Fatal(err)
				}
				if err := writeJSON(filepath.Join(ownerDir, "intent.json"), intent{Owner: record.Owner, Spec: record.Spec}); err != nil {
					t.Fatal(err)
				}
			case "different-branch":
				runGitTest(t, spec.Path, "switch", "-c", "unowned-branch")
			case "git-locked":
				runGitTest(t, spec.Repository, "worktree", "lock", spec.Path)
			case "worktree-symlink":
				if err := os.Rename(spec.Path, spec.Path+"-preserved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(spec.Repository, spec.Path); err != nil {
					t.Fatal(err)
				}
			}
			if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", true); err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if _, err := os.Stat(filepath.Join(spec.Repository, "README.md")); err != nil {
				t.Fatalf("source removed by unsafe cleanup: %v", err)
			}
			if _, err := os.Stat(filepath.Join(ownerDir, "prepared.json")); err != nil {
				t.Fatalf("ownership evidence removed: %v", err)
			}
		})
	}
}

func TestManagedInspectionDoesNotCreateDirectoriesAndRejectsUnsafeOwners(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "missing")
	if status, err := InspectManagedWorkspace(context.Background(), dataDir, "owner"); err != nil || status.Exists {
		t.Fatalf("missing inspection: %+v, %v", status, err)
	}
	if err := CleanupManagedWorkspace(context.Background(), dataDir, "owner", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read/cleanup created data directory: %v", err)
	}
	if _, err := InspectManagedWorkspace(context.Background(), dataDir, "../outside"); err == nil {
		t.Fatal("unsafe owner accepted")
	}
}

func managedCleanupFixture(t *testing.T) (string, api.WorkspaceSpec) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "repo")
	initRepository(t, source)
	dataDir := filepath.Join(root, "data")
	spec, err := Prepare(context.Background(), dataDir, api.WorkspaceSpec{Kind: KindWorktree, Repository: source, Branch: "feature/managed"}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return dataDir, spec
}

func handoffWriteTest(t *testing.T, root, name string, data []byte) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func handoffReadGitTest(t *testing.T, directory string, args ...string) []byte {
	t.Helper()
	output, err := handoffGit(context.Background(), directory, nil, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func assertHandoffSourceTest(t *testing.T, source string, expected handoffSnapshot) {
	t.Helper()
	actual, err := captureHandoff(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("source changes or index were not preserved\nexpected staged: %s\nactual staged: %s\nexpected unstaged: %s\nactual unstaged: %s", expected.Staged, actual.Staged, expected.Unstaged, actual.Unstaged)
	}
}
