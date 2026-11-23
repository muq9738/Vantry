// Package workspace prepares provider working directories without ever falling
// back to a different checkout after a preparation failure.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	KindDirectory = "directory"
	KindWorktree  = "worktree"
)

type intent struct {
	Owner string            `json:"owner"`
	Spec  api.WorkspaceSpec `json:"spec"`
}

type prepared struct {
	Owner     string            `json:"owner"`
	Spec      api.WorkspaceSpec `json:"spec"`
	Path      string            `json:"path"`
	CreatedAt time.Time         `json:"createdAt"`
}

// Prepare normalizes and prepares a workspace owned by owner (normally a
// thread ID). The intent is committed before external preparation. Repeating
// the same preparation recovers an existing worktree, including a dirty one.
func Prepare(ctx context.Context, dataDir string, spec api.WorkspaceSpec, owner string) (api.WorkspaceSpec, error) {
	if err := validComponent(owner); err != nil {
		return api.WorkspaceSpec{}, fmt.Errorf("invalid workspace owner: %w", err)
	}
	if dataDir == "" {
		return api.WorkspaceSpec{}, errors.New("workspace data directory is required")
	}
	absoluteData, err := filepath.Abs(dataDir)
	if err != nil {
		return api.WorkspaceSpec{}, err
	}
	if err := privateDirectory(absoluteData); err != nil {
		return api.WorkspaceSpec{}, err
	}
	normalized, err := normalizeSpec(absoluteData, spec, owner)
	if err != nil {
		return api.WorkspaceSpec{}, err
	}
	ownerDir := filepath.Join(absoluteData, "workspaces", owner)
	if err := privateDirectory(ownerDir); err != nil {
		return api.WorkspaceSpec{}, err
	}
	lock, err := lockFile(ctx, filepath.Join(ownerDir, "prepare.lock"))
	if err != nil {
		return api.WorkspaceSpec{}, err
	}
	defer Unlock(lock)

	intentPath := filepath.Join(ownerDir, "intent.json")
	preparedPath := filepath.Join(ownerDir, "prepared.json")
	var existing intent
	found, err := readJSON(intentPath, &existing)
	if err != nil {
		return api.WorkspaceSpec{}, err
	}
	if found {
		if existing.Owner != owner || !reflect.DeepEqual(existing.Spec, normalized) {
			return api.WorkspaceSpec{}, fmt.Errorf("workspace intent conflict for owner %s", owner)
		}
	} else {
		existing = intent{Owner: owner, Spec: normalized}
		if err := writeJSON(intentPath, existing); err != nil {
			return api.WorkspaceSpec{}, fmt.Errorf("persist workspace intent: %w", err)
		}
	}

	var record prepared
	ready, err := readJSON(preparedPath, &record)
	if err != nil {
		return api.WorkspaceSpec{}, err
	}
	if ready {
		if record.Owner != owner || !reflect.DeepEqual(record.Spec, normalized) {
			return api.WorkspaceSpec{}, fmt.Errorf("workspace ownership conflict for owner %s", owner)
		}
		if err := verifyPrepared(ctx, record); err != nil {
			return api.WorkspaceSpec{}, err
		}
		return record.Spec, nil
	}

	switch normalized.Kind {
	case KindDirectory:
		info, err := os.Stat(normalized.Path)
		if err != nil {
			return api.WorkspaceSpec{}, fmt.Errorf("prepare directory workspace: %w", err)
		}
		if !info.IsDir() {
			return api.WorkspaceSpec{}, fmt.Errorf("workspace path is not a directory: %s", normalized.Path)
		}
	case KindWorktree:
		if err := prepareWorktree(ctx, normalized); err != nil {
			return api.WorkspaceSpec{}, err
		}
	default:
		return api.WorkspaceSpec{}, fmt.Errorf("unsupported workspace kind %q", normalized.Kind)
	}
	record = prepared{Owner: owner, Spec: normalized, Path: normalized.Path, CreatedAt: time.Now().UTC()}
	if err := verifyPrepared(ctx, record); err != nil {
		return api.WorkspaceSpec{}, err
	}
	if err := writeJSON(preparedPath, record); err != nil {
		return api.WorkspaceSpec{}, fmt.Errorf("persist workspace ownership: %w", err)
	}
	return normalized, nil
}

