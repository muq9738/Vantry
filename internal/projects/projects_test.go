package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestCreateInitializeUpdateAndDeleteProject(t *testing.T) {
	// Exercise initialization on a machine without a configured Git identity.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "missing-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"APP_MODE": "test"}
	project, err := store.CreateOrAdopt(filepath.Join(root, "new-project"), CreateOptions{
		Title: "Lane 10", InitGitIfMissing: true, SetupScript: "echo ready", DevCommand: "npm run dev",
		CustomInstructions: "Use Go", Env: env,
	})
	if err != nil {
		t.Fatal(err)
	}
	env["APP_MODE"] = "caller mutation"
	if project.ID == "" || project.Title != "Lane 10" || project.DefaultBaseBranch != "main" ||
		!filepath.IsAbs(project.RootPath) || project.CreatedAt.IsZero() || project.CreatedAt != project.UpdatedAt {
		t.Fatalf("invalid project: %+v", project)
	}
	if got := projectTestGit(t, project.RootPath, "status", "--porcelain"); got != "" {
		t.Fatalf("initialized project is dirty: %s", got)
	}
	if got := projectTestGit(t, project.RootPath, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("initial commit count = %s", got)
	}
	for _, name := range []string{"README.md", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(project.RootPath, name)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(store.dataDir, "projects.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("registry permission = %v, %v", info, err)
	}
	reopened, err := NewStore(store.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := reopened.List()
	if err != nil || len(listed) != 1 || listed[0].Env["APP_MODE"] != "test" {
		t.Fatalf("registry round trip: %+v, %v", listed, err)
	}
	title, empty := "Renamed", ""
	updated, err := reopened.Update(project.ID, UpdateOptions{Title: &title, SetupScript: &empty, Env: &map[string]string{"APP_MODE": "updated"}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != title || updated.SetupScript != "" || updated.DevCommand != project.DevCommand ||
		updated.CustomInstructions != project.CustomInstructions || updated.CreatedAt != project.CreatedAt || !updated.UpdatedAt.After(project.UpdatedAt) {
		t.Fatalf("updated project: %+v", updated)
	}
	if err := reopened.Delete(project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project.RootPath, "README.md")); err != nil {
		t.Fatalf("Delete removed user source: %v", err)
	}
	if projects, err := reopened.List(); err != nil || len(projects) != 0 {
		t.Fatalf("registry after delete: %+v, %v", projects, err)
	}
	if err := reopened.Delete(project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Delete = %v", err)
	}
	if _, err := reopened.Update(project.ID, UpdateOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Update = %v", err)
	}
}

func TestAdoptCanonicalDirectoryAndFindLinkedWorktree(t *testing.T) {
	store, project, spec := setupProject(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(project.RootPath, alias); err != nil {
		t.Fatal(err)
	}
	again, err := store.CreateOrAdopt(alias, CreateOptions{Title: "ignored on adoption"})
	if err != nil || again.ID != project.ID || again.RootPath != project.RootPath {
		t.Fatalf("canonical re-adoption: %+v, %v", again, err)
	}
	for _, path := range []string{alias, project.RootPath, spec.Path} {
		found, err := store.FindByPath(path)
		if err != nil || found.ID != project.ID {
			t.Fatalf("FindByPath(%s) = %+v, %v", path, found, err)
		}
	}
	nested := filepath.Join(project.RootPath, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedProject, err := store.CreateOrAdopt(nested, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found, err := store.FindByPath(nested)
	if err != nil || found.ID != nestedProject.ID {
		t.Fatalf("nested project match = %+v, %v", found, err)
	}
	found, err = store.FindByPath(spec.Path)
	if err != nil || found.ID != project.ID {
		t.Fatalf("registered subdirectory claimed the whole linked worktree: %+v, %v", found, err)
	}
	if _, err := store.FindByPath(t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered path = %v", err)
	}
}

func TestAdoptDoesNotInitializeAndInitRefusesNonemptyDirectory(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "user.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrAdopt(existing, CreateOptions{InitGitIfMissing: true}); err == nil {
		t.Fatal("initialized a nonempty directory")
	}
	if _, err := os.Lstat(filepath.Join(existing, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed initialization modified directory: %v", err)
	}
	project, err := store.CreateOrAdopt(existing, CreateOptions{ID: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(existing, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plain adoption initialized Git")
	}
	if _, err := store.CreateOrAdopt(t.TempDir(), CreateOptions{ID: project.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate ID = %v", err)
	}
	if _, err := store.CreateOrAdopt(t.TempDir(), CreateOptions{Env: map[string]string{"BAD=KEY": "value"}}); err == nil {
		t.Fatal("invalid environment accepted")
	}
}

func TestConcurrentRegistryWritersKeepEveryProject(t *testing.T) {
	root := t.TempDir()
	const count = 8
	stores := make([]*Store, count)
	paths := make([]string, count)
	for i := range count {
		var err error
		stores[i], err = NewStore(filepath.Join(root, "data"))
		if err != nil {
			t.Fatal(err)
		}
		paths[i] = filepath.Join(root, fmt.Sprintf("project-%d", i))
		if err := os.Mkdir(paths[i], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var writers sync.WaitGroup
	failures := make(chan error, count)
	for i := range count {
		writers.Go(func() {
			_, err := stores[i].CreateOrAdopt(paths[i], CreateOptions{})
			failures <- err
		})
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	projects, err := stores[0].List()
	if err != nil || len(projects) != count {
		t.Fatalf("concurrent registry = %d projects, %v", len(projects), err)
	}
}

func TestSetupRunsOnceWithEnvironmentInWorktree(t *testing.T) {
	store, project, spec := setupProject(t)
	t.Setenv("LANE_TEST_VALUE", "inherited")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong-git"))
	project.Env = map[string]string{"LANE_TEST_VALUE": "project value"}
	project.SetupScript = "printf '%s\\n' \"$LANE_TEST_VALUE\" >> count.txt; pwd > cwd.txt; git rev-parse --show-toplevel > git-root.txt"
	var callers sync.WaitGroup
	failures := make(chan error, 6)
	for range 6 {
		callers.Go(func() { failures <- store.RunSetupScript(context.Background(), project, spec.Path) })
	}
	callers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{"count.txt": "project value\n", "cwd.txt": spec.Path + "\n", "git-root.txt": spec.Path + "\n"} {
		data, err := os.ReadFile(filepath.Join(spec.Path, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v, want %q", name, data, err, want)
		}
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(spec.Path), "setup-done.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("setup marker = %v, %v", info, err)
	}
	// Editing a registered script does not reinitialize the same checkout.
	project.SetupScript = "exit 99"
	if err := RunSetupScript(context.Background(), project, spec.Path); err != nil {
		t.Fatalf("completed setup reran: %v", err)
	}
}

func TestPrepareWorktreeAutomaticallyRunsSetupOncePerCheckout(t *testing.T) {
	store, project, _ := setupProject(t)
	project.SetupScript = "printf initialized >> setup.txt"
	for generation := range 2 {
		for range 2 {
			spec, err := store.PrepareWorktree(context.Background(), project, "auto-owner", "feature/automatic", "")
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(spec.Path, "setup.txt"))
			if err != nil || string(data) != "initialized" || spec.BaseRef != project.DefaultBaseBranch {
				t.Fatalf("automatic setup generation %d: %q, %+v, %v", generation, data, spec, err)
			}
		}
		if err := workspace.CleanupManagedWorkspace(context.Background(), store.dataDir, "auto-owner", true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetupTimeoutKillsProcessGroupAndAllowsRetry(t *testing.T) {
	_, project, spec := setupProject(t)
	project.SetupScript = "(sleep 0.6; printf survived > orphan.txt) & wait"
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := RunSetupScript(ctx, project, spec.Path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("setup timeout = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("setup did not return promptly")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(spec.Path), "setup-done.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed setup marked done: %v", err)
	}
	lockCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	lock, err := workspace.LockDirectory(lockCtx, spec.Path)
	if err != nil {
		t.Fatalf("setup leaked directory lock: %v", err)
	}
	workspace.Unlock(lock)
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(spec.Path, "orphan.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("background child survived timeout: %v", err)
	}
	project.SetupScript = "printf retried > retry.txt"
	if err := RunSetupScript(context.Background(), project, spec.Path); err != nil {
		t.Fatalf("retry after timeout: %v", err)
	}
}

func TestSetupHoldsCleanupLocksUntilCancellation(t *testing.T) {
	store, project, spec := setupProject(t)
	project.SetupScript = "touch started.txt; sleep 10"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- store.RunSetupScript(ctx, project, spec.Path) }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(spec.Path, "started.txt")); err == nil {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("setup exited before starting: %v", err)
		case <-deadline.C:
			t.Fatal("setup did not start")
		case <-ticker.C:
		}
	}
	if err := workspace.CleanupManagedWorkspace(context.Background(), store.dataDir, "setup-owner", true); !errors.Is(err, workspace.ErrWorkspaceActive) {
		t.Fatalf("cleanup ignored running setup: %v", err)
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("setup cancellation = %v", err)
	}
}

func TestSetupOutputIsBoundedAndFailureCanRetry(t *testing.T) {
	_, project, spec := setupProject(t)
	project.SetupScript = "head -c 131072 /dev/zero; printf stderr >&2; exit 9"
	err := RunSetupScript(context.Background(), project, spec.Path)
	var setupErr *SetupError
	if !errors.As(err, &setupErr) || len(setupErr.Output) != MaxSetupOutput || !setupErr.Truncated {
		t.Fatalf("setup output was not bounded: %T, %v", err, err)
	}
	project.SetupScript = "true"
	if err := RunSetupScript(context.Background(), project, spec.Path); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRejectsAnotherProjectAndHonorsWriterLock(t *testing.T) {
	store, project, spec := setupProject(t)
	project.SetupScript = "touch must-not-run"
	other := project
	other.RootPath = t.TempDir()
	if err := store.RunSetupScript(context.Background(), other, spec.Path); err == nil {
		t.Fatal("ran setup for an unrelated project")
	}
	lock, err := workspace.LockDirectory(context.Background(), spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Unlock(lock)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := RunSetupScript(ctx, project, spec.Path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("setup ignored writer lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spec.Path, "must-not-run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup started while a writer was active")
	}
}

func TestRegistryReadFailurePreservesExistingData(t *testing.T) {
	store, project, _ := setupProject(t)
	registry := filepath.Join(store.dataDir, "projects.json")
	invalid := []byte("not valid JSON\n")
	if err := os.WriteFile(registry, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(project.ID); err == nil {
		t.Fatal("corrupt registry silently replaced")
	}
	got, err := os.ReadFile(registry)
	if err != nil || !reflect.DeepEqual(got, invalid) {
		t.Fatal("corrupt registry was modified")
	}
}

func setupProject(t *testing.T) (*Store, Project, api.WorkspaceSpec) {
	t.Helper()
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateOrAdopt(filepath.Join(root, "repo"), CreateOptions{InitGitIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := workspace.Prepare(context.Background(), store.dataDir, api.WorkspaceSpec{Kind: workspace.KindWorktree, Repository: project.RootPath, Branch: "setup-test"}, "setup-owner")
	if err != nil {
		t.Fatal(err)
	}
	return store, project, spec
}

func projectTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	command.Env = workspace.GitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
