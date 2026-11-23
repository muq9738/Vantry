package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func changeFile(t *testing.T, root, path string) api.GitFile {
	t.Helper()
	state, err := Inspect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range state.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("file %q absent: %+v", path, state.Files)
	return api.GitFile{}
}

func TestChangeUsesLiteralPathsAndUnstageKeepsFiles(t *testing.T) {
	root, _ := repository(t)
	names := []string{"中文 空格.txt", "-n.txt", "*.txt", ":(glob)magic.txt", "a[b].txt", "new\nline.txt"}
	for _, name := range names {
		put(t, filepath.Join(root, name), "content\n")
	}
	for _, name := range names {
		if _, err := Change(context.Background(), root, api.WorkbenchRequest{Action: "git.stage", Branch: "main", Paths: []string{name}}); err != nil {
			t.Fatal(err)
		}
		if file := changeFile(t, root, name); file.Index != "A" {
			t.Fatalf("not staged: %+v", file)
		}
		if _, err := Change(context.Background(), root, api.WorkbenchRequest{Action: "git.unstage", Branch: "main", Paths: []string{name}}); err != nil {
			t.Fatal(err)
		}
		if file := changeFile(t, root, name); file.Index != "?" {
			t.Fatalf("not untracked: %+v", file)
		}
		if data, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(data) != "content\n" {
			t.Fatalf("worktree changed: %q %v", data, err)
		}
	}
}

func TestChangeCommitRetainsUnstagedWorkAndReportsHookFailure(t *testing.T) {
	root, run := repository(t)
	put(t, filepath.Join(root, "version.txt"), "base\n")
	run("add", "version.txt")
	run("commit", "-m", "base file")
	put(t, filepath.Join(root, "version.txt"), "staged\n")
	run("add", "version.txt")
	put(t, filepath.Join(root, "version.txt"), "unstaged\n")
	put(t, filepath.Join(root, "untracked.txt"), "keep\n")
	hook(t, root, run, "pre-commit", "echo 'blocked by test hook' >&2\nexit 1")
	req := api.WorkbenchRequest{Action: "git.commit", Branch: "main", Message: "staged only"}
	if _, err := Change(context.Background(), root, req); err == nil || !strings.Contains(err.Error(), "blocked by test hook") {
		t.Fatalf("hook failure: %v", err)
	}
	if file := changeFile(t, root, "version.txt"); file.Index != "M" || file.Worktree != "M" {
		t.Fatalf("failed hook changed state: %+v", file)
	}
	run("config", "--unset", "core.hooksPath")
	if _, err := Change(context.Background(), root, req); err != nil {
		t.Fatal(err)
	}
	if got := run("show", "HEAD:version.txt"); strings.TrimSpace(got) != "staged" {
		t.Fatalf("committed %q", got)
	}
	if file := changeFile(t, root, "version.txt"); file.Index != " " || file.Worktree != "M" {
		t.Fatalf("lost unstaged work: %+v", file)
	}
	if file := changeFile(t, root, "untracked.txt"); file.Index != "?" {
		t.Fatalf("lost untracked file: %+v", file)
	}
}

func TestChangeRejectsBranchDriftAndUnknownPaths(t *testing.T) {
	root, run := repository(t)
	run("switch", "-c", "other")
	put(t, filepath.Join(root, "file.txt"), "edit\n")
	for _, req := range []api.WorkbenchRequest{
		{Action: "git.stage", Branch: "main", Paths: []string{"file.txt"}},
		{Action: "git.stage", Branch: "other", Paths: []string{"../escape"}},
		{Action: "git.stage", Branch: "other", Paths: []string{"missing"}},
		{Action: "git.commit", Branch: "other", Message: " "},
	} {
		if _, err := Change(context.Background(), root, req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
}

func TestChangeUnstageInUnbornRepository(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent"))
	if _, err := command(context.Background(), root, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "first.txt"), "first\n")
	for _, action := range []string{"git.stage", "git.unstage"} {
		if _, err := Change(context.Background(), root, api.WorkbenchRequest{Action: action, Branch: "main", Paths: []string{"first.txt"}}); err != nil {
			t.Fatal(err)
		}
	}
	if file := changeFile(t, root, "first.txt"); file.Index != "?" {
		t.Fatalf("unstage changed file: %+v", file)
	}
}
