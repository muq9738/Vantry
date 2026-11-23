package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

// Commit stages explicitly selected paths and commits only that selection. With
// no paths it commits the index, preserving the worktree's unstaged changes.
func Commit(ctx context.Context, path string, paths []string, message string, amend bool) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)
	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}
	return commitSelected(ctx, root, paths, message, amend)
}

// commitSelected requires the caller to hold the checkout lock.
func commitSelected(ctx context.Context, root string, paths []string, message string, amend bool) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 16<<10 || strings.ContainsRune(message, 0) {
		return "", errors.New("提交说明不能为空，且最多 16 KiB")
	}
	conflict, err := conflicts(ctx, root)
	if err != nil {
		return "", err
	}
	if conflict {
		return "", errors.New("请先解决冲突")
	}
	if len(paths) > 0 {
		paths, err = checkedPaths(root, paths)
		if err != nil {
			return "", err
		}
		if _, err := command(ctx, root, append([]string{"--literal-pathspecs", "add", "--"}, paths...)...); err != nil {
			return "", err
		}
	}
	args := []string{"--literal-pathspecs", "commit", "-m", message}
	if amend {
		args = append(args, "--amend")
	}
	args = append(args, "--")
	args = append(args, paths...)
	result, err := execute(ctx, root, args...)
	if err != nil {
		return result.text(false), fmt.Errorf("提交未确认；已保留现场，请刷新后检查，勿直接重复提交：%w", err)
	}
	return result.text(false), nil
}

func requireNoPendingOperation(ctx context.Context, root string) error {
	pending, err := pendingOperation(ctx, root)
	if err != nil {
		return err
	}
	if pending != "" {
		return fmt.Errorf("请先处理未完成的 Git 操作：%s", pending)
	}
	return nil
}

// checkedPaths treats every selection as a literal repository-relative path.
// Existing symlink components must also resolve inside the checkout.
func checkedPaths(root string, paths []string) ([]string, error) {
	if len(paths) == 0 || len(paths) > 1000 {
		return nil, errors.New("请选择 1–1000 个文件或目录")
	}
	selected := make([]string, 0, len(paths))
	seen := make(map[string]bool)
	for _, name := range paths {
		if err := checkPath(root, name); err != nil {
			return nil, err
		}
		name = filepath.ToSlash(filepath.Clean(name))
		if !seen[name] {
			selected = append(selected, name)
			seen[name] = true
		}
	}
	return selected, nil
}

func checkPath(root, name string) error {
	if !filepath.IsLocal(name) || filepath.Clean(name) == "." || strings.ContainsRune(name, 0) {
		return errors.New("文件路径必须严格位于工作区内")
	}
	for _, part := range strings.Split(filepath.Clean(name), string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") {
			return errors.New("不能操作 Git 元数据路径")
		}
	}
	for ancestor := filepath.Clean(name); ancestor != "."; ancestor = filepath.Dir(ancestor) {
		absolute := filepath.Join(root, ancestor)
		info, err := os.Lstat(absolute)
		if errors.Is(err, os.ErrNotExist) {
			continue // Deleted tracked paths still need to be restorable.
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(absolute)
			if err != nil {
				return fmt.Errorf("无法确认符号链接位于工作区内：%s：%w", name, err)
			}
			relative, err := filepath.Rel(root, resolved)
			if err != nil || !filepath.IsLocal(relative) {
				return fmt.Errorf("符号链接指向工作区之外：%s", name)
			}
		}
	}
	return nil
}

// Discard restores selected tracked paths and removes selected untracked files
// and directories. All selections are validated before the first mutation.
func Discard(ctx context.Context, path string, paths []string) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)
	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}
	return discardSelected(ctx, root, paths)
}

