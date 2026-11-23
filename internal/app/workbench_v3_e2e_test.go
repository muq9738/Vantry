package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestWorkbenchV3SSHGitPluginsAndRemoteDesktopBridgeE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dataDir, err := os.MkdirTemp("/tmp", "vt-wbv3-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })

	// Create a real Git repo workspace with initial commit.
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module example.com/demo\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "go.mod")
	runGit("commit", "-m", "chore: initial commit")

	coreCtx, stopCore := context.WithCancel(ctx)
	coreDone := make(chan error, 1)
	cfg := Config{
		DataDir:    dataDir,
		ProviderID: "controlled",
		Delay:      5 * time.Millisecond,
		Workspace:  &api.WorkspaceSpec{Kind: "directory", Path: repoDir},
	}
	go func() { coreDone <- RunCore(coreCtx, cfg) }()
	defer func() {
		stopCore()
		<-coreDone
	}()

	var client *local.Client
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := EnsureCore(ctx, cfg, true)
		if err == nil {
			client = c
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if client == nil {
		t.Fatal("EnsureCore attach timed out")
	}
	defer client.Close()

	// 1. SSH host discovery works over Workbench.
	sshResp, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "ssh.hosts"})
	if err != nil {
		t.Fatalf("ssh.hosts: %v", err)
	}
	_ = sshResp.SSHHosts // valid even if ~/.ssh/config is empty on CI

	// 2. IDEA Git works BEFORE first message (local draft ThreadID + Workspace fallback)!
	if err := os.WriteFile(filepath.Join(repoDir, "feature.go"), []byte("package demo\n// TODO: ship\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitStatus, err := client.Workbench(ctx, api.WorkbenchRequest{
		Action:    "git.status",
		ThreadID:  "local.1",
		Workspace: repoDir,
	})
	if err != nil || gitStatus.Git == nil {
		t.Fatalf("git.status before thread creation failed: resp=%+v err=%v", gitStatus, err)
	}
	if gitStatus.Git.Branch != "main" || len(gitStatus.Git.Commits) == 0 {
		t.Fatalf("unexpected git.status: %+v", gitStatus.Git)
	}
	if !strings.Contains(gitStatus.Git.Diff, "feature.go") {
		t.Fatalf("expected untracked file diff in git.status, got: %q", gitStatus.Git.Diff)
	}

	// Create branch, checkbox-commit untracked file directly, inspect commit detail, and discard dirty edit.
	if _, err := client.Workbench(ctx, api.WorkbenchRequest{
		Action:    "git.branch.create",
		Workspace: repoDir,
		Name:      "feat/idea-git",
	}); err != nil {
		t.Fatalf("git.branch.create: %v", err)
	}
	commitResp, err := client.Workbench(ctx, api.WorkbenchRequest{
		Action:    "git.commit",
		Workspace: repoDir,
		Branch:    "feat/idea-git",
		Paths:     []string{"feature.go"},
		Message:   "feat: add feature via IDEA checkbox commit",
	})
	if err != nil || commitResp.Git == nil || commitResp.Git.Branch != "feat/idea-git" {
		t.Fatalf("git.commit: resp=%+v err=%v", commitResp, err)
	}
	headID := commitResp.Git.Commits[0].ID
	detailResp, err := client.Workbench(ctx, api.WorkbenchRequest{
		Action:    "git.commit_detail",
		Workspace: repoDir,
		Name:      headID,
	})
	if err != nil || !strings.Contains(detailResp.Output, "feature.go") {
		t.Fatalf("git.commit_detail: out=%q err=%v", detailResp.Output, err)
	}

	// Dirty tracked file and test git.discard rollback.
	if err := os.WriteFile(filepath.Join(repoDir, "feature.go"), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterDiscard, err := client.Workbench(ctx, api.WorkbenchRequest{
		Action:    "git.discard",
		Workspace: repoDir,
		Branch:    "feat/idea-git",
		Paths:     []string{"feature.go"},
	})
	if err != nil || afterDiscard.Git == nil || len(afterDiscard.Git.Files) != 0 {
		t.Fatalf("git.discard: resp=%+v err=%v", afterDiscard, err)
	}

	// 3. Clean Core installs must not force-seed IDE connections or remote desktop as built-in plugins.
	plugList, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "plugins.list", Workspace: repoDir})
	if err != nil || len(plugList.Plugins) != 0 {
		t.Fatalf("plugins.list should start empty on clean dataDir: %+v err=%v", plugList.Plugins, err)
	}

	// 4. RTC hosting is served by this Core and refuses a missing native helper
	// without opening a legacy listener or attempting capture.
	t.Setenv("VANTRY_REMOTE_HELPER", filepath.Join(dataDir, "not-packaged"))
	if _, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "rd.status"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "rd.host_start", Name: "qmt"}); err == nil {
		t.Fatal("unpackaged Core silently started another remote desktop transport")
	}
}
