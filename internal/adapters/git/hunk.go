package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

// HunkRequest targets one unified-diff hunk in a single checkout file.
type HunkRequest struct {
	Branch         string `json:"branch,omitempty"`
	FilePath       string `json:"file"`
	Staged         bool   `json:"staged"`
	HunkIndex      int    `json:"hunkIndex"`
	Action         string `json:"action"` // stage | unstage | revert
	ExpectedHeader string `json:"header,omitempty"`
}

type parsedDiffHunk struct {
	Header string
	Lines  []string
}

// ApplyHunk stages, unstages, or reverts a single unified-diff hunk for FilePath.
// It re-reads the live git diff under the directory lock and verifies ExpectedHeader
// when supplied so stale UI clicks cannot patch the wrong hunk.
func ApplyHunk(ctx context.Context, path string, req HunkRequest) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()

	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "stage":
		if req.Staged {
			return "", errors.New("该变更块已在暂存区")
		}
	case "unstage":
		if !req.Staged {
			return "", errors.New("仅已暂存的变更块可取消暂存")
		}
	case "revert":
		if req.Staged {
			return "", errors.New("请先取消暂存该块，再撤回工作区修改")
		}
	default:
		return "", fmt.Errorf("不支持的块级 Git 操作：%s", req.Action)
	}

	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	if err := checkPath(root, req.FilePath); err != nil {
		return "", err
	}

	lockCtx, lockCancel := context.WithTimeout(ctx, 2*time.Second)
	defer lockCancel()
	lock, err := workspace.LockDirectory(lockCtx, root)
	if err != nil {
		return "", fmt.Errorf("工作区正在使用，请稍后重试：%w", err)
	}
	defer workspace.Unlock(lock)

	if req.Branch != "" && req.Branch != "detached HEAD" {
		branch, err := command(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return "", errors.New("请先切换到本地分支再操作 Git")
		}
		if strings.TrimSpace(branch) != req.Branch {
			return "", errors.New("当前分支已变化，请刷新后再操作")
		}
	}
	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}

	diffArgs := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--unified=3"}
	if req.Staged {
		diffArgs = append(diffArgs, "--cached")
	}
	diffArgs = append(diffArgs, "--", req.FilePath)
	rawDiff, err := command(ctx, root, diffArgs...)
	if err != nil {
		return "", err
	}
	header, hunks, err := splitSingleFileDiffHunks(rawDiff)
	if err != nil {
		return "", err
	}
	if req.HunkIndex < 0 || req.HunkIndex >= len(hunks) {
		return "", errors.New("目标变更块已变化或不存在，请刷新差异后重试")
	}
	chosen := hunks[req.HunkIndex]
	if expected := strings.TrimSpace(req.ExpectedHeader); expected != "" && strings.TrimSpace(chosen.Header) != expected {
		return "", errors.New("目标变更块内容已更新，请刷新差异后重试")
	}

	patch := strings.Join(header, "\n") + "\n" + strings.Join(chosen.Lines, "\n") + "\n"
	applyArgs := []string{"apply", "--recount"}
	switch action {
	case "stage":
		applyArgs = append(applyArgs, "--cached")
	case "unstage":
		applyArgs = append(applyArgs, "--cached", "--reverse")
	case "revert":
		applyArgs = append(applyArgs, "--reverse")
	}
	applyArgs = append(applyArgs, "-")
	res, err := executeWithInput(ctx, root, nil, strings.NewReader(patch), applyArgs...)
	if err != nil {
		return res.text(false), err
	}
	return res.text(false), nil
}

func splitSingleFileDiffHunks(raw string) ([]string, []parsedDiffHunk, error) {
	trimmed := strings.TrimSuffix(raw, "\n")
	if strings.TrimSpace(trimmed) == "" {
		return nil, nil, errors.New("该文件当前没有可操作的文本差异（未跟踪或二进制文件请使用文件级操作）")
	}
	lines := strings.Split(trimmed, "\n")
	var header []string
	var hunks []parsedDiffHunk
	var current *parsedDiffHunk
	for _, line := range lines {
		if strings.HasPrefix(line, "@@ ") {
			if current != nil {
				hunks = append(hunks, *current)
			}
			current = &parsedDiffHunk{
				Header: line,
				Lines:  []string{line},
			}
			continue
		}
		if current != nil {
			current.Lines = append(current.Lines, line)
		} else {
			header = append(header, line)
		}
	}
	if current != nil {
		hunks = append(hunks, *current)
	}
	if len(header) == 0 || len(hunks) == 0 {
		return nil, nil, errors.New("无法解析该文件的差异块（可能是空文件或二进制更改）")
	}
	return header, hunks, nil
}