func normalizeSpec(dataDir string, spec api.WorkspaceSpec, owner string) (api.WorkspaceSpec, error) {
	if spec.EnvironmentID != "" {
		return api.WorkspaceSpec{}, errors.New("远端工作区只能在其所属执行环境访问，请打开目标机器的会话")
	}
	kind := spec.Kind
	if kind == "" {
		if spec.Repository != "" {
			kind = KindWorktree
		} else {
			kind = KindDirectory
		}
	}
	normalized := api.WorkspaceSpec{Kind: kind, Path: spec.Path, Repository: spec.Repository, Branch: spec.Branch, BaseRef: spec.BaseRef}
	var err error
	switch kind {
	case KindDirectory:
		if normalized.Path == "" {
			return api.WorkspaceSpec{}, errors.New("directory workspace path is required")
		}
		if normalized.Repository != "" || normalized.Branch != "" || normalized.BaseRef != "" {
			return api.WorkspaceSpec{}, errors.New("directory workspace cannot include repository, branch, or baseRef")
		}
		normalized.Path, err = absoluteClean(normalized.Path)
		if err != nil {
			return api.WorkspaceSpec{}, err
		}
	case KindWorktree:
		if normalized.Repository == "" {
			return api.WorkspaceSpec{}, errors.New("worktree repository is required")
		}
		normalized.Repository, err = absoluteClean(normalized.Repository)
		if err != nil {
			return api.WorkspaceSpec{}, err
		}
		if normalized.Path == "" {
			normalized.Path = filepath.Join(dataDir, "workspaces", owner, "worktree")
		} else {
			normalized.Path, err = absoluteClean(normalized.Path)
			if err != nil {
				return api.WorkspaceSpec{}, err
			}
		}
		if samePath(normalized.Path, normalized.Repository) {
			return api.WorkspaceSpec{}, errors.New("隔离 worktree 不能使用来源仓库目录")
		}
		if normalized.Branch == "" {
			normalized.Branch = "vantry/" + owner
		}
		if normalized.BaseRef == "" {
			normalized.BaseRef = "HEAD"
		}
	default:
		return api.WorkspaceSpec{}, fmt.Errorf("unsupported workspace kind %q", kind)
	}
	return normalized, nil
}

func prepareWorktree(ctx context.Context, spec api.WorkspaceSpec) error {
	if _, err := canonicalRepository(ctx, spec.Repository); err != nil {
		return err
	}
	if info, statErr := os.Lstat(spec.Path); statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("worktree path exists and is not a directory: %s", spec.Path)
		}
		if err := verifyWorktree(ctx, spec); err != nil {
			return fmt.Errorf("worktree path is preserved because its ownership is not confirmed: %w", err)
		}
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o700); err != nil {
		return err
	}
	branchExists, err := branchExists(ctx, spec.Repository, spec.Branch)
	if err != nil {
		return err
	}
	args := []string{"-C", spec.Repository, "worktree", "add"}
	if !branchExists {
		args = append(args, "-b", spec.Branch, spec.Path, spec.BaseRef)
	} else {
		args = append(args, spec.Path, spec.Branch)
	}
	if _, err := runGit(ctx, args...); err != nil {
		// A crash or a partially completed git command may nevertheless have
		// created the requested worktree. Recover only that exact worktree.
		if verifyErr := verifyWorktree(ctx, spec); verifyErr == nil {
			return nil
		}
		return fmt.Errorf("prepare requested worktree (no fallback checkout): %w", err)
	}
	return verifyWorktree(ctx, spec)
}

