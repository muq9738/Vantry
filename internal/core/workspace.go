package core

import (
	"os"
	"path/filepath"

	"github.com/muq9738/Vantry/pkg/api"
)

// childWorkspace plans the default isolation without performing Git mutations.
// The provider prepares and verifies the resulting workspace before execution.
// An explicitly supplied child workspace bypasses this default entirely.
func childWorkspace(parent *api.WorkspaceSpec) *api.WorkspaceSpec {
	if parent == nil {
		return nil
	}
	if parent.Kind == "worktree" {
		return &api.WorkspaceSpec{Kind: "worktree", Repository: parent.Repository,
			BaseRef: firstNonEmpty(parent.Branch, parent.BaseRef)}
	}
	if parent.Kind != "directory" || parent.Path == "" {
		return parent
	}
	path, err := filepath.Abs(parent.Path)
	if err != nil {
		return parent
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	for {
		if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return &api.WorkspaceSpec{Kind: "worktree", Repository: path}
		}
		next := filepath.Dir(path)
		if next == path {
			return parent
		}
		path = next
	}
}
