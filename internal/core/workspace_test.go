package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDelegatePlansGitIsolationAndPreservesExplicitWorkspace(t *testing.T) {
	repository := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	nested := filepath.Join(repository, "source")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	c := openTestCore(t, &routeProvider{})
	root := dispatchOrFail(t, c, api.Command{Version: 1, CommandID: "root", Kind: api.CreateThread,
		Workspace: &api.WorkspaceSpec{Kind: "directory", Path: nested}})
	parent := mustSend(t, c, "parent", root.ThreadID, "initial")
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	current := latestAttempt(t, c, parent.RunID)
	child := dispatchOrFail(t, c, delegateCommand("default-child", parent.RunID, current.AttemptID, "inspect", "manual"))
	childRun := latestAttempt(t, c, child.RunID)
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if childRun.Workspace == nil || childRun.Workspace.Kind != "worktree" || childRun.Workspace.Repository != canonicalRepository || childRun.Workspace.Path != "" {
		t.Fatalf("Git child inherited the parent's working directory: %+v", childRun.Workspace)
	}
	explicitPath := t.TempDir()
	command := delegateCommand("explicit-child", parent.RunID, current.AttemptID, "inspect separate directory", "manual")
	command.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: explicitPath}
	explicit := dispatchOrFail(t, c, command)
	if spec := latestAttempt(t, c, explicit.RunID).Workspace; spec.Kind != "directory" || spec.Path != explicitPath {
		t.Fatalf("explicit workspace replaced: %+v", spec)
	}
}