func verifyPrepared(ctx context.Context, record prepared) error {
	if record.Path != record.Spec.Path {
		return errors.New("workspace prepared path does not match its intent")
	}
	switch record.Spec.Kind {
	case KindDirectory:
		info, err := os.Stat(record.Path)
		if err != nil {
			return fmt.Errorf("prepared directory is unavailable: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("prepared workspace is not a directory: %s", record.Path)
		}
		return nil
	case KindWorktree:
		return verifyWorktree(ctx, record.Spec)
	default:
		return fmt.Errorf("unsupported prepared workspace kind %q", record.Spec.Kind)
	}
}

func verifyWorktree(ctx context.Context, spec api.WorkspaceSpec) error {
	if samePath(spec.Path, spec.Repository) {
		return errors.New("隔离 worktree 不能使用来源仓库目录")
	}
	// Preparation and recovery must agree on what a source repository is: the
	// top-level checkout it was recorded from, never a subdirectory of one.
	if _, err := canonicalRepository(ctx, spec.Repository); err != nil {
		return err
	}
	actualRoot, err := repositoryRoot(ctx, spec.Path)
	if err != nil {
		return err
	}
	if !samePath(actualRoot, spec.Path) {
		return fmt.Errorf("worktree root mismatch: %s", actualRoot)
	}
	common, err := gitCommonDir(ctx, spec.Path)
	if err != nil {
		return err
	}
	gitDirectory, err := runGit(ctx, "-C", spec.Path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if samePath(gitDirectory, common) {
		return errors.New("隔离 worktree 不能使用主 checkout")
	}
	expectedCommon, err := gitCommonDir(ctx, spec.Repository)
	if err != nil {
		return err
	}
	if !samePath(common, expectedCommon) {
		return fmt.Errorf("worktree belongs to a different repository: %s", spec.Path)
	}
	branch, err := runGit(ctx, "-C", spec.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("worktree branch is unavailable: %w", err)
	}
	if branch != spec.Branch {
		return fmt.Errorf("worktree branch mismatch: got %s, want %s", branch, spec.Branch)
	}
	return nil
}

// canonicalRepository verifies that repository names the top-level checkout of
// a Git repository instead of a subdirectory of one. The durable intent records
// that root, so Prepare and Existing accept and reject the same requests.
func canonicalRepository(ctx context.Context, repository string) (string, error) {
	root, err := repositoryRoot(ctx, repository)
	if err != nil {
		return "", err
	}
	if !samePath(root, repository) {
		return "", fmt.Errorf("repository must name its top-level checkout %s (got %s)", root, repository)
	}
	return root, nil
}

func repositoryRoot(ctx context.Context, path string) (string, error) {
	root, err := runGit(ctx, "-C", path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve git checkout %s: %w", path, err)
	}
	return absoluteClean(root)
}

func gitCommonDir(ctx context.Context, path string) (string, error) {
	common, err := runGit(ctx, "-C", path, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	return absoluteClean(common)
}

func branchExists(ctx context.Context, repository, branch string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "-C", repository, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	command.Env = GitEnvironment()
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect worktree branch: %w", errors.Join(ctx.Err(), err))
}

func runGit(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = GitEnvironment()
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		err = errors.Join(ctx.Err(), err)
		const limit = 4096
		if len(text) > limit {
			text = text[:limit] + "..."
		}
		if text != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, text)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return text, nil
}

func absoluteClean(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return absolute, nil
}

func samePath(left, right string) bool {
	leftAbsolute, leftErr := absoluteClean(left)
	rightAbsolute, rightErr := absoluteClean(right)
	return leftErr == nil && rightErr == nil && leftAbsolute == rightAbsolute
}

func validComponent(value string) error {
	if value == "" || value == "." || value == ".." {
		return errors.New("identifier is empty or reserved")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("identifier contains unsafe character %q", r)
	}
	return nil
}

func readJSON(path string, target any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
