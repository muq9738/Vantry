package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

// WorktreeStatus describes whether a checkout is an isolated Git worktree and
// how cleanly it can merge back into its main repository checkout.
type WorktreeStatus struct {
	IsWorktree      bool   `json:"isWorktree"`
	Managed         bool   `json:"managed"`
	Exists          bool   `json:"exists"`
	Path            string `json:"path,omitempty"`
	Repository      string `json:"repository,omitempty"`
	Branch          string `json:"branch,omitempty"`
	BaseBranch      string `json:"baseBranch,omitempty"`
	UnmergedCommits int    `json:"unmergedCommits"`
	Dirty           bool   `json:"dirty"`
	CanMerge        bool   `json:"canMerge"`
	CanCleanup      bool   `json:"canCleanup"`
	Note            string `json:"note,omitempty"`
}

// WorktreeMergeResult reports the outcome of MergeAndCleanupWorktree.
type WorktreeMergeResult struct {
	Repository    string `json:"repository"`
	TargetBranch  string `json:"targetBranch"`
	MergedBranch  string `json:"mergedBranch"`
	CleanedUp     bool   `json:"cleanedUp"`
	BranchDeleted bool   `json:"branchDeleted"`
	Output        string `json:"output,omitempty"`
}

// InspectWorktree inspects a thread's checkout (and managed workspace record when
// dataDir and threadID are available) to report worktree status and mergeability.
func InspectWorktree(ctx context.Context, dataDir, threadID, checkoutPath string, spec *api.WorkspaceSpec) (WorktreeStatus, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()

	var out WorktreeStatus
	if spec != nil && spec.Kind == workspace.KindWorktree {
		out.IsWorktree = true
		out.Branch = spec.Branch
		out.BaseBranch = spec.BaseRef
		out.Repository = spec.Repository
	}
	if dataDir != "" && threadID != "" {
		if managed, err := workspace.InspectManagedWorkspace(ctx, dataDir, threadID); err == nil {
			if spec != nil && spec.Kind == workspace.KindWorktree || managed.Exists {
				out.Managed = true
				out.Exists = managed.Exists
				out.Dirty = managed.Dirty
				out.UnmergedCommits = managed.UnmergedCommits
				if out.Path == "" {
					out.Path = managed.Path
				}
			}
		}
	}

	if checkoutPath != "" {
		if _, statErr := os.Stat(checkoutPath); statErr == nil {
			root, err := checkoutRoot(ctx, checkoutPath)
			if err != nil {
				return out, err
			}
			out.Exists = true
			out.Path = root
			gitDirRaw, err := command(ctx, root, "rev-parse", "--absolute-git-dir")
			if err != nil {
				return out, err
			}
			commonDirRaw, err := command(ctx, root, "rev-parse", "--git-common-dir")
			if err != nil {
				return out, err
			}
			gitDir := filepath.Clean(strings.TrimSpace(gitDirRaw))
			commonDir := strings.TrimSpace(commonDirRaw)
			if !filepath.IsAbs(commonDir) {
				commonDir = filepath.Join(root, commonDir)
			}
			commonDir = filepath.Clean(commonDir)
			if evalCommon, err := filepath.EvalSymlinks(commonDir); err == nil {
				commonDir = evalCommon
			}
			if evalGit, err := filepath.EvalSymlinks(gitDir); err == nil {
				gitDir = evalGit
			}
			if gitDir != commonDir {
				out.IsWorktree = true
				if out.Repository == "" {
					out.Repository = filepath.Dir(commonDir)
				}
			}
			if out.IsWorktree {
				if br, err := command(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
					out.Branch = strings.TrimSpace(br)
				}
				if st, err := status(ctx, root); err == nil {
					out.Dirty = out.Dirty || strings.TrimSpace(st) != ""
				}
			}
		}
	}
	if !out.IsWorktree {
		return out, nil
	}
	if out.Repository != "" {
		if repoBranch, err := command(ctx, out.Repository, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			out.BaseBranch = strings.TrimSpace(repoBranch)
		}
		if out.Branch != "" && out.BaseBranch != "" && out.UnmergedCommits == 0 {
			if cntRaw, err := command(ctx, out.Repository, "rev-list", "--count", "refs/heads/"+out.Branch, "--not", "refs/heads/"+out.BaseBranch, "--"); err == nil {
				if n, convErr := strconv.Atoi(strings.TrimSpace(cntRaw)); convErr == nil {
					out.UnmergedCommits = n
				}
			}
		}
	}
	out.CanCleanup = out.Exists
	out.CanMerge = out.Exists && !out.Dirty && out.Repository != "" && out.Branch != "" && out.BaseBranch != "" && out.Branch != out.BaseBranch
	if out.Dirty {
		out.Note = "当前 Worktree 有未提交更改，请先提交或回滚后再合并"
	} else if out.Branch == "" {
		out.Note = "Worktree 当前处于 detached HEAD 状态，无法按分支合并"
	} else if out.UnmergedCommits == 0 && out.Exists {
		out.Note = "没有新的未合并提交，可直接清理 Worktree"
	}
	return out, nil
}

