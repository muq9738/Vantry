package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

// Images are read on the Core host, including remote threads. os.Root keeps
// relative links and symlinks within this thread's actual workspace.
func (s *workbenchService) replyImage(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	directory, err := s.directory(ctx, request.ThreadID)
	if err != nil {
		return api.WorkbenchResponse{}, err
	}
	data, err := readReplyImage(directory, request.Name)
	return api.WorkbenchResponse{Data: data}, err
}

func readReplyImage(directory, name string) ([]byte, error) {
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(directory, name)
		if err != nil {
			return nil, err
		}
		name = rel
	}
	if !filepath.IsLocal(name) {
		return nil, errors.New("图片必须位于当前会话工作区")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	const limit = 8 << 20
	if !stat.Mode().IsRegular() || stat.Size() > limit {
		return nil, errors.New("图片必须为 8 MiB 以内的普通文件")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("图片过大")
	}
	if strings.EqualFold(filepath.Ext(name), ".svg") {
		return data, nil
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return nil, errors.New("图片像素过大")
	}
	return data, nil
}
