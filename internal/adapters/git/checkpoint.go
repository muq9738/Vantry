package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

const checkpointRefPrefix = "refs/vantry/checkpoints/"

// Checkpoint is a Git-native worktree snapshot. Ref keeps it reachable without
// moving HEAD, modifying the user's index, or using the stash.
type Checkpoint struct {
	ID        string    `json:"id"`
	Ref       string    `json:"ref"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
}

type CheckpointRestoreOptions struct {
	AllowPrimaryCheckout bool `json:"allowPrimaryCheckout,omitempty"`
}

// CheckpointRestoreResult exposes the backup even if applying a restore fails.
// Restoring Backup.ID uses the same safety checks as any other checkpoint.
type CheckpointRestoreResult struct {
	Backup Checkpoint `json:"backup"`
}

// CaptureCheckpoint saves tracked edits and non-ignored untracked files. It
// captures worktree contents, rather than the user's staged/unstaged split.
func CaptureCheckpoint(ctx context.Context, workspacePath, checkpointID, label string) (Checkpoint, error) {
	path, lock, err := lockCheckpointWorkspace(ctx, workspacePath)
	if err != nil {
		return Checkpoint{}, err
	}
	defer workspace.Unlock(lock)
	ref, err := checkpointRef(ctx, path, checkpointID)
	if err != nil {
		return Checkpoint{}, err
	}
	index, cleanup, err := checkpointIndex(ctx, path)
	if err != nil {
		return Checkpoint{}, err
	}
	defer cleanup()
	return captureCheckpoint(ctx, path, index, ref, label)
}

// ListCheckpoints lists this repository's checkpoint refs, newest first.
func ListCheckpoints(ctx context.Context, workspacePath string) ([]Checkpoint, error) {
	path, err := checkoutRoot(ctx, workspacePath)
	if err != nil {
		return nil, err
	}
	result, err := execute(ctx, path, "for-each-ref", "--sort=-refname", "--sort=-creatordate",
		"--format=%(refname)%00%(creatordate:iso-strict)%00%(contents:subject)", checkpointRefPrefix)
	if err != nil {
		return nil, err
	}
	if result.stdout.truncated {
		return nil, errors.New("checkpoint 列表过大，请在终端查看完整 refs/vantry/checkpoints 列表")
	}
	checkpoints := make([]Checkpoint, 0)
	for _, line := range strings.Split(strings.TrimSuffix(result.stdout.data.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			return nil, errors.New("无法解析 Git checkpoint 元数据")
		}
		created, err := time.Parse(time.RFC3339, parts[1])
		if err != nil {
			return nil, fmt.Errorf("无法解析 checkpoint 创建时间：%w", err)
		}
		checkpoints = append(checkpoints, Checkpoint{
			ID: strings.TrimPrefix(parts[0], checkpointRefPrefix), Ref: parts[0], Label: parts[2], CreatedAt: created,
		})
	}
	return checkpoints, nil
}

// DiffCheckpoint compares the snapshot with current worktree contents,
// including untracked ordinary files and deletions, without changing the index.
func DiffCheckpoint(ctx context.Context, workspacePath, checkpointID string) (string, error) {
	path, lock, err := lockCheckpointWorkspace(ctx, workspacePath)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)
	ref, err := resolveCheckpoint(ctx, path, checkpointID)
	if err != nil {
		return "", err
	}
	index, cleanup, err := checkpointIndex(ctx, path)
	if err != nil {
		return "", err
	}
	defer cleanup()
	tree, err := checkpointCommand(ctx, path, index, "write-tree")
	if err != nil {
		return "", err
	}
	return command(ctx, path, "diff", "--no-ext-diff", "--no-textconv", "--no-color", ref+"^{tree}", strings.TrimSpace(tree), "--")
}

// RestoreCheckpoint changes only worktree files, leaving HEAD, branches and the
// user's index intact. Main checkouts require an explicit opt-in; linked Git
// worktrees are isolated from their primary checkout. Pending operations are
// always refused. The current state is backed up before attempting any writes.
func RestoreCheckpoint(ctx context.Context, workspacePath, checkpointID string, opts CheckpointRestoreOptions) (CheckpointRestoreResult, error) {
	var result CheckpointRestoreResult
	path, lock, err := lockCheckpointWorkspace(ctx, workspacePath)
	if err != nil {
		return result, err
	}
	defer workspace.Unlock(lock)
	pending, err := pendingOperation(ctx, path)
	if err != nil {
		return result, err
	}
	if pending != "" {
		return result, fmt.Errorf("拒绝恢复 checkpoint：已有未完成的 Git 操作（%s），请先在终端处理", pending)
	}
	gitDir, err := command(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return result, err
	}
	commonDir, err := command(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return result, err
	}
	gitDirPath, err := filepath.EvalSymlinks(strings.TrimSuffix(gitDir, "\n"))
	if err != nil {
		return result, err
	}
	commonDirPath, err := filepath.EvalSymlinks(strings.TrimSuffix(commonDir, "\n"))
	if err != nil {
		return result, err
	}
	if gitDirPath == commonDirPath && !opts.AllowPrimaryCheckout {
		return result, errors.New("拒绝覆盖主 checkout：请使用隔离 Git worktree，或显式设置 AllowPrimaryCheckout: true")
	}
	ref, err := resolveCheckpoint(ctx, path, checkpointID)
	if err != nil {
		return result, err
	}
	index, cleanup, err := checkpointIndex(ctx, path)
	if err != nil {
		return result, err
	}
	defer cleanup()
	backupRef := checkpointRefPrefix + "pre-restore-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	result.Backup, err = captureCheckpoint(ctx, path, index, backupRef, "Before restoring "+checkpointID)
	if err != nil {
		return result, fmt.Errorf("无法保存恢复前备份，未覆盖工作区：%w", err)
	}
	if err := protectIgnoredCheckpointPaths(ctx, path, index, result.Backup.Ref, ref); err != nil {
		return result, err
	}
	// Two-tree merge uses the captured worktree as its old index. Only the
	// private index is updated. Files absent from the target are also removed.
	// Git allows ignored obstructions, so they were checked explicitly above.
	_, err = checkpointCommand(ctx, path, index, "read-tree", "-m", "-u", result.Backup.Ref+"^{tree}", ref+"^{tree}")
	if err != nil {
		return result, fmt.Errorf("恢复 checkpoint 失败；当前状态备份保留在 %s：%w", result.Backup.Ref, err)
	}
	return result, nil
}

func lockCheckpointWorkspace(ctx context.Context, path string) (string, *os.File, error) {
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", nil, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lock, err := workspace.LockDirectory(lockCtx, root)
	if err != nil {
		return "", nil, fmt.Errorf("无法取得 checkpoint 工作区写入锁：%w", err)
	}
	return root, lock, nil
}

func checkpointRef(ctx context.Context, path, id string) (string, error) {
	if id == "" || strings.Contains(id, "/") {
		return "", errors.New("checkpoint ID 必须是非空的单段 Git ref 名")
	}
	ref := checkpointRefPrefix + id
	if _, err := command(ctx, path, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("无效的 checkpoint ID：%w", err)
	}
	return ref, nil
}

func resolveCheckpoint(ctx context.Context, path, id string) (string, error) {
	ref, err := checkpointRef(ctx, path, id)
	if err != nil {
		return "", err
	}
	if _, err := command(ctx, path, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return "", fmt.Errorf("checkpoint %q 不存在或不是有效提交：%w", id, err)
	}
	return ref, nil
}

func checkpointIndex(ctx context.Context, path string) (string, func(), error) {
	directory, err := os.MkdirTemp("", "vantry-checkpoint-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	index := filepath.Join(directory, "index")
	if _, err := checkpointCommand(ctx, path, index, "read-tree", "--empty"); err != nil {
		cleanup()
		return "", nil, err
	}
	// Import native index entries without their assume-unchanged/skip-worktree
	// flags. This also includes staged additions that are now ignored. No bytes
	// of the real index are changed, including its cached stat information.
	tracked, err := execute(ctx, path, "ls-files", "--stage", "-z")
	if err != nil || tracked.stdout.truncated {
		cleanup()
		if err == nil {
			err = errors.New("Git index 路径列表过大，无法完整捕获 checkpoint")
		}
		return "", nil, err
	}
	if _, err := executeWithInput(ctx, path, []string{"GIT_INDEX_FILE=" + index, "GIT_OPTIONAL_LOCKS=0"},
		strings.NewReader(tracked.stdout.data.String()), "update-index", "-z", "--index-info"); err != nil {
		cleanup()
		return "", nil, err
	}
	if _, err := checkpointCommand(ctx, path, index, "-c", "core.sparseCheckout=false", "add", "-A", "--", "."); err != nil {
		cleanup()
		return "", nil, err
	}
	return index, cleanup, nil
}

// protectIgnoredCheckpointPaths checks native Git path lists rather than file
// contents. Ignored directories are collapsed so large build directories do
// not require enumerating every file. Truncated output always refuses restore.
func protectIgnoredCheckpointPaths(ctx context.Context, path, index, current, target string) error {
	ignored, err := executeWithEnvironment(ctx, path, []string{"GIT_INDEX_FILE=" + index, "GIT_OPTIONAL_LOCKS=0"},
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return err
	}
	if ignored.stdout.truncated {
		return errors.New("ignored 路径列表过大，无法安全恢复 checkpoint；恢复前备份已保留")
	}
	if ignored.stdout.Len() == 0 {
		return nil
	}
	changed, err := execute(ctx, path, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", current+"^{tree}", target+"^{tree}", "--")
	if err != nil {
		return err
	}
	if changed.stdout.truncated {
		return errors.New("checkpoint 变更路径列表过大，无法安全恢复；恢复前备份已保留")
	}
	paths := strings.Split(strings.TrimSuffix(changed.stdout.data.String(), "\x00"), "\x00")
	sort.Strings(paths)
	for _, name := range strings.Split(strings.TrimSuffix(ignored.stdout.data.String(), "\x00"), "\x00") {
		name = strings.TrimSuffix(name, "/")
		// The ignored path itself or any changed ancestor would replace it.
		for ancestor := name; ancestor != "."; ancestor = filepath.ToSlash(filepath.Dir(ancestor)) {
			if index := sort.SearchStrings(paths, ancestor); index < len(paths) && paths[index] == ancestor {
				return fmt.Errorf("拒绝恢复 checkpoint：会覆盖未纳入备份的 ignored 路径 %q；恢复前备份已保留", name)
			}
		}
		// A changed descendant would write into an ignored directory.
		prefix := name + "/"
		if index := sort.SearchStrings(paths, prefix); index < len(paths) && strings.HasPrefix(paths[index], prefix) {
			return fmt.Errorf("拒绝恢复 checkpoint：会覆盖未纳入备份的 ignored 目录 %q；恢复前备份已保留", name)
		}
	}
	return nil
}

func checkpointCommand(ctx context.Context, path, index string, args ...string) (string, error) {
	result, err := executeWithEnvironment(ctx, path, []string{"GIT_INDEX_FILE=" + index, "GIT_OPTIONAL_LOCKS=0"}, args...)
	return result.stdout.String(), err
}

func captureCheckpoint(ctx context.Context, path, index, ref, label string) (Checkpoint, error) {
	var checkpoint Checkpoint
	if strings.ContainsAny(label, "\x00\r\n") {
		return checkpoint, errors.New("checkpoint label 必须是单行文本")
	}
	tree, err := checkpointCommand(ctx, path, index, "write-tree")
	if err != nil {
		return checkpoint, err
	}
	args := []string{"-c", "commit.gpgsign=false", "commit-tree", strings.TrimSpace(tree)}
	if _, err := command(ctx, path, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		args = append(args, "-p", "HEAD")
	} else if !exitCode(err, 1) {
		return checkpoint, err
	}
	if label == "" {
		label = strings.TrimPrefix(ref, checkpointRefPrefix)
	}
	args = append(args, "-m", label)
	commit, err := executeWithEnvironment(ctx, path, []string{
		"GIT_AUTHOR_NAME=Vantry", "GIT_AUTHOR_EMAIL=checkpoint@vantry.local",
		"GIT_COMMITTER_NAME=Vantry", "GIT_COMMITTER_EMAIL=checkpoint@vantry.local",
	}, args...)
	if err != nil {
		return checkpoint, err
	}
	// An empty old value requires a new ref. Reusing an ID must never destroy a
	// previous checkpoint, including one created by a different worktree.
	if _, err := command(ctx, path, "update-ref", ref, strings.TrimSpace(commit.stdout.data.String()), ""); err != nil {
		return checkpoint, fmt.Errorf("无法创建 checkpoint ref（ID 可能已存在）：%w", err)
	}
	created, err := command(ctx, path, "show", "-s", "--format=%cI", ref)
	if err != nil {
		return checkpoint, err
	}
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(created))
	if err != nil {
		return checkpoint, err
	}
	return Checkpoint{ID: strings.TrimPrefix(ref, checkpointRefPrefix), Ref: ref, Label: label, CreatedAt: createdAt}, nil
}

// RunCheckpointID sanitizes an arbitrary run ID into a valid single-segment
// Git ref suffix under refs/vantry/checkpoints/.
func RunCheckpointID(runID string) string {
	var b strings.Builder
	b.Grow(len(runID))
	for _, r := range strings.TrimSpace(runID) {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	safe := b.String()
	for strings.Contains(safe, "..") {
		safe = strings.ReplaceAll(safe, "..", "-")
	}
	safe = strings.Trim(safe, ".-")
	if strings.HasSuffix(safe, ".lock") {
		safe = strings.TrimSuffix(safe, ".lock") + "-lock"
	}
	if safe == "" {
		safe = "unknown"
	}
	return "run-" + safe
}

// IsGitWorkspace reports whether path or any ancestor directory contains a .git
// entry without spawning a git subprocess.
func IsGitWorkspace(path string) bool {
	cleaned := strings.TrimSpace(path)
	if cleaned == "" {
		return false
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		abs = filepath.Clean(cleaned)
	}
	cur := abs
	for {
		if _, statErr := os.Stat(filepath.Join(cur, ".git")); statErr == nil {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}
