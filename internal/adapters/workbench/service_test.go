package workbench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type snapshotService struct {
	api.Service
	snapshot api.Snapshot
}

func (s snapshotService) Snapshot(context.Context) (api.Snapshot, error) { return s.snapshot, nil }

type catalogProvider struct{}

func (catalogProvider) Run(context.Context, provider.Request, func(string) error) error {
	panic("workbench queries must never execute prompts")
}
func (catalogProvider) Models(context.Context) ([]api.ModelInfo, error) {
	return []api.ModelInfo{{ID: "test/model", Name: "Test model", Provider: "test"}}, nil
}

func testService(t *testing.T, spec api.WorkspaceSpec) *Service {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return New(snapshotService{snapshot: api.Snapshot{Threads: []api.Thread{{ID: "thread-1", Workspace: &spec}}}}, catalogProvider{}, dataDir)
}

func testGit(t *testing.T, path string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", path}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testRepository(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, path, "init", "-q")
	writeTestFile(t, filepath.Join(path, "tracked.txt"), "before\n")
	testGit(t, path, "add", ".")
	testGit(t, path, "-c", "user.name=Vantry test", "-c", "user.email=test@localhost", "commit", "-qm", "fixture")
	return path
}

func TestWorkspaceQueriesDoNotPrepareOrSubmit(t *testing.T) {
	ctx := context.Background()
	repository := testRepository(t)
	service := testService(t, api.WorkspaceSpec{Kind: "worktree", Repository: repository})
	models, err := service.ListModels(ctx)
	if err != nil || len(models) != 1 || models[0].ID != "test/model" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	if _, err := service.InspectWorkspace(ctx, "thread-1"); api.CodeOf(err) != api.NotReady {
		t.Fatalf("unprepared worktree must not fall back to repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(service.dataDir, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("query created workspace state: %v", err)
	}
	opened := ""
	service.launchTerminal = func(_ context.Context, path string) error { opened = path; return nil }
	if err := service.OpenTerminal(ctx, "thread-1"); err == nil || opened != "" {
		t.Fatalf("opened unprepared workspace %q: %v", opened, err)
	}
	spec := *service.Service.(snapshotService).snapshot.Threads[0].Workspace
	prepared, err := workspace.Prepare(ctx, service.dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(prepared.Path, "child.txt"), "child-only\n")
	changes, err := service.InspectWorkspace(ctx, "thread-1")
	if err != nil || changes.Path != prepared.Path || !strings.Contains(changes.Diff, "child-only") {
		t.Fatalf("child changes=%+v err=%v", changes, err)
	}
	if err := service.OpenTerminal(ctx, "thread-1"); err != nil || opened != prepared.Path || opened == repository {
		t.Fatalf("terminal selected wrong workspace: %q %v", opened, err)
	}
}

func TestDiffIncludesStagedUnstagedAndUntrackedWithoutMutation(t *testing.T) {
	path := testRepository(t)
	writeTestFile(t, filepath.Join(path, "tracked.txt"), "staged\n")
	testGit(t, path, "add", "tracked.txt")
	writeTestFile(t, filepath.Join(path, "tracked.txt"), "unstaged\n")
	writeTestFile(t, filepath.Join(path, "新 文件.txt"), "new text\n")
	writeTestFile(t, filepath.Join(path, "binary.dat"), "\x00binary")
	outside := filepath.Join(t.TempDir(), "private.txt")
	writeTestFile(t, outside, "OUTSIDE_SECRET")
	if err := os.Symlink(outside, filepath.Join(path, "link.txt")); err != nil {
		t.Fatal(err)
	}
	before := testGit(t, path, "status", "--porcelain=v1", "-z")
	service := testService(t, api.WorkspaceSpec{Kind: "directory", Path: path})
	changes, err := service.InspectWorkspace(context.Background(), "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"staged", "unstaged", "new text", "二进制文件"} {
		if !strings.Contains(changes.Diff, expected) {
			t.Errorf("missing %q in diff: %s", expected, changes.Diff)
		}
	}
	if !changes.IsGit || changes.Branch == "" || len(changes.Files) != 4 || strings.Contains(changes.Diff, "OUTSIDE_SECRET") {
		t.Fatalf("unexpected workspace view: %+v", changes)
	}
	if after := testGit(t, path, "status", "--porcelain=v1", "-z"); before != after {
		t.Fatalf("query modified Git state: %q => %q", before, after)
	}
}

func TestDiffHandlesSubdirectoriesAndUnbornRepository(t *testing.T) {
	path := t.TempDir()
	testGit(t, path, "init", "-q")
	subdir := filepath.Join(path, "sub")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(subdir, "file.txt"), "subdirectory text\n")
	writeTestFile(t, filepath.Join(path, "outside.txt"), "outside workspace\n")
	service := testService(t, api.WorkspaceSpec{Kind: "directory", Path: subdir})
	changes, err := service.InspectWorkspace(context.Background(), "thread-1")
	if err != nil || len(changes.Files) != 1 || changes.Files[0].Path != "file.txt" || !strings.Contains(changes.Diff, "subdirectory text") || strings.Contains(changes.Diff, "outside workspace") {
		t.Fatalf("subdirectory changes=%+v err=%v", changes, err)
	}
	testGit(t, path, "add", "sub/file.txt")
	changes, err = service.InspectWorkspace(context.Background(), "thread-1")
	if err != nil || !strings.Contains(changes.Diff, "已暂存") {
		t.Fatalf("unborn staged diff=%+v err=%v", changes, err)
	}
}

func TestDiffIsBoundedAndDirectoryCanBeNonGit(t *testing.T) {
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := testService(t, api.WorkspaceSpec{Kind: "directory", Path: path})
	changes, err := service.InspectWorkspace(context.Background(), "thread-1")
	if err != nil || changes.IsGit || changes.Path != path {
		t.Fatalf("ordinary directory=%+v err=%v", changes, err)
	}
	testGit(t, path, "init", "-q")
	writeTestFile(t, filepath.Join(path, "large.txt"), strings.Repeat("a", 40000))
	changes, err = service.InspectWorkspace(context.Background(), "thread-1")
	if err != nil || !changes.Truncated || len(changes.Diff) > displayLimit {
		t.Fatalf("bounded diff length=%d truncated=%v err=%v", len(changes.Diff), changes.Truncated, err)
	}
	if _, err := service.InspectWorkspace(context.Background(), "missing"); api.CodeOf(err) != api.NotFound {
		t.Fatalf("missing thread: %v", err)
	}
}
