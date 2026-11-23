package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/hostfs"
)

var (
	ErrWorkspaceActive = errors.New("managed workspace has an active worker or preparation lock")
	ErrWorkspaceDirty  = errors.New("managed workspace has uncommitted or untracked files; preserved (use forceDirty to discard)")
)

// ManagedStatus describes only an owned, isolated checkout. UnmergedCommits is
// relative to the recorded base in the source repository. Removing a worktree
// never removes its branch, so those commits remain reachable after cleanup.
type ManagedStatus struct {
	Owner           string `json:"owner"`
	Path            string `json:"path"`
	Exists          bool   `json:"exists"`
	Locked          bool   `json:"locked"`
	Dirty           bool   `json:"dirty"`
	UnmergedCommits int    `json:"unmergedCommits"`
}

func InspectManagedWorkspace(ctx context.Context, dataDir, owner string) (ManagedStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ownerDir, exists, err := managedOwnerDir(dataDir, owner, false)
	status := ManagedStatus{Owner: owner, Path: filepath.Join(ownerDir, "worktree")}
	if err != nil || !exists {
		return status, err
	}
	if err := ctx.Err(); err != nil {
		return status, err
	}
	var locks []*os.File
	defer func() { releaseManagedLocks(locks) }()
	for _, name := range []string{"workspace.lock", "prepare.lock"} {
		file, err := tryManagedFileLock(filepath.Join(ownerDir, name), false)
		if errors.Is(err, ErrWorkspaceActive) {
			status.Locked = true
			continue
		}
		if err != nil {
			return status, err
		}
		locks = append(locks, file)
	}
	if info, err := os.Lstat(status.Path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return status, errors.New("managed worktree path is not a real directory; preserved")
		}
		status.Exists = true
		file, err := tryManagedDirectoryLock(status.Path)
		if errors.Is(err, ErrWorkspaceActive) {
			status.Locked = true
		} else if err != nil {
			return status, err
		} else {
			locks = append(locks, file)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return status, err
	}
	record, err := managedRecord(ownerDir, owner)
	if err != nil {
		return status, err
	}
	if status.Exists {
		if err := verifyPrepared(ctx, record); err != nil {
			return status, err
		}
		if err := inspectManagedGit(ctx, record, &status); err != nil {
			return status, err
		}
	} else {
		status.UnmergedCommits, err = managedUnmerged(ctx, record)
		if err != nil {
			return status, err
		}
	}
	if _, err := os.Lstat(filepath.Join(ownerDir, "handoff-source.json")); err == nil {
		status.Dirty = true // Retained recovery data must not disappear silently.
	} else if !errors.Is(err, os.ErrNotExist) {
		return status, err
	}
	return status, nil
}

