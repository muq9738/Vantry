package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

const (
	defaultRemoteOpTimeout = 2 * time.Minute
	defaultLocalOpTimeout  = 30 * time.Second
	defaultQueryTimeout    = 15 * time.Second
)

// StashEntry represents a single entry in the repository's stash reflog.
type StashEntry struct {
	Index     int       `json:"index"`
	Ref       string    `json:"ref"`
	Branch    string    `json:"branch,omitempty"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func lockCheckout(ctx context.Context, root string) (*os.File, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lock, err := workspace.LockDirectory(lockCtx, root)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("等待工作区时取消：%w", ctx.Err())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("工作区仍被任务占用，请稍后重试：%w", err)
		}
		return nil, fmt.Errorf("无法锁定工作区：%w", err)
	}
	return lock, nil
}

func validateBranchName(ctx context.Context, root, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return errors.New("分支名称无效")
	}
	checked, err := command(ctx, root, "check-ref-format", "--branch", branch)
	if err != nil {
		return fmt.Errorf("分支名称格式无效：%w", err)
	}
	if strings.TrimSpace(checked) != branch {
		return errors.New("分支名称必须是明确的本地分支名")
	}
	return nil
}

func resolveRemoteName(ctx context.Context, root, remote, branch string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" && branch != "" {
		if configured, err := command(ctx, root, "config", "--get", "branch."+branch+".remote"); err == nil {
			configured = strings.TrimSpace(configured)
			if configured != "" && configured != "." {
				remote = configured
			}
		}
	}
	if remote == "" {
		remote = "origin"
	}
	if strings.HasPrefix(remote, "-") || strings.Contains(remote, "::") || strings.IndexFunc(remote, unicode.IsSpace) >= 0 || strings.IndexFunc(remote, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("远端名称无效：%q", remote)
	}
	fetchURLs, err := command(ctx, root, "remote", "get-url", "--all", "--", remote)
	if err != nil {
		return "", fmt.Errorf("未配置远端 %q：%w", remote, err)
	}
	pushURLs, err := command(ctx, root, "remote", "get-url", "--push", "--all", "--", remote)
	if err != nil {
		return "", fmt.Errorf("无法读取远端 %q 的推送地址：%w", remote, err)
	}
	for _, raw := range strings.Split(fetchURLs+"\n"+pushURLs, "\n") {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		if strings.Contains(u, "::") {
			return "", fmt.Errorf("拒绝执行包含外部 helper 协议的远端地址：%s", remote)
		}
	}
	return remote, nil
}

func currentLocalBranch(ctx context.Context, root string) (string, bool, error) {
	ref, err := command(ctx, root, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if exitCode(err, 1) {
			return "", false, nil
		}
		return "", false, err
	}
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", false, nil
	}
	return strings.TrimPrefix(ref, "refs/heads/"), true, nil
}

// Fetch updates remote-tracking branches for remote (defaults to the current
// branch's configured remote or "origin").
func Fetch(ctx context.Context, path, remote string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultRemoteOpTimeout)
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

	branch, _, err := currentLocalBranch(ctx, root)
	if err != nil {
		return "", err
	}
	resolvedRemote, err := resolveRemoteName(ctx, root, remote, branch)
	if err != nil {
		return "", err
	}
	result, err := execute(ctx, root, "fetch", "--prune", "--no-recurse-submodules", "--", resolvedRemote)
	output := result.text(false)
	if err != nil {
		return output, fmt.Errorf("获取远端更新失败：%w", err)
	}
	return output, nil
}

// PullFFOnly performs a strict fast-forward-only pull on the checked-out local
// branch. It refuses to run while any merge/rebase/cherry-pick/conflict is pending.
func PullFFOnly(ctx context.Context, path, remote, branch string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultRemoteOpTimeout)
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

	pending, err := pendingOperation(ctx, root)
	if err != nil {
		return "", err
	}
	if pending != "" {
		return "", fmt.Errorf("存在未完成的 Git 合并或变基操作（%s），请先完成或中止后再拉取", pending)
	}
	conflict, err := conflicts(ctx, root)
	if err != nil {
		return "", err
	}
	if conflict {
		return "", errors.New("工作区存在未解决的冲突，请先解决后再拉取")
	}

	currentBranch, onBranch, err := currentLocalBranch(ctx, root)
	if err != nil {
		return "", err
	}
	if !onBranch {
		return "", errors.New("当前处于 detached HEAD 状态，请先切换到本地分支后再拉取")
	}

	remote = strings.TrimSpace(remote)
	branch = strings.TrimSpace(branch)
	if branch == "" {
		if remote == "" {
			if mergeRef, cfgErr := command(ctx, root, "config", "--get", "branch."+currentBranch+".merge"); cfgErr == nil {
				mergeRef = strings.TrimSpace(mergeRef)
				if strings.HasPrefix(mergeRef, "refs/heads/") {
					branch = strings.TrimPrefix(mergeRef, "refs/heads/")
				}
			}
		}
		if branch == "" {
			branch = currentBranch
		}
	}
	if err := validateBranchName(ctx, root, branch); err != nil {
		return "", err
	}
	resolvedRemote, err := resolveRemoteName(ctx, root, remote, currentBranch)
	if err != nil {
		return "", err
	}

	result, pullErr := execute(ctx, root, "pull", "--ff-only", "--no-rebase", "--no-autostash", "--no-recurse-submodules", "--", resolvedRemote, branch)
	output := result.text(false)
	if pullErr != nil {
		errText := pullErr.Error()
		if strings.Contains(errText, "Not possible to fast-forward") || strings.Contains(errText, "diverging") {
			return output, fmt.Errorf("无法快进拉取（--ff-only），本地分支与远端已分叉，请先合并或变基：%w", pullErr)
		}
		return output, fmt.Errorf("快进拉取失败：%w", pullErr)
	}
	return output, nil
}

// Push pushes a local branch to a configured remote without ever force-pushing.
// When setUpstream is true, Git configures upstream tracking (--set-upstream).
func Push(ctx context.Context, path, remote, branch string, setUpstream bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultRemoteOpTimeout)
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

	pending, err := pendingOperation(ctx, root)
	if err != nil {
		return "", err
	}
	if pending != "" {
		return "", fmt.Errorf("存在未完成的 Git 操作（%s），请先处理后再推送", pending)
	}
	conflict, err := conflicts(ctx, root)
	if err != nil {
		return "", err
	}
	if conflict {
		return "", errors.New("工作区存在未解决的冲突，请先解决后再推送")
	}

	branch = strings.TrimSpace(branch)
	if branch == "" {
		currentBranch, onBranch, branchErr := currentLocalBranch(ctx, root)
		if branchErr != nil {
			return "", branchErr
		}
		if !onBranch {
			return "", errors.New("当前处于 detached HEAD 状态，请先切换到本地分支后再推送")
		}
		branch = currentBranch
	}
	if err := validateBranchName(ctx, root, branch); err != nil {
		return "", err
	}
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}"); err != nil {
		return "", fmt.Errorf("本地分支 %q 尚无提交或不存在，无法推送：%w", branch, err)
	}
	resolvedRemote, err := resolveRemoteName(ctx, root, remote, branch)
	if err != nil {
		return "", err
	}

	args := []string{"push", "--no-recurse-submodules"}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	args = append(args, "--", resolvedRemote, "refs/heads/"+branch+":refs/heads/"+branch)

	result, err := execute(ctx, root, args...)
	output := result.text(false)
	if err != nil {
		return output, fmt.Errorf("推送到远端失败：%w", err)
	}
	return output, nil
}

// StashPush saves dirty workspace changes onto the stash stack.
func StashPush(ctx context.Context, path, message string, includeUntracked bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()

	message = strings.TrimSpace(message)
	if len(message) > 16<<10 {
		return "", errors.New("stash 说明长度最多 16 KiB")
	}

	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)

	pending, err := pendingOperation(ctx, root)
	if err != nil {
		return "", err
	}
	if pending != "" {
		return "", fmt.Errorf("存在未完成的 Git 操作（%s），请先处理后再贮藏", pending)
	}
	conflict, err := conflicts(ctx, root)
	if err != nil {
		return "", err
	}
	if conflict {
		return "", errors.New("工作区存在未解决的冲突，请先解决后再贮藏")
	}
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return "", fmt.Errorf("当前分支尚无初始提交，无法创建 stash：%w", err)
	}

	untrackedMode := "no"
	if includeUntracked {
		untrackedMode = "all"
	}
	dirty, err := command(ctx, root, "status", "--porcelain=v1", "--untracked-files="+untrackedMode, "--ignore-submodules=none")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dirty) == "" {
		return "", errors.New("当前工作区没有可贮藏的改动")
	}

	args := []string{"stash", "push"}
	if includeUntracked {
		args = append(args, "--include-untracked")
	}
	if message != "" {
		args = append(args, "--message="+message)
	}
	result, err := execute(ctx, root, args...)
	output := result.text(false)
	if err != nil {
		return output, fmt.Errorf("创建 stash 失败：%w", err)
	}
	return output, nil
}

// StashPop applies and removes a selected stash, defaulting to stash@{0}.
// The optional argument preserves callers that previously popped the top entry.
func StashPop(ctx context.Context, path string, refs ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ref, err := selectedStashRef(refs)
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

	pending, err := pendingOperation(ctx, root)
	if err != nil {
		return "", err
	}
	if pending != "" {
		return "", fmt.Errorf("存在未完成的 Git 操作（%s），请先处理后再恢复 stash", pending)
	}
	conflict, err := conflicts(ctx, root)
	if err != nil {
		return "", err
	}
	if conflict {
		return "", errors.New("工作区存在未解决的冲突，请先解决后再恢复 stash")
	}
	if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "refs/stash"); err != nil {
		if exitCode(err, 1) {
			return "", errors.New("当前没有可恢复的 stash 记录")
		}
		return "", err
	}

	result, err := execute(ctx, root, "stash", "pop", "--", ref)
	output := result.text(false)
	if err != nil {
		return output, fmt.Errorf("恢复 stash 失败（原 stash 记录已保留，请在终端检查冲突）：%w", err)
	}
	return output, nil
}

// StashList returns the stash stack ordered from newest (index 0) to oldest.
func StashList(ctx context.Context, path string) ([]StashEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()

	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	result, err := execute(ctx, root, "stash", "list", "--format=%gd%x09%cI%x09%gs")
	if err != nil {
		return nil, err
	}
	raw := result.stdout.data.String()
	if result.stdout.truncated {
		if idx := strings.LastIndex(raw, "\n"); idx >= 0 {
			raw = raw[:idx+1]
		} else {
			raw = ""
		}
	}
	entries := []StashEntry{}
	for i, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		ref := strings.TrimSpace(parts[0])
		index := parseStashIndex(ref, i)
		var createdAt time.Time
		if ts, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(parts[1])); parseErr == nil {
			createdAt = ts
		}
		subject := parts[2]
		entries = append(entries, StashEntry{
			Index:     index,
			Ref:       ref,
			Branch:    parseStashBranch(subject),
			Message:   subject,
			CreatedAt: createdAt,
		})
	}
	return entries, nil
}

func parseStashIndex(ref string, fallback int) int {
	if strings.HasPrefix(ref, "stash@{") && strings.HasSuffix(ref, "}") {
		inner := ref[len("stash@{") : len(ref)-1]
		if n, err := strconv.Atoi(inner); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

func parseStashBranch(subject string) string {
	for _, prefix := range []string{"On ", "WIP on "} {
		if strings.HasPrefix(subject, prefix) {
			rest := strings.TrimPrefix(subject, prefix)
			if branch, _, ok := strings.Cut(rest, ":"); ok {
				return strings.TrimSpace(branch)
			}
		}
	}
	return ""
}

// AheadBehind returns how many commits the checked-out local branch is ahead of
// and behind its configured upstream branch. When HEAD is detached, unborn, or
// has no configured/fetched upstream, it returns (0, 0, "", nil).
func AheadBehind(ctx context.Context, path string) (ahead int, behind int, upstream string, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, "", err
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()

	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return 0, 0, "", err
	}
	return aheadBehind(ctx, root)
}

func aheadBehind(ctx context.Context, root string) (ahead int, behind int, upstream string, err error) {
	if _, onBranch, branchErr := currentLocalBranch(ctx, root); branchErr != nil {
		return 0, 0, "", branchErr
	} else if !onBranch {
		return 0, 0, "", nil
	}
	if _, verifyErr := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); verifyErr != nil {
		if exitCode(verifyErr, 1) {
			return 0, 0, "", nil
		}
		return 0, 0, "", verifyErr
	}

	upstreamOut, upErr := command(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if upErr != nil {
		if exitCode(upErr, 128) {
			return 0, 0, "", nil
		}
		return 0, 0, "", upErr
	}
	upstream = strings.TrimSpace(upstreamOut)
	if upstream == "" {
		return 0, 0, "", nil
	}

	countsOut, countErr := command(ctx, root, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if countErr != nil {
		return 0, 0, "", countErr
	}
	fields := strings.Fields(countsOut)
	if len(fields) != 2 {
		return 0, 0, "", fmt.Errorf("无法解析领先/落后提交计数：%q", countsOut)
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, "", fmt.Errorf("无法解析领先提交数：%w", err)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, "", fmt.Errorf("无法解析落后提交数：%w", err)
	}
	return ahead, behind, upstream, nil
}
