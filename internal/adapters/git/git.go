package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

func checkoutRoot(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", errors.New("工作区路径不能为空")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	out, err := command(ctx, absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(strings.TrimSuffix(out, "\n"))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Git 配置指向了请求目录之外的工作区")
	}
	return root, nil
}

func status(ctx context.Context, path string) (string, error) {
	return command(ctx, path, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
}

func conflicts(ctx context.Context, path string) (bool, error) {
	out, err := command(ctx, path, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--name-only", "--diff-filter=U", "--")
	return out != "", err
}

func Inspect(ctx context.Context, path string) (api.GitState, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()
	var state api.GitState
	path, err := checkoutRoot(ctx, path)
	if err != nil {
		return state, err
	}
	branch, err := command(ctx, path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if !exitCode(err, 1) {
			return state, err
		}
		branch = "detached HEAD"
	}
	state.Root = path
	state.Branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	records, err := execute(ctx, path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return state, err
	}
	state.Files = parseFiles(records.stdout.data.String())
	state.Truncated = records.stdout.truncated
	// The plain status is the same records as lines: one status walk of the
	// working tree instead of two, which is what large checkouts pay for.
	state.Status = statusText(state.Files)
	// Everything below only reads refs, history and diffs. Running the
	// queries side by side keeps a refresh close to its slowest one.
	var (
		wait        sync.WaitGroup
		mu          sync.Mutex
		firstErr    error
		branchList  string
		branchTrunc bool
		staged      commandResult
		unstaged    commandResult
		history     api.GitState
		untracked   limitedOutput
		untrackTrnc bool
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	run := func(fn func() error) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := fn(); err != nil {
				fail(err)
			}
		}()
	}
	run(func() error {
		branches, err := execute(ctx, path, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads/")
		branchList, branchTrunc = branches.stdout.data.String(), branches.stdout.truncated
		return err
	})
	run(func() error {
		var err error
		state.Conflicts, err = conflicts(ctx, path)
		return err
	})
	run(func() error { return inspectHistory(ctx, path, &history) })
	diffArgs := func(extra ...string) []string {
		args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--ignore-submodules=none"}
		return append(append(args, extra...), "--")
	}
	run(func() error {
		var err error
		staged, err = execute(ctx, path, diffArgs("--cached")...)
		return err
	})
	run(func() error {
		var err error
		unstaged, err = execute(ctx, path, diffArgs()...)
		return err
	})
	run(func() error {
		var err error
		untrackTrnc, err = appendUntrackedDiff(ctx, path, state.Files, &untracked)
		return err
	})
	wait.Wait()
	if firstErr != nil {
		return state, firstErr
	}
	if branchTrunc {
		// A partial last line must never become a selectable branch name.
		branchList = branchList[:strings.LastIndex(branchList, "\n")+1]
		state.Status += "[本地分支列表已截断；请在终端查看全部分支。]\n"
	}
	for _, name := range strings.Split(strings.TrimSuffix(branchList, "\n"), "\n") {
		if name != "" {
			state.Branches = append(state.Branches, name)
		}
	}
	state.Commits, state.Ahead, state.Behind, state.RemoteBranch = history.Commits, history.Ahead, history.Behind, history.RemoteBranch
	state.RemoteBranches, state.Stashes = history.RemoteBranches, history.Stashes
	state.Truncated = state.Truncated || history.Truncated
	var diff limitedOutput
	for _, section := range []struct {
		label  string
		result commandResult
	}{{"已暂存 / staged", staged}, {"未暂存 / unstaged", unstaged}} {
		if section.result.stdout.Len() > 0 {
			_, _ = diff.Write([]byte("\n[" + section.label + "]\n"))
			_, _ = diff.Write(section.result.stdout.Bytes())
		}
		diff.truncated = diff.truncated || section.result.stdout.truncated
	}
	if untracked.Len() > 0 {
		_, _ = diff.Write(untracked.Bytes())
	}
	diff.truncated = diff.truncated || untracked.truncated
	state.Diff = diff.String()
	state.Truncated = state.Truncated || diff.truncated || untrackTrnc
	return state, nil
}

// statusText renders parsed porcelain records as "XY path" lines, with a
// rename's original path first, as git status --porcelain prints them.
func statusText(files []api.GitFile) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.Index)
		b.WriteString(f.Worktree)
		b.WriteByte(' ')
		if f.OriginalPath != "" {
			b.WriteString(f.OriginalPath)
			b.WriteString(" -> ")
		}
		b.WriteString(f.Path)
		b.WriteByte('\n')
	}
	return b.String()
}

