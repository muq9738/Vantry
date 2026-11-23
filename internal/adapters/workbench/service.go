// Package workbench supplies explicit local model/workspace queries alongside
// Core's durable command service. It does not submit prompts or prepare Git work.
package workbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

const displayLimit = 256 * 1024

// Service preserves Core as the sole writer of its state.
type Service struct {
	api.Service
	provider       provider.Provider
	dataDir        string
	launchTerminal func(context.Context, string) error
}

func New(service api.Service, executor provider.Provider, dataDir string) *Service {
	return &Service{Service: service, provider: executor, dataDir: dataDir, launchTerminal: openTerminal}
}

func (s *Service) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	catalog, ok := s.provider.(provider.ModelCatalog)
	if !ok {
		return nil, &api.Error{Code: api.InvalidCommand, Message: "当前执行器不提供模型目录"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return catalog.Models(ctx)
}

func (s *Service) workspacePath(ctx context.Context, threadID string) (string, error) {
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, thread := range snapshot.Threads {
		if thread.ID != threadID {
			continue
		}
		path, err := workspace.ThreadDirectory(ctx, s.dataDir, thread.Workspace, thread.ID)
		if err != nil {
			return "", &api.Error{Code: api.NotReady, Message: err.Error()}
		}
		return path, nil
	}
	return "", &api.Error{Code: api.NotFound, Message: "会话不存在"}
}

func (s *Service) InspectWorkspace(ctx context.Context, threadID string) (api.WorkspaceChanges, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path, err := s.workspacePath(ctx, threadID)
	if err != nil {
		return api.WorkspaceChanges{}, err
	}
	result := api.WorkspaceChanges{Path: path, Files: []api.WorkspaceFile{}}
	inside, _, err := git(ctx, path, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.Contains(err.Error(), "not a git repository") {
			return result, nil // ordinary non-Git directory
		}
		return result, err
	}
	if strings.TrimSpace(inside) != "true" {
		return result, nil
	}
	result.IsGit = true
	branch, _, branchErr := git(ctx, path, "symbolic-ref", "--short", "-q", "HEAD")
	if branchErr != nil {
		branch, _, err = git(ctx, path, "rev-parse", "--short", "HEAD")
		if err != nil {
			return result, err
		}
	}
	result.Branch = strings.TrimSpace(branch)
	status, truncated, err := git(ctx, path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	if err != nil {
		return result, err
	}
	result.Truncated = truncated
	result.Files = parseStatus(status)
	prefix, _, err := git(ctx, path, "rev-parse", "--show-prefix")
	if err != nil {
		return result, err
	}
	prefix = strings.TrimSuffix(prefix, "\n")
	for i := range result.Files {
		result.Files[i].Path = strings.TrimPrefix(result.Files[i].Path, prefix)
	}

	var diff cappedBuffer
	// Separating staged and working changes also works in repositories without
	// an initial commit. No external diff/textconv helpers are executed.
	for _, item := range []struct {
		label string
		args  []string
	}{
		{"已暂存", []string{"diff", "--relative", "--cached", "--no-ext-diff", "--no-textconv", "--no-color", "--", "."}},
		{"未暂存", []string{"diff", "--relative", "--no-ext-diff", "--no-textconv", "--no-color", "--", "."}},
	} {
		text, cut, err := git(ctx, path, item.args...)
		if err != nil {
			return result, err
		}
		result.Truncated = result.Truncated || cut
		if text != "" {
			fmt.Fprintf(&diff, "%s\n%s\n", item.label, text)
		}
	}
	for _, file := range result.Files {
		if file.Status != "??" || diff.truncated {
			continue
		}
		// Git paths are relative to this query's working directory. Never follow
		// symlinks or special files when displaying untracked file contents.
		full := filepath.Join(path, file.Path)
		rel, err := filepath.Rel(path, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(f, 32*1024+1))
		_ = f.Close()
		if err != nil {
			continue
		}
		fmt.Fprintf(&diff, "未跟踪：%s\n", file.Path)
		if bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
			fmt.Fprintln(&diff, "（二进制文件）")
			continue
		}
		if len(content) > 32*1024 {
			content = content[:32*1024]
			result.Truncated = true
		}
		diff.Write(content)
		fmt.Fprintln(&diff)
	}
	result.Diff = strings.ToValidUTF8(diff.String(), "�")
	result.Truncated = result.Truncated || diff.truncated
	return result, nil
}

func (s *Service) OpenTerminal(ctx context.Context, threadID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path, err := s.workspacePath(ctx, threadID)
	if err != nil {
		return err
	}
	return s.launchTerminal(ctx, path)
}

func openTerminal(ctx context.Context, path string) error {
	if runtime.GOOS != "darwin" {
		return &api.Error{Code: api.InvalidCommand, Message: "当前系统尚未接入外部终端；工作区：" + path}
	}
	// An absolute directory argument opens a shell in Terminal; no user text is
	// interpolated into a shell command or AppleScript.
	if output, err := exec.CommandContext(ctx, "/usr/bin/open", "-a", "Terminal", path).CombinedOutput(); err != nil {
		return fmt.Errorf("打开 Terminal：%w %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func parseStatus(text string) []api.WorkspaceFile {
	var files []api.WorkspaceFile
	parts := strings.Split(text, "\x00")
	for i := 0; i < len(parts)-1; i++ {
		item := parts[i]
		if len(item) < 4 || item[2] != ' ' {
			continue
		}
		files = append(files, api.WorkspaceFile{Status: item[:2], Path: item[3:]})
		if strings.ContainsAny(item[:2], "RC") {
			i++ // porcelain -z gives destination then original path for renames
		}
	}
	return files
}

func git(ctx context.Context, path string, args ...string) (string, bool, error) {
	arguments := append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-C", path}, args...)
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Env = append(workspace.GitEnvironment(), "LC_ALL=C")
	var output, diagnostics cappedBuffer
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		return "", false, fmt.Errorf("git %s: %w %s", args[0], err, strings.TrimSpace(diagnostics.String()))
	}
	return output.String(), output.truncated, nil
}

type cappedBuffer struct {
	data      bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Len() int       { return b.data.Len() }
func (b *cappedBuffer) String() string { return b.data.String() }
func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := displayLimit - b.Len()
	if n > remaining {
		p, b.truncated = p[:remaining], true
	}
	_, _ = b.data.Write(p)
	return n, nil
}

var _ api.WorkbenchQueries = (*Service)(nil)
