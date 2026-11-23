package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

// BranchCompareResult summarizes differences between two branches for IDEA's
// "Compare with Current" view: commits exclusive to target (incoming), commits
// exclusive to base (local-only), changed files, and unified patch text.
type BranchCompareResult struct {
	Base          string          `json:"base"`
	Target        string          `json:"target"`
	AheadCommits  []api.GitCommit `json:"aheadCommits"`
	BehindCommits []api.GitCommit `json:"behindCommits"`
	Files         []api.GitFile   `json:"files"`
	Diff          string          `json:"diff"`
	Truncated     bool            `json:"truncated,omitempty"`
}

func resolveBranchRef(ctx context.Context, root, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "-") {
		return "", errors.New("分支名称无效")
	}
	if err := validateBranchName(ctx, root, name); err != nil {
		return "", err
	}
	localRef := "refs/heads/" + name
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", localRef+"^{commit}"); err == nil {
		return localRef, nil
	} else if !exitCode(err, 1) {
		return "", err
	}
	remoteRef := "refs/remotes/" + name
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); err == nil {
		return remoteRef, nil
	} else if !exitCode(err, 1) {
		return "", err
	}
	return "", fmt.Errorf("分支 %q 不存在或不可用", name)
}

// UpdateBranch fast-forward updates a local branch from its upstream (or
// origin/<branch>) without requiring checkout when it is not the current branch.
func UpdateBranch(ctx context.Context, path, branch string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultRemoteOpTimeout)
	defer cancel()

	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	branch = strings.TrimSpace(branch)
	if err := validateBranchName(ctx, root, branch); err != nil {
		return "", err
	}

	current, isLocal, err := currentLocalBranch(ctx, root)
	if err != nil {
		return "", err
	}
	if isLocal && current == branch {
		return PullFFOnly(ctx, path, "", branch)
	}

	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)

	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}"); err != nil {
		return "", fmt.Errorf("本地分支 %q 不存在：%w", branch, err)
	}

	// Resolve upstream tracking ref, falling back to origin/<branch>.
	remoteName := ""
	remoteBranch := ""
	if upstream, err := command(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}"); err == nil {
		upstream = strings.TrimSpace(upstream)
		if r, b, ok := strings.Cut(upstream, "/"); ok && r != "" && b != "" {
			remoteName, remoteBranch = r, b
		}
	}
	if remoteName == "" {
		resolved, err := resolveRemoteName(ctx, root, "", branch)
		if err != nil {
			return "", fmt.Errorf("分支 %q 未配置上游且无法解析远端：%w", branch, err)
		}
		remoteName = resolved
		remoteBranch = branch
	} else {
		if _, err := resolveRemoteName(ctx, root, remoteName, branch); err != nil {
			return "", err
		}
	}

	// Non-forced refspec (<remoteBranch>:<branch>) strictly fast-forwards and
	// refuses divergent updates without touching the working tree.
	refspec := "refs/heads/" + remoteBranch + ":refs/heads/" + branch
	result, err := execute(ctx, root, "fetch", "--no-recurse-submodules", "--", remoteName, refspec)
	output := result.text(false)
	if err != nil {
		return output, fmt.Errorf("快进更新分支 %s 失败（可能存在分叉，请签出后拉取）：%w", branch, err)
	}
	if strings.TrimSpace(output) == "" {
		output = fmt.Sprintf("分支 %s 已是最新（来自 %s/%s）", branch, remoteName, remoteBranch)
	}
	return output, nil
}

