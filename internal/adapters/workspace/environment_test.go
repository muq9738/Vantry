package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestEnvironmentDropsRepositoryRoutingAndKeepsUserConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	t.Setenv("GIT_WORK_TREE", root)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(root, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(root, ".git", "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(root, "objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(root, "alternates"))
	t.Setenv("GIT_NAMESPACE", "decoy")
	t.Setenv("GIT_PREFIX", "sub/")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Vantry User")
	t.Setenv("GIT_SSH_COMMAND", "/usr/bin/ssh -i /dev/null")
	t.Setenv("PATH", "/usr/bin:/bin")
	routing := []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX"}
	for name, environment := range map[string][]string{"Environment": Environment(), "GitEnvironment": GitEnvironment()} {
		values := environmentValues(environment)
		for _, key := range routing {
			if value, ok := values[key]; ok {
				t.Fatalf("%s kept launcher routing variable %s=%s", name, key, value)
			}
		}
		if values["GIT_AUTHOR_NAME"] != "Vantry User" {
			t.Fatalf("%s dropped user Git configuration: %v", name, values["GIT_AUTHOR_NAME"])
		}
		if values["GIT_SSH_COMMAND"] != "/usr/bin/ssh -i /dev/null" {
			t.Fatalf("%s dropped user transport configuration", name)
		}
		if values["PATH"] != "/usr/bin:/bin" {
			t.Fatalf("%s changed PATH: %q", name, values["PATH"])
		}
	}
	if values := environmentValues(Environment()); values["GIT_TERMINAL_PROMPT"] != "" || values["GIT_MERGE_AUTOEDIT"] != "" {
		t.Fatal("Environment kept launcher Git prompt settings")
	}
	values := environmentValues(GitEnvironment())
	if values["GIT_TERMINAL_PROMPT"] != "0" || values["GIT_MERGE_AUTOEDIT"] != "no" {
		t.Fatalf("Git commands must never wait for a terminal: %v", values)
	}
}

