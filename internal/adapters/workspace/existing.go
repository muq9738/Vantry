package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/muq9738/Vantry/pkg/api"
)

// Existing resolves a prepared checkout without creating one as a side effect
// of opening Git or a terminal pane.
func Existing(ctx context.Context, dataDir string, spec api.WorkspaceSpec, owner string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validComponent(owner); err != nil {
		return "", err
	}
	if dataDir == "" {
		return "", errors.New("workspace data directory is required")
	}
	absoluteData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	normalized, err := normalizeSpec(absoluteData, spec, owner)
	if err != nil {
		return "", err
	}
	var record prepared
	found, err := readJSON(filepath.Join(absoluteData, "workspaces", owner, "prepared.json"), &record)
	if err != nil {
		return "", err
	}
	if found {
		repositoryMatches := record.Spec.Repository == normalized.Repository ||
			(record.Spec.Repository != "" && normalized.Repository != "" && samePath(record.Spec.Repository, normalized.Repository))
		if record.Owner != owner || record.Spec.Kind != normalized.Kind ||
			!samePath(record.Spec.Path, normalized.Path) || !repositoryMatches ||
			record.Spec.Branch != normalized.Branch || record.Spec.BaseRef != normalized.BaseRef {
			return "", errors.New("工作区请求与此会话实际准备的目录不符")
		}
		if normalized.Kind == KindWorktree && samePath(record.Path, normalized.Repository) {
			return "", errors.New("隔离 worktree 不能使用主仓库目录")
		}
		if err := verifyPrepared(ctx, record); err != nil {
			return "", err
		}
		return record.Path, nil
	}
	if normalized.Kind == KindDirectory {
		info, err := os.Stat(normalized.Path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("工作区不是目录")
		}
		return normalized.Path, nil
	}
	return "", errors.New("此任务的 worktree 尚未准备完成")
}