// RebaseBranch rebases the current local branch onto target (local or remote
// tracking branch). Requires a clean working tree and aborts cleanly on failure.
func RebaseBranch(ctx context.Context, path, target string) (string, error) {
	if err := ctx.Err(); err != nil {
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

	current, isLocal, err := currentLocalBranch(ctx, root)
	if err != nil {
		return "", err
	}
	if !isLocal || current == "" {
		return "", errors.New("detached HEAD 不能变基，请先切换到本地分支")
	}
	targetRef, err := resolveBranchRef(ctx, root, target)
	if err != nil {
		return "", err
	}
	if targetRef == "refs/heads/"+current {
		return "", errors.New("目标分支与当前分支相同")
	}
	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}
	dirty, err := status(ctx, root)
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", errors.New("工作区有未提交或未跟踪内容，请先提交或储藏后再变基")
	}
	result, rebaseErr := execute(ctx, root, "rebase", "--no-autostash", "--", targetRef)
	output := result.text(false)
	if rebaseErr != nil {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelCleanup()
		_, _ = execute(cleanup, root, "rebase", "--abort")
		return output, fmt.Errorf("变基到 %s 遇到冲突或失败，已自动回滚以保护现场：%w", target, rebaseErr)
	}
	return output, nil
}

// RenameBranch renames an existing local branch.
func RenameBranch(ctx context.Context, path, oldName, newName string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()

	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if err := validateBranchName(ctx, root, oldName); err != nil {
		return "", err
	}
	if err := validateBranchName(ctx, root, newName); err != nil {
		return "", err
	}
	if oldName == newName {
		return "", errors.New("新旧分支名称相同")
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)

	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+oldName+"^{commit}"); err != nil {
		return "", fmt.Errorf("本地分支 %q 不存在：%w", oldName, err)
	}
	result, err := execute(ctx, root, "branch", "-m", "--", oldName, newName)
	if err != nil {
		return result.text(false), fmt.Errorf("重命名分支失败：%w", err)
	}
	return fmt.Sprintf("已将分支 %s 重命名为 %s", oldName, newName), nil
}

// CompareBranches returns the commit and file diffs between base (defaults to
// current local branch or HEAD) and target (local or remote tracking branch).
func CompareBranches(ctx context.Context, path, base, target string) (BranchCompareResult, error) {
	var out BranchCompareResult
	if err := ctx.Err(); err != nil {
		return out, err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()

	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return out, err
	}

	base = strings.TrimSpace(base)
	var baseRef string
	if base == "" {
		cur, isLocal, curErr := currentLocalBranch(ctx, root)
		if curErr != nil {
			return out, curErr
		}
		if isLocal && cur != "" {
			base = cur
			baseRef = "refs/heads/" + cur
		} else {
			base = "HEAD"
			baseRef = "HEAD"
		}
	} else {
		baseRef, err = resolveBranchRef(ctx, root, base)
		if err != nil {
			return out, err
		}
	}
	target = strings.TrimSpace(target)
	targetRef, err := resolveBranchRef(ctx, root, target)
	if err != nil {
		return out, err
	}

	out.Base = base
	out.Target = target

	// AheadCommits: commits present in target that are not in base (base..target).
	ahead, aheadTrunc, err := readCommitLog(ctx, root, 100, []string{baseRef + ".." + targetRef}, nil)
	if err != nil {
		return out, fmt.Errorf("读取目标分支独有提交失败：%w", err)
	}
	// BehindCommits: commits present in base that are not in target (target..base).
	behind, behindTrunc, err := readCommitLog(ctx, root, 100, []string{targetRef + ".." + baseRef}, nil)
	if err != nil {
		return out, fmt.Errorf("读取当前分支独有提交失败：%w", err)
	}
	out.AheadCommits = ahead
	out.BehindCommits = behind
	out.Truncated = aheadTrunc || behindTrunc

	filesRes, err := execute(ctx, root, "diff", "--name-status", "-z", "--find-renames", "--no-ext-diff", "--no-textconv", baseRef, targetRef, "--")
	if err != nil {
		return out, fmt.Errorf("读取分支文件差异失败：%w", err)
	}
	out.Files = parseCommitFiles(filesRes.stdout.data.String())
	out.Truncated = out.Truncated || filesRes.stdout.truncated

	diffRes, err := execute(ctx, root, "diff", "--patch", "--no-ext-diff", "--no-textconv", "--no-color", baseRef, targetRef, "--")
	if err != nil {
		return out, fmt.Errorf("读取分支补丁差异失败：%w", err)
	}
	out.Diff = diffRes.stdout.String()
	out.Truncated = out.Truncated || diffRes.stdout.truncated
	return out, nil
}
