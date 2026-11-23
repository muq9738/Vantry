package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/muq9738/Vantry/pkg/api"
)

// ChatSpec plans a Core-owned directory for a conversation with no project.
// It never substitutes the launcher's cwd or the user's home directory.
func ChatSpec(dataDir, owner string) (api.WorkspaceSpec, error) {
	if err := validComponent(owner); err != nil {
		return api.WorkspaceSpec{}, err
	}
	if dataDir == "" {
		return api.WorkspaceSpec{}, errors.New("会话数据目录不能为空")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return api.WorkspaceSpec{}, err
	}
	return api.WorkspaceSpec{Kind: KindDirectory, Path: filepath.Join(root, "conversations", owner)}, nil
}

// PrepareChatDirectory runs only at execution time, never during model queries
// or command decisions. Explicit project requests use their original path.
func PrepareChatDirectory(dataDir, owner string) error {
	spec, err := ChatSpec(dataDir, owner)
	if err != nil {
		return err
	}
	for _, path := range []string{filepath.Dir(spec.Path), spec.Path} {
		if err := privateDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

// ThreadDirectory resolves either the chosen project or an existing chat
// directory. Querying a thread cannot create a new directory or checkout.
func ThreadDirectory(ctx context.Context, dataDir string, spec *api.WorkspaceSpec, owner string) (string, error) {
	if spec != nil {
		return Existing(ctx, dataDir, *spec, owner)
	}
	chat, err := ChatSpec(dataDir, owner)
	if err != nil {
		return "", err
	}
	path, err := Existing(ctx, dataDir, chat, owner)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("此会话的独立目录尚未准备，发送消息后即可使用")
	}
	return path, err
}