func discardSelected(ctx context.Context, root string, paths []string) (string, error) {
	paths, err := checkedPaths(root, paths)
	if err != nil {
		return "", err
	}
	readPaths := func(args ...string) ([]string, error) {
		args = append(args, "--")
		result, err := execute(ctx, root, append(args, paths...)...)
		if err != nil {
			return nil, err
		}
		if result.stdout.truncated {
			return nil, errors.New("变更路径过多，无法完整还原，请缩小选择范围")
		}
		var names []string
		for _, name := range strings.Split(result.stdout.data.String(), "\x00") {
			if name == "" {
				continue
			}
			if err := checkPath(root, name); err != nil {
				return nil, err
			}
			names = append(names, name)
		}
		return names, nil
	}
	indexed, err := readPaths("--literal-pathspecs", "ls-files", "-z")
	if err != nil {
		return "", err
	}
	var tracked []string
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		tracked, err = readPaths("--literal-pathspecs", "ls-tree", "-r", "--name-only", "-z", "HEAD")
		if err != nil {
			return "", err
		}
	} else if !exitCode(err, 1) {
		return "", err
	}
	inHead := make(map[string]bool, len(tracked))
	for _, name := range tracked {
		inHead[name] = true
	}
	var additions []string
	seen := make(map[string]bool)
	for _, name := range indexed {
		if !inHead[name] && !seen[name] {
			additions = append(additions, name)
			seen[name] = true
		}
	}
	var output limitedOutput
	for _, operation := range []struct {
		args  []string
		paths []string
	}{
		{[]string{"--literal-pathspecs", "checkout", "HEAD", "--"}, tracked},
		{[]string{"--literal-pathspecs", "rm", "-f", "--"}, additions},
		{[]string{"--literal-pathspecs", "clean", "-fd", "--"}, paths},
	} {
		if len(operation.paths) == 0 {
			continue
		}
		result, err := execute(ctx, root, append(operation.args, operation.paths...)...)
		_, _ = output.Write([]byte(result.text(false)))
		if err != nil {
			return output.String(), err
		}
	}
	return output.String(), nil
}

// CheckoutBranch switches local branches, or creates a local tracking branch
// when given a remote-tracking name such as origin/feature.
func CheckoutBranch(ctx context.Context, path, branch string, create bool, startPoint string) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	if err := validateBranchName(ctx, root, branch); err != nil {
		return "", err
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)
	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}
	args := []string{"switch", "--no-overwrite-ignore", "--no-guess"}
	if create {
		args = append(args, "-c", branch)
		if startPoint != "" {
			id, err := resolveCommit(ctx, root, startPoint)
			if err != nil {
				return "", err
			}
			args = append(args, "--", id)
		}
	} else {
		_, localErr := command(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
		if localErr != nil && !exitCode(localErr, 1) {
			return "", localErr
		}
		if localErr != nil {
			remoteRef := "refs/remotes/" + branch
			if _, remoteErr := command(ctx, root, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); remoteErr == nil {
				_, local, ok := strings.Cut(branch, "/")
				if !ok {
					return "", errors.New("远端跟踪分支名称无效")
				}
				if err := validateBranchName(ctx, root, local); err != nil {
					return "", err
				}
				if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+local+"^{commit}"); err == nil {
					branch = local
				} else if exitCode(err, 1) {
					args = append(args, "--track", "-c", local, "--", remoteRef)
					result, err := execute(ctx, root, args...)
					return result.text(false), err
				} else {
					return "", err
				}
			} else if !exitCode(remoteErr, 1) {
				return "", remoteErr
			}
		}
		args = append(args, "--", branch)
	}
	result, err := execute(ctx, root, args...)
	return result.text(false), err
}

func DeleteBranch(ctx context.Context, path, branch string, force bool) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	if err := validateBranchName(ctx, root, branch); err != nil {
		return "", err
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)
	current, _, err := currentLocalBranch(ctx, root)
	if err != nil {
		return "", err
	}
	if current == branch {
		return "", errors.New("不能删除当前检出的分支")
	}
	flag := "-d"
	if force {
		flag = "-D"
	}
	result, err := execute(ctx, root, "branch", flag, "--", branch)
	return result.text(false), err
}

var stashRefPattern = regexp.MustCompile(`^stash@\{[0-9]+\}$`)

func selectedStashRef(refs []string) (string, error) {
	if len(refs) > 1 {
		return "", errors.New("请选择一个 stash 记录")
	}
	ref := "stash@{0}"
	if len(refs) == 1 && refs[0] != "" {
		ref = refs[0]
	}
	if !stashRefPattern.MatchString(ref) {
		return "", errors.New("stash 引用必须形如 stash@{0}")
	}
	return ref, nil
}

func StashDrop(ctx context.Context, path, ref string) (string, error) {
	ref, err := selectedStashRef([]string{ref})
	if err != nil {
		return "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)
	result, err := execute(ctx, root, "stash", "drop", "--", ref)
	return result.text(false), err
}