func TestPrepareIgnoresInheritedRepositoryRouting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	decoy := filepath.Join(root, "decoy")
	initRepository(t, source)
	initRepository(t, decoy)
	dataDir := filepath.Join(root, "data")
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(decoy, ".git"))

	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: source, Branch: "vantry/thread-1", BaseRef: "HEAD"}
	ready, err := Prepare(ctx, dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if ready.Path == "" || samePath(ready.Path, source) || samePath(ready.Path, decoy) {
		t.Fatalf("preparation was redirected: %+v", ready)
	}
	if !samePath(commonDirInTest(t, ready.Path), commonDirInTest(t, source)) {
		t.Fatalf("worktree does not belong to the requested repository: %s", ready.Path)
	}
	if branch := runGitFiltered(t, ready.Path, "symbolic-ref", "--short", "HEAD"); branch != spec.Branch {
		t.Fatalf("worktree branch = %q", branch)
	}
	if branches := runGitFiltered(t, decoy, "branch", "--list", spec.Branch); branches != "" {
		t.Fatalf("decoy repository received the branch: %q", branches)
	}
	if worktrees := runGitFiltered(t, decoy, "worktree", "list", "--porcelain"); strings.Contains(worktrees, ready.Path) {
		t.Fatalf("decoy repository received the worktree: %q", worktrees)
	}

	dirty := filepath.Join(ready.Path, "dirty.txt")
	if err := os.WriteFile(dirty, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := Prepare(ctx, dataDir, spec, "thread-1")
	if err != nil || again.Path != ready.Path {
		t.Fatalf("repeat prepare = %+v, %v", again, err)
	}
	existing, err := Existing(ctx, dataDir, spec, "thread-1")
	if err != nil || !samePath(existing, ready.Path) {
		t.Fatalf("existing = %q, %v", existing, err)
	}
	if data, err := os.ReadFile(dirty); err != nil || string(data) != "keep me" {
		t.Fatalf("uncommitted file was not preserved: %q, %v", data, err)
	}
	if status := runGitFiltered(t, ready.Path, "status", "--porcelain"); !strings.Contains(status, "dirty.txt") {
		t.Fatalf("uncommitted file missing from worktree status: %q", status)
	}
	if status := runGitFiltered(t, source, "status", "--porcelain"); status != "" {
		t.Fatalf("source checkout was modified: %q", status)
	}
}

func TestWorktreePreparationRejectsPrimaryCheckoutAndSubdirectories(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	initRepository(t, source)
	dataDir := filepath.Join(root, "data")
	marker := filepath.Join(source, "uncommitted.txt")
	if err := os.WriteFile(marker, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "primary-link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	subdirectory := filepath.Join(source, "nested")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name string
		spec api.WorkspaceSpec
	}{
		{"primary path", api.WorkspaceSpec{Kind: KindWorktree, Repository: source, Path: source}},
		{"symlinked primary path", api.WorkspaceSpec{Kind: KindWorktree, Repository: source, Path: link}},
		{"repository subdirectory", api.WorkspaceSpec{Kind: KindWorktree, Repository: subdirectory}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if path, err := Prepare(ctx, dataDir, testCase.spec, "thread-1"); err == nil {
				t.Fatalf("preparation accepted a checkout alias: %+v", path)
			}
			if path, err := Existing(ctx, dataDir, testCase.spec, "thread-1"); err == nil {
				t.Fatalf("opening accepted a checkout alias: %q", path)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "keep me" {
				t.Fatalf("rejected request touched the source checkout: %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "workspaces", "thread-1", "prepared.json")); !os.IsNotExist(err) {
				t.Fatalf("rejected request produced a ready record: %v", err)
			}
		})
	}
}

func TestPrepareFromLinkedWorktreeStaysInTheSameRepository(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	primary := filepath.Join(root, "primary")
	initRepository(t, primary)
	linked := filepath.Join(root, "linked")
	runGitTest(t, primary, "worktree", "add", "-b", "linked-source", linked, "HEAD")
	dataDir := filepath.Join(root, "data")
	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: linked, Branch: "vantry/thread-1", BaseRef: "HEAD"}
	ready, err := Prepare(ctx, dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if samePath(ready.Path, primary) || samePath(ready.Path, linked) {
		t.Fatalf("preparation fell back to another checkout: %+v", ready)
	}
	if !samePath(commonDirInTest(t, ready.Path), commonDirInTest(t, primary)) {
		t.Fatalf("linked worktree source produced a foreign checkout: %s", ready.Path)
	}
	if branch := runGitFiltered(t, ready.Path, "symbolic-ref", "--short", "HEAD"); branch != spec.Branch {
		t.Fatalf("worktree branch = %q", branch)
	}
}

func TestHistoricalPreparedRecordsAreRejectedWithoutBeingRewritten(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	other := filepath.Join(root, "other")
	initRepository(t, source)
	initRepository(t, other)
	foreignWorktree := filepath.Join(root, "foreign")
	runGitTest(t, other, "worktree", "add", "-b", "foreign-branch", foreignWorktree, "HEAD")
	foreignMarker := filepath.Join(foreignWorktree, "foreign.txt")
	if err := os.WriteFile(foreignMarker, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("primary checkout", func(t *testing.T) {
		const owner = "thread-primary"
		dataDir := filepath.Join(root, "data-primary")
		branch := runGitFiltered(t, source, "symbolic-ref", "--short", "HEAD")
		spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: source, Path: source, Branch: branch}
		writeRecord(t, dataDir, owner, prepared{Owner: owner, Spec: spec, Path: source})
		records := recordBytes(t, dataDir, owner)
		if path, err := Existing(ctx, dataDir, spec, owner); err == nil {
			t.Fatalf("historical primary checkout accepted: %q", path)
		}
		if path, err := Prepare(ctx, dataDir, spec, owner); err == nil {
			t.Fatalf("historical primary checkout prepared: %+v", path)
		}
		assertRecordsUnchanged(t, dataDir, owner, records)
		if _, err := os.Stat(filepath.Join(dataDir, "workspaces", owner, "intent.json")); !os.IsNotExist(err) {
			t.Fatalf("rejected intent was persisted: %v", err)
		}
	})

	t.Run("symlinked primary checkout", func(t *testing.T) {
		const owner = "thread-symlink"
		dataDir := filepath.Join(root, "data-symlink")
		link := filepath.Join(root, "primary-link")
		if err := os.Symlink(source, link); err != nil {
			t.Fatal(err)
		}
		branch := runGitFiltered(t, source, "symbolic-ref", "--short", "HEAD")
		spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: source, Path: link, Branch: branch}
		writeRecord(t, dataDir, owner, prepared{Owner: owner, Spec: spec, Path: link})
		records := recordBytes(t, dataDir, owner)
		if path, err := Existing(ctx, dataDir, spec, owner); err == nil {
			t.Fatalf("symlinked primary checkout accepted: %q", path)
		}
		if path, err := Prepare(ctx, dataDir, spec, owner); err == nil {
			t.Fatalf("symlinked primary checkout prepared: %+v", path)
		}
		assertRecordsUnchanged(t, dataDir, owner, records)
	})

	t.Run("foreign worktree", func(t *testing.T) {
		const owner = "thread-foreign"
		dataDir := filepath.Join(root, "data-foreign")
		spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: source, Path: foreignWorktree, Branch: "foreign-branch", BaseRef: "HEAD"}
		writeRecord(t, dataDir, owner, prepared{Owner: owner, Spec: spec, Path: foreignWorktree})
		records := recordBytes(t, dataDir, owner)
		if path, err := Existing(ctx, dataDir, spec, owner); err == nil {
			t.Fatalf("checkout of another project accepted: %q", path)
		}
		if path, err := Prepare(ctx, dataDir, spec, owner); err == nil {
			t.Fatalf("checkout of another project prepared: %+v", path)
		}
		assertRecordsUnchanged(t, dataDir, owner, records)
		if data, err := os.ReadFile(foreignMarker); err != nil || string(data) != "keep me" {
			t.Fatalf("foreign worktree was changed: %q, %v", data, err)
		}
	})

	t.Run("wrong branch", func(t *testing.T) {
		const owner = "thread-branch"
		dataDir := filepath.Join(root, "data-branch")
		ready, err := Prepare(ctx, dataDir, api.WorkspaceSpec{Kind: KindWorktree, Repository: source}, owner)
		if err != nil {
			t.Fatal(err)
		}
		dirty := filepath.Join(ready.Path, "dirty.txt")
		if err := os.WriteFile(dirty, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		record := prepared{Owner: owner, Spec: ready, Path: ready.Path}
		record.Spec.Branch = "main"
		writeRecord(t, dataDir, owner, record)
		records := recordBytes(t, dataDir, owner)
		request := api.WorkspaceSpec{Kind: KindWorktree, Repository: ready.Repository, Path: ready.Path, Branch: "main", BaseRef: ready.BaseRef}
		if path, err := Existing(ctx, dataDir, request, owner); err == nil {
			t.Fatalf("worktree on another branch accepted: %q", path)
		}
		assertRecordsUnchanged(t, dataDir, owner, records)
		if data, err := os.ReadFile(dirty); err != nil || string(data) != "keep me" {
			t.Fatalf("rejected request changed the worktree: %q, %v", data, err)
		}
	})

	t.Run("repository subdirectory", func(t *testing.T) {
		const owner = "thread-subdirectory"
		dataDir := filepath.Join(root, "data-subdirectory")
		ready, err := Prepare(ctx, dataDir, api.WorkspaceSpec{Kind: KindWorktree, Repository: source}, owner)
		if err != nil {
			t.Fatal(err)
		}
		subdirectory := filepath.Join(source, "nested")
		if err := os.Mkdir(subdirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		record := prepared{Owner: owner, Spec: ready, Path: ready.Path}
		record.Spec.Repository = subdirectory
		writeRecord(t, dataDir, owner, record)
		records := recordBytes(t, dataDir, owner)
		request := ready
		request.Repository = subdirectory
		if path, err := Existing(ctx, dataDir, request, owner); err == nil {
			t.Fatalf("historical subdirectory repository accepted: %q", path)
		}
		assertRecordsUnchanged(t, dataDir, owner, records)
	})
}

func TestHistoricalRecordPathMustMatchItsIntent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	initRepository(t, source)
	dataDir := filepath.Join(root, "data")
	ready, err := Prepare(ctx, dataDir, api.WorkspaceSpec{Kind: KindWorktree, Repository: source}, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	record := prepared{Owner: "thread-1", Spec: ready, Path: root}
	writeRecord(t, dataDir, "thread-1", record)
	records := recordBytes(t, dataDir, "thread-1")
	if path, err := Existing(ctx, dataDir, ready, "thread-1"); err == nil {
		t.Fatalf("mismatched record path accepted: %q", path)
	}
	assertRecordsUnchanged(t, dataDir, "thread-1", records)
}

func environmentValues(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	return values
}

// runGitFiltered inspects fixtures with the same environment Git commands use,
// so a test can inject routing variables without inspecting the wrong checkout.
func runGitFiltered(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Env = GitEnvironment()
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func commonDirInTest(t *testing.T, path string) string {
	t.Helper()
	common := runGitFiltered(t, path, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	resolved, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func writeRecord(t *testing.T, dataDir, owner string, record prepared) {
	t.Helper()
	if err := writeJSON(filepath.Join(dataDir, "workspaces", owner, "prepared.json"), record); err != nil {
		t.Fatal(err)
	}
}

func recordBytes(t *testing.T, dataDir, owner string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dataDir, "workspaces", owner, "prepared.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertRecordsUnchanged(t *testing.T, dataDir, owner string, before []byte) {
	t.Helper()
	after := recordBytes(t, dataDir, owner)
	if string(after) != string(before) {
		t.Fatalf("prepared record was rewritten:\nbefore %s\nafter  %s", before, after)
	}
}
