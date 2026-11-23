package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/hostfs"
	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/pkg/api"
)

// HandoffToWorktree creates a new branch and owned checkout. With
// moveDirtyChanges, both index and working-tree changes (including nonignored
// untracked files) are copied and verified before the source is restored.
// A failure during source restoration returns the prepared target with an error;
// the source is restored and the target/recovery data are retained for inspection.
func HandoffToWorktree(ctx context.Context, dataDir, owner, sourceDir, targetBranch, baseRef string, moveDirtyChanges bool) (result api.WorkspaceSpec, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if sourceDir == "" {
		return result, errors.New("handoff source directory is required")
	}
	source, err := absoluteClean(sourceDir)
	if err != nil {
		return result, err
	}
	if _, err := canonicalRepository(ctx, source); err != nil {
		return result, err
	}
	if err := handoffRepositoryReady(ctx, source); err != nil {
		return result, err
	}
	if targetBranch == "" || strings.HasPrefix(targetBranch, "-") {
		return result, errors.New("handoff requires a new target branch")
	}
	if _, err := handoffGit(ctx, source, nil, nil, "check-ref-format", "--branch", targetBranch); err != nil {
		return result, err
	}
	if exists, err := branchExists(ctx, source, targetBranch); err != nil {
		return result, err
	} else if exists {
		return result, fmt.Errorf("handoff target branch already exists: %s", targetBranch)
	}
	if baseRef == "" {
		baseRef = "HEAD"
	}
	base, err := handoffGit(ctx, source, nil, nil, "rev-parse", "--verify", "--end-of-options", baseRef+"^{commit}")
	if err != nil {
		return result, err
	}
	ownerDir, _, err := managedOwnerDir(dataDir, owner, false)
	if err != nil {
		return result, err
	}
	if relative, err := filepath.Rel(source, ownerDir); err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return result, errors.New("handoff metadata must be outside the source checkout")
	}
	if _, _, err := managedOwnerDir(dataDir, owner, true); err != nil {
		return result, err
	}
	spec := api.WorkspaceSpec{Kind: KindWorktree, Path: filepath.Join(ownerDir, "worktree"), Repository: source, Branch: targetBranch, BaseRef: baseRef}
	var locks []*os.File
	defer func() { releaseManagedLocks(locks) }()
	for _, name := range []string{"workspace.lock", "prepare.lock"} {
		file, err := tryManagedFileLock(filepath.Join(ownerDir, name), true)
		if err != nil {
			return result, err
		}
		locks = append(locks, file)
	}
	sourceLock, err := tryManagedDirectoryLock(source)
	if err != nil {
		return result, err
	}
	locks = append(locks, sourceLock)
	if err := handoffRepositoryReady(ctx, source); err != nil {
		return result, err
	}
	if _, err := os.Lstat(spec.Path); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("handoff requires a fresh managed worktree path; existing contents were preserved")
	}
	// Only a directory intent for this exact source may be replaced. Keep the
	// original records so a failed target preparation can restore the session.
	previous := map[string]json.RawMessage{}
	for _, name := range []string{"intent.json", "prepared.json"} {
		data, err := os.ReadFile(filepath.Join(ownerDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, err
		}
		var old prepared
		if err := json.Unmarshal(data, &old); err != nil {
			return result, err
		}
		if old.Owner != owner || old.Spec.Kind != KindDirectory || !samePath(old.Spec.Path, source) || (name == "prepared.json" && !samePath(old.Path, source)) {
			return result, errors.New("handoff conflicts with an existing workspace owner")
		}
		previous[name] = data
	}
	var snapshot handoffSnapshot
	if moveDirtyChanges {
		snapshot, err = captureHandoff(ctx, source)
		if err != nil {
			return result, err
		}
	}
	published, created, preserveTarget := false, false, false
	var targetLock *os.File
	defer func() {
		if resultErr == nil || published || preserveTarget {
			return
		}
		// Source has not been touched. Remove only the checkout created by
		// this call, and only if no hook has added commits of its own.
		recoveryCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if created {
			if targetLock == nil {
				guard, err := tryManagedDirectoryLock(spec.Path)
				if err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("failed target retained at %s: %w", spec.Path, err))
					return
				}
				locks = append(locks, guard)
			}
			if err := verifyWorktree(recoveryCtx, spec); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("failed target retained at %s: %w", spec.Path, err))
				return
			}
			head, err := handoffGit(recoveryCtx, spec.Path, nil, nil, "rev-parse", "HEAD")
			if err != nil || !bytes.Equal(bytes.TrimSpace(head), bytes.TrimSpace(base)) {
				resultErr = errors.Join(resultErr, fmt.Errorf("failed target retained at %s for inspection", spec.Path))
				return
			}
			if _, err := handoffGit(recoveryCtx, source, nil, nil, "worktree", "remove", "--force", "--", spec.Path); err != nil {
				resultErr = errors.Join(resultErr, err)
				return
			}
			// This branch was created at base by this invocation, without commits.
			_, err = handoffGit(recoveryCtx, source, nil, nil, "branch", "-D", "--", targetBranch)
			resultErr = errors.Join(resultErr, err)
		}
		for _, name := range []string{"intent.json", "prepared.json"} {
			path := filepath.Join(ownerDir, name)
			if data, found := previous[name]; found {
				resultErr = errors.Join(resultErr, writeJSON(path, data))
			} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	if err := writeJSON(filepath.Join(ownerDir, "intent.json"), intent{Owner: owner, Spec: spec}); err != nil {
		return result, err
	}
	if err := os.Remove(filepath.Join(ownerDir, "prepared.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	_, err = handoffGit(ctx, source, nil, nil, "worktree", "add", "-b", targetBranch, "--", spec.Path, strings.TrimSpace(string(base)))
	// Even a failed hook can leave behind a successfully registered worktree.
	if _, statErr := os.Lstat(spec.Path); statErr == nil {
		created = true
	}
	if err != nil {
		return result, err
	}
	targetLock, err = tryManagedDirectoryLock(spec.Path)
	if err != nil {
		return result, err
	}
	locks = append(locks, targetLock)
	if err := verifyWorktree(ctx, spec); err != nil {
		return result, err
	}
	if dirty, err := managedDirty(ctx, spec.Path); err != nil {
		preserveTarget = true
		return result, err
	} else if dirty {
		preserveTarget = true
		return result, fmt.Errorf("new worktree has hook-created changes; target retained at %s and source untouched", spec.Path)
	}
	if moveDirtyChanges {
		for _, patch := range []struct {
			data  []byte
			index bool
		}{{snapshot.Staged, true}, {snapshot.Unstaged, false}} {
			if len(patch.data) == 0 {
				continue
			}
			args := []string{"apply", "--binary", "--whitespace=nowarn"}
			if patch.index {
				args = append(args, "--index")
			}
			if _, err := handoffGit(ctx, spec.Path, nil, patch.data, args...); err != nil {
				return result, fmt.Errorf("apply handoff changes (source untouched): %w", err)
			}
		}
		// Copy actual affected files as well: preserve permissions, symlinks,
		// binary bytes and working files intentionally different from the index.
		if err := restoreHandoffFiles(spec.Path, snapshot.Files); err != nil {
			return result, err
		}
		if err := verifyHandoff(ctx, spec.Path, snapshot); err != nil {
			return result, err
		}
	}
	record := prepared{Owner: owner, Spec: spec, Path: spec.Path, CreatedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(ownerDir, "prepared.json"), record); err != nil {
		return result, err
	}
	published = true
	result = spec
	if moveDirtyChanges && len(snapshot.Files) != 0 {
		if err := cleanHandoffSource(ctx, ownerDir, snapshot); err != nil {
			return result, fmt.Errorf("handoff target is prepared at %s; source changes preserved: %w", spec.Path, err)
		}
	}
	return result, nil
}

type handoffFile struct {
	Name   string      `json:"name"`
	Exists bool        `json:"exists"`
	Mode   os.FileMode `json:"mode"`
	Data   []byte      `json:"data,omitempty"`
	Link   string      `json:"link,omitempty"`
}

type handoffSnapshot struct {
	Source       string        `json:"source"`
	Head         string        `json:"head"`
	IndexPath    string        `json:"indexPath"`
	Index        []byte        `json:"index"`
	IndexEntries []byte        `json:"indexEntries"`
	Staged       []byte        `json:"staged"`
	Unstaged     []byte        `json:"unstaged"`
	Tracked      []string      `json:"tracked"`
	Untracked    []string      `json:"untracked"`
	Files        []handoffFile `json:"files"`
}

func handoffRepositoryReady(ctx context.Context, source string) error {
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer"} {
		path, err := handoffGit(ctx, source, nil, nil, "rev-parse", "--git-path", name)
		if err != nil {
			return err
		}
		resolved := strings.TrimSpace(string(path))
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(source, resolved)
		}
		if _, err := os.Lstat(resolved); err == nil {
			return fmt.Errorf("handoff refused: unfinished Git operation (%s)", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	unmerged, err := handoffGit(ctx, source, nil, nil, "ls-files", "--unmerged", "-z")
	if err != nil {
		return err
	}
	if len(unmerged) != 0 {
		return errors.New("handoff refused: unresolved Git index conflicts")
	}
	return nil
}

func captureHandoff(ctx context.Context, source string) (handoffSnapshot, error) {
	snapshot := handoffSnapshot{Source: source}
	head, err := handoffGit(ctx, source, nil, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return snapshot, err
	}
	snapshot.Head = strings.TrimSpace(string(head))
	indexPath, err := handoffGit(ctx, source, nil, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return snapshot, err
	}
	snapshot.IndexPath = strings.TrimSpace(string(indexPath))
	if !filepath.IsAbs(snapshot.IndexPath) {
		snapshot.IndexPath = filepath.Join(source, snapshot.IndexPath)
	}
	snapshot.Index, err = os.ReadFile(snapshot.IndexPath)
	if err != nil {
		return snapshot, err
	}
	flags := []string{"diff", "--binary", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=none", "--src-prefix=a/", "--dst-prefix=b/"}
	snapshot.Staged, err = handoffGit(ctx, source, nil, nil, append(flags, "--cached", "HEAD", "--")...)
	if err != nil {
		return snapshot, err
	}
	snapshot.Unstaged, err = handoffGit(ctx, source, nil, nil, append(flags, "--")...)
	if err != nil {
		return snapshot, err
	}
	paths := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z", "--no-renames", "--cached", "HEAD", "--"},
		{"diff", "--name-only", "-z", "--no-renames", "--"},
	} {
		output, err := handoffGit(ctx, source, nil, nil, args...)
		if err != nil {
			return snapshot, err
		}
		for _, name := range nulPaths(output) {
			paths[name] = true
		}
	}
	for name := range paths {
		snapshot.Tracked = append(snapshot.Tracked, name)
	}
	sort.Strings(snapshot.Tracked)
	if len(snapshot.Tracked) > 0 {
		snapshot.IndexEntries, err = handoffGit(ctx, source, nil, nil, append([]string{"ls-files", "--stage", "-z", "--"}, literalHandoffPaths(snapshot.Tracked)...)...)
		if err != nil {
			return snapshot, err
		}
		for _, entry := range nulPaths(snapshot.IndexEntries) {
			if strings.HasPrefix(entry, "160000 ") {
				return snapshot, errors.New("handoff of dirty submodules is unsupported; source preserved")
			}
		}
	}
	untracked, err := handoffGit(ctx, source, nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return snapshot, err
	}
	snapshot.Untracked = nulPaths(untracked)
	for _, name := range snapshot.Untracked {
		paths[name] = true
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		if !filepath.IsLocal(name) || name == ".git" || strings.HasPrefix(name, ".git/") {
			return snapshot, fmt.Errorf("unsupported handoff path %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	root, err := os.OpenRoot(source)
	if err != nil {
		return snapshot, err
	}
	defer root.Close()
	for _, name := range names {
		file, err := readHandoffFile(root, name)
		if err != nil {
			return snapshot, err
		}
		snapshot.Files = append(snapshot.Files, file)
	}
	return snapshot, nil
}

func readHandoffFile(root *os.Root, name string) (handoffFile, error) {
	file := handoffFile{Name: name}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return file, err
	}
	file.Exists, file.Mode = true, info.Mode()
	switch {
	case info.Mode().IsRegular():
		opened, err := hostfs.OpenRootNoFollow(root, name)
		if err != nil {
			return file, err
		}
		defer opened.Close()
		var data bytes.Buffer
		if _, err := data.ReadFrom(opened); err != nil {
			return file, err
		}
		file.Data = data.Bytes()
	case info.Mode()&os.ModeSymlink != 0:
		file.Link, err = root.Readlink(name)
	default:
		err = fmt.Errorf("handoff only supports regular files and symlinks: %s", name)
	}
	return file, err
}

func restoreHandoffFiles(path string, files []handoffFile) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range files {
		current, err := readHandoffFile(root, file.Name)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(current, file) {
			continue
		}
		if current.Exists {
			if err := root.Remove(file.Name); err != nil {
				return err
			}
		}
		if !file.Exists {
			continue
		}
		if err := root.MkdirAll(filepath.Dir(file.Name), 0o755); err != nil {
			return err
		}
		if file.Mode&os.ModeSymlink != 0 {
			if err := root.Symlink(file.Link, file.Name); err != nil {
				return err
			}
			continue
		}
		opened, err := root.OpenFile(file.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, file.Mode.Perm())
		if err != nil {
			return err
		}
		_, writeErr := opened.Write(file.Data)
		err = errors.Join(writeErr, opened.Chmod(file.Mode.Perm()), opened.Sync(), opened.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func verifyHandoff(ctx context.Context, target string, snapshot handoffSnapshot) error {
	if len(snapshot.Tracked) > 0 {
		entries, err := handoffGit(ctx, target, nil, nil, append([]string{"ls-files", "--stage", "-z", "--"}, literalHandoffPaths(snapshot.Tracked)...)...)
		if err != nil {
			return err
		}
		if !bytes.Equal(entries, snapshot.IndexEntries) {
			return errors.New("handoff staged entries differ; source preserved")
		}
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, expected := range snapshot.Files {
		actual, err := readHandoffFile(root, expected.Name)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("handoff file %s was not fully written; source preserved", expected.Name)
		}
	}
	return nil
}

func cleanHandoffSource(ctx context.Context, ownerDir string, snapshot handoffSnapshot) (resultErr error) {
	// Reserve Git's real index commit slot. Worktree restoration uses a private
	// index, so the real index stays untouched until every file operation works.
	lockPath := snapshot.IndexPath + ".lock"
	indexLock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reserve source Git index (source untouched): %w", err)
	}
	committed, restoring := false, false
	defer func() {
		_ = indexLock.Close()
		if !committed {
			_ = os.Remove(lockPath)
		}
		if resultErr != nil && restoring {
			if err := restoreHandoffFiles(snapshot.Source, snapshot.Files); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("restore source files: %w; recovery copy: %s", err, filepath.Join(ownerDir, "handoff-source.json")))
			}
		}
	}()
	current, err := captureHandoff(ctx, snapshot.Source)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, snapshot) {
		return errors.New("source changed during handoff; current source was preserved")
	}
	backup := filepath.Join(ownerDir, "handoff-source.json")
	if err := writeJSON(backup, snapshot); err != nil {
		return err
	}
	alternate, err := os.CreateTemp(ownerDir, ".handoff-index-")
	if err != nil {
		return err
	}
	defer os.Remove(alternate.Name())
	_, writeErr := alternate.Write(snapshot.Index)
	if err := errors.Join(writeErr, alternate.Close()); err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + alternate.Name()}
	trackedAtHead := map[string]bool{}
	if len(snapshot.Untracked) > 0 {
		output, err := handoffGit(ctx, snapshot.Source, nil, nil, append([]string{"ls-tree", "-r", "--name-only", "-z", "HEAD", "--"}, literalHandoffPaths(snapshot.Untracked)...)...)
		if err != nil {
			return err
		}
		for _, name := range nulPaths(output) {
			trackedAtHead[name] = true
		}
	}
	restoring = true
	if len(snapshot.Tracked) > 0 {
		args := append([]string{"restore", "--source=HEAD", "--staged", "--worktree", "--"}, literalHandoffPaths(snapshot.Tracked)...)
		if _, err := handoffGit(ctx, snapshot.Source, env, nil, args...); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(snapshot.Source)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range snapshot.Untracked {
		// git rm --cached can make a HEAD-tracked path appear untracked. Its
		// restored HEAD file belongs in the source, so do not remove it again.
		if trackedAtHead[name] {
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if err := root.Remove(parent); err != nil {
				break // Keep nonempty parents, especially ignored user files.
			}
		}
	}
	status, err := handoffGit(ctx, snapshot.Source, env, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return errors.New("source did not become clean; original changes restored")
	}
	cleanIndex, err := os.ReadFile(alternate.Name())
	if err != nil {
		return err
	}
	_, writeErr = indexLock.Write(cleanIndex)
	if err := errors.Join(writeErr, indexLock.Sync(), indexLock.Close(), ctx.Err()); err != nil {
		return err
	}
	if err := os.Rename(lockPath, snapshot.IndexPath); err != nil {
		return err
	}
	// No fallible operation after committing the source index can turn this
	// successful transfer into an error. A leftover backup is safe to retain.
	committed = true
	_ = os.Remove(backup)
	return nil
}

func literalHandoffPaths(paths []string) []string {
	result := make([]string, len(paths))
	for i, path := range paths {
		result[i] = ":(literal)" + path
	}
	return result
}

func nulPaths(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

// handoffGit keeps patch bytes and NUL-delimited filenames intact. Unlike the
// display-oriented runGit helper, it never trims command output.
func handoffGit(ctx context.Context, directory string, env []string, input []byte, args ...string) ([]byte, error) {
	command := hostprocess.NewContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	command.Env = append(GitEnvironment(), "GIT_OPTIONAL_LOCKS=0")
	command.Env = append(command.Env, env...)
	command.Stdin = bytes.NewReader(input)
	defer command.Close()
	command.WaitDelay = 250 * time.Millisecond
	var output, stderr bytes.Buffer
	command.Stdout, command.Stderr = &output, &stderr
	err := command.Run()
	if command.Process != nil {
		_ = command.KillTree()
	}
	if err != nil || ctx.Err() != nil {
		message := stderr.Bytes()
		if len(message) > 4096 {
			message = message[:4096]
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], errors.Join(ctx.Err(), err), message)
	}
	return output.Bytes(), nil
}
