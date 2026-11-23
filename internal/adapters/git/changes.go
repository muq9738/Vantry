package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

func parseFiles(raw string) []api.GitFile {
	parts := strings.Split(raw, "\x00")
	files := []api.GitFile{}
	for i := 0; i < len(parts)-1; i++ {
		record := parts[i]
		if len(record) < 4 || record[2] != ' ' {
			continue
		}
		f := api.GitFile{Path: record[3:], Index: record[:1], Worktree: record[1:2]}
		if strings.ContainsAny(f.Index+f.Worktree, "RC") {
			if i+1 >= len(parts)-1 {
				break
			} // truncated rename never becomes an action target
			i++
			f.OriginalPath = parts[i]
		}
		files = append(files, f)
	}
	return files
}

// Change stages, unstages, commits, or discards selected local changes. It shares
// the checkout lock with provider work and verifies the caller's branch before
// any mutation. Callers must refresh even on error.
func Change(ctx context.Context, path string, request api.WorkbenchRequest) (string, error) {
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lock, err := workspace.LockDirectory(lockCtx, root)
	if err != nil {
		return "", fmt.Errorf("工作区正在使用，请稍后重试：%w", err)
	}
	defer workspace.Unlock(lock)
	branch, err := command(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", errors.New("请先切换到本地分支再操作 Git")
	}
	if strings.TrimSpace(branch) != request.Branch || request.Branch == "" {
		return "", errors.New("当前分支已变化，请刷新后再操作")
	}
	pending, err := pendingOperation(ctx, root)
	if err != nil {
		return "", err
	}
	if pending != "" {
		return "", fmt.Errorf("请先处理未完成的 Git 操作：%s", pending)
	}
	switch request.Action {
	case "git.stage", "git.unstage":
		if len(request.Paths) == 0 || len(request.Paths) > 1000 {
			return "", errors.New("请选择 1–1000 个文件")
		}
		records, err := execute(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
		if err != nil {
			return "", err
		}
		known := map[string]api.GitFile{}
		for _, f := range parseFiles(records.stdout.data.String()) {
			known[f.Path] = f
		}
		paths := make([]string, 0, len(request.Paths)*2)
		seen := map[string]bool{}
		add := func(path string) error {
			if !filepath.IsLocal(path) || filepath.Clean(path) == "." {
				return errors.New("文件路径必须位于工作区内")
			}
			if !seen[path] {
				paths = append(paths, path)
				seen[path] = true
			}
			return nil
		}
		for _, path := range request.Paths {
			f, ok := known[path]
			if !ok {
				return "", fmt.Errorf("文件状态已变化，请刷新：%s", path)
			}
			if request.Action == "git.stage" && (f.Worktree == " " || f.Worktree == "") {
				return "", fmt.Errorf("文件没有未暂存变更：%s", path)
			}
			if request.Action == "git.unstage" && (f.Index == " " || f.Index == "?") {
				return "", fmt.Errorf("文件尚未暂存：%s", path)
			}
			if err := add(f.Path); err != nil {
				return "", err
			}
			if f.OriginalPath != "" && (request.Action == "git.unstage" || f.Worktree == "R") {
				if err := add(f.OriginalPath); err != nil {
					return "", err
				}
			}
		}
		args := []string{"--literal-pathspecs", "add", "--"}
		if request.Action == "git.unstage" {
			if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
				args = []string{"--literal-pathspecs", "restore", "--staged", "--"}
			} else if exitCode(err, 1) {
				args = []string{"--literal-pathspecs", "rm", "--cached", "--"}
			} else {
				return "", err
			}
		}
		result, err := execute(ctx, root, append(args, paths...)...)
		return result.text(false), err
	case "git.commit":
		return commitSelected(ctx, root, request.Paths, request.Message, request.Amend)
	case "git.discard":
		return discardSelected(ctx, root, request.Paths)
	default:
		return "", errors.New("不支持的 Git 操作")
	}
}