// pendingOperation checks worktree-local state, including clean paused rebases
// and cherry-picks that a porcelain status alone cannot detect.
func pendingOperation(ctx context.Context, path string) (string, error) {
	out, err := command(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	gitDir := strings.TrimSuffix(out, "\n")
	for _, name := range []string{"MERGE_HEAD", "MERGE_AUTOSTASH", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer", "BISECT_START"} {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return name, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// Merge merges a local source into the branch checked out in path. It never
// stashes dirty work. Only actual conflicts are automatically aborted; failed
// hooks/signing and configured no-commit/squash results retain their edits.
func Merge(ctx context.Context, path, branch string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if branch == "" || strings.HasPrefix(branch, "-") {
		return "", errors.New("请选择本地来源分支")
	}
	path, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	checked, err := command(ctx, path, "check-ref-format", "--branch", branch)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(checked) != branch {
		return "", errors.New("来源必须是明确的本地分支名")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lock, err := workspace.LockDirectory(lockCtx, path)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("等待工作区时取消：%w", ctx.Err())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("工作区仍被任务占用，请结束后重试：%w", err)
		}
		return "", fmt.Errorf("无法锁定工作区：%w", err)
	}
	defer workspace.Unlock(lock)
	target, err := command(ctx, path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if exitCode(err, 1) {
			return "", errors.New("detached HEAD 不能自动合并，请先切换到目标分支")
		}
		return "", err
	}
	target = strings.TrimSpace(target)
	if !strings.HasPrefix(target, "refs/heads/") {
		return "", errors.New("当前目标不是本地分支")
	}
	if _, err := command(ctx, path, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return "", fmt.Errorf("目标分支尚无可合并的提交：%w", err)
	}
	source := "refs/heads/" + branch
	if source == target {
		return "", errors.New("来源分支与当前目标分支相同")
	}
	if _, err := command(ctx, path, "rev-parse", "--verify", "--quiet", source+"^{commit}"); err != nil {
		return "", fmt.Errorf("本地来源分支不存在或不可用：%w", err)
	}
	pending, err := pendingOperation(ctx, path)
	if err != nil {
		return "", err
	}
	if pending != "" {
		return "", fmt.Errorf("已有未完成的 Git 操作（%s），请先在终端处理", pending)
	}
	dirty, err := status(ctx, path)
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", errors.New("工作区有未提交或未跟踪内容，请先处理后再合并")
	}
	result, mergeErr := execute(ctx, path, "merge", "--no-edit", "--no-autostash", "--no-overwrite-ignore", "--", source)
	output := result.text(false)
	if mergeErr == nil {
		current, err := command(ctx, path, "symbolic-ref", "--quiet", "HEAD")
		if err != nil {
			return output, fmt.Errorf("合并命令已结束，但无法确认当前目标分支，请检查工作区：%w", err)
		}
		if strings.TrimSpace(current) != target {
			return output, errors.New("合并命令已结束，但 hook 等改变了当前目标分支；已保留现场，请在终端检查")
		}
		pending, err := pendingOperation(ctx, path)
		if err != nil {
			return output, fmt.Errorf("合并命令已结束，但无法确认最终状态，请检查工作区：%w", err)
		}
		if pending != "" {
			return output, fmt.Errorf("合并尚未完成（%s）；已保留当前改动，请在终端处理", pending)
		}
		if _, err := command(ctx, path, "merge-base", "--is-ancestor", source, "HEAD"); err != nil {
			return output, fmt.Errorf("来源分支尚未确认合入目标分支；已保留当前改动（例如 squash 结果），请在终端检查：%w", err)
		}
		dirty, err := status(ctx, path)
		if err != nil {
			return output, fmt.Errorf("合并命令已结束，但无法读取最终状态：%w", err)
		}
		if dirty != "" {
			output += "\n[合并已完成；hook 等留下的未提交内容已保留，请检查工作区。]\n"
		}
		return output, nil
	}
	return output, recoverFailure(path, target, source, mergeErr)
}

func recoverFailure(path, target, source string, mergeErr error) error {
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := command(cleanup, path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || strings.TrimSpace(current) != target {
		return fmt.Errorf("合并失败或被取消；目标分支已改变或无法确认，未自动中止：%w", errors.Join(mergeErr, err))
	}
	pending, pendingErr := pendingOperation(cleanup, path)
	unmerged, conflictErr := conflicts(cleanup, path)
	if pendingErr != nil || conflictErr != nil {
		return fmt.Errorf("合并失败或被取消；无法确认工作区状态，已保留当前改动：%w", errors.Join(mergeErr, pendingErr, conflictErr))
	}
	if _, err := command(cleanup, path, "merge-base", "--is-ancestor", source, "HEAD"); err == nil {
		// Git may run post-merge before deleting MERGE_HEAD. Cancellation at
		// that point has already advanced the branch and must not undo it.
		return fmt.Errorf("Git 返回错误或被取消，但来源分支已合入目标分支；未回滚提交，请检查工作区和 Git 操作状态：%w", mergeErr)
	}
	if pending == "MERGE_HEAD" && unmerged {
		if _, err := command(cleanup, path, "merge", "--abort"); err != nil {
			return fmt.Errorf("合并失败；自动中止也失败，请在终端检查：%w", errors.Join(mergeErr, err))
		}
		dirty, err := status(cleanup, path)
		if err != nil || dirty != "" {
			return fmt.Errorf("冲突合并已中止；工作区仍有改动或状态无法确认，请检查：%w", errors.Join(mergeErr, err))
		}
		return fmt.Errorf("合并发生冲突，已中止并恢复合并前的工作区：%w", mergeErr)
	}
	return fmt.Errorf("合并失败或被取消；已保留当前工作区和未完成操作，请在终端检查：%w", mergeErr)
}