func CleanupManagedWorkspace(ctx context.Context, dataDir, owner string, forceDirty bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ownerDir, exists, err := managedOwnerDir(dataDir, owner, false)
	if err != nil || !exists {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var locks []*os.File
	defer func() { releaseManagedLocks(locks) }()
	// Never wait for a worker. A busy preparation is just as live as execution.
	for _, name := range []string{"workspace.lock", "prepare.lock"} {
		file, err := tryManagedFileLock(filepath.Join(ownerDir, name), true)
		if err != nil {
			return err
		}
		locks = append(locks, file)
	}
	record, err := managedRecord(ownerDir, owner)
	if err != nil {
		return err
	}
	// A failed handoff backup can outlive its checkout. It remains protected.
	if !forceDirty {
		if _, err := os.Lstat(filepath.Join(ownerDir, "handoff-source.json")); err == nil {
			return fmt.Errorf("%w: handoff recovery data remains", ErrWorkspaceDirty)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	remove := false
	info, err := os.Lstat(record.Path)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed worktree path is not a real directory; preserved")
		}
		file, err := tryManagedDirectoryLock(record.Path)
		if err != nil {
			return err
		}
		locks = append(locks, file)
		if err := verifyPrepared(ctx, record); err != nil {
			return fmt.Errorf("cleanup refused because ownership is not confirmed: %w", err)
		}
		dirty, err := managedDirty(ctx, record.Path)
		if err != nil {
			return err
		}
		if dirty && !forceDirty {
			return ErrWorkspaceDirty
		}
		remove = true
	} else if errors.Is(err, os.ErrNotExist) {
		remove, err = missingManagedRegistration(ctx, record)
		if err != nil {
			return err
		}
	} else {
		return err
	}
	if remove {
		args := []string{"worktree", "remove"}
		if forceDirty {
			args = append(args, "--force")
		}
		args = append(args, "--", record.Path)
		if _, err := handoffGit(ctx, record.Spec.Repository, nil, nil, args...); err != nil {
			return fmt.Errorf("remove managed worktree (metadata preserved): %w", err)
		}
	}
	return os.RemoveAll(ownerDir)
}

func missingManagedRegistration(ctx context.Context, record prepared) (bool, error) {
	if _, err := canonicalRepository(ctx, record.Spec.Repository); err != nil {
		return false, err
	}
	output, err := handoffGit(ctx, record.Spec.Repository, nil, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	selected, found, locked := false, false, false
	branch := ""
	for _, field := range nulPaths(output) {
		switch {
		case strings.HasPrefix(field, "worktree "):
			selected = samePath(strings.TrimPrefix(field, "worktree "), record.Path)
			found = found || selected
		case selected && strings.HasPrefix(field, "branch "):
			branch = strings.TrimPrefix(field, "branch ")
		case selected && (field == "locked" || strings.HasPrefix(field, "locked ")):
			locked = true
		case field == "":
			selected = false
		}
	}
	if found && (branch != "refs/heads/"+record.Spec.Branch || locked) {
		return false, errors.New("missing worktree registration changed or is Git-locked; preserved")
	}
	return found, nil
}

func managedDirty(ctx context.Context, path string) (bool, error) {
	// Include ignored files too: setup-created .env files are still user data.
	dirty, err := handoffGit(ctx, path, nil, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored")
	return len(dirty) != 0, err
}

func inspectManagedGit(ctx context.Context, record prepared, status *ManagedStatus) error {
	var err error
	status.Dirty, err = managedDirty(ctx, record.Path)
	if err != nil {
		return err
	}
	status.UnmergedCommits, err = managedUnmerged(ctx, record)
	return err
}

func managedUnmerged(ctx context.Context, record prepared) (int, error) {
	base, err := handoffGit(ctx, record.Spec.Repository, nil, nil, "rev-parse", "--verify", "--end-of-options", record.Spec.BaseRef+"^{commit}")
	if err != nil {
		return 0, fmt.Errorf("inspect managed worktree base: %w", err)
	}
	count, err := handoffGit(ctx, record.Spec.Repository, nil, nil, "rev-list", "--count", "refs/heads/"+record.Spec.Branch, "--not", strings.TrimSpace(string(base)), "--")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(count)))
}

func managedOwnerDir(dataDir, owner string, create bool) (string, bool, error) {
	if err := validComponent(owner); err != nil {
		return "", false, fmt.Errorf("invalid workspace owner: %w", err)
	}
	if dataDir == "" {
		return "", false, errors.New("workspace data directory is required")
	}
	absolute, err := managedDataPath(dataDir)
	if err != nil {
		return "", false, err
	}
	ownerDir := filepath.Join(absolute, "workspaces", owner)
	for _, path := range []string{absolute, filepath.Dir(ownerDir), ownerDir} {
		if create {
			if err := privateDirectory(path); err != nil {
				return "", false, err
			}
		} else {
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				return ownerDir, false, nil
			}
			if err != nil {
				return "", false, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", false, fmt.Errorf("workspace metadata is not a real directory: %s", path)
			}
		}
	}
	return ownerDir, true, nil
}

// Resolve existing ancestors even before dataDir is created. This preserves
// macOS /var aliases and prevents a future path through a symlink from hiding
// the fact that handoff metadata would live inside the source checkout.
func managedDataPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := filepath.Clean(absolute)
	var missing []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(ancestor))
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("workspace data directory has no accessible ancestor")
		}
		ancestor = parent
	}
}

func managedRecord(ownerDir, owner string) (prepared, error) {
	var planned intent
	found, err := readJSON(filepath.Join(ownerDir, "intent.json"), &planned)
	if err != nil {
		return prepared{}, err
	}
	var record prepared
	ready, err := readJSON(filepath.Join(ownerDir, "prepared.json"), &record)
	if err != nil {
		return prepared{}, err
	}
	if !ready && found {
		record = prepared{Owner: planned.Owner, Spec: planned.Spec, Path: planned.Spec.Path}
	}
	if (!found && !ready) || record.Owner != owner || record.Spec.Kind != KindWorktree ||
		record.Path != record.Spec.Path || filepath.Base(record.Path) != "worktree" || !samePath(filepath.Dir(record.Path), ownerDir) ||
		(found && (planned.Owner != owner || !reflect.DeepEqual(planned.Spec, record.Spec))) {
		return prepared{}, errors.New("managed workspace ownership is not confirmed; preserved")
	}
	if record.Spec.Repository == "" || record.Spec.Branch == "" || record.Spec.BaseRef == "" {
		return prepared{}, errors.New("managed worktree intent is incomplete; preserved")
	}
	// Prepare historically preserves aliases in dataDir (for example /var on
	// macOS). Use the verified managed spelling when passing a removal path.
	record.Path = filepath.Join(ownerDir, "worktree")
	record.Spec.Path = record.Path
	return record, nil
}

func tryManagedFileLock(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := hostfs.OpenNoFollow(path, flags, 0o600)
	if errors.Is(err, os.ErrNotExist) && !create {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("workspace lock is not a regular file")
	}
	return tryManagedFlock(file)
}

func tryManagedDirectoryLock(path string) (*os.File, error) {
	file, err := hostfs.OpenDirectoryLock(path)
	if err != nil {
		return nil, err
	}
	return tryManagedFlock(file)
}

func tryManagedFlock(file *os.File) (*os.File, error) {
	err := hostfs.TryLock(file)
	if err == nil {
		return file, nil
	}
	_ = file.Close()
	if hostfs.LockBusy(err) {
		return nil, ErrWorkspaceActive
	}
	return nil, err
}

func releaseManagedLocks(locks []*os.File) {
	for i := len(locks) - 1; i >= 0; i-- {
		Unlock(locks[i])
	}
}