// MergeAndCleanupWorktree merges a clean worktree branch into the main
// repository's active branch, then removes the worktree and deletes the branch.
// Merge conflicts automatically abort cleanly in the main repository and leave
// the worktree intact.
func MergeAndCleanupWorktree(ctx context.Context, dataDir, threadID, checkoutPath string, spec *api.WorkspaceSpec) (WorktreeMergeResult, error) {
	ctx, cancel := withDefaultTimeout(ctx, 2*time.Minute)
	defer cancel()

	wt, err := InspectWorktree(ctx, dataDir, threadID, checkoutPath, spec)
	if err != nil {
		return WorktreeMergeResult{}, err
	}
	if !wt.IsWorktree || !wt.Exists {
		return WorktreeMergeResult{}, errors.New("当前会话没有可合并的独立 Worktree")
	}
	if wt.Dirty {
		return WorktreeMergeResult{}, errors.New("Worktree 有未提交或未跟踪更改，请先提交或回滚后再合并清理")
	}
	if wt.Repository == "" || wt.Branch == "" {
		return WorktreeMergeResult{}, errors.New("无法定位主仓库或 Worktree 分支")
	}

	repoRoot, err := checkoutRoot(ctx, wt.Repository)
	if err != nil {
		return WorktreeMergeResult{}, err
	}
	targetRef, err := command(ctx, repoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return WorktreeMergeResult{}, errors.New("主仓库当前处于 detached HEAD，请先切换回目标分支")
	}
	targetBranch := strings.TrimSpace(targetRef)
	if targetBranch == wt.Branch {
		return WorktreeMergeResult{}, errors.New("主仓库当前检出的就是该 Worktree 分支，无需合并")
	}

	output, err := Merge(ctx, repoRoot, wt.Branch)
	if err != nil {
		return WorktreeMergeResult{
			Repository:   repoRoot,
			TargetBranch: targetBranch,
			MergedBranch: wt.Branch,
			Output:       output,
		}, err
	}

	res := WorktreeMergeResult{
		Repository:   repoRoot,
		TargetBranch: targetBranch,
		MergedBranch: wt.Branch,
		Output:       output,
	}
	if err := removeWorktreeCheckout(ctx, dataDir, threadID, wt.Path, repoRoot, wt.Managed, false); err != nil {
		return res, fmt.Errorf("已成功合并分支 %s 到 %s，但移除 Worktree 时出错：%w", wt.Branch, targetBranch, err)
	}
	res.CleanedUp = true
	if _, delErr := DeleteBranch(ctx, repoRoot, wt.Branch, false); delErr == nil {
		res.BranchDeleted = true
	}
	return res, nil
}

// CleanupWorktreeOnly removes an isolated worktree without merging it.
func CleanupWorktreeOnly(ctx context.Context, dataDir, threadID, checkoutPath string, spec *api.WorkspaceSpec, forceDirty bool) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx, time.Minute)
	defer cancel()

	wt, err := InspectWorktree(ctx, dataDir, threadID, checkoutPath, spec)
	if err != nil {
		return "", err
	}
	if !wt.IsWorktree {
		return "", errors.New("当前工作区不是独立 Worktree")
	}
	if wt.Dirty && !forceDirty {
		return "", errors.New("Worktree 有未提交更改；请先提交/丢弃更改，或确认强制清理")
	}
	repoRoot := wt.Repository
	if repoRoot != "" {
		if resolved, rootErr := checkoutRoot(ctx, repoRoot); rootErr == nil {
			repoRoot = resolved
		}
	}
	if err := removeWorktreeCheckout(ctx, dataDir, threadID, wt.Path, repoRoot, wt.Managed, forceDirty); err != nil {
		return "", err
	}
	if wt.Branch != "" && repoRoot != "" {
		_, _ = DeleteBranch(ctx, repoRoot, wt.Branch, forceDirty)
	}
	return fmt.Sprintf("已清理 Worktree（%s）", nonEmptyStr(wt.Branch, wt.Path)), nil
}

func removeWorktreeCheckout(ctx context.Context, dataDir, threadID, wtPath, repoRoot string, managed, force bool) error {
	if managed && dataDir != "" && threadID != "" {
		return workspace.CleanupManagedWorkspace(ctx, dataDir, threadID, force)
	}
	if wtPath == "" || repoRoot == "" {
		return errors.New("缺少 Worktree 路径或主仓库路径")
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "--", wtPath)
	_, err := command(ctx, repoRoot, args...)
	return err
}

func nonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
